package mcpserver

import (
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// TestBrowseAuditPathFollowsTheInstallRoot: the audit trail a browse grant requires (and
// the one audit_all_doors attaches) is data, so it hangs off the harness install root like
// every other store. It used to be pinned to ~/.local-offload, which put it on the OS
// drive of a node whose `home` had moved to a data drive (register C-92).
func TestBrowseAuditPathFollowsTheInstallRoot(t *testing.T) {
	base := filepath.Join(t.TempDir(), "local-offload")
	want := filepath.Join(base, "agent-audit.jsonl")
	for _, tc := range []struct {
		mode   string
		browse bool
		path   string
	}{
		{"off", true, want},
		{"off", false, ""},
		{"warn", false, want},
		{"enforce", false, want},
	} {
		got := doorAuditFor(config.Config{Home: base, AuditAllDoors: tc.mode}, tc.browse)
		if got.Path != tc.path {
			t.Errorf("mode %q browse %v: path %q, want %q", tc.mode, tc.browse, got.Path, tc.path)
		}
	}
}
