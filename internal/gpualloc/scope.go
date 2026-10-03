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
}

// NewLeaseScope reads the card table once (deps.Cards) for a lease holding ids (lease ids). A
// table it cannot read is said once on warn (nil = silent) and every seat is then treated as on
// the leased cards.
func NewLeaseScope(ctx context.Context, cfg config.Config, ids []string, deps Deps, warn io.Writer) LeaseScope {
	s := LeaseScope{cfg: cfg, ids: ids}
	if len(ids) == 0 {
		return s
	}
	cards, _, err := deps.withDefaults().Cards(ctx, cfg)
	s.cards, s.cardsOK = cards, err == nil && len(cards) > 0
	if !s.cardsOK && warn != nil {
		// The fallback is right (every doubt fences) and silent, until now: the operator would
		// believe a seat on another card was spared.
		why := "no card was listed"
		if err != nil {
			why = err.Error()
		}
		fmt.Fprintf(warn, "gpu reserve: the card table could not be read (%s): every seat is treated as sitting on the leased cards, so the drain and unload act as for the whole node\n", why)
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

// Split divides models into those on the leased cards and those that are not.
func (s LeaseScope) Split(models []string) (on, off []string) {
	for _, m := range models {
		if s.Touches(m) {
			on = append(on, m)
		} else {
			off = append(off, m)
		}
	}
	return on, off
}

// EffectiveMemoryStack is the memory stack a lease never unloads: the config's, else the default.
func EffectiveMemoryStack(cfg config.Config) []string {
	if len(cfg.MemoryStack) == 0 {
		return config.Default().MemoryStack
	}
	return cfg.MemoryStack
}

// UnloadModels is the model-id list the render lane's freeLlamaSwap unloads under a lease that
// holds leaseIDs: the llama-swap roster, minus the memory stack, minus every seat pinned to
// cards the lease does not hold. A model nobody declared a pin for stays on the list (it could
// be anywhere). ok is false for a whole-node lease (nothing to narrow: the render lane keeps its
// own rule) and when the roster is empty.
func UnloadModels(ctx context.Context, cfg config.Config, roster []string, leaseIDs []string, deps Deps, warn io.Writer) ([]string, bool) {
	if len(leaseIDs) == 0 || len(roster) == 0 {
		return nil, false
	}
	scope := NewLeaseScope(ctx, cfg, leaseIDs, deps, warn)
	keep := map[string]bool{}
	for _, m := range EffectiveMemoryStack(cfg) {
		keep[strings.ToLower(strings.TrimSpace(m))] = true
	}
	var out []string
	for _, id := range roster {
		if keep[strings.ToLower(strings.TrimSpace(id))] {
			continue
		}
		if scope.Touches(id) {
			out = append(out, id)
		}
	}
	return out, true
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
