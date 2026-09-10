package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// The structured re-pack's budget scales with the answer (0.115.10). A fixed
// 1,024 tokens was right for a one-line answer and wrong the moment 0.115.8 let
// a thinking seat finish a long one: the acceptance run's 22,865-char, seven-
// array extraction came back as a JSON prefix and was filed as invalid JSON.

func TestRepackTimeoutScalesWithTheBudget(t *testing.T) {
	cfg := config.Config{RequestTimeoutSec: 120}
	for budget, want := range map[int]time.Duration{1024: 170 * time.Second, 3283: 547 * time.Second, 8192: 1365 * time.Second} {
		if got := repackTimeout(cfg, budget); got != want {
			t.Errorf("repackTimeout(%d) = %v, want %v", budget, got, want)
		}
	}
	if got := repackTimeout(config.Config{RequestTimeoutSec: 600}, 1024); got != 600*time.Second {
		t.Fatalf("a longer request_timeout_sec must win: %v", got)
	}
	if got := repackTimeout(config.Config{}, 100); got != agentRepackChatTimeout {
		t.Fatalf("the floor is the chat-lane timeout: %v", got)
	}
}

// TestRunAgentTaskRepackOutlivesTheSeatClientTimeout: the re-pack runs on its
// own client, so a seat client with a 1 s per-call timeout no longer turns a
// 2 s grammar completion into "structured re-pack unreachable".
func TestRunAgentTaskRepackOutlivesTheSeatClientTimeout(t *testing.T) {
	fake := &agentFake{
		rosterIDs:   []string{agentTestSeat},
		loop:        func(int64) string { return doneChat("The answer is 42.") },
		repack:      func(int64) string { return `{"answer":"42"}` },
		repackDelay: 2 * time.Second,
	}
	srv := fake.server(t)
	defer srv.Close()
	cfg := config.Config{Endpoint: srv.URL, Model: "workhorse", AgentModel: agentTestSeat, FleetNodeID: "node-t", Temperature: 0.1}
	p := New(cfg, llamaclient.New(srv.URL, "", cfg.Model, 1*time.Second), nil, nil)
	res := p.Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("deferred: %s — the re-pack must not ride the seat client's per-call timeout", wire.Reason)
	}
	if len(wire.Structured) == 0 {
		t.Fatal("structured result missing")
	}
}

// TestRunAgentTaskAnswerAlreadyInShapeSkipsTheRepack (D-84): a final answer
// that is already the requested object (fenced, with a prose tail) is
// validated directly — no grammar completion is spent — while a prose answer
// still re-packs.
func TestRunAgentTaskAnswerAlreadyInShapeSkipsTheRepack(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("Here is the result:\n```json\n{\"answer\": \"42\"}\n```\nDone.") },
		repack:    func(int64) string { return `{"answer":"never asked"}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	var st map[string]string
	if err := json.Unmarshal(wire.Structured, &st); err != nil || st["answer"] != "42" {
		t.Fatalf("structured = %s (%v), want the loop's own object", wire.Structured, err)
	}
	if fake.grammarCNT.Load() != 0 {
		t.Fatalf("re-pack completions = %d, want 0 — the answer was already in shape", fake.grammarCNT.Load())
	}
	prose := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42 and {this} is not it.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
	}
	srv2 := prose.server(t)
	defer srv2.Close()
	res2 := agentTestPipeline(t, srv2.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	if wire2 := decodeWire(t, res2); wire2.Deferred || prose.grammarCNT.Load() != 1 {
		t.Fatalf("a prose answer must still re-pack exactly once: deferred=%v re-packs=%d", wire2.Deferred, prose.grammarCNT.Load())
	}
}

func TestRepackBudgetScalesWithTheAnswerBetweenFloorAndCap(t *testing.T) {
	for chars, want := range map[int]int{0: 1024, 900: 1024, 1536: 1024, 3000: 1512, 22865: 8133, 30000: 8192, 100000: 8192} {
		if got := repackBudget(strings.Repeat("x", chars)); got != want {
			t.Errorf("repackBudget(%d chars) = %d, want %d", chars, got, want)
		}
	}
}

// TestRunAgentTaskRepackAsksForABudgetSizedToTheAnswer: the grammar request's
// max_tokens is the scaled budget, not the historical constant.
func TestRunAgentTaskRepackAsksForABudgetSizedToTheAnswer(t *testing.T) {
	long := strings.Repeat("Decision recorded: arm C approved by the operator. ", 450) // ~23 KB
	fake := &agentFake{
		rosterIDs:    []string{agentTestSeat},
		loop:         func(int64) string { return doneChat(long) },
		repack:       func(int64) string { return `{"answer":"42"}` },
		repackBodies: make(chan map[string]any, 4),
	}
	srv := fake.server(t)
	defer srv.Close()

	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	select {
	case body := <-fake.repackBodies:
		if mt, _ := body["max_tokens"].(float64); int(mt) != repackBudget(long) || int(mt) <= agentRepackMaxTokens {
			t.Fatalf("grammar max_tokens = %v, want %d (scaled to a %d-char answer)", body["max_tokens"], repackBudget(long), len(long))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no grammar request body recorded")
	}
}

// TestRunAgentTaskRepackTruncationIsNamedAndRetriedAtTheCap: a grammar
// completion cut at max_tokens is never validated as JSON; the retry runs at
// the cap, and when both are cut the defer names the truncation.
func TestRunAgentTaskRepackTruncationIsNamedAndRetriedAtTheCap(t *testing.T) {
	fake := &agentFake{
		rosterIDs:       []string{agentTestSeat},
		loop:            func(int64) string { return doneChat("The answer is 42, and a great deal more.") },
		repack:          func(int64) string { return `{"answer":"4` }, // a JSON prefix
		repackTruncated: func(int64) bool { return true },
		repackBodies:    make(chan map[string]any, 4),
	}
	srv := fake.server(t)
	defer srv.Close()

	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if !wire.Deferred || wire.DeferClass != core.DeferClassAbstention {
		t.Fatalf("deferred/class = %v/%q, want an abstention", wire.Deferred, wire.DeferClass)
	}
	if !strings.Contains(wire.Reason, "re-pack truncated at") || strings.Contains(wire.Reason, "unexpected end of JSON") {
		t.Fatalf("reason = %q, want the truncation named rather than an invalid-json verdict", wire.Reason)
	}
	if fake.grammarCNT.Load() != 2 {
		t.Fatalf("grammar attempts = %d, want 2", fake.grammarCNT.Load())
	}
	first, second := <-fake.repackBodies, <-fake.repackBodies
	if mt1, _ := first["max_tokens"].(float64); int(mt1) != agentRepackMaxTokens {
		t.Fatalf("first attempt max_tokens = %v, want the floor %d for a short answer", first["max_tokens"], agentRepackMaxTokens)
	}
	if mt2, _ := second["max_tokens"].(float64); int(mt2) != agentRepackMaxTokensCap {
		t.Fatalf("retry max_tokens = %v, want the cap %d after a truncation", second["max_tokens"], agentRepackMaxTokensCap)
	}
}
