package gpuprobe

// This file is the ONE rule for "which card is a display card", shared by the
// two places that have to know: the lease verdict (internal/gpuactivity, which
// must never call the operator's desktop the holder's work) and the fleet
// node's health (internal/fleetnode, which must never let that desktop cost the
// node a placement). It lives in this leaf so the two cannot drift — the
// placement guards and the health sampler already read every card through one
// parser here, for the same reason.
//
// It reads nvidia-smi's display_active: the card property itself. The first cut
// inferred it from the process list instead — "nvidia-smi could not size this
// process, so it is the desktop" — and that was wrong twice over. `[N/A]`
// memory is a WDDM property, not a graphics-process property: nvidia-smi cannot
// size ANY process on Windows, and it types every one of the Qube's 24 desktop
// rows `C+G`, compute AND graphics. A native-Windows CUDA seat (ComfyUI is
// exactly that, and the 3-card law pins it to card 0 or 2) produces the same
// `[N/A]`, so the heuristic would have flagged the card the harness was working
// on, dropped it from work_util_pct, and made a SATURATED node advertise itself
// as idle — inverting the defect it was written to cure.
//
// Measured 2026-09-21 with `--query-gpu=display_active`:
//
//	Qube, 3 cards, operator gaming ....... Disabled / ENABLED / Disabled  (card 1, exactly)
//	Lenovo, headless Linux, A2 ........... Disabled                        (nothing flagged)
//	Aorus, laptop, screen on the iGPU .... Disabled                        (the RTX is scored)

// DisplayCardUUIDs returns, by UUID, the cards driving a display — the ones the
// 3-card law forbids seats from using, so utilization on them is never the
// harness's own work.
//
// The guard matters as much as the rule: a box whose ONLY card is its display
// card runs its seats there by necessity (a single-GPU desktop, an iGPU node).
// Excluding it would leave that box with no card left to score, so a display
// card is only ever excluded when at least one non-display card exists.
//
// A nil result means "exclude nothing" — which is also what a driver that does
// not report display_active yields, because absent evidence is never a yes.
func DisplayCardUUIDs(devices []Device) map[string]bool {
	display := make(map[string]bool)
	eligible := 0
	for _, d := range devices {
		if d.DisplayActive && d.UUID != "" {
			display[d.UUID] = true
			continue
		}
		eligible++
	}
	if len(display) == 0 || eligible == 0 {
		return nil
	}
	return display
}
