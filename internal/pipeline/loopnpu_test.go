package pipeline

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// TestLoopNPUSidecarSingleton: every loop on one process shares ONE Sidecar
// per endpoint — the spawn-storm guard (adversarial review, 2026-08-23). Two
// Build calls (two delegated contracts, two agent_run requests) must not get
// independent Ensure mutexes racing independent spawns.
func TestLoopNPUSidecarSingleton(t *testing.T) {
	cfg := config.Default()
	cfg.Accelerators = []string{"hailo-8l"}
	cfg.HailoEndpoint = "http://127.0.0.1:59999" // never contacted in this test
	a := loopNPUSidecar(cfg)
	b := loopNPUSidecar(cfg)
	if a != b {
		t.Fatal("two loopNPUSidecar calls for one endpoint returned distinct Sidecars — spawn-storm guard broken")
	}
	cfg2 := cfg
	cfg2.HailoEndpoint = "http://127.0.0.1:59998"
	if c := loopNPUSidecar(cfg2); c == a {
		t.Fatal("distinct endpoints must not share a Sidecar")
	}
}

// TestNewLoopNPUGated: no accelerator listed => nil (the loop registers no NPU
// tools); listed => a non-nil closure.
func TestNewLoopNPUGated(t *testing.T) {
	cfg := config.Default()
	if NewLoopNPU(cfg) != nil {
		t.Fatal("NewLoopNPU must be nil without the accelerator")
	}
	cfg.Accelerators = []string{"hailo-8l"}
	cfg.HailoEndpoint = "http://127.0.0.1:59997"
	if NewLoopNPU(cfg) == nil {
		t.Fatal("NewLoopNPU must be non-nil with the accelerator listed")
	}
}
