// The FLUX 3 Image Klein prompt upsampler (imagegen_upsampler).
//
// BFL ships the model with one rule: "Do not use the model without a prompt upsampler." It takes full scene captions, and
// a short prompt sent as typed gives generic output. Their 9B upsampler, or any LLM given their t2i system prompt, writes
// that caption. On the ComfyUI engine the flux3-klein graph runs BFL's own upsampler file in-graph (render/wf-flux3-klein.mjs),
// so this side only passes the file and keeps the node refiner off; on the sd.cpp engine there is no graph, so the harness
// calls the llama-swap model the binding names with BFL's t2i system prompt (flux3_klein_t2i_system.txt, verbatim from
// black-forest-labs/flux-3-image-klein-pup system_prompts.json, Apache-2.0; render/flux3-klein-system-prompts.json is the
// runner's copy) before the media lease, the way the refiner runs, so the text call never contends with the render.
//
// Fail-safe like the refiner: a transport error, a timeout, an empty or truncated answer or a dropped "double-quoted" span
// renders the raw prompt and records why. Unlike the refiner it may ADD quoted text: BFL's prompt tells the upsampler to
// author the wording a request needs and quote it, which is how the model renders text.
package pipeline

import (
	"context"
	_ "embed"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"llamaswap-pp-cli/pkg/llamaswap"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

//go:embed flux3_klein_t2i_system.txt
var flux3KleinT2ISystem string

const (
	// upsamplerMaxTokens is BFL's own budget (pup.py SAMPLING max_tokens); a caption is typically 100 to 350 words.
	upsamplerMaxTokens = 3584
	// upsamplerTimeout covers a cold llama-swap load of the model and a few hundred tokens on a small card.
	upsamplerTimeout = 300 * time.Second
)

// comfyUpsampler is the upsampler file the ComfyUI flux3-klein graph runs in-graph, "" for every other binding and for the model form.
func comfyUpsampler(cfg config.Config) string { return cfg.UpsamplerFile() }

// llmUpsampler is the llama-swap model the harness calls for a flux3-klein caption before the media lease (either engine), "" otherwise.
func llmUpsampler(cfg config.Config) string { return cfg.UpsamplerModel() }

// harnessUpsample is the model form's caption step, shared by both engines: BFL's system prompt on the bound model, before the lease, the
// raw prompt on any problem. It then unloads the model (releaseUpsampler), on both engines: the upsampler is a 4-9B seat
// that usually sits on a card the render then leases (<node-b>'s PUP seat shares the 5070 Ti with the multicard compute side), and a
// render run inside an inherited `gpu reserve` lease gets no second unload pass, so a seat left loaded would share the compute card with
// the render. A reload costs seconds (the PUP Q6_K: 7.6 s).
func (p *Pipeline) harnessUpsample(ctx context.Context, model, prompt string) (caption string, upsampled bool, note string) {
	caption, note = p.upsamplePrompt(ctx, model, prompt)
	if caption == "" {
		log.Printf("imagegen upsampler: rendering the raw prompt (%s)", note)
		caption = prompt
	} else {
		upsampled = true
	}
	p.releaseUpsampler(ctx, model)
	return caption, upsampled, note
}

// releaseUpsampler unloads the upsampler model once its caption is in, before the render asks for the media lease. An sd.cpp
// node has one device, so the render's own unload (GPU_LEASE_UNLOAD_MODELS) would take this model off it anyway, but only
// after the lease's host-RAM admission, which counts the model as still resident: on <node-c> the 4B seat's --cache-ram
// 8192 held 8 GiB of commit, and a 6.5 GiB render waited for memory its own unload would have freed (2026-10-24). Doing the
// unload here moves it ahead of that admission and changes nothing else. It drains first (the model may be another
// caller's seat, e.g. Hermes on the 4B), a seat the llama-swap config marks resident is refused by the client, and any
// failure only costs the wait it was meant to save, so it is logged and the render goes on.
func (p *Pipeline) releaseUpsampler(ctx context.Context, model string) {
	if strings.TrimSpace(p.cfg.Endpoint) == "" {
		return
	}
	ls, err := swapclient.New(p.cfg.Endpoint, 10*time.Second)
	if err == nil {
		_, err = ls.Unload(ctx, model, &llamaswap.UnloadOpts{Drain: true, DrainTimeout: 30 * time.Second})
	}
	if err != nil {
		log.Printf("imagegen upsampler: %s left loaded (%v); the render's own unload takes it after the lease's host-RAM check", model, err)
	}
}

// upsamplePrompt asks model for BFL's full scene caption of raw. It returns the caption, or "" and why it fell back.
func (p *Pipeline) upsamplePrompt(ctx context.Context, model, raw string) (caption, note string) {
	uctx, cancel := context.WithTimeout(ctx, upsamplerTimeout)
	defer cancel()
	gres, err := p.client.Generate(uctx, model, flux3KleinT2ISystem, raw, "", upsamplerMaxTokens, 0, 0, llamaclient.WithoutThinking())
	if err != nil {
		reason := "upsampler call failed: " + err.Error()
		if errors.Is(err, context.DeadlineExceeded) || uctx.Err() == context.DeadlineExceeded {
			reason += " — cold model swap?"
		}
		return "", reason
	}
	out := strings.TrimSpace(gres.Content)
	switch {
	case out == "":
		return "", "empty upsampler output"
	case gres.Truncated:
		return "", "upsampler output truncated (budget)"
	}
	out = stripWrappingQuotes(out, raw)
	if reason := spanGuard(raw, out); reason != "" {
		return "", reason
	}
	return out, ""
}

// readCaption reads the caption the ComfyUI runner kept beside a picture (<name>.caption.txt, render/comfy-render.mjs captionPath);
// "" when there is none.
func readCaption(imagePath string) string {
	b, err := os.ReadFile(strings.TrimSuffix(imagePath, filepath.Ext(imagePath)) + ".caption.txt")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// addUpsampleData annotates a generate_image result with the upsampler outcome. Keys appear only on a binding that names an
// upsampler, so every other result stays as it was.
func addUpsampleData(data map[string]any, upsampler string, upsampled bool, caption, note string) {
	if upsampler == "" {
		return
	}
	data["upsampler"] = upsampler
	data["upsampled"] = upsampled
	if upsampled && caption != "" {
		data["upsampled_prompt"] = caption
	} else if note != "" {
		data["upsample_fallback"] = note
	}
}
