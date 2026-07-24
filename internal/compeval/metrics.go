// metrics.go — replay + per-kind aggregation. Evaluate is DETERMINISTIC and
// model-free: it replays each entry through the production ladder
// (agent.CompactReplay) at the entry's budget and measures with the ladder's
// own token estimator. Reports are stamped with the corpus hash and the ladder
// configuration; savings exist only as these measured ratios.
package compeval

import (
	"sort"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// LadderOpts selects the optional rungs for a replay run (mirrors production
// flags --gcf-compact / --skeleton-prune).
type LadderOpts struct {
	GCF      bool `json:"gcf"`
	Skeleton bool `json:"skeleton"`
}

// Label is the ladder id stamped into reports ("base", "gcf", "skeleton",
// "gcf+skeleton").
func (o LadderOpts) Label() string {
	switch {
	case o.GCF && o.Skeleton:
		return "gcf+skeleton"
	case o.GCF:
		return "gcf"
	case o.Skeleton:
		return "skeleton"
	default:
		return "base"
	}
}

// EntryResult is one entry's replay measurement.
type EntryResult struct {
	ID              string   `json:"id"`
	Kind            string   `json:"kind"`
	TokensBefore    int      `json:"tokens_before"`
	TokensAfter     int      `json:"tokens_after"`
	Budget          int      `json:"budget"`
	FitBudget       bool     `json:"fit_budget"`
	Ratio           float64  `json:"ratio"` // after/before (lower = more compression)
	EntityRetention float64  `json:"entity_retention"`
	LostEntities    []string `json:"lost_entities,omitempty"`
}

// KindStats aggregates one content-kind bucket.
type KindStats struct {
	Kind            string  `json:"kind"`
	Entries         int     `json:"entries"`
	MeanRatio       float64 `json:"mean_ratio"`
	MeanRetention   float64 `json:"mean_retention"`
	FitBudgetShare  float64 `json:"fit_budget_share"`
	TotalLostCount  int     `json:"total_lost_count"`
}

// Report is the eval artifact. CorpusHash pins WHICH corpus produced it;
// Ladder pins which rungs ran; ModelIDs record the serving context for runs
// whose scores involve a model (the deterministic replay itself is model-free,
// so ModelIDs may be empty there — honesty over decoration).
type Report struct {
	CorpusHash string        `json:"corpus_hash"`
	Ladder     string        `json:"ladder"`
	ModelIDs   []string      `json:"model_ids,omitempty"`
	Entries    []EntryResult `json:"entries"`
	PerKind    []KindStats   `json:"per_kind"`
}

// deriveBudget: entries without an explicit budget replay at 60% of their own
// size — enough pressure that the ladder must actually engage.
func deriveBudget(tokens int) int {
	b := tokens * 6 / 10
	if b < 1 {
		b = 1
	}
	return b
}

// Evaluate replays every entry and aggregates per kind. Pure of I/O.
func Evaluate(entries []Entry, corpusHash string, opts LadderOpts) Report {
	rep := Report{CorpusHash: corpusHash, Ladder: opts.Label()}
	for _, e := range entries {
		before := agent.EstimateTokens(e.Turns)
		budget := e.BudgetTokens
		if budget <= 0 {
			budget = deriveBudget(before)
		}
		keep := e.KeepRecent
		if keep <= 0 {
			keep = 1
		}
		prot := e.ProtectedPrefix
		if prot <= 0 {
			prot = 1
		}
		after := agent.CompactReplay(e.Turns, budget, keep, prot, agent.ReplayOpts{GCF: opts.GCF, Skeleton: opts.Skeleton})
		afterTokens := agent.EstimateTokens(after)
		ret, lost := Retention(e.Turns, after)
		ratio := 1.0
		if before > 0 {
			ratio = float64(afterTokens) / float64(before)
		}
		rep.Entries = append(rep.Entries, EntryResult{
			ID: e.ID, Kind: e.Kind,
			TokensBefore: before, TokensAfter: afterTokens, Budget: budget,
			FitBudget: afterTokens <= budget, Ratio: round4(ratio),
			EntityRetention: round4(ret), LostEntities: lost,
		})
	}
	rep.PerKind = aggregate(rep.Entries)
	return rep
}

func aggregate(entries []EntryResult) []KindStats {
	byKind := map[string][]EntryResult{}
	for _, r := range entries {
		byKind[r.Kind] = append(byKind[r.Kind], r)
	}
	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	var out []KindStats
	for _, k := range kinds {
		rs := byKind[k]
		var ratio, ret, fit float64
		lost := 0
		for _, r := range rs {
			ratio += r.Ratio
			ret += r.EntityRetention
			if r.FitBudget {
				fit++
			}
			lost += len(r.LostEntities)
		}
		n := float64(len(rs))
		out = append(out, KindStats{
			Kind: k, Entries: len(rs),
			MeanRatio: round4(ratio / n), MeanRetention: round4(ret / n),
			FitBudgetShare: round4(fit / n), TotalLostCount: lost,
		})
	}
	return out
}

func round4(f float64) float64 {
	return float64(int(f*10000+0.5)) / 10000
}
