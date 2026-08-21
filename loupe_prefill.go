package main

// Agent-loop prefill view (memory-frontier T2-B) — the accrual side of the instrument.
//
// # Why this file exists
//
// The prefill instrument itself shipped earlier and worked: it aggregates llama.cpp's own
// `timings` across an agent run. What it lacked was anywhere durable to put the answer —
// the report rode home on an in-process Result and a stderr line, both of which vanish with
// the call. So the question T2-B was re-aimed at ("does the agent loop have a large repeated
// prefix worth stabilising?") remained unanswerable from real traffic while the numbers were
// arriving on every step and being dropped.
//
// The ledger now carries them. This reads them back.
//
// # The decision this is for
//
// T2-B's original target was falsified by arithmetic: the text cascade's prompts have a
// median of 177 tokens and its ENTIRE multi-week prefill is a few seconds per day. The agent
// loop is the opposite shape — a long system prompt plus tool schemas plus a growing
// transcript, re-sent every step. High KV reuse here means llama.cpp is already handling it
// and prefix-stability work is unnecessary. LOW reuse with high absolute prefill is the only
// result that justifies the work.

import (
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// PrefillReport aggregates the agent-loop prefill rows in the ledger.
type PrefillReport struct {
	// Rows that carried a measurement. This is the discriminator: zero means the question
	// was never observed, which is NOT the same as observing zero prefill.
	MeasuredRows  int     `json:"measured_rows"`
	ObservedSteps int     `json:"observed_steps"`
	PrefillTokens int64   `json:"prefill_tokens"`
	CacheTokens   int64   `json:"cache_tokens"`
	PrefillMS     float64 `json:"prefill_ms"`

	// KVReusePct is CacheN/(CacheN+PromptN) — NOT CacheN/PromptN, which leaves the cached
	// tokens out of its own denominator and reports past 100%.
	//
	// A POINTER on purpose: with nothing measured there is no rate, and a float64 zero is
	// indistinguishable from a measured "nothing was reused" — the opposite conclusion, and
	// the one that would wrongly justify building the feature.
	KVReusePct        *float64 `json:"kv_reuse_pct"`
	AvgPrefillPerStep *float64 `json:"avg_prefill_tokens_per_step"`
	Basis             string   `json:"basis"` // "measured" | "insufficient_data"
	Verdict           string   `json:"verdict"`
}

// prefillMinRows is the floor below which no verdict is offered. A handful of agent runs
// cannot characterise a workload, and quoting a reuse rate off three rows is how a number
// nobody should trust ends up in a decision.
const prefillMinRows = 20

func buildPrefill(rows []ledger.Entry) PrefillReport {
	r := PrefillReport{Basis: "insufficient_data"}
	for _, e := range rows {
		if e.PrefillSteps <= 0 {
			continue // no observation on this row
		}
		r.MeasuredRows++
		r.ObservedSteps += e.PrefillSteps
		r.PrefillTokens += e.PrefillTokens
		r.CacheTokens += e.CacheTokens
		r.PrefillMS += e.PrefillMS
	}
	if r.MeasuredRows == 0 {
		r.Verdict = "no agent run has reported prefill timings yet — the question is unobserved, not answered"
		return r
	}
	if total := r.CacheTokens + r.PrefillTokens; total > 0 {
		v := float64(r.CacheTokens) / float64(total) * 100
		r.KVReusePct = &v
	}
	if r.ObservedSteps > 0 {
		a := float64(r.PrefillTokens) / float64(r.ObservedSteps)
		r.AvgPrefillPerStep = &a
	}
	if r.MeasuredRows < prefillMinRows {
		r.Verdict = "below the sample floor — counts shown, no verdict offered"
		return r
	}
	r.Basis = "measured"
	switch {
	case r.KVReusePct == nil:
		r.Verdict = "measured, but no prompt tokens were seen at all"
	case *r.KVReusePct >= 80:
		r.Verdict = "HIGH REUSE — llama.cpp is already reusing the agent prefix; prefix-stability work is NOT justified"
	case *r.KVReusePct >= 40:
		r.Verdict = "PARTIAL REUSE — investigate what breaks the prefix before building anything"
	default:
		r.Verdict = "LOW REUSE — the agent prefix is being re-prefilled; this is the case that justifies prefix-stability work"
	}
	return r
}
