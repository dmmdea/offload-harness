package config

import (
	"strings"
	"testing"
)

// TestConfigRejectsAnUnknownCoherenceProbePolicy (register D-118): the key is a
// closed set, and a typo must fail at the config door NAMING the key — not
// silently resolve to the default. "off" and "always" differ by one GPU lease
// when a seat goes NaN, so a policy the operator did not choose is a real cost.
func TestConfigRejectsAnUnknownCoherenceProbePolicy(t *testing.T) {
	_, err := Load(writeCfg(t, `{"model":"x","agent_coherence_probe":"sometimes"}`))
	if err == nil {
		t.Fatal("an unknown agent_coherence_probe must refuse the load")
	}
	if !strings.Contains(err.Error(), "agent_coherence_probe") {
		t.Fatalf("error = %q, want it to name the key", err)
	}
	if !strings.Contains(err.Error(), "sometimes") {
		t.Fatalf("error = %q, want it to quote the rejected value", err)
	}
	for _, want := range []string{"cold", "off", "always"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to list the valid policy %q", err, want)
		}
	}
}

// TestConfigAcceptsEveryCoherenceProbePolicy: the closed set loads, and
// CoherenceProbe() resolves each one — including the unset key, which is
// "cold" (probe after a cold load).
func TestConfigAcceptsEveryCoherenceProbePolicy(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"", "cold"},
		{"cold", "cold"},
		{"off", "off"},
		{"always", "always"},
		{" Always ", "always"}, // a copy-paste is accepted, never silently demoted
	} {
		c, err := Load(writeCfg(t, `{"model":"x","agent_coherence_probe":"`+tc.raw+`"}`))
		if err != nil {
			t.Fatalf("agent_coherence_probe %q: %v", tc.raw, err)
		}
		if got := c.CoherenceProbe(); got != tc.want {
			t.Fatalf("agent_coherence_probe %q resolved to %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestCoherenceProbeDefaultsToColdOnAHandBuiltConfig: an in-process Config
// (every test pipeline, and the CLI's own construction) never went through
// Load, so CoherenceProbe must fall back to the default rather than to "off" —
// an unvalidated value must not be able to disable the gate silently.
func TestCoherenceProbeDefaultsToColdOnAHandBuiltConfig(t *testing.T) {
	if got := (Config{}).CoherenceProbe(); got != "cold" {
		t.Fatalf("zero Config resolves to %q, want \"cold\"", got)
	}
	if got := (Config{AgentCoherenceProbe: "nonsense"}).CoherenceProbe(); got != "cold" {
		t.Fatalf("an unvalidated value resolves to %q, want the default \"cold\"", got)
	}
	if got := Default().CoherenceProbe(); got != "cold" {
		t.Fatalf("Default() resolves to %q, want \"cold\"", got)
	}
}
