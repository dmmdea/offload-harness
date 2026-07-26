// kvbench.go — the OmniRoute-harvest Phase D measurement core: KV-prefix reuse
// and REAL token accounting for the compaction ladder.
//
// WHAT THIS MEASURES, AND WHY THE ORIGINAL PREMISE WAS WRONG. The harvest
// design asked us to "verify the monotonic ladder preserves llama.cpp's KV
// prefix cache". Measured on this stack (2026-07-24), that statement is not
// true as written, for a reason stronger than expected: reuse here is BINARY.
// Appending a turn reuses everything the server held (2429 kept, 19 prefilled),
// but ANY edit to the prompt discards the ENTIRE cache — a position sweep
// showed a one-line edit at 98% of the prompt costing all 2662 tokens,
// identically to an edit at 4%.
// Since every lossy rung edits from protectedEnd FORWARD (compaction.go —
// dedupe/GCF/skeleton/elide/drop all iterate `for i := protectedEnd; i <
// recentStart`), the step on which the ladder fires always pays a full
// re-prefill. What monotonicity + the idempotence guards DO buy is that the
// steps BETWEEN fires stay pure appends, so the cache returns immediately
// afterwards.
//
// So the honest quantity is a RAMP, not a constant win:
//   - no-fire step   => append-only => reuse ~1.0 (measured 0.997)
//   - firing step    => any divergence => reuse ~0 (measured 0.0004)
//   - post-fire step => append again => reuse ~1.0
//
// And the fire's cost is not a compaction defect: NOT compacting does not keep
// the cache either — the transcript simply grows until the server rejects it
// outright (measured live as exceed_context_size 400s). The trade is a full
// re-prefill per fire against a request that does not complete at all.
// Phase D therefore measures how OFTEN the ladder fires over a real transcript
// and what each fire costs, against the size win compaction buys — two effects
// this package keeps strictly separate and NEVER sums.
//
// Everything in this file is pure and deterministic (no clock, no network) so
// it is unit-testable; the live sequencing lives in kvrunner.go.
package compeval

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// --- measured units ---------------------------------------------------------

// ProbeResult is one measured request as the SERVER reported it. Measured is
// false when the backend reported no accounting at all — an unmeasured request
// must never be read as "zero reuse".
type ProbeResult struct {
	CacheN            int     `json:"cache_n"`
	PromptN           int     `json:"prompt_n"`
	PromptMS          float64 `json:"prompt_ms"`
	PredictedMS       float64 `json:"predicted_ms"`
	UsagePromptTokens int     `json:"usage_prompt_tokens"`
	UsageCachedTokens int     `json:"usage_cached_tokens"`
	WallMS            float64 `json:"wall_ms"`
	// Measured: the backend reported SOME accounting (usage and/or timings).
	// KVMeasured: it reported the TIMINGS block, the only source of KV reuse.
	// A backend that emits usage but no timings (vLLM, OpenAI, most proxies)
	// is Measured && !KVMeasured — reporting reuse=0 for it would be exactly
	// the "unmeasured read as measured zero" the design forbids.
	Measured   bool `json:"measured"`
	KVMeasured bool `json:"kv_measured"`
}

// RealPromptTokens is the server's true prompt length: reused + prefilled.
// Prefer usage.prompt_tokens when present (it is the same number by a
// different route and cross-checks the timings block).
func (r ProbeResult) RealPromptTokens() int {
	if r.UsagePromptTokens > 0 {
		return r.UsagePromptTokens
	}
	return r.CacheN + r.PromptN
}

// ReuseFrac is cache_n/(cache_n+prompt_n) — the fraction of the prompt served
// from cache. Denominator is the REAL prompt length, never prompt_n alone
// (that would report >1 on a cache hit).
func ReuseFrac(r ProbeResult) float64 {
	total := r.CacheN + r.PromptN
	if total <= 0 {
		return 0
	}
	return float64(r.CacheN) / float64(total)
}

// Sample is one row of the bench: a single measured request plus the offline
// facts needed to interpret it. Rows are emitted RAW and never pre-aggregated,
// so a reader can re-derive every headline number.
type Sample struct {
	Arm       string `json:"arm"`   // "off" | "on"
	Round     int    `json:"round"`
	EntryID   string `json:"entry_id"`
	EntryKind string `json:"entry_kind"`
	Step      int    `json:"step"` // prefix length (message count) replayed

	// FirstStep marks the first request of an entry: the cache holds the
	// PREVIOUS entry, so a near-zero reuse here is conversation-start, not a
	// ladder property. Aggregation counts these separately in both directions.
	FirstStep bool `json:"first_step"`

	// Ladder facts (offline, deterministic).
	Fired           bool `json:"fired"`            // the ladder changed the transcript on this step
	DropFired       bool `json:"drop_fired"`       // a whole turn was dropped (turn count shrank)
	Exhausted       bool `json:"exhausted"`        // ladder ran and still could not fit the budget
	DivergenceIndex int  `json:"divergence_index"` // first message index differing from the previously SENT transcript; -1 = pure append
	// PrefixRelation/PrefixSharedChars describe the BYTE-stream relationship to
	// the previous request — the actual predictor of KV survival.
	PrefixRelation    string  `json:"prefix_relation"`
	PrefixSharedChars int     `json:"prefix_shared_chars"`
	PrefixSharedFrac  float64 `json:"prefix_shared_frac"` // how much of the PREVIOUS stream survived
	// PrevRealTokens is the server-reported length of the PREVIOUS successful
	// request — the yardstick the eviction invariant needs (see
	// SuspectedEviction); 0 when there was no previous request.
	PrevRealTokens int `json:"prev_real_tokens"`
	EstTokens       int  `json:"est_tokens"`       // the ladder's own chars/4 estimate of what was sent
	SentMsgs        int  `json:"sent_msgs"`
	SentChars       int  `json:"sent_chars"`

	// Server facts (measured).
	Probe     ProbeResult `json:"probe"`
	ReuseFrac float64     `json:"reuse_frac"`
	// Error is set when the server REJECTED the request — data, not a run
	// failure: the compaction-off arm is expected to overflow the window on
	// long transcripts, which is exactly the failure the ladder prevents.
	Error string `json:"error,omitempty"`
	// ErrorClass is "overflow" | "timeout" | "other" — a timeout carries the
	// text "context deadline exceeded" and must never be filed as an overflow.
	ErrorClass string `json:"error_class,omitempty"`
	// EvictionSuspected marks a sample the SCHEDULER ruined rather than the
	// ladder — see SuspectedEviction. Recorded per row so the exclusion is
	// auditable rather than silent.
	EvictionSuspected bool `json:"eviction_suspected,omitempty"`
	// EstRatio is real/estimated — the estimator's error on this exact
	// payload, the only sound basis for tuning the compaction safety margin.
	EstRatio float64 `json:"est_ratio"`
}

// evictionKeptFloor: on an EXTENSION the server should still hold (nearly) all
// of what it held before. Below this fraction of the PREVIOUS prompt, the
// cache was dropped between requests.
const evictionKeptFloor = 0.5

// SuspectedEviction reports whether this sample measured the SCHEDULER instead
// of the ladder.
//
// The test needs no extra requests, which is the point: injecting inline
// control shots between body requests would itself destroy the cache
// continuity being measured. It uses a measured INVARIANT — on a pure prefix
// EXTENSION the server keeps everything it already held — so an extension that
// LOST that content was torn down between the two requests. Mid-run evictions
// are not theoretical here: a single /v1/embeddings call causes one.
//
// The comparison is cache_n vs the PREVIOUS prompt's real length, NOT the
// reuse fraction. Reuse is a fraction of the NEW prompt, so appending a
// 750-token tool result onto a 17-token prefix legitimately scores 0.02 while
// the cache did its job perfectly — an earlier version of this check used that
// fraction and silently deleted ~20% of the body, biasing every median upward.
func (s Sample) SuspectedEviction() bool {
	if s.Error != "" || !s.Probe.KVMeasured || s.FirstStep || s.PrevRealTokens <= 0 {
		return false
	}
	if s.PrefixRelation != string(RelExtension) {
		return false // a divergence legitimately reuses nothing — that is the ladder's cost
	}
	return float64(s.Probe.CacheN) < evictionKeptFloor*float64(s.PrevRealTokens)
}

// --- controls ---------------------------------------------------------------

// Control is one calibration shot. The bench is only admissible when the
// controls prove the instrument works ON THIS BOX AT THIS MOMENT — residency
// here is a property of the scheduler, and a single embedding request was
// measured to evict the model and zero the cache.
type Control struct {
	Name      string      `json:"name"`
	Probe     ProbeResult `json:"probe"`
	ReuseFrac float64     `json:"reuse_frac"`
}

// Control names. POS/NEG gate admissibility; APPEND and MIDEDIT are MEASURED
// PROPERTIES of the serving stack (they characterize what the ladder's two
// access patterns actually cost) and never abort a run.
const (
	CtrlPosPre  = "pos-pre"
	CtrlNegPre  = "neg-pre"
	CtrlAppend  = "append"
	CtrlMidEdit = "mid-edit"
	CtrlPosPost = "pos-post"
)

// Admissibility thresholds.
//
// A byte-identical resend must reuse essentially everything. An unrelated
// prompt must reuse only the UNAVOIDABLE COMMON FRAMING — and that floor is not
// zero: every production request carries the same tool specs, which the chat
// template renders into a shared prefix. Measured here, that floor is ~16.8%
// of a ~2600-token prompt (≈440 tokens of specs). An earlier absolute ceiling
// of 5% encoded the assumption that no common framing exists; sending the real
// specs (as production does) falsified it, and the gate correctly refused to
// publish rather than quietly passing.
//
// So the ceiling is generous, and the LOAD-BEARING criterion is SEPARATION:
// the instrument is only trustworthy if it can tell reuse from no-reuse by a
// wide margin. That is a property of the measurement, not of the framing.
// NegReuseMax is a loose sanity bound only — the framing floor is legitimate
// and its size depends on how big the tool specs are relative to the prompt.
// MinPosNegMargin is the criterion that actually binds: with a large enough
// floor (small prompts, big spec set) the absolute numbers can both look
// "fine" while the metric has lost its ability to discriminate, and that is
// the condition worth refusing.
const (
	PosReuseMin     = 0.90
	NegReuseMax     = 0.75
	MinPosNegMargin = 0.40
)

// Admissible reports whether the control shots prove the instrument works.
//
// Fails CLOSED, and checks EVERY control instance rather than one per name: a
// multi-round run appends each round's controls, so keying by name would let a
// later round's pass silently overwrite an earlier round's failure — exactly
// the contamination the controls exist to catch.
func Admissible(cs []Control) (bool, string) {
	present := map[string]int{}
	for i, c := range cs {
		switch c.Name {
		case CtrlPosPre, CtrlPosPost, CtrlNegPre:
		default:
			continue // append / mid-edit are measured properties, not gates
		}
		present[c.Name]++
		where := " (control #" + strconv.Itoa(i+1) + ")"
		if !c.Probe.Measured {
			return false, "control " + c.Name + " unmeasured" + where + " — the backend reported no timings; KV reuse cannot be observed here"
		}
		switch c.Name {
		case CtrlPosPre:
			if c.ReuseFrac < PosReuseMin {
				return false, "positive control pre-run reused only " + pct(c.ReuseFrac) + " (need >=" + pct(PosReuseMin) + ")" + where + " — the model was evicted between identical requests; every arm would read as zero reuse for reasons that have nothing to do with compaction"
			}
		case CtrlPosPost:
			if c.ReuseFrac < PosReuseMin {
				return false, "positive control post-run reused only " + pct(c.ReuseFrac) + " (need >=" + pct(PosReuseMin) + ")" + where + " — residency was lost DURING the run; the body samples are contaminated"
			}
		case CtrlNegPre:
			if c.ReuseFrac > NegReuseMax {
				return false, "negative control reused " + pct(c.ReuseFrac) + " (need <=" + pct(NegReuseMax) + ")" + where + " — an unrelated prompt reused the MAJORITY of its prompt; the metric is not measuring what we think"
			}
		}
	}
	// Separation: the instrument must distinguish reuse from no-reuse by a
	// wide margin. This is what makes the numbers meaningful once a non-zero
	// common framing (tool specs) exists.
	pos, neg, havePos, haveNeg := 1.0, 0.0, false, false
	for _, c := range cs {
		switch c.Name {
		case CtrlPosPre, CtrlPosPost:
			if !havePos || c.ReuseFrac < pos {
				pos, havePos = c.ReuseFrac, true // worst positive
			}
		case CtrlNegPre:
			if !haveNeg || c.ReuseFrac > neg {
				neg, haveNeg = c.ReuseFrac, true // worst negative
			}
		}
	}
	if havePos && haveNeg && pos-neg < MinPosNegMargin {
		return false, "positive and negative controls are only " + pct(pos-neg) + " apart (need >=" + pct(MinPosNegMargin) + ") — the metric cannot distinguish a cache hit from a miss on this box right now"
	}
	for _, name := range []string{CtrlPosPre, CtrlNegPre, CtrlPosPost} {
		if present[name] == 0 {
			return false, "control " + name + " missing — an unproven instrument is not admissible"
		}
	}
	return true, ""
}

func pct(f float64) string {
	return trimFloat(math.Round(f*1000)/10) + "%"
}

// --- aggregation ------------------------------------------------------------

// ArmStats aggregates one arm honestly: medians plus the raw vectors, never a
// parametric interval over a handful of wall-clock numbers.
type ArmStats struct {
	Arm        string `json:"arm"`
	Samples    int    `json:"samples"`
	Fires      int    `json:"fires"`
	FirstSteps int    `json:"first_steps"` // conversation-start requests, excluded from the ramp medians
	// Evictions counts samples excluded because the tier was torn down between
	// requests (an extension that did not reuse). Reported, never hidden: a
	// high count means the box was busy and the medians rest on less data.
	Evictions int `json:"evictions_excluded"`
	// Unmeasured counts samples whose backend reported no timings block — kept
	// out of every rate, counted so the exclusion is visible.
	Unmeasured int `json:"unmeasured_excluded"`
	// NoFireSamples / OnFireSamples are the class counts behind the medians:
	// a class with zero samples reports a 0.0 median, and only these counts
	// distinguish that from a measured zero.
	NoFireSamples int `json:"no_fire_samples"`
	OnFireSamples int `json:"on_fire_samples"`
	// ExtensionReuse is the median reuse over CLEAN extension steps — the
	// direct, high-n answer to "do appends reuse?", far more robust than the
	// single append control shot.
	ExtensionReuse float64 `json:"median_extension_reuse"`

	MedianReuseFrac float64 `json:"median_reuse_frac"`
	MedianPromptMS  float64 `json:"median_prompt_ms"`
	MedianWallMS    float64 `json:"median_wall_ms"`

	// Split by ladder behavior — the whole point of the ramp. A single mean
	// across both classes hides the effect in both directions.
	MedianReuseNoFire float64 `json:"median_reuse_no_fire"`
	MedianReuseOnFire float64 `json:"median_reuse_on_fire"`
	MedianPromptMSNoFire float64 `json:"median_prompt_ms_no_fire"`
	MedianPromptMSOnFire float64 `json:"median_prompt_ms_on_fire"`

	// Real prompt tokens actually PREFILLED across the arm — the cost that
	// matters, and the one place the size win shows up.
	TotalPrefilled int `json:"total_prefilled_tokens"`
	TotalReused    int `json:"total_reused_tokens"`
	TotalRealTokens int `json:"total_real_prompt_tokens"`
}

// Summarize aggregates samples for one arm. Pure.
func Summarize(arm string, samples []Sample) ArmStats {
	st := ArmStats{Arm: arm, Samples: len(samples)}
	var reuse, promptMS, wallMS, reuseNo, reuseOn, msNo, msOn, ext []float64
	for _, s := range samples {
		// Token TOTALS keep every sample: they describe the work the server
		// actually did, and an evicted request really did prefill everything.
		st.TotalPrefilled += s.Probe.PromptN
		st.TotalReused += s.Probe.CacheN
		st.TotalRealTokens += s.Probe.RealPromptTokens()
		// RATE statistics require a KV-MEASURED sample: a backend that reports
		// usage but no timings has no reuse to report, and folding its
		// structural zero into a median is the "unmeasured as measured zero"
		// error this design forbids.
		if !s.Probe.KVMeasured {
			st.Unmeasured++
			continue
		}
		// ...and exclude scheduler artifacts — an evicted step measures
		// llama-swap, not the compaction ladder.
		if s.SuspectedEviction() {
			st.Evictions++
			continue
		}
		reuse = append(reuse, s.ReuseFrac)
		promptMS = append(promptMS, s.Probe.PromptMS)
		wallMS = append(wallMS, s.Probe.WallMS)
		if s.PrefixRelation == string(RelExtension) && !s.FirstStep {
			ext = append(ext, s.ReuseFrac)
		}
		switch {
		case s.FirstStep:
			// Cold by construction (the cache holds the previous entry) —
			// counted, never folded into either ramp class.
			st.FirstSteps++
		case s.Fired:
			st.Fires++
			reuseOn = append(reuseOn, s.ReuseFrac)
			msOn = append(msOn, s.Probe.PromptMS)
		default:
			reuseNo = append(reuseNo, s.ReuseFrac)
			msNo = append(msNo, s.Probe.PromptMS)
		}
	}
	st.MedianReuseFrac = Median(reuse)
	st.MedianPromptMS = Median(promptMS)
	st.MedianWallMS = Median(wallMS)
	st.NoFireSamples, st.OnFireSamples = len(reuseNo), len(reuseOn)
	st.MedianReuseNoFire = Median(reuseNo)
	st.MedianReuseOnFire = Median(reuseOn)
	st.MedianPromptMSNoFire = Median(msNo)
	st.MedianPromptMSOnFire = Median(msOn)
	st.ExtensionReuse = Median(ext)
	return st
}

// Median of a float slice (0 for empty). Does not mutate the input.
func Median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	c := append([]float64(nil), xs...)
	sort.Float64s(c)
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}

// --- estimator calibration + safety margin ----------------------------------

// EstimatorFit reports how far the ladder's chars/4 estimator is from the
// server's real token count. This is eviction-immune (token counts are exact
// however many times the process restarted), so it is measurable even when the
// KV leg is not.
type EstimatorFit struct {
	Samples int     `json:"samples"`
	P50     float64 `json:"ratio_p50"`
	P95     float64 `json:"ratio_p95"`
	P99     float64 `json:"ratio_p99"`
	Max     float64 `json:"ratio_max"`
	// ByKind isolates content-dependence — the reason a single global margin
	// is the wrong lever if the error is dense-JSON-specific.
	ByKind map[string]float64 `json:"ratio_p95_by_kind"`
	// Note warns that the ratio conflates a FIXED per-request payload (tool
	// specs, invisible to estimateTokens) with content density. Small prompts
	// are dominated by the fixed term, so a single multiplicative r read off
	// this distribution overstates the margin needed by large prompts — the
	// ones that actually overflow. tokencal.go models both terms; this
	// distribution does not.
	Note string `json:"note"`
}

// FitEstimator computes the real/estimated ratio distribution. Ratios come
// from samples that were actually measured; unmeasured rows are skipped.
func FitEstimator(samples []Sample) EstimatorFit {
	fit := EstimatorFit{ByKind: map[string]float64{}}
	var all []float64
	byKind := map[string][]float64{}
	// DEDUPE identical (est, real) payloads. Every corpus entry begins with the
	// same system+objective preamble, so step 1 contributes a byte-identical
	// row once per entry — 34 of 192 rows on the real corpus. Those duplicates
	// carry no extra information, yet being the densest rows (the fixed
	// tool-spec payload dominates a small prompt) they became the p95 for EVERY
	// content kind, reporting one identical number for code, logs, tool-json
	// and tool-text. A metric whose stated purpose is isolating
	// content-dependence must not be decided by a repeated constant.
	seen := map[[2]int]bool{}
	for _, s := range samples {
		if !s.Probe.Measured || s.EstTokens <= 0 {
			continue
		}
		r := s.EstRatio
		if r <= 0 {
			continue
		}
		// Identity requires BOTH numbers: with no real count we cannot tell two
		// payloads apart, so such rows are never deduped away.
		if real := s.Probe.RealPromptTokens(); real > 0 {
			key := [2]int{s.EstTokens, real}
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		all = append(all, r)
		byKind[s.EntryKind] = append(byKind[s.EntryKind], r)
	}
	fit.Samples = len(all)
	fit.P50 = Percentile(all, 0.50)
	fit.P95 = Percentile(all, 0.95)
	fit.P99 = Percentile(all, 0.99)
	fit.Max = Percentile(all, 1.0)
	for k, v := range byKind {
		fit.ByKind[k] = Percentile(v, 0.95)
	}
	fit.Note = "ratio = real/estimated, conflating a FIXED per-request payload (tool specs) with content density; small prompts are dominated by the fixed term, so a single multiplicative r overstates the margin large prompts need — see tokencal.go for the two-term model"
	return fit
}

// Percentile (nearest-rank, p in [0,1]). Deterministic; 0 for empty.
func Percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	c := append([]float64(nil), xs...)
	sort.Float64s(c)
	if p <= 0 {
		return c[0]
	}
	if p >= 1 {
		return c[len(c)-1]
	}
	idx := int(math.Ceil(p*float64(len(c)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(c) {
		idx = len(c) - 1
	}
	return c[idx]
}

// MarginRow is one line of the safety-margin decision table.
type MarginRow struct {
	RatioCovered float64 `json:"ratio_covered"` // real/estimated this margin survives
	ImpliedS     int     `json:"implied_margin"`
	InputBudget  int     `json:"input_budget"`  // ctx - maxOut - S
	BudgetCostPct float64 `json:"budget_cost_pct"` // budget lost vs S=0
}

// ImpliedMargin solves S >= (C - M)(1 - 1/r): the smallest safety margin that
// still fits when the real token count is r times the ladder's estimate.
//
// Derivation (kept here because the constant is otherwise unfalsifiable): the
// ladder compacts until EST <= C - M - S; the server then sees REAL = r*EST;
// the request fits when REAL + M <= C.
func ImpliedMargin(r float64, ctxTokens, maxOut int) int {
	room := ctxTokens - maxOut
	if room <= 0 || r <= 1 {
		return 0
	}
	s := float64(room) * (1 - 1/r)
	return int(math.Ceil(s))
}

// CoveredRatio inverts ImpliedMargin: the largest estimator error a given
// margin survives. (S=512 with ctx 8192 / maxOut 1024 covers r<=1.077.)
func CoveredRatio(s, ctxTokens, maxOut int) float64 {
	room := ctxTokens - maxOut
	if room <= 0 || s <= 0 {
		return 1
	}
	if s >= room {
		return math.Inf(1)
	}
	return float64(room) / float64(room-s)
}

// MarginTable builds the operator-facing decision table. It deliberately
// returns a TABLE, not a recommendation: raising the margin buys safety by
// unconditionally shrinking usable context on every request, and that is the
// operator's trade to make.
func MarginTable(ratios []float64, ctxTokens, maxOut int) []MarginRow {
	room := ctxTokens - maxOut
	out := make([]MarginRow, 0, len(ratios))
	for _, r := range ratios {
		s := ImpliedMargin(r, ctxTokens, maxOut)
		budget := room - s
		cost := 0.0
		if room > 0 {
			cost = math.Round(float64(s)/float64(room)*1000) / 10
		}
		out = append(out, MarginRow{RatioCovered: r, ImpliedS: s, InputBudget: budget, BudgetCostPct: cost})
	}
	return out
}

// --- offline transcript facts ------------------------------------------------

// DivergenceIndex returns the index of the first message where cur differs
// from prev, or -1 when cur is a pure APPEND of prev (the case that reuses the
// KV cache). Message-level is the right granularity: the ladder edits whole
// message bodies.
func DivergenceIndex(prev, cur []agent.Msg) int {
	n := len(prev)
	if len(cur) < n {
		n = len(cur)
	}
	for i := 0; i < n; i++ {
		if !sameMsg(prev[i], cur[i]) {
			return i
		}
	}
	if len(cur) < len(prev) {
		return len(cur) // truncation: diverges at the new end
	}
	return -1
}

// renderWire approximates the BYTE stream the server tokenizes — role framing,
// content, and the tool-call envelope. KV reuse is a property of that stream,
// not of message identity: appending to one message's content extends the
// prefix even though the message object changed. Approximate on purpose (the
// real chat template is the server's), and used only for prefix ANNOTATION.
// Fields are LENGTH-PREFIXED, which makes the encoding injective: no payload
// can forge a field boundary. That matters because PrefixRelation now gates
// the eviction exclusion — with plain separators, a tool result containing the
// separator byte could render identically to a genuinely different transcript
// and silently delete a divergent row.
//
// Appending whole MESSAGES still preserves the byte prefix (earlier messages'
// encodings are untouched), which is the only shape the agent loop produces.
// Growing an existing message's CONTENT reads as divergent — correct, because
// the chat template writes an end-of-turn marker straight after the content,
// so extending it diverges on the wire too.
func renderWire(msgs []agent.Msg) string {
	var b strings.Builder
	field := func(s string) {
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteByte(':')
		b.WriteString(s)
	}
	for _, m := range msgs {
		field(m.Role)
		field(m.ToolCallID)
		field(m.Content)
		b.WriteString(strconv.Itoa(len(m.ToolCalls)))
		b.WriteByte('|')
		for _, c := range m.ToolCalls {
			field(c.ID)
			field(c.Name)
			field(c.Args)
		}
	}
	return b.String()
}

// PrefixRelation classifies how a request relates to the one before it — the
// distinction that decides whether the KV cache survives. Measured on this
// stack: EXTENSION and TRUNCATION reuse; DIVERGENT collapses the whole cache
// (a sliding-window model cannot truncate mid-sequence and continue).
type PrefixRelation string

const (
	RelExtension PrefixRelation = "extension" // previous stream is a prefix of this one (or identical)
	RelTruncation PrefixRelation = "truncation" // this stream is a strict prefix of the previous
	RelDivergent  PrefixRelation = "divergent"  // they part company mid-stream
	RelNone       PrefixRelation = "none"       // nothing cached yet
)

// Relation classifies prev→cur, reporting how many bytes they share and what
// fraction of the previous stream survived.
//
// MEASURED RULE (gemma-4-e4b, llama.cpp b50, --cache-reuse unset, 2026-07-24):
// reuse is BINARY, not proportional. An EXTENSION reuses everything the server
// already held — appending a turn to a 2429-token prompt reused 2429 and
// prefilled only the 19 new tokens, corroborated by ~80 append steps per arm
// reusing a median 0.88 of their (larger) prompts. ANY divergence discards the
// WHOLE cache regardless of how much was shared: a position sweep editing one
// line at 4%, 17%, 33%, 50%, 62%, 75%, 83%, 92% and 98% of a 120-line prompt
// returned cache_n of 1,1,1,1,1,0,0,0,0 — even a divergence two lines from the
// end cost all 2662 tokens. SharedFrac is therefore recorded as evidence, NOT
// as a predictor: on this stack only the relation matters.
//
// This is why the compaction ladder cannot be "KV-friendly" by being monotonic:
// it edits older turns, and any edit at all is a full re-prefill.
func Relation(prev, cur []agent.Msg) (rel PrefixRelation, sharedChars int, sharedFrac float64) {
	if len(prev) == 0 {
		return RelNone, 0, 0
	}
	p, c := renderWire(prev), renderWire(cur)
	shared := commonPrefixLen(p, c)
	frac := 0.0
	if len(p) > 0 {
		frac = float64(shared) / float64(len(p))
	}
	switch {
	case shared == len(p):
		return RelExtension, shared, frac
	case shared == len(c):
		return RelTruncation, shared, frac
	default:
		return RelDivergent, shared, frac
	}
}

func commonPrefixLen(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func sameMsg(a, b agent.Msg) bool {
	if a.Role != b.Role || a.Content != b.Content || a.ToolCallID != b.ToolCallID || len(a.ToolCalls) != len(b.ToolCalls) {
		return false
	}
	for i := range a.ToolCalls {
		if a.ToolCalls[i] != b.ToolCalls[i] {
			return false
		}
	}
	return true
}

// SameTranscript reports byte-for-byte equality of two message slices.
func SameTranscript(a, b []agent.Msg) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameMsg(a[i], b[i]) {
			return false
		}
	}
	return true
}

// charsOf is the payload size the estimator sees.
func charsOf(msgs []agent.Msg) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
		for _, c := range m.ToolCalls {
			n += len(c.Name) + len(c.Args)
		}
	}
	return n
}

// wellFormed reports whether a transcript prefix can be sent as-is: every
// assistant tool_call has its matching tool result. A prefix cut between an
// assistant tool-call and its result is not a state the loop ever sends, and
// strict chat templates reject it.
func wellFormed(msgs []agent.Msg) bool {
	pending := map[string]bool{}
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			pending[c.ID] = true
		}
		if m.Role == "tool" {
			delete(pending, m.ToolCallID)
		}
	}
	return len(pending) == 0
}

// trimFloat formats a float without trailing zeros (report cosmetics only).
func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
