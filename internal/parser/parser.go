// Package parser forgivingly extracts a JSON object from messy model output.
// Grammar-constrained decoding already yields valid JSON in the common case;
// this is the safety net for fenced blocks, leading/trailing prose, and minor
// defects (trailing commas, smart quotes).
package parser

import (
	"encoding/json"
	"errors"
	"strings"
)

var ErrNoJSON = errors.New("no JSON object found in output")

// StripThink removes a leading <think>…</think> reasoning span (emitted by the reasoning
// tier's think-prefixed grammar, see gbnf.WrapThinking) so the remainder is the bare JSON
// that Extract expects — needed for bare-string outputs (classify/triage enums) where the
// first-object-span fallback can't find the value. The grammar guarantees the output starts
// with "<think>" and that the FIRST "</think>" is the structural separator (the think rule
// forbids "</think>" inside reasoning); a later "</think>" therefore belongs to the JSON
// answer and must be preserved — so split on the FIRST close tag, and only when the content
// actually opens with <think> (a non-reasoning response that merely contains </think> is left
// untouched). Returns s unchanged if it is not a think-wrapped response.
func StripThink(s string) string {
	if !strings.HasPrefix(strings.TrimSpace(s), "<think>") {
		return s
	}
	if i := strings.Index(s, "</think>"); i >= 0 {
		return strings.TrimSpace(s[i+len("</think>"):])
	}
	return s
}

// Extract returns the first valid JSON object found in raw, as compact bytes.
func Extract(raw string) (json.RawMessage, error) {
	s := strings.TrimSpace(raw)
	s = stripFences(s)

	// Fast path: whole string is valid JSON.
	if obj, ok := tryParse(s); ok {
		return obj, nil
	}

	// Find the first balanced {...} span and try it (with repairs).
	span := firstObjectSpan(s)
	if span == "" {
		return nil, ErrNoJSON
	}
	if obj, ok := tryParse(span); ok {
		return obj, nil
	}
	if obj, ok := tryParse(minorRepair(span)); ok {
		return obj, nil
	}
	return nil, ErrNoJSON
}

func tryParse(s string) (json.RawMessage, bool) {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, false
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, false
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return b, true
}

// stripFences removes a single ```lang ... ``` markdown code fence if present.
func stripFences(s string) string {
	i := strings.Index(s, "```")
	if i < 0 {
		return s
	}
	rest := s[i+3:]
	// drop an optional language tag on the same line
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		firstLine := strings.TrimSpace(rest[:nl])
		if firstLine == "" || isWord(firstLine) {
			rest = rest[nl+1:]
		}
	}
	if j := strings.Index(rest, "```"); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}

func isWord(s string) bool {
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return len(s) > 0
}

// firstObjectSpan returns the substring from the first '{' to its matching '}',
// respecting string literals and escapes.
func firstObjectSpan(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// minorRepair fixes the most common small-model JSON defects.
func minorRepair(s string) string {
	// Normalize smart quotes.
	r := strings.NewReplacer("“", "\"", "”", "\"", "‘", "'", "’", "'")
	s = r.Replace(s)
	// Remove trailing commas before } or ].
	var b strings.Builder
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			b.WriteByte(c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\n' || s[j] == '\t' || s[j] == '\r') {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				continue // drop the comma
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}
