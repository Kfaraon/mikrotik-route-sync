package storage

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

// SnapshotInfo представляет метаданные snapshot
type SnapshotInfo struct {
	ID        string    `json:"id"`
	Service   string    `json:"service"`
	CreatedAt time.Time `json:"created_at"`
	Count     int       `json:"route_count"`
}

// CreateSnapshot создаёт новый snapshot и возвращает его ID
func (c *Cache) CreateSnapshot(service string, routes any) (string, error) {
	data, err := json.Marshal(routes)
	if err != nil {
		return "", fmt.Errorf("marshal routes: %w", err)
	}

	snapshotID := fmt.Sprintf("%d", time.Now().UnixNano())
	key := fmt.Sprintf("%s:%s", service, snapshotID)

	err = c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("snapshots"))
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
	var snapshots []SnapshotInfo

	err := c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("snapshots"))
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
	return c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("snapshots"))
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

	err := c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("snapshots"))
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

// splitKey разделяет ключ на части
func splitKey(key string) []string {
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == ':' {
			return []string{key[:i], key[i+1:]}
		}
	}
	return []string{key}
}

// CountSnapshots возвращает количество snapshots для сервиса
func (c *Cache) CountSnapshots(service string) (int, error) {
	count := 0
	err := c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("snapshots"))
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
