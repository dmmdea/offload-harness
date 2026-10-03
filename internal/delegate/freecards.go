package delegate

import (
	"strings"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	placetable "github.com/dmmdea/offload-harness/internal/placement"
)

// Free cards (GPU routing P1).
//
// A node's health has always carried gpu_devices[], one row per card, and the
// delegator dropped it: placement ranked nodes on ONE utilisation scalar (the
// busiest card), so a box with one busy card and two idle ones read as busier
// than a box with three cards at 30 % each, and work went to the box with no
// room. This file reads the rows instead.
//
// What it does NOT do is gate. A free-card reading is a RANKING input over a
// snapshot that is stale by construction (the same posture as saturated()):
// it orders eligible nodes, it never makes one ineligible, and a node with no
// free card still takes work, which queues on the node (a busy card is a place
// in line, never a refusal).
//
// Index spaces. A seat pin (LayerSeat.Device, "0,2") is a CUDA index and a
// gpu_devices[].index is nvidia-smi's PCI order; on a board whose CUDA order
// is fastest-first they can differ. So nothing here matches a pin to a device
// by index equality. A layer needs as many free cards as its seat spans, and
// every card is a candidate. That is conservative where the spaces could
// disagree and exact where they cannot.

const (
	// freeCardUtilBelowPct is the utilisation under which a card is not doing
	// work. It is the same line gpuactivity's utilBusyPct draws for "the cards
	// are busy under the lease" (15), so the delegator and `gpu status` agree on
	// what an idle card is.
	freeCardUtilBelowPct = 15

	// freeCardMinFreeFraction is the share of a card's VRAM that must be free
	// for it to count as a place to put a seat when the seat's own footprint is
	// not published. A quarter of a 16 GB card is 4 GiB, below the smallest
	// seat in the fleet's tier table, so a card under it is never a place to
	// load anything. It is a floor for the unknown case only: a published
	// footprint replaces it.
	freeCardMinFreeFraction = 0.25
)

// Card-room tiers, per node. Ordered so a higher tier is a better target.
const (
	tierNone    = 0 // the node published its cards and none can take this contract
	tierUnknown = 1 // the node published no usable per-card truth: neither credited nor blamed
	tierSome    = 2 // the node published its cards and one can take this contract
)

// cardRoom is a node's free-card reading for one contract shape.
type cardRoom struct {
	known bool // at least one card reported a utilisation
	free  int  // cards that are idle, not a display, and have the VRAM
	total int  // cards the node published
	need  int  // cards one seat of the layer spans (1 when no layer is named)
}

// cardIdle reports whether d is a card work could run on: not the operator's
// display, and utilisation known and below the working line (an unknown is never
// idle).
func cardIdle(d gpuprobe.Device) bool {
	return !d.DisplayActive && d.UtilKnown && d.UtilPct < freeCardUtilBelowPct
}

// cardFree is cardIdle plus at least needGiB of VRAM free.
func cardFree(d gpuprobe.Device, needGiB float64) bool {
	return cardIdle(d) && d.FreeGiB >= needGiB
}

// cardRoomFor reads v's cards for a contract that names layer ("" = none).
//
// The layer sharpens two numbers when the node declares it: how many cards one
// seat spans (a pair seat needs two free cards, not one) and how much VRAM each
// of them must have (the placement seat's published footprint, split across its
// cards). A layer the node does not declare, or declares without a footprint,
// falls back to the layer-agnostic reading: one card, with the VRAM floor.
//
// A WARM seat fills its own card. An idle card holding the seat the contract
// would run on has little VRAM free because of that seat, not because something
// else took it, and a node serving from a resident seat must not lose to a cold
// node for being warm. So when the seat is known loaded (the node's own
// seat_loaded, or the layer row's loaded), it vouches for the VRAM of as many
// idle cards as it spans: free = max(cards that fit, min(idle cards, span)).
// It vouches for VRAM only, never utilisation, and an unknown residency waives
// nothing.
func cardRoomFor(v NodeView, layer string) cardRoom {
	room := cardRoom{total: len(v.Devices), need: 1}
	if len(v.Devices) == 0 {
		return room
	}
	for _, d := range v.Devices {
		if d.UtilKnown {
			room.known = true
			break
		}
	}
	if !room.known {
		return room
	}
	var footprintPerCard float64
	resident := v.SeatLoaded != nil && *v.SeatLoaded
	if layer != "" {
		for _, row := range v.Layers {
			if row.Name != layer {
				continue
			}
			n, fp, loaded := layerSeatShape(row)
			if n > 0 {
				room.need = n
				footprintPerCard = fp
			}
			// The layer's own row is the evidence for its own seat; the node-level
			// seat_loaded describes the default agent seat, which may be another
			// layer's.
			resident = loaded
			break
		}
	} else {
		for _, row := range v.Layers {
			for _, s := range row.Seats {
				if s.Role == placetable.RoleAgent && s.Loaded {
					resident = true
				}
			}
		}
	}
	var idle, fits int
	for _, d := range v.Devices {
		need := footprintPerCard
		if need <= 0 {
			need = d.TotalGiB * freeCardMinFreeFraction
		}
		if cardIdle(d) {
			idle++
		}
		if cardFree(d, need) {
			fits++
		}
	}
	room.free = fits
	if resident {
		room.free = max(fits, min(idle, room.need))
	}
	return room
}

// layerSeatShape is the shape of the seat a layer places on: how many cards it
// spans, the VRAM each needs and whether the node says it is loaded. The
// placement seat is the agent seat where the layer has one (the agent lane is
// what the delegator is placing), else the largest seat the layer declares.
// Zeros when the row names no pin.
func layerSeatShape(row placetable.LayerRow) (cards int, perCardGiB float64, loaded bool) {
	var pick *placetable.SeatRow
	for i := range row.Seats {
		s := &row.Seats[i]
		if s.Role == placetable.RoleAgent {
			pick = s
			break
		}
		if pick == nil || s.FootprintGiB > pick.FootprintGiB {
			pick = s
		}
	}
	if pick == nil {
		return 0, 0, false
	}
	cards = 0
	for _, part := range strings.Split(pick.Device, ",") {
		if strings.TrimSpace(part) != "" {
			cards++
		}
	}
	if cards == 0 {
		return 0, 0, pick.Loaded
	}
	return cards, pick.FootprintGiB / float64(cards), pick.Loaded
}

// cardTier is v's tier for a contract naming layer, after the subtasks the
// deal in progress has already committed to its cards (v.cardsDealt, one seat
// of the layer each).
//
// Per node, never per pair: betterRemote folds a pairwise relation over a
// roster, and a relation that compared a node's cards against whichever node it
// was being compared with could form a cycle (the same lesson placementUtil
// records). A tier is a total preorder: some > unknown > none.
func cardTier(v NodeView, layer string) int {
	room := cardRoomFor(v, layer)
	if !room.known {
		return tierUnknown
	}
	if room.free-v.cardsDealt*room.need >= room.need {
		return tierSome
	}
	return tierNone
}

// FreeCards is the layer-agnostic count offload_status publishes: how many of
// the node's cards are idle, not a display and have room for a seat, out of how
// many it published. known is false when the node published no per-card
// utilisation, which a caller must read as "no figure", never as zero.
func (v NodeView) FreeCards() (free, total int, known bool) {
	room := cardRoomFor(v, "")
	if !room.known {
		return 0, room.total, false
	}
	return room.free, room.total, true
}

// layerOf is the layer a Subtask names, "" for none or a nil Subtask (the
// vision and text lanes rank with no contract).
func layerOf(st *Subtask) string {
	if st == nil {
		return ""
	}
	return st.Contract.Layer
}

// withCardsDealt is v as the deal ranks it after n subtasks have been given to
// its cards. It returns a copy; the stored view is never modified.
func (v NodeView) withCardsDealt(n int) NodeView {
	v.cardsDealt = n
	return v
}
