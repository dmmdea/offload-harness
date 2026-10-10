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
// share. This governs ordering among REGISTERED waiters, and since register
// D-1xx-3 (2026-10-09) every production claim is one: Acquire registers BEFORE
// its first attempt, with or without a Wait (a zero Wait is one gated attempt),
// so a fresh claim can no longer land in the window between a release and the
// front waiter's next tick. The one door that still skips the line is a bare
// TryAcquire, which setup and tests use and no production path calls.
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
	"sync/atomic"
	"time"
)

const (
	waitersDirName   = "waiters"
	seatWarmOwedName = "seat-warm-owed"

	// waiterStaleAfterMs is the debris cap for a live, heartbeating waiter that declared no wait
	// (a seat admission, an older binary's record, a one-attempt claim): older than the default
	// --wait's reach, it is not queued, it is stuck. A waiter that DECLARED a longer wait is kept
	// for that wait instead (waiterOutlived), because the cap is only ever a guess at the wait.
	waiterStaleAfterMs = 12 * 60 * 60 * 1000
	// waiterDeadlineSlackMs is how long past its own declared deadline a waiter that is still
	// alive and heartbeating is kept. Acquire leaves at its deadline, so a record that outlives it
	// by this much belongs to a process that stopped acting on its own wait; the hour is slack for
	// a clock step, not a grace anyone relies on.
	waiterDeadlineSlackMs = 60 * 60 * 1000

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
	// Devices are the cards the waiter wants (GPU UUIDs); empty = the whole node. A
	// pre-v2 reader ignores the field, which reads the waiter as whole-node: the
	// conservative direction.
	Devices []string `json:"devices,omitempty"`
	// Token names the place-keeping token this waiter resumed (tokens.go), so it never queues
	// behind its own. An older reader ignores the field.
	Token string `json:"token,omitempty"`
	// DeadlineMs is when the waiter's own wait ends (registration time plus the wait it declared),
	// 0 when it declared none. The debris cap reads it (waiterOutlived): a waiter that asked for
	// 20 h must not be reaped as debris at 12 h while it is alive and heartbeating. An older reader
	// ignores the field and keeps its flat cap, the conservative direction for a record it cannot
	// read the wait of.
	DeadlineMs int64 `json:"deadline_ms,omitempty"`
	path       string
}

// waiterOutlived reports whether a live, heartbeating record has outlived every wait it could
// still be honouring, so it is debris and not a place in line. The floor is waiterStaleAfterMs
// from the arrival; a waiter that declared a wait is kept until that deadline plus the slack,
// whichever is later. It never SHORTENS the floor: a record with no declared wait is judged
// exactly as before.
func waiterOutlived(w Waiter, nowMs int64) bool {
	limit := w.SinceMs + waiterStaleAfterMs
	if w.DeadlineMs > 0 {
		if d := w.DeadlineMs + waiterDeadlineSlackMs; d > limit {
			limit = d
		}
	}
	return nowMs > limit
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

// unregisteredWarned makes the warning below print once per process: a long-lived server whose
// waiters directory is unwritable would otherwise repeat it on every media call, and a line per
// call is a notification per call in whatever session wraps it.
var unregisteredWarned atomic.Bool

// warnUnregistered says, once, that this request could not take a place in line. The gate it
// stands behind is fail-soft by design (isFrontOfQueue answers true for a record that was never
// written, so a bookkeeping fault never refuses GPU work), which makes the failure silent: the
// request would claim the card like a bare claim, ahead of waiters it should queue behind, and
// nothing would show it. The claim itself is untouched; this only makes the loss of the queue
// visible.
func warnUnregistered(err error) {
	if unregisteredWarned.Swap(true) {
		return
	}
	fmt.Fprintf(os.Stderr, "gpulease: warning: could not record this request's place in line (%v); it will claim the card without queueing behind earlier waiters, and later requests will not see it in line. Check that the waiters directory under the lease state root is writable\n", err)
}

// registerWaiter records this process as queued and returns the record it wrote
// (with path set, so the caller can find itself again in a later Waiters() read) and a
// func that removes it; the func is safe to call more than once. A failure to write
// the record (unwritable waiters dir) returns a zero Waiter — see isFrontOfQueue for
// what that degrades to — and says so once on stderr (warnUnregistered), because the
// degradation is otherwise invisible: the request claims the card as a bare claim would.
func (m *Manager) registerWaiter(class Class, opts Options) (Waiter, func()) {
	if err := os.MkdirAll(m.waitersDir(), 0o777); err != nil {
		warnUnregistered(err)
		return Waiter{}, func() {}
	}
	pid := os.Getpid()
	// A call that resumes a place in line keeps the arrival time it left with (tokens.go); one
	// that was handed an arrival time (QueuedSince) carries that; otherwise it arrives now.
	since := m.now()
	if !opts.QueuedSince.IsZero() {
		since = opts.QueuedSince
	}
	resumed := ""
	if tok, ok := m.ResumeToken(opts.ResumeToken); ok {
		since, resumed = tok.Since(), tok.ID
	}
	w := Waiter{PID: pid, Class: class, Reason: clipCommand(opts.Reason), SinceMs: since.UnixMilli(), Devices: opts.Devices, Token: resumed}
	if opts.Wait > 0 {
		// The wait is the record's own: the debris cap is derived from it, not from the longest
		// wait anyone is expected to pass (waiterOutlived).
		w.DeadlineMs = m.now().Add(opts.Wait).UnixMilli()
	}
	if st, ok := m.procStart(pid); ok {
		w.StartTimeMs = st
	}
	b, err := json.Marshal(w)
	if err != nil {
		warnUnregistered(err)
		return Waiter{}, func() {}
	}
	path := filepath.Join(m.waitersDir(), strconv.Itoa(pid)+"."+strconv.FormatInt(w.SinceMs, 10)+"."+randomToken()+".json")
	if err := os.WriteFile(path, b, 0o666); err != nil {
		warnUnregistered(err)
		return Waiter{}, func() {}
	}
	w.path = path
	// The live waiter stands for the token from here: consume it, so the line counts one place.
	if resumed != "" {
		m.DropToken(resumed)
	}
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
// registered waiter lose its place. It is not silent: registerWaiter has said so once on
// stderr (warnUnregistered), because a gate that quietly becomes a bare claim is the
// defect this queue exists to end.
func (m *Manager) isFrontOfQueue(self Waiter) bool {
	if self.path == "" {
		return true
	}
	for _, w := range m.Waiters() {
		if w.path == self.path {
			continue
		}
		// FIFO among waiters that CONFLICT. A waiter ahead of us that wants other
		// cards is no reason to wait (disjoint backfill); a whole-node waiter wants
		// everything, so it is a barrier that every later waiter queues behind.
		if waiterBefore(w, self) && devicesConflict(w.Devices, self.Devices) {
			return false
		}
	}
	// A token is a waiter that is not there: it holds its place for the grace and is ignored after
	// it (tokens.go), by every waiter, a whole-node barrier included.
	return !m.tokenBlocks(self)
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
		if !alive || waiterOutlived(w, nowMs) {
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

// managerAt builds a throwaway Manager bound to an explicit lease directory,
// for a package-level helper that operates on a directory string rather than
// an owned Manager — InspectDir's own pattern (gpulease.go), extended here to
// the waiters queue so a caller with only a resolved `dir` (modelaffinity,
// which arms its gate from config.Load and never opens a full Manager) can
// still register a waiter. now/procStart use the real clock/OS exactly like
// OpenAt's Manager; only tests need the injectable seams, and they build
// their Manager directly.
func managerAt(leaseDir string) *Manager {
	return &Manager{
		leaseOverride: leaseDir,
		root:          filepath.Dir(filepath.Dir(leaseDir)),
		heartbeatTTL:  DefaultHeartbeatTTL,
		now:           time.Now,
		procStart:     processStart,
		pid:           os.Getpid(),
	}
}

// RegisterSeatWaiter marks a blocked seat/text-load admission as queued for
// the lease at leaseDir (register D-1xx-2, 2026-09-23; R2 2026-09-23: "seat
// starvation behind chained media leases").
//
// THE INCIDENT. A `transcribe` call's admission (modelaffinity.awaitLease)
// waited 15+ minutes behind another client's back-to-back media leases and
// was never admitted between them: epoch 111 (music) released and epoch 112
// (music) was re-acquired by the SAME client with no gap the admission's own
// 1 s poll ever caught free — seat/text-load admission had no representation
// in the lease queue at all, so a fresh media Acquire (once D-1xx's FIFO fix
// makes it queue behind registered waiters) still had nothing to queue
// BEHIND in this case, because the admission was not one.
//
// THE FIX reuses the SAME waiters/ directory and Waiter record a lease
// Acquire itself registers (waiters.go's FIFO doctrine), tagged ClassSeat so
// `gpu status` and the queue log can tell it apart. Because isFrontOfQueue /
// Waiters() are class-agnostic (no ADR gives the queue a class priority —
// see waiters.go), a media/text Acquire that reaches its own front-of-queue
// check automatically yields to a live ClassSeat entry exactly as it yields
// to a real lease waiter, with NO changes needed to the ordering algorithm
// itself.
//
// WHY THIS CANNOT DEADLOCK OR STARVE THE LEASE. A ClassSeat waiter NEVER
// itself calls TryAcquire — it is not competing FOR the lease, only for a
// fair turn once the card frees — so it can only ever DELAY another
// acquirer's first successful claim, never block it forever: once the
// admission notices the lease free (its own leasePollInterval-cadenced
// InspectDir, independent of this registration) it proceeds and the CALLER
// unregisters via the returned func, freeing the front of the queue for the
// next real Acquire loop within at most one of the admission's own poll
// ticks. While the lease is genuinely held by someone else, a ClassSeat
// entry changes nothing: TryAcquire already refuses on its own, registered
// waiter or not.
//
// WHICH CARDS. devices are the cards the blocked admission is waiting to load onto — its
// seat's cards (lease ids), the same set awaitLease blocks on (ScopeToModel). Every newcomer
// is gated by a registered waiter that CONFLICTS with it, so the set is what keeps this entry
// from holding back work it has nothing to do with: registered as the whole node, a text-load
// admission blocked on its seat's card stopped a fresh `gpu reserve --devices <other card>`
// on a free, unrelated card (the 0.178.0 review). nil is the whole node, the right answer
// only when the seat's cards cannot be named (an undeclared model, a pin the card table
// cannot place, a card table that cannot be read): unknown is every card, the gate's rule.
//
// refresh must be called on every one of the caller's own poll ticks — a
// long wait's record otherwise goes heartbeat-stale (waiterStaleWindow, see
// the ALIVE BUT NOT POLLING section atop this file) and stops protecting the
// admission's place in line, exactly like an Acquire loop's own
// refreshWaiter call. unregister is safe to call more than once and must run
// via defer so a cancelled or timed-out admission never leaks a waiter that
// would otherwise sit at the front of the queue until it goes stale.
func RegisterSeatWaiter(leaseDir, reason string, devices []string) (refresh func(), unregister func()) {
	m := managerAt(leaseDir)
	// A set that does not normalize (a blank or non-token id) is not a set of cards: it falls
	// back to the whole node rather than to a record that names nothing the claims use.
	devs, err := NormalizeDevices(devices)
	if err != nil {
		devs = nil
	}
	self, unreg := m.registerWaiter(ClassSeat, Options{Reason: reason, Devices: devs})
	return func() { m.refreshWaiter(self) }, unreg
}
