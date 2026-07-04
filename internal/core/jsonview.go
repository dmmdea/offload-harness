package core

import (
	"encoding/json"
	"strings"
)

// SelectKeys parses a comma-separated --select value into a clean key list.
// Whitespace is trimmed and empty entries dropped; "" yields nil (no selection).
func SelectKeys(csv string) []string {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ProjectFields reduces the JSON object `data` to only the given top-level keys
// (the harness's fastcontext citation pattern: let the caller pull only the
// fields it needs and leave the verbose rest on disk / out of context). It is
// best-effort and never errors:
//   - an empty key list returns data unchanged;
//   - if data is not a JSON object (e.g. an array) or is invalid JSON, it is
//     returned unchanged (projection does not apply);
//   - keys absent from data are skipped (no error, no null).
//
// Nested/dotted-path and array-element projection are a documented follow-up;
// top-level selection covers the common case (drop segments[], keep gist, etc.).
func ProjectFields(data json.RawMessage, keys []string) json.RawMessage {
	if len(keys) == 0 || len(data) == 0 {
		return data
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return data // not an object / invalid JSON — selection N/A
	}
	out := make(map[string]json.RawMessage, len(keys))
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return data
	}
	return b
}
