package embedmemo

import (
	"errors"
	"sync"

	bolt "go.etcd.io/bbolt"
)

// Shared returns one process-wide Memo per (path, embedderID, epoch).
//
// # WHY A SINGLETON AND NOT A PLAIN Open PER CALLER
//
// bbolt takes an exclusive file lock. A single harness process legitimately
// builds SEVERAL pipelines — the main one plus an in-loop one — and several CLI
// subcommands construct an embedder of their own. If each called Open, the first
// would win the lock and every later one would time out after a second and
// silently fall back to pass-through. The result would be a memo that "works"
// while quietly serving nothing to most of its callers, and a full second of
// startup latency paid per failed open. Sharing one handle makes every
// in-process caller a real participant.
//
// # WHY THE KEY IS THE FULL IDENTITY AND NOT JUST THE PATH
//
// Keying on the path alone meant a second caller asking for a DIFFERENT embedder
// id or epoch received the first caller's handle — and was then served the other
// model's vectors, silently, in a cosine routine. That voids this package's
// central guarantee. Two callers wanting different identities on one file cannot
// both be served (bbolt allows one writer), so the second gets
// ErrIdentityMismatch and runs without a memo. Refusing is the only honest
// option: handing back a mismatched handle is not a degradation, it is a wrong
// answer.
//
// Cross-PROCESS contention is a different matter and is still expected: a CLI run
// while the MCP server holds the file gets (nil, err) and runs without a memo.
var (
	sharedMu sync.Mutex
	shared   = map[string]*sharedEntry{}
)

type sharedEntry struct {
	memo *Memo
	err  error
}

func sharedKey(path, embedderID, epoch string) string {
	return path + "\x00" + embedderID + "\x00" + epoch
}

// Shared opens (once per identity) and returns the process-wide memo. Callers
// must NOT Close the returned Memo — its lifetime is the process's. Use
// CloseShared from the owning binary's shutdown path.
func Shared(path, embedderID, epoch string, maxEntries int) (*Memo, error) {
	if path == "" {
		return nil, ErrDisabled
	}
	sharedMu.Lock()
	defer sharedMu.Unlock()

	// A handle already open on this file under a different identity must not be
	// reused, and must not be shadowed by a second open that would deadlock on
	// bbolt's exclusive lock. Report it.
	for k, e := range shared {
		if e.memo == nil {
			continue
		}
		if pathOfKey(k) == path && (e.memo.embedderID != embedderID || e.memo.epoch != epoch) {
			return nil, ErrIdentityMismatch
		}
	}

	sk := sharedKey(path, embedderID, epoch)
	if e, ok := shared[sk]; ok {
		// A cached TIMEOUT is a transient condition (another process held the
		// lock at that instant) and must not disable the memo for the rest of
		// this process's life — which, for a long-running MCP server, meant a
		// single `loupe` run at startup permanently switched the feature off.
		// Any other error is a real property of the file, so it stays cached.
		if e.err == nil || !errors.Is(e.err, bolt.ErrTimeout) {
			return e.memo, e.err
		}
		delete(shared, sk)
	}
	m, err := Open(path, embedderID, epoch, maxEntries)
	shared[sk] = &sharedEntry{memo: m, err: err}
	return m, err
}

func pathOfKey(k string) string {
	for i := 0; i < len(k); i++ {
		if k[i] == 0 {
			return k[:i]
		}
	}
	return k
}

// FlushShared persists every shared memo's counters WITHOUT closing them.
//
// For long-running processes: a shutdown-only flush bounds the loss to the whole
// process lifetime, and this one can run for weeks. A `kill -9` (or any exit that
// skips defers) then destroys every hit counter accumulated since start, and the
// hit-rate gate reads a fabricated zero. Calling this periodically bounds the
// loss to one interval.
func FlushShared() error {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	var firstErr error
	for _, e := range shared {
		if e.memo != nil {
			if err := e.memo.Flush(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// CloseShared flushes and closes every shared memo, returning the first error
// encountered.
//
// EVERY BINARY THAT MAY OPEN A MEMO MUST CALL THIS ON SHUTDOWN. Flush is the
// only writer of the persisted hit/miss totals, so a process that exits without
// it leaves them at zero — and the reporting surfaces then state, in the one
// number the Phase 0.4 gate reads, that a memo serving thousands of hits was
// "never consulted".
//
// Flush is attempted even when Close later fails, and a Flush error never
// prevents the Close — leaking a file lock would lock out the next process.
func CloseShared() error {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	var firstErr error
	for k, e := range shared {
		if e.memo != nil {
			if err := e.memo.Flush(); err != nil && firstErr == nil {
				firstErr = err
			}
			if err := e.memo.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		delete(shared, k)
	}
	return firstErr
}
