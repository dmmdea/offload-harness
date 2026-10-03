package gpulease

// Bounded terms, renewal and the expired label (plan P9, ADR 0070). All on a scratch lease
// root with an injected clock and process table; nothing here touches a real lease
// directory or a GPU.

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// termOwnerPID is the live owner process most tests name; fakeProcs(t, termOwnerPID) makes it
// alive, and fakeProcs(t) leaves it dead.
const termOwnerPID = 777

func progressFileAt(t *testing.T, at time.Time) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "work", "log.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
		t.Fatal(err)
	}
	touchProgress(t, p, at)
	return p
}

func touchProgress(t *testing.T, p string, at time.Time) {
	t.Helper()
	if err := os.WriteFile(p, []byte(`{"done":1,"total":9}`+"\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

func recordPath(m *Manager, l *Lease) string {
	if l.v2 {
		return epochRecordPath(m.leaseDir(), l.epoch)
	}
	return m.metaPath()
}

func recordSum(t *testing.T, m *Manager, l *Lease) [32]byte {
	t.Helper()
	b, err := os.ReadFile(recordPath(m, l))
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// recordOfLease reads the lease's own record back from disk.
func recordOfLease(t *testing.T, m *Manager, l *Lease) Meta {
	t.Helper()
	b, err := os.ReadFile(recordPath(m, l))
	if err != nil {
		t.Fatal(err)
	}
	var meta Meta
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	return meta
}

// clockAt moves the fake clock to base+d.
func clockAt(now *time.Time, base time.Time, d time.Duration) { *now = base.Add(d) }

func neverUtil(t *testing.T) func() bool {
	return func() bool {
		t.Helper()
		t.Fatal("the cards' utilisation was consulted where it cannot change the answer")
		return false
	}
}

// ---------------------------------------------------------------------------
// The plan: what a request comes to
// ---------------------------------------------------------------------------

func TestOverCapRequestIsRecordedWarnedNotShortened(t *testing.T) {
	// A request above the cap on a term is accepted whole: the declared window is the request,
	// the request is recorded (RequestedMs) and the caller is handed a warning to print.
	p := PlanTerm(20*time.Hour, false, 6*time.Hour, 48*time.Hour)
	if p.Window != 20*time.Hour || p.Term != 6*time.Hour || p.Requested != 20*time.Hour || p.Cap != 6*time.Hour || p.MaxTotal != 48*time.Hour {
		t.Fatalf("plan = %+v, want window 20h, term 6h, requested 20h, cap 6h, max total 48h", p)
	}
	for _, want := range []string{"20h0m0s", "6h0m0s", "never shortened", "gpu_max_term_min"} {
		if !strings.Contains(p.Warning, want) {
			t.Errorf("the warning must say %q: %q", want, p.Warning)
		}
	}
	// At or under the cap: nothing recorded, nothing said.
	if p := PlanTerm(5*time.Hour, false, 6*time.Hour, 48*time.Hour); p.Requested != 0 || p.Warning != "" || p.Term != 5*time.Hour {
		t.Errorf("a request under the cap records and says nothing: %+v", p)
	}
	if p := PlanTerm(6*time.Hour, false, 6*time.Hour, 48*time.Hour); p.Requested != 0 || p.Warning != "" || p.Term != 6*time.Hour {
		t.Errorf("a request AT the cap is not over it: %+v", p)
	}
	// A progress contract may declare up to 24 h at the start without a word.
	if p := PlanTerm(20*time.Hour, true, 6*time.Hour, 48*time.Hour); p.Requested != 0 || p.Warning != "" || p.Term != 20*time.Hour || p.Cap != 24*time.Hour {
		t.Errorf("a progress contract lifts the cap to 24h: %+v", p)
	}
	if p := PlanTerm(30*time.Hour, true, 6*time.Hour, 48*time.Hour); p.Requested != 30*time.Hour || p.Term != 24*time.Hour || p.Warning == "" {
		t.Errorf("above 24h even a progress contract is recorded and warned: %+v", p)
	}
	// An operator who raised the term cap above 24h keeps it for a progress contract too.
	if p := PlanTerm(30*time.Hour, true, 36*time.Hour, 72*time.Hour); p.Cap != 36*time.Hour || p.Requested != 0 {
		t.Errorf("the cap is the larger of the term limit and 24h: %+v", p)
	}
	// A request above the maximum total is not shortened either; it is simply never renewed.
	p = PlanTerm(60*time.Hour, false, 6*time.Hour, 48*time.Hour)
	if p.Window != 60*time.Hour || p.MaxTotal != 60*time.Hour {
		t.Fatalf("the hard end is never before the declared end: %+v", p)
	}
	if !strings.Contains(p.Warning, "gpu_max_total_min") || !strings.Contains(p.Warning, "never renewed") {
		t.Errorf("a request above the maximum total says it is never renewed: %q", p.Warning)
	}
	// No window at all is the default one, as the record has always had it.
	if p := PlanTerm(0, false, 0, 0); p.Window != DefaultTTL {
		t.Errorf("no window = DefaultTTL: %+v", p)
	}
}

func TestInstalledTermLimitsReachThePlan(t *testing.T) {
	t.Cleanup(func() { SetDefaultTerms(0, 0) })
	if MaxTerm() != DefaultMaxTerm || MaxTotal() != DefaultMaxTotal {
		t.Fatalf("unset limits are the defaults: %s %s", MaxTerm(), MaxTotal())
	}
	SetDefaultTerms(2*time.Hour, 10*time.Hour)
	p := PlanTerm(3*time.Hour, false, 0, 0)
	if p.Cap != 2*time.Hour || p.MaxTotal != 10*time.Hour || p.Requested != 3*time.Hour {
		t.Fatalf("the installed limits are the plan's when none is given: %+v", p)
	}
	// An explicit limit wins over the installed one.
	if p := PlanTerm(3*time.Hour, false, 4*time.Hour, 0); p.Cap != 4*time.Hour || p.Requested != 0 {
		t.Fatalf("an explicit cap wins: %+v", p)
	}
	SetDefaultTerms(-5, -5)
	if MaxTerm() != DefaultMaxTerm || MaxTotal() != DefaultMaxTotal {
		t.Fatalf("a negative limit is the default, not a switch: %s %s", MaxTerm(), MaxTotal())
	}
}

func TestOverCapRequestIsRecordedOnTheLeaseAndTheWindowIsTheWholeRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		mgr  func(*testing.T) (*Manager, *time.Time)
		opts Options
	}{
		{"whole node", newTestManager, Options{}},
		{"card lease", scopedManager, Options{Devices: []string{card0}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, now := tc.mgr(t)
			opts := tc.opts
			opts.Reason, opts.TTL, opts.MaxTerm, opts.MaxTotal = "film", 20*time.Hour, 6*time.Hour, 48*time.Hour
			l, err := m.TryAcquire(ClassMedia, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = l.Release() }()
			info := m.Inspect()
			if got := info.ExpiresAt.Sub(info.AcquiredAt); got != 20*time.Hour {
				t.Fatalf("the declared window was shortened to %s, want the whole 20h request", got)
			}
			if info.Requested != 20*time.Hour || info.Term != 6*time.Hour || !info.HardEnd.Equal(now.Add(48*time.Hour)) {
				t.Fatalf("the record must say what was asked and what renews it: requested %s term %s hard end %s", info.Requested, info.Term, info.HardEnd)
			}
			if info.Expired {
				t.Fatal("a fresh lease is not expired")
			}
			// An in-cap request records no request at all.
			opts.TTL = 5 * time.Hour
			_ = l.Release()
			l2, err := m.TryAcquire(ClassMedia, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = l2.Release() }()
			if in := m.Inspect(); in.Requested != 0 || in.Term != 5*time.Hour {
				t.Fatalf("an in-cap request: requested %s term %s", in.Requested, in.Term)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The tick: nothing, one renewal, or the label
// ---------------------------------------------------------------------------

// termFixture is a lease whose owner process is up and whose progress file moves.
type termFixture struct {
	m     *Manager
	now   *time.Time
	base  time.Time
	l     *Lease
	prog  string
	owner func(pid int, up bool)
}

// newTermFixture takes a lease with a progress contract (a file that moves, a 30 minute
// stall window) unless opts already names one; newTermFixtureNP takes one without.
func newTermFixture(t *testing.T, scoped bool, opts Options) *termFixture {
	t.Helper()
	return buildTermFixture(t, scoped, opts, true)
}

func newTermFixtureNP(t *testing.T, scoped bool, opts Options) *termFixture {
	t.Helper()
	return buildTermFixture(t, scoped, opts, false)
}

func buildTermFixture(t *testing.T, scoped bool, opts Options, withProgress bool) *termFixture {
	t.Helper()
	owner := fakeProcs(t, termOwnerPID)
	var m *Manager
	var now *time.Time
	if scoped {
		m, now = scopedManager(t)
		opts.Devices = []string{card0}
	} else {
		m, now = newTestManager(t)
	}
	f := &termFixture{m: m, now: now, base: *now, owner: owner}
	if opts.Owner.IsZero() {
		opts.Owner = Owner{PID: termOwnerPID, StartMs: 4242}
	}
	if opts.TTL == 0 {
		opts.TTL = time.Hour
	}
	if withProgress && opts.ProgressFile == "" && opts.Stall == 0 {
		opts.ProgressFile, opts.Stall = progressFileAt(t, *now), 30*time.Minute
	}
	f.prog = opts.ProgressFile
	opts.Reason = "film"
	l, err := m.TryAcquire(ClassMedia, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	f.l = l
	return f
}

// at moves the clock to d after the acquisition, renews the heartbeat the way the holder's
// tick does, and touches the progress file (when it has one) so it reads as advancing.
func (f *termFixture) at(t *testing.T, d time.Duration, progressMoves bool) {
	t.Helper()
	clockAt(f.now, f.base, d)
	if progressMoves && f.prog != "" {
		touchProgress(t, f.prog, *f.now)
	}
	if err := f.l.Renew(); err != nil {
		t.Fatalf("renew at +%s: %v", d, err)
	}
}

func TestTermEndWithLiveOwnerAndProgressExtendsOnce(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		name := "whole node"
		if scoped {
			name = "card lease"
		}
		t.Run(name, func(t *testing.T) {
			f := newTermFixture(t, scoped, Options{})
			end0 := f.base.Add(time.Hour)

			// Inside the term: nothing happens, nothing is written.
			f.at(t, 30*time.Minute, true)
			before := recordSum(t, f.m, f.l)
			if r, err := f.l.AdvanceTerm(TermSignals{UtilWorking: neverUtil(t)}); err != nil || r.Outcome != TermNotDue {
				t.Fatalf("inside the term: %+v, %v", r, err)
			}
			if recordSum(t, f.m, f.l) != before {
				t.Fatal("a tick inside the term rewrote the record")
			}

			// The term ends with the owner alive and the progress file moving: ONE renewal, by one
			// term, from the old end. Utilisation is not consulted: progress already says it.
			f.at(t, time.Hour+10*time.Second, true)
			r, err := f.l.AdvanceTerm(TermSignals{UtilWorking: neverUtil(t)})
			if err != nil {
				t.Fatal(err)
			}
			if r.Outcome != TermExtended || !r.PrevEnd.Equal(end0) || !r.End.Equal(end0.Add(time.Hour)) {
				t.Fatalf("want one renewal of one term from %s, got %+v", end0, r)
			}
			rec := recordOfLease(t, f.m, f.l)
			if rec.ExpiresAtMs != end0.Add(time.Hour).UnixMilli() || rec.Expired || rec.ExpiredWhy != "" {
				t.Fatalf("record after the renewal: expires %d expired %v why %q", rec.ExpiresAtMs, rec.Expired, rec.ExpiredWhy)
			}
			// The same tick again, and a hundred more inside the new term, renew nothing more.
			for i := 0; i < 3; i++ {
				f.at(t, time.Hour+10*time.Second+time.Duration(i)*15*time.Second, true)
				if r, err := f.l.AdvanceTerm(TermSignals{UtilWorking: neverUtil(t)}); err != nil || r.Outcome != TermNotDue {
					t.Fatalf("a second tick in the same term: %+v, %v", r, err)
				}
			}
			if got := recordOfLease(t, f.m, f.l).ExpiresAtMs; got != end0.Add(time.Hour).UnixMilli() {
				t.Fatalf("the end moved by more than one term: %d", got)
			}
			// The next term end renews again, by one term again.
			f.at(t, 2*time.Hour+time.Second, true)
			r, _ = f.l.AdvanceTerm(TermSignals{UtilWorking: neverUtil(t)})
			if r.Outcome != TermExtended || !r.End.Equal(end0.Add(2*time.Hour)) {
				t.Fatalf("the second term: %+v", r)
			}
			if err := f.l.Check(); err != nil {
				t.Fatalf("a renewed lease is still ours: %v", err)
			}
		})
	}
}

func TestTermEndWithDeadOwnerSetsExpiredKeepsHeartbeat(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		name := "whole node"
		if scoped {
			name = "card lease"
		}
		t.Run(name, func(t *testing.T) {
			f := newTermFixtureNP(t, scoped, Options{})
			end0 := f.base.Add(time.Hour)
			f.owner(termOwnerPID, false) // the session that asked for it is gone

			f.at(t, time.Hour+10*time.Second, false)
			r, err := f.l.AdvanceTerm(TermSignals{UtilWorking: neverUtil(t)}) // a gone owner is not rescued by a busy card
			if err != nil {
				t.Fatal(err)
			}
			if r.Outcome != TermExpired || !r.End.Equal(end0) || !strings.Contains(r.Why, "owner") {
				t.Fatalf("a term that ends with its owner gone is labelled expired and its end is left alone: %+v", r)
			}
			rec := recordOfLease(t, f.m, f.l)
			if !rec.Expired || !strings.Contains(rec.ExpiredWhy, "owner") || rec.ExpiresAtMs != end0.UnixMilli() {
				t.Fatalf("the label and its sentence are on the record: %+v", rec)
			}
			info := f.m.Inspect()
			if !info.Held || !info.Expired || !strings.Contains(info.ExpiredWhy, "owner") {
				t.Fatalf("an expired lease is still HELD, and says it is expired: %+v", info)
			}

			// A label is not a release: the heartbeat goes on, the holder's own fence still passes,
			// and a second tick finds nothing new to write.
			sum := recordSum(t, f.m, f.l)
			f.at(t, time.Hour+25*time.Second, false)
			if err := f.l.Check(); err != nil {
				t.Fatalf("an expired lease must still pass its holder's fence (or the job under it is killed): %v", err)
			}
			if hb := f.m.lastHeartbeat(&rec); hb != f.now.UnixMilli() {
				t.Fatalf("the heartbeat must go on: %d, want %d", hb, f.now.UnixMilli())
			}
			if r, _ := f.l.AdvanceTerm(TermSignals{}); r.Outcome != TermStillExpired {
				t.Fatalf("a second tick on an expired lease: %+v", r)
			}
			if recordSum(t, f.m, f.l) != sum {
				t.Fatal("a tick that changed nothing rewrote the record")
			}

			// The label clears when the term becomes renewable again: the owner came back and the
			// cards are at work. It is information about the lease, not a one-way door.
			f.owner(termOwnerPID, true)
			f.at(t, time.Hour+40*time.Second, false)
			r, err = f.l.AdvanceTerm(TermSignals{UtilWorking: func() bool { return true }})
			if err != nil || r.Outcome != TermExtended || !r.End.Equal(end0.Add(time.Hour)) {
				t.Fatalf("an owner that is back and a busy card renew the term: %+v, %v", r, err)
			}
			if rec := recordOfLease(t, f.m, f.l); rec.Expired || rec.ExpiredWhy != "" {
				t.Fatalf("the label is cleared on renewal: %+v", rec)
			}
		})
	}
}

func TestExpiredLeaseIsNotReclaimableAndNeverTwoJobsOnOneCard(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		name := "whole node"
		if scoped {
			name = "card lease"
		}
		t.Run(name, func(t *testing.T) {
			f := newTermFixtureNP(t, scoped, Options{})
			f.owner(termOwnerPID, false)
			f.at(t, time.Hour+10*time.Second, false)
			if r, _ := f.l.AdvanceTerm(TermSignals{}); r.Outcome != TermExpired {
				t.Fatalf("setup: %+v", r)
			}
			// Hours pass; the holder renews its heartbeat every tick as the wrapper does.
			for _, d := range []time.Duration{2 * time.Hour, 5 * time.Hour, 11 * time.Hour} {
				f.at(t, d, false)
				// ANOTHER process, alive, wanting the same card(s): it must queue, not take them.
				other := asPID(f.m, os.Getpid())
				if _, err := other.TryAcquire(ClassMedia, Options{Reason: "second job", Devices: f.l.Devices(), TTL: time.Hour}); err == nil {
					t.Fatalf("at +%s a second job was granted the card of an expired lease that is still heartbeating", d)
				} else if _, ok := err.(*ErrHeld); !ok {
					t.Fatalf("at +%s: want a held refusal, got %v", d, err)
				}
				if f.m.reclaimable(metaOf(t, f.m, f.l), f.m.now()) {
					t.Fatalf("at +%s an expired lease with a live heartbeat was judged reclaimable", d)
				}
				if info := f.m.Inspect(); !info.Held || info.Epoch != f.l.Epoch() {
					t.Fatalf("at +%s the lease is no longer the held one: %+v", d, info)
				}
				if err := f.l.Check(); err != nil {
					t.Fatalf("at +%s the holder was fenced out: %v", d, err)
				}
			}
			// The label adds no protection and no exposure: a holder that stops heartbeating is
			// reclaimed by the same rule as before (the reclaim conjunction is untouched).
			clockAt(f.now, f.base, 11*time.Hour+10*time.Minute)
			if !f.m.reclaimable(metaOf(t, f.m, f.l), f.m.now()) {
				t.Fatal("an expired lease whose holder stopped heartbeating follows the old reclaim rule")
			}
		})
	}
}

// metaOf reads the record the way a reader judges it.
func metaOf(t *testing.T, m *Manager, l *Lease) *Meta {
	t.Helper()
	rec := recordOfLease(t, m, l)
	return &rec
}

func TestMaxTotalOverdueEvenWithLiveOwner(t *testing.T) {
	// Owner alive, progress advancing, every term: the lease is renewed up to the maximum
	// total from acquisition and not one minute past it.
	f := newTermFixture(t, true, Options{TTL: time.Hour, MaxTotal: 3 * time.Hour})
	hard := f.base.Add(3 * time.Hour)
	if got := f.m.Inspect().HardEnd; !got.Equal(hard) {
		t.Fatalf("hard end = %s, want %s", got, hard)
	}
	for i, step := range []time.Duration{time.Hour + time.Second, 2*time.Hour + time.Second} {
		f.at(t, step, true)
		r, err := f.l.AdvanceTerm(TermSignals{})
		if err != nil || r.Outcome != TermExtended {
			t.Fatalf("term %d: %+v, %v", i+1, r, err)
		}
	}
	if end := f.m.Inspect().ExpiresAt; !end.Equal(hard) {
		t.Fatalf("the last renewal must end at the hard end, got %s", end)
	}
	f.at(t, 3*time.Hour+time.Second, true) // owner alive, progress advancing, still
	r, err := f.l.AdvanceTerm(TermSignals{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome != TermExpired || !strings.Contains(r.Why, "maximum total") {
		t.Fatalf("at the maximum total the lease is overdue however healthy it looks: %+v", r)
	}
	if end := f.m.Inspect().ExpiresAt; !end.Equal(hard) {
		t.Fatalf("the end must not move past the hard end: %s", end)
	}
	if err := f.l.Check(); err != nil {
		t.Fatalf("overdue is a label, not a fence-out: %v", err)
	}
}

func TestExtensionIsClippedToTheMaximumTotal(t *testing.T) {
	// A 2h term with a 3h maximum: the first renewal gets the 1h that is left, not a second 2h.
	f := newTermFixture(t, false, Options{TTL: 2 * time.Hour, MaxTotal: 3 * time.Hour})
	f.at(t, 2*time.Hour+time.Second, true)
	r, _ := f.l.AdvanceTerm(TermSignals{})
	if r.Outcome != TermExtended || !r.End.Equal(f.base.Add(3*time.Hour)) {
		t.Fatalf("the renewal is clipped to the hard end: %+v", r)
	}
	// A holder that was suspended past the hard end and wakes up has nothing left to renew.
	g := newTermFixture(t, false, Options{TTL: time.Hour, MaxTotal: 2 * time.Hour})
	g.at(t, 5*time.Hour, true)
	if r, _ := g.l.AdvanceTerm(TermSignals{}); r.Outcome != TermExpired || !strings.Contains(r.Why, "maximum total") {
		t.Fatalf("a hard end already in the past renews nothing: %+v", r)
	}
}

func TestIdleLiveOwnerWithoutProgressDoesNotRenew(t *testing.T) {
	f := newTermFixtureNP(t, false, Options{})
	f.at(t, time.Hour+time.Second, false)
	asked := 0
	r, err := f.l.AdvanceTerm(TermSignals{UtilWorking: func() bool { asked++; return false }})
	if err != nil {
		t.Fatal(err)
	}
	if asked != 1 {
		t.Fatalf("an attended lease with a live owner and no progress contract is the one case that reads the cards: asked %d times", asked)
	}
	if r.Outcome != TermExpired || !strings.Contains(r.Why, "owner is still there") {
		t.Fatalf("a live owner with nothing moving and nothing running does not renew the term: %+v", r)
	}
	if !f.m.Inspect().Expired {
		t.Fatal("and the lease says so")
	}
	// Without the signal at all (nvidia-smi absent) the answer is the same: unknown is not work.
	g := newTermFixtureNP(t, false, Options{})
	g.at(t, time.Hour+time.Second, false)
	if r, _ := g.l.AdvanceTerm(TermSignals{}); r.Outcome != TermExpired {
		t.Fatalf("no utilisation reading must not renew: %+v", r)
	}
}

func TestLiveOwnerWithBusyCardsRenews(t *testing.T) {
	// The plan's second leg: an attended lease with no progress contract is judged by whether
	// its cards are working.
	f := newTermFixtureNP(t, true, Options{})
	f.at(t, time.Hour+time.Second, false)
	r, err := f.l.AdvanceTerm(TermSignals{UtilWorking: func() bool { return true }})
	if err != nil || r.Outcome != TermExtended || !strings.Contains(r.Why, "cards") {
		t.Fatalf("a live owner and busy cards renew: %+v, %v", r, err)
	}
}

func TestUnattendedLeaseRenewsOnProgressAloneAndNeverOnUtilisation(t *testing.T) {
	// Unattended: nobody is expected at the desk, so the owner's presence says nothing and
	// neither does a busy card; only the progress file does.
	f := newTermFixture(t, true, Options{Unattended: true, TTL: time.Hour})
	f.owner(termOwnerPID, false)
	f.at(t, time.Hour+time.Second, true) // the progress file moved
	r, err := f.l.AdvanceTerm(TermSignals{UtilWorking: neverUtil(t)})
	if err != nil || r.Outcome != TermExtended || !strings.Contains(r.Why, "progress") {
		t.Fatalf("an unattended lease whose progress advances renews, owner gone or not: %+v, %v", r, err)
	}
	// Its progress stops: owner alive, cards busy, and it still does not renew.
	f.owner(termOwnerPID, true)
	clockAt(f.now, f.base, 2*time.Hour+10*time.Second)
	if err := f.l.Renew(); err != nil {
		t.Fatal(err)
	}
	touchProgress(t, f.prog, f.base.Add(time.Hour)) // last moved an hour ago; the stall window is 30m
	r, _ = f.l.AdvanceTerm(TermSignals{UtilWorking: neverUtil(t)})
	if r.Outcome != TermExpired || !strings.Contains(r.Why, "progress") {
		t.Fatalf("an unattended lease whose progress stalled expires whatever the cards do: %+v", r)
	}
}

func TestMissingProgressFileIsNotProgress(t *testing.T) {
	// Unknown is never advancing: a contract whose file does not exist renews nothing on its own.
	f := newTermFixture(t, false, Options{Unattended: true, TTL: time.Hour})
	if err := os.Remove(f.prog); err != nil {
		t.Fatal(err)
	}
	f.at(t, time.Hour+time.Second, false)
	if r, _ := f.l.AdvanceTerm(TermSignals{UtilWorking: neverUtil(t)}); r.Outcome != TermExpired {
		t.Fatalf("a missing progress file is not progress: %+v", r)
	}
}

func TestExtensionRestampsOnlyTheExtendedLeaseAndWritesNoUmbrella(t *testing.T) {
	// Plan P9 said the umbrella meta.json is restamped on an extension. P2 dropped the
	// umbrella (devices.go), so there is nothing to restamp: an extension rewrites the lease's
	// OWN record, leaves every sibling's record byte for byte as it was, and a card lease
	// never causes a meta.json to appear.
	owner := fakeProcs(t, termOwnerPID)
	_ = owner
	m, now := scopedManager(t)
	base := *now
	progA, progB := progressFileAt(t, base), progressFileAt(t, base)
	a, err := m.TryAcquire(ClassMedia, Options{Reason: "a", Devices: []string{card0}, TTL: time.Hour, Owner: Owner{PID: termOwnerPID, StartMs: 4242}, ProgressFile: progA, Stall: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.TryAcquire(ClassMedia, Options{Reason: "b", Devices: []string{card1}, TTL: 5 * time.Hour, Owner: Owner{PID: termOwnerPID, StartMs: 4242}, ProgressFile: progB, Stall: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Release(); _ = b.Release() }()
	sumB := recordSum(t, m, b)
	sumA := recordSum(t, m, a)

	clockAt(now, base, time.Hour+time.Second)
	touchProgress(t, progA, *now)
	touchProgress(t, progB, *now)
	if err := a.Renew(); err != nil {
		t.Fatal(err)
	}
	if err := b.Renew(); err != nil {
		t.Fatal(err)
	}
	r, err := a.AdvanceTerm(TermSignals{})
	if err != nil || r.Outcome != TermExtended {
		t.Fatalf("a: %+v, %v", r, err)
	}
	if recordSum(t, m, a) == sumA {
		t.Fatal("the extended lease's record was not rewritten")
	}
	if recordSum(t, m, b) != sumB {
		t.Fatal("an extension of lease a rewrote lease b's record")
	}
	if rb, _ := b.AdvanceTerm(TermSignals{}); rb.Outcome != TermNotDue {
		t.Fatalf("b is inside its own term: %+v", rb)
	}
	if _, err := os.Stat(m.metaPath()); !os.IsNotExist(err) {
		t.Fatalf("a card lease's extension must not create a legacy meta.json umbrella: %v", err)
	}
	for name, l := range map[string]*Lease{"a": a, "b": b} {
		if err := l.Check(); err != nil {
			t.Errorf("lease %s fenced out by its sibling's extension: %v", name, err)
		}
	}

	// A whole-node lease rewrites meta.json in place and keeps its epoch.
	w, wnow := newTestManager(t)
	wbase := *wnow
	wl, err := w.TryAcquire(ClassMedia, Options{Reason: "w", TTL: time.Hour, Owner: Owner{PID: termOwnerPID, StartMs: 4242}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = wl.Release() }()
	clockAt(wnow, wbase, time.Hour+time.Second)
	if r, err := wl.AdvanceTerm(TermSignals{UtilWorking: func() bool { return true }}); err != nil || r.Outcome != TermExtended {
		t.Fatalf("whole node: %+v, %v", r, err)
	}
	if rec := recordOfLease(t, w, wl); rec.Epoch != wl.Epoch() || rec.ExpiresAtMs != wbase.Add(2*time.Hour).UnixMilli() {
		t.Fatalf("whole-node record after the renewal: %+v", rec)
	}
	if err := wl.Check(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentTicksExtendOnce(t *testing.T) {
	f := newTermFixture(t, true, Options{})
	f.at(t, time.Hour+time.Second, true)
	var wg sync.WaitGroup
	results := make([]TermResult, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := f.l.AdvanceTerm(TermSignals{})
			if err != nil {
				t.Errorf("tick %d: %v", i, err)
			}
			results[i] = r
		}(i)
	}
	wg.Wait()
	extended := 0
	for _, r := range results {
		if r.Outcome == TermExtended {
			extended++
		}
	}
	if extended != 1 {
		t.Fatalf("%d ticks of one term end renewed it %d times, want exactly once: %+v", len(results), extended, results)
	}
	if got := recordOfLease(t, f.m, f.l).ExpiresAtMs; got != f.base.Add(2*time.Hour).UnixMilli() {
		t.Fatalf("end = %d, want one term past the old end", got)
	}
}

func TestLegacyRecordWithoutTermFieldsIsOneTermOfItsWindow(t *testing.T) {
	// A record written before terms carries none of the fields. Its term is its declared
	// window (under the cap) and its hard end the installed maximum from its acquisition.
	fakeProcs(t, termOwnerPID)
	m, now := newTestManager(t)
	base := *now
	if err := os.MkdirAll(m.leaseDir(), 0o777); err != nil {
		t.Fatal(err)
	}
	rec := `{"epoch":7,"class":"media","holder":{"pid":` + strconv.Itoa(os.Getpid()) + `,"start_time_ms":4242},` +
		`"owner":{"pid":` + strconv.Itoa(termOwnerPID) + `,"start_ms":4242},` +
		`"acquired_at_ms":` + strconv.FormatInt(base.UnixMilli(), 10) + `,"expires_at_ms":` + strconv.FormatInt(base.Add(90*time.Minute).UnixMilli(), 10) +
		`,"renewed_at_ms":` + strconv.FormatInt(base.UnixMilli(), 10) + `}`
	if err := os.WriteFile(m.metaPath(), []byte(rec), 0o666); err != nil {
		t.Fatal(err)
	}
	clockAt(now, base, 91*time.Minute)
	r, err := m.AdvanceTerm(7, TermSignals{UtilWorking: func() bool { return true }})
	if err != nil || r.Outcome != TermExtended || !r.End.Equal(base.Add(3*time.Hour)) {
		t.Fatalf("a record with no term fields renews by its own window: %+v, %v", r, err)
	}
	// And a record with no window at all has nothing to end.
	if err := os.WriteFile(m.metaPath(), []byte(strings.Replace(rec, `"expires_at_ms":`+strconv.FormatInt(base.Add(90*time.Minute).UnixMilli(), 10), `"expires_at_ms":0`, 1)), 0o666); err != nil {
		t.Fatal(err)
	}
	if r, err := m.AdvanceTerm(7, TermSignals{}); err != nil || r.Outcome != TermNotDue {
		t.Fatalf("no declared end, no term end: %+v, %v", r, err)
	}
}

func TestAdvanceTermRefusesALeaseThatIsNotOurs(t *testing.T) {
	f := newTermFixture(t, true, Options{})
	if _, err := f.m.AdvanceTerm(f.l.Epoch()+99, TermSignals{}); err == nil {
		t.Fatal("a tick for an epoch that is not a live lease here must be an error, not a silent no-op")
	}
	_ = f.l.Release()
	if _, err := f.l.AdvanceTerm(TermSignals{}); err == nil {
		t.Fatal("a released lease has no term")
	}
}

// ---------------------------------------------------------------------------
// The label is invisible to every fence, old and new
// ---------------------------------------------------------------------------

func TestExpiredLabelNeverChangesTheFenceState(t *testing.T) {
	// A record's State is the word render/gpu-lock.mjs and checkV2 fence on, and a reader built
	// before terms fences out anything but "active". Expiry must not touch it.
	for _, scoped := range []bool{false, true} {
		name := "whole node"
		if scoped {
			name = "card lease"
		}
		t.Run(name, func(t *testing.T) {
			f := newTermFixtureNP(t, scoped, Options{})
			f.owner(termOwnerPID, false)
			f.at(t, time.Hour+time.Second, false)
			if r, _ := f.l.AdvanceTerm(TermSignals{}); r.Outcome != TermExpired {
				t.Fatalf("setup: %+v", r)
			}
			raw, err := os.ReadFile(recordPath(f.m, f.l))
			if err != nil {
				t.Fatal(err)
			}
			var generic map[string]any
			if err := json.Unmarshal(raw, &generic); err != nil {
				t.Fatal(err)
			}
			wantState := ""
			if scoped {
				wantState = "active"
			}
			if got, _ := generic["state"].(string); got != wantState {
				t.Fatalf("state = %q, want %q (the fence's word is unchanged by expiry): %s", got, wantState, raw)
			}
			if generic["expired"] != true {
				t.Fatalf("the label is its own key: %s", raw)
			}
			// The package's own per-epoch fence for a process that holds no Lease object (an inherited
			// render child) agrees.
			if !f.m.EpochIsCurrent(f.l.Epoch()) {
				t.Fatal("an expired lease must still be current for the render child running under it")
			}
		})
	}
}

func TestOldReaderStillSeesAnExpiredWholeNodeLeaseAsHeld(t *testing.T) {
	// The pre-terms reader (kept as a fixture): an expired, heartbeating lease is held and not
	// reclaimable to it, as it was before the label existed.
	f := newTermFixtureNP(t, false, Options{})
	f.owner(termOwnerPID, false)
	f.at(t, time.Hour+time.Second, false)
	if r, _ := f.l.AdvanceTerm(TermSignals{}); r.Outcome != TermExpired {
		t.Fatalf("setup: %+v", r)
	}
	f.at(t, 4*time.Hour, false)
	info, _, reclaimable := legacyInspect(f.m.leaseDir(), f.m.now(), f.m.heartbeatTTL, f.m.procStart)
	if !info.Held || reclaimable {
		t.Fatalf("an old reader must read an expired, heartbeating lease as held: held=%v reclaimable=%v", info.Held, reclaimable)
	}
	if err := legacyCheck(f.m.leaseDir(), f.l.Epoch()); err != nil {
		t.Fatalf("an old reader's fence on the expired lease: %v", err)
	}
	if _, err := legacyTryAcquire(t, f.m, ClassMedia); err == nil {
		t.Fatal("an old writer must queue behind an expired lease that is still heartbeating")
	}
}
