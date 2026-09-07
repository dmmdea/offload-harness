package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestAgentEnvRulesZeroAndNilAreNoOps(t *testing.T) {
	var nilRules *AgentEnvRules
	if !nilRules.IsZero() || nilRules.Validate() != nil || nilRules.Summary() != "none" {
		t.Fatal("nil table must be zero, valid, and summarize as none")
	}
	if !(&AgentEnvRules{}).IsZero() {
		t.Fatal("empty table must be zero")
	}
}

func TestAgentEnvRulesValidateNamesEveryDefect(t *testing.T) {
	r := &AgentEnvRules{
		DenyTools:            []string{" ", " list_dir"},
		AllowTools:           []string{""},
		MaxCallsPerTool:      map[string]int{"read_file": 0, "grep ": 2},
		ArgLimits:            map[string]map[string]float64{"read_file": {"limit": -1, "offset": 400.5, "depth": 1e21}, "list_dir": {}},
		MaxObservationTokens: -5,
		ObservationStrip:     []string{"", "(unclosed"},
		RewriteError:         []AgentErrorRewrite{{Match: "", Text: ""}, {Match: "[bad", Text: "x"}},
	}
	err := r.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	if !errors.Is(err, ErrAgentEnvRules) {
		t.Fatalf("validation errors must wrap ErrAgentEnvRules: %v", err)
	}
	for _, want := range []string{
		"deny_tools[0]: empty name", `deny_tools[1]: " list_dir" has surrounding whitespace`, "allow_tools[0]",
		"max_calls_per_tool[read_file]", `max_calls_per_tool: "grep " has surrounding whitespace`,
		"arg_limits[read_file][limit]: negative", "arg_limits[read_file][offset]: cap 400.5 must be a whole number",
		"arg_limits[read_file][depth]: cap 1e+21 must be a whole number",
		"arg_limits[list_dir]: no arguments", "max_observation_tokens", "observation_strip[0]", "observation_strip[1]",
		"rewrite_error[0]: empty match", "rewrite_error[0]: empty text", "rewrite_error[1]",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q; got:\n%s", want, err)
		}
	}
	if err := (&AgentEnvRules{MaxObservationTokens: 10}).Validate(); err == nil || !strings.Contains(err.Error(), "want ≥ 64") {
		t.Fatalf("a cap below the marker floor must be refused: %v", err)
	}
	if err := (&AgentEnvRules{MaxObservationTokens: 64}).Validate(); err != nil {
		t.Fatalf("the floor itself is valid: %v", err)
	}
}

func TestAgentEnvRulesValidTableRoundTripsJSON(t *testing.T) {
	src := `{"deny_tools":["run_shell"],"max_calls_per_tool":{"list_dir":2},"arg_limits":{"read_file":{"limit":400}},"max_observation_tokens":3000,"observation_strip":["(?m)^\\[DEBUG\\].*$"],"rewrite_error":[{"match":"no such file","text":"That path does not exist; list the directory first."}]}`
	var r AgentEnvRules
	if err := json.Unmarshal([]byte(src), &r); err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("valid table rejected: %v", err)
	}
	if r.IsZero() {
		t.Fatal("populated table reads as zero")
	}
	sum := r.Summary()
	for _, k := range []string{"deny_tools=1", "max_calls_per_tool=1", "arg_limits=1", "max_observation_tokens=3000", "observation_strip=1", "rewrite_error=1"} {
		if !strings.Contains(sum, k) {
			t.Errorf("summary missing %q: %s", k, sum)
		}
	}
	out, _ := json.Marshal(r)
	var back AgentEnvRules
	if err := json.Unmarshal(out, &back); err != nil || back.Validate() != nil || back.MaxObservationTokens != 3000 {
		t.Fatalf("round trip lost data: %s", out)
	}
}
