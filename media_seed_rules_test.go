package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// mediaSeedDoc reads each tier's two seed layers: the base config_seed and the RAM-conditional
// overlay. A media route must be complete within what one box actually receives, so a model in the
// overlay may take its script from either layer.
type mediaSeedDoc struct {
	Profiles map[string]struct {
		ConfigSeed        map[string]any `json:"config_seed"`
		ConfigSeedMidHigh map[string]any `json:"config_seed_ram_mid_high"`
	} `json:"profiles"`
}

func loadMediaSeedDoc(t *testing.T) mediaSeedDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc mediaSeedDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}
	if len(doc.Profiles) == 0 {
		t.Fatal("profiles.json has no profiles — every gate below went blind")
	}
	return doc
}

// layers returns what a box of this tier receives: the base seed alone, and — for a mid/high-RAM
// box — the base with the overlay on top.
func mediaLayers(base, overlay map[string]any) []map[string]any {
	out := []map[string]any{base}
	if len(overlay) > 0 {
		merged := map[string]any{}
		for k, v := range base {
			merged[k] = v
		}
		for k, v := range overlay {
			merged[k] = v
		}
		out = append(out, merged)
	}
	return out
}

func seedStr(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// TestEveryComfyMediaModelHasItsScript: a ComfyUI media route needs BOTH halves — the model the
// tier chose and the script that runs it (internal/mediacap reports the route NOT CONFIGURED when
// either is empty, and there is no default script). Until 0.132.5 eight 16 GB+ tiers seeded an image
// model with no imagegen_script, thirteen seeded upscale_model with no upscale_script anywhere, and
// blackwell-8 seeded an edit UNET and an inpaint checkpoint with no scripts — so on every fresh
// install those routes deferred, while the Qube served them only because they were hand-wired.
func TestEveryComfyMediaModelHasItsScript(t *testing.T) {
	doc := loadMediaSeedDoc(t)
	names := make([]string, 0, len(doc.Profiles))
	for n := range doc.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	pairs := []struct{ model, script string }{
		{"gen_edit_unet", "gen_edit_script"},
		{"inpaint_ckpt", "inpaint_script"},
	}
	for _, name := range names {
		p := doc.Profiles[name]
		for i, seed := range mediaLayers(p.ConfigSeed, p.ConfigSeedMidHigh) {
			where := name
			if i == 1 {
				where += " (mid/high RAM)"
			}
			comfyImage := seedStr(seed, "imagegen_family") != "" && seedStr(seed, "imagegen_engine") != "sdcpp"
			if comfyImage && seedStr(seed, "imagegen_script") == "" {
				t.Errorf("%s seeds a ComfyUI image model (%s) with no imagegen_script: the route defers on every fresh install",
					where, seedStr(seed, "imagegen_family"))
			}
			// Upscale runs through ComfyUI, so it is a route only where the tier runs ComfyUI.
			if comfyImage && seedStr(seed, "upscale_model") != "" && seedStr(seed, "upscale_script") == "" {
				t.Errorf("%s seeds upscale_model with no upscale_script: the route defers on every fresh install", where)
			}
			for _, pr := range pairs {
				if seedStr(seed, pr.model) != "" && seedStr(seed, pr.script) == "" {
					t.Errorf("%s seeds %s with no %s: the route defers on every fresh install", where, pr.model, pr.script)
				}
			}
		}
	}
}

// TestMediaScriptsResolveNextToTheBinary: gpugen.ResolveScriptIn joins a RELATIVE script to the
// binary's own directory, and both installers put render/ there (install.sh: "$PREFIX/bin/render";
// Windows: <home>\bin\render). ampere-8 and blackwell-8 seeded "__OFFLOAD_HOME__/render/…", a
// directory that does not exist on an installed box (measured on the Aorus 2026-09-21:
// D:\offload-stack\render absent, D:\offload-stack\bin\render present), so the seeded image route
// pointed at nothing and the Aorus worked only because it was hand-set.
func TestMediaScriptsResolveNextToTheBinary(t *testing.T) {
	doc := loadMediaSeedDoc(t)
	for name, p := range doc.Profiles {
		for _, layer := range []map[string]any{p.ConfigSeed, p.ConfigSeedMidHigh} {
			for k, v := range layer {
				s, ok := v.(string)
				if !ok || !strings.HasSuffix(k, "_script") || s == "" {
					continue
				}
				if !strings.HasPrefix(s, "render/") {
					t.Errorf("%s seeds %s=%q: seed the relative \"render/…\" form, which resolves next to the installed binary", name, k, s)
				}
			}
		}
	}
}

// TestSixteenGBComfyTiersSeedTheMeasuredEditInpaintAnimateRoutes pins the three routes that were
// measured, kept, and served on the Qube — and seeded by no tier:
//   - edit: Qwen-Image-Edit-2511 Q5_1 + the lightning8 preset, "frontier confirmed ≥16GB edit
//     primitive" (2026-08-14-seat-frontier-leg0-1-2-notes.md:323).
//   - inpaint: RealVisXL_V5.0_fp16 held its seat (2026-08-19-nightshift8-notes.md:201).
//   - animate: WAN-Animate-2 int8 on ONE 16 GB card, 15.9 GB, 81 frames @480x854 (register F-18);
//     operator 2026-08-27: "route WAN-Animate as a media capability".
func TestSixteenGBComfyTiersSeedTheMeasuredEditInpaintAnimateRoutes(t *testing.T) {
	doc := loadMediaSeedDoc(t)
	want := map[string]string{
		"gen_edit_script":   "render/comfy-edit.mjs",
		"gen_edit_unet":     "qwen-image-edit-2511-Q5_1.gguf",
		"gen_edit_preset":   "lightning8",
		"inpaint_script":    "render/comfy-inpaint.mjs",
		"inpaint_ckpt":      "RealVisXL_V5.0_fp16.safetensors",
		"animategen_script": "render/comfy-animate.mjs",
	}
	for _, tier := range []string{"blackwell-16", "blackwell-32", "blackwell-2x16", "blackwell-3x16",
		"blackwell-48", "blackwell-72", "ampere-16", "volta-16"} {
		p, ok := doc.Profiles[tier]
		if !ok {
			t.Fatalf("tier %s not found — this gate went blind", tier)
		}
		for k, w := range want {
			if got := seedStr(p.ConfigSeed, k); got != w {
				t.Errorf("%s config_seed.%s = %q, want %q", tier, k, got, w)
			}
		}
	}
}
