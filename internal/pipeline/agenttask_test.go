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
	rosterIDs []string
	loop      func(n int64) string // returns the raw chat-completions response body
	repack    func(n int64) string // content of the grammar completion (a JSON object string)
	// repackStatus, when non-zero, is returned for every grammar completion
	// instead of a body: the seat ANSWERED with that status rather than with a
	// result. 5xx = unreachable-class; 4xx = the seat refusing THIS request.
	repackStatus int
	// repackStatusFor, when set, wins over repackStatus and scripts the status
	// per attempt (1-based) — the seam a transport-THEN-validation test needs,
	// since the two attempts must fail differently.
	repackStatusFor func(n int64) int
	// repackEmptyChoices returns a 200 carrying zero choices: the seat is up
	// and answered, it just produced nothing.
	repackEmptyChoices bool
	// repackRawBody, when set, answers the grammar completion with a 200 whose
	// body is this raw text instead of a chat completion — the shape a proxy or
	// captive portal produces when it, not llama-server, is what answered.
	repackRawBody func(n int64) string
	// repackCutBody makes repackRawBody's answer arrive with a Content-Length
	// that LIES and the connection dropped mid-body: the failure then happens
	// during the body READ, after client.Do already succeeded, so no *url.Error
	// and no net.Error is anywhere in the returned error's chain.
	repackCutBody bool
	// repackDelay stalls every grammar completion — used to expire the
	// contract's wall deadline inside the re-pack.
	repackDelay time.Duration
	// rosterStatus, when non-zero, is the status /v1/models answers with.
	rosterStatus int
	loopCalls    atomic.Int64
	grammarCNT   atomic.Int64
}

func (f *agentFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			if f.rosterStatus != 0 {
				w.WriteHeader(f.rosterStatus)
				return
			}
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
			if f.repackDelay > 0 {
				time.Sleep(f.repackDelay)
			}
			status := f.repackStatus
			if f.repackStatusFor != nil {
				status = f.repackStatusFor(n)
			}
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			if f.repackRawBody != nil {
				body := f.repackRawBody(n)
				if !f.repackCutBody {
					w.Header().Set("Content-Type", "text/html")
					_, _ = w.Write([]byte(body))
					return
				}
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Errorf("test server does not support hijacking; the cut-body shape cannot be scripted")
					return
				}
				conn, bufrw, herr := hj.Hijack()
				if herr != nil {
					t.Errorf("hijack: %v", herr)
					return
				}
				// Promise more bytes than we send, then drop the connection.
				fmt.Fprintf(bufrw, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body)+64, body)
				_ = bufrw.Flush()
				_ = conn.Close()
				return
			}
			if f.repackEmptyChoices {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":0}}`))
				return
			}
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

// TestRunAgentTaskSchemalessContractReturnsOutput is the DEFAULT idle-local
// path (route:local, and route:auto with an idle GPU — the quality-first path
// the whole design centers on). A schemaless contract is explicitly legal
// there: RunAgentContract's doc says so, delegate/gate.go makes the schema a
// REMOTE-eligibility condition only, contracts/README.md documents it, and the
// MCP InputSchema requires nothing but `goal`. With no output_schema there is
// nothing to re-pack, so the loop's answer must come back AS IS — Deferred
// false, Output populated, Structured empty — and the seat must never be asked
// for a grammar completion it was given no grammar for.
func TestRunAgentTaskSchemalessContractReturnsOutput(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { t.Error("re-pack ran for a contract with no output_schema"); return `{}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	contract := testContract()
	contract.OutputSchema = nil
	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract))
	wire := decodeWire(t, res)

	if wire.Deferred {
		t.Fatalf("a schemaless contract deferred (%s / %s) — the perfect answer %q was thrown away",
			wire.DeferClass, wire.Reason, wire.Output)
	}
	if wire.Output != "The answer is 42." {
		t.Fatalf("output = %q, want the loop's answer returned verbatim", wire.Output)
	}
	if len(wire.Structured) != 0 {
		t.Fatalf("structured = %s, want empty — no schema was asked for", wire.Structured)
	}
	if got := fake.grammarCNT.Load(); got != 0 {
		t.Fatalf("re-pack completions = %d, want 0 — an absent schema means the re-pack is SKIPPED, not attempted", got)
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
	if wire.DeferClass != core.DeferClassBudget {
		t.Fatalf("defer_class = %q, want %q — a wall ceiling is a BUDGET defer, not a broken box", wire.DeferClass, core.DeferClassBudget)
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
	if wire.DeferClass != core.DeferClassAbstention {
		t.Fatalf("defer_class = %q, want %q — the seat answered, its SHAPE was wrong", wire.DeferClass, core.DeferClassAbstention)
	}
	if got := fake.grammarCNT.Load(); got != 2 {
		t.Fatalf("re-pack attempts = %d, want exactly 2 (one retry)", got)
	}
	if wire.Output != "The answer is 42." {
		t.Fatalf("output = %q, want the loop text preserved on a schema defer", wire.Output)
	}
}

// TestRunAgentTaskRepackTransportFailureIsNotASchemaFailure (H-2): when the
// re-pack cannot REACH the seat, the defer must say so — its own prefix
// ("structured re-pack unreachable: ") and the infrastructure class. Filed
// under "output failed schema:" (the old behavior, one merged lastErr), a
// llama-swap 500 reads as a model that cannot follow a schema and sends the
// operator to rewrite a schema that was never the problem.
func TestRunAgentTaskRepackTransportFailureIsNotASchemaFailure(t *testing.T) {
	fake := &agentFake{
		rosterIDs:    []string{agentTestSeat},
		loop:         func(int64) string { return doneChat("The answer is 42.") },
		repack:       func(int64) string { return `{"answer":"42"}` },
		repackStatus: http.StatusInternalServerError,
	}
	srv := fake.server(t)
	defer srv.Close()

	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)

	if !wire.Deferred {
		t.Fatalf("want deferred, got %+v", wire)
	}
	if !strings.HasPrefix(wire.Reason, "structured re-pack unreachable: ") {
		t.Fatalf("reason = %q, want the transport-specific prefix", wire.Reason)
	}
	if wire.DeferClass != core.DeferClassInfrastructure {
		t.Fatalf("defer_class = %q, want %q", wire.DeferClass, core.DeferClassInfrastructure)
	}
	if got := fake.grammarCNT.Load(); got != 2 {
		t.Fatalf("re-pack attempts = %d, want 2 (the one retry still applies to transport)", got)
	}
	if wire.Output != "The answer is 42." {
		t.Fatalf("output = %q, want the loop text preserved", wire.Output)
	}
}

// TestRunAgentTaskRepack4xxIsAnAbstentionNotABrokenBox (C-E): decodeGenResult
// errors on EVERY non-200, and the re-pack re-packed any such error as a
// TRANSPORT failure — so a 400 "context length exceeded" or a bad-grammar 400
// came back as defer_class infrastructure, exited non-zero, and told the
// operator a box was broken when the real fix was a smaller context or a
// flatter schema. A 4xx is the seat REFUSING this request: model/contract
// side, i.e. an abstention under the stable "output failed schema:" prefix.
func TestRunAgentTaskRepack4xxIsAnAbstentionNotABrokenBox(t *testing.T) {
	fake := &agentFake{
		rosterIDs:    []string{agentTestSeat},
		loop:         func(int64) string { return doneChat("The answer is 42.") },
		repack:       func(int64) string { return `{"answer":"42"}` },
		repackStatus: http.StatusBadRequest,
	}
	srv := fake.server(t)
	defer srv.Close()

	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)

	if !wire.Deferred {
		t.Fatalf("want deferred, got %+v", wire)
	}
	if wire.DeferClass != core.DeferClassAbstention {
		t.Fatalf("defer_class = %q (reason %q), want %q — a 4xx is the seat refusing THIS request, not a box an operator has to fix",
			wire.DeferClass, wire.Reason, core.DeferClassAbstention)
	}
	if !strings.HasPrefix(wire.Reason, "output failed schema: ") {
		t.Fatalf("reason = %q, want the stable schema-side prefix", wire.Reason)
	}
	if !strings.Contains(wire.Reason, "400") {
		t.Fatalf("reason = %q, want the refusal's status still named for the operator", wire.Reason)
	}
}

// TestRunAgentTaskRepackEmptyChoicesIsAnAbstention (C-E): a 200 carrying zero
// choices means the seat is UP and answered — it just produced nothing. Filed
// as transport, it made a healthy endpoint read as unreachable.
func TestRunAgentTaskRepackEmptyChoicesIsAnAbstention(t *testing.T) {
	fake := &agentFake{
		rosterIDs:          []string{agentTestSeat},
		loop:               func(int64) string { return doneChat("The answer is 42.") },
		repack:             func(int64) string { return `{"answer":"42"}` },
		repackEmptyChoices: true,
	}
	srv := fake.server(t)
	defer srv.Close()

	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)

	if !wire.Deferred {
		t.Fatalf("want deferred, got %+v", wire)
	}
	if wire.DeferClass != core.DeferClassAbstention {
		t.Fatalf("defer_class = %q (reason %q), want %q — the endpoint answered 200; nothing about it is unreachable",
			wire.DeferClass, wire.Reason, core.DeferClassAbstention)
	}
	if !strings.HasPrefix(wire.Reason, "output failed schema: ") {
		t.Fatalf("reason = %q, want the stable schema-side prefix", wire.Reason)
	}
}

// TestRunAgentTaskRepackTransportThenValidationStaysInfrastructure (C-E,
// inverse): the transport flag was LAST-WINS, so a 500 on the first attempt
// followed by a wrong-shape answer on the retry ended classed abstention —
// "the model got the shape wrong" — while the box had in fact just failed a
// request. A transport failure that happened AT ALL is the operator's signal.
func TestRunAgentTaskRepackTransportThenValidationStaysInfrastructure(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"wrong":"shape"}` }, // attempt 2 fails validation
		repackStatusFor: func(n int64) int {
			if n == 1 {
				return http.StatusInternalServerError
			}
			return 0
		},
	}
	srv := fake.server(t)
	defer srv.Close()

	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)

	if !wire.Deferred {
		t.Fatalf("want deferred, got %+v", wire)
	}
	if wire.DeferClass != core.DeferClassInfrastructure {
		t.Fatalf("defer_class = %q (reason %q), want %q — the seat failed a request during this re-pack",
			wire.DeferClass, wire.Reason, core.DeferClassInfrastructure)
	}
	if !strings.HasPrefix(wire.Reason, "structured re-pack unreachable: ") {
		t.Fatalf("reason = %q, want the transport-specific prefix", wire.Reason)
	}
	if !strings.Contains(wire.Reason, "500") {
		t.Fatalf("reason = %q, want the transport failure itself named, not the retry's validation error", wire.Reason)
	}
}

// TestRunAgentTaskRepackParentCancellation: when the DELEGATOR (or the node's
// shutdown) cancels the parent context mid-re-pack, the failed request is a
// *url.Error like any dial refusal — so it read as a broken endpoint. Nothing on
// this box failed; the caller went away, which is a BUDGET shape.
//
// It asserts what the cancellation arm PRODUCES, not merely what it avoids. The
// earlier version checked `!= infrastructure` plus "cancel" in the reason, and
// both held with the arm deleted: the error then fell through to the default
// case as `abstention` with "output failed schema: … context canceled". So the
// test passed on code that had lost the behavior entirely — it was pinning
// genErrIsTransport's context.Canceled exclusion, never this arm.
func TestRunAgentTaskRepackParentCancellation(t *testing.T) {
	fake := &agentFake{
		rosterIDs:   []string{agentTestSeat},
		loop:        func(int64) string { return doneChat("The answer is 42.") },
		repack:      func(int64) string { return `{"answer":"42"}` },
		repackDelay: 3 * time.Second,
	}
	srv := fake.server(t)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(150*time.Millisecond, cancel)

	res := agentTestPipeline(t, srv.URL).Run(ctx, agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)

	if !wire.Deferred {
		t.Fatalf("want deferred, got %+v", wire)
	}
	if wire.DeferClass != core.DeferClassBudget {
		t.Fatalf("defer_class = %q (reason %q), want %q — a ceiling outside the model's control stopped the run; it is neither a broken box nor a model that got the shape wrong",
			wire.DeferClass, wire.Reason, core.DeferClassBudget)
	}
	if !strings.Contains(wire.Reason, "structured re-pack") || !strings.Contains(wire.Reason, "cancel") {
		t.Fatalf("reason = %q, want this arm's own message naming the cancelled re-pack (not the default arm's \"output failed schema\")", wire.Reason)
	}
}

// TestRunAgentTaskRepackDeadlineIsAWallTimeout (H-2): when the contract's wall
// expires DURING the re-pack, the defer is the wall-timeout shape — not a
// schema failure and not an "unreachable" endpoint. The delegator sizes future
// contracts off this shape, so mislabeling it teaches it the wrong lesson.
func TestRunAgentTaskRepackDeadlineIsAWallTimeout(t *testing.T) {
	fake := &agentFake{
		rosterIDs:   []string{agentTestSeat},
		loop:        func(int64) string { return doneChat("The answer is 42.") },
		repack:      func(int64) string { return `{"answer":"42"}` },
		repackDelay: 2500 * time.Millisecond, // past the 1s contract wall
	}
	srv := fake.server(t)
	defer srv.Close()

	contract := testContract()
	contract.TimeoutSec = 1
	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract))
	wire := decodeWire(t, res)

	if !wire.Deferred || !strings.HasPrefix(wire.Reason, "wall timeout after") {
		t.Fatalf("deferred/reason = %v/%q, want the wall-timeout defer", wire.Deferred, wire.Reason)
	}
	if wire.DeferClass != core.DeferClassBudget {
		t.Fatalf("defer_class = %q, want %q", wire.DeferClass, core.DeferClassBudget)
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
	if wire.DeferClass != core.DeferClassBudget {
		t.Fatalf("defer_class = %q, want %q", wire.DeferClass, core.DeferClassBudget)
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
	if wire.DeferClass != core.DeferClassConfig {
		t.Fatalf("defer_class = %q, want %q — a seat this node does not serve is a CONFIG defect, not an abstention", wire.DeferClass, core.DeferClassConfig)
	}
	if fake.loopCalls.Load() != 0 {
		t.Fatalf("loop chats = %d, want 0 (defer BEFORE any planner call)", fake.loopCalls.Load())
	}
}

// TestRunAgentTaskRosterProbeFailureIsLogged (M-4): the seat-residency probe
// fails OPEN by design (the loop's first chat call carries a better error), but
// failing open silently hid the first and clearest evidence that the endpoint
// is wrong or down — the operator then debugs the loop error with no idea the
// roster was already unreachable.
func TestRunAgentTaskRosterProbeFailureIsLogged(t *testing.T) {
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })

	fake := &agentFake{
		rosterStatus: http.StatusInternalServerError, // the probe cannot answer
		loop:         func(int64) string { return doneChat("The answer is 42.") },
		repack:       func(int64) string { return `{"answer":"42"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)

	if wire.Deferred {
		t.Fatalf("an unreachable roster must fail OPEN, not defer: %q", wire.Reason)
	}
	if out := buf.String(); !strings.Contains(out, "roster probe") {
		t.Fatalf("log = %q, want the swallowed roster-probe failure surfaced", out)
	}
}

// TestRunAgentTaskLoopErrorIsInfrastructure: the planner endpoint failing
// mid-loop is an INFRASTRUCTURE defer — nothing was learned about the task and
// no retry of the same contract can help until the box is fixed. Classing it
// with abstentions is what lets a dead llama-swap read as "the small model
// couldn't do it".
func TestRunAgentTaskLoopErrorIsInfrastructure(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return `{"error":{"message":"model load failed"}}` },
		repack:    func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)

	if !wire.Deferred {
		t.Fatalf("want deferred, got %+v", wire)
	}
	if wire.DeferClass != core.DeferClassInfrastructure {
		t.Fatalf("defer_class = %q (reason %q), want %q", wire.DeferClass, wire.Reason, core.DeferClassInfrastructure)
	}
}

// TestRunAgentTaskUnknownProfileIsConfig: a contract naming a profile this
// build does not have can never run here — config, not abstention.
func TestRunAgentTaskUnknownProfileIsConfig(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("never reached") },
		repack:    func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	contract := testContract()
	contract.Profile = "no-such-profile"
	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract))
	wire := decodeWire(t, res)

	if !wire.Deferred || wire.DeferClass != core.DeferClassConfig {
		t.Fatalf("deferred/class = %v/%q (reason %q), want a config defer", wire.Deferred, wire.DeferClass, wire.Reason)
	}
}

// TestRunAgentTaskRepackWireFailuresAreInfrastructure (R4-3): genErrIsTransport
// recognized only *url.Error, net.Error and a 5xx — and llamaclient returned the
// JSON decoder's error UNWRAPPED for a body it could not read or parse, which
// happens AFTER client.Do already succeeded, so neither of those two types is
// anywhere in the chain. Measured against scripted servers, three real WIRE
// failures came back as abstentions at exit 0 ("the model got the shape wrong"):
//
//   - a proxy / captive portal answering 200 with an HTML page,
//   - a 200 whose connection died mid-body (a Content-Length that lied),
//   - a 429 from a rate limiter sitting in front of the seat.
//
// A non-JSON body from something claiming to be llama-server means SOMETHING
// ELSE ANSWERED, and llama-server itself never emits 429 — both are the wire,
// not the request. Default to loud: the operator's box is what needs a look.
func TestRunAgentTaskRepackWireFailuresAreInfrastructure(t *testing.T) {
	cases := []struct {
		name    string
		script  func(*agentFake)
		wantSub string
	}{
		{
			name: "a proxy answers 200 with an HTML error page",
			script: func(f *agentFake) {
				f.repackRawBody = func(int64) string { return "<html><body>502 Bad Gateway</body></html>" }
			},
			wantSub: "body",
		},
		{
			name: "the connection dies mid-body (Content-Length lied)",
			script: func(f *agentFake) {
				f.repackRawBody = func(int64) string { return `{"choices":[{"message":{"role":"assis` }
				f.repackCutBody = true
			},
			wantSub: "body",
		},
		{
			name: "a rate limiter answers 429",
			script: func(f *agentFake) { f.repackStatus = http.StatusTooManyRequests },
			wantSub: "429",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &agentFake{
				rosterIDs: []string{agentTestSeat},
				loop:      func(int64) string { return doneChat("The answer is 42.") },
				repack:    func(int64) string { return `{"answer":"42"}` },
			}
			tc.script(fake)
			srv := fake.server(t)
			defer srv.Close()

			res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
			wire := decodeWire(t, res)

			if !wire.Deferred {
				t.Fatalf("want deferred, got %+v", wire)
			}
			if wire.DeferClass != core.DeferClassInfrastructure {
				t.Fatalf("defer_class = %q (reason %q), want %q — the seat was never REACHED; something else answered for it",
					wire.DeferClass, wire.Reason, core.DeferClassInfrastructure)
			}
			if !strings.HasPrefix(wire.Reason, "structured re-pack unreachable: ") {
				t.Fatalf("reason = %q, want the transport-specific prefix, not the schema one", wire.Reason)
			}
			if !strings.Contains(wire.Reason, tc.wantSub) {
				t.Fatalf("reason = %q, want the wire failure itself named (%q)", wire.Reason, tc.wantSub)
			}
		})
	}
}
