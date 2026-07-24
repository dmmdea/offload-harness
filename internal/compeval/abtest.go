// abtest.go — the task-outcome A/B: the same entries scored FULL vs COMPACTED
// by an injected model-backed scorer, with the control-pair SELF-TEST gate in
// front. The gate is the harvested lesson this file exists for: before any A/B
// verdict counts, the scorer must correctly rank a known-good/known-degraded
// pair — a scorer that cannot tell those apart CANNOT be trusted to rank
// full-vs-compacted, and the run ABORTS instead of producing a confident
// number from a blind judge.
package compeval

import (
	"context"
	"fmt"
	"strings"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/eval"
)

// Scorer scores one rendered transcript (higher = better task signal). The
// concrete signal is injected — the CLI wires the pipeline's accept+grounded
// OUTCOME scorer (see compaction_eval.go); tests inject deterministic fakes.
type Scorer func(ctx context.Context, rendered string) (float64, error)

// ControlPair is a known-ordering probe: Good must outscore Degraded.
type ControlPair struct {
	Name     string `json:"name"`
	Good     string `json:"good"`
	Degraded string `json:"degraded"`
}

// SelfTest runs every control pair through the scorer and errors on the FIRST
// mis-ranking (ties count as failures — a scorer with no discrimination is as
// unusable as an inverted one).
func SelfTest(ctx context.Context, score Scorer, pairs []ControlPair) error {
	if len(pairs) == 0 {
		return fmt.Errorf("control-pair self-test: no pairs configured — an A/B without the gate is not admissible")
	}
	for _, p := range pairs {
		g, err := score(ctx, p.Good)
		if err != nil {
			return fmt.Errorf("control pair %q: scoring good side: %w", p.Name, err)
		}
		d, err := score(ctx, p.Degraded)
		if err != nil {
			return fmt.Errorf("control pair %q: scoring degraded side: %w", p.Name, err)
		}
		if g <= d {
			return fmt.Errorf("control-pair self-test FAILED on %q: good=%.4f <= degraded=%.4f — the scorer cannot rank known-ordered content; aborting the A/B", p.Name, g, d)
		}
	}
	return nil
}

// Render flattens a transcript slice to the text a scorer reads (role-tagged,
// deterministic).
func Render(msgs []agent.Msg) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// ABResult is one entry's paired scores.
type ABResult struct {
	ID            string  `json:"id"`
	Kind          string  `json:"kind"`
	ScoreFull     float64 `json:"score_full"`
	ScoreCompact  float64 `json:"score_compact"`
}

// ABReport is the A/B artifact: per-entry paired scores plus the bootstrap
// delta (compact − full; CI from eval.BootstrapDeltaMean). GatePassed records
// that the control-pair gate ran and passed — a report cannot exist without it.
type ABReport struct {
	CorpusHash string     `json:"corpus_hash"`
	Ladder     string     `json:"ladder"`
	ModelIDs   []string   `json:"model_ids,omitempty"`
	GatePassed bool       `json:"gate_passed"`
	Results    []ABResult `json:"results"`
	Delta      float64    `json:"delta"`
	DeltaLo    float64    `json:"delta_lo"`
	DeltaHi    float64    `json:"delta_hi"`
}

// RunAB gates on SelfTest, then scores each entry full-vs-compacted and
// aggregates the paired bootstrap delta. Entries whose scoring errors abort
// the run (a partial A/B silently missing entries would bias the delta).
func RunAB(ctx context.Context, score Scorer, entries []Entry, corpusHash string, opts LadderOpts, pairs []ControlPair, modelIDs []string) (ABReport, error) {
	rep := ABReport{CorpusHash: corpusHash, Ladder: opts.Label(), ModelIDs: modelIDs}
	if err := SelfTest(ctx, score, pairs); err != nil {
		return rep, err
	}
	rep.GatePassed = true
	var full, comp []float64
	for _, e := range entries {
		msgs := e.Msgs()
		budget, keep, prot := entryParams(e, msgs)
		compacted := agent.CompactReplay(msgs, budget, keep, prot, agent.ReplayOpts{GCF: opts.GCF, Skeleton: opts.Skeleton})
		sf, err := score(ctx, Render(msgs))
		if err != nil {
			return rep, fmt.Errorf("entry %q: scoring full: %w", e.ID, err)
		}
		sc, err := score(ctx, Render(compacted))
		if err != nil {
			return rep, fmt.Errorf("entry %q: scoring compacted: %w", e.ID, err)
		}
		rep.Results = append(rep.Results, ABResult{ID: e.ID, Kind: e.Kind, ScoreFull: round4(sf), ScoreCompact: round4(sc)})
		full = append(full, sf)
		comp = append(comp, sc)
	}
	if len(full) > 0 {
		d, lo, hi := eval.BootstrapDeltaMean(full, comp, 2000, 42)
		rep.Delta, rep.DeltaLo, rep.DeltaHi = round4(d), round4(lo), round4(hi)
	}
	return rep, nil
}
