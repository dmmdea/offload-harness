package gpulease

// The card allocator (plan P3): which cards a reservation that asked for "N cards"
// (rather than naming them) should take. It is a pure function over an input the CLI
// assembles, so it imports neither placement nor config, and every term of the rule has
// a test.
//
// A card is ALLOCATABLE when it is: not quarantined, not a display card (I6, unless the
// caller says the operator is away), not claimed by a live lease (a whole-node lease
// claims every card), not busy under a foreign compute process, and its free VRAM fits
// the footprint; and the HOST has the RAM the job declares plus the configured headroom.
// Order among allocatable cards: no resident seat first, then the cheapest eviction, then
// the lowest id.
//
// Naming cards (`--devices`) bypasses this: an explicit word is the operator's, queued
// FIFO behind whoever holds those cards, and never second-guessed here.
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
	// FootprintGiB is the VRAM the job needs free on EACH card (0 = not declared).
	FootprintGiB float64
	// Host RAM term: free RAM now, the job's declared need and the headroom to keep.
	HostFreeGiB, HostNeedGiB, HostHeadroomGiB float64
	HostFreeOK                                bool
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
			if in.ForeignBusy[id] == "" && fits(c, in.FootprintGiB) {
				claimedFit = append(claimedFit, cand{c, id, res})
			}
		case in.ForeignBusy[id] != "":
			skip(c, id, ReasonForeignBusy, in.ForeignBusy[id])
		case !fits(c, in.FootprintGiB):
			skip(c, id, ReasonVRAM, fmt.Sprintf("%.1f GiB free, %.1f GiB needed", c.VRAMFreeGiB, in.FootprintGiB))
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

	hostOK, hostWhy := gpuprobe.RAMHeadroom(in.HostFreeGiB, in.HostFreeOK, in.HostNeedGiB, in.HostHeadroomGiB)
	if !hostOK || len(ok) < in.Min {
		err := &NoCardsError{Want: in.Min, Have: len(ok), Skipped: skipped}
		if !hostOK {
			err.HostReason = hostWhy
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
