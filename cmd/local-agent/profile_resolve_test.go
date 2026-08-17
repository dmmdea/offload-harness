package main

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
)

// resolveProfileName is the CLI's half of the tier-default contract. The case that
// matters most is the LAST one: --two-tier builds its own architect/editor
// toolsets, so a box-level agent_profile must NOT bleed into it. Without that
// branch a box seeded "research" would silently strip the editor's write tools on
// every two-tier run — and validateFlagCombo cannot catch it, because the operator
// passed no --profile at all.
func TestResolveProfileName(t *testing.T) {
	seeded := config.Config{AgentProfile: "research"}
	unseeded := config.Config{}

	cases := []struct {
		name    string
		flag    string
		twoTier bool
		cfg     config.Config
		want    string
	}{
		{"explicit flag wins over the box default", "edit", false, seeded, "edit"},
		{"unset flag inherits the box default", "", false, seeded, "research"},
		{"whitespace-only flag counts as unset", "   ", false, seeded, "research"},
		{"unset flag on an unseeded box stays general", "", false, unseeded, "general"},
		{"explicit flag on an unseeded box wins", "build", false, unseeded, "build"},

		// The exemption, both directions.
		{"two-tier IGNORES the box default", "", true, seeded, "general"},
		{"two-tier on an unseeded box stays general", "", true, unseeded, "general"},
		{"two-tier keeps an explicit general", "general", true, seeded, "general"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveProfileName(tc.flag, tc.twoTier, tc.cfg); got != tc.want {
				t.Fatalf("resolveProfileName(%q, twoTier=%v, agent_profile=%q) = %q, want %q",
					tc.flag, tc.twoTier, tc.cfg.AgentProfile, got, tc.want)
			}
		})
	}
}

// Whatever the resolver returns must be a name the profile registry actually
// knows, on every path — otherwise the CLI exits 2 on a box whose only mistake
// was leaving --profile off.
func TestResolveProfileNameAlwaysResolvable(t *testing.T) {
	for _, cfg := range []config.Config{{}, {AgentProfile: "research"}, {AgentProfile: "build"}} {
		for _, twoTier := range []bool{false, true} {
			name := resolveProfileName("", twoTier, cfg)
			if name == "" {
				t.Fatalf("empty profile name for cfg=%+v twoTier=%v", cfg, twoTier)
			}
			if _, err := agent.LookupProfile(name); err != nil {
				t.Fatalf("resolver produced an unknown profile %q (cfg=%+v twoTier=%v): %v", name, cfg, twoTier, err)
			}
		}
	}
}
