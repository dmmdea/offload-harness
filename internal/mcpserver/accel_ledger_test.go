package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// E-04: a forwarded accelerator call that defers (here: no delegate_remotes)
// still lands as one ledger row, so the savings ledger sees accelerator
// traffic instead of nothing. The row names the sidecar tool and the device
// with the fleet suffix, and carries the same reason text the caller saw.
func TestForwardedAccelCallWritesALedgerRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	led, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer led.Close()
	cfg := config.Default()
	cfg.FleetAccelerators = []string{"coral-edgetpu"}
	s := New(pipeline.New(cfg, nil, nil, led))
	h := s.handleFleetAccelTool("coral-edgetpu", "classify", "image_path")
	args, _ := json.Marshal(map[string]any{"image_path": "x.jpg"})
	res, err := h(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}})
	if err != nil {
		t.Fatal(err)
	}
	if txt := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(txt, `"deferred":true`) {
		t.Fatalf("result = %s", txt)
	}
	rows, err := ledger.ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 ledger row, got %d", len(rows))
	}
	r := rows[0]
	if r.Task != "classify" || r.ModelTier != "coral-edgetpu"+FleetOwnerSuffix || !r.Deferred || !strings.Contains(r.Reason, "delegate_remotes") {
		t.Errorf("row = %+v", r)
	}
	// An empty required argument is refused before any forwarding and writes
	// no row: nothing ran, so there is nothing to account for.
	empty, _ := json.Marshal(map[string]any{"image_path": ""})
	if _, err := h(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: empty}}); err != nil {
		t.Fatal(err)
	}
	if rows, _ := ledger.ReadAll(path); len(rows) != 1 {
		t.Fatalf("empty-arg refusal must not write a row; got %d rows", len(rows))
	}
}
