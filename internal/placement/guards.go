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
// nvidia-smi on the Qube's display card; the Qube did, and said so); a live
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
			pass, text = presence(live, rowVerdict)
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

func displayFloor(l config.LayerSpec, s config.LayerSeat, live Live, rowVerdict *bool) (bool, string) {
	if live.DeviceFree == nil {
		return verdictOr(rowVerdict, fmt.Sprintf("free VRAM on display device %s", l.DisplayDevice))
	}
	display := strings.TrimSpace(l.DisplayDevice)
	if display == "" {
		return false, "display_device undeclared — refused"
	}
	index := display
	if !isCUDAIndex(display) {
		if live.DeviceIndex == nil {
			return false, fmt.Sprintf("display device %s is a UUID pin and no reader can resolve it to a CUDA index — refused", display)
		}
		idx, ok := live.DeviceIndex(display)
		if !ok {
			return false, fmt.Sprintf("display device %s cannot be resolved to a CUDA index (not in the probe, or ambiguous) — refused", display)
		}
		index = idx
	}
	onDisplay := false
	for _, d := range s.DeviceList() {
		if d == index {
			onDisplay = true
		}
	}
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

func presence(live Live, rowVerdict *bool) (bool, string) {
	if live.Presence == nil {
		return verdictOr(rowVerdict, "operator presence")
	}
	p := live.Presence()
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
