// offload_ask's MCP surface. The contract-building rules live in internal/askjob and are
// tested there; what is pinned HERE is the front door's own three jobs — advertise the tool
// unconditionally, publish a verdict on the generated acceptance rather than leaving it
// decorative, and turn every failure into a deferred-shape result instead of an MCP error.

package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// askTestServer builds a Server with the ask lane's local execution replaced by fake, and
// every side effect rooted in a temp dir. Note agent_delegation stays OFF: offload_ask must
// work on a box that has never enabled the delegator role.
func askTestServer(t *testing.T, local func(context.Context, core.AgentContract) (core.AgentWireResult, error)) *Server {
	t.Helper()
	home := t.TempDir()
	cfg := config.Default()
	cfg.Home = home
	cfg.LedgerPath = filepath.Join(home, "ledger.jsonl")
	s := New(pipeline.New(cfg, nil, nil, nil))
	s.localAgent = local
	return s
}

// askFixture writes one file with a distinctive identifier and returns (dir, path).
func askFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.go")
	body := "package x\n\n// FleetMaxQueueDepth caps accepted+running work.\nconst FleetMaxQueueDepth = 32\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, p
}

// anchorsOf recovers the tokens the harness chose out of the generated regex alternation,
// so a fake answer can cite one (or none) on purpose instead of the test hard-coding the
// builder's current choice.
func anchorsOf(t *testing.T, c core.AgentContract) []string {
	t.Helper()
	for _, a := range c.Acceptance {
		if rest, ok := strings.CutPrefix(a, "regex:("); ok {
			return strings.Split(strings.TrimSuffix(rest, ")"), "|")
		}
	}
	t.Fatalf("no regex: grounding check in %v", c.Acceptance)
	return nil
}

// TestAskAdvertisedUnconditionally: the whole point of this lane is that the cheap path is
// cheap, so unlike agent_delegate it must be on tools/list with no config flag set.
func TestAskAdvertisedUnconditionally(t *testing.T) {
	for _, tool := range listTools(t, config.Default()) {
		if tool.Name == "offload_ask" {
			if !strings.Contains(tool.Description, "question") || tool.InputSchema == nil {
				t.Fatalf("offload_ask advertised without a usable description/schema: %+v", tool)
			}
			return
		}
	}
	t.Fatal("offload_ask not advertised on tools/list")
}

func TestAskHandlerPublishesTheAnswerAndAVerdict(t *testing.T) {
	dir, p := askFixture(t)
	var got core.AgentContract
	s := askTestServer(t, func(_ context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		got = c
		anchor := anchorsOf(t, c)[0]
		return core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Seat:          "fake-seat",
			Output:        "the cap is 32, set by " + anchor,
			Structured:    json.RawMessage(`{"answer":"the cap is 32","evidence":"const ` + anchor + ` = 32"}`),
			Steps:         2,
			StopReason:    "done",
		}, nil
	})

	res, err := s.handleAsk(context.Background(), callReq(askArgs("what is the queue cap", p, dir)))
	if err != nil {
		t.Fatalf("handleAsk: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != nil {
		t.Fatalf("a healthy run must not defer: %v", m)
	}
	if m["answer"] != "the cap is 32" {
		t.Fatalf("answer = %v, want the structured answer field", m["answer"])
	}
	if !strings.Contains(m["evidence"].(string), "FleetMaxQueueDepth") {
		t.Fatalf("evidence = %v, want the cited line", m["evidence"])
	}
	if m["verified"] != true {
		t.Fatalf("an answer satisfying every check must read verified: %v", m)
	}
	if m["acceptance_failures"] != nil {
		t.Fatalf("no failures expected: %v", m["acceptance_failures"])
	}
	// The checks themselves ride the response: "verified" is only meaningful beside what
	// was actually checked.
	if acc, _ := m["acceptance"].([]any); len(acc) == 0 {
		t.Fatalf("acceptance must be published with the verdict: %v", m)
	}
	// The caller's context must never pay for the files — the harness inlines them.
	if len(got.Context) != 1 || !strings.Contains(got.Context[0].Text, "FleetMaxQueueDepth") {
		t.Fatalf("the handler must ship the file inline: %+v", got.Context)
	}
}

// TestAskHandlerReportsUnverified is the reason the acceptance is evaluated here at all:
// this lane runs one local seat and never enters delegate.Run, so if the handler did not
// check, an answer that cites nothing from the files would come back looking identical to
// one that does.
func TestAskHandlerReportsUnverified(t *testing.T) {
	dir, p := askFixture(t)
	s := askTestServer(t, func(_ context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		return core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Seat:          "fake-seat",
			Output:        "I think the cap is probably 32.", // cites nothing from the file
			Structured:    json.RawMessage(`{"answer":"probably 32","evidence":""}`),
			Steps:         1,
			StopReason:    "done",
		}, nil
	})

	res, err := s.handleAsk(context.Background(), callReq(askArgs("what is the queue cap", p, dir)))
	if err != nil {
		t.Fatalf("handleAsk: %v", err)
	}
	m := decodeResult(t, res)
	if m["verified"] != false {
		t.Fatalf("an ungrounded answer must NOT read verified: %v", m)
	}
	fails, _ := m["acceptance_failures"].([]any)
	if len(fails) != 2 {
		t.Fatalf("both the anchor and the empty evidence must be named: %v", m["acceptance_failures"])
	}
	// The answer is still published: the caller needs it to decide whether to re-read.
	if m["answer"] != "probably 32" {
		t.Fatalf("an unverified answer is still returned: %v", m)
	}
}

// TestAskHandlerGradesTheAnswerItPublishesNotTheProseItDiscards is the pin for the layer
// this lane added. core.evalText prefers wire.Output whenever it is non-empty, and
// runAgentTask always sets Output before the re-pack — so grading the wire as it arrives
// grades the loop's final PROSE, which this handler never publishes. The caller receives
// only the condensed {answer, evidence} pair.
//
// The divergence runs one way: prose is longer than the pair, so it is likelier to contain
// one of the three frequent tokens. The error mode is therefore verified:true printed
// beside a published answer that cites nothing from the files — "reads as verified while
// nothing verified it", one layer up from where the anchor design closes it.
//
// This fixture is exactly that shape: the prose carries the anchor, the pair does not.
func TestAskHandlerGradesTheAnswerItPublishesNotTheProseItDiscards(t *testing.T) {
	dir, p := askFixture(t)
	s := askTestServer(t, func(_ context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		anchor := anchorsOf(t, c)[0]
		return core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Seat:          "fake-seat",
			// The PROSE cites the file...
			Output: "Reading the attached file, the cap is set by " + anchor + " = 32.",
			// ...but the pair the caller actually receives cites nothing.
			Structured: json.RawMessage(`{"answer":"the cap is 32","evidence":"it is in the config"}`),
			Steps:      2,
			StopReason: "done",
		}, nil
	})

	res, err := s.handleAsk(context.Background(), callReq(askArgs("what is the queue cap", p, dir)))
	if err != nil {
		t.Fatalf("handleAsk: %v", err)
	}
	m := decodeResult(t, res)
	if m["verified"] != false {
		t.Fatalf("the verdict must grade the PUBLISHED pair, not the discarded prose: %v", m)
	}
	fails, _ := m["acceptance_failures"].([]any)
	if len(fails) != 1 {
		t.Fatalf("exactly the grounding check should fail here: %v", m["acceptance_failures"])
	}
	// And the published fields really are the pair, not the prose — otherwise the test
	// would pass for the wrong reason.
	if m["answer"] != "the cap is 32" || m["evidence"] != "it is in the config" {
		t.Fatalf("the handler must publish the structured pair: %v", m)
	}
}

// TestAskHandlerGradesTheProseItFallsBackToPublishing closes the second door on the same
// invariant. When the re-pack emits an empty `answer` — schema-legal, and its own system
// prompt says "Use empty values when a field is absent" — the handler falls back to
// publishing the loop's prose as the answer. Grading only the JSON at that point would
// grade a text the caller never sees, and in THIS direction the error is a false negative:
// verified:false on a properly cited answer. The graded text is built from the decoded
// fields after the fallback has run, so what is graded is what is shown.
func TestAskHandlerGradesTheProseItFallsBackToPublishing(t *testing.T) {
	dir, p := askFixture(t)
	var prose string
	s := askTestServer(t, func(_ context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		anchor := anchorsOf(t, c)[0]
		prose = "The cap is 32."
		return core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Seat:          "fake-seat",
			Output:        prose, // published as the answer, and it does NOT carry the anchor
			// answer empty, evidence carries the citation the caller is shown
			Structured: json.RawMessage(`{"answer":"","evidence":"const ` + anchor + ` = 32"}`),
			Steps:      2,
			StopReason: "done",
		}, nil
	})

	res, err := s.handleAsk(context.Background(), callReq(askArgs("what is the queue cap", p, dir)))
	if err != nil {
		t.Fatalf("handleAsk: %v", err)
	}
	m := decodeResult(t, res)
	if m["answer"] != prose {
		t.Fatalf("an empty structured answer must fall back to the prose: %v", m)
	}
	if m["verified"] != true {
		t.Fatalf("the evidence the caller is shown carries the citation, so this must verify: %v", m)
	}
}

// TestAskHandlerGradesProseWhenThereIsNoStructured: the other half of the rule. With no
// structured pair the handler publishes the prose, so grading the prose is what keeps the
// verdict about the text the caller actually got.
func TestAskHandlerGradesProseWhenThereIsNoStructured(t *testing.T) {
	dir, p := askFixture(t)
	s := askTestServer(t, func(_ context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		return core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Seat:          "fake-seat",
			Output:        "the cap is 32, set by " + anchorsOf(t, c)[0],
			Steps:         1,
			StopReason:    "done",
		}, nil
	})

	res, err := s.handleAsk(context.Background(), callReq(askArgs("what is the queue cap", p, dir)))
	if err != nil {
		t.Fatalf("handleAsk: %v", err)
	}
	m := decodeResult(t, res)
	// The grounding check passes on the prose that IS published; only nonempty:evidence
	// fails, because a re-packless result genuinely has no evidence field.
	fails, _ := m["acceptance_failures"].([]any)
	if len(fails) != 1 || !strings.Contains(fails[0].(string), "nonempty:evidence") {
		t.Fatalf("only the missing evidence should fail here: %v", m["acceptance_failures"])
	}
}

// TestAskHandlerRefusesBeforePlacement: a refusal is a REFUSAL — it must cost no seat time.
func TestAskHandlerRefusesBeforePlacement(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nothing.txt")
	if err := os.WriteFile(p, []byte("the the the\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ran := false
	s := askTestServer(t, func(context.Context, core.AgentContract) (core.AgentWireResult, error) {
		ran = true
		return core.AgentWireResult{}, nil
	})

	res, err := s.handleAsk(context.Background(), callReq(askArgs("what is here", p, dir)))
	if err != nil {
		t.Fatalf("a refusal must be a deferred RESULT, never an MCP error: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true {
		t.Fatalf("ungroundable input must defer: %v", m)
	}
	if !strings.Contains(m["reason"].(string), "anchor") {
		t.Fatalf("the reason must name what is missing: %v", m["reason"])
	}
	if ran {
		t.Fatal("the seat was placed on despite the contract being refused")
	}
}

// TestAskHandlerPassesTheSeatsDeferThrough: a seat that reports it could not do the work is
// a successful RESULT shape, and its defer_class is what tells the caller whether the fix is
// theirs or the operator's — losing it would turn "llama-swap is down" into a quiet defer.
func TestAskHandlerPassesTheSeatsDeferThrough(t *testing.T) {
	dir, p := askFixture(t)
	s := askTestServer(t, func(context.Context, core.AgentContract) (core.AgentWireResult, error) {
		return core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Seat:          "fake-seat",
			Deferred:      true,
			Reason:        "endpoint unreachable",
			DeferClass:    core.DeferClassInfrastructure,
		}, nil
	})

	res, err := s.handleAsk(context.Background(), callReq(askArgs("what is the queue cap", p, dir)))
	if err != nil {
		t.Fatalf("handleAsk: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true || m["reason"] != "endpoint unreachable" {
		t.Fatalf("the seat's defer must reach the caller intact: %v", m)
	}
	if m["defer_class"] != core.DeferClassInfrastructure {
		t.Fatalf("defer_class = %v, want %q", m["defer_class"], core.DeferClassInfrastructure)
	}
}

// TestAskHandlerPublishesProseWhenTheRePackFailed: when the structured re-pack seat could
// not be reached the loop's prose still arrived, and throwing it away would lose finished
// work. It is published as the answer with evidence empty — which nonempty:evidence then
// reports, so the caller is never handed unchecked prose under a green verdict.
func TestAskHandlerPublishesProseWhenTheRePackFailed(t *testing.T) {
	dir, p := askFixture(t)
	s := askTestServer(t, func(_ context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		return core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Seat:          "fake-seat",
			Output:        "the cap is 32 (const " + anchorsOf(t, c)[0] + ")",
			Steps:         1,
			StopReason:    "done",
		}, nil
	})

	res, err := s.handleAsk(context.Background(), callReq(askArgs("what is the queue cap", p, dir)))
	if err != nil {
		t.Fatalf("handleAsk: %v", err)
	}
	m := decodeResult(t, res)
	if !strings.Contains(m["answer"].(string), "the cap is 32") {
		t.Fatalf("prose must be published when structured is absent: %v", m)
	}
	if m["verified"] != false {
		t.Fatal("a result with no structured evidence must not read verified")
	}
}

// TestAskHandlerDefersWhenTheRunnerErrors covers the one failure branch nothing else
// reaches: RunAgentContract returning a real error (contract rejected at Validate, the job
// dir unwritable, the result undecodable). House posture — a deferred RESULT naming the
// cause, never an MCP error, because a caller told "the call failed" discards the work
// instead of reading the files itself.
func TestAskHandlerDefersWhenTheRunnerErrors(t *testing.T) {
	dir, p := askFixture(t)
	s := askTestServer(t, func(context.Context, core.AgentContract) (core.AgentWireResult, error) {
		return core.AgentWireResult{}, errors.New("agent contract: creating job dir: disk full")
	})

	res, err := s.handleAsk(context.Background(), callReq(askArgs("what is the queue cap", p, dir)))
	if err != nil {
		t.Fatalf("a runner error must be a deferred RESULT, not an MCP error: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true {
		t.Fatalf("a runner error must defer: %v", m)
	}
	if !strings.Contains(m["reason"].(string), "disk full") {
		t.Fatalf("the underlying cause must survive to the caller: %v", m["reason"])
	}
}

// TestAskHandlerDefersOnAnEmptyAnswer: a non-deferred result carrying neither prose nor a
// structured answer must not be published as answer:"" — that reads as "the seat answered,
// and the answer is nothing", which is the silent shape this lane exists to avoid.
func TestAskHandlerDefersOnAnEmptyAnswer(t *testing.T) {
	dir, p := askFixture(t)
	s := askTestServer(t, func(context.Context, core.AgentContract) (core.AgentWireResult, error) {
		return core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Seat:          "fake-seat",
			Output:        "   ",
			StopReason:    "budget",
		}, nil
	})

	res, err := s.handleAsk(context.Background(), callReq(askArgs("what is the queue cap", p, dir)))
	if err != nil {
		t.Fatalf("handleAsk: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true {
		t.Fatalf("an empty answer must defer, not publish an empty string: %v", m)
	}
	if m["defer_class"] != core.DeferClassAbstention {
		t.Fatalf("defer_class = %v, want %q", m["defer_class"], core.DeferClassAbstention)
	}
	if _, published := m["answer"]; published {
		t.Fatalf("a defer must not also publish an answer field: %v", m)
	}
}

func askArgs(question, path, readRoot string) string {
	b, err := json.Marshal(map[string]any{"question": question, "paths": []string{path}, "read_root": readRoot})
	if err != nil {
		panic(err)
	}
	return string(b)
}
