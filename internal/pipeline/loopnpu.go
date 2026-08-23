// loopnpu.go wires the agent loop's accelerator lane (ADR 0024): the same
// on-demand Hailo sidecar the MCP tools use, injected into the loop as an
// agent.NPUFunc so internal/agent stays free of any hailoclient import.
package pipeline

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/hailoclient"
)

// One Sidecar per endpoint per PROCESS — the loop-side twin of the MCP
// server's sync.Once singleton (mcpserver.hailoSidecar). Without this, every
// agent.Build call constructed its own Sidecar with its own Ensure mutex, and
// an agent_delegate fan-out landing N concurrent contracts on a cold sidecar
// would race N independent spawns (adversarial review finding, 2026-08-23).
// Sharing one instance serializes Ensure for every loop on this process;
// cross-PROCESS spawn races remain governed by health-check-first plus the
// sidecar's own loopback port-bind exclusivity, exactly as they already are
// between the MCP server and a concurrent CLI run.
var (
	loopNPUMu       sync.Mutex
	loopNPUSidecars = map[string]*hailoclient.Sidecar{}
)

func loopNPUSidecar(cfg config.Config) *hailoclient.Sidecar {
	loopNPUMu.Lock()
	defer loopNPUMu.Unlock()
	if sc, ok := loopNPUSidecars[cfg.HailoEndpoint]; ok {
		return sc
	}
	timeout := time.Duration(cfg.HailoTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	var spawn func() error
	if cfg.HailoSidecarCmd != "" {
		spawn = hailoclient.SpawnCmd(cfg.HailoSidecarCmd, cfg.HailoIdleSec)
	}
	sc := hailoclient.NewSidecar(hailoclient.New(cfg.HailoEndpoint, timeout), spawn, 45*time.Second)
	loopNPUSidecars[cfg.HailoEndpoint] = sc
	return sc
}

// NewLoopNPU returns the agent loop's accelerator closure for this box, or nil
// when the config lists no hailo-8l accelerator — nil keeps the loop's
// advertised tool list byte-identical (the registration gate lives in
// agent.ReadOnlyTools, mirroring the MCP surface's HasAccelerator gate).
//
// Defer-not-crash: transport/spawn failures become {"deferred":true,...}
// STRINGS with a nil error (never tool errors), the exact semantics of
// mcpserver.hailoCall — the loop reads the defer and does the work another way.
func NewLoopNPU(cfg config.Config) agent.NPUFunc {
	if !cfg.HasAccelerator("hailo-8l") {
		return nil
	}
	sc := loopNPUSidecar(cfg)
	return func(ctx context.Context, tool string, args map[string]any) (string, error) {
		deferOut := func(reason string) (string, error) {
			b, _ := json.Marshal(map[string]any{"deferred": true, "reason": "hailo-8l: " + reason})
			return string(b), nil
		}
		if err := sc.Ensure(ctx); err != nil {
			return deferOut(err.Error())
		}
		out, err := sc.Client().Call(ctx, tool, args)
		if err != nil {
			return deferOut(err.Error())
		}
		b, err := json.Marshal(out)
		if err != nil {
			return deferOut("non-serializable result: " + err.Error())
		}
		return string(b), nil
	}
}
