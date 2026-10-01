// Package untrusted handles text that came from someone else's page: the web_fetch
// and browse tools hand it to a model inside a fence, and offload_research hands a
// seat's digest of it to the caller (register SF-45, security standard gate G13).
// The same rules apply on both paths, so they live here once.
package untrusted

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Markers are chat-template and instruction-header strings a page can plant to
// impersonate a role or the operator. Neutralizing them is belt-and-suspenders;
// the primary defense on every path is structural (the text travels as a JSON
// string value, so it cannot close a fence or a field).
var Markers = []string{
	"<|im_start|>", "<|im_end|>", "<|system|>", "<|user|>", "<|assistant|>",
	"</system>", "<system>", "[INST]", "[/INST]", "### Instruction", "### System",
	"UNTRUSTED_WEB_CONTENT",
}

// markerRE matches the markers CASE-INSENSITIVELY (a page may use lowercase forms
// like "[inst]" or "### system"), each one QuoteMeta-escaped.
var markerRE = func() *regexp.Regexp {
	parts := make([]string, len(Markers))
	for i, m := range Markers {
		parts[i] = regexp.QuoteMeta(m)
	}
	return regexp.MustCompile("(?i)(" + strings.Join(parts, "|") + ")")
}()

// Sanitize drops zero-width, format and line-separator runes (used to hide a
// payload or break naive concatenation) and replaces every marker with
// "[neutralized]". Code points are tested numerically so the source stays ASCII.
func Sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case 0x200B, // zero-width space
			0x200C, // zero-width non-joiner
			0x200D, // zero-width joiner
			0x2060, // word joiner
			0xFEFF, // zero-width no-break space / BOM
			0x00AD, // soft hyphen
			0x2028, // line separator
			0x2029: // paragraph separator
			continue
		}
		b.WriteRune(r)
	}
	return markerRE.ReplaceAllString(b.String(), "[neutralized]")
}

// Bound caps s at max characters (runes), never splitting one, and says how
// much it cut. max <= 0 leaves s whole.
func Bound(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max]) + fmt.Sprintf(" ...[%d characters cut: untrusted text is capped at %d]", len(runes)-max, max)
}

// Value sanitizes and bounds every string inside a JSON-shaped value (object
// keys included) and returns the result as plain JSON values. It round-trips
// through encoding/json with UseNumber, so numbers keep their exact text.
func Value(v any, max int) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return walk(out, max), nil
}

func walk(v any, max int) any {
	switch t := v.(type) {
	case string:
		return Bound(Sanitize(t), max)
	case []any:
		for i := range t {
			t[i] = walk(t[i], max)
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[Bound(Sanitize(k), max)] = walk(x, max)
		}
		return out
	default:
		return v
	}
}
