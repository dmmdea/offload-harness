package gcf

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// mustCompactArray encodes src (a JSON array literal) or fails the test.
func mustCompactArray(t *testing.T, src string) string {
	t.Helper()
	enc, ok := compactArray(strings.TrimSpace(src))
	if !ok {
		t.Fatalf("compactArray declined eligible input: %s", src[:min(len(src), 120)])
	}
	return enc
}

// roundTrip asserts Decode(compactArray(src)) deep-equals parseEligible(src)
// after canonicalizing per-element key order to column order — the columnar
// format states keys once, so an element's own key ordering is the ONE thing
// deliberately not preserved (same contract as the upstream's tests, which
// treat key order as semantically irrelevant). Values, types, and presence
// are compared exactly.
func roundTrip(t *testing.T, src string) {
	t.Helper()
	want, ok := parseEligible(strings.TrimSpace(src))
	if !ok {
		t.Fatalf("setup: parseEligible rejected the fixture")
	}
	enc := mustCompactArray(t, src)
	got, err := Decode(enc)
	if err != nil {
		t.Fatalf("Decode: %v\nencoded:\n%s", err, enc)
	}
	if !reflect.DeepEqual(canonKeys(got), canonKeys(want)) {
		t.Fatalf("round-trip mismatch\n got: %#v\nwant: %#v\nencoded:\n%s", got, want, enc)
	}
}

// canonKeys sorts each row's Keys so per-element key order (canonicalized by
// the columnar format) does not fail an otherwise exact comparison.
func canonKeys(rs []row) []row {
	out := make([]row, len(rs))
	for i, r := range rs {
		ks := append([]string(nil), r.Keys...)
		sort.Strings(ks)
		out[i] = row{Keys: ks, Vals: r.Vals}
	}
	return out
}

// rows builds a JSON array of n objects via the template function.
func rowsJSON(n int, f func(i int) string) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = f(i)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func TestRoundTripBasicHomogeneous(t *testing.T) {
	roundTrip(t, rowsJSON(20, func(i int) string {
		return fmt.Sprintf(`{"id":%d,"name":"item-%d","value":%d.5,"active":%t}`, i, i, i*10, i%2 == 0)
	}))
}

func TestRoundTripHostileStrings(t *testing.T) {
	// Pipes, quotes, newlines, backticks, sentinel/keyword/number lookalikes,
	// unicode, leading/trailing spaces, empty strings — every needsQuote class.
	hostile := []string{
		`a|b|c`, `say "hi"`, "line1\nline2", "tick`tick", "~", "-", "true", "null",
		"12345", "1e9", " padded ", "", "año útil ñ", "tab\there", "```", "back\\slash",
	}
	roundTrip(t, rowsJSON(len(hostile), func(i int) string {
		v, _ := json.Marshal(hostile[i])
		return fmt.Sprintf(`{"i":%d,"s":%s}`, i, v)
	}))
}

func TestRoundTripHeterogeneousKeysAndTypes(t *testing.T) {
	// Union columns with missing keys, null vs missing distinction, and a
	// column whose type varies per row.
	roundTrip(t, `[
		{"a":1,"b":"x"},
		{"a":2,"c":null},
		{"b":"y","c":"z"},
		{"a":"three","b":true},
		{"a":4},
		{"c":null,"a":5},
		{"a":6,"b":"w","c":"v"},
		{"a":7,"b":null}
	]`)
}

func TestRoundTripNumberLiteralsExact(t *testing.T) {
	// json.Number must preserve the exact literal — 10.50 stays 10.50.
	src := rowsJSON(8, func(i int) string {
		return fmt.Sprintf(`{"p":10.50,"q":1e3,"r":-0.007,"i":%d}`, i)
	})
	enc := mustCompactArray(t, src)
	if !strings.Contains(enc, "10.50|1e3|-0.007") {
		t.Errorf("number literals not preserved verbatim in encoding:\n%s", enc)
	}
	roundTrip(t, src)
}

func TestGateMinRows(t *testing.T) {
	src := rowsJSON(minRows-1, func(i int) string { return fmt.Sprintf(`{"a":%d}`, i) })
	if _, ok := compactArray(src); ok {
		t.Errorf("accepted an array below minRows=%d", minRows)
	}
}

func TestGateRejectsIneligibleShapes(t *testing.T) {
	bad := map[string]string{
		"nested object value": rowsJSON(10, func(i int) string { return fmt.Sprintf(`{"a":%d,"o":{"x":1}}`, i) }),
		"nested array value":  rowsJSON(10, func(i int) string { return fmt.Sprintf(`{"a":%d,"l":[1,2]}`, i) }),
		"bare primitive rows": `[1,2,3,4,5,6,7,8,9,10]`,
		"null element":        `[{"a":1},null,{"a":2},{"a":3},{"a":4},{"a":5},{"a":6},{"a":7}]`,
		"not an array":        `{"a":1}`,
		"all empty objects":   `[{},{},{},{},{},{},{},{},{},{},{},{},{},{},{},{},{},{},{},{},{},{},{},{},{},{},{},{},{},{}]`,
		"malformed":           `[{"a":1},{"a":2}`,
		"trailing content":    `[{"a":1},{"a":2},{"a":3},{"a":4},{"a":5},{"a":6},{"a":7},{"a":8}] extra`,
		"duplicate key":       rowsJSON(10, func(i int) string { return fmt.Sprintf(`{"a":%d,"a":%d}`, i, i+1) }),
		"unsafe field name":   rowsJSON(10, func(i int) string { return fmt.Sprintf(`{"a,b":%d}`, i) }),
	}
	for name, src := range bad {
		if _, ok := compactArray(src); ok {
			t.Errorf("%s: accepted ineligible input", name)
		}
	}
}

func TestGateStrictlySmaller(t *testing.T) {
	// Tiny objects with long hostile values quote-expand; if the encoding
	// cannot beat the source, the original must be left alone.
	src := rowsJSON(8, func(i int) string { return fmt.Sprintf(`{"a":"%d"}`, i) })
	if enc, ok := compactArray(src); ok && len(enc) >= len(src) {
		t.Errorf("returned an encoding not strictly smaller (%d >= %d)", len(enc), len(src))
	}
}

func TestSavingsOnTypicalToolOutput(t *testing.T) {
	// The design's acceptance floor: >=30% on a typical homogeneous array.
	src := rowsJSON(20, func(i int) string {
		return fmt.Sprintf(`{"path":"internal/agent/file_%d.go","line":%d,"match":"func helper%d() error","kind":"function"}`, i, i*17, i)
	})
	enc := mustCompactArray(t, src)
	savings := 1 - float64(len(enc))/float64(len(src))
	if savings < 0.30 {
		t.Errorf("savings %.1f%% below the 30%% acceptance floor\nencoded:\n%s", savings*100, enc)
	}
	roundTrip(t, src)
}

func TestCompactWholeTextAndFencedBlocks(t *testing.T) {
	arr := rowsJSON(10, func(i int) string { return fmt.Sprintf(`{"n":%d,"s":"v%d"}`, i, i) })

	// Whole text IS the array.
	out, ok := Compact("  " + arr + "  ")
	if !ok || !IsCompacted(out) {
		t.Fatalf("whole-text array not compacted")
	}

	// Array embedded in a ```json fence inside prose; prose must survive.
	text := "Results below.\n```json\n" + arr + "\n```\nDone."
	out2, ok2 := Compact(text)
	if !ok2 || !IsCompacted(out2) {
		t.Fatalf("fenced array not compacted")
	}
	if !strings.Contains(out2, "Results below.") || !strings.Contains(out2, "Done.") {
		t.Errorf("surrounding prose lost:\n%s", out2)
	}
	if strings.Contains(out2, "```json") {
		t.Errorf("original fence still present:\n%s", out2)
	}

	// Ineligible content: untouched, ok=false.
	if out3, ok3 := Compact("just prose, no json"); ok3 || out3 != "just prose, no json" {
		t.Errorf("ineligible text was modified")
	}

	// "["-prefixed prose (log framing) containing an eligible fence: the
	// whole-text path declines but the fence inside must still compact.
	logText := "[INFO] scan finished\n```json\n" + arr + "\n```\n[INFO] done"
	out4, ok4 := Compact(logText)
	if !ok4 || !IsCompacted(out4) {
		t.Fatalf("fenced array inside bracket-prefixed prose not compacted")
	}
	if !strings.Contains(out4, "[INFO] scan finished") || !strings.Contains(out4, "[INFO] done") {
		t.Errorf("log framing lost:\n%s", out4)
	}

	// Idempotence: a compacted text is never re-encoded.
	if _, again := Compact(out); again && strings.Count(out, fenceOpen) != 1 {
		t.Errorf("double-encoding occurred")
	}
}

func TestDeterministic(t *testing.T) {
	src := rowsJSON(12, func(i int) string {
		return fmt.Sprintf(`{"a":%d,"b":"x%d","c":%t}`, i, i, i%2 == 0)
	})
	a := mustCompactArray(t, src)
	b := mustCompactArray(t, src)
	if a != b {
		t.Errorf("non-deterministic encoding")
	}
}

// FuzzRoundTrip: for any JSON array the gate ACCEPTS, Decode(encode) must
// deep-equal the parse. Inputs the gate declines are skipped — fail-closed is
// their contract, not losslessness.
func FuzzRoundTrip(f *testing.F) {
	f.Add(`[{"a":1,"b":"x"},{"a":2,"b":"y"},{"a":3},{"b":"z"},{"a":4,"b":"|"},{"a":5,"b":"~"},{"a":6,"b":null},{"a":7,"b":"true"}]`)
	f.Add(`[{"s":"a\nb"},{"s":""},{"s":" x "},{"s":"12"},{"s":"-"},{"s":"ñ"},{"s":"q\"q"},{"s":"p|p"}]`)
	f.Fuzz(func(t *testing.T, src string) {
		want, ok := parseEligible(strings.TrimSpace(src))
		if !ok || len(want) < minRows {
			return
		}
		enc, ok := compactArray(strings.TrimSpace(src))
		if !ok {
			return // strictly-smaller gate declined; fail-closed is fine.
		}
		got, err := Decode(enc)
		if err != nil {
			t.Fatalf("Decode failed on accepted input: %v\nsrc: %s\nenc:\n%s", err, src, enc)
		}
		if !reflect.DeepEqual(canonKeys(got), canonKeys(want)) {
			t.Fatalf("round-trip mismatch\nsrc: %s\nenc:\n%s", src, enc)
		}
	})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
