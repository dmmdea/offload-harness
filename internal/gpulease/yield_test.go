package gpulease

// The yield request (plan P11, ADR 0085): a lease that outlived its window while somebody waits
// for its cards has its HOLDER asked to yield. Every test holds a lease on a scratch state root
// with an injected clock and process table; none touches a real lease directory, a GPU or a
// process. The rule under test each time is named in the test; a negative test carries a
// positive control so that it cannot pass by never asking at all.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	yieldWaiterA = 5551
	yieldWaiterB = 5552
	yieldWindow  = time.Hour
)

// yieldBase is the instant newTestManager's clock starts at.
var yieldBase = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

// yieldPolicy installs a gpu_on_overdue value for the test and restores the unset default.
func yieldPolicy(t *testing.T, raw string) {
	t.Helper()
	SetDefaultOnOverdue(raw)
	t.Cleanup(func() { SetDefaultOnOverdue("") })
}

// yieldRig is one held lease whose window the test moves the clock past.
type yieldRig struct {
	t     *testing.T
	m     *Manager
	now   *time.Time
	base  time.Time
	l     *Lease
	procs func(pid int, up bool)
}

// newYieldRig holds a lease that runs the request (a wrapper that recorded an --on-yield and a
// grace), on a clock that starts at its acquisition. Options override the defaults.
func newYieldRig(t *testing.T, over func(*Options)) *yieldRig {
	t.Helper()
	procs := fakeProcs(t, yieldWaiterA, yieldWaiterB)
	m, now := newTestManager(t)
	opts := Options{Reason: "bench", TTL: yieldWindow, Yields: YieldsWrapper, OnYield: "touch STOP", YieldGrace: 5 * time.Minute}
	if over != nil {
		over(&opts)
	}
	if len(opts.Devices) > 0 {
		m.SetCardScoped(true)
	}
	l, err := m.TryAcquire(ClassMedia, opts)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	return &yieldRig{t: t, m: m, now: now, base: *now, l: l, procs: procs}
}

// overdueBy moves the clock to d past the end of the declared window and lets the holder
// heartbeat, as a live holder does every tick.
func (r *yieldRig) overdueBy(d time.Duration) {
	r.t.Helper()
	*r.now = r.base.Add(yieldWindow + d)
	if err := r.l.Renew(); err != nil {
		r.t.Fatalf("renew: %v", err)
	}
}

// waiter plants a live waiter record for the cards it wants (none = the whole node).
func (r *yieldRig) waiter(pid int, devices ...string) Waiter {
	r.t.Helper()
	return plantYieldWaiter(r.t, r.m, Waiter{PID: pid, Class: ClassText, Reason: fmt.Sprintf("job of %d", pid), Devices: devices})
}

func plantYieldWaiter(t *testing.T, m *Manager, w Waiter) Waiter {
	t.Helper()
	if err := os.MkdirAll(m.waitersDir(), 0o777); err != nil {
		t.Fatal(err)
	}
	if w.Class == "" {
		w.Class = ClassText
	}
	if w.SinceMs == 0 {
		w.SinceMs = m.now().UnixMilli()
	}
	w.StartTimeMs = 4242 // what newTestManager's process table reports for every pid
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(m.waitersDir(), fmt.Sprintf("%d.%d.json", w.PID, w.SinceMs))
	if err := os.WriteFile(p, b, 0o666); err != nil {
		t.Fatal(err)
	}
	return w
}

func dropYieldWaiter(t *testing.T, m *Manager, w Waiter) {
	t.Helper()
	if err := os.Remove(filepath.Join(m.waitersDir(), fmt.Sprintf("%d.%d.json", w.PID, w.SinceMs))); err != nil {
		t.Fatal(err)
	}
}

func (r *yieldRig) yieldFile() string { return YieldFilePath(r.m.leaseDir(), r.l.Epoch()) }

func (r *yieldRig) asked() bool {
	_, err := os.Stat(r.yieldFile())
	return err == nil
}

// mustAsk ticks until the asker asks (the same waiter must be seen on two consecutive ticks) and
// returns the request it wrote. It is the positive control of every negative test.
func (r *yieldRig) mustAsk(a *YieldAsker) YieldRequest {
	r.t.Helper()
	for i := 0; i < 2; i++ {
		req, asked, err := a.Tick()
		if err != nil {
			r.t.Fatalf("tick %d: %v", i+1, err)
		}
		if asked {
			return req
		}
	}
	r.t.Fatal("the holder of an overdue lease with a waiter on its cards was not asked after two ticks")
	return YieldRequest{}
}

// tickNone ticks n times and fails if any of them asks or writes the sidecar.
func (r *yieldRig) tickNone(a *YieldAsker, n int, why string) {
	r.t.Helper()
	for i := 0; i < n; i++ {
		if _, asked, err := a.Tick(); err != nil || asked {
			r.t.Fatalf("%s: tick %d asked=%v err=%v", why, i+1, asked, err)
		}
	}
	if r.asked() {
		r.t.Fatalf("%s: the sidecar exists", why)
	}
}

// ---------------------------------------------------------------------------
// The policy
// ---------------------------------------------------------------------------

func TestParseOnOverdueTrimsLowerCasesAndRefusesTheRest(t *testing.T) {
	for raw, want := range map[string]OnOverdue{"": OnOverdueUnset, "  ": OnOverdueUnset, "request": OnOverdueRequest, " Request ": OnOverdueRequest, "HOLD": OnOverdueHold, "hold": OnOverdueHold} {
		got, err := ParseOnOverdue(raw)
		if err != nil || got != want {
			t.Errorf("ParseOnOverdue(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"yield", "kill", "takeover", "requests", "on", "true"} {
		_, err := ParseOnOverdue(raw)
		if err == nil {
			t.Errorf("ParseOnOverdue(%q) accepted a value that is neither request nor hold", raw)
			continue
		}
		for _, name := range []string{"gpu_on_overdue", "request", "hold", raw} {
			if !strings.Contains(err.Error(), name) {
				t.Errorf("the refusal of %q must name %q: %v", raw, name, err)
			}
		}
	}
}

func TestSetDefaultOnOverdueInstallsAndAnUnrecognisedValueIsHold(t *testing.T) {
	t.Cleanup(func() { SetDefaultOnOverdue("") })
	for raw, want := range map[string]OnOverdue{"request": OnOverdueRequest, " Request ": OnOverdueRequest, "hold": OnOverdueHold, "": OnOverdueUnset} {
		SetDefaultOnOverdue(raw)
		if got := InstalledOnOverdue(); got != want {
			t.Errorf("installed %q reads %q, want %q", raw, got, want)
		}
		if n := OnOverdueNote(); n != "" {
			t.Errorf("a recognised value %q has no note: %q", raw, n)
		}
	}
	// The permissive direction is never the fallback: a value nobody recognises asks nobody and says so.
	for _, raw := range []string{"kill", "yield", "takeover", "reques"} {
		SetDefaultOnOverdue(raw)
		if got := InstalledOnOverdue(); got != OnOverdueHold {
			t.Errorf("installed %q reads %q, want hold", raw, got)
		}
		n := OnOverdueNote()
		for _, want := range []string{raw, "hold", "no request"} {
			if !strings.Contains(n, want) {
				t.Errorf("the note for %q must say %q: %q", raw, want, n)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// When a holder is asked
// ---------------------------------------------------------------------------

func TestAskToYieldWritesSidecarForOverdueLeaseWithConflictingWaiter(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(12 * time.Minute)
	r.waiter(yieldWaiterA)
	a := r.m.NewYieldAsker(r.l.Epoch())
	req := r.mustAsk(a)
	if req.Epoch != r.l.Epoch() || req.Policy != "request" || req.GraceMs != (5*time.Minute).Milliseconds() || req.AskedMs != r.now.UnixMilli() {
		t.Fatalf("request = %+v", req)
	}
	if !strings.Contains(req.Basis, "overdue by 12m0s") {
		t.Errorf("the basis must say how far past its window the lease is: %q", req.Basis)
	}
	if len(req.Waiters) != 1 || req.Waiters[0].PID != yieldWaiterA || req.Waiters[0].Reason == "" {
		t.Errorf("the request names who is waiting: %+v", req.Waiters)
	}
	got, ok := ReadYield(r.m.leaseDir(), r.l.Epoch())
	if !ok || got.Epoch != req.Epoch || got.AskedMs != req.AskedMs || got.Basis != req.Basis {
		t.Fatalf("the sidecar on disk = %+v (found %v), want the request just made", got, ok)
	}
	if want := filepath.Join(r.m.leaseDir(), "yield."+fmt.Sprint(r.l.Epoch())); r.yieldFile() != want {
		t.Errorf("YieldFilePath = %q, want %q", r.yieldFile(), want)
	}
}

func TestAnExpiredLabelledLeaseIsAskedAndTheBasisSaysWhy(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, func(o *Options) {
		o.Unattended, o.ProgressFile, o.Stall = true, progressFileAt(t, yieldBase), 30*time.Minute
	})
	r.overdueBy(time.Second)
	// Nothing vouches for it (its progress file is silent): the holder's own term check labels it.
	if res, err := r.l.AdvanceTerm(TermSignals{}); err != nil || res.Outcome != TermExpired {
		t.Fatalf("setup: %+v, %v", res, err)
	}
	r.waiter(yieldWaiterA)
	req := r.mustAsk(r.m.NewYieldAsker(r.l.Epoch()))
	if !strings.Contains(req.Basis, "not renewed") || !strings.Contains(req.Basis, "progress") {
		t.Errorf("the basis of an expired lease carries the reason its term was not renewed: %q", req.Basis)
	}
}

func TestNoYieldWhileWindowRuns(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.waiter(yieldWaiterA)
	a := r.m.NewYieldAsker(r.l.Epoch())
	*r.now = r.base.Add(yieldWindow - time.Minute)
	if err := r.l.Renew(); err != nil {
		t.Fatal(err)
	}
	r.tickNone(a, 4, "the window has not ended")
	r.overdueBy(time.Second)
	r.mustAsk(a) // the control: the same waiter, one second after the end
}

func TestNoYieldWithoutAConflictingWaiter(t *testing.T) {
	yieldPolicy(t, "request")
	t.Run("nobody waits", func(t *testing.T) {
		r := newYieldRig(t, nil)
		r.overdueBy(time.Minute)
		a := r.m.NewYieldAsker(r.l.Epoch())
		r.tickNone(a, 4, "nobody waits")
		r.waiter(yieldWaiterA)
		r.mustAsk(a)
	})
	t.Run("a waiter on another card of a card-scoped lease", func(t *testing.T) {
		r := newYieldRig(t, func(o *Options) { o.Devices = []string{card0} })
		r.overdueBy(time.Minute)
		a := r.m.NewYieldAsker(r.l.Epoch())
		r.waiter(yieldWaiterA, card1)
		r.tickNone(a, 4, "the waiter wants a card the lease does not hold")
		r.waiter(yieldWaiterB, card0)
		r.mustAsk(a)
	})
	t.Run("a waiter that waits only on host RAM", func(t *testing.T) {
		r := newYieldRig(t, nil)
		r.overdueBy(time.Minute)
		a := r.m.NewYieldAsker(r.l.Epoch())
		plantYieldWaiter(t, r.m, Waiter{PID: yieldWaiterA, Class: ClassText, WaitingFor: WaitHostRAM})
		r.tickNone(a, 4, "its cards are free; the memory is what it waits for")
		r.waiter(yieldWaiterB)
		r.mustAsk(a)
	})
	t.Run("the holder itself", func(t *testing.T) {
		r := newYieldRig(t, nil)
		r.overdueBy(time.Minute)
		a := r.m.NewYieldAsker(r.l.Epoch())
		plantYieldWaiter(t, r.m, Waiter{PID: os.Getpid(), Class: ClassText})
		r.tickNone(a, 4, "the only waiter is the holder's own process")
		r.waiter(yieldWaiterA)
		r.mustAsk(a)
	})
}

// A `gpu reserve --wait 30s` probe, or a seat admission that leaves after seconds, would otherwise
// stop a long job through its on_yield and be gone: the ask is once per epoch and cannot be taken back.
func TestDriveByWaiterDoesNotTriggerAsk(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	a := r.m.NewYieldAsker(r.l.Epoch())

	w := r.waiter(yieldWaiterA)
	r.tickNone(a, 1, "first sighting of a waiter")
	dropYieldWaiter(t, r.m, w)
	r.tickNone(a, 2, "the waiter left after one tick")

	// A different waiter on each tick is not one waiter that stayed.
	wa := r.waiter(yieldWaiterA)
	r.tickNone(a, 1, "a waiter, again for the first time")
	dropYieldWaiter(t, r.m, wa)
	r.waiter(yieldWaiterB)
	r.tickNone(a, 1, "another waiter on the next tick")

	// The same waiter on two consecutive ticks stayed.
	r.mustAsk(a)
}

func TestPolicyUnsetHoldAndUnrecognisedNeverWriteYield(t *testing.T) {
	for _, raw := range []string{"", "hold", "kill"} {
		t.Run("gpu_on_overdue="+raw, func(t *testing.T) {
			yieldPolicy(t, raw)
			r := newYieldRig(t, nil)
			r.overdueBy(time.Hour)
			r.waiter(yieldWaiterA)
			a := r.m.NewYieldAsker(r.l.Epoch())
			r.tickNone(a, 5, "the policy asks nobody")
			yieldPolicy(t, "request")
			r.mustAsk(a) // the control: the same state under request
		})
	}
}

func TestAHolderThatPredatesTheRequestIsNeverAsked(t *testing.T) {
	yieldPolicy(t, "request")
	// A 0.185.1-style record: a version and a window, and no `yields` stamp.
	r := newYieldRig(t, func(o *Options) { o.Yields, o.WrapperVersion = "", "0.185.1" })
	r.overdueBy(time.Hour)
	r.waiter(yieldWaiterA)
	a := r.m.NewYieldAsker(r.l.Epoch())
	r.tickNone(a, 5, "no stamp, no ask")
	if err := r.m.Restamp(r.l.Epoch(), func(meta *Meta) { meta.Yields = YieldsWrapper }); err != nil {
		t.Fatal(err)
	}
	r.mustAsk(a)
}

func TestYieldAskedOncePerEpochAcrossTicksAndAskers(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	a := r.m.NewYieldAsker(r.l.Epoch())
	first := r.mustAsk(a)
	for i := 0; i < 4; i++ {
		if _, asked, err := a.Tick(); err != nil || asked {
			t.Fatalf("tick %d after the ask asked again (%v, %v)", i+1, asked, err)
		}
	}
	// Another asker on the same epoch finds it asked and says nothing.
	b := r.m.NewYieldAsker(r.l.Epoch())
	for i := 0; i < 4; i++ {
		if _, asked, err := b.Tick(); err != nil || asked {
			t.Fatalf("a second asker asked an epoch that was asked (%v, %v)", asked, err)
		}
	}
	got, _ := ReadYield(r.m.leaseDir(), r.l.Epoch())
	if got.AskedMs != first.AskedMs || got.ByPID != first.ByPID {
		t.Errorf("the first ask stands: %+v vs %+v", got, first)
	}
}

// Two holders' ticks racing on one epoch write one request. The hook holds both at the point
// between the unlocked checks and the epoch lock, so the re-check inside the lock is what is tested.
func TestYieldAskedOnceWhenTwoAskersRace(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	askers := []*YieldAsker{r.m.NewYieldAsker(r.l.Epoch()), r.m.NewYieldAsker(r.l.Epoch())}
	for _, a := range askers {
		if _, asked, _ := a.Tick(); asked { // the first sighting of the waiter for each
			t.Fatal("asked on first sighting")
		}
	}
	var arrive sync.WaitGroup
	arrive.Add(2)
	yieldBeforeLockHook = func() { arrive.Done(); arrive.Wait() }
	t.Cleanup(func() { yieldBeforeLockHook = nil })
	var wg sync.WaitGroup
	var mu sync.Mutex
	asked := 0
	for _, a := range askers {
		wg.Add(1)
		go func(a *YieldAsker) {
			defer wg.Done()
			if _, ok, err := a.Tick(); err == nil && ok {
				mu.Lock()
				asked++
				mu.Unlock()
			}
		}(a)
	}
	wg.Wait()
	if asked != 1 {
		t.Fatalf("%d askers wrote the request, want exactly 1", asked)
	}
}

// A progressing job is renewed by its holder's own term check; the ask is decided from the record
// alone, so a lease whose end moved is not asked.
func TestJustRenewedLeaseIsNotAsked(t *testing.T) {
	yieldPolicy(t, "request")
	prog := progressFileAt(t, yieldBase)
	r := newYieldRig(t, func(o *Options) { o.Unattended, o.ProgressFile, o.Stall = true, prog, 30*time.Minute })
	r.overdueBy(time.Minute)
	touchProgress(t, prog, *r.now)
	r.waiter(yieldWaiterA)
	a := r.m.NewYieldAsker(r.l.Epoch())
	if res, err := r.l.AdvanceTerm(TermSignals{}); err != nil || res.Outcome != TermExtended {
		t.Fatalf("setup: the term must be renewed: %+v, %v", res, err)
	}
	r.tickNone(a, 4, "the term was renewed")
	// The control: the job stops progressing and the next term end is labelled.
	*r.now = r.now.Add(2 * time.Hour)
	if err := r.l.Renew(); err != nil {
		t.Fatal(err)
	}
	if res, _ := r.l.AdvanceTerm(TermSignals{}); res.Outcome != TermExpired {
		t.Fatalf("setup: %+v", res)
	}
	r.mustAsk(a)
}

func TestAskToYieldNeverTouchesTheClaim(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	a := r.m.NewYieldAsker(r.l.Epoch())
	before := recordSum(t, r.m, r.l)
	hb := readFileOr(t, r.m.heartbeatPath(r.l.Epoch()))
	r.mustAsk(a)
	if after := recordSum(t, r.m, r.l); after != before {
		t.Fatal("the ask rewrote the lease's record: the claim is the holder's, not the request's")
	}
	if got := readFileOr(t, r.m.heartbeatPath(r.l.Epoch())); got != hb {
		t.Errorf("the ask touched the heartbeat: %q -> %q", hb, got)
	}
	if err := r.l.Check(); err != nil {
		t.Errorf("the fence no longer passes after the ask: %v", err)
	}
	info := r.m.Inspect()
	if !info.Held || info.Epoch != r.l.Epoch() || info.PID != os.Getpid() {
		t.Errorf("Inspect after the ask = %+v, want the same holder", info)
	}
	if st := r.m.StandingReadOnly(info, 0); !st.Overdue || st.TreeOrphan {
		t.Errorf("the lease is still overdue and held: %+v", st)
	}
	if err := r.l.Renew(); err != nil {
		t.Errorf("the holder can still heartbeat: %v", err)
	}
}

func readFileOr(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// The sidecar's life
// ---------------------------------------------------------------------------

func TestYieldSidecarRemovedWithTheLease(t *testing.T) {
	yieldPolicy(t, "request")
	for _, tc := range []struct {
		name    string
		devices []string
	}{{"whole-node lease", nil}, {"card-scoped lease", []string{card0}}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newYieldRig(t, func(o *Options) { o.Devices = tc.devices })
			r.overdueBy(time.Minute)
			r.waiter(yieldWaiterA)
			r.mustAsk(r.m.NewYieldAsker(r.l.Epoch()))
			tmp := r.yieldFile() + ".tmp"
			if err := os.WriteFile(tmp, []byte("{"), 0o666); err != nil {
				t.Fatal(err)
			}
			if err := r.l.Release(); err != nil {
				t.Fatal(err)
			}
			for _, p := range []string{r.yieldFile(), tmp} {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Errorf("%s outlived its lease (%v)", filepath.Base(p), err)
				}
			}
		})
	}
}

func TestStaleYieldFromEarlierEpochNotInherited(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	r.mustAsk(r.m.NewYieldAsker(r.l.Epoch()))
	old := r.l.Epoch()
	if err := r.l.Release(); err != nil {
		t.Fatal(err)
	}
	next, err := r.m.TryAcquire(ClassMedia, Options{Reason: "next", TTL: yieldWindow, Yields: YieldsWrapper})
	if err != nil {
		t.Fatal(err)
	}
	if next.Epoch() == old {
		t.Fatalf("epoch %d was reused", old)
	}
	if _, ok := ReadYield(r.m.leaseDir(), next.Epoch()); ok {
		t.Error("a new epoch starts with a request already made")
	}
	if v := r.m.YieldViewOf(r.m.Inspect()); v.Asked {
		t.Errorf("the view of the new lease says it was asked: %+v", v)
	}
	*r.now = r.base.Add(2*yieldWindow + time.Minute)
	if err := next.Renew(); err != nil {
		t.Fatal(err)
	}
	if _, asked, _ := r.m.NewYieldAsker(next.Epoch()).Tick(); asked {
		t.Error("asked on a first sighting")
	}
	r.mustAsk(r.m.NewYieldAsker(next.Epoch())) // the new epoch can be asked on its own
}

// The action the holder records after its on_yield runs is made later, by another goroutine,
// and can race the release; it must not bring the file back for a lease that is gone.
func TestLateActionAfterReleaseLeavesNoFile(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	r.mustAsk(r.m.NewYieldAsker(r.l.Epoch()))
	if err := r.m.RecordYieldAction(r.l.Epoch(), YieldActionRan, 0); err != nil {
		t.Fatalf("the action of a live lease is recorded: %v", err)
	}
	if got, _ := ReadYield(r.m.leaseDir(), r.l.Epoch()); got.Action != YieldActionRan {
		t.Fatalf("the action was not recorded: %+v", got)
	}
	if err := r.l.Release(); err != nil {
		t.Fatal(err)
	}
	_ = r.m.RecordYieldAction(r.l.Epoch(), YieldActionFailed, 3)
	if r.asked() {
		t.Fatal("a late action update brought the sidecar of a released lease back")
	}
	if _, err := os.Stat(r.yieldFile() + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("a temporary file was left behind: %v", err)
	}
}

func TestAnAskThatCannotBeRecordedIsNotMadeAndIsRetried(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	a := r.m.NewYieldAsker(r.l.Epoch())
	a.Tick() // first sighting
	r.m.writeProbe = func(string) error { return errors.New("read-only file system") }
	req, asked, err := a.Tick()
	if err == nil || asked || req.AskedMs != 0 {
		t.Fatalf("an unwritable lease directory must be an error and no ask: %+v, %v, %v", req, asked, err)
	}
	if !strings.Contains(err.Error(), "yield") {
		t.Errorf("the error says what could not be recorded: %v", err)
	}
	if r.asked() {
		t.Fatal("a sidecar was written into a directory that is not writable")
	}
	r.m.writeProbe = nil
	r.mustAsk(a) // the ask was not spent: it is made as soon as it can be recorded
}

func TestRecordYieldActionUpdatesTheSidecar(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	req := r.mustAsk(r.m.NewYieldAsker(r.l.Epoch()))
	*r.now = r.now.Add(7 * time.Second)
	if err := r.m.RecordYieldAction(r.l.Epoch(), YieldActionFailed, 3); err != nil {
		t.Fatal(err)
	}
	got, ok := ReadYield(r.m.leaseDir(), r.l.Epoch())
	if !ok || got.Action != YieldActionFailed || got.Exit != 3 || got.ActedMs != r.now.UnixMilli() {
		t.Fatalf("sidecar after the action = %+v", got)
	}
	if got.AskedMs != req.AskedMs || got.Basis != req.Basis || len(got.Waiters) != len(req.Waiters) {
		t.Errorf("recording the action rewrote the ask: %+v vs %+v", got, req)
	}
	// An epoch that was never asked has nothing to update and gets no file from the attempt.
	if err := r.m.RecordYieldAction(r.l.Epoch()+99, YieldActionRan, 0); err != nil {
		t.Logf("an epoch that was never asked: %v", err)
	}
	if _, ok := ReadYield(r.m.leaseDir(), r.l.Epoch()+99); ok {
		t.Error("an action was recorded for an epoch that was never asked")
	}
}

// ---------------------------------------------------------------------------
// What a reader says, and what it never does
// ---------------------------------------------------------------------------

func TestReadOnlySurfacesNeverWriteYield(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Hour)
	r.waiter(yieldWaiterA)
	info := r.m.Inspect()
	_ = r.m.ExplainHeld(info, 0)
	_ = ExplainHeld(r.m.leaseDir(), info, 0)
	_ = r.m.StandingReadOnly(info, 0)
	_ = r.m.YieldViewOf(info)
	_ = r.m.HeldError(info).Error()
	_ = r.m.Waiters()
	if r.asked() {
		t.Fatal("a status or refusal surface wrote the request: only the holder's own tick may")
	}
	if entries, _ := os.ReadDir(r.m.leaseDir()); func() bool {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), yieldPrefix) {
				return true
			}
		}
		return false
	}() {
		t.Fatal("a yield file exists after read-only calls")
	}
	r.mustAsk(r.m.NewYieldAsker(r.l.Epoch())) // the control: the holder's tick does
}

func (r *yieldRig) askedAndActed(action string, exit int) YieldRequest {
	r.t.Helper()
	r.mustAsk(r.m.NewYieldAsker(r.l.Epoch()))
	if action != "" {
		if err := r.m.RecordYieldAction(r.l.Epoch(), action, exit); err != nil {
			r.t.Fatal(err)
		}
	}
	got, _ := ReadYield(r.m.leaseDir(), r.l.Epoch())
	return got
}

func TestViewNamesAskedActedAndUnanswered(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	// Capable and not asked yet: the path is known, nothing is said.
	v := r.m.YieldViewOf(r.m.Inspect())
	if !v.Capable || v.File != r.yieldFile() || v.Asked || v.Sentence != "" {
		t.Fatalf("a capable holder that was not asked: %+v", v)
	}
	r.askedAndActed(YieldActionRan, 0)
	at := r.now.Local().Format("15:04")

	*r.now = r.now.Add(3 * time.Minute) // inside the 5m grace the job declared
	_ = r.l.Renew()
	v = r.m.YieldViewOf(r.m.Inspect())
	if !v.Asked || v.Unanswered {
		t.Fatalf("inside the declared grace: %+v", v)
	}
	for _, want := range []string{"was asked to yield at " + at, "1 waiter", "ran its on_yield", "exit 0", "still holds the card", "3m0s"} {
		if !strings.Contains(v.Sentence, want) {
			t.Errorf("the asked sentence must say %q: %q", want, v.Sentence)
		}
	}

	*r.now = r.now.Add(3 * time.Minute) // 6m after the ask, past the 5m grace
	_ = r.l.Renew()
	v = r.m.YieldViewOf(r.m.Inspect())
	if !v.Unanswered {
		t.Fatalf("past the declared grace the ask is unanswered: %+v", v)
	}
	for _, want := range []string{"has not ended", "nothing is killed", "stop its job by hand", fmt.Sprintf("wrapper pid %d", os.Getpid()), "the claim frees when the wrapper ends"} {
		if !strings.Contains(v.Sentence, want) {
			t.Errorf("the unanswered sentence of a live wrapper must say %q: %q", want, v.Sentence)
		}
	}
}

func TestViewWithoutADeclaredGraceUsesTheDisplayThreshold(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, func(o *Options) { o.YieldGrace = 0 })
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	r.askedAndActed(YieldActionRan, 0)
	*r.now = r.now.Add(9 * time.Minute)
	_ = r.l.Renew()
	if v := r.m.YieldViewOf(r.m.Inspect()); !v.Asked || v.Unanswered {
		t.Fatalf("9m after the ask, with no grace declared: %+v", v)
	}
	*r.now = r.now.Add(2 * time.Minute)
	_ = r.l.Renew()
	if v := r.m.YieldViewOf(r.m.Inspect()); !v.Unanswered {
		t.Fatalf("11m after the ask, with no grace declared: %+v", v)
	}
}

func TestViewNeverNamesReleaseForALiveWrapper(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	r.askedAndActed(YieldActionRan, 0)
	for _, age := range []time.Duration{time.Minute, 30 * time.Minute, 5 * time.Hour} {
		*r.now = r.now.Add(age)
		_ = r.l.Renew()
		s := r.m.YieldViewOf(r.m.Inspect()).Sentence
		if s == "" || strings.Contains(s, "release") || strings.Contains(s, ReleaseCommand(r.l.Epoch())) {
			t.Errorf("%s after the ask: a live wrapper's sentence must not point at `gpu release` (it ends the job within a tick while the card is free): %q", age, s)
		}
	}
	// ExplainHeld, the sentence a waiter reads, carries the same.
	if e := r.m.ExplainHeld(r.m.Inspect(), 0); !strings.Contains(e, "was asked to yield") || strings.Contains(e, ReleaseCommand(r.l.Epoch())) {
		t.Errorf("ExplainHeld = %q", e)
	}
}

func TestViewNamesReleaseWithItsTruthForADetachedHolder(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, func(o *Options) { o.Yields = YieldsDetached })
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	r.askedAndActed(YieldActionRan, 0)
	*r.now = r.now.Add(time.Hour)
	_ = r.l.Renew()
	s := r.m.YieldViewOf(r.m.Inspect()).Sentence
	for _, want := range []string{ReleaseCommand(r.l.Epoch()), "frees the claim at once", "keeps running"} {
		if !strings.Contains(s, want) {
			t.Errorf("a detached holder's job is a separate process; the sentence must say %q: %q", want, s)
		}
	}
	if strings.Contains(s, "wrapper pid") {
		t.Errorf("a detached holder has no wrapper: %q", s)
	}
}

func TestViewSaysWhenNoOnYieldWasRecorded(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, func(o *Options) { o.OnYield = "" })
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	r.askedAndActed(YieldActionNone, 0)
	s := r.m.YieldViewOf(r.m.Inspect()).Sentence
	if !strings.Contains(s, "no --on-yield") || !strings.Contains(s, "yield."+fmt.Sprint(r.l.Epoch())) {
		t.Errorf("the job recorded no on_yield: the sentence says so and where the job can watch: %q", s)
	}
}

func TestViewSaysAHolderThatPredatesTheRequestCannotBeAsked(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, func(o *Options) { o.Yields, o.WrapperVersion = "", "0.185.1" })
	r.overdueBy(time.Hour)
	r.waiter(yieldWaiterA)
	v := r.m.YieldViewOf(r.m.Inspect())
	if v.Capable || v.File != "" || v.Asked {
		t.Fatalf("view = %+v", v)
	}
	for _, want := range []string{"predates the yield request", "ask whoever owns it"} {
		if !strings.Contains(v.Sentence, want) {
			t.Errorf("the sentence must say %q: %q", want, v.Sentence)
		}
	}
	if strings.Contains(v.Sentence, "release") {
		t.Errorf("a wrapper of this age ends its job when its claim is released: %q", v.Sentence)
	}
}

func TestViewSaysTheHoldPolicyOnlyWhenItWasChosenOrUnrecognised(t *testing.T) {
	r := newYieldRig(t, nil)
	r.overdueBy(time.Hour)
	r.waiter(yieldWaiterA)
	yieldPolicy(t, "")
	if s := r.m.YieldViewOf(r.m.Inspect()).Sentence; s != "" {
		t.Errorf("an unset key keeps the sentences of before it existed: %q", s)
	}
	yieldPolicy(t, "hold")
	if s := r.m.YieldViewOf(r.m.Inspect()).Sentence; !strings.Contains(s, "gpu_on_overdue=hold") || !strings.Contains(s, "not asked") {
		t.Errorf("hold: %q", s)
	}
	yieldPolicy(t, "kill")
	if s := r.m.YieldViewOf(r.m.Inspect()).Sentence; !strings.Contains(s, "gpu_on_overdue=hold") || !strings.Contains(s, "kill") {
		t.Errorf("an unrecognised value is worded as the hold it falls back to, naming the value: %q", s)
	}
}

func TestExplainHeldAppendsTheYieldSentenceAndKeepsTheRest(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	plain := r.m.ExplainHeld(r.m.Inspect(), 0)
	if !strings.Contains(plain, "held-overdue") || !strings.Contains(plain, TakeoverCommand(r.l.Epoch())) {
		t.Fatalf("setup: %q", plain)
	}
	r.askedAndActed(YieldActionRan, 0)
	asked := r.m.ExplainHeld(r.m.Inspect(), 0)
	if !strings.HasPrefix(asked, plain) || !strings.Contains(asked, "was asked to yield") {
		t.Errorf("the yield sentence is appended to the existing one:\n plain: %q\n asked: %q", plain, asked)
	}
}

func TestOldReaderIgnoresTheYieldRecordAndSidecar(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	read := func() (Info, bool, error, error) {
		info, _, reclaimable := legacyInspect(r.m.leaseDir(), r.m.now(), r.m.heartbeatTTL, r.m.procStart)
		_, terr := legacyTryAcquire(t, r.m, ClassMedia)
		return info, reclaimable, legacyCheck(r.m.leaseDir(), r.l.Epoch()), terr
	}
	before, brec, bchk, bacq := read()
	if !before.Held || brec || bchk != nil || bacq == nil {
		t.Fatalf("setup: the old reader must see a held, current lease: %+v reclaimable=%v check=%v acquire=%v", before, brec, bchk, bacq)
	}
	r.mustAsk(r.m.NewYieldAsker(r.l.Epoch()))
	if !r.asked() {
		t.Fatal("setup: the request was not written")
	}
	after, arec, achk, aacq := read()
	if !reflect.DeepEqual(after, before) || arec != brec || achk != nil || aacq == nil {
		t.Fatalf("the sidecar changed what a pre-change reader makes of the lease:\n before %+v\n after  %+v\n reclaimable=%v check=%v acquire=%v", before, after, arec, achk, aacq)
	}
	// The record's own new key is one the old struct ignores.
	b, err := os.ReadFile(recordPath(r.m, r.l))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"yields":"wrapper"`) {
		t.Errorf("the record does not carry the stamp: %s", b)
	}
}

func TestTheRecordCarriesTheHolderFormAndInfoMirrorsIt(t *testing.T) {
	for _, form := range []string{YieldsWrapper, YieldsDetached, ""} {
		r := newYieldRig(t, func(o *Options) { o.Yields = form })
		if got := recordOfLease(t, r.m, r.l).Yields; got != form {
			t.Errorf("record yields = %q, want %q", got, form)
		}
		if got := r.m.Inspect().Yields; got != form {
			t.Errorf("Info.Yields = %q, want %q", got, form)
		}
	}
}

// ---------------------------------------------------------------------------
// The holder's ticker
// ---------------------------------------------------------------------------

type yieldRun struct {
	mu    sync.Mutex
	calls []yieldCall
	exit  int
	err   error
	block chan struct{}
}

type yieldCall struct {
	line string
	env  []string
}

func (y *yieldRun) run(line string, env []string) (int, error) {
	y.mu.Lock()
	y.calls = append(y.calls, yieldCall{line, append([]string(nil), env...)})
	block := y.block
	y.mu.Unlock()
	if block != nil {
		<-block
	}
	return y.exit, y.err
}

func (y *yieldRun) count() int {
	y.mu.Lock()
	defer y.mu.Unlock()
	return len(y.calls)
}

type syncBuf struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

func (r *yieldRig) ticker(onYield string, y *yieldRun, out *syncBuf) *YieldTicker {
	r.t.Helper()
	return r.l.NewYieldTicker(YieldTickerOptions{OnYield: onYield, Run: y.run, Out: out, Label: "gpu reserve"})
}

// tickUntilAsked ticks the ticker until the ask is made (two ticks of one waiter).
func (r *yieldRig) tickUntilAsked(tk *YieldTicker) {
	r.t.Helper()
	for i := 0; i < 2; i++ {
		tk.Tick()
	}
	if !r.asked() {
		r.t.Fatal("the ticker did not ask after two ticks of an overdue lease with a waiter")
	}
}

func TestTickerRunsItsOwnOnYieldOnceInTheEnvironmentTheJobExpects(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	y, out := &yieldRun{}, &syncBuf{}
	// The holder's own value is what runs, never what the shared record says: a record rewritten
	// after the acquire must not be able to make the holder execute something else.
	if err := r.m.Restamp(r.l.Epoch(), func(meta *Meta) { meta.OnYield = "echo from-the-record" }); err != nil {
		t.Fatal(err)
	}
	tk := r.ticker("touch STOP", y, out)
	r.tickUntilAsked(tk)
	tk.Wait()
	for i := 0; i < 3; i++ { // later ticks do not run it again
		tk.Tick()
	}
	tk.Wait()
	if y.count() != 1 {
		t.Fatalf("on_yield ran %d times, want exactly once", y.count())
	}
	c := y.calls[0]
	if c.line != "touch STOP" {
		t.Errorf("ran %q, want the holder's own in-memory value", c.line)
	}
	env := strings.Join(c.env, "\n")
	for _, want := range []string{
		"GPU_LEASE_EPOCH=" + fmt.Sprint(r.l.Epoch()),
		"GPU_LEASE_DIR=" + r.m.leaseDir(),
		"GPU_LEASE_YIELD_FILE=" + r.yieldFile(),
	} {
		if !strings.Contains(env, want) {
			t.Errorf("the on_yield environment lacks %q:\n%s", want, env)
		}
	}
	got, _ := ReadYield(r.m.leaseDir(), r.l.Epoch())
	if got.Action != YieldActionRan || got.Exit != 0 || got.ActedMs == 0 {
		t.Errorf("sidecar after a clean on_yield = %+v", got)
	}
	line := out.String()
	for _, want := range []string{"gpu reserve: asked to yield", "1 waiter", "--on-yield", "touch STOP", "nothing is released or killed"} {
		if !strings.Contains(line, want) {
			t.Errorf("the holder says %q on stderr:\n%s", want, line)
		}
	}
	if n := strings.Count(line, "\n"); n != 1 {
		t.Errorf("a clean ask is one line on the wrapping session's stream, got %d:\n%s", n, line)
	}
}

func TestTickerWithoutOnYieldSaysSoAndKeepsHolding(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, func(o *Options) { o.OnYield = "" })
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	y, out := &yieldRun{}, &syncBuf{}
	tk := r.ticker("", y, out)
	r.tickUntilAsked(tk)
	tk.Wait()
	if y.count() != 0 {
		t.Fatal("something was run although no --on-yield was recorded")
	}
	got, _ := ReadYield(r.m.leaseDir(), r.l.Epoch())
	if got.Action != YieldActionNone {
		t.Errorf("action = %q, want %q", got.Action, YieldActionNone)
	}
	line := out.String()
	for _, want := range []string{"no --on-yield was recorded", YieldFileEnv, "nothing is released or killed"} {
		if !strings.Contains(line, want) {
			t.Errorf("the line must say %q:\n%s", want, line)
		}
	}
	if err := r.l.Check(); err != nil {
		t.Errorf("the lease is still held: %v", err)
	}
}

func TestOnYieldRunsAsynchronouslyAndTheHeartbeatContinues(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	y, out := &yieldRun{block: make(chan struct{})}, &syncBuf{}
	tk := r.ticker("slow-stop.sh", y, out)
	done := make(chan struct{})
	go func() { r.tickUntilAsked(tk); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Tick blocked on the job's on_yield: the holder's heartbeat would stop with it")
	}
	// The holder goes on heartbeating while the helper is still running.
	hb0 := r.m.Inspect().HeartbeatAt
	*r.now = r.now.Add(30 * time.Second)
	if err := r.l.Renew(); err != nil {
		t.Fatal(err)
	}
	if hb1 := r.m.Inspect().HeartbeatAt; !hb1.After(hb0) {
		t.Fatalf("the heartbeat did not advance while on_yield ran: %s -> %s", hb0, hb1)
	}
	if got, _ := ReadYield(r.m.leaseDir(), r.l.Epoch()); got.Action == YieldActionRan || got.Action == YieldActionFailed {
		t.Errorf("the helper has not finished and the sidecar says %q", got.Action)
	}
	close(y.block)
	tk.Wait()
	if got, _ := ReadYield(r.m.leaseDir(), r.l.Epoch()); got.Action != YieldActionRan {
		t.Errorf("after the helper ended the sidecar says %q, want %q", got.Action, YieldActionRan)
	}
}

func TestOnYieldFailureIsRecordedAndNothingElseHappens(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	y, out := &yieldRun{exit: 3}, &syncBuf{}
	tk := r.ticker("stop.sh", y, out)
	r.tickUntilAsked(tk)
	tk.Wait()
	got, _ := ReadYield(r.m.leaseDir(), r.l.Epoch())
	if got.Action != YieldActionFailed || got.Exit != 3 {
		t.Errorf("sidecar = %+v, want the failure and its exit status", got)
	}
	if !strings.Contains(out.String(), "failed") || !strings.Contains(out.String(), "exit 3") {
		t.Errorf("a failed on_yield is said on the stream:\n%s", out.String())
	}
	if err := r.l.Check(); err != nil || !r.m.Inspect().Held {
		t.Errorf("a failed on_yield leaves the lease held: %v", err)
	}
	for i := 0; i < 3; i++ {
		tk.Tick()
	}
	tk.Wait()
	if y.count() != 1 {
		t.Errorf("a failure is not retried: ran %d times", y.count())
	}
}

func TestTickerRunsNothingWhenTheAskCannotBeRecorded(t *testing.T) {
	yieldPolicy(t, "request")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	y, out := &yieldRun{}, &syncBuf{}
	tk := r.ticker("stop.sh", y, out)
	tk.Tick()
	r.m.writeProbe = func(string) error { return errors.New("read-only file system") }
	for i := 0; i < 3; i++ {
		tk.Tick()
	}
	tk.Wait()
	if y.count() != 0 || r.asked() {
		t.Fatalf("on_yield ran (%d) or the sidecar exists although the ask could not be recorded", y.count())
	}
	if n := strings.Count(out.String(), "could not record"); n != 1 {
		t.Errorf("the holder says once that the request could not be recorded, got %d:\n%s", n, out.String())
	}
	r.m.writeProbe = nil
	tk.Tick()
	tk.Wait()
	if y.count() != 1 {
		t.Errorf("once it can be recorded the ask is made and on_yield runs once, got %d runs", y.count())
	}
}

func TestTickerSaysOnceThatAnUnrecognisedPolicyMeansNoRequest(t *testing.T) {
	yieldPolicy(t, "kill")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Minute)
	r.waiter(yieldWaiterA)
	y, out := &yieldRun{}, &syncBuf{}
	tk := r.ticker("stop.sh", y, out)
	for i := 0; i < 4; i++ {
		tk.Tick()
	}
	tk.Wait()
	if y.count() != 0 || r.asked() {
		t.Fatal("an unrecognised policy asked")
	}
	if got := out.String(); strings.Count(got, "kill") != 1 || !strings.Contains(got, "no request") {
		t.Errorf("the first tick says once why no request will be made:\n%s", got)
	}
}

func TestTickerSilentUnderTheDefaultPolicy(t *testing.T) {
	yieldPolicy(t, "")
	r := newYieldRig(t, nil)
	r.overdueBy(time.Hour)
	r.waiter(yieldWaiterA)
	y, out := &yieldRun{}, &syncBuf{}
	tk := r.ticker("stop.sh", y, out)
	for i := 0; i < 4; i++ {
		tk.Tick()
	}
	if out.String() != "" || y.count() != 0 || r.asked() {
		t.Fatalf("the default behaviour is the one from before the key: no word, no ask (%q)", out.String())
	}
}

// ---------------------------------------------------------------------------
// The invariant: nothing here kills, signals or takes a lease
// ---------------------------------------------------------------------------

// TestNoAutomaticYieldModeExists pins GPU-I5: the request has no mode that ends a process or frees
// a claim. It reads the source of the request (this package's yield.go and the root runner), and
// allows the runner exactly one process start.
func TestNoAutomaticYieldModeExists(t *testing.T) {
	forbidden := []string{".Kill(", ".Signal(", "proctree.", "stopWrappedTree", "TerminateExcept", "taskkill", "syscall.Kill", "os.FindProcess"}
	files := []string{"yield.go"}
	root, err := filepath.Glob(filepath.Join("..", "..", "gpu_yield*.go"))
	if err != nil || len(root) == 0 {
		t.Fatalf("the root runner gpu_yield*.go was not found: %v %v", root, err)
	}
	files = append(files, root...)
	starts := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, bad := range forbidden {
			if strings.Contains(src, bad) {
				t.Errorf("%s contains %q: the yield request never ends a process", filepath.Base(f), bad)
			}
		}
		for _, bad := range []string{"os.Remove(m.", "removeClaim(", "ReleaseByEpoch(", ".Release()", "releaseV2("} {
			if filepath.Base(f) == "yield.go" && strings.Contains(src, bad) {
				t.Errorf("yield.go contains %q: the request never frees a claim", bad)
			}
		}
		starts += strings.Count(src, "winexec.Command")
	}
	if starts != 1 {
		t.Errorf("the runner starts %d processes through winexec, want exactly one (the job's own on_yield line)", starts)
	}
}
