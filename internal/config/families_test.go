package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// A node with a pooled krea2 default and the qwen-image-2.1 family beside it: the
// shape ADR 0058 describes and the reference box will run.
const familiesCfg = `{
  "model": "x",
  "imagegen_script": "render/comfy-generate.mjs",
  "imagegen_family": "krea2",
  "imagegen_ckpt": "krea2_turbo_bf16.safetensors",
  "imagegen_vae": "qwen_image_vae.safetensors",
  "imagegen_steps": 8, "imagegen_cfg": 1,
  "imagegen_lora": "leftover.safetensors",
  "imagegen_pool_vvram_gb": 12, "imagegen_pool_compute": "cuda:1", "imagegen_pool_donor": "cuda:2",
  "imagegen_timeout_sec": 600,
  "comfy_cuda_device": "1",
  "comfy_extra_args": "--disable-dynamic-vram",
  "imagegen_families": {
    "qwen-image-2.1": {
      "license": "Qwen Research License", "commercial_use": false,
      "imagegen_family": "qwen-image-2.1",
      "imagegen_ckpt": "qwen_image_2.1_bf16.safetensors",
      "imagegen_clip": "qwen3vl_8b_bf16.safetensors",
      "imagegen_vae": "qwen_image_2.1_vae_bf16.safetensors",
      "imagegen_steps": 40, "imagegen_cfg": 1,
      "imagegen_schedule": "official",
      "imagegen_timeout_sec": 2400,
      "comfy_dynamic_vram": "on"
    }
  },
  "gen_edit_script": "render/comfy-edit.mjs",
  "gen_edit_unet": "qwen-image-edit-2511-Q5_1.gguf",
  "gen_edit_families": {
    "qwen-image-2.1-edit": {
      "license": "Qwen Research License", "commercial_use": false,
      "gen_edit_family": "qwen-image-2.1",
      "gen_edit_unet": "qwen_image_2.1_bf16.safetensors",
      "gen_edit_clip": "qwen3vl_8b_bf16.safetensors",
      "gen_edit_vae": "qwen_image_2.1_vae_bf16.safetensors",
      "gen_edit_cache_device": "gpu",
      "gen_edit_resolution": 992
    }
  }
}`

func TestImageFamilyOverlayResolvesAsACompleteBinding(t *testing.T) {
	c, err := Load(writeCfg(t, familiesCfg))
	if err != nil {
		t.Fatal(err)
	}
	// The default binding is the config itself.
	def, fi, err := c.ResolveImageFamily("")
	if err != nil || !fi.Default || fi.Name != "krea2" || def.ImageGenCkpt != "krea2_turbo_bf16.safetensors" {
		t.Fatalf("default: %+v %+v %v", fi, def.ImageGenCkpt, err)
	}
	if _, fi2, _ := c.ResolveImageFamily("krea2"); !fi2.Default {
		t.Fatalf("the default family's own name must reach the default binding: %+v", fi2)
	}
	q, fi, err := c.ResolveImageFamily("qwen-image-2.1")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Default || fi.Name != "qwen-image-2.1" || fi.Family != "qwen-image-2.1" || fi.License != "Qwen Research License" || !fi.NonCommercial() {
		t.Fatalf("family info: %+v", fi)
	}
	if !strings.Contains(fi.LicenseNote(), "research/evaluation use only under Qwen Research License") {
		t.Errorf("license note: %q", fi.LicenseNote())
	}
	// Overlay keys applied.
	if q.ImageGenCkpt != "qwen_image_2.1_bf16.safetensors" || q.ImageGenCLIP != "qwen3vl_8b_bf16.safetensors" ||
		q.ImageGenSteps != 40 || q.ImageGenSchedule != "official" || q.ImageGenTimeoutSec != 2400 || q.ComfyDynamicVRAM != "on" {
		t.Errorf("overlay not applied: %+v", q)
	}
	// Model-binding keys the overlay did not set are CLEARED, never inherited from krea2.
	if q.ImageGenLoRA != "" || q.ImageGenPoolVvramGB != 0 || q.ImageGenPoolCompute != "" || q.ImageGenPoolDonor != "" {
		t.Errorf("a family must not inherit the default's LoRA or pool: lora=%q pool=%g/%q/%q",
			q.ImageGenLoRA, q.ImageGenPoolVvramGB, q.ImageGenPoolCompute, q.ImageGenPoolDonor)
	}
	// Route and launch keys ARE inherited.
	if q.ImageGenScript != "render/comfy-generate.mjs" || q.ComfyCudaDevice != "1" || q.ComfyExtraArgs != "--disable-dynamic-vram" {
		t.Errorf("route/launch keys must be inherited: script=%q cuda=%q extra=%q", q.ImageGenScript, q.ComfyCudaDevice, q.ComfyExtraArgs)
	}
	if !q.SupportsTransparentImage() || c.SupportsTransparentImage() {
		t.Error("only the 2.1 graph keeps alpha")
	}
	if q.ImagePooled() || !c.ImagePooled() {
		t.Error("pool state must follow the effective binding")
	}
	// The base config is untouched by resolution.
	if c.ImageGenCkpt != "krea2_turbo_bf16.safetensors" || c.ImageGenLoRA != "leftover.safetensors" {
		t.Error("resolving a family mutated the node's config")
	}
	names := []string{}
	for _, f := range c.ImageFamilies() {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "krea2,qwen-image-2.1" {
		t.Errorf("ImageFamilies = %v (default first, then by name)", names)
	}
}

func TestEditFamilyOverlayResolves(t *testing.T) {
	c, err := Load(writeCfg(t, familiesCfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, fi, _ := c.ResolveEditFamily(""); !fi.Default || fi.Name != EditFamily2511 {
		t.Fatalf("default edit binding: %+v", fi)
	}
	if _, fi, _ := c.ResolveEditFamily(EditFamily2511); !fi.Default {
		t.Fatalf("the 2511 graph's name reaches the default: %+v", fi)
	}
	e, fi, err := c.ResolveEditFamily("qwen-image-2.1-edit")
	if err != nil {
		t.Fatal(err)
	}
	if !fi.NonCommercial() || e.GenEditFamily != FamilyQwenImage21 || e.GenEditCacheDevice != "gpu" || e.GenEditResolution != 992 {
		t.Fatalf("edit overlay: %+v / %+v", fi, e)
	}
	// Default() binds gen_edit_preset lightning8: a 2.1 family must not inherit a 2511 preset.
	if e.GenEditPreset != "" || e.GenEditScript != "render/comfy-edit.mjs" {
		t.Errorf("preset=%q (must clear) script=%q (must inherit)", e.GenEditPreset, e.GenEditScript)
	}
	if !e.SupportsTransparentEdit() || c.SupportsTransparentEdit() {
		t.Error("transparent edit follows the 2.1 graph")
	}
}

// A binxarn-shaped node: no default image binding at all (family-only, sdcpp
// engine) — the family-only tier the fleet gate defect (D1) covers, distinct from
// TestImageFamilyOverlayResolvesAsACompleteBinding's pooled-krea2-plus-family node.
const sdcppFamilyOnlyCfg = `{
  "model": "x",
  "imagegen_families": {
    "qwen-image-2.1": {
      "license": "Qwen Research License", "commercial_use": false,
      "imagegen_family": "qwen-image-2.1",
      "imagegen_engine": "sdcpp",
      "sdcpp_bin": "/opt/offload/sdcpp/sd-cli",
      "sdcpp_model": "/opt/offload/models/qwen-image-2.1/diffusion_models/qwen_image_2.1-Q8_0.gguf",
      "sdcpp_model_kind": "diffusion"
    },
    "z-image": {
      "license": "Apache-2.0", "commercial_use": true,
      "imagegen_family": "z-image",
      "imagegen_engine": "sdcpp",
      "sdcpp_bin": "/opt/offload/sdcpp/sd-cli",
      "sdcpp_model": "/opt/offload/models/z-image/z_image_turbo-Q8_0.gguf"
    }
  }
}`

// TestSdcppQwenImage21SupportsTransparent: D5's root cause — SupportsTransparentImage
// used to unconditionally exclude the sdcpp engine (`ImageGenEngine != "sdcpp"`),
// refusing transparent:true on every sdcpp binding regardless of family, even though
// sd.cpp's qwen-image-2.1 build carries the identical RGBA VAE (binxarn wave session
// 5d227d30 §3b/§3c: P1/P8 both came out RGBA from sd.cpp with zero ComfyUI involved).
// A sibling sdcpp family with no RGBA VAE (z-image) must still be refused, so the fix
// is "follow the family", not "always allow sdcpp".
func TestSdcppQwenImage21SupportsTransparent(t *testing.T) {
	c, err := Load(writeCfg(t, sdcppFamilyOnlyCfg))
	if err != nil {
		t.Fatal(err)
	}
	q, _, err := c.ResolveImageFamily("qwen-image-2.1")
	if err != nil {
		t.Fatal(err)
	}
	if q.ImageGenEngine != "sdcpp" {
		t.Fatalf("fixture must resolve to the sdcpp engine: %+v", q)
	}
	if !q.SupportsTransparentImage() {
		t.Error("sd.cpp's qwen-image-2.1 build has the same RGBA VAE as the ComfyUI graph — must support transparent")
	}
	z, _, err := c.ResolveImageFamily("z-image")
	if err != nil {
		t.Fatal(err)
	}
	if z.SupportsTransparentImage() {
		t.Error("a non-2.1 sdcpp family has no RGBA VAE and must stay refused")
	}
}

func TestUnknownFamilyNameListsWhatTheNodeServes(t *testing.T) {
	c, err := Load(writeCfg(t, familiesCfg))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.ResolveImageFamily("flux-dev")
	if err == nil || !strings.Contains(err.Error(), `unknown image family "flux-dev"`) ||
		!strings.Contains(err.Error(), "krea2 (default), qwen-image-2.1 [non-commercial: Qwen Research License]") {
		t.Fatalf("got %v", err)
	}
	if _, _, err := c.ResolveEditFamily("nope"); err == nil || !strings.Contains(err.Error(), "qwen-image-2.1-edit") {
		t.Fatalf("got %v", err)
	}
}

// Every refusal is a LOAD error that names the overlay and the key — the only moment
// an operator is looking, and the alternative is a render on the wrong binding.
func TestUnknownOrForbiddenFamilyOverlayKeysRefuseTheLoad(t *testing.T) {
	ov := func(img string) string {
		return `{"model":"x","imagegen_family":"krea2","imagegen_families":{"q":{` + img + `}}}`
	}
	edit := func(body string) string {
		return `{"model":"x","gen_edit_families":{"e":{` + body + `}}}`
	}
	lic := `"license":"L","commercial_use":false`
	cases := []struct{ name, body, want string }{
		{"unknown key", ov(lic + `,"imagegen_ckpts":"x"`), `imagegen_families["q"]: unknown key "imagegen_ckpts"`},
		{"wrong route prefix", ov(lic + `,"videogen_script":"x"`), `key "videogen_script" is outside this overlay's keys`},
		{"top-level key", ov(lic + `,"model":"y"`), `key "model" is outside this overlay's keys`},
		{"edit key in image overlay", ov(lic + `,"gen_edit_unet":"x"`), `key "gen_edit_unet" is outside`},
		{"image key in edit overlay", edit(lic + `,"imagegen_ckpt":"x"`), `gen_edit_families["e"]: key "imagegen_ckpt" is outside`},
		{"nested families", ov(lic + `,"imagegen_families":{}`), `"imagegen_families" is not allowed in a family overlay (families do not nest)`},
		{"refiner", ov(lic + `,"imagegen_refiner_model":"gemma"`), `"imagegen_refiner_model" is not allowed`},
		{"duplicate license spelling", ov(lic + `,"imagegen_license":"L"`), `"imagegen_license" is not allowed`},
		{"missing license", ov(`"commercial_use":false`), `"license" is required`},
		{"empty license", ov(`"license":" ","commercial_use":false`), `"license" must be a non-empty string`},
		{"missing commercial_use", ov(`"license":"L"`), `"commercial_use" is required`},
		{"string commercial_use", ov(`"license":"L","commercial_use":"false"`), `"commercial_use" must be true or false`},
		{"type mismatch", ov(lic + `,"imagegen_steps":"forty"`), `imagegen_families["q"].imagegen_steps`},
		{"bad enum inside overlay", ov(lic + `,"comfy_dynamic_vram":"yes"`), `imagegen_families["q"].comfy_dynamic_vram: "yes" is not on, off`},
		{"bad name", `{"model":"x","imagegen_families":{"Qwen 2.1":{` + lic + `}}}`, `a family name is lower-case`},
		{"collides with default", `{"model":"x","imagegen_family":"krea2","imagegen_families":{"krea2":{` + lic + `}}}`, `collides with this node's default binding ("krea2")`},
		{"edit collides with the 2511 default", edit(lic)[:0] + `{"model":"x","gen_edit_families":{"qwen-image-edit-2511":{` + lic + `}}}`, `collides with this node's default binding`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeCfg(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want load error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestLaunchAndEditEnumsRefuseTheLoad(t *testing.T) {
	for key, bad := range map[string]string{
		"imagegen_schedule":     `"karras"`,
		"comfy_dynamic_vram":    `"auto"`,
		"comfy_cuda_device":     `"cuda:1"`,
		"gen_edit_family":       `"flux-kontext"`,
		"gen_edit_cache_device": `"vram"`,
		"gen_edit_resolution":   `1000`,
	} {
		t.Run(key, func(t *testing.T) {
			_, err := Load(writeCfg(t, `{"model":"x","`+key+`":`+bad+`}`))
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("want a load error naming %s, got %v", key, err)
			}
		})
	}
	for _, good := range []string{
		`"imagegen_schedule":"comfy"`, `"comfy_dynamic_vram":"off"`, `"comfy_cuda_device":"1,2"`,
		`"gen_edit_family":"qwen-image-2.1"`, `"gen_edit_cache_device":"off"`, `"gen_edit_resolution":2048`,
	} {
		if _, err := Load(writeCfg(t, `{"model":"x",`+good+`}`)); err != nil {
			t.Errorf("%s must load: %v", good, err)
		}
	}
}

func TestDefaultBindingLicenseIsBothOrNeither(t *testing.T) {
	for _, body := range []string{
		`{"model":"x","imagegen_license":"Apache-2.0"}`,
		`{"model":"x","imagegen_commercial_use":true}`,
		`{"model":"x","gen_edit_license":"Apache-2.0"}`,
	} {
		if _, err := Load(writeCfg(t, body)); err == nil || !strings.Contains(err.Error(), "declare both") {
			t.Errorf("%s: want a both-or-neither refusal, got %v", body, err)
		}
	}
	c, err := Load(writeCfg(t, `{"model":"x","imagegen_family":"krea2","imagegen_license":"Apache-2.0","imagegen_commercial_use":true}`))
	if err != nil {
		t.Fatal(err)
	}
	_, fi, _ := c.ResolveImageFamily("")
	if fi.License != "Apache-2.0" || fi.CommercialUse == nil || !*fi.CommercialUse || fi.NonCommercial() || fi.LicenseNote() != "" {
		t.Errorf("default binding license: %+v", fi)
	}
}

// An overlay decodes each value into a FRESH value of the field's type: decoding a JSON
// array into the base config's slice would reuse its backing array and rewrite the
// default binding's own sd.cpp flags underneath every other request.
func TestOverlayNeverWritesThroughTheBaseConfigsStorage(t *testing.T) {
	base := Default()
	base.SdcppExtraArgs = []string{"--vae-on-cpu", "--keep"}
	base.ImageGenFamilies = map[string]FamilyOverlay{
		"z": {"license": []byte(`"L"`), "commercial_use": []byte(`true`), "imagegen_engine": []byte(`"sdcpp"`),
			"sdcpp_extra_args": []byte(`["--flash"]`), "sdcpp_model": []byte(`"~/models/z.gguf"`)},
	}
	cp, _, err := base.ResolveImageFamily("z")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(base.SdcppExtraArgs, []string{"--vae-on-cpu", "--keep"}) {
		t.Fatalf("base slice rewritten: %v", base.SdcppExtraArgs)
	}
	if !reflect.DeepEqual(cp.SdcppExtraArgs, []string{"--flash"}) || cp.ImageGenEngine != "sdcpp" {
		t.Fatalf("overlay: %v %q", cp.SdcppExtraArgs, cp.ImageGenEngine)
	}
	if home, herr := os.UserHomeDir(); herr == nil && cp.SdcppModel != filepath.Join(home, "models/z.gguf") && cp.SdcppModel != ExpandTilde("~/models/z.gguf", home) {
		t.Errorf("an overlay's path key must be tilde-expanded like the rest: %q", cp.SdcppModel)
	}
	// The clear list happens to zero sdcpp_extra_args first; the fresh-value decode
	// must hold on its own for a slice key a kind does NOT clear.
	keep := overlayKind{key: "probe", prefixes: []string{"sdcpp_"}}
	base.SdcppExtraArgs = []string{"--vae-on-cpu", "--keep"}
	if _, _, _, err := applyOverlay(base, "z", FamilyOverlay{"license": []byte(`"L"`), "commercial_use": []byte(`true`),
		"sdcpp_extra_args": []byte(`["--flash"]`)}, keep); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(base.SdcppExtraArgs, []string{"--vae-on-cpu", "--keep"}) {
		t.Fatalf("decoding an overlay array into the shared slice rewrote the base config: %v", base.SdcppExtraArgs)
	}
}

// Every imagegen_*/sdcpp_* and gen_edit_* key must be classified for overlays —
// cleared (model binding), inherited (route) or forbidden. A new key that is none of
// them would silently be INHERITED by every family: the exact leak ("the family
// rendered with the default's LoRA") the cleared list exists to prevent.
func TestEveryMediaKeyIsClassifiedForOverlays(t *testing.T) {
	idx, _ := configFieldIndex()
	check := func(kind overlayKind, prefixes ...string) {
		// clear and inherited are exclusive; a forbidden key is either never in the
		// effective config's binding (0) or also cleared (1: the default binding's own
		// license must not leak into a family, AND an overlay may not set it).
		seen := map[string]int{}
		for _, k := range kind.clear {
			seen[k]++
		}
		for _, k := range kind.inherited {
			seen[k]++
		}
		for k := range kind.forbidden {
			if seen[k] == 0 {
				seen[k]++
			}
			for _, inh := range kind.inherited {
				if inh == k {
					t.Errorf("%s: %q is both forbidden and inherited", kind.key, k)
				}
			}
		}
		var tags []string
		for tag := range idx {
			for _, p := range prefixes {
				if strings.HasPrefix(tag, p) {
					tags = append(tags, tag)
				}
			}
		}
		sort.Strings(tags)
		for _, tag := range tags {
			switch seen[tag] {
			case 1:
			case 0:
				t.Errorf("%s: config key %q is not classified (clear / inherited / forbidden)", kind.key, tag)
			default:
				t.Errorf("%s: config key %q is classified %d times", kind.key, tag, seen[tag])
			}
		}
		for k := range seen {
			if _, ok := idx[k]; !ok {
				t.Errorf("%s: classifies %q, which is not a config key", kind.key, k)
			}
		}
	}
	check(imageOverlay, "imagegen_", "sdcpp_")
	check(editOverlay, "gen_edit_")
}

func famWarn(t *testing.T, c Config) string {
	t.Helper()
	var buf bytes.Buffer
	warnMediaGenBindingTrapsTo(c, &buf)
	return buf.String()
}

func TestLaunchProfileWarningsOnPooledSeats(t *testing.T) {
	t.Setenv("COMFY_EXTRA_ARGS", "")
	krea := Config{ImageGenFamily: "krea2", ImageGenSteps: 8, ImageGenCFG: 1, ImageGenPoolVvramGB: 12}
	on := krea
	on.ComfyDynamicVRAM = "on"
	if out := famWarn(t, on); !strings.Contains(out, "comfy_dynamic_vram is on while the image seat pools") {
		t.Fatalf("dynamic on + image pool must warn, got %q", out)
	}
	off := krea
	off.ComfyDynamicVRAM = "off"
	if out := famWarn(t, off); strings.Contains(out, "un-pools") {
		t.Fatalf("comfy_dynamic_vram off satisfies the pooled seat, got %q", out)
	}
	viaCfg := krea
	viaCfg.ComfyExtraArgs = "--disable-dynamic-vram"
	if out := famWarn(t, viaCfg); strings.Contains(out, "un-pools") {
		t.Fatalf("comfy_extra_args carrying the flag satisfies the pooled seat, got %q", out)
	}
	pin := off
	pin.ComfyCudaDevice = "1"
	if out := famWarn(t, pin); !strings.Contains(out, `comfy_cuda_device "1" is not applied to the pooled image seat`) {
		t.Fatalf("a pin beside image pool keys must say it is not applied there, got %q", out)
	}
	if out := famWarn(t, off); strings.Contains(out, "comfy_cuda_device") {
		t.Fatalf("no pin, no pin warning: %q", out)
	}
	vid := Config{VideoGenPoolVvramGB: 30, ComfyDynamicVRAM: "on", ComfyCudaDevice: "1"}
	out := famWarn(t, vid)
	if !strings.Contains(out, "comfy_dynamic_vram is on while the video seat pools") || !strings.Contains(out, "not applied to the pooled video seat") {
		t.Fatalf("video pool warnings, got %q", out)
	}
}

func TestFamilyBindingWarnings(t *testing.T) {
	t.Setenv("COMFY_EXTRA_ARGS", "--disable-dynamic-vram")
	c := Config{
		ImageGenFamily: "krea2", ImageGenSteps: 8, ImageGenCFG: 1,
		ImageGenFamilies: map[string]FamilyOverlay{
			"q": {"license": []byte(`"L"`), "commercial_use": []byte(`false`),
				"imagegen_family": []byte(`"qwen-image-2.1"`), "imagegen_steps": []byte(`40`)},
			"k": {"license": []byte(`"L"`), "commercial_use": []byte(`true`),
				"imagegen_family": []byte(`"krea2"`), "imagegen_schedule": []byte(`"official"`)},
		},
		GenEditFamilies: map[string]FamilyOverlay{
			"e": {"license": []byte(`"L"`), "commercial_use": []byte(`false`),
				"gen_edit_family": []byte(`"qwen-image-2.1"`), "gen_edit_megapixels": []byte(`2`)},
		},
		GenEditCacheDevice: "gpu",
	}
	out := famWarn(t, c)
	for _, want := range []string{
		`imagegen_families["q"]: imagegen_steps/imagegen_cfg are half-bound (40 / 0) on the qwen-image-2.1 binding`,
		`imagegen_families["k"]: imagegen_schedule "official" is set but imagegen_family is "krea2"`,
		`gen_edit_families["e"]: gen_edit_megapixels is set on the qwen-image-2.1 edit binding`,
		`gen_edit_cache_device is set but gen_edit_family is "qwen-image-edit-2511"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing warning %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "on the DEFAULT image binding") {
		t.Errorf("a NAMED 2.1 family is the sanctioned shape and must not trip the default-binding warning:\n%s", out)
	}
	def := Config{ImageGenFamily: FamilyQwenImage21, ImageGenSteps: 40, ImageGenCFG: 1}
	if out := famWarn(t, def); !strings.Contains(out, "qwen-image-2.1 on the DEFAULT image binding") {
		t.Errorf("2.1 as the default binding must warn (ADR 0058), got %q", out)
	}
}
