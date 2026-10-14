package eval

import (
	"errors"
)

// Verdicts.
const (
	VerdictAdopt        = "ADOPT"
	VerdictBlock        = "BLOCK"
	VerdictInconclusive = "INCONCLUSIVE"
)

// Judge names the scorer that produced the scores. A deterministic scorer (the acceptance DSL, a
// regex, an exact match) needs no receipt; a model judge needs a passed self-test receipt for the
// same scorer, or the gate refuses.
type Judge struct {
	Kind     string `json:"kind"`     // "deterministic" | "model"
	Identity string `json:"identity"` // scorer id, e.g. "acceptance-dsl" or a model id
}

// JudgeReceipt is the self-test receipt `gate selftest` (a later change) produces for a model
// judge. Data only here.
type JudgeReceipt struct {
	ScorerID  string `json:"scorer_id"`
	PairsHash string `json:"pairs_hash"`
	Passed    bool   `json:"passed"`
	BuildSHA  string `json:"build_sha256"`
}

// GateInput is one comparison: a candidate arm against the incumbent arm under a frozen baseline.
type GateInput struct {
	BaselineHash string        `json:"baseline_hash"`
	Incumbent    *Arm          `json:"incumbent,omitempty"` // required unless the baseline froze one; if both, must be equal
	Candidate    Arm           `json:"candidate"`
	Judge        Judge         `json:"judge"`
	Receipt      *JudgeReceipt `json:"receipt,omitempty"`
}

// OutcomeCounts is the number of attempts of each outcome class on one arm, every class present
// (a zero is a zero, not an absent key).
type OutcomeCounts map[string]int

// AttemptCounts holds the per-arm outcome counts.
type AttemptCounts struct {
	Incumbent OutcomeCounts `json:"incumbent"`
	Candidate OutcomeCounts `json:"candidate"`
}

// ArmCount is one integer per arm.
type ArmCount struct {
	Incumbent int `json:"incumbent"`
	Candidate int `json:"candidate"`
}

// EfficiencyResult is the paired-bootstrap read of one declared efficiency metric over the
// usable pairs that resolved it on both arms.
type EfficiencyResult struct {
	Name            string  `json:"name"`
	Class           string  `json:"class"`
	LowerIsBetter   bool    `json:"lower_is_better"`
	Delta           float64 `json:"delta"`
	Lo              float64 `json:"lo"`
	Hi              float64 `json:"hi"`
	N               int     `json:"n"`
	UnresolvedPairs int     `json:"unresolved_pairs"`
	Improves        bool    `json:"improves"`
}

// HeldOutResult is the point-estimate check on the held-out pairs (GATE-R5).
type HeldOutResult struct {
	N      int     `json:"n"`
	Delta  float64 `json:"delta"`
	Failed bool    `json:"failed"`
}

// Projected is the half-width of the main interval projected to 16, 24 and 120 usable tasks.
type Projected struct {
	N16  float64 `json:"16"`
	N24  float64 `json:"24"`
	N120 float64 `json:"120"`
}

// TaskCount is a number of tasks, or the word "unbounded" when no finite number follows from the
// interval (superiority with no observed effect, fewer than two pairs).
type TaskCount struct {
	N         int
	Unbounded bool
}

// PowerNote says how far the corpus is from a conclusive size. It adds no statistic: it is the
// half-width of the main bootstrap interval read three ways (GATE-R6).
type PowerNote struct {
	HalfWidth   float64   `json:"half_width"`
	Projected   Projected `json:"projected"`
	TasksNeeded TaskCount `json:"tasks_needed"`
	TasksToAdd  TaskCount `json:"tasks_to_add"`
}

// ResultStamps are the stamps of the two runs.
type ResultStamps struct {
	Incumbent Stamps `json:"incumbent"`
	Candidate Stamps `json:"candidate"`
}

// GateResult is the gate's whole output. Field order is the struct's, and nothing in it is a
// timestamp: the same input twice gives byte-identical JSON.
type GateResult struct {
	Verdict       string             `json:"verdict"`
	Reason        string             `json:"reason"`
	KeepIncumbent bool               `json:"keep_incumbent"`
	Mode          string             `json:"mode"`
	Margin        float64            `json:"margin"`
	Delta         float64            `json:"delta"`
	Lo            float64            `json:"lo"`
	Hi            float64            `json:"hi"`
	NUsed         int                `json:"n_used"`
	NDeclared     int                `json:"n_declared"`
	ExcludedPairs int                `json:"excluded_pairs"`
	ExcludedShare float64            `json:"excluded_share"`
	AttemptCounts AttemptCounts      `json:"attempt_counts"`
	Cheats        ArmCount           `json:"cheats"`
	Efficiency    []EfficiencyResult `json:"efficiency"`
	HeldOut       *HeldOutResult     `json:"held_out"`
	Power         PowerNote          `json:"power"`
	Stamps        ResultStamps       `json:"stamps"`
	Judge         Judge              `json:"judge"`
	EraMarker     string             `json:"era_marker"`
	GateVersion   string             `json:"gate_version"`
	CorpusHash    string             `json:"corpus_hash"`
	BaselineHash  string             `json:"baseline_hash"`
	BootstrapB    int                `json:"bootstrap_b"`
	Seed          int64              `json:"seed"`
	// GateBuildSHA256 is the hash of the binary that ran the gate, set by the verb (Gate itself
	// is pure). It is not part of the era marker. Absent means unknown.
	GateBuildSHA256 string `json:"gate_build_sha256,omitempty"`
}

// Gate is the one measurement gate. It is pure: no I/O, no clock, no randomness beyond the
// baseline's seed.
func Gate(b Baseline, baselineHash string, in GateInput) (GateResult, error) {
	return GateResult{}, errors.New("gate: not implemented")
}
