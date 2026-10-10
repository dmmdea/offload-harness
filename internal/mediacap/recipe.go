package mediacap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
)

// What an image family IS (P0 plan, S1; ADR 0082).
//
// A node that takes another machine's image job must render the SAME image the caller's own lane would have: the
// same weights, the same sampling. Two nodes can name a family alike and bind different files under it (one node's
// qwen-image-2.1 is the bf16 DiT and another's is an int8 build), and a name says nothing about a file that was
// truncated in a copy. So a family is identified by its RECIPE: the weight files it loads, each with its byte
// size, and the sampling it renders with, digested over RESOLVED values and compared strictly.
//
// RESOLVED, not raw. The same render has more than one config spelling: a binding that leaves the scheduler unset
// gets the builder's own default, and a binding that writes that default out explicitly renders identically. A
// digest over the raw keys would refuse the match for a cosmetic reason (measured against the live blocks of two
// nodes that render the same bf16 family, one of which spells the scheduler out), so a key a graph family's builder
// fills in is filled in here first, from the table below, which a test pins to the constants the builder ships.
//
// Not in the recipe, on purpose: everything that is the NODE's own business and cannot change the pixels it
// renders for a given seed: comfy_* (which card, launch flags), timeouts, scripts, reserve_vram and the pool keys
// (where the weights are staged, not which weights). The recipe's key set is the ComfyUI members of the image
// overlay's clear list less those; TestRecipeChangesOnEveryRenderKey fails when a key joins that list unclassified.

// RecipeVersion is stamped into the digest, so a change to what a recipe digests can never collide with an older one.
const RecipeVersion = 1

// EngineComfyUI is the only engine a recipe is defined for: an sd.cpp binding renders other pixels for the same
// weights, so a ComfyUI recipe never matches it and a recipe is not built for it.
const EngineComfyUI = "comfyui"

// The file roles of a recipe.
const (
	RecipeRoleCkpt = "ckpt"
	RecipeRoleClip = "clip"
	RecipeRoleVAE  = "vae"
	RecipeRoleLoRA = "lora"
)

// RecipeFile is one weight file the graph loads: its role, its ComfyUI-relative name and its size on this machine
// (-1 when it is not found: a recipe with a missing file is never a match).
type RecipeFile struct {
	Role  string `json:"role"`
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// Recipe is the resolved identity of one image binding.
type Recipe struct {
	Engine string       `json:"engine"`
	Graph  string       `json:"graph"`
	Files  []RecipeFile `json:"files"`
	Preset string       `json:"preset,omitempty"`
	// The sampling the render uses. For a graph family in builderSampling a key the binding leaves unset is the
	// builder's default (Explicit says which keys the binding actually set); for any other graph family an unset
	// key stays unset, which is stricter, never looser.
	Steps         int     `json:"steps,omitempty"`
	CFG           float64 `json:"cfg,omitempty"`
	Sampler       string  `json:"sampler,omitempty"`
	Scheduler     string  `json:"scheduler,omitempty"`
	Schedule      string  `json:"schedule,omitempty"`
	Shift         float64 `json:"shift,omitempty"`
	LoRAStrength  float64 `json:"lora_strength,omitempty"`
	License       string  `json:"license,omitempty"`
	CommercialUse *bool   `json:"commercial_use,omitempty"`
	// Explicit lists the sampling keys (steps, cfg, sampler, scheduler, schedule) the binding SET, as opposed to
	// taking the builder's default. It is NOT in the digest. It exists for the one place the two spellings render
	// differently: a request that carries its own `steps` is only valid on a graph that takes steps and cfg
	// together when the binding set cfg too (see PairsStepsAndCFG).
	Explicit []string `json:"explicit,omitempty"`
}

// samplingDefaults are the values a graph family's runner fills in for a sampling key the binding leaves unset.
type samplingDefaults struct {
	Steps                        int
	CFG                          float64
	Sampler, Scheduler, Schedule string
}

// builderSampling is the table the digest resolves against, one row per graph family whose defaults are known.
// Qwen-Image-2.1: 40 steps, cfg 1.0, euler and the "simple" scheduler are QWEN_IMAGE_21_RECIPE in
// render/wf-qwen-image-21.mjs, and the "official" schedule is comfy-render.mjs's own fallback.
// TestBuilderDefaultsMatchTheJSConstants reads both files and fails when this table and the code that renders
// disagree. A family that is not here has no resolved defaults: its unset keys stay unset in the digest.
var builderSampling = map[string]samplingDefaults{
	config.FamilyQwenImage21: {Steps: 40, CFG: 1.0, Sampler: "euler", Scheduler: "simple", Schedule: "official"},
}

// stepsAndCFGTogether are the graph families whose runner refuses a `steps` without a `cfg` (or the reverse):
// render/comfy-render.mjs throws a usage error for exactly these three.
var stepsAndCFGTogether = map[string]bool{"krea2": true, "qwen-image": true, config.FamilyQwenImage21: true}

// PairsStepsAndCFG reports whether this recipe's graph takes steps and cfg together, so that a per-request `steps`
// renders only when the binding also set cfg. A node whose binding leaves cfg to the builder's default is a
// correct target for a request without `steps` and a failing one for a request with it.
func (r Recipe) PairsStepsAndCFG() bool { return stepsAndCFGTogether[r.Graph] }

// SetsCFG reports whether the binding set cfg itself (rather than taking the builder's default).
func (r Recipe) SetsCFG() bool {
	for _, k := range r.Explicit {
		if k == "cfg" {
			return true
		}
	}
	return false
}

// roledFile is a weight file with the role it plays in the recipe.
type roledFile struct {
	role string
	file ModelFile
}

// imageRecipeFiles are the weight files an image binding's graph loads: the checkpoint or UNet, the text encoder,
// the VAE and the LoRA, each as the binding names it or as the graph family's builder defaults it. A binding that
// names no checkpoint (a generic SDXL graph with the builder's own) contributes no ckpt row, and a VAE of "builtin"
// or "none" is the checkpoint's own. Beside ImageModelFiles, which the host-RAM estimate reads and which sizes only
// the two files that dominate it (ImageModelFiles is reused here for those two, so the two readers cannot name
// different files).
func imageRecipeFiles(cfg config.Config) []roledFile {
	var out []roledFile
	for _, f := range ImageModelFiles(cfg) {
		role := RecipeRoleCkpt
		if f.Role == RoleTextEncoder {
			role = RecipeRoleClip
		}
		out = append(out, roledFile{role, f})
	}
	vae, label := strings.TrimSpace(cfg.ImageGenVAE), "imagegen_vae"
	if vae == "" {
		if c, ok := builderCompanions[cfg.ImageGenFamily]; ok {
			vae, label = c.vae, "imagegen_vae ("+cfg.ImageGenFamily+" default)"
		}
	}
	if !builtinName(vae) {
		out = append(out, roledFile{RecipeRoleVAE, ModelFile{Label: label, Name: vae, Classes: expectedClasses["imagegen_vae"], Role: RoleOther}})
	}
	lora, label := strings.TrimSpace(cfg.ImageGenLoRA), "imagegen_lora"
	if lora == "" && cfg.ImageGenFamily == "qwen-image" {
		preset := cfg.ImageGenPreset
		if preset == "" {
			preset = defaultImagePreset
		}
		lora, label = imagePresetLoRAs[preset], "imagegen_lora (preset "+preset+")"
	}
	if !builtinName(lora) {
		out = append(out, roledFile{RecipeRoleLoRA, ModelFile{Label: label, Name: lora, Classes: expectedClasses["imagegen_lora"], Role: RoleOther}})
	}
	return out
}

// ImageRecipe is the recipe of an EFFECTIVE image binding (the config a request's family resolved to, with fi its
// FamilyInfo). roots are the models roots the files are looked up under (ModelRoots of the ComfyUI directory) and
// stat reads a size (nil = the file system); both are the caller's so a test needs no model tree. ok is false for a
// binding that is not a ComfyUI one: no recipe is defined for it, and it never matches another.
func ImageRecipe(cfg config.Config, fi config.FamilyInfo, roots []ModelRoot, stat func(path string) (int64, bool)) (Recipe, bool) {
	if fi.Engine != "" && fi.Engine != EngineComfyUI {
		return Recipe{}, false
	}
	if cfg.ImageGenEngine != "" && cfg.ImageGenEngine != "comfy" && cfg.ImageGenEngine != EngineComfyUI {
		return Recipe{}, false
	}
	r := Recipe{
		Engine: EngineComfyUI, Graph: strings.TrimSpace(cfg.ImageGenFamily), Preset: strings.TrimSpace(cfg.ImageGenPreset),
		Steps: cfg.ImageGenSteps, CFG: cfg.ImageGenCFG,
		Sampler: strings.TrimSpace(cfg.ImageGenSampler), Scheduler: strings.TrimSpace(cfg.ImageGenScheduler),
		Schedule: strings.TrimSpace(cfg.ImageGenSchedule),
		Shift:    cfg.ImageGenShift, LoRAStrength: cfg.ImageGenLoRAStrength,
		License: strings.TrimSpace(fi.License), CommercialUse: fi.CommercialUse,
	}
	for _, k := range []struct {
		name string
		set  bool
	}{{"steps", r.Steps > 0}, {"cfg", r.CFG > 0}, {"sampler", r.Sampler != ""}, {"scheduler", r.Scheduler != ""}, {"schedule", r.Schedule != ""}} {
		if k.set {
			r.Explicit = append(r.Explicit, k.name)
		}
	}
	if d, ok := builderSampling[r.Graph]; ok {
		if r.Steps == 0 {
			r.Steps = d.Steps
		}
		if r.CFG == 0 {
			r.CFG = d.CFG
		}
		if r.Sampler == "" {
			r.Sampler = d.Sampler
		}
		if r.Scheduler == "" {
			r.Scheduler = d.Scheduler
		}
		if r.Schedule == "" {
			r.Schedule = d.Schedule
		}
	}
	for _, rf := range imageRecipeFiles(cfg) {
		size, found := ResolveModelFile(roots, rf.file, stat)
		if !found {
			size = -1
		}
		r.Files = append(r.Files, RecipeFile{Role: rf.role, Name: rf.file.Name, Bytes: size})
	}
	sort.SliceStable(r.Files, func(i, j int) bool { return r.Files[i].Role < r.Files[j].Role })
	return r, true
}

// Missing names the files that were not found on the machine that built the recipe. A recipe that is missing a
// file is never a match, on either side of the comparison: nothing can be said to render alike from a weight that
// is not there.
func (r Recipe) Missing() []string {
	var out []string
	for _, f := range r.Files {
		if f.Bytes < 0 {
			out = append(out, f.Name)
		}
	}
	return out
}

// digestForm is the canonical shape that is hashed: fixed field order, no omitted zero, files in role order.
type digestForm struct {
	V             int          `json:"v"`
	Engine        string       `json:"engine"`
	Graph         string       `json:"graph"`
	Files         []RecipeFile `json:"files"`
	Preset        string       `json:"preset"`
	Steps         int          `json:"steps"`
	CFG           float64      `json:"cfg"`
	Sampler       string       `json:"sampler"`
	Scheduler     string       `json:"scheduler"`
	Schedule      string       `json:"schedule"`
	Shift         float64      `json:"shift"`
	LoRAStrength  float64      `json:"lora_strength"`
	License       string       `json:"license"`
	CommercialUse *bool        `json:"commercial_use"`
}

// Digest is the sha256 of the canonical JSON of the resolved recipe (hex). Explicit is not in it.
func (r Recipe) Digest() string {
	files := append([]RecipeFile(nil), r.Files...)
	sort.SliceStable(files, func(i, j int) bool { return files[i].Role < files[j].Role })
	if files == nil {
		files = []RecipeFile{}
	}
	b, _ := json.Marshal(digestForm{V: RecipeVersion, Engine: r.Engine, Graph: r.Graph, Files: files, Preset: r.Preset,
		Steps: r.Steps, CFG: r.CFG, Sampler: r.Sampler, Scheduler: r.Scheduler, Schedule: r.Schedule, Shift: r.Shift,
		LoRAStrength: r.LoRAStrength, License: r.License, CommercialUse: r.CommercialUse})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Diff names, in the config keys' own words and a fixed order, every way o differs from r ("imagegen_ckpt: a vs b").
// Empty when the two digest alike. It is what a refusal prints so the operator sees WHICH key to change, or which
// family to name instead.
func (r Recipe) Diff(o Recipe) []string {
	var d []string
	str := func(key, a, b string) {
		if a != b {
			d = append(d, fmt.Sprintf("%s: %s vs %s", key, shown(a), shown(b)))
		}
	}
	num := func(key string, a, b float64) {
		if a != b {
			d = append(d, fmt.Sprintf("%s: %s vs %s", key, strconv.FormatFloat(a, 'g', -1, 64), strconv.FormatFloat(b, 'g', -1, 64)))
		}
	}
	str("engine", r.Engine, o.Engine)
	str("imagegen_family", r.Graph, o.Graph)
	byRole := func(fs []RecipeFile) map[string]RecipeFile {
		m := map[string]RecipeFile{}
		for _, f := range fs {
			m[f.Role] = f
		}
		return m
	}
	a, b := byRole(r.Files), byRole(o.Files)
	for _, role := range []string{RecipeRoleCkpt, RecipeRoleClip, RecipeRoleVAE, RecipeRoleLoRA} {
		key := "imagegen_" + role
		fa, fb := a[role], b[role]
		if fa.Name != fb.Name {
			d = append(d, fmt.Sprintf("%s: %s vs %s", key, shown(fa.Name), shown(fb.Name)))
		} else if fa.Bytes != fb.Bytes {
			d = append(d, fmt.Sprintf("%s: %s is %s vs %s bytes", key, shown(fa.Name), sizeShown(fa.Bytes), sizeShown(fb.Bytes)))
		}
	}
	str("imagegen_preset", r.Preset, o.Preset)
	num("imagegen_steps", float64(r.Steps), float64(o.Steps))
	num("imagegen_cfg", r.CFG, o.CFG)
	str("imagegen_sampler", r.Sampler, o.Sampler)
	str("imagegen_scheduler", r.Scheduler, o.Scheduler)
	str("imagegen_schedule", r.Schedule, o.Schedule)
	num("imagegen_shift", r.Shift, o.Shift)
	num("imagegen_lora_strength", r.LoRAStrength, o.LoRAStrength)
	str("license", r.License, o.License)
	switch {
	case (r.CommercialUse == nil) != (o.CommercialUse == nil):
		d = append(d, "commercial_use: declared on one side only")
	case r.CommercialUse != nil && *r.CommercialUse != *o.CommercialUse:
		d = append(d, fmt.Sprintf("commercial_use: %v vs %v", *r.CommercialUse, *o.CommercialUse))
	}
	return d
}

func shown(s string) string {
	if s == "" {
		return "unset"
	}
	return s
}

func sizeShown(n int64) string {
	if n < 0 {
		return "missing"
	}
	return strconv.FormatInt(n, 10)
}
