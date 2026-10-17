package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
)

// scriptedPlanner answers each Chat with the next completion.
type scriptedPlanner struct {
	script []agent.Completion
	n      int
}

func (s *scriptedPlanner) Chat(context.Context, []agent.Msg, []agent.ToolSpec, int) (agent.Completion, error) {
	if s.n >= len(s.script) {
		return agent.Completion{}, fmt.Errorf("script exhausted")
	}
	c := s.script[s.n]
	s.n++
	return c, nil
}

// TestEvidenceReducerRidesTheRecordlessOffload: the reducer's cascade call goes through the
// in-process offload closure the agent doors hand to Build (the recordless offload, on the
// planner seat), and the shape that closure returns for the extract task is the one the
// reducer parses: a build log becomes a receipt whose quote is the log's own line.
func TestEvidenceReducerRidesTheRecordlessOffload(t *testing.T) {
	const failing = "    parse_test.go:42: got 3 items, want 4"
	var body strings.Builder
	body.WriteString("exit=1\n--- stdout ---\n")
	for i := 0; body.Len() < 6000; i++ {
		fmt.Fprintf(&body, "=== RUN   TestCase%03d\n--- PASS: TestCase%03d (0.00s)\n", i, i)
	}
	body.WriteString("--- FAIL: TestParse (0.01s)\n" + failing + "\nFAIL\n\n--- stderr ---\n")

	reqs := make(chan map[string]any, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		reqs <- req
		answer, _ := json.Marshal(map[string]any{"claims": []map[string]string{
			{"claim": "TestParse failed because it found 3 items, not 4", "quote": strings.TrimSpace(failing)},
		}})
		reply, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": string(answer)}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 40, "completion_tokens": 30},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(reply)
	}))
	defer srv.Close()

	cfg := config.Config{Endpoint: srv.URL, Model: "workhorse", Temperature: 0.1}
	offload := NewRecordlessOffloadForPlanner(cfg, "seat-x", 10*time.Second)

	planner := &scriptedPlanner{script: []agent.Completion{
		{Msg: agent.Msg{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "c1", Name: "run_shell", Args: `{"command":"go test ./..."}`}}}, FinishReason: "tool_calls"},
		{Msg: agent.Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	tools := []agent.Tool{{
		ToolSpec: agent.ToolSpec{Name: "run_shell", Description: "run_shell", Schema: json.RawMessage(`{"type":"object"}`)},
		Exec:     func(context.Context, string) (string, error) { return body.String(), nil },
	}}
	loop := agent.NewLoop(planner, tools, 6).WithContextTokens(65536).WithToolResultCap(1 << 20).WithEvidenceReducer(offload)
	res, err := loop.Run(context.Background(), "make the tests pass")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var got string
	for _, m := range res.Transcript {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			got = m.Content
		}
	}
	if !agent.IsEvidenceReceipt(got) {
		t.Fatalf("the build log was not reduced; the transcript carries %q", got[:min(len(got), 120)])
	}
	if !strings.Contains(got, strings.TrimSpace(failing)) || !strings.Contains(got, "exit=1\n") || len(got) >= body.Len() {
		t.Errorf("the receipt lacks the quote or the exit line, or is not smaller (%d vs %d):\n%s", len(got), body.Len(), got)
	}
	select {
	case req := <-reqs:
		if req["model"] != "seat-x" {
			t.Errorf("the reducer's call went to %v, want the planner seat seat-x", req["model"])
		}
		if !strings.Contains(fmt.Sprint(req["messages"]), "BEGIN OUTPUT") {
			t.Errorf("the seat was not shown the output: %v", req["messages"])
		}
	default:
		t.Fatal("the seat was never called")
	}
}
