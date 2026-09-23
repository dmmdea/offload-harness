package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// cutByBudget reports a completion the engine stopped because it reached the
// call's completion budget. finish_reason alone cannot say so: vLLM rewrites
// it to "tool_calls" whenever a tool call was streamed, cap or no cap
// (measured 2026-09-23), so the server's own completion count is read too.
// maxTokens is the budget THIS call was sent with; 0 means unknown, and then
// only the finish reason counts.
func cutByBudget(c Completion, maxTokens int) bool {
	if c.FinishReason == "length" {
		return true
	}
	return maxTokens > 0 && c.Serve != nil && c.Serve.UsageCompletionTokens >= maxTokens
}

// cutLabel names the evidence cutByBudget acted on, for a stop note.
func cutLabel(c Completion, maxTokens int) string {
	if c.FinishReason == "length" {
		return "finish length"
	}
	return fmt.Sprintf("finish %s at the %d-token cap (%d completion tokens)", c.FinishReason, maxTokens, c.Serve.UsageCompletionTokens)
}

// validToolArgs is the one predicate for "the engine can parse these tool-call
// arguments": empty (how a no-argument call arrives on some engines) or valid
// JSON.
func validToolArgs(s string) bool {
	return strings.TrimSpace(s) == "" || json.Valid([]byte(s))
}

// argsShape is how a tool call's arguments parse.
type argsShape int

const (
	argsValid        argsShape = iota // empty, or valid JSON
	argsUnterminated                  // a JSON prefix cut before its end: only a budget cut leaves that
	argsMalformed                     // invalid somewhere in the middle
)

// classifyToolArgs parses once and classifies the error. encoding/json
// validates the whole input before decoding, and names an input that ends
// mid-value (an open string, object or array) "unexpected end of JSON input".
func classifyToolArgs(s string) argsShape {
	if strings.TrimSpace(s) == "" {
		return argsValid
	}
	var raw json.RawMessage
	err := json.Unmarshal([]byte(s), &raw)
	if err == nil {
		return argsValid
	}
	var se *json.SyntaxError
	if errors.As(err, &se) && strings.Contains(se.Error(), "unexpected end of JSON input") {
		return argsUnterminated
	}
	return argsMalformed
}

// decodeToolArgs is how every tool reads its arguments. A decode error of ANY
// kind refuses the call before it does anything. A syntax error leaves the
// struct zero, but a TYPE error does not: encoding/json fills every field it
// can and reports the first mismatch, so `"new_string":123` used to reach
// edit_file as "" and delete the matched snippet, reporting success. Empty
// arguments stay a zero-valued call, which is how a no-argument call arrives
// on some engines.
func decodeToolArgs(tool, args string, v any) error {
	if strings.TrimSpace(args) == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(args), v); err != nil {
		return NotPerformed(fmt.Sprintf("NOT performed: %s arguments do not match the tool's schema (%v); nothing was run or changed. Resend the call with every field in its documented type", tool, err))
	}
	return nil
}
