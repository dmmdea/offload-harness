package gpulease

// The card allocator (plan P3): which cards a reservation that asked for "N cards"
// (rather than naming them) should take. It is a pure function over an input the CLI
// assembles, so it imports neither placement nor config, and every term of the rule has
// a test.
//
// A card is ALLOCATABLE when it is: not quarantined, not a display card whose cap is closed
// (AllocInput.DisplayCapPct 0) and not one the reading could not place, not claimed by a live
// lease (a whole-node lease claims every card), not busy under a foreign compute process, and
// its free VRAM fits the footprint; and the HOST's memory admits the RAM the job declares
// (gpuprobe.HostRAMAdmits, the rule the lease grant itself applies, hostram.go).
// The display card is open to the allocator at all times, up to a share of its total VRAM
// (A-RAM-15, the operator's 60 percent rule): the VRAM in use now, the desktop's included, plus
// the job's declared footprint must stay at or under DisplayCapPct of the card (fitsCard).
// Nothing about the operator's presence enters: 0 percent is the "I need the card" switch.
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

// The words of the two display skips, one place for the allocator's pick and the grant-time check.
const (
	displayClosedDetail  = "the operator's screen; gpu_display_cap_pct is 0, so it is not auto-assigned"
	displayUnknownDetail = "this reading could not say which card the monitor is on; no card is auto-assigned until one can"
)

// capEpsilonGiB absorbs the float error of a share of a total (15.9 x 40 / 100 is not exactly 6.36), so a
// job that lands exactly on the cap is not refused by the last bit of a division.
const capEpsilonGiB = 1e-6

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
	// DisplayCapPct is how much of a display card's total VRAM jobs may fill, the desktop's own
	// use included (config gpu_display_cap_pct, default 60, A-RAM-15): the card is allocatable only
	// while the VRAM in use now plus the job's declared footprint is at most this share of it,
	// which is the display layer's display_floor arithmetic (free - footprint >= floor) with the
	// floor set to the rest of the card. 0 = closed, the operator's "I need the card" switch, and
	// the zero value of the field. 100 = the whole card. It applies to the display card only; every
	// other card is judged by the footprint alone. A card whose reading could not say where the
	// monitor is (DisplayUnknown) stays closed whatever this says.
	DisplayCapPct int
	// DisplayFloorGiB is a floor the display layer declares in absolute terms (display_floor_gib,
	// internal/placement/guards.go) and keeps on the display card on top of the cap: the card keeps
	// the LARGER of this and the cap's remainder, so a stricter declaration still holds and the
	// two doors onto the card agree. 0 = none declared.
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
		case c.Display && !in.displayOpen():
			skip(c, id, ReasonDisplay, displayClosedDetail)
		case c.DisplayUnknown:
			skip(c, id, ReasonDisplayUnknown, displayUnknownDetail)
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

// displayOpen: the cap lets the allocator consider the display card at all.
func (in AllocInput) displayOpen() bool { return in.DisplayCapPct > 0 }

// floorApplies: the display card's cap is in force on this card. It is the display card's alone (a pair
// card is judged by the footprint, as it always was), and only while the cap is open and holds something
// back: at 100 percent with no declared floor the whole card is the job's and nothing is checked. A card
// of a reading that could not say which card the monitor is on (DisplayUnknown) never gets here: it is
// closed before the cap is asked.
func (in AllocInput) floorApplies(c gpuprobe.Card) bool {
	return c.Display && in.displayOpen() && (in.DisplayCapPct < 100 || in.DisplayFloorGiB > 0)
}

// displayFloorGiB is the VRAM that must still be free on the display card c after the job loads: the rest of
// the card past the cap, (100 - cap) percent of its total, or the display layer's declared absolute floor,
// whichever is larger. ok is false when the cap needs a total the reading did not give.
func (in AllocInput) displayFloorGiB(c gpuprobe.Card) (floor float64, ok bool) {
	floor = in.DisplayFloorGiB
	if in.DisplayCapPct >= 100 {
		return floor, true
	}
	if c.VRAMTotalGiB <= 0 {
		return 0, false
	}
	if rest := c.VRAMTotalGiB * float64(100-in.DisplayCapPct) / 100; rest > floor {
		floor = rest
	}
	return floor, true
}

// fitsCard is fits plus the display card's cap. The VRAM in use now (the desktop's included) plus the footprint
// must stay at or under DisplayCapPct of the card, which is the display layer's own display_floor arithmetic
// (free - footprint >= floor) with the floor set to the rest of the card (displayFloorGiB). A footprint left
// undeclared cannot be shown to stay under anything, so on the display card it refuses, as the layer's guard
// refuses a seat with no display footprint "before any arithmetic": judging it as free >= floor would let a
// 10 GiB render onto a card with 12 free, to leave the desktop 2. Nothing unloads a lease's work once it is
// placed, so the refusal has to come before the claim.
func (in AllocInput) fitsCard(c gpuprobe.Card) bool {
	if !fits(c, in.FootprintGiB) {
		return false
	}
	if !in.floorApplies(c) {
		return true
	}
	floor, ok := in.displayFloorGiB(c)
	return ok && in.FootprintGiB > 0 && c.VRAMFreeGiB-in.FootprintGiB >= floor-capEpsilonGiB
}

// DesktopRefusals is the desktop rule alone, put again to cards a request already chose: of ids, the
// cards that rule refuses on this reading. A queued request is chosen at enqueue, against the display
// card's cap and the VRAM in use as they were then; when its turn comes the caller builds a fresh
// AllocInput and asks this before the claim (Options.GrantCheck), so a card the allocator would no
// longer open is not handed over because it was open once.
//
// It judges the desktop's reasons only, which are the ones that change while a request waits: the
// display card once the operator has closed it (cap 0), a card that may be the display card on a
// reading that could not say, and the display card past its cap because the desktop or a game has
// grown on it. Other skips are not repeated here: a card another lease has taken since is the claim's
// to refuse (ErrHeld), and the rest were never a reason the FIFO position could be lost over. Each
// refusal is a Skip with the allocator's own reason and detail.
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
		case c.Display && !in.displayOpen():
			out = append(out, Skip{ID: id, Index: c.NvidiaIndex, Reason: ReasonDisplay, Detail: displayClosedDetail})
		case c.DisplayUnknown:
			out = append(out, Skip{ID: id, Index: c.NvidiaIndex, Reason: ReasonDisplayUnknown, Detail: displayUnknownDetail})
		case in.floorApplies(c) && !in.fitsCard(c):
			out = append(out, Skip{ID: id, Index: c.NvidiaIndex, Reason: ReasonVRAM, Detail: in.vramDetail(c)})
		}
	}
	return out
}

// vramDetail says why a card that does not fit was skipped, naming the display card's cap where that is
// what refused it.
func (in AllocInput) vramDetail(c gpuprobe.Card) string {
	detail := fmt.Sprintf("%.1f GiB free, %.1f GiB needed", c.VRAMFreeGiB, in.FootprintGiB)
	if !in.floorApplies(c) {
		return detail
	}
	floor, ok := in.displayFloorGiB(c)
	if !ok {
		return fmt.Sprintf("%.1f GiB free on the display card, whose total VRAM this reading did not give, so its gpu_display_cap_pct of %d %% cannot be applied", c.VRAMFreeGiB, in.DisplayCapPct)
	}
	cap := fmt.Sprintf("the cap is gpu_display_cap_pct %d %% of %.1f GiB (the desktop and jobs together)", in.DisplayCapPct, c.VRAMTotalGiB)
	switch {
	case in.DisplayCapPct >= 100:
		cap = fmt.Sprintf("the declared display_floor_gib is %.1f GiB and gpu_display_cap_pct is %d %%", in.DisplayFloorGiB, in.DisplayCapPct)
	case in.DisplayFloorGiB > floor-capEpsilonGiB:
		cap = fmt.Sprintf("the declared display_floor_gib of %.1f GiB binds, stricter than the cap of gpu_display_cap_pct %d %% of %.1f GiB", in.DisplayFloorGiB, in.DisplayCapPct, c.VRAMTotalGiB)
	}
	if in.FootprintGiB <= 0 {
		return fmt.Sprintf("%.1f GiB free on the display card, whose desktop floor is %.1f GiB (%s): a job that declares no footprint (--vram) cannot be shown to stay under the cap", c.VRAMFreeGiB, floor, cap)
	}
	return detail + fmt.Sprintf(" on the display card, which leaves %.1f GiB, under its desktop floor of %.1f GiB (%s)", c.VRAMFreeGiB-in.FootprintGiB, floor, cap)
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
