// Package gcf implements a LOSSLESS columnar re-encoding of homogeneous JSON
// arrays — the highest-leverage harvest from the OmniRoute compression study
// (see OMNIROUTE-HARVEST-DESIGN-2026-07-23.md). Tool outputs are dominated by
// arrays of flat objects (file matches, directory listings, test rows); their
// JSON repeats every key on every element. GCF states the keys once:
//
//	[{"id":1,"name":"a","ok":true}, ...x20]
//
// becomes
//
//	```gcf-generic
//	GCF profile=generic
//	## [20]{id,name,ok}
//	1|a|true
//	...
//	```
//
// Losslessness is the contract, proven the way the upstream proves it: the
// decoder below is the ROUND-TRIP ORACLE — production never decodes (the
// model reads the compact form), but every encoder path must deep-equal back
// through Decode in tests and fuzz. Anything the codec cannot round-trip
// bit-honestly it refuses to touch (fail-closed to the original text):
// arrays under minRows, non-object elements, nested values, unsafe field
// names, or an encoding that is not strictly smaller.
//
// Format origin: the GCF generic profile from blackwell-systems/gcf-typescript
// (MIT), vendored into OmniRoute (MIT) and reimplemented here from its
// observed behavior — scalars-only, no v3.2 nested flattening. Attribution
// carried in NOTICE per both licenses.
package gcf

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// minRows is the eligibility floor: below it the header overhead eats the
// savings and the original JSON is left untouched. Matches upstream's default.
const minRows = 8

// fenceOpen / fenceHeader open every compact block; fenceOpen doubles as the
// idempotence marker (IsCompacted) so a re-compaction pass never re-encodes.
const fenceOpen = "```gcf-generic"
const fenceHeader = "GCF profile=generic"

// cellMissing / cellNull are the reserved cell sentinels: a key absent from
// this element vs a key present with JSON null. Strings that LOOK like either
// are quoted by needsQuote, so the sentinels are unambiguous.
const cellMissing = "~"
const cellNull = "-"

// jsonFence finds ```json blocks embedded in larger text, mirroring the
// upstream's second extraction path. (?s) spans newlines; non-greedy body.
var jsonFence = regexp.MustCompile("(?s)```json\n(.*?)\n```")

// Compact rewrites every eligible JSON array in text into its GCF block:
// the whole text when it IS a JSON array, otherwise each ```json fenced block
// whose body qualifies. Returns (text, false) unchanged when nothing was
// eligible or nothing got smaller.
func Compact(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "[") {
		if enc, ok := compactArray(trimmed); ok {
			return enc, true
		}
		// Not an eligible whole-text array — but "["-prefixed text is often
		// just log framing ("[INFO] ...", "[1/10] ..."); fall through so an
		// eligible ```json fence inside it still compacts (review finding).
	}
	changed := false
	out := jsonFence.ReplaceAllStringFunc(text, func(block string) string {
		body := jsonFence.FindStringSubmatch(block)[1]
		enc, ok := compactArray(strings.TrimSpace(body))
		if !ok {
			return block
		}
		changed = true
		return enc
	})
	if !changed {
		return text, false
	}
	return out, true
}

// IsCompacted reports whether text already carries a GCF block, so callers
// (and re-compaction passes) never double-encode.
func IsCompacted(text string) bool {
	return strings.Contains(text, fenceOpen)
}

// row is one decoded element: Keys in first-seen column order restricted to
// the keys this element actually has, Vals typed string/json.Number/bool/nil.
// json.Number preserves the EXACT number literal, so round-trip equality is
// byte-honest on numbers, not float-canonicalized.
type row struct {
	Keys []string
	Vals map[string]any
}

// compactArray encodes one JSON array. (encoded, true) only when the array is
// eligible AND the encoding is strictly smaller than the source text.
func compactArray(src string) (string, bool) {
	rows, ok := parseEligible(src)
	if !ok || len(rows) < minRows {
		return "", false
	}

	// Column order: union of keys, first-seen across elements (upstream rule).
	var fields []string
	seen := map[string]bool{}
	for _, r := range rows {
		for _, k := range r.Keys {
			if !seen[k] {
				seen[k] = true
				fields = append(fields, k)
			}
		}
	}
	// An all-empty-objects array has no columns; its header would be `{}` and
	// the oracle would decode one phantom empty-named field per row (review
	// finding, reproduced). Nothing to tabulate — fail closed.
	if len(fields) == 0 {
		return "", false
	}
	for _, f := range fields {
		if !safeFieldName(f) {
			return "", false
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%s\n## [%d]{%s}\n", fenceOpen, fenceHeader, len(rows), strings.Join(fields, ","))
	for _, r := range rows {
		cells := make([]string, len(fields))
		for i, f := range fields {
			v, present := r.Vals[f]
			cells[i] = encodeCell(v, present)
		}
		b.WriteString(strings.Join(cells, "|"))
		b.WriteByte('\n')
	}
	b.WriteString("```")

	enc := b.String()
	if len(enc) >= len(src) {
		return "", false // no gain — leave the original alone.
	}
	return enc, true
}

// safeFieldName rejects key names the single-line header cannot carry
// unambiguously. Rare in real tool output; fail-closed beats escaping.
func safeFieldName(f string) bool {
	return f != "" && !strings.ContainsAny(f, ",{}|\"\n\r")
}

// parseEligible parses src as an array of flat objects with scalar values,
// preserving each object's key order (encoding/json maps drop order, so this
// walks the token stream). Any shape outside the contract → (nil, false).
func parseEligible(src string) ([]row, bool) {
	dec := json.NewDecoder(strings.NewReader(src))
	dec.UseNumber()

	if !expectDelim(dec, '[') {
		return nil, false
	}
	var rows []row
	for dec.More() {
		if !expectDelim(dec, '{') {
			return nil, false
		}
		r := row{Vals: map[string]any{}}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, false
			}
			key, ok := kt.(string)
			if !ok {
				return nil, false
			}
			vt, err := dec.Token()
			if err != nil {
				return nil, false
			}
			switch v := vt.(type) {
			case string, json.Number, bool, nil:
				if _, dup := r.Vals[key]; dup {
					return nil, false // duplicate key: order/value ambiguity — refuse.
				}
				r.Keys = append(r.Keys, key)
				r.Vals[key] = v
			default:
				return nil, false // '{' or '[' — nested value, outside the contract.
			}
		}
		if !expectDelim(dec, '}') {
			return nil, false
		}
		rows = append(rows, r)
	}
	if !expectDelim(dec, ']') {
		return nil, false
	}
	// ONLY clean EOF may follow: any trailing token or garbage means this text
	// is not purely the array, and encoding would silently eat the trailer.
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return rows, true
}

// expectDelim consumes exactly the given delimiter token.
func expectDelim(dec *json.Decoder, d json.Delim) bool {
	t, err := dec.Token()
	if err != nil {
		return false
	}
	got, ok := t.(json.Delim)
	return ok && got == d
}

// encodeCell renders one value. Bare forms: sentinels, true/false, number
// literals, and strings that cannot be misread. Everything ambiguous is a
// JSON-quoted string (escaping folds real newlines, so rows stay one line).
func encodeCell(v any, present bool) string {
	if !present {
		return cellMissing
	}
	switch x := v.(type) {
	case nil:
		return cellNull
	case bool:
		if x {
			return "true"
		}
		return "false"
	case json.Number:
		return x.String()
	case string:
		if needsQuote(x) {
			q, _ := json.Marshal(x)
			return string(q)
		}
		return x
	}
	return cellMissing // unreachable under parseEligible's contract.
}

// needsQuote lists every string a bare cell could misrepresent: sentinels,
// keyword lookalikes, number lookalikes, structural characters (| splits
// cells, quotes open tokens, backticks could fake a fence at line start),
// whitespace at the edges, and any control character (real newlines would
// break the one-row-per-line invariant).
func needsQuote(s string) bool {
	if s == "" || s == cellMissing || s == cellNull || s == "true" || s == "false" || s == "null" {
		return true
	}
	if strings.ContainsAny(s, "|\"`") {
		return true
	}
	if strings.TrimSpace(s) != s {
		return true
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	if looksNumeric(s) {
		return true
	}
	return false
}

// looksNumeric reports whether a bare cell would decode as a number.
func looksNumeric(s string) bool {
	var n json.Number
	return json.Unmarshal([]byte(s), &n) == nil
}
