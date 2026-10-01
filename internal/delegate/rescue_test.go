package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// legacyRepackStall is the wire a 0.140.12 node sent for a finished loop whose
// structured re-pack was killed at 120 s (the dashboard job of 2026-09-29): a
// defer of class infrastructure, the loop's 2.6k-character answer intact in
// output, stop_reason done, and no schema_miss flag — the field did not exist.
func legacyRepackStall() core.AgentWireResult {
	w := remoteWire(strings.Repeat("The answer is 42, from the notes. ", 66), "")
	w.Deferred = true
	w.DeferClass = core.DeferClassInfrastructure
	w.Reason = "structured re-pack unreachable: stalled: no progress for 120s in repack (allowed 120s; 661 tok so far)"
	w.TokensOut = 661
	w.RepackMs, w.RepackAttempts = 120001, 3
	return w
}

// rescueContract is a schema contract whose acceptance reads the object's field
// and the finished answer's text.
func rescueContract() core.AgentContract {
	c := remoteContract()
	c.Acceptance = []string{"contains:answer", "nonempty:answer"}
	return c
}

// rescueNode is a fake fleet node that answers every poll with wire.
func rescueNode(t *testing.T, wire core.AgentWireResult) string {
	t.Helper()
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(int64) (map[string]any, int) { return doneWire(t, wire), http.StatusOK },
	}
	return node.server().URL
}

// rescuer is a RescueFunc that records what it was asked and answers with
// structured (or the error).
type rescuer struct {
	calls      atomic.Int64
	gotOutput  atomic.Value // string
	gotBudget  atomic.Int64
	structured string
	err        error
}

func (r *rescuer) fn() RescueFunc {
	return func(ctx context.Context, c core.AgentContract, output string, budget time.Duration) (Rescued, error) {
		r.calls.Add(1)
		r.gotOutput.Store(output)
		r.gotBudget.Store(int64(budget))
		if r.err != nil {
			return Rescued{}, r.err
		}
		return Rescued{Structured: json.RawMessage(r.structured), Seat: "local-seat", How: "one re-pack completion", TokensOut: 30}, nil
	}
}

func runRescued(t *testing.T, base string, contract core.AgentContract, rescue RescueFunc) ([]PlacedResult, Summary) {
	t.Helper()
	return runRescuedWith(t, base, contract, rescue, neverLocal(t))
}

// runRescuedWith is runRescued with a local runner, for a result that fails
// acceptance and so earns the verification retry on the local seat.
func runRescuedWith(t *testing.T, base string, contract core.AgentContract, rescue RescueFunc, local LocalRunner) ([]PlacedResult, Summary) {
	t.Helper()
	compressPolls(t, 5*time.Millisecond, time.Second)
	results, sum, err := RunWith(t.Context(), testCfg(t), local, []core.AgentContract{contract}, "remote", []string{base}, &RunOptions{Rescue: rescue})
	if err != nil {
		t.Fatalf("RunWith: %v", err)
	}
	return results, sum
}

// The 0.140.12 shape is rescued on the delegator (PR-4): the finished answer is
// re-packed on this box, acceptance runs over the object, and the subtask is a
// success rather than lost work. 80 of 80 re-packs the fleet's nodes killed on
// 2026-09-29 carried such an answer.
func TestRunRemoteLegacyRepackDeferIsRescued(t *testing.T) {
	wire := legacyRepackStall()
	rs := &rescuer{structured: `{"answer":"42"}`}
	results, sum := runRescued(t, rescueNode(t, wire), rescueContract(), rs.fn())

	if sum.Succeeded != 1 || sum.Deferred != 0 || sum.LostToStack != 0 || sum.Infrastructure != 0 || sum.FailedVerification != 0 {
		t.Fatalf("summary = %+v, want the rescued subtask counted as a success", sum)
	}
	r := results[0]
	if r.Result.Deferred || r.Result.DeferClass != "" || r.Result.Reason != "" {
		t.Fatalf("result still deferred: %+v", r.Result)
	}
	if string(r.Result.Structured) != `{"answer":"42"}` {
		t.Fatalf("structured = %s", r.Result.Structured)
	}
	if !strings.Contains(r.Result.RepackNote, "rescued on local-seat (one re-pack completion)") || !strings.Contains(r.Result.RepackNote, "stalled: no progress for 120s in repack") {
		t.Fatalf("repack_note = %q, want the rescue and the node's original reason", r.Result.RepackNote)
	}
	if len(r.AcceptanceFailures) != 0 {
		t.Fatalf("acceptance_failures = %v", r.AcceptanceFailures)
	}
	if rs.calls.Load() != 1 || rs.gotOutput.Load() != wire.Output {
		t.Fatalf("rescue calls=%d, want one over the node's finished answer", rs.calls.Load())
	}
	if r.Result.TokensOut != 661+30 {
		t.Fatalf("tokens_out = %d, want the node's 661 plus the rescue's 30", r.Result.TokensOut)
	}
	if got := time.Duration(rs.gotBudget.Load()); got < rescueFloor {
		t.Fatalf("rescue budget = %s, want at least the %s floor", got, rescueFloor)
	}
	// Published, and NOT an error for the calling model.
	resp := WireResponse(results, sum, nil)
	if resp.Results[0].Deferred || resp.Summary.LostToStack != 0 || string(resp.Results[0].Structured) != `{"answer":"42"}` {
		t.Fatalf("published result = %+v summary=%+v", resp.Results[0], resp.Summary)
	}
}

// A node that publishes the flag is rescued the same way, whatever class and
// reason it filed (here: a cut attempt, class budget).
func TestRunSchemaMissRescuedIsNotLostToStack(t *testing.T) {
	wire := legacyRepackStall()
	wire.SchemaMiss = true
	wire.DeferClass = core.DeferClassBudget
	wire.Reason = "structured re-pack attempt 3/3 cut by its 2m56s share of the wall"
	rs := &rescuer{structured: `{"answer":"42"}`}
	results, sum := runRescued(t, rescueNode(t, wire), rescueContract(), rs.fn())
	if sum.Succeeded != 1 || sum.LostToStack != 0 || results[0].Result.SchemaMiss {
		t.Fatalf("summary = %+v schema_miss=%v, want a delivered success with the flag cleared", sum, results[0].Result.SchemaMiss)
	}
}

// The guard, and the reason the rescue rewrites nothing on failure: when the
// rescue cannot produce an object, the node's defer stands and is still counted
// as lost work — with a note that a rescue was tried. So does a delegator with
// no rescue wired (the behaviour before the rescue existed).
func TestRunSchemaMissUnrescuedStillCountsLostToStack(t *testing.T) {
	for name, rescue := range map[string]RescueFunc{
		"rescue fails":    (&rescuer{err: errors.New("the local seat is not serving")}).fn(),
		"no rescue wired": nil,
	} {
		t.Run(name, func(t *testing.T) {
			results, sum := runRescued(t, rescueNode(t, legacyRepackStall()), rescueContract(), rescue)
			if sum.Deferred != 1 || sum.LostToStack != 1 || sum.Infrastructure != 1 || sum.Succeeded != 0 {
				t.Fatalf("summary = %+v, want the finished answer still counted as lost to the stack", sum)
			}
			r := results[0].Result
			if !r.Deferred || r.DeferClass != core.DeferClassInfrastructure || !strings.HasPrefix(r.Reason, "structured re-pack unreachable") || r.Output == "" || len(r.Structured) != 0 {
				t.Fatalf("the node's defer must stand untouched: %+v", r)
			}
			if rescue != nil && !strings.Contains(r.RepackNote, "rescue on the delegator failed: the local seat is not serving") {
				t.Fatalf("repack_note = %q, want the failed rescue named", r.RepackNote)
			}
		})
	}
}

// The other guard (INV-5): a rescue can never turn an unvalidated answer green.
// Prose is not delivered as structured; an object that breaks the schema or the
// contract's checks is refused or failed, and a rescue that returns nothing is a
// failure to rescue.
func TestRunSchemaMissNeverReadsGreenWithoutCheckedStructured(t *testing.T) {
	cases := []struct {
		name       string
		structured string
		wantDefer  bool // the defer stands
		wantFailed bool // failed_verification
	}{
		{"nothing returned", ``, true, false},
		{"not the schema's type", `{"answer":7}`, true, false},
		{"not an object", `"the answer"`, true, false},
		{"fails the contract's checks", `{"answer":""}`, false, true}, // nonempty:answer
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := &rescuer{structured: tc.structured}
			var localCalls atomic.Int64
			results, sum := runRescuedWith(t, rescueNode(t, legacyRepackStall()), rescueContract(), rs.fn(), failingLocal(&localCalls))
			if sum.Succeeded != 0 {
				t.Fatalf("summary = %+v: an unvalidated or failing object read as a success", sum)
			}
			if got := results[0].Result.Deferred; got != tc.wantDefer {
				t.Fatalf("deferred = %v, want %v (result %+v)", got, tc.wantDefer, results[0].Result)
			}
			if (sum.FailedVerification == 1) != tc.wantFailed {
				t.Fatalf("summary = %+v, want failed_verification=%v", sum, tc.wantFailed)
			}
			if tc.wantDefer && len(results[0].Result.Structured) != 0 {
				t.Fatalf("a refused object was published as structured: %s", results[0].Result.Structured)
			}
		})
	}
}

// A rescue that FAILS records the wall it spent on the result (rescueSpent), whichever
// way it failed — an error, nothing at all, an object the schema refuses — because a
// re-placed seat-down defer is credited it back (admissionCredit). One that delivers, and
// a result nobody tries to rescue, record nothing.
func TestAFailedRescueRecordsTheWallItSpent(t *testing.T) {
	const took = 50 * time.Millisecond
	after := func(structured string, err error) RescueFunc {
		return func(context.Context, core.AgentContract, string, time.Duration) (Rescued, error) {
			time.Sleep(took)
			return Rescued{Structured: json.RawMessage(structured)}, err
		}
	}
	for _, tc := range []struct {
		name      string
		rescue    RescueFunc
		wantSpent bool
	}{
		{"the rescue errors", after("", errors.New("the local seat is not serving")), true},
		{"the rescue returns nothing", after("", nil), true},
		{"the rescue returns an object the schema refuses", after(`{"answer":7}`, nil), true},
		{"the rescue delivers", after(`{"answer":"42"}`, nil), false},
		{"no rescue is wired", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := PlacedResult{Result: seatDownDuringTheRepack("node-a")}
			got := rescueSchemaMiss(context.Background(), tc.rescue, rescueContract(), pr, time.Now(), nil)
			if tc.wantSpent && got.rescueSpent < took {
				t.Fatalf("rescueSpent = %v, want at least the %v the failed rescue took", got.rescueSpent, took)
			}
			if !tc.wantSpent && got.rescueSpent != 0 {
				t.Fatalf("rescueSpent = %v, want none: %s", got.rescueSpent, tc.name)
			}
		})
	}
}

// The rescue is for a finished answer whose structuring failed and nothing else.
func TestRescueRunsOnlyForASchemaMiss(t *testing.T) {
	ok := remoteWire("the answer", `{"answer":"42"}`)
	budget := remoteWire("", "")
	budget.Deferred, budget.DeferClass, budget.Reason = true, core.DeferClassBudget, "ceiling 1800s reached while producing (900 tok at 0.5 tok/s)"
	truncated := legacyRepackStall()
	truncated.OutputTruncated = true
	empty := legacyRepackStall()
	empty.Output = ""
	for name, wire := range map[string]core.AgentWireResult{"a success": ok, "another defer": budget, "a cut answer": truncated, "an empty answer": empty} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int64
			rescue := func(context.Context, core.AgentContract, string, time.Duration) (Rescued, error) {
				calls.Add(1)
				return Rescued{Structured: json.RawMessage(`{"answer":"42"}`)}, nil
			}
			runRescued(t, rescueNode(t, wire), rescueContract(), rescue)
			if calls.Load() != 0 {
				t.Fatalf("the rescue ran for %s", name)
			}
		})
	}
	// A contract with no schema has nothing to structure. It cannot go through the
	// engine at route=remote (remote placement refuses it before any node answers,
	// so the rescue would never be consulted whatever the guard says): the guard
	// inside the rescue is asked directly, with the shape that would otherwise be
	// rescued.
	var calls atomic.Int64
	c := rescueContract()
	c.OutputSchema = nil
	pr := PlacedResult{Result: legacyRepackStall()}
	got := rescueSchemaMiss(context.Background(), func(context.Context, core.AgentContract, string, time.Duration) (Rescued, error) {
		calls.Add(1)
		return Rescued{Structured: json.RawMessage(`{"answer":"42"}`)}, nil
	}, c, pr, time.Now(), nil)
	if calls.Load() != 0 {
		t.Fatal("the rescue ran for a contract with no output_schema")
	}
	if !got.Result.Deferred || len(got.Result.Structured) != 0 || got.Result.RepackNote != pr.Result.RepackNote {
		t.Fatalf("a contract with no schema was touched: %+v", got.Result)
	}
}

// The recognizer, on the shapes that matter: the flag, the legacy shape, and the
// ones that must never be re-packed.
func TestSchemaMissRescuable(t *testing.T) {
	base := func() core.AgentWireResult {
		return core.AgentWireResult{Deferred: true, Output: "an answer", StopReason: "done", DeferClass: core.DeferClassInfrastructure}
	}
	with := func(f func(*core.AgentWireResult)) core.AgentWireResult { w := base(); f(&w); return w }
	cases := []struct {
		name string
		wire core.AgentWireResult
		want bool
	}{
		{"flagged", with(func(w *core.AgentWireResult) { w.SchemaMiss = true }), true},
		{"flagged whatever the stop reason", with(func(w *core.AgentWireResult) { w.SchemaMiss, w.StopReason = true, "" }), true},
		{"legacy stall", with(func(w *core.AgentWireResult) {
			w.Reason = "structured re-pack unreachable: stalled: no progress for 120s in repack"
		}), true},
		{"legacy cut attempt", with(func(w *core.AgentWireResult) {
			w.Reason = "structured re-pack attempt 3/3 cut by its 2m56s share of the wall"
		}), true},
		{"legacy schema failure", with(func(w *core.AgentWireResult) {
			w.Reason = "output failed schema: attempt 3/3 (bound 2m0s): schema validation failed"
		}), true},
		{"legacy but not a finished loop", with(func(w *core.AgentWireResult) { w.Reason, w.StopReason = "structured re-pack unreachable: x", "budget" }), false},
		{"the caller went away", with(func(w *core.AgentWireResult) {
			w.Reason = "canceled during the structured re-pack (the caller's context ended)"
		}), false},
		{"the caller went away, and the node flagged the miss", with(func(w *core.AgentWireResult) {
			w.SchemaMiss, w.Reason = true, core.RepackCanceledReason+" (the caller's context ended)"
		}), false},
		{"another reason", with(func(w *core.AgentWireResult) { w.Reason = "agent loop: connection refused" }), false},
		{"not deferred", with(func(w *core.AgentWireResult) { w.Deferred, w.SchemaMiss = false, true }), false},
		{"already structured", with(func(w *core.AgentWireResult) { w.SchemaMiss, w.Structured = true, json.RawMessage(`{}`) }), false},
		{"no output", with(func(w *core.AgentWireResult) { w.SchemaMiss, w.Output = true, "  \n" }), false},
		{"cut answer", with(func(w *core.AgentWireResult) { w.SchemaMiss, w.OutputTruncated = true, true }), false},
	}
	for _, tc := range cases {
		if got := SchemaMissRescuable(tc.wire); got != tc.want {
			t.Errorf("%s: SchemaMissRescuable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A local placement gets the same rescue: the pipeline that just failed to
// structure the answer hands the delegator the same wire shape.
func TestRunLocalSchemaMissIsRescued(t *testing.T) {
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		w := legacyRepackStall()
		w.SchemaMiss = true
		return w, nil
	}
	rs := &rescuer{structured: `{"answer":"42"}`}
	results, sum, err := RunWith(t.Context(), testCfg(t), local, []core.AgentContract{rescueContract()}, "local", nil, &RunOptions{Rescue: rs.fn()})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 1 || sum.LostToStack != 0 || results[0].Result.Deferred || rs.calls.Load() != 1 {
		t.Fatalf("summary = %+v deferred=%v rescue calls=%d", sum, results[0].Result.Deferred, rs.calls.Load())
	}
}

// The ledger row and the corpus say what the caller received: the rescue runs
// before the row is written, so a rescued subtask is one passing row, not a
// deferred one that was later "fixed" in the published result.
func TestRescuedResultIsWhatTheLedgerRecords(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	cfg := testCfg(t)
	rs := &rescuer{structured: `{"answer":"42"}`}
	_, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{rescueContract()}, "remote", []string{rescueNode(t, legacyRepackStall())}, &RunOptions{Rescue: rs.fn()})
	if err != nil || sum.Succeeded != 1 {
		t.Fatalf("err=%v summary=%+v", err, sum)
	}
	all, err := ledger.ReadAll(cfg.LedgerPath)
	// The dispatch marker (Phase started, ADR 0064) is written before the run and is
	// not the result: count the finished rows only, as every job-row reader does.
	var rows []ledger.Entry
	for _, r := range all {
		if r.Phase != ledger.PhaseStarted {
			rows = append(rows, r)
		}
	}
	if err != nil || len(rows) != 1 {
		t.Fatalf("finished ledger rows = %+v (%v), want one", rows, err)
	}
	if rows[0].Deferred || rows[0].AcceptanceResult != "pass" || rows[0].Reason != "" {
		t.Fatalf("ledger row = deferred:%v acceptance:%q reason:%q, want the rescued success", rows[0].Deferred, rows[0].AcceptanceResult, rows[0].Reason)
	}
}
