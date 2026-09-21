package delegate

import (
	"context"
	"strings"
	"testing"
)

// A node whose lease FENCES the cards (exclusive, draining, or a non-text
// busy hold — a media render, which gpulease never marks exclusive/draining)
// is not a placement target. Before 0.113.27 only a TEXT lease refused, so
// the harness's longest job class — anything holding the MEDIA lease, up to a
// training run — left the node advertising idle slots and placement routed
// work toward it (2026-09-07 audit).
//
// W-14 (register S-15, PR-5 item 3) narrowed the TEXT case: a plain
// reservation the node reads as merely "busy" (its DECLARED window, not a
// measurement of the cards — see fleetnode's held-idle verdict) no longer
// excludes here. It DEMOTES instead (TestBetterRemoteDemotesALeaseBusyNode),
// because a 6-hour text lease over quiet cards was routing work away from a
// node that could run it.
func TestRemoteEligibleExcludesABusyLease(t *testing.T) {
	if !remoteEligible(schemaSubtask(), eligibleRemote()) {
		t.Fatal("the baseline node must be eligible, else this test proves nothing")
	}
	// A busy lease with no text/exclusive/draining marker is the MEDIA shape
	// (gpulease only ever sets Exclusive/Draining for class=text, and only a
	// text lease decodes LeasedText) — still a hard exclude.
	mediaBusy := eligibleRemote()
	mediaBusy.LeaseBusy = true
	if remoteEligible(schemaSubtask(), mediaBusy) {
		t.Fatal("a node reporting a long NON-TEXT (media) lease must not be a placement target")
	}
	// Exclusive and draining fence the cards outright, whatever LeaseBusy says.
	exclusive := eligibleRemote()
	exclusive.LeasedText, exclusive.LeaseBusy, exclusive.LeaseExclusive = true, true, true
	if remoteEligible(schemaSubtask(), exclusive) {
		t.Fatal("an exclusive lease must exclude the node")
	}
	draining := eligibleRemote()
	draining.LeasedText, draining.LeaseBusy, draining.LeaseDraining = true, true, true
	if remoteEligible(schemaSubtask(), draining) {
		t.Fatal("a draining lease must exclude the node")
	}
	// A plain TEXT lease, not yet read as busy, was never excluded on
	// LeaseBusy alone (it is false) and still is not now.
	leased := eligibleRemote()
	leased.LeasedText = true
	if !remoteEligible(schemaSubtask(), leased) {
		t.Fatal("a text lease the node has not yet called busy must stay eligible")
	}
	// The demotion case: plain text, busy, neither exclusive nor draining —
	// eligible (W-14), ranked last by betterRemote instead of excluded.
	quietlyBusy := eligibleRemote()
	quietlyBusy.LeasedText, quietlyBusy.LeaseBusy = true, true
	if !remoteEligible(schemaSubtask(), quietlyBusy) {
		t.Fatal("a plain, non-exclusive, non-draining text lease that reads busy must stay eligible (W-14 demotes, never excludes)")
	}
}

// TestBetterRemoteDemotesALeaseBusyNode (W-14): among eligible remotes, a
// node under a plain busy text lease loses to ANY node that is not — even one
// that is otherwise worse on every other key (saturated, no provable free
// slot, deeper queue) — because "ranked last" means last, not merely
// tie-broken. Two lease-busy nodes still fall back to the keys below it.
func TestBetterRemoteDemotesALeaseBusyNode(t *testing.T) {
	quiet := eligibleRemote()
	quiet.LeasedText, quiet.LeaseBusy = true, true
	quiet.QueueDepth = 0 // otherwise the best possible node on every other key

	worseButFree := eligibleRemote()
	worseButFree.NodeID = "aorus"
	worseButFree.QueueDepth = 5

	st := schemaSubtask()
	if betterRemote("seed", &st, 0, quiet, worseButFree) {
		t.Fatal("a lease-busy node must not beat a non-lease-busy node, however much better its other keys read")
	}
	if !betterRemote("seed", &st, 0, worseButFree, quiet) {
		t.Fatal("a non-lease-busy node must beat a lease-busy one")
	}

	// Two lease-busy nodes: the existing keys still decide between them.
	quiet2 := eligibleRemote()
	quiet2.NodeID, quiet2.LeasedText, quiet2.LeaseBusy = "lenovo2", true, true
	quiet2.QueueDepth = 3
	if !betterRemote("seed", &st, 0, quiet, quiet2) {
		t.Fatal("between two lease-busy nodes, the lower queue depth must still win")
	}
}

// The node's own busy verdict travels on the wire. A node one release behind
// omits it, which decodes to false — exactly the pre-0.113.27 behaviour, so a
// staggered fleet never changes meaning mid-upgrade.
func TestFetchNodeViewDecodesTheBusyLeaseVerdict(t *testing.T) {
	view := func(body string) NodeView {
		t.Helper()
		srv := healthServer(t, body, nil)
		v, err := FetchNodeView(context.Background(), srv.URL, "")
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	long := view(`{"node_id":"n","agent_enabled":true,"lease":{"held":true,"class":"media","busy":true,"remaining_sec":21600}}`)
	if !long.LeaseBusy {
		t.Fatal("a node saying busy must decode to LeaseBusy")
	}
	if long.LeasedText {
		t.Fatal("a media lease must not decode as a text lease")
	}

	if short := view(`{"node_id":"n","agent_enabled":true,"lease":{"held":true,"class":"media","busy":false,"remaining_sec":20}}`); short.LeaseBusy {
		t.Fatal("a short render must not read as busy")
	}
	if older := view(`{"node_id":"n","agent_enabled":true,"lease":{"held":true,"class":"media"}}`); older.LeaseBusy {
		t.Fatal("a node that omits the verdict must decode to false, i.e. the previous behaviour")
	}
	if none := view(`{"node_id":"n","agent_enabled":true}`); none.LeaseBusy || none.LeasedText {
		t.Fatal("no lease block at all decodes to neither")
	}
}

// An operator asking why nothing landed on a node gets the same kind of
// sentence for a long lease as for a text one.
func TestLeasedLanesNamesALongLeaseToo(t *testing.T) {
	busy := eligibleRemote()
	busy.LeaseBusy = true
	text := eligibleRemote()
	text.LeasedText = true
	free := eligibleRemote()

	got := leasedLanes([]NodeView{busy, text, free})
	if len(got) != 2 {
		t.Fatalf("leasedLanes = %v, want the two leased nodes named", got)
	}
	joined := got[0] + " " + got[1]
	for _, want := range []string{"long GPU lease held", "text lease held"} {
		if !strings.Contains(joined, want) {
			t.Errorf("leasedLanes must say %q; got %v", want, got)
		}
	}
}
