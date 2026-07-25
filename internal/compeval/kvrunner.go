// kvrunner.go — the LIVE sequencing for the Phase D KV/TTFT bench. Pure
// orchestration over a ProbeFunc: no HTTP here, so the whole sequence is
// testable against a fake server (see kvbench_test.go).
//
// TWO SEQUENCING RULES THAT ARE NOT NEGOTIABLE ON THIS STACK:
//
//  1. ARMS RUN IN BLOCKS, NEVER INTERLEAVED. The serving tier runs
//     `--parallel 1` — ONE KV slot. Interleaving arm A and arm B round-robin
//     (the textbook way to spread thermal/background drift) would make every
//     single request a cache miss, because each arm would evict the other's
//     prefix. Blocks confound drift with arm, which is why rounds are repeated
//     and per-round values are reported rather than pooled.
//  2. CONTROLS BRACKET EVERY RUN. Residency here is a property of the
//     SCHEDULER, not of our code: one measured `/v1/embeddings` request was
//     enough to evict the model and take the cache with it. Without a positive
//     control before AND after, a table of zeros reads as "compaction destroys
//     the KV cache" when the true cause is that the model process died. The
//     run fails closed to INCONCLUSIVE.
package compeval

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// ProbeFunc issues ONE measured request and returns the server's accounting.
type ProbeFunc func(ctx context.Context, msgs []agent.Msg) (ProbeResult, error)

// BenchOpts configures a bench run.
type BenchOpts struct {
	Ladder LadderOpts
	// Rounds repeats the whole (controls + both arms) block; more rounds is
	// the only honest defence against drift given rule 1 above.
	Rounds int
	// StepStride samples every Nth step of a transcript (1 = every step). The
	// final step of an entry is always measured regardless of stride.
	StepStride int
	// MaxEntries caps how many corpus entries are replayed (0 = all).
	MaxEntries int
	// CtxTokens / MaxOut describe the SERVED window and the reserved output
	// budget; they parameterize the safety-margin decision table and, in
	// production budget mode, the ladder budget itself.
	CtxTokens int
	MaxOut    int
	// BudgetMode selects how the on-arm ladder budget is derived:
	//
	//   "production" — ctxTokens - maxOut - margin, i.e. exactly what
	//     Loop.inputBudget() uses. Fire counts are then meaningful as a
	//     statement about this corpus at this window.
	//   "pressure"   — 60% of each entry's own estimate (what Evaluate uses).
	//     Guarantees the ladder engages so the RAMP is observable, but the
	//     fire COUNT is then a property of the fixture, not of production.
	//
	// Whichever is used is stamped in the report; the distinction is the
	// difference between a measurement and an artifact.
	BudgetMode string
	// Margin is the production safety margin (agent's compactionMargin) used
	// by production budget mode.
	Margin int
	// RunNonce makes every request unique to this run+arm so the server's
	// MULTI-PROMPT cache cannot serve one arm from another arm's entries, or
	// this run from a previous run's. Auto-generated when empty; recorded in
	// the report. See salt().
	RunNonce string
}

func (o BenchOpts) withDefaults() BenchOpts {
	if o.Rounds <= 0 {
		o.Rounds = 1
	}
	if o.StepStride <= 0 {
		o.StepStride = 1
	}
	if o.CtxTokens <= 0 {
		o.CtxTokens = agent.FallbackContextTokens
	}
	if o.MaxOut <= 0 {
		o.MaxOut = 1024
	}
	if o.BudgetMode != BudgetProduction && o.BudgetMode != BudgetPressure {
		o.BudgetMode = BudgetPressure
	}
	if o.Margin <= 0 {
		o.Margin = agent.CompactionMargin
	}
	if o.RunNonce == "" {
		o.RunNonce = strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return o
}

// Budget modes — see BenchOpts.BudgetMode.
const (
	BudgetProduction = "production"
	BudgetPressure   = "pressure"
)

// KVReport is the bench artifact. Samples are RAW — every headline number
// below can be re-derived from them.
type KVReport struct {
	CorpusHash string `json:"corpus_hash"`
	// EntriesReplayed disambiguates the hash: --max-entries truncates the
	// corpus, so two runs over the same file can legitimately differ. The hash
	// alone would claim they are the same artifact.
	EntriesReplayed int    `json:"entries_replayed"`
	Ladder          string `json:"ladder"`
	Rounds          int    `json:"rounds"`
	// BudgetMode records how the on-arm ladder budget was derived — the fire
	// COUNT is a direct consequence of it and means nothing without it.
	BudgetMode string `json:"budget_mode"`
	// RunNonce is the per-run marker that isolates this run (and each arm
	// within it) from the server's multi-prompt cache. Two reports with the
	// same nonce came from the same run.
	RunNonce string `json:"run_nonce"`

	// Verdict is ADMISSIBLE or INCONCLUSIVE. On INCONCLUSIVE the samples are
	// still emitted (they are evidence about the environment) but no headline
	// number may be quoted from them.
	Verdict            string    `json:"verdict"`
	InadmissibleReason string    `json:"inadmissible_reason,omitempty"`
	Controls           []Control `json:"controls"`
	// FramingFloorReuse is what an UNRELATED prompt still reuses: the shared
	// template + tool-spec prefix every production request carries. It is the
	// zero point every other reuse number should be read against, not an
	// anomaly — measured ~0.17 once the real tool specs are sent.
	FramingFloorReuse float64 `json:"framing_floor_reuse"`

	Arms        []ArmStats    `json:"arms"`
	Overflow    OverflowStats `json:"overflow"`
	Paired      PairedTotals  `json:"paired_totals"`
	SizeVsCache SizeVsCache   `json:"size_vs_cache"`
	Estimator   EstimatorFit `json:"estimator_fit"`
	MarginTable []MarginRow `json:"margin_table"`
	Samples     []Sample    `json:"samples"`
	Limits      []string    `json:"limits"`
}

// OverflowStats counts requests the SERVER REJECTED for exceeding the context
// window, per arm. This is not noise: it is the failure the ladder exists to
// prevent, and an off-arm overflow means that request would simply not have
// completed at all.
type OverflowStats struct {
	OffErrors int `json:"off_errors"`
	OnErrors  int `json:"on_errors"`
	// Timeouts and OtherErrors keep every failure visible. A client timeout
	// carries the text "context deadline exceeded", which a naive substring
	// match on "context"/"exceed" would file as an overflow — i.e. as evidence
	// FOR the phase's own headline. Timeouts are therefore classified first
	// and counted separately, and anything unrecognised lands in OtherErrors
	// rather than nowhere.
	Timeouts    int `json:"timeouts"`
	OtherErrors int `json:"other_errors"`
}

// ErrorClass buckets a probe failure. Order matters: timeout BEFORE overflow.
func ErrorClass(errText string) string {
	l := strings.ToLower(errText)
	switch {
	case l == "":
		return ""
	case strings.Contains(l, "deadline exceeded"), strings.Contains(l, "timeout"),
		strings.Contains(l, "client.timeout"):
		return "timeout"
	case strings.Contains(l, "exceed_context_size"),
		strings.Contains(l, "exceeds the available context"),
		strings.Contains(l, "context length"), strings.Contains(l, "too many tokens"):
		return "overflow"
	case strings.HasPrefix(l, "chat 413"):
		return "overflow"
	default:
		return "other"
	}
}

// PairedTotals compares the arms over the INTERSECTION of steps that succeeded
// in BOTH — the only apples-to-apples comparison. Totals over unequal sample
// sets would silently overstate whichever arm had fewer (usually larger)
// requests. Unpaired steps are reported, never dropped in silence.
type PairedTotals struct {
	PairedSteps int `json:"paired_steps"`
	UnpairedOff int `json:"unpaired_off"`
	UnpairedOn  int `json:"unpaired_on"`

	OffRealPromptTokens int `json:"off_real_prompt_tokens"`
	OnRealPromptTokens  int `json:"on_real_prompt_tokens"`
	// SizeEffectTokens: real prompt tokens saved by a smaller transcript (off-on).
	SizeEffectTokens int `json:"size_effect_tokens"`
	OffPrefilled     int `json:"off_prefilled_tokens"`
	OnPrefilled      int `json:"on_prefilled_tokens"`
	// PrefillDeltaTokens = on - off. POSITIVE means compaction made the server
	// do MORE prefill work despite sending fewer tokens, because each fire
	// discards the KV cache.
	PrefillDeltaTokens int `json:"prefill_delta_tokens"`
}

// SizeVsCache is a PER-ARM diagnostic only: raw totals over each arm's own
// successful samples, which are NOT the same step set (the off arm loses steps
// to overflow rejections). Differences between these columns are not a valid
// comparison — PairedTotals is. Field names carry the warning so a reader
// grepping the JSON cannot mistake one for the other.
type SizeVsCache struct {
	OffRealPromptTokensUnpaired int `json:"off_real_prompt_tokens_unpaired_diagnostic"`
	OnRealPromptTokensUnpaired  int `json:"on_real_prompt_tokens_unpaired_diagnostic"`
	OffPrefilledUnpaired        int `json:"off_prefilled_tokens_unpaired_diagnostic"`
	OnPrefilledUnpaired         int `json:"on_prefilled_tokens_unpaired_diagnostic"`
	// Cache effect per arm: tokens the SERVER skipped via KV reuse.
	OffCacheEffectTokens int `json:"off_cache_effect_tokens"`
	OnCacheEffectTokens  int `json:"on_cache_effect_tokens"`
	Note                 string `json:"note"`
}

// --- controls ---------------------------------------------------------------

// controlBase builds a deterministic multi-hundred-token payload. Distinct,
// numbered lines make the divergence point of the mid-edit control exact.
func controlBase(tag string) string {
	var b strings.Builder
	for i := 1; i <= 90; i++ {
		fmt.Fprintf(&b, "Line %d of the %s control block: the quick brown fox jumps over the lazy dog near the river bank at dawn.\n", i, tag)
	}
	return b.String()
}

// controlMidEdited returns the base with ONE line rewritten ~20% in — the
// shape the compaction ladder actually produces (it edits from protectedEnd
// forward, i.e. near the START of the prompt, not the end).
func controlMidEdited(tag string) string {
	lines := strings.Split(strings.TrimRight(controlBase(tag), "\n"), "\n")
	if len(lines) > 20 {
		lines[19] = "Line 20 of the " + tag + " control block: COMPLETELY DIFFERENT WORDING now occupies this particular position instead."
	}
	return strings.Join(lines, "\n") + "\n"
}

func msgsOf(text string) []agent.Msg {
	return []agent.Msg{{Role: "user", Content: text}}
}

// salt prefixes the FIRST message with a per-run, per-arm marker so no request
// can be served from a cache entry created by a different arm or a different
// run.
//
// WHY THIS IS REQUIRED (measured 2026-07-24): llama.cpp keeps a MULTI-PROMPT
// cache, not merely the last prefix. Sending prompt A, then an unrelated B,
// then A again returned cache_n=2064 for A — B did not evict it, and both were
// restorable. Two consequences the bench must defend against:
//
//  1. The negative control is only "unrelated" if that exact prompt has never
//     been sent. Without a per-run salt, a second run finds its own negative
//     control still cached from the first and reports ~100% reuse (observed).
//  2. The arms replay OVERLAPPING transcripts — on a no-fire step the
//     compacted transcript is byte-identical to the raw one — so the arm that
//     runs second could be served from the first arm's cache entries, making
//     the comparison measure ordering rather than compaction.
//
// The cost is a ~10-token marker on the first message, disclosed in the
// report's limits. The alternative — disabling the server's prompt cache —
// means editing the operator's live llama-swap config, which is out of scope
// for a measurement.
func salt(msgs []agent.Msg, nonce, arm string) []agent.Msg {
	if nonce == "" || len(msgs) == 0 {
		return msgs
	}
	out := make([]agent.Msg, len(msgs))
	copy(out, msgs)
	out[0].Content = "[kvbench " + nonce + "/" + arm + "]\n" + out[0].Content
	return out
}

// RunControls issues the calibration shots. phase is "pre" or "post": the pre
// phase measures all four properties, the post phase re-checks residency only.
//
// The APPEND and MIDEDIT controls do NOT gate admissibility — they are
// MEASURED PROPERTIES of the serving stack that decide how the body samples
// must be read (append-only reuse working while mid-prompt divergence collapses
// the cache is exactly what makes the ladder's fires expensive).
func RunControls(ctx context.Context, probe ProbeFunc, phase, nonce string) ([]Control, error) {
	// Salted so the negative control is genuinely unseen: the server's
	// multi-prompt cache would otherwise hand back a previous run's copy.
	base := salt(msgsOf(controlBase(phase)), nonce, "ctl")[0].Content
	var out []Control

	measureMsgs := func(name string, prime, then []agent.Msg) error {
		if len(prime) > 0 {
			if _, err := probe(ctx, prime); err != nil {
				return fmt.Errorf("control %s (prime): %w", name, err)
			}
		}
		r, err := probe(ctx, then)
		if err != nil {
			return fmt.Errorf("control %s: %w", name, err)
		}
		out = append(out, Control{Name: name, Probe: r, ReuseFrac: ReuseFrac(r)})
		return nil
	}
	measure := func(name, prime, then string) error {
		var p []agent.Msg
		if prime != "" {
			p = msgsOf(prime)
		}
		return measureMsgs(name, p, msgsOf(then))
	}

	posName := CtrlPosPre
	if phase == "post" {
		posName = CtrlPosPost
	}
	// Positive: prime with X, then resend X byte-identically.
	if err := measure(posName, base, base); err != nil {
		return out, err
	}
	if phase == "post" {
		return out, nil
	}
	// Negative: an unrelated payload right after X must share ~nothing.
	if err := measure(CtrlNegPre, "", controlBase("unrelated-negative")); err != nil {
		return out, err
	}
	// Append: re-prime X, then send X plus a NEW MESSAGE — the shape the agent
	// loop actually produces between fires (a new turn is appended; earlier
	// turns keep their bytes AND their template framing, so the cached prompt
	// is a true prefix of the new one).
	//
	// Deliberately NOT an append to the last message's CONTENT: that is a
	// different shape, because the chat template puts an end-of-turn marker
	// straight after the content, so extending it DIVERGES rather than
	// extends. Measured content-append behaviour was inconsistent across runs
	// (2316/2321 once, 1/2447 twice) and is recorded as unresolved rather than
	// smoothed over — the loop never produces that shape anyway.
	if err := measureMsgs(CtrlAppend, msgsOf(base), append(msgsOf(base),
		agent.Msg{Role: "assistant", Content: "Acknowledged."},
		agent.Msg{Role: "user", Content: "Continue with the next step."},
	)); err != nil {
		return out, err
	}
	// Mid-edit: re-prime X, then send X with an early line rewritten (the
	// on-fire shape).
	if err := measure(CtrlMidEdit, base, controlMidEdited(phase)); err != nil {
		return out, err
	}
	return out, nil
}

// --- body -------------------------------------------------------------------

// runArm replays every entry step-by-step for one arm, in order, tracking the
// previously SENT transcript so each sample knows whether it was a pure append
// (cache-friendly) or a mid-prompt divergence (cache-destroying).
func runArm(ctx context.Context, probe ProbeFunc, entries []Entry, arm string, round int, opts BenchOpts) ([]Sample, error) {
	var out []Sample
	var prevSent []agent.Msg
	prevReal := 0 // server-reported length of the previous successful request

	for _, e := range entries {
		full := e.Msgs()
		budget, keep, prot := entryParams(e, full)
		if opts.BudgetMode == BudgetProduction {
			// Exactly Loop.inputBudget(): the budget production actually
			// compacts against. Fire counts under this mode describe the
			// corpus at this window; under "pressure" they describe the
			// fixture.
			budget = opts.CtxTokens - opts.MaxOut - opts.Margin
			if budget < 256 {
				budget = 256
			}
			keep = agent.DefaultKeepRecent
		}
		first := true
		for k := 1; k <= len(full); k++ {
			// Stride sampling, but never skip the final (largest) step.
			if k != len(full) && (k%opts.StepStride) != 0 {
				continue
			}
			msgs := full[:k]
			if !wellFormed(msgs) {
				continue // a prefix cut between a tool call and its result is not a state the loop ever sends
			}
			sent := msgs
			s := Sample{
				Arm: arm, Round: round, EntryID: e.ID, EntryKind: e.Kind, Step: k,
				FirstStep: first,
			}
			if arm == "on" {
				sent = agent.CompactReplay(msgs, budget, keep, prot,
					agent.ReplayOpts{GCF: opts.Ladder.GCF, Skeleton: opts.Ladder.Skeleton})
				s.Fired = !SameTranscript(sent, msgs)
				s.DropFired = len(sent) < len(msgs)
				s.Exhausted = agent.EstimateTokens(sent) > budget
			}
			// Salt AFTER the ladder ran (so compaction sees the real
			// transcript) and BEFORE anything is measured or compared, so the
			// relation annotations describe the bytes actually sent.
			sent = salt(sent, opts.RunNonce, arm)
			s.EstTokens = agent.EstimateTokens(sent)
			s.SentMsgs = len(sent)
			s.SentChars = charsOf(sent)
			s.DivergenceIndex = DivergenceIndex(prevSent, sent)
			rel, shared, frac := Relation(prevSent, sent)
			s.PrefixRelation, s.PrefixSharedChars, s.PrefixSharedFrac = string(rel), shared, frac

			r, err := probe(ctx, sent)
			if err != nil {
				// A rejected request is DATA, not a run failure: the off arm
				// is expected to overflow the window on long transcripts —
				// that overflow is precisely what the ladder exists to
				// prevent. Record it and keep going; the cache is untouched,
				// so prevSent deliberately does NOT advance.
				s.Error = err.Error()
				s.ErrorClass = ErrorClass(s.Error)
				out = append(out, s)
				first = false
				continue
			}
			s.Probe = r
			s.ReuseFrac = ReuseFrac(r)
			s.PrevRealTokens = prevReal
			s.EvictionSuspected = s.SuspectedEviction()
			if real := r.RealPromptTokens(); real > 0 && s.EstTokens > 0 {
				s.EstRatio = float64(real) / float64(s.EstTokens)
			}
			out = append(out, s)
			prevSent = sent
			prevReal = r.RealPromptTokens()
			first = false
		}
	}
	return out, nil
}

// RunBench executes the full measurement: controls, both arms in blocks, then
// closing controls — repeated for opts.Rounds. It NEVER returns a headline
// number without an admissible instrument.
func RunBench(ctx context.Context, probe ProbeFunc, entries []Entry, corpusHash string, opts BenchOpts) (KVReport, error) {
	opts = opts.withDefaults()
	if len(entries) == 0 {
		return KVReport{}, fmt.Errorf("kvbench: empty corpus")
	}
	if opts.MaxEntries > 0 && len(entries) > opts.MaxEntries {
		entries = entries[:opts.MaxEntries]
	}
	rep := KVReport{
		CorpusHash: corpusHash,
		Ladder:     opts.Ladder.Label(),
		Rounds:     opts.Rounds,
		// Starts INCONCLUSIVE and is promoted only after the controls pass:
		// every early-return path below writes a report to disk, and an
		// aborted run stamped ADMISSIBLE would be a lie that outlives the
		// non-zero exit code.
		Verdict:         "INCONCLUSIVE",
		EntriesReplayed: len(entries),
		BudgetMode:      opts.BudgetMode,
		RunNonce:        opts.RunNonce,
	}

	for round := 1; round <= opts.Rounds; round++ {
		pre, err := RunControls(ctx, probe, "pre", opts.RunNonce)
		rep.Controls = append(rep.Controls, pre...)
		if err != nil {
			return rep, err
		}
		for _, arm := range []string{"off", "on"} { // BLOCKS, never interleaved — one KV slot
			ss, err := runArm(ctx, probe, entries, arm, round, opts)
			rep.Samples = append(rep.Samples, ss...)
			if err != nil {
				return rep, err
			}
		}
		post, err := RunControls(ctx, probe, "post", opts.RunNonce)
		rep.Controls = append(rep.Controls, post...)
		if err != nil {
			return rep, err
		}
	}

	// Promotion, never assumption: the report only claims ADMISSIBLE here,
	// after a complete run whose controls all passed.
	for _, c := range rep.Controls {
		if c.Name == CtrlNegPre && c.ReuseFrac > rep.FramingFloorReuse {
			rep.FramingFloorReuse = c.ReuseFrac
		}
	}
	if ok, reason := Admissible(rep.Controls); ok {
		rep.Verdict = "ADMISSIBLE"
	} else {
		rep.InadmissibleReason = reason
	}

	var off, on []Sample
	okOff, okOn := map[string]Sample{}, map[string]Sample{}
	for _, s := range rep.Samples {
		if s.Error != "" {
			// Errored requests carry no timings to aggregate, but the failure
			// itself is a headline: an overflow means that request would not
			// have completed at all. Every other failure is counted too — a
			// failure nobody counts is a failure nobody sees.
			switch ErrorClass(s.Error) {
			case "overflow":
				if s.Arm == "off" {
					rep.Overflow.OffErrors++
				} else {
					rep.Overflow.OnErrors++
				}
			case "timeout":
				rep.Overflow.Timeouts++
			default:
				rep.Overflow.OtherErrors++
			}
			continue
		}
		key := s.EntryID + "\x00" + strconv.Itoa(s.Step) + "\x00" + strconv.Itoa(s.Round)
		if s.Arm == "off" {
			off = append(off, s)
			okOff[key] = s
		} else {
			on = append(on, s)
			okOn[key] = s
		}
	}
	// Paired comparison over steps that succeeded in BOTH arms.
	for key, o := range okOff {
		n, ok := okOn[key]
		if !ok {
			rep.Paired.UnpairedOff++
			continue
		}
		rep.Paired.PairedSteps++
		rep.Paired.OffRealPromptTokens += o.Probe.RealPromptTokens()
		rep.Paired.OnRealPromptTokens += n.Probe.RealPromptTokens()
		rep.Paired.OffPrefilled += o.Probe.PromptN
		rep.Paired.OnPrefilled += n.Probe.PromptN
	}
	for key := range okOn {
		if _, ok := okOff[key]; !ok {
			rep.Paired.UnpairedOn++
		}
	}
	rep.Paired.SizeEffectTokens = rep.Paired.OffRealPromptTokens - rep.Paired.OnRealPromptTokens
	rep.Paired.PrefillDeltaTokens = rep.Paired.OnPrefilled - rep.Paired.OffPrefilled
	offStats, onStats := Summarize("off", off), Summarize("on", on)
	rep.Arms = []ArmStats{offStats, onStats}
	rep.SizeVsCache = SizeVsCache{
		OffRealPromptTokensUnpaired: offStats.TotalRealTokens,
		OnRealPromptTokensUnpaired:  onStats.TotalRealTokens,
		OffPrefilledUnpaired:        offStats.TotalPrefilled,
		OnPrefilledUnpaired:         onStats.TotalPrefilled,
		OffCacheEffectTokens:        offStats.TotalReused,
		OnCacheEffectTokens:         onStats.TotalReused,
		Note:                        "per-arm totals over DIFFERENT step sets; do not subtract these columns — use paired_totals",
	}
	rep.Estimator = FitEstimator(rep.Samples)
	// The margin table spans a standard ladder PLUS the ratios actually
	// measured here — so the operator sees both the textbook grid and where
	// this corpus really lands on it.
	ratios := []float64{1.0, 1.1, 1.25, 1.5, 2.0}
	for _, r := range []float64{rep.Estimator.P50, rep.Estimator.P95, rep.Estimator.P99, rep.Estimator.Max} {
		if r > 1 {
			ratios = append(ratios, r)
		}
	}
	sort.Float64s(ratios)
	rep.MarginTable = MarginTable(ratios, opts.CtxTokens, opts.MaxOut)
	rep.Limits = benchLimits()
	return rep, nil
}

// benchLimits are the disclosures every report carries verbatim. They are part
// of the artifact, not decoration: each one names a way these numbers could be
// over-read.
func benchLimits() []string {
	return []string{
		"Single box, single model tier, single serving config — KV-cache lifetime is a property of the SCHEDULER, not of the compaction ladder.",
		"Arms run in BLOCKS, never interleaved (the tier serves --parallel 1: one KV slot, so interleaving would make every request a cache miss). Drift is therefore confounded with arm; repeat rounds and read per-round values.",
		"Medians without confidence intervals, by design: N is small and wall-clock on a shared desktop is not normally distributed.",
		"Transcripts are harvested replays, not live agent runs: real tool latency and the H8 pin state (compactOpts.Pinned) are absent, so the replayed ladder is slightly gentler than production.",
		"The size effect and the cache effect are reported separately and must never be summed into one 'compaction speedup' — a shorter transcript prefills faster at zero cache reuse.",
		"A concurrent embedding/other-model request can evict the tier mid-run; the bracketing positive controls are what make a run admissible, and a failed control means INCONCLUSIVE, not zero reuse.",
		"The server keeps a MULTI-PROMPT cache (measured: A, then unrelated B, then A again restored A at cache_n=2064), so every request here carries a per-run/per-arm salt on its first message. Without it the arms would read each other's cache entries and a re-run would find its own negative control still cached. Cost: a ~10-token marker the production transcript does not have.",
		"Because the salt makes each arm's prompts unique, the reuse a REAL agent gets may be HIGHER than measured here: production reruns over similar transcripts can hit that same multi-prompt cache across runs.",
	}
}
