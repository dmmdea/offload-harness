package main

// Escalation-agreement view (memory-frontier Phase 2, Tier 1 item 10).
//
// # This reads a counterfactual that was ALREADY RUNNING, unreported
//
// The Counterfactual-replay gate was held for a month as "blocked on identity coverage".
// It was not blocked. The experiment it wanted has been running in production the whole
// time and simply had no reader: every escalated call records whether the ENTRY tier's
// answer agreed with the FINAL tier's, into confhead-labels.jsonl, at 1:1 coverage of the
// rows with escalations > 0.
//
// Identity coverage was never the blocker, and it is worth saying why so the gate is not
// re-opened on that basis: input_sha256 / prompt_prefix_sha256 are ONE-WAY. Even at 100%
// coverage the ledger could never reconstruct an input to replay. The agreement label is
// the only counterfactual this system can produce, and it produces it for free.
//
// # What the rate is, and the four things that make it NOT the flip rate
//
// This view deliberately refuses to publish an unconditional "flip rate", because the
// population it is drawn from is not the population such a rate would describe:
//
//  1. CONDITIONAL ON ESCALATION. Only escalated calls are labelled -- a small share of all
//     traffic. Escalation is triggered by low confidence, so these are the calls most
//     likely to disagree. The rate is therefore an UPPER bound on the unconditional one.
//  2. POOLED ACROSS TIER BINDINGS. The entry tier has not been one model over the corpus.
//     A single percentage silently averages different systems.
//  3. BURSTY PROVENANCE. The rows arrive in tight clusters with long gaps -- a bench-sweep
//     signature, not organic traffic. Inter-arrival is reported so a reader can see it.
//  4. DROP-BIASED. answersAgree judges only classify/triage and only parseable candidates;
//     everything else is discarded. Discards skew toward extreme disagreement, so the
//     surviving rate is biased UPWARD (toward agreement).
//
// So: report the conditional rate with its denominator, and report the unconditional rate
// as insufficient_data. A number that cannot state which population it describes is not a
// measurement, and this one has four reasons not to be mistaken for one.

import (
	"time"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// AgreementReport is the read side of confhead-labels.jsonl.
type AgreementReport struct {
	Rows int `json:"rows"`
	// Agreed + Disagreed sum to Rows: every row carries a decided verdict by construction
	// (labelAgreement returns early when answersAgree cannot judge).
	Agreed    int `json:"agreed"`
	Disagreed int `json:"disagreed"`

	// DisagreementRate is CONDITIONAL ON ESCALATION. A pointer so an empty corpus reports
	// null rather than a fabricated 0% -- 0% and "no labels yet" are opposite findings.
	DisagreementRate *float64 `json:"disagreement_rate_given_escalation"`
	// Unconditional is always insufficient_data. Stated as a field rather than left out so
	// a consumer cannot mistake the conditional rate for it.
	UnconditionalBasis string `json:"unconditional_flip_rate_basis"`

	ByTask []statRow `json:"disagreements_by_task,omitempty"`
	Tiers  []statRow `json:"entry_tiers_pooled,omitempty"`

	FirstTS string `json:"first_ts,omitempty"`
	LastTS  string `json:"last_ts,omitempty"`
	// Bursts counts clusters separated by more than burstGapHours. A corpus that is a
	// handful of bursts is a bench sweep; one with many is organic traffic. This is what
	// lets a reader judge caveat 3 instead of taking it on trust.
	Bursts int    `json:"arrival_bursts"`
	Basis  string `json:"basis"`
}

const burstGapHours = 6

func buildAgreement(rows []ledger.Entry) AgreementReport {
	r := AgreementReport{
		Basis:              "insufficient_data",
		UnconditionalBasis: "insufficient_data (labels exist only for escalated calls)",
	}
	byTask := map[string]int{}
	tiers := map[string]int{}
	var stamps []int64
	for _, e := range rows {
		if e.EscalatedAgreed == nil {
			continue // not a decided label row
		}
		r.Rows++
		if *e.EscalatedAgreed {
			r.Agreed++
		} else {
			r.Disagreed++
			if e.Task != "" {
				byTask[e.Task]++
			}
		}
		if e.ModelTier != "" {
			tiers[e.ModelTier]++
		}
		if e.TS > 0 {
			stamps = append(stamps, e.TS)
		}
	}
	if r.Rows == 0 {
		return r
	}
	r.Basis = "measured (conditional on escalation)"
	v := float64(r.Disagreed) / float64(r.Rows) * 100
	r.DisagreementRate = &v
	r.ByTask = topN(byTask, r.Disagreed, 8)
	r.Tiers = topN(tiers, r.Rows, 8)

	sortInt64(stamps)
	r.FirstTS = time.Unix(stamps[0], 0).Format(time.RFC3339)
	r.LastTS = time.Unix(stamps[len(stamps)-1], 0).Format(time.RFC3339)
	r.Bursts = 1
	for i := 1; i < len(stamps); i++ {
		if float64(stamps[i]-stamps[i-1])/3600.0 > burstGapHours {
			r.Bursts++
		}
	}
	return r
}

func sortInt64(a []int64) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// safeDate trims an RFC3339 stamp to its date, tolerating an empty or short value rather
// than panicking on a slice bound -- a report that crashes on a thin corpus is worse than
// one that prints "?".
func safeDate(ts string) string {
	if len(ts) < 10 {
		return "?"
	}
	return ts[:10]
}
