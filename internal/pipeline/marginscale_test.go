package pipeline

import (
	"math"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// The margin gate's denominator (register D-130). The matched scale normalises
// over matched class tokens only; the full scale over every alternative at the
// decision position. On the fixture below the legal classes hold 0.10 of the
// raw mass, so matched = 0.6 and full = 0.06 — the ~10x the research measured.
//
// The flag defaults OFF, and with it off NOTHING about the gate may change
// except the new row fields. That is what most of these tests pin.

func lpOf(p float64) float64 { return math.Log(p) }

func tok(s string) llamaclient.TokenLogprob { return llamaclient.TokenLogprob{Token: s} }

func alt(t string, p float64) llamaclient.AltToken {
	return llamaclient.AltToken{Token: t, Logprob: lpOf(p)}
}

// tenthMassTriage: yes .08 / no .02 legal, .90 on tokens no class matches.
func tenthMassTriage() []llamaclient.TokenLogprob {
	return []llamaclient.TokenLogprob{
		tok(`{"decision":"`),
		llamaclient.TokenLogprob{Token: "yes", Top: []llamaclient.AltToken{
			alt("yes", 0.08), alt("no", 0.02), alt("Sure", 0.50), alt("Maybe", 0.30), alt("\n", 0.10),
		}},
		tok(`","reason":"x"}`),
	}
}

func marginGatePipeline(t *testing.T, tweak func(*config.Config)) *Pipeline {
	t.Helper()
	cfg := config.Default()
	cfg.ThresholdsPath = ""
	if tweak != nil {
		tweak(&cfg)
	}
	return New(cfg, nil, nil, nil)
}

func marginTriageReq() core.Request { return core.Request{Task: core.TaskTriage} }

// Flag OFF: the matched margin 0.6 is below the 0.65 constant, so the call
// escalates exactly as it does today — and the row now says which scale that
// number is on, plus the declared mass that makes the re-derivation possible.
func TestMatchedScaleStillEscalatesAndStampsTheScale(t *testing.T) {
	p := marginGatePipeline(t, nil)
	reason, mi, src, low := p.confidenceGate(marginTriageReq(), nil, tenthMassTriage())
	if !low || src != core.EscMargin {
		t.Fatalf("low=%v src=%q reason=%q — want the margin gate to fire as it does today", low, src, reason)
	}
	if math.Abs(mi.Margin-0.6) > 1e-9 {
		t.Errorf("margin = %.6f, want the unchanged matched 0.6", mi.Margin)
	}
	if mi.Scale != core.MarginScaleMatched {
		t.Errorf("scale = %q, want %q", mi.Scale, core.MarginScaleMatched)
	}
	if math.Abs(mi.DeclaredMass-0.10) > 1e-9 {
		t.Errorf("declared mass = %.6f, want 0.10 — the re-derivation data must accrue with the flag OFF", mi.DeclaredMass)
	}
}

// Flag ON with no threshold: the gate is disabled on the full scale. Nothing
// escalates, and the row says "full" so its margin is never pooled with history.
func TestFullScaleWithZeroThresholdNeverFires(t *testing.T) {
	p := marginGatePipeline(t, func(c *config.Config) { c.ConfidenceMarginFullDenominator = true })
	reason, mi, src, low := p.confidenceGate(marginTriageReq(), nil, tenthMassTriage())
	if low {
		t.Fatalf("gate fired with a 0 full-scale threshold (reason=%q)", reason)
	}
	if src != core.EscNone {
		t.Errorf("src = %q, want none", src)
	}
	if math.Abs(mi.Margin-0.06) > 1e-9 {
		t.Errorf("margin = %.6f, want the full-scale 0.06", mi.Margin)
	}
	if mi.Scale != core.MarginScaleFull {
		t.Errorf("scale = %q, want %q", mi.Scale, core.MarginScaleFull)
	}
}

// Flag ON with a re-derived threshold: the FULL margin gates.
func TestFullScaleThresholdGatesTheFullMargin(t *testing.T) {
	p := marginGatePipeline(t, func(c *config.Config) {
		c.ConfidenceMarginFullDenominator = true
		c.ConfidenceMarginThresholdFull = 0.10 // 0.06 < 0.10 -> escalate
	})
	_, mi, src, low := p.confidenceGate(marginTriageReq(), nil, tenthMassTriage())
	if !low || src != core.EscMargin {
		t.Fatalf("low=%v src=%q — a full margin below the full threshold must escalate", low, src)
	}
	if mi.Scale != core.MarginScaleFull {
		t.Errorf("scale = %q, want full", mi.Scale)
	}

	p2 := marginGatePipeline(t, func(c *config.Config) {
		c.ConfidenceMarginFullDenominator = true
		c.ConfidenceMarginThresholdFull = 0.05 // 0.06 >= 0.05 -> accept
	})
	if _, _, _, low := p2.confidenceGate(marginTriageReq(), nil, tenthMassTriage()); low {
		t.Error("a full margin above the full threshold must be accepted")
	}
}

// The matched-scale per-task conformal thresholds must NOT be consulted on the
// full scale: they were derived from ~10x-larger numbers.
func TestFullScaleIgnoresTheMatchedConformalThreshold(t *testing.T) {
	p := marginGatePipeline(t, func(c *config.Config) { c.ConfidenceMarginFullDenominator = true })
	p.thresholds = map[string]float64{string(core.TaskTriage): 0.5}
	if _, _, _, low := p.confidenceGate(marginTriageReq(), nil, tenthMassTriage()); low {
		t.Fatal("the matched-scale conformal threshold was applied to a full-scale margin")
	}
}

func TestAmbiguousPrefixCountReachesTheRow(t *testing.T) {
	p := marginGatePipeline(t, nil)
	req := core.Request{Task: core.TaskClassify, Params: map[string]any{
		"labels": []string{"billing", "billing_dispute", "refund"},
	}}
	lps := []llamaclient.TokenLogprob{
		tok(`{"label":"`),
		llamaclient.TokenLogprob{Token: "billing", Top: []llamaclient.AltToken{
			alt("bill", 0.55), alt("refund", 0.30), alt("billing", 0.15),
		}},
		tok(`","confidence":0.99}`),
	}
	_, mi, _, _ := p.confidenceGate(req, []byte(`{"label":"billing","confidence":0.99}`), lps)
	if mi.Ambiguous != 1 {
		t.Errorf("ambiguous = %d, want 1 — a token prefixing two labels is credited to none", mi.Ambiguous)
	}
}

func TestEntryFromCarriesTheMarginScaleFields(t *testing.T) {
	e := entryFrom(core.TaskTriage, core.Meta{
		Margin: 0.06, MarginScale: core.MarginScaleFull, MarginDeclaredMass: 0.1, MarginAmbiguous: 2,
	}, false, 10)
	if e.MarginScale != core.MarginScaleFull {
		t.Errorf("row margin_scale = %q, want full", e.MarginScale)
	}
	if e.MarginDeclaredMass != 0.1 {
		t.Errorf("row margin_declared_mass = %v, want 0.1", e.MarginDeclaredMass)
	}
	if e.MarginAmbiguous != 2 {
		t.Errorf("row margin_ambiguous = %v, want 2", e.MarginAmbiguous)
	}
}

// The harvest gate: a full-scale margin must never be compared against the
// matched-scale 0.6 constant.
func TestExemplarHarvestOnTheFullScale(t *testing.T) {
	full := core.Meta{Margin: 0.06, MarginScale: core.MarginScaleFull}

	p := marginGatePipeline(t, func(c *config.Config) { c.ConfidenceMarginFullDenominator = true })
	if p.goodExemplar(full) {
		t.Error("harvested on the full scale with no full threshold set")
	}

	p2 := marginGatePipeline(t, func(c *config.Config) {
		c.ConfidenceMarginFullDenominator = true
		c.ConfidenceMarginThresholdFull = 0.05
	})
	if !p2.goodExemplar(full) {
		t.Error("a full margin clearing the full threshold must be harvested")
	}
	if p2.goodExemplar(core.Meta{Margin: 0.01, MarginScale: core.MarginScaleFull}) {
		t.Error("a full margin below the full threshold must not be harvested")
	}
}

// Flag off: the harvest gate keeps its historic matched-scale behaviour.
func TestExemplarHarvestOnTheMatchedScaleIsUnchanged(t *testing.T) {
	p := marginGatePipeline(t, nil)
	if p.goodExemplar(core.Meta{Margin: 0.5, MarginScale: core.MarginScaleMatched}) {
		t.Error("matched margin 0.5 must still be rejected by the 0.6 constant")
	}
	if !p.goodExemplar(core.Meta{Margin: 0.7, MarginScale: core.MarginScaleMatched}) {
		t.Error("matched margin 0.7 must still be harvested")
	}
	no := false
	if p.goodExemplar(core.Meta{Grounded: &no}) {
		t.Error("an ungrounded row must still be rejected")
	}
}
