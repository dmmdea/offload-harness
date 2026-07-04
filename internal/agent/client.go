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
}
type wireToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}
type wireReq struct {
	Model       string        `json:"model"`
	Messages    []wireMsg     `json:"messages"`
	Tools       []wireToolDef `json:"tools,omitempty"`
	ToolChoice  string        `json:"tool_choice,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature"`
	Stream      bool          `json:"stream"`
}
type wireResp struct {
	Choices []struct {
		Message      wireMsg `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
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
	for _, t := range tools {
		var wd wireToolDef
		wd.Type = "function"
		wd.Function.Name = t.Name
		wd.Function.Description = t.Description
		wd.Function.Parameters = t.Schema
		req.Tools = append(req.Tools, wd)
	}
	if len(req.Tools) > 0 {
		req.ToolChoice = "auto"
	}

	buf, err := json.Marshal(req)
	if err != nil {
		return Completion{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return Completion{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return Completion{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 600))
		return Completion{}, fmt.Errorf("chat %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var wr wireResp
	if err := json.NewDecoder(resp.Body).Decode(&wr); err != nil {
		return Completion{}, err
	}
	if len(wr.Choices) == 0 {
		return Completion{}, fmt.Errorf("no choices in response")
	}
	ch := wr.Choices[0]
	out := Msg{Role: "assistant", Content: ch.Message.Content}
	for _, tc := range ch.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: tc.Function.Arguments})
	}
	return Completion{Msg: out, FinishReason: ch.FinishReason}, nil
}
