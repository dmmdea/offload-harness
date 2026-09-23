package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// sseChunk is one `data:` frame of an OpenAI-style completion stream, in the
// union both engines emit: vLLM's `reasoning`, llama.cpp's
// `reasoning_content`, llama.cpp's `timings` on the usage frame, and an
// `error` object when the engine dies mid-stream.
type sseChunk struct {
	Choices []struct {
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
		// TokenIDs is vLLM's per-step generated ids (`return_token_ids`,
		// LLMClient.WithStreamTokenIDs). A frame can carry ids and NO delta:
		// the tool parser is holding the text of an argument it cannot emit
		// yet. Those ids are the engine's real progress.
		TokenIDs []int `json:"token_ids"`
	} `json:"choices"`
	Usage   *json.RawMessage `json:"usage"`
	Timings *json.RawMessage `json:"timings"`
	Error   *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// sseMaxFrame bounds one `data:` line: a 12 KB answer arriving in a single
// frame is ordinary on a proxy that buffers; 16 MiB is generous, not open.
const sseMaxFrame = 16 << 20

// decodeSSE reads a streamed chat completion and returns the SAME wireResp a
// non-streamed answer decodes to, so everything after the decode is shared
// with the JSON path. onDelta (nil = none) is called with the running token
// count after every frame that carried generated text. The count is DELTAS,
// not characters — one delta is one token on both engines — and the usage
// frame's exact completion_tokens overwrites it at the end. A frame carrying
// generated token ids (vLLM `return_token_ids`) counts by its ids instead,
// whether or not it carries a delta: that is how a tool-call argument the
// parser is still holding stays visible as progress (0.140.1).
func decodeSSE(r io.Reader, onDelta func(tokensSoFar int)) (wireResp, error) {
	var wr wireResp
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), sseMaxFrame)
	tokens, done := 0, false
	var content, reasoning strings.Builder
	var finish string
	usedReasoningContent := false
	type toolAcc struct{ id, typ, name, args string }
	calls := map[int]*toolAcc{}
	var order []int
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue // blank separators, `event:` and `:` comment lines
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			done = true
			break
		}
		var c sseChunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			return wr, fmt.Errorf("stream frame: %w", err)
		}
		if c.Error != nil {
			return wr, fmt.Errorf("engine error in stream: %s", c.Error.Message)
		}
		if c.Usage != nil {
			_ = json.Unmarshal(*c.Usage, &wr.Usage)
		}
		if c.Timings != nil {
			_ = json.Unmarshal(*c.Timings, &wr.Timings)
		}
		for _, ch := range c.Choices {
			d := ch.Delta
			ticked := false
			if d.Content != "" {
				content.WriteString(d.Content)
				ticked = true
			}
			if d.Reasoning != "" {
				reasoning.WriteString(d.Reasoning)
				ticked = true
			}
			if d.ReasoningContent != "" {
				reasoning.WriteString(d.ReasoningContent)
				usedReasoningContent = true
				ticked = true
			}
			for _, t := range d.ToolCalls {
				cur, ok := calls[t.Index]
				if !ok {
					cur = &toolAcc{}
					calls[t.Index] = cur
					order = append(order, t.Index)
				}
				if t.ID != "" {
					cur.id = t.ID
				}
				if t.Type != "" {
					cur.typ = t.Type
				}
				if t.Function.Name != "" {
					cur.name = t.Function.Name
					// the tool name is generated output; vLLM sends it in a
					// frame of its own, before any argument
					ticked = true
				}
				if t.Function.Arguments != "" {
					cur.args += t.Function.Arguments
					ticked = true
				}
			}
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
			if n := len(ch.TokenIDs); n > 0 {
				tokens += n
				ticked = true
			} else if ticked {
				tokens++
			}
			if ticked {
				if onDelta != nil {
					onDelta(tokens)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return wr, fmt.Errorf("stream read: %w", err)
	}
	if !done && finish == "" {
		return wr, errors.New("stream ended without a finish_reason or [DONE]")
	}
	msg := wireMsg{Role: "assistant", Content: content.String()}
	if usedReasoningContent {
		msg.ReasoningContent = reasoning.String()
	} else {
		msg.Reasoning = reasoning.String()
	}
	for _, i := range order {
		c := calls[i]
		typ := c.typ
		if typ == "" {
			typ = "function"
		}
		msg.ToolCalls = append(msg.ToolCalls, wireToolCall{ID: c.id, Type: typ, Function: wireFn{Name: c.name, Arguments: c.args}})
	}
	wr.Choices = append(wr.Choices, struct {
		Message      wireMsg `json:"message"`
		FinishReason string  `json:"finish_reason"`
	}{Message: msg, FinishReason: finish})
	if wr.Usage.CompletionTokens == 0 {
		// the engine sent no usage frame: the delta count is the honest estimate
		wr.Usage.CompletionTokens = tokens
	}
	return wr, nil
}
