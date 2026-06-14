// Package cache is a bbolt content-hash cache: identical (task+input+params+
// model+grammar) requests return the stored result and skip the model entirely.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

var bucket = []byte("results")

type Cache struct{ db *bolt.DB }

// Open opens (creating if needed) the cache db.
func Open(path string) (*Cache, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		_, e := tx.CreateBucketIfNotExists(bucket)
		return e
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Cache{db: db}, nil
}

func (c *Cache) Close() error { return c.db.Close() }

// Key derives a stable cache key from the given parts.
func Key(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}

// Get returns the stored value and true if present.
func (c *Cache) Get(key string) ([]byte, bool) {
	var out []byte
	_ = c.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucket).Get([]byte(key))
		if v != nil {
			out = append([]byte(nil), v...)
		}
		return nil
	})
	return out, out != nil
}

// Put stores val under key.
func (c *Cache) Put(key string, val []byte) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Put([]byte(key), val)
	})
}
