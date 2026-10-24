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
// A node that takes another machine's image job should render the SAME image the caller's own lane would have: the
// same weights, the same sampling. Two nodes can name a family alike and bind different files under it (one node's
// qwen-image-2.1 is the bf16 DiT and another's is an int8 build), and a name says nothing about a file that was
// truncated in a copy. So a family is identified by its RECIPE: the weight files it loads, each with its NAME, BYTE
// SIZE and CONTENT FINGERPRINT, and the sampling it renders with, digested over RESOLVED values and compared strictly.
//
// WHAT THAT CAN AND CANNOT SEE. Name plus size separates builds that differ in size (bf16 against int8 against NVFP4)
// and catches a truncated copy. The fingerprint (fingerprint.go) adds the content: two files of one name and one size
// whose bytes differ (a same-named re-release with the same tensor layout, a full-length altered copy, one node's file
// updated in place and the other's not) no longer digest alike, and the node's own 412 check recomputes the same
// fingerprint from the disk. The fingerprint is a SAMPLE (64 evenly spaced MiB of the file), so it sees a wholesale
// difference in weights and not a change confined to the stretch between two sample blocks (about a sixty-third of the
// file); fingerprint.go says what it was chosen for and what it measured.
//
// A file whose fingerprint is not known yet (still being hashed, or unreadable) is never a match, on either side of the
// comparison: Unfingerprinted names such files, and a recipe that has any is not an identity. The digest of a recipe
// built before the fingerprint existed cannot collide with one built after it (RecipeVersion).
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
// 2: each present weight file carries its content fingerprint (F59).
const RecipeVersion = 2

// EngineComfyUI is the only engine a recipe is defined for: an sd.cpp binding renders other pixels for the same
// weights, so a ComfyUI recipe never matches it and a recipe is not built for it.
const EngineComfyUI = "comfyui"

// The wire words of a node's 412. A node refuses an image dispatch whose recipe_digest is not the digest of the family it names
// with 412, and the JSON body of that refusal carries, beside "status" and "error", the member RecipeRefusalCauseKey with one of
// the causes below. The node writes it (fleetnode/image_recipe.go) and the delegator reads it (mediaremote/place.go), both from
// these constants, so a delegator can tell a node that has only not finished reading its weight files (a wait) from one whose
// files or binding are not what it published (a difference) without matching a sentence. A cause is a word of the wire: a test
// pins the values, because a node and a delegator of two builds must read them alike.
const (
	RecipeRefusalCauseKey = "cause"
	// RecipeRefusalPending: every weight file the node lacks a fingerprint for is still being read, so it cannot say yet whether
	// the family is what it was sent. It has made no finding that anything changed.
	RecipeRefusalPending = "pending"
	// RecipeRefusalUnreadable: a weight file is there and could not be read, so the node cannot vouch for the family.
	RecipeRefusalUnreadable = "unreadable"
	// RecipeRefusalChanged: the family the node holds no longer digests to the digest it was sent, or the node no longer serves it.
	RecipeRefusalChanged = "changed"
)

// The file roles of a recipe.
const (
	RecipeRoleCkpt = "ckpt"
	RecipeRoleClip = "clip"
	RecipeRoleVAE  = "vae"
	RecipeRoleLoRA = "lora"
	// RecipeRoleUpsampler is the prompt upsampler a flux3-klein graph runs before it renders (imagegen_upsampler): it writes the
	// caption the DiT renders, so another upsampler file is another picture.
	RecipeRoleUpsampler = "upsampler"
)

// RecipeFile is one weight file the graph loads: its role, its ComfyUI-relative name, its size on this machine (-1 when
// it is not found: a recipe with a missing file is never a match) and its content fingerprint (fingerprint.go): a
// sha256 in hex, FingerprintPending while this machine is still hashing it, FingerprintUnreadable when it could not be
// read, and absent for a file that is not there. Only a hash is an identity.
type RecipeFile struct {
	Role        string `json:"role"`
	Name        string `json:"name"`
	Bytes       int64  `json:"bytes"`
	Fingerprint string `json:"fingerprint,omitempty"`
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
	Steps        int     `json:"steps,omitempty"`
	CFG          float64 `json:"cfg,omitempty"`
	Sampler      string  `json:"sampler,omitempty"`
	Scheduler    string  `json:"scheduler,omitempty"`
	Schedule     string  `json:"schedule,omitempty"`
	Shift        float64 `json:"shift,omitempty"`
	LoRAStrength float64 `json:"lora_strength,omitempty"`
	// UpsamplerModel is a flux3-klein binding's llama-swap upsampler (imagegen_upsampler in its model form): another model writes another
	// caption, so it is part of what renders alike. By NAME only: the model's weights live in llama-swap, not in the ComfyUI roots.
	UpsamplerModel string `json:"upsampler_model,omitempty"`
	License        string `json:"license,omitempty"`
	CommercialUse  *bool  `json:"commercial_use,omitempty"`
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
// render/comfy-render.mjs throws a usage error for exactly these four.
var stepsAndCFGTogether = map[string]bool{"krea2": true, "qwen-image": true, config.FamilyQwenImage21: true, config.FamilyFlux3Klein: true}

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
		if f.Label == "imagegen_upsampler" {
			continue // its own role, below
		}
		role := RecipeRoleCkpt
		if f.Role == RoleTextEncoder {
			role = RecipeRoleClip
		}
		out = append(out, roledFile{role, f})
	}
	// Digested whenever a ComfyUI flux3-klein binding names an upsampler FILE (the model form is Recipe.UpsamplerModel).
	if u := cfg.UpsamplerFile(); u != "" {
		out = append(out, roledFile{RecipeRoleUpsampler, ModelFile{Label: "imagegen_upsampler", Name: u, Classes: expectedClasses["imagegen_upsampler"], Role: RoleTextEncoder}})
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
//
// ImageRecipe WAITS for each present file's content fingerprint (SharedFingerprints().Await). NO PRODUCTION CODE CALLS IT any
// more: the callers that need the answer read through ImageRecipeWith with the AwaitContext of their own request, so that
// the wait ends with the request (the delegator's own identity, mediaremote.localIdentity, and the node's 412 re-check,
// fleetnode.checkRecipe), and the node's health uses ImageRecipeWith with Peek, which never waits. ImageRecipe stays as the
// blocking spelling for tests and for a caller with no request to bound the wait by (the wait still ends at the
// fingerprinter's own bound).
func ImageRecipe(cfg config.Config, fi config.FamilyInfo, roots []ModelRoot, stat func(path string) (int64, bool)) (Recipe, bool) {
	return ImageRecipeWith(cfg, fi, roots, stat, SharedFingerprints().Await)
}

// ImageRecipeWith is ImageRecipe with the source of the content fingerprints chosen by the caller. A nil fp leaves
// every present file FingerprintPending, which is never a match: a caller that has no way to read content gets a recipe
// that cannot be mistaken for an identity.
func ImageRecipeWith(cfg config.Config, fi config.FamilyInfo, roots []ModelRoot, stat func(path string) (int64, bool), fp FingerprintFunc) (Recipe, bool) {
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
		UpsamplerModel: cfg.UpsamplerModel(),
		License:        strings.TrimSpace(fi.License), CommercialUse: fi.CommercialUse,
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
		path, size, found := ResolveModelFilePath(roots, rf.file, stat)
		file := RecipeFile{Role: rf.role, Name: rf.file.Name, Bytes: -1}
		if found {
			file.Bytes, file.Fingerprint = size, FingerprintPending
			if fp != nil {
				file.Fingerprint = fp(path, size)
			}
		}
		r.Files = append(r.Files, file)
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

// Unfingerprinted names, with the state each is in, the files that are there but have no content fingerprint yet
// ("name (pending)", "name (unreadable)"). A recipe that has any is not an identity: it is never a match, on either
// side of the comparison, and a node publishes no digest for it. Missing files are Missing's, not these.
func (r Recipe) Unfingerprinted() []string {
	var out []string
	for _, f := range r.Files {
		if f.Bytes >= 0 && !IsFingerprint(f.Fingerprint) {
			out = append(out, f.Name+" ("+fingerprintState(f.Fingerprint)+")")
		}
	}
	return out
}

// WithoutContent is the recipe with every file's fingerprint dropped: what it says by name, size and sampling alone. A
// delegator uses it to tell a node that holds this machine's build but has not finished hashing it (a named wait) from
// one that holds another build.
func (r Recipe) WithoutContent() Recipe {
	r.Files = append([]RecipeFile(nil), r.Files...)
	for i := range r.Files {
		r.Files[i].Fingerprint = ""
	}
	return r
}

func fingerprintState(fp string) string {
	switch {
	case fp == FingerprintPending || fp == FingerprintUnreadable:
		return fp
	case fp == "":
		return "not published"
	}
	return "invalid"
}

// digestForm is the canonical shape that is hashed: fixed field order, no omitted zero, files in role order.
type digestForm struct {
	V            int          `json:"v"`
	Engine       string       `json:"engine"`
	Graph        string       `json:"graph"`
	Files        []RecipeFile `json:"files"`
	Preset       string       `json:"preset"`
	Steps        int          `json:"steps"`
	CFG          float64      `json:"cfg"`
	Sampler      string       `json:"sampler"`
	Scheduler    string       `json:"scheduler"`
	Schedule     string       `json:"schedule"`
	Shift        float64      `json:"shift"`
	LoRAStrength float64      `json:"lora_strength"`
	// omitempty keeps every digest that has no model-form upsampler byte for byte what it was
	UpsamplerModel string `json:"upsampler_model,omitempty"`
	License        string `json:"license"`
	CommercialUse  *bool  `json:"commercial_use"`
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
		LoRAStrength: r.LoRAStrength, UpsamplerModel: r.UpsamplerModel, License: r.License, CommercialUse: r.CommercialUse})
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
	for _, role := range []string{RecipeRoleCkpt, RecipeRoleClip, RecipeRoleVAE, RecipeRoleLoRA, RecipeRoleUpsampler} {
		key := "imagegen_" + role
		fa, fb := a[role], b[role]
		if fa.Name != fb.Name {
			d = append(d, fmt.Sprintf("%s: %s vs %s", key, shown(fa.Name), shown(fb.Name)))
		} else if fa.Bytes != fb.Bytes {
			d = append(d, fmt.Sprintf("%s: %s is %s vs %s bytes", key, shown(fa.Name), sizeShown(fa.Bytes), sizeShown(fb.Bytes)))
		} else if fa.Fingerprint != fb.Fingerprint {
			d = append(d, fingerprintDiff(key, fa, fb))
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
	str("imagegen_upsampler", r.UpsamplerModel, o.UpsamplerModel)
	str("license", r.License, o.License)
	switch {
	case (r.CommercialUse == nil) != (o.CommercialUse == nil):
		d = append(d, "commercial_use: declared on one side only")
	case r.CommercialUse != nil && *r.CommercialUse != *o.CommercialUse:
		d = append(d, fmt.Sprintf("commercial_use: %v vs %v", *r.CommercialUse, *o.CommercialUse))
	}
	return d
}

// fingerprintDiff words a pair of files of one name and one size whose fingerprints differ: other content, or content
// that cannot be compared because one side has no hash yet.
func fingerprintDiff(key string, a, b RecipeFile) string {
	if IsFingerprint(a.Fingerprint) && IsFingerprint(b.Fingerprint) {
		return fmt.Sprintf("%s: %s has the same size on both and different content (fingerprint %s vs %s)", key, shown(a.Name), a.Fingerprint[:12], b.Fingerprint[:12])
	}
	return fmt.Sprintf("%s: %s has the same size on both and its content is not comparable (fingerprint %s vs %s)", key, shown(a.Name), fingerprintState(a.Fingerprint), fingerprintState(b.Fingerprint))
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
