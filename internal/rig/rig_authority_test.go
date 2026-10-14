package rig

import "testing"

// A failure of an authority-qualified check opens with the check as written
// (contains:@grounded:..., regex:@field=...:...), so the failure-class register
// still reads it as a content-anchor miss, the unknown verdict included: the
// prefix is the verb, and the qualifier sits in the argument.
func TestAnAuthorityQualifiedFailureStaysAnAnchorMiss(t *testing.T) {
	for _, f := range []string{
		`contains:@grounded:pelican: "pelican" is in the answer but in none of the 1 context documents (not verified)`,
		`contains:@answer:pelican: unknown at answer authority: the answer text is empty, so there is no evidence either way`,
		`regex:@field=summary:pel[a-z]+: unknown at field authority: field "summary" is absent from the structured output`,
		`not_contains:@answer:PLACEHOLDER: forbidden "PLACEHOLDER" found in the answer text`,
	} {
		r := row("qualified", func(r *Row) { r.AcceptanceFailures = []string{f} })
		if got := Classify(r); got.Axis != AxisAnchorMiss {
			t.Errorf("%q classified %s/%s, want %s", f, got.Axis, got.Sub, AxisAnchorMiss)
		}
	}
}
