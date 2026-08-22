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
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// AgreementReport is the read side of confhead-labels.jsonl.
//
// It reports the two writers SEPARATELY. Pooling them produces a rate that is conditional on
// escalation and unconditional at the same time, which is not a rate at all.
type AgreementReport struct {
	// --- live-escalation writer: pipeline.labelAgreement, ESCALATED calls only ---
	Rows      int `json:"rows"`
	Agreed    int `json:"agreed"`
	Disagreed int `json:"disagreed"`
	// DisagreementRate is CONDITIONAL ON ESCALATION. A pointer so an empty corpus reports
	// null rather than a fabricated 0% -- 0% and "no labels yet" are opposite findings.
	DisagreementRate *float64 `json:"disagreement_rate_given_escalation"`
	// Unconditional is always insufficient_data from THIS population by construction.
	UnconditionalBasis string `json:"unconditional_flip_rate_basis"`

	// --- shadow-counterfactual writer: NON-escalated calls, summarize judged by cosine ---
	// Counted and reported, never merged into the numbers above.
	ShadowRows      int      `json:"shadow_rows"`
	ShadowDisagreed int      `json:"shadow_disagreed"`
	ShadowRate      *float64 `json:"shadow_disagreement_rate"`

	// Rows written before provenance stamping existed. They get their OWN bucket and their
	// OWN rate: folding them into either population would be the guess the split exists to
	// prevent, but discarding them would throw away real measurements to achieve tidiness.
	// The reader is given the number and told what is unknown about it.
	UnknownSourceRows      int      `json:"unknown_source_rows"`
	UnknownSourceDisagreed int      `json:"unknown_source_disagreed"`
	UnknownSourceRate      *float64 `json:"unknown_source_disagreement_rate"`

	// Drops is the judge's own coverage loss, read from the sidecar counter. Pointer: a
	// missing counter file is unknown coverage, not zero drops.
	UnparseableDrops *int64 `json:"unparseable_drops"`

	ByTask []statRow `json:"disagreements_by_task,omitempty"`
	Tiers  []statRow `json:"entry_tiers_as_recorded,omitempty"`

	FirstTS string `json:"first_ts,omitempty"`
	LastTS  string `json:"last_ts,omitempty"`
	Bursts  int    `json:"arrival_bursts"`
	Basis   string `json:"basis"`
	// WindowDays echoes the --since filter applied to the LABELS, so a windowed report
	// cannot be mistaken for an all-time one.
	WindowDays int `json:"window_days,omitempty"`
}

// newAgreementReport stamps the basis strings that must never be empty. A zero-value
// AgreementReport escaping to a consumer publishes "" where a caller keys on
// "insufficient_data" -- the exact unfalsifiable shape this view exists to avoid.
func newAgreementReport() AgreementReport {
	return AgreementReport{
		Basis:              "insufficient_data",
		UnconditionalBasis: "insufficient_data (labels exist only for escalated calls)",
	}
}

const burstGapHours = 6

func buildAgreement(rows []ledger.Entry, sinceTS int64, windowDays int, drops *int64) AgreementReport {
	r := newAgreementReport()
	r.WindowDays = windowDays
	r.UnparseableDrops = drops

	byTask := map[string]int{}
	tiers := map[string]int{}
	var stamps []int64

	for _, e := range rows {
		if e.EscalatedAgreed == nil {
			continue // not a decided label row
		}
		// Apply the SAME window the ledger half of the report uses. Previously the labels
		// were read unfiltered, so `loupe --since 7` printed a 7-day ledger beside an
		// all-time agreement rate and all-time burst count, under one heading.
		if sinceTS > 0 && e.TS > 0 && e.TS < sinceTS {
			continue
		}

		switch e.LabelSource {
		case ledger.LabelSourceShadowCounterfactual:
			// A different population entirely: non-escalated calls, and summarize judged by
			// embedding cosine rather than answersAgree. Counted, never merged.
			r.ShadowRows++
			if !*e.EscalatedAgreed {
				r.ShadowDisagreed++
			}
			continue
		case ledger.LabelSourceLiveEscalation:
			// the population this report's headline describes
		default:
			// Written before provenance stamping existed. Counted in its own bucket rather
			// than guessed into one of the others -- and rated, because the rows are real
			// measurements whose provenance is the only unknown thing about them.
			r.UnknownSourceRows++
			if !*e.EscalatedAgreed {
				r.UnknownSourceDisagreed++
			}
			continue
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

	if r.UnknownSourceRows > 0 {
		v := float64(r.UnknownSourceDisagreed) / float64(r.UnknownSourceRows) * 100
		r.UnknownSourceRate = &v
	}
	if r.ShadowRows > 0 {
		v := float64(r.ShadowDisagreed) / float64(r.ShadowRows) * 100
		r.ShadowRate = &v
	}
	if r.Rows == 0 {
		return r
	}
	r.Basis = "measured (conditional on escalation)"
	v := float64(r.Disagreed) / float64(r.Rows) * 100
	r.DisagreementRate = &v
	r.ByTask = topN(byTask, r.Disagreed, 8)
	r.Tiers = topN(tiers, r.Rows, 8)

	// Guarded on len(stamps), NOT on r.Rows. A decided row with ts:0 is counted in Rows but
	// contributes no stamp, so `Rows > 0` did not imply a non-empty slice and stamps[0]
	// panicked -- taking down the whole loupe report, not just this block.
	if len(stamps) == 0 {
		return r
	}
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

// readAgreement loads the label sidecar and its drop counter.
//
// Every failure path returns a report whose basis strings are already stamped, so a caller
// can never receive one that says "". The three states are kept distinct because they mean
// different things: unreadable sidecar (a fault), no sidecar (never configured), and an
// empty sidecar (configured, nothing judged yet).
func readAgreement(cfg config.Config, sinceTS int64, windowDays int) AgreementReport {
	if cfg.ConfHeadLabelsPath == "" {
		r := newAgreementReport()
		r.Basis = "insufficient_data (no confhead_labels_path configured)"
		return r
	}
	lrows, lerr := ledger.ReadAll(cfg.ConfHeadLabelsPath)
	if lerr != nil {
		r := newAgreementReport()
		// A fault, NOT an empty corpus. Reporting these the same way would let a broken
		// reader look like a quiet one.
		r.Basis = "insufficient_data (label sidecar unreadable: " + lerr.Error() + ")"
		return r
	}
	return buildAgreement(lrows, sinceTS, windowDays, readLabelDrops(cfg.ConfHeadLabelsPath))
}

// readLabelDrops reads the judge's coverage-loss counter written beside the sidecar.
//
// Returns nil when the file is absent, and that distinction is load-bearing: nil means
// "coverage unknown", 0 means "measured, nothing dropped". Collapsing them would let an
// unmeasured corpus advertise perfect coverage.
func readLabelDrops(labelsPath string) *int64 {
	b, err := os.ReadFile(labelsPath + ".drops")
	if err != nil {
		return nil
	}
	var n int64
	if _, serr := fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &n); serr != nil {
		return nil
	}
	return &n
}
