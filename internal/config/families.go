package config

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Named, license-tagged media families (ADR 0058).
//
// A node has ONE default image binding (imagegen_*) and ONE default edit binding
// (gen_edit_*). imagegen_families / gen_edit_families add NAMED bindings beside them:
// a request selects one with its `family` param, and a request without one keeps
// rendering exactly what it rendered before. That is how a non-commercial model
// (Qwen-Image-2.1, Qwen Research License) ships at all: never a default, never a seed
// default, always an explicit per-request opt-in whose every result carries its
// license.
//
// An overlay is a JSON object of this Config's own keys plus two meta keys:
//
//	"license":        string, required, non-empty — the model's license name
//	"commercial_use": bool,   required — false tags every result research-only
//
// Resolution builds the family's EFFECTIVE config: this node's config, with every
// MODEL-BINDING key of that route cleared (so a family never inherits the default's
// checkpoint, LoRA, preset or pool), and then the overlay's keys applied. The route
// keys (script, engine, timeout, reserve) and the machine's launch keys (comfy_*)
// stay inherited unless the overlay sets them.

// FamilyOverlay is one named binding's keys, verbatim from the config file. Keys are
// validated at load (validateFamilies); values are decoded into the matching Config
// field's own type, so a wrong type fails the load by name.
type FamilyOverlay map[string]json.RawMessage

// FamilyQwenImage21 is the Qwen-Image-2.1 graph family (render/wf-qwen-image-21.mjs):
// the only family with an RGBA VAE, so the only one transparent output applies to.
const FamilyQwenImage21 = "qwen-image-2.1"

// EditFamily2511 names the default edit graph (Qwen-Image-Edit 2511), which an empty
// gen_edit_family selects.
const EditFamily2511 = "qwen-image-edit-2511"

// FamilyInfo is what a request resolved to: which binding answered and under which
// license. CommercialUse nil = the binding declares no license (the default binding
// on a box that never set imagegen_license).
type FamilyInfo struct {
	// Name is the request-facing name: an overlay's key, or the default binding's
	// family ("" when the default binding names none — the generic SDXL graph).
	Name string
	// Family is the graph family the binding renders with (imagegen_family /
	// gen_edit_family after the overlay).
	Family        string
	Engine        string
	License       string
	CommercialUse *bool
	Default       bool
}

// NonCommercial reports a binding that declared commercial_use false.
func (f FamilyInfo) NonCommercial() bool { return f.CommercialUse != nil && !*f.CommercialUse }

// LicenseNote is the sentence every non-commercial result carries ("" otherwise).
func (f FamilyInfo) LicenseNote() string {
	if !f.NonCommercial() {
		return ""
	}
	return fmt.Sprintf("research/evaluation use only under %s; not for commercial work", f.License)
}

// overlayKind is the per-route contract for one families map.
type overlayKind struct {
	key       string   // the config key ("imagegen_families")
	prefixes  []string // key prefixes an overlay may set
	clear     []string // model-binding keys cleared before the overlay applies
	inherited []string // route keys kept from the node (documentation + coverage test)
	forbidden map[string]string
}

// imageBindingKeys are the image route's MODEL-BINDING keys: what a family replaces
// wholesale. Every imagegen_*/sdcpp_* key is in exactly one of clear / inherited /
// forbidden (TestEveryMediaKeyIsClassifiedForOverlays).
var imageOverlay = overlayKind{
	key:      "imagegen_families",
	prefixes: []string{"imagegen_", "sdcpp_", "comfy_"},
	clear: []string{
		"imagegen_ckpt", "imagegen_family", "imagegen_vae", "imagegen_steps", "imagegen_cfg",
		"imagegen_sampler", "imagegen_scheduler", "imagegen_preset", "imagegen_clip", "imagegen_lora",
		"imagegen_lora_strength", "imagegen_shift", "imagegen_pool_vvram_gb", "imagegen_pool_compute",
		"imagegen_pool_donor", "imagegen_schedule", "imagegen_license", "imagegen_commercial_use",
		"sdcpp_model", "sdcpp_model_kind", "sdcpp_vae", "sdcpp_clip_l", "sdcpp_clip_g", "sdcpp_t5xxl",
		"sdcpp_llm", "sdcpp_extra_args",
	},
	inherited: []string{
		"imagegen_script", "imagegen_engine", "imagegen_timeout_sec", "imagegen_reserve_vram",
		"sdcpp_bin", "sdcpp_script",
	},
	forbidden: map[string]string{
		"imagegen_families":            "families do not nest",
		"imagegen_license":             "use the overlay's own \"license\" key",
		"imagegen_commercial_use":      "use the overlay's own \"commercial_use\" key",
		"imagegen_refiner_model":       "the prompt refiner is this node's text-tier preprocessing, shared by every image family",
		"imagegen_refiner_timeout_sec": "the prompt refiner is this node's text-tier preprocessing, shared by every image family",
	},
}

var editOverlay = overlayKind{
	key:      "gen_edit_families",
	prefixes: []string{"gen_edit_", "comfy_"},
	clear: []string{
		"gen_edit_unet", "gen_edit_family", "gen_edit_preset", "gen_edit_lora", "gen_edit_lora_strength",
		"gen_edit_clip", "gen_edit_vae", "gen_edit_steps", "gen_edit_cfg", "gen_edit_sampler",
		"gen_edit_scheduler", "gen_edit_megapixels", "gen_edit_resolution", "gen_edit_cache_device",
		"gen_edit_license", "gen_edit_commercial_use",
	},
	inherited: []string{"gen_edit_script", "gen_edit_timeout_sec"},
	forbidden: map[string]string{
		"gen_edit_families":       "families do not nest",
		"gen_edit_license":        "use the overlay's own \"license\" key",
		"gen_edit_commercial_use": "use the overlay's own \"commercial_use\" key",
	},
}

// familyNameRe keeps names safe in a request param, a CLI flag, a ledger row and a
// fleet advertisement.
var familyNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

var (
	fieldIdxOnce sync.Once
	fieldIdx     map[string]int  // json tag -> Config field index
	pathTags     map[string]bool // json tags of path-typed fields (tilde-expanded)
)

func configFieldIndex() (map[string]int, map[string]bool) {
	fieldIdxOnce.Do(func() {
		fieldIdx = map[string]int{}
		t := reflect.TypeOf(Config{})
		for i := 0; i < t.NumField(); i++ {
			name := strings.SplitN(t.Field(i).Tag.Get("json"), ",", 2)[0]
			if name != "" && name != "-" {
				fieldIdx[name] = i
			}
		}
		// Path-typed fields, by address against a zero Config: the same enumeration
		// the load path expands, so an overlay's sdcpp_* paths get "~/" like the rest.
		var z Config
		zv := reflect.ValueOf(&z).Elem()
		byAddr := map[*string]bool{}
		for _, p := range pathFields(&z) {
			byAddr[p] = true
		}
		pathTags = map[string]bool{}
		for tag, i := range fieldIdx {
			f := zv.Field(i)
			if f.Kind() == reflect.String && byAddr[f.Addr().Interface().(*string)] {
				pathTags[tag] = true
			}
		}
	})
	return fieldIdx, pathTags
}

// applyOverlay builds the effective config for one overlay. The copy is shallow for
// the fields it does not touch (maps/slices shared with base and read-only there);
// every field it sets is freshly decoded, never decoded INTO base's backing storage.
func applyOverlay(base Config, name string, ov FamilyOverlay, kind overlayKind) (Config, string, bool, error) {
	where := fmt.Sprintf("%s[%q]", kind.key, name)
	var license string
	if raw, ok := ov["license"]; !ok {
		return base, "", false, fmt.Errorf("%s: \"license\" is required (the model's license name, e.g. \"Qwen Research License\")", where)
	} else if err := json.Unmarshal(raw, &license); err != nil || strings.TrimSpace(license) == "" {
		return base, "", false, fmt.Errorf("%s: \"license\" must be a non-empty string", where)
	}
	var commercial bool
	if raw, ok := ov["commercial_use"]; !ok {
		return base, "", false, fmt.Errorf("%s: \"commercial_use\" is required (true or false — false tags every result research/evaluation-only)", where)
	} else if err := json.Unmarshal(raw, &commercial); err != nil {
		return base, "", false, fmt.Errorf("%s: \"commercial_use\" must be true or false", where)
	}
	idx, paths := configFieldIndex()
	keys := make([]string, 0, len(ov))
	for k := range ov {
		if k != "license" && k != "commercial_use" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if why, bad := kind.forbidden[k]; bad {
			return base, "", false, fmt.Errorf("%s: key %q is not allowed in a family overlay (%s)", where, k, why)
		}
		allowed := false
		for _, p := range kind.prefixes {
			if strings.HasPrefix(k, p) {
				allowed = true
				break
			}
		}
		if !allowed {
			return base, "", false, fmt.Errorf("%s: key %q is outside this overlay's keys (allowed: %s* plus \"license\", \"commercial_use\")",
				where, k, strings.Join(kind.prefixes, "*, "))
		}
		if _, known := idx[k]; !known {
			return base, "", false, fmt.Errorf("%s: unknown key %q (not a config key — typo?)", where, k)
		}
	}
	cp := base
	v := reflect.ValueOf(&cp).Elem()
	for _, k := range kind.clear {
		if i, ok := idx[k]; ok {
			v.Field(i).Set(reflect.Zero(v.Field(i).Type()))
		}
	}
	home, _ := os.UserHomeDir()
	for _, k := range keys {
		f := v.Field(idx[k])
		nv := reflect.New(f.Type())
		if err := json.Unmarshal(ov[k], nv.Interface()); err != nil {
			return base, "", false, fmt.Errorf("%s.%s: %v", where, k, err)
		}
		f.Set(nv.Elem())
		if paths[k] && home != "" {
			f.SetString(ExpandTilde(f.String(), home))
		}
	}
	return cp, license, commercial, nil
}

// cudaDeviceRe is ComfyUI --cuda-device's accepted shape for a pin: an index or a
// comma list of indices ("all" is ComfyUI's "no pin", which is what "" means here).
var cudaDeviceRe = regexp.MustCompile(`^\d+(,\d+)*$`)

// validateMediaEnums refuses the new media keys' out-of-range values by name, at the
// config door — never as a runner exit 2 on every render. where prefixes the key
// ("" at the top level, `imagegen_families["x"].` inside an overlay).
func validateMediaEnums(c Config, where string) error {
	switch strings.TrimSpace(c.ImageGenSchedule) {
	case "", "official", "comfy":
	default:
		return fmt.Errorf("%simagegen_schedule: unknown schedule %q (valid: \"\", \"official\", \"comfy\")", where, c.ImageGenSchedule)
	}
	switch c.ComfyDynamicVRAM {
	case "", "on", "off":
	default:
		return fmt.Errorf("%scomfy_dynamic_vram: %q is not on, off or \"\"", where, c.ComfyDynamicVRAM)
	}
	if d := strings.ReplaceAll(c.ComfyCudaDevice, " ", ""); d != "" && !cudaDeviceRe.MatchString(d) {
		return fmt.Errorf("%scomfy_cuda_device: %q is not a ComfyUI device index or comma list (e.g. \"1\" or \"1,2\"; ComfyUI order, not nvidia-smi's)", where, c.ComfyCudaDevice)
	}
	switch c.GenEditFamily {
	case "", EditFamily2511, FamilyQwenImage21:
	default:
		return fmt.Errorf("%sgen_edit_family: unknown edit family %q (valid: \"\", %q, %q)", where, c.GenEditFamily, EditFamily2511, FamilyQwenImage21)
	}
	switch c.GenEditCacheDevice {
	case "", "auto", "gpu", "cpu", "off":
	default:
		return fmt.Errorf("%sgen_edit_cache_device: %q is not auto, gpu, cpu or off", where, c.GenEditCacheDevice)
	}
	if r := c.GenEditResolution; r < 0 || r > 4096 || r%32 != 0 {
		return fmt.Errorf("%sgen_edit_resolution: %d must be 0 (the builder default, 1024) or a multiple of 32 up to 4096", where, r)
	}
	return nil
}

// validateFamilies is the load-time door for everything ADR 0058 adds: the new enum
// keys, the default bindings' license pairs, and every overlay (names, keys, license,
// and the overlay's own effective values).
func validateFamilies(c Config) error {
	if err := validateMediaEnums(c, ""); err != nil {
		return err
	}
	if err := licensePair("imagegen", c.ImageGenLicense, c.ImageGenCommercialUse); err != nil {
		return err
	}
	if err := licensePair("gen_edit", c.GenEditLicense, c.GenEditCommercialUse); err != nil {
		return err
	}
	for _, kind := range []struct {
		k        overlayKind
		fams     map[string]FamilyOverlay
		defaultN string
	}{
		{imageOverlay, c.ImageGenFamilies, c.ImageGenFamily},
		{editOverlay, c.GenEditFamilies, c.defaultEditName()},
	} {
		for _, name := range sortedFamilyNames(kind.fams) {
			where := fmt.Sprintf("%s[%q]", kind.k.key, name)
			if !familyNameRe.MatchString(name) {
				return fmt.Errorf("%s: a family name is lower-case letters, digits, '.', '_' or '-' (at most 64)", where)
			}
			if name == kind.defaultN {
				return fmt.Errorf("%s: collides with this node's default binding (%q) — a request naming it would reach the default, never this overlay; rename it", where, kind.defaultN)
			}
			cp, _, _, err := applyOverlay(c, name, kind.fams[name], kind.k)
			if err != nil {
				return err
			}
			if err := validateMediaEnums(cp, where+"."); err != nil {
				return err
			}
		}
	}
	return nil
}

func licensePair(prefix, license string, commercial *bool) error {
	switch {
	case strings.TrimSpace(license) != "" && commercial == nil:
		return fmt.Errorf("%s_license is set but %s_commercial_use is not — declare both (a license with no commercial verdict cannot tag a result)", prefix, prefix)
	case strings.TrimSpace(license) == "" && commercial != nil:
		return fmt.Errorf("%s_commercial_use is set but %s_license is not — declare both", prefix, prefix)
	}
	return nil
}

func sortedFamilyNames(m map[string]FamilyOverlay) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// defaultEditName is the default edit binding's request-facing name: its family, or
// the 2511 graph when gen_edit_family is unset.
func (c Config) defaultEditName() string {
	if c.GenEditFamily != "" {
		return c.GenEditFamily
	}
	return EditFamily2511
}

// ImageFamilies lists this node's image bindings: the default first, then every
// named family in name order. A family whose overlay no longer resolves (only
// possible for an in-process Config that never went through Load) is skipped.
func (c Config) ImageFamilies() []FamilyInfo {
	out := []FamilyInfo{c.defaultImageInfo()}
	for _, name := range sortedFamilyNames(c.ImageGenFamilies) {
		if _, fi, err := c.ResolveImageFamily(name); err == nil {
			out = append(out, fi)
		}
	}
	return out
}

// EditFamilies is ImageFamilies for the generative edit route.
func (c Config) EditFamilies() []FamilyInfo {
	out := []FamilyInfo{c.defaultEditInfo()}
	for _, name := range sortedFamilyNames(c.GenEditFamilies) {
		if _, fi, err := c.ResolveEditFamily(name); err == nil {
			out = append(out, fi)
		}
	}
	return out
}

func (c Config) defaultImageInfo() FamilyInfo {
	engine := "comfyui"
	if c.ImageGenEngine == "sdcpp" {
		engine = "sdcpp"
	}
	return FamilyInfo{Name: c.ImageGenFamily, Family: c.ImageGenFamily, Engine: engine,
		License: c.ImageGenLicense, CommercialUse: c.ImageGenCommercialUse, Default: true}
}

func (c Config) defaultEditInfo() FamilyInfo {
	return FamilyInfo{Name: c.defaultEditName(), Family: c.defaultEditName(), Engine: "comfyui",
		License: c.GenEditLicense, CommercialUse: c.GenEditCommercialUse, Default: true}
}

// ResolveImageFamily returns the effective config for a generate_image request's
// `family` param. "" or the default binding's own family name = the default binding
// (the config itself, unchanged). A named family = its overlay applied. Anything
// else is an error that lists what this node serves.
func (c Config) ResolveImageFamily(name string) (Config, FamilyInfo, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == c.ImageGenFamily {
		return c, c.defaultImageInfo(), nil
	}
	ov, ok := c.ImageGenFamilies[name]
	if !ok {
		return c, FamilyInfo{}, fmt.Errorf("unknown image family %q — this node serves %s", name, familyList(c.ImageFamilies()))
	}
	cp, lic, commercial, err := applyOverlay(c, name, ov, imageOverlay)
	if err != nil {
		return c, FamilyInfo{}, err
	}
	fi := cp.defaultImageInfo()
	fi.Name, fi.License, fi.CommercialUse, fi.Default = name, lic, &commercial, false
	return cp, fi, nil
}

// ResolveEditFamily is ResolveImageFamily for edit_image_generative.
func (c Config) ResolveEditFamily(name string) (Config, FamilyInfo, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == c.defaultEditName() {
		return c, c.defaultEditInfo(), nil
	}
	ov, ok := c.GenEditFamilies[name]
	if !ok {
		return c, FamilyInfo{}, fmt.Errorf("unknown edit family %q — this node serves %s", name, familyList(c.EditFamilies()))
	}
	cp, lic, commercial, err := applyOverlay(c, name, ov, editOverlay)
	if err != nil {
		return c, FamilyInfo{}, err
	}
	fi := cp.defaultEditInfo()
	fi.Name, fi.License, fi.CommercialUse, fi.Default = name, lic, &commercial, false
	return cp, fi, nil
}

// familyList renders a node's families for an error or a defer reason.
func familyList(fs []FamilyInfo) string {
	parts := make([]string, 0, len(fs))
	for _, f := range fs {
		label := f.Name
		if label == "" {
			label = "(unnamed)"
		}
		if f.Default {
			label += " (default)"
		}
		if f.NonCommercial() {
			label += " [non-commercial: " + f.License + "]"
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, ", ")
}

// SupportsTransparentImage reports whether this (effective) image binding can keep
// an alpha channel: only the qwen-image-2.1 model has an RGBA VAE — true on EITHER
// engine (the ComfyUI graph's SplitImageWithAlpha and the sdcpp runner's own
// alpha-flatten step both key off this same predicate to decide whether to keep or
// drop the channel). Engine-independent on purpose: sd.cpp's build of the model
// carries the identical RGBA VAE, so refusing transparency for the sdcpp engine was
// never a model limit, only a gap in the runner (D5, binxarn wave session
// 5d227d30 §3b/§3c).
func (c Config) SupportsTransparentImage() bool {
	return c.ImageGenFamily == FamilyQwenImage21
}

// SupportsTransparentEdit is SupportsTransparentImage for the edit route.
func (c Config) SupportsTransparentEdit() bool { return c.GenEditFamily == FamilyQwenImage21 }

// ImagePooled reports an image binding that loads through the DisTorch2 pool — the
// pool keys, not comfy_cuda_device, place it.
func (c Config) ImagePooled() bool { return c.ImageGenPoolVvramGB > 0 }

// VideoPooled reports a video binding that loads through the DisTorch2 pool — same
// rule as ImagePooled. An un-pooled video seat (videogen_pool_vvram_gb <= 0, the
// native/streaming shape) is a single-card route like image generation, so
// comfy_cuda_device DOES apply to it; a pooled one still must not be pinned (the
// blackwell-3x16 pool computes on ComfyUI's default device, MultiGPU #220).
func (c Config) VideoPooled() bool { return c.VideoGenPoolVvramGB > 0 }
