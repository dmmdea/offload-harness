package config

import (
	"encoding/json"
	"testing"
)

// The planner seat's precedence chain: explicit > agent_model > model.
// Pinned because a silent inversion would either ignore operator overrides or
// resurrect the seat/workhorse conflation this field exists to end.
func TestAgentPlannerModelPrecedence(t *testing.T) {
	c := Config{Model: "workhorse", AgentModel: "seat"}
	if got := c.AgentPlannerModel("explicit"); got != "explicit" {
		t.Fatalf("explicit override must win, got %q", got)
	}
	if got := c.AgentPlannerModel(""); got != "seat" {
		t.Fatalf("agent_model must beat the workhorse, got %q", got)
	}
	c.AgentModel = ""
	if got := c.AgentPlannerModel(""); got != "workhorse" {
		t.Fatalf("unset seat must fall back to the workhorse, got %q", got)
	}
}

// The fallback must stay LIVE at rest: a config that never set agent_model
// must not materialize one through a load->save round-trip. A materialized
// copy silently forks the chain — the operator later changes `model` expecting
// the planner to follow (as it always did) and it doesn't.
func TestAgentModelAbsencePreservedRoundTrip(t *testing.T) {
	c := Default() // never sets AgentModel
	if c.AgentModel != "" {
		t.Fatalf("Default() must leave the agent seat unset, got %q", c.AgentModel)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if _, present := raw["agent_model"]; present {
		t.Fatal("agent_model materialized in serialized config despite being unset — the live fallback chain has been forked at rest")
	}
	if _, present := raw["agent_timeout_sec"]; present {
		t.Fatal("agent_timeout_sec materialized in serialized config despite being unset")
	}
}
