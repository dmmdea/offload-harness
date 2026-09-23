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

	"github.com/dmmdea/offload-harness/internal/agent"
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
	// loopStatus, when set, is the HTTP status the n-th LOOP completion
	// answers with (0 = 200, as every older test gets); the body is still
	// f.loop(n), which is how llama.cpp answers a refused tool call: a 500
	// carrying the parse error (register D-114).
	loopStatus func(n int64) int
	// loopStream, when set, answers the n-th LOOP completion itself (a
	// streamed seat) and receives the decoded request body; nil = f.loop.
	loopStream func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request)
	repack     func(n int64) string // content of the grammar completion (a JSON object string)
	// repackStatus, when non-zero, is returned for every grammar completion
	// instead of a body: the seat ANSWERED with that status rather than with a
	// result. 5xx = unreachable-class; 4xx = the seat refusing THIS request.
	repackStatus int
	// running, when set, scripts GET /running (llama-swap's residency view)
	// for the admission pre-flight; n counts the polls. Unset = 404, which the
	// pre-flight treats as "probe failed, proceed" — so every older test keeps
	// its exact shape.
	running    func(n int64) string
	runningCNT atomic.Int64
	// runningStatus, when non-zero, is the status GET /running answers with
	// instead of a body — llama-swap answering 500, the shape that makes every
	// residency read FAIL rather than report "the seat is absent" (register S-24).
	runningStatus int
	// seatName overrides agentTestSeat as the seat this fake serves on its
	// per-model passthrough routes (/upstream/<seat>/…). Empty = agentTestSeat,
	// which is what every test that does not name its own seat wants.
	seatName string
	// rosterAliases maps a canonical roster id to the names llama-swap also
	// answers to for it, published under meta.llamaswap.aliases exactly as the
	// real /v1/models does. Unset = no alias block at all, so every older test
	// sees the byte-identical roster it always saw.
	rosterAliases map[string][]string
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
	// repackThinkingSeat models a THINKING agent seat (Qwen3-class, e.g. Qube's
	// own qwen3.8-27b) as measured live: with a grammar active and thinking
	// left ON, the constrained output lands in `reasoning_content` and
	// `content` comes back EMPTY. Sending
	// chat_template_kwargs.enable_thinking=false makes the same seat answer in
	// `content`. When set, every grammar completion that does NOT carry the
	// flag answers empty-content; the ones that do are answered normally.
	repackThinkingSeat bool
	// repackBodies records every grammar completion's decoded request body, so
	// a test can assert on what the re-pack actually sent.
	repackBodies chan map[string]any
	// repackTruncated, when set, marks the n-th grammar completion as cut at
	// max_tokens (finish_reason "length") — the 0.115.10 truncation shape.
	repackTruncated func(n int64) bool
	// upstreamCNT counts GETs on /upstream/<seat>/v1/models — the warm-up
	// request (0.115.11); the fake answers 404 like a llama-swap that does
	// not know the seat, unless upstreamModels is set.
	upstreamCNT    atomic.Int64
	upstreamModels func(n int64) string
	// rosterStatus, when non-zero, is the status /v1/models answers with.
	rosterStatus int
	// props, when non-nil, is served (as JSON) at the seat's
	// /upstream/{seat}/props passthrough — the A1 pin probe's source. Nil
	// keeps the historical 404, under which every consumer fails open and the
	// pin stays absent.
	props    any
	propsCNT atomic.Int64
	// propsDelay stalls the FIRST /upstream/<seat>/props answer — the
	// served-window probe's own cold-start cost, which must be paid out of the
	// ADMISSION budget and never out of the contract's wall (register S-24).
	// Only the first, because the post-run seat-PIN probe reads the same route
	// on a seat that is warm by then, and stalling it would spend the wall the
	// window probe just stopped spending.
	propsDelay time.Duration
	loopCalls  atomic.Int64
	grammarCNT atomic.Int64
	// chatFallback scripts the grammar-FREE re-pack lane (repackViaChat): a
	// chat request with neither tools nor grammar. nil = the route 404s, so
	// every pre-fallback test keeps its exact outcome.
	chatFallback    func(int64) string
	chatFallbackCNT atomic.Int64
	// chatFallbackDelay stalls every chat-fallback completion — the mirror of
	// repackDelay for the grammar lane, used to expire the contract's wall
	// deadline DURING the chat fallback (register D-108: the chat fallback is
	// the re-pack's own LAST attempt, so it is the one a wall-timeout test
	// must stall). 0 = no delay, every pre-existing test's exact timing.
	chatFallbackDelay time.Duration
	// chatFallbackStatus, when non-zero and chatFallback is nil, is the status
	// the chat-fallback lane answers with instead of its usual 404 — a test's
	// way of making the re-pack's LAST attempt (register D-108: the re-pack
	// now decides its class from the LAST attempt alone, PR #366 correctness
	// review) carry the same wire failure the grammar lane already carries,
	// since an unconfigured chat lane's plain 404 would otherwise always win
	// the verdict for a scripted-failure test that never meant to test the
	// chat lane at all.
	chatFallbackStatus int
	// probe answers the admission-time COHERENCE probe (register D-118),
	// recognised by its SHAPE rather than by a counter: exactly one user
	// message opening with the probe goal. Routing it away from loop(n) is what
	// keeps every older test's call indexing unchanged — the probe is a chat
	// request carrying tools, so without this it would have become loop call 1
	// everywhere it fires. nil = a parsed read_file tool call, i.e. a coherent
	// seat, which is what every test that is not ABOUT the probe wants.
	probe func(n int64) string
	// probeStatus, when set, is the HTTP status the n-th probe answers with
	// (0 = 200) — the transport-failure arm, which must FAIL OPEN.
	probeStatus func(n int64) int
	probeCNT    atomic.Int64
	// upstreamAll counts EVERY request to the per-model passthrough
	// /upstream/<model>/… — on a real llama-swap each one starts the model when
	// it is not loaded, which is what the GPU-lease fence tests count.
	upstreamAll atomic.Int64
	// tokenize, when set, serves the seat's /upstream/<seat>/tokenize
	// passthrough (a warm seat's real tokenizer); nil keeps the historical 404.
	tokenize http.HandlerFunc
}

// isCoherenceProbeCall recognises the D-118 probe request by its shape. It must
// stay in step with pipeline.coherenceProbeGoal; the probe test asserts the
// count, so a drift shows up as "the fake never saw a probe" rather than
// silently reclassifying it as a loop step.
func isCoherenceProbeCall(body map[string]any) bool {
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		return false
	}
	m, _ := msgs[0].(map[string]any)
	content, _ := m["content"].(string)
	return m["role"] == "user" && strings.HasPrefix(content, "Read the file notes.md")
}

// probeToolCall is the coherent answer: a parsed read_file tool call, the
// strongest pass the probe can get.
func probeToolCall() string {
	return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"probe","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"notes.md\"}"}}]},"finish_reason":"tool_calls"}]}`
}

// repackDisablesThinking reports whether a captured grammar-completion body
// carries chat_template_kwargs.enable_thinking=false — the exact wire shape the
// live fix depends on.
// isForcedFinalCall reports whether a chat request is the loop's forced final
// step: its last message is the agent package's answer-now turn.
func isForcedFinalCall(body map[string]any) bool {
	msgs, _ := body["messages"].([]any)
	if len(msgs) == 0 {
		return false
	}
	last, _ := msgs[len(msgs)-1].(map[string]any)
	content, _ := last["content"].(string)
	return last["role"] == "user" && content == agent.FinalAnswerTurn
}

func repackDisablesThinking(body map[string]any) bool {
	kw, ok := body["chat_template_kwargs"].(map[string]any)
	if !ok {
		return false
	}
	et, ok := kw["enable_thinking"].(bool)
	return ok && !et
}

// seat is the model name this fake serves on its per-model passthrough routes.
func (f *agentFake) seat() string {
	if f.seatName != "" {
		return f.seatName
	}
	return agentTestSeat
}

func (f *agentFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if os.Getenv("AGENTFAKE_TRACE") != "" {
			log.Printf("FAKE %s %s", r.Method, r.URL.Path)
		}
		if strings.HasPrefix(r.URL.Path, "/upstream/") {
			f.upstreamAll.Add(1)
		}
		switch r.URL.Path {
		case "/running":
			if f.runningStatus != 0 {
				f.runningCNT.Add(1)
				w.WriteHeader(f.runningStatus)
				return
			}
			if f.running == nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(f.running(f.runningCNT.Add(1))))
		case "/v1/models":
			if f.rosterStatus != 0 {
				w.WriteHeader(f.rosterStatus)
				return
			}
			// meta.llamaswap.aliases is where llama-swap publishes the names a
			// model ALSO answers to; the roster reader resolves a bound alias to
			// the canonical id through exactly this block.
			type meta struct {
				Llamaswap struct {
					Aliases []string `json:"aliases,omitempty"`
				} `json:"llamaswap"`
			}
			type m struct {
				ID   string `json:"id"`
				Meta *meta  `json:"meta,omitempty"`
			}
			var data []m
			for _, id := range f.rosterIDs {
				e := m{ID: id}
				if al := f.rosterAliases[id]; len(al) > 0 {
					e.Meta = &meta{}
					e.Meta.Llamaswap.Aliases = al
				}
				data = append(data, e)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
		case "/v1/chat/completions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			// A loop call carries tools — except the forced final step
			// (0.115.19, D-89), which offers none and opens with the
			// answer-now turn; recognise it by that turn, not by tools.
			if isCoherenceProbeCall(body) {
				n := f.probeCNT.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if f.probeStatus != nil {
					if st := f.probeStatus(n); st != 0 {
						w.WriteHeader(st)
						return
					}
				}
				if f.probe == nil {
					_, _ = w.Write([]byte(probeToolCall()))
					return
				}
				_, _ = w.Write([]byte(f.probe(n)))
				return
			}
			if _, hasTools := body["tools"]; hasTools || isForcedFinalCall(body) {
				n := f.loopCalls.Add(1)
				if f.loopStream != nil {
					f.loopStream(n, body, w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if f.loopStatus != nil {
					if st := f.loopStatus(n); st != 0 {
						w.WriteHeader(st)
					}
				}
				_, _ = w.Write([]byte(f.loop(n)))
				return
			}
			if g, _ := body["grammar"].(string); g == "" {
				// The grammar-free chat fallback lane (repackViaChat).
				n := f.chatFallbackCNT.Add(1)
				if f.chatFallback != nil {
					if f.chatFallbackDelay > 0 {
						time.Sleep(f.chatFallbackDelay)
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(f.chatFallback(n)))
					return
				}
				if f.chatFallbackStatus != 0 {
					w.WriteHeader(f.chatFallbackStatus)
					return
				}
				if f.repackRawBody != nil {
					// The re-pack's LAST attempt decides its class (register
					// D-108, PR #366 correctness review), and an unconfigured
					// chat lane's plain 404 would always win that verdict for a
					// test that scripted the GRAMMAR lane's wire failure and
					// never meant to test the chat lane at all — so a test that
					// scripts repackRawBody without its own chatFallback gets
					// the SAME wire-level failure on both lanes.
					writeRawOrCutBody(t, w, f.repackRawBody(n), f.repackCutBody)
					return
				}
				http.NotFound(w, r)
				return
			}
			n := f.grammarCNT.Add(1)
			if f.repackBodies != nil {
				select {
				case f.repackBodies <- body:
				default: // never block the seat on an un-drained recorder
				}
			}
			if f.repackThinkingSeat && !repackDisablesThinking(body) {
				// The live defect: grammar + thinking template ⇒ the answer is
				// emitted into reasoning_content and content is empty, which
				// the re-pack then hands to json.Unmarshal as "".
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"The user wants the answer field. It is 42."},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":67}}`))
				return
			}
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
				writeRawOrCutBody(t, w, f.repackRawBody(n), f.repackCutBody)
				return
			}
			if f.repackEmptyChoices {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":0}}`))
				return
			}
			content, _ := json.Marshal(f.repack(n))
			finish := "stop"
			if f.repackTruncated != nil && f.repackTruncated(n) {
				finish = "length"
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":` + string(content) + `},"finish_reason":"` + finish + `"}],"usage":{"prompt_tokens":10,"completion_tokens":7}}`))
		default:
			if f.tokenize != nil && r.URL.Path == "/upstream/"+f.seat()+"/tokenize" {
				f.tokenize(w, r)
				return
			}
			if f.props != nil && r.URL.Path == "/upstream/"+f.seat()+"/props" {
				if f.propsCNT.Add(1) == 1 && f.propsDelay > 0 {
					time.Sleep(f.propsDelay)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(f.props)
				return
			}
			if r.URL.Path == "/upstream/"+f.seat()+"/v1/models" {
				n := f.upstreamCNT.Add(1)
				if f.upstreamModels != nil {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(f.upstreamModels(n)))
					return
				}
			}
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

// writeRawOrCutBody answers a request with body verbatim (a proxy/captive
// portal shape — text/html, not a chat completion), or, when cut is true,
// with a Content-Length that LIES and the connection dropped mid-body (the
// failure then happens during the body READ, after client.Do already
// succeeded, so no *url.Error / net.Error is anywhere in the returned
// error's chain). Shared by the grammar and chat-fallback lanes so a test
// can script the SAME wire-level failure shape on whichever lane turns out
// to be the re-pack's decisive last attempt (register D-108).
func writeRawOrCutBody(t *testing.T, w http.ResponseWriter, body string, cut bool) {
	t.Helper()
	if !cut {
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
// Liveness walls (0.131.0, ADR 0055): the contract's wall is the EXPECTATION.
// A seat that answers 2.5 s into a declared 1 s wall is slow, not dead — the
// run completes. Until 0.130.x this exact fixture was the wall-timeout test.
func TestRunAgentTaskSlowSeatOutlivesTheDeclaredWall(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(int64) string {
			time.Sleep(2500 * time.Millisecond) // well past the 1s "wall"
			return doneChat("not too late any more")
		},
		repack: func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	contract := testContract()
	contract.TimeoutSec = 1
	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract))
	wire := decodeWire(t, res)

	if wire.Deferred {
		t.Fatalf("a producing run was killed by its expectation: %q (%s)", wire.Reason, wire.DeferClass)
	}
	if wire.WallSec != 0 && wire.WallSec != 1 {
		t.Fatalf("wall_sec = %d, the expectation must still be reported", wire.WallSec)
	}
	if wire.CeilingSec < core.AgentCeilingSecFloor || wire.StallAllowanceSec == 0 || wire.LastProgressMs == 0 {
		t.Fatalf("liveness telemetry missing on the wire: ceiling=%d allowance=%d last=%d", wire.CeilingSec, wire.StallAllowanceSec, wire.LastProgressMs)
	}
}

// A seat that accepts the request and never answers is a STALL: filed as
// infrastructure (the seat's health), with the allowance arithmetic in the
// reason, never as the budget signal the delegator sizes from.
func TestRunAgentTaskSilentSeatIsAStallNotABudgetDefer(t *testing.T) {
	restore := compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)
	defer restore()
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(int64) string {
			time.Sleep(8 * time.Second) // far past any prefill allowance a test prompt earns
			return doneChat("never read")
		},
		repack: func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	contract := testContract()
	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract))
	wire := decodeWire(t, res)

	if !wire.Deferred || !strings.HasPrefix(wire.Reason, "stalled: no progress for ") {
		t.Fatalf("want a stall, got deferred=%v reason=%q", wire.Deferred, wire.Reason)
	}
	if wire.DeferClass != core.DeferClassInfrastructure {
		t.Fatalf("defer_class = %q, want %q — a silent seat is the seat's health, not the contract's budget", wire.DeferClass, core.DeferClassInfrastructure)
	}
	if !strings.Contains(wire.Reason, "in prefill (allowed ") {
		t.Fatalf("the reason must carry the phase and the allowance: %q", wire.Reason)
	}
}

// A run that reaches the safety ceiling is a BUDGET defer with the ceiling's
// own words — the sizing signal — never the retired "wall timeout".
func TestRunAgentTaskCeilingIsABudgetDefer(t *testing.T) {
	restore := compressLiveness(t, 60*time.Second, 30*time.Second, 2) // ceiling capped at 2 s
	defer restore()
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(int64) string {
			time.Sleep(4 * time.Second) // past the 2 s ceiling, inside the 60 s stall floor
			return doneChat("past the ceiling")
		},
		repack: func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	contract := testContract()
	contract.TimeoutSec = 1
	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract))
	wire := decodeWire(t, res)

	if !wire.Deferred || !strings.HasPrefix(wire.Reason, "ceiling ") {
		t.Fatalf("want the ceiling defer, got deferred=%v reason=%q", wire.Deferred, wire.Reason)
	}
	if wire.DeferClass != core.DeferClassBudget {
		t.Fatalf("defer_class = %q, want %q", wire.DeferClass, core.DeferClassBudget)
	}
	if strings.Contains(wire.Reason, "wall timeout") {
		t.Fatalf("the retired reason must not come back: %q", wire.Reason)
	}
}

// compressLiveness sets the package's liveness knobs for one test and hands
// back the restore. Production values: floor 60 s, slack 30 s, ceiling floor
// core.AgentCeilingSecFloor.
func compressLiveness(t *testing.T, floor, slack time.Duration, ceilingCap int) func() {
	t.Helper()
	f, s, fl, c := livenessFloor, livenessSlack, ceilingFloorSec, ceilingCapSec
	livenessFloor, livenessSlack, ceilingFloorSec, ceilingCapSec = floor, slack, 1, ceilingCap
	return func() { livenessFloor, livenessSlack, ceilingFloorSec, ceilingCapSec = f, s, fl, c }
}

// TestRunAgentTaskSchemaFailRetriesOnceThenDefers: the structured re-pack
// gets exactly ONE retry; a second schema failure defers with the stable
// "output failed schema" prefix while the loop's text Output is preserved
// (the CALLER still receives the loop's answer — not delegator-side acceptance,
// which never runs over a deferred result).
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
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
		// Both lanes unreachable (register D-108, PR #366 correctness review):
		// the re-pack's LAST attempt decides its class, so an unconfigured chat
		// lane's plain 404 would otherwise win over the grammar lane's 500 and
		// read as an abstention instead of the infrastructure this test names.
		repackStatus:       http.StatusInternalServerError,
		chatFallbackStatus: http.StatusInternalServerError,
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

// TestRunAgentTaskRepackLastAttemptTransportOutranksEarlierValidation (C-E,
// inverse; rewritten for register D-108, PR #366 correctness review): this
// test used to pin "the transport flag is LAST-WINS across every attempt" —
// a 500 on attempt 1 stayed infrastructure even after a later attempt merely
// answered the wrong shape, because "a transport failure that happened AT
// ALL is the operator's signal." That rule is gone on purpose: EARLIER
// attempts are now diagnostic notes only, and the re-pack's FINAL, DECISIVE
// attempt alone decides the class (a self-imposed per-attempt cutoff earlier
// in the run must never read as a broken box either — the same correction,
// mirrored). This test now proves the surviving half of the original claim
// the right way round: two EARLIER validation failures do not paper over a
// LATER, genuine transport failure — the seat that never answered the FINAL
// attempt is still reported as unreachable, not as "the model got the shape
// wrong" using a stale, earlier error.
func TestRunAgentTaskRepackLastAttemptTransportOutranksEarlierValidation(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"wrong":"shape"}` }, // both grammar attempts fail validation
		// The chat fallback — the re-pack's LAST, decisive attempt — is
		// genuinely unreachable.
		chatFallbackStatus: http.StatusInternalServerError,
	}
	srv := fake.server(t)
	defer srv.Close()

	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)

	if !wire.Deferred {
		t.Fatalf("want deferred, got %+v", wire)
	}
	if wire.DeferClass != core.DeferClassInfrastructure {
		t.Fatalf("defer_class = %q (reason %q), want %q — the LAST attempt failed a request during this re-pack",
			wire.DeferClass, wire.Reason, core.DeferClassInfrastructure)
	}
	if !strings.HasPrefix(wire.Reason, "structured re-pack unreachable: ") {
		t.Fatalf("reason = %q, want the transport-specific prefix", wire.Reason)
	}
	if !strings.Contains(wire.Reason, "500") {
		t.Fatalf("reason = %q, want the LAST attempt's transport failure named", wire.Reason)
	}
	if !strings.Contains(wire.Reason, "attempt 3/3") {
		t.Fatalf("reason = %q, want the decisive attempt named (attempt 3/3)", wire.Reason)
	}
	if got := fake.grammarCNT.Load(); got != 2 {
		t.Fatalf("grammar attempts = %d, want 2 (both fail validation before the chat fallback)", got)
	}
	if got := fake.chatFallbackCNT.Load(); got != 1 {
		t.Fatalf("chat-fallback attempts = %d, want 1", got)
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
//
// Every lane hangs past the 1 s wall (register D-108, W-19): since the
// re-pack's per-attempt bound now splits the wall across the attempts still
// owed a turn, a seat that never answers must be given every chance to prove
// it — grammar AND the chat fallback — or an early, fast-failing lane (the
// old fixture's un-configured chat fallback, a fast 404) would let the run
// finish with wall to spare and never exercise the wall-timeout shape at all.
func TestRunAgentTaskRepackDeadlineIsACeilingDefer(t *testing.T) {
	restore := compressLiveness(t, 60*time.Second, 30*time.Second, 2) // ceiling capped at 2 s: under the 2.5 s re-pack delays
	defer restore()
	fake := &agentFake{
		rosterIDs:         []string{agentTestSeat},
		loop:              func(int64) string { return doneChat("The answer is 42.") },
		repack:            func(int64) string { return `{"answer":"42"}` },
		repackDelay:       2500 * time.Millisecond, // past the 1s contract wall
		chatFallback:      func(int64) string { return doneChat(`{"answer":"42"}`) },
		chatFallbackDelay: 2500 * time.Millisecond, // past the 1s contract wall
	}
	srv := fake.server(t)
	defer srv.Close()

	contract := testContract()
	contract.TimeoutSec = 1
	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract))
	wire := decodeWire(t, res)

	if !wire.Deferred || !strings.HasPrefix(wire.Reason, "ceiling ") {
		t.Fatalf("deferred/reason = %v/%q, want the ceiling defer", wire.Deferred, wire.Reason)
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
	// 0.115.19 (D-89): the last step was the forced final; the seat still
	// answered with a tool call, and the reason says so.
	if !strings.Contains(wire.Reason, "forced final step") || !strings.Contains(wire.StopNote, "forced final step") {
		t.Fatalf("reason/stop_note = %q/%q, want the forced-final evidence in both", wire.Reason, wire.StopNote)
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
			name:    "a rate limiter answers 429",
			script:  func(f *agentFake) { f.repackStatus = http.StatusTooManyRequests },
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

// TestRepackStructuredDisablesThinking pins the request the re-pack sends: the
// grammar AND chat_template_kwargs.enable_thinking=false, together. The re-pack
// is a mechanical shape transformation over text the loop already produced —
// there is nothing left to reason about — and on a thinking seat the two
// interact destructively (see TestRunAgentTaskThinkingSeatRepackSucceeds).
func TestRepackStructuredDisablesThinking(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(int64) string {
			t.Error("the agent loop must not run: this test calls the re-pack directly")
			return doneChat("")
		},
		repack:       func(int64) string { return `{"answer":"42"}` },
		repackBodies: make(chan map[string]any, 4),
	}
	srv := fake.server(t)
	defer srv.Close()

	p := agentTestPipeline(t, srv.URL)
	schema := json.RawMessage(`{"properties":{"answer":{"type":"string"}},"required":["answer"]}`)
	structured, _, _, _, err := p.repackStructured(context.Background(), agentTestSeat, schema, "The answer is 42.", 0)
	if err != nil {
		t.Fatalf("repackStructured: %v", err)
	}
	if !bytes.Contains(structured, []byte(`"42"`)) {
		t.Fatalf("structured = %s", structured)
	}

	select {
	case body := <-fake.repackBodies:
		if !repackDisablesThinking(body) {
			t.Fatalf("the re-pack did not ask for a NON-thinking completion: chat_template_kwargs = %#v", body["chat_template_kwargs"])
		}
		// The flag must ride ALONGSIDE the grammar, not replace it: the schema
		// seam is the grammar, and a re-pack that quietly dropped it would
		// still pass a thinking check while losing its shape constraint.
		if g, _ := body["grammar"].(string); g == "" {
			t.Fatalf("the re-pack sent no grammar: %#v", body)
		}
	default:
		t.Fatal("the re-pack sent no grammar completion at all")
	}
}

// TestRunAgentTaskThinkingSeatRepackSucceeds is the LIVE defect as a test.
// Found by end-to-end testing on real seats after 0.66.0 merged — no unit test
// could have caught it, because every one of them fakes the seat, and the
// interaction lives in the seat's chat template.
//
// Measured, same request at temp 0 / max_tokens 512 against llama-swap:
//
//	seat          grammar  len(content)  len(reasoning)
//	qwen3.8-27b   none      73 (valid)    326
//	qwen3.8-27b   GBNF       0            67     <- the defect
//	gemma-4-e4b   none      85             0
//	gemma-4-e4b   GBNF      67 (valid)     0
//
// A thinking template emits the grammar-constrained output into
// `reasoning_content` and leaves `content` empty, so the re-pack failed both
// attempts and the run deferred as an abstention — throwing away a finished,
// correct answer. Qube's own agent seat is a thinking model, so this broke the
// DEFAULT idle-local path. The seat below reproduces exactly that: empty
// content for a grammar completion with thinking left on, valid JSON when the
// flag is present.
func TestRunAgentTaskThinkingSeatRepackSucceeds(t *testing.T) {
	fake := &agentFake{
		rosterIDs:          []string{agentTestSeat},
		loop:               func(int64) string { return doneChat("The answer is 42.") },
		repack:             func(int64) string { return `{"answer":"42"}` },
		repackThinkingSeat: true,
	}
	srv := fake.server(t)
	defer srv.Close()

	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)

	if wire.Deferred {
		t.Fatalf("a thinking agent seat deferred (%s / %s) — the finished answer %q was thrown away",
			wire.DeferClass, wire.Reason, wire.Output)
	}
	var structured map[string]string
	if err := json.Unmarshal(wire.Structured, &structured); err != nil || structured["answer"] != "42" {
		t.Fatalf("structured = %s (%v)", wire.Structured, err)
	}
	// One completion, not two: the flag has to be on the FIRST attempt. A fix
	// that only reached the shape via the retry would still pass the assertions
	// above while doubling the cost of every re-pack on a thinking seat.
	if got := fake.grammarCNT.Load(); got != 1 {
		t.Fatalf("re-pack completions = %d, want exactly 1 — the non-thinking flag belongs on the first attempt", got)
	}
}

// TestRunAgentTaskRepackFallsBackToChatWhenGrammarRouteMissing is the Lenovo
// FreeToken shape found live 2026-08-27: an OpenAI-only engine serves no
// native completion route, so both grammar re-pack attempts read an HTML 404
// ("invalid character '<'") — and before the fallback, a seat that had just
// produced a CORRECT answer abstained on every schema'd contract. The
// grammar-free chat lane must recover it, fences and all, with the validator
// still enforcing types.
func TestRunAgentTaskRepackFallsBackToChatWhenGrammarRouteMissing(t *testing.T) {
	fake := &agentFake{
		rosterIDs:     []string{agentTestSeat},
		loop:          func(int64) string { return doneChat("Shipment RF-9082 holds 7 pallets.") },
		repackRawBody: func(int64) string { return "<html><body>404 not found</body></html>" },
		chatFallback: func(int64) string {
			// Fenced + prefixed, as a chat-route answer legitimately arrives.
			return doneChat("Here you go:\n```json\n{\"answer\":\"RF-9082: 7 pallets\"}\n```")
		},
	}
	srv := fake.server(t)
	defer srv.Close()

	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)

	if wire.Deferred {
		t.Fatalf("fallback must recover the re-pack, got defer: %s", wire.Reason)
	}
	if got := string(wire.Structured); !strings.Contains(got, "RF-9082") {
		t.Fatalf("structured = %s, want the chat-lane JSON", got)
	}
	if got := fake.grammarCNT.Load(); got != 2 {
		t.Fatalf("grammar attempts = %d, want 2 before the fallback", got)
	}
	if got := fake.chatFallbackCNT.Load(); got != 1 {
		t.Fatalf("chat-fallback attempts = %d, want exactly 1", got)
	}
}

// TestRunAgentTaskRepackCoercesStringTypedScalars is the second live FreeToken
// shape (2026-08-28): the grammar-less seat answers CORRECTLY but quotes every
// scalar — {"pallet_count":"7"} against a number-typed schema — so each typed
// contract abstained after a right answer. Coercion converts only what the
// schema demands and the value parses as, and the result re-validates in full.
func TestRunAgentTaskRepackCoercesStringTypedScalars(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		// The grammar lane itself returns the string-typed scalar (an engine
		// that accepts the request but cannot honor the grammar).
		repack: func(int64) string { return `{"answer_num":"42"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	contract := testContract()
	contract.OutputSchema = json.RawMessage(`{"type":"object","properties":{"answer_num":{"type":"number"}}}`)

	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract))
	wire := decodeWire(t, res)

	if wire.Deferred {
		t.Fatalf("coercion must recover the typed scalar, got defer: %s", wire.Reason)
	}
	var out struct {
		AnswerNum float64 `json:"answer_num"`
	}
	if err := json.Unmarshal(wire.Structured, &out); err != nil || out.AnswerNum != 42 {
		t.Fatalf("structured = %s (err %v), want answer_num 42 as a NUMBER", wire.Structured, err)
	}
	if got := fake.grammarCNT.Load(); got != 1 {
		t.Fatalf("grammar attempts = %d, want 1 (coercion recovers the first)", got)
	}
}

// TestCoerceToSchemaRefusesNonScalarRepairs: coercion never invents structure —
// a missing field or an unparseable string still fails.
func TestCoerceToSchemaRefusesNonScalarRepairs(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"n": map[string]any{"type": "number"},
	}, "required": []any{"n"}}
	if _, ok := coerceToSchema([]byte(`{"n":"seven"}`), schema); ok {
		t.Fatal("an unparseable number string must not coerce")
	}
	if _, ok := coerceToSchema([]byte(`{"other":1}`), schema); ok {
		t.Fatal("a missing required field must not coerce")
	}
	if fixed, ok := coerceToSchema([]byte(`{"n":"3.5"}`), schema); !ok || !strings.Contains(string(fixed), "3.5") {
		t.Fatalf("a parseable number string must coerce, got ok=%v %s", ok, fixed)
	}
}
