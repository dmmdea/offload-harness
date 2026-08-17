// Task 4 (multi-node agent delegation): runAgentTask — the node-side executor
// of a fleet "agent" contract. Faked entirely over HTTP: agent.Build's loop
// client and the structured re-pack both speak /v1/chat/completions against an
// httptest server (zero-diff on internal/agent — no injection point added).
// The handler tells the two apart by shape: the LOOP's chat requests carry a
// "tools" array; the re-pack Generate carries a "grammar" and no tools.
//
// The load-bearing pins: a defer is a SUCCESS shape at the job level (res.OK
// true, wire.Deferred true — the fleet job must terminal-DONE, never error),
// and the schema re-pack retries exactly ONCE before deferring.
package pipeline

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
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

const agentTestSeat = "seat-x"

// agentFake is the scripted llama endpoint for one test: rosterIDs feeds
// /v1/models; loop answers the tool-calling chat requests (nth call, 1-based);
// repack answers the grammar-constrained re-pack completions.
type agentFake struct {
	rosterIDs  []string
	loop       func(n int64) string // returns the raw chat-completions response body
	repack     func(n int64) string // content of the grammar completion (a JSON object string)
	loopCalls  atomic.Int64
	grammarCNT atomic.Int64
}

func (f *agentFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			type m struct {
				ID string `json:"id"`
			}
			var data []m
			for _, id := range f.rosterIDs {
				data = append(data, m{ID: id})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
		case "/v1/chat/completions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, hasTools := body["tools"]; hasTools {
				n := f.loopCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(f.loop(n)))
				return
			}
			if g, _ := body["grammar"].(string); g == "" {
				t.Errorf("chat request with neither tools nor grammar: %v", body)
			}
			n := f.grammarCNT.Add(1)
			content, _ := json.Marshal(f.repack(n))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":` + string(content) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":7}}`))
		default:
			// /upstream/... /props, /tokenize, ...: absent — every consumer
			// fails open (window fallback, legacy tokenizer rung).
			http.NotFound(w, r)
		}
	}))
}

func doneChat(content string) string {
	b, _ := json.Marshal(content)
	return `{"choices":[{"message":{"role":"assistant","content":` + string(b) + `},"finish_reason":"stop"}]}`
}

// toolChat is an assistant turn that calls list_dir — used to burn steps so
// the loop exits on its step budget.
func toolChat(n int64) string {
	return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c` +
		jsonNum(n) + `","type":"function","function":{"name":"list_dir","arguments":"{\"path\":\".\"}"}}]},"finish_reason":"tool_calls"}]}`
}

func jsonNum(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func agentTestPipeline(t *testing.T, base string) *Pipeline {
	t.Helper()
	cfg := config.Config{
		Endpoint:    base,
		Model:       "workhorse",
		AgentModel:  agentTestSeat,
		FleetNodeID: "node-t",
		Temperature: 0.1,
	}
	return New(cfg, llamaclient.New(base, "", cfg.Model, 30*time.Second), nil, nil)
}

// agentTestRequest builds the core.Request exactly as fleetnode.buildAgentRun
// hands it over: decoded contract + a materialized context dir.
func agentTestRequest(t *testing.T, contract core.AgentContract) core.Request {
	t.Helper()
	ctxDir := filepath.Join(t.TempDir(), "context")
	if err := os.MkdirAll(ctxDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range contract.Context {
		if err := os.WriteFile(filepath.Join(ctxDir, d.Name), []byte(d.Text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return core.Request{
		Task:  core.TaskAgentRun,
		Input: contract.Goal,
		Params: map[string]any{
			"contract":    contract,
			"context_dir": ctxDir,
			"job_id":      "agent-test",
		},
	}
}

func testContract() core.AgentContract {
	return core.AgentContract{
		SchemaVersion: core.AgentWireSchemaVersion,
		Goal:          "answer the question",
		Context:       []core.ContextDoc{{Name: "notes.md", Text: "the answer is 42"}},
		OutputSchema:  json.RawMessage(`{"properties":{"answer":{"type":"string"}},"required":["answer"]}`),
		MaxSteps:      4,
		TimeoutSec:    30,
		Depth:         1,
	}
}

func decodeWire(t *testing.T, res core.Result) core.AgentWireResult {
	t.Helper()
	if !res.OK {
		t.Fatalf("res.OK = false (reason %q) — every terminal agent outcome, defers included, must be a job-level SUCCESS", res.Reason)
	}
	var wire core.AgentWireResult
	if err := json.Unmarshal(res.Data, &wire); err != nil {
		t.Fatalf("result data is not an AgentWireResult (%v): %s", err, res.Data)
	}
	return wire
}

func TestRunAgentTaskHappyPath(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)

	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	if wire.SchemaVersion != core.AgentWireSchemaVersion {
		t.Fatalf("schema_version = %d", wire.SchemaVersion)
	}
	if wire.NodeID != "node-t" || wire.Seat != agentTestSeat {
		t.Fatalf("node/seat = %q/%q", wire.NodeID, wire.Seat)
	}
	if wire.Output != "The answer is 42." {
		t.Fatalf("output = %q", wire.Output)
	}
	if wire.Steps != 1 || wire.StopReason != "done" {
		t.Fatalf("steps/stop = %d/%q", wire.Steps, wire.StopReason)
	}
	var structured map[string]string
	if err := json.Unmarshal(wire.Structured, &structured); err != nil || structured["answer"] != "42" {
		t.Fatalf("structured = %s (%v)", wire.Structured, err)
	}
	if wire.TokensOut != 7 {
		t.Fatalf("tokens_out = %d, want the re-pack completion's usage (7)", wire.TokensOut)
	}
	if fake.grammarCNT.Load() != 1 {
		t.Fatalf("re-pack completions = %d, want exactly 1", fake.grammarCNT.Load())
	}
}

// TestRunAgentTaskTimeoutDefersNotErrors: the contract's TimeoutSec is a ctx
// deadline; hitting it yields a DEFERRED wire result on a job-level success —
// the fleet job must land terminal-done, never error.
func TestRunAgentTaskTimeoutDefersNotErrors(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(int64) string {
			time.Sleep(2500 * time.Millisecond) // well past the 1s wall
			return doneChat("too late")
		},
		repack: func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	contract := testContract()
	contract.TimeoutSec = 1
	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract))
	wire := decodeWire(t, res)

	if !wire.Deferred {
		t.Fatalf("want deferred, got: %+v", wire)
	}
	if !strings.Contains(wire.Reason, "timeout") {
		t.Fatalf("reason = %q, want a wall-timeout reason", wire.Reason)
	}
}

// TestRunAgentTaskSchemaFailRetriesOnceThenDefers: the structured re-pack
// gets exactly ONE retry; a second schema failure defers with the stable
// "output failed schema" prefix while the loop's text Output is preserved
// (the delegator's text-verb acceptance checks still have something to read).
func TestRunAgentTaskSchemaFailRetriesOnceThenDefers(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"wrong":"shape"}` }, // fails required:["answer"] every time
	}
	srv := fake.server(t)
	defer srv.Close()

	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)

	if !wire.Deferred || !strings.HasPrefix(wire.Reason, "output failed schema") {
		t.Fatalf("deferred/reason = %v/%q, want the output-failed-schema defer", wire.Deferred, wire.Reason)
	}
	if got := fake.grammarCNT.Load(); got != 2 {
		t.Fatalf("re-pack attempts = %d, want exactly 2 (one retry)", got)
	}
	if wire.Output != "The answer is 42." {
		t.Fatalf("output = %q, want the loop text preserved on a schema defer", wire.Output)
	}
}

// TestRunAgentTaskBudgetDefers: a loop that burns every step on tool calls
// stops on "budget" — a defer shape, not a success with empty output.
func TestRunAgentTaskBudgetDefers(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      toolChat, // every turn calls list_dir; never finishes
		repack:    func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	contract := testContract()
	contract.MaxSteps = 2
	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract))
	wire := decodeWire(t, res)

	if !wire.Deferred || !strings.Contains(wire.Reason, "step budget") {
		t.Fatalf("deferred/reason = %v/%q, want a step-budget defer", wire.Deferred, wire.Reason)
	}
	if wire.StopReason != "budget" {
		t.Fatalf("stop_reason = %q", wire.StopReason)
	}
	if fake.grammarCNT.Load() != 0 {
		t.Fatal("a budget defer must not spend a re-pack completion")
	}
}

// TestRunAgentTaskSeatUnservedDefers: a roster that answers WITHOUT the seat
// defers before any planner call (mirror of mcpserver's plannerUnserved gate).
func TestRunAgentTaskSeatUnservedDefers(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{"some-other-model"},
		loop:      func(int64) string { return doneChat("never reached") },
		repack:    func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)

	if !wire.Deferred || !strings.Contains(wire.Reason, "roster") {
		t.Fatalf("deferred/reason = %v/%q, want a seat-unserved defer", wire.Deferred, wire.Reason)
	}
	if fake.loopCalls.Load() != 0 {
		t.Fatalf("loop chats = %d, want 0 (defer BEFORE any planner call)", fake.loopCalls.Load())
	}
}
