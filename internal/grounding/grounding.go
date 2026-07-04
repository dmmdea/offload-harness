// Package grounding is the harness's free, deterministic quality check: did the
// model invent values not present in the source? No inference, sub-millisecond
// string ops. It is the keystone label generator for the self-learning loop
// (conformal calibration, health monitoring, router training all train on it).
package grounding

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
)

var (
	reNum  = regexp.MustCompile(`\d[\d.,]*`)
	reWord = regexp.MustCompile(`[A-Za-z][A-Za-z0-9'\-]+`)
)

// Check reports whether structured output is grounded in the source input.
// ok=false means grounding does not apply (classify/triage are grammar-pinned to
// a label set, nothing to verify; or the output has no checkable values).
//
//   - extract:   every leaf string/number value must appear in the source
//     (extraction is verbatim — a value not in the source is a hallucination).
//   - summarize: every NUMBER in the summary must appear in the source (the
//     clearest invented-fact signal; entity paraphrasing is intentionally not
//     flagged to avoid false positives).
//
// The pipeline ACTS on a false result only for extract (retry/escalate); for
// summarize it is logged as a quality signal but not actioned.
func Check(task core.TaskType, input string, data []byte) (grounded bool, ok bool) {
	src := normalize(input)
	srcNums := numberSet(input)

	switch task {
	case core.TaskExtract:
		var obj map[string]any
		if json.Unmarshal(data, &obj) != nil {
			return false, false
		}
		vals := leafValues(obj)
		if len(vals) == 0 {
			return false, false
		}
		for _, v := range vals {
			if !valueInSource(v, src, srcNums) {
				return false, true
			}
		}
		return true, true

	case core.TaskSummarize:
		var s struct {
			Summary string   `json:"summary"`
			Bullets []string `json:"bullets"`
		}
		if json.Unmarshal(data, &s) != nil {
			return false, false
		}
		text := s.Summary + " " + strings.Join(s.Bullets, " ")
		nums := reNum.FindAllString(text, -1)
		if strings.TrimSpace(text) == "" {
			return false, false
		}
		if len(nums) == 0 {
			return true, true // nothing falsifiable; treat as grounded
		}
		for _, n := range nums {
			if _, in := srcNums[normNum(n)]; !in {
				return false, true
			}
		}
		return true, true
	}
	return false, false
}

// CheckFields is the per-field variant of Check for extract: it returns the
// list of TOP-LEVEL field names whose value is not grounded in the source.
// ok=false means grounding does not apply (non-extract task). Nested values are
// attributed to their top-level key. Used to name offenders in a targeted
// corrective re-prompt (lightweight atomic-claim verification).
func CheckFields(task core.TaskType, input string, data []byte) (ungrounded []string, ok bool) {
	if task != core.TaskExtract {
		return nil, false
	}
	var obj map[string]any
	if json.Unmarshal(data, &obj) != nil {
		return nil, false
	}
	if len(obj) == 0 {
		return nil, false
	}
	src := normalize(input)
	srcNums := numberSet(input)
	for k, v := range obj {
		for _, leaf := range leafValues(v) {
			if !valueInSource(leaf, src, srcNums) {
				ungrounded = append(ungrounded, k)
				break
			}
		}
	}
	sort.Strings(ungrounded)
	return ungrounded, true
}

func normalize(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }
func normNum(s string) string   { return strings.NewReplacer(",", "", " ", "").Replace(s) }

func numberSet(s string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, n := range reNum.FindAllString(s, -1) {
		m[normNum(n)] = struct{}{}
	}
	return m
}

// leafValues collects every scalar leaf (string/number) in a parsed JSON object,
// as strings, recursing through nested objects/arrays.
func leafValues(v any) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		for _, vv := range t {
			out = append(out, leafValues(vv)...)
		}
	case []any:
		for _, vv := range t {
			out = append(out, leafValues(vv)...)
		}
	case string:
		if strings.TrimSpace(t) != "" {
			out = append(out, t)
		}
	case float64:
		out = append(out, strconv.FormatFloat(t, 'f', -1, 64))
	case bool:
		// booleans are not source-checkable
	}
	return out
}

// valueInSource is true if a value appears in the (normalized) source: as a
// substring, or — for numeric values — all its numbers are present in the source.
func valueInSource(v, src string, srcNums map[string]struct{}) bool {
	nv := normalize(v)
	if nv == "" {
		return true
	}
	if strings.Contains(src, nv) {
		return true
	}
	if reNum.MatchString(v) {
		for _, n := range reNum.FindAllString(v, -1) {
			if _, in := srcNums[normNum(n)]; !in {
				return false
			}
		}
		return true
	}
	return false
}
