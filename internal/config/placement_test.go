package config

// The card-only placement keys: imagegen_placement and gen_edit_placement ("multicard" asks the typed image and edit doors for the card-only recipe on
// two leased non-display cards; internal/pipeline mediaadmit.go, render/multicard-recipes.json). Both are unset by default, so no box changes behaviour until
// its operator binds one, and both are model-binding keys of the named-family overlays (a family replaces them wholesale, like the pool keys).

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const placementCfg = `{
  "model": "x",
  "imagegen_script": "render/comfy-generate.mjs",
  "imagegen_family": "qwen-image",
  "imagegen_ckpt": "qwen-image-2512-Q5_1.gguf",
  "imagegen_placement": "multicard",
  "imagegen_families": {
    "qwen-image-2.1": {
      "license": "Qwen Research License", "commercial_use": false,
      "imagegen_family": "qwen-image-2.1",
      "imagegen_ckpt": "qwen_image_2.1_bf16.safetensors",
      "imagegen_clip": "qwen3vl_8b_bf16.safetensors",
      "imagegen_vae": "qwen_image_2.1_vae_bf16.safetensors",
      "imagegen_placement": "multicard"
    },
    "plain": { "license": "Apache-2.0", "commercial_use": true, "imagegen_family": "qwen-image", "imagegen_ckpt": "other.gguf" }
  },
  "gen_edit_script": "render/comfy-edit.mjs",
  "gen_edit_unet": "qwen_image_edit_2511_fp8mixed.safetensors",
  "gen_edit_placement": "multicard",
  "gen_edit_families": {
    "edit-plain": { "license": "Apache-2.0", "commercial_use": true, "gen_edit_unet": "qwen-image-edit-2511-Q5_1.gguf" }
  }
}`

func TestImagePlacementIsAnOverlayKeyAndWarnsOnAnUnsupportedFamily(t *testing.T) {
	c, err := Load(writeCfg(t, placementCfg))
	if err != nil {
		t.Fatal(err)
	}
	if c.ImageGenPlacement != "multicard" || !c.ImageMulticard() || c.GenEditPlacement != "multicard" || !c.EditMulticard() {
		t.Fatalf("the default bindings: image %q edit %q", c.ImageGenPlacement, c.GenEditPlacement)
	}
	q, _, err := c.ResolveImageFamily("qwen-image-2.1")
	if err != nil || q.ImageGenPlacement != "multicard" || !q.ImageMulticard() {
		t.Fatalf("a family overlay may set the key: %q %v", q.ImageGenPlacement, err)
	}
	// a family that does not set it is cleared, never inherited from the default binding
	p, _, err := c.ResolveImageFamily("plain")
	if err != nil || p.ImageGenPlacement != "" || p.ImageMulticard() {
		t.Fatalf("an overlay replaces the placement wholesale: %q %v", p.ImageGenPlacement, err)
	}
	e, _, err := c.ResolveEditFamily("edit-plain")
	if err != nil || e.GenEditPlacement != "" || e.EditMulticard() {
		t.Fatalf("the edit overlay replaces it too: %q %v", e.GenEditPlacement, err)
	}
	// single is a value (the explicit default); anything else is refused at load
	for _, ok := range []string{"", "single", "multicard"} {
		if err := validateMediaEnums(Config{ImageGenPlacement: ok, GenEditPlacement: ok}, ""); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	if err := validateMediaEnums(Config{ImageGenPlacement: "both"}, ""); err == nil || !strings.Contains(err.Error(), "imagegen_placement") {
		t.Errorf("an unknown image placement must be refused naming the key: %v", err)
	}
	if err := validateMediaEnums(Config{GenEditPlacement: "both"}, "gen_edit_families[\"x\"]: "); err == nil || !strings.Contains(err.Error(), "gen_edit_placement") {
		t.Errorf("an unknown edit placement must be refused naming the key: %v", err)
	}
	bad := strings.Replace(placementCfg, `"imagegen_placement": "multicard",
  "imagegen_families"`, `"imagegen_placement": "both",
  "imagegen_families"`, 1)
	if _, err := Load(writeCfg(t, bad)); err == nil || !strings.Contains(err.Error(), "imagegen_placement") {
		t.Errorf("Load of a bad placement: %v", err)
	}

	// the recipe's families: qwen-image and krea2 on the image route (qwen-image-2.1 has two rows, both held back, so no launchable recipe), the 2511 graph on the edit route; every other is said at load
	say := func(c Config) string {
		var buf bytes.Buffer
		warnMediaGenBindingTrapsTo(c, &buf)
		return buf.String()
	}
	const no = "has no card-only recipe"
	if out := say(Config{ImageGenFamily: "hidream-o1", ImageGenPlacement: "multicard", GPUCardScopedLeases: true}); !strings.Contains(out, "imagegen_placement") || !strings.Contains(out, no) || !strings.Contains(out, `"hidream-o1"`) {
		t.Errorf("hidream-o1 has no card-only recipe, and EVERY render will be refused: %q", out)
	}
	if out := say(Config{ImageGenPlacement: "multicard", GPUCardScopedLeases: true}); !strings.Contains(out, no) || !strings.Contains(out, "sdxl") {
		t.Errorf("the generic SDXL graph has none: %q", out)
	}
	for _, fam := range []string{"qwen-image", "krea2", FamilyQwenImage21} {
		if out := say(Config{ImageGenFamily: fam, ImageGenPlacement: "multicard", ImageGenSteps: 8, ImageGenCFG: 1, GPUCardScopedLeases: true}); strings.Contains(out, no) {
			t.Errorf("%s has a recipe: %q", fam, out)
		}
	}
	if out := say(Config{GenEditFamily: FamilyQwenImage21, GenEditPlacement: "multicard", GPUCardScopedLeases: true}); !strings.Contains(out, "gen_edit_placement") || !strings.Contains(out, no) {
		t.Errorf("the 2.1 edit graph has none: %q", out)
	}
	for _, fam := range []string{"", EditFamily2511} {
		if out := say(Config{GenEditFamily: fam, GenEditPlacement: "multicard", GPUCardScopedLeases: true}); strings.Contains(out, no) {
			t.Errorf("edit family %q has a recipe: %q", fam, out)
		}
	}
	// silent arm: no placement, no placement warning, on any family
	if out := say(Config{ImageGenFamily: "krea2", ImageGenSteps: 8, ImageGenCFG: 1}); strings.Contains(out, "placement") {
		t.Errorf("a box that binds no placement hears nothing about it: %q", out)
	}
}

func TestLaunchProfileWarningsOnMulticardSeats(t *testing.T) {
	t.Setenv("COMFY_EXTRA_ARGS", "")
	say := func(c Config) string {
		var buf bytes.Buffer
		warnMediaGenBindingTrapsTo(c, &buf)
		return buf.String()
	}
	seat := Config{ImageGenFamily: "qwen-image", ImageGenPlacement: "multicard", GPUCardScopedLeases: true}
	if out := say(seat); strings.Contains(out, "warning") {
		t.Fatalf("a clean multicard seat warns of nothing: %q", out)
	}
	// ignored under multicard, and said at load
	pin := seat
	pin.ComfyCudaDevice = "1"
	if out := say(pin); !strings.Contains(out, "comfy_cuda_device") || !strings.Contains(out, "not applied") || !strings.Contains(out, "imagegen_placement") {
		t.Errorf("the device pin is the lease's two cards under multicard: %q", out)
	}
	dyn := seat
	dyn.ComfyDynamicVRAM = "on"
	if out := say(dyn); !strings.Contains(out, "comfy_dynamic_vram") || !strings.Contains(out, "--gpu-only") {
		t.Errorf("a multicard launch runs --gpu-only: %q", out)
	}
	off := seat
	off.ComfyDynamicVRAM = "off"
	if out := say(off); strings.Contains(out, "comfy_dynamic_vram") {
		t.Errorf("off is what the launch does anyway: %q", out)
	}
	pool := seat
	pool.ImageGenPoolVvramGB, pool.ImageGenPoolCompute = 12, "cuda:1"
	if out := say(pool); !strings.Contains(out, "imagegen_pool_") || !strings.Contains(out, "ignored") {
		t.Errorf("a pooled seat is another placement; its keys are ignored under multicard: %q", out)
	}
	edit := Config{GenEditPlacement: "multicard", GPUCardScopedLeases: true, ComfyCudaDevice: "2"}
	if out := say(edit); !strings.Contains(out, "comfy_cuda_device") || !strings.Contains(out, "gen_edit_placement") {
		t.Errorf("the edit seat too: %q", out)
	}
	// without card-scoped leases the call holds the whole node and names no cards, so the runner refuses it
	whole := Config{ImageGenFamily: "qwen-image", ImageGenPlacement: "multicard"}
	if out := say(whole); !strings.Contains(out, "gpu_card_scoped_leases") || !strings.Contains(out, "EVERY render will be refused") {
		t.Errorf("a multicard binding on a host that does not lease cards cannot run: %q", out)
	}
	// the sd.cpp engine does not read it
	sd := Config{ImageGenEngine: "sdcpp", ImageGenPlacement: "multicard", GPUCardScopedLeases: true}
	if out := say(sd); !strings.Contains(out, "sdcpp") || !strings.Contains(out, "ComfyUI placement") {
		t.Errorf("sd.cpp has no ComfyUI placement: %q", out)
	}
}

func TestNoBoxBindsAPlacementByDefault(t *testing.T) {
	d := Default()
	if d.ImageGenPlacement != "" || d.GenEditPlacement != "" || d.ImageMulticard() || d.EditMulticard() {
		t.Fatalf("the default config binds no placement: %q %q", d.ImageGenPlacement, d.GenEditPlacement)
	}
	for _, f := range []string{"../../setup/templates/config.json"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Skipf("the shipped template is not beside this package: %v", err)
		}
		if strings.Contains(string(b), "_placement") {
			t.Errorf("%s binds a placement: no box changes behaviour until its operator measures the rows", f)
		}
	}
}

// The families the load-time warning calls recipe-less are the runner's: render/multicard-recipes.json lists the families that have a row that is not held back, per route.
func TestThePlacementWarningKnowsExactlyTheFamiliesThatHaveARecipe(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "render", "multicard-recipes.json"))
	if err != nil {
		t.Skipf("the render folder is not beside this package: %v", err)
	}
	var file struct {
		Rows []struct{ Route, Family, Blocked string }
	}
	if err := json.Unmarshal(src, &file); err != nil {
		t.Fatal(err)
	}
	img, edit := map[string]bool{}, map[string]bool{}
	for _, r := range file.Rows {
		if r.Blocked != "" {
			continue // a row that is held back is no recipe: a family whose rows are all held back has none, and the load-time warning says so
		}
		switch r.Route {
		case "image":
			img[r.Family] = true
		case "edit":
			edit[r.Family] = true
		default:
			t.Fatalf("a recipe for the route %q", r.Route)
		}
	}
	eq := func(a, b map[string]bool) bool {
		if len(a) != len(b) {
			return false
		}
		for k := range a {
			if !b[k] {
				return false
			}
		}
		return true
	}
	if !eq(img, imageRecipeFamilies) || !eq(edit, editRecipeFamilies) {
		t.Fatalf("recipe families: image %v edit %v in render/multicard-recipes.json, image %v edit %v in the load-time warning", img, edit, imageRecipeFamilies, editRecipeFamilies)
	}
}
