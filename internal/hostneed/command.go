package hostneed

import (
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

// helperRoute maps the render helpers to the route whose files they load. comfy-run-graph.mjs is
// deliberately absent: it runs an arbitrary graph, so what it loads is unknown to the caller. The
// same helpers run in --graph mode (comfy-render, comfy-video) are the same case, handled in Estimate.
var helperRoute = map[string]Route{
	"comfy-generate.mjs": RouteImage,
	"comfy-render.mjs":   RouteImage,
	"comfy-edit.mjs":     RouteEdit,
	"comfy-inpaint.mjs":  RouteInpaint,
	"comfy-video.mjs":    RouteVideo,
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
	// --graph POSTS THE CALLER'S OWN WORKFLOW as it is ("any nodes, any model, any pipeline",
	// comfy-render.mjs; comfy-video.mjs says the same): the family flags are not read, so what it
	// loads is as unknown to the caller as what run-graph loads, and the media class default stands in.
	// Sizing it as the helper's default family declared 0 for a 40 GiB graph (SDXL fits the card) and
	// the wrong family for a video one (review of 2026-10-10).
	if strings.TrimSpace(c.Flags["graph"]) != "" {
		return Need{}, false
	}
	if v := c.Flags["reserve-vram"]; v != "" {
		if r, err := strconv.ParseFloat(v, 64); err == nil && r > 0 {
			f.ReserveVRAMGiB = r
		}
	}
	cfg := f.Cfg
	switch c.Route {
	case RouteImage:
		cfg.ImageGenFamily = c.Flags["family"]
		cfg.ImageGenCkpt = firstNonEmpty(c.Flags["ckpt"], f.env("COMFY_CKPT"))
		cfg.ImageGenCLIP = c.Flags["clip"]
		return ForRoute(RouteImage, cfg, f)
	case RouteEdit:
		cfg.GenEditFamily = c.Flags["family"]
		cfg.GenEditUnet = firstNonEmpty(c.Flags["unet"], f.env("COMFY_EDIT_UNET"))
		cfg.GenEditCLIP = c.Flags["clip"]
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
		return estimate(model, mediacap.VideoModelFilesFor(model, fb), f)
	}
	return Need{}, false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
