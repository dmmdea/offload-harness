// Package report turns the telemetry-only ledger into per-task quality stats
// (no model calls). It is the observational complement to package eval's
// labeled gold-set metrics — it reports raw production signals, not AURC/AUDC.
package report

import (
	"sort"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

type TaskStat struct {
	Task               string  `json:"task"`
	N                  int     `json:"n"`
	Deferred           int     `json:"deferred"`
	EscalationResolved int     `json:"escalation_resolved"` // a larger tier produced an accepted answer
	ReasoningReclaimed int     `json:"reasoning_reclaimed"` // completed via the terminal reasoning tier
	LabeledGrounded    int     `json:"labeled_grounded"`
	Grounded           int     `json:"grounded"`
	LowMarginAccepts   int     `json:"low_margin_accepts"` // accepted but margin in (0, threshold)
	DeferRate          float64 `json:"defer_rate"`
	GroundedRate       float64 `json:"grounded_rate"`
	TokensOut          int     `json:"tokens_out"`
}

// Summarize aggregates ledger entries per task. marginThreshold flags the
// "barely confident" accept band (the Phase-5 two-pass target). Cache hits are
// skipped (they carry no fresh model signal).
func Summarize(entries []ledger.Entry, marginThreshold float64) map[string]TaskStat {
	m := map[string]*TaskStat{}
	for _, e := range entries {
		if e.CacheHit {
			continue
		}
		s := m[e.Task]
		if s == nil {
			s = &TaskStat{Task: e.Task}
			m[e.Task] = s
		}
		s.N++
		s.TokensOut += e.TokensOut
		if e.Deferred {
			s.Deferred++
			continue
		}
		// A reclaim carries Escalations>0 (the cascade climbed before deferring to
		// the reasoning tier), but the escalation tier did NOT produce the answer —
		// attribute it to reasoning, never double-count it as an escalation resolve.
		if e.Reasoning {
			s.ReasoningReclaimed++
		} else if e.Escalations > 0 {
			s.EscalationResolved++
		}
		if e.Grounded != nil {
			s.LabeledGrounded++
			if *e.Grounded {
				s.Grounded++
			}
		}
		if e.Margin > 0 && marginThreshold > 0 && e.Margin < marginThreshold {
			s.LowMarginAccepts++
		}
	}
	out := map[string]TaskStat{}
	for k, s := range m {
		if s.N > 0 {
			s.DeferRate = float64(s.Deferred) / float64(s.N)
		}
		if s.LabeledGrounded > 0 {
			s.GroundedRate = float64(s.Grounded) / float64(s.LabeledGrounded)
		}
		out[k] = *s
	}
	return out
}

// SortedTasks returns stable task keys.
func SortedTasks(m map[string]TaskStat) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
