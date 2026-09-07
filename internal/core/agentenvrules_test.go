package core

import (
	"encoding/json"
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
		DenyTools:            []string{" "},
		AllowTools:           []string{""},
		MaxCallsPerTool:      map[string]int{"read_file": 0},
		ArgLimits:            map[string]map[string]float64{"read_file": {"limit": -1}, "list_dir": {}},
		MaxObservationTokens: -5,
		ObservationStrip:     []string{"", "(unclosed"},
		RewriteError:         []AgentErrorRewrite{{Match: "", Text: ""}, {Match: "[bad", Text: "x"}},
	}
	err := r.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{
		"deny_tools[0]", "allow_tools[0]", "max_calls_per_tool[read_file]", "arg_limits[read_file][limit]",
		"arg_limits[list_dir]: no arguments", "max_observation_tokens", "observation_strip[0]", "observation_strip[1]",
		"rewrite_error[0]: empty match", "rewrite_error[0]: empty text", "rewrite_error[1]",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q; got:\n%s", want, err)
		}
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
