package pipeline

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// The clip an attempt's record keeps of what the seat wrote: both ends, cut on
// character boundaries, and a short answer once rather than twice.
func TestClipEnds(t *testing.T) {
	if h, tl := clipEnds("short"); h != "short" || tl != "" {
		t.Errorf("clipEnds(short) = %q, %q, want the whole text as the head and no tail", h, tl)
	}
	exactly := strings.Repeat("a", 2*repackClipBytes)
	if h, tl := clipEnds(exactly); h != exactly || tl != "" {
		t.Errorf("a text that fits both clips is returned whole, got head %d bytes, tail %d", len(h), len(tl))
	}
	long := strings.Repeat("a", 100) + "MIDDLE" + strings.Repeat("b", 100)
	h, tl := clipEnds(long)
	if len(h) != repackClipBytes || len(tl) != repackClipBytes || !strings.HasPrefix(long, h) || !strings.HasSuffix(long, tl) || strings.Contains(h+tl, "MIDDLE") {
		t.Errorf("clipEnds(long) = %q / %q, want the first and last %d bytes", h, tl, repackClipBytes)
	}
	// A multi-byte character on a cut is dropped, never split.
	wide := strings.Repeat("é", 200) // 2 bytes each
	h, tl = clipEnds(wide)
	if !utf8.ValidString(h) || !utf8.ValidString(tl) || len(h) > repackClipBytes || len(tl) > repackClipBytes {
		t.Errorf("clipEnds(wide) cut a character: head valid=%v (%d bytes), tail valid=%v (%d bytes)", utf8.ValidString(h), len(h), utf8.ValidString(tl), len(tl))
	}
}

func TestRepackWhyIsOneClippedLine(t *testing.T) {
	multi := errors.New("schema validation failed: jsonschema validation failed with 'mem://schema#'\n  - at '': missing properties 'a'\n  - at '/n': got number, want string")
	got := repackWhy(multi)
	if strings.ContainsAny(got, "\n\r\t") || !strings.Contains(got, "got number, want string") {
		t.Errorf("repackWhy = %q, want one line carrying the whole message", got)
	}
	long := errors.New(strings.Repeat("x", 1000))
	if got := repackWhy(long); len(got) > 245 || !strings.HasSuffix(got, "…") {
		t.Errorf("repackWhy of a long message = %d bytes %q, want it clipped", len(got), got[:20])
	}
}

// What the prompt says about each kind of property.
func TestFieldTypeText(t *testing.T) {
	cases := []struct {
		name string
		spec map[string]any
		want string
	}{
		{"string", map[string]any{"type": "string"}, "string"},
		{"no type reads as a string, as the grammar compiles it", map[string]any{}, "string"},
		{"nil spec", nil, "string"},
		{"integer", map[string]any{"type": "integer"}, "integer"},
		{"enum", map[string]any{"type": "string", "enum": []any{"a", "b"}}, `string, one of "a", "b"`},
		{"list of strings", map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "array of strings"},
		{"list of numbers", map[string]any{"type": "array", "items": map[string]any{"type": "number"}}, "array of numbers"},
		{"list of objects", map[string]any{"type": "array", "items": map[string]any{"type": "object"}}, "array of objects"},
		{"list with an untyped item", map[string]any{"type": "array", "items": map[string]any{"description": "x"}}, "array of strings"},
		{"list with no items", map[string]any{"type": "array"}, "array"},
		{"list of enum strings", map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []any{"x", "y"}}}, `array of strings, each one of "x", "y"`},
	}
	for _, tc := range cases {
		if got := fieldTypeText(tc.spec); got != tc.want {
			t.Errorf("%s: fieldTypeText = %q, want %q", tc.name, got, tc.want)
		}
	}
}
