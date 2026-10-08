package delegate

// A health read taken after a refusal that proves a free worker lifts the node's Retry-After
// cooldown (ADR 0073).
//
// A 503's Retry-After is the node's own estimate, at the moment it refused, of when a worker frees. The
// capacity wait re-reads the node's health every tick and kept the node out until the hint ended all the
// same, so a node that drained a minute into a 300 s hint stayed unused for the rest of the wait while
// the subtask went on to defer. Now a read taken AFTER the refusal that PROVES a free worker (a
// published ceiling, fewer jobs running than it, none queued, not saturated) lifts it. Anything less
// proves nothing, and the cooldown stands; and a node that refuses again after a lift is asked early no
// more this run, so a node whose counters do not predict its refusals costs one ask, not one per tick.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestFreeWorkerProvenNeedsAPublishedCeiling: "proves a free worker" is positive evidence only. A
// node whose queue is empty because it publishes no counters proves nothing about its workers, which
// is the difference from provablyStartsNow.
func TestFreeWorkerProvenNeedsAPublishedCeiling(t *testing.T) {
	cases := []struct {
		name string
		v    NodeView
		want bool
	}{
		{"a published ceiling, a worker free, nothing queued", NodeView{MaxConcurrentJobs: 4, JobsRunning: 1}, true},
		{"idle with a published ceiling", NodeView{MaxConcurrentJobs: 4}, true},
		{"publishes nothing, and its queue is empty", NodeView{}, false},
		{"publishes a queue depth of 0 and no ceiling", NodeView{QueueDepth: 0, MaxQueueDepth: 8}, false},
		{"every worker busy", NodeView{MaxConcurrentJobs: 4, JobsRunning: 4, QueueDepth: 4}, false},
		{"a worker free but a job queued: a node mid-transition", NodeView{MaxConcurrentJobs: 4, JobsRunning: 1, JobsQueued: 1, QueueDepth: 2}, false},
		{"reports itself saturated", NodeView{MaxConcurrentJobs: 4, JobsRunning: 1, SaturationKnown: true, SaturationHigh: true}, false},
		{"its admission queue is at the ceiling", NodeView{MaxConcurrentJobs: 4, JobsRunning: 1, MaxQueueDepth: 2, QueueDepth: 2}, false},
	}
	for _, tc := range cases {
		if got := freeWorkerProven(tc.v); got != tc.want {
			t.Errorf("%s: freeWorkerProven = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestACooldownIsLiftedOnlyOnProofFromAReadAfterTheRefusal is the lift itself, one row per condition.
func TestACooldownIsLiftedOnlyOnProofFromAReadAfterTheRefusal(t *testing.T) {
	const base = "http://node-a:18811"
	free := NodeView{MaxConcurrentJobs: 4, JobsRunning: 1}
	held := func() *cooldowns {
		c := &cooldowns{}
		c.hold(base, time.Now().Add(5*time.Minute))
		return c
	}
	// after is "now, once the refusal has been noted": a read taken from here postdates it. It is taken
	// per case, after the hold, never up front: a slow runner must not move the refusal past the read.
	after := time.Now

	c := held()
	if note, ok := c.lift(base, free, after(), time.Now()); !ok || !strings.Contains(note, "Retry-After cooldown") || !strings.Contains(note, "1 of 4 worker(s) running") {
		t.Fatalf("lift = %q %v, want the cooldown lifted with the proof in the note", note, ok)
	}
	if _, still := c.heldUntil(base, time.Now()); still {
		t.Fatal("the node is still held after its cooldown was lifted")
	}

	for _, tc := range []struct {
		name      string
		v         NodeView
		readAfter func() time.Time
	}{
		{"a read that predates the refusal", free, func() time.Time { return time.Now().Add(-time.Minute) }},
		{"a read of unknown age", free, func() time.Time { return time.Time{} }},
		{"health that proves nothing", NodeView{}, after},
		{"a saturated node", NodeView{MaxConcurrentJobs: 4, SaturationKnown: true, SaturationHigh: true}, after},
		{"a node with a backlog", NodeView{MaxConcurrentJobs: 4, JobsRunning: 4, JobsQueued: 2}, after},
	} {
		c := held()
		if _, ok := c.lift(base, tc.v, tc.readAfter(), time.Now()); ok {
			t.Errorf("%s: the cooldown was lifted", tc.name)
		}
		if _, still := c.heldUntil(base, time.Now()); !still {
			t.Errorf("%s: the hold is gone", tc.name)
		}
	}

	if _, ok := (&cooldowns{}).lift(base, free, after(), time.Now()); ok {
		t.Error("a node with no hold was reported lifted")
	}
	expired := &cooldowns{}
	expired.hold(base, time.Now().Add(-time.Second))
	if _, ok := expired.lift(base, free, after(), time.Now()); ok {
		t.Error("an expired hold was reported lifted")
	}

	// A node that refuses again after a lift is firm: its counters did not predict its refusal, so its
	// Retry-After stands for the rest of the run.
	c = held()
	if _, ok := c.lift(base, free, after(), time.Now()); !ok {
		t.Fatal("setup: the first lift failed")
	}
	c.hold(base, time.Now().Add(5*time.Minute))
	if _, ok := c.lift(base, free, after(), time.Now()); ok {
		t.Fatal("a node that refused after a lift was lifted a second time")
	}
}

// TestNewestHoldIsTheLatestRefusalStillHolding: the wait's read must postdate it, and only a hold in
// force counts.
func TestNewestHoldIsTheLatestRefusalStillHolding(t *testing.T) {
	var c cooldowns
	if !c.newestHold(time.Now()).IsZero() {
		t.Fatal("a run with no refusal has a newest hold")
	}
	c.hold("http://a:1", time.Now().Add(-time.Second)) // already over
	if !c.newestHold(time.Now()).IsZero() {
		t.Fatal("an expired hold counted as the newest")
	}
	c.hold("http://b:1", time.Now().Add(time.Minute))
	first := c.newestHold(time.Now())
	time.Sleep(2 * time.Millisecond)
	c.hold("http://c:1", time.Now().Add(time.Minute))
	if second := c.newestHold(time.Now()); !second.After(first) {
		t.Fatalf("newest hold %s did not move past %s after a later refusal", second, first)
	}
}

// liftingNode is an accepting node that refuses its first `refuse` dispatches with a long
// Retry-After while its health advertises `published` capacity: the node of the diagnosis whose hint
// outlives its own backlog.
func liftingNode(t *testing.T, refuse int64, published bool) (*fakeNode, string) {
	t.Helper()
	return acceptingNode(t, "node-late", "answer", func(f *fakeNode) {
		f.dispatchHook = freesAfter(refuse, http.StatusServiceUnavailable)
		f.dispatchRetryAfter = "300"
		if published {
			f.maxConcurrentJobs, f.maxQueueDepth = 4, 8 // jobs_running 0, nothing queued: a free worker, proven
		}
	})
}

// TestAWaitingSubtaskIsPlacedOnANodeWhoseHealthShowsAFreeWorkerAfterItsRefusal: the node refuses
// once with a 300 s Retry-After, then its health shows a free worker. Before, the subtask sat out the
// hint (here: the wait's whole 5 s) and deferred with the node idle; now the next tick's read proves
// the worker and the node is asked again at once.
func TestAWaitingSubtaskIsPlacedOnANodeWhoseHealthShowsAFreeWorkerAfterItsRefusal(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := liftingNode(t, 1, true)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 5

	start := time.Now()
	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{url}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the run took %s: the node's Retry-After was sat out although its health proved a free worker", elapsed)
	}
	pr := results[0]
	if sum.Succeeded != 1 || pr.Node != "node-late" || node.dispatches.Load() != 2 {
		t.Fatalf("summary %+v node %q dispatches %d, want the subtask placed on the node by the second ask", sum, pr.Node, node.dispatches.Load())
	}
	for _, want := range []string{"capacity wait", "Retry-After cooldown", "was lifted", "none queued"} {
		if !strings.Contains(pr.PlacementReason, want) {
			t.Errorf("placement = %q, want it to say %q", pr.PlacementReason, want)
		}
	}
}

// TestALiftExplainsADispatchOnlyWhileItIsWhatLetTheNodeBeAsked: the words "its cooldown was lifted" ride the
// reason of a dispatch only when the lift is what allowed it. Lifted and then asked again before the hint
// would have ended: narrated. Lifted, then not asked until the hint had run out anyway: the node was asked
// because the hint ended, and the reason must not claim a speed-up. Lifted, then refused again: the next
// dispatch follows the new hold, not the lift.
func TestALiftExplainsADispatchOnlyWhileItIsWhatLetTheNodeBeAsked(t *testing.T) {
	const base = "http://node-a:18811"
	free := NodeView{MaxConcurrentJobs: 4, JobsRunning: 1}
	hint := time.Now().Add(5 * time.Minute)
	lifted := func() *cooldowns {
		c := &cooldowns{}
		c.hold(base, hint)
		if _, ok := c.lift(base, free, time.Now(), time.Now()); !ok {
			t.Fatal("setup: the cooldown was not lifted")
		}
		return c
	}

	c := lifted()
	if got := c.liftNarration(base, time.Now()); !strings.Contains(got, "was lifted") {
		t.Errorf("a dispatch inside the hint the lift ended: narration %q, want the lift named", got)
	}
	if got := c.liftNarration(base, time.Now()); got == "" {
		t.Error("the narration of a lift is not spent by being read: a gated dispatch (nothing sent) must be able to read it again")
	}
	if got := lifted().liftNarration(base, hint.Add(time.Second)); got != "" {
		t.Errorf("a dispatch after the hint would have ended anyway: narration %q, want none", got)
	}
	again := lifted()
	again.hold(base, time.Now().Add(time.Minute)) // the node refused the dispatch the lift allowed
	if got := again.liftNarration(base, time.Now()); got != "" {
		t.Errorf("a dispatch after the node refused again: narration %q, want none: the new hold, not the lift, ended before it", got)
	}
	if got := (&cooldowns{}).liftNarration(base, time.Now()); got != "" {
		t.Errorf("a node never lifted: narration %q, want none", got)
	}
}

// TestALiftIsNotClaimedByALaterDispatchThatFollowedARefusal is the wait end to end: the node's health shows
// a free worker, it refuses twice (no Retry-After, so each refusal holds it for the compressed 150 ms), and
// takes the third. The cooldown is lifted once, after the first refusal, and the second dispatch is the one
// the lift allowed; it is refused, the node goes firm, and the third dispatch lands when the second
// refusal's own hold has run out. Its reason must not say the cooldown was lifted: that dispatch followed a
// hold the lift did not end.
func TestALiftIsNotClaimedByALaterDispatchThatFollowedARefusal(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 150*time.Millisecond)
	node, url := acceptingNode(t, "node-late", "answer", func(f *fakeNode) {
		f.dispatchHook = freesAfter(2, http.StatusServiceUnavailable)
		f.maxConcurrentJobs, f.maxQueueDepth = 4, 8 // jobs_running 0, nothing queued: a free worker, proven
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 5

	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{url}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	pr := results[0]
	if sum.Succeeded != 1 || node.dispatches.Load() != 3 {
		t.Fatalf("summary %+v dispatches %d, want the third dispatch to land after two refusals", sum, node.dispatches.Load())
	}
	if !strings.Contains(pr.PlacementReason, "capacity wait") {
		t.Fatalf("placement = %q, want the capacity wait's placement", pr.PlacementReason)
	}
	if strings.Contains(pr.PlacementReason, "was lifted") {
		t.Errorf("placement = %q claims a lift for a dispatch that followed the node's second refusal", pr.PlacementReason)
	}
}

// TestAHealthReadThatProvesNothingKeepsTheCooldown is the control arm: the same refusal, but the
// node's health publishes no ceiling, so nothing proves a worker free. The cooldown stands, the node
// is asked once, and the wait ends as it always did, naming the cooldown.
func TestAHealthReadThatProvesNothingKeepsTheCooldown(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := liftingNode(t, 1, false)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1

	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{url}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Deferred != 1 || node.dispatches.Load() != 1 {
		t.Fatalf("summary %+v dispatches %d, want one ask and a capacity defer: health that proves nothing must not lift the cooldown", sum, node.dispatches.Load())
	}
	if r := results[0].Result.Reason; !strings.Contains(r, "cooling down after its own refusal") {
		t.Fatalf("reason = %q, want the cooldown named", r)
	}
}

// TestANodeThatRefusesAfterALiftIsAskedEarlyNoMore: the node's health shows a free worker on every
// read, and it refuses every dispatch anyway (a stale VRAM snapshot, say). The cooldown is lifted
// once; the refusal that follows makes it firm; the wait does not ask it once per tick for the rest
// of the call.
func TestANodeThatRefusesAfterALiftIsAskedEarlyNoMore(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := liftingNode(t, 1<<30, true)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1

	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{url}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Deferred != 1 {
		t.Fatalf("summary %+v, want the wait to end as a capacity defer", sum)
	}
	if got := node.dispatches.Load(); got != 2 {
		t.Fatalf("the node was asked %d time(s) in a 1 s wait ticking every 20 ms, want 2: the refusal, and one early ask after the lift", got)
	}
	if r := results[0].Result.Reason; !strings.Contains(r, "cooling down after its own refusal") {
		t.Fatalf("reason = %q, want the firm cooldown named", r)
	}
}

// TestReplacementLiftsACooldownOnProof: the re-placement path reads the fleet after the refusal
// (fetchViewsSince), so a cooled node whose health proves a free worker is a candidate again, and
// the reason says why; with a read of unknown age, or health that proves nothing, it stays out.
func TestReplacementLiftsACooldownOnProof(t *testing.T) {
	const base = "http://node-a:18811"
	st := oneStepSchemaContract(300, false)
	free := eligibleRemote()
	free.NodeID = "node-a"
	free.AgentCtxTokens = 32768
	free.MaxConcurrentJobs, free.MaxQueueDepth = 4, 8
	free.QueueDepth = 0
	unknown := free
	unknown.MaxConcurrentJobs = 0

	// The read's age is taken after the hold, per case: a slow runner must not move the refusal past it.
	hold := func(r *runner) time.Time {
		r.cool.hold(base, time.Now().Add(5*time.Minute))
		return time.Now()
	}

	r := &runner{cfg: testCfg(t)}
	readAfter := hold(r)
	cands, _, busy, lifted := r.withRoomAfter(st, []NodeView{free}, []string{base}, readAfter)
	if len(cands) != 1 || len(busy) != 0 || !strings.Contains(lifted[base], "was lifted") {
		t.Fatalf("candidates %d busy %v lifted %v, want the node a candidate again with the lift named", len(cands), busy, lifted)
	}

	for name, tc := range map[string]struct {
		v      NodeView
		unaged bool
	}{
		"health that proves nothing": {unknown, false},
		"a read of unknown age":      {free, true},
	} {
		r := &runner{cfg: testCfg(t)}
		readAfter := hold(r)
		if tc.unaged {
			readAfter = time.Time{}
		}
		cands, _, busy, lifted := r.withRoomAfter(st, []NodeView{tc.v}, []string{base}, readAfter)
		if len(cands) != 0 || len(busy) != 1 || !strings.Contains(busy[0], "cooling down after its own refusal") || len(lifted) != 0 {
			t.Errorf("%s: candidates %d busy %v lifted %v, want the node held out by its cooldown", name, len(cands), busy, lifted)
		}
	}
}

// TestTheWaitsReadPostdatesTheNewestRefusal: with the probe memo ON (production), a snapshot another
// waiter's probe took before a refusal must not be reused to judge that refusal's cooldown. With no
// hold in force the memo serves the read, as it always did.
func TestTheWaitsReadPostdatesTheNewestRefusal(t *testing.T) {
	node, url := acceptingNode(t, "node-a", "answer", func(f *fakeNode) { f.maxConcurrentJobs = 4 })
	r := &runner{cfg: testCfg(t), route: "remote", remotes: []string{url}}
	ctx := context.Background()

	if _, _, _, _, after := r.fleetReadForWait(ctx); !after.IsZero() {
		t.Fatalf("a read with no hold in force is bound to %s, want the plain memoised read", after)
	}
	if _, _, _, _, _ = r.fleetReadForWait(ctx); node.healths.Load() != 1 {
		t.Fatalf("health read %d times, want the second no-hold read served by the memo", node.healths.Load())
	}

	time.Sleep(2 * time.Millisecond)
	r.cool.hold(url, time.Now().Add(5*time.Minute)) // a refusal AFTER the memoised snapshot
	_, _, _, _, after := r.fleetReadForWait(ctx)
	if node.healths.Load() != 2 {
		t.Fatalf("health read %d times, want a fresh read: the memo predates the refusal", node.healths.Load())
	}
	if _, ok := r.cool.lift(url, NodeView{MaxConcurrentJobs: 4}, after, time.Now()); !ok {
		t.Fatal("a read taken after the refusal that shows a free worker did not lift the cooldown")
	}
}

// TestReplacementNodeChoosesACooledNodeWhoseFreshHealthProvesAFreeWorker is the re-placement wiring:
// a sibling's refusal put the node on a 300 s cooldown; this subtask has not tried it, and the read
// replacementNode takes after its own refusal shows a free worker. The node is chosen, and the reason
// says its cooldown was lifted. Without the read's age passed on, the cooldown stands.
func TestReplacementNodeChoosesACooledNodeWhoseFreshHealthProvesAFreeWorker(t *testing.T) {
	noProbeMemo(t)
	node, url := acceptingNode(t, "node-a", "answer", func(f *fakeNode) { f.maxConcurrentJobs, f.maxQueueDepth = 4, 8 })
	r := &runner{cfg: testCfg(t), route: "remote", remotes: []string{url}, local: neverLocal(t)}
	r.cool.hold(url, time.Now().Add(5*time.Minute))

	next, why, ok := r.replacementNode(t.Context(), plainContract(), newPlacements(), 1)
	if !ok || next.base != url {
		t.Fatalf("replacementNode = %+v %q %v, want the cooled node chosen: its health, read after the refusal, proves a free worker", next, why, ok)
	}
	if !strings.Contains(next.reason, "re-placed on node-a") || !strings.Contains(next.reason, "was lifted") {
		t.Fatalf("reason = %q, want the re-placement to say the cooldown was lifted", next.reason)
	}
	if node.healths.Load() < 1 {
		t.Fatal("the node's health was never read")
	}
}
