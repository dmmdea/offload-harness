package main

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// TestSmokeRowForUnexercisedNodeNamesTheBaseNotTheLocalBox is the regression for
// issue #250. A forced remote route that finds nothing eligible (the base is
// unreachable, or fails the capability gate) returns a DEFER carrying the LOCAL
// node and seat — the box that made the decision, not one that ran anything.
// Rendered straight, that row read "local-box / agent-pool / DEFER" and an operator
// reasonably concluded their own workstation had failed a smoke test, while the
// base that was actually dead went unnamed.
func TestSmokeRowForUnexercisedNodeNamesTheBaseNotTheLocalBox(t *testing.T) {
	base := "http://node-a.invalid:18811"
	r := delegate.PlacedResult{
		Node:            "local-box",
		Seat:            "agent-pool",
		Unplaced:        true,
		PlacementReason: "route=remote: no eligible remote",
		Result: core.AgentWireResult{
			Deferred: true,
			Reason:   "route=remote: all 1 configured remote(s) failed the health probe: dial tcp: refused",
		},
	}

	row := smokeRowFor(base, r)

	if row.Verdict != "DEFER" {
		t.Fatalf("verdict = %q, want DEFER — an unexercised node is real fleet signal", row.Verdict)
	}
	if row.Node != "" || row.Seat != "" {
		t.Fatalf("row named node=%q seat=%q; want both empty so the table renders the BASE that was not exercised", row.Node, row.Seat)
	}
	if row.Base != base {
		t.Fatalf("row.Base = %q, want %q", row.Base, base)
	}
	if !strings.Contains(row.Detail, "not exercised") || !strings.Contains(row.Detail, "health probe") {
		t.Fatalf("detail = %q, want it to say the node was not exercised AND why", row.Detail)
	}
	// The rendered table must show the base, never the local box.
	table := renderSmokeTable([]smokeRow{row})
	if !strings.Contains(table, base) {
		t.Fatalf("table does not name the base:\n%s", table)
	}
	if strings.Contains(table, "agent-pool") || strings.Contains(table, "local-box") {
		t.Fatalf("table still names the LOCAL box/seat as the node under test:\n%s", table)
	}
	// And it must still fail the run.
	if err := exitError([]smokeRow{row}); err == nil {
		t.Fatal("exitError = nil for an unexercised node; want non-nil")
	}
}

// TestSmokeRowForRealLandingKeepsTheNode pins the other side: a result that
// actually ran on the remote keeps its node and seat, so the fix above cannot
// be "blank the node whenever things look odd".
func TestSmokeRowForRealLandingKeepsTheNode(t *testing.T) {
	r := delegate.PlacedResult{
		Node:            "node-a",
		Seat:            "qwen3.5-4b-vllm",
		PlacementReason: "route=remote forced → node-a",
		Result:          core.AgentWireResult{WallMs: 1234},
	}

	row := smokeRowFor("http://node-a.invalid:18811", r)

	if row.Verdict != "PASS" {
		t.Fatalf("verdict = %q, want PASS", row.Verdict)
	}
	if row.Node != "node-a" || row.Seat != "qwen3.5-4b-vllm" {
		t.Fatalf("node/seat = %q/%q, want the REMOTE's identity preserved", row.Node, row.Seat)
	}
	if row.WallMs != 1234 {
		t.Fatalf("wall = %d, want 1234", row.WallMs)
	}
}

// TestSmokeRowForDeferredByTheNodeKeepsItsIdentity: a node that ANSWERED and
// deferred is a different story from one that was never asked — its row must
// still name it, or an operator cannot tell an overloaded node from a dead one.
func TestSmokeRowForDeferredByTheNodeKeepsItsIdentity(t *testing.T) {
	r := delegate.PlacedResult{
		Node:            "node-b",
		Seat:            "qwen3.5-9b-agent",
		PlacementReason: "route=remote forced → node-b",
		Result: core.AgentWireResult{
			Deferred: true,
			Reason:   "seat busy",
		},
	}

	row := smokeRowFor("http://node-b.invalid:18811", r)

	if row.Verdict != "DEFER" {
		t.Fatalf("verdict = %q, want DEFER", row.Verdict)
	}
	if row.Node != "node-b" {
		t.Fatalf("node = %q, want the node that answered to keep its name", row.Node)
	}
	if strings.Contains(row.Detail, "not exercised") {
		t.Fatalf("detail = %q; the node DID answer — that phrasing is for a node never asked", row.Detail)
	}
}
