package mediacap

// What an image family IS (recipe.go). The fixtures reproduce the image blocks of three nodes as they stood in the
// config text read on 2026-10-09 and 2026-10-10 (two of them from stored extracts), with the machines reduced to
// node letters: node A is the workstation that submits heavy media, node B the headless node that is the first
// overflow target, node C the editor node that is the second. They are config text, not a render, and the extract of
// node B was cut inside the block this matters for. Sizes are synthetic and equal for equal names (a copy of a file is
// the same file); a test that needs a different size says so.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// recipeSizes sizes the fixture's weight files by "<class>/<name>".
var recipeSizes = map[string]int64{
	"diffusion_models/qwen_image_2.1_uc_bf16.safetensors":         40_900_000_000,
	"diffusion_models/qwen-image-2.1-UC-int8_convrot.safetensors": 21_700_000_000,
	"diffusion_models/qwen-image-2.1-UC-NVFP4.safetensors":        11_800_000_000,
	"diffusion_models/krea2_turbo_bf16.safetensors":               24_480_000_000,
	"text_encoders/qwen3vl_8b_bf16.safetensors":                   17_530_000_000,
	"text_encoders/qwen3vl_4b_bf16.safetensors":                   8_890_000_000,
	"vae/qwen_image_2.1_vae_bf16.safetensors":                     320_000_000,
	"vae/qwen_image_vae.safetensors":                              254_000_000,
}

// recipeEnv is a models root whose classes are the ones the graphs load from, and a stat that answers from recipeSizes
// plus extra (a test's own files).
func recipeEnv(extra map[string]int64) ([]ModelRoot, func(string) (int64, bool)) {
	classes := map[string][]string{}
	for _, c := range []string{"checkpoints", "diffusion_models", "unet", "text_encoders", "clip", "vae", "loras"} {
		classes[c] = []string{c}
	}
	roots := []ModelRoot{{Label: "models", Dir: filepath.Join(string(filepath.Separator), "models"), Classes: classes}}
	stat := func(path string) (int64, bool) {
		p := filepath.ToSlash(path)
		for k, n := range recipeSizes {
			if strings.HasSuffix(p, "/"+k) {
				return n, true
			}
		}
		for k, n := range extra {
			if strings.HasSuffix(p, "/"+k) {
				return n, true
			}
		}
		return 0, false
	}
	return roots, stat
}

// nodeBlocks are the family blocks of the three fixture nodes.
const (
	// A: the workstation. Leaves the scheduler to the builder (unset), 900 s timeout, pinned to a card.
	nodeAFast = `{"license":"Qwen Research License","commercial_use":false,"imagegen_family":"qwen-image-2.1",
		"imagegen_ckpt":"qwen-image-2.1-UC-int8_convrot.safetensors","imagegen_clip":"qwen3vl_8b_bf16.safetensors",
		"imagegen_vae":"qwen_image_2.1_vae_bf16.safetensors","imagegen_steps":40,"imagegen_cfg":1,"imagegen_sampler":"euler",
		"imagegen_schedule":"official","imagegen_timeout_sec":900,"comfy_cuda_device":"2","comfy_dynamic_vram":"on"}`
	nodeABf16 = `{"license":"Qwen Research License","commercial_use":false,"imagegen_family":"qwen-image-2.1",
		"imagegen_ckpt":"qwen_image_2.1_uc_bf16.safetensors","imagegen_clip":"qwen3vl_8b_bf16.safetensors",
		"imagegen_vae":"qwen_image_2.1_vae_bf16.safetensors","imagegen_steps":40,"imagegen_cfg":1,"imagegen_sampler":"euler",
		"imagegen_schedule":"official","imagegen_timeout_sec":900,"comfy_cuda_device":"2","comfy_dynamic_vram":"on"}`
	// B: the headless node's one named family. Its stored extract was cut inside this block, so the scheduler is read
	// here as unset; the live acceptance reads image_recipes from the node first (docs/systems/media-generation.md).
	nodeB = `{"license":"Qwen Research License","commercial_use":false,"imagegen_family":"qwen-image-2.1",
		"imagegen_ckpt":"qwen-image-2.1-UC-int8_convrot.safetensors","imagegen_clip":"qwen3vl_8b_bf16.safetensors",
		"imagegen_vae":"qwen_image_2.1_vae_bf16.safetensors","imagegen_steps":40,"imagegen_cfg":1,"imagegen_sampler":"euler",
		"imagegen_schedule":"official"}`
	// C: the editor node. SPELLS the scheduler out ("simple", the builder's default), reserves 1 GiB, a 2400 s timeout.
	nodeCBf16 = `{"license":"Qwen Research License","commercial_use":false,"imagegen_engine":"","imagegen_family":"qwen-image-2.1",
		"imagegen_ckpt":"qwen_image_2.1_uc_bf16.safetensors","imagegen_clip":"qwen3vl_8b_bf16.safetensors",
		"imagegen_vae":"qwen_image_2.1_vae_bf16.safetensors","imagegen_steps":40,"imagegen_cfg":1,"imagegen_sampler":"euler",
		"imagegen_scheduler":"simple","imagegen_schedule":"official","imagegen_reserve_vram":1.0,"imagegen_timeout_sec":2400,
		"comfy_dynamic_vram":"on"}`
	// C's "fast" family is a different quantization of the same graph.
	nodeCFast = `{"license":"Qwen Research License","commercial_use":false,"imagegen_engine":"","imagegen_family":"qwen-image-2.1",
		"imagegen_ckpt":"qwen-image-2.1-UC-NVFP4.safetensors","imagegen_clip":"qwen3vl_8b_bf16.safetensors",
		"imagegen_vae":"qwen_image_2.1_vae_bf16.safetensors","imagegen_steps":40,"imagegen_cfg":1,"imagegen_sampler":"euler",
		"imagegen_scheduler":"simple","imagegen_schedule":"official","imagegen_reserve_vram":1.0,"imagegen_timeout_sec":2400,
		"comfy_dynamic_vram":"on"}`
)

// familyRecipe is the recipe of the named family of a node whose families are blocks (name -> JSON overlay).
func familyRecipe(t *testing.T, blocks map[string]string, name string, extra map[string]int64) Recipe {
	t.Helper()
	cfg := bare()
	cfg.ImageGenScript = "render/comfy-generate.mjs"
	cfg.ImageGenFamily = "krea2"
	cfg.ImageGenCkpt = "krea2_turbo_bf16.safetensors"
	cfg.ImageGenVAE = "qwen_image_vae.safetensors"
	cfg.ImageGenSteps, cfg.ImageGenCFG = 8, 1
	cfg.ImageGenFamilies = map[string]config.FamilyOverlay{}
	for n, js := range blocks {
		cfg.ImageGenFamilies[n] = overlay(t, js)
	}
	eff, fi, err := cfg.ResolveImageFamily(name)
	if err != nil {
		t.Fatalf("family %q: %v", name, err)
	}
	roots, stat := recipeEnv(extra)
	r, ok := ImageRecipe(eff, fi, roots, stat)
	if !ok {
		t.Fatalf("family %q has no recipe", name)
	}
	return r
}

// THE LIVE SHAPE (fast family). Node A's qwen-image-2.1-fast and node B's qwen-image-2.1 are the same int8 build with
// the same sampling, and B leaves the scheduler to the builder: they match, and a call may overflow from one to the
// other. Under another NAME on each side: a family is its recipe, not its label.
func TestWorkstationFastFamilyMatchesTheHeadlessNodesBlock(t *testing.T) {
	a := familyRecipe(t, map[string]string{"qwen-image-2.1-fast": nodeAFast}, "qwen-image-2.1-fast", nil)
	b := familyRecipe(t, map[string]string{"qwen-image-2.1": nodeB}, "qwen-image-2.1", nil)
	if a.Digest() != b.Digest() {
		t.Fatalf("the same build with the same sampling must digest alike; differs in: %v", a.Diff(b))
	}
	if d := a.Diff(b); len(d) != 0 {
		t.Errorf("Diff of two matching recipes must be empty: %v", d)
	}
}

// THE LIVE SHAPE (bf16 family). Node A leaves the scheduler unset and node C writes "simple" out: the same render. A
// digest over the raw keys would have refused this match for a cosmetic reason.
func TestWorkstationBf16FamilyMatchesTheEditorNodesBlockWithItsExplicitScheduler(t *testing.T) {
	a := familyRecipe(t, map[string]string{"qwen-image-2.1": nodeABf16}, "qwen-image-2.1", nil)
	c := familyRecipe(t, map[string]string{"qwen-image-2.1": nodeCBf16}, "qwen-image-2.1", nil)
	if a.Digest() != c.Digest() {
		t.Fatalf("an unset scheduler and the builder's default written out are one recipe; differs in: %v", a.Diff(c))
	}
	if a.Scheduler != "simple" || c.Scheduler != "simple" {
		t.Errorf("both resolve to the builder's scheduler: %q / %q", a.Scheduler, c.Scheduler)
	}
	// What the binding actually set is kept beside the digest, not in it.
	if reflect.DeepEqual(a.Explicit, c.Explicit) {
		t.Errorf("A set no scheduler and C did; Explicit must say so: %v / %v", a.Explicit, c.Explicit)
	}
}

// A different quantization under the same family name is a different recipe: the editor node's fast family is an
// NVFP4 build, the workstation's an int8 one, and the diff names the checkpoint. Strict identity never substitutes.
func TestADifferentQuantUnderTheSameNameNeverMatches(t *testing.T) {
	a := familyRecipe(t, map[string]string{"qwen-image-2.1-fast": nodeAFast}, "qwen-image-2.1-fast", nil)
	c := familyRecipe(t, map[string]string{"qwen-image-2.1-fast": nodeCFast}, "qwen-image-2.1-fast", nil)
	if a.Digest() == c.Digest() {
		t.Fatal("an int8 build and an NVFP4 build are not one recipe")
	}
	d := a.Diff(c)
	if len(d) != 1 || !strings.HasPrefix(d[0], "imagegen_ckpt: qwen-image-2.1-UC-int8_convrot.safetensors vs qwen-image-2.1-UC-NVFP4.safetensors") {
		t.Errorf("the diff must name the checkpoint and only it: %v", d)
	}
}

// The bf16 build and the int8 build of the same graph differ in the checkpoint, and the diff says so in the config key's
// own word (the cluster answer prints it).
func TestStrictMismatchDiffNamesTheCheckpointKey(t *testing.T) {
	bf16 := familyRecipe(t, map[string]string{"qwen-image-2.1": nodeABf16}, "qwen-image-2.1", nil)
	int8 := familyRecipe(t, map[string]string{"qwen-image-2.1": nodeB}, "qwen-image-2.1", nil)
	d := bf16.Diff(int8)
	if len(d) != 1 || !strings.Contains(d[0], "imagegen_ckpt: qwen_image_2.1_uc_bf16.safetensors vs qwen-image-2.1-UC-int8_convrot.safetensors") {
		t.Fatalf("diff = %v", d)
	}
}

// A binding that leaves the Qwen-Image-2.1 sampling keys unset resolves to the builder's recipe, and a binding that
// writes exactly those values out is the same recipe. A key the binding sets differently is not.
func TestRecipeFillsQwen21BuilderDefaults(t *testing.T) {
	const bare21 = `{"license":"L","commercial_use":false,"imagegen_family":"qwen-image-2.1",
		"imagegen_ckpt":"qwen_image_2.1_uc_bf16.safetensors","imagegen_clip":"qwen3vl_8b_bf16.safetensors",
		"imagegen_vae":"qwen_image_2.1_vae_bf16.safetensors"}`
	const spelled = `{"license":"L","commercial_use":false,"imagegen_family":"qwen-image-2.1",
		"imagegen_ckpt":"qwen_image_2.1_uc_bf16.safetensors","imagegen_clip":"qwen3vl_8b_bf16.safetensors",
		"imagegen_vae":"qwen_image_2.1_vae_bf16.safetensors","imagegen_steps":40,"imagegen_cfg":1.0,"imagegen_sampler":"euler",
		"imagegen_scheduler":"simple","imagegen_schedule":"official"}`
	const turbo = `{"license":"L","commercial_use":false,"imagegen_family":"qwen-image-2.1",
		"imagegen_ckpt":"qwen_image_2.1_uc_bf16.safetensors","imagegen_clip":"qwen3vl_8b_bf16.safetensors",
		"imagegen_vae":"qwen_image_2.1_vae_bf16.safetensors","imagegen_steps":8,"imagegen_cfg":1,"imagegen_schedule":"turbo"}`
	r := familyRecipe(t, map[string]string{"x": bare21}, "x", nil)
	if r.Steps != 40 || r.CFG != 1 || r.Sampler != "euler" || r.Scheduler != "simple" || r.Schedule != "official" {
		t.Fatalf("resolved sampling = %d / %v / %q / %q / %q, want the builder's 40 / 1 / euler / simple / official", r.Steps, r.CFG, r.Sampler, r.Scheduler, r.Schedule)
	}
	if len(r.Explicit) != 0 {
		t.Errorf("the binding set none of them: %v", r.Explicit)
	}
	s := familyRecipe(t, map[string]string{"x": spelled}, "x", nil)
	if s.Digest() != r.Digest() {
		t.Errorf("the defaults written out are the same recipe; differs in %v", r.Diff(s))
	}
	if len(s.Explicit) != 5 {
		t.Errorf("the binding set all five: %v", s.Explicit)
	}
	tb := familyRecipe(t, map[string]string{"x": turbo}, "x", nil)
	if tb.Digest() == r.Digest() || tb.Steps != 8 || tb.Schedule != "turbo" {
		t.Errorf("a turbo binding keeps its own steps and schedule: %+v", tb)
	}
}

// Any other graph family has no resolved defaults: an unset key stays unset, so a binding that writes the sampler out
// is NOT the one that leaves it to the builder (stricter, never looser; the diff names it).
func TestOtherGraphFamiliesLeaveUnsetKeysUnset(t *testing.T) {
	const unsetSampler = `{"license":"L","commercial_use":true,"imagegen_family":"krea2","imagegen_ckpt":"krea2_turbo_bf16.safetensors","imagegen_steps":8,"imagegen_cfg":1}`
	const setSampler = `{"license":"L","commercial_use":true,"imagegen_family":"krea2","imagegen_ckpt":"krea2_turbo_bf16.safetensors","imagegen_steps":8,"imagegen_cfg":1,"imagegen_sampler":"euler"}`
	a := familyRecipe(t, map[string]string{"x": unsetSampler}, "x", nil)
	b := familyRecipe(t, map[string]string{"x": setSampler}, "x", nil)
	if a.Sampler != "" || a.Digest() == b.Digest() {
		t.Fatalf("an unset sampler on krea2 stays unset: %q", a.Sampler)
	}
	if d := a.Diff(b); len(d) != 1 || d[0] != "imagegen_sampler: unset vs euler" {
		t.Errorf("diff = %v", d)
	}
}

// The files carry their sizes: a same-named file of another size is another recipe (a truncated copy, a rebuilt file),
// and a file that is not there makes the recipe incomplete, which never matches.
func TestExactNeedsSameBytes(t *testing.T) {
	blocks := map[string]string{"qwen-image-2.1": nodeABf16}
	a := familyRecipe(t, blocks, "qwen-image-2.1", nil)
	cfg := bare()
	cfg.ImageGenScript = "render/comfy-generate.mjs"
	cfg.ImageGenFamilies = map[string]config.FamilyOverlay{"qwen-image-2.1": overlay(t, nodeABf16)}
	eff, fi, err := cfg.ResolveImageFamily("qwen-image-2.1")
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := recipeEnv(nil)
	short := func(path string) (int64, bool) {
		p := filepath.ToSlash(path)
		if strings.HasSuffix(p, "/diffusion_models/qwen_image_2.1_uc_bf16.safetensors") {
			return 40_900_000_001, true // one byte off
		}
		for k, n := range recipeSizes {
			if strings.HasSuffix(p, "/"+k) {
				return n, true
			}
		}
		return 0, false
	}
	b, _ := ImageRecipe(eff, fi, roots, short)
	if a.Digest() == b.Digest() {
		t.Fatal("a file one byte off is not the same file")
	}
	if d := a.Diff(b); len(d) != 1 || !strings.Contains(d[0], "is 40900000000 vs 40900000001 bytes") {
		t.Errorf("the diff must say the sizes: %v", d)
	}
	gone := func(path string) (int64, bool) {
		if strings.Contains(filepath.ToSlash(path), "text_encoders") {
			return 0, false
		}
		return short(path)
	}
	c, _ := ImageRecipe(eff, fi, roots, gone)
	if m := c.Missing(); len(m) != 1 || m[0] != "qwen3vl_8b_bf16.safetensors" {
		t.Errorf("Missing = %v, want the text encoder", m)
	}
	if len(a.Missing()) != 0 {
		t.Errorf("a complete recipe has nothing missing: %v", a.Missing())
	}
}

// No recipe exists for an sd.cpp binding: ComfyUI never matches it.
func TestRecipeIsNotDefinedForAnSdcppBinding(t *testing.T) {
	cfg := bare()
	cfg.ImageGenEngine = "sdcpp"
	roots, stat := recipeEnv(nil)
	if _, ok := ImageRecipe(cfg, config.FamilyInfo{Name: "z", Engine: "sdcpp"}, roots, stat); ok {
		t.Error("an sdcpp family has a recipe")
	}
	cfg.ImageGenEngine = ""
	if _, ok := ImageRecipe(cfg, config.FamilyInfo{Name: "z", Engine: "sdcpp"}, roots, stat); ok {
		t.Error("a family whose info says sdcpp has a recipe")
	}
	if _, ok := ImageRecipe(cfg, config.FamilyInfo{Name: "z", Engine: "comfyui"}, roots, stat); !ok {
		t.Error("a ComfyUI binding must have a recipe")
	}
}

// renderKeys are the image overlay's clear-list keys a recipe digests: everything that decides which weights load
// or how they sample, plus the license the result is tagged with. localKeys are the clear-list keys that are the
// node's own business or that only matter on the other engine. Every key of the clear list is in exactly one of them
// (TestRecipeClassifiesEveryClearListKey), so a new key cannot join the list without a decision.
var (
	renderKeys = map[string]any{
		"imagegen_ckpt": "other_ckpt.safetensors", "imagegen_family": "krea2", "imagegen_vae": "other_vae.safetensors",
		"imagegen_steps": 41, "imagegen_cfg": 2.5, "imagegen_sampler": "dpmpp_2m", "imagegen_scheduler": "karras",
		"imagegen_preset": "lightning4", "imagegen_clip": "other_clip.safetensors", "imagegen_lora": "l.safetensors",
		"imagegen_lora_strength": 0.7, "imagegen_shift": 3.1, "imagegen_schedule": "comfy",
		"imagegen_license": "Another License", "imagegen_commercial_use": true,
	}
	localKeys = map[string]any{
		"imagegen_pool_vvram_gb": 12.0, "imagegen_pool_compute": "cuda:0", "imagegen_pool_donor": "cuda:1",
		"sdcpp_model": "m.gguf", "sdcpp_model_kind": "dit", "sdcpp_vae": "v.safetensors", "sdcpp_clip_l": "l.safetensors",
		"sdcpp_clip_g": "g.safetensors", "sdcpp_t5xxl": "t5.safetensors", "sdcpp_llm": "llm.gguf", "sdcpp_extra_args": []string{"--x"},
		// not in the clear list at all: inherited, or the node's launch
		"imagegen_timeout_sec": 7200, "imagegen_reserve_vram": 3.5, "imagegen_script": "render/other.mjs",
		"comfy_cuda_device": "1", "comfy_dynamic_vram": "off", "comfy_extra_args": "--lowvram", "comfy_dir": "/elsewhere",
	}
)

func withKey(t *testing.T, cfg config.Config, key string, val any) config.Config {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m[key] = val
	raw, _ = json.Marshal(m)
	var out config.Config
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return out
}

// baseBinding is a Qwen-Image-2.1 binding with every render key set, so a change to any one of them shows.
func baseBinding() config.Config {
	cfg := bare()
	cfg.ImageGenScript = "render/comfy-generate.mjs"
	cfg.ImageGenFamily = config.FamilyQwenImage21
	cfg.ImageGenCkpt = "qwen_image_2.1_uc_bf16.safetensors"
	cfg.ImageGenCLIP = "qwen3vl_8b_bf16.safetensors"
	cfg.ImageGenVAE = "qwen_image_2.1_vae_bf16.safetensors"
	cfg.ImageGenSteps, cfg.ImageGenCFG = 40, 1
	cfg.ImageGenSampler, cfg.ImageGenScheduler, cfg.ImageGenSchedule = "euler", "simple", "official"
	cfg.ImageGenPreset, cfg.ImageGenLoRA, cfg.ImageGenLoRAStrength, cfg.ImageGenShift = "full", "base_lora.safetensors", 0.9, 1.5
	lic, no := "Qwen Research License", false
	cfg.ImageGenLicense, cfg.ImageGenCommercialUse = lic, &no
	return cfg
}

func recipeOf(t *testing.T, cfg config.Config) Recipe {
	t.Helper()
	_, fi, err := cfg.ResolveImageFamily("")
	if err != nil {
		t.Fatal(err)
	}
	roots, stat := recipeEnv(map[string]int64{"loras/base_lora.safetensors": 1_000, "loras/l.safetensors": 2_000,
		"checkpoints/other_ckpt.safetensors": 5, "diffusion_models/other_ckpt.safetensors": 5,
		"vae/other_vae.safetensors": 6, "text_encoders/other_clip.safetensors": 7})
	r, ok := ImageRecipe(cfg, fi, roots, stat)
	if !ok {
		t.Fatal("no recipe")
	}
	return r
}

// THE CLASSIFICATION IS COMPLETE: every key of the image overlay's clear list is decided here, as part of what a
// family is or as the node's own business. A new model-binding key fails this test until someone decides.
func TestRecipeClassifiesEveryClearListKey(t *testing.T) {
	for _, k := range config.ImageOverlayClearKeys() {
		_, render := renderKeys[k]
		_, local := localKeys[k]
		if render == local {
			t.Errorf("clear-list key %q must be classified as exactly one of: a render key (it changes the digest) or a node-local one (it does not); it is %v / %v", k, render, local)
		}
	}
	clear := map[string]bool{}
	for _, k := range config.ImageOverlayClearKeys() {
		clear[k] = true
	}
	for k := range renderKeys {
		if !clear[k] {
			t.Errorf("render key %q is not in the overlay's clear list: the table is stale", k)
		}
	}
}

// Every render key changes the digest.
func TestRecipeChangesOnEveryRenderKey(t *testing.T) {
	base := baseBinding()
	want := recipeOf(t, base).Digest()
	keys := make([]string, 0, len(renderKeys))
	for k := range renderKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		changed := recipeOf(t, withKey(t, base, k, renderKeys[k]))
		if changed.Digest() == want {
			t.Errorf("changing %s did not change the recipe", k)
		}
	}
}

// No node-local key changes the digest: which card, which timeout, which launch flags and where the weights are staged
// do not change the pixels a seed renders.
func TestRecipeIgnoresNodeLocalKeys(t *testing.T) {
	base := baseBinding()
	want := recipeOf(t, base).Digest()
	keys := make([]string, 0, len(localKeys))
	for k := range localKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if got := recipeOf(t, withKey(t, base, k, localKeys[k])).Digest(); got != want {
			t.Errorf("changing the node-local key %s changed the recipe", k)
		}
	}
}

// The recipe's role set: a binding that names every file lists them in role order, and the builder's companions fill
// in the ones it leaves out (krea2 loads a text encoder and a VAE that no key names).
func TestRecipeListsTheFilesTheGraphLoads(t *testing.T) {
	r := recipeOf(t, baseBinding())
	var roles []string
	for _, f := range r.Files {
		roles = append(roles, f.Role+"="+f.Name+":"+strconv.FormatInt(f.Bytes, 10))
	}
	want := []string{"ckpt=qwen_image_2.1_uc_bf16.safetensors:40900000000", "clip=qwen3vl_8b_bf16.safetensors:17530000000",
		"lora=base_lora.safetensors:1000", "vae=qwen_image_2.1_vae_bf16.safetensors:320000000"}
	if !reflect.DeepEqual(roles, want) {
		t.Errorf("files = %v, want %v", roles, want)
	}
	k := bare()
	k.ImageGenScript = "render/comfy-generate.mjs"
	k.ImageGenFamily, k.ImageGenCkpt = "krea2", "krea2_turbo_bf16.safetensors"
	k.ImageGenSteps, k.ImageGenCFG = 8, 1
	rk := recipeOf(t, k)
	var got []string
	for _, f := range rk.Files {
		got = append(got, f.Role+"="+f.Name)
	}
	wantK := []string{"ckpt=krea2_turbo_bf16.safetensors", "clip=qwen3vl_4b_bf16.safetensors", "vae=qwen_image_vae.safetensors"}
	if !reflect.DeepEqual(got, wantK) {
		t.Errorf("krea2 with no companions bound loads the builder's: %v, want %v", got, wantK)
	}
}

// steps and cfg travel together on three graphs; a per-request steps needs the node's binding to have set cfg.
func TestStepsAndCFGPairingIsKnownPerGraph(t *testing.T) {
	r := recipeOf(t, baseBinding())
	if !r.PairsStepsAndCFG() || !r.SetsCFG() {
		t.Errorf("qwen-image-2.1 pairs steps and cfg and this binding set cfg: %+v", r)
	}
	b := baseBinding()
	b.ImageGenSteps, b.ImageGenCFG = 0, 0
	if r := recipeOf(t, b); !r.PairsStepsAndCFG() || r.SetsCFG() {
		t.Errorf("an unset cfg is the builder's: %+v", r)
	}
	h := baseBinding()
	h.ImageGenFamily = "hidream-o1"
	if recipeOf(t, h).PairsStepsAndCFG() {
		t.Error("hidream-o1 has no pairing rule")
	}
}

// The digest does not depend on how a recipe was assembled: file order and the Explicit list are not in it.
func TestDigestIsCanonical(t *testing.T) {
	r := recipeOf(t, baseBinding())
	shuffled := r
	shuffled.Files = []RecipeFile{r.Files[3], r.Files[1], r.Files[0], r.Files[2]}
	shuffled.Explicit = []string{"schedule"}
	if r.Digest() != shuffled.Digest() {
		t.Error("file order and Explicit are not part of the digest")
	}
	if len(r.Digest()) != 64 {
		t.Errorf("digest = %q, want a sha256 in hex", r.Digest())
	}
}

// THE DEFAULTS TABLE IS THE BUILDER'S. The values filled in for an unset Qwen-Image-2.1 key are read out of the
// two files that render, so the Go table cannot drift from the code that renders.
func TestBuilderDefaultsMatchTheJSConstants(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("..", "..", "render", name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	wf := read("wf-qwen-image-21.mjs")
	m := regexp.MustCompile(`QWEN_IMAGE_21_RECIPE = Object\.freeze\(\{ steps: (\d+), cfg: ([0-9.]+), sampler: "([^"]+)", scheduler: "([^"]+)" \}\)`).FindStringSubmatch(wf)
	if m == nil {
		t.Fatal("QWEN_IMAGE_21_RECIPE not found in wf-qwen-image-21.mjs: the pin test must be updated with the builder")
	}
	steps, _ := strconv.Atoi(m[1])
	cfg, _ := strconv.ParseFloat(m[2], 64)
	render := read("comfy-render.mjs")
	i := strings.Index(render, `flags.family === "qwen-image-2.1"`)
	if i < 0 {
		t.Fatal("the qwen-image-2.1 branch of comfy-render.mjs not found")
	}
	sm := regexp.MustCompile(`const schedule = flags\.schedule \|\| "([^"]+)"`).FindStringSubmatch(render[i:])
	if sm == nil {
		t.Fatal("the schedule default of the qwen-image-2.1 branch not found")
	}
	got := builderSampling[config.FamilyQwenImage21]
	want := samplingDefaults{Steps: steps, CFG: cfg, Sampler: m[3], Scheduler: m[4], Schedule: sm[1]}
	if got != want {
		t.Errorf("builderSampling[qwen-image-2.1] = %+v, the builder ships %+v", got, want)
	}
	// And the pairing rule: the runner refuses steps without cfg for exactly the graphs in stepsAndCFGTogether.
	if n := strings.Count(render, "takes --steps and --cfg together"); n != len(stepsAndCFGTogether) {
		t.Errorf("comfy-render.mjs states the steps/cfg pairing %d times, stepsAndCFGTogether lists %d graphs", n, len(stepsAndCFGTogether))
	}
	for fam := range stepsAndCFGTogether {
		if !strings.Contains(render, "--family "+fam+" takes --steps and --cfg together") {
			t.Errorf("comfy-render.mjs does not state the pairing rule for %s", fam)
		}
	}
}
