package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func f64(v float64) *float64 { return &v }
func i32(v int) *int         { return &v }

// captureBody runs one Chat against a fake seat and returns the request body.
func captureBody(t *testing.T, ctx context.Context) map[string]any {
	t.Helper()
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	c := NewLLMClient(srv.URL, "qwen3.5-4b-vllm", "", 5*time.Second)
	if _, err := c.Chat(ctx, []Msg{{Role: "user", Content: "hi"}}, nil, 256); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	return body
}

// TestDefaultSamplingRequestIsUnchanged: with no policy installed the request
// is byte-for-byte what the client has always sent — temperature 0 and NO
// other sampling key. Every existing seat, node and test depends on that.
func TestDefaultSamplingRequestIsUnchanged(t *testing.T) {
	body := captureBody(t, context.Background())
	if v, ok := body["temperature"]; !ok || v != float64(0) {
		t.Fatalf("temperature = %v (present=%v), want 0", v, ok)
	}
	for _, k := range []string{"top_p", "top_k", "presence_penalty", "repetition_penalty"} {
		if _, ok := body[k]; ok {
			t.Errorf("%s must be ABSENT when no sampling is configured, got %v", k, body[k])
		}
	}
}

// TestSamplingRidesTheRequestWhenSet: the operator's measured policy reaches
// the seat under the OpenAI-compatible key names vLLM and llama.cpp both read.
func TestSamplingRidesTheRequestWhenSet(t *testing.T) {
	s := &Sampling{Temperature: f64(0.7), TopP: f64(0.8), TopK: i32(20), PresencePenalty: f64(1.5), RepetitionPenalty: f64(1.05)}
	body := captureBody(t, ContextWithSampling(context.Background(), s))
	want := map[string]float64{"temperature": 0.7, "top_p": 0.8, "top_k": 20, "presence_penalty": 1.5, "repetition_penalty": 1.05}
	for k, v := range want {
		got, ok := body[k]
		if !ok {
			t.Errorf("%s missing from the request body", k)
			continue
		}
		if got != v {
			t.Errorf("%s = %v, want %v", k, got, v)
		}
	}
}

// TestPartialSamplingSendsOnlyWhatIsSet: a policy that names one knob must not
// smuggle defaults for the other four — a seat's own defaults are what an
// unset key means.
func TestPartialSamplingSendsOnlyWhatIsSet(t *testing.T) {
	body := captureBody(t, ContextWithSampling(context.Background(), &Sampling{PresencePenalty: f64(1.5)}))
	if body["presence_penalty"] != float64(1.5) {
		t.Fatalf("presence_penalty = %v", body["presence_penalty"])
	}
	if body["temperature"] != float64(0) {
		t.Fatalf("temperature = %v, want the unchanged 0", body["temperature"])
	}
	for _, k := range []string{"top_p", "top_k", "repetition_penalty"} {
		if _, ok := body[k]; ok {
			t.Errorf("%s must stay absent, got %v", k, body[k])
		}
	}
}

// TestLoopAppliesFinalSamplingToTheAnswerTurnOnly: the planner policy governs
// the tool steps, the final policy the forced final answer — the whole point
// of the split (a thinking-off final is prose generation, a tool step is not).
// Both are published in calls[] so a measurement can prove which ran.
func TestLoopAppliesFinalSamplingToTheAnswerTurnOnly(t *testing.T) {
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "list_dir", `{}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "the answer"}, FinishReason: "stop"},
	}}
	planner := &Sampling{Temperature: f64(0.2)}
	final := &Sampling{Temperature: f64(0.7), TopP: f64(0.8), TopK: i32(20), PresencePenalty: f64(1.5)}
	l := NewLoop(client, mkTools("list_dir"), 2).WithSampling(planner, final)
	res, err := l.Run(context.Background(), "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(client.seenSampling) != 2 {
		t.Fatalf("want 2 completions, got %d", len(client.seenSampling))
	}
	if got := client.seenSampling[0]; got.IsZero() || got.Temperature == nil || *got.Temperature != 0.2 {
		t.Fatalf("tool step must decode under the PLANNER policy: %+v", got)
	}
	if got := client.seenSampling[1]; got.TopK == nil || *got.TopK != 20 || got.PresencePenalty == nil || *got.PresencePenalty != 1.5 {
		t.Fatalf("the final turn must decode under the FINAL policy: %+v", got)
	}
	if len(res.Calls) != 2 {
		t.Fatalf("calls = %d", len(res.Calls))
	}
	if res.Calls[0].Sampling != "temperature=0.2" {
		t.Errorf("calls[0].sampling = %q", res.Calls[0].Sampling)
	}
	if want := "temperature=0.7 top_p=0.8 top_k=20 presence_penalty=1.5"; res.Calls[1].Sampling != want {
		t.Errorf("calls[1].sampling = %q, want %q", res.Calls[1].Sampling, want)
	}
}

// TestNoSamplingConfiguredPublishesTheDefault: a run with no policy still says
// what it decoded under — "not recorded" and "the default" must never read the
// same in a measurement.
func TestNoSamplingConfiguredPublishesTheDefault(t *testing.T) {
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", Content: "the answer"}, FinishReason: "stop"},
	}}
	res, err := NewLoop(client, mkTools("list_dir"), 1).Run(context.Background(), "digest")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Calls) != 1 || res.Calls[0].Sampling != "temperature=0" {
		t.Fatalf("calls[0].sampling = %q, want %q", res.Calls[0].Sampling, "temperature=0")
	}
	if !client.seenSampling[0].IsZero() {
		t.Fatalf("no policy installed must mean no policy on the context: %+v", client.seenSampling[0])
	}
}

// TestPlannerSamplingAloneGovernsTheFinalTurn: agent_sampling_final is
// OPTIONAL — with only a planner policy set, the final turn decodes under it
// rather than falling back to the default.
func TestPlannerSamplingAloneGovernsTheFinalTurn(t *testing.T) {
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "list_dir", `{}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "the answer"}, FinishReason: "stop"},
	}}
	l := NewLoop(client, mkTools("list_dir"), 2).WithSampling(&Sampling{Temperature: f64(0.4)}, nil)
	if _, err := l.Run(context.Background(), "digest"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i, s := range client.seenSampling {
		if s.Temperature == nil || *s.Temperature != 0.4 {
			t.Fatalf("call %d decoded under %+v, want the planner policy", i, s)
		}
	}
}

// TestSamplingSummaryIsTheWireOrder keeps the published string stable — it is
// what a measurement greps.
func TestSamplingSummaryIsTheWireOrder(t *testing.T) {
	s := &Sampling{TopK: i32(20), PresencePenalty: f64(1.5), TopP: f64(0.8), Temperature: f64(0.7)}
	if got, want := s.Summary(), "temperature=0.7 top_p=0.8 top_k=20 presence_penalty=1.5"; got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
	var zero *Sampling
	if got := zero.Summary(); got != "temperature=0" {
		t.Fatalf("zero Summary() = %q", got)
	}
	if !strings.Contains((&Sampling{RepetitionPenalty: f64(1.1)}).Summary(), "repetition_penalty=1.1") {
		t.Fatal("repetition_penalty must render")
	}
}
