package placement

import (
	"fmt"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
)

// LayerAdmissible runs a layer's guards, in declared order, for ONE seat and
// reports the first refusal (ok=false, reason, guard) or an admission whose
// reason carries every guard's reading. It is the only place a guard is
// evaluated: the local decision, the node's re-check at admission and the
// health row's verdict all call it, so "admissible" means one thing.
//
// Every guard fails CLOSED: a reader that is nil, a card the probe cannot
// see, a presence the OS cannot tell — each refuses rather than admitting on
// a number it never had. rowVerdict is a remote row's own answer and stands
// in for a guard ONLY where that guard's reader is nil (the delegator has no
// nvidia-smi on <node-b>'s display card; <node-b> did, and said so); a live
// reading always wins over a carried verdict.
//
// display_floor is `free(display) − seat.DisplayFootprintGiB ≥ floor` on the
// resolved display card (council R6: the old free-VRAM check admitted a
// 10.5 GB load onto a card with 9 GB free). A seat whose pin includes the
// display device but declares no footprint refuses as "display footprint
// undeclared" BEFORE any arithmetic; a seat pinned off the display card is
// checked as free − 0 honestly. A UUID display pin is resolved to its index
// through Live.DeviceIndex; unresolvable = refuse (the seat may be on it).
func LayerAdmissible(l config.LayerSpec, s config.LayerSeat, live Live, rowVerdict *bool) (ok bool, reason, guard string) {
	if len(l.Guards) == 0 {
		return true, "no guards", ""
	}
	var readings []string
	for _, g := range l.Guards {
		var (
			pass bool
			text string
		)
		switch g {
		case "display_floor":
			pass, text = displayFloor(l, s, live, rowVerdict)
		case "host_ram":
			pass, text = hostRAM(s, live, rowVerdict)
		case "presence":
			pass, text = admissionPresence(l, live, rowVerdict)
		default:
			pass, text = false, fmt.Sprintf("unknown guard %q (display_floor, presence, host_ram) — refused", g)
		}
		if !pass {
			return false, g + ": " + text, g
		}
		readings = append(readings, g+": "+text)
	}
	return true, joinReadings(readings), ""
}

// verdictOr is the nil-reader branch shared by the three guards: a carried
// verdict answers, otherwise the guard refuses as unreadable.
func verdictOr(rowVerdict *bool, what string) (bool, string) {
	if rowVerdict != nil {
		if *rowVerdict {
			return true, "admitted on the node's own verdict (no local reader)"
		}
		return false, "refused on the node's own verdict (no local reader)"
	}
	return false, what + " unreadable (no reader) — refused"
}

// resolveDisplay turns a layer's display_device into the CUDA index the probe knows it by, and checks
// that index against the cards the driver says drive the monitor. refusal != "" says why the card
// cannot be vouched for, in the words the guard refuses with.
//
// The declaration is the operator's, the evidence is the driver's. The floor protects the card the
// config NAMES; when the driver singles out other cards as the ones driving the screen (the board
// reorders on a power loss, a cable moves) the floor would guard the wrong card while the desktop's
// own is loaded, so the guard refuses by naming both. It contradicts only on positive evidence: a
// reader that is absent, or a reading that cannot say, trusts the declaration, and a second monitor
// on another card never contradicts a declared card that also drives a display.
func resolveDisplay(l config.LayerSpec, live Live) (display, index, refusal string) {
	display = strings.TrimSpace(l.DisplayDevice)
	if display == "" {
		return "", "", "display_device undeclared — refused"
	}
	index = display
	if !isCUDAIndex(display) {
		if live.DeviceIndex == nil {
			return display, "", fmt.Sprintf("display device %s is a UUID pin and no reader can resolve it to a CUDA index — refused", display)
		}
		idx, ok := live.DeviceIndex(display)
		if !ok {
			return display, "", fmt.Sprintf("display device %s cannot be resolved to a CUDA index (not in the probe, or ambiguous) — refused", display)
		}
		index = idx
	}
	if live.ScreenCards != nil {
		if screens := live.ScreenCards(); len(screens) > 0 && !containsIndex(screens, index) {
			return display, index, fmt.Sprintf("display_device %s is CUDA index %s, but the driver reports the monitor on index %s (display_active / display_attached) — refused: the floor would guard a card that is not the desktop's until display_device names the one that drives the screen",
				display, index, strings.Join(screens, ", "))
		}
	}
	return display, index, ""
}

func containsIndex(indexes []string, want string) bool {
	for _, i := range indexes {
		if i == want {
			return true
		}
	}
	return false
}

// seatOnIndex reports whether the seat's pin includes the CUDA index. A pin element that is a GPU UUID
// (a seat on a box that pins by UUID, because the board re-enumerates indices) is resolved to its index
// through the live probe. One that cannot be resolved cannot be shown to be off the card, so it counts as
// on it and the footprint is then required: an unreadable pin must not turn the floor's arithmetic into
// free - 0 for a seat that may well be the one on the desktop's card.
func seatOnIndex(s config.LayerSeat, index string, live Live) bool {
	for _, d := range s.DeviceList() {
		if isCUDAIndex(d) {
			if d == index {
				return true
			}
			continue
		}
		if live.DeviceIndex == nil {
			return true
		}
		if idx, ok := live.DeviceIndex(d); !ok || idx == index {
			return true
		}
	}
	return false
}

func displayFloor(l config.LayerSpec, s config.LayerSeat, live Live, rowVerdict *bool) (bool, string) {
	if live.DeviceFree == nil {
		return verdictOr(rowVerdict, fmt.Sprintf("free VRAM on display device %s", l.DisplayDevice))
	}
	display, index, refusal := resolveDisplay(l, live)
	if refusal != "" {
		return false, refusal
	}
	onDisplay := seatOnIndex(s, index, live)
	if onDisplay && s.DisplayFootprintGiB <= 0 {
		return false, fmt.Sprintf("display footprint undeclared for seat %s pinned to %s (includes display device %s → index %s) — refused before any arithmetic", s.Model, s.Device, display, index)
	}
	free, ok := live.DeviceFree(display)
	if !ok {
		return false, fmt.Sprintf("free VRAM on display device %s unreadable (card not in the probe) — refused", display)
	}
	footprint := 0.0
	if onDisplay {
		footprint = s.DisplayFootprintGiB
	}
	left := free - footprint
	if left < l.DisplayFloorGiB {
		return false, fmt.Sprintf("free %.1f GiB − footprint %.1f = %.1f < floor %.1f on display device %s (index %s)", free, footprint, left, l.DisplayFloorGiB, display, index)
	}
	return true, fmt.Sprintf("free %.1f GiB − footprint %.1f = %.1f ≥ floor %.1f on display device %s (index %s)", free, footprint, left, l.DisplayFloorGiB, display, index)
}

// ResidentVerdict puts a layer's desktop guards to a seat that is ALREADY loaded: the admission
// guards decide once, at the moment of the placement, and a twin then sits on the desktop's card
// until llama-swap's idle ttl takes it down, long after the operator may have come back or a game
// may have taken the card's memory. It evaluates the two guards that are about the desktop,
// presence and display_floor, in the layer's declared order, with the same readers, the same
// card resolution and screen cross-check, and the same fail-closed reading of anything unreadable.
// ok=false carries one reason per guard that refused.
//
// The floor is free(card) ≥ floor, never the admission arithmetic free − footprint ≥ floor: a
// resident seat's footprint is already out of the free number, and subtracting it again would
// read a healthy 6.2 GiB twin as a violation and unload it forever. host_ram bounds what a seat
// holds in RAM at load time, so it is no violation of one that is already up and is not evaluated
// here; an unknown guard name refuses, as it does at admission.
func ResidentVerdict(l config.LayerSpec, live Live) (ok bool, reasons []string) {
	for _, g := range l.Guards {
		var (
			pass bool
			text string
		)
		switch g {
		case "display_floor":
			pass, text = displayFloorHeld(l, live)
		case "presence":
			pass, text = presence(live, nil)
		case "host_ram":
			continue
		default:
			pass, text = false, fmt.Sprintf("unknown guard %q (display_floor, presence, host_ram) — refused", g)
		}
		if !pass {
			reasons = append(reasons, g+": "+text)
		}
	}
	return len(reasons) == 0, reasons
}

// displayFloorHeld is the display_floor question put to a loaded seat: does the display card still
// have its floor free? See ResidentVerdict for why the footprint is not subtracted.
func displayFloorHeld(l config.LayerSpec, live Live) (bool, string) {
	if live.DeviceFree == nil {
		return false, fmt.Sprintf("free VRAM on display device %s unreadable (no reader) — refused", l.DisplayDevice)
	}
	display, index, refusal := resolveDisplay(l, live)
	if refusal != "" {
		return false, refusal
	}
	free, ok := live.DeviceFree(display)
	if !ok {
		return false, fmt.Sprintf("free VRAM on display device %s unreadable (card not in the probe) — refused", display)
	}
	if free < l.DisplayFloorGiB {
		return false, fmt.Sprintf("free %.1f GiB < floor %.1f GiB on display device %s (index %s) with the layer's seats loaded", free, l.DisplayFloorGiB, display, index)
	}
	return true, fmt.Sprintf("free %.1f GiB ≥ floor %.1f GiB on display device %s (index %s)", free, l.DisplayFloorGiB, display, index)
}

func hostRAM(s config.LayerSeat, live Live, rowVerdict *bool) (bool, string) {
	if live.HostFree == nil {
		return verdictOr(rowVerdict, "free host RAM")
	}
	if s.HostRAMGiB <= 0 {
		return false, fmt.Sprintf("host RAM need undeclared (0) for seat %s — free ≥ 0 is not a guard, refused", s.Model)
	}
	free, ok := live.HostFree()
	if !ok {
		return false, "free host RAM unreadable — refused"
	}
	if free < s.HostRAMGiB {
		return false, fmt.Sprintf("free %.1f GiB < seat need %.1f GiB", free, s.HostRAMGiB)
	}
	return true, fmt.Sprintf("free %.1f GiB ≥ seat need %.1f GiB", free, s.HostRAMGiB)
}

// admissionPresence is the presence guard as admission puts it: the operator is away AND, on the
// display layer, something is watching a twin once it is loaded. The second half is the display layer's
// own: its guards decide once, at the moment of the placement, and what takes a twin down when the
// operator returns is the watcher in fleet-serve. A watcher that is not alive (no heartbeat, a stale
// one, switched off, or up but unable to read llama-swap) leaves an admitted twin on the desktop's
// card until llama-swap's 300 s idle ttl, so the layer does not open while it is so. Other layers
// named in the config are not the watcher's and are judged by presence alone.
func admissionPresence(l config.LayerSpec, live Live, rowVerdict *bool) (bool, string) {
	ok, text := presence(live, rowVerdict)
	if !ok || l.Name != LayerDisplay || live.Presence == nil {
		// A refused presence stands; a nil presence reader means a remote row's own verdict answered
		// for the whole guard, the node having asked its own watcher.
		return ok, text
	}
	if live.WatcherAlive == nil {
		return false, "display watcher unreadable (no reader) — refused: nothing can show a loaded twin is being watched"
	}
	alive, why := live.WatcherAlive()
	if !alive {
		return false, "display watcher not alive — refused: " + why + "; a twin admitted now would stay on the operator's card until llama-swap's idle ttl"
	}
	return true, text + "; " + why
}

func presence(live Live, rowVerdict *bool) (bool, string) {
	if live.Presence == nil {
		return verdictOr(rowVerdict, "operator presence")
	}
	return PresenceAllows(live.Presence())
}

// PresenceAllows is the presence guard's rule over one reading: whether the operator's being away
// (or having said so) lets the display card take, or keep, a load, and the reading in words. It is
// the ONE rule behind both the admission guard and the check on a seat that is already loaded
// (ResidentVerdict), so the two cannot disagree about what "away" is: present never, away always
// (the operator's unconditional override, no idle, lock or fullscreen test), auto only on a known
// reading that is locked or idle, and an unknown reading is not away.
func PresenceAllows(p Presence) (bool, string) {
	switch p.Mode {
	case "present":
		return false, "operator_presence is present (the default; set auto or away to open the display card) — refused"
	case "away":
		return true, "operator override: away"
	case "auto":
		if !p.Known {
			return false, "presence unknown (probe failed or no console session) — refused: " + p.Note
		}
		if p.Locked {
			return true, "console session locked"
		}
		if !p.Away {
			return false, fmt.Sprintf("operator at the desk (%s) — refused", p.Note)
		}
		return true, fmt.Sprintf("idle %d s (%s)", p.IdleSec, p.Note)
	}
	return false, fmt.Sprintf("presence mode %q unknown — refused", p.Mode)
}

// isCUDAIndex mirrors config's: a display pin that is all digits is an index,
// anything else a UUID prefix that needs the probe to resolve.
func isCUDAIndex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
