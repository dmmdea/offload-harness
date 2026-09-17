package vllmseat

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

func f64(v float64) *float64 { return &v }
func iptr(v int) *int        { return &v }

// TestBindingsCarryTheBoundLaneSettingsOnlyWhenSet (ADR 0049 Amendment 3): a
// seat that declares its bound-lane settings binds them beside agent_model
// under the SAME keys the harness config reads; a seat that declares none
// binds none — a zero must never reach a box config as if it were a decision
// — and the fallback bindings never carry them.
func TestBindingsCarryTheBoundLaneSettingsOnlyWhenSet(t *testing.T) {
	bare := ref().Bindings()
	for _, k := range []string{"agent_max_tokens", "agent_thinking", "agent_sampling", "agent_timeout_sec", "agent_seat_tok_s"} {
		if _, present := bare[k]; present {
			t.Errorf("an undeclared bound-lane setting was bound: %s=%v", k, bare[k])
		}
	}
	s := ref()
	s.AgentMaxTokens, s.AgentThinking, s.AgentTimeoutSec, s.AgentSeatTokS = 4096, "off", 900, 7.17
	s.AgentSampling = &core.AgentSampling{Temperature: f64(0.7), TopP: f64(0.8), TopK: iptr(20), PresencePenalty: f64(1.5)}
	if err := s.Validate("t"); err != nil {
		t.Fatalf("the measured bound-lane settings must validate: %v", err)
	}
	b := s.Bindings()
	if b["agent_model"] != s.ID || b["agent_max_tokens"] != 4096 || b["agent_thinking"] != "off" || b["agent_timeout_sec"] != 900 || b["agent_seat_tok_s"] != 7.17 {
		t.Fatalf("bound-lane bindings: %v", b)
	}
	if smp, ok := b["agent_sampling"].(*core.AgentSampling); !ok || smp == nil || smp.TopK == nil || *smp.TopK != 20 {
		t.Fatalf("agent_sampling must bind as the config's own AgentSampling shape, got %T %v", b["agent_sampling"], b["agent_sampling"])
	}
	f := s.FallbackBindings()
	for _, k := range []string{"agent_max_tokens", "agent_thinking", "agent_sampling", "agent_timeout_sec", "agent_seat_tok_s"} {
		if _, present := f[k]; present {
			t.Errorf("the FALLBACK lane must keep the tier's config_seed values; it bound %s=%v", k, f[k])
		}
	}
	// A declared-but-empty sampling block is not a decision either.
	s.AgentSampling = &core.AgentSampling{}
	if _, present := s.Bindings()["agent_sampling"]; present {
		t.Errorf("an empty agent_sampling block must not be bound")
	}
}

// TestValidateRefusesBadBoundLaneSettings: the declaration is checked with the
// rules the harness config applies, so a tier cannot seed a lane the box
// would refuse or silently misrun.
func TestValidateRefusesBadBoundLaneSettings(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Spec)
		want   string
	}{
		"thinking outside the closed vocabulary": {func(s *Spec) { s.AgentThinking = "sometimes" }, "agent_thinking"},
		"timeout above the wire cap":             {func(s *Spec) { s.AgentTimeoutSec = core.AgentTimeoutSecCap + 1 }, "agent_timeout_sec"},
		"negative step budget":                   {func(s *Spec) { s.AgentMaxTokens = -1 }, "agent_max_tokens"},
		"negative seat rate":                     {func(s *Spec) { s.AgentSeatTokS = -0.5 }, "agent_seat_tok_s"},
		"sampling the config would refuse":       {func(s *Spec) { s.AgentSampling = &core.AgentSampling{TopP: f64(1.5)} }, "agent_sampling"},
	}
	for name, c := range cases {
		s := ref()
		c.mutate(&s)
		err := s.Validate("t")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Validate = %v, want an error naming %s", name, err, c.want)
		}
	}
	// The cap itself is allowed: the operator's 900 s is exactly the wire cap.
	s := ref()
	s.AgentTimeoutSec = core.AgentTimeoutSecCap
	if err := s.Validate("t"); err != nil {
		t.Errorf("agent_timeout_sec at the cap must validate: %v", err)
	}
}
