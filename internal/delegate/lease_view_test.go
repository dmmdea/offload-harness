package delegate

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestRemoteEligible_TextLeasedNodeIsIneligible: a node advertising a held
// EXCLUSIVE or DRAINING text lease is not a placement target; the control arm
// (lease cleared) is eligible again, so the refusal is the lease's and
// nothing else's.
//
// W-14 (register S-15, PR-5 item 3) narrowed this: before, `LeasedText`
// ALONE — held, whatever the duration or the node's own busy verdict —
// hard-excluded (the 0.113.16 rule this test originally pinned). That is
// exactly the declared-window-over-quiet-cards shape S-15 measures (47
// contracts burned 300 s each; the Qube had zero agent jobs in flight in 10
// of them): a lease the node has not even called BUSY yet, let alone
// exclusive or draining, no longer excludes a remote on its own. See
// busylease_test.go's TestRemoteEligibleExcludesABusyLease for the full
// exclude/demote matrix this test's fixture is one corner of.
func TestRemoteEligible_TextLeasedNodeIsIneligible(t *testing.T) {
	r := eligibleRemote()
	if !remoteEligible(schemaSubtask(), r) {
		t.Fatal("control: the baseline remote must be eligible")
	}
	r.LeasedText, r.LeaseExclusive = true, true
	if remoteEligible(schemaSubtask(), r) {
		t.Fatal("a node whose card is reserved by an EXCLUSIVE text lease must not be placed on")
	}
	r.LeaseExclusive = false
	if !remoteEligible(schemaSubtask(), r) {
		t.Fatal("a plain (non-exclusive, non-draining, not-yet-busy) text lease must stay eligible (W-14)")
	}
	r.LeasedText = false
	if !remoteEligible(schemaSubtask(), r) {
		t.Fatal("lease cleared: eligible again")
	}
}

// TestNoEligibleRemoteNamesTheLeasedNode: when the only fitting remote holds a
// text lease, the deferral names it (class infrastructure) instead of the
// defensive "placement and gate disagree — please report" line; the control
// arm (lease cleared) still reaches that defensive line, so the wording is
// the lease's and not a rewrite of the fallback.
func TestNoEligibleRemoteNamesTheLeasedNode(t *testing.T) {
	r := &runner{remotes: []string{"http://lenovo:18811"}}
	v := eligibleRemote()
	v.NodeID = "lenovo-ampere16"
	// EXCLUSIVE, not merely held (W-14): a plain held-and-not-yet-busy text
	// lease no longer excludes on its own (TestRemoteEligible_TextLeasedNodeIsIneligible),
	// so this fixture must fence the card the way remoteEligible actually
	// still refuses it — leasedLanes' reporting still names any HELD text
	// lease by its own rule, which is independent of the gate.
	v.LeasedText, v.LeaseExclusive = true, true
	reason, class := r.noEligibleRemote(schemaSubtask(), []NodeView{v}, nil)
	if !strings.Contains(reason, "text GPU lease") || !strings.Contains(reason, "lenovo-ampere16") || strings.Contains(reason, "please report") {
		t.Fatalf("reason must name the lease and the node: %q", reason)
	}
	if class != core.DeferClassInfrastructure {
		t.Fatalf("class = %q, want infrastructure (the box needs a timing decision, not a rewritten contract)", class)
	}
	v.LeasedText, v.LeaseExclusive = false, false
	reason, _ = r.noEligibleRemote(schemaSubtask(), []NodeView{v}, nil)
	if !strings.Contains(reason, "please report") {
		t.Fatalf("control: without a lease the defensive line must still be reached, got %q", reason)
	}
}

// TestFetchNodeViewDecodesTheLease: health "lease" maps to LeasedText only for
// a HELD TEXT lease; a media lease or an absent key (older node) reads false.
func TestFetchNodeViewDecodesTheLease(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"text held", `{"node_id":"n","agent_enabled":true,"agent_seat":"s","agent_seat_resident":true,"agent_ctx_tokens":8192,"lease":{"held":true,"class":"text","pid":1,"until":"2026-09-06T17:00:00Z"}}`, true},
		{"media held", `{"node_id":"n","lease":{"held":true,"class":"media","pid":1}}`, false},
		{"absent (older node)", `{"node_id":"n","queue_depth":1}`, false},
	}
	for _, c := range cases {
		srv := healthServer(t, c.body, nil)
		got, err := FetchNodeView(context.Background(), srv.URL, "")
		srv.Close()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.LeasedText != c.want {
			t.Fatalf("%s: LeasedText = %v, want %v", c.name, got.LeasedText, c.want)
		}
	}
}
