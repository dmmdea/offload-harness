// Task 6 (multi-node agent delegation): delegate.Run — the shared execution
// engine both delegator surfaces (MCP agent_delegate, the CLI verb) call. The
// fleet node is faked over httptest (dispatch + jobs, the real wire shapes).
//
// Load-bearing pins: delegator-side acceptance flips a schema-valid result to
// failed-verification (the wrong-valid-schema hole this engine exists to
// close); the poll deadline (TimeoutSec + grace) stops polling and yields a
// "poll deadline" DEFER only when the node actually answered — a node that
// never answered is a FAILURE, never a defer authored on its behalf; a lost job (poll 404) is
// re-dispatched under the SAME delegator-minted job id (202-reack semantics);
// and telemetry lands in both the delegation-log corpus and the ledger.

package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// compressPolls shrinks the poll cadence/grace for a test and restores them.
func compressPolls(t *testing.T, every, grace time.Duration) {
	t.Helper()
	oldEvery, oldGrace := pollEvery, pollGrace
	pollEvery, pollGrace = every, grace
	t.Cleanup(func() { pollEvery, pollGrace = oldEvery, oldGrace })
}

// fakeNode is one scripted fleet node: health advertises the agent lane,
// dispatch acks 202 (recording every POST), jobs answers per pollState.
type fakeNode struct {
	t            *testing.T
	token        string // expected bearer; "" = no auth
	agentEnabled bool
	resident     bool
	ctxTokens    int
	nodeID       string

	// pollState returns (the jobWire body, HTTP status) for the nth poll (1-based).
	pollState func(n int64) (map[string]any, int)
	// pollByJob, when set, wins over pollState: a fan-out test needs to answer
	// PER JOB (each subtask gets its own result) rather than per poll ordinal.
	pollByJob func(jobID string, n int64) (map[string]any, int)
	// onDispatch, when set, runs inside the dispatch handler with the decoded
	// job id and contract — the seam a concurrency test uses to hold a slot and
	// sample how many dispatches overlap.
	onDispatch func(jobID string, contract core.AgentContract)
	// dispatchStatus overrides the 202 ack; 0 = normal ack.
	dispatchStatus int
	// killOnPoll models a node that DIES after acking: the first poll's
	// connection is hijacked and dropped with no HTTP answer at all, and the
	// server itself goes down, so every later poll is refused at connect.
	killOnPoll bool

	dispatches atomic.Int64
	polls      atomic.Int64
	lastJobID  atomic.Value // string
}

func (f *fakeNode) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node_id":             f.nodeID,
			"queue_depth":         0,
			"agent_seat":          "remote-seat",
			"agent_ctx_tokens":    f.ctxTokens,
			"agent_seat_resident": f.resident,
			"agent_enabled":       f.agentEnabled,
		})
	})
	mux.HandleFunc("POST /fleet/dispatch", func(w http.ResponseWriter, r *http.Request) {
		f.dispatches.Add(1)
		if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": "unauthorized"})
			return
		}
		if f.dispatchStatus != 0 {
			w.WriteHeader(f.dispatchStatus)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": "scripted refusal"})
			return
		}
		var env struct {
			JobID    string          `json:"job_id"`
			TaskType string          `json:"task_type"`
			Payload  json.RawMessage `json:"payload"`
		}
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			f.t.Errorf("dispatch body: %v", err)
		}
		if env.TaskType != "agent" {
			f.t.Errorf("task_type = %q, want agent", env.TaskType)
		}
		if !strings.HasPrefix(env.JobID, "agd-") {
			f.t.Errorf("job_id = %q, want the delegator-minted agd- prefix", env.JobID)
		}
		// The payload must be a real v1 contract — decode it exactly as the
		// node would (schema_version check included).
		contract, err := core.DecodeAgentContract(strings.NewReader(string(env.Payload)))
		if err != nil {
			f.t.Errorf("payload is not a dispatchable contract: %v", err)
		}
		f.lastJobID.Store(env.JobID)
		if f.onDispatch != nil {
			f.onDispatch(env.JobID, contract)
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"job_id": env.JobID, "status": "accepted"})
	})
	var srv *httptest.Server
	mux.HandleFunc("GET /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		n := f.polls.Add(1)
		if f.killOnPoll {
			// No status line, no body: the caller sees a transport error, never
			// an HTTP answer. Then take the listener down so subsequent polls
			// are refused at connect (the "node died mid-poll" shape).
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, herr := hj.Hijack(); herr == nil {
					_ = conn.Close()
				}
			}
			if n == 1 {
				go srv.Close() // in a goroutine: Close waits for handlers to return
			}
			return
		}
		var body map[string]any
		var status int
		if f.pollByJob != nil {
			body, status = f.pollByJob(r.PathValue("id"), n)
		} else {
			body, status = f.pollState(n)
		}
		if body != nil {
			if _, has := body["job_id"]; !has {
				body["job_id"] = r.PathValue("id")
			}
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
	srv = httptest.NewServer(mux)
	f.t.Cleanup(srv.Close)
	return srv
}

// doneWire wraps an AgentWireResult in the jobs endpoint's done shape.
func doneWire(t *testing.T, w core.AgentWireResult) map[string]any {
	t.Helper()
	data, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"state": "done", "data": json.RawMessage(data)}
}

func remoteWire(output string, structured string) core.AgentWireResult {
	w := core.AgentWireResult{
		SchemaVersion: core.AgentWireSchemaVersion,
		NodeID:        "fake-node",
		Seat:          "remote-seat",
		Output:        output,
		Steps:         1,
		StopReason:    "done",
		WallMs:        42,
		TokensOut:     7,
	}
	if structured != "" {
		w.Structured = json.RawMessage(structured)
	}
	return w
}

// testCfg roots every side effect (delegation-log, ledger) in a temp dir.
func testCfg(t *testing.T) config.Config {
	home := t.TempDir()
	return config.Config{
		Home:       home,
		LedgerPath: filepath.Join(home, "ledger.jsonl"),
		AgentModel: "local-seat",
	}
}

func remoteContract() core.AgentContract {
	return core.AgentContract{
		SchemaVersion: core.AgentWireSchemaVersion,
		Goal:          "answer the question",
		OutputSchema:  json.RawMessage(`{"properties":{"answer":{"type":"string"}}}`),
		Acceptance:    []string{"contains:qube", "nonempty:answer"},
		MaxSteps:      4,
		TimeoutSec:    30,
	}
}

// neverLocal is a LocalRunner that fails the test if the engine falls local.
func neverLocal(t *testing.T) LocalRunner {
	return func(ctx context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		t.Error("local runner called; this test expects a remote placement")
		return core.AgentWireResult{}, nil
	}
}

func TestRunRemoteHappyPathWithAcceptance(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	node := &fakeNode{
		t: t, token: "sekrit", agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(n int64) (map[string]any, int) {
			if n == 1 {
				return map[string]any{"state": "running"}, http.StatusOK
			}
			return doneWire(t, remoteWire("the qube answer", `{"answer":"42"}`)), http.StatusOK
		},
	}
	srv := node.server()

	cfg := testCfg(t)
	cfg.FleetAuthToken = "sekrit"
	results, sum, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Succeeded: 1}) {
		t.Fatalf("summary = %+v, want exactly one success", sum)
	}
	r := results[0]
	if r.Err != "" {
		t.Fatalf("unexpected subtask error: %s", r.Err)
	}
	if r.Node != "fake-node" || r.Seat != "remote-seat" {
		t.Errorf("node/seat = %q/%q", r.Node, r.Seat)
	}
	if len(r.AcceptanceFailures) != 0 {
		t.Errorf("acceptance failures = %v, want none", r.AcceptanceFailures)
	}
	if r.Result.Output != "the qube answer" {
		t.Errorf("output = %q", r.Result.Output)
	}
	if !strings.HasPrefix(r.JobID, "agd-") {
		t.Errorf("job id = %q, want the agd- prefix", r.JobID)
	}
	if node.dispatches.Load() != 1 {
		t.Errorf("dispatches = %d, want 1", node.dispatches.Load())
	}

	// Telemetry (roast delta 9): one delegation-log corpus line + one ledger row.
	logDir := filepath.Join(cfg.BaseDir(), "delegation-log")
	entries, derr := os.ReadDir(logDir)
	if derr != nil || len(entries) != 1 {
		t.Fatalf("delegation-log dir: %v (entries %d)", derr, len(entries))
	}
	raw, rerr := os.ReadFile(filepath.Join(logDir, entries[0].Name()))
	if rerr != nil {
		t.Fatal(rerr)
	}
	var line struct {
		JobID          string          `json:"job_id"`
		Node           string          `json:"node"`
		Seat           string          `json:"seat"`
		Placement      string          `json:"placement_reason"`
		Deferred       bool            `json:"deferred"`
		AcceptancePass bool            `json:"acceptance_pass"`
		WallMs         int64           `json:"wall_ms"`
		EstTokens      int             `json:"est_tokens"`
		Contract       json.RawMessage `json:"contract"`
		Result         json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &line); err != nil {
		t.Fatalf("delegation-log line: %v (%s)", err, raw)
	}
	if line.JobID != r.JobID || line.Node != "fake-node" || !line.AcceptancePass || line.Deferred {
		t.Errorf("log line = %+v", line)
	}
	if line.EstTokens <= 0 || line.Placement == "" || len(line.Contract) == 0 || len(line.Result) == 0 {
		t.Errorf("log line missing corpus fields: %+v", line)
	}
	rows, lerr := ledger.ReadAll(cfg.LedgerPath)
	if lerr != nil || len(rows) != 1 {
		t.Fatalf("ledger rows = %d (%v), want 1", len(rows), lerr)
	}
	if rows[0].Task != "agent_delegate" || rows[0].Deferred {
		t.Errorf("ledger row = %+v, want a completed agent_delegate row", rows[0])
	}
	if rows[0].TokensIn != 0 {
		t.Errorf("ledger tokens_in = %d, want 0 (a delegation row must never inflate tokens-saved)", rows[0].TokensIn)
	}
}

// TestRunAcceptanceFailureFlipsToFailedVerification: a schema-VALID result
// that fails a delegator-side acceptance check is NOT a success — this is the
// wrong-valid-schema hole the engine exists to close.
func TestRunAcceptanceFailureFlipsToFailedVerification(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(int64) (map[string]any, int) {
			// Valid schema shape, but the output lacks the required substring.
			return doneWire(t, remoteWire("an unrelated answer", `{"answer":"nope"}`)), http.StatusOK
		},
	}
	srv := node.server()

	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{FailedVerification: 1}) {
		t.Fatalf("summary = %+v, want exactly one failed-verification", sum)
	}
	r := results[0]
	if len(r.AcceptanceFailures) != 1 || !strings.Contains(r.AcceptanceFailures[0], "contains:qube") {
		t.Fatalf("acceptance failures = %v, want the contains:qube failure named", r.AcceptanceFailures)
	}
}

// TestRunPollDeadlineDefers: a node that ANSWERS every poll but never finishes
// is abandoned at TimeoutSec + grace with the "poll deadline" defer (stable
// prefix, honest tail — it acked and never reached a terminal state), and
// polling STOPS. Contrast TestRunPollDeadNodeIsAFailureNotAFabricatedDefer:
// only a node that actually answered may be reported as having deferred.
func TestRunPollDeadlineDefers(t *testing.T) {
	compressPolls(t, 20*time.Millisecond, 100*time.Millisecond)
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(int64) (map[string]any, int) {
			return map[string]any{"state": "running"}, http.StatusOK
		},
	}
	srv := node.server()

	contract := remoteContract()
	contract.TimeoutSec = 1 // deadline = 1s + the compressed 100ms grace
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Deferred: 1}) {
		t.Fatalf("summary = %+v, want exactly one defer", sum)
	}
	r := results[0]
	if !r.Result.Deferred || !strings.HasPrefix(r.Result.Reason, "poll deadline") {
		t.Fatalf("deferred/reason = %v/%q, want the poll-deadline defer", r.Result.Deferred, r.Result.Reason)
	}
	if r.Err != "" {
		t.Fatalf("err = %q, want none — this node answered every poll", r.Err)
	}
	if r.Result.DeferClass != core.DeferClassBudget {
		t.Errorf("defer_class = %q, want %q — the node was alive and simply out of clock", r.Result.DeferClass, core.DeferClassBudget)
	}
	polled := node.polls.Load()
	time.Sleep(150 * time.Millisecond)
	if node.polls.Load() != polled {
		t.Error("polling continued after the deadline; the delegator must STOP")
	}
}

// TestRunPollUnusableAnswersAreInfrastructure (C-1): a node answering 503 to
// every poll is not a slow node. The status used to fall through the switch
// unread, so the run ended in the same "poll deadline" defer a healthy-but-slow
// node produces. It must keep polling (the state may still resolve) but end
// classed infrastructure, with the 503 named in the reason.
func TestRunPollUnusableAnswersAreInfrastructure(t *testing.T) {
	compressPolls(t, 20*time.Millisecond, 100*time.Millisecond)
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(int64) (map[string]any, int) {
			return map[string]any{"status": "error", "error": "vram snapshot stale"}, http.StatusServiceUnavailable
		},
	}
	srv := node.server()

	contract := remoteContract()
	contract.TimeoutSec = 1
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Deferred: 1, Infrastructure: 1}) {
		t.Fatalf("summary = %+v, want the defer counted as infrastructure", sum)
	}
	if r := results[0]; !strings.Contains(r.Result.Reason, "503") {
		t.Errorf("reason = %q, want the unusable 503 answers named", r.Result.Reason)
	}
}

// TestRunPollDeadNodeIsAFailureNotAFabricatedDefer (C-1): a node that dies
// after acking — every poll dies on the wire, so the node never answers about
// the job — must land in Summary.Failed. The old code manufactured an
// AgentWireResult{Deferred:true} for it and let runOne stamp the CHOSEN node's
// id and seat onto it, so a dead node published a result reading "fake-node /
// remote-seat ran the contract and reported it could not finish" — a sentence
// nobody on that node ever said, delivered with a zero CLI exit.
func TestRunPollDeadNodeIsAFailureNotAFabricatedDefer(t *testing.T) {
	compressPolls(t, 20*time.Millisecond, 100*time.Millisecond)
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		killOnPoll: true,
		pollState:  func(int64) (map[string]any, int) { return map[string]any{"state": "running"}, http.StatusOK },
	}
	srv := node.server()

	contract := remoteContract()
	contract.TimeoutSec = 1 // deadline = 1s + the compressed 100ms grace
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Failed: 1}) {
		t.Fatalf("summary = %+v, want exactly one FAILURE — a node that never answered is not a defer", sum)
	}
	r := results[0]
	if r.Result.Deferred {
		t.Errorf("result claims a defer (%q) for a node that never answered", r.Result.Reason)
	}
	if r.Result.NodeID != "" || r.Result.Seat != "" {
		t.Errorf("result carries node/seat %q/%q — nothing came back from the node to fill them", r.Result.NodeID, r.Result.Seat)
	}
	if !strings.Contains(r.Err, "never answered") {
		t.Errorf("err = %q, want it to say the node never answered", r.Err)
	}
	// The PUBLISHED shape is what an operator and the MCP caller read.
	rw := WireResponse(results, sum).Results[0]
	if rw.Deferred || !rw.Failed {
		t.Errorf("published result = deferred:%v failed:%v, want a loud failure", rw.Deferred, rw.Failed)
	}
	if node.dispatches.Load() == 0 {
		t.Error("the contract was never dispatched; this test must exercise the POLL path")
	}
}

// TestRunInfrastructureDeferCountsSeparately (H-1): a node that deferred
// because its own stack is broken (defer_class infrastructure) must be tallied
// in Summary.Infrastructure and published with its class — otherwise a fleet
// node with a dead llama-swap reports the same all-green "1 deferred" as a
// small model honestly abstaining, and the CLI exits 0 either way.
func TestRunInfrastructureDeferCountsSeparately(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	deferred := remoteWire("", "")
	deferred.Deferred = true
	deferred.DeferClass = core.DeferClassInfrastructure
	deferred.Reason = "structured re-pack unreachable: llama-server 500"
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(int64) (map[string]any, int) { return doneWire(t, deferred), http.StatusOK },
	}
	srv := node.server()

	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Deferred: 1, Infrastructure: 1}) {
		t.Fatalf("summary = %+v, want one defer ALSO counted as infrastructure", sum)
	}
	resp := WireResponse(results, sum)
	if resp.Summary.Infrastructure != 1 {
		t.Errorf("published summary.infrastructure = %d, want 1", resp.Summary.Infrastructure)
	}
	if resp.Results[0].DeferClass != core.DeferClassInfrastructure {
		t.Errorf("published defer_class = %q, want it carried to the caller", resp.Results[0].DeferClass)
	}
}

// TestRunAbstentionDeferIsNotInfrastructure: the contrast case — a model that
// answered in the wrong shape is a healthy stack producing a bad answer, and
// must NOT trip the broken-node signal.
func TestRunAbstentionDeferIsNotInfrastructure(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	deferred := remoteWire("the qube answer", "")
	deferred.Deferred = true
	deferred.DeferClass = core.DeferClassAbstention
	deferred.Reason = "output failed schema: missing required field answer"
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(int64) (map[string]any, int) { return doneWire(t, deferred), http.StatusOK },
	}
	srv := node.server()

	_, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Deferred: 1}) {
		t.Fatalf("summary = %+v, want a plain defer (abstention is not a broken node)", sum)
	}
}

// TestRunDispatch401IsAFailure: an auth refusal is a transport/config FAILURE
// (Summary.Failed), never a quiet defer.
func TestRunDispatch401IsAFailure(t *testing.T) {
	node := &fakeNode{
		t: t, token: "right-token", agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(int64) (map[string]any, int) {
			return map[string]any{"state": "running"}, http.StatusOK
		},
	}
	srv := node.server()

	cfg := testCfg(t)
	cfg.FleetAuthToken = "wrong-token"
	results, sum, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Failed: 1}) {
		t.Fatalf("summary = %+v, want exactly one failure", sum)
	}
	if !strings.Contains(results[0].Err, "401") {
		t.Fatalf("err = %q, want the 401 surfaced", results[0].Err)
	}
}

// TestRunRedispatchOnLostJob (202-reack, roast delta 14): a poll 404 means the
// ack was lost or the node restarted — the engine re-POSTs the SAME job id
// (the store re-acks idempotently; a duplicate can never double-run) and keeps
// polling to completion.
func TestRunRedispatchOnLostJob(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
	}
	node.pollState = func(n int64) (map[string]any, int) {
		if n == 1 {
			return map[string]any{"status": "error", "error": "unknown job"}, http.StatusNotFound
		}
		return doneWire(t, remoteWire("the qube answer", `{"answer":"42"}`)), http.StatusOK
	}
	srv := node.server()

	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Succeeded: 1}) {
		t.Fatalf("summary = %+v, want success after the redispatch", sum)
	}
	if got := node.dispatches.Load(); got != 2 {
		t.Fatalf("dispatches = %d, want 2 (initial + the 404-triggered redispatch)", got)
	}
	if results[0].JobID != node.lastJobID.Load().(string) {
		t.Error("redispatch must reuse the SAME delegator-minted job id")
	}
}

// TestRunRouteRemoteNoEligibleRemoteDefers: with route=remote forced and no
// remote passing the hard gate (agent lane disabled here), the subtask defers
// loudly — it must NOT silently fall local against an explicit route.
func TestRunRouteRemoteNoEligibleRemoteDefers(t *testing.T) {
	node := &fakeNode{
		t: t, agentEnabled: false, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(int64) (map[string]any, int) { return nil, http.StatusNotFound },
	}
	srv := node.server()

	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Config-classed: an explicit remote route with nothing eligible is an
	// operator problem, so it also trips the infrastructure counter (non-zero
	// CLI exit) rather than passing as a routine defer.
	if sum != (Summary{Deferred: 1, Infrastructure: 1}) {
		t.Fatalf("summary = %+v, want one config-classed defer", sum)
	}
	if !strings.Contains(results[0].Result.Reason, "route=remote") || !strings.Contains(results[0].Result.Reason, "capability gate") {
		t.Fatalf("reason = %q, want a route=remote defer naming the gate", results[0].Result.Reason)
	}
	if node.dispatches.Load() != 0 {
		t.Error("an ineligible node must never be dispatched to")
	}
}

// TestRunNoEligibleRemoteNamesTheRealCause (H-3): "no eligible remote" is
// three very different failures wearing one sentence. The health-probe errors
// used to be dropped on the floor by fetchViews, so a wrong token, a node that
// is down, and a node that simply does not qualify all produced the identical
// defer — and the operator's first move (re-check the gate) was wrong in two
// of the three cases.
func TestRunNoEligibleRemoteNamesTheRealCause(t *testing.T) {
	// (1) Nothing configured at all.
	t.Run("no remotes configured", func(t *testing.T) {
		results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{remoteContract()}, "remote", nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if sum.Deferred != 1 {
			t.Fatalf("summary = %+v, want one defer", sum)
		}
		if !strings.Contains(results[0].Result.Reason, "no remote fleet nodes are configured") {
			t.Fatalf("reason = %q, want it to say no remotes were configured", results[0].Result.Reason)
		}
		if results[0].Result.DeferClass != core.DeferClassConfig {
			t.Errorf("defer_class = %q, want %q", results[0].Result.DeferClass, core.DeferClassConfig)
		}
	})

	// (2) Configured but unreachable — the probe error IS the answer.
	t.Run("every remote failed its probe", func(t *testing.T) {
		node := &fakeNode{
			t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
			pollState: func(int64) (map[string]any, int) { return nil, http.StatusNotFound },
		}
		srv := node.server()
		base := srv.URL
		srv.Close() // refused at connect from here on

		results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{base})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if sum != (Summary{Deferred: 1, Infrastructure: 1}) {
			t.Fatalf("summary = %+v, want an infrastructure-classed defer", sum)
		}
		reason := results[0].Result.Reason
		if !strings.Contains(reason, "health probe") || !strings.Contains(reason, base) {
			t.Fatalf("reason = %q, want the failed probe and the node URL named", reason)
		}
	})

	// (3) Reachable and healthy, but it does not qualify — this one really IS
	// the gate, and must say so rather than borrowing the probe's excuse.
	t.Run("gate rejected a healthy remote", func(t *testing.T) {
		node := &fakeNode{
			t: t, agentEnabled: false, resident: true, ctxTokens: 8192, nodeID: "fake-node",
			pollState: func(int64) (map[string]any, int) { return nil, http.StatusNotFound },
		}
		srv := node.server()

		results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{srv.URL})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if sum.Deferred != 1 {
			t.Fatalf("summary = %+v, want one defer", sum)
		}
		reason := results[0].Result.Reason
		if !strings.Contains(reason, "capability gate") {
			t.Fatalf("reason = %q, want the GATE named (every remote answered its probe)", reason)
		}
		if strings.Contains(reason, "health probe") {
			t.Fatalf("reason = %q, must not blame a probe that succeeded", reason)
		}
	})
}

// TestRunAutoBusyFallsLocalWhenNoEligibleRemote: route=auto with the local GPU
// lease HELD and no remote passing the gate keeps the work local — queued-local
// beats ineligible-remote (quality-first).
func TestRunAutoBusyFallsLocalWhenNoEligibleRemote(t *testing.T) {
	node := &fakeNode{
		t: t, agentEnabled: false, resident: false, ctxTokens: 0, nodeID: "fake-node",
		pollState: func(int64) (map[string]any, int) { return nil, http.StatusNotFound },
	}
	srv := node.server()

	// A REAL held lease through gpulease's own write path (the gate_test
	// pattern) so LocalBusy reads busy=true inside Run.
	leaseDir := filepath.Join(t.TempDir(), "lease")
	m, err := gpulease.OpenAt(leaseDir, "")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "delegate-run-test", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()

	cfg := testCfg(t)
	cfg.GPULockPath = leaseDir

	localCalls := 0
	local := func(ctx context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		localCalls++
		return core.AgentWireResult{SchemaVersion: 1, NodeID: "this-box", Seat: "local-seat", Output: "local qube answer", Structured: json.RawMessage(`{"answer":"local"}`), StopReason: "done"}, nil
	}
	results, sum, err := Run(t.Context(), cfg, local, []core.AgentContract{remoteContract()}, "auto", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Succeeded: 1}) {
		t.Fatalf("summary = %+v, want one local success", sum)
	}
	if localCalls != 1 {
		t.Fatalf("local runner calls = %d, want 1", localCalls)
	}
	if !results[0].Result.Deferred && results[0].Node != "this-box" {
		t.Errorf("node = %q, want the local node", results[0].Node)
	}
	if !strings.Contains(results[0].PlacementReason, "no eligible remote") {
		t.Errorf("placement reason = %q, want it to say why the busy box still ran locally", results[0].PlacementReason)
	}
}

// TestRunLocalDeferSkipsAcceptance (L-2): a DEFERRED local result must not be
// run through the acceptance checks. runRemote already guards this; runLocal
// did not, so a local defer came back carrying "failure" reasons for checks
// against an answer that was never produced — the ledger row and the corpus
// then read as a verification failure instead of a defer.
func TestRunLocalDeferSkipsAcceptance(t *testing.T) {
	local := func(ctx context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		return core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			NodeID:        "this-box", Seat: "local-seat",
			Deferred: true, DeferClass: core.DeferClassBudget,
			Reason: "step budget exhausted (12 steps)",
		}, nil
	}
	results, sum, err := Run(t.Context(), testCfg(t), local, []core.AgentContract{remoteContract()}, "local", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Deferred: 1}) {
		t.Fatalf("summary = %+v, want one plain defer", sum)
	}
	if len(results[0].AcceptanceFailures) != 0 {
		t.Fatalf("acceptance failures = %v, want none — there was no answer to check", results[0].AcceptanceFailures)
	}
}

// TestRunRouteLocalNeverTouchesTheNetwork: route=local must not even fetch
// health — the engine sees zero HTTP traffic.
func TestRunRouteLocalNeverTouchesTheNetwork(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	local := func(ctx context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: 1, NodeID: "this-box", Seat: "local-seat", Output: "the qube answer", Structured: json.RawMessage(`{"answer":"42"}`), StopReason: "done"}, nil
	}
	_, sum, err := Run(t.Context(), testCfg(t), local, []core.AgentContract{remoteContract()}, "local", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Succeeded: 1}) {
		t.Fatalf("summary = %+v", sum)
	}
	if hits.Load() != 0 {
		t.Fatalf("route=local made %d HTTP calls, want 0", hits.Load())
	}
}

// captureLog redirects the standard logger into a buffer for one test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })
	return &buf
}

// TestRunTelemetryFailureIsLoudOnceAndNeverFailsTheRun (H-4): the delegation-log
// corpus is a stated deliverable of this arc, and its write error was discarded
// — a delegator writing nothing to disk looked exactly like one writing
// everything. Telemetry still must never fail the work, so: results complete,
// and the failure is announced ONCE per run (not once per subtask, which would
// turn an 8-subtask fan-out into 8 identical lines).
func TestRunTelemetryFailureIsLoudOnceAndNeverFailsTheRun(t *testing.T) {
	logs := captureLog(t)
	home := t.TempDir()
	// A FILE where the corpus DIRECTORY must go: every append fails at MkdirAll.
	if err := os.WriteFile(filepath.Join(home, "delegation-log"), []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Home: home,
		// A ledger under a path that cannot be created either: ledger.Open fails.
		LedgerPath: filepath.Join(home, "delegation-log", "ledger.jsonl"),
		AgentModel: "local-seat",
	}

	local := func(ctx context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: 1, NodeID: "this-box", Seat: "local-seat", Output: "the qube answer", Structured: json.RawMessage(`{"answer":"42"}`), StopReason: "done"}, nil
	}
	_, sum, err := Run(t.Context(), cfg, local, []core.AgentContract{remoteContract(), remoteContract()}, "local", nil)
	if err != nil {
		t.Fatalf("Run: %v — telemetry must never fail the work it describes", err)
	}
	if sum != (Summary{Succeeded: 2}) {
		t.Fatalf("summary = %+v, want both subtasks delivered", sum)
	}
	out := logs.String()
	if n := strings.Count(out, "delegation-log write failed"); n != 1 {
		t.Fatalf("corpus warnings = %d, want exactly 1 per run (log: %s)", n, out)
	}
	if !strings.Contains(out, "ledger") {
		t.Fatalf("a ledger that could not be opened must say so: %s", out)
	}
}

// TestRunRejectsBadInputs: route vocabulary, subtask bounds, and non-tailnet
// remotes are CONFIG errors — the whole Run refuses, nothing executes.
func TestRunRejectsBadInputs(t *testing.T) {
	cfg := testCfg(t)
	local := func(ctx context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		return core.AgentWireResult{}, nil
	}
	one := []core.AgentContract{remoteContract()}

	if _, _, err := Run(t.Context(), cfg, local, one, "sideways", nil); err == nil {
		t.Error("bad route must be refused")
	}
	if _, _, err := Run(t.Context(), cfg, local, nil, "auto", nil); err == nil {
		t.Error("zero subtasks must be refused")
	}
	nine := make([]core.AgentContract, 9)
	for i := range nine {
		nine[i] = remoteContract()
	}
	if _, _, err := Run(t.Context(), cfg, local, nine, "auto", nil); err == nil {
		t.Error("more than 8 subtasks must be refused")
	}
	if _, _, err := Run(t.Context(), cfg, local, one, "auto", []string{"https://api.evil.com"}); err == nil {
		t.Error("a non-tailnet remote must be refused (never-cloud)")
	}
}

// TestRunFansOutBoundedAndOrdered is the concurrency pin the engine never had:
// every other Run test passes exactly ONE subtask, so the fan-out — the
// semaphore, the per-goroutine result slot, the delegation-log's interleaving
// guard — ran unexercised in every green suite.
//
// Eight subtasks, one node, route=remote. Four things must hold at once:
//
//   - EIGHT dispatches with eight DISTINCT job ids. A shared or reused id would
//     make the node's own idempotent re-ack path silently collapse subtasks
//     into one run, and seven results would be a copy of the first.
//   - Results in SUBMISSION order. Callers index results against the subtasks
//     they sent; goroutines finishing out of order must not reorder them.
//   - Never more than runConcurrency in flight. The bound exists so a local
//     fallback burst cannot stampede the one GPU; an unbounded fan-out would
//     pass every single-subtask test.
//   - Exactly EIGHT well-formed JSONL corpus lines. delegationLogMu's own
//     comment says O_APPEND atomicity is insufficient at these line sizes
//     (a corpus line carries the whole contract), and it had zero coverage —
//     an interleaved write corrupts the standing agent-task dataset silently.
func TestRunFansOutBoundedAndOrdered(t *testing.T) {
	compressPolls(t, 2*time.Millisecond, time.Second)
	const n = 8

	var (
		mu       sync.Mutex
		goalByID = map[string]string{}
		idOrder  []string

		inFlight    atomic.Int64
		maxInFlight atomic.Int64
	)
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 16384, nodeID: "fan-node",
		onDispatch: func(jobID string, c core.AgentContract) {
			cur := inFlight.Add(1)
			for {
				peak := maxInFlight.Load()
				if cur <= peak || maxInFlight.CompareAndSwap(peak, cur) {
					break
				}
			}
			mu.Lock()
			goalByID[jobID] = c.Goal
			idOrder = append(idOrder, jobID)
			mu.Unlock()
			// Hold the slot so overlapping dispatches are OBSERVABLE: without a
			// hold, an unbounded fan-out and a serial one both sample 1.
			time.Sleep(20 * time.Millisecond)
			inFlight.Add(-1)
		},
		pollByJob: func(jobID string, _ int64) (map[string]any, int) {
			mu.Lock()
			goal := goalByID[jobID]
			mu.Unlock()
			// Echo the subtask's own goal back as the output: that is what makes
			// "results came back in submission order" checkable at all.
			return doneWire(t, remoteWire(goal, `{"answer":"ok"}`)), http.StatusOK
		},
	}
	srv := node.server()

	subtasks := make([]core.AgentContract, 0, n)
	for i := 0; i < n; i++ {
		c := remoteContract()
		c.Goal = fmt.Sprintf("subtask-%d", i)
		c.Acceptance = nil // acceptance is pinned elsewhere; this test is about the fan-out
		subtasks = append(subtasks, c)
	}

	cfg := testCfg(t)
	results, sum, err := Run(t.Context(), cfg, neverLocal(t), subtasks, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Succeeded: n}) {
		t.Fatalf("summary = %+v, want %d successes", sum, n)
	}

	if got := node.dispatches.Load(); got != n {
		t.Fatalf("dispatches = %d, want %d (one per subtask)", got, n)
	}
	seen := map[string]bool{}
	for _, id := range idOrder {
		if !strings.HasPrefix(id, "agd-") {
			t.Errorf("job id %q lacks the delegator-minted agd- prefix", id)
		}
		if seen[id] {
			t.Fatalf("job id %q dispatched twice — a shared id makes the node's idempotent re-ack collapse two subtasks into one run", id)
		}
		seen[id] = true
	}
	if len(seen) != n {
		t.Fatalf("distinct job ids = %d, want %d", len(seen), n)
	}

	for i, r := range results {
		want := fmt.Sprintf("subtask-%d", i)
		if r.Result.Output != want {
			t.Errorf("results[%d].output = %q, want %q — results must stay in SUBMISSION order however the goroutines finish", i, r.Result.Output, want)
		}
	}

	peak := maxInFlight.Load()
	if peak > runConcurrency {
		t.Errorf("peak concurrent dispatches = %d, want at most runConcurrency (%d) — the bound keeps a local-fallback burst off the one GPU", peak, runConcurrency)
	}
	if peak < 2 {
		t.Errorf("peak concurrent dispatches = %d — the fan-out never actually overlapped, so this test proved nothing about the bound", peak)
	}

	// The corpus: exactly n well-formed lines, none shredded by an interleaved
	// concurrent append.
	logDir := filepath.Join(cfg.BaseDir(), "delegation-log")
	entries, derr := os.ReadDir(logDir)
	if derr != nil || len(entries) != 1 {
		t.Fatalf("delegation-log dir: %v (entries %d)", derr, len(entries))
	}
	raw, rerr := os.ReadFile(filepath.Join(logDir, entries[0].Name()))
	if rerr != nil {
		t.Fatal(rerr)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != n {
		t.Fatalf("delegation-log lines = %d, want %d (interleaved appends shred the corpus)", len(lines), n)
	}
	logged := map[string]bool{}
	for i, ln := range lines {
		var rec struct {
			JobID    string `json:"job_id"`
			Contract struct {
				Goal string `json:"goal"`
			} `json:"contract"`
		}
		if err := json.Unmarshal([]byte(ln), &rec); err != nil {
			t.Fatalf("delegation-log line %d is not well-formed JSON (%v): %q", i, err, ln)
		}
		if !seen[rec.JobID] {
			t.Errorf("delegation-log line %d names job %q, which was never dispatched", i, rec.JobID)
		}
		logged[rec.Contract.Goal] = true
	}
	if len(logged) != n {
		t.Fatalf("distinct goals in the corpus = %d, want %d (a lost or duplicated line)", len(logged), n)
	}
}
