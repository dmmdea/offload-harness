package grounding

// CPU Proof Farm (memory-frontier R2-10) — deterministic validators that need no GPU and no
// model, run on cores that are otherwise idle while a seat decodes.
//
// # Exactly two passes, and the first slot was reassigned by its own precondition
//
// R2-10 specifies two hard-coded validators: no registry, no profiles.json surface, no proof
// cache (they run in microseconds — a cache would cost more than it saves).
//
// The item made slot 1 conditional: "check whether offload_extract already uses
// GBNF-constrained decoding — if so, JSON structural validity is guaranteed at decode time and
// that slot goes to citation/path-existence instead."
//
// It does. `tasks.buildExtract` sets `Grammar: gbnf.Object(fields)`, so the decoder cannot
// emit structurally invalid JSON for extract. A JSON-validity validator would therefore be
// pure ceremony: it can only ever pass. So both slots go where the precondition sent them:
//
//	1. PathsExist   — a path-shaped value the model emitted must actually exist on disk.
//	2. CitedSpans   — a quoted span attributed to the source must actually appear in it.
//
// # Relationship to Check()
//
// Check() already answers "is every leaf value present in the source". These answer two things
// it cannot:
//
//   - A path can be present in the source and still be WRONG — copied from a stale doc, or
//     from a different machine's layout. Only the filesystem knows.
//   - A quoted span can be assembled from words that all appear in the source while never
//     appearing as a contiguous quote. Check()'s per-value test passes; the quote is invented.
//
// Both are advisory signals for the existing verify/ground loop, never new gates: nothing here
// fails a request on its own.

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
)

// rePathish matches values that LOOK like filesystem paths — Windows drive letters, UNC, or
// POSIX absolute paths. Deliberately conservative: a false positive here would send a
// perfectly good answer to a filesystem check it was never meant to pass, and this is an
// advisory signal, so a missed path costs far less than a fabricated failure.
var rePathish = regexp.MustCompile(`^(?:[A-Za-z]:[\\/]|\\\\|/)[^\r\n]*$`)

// ProofResult is one validator's finding. Checked is the population it actually examined, so
// "0 failures" over 0 candidates can never be read as a clean bill of health — the silent
// -failure shape this estate keeps hitting.
type ProofResult struct {
	Name     string   `json:"name"`
	Checked  int      `json:"checked"`
	Failures []string `json:"failures,omitempty"`
	// Applicable is false when the validator found nothing of its kind to inspect. A caller
	// must branch on this before treating Failures as meaningful.
	Applicable bool `json:"applicable"`
}

// OK reports a pass. An inapplicable validator is NOT a pass — it is a non-answer, and
// collapsing the two is how "we ran the checks" comes to mean nothing.
func (p ProofResult) OK() bool { return p.Applicable && len(p.Failures) == 0 }

// ProvePathsExist checks every path-shaped leaf value against the filesystem.
//
// statFn is injected so the validator is testable without touching real files; nil uses
// os.Stat. This is the seam, and the real implementation is unit-tested directly rather than
// only through it — a seam that stands in for untested logic is how a gate ends up certifying
// nothing.
func ProvePathsExist(data []byte, statFn func(string) error) ProofResult {
	if statFn == nil {
		statFn = func(p string) error { _, err := os.Stat(p); return err }
	}
	res := ProofResult{Name: "paths_exist"}
	var obj map[string]any
	if json.Unmarshal(data, &obj) != nil {
		return res
	}
	for _, v := range leafValues(obj) {
		s := strings.TrimSpace(v)
		if s == "" || !rePathish.MatchString(s) {
			continue
		}
		res.Checked++
		res.Applicable = true
		if err := statFn(s); err != nil {
			res.Failures = append(res.Failures, s)
		}
	}
	return res
}

// ProveCitedSpans checks that every quoted span in the output appears CONTIGUOUSLY in the
// source.
//
// This is the gap Check() cannot close: it tests values one at a time, so a quote assembled
// from words that each appear somewhere in the source passes it while being invented as a
// quote. Comparison is whitespace-normalised and case-insensitive, because a model reflowing
// or re-casing a genuine quote is a formatting difference, not a fabrication — flagging that
// would bury the real signal in noise.
func ProveCitedSpans(input string, data []byte) ProofResult {
	res := ProofResult{Name: "cited_spans"}
	var obj map[string]any
	if json.Unmarshal(data, &obj) != nil {
		return res
	}
	src := strings.ToLower(collapseWS(input))
	for _, v := range leafValues(obj) {
		for _, q := range quotedSpans(v) {
			// One or two words inside quotes is punctuation or emphasis, not a citation;
			// treating it as one produces constant false alarms.
			if len(strings.Fields(q)) < 3 {
				continue
			}
			res.Checked++
			res.Applicable = true
			if !strings.Contains(src, strings.ToLower(collapseWS(q))) {
				res.Failures = append(res.Failures, q)
			}
		}
	}
	return res
}

// reQuoted deliberately has NO minimum length inside the pattern.
//
// An earlier version used `"([^"]{3,})"`, and it mispaired quotes: given
// `the "ok" flag and the "two words" case`, the 3-char minimum makes the engine reject
// `"ok"`, skip ahead, and then match the CLOSING quote of `ok` against the OPENING quote of
// `two words` — capturing ` flag and the `, text that was never quoted at all. That span is
// then checked for citation fidelity and flagged as an invented quote.
//
// Pairing must therefore be decided by adjacency alone; the length filter belongs to the
// CALLER, after the spans are correctly paired.
var reQuoted = regexp.MustCompile(`"([^"]*)"|“([^”]*)”`)

func quotedSpans(s string) []string {
	var out []string
	for _, m := range reQuoted.FindAllStringSubmatch(s, -1) {
		if m[1] != "" {
			out = append(out, m[1])
		} else if m[2] != "" {
			out = append(out, m[2])
		}
	}
	return out
}

func collapseWS(s string) string { return strings.Join(strings.Fields(s), " ") }
