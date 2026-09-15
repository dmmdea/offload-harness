package agent

import (
	"fmt"
	"path/filepath"
	"sort"
	"sync"
)

// WriteLimit is the per-run ceiling on a delegated write door: how many
// distinct files one contract may touch and how many bytes it may write in
// total (register D-06). It is enforced at the TOOL, before the bytes reach
// the disk, so a seat that runs away is told "NOT performed" and can correct —
// a cap discovered only after the run is a cap the node already paid for.
//
// The delegation door also re-counts the finished write set against the same
// numbers. Two enforcement points on purpose: this one bounds the node's disk
// and memory while the loop runs; the second one bounds what crosses the wire,
// and neither can be the other's proof.
//
// A nil *WriteLimit admits everything. That is the CLI door (local-agent
// --allow-write), which is an operator at a keyboard with their own worktree
// and has never had a byte cap; giving it one here would be an unannounced
// behaviour change to a shipped tool.
type WriteLimit struct {
	maxFiles int
	maxBytes int

	mu      sync.Mutex
	touched map[string]bool
	bytes   int
}

// NewWriteLimit builds a limit. Non-positive bounds are treated as "no bound
// of that kind" rather than "nothing may be written": a zero ceiling that
// silently refused every write would read in the logs exactly like a broken
// seat.
func NewWriteLimit(maxFiles, maxBytes int) *WriteLimit {
	return &WriteLimit{maxFiles: maxFiles, maxBytes: maxBytes, touched: map[string]bool{}}
}

// Admit reserves a write of n bytes to rel (a worktree-relative path) and
// returns the refusal reason, or "" when the write may proceed. The reservation
// is made BEFORE the write and is NOT released if the write then fails on IO:
// the accounting is deliberately conservative, because the alternative — a
// refund path — is a second place for a runaway to slip through.
//
// Re-writing a file already touched does not spend another file slot, so an
// edit-then-fix cycle on one file stays within a one-file budget; its bytes are
// charged again, because they were written again.
func (l *WriteLimit) Admit(rel string, n int) string {
	if l == nil {
		return ""
	}
	key := filepath.ToSlash(filepath.Clean(rel))
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.touched == nil {
		l.touched = map[string]bool{}
	}
	if l.maxFiles > 0 && !l.touched[key] && len(l.touched) >= l.maxFiles {
		return fmt.Sprintf("the write budget for this run allows %d file(s) and %s would be number %d — already touched: %v",
			l.maxFiles, key, len(l.touched)+1, l.namesLocked())
	}
	if l.maxBytes > 0 && l.bytes+n > l.maxBytes {
		return fmt.Sprintf("the write budget for this run allows %d bytes; %d are already written and %s would add %d — make a smaller, more surgical change",
			l.maxBytes, l.bytes, key, n)
	}
	l.touched[key] = true
	l.bytes += n
	return ""
}

// Files reports how many distinct paths have been admitted.
func (l *WriteLimit) Files() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.touched)
}

// Bytes reports how many bytes have been admitted.
func (l *WriteLimit) Bytes() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bytes
}

// Names lists the admitted paths, sorted.
func (l *WriteLimit) Names() []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.namesLocked()
}

// namesLocked is Names for callers that already hold l.mu (Admit builds its
// refusal message while holding it — re-locking would deadlock, and dropping
// the lock to build a message would report a set nobody ever had).
func (l *WriteLimit) namesLocked() []string {
	out := make([]string, 0, len(l.touched))
	for p := range l.touched {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
