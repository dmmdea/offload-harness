package mediaremote

// What the placer remembers, and the rules that keep one call from running twice (ADR 0082). Two reviews of the first
// cut found the same hole from two sides: a node that read idle in health but whose own grant refused the job was sent
// one job per call for ever (each parked for the node's whole window), and the rules that stop a job running on two nodes
// (an ambiguous POST is final, a refused dial moves on, a call is sent to at most three nodes) were pinned by no test: a
// mutation of each left the whole package green.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
)

// pinPlacerClock fixes the placer's clock and removes its jitter, so a test moves time by assigning to the returned
// pointer and every pause is exactly the step it was set from.
func pinPlacerClock(t *testing.T) *time.Time {
	t.Helper()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	prevNow, prevJitter := placerNow, placerJitter
	placerNow = func() time.Time { return now }
	placerJitter = func(d time.Duration) time.Duration { return d }
	t.Cleanup(func() { placerNow, placerJitter = prevNow, prevJitter })
	return &now
}

// A node that reads idle in health and answers every job "another job holds my card" (a card the display rule keeps
// closed, a quarantined card, a host short of RAM: its health carries none of that) used to be sent one job per call, and
// each parked for the node's whole gpu_wait_ms before the bounce came back. A bounce now pauses the node, a minute at
// first and then the cap, and only a call the node serves forgets it.
func TestABounceIsRememberedAndTheNodeComesBackAtTheNextStep(t *testing.T) {
	fams := map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}
	a := startImageNode(t, "node-a", "hidream-o1", fams, nodeOpts{deferAs: "gpu busy: no card can take this call right now: card 0 is the display's", deferClass: core.ErrClassGPUBusy})
	b := startImageNode(t, "node-b", "hidream-o1", fams, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, a, b)
	now := pinPlacerClock(t)
	call := func(step string) {
		t.Helper()
		lr := &laneRunner{verdict: heldLane()}
		res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
		if !res.OK || res.Meta.Node != "node-b" {
			t.Fatalf("%s: the node that admits serves the call: %+v", step, res)
		}
		if _, runs := lr.counts(); runs != 0 {
			t.Fatalf("%s: a call a node served is not also run here", step)
		}
	}
	posts := func(step string, want int) {
		t.Helper()
		if got := len(a.posts()); got != want {
			t.Fatalf("%s: node-a has been sent %d job(s), want %d", step, got, want)
		}
	}

	call("first call")
	posts("first call (it bounces)", 1)
	call("second call")
	call("third call")
	posts("three calls in the first minute: no more jobs for a node that just passed one back", 1)
	if len(b.posts()) != 3 {
		t.Fatalf("the other node served all three: %d", len(b.posts()))
	}

	*now = now.Add(61 * time.Second) // the first pause (a minute) is over
	call("after a minute")
	posts("after a minute: tried once more, bounces again", 2)
	*now = now.Add(4 * time.Minute) // inside the second pause (the cap, 5 minutes)
	call("inside the second pause")
	posts("inside the cap", 2)
	*now = now.Add(2 * time.Minute)
	call("past the second pause")
	posts("past the cap: tried again, and the pause stays at the cap", 3)
	*now = now.Add(4 * time.Minute)
	call("inside the held cap")
	posts("the last step holds", 3)
}

// A node that refused a call and then SERVES one is not bouncing: the next refusal starts again at the first step. Without
// that a node that was busy once would be left alone for the cap on its second bounce, a week later.
func TestACallTheNodeServesForgetsItsBounces(t *testing.T) {
	a := startImageNode(t, "node-a", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")},
		nodeOpts{deferAs: "gpu busy: card(s) held by a media-class lease", deferClass: core.ErrClassGPUBusy})
	b := startImageNode(t, "node-b", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, a, b)
	now := pinPlacerClock(t)
	busy := func(on bool) {
		a.runner.mu.Lock()
		defer a.runner.mu.Unlock()
		if on {
			a.runner.deferAs, a.runner.deferClass = "gpu busy: card(s) held by a media-class lease", core.ErrClassGPUBusy
		} else {
			a.runner.deferAs, a.runner.deferClass = "", ""
		}
	}
	call := func() core.Result {
		return Run(context.Background(), cfg, &laneRunner{verdict: heldLane()}, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	}

	if res := call(); !res.OK || res.Meta.Node != "node-b" || len(a.posts()) != 1 {
		t.Fatalf("node-a bounces the first call, node-b serves it: %+v (%d POSTs on node-a)", res, len(a.posts()))
	}
	*now = now.Add(61 * time.Second)
	busy(false)
	if res := call(); !res.OK || res.Meta.Node != "node-a" || len(a.posts()) != 2 {
		t.Fatalf("its pause is over and it is idle now: it serves the call: %+v (%d POSTs on node-a)", res, len(a.posts()))
	}
	busy(true)
	*now = now.Add(time.Second)
	if res := call(); !res.OK || res.Meta.Node != "node-b" || len(a.posts()) != 3 {
		t.Fatalf("it bounces again: %+v (%d POSTs on node-a)", res, len(a.posts()))
	}
	*now = now.Add(61 * time.Second)
	call()
	if got := len(a.posts()); got != 4 {
		t.Errorf("a node that served a call in between starts again at the first step (a minute), not the cap: %d POSTs on node-a", got)
	}
}

// The pause is told as what it is: a node that took the call and passed it back is "bounced", not "unreachable" (it
// answered), and its row says for how long it is being left alone. The old wording said "did not answer a moment ago"
// of a node that had answered.
func TestABouncedNodeReadsAsBouncedAndNeverAsUnreachable(t *testing.T) {
	a := startImageNode(t, "node-a", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")},
		nodeOpts{deferAs: "gpu busy: card(s) held by a media-class lease", deferClass: core.ErrClassGPUBusy})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, a)
	pinPlacerClock(t)

	first := Run(context.Background(), cfg, &laneRunner{verdict: heldLane(), local: queuedLocal()}, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	row, ok := rowFor(clusterOf(t, first), "node-a")
	if !ok || row.State != "bounced" || !strings.Contains(row.Why, "passed it back") || !strings.Contains(row.Why, "not offered another call for 1m0s") {
		t.Fatalf("the call that was bounced says so and says for how long the node is left alone: %+v", row)
	}
	second := Run(context.Background(), cfg, &laneRunner{verdict: heldLane(), local: queuedLocal()}, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	row, ok = rowFor(clusterOf(t, second), "node-a")
	if !ok || row.State != "bounced" || !strings.HasPrefix(row.Why, "not offered a call: ") || !strings.Contains(row.Why, "took the last call and passed it back") || !strings.Contains(row.Why, "looking again in 1m0s") {
		t.Fatalf("the next call is told why the node was not offered it: %+v", row)
	}
	if strings.Contains(row.Why, "did not answer") || row.State == "unreachable" {
		t.Errorf("a node that answered never reads as one that did not: %+v", row)
	}
	if got := len(a.posts()); got != 1 {
		t.Errorf("the node was sent the call once, not once per call: %d", got)
	}
}

// A 503 or 429 with a Retry-After is an answer, and the node is left alone for what it asked (jittered once, capped):
// the row says "refused", and the delegator neither probes nor posts to it meanwhile.
func TestARetryAfterHoldsTheNodeAndReadsAsRefused(t *testing.T) {
	n := startImageNode(t, "node-c", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, nodeOpts{})
	n.refusePost, n.refuseRetryAfter = 503, "30"
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, n)
	now := pinPlacerClock(t)
	call := func() core.Result {
		return Run(context.Background(), cfg, &laneRunner{verdict: heldLane(), local: queuedLocal()}, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	}

	first := call()
	if row, ok := rowFor(clusterOf(t, first), "node-c"); !ok || row.State != "refused" || !strings.Contains(row.Why, "503") {
		t.Fatalf("the call that was refused says so: %+v", row)
	}
	second := call()
	row, ok := rowFor(clusterOf(t, second), "node-c")
	if !ok || row.State != "refused" || !strings.HasPrefix(row.Why, "not offered a call: ") || !strings.Contains(row.Why, "answered 503") || !strings.Contains(row.Why, "asked to be left alone") || !strings.Contains(row.Why, "looking again in 30s") {
		t.Fatalf("the next call is told the node asked to be left alone, and for how long: %+v", row)
	}
	if strings.Contains(row.Why, "did not answer") || row.State == "unreachable" {
		t.Errorf("a node that answered 503 never reads as one that did not answer: %+v", row)
	}
	if got := len(n.posts()); got != 1 {
		t.Fatalf("held for its Retry-After, the node is not sent a second job: %d POSTs", got)
	}
	*now = now.Add(31 * time.Second)
	call()
	if got := len(n.posts()); got != 2 {
		t.Errorf("once the Retry-After is over the node is tried again: %d POSTs", got)
	}
}

// A node that accepted the job and could not take its lease (a lease location it cannot use, a host-RAM need no state of
// it admits) ran nothing: the call goes on to the next node under a fresh job id instead of ending on one node's fault,
// and the node is left alone like any node that passed a call back.
func TestALeaseThatCannotBeTakenMovesTheCallOnAndPausesTheNode(t *testing.T) {
	fams := map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}
	a := startImageNode(t, "node-a", "hidream-o1", fams, nodeOpts{deferAs: "gpu lease unavailable: host RAM can never admit this need", deferClass: core.ErrClassGPULeaseUnavailable})
	b := startImageNode(t, "node-b", "hidream-o1", fams, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, a, b)
	pinPlacerClock(t)
	lr := &laneRunner{verdict: heldLane()}
	res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	if !res.OK || res.Meta.Node != "node-b" {
		t.Fatalf("the next node serves a call the first could not admit: %+v", res)
	}
	if _, runs := lr.counts(); runs != 0 {
		t.Errorf("a call a node served is not also run here: %d", runs)
	}
	ida, idb := jobIDs(t, a), jobIDs(t, b)
	if len(ida) != 1 || len(idb) != 1 || ida[0] == idb[0] {
		t.Errorf("each node is sent the call once, under a fresh job id: %v / %v", ida, idb)
	}
	if _, backed := placer.backedOff(a.srv.URL); !backed {
		t.Error("the node that could not take its lease is left alone")
	}
	// Alone, the call stays in its own queue and the row says what happened, as a refusal and not as a bounce off a card.
	only := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, a)
	pinPlacerClock(t)
	res = Run(context.Background(), only, &laneRunner{verdict: heldLane(), local: queuedLocal()}, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	row, ok := rowFor(clusterOf(t, res), "node-a")
	if !ok || row.State != "refused" || !strings.Contains(row.Why, "could not take its lease") || !strings.Contains(row.Why, "nothing ran") {
		t.Errorf("the row names the fault: %+v", row)
	}
}

// THE RULE THAT KEEPS A JOB FROM RUNNING TWICE. A node that took the job and whose answer never reached the delegator
// (the connection closed before a byte came back) may be rendering it. The POST is ambiguous, so the call is not placed
// on another node and not run here: it ends naming the node. A refactor of post() that read every transport failure as
// "never reached" would send the same render to two nodes, and no other test noticed.
func TestAnAmbiguousPostIsFinal(t *testing.T) {
	fams := map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}
	a := startImageNode(t, "node-a", "hidream-o1", fams, nodeOpts{})
	b := startImageNode(t, "node-b", "hidream-o1", fams, nodeOpts{})
	a.dropAck = true
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, a, b)
	lr := &laneRunner{verdict: heldLane()}

	done := make(chan core.Result, 1)
	go func() {
		done <- Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	}()
	var res core.Result
	select {
	case res = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the call never ended")
	}
	if got := len(a.posts()); got != 1 {
		t.Fatalf("node-a took the job once: %d POSTs", got)
	}
	if got := len(b.posts()); got != 0 {
		t.Fatalf("DUPLICATE RENDER: node-a took the job (its answer was lost) and the call was also sent to node-b (%d POST)", got)
	}
	if _, runs := lr.counts(); runs != 0 {
		t.Fatalf("DUPLICATE RENDER: node-a took the job and the call was also run here (%d)", runs)
	}
	if res.OK || !res.Deferred || res.DeferClass != core.DeferClassInfrastructure || !strings.Contains(res.Reason, "dispatch") {
		t.Errorf("the call ends as an infrastructure deferral that names the failed dispatch: %+v", res)
	}
	if !strings.Contains(res.Meta.Placement, "remote:") {
		t.Errorf("the answer still says the call left this machine: %q", res.Meta.Placement)
	}
}

// A dial that never connected (the node went away between the health read and the POST) sent nothing, so the call moves
// on to the next node, and the node that refused the connection is left alone for the failure steps (5, 15, 60, 300 s)
// instead of being read and posted to again by the next call. TestDeadNodeBacksOff covers a node whose HEALTH read fails;
// this is the other door.
func TestARefusedDialMovesOnAndBacksTheNodeOff(t *testing.T) {
	fams := map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}
	a := startImageNode(t, "node-a", "hidream-o1", fams, nodeOpts{})
	b := startImageNode(t, "node-b", "hidream-o1", fams, nodeOpts{})
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, a, b)
	now := pinPlacerClock(t)

	// node-a is read healthy, then gone before the POST.
	var asked [][]string
	prev := probeRoster
	probeRoster = func(ctx context.Context, remotes []string, token string, timeout time.Duration) []rosterprobe.Reading {
		asked = append(asked, append([]string(nil), remotes...))
		out := prev(ctx, remotes, token, timeout)
		a.srv.CloseClientConnections()
		a.srv.Close()
		return out
	}
	t.Cleanup(func() { probeRoster = prev })

	lr := &laneRunner{verdict: heldLane()}
	res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	if !res.OK || res.Meta.Node != "node-b" {
		t.Fatalf("a dial that never connected sent nothing, so the next node serves the call: %+v", res)
	}
	if pz, backed := placer.backedOff(a.srv.URL); !backed || pz.state != "unreachable" || pz.left != 5*time.Second {
		t.Fatalf("the node that refused the connection backs off for the first step: %+v %v", pz, backed)
	}
	*now = now.Add(2 * time.Second)
	if pz, _ := placer.backedOff(a.srv.URL); pz.why() != "not probed: it did not answer a moment ago; looking again in 3s" {
		t.Errorf("a node that was never reached reads as not probed, and says for how long: %q", pz.why())
	}
	if res := Run(context.Background(), cfg, &laneRunner{verdict: heldLane()}, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil); !res.OK || res.Meta.Node != "node-b" {
		t.Fatalf("%+v", res)
	}
	if len(asked) != 2 || len(asked[1]) != 1 || asked[1][0] != b.srv.URL {
		t.Errorf("inside its pause the node is not even read: %v", asked)
	}
}

// A call is sent to at most three nodes: a fourth idle node that would also bounce is named and not tried. Without the
// bound a roster of bouncing nodes parks the call for each node's whole window in turn.
func TestACallIsSentToAtMostThreeNodes(t *testing.T) {
	fams := map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}
	busy := nodeOpts{deferAs: "gpu busy: card(s) held by a media-class lease", deferClass: core.ErrClassGPUBusy}
	var nodes []*node
	for _, id := range []string{"node-a", "node-b", "node-c", "node-d"} {
		nodes = append(nodes, startImageNode(t, id, "hidream-o1", fams, busy))
	}
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, nodes...)
	pinPlacerClock(t)
	lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}
	res := Run(context.Background(), cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), "auto", nil)
	if probes, runs := lr.counts(); probes != 1 || runs != 1 {
		t.Fatalf("every node bounced, so the call runs here once: probes %d runs %d", probes, runs)
	}
	total := 0
	for _, n := range nodes {
		total += len(n.posts())
	}
	if total != maxPlacements {
		t.Fatalf("a call is sent to at most %d nodes: %d POSTs in all", maxPlacements, total)
	}
	noPosts(t, "the fourth node", nodes[3])
	rows := clusterOf(t, res)
	if len(rows) != 4 {
		t.Fatalf("one row per roster node: %+v", rows)
	}
	for i, want := range []string{"bounced", "bounced", "bounced", "skipped"} {
		if rows[i].State != want {
			t.Errorf("row %d (%s) = %s, want %s: %+v", i, rows[i].Node, rows[i].State, want, rows[i])
		}
	}
	if !strings.Contains(rows[3].Why, "at most 3 nodes") {
		t.Errorf("the fourth row says why it was not tried: %+v", rows[3])
	}
}
