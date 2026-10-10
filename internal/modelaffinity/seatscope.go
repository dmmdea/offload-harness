// seatscope.go — which leases matter to WHICH seat (plan P4, register C-86).
//
// THE DEFECT. The gate asked "is a lease held", so a render on card 2 made every text load
// on the box wait, cards 0 and 1 included. Now it asks the narrower question: does a held
// lease sit on a card THIS seat is pinned to. The seat's pin is declared in the box's
// layers (config: LayerSeat.Device, PCI-order indices, the same pin the seat is launched
// with), so it is ARMED from config.Load (SetSeatPins) rather than imported: config
// imports this package, and a second resolution order is how a gate and a launcher come to
// disagree.
//
// THE DIRECTION OF EVERY DOUBT IS "FENCE". A model nobody declared a pin for, a pin the card
// table cannot place, a card table that cannot be read, a whole-node lease: each reads as
// every card, which is the answer this gate gave before card-scoped leases existed (plan
// invariant I7). Narrowing happens only on a positive resolution.
//
// COST. The card table is an nvidia-smi exec, so it is read only when a held lease actually
// names cards (a whole-node lease or an idle box never need it) and memoised for
// cardTableTTL; the unfenced path still pays one ReadFile, as before.
package modelaffinity

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// cardTableTTL is how long one card-table reading serves the polls of a blocked admission
// (leasePollInterval is 1 s, and a scoped lease that does not touch the seat is the
// common case this memo exists for).
const cardTableTTL = 2 * time.Second

// cardTableTimeout bounds the one nvidia-smi exec: an admission must not wait behind a
// wedged driver.
const cardTableTimeout = 5 * time.Second

var (
	seatMu   sync.RWMutex
	seatPins func(model string) ([]string, bool)
	// cardOrder is config gpu_comfy_order: how the card table learns ComfyUI's device order.
	cardOrder string

	// readCardTable is the card-table reader; a test swaps it.
	readCardTable = gpuprobe.ReadCards

	cardMemoMu sync.Mutex
	cardMemoAt time.Time
	cardMemo   []gpuprobe.Card
	cardMemoOK bool
	cardNow    = time.Now
)

// SetSeatPins arms the seat-to-cards map: for a model name it returns the device pins of
// the layer seat(s) that serve it (PCI-order indices or UUID prefixes) and whether the
// model is declared at all. config.Load arms it, beside SetGPULease. nil disarms it, which
// reads every model as unknown: every lease fences every seat.
func SetSeatPins(fn func(model string) ([]string, bool)) {
	seatMu.Lock()
	seatPins = fn
	seatMu.Unlock()
}

// SetCardOrder arms config gpu_comfy_order for the card-table reads this package makes.
func SetCardOrder(order string) {
	seatMu.Lock()
	cardOrder = order
	seatMu.Unlock()
	resetCardMemo()
}

func resetCardMemo() {
	cardMemoMu.Lock()
	cardMemoAt, cardMemo, cardMemoOK = time.Time{}, nil, false
	cardMemoMu.Unlock()
}

// pinsOf reports the declared device pins of a model; ok is false for an undeclared one.
func pinsOf(model string) ([]string, bool) {
	seatMu.RLock()
	fn := seatPins
	seatMu.RUnlock()
	if fn == nil {
		return nil, false
	}
	pins, ok := fn(model)
	if !ok || len(pins) == 0 {
		return nil, false
	}
	return pins, true
}

// PinsFor reports the device pins the gate has armed for model (PCI-order indices or UUID
// prefixes); ok is false for a model nobody declared, which the gate reads as every card.
func PinsFor(model string) ([]string, bool) { return pinsOf(model) }

// cardTable is the memoised card table; ok is false when it cannot be read.
func cardTable() ([]gpuprobe.Card, bool) {
	cardMemoMu.Lock()
	defer cardMemoMu.Unlock()
	if !cardMemoAt.IsZero() && cardNow().Sub(cardMemoAt) < cardTableTTL {
		return cardMemo, cardMemoOK
	}
	seatMu.RLock()
	order := cardOrder
	seatMu.RUnlock()
	ctx, cancel := context.WithTimeout(context.Background(), cardTableTimeout)
	defer cancel()
	cards, _, err := readCardTable(ctx, order)
	cardMemoAt = cardNow()
	cardMemo, cardMemoOK = cards, err == nil && len(cards) > 0
	return cardMemo, cardMemoOK
}

// namesCards reports whether any live lease in info sits on a bounded card set. Only then
// does a seat's pin need resolving: a whole-node lease touches every seat whatever its pin.
func namesCards(info gpulease.Info) bool {
	for _, l := range info.Each() {
		if l.Held && len(l.EffectiveDevices()) > 0 {
			return true
		}
	}
	return false
}

// ScopeToPins narrows info to the leases that sit on the cards pins name (PCI-order
// indices or UUID prefixes, a layer seat's Device split by DeviceList). No pins, an
// unresolvable pin or an unreadable card table returns info unchanged: unknown is every card.
func ScopeToPins(info gpulease.Info, pins []string) gpulease.Info {
	if len(pins) == 0 || !info.Held || !namesCards(info) {
		return info
	}
	cards, ok := cardTable()
	if !ok {
		return info
	}
	ids, ok := gpulease.ResolvePins(pins, cards)
	if !ok {
		return info
	}
	return info.For(ids)
}

// ScopeToModel is ScopeToPins for the seat serving model.
func ScopeToModel(info gpulease.Info, model string) gpulease.Info {
	pins, ok := pinsOf(model)
	if !ok {
		return info
	}
	return ScopeToPins(info, pins)
}

// SeatCards names the cards (lease ids) the seat serving model sits on, resolved by the same
// pins and the same card table ScopeToModel narrows a lease with, so the cards a blocked
// admission WAITS on are the cards it was blocked on. nil means "cannot be said": an undeclared
// model, a pin the card table cannot place, a card table that cannot be read. The caller reads
// that as the whole node, the direction of every doubt in this file. It reads the card table (an
// nvidia-smi exec, memoised) exactly as ScopeToPins does, so the caller asks it only under the
// condition ScopeToPins reads it under: a held lease that names cards (namesCards). It is asked
// once, on the blocked path, to register the admission's place in line, never on the unfenced
// fast path and never for a whole-node lease, which needs no card table.
func SeatCards(model string) []string {
	pins, ok := pinsOf(model)
	if !ok {
		return nil
	}
	cards, ok := cardTable()
	if !ok {
		return nil
	}
	ids, ok := gpulease.ResolvePins(pins, cards)
	if !ok {
		return nil
	}
	return ids
}

// SeatLease reads the armed lease directory and returns what it holds against model's
// cards: the leases that sit on them, and nothing else. The zero Info when the gate is not
// armed (no config.Load ran in this process) or nothing relevant is held.
func SeatLease(model string) gpulease.Info {
	dir := gpuLeaseDir()
	if dir == "" {
		return gpulease.Info{}
	}
	return ScopeToModel(InspectLease(dir), model)
}

// SetCardTableReader replaces the card-table reader (nvidia-smi by default) and returns a
// function that puts the previous one back. A fleet node that already holds a background
// VRAM sample, and a test with a synthetic box, install their own.
func SetCardTableReader(fn func(ctx context.Context, comfyOrder string) ([]gpuprobe.Card, string, error)) (restore func()) {
	cardMemoMu.Lock()
	prev := readCardTable
	if fn == nil {
		fn = gpuprobe.ReadCards
	}
	readCardTable = fn
	cardMemoMu.Unlock()
	resetCardMemo()
	return func() {
		cardMemoMu.Lock()
		readCardTable = prev
		cardMemoMu.Unlock()
		resetCardMemo()
	}
}

// InsideLease reports whether this process runs under the very lease info describes (see
// insideLease): the epoch comparison every consumer of a lease must use for its own holder.
func InsideLease(info gpulease.Info) bool { return insideLease(info) }

// CardsHeld reports whether a lease this process does not hold sits on any card the pins
// name, and says which. It reads the armed lease directory, so the unarmed gate (no
// config.Load) holds nothing, and it is the placement table's reader of "is the home seat's
// card taken". No pins is an unknown seat: every card, so any live foreign lease holds it. It
// is an inspection (PeekLease): it writes nothing.
func CardsHeld(pins []string) (held bool, why string) {
	dir := gpuLeaseDir()
	if dir == "" {
		return false, ""
	}
	for _, l := range ScopeToPins(PeekLease(dir), pins).Each() {
		if !l.Held || insideLease(l) {
			continue
		}
		return true, describeLease(l)
	}
	return false, ""
}

// describeLease names a lease for a placement reason: its class, epoch and cards.
func describeLease(l gpulease.Info) string {
	cards := "the whole node"
	if eff := l.EffectiveDevices(); len(eff) > 0 {
		cards = "cards " + strings.Join(eff, ",")
	}
	return fmt.Sprintf("%s lease epoch %d on %s", l.Class, l.Epoch, cards)
}

// BlocksLoadFor is blocksLoad for a seat on the cards pins name.
func BlocksLoadFor(info gpulease.Info, pins []string) bool {
	return blocksLoad(ScopeToPins(info, pins))
}

// BlocksNewRunFor is BlocksNewRun for a seat on the cards pins name.
func BlocksNewRunFor(info gpulease.Info, pins []string) bool {
	return BlocksNewRun(ScopeToPins(info, pins))
}
