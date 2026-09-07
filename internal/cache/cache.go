// Package cache is a bbolt content-hash cache: identical (task+input+params+
// model+grammar) requests return the stored result and skip the model entirely.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

var bucket = []byte("results")

type Cache struct {
	db *bolt.DB
	// path is the file actually opened; fallback is true when it is a per-process
	// sibling of the configured path (OpenPreferred).
	path     string
	fallback bool
}

// Path is the file this cache actually opened (the configured path, or the
// per-process sibling OpenPreferred fell back to).
func (c *Cache) Path() string { return c.path }

// Fallback reports whether this cache is a per-process sibling rather than the
// configured, shared file.
func (c *Cache) Fallback() bool { return c.fallback }

// SiblingMaxAge is how old a per-process sibling cache may be before
// OpenPreferred sweeps it. Pids are reused on both Windows and Linux, so
// liveness cannot say whether a sibling's owner is gone — age can: an MCP
// server that outlives this window is rare, and a sibling it still holds
// cannot be removed anyway (the sweep tolerates EBUSY/EPERM).
const SiblingMaxAge = 12 * time.Hour

// OpenPreferred opens path; when another harness process already holds its
// exclusive bbolt lock (bolt.ErrTimeout) it opens a per-process sibling
// "<stem>.p<pid>.db" beside it instead, so a session whose MCP server lost the
// race still gets its in-loop hits (agent_run's repeated identical offloads)
// rather than running cache-less for its whole life. Measured 2026-09-07: six
// harness processes on the workstation, one holding cache.db, five reporting
// "not opened (held by another process)". Returns the cache, the path it
// actually opened, whether it fell back, and the error when neither could be
// opened. Stale siblings are swept best-effort on every fallback open.
func OpenPreferred(path string) (c *Cache, used string, fellBack bool, err error) {
	// The sweep runs on EVERY open, not only on the fallback path: siblings
	// created during a contention window would otherwise be revisited only by a
	// later process that also lost the lock, i.e. never once contention ends.
	sweepStaleSiblings(path)
	c, err = Open(path)
	if err == nil {
		return c, path, false, nil
	}
	if !errors.Is(err, bolt.ErrTimeout) {
		return nil, "", false, err
	}
	sib := siblingPath(path, os.Getpid())
	c, serr := Open(sib)
	if serr != nil {
		return nil, "", false, fmt.Errorf("%w; per-process fallback %s: %v", err, sib, serr)
	}
	c.path, c.fallback = sib, true
	return c, sib, true, nil
}

// siblingPath is "<dir>/<stem>.p<pid><ext>" beside the configured path.
func siblingPath(path string, pid int) string {
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	return fmt.Sprintf("%s.p%d%s", stem, pid, ext)
}

// sweepStaleSiblings removes per-process siblings of path older than
// SiblingMaxAge. ONLY files this package itself would have named are eligible:
// "<stem>.p<digits><ext>", with the digits parsed as a pid — an operator's
// "cache.prev.db" or "cache.patched.db" beside the cache is never touched (the
// first draft's "<stem>.p*<ext>" glob would have removed them; review finding
// 2026-09-07). Best-effort: a younger sibling is left alone, and a removal
// that fails (Windows refuses to unlink a file another process holds open) is
// ignored. On Linux an unlink of a still-open sibling succeeds and merely
// detaches its directory entry — the live holder keeps working on its open
// descriptor, so the age rule costs it nothing but the name.
func sweepStaleSiblings(path string) {
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(filepath.Base(path), ext)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), stem+".p[0-9]*"+ext))
	own := siblingPath(path, os.Getpid())
	for _, m := range matches {
		if m == own || !isSiblingName(filepath.Base(m), stem, ext) {
			continue
		}
		st, serr := os.Stat(m)
		if serr != nil || time.Since(st.ModTime()) < SiblingMaxAge {
			continue
		}
		_ = os.Remove(m)
	}
}

// isSiblingName reports whether base is exactly "<stem>.p<pid><ext>" with a
// numeric pid — the one shape this package creates.
func isSiblingName(base, stem, ext string) bool {
	if !strings.HasPrefix(base, stem+".p") || !strings.HasSuffix(base, ext) {
		return false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(base, stem+".p"), ext)
	if mid == "" {
		return false
	}
	_, err := strconv.Atoi(mid)
	return err == nil && !strings.HasPrefix(mid, "-") && !strings.HasPrefix(mid, "+")
}

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
	return &Cache{db: db, path: path}, nil
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

// Count returns how many entries the cache holds.
//
// Exists so a test can assert "nothing was stored" against the STORE rather than
// inferring it from a call count — the difference between a real gate test and
// one that passes because the code under test was never reached.
func (c *Cache) Count() (int, error) {
	n := 0
	err := c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return nil
		}
		n = b.Stats().KeyN
		return nil
	})
	return n, err
}
