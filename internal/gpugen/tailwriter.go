package gpugen

import (
	"fmt"
	"sync"
)

// tailWriterCap bounds how many bytes a tailWriter retains. 256 KiB is large
// enough for any real stage log tail — including a multi-KB stack trace, the
// thing the error-format tail(o, 400) actually needs to find — while small
// enough that even 16 concurrent children (a busy fleet node) can't matter:
// 16 * 256 KiB = 4 MiB worst case, versus the previous unbounded growth over
// a 40-minute timeout_sec window from a runaway CLI.
const tailWriterCap = 256 * 1024

// tailWriter is a fixed-capacity io.Writer that keeps only the LAST cap bytes
// ever written (tail semantics — the error extraction downstream always
// wants the END of the output, never the beginning), while still tracking
// the TOTAL bytes written so callers can tell a caller "this was truncated"
// even though the retained window is bounded. It replaces the two unbounded
// capture sites in Generate (cmd.CombinedOutput()'s internal buffer and
// runSampled's bytes.Buffer): those accumulated the child's entire
// stdout+stderr in RAM even though tail(o, 400) only ever formats the last
// 400 bytes at error time — a runaway child spewing gigabytes over a long
// timeout would balloon the node's memory before that truncation ever ran.
//
// Safe for concurrent use (the sampled path's ticker goroutine and cmd's own
// writes are not synchronized with each other by exec.Cmd itself when
// Stdout/Stderr point at the same io.Writer, so the writer must serialize
// itself).
type tailWriter struct {
	mu    sync.Mutex
	buf   []byte // len grows up to cap; backing array is pre-sized to cap and never reallocated larger
	cap   int
	total int64
}

// newTailWriter returns a tailWriter that retains at most capacity bytes.
func newTailWriter(capacity int) *tailWriter {
	return &tailWriter{cap: capacity, buf: make([]byte, 0, capacity)}
}

// Write implements io.Writer. It never grows buf's backing array past cap:
// on overflow it shifts the retained old bytes down (an in-place, safe-for-
// overlap copy) to make room for the new tail, so len(buf) — and cap(buf) —
// never exceed the configured capacity, however much is written overall.
func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	w.total += int64(n)

	if w.cap == 0 {
		return n, nil
	}

	if n >= w.cap {
		// p alone overflows the whole cap: only ITS OWN tail can survive.
		w.buf = w.buf[:w.cap]
		copy(w.buf, p[n-w.cap:])
		return n, nil
	}

	have := len(w.buf)
	total := have + n
	if total <= w.cap {
		// Room to just append.
		w.buf = w.buf[:total]
		copy(w.buf[have:], p)
		return n, nil
	}

	// Overflow: drop the oldest `drop` bytes so retained-old + new == cap.
	drop := total - w.cap
	keep := have - drop // bytes of old content retained
	w.buf = w.buf[:w.cap]
	copy(w.buf, w.buf[drop:have]) // shift retained old bytes to the front; copy() is overlap-safe (memmove semantics)
	copy(w.buf[keep:], p)
	return n, nil
}

// Contents returns a copy of the currently retained tail bytes: byte-
// identical to everything written when Total() <= cap, else the last cap
// bytes written.
func (w *tailWriter) Contents() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]byte, len(w.buf))
	copy(out, w.buf)
	return out
}

// Total returns the total bytes ever written, uncapped (used to detect and
// report truncation — the retained window itself never reflects this).
func (w *tailWriter) Total() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total
}

// Truncated reports whether more was written than the writer could retain.
func (w *tailWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total > int64(w.cap)
}

// String renders the retained tail, prefixed with a "truncated: total X
// bytes" marker when Truncated() is true — the form callers embed in a
// gpugen failure detail so a bounded capture never silently hides how much
// was actually dropped.
func (w *tailWriter) String() string {
	w.mu.Lock()
	total, capacity, buf := w.total, w.cap, string(w.buf)
	w.mu.Unlock()
	if total > int64(capacity) {
		return fmt.Sprintf("truncated: total %d bytes, showing last %d: %s", total, capacity, buf)
	}
	return buf
}
