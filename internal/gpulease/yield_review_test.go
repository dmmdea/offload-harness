package gpulease

// The review round of the yield request (plan P11, ADR 0085). Each test names the finding it closes:
// what a reader says about a lease that was asked and then renewed, the label that has to survive a
// look before its holder is asked, an action update that fails to record, a waiter's free text on
// the holder's one-line stream, and the wording for a holder that cannot be asked. Every test holds
// a lease on a scratch state root with an injected clock; none touches a GPU or a real process.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// holderTick moves the clock forward by d, lets the holder heartbeat and runs one tick of its yield
// ticker, as one tick of a holder does.
func (r *yieldRig) holderTick(tk *YieldTicker, d time.Duration) {
	r.t.Helper()
	*r.now = r.now.Add(d)
	if err := r.l.Renew(); err != nil {
		r.t.Fatalf("renew: %v", err)
	}
	tk.Tick()
}

// newLabelledYieldRig holds an unattended lease whose progress file is silent: the holder's own term
// check, run at the end of the window, labels it expired. It returns the progress file so a test can
// make the job resume.
func newLabelledYieldRig(t *testing.T) (*yieldRig, string) {
	t.Helper()
	prog := progressFileAt(t, yieldBase)
	r := newYieldRig(t, func(o *Options) { o.Unattended, o.ProgressFile, o.Stall = true, prog, 30*time.Minute })
	r.overdueBy(time.Second)
	if res, err := r.l.AdvanceTerm(TermSignals{}); err != nil || res.Outcome != TermExpired {
		t.Fatalf("setup: the term check must label the lease expired: %+v, %v", res, err)
	}
	return r, prog
}

// failProbeOn makes the lease directory unwritable on the numbered calls of the writability probe. The
// ask makes the first call, the first write of what the holder did makes the second, the next the third.
func (r *yieldRig) failProbeOn(calls ...int) {
	n := 0
	r.m.writeProbe = func(string) error {
		n++
		for _, c := range calls {
			if c == n {
				return errors.New("read-only file system")
			}
		}
		return nil
	}
}

// ---------------------------------------------------------------------------
// What a reader says about a lease that was asked and then renewed
// ---------------------------------------------------------------------------

// The ask is made once per epoch and its sidecar stays until the lease ends; the lease itself can be
// renewed after it (its owner came back, its progress resumed). Every surface then described a healthy
// lease as one whose holder had been asked and had not ended: nothing was killed, ask whoever owns it.
func TestAnAskedLeaseThatWasRenewedAfterwardsIsNoLongerSaidToBeUnanswered(t *testing.T) {
	yieldPolicy(t, "request")
	r, prog := newLabelledYieldRig(t)
	r.waiter(yieldWaiterA)
	r.askedAndActed(YieldActionRan, 0)

	// The control: still overdue past the job's 5 minute grace, the ask is unanswered and says so.
	*r.now = r.now.Add(6 * time.Minute)
	_ = r.l.Renew()
	v := r.m.YieldViewOf(r.m.Inspect())
	if !v.Unanswered || !strings.Contains(v.Sentence, "has not ended") {
		t.Fatalf("setup: an overdue lease asked 6m ago, past its 5m grace, is unanswered: %+v", v)
	}

	// The job resumed its progress and the holder's term check renewed the lease.
	touchProgress(t, prog, *r.now)
	if res, err := r.l.AdvanceTerm(TermSignals{}); err != nil || res.Outcome != TermExtended {
		t.Fatalf("setup: the term must be renewed: %+v, %v", res, err)
	}
	v = r.m.YieldViewOf(r.m.Inspect())
	if !v.Asked {
		t.Fatalf("the sidecar of the epoch stands, so the view still knows it was asked: %+v", v)
	}
	if v.Unanswered {
		t.Errorf("a lease that is not overdue is not an ask nobody answered: %+v", v)
	}
	for _, bad := range []string{"has not ended", "ask whoever owns it", "stop its job by hand"} {
		if strings.Contains(v.Sentence, bad) {
			t.Errorf("the sentence about a renewed lease must not say %q: %q", bad, v.Sentence)
		}
	}
	for _, want := range []string{"was asked to yield", "renewed"} {
		if !strings.Contains(v.Sentence, want) {
			t.Errorf("the sentence must say %q: %q", want, v.Sentence)
		}
	}
}

// ---------------------------------------------------------------------------
// A label is asked about only after it survived a look
// ---------------------------------------------------------------------------

// The holder's term check looks again at a lease it labelled expired once a minute (a look at the cards
// that failed can label a lease that a second look would renew). The ask cannot be taken back, so a lease is
// asked about only after its label survived that look: with a holder that settles for a minute, no tick
// inside the minute asks, however long the waiter has stayed.
func TestALabelThatHasNotSurvivedARecheckIsNotAsked(t *testing.T) {
	yieldPolicy(t, "request")
	r, _ := newLabelledYieldRig(t)
	r.waiter(yieldWaiterA)
	y, out := &yieldRun{}, &syncBuf{}
	tk := r.l.NewYieldTicker(YieldTickerOptions{OnYield: "touch STOP", Run: y.run, Out: out, Label: "gpu reserve", Settle: time.Minute})

	tk.Tick() // the tick that found the label
	for i := 0; i < 3; i++ {
		r.holderTick(tk, 15*time.Second) // +15 s, +30 s, +45 s: the label has not been looked at again
		if r.asked() {
			t.Fatalf("asked %d s after the label, before the holder's term check looked at it again", (i+1)*15)
		}
	}
	// The control: the label survives the look and the waiter has stayed, so the holder is asked.
	for i := 0; i < 3 && !r.asked(); i++ {
		r.holderTick(tk, 15*time.Second)
	}
	if !r.asked() {
		t.Fatal("a label that survived a minute with a waiter on its cards was never asked")
	}
	tk.Wait()
	if y.count() != 1 {
		t.Errorf("on_yield ran %d times, want once", y.count())
	}
}

// The look renews the lease (the job was progressing after all): it is never asked.
func TestALabelThatTheRecheckRenewsIsNeverAsked(t *testing.T) {
	yieldPolicy(t, "request")
	r, prog := newLabelledYieldRig(t)
	r.waiter(yieldWaiterA)
	y, out := &yieldRun{}, &syncBuf{}
	tk := r.l.NewYieldTicker(YieldTickerOptions{OnYield: "touch STOP", Run: y.run, Out: out, Label: "gpu reserve", Settle: time.Minute})

	tk.Tick()
	for i := 0; i < 3; i++ {
		r.holderTick(tk, 15*time.Second)
	}
	// The job's progress resumes before the minute is out; the term check at +60 s renews the lease.
	touchProgress(t, prog, *r.now)
	*r.now = r.now.Add(15 * time.Second)
	_ = r.l.Renew()
	if res, err := r.l.AdvanceTerm(TermSignals{}); err != nil || res.Outcome != TermExtended {
		t.Fatalf("setup: the recheck must renew the lease: %+v, %v", res, err)
	}
	tk.Tick()
	for i := 0; i < 12; i++ {
		r.holderTick(tk, 15*time.Second)
	}
	tk.Wait()
	if r.asked() || y.count() != 0 {
		t.Fatalf("a lease the recheck renewed was asked (asked=%v, on_yield ran %d times)", r.asked(), y.count())
	}
}

// A lease that was renewed and is later labelled again starts its settle interval again: the interval is about
// the label in front of the holder, not about the first time the lease was ever overdue.
func TestTheSettleIntervalStartsAgainWhenARenewedLeaseIsLabelledAgain(t *testing.T) {
	yieldPolicy(t, "request")
	r, prog := newLabelledYieldRig(t)
	r.waiter(yieldWaiterA)
	y, out := &yieldRun{}, &syncBuf{}
	tk := r.l.NewYieldTicker(YieldTickerOptions{OnYield: "touch STOP", Run: y.run, Out: out, Label: "gpu reserve", Settle: time.Minute})
	tk.Tick() // the tick that finds the first label

	// The job resumes, the term check renews the lease, and the next tick sees a lease that is not overdue.
	*r.now = r.now.Add(15 * time.Second)
	_ = r.l.Renew()
	touchProgress(t, prog, *r.now)
	if res, err := r.l.AdvanceTerm(TermSignals{}); err != nil || res.Outcome != TermExtended {
		t.Fatalf("setup: the term must be renewed: %+v, %v", res, err)
	}
	tk.Tick()

	// Two hours on, the job is silent again and the term check labels the lease a second time.
	*r.now = r.now.Add(2 * time.Hour)
	_ = r.l.Renew()
	if res, err := r.l.AdvanceTerm(TermSignals{}); err != nil || res.Outcome != TermExpired {
		t.Fatalf("setup: the second label: %+v, %v", res, err)
	}
	tk.Tick() // the tick that finds the second label
	for i := 0; i < 3; i++ {
		r.holderTick(tk, 15*time.Second)
		if r.asked() {
			t.Fatalf("asked %d s after the second label, on the strength of a label that was seen two hours ago", (i+1)*15)
		}
	}
	for i := 0; i < 3 && !r.asked(); i++ { // the control: the second label survives its own minute
		r.holderTick(tk, 15*time.Second)
	}
	if !r.asked() {
		t.Fatal("the second label survived a minute with a waiter on its cards and was never asked")
	}
	tk.Wait()
}

// ---------------------------------------------------------------------------
// What the holder did about the request, and could not record
// ---------------------------------------------------------------------------

// The update that records the result of the job's on_yield can fail (the epoch lock did not clear, the
// directory went read-only). Left unsaid, the sidecar read "started its on_yield, which has not ended" for the
// life of the lease about a command that had finished.
func TestAFailedRecordOfTheFinalActionIsSaidOnceAndRetried(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	y, out := &yieldRun{block: make(chan struct{})}, &syncBuf{}
	tk := r.ticker("stop.sh", y, out)
	r.failProbeOn(3)
	r.tickUntilAsked(tk)
	close(y.block)
	tk.Wait()
	endedMs := r.now.UnixMilli()
	if got, _ := ReadYield(r.m.leaseDir(), r.l.Epoch()); got.Action != YieldActionRunning {
		t.Fatalf("setup: the final write failed, so the sidecar still says %q, got %q", YieldActionRunning, got.Action)
	}
	if n := strings.Count(out.String(), "could not record what its --on-yield did"); n != 1 {
		t.Errorf("the holder says once that the result of its on_yield could not be recorded, got %d:\n%s", n, out.String())
	}
	*r.now = r.now.Add(7 * time.Second) // the next heartbeat tries again, a little later
	tk.Tick()
	tk.Wait()
	got, _ := ReadYield(r.m.leaseDir(), r.l.Epoch())
	if got.Action != YieldActionRan {
		t.Errorf("the retry did not record the result: %+v", got)
	}
	if got.ActedMs != endedMs {
		t.Errorf("the retry records when the command ended (%d), not when the write finally succeeded: %d", endedMs, got.ActedMs)
	}
	if n := strings.Count(out.String(), "could not record what its --on-yield did"); n != 1 {
		t.Errorf("the failure is not said again, got %d:\n%s", n, out.String())
	}
	if y.count() != 1 {
		t.Errorf("a failed record is not a reason to run the command again: ran %d times", y.count())
	}
}

func TestAFailedRecordOfTheRunningActionIsSaid(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	y, out := &yieldRun{}, &syncBuf{}
	tk := r.ticker("stop.sh", y, out)
	r.failProbeOn(2)
	r.tickUntilAsked(tk)
	tk.Wait()
	if n := strings.Count(out.String(), "could not record what its --on-yield did"); n != 1 {
		t.Errorf("the holder says once that it could not record the start of its on_yield, got %d:\n%s", n, out.String())
	}
	if got, _ := ReadYield(r.m.leaseDir(), r.l.Epoch()); got.Action != YieldActionRan {
		t.Errorf("the result of the command, which could be recorded, is on the sidecar: %+v", got)
	}
}

func TestAFailedRecordOfNoOnYieldIsRetried(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, func(o *Options) { o.OnYield = "" })
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	y, out := &yieldRun{}, &syncBuf{}
	tk := r.ticker("", y, out)
	r.failProbeOn(2)
	r.tickUntilAsked(tk)
	if n := strings.Count(out.String(), "could not record what its --on-yield did"); n != 1 {
		t.Errorf("the holder says once that it could not record, got %d:\n%s", n, out.String())
	}
	if got, _ := ReadYield(r.m.leaseDir(), r.l.Epoch()); got.Action != "" {
		t.Fatalf("setup: nothing was recorded yet: %+v", got)
	}
	tk.Tick()
	if got, _ := ReadYield(r.m.leaseDir(), r.l.Epoch()); got.Action != YieldActionNone {
		t.Errorf("the next tick records that no on_yield was recorded: %+v", got)
	}
}

// A sidecar that exists and cannot be read is not "never asked": an update that finds nothing to change
// and says nothing would leave the file as it is for ever.
func TestRecordYieldActionOnAnUnreadableSidecarIsAnError(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	r.mustAsk(r.m.NewYieldAsker(r.l.Epoch()))
	if err := os.WriteFile(r.yieldFile(), []byte("{"), 0o666); err != nil {
		t.Fatal(err)
	}
	err := r.m.RecordYieldAction(r.l.Epoch(), YieldActionRan, 0)
	if err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("an unreadable request must be an error naming it, got %v", err)
	}
	if got := readFileOr(t, r.yieldFile()); got != "{" {
		t.Errorf("the file was rewritten: %q", got)
	}
	// An epoch nobody asked has nothing to update, and that is not an error.
	if err := r.m.RecordYieldAction(r.l.Epoch()+99, YieldActionRan, 0); err != nil {
		t.Errorf("an epoch that was never asked has nothing to update: %v", err)
	}
}

// ---------------------------------------------------------------------------
// A waiter's free text on the holder's one-line stream
// ---------------------------------------------------------------------------

// The reason of a waiter is typed by any local process. The holder's stderr is the notification stream
// of the session that wraps it, one line per event, so a newline or a control sequence in the reason must
// not split the line or inject text.
func TestAWaitersFreeTextReasonCannotSplitTheHoldersLine(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	plantYieldWaiter(t, r.m, Waiter{PID: yieldWaiterA, Class: ClassText, Reason: "kv bench\nlocal-offload: the lease was released by the harness\x1b[2J\r\tdone"})
	y, out := &yieldRun{}, &syncBuf{}
	tk := r.ticker("stop.sh", y, out)
	r.tickUntilAsked(tk)
	tk.Wait()
	line := out.String()
	if n := strings.Count(line, "\n"); n != 1 {
		t.Errorf("a clean ask is one line on the wrapping session's stream, got %d:\n%q", n, line)
	}
	if strings.ContainsAny(strings.TrimSuffix(line, "\n"), "\r\t\x1b") {
		t.Errorf("a control character of the waiter's reason reached the stream: %q", line)
	}
	if !strings.Contains(line, "kv bench") {
		t.Errorf("the line still names who waits: %q", line)
	}
}

// ---------------------------------------------------------------------------
// Coverage of rules the ADR states: what the sidecar says while the command runs, what a reader says of a
// command that failed or never started, that the waiters are read last, and that the ask re-checks the lease
// under the lock
// ---------------------------------------------------------------------------

// While the command runs the sidecar says so, and a reader says the command was started and has not ended.
func TestTheSidecarSaysItsOnYieldIsRunningWhileItRuns(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	y, out := &yieldRun{block: make(chan struct{})}, &syncBuf{}
	tk := r.ticker("stop.sh", y, out)
	r.tickUntilAsked(tk)
	got, _ := ReadYield(r.m.leaseDir(), r.l.Epoch())
	if got.Action != YieldActionRunning || got.ActedMs == 0 {
		t.Fatalf("while the command runs the sidecar says %q at %d, want %q", got.Action, got.ActedMs, YieldActionRunning)
	}
	at := time.UnixMilli(got.ActedMs).Local().Format("15:04")
	s := r.m.YieldViewOf(r.m.Inspect()).Sentence
	for _, want := range []string{"started its on_yield at " + at, "which has not ended"} {
		if !strings.Contains(s, want) {
			t.Errorf("a reader says %q while the command runs: %q", want, s)
		}
	}
	close(y.block)
	tk.Wait()
	if got, _ := ReadYield(r.m.leaseDir(), r.l.Epoch()); got.Action != YieldActionRan {
		t.Errorf("when the command ends the sidecar says %q, want %q", got.Action, YieldActionRan)
	}
}

func TestAFailedOnYieldIsWordedWithItsExitStatusForAReader(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	r.askedAndActed(YieldActionFailed, 3)
	at := r.now.Local().Format("15:04")
	s := r.m.YieldViewOf(r.m.Inspect()).Sentence
	for _, want := range []string{"was asked to yield at " + at, "its on_yield failed at " + at, "exit 3"} {
		if !strings.Contains(s, want) {
			t.Errorf("a reader says %q of a command that failed: %q", want, s)
		}
	}
	if strings.Contains(s, "ran its on_yield") {
		t.Errorf("a command that failed did not run to its end: %q", s)
	}
}

// A command that cannot be started at all (no shell) is a failure with no exit status of its own: it is
// recorded as one, with the reason, and nothing else happens.
func TestAnOnYieldThatCannotBeStartedIsRecordedAsAFailure(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	y, out := &yieldRun{err: errors.New("exec: sh: executable file not found in $PATH")}, &syncBuf{}
	tk := r.ticker("stop.sh", y, out)
	r.tickUntilAsked(tk)
	tk.Wait()
	got, _ := ReadYield(r.m.leaseDir(), r.l.Epoch())
	if got.Action != YieldActionFailed || got.Exit != -1 {
		t.Errorf("a command that could not start is a failure with exit -1, got %+v", got)
	}
	for _, want := range []string{"failed", "exit -1", "executable file not found"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the holder says %q:\n%s", want, out.String())
		}
	}
	if err := r.l.Check(); err != nil || !r.m.Inspect().Held {
		t.Errorf("a command that could not start leaves the lease held: %v", err)
	}
}

// Reading the waiters prunes the records of the dead ones, so it is the last thing a tick does: a tick that
// stops earlier (the policy asks nobody, the window runs, the holder was asked, the holder cannot be asked,
// the label is too young) reads none and leaves another process's waiter records as they were.
func TestTheWaitersAreReadLastBecauseReadingPrunesTheDead(t *testing.T) {
	const dead = 5599 // not in the table of live processes
	deadRecord := func(r *yieldRig) string {
		w := plantYieldWaiter(r.t, r.m, Waiter{PID: dead, Class: ClassText, Reason: "gone"})
		return filepath.Join(r.m.waitersDir(), fmt.Sprintf("%d.%d.json", w.PID, w.SinceMs))
	}
	survives := func(t *testing.T, path, why string) {
		t.Helper()
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s: the tick read the waiters and pruned a record it had no reason to read (%v)", why, err)
		}
	}
	t.Run("the policy asks nobody", func(t *testing.T) {
		yieldPolicy(t, "")
		r := newYieldRig(t, nil)
		r.overdueBy(time.Hour)
		rec := deadRecord(r)
		a := r.m.NewYieldAsker(r.l.Epoch())
		r.tickNone(a, 3, "unset policy")
		survives(t, rec, "unset policy")
	})
	t.Run("the window runs", func(t *testing.T) {
		yieldPolicy(t, "request")
		r := newYieldRig(t, nil)
		rec := deadRecord(r)
		a := r.m.NewYieldAsker(r.l.Epoch())
		*r.now = r.base.Add(yieldWindow - time.Minute)
		_ = r.l.Renew()
		r.tickNone(a, 3, "inside the window")
		survives(t, rec, "inside the window")
	})
	t.Run("the holder cannot be asked", func(t *testing.T) {
		yieldPolicy(t, "request")
		r := newYieldRig(t, func(o *Options) { o.Yields = "" })
		r.overdueBy(time.Hour)
		rec := deadRecord(r)
		a := r.m.NewYieldAsker(r.l.Epoch())
		r.tickNone(a, 3, "no stamp")
		survives(t, rec, "a holder with no stamp")
	})
	t.Run("the holder was asked", func(t *testing.T) {
		yieldPolicy(t, "request")
		r := newYieldRig(t, nil)
		r.overdueBy(time.Hour)
		r.waiter(yieldWaiterA)
		a := r.m.NewYieldAsker(r.l.Epoch())
		r.mustAsk(a)
		rec := deadRecord(r)
		for i := 0; i < 3; i++ {
			a.Tick()
		}
		survives(t, rec, "an epoch already asked")
	})
	t.Run("the label is too young", func(t *testing.T) {
		yieldPolicy(t, "request")
		r, _ := newLabelledYieldRig(t)
		rec := deadRecord(r)
		tk := r.l.NewYieldTicker(YieldTickerOptions{OnYield: "stop.sh", Run: (&yieldRun{}).run, Out: &syncBuf{}, Label: "gpu reserve", Settle: time.Minute})
		for i := 0; i < 3; i++ {
			r.holderTick(tk, 10*time.Second)
		}
		survives(t, rec, "a label inside its settle interval")
	})
	// The control: a tick that does read the waiters (overdue, capable, asking) prunes the dead record.
	t.Run("the control prunes it", func(t *testing.T) {
		yieldPolicy(t, "request")
		r := newYieldRig(t, nil)
		r.overdueBy(time.Hour)
		rec := deadRecord(r)
		a := r.m.NewYieldAsker(r.l.Epoch())
		a.Tick()
		if _, err := os.Stat(rec); !os.IsNotExist(err) {
			t.Errorf("setup: a tick that reads the waiters prunes a dead record, and this one did not (%v)", err)
		}
	})
}

// The ask re-checks, under the epoch lock, that the lease is still live: a lease released between the checks
// made outside the lock and the lock itself must not get a request file after the sweep that ended it.
func TestAnAskDoesNotWriteForALeaseReleasedBeforeTheLock(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	a := r.m.NewYieldAsker(r.l.Epoch())
	if _, asked, _ := a.Tick(); asked { // the first sighting of the waiter
		t.Fatal("asked on first sighting")
	}
	released := false
	yieldBeforeLockHook = func() {
		if err := r.l.Release(); err != nil {
			t.Errorf("release: %v", err)
		}
		released = true
	}
	t.Cleanup(func() { yieldBeforeLockHook = nil })
	req, asked, err := a.Tick()
	if !released {
		t.Fatal("setup: the ask never reached the lock")
	}
	if err != nil || asked || req.AskedMs != 0 {
		t.Errorf("a lease released before the lock is not asked: %+v, %v, %v", req, asked, err)
	}
	if r.asked() {
		t.Error("the ask wrote a request file for a lease that was already gone")
	}
}

// The case the settle interval is for: an attended lease whose owner is alive, labelled because the look at its
// cards failed (the sample timed out under a saturated GPU). A waiter stays on its cards; the holder does not
// ask inside the minute, and when the second look finds the cards working the lease is renewed and never asked.
func TestALabelThatCameFromAnUnreadableLookAndTheSecondLookRenewsIsNeverAsked(t *testing.T) {
	yieldPolicy(t, "request")
	f := newTermFixtureNP(t, false, Options{Yields: YieldsWrapper, OnYield: "touch STOP", YieldGrace: 5 * time.Minute})
	f.at(t, time.Hour+time.Second, false)
	res, err := f.l.AdvanceTerm(TermSignals{Cards: cardsAre(CardsUnreadable)})
	if err != nil || res.Outcome != TermExpired || !strings.Contains(res.Why, "could not be read") {
		t.Fatalf("setup: the term check must label the lease because the cards could not be read: %+v, %v", res, err)
	}
	plantYieldWaiter(t, f.m, Waiter{PID: termOwnerPID, Class: ClassText, Reason: "kv bench"})
	y, out := &yieldRun{}, &syncBuf{}
	tk := f.l.NewYieldTicker(YieldTickerOptions{OnYield: "touch STOP", Run: y.run, Out: out, Label: "gpu reserve", Settle: time.Minute})
	asked := func() bool { _, err := os.Stat(YieldFilePath(f.m.leaseDir(), f.l.Epoch())); return err == nil }

	tk.Tick() // the tick that found the label
	for i := 0; i < 3; i++ {
		*f.now = f.now.Add(15 * time.Second)
		if err := f.l.Renew(); err != nil {
			t.Fatal(err)
		}
		tk.Tick()
		if asked() {
			t.Fatalf("asked %d s after a label that came from a look at the cards that failed", (i+1)*15)
		}
	}
	// A minute after the label the term check looks again, the cards are busy, and the lease is renewed.
	*f.now = f.now.Add(15 * time.Second)
	if err := f.l.Renew(); err != nil {
		t.Fatal(err)
	}
	if res, err := f.l.AdvanceTerm(TermSignals{Cards: cardsAre(CardsWorking)}); err != nil || res.Outcome != TermExtended {
		t.Fatalf("setup: the second look must renew the lease: %+v, %v", res, err)
	}
	tk.Tick()
	for i := 0; i < 16; i++ {
		*f.now = f.now.Add(15 * time.Second)
		if err := f.l.Renew(); err != nil {
			t.Fatal(err)
		}
		tk.Tick()
	}
	tk.Wait()
	if asked() || y.count() != 0 {
		t.Fatalf("a lease the second look renewed was asked (asked=%v, on_yield ran %d times)", asked(), y.count())
	}
}

// A lease the same tick renews is never asked, and that does not depend on which of the holder's two checks
// runs first within a tick: the asker never asks on the first tick a lease reads overdue (that is where it first
// sees the waiter), and a renewal moves the end before the next tick reads it. Both orders, a progressing job and
// a waiter on its cards for the whole of it, with no settle interval to help.
func TestARenewableLeaseIsNeverAskedWhicheverCheckRunsFirstInATick(t *testing.T) {
	for _, order := range []string{"term check then yield tick", "yield tick then term check"} {
		t.Run(order, func(t *testing.T) {
			yieldPolicy(t, "request")
			prog := progressFileAt(t, yieldBase)
			r := newYieldRig(t, func(o *Options) { o.Unattended, o.ProgressFile, o.Stall = true, prog, 30*time.Minute })
			r.waiter(yieldWaiterA)
			y, out := &yieldRun{}, &syncBuf{}
			tk := r.ticker("stop.sh", y, out)
			renewals := 0
			for i := 0; i < 16; i++ { // 15 s ticks, from a minute before the end of the first term to three minutes after
				*r.now = r.base.Add(yieldWindow - time.Minute + time.Duration(i)*15*time.Second)
				_ = r.l.Renew()
				touchProgress(t, prog, *r.now) // the job is working
				termCheck := func() {
					if res, err := r.l.AdvanceTerm(TermSignals{}); err != nil {
						t.Fatalf("term check: %v", err)
					} else if res.Outcome == TermExtended {
						renewals++
					}
				}
				if order == "term check then yield tick" {
					termCheck()
					tk.Tick()
				} else {
					tk.Tick()
					termCheck()
				}
				if r.asked() {
					t.Fatalf("tick %d: a progressing job, renewed at its term end, was asked to yield", i+1)
				}
			}
			tk.Wait()
			if renewals != 1 || y.count() != 0 {
				t.Fatalf("setup: the term must have been renewed once (%d) and nothing run (%d)", renewals, y.count())
			}
		})
	}
}
