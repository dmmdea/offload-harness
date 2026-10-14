package gpulease

// The review round of the yield request (plan P11, ADR 0085). Each test names the finding it closes:
// what a reader says about a lease that was asked and then renewed, the label that has to survive a
// look before its holder is asked, an action update that fails to record, a waiter's free text on
// the holder's one-line stream, and the wording for a holder that cannot be asked. Every test holds
// a lease on a scratch state root with an injected clock; none touches a GPU or a real process.

import (
	"errors"
	"os"
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
	if got, _ := ReadYield(r.m.leaseDir(), r.l.Epoch()); got.Action != YieldActionRunning {
		t.Fatalf("setup: the final write failed, so the sidecar still says %q, got %q", YieldActionRunning, got.Action)
	}
	if n := strings.Count(out.String(), "could not record what its --on-yield did"); n != 1 {
		t.Errorf("the holder says once that the result of its on_yield could not be recorded, got %d:\n%s", n, out.String())
	}
	tk.Tick() // the next heartbeat tries again
	tk.Wait()
	if got, _ := ReadYield(r.m.leaseDir(), r.l.Epoch()); got.Action != YieldActionRan || got.ActedMs == 0 {
		t.Errorf("the retry did not record the result: %+v", got)
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
