// ratchet.go — the tokens-per-entry ratchet. A baseline is FROZEN once from a
// report (an explicit operator act, like pinning a version); every later run
// on the SAME corpus must land within the tolerance of that baseline or the
// check fails loudly. Cross-corpus comparisons are refused — a ratchet is
// only meaningful against the pinned artifact its baseline was frozen from.
package compeval

import (
	"encoding/json"
	"fmt"
	"os"
)

// DefaultTolerance is the ±2% band from the Phase-B design.
const DefaultTolerance = 0.02

// Baseline is the frozen reference: per-entry compacted token counts plus the
// pins that make comparison meaningful.
type Baseline struct {
	CorpusHash     string         `json:"corpus_hash"`
	Ladder         string         `json:"ladder"`
	TokensPerEntry map[string]int `json:"tokens_per_entry"`
}

// Freeze derives a Baseline from a report.
func Freeze(rep Report) Baseline {
	b := Baseline{CorpusHash: rep.CorpusHash, Ladder: rep.Ladder, TokensPerEntry: map[string]int{}}
	for _, e := range rep.Entries {
		b.TokensPerEntry[e.ID] = e.TokensAfter
	}
	return b
}

// SaveBaseline / LoadBaseline round-trip the frozen artifact as JSON.
func SaveBaseline(path string, b Baseline) error {
	j, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(j, '\n'), 0o644)
}

func LoadBaseline(path string) (Baseline, error) {
	var b Baseline
	raw, err := os.ReadFile(path)
	if err != nil {
		return b, err
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, fmt.Errorf("baseline %s: %w", path, err)
	}
	return b, nil
}

// Breach names one entry outside the tolerance band.
type Breach struct {
	EntryID  string  `json:"entry_id"`
	Baseline int     `json:"baseline_tokens"`
	Current  int     `json:"current_tokens"`
	Delta    float64 `json:"delta"` // (current-baseline)/baseline, signed
}

// Check compares a report against a frozen baseline. Refuses (error) on a
// corpus-hash or ladder mismatch, or on entries missing from either side —
// silence there would let the ratchet rot. Returns the breaches beyond
// ±tolerance (empty = ratchet holds). The band is TWO-SIDED on purpose: an
// IMPROVEMENT breaches too — any ladder-behavior change is an explicit
// operator act (re-freeze), never silent drift. tolerance 0 = strict
// equality; negative = DefaultTolerance.
func Check(b Baseline, rep Report, tolerance float64) ([]Breach, error) {
	if tolerance < 0 {
		tolerance = DefaultTolerance
	}
	if b.CorpusHash != rep.CorpusHash {
		return nil, fmt.Errorf("ratchet refused: baseline corpus %.12s != report corpus %.12s (re-freeze on the new pinned corpus instead)", b.CorpusHash, rep.CorpusHash)
	}
	if b.Ladder != rep.Ladder {
		return nil, fmt.Errorf("ratchet refused: baseline ladder %q != report ladder %q", b.Ladder, rep.Ladder)
	}
	var breaches []Breach
	seen := map[string]bool{}
	for _, e := range rep.Entries {
		base, ok := b.TokensPerEntry[e.ID]
		if !ok {
			return nil, fmt.Errorf("ratchet refused: entry %q absent from the baseline (corpus changed without a hash change?)", e.ID)
		}
		seen[e.ID] = true
		if base == 0 {
			continue
		}
		delta := float64(e.TokensAfter-base) / float64(base)
		if delta > tolerance || delta < -tolerance {
			breaches = append(breaches, Breach{EntryID: e.ID, Baseline: base, Current: e.TokensAfter, Delta: round4(delta)})
		}
	}
	for id := range b.TokensPerEntry {
		if !seen[id] {
			return nil, fmt.Errorf("ratchet refused: baseline entry %q missing from the report", id)
		}
	}
	return breaches, nil
}
