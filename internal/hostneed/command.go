package hostneed

import (
	"math"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// RenderCall is a recognised render-helper invocation: `node render/comfy-generate.mjs ... --family
// krea2 --ckpt <file> ...`. Only the flags that select weights are read.
type RenderCall struct {
	Script string // the helper's lower-cased file name
	Route  Route
	Flags  map[string]string
}

// helperRoute maps the render helpers to the route whose files they load. comfy-run-graph.mjs has a route of its own
// (RouteRunGraph): it reads `--graph FILE` or `--template NAME --manifest FILE` as flags, and what it loads is what that graph or manifest
// names, never a family's files. The same helpers run in --graph mode (comfy-render, comfy-video) post the caller's own graph, handled in Estimate.
var helperRoute = map[string]Route{
	"comfy-generate.mjs":  RouteImage,
	"comfy-render.mjs":    RouteImage,
	"comfy-edit.mjs":      RouteEdit,
	"comfy-inpaint.mjs":   RouteInpaint,
	"comfy-video.mjs":     RouteVideo,
	"comfy-run-graph.mjs": RouteRunGraph,
}

// boolFlags take no value in the helpers' own parsers (comfy-render.mjs BOOL_FLAGS, comfy-video.mjs,
// and the `no-lock`/`keep-comfy` pair every runner shares): reading the next token as their value
// would swallow a real flag.
var boolFlags = map[string]bool{"no-lock": true, "keep-comfy": true, "no-lifecycle": true, "fast": true, "hero": true}

// ParseRenderCall finds a known helper in a wrapped command and reads its flags. The helper may sit
// behind an interpreter and its path in either separator style (`node D:\x\render\comfy-edit.mjs`).
func ParseRenderCall(args []string) (RenderCall, bool) {
	for i, a := range args {
		route, ok := helperRoute[scriptBase(a)]
		if !ok {
			continue
		}
		call := RenderCall{Script: scriptBase(a), Route: route, Flags: map[string]string{}}
		rest := args[i+1:]
		for j := 0; j < len(rest); j++ {
			tok := rest[j]
			if !strings.HasPrefix(tok, "--") {
				continue
			}
			name := strings.TrimPrefix(tok, "--")
			if k, v, ok := strings.Cut(name, "="); ok {
				call.Flags[k] = v
				continue
			}
			if boolFlags[name] || j+1 >= len(rest) || strings.HasPrefix(rest[j+1], "--") {
				call.Flags[name] = "true"
				continue
			}
			call.Flags[name] = rest[j+1]
			j++
		}
		return call, true
	}
	return RenderCall{}, false
}

// Estimate sizes the call's weights. A helper selects its weights by flag (or the environment the
// harness gave it), not by this machine's config, so the binding is built from the flags; the
// machine's config supplies only where the model files are (ComfyDir) and, for video, the family
// bindings the runner receives from the harness.
func (c RenderCall) Estimate(f Facts) (Need, bool) {
	// --reserve-vram is the runner's margin on every route, a graph call's included: the card it fits against is the card less it.
	if v := c.Flags["reserve-vram"]; v != "" {
		if r, err := strconv.ParseFloat(v, 64); err == nil && r > 0 {
			f.ReserveVRAMGiB = r
		}
	}
	// --graph POSTS THE CALLER'S OWN WORKFLOW as it is ("any nodes, any model, any pipeline",
	// comfy-render.mjs; comfy-video.mjs says the same): the family flags are not read, so the helper's
	// default family says nothing about what it loads. Sizing it as that family declared 0 for a 40 GiB
	// graph (SDXL fits the card) and the wrong family for a video one (review of 2026-10-10). What it
	// loads is what the graph's own loaders name: when the lease can read the graph and size every file
	// they name, that sum is the need (ForGraphFile); otherwise the call is not sized and the media
	// class default stands in, as it always did.
	if c.Route == RouteRunGraph {
		// A template run carries no graph file: what it loads is the model files of its runtime manifest (manifest.go). Both flags is a
		// contradiction the runner defers, so the template reading is the one that applies only when --graph is absent.
		if !c.RunsOwnGraph() && strings.TrimSpace(c.Flags["template"]) != "" {
			return ForManifestFile(c.Flags["manifest"], f)
		}
		return ForGraphFile(c.Flags["graph"], f)
	}
	if c.RunsOwnGraph() {
		return ForGraphFile(c.Flags["graph"], f)
	}
	cfg := f.Cfg
	switch c.Route {
	case RouteImage:
		cfg.ImageGenFamily = c.Flags["family"]
		cfg.ImageGenCkpt = firstNonEmpty(c.Flags["ckpt"], f.env("COMFY_CKPT"))
		cfg.ImageGenCLIP = c.Flags["clip"]
		// --placement multicard asks for the card-only recipe of the family (hostneed/multicard.go); --resident-gb moves its resident share.
		f = f.withPlacementFlags(c.Flags)
		return ForRoute(RouteImage, cfg, f)
	case RouteEdit:
		cfg.GenEditFamily = c.Flags["family"]
		cfg.GenEditUnet = firstNonEmpty(c.Flags["unet"], f.env("COMFY_EDIT_UNET"))
		cfg.GenEditCLIP = c.Flags["clip"]
		f = f.withPlacementFlags(c.Flags)
		if mp := megapixelsFlag(firstNonEmpty(c.Flags["megapixels"], f.env("COMFY_EDIT_MEGAPIXELS"))); mp > 0 {
			f.Megapixels = mp
		}
		return ForRoute(RouteEdit, cfg, f)
	case RouteInpaint:
		if strings.EqualFold(c.Flags["family"], "qwen") {
			// The DiffSynth inpaint patch rides a Qwen-Image DiT: the UNet and the Qwen2.5-VL encoder.
			files := []mediacap.ModelFile{{Label: "unet", Name: c.Flags["unet"], Classes: []string{"diffusion_models", "unet"}, Role: mediacap.RoleDiffusion}}
			if clip := c.Flags["clip"]; clip != "" {
				files = append(files, mediacap.ModelFile{Label: "clip", Name: clip, Classes: []string{"text_encoders", "clip"}, Role: mediacap.RoleTextEncoder})
			}
			if files[0].Name == "" {
				files = files[:0]
			}
			return estimate("qwen-inpaint", files, f)
		}
		cfg.InpaintCkpt = firstNonEmpty(c.Flags["ckpt"], f.env("COMFY_CKPT"))
		return ForRoute(RouteInpaint, cfg, f)
	case RouteVideo:
		// What the runner loads is what its FLAGS name (--transformer, --high-unet, --low-unet,
		// --text-encoder) and, for each one it is not given, the builder's default: the machine's config
		// reaches the runner only as the flags the pipeline passes, and a hand-run helper gets none. So
		// the binding is built from the flags alone (the way the image routes above build theirs), not
		// from the config's: sizing a bf16 --transformer as the configured int8 file under-declared it by
		// 19 GiB (review of 2026-10-10).
		model := videoFamily(firstNonEmpty(c.Flags["model"], "wan"))
		fb := config.VideoFamilyBinding{
			Transformer: c.Flags["transformer"], UnetHigh: c.Flags["high-unet"],
			UnetLow: c.Flags["low-unet"], TextEncoder: c.Flags["text-encoder"],
		}
		// The card-only placement (--wan-placement multicard) is read for the Wan and LTX-2.5 graphs alone: no other family has it.
		var cardOnly *CardOnly
		if strings.EqualFold(strings.TrimSpace(c.Flags["wan-placement"]), "multicard") {
			switch model {
			case "wan22":
				cardOnly = &CardOnly{ResidentGiB: wanResidentGiB(c.Flags), SageAttention: sageAttention(f.env("COMFY_EXTRA_ARGS"))}
			case "ltx25":
				cardOnly = &CardOnly{LTX: true, ResidentGiB: residentGiBOr(c.Flags, ltxResidentDefaultGiB)}
			}
		}
		return estimateWith(model, mediacap.VideoModelFilesFor(model, fb), f, cardOnly)
	}
	return Need{}, false
}

// sageAttentionFlag is the attention flag the film session of 2026-10-12 measured (ComfyUI's --use-sage-attention).
const sageAttentionFlag = "--use-sage-attention"

// sageAttention reports whether a COMFY_EXTRA_ARGS value carries sage attention, token by token, by the rule render/comfy-lifecycle.mjs isSageAttentionToken has (a
// test reads ONE table with the runner's test, render/testdata/sage-token-table.json): the exact flag, or an argparse prefix of it of at least 8 characters (ComfyUI's parser accepts an unambiguous
// abbreviation, `--use-sa`; `--use-s` also begins --use-split-cross-attention and is not this flag; a longer token is no prefix). Another attention flag is the
// default attention: only sage was measured.
func sageAttention(extra string) bool {
	for _, tok := range strings.Fields(extra) {
		if len(tok) >= 8 && strings.HasPrefix(sageAttentionFlag, tok) {
			return true
		}
	}
	return false
}

// wanResidentGiB reads --wan-resident-gb (the share of each Wan expert the card-only placement keeps on the compute card). The runner
// refuses a value it cannot read or one outside 0..64 before it starts, so sizing the call as the default one is only ever a sizing.
func wanResidentGiB(flags map[string]string) float64 {
	return residentGiBOr(flags, wanResidentDefaultGiB)
}

// residentGiBOr is wanResidentGiB with the default of the graph the call names (the LTX-2.5 DiT's share is the same flag, with a default of its own).
func residentGiBOr(flags map[string]string, dflt float64) float64 {
	raw, ok := flags["wan-resident-gb"]
	if !ok {
		return dflt
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(v) || v < 0 || v > 64 {
		return dflt
	}
	return v
}

// withPlacementFlags puts the placement flags a call carries over the facts: a flag the call does not carry leaves the facts' own (the pipeline's binding).
func (f Facts) withPlacementFlags(flags map[string]string) Facts {
	if v, ok := flags["placement"]; ok {
		f.Placement = v
	}
	if v := residentGiBFlag(flags); v != nil {
		f.ResidentGiB = v
	}
	return f
}

// residentGiBFlag reads --resident-gb (the card-only image placement's resident share of the DiT on the compute card): nil when absent or when the
// runner would refuse it (not a number from 0 to 64), in which case the call is sized with the recipe's own share.
func residentGiBFlag(flags map[string]string) *float64 {
	raw, ok := flags["resident-gb"]
	if !ok {
		return nil
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(v) || v < 0 || v > 64 {
		return nil
	}
	return &v
}

// megapixelsFlag reads an edit canvas in megapixels: 0 when absent or unreadable (the runner's own: the source's size, capped).
func megapixelsFlag(raw string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 0
	}
	return v
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
