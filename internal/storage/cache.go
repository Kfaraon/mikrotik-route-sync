package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
	bolt "go.etcd.io/bbolt"
)

var bucketASN = []byte("asn_by_ip")
var bucketPrefixes = []byte("prefixes_by_asn")

type Cache struct { db *bolt.DB }
type cachedEntry struct { Value json.RawMessage `json:"value"`; ExpiresAt time.Time `json:"expires_at"` }

func Open(path string) (*Cache, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { return nil, err }
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil { return nil, err }
	_ = db.Update(func(tx *bolt.Tx) error {
		tx.CreateBucketIfNotExists(bucketASN)
		tx.CreateBucketIfNotExists(bucketPrefixes)
		return nil
	})
	return &Cache{db: db}, nil
}

func (c *Cache) Close() error { return c.db.Close() }

func (c *Cache) GetASN(ip string) (int, bool) {
	var asn int; return asn, c.get(bucketASN, ip, &asn)
}
func (c *Cache) SetASN(ip string, asn int, ttl time.Duration) error { return c.set(bucketASN, ip, asn, ttl) }

func (c *Cache) GetPrefixes(asn int) ([]string, bool) {
	var p []string; return p, c.get(bucketPrefixes, fmt.Sprintf("%d", asn), &p)
}
func (c *Cache) SetPrefixes(asn int, prefixes []string, ttl time.Duration) error {
	return c.set(bucketPrefixes, fmt.Sprintf("%d", asn), prefixes, ttl)
}

func (c *Cache) get(bucket []byte, key string, dst any) bool {
	var found bool
	_ = c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil { return nil }
		raw := b.Get([]byte(key))
		if raw == nil { return nil }
		var entry cachedEntry
		if err := json.Unmarshal(raw, &entry); err != nil { return nil }
		if time.Now().After(entry.ExpiresAt) { return nil }
		if err := json.Unmarshal(entry.Value, dst); err != nil { return nil }
		found = true; return nil
	})
	return found
}

func (c *Cache) set(bucket []byte, key string, value any, ttl time.Duration) error {
	raw, _ := json.Marshal(value)
	entry := cachedEntry{Value: raw, ExpiresAt: time.Now().Add(ttl)}
	encoded, _ := json.Marshal(entry)
	return c.db.Update(func(tx *bolt.Tx) error {
		b, _ := tx.CreateBucketIfNotExists(bucket)
		return b.Put([]byte(key), encoded)
	})
}

func (c *Cache) PurgeExpired() error {
	return c.db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketASN, bucketPrefixes} {
			b := tx.Bucket(name)
			if b == nil { continue }
			var toDelete [][]byte
			_ = b.ForEach(func(k, v []byte) error {
				var entry cachedEntry
				if err := json.Unmarshal(v, &entry); err != nil || time.Now().After(entry.ExpiresAt) {
					toDelete = append(toDelete, append([]byte(nil), k...))
				}
				return nil
			})
			for _, k := range toDelete { b.Delete(k) }
		}
		return nil
	})
}