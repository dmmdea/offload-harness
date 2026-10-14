package backendchain

import "github.com/dmmdea/offload-harness/internal/config"

// TierSeat is the vLLM agent seat an installed tier declares, with the llama.cpp seat the tier serves
// when the vLLM prerequisites are missing.
type TierSeat struct {
	ID       string
	Aliases  []string
	Fallback string
}

// TierLookup answers whether the box's tier declares a vLLM agent seat that agentSeat is (or is the
// fallback of).
type TierLookup func(cfg config.Config, agentSeat string) (TierSeat, bool)

// SetTierLookup installs the tier-table reader.
func SetTierLookup(fn TierLookup) (restore func()) { return func() {} }

// PlanLanes derives the lanes to report from a config.
func PlanLanes(cfg config.Config) []Lane { return nil }
