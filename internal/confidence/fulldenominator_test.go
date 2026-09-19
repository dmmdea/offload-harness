package confidence

import (
	"math"
	"testing"

	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// lp is the natural log of p — the fixtures below are written in PROBABILITY
// space because the defect (register D-130) is about which probabilities land
// in the denominator, and stating them directly is what makes the 10x visible.
func lp(p float64) float64 { return math.Log(p) }

// tenthMass builds a decision position where the legal classes carry 0.10 of
// the raw mass and 0.90 sits on tokens the grammar never allows — the shape
// research measured on llama.cpp (declared mass ~0.097).
func tenthMass() []llamaclient.TokenLogprob {
	return triage("yes",
		alt("yes", lp(0.08)),
		alt("no", lp(0.02)),
		alt("Sure", lp(0.50)),
		alt("Maybe", lp(0.30)),
		alt("\n", lp(0.10)),
	)
}

func TestMarginDetailFullDenominatorIsTenTimesSmaller(t *testing.T) {
	classes := []string{"yes", "no", "unsure"}
	d, ok := MarginDetail(tenthMass(), "decision", classes)
	if !ok {
		t.Fatal("MarginDetail ok=false, want true")
	}
	// matched: (0.08-0.02)/0.10 = 0.6 ; full: (0.08-0.02)/1.0 = 0.06
	if math.Abs(d.MatchedMargin-0.6) > 1e-9 {
		t.Errorf("MatchedMargin = %.6f, want 0.6", d.MatchedMargin)
	}
	if math.Abs(d.Margin-0.06) > 1e-9 {
		t.Errorf("Margin (full) = %.6f, want 0.06", d.Margin)
	}
	if ratio := d.MatchedMargin / d.Margin; math.Abs(ratio-10) > 1e-6 {
		t.Errorf("matched/full = %.4f, want 10", ratio)
	}
	if math.Abs(d.DeclaredMass-0.10) > 1e-9 {
		t.Errorf("DeclaredMass = %.6f, want 0.10", d.DeclaredMass)
	}
	if d.Ambiguous != 0 {
		t.Errorf("Ambiguous = %d, want 0", d.Ambiguous)
	}
}

func TestMarginFullWrapper(t *testing.T) {
	m, mass, ok := MarginFull(tenthMass(), "decision", []string{"yes", "no", "unsure"})
	if !ok {
		t.Fatal("MarginFull ok=false, want true")
	}
	if math.Abs(m-0.06) > 1e-9 {
		t.Errorf("margin = %.6f, want 0.06", m)
	}
	if math.Abs(mass-0.10) > 1e-9 {
		t.Errorf("declaredMass = %.6f, want 0.10", mass)
	}
}

// Margin must stay byte-identical in behaviour: it is the matched scale, and
// three consumers compare it against stored history.
func TestMarginUnchangedByFullDenominator(t *testing.T) {
	m, ok := Margin(tenthMass(), "decision", []string{"yes", "no", "unsure"})
	if !ok || math.Abs(m-0.6) > 1e-9 {
		t.Fatalf("Margin = %.6f, ok=%v — want 0.6, true (matched scale unchanged)", m, ok)
	}
}

// A token that prefixes MORE THAN ONE class is credited to none (classOf
// returns -1), which silently zeroes mass on taxonomy-shaped label sets like
// {billing, billing_dispute}. The count makes that rate measurable.
func TestMarginDetailCountsAmbiguousPrefixTokens(t *testing.T) {
	classes := []string{"billing", "billing_dispute", "refund"}
	toks := []llamaclient.TokenLogprob{
		tok(`{"label":"`),
		decTok("billing_dispute",
			alt("bill", lp(0.55)), // prefixes BOTH billing and billing_dispute
			alt("refund", lp(0.30)),
			alt("billing", lp(0.15)), // exact match wins outright — not ambiguous
		),
		tok(`"}`),
	}
	d, ok := MarginDetail(toks, "label", classes)
	if !ok {
		t.Fatal("MarginDetail ok=false, want true")
	}
	if d.Ambiguous != 1 {
		t.Errorf("Ambiguous = %d, want 1", d.Ambiguous)
	}
	// matched mass = refund .30 + billing .15 = .45 of a 1.0 total
	if math.Abs(d.DeclaredMass-0.45) > 1e-9 {
		t.Errorf("DeclaredMass = %.6f, want 0.45", d.DeclaredMass)
	}
}

// The -inf sentinel must stay out of BOTH denominators.
func TestMarginDetailExcludesInfSentinel(t *testing.T) {
	toks := triage("yes",
		alt("yes", lp(0.60)),
		alt("no", lp(0.20)),
		alt("junk", lp(0.20)),
		alt("clamped", math.Inf(-1)),
		alt("clamped2", -3.4e38),
	)
	d, ok := MarginDetail(toks, "decision", []string{"yes", "no", "unsure"})
	if !ok {
		t.Fatal("ok=false, want true")
	}
	if math.Abs(d.Margin-0.40) > 1e-9 {
		t.Errorf("full margin = %.6f, want 0.40", d.Margin)
	}
	if math.Abs(d.DeclaredMass-0.80) > 1e-9 {
		t.Errorf("DeclaredMass = %.6f, want 0.80", d.DeclaredMass)
	}
}

func TestMarginDetailNoDecisionPosition(t *testing.T) {
	if _, ok := MarginDetail([]llamaclient.TokenLogprob{tok(`{"x":"y"}`)}, "decision", []string{"yes", "no"}); ok {
		t.Error("ok=true for a missing decision position, want false")
	}
	if _, ok := MarginDetail(triage("yes"), "decision", []string{"yes", "no"}); ok {
		t.Error("ok=true with no alternatives, want false")
	}
}
