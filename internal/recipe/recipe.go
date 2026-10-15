// Package recipe is the recipe store of ADR 0036's 2026-10-15 amendment: a recipe is a typed
// record of closed-vocabulary agent_env_rules data, tagged with the seat capability it was
// distilled for, the acceptance-passing runs it came from and its status.
package recipe

import "github.com/dmmdea/offload-harness/internal/core"

// Statuses.
const (
	StatusProposed  = "proposed"
	StatusPublished = "published"
	StatusDemoted   = "demoted"
)

// Recipe is one record.
type Recipe struct {
	ID         string             `json:"id"`
	SeatTag    string             `json:"seat_tag"`
	Rules      core.AgentEnvRules `json:"rules"`
	Provenance []string           `json:"provenance"`
	Status     string             `json:"status"`
	Audit      *Audit             `json:"audit,omitempty"`
}

// Audit is what the last recipe-audit run found.
type Audit struct {
	Seat           string  `json:"seat"`
	BaselineHash   string  `json:"baseline_hash"`
	EraMarker      string  `json:"era_marker"`
	Verdict        string  `json:"verdict"`
	StepsIncumbent float64 `json:"steps_incumbent"`
	StepsCandidate float64 `json:"steps_candidate"`
}

// Decode reads one recipe.
func Decode(raw []byte) (Recipe, error) { return Recipe{}, nil }

// Validate checks a recipe.
func (r Recipe) Validate() error { return nil }

// Encode writes the canonical bytes of a recipe.
func (r Recipe) Encode() ([]byte, error) { return nil, nil }
