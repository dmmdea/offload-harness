package pipeline

import (
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

// The loop's lane table reads the RKNPU's own four keys — endpoint, launcher, call bound, idle window —
// and a zero timeout falls back to 60 s, like the Hailo's (the Coral's is 30). mcpserver keeps the same
// row for the MCP surface; the two must stay level, so both are pinned to the same values.
func TestLaneConfigForRknpu(t *testing.T) {
	cfg := config.Default()
	cfg.RknpuEndpoint, cfg.RknpuSidecarCmd, cfg.RknpuTimeoutSec, cfg.RknpuIdleSec = "http://127.0.0.1:1", "/x/rknpu-http.sh", 42, 77
	cfg.HailoEndpoint, cfg.CoralEndpoint = "http://127.0.0.1:2", "http://127.0.0.1:3"
	lc, ok := laneConfigFor(cfg, "rknpu")
	if !ok || lc.endpoint != "http://127.0.0.1:1" || lc.cmd != "/x/rknpu-http.sh" || lc.timeout != 42*time.Second || lc.idleSec != 77 {
		t.Fatalf("lane config = %+v (ok %v)", lc, ok)
	}
	cfg.RknpuTimeoutSec = 0
	if lc, _ := laneConfigFor(cfg, "rknpu"); lc.timeout != 60*time.Second {
		t.Errorf("a zero rknpu_timeout_sec must fall back to 60 s, got %s", lc.timeout)
	}
}
