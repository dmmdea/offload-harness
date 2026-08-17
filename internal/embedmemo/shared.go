package embedmemo

import "sync"

// Shared returns one process-wide Memo per db path.
//
// WHY A SINGLETON AND NOT A PLAIN Open PER CALLER
//
// bbolt takes an exclusive file lock. A single harness process legitimately
// builds SEVERAL pipelines — the main one plus a recordless/in-loop one — and
// several CLI subcommands construct an embedder of their own. If each called
// Open, the first would win the lock and every later one would time out after a
// second and silently fall back to pass-through. The result would be a memo that
// "works" while quietly serving nothing to most of its callers, and a full
// second of startup latency paid per failed open — a slow, silent, self-inflicted
// miss. Sharing one handle makes every in-process caller a real participant.
//
// Cross-PROCESS contention is a different matter and is still expected: a CLI run
// while the MCP server holds the file gets (nil, err) and runs without a memo.
// That is the same graceful degradation the result cache already documents.
//
// The error from the first attempt is cached alongside the handle, so a locked
// file costs one bolt timeout for the whole process instead of one per caller.
var (
	sharedMu sync.Mutex
	shared   = map[string]*sharedEntry{}
)

type sharedEntry struct {
	memo *Memo
	err  error
}

// Shared opens (once per path) and returns the process-wide memo. Callers must
// NOT Close the returned Memo — its lifetime is the process's. Use CloseShared
// from the owning binary's shutdown path.
func Shared(path, embedderID, epoch string, maxEntries int) (*Memo, error) {
	if path == "" {
		return nil, ErrDisabled
	}
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if e, ok := shared[path]; ok {
		return e.memo, e.err
	}
	m, err := Open(path, embedderID, epoch, maxEntries)
	shared[path] = &sharedEntry{memo: m, err: err}
	return m, err
}

// CloseShared flushes and closes every shared memo. Safe to call more than once.
// Flush is attempted even when Close later fails, and a Flush error never
// prevents the Close — losing a hit counter is trivial next to leaking a file
// lock that would lock out the next process.
func CloseShared() {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	for path, e := range shared {
		if e.memo != nil {
			_ = e.memo.Flush()
			_ = e.memo.Close()
		}
		delete(shared, path)
	}
}
