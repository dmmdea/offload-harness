package main

import (
	"strings"
	"testing"
)

// A-40: a llama.cpp 9B agent seat must not be offered on the A2 (the ampere-16 tier's 16 GB card).
//
// It was measured on that card (register A-07): the 9B under vLLM TIES the 4B at a 4096 window, the 9B under
// llama.cpp FAILS, and the tier's answer is the 27B GSQ seat under vLLM with the 4B as its fallback. The row was
// recorded as a prohibition and never code-gated: ampere-16 simply has no include_mimo_9b / include_qwen35_9b key,
// so nothing refused a profile edit or a template change that rendered one. `install render` is where a rule like
// this becomes a refusal, because the render is the last point before the config is written.

// withModelEntry adds a minimal llama-swap model entry to a rendered config and fails when there is no `models:`
// block to add it to, so the control edit cannot silently be a no-op.
func withModelEntry(t *testing.T, cfg, name string) string {
	t.Helper()
	out := strings.Replace(cfg, "\nmodels:\n", "\nmodels:\n  "+name+":\n    cmd: \"true\"\n    ttl: 300\n", 1)
	if out == cfg {
		t.Fatalf("the control edit found no `models:` block to put %q in", name)
	}
	return out
}

func TestRenderGateRefusesALlamaCppNineBAgentSeatOnAmpere16(t *testing.T) {
	shipped := tierRender(t, "ampere-16")
	if err := renderGate(shipped); err != nil {
		t.Fatalf("control: the shipped ampere-16 render must pass the write gate: %v", err)
	}
	for _, seat := range []string{"mimo-9b-agent", "qwen3.5-9b-agent"} {
		res := shipped
		res.Config = withModelEntry(t, shipped.Config, seat)
		err := renderGate(res)
		if err == nil {
			t.Fatalf("ampere-16 rendered a llama.cpp 9B agent seat (%s) and the write gate passed it", seat)
		}
		for _, want := range []string{"ampere-16", seat, "A-40", "not written"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal for %s must carry %q, got: %v", seat, want, err)
			}
		}
	}
}

// The rule is the A2's, not a rule against 9B seats: the 8 GB tiers ship the very same entries and the gate must
// not touch them.
func TestRenderGateLeavesTheNineBSeatsOfTheEightGBTiersAlone(t *testing.T) {
	for _, tier := range []string{"ampere-8", "blackwell-8"} {
		res := tierRender(t, tier)
		if !strings.Contains(res.Config, "mimo-9b-agent") {
			t.Fatalf("control: the %s render no longer carries mimo-9b-agent, so this test says nothing", tier)
		}
		if err := renderGate(res); err != nil && strings.Contains(err.Error(), "A-40") {
			t.Errorf("%s: the A2 rule refused an 8 GB tier: %v", tier, err)
		}
	}
}
