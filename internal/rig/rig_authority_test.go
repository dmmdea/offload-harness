package rig

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// A failure of an authority-qualified check opens with the check as written
// (contains:@grounded:..., regex:@field=...:...), so the failure-class register
// still reads it as a content-anchor miss: the prefix is the verb, and the
// qualifier sits in the argument.
//
// That includes the UNKNOWN verdict (empty or absent evidence: an empty answer, an
// absent field). It is classified anchor-miss on purpose and not schema-miss: the
// axis names which kind of check failed (a content anchor), and what tells an empty
// answer from a wrong one is the evidence column, which carries the reason's
// "unknown at <level> authority" text. The failures here are the real ones
// core.AcceptanceCheck.Grade writes, not hand-copied strings, so a change to the
// reason's shape is seen here.
func TestAnAuthorityQualifiedFailureStaysAnAnchorMiss(t *testing.T) {
	docs := []core.ContextDoc{{Name: "spec.md", Text: "The retry limit is 7."}}
	structured := json.RawMessage(`{"verdict":"ok"}`)
	for _, tc := range []struct {
		check string
		res   core.AgentWireResult
		want  core.Verdict
	}{
		{"contains:@grounded:pelican", core.AgentWireResult{Output: "the cache is named pelican"}, core.VerdictFail},
		{"contains:@answer:pelican", core.AgentWireResult{}, core.VerdictUnknown},
		{"regex:@field=summary:pel[a-z]+", core.AgentWireResult{Structured: structured}, core.VerdictUnknown},
		{"not_contains:@answer:PLACEHOLDER", core.AgentWireResult{Output: "PLACEHOLDER later"}, core.VerdictFail},
		{"contains:@grounded:pelican", core.AgentWireResult{}, core.VerdictUnknown},
	} {
		chk, err := core.ParseAcceptanceCheck(tc.check)
		if err != nil {
			t.Fatalf("%s: %v", tc.check, err)
		}
		o := chk.Grade(tc.res, docs)
		if o.Verdict != tc.want {
			t.Fatalf("%s graded %s (%s), want %s", tc.check, o.Verdict, o.Reason, tc.want)
		}
		r := row("qualified", func(r *Row) { r.AcceptanceFailures = []string{o.Reason} })
		got := Classify(r)
		if got.Axis != AxisAnchorMiss {
			t.Errorf("%q classified %s/%s, want %s", o.Reason, got.Axis, got.Sub, AxisAnchorMiss)
		}
		if tc.want == core.VerdictUnknown && !strings.Contains(got.Evidence, "unknown at ") {
			t.Errorf("an unknown must stay visible in the evidence column, got %q", got.Evidence)
		}
	}
}
