package main

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The authority qualifier must not become a way around the two things
// the closed acceptance DSL guarantees: the verb set is the reviewed one, and no
// acceptance string carries a speed term. The qualifier sits in the argument
// tail, so neither guarantee has anything new to review; these tests pin that.

// TestAuthorityQualifierAddsNoVerb: every qualified spelling parses to one of the
// reviewed verbs, a qualifier on the verb itself is an unknown verb, and a
// clock-shaped verb stays refused whatever follows it.
func TestAuthorityQualifierAddsNoVerb(t *testing.T) {
	reviewed := map[core.AcceptanceKind]bool{}
	for verb := range reviewedAcceptanceVerbs {
		reviewed[core.AcceptanceKind(verb)] = true
	}
	for _, s := range []string{
		"contains:@answer:alpha", "contains:@field=summary:alpha", "contains:@grounded:alpha",
		"regex:@answer:al[a-z]+", "regex:@field=summary:al[a-z]+", "regex:@grounded:al[a-z]+",
		"not_contains:@answer:alpha", "not_contains:@field=summary:alpha",
	} {
		chk, err := core.ParseAcceptanceCheck(s)
		if err != nil {
			t.Errorf("%q does not parse: %v", s, err)
			continue
		}
		if !reviewed[chk.Kind] {
			t.Errorf("%q parsed to the unreviewed verb %q", s, chk.Kind)
		}
		if chk.Authority == core.AuthNone {
			t.Errorf("%q parsed without an authority", s)
		}
	}
	for _, s := range []string{"contains@answer:alpha", "regex@grounded:alpha", "wall:@answer:1", "latency:@grounded:1", "tok_s:@field=x:1"} {
		_, err := core.ParseAcceptanceCheck(s)
		if err == nil || !strings.Contains(err.Error(), "unknown verb") {
			t.Errorf("%q = %v, want an unknown-verb refusal", s, err)
		}
	}
}

// TestSpeedTermIsStillSeenBehindAQualifier: the scan reads the whole string, so a
// speed term after the qualifier is found exactly as it is after a bare verb, and
// the qualifier spellings themselves are clean.
func TestSpeedTermIsStillSeenBehindAQualifier(t *testing.T) {
	for _, s := range []string{
		"contains:@answer:faster", "contains:@grounded:tok/s >= 20", "regex:@field=summary:within 90 s",
		"regex:@grounded:tok/s >= 20", "not_contains:@answer:slower",
	} {
		if _, hit := speedTermIn(s); !hit {
			t.Errorf("%q carries a speed term and the scan missed it", s)
		}
	}
	for _, s := range []string{
		"contains:@answer:alpha", "contains:@field=summary:alpha", "contains:@grounded:alpha",
		"regex:@answer:al[a-z]+", "regex:@grounded:al[a-z]+", "not_contains:@field=verdict:alpha",
	} {
		if term, hit := speedTermIn(s); hit {
			t.Errorf("%q is clean but the scan flagged %q", s, term)
		}
	}
}
