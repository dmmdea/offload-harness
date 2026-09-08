// loopaccel.go wires every configured accelerator into the agent loop as an
// ordered set of lanes (Coral design D5). It generalises loopnpu.go, which
// built the single Hailo lane: same on-demand sidecar, same per-process
// singleton per endpoint, same defer-not-crash semantics — one table entry per
// device instead of one function per device.
package pipeline

import (
	"context"
	"encoding/json"
	"time"

	"github.com/dmmdea/offload-harness/internal/accelclient"
	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
)

// laneConfig is one accelerator's sidecar parameters as the config names them.
// Adding a device means adding a row here and in mcpserver's table — the
// config keys mirror each other by design (Coral D3).
type laneConfig struct {
	endpoint string
	cmd      string
	timeout  time.Duration
	idleSec  int
}

func laneConfigFor(cfg config.Config, id string) (laneConfig, bool) {
	switch id {
	case "hailo-8l":
		t := time.Duration(cfg.HailoTimeoutSec) * time.Second
		if t <= 0 {
			t = 60 * time.Second
		}
		return laneConfig{cfg.HailoEndpoint, cfg.HailoSidecarCmd, t, cfg.HailoIdleSec}, true
	case "coral-edgetpu":
		t := time.Duration(cfg.CoralTimeoutSec) * time.Second
		if t <= 0 {
			t = 30 * time.Second
		}
		return laneConfig{cfg.CoralEndpoint, cfg.CoralSidecarCmd, t, cfg.CoralIdleSec}, true
	}
	return laneConfig{}, false
}

// loopAccelSidecar is the per-process singleton per (device, endpoint) — the
// same rule loopNPUSidecar documents: N concurrent loops on a cold sidecar
// must share ONE spawn, so Ensure is serialised on one instance.
func loopAccelSidecar(id string, lc laneConfig) *accelclient.Sidecar {
	loopNPUMu.Lock()
	defer loopNPUMu.Unlock()
	key := id + "@" + lc.endpoint
	if sc, ok := loopNPUSidecars[key]; ok {
		return sc
	}
	var spawn func() error
	if lc.cmd != "" {
		spawn = accelclient.SpawnCmd(lc.cmd, lc.idleSec)
	}
	sc := accelclient.NewSidecar(accelclient.NewDevice(id, lc.endpoint, lc.timeout), spawn, 45*time.Second)
	loopNPUSidecars[key] = sc
	return sc
}

// NewLoopAccel returns the loop's accelerator lanes in config.Accelerators
// order — nil when the box lists none, which keeps the advertised tool list
// byte-identical to a box without any device (the registration pin). An id
// with no lane config (a device this build has no adapter for) is skipped, and
// the loop registers nothing for it rather than inventing tools.
func NewLoopAccel(cfg config.Config) []agent.AccelLane {
	var lanes []agent.AccelLane
	for _, id := range cfg.Accelerators {
		lc, ok := laneConfigFor(cfg, id)
		if !ok {
			continue
		}
		sc := loopAccelSidecar(id, lc)
		device := id
		lanes = append(lanes, agent.AccelLane{ID: id, Call: func(ctx context.Context, tool string, args map[string]any) (string, error) {
			deferOut := func(reason string) (string, error) {
				b, _ := json.Marshal(map[string]any{"deferred": true, "reason": device + ": " + reason})
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
		}})
	}
	return lanes
}
