package pipeline

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/reviewlane"
)

// The review lane salvages a deferred answer by reading what the node SAYS about the deferral
// (reviewlane.Salvage: output_truncated, or schema_miss with the budget class), so the node and
// the lane are two ends of one contract that neither package's own tests can pin. A lane test
// that builds the wire by hand proves the lane reads its own reading of the node, and a runner
// test proves the node files what it files; if either side moves, the salvage would silently stop
// (or start salvaging a seat failure) with every test still green. These run the lane's REAL
// contract through the real runner against a fake seat and hand the wire to the lane's reader.
func TestReviewLaneReadsWhatTheRunnerFiles(t *testing.T) {
	const diff = "diff --git a/run.go b/run.go\n--- a/run.go\n+++ b/run.go\n@@ -1 +1 @@\n+x\n"
	contract, err := reviewlane.BuildContract("iterate over every element exactly once", diff)
	if err != nil {
		t.Fatalf("BuildContract: %v", err)
	}
	const l1, l2 = "severe | run.go:5 | off-by-one in the loop bound | indexes one past the end", "moderate | run.go:9 | missing nil check | panics on empty input"

	// The seat's final answer ends on the completion budget: a partial, flagged output_truncated,
	// filed as an abstention, with no re-pack sent. The line the budget cut is not a finding.
	t.Run("a final cut at the completion budget", func(t *testing.T) {
		fake := &agentFake{
			rosterIDs:    []string{agentTestSeat},
			loop:         func(int64) string { return lengthChat(l1 + "\n" + l2 + "\nminor | run.go:11 | the cut li") },
			repack:       func(int64) string { return `{"findings":["never asked"]}` },
			chatFallback: func(int64) string { return doneChat(`{"findings":["never asked"]}`) },
		}
		srv := fake.server(t)
		defer srv.Close()
		wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract)))
		if !wire.Deferred || !wire.OutputTruncated {
			t.Fatalf("deferred=%v truncated=%v reason=%q, want the cut-final deferral", wire.Deferred, wire.OutputTruncated, wire.Reason)
		}
		kind, lines := reviewlane.Salvage(wire)
		if kind != reviewlane.SalvagedOutputTruncated || len(lines) != 2 || lines[0] != l1 || lines[1] != l2 {
			t.Fatalf("salvage = %q %q, want output_truncated and the two complete lines", kind, lines)
		}
	})

	// The loop finished and the wall could not buy the re-pack (0.5 tok/s over a 30 s wall plus
	// its slack): the clock decided, the finished answer rides in output flagged schema_miss.
	t.Run("a re-pack the wall could not buy", func(t *testing.T) {
		short := contract
		short.TimeoutSec, short.TimeoutAuto = 30, false // the lane's own contract auto-sizes its wall; this one pins it
		fake := &agentFake{
			rosterIDs: []string{agentTestSeat},
			loop:      func(int64) string { return doneChat(l1 + "\n" + l2) },
			repack:    func(int64) string { return `{"findings":["never asked"]}` },
		}
		srv := fake.server(t)
		defer srv.Close()
		wire := decodeWire(t, rateTestPipeline(t, srv.URL, 0.5).Run(context.Background(), agentTestRequest(t, short)))
		if !wire.Deferred || wire.DeferClass != core.DeferClassBudget || !wire.SchemaMiss || !strings.Contains(wire.RepackNote, "re-pack skipped:") {
			t.Fatalf("deferred=%v class=%q schema_miss=%v note=%q, want the wall-skipped re-pack", wire.Deferred, wire.DeferClass, wire.SchemaMiss, wire.RepackNote)
		}
		kind, lines := reviewlane.Salvage(wire)
		if kind != reviewlane.SalvagedWall || len(lines) != 2 || lines[1] != l2 {
			t.Fatalf("salvage = %q %q, want wall and both lines of the finished answer", kind, lines)
		}
	})

	// A re-pack the stack failed, or that the seat answered in the wrong shape, is the stack or the
	// seat failing and not the clock: it stays a defer. Asserting the shape of the wire first keeps
	// these from passing vacuously should the runner ever stop filing them.
	t.Run("a re-pack the stack failed is not the clock", func(t *testing.T) {
		fake := &agentFake{
			rosterIDs:          []string{agentTestSeat},
			loop:               func(int64) string { return doneChat(l1 + "\n" + l2) },
			repackStatus:       http.StatusInternalServerError,
			chatFallbackStatus: http.StatusInternalServerError,
		}
		srv := fake.server(t)
		defer srv.Close()
		wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract)))
		if !wire.Deferred || !wire.SchemaMiss || wire.DeferClass != core.DeferClassInfrastructure {
			t.Fatalf("deferred=%v schema_miss=%v class=%q reason=%q, want a flagged infrastructure defer", wire.Deferred, wire.SchemaMiss, wire.DeferClass, wire.Reason)
		}
		if kind, _ := reviewlane.Salvage(wire); kind != "" {
			t.Fatalf("salvage = %q on an unreachable re-pack: a seat or stack failure must stay a defer", kind)
		}
	})
	t.Run("a re-pack answered in the wrong shape is not the clock", func(t *testing.T) {
		fake := &agentFake{
			rosterIDs:    []string{agentTestSeat},
			loop:         func(int64) string { return doneChat(l1 + "\n" + l2) },
			repack:       func(int64) string { return `{"wrong":"shape"}` },
			chatFallback: func(int64) string { return doneChat(`{"wrong":"shape"}`) },
		}
		srv := fake.server(t)
		defer srv.Close()
		wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract)))
		if !wire.Deferred || !wire.SchemaMiss || wire.DeferClass != core.DeferClassAbstention {
			t.Fatalf("deferred=%v schema_miss=%v class=%q reason=%q, want a flagged abstention", wire.Deferred, wire.SchemaMiss, wire.DeferClass, wire.Reason)
		}
		if kind, _ := reviewlane.Salvage(wire); kind != "" {
			t.Fatalf("salvage = %q on a wrong-shape re-pack: an abstention must stay a defer", kind)
		}
	})
}
