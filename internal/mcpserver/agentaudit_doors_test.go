package mcpserver

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// TestAgentRunDoorAuditFollowsTheConfig (register SF-02): the MCP agent_run door
// attaches no trail with audit_all_doors off (unless browse asks for one, as before),
// an advisory one under warn and an enforcing one under enforce.
func TestAgentRunDoorAuditFollowsTheConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	want := filepath.Join(home, ".local-offload", "agent-audit.jsonl")
	for _, tc := range []struct {
		mode     string
		browse   bool
		path     string
		advisory bool
	}{
		{"", false, "", false},
		{"off", false, "", false},
		{"off", true, want, false},
		{"warn", false, want, true},
		{"warn", true, want, false},
		{"enforce", false, want, false},
	} {
		got := doorAuditFor(config.Config{AuditAllDoors: tc.mode}, tc.browse)
		if got.Path != tc.path || got.Advisory != tc.advisory || got.Refuse != "" {
			t.Errorf("mode %q browse %v: got %+v, want path %q advisory %v", tc.mode, tc.browse, got, tc.path, tc.advisory)
		}
	}
}

// TestAgentRunDoorRefusesAMistypedOrUnresolvableEnforce (SF-02 review): a one-shot verb
// keeps going after a config load error, so a mistyped mode reaches the door with its raw
// value; the door must refuse it rather than run with no trail, and enforce with no
// resolvable home defers by class config, naming the key and the way out.
func TestAgentRunDoorRefusesAMistypedOrUnresolvableEnforce(t *testing.T) {
	if got := doorAuditFor(config.Config{AuditAllDoors: "Enforc"}, false); got.Refuse == "" || got.Path != "" {
		t.Errorf("a mistyped mode must refuse, got %+v", got)
	}
	t.Setenv("USERPROFILE", "")
	t.Setenv("HOME", "")
	cfg := config.Default()
	cfg.Endpoint = "http://127.0.0.1:1" // unreachable: the roster check proceeds, as it does on a dead seat
	cfg.AuditAllDoors = "enforce"
	s := New(pipeline.New(cfg, nil, nil, nil))
	text := browseCallText(t, s.handleAgentRun, map[string]any{"goal": "g"})
	for _, want := range []string{`"deferred":true`, `"defer_class":"config"`, "audit_all_doors", "warn or off"} {
		if !strings.Contains(text, want) {
			t.Errorf("agent_run under enforce with no home: %s does not contain %q", text, want)
		}
	}
}
