package gpulease

// Waiters and the seat-warm-owed marker (register D-124, 2026-09-18).
//
// THE INCIDENT. The queue hands the card to the next waiter the moment the
// holder's claim disappears. The wrapper form (`gpu reserve --unload-seat --
// <cmd>`) warms the agent seat back BEFORE it releases, so the order held —
// until the night the wrapped command was cut and the holder's lease had
// already moved on: the warm ran unordered against the next lease, which had
// drained an "idle" seat, unloaded it and launched its first row; systemd then
// relaunched the seat the unload had killed, and three rows measured the
// previous holder's seat instead of their own fit. Two things were missing:
//
//   - the releasing holder could not SEE that someone was queued behind it, so
//     it warmed a seat the next holder would unload again seconds later — a
//     3-minute load bought nothing and, once unordered, poisoned the card;
//   - nothing recorded that a warm was OWED, so "the warm belongs to the LAST
//     holder" had no state to rest on.
//
// Waiters: Acquire registers a small record under <gpu>/waiters/ for as long as
// it polls, and removes it on return. A holder deciding whether to warm reads
// the live ones (dead pids are pruned on read). This is INFORMATION for the
// release path, never a claim: the O_EXCL meta.json stays the sole arbiter of
// who holds the card, and a stale waiter file can at most defer one warm to
// the next holder.
//
// Seat-warm-owed: a holder that unloaded the seat stamps <gpu>/seat-warm-owed
// with the seat's name. The LAST releasing holder — the one with no waiter
// behind it — clears it by warming; every other releaser leaves it in place.
// A `gpu release --warm-seat` reads the same marker.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	waitersDirName     = "waiters"
	seatWarmOwedName   = "seat-warm-owed"
	waiterStaleAfterMs = 12 * 60 * 60 * 1000 // a waiter older than the longest --wait is debris
)

// Waiter is one process queued for the card.
type Waiter struct {
	PID         int    `json:"pid"`
	StartTimeMs int64  `json:"start_time_ms,omitempty"`
	Class       Class  `json:"class"`
	Reason      string `json:"reason,omitempty"`
	SinceMs     int64  `json:"since_ms"`
	path        string
}

// Since is when the waiter started queueing.
func (w Waiter) Since() time.Time { return time.UnixMilli(w.SinceMs) }

func (m *Manager) waitersDir() string { return filepath.Join(m.gpuDir(), waitersDirName) }

// registerWaiter records this process as queued. The returned func removes the
// record; it is safe to call more than once.
func (m *Manager) registerWaiter(class Class, opts Options) func() {
	if err := os.MkdirAll(m.waitersDir(), 0o777); err != nil {
		return func() {}
	}
	pid := os.Getpid()
	w := Waiter{PID: pid, Class: class, Reason: clipCommand(opts.Reason), SinceMs: m.now().UnixMilli()}
	if st, ok := m.procStart(pid); ok {
		w.StartTimeMs = st
	}
	b, err := json.Marshal(w)
	if err != nil {
		return func() {}
	}
	path := filepath.Join(m.waitersDir(), strconv.Itoa(pid)+"."+strconv.FormatInt(w.SinceMs, 10)+".json")
	if err := os.WriteFile(path, b, 0o666); err != nil {
		return func() {}
	}
	return func() { _ = os.Remove(path) }
}

// Waiters lists the processes queued for the card, oldest first. Records whose
// process is gone (or whose pid was recycled) are pruned as they are read, so a
// waiter killed mid-queue never defers a warm forever.
func (m *Manager) Waiters() []Waiter {
	entries, err := os.ReadDir(m.waitersDir())
	if err != nil {
		return nil
	}
	nowMs := m.now().UnixMilli()
	var out []Waiter
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(m.waitersDir(), e.Name())
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			continue
		}
		var w Waiter
		if json.Unmarshal(b, &w) != nil || w.PID <= 0 {
			_ = os.Remove(p)
			continue
		}
		alive := pidAlive(w.PID)
		if alive && w.StartTimeMs != 0 {
			if st, ok := m.procStart(w.PID); ok && st != w.StartTimeMs {
				alive = false // recycled pid
			}
		}
		if !alive || nowMs-w.SinceMs > waiterStaleAfterMs {
			_ = os.Remove(p)
			continue
		}
		w.path = p
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SinceMs < out[j].SinceMs })
	return out
}

func (m *Manager) seatWarmOwedPath() string { return filepath.Join(m.gpuDir(), seatWarmOwedName) }

// MarkSeatWarmOwed records that seat was unloaded for a lease and is owed a
// warm by the last holder to release.
func (m *Manager) MarkSeatWarmOwed(seat string) error {
	if strings.TrimSpace(seat) == "" {
		return errors.New("gpulease: seat-warm-owed needs a seat name")
	}
	if err := os.MkdirAll(m.gpuDir(), 0o777); err != nil {
		return err
	}
	rec := fmt.Sprintf("%s %s\n", seat, m.now().UTC().Format(time.RFC3339))
	return os.WriteFile(m.seatWarmOwedPath(), []byte(rec), 0o666)
}

// SeatWarmOwed reports the seat a warm is owed to, or "" when none is.
func (m *Manager) SeatWarmOwed() string {
	b, err := os.ReadFile(m.seatWarmOwedPath())
	if err != nil {
		return ""
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// ClearSeatWarmOwed removes the marker once the seat has been warmed back.
func (m *Manager) ClearSeatWarmOwed() {
	_ = os.Remove(m.seatWarmOwedPath())
}
