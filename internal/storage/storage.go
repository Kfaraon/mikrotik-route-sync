package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Имена бакетов в bbolt (только IPv4 данные).
var (
	bucketASN          = []byte("asn_by_ip")
	bucketPrefixes     = []byte("prefixes_by_asn")
	bucketCache        = []byte("cache")
	bucketSnapshots    = []byte("snapshots")
	bucketHistory      = []byte("history")
	bucketTransactions = []byte("transactions")
)

// Cache — обёртка над bbolt для кэша и хранения состояния.
type Cache struct {
	db *bolt.DB
	mu sync.RWMutex
}

// SnapshotInfo — метаинформация о снапшоте.
type SnapshotInfo struct {
	ID        string    `json:"id"`
	Service   string    `json:"service"`
	CreatedAt time.Time `json:"created_at"`
	Count     int       `json:"route_count"`
}

// cachedEntry — запись кэша с TTL.
type cachedEntry struct {
	Value     json.RawMessage `json:"value"`
	ExpiresAt time.Time       `json:"expires_at"`
}

// Open открывает или создаёт базу bbolt и инициализирует все бакеты.
func Open(path string) (*Cache, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create dir: %w", err)
		}
	}

	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bolt: %w", err)
	}

	// Создание всех необходимых бакетов
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

// Close закрывает базу.
func (c *Cache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.db.Close()
}

// get читает значение из бакета с проверкой TTL.
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

// set записывает значение в бакет с TTL.
func (c *Cache) set(bucket []byte, key string, value any, ttl time.Duration) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
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

// GetASN получает кэшированный ASN для IP-адреса.
func (c *Cache) GetASN(ip string) (int, bool) {
	var asn int
	return asn, c.get(bucketASN, ip, &asn)
}

// SetASN кэширует ASN для IP с TTL.
func (c *Cache) SetASN(ip string, asn int, ttl time.Duration) error {
	return c.set(bucketASN, ip, asn, ttl)
}

// GetPrefixes получает кэшированные префиксы для ASN.
func (c *Cache) GetPrefixes(asn int) ([]string, bool) {
	var prefixes []string
	return prefixes, c.get(bucketPrefixes, fmt.Sprintf("%d", asn), &prefixes)
}

// SetPrefixes кэширует префиксы для ASN с TTL.
func (c *Cache) SetPrefixes(asn int, prefixes []string, ttl time.Duration) error {
	return c.set(bucketPrefixes, fmt.Sprintf("%d", asn), prefixes, ttl)
}

// PurgeExpired удаляет просроченные записи из кэш-бакетов.
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

// Put записывает значение в указанный бакет.
func (c *Cache) Put(bucket, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
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

// Get читает значение из указанного бакета.
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
			return fmt.Errorf("key not found in %s", bucket)
		}
		return json.Unmarshal(data, v)
	})
}

// snapshotEnvelope — формат хранения снапшота в bbolt.
type snapshotEnvelope struct {
	CreatedAt time.Time `json:"created_at"`
	Prefixes  []string  `json:"prefixes"`
}

// CreateSnapshot создаёт новый снапшот (список CIDR-строк) и возвращает его ID.
func (c *Cache) CreateSnapshot(service string, prefixes []string) (string, error) {
	env := snapshotEnvelope{CreatedAt: time.Now().UTC(), Prefixes: prefixes}
	data, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
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

// ListSnapshots возвращает список снапшотов для сервиса (новые первыми).
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
		for key, val := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, val = cursor.Next() {
			snapshotID := string(key[len(prefix):])
			info := SnapshotInfo{ID: snapshotID, Service: service}
			var env snapshotEnvelope
			if err := json.Unmarshal(val, &env); err == nil {
				info.CreatedAt = env.CreatedAt
				info.Count = len(env.Prefixes)
			}
			snapshots = append(snapshots, info)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Сортировка: новые первыми
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].ID > snapshots[j].ID
	})
	return snapshots, nil
}

// GetSnapshot читает список CIDR снапшота в v (*[]string).
func (c *Cache) GetSnapshot(service, snapshotID string, v any) error {
	key := fmt.Sprintf("%s:%s", service, snapshotID)

	var raw []byte
	c.mu.RLock()
	err := c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSnapshots)
		if b == nil {
			return fmt.Errorf("snapshots bucket not found")
		}
		data := b.Get([]byte(key))
		if data == nil {
			return fmt.Errorf("snapshot %s not found", snapshotID)
		}
		raw = append([]byte(nil), data...)
		return nil
	})
	c.mu.RUnlock()
	if err != nil {
		return err
	}

	// Формат: {"created_at":..., "prefixes":[...]};
	// обратная совместимость со старым "просто []string".
	var env snapshotEnvelope
	if json.Unmarshal(raw, &env) == nil && env.Prefixes != nil {
		b, _ := json.Marshal(env.Prefixes)
		return json.Unmarshal(b, v)
	}
	return json.Unmarshal(raw, v)
}

// DeleteSnapshot удаляет конкретный снапшот.
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
