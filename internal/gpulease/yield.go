package gpulease

// yield.go — an overdue lease's HOLDER is asked to yield; it is never taken from (plan P11, ADR 0085).
//
// WHAT IS BUILT. When a lease outlives its declared window while somebody waits for its cards, the
// holder's own tick writes a small sidecar, yield.<epoch>, and runs the --on-yield command the job
// recorded at the start, once. That is the whole request. The card frees when the holder ends or
// releases in the ordinary way (the wrapper form when its command ends, a detached job when its driver
// releases), never because of the request: nothing here frees a claim, stops a command or signals a
// process, and the source of this file and of the root runner is pinned to say so
// (TestNoAutomaticYieldModeExists). A lease whose holder ignores the request stays held-overdue, its
// heartbeat and fence intact, and its waiters keep their place in line.
//
// WHO WRITES. The holder's tick, not a blocked waiter: the text and seat admission waiter never goes
// through Acquire, the refusal text and every status surface are read-only by contract (explain.go,
// owner.go), and only a live ticking holder can act on a request anyway. Waiter presence is the demand
// signal: the asker reads the live waiters and asks when one of them wants a card the lease holds.
//
// WHEN. Every precondition is read from the record and the line, cheapest first: the policy is
// "request" (config gpu_on_overdue; unset behaves as before the key existed and asks nobody), the
// lease's record is expired or past its declared end (decided from the record alone, so a lease whose
// term the same tick renewed is not asked), the holder was not asked yet, its record says it runs the
// request (Meta.Yields; an older build, or an in-process holder, never ticks this), that the lease has read
// overdue for the holder's settle interval (the holders pass the interval at which their term check looks again
// at a label, so a label is asked about only after it survived a look), and only then the waiters are read,
// because reading them prunes the dead ones. The same waiter (pid and start identity) must be seen on two
// consecutive ticks: a short `gpu reserve --wait` probe, or a seat admission that leaves after seconds,
// would otherwise stop a long job through its on_yield and be gone, and the ask cannot be taken back.
//
// THE SIDECAR is written under the epoch lock, after a probe that the lease directory is writable and
// a check that the lease is still live, with tmp and rename, exactly as the orphan marker is. The
// second write (what the holder did about it) takes the same lock and the same checks, so a late update
// that races the release cannot bring the file back for a lease that is gone. Both sweeps that end a
// lease remove it.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// YieldFileEnv names the variable the wrapper form hands its command: the path of the lease's
// yield sidecar, which exists only once the holder has been asked.
const YieldFileEnv = "GPU_LEASE_YIELD_FILE"

// yieldPrefix names the sidecar yield.<epoch>, a sidecar like orphan.<epoch>, removed with the lease.
const yieldPrefix = "yield."

// The forms of holder that run the request, as Meta.Yields records them.
const (
	YieldsWrapper  = "wrapper"
	YieldsDetached = "detached"
)

// yieldUnansweredDisplay is how long after the ask a holder that declared no --yield-grace is still
// described as having time. It is a chosen, unmeasured figure that only words a sentence: nothing keys
// off it, and nothing is stopped or taken when it passes.
const yieldUnansweredDisplay = 10 * time.Minute

// yieldWaiterSummaries bounds how many waiters the sidecar names.
const yieldWaiterSummaries = 3

// OnOverdue is the policy for a lease that outlived its window while someone waits for its cards
// (config gpu_on_overdue).
type OnOverdue string

const (
	// OnOverdueUnset: the key is not set. The harness behaves as it did before the request existed:
	// nobody is asked.
	OnOverdueUnset OnOverdue = ""
	// OnOverdueHold: the holder is never asked; the lease is held until its holder ends it.
	OnOverdueHold OnOverdue = "hold"
	// OnOverdueRequest: the holder is asked to yield and runs the job's own recorded on_yield.
	OnOverdueRequest OnOverdue = "request"
)

// ParseOnOverdue reads a configured policy. Matching trims and lower-cases, as the other policy
// validators do. A value that is neither request nor hold is an error naming both, and comes back as
// hold: the permissive direction is never the fallback.
func ParseOnOverdue(raw string) (OnOverdue, error) {
	switch v := OnOverdue(strings.ToLower(strings.TrimSpace(raw))); v {
	case OnOverdueUnset, OnOverdueHold, OnOverdueRequest:
		return v, nil
	}
	return OnOverdueHold, fmt.Errorf("gpu_on_overdue: unknown policy %q (valid: \"request\" (ask the lease's holder to yield) or \"hold\" (never ask); unset asks nobody, as before the key existed)", raw)
}

type onOverdueState struct {
	mode OnOverdue
	note string
}

var installedOnOverdue atomic.Pointer[onOverdueState]

// SetDefaultOnOverdue installs the configured policy for every holder tick and every sentence of this
// process, the way the orphan grace is installed. A value nobody recognises installs hold and a note
// that says why no request will be made.
func SetDefaultOnOverdue(raw string) {
	mode, err := ParseOnOverdue(raw)
	st := &onOverdueState{mode: mode}
	if err != nil {
		st.note = fmt.Sprintf("gpu_on_overdue %q is neither request nor hold, so it is treated as hold: no request is made of any holder", strings.TrimSpace(raw))
	}
	installedOnOverdue.Store(st)
}

// InstalledOnOverdue is the policy in force.
func InstalledOnOverdue() OnOverdue {
	if st := installedOnOverdue.Load(); st != nil {
		return st.mode
	}
	return OnOverdueUnset
}

// OnOverdueNote is the sentence a holder says once when the installed value was not recognised;
// empty for a recognised one.
func OnOverdueNote() string {
	if st := installedOnOverdue.Load(); st != nil {
		return st.note
	}
	return ""
}

// Yield actions, the words the sidecar's action field carries.
const (
	YieldActionRunning = "on_yield running"
	YieldActionRan     = "ran on_yield"
	YieldActionFailed  = "on_yield failed"
	YieldActionNone    = "no on_yield recorded"
)

// YieldWaiter is one waiter that was queued for the lease's cards when its holder was asked.
type YieldWaiter struct {
	PID    int    `json:"pid"`
	Class  Class  `json:"class"`
	Reason string `json:"reason,omitempty"`
}

// YieldRequest is the content of the yield.<epoch> sidecar.
type YieldRequest struct {
	Epoch   uint64 `json:"epoch"`
	AskedMs int64  `json:"asked_ms"`
	ByPID   int    `json:"by_pid"`
	// Basis says why the lease was overdue when it was asked.
	Basis string `json:"basis"`
	// Waiters names up to three of the waiters that wanted the lease's cards; Queued counts them all.
	Waiters []YieldWaiter `json:"waiters,omitempty"`
	Queued  int           `json:"queued,omitempty"`
	Policy  string        `json:"policy"`
	// GraceMs is the time the job declared it needs to stop cleanly (--yield-grace); 0 = none declared.
	GraceMs int64 `json:"grace_ms,omitempty"`
	// ActedMs, Action and Exit are what the holder did about it (a second write, under the same lock).
	ActedMs int64  `json:"acted_ms,omitempty"`
	Action  string `json:"action,omitempty"`
	Exit    int    `json:"exit,omitempty"`
}

// QueuedCount is the number of waiters the ask found.
func (r YieldRequest) QueuedCount() int {
	if r.Queued > 0 {
		return r.Queued
	}
	return len(r.Waiters)
}

// YieldFilePath is where the sidecar of one epoch is (or will be) in a lease directory.
func YieldFilePath(leaseDir string, epoch uint64) string {
	return filepath.Join(leaseDir, yieldPrefix+strconv.FormatUint(epoch, 10))
}

// ReadYield reads the sidecar of one epoch; false when the holder was never asked. It writes nothing.
func ReadYield(leaseDir string, epoch uint64) (YieldRequest, bool) {
	b, err := os.ReadFile(YieldFilePath(leaseDir, epoch))
	if err != nil {
		return YieldRequest{}, false
	}
	var req YieldRequest
	if json.Unmarshal(b, &req) != nil || req.AskedMs <= 0 {
		return YieldRequest{}, false
	}
	return req, true
}

// YieldOf is ReadYield on a Manager.
func (m *Manager) YieldOf(epoch uint64) (YieldRequest, bool) { return ReadYield(m.leaseDir(), epoch) }

// ---------------------------------------------------------------------------
// The decision
// ---------------------------------------------------------------------------

// yieldBeforeLockHook is a test seam, called by an ask that has passed every check made outside the epoch
// lock and is about to take it. nil in production.
var yieldBeforeLockHook func()

type yieldWaiterKey struct {
	pid   int
	start int64
}

// YieldAsker decides, on a holder's tick, whether the holder of one lease is to be asked to yield.
// It is not safe for concurrent use: one holder loop owns one asker.
type YieldAsker struct {
	m     *Manager
	epoch uint64
	// asked is set once the ask was made, or found made: it is made once per epoch.
	asked bool
	// prev holds the conflicting waiters the previous tick saw, for the drive-by guard.
	prev map[yieldWaiterKey]struct{}
	// settle is how long the lease must have read overdue, tick after tick, before it is asked; overdueSince
	// is when this asker first saw it so, zero while it does not read overdue.
	settle       time.Duration
	overdueSince time.Time
}

// forget drops what a run of ticks had gathered: the lease no longer reads overdue, or the policy no longer asks.
func (a *YieldAsker) forget() {
	a.prev = nil
	a.overdueSince = time.Time{}
}

// NewYieldAsker binds the decision to one lease of this directory, held by this process.
func (m *Manager) NewYieldAsker(epoch uint64) *YieldAsker {
	return &YieldAsker{m: m, epoch: epoch}
}

// Tick makes the decision. It reports the request it wrote and true on the one tick that asked; an
// error means the request could not be recorded (and so was not made: the next tick tries again).
func (a *YieldAsker) Tick() (YieldRequest, bool, error) {
	// (a) the policy: anything but request asks nobody.
	if InstalledOnOverdue() != OnOverdueRequest {
		a.forget()
		return YieldRequest{}, false, nil
	}
	// (b) once per epoch.
	if a.asked {
		return YieldRequest{}, false, nil
	}
	m := a.m
	// (c) the record, read again now and decided from alone. A term the holder's own check renewed
	// moved the end, so a lease that was just renewed is not overdue here; the result of that check is
	// not consulted.
	rec, err := m.recordOf(a.epoch)
	if err != nil {
		a.forget() // the lease is gone; the holder's heartbeat is what notices
		return YieldRequest{}, false, nil
	}
	now := m.now()
	if !rec.Expired && !termDue(rec, now) {
		a.forget()
		return YieldRequest{}, false, nil
	}
	if a.overdueSince.IsZero() {
		a.overdueSince = now
	}
	// (d) already asked (by an earlier ask of this epoch from another asker).
	if _, serr := os.Stat(YieldFilePath(m.leaseDir(), a.epoch)); serr == nil {
		a.asked = true
		return YieldRequest{}, false, nil
	}
	// (e) the holder runs the request: a record without the stamp was written by a build that never
	// ticks this, and nothing would answer.
	if rec.Yields == "" {
		a.prev = nil
		return YieldRequest{}, false, nil
	}
	// (e2) the label has to have survived the holder's own look. The term check looks again at a lease it
	// labelled expired only every so often, and a look that failed (the cards could not be read) can label a
	// lease that a second look renews; the ask cannot be taken back, so it is not made on a label that is
	// younger than that interval. The waiter is counted from the first tick after it.
	if now.Sub(a.overdueSince) < a.settle {
		a.prev = nil
		return YieldRequest{}, false, nil
	}
	// (f) only now the waiters: reading them prunes the dead ones.
	waiters := a.conflictingWaiters(rec)
	cur := make(map[yieldWaiterKey]struct{}, len(waiters))
	var stayed []Waiter
	for _, w := range waiters {
		k := yieldWaiterKey{w.PID, w.StartTimeMs}
		cur[k] = struct{}{}
		if _, ok := a.prev[k]; ok {
			stayed = append(stayed, w)
		}
	}
	a.prev = cur
	if len(stayed) == 0 {
		return YieldRequest{}, false, nil
	}
	req := YieldRequest{
		Epoch: a.epoch, AskedMs: now.UnixMilli(), ByPID: m.pid, Basis: yieldBasis(rec, now),
		Queued: len(stayed), Policy: string(OnOverdueRequest), GraceMs: rec.YieldGraceMs,
	}
	for i, w := range stayed {
		if i == yieldWaiterSummaries {
			break
		}
		req.Waiters = append(req.Waiters, YieldWaiter{PID: w.PID, Class: w.Class, Reason: clipCommand(w.Reason)})
	}
	created, err := a.write(req)
	if err != nil {
		return YieldRequest{}, false, err
	}
	if created {
		a.asked = true
		return req, true, nil
	}
	return YieldRequest{}, false, nil
}

// conflictingWaiters lists the live waiters that want a card the lease holds. A waiter that waits only on
// host RAM (its cards are free) and the holder's own process are not demand.
func (a *YieldAsker) conflictingWaiters(rec *Meta) []Waiter {
	var out []Waiter
	for _, w := range a.m.Waiters() {
		if w.PID == a.m.pid || w.WaitingFor == WaitHostRAM {
			continue
		}
		if !devicesConflict(w.Devices, rec.Devices) {
			continue
		}
		out = append(out, w)
	}
	return out
}

// yieldBasis says how far past its end the lease is and, for a labelled one, why its term was not renewed.
func yieldBasis(rec *Meta, now time.Time) string {
	s := "expired"
	if end := endOf(rec); !end.IsZero() {
		s = "overdue by " + roundDur(now.Sub(end))
	}
	if rec.Expired {
		why := rec.ExpiredWhy
		if why == "" {
			why = "nothing vouched for it"
		}
		s += "; not renewed: " + why
	}
	return s
}

// write records the ask. The directory is probed for writability BEFORE the lock, as the orphan
// marker does (an unwritable directory fails the lock's create with a permission error the lock cannot
// tell from contention), and the lock re-checks that nobody asked meanwhile and that the lease is
// still live. It reports whether this call made the ask.
func (a *YieldAsker) write(req YieldRequest) (bool, error) {
	m := a.m
	if err := m.canWrite(); err != nil {
		return false, fmt.Errorf("yield request not recorded: the lease directory is not writable from here: %w", err)
	}
	if h := yieldBeforeLockHook; h != nil {
		h()
	}
	path := YieldFilePath(m.leaseDir(), a.epoch)
	created, exists := false, false
	var werr error
	lerr := m.withEpochLock(func() error {
		if _, serr := os.Stat(path); serr == nil {
			exists = true
			return nil
		}
		if !m.leaseStillLive(a.epoch) {
			return nil
		}
		b, merr := json.Marshal(req)
		if merr != nil {
			werr = merr
			return nil
		}
		werr = writeYieldFile(path, b)
		created = werr == nil
		return nil
	})
	switch {
	case lerr != nil:
		return false, fmt.Errorf("yield request not recorded: %w", lerr)
	case werr != nil:
		return false, fmt.Errorf("yield request not recorded: %w", werr)
	case exists:
		a.asked = true
	}
	return created, nil
}

// writeYieldFile is tmp and rename, the way every sidecar of this package is written.
func writeYieldFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o666); err != nil {
		return err
	}
	if err := renameReplacing(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// RecordYieldAction updates the sidecar of an epoch that was asked with what the holder did about it.
// It takes the epoch lock and checks the lease is still live, like the ask: an update that races the
// release leaves no file behind. An epoch that was never asked has nothing to update and is left alone;
// a request file that exists and cannot be read is an error.
func (m *Manager) RecordYieldAction(epoch uint64, action string, exit int) error {
	return m.recordYieldActionAt(epoch, action, exit, m.now().UnixMilli())
}

// recordYieldActionAt is RecordYieldAction for an action that happened at actedMs: a write that is retried
// later still says when the holder did what it did.
func (m *Manager) recordYieldActionAt(epoch uint64, action string, exit int, actedMs int64) error {
	if err := m.canWrite(); err != nil {
		return fmt.Errorf("yield action not recorded: the lease directory is not writable from here: %w", err)
	}
	path := YieldFilePath(m.leaseDir(), epoch)
	var werr error
	lerr := m.withEpochLock(func() error {
		if !m.leaseStillLive(epoch) {
			return nil
		}
		req, ok := ReadYield(m.leaseDir(), epoch)
		if !ok {
			// Never asked: nothing to update. A file that is there and cannot be read is not that.
			if _, serr := os.Stat(path); serr == nil {
				werr = fmt.Errorf("the request file %s exists but cannot be read", filepath.Base(path))
			}
			return nil
		}
		req.ActedMs, req.Action, req.Exit = actedMs, action, exit
		b, merr := json.Marshal(req)
		if merr != nil {
			werr = merr
			return nil
		}
		werr = writeYieldFile(path, b)
		return nil
	})
	if lerr != nil {
		return fmt.Errorf("yield action not recorded: %w", lerr)
	}
	if werr != nil {
		return fmt.Errorf("yield action not recorded: %w", werr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The holder's ticker
// ---------------------------------------------------------------------------

// YieldRunner runs the job's own on_yield line to its end and reports the exit status. An error is a
// failure to start it.
type YieldRunner func(line string, env []string) (exit int, err error)

// YieldTickerOptions configure a holder's yield ticker.
type YieldTickerOptions struct {
	// OnYield is the holder's OWN in-memory --on-yield value; the record is never re-read for it, so a
	// record rewritten after the acquire cannot make the holder run something else.
	OnYield string
	Run     YieldRunner
	// Out is the holder's stderr, the notification stream of the session that wraps it.
	Out io.Writer
	// Label leads the holder's lines ("gpu reserve", "gpu hold").
	Label string
	// Settle is how long a lease must have read overdue, on every tick of this holder, before the holder is
	// asked. The holders pass the interval at which their term check looks again at a lease it has labelled
	// expired, so a label is asked about only after it survived a look. Zero asks on the first overdue tick
	// that has a waiter.
	Settle time.Duration
}

// YieldTicker is the holder side of the request: ask when due, then run the job's own on_yield once.
// The runner is started in a goroutine so the holder's heartbeat goes on, and it is never waited out
// or cut short: a helper that takes an hour is left to take it.
type YieldTicker struct {
	asker *YieldAsker
	m     *Manager
	epoch uint64
	opts  YieldTickerOptions
	// noted: the unrecognised-policy sentence was said. unrecorded: the failure to record the ask was said.
	noted, unrecorded bool
	wg                sync.WaitGroup
	// pending is what the holder did that could not be written to the sidecar yet, kept for the next tick;
	// actionSaid: the failure to write it was said. The on_yield goroutine and the tick both use them.
	mu         sync.Mutex
	pending    *yieldAction
	actionSaid bool
}

// yieldAction is one thing the holder did about the request, and when.
type yieldAction struct {
	action string
	exit   int
	atMs   int64
}

// NewYieldTicker binds a yield ticker to a lease this process holds.
func (l *Lease) NewYieldTicker(o YieldTickerOptions) *YieldTicker {
	if o.Out == nil {
		o.Out = io.Discard
	}
	asker := l.mgr.NewYieldAsker(l.epoch)
	asker.settle = o.Settle
	return &YieldTicker{asker: asker, m: l.mgr, epoch: l.epoch, opts: o}
}

func (t *YieldTicker) say(format string, args ...any) {
	fmt.Fprintf(t.opts.Out, t.opts.Label+": "+format+"\n", args...)
}

// record writes what the holder did about the request. A write that fails is said once and kept for the
// next tick, which tries again: a sidecar left saying "started its on_yield" about a command that had
// ended would be read as true for the life of the lease.
func (t *YieldTicker) record(action string, exit int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending = &yieldAction{action: action, exit: exit, atMs: t.m.now().UnixMilli()}
	t.flushLocked()
}

// flush retries a write that failed.
func (t *YieldTicker) flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.flushLocked()
}

func (t *YieldTicker) flushLocked() {
	p := t.pending
	if p == nil {
		return
	}
	if err := t.m.recordYieldActionAt(t.epoch, p.action, p.exit, p.atMs); err != nil {
		if !t.actionSaid {
			t.actionSaid = true
			t.say("could not record what its --on-yield did (%v); the request file may be out of date, and the next heartbeat tries again", err)
		}
		return
	}
	t.pending, t.actionSaid = nil, false
}

// Tick is called on every holder tick, after the term check.
func (t *YieldTicker) Tick() {
	t.flush()
	if !t.noted && InstalledOnOverdue() == OnOverdueHold {
		if n := OnOverdueNote(); n != "" {
			t.noted = true
			t.say("%s", n)
		}
	}
	req, asked, err := t.asker.Tick()
	if err != nil {
		// Said once per failure; the ask was not made, nothing is run, and the next tick tries again.
		if !t.unrecorded {
			t.unrecorded = true
			t.say("could not record the yield request (%v); no on_yield is run until it can be recorded, and the next heartbeat tries again", err)
		}
		return
	}
	t.unrecorded = false
	if !asked {
		return
	}
	who := ""
	if len(req.Waiters) > 0 {
		w := req.Waiters[0]
		who = fmt.Sprintf(": pid %d", w.PID)
		if w.Reason != "" {
			who = ": " + w.Reason
		}
	}
	head := fmt.Sprintf("asked to yield (%s; %d waiter(s) queued%s)", req.Basis, req.QueuedCount(), who)
	line := strings.TrimSpace(t.opts.OnYield)
	if line == "" || t.opts.Run == nil {
		t.say("%s; no --on-yield was recorded, so there is nothing to run: the job can watch $%s (%s). The lease stays held; nothing is released or killed",
			head, YieldFileEnv, YieldFilePath(t.m.leaseDir(), t.epoch))
		t.record(YieldActionNone, 0)
		return
	}
	t.say("%s; running its --on-yield %q. The job keeps running until it stops; nothing is released or killed", head, line)
	t.record(YieldActionRunning, 0)
	env := []string{
		"GPU_LEASE_EPOCH=" + strconv.FormatUint(t.epoch, 10),
		"GPU_LEASE_DIR=" + t.m.leaseDir(),
		YieldFileEnv + "=" + YieldFilePath(t.m.leaseDir(), t.epoch),
	}
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		exit, rerr := t.opts.Run(line, env)
		action := YieldActionRan
		if rerr != nil || exit != 0 {
			action = YieldActionFailed
			if rerr != nil && exit == 0 {
				exit = -1
			}
		}
		t.record(action, exit)
		if action == YieldActionFailed {
			detail := ""
			if rerr != nil {
				detail = ": " + rerr.Error()
			}
			t.say("its --on-yield failed (exit %d%s); nothing else was done: the lease stays held and the job keeps running", exit, detail)
		}
	}()
}

// Wait blocks until the on_yield this ticker started has ended.
func (t *YieldTicker) Wait() { t.wg.Wait() }

// ---------------------------------------------------------------------------
// What a reader says
// ---------------------------------------------------------------------------

// YieldView is what a read-only surface says about the request on one lease.
type YieldView struct {
	// Capable: the holder's record says it runs the request.
	Capable bool
	// File is the sidecar path; "" for a holder that cannot be asked.
	File string
	// Asked: the sidecar exists.
	Asked bool
	Req   YieldRequest
	// Unanswered: the lease still holds its cards past the job's declared grace (or past a display-only
	// threshold when it declared none). Nothing is stopped or taken because of it.
	Unanswered bool
	// Sentence is the wording for a waiter or a status line; "" when there is nothing to say.
	Sentence string
}

// YieldViewOf reads the request state of one lease without writing anything. It is for a lease whose
// holder is alive: a tree-orphan has nobody to ask, and its callers do not call this for one.
func (m *Manager) YieldViewOf(info Info) YieldView {
	v := YieldView{Capable: info.Yields != ""}
	if info.Epoch == 0 {
		return v
	}
	if v.Capable {
		v.File = YieldFilePath(m.leaseDir(), info.Epoch)
	}
	now := m.now()
	v.Req, v.Asked = m.YieldOf(info.Epoch)
	overdue := info.Expired || (!info.ExpiresAt.IsZero() && now.After(info.ExpiresAt))
	if v.Asked {
		limit := yieldUnansweredDisplay
		if v.Req.GraceMs > 0 {
			limit = time.Duration(v.Req.GraceMs) * time.Millisecond
		}
		// The ask is once per epoch and its sidecar stays until the lease ends, so a lease can be renewed after
		// it (its owner came back, its progress resumed): that lease is no longer overdue, and an ask that was
		// answered by the job going on is not one nobody answered.
		v.Unanswered = overdue && now.Sub(time.UnixMilli(v.Req.AskedMs)) > limit
	}
	v.Sentence = yieldSentence(v, info, overdue, now)
	return v
}

func yieldSentence(v YieldView, info Info, overdue bool, now time.Time) string {
	if v.Asked {
		return askedSentence(v, info, overdue, now)
	}
	if !overdue {
		return ""
	}
	switch InstalledOnOverdue() {
	case OnOverdueHold:
		s := "gpu_on_overdue=hold: its holder is not asked to yield; it holds until it ends"
		if n := OnOverdueNote(); n != "" {
			s += " (" + n + ")"
		}
		return s
	case OnOverdueRequest:
		if !v.Capable {
			// Two kinds of holder carry no stamp and the words are true of both: a build from before the
			// request, and a holder of this build that is not a gpu reserve / gpu hold process (the media
			// pipeline acquires in process and never ticks the request).
			return "its holder does not run the yield request (a build from before it, or an in-process holder such as the media pipeline's), so no request can be made of it: ask whoever owns it"
		}
	}
	return ""
}

func askedSentence(v YieldView, info Info, overdue bool, now time.Time) string {
	req := v.Req
	hhmm := func(ms int64) string { return time.UnixMilli(ms).Local().Format("15:04") }
	s := fmt.Sprintf("its holder was asked to yield at %s (%d waiter(s) queued)", hhmm(req.AskedMs), req.QueuedCount())
	switch req.Action {
	case YieldActionRan:
		s += fmt.Sprintf(" and ran its on_yield at %s (exit %d)", hhmm(req.ActedMs), req.Exit)
	case YieldActionFailed:
		s += fmt.Sprintf(" and its on_yield failed at %s (exit %d)", hhmm(req.ActedMs), req.Exit)
	case YieldActionRunning:
		s += fmt.Sprintf(" and started its on_yield at %s, which has not ended", hhmm(req.ActedMs))
	case YieldActionNone:
		file := v.File
		if file == "" {
			file = yieldPrefix + strconv.FormatUint(info.Epoch, 10)
		}
		s += " but recorded no --on-yield, so it has nothing to run; the job can watch " + file
	}
	age := roundDur(now.Sub(time.UnixMilli(req.AskedMs)))
	if !overdue {
		// Asked while it was overdue, and the holder's term check has renewed it since: the job went on, and
		// there is nothing left to ask of anyone.
		return s + "; the lease has since been renewed and is not overdue now"
	}
	if !v.Unanswered {
		return s + fmt.Sprintf("; it still holds the card %s later", age)
	}
	if info.Yields == YieldsDetached {
		// The job is a separate process: releasing the claim frees the card at once and the job goes on
		// running on it until it ends. That is the whole truth of the command, so it is said with it.
		return s + fmt.Sprintf("; asked %s ago and it has not ended: nothing is killed; ask whoever owns it, or have its driver stop the job and run `%s` (the job is a separate process: that command frees the claim at once, and the job keeps running on the card until it ends)",
			age, ReleaseCommand(info.Epoch))
	}
	// A wrapper that loses its claim ends its own job within a tick while the card is already free, so
	// a live wrapper's lease is never pointed at the release command: the claim frees when the wrapper ends.
	return s + fmt.Sprintf("; asked %s ago and it has not ended: nothing is killed; ask whoever owns it, or stop its job by hand (wrapper pid %d); the claim frees when the wrapper ends",
		age, info.PID)
}
