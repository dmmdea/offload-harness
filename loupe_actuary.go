package main

// Reliability bands (R2-14) and the failure-class atlas (R2-16) — two read-only ledger
// views that answer their own gate and are designed to CLOSE themselves for free.
//
// Both live here rather than in loupe.go to keep that file's aggregation loop readable;
// both are pure functions over already-read rows and touch nothing else.

import (
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// ---------------------------------------------------------------------------
// R2-14 — Confidence Actuary, REPORT HALF ONLY
// ---------------------------------------------------------------------------
//
// The routing half of this idea was deliberately stripped before it was ever built, and the
// reason is arithmetic rather than taste: at roughly 50 calls/day split across
// (task × tier), most cells hold well under one sample per day. Laplace smoothing would make
// those noisy cells LOOK confident, and a mis-ordered escalation rung is a quality regression
// on a quality-first stack. So this reports reliability and never routes on it.
//
// The one thing it must get right is saying "I don't know" loudly enough that nobody builds
// on a cell backed by three samples.

// minReliableSamples is the floor below which a cell reports insufficient_data instead of a
// rate. Chosen so a cell must carry more than a single day of that (task,tier) pair's typical
// traffic before it is quotable — not a round number for its own sake.
const minReliableSamples = 20

// ReliabilityCell is one (task, tier) pair's observed outcome mix.
type ReliabilityCell struct {
	Task string `json:"task"`
	Tier string `json:"tier"`
	N    int    `json:"n"`
	// Deferred / Escalated are counts, not rates, so a reader can recompute.
	Deferred  int `json:"deferred"`
	Escalated int `json:"escalated"`
	// SuccessRate is the share of calls that produced an answer without deferring.
	//
	// A POINTER: below minReliableSamples there is no defensible rate, and a float64 zero
	// would be indistinguishable from a measured "this cell fails every time" — which is the
	// opposite conclusion. nil serialises as null; branch on Basis first.
	SuccessRate *float64 `json:"success_rate"`
	Basis       string   `json:"basis"` // "measured" | "insufficient_data"
}

// ReliabilityReport is the whole R2-14 surface.
type ReliabilityReport struct {
	MinSamples int               `json:"min_samples_for_a_rate"`
	Cells      []ReliabilityCell `json:"cells,omitempty"`
	// Counted so the report can state its own coverage rather than implying the listed
	// cells are the whole picture.
	CellsMeasured   int `json:"cells_measured"`
	CellsSuppressed int `json:"cells_insufficient_data"`
}

func buildReliability(rows []ledger.Entry) ReliabilityReport {
	type agg struct{ n, def, esc int }
	m := map[[2]string]*agg{}
	for _, e := range rows {
		if e.Task == "" || e.ModelTier == "" {
			continue
		}
		k := [2]string{e.Task, e.ModelTier}
		a := m[k]
		if a == nil {
			a = &agg{}
			m[k] = a
		}
		a.n++
		if e.Deferred {
			a.def++
		}
		if e.Escalations > 0 {
			a.esc++
		}
	}
	rep := ReliabilityReport{MinSamples: minReliableSamples}
	for k, a := range m {
		c := ReliabilityCell{Task: k[0], Tier: k[1], N: a.n, Deferred: a.def, Escalated: a.esc, Basis: "insufficient_data"}
		if a.n >= minReliableSamples {
			v := float64(a.n-a.def) / float64(a.n) * 100
			c.SuccessRate = &v
			c.Basis = "measured"
			rep.CellsMeasured++
		} else {
			rep.CellsSuppressed++
		}
		rep.Cells = append(rep.Cells, c)
	}
	// Deterministic order: biggest cells first, then by name so equal-N cells do not shuffle
	// between runs (a report that reorders itself is a report nobody can diff).
	sort.Slice(rep.Cells, func(i, j int) bool {
		if rep.Cells[i].N != rep.Cells[j].N {
			return rep.Cells[i].N > rep.Cells[j].N
		}
		if rep.Cells[i].Task != rep.Cells[j].Task {
			return rep.Cells[i].Task < rep.Cells[j].Task
		}
		return rep.Cells[i].Tier < rep.Cells[j].Tier
	})
	return rep
}

// ---------------------------------------------------------------------------
// R2-16 — Ledger Counterexample Atlas
// ---------------------------------------------------------------------------
//
// Histogram the failure classes, and ship NOTHING unless a non-obsolete class recurs
// >= 5/month. What ships then is a hand-written anti-patterns block in the STABLE prompt
// prefix — never a BM25-retrieved varying block, which would enlarge exactly the volatile
// tail that prefix work exists to shrink and would put a cold-embedder swap on the critical
// path.
//
// # The exclusion is the whole point, and it is not cosmetic
//
// Most historical defers on this box are CONTEXT-OVERFLOW defers from before the seats moved
// to a 131k window. That class is dead — the condition that produced it cannot recur at the
// current context sizes. Leaving it in makes the corpus mostly dead labels and would send
// someone to write anti-patterns for a failure that no longer exists.
//
// So obsolete classes are excluded from the GATE but still reported, with their count, so the
// exclusion is auditable rather than a silent filter.

// obsoleteDeferPatterns marks defer classes that cannot recur under the current
// configuration. Substring match, lowercased.
//
// # These patterns are narrow for two reasons, both learned by checking the real ledger
//
// 1. THE LEDGER TRUNCATES REASONS (see maxReasonLen in internal/ledger). The first version
//    matched "exceeds the available context size" — which never fires, because the stored
//    string is cut mid-word to "...(10532 tokens) exceeds the availa". The classifier
//    reported "0 obsolete" against a ledger that contains 12 of them, and the atlas verdict
//    was computed on a corpus the exclusion had silently failed to clean. Match on
//    "tokens) exceeds", which is distinctive AND survives truncation.
//
// 2. "context" IS AMBIGUOUS IN GO. `context deadline exceeded` and `context canceled` are
//    HTTP timeout/cancellation errors with nothing to do with a context WINDOW. A loose
//    "context ..." pattern would sweep them in and silently delete a LIVE failure class from
//    the gate — the opposite of the mistake in (1), and worse, because it hides a real
//    problem instead of merely failing to hide a dead one. TestTimeoutDefersAreNotObsolete
//    guards this direction specifically.
//
// # Why this class is genuinely obsolete, evidenced rather than assumed
//
// All 12 occurrences in the live ledger are on `gemma-4-26b` and dated 2026-07-23/24, with
// requests of 8.6k-11.4k tokens — i.e. before the cascade seats moved to their 131k windows.
// The condition that produced them cannot recur at the current context sizes. That was
// checked (tier + date + request size), not inferred from the plan's say-so.
var obsoleteDeferPatterns = []string{
	"tokens) exceeds", // truncation-safe form of "(N tokens) exceeds the available context size"
	"exceed_context_size",
}

// atlasGatePerMonth is the recurrence threshold from R2-16. Below it, nothing ships.
const atlasGatePerMonth = 5.0

// FailureClass is one defer reason with its recurrence.
type FailureClass struct {
	Reason   string  `json:"reason"`
	Count    int     `json:"count"`
	PerMonth float64 `json:"per_month"`
	Obsolete bool    `json:"obsolete"`
	// ClearsGate is only meaningful for non-obsolete classes.
	ClearsGate bool `json:"clears_gate"`
}

// AtlasReport is the whole R2-16 surface, including its own verdict.
type AtlasReport struct {
	SpanDays        float64        `json:"span_days"`
	GatePerMonth    float64        `json:"gate_per_month"`
	TotalDefers     int            `json:"total_defers"`
	ObsoleteDefers  int            `json:"obsolete_defers"`
	LiveDefers      int            `json:"live_defers"`
	Classes         []FailureClass `json:"classes,omitempty"`
	QualifyingCount int            `json:"qualifying_classes"`
	// Verdict states the action, so the gate cannot be quietly reinterpreted later.
	Verdict string `json:"verdict"`
	Basis   string `json:"basis"` // "measured" | "insufficient_data"
}

func isObsoleteDefer(reason string) bool {
	r := strings.ToLower(reason)
	for _, p := range obsoleteDeferPatterns {
		if strings.Contains(r, p) {
			return true
		}
	}
	return false
}

func buildAtlas(rows []ledger.Entry, spanDays float64) AtlasReport {
	rep := AtlasReport{SpanDays: spanDays, GatePerMonth: atlasGatePerMonth, Basis: "measured"}
	counts := map[string]int{}
	for _, e := range rows {
		if !e.Deferred || e.Reason == "" {
			continue
		}
		rep.TotalDefers++
		counts[e.Reason]++
	}
	// A rate needs a window. Without one, report the counts and say the gate could not be
	// evaluated rather than dividing by a guess.
	if spanDays <= 0 {
		rep.Basis = "insufficient_data"
		rep.Verdict = "span unknown — cannot compute a per-month recurrence"
	}
	for reason, n := range counts {
		fc := FailureClass{Reason: reason, Count: n, Obsolete: isObsoleteDefer(reason)}
		if spanDays > 0 {
			fc.PerMonth = float64(n) / spanDays * 30
		}
		if fc.Obsolete {
			rep.ObsoleteDefers += n
		} else {
			rep.LiveDefers += n
			if rep.Basis == "measured" && fc.PerMonth >= atlasGatePerMonth {
				fc.ClearsGate = true
				rep.QualifyingCount++
			}
		}
		rep.Classes = append(rep.Classes, fc)
	}
	sort.Slice(rep.Classes, func(i, j int) bool {
		if rep.Classes[i].Count != rep.Classes[j].Count {
			return rep.Classes[i].Count > rep.Classes[j].Count
		}
		return rep.Classes[i].Reason < rep.Classes[j].Reason
	})
	if rep.Basis == "measured" {
		if rep.QualifyingCount == 0 {
			rep.Verdict = "CLOSED — no non-obsolete failure class recurs at the gate; ship nothing"
		} else {
			rep.Verdict = "QUALIFIES — hand-write an anti-patterns block into the STABLE prefix for the classes flagged clears_gate (never a retrieved varying block)"
		}
	}
	return rep
}

// truncReason keeps a long upstream error from wrapping the text report into noise.
// Rune-safe: a defer reason can carry non-ASCII (a Spanish input echoed back in an error),
// and cutting mid-rune would emit a replacement char.
func truncReason(s string) string {
	const max = 84
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
