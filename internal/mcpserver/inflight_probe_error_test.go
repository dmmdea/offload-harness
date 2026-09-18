// inflight_probe_error_test.go: review round 1, LOW item 5 — localSeatView
// reports WHY the in-flight probe failed, mirroring ctx_probe_error, instead
// of leaving an operator staring at verdict:"unknown" with no trace.

package mcpserver

import (
	"context"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

func TestLocalSeatViewReportsTheInflightProbeError(t *testing.T) {
	cfg := config.Default()
	cfg.Endpoint = "http://127.0.0.1:1" // nothing listens on port 1
	if cfg.AgentPlannerModel("") == "" {
		t.Skip("fixture has no resolvable seat name; nothing to probe")
	}
	v := localSeatView(context.Background(), cfg)
	if v["verdict"] != "unknown" {
		t.Fatalf("verdict = %v, want unknown (the endpoint is unreachable)", v["verdict"])
	}
	if errStr, _ := v["inflight_probe_error"].(string); errStr == "" {
		t.Fatalf("inflight_probe_error must explain the failed probe, got %v", v)
	}
}
