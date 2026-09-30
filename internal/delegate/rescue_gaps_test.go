package delegate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetqueue"
)

// the rescue's budget follows the contract's own execution budget (with the
// floor under it). Every budget the existing tests use (timeout_sec 30) is
// below the 120 s floor, so a rescue that always waited exactly the floor kept
// the suite green.
func TestRescueBudgetFollowsTheContractsOwnBudget(t *testing.T) {
	rs := &rescuer{structured: `{"answer":"42"}`}
	c := rescueContract()
	c.TimeoutSec = 900
	runRescued(t, rescueNode(t, legacyRepackStall()), c, rs.fn())
	if rs.calls.Load() != 1 {
		t.Fatalf("rescue calls = %d", rs.calls.Load())
	}
	got := time.Duration(rs.gotBudget.Load())
	// 900 s + the poll grace the test compresses to 1 s, less what already elapsed.
	if got < 850*time.Second || got > 902*time.Second {
		t.Fatalf("rescue budget = %s, want about the contract's 900 s (+ grace)", got)
	}
}

// the delegator holds a rescued object to the schema the answer is held to:
// a field the acceptance reads must be present, or the rescue failed and the
// node's defer stands. Without it the object is delivered, the acceptance check
// fails closed on the absent field, and a rescue failure is filed as a
// failed_verification (and earns a whole second run on another node).
func TestRescuedObjectMissingAnAcceptanceFieldIsARefusedRescueNotAFailedVerification(t *testing.T) {
	rs := &rescuer{structured: `{"other":"x"}`} // valid for a schema that requires nothing; no `answer`
	results, sum := runRescued(t, rescueNode(t, legacyRepackStall()), rescueContract(), rs.fn())
	r := results[0].Result
	if !r.Deferred || len(r.Structured) != 0 || sum.FailedVerification != 0 || sum.LostToStack != 1 {
		t.Fatalf("summary = %+v result = %+v, want the node's defer standing (lost work), not a delivered object failing acceptance", sum, r)
	}
}

// a rescued answer that fails the DOCUMENT FINGERPRINT is what a node that
// answers about the wrong document produces; the rescue path strikes the node
// exactly as the ordinary path does (two strikes quarantine it).
func TestRescuedAnswerFailingTheDocumentFingerprintStrikesTheNode(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	q := NewQuarantine(10 * time.Minute)
	node := rescueNode(t, legacyRepackStall())
	c := rescueContract()
	c.Acceptance = []string{"regex:(?i)(?P<docanchor>zzzunmatched|qqqunmatched)"}
	rs := &rescuer{structured: `{"answer":"42"}`}
	for i := 0; i < 2; i++ {
		var localCalls atomic.Int64
		if _, _, err := RunWith(context.Background(), testCfg(t), failingLocal(&localCalls), []core.AgentContract{c}, "remote", []string{node}, &RunOptions{Rescue: rs.fn(), Quarantine: q}); err != nil {
			t.Fatal(err)
		}
	}
	if !q.Blocked(node) {
		t.Fatal("two rescued answers that failed the document fingerprint did not quarantine the node that produced them")
	}
}

// for the common case (a loop that used its timeout_sec and finished) the
// rescue's budget IS the floor, so the floor's value is what decides whether a
// slow re-pack can complete. The existing assertion compares the budget with the
// constant itself, so a floor lowered to seconds kept it green.
func TestRescueBudgetOfAShortContractIsTwoMinutes(t *testing.T) {
	rs := &rescuer{structured: `{"answer":"42"}`}
	runRescued(t, rescueNode(t, legacyRepackStall()), rescueContract(), rs.fn()) // timeout_sec 30
	if got := time.Duration(rs.gotBudget.Load()); got != 2*time.Minute {
		t.Fatalf("rescue budget = %s, want the 2 minute floor: a re-pack of a long answer on a slow seat is minutes", got)
	}
}

// a queue-route result that stays deferred (nothing to rescue, or the
// rescue failed) is published as the defer it is: acceptance is never evaluated
// over a result the node did not complete, or a budget defer would read as a
// failed_verification.
func TestRunQueueRouteDoesNotEvaluateAcceptanceOverADeferredResult(t *testing.T) {
	q, err := fleetqueue.Open(t.TempDir() + "/q.db")
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	mux := http.NewServeMux()
	fleetqueue.Mount(mux, q, func(*http.Request) bool { return true })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	go func() {
		for i := 0; i < 100; i++ {
			job, ok, _ := q.Claim("sim-node", []string{"agent"})
			if ok {
				wire, _ := json.Marshal(core.AgentWireResult{
					SchemaVersion: core.AgentWireSchemaVersion, NodeID: "sim-node", Seat: "sim-seat",
					Deferred: true, DeferClass: core.DeferClassBudget, Reason: "ceiling 1800s reached while producing",
				})
				_ = q.Ack(job.ID, "sim-node", wire, "")
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	cfg := config.Config{FleetQueueHolder: srv.URL, StateDir: t.TempDir()}
	contract := core.AgentContract{
		Goal:         "which shipment is refrigerated?",
		OutputSchema: json.RawMessage(`{"properties":{"shipment_id":{"type":"string"}}}`),
		Acceptance:   []string{"contains:RF-9082"},
		TimeoutSec:   30,
	}
	results, sum, rerr := RunWith(context.Background(), cfg, nil, []core.AgentContract{contract}, "queue", nil, nil)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if sum.Deferred != 1 || sum.FailedVerification != 0 || len(results[0].AcceptanceFailures) != 0 {
		t.Fatalf("summary = %+v failures = %v, want the budget defer counted as a defer, with no acceptance verdict", sum, results[0].AcceptanceFailures)
	}
}
