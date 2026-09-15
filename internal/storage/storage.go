package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Bucket names
var (
	bucketASN          = []byte("asn_by_ip")
	bucketPrefixes     = []byte("prefixes_by_asn")
	bucketCache        = []byte("cache")
	bucketSnapshots    = []byte("snapshots")
	bucketHistory      = []byte("history")
	bucketTransactions = []byte("transactions")
)

// Cache represents the bbolt database wrapper
type Cache struct {
	db *bolt.DB
	mu sync.RWMutex
}

// SnapshotInfo представляет метаданные snapshot
type SnapshotInfo struct {
	ID        string    `json:"id"`
	Service   string    `json:"service"`
	CreatedAt time.Time `json:"created_at"`
	Count     int       `json:"route_count"`
}

type cachedEntry struct {
	Value     json.RawMessage `json:"value"`
	ExpiresAt time.Time       `json:"expires_at"`
}

// Open opens or creates the bbolt database and initializes all buckets
func Open(path string) (*Cache, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create directory: %w", err)
		}
	}

	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bolt db: %w", err)
	}

	// Initialize all required buckets
	err = db.Update(func(tx *bolt.Tx) error {
		buckets := [][]byte{
			bucketASN, bucketPrefixes, bucketCache,
			bucketSnapshots, bucketHistory, bucketTransactions,
		}
		for _, b := range buckets {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return fmt.Errorf("create bucket %s: %w", string(b), err)
			}
		}
		return nil
	})

	if err != nil {
		db.Close()
		return nil, err
	}

	return &Cache{db: db}, nil
}

// Close closes the database
func (c *Cache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.db.Close()
}

// ============================================================================
// Generic cache methods (for ASN/prefixes caching)
// ============================================================================

func (c *Cache) get(bucket []byte, key string, dst any) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var found bool
	_ = c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return nil
		}
		raw := b.Get([]byte(key))
		if raw == nil {
			return nil
		}
		var entry cachedEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil
		}
		if time.Now().After(entry.ExpiresAt) {
			return nil
		}
		if err := json.Unmarshal(entry.Value, dst); err != nil {
			return nil
		}
		found = true
		return nil
	})
	return found
}

func (c *Cache) set(bucket []byte, key string, value any, ttl time.Duration) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal value: %w", err)
	}
	entry := cachedEntry{Value: raw, ExpiresAt: time.Now().Add(ttl)}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal entry: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return c.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucket)
		if err != nil {
			return err
		}
		return b.Put([]byte(key), encoded)
	})
}

// GetASN retrieves cached ASN for an IP
func (c *Cache) GetASN(ip string) (int, bool) {
	var asn int
	return asn, c.get(bucketASN, ip, &asn)
}

// SetASN caches ASN for an IP with TTL
func (c *Cache) SetASN(ip string, asn int, ttl time.Duration) error {
	return c.set(bucketASN, ip, asn, ttl)
}

// GetPrefixes retrieves cached prefixes for an ASN
func (c *Cache) GetPrefixes(asn int) ([]string, bool) {
	var prefixes []string
	return prefixes, c.get(bucketPrefixes, fmt.Sprintf("%d", asn), &prefixes)
}

// SetPrefixes caches prefixes for an ASN with TTL
func (c *Cache) SetPrefixes(asn int, prefixes []string, ttl time.Duration) error {
	return c.set(bucketPrefixes, fmt.Sprintf("%d", asn), prefixes, ttl)
}

// PurgeExpired removes expired entries from cache buckets
func (c *Cache) PurgeExpired() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketASN, bucketPrefixes, bucketCache} {
			b := tx.Bucket(name)
			if b == nil {
				continue
			}
			var toDelete [][]byte
			_ = b.ForEach(func(k, v []byte) error {
				var entry cachedEntry
				if err := json.Unmarshal(v, &entry); err != nil || time.Now().After(entry.ExpiresAt) {
					toDelete = append(toDelete, append([]byte(nil), k...))
				}
				return nil
			})
			for _, k := range toDelete {
				if err := b.Delete(k); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// ============================================================================
// Generic bucket methods (for history/transactions)
// ============================================================================

// Put stores a value in a specific bucket
func (c *Cache) Put(bucket, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal value: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("bucket %s not found", bucket)
		}
		return b.Put([]byte(key), data)
	})
}

// Get retrieves a value from a specific bucket
func (c *Cache) Get(bucket, key string, v any) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("bucket %s not found", bucket)
		}
		data := b.Get([]byte(key))
		if data == nil {
			return fmt.Errorf("key not found in bucket %s", bucket)
		}
		return json.Unmarshal(data, v)
	})
}

// ============================================================================
// Snapshot methods
// ============================================================================

// CreateSnapshot создаёт новый snapshot и возвращает его ID
func (c *Cache) CreateSnapshot(service string, routes any) (string, error) {
	data, err := json.Marshal(routes)
	if err != nil {
		return "", fmt.Errorf("marshal routes: %w", err)
	}

	snapshotID := fmt.Sprintf("%d", time.Now().UnixNano())
	key := fmt.Sprintf("%s:%s", service, snapshotID)

	c.mu.Lock()
	defer c.mu.Unlock()

	err = c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSnapshots)
		if b == nil {
			return fmt.Errorf("snapshots bucket not found")
		}
		return b.Put([]byte(key), data)
	})

	if err != nil {
		return "", err
	}

	return snapshotID, nil
}

// ListSnapshots возвращает список snapshots для сервиса
func (c *Cache) ListSnapshots(service string) ([]SnapshotInfo, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var snapshots []SnapshotInfo

	err := c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSnapshots)
		if b == nil {
			return nil
		}

		prefix := []byte(service + ":")
		cursor := b.Cursor()

		for key, _ := cursor.Seek(prefix); key != nil && len(key) > 0; key, _ = cursor.Next() {
			if len(key) < len(prefix) {
				break
			}
			if string(key[:len(prefix)]) != string(prefix) {
				break
			}

			keyStr := string(key)
			snapshotID := keyStr[len(service)+1:]

			snapshots = append(snapshots, SnapshotInfo{
				ID:      snapshotID,
				Service: service,
			})
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].ID > snapshots[j].ID
	})

	return snapshots, nil
}

// GetSnapshot получает snapshot по ID
func (c *Cache) GetSnapshot(service, snapshotID string, v any) error {
	key := fmt.Sprintf("%s:%s", service, snapshotID)
	return c.Get("snapshots", key, v)
}

// DeleteSnapshot удаляет snapshot
func (c *Cache) DeleteSnapshot(service, snapshotID string) error {
	key := fmt.Sprintf("%s:%s", service, snapshotID)

	c.mu.Lock()
	defer c.mu.Unlock()

	return c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSnapshots)
		if b == nil {
			return fmt.Errorf("snapshots bucket not found")
		}
		return b.Delete([]byte(key))
	})
}

// CleanupExpiredSnapshots удаляет snapshots старше TTL
func (c *Cache) CleanupExpiredSnapshots(ttl time.Duration) (int, error) {
	cutoff := time.Now().Add(-ttl)
	deleted := 0

	c.mu.Lock()
	defer c.mu.Unlock()

	err := c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSnapshots)
		if b == nil {
			return nil
		}

		cursor := b.Cursor()
		var toDelete []string

		for key, _ := cursor.First(); key != nil; key, _ = cursor.Next() {
			keyStr := string(key)
			if len(keyStr) < 3 {
				continue
			}

			parts := splitKey(keyStr)
			if len(parts) != 2 {
				continue
			}

			snapshotID := parts[1]
			var ts int64
			if _, err := fmt.Sscanf(snapshotID, "%d", &ts); err != nil {
				continue
			}

			createdAt := time.Unix(0, ts)
			if createdAt.Before(cutoff) {
				toDelete = append(toDelete, keyStr)
			}
		}

		for _, key := range toDelete {
			if err := b.Delete([]byte(key)); err == nil {
				deleted++
			}
		}

		return nil
	})

	return deleted, err
}

// CountSnapshots возвращает количество snapshots для сервиса
func (c *Cache) CountSnapshots(service string) (int, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	count := 0
	err := c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSnapshots)
		if b == nil {
			return nil
		}

		prefix := []byte(service + ":")
		cursor := b.Cursor()

		for key, _ := cursor.Seek(prefix); key != nil && len(key) > 0; key, _ = cursor.Next() {
			if len(key) < len(prefix) {
				break
			}
			if string(key[:len(prefix)]) != string(prefix) {
				break
			}
			count++
		}

		return nil
	})

	return count, err
}

// splitKey разделяет ключ на части
func splitKey(key string) []string {
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == ':' {
			return []string{key[:i], key[i+1:]}
		}
	}
	return []string{key}
}
