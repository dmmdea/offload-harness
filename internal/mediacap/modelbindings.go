package mediacap

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dmmdea/offload-harness/internal/config"
)

// Model-tree binding check (register F-31). Every ComfyUI-backed route names
// its weights as a FILENAME relative to a class directory (checkpoints/,
// diffusion_models/, vae/, …) under a models root — <comfy_dir>/models and every
// base_path in <comfy_dir>/extra_model_paths.yaml; the graph builder decides the
// loader node and therefore the class the file must live in. Until now nothing
// verified that binding before a render: three casualties after the 2026-08-31
// disk swap (an H3 int8 DiT, an H3 turbo LoRA, every krea2 file) each surfaced
// only as a graph rejection at render time. This check resolves every
// configured model name against the class directories the render scripts load
// it from, so a missing or misplaced file is a doctor finding instead of a
// failed render.

// BindingState is a model binding's verdict.
type BindingState string

const (
	// BindingFound — the file exists in a class directory the graph loads from.
	BindingFound BindingState = "FOUND"
	// BindingMissing — the file exists in no class directory of any models root.
	BindingMissing BindingState = "MISSING"
	// BindingMisplaced — the file exists under a models root, but only in a
	// class directory the graph never loads from (the 2026-08-31 krea2 shape).
	BindingMisplaced BindingState = "MISPLACED"
)

// Binding is one configured model file and where it was (or was not) found.
type Binding struct {
	Key      string       // config key ("imagegen_ckpt")
	Name     string       // configured value (ComfyUI-relative name)
	Expected []string     // class directories the render scripts load it from
	FoundIn  []string     // class directories that hold it (any root)
	State    BindingState // FOUND / MISSING / MISPLACED
	Detail   string
}

// expectedClasses maps a config key to the ComfyUI class directories the
// shipped graph builders load it from (render/wf-*.mjs, verified 2026-09-18):
//
//	CheckpointLoaderSimple -> checkpoints; UNETLoader / UnetLoaderGGUF /
//	UNETLoaderDisTorch2MultiGPU -> diffusion_models (legacy alias unet);
//	VAELoader -> vae; CLIPLoader / DualCLIPLoader -> text_encoders (legacy
//	alias clip); CLIPVisionLoader -> clip_vision; LoraLoaderModelOnly -> loras;
//	UpscaleModelLoader -> upscale_models; LatentUpscaleModelLoader (LTX) ->
//	latent_upscale_models with upscale_models accepted.
//
// imagegen_ckpt is family-dependent (CheckpointLoaderSimple for sdxl /
// hidream-o1, UNETLoader for krea2 / qwen-image), so both classes are accepted.
var expectedClasses = map[string][]string{
	"imagegen_ckpt":            {"checkpoints", "diffusion_models", "unet"},
	"imagegen_refiner_model":   {"checkpoints"},
	"imagegen_vae":             {"vae"},
	"imagegen_clip":            {"text_encoders", "clip"},
	"imagegen_lora":            {"loras"},
	"inpaint_ckpt":             {"checkpoints", "diffusion_models", "unet"},
	"inpaint_vae":              {"vae"},
	"gen_edit_unet":            {"diffusion_models", "unet"},
	"gen_edit_clip":            {"text_encoders", "clip"},
	"gen_edit_vae":             {"vae"},
	"gen_edit_lora":            {"loras"},
	"upscale_model":            {"upscale_models"},
	"videogen_unet_high":       {"diffusion_models", "unet"},
	"videogen_unet_low":        {"diffusion_models", "unet"},
	"videogen_transformer":     {"diffusion_models", "unet"},
	"videogen_text_encoder":    {"text_encoders", "clip"},
	"videogen_video_vae":       {"vae"},
	"videogen_audio_vae":       {"vae"},
	"videogen_latent_upscaler": {"latent_upscale_models", "upscale_models"},
	"videogen_upscale_model":   {"upscale_models"},
	"animategen_unet":          {"diffusion_models", "unet"},
	"animategen_text_encoder":  {"text_encoders", "clip"},
	"animategen_clip_vision":   {"clip_vision"},
	"animategen_vae":           {"vae"},
}

// bindingKeys is the report order: image, inpaint, edit, upscale, video, animate.
var bindingKeys = []string{
	"imagegen_ckpt", "imagegen_refiner_model", "imagegen_vae", "imagegen_clip", "imagegen_lora",
	"inpaint_ckpt", "inpaint_vae",
	"gen_edit_unet", "gen_edit_clip", "gen_edit_vae", "gen_edit_lora",
	"upscale_model",
	"videogen_unet_high", "videogen_unet_low", "videogen_transformer", "videogen_text_encoder",
	"videogen_video_vae", "videogen_audio_vae", "videogen_latent_upscaler", "videogen_upscale_model",
	"animategen_unet", "animategen_text_encoder", "animategen_clip_vision", "animategen_vae",
}

func bindingValues(cfg config.Config) map[string]string {
	return map[string]string{
		"imagegen_ckpt":            cfg.ImageGenCkpt,
		"imagegen_refiner_model":   cfg.ImageGenRefinerModel,
		"imagegen_vae":             cfg.ImageGenVAE,
		"imagegen_clip":            cfg.ImageGenCLIP,
		"imagegen_lora":            cfg.ImageGenLoRA,
		"inpaint_ckpt":             cfg.InpaintCkpt,
		"inpaint_vae":              cfg.InpaintVAE,
		"gen_edit_unet":            cfg.GenEditUnet,
		"gen_edit_clip":            cfg.GenEditCLIP,
		"gen_edit_vae":             cfg.GenEditVAE,
		"gen_edit_lora":            cfg.GenEditLoRA,
		"upscale_model":            cfg.UpscaleModel,
		"videogen_unet_high":       cfg.VideoGenUnetHigh,
		"videogen_unet_low":        cfg.VideoGenUnetLow,
		"videogen_transformer":     cfg.VideoGenTransformer,
		"videogen_text_encoder":    cfg.VideoGenTextEncoder,
		"videogen_video_vae":       cfg.VideoGenVideoVAE,
		"videogen_audio_vae":       cfg.VideoGenAudioVAE,
		"videogen_latent_upscaler": cfg.VideoGenLatentUpscaler,
		"videogen_upscale_model":   cfg.VideoGenUpscaleModel,
		"animategen_unet":          cfg.AnimateGenUnet,
		"animategen_text_encoder":  cfg.AnimateGenTextEncoder,
		"animategen_clip_vision":   cfg.AnimateGenClipVision,
		"animategen_vae":           cfg.AnimateGenVAE,
	}
}

// builtinName reports a value that names no file: "" and "builtin" (the
// checkpoint's own VAE) and "none".
func builtinName(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "" || v == "builtin" || v == "none"
}

// ModelRoot is one place ComfyUI resolves class directories: the install's own
// models/ (every subdirectory is a class) or one extra_model_paths.yaml entry
// (base_path plus an explicit class -> subdirectories map, exactly as ComfyUI's
// folder_paths reads it).
type ModelRoot struct {
	Label   string
	Dir     string
	Classes map[string][]string // nil = every subdirectory of Dir is a class
}

// ModelRoots lists the models roots ComfyUI would search for comfyDir: its
// models/ directory when present, then every entry of extra_model_paths.yaml
// beside it (a relative base_path resolves against comfyDir). Nil when neither
// exists — a box without ComfyUI has nothing to bind.
func ModelRoots(comfyDir string) []ModelRoot {
	if comfyDir == "" {
		return nil
	}
	var roots []ModelRoot
	if dir := filepath.Join(comfyDir, "models"); isDir(dir) {
		roots = append(roots, ModelRoot{Label: "models", Dir: dir})
	}
	roots = append(roots, extraModelRoots(filepath.Join(comfyDir, "extra_model_paths.yaml"), comfyDir)...)
	return roots
}

// extraModelRoots parses ComfyUI's extra_model_paths.yaml: top-level entries
// name a root, `base_path` its directory, every other key a class whose value
// is one subdirectory or a newline-separated list of them. Keys that are not
// class maps (is_default, comments) are ignored; a malformed file yields no
// roots rather than a crash — doctor must stay side-effect free and honest.
func extraModelRoots(path, comfyDir string) []ModelRoot {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc map[string]map[string]any
	if yaml.Unmarshal(raw, &doc) != nil {
		return nil
	}
	names := make([]string, 0, len(doc))
	for n := range doc {
		names = append(names, n)
	}
	sort.Strings(names)
	var roots []ModelRoot
	for _, n := range names {
		entry := doc[n]
		base, _ := entry["base_path"].(string)
		base = strings.TrimSpace(base)
		if base == "" {
			continue
		}
		if !filepath.IsAbs(base) {
			base = filepath.Join(comfyDir, base)
		}
		if !isDir(base) {
			continue
		}
		classes := map[string][]string{}
		for k, v := range entry {
			if k == "base_path" || k == "is_default" {
				continue
			}
			s, ok := v.(string)
			if !ok {
				continue
			}
			for _, line := range strings.Split(s, "\n") {
				if line = strings.TrimSpace(line); line != "" {
					classes[k] = append(classes[k], line)
				}
			}
		}
		roots = append(roots, ModelRoot{Label: n, Dir: filepath.Clean(base), Classes: classes})
	}
	return roots
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// ModelBindings resolves every configured ComfyUI model name against the class
// directories of every models root. Nil when the box has no models root.
func ModelBindings(cfg config.Config) []Binding {
	roots := ModelRoots(cfg.ComfyDir)
	if len(roots) == 0 {
		return nil
	}
	vals := bindingValues(cfg)
	var out []Binding
	for _, key := range bindingKeys {
		name := vals[key]
		if builtinName(name) {
			continue
		}
		out = append(out, resolveBinding(roots, key, name, expectedClasses[key]))
	}
	// Files the default bindings' graphs load WITHOUT a key naming them: a builder's
	// default text encoder/VAE and a preset's distillation LoRA. These were invisible —
	// the Qube's 2511 edit route is bound to preset lightning8 with gen_edit_lora unset,
	// and the Lightning LoRA that preset loads was absent from every models root while
	// doctor stayed green. Only routes that are actually bound are checked.
	if cfg.ImageGenScript != "" && cfg.ImageGenEngine != "sdcpp" {
		out = append(out, familyModelBindings(roots, cfg, "", nil, impliedImageFiles(cfg))...)
	}
	if cfg.GenEditScript != "" && cfg.GenEditUnet != "" {
		out = append(out, familyModelBindings(roots, cfg, "", nil, impliedEditFiles(cfg))...)
	}
	// Every named family's files, labelled with the family's config path, resolved
	// under the family's own comfy_dir (an overlay may point at a side-by-side install).
	for _, fi := range cfg.ImageFamilies() {
		if fi.Default {
			continue
		}
		fcfg, _, err := cfg.ResolveImageFamily(fi.Name)
		if err != nil || fcfg.ImageGenEngine == "sdcpp" {
			continue // sd.cpp binds full paths, stat'd by the family's route verdict
		}
		froots := ModelRoots(fcfg.ComfyDir)
		if len(froots) == 0 {
			continue
		}
		out = append(out, familyModelBindings(froots, fcfg, fmt.Sprintf("imagegen_families[%q].", fi.Name),
			imageFamilyModelKeys, impliedImageFiles(fcfg))...)
	}
	for _, fi := range cfg.EditFamilies() {
		if fi.Default {
			continue
		}
		fcfg, _, err := cfg.ResolveEditFamily(fi.Name)
		if err != nil {
			continue
		}
		froots := ModelRoots(fcfg.ComfyDir)
		if len(froots) == 0 {
			continue
		}
		out = append(out, familyModelBindings(froots, fcfg, fmt.Sprintf("gen_edit_families[%q].", fi.Name),
			editFamilyModelKeys, impliedEditFiles(fcfg))...)
	}
	return out
}

// classDirs enumerates (class, directory) pairs for a root.
func classDirs(r ModelRoot) [][2]string {
	var out [][2]string
	if r.Classes == nil {
		entries, _ := os.ReadDir(r.Dir)
		for _, e := range entries {
			if e.IsDir() {
				out = append(out, [2]string{e.Name(), filepath.Join(r.Dir, e.Name())})
			}
		}
		return out
	}
	names := make([]string, 0, len(r.Classes))
	for c := range r.Classes {
		names = append(names, c)
	}
	sort.Strings(names)
	for _, c := range names {
		for _, sub := range r.Classes[c] {
			out = append(out, [2]string{c, filepath.Join(r.Dir, filepath.FromSlash(sub))})
		}
	}
	return out
}

// resolveBinding looks for name under every class directory of every root:
// first as the ComfyUI-relative path the loader would open (subfolders
// included), then by basename anywhere under that class (a file that moved one
// level is still reported in its class, never as MISSING).
func resolveBinding(roots []ModelRoot, key, name string, expected []string) Binding {
	b := Binding{Key: key, Name: name, Expected: expected}
	rel := filepath.FromSlash(name)
	base := filepath.Base(rel)
	seen := map[string]bool{}
	var searched []string
	for _, r := range roots {
		searched = append(searched, r.Dir)
		for _, cd := range classDirs(r) {
			class, dir := cd[0], cd[1]
			if seen[class] || !isDir(dir) {
				continue
			}
			if fi, err := os.Stat(filepath.Join(dir, rel)); err == nil && !fi.IsDir() {
				seen[class] = true
				continue
			}
			if hasBasename(dir, base) {
				seen[class] = true
			}
		}
	}
	for c := range seen {
		b.FoundIn = append(b.FoundIn, c)
	}
	sort.Strings(b.FoundIn)
	switch {
	case len(b.FoundIn) == 0:
		b.State = BindingMissing
		b.Detail = key + "=" + name + " is under no class directory of " + strings.Join(searched, ", ") + " (expected " + strings.Join(expected, "|") + ")"
	case intersects(b.FoundIn, expected):
		b.State = BindingFound
		b.Detail = key + "=" + name + " in " + strings.Join(b.FoundIn, ",")
	default:
		b.State = BindingMisplaced
		b.Detail = key + "=" + name + " found only in " + strings.Join(b.FoundIn, ",") + " — the graph loads it from " + strings.Join(expected, "|")
	}
	return b
}

func hasBasename(root, base string) bool {
	found := false
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		if !d.IsDir() && d.Name() == base {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func intersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}
