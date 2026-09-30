// Package grounding is the harness's free, deterministic quality check: did the
// model invent values not present in the source? No inference, sub-millisecond
// string ops. It is the keystone label generator for the self-learning loop
// (conformal calibration, health monitoring, router training all train on it).
package grounding

import (
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
)

var (
	reNum    = regexp.MustCompile(`\d[\d.,]*`)
	reDigits = regexp.MustCompile(`\d+`)
	reWord   = regexp.MustCompile(`[A-Za-z][A-Za-z0-9'\-]+`)
)

// Check reports whether structured output is grounded in the source input.
// ok=false means grounding does not apply (classify/triage are grammar-pinned to
// a label set, nothing to verify; or the output has no checkable values).
//
//   - extract:   every leaf string/number value must appear in the source
//     (extraction is verbatim — a value not in the source is a hallucination).
//     A string may match as a phrase; a JSON number is compared by VALUE against
//     the numbers the source writes, in either locale (see numberValues).
//   - summarize: every NUMBER in the summary must appear in the source, by value
//     (the clearest invented-fact signal; entity paraphrasing is intentionally
//     not flagged to avoid false positives).
//
// The pipeline ACTS on a false result only for extract (retry/escalate); for
// summarize it is logged as a quality signal but not actioned.
func Check(task core.TaskType, input string, data []byte) (grounded bool, ok bool) {
	src := newSource(input)

	switch task {
	case core.TaskExtract:
		var obj map[string]any
		if json.Unmarshal(data, &obj) != nil {
			return false, false
		}
		vals := leaves(obj)
		if len(vals) == 0 {
			return false, false
		}
		for _, v := range vals {
			if !valueInSource(v, src) {
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
			if !src.hasToken(n) {
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
	src := newSource(input)
	for k, v := range obj {
		for _, l := range leaves(v) {
			if !valueInSource(l, src) {
				ungrounded = append(ungrounded, k)
				break
			}
		}
	}
	sort.Strings(ungrounded)
	return ungrounded, true
}

func normalize(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }

// Numbers are compared EXACTLY, with no tolerance. Both sides are parsed by
// strconv.ParseFloat from decimal text, and equal text parses to bit-identical
// floats, so slack would only make two different numbers of 13+ digits (an epoch
// in ms, a phone number, a card or order id) indistinguishable. A JSON number is a
// float64 and is exact only up to 2^53 (about 9e15, 16 digits): two integers of 17+
// digits that differ in the last place can collapse to one float. A string token of
// idMinDigits or more plain digits therefore skips the float and is compared as a
// canonical digit string (see hasToken).
const idMinDigits = 16

func numEqual(a, b float64) bool { return a == b }

// source is the input prepared for grounding: its normalized text, for the
// verbatim-phrase check, the sorted values of every numeric token in it, and the
// canonical digit strings of its long plain-digit tokens.
type source struct {
	text string
	nums []float64
	ids  map[string]bool
}

func newSource(input string) source {
	s := source{text: normalize(input), ids: map[string]bool{}}
	for _, tok := range reNum.FindAllString(input, -1) {
		s.nums = append(s.nums, numberValues(tok)...)
		if id, ok := longID(tok); ok {
			s.ids[id] = true
		}
	}
	sort.Float64s(s.nums)
	return s
}

// hasValue is true if v equals (numEqual) a value the source writes. The nearest
// candidates in a sorted slice are the two either side of v's insertion point.
func (s source) hasValue(v float64) bool {
	i := sort.SearchFloat64s(s.nums, v)
	return (i < len(s.nums) && numEqual(s.nums[i], v)) || (i > 0 && numEqual(s.nums[i-1], v))
}

// longID reports whether tok, less sentence punctuation, is plain digits of at
// least idMinDigits, and returns them without leading zeros.
func longID(tok string) (string, bool) {
	tok = strings.TrimRight(tok, ".,")
	id := strings.TrimLeft(tok, "0")
	return id, len(id) >= idMinDigits && reDigits.FindString(tok) == tok
}

// hasToken is true if any value the numeric token tok can denote is in the source.
// A long plain-digit token is an identifier, not a quantity: it must match a
// source token digit for digit (leading zeros aside).
func (s source) hasToken(tok string) bool {
	if id, ok := longID(tok); ok {
		return s.ids[id]
	}
	for _, v := range numberValues(tok) {
		if s.hasValue(v) {
			return true
		}
	}
	return false
}

// numberValues returns every value a numeric token (a reNum match) can denote. A
// token's separators mean different things in different locales ("2.354,40" and
// "2,354.40" are the same amount), so a token is read the ways the two locales allow:
//
//   - no separator: the integer.
//   - both kinds ("2.354,40"): the LAST separator is the decimal mark and occurs
//     once; the other kind groups thousands before it. One reading.
//   - one kind, repeated ("1,250,000"): thousands groups, an integer. English
//     commas also read as Indian lakh/crore groups ("1,23,456": 2-digit groups,
//     then a final group of 3).
//   - one separator, once: followed by exactly 3 digits ("1,234") it is ambiguous
//     (thousands: 1234, decimal: 1.234) and both readings count; otherwise
//     ("185,50", "3,1415") it is a decimal mark.
//   - anything else — a date "18.06.2026", a list "3,4,5" — is no number in either
//     locale and reads as its digit groups, as the hyphenated date "2026-06-18"
//     already does. A two-part token is NOT such a case: "3.1", "9.30" and "5,10"
//     read only as decimals, so a model that splits them into 3 and 1 is not
//     grounded ("7 is not in 7.5" is the same rule; precision over recall).
//
// A digits-only reading of a separated token is never invented ("2.354,40" is not
// 235440). A trailing "." or "," is sentence punctuation, not part of the number.
// Signs are not part of a token (reNum starts at a digit), so values are magnitudes.
func numberValues(tok string) []float64 {
	tok = strings.TrimRight(tok, ".,")
	var out []float64
	add := func(whole, frac string) {
		if frac != "" {
			whole += "." + frac
		}
		if f, err := strconv.ParseFloat(whole, 64); err == nil {
			out = append(out, f)
		}
	}
	dots, commas := strings.Count(tok, "."), strings.Count(tok, ",")
	switch {
	case dots+commas == 0:
		add(tok, "")
	case dots > 0 && commas > 0:
		dec, grp := ".", ","
		if strings.LastIndex(tok, ",") > strings.LastIndex(tok, ".") {
			dec, grp = ",", "."
		}
		if i := strings.LastIndex(tok, dec); strings.Count(tok, dec) == 1 && grouped(tok[:i], grp) {
			add(strings.ReplaceAll(tok[:i], grp, ""), tok[i+1:])
		}
	default:
		mark := "."
		if commas > 0 {
			mark = ","
		}
		if dots+commas == 1 {
			i := strings.Index(tok, mark)
			add(tok[:i], tok[i+1:])
			if len(tok[i+1:]) == 3 && thousands(tok, mark) {
				add(tok[:i]+tok[i+1:], "")
			}
		} else if grouped(tok, mark) {
			add(strings.ReplaceAll(tok, mark, ""), "")
		}
	}
	if len(out) == 0 {
		for _, g := range reDigits.FindAllString(tok, -1) {
			add(g, "")
		}
	}
	return out
}

// thousands reports whether s is grouped by sep the way thousands are written: a
// first group of 1-3 digits without a leading zero, then groups of exactly three.
func thousands(s, sep string) bool {
	groups := strings.Split(s, sep)
	if len(groups) < 2 || groups[0] == "" || len(groups[0]) > 3 || groups[0][0] == '0' {
		return false
	}
	for _, g := range groups[1:] {
		if len(g) != 3 {
			return false
		}
	}
	return true
}

// grouped is thousands, or for commas also Indian grouping.
func grouped(s, sep string) bool { return thousands(s, sep) || (sep == "," && lakh(s)) }

// lakh reports whether s is comma-grouped the Indian way: a first group of 1-2
// digits without a leading zero, one or more groups of exactly two, then a final
// group of exactly three ("12,34,567").
func lakh(s string) bool {
	groups := strings.Split(s, ",")
	n := len(groups)
	if n < 3 || groups[0] == "" || len(groups[0]) > 2 || groups[0][0] == '0' || len(groups[n-1]) != 3 {
		return false
	}
	for _, g := range groups[1 : n-1] {
		if len(g) != 2 {
			return false
		}
	}
	return true
}

// leaf is one scalar of an extraction: a string, or a JSON number kept as a number
// rather than as its rendering, so it can be compared by value.
type leaf struct {
	str   string
	num   float64
	isNum bool
}

func (l leaf) String() string {
	if l.isNum {
		return strconv.FormatFloat(l.num, 'f', -1, 64)
	}
	return l.str
}

// leaves collects every scalar leaf (string/number) in a parsed JSON object,
// recursing through nested objects/arrays. Empty strings are skipped (absence
// convention: "" means the field is absent) and booleans are not source-checkable.
func leaves(v any) []leaf {
	var out []leaf
	switch t := v.(type) {
	case map[string]any:
		for _, vv := range t {
			out = append(out, leaves(vv)...)
		}
	case []any:
		for _, vv := range t {
			out = append(out, leaves(vv)...)
		}
	case string:
		if strings.TrimSpace(t) != "" {
			out = append(out, leaf{str: t})
		}
	case float64:
		out = append(out, leaf{num: t, isNum: true})
	case bool:
		// booleans are not source-checkable
	}
	return out
}

// leafValues is leaves as strings (numbers rendered), for the proof validators.
func leafValues(v any) []string {
	var out []string
	for _, l := range leaves(v) {
		out = append(out, l.String())
	}
	return out
}

// valueInSource is true if a leaf appears in the source. A JSON number is a value,
// not a phrase: it is grounded only if the source writes that value (by magnitude —
// see numberValues), never because its digits occur inside another number ("0" is a
// substring of "4200"). A string is grounded as a verbatim phrase, or — when it is
// not one and carries numbers — if every number in it is a value the source writes.
func valueInSource(l leaf, src source) bool {
	if l.isNum {
		return src.hasValue(math.Abs(l.num))
	}
	nv := normalize(l.str)
	if nv == "" {
		return true
	}
	if strings.Contains(src.text, nv) {
		return true
	}
	if reNum.MatchString(l.str) {
		// The numbers alone would accept "ABC-42" for a source that says
		// "ABC-0042", so an identifier-like word (one with a digit or a hyphen)
		// must be the source's too; plain words ("EUR" for "€") stay free. A
		// zero-padded integer ("007") is a code, which a value comparison would
		// fold into 7: it must appear as written.
		for _, w := range reWord.FindAllString(l.str, -1) {
			if strings.ContainsAny(w, "0123456789-") && !strings.Contains(src.text, strings.ToLower(w)) {
				return false
			}
		}
		for _, n := range reNum.FindAllString(l.str, -1) {
			if len(n) > 1 && n[0] == '0' && reDigits.FindString(n) == n && !strings.Contains(src.text, n) {
				return false
			}
			if !src.hasToken(n) {
				return false
			}
		}
		return true
	}
	return false
}
