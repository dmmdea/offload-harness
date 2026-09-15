// Package cache is a bbolt content-hash cache: identical (task+input+params+
// model+grammar) requests return the stored result and skip the model entirely.
//
// # Why the handle is lazy (register D-05, 0.117.8)
//
// bbolt takes an EXCLUSIVE file lock for the whole life of a read-write handle,
// and every harness process used to grab that lock while CONSTRUCTING its
// pipeline — before knowing whether it would ever run a cacheable task. With one
// MCP server per session (14 measured 2026-08-25, 21 live 2026-09-14) exactly one
// process won the primary and every other one immediately created a 32 KB
// per-process sibling it then never wrote a single entry into: 49 sibling files
// on the workstation at ~12/h, swept only after 12 h.
//
// The fix is not a lock-free store or a daemon in front of it — it is to stop
// opening a file nobody asked for. A Cache is now a handle that resolves on FIRST
// USE; a process that never runs a cacheable task never touches the cache
// directory at all. What remains:
//
//   - Get/Put resolve the handle once, with a short lock Timeout so a held
//     primary costs milliseconds rather than a second.
//   - Pure-read surfaces take a READ-ONLY handle (NewReader), which never
//     creates a file: not the primary, not a sibling. See resolveReaderLocked for
//     the bbolt locking semantics that force the read-through design.
//   - A sibling that turns out to be empty is deleted on Close, and a non-empty
//     one makes one bounded attempt to promote its entries into the primary, so
//     a session's work is not stranded in a file nobody else reads.
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
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

var bucket = []byte("results")

// Mode names how a handle is backed. It is reported verbatim by
// offload_status.reuse.result_cache so an operator reading "no cache" can tell
// the very different reasons apart.
type Mode string

const (
	// ModeUnopened is a lazy handle nothing has needed yet. It is NOT a failure:
	// it is the expected steady state of an MCP server that has served only
	// status and fleet calls, and it is the state that stops the sibling churn.
	ModeUnopened Mode = "unopened"
	// ModePrimary holds the configured file read-write: the shared cache.
	ModePrimary Mode = "primary"
	// ModeSibling holds this process's own "<stem>.p<pid><ext>" read-write
	// because another process holds the primary. Hits are in-process only.
	ModeSibling Mode = "sibling"
	// ModeReadOnly holds a file read-only: a read-through reader. Path says which
	// file it reached (the primary, or this process's own sibling).
	ModeReadOnly Mode = "readonly"
	// ModeUnavailable means no file could be opened — disk, permissions, a bad
	// path, or (for a reader) a primary held by a writer with no sibling to fall
	// through to. A reader in this mode answers every Get with a miss.
	ModeUnavailable Mode = "unavailable"
)

// LockTimeout is how long an open waits for bbolt's file lock before giving up.
//
// Deliberately short (it was 1 s): with a lazy handle the only opens left are
// ones that will really use the cache, and a process that has lost the race
// should reach its sibling — or, for a reader, its miss — in milliseconds rather
// than stalling a tool call for a second per attempt. bbolt retries the lock
// every flockRetryTimeout (50 ms), so this is still four or five attempts.
const LockTimeout = 250 * time.Millisecond

// SiblingMaxAge is how old a per-process sibling may be before the sweep removes
// it. Pids are reused on both Windows and Linux, so liveness cannot say whether
// a sibling's owner is gone — age can.
//
// 0.117.8: 12 h -> 1 h. The long window existed to protect a long-lived MCP
// server's sibling, and that protection is not what keeps a live sibling alive:
// a sibling a process still holds cannot be removed on Windows (the sweep
// tolerates EBUSY/EPERM), and on Linux the unlink merely detaches the directory
// entry while the holder keeps working — and keeps its entries, since Close
// promotes from the open descriptor, not from the name.
const SiblingMaxAge = time.Hour

// PromotionMaxKeys and PromotionMaxDuration bound the one promotion attempt a
// non-empty sibling makes on Close. Process exit is not the place for an
// unbounded copy: a sibling that has accumulated more than this keeps its
// surplus and is swept on age like any other.
const (
	PromotionMaxKeys     = 1000
	PromotionMaxDuration = 5 * time.Second
)

// ErrReadOnly is returned by Put on a handle built with NewReader. A read-only
// surface that tries to write is a wiring mistake, and the whole point of the
// read-only path is that it never creates a sibling — so this fails loudly
// rather than silently opening one.
var ErrReadOnly = errors.New("cache: handle is read-only")

// Cache is a lazily-resolved handle to the result cache. The zero value is not
// usable; build one with New, NewReader or Open.
type Cache struct {
	configured string
	readOnly   bool
	notify     func(Mode, string, error)

	mu       sync.RWMutex
	resolved bool
	db       *bolt.DB
	path     string
	mode     Mode
	openErr  error
}

// New returns a lazy read-write handle for path. It touches no file until the
// first Get/Put/Count, except for the stale-sibling sweep below.
//
// The sweep runs at CONSTRUCTION, not at resolve: it is a directory glob and a
// stat, it takes no lock and creates nothing, and running it here is what keeps
// the cleanup alive now that most processes never resolve their handle at all.
// A sweep that only ran on a real open would, after this change, essentially
// never run.
func New(path string) *Cache {
	sweepStaleSiblings(path)
	return &Cache{configured: path, mode: ModeUnopened}
}

// NewReader returns a lazy READ-THROUGH handle: it never creates a file and
// never writes one. Use it for surfaces that only report or look up — status,
// inspection — so that merely asking what the cache holds cannot itself be the
// thing that litters the cache directory.
func NewReader(path string) *Cache {
	sweepStaleSiblings(path)
	return &Cache{configured: path, readOnly: true, mode: ModeUnopened}
}

// WithNotify registers a callback invoked once, at resolve time, when the handle
// lands on anything other than the primary. It exists so the binaries can keep
// printing the operator note they printed when the open was eager — at the
// moment it becomes true, instead of at startup when it was merely predicted.
func (c *Cache) WithNotify(f func(mode Mode, path string, err error)) *Cache {
	c.notify = f
	return c
}

// Open opens path read-write eagerly and fails if it cannot. Kept for callers
// that genuinely need the handle now and want the error now (tests, and any
// tool whose whole job is the store itself).
func Open(path string) (*Cache, error) {
	db, err := openWritable(path, LockTimeout)
	if err != nil {
		return nil, err
	}
	return &Cache{configured: path, db: db, path: path, mode: ModePrimary, resolved: true}, nil
}

// Path is the file this handle actually opened, or "" while it is unopened.
func (c *Cache) Path() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.path
}

// Configured is the cache path this handle was built for, whatever it resolved to.
func (c *Cache) Configured() string { return c.configured }

// Mode reports how the handle is backed WITHOUT resolving it: asking a status
// tool what the cache is doing must not be the call that takes the lock.
func (c *Cache) Mode() Mode {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.mode
}

// OpenErr is why the handle is ModeUnavailable, or nil.
func (c *Cache) OpenErr() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.openErr
}

// Fallback reports whether this handle is a per-process sibling rather than the
// configured, shared file.
func (c *Cache) Fallback() bool { return c.Mode() == ModeSibling }

// handle resolves the cache on first use and returns the open db (nil when the
// handle is unavailable or closed).
func (c *Cache) handle() *bolt.DB {
	c.mu.RLock()
	if c.resolved {
		db := c.db
		c.mu.RUnlock()
		return db
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.resolved {
		c.resolveLocked()
	}
	return c.db
}

// resolveLocked makes the ONE open attempt this handle ever makes.
func (c *Cache) resolveLocked() {
	c.resolved = true
	if c.configured == "" {
		c.mode, c.openErr = ModeUnavailable, errors.New("cache: no path configured")
		return
	}
	if c.readOnly {
		c.resolveReaderLocked()
		return
	}
	db, err := openWritable(c.configured, LockTimeout)
	if err == nil {
		c.db, c.path, c.mode = db, c.configured, ModePrimary
		return
	}
	if !errors.Is(err, bolt.ErrTimeout) {
		// A bad path or a permissions problem is NOT lock contention, and
		// falling back would report the wrong diagnosis for the rest of the run.
		c.mode, c.openErr = ModeUnavailable, err
		c.notifyLocked()
		return
	}
	sib := siblingPath(c.configured, os.Getpid())
	sdb, serr := openWritable(sib, LockTimeout)
	if serr != nil {
		c.mode = ModeUnavailable
		c.openErr = fmt.Errorf("%w; per-process fallback %s: %v", err, sib, serr)
		c.notifyLocked()
		return
	}
	c.db, c.path, c.mode = sdb, sib, ModeSibling
	c.notifyLocked()
}

// resolveReaderLocked is the read-through path, and its shape is dictated by
// what bbolt's ReadOnly option actually does rather than by what the name
// suggests.
//
// `go doc go.etcd.io/bbolt Options` says of ReadOnly: "Open database in
// read-only mode. Uses flock(..., LOCK_SH |LOCK_NB) to grab a shared lock
// (UNIX)" — and db.go's own comment at the flock call is the precise version:
// "The database file is locked exclusively (only one process can grab the lock)
// if !options.ReadOnly. The database file is locked using the shared lock (more
// than one process may hold a lock at the same time) otherwise". On Windows
// (bolt_windows.go) the same call is LockFileEx with LOCKFILE_FAIL_IMMEDIATELY,
// adding LOCKFILE_EXCLUSIVE_LOCK only when exclusive — so a read-only open there
// also asks for a genuine shared byte-range lock.
//
// The consequence: MANY read-only openers coexist with EACH OTHER, but a shared
// request still conflicts with a writer's exclusive hold (ERROR_LOCK_VIOLATION,
// retried to ErrTimeout). Since exactly one harness process holds the primary
// read-write at any time, a reader cannot assume the primary is reachable. It
// therefore reads THROUGH: primary if free, else this process's own sibling if
// one exists, else nothing — and "nothing" answers every Get with a miss. A miss
// is always a safe answer for a cache; creating a sibling to record one is not.
func (c *Cache) resolveReaderLocked() {
	db, err := openReadOnly(c.configured)
	if err == nil {
		c.db, c.path, c.mode = db, c.configured, ModeReadOnly
		return
	}
	c.openErr = err
	sib := siblingPath(c.configured, os.Getpid())
	if sdb, serr := openReadOnly(sib); serr == nil {
		c.db, c.path, c.mode = sdb, sib, ModeReadOnly
		c.openErr = nil
		return
	}
	c.mode = ModeUnavailable
	c.notifyLocked()
}

// notifyLocked fires the operator note for a non-primary resolve. Called with
// c.mu held, so the mode/path/err are passed IN: a callback that reached back
// through Mode() or OpenErr() would deadlock on this non-reentrant lock.
func (c *Cache) notifyLocked() {
	if c.notify != nil {
		c.notify(c.mode, c.path, c.openErr)
	}
}

func openWritable(path string, timeout time.Duration) (*bolt.DB, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: timeout})
	if err != nil {
		return nil, err
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, e := tx.CreateBucketIfNotExists(bucket)
		return e
	}); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// openReadOnly opens path read-only. bbolt's ReadOnly path uses os.O_RDONLY
// WITHOUT os.O_CREATE (db.go), which is the property the read-through design
// leans on: this can never bring a cache file into existence.
func openReadOnly(path string) (*bolt.DB, error) {
	return bolt.Open(path, 0o600, &bolt.Options{Timeout: LockTimeout, ReadOnly: true})
}

// siblingPath is "<dir>/<stem>.p<pid><ext>" beside the configured path.
func siblingPath(path string, pid int) string {
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	return fmt.Sprintf("%s.p%d%s", stem, pid, ext)
}

// siblingMatches lists the per-process siblings of path on disk. ONLY files this
// package itself would have named are eligible: "<stem>.p<digits><ext>", with
// the digits parsed as a pid — an operator's "cache.prev.db" or
// "cache.patched.db" beside the cache is never included (the first draft's
// "<stem>.p*<ext>" glob would have removed them; review finding 2026-09-07).
func siblingMatches(path string) []string {
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(filepath.Base(path), ext)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), stem+".p[0-9]*"+ext))
	var out []string
	for _, m := range matches {
		if isSiblingName(filepath.Base(m), stem, ext) {
			out = append(out, m)
		}
	}
	return out
}

// SiblingStats reports how many per-process siblings of path exist on disk and
// how many bytes they occupy, INCLUDING this process's own. It is the number the
// D-05 regression is measured in, so offload_status publishes it: a rising count
// is the defect coming back, and a count that stays low is the fix holding.
func SiblingStats(path string) (count int, bytes int64) {
	if path == "" {
		return 0, 0
	}
	for _, m := range siblingMatches(path) {
		st, err := os.Stat(m)
		if err != nil {
			continue
		}
		count++
		bytes += st.Size()
	}
	return count, bytes
}

// sweepStaleSiblings removes per-process siblings of path older than
// SiblingMaxAge. Best-effort: a younger sibling is left alone, and a removal
// that fails (Windows refuses to unlink a file another process holds open) is
// ignored. This process's own sibling is never swept.
func sweepStaleSiblings(path string) {
	if path == "" {
		return
	}
	own := siblingPath(path, os.Getpid())
	for _, m := range siblingMatches(path) {
		if m == own {
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

// Close releases the handle, and cleans up after a sibling.
//
// An EMPTY sibling is deleted outright: it is the 32 KB file the eager open used
// to leave behind on every losing process, and keeping it buys nothing. A
// NON-EMPTY one makes exactly one bounded attempt to promote its entries into
// the primary — if the primary's lock happens to be free at that moment — and is
// removed only when every entry made it across. When the primary is still held,
// the sibling stays exactly as it is and the age sweep eventually takes it.
func (c *Cache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	db, mode, path := c.db, c.mode, c.path
	c.db, c.resolved = nil, true
	if db == nil {
		return nil
	}
	if mode != ModeSibling {
		return db.Close()
	}
	n, cerr := countIn(db)
	if cerr == nil && n == 0 {
		err := db.Close()
		_ = os.Remove(path)
		return err
	}
	drained := promoteSibling(db, c.configured)
	err := db.Close()
	if drained {
		_ = os.Remove(path)
	}
	return err
}

// errPromotionBudget stops the collecting ForEach once a bound is reached. It
// never escapes promoteSibling.
var errPromotionBudget = errors.New("cache: promotion budget reached")

// promoteSibling copies up to PromotionMaxKeys entries from the still-open
// sibling into the configured primary, within PromotionMaxDuration, and reports
// whether the sibling was drained COMPLETELY (the only case in which the caller
// may delete it).
//
// Entries are collected first and written in a single transaction: a deadline
// checked inside the Update would roll the whole batch back on expiry, which
// turns a slow promotion into a lost one.
func promoteSibling(sib *bolt.DB, primary string) bool {
	if primary == "" {
		return false
	}
	deadline := time.Now().Add(PromotionMaxDuration)
	type kv struct{ k, v []byte }
	var entries []kv
	total := 0
	_ = sib.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return nil
		}
		total = b.Stats().KeyN
		return b.ForEach(func(k, v []byte) error {
			if len(entries) >= PromotionMaxKeys || time.Now().After(deadline) {
				return errPromotionBudget
			}
			entries = append(entries, kv{append([]byte(nil), k...), append([]byte(nil), v...)})
			return nil
		})
	})
	if len(entries) == 0 || time.Now().After(deadline) {
		return false
	}
	// ONE attempt: if the primary is held right now, the sibling keeps its
	// entries rather than blocking process exit on another process's lock.
	pdb, err := bolt.Open(primary, 0o600, &bolt.Options{Timeout: LockTimeout})
	if err != nil {
		return false
	}
	defer pdb.Close()
	if err := pdb.Update(func(tx *bolt.Tx) error {
		b, e := tx.CreateBucketIfNotExists(bucket)
		if e != nil {
			return e
		}
		for _, entry := range entries {
			if e := b.Put(entry.k, entry.v); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		return false
	}
	return len(entries) == total
}

// Key derives a stable cache key from the given parts.
func Key(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}

// Get returns the stored value and true if present. An unavailable handle — a
// reader that could reach neither the primary nor a sibling — answers every Get
// with a miss, which costs a model call and never a wrong result.
func (c *Cache) Get(key string) ([]byte, bool) {
	db := c.handle()
	if db == nil {
		return nil, false
	}
	var out []byte
	_ = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return nil
		}
		if v := b.Get([]byte(key)); v != nil {
			out = append([]byte(nil), v...)
		}
		return nil
	})
	return out, out != nil
}

// Put stores val under key.
func (c *Cache) Put(key string, val []byte) error {
	if c.readOnly {
		return ErrReadOnly
	}
	db := c.handle()
	if db == nil {
		if err := c.OpenErr(); err != nil {
			return err
		}
		return errors.New("cache: unavailable")
	}
	return db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucket)
		if err != nil {
			return err
		}
		return b.Put([]byte(key), val)
	})
}

// Count returns how many entries the cache holds. Note this RESOLVES a lazy
// handle: it is a question about the store, not about the handle.
//
// Exists so a test can assert "nothing was stored" against the STORE rather than
// inferring it from a call count — the difference between a real gate test and
// one that passes because the code under test was never reached.
func (c *Cache) Count() (int, error) {
	db := c.handle()
	if db == nil {
		if err := c.OpenErr(); err != nil {
			return 0, err
		}
		return 0, errors.New("cache: unavailable")
	}
	return countIn(db)
}

func countIn(db *bolt.DB) (int, error) {
	n := 0
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return nil
		}
		n = b.Stats().KeyN
		return nil
	})
	return n, err
}
