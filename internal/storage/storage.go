package storage

import (
	"encoding/json"
	"time"

	bolt "go.etcd.io/bbolt"
)

type Cache struct{ db *bolt.DB }
type Snapshot struct {
	ID, Service string
	Created     time.Time
	Routes      json.RawMessage
}

func Open(path string) (*Cache, error) {
	db, e := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if e != nil {
		return nil, e
	}
	c := &Cache{db: db}
	e = db.Update(func(tx *bolt.Tx) error {
		for _, b := range []string{"cache", "snapshots", "history", "transactions"} {
			if _, e := tx.CreateBucketIfNotExists([]byte(b)); e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		db.Close()
		return nil, e
	}
	return c, nil
}
func (c *Cache) Close() error { return c.db.Close() }
func (c *Cache) Put(bucket, key string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return c.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(bucket)).Put([]byte(key), b) })
}
func (c *Cache) Get(bucket, key string, v any) error {
	return c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket)).Get([]byte(key))
		if b == nil {
			return bolt.ErrBucketNotFound
		}
		return json.Unmarshal(b, v)
	})
}
