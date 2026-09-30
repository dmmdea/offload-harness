package delegate

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// This file pins the delegator half of PR-14 (ADR 0064, register C-68): every
// agent_delegate row names its door, the fleet job id, a closed reason code and
// the whole reason, and a `started` marker row exists from the moment a job is
// handed to a seat.

func readRows(t *testing.T, path string) []ledger.Entry {
	t.Helper()
	rows, err := ledger.ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestAgentDelegateRowCarriesDoorAndFleetJobID: the row a delegated job leaves
// names the surface that admitted it and, when the job went to a fleet node, the
// id that node knows it by — so the node's ledger joins to it on one equality.
func TestAgentDelegateRowCarriesDoorAndFleetJobID(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(int64) (map[string]any, int) {
			return doneWire(t, remoteWire("the answer", `{"answer":"ok"}`)), http.StatusOK
		},
	}
	srv := node.server()

	cfg := testCfg(t)
	named := withdrawContract()
	named.Door = "agent_delegate"
	unnamed := withdrawContract() // a direct engine caller that stamped no door
	results, _, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{named, unnamed}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := readRows(t, cfg.LedgerPath)
	byJob := map[string]ledger.Entry{}
	markers := 0
	for _, e := range rows {
		if e.Phase == ledger.PhaseStarted {
			markers++ // the dispatch marker: one per job, and never a job itself
			continue
		}
		byJob[e.JobID] = e
	}
	if len(byJob) != 2 || markers != 2 {
		t.Fatalf("ledger holds %d finished row(s) and %d marker(s), want 2 and 2 (one of each per job)", len(byJob), markers)
	}
	for i, r := range results {
		row, ok := byJob[r.JobID]
		if !ok {
			t.Fatalf("no ledger row for job %s", r.JobID)
		}
		if row.FleetJobID != r.JobID {
			t.Errorf("job %d: fleet_job_id = %q, want the id dispatched to the node (%q)", i, row.FleetJobID, r.JobID)
		}
		wantDoor := "agent_delegate"
		if i == 1 {
			wantDoor = defaultDelegateDoor
		}
		if row.Door != wantDoor {
			t.Errorf("job %d: door = %q, want %q", i, row.Door, wantDoor)
		}
		if row.ReasonCode != ledger.ReasonOK {
			t.Errorf("job %d: reason_code = %q, want ok on a completed job", i, row.ReasonCode)
		}
	}
}

// TestLocalRowCarriesADoorButNoFleetJobID: a job that never left this box has no
// id on any node, and says so by omitting the column.
func TestLocalRowCarriesADoorButNoFleetJobID(t *testing.T) {
	pairAppDir(t)
	cfg := testCfg(t)
	cfg.Endpoint = "http://127.0.0.1:11434"
	local := LocalRunner(func(ctx context.Context, ac core.AgentContract, opts LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: "done", Seat: "local-seat"}, nil
	})
	c := core.AgentContract{Goal: "say done", Door: "cli:delegate"}
	if _, _, err := RunWith(context.Background(), cfg, local, []core.AgentContract{c}, "local", nil, nil); err != nil {
		t.Fatal(err)
	}
	rows := readRows(t, cfg.LedgerPath)
	// The fake local runner writes no inner row, so the ledger holds the marker
	// and the finished row.
	var finished []ledger.Entry
	for _, e := range rows {
		if e.Phase == "" {
			finished = append(finished, e)
		}
	}
	if len(finished) != 1 {
		t.Fatalf("finished rows = %d (%+v), want 1", len(finished), rows)
	}
	if e := finished[0]; e.Door != "cli:delegate" || e.FleetJobID != "" || e.ReasonCode != ledger.ReasonOK {
		t.Fatalf("row = door %q fleet_job_id %q reason_code %q, want cli:delegate / none / ok", e.Door, e.FleetJobID, e.ReasonCode)
	}
}

// TestFullReasonReachesTheDelegateRow: the row's reason is the reason the result
// published — the whole of it, with the code beside it — not its first 120 bytes.
func TestFullReasonReachesTheDelegateRow(t *testing.T) {
	compressQueueBudget(t)
	stuck := stuckNode(t, "node-stuck")
	url := (&withdrawProbe{}).front(t, stuck.server()).URL // no DELETE route: an old node, so the deadline stays a plain failure

	cfg := testCfg(t)
	contract := withdrawContract()
	contract.TimeoutSec = 30
	results, _, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	full := results[0].Err
	if len(full) <= 120 {
		t.Fatalf("fixture: the queue-deadline message is only %d bytes; this test needs one past the old cut", len(full))
	}
	var row ledger.Entry
	for _, e := range readRows(t, cfg.LedgerPath) {
		if e.Phase == "" {
			row = e
		}
	}
	if row.Reason != full {
		t.Fatalf("ledger reason = %q\nwant the whole %q", row.Reason, full)
	}
	if row.ReasonCode != ledger.ReasonQueueDeadline {
		t.Fatalf("reason_code = %q, want %q", row.ReasonCode, ledger.ReasonQueueDeadline)
	}
}

// TestReasonCodeIsAlwaysSetOnEveryOutcomeShape: the classifier is total over what
// the delegator can produce — every shape maps to a member of the closed set, and
// to the RIGHT member for the shapes that have one.
func TestReasonCodeIsAlwaysSetOnEveryOutcomeShape(t *testing.T) {
	stall := func(s string) PlacedResult {
		return PlacedResult{Result: core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: s}}
	}
	deferred := func(class, reason string) PlacedResult {
		return PlacedResult{Result: core.AgentWireResult{Deferred: true, DeferClass: class, Reason: reason}}
	}
	failed := func(err string) PlacedResult { return PlacedResult{Err: err} }
	refused := func(status int, err string) PlacedResult {
		return PlacedResult{Err: err, refused: true, refusalStatus: status}
	}
	// ownCapacityWait is what the delegator's own capacity wait produces, built by the
	// real constructor rather than typed by hand, so the classifier is anchored to the
	// shape production creates: a deferred capacity result marked Unplaced.
	ownCapacityWait := func() PlacedResult {
		pr := (&runner{cfg: config.Config{AgentPlacementWaitSec: 120}}).capacityDefer(
			NodeView{NodeID: "this-box", AgentSeat: "seat"}, PlacedResult{JobID: "agd-1"}, time.Second, 2*time.Minute, []string{"node-a: full"}, waitEvidence{})
		if !pr.Unplaced || !pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassCapacity {
			t.Fatalf("capacityDefer no longer builds a deferred, unplaced capacity result: %+v", pr)
		}
		return pr
	}
	cases := []struct {
		name string
		pr   PlacedResult
		want string
	}{
		{"completed", PlacedResult{}, ledger.ReasonOK},
		{"acceptance failed", PlacedResult{AcceptanceFailures: []string{"contains:x"}}, ledger.ReasonFailedVerification},

		{"queue deadline", failed("queue deadline after 5m0s: the node accepted the job but never started it"), ledger.ReasonQueueDeadline},
		{"queue deadline withdrawn", PlacedResult{Err: "queue deadline after 5m0s: ...; the job was withdrawn", withdrawn: true, refused: true, refusalStatus: 503}, ledger.ReasonQueueWithdrawn},
		{"exhausted after a withdrawal", PlacedResult{Err: "placement refused: 1 node(s) refused this subtask (node-a: queue deadline ...)", withdrawn: true, refused: true, refusalStatus: 503}, ledger.ReasonQueueWithdrawn},
		{"queue route deadline", failed("queue wait deadline after 5m0s: no node claimed the job"), ledger.ReasonQueueDeadline},
		{"canceled", failed("canceled: context canceled"), ledger.ReasonCanceled},

		{"placement refused, capacity", refused(503, "placement refused: 2 node(s) refused this subtask and none of them ran it"), ledger.ReasonQueueFull},
		{"placement refused, 429", refused(429, "dispatch http://x: status 429: slow down"), ledger.ReasonQueueFull},
		{"placement refused, unreachable", refused(0, "placement refused: 1 node(s) refused this subtask (dispatch http://x: dial tcp: refused)"), ledger.ReasonNodeUnreachable},
		{"dispatch refused, auth", refused(401, "dispatch http://x/fleet/dispatch: status 401: unauthorized"), ledger.ReasonDispatchRefused},
		{"dispatch refused, 400", refused(400, "dispatch http://x/fleet/dispatch: status 400: bad envelope"), ledger.ReasonDispatchRefused},
		{"poll 401", failed("poll: 401 unauthorized (fleet_auth_token mismatch)"), ledger.ReasonDispatchRefused},
		{"queue poll 401", failed("queue poll: 401 unauthorized (fleet_auth_token mismatch)"), ledger.ReasonDispatchRefused},

		{"never answered", failed("poll deadline after 1m0s: node never answered (last: dial tcp: refused)"), ledger.ReasonNodeUnreachable},
		{"denied the job", failed("poll deadline after 1m0s: the node answered but never reported owning the job — a poll 404 DENIES it ever held it (2 re-dispatch(es) made)"), ledger.ReasonJobLost},
		{"answered but unusable", failed("poll deadline after 1m0s: the node answered but never reported owning the job, and never denied holding it either"), ledger.ReasonRemoteError},
		{"lost repeatedly", failed("node lost job agd-1 3 times (poll 404 after re-dispatch)"), ledger.ReasonJobLost},
		{"holder denies", failed("holder denies the job (submitted then vanished — holder store reset?)"), ledger.ReasonJobLost},
		{"remote job error", failed("remote job error: interrupted"), ledger.ReasonRemoteError},
		{"undecodable result", failed("job done but data is not an AgentWireResult: bad json"), ledger.ReasonRemoteError},
		{"queue job failed", failed("queue job failed: boom"), ledger.ReasonRemoteError},
		{"local run failed", failed("local run: boom"), ledger.ReasonInfrastructure},
		{"no local runner", failed("no local runner wired (delegator surfaces must supply one)"), ledger.ReasonConfig},
		{"refused before placement", PlacedResult{Err: "contract: goal required", PlacementReason: "refused before placement"}, ledger.ReasonContract},
		{"something new", failed("a failure shape nobody wrote a code for"), ledger.ReasonOther},

		{"poll deadline defer", deferred(core.DeferClassBudget, "poll deadline after 5m0s: node accepted the job but did not reach a terminal state"), ledger.ReasonPollDeadline},
		{"poll deadline defer, infrastructure", deferred(core.DeferClassInfrastructure, "poll deadline after 5m0s: node accepted the job but did not reach a terminal state (last poll error: eof)"), ledger.ReasonPollDeadline},
		{"stall in prefill", stall("stalled: no progress for 300s in prefill (allowed 240s; 0 tok so far)"), ledger.ReasonStallPrefill},
		{"stall in decoding", stall("stalled: no progress for 90s in decoding (allowed 60s; 812 tok so far)"), ledger.ReasonStallDecode},
		{"stall in admission", stall("stalled: no progress for 300s in admission (allowed 300s; 0 tok so far)"), ledger.ReasonStallAdmission},
		{"stall in repack", stall("stalled: no progress for 120s in repack (allowed 120s; 900 tok so far)"), ledger.ReasonStallRepack},
		{"stall in tool", stall("stalled: no progress for 120s in tool (allowed 120s; 5 tok so far)"), ledger.ReasonStallTool},
		{"stall in cold load", stall("stalled: seat still loading after 400s in cold-load (allowed 300s; 0 tok so far)"), ledger.ReasonStallColdLoad},
		{"stall, engine flat", stall("stalled: the seat's engine did no work for 120s while this request waited in prefill (allowed 60s; engine: x; 0 tok so far)"), ledger.ReasonStallEngine},
		{"stall, engine thrash", stall("stalled: the seat's engine kept stepping but produced no token for 360s while this request waited in decoding — a preempt-and-recompute thrash"), ledger.ReasonStallEngine},
		{"stall, unnamed phase", stall("stalled: no progress for 60s"), ledger.ReasonStallOther},
		{"seat unreachable", stall("agent loop: Post \"http://127.0.0.1:1/v1/chat/completions\": dial tcp 127.0.0.1:1: connect: connection refused"), ledger.ReasonSeatDown},
		{"infrastructure, generic", stall("building agent: boom"), ledger.ReasonInfrastructure},
		{"shed", PlacedResult{shed: true, Result: core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassCapacity, Reason: "shed (priority -1): ..."}}, ledger.ReasonShed},
		// The delegator's own placement outcome — a result NO node ran — versus a node
		// that took the job and answered a capacity defer after admission. Two
		// diagnosed root causes (the fleet had no room; a seat sat at its run cap) with
		// different fixes: one code each, so a reader never reads prose to tell them apart.
		{"capacity wait", ownCapacityWait(), ledger.ReasonCapacityWait},
		{"placement decision, capacity", PlacedResult{Unplaced: true, Result: core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassCapacity, Reason: "layer fast refused by display-floor: ..."}}, ledger.ReasonCapacityWait},
		{"node: seat at its run cap", deferred(core.DeferClassCapacity, "seat busy: seat agent-pool is at its local run cap (2 running)"), ledger.ReasonNodeBusy},
		{"node: gpu lease or fence", deferred(core.DeferClassCapacity, "gpu busy: exclusive text lease held; no new run is admitted on this box until it is released"), ledger.ReasonNodeBusy},
		{"node: engine held busy", deferred(core.DeferClassCapacity, "wall timeout: the run was held behind the seat's other work"), ledger.ReasonNodeBusy},
		{"no eligible remote", PlacedResult{Unplaced: true, Result: core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "route=remote: every remote failed the health probe"}}, ledger.ReasonNoEligibleNode},
		{"budget", deferred(core.DeferClassBudget, "wall timeout after 300s"), ledger.ReasonBudget},
		{"abstention", deferred(core.DeferClassAbstention, "the seat declined"), ledger.ReasonAbstention},
		{"contract", deferred(core.DeferClassContract, "no output_schema"), ledger.ReasonContract},
		{"config", deferred(core.DeferClassConfig, "no agent seat resolvable"), ledger.ReasonConfig},
		{"write", deferred(core.DeferClassWrite, "write root refused"), ledger.ReasonWrite},
		{"defer with no class", deferred("", "who knows"), ledger.ReasonOther},
	}
	for _, c := range cases {
		got := reasonCodeFor(c.pr)
		if !ledger.IsReasonCode(got) {
			t.Errorf("%s: code %q is not in the closed set", c.name, got)
		}
		if got != c.want {
			t.Errorf("%s: code = %q, want %q", c.name, got, c.want)
		}
	}
	// Total over combinations too: no mix of the flags can produce an empty code.
	for _, err := range []string{"", "x", "queue deadline after 1s", "canceled: c", "placement refused: x"} {
		for _, deferredFlag := range []bool{false, true} {
			for _, status := range []int{0, 400, 503} {
				pr := PlacedResult{Err: err, refused: status != 0, refusalStatus: status, withdrawn: status == 503}
				pr.Result.Deferred = deferredFlag
				if code := reasonCodeFor(pr); !ledger.IsReasonCode(code) {
					t.Fatalf("reasonCodeFor(%+v) = %q, not a member of the closed set", pr, code)
				}
			}
		}
	}
}

// TestStartedRowIsWrittenAtDispatch: from the moment a node acks the job there is
// a marker row in the ledger — before the job ends, so a hang or a ghost is
// visible while it runs — and the marker is never counted as a job. A dispatch
// the node REFUSES leaves nothing behind: there is no job anywhere.
func TestStartedRowIsWrittenAtDispatch(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	release := make(chan struct{})
	var polls atomic.Int64
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(int64) (map[string]any, int) {
			polls.Add(1)
			select {
			case <-release:
				return doneWire(t, remoteWire("the answer", `{"answer":"ok"}`)), http.StatusOK
			default:
				return map[string]any{"state": "running"}, http.StatusOK
			}
		},
	}
	srv := node.server()
	cfg := testCfg(t)

	type outcome struct {
		results []PlacedResult
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		contract := withdrawContract()
		contract.Door = "agent_delegate"
		res, _, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{srv.URL})
		done <- outcome{res, err}
	}()

	// Wait until the delegator is polling a running job, then read the ledger.
	deadline := time.Now().Add(5 * time.Second)
	for polls.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatal("the delegator never started polling")
		}
		time.Sleep(2 * time.Millisecond)
	}
	rows := readRows(t, cfg.LedgerPath)
	if len(rows) != 1 || rows[0].Phase != ledger.PhaseStarted {
		t.Fatalf("mid-run ledger = %+v, want exactly one row: the started marker", rows)
	}
	m := rows[0]
	jobID, _ := node.lastJobID.Load().(string)
	if m.Task != "agent_delegate" || m.JobID != jobID || m.FleetJobID != jobID || m.Door != "agent_delegate" ||
		m.Route != "remote" || m.ReasonCode != ledger.ReasonStarted || !strings.HasPrefix(m.ModelTier, "fake-node:") {
		t.Fatalf("marker = %+v, want task agent_delegate, job and fleet job id %q, door agent_delegate, route remote, code started, on fake-node", m, jobID)
	}

	close(release)
	out := <-done
	if out.err != nil {
		t.Fatalf("Run: %v", out.err)
	}
	all := readRows(t, cfg.LedgerPath)
	if len(all) != 2 {
		t.Fatalf("ledger rows = %d, want the marker and the finished row", len(all))
	}
	if jr := ledger.JobRows(all); len(jr) != 1 || jr[0].Phase != "" || jr[0].JobID != jobID {
		t.Fatalf("JobRows = %+v, want exactly the finished row of %s", jr, jobID)
	}
	s, err := ledger.SummarizeFile(cfg.LedgerPath, 0, ledger.DefaultPrices)
	if err != nil || s.Calls != 1 {
		t.Fatalf("Summarize = %+v (%v), want 1 call: the marker is not a job", s, err)
	}

	// A refused dispatch: nothing was handed to any seat, so nothing is marked.
	refusing, refusingURL := refusingNode(t, "node-full", http.StatusServiceUnavailable, nil)
	cfg2 := testCfg(t)
	if _, _, err := Run(t.Context(), cfg2, neverLocal(t), []core.AgentContract{withdrawContract()}, "remote", []string{refusingURL}); err != nil {
		t.Fatalf("Run against a refusing node: %v", err)
	}
	for _, e := range readRows(t, cfg2.LedgerPath) {
		if e.Phase == ledger.PhaseStarted {
			t.Fatalf("a refused dispatch left a started marker: %+v", e)
		}
	}
	if refusing.dispatches.Load() == 0 {
		t.Fatal("the refusing node was never asked; this half proved nothing")
	}
}

// TestStartedRowIsWrittenForALocalRun: a run on this box's own seat is handed to
// the runner at once, and a runner that wedges is exactly the hang the marker
// exists to show.
func TestStartedRowIsWrittenForALocalRun(t *testing.T) {
	pairAppDir(t)
	cfg := testCfg(t)
	cfg.Endpoint = "http://127.0.0.1:11434"
	release := make(chan struct{})
	entered := make(chan struct{})
	local := LocalRunner(func(ctx context.Context, ac core.AgentContract, opts LocalOptions) (core.AgentWireResult, error) {
		close(entered)
		<-release
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: "done", Seat: "local-seat"}, nil
	})
	done := make(chan error, 1)
	go func() {
		_, _, err := RunWith(context.Background(), cfg, local, []core.AgentContract{{Goal: "say done", Door: "cli:delegate"}}, "local", nil, nil)
		done <- err
	}()
	<-entered
	rows := readRows(t, cfg.LedgerPath)
	if len(rows) != 1 || rows[0].Phase != ledger.PhaseStarted || rows[0].Route != "local" || rows[0].Door != "cli:delegate" || rows[0].FleetJobID != "" {
		t.Fatalf("mid-run ledger = %+v, want one local started marker with no fleet job id", rows)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestInnerAgentRowCarriesParentJobID (register C-62 kept intact under ADR 0064):
// the delegator hands the runner its own job id as the parent, the runner's row
// for that run is written as an INNER row of it, and with the delegator's dispatch
// marker and finished row beside it the ledger still counts ONE job — the finished
// row — not three. The runner here writes the inner row the way the pipeline does
// for a run it executes; the real writer is pinned in internal/pipeline.
func TestInnerAgentRowCarriesParentJobID(t *testing.T) {
	pairAppDir(t)
	cfg := testCfg(t)
	cfg.Endpoint = "http://127.0.0.1:11434"
	var handed string
	local := LocalRunner(func(ctx context.Context, ac core.AgentContract, opts LocalOptions) (core.AgentWireResult, error) {
		handed = opts.ParentJobID
		led, err := ledger.Open(cfg.LedgerPath)
		if err != nil {
			t.Errorf("runner could not open the ledger: %v", err)
		} else {
			defer led.Close()
			if rerr := led.Record(ledger.Entry{Task: "agent", JobID: "agent-local-1", ParentJobID: opts.ParentJobID, TokensIn: 500, TokensOut: 30}); rerr != nil {
				t.Errorf("runner's inner row: %v", rerr)
			}
		}
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: "done", Seat: "local-seat"}, nil
	})
	results, _, err := RunWith(context.Background(), cfg, local, []core.AgentContract{{Goal: "say done", Door: "cli:delegate"}}, "local", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	jobID := results[0].JobID
	if handed == "" || handed != jobID {
		t.Fatalf("the runner was handed parent job id %q, want the delegator's job id %q", handed, jobID)
	}
	rows := readRows(t, cfg.LedgerPath)
	var marker, inner, finished int
	for _, e := range rows {
		switch {
		case e.Phase == ledger.PhaseStarted:
			marker++
		case e.Task == "agent":
			inner++
			if e.ParentJobID != jobID || e.CardsTokens != 0 {
				t.Fatalf("inner row = parent %q cards_tokens %d, want parent %q and 0 (the parent row carries the card work)", e.ParentJobID, e.CardsTokens, jobID)
			}
		default:
			finished++
		}
	}
	if marker != 1 || inner != 1 || finished != 1 {
		t.Fatalf("ledger holds %d marker(s), %d inner row(s), %d finished row(s), want one of each", marker, inner, finished)
	}
	jobs := ledger.JobRows(rows)
	if len(jobs) != 1 || jobs[0].Task != "agent_delegate" || jobs[0].JobID != jobID {
		t.Fatalf("JobRows = %+v, want exactly the delegator's finished row for %s", jobs, jobID)
	}
	if s, serr := ledger.SummarizeFile(cfg.LedgerPath, 0, ledger.DefaultPrices); serr != nil || s.Calls != 1 {
		t.Fatalf("Summarize = %+v (%v), want 1 call: a local job is counted once", s, serr)
	}
}

// TestOnlyAJobANodeAckedCarriesAFleetJobID: fleet_job_id is "the id the fleet node
// knows the job by". A dispatch the node REFUSED left no job on any node, and a
// row that named one anyway read, to a join from the delegator's side, as a job
// the node lost (232 of the 836 attempts on 2026-09-29 were refusals). The id is
// on a finished row exactly when a started marker was written for it: the ack.
func TestOnlyAJobANodeAckedCarriesAFleetJobID(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)

	finishedAndMarkers := func(t *testing.T, path string) (finished []ledger.Entry, markers map[string]bool) {
		t.Helper()
		markers = map[string]bool{}
		for _, e := range readRows(t, path) {
			if e.Phase == ledger.PhaseStarted {
				markers[e.FleetJobID] = true
			} else {
				finished = append(finished, e)
			}
		}
		return finished, markers
	}

	t.Run("a dispatch the node refused", func(t *testing.T) {
		_, url := refusingNode(t, "node-full", http.StatusServiceUnavailable, nil)
		cfg := testCfg(t)
		if _, _, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{withdrawContract()}, "remote", []string{url}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		finished, markers := finishedAndMarkers(t, cfg.LedgerPath)
		if len(finished) != 1 || len(markers) != 0 {
			t.Fatalf("ledger = %d finished row(s) and %d marker(s), want 1 and 0", len(finished), len(markers))
		}
		row := finished[0]
		if row.ReasonCode != ledger.ReasonQueueFull || row.JobID == "" {
			t.Fatalf("row = reason_code %q job_id %q, want %q and a job id", row.ReasonCode, row.JobID, ledger.ReasonQueueFull)
		}
		if row.FleetJobID != "" {
			t.Fatalf("fleet_job_id = %q on a dispatch the node refused: no node ever held that id", row.FleetJobID)
		}
	})

	t.Run("a job the node acked and then failed", func(t *testing.T) {
		node := &fakeNode{
			t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: "node-broken",
			pollState: func(int64) (map[string]any, int) {
				return map[string]any{"state": "error", "error": "the seat exploded"}, http.StatusOK
			},
		}
		url := node.server().URL
		cfg := testCfg(t)
		if _, _, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{withdrawContract()}, "remote", []string{url}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		finished, markers := finishedAndMarkers(t, cfg.LedgerPath)
		jobID, _ := node.lastJobID.Load().(string)
		if len(finished) != 1 || finished[0].FleetJobID != jobID || jobID == "" {
			t.Fatalf("finished = %+v, want one row whose fleet_job_id is the acked job %q", finished, jobID)
		}
		if !markers[jobID] {
			t.Fatalf("no started marker for %s: a finished row and its marker name the same id", jobID)
		}
	})
}
