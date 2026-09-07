package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

func TestResolveEnvRulesPrecedence(t *testing.T) {
	cfg := config.Config{AgentEnvRules: &core.AgentEnvRules{MaxObservationTokens: 100}}

	got, err := resolveEnvRules("", cfg)
	if err != nil || got == nil || got.MaxObservationTokens != 100 {
		t.Fatalf("empty flag must return the config table: %+v %v", got, err)
	}
	got, err = resolveEnvRules("off", cfg)
	if err != nil || got != nil {
		t.Fatalf("off must return no table: %+v %v", got, err)
	}

	dir := t.TempDir()
	good := filepath.Join(dir, "rules.json")
	if err := os.WriteFile(good, []byte(`{"deny_tools":["run_shell"],"max_calls_per_tool":{"list_dir":2}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = resolveEnvRules(good, cfg)
	if err != nil || got == nil || got.MaxObservationTokens != 0 || got.MaxCallsPerTool["list_dir"] != 2 {
		t.Fatalf("file must REPLACE the config table: %+v %v", got, err)
	}

	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte(`{"observation_strip":["(unclosed"]}`), 0o644)
	if _, err = resolveEnvRules(bad, cfg); err == nil || !strings.Contains(err.Error(), "observation_strip[0]") {
		t.Fatalf("invalid file must fail by name: %v", err)
	}
	typo := filepath.Join(dir, "typo.json")
	_ = os.WriteFile(typo, []byte(`{"deny_tool":["x"]}`), 0o644)
	if _, err = resolveEnvRules(typo, cfg); err == nil || !strings.Contains(err.Error(), "deny_tool") {
		t.Fatalf("an unknown key must fail (a typo would otherwise no-op silently): %v", err)
	}
	if _, err = resolveEnvRules(filepath.Join(dir, "missing.json"), cfg); err == nil {
		t.Fatal("a missing file must fail, never fall back to the config table")
	}
}

// The shipped starter table must load through the flag that advertises it —
// the strict decoder rejected the first draft's "_comment" key (review
// finding 2026-09-07), which no test had exercised.
func TestShippedEnvRulesExampleLoads(t *testing.T) {
	got, err := resolveEnvRules(filepath.Join("..", "..", "examples", "agent-env-rules.json"), config.Config{})
	if err != nil {
		t.Fatalf("examples/agent-env-rules.json does not load: %v", err)
	}
	if got.IsZero() || got.MaxObservationTokens == 0 || len(got.RewriteError) == 0 {
		t.Fatalf("example decoded empty: %+v", got)
	}
}
