package config

// Coverage for the full-denominator decision-margin flag (register D-130).
//
// The two scales are NOT interchangeable: research measured the declared
// (matched) mass at ~0.097 on llama.cpp, so the matched margin runs about 10x
// the full one. Applying the calibrated matched constant 0.65 to the full scale
// would turn a gate that fires on the torn few into one that fires on almost
// everything — so the full scale gets its OWN threshold, and until it is
// re-derived the gate must be loudly disabled rather than quietly wrong.

import (
	"bytes"
	"strings"
	"testing"
)

const (
	fullDisabledWarn = "confidence_margin_full_denominator is on but confidence_margin_threshold_full is 0"
	fullMatchedWarn  = "confidence_margin_threshold_full"
	fullDeadFlagWarn = "is set but confidence_margin_full_denominator is off"
)

func deadThresholdOut(t *testing.T, c Config) string {
	t.Helper()
	var buf bytes.Buffer
	warnDeadThresholdsTo(c, &buf)
	return buf.String()
}

func TestDefaultLeavesFullDenominatorOff(t *testing.T) {
	d := Default()
	if d.ConfidenceMarginFullDenominator {
		t.Error("confidence_margin_full_denominator must default to false")
	}
	if d.ConfidenceMarginThresholdFull != 0 {
		t.Errorf("confidence_margin_threshold_full = %v, want 0", d.ConfidenceMarginThresholdFull)
	}
	if d.ConfidenceMarginThreshold != 0.65 {
		t.Errorf("confidence_margin_threshold = %v, want the unchanged 0.65", d.ConfidenceMarginThreshold)
	}
}

func TestFullDenominatorOnWithoutThresholdWarnsGateDisabled(t *testing.T) {
	out := deadThresholdOut(t, Config{
		ConfidenceMarginThreshold:       0.65,
		ConfidenceMarginFullDenominator: true,
	})
	if !strings.Contains(out, fullDisabledWarn) {
		t.Fatalf("want the gate-DISABLED warning, got %q", out)
	}
	if !strings.Contains(out, "DISABLED") {
		t.Fatalf("the warning must say the gate is DISABLED, got %q", out)
	}
	// The matched-scale constant must never be presented as usable on the full scale.
	if strings.Contains(out, "calibrated default is 0.65 (remove the key to inherit it)") {
		t.Fatalf("must not offer the matched-scale 0.65 for the full scale, got %q", out)
	}
}

func TestFullDenominatorOnWithReDerivedThresholdIsSilent(t *testing.T) {
	out := deadThresholdOut(t, Config{
		ConfidenceMarginThreshold:       0.65,
		ConfidenceMarginFullDenominator: true,
		ConfidenceMarginThresholdFull:   0.06,
	})
	if strings.Contains(out, fullMatchedWarn) {
		t.Fatalf("a set full threshold must not warn, got %q", out)
	}
}

func TestMatchedConstantOnTheFullScaleWarns(t *testing.T) {
	out := deadThresholdOut(t, Config{
		ConfidenceMarginFullDenominator: true,
		ConfidenceMarginThresholdFull:   0.65,
	})
	if !strings.Contains(out, "matched-scale") {
		t.Fatalf("0.65 on the full scale must be flagged as a matched-scale value, got %q", out)
	}
}

func TestFullThresholdWithFlagOffWarnsDead(t *testing.T) {
	out := deadThresholdOut(t, Config{ConfidenceMarginThresholdFull: 0.06})
	if !strings.Contains(out, fullDeadFlagWarn) {
		t.Fatalf("a full threshold with the flag off is dead config, got %q", out)
	}
}

func TestFlagOffIsSilentAboutTheFullScale(t *testing.T) {
	out := deadThresholdOut(t, Config{ConfidenceMarginThreshold: 0.65})
	if strings.Contains(out, "full") {
		t.Fatalf("with the flag off and no full threshold nothing about the full scale may be said, got %q", out)
	}
}
