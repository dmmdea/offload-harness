package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestBrowseAuditPathFollowsTheInstallRoot: the audit trail an agent door attaches is
// data, so it lives under the node's install root (config `home`), not the user
// profile's ~/.local-offload. Pinning it there put it on the OS drive of a node whose
// install root had moved to a data drive (register C-92). Driven through a writing
// contract with audit_all_doors=warn, so the trail actually receives rows.
func TestBrowseAuditPathFollowsTheInstallRoot(t *testing.T) {
	profile := t.TempDir()
	homeAt(t, profile)
	base := filepath.Join(t.TempDir(), "local-offload")
	srv := writingFake().server(t)
	defer srv.Close()
	p := writeDoorPipeline(t, srv.URL, true)
	p.cfg.Home, p.cfg.AuditAllDoors = base, "warn"
	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, writeContract("."))))
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	if _, err := os.Stat(filepath.Join(base, "agent-audit.jsonl")); err != nil {
		t.Errorf("no trail under the install root %s: %v", base, err)
	}
	if got := auditRows(t, profile); got != 0 {
		t.Errorf("%d rows landed under the user profile's .local-offload, want 0", got)
	}
}
