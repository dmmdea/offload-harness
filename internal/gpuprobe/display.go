package gpuprobe

import (
	"context"
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

// ComputeApp is one row of the per-process query: which card a process sits on,
// what it is, and whether nvidia-smi could size it.
type ComputeApp struct {
	GPUUUID string
	// Name is the process image nvidia-smi reported. It is what keeps the
	// harness's OWN engines from marking the card they run on (see
	// DisplayCardUUIDs): `[N/A]` is a WDDM property, not a graphics-process
	// property, so a CUDA seat visible to this query would otherwise look
	// exactly like the desktop.
	Name string
	// UsedKnown is false when nvidia-smi printed `[N/A]` for the process's
	// memory. On Windows/WDDM that is how GRAPHICS processes appear under
	// --query-compute-apps — the desktop, a browser, a game — and it is the
	// evidence DisplayCardUUIDs reads.
	UsedKnown bool
}

// computeAppsArgs is the per-process query. gpu_uuid and used_memory first and
// process_name LAST: an image path may contain a comma, so putting the name at
// the end keeps it from shifting the two columns the rule reads positionally.
var computeAppsArgs = []string{"--query-compute-apps=gpu_uuid,used_memory,process_name", "--format=csv,noheader,nounits"}

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
		parts := strings.SplitN(line, ",", 3)
		uuid := strings.TrimSpace(parts[0])
		if !strings.HasPrefix(uuid, "GPU-") {
			continue
		}
		used, name := "", ""
		if len(parts) > 1 {
			used = strings.TrimSpace(parts[1])
		}
		if len(parts) > 2 {
			name = strings.TrimSpace(parts[2])
		}
		known := used != "" && !strings.EqualFold(used, "[N/A]") && !strings.EqualFold(used, "N/A")
		apps = append(apps, ComputeApp{GPUUUID: uuid, UsedKnown: known, Name: name})
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

// DisplayCardUUIDs returns, by UUID, the cards a box can PROVE are display
// cards — the ones the 3-card law forbids seats from using, so utilization on
// them is never the harness's own work.
//
// The evidence is the process sample, not config. A card is flagged when it
// hosts a row nvidia-smi could not size (`[N/A]`, which on Windows/WDDM is how
// the desktop appears) whose process is NOT one the harness launches. Measured
// on the Qube 2026-09-20: 24 such rows, every one on card 1 (the 5070 Ti), all
// desktop images — explorer, Chrome, the game — while the harness's own resident
// seat on card 0 produced no row at all. On Linux the query lists only CUDA
// processes, with real memory, so nothing is flagged and every card stays
// eligible — behaviour there is exactly what it was.
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
		if a.UsedKnown || a.GPUUUID == "" {
			continue
		}
		// `[N/A]` is a WDDM property, not a graphics-process property: nvidia-smi
		// cannot size ANY process on WDDM, compute included. On the Qube the
		// harness's own seats happen to be invisible to this query (llama-swap
		// runs in session 0 while the fleet node runs in the operator's session),
		// but that is a session accident, not a guarantee — on a node where they
		// share a session a CUDA seat would look exactly like the desktop and its
		// card would be excluded from the placement figure, making a BUSY node
		// advertise itself as idle. Over-flagging sends work to a loaded box;
		// under-flagging only costs the tie it used to lose. So an engine the
		// harness launches never marks the card it runs on, and the rule needs
		// positive evidence of something else.
		if isHarnessEngine(a.Name) {
			continue
		}
		graphics[a.GPUUUID] = true
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

// harnessEngines are the image names the harness itself launches onto a card.
// Matched on the base name so a full path or a .exe suffix still hits. "python"
// covers the vLLM seats and ComfyUI; including it also means a user's own CUDA
// python never marks a card, which is the safe direction here.
var harnessEngines = []string{"llama-server", "llama-swap", "whisper-server", "vllm", "python", "sd", "stable-diffusion", "comfy"}

// isHarnessEngine reports whether an nvidia-smi process image is one of ours.
func isHarnessEngine(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return false // unattributable: let the caller's other evidence decide
	}
	if i := strings.LastIndexAny(n, `/\`); i >= 0 {
		n = n[i+1:]
	}
	n = strings.TrimSuffix(n, ".exe")
	for _, e := range harnessEngines {
		if n == e || strings.HasPrefix(n, e) {
			return true
		}
	}
	return false
}
