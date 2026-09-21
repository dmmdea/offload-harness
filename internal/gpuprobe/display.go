package gpuprobe

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// This file is the ONE rule for "which card is a display card", shared by the
// two places that have to know: the lease verdict (internal/gpuactivity, which
// must never call the operator's desktop the holder's work) and the fleet
// node's health (internal/fleetnode, which must never let that desktop cost the
// node a placement). It lives in this leaf so the two cannot drift — the
// placement guards and the health sampler already read every card through one
// parser here for the same reason.
//
// The history that made it one rule rather than two: 0.132.1 fixed the lease
// verdict, and the same bug class was left standing in the node's
// `gpu_util_pct`, which delegate/gate.go uses to break placement ties. A second
// copy of the rule would have been a second chance to get it wrong.

// ComputeApp is one row of `nvidia-smi --query-compute-apps=gpu_uuid,used_memory`:
// which card a process sits on, and whether nvidia-smi could size it.
type ComputeApp struct {
	GPUUUID string
	// UsedKnown is false when nvidia-smi printed `[N/A]` for the process's
	// memory. On Windows/WDDM that is how GRAPHICS processes appear under
	// --query-compute-apps — the desktop, a browser, a game — and it is the
	// evidence DisplayCardUUIDs reads.
	UsedKnown bool
}

// computeAppsArgs is the per-process query. gpu_uuid first so a row with a
// process name containing a comma cannot shift the columns this parse reads.
var computeAppsArgs = []string{"--query-compute-apps=gpu_uuid,used_memory", "--format=csv,noheader,nounits"}

// ParseComputeApps parses computeAppsArgs' output. Lines that do not carry a
// GPU UUID are skipped, never guessed at: an unattributable row cannot mark any
// card, which is the conservative direction (nothing excluded).
func ParseComputeApps(out string) []ComputeApp {
	var apps []ComputeApp
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ",", 2)
		uuid := strings.TrimSpace(parts[0])
		if !strings.HasPrefix(uuid, "GPU-") {
			continue
		}
		used := ""
		if len(parts) == 2 {
			used = strings.TrimSpace(parts[1])
		}
		known := used != "" && !strings.EqualFold(used, "[N/A]") && !strings.EqualFold(used, "N/A")
		apps = append(apps, ComputeApp{GPUUUID: uuid, UsedKnown: known})
	}
	return apps
}

// ReadComputeApps runs the per-process query. ctx bounds the exec — a wedged
// driver must not stall a sampler tick.
func ReadComputeApps(ctx context.Context) ([]ComputeApp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	bin, err := nvidiaSmiPath()
	if err != nil {
		return nil, err
	}
	out, err := exec.CommandContext(ctx, bin, computeAppsArgs...).Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("nvidia-smi: %w", ctx.Err())
		}
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	return ParseComputeApps(string(out)), nil
}

// ErrNoComputeAppsRunner is returned for a nil injected runner.
var ErrNoComputeAppsRunner = errors.New("gpuprobe: nil compute-apps runner")

// DisplayCardUUIDs returns, by UUID, the cards a box can PROVE are display
// cards — the ones the 3-card law forbids seats from using, so utilization on
// them is never the harness's own work.
//
// The evidence is the process sample, not config: on Windows/WDDM nvidia-smi
// reports graphics processes with `[N/A]` memory. Measured on the Qube
// 2026-09-20: all 28 such rows sat on card 1, the 5070 Ti, while the harness's
// own resident seat on card 0 produced NO row at all. On Linux the query lists
// only CUDA processes, with real memory, so nothing is flagged and every card
// stays eligible — behaviour there is exactly what it was.
//
// The guard matters as much as the rule: a box whose ONLY card is its display
// card runs its seats there by necessity (a single-GPU laptop, an iGPU node).
// Excluding it would leave that box with no card to score at all, so a display
// card is only ever excluded when at least one non-display card exists.
//
// deviceUUIDs is every card the caller knows about; a nil result means
// "exclude nothing".
func DisplayCardUUIDs(deviceUUIDs []string, apps []ComputeApp) map[string]bool {
	graphics := make(map[string]bool)
	for _, a := range apps {
		if !a.UsedKnown && a.GPUUUID != "" {
			graphics[a.GPUUUID] = true
		}
	}
	if len(graphics) == 0 {
		return nil
	}
	eligible := 0
	for _, u := range deviceUUIDs {
		if !graphics[u] {
			eligible++
		}
	}
	if eligible == 0 {
		return nil // every card is a display card: this box works on them anyway
	}
	return graphics
}
