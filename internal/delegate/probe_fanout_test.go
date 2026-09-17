// The fleet health probe: concurrent, memoised, negative-cached, and bounded
// inside the capacity wait (W-02/S-10 of
// plans/2026-09-17-harness-scheduling-diagnosis.md), plus the two placement
// reads that ignored a node's own ceiling (W-15a/W-15b, S-14/S-12).
//
// Measured motivation for the whole file: 46 live delegation rows kept work on
// the local seat because ONE remote's health probe timed out — and in 41 of
// them that remote had zero jobs in flight. The probe was serial, uncached, and
// re-run per subtask, per re-placement, per retry and per capacity-wait tick.

package delegate

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// slowNode is a health-only fixture: it answers /fleet/health after delay and
// counts how many times it was asked. Nothing dispatches to it — these tests
// exercise the PROBE, not the job wire.
func slowNode(t *testing.T, id string, delay time.Duration) (*fakeNode, string) {
	t.Helper()
	f := &fakeNode{t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: id, healthDelay: delay}
	return f, f.server().URL
}

// TestFetchViewsProbesEveryRemoteConcurrently (W-02a). Three remotes, each
// spending 1.5 s on health: serially that is 4.5 s of pure waiting on the
// critical path of EVERY subtask, and the fan-out multiplies it. Concurrently
// it is one remote's latency.
//
// Why all three sleep and not just one: with a single slow remote the serial
// loop and the concurrent fan-out cost the same wall, so the assertion could
// never fail on the defect it exists to catch.
//
// The parallel views/bases/probeErrs contract is asserted too — the pairing is
// positional (a NodeView carries no base), so a fan-out that reassembled in
// COMPLETION order rather than configured order would hand every caller the
// wrong dial address for the node it picked.
func TestFetchViewsProbesEveryRemoteConcurrently(t *testing.T) {
	const delay = 1500 * time.Millisecond
	_, aURL := slowNode(t, "node-a", delay)
	_, bURL := slowNode(t, "node-b", delay)
	_, cURL := slowNode(t, "node-c", delay)
	r := &runner{cfg: testCfg(t), remotes: []string{aURL, bURL, cURL}}

	start := time.Now()
	views, bases, probeErrs := r.fetchViews(t.Context())
	elapsed := time.Since(start)

	if len(probeErrs) != 0 {
		t.Fatalf("probeErrs = %v, want none — every stub answers", probeErrs)
	}
	if got, want := len(views), 3; got != want {
		t.Fatalf("views = %d, want %d", got, want)
	}
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("fetchViews took %s for three %s remotes — serial, not concurrent (concurrent is ~%s)",
			elapsed.Round(time.Millisecond), delay, delay)
	}
	wantBases := []string{aURL, bURL, cURL}
	wantIDs := []string{"node-a", "node-b", "node-c"}
	for i := range wantBases {
		if bases[i] != wantBases[i] {
			t.Fatalf("bases = %v, want the CONFIGURED order %v", bases, wantBases)
		}
		if views[i].NodeID != wantIDs[i] {
			t.Fatalf("views[%d] = %q, want %q — views and bases must stay parallel", i, views[i].NodeID, wantIDs[i])
		}
	}
}

// TestFetchViewsMemoisesOneSnapshotWithinARun (W-02b): the sibling subtasks of
// ONE Run share one fleet snapshot instead of each paying its own probe.
// route=spread already amortised this with a single probe at run start; auto,
// remote, every re-placement and every retry did not.
//
// The memo is per RUNNER, which is per Run: a second Run must probe again, or a
// long-lived delegator would place on a snapshot from a previous call.
func TestFetchViewsMemoisesOneSnapshotWithinARun(t *testing.T) {
	node, url := slowNode(t, "node-a", 0)
	r := &runner{cfg: testCfg(t), remotes: []string{url}}

	// One priming call, then the siblings: concurrent, the way a fan-out's
	// subtasks actually arrive.
	first, _, _ := r.fetchViews(t.Context())
	if len(first) != 1 {
		t.Fatalf("views = %d, want 1", len(first))
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			views, bases, _ := r.fetchViews(t.Context())
			if len(views) != 1 || len(bases) != 1 || views[0].NodeID != "node-a" {
				t.Errorf("memoised snapshot = %v / %v, want the one node", views, bases)
			}
		}()
	}
	wg.Wait()
	if got := node.healths.Load(); got != 1 {
		t.Fatalf("the node served %d health probes for one run's siblings, want 1 — the probe is not memoised", got)
	}

	// A SECOND Run gets its own snapshot.
	r2 := &runner{cfg: testCfg(t), remotes: []string{url}}
	if _, _, _ = r2.fetchViews(t.Context()); node.healths.Load() != 2 {
		t.Fatalf("a new run brought the total to %d health probes, want 2 — the memo must not outlive its Run", node.healths.Load())
	}
}

// TestRetrySeatBusyReadsTheNodeCeiling (W-15a, S-14; register D-46 shipped the
// RULE and not the threshold). "The retry seat is already generating for
// another job" was implemented as jobs_running > 0, so a four-worker node with
// ONE job running refused a cross-seat retry it had three free workers for. The
// predicate is the ceiling-aware one the gate already owns:
// !provablyStartsNow(view).
func TestRetrySeatBusyReadsTheNodeCeiling(t *testing.T) {
	// queue_depth is set on both fixtures because a node publishes it as its
	// non-terminal job count: {queue_depth 0, jobs_running 4} is not a shape
	// any node emits, and provablyStartsNow reads queue_depth 0 as proof that
	// the next job starts at once.
	room := &fakeNode{t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: "node-room",
		maxConcurrentJobs: 4, jobsRunning: 1, jobsQueued: 0, queueDepth: 1}
	roomURL := room.server().URL
	full := &fakeNode{t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: "node-full",
		maxConcurrentJobs: 4, jobsRunning: 4, jobsQueued: 0, queueDepth: 4}
	fullURL := full.server().URL

	r := &runner{cfg: testCfg(t)}
	if busy, why := r.retrySeatBusy(t.Context(), placement{base: roomURL}); busy {
		t.Fatalf("a node at 1 of 4 workers reported busy (%q) — three workers are free, the retry must land", why)
	}
	busy, why := r.retrySeatBusy(t.Context(), placement{base: fullURL})
	if !busy {
		t.Fatal("a node at 4 of 4 workers must be busy: the retry would only queue behind a full node")
	}
	if !strings.Contains(why, "jobs_running 4") || !strings.Contains(why, "fresh health") {
		t.Fatalf("busy note = %q, want the node's own numbers and the source", why)
	}
}

// TestDealSpreadKeysTheCycleOnTheDialBase (W-15b, S-12): the per-cycle `dealt`
// set keyed on NodeID while every other exclusion in run.go keys on the dial
// base, so two remotes advertising an EMPTY (or identical) node_id collapsed
// into one entry — the second remote of the cycle found its key already taken,
// the cycle was reshuffled, and one seat took both subtasks while the other
// idled.
//
// The fixture is the shape that makes the collapse visible: two seats of
// different sizes, both with node_id "", and mechanical contracts — the fit
// score sends every mechanical slot to the SMALLEST adequate seat, so after a
// reshuffle it wins again and the roomy seat is never dealt.
func TestDealSpreadKeysTheCycleOnTheDialBase(t *testing.T) {
	const aBase, bBase = "http://node-a:18811", "http://node-b:18811"
	r := &runner{
		spreadViews: []NodeView{
			{NodeID: "", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 131072},
			{NodeID: "", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 32768},
		},
		spreadBases: []string{aBase, bBase},
	}
	goals := repeatGoal(fitMechGoal, 4)
	contracts := make([]core.AgentContract, len(goals))
	for i, g := range goals {
		contracts[i] = fitSubtask(g, 100).Contract
	}
	dealtTo := map[string]int{}
	for _, sl := range r.dealSpread(contracts, fitLocal()) {
		dealtTo[sl.base]++
	}
	for _, base := range []string{aBase, bBase} {
		if dealtTo[base] == 0 {
			t.Fatalf("deal = %v: %s was never dealt a subtask — two remotes with the same node_id collapsed into one cycle entry", dealtTo, base)
		}
	}
}

// deadListener accepts a TCP connection and closes it at once: the probe gets a
// transport failure with no HTTP answer at all — a dead node's shape — and the
// listener COUNTS the dials, which is how a test proves a probe did NOT happen.
func deadListener(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var dials atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			dials.Add(1)
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return "http://" + ln.Addr().String(), &dials
}

// blackHole accepts a connection and then never answers, so the probe blocks
// until its own deadline. That is the shape a node behind a dropped route has,
// and it is the only one that can demonstrate a BOUND — a refused dial fails
// instantly and would prove nothing.
func blackHole(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var dials atomic.Int64
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			dials.Add(1)
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	return "http://" + ln.Addr().String(), &dials
}

// shortProbeTimeout compresses the per-base health bound for one test.
func shortProbeTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := fetchNodeViewTimeout
	fetchNodeViewTimeout = d
	t.Cleanup(func() { fetchNodeViewTimeout = old })
}

// withNegativeProbeCache sets the negative-cache window for one test; 0 is off.
func withNegativeProbeCache(t *testing.T, d time.Duration) {
	t.Helper()
	old := probeNegativeTTL
	probeNegativeTTL = d
	t.Cleanup(func() { probeNegativeTTL = old })
}

// TestFetchViewsNegativeCachesADeadBase (W-02c). A base that failed at the
// TRANSPORT is not re-dialled for probeNegativeTTL — the measured defect is a
// fan-out paying fetchNodeViewTimeout per subtask for the same dead socket —
// and its reason is REPLAYED into probeErrs, because a base that silently
// disappeared from the errors would read as a node nobody ever configured.
// A healthy sibling is untouched: the cache is per base, never per fleet.
func TestFetchViewsNegativeCachesADeadBase(t *testing.T) {
	noProbeMemo(t) // the memo would answer the second call before the cache is reached
	deadURL, dials := deadListener(t)
	good, goodURL := slowNode(t, "node-good", 0)
	r := &runner{cfg: testCfg(t), remotes: []string{deadURL, goodURL}}

	views, bases, errs := r.fetchViews(t.Context())
	if len(views) != 1 || len(bases) != 1 || bases[0] != goodURL {
		t.Fatalf("first probe: views/bases = %v / %v, want only the healthy node", views, bases)
	}
	if len(errs) != 1 || !strings.Contains(errs[0], deadURL) {
		t.Fatalf("first probe: probeErrs = %v, want one error naming %s", errs, deadURL)
	}
	if strings.Contains(errs[0], "not re-dialled") {
		t.Fatalf("first probe reported a CACHED reason (%q) — the first probe must be a real dial", errs[0])
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dead base was dialled %d times on the first probe, want 1", got)
	}

	_, _, errs = r.fetchViews(t.Context())
	if got := dials.Load(); got != 1 {
		t.Fatalf("dead base was dialled %d times over two probes — it is re-dialled inside the negative window", got)
	}
	if len(errs) != 1 || !strings.Contains(errs[0], deadURL) || !strings.Contains(errs[0], "not re-dialled") {
		t.Fatalf("probeErrs = %v, want the cached reason still naming %s so the placement note can name it too", errs, deadURL)
	}
	if got := good.healths.Load(); got != 2 {
		t.Fatalf("the healthy node served %d probes, want 2 — the negative cache is per base, not per fleet", got)
	}

	// The window EXPIRES: a node that reboots inside a long Run is picked up
	// again rather than written off for the rest of it.
	withNegativeProbeCache(t, 0)
	if _, _, errs = r.fetchViews(t.Context()); dials.Load() != 2 {
		t.Fatalf("dead base was dialled %d times after the window expired, want 2 (%v)", dials.Load(), errs)
	}
}

// TestCapacityWaitTickIsBoundedByTwiceThePollInterval (W-02d, S-10). The
// capacity wait re-reads fleet health every placementPollInterval and passed
// the RAW run context to that read, while every other call site wraps it in a
// remaining-budget context. One black-holed remote therefore stalled each tick
// for fetchNodeViewTimeout — the wait's whole TTL could be spent inside a
// single tick's probe, and the node that DID free was never re-asked.
//
// The fixture isolates the bound deliberately: the memo is off (compressWait)
// and so is the negative cache, so nothing but the per-tick context can keep
// the tick short. fetchNodeViewTimeout is compressed to 2 s — longer than the
// 200 ms tick bound, shorter than a test's patience — and the wait TTL is 2 s,
// so an unbounded tick spends the whole TTL on its first probe and the run
// ends as a capacity defer.
func TestCapacityWaitTickIsBoundedByTwiceThePollInterval(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 100*time.Millisecond, 0)
	withNegativeProbeCache(t, 0)
	shortProbeTimeout(t, 2*time.Second)

	blackURL, _ := blackHole(t)
	node, url := acceptingNode(t, "node-busy", "qube after the wait", func(f *fakeNode) {
		f.dispatchHook = freesAfter(2, http.StatusServiceUnavailable)
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 2

	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{blackURL, url}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 1 {
		t.Fatalf("summary = %+v (%q): the wait never re-asked the node that freed — a black-holed remote ate the tick",
			sum, results[0].Result.Reason)
	}
	if got := node.dispatches.Load(); got < 3 {
		t.Fatalf("node saw %d dispatches, want the wait to have re-asked it (>= 3)", got)
	}
	// The wait itself has to be a small share of its own TTL: an unbounded tick
	// would spend ~2 s of a 2 s TTL inside one probe.
	if pr := results[0]; pr.CapacityWaitSec > 1.0 {
		t.Fatalf("capacity_wait_sec = %.2f of a 2 s TTL, want well under it — the per-tick probe is not bounded", pr.CapacityWaitSec)
	}
}

// TestFixedSleepsAreJittered: every dispatcher in the fleet sleeps on the same
// three constants (pollEvery, placementPollInterval, refusalCooldown), so K
// sessions started within a second of each other re-read health, re-dispatch
// and re-ask a refusing node in lockstep for the whole run. Each sleep is now
// randomised by +/-20 %, which spreads them apart within a few ticks and leaves
// the cadence's mean alone.
//
// The clamp is the other half and the one with teeth: jitter de-synchronises,
// it never licenses a sleep to overshoot the deadline it lives under.
func TestFixedSleepsAreJittered(t *testing.T) {
	const base = time.Second
	lo, hi := time.Duration(0.8*float64(base)), time.Duration(1.2*float64(base))
	seen := map[time.Duration]int{}
	for i := 0; i < 200; i++ {
		d := jittered(base)
		if d < lo || d > hi {
			t.Fatalf("jittered(%s) = %s, want it inside [%s, %s]", base, d, lo, hi)
		}
		seen[d]++
	}
	if len(seen) < 2 {
		t.Fatalf("200 samples produced %d distinct duration(s) — the sleep is not jittered at all", len(seen))
	}

	// A compressed clock stays compressed: a test that set an interval to zero
	// means zero, and jitter must not manufacture a sleep out of it.
	if got := jittered(0); got != 0 {
		t.Fatalf("jittered(0) = %s, want 0 — a zeroed test clock must stay zero", got)
	}

	// The clamp: with less left than the jittered sleep, the sleep is what is
	// left — never one millisecond past the deadline.
	if got := jitteredWithin(base, 10*time.Millisecond); got != 10*time.Millisecond {
		t.Fatalf("jitteredWithin(%s, 10ms) = %s, want 10ms — jitter must not run past its deadline", base, got)
	}
	// With no deadline to protect, the jittered value stands.
	if got := jitteredWithin(base, 0); got < lo || got > hi {
		t.Fatalf("jitteredWithin(%s, 0) = %s, want it inside [%s, %s]", base, got, lo, hi)
	}
}

// TestProbeTickBoundIsTheSmallerOfTwoTicksAndWhatIsLeft pins the arithmetic the
// capacity wait's per-tick probe context is built from.
func TestProbeTickBoundIsTheSmallerOfTwoTicksAndWhatIsLeft(t *testing.T) {
	compressWait(t, 100*time.Millisecond, 0)
	if got, want := probeTickBound(time.Now().Add(time.Hour)), 200*time.Millisecond; got != want {
		t.Fatalf("bound with an hour left = %s, want %s (two ticks)", got, want)
	}
	if got := probeTickBound(time.Now().Add(30 * time.Millisecond)); got > 30*time.Millisecond || got <= 0 {
		t.Fatalf("bound with 30ms left = %s, want what is left — a probe may not outlive the wait", got)
	}
	if got := probeTickBound(time.Now().Add(-time.Second)); got <= 0 {
		t.Fatalf("bound past the deadline = %s, want a positive value — a zero-length context cancels the probe before it is sent", got)
	}
}
