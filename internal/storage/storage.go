package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

var cacheBucket = []byte("cache")

type Cache struct {
	db *bolt.DB
}

type cacheEntry struct {
	Value     []byte    `json:"value"`
	ExpiresAt time.Time `json:"expires_at"`
}

func Open(path string) (*Cache, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, err
	}

	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(cacheBucket)
		return err
	})
	if err != nil {
		db.Close()
		return nil, err
	}

	return &Cache{db: db}, nil
}

func (c *Cache) Close() error {
	if c.db != nil {
		return c.db.Close()
	}
	return nil
}

func (c *Cache) Get(key string) ([]byte, bool) {
	var result []byte

	c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(cacheBucket)
		if b == nil {
			return nil
		}

		v := b.Get([]byte(key))
		if v == nil {
			return nil
		}

		var entry cacheEntry
		if err := json.Unmarshal(v, &entry); err != nil {
			return nil
		}

		if time.Now().After(entry.ExpiresAt) {
			return nil
		}

		result = entry.Value
		return nil
	})

	return result, result != nil
}

func (c *Cache) Set(key string, value []byte, ttl time.Duration) error {
	entry := cacheEntry{
		Value:     value,
		ExpiresAt: time.Now().Add(ttl),
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	return c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(cacheBucket)
		return b.Put([]byte(key), data)
	})
}

func (c *Cache) Delete(key string) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(cacheBucket)
		return b.Delete([]byte(key))
	})
}
