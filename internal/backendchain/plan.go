package backendchain

import (
	"strings"
	"sync"

	"github.com/dmmdea/offload-harness/internal/config"
)

// TierSeat is the vLLM agent seat an installed tier declares, with the llama.cpp seat the tier serves
// when the vLLM prerequisites are missing (vllmseat.Spec.Fallback).
type TierSeat struct {
	ID       string
	Aliases  []string
	Fallback string
}

// TierLookup answers whether the box's tier declares a vLLM agent seat that agentSeat is (or is the
// fallback of). The tier table is embedded in the command package, so the command installs the reader
// (SetTierLookup) and every consumer of PlanLanes sees the same chain.
type TierLookup func(cfg config.Config, agentSeat string) (TierSeat, bool)

var (
	tierMu     sync.RWMutex
	tierLookup TierLookup
)

// SetTierLookup installs the tier-table reader and returns a function that puts the previous one back.
func SetTierLookup(fn TierLookup) (restore func()) {
	tierMu.Lock()
	prev := tierLookup
	tierLookup = fn
	tierMu.Unlock()
	return func() {
		tierMu.Lock()
		tierLookup = prev
		tierMu.Unlock()
	}
}

func lookupTier(cfg config.Config, agentSeat string) (TierSeat, bool) {
	tierMu.RLock()
	fn := tierLookup
	tierMu.RUnlock()
	if fn == nil {
		return TierSeat{}, false
	}
	return fn(cfg, agentSeat)
}

func (t TierSeat) answers(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(t.ID), name) {
		return true
	}
	for _, a := range t.Aliases {
		if strings.EqualFold(strings.TrimSpace(a), name) {
			return true
		}
	}
	return false
}

// PlanLanes derives the lanes to report from a config: one per model route that is bound (the config's
// own table, config.ModelRoutes, so doctor, offload_status and this report read one list). Every lane is
// its one bound seat, except the agent lane of a box whose tier declares a vLLM agent seat: the tier
// prefers that seat and serves a llama.cpp seat when the vLLM prerequisites are missing, so the lane's
// chain is [the vLLM seat, the llama.cpp fallback], with the bound one marked.
func PlanLanes(cfg config.Config) []Lane {
	var out []Lane
	for _, r := range cfg.ModelRoutes() {
		seat := strings.TrimSpace(r.Effective)
		if seat == "" {
			continue
		}
		chain := []Candidate{{Seat: seat, Bound: true}}
		if r.StatusKey == "agent" {
			if t, ok := lookupTier(cfg, seat); ok && strings.TrimSpace(t.ID) != "" {
				chain = []Candidate{{Seat: t.ID, Engine: EngineVLLM, Bound: t.answers(seat)}}
				if fb := strings.TrimSpace(t.Fallback); fb != "" && !strings.EqualFold(fb, t.ID) {
					// The tier table defines the fallback as a llama.cpp seat.
					chain = append(chain, Candidate{Seat: fb, Engine: EngineLlamaCpp, Bound: strings.EqualFold(fb, seat)})
				}
			}
		}
		out = append(out, Lane{Name: r.StatusKey, Chain: chain})
	}
	return out
}
