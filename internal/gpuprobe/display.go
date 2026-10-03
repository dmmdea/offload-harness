package gpuprobe

// This file is the ONE rule for "which card is a display card", shared by the
// two places that have to know: the lease verdict (internal/gpuactivity, which
// must never call the operator's desktop the holder's work) and the fleet
// node's health (internal/fleetnode, which must never let that desktop cost the
// node a placement). It lives in this leaf so the two cannot drift — the
// placement guards and the health sampler already read every card through one
// parser here, for the same reason.
//
// It reads two card properties from nvidia-smi, never the process list. The
// first cut inferred the display card from the process list — "nvidia-smi could
// not size this process, so it is the desktop" — and that was wrong twice over.
// `[N/A]` memory is a WDDM property, not a graphics-process property: nvidia-smi
// cannot size ANY process on Windows, and it types every one of <node-b>'s 24
// desktop rows `C+G`, compute AND graphics. A native-Windows CUDA seat (ComfyUI
// is exactly that, and the 3-card law pins it to card 0 or 2) produces the same
// `[N/A]`, so the heuristic would have flagged the card the harness was working
// on, dropped it from work_util_pct, and made a SATURATED node advertise itself
// as idle — inverting the defect it was written to cure.
//
// THE TWO PROPERTIES, and why one is not enough:
//
//   - display_active ("Enabled"/"Disabled") says a display is INITIALISED on the
//     card: memory is allocated for it. It is true while a game or a lit screen is
//     being driven and false when the screen sleeps, so on its own it fires only
//     part of the time.
//   - display_attached ("Yes"/"No") says a physical monitor is connected to one of
//     the card's connectors. It does not depend on the screen being awake, and it is
//     what holds at the desk.
//
// A card is a display card when EITHER says so (Device.DrivesDisplay). Only an exact
// "Enabled" / "Yes" counts; "[Not Supported]", "[N/A]" and a missing column mean
// "we do not know", and an unknown is never the operator's screen.
//
// A driver that refuses the display_attached field (an older one, a headless
// Linux build) rejects the whole query, so the reader retries without it
// (RunDisplayAware) and the rule then rests on display_active alone, as before.
// Only a failure that names the field (or a run of them) arms that for ten
// minutes, and it is logged when it does. One transient failure of the full
// query answers that call from the fallback and is forgotten, and the reading it
// produced says so (Device.AttachedUnknown, Card.DisplayUnknown): it cannot tell
// the monitor's card from the others, so the allocator hands out no card from it.
//
// Measured with `--query-gpu=display_active[,display_attached]`:
//
//	2026-09-21  <node-b>, 3 cards, operator gaming ... Disabled / ENABLED / Disabled  (card 1, exactly)
//	2026-09-21  <node-c>, headless Linux, A2 ......... Disabled                        (nothing flagged)
//	2026-09-21  <node-a>, laptop, screen on the iGPU . Disabled                        (the RTX is scored)
//	2026-10-03  the 3-card box, screen asleep ........ active Disabled on all three,
//	                                                   attached No / YES / No          (card 1: the monitor)
//
// The last row is why the rule has two inputs: with the screen asleep the 3-card
// box reported no display card at all through display_active, so the allocator's
// never-auto-pick-the-display-card rule (plan invariant I6) had nothing to act on.

// DisplayCardUUIDs returns, by UUID, the cards driving a display — the ones the
// 3-card law forbids seats from using, so utilization on them is never the
// harness's own work.
//
// The guard matters as much as the rule: a box whose ONLY card is its display
// card runs its seats there by necessity (a single-GPU desktop, an iGPU node).
// Excluding it would leave that box with no card left to score, so a display
// card is only ever excluded when at least one non-display card exists.
//
// A nil result means "exclude nothing" — which is also what a driver that
// reports neither display_active nor display_attached yields, because absent
// evidence is never a yes.
func DisplayCardUUIDs(devices []Device) map[string]bool {
	display := make(map[string]bool)
	eligible := 0
	for _, d := range devices {
		if d.DrivesDisplay() {
			// A display card with no UUID cannot be keyed, so it cannot be
			// excluded — but it is still not a card the harness may place a seat
			// on, so counting it as one would let the guard below exclude the
			// REAL display cards on a box that has nothing left to score.
			if d.UUID != "" {
				display[d.UUID] = true
			}
			continue
		}
		eligible++
	}
	if len(display) == 0 || eligible == 0 {
		return nil
	}
	return display
}
