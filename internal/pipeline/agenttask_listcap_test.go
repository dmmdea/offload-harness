package pipeline

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// TestListCapInstructionHonoursTheSchemasMaxItems (D-95, half B): the re-issue's
// instruction is DERIVED from the contract's schema, not invented. When the
// author set maxItems the instruction never asks for more than the schema
// admits; with no maxItems it falls back to listCapMax, which is what stopped
// the 2026-09-14 finals from closing.
func TestListCapInstructionHonoursTheSchemasMaxItems(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		want   int
	}{
		{"no maxItems falls back to the cap", `{"type":"object","properties":{"mechanisms":{"type":"array","items":{"type":"string"}}}}`, listCapMax},
		{"a tighter maxItems wins", `{"type":"object","properties":{"mechanisms":{"type":"array","maxItems":3,"items":{"type":"string"}}}}`, 3},
		{"a looser maxItems never raises the cap", `{"type":"object","properties":{"mechanisms":{"type":"array","maxItems":50,"items":{"type":"string"}}}}`, listCapMax},
		{"nested arrays are found", `{"type":"object","properties":{"sections":{"type":"array","items":{"type":"object","properties":{"points":{"type":"array","maxItems":2}}}}}}`, 2},
		{"unparseable schema falls back to the cap", `not json`, listCapMax},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := schemaListCap(json.RawMessage(tc.schema)); got != tc.want {
				t.Fatalf("schemaListCap = %d, want %d", got, tc.want)
			}
			instr := listCapInstruction(json.RawMessage(tc.schema))
			if !strings.Contains(instr, "cap every list at") {
				t.Fatalf("instruction must spell the cap out: %q", instr)
			}
			if !strings.Contains(instr, "200 characters") {
				t.Fatalf("instruction must cap string length too: %q", instr)
			}
			if !strings.Contains(instr, "CUT OFF") {
				t.Fatalf("instruction must say WHY it is being asked again: %q", instr)
			}
		})
	}
}

// TestTruncatedRepackNoteNamesTheListCapAttempt: when the re-issue ran and the
// seat cut the capped answer too, the abstention must say so — otherwise the
// operator reads a D-91 note and cannot tell a first cut from a second.
func TestTruncatedRepackNoteNamesTheListCapAttempt(t *testing.T) {
	calls := []agent.CallRecord{{Step: 1, FinishReason: "length"}, {Step: 1, FinishReason: "length"}}
	plain := truncatedRepackNote("", nil)
	if strings.Contains(plain, "list-cap") {
		t.Fatalf("a first cut with no re-issue keeps the D-91 note: %q", plain)
	}
	if !strings.Contains(plain, "output_truncated") || !strings.Contains(plain, "rides in output") {
		t.Fatalf("the D-91 note must survive verbatim: %q", plain)
	}
	withReissue := truncatedRepackNote(agent.FinalReissueListCap, calls)
	if !strings.Contains(withReissue, "list_cap") {
		t.Fatalf("the note must record the re-issue: %q", withReissue)
	}
	if !strings.Contains(withReissue, "length, length") {
		t.Fatalf("the note must carry BOTH finish reasons: %q", withReissue)
	}
}
