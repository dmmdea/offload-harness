// thinking.go — hidden reasoning as a first-class fact of a completion.
//
// A THINKING seat (vLLM --reasoning-parser qwen3, llama.cpp --reasoning-format)
// spends its completion budget in two channels the caller cannot see from the
// content alone: the think block and the visible answer. Until 0.115.8 the loop
// could not tell "the model said nothing" from "the model spent every token
// thinking and was cut before it could answer": both arrived as an empty
// content string. On 2026-09-10 that indistinguishability cost 20,526 tokens
// on the Qube 27B seat and 9,628 on the Lenovo 4B for zero visible output,
// then published the empty final as a finished answer (retrospective D-01).
//
// This file gives the loop the three things it needs: a per-call switch that
// renders the seat's template in non-thinking mode, a classifier that names a
// starved completion, and a per-call record the corpus keeps so the rigger can
// see the starvation instead of inferring it from token totals.
package agent

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
)

// Stop reasons a run can END on besides "done" / "budget" / "error"
// (Result.StopReason). Both are terminal and carry an EMPTY Output; the node
// (pipeline/agenttask.go) turns them into a defer instead of re-packing the
// empty string into a schema-valid, all-empty object.
const (
	// StopReasoningStarved: the final completion spent its budget on hidden
	// reasoning and produced no visible content, on the first attempt AND on
	// the one retry with thinking off. Result.StopNote carries the arithmetic.
	StopReasoningStarved = "reasoning_starved"
	// StopEmpty: the model closed with an empty message (finish "stop", no
	// content, no tool calls) twice — once thinking, once not. Not a budget
	// shape: the seat had room and said nothing.
	StopEmpty = "empty"
)

// ThinkingMode is the loop's policy for the seat's think block on planner
// calls (config `agent_thinking`, contract `thinking`, Loop.WithThinking).
//
//	ThinkingAuto (""/"auto")  every step thinks; a step that ends empty is
//	                          re-issued ONCE with thinking off at the final
//	                          budget (finalMaxTokens), then the run stops with
//	                          a named reason. The default.
//	ThinkingOff  ("off")      every planner call renders in non-thinking mode.
//	                          For grounded extraction on a seat whose think
//	                          block was measured to starve the answer.
//	ThinkingOn   ("on")       never send the non-thinking kwarg — for a seat
//	                          whose template rejects it. The empty-final retry
//	                          still runs, at the final budget, thinking on.
type ThinkingMode string

const (
	ThinkingAuto ThinkingMode = "auto"
	ThinkingOff  ThinkingMode = "off"
	ThinkingOn   ThinkingMode = "on"
)

// ParseThinkingMode validates a mode string from config or a contract. The
// empty string is auto. Anything else is a caller error, named. The
// vocabulary is exactly core.ValidateThinking's (auto / on / off,
// case-insensitive) so a value the contract door accepts is one the build
// accepts, and vice versa.
func ParseThinkingMode(s string) (ThinkingMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return ThinkingAuto, nil
	case "off":
		return ThinkingOff, nil
	case "on":
		return ThinkingOn, nil
	}
	return "", fmt.Errorf("thinking mode %q: want auto, on or off", s)
}

// maxReissues bounds the empty-final re-issues per run: each costs one
// full-budget generation, and a seat that starves on every final it reaches
// is not going to answer on the third.
const maxReissues = 2

// finalBudgetCap bounds the completion budget of the thinking-off retry: the
// visible answer to a 40 KB / seven-array extraction is a few thousand tokens,
// and the retry exists to give it that room, but a runaway non-thinking seat
// must still stop.
const finalBudgetCap = 8192

// finalMaxTokens is the completion budget of the empty-final retry: 4x the
// step budget, capped, never below the step budget. A step budget sized for
// tool turns (1,024 on the 4B, 4,096 on the 27B) is not sized for the final
// answer — the 2026-09-10 `ledger-02` row was cut at exactly 1,024 tokens of
// a correct partial answer.
func finalMaxTokens(stepBudget int) int {
	if stepBudget <= 0 {
		stepBudget = 1024
	}
	b := stepBudget * 4
	if b > finalBudgetCap {
		b = finalBudgetCap
	}
	if b < stepBudget {
		b = stepBudget
	}
	return b
}

// thinkingCtxKey marks a context whose next Chat call must render the seat's
// template in non-thinking mode. A context value rather than a Client method
// so every Client implementation (the fleet's LLMClient, tests' fakes, the NIM
// client) keeps its signature; a client that ignores it simply thinks.
type thinkingCtxKey struct{}

// ContextWithoutThinking returns a context whose Chat calls send
// `chat_template_kwargs: {"enable_thinking": false}`.
func ContextWithoutThinking(ctx context.Context) context.Context {
	return context.WithValue(ctx, thinkingCtxKey{}, true)
}

// IsThinkingOff reports whether ctx asks for a non-thinking render.
func IsThinkingOff(ctx context.Context) bool {
	v, _ := ctx.Value(thinkingCtxKey{}).(bool)
	return v
}

// starvedReasoningShare: a completion whose reasoning tokens are at least this
// share of its completion tokens, with nothing visible, spent its budget
// thinking (D-41: ">= 0.9 x completion").
const starvedReasoningShare = 0.9

// Starvation classifies a completion that carries NO tool calls and NO visible
// content. kind is StopReasoningStarved when the budget went to hidden
// reasoning, StopEmpty when the seat simply closed with nothing; basis is the
// one-line evidence. ok=false when the completion is not empty at all.
func (c Completion) Starvation() (kind, basis string, ok bool) {
	if len(c.Msg.ToolCalls) > 0 || strings.TrimSpace(c.Msg.Content) != "" {
		return "", "", false
	}
	rTok, cTok := 0, 0
	if c.Serve != nil {
		rTok, cTok = c.Serve.UsageReasoningTokens, c.Serve.UsageCompletionTokens
	}
	rChars := len(strings.TrimSpace(c.Reasoning))
	switch {
	case cTok > 0 && rTok > 0 && float64(rTok) >= starvedReasoningShare*float64(cTok):
		return StopReasoningStarved, fmt.Sprintf("finish %s, %d of %d completion tokens were reasoning (%q), 0 visible", c.FinishReason, rTok, cTok, c.ReasoningKey), true
	case c.FinishReason == "length" && rChars > 0:
		return StopReasoningStarved, fmt.Sprintf("finish length, %d chars of hidden reasoning (%q), 0 visible", rChars, c.ReasoningKey), true
	case c.FinishReason == "length":
		// Cut with nothing in either channel that this client can see: the
		// budget was spent on something the seat did not return. Starved by
		// shape — the 0.113.5 raise fired on exactly this — and named so.
		return StopReasoningStarved, "finish length, no content and no reasoning channel reported", true
	case rChars > 0:
		return StopEmpty, fmt.Sprintf("finish %s, %d chars of hidden reasoning (%q) and no answer", c.FinishReason, rChars, c.ReasoningKey), true
	}
	return StopEmpty, fmt.Sprintf("finish %s, empty message", c.FinishReason), true
}

// CallRecord is one planner completion as the corpus keeps it (D-47): the
// per-call facts a starvation diagnosis needs, without transcript bytes. Every
// Chat the loop makes — a compaction retry, the thinking-off retry — writes
// one, on every Result return path.
type CallRecord struct {
	Step             int    `json:"step"`
	MaxTokens        int    `json:"max_tokens"`
	FinishReason     string `json:"finish_reason,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
	ReasoningTokens  int    `json:"reasoning_tokens,omitempty"`
	ContentChars     int    `json:"content_chars,omitempty"`
	ReasoningChars   int    `json:"reasoning_chars,omitempty"`
	ToolCalls        int    `json:"tool_calls,omitempty"`
	ThinkingOff      bool   `json:"thinking_off,omitempty"`
	ReasoningKey     string `json:"reasoning_key,omitempty"`
}

func recordOf(step, maxTokens int, c Completion) CallRecord {
	r := CallRecord{
		Step: step, MaxTokens: maxTokens, FinishReason: c.FinishReason,
		ContentChars: len(c.Msg.Content), ReasoningChars: len(c.Reasoning),
		ToolCalls: len(c.Msg.ToolCalls), ThinkingOff: c.ThinkingOff, ReasoningKey: c.ReasoningKey,
	}
	if c.Serve != nil {
		r.CompletionTokens = c.Serve.UsageCompletionTokens
		r.ReasoningTokens = c.Serve.UsageReasoningTokens
	}
	return r
}

// noteReasoningKey logs, once per (endpoint, model, key), which wire key a
// seat returns its hidden reasoning under — the response-shape fact D-45
// wants on record, cheap enough to keep on every run.
var seenReasoningKeys sync.Map

func noteReasoningKey(base, model, key string) {
	k := base + "|" + model + "|" + key
	if _, loaded := seenReasoningKeys.LoadOrStore(k, true); !loaded {
		log.Printf("agent client: seat %s/%s returns hidden reasoning under %q", base, model, key)
	}
}
