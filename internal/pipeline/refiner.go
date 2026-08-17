// Opt-in image-prompt refiner: when imagegen_refiner_model names a llama-swap
// text model, generate_image expands the raw prompt with concrete photographic
// detail on the LOCAL text tier before the render — the arena-scored
// prompt-refiner pattern, replicated in the harness prompt path for free.
//
// This file is the ONE place the refinement decision + guards live, shared by
// the single ComfyUI path, the sdcpp path, and the warm-batch path (the same
// drift class imageModelFromConfig deletes for the model binding: a prompt
// surface added later must call maybeRefinePrompt, never re-implement it).
//
// FAIL-SAFE is the load-bearing property: on ANY refiner problem — transport
// error, timeout, truncated or empty output, output shorter than the input, a
// dropped/altered "double-quoted" text span, or net-new quoted text — the
// render proceeds with the RAW prompt and the fallback reason is recorded
// (result data + server log). Refinement must never make a render fail.
package pipeline

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// refinerSystem is the refiner's system prompt. The quoted-span rules are
// stated to the model AND enforced in code, in BOTH directions: spans of the
// raw prompt must survive verbatim (spanGuard), and the refiner may not add
// new quoted text (the added-quote guard, after stripWrappingQuotes removes a
// whole-output wrap). Quoted spans are how callers request literal rendered
// text — the model family's whole selling point — so a refinement that touches
// them is discarded, not trusted.
//
// The closing no-quotes-when-none sentence is load-bearing, not style: with
// only the span rule stated, Gemma-class refiners wrap the SUBJECT itself in
// quotes ("a green backpack and a pig"), and the added-quote guard then
// rejects nearly every span-less refinement — the flag silently self-cancels
// on exactly the prompts it exists for. Measured on GenEval2 span-less
// prompts 2026-08-16: gemma-4-12b 2% -> 87% refine rate with the sentence,
// gemma-4-31b 0% without it; quoted-span prompts were unaffected (90-100%).
const refinerSystem = `You are a professional photography prompt engineer for a text-to-image model. Expand the user's image prompt with concrete photographic detail: lighting, composition, materials, mood, and lens vocabulary. NEVER change the subject of the prompt. NEVER add, remove, or alter any "double-quoted" text span — reproduce each one verbatim, quotes included. If the user's prompt contains no "double-quoted" text, your output must contain no quotation marks at all. Return ONLY the refined prompt as plain text, with no preamble, no explanation, and no surrounding quotation marks.`

const (
	refinerMaxTokens      = 256
	refinerTemperature    = 0.4
	refinerDefaultTimeout = 30 * time.Second
	// refinerMaxPromptChars skips refinement up front for prompts near or past
	// the completion budget: ~200 tokens at the rough ~4-chars/token English
	// average. Beyond this the 256-token budget cannot EXPAND the prompt — the
	// call could only ever truncate (which the truncation guard rejects), so it
	// would burn a model swap to fall back every time, with a reason blaming
	// the model instead of the budget.
	refinerMaxPromptChars = 800
	// refinerEscalateEvery: every Nth CONSECUTIVE process-level fallback logs
	// an escalated line naming the refiner alias, so a down tier is visible in
	// the server log without waiting for a health surface. Full health/
	// offload_status wiring for these counters is a recorded follow-up.
	refinerEscalateEvery = 5
	// refinerBreakerLimit: consecutive TRANSPORT/TIMEOUT-class fallbacks before
	// a batch stops calling the refiner for its remaining jobs (RunImageBatch).
	// Three in a row means the tier is down or swapping forever — burning the
	// per-call timeout on every remaining job would stall the batch for minutes
	// with zero GPU activity. Guard-class rejections never count.
	refinerBreakerLimit = 3
)

// normalizeQuotes maps curly double quotes (U+201C/U+201D) to straight ones so
// a pasted smart-quoted span gets the same protection as a typed "span" —
// guard-space only; the prompt sent to the render is never rewritten.
func normalizeQuotes(s string) string {
	return strings.NewReplacer("“", `"`, "”", `"`).Replace(s)
}

// quoteCount counts double-quote characters in normalized-quote space.
func quoteCount(s string) int {
	return strings.Count(normalizeQuotes(s), `"`)
}

// quotedSpans extracts the "double-quoted" spans of a prompt (normalized-quote
// space, quotes included) by pairing consecutive quote characters left to
// right. When the prompt holds an ODD number of quotes the LAST quote is
// dropped before pairing, so a trailing inch mark or stray quote after real
// spans (`see the "SALE" sign on a 5" pipe`) never pairs into a bogus span.
// KNOWN LIMITATION: an inch mark or stray quote BEFORE a real span still
// mis-pairs (`a 5" pipe with "STEEL" stamp` pairs the inch mark with the
// span's opening quote) — the guard then falls back to the raw prompt, which
// is safe but unrefined; the distinct dropped/altered reasons make that
// diagnosable from the ledger.
func quotedSpans(s string) []string {
	norm := normalizeQuotes(s)
	var idx []int
	for i := 0; i < len(norm); i++ {
		if norm[i] == '"' {
			idx = append(idx, i)
		}
	}
	if len(idx)%2 == 1 {
		idx = idx[:len(idx)-1]
	}
	spans := make([]string, 0, len(idx)/2)
	for i := 0; i+1 < len(idx); i += 2 {
		spans = append(spans, norm[idx[i]:idx[i+1]+1])
	}
	return spans
}

// collapseWS folds every whitespace run to a single space — the "whitespace
// differs" half of the altered-span diagnosis.
func collapseWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// spanGuard checks every quoted span of raw against refined, both in
// normalized-quote space, and returns "" when all spans survive verbatim or a
// distinct fallback reason: "altered (glyphs/whitespace)" when a span's inner
// text still appears but the quoting/whitespace around or inside it changed (a
// systematic normalizer in the refiner — diagnosable), else "dropped".
func spanGuard(raw, refined string) string {
	normRef := normalizeQuotes(refined)
	for _, span := range quotedSpans(raw) {
		if strings.Contains(normRef, span) {
			continue
		}
		inner := strings.TrimSpace(span[1 : len(span)-1])
		if inner != "" && strings.Contains(collapseWS(normRef), collapseWS(inner)) {
			return "quoted span altered (glyphs/whitespace): " + span
		}
		return "refined prompt dropped quoted span " + span
	}
	return ""
}

// stripWrappingQuotes removes ONE fully-wrapping double-quote pair (straight
// or curly) from refined — some models return their whole answer wrapped in
// quotes, which on this harness's quote-aware image families reads as a
// draw-this-text instruction. The strip is applied only when the interior
// still preserves every quoted span of raw, so a refined prompt that
// legitimately STARTS and ENDS with two distinct spans is never split.
func stripWrappingQuotes(refined, raw string) string {
	r := []rune(refined)
	if len(r) < 2 {
		return refined
	}
	isQ := func(c rune) bool { return c == '"' || c == '“' || c == '”' }
	if !isQ(r[0]) || !isQ(r[len(r)-1]) {
		return refined
	}
	inner := strings.TrimSpace(string(r[1 : len(r)-1]))
	if inner == "" || spanGuard(raw, inner) != "" {
		return refined
	}
	return inner
}

// refineExplicitlyOff reports whether the request param "refine" is EXPLICITLY
// false (JSON false, or the string "false"). Absent or any other value means
// the configured default applies — the knob only ever turns refinement off.
func refineExplicitlyOff(params map[string]any) bool {
	v, ok := params["refine"]
	if !ok {
		return false
	}
	switch t := v.(type) {
	case bool:
		return !t
	case string:
		return strings.EqualFold(t, "false")
	}
	return false
}

// stripRefineParam returns params without the "refine" knob, for OUTPUT-PATH
// hashing only: the knob selects prompt preprocessing, and the out path
// already derives from the RAW prompt regardless of refinement, so the knob's
// mere presence must not fork the default output path of an otherwise
// identical render — notably on refiner-less boxes where it is a no-op.
func stripRefineParam(params map[string]any) map[string]any {
	if _, ok := params["refine"]; !ok {
		return params
	}
	cp := make(map[string]any, len(params)-1)
	for k, v := range params {
		if k != "refine" {
			cp[k] = v
		}
	}
	return cp
}

// maybeRefinePrompt is the shared refinement decision point for every image
// prompt surface. It returns the prompt the RENDER should receive, whether it
// was refined, the fallback reason when a configured refiner fell back, and
// whether that fallback was TRANSPORT/TIMEOUT-class (the model never answered
// — what the batch circuit breaker counts; a guard rejection means the tier is
// alive and must not trip it). With no refiner configured it returns the raw
// prompt untouched and callers add nothing to the result data, keeping the OFF
// path byte-identical.
func (p *Pipeline) maybeRefinePrompt(ctx context.Context, raw string, explicitOff bool) (prompt string, refined bool, note string, transient bool) {
	if p.cfg.ImageGenRefinerModel == "" || explicitOff {
		return raw, false, "", false
	}
	out, ferr, trans := p.refinePrompt(ctx, raw)
	if ferr != "" {
		p.noteRefineFallback(ferr)
		return raw, false, ferr, trans
	}
	p.refineConsecutive.Store(0)
	return out, true, "", false
}

// noteRefineFallback records one refiner fallback on the process-level
// counters and logs it — with an escalated line every refinerEscalateEvery
// consecutive fallbacks, so a down refiner tier is loud in the server log.
func (p *Pipeline) noteRefineFallback(reason string) {
	total := p.refineFallbacks.Add(1)
	consec := p.refineConsecutive.Add(1)
	log.Printf("imagegen prompt refiner: falling back to the raw prompt (%s)", reason)
	if consec%refinerEscalateEvery == 0 {
		log.Printf("imagegen prompt refiner: %d consecutive fallbacks (%d total this process) — is the refiner tier %q reachable?",
			consec, total, p.cfg.ImageGenRefinerModel)
	}
}

// refinePrompt performs one guarded refiner call. It returns the refined
// prompt, or "" plus a non-empty fallback reason and whether the failure was
// transport/timeout-class. Every guard here exists so a refiner can only ever
// improve a render, never break one.
func (p *Pipeline) refinePrompt(ctx context.Context, raw string) (refined, note string, transient bool) {
	// Budget pre-check: refuse up front rather than truncating and blaming the
	// model (the truncation guard below would reject the output anyway).
	if len(raw) > refinerMaxPromptChars {
		return "", "prompt too long to refine (over the refiner's ~200-token budget)", false
	}
	timeout := time.Duration(p.cfg.ImageGenRefinerTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = refinerDefaultTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	gen := p.refineGen
	if gen == nil {
		gen = func(gctx context.Context, model, system, user string, maxTokens int, temperature float64) (llamaclient.GenResult, error) {
			return p.client.Generate(gctx, model, system, user, "", maxTokens, temperature, 0)
		}
	}
	gres, err := gen(rctx, p.cfg.ImageGenRefinerModel, refinerSystem, raw, refinerMaxTokens, refinerTemperature)
	if err != nil {
		reason := "refiner call failed: " + err.Error()
		// A deadline hit on an idle text tier is usually llama-swap loading the
		// model, not a sick tier — say so, mirroring the LO-9 cold-swap insight.
		if errors.Is(err, context.DeadlineExceeded) || rctx.Err() == context.DeadlineExceeded {
			reason += " — cold model swap?"
		}
		return "", reason, true
	}
	out := strings.TrimSpace(gres.Content)
	if out == "" {
		return "", "empty refiner output", false
	}
	// A truncated completion ends mid-sentence: even span-clean and longer than
	// the input it is not a trustworthy prompt.
	if gres.Truncated {
		return "", "refiner output truncated (budget)", false
	}
	out = stripWrappingQuotes(out, raw)
	// A "refinement" shorter than the input is not an expansion — the model
	// misunderstood the job. Rune count, not bytes: prompts are not always
	// ASCII.
	if utf8.RuneCountInString(out) < utf8.RuneCountInString(raw) {
		return "", "refined prompt shorter than the input", false
	}
	if reason := spanGuard(raw, out); reason != "" {
		return "", reason, false
	}
	// The reverse guard: net-new quoted text is a net-new draw-this-text
	// instruction (a whole-output wrap was already stripped above; whatever
	// quotes remain beyond the raw prompt's are the model's own addition).
	if quoteCount(out) > quoteCount(raw) {
		return "", "refined prompt added quote characters", false
	}
	return out, "", false
}

// addRefineData annotates a generate_image result's data map with the refiner
// outcome. Keys are added ONLY when a refiner model is configured, so an
// unconfigured box's result stays byte-identical to the pre-refiner harness.
func addRefineData(data map[string]any, refinerModel string, refined bool, refinedPrompt, note string) {
	if refinerModel == "" {
		return
	}
	data["refined"] = refined
	if refined {
		data["refined_prompt"] = refinedPrompt
	} else if note != "" {
		data["refine_fallback"] = note
	}
}
