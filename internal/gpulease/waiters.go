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
// ALIVE BUT NOT POLLING (2026-09-22, lead review of D-13x before ship). Pid
// liveness alone cannot tell "queued and actively polling" from "queued,
// still alive, and never going to poll again" — a process suspended by the
// OS, wedged in another goroutine, paused in a debugger, or an OLDER harness
// binary whose Acquire loop exited without unregistering (a bug, a panic that
// skipped the defer, a crash the OS hasn't reaped yet). Under a naive "oldest
// LIVE waiter wins" rule that waiter is the permanent, unbreakable head of the
// line: every other process defers to it forever, because its pid genuinely
// never dies. So being alive is necessary but not sufficient — a live waiter
// must also keep PROVING it is still actually queued. Every poll tick,
// win-or-lose, a waiter re-stamps its own record's mtime (refreshWaiter, via
// the same beside-then-rename-over pattern as Renew/Restamp — this file is
// read by every OTHER waiter on every one of their ticks, so the Windows
// rename-vs-concurrent-reader retry is not optional here either). A reader
// that finds a record older than waiterStaleWindow (10x the poll interval,
// floor 15s — generous: normal jitter under load is one or two missed ticks,
// not ten) treats it exactly like a dead pid: skipped for ordering, pruned
// best-effort so nobody re-judges it every read. This is also what keeps a
// MIXED-VERSION rollout safe: an older binary's waiter file is the same JSON
// shape (no schema change here) so a newer reader parses it fine, but the
// older code never calls refreshWaiter — it only ever raced TryAcquire, never
// honoured order — so its record goes stale on the same clock and stops
// affecting anyone's ordering decision. It cannot wedge the line; it just
// keeps racing as it always did, which is the documented, accepted limit for
// an old process, not a new one.
//
// Pid recycling is covered the same way it is everywhere else in this
// package: StartTimeMs, stamped from procStart at registration, is compared
// against the CURRENT process behind that pid on every read (Waiters(),
// below) — the identical check Reclaimable uses for the lease holder itself.
// A record whose pid was handed to an unrelated process reads as dead, not
// alive, so a recycled pid can neither hold the lease nor jump the waiter
// queue.
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

	// waiterHeartbeatMultiple x the poll interval, floored at
	// waiterHeartbeatFloor, is the default heartbeat staleness window (see
	// waiterStaleWindow). Ten missed ticks is generous slack for ordinary
	// scheduling jitter under load while still catching a genuinely wedged
	// waiter — and an old-version waiter, which never refreshes at all —
	// within a bounded time instead of forever.
	waiterHeartbeatMultiple = 10
	waiterHeartbeatFloor    = 15 * time.Second
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

// waiterStaleWindow is how long a waiter's record may go unrefreshed before a
// reader stops trusting it as "still actually queued" (see the ALIVE BUT NOT
// POLLING section atop this file). waiterHeartbeatTTL, when set, overrides the
// computed default for tests; production always uses the formula.
func (m *Manager) waiterStaleWindow() time.Duration {
	if m.waiterHeartbeatTTL > 0 {
		return m.waiterHeartbeatTTL
	}
	w := waiterHeartbeatMultiple * m.pollInterval()
	if w < waiterHeartbeatFloor {
		w = waiterHeartbeatFloor
	}
	return w
}

// refreshWaiter re-stamps self's own record so it reads as freshly polled.
// Called every poll tick regardless of front-of-queue status: a live process
// proves it is still actually queued by continuing to do this, not merely by
// having a pid that has not exited. The content never changes (same pid,
// class, reason, since) — only the file's mtime, which IS the heartbeat, so
// this is a plain re-write rather than a schema change; an older binary's
// record (which is never refreshed) and a newer one are byte-identical in
// shape.
//
// Beside-then-rename-over, exactly like Renew/Restamp: a bare in-place
// rewrite is not needed for atomicity here (the content is unchanged), but
// the RENAME still has to survive a concurrent reader. This file is read by
// every OTHER waiter on every one of ITS ticks — Waiters() opens it with
// os.ReadFile, which on Windows blocks a delete/replace exactly like it does
// for meta.json — so renameReplacing's retry is load-bearing, not decoration.
// self.path=="" (registration failed) is a silent no-op, matching
// isFrontOfQueue's fail-soft treatment of the same case.
func (m *Manager) refreshWaiter(self Waiter) {
	if self.path == "" {
		return
	}
	b, err := json.Marshal(self)
	if err != nil {
		return
	}
	tmp := self.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o666); err != nil {
		return
	}
	if err := renameReplacing(tmp, self.path); err != nil {
		_ = os.Remove(tmp)
	}
}

// waiterIOAttempts x waiterIOPause bound a retry on a TRANSIENT read/stat
// failure against a waiter file. refreshWaiter now rewrites a waiter's own
// record roughly once per poll interval, and every OTHER waiter reads that
// same file at the same cadence — on Windows, a plain read can transiently
// fail while a concurrent rename is in flight over the same path, the same
// class of ephemeral error renameReplacing/removeClaim already retry
// elsewhere. Sized identically to removeAttempts/removePause: a handful of
// 5ms retries is orders of magnitude more than the rename itself takes.
const (
	waiterIOAttempts = 20
	waiterIOPause    = 5 * time.Millisecond
)

// readWaiterFile retries a transient read failure. os.IsNotExist is returned
// immediately (unretried) — the file is genuinely gone, not racing a rename,
// and the caller must not treat retry-exhaustion the same as confirmed
// absence: one is "try again next read", the other is "safe to prune".
func readWaiterFile(path string) ([]byte, error) {
	var b []byte
	var err error
	for attempt := 0; attempt < waiterIOAttempts; attempt++ {
		b, err = os.ReadFile(path)
		if err == nil || os.IsNotExist(err) {
			return b, err
		}
		time.Sleep(waiterIOPause)
	}
	return b, err
}

// statWaiterFile is readWaiterFile's twin for the heartbeat mtime check.
func statWaiterFile(path string) (os.FileInfo, error) {
	var fi os.FileInfo
	var err error
	for attempt := 0; attempt < waiterIOAttempts; attempt++ {
		fi, err = os.Stat(path)
		if err == nil || os.IsNotExist(err) {
			return fi, err
		}
		time.Sleep(waiterIOPause)
	}
	return fi, err
}

// Waiters lists the processes queued for the card, oldest first. Records whose
// process is gone, whose pid was recycled, or which have not been refreshed
// within waiterStaleWindow (an alive process that has stopped actually
// polling — suspended, wedged, an old binary that never refreshes at all) are
// pruned as they are read, so neither a dead waiter nor a merely stalled one
// ever blocks the line or defers a warm forever.
//
// TRANSIENT read/stat failures NEVER prune. Confirmed-gone (os.IsNotExist)
// does; a retry-exhausted transient error is treated as "skip this entry for
// THIS read only, try again next time" — reproduced directly (2026-09-22):
// treating any os.Stat error as "prune it" made a live, correctly-refreshing
// waiter's record vanish PERMANENTLY the one time its owner's rename and
// another goroutine's Stat overlapped, which is ordinary traffic once every
// waiter is reading every other waiter's file every poll tick.
func (m *Manager) Waiters() []Waiter {
	entries, err := os.ReadDir(m.waitersDir())
	if err != nil {
		return nil
	}
	now := m.now()
	nowMs := now.UnixMilli()
	staleWindow := m.waiterStaleWindow()
	var out []Waiter
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(m.waitersDir(), e.Name())
		b, rerr := readWaiterFile(p)
		if rerr != nil {
			if os.IsNotExist(rerr) {
				continue // confirmed gone: nothing to prune, nothing to list
			}
			continue // retries exhausted on a transient error: try again next read
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
		// HEARTBEAT STALENESS. A live, non-recycled pid is not enough: the
		// process behind it may have stopped polling altogether (suspended,
		// wedged in another goroutine, an OLD binary that registered once and
		// never calls refreshWaiter at all). Every legitimate waiter re-stamps
		// its own record's mtime on every poll tick, so a record older than
		// staleWindow has stopped proving it is still actually in line —
		// treated the same as a dead waiter: skipped for ordering, pruned
		// best-effort.
		fi, serr := statWaiterFile(p)
		if serr != nil {
			if os.IsNotExist(serr) {
				continue // confirmed gone
			}
			continue // transient: do not prune a record we could not actually check
		}
		if now.Sub(fi.ModTime()) > staleWindow {
			_ = os.Remove(p)
			continue
		}
		w.path = p
		out = append(out, w)
	}
	// The same order isFrontOfQueue serves (waiterBefore), so `gpu status` lists the line
	// exactly as it will be granted, exact-millisecond ties included.
	sort.Slice(out, func(i, j int) bool { return waiterBefore(out[i], out[j]) })
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
