package agent

import (
	"strings"
	"testing"
)

const sseFixture = `data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"choices":[{"delta":{"reasoning_content":"think "},"finish_reason":null}]}

: keep-alive comment

data: {"choices":[{"delta":{"content":"Hel"},"finish_reason":null}]}

data: {"choices":[{"delta":{"content":"lo"},"finish_reason":null}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"fetch","arguments":"{\"u"}}]},"finish_reason":null}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"rl\":1}"}}]},"finish_reason":null}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":100},"completion_tokens_details":{"reasoning_tokens":1}},"timings":{"prompt_n":20,"prompt_ms":40.0,"predicted_n":9,"predicted_ms":900.0}}

data: [DONE]

`

func TestDecodeSSEAccumulatesLikeJSON(t *testing.T) {
	var ticks []int
	wr, err := decodeSSE(strings.NewReader(sseFixture), func(n int) { ticks = append(ticks, n) })
	if err != nil {
		t.Fatal(err)
	}
	if len(wr.Choices) != 1 {
		t.Fatalf("choices = %d", len(wr.Choices))
	}
	ch := wr.Choices[0]
	if ch.Message.Content != "Hello" || ch.Message.ReasoningContent != "think " || ch.Message.Reasoning != "" || ch.FinishReason != "tool_calls" {
		t.Fatalf("message = %+v finish=%q", ch.Message, ch.FinishReason)
	}
	if len(ch.Message.ToolCalls) != 1 || ch.Message.ToolCalls[0].ID != "call_1" || ch.Message.ToolCalls[0].Type != "function" ||
		ch.Message.ToolCalls[0].Function.Name != "fetch" || ch.Message.ToolCalls[0].Function.Arguments != `{"url":1}` {
		t.Fatalf("tool calls = %+v", ch.Message.ToolCalls)
	}
	if wr.Usage.CompletionTokens != 9 || wr.Usage.PromptTokens != 120 || wr.Usage.PromptTokensDetails.CachedTokens != 100 ||
		wr.Usage.CompletionTokensDetails.ReasoningTokens != 1 || wr.Timings.PromptN != 20 || wr.Timings.PredictedMS != 900 {
		t.Fatalf("usage/timings not read: %+v %+v", wr.Usage, wr.Timings)
	}
	// one tick per delta that carried tokens (reasoning, content x2, tool-call x2) = 5, monotone
	if len(ticks) != 5 || ticks[len(ticks)-1] != 5 || ticks[0] != 1 {
		t.Fatalf("progress ticks = %v", ticks)
	}
}

func TestDecodeSSEVLLMReasoningKeyAndNoUsage(t *testing.T) {
	stream := `data: {"choices":[{"delta":{"reasoning":"hm"},"finish_reason":null}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	wr, err := decodeSSE(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	ch := wr.Choices[0]
	if ch.Message.Reasoning != "hm" || ch.Message.ReasoningContent != "" || ch.Message.Content != "ok" || ch.FinishReason != "stop" {
		t.Fatalf("message = %+v", ch.Message)
	}
	if wr.Usage.CompletionTokens != 2 {
		t.Fatalf("no usage frame: delta count must stand in, got %d", wr.Usage.CompletionTokens)
	}
}

func TestDecodeSSEErrorsOnTruncatedStream(t *testing.T) {
	_, err := decodeSSE(strings.NewReader(`data: {"choices":[{"delta":{"content":"x"},"finish_reason":null}]}`+"\n\n"), nil)
	if err == nil || !strings.Contains(err.Error(), "stream ended without") {
		t.Fatalf("truncated stream must error, got %v", err)
	}
}

func TestDecodeSSEEngineErrorFrame(t *testing.T) {
	_, err := decodeSSE(strings.NewReader(`data: {"error":{"message":"CUDA error: unhandled","type":"server_error"}}`+"\n\n"), nil)
	if err == nil || !strings.Contains(err.Error(), "CUDA error") {
		t.Fatalf("engine error frame must surface, got %v", err)
	}
}
