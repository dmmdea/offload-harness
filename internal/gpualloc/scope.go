package gpualloc

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// LeaseScope says which seats sit on the cards a lease holds (plan P5, register C-86): a drain
// and an unload under a lease clear THOSE cards, not the node. A whole-node lease (no ids)
// reaches every seat. A seat whose pin is unknown, cannot be placed on a card, or is read while
// the card table cannot be, is on the leased cards: the direction of every doubt is the
// whole-node behaviour.
type LeaseScope struct {
	cfg     config.Config
	ids     []string
	cards   []gpuprobe.Card
	cardsOK bool
	// cardsWhy says why the table is unusable: the read's error, or that it listed no card.
	cardsWhy string
	// holdsAll: the lease names every card the table lists, so it holds the whole node however it is
	// spelled (a one-card box's only card, a lease on all three).
	holdsAll bool
}

// NewLeaseScope reads the card table once (deps.CardTable, which retries a read that ran out of
// time) for a lease holding ids (lease ids). A table it cannot read is said once on warn (nil =
// silent): Touches then reads every seat as on the leased cards (a drain waits, a fence holds) and
// Placement reads no seat as placed (an unload takes nothing it cannot place).
func NewLeaseScope(ctx context.Context, cfg config.Config, ids []string, deps Deps, warn io.Writer) LeaseScope {
	s := LeaseScope{cfg: cfg, ids: ids}
	if len(ids) == 0 {
		return s
	}
	cards, _, err := deps.CardTable(ctx, cfg)
	s.cards, s.cardsOK = cards, err == nil && len(cards) > 0
	if !s.cardsOK {
		s.cardsWhy = "no card was listed"
		if err != nil {
			s.cardsWhy = err.Error()
		}
		if warn != nil {
			// The drain keeps its direction (every doubt waits: a seat is treated as on the leased
			// cards); an unload takes the opposite one (only a seat placed on a leased card goes).
			// Said, because the operator would otherwise believe the card table had been read.
			fmt.Fprintf(warn, "gpu reserve: the card table could not be read (%s): the drain waits for every run on the node, and no seat but the configured agent seat can be placed on a card, so only that one is unloaded\n", s.cardsWhy)
		}
	}
	if s.cardsOK {
		held := map[string]bool{}
		for _, id := range ids {
			held[strings.ToLower(strings.TrimSpace(id))] = true
		}
		s.holdsAll = true
		for _, c := range cards {
			if !held[c.LeaseID()] {
				s.holdsAll = false
				break
			}
		}
	}
	return s
}

// Bounded reports whether the lease holds a set of cards (false = the whole node).
func (s LeaseScope) Bounded() bool { return len(s.ids) > 0 }

// TouchesPins reports whether a seat pinned to pins sits on the leased cards.
func (s LeaseScope) TouchesPins(pins []string) bool {
	if !s.Bounded() || !s.cardsOK {
		return true
	}
	rids, ok := gpulease.ResolvePins(pins, s.cards)
	if !ok {
		return true
	}
	return gpulease.Info{Held: true, Devices: s.ids}.Touches(rids)
}

// CardsReadable reports whether the card table was read: only then can a pin be placed on a card.
func (s LeaseScope) CardsReadable() bool { return !s.Bounded() || s.cardsOK }

// CardsWhy says why the card table could not be used ("" when it was).
func (s LeaseScope) CardsWhy() string { return s.cardsWhy }

// Placement says where the seat serving model sits, by POSITIVE knowledge only: known is true when
// the config declares the seat's pins and the card table places every one of them, and then on says
// whether any of those cards is a leased one. A model nobody declared, a pin the table cannot
// place and a table that cannot be read are known=false: the placement is not known, and on is
// false with it. A whole-node lease holds every card, so every seat is on it and known; so does a
// lease that names every card the table lists.
//
// This is the reading an UNLOAD takes, because an unload evicts a model for good (plan P5, F15).
// Touches reads the same facts in the other direction (a doubt is "on the leased cards"), which is
// right for a drain or a fence: waiting for a run that is not there costs a moment, and unloading a
// seat that is not there costs someone's work.
func (s LeaseScope) Placement(model string) (on, known bool) {
	if !s.Bounded() || s.holdsAll {
		return true, true
	}
	if !s.cardsOK {
		return false, false
	}
	pins, ok := s.cfg.ModelPins(model)
	if !ok {
		return false, false
	}
	rids, ok := gpulease.ResolvePins(pins, s.cards)
	if !ok {
		return false, false
	}
	return gpulease.Info{Held: true, Devices: s.ids}.Touches(rids), true
}

// Touches reports whether the seat serving model sits on the leased cards.
func (s LeaseScope) Touches(model string) bool {
	if !s.Bounded() {
		return true
	}
	pins, ok := s.cfg.ModelPins(model)
	if !ok {
		return true
	}
	return s.TouchesPins(pins)
}

// TouchesRun reports whether a registered run is on the leased cards: by the pins it recorded,
// else by the seat it runs on.
func (s LeaseScope) TouchesRun(r gpuactivity.Run) bool {
	if len(r.Devices) > 0 {
		return s.TouchesPins(r.Devices)
	}
	return s.Touches(r.Seat)
}

// SplitPlaced divides models for an unload: on are the ones positively placed on a leased card,
// off the ones positively placed elsewhere, unplaced the ones whose card is not known. Only on is
// unloaded; off and unplaced stay resident, and the caller names the unplaced.
func (s LeaseScope) SplitPlaced(models []string) (on, off, unplaced []string) {
	for _, m := range models {
		switch placed, known := s.Placement(m); {
		case !known:
			unplaced = append(unplaced, m)
		case placed:
			on = append(on, m)
		default:
			off = append(off, m)
		}
	}
	return on, off, unplaced
}

// EffectiveMemoryStack is the memory stack a lease never unloads: the config's, else the default.
func EffectiveMemoryStack(cfg config.Config) []string {
	if len(cfg.MemoryStack) == 0 {
		return config.Default().MemoryStack
	}
	return cfg.MemoryStack
}

// UnloadModels is the model-id list the render lane's freeLlamaSwap unloads under a lease that
// holds leaseIDs: the models that are POSITIVELY placed on a leased card (LeaseScope.Placement),
// minus the memory stack. A model placed on another card stays, and so does a model whose card is
// not known (nobody declared it, its pin cannot be placed, the card table cannot be read): "it could
// be anywhere" is a reason to say so, never a reason to unload it, because an unload evicts work the
// render may not need gone (register F15; the operator's rule that a render must not evict what it
// does not need to). warn, when not nil, names what was left resident and why, once.
//
// The candidates are the llama-swap roster. When the roster is empty or could not be read the
// candidates are the seats the config itself declares (Config.LayerSeatModels): the list is then
// the config's seats on the leased cards, said so, and never the unknown list the render lane reads
// as "every model off the memory stack".
//
// ok is false only for a whole-node lease (no ids): it holds every card, so nothing narrows and the
// render lane keeps its own rule. A card lease always has a list, empty when nothing is placed on
// its cards (exported as the none sentinel by UnloadEnv).
func UnloadModels(ctx context.Context, cfg config.Config, roster []string, leaseIDs []string, deps Deps, warn io.Writer) ([]string, bool) {
	if len(leaseIDs) == 0 {
		return nil, false
	}
	candidates, fromConfig := roster, false
	if len(candidates) == 0 {
		candidates, fromConfig = cfg.LayerSeatModels(), true
	}
	keep := map[string]bool{}
	for _, m := range EffectiveMemoryStack(cfg) {
		keep[strings.ToLower(strings.TrimSpace(m))] = true
	}
	var models []string
	seen := map[string]bool{}
	for _, id := range candidates {
		k := strings.ToLower(strings.TrimSpace(id))
		if k == "" || keep[k] || seen[k] {
			continue
		}
		seen[k] = true
		models = append(models, id)
	}
	if fromConfig && warn != nil {
		fmt.Fprintf(warn, "unload list: the llama-swap roster is empty or could not be read, so the list is the seats the config places on the leased cards and nothing else\n")
	}
	if len(models) == 0 {
		return nil, true
	}
	scope := NewLeaseScope(ctx, cfg, leaseIDs, deps, nil)
	on, _, unplaced := scope.SplitPlaced(models)
	if len(unplaced) > 0 && warn != nil {
		why := "the config does not place them on a card"
		if !scope.CardsReadable() {
			why = "the card table could not be read (" + scope.CardsWhy() + "), so no seat can be placed on a card"
		}
		fmt.Fprintf(warn, "unload list: left resident, placement unknown (%s): %s\n", why, strings.Join(unplaced, ", "))
	}
	return on, true
}

// UnloadEnv renders the list as the environment variable the render lane reads
// (GPU_LEASE_UNLOAD_MODELS): a comma list, `-` for "unload nothing", and no variable at all
// when the list is unknown, which leaves the lane on its own rule.
func UnloadEnv(models []string, known bool) string {
	if !known {
		return ""
	}
	if len(models) == 0 {
		return "GPU_LEASE_UNLOAD_MODELS=-"
	}
	return "GPU_LEASE_UNLOAD_MODELS=" + strings.Join(models, ",")
}
