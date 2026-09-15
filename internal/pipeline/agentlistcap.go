package pipeline

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// listCapMax is the list length the re-issue asks for when the contract's
// schema names none (register D-95). Eight is what the 2026-09-14 A/B says a
// 4B seat closes inside a 900 s wall on a list-heavy grounded extraction; the
// three contracts that died there were asked for "every" item with no bound at
// all. A schema's own maxItems always wins when it is tighter — the author
// asked for a shape, and a re-issue that breaks it is not an answer.
const listCapMax = 8

// listCapStringChars is the per-string bound the re-issue asks for. The cut
// finals of 2026-09-14 were long-prose-per-item, not many-items: capping the
// list alone would have left the same overrun one level down.
const listCapStringChars = 200

// listCapInstruction is the user turn a final answer cut at the completion
// budget is re-issued with on a SCHEMA contract (register D-95). It is DERIVED
// from the schema — the caps it names are ones the schema admits — and it says
// what happened, because a seat that is simply asked again produces the same
// over-long answer again.
func listCapInstruction(schema json.RawMessage) string {
	return fmt.Sprintf(
		"Your previous answer was CUT OFF at the token budget, so it is unusable — it ends mid-value and nothing can read it. "+
			"Answer again now, from what you have already read, and make it FIT: cap every list at %d items (keep the most important ones and drop the rest), "+
			"and keep every string under %d characters. Return the same JSON object that was asked for, complete and closed. "+
			"A short complete answer is the answer; a long cut-off one is not.",
		schemaListCap(schema), listCapStringChars)
}

// schemaListCap is the list cap to ask for: the SMALLEST maxItems anywhere in
// the contract's schema, never above listCapMax. The smallest is the safe
// direction — one sentence caps every list at once, so any larger number could
// break a maxItems the author set. An absent, unparseable or looser maxItems
// falls back to listCapMax.
func schemaListCap(schema json.RawMessage) int {
	n := listCapMax
	var root any
	if err := json.Unmarshal(schema, &root); err != nil {
		return n
	}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if f, ok := t["maxItems"].(float64); ok && int(f) > 0 && int(f) < n {
				n = int(f)
			}
			for _, vv := range t {
				walk(vv)
			}
		case []any:
			for _, vv := range t {
				walk(vv)
			}
		}
	}
	walk(root)
	return n
}

// truncatedRepackNote is the repack_note for a final answer that came back cut
// (D-91's abstention, D-95's attempt). The D-91 sentence is unchanged — it is
// what an operator greps — and the list-cap attempt is named after it, with the
// finish reasons of the completions that produced it, so a first cut and a
// second are never read as the same event.
func truncatedRepackNote(reissue string, calls []agent.CallRecord) string {
	note := "re-pack skipped: the final answer was cut at the completion budget (output_truncated) — a partial cannot be re-packed into the requested object; the partial rides in output"
	if reissue == "" {
		return note
	}
	return note + fmt.Sprintf("; the final turn was already re-issued once with explicit list caps (final_reissue=%s) and was cut again (finish %s)",
		reissue, finalFinishReasons(calls))
}

// finalFinishReasons names the finish reasons of the last two planner
// completions, oldest first — the two the list-cap re-issue produced. "unknown"
// stands in for a backend that reported none; an empty list yields "".
func finalFinishReasons(calls []agent.CallRecord) string {
	if len(calls) == 0 {
		return ""
	}
	if len(calls) > 2 {
		calls = calls[len(calls)-2:]
	}
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		r := strings.TrimSpace(c.FinishReason)
		if r == "" {
			r = "unknown"
		}
		out = append(out, r)
	}
	return strings.Join(out, ", ")
}
