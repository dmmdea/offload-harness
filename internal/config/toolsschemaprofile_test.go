package config

import (
	"strings"
	"testing"
)

// TestToolsSchemaProfileDefaultsToFull (ENH-13a): a config that does not mention the key, and Default() itself, advertise the full
// tool list; the key is opt-in.
func TestToolsSchemaProfileDefaultsToFull(t *testing.T) {
	if got := Default().ToolsSchemaProfile; got != ToolsProfileFull {
		t.Fatalf("Default().ToolsSchemaProfile = %q, want %q", got, ToolsProfileFull)
	}
	c, err := Load(writeCfg(t, `{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ToolsSchemaProfileMode(); got != ToolsProfileFull {
		t.Fatalf("a config without the key resolves to %q, want %q", got, ToolsProfileFull)
	}
	if got := (Config{}).ToolsSchemaProfileMode(); got != ToolsProfileFull {
		t.Fatalf("the zero Config resolves to %q, want %q", got, ToolsProfileFull)
	}
}

// TestToolsSchemaProfileAcceptsTheClosedSet: both profiles load and resolve, and a copy-pasted value is not demoted.
func TestToolsSchemaProfileAcceptsTheClosedSet(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"full", ToolsProfileFull},
		{"compact", ToolsProfileCompact},
		{" Compact ", ToolsProfileCompact},
		{"FULL", ToolsProfileFull},
		{"", ToolsProfileFull},
	} {
		c, err := Load(writeCfg(t, `{"model":"x","tools_schema_profile":"`+tc.raw+`"}`))
		if err != nil {
			t.Fatalf("tools_schema_profile %q: load failed: %v", tc.raw, err)
		}
		if got := c.ToolsSchemaProfileMode(); got != tc.want {
			t.Errorf("tools_schema_profile %q resolves to %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestToolsSchemaProfileRefusesAnUnknownProfile: a typo fails at the config door naming the key, the value and the valid profiles,
// never silently reads as full (the list the operator meant to shrink stays large) or as compact (prose the operator meant to keep
// is cut).
func TestToolsSchemaProfileRefusesAnUnknownProfile(t *testing.T) {
	_, err := Load(writeCfg(t, `{"model":"x","tools_schema_profile":"tiny"}`))
	if err == nil {
		t.Fatal("an unknown tools_schema_profile must refuse the load")
	}
	for _, want := range []string{"tools_schema_profile", `"tiny"`, "full", "compact"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
	// A Config built by hand with the same value resolves to full: the resolver never shrinks on a value it does not know.
	if got := (Config{ToolsSchemaProfile: "tiny"}).ToolsSchemaProfileMode(); got != ToolsProfileFull {
		t.Fatalf("a hand-built unknown profile resolves to %q, want %q", got, ToolsProfileFull)
	}
}
