package gpulease

// The card allocator (plan P3): which cards a reservation that asked for "N cards"
// (rather than naming them) should take. It is a pure function over an input the CLI
// assembles, so it imports neither placement nor config, and every term of the rule has
// a test.
//
// A card is ALLOCATABLE when it is: not quarantined, not a display card (I6, unless the
// caller says the operator is away), not claimed by a live lease (a whole-node lease
// claims every card), not busy under a foreign compute process, and its free VRAM fits
// the footprint; and the HOST's memory admits the RAM the job declares (gpuprobe.HostRAMAdmits,
// the rule the lease grant itself applies, hostram.go).
// A display card the caller has opened keeps the desktop floor on top of that: its free
// VRAM less the footprint must still leave AllocInput.DisplayFloorGiB (fitsCard).
// Order among allocatable cards: no resident seat first, then the cheapest eviction, then
// the lowest id.
//
// Naming cards (`--devices`) bypasses the CARD choice: an explicit word is the operator's,
// queued FIFO behind whoever holds those cards, and never second-guessed here. It does not
// bypass the host-RAM rule: that one lives in the grant (Manager.hostRAMRefusal), so a lease
// that names its cards declares its RAM need and is admitted against it like any other. Until
// 2026-10-09 the host term lived only here, which is how two lanes that stream weights from
// RAM ran at once under two `--devices` leases and committed 162.9 GiB on a 127.7 GiB box.
//
// A card with a foreign non-seat process is reported `foreign-busy` and skipped, never
// killed. On Windows (WDDM) nvidia-smi lists no per-process rows for compute apps, so the
// foreign-busy set the CLI passes in is empty there: it is Linux-only evidence today.

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// Skip reasons. A caller branches on these, so they are constants.
const (
	ReasonQuarantined = "quarantined"
	ReasonDisplay     = "display"
	// ReasonDisplayUnknown: the reading could not say which card the monitor is on
	// (gpuprobe.Card.DisplayUnknown), so no card is vouched for.
	ReasonDisplayUnknown = "display-unknown"
	ReasonClaimed        = "claimed"
	ReasonForeignBusy    = "foreign-busy"
	ReasonVRAM           = "vram"
)

// ResidentInfo is what is loaded on one card that taking it would evict.
type ResidentInfo struct {
	Seats   []string
	CostGiB float64 // estimated cost of evicting them (their footprint)
}

// AllocInput is everything the rule reads.
type AllocInput struct {
	Cards []gpuprobe.Card
	// Claimed holds lease ids of cards a live lease claims. WholeNodeHeld says a
	// whole-node lease is live, which claims every card.
	Claimed       map[string]bool
	WholeNodeHeld bool
	Quarantined   map[string]bool
	// ForeignBusy maps a lease id to a short description of the foreign process on it.
	ForeignBusy map[string]string
	// Resident maps a lease id to the seats loaded on that card.
	Resident map[string]ResidentInfo
	// AllowDisplay lets the display card be auto-assigned. The caller sets it only when
	// the presence guard says the operator is away.
	AllowDisplay bool
	// DisplayFloorGiB is the VRAM the desktop keeps on a display card that AllowDisplay has
	// opened: such a card is allocatable only while its free VRAM, less the job's footprint,
	// still leaves this much (the display layer's display_floor arithmetic,
	// internal/placement/guards.go, so the two doors onto the card agree). 0 = no floor
	// declared, the behaviour before this field existed. It applies to the display card
	// only; every other card is judged by the footprint alone.
	DisplayFloorGiB float64
	// FootprintGiB is the VRAM the job needs free on EACH card (0 = not declared).
	FootprintGiB float64
	// Host RAM term, the one rule the lease grant applies (gpuprobe.HostRAMAdmits): the host's memory
	// now, the job's declared need, the part of the leases ALREADY GRANTED that has not loaded yet
	// (Manager.HostRAMPending) and the headroom to keep. Here it is advisory (it keeps the allocator
	// from picking cards a grant would then refuse); the grant, under the epoch lock, is the authority.
	HostMem                                      gpuprobe.HostMemory
	HostMemOK                                    bool
	HostNeedGiB, HostPendingGiB, HostHeadroomGiB float64
	// Min and Max bound how many cards to take (1 <= Min <= Max).
	Min, Max int
}

// Skip is one card the allocator did not take, and why.
type Skip struct {
	ID     string `json:"id"`
	Index  int    `json:"index"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// Allocation is the cards taken (lease ids, in the order chosen) and what was skipped.
type Allocation struct {
	Devices []string
	Skipped []Skip
}

// NoCardsError says no allocation of at least Min cards exists right now. It carries the
// reasons, so a caller can print them and decide whether waiting can help.
type NoCardsError struct {
	Want    int
	Have    int
	Skipped []Skip
	// HostReason is set when the HOST (RAM headroom), not the cards, is what is short.
	HostReason string
	// HostImpossible: the host's memory could never admit the declared need however long a caller
	// waits (HostReason says so). Waiting does not help, so a caller that would poll returns instead.
	HostImpossible bool
	// Waitable lists, in allocation order, the cards a queued request can be given: the
	// cards that are free right now first, then the cards skipped ONLY because a live
	// lease claims them (and that would otherwise fit). A caller that must wait queues
	// FIFO on the first Min of them, so an idle card is held as part of the set instead
	// of being thrown away while the request waits for busy ones. A card skipped for
	// any other reason (display, quarantine, foreign process, VRAM) is never listed:
	// waiting does not fix it.
	Waitable []string
}

func (e *NoCardsError) Error() string {
	var parts []string
	if e.HostReason != "" {
		parts = append(parts, e.HostReason)
	}
	for _, s := range e.Skipped {
		d := s.Reason
		if s.Detail != "" {
			d += " (" + s.Detail + ")"
		}
		parts = append(parts, fmt.Sprintf("card %d: %s", s.Index, d))
	}
	return fmt.Sprintf("gpulease: %d allocatable card(s), %d wanted: %s", e.Have, e.Want, strings.Join(parts, "; "))
}

// Allocate picks the cards. A request that is itself wrong (Min < 1, Max < Min) is a
// plain error; a request that cannot be met right now is a *NoCardsError.
func Allocate(in AllocInput) (Allocation, error) {
	if in.Min < 1 || in.Max < in.Min {
		return Allocation{}, fmt.Errorf("gpulease: a card count of %d..%d is not a request (need 1 <= min <= max)", in.Min, in.Max)
	}
	type cand struct {
		card gpuprobe.Card
		id   string
		res  ResidentInfo
	}
	var ok, claimedFit []cand
	var skipped []Skip
	skip := func(c gpuprobe.Card, id, reason, detail string) {
		skipped = append(skipped, Skip{ID: id, Index: c.NvidiaIndex, Reason: reason, Detail: detail})
	}
	for _, c := range in.Cards {
		id := c.LeaseID()
		res := in.Resident[id]
		switch {
		case in.Quarantined[id]:
			skip(c, id, ReasonQuarantined, "")
		case c.Display && !in.AllowDisplay:
			skip(c, id, ReasonDisplay, "the operator's screen; never auto-assigned while they are at the desk")
		case c.DisplayUnknown && !in.AllowDisplay:
			skip(c, id, ReasonDisplayUnknown, "this reading could not say which card the monitor is on; no card is auto-assigned until one can")
		case in.WholeNodeHeld || in.Claimed[id]:
			detail := "held by a live lease"
			if in.WholeNodeHeld {
				detail = "a whole-node lease is live"
			}
			skip(c, id, ReasonClaimed, detail)
			if in.ForeignBusy[id] == "" && in.fitsCard(c) {
				claimedFit = append(claimedFit, cand{c, id, res})
			}
		case in.ForeignBusy[id] != "":
			skip(c, id, ReasonForeignBusy, in.ForeignBusy[id])
		case !in.fitsCard(c):
			skip(c, id, ReasonVRAM, in.vramDetail(c))
		default:
			ok = append(ok, cand{c, id, res})
		}
	}
	less := func(a, b cand) bool {
		ar, br := len(a.res.Seats) > 0, len(b.res.Seats) > 0
		if ar != br {
			return !ar
		}
		if a.res.CostGiB != b.res.CostGiB {
			return a.res.CostGiB < b.res.CostGiB
		}
		return a.id < b.id
	}
	sort.SliceStable(ok, func(i, j int) bool { return less(ok[i], ok[j]) })
	sort.SliceStable(claimedFit, func(i, j int) bool { return less(claimedFit[i], claimedFit[j]) })
	sort.SliceStable(skipped, func(i, j int) bool { return skipped[i].Index < skipped[j].Index })

	host := gpuprobe.HostRAMAdmits(in.HostMem, in.HostMemOK, in.HostNeedGiB, in.HostPendingGiB, in.HostHeadroomGiB)
	if !host.OK || len(ok) < in.Min {
		err := &NoCardsError{Want: in.Min, Have: len(ok), Skipped: skipped}
		if !host.OK {
			err.HostReason = host.Why
			err.HostImpossible = host.Impossible
			err.Have = 0
		}
		for _, c := range ok {
			err.Waitable = append(err.Waitable, c.id)
		}
		for _, c := range claimedFit {
			err.Waitable = append(err.Waitable, c.id)
		}
		return Allocation{}, err
	}
	n := len(ok)
	if n > in.Max {
		n = in.Max
	}
	out := Allocation{Skipped: skipped}
	for _, c := range ok[:n] {
		out.Devices = append(out.Devices, c.id)
	}
	return out, nil
}

// fits: the card has the free VRAM the job needs (always true when none is declared).
func fits(c gpuprobe.Card, needGiB float64) bool {
	return needGiB <= 0 || c.VRAMFreeGiB >= needGiB
}

// floorApplies: the desktop floor is in force on this card. It is the display card's alone (a pair
// card is judged by the footprint, as it always was), and only where a floor was declared. A card
// of a reading that could not say which card the monitor is on (DisplayUnknown, set on every card
// of a degraded reading, none of them Display) counts as the display card: "unknown" is never "not
// the monitor", so an operator-away request that AllowDisplay lets through still has to leave the
// desktop its floor on whichever card the screen turns out to be on.
func (in AllocInput) floorApplies(c gpuprobe.Card) bool {
	return (c.Display || c.DisplayUnknown) && in.DisplayFloorGiB > 0
}

// fitsCard is fits plus the desktop floor. A display card that AllowDisplay has opened is still the
// desktop's: the job must leave DisplayFloorGiB free after it loads, which is the display layer's
// own display_floor arithmetic (free − footprint ≥ floor). A footprint left undeclared cannot be
// shown to leave anything, so on the display card it refuses, as the layer's guard refuses a seat
// with no display footprint "before any arithmetic": judging it as free ≥ floor would let a 10 GiB
// render onto a card with 12 free, to leave the desktop 2. Nothing unloads a lease's work once it
// is placed, so the refusal has to come before the claim.
func (in AllocInput) fitsCard(c gpuprobe.Card) bool {
	if !fits(c, in.FootprintGiB) {
		return false
	}
	if !in.floorApplies(c) {
		return true
	}
	return in.FootprintGiB > 0 && c.VRAMFreeGiB-in.FootprintGiB >= in.DisplayFloorGiB
}

// DesktopRefusals is the desktop rule alone, put again to cards a request already chose: of ids, the
// cards that rule refuses on this reading. A queued request is chosen at enqueue, against the
// operator's presence and the floor as they were then; when its turn comes the caller builds a fresh
// AllocInput and asks this before the claim (Options.GrantCheck), so a card the allocator would no
// longer open is not handed over because it was open once.
//
// It judges the desktop's reasons only, which are the ones that change while a request waits: the
// display card (or a card that may be, on a reading that could not say) while the operator is at the
// desk, and the display card under its floor. Other skips are not repeated here: a card another lease
// has taken since is the claim's to refuse (ErrHeld), and the rest were never a reason the FIFO
// position could be lost over. Each refusal is a Skip with the allocator's own reason and detail.
func DesktopRefusals(in AllocInput, ids []string) []Skip {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var out []Skip
	for _, c := range in.Cards {
		id := c.LeaseID()
		if !want[id] || in.WholeNodeHeld || in.Claimed[id] {
			continue
		}
		switch {
		case c.Display && !in.AllowDisplay:
			out = append(out, Skip{ID: id, Index: c.NvidiaIndex, Reason: ReasonDisplay, Detail: "the operator's screen; never auto-assigned while they are at the desk"})
		case c.DisplayUnknown && !in.AllowDisplay:
			out = append(out, Skip{ID: id, Index: c.NvidiaIndex, Reason: ReasonDisplayUnknown, Detail: "this reading could not say which card the monitor is on; no card is auto-assigned until one can"})
		case in.floorApplies(c) && !in.fitsCard(c):
			out = append(out, Skip{ID: id, Index: c.NvidiaIndex, Reason: ReasonVRAM, Detail: in.vramDetail(c)})
		}
	}
	return out
}

// vramDetail says why a card that does not fit was skipped, naming the desktop floor where that is
// what refused it.
func (in AllocInput) vramDetail(c gpuprobe.Card) string {
	detail := fmt.Sprintf("%.1f GiB free, %.1f GiB needed", c.VRAMFreeGiB, in.FootprintGiB)
	if !in.floorApplies(c) {
		return detail
	}
	which := "the display card"
	if !c.Display {
		which = "a card that may be the display card (the reading could not say which is)"
	}
	if in.FootprintGiB <= 0 {
		return fmt.Sprintf("%.1f GiB free on %s, whose desktop floor is %.1f GiB: a job that declares no footprint (--vram) cannot be shown to leave it", c.VRAMFreeGiB, which, in.DisplayFloorGiB)
	}
	return detail + fmt.Sprintf(" on %s, which leaves %.1f GiB, under the desktop floor of %.1f GiB", which, c.VRAMFreeGiB-in.FootprintGiB, in.DisplayFloorGiB)
}

// QuarantinedCards reads the `quarantine.<id>` sidecars beside the lease record: cards
// whose previous holder's process tree could not be stopped, which the allocator never
// hands out. Plan P12 writes them and clears them as observation shows the tree gone;
// until then the sidecars are only ever read.
func (m *Manager) QuarantinedCards() map[string]bool {
	out := map[string]bool{}
	entries, err := os.ReadDir(m.leaseDir())
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		id, ok := strings.CutPrefix(e.Name(), "quarantine.")
		if !ok {
			continue
		}
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
			out[id] = true
		}
	}
	return out
}
