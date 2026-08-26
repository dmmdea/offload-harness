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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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

	"github.com/dmmdea/offload-harness/internal/buildinfo"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// hashOwnExecutable independently hashes the running test binary — the
// EXPECTED value for buildinfo.BuildSHA256, computed without buildinfo so the
// assertion compares two implementations rather than the package with itself.
func hashOwnExecutable(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	f, err := os.Open(exe)
	if err != nil {
		t.Fatalf("open %s: %v", exe, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", exe, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

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
	// dispatchHook, when set, runs FIRST inside the dispatch handler with this
	// node's 1-based dispatch ordinal. A non-zero status it returns is written
	// as a refusal and the ack path never runs; 0 falls through to
	// dispatchStatus and then to the normal ack. It is the seam a re-placement
	// test uses to script "refuse the first dispatch, take the second" and to
	// spend wall clock before refusing.
	dispatchHook func(n int64) int
	// healthDelay makes this node's /fleet/health spend real wall clock, which
	// is how a test makes the SELECTION step expensive: fetchViews probes every
	// remote sequentially, so a slow health handler is time the delegator
	// spends between measuring its remaining budget and using it.
	healthDelay time.Duration
	// killOnDispatch models a node the delegator can never REACH: the dispatch
	// connection is hijacked and dropped with no HTTP answer at all, so both
	// dispatch attempts end as transport errors. Distinct from a node that is
	// simply down — health still answers, so it is still a placement candidate.
	killOnDispatch bool
	// killOnPoll models a node that DIES after acking: the first poll's
	// connection is hijacked and dropped with no HTTP answer at all, and the
	// server itself goes down, so every later poll is refused at connect.
	killOnPoll bool

	// The capacity advertisement (0.100.0). All zero by default, which is what
	// a node too old to publish them decodes to — so every pre-existing fake
	// keeps advertising exactly what it advertised before.
	queueDepth        int
	jobsQueued        int
	jobsRunning       int
	maxConcurrentJobs int
	maxQueueDepth     int

	dispatches atomic.Int64
	polls      atomic.Int64
	lastJobID  atomic.Value // string
}

func (f *fakeNode) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, r *http.Request) {
		if f.healthDelay > 0 {
			select {
			case <-time.After(f.healthDelay):
			case <-r.Context().Done():
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node_id":             f.nodeID,
			"queue_depth":         f.queueDepth,
			"agent_seat":          "remote-seat",
			"agent_ctx_tokens":    f.ctxTokens,
			"agent_seat_resident": f.resident,
			"agent_enabled":       f.agentEnabled,
			"jobs_queued":         f.jobsQueued,
			"jobs_running":        f.jobsRunning,
			"max_concurrent_jobs": f.maxConcurrentJobs,
			"max_queue_depth":     f.maxQueueDepth,
		})
	})
	mux.HandleFunc("POST /fleet/dispatch", func(w http.ResponseWriter, r *http.Request) {
		n := f.dispatches.Add(1)
		if f.killOnDispatch {
			// No status line, no body: the delegator sees a transport error and
			// never an HTTP answer, on both dispatch attempts.
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, herr := hj.Hijack(); herr == nil {
					_ = conn.Close()
				}
			}
			return
		}
		if f.dispatchHook != nil {
			if status := f.dispatchHook(n); status != 0 {
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": "scripted refusal"})
				return
			}
		}
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
		// A1 delegator-side pins (0.81.0).
		DelegatorVersion     string `json:"delegator_version"`
		DelegatorBuildSHA256 string `json:"delegator_build_sha256"`
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
	// The delegator pin must name THIS build: version from buildinfo, and the
	// self-hash equal to hashing the running test binary independently — not
	// merely non-empty, which would also pass a hash of the wrong file.
	if line.DelegatorVersion != buildinfo.Version {
		t.Errorf("delegator_version = %q, want %q", line.DelegatorVersion, buildinfo.Version)
	}
	if want := hashOwnExecutable(t); line.DelegatorBuildSHA256 != want {
		t.Errorf("delegator_build_sha256 = %q, want the running binary's own sha256 %q", line.DelegatorBuildSHA256, want)
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

	// 0.79.0: a failed verification earns ONE retry on a different node — here
	// the local seat, which answers just as wrongly, so the first (remote)
	// attempt stands and the summary reports the retry without a recovery.
	results, sum, err := Run(t.Context(), testCfg(t), failingLocal(new(atomic.Int64)), []core.AgentContract{remoteContract()}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{FailedVerification: 1, Retried: 1}) {
		t.Fatalf("summary = %+v, want exactly one failed-verification (retried once, not recovered)", sum)
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

// TestRunPollUnusableAnswers (C-1, re-scoped by C-A): a node answering 503 to
// every poll is not a slow node. The status used to fall through the switch
// unread, so the run ended in the same "poll deadline" defer a healthy-but-slow
// node produces. It must keep polling (the state may still resolve) — but WHAT
// it ends as now depends on whether the node ever reported OWNING the job, and
// a 503 never does:
//
//   - unusable from the first poll to the last: nothing ever said the job was
//     accepted THERE, so a defer would be authored on that node's behalf. It is
//     a FAILURE, with the 503 named.
//   - unusable only AFTER the node reported the job accepted/running: the node
//     did take the work, so the honest outcome is a defer — classed
//     infrastructure, never the plain budget defer a healthy node earns.
func TestRunPollUnusableAnswers(t *testing.T) {
	t.Run("never owned: a failure naming the 503", func(t *testing.T) {
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
		if sum != (Summary{Failed: 1}) {
			t.Fatalf("summary = %+v, want a FAILURE — no answer ever reported the job accepted on that node", sum)
		}
		r := results[0]
		if r.Result.Deferred {
			t.Errorf("result claims a defer for a node that never reported owning the job: %q", r.Result.Reason)
		}
		if !strings.Contains(r.Err, "503") {
			t.Errorf("err = %q, want the unusable 503 answers named", r.Err)
		}
		if node.polls.Load() < 3 {
			t.Errorf("polls = %d — an unusable answer must not abandon the job early; the deadline decides", node.polls.Load())
		}
	})

	t.Run("owned, then unusable: an infrastructure defer", func(t *testing.T) {
		compressPolls(t, 20*time.Millisecond, 100*time.Millisecond)
		node := &fakeNode{
			t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
			pollState: func(n int64) (map[string]any, int) {
				if n == 1 {
					return map[string]any{"state": "running"}, http.StatusOK
				}
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
		if sum != (Summary{Deferred: 1, Infrastructure: 1, LostToStack: 1}) {
			t.Fatalf("summary = %+v, want the defer counted as infrastructure", sum)
		}
		if r := results[0]; !strings.Contains(r.Result.Reason, "503") {
			t.Errorf("reason = %q, want the unusable 503 answers named", r.Result.Reason)
		}
	})
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
	rw := WireResponse(results, sum, nil).Results[0]
	if rw.Deferred || !rw.Failed {
		t.Errorf("published result = deferred:%v failed:%v, want a loud failure", rw.Deferred, rw.Failed)
	}
	if node.dispatches.Load() == 0 {
		t.Error("the contract was never dispatched; this test must exercise the POLL path")
	}
}

// TestRunPoll404InsideTheRedispatchWindowIsAFailure (C-A): a poll 404 is the
// node POSITIVELY DENYING it ever held the job — the one answer that proves
// nothing is running there. It used to set the same "the node answered" flag a
// live `running` answer sets, so a deadline landing INSIDE the re-dispatch
// window (≤2 404s) took the ANSWERED path and published
// {deferred:true, class:"budget", reason:"…node accepted the job but did not
// reach a terminal state"} stamped with that node's id and seat, at exit 0 —
// the exact fabrication TestRunPollDeadNodeIsAFailureNotAFabricatedDefer
// forbids, reached by a different door.
func TestRunPoll404InsideTheRedispatchWindowIsAFailure(t *testing.T) {
	// Poll budget = TimeoutSec(1s) + grace(-700ms) = 300ms, polls every 200ms:
	// two polls land (t≈0, t≈200ms), each spending one of the two allowed
	// re-dispatches, and the deadline fires at t≈400ms with redispatches STILL
	// under the cap — i.e. inside the window, never reaching the
	// "lost job N times" failure that guards the far end of it.
	compressPolls(t, 200*time.Millisecond, -700*time.Millisecond)
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(int64) (map[string]any, int) {
			return map[string]any{"status": "error", "error": "unknown job"}, http.StatusNotFound
		},
	}
	srv := node.server()

	contract := remoteContract()
	contract.TimeoutSec = 1
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if node.dispatches.Load() != 3 {
		t.Fatalf("dispatches = %d, want 3 (initial + two 404-triggered re-dispatches) — this test must land the deadline INSIDE the re-dispatch window", node.dispatches.Load())
	}
	if sum != (Summary{Failed: 1}) {
		t.Fatalf("summary = %+v, want exactly one FAILURE — every answer was a 404 denying the job ever existed there", sum)
	}
	r := results[0]
	if r.Result.Deferred {
		t.Errorf("result claims a defer (%q / class %q) for a node that denied ever holding the job", r.Result.Reason, r.Result.DeferClass)
	}
	if r.Result.NodeID != "" || r.Result.Seat != "" {
		t.Errorf("result carries node/seat %q/%q — nothing that ran the contract ever reported", r.Result.NodeID, r.Result.Seat)
	}
	if !strings.Contains(r.Err, "404") {
		t.Errorf("err = %q, want the 404 denial named", r.Err)
	}
}

// TestRunPollErrorIsClearedByHealthyAnswers (C-C): lastPollErr was assigned
// and never cleared, so ONE early blip followed by dozens of clean `running`
// answers still ended classed infrastructure, exited non-zero, and quoted an
// error from tens of polls ago — contradicting the class comment's own claim
// that "a node that answered every poll normally simply ran out of clock".
func TestRunPollErrorIsClearedByHealthyAnswers(t *testing.T) {
	compressPolls(t, 20*time.Millisecond, -900*time.Millisecond) // budget = 100ms
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(n int64) (map[string]any, int) {
			if n == 1 {
				return map[string]any{"status": "error", "error": "vram snapshot stale"}, http.StatusServiceUnavailable
			}
			return map[string]any{"state": "running"}, http.StatusOK
		},
	}
	srv := node.server()

	contract := remoteContract()
	contract.TimeoutSec = 1
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if polls := node.polls.Load(); polls < 3 {
		t.Fatalf("polls = %d — the healthy-answers-after-the-blip premise never happened", polls)
	}
	if sum != (Summary{Deferred: 1}) {
		t.Fatalf("summary = %+v, want a plain BUDGET defer — the node answered normally after one blip", sum)
	}
	r := results[0]
	if r.Result.DeferClass != core.DeferClassBudget {
		t.Errorf("defer_class = %q, want %q — a stale error must not outlive the polls that succeeded after it", r.Result.DeferClass, core.DeferClassBudget)
	}
	if strings.Contains(r.Result.Reason, "last poll error") {
		t.Errorf("reason = %q, still quotes an error the node recovered from", r.Result.Reason)
	}
}

// TestRunAutoLocalFallbackReportsADeadFleet (C-D): route=remote classes a
// totally unreachable fleet infrastructure and exits non-zero; route=auto with
// a busy local GPU used to DISCARD that same class (`why, _ :=`), so a fleet
// that has been down for a week read green forever. The work still runs
// locally — the placement is right — but the broken fleet must be reported.
func TestRunAutoLocalFallbackReportsADeadFleet(t *testing.T) {
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(int64) (map[string]any, int) { return nil, http.StatusNotFound },
	}
	srv := node.server()
	base := srv.URL
	srv.Close() // refused at connect: every probe fails

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
	local := func(ctx context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: 1, NodeID: "this-box", Seat: "local-seat",
			Output: "local qube answer", Structured: json.RawMessage(`{"answer":"local"}`), StopReason: "done"}, nil
	}
	results, sum, err := Run(t.Context(), cfg, local, []core.AgentContract{remoteContract()}, "auto", []string{base})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Succeeded: 1, Infrastructure: 1}) {
		t.Fatalf("summary = %+v, want the local success PLUS the dead fleet counted — a fleet that never answers must not read green", sum)
	}
	if !strings.Contains(results[0].PlacementReason, "health probe") {
		t.Errorf("placement reason = %q, want the failed probe named", results[0].PlacementReason)
	}
}

// TestRunContractSideGateRejectionIsNotABrokenStack (C-F): remoteEligible
// rejects on five conditions and three are properties of the CALLER'S
// CONTRACT (no output_schema — legal per Validate; depth != 0; a token
// estimate no advertised ceiling can hold). Filing those as `config` put them
// in BrokenStackDefer, so `--route remote` exited non-zero and told the
// delegating model a node was broken on a run where every node was healthy.
func TestRunContractSideGateRejectionIsNotABrokenStack(t *testing.T) {
	cases := []struct {
		name      string
		ctxTokens int
		mutate    func(*core.AgentContract)
		wantSub   string
		// wantCtxFit pins the exact ceiling PHRASING on the ctx-fit row (R7).
		// `wantSub: "context"` alone cannot: it is common to both the fleet-wide
		// wording and the scoped one contractIneligible switches to when a lane
		// advertised nothing, so flipping that switch to always-scoped left the
		// whole tree green. This fleet is fully advertised, so the unscoped
		// MAX claim is the honest one and the scoped phrasing would be a
		// silent regression — the mixed-fleet sibling test
		// (TestRunCtxFitIsContractSideOnlyWithARealCeiling) pins the other side.
		wantCtxFit []string
	}{
		{"no output_schema", 8192, func(c *core.AgentContract) { c.OutputSchema = nil }, "output_schema", nil},
		{"already past the origin hop", 8192, func(c *core.AgentContract) { c.Depth = 1 }, "depth", nil},
		{"contract cannot fit any advertised ceiling", 4096,
			func(c *core.AgentContract) { c.Goal = strings.Repeat("x", 30000) }, "context",
			[]string{"the contract needs ~", "the roomiest agent-enabled remote advertises 4096"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := &fakeNode{
				t: t, agentEnabled: true, resident: true, ctxTokens: tc.ctxTokens, nodeID: "healthy-node",
				pollState: func(int64) (map[string]any, int) { return nil, http.StatusNotFound },
			}
			srv := node.server()

			contract := remoteContract()
			tc.mutate(&contract)
			results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{srv.URL})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if sum != (Summary{Deferred: 1}) {
				t.Fatalf("summary = %+v, want ONE defer and NO infrastructure — every node here is healthy; the contract is what cannot be placed", sum)
			}
			r := results[0]
			if r.Result.DeferClass != core.DeferClassContract {
				t.Errorf("defer_class = %q, want %q", r.Result.DeferClass, core.DeferClassContract)
			}
			if !strings.Contains(r.Result.Reason, tc.wantSub) {
				t.Errorf("reason = %q, want the contract property named (%q)", r.Result.Reason, tc.wantSub)
			}
			for _, want := range tc.wantCtxFit {
				if !strings.Contains(r.Result.Reason, want) {
					t.Errorf("reason = %q, want %q — every lane here ADVERTISED a ceiling, so the fleet-wide MAX claim is measured, not authored on a silent node's behalf", r.Result.Reason, want)
				}
			}
			if BrokenStackDefer(r.Result.DeferClass) {
				t.Errorf("class %q counts as a broken stack — a caller-contract mistake must never tell the operator a box is down", r.Result.DeferClass)
			}
		})
	}
}

// TestRunPollFailureLoggingIsBounded (C-G): both poll-failure log calls fired
// ONCE PER POLL, in the very commit that added sync.Once because unbounded
// per-subtask logging buries the results it warns about. One dead node at a
// production cadence is ~120 lines per subtask; an 8-way fan-out, ~1000.
func TestRunPollFailureLoggingIsBounded(t *testing.T) {
	logs := captureLog(t)
	compressPolls(t, 20*time.Millisecond, -700*time.Millisecond) // budget = 300ms ⇒ ~15 polls
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(int64) (map[string]any, int) {
			return map[string]any{"status": "error", "error": "vram snapshot stale"}, http.StatusServiceUnavailable
		},
	}
	srv := node.server()

	contract := remoteContract()
	contract.TimeoutSec = 1
	if _, _, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{srv.URL}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if polls := node.polls.Load(); polls < 5 {
		t.Fatalf("polls = %d — too few for this test to say anything about per-poll logging", polls)
	}
	out := logs.String()
	if n := strings.Count(out, "delegate: poll"); n > 3 {
		t.Errorf("poll log lines = %d for %d polls, want the first of each shape plus one summary (log:\n%s)", n, node.polls.Load(), out)
	}
	if !strings.Contains(out, "failed poll(s)") {
		t.Errorf("no end-of-poll summary line: %s", out)
	}
}

// TestRunProbeFailureLogsOncePerRemotePerRun (C-G): fetchViews runs inside
// runOne, so its per-remote probe warning fired once per remote PER SUBTASK —
// an 8-subtask fan-out against 2 dead remotes printed 16 identical lines.
func TestRunProbeFailureLogsOncePerRemotePerRun(t *testing.T) {
	logs := captureLog(t)
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(int64) (map[string]any, int) { return nil, http.StatusNotFound },
	}
	srv := node.server()
	base := srv.URL
	srv.Close()

	results, _, err := Run(t.Context(), testCfg(t), neverLocal(t),
		[]core.AgentContract{remoteContract(), remoteContract(), remoteContract()}, "remote", []string{base})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}
	if n := strings.Count(logs.String(), "health probe of"); n != 1 {
		t.Errorf("probe warnings = %d for 3 subtasks against 1 remote, want exactly 1 per base per run (log:\n%s)", n, logs.String())
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
	if sum != (Summary{Deferred: 1, Infrastructure: 1, LostToStack: 1}) {
		t.Fatalf("summary = %+v, want one defer ALSO counted as infrastructure", sum)
	}
	resp := WireResponse(results, sum, nil)
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

	// 0.79.0: an abstention earns ONE retry on a different node; the local seat
	// abstains too, so the defer stands — plain, retried, not infrastructure.
	_, sum, err := Run(t.Context(), testCfg(t), abstainingLocal(), []core.AgentContract{remoteContract()}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Deferred: 1, Retried: 1}) {
		t.Fatalf("summary = %+v, want a plain defer (abstention is not a broken node), retried once", sum)
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
	if sum != (Summary{Deferred: 1, Infrastructure: 1, LostToStack: 1}) {
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
		if sum != (Summary{Deferred: 1, Infrastructure: 1, LostToStack: 1}) {
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
	results, sum, err := Run(t.Context(), cfg, local, []core.AgentContract{remoteContract(), remoteContract()}, "local", nil)
	if err != nil {
		t.Fatalf("Run: %v — telemetry must never fail the work it describes", err)
	}
	// R4-6: this scenario ALSO kills the ledger (its path is under the
	// uncreatable dir), and the ledger loss used to count as zero — record()'s
	// `if r.led != nil` guard skipped a ledger that never opened, so the summary
	// asserted here was the total-loss case published as no loss at all.
	if sum != (Summary{Succeeded: 2, CorpusRowsAttempted: 2, CorpusRowsLost: 2, LedgerRowsAttempted: 2, LedgerRowsLost: 2}) {
		t.Fatalf("summary = %+v, want both subtasks delivered AND both telemetry losses counted with their denominators", sum)
	}
	out := logs.String()
	if n := strings.Count(out, "delegation-log write failed"); n != 1 {
		t.Fatalf("corpus warnings = %d, want exactly 1 per run (log: %s)", n, out)
	}
	if !strings.Contains(out, "ledger") {
		t.Fatalf("a ledger that could not be opened must say so: %s", out)
	}
	// C-H: "this run's corpus rows are LOST" reads identically whether 1 of 8
	// or 8 of 8 failed. The end-of-run line has to say HOW MANY.
	if !strings.Contains(out, "2 of 2") {
		t.Fatalf("no end-of-run tally naming how many rows were lost: %s", out)
	}
	// …and the MCP caller — the lane's primary consumer — never saw it at all:
	// SummaryWire had no field for it, so a delegation that recorded nothing
	// published byte-identically to one that recorded everything.
	published := WireResponse(results, sum, nil).Summary
	if published.CorpusRowsLost != 2 || published.CorpusRowsAttempted != 2 {
		t.Fatalf("published corpus rows = %d lost of %d attempted, want 2 of 2", published.CorpusRowsLost, published.CorpusRowsAttempted)
	}
	if published.LedgerRowsLost != 2 || published.LedgerRowsAttempted != 2 {
		t.Fatalf("published ledger rows = %d lost of %d attempted, want 2 of 2 — a ledger that never opened published as a ledger that wrote everything",
			published.LedgerRowsLost, published.LedgerRowsAttempted)
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

// ---------------------------------------------------------------------------
// Round-4 review: DEFAULT TO LOUD.
//
// Every review round's new defects clustered in one place — the loud/quiet
// classification (defer classes, Summary.Infrastructure, exit codes) — because
// each added nuance encoded "whose fault is this" from ever-thinner evidence.
// The rule the tests below pin: a result may be classed QUIET (contract-side,
// abstention, budget) ONLY when the quiet explanation is positively
// established — every configured node answered and the node side is
// demonstrably fine. Absence of evidence about the fleet is never evidence the
// CALLER is at fault. A false alarm costs an operator one look; a silent
// failure costs a night.
// ---------------------------------------------------------------------------

// deadRemoteBase returns the base URL of a fleet node that is NOT listening:
// every health probe is refused at connect.
func deadRemoteBase(t *testing.T) string {
	t.Helper()
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "dead-node",
		pollState: func(int64) (map[string]any, int) { return nil, http.StatusNotFound },
	}
	srv := node.server()
	base := srv.URL
	srv.Close() // refused at connect from here on
	return base
}

// heldLease acquires the machine-wide GPU lease for the test's duration through
// gpulease's own write path, so LocalBusy reads busy=true inside Run.
func heldLease(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "lease")
	m, err := gpulease.OpenAt(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "delegate-run-test", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Release() })
	return dir
}

// TestRunContractSideClassRequiresAHealthyFleet (R4-1): contractIneligible ran
// BEFORE the "every remote failed its probe" cases, and two of its conditions
// (no output_schema, a non-origin depth) had NO node-side guard — so a caller's
// contract property short-circuited the whole classification even when not one
// node had answered. The same fleet state WITH a schema counted infrastructure,
// so adding a schema to a contract was what made a week-dead fleet audible.
func TestRunContractSideClassRequiresAHealthyFleet(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*core.AgentContract)
	}{
		{"no output_schema", func(c *core.AgentContract) { c.OutputSchema = nil }},
		{"already past the origin hop", func(c *core.AgentContract) { c.Depth = 1 }},
		{"too big for any advertised ceiling", func(c *core.AgentContract) { c.Goal = strings.Repeat("x", 30000) }},
	}
	for _, tc := range mutations {
		t.Run("route=remote/"+tc.name, func(t *testing.T) {
			contract := remoteContract()
			tc.mutate(&contract)
			results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{deadRemoteBase(t)})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if sum != (Summary{Deferred: 1, Infrastructure: 1, LostToStack: 1}) {
				t.Fatalf("summary = %+v, want the dead fleet counted — NOT ONE node answered, so nothing establishes the contract as the story", sum)
			}
			r := results[0]
			if r.Result.DeferClass == core.DeferClassContract {
				t.Errorf("defer_class = contract (reason %q) for a fleet that never answered", r.Result.Reason)
			}
			if !BrokenStackDefer(r.Result.DeferClass) {
				t.Errorf("defer_class = %q does not count as a broken stack; a totally dead fleet must exit non-zero", r.Result.DeferClass)
			}
			if !strings.Contains(r.Result.Reason, "health probe") {
				t.Errorf("reason = %q, want the failed probe named", r.Result.Reason)
			}
		})
	}

	// The reported repro: route=auto, local GPU busy, the remote refusing
	// connections, a contract with no output_schema. The work runs locally and
	// SUCCEEDS — that placement is right — but runOne's deadFleet keys off this
	// same class, so the contract short-circuit published {succeeded:1,
	// infrastructure:0} at exit 0 while the fleet had been down for a week.
	t.Run("route=auto: a local success must not hide a dead fleet", func(t *testing.T) {
		cfg := testCfg(t)
		cfg.GPULockPath = heldLease(t)
		contract := remoteContract()
		contract.OutputSchema = nil
		contract.Acceptance = []string{"contains:qube"} // text-verb only: nothing structured was asked for
		local := func(ctx context.Context, c core.AgentContract) (core.AgentWireResult, error) {
			return core.AgentWireResult{SchemaVersion: 1, NodeID: "this-box", Seat: "local-seat",
				Output: "local qube answer", StopReason: "done"}, nil
		}
		results, sum, err := Run(t.Context(), cfg, local, []core.AgentContract{contract}, "auto", []string{deadRemoteBase(t)})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if sum != (Summary{Succeeded: 1, Infrastructure: 1}) {
			t.Fatalf("summary = %+v, want the local success PLUS the dead fleet — a caller-contract property must never mask an unreachable fleet", sum)
		}
		if !strings.Contains(results[0].PlacementReason, "health probe") {
			t.Errorf("placement reason = %q, want the failed probe named", results[0].PlacementReason)
		}
	})
}

// TestRunCtxFitIsContractSideOnlyWithARealCeiling (R4-2, widened R5-1):
// agent_ctx_tokens is omitempty on the wire and config documents 0 as "not
// advertised — set it when opting a node in", i.e. an OPERATOR fix on a box. An
// unset value decodes as 0, still counts into lanes, and so satisfied
// `lanes > 0 && tooSmall == lanes` — blaming a 30-token goal on the caller's
// contract at exit 0. Both fixtures below are ordinary agent-enabled nodes
// (`agentEnabled: true`), which is what this state actually takes: the lane is
// admitted without any ceiling, so a peer PREDATING the lane is not the producer
// and could not be — it sends no agent_enabled and never reaches `lanes`.
//
// R5-1 — the round-4 guard was FLEET-WIDE where its own doc claimed per-lane,
// and this test was VACUOUS about it: it exercised only the ALL-zero fleet,
// where `nodeSideVerdict`'s roomiest==0 case alone satisfied every assertion, so
// `roomiest > 0 &&` could be deleted from contractIneligible with the whole tree
// still green (verified by mutation). Because `roomiest` is a fleet-wide MAX,
// ONE node advertising a real ceiling supplied it for EVERY node advertising
// none — a mixed fleet still shipped a quiet exit-0 contract-classed defer whose
// reason ALSO authored a claim on the silent node's behalf ("the roomiest
// agent-enabled remote advertises 4096"). An absent agent_ctx_tokens means the
// ceiling is UNKNOWN, not small; the box may be a 128k machine. So the mixed row
// is here, and BOTH rows now assert the two halves the old test never separated:
// the class is loud, AND no ceiling claim is made for a lane that advertised
// none.
//
// R6 — the R5 fix over-corrected into SUPPRESSION: with a silent lane present,
// the ctx-fit sentence vanished even when every ADVERTISED lane was genuinely
// too small, so the operator heard one of two true causes per run. The mixed row
// now pins the merged reason, scoped to the lanes that sent a number.
func TestRunCtxFitIsContractSideOnlyWithARealCeiling(t *testing.T) {
	type lane struct {
		id  string
		ctx int // advertised agent_ctx_tokens; 0 = the field never arrived
	}
	cases := []struct {
		name  string
		fleet []lane
		goal  string
		// wantNamed are the node ids the reason must name — the operator has to
		// know WHICH box to go set the field on.
		wantNamed []string
		// wantCtxFit is the SECOND truth the reason must also carry when the
		// contract does not fit any ceiling that WAS advertised (R6). Empty means
		// no ctx-fit sentence may be spoken at all — there is no advertised number
		// for the contract to be too big for.
		wantCtxFit []string
	}{
		{
			name:      "no remote advertises a ceiling",
			fleet:     []lane{{"no-ceiling-node", 0}},
			goal:      "name the capital", // nothing about this contract is big
			wantNamed: []string{"no-ceiling-node"},
		},
		{
			// The mixed fleet: one node answers with a real ceiling, one runs the
			// lane with agent_ctx_tokens unset. AgentLaneAdmissible gates the lane
			// on fleet_agent_enabled + a resolvable planner seat + a safely
			// reachable listener — never on a ceiling — and health advertises
			// whatever is configured, 0 included, so both nodes here are ordinary
			// agent-enabled peers whose operators made different choices. (A peer
			// PREDATING the lane cannot produce this state: it sends no
			// agent_enabled either, decodes as AgentEnabled:false, and never enters
			// `lanes`.)
			//
			// R6: the ctx-fit sentence used to be SUPPRESSED here rather than
			// merged, so the operator heard "set agent_ctx_tokens on
			// no-ceiling-node" and never "and the contract does not fit the 4096
			// the other one advertises" — one fix, then a second run to discover
			// the second. Both truths must survive, and the ceiling clause must be
			// phrased so it claims nothing about the silent node.
			name:       "one remote advertises, one does not",
			fleet:      []lane{{"advertised-node", 4096}, {"no-ceiling-node", 0}},
			goal:       strings.Repeat("x", 30000), // ~13k tokens with the reserve: genuinely past 4096
			wantNamed:  []string{"no-ceiling-node"},
			wantCtxFit: []string{"the contract needs ~", "every remote that DID advertise a ceiling tops out at 4096"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bases := make([]string, 0, len(tc.fleet))
			for _, l := range tc.fleet {
				node := &fakeNode{
					t: t, agentEnabled: true, resident: true, ctxTokens: l.ctx, nodeID: l.id,
					pollState: func(int64) (map[string]any, int) { return nil, http.StatusNotFound },
				}
				bases = append(bases, node.server().URL)
			}
			contract := remoteContract()
			contract.Goal = tc.goal
			results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", bases)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if sum != (Summary{Deferred: 1, Infrastructure: 1, LostToStack: 1}) {
				t.Fatalf("summary = %+v, want a LOUD class — a lane advertised no ceiling, which an operator fixes on the node", sum)
			}
			r := results[0]
			if r.Result.DeferClass == core.DeferClassContract {
				t.Errorf("defer_class = contract (reason %q) — an unadvertised ceiling is UNKNOWN, and the caller cannot rewrite a contract to fit an unknown", r.Result.Reason)
			}
			if !strings.Contains(r.Result.Reason, "agent_ctx_tokens") {
				t.Errorf("reason = %q, want the unset node-side field named", r.Result.Reason)
			}
			for _, id := range tc.wantNamed {
				if !strings.Contains(r.Result.Reason, id) {
					t.Errorf("reason = %q, want the silent node %q named — the fix is on THAT box", r.Result.Reason, id)
				}
			}
			// The FABRICATION half, and the assertion the old test lacked: no
			// "roomiest" verdict may be spoken over a fleet with a silent lane —
			// "roomiest" is a fleet-wide MAX, so it implies the silent node is
			// SMALLER, a claim about a ceiling nobody published (the same defect
			// class as the invented 404 denial a previous round removed).
			if strings.Contains(r.Result.Reason, "roomiest") {
				t.Errorf("reason = %q asserts a roomiest-ceiling verdict over a fleet where a lane advertised nothing", r.Result.Reason)
			}
			// R6, the other half of the same sentence: suppressing it entirely
			// costs the operator the SECOND truth. When every ADVERTISED lane is
			// genuinely too small, that must be said too — scoped to the lanes that
			// actually sent a number, which fabricates nothing.
			for _, want := range tc.wantCtxFit {
				if !strings.Contains(r.Result.Reason, want) {
					t.Errorf("reason = %q, want it to ALSO carry %q — both causes are true and the operator needs both", r.Result.Reason, want)
				}
			}
			if len(tc.wantCtxFit) == 0 && strings.Contains(r.Result.Reason, "the contract needs ~") {
				t.Errorf("reason = %q speaks a ctx-fit verdict although no lane advertised a ceiling to be too big for", r.Result.Reason)
			}
		})
	}
}

// TestRunPollFailureNeverInventsA404Denial (R4-4): unownedDetail printed the
// 404-denial sentence for EVERY answering node, so a node that returned only
// 503s published "a poll 404 DENIES it ever held it (0 re-dispatch(es) made)" —
// the delegator authoring a claim on the node's behalf, which is the exact class
// of defect the surrounding message was written to fix.
func TestRunPollFailureNeverInventsA404Denial(t *testing.T) {
	compressPolls(t, 20*time.Millisecond, -900*time.Millisecond) // budget = 100ms
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
	if sum != (Summary{Failed: 1}) {
		t.Fatalf("summary = %+v, want one failure", sum)
	}
	r := results[0]
	if strings.Contains(r.Err, "404") || strings.Contains(r.Err, "DENIES") {
		t.Errorf("err = %q asserts a 404 denial; this node answered only 503 and never 404ed", r.Err)
	}
	if !strings.Contains(r.Err, "503") {
		t.Errorf("err = %q, want the answers the node ACTUALLY gave named", r.Err)
	}
}

// TestRunDeferAfterLostJobsIsInfrastructure (R4-5): the 404 arm set neither
// lastPollErr nor pollFails.note, so a node that DROPPED the job twice and then
// answered normally to the deadline published a plain budget defer — "node
// accepted the job but did not reach a terminal state", exit 0 — with nothing
// anywhere saying it had lost the work. unownedDetail renders the re-dispatch
// count on the FAILURE path; the defer path discarded it.
func TestRunDeferAfterLostJobsIsInfrastructure(t *testing.T) {
	compressPolls(t, 20*time.Millisecond, -900*time.Millisecond) // budget = 100ms
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(n int64) (map[string]any, int) {
			if n <= 2 {
				return map[string]any{"status": "error", "error": "unknown job"}, http.StatusNotFound
			}
			return map[string]any{"state": "running"}, http.StatusOK
		},
	}
	srv := node.server()

	contract := remoteContract()
	contract.TimeoutSec = 1
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d := node.dispatches.Load(); d != 3 {
		t.Fatalf("dispatches = %d, want 3 (initial + two 404-triggered re-dispatches) — the premise never happened", d)
	}
	if sum != (Summary{Deferred: 1, Infrastructure: 1, LostToStack: 1}) {
		t.Fatalf("summary = %+v, want the defer counted as infrastructure — a node that loses jobs is broken, not slow", sum)
	}
	if r := results[0]; !strings.Contains(r.Result.Reason, "re-dispatch") {
		t.Errorf("reason = %q, want the lost job and its re-dispatches named", r.Result.Reason)
	}
}

// TestRunTotalLedgerLossIsPublished (R4-6): the `if r.led != nil` guard meant a
// ledger that never OPENED incremented nothing, so LedgerRowsLost stayed 0 and
// omitempty dropped it — a run that recorded not one row published byte-for-byte
// what a run that recorded every row publishes. The doc promised "N of M
// attempted" while shipping only N.
func TestRunTotalLedgerLossIsPublished(t *testing.T) {
	logs := captureLog(t)
	cfg := testCfg(t)
	// A DIRECTORY where the ledger file belongs: ledger.Open's O_WRONLY open
	// fails on every platform — the total-loss shape a read-only or full disk
	// produces in production.
	cfg.LedgerPath = filepath.Join(t.TempDir(), "ledger-as-a-dir")
	if err := os.MkdirAll(cfg.LedgerPath, 0o755); err != nil {
		t.Fatal(err)
	}
	local := func(ctx context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: 1, NodeID: "this-box", Seat: "local-seat",
			Output: "the qube answer", Structured: json.RawMessage(`{"answer":"42"}`), StopReason: "done"}, nil
	}
	results, sum, err := Run(t.Context(), cfg, local, []core.AgentContract{remoteContract(), remoteContract()}, "local", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 2 {
		t.Fatalf("summary = %+v, want both subtasks to succeed — telemetry never fails the work", sum)
	}
	blob, merr := json.Marshal(WireResponse(results, sum, nil))
	if merr != nil {
		t.Fatal(merr)
	}
	var published struct {
		Summary map[string]any `json:"summary"`
	}
	if uerr := json.Unmarshal(blob, &published); uerr != nil {
		t.Fatal(uerr)
	}
	if published.Summary["ledger_rows_lost"] != float64(2) {
		t.Errorf("ledger_rows_lost = %v, want 2 — a TOTAL ledger loss published as no loss at all", published.Summary["ledger_rows_lost"])
	}
	if published.Summary["ledger_rows_attempted"] != float64(2) {
		t.Errorf("ledger_rows_attempted = %v, want 2 — without M the caller cannot read the documented N-of-M",
			published.Summary["ledger_rows_attempted"])
	}
	if !strings.Contains(logs.String(), "ledger") {
		t.Errorf("log = %q, want the ledger loss named once", logs.String())
	}
}

// TestPollFailLogCountsEveryShapePastTheCap (R4-7): note() returned at the shape
// cap BEFORE incrementing the counter, so occurrences of the 9th and later
// shapes vanished — 36 failures presented as a 24-occurrence breakdown with
// nothing marking the omission, and the TEXT of those shapes appeared nowhere at
// all.
func TestPollFailLogCountsEveryShapePastTheCap(t *testing.T) {
	logs := captureLog(t)
	p := newPollFailLog("agd-test", "http://node:18811")
	const shapes, each = 12, 3
	for i := 0; i < shapes; i++ {
		for j := 0; j < each; j++ {
			p.note(fmt.Errorf("poll: unusable answer (status %d)", 500+i))
		}
	}
	if p.total != shapes*each {
		t.Fatalf("total = %d, want %d", p.total, shapes*each)
	}
	counted := 0
	for _, n := range p.counts {
		counted += n
	}
	if counted != p.total {
		t.Errorf("counts sum to %d of %d occurrences — every failure past the shape cap was dropped from the tally", counted, p.total)
	}
	p.summarize()
	out := logs.String()
	if !strings.Contains(out, "omitted") {
		t.Errorf("summary line = %q, presents a partial breakdown as a complete one", out)
	}
	residual := (shapes - pollFailShapeCap) * each
	if !strings.Contains(out, fmt.Sprintf("%d", residual)) {
		t.Errorf("summary line = %q, want the %d occurrences behind the omitted shapes counted", out, residual)
	}
}
