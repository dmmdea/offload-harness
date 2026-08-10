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
// error, timeout, empty output, output shorter than the input, or a dropped
// "double-quoted" text span — the render proceeds with the RAW prompt and the
// fallback reason is recorded (result data + server log). Refinement must
// never make a render fail.
package pipeline

import (
	"context"
	"log"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// refinerSystem is the refiner's system prompt. The quoted-span rule is stated
// to the model AND enforced in code (missingQuotedSpan): quoted spans are how
// callers request literal rendered text — the model family's whole selling
// point — so a refiner that rewrites one is discarded, not trusted.
const refinerSystem = `You are a professional photography prompt engineer for a text-to-image model. Expand the user's image prompt with concrete photographic detail: lighting, composition, materials, mood, and lens vocabulary. NEVER change the subject of the prompt. NEVER add, remove, or alter any "double-quoted" text span — reproduce each one verbatim, quotes included. Return ONLY the refined prompt as plain text, with no preamble, no explanation, and no surrounding quotation marks.`

const (
	refinerMaxTokens      = 256
	refinerTemperature    = 0.4
	refinerDefaultTimeout = 30 * time.Second
)

// quotedSpanRe matches a complete "double-quoted" span, quotes included.
// Straight double quotes only: apostrophes and single quotes are prose, not
// text-render spans, and an unpaired trailing quote matches nothing.
var quotedSpanRe = regexp.MustCompile(`"[^"]*"`)

// missingQuotedSpan returns the first "double-quoted" span of raw (quotes
// included) that does not appear verbatim in refined, or "" when every span is
// preserved.
func missingQuotedSpan(raw, refined string) string {
	for _, span := range quotedSpanRe.FindAllString(raw, -1) {
		if !strings.Contains(refined, span) {
			return span
		}
	}
	return ""
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

// maybeRefinePrompt is the shared refinement decision point for every image
// prompt surface. It returns the prompt the RENDER should receive, whether it
// was refined, and — when a configured refiner fell back — the reason. With no
// refiner configured it returns the raw prompt untouched and callers add
// nothing to the result data, keeping the OFF path byte-identical.
func (p *Pipeline) maybeRefinePrompt(ctx context.Context, raw string, explicitOff bool) (prompt string, refined bool, note string) {
	if p.cfg.ImageGenRefinerModel == "" || explicitOff {
		return raw, false, ""
	}
	out, ferr := p.refinePrompt(ctx, raw)
	if ferr != "" {
		log.Printf("imagegen prompt refiner: falling back to the raw prompt (%s)", ferr)
		return raw, false, ferr
	}
	return out, true, ""
}

// refinePrompt performs one guarded refiner call. It returns the refined
// prompt, or "" plus a non-empty fallback reason. Every guard here exists so a
// refiner can only ever improve a render, never break one.
func (p *Pipeline) refinePrompt(ctx context.Context, raw string) (string, string) {
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
		return "", "refiner call failed: " + err.Error()
	}
	refined := strings.TrimSpace(gres.Content)
	if refined == "" {
		return "", "empty refiner output"
	}
	// A "refinement" shorter than the input is not an expansion — the model
	// misunderstood the job (or truncated into uselessness). Rune count, not
	// bytes: prompts are not always ASCII.
	if utf8.RuneCountInString(refined) < utf8.RuneCountInString(raw) {
		return "", "refined prompt shorter than the input"
	}
	if span := missingQuotedSpan(raw, refined); span != "" {
		return "", "refined prompt dropped quoted span " + span
	}
	return refined, ""
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
