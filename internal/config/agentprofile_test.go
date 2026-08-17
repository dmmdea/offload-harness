package config

import (
	"encoding/json"
	"testing"
)

// The tool profile's precedence chain: explicit > agent_profile > "general".
// It mirrors AgentPlannerModel deliberately — same shape, same live-at-rest rule.
func TestAgentTaskProfilePrecedence(t *testing.T) {
	c := Config{AgentProfile: "research"}
	if got := c.AgentTaskProfile("edit"); got != "edit" {
		t.Fatalf("an explicit per-call profile must win, got %q", got)
	}
	if got := c.AgentTaskProfile(""); got != "research" {
		t.Fatalf("the configured agent_profile must beat the general default, got %q", got)
	}
	c.AgentProfile = ""
	if got := c.AgentTaskProfile(""); got != "general" {
		t.Fatalf("an unset agent_profile must fall back to general, got %q", got)
	}
}

// The resolver must return a REAL profile name, never "". Every front door feeds
// the result straight to agent.LookupProfile, so an empty return would look up
// the empty profile and error instead of running the documented default.
func TestAgentTaskProfileNeverReturnsEmpty(t *testing.T) {
	for _, c := range []Config{{}, {AgentProfile: ""}, {AgentProfile: "build"}} {
		if got := c.AgentTaskProfile(""); got == "" {
			t.Fatalf("resolver returned empty for %+v", c)
		}
	}
}

// The fallback must stay LIVE at rest: a config that never set agent_profile must
// not materialize "general" into the field, or the chain is forked on disk and a
// later change to the default silently stops reaching existing installs. Same
// contract the agent_model seat is pinned to.
func TestAgentProfileAbsencePreservedRoundTrip(t *testing.T) {
	c := Default()
	if c.AgentProfile != "" {
		t.Fatalf("Default() must leave agent_profile unset, got %q", c.AgentProfile)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if _, present := raw["agent_profile"]; present {
		t.Fatal("agent_profile materialized in the serialized config despite being unset — the live fallback chain has been forked at rest")
	}
}
