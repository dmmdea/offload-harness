package mediacap

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
)

// Named families (ADR 0058) get their own route verdicts, and every model file a
// binding makes the graph load is checked — including the ones no config key names.
//
// A family is a whole binding: its script, its engine, and its model files. A family
// whose checkpoint is not on the box is not "configured" just because the render
// script is, so a family route is CONFIGURED only when its script resolves AND every
// ComfyUI model name it binds (or its graph implies) sits in the class directory the
// loader opens. That is stricter than the default image route, whose model names are
// doctor's separate `comfyui model bindings` section — a family is opt-in, and an
// operator adding one wants the whole answer on the one line.

// ImageFamilyRoute / EditFamilyRoute name a family's route in Routes/Map.
func ImageFamilyRoute(name string) string { return "generate_image:" + name }
func EditFamilyRoute(name string) string  { return "edit_image_generative:" + name }

// Builder defaults and preset LoRAs the render graphs load WITHOUT a config key naming
// them. They mirror render/wf-qwen-image.mjs, render/wf-krea2.mjs and
// render/wf-qwen-image-edit.mjs; TestImpliedFilesMirrorTheRenderBuilders parses those
// files and fails when the two drift.
var (
	// imagePresetLoRAs: QWEN_IMAGE_PRESETS (qwen-image 2512), preset -> LoRA ("" = none).
	imagePresetLoRAs = map[string]string{
		"full":       "",
		"lightning4": "Qwen-Image-2512-Lightning-4steps-V1.0-fp32.safetensors",
	}
	// editPresetLoRAs: QWEN_EDIT_PRESETS (Qwen-Image-Edit 2511).
	editPresetLoRAs = map[string]string{
		"full":       "",
		"lightning8": "Qwen-Image-Edit-2511-Lightning-8steps-V1.0-bf16.safetensors",
		"lightning4": "Qwen-Image-Edit-2511-Lightning-4steps-V1.0-bf16.safetensors",
	}
	// builderCompanions: the text encoder and VAE each graph loads when the binding
	// leaves them unset.
	builderCompanions = map[string]struct{ clip, vae string }{
		"qwen-image":          {"qwen_2.5_vl_7b_fp8_scaled.safetensors", "qwen_image_vae.safetensors"},
		"krea2":               {"qwen3vl_4b_bf16.safetensors", "qwen_image_vae.safetensors"},
		config.EditFamily2511: {"qwen_2.5_vl_7b_fp8_scaled.safetensors", "qwen_image_vae.safetensors"},
	}
)

// defaultImagePreset / defaultEditPreset are the runners' own fallbacks when no preset
// is bound (comfy-render.mjs: "full"; comfy-edit.mjs: "lightning8").
const (
	defaultImagePreset = "full"
	defaultEditPreset  = "lightning8"
)

// impliedImageFiles lists the (label, class-key, name) of every file an image binding's
// graph loads without a key naming it. class-key picks the expected class directories.
func impliedImageFiles(cfg config.Config) [][3]string {
	var out [][3]string
	if c, ok := builderCompanions[cfg.ImageGenFamily]; ok {
		if cfg.ImageGenCLIP == "" {
			out = append(out, [3]string{"imagegen_clip (" + cfg.ImageGenFamily + " default)", "imagegen_clip", c.clip})
		}
		if cfg.ImageGenVAE == "" {
			out = append(out, [3]string{"imagegen_vae (" + cfg.ImageGenFamily + " default)", "imagegen_vae", c.vae})
		}
	}
	if cfg.ImageGenFamily == "qwen-image" && cfg.ImageGenLoRA == "" {
		preset := cfg.ImageGenPreset
		if preset == "" {
			preset = defaultImagePreset
		}
		if lora := imagePresetLoRAs[preset]; lora != "" {
			out = append(out, [3]string{"imagegen_lora (preset " + preset + ")", "imagegen_lora", lora})
		}
	}
	return out
}

// impliedEditFiles is impliedImageFiles for the edit route. The 2.1 edit graph has no
// defaults and no presets, so it implies nothing.
func impliedEditFiles(cfg config.Config) [][3]string {
	if cfg.GenEditFamily == config.FamilyQwenImage21 {
		return nil
	}
	var out [][3]string
	c := builderCompanions[config.EditFamily2511]
	if cfg.GenEditCLIP == "" {
		out = append(out, [3]string{"gen_edit_clip (2511 default)", "gen_edit_clip", c.clip})
	}
	if cfg.GenEditVAE == "" {
		out = append(out, [3]string{"gen_edit_vae (2511 default)", "gen_edit_vae", c.vae})
	}
	if cfg.GenEditLoRA == "" {
		preset := cfg.GenEditPreset
		if preset == "" {
			preset = defaultEditPreset
		}
		if lora := editPresetLoRAs[preset]; lora != "" {
			out = append(out, [3]string{"gen_edit_lora (preset " + preset + ")", "gen_edit_lora", lora})
		}
	}
	return out
}

// imageFamilyModelKeys / editFamilyModelKeys are the model-file keys a family binds.
var (
	imageFamilyModelKeys = []string{"imagegen_ckpt", "imagegen_vae", "imagegen_clip", "imagegen_lora"}
	editFamilyModelKeys  = []string{"gen_edit_unet", "gen_edit_vae", "gen_edit_clip", "gen_edit_lora"}
)

func imageKeyValue(cfg config.Config, key string) string {
	switch key {
	case "imagegen_ckpt":
		return cfg.ImageGenCkpt
	case "imagegen_vae":
		return cfg.ImageGenVAE
	case "imagegen_clip":
		return cfg.ImageGenCLIP
	case "imagegen_lora":
		return cfg.ImageGenLoRA
	case "gen_edit_unet":
		return cfg.GenEditUnet
	case "gen_edit_vae":
		return cfg.GenEditVAE
	case "gen_edit_clip":
		return cfg.GenEditCLIP
	case "gen_edit_lora":
		return cfg.GenEditLoRA
	}
	return ""
}

// familyModelBindings resolves one (effective) binding's model files — the bound keys
// and the implied files — labelling each with prefix (a family's config path).
func familyModelBindings(roots []ModelRoot, cfg config.Config, prefix string, keys []string, implied [][3]string) []Binding {
	var out []Binding
	for _, key := range keys {
		v := imageKeyValue(cfg, key)
		if builtinName(v) {
			continue
		}
		out = append(out, resolveBinding(roots, prefix+key, v, expectedClasses[key]))
	}
	for _, f := range implied {
		out = append(out, resolveBinding(roots, prefix+f[0], f[2], expectedClasses[f[1]]))
	}
	return out
}

// requiredFamilyKeys names the keys a graph cannot run without (the 2.1 builders have
// no default for any of their three files).
func requiredImageKeys(cfg config.Config) []string {
	if cfg.ImageGenFamily == config.FamilyQwenImage21 {
		return []string{"imagegen_ckpt", "imagegen_clip", "imagegen_vae"}
	}
	return []string{"imagegen_ckpt"}
}

func requiredEditKeys(cfg config.Config) []string {
	if cfg.GenEditFamily == config.FamilyQwenImage21 {
		return []string{"gen_edit_unet", "gen_edit_clip", "gen_edit_vae"}
	}
	return []string{"gen_edit_unet"}
}

// familyRoutes derives one route per named family: script + engine binding like the
// default route, then (ComfyUI) every model file resolved under the family's own
// comfy_dir, or (sdcpp) every bound file stat'd. Reports whether any family drives
// ComfyUI / node, so the prereq rows stay honest.
func familyRoutes(cfg config.Config, exeDir string) (out []Route, comfyUsed, nodeUsed bool) {
	names := make([]string, 0, len(cfg.ImageGenFamilies))
	for n := range cfg.ImageGenFamilies {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		fcfg, fi, err := cfg.ResolveImageFamily(name)
		rn := ImageFamilyRoute(name)
		if err != nil {
			out = append(out, Route{Name: rn, Engine: "comfyui", State: BoundButMissing, Detail: err.Error()})
			continue
		}
		var r Route
		if fcfg.ImageGenEngine == "sdcpp" {
			if fcfg.SdcppBin == "" || fcfg.SdcppModel == "" {
				out = append(out, Route{Name: rn, Engine: "sdcpp", State: BoundButMissing,
					Detail: fmt.Sprintf("imagegen_families[%q] selects sdcpp but binds no sdcpp_bin/sdcpp_model", name)})
				continue
			}
			nodeUsed = true
			script := fcfg.SdcppScript
			if script == "" {
				script = "render/sdcpp-generate.mjs"
			}
			bs := []binding{
				{key: "sdcpp_bin", value: fcfg.SdcppBin, kind: binaryBinding},
				{key: "sdcpp_model", value: fcfg.SdcppModel, kind: fileBinding},
				{key: "sdcpp_script", value: script, kind: scriptBinding},
			}
			for _, c := range []struct{ k, v string }{{"sdcpp_vae", fcfg.SdcppVAE}, {"sdcpp_clip_l", fcfg.SdcppClipL},
				{"sdcpp_clip_g", fcfg.SdcppClipG}, {"sdcpp_t5xxl", fcfg.SdcppT5}, {"sdcpp_llm", fcfg.SdcppLLM}} {
				if c.v != "" {
					bs = append(bs, binding{key: c.k, value: c.v, kind: fileBinding})
				}
			}
			r = fileRoute(rn, "sdcpp", exeDir, bs...)
		} else {
			if fcfg.ImageGenScript == "" {
				out = append(out, Route{Name: rn, Engine: "comfyui", State: NotConfigured,
					Detail: fmt.Sprintf("imagegen_families[%q]: imagegen_script is unset", name)})
				continue
			}
			comfyUsed, nodeUsed = true, true
			r = fileRoute(rn, "comfyui", exeDir, binding{key: "imagegen_script", value: fcfg.ImageGenScript, kind: scriptBinding})
			if r.State == Configured {
				r = withModelFiles(r, fcfg, fmt.Sprintf("imagegen_families[%q].", name),
					requiredImageKeys(fcfg), imageFamilyModelKeys, impliedImageFiles(fcfg))
			}
		}
		r.Detail = licenseDetail(fi) + r.Detail
		out = append(out, r)
	}

	enames := make([]string, 0, len(cfg.GenEditFamilies))
	for n := range cfg.GenEditFamilies {
		enames = append(enames, n)
	}
	sort.Strings(enames)
	for _, name := range enames {
		fcfg, fi, err := cfg.ResolveEditFamily(name)
		rn := EditFamilyRoute(name)
		if err != nil {
			out = append(out, Route{Name: rn, Engine: "comfyui", State: BoundButMissing, Detail: err.Error()})
			continue
		}
		if fcfg.GenEditScript == "" {
			out = append(out, Route{Name: rn, Engine: "comfyui", State: NotConfigured,
				Detail: fmt.Sprintf("gen_edit_families[%q]: gen_edit_script is unset", name)})
			continue
		}
		comfyUsed, nodeUsed = true, true
		r := fileRoute(rn, "comfyui", exeDir, binding{key: "gen_edit_script", value: fcfg.GenEditScript, kind: scriptBinding})
		if r.State == Configured {
			r = withModelFiles(r, fcfg, fmt.Sprintf("gen_edit_families[%q].", name),
				requiredEditKeys(fcfg), editFamilyModelKeys, impliedEditFiles(fcfg))
		}
		r.Detail = licenseDetail(fi) + r.Detail
		out = append(out, r)
	}
	return out, comfyUsed, nodeUsed
}

// withModelFiles upgrades a script-CONFIGURED family route with its model files: a
// required key left unset, or any bound/implied file MISSING or MISPLACED, turns it
// BOUND-BUT-MISSING. With no ComfyUI models root the names cannot be checked, and
// the detail says so rather than claiming a verdict (comfy_dir's own prereq row names
// the cause).
func withModelFiles(r Route, fcfg config.Config, prefix string, required, keys []string, implied [][3]string) Route {
	for _, k := range required {
		if imageKeyValue(fcfg, k) == "" {
			r.State = BoundButMissing
			r.Detail += fmt.Sprintf("; %s%s is required by this graph and unset", prefix, k)
			return r
		}
	}
	roots := ModelRoots(fcfg.ComfyDir)
	if len(roots) == 0 {
		r.Detail += "; model names not checked (no ComfyUI models root under comfy_dir)"
		return r
	}
	var bad, good []string
	for _, b := range familyModelBindings(roots, fcfg, prefix, keys, implied) {
		if b.State != BindingFound {
			bad = append(bad, b.Detail)
		} else {
			good = append(good, b.Name)
		}
	}
	if len(bad) > 0 {
		r.State = BoundButMissing
		r.Detail += "; " + strings.Join(bad, "; ")
		return r
	}
	r.Detail += "; models " + strings.Join(good, ", ")
	return r
}

func licenseDetail(fi config.FamilyInfo) string {
	switch {
	case fi.NonCommercial():
		return fmt.Sprintf("NON-COMMERCIAL (%s); ", fi.License)
	case fi.License != "":
		return fmt.Sprintf("license %s; ", fi.License)
	}
	return ""
}

// FamilyRow is one binding a request's `family` param can select, as offload_status
// (media.image_families / media.edit_families) and /fleet/health publish it. The
// default binding comes first. License and CommercialUse are null when the binding
// declares none (a default binding that never set imagegen_license): a reader must
// treat that as UNKNOWN, never as commercial-safe. State is the binding's route
// verdict from the same derivation as media.routes.
type FamilyRow struct {
	Name          string  `json:"name"`
	Family        string  `json:"family"`
	Engine        string  `json:"engine"`
	Ckpt          string  `json:"ckpt"`
	License       *string `json:"license"`
	CommercialUse *bool   `json:"commercial_use"`
	Default       bool    `json:"default"`
	State         string  `json:"state"`
}

// ImageFamilyRows lists this box's image bindings with their verdicts (routes is
// Routes(cfg), passed in so one status call derives the filesystem view once).
func ImageFamilyRows(cfg config.Config, routes []Route) []FamilyRow {
	byRoute := map[string]Route{}
	for _, r := range routes {
		byRoute[r.Name] = r
	}
	var out []FamilyRow
	for _, fi := range cfg.ImageFamilies() {
		fcfg, _, err := cfg.ResolveImageFamily(fi.Name)
		if fi.Default {
			fcfg, err = cfg, nil
		}
		if err != nil {
			continue
		}
		rn := "generate_image"
		if !fi.Default {
			rn = ImageFamilyRoute(fi.Name)
		}
		ckpt := fcfg.ImageGenCkpt
		if fcfg.ImageGenEngine == "sdcpp" {
			ckpt = filepathBase(fcfg.SdcppModel)
		}
		out = append(out, familyRow(fi, ckpt, byRoute[rn]))
	}
	return out
}

// EditFamilyRows is ImageFamilyRows for the generative edit route.
func EditFamilyRows(cfg config.Config, routes []Route) []FamilyRow {
	byRoute := map[string]Route{}
	for _, r := range routes {
		byRoute[r.Name] = r
	}
	var out []FamilyRow
	for _, fi := range cfg.EditFamilies() {
		fcfg, _, err := cfg.ResolveEditFamily(fi.Name)
		if fi.Default {
			fcfg, err = cfg, nil
		}
		if err != nil {
			continue
		}
		rn := "edit_image_generative"
		if !fi.Default {
			rn = EditFamilyRoute(fi.Name)
		}
		out = append(out, familyRow(fi, fcfg.GenEditUnet, byRoute[rn]))
	}
	return out
}

func familyRow(fi config.FamilyInfo, ckpt string, r Route) FamilyRow {
	row := FamilyRow{Name: fi.Name, Family: fi.Family, Engine: fi.Engine, Ckpt: ckpt,
		CommercialUse: fi.CommercialUse, Default: fi.Default, State: string(r.State)}
	if fi.License != "" {
		lic := fi.License
		row.License = &lic
	}
	if row.State == "" {
		row.State = string(NotConfigured)
	}
	return row
}

// filepathBase is filepath.Base that keeps "" as "".
func filepathBase(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Base(p)
}
