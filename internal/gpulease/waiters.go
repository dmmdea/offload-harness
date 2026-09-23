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
// the live ones (dead pids are pruned on read). The O_EXCL meta.json stays the
// sole arbiter of who HOLDS the card — a stale or missing waiter file can never
// grant possession, only affect who is ALLOWED TO TRY next (see FIFO below) or
// defer one warm to the next holder.
//
// FIFO (register D-13x, 2026-09-22). THE INCIDENT: `gpu status` showed a text
// waiter queued 1h43m while two media reservations that queued LATER each took
// the card ahead of it. The cause: every waiting process polled TryAcquire once
// a second with NO ordering between them — the waiter record was written and
// read, but nothing in the acquire path ever consulted it before racing for the
// O_EXCL claim, so whichever process's poll tick landed first after a release
// won. Arrival order was pure scheduling luck, and a waiter could in principle
// lose that race forever.
//
// The fix: on each poll, a waiter first checks isFrontOfQueue — is it the
// OLDEST live waiter recorded right now? Only the front of the line attempts
// the claim; everyone else skips the attempt and waits for its own next tick.
// The instant the holder releases, the front waiter's next poll (at most one
// poll interval later) claims it uncontested by every OTHER waiter, because
// they are deliberately not racing. Ties (identical SinceMs) break on the
// waiter file's name, which registerWaiter makes unique with a random token —
// every reader computes the same order from the same directory listing, so the
// tie-break needs no coordination beyond the filesystem both sides already
// share. This governs ordering among REGISTERED waiters only: a brand-new
// Acquire's very first, pre-registration TryAcquire (and any bare TryAcquire
// call that never sets Wait) can still land in the narrow window between a
// release and the front waiter's next tick — the same residual race every
// poll-based queue has, bounded by one poll interval, and unrelated to the
// hours-long starvation this fixes.
//
// Class carries no priority here: no ADR documents a queue-level class
// priority (0026 gates text LOADS behind a media lease; 0041 sizes the drain
// budget; neither says anything about acquisition order), so text and media
// waiters interleave in pure arrival order.
//
// Seat-warm-owed: a holder that unloaded the seat stamps <gpu>/seat-warm-owed
// with the seat's name. The LAST releasing holder — the one with no waiter
// behind it — clears it by warming; every other releaser leaves it in place.
// A `gpu release --warm-seat` reads the same marker.

import (
	"crypto/rand"
	"encoding/hex"
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

// randomToken is a short, filesystem-safe token with no ordering meaning of its own.
// It exists to (1) keep two registrations from the SAME pid in the SAME millisecond —
// two goroutines in one process, or two CLI invocations that land on the same clock
// tick — from colliding on one waiter file (a collision would silently drop one of the
// two: the second WriteFile stomps the first's record, and whichever one unregisters
// first then deletes the file the SURVIVOR was relying on, making it vanish from
// Waiters() as if it had never queued), and (2) give isFrontOfQueue a tie-break that
// every reader computes identically from the shared directory listing alone, with no
// extra coordination.
//
// Never security-sensitive — a predictable fallback is fine — so registration must
// never BLOCK or FAIL because entropy is briefly unavailable.
func randomToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// registerWaiter records this process as queued and returns the record it wrote
// (with path set, so the caller can find itself again in a later Waiters() read) and a
// func that removes it; the func is safe to call more than once. A failure to write
// the record (unwritable waiters dir) returns a zero Waiter — see isFrontOfQueue for
// what that degrades to.
func (m *Manager) registerWaiter(class Class, opts Options) (Waiter, func()) {
	if err := os.MkdirAll(m.waitersDir(), 0o777); err != nil {
		return Waiter{}, func() {}
	}
	pid := os.Getpid()
	w := Waiter{PID: pid, Class: class, Reason: clipCommand(opts.Reason), SinceMs: m.now().UnixMilli()}
	if st, ok := m.procStart(pid); ok {
		w.StartTimeMs = st
	}
	b, err := json.Marshal(w)
	if err != nil {
		return Waiter{}, func() {}
	}
	path := filepath.Join(m.waitersDir(), strconv.Itoa(pid)+"."+strconv.FormatInt(w.SinceMs, 10)+"."+randomToken()+".json")
	if err := os.WriteFile(path, b, 0o666); err != nil {
		return Waiter{}, func() {}
	}
	w.path = path
	// removeClaim, not a bare os.Remove: since isFrontOfQueue makes this directory
	// a HOT path — every queued waiter reads it on every poll tick — a plain
	// os.Remove is no longer safe. On Windows a reader blocks a delete (the same
	// sharing-violation window documented for meta.json and the epoch lock), and
	// with several waiters polling every couple of milliseconds that window is
	// essentially always open. A bare os.Remove failing there does not just leak
	// a file: the un-removed record still parses as a LIVE waiter (same pid,
	// same process, forever "alive"), so it becomes an immovable head of the
	// queue nobody can ever get past — the exact starvation this file exists to
	// end. Measured: without the retry, a 6-waiter mixed-class chain stalled
	// after the 2nd handoff and never recovered inside a 45 s budget.
	return w, func() { _ = removeClaim(path) }
}

// waiterBefore reports whether a is strictly ahead of b in FIFO order: earlier
// SinceMs, or — on an exact tie — the lexicographically earlier waiter file name.
// registerWaiter's random token makes two DIFFERENT waiters' file names differ with
// overwhelming probability, so the tie-break is total in practice; every reader
// derives it from the same directory listing, so no two processes can disagree about
// which of two tied waiters goes first.
func waiterBefore(a, b Waiter) bool {
	if a.SinceMs != b.SinceMs {
		return a.SinceMs < b.SinceMs
	}
	return filepath.Base(a.path) < filepath.Base(b.path)
}

// isFrontOfQueue reports whether self is the OLDEST live waiter queued for the card
// right now. Waiters() prunes dead and stale records as it reads, so a waiter that
// died mid-queue drops out of every OTHER waiter's view on its very next poll — it
// never blocks the line behind it.
//
// A self with an empty path (registerWaiter could not write its record) always
// reports front-of-queue: this is the fail-soft path for an unwritable waiters
// directory, which the rest of the package already treats as advisory-only rather
// than refusing GPU work over a bookkeeping failure. It costs that one caller its
// place in the (unrecorded) line, not the reverse — it never makes an OTHER, properly
// registered waiter lose its place.
func (m *Manager) isFrontOfQueue(self Waiter) bool {
	if self.path == "" {
		return true
	}
	for _, w := range m.Waiters() {
		if w.path == self.path {
			continue
		}
		if waiterBefore(w, self) {
			return false
		}
	}
	return true
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
