package delegate

import (
	"context"
	"strings"
	"testing"
)

// A node that says its card is spoken for is not a placement target, whatever
// the lease class. Before 0.113.27 only a TEXT lease refused, so the harness's
// longest job class — anything holding the MEDIA lease, up to a training run —
// left the node advertising idle slots and placement routed work toward it
// (2026-09-07 audit).
func TestRemoteEligibleExcludesABusyLease(t *testing.T) {
	if !remoteEligible(schemaSubtask(), eligibleRemote()) {
		t.Fatal("the baseline node must be eligible, else this test proves nothing")
	}
	busy := eligibleRemote()
	busy.LeaseBusy = true
	if remoteEligible(schemaSubtask(), busy) {
		t.Fatal("a node reporting a LONG GPU lease of any class must not be a placement target")
	}
	leased := eligibleRemote()
	leased.LeasedText = true
	if remoteEligible(schemaSubtask(), leased) {
		t.Fatal("a text lease must still refuse (unchanged)")
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
