package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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

// decodeToolArgs is how every tool reads its arguments. It decodes strictly
// first. A syntax error leaves the struct zero, but a TYPE error does not:
// encoding/json fills every field it can and reports the first mismatch, so
// `"new_string":123` used to reach edit_file as "" and delete the matched
// snippet, reporting success. So a decode error refuses the call before it
// does anything, with one exception.
//
// On a *json.UnmarshalTypeError only, ONE coercion pass (coerceToolArgs) fixes
// the shapes small models send for NON-string fields — "5" for an int, "true"
// for a bool, "./..." for a []string — and the result is decoded strictly
// again. Those used to work with the field silently dropped; refusing them
// outright would burn the seat's same-tool retries on a harmless shape. A
// string field is never coerced: a number, object, array or bool where a
// string is declared is the content class (edit_file new_string, write_file
// content, a github body, a path), and coercing it could change what is
// written. Unknown fields stay ignored. Empty arguments stay a zero-valued
// call, which is how a no-argument call arrives on some engines.
func decodeToolArgs(tool, args string, v any) error {
	if strings.TrimSpace(args) == "" {
		return nil
	}
	err := json.Unmarshal([]byte(args), v)
	if err == nil {
		return nil
	}
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		if coerced, ok := coerceToolArgs(args, v); ok {
			rv := reflect.ValueOf(v).Elem()
			rv.Set(reflect.Zero(rv.Type())) // drop the first attempt's partial fill
			if err = json.Unmarshal([]byte(coerced), v); err == nil {
				return nil
			}
		}
	}
	return NotPerformed(fmt.Sprintf("NOT performed: %s arguments do not match the tool's schema (%v); nothing was run or changed. Resend the call with every field in its documented type", tool, err))
}

// coerceToolArgs rewrites, in a JSON object, each value that is a JSON STRING
// sent for a non-string field of the target struct v, driven only by the
// field's type (json tags, matched as encoding/json matches them):
//   - an int/uint/float field takes a string that decodes as that number;
//   - a bool field takes "true"/"false", case-insensitively;
//   - a []string field takes a single string as a one-element list.
//
// Everything else is left for the strict re-decode to refuse: a string field
// is never a coercion target, and a value that is not a JSON string is never
// rewritten. ok=false when nothing was rewritten.
func coerceToolArgs(args string, v any) (string, bool) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.Elem().Kind() != reflect.Struct {
		return "", false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &obj) != nil {
		return "", false
	}
	st := rv.Elem().Type()
	changed := false
	for key, raw := range obj {
		ft, ok := argField(st, key)
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			continue // only a JSON string is ever rewritten
		}
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch ft.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64:
			lit := json.RawMessage(strings.TrimSpace(s))
			// "null" decodes into a number as a no-op: that would be the old
			// silent drop, so only a real number literal is taken.
			if string(lit) != "null" && json.Unmarshal(lit, reflect.New(ft).Interface()) == nil {
				obj[key], changed = lit, true
			}
		case reflect.Bool:
			switch {
			case strings.EqualFold(s, "true"):
				obj[key], changed = json.RawMessage("true"), true
			case strings.EqualFold(s, "false"):
				obj[key], changed = json.RawMessage("false"), true
			}
		case reflect.Slice:
			if ft.Elem().Kind() == reflect.String {
				if b, err := json.Marshal([]string{s}); err == nil {
					obj[key], changed = b, true
				}
			}
		}
	}
	if !changed {
		return "", false
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// argField finds the struct field a JSON key decodes into, the way
// encoding/json does: an exact tag (or field) name first, then a
// case-insensitive match. Unexported and `json:"-"` fields never match.
func argField(st reflect.Type, key string) (reflect.Type, bool) {
	var fold reflect.Type
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		if !f.IsExported() {
			continue
		}
		name := f.Name
		if tag, ok := f.Tag.Lookup("json"); ok {
			if tag == "-" {
				continue
			}
			if n, _, _ := strings.Cut(tag, ","); n != "" {
				name = n
			}
		}
		if name == key {
			return f.Type, true
		}
		if fold == nil && strings.EqualFold(name, key) {
			fold = f.Type
		}
	}
	return fold, fold != nil
}
