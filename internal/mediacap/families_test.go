package mediacap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// overlay builds a FamilyOverlay from a JSON object literal.
func overlay(t *testing.T, js string) config.FamilyOverlay {
	t.Helper()
	var ov config.FamilyOverlay
	if err := json.Unmarshal([]byte(js), &ov); err != nil {
		t.Fatalf("overlay %s: %v", js, err)
	}
	return ov
}

// qi21Box is a ComfyUI box whose models root holds the Qwen-Image-2.1 quality set
// (unless skip names a file to leave out) and whose render tree ships both runners.
func qi21Box(t *testing.T, skip string) (cfg config.Config, exeDir string) {
	t.Helper()
	exeDir = t.TempDir()
	comfy := t.TempDir()
	for _, f := range []string{
		"diffusion_models/qwen_image_2.1_bf16.safetensors",
		"text_encoders/qwen3vl_8b_bf16.safetensors",
		"vae/qwen_image_2.1_vae_bf16.safetensors",
	} {
		if f != skip {
			touchModel(t, filepath.Join(comfy, "models", filepath.FromSlash(f)))
		}
	}
	touch(t, exeDir, "render/comfy-generate.mjs")
	touch(t, exeDir, "render/comfy-edit.mjs")
	cfg = bare()
	cfg.ComfyDir = comfy
	cfg.NodePath = "node"
	return cfg, exeDir
}

const qi21ImageOverlay = `{"license":"Qwen Research License","commercial_use":false,
	"imagegen_script":"render/comfy-generate.mjs","imagegen_family":"qwen-image-2.1",
	"imagegen_ckpt":"qwen_image_2.1_bf16.safetensors","imagegen_clip":"qwen3vl_8b_bf16.safetensors",
	"imagegen_vae":"qwen_image_2.1_vae_bf16.safetensors"}`

const qi21EditOverlay = `{"license":"Qwen Research License","commercial_use":false,
	"gen_edit_script":"render/comfy-edit.mjs","gen_edit_family":"qwen-image-2.1",
	"gen_edit_unet":"qwen_image_2.1_bf16.safetensors","gen_edit_clip":"qwen3vl_8b_bf16.safetensors",
	"gen_edit_vae":"qwen_image_2.1_vae_bf16.safetensors"}`

// A named family is a whole binding: it is CONFIGURED only when its script resolves
// AND every model file its graph opens sits in the class directory the loader reads.
// A family whose DiT is not on the box must read BOUND-BUT-MISSING on its own route
// line (naming the file), never CONFIGURED because the shared script exists.
func TestFamilyRouteChecksItsModelFiles(t *testing.T) {
	cfg, exeDir := qi21Box(t, "")
	cfg.ImageGenFamilies = map[string]config.FamilyOverlay{"qwen-image-2.1": overlay(t, qi21ImageOverlay)}
	cfg.GenEditFamilies = map[string]config.FamilyOverlay{"qwen-image-2.1": overlay(t, qi21EditOverlay)}

	got := byName(routesIn(cfg, exeDir))
	img := got[ImageFamilyRoute("qwen-image-2.1")]
	if img.State != Configured {
		t.Fatalf("complete family = %q (%s), want CONFIGURED", img.State, img.Detail)
	}
	if !strings.Contains(img.Detail, "NON-COMMERCIAL (Qwen Research License)") {
		t.Errorf("a non-commercial family's route must say so first: %q", img.Detail)
	}
	if !strings.Contains(img.Detail, "qwen_image_2.1_bf16.safetensors") {
		t.Errorf("the verdict must name the files it checked: %q", img.Detail)
	}
	if ed := got[EditFamilyRoute("qwen-image-2.1")]; ed.State != Configured {
		t.Errorf("complete edit family = %q (%s), want CONFIGURED", ed.State, ed.Detail)
	}
	if _, ok := got["comfyui"]; !ok {
		t.Error("a family that drives ComfyUI must bring the comfyui prereq row")
	}

	// The DiT missing: only the family routes flip, and they name the file.
	cfg, exeDir = qi21Box(t, "diffusion_models/qwen_image_2.1_bf16.safetensors")
	cfg.ImageGenFamilies = map[string]config.FamilyOverlay{"qwen-image-2.1": overlay(t, qi21ImageOverlay)}
	cfg.GenEditFamilies = map[string]config.FamilyOverlay{"qwen-image-2.1": overlay(t, qi21EditOverlay)}
	routes := routesIn(cfg, exeDir)
	got = byName(routes)
	for _, rn := range []string{ImageFamilyRoute("qwen-image-2.1"), EditFamilyRoute("qwen-image-2.1")} {
		r := got[rn]
		if r.State != BoundButMissing {
			t.Errorf("%s with its DiT absent = %q (%s), want BOUND-BUT-MISSING", rn, r.State, r.Detail)
		}
		if !strings.Contains(r.Detail, "qwen_image_2.1_bf16.safetensors") || !strings.Contains(r.Detail, "families[\"qwen-image-2.1\"]") {
			t.Errorf("%s must name the missing file under the family's config path: %q", rn, r.Detail)
		}
	}
	if n := len(Missing(routes)); n != 2 {
		t.Errorf("Missing() = %d, want exactly the two family routes", n)
	}
	if got["generate_image"].State != NotConfigured {
		t.Errorf("the default route must be untouched by a family: %+v", got["generate_image"])
	}
}

// The 2.1 graphs have NO default text encoder or VAE: a family that leaves one of
// its three files unset is not a family that can render, whatever the script says.
func TestFamilyRouteRequiresEveryQwenImage21File(t *testing.T) {
	cfg, exeDir := qi21Box(t, "")
	cfg.ImageGenFamilies = map[string]config.FamilyOverlay{"qi21": overlay(t, `{"license":"Qwen Research License","commercial_use":false,
		"imagegen_script":"render/comfy-generate.mjs","imagegen_family":"qwen-image-2.1",
		"imagegen_ckpt":"qwen_image_2.1_bf16.safetensors","imagegen_vae":"qwen_image_2.1_vae_bf16.safetensors"}`)}
	r := byName(routesIn(cfg, exeDir))[ImageFamilyRoute("qi21")]
	if r.State != BoundButMissing || !strings.Contains(r.Detail, "imagegen_clip is required") {
		t.Fatalf("a 2.1 family without imagegen_clip = %q (%s), want BOUND-BUT-MISSING naming imagegen_clip", r.State, r.Detail)
	}
}

// A family whose overlay does not resolve (possible only in-process: Load refuses
// it) is reported, not dropped; one with no script is NOT CONFIGURED.
func TestFamilyRouteReportsAnUnresolvableOrScriptlessOverlay(t *testing.T) {
	cfg, exeDir := qi21Box(t, "")
	cfg.ImageGenFamilies = map[string]config.FamilyOverlay{
		"nolicense": overlay(t, `{"imagegen_family":"qwen-image-2.1"}`),
		"noscript":  overlay(t, `{"license":"x","commercial_use":true,"imagegen_ckpt":"a.safetensors"}`),
	}
	got := byName(routesIn(cfg, exeDir))
	if r := got[ImageFamilyRoute("nolicense")]; r.State != BoundButMissing || !strings.Contains(r.Detail, "license") {
		t.Errorf("unresolvable overlay = %+v", r)
	}
	if r := got[ImageFamilyRoute("noscript")]; r.State != NotConfigured {
		t.Errorf("a family with no script (and none inherited) = %+v, want NOT CONFIGURED", r)
	}
}

// Preset-implied files (register D7): the Qube's 2511 edit route was bound to
// preset lightning8 with gen_edit_lora unset, and the Lightning LoRA that preset
// loads was on no models root while doctor stayed green. The files a binding's
// graph opens WITHOUT a key naming them — a preset's LoRA, a builder's default
// text encoder/VAE — are now resolved like the named ones.
func TestModelBindingsCheckPresetAndBuilderImpliedFiles(t *testing.T) {
	comfy := t.TempDir()
	touchModel(t, filepath.Join(comfy, "models", "diffusion_models", "qwen-image-edit-2511-Q5_1.gguf"))
	touchModel(t, filepath.Join(comfy, "models", "text_encoders", "qwen_2.5_vl_7b_fp8_scaled.safetensors"))
	touchModel(t, filepath.Join(comfy, "models", "vae", "qwen_image_vae.safetensors"))
	touchModel(t, filepath.Join(comfy, "models", "diffusion_models", "qwen-image-2512-Q5_1.gguf"))
	cfg := config.Config{
		ComfyDir:       comfy,
		GenEditScript:  "render/comfy-edit.mjs",
		GenEditUnet:    "qwen-image-edit-2511-Q5_1.gguf",
		GenEditPreset:  "lightning8",
		ImageGenScript: "render/comfy-generate.mjs",
		ImageGenFamily: "qwen-image",
		ImageGenCkpt:   "qwen-image-2512-Q5_1.gguf",
		ImageGenPreset: "lightning4",
	}
	got := map[string]Binding{}
	for _, b := range ModelBindings(cfg) {
		got[b.Key] = b
	}
	if b, ok := got["gen_edit_lora (preset lightning8)"]; !ok || b.State != BindingMissing ||
		b.Name != "Qwen-Image-Edit-2511-Lightning-8steps-V1.0-bf16.safetensors" {
		t.Errorf("the lightning8 preset's LoRA must be checked and MISSING: %+v (all: %v)", b, keysOf(got))
	}
	if b, ok := got["imagegen_lora (preset lightning4)"]; !ok || b.State != BindingMissing {
		t.Errorf("the qwen-image lightning4 preset's LoRA must be checked and MISSING: %+v", b)
	}
	if b, ok := got["gen_edit_clip (2511 default)"]; !ok || b.State != BindingFound {
		t.Errorf("the 2511 builder's default text encoder must be checked: %+v", b)
	}
	if b, ok := got["imagegen_vae (qwen-image default)"]; !ok || b.State != BindingFound {
		t.Errorf("the qwen-image builder's default VAE must be checked: %+v", b)
	}

	// Bound explicitly = the key's own row; the implied row disappears.
	cfg.GenEditLoRA = "my-lora.safetensors"
	cfg.GenEditPreset = "full"
	got = map[string]Binding{}
	for _, b := range ModelBindings(cfg) {
		got[b.Key] = b
	}
	for k := range got {
		if strings.HasPrefix(k, "gen_edit_lora (") {
			t.Errorf("an explicit gen_edit_lora must replace the implied row, got %q", k)
		}
	}
	if _, ok := got["gen_edit_lora"]; !ok {
		t.Error("the explicit gen_edit_lora must be checked")
	}

	// An unbound route implies nothing.
	cfg = config.Config{ComfyDir: comfy, GenEditPreset: "lightning8", ImageGenFamily: "qwen-image"}
	if bs := ModelBindings(cfg); len(bs) != 0 {
		t.Errorf("unbound routes imply no files, got %+v", bs)
	}
}

// ModelBindings (doctor's `comfyui model bindings`) lists every named family's
// files under the family's config path, resolved under the family's own comfy_dir.
func TestModelBindingsListEveryFamilysFiles(t *testing.T) {
	cfg, _ := qi21Box(t, "vae/qwen_image_2.1_vae_bf16.safetensors")
	cfg.ImageGenFamilies = map[string]config.FamilyOverlay{"qwen-image-2.1": overlay(t, qi21ImageOverlay)}
	got := map[string]Binding{}
	for _, b := range ModelBindings(cfg) {
		got[b.Key] = b
	}
	if b := got[`imagegen_families["qwen-image-2.1"].imagegen_ckpt`]; b.State != BindingFound {
		t.Errorf("family DiT: %+v (all: %v)", b, keysOf(got))
	}
	if b := got[`imagegen_families["qwen-image-2.1"].imagegen_vae`]; b.State != BindingMissing {
		t.Errorf("family VAE (absent) must be MISSING: %+v", b)
	}
}

// The implied-file tables above restate the render builders' defaults. This parses
// the builders and fails when the two drift — the same way a restated preset table
// went stale before (a LoRA renamed in the builder would make doctor check a file
// the graph never opens).
func TestImpliedFilesMirrorTheRenderBuilders(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("..", "..", "render", name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	presets := func(src, table string) map[string]string {
		t.Helper()
		i := strings.Index(src, "export const "+table+" = {")
		if i < 0 {
			t.Fatalf("%s not found", table)
		}
		body := src[i:]
		body = body[:strings.Index(body, "\n};")]
		out := map[string]string{}
		for _, m := range regexp.MustCompile(`(?m)^\s+([a-z0-9]+): \{[^}]*lora: "([^"]*)"`).FindAllStringSubmatch(body, -1) {
			out[m[1]] = m[2]
		}
		return out
	}
	defaults := func(src, fn string) (clip, vae string) {
		t.Helper()
		i := strings.Index(src, "export function "+fn+"(")
		if i < 0 {
			t.Fatalf("%s not found", fn)
		}
		sig := src[i:]
		sig = sig[:strings.Index(sig, "})")]
		c := regexp.MustCompile(`clip = "([^"]+)"`).FindStringSubmatch(sig)
		v := regexp.MustCompile(`vae = "([^"]+)"`).FindStringSubmatch(sig)
		if c == nil || v == nil {
			t.Fatalf("%s: no clip/vae default in its signature", fn)
		}
		return c[1], v[1]
	}
	same := func(what string, got, want map[string]string) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s: builder has %v, mediacap has %v", what, got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s[%s]: builder %q, mediacap %q", what, k, got[k], v)
			}
		}
	}
	qi, krea, edit := read("wf-qwen-image.mjs"), read("wf-krea2.mjs"), read("wf-qwen-image-edit.mjs")
	same("QWEN_IMAGE_PRESETS", presets(qi, "QWEN_IMAGE_PRESETS"), imagePresetLoRAs)
	same("QWEN_EDIT_PRESETS", presets(edit, "QWEN_EDIT_PRESETS"), editPresetLoRAs)
	for fam, src := range map[string][2]string{
		"qwen-image":          {qi, "buildQwenImage"},
		"krea2":               {krea, "buildKrea2"},
		config.EditFamily2511: {edit, "buildQwenImageEdit"},
	} {
		c, v := defaults(src[0], src[1])
		if want := builderCompanions[fam]; c != want.clip || v != want.vae {
			t.Errorf("%s defaults: builder (%s, %s), mediacap (%s, %s)", fam, c, v, want.clip, want.vae)
		}
	}
	// The runners' own preset fallbacks.
	if !strings.Contains(read("comfy-render.mjs"), `flags.preset || "`+defaultImagePreset+`"`) {
		t.Errorf("comfy-render.mjs no longer falls back to preset %q", defaultImagePreset)
	}
	if !strings.Contains(read("comfy-edit.mjs"), `|| "`+defaultEditPreset+`"`) {
		t.Errorf("comfy-edit.mjs no longer falls back to preset %q", defaultEditPreset)
	}
}

func keysOf(m map[string]Binding) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
