package agent

import "sync"

// Agent-loop prefill instrument (memory-frontier T2-B, re-aimed).
//
// # Why this exists, and why it replaced what T2-B originally proposed
//
// T2-B was ranked "highest leverage/effort in track": restructure the task prompts
// so the STABLE part (system + grammar + pinned exemplars) stops being disturbed by
// the BM25 exemplar injection that currently mutates the FRONT of consecutive
// prompts, defeating llama.cpp's own prefix reuse.
//
// Then the ledger was measured, and the ranking did not survive. Across 34.5 days
// the text cascade's prompts have a MEDIAN of 177 tokens and total 412,372 prompt
// tokens — about 412 seconds of prefill in total, or ~12 seconds PER DAY at the
// ~1,000 tok/s this box measures. Eliminating 100% of it saves 12 s/day, against a
// prompt restructure carrying a stated accept-rate-parity (i.e. quality) risk.
// That is falsified by arithmetic, not by prematurity.
//
// But the same arithmetic says where the leverage actually is. The ledger records
// only the PIPELINE. The agent loop's calls never enter it — and the agent loop is
// the one workload here that structurally has a large repeated prefix: a long
// system prompt plus tool schemas plus a growing transcript, re-sent on EVERY step.
// It is also entirely unmeasured.
//
// So T2-B is re-aimed: build the instrument first and let it decide, instead of
// restructuring cascade prompts on an assumption the cascade's own numbers refute.
//
// # What was already here, and what was actually missing
//
// The per-call half already existed: LLMClient.Chat decodes llama.cpp's `timings`
// into Completion.Serve (CacheN, PromptN, PromptMS). What did NOT exist was any
// aggregation — Serve was consumed only by the token calibrator and by
// compaction_eval. So the question "does the agent loop have a big repeated prefix
// worth stabilising?" could not be answered from a real run at all.
//
// This is that aggregation, and nothing more. It changes no request, no routing and
// no prompt. It is an instrument whose job is to decide whether the expensive thing
// is worth building.

// PrefillStats accumulates the SERVER's own prefill accounting across one agent run.
//
// Every field is a count, never a rate: rates are computed at read time so a caller
// cannot accidentally average a set of averages.
//
// # Concurrency — this is why the mutex is here and not an afterthought
//
// `--serve` shares ONE *Loop across concurrent HTTP handlers. The token calibrator
// sitting at the same call site was found mutating a shared slice from several
// goroutines at once, and is gated OFF by default partly for that reason. This
// instrument is deliberately always-on — an instrument that has to be switched on
// is one nobody switches on, and it would then be measuring a special mode rather
// than real traffic. Always-on therefore obliges it to be safe under exactly the
// concurrency that caught the calibrator.
type PrefillStats struct {
	mu sync.Mutex
	// Steps that carried server timings. Tracked separately from the run's step
	// count because a backend that reports no `timings` block yields nil Serve,
	// and "10 steps, 0 of them measured" must never read as "10 steps measured".
	Observed int
	// CacheN is prompt tokens the server served FROM ITS KV CACHE.
	CacheN int64
	// PromptN is prompt tokens the server actually had to prefill.
	PromptN int64
	// PromptMS is wall time spent prefilling — the quantity T2-B would be trying
	// to reduce, and the one that makes the win concrete rather than proportional.
	PromptMS float64
	// PredictedN is generated tokens. Carried so a reader can see prefill cost in
	// proportion to the work the run actually produced.
	PredictedN int64
}

// Observe folds one completion's server accounting into the run total.
//
// A nil ServeStats is IGNORED rather than counted as a zero-token step. Counting it
// would silently drag the reuse ratio toward whatever the absent value implies,
// which is the same class of defect as publishing a fabricated 0 for a rate nobody
// measured.
func (p *PrefillStats) Observe(s *ServeStats) {
	if p == nil || s == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// A backend can emit a `timings` block with everything zero. That is a real
	// observation of a step that prefilled nothing, so it counts.
	p.Observed++
	p.CacheN += int64(s.CacheN)
	p.PromptN += int64(s.PromptN)
	p.PromptMS += s.PromptMS
	p.PredictedN += int64(s.PredictedN)
}

// PrefillReport is the read-side view. It is what a decision gets made from.
type PrefillReport struct {
	// Steps with server timings. Zero means NOTHING below was measured.
	ObservedSteps int `json:"observed_steps"`
	// Raw totals, so a reader can recompute anything here independently.
	CacheTokens    int64   `json:"cache_tokens"`
	PrefillTokens  int64   `json:"prefill_tokens"`
	PrefillMS      float64 `json:"prefill_ms"`
	GeneratedToken int64   `json:"generated_tokens"`

	// KVReusePct is the share of PROMPT tokens the server did not have to prefill:
	//
	//	CacheN / (CacheN + PromptN)
	//
	// NOT CacheN/PromptN — that denominator excludes the cached tokens themselves
	// and runs past 100%. LLMClient.ServeStats carries the same warning, and it is
	// repeated here because this is where the number gets published.
	//
	// A POINTER, deliberately. With no observed steps there is no ratio, and a
	// float64 zero would be indistinguishable from a genuine, measured "nothing was
	// reused" — the precise failure this estate has already shipped once, in
	// `duplicate_input_rate`, where an unmeasured 0 would have closed a gate. nil
	// serialises as JSON null; a consumer must branch on Basis first.
	KVReusePct *float64 `json:"kv_reuse_pct"`
	// Basis is the machine-readable discriminator: "measured" | "insufficient_data".
	Basis string `json:"basis"`

	// AvgPrefillPerStep sizes the prize in the unit T2-B would attack. nil when
	// unmeasured, for the same reason as above.
	AvgPrefillTokensPerStep *float64 `json:"avg_prefill_tokens_per_step"`
	AvgPrefillMSPerStep     *float64 `json:"avg_prefill_ms_per_step"`
}

// Report renders under the lock and copies the counters out, so a concurrent
// Observe can neither tear a field nor be blocked for longer than the copy.
func (p *PrefillStats) Report() PrefillReport {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := PrefillReport{
		ObservedSteps:  p.Observed,
		CacheTokens:    p.CacheN,
		PrefillTokens:  p.PromptN,
		PrefillMS:      p.PromptMS,
		GeneratedToken: p.PredictedN,
		Basis:          "insufficient_data",
	}
	if p.Observed == 0 {
		return r
	}
	r.Basis = "measured"
	if total := p.CacheN + p.PromptN; total > 0 {
		v := float64(p.CacheN) / float64(total) * 100
		r.KVReusePct = &v
	}
	// Guarded separately from the ratio above: a run CAN observe steps that carried
	// zero prompt tokens, and those still have a meaningful per-step average of 0.
	tps := float64(p.PromptN) / float64(p.Observed)
	mps := p.PromptMS / float64(p.Observed)
	r.AvgPrefillTokensPerStep = &tps
	r.AvgPrefillMSPerStep = &mps
	return r
}
