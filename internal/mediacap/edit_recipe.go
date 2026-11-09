package mediacap

// The recipe of the generative-edit lane (Qwen-Image-Edit through render/comfy-edit.mjs), the edit twin of ImageRecipeWith.
//
// It is the node's CAPABILITY for the lane, not one render's settings: the edit graph the runner builds, the UNet, the text
// encoder, the VAE and the SET of LoRA files the node holds for the lane, each file with its size and the content fingerprint
// every image recipe carries (fingerprint.go). Everything a single edit chooses on its own (preset, steps, cfg, sampler,
// megapixels, placement, seed, negative prompt) travels in the request and is NOT in the digest, so two nodes that hold the same
// weights match whatever a request later asks of them.
//
// The digest is Recipe.Digest with Lane "edit". Lane enters the digest only when set, so an image recipe digests byte for byte
// as it did before the field existed, and an edit recipe cannot collide with an image recipe of the same graph name.
//
// The LoRA set: the LoRA the binding itself names (gen_edit_lora, else the one its preset pairs with) is part of the lane, so it
// is listed even when the file is missing (bytes -1 keeps the recipe from ever matching); the other preset LoRAs the 2511 graph
// can be asked for per request are listed only when the node holds them. A node that holds more of them is another capability, so
// it digests differently, and the asker sees the difference by name.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
)

// RecipeLaneEdit is Recipe.Lane of an edit recipe.
const RecipeLaneEdit = "edit"

// EditMaxImages is the most images one generative edit takes, the target included (the 2.1 graph's official limit; the pipeline
// holds the same number).
const EditMaxImages = 10

// editRecipeFile is a weight file of the edit lane. optional marks a file that is part of the recipe only when the node holds it.
type editRecipeFile struct {
	roledFile
	optional bool
}

// editRecipeFiles are the weight files the edit lane's graph can load on cfg's (effective) edit binding, in the order they are
// listed: UNet, text encoder, VAE, then the LoRAs by name. The 2.1 graph has no builder defaults and no presets, so it lists
// exactly what the binding names.
func editRecipeFiles(cfg config.Config) []editRecipeFile {
	var out []editRecipeFile
	is21 := cfg.GenEditFamily == config.FamilyQwenImage21
	for _, f := range EditModelFiles(cfg) {
		role := RecipeRoleCkpt
		if f.Role == RoleTextEncoder {
			role = RecipeRoleClip
		}
		out = append(out, editRecipeFile{roledFile: roledFile{role, f}})
	}
	vae, label := strings.TrimSpace(cfg.GenEditVAE), "gen_edit_vae"
	if builtinName(vae) {
		vae = ""
		if !is21 {
			vae, label = builderCompanions[config.EditFamily2511].vae, "gen_edit_vae (2511 default)"
		}
	}
	if vae != "" {
		out = append(out, editRecipeFile{roledFile: roledFile{RecipeRoleVAE, ModelFile{Label: label, Name: vae, Classes: expectedClasses["gen_edit_vae"], Role: RoleOther}}})
	}
	if is21 {
		return out
	}
	// The LoRAs of the 2511 graph: the binding's own, then the other presets' (a request may ask for any preset).
	own, ownLabel := strings.TrimSpace(cfg.GenEditLoRA), "gen_edit_lora"
	if builtinName(own) {
		own = ""
		preset := strings.TrimSpace(cfg.GenEditPreset)
		if preset == "" {
			preset = defaultEditPreset
		}
		own, ownLabel = editPresetLoRAs[preset], "gen_edit_lora (preset "+preset+")"
	}
	var loras []editRecipeFile
	seen := map[string]bool{}
	add := func(name, label string, optional bool) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		loras = append(loras, editRecipeFile{roledFile{RecipeRoleLoRA, ModelFile{Label: label, Name: name, Classes: expectedClasses["gen_edit_lora"], Role: RoleOther}}, optional})
	}
	add(own, ownLabel, false)
	for preset, name := range editPresetLoRAs {
		add(name, "gen_edit_lora (preset "+preset+")", true)
	}
	sort.SliceStable(loras, func(i, j int) bool { return loras[i].file.Name < loras[j].file.Name })
	return append(out, loras...)
}

// EditRecipeWith is the recipe of an EFFECTIVE edit binding (the config a request's family resolved to, with fi its FamilyInfo),
// with the source of the content fingerprints chosen by the caller, as ImageRecipeWith. ok is false for a binding that is not a
// ComfyUI one. roots and stat are the caller's, as there.
func EditRecipeWith(cfg config.Config, fi config.FamilyInfo, roots []ModelRoot, stat func(path string) (int64, bool), fp FingerprintFunc) (Recipe, bool) {
	if fi.Engine != "" && fi.Engine != EngineComfyUI {
		return Recipe{}, false
	}
	graph := strings.TrimSpace(cfg.GenEditFamily)
	if graph == "" {
		graph = config.EditFamily2511
	}
	r := Recipe{Engine: EngineComfyUI, Graph: graph, Lane: RecipeLaneEdit}
	for _, ef := range editRecipeFiles(cfg) {
		path, size, found := ResolveModelFilePath(roots, ef.file, stat)
		if !found && ef.optional {
			continue
		}
		file := RecipeFile{Role: ef.role, Name: ef.file.Name, Bytes: -1}
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

// diffEdit is Diff for a pair of edit recipes, in the edit binding's own key names (gen_edit_unet, gen_edit_clip, gen_edit_vae and
// gen_edit_lora, the LoRA files as a set by name).
func (r Recipe) diffEdit(o Recipe) []string {
	var d []string
	if r.Lane != o.Lane {
		d = append(d, fmt.Sprintf("lane: %s vs %s", shown(r.Lane), shown(o.Lane)))
	}
	if r.Engine != o.Engine {
		d = append(d, fmt.Sprintf("engine: %s vs %s", shown(r.Engine), shown(o.Engine)))
	}
	if r.Graph != o.Graph {
		d = append(d, fmt.Sprintf("gen_edit_family: %s vs %s", shown(r.Graph), shown(o.Graph)))
	}
	keys := map[string]string{RecipeRoleCkpt: "gen_edit_unet", RecipeRoleClip: "gen_edit_clip", RecipeRoleVAE: "gen_edit_vae"}
	byRole := func(fs []RecipeFile, role string) RecipeFile {
		for _, f := range fs {
			if f.Role == role {
				return f
			}
		}
		return RecipeFile{}
	}
	one := func(key string, fa, fb RecipeFile) {
		if fa.Name != fb.Name {
			d = append(d, fmt.Sprintf("%s: %s vs %s", key, shown(fa.Name), shown(fb.Name)))
		} else if fa.Bytes != fb.Bytes {
			d = append(d, fmt.Sprintf("%s: %s is %s vs %s bytes", key, shown(fa.Name), sizeShown(fa.Bytes), sizeShown(fb.Bytes)))
		} else if fa.Fingerprint != fb.Fingerprint {
			d = append(d, fingerprintDiff(key, fa, fb))
		}
	}
	for _, role := range []string{RecipeRoleCkpt, RecipeRoleClip, RecipeRoleVAE} {
		one(keys[role], byRole(r.Files, role), byRole(o.Files, role))
	}
	loras := func(fs []RecipeFile) map[string]RecipeFile {
		m := map[string]RecipeFile{}
		for _, f := range fs {
			if f.Role == RecipeRoleLoRA {
				m[f.Name] = f
			}
		}
		return m
	}
	a, b := loras(r.Files), loras(o.Files)
	names := map[string]bool{}
	for n := range a {
		names[n] = true
	}
	for n := range b {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, n := range sorted {
		fa, inA := a[n]
		fb, inB := b[n]
		switch {
		case !inA, !inB:
			d = append(d, fmt.Sprintf("gen_edit_lora: %s is held on one side only", n))
		default:
			one("gen_edit_lora", fa, fb)
		}
	}
	return d
}
