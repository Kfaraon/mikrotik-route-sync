package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

var bucketCache = []byte("cache")

type Cache struct{ db *bolt.DB }
type entry struct {
	Value     json.RawMessage `json:"value"`
	ExpiresAt time.Time       `json:"expires_at"`
}

func Open(path string) (*Cache, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, err
	}
	if err := db.Update(func(tx *bolt.Tx) error { _, err := tx.CreateBucketIfNotExists(bucketCache); return err }); err != nil {
		db.Close()
		return nil, err
	}
	return &Cache{db: db}, nil
}
func (c *Cache) Close() error { return c.db.Close() }
func (c *Cache) Get(key string, dst any) bool {
	ok := false
	_ = c.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketCache).Get([]byte(key))
		if raw == nil {
			return nil
		}
		var e entry
		if json.Unmarshal(raw, &e) != nil || time.Now().After(e.ExpiresAt) {
			return nil
		}
		if json.Unmarshal(e.Value, dst) == nil {
			ok = true
		}
		return nil
	})
	return ok
}
func (c *Cache) Set(key string, value any, ttl time.Duration) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	e := entry{Value: raw, ExpiresAt: time.Now().Add(ttl)}
	enc, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return c.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketCache).Put([]byte(key), enc) })
}
func (c *Cache) Delete(key string) error {
	return c.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketCache).Delete([]byte(key)) })
}
func (c *Cache) PurgeExpired() error {
	now := time.Now()
	return c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketCache)
		var del [][]byte
		if err := b.ForEach(func(k, v []byte) error {
			var e entry
			if json.Unmarshal(v, &e) != nil || now.After(e.ExpiresAt) {
				del = append(del, append([]byte(nil), k...))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, k := range del {
			if err := b.Delete(k); err != nil {
				return fmt.Errorf("delete cache key: %w", err)
			}
		}
		return nil
	})
}
