package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/seatwait"
)

// LLMClient is the concrete OpenAI-compatible tool-calling Client. It targets
// any /v1/chat/completions endpoint — llama-swap (:11436, keyless) by default,
// or an NIM endpoint (apiKey set) for the hybrid "ask" escalation. One type,
// base_url swap. It implements the agent.Client interface.
type LLMClient struct {
	base   string
	model  string
	apiKey string
	http   *http.Client
}

// StatusError is a non-200 from the seat's chat route, typed so callers key
// on the STATUS (never on body prose — the same rule delegate.replaceableRefusal
// follows for fleet nodes). Its text is unchanged from the old fmt.Errorf so
// every existing log grep and reason string still matches.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("chat %d: %s", e.Code, e.Body) }

// NewLLMClient builds a client. base is the server root (no /v1); apiKey "" =>
// no Authorization header (local).
func NewLLMClient(base, model, apiKey string, timeout time.Duration) *LLMClient {
	return &LLMClient{
		base:   strings.TrimRight(base, "/"),
		model:  model,
		apiKey: apiKey,
		http:   &http.Client{Timeout: timeout},
	}
}

// --- OpenAI wire types ---

type wireFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function wireFn `json:"function"`
}
type wireMsg struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	// ReasoningContent is decode-only: reasoning/harmony models (DeepSeek V4 thinking,
	// gpt-oss) can return message.content EMPTY with the entire answer in
	// reasoning_content. Outgoing messages never set it, so omitempty keeps it off
	// the request wire.
	ReasoningContent string `json:"reasoning_content,omitempty"`
	// Reasoning is the SAME channel under the key vLLM (>= 0.11, every
	// --reasoning-parser) and the OpenAI-style servers emit: `reasoning`, not
	// `reasoning_content`. Decode-only, never sent. Until 0.115.8 the client
	// read only reasoning_content, so a vLLM seat that spent its whole
	// completion budget inside the think block (content: null, reasoning: the
	// unclosed block, finish_reason: length) read as SILENCE — the loop raised
	// the budget, nudged, and published an empty final as "done" (2026-09-10:
	// 20,526 tokens for zero visible characters on the Qube 27B seat).
	Reasoning string `json:"reasoning,omitempty"`
}
type wireToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// wireToolDefs converts the loop's specs to the exact on-wire tool
// definitions. The ONE producer of that shape — Chat ships it and
// wireToolsJSON measures it, so the two can never drift (round-3 review
// finding 2026-08-14: the spec reserve was measuring a marshal of ToolSpec
// itself — different keys, 34 fewer fixed bytes per tool — under-counting on
// exactly the path advertised as exact).
func wireToolDefs(tools []ToolSpec) []wireToolDef {
	if len(tools) == 0 {
		return nil
	}
	defs := make([]wireToolDef, 0, len(tools))
	for _, t := range tools {
		var wd wireToolDef
		wd.Type = "function"
		wd.Function.Name = t.Name
		wd.Function.Description = t.Description
		wd.Function.Parameters = t.Schema
		defs = append(defs, wd)
	}
	return defs
}

// wireToolsJSON renders the tool block EXACTLY as Chat adds it to the request
// — the "tools" array plus the "tool_choice" key — so the spec reserve
// tokenizes the bytes that actually ship. nil for a tool-less loop.
func wireToolsJSON(tools []ToolSpec) ([]byte, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	return json.Marshal(struct {
		Tools      []wireToolDef `json:"tools"`
		ToolChoice string        `json:"tool_choice"`
	}{wireToolDefs(tools), "auto"})
}

type wireReq struct {
	Model       string        `json:"model"`
	Messages    []wireMsg     `json:"messages"`
	Tools       []wireToolDef `json:"tools,omitempty"`
	ToolChoice  string        `json:"tool_choice,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature"`
	Stream      bool          `json:"stream"`
	// ChatTemplateKwargs is set ONLY when the call's context carries
	// ContextWithoutThinking (thinking.go): `{"enable_thinking": false}` is the
	// key Qwen3-class templates (and vLLM's reasoning layer) read to render the
	// turn in non-thinking mode — the same knob the structured re-pack has sent
	// since 0.81.0 (llamaclient.WithoutThinking). nil = key absent = the
	// historical request, byte for byte.
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
}
type wireResp struct {
	Choices []struct {
		Message      wireMsg `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	// Usage / Timings are OPTIONAL server accounting. llama.cpp emits both by
	// default on /v1/chat/completions; other OpenAI-compatible backends emit
	// neither. Absent fields decode to zero and Completion.Serve stays nil, so
	// a caller can always tell "unmeasured" from "measured zero" — never an
	// error, never a behavior change (Phase D, ADR 0017).
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		// CompletionTokensDetails.ReasoningTokens: vLLM's count of the
		// completion tokens spent inside the think block. Absent on llama.cpp
		// (0 = not reported, never "no reasoning").
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Timings struct {
		CacheN      int     `json:"cache_n"`
		PromptN     int     `json:"prompt_n"`
		PromptMS    float64 `json:"prompt_ms"`
		PredictedN  int     `json:"predicted_n"`
		PredictedMS float64 `json:"predicted_ms"`
	} `json:"timings"`
}

// ServeStats is the SERVER's own accounting for one completion — the only
// trustworthy source for KV-prefix reuse and REAL token counts (the ladder's
// own estimator is chars/4 and drifts by content kind).
//
// CacheN is the count of prompt tokens served from the KV cache; PromptN the
// count actually prefilled. Their sum is the real prompt length, which is why
// reuse is CacheN/(CacheN+PromptN) and never CacheN/PromptN.
type ServeStats struct {
	CacheN            int     `json:"cache_n"`
	PromptN           int     `json:"prompt_n"`
	PromptMS          float64 `json:"prompt_ms"`
	PredictedN        int     `json:"predicted_n"`
	PredictedMS       float64 `json:"predicted_ms"`
	UsagePromptTokens int     `json:"usage_prompt_tokens"`
	UsageCachedTokens int     `json:"usage_cached_tokens"`
	// UsageCompletionTokens is the server's own count of tokens it GENERATED for
	// this completion. It is the number the harness ledger needs for "work the seat
	// did" — until 0.115.5 the loop never summed it, so an agent run that generated
	// for minutes was ledgered as 0 (only the structured re-pack's tokens counted).
	UsageCompletionTokens int `json:"usage_completion_tokens"`
	// UsageReasoningTokens is the server's count of completion tokens spent on
	// hidden reasoning (vLLM `usage.completion_tokens_details.reasoning_tokens`).
	// 0 when the backend does not report it — a starvation verdict then rests
	// on the reasoning TEXT and the finish reason instead (Completion.Starvation).
	UsageReasoningTokens int `json:"usage_reasoning_tokens,omitempty"`
}

// Chat sends the running transcript + tool specs and returns the next completion.
func (c *LLMClient) Chat(ctx context.Context, msgs []Msg, tools []ToolSpec, maxTokens int) (Completion, error) {
	req := wireReq{
		Model:     c.model,
		Messages:  make([]wireMsg, 0, len(msgs)),
		MaxTokens: maxTokens,
		Stream:    false,
	}
	for _, m := range msgs {
		wm := wireMsg{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
				ID: tc.ID, Type: "function",
				Function: wireFn{Name: tc.Name, Arguments: tc.Args},
			})
		}
		req.Messages = append(req.Messages, wm)
	}
	req.Tools = wireToolDefs(tools)
	if len(req.Tools) > 0 {
		req.ToolChoice = "auto"
	}
	thinkingOff := IsThinkingOff(ctx)
	if thinkingOff {
		req.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	}

	buf, err := json.Marshal(req)
	if err != nil {
		return Completion{}, err
	}
	// One busy-seat budget per CONTRACT (seatwait): llama-swap's 429 / 503
	// not-ready / 500 src=llama-swap mean peers hold the seat, so the request
	// is re-sent after a counted sleep instead of failing. The affinity ticket
	// is RELEASED before every sleep — the contention is other processes'
	// load, and a parked ticket would only wedge this process's other lanes.
	budget := seatwait.FromContext(ctx)
	var resp *http.Response
	for {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/chat/completions", bytes.NewReader(buf))
		if err != nil {
			return Completion{}, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		// The agent seat shares one llama-swap endpoint with every cascade text seat
		// in the default config, and this path does NOT go through
		// internal/llamaclient — so the gate has to be taken here too, or the
		// cascade side would be the only lane observing it. Keyed on this client's
		// base; a client pointed at a hosted multi-model endpoint keys on ITS base
		// and, since one LLMClient names one model, always matches the resident one.
		tk, err := modelaffinity.Admit(ctx, c.base, c.model, c.http.Timeout)
		if err != nil {
			return Completion{}, err
		}
		r, err := c.http.Do(httpReq)
		if err != nil {
			tk.Release()
			return Completion{}, err
		}
		if r.StatusCode == http.StatusOK {
			// Held until the body below is decoded: llama-swap only stops needing
			// the model resident once the response is fully served.
			defer tk.Release()
			resp = r
			break
		}
		b, _ := io.ReadAll(io.LimitReader(r.Body, 600))
		retryAfter := r.Header.Get("Retry-After")
		r.Body.Close()
		tk.Release()
		if seatwait.Retryable(r.StatusCode, string(b)) {
			if d, ok := budget.NextFor(r.StatusCode, retryAfter); ok {
				if serr := budget.Sleep(ctx, d); serr != nil {
					return Completion{}, serr
				}
				continue
			}
		}
		return Completion{}, &StatusError{Code: r.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	defer resp.Body.Close()
	var wr wireResp
	if err := json.NewDecoder(resp.Body).Decode(&wr); err != nil {
		return Completion{}, err
	}
	if len(wr.Choices) == 0 {
		return Completion{}, fmt.Errorf("no choices in response")
	}
	ch := wr.Choices[0]
	out := Msg{Role: "assistant", Content: ch.Message.Content}
	// The hidden-reasoning channel, under whichever key this seat uses:
	// `reasoning` (vLLM >= 0.11, OpenAI-style) or `reasoning_content`
	// (llama.cpp --reasoning-format, DeepSeek, gpt-oss). Which key a seat
	// answers under is a seat fact worth one log line per seat (D-45's
	// response-shape record), never a silent guess.
	reasoning, reasoningKey := ch.Message.Reasoning, ""
	switch {
	case ch.Message.Reasoning != "":
		reasoningKey = "reasoning"
	case ch.Message.ReasoningContent != "":
		reasoning, reasoningKey = ch.Message.ReasoningContent, "reasoning_content"
	}
	if reasoningKey != "" {
		noteReasoningKey(c.base, c.model, reasoningKey)
	}
	// Reasoning-model fallback (ports nimclient's proven behavior): when content is
	// empty, no tool call was made, the completion FINISHED, and the reasoning
	// channel is populated, the answer is in the reasoning channel — without this
	// the loop sees an empty turn and a perfectly good completion is scored as
	// silence. This exact blind spot is what disqualified gpt-oss-20b's free-text
	// role (2026-08-03 round-2 record) and hid one eval answer on 2026-08-05.
	// Tool-call turns keep empty content: that is the normal shape, not a failure.
	//
	// finish_reason "length" is EXCLUDED on purpose (0.115.8): a think block cut
	// by max_tokens is not an answer, and handing 8,192 tokens of "Thinking
	// Process: 1. Analyze the request…" to the structured re-pack as if it were
	// the answer would be worse than the silence it replaces. The loop reads
	// Completion.Reasoning to classify that turn as reasoning-starved instead.
	if out.Content == "" && len(ch.Message.ToolCalls) == 0 && reasoning != "" && ch.FinishReason != "length" {
		out.Content = reasoning
	}
	for _, tc := range ch.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: tc.Function.Arguments})
	}
	comp := Completion{Msg: out, FinishReason: ch.FinishReason, Reasoning: reasoning, ReasoningKey: reasoningKey, ThinkingOff: thinkingOff}
	// Attach server accounting only when the backend actually reported some —
	// nil means "this backend does not tell us", which a measurement must
	// report as unmeasured rather than as zero reuse.
	if wr.Timings.PromptN > 0 || wr.Timings.CacheN > 0 || wr.Usage.PromptTokens > 0 || wr.Usage.CompletionTokens > 0 {
		comp.Serve = &ServeStats{
			CacheN: wr.Timings.CacheN, PromptN: wr.Timings.PromptN,
			PromptMS:   wr.Timings.PromptMS,
			PredictedN: wr.Timings.PredictedN, PredictedMS: wr.Timings.PredictedMS,
			UsagePromptTokens:     wr.Usage.PromptTokens,
			UsageCachedTokens:     wr.Usage.PromptTokensDetails.CachedTokens,
			UsageCompletionTokens: wr.Usage.CompletionTokens,
			UsageReasoningTokens:  wr.Usage.CompletionTokensDetails.ReasoningTokens,
		}
	}
	return comp, nil
}
