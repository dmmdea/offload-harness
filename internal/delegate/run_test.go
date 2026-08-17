// Task 6 (multi-node agent delegation): delegate.Run — the shared execution
// engine both delegator surfaces (MCP agent_delegate, the CLI verb) call. The
// fleet node is faked over httptest (dispatch + jobs, the real wire shapes).
//
// Load-bearing pins: delegator-side acceptance flips a schema-valid result to
// failed-verification (the wrong-valid-schema hole this engine exists to
// close); the poll deadline (TimeoutSec + grace) marks the subtask DEFERRED
// with reason "poll deadline" and stops polling; a lost job (poll 404) is
// re-dispatched under the SAME delegator-minted job id (202-reack semantics);
// and telemetry lands in both the delegation-log corpus and the ledger.

package delegate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	// dispatchStatus overrides the 202 ack; 0 = normal ack.
	dispatchStatus int

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
		if _, err := core.DecodeAgentContract(strings.NewReader(string(env.Payload))); err != nil {
			f.t.Errorf("payload is not a dispatchable contract: %v", err)
		}
		f.lastJobID.Store(env.JobID)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"job_id": env.JobID, "status": "accepted"})
	})
	mux.HandleFunc("GET /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		n := f.polls.Add(1)
		body, status := f.pollState(n)
		if body != nil {
			if _, has := body["job_id"]; !has {
				body["job_id"] = r.PathValue("id")
			}
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
	srv := httptest.NewServer(mux)
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

// TestRunPollDeadlineDefers: a node that never finishes is abandoned at
// TimeoutSec + grace with the verbatim "poll deadline" defer, and polling STOPS.
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
	if !r.Result.Deferred || r.Result.Reason != "poll deadline" {
		t.Fatalf("deferred/reason = %v/%q, want the verbatim poll-deadline defer", r.Result.Deferred, r.Result.Reason)
	}
	polled := node.polls.Load()
	time.Sleep(150 * time.Millisecond)
	if node.polls.Load() != polled {
		t.Error("polling continued after the deadline; the delegator must STOP")
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
	if sum != (Summary{Deferred: 1}) {
		t.Fatalf("summary = %+v, want one defer", sum)
	}
	if !strings.Contains(results[0].Result.Reason, "no eligible remote") {
		t.Fatalf("reason = %q, want a no-eligible-remote defer", results[0].Result.Reason)
	}
	if node.dispatches.Load() != 0 {
		t.Error("an ineligible node must never be dispatched to")
	}
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
