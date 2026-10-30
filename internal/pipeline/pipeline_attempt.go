// The attempt ladder: attempt, the reasoning attempts, and the RunTier entry points.

package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/confhead"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/exemplars"
	"github.com/dmmdea/offload-harness/internal/gbnf"
	"github.com/dmmdea/offload-harness/internal/grounding"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/parser"
	"github.com/dmmdea/offload-harness/internal/tasks"
	"github.com/dmmdea/offload-harness/internal/verifier"
)

// attempt runs the grammar+retry loop for ONE model tier. It returns the result
// and whether a quality failure could plausibly be fixed by a larger tier
// (escalatable). Infra failures return escalatable=false (defer straight out).
// Success is cached + recorded here; a defer is NOT recorded (Run records the
// final one once, so escalation does not double-count).
//
// record gates ALL persistent side-effects on a successful result: the savings
// ledger, the cache write, the shadow-queue capture, and the exemplar harvest.
// Pass true for normal Run calls; pass false for shadow/counterfactual RunTier
// calls that must produce a gradeable result but write NO production side-effects.
//
// entryChars is the ENTRY view's input length: req.Input may hold a TO-3
// repacked tier view, but input_chars is a trained confhead feature (loginput)
// whose label stream is entry-scale, so every recorded row keeps the entry
// semantics (round-1 review finding 2026-08-14).
func (p *Pipeline) attempt(ctx context.Context, req core.Request, built tasks.Built, ck, model string, meta core.Meta, start time.Time, record bool, entryChars int, opts ...llamaclient.GenOption) (core.Result, bool) {
	attempts := p.cfg.MaxRetries + 1
	if attempts < 1 {
		attempts = 1
	}
	// A seat whose runtime cannot constrain decoding (config unconstrained_seats) gets the
	// prompt-carried shape and no constraint field at all; see unconstrained.go. Decided once,
	// before the prompt is used, so every retry sends the same thing.
	unconstrained := p.unconstrainedFor(model, built)
	if unconstrained {
		built = built.ForUnconstrained()
		meta.PromptPrefixSHA256 = promptFingerprintSent(built, req.Input)
	}
	user := built.User
	var lastContent string

	// triage/classify carry a single decision token whose raw logprob margin is a
	// genuine-uncertainty signal; request top logprobs for those tasks only. An unconstrained
	// seat ignores logprobs, so none are requested and the margin gate stays inert there.
	topLP := 0
	if !unconstrained && (req.Task == core.TaskTriage || req.Task == core.TaskClassify) {
		topLP = 10
	}

	// Phase 4: a health-derived per-tier timeout (P95×2), if one was learned.
	actx := ctx
	if ov := p.overridesSnap(); ov != nil {
		if ms, ok := ov.TierTimeoutsMs[model]; ok && ms > 0 {
			var cancel context.CancelFunc
			actx, cancel = context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
			defer cancel()
		}
	}

	// D-129: a seat this box declares as vLLM (directly or through its
	// llama-swap alias) is constrained by vLLM's OWN field — it accepts and
	// DISCARDS llama.cpp's `grammar`, so this tier was answering unconstrained
	// and paying for it downstream (1,018 re-packs on vLLM seats over nine
	// days against 506 on every other seat). Resolved ONCE per attempt loop,
	// not per retry: the answer is memoized anyway, and a retry must not be
	// able to change engines mid-call.
	//
	// WithoutThinking rides along because vLLM applies the constraint to the
	// WHOLE output: a thinking seat would have to emit its think block inside
	// the JSON schema, which is unsatisfiable. The reasoning tier's trick of
	// putting the think span INSIDE the grammar (gbnf.WrapThinking) is
	// llama.cpp-only for exactly this reason, so a vLLM rung asks for the
	// non-thinking render instead.
	// The render this adds is a function of the MODEL, which every cache key
	// on this path already carries, so it cannot serve one render's answer to
	// the other's caller the way RenderKey exists to prevent.
	grammar, genOpts := built.Grammar, opts
	if !unconstrained && len(built.Fields) > 0 && p.isVLLMSeat(ctx, model) {
		grammar = ""
		// A fresh slice, never append-in-place: opts is the CALLER's variadic
		// array, and growing it under a spare cap would leak this call's
		// options into the caller's next one.
		genOpts = make([]llamaclient.GenOption, 0, len(opts)+2)
		genOpts = append(genOpts, opts...)
		genOpts = append(genOpts, llamaclient.WithJSONSchema(gbnf.JSONSchema(built.Fields)), llamaclient.WithoutThinking())
	}

	for i := 0; i < attempts; i++ {
		meta.Retries = i
		gen, gerr := p.client.Generate(actx, llamaclient.GenRequest{Model: model, System: built.System, User: user, Grammar: grammar, MaxTokens: built.MaxTokens, Temperature: p.cfg.Temperature, TopLogprobs: topLP}, genOpts...)
		if gerr != nil {
			meta.LatencyMs = time.Since(start).Milliseconds()
			meta.ErrClass = classifyErr(gerr)
			return core.Deferf("model call failed: "+gerr.Error(), lastContent, meta), false
		}
		lastContent = gen.Content
		meta.TokensIn = gen.TokensIn
		meta.TokensOut = gen.TokensOut
		meta.TokPerSec = gen.TokPerSec
		meta.Truncated = gen.Truncated

		data, perr := extractReply(gen.Content, unconstrained)
		v := verifier.Check(gen.Content, gen.Truncated, perr)
		if v.OK {
			if verr := validateReply(data, built); verr != nil {
				v = verifier.Verdict{Retry: true, Reason: "schema: " + verr.Error()}
				meta.EscSource = core.EscSchema
			} else if g, ok := grounding.Check(req.Task, req.Input, data); ok {
				// Phase 1 quality eval. Log grounded for the calibration label; act
				// (retry/escalate) ONLY on extract — extraction is verbatim, so a
				// value not in source is a real error. Summarize grounding is noisier
				// (paraphrase), so it's recorded but not actioned.
				meta.Grounded = &g
				if !g && req.Task == core.TaskExtract {
					reason := "ungrounded extract (values not in source)"
					if bad, okf := grounding.CheckFields(req.Task, req.Input, data); okf && len(bad) > 0 {
						reason = "ungrounded extract fields: " + strings.Join(bad, ", ")
					}
					v = verifier.Verdict{Retry: true, Reason: reason}
					meta.EscSource = core.EscGrounding
				}
			}
			if v.OK {
				reason, mi, src, low := p.confidenceGate(req, data, gen.Logprobs)
				mi.apply(&meta)
				// Confhead correctness gate (opt-in, ADOPT tasks only): if the head
				// predicts a low p(correct) for this call, treat it as low-confidence
				// so Run escalates to a larger tier. Only fires when (a) enabled + head
				// loaded, (b) the task has a learned threshold, and (c) a larger tier
				// exists to escalate to (never on the escalation tier itself — the head
				// does not model it). Never touches grammar.
				// P1 no torn read: snapshot the head AND its thresholds together
				// under one RLock, then use ONLY these two locals for the gate. A
				// concurrent reload that swaps both can never yield a crossed
				// (old-head, new-thresholds) pair here.
				chHead, chThr := p.confheadSnap()
				// A single-model cascade (escalation_model empty or the workhorse itself) has nowhere to
				// escalate to, so the gate below is skipped. Record the gap on the call's ledger row (one
				// key, idempotent across attempts) so it is never a silent same-weights "escalation".
				if !low && chHead != nil && len(chThr) > 0 && (p.cfg.EscalationModel == "" || p.cfg.EscalationModel == p.cfg.Model) && meta.Feat != nil {
					meta.Feat[core.FeatEscalationUnavailable] = 1
				}
				if !low && chHead != nil && len(chThr) > 0 && p.cfg.EscalationModel != "" && model != p.cfg.EscalationModel {
					if tau, ok := chThr[string(req.Task)]; ok {
						// entryChars, NOT len(req.Input): the head's loginput
						// feature is trained on entry-scale label rows, and a
						// repacked mid-chain view would be served
						// out-of-distribution (round-1 review finding).
						e := entryFrom(req.Task, meta, false, entryChars)
						pc := chHead.Predict(string(req.Task), confhead.FeatureRow(e))
						if pc >= 0 && pc < tau {
							low = true
							reason = fmt.Sprintf("low confhead p(correct)=%.3f < threshold %.3f", pc, tau)
							src = core.EscConfhead
						}
					}
				}
				if low {
					meta.LatencyMs = time.Since(start).Milliseconds()
					meta.EscSource = src
					// The escalating attempt is a call the seat ANSWERED: its own
					// row — this tier, this margin, the gate that sent it up —
					// goes to the ledger before the defer (register D-127). Until
					// this, the defer returned first and the only trace of a
					// margin escalation was the successor's row (esc_source
					// carried, margin 0) plus the labels sidecar: 4 rows in
					// 9,217 for 43 real firings. Same record gate as a success
					// (counterfactual RunTier calls write nothing).
					if record {
						// The row is a step of the call, not the call (plan D12):
						// an inner row of the row the tier that finally answers
						// writes. The id is minted here, on the first climb, so a
						// call that never climbs writes the same plain row as ever.
						// A call already inner (a composite's sub-call) stays inner
						// under its composite's id: rows never nest.
						row := meta
						if row.ParentJobID == "" {
							if meta.JobID == "" {
								meta.JobID = mintJobID("cs")
							}
							row.ParentJobID, row.JobID = meta.JobID, ""
						}
						p.record(req.Task, row, entryChars)
						// The inner row's cards_tokens is written 0: the row of the tier
						// that answers carries this attempt's work (Meta.CardsCarried).
						meta.CardsCarried += ledger.CardsTokensOf(entryFrom(req.Task, row, false, entryChars))
					}
					// a larger, more decisive tier may clear the threshold
					return core.Deferf(reason, gen.Content, meta), true
				}
			}
		}

		if v.OK {
			meta.LatencyMs = time.Since(start).Milliseconds()
			// record gates ALL persistent side-effects: ledger, cache, shadow queue,
			// and exemplar harvest. Pass record=false for counterfactual RunTier calls
			// that must produce a gradeable result without any production side-effects.
			if record {
				if p.cache != nil && ck != "" {
					if b, e := json.Marshal(cacheVal{Data: data, TokensIn: gen.TokensIn, Model: meta.Model, InLoop: p.tierCache}); e == nil {
						_ = p.cache.Put(ck, b)
					}
				}
				p.record(req.Task, meta, entryChars)
				// Phase A.3: sampled shadow-queue capture (non-escalated classify/triage/extract; config-gated, off by default).
				p.captureShadow(req, entryFrom(req.Task, meta, false, entryChars), core.Result{OK: true, Data: data, Meta: meta})
				// Phase 6: harvest a verified-good (input, output) exemplar for the sidecar.
				// ENTRY rows only (TierPack empty): a climbed tier's req.Input may be
				// the unbounded original (sidecar bloat) or a cut carrying
				// repackMarker, which future prompts would re-inject as few-shot
				// CONTENT (round-1 review finding).
				// Params that cannot be keyed (tasks.CacheParamsKey) are not harvested either:
				// the row's StableParamsKey prints them with %v, which has no cycle guard, so
				// a params value that holds itself would kill the process here.
				if p.cfg.ExemplarsDir != "" && p.goodExemplar(meta) && meta.TierPack == "" && paramsKeyable(req.Params) {
					_ = exemplars.Append(p.cfg.ExemplarsDir, string(req.Task), tasks.StableParamsKey(req.Params), req.Input, data, meta.Margin)
				}
			}
			return core.Result{OK: true, Data: data, Meta: meta}, false
		}

		if v.Retry && i < attempts-1 {
			user = built.User + "\n\nYour previous reply was rejected (" + v.Reason + "). Output ONLY a single valid JSON object with the exact required fields and nothing else."
			continue
		}
		meta.LatencyMs = time.Since(start).Milliseconds()
		// Schema/grounding already stamped their own source above; anything
		// still unattributed here failed parse/verify.
		if meta.EscSource == core.EscNone {
			meta.EscSource = core.EscVerifier
		}
		// A terminal failure (e.g. truncation — input too large for ANY local
		// tier) defers straight to Opus; escalating would just burn the slow 26B.
		return core.Deferf(v.Reason, gen.Content, meta), !v.Terminal
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	meta.EscSource = core.EscRetries
	return core.Deferf("exhausted retries", lastContent, meta), true
}

// reasoningThinkBudget is extra generation budget granted to the reasoning tier on top of a
// task's native budget, so the grammar-forced <think> span has room before the JSON answer.
const reasoningThinkBudget = 512

// attemptReasoning is the terminal LOCAL tier for grammar tasks: a thinking model reasons
// under a think-wrapped grammar (gbnf.WrapThinking), the <think> span is stripped, then the
// SAME verify + validate + grounding gates as attempt() run. It is deliberately simpler than
// attempt — no retries and no confidence-escalation gate (there is no larger local tier to
// escalate to; a valid answer here reclaims a cloud deferral, an invalid one falls through to
// the normal defer-to-Opus). Returns (result, ok). On ok the result is recorded + cached; a
// defer is NOT recorded (Run records the final one once).
func (p *Pipeline) attemptReasoning(ctx context.Context, req core.Request, built tasks.Built, ck string, meta core.Meta, start time.Time, entryChars int, opts ...llamaclient.GenOption) (core.Result, bool) {
	return p.attemptReasoningOn(ctx, p.cfg.ReasoningModel, req, built, ck, meta, start, entryChars, opts...)
}

// attemptReasoningOn is the terminal reasoning attempt on a given model: the
// configured reasoning tier, or the loaded vLLM seat the cascade seat guard
// substitutes for it when the reasoning tier's load would evict that seat.
// Either way it is ONE attempt with the reasoning budget, marked Reasoning,
// with no confidence gate. The think span differs by engine: llama.cpp gets
// the think-wrapped GBNF; a vLLM seat cannot take it — it discards `grammar`
// and constrains the WHOLE output (ADR 0002, 2026-09-18 amendment) — so it
// gets its own structured_outputs and the non-thinking render, and the extra
// budget is answer headroom there.
func (p *Pipeline) attemptReasoningOn(ctx context.Context, model string, req core.Request, built tasks.Built, ck string, meta core.Meta, start time.Time, entryChars int, opts ...llamaclient.GenOption) (core.Result, bool) {
	meta.Model = model
	meta.Reasoning = true // tag every reasoning-tier outcome so a reclaim is distinguishable from an escalation answer (same model)
	unconstrained := p.unconstrainedFor(model, built)
	if unconstrained {
		built = built.ForUnconstrained()
		meta.PromptPrefixSHA256 = promptFingerprintSent(built, req.Input)
	}
	grammar, genOpts := gbnf.WrapThinking(built.Grammar), opts
	if unconstrained {
		grammar = "" // nothing to wrap: the seat takes no grammar, and the shape rides in the prompt
	} else if len(built.Fields) > 0 && p.isVLLMSeat(ctx, model) {
		grammar = ""
		genOpts = make([]llamaclient.GenOption, 0, len(opts)+2)
		genOpts = append(genOpts, opts...)
		genOpts = append(genOpts, llamaclient.WithJSONSchema(gbnf.JSONSchema(built.Fields)), llamaclient.WithoutThinking())
	}
	// The wrapped grammar emits a <think> span BEFORE the JSON, so the task's native token
	// budget (classify=64, assess=128) would truncate the reasoning before any answer. Give the
	// think span headroom on top of the original budget.
	gen, gerr := p.client.Generate(ctx, llamaclient.GenRequest{Model: model, System: built.System, User: built.User, Grammar: grammar, MaxTokens: built.MaxTokens + reasoningThinkBudget, Temperature: p.cfg.Temperature}, genOpts...)
	if gerr != nil {
		meta.LatencyMs = time.Since(start).Milliseconds()
		meta.ErrClass = classifyErr(gerr)
		return core.Deferf("reasoning model call failed: "+gerr.Error(), "", meta), false
	}
	content := parser.StripThink(gen.Content)
	meta.TokensIn = gen.TokensIn
	meta.TokensOut = gen.TokensOut
	meta.TokPerSec = gen.TokPerSec
	meta.Truncated = gen.Truncated

	data, perr := extractReply(content, unconstrained)
	v := verifier.Check(content, gen.Truncated, perr)
	if v.OK {
		if verr := validateReply(data, built); verr != nil {
			v = verifier.Verdict{Reason: "schema: " + verr.Error()}
		} else if g, ok := grounding.Check(req.Task, req.Input, data); ok {
			meta.Grounded = &g
			if !g && req.Task == core.TaskExtract {
				v = verifier.Verdict{Reason: "ungrounded extract (values not in source)"}
			}
		}
	}
	// Classify self-confidence: honor the same accept/defer gate the cascade uses, so a
	// model-flagged-unsure classify answer defers (to Opus) rather than being accepted here.
	if v.OK && req.Task == core.TaskClassify {
		if conf, low := lowConfidence(data, p.cfg.ClassifyMinConfidence); low {
			v = verifier.Verdict{Reason: fmt.Sprintf("low classify confidence %.2f < %.2f", conf, p.cfg.ClassifyMinConfidence)}
		}
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	if !v.OK {
		return core.Deferf("reasoning tier: "+v.Reason, gen.Content, meta), false
	}
	if p.cache != nil && ck != "" {
		if b, e := json.Marshal(cacheVal{Data: data, TokensIn: gen.TokensIn, Model: meta.Model, InLoop: p.tierCache}); e == nil {
			_ = p.cache.Put(ck, b)
		}
	}
	p.record(req.Task, meta, entryChars) // input_chars keeps ENTRY semantics (see attempt)
	return core.Result{OK: true, Data: data, Meta: meta}, true
}

// RunTier runs req through exactly the named tier (bypassing modelChain), with
// the full quality gate (grammar/verify/validate/ground/confidence) that attempt
// applies. It records NOTHING to the savings ledger — used by the offline
// shadow-labeling flywheel to evaluate a counterfactual tier without polluting
// the savings stats. Returns the tier's result and whether it was accepted.
func (p *Pipeline) RunTier(ctx context.Context, req core.Request, model string) (core.Result, bool) {
	return p.RunTierWith(ctx, req, model)
}

// RunTierWith is RunTier with per-call generate knobs threaded to the tier
// request (0.115.18, register D-88): the in-loop offload closure passes
// WithoutThinking when the tier IS the planner seat, so a thinking seat renders
// the mechanical shape instead of spending the task's small budget inside a
// think block. RunTier keeps its option-free signature because four consumers
// (the NIM oracle, shadow labelling, the trajectory judge, the CLI) hold it as
// a func value.
func (p *Pipeline) RunTierWith(ctx context.Context, req core.Request, model string, opts ...llamaclient.GenOption) (core.Result, bool) {
	start := time.Now()

	built, err := tasks.Build(req)
	if err != nil {
		return core.Result{}, false
	}
	feat := featurize(req.Task, req.Input)
	// T2-D / T2-A: the key is built by cacheKeyForTier, which WRAPS the same
	// cacheKeyFor constructor Run uses inside RunTier's own keyspace — so the
	// ingredient list has one source and cannot drift, while the two paths can
	// never collide. Two corrections over the former hand-rolled cache.Key call:
	//
	//  1. It keys on the ACTUAL TIER (model), not p.cfg.Model. RunTier pins one
	//     named tier, so its answer belongs to that tier; keying on the primary
	//     model would let two different tiers share one entry — harmless while
	//     nothing read the key, fatal the moment anything did.
	//  2. It carries the template tag, so editing a task's prompt invalidates
	//     these entries exactly as it does the cascade's. The old call here had
	//     the pre-fix shape and was simply never exercised; reviving it as-is
	//     would have reinstated the stale-prompt bug on a brand-new path.
	//
	// ...and it lives in RunTier's OWN keyspace, so Run and RunTier can never
	// compute the same key and overwrite each other's entries. See
	// tierKeyspaceTag for why guarding only the read was not enough.
	var ck string // set below, once the call is known to use the cache and its params can be keyed
	meta := core.Meta{Model: model, Feat: feat}
	meta.InputSHA256 = inputFingerprint(req.Input)
	meta.PromptPrefixSHA256 = promptPrefixFingerprint(built.System, userPreambleOf(built.User, req.Input))
	meta.ContextHash = p.contextHash()

	// Cache participation is a property of THIS pipeline, never of RunTier itself.
	// The shadow-labelling flywheel calls RunTier on the MAIN pipeline (cache
	// open) to evaluate counterfactual tiers; serving those from cache — or
	// writing them — would corrupt the very measurement it exists to produce. Only
	// NewInLoopPipeline sets tierCache.
	useCache := p.tierCache && p.cache != nil
	if useCache {
		// The typed canonical params, as for Run. Params that cannot be keyed switch the
		// cache off for this call: no read, no write (resultCacheParamsKey says why).
		if paramsKey, keyable := p.resultCacheParamsKey(req.Task, req.Params, &meta); keyable {
			ck = cacheKeyForTierRender(req.Task, req.Input, paramsKey, model, built, llamaclient.RenderKey(opts...))
		} else {
			useCache = false
		}
	}
	if useCache {
		if raw, ok := p.cache.Get(ck); ok {
			var cv cacheVal
			// cv.Model == model is DEFENCE IN DEPTH and is unreachable by
			// construction today — say so rather than letting the comment assert a
			// live hazard that no longer exists.
			//
			// It was load-bearing when Run and RunTier shared a keyspace: a RunTier
			// call pinned to the workhorse was served whatever tier the cascade had
			// answered with (measured: the E2B triage tier), while meta.Model
			// reported the workhorse that never ran. Guarding the read fixed the
			// wrong answer but left both paths WRITING that key, so the real fix was
			// cacheKeyForTier's separate keyspace — and `model` is already an
			// ingredient of it, so nothing but a same-tier RunTier can reach here.
			//
			// Kept because it costs one comparison and fails CLOSED. Note a pre-0.63
			// entry cannot reach it either: tierKeyspaceTag is the first ingredient
			// of this key, so an old entry misses at the LOOKUP, not at this guard.
			// The only thing it can still catch is a hand-crafted or externally
			// written entry.
			if json.Unmarshal(raw, &cv) == nil && len(cv.Data) > 0 && cv.Model == model {
				meta.CacheHit = true
				meta.CacheHitInLoop = cv.InLoop
				meta.TokensIn = cv.TokensIn
				meta.LatencyMs = time.Since(start).Milliseconds()
				return core.Result{OK: true, Data: cv.Data, Meta: meta}, true
			}
		}
	}

	res, _ := p.attempt(ctx, req, built, ck, model, meta, start, false /* record=false: ledger/shadow/exemplars stay untouched */, len(req.Input), opts...)
	// escalatable ignored: RunTier never escalates.
	//
	// The Put lives here rather than inside attempt because `record` means "write
	// the SAVINGS-ACCOUNTING side-effects" (ledger, shadow queue, exemplar
	// harvest) and must stay false here — that is invariant (a) in
	// NewInLoopPipeline's doc. Caching is invariant (b), and only these two lines
	// grant it. A defer never reaches this point (res.OK is false), so a
	// low-confidence or ungrounded answer is never cached.
	if useCache && res.OK && len(res.Data) > 0 {
		// InLoop stamps the provenance: only NewInLoopPipeline sets tierCache, so
		// every entry written here came from the agent loop talking to itself,
		// whose generation was never costed in the savings ledger.
		if b, e := json.Marshal(cacheVal{Data: res.Data, TokensIn: res.Meta.TokensIn, Model: model, InLoop: true}); e == nil {
			_ = p.cache.Put(ck, b)
		}
	}
	return res, res.OK
}
