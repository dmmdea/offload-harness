// Package hostneed decides how much host RAM a GPU lease DECLARES (gpulease.Options.HostRAMGiB),
// the number the grant admits against committed memory (gpuprobe.HostRAMAdmits).
//
// THE PRINCIPLE (AGENTS.md): the cards do the inference; RAM is overflow only. A render whose weights
// all sit on the card needs no host RAM worth declaring. One whose weights do not fit the card
// streams them from RAM: ComfyUI's dynamic VRAM stages the whole file in host memory ("Model Krea2
// prepared for dynamic VRAM loading. 24449MB Staged" in the log of the incident's lane), next to the
// text encoder ("8463MB Staged"), so what the host pays is the FULL size of the files that do not fit,
// not the overflow. The incident lanes were exactly that: a 24.5 GiB bf16 UNet and an 8.3 GiB text
// encoder on a 16 GiB card, 32.7 GiB each, two at once.
//
// WHERE THE NUMBER COMES FROM, in order:
//
//  1. `gpu reserve --ram <GiB>`, the operator's word (0 is allowed: "needs no host RAM").
//  2. A recognised render helper call (render/comfy-generate.mjs, comfy-render, comfy-edit,
//     comfy-inpaint, comfy-video): the sizes of the UNet/checkpoint and text-encoder files the
//     family loads, resolved through the configured ComfyUI model paths (mediacap.ModelRoots), and
//     counted in full when together they do not fit the card (largest card of the lease, less the
//     runner's --reserve-vram); 0 when they do. A file whose size cannot be read takes the
//     documented per-family size in familySizes; a family with neither is unknown, and the call
//     takes the class default.
//  3. The media class default: the largest of the render families THIS BOX has bound (its own
//     imagegen_*, gen_edit_*, videogen_* bindings), so a 32 GiB node is not held to the numbers of a
//     128 GiB one; 0 when nothing is bound. It is never clamped to fit the box: a default that
//     cannot be admitted here is refused with the reason and the way out (--ram), because quietly
//     lowering it would defeat the guard.
//  4. Text and seat leases: 0.
//
// The pipeline's media admission (internal/pipeline) asks the same estimate per route from the
// binding it is about to render with.
package hostneed

import (
	"fmt"
	"math"
	"path"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// Source says where a declared need came from.
type Source string

const (
	// SourceExplicit: the operator's `--ram`.
	SourceExplicit Source = "explicit"
	// SourceEstimate: the sizes of the model files of a recognised render call.
	SourceEstimate Source = "estimate"
	// SourceFamilyDefault: a recognised render call whose file sizes could not be read, so the
	// documented per-family sizes stand in for at least one of them.
	SourceFamilyDefault Source = "family default"
	// SourceClassDefault: the media class default, the largest family this box binds.
	SourceClassDefault Source = "class default"
	// SourceNone: a text or seat lease, or a box that binds no render family: nothing declared.
	SourceNone Source = "none"
)

// Need is a declared host-RAM need and where it came from.
type Need struct {
	GiB    float64
	Source Source
	// Detail is one sentence for the line the CLI prints and the docs quote.
	Detail string
}

func (n Need) String() string {
	return fmt.Sprintf("%.1f GiB host RAM (%s: %s)", n.GiB, n.Source, n.Detail)
}

// Route is one of the render routes whose model files are known.
type Route string

const (
	RouteImage   Route = "image"
	RouteEdit    Route = "edit"
	RouteInpaint Route = "inpaint"
	RouteVideo   Route = "video"
	RouteAnimate Route = "animate"
	RouteMusic   Route = "music"
)

// Facts are the readings an estimate needs besides the binding.
type Facts struct {
	// Cfg is the machine's config: ComfyDir (the model roots) and the video/animate bindings.
	Cfg config.Config
	// VRAMGiB is the total VRAM of the card the job runs on: the LARGEST single card of the leased
	// set (a job runs on one card, so the smaller cards of a multi-card lease do not constrain it).
	// 0 = unknown, and then nothing is assumed to fit.
	VRAMGiB float64
	// ReserveVRAMGiB is VRAM the runner holds back for the display (its --reserve-vram); 0 = the
	// cfg's imagegen_reserve_vram, else the runners' own 1.0.
	ReserveVRAMGiB float64
	// Stat reports a file's size; nil = os.Stat. A table in tests, because a 24 GiB fixture is not an option.
	Stat func(path string) (size int64, ok bool)
	// Env reads an environment variable (COMFY_CKPT and friends); nil = none set.
	Env func(string) string
}

func (f Facts) env(k string) string {
	if f.Env == nil {
		return ""
	}
	return strings.TrimSpace(f.Env(k))
}

func (f Facts) reserve() float64 {
	switch {
	case f.ReserveVRAMGiB > 0:
		return f.ReserveVRAMGiB
	case f.Cfg.ImageGenReserveVRAM > 0:
		return f.Cfg.ImageGenReserveVRAM
	}
	return 1.0
}

// LargestCardGiB is the total VRAM of the largest card among ids (lease ids; none = every card).
func LargestCardGiB(cards []gpuprobe.Card, ids []string) float64 {
	var best float64
	for _, c := range cards {
		if len(ids) > 0 && !containsFold(ids, c.LeaseID()) {
			continue
		}
		best = math.Max(best, c.VRAMTotalGiB)
	}
	return best
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// familySizes are the documented sizes (GiB) a family's weights take when the file named cannot be
// found or read, per role. They are the files the reference box binds, read off its model tree on
// 2026-10-09 and rounded up to one decimal, so an estimate that falls back errs high; the two
// Qwen-Image 2512 sizes are the bf16 UNet the incident streamed (~38 GiB) and the fp8-scaled Qwen2.5-VL
// encoder. A role a family does not list is not loaded by it; a family absent from the table is
// unknown (no estimate, so the class default stands in).
var familySizes = map[string]map[mediacap.ModelRole][]float64{
	"sdxl":                 {mediacap.RoleDiffusion: {6.5}},
	"krea2":                {mediacap.RoleDiffusion: {24.5}, mediacap.RoleTextEncoder: {8.3}},
	"qwen-image":           {mediacap.RoleDiffusion: {38.0}, mediacap.RoleTextEncoder: {8.8}},
	"qwen-image-2.1":       {mediacap.RoleDiffusion: {13.3}, mediacap.RoleTextEncoder: {16.4}},
	"qwen-image-edit-2511": {mediacap.RoleDiffusion: {19.2}, mediacap.RoleTextEncoder: {8.8}},
	"qwen-inpaint":         {mediacap.RoleDiffusion: {19.2}, mediacap.RoleTextEncoder: {8.8}},
	"wan22":                {mediacap.RoleDiffusion: {13.4, 13.4}, mediacap.RoleTextEncoder: {6.3}},
	"ltx25":                {mediacap.RoleDiffusion: {20.1}, mediacap.RoleTextEncoder: {14.4}},
	"h3":                   {mediacap.RoleDiffusion: {19.6}, mediacap.RoleTextEncoder: {14.7}},
	"wan-animate2":         {mediacap.RoleDiffusion: {15.6}, mediacap.RoleTextEncoder: {6.3}},
}

const gib = 1 << 30

// slot is one weight file of an estimate.
type slot struct {
	role  mediacap.ModelRole
	label string
	name  string
	gib   float64
	from  string // "file" (size read), "default" (the documented per-family size), "" (unknown)
}

// estimate is the rule: the files the family loads, sized, and counted in full when together they
// do not fit the card. files are what the binding names (mediacap's tables); family picks the
// documented fallbacks and the roles the family is known to load.
func estimate(family string, files []mediacap.ModelFile, f Facts) (Need, bool) {
	roots := mediacap.ModelRoots(f.Cfg.ComfyDir)
	defaults := familySizes[family]
	var slots []slot
	have := map[mediacap.ModelRole]int{}
	for _, mf := range files {
		if mf.Role == mediacap.RoleOther {
			continue
		}
		s := slot{role: mf.Role, label: mf.Label, name: mf.Name}
		if n, ok := mediacap.ResolveModelFile(roots, mf, f.Stat); ok {
			s.gib, s.from = float64(n)/gib, "file"
		} else if d := defaults[mf.Role]; len(d) > have[mf.Role] {
			s.gib, s.from = d[have[mf.Role]], "default"
		}
		have[mf.Role]++
		slots = append(slots, s)
	}
	// A role the family loads that the binding did not name (an unbound UNet, a text encoder with no
	// builder default): its documented size stands in for it.
	for role, sizes := range defaults {
		for i := have[role]; i < len(sizes); i++ {
			slots = append(slots, slot{role: role, label: string(role), gib: sizes[i], from: "default"})
		}
	}
	if len(slots) == 0 {
		return Need{}, false
	}
	var total float64
	defaulted := false
	var parts []string
	for _, s := range slots {
		if s.from == "" {
			return Need{}, false // a file of unknown size: no estimate, the caller takes the class default
		}
		total += s.gib
		if s.from == "default" {
			defaulted = true
		}
		parts = append(parts, fmt.Sprintf("%s %.1f", roleWord(s.role), s.gib))
	}
	src := SourceEstimate
	if defaulted {
		src = SourceFamilyDefault
	}
	set := strings.Join(parts, " + ")
	usable := f.VRAMGiB - f.reserve()
	if f.VRAMGiB > 0 && total <= usable {
		return Need{GiB: 0, Source: src, Detail: fmt.Sprintf("%s: %s = %.1f GiB fits the %.1f GiB card (%.1f GiB usable), so no weights stream from RAM", family, set, total, f.VRAMGiB, usable)}, true
	}
	card := "an unknown card"
	if f.VRAMGiB > 0 {
		card = fmt.Sprintf("the %.1f GiB card (%.1f GiB usable)", f.VRAMGiB, usable)
	}
	return Need{GiB: total, Source: src, Detail: fmt.Sprintf("%s: %s = %.1f GiB does not fit %s, so the weights stream from RAM", family, set, total, card)}, true
}

func roleWord(r mediacap.ModelRole) string {
	if r == mediacap.RoleTextEncoder {
		return "text encoder"
	}
	return "unet"
}

// ForRoute is the estimate for one route of the pipeline, from the EFFECTIVE binding of the call
// (the config after any named-family overlay). ok=false means the binding gave nothing to size.
func ForRoute(route Route, cfg config.Config, f Facts) (Need, bool) {
	f.Cfg = withDir(f.Cfg, cfg)
	switch route {
	case RouteImage:
		fam := strings.TrimSpace(cfg.ImageGenFamily)
		if fam == "" {
			fam = "sdxl"
		}
		return estimate(fam, mediacap.ImageModelFiles(cfg), f)
	case RouteEdit:
		fam := strings.TrimSpace(cfg.GenEditFamily)
		if fam == "" {
			fam = config.EditFamily2511
		}
		return estimate(fam, mediacap.EditModelFiles(cfg), f)
	case RouteInpaint:
		return estimate("sdxl", mediacap.InpaintModelFiles(cfg), f)
	case RouteVideo:
		return ForVideo(cfg, "", f)
	case RouteAnimate:
		return estimate("wan-animate2", mediacap.AnimateModelFiles(cfg), f)
	case RouteMusic:
		return estimate("acestep", mediacap.MusicModelFiles(), f)
	}
	return Need{}, false
}

// ForVideo is the video route's estimate for the family that will render: renderFamily is the
// config-namespace family the pipeline resolved for the request ("" = this box's default family),
// and the files are the binding that family renders with (config.ResolveVideoFamilyBinding).
func ForVideo(cfg config.Config, renderFamily string, f Facts) (Need, bool) {
	f.Cfg = withDir(f.Cfg, cfg)
	fam := strings.TrimSpace(renderFamily)
	label := fam
	if label == "" {
		label = strings.TrimSpace(cfg.VideoGenFamily)
	}
	return estimate(videoFamily(label), mediacap.VideoModelFiles(cfg, fam), f)
}

// withDir keeps the facts' own ComfyDir when it has one, else the binding's.
func withDir(facts, binding config.Config) config.Config {
	if strings.TrimSpace(facts.ComfyDir) == "" {
		facts.ComfyDir = binding.ComfyDir
	}
	return facts
}

// videoFamily maps a videogen_family / --model word onto the family the runner builds (the runner
// renders anything it does not name as Wan 2.2).
func videoFamily(fam string) string {
	switch strings.TrimSpace(fam) {
	case "ltx25", "h3", "hunyuan", "ace":
		return strings.TrimSpace(fam)
	}
	return "wan22"
}

// ClassDefault is the media class default: the largest estimate over the render families this box
// binds (config), so it describes what THIS box can be asked to run. A route whose script is unset,
// or whose files cannot be sized, is not counted; a box that binds nothing declares 0.
func ClassDefault(f Facts) Need {
	cfg := f.Cfg
	if strings.TrimSpace(cfg.ComfyDir) == "" {
		return Need{Source: SourceNone, Detail: "no ComfyUI install is bound on this box, so there is no render family to size"}
	}
	var best Need
	bestLabel := ""
	consider := func(label string, n Need, ok bool) {
		if ok && n.GiB > best.GiB {
			best, bestLabel = n, label
		}
	}
	if cfg.ImageGenScript != "" && cfg.ImageGenEngine != "sdcpp" {
		n, ok := ForRoute(RouteImage, cfg, f)
		consider("image", n, ok)
		for _, fi := range cfg.ImageFamilies() {
			if fi.Default {
				continue
			}
			if fcfg, _, err := cfg.ResolveImageFamily(fi.Name); err == nil && fcfg.ImageGenEngine != "sdcpp" {
				n, ok := ForRoute(RouteImage, fcfg, f)
				consider("image family "+fi.Name, n, ok)
			}
		}
	}
	if cfg.GenEditScript != "" {
		n, ok := ForRoute(RouteEdit, cfg, f)
		consider("edit", n, ok)
		for _, fi := range cfg.EditFamilies() {
			if fi.Default {
				continue
			}
			if fcfg, _, err := cfg.ResolveEditFamily(fi.Name); err == nil {
				n, ok := ForRoute(RouteEdit, fcfg, f)
				consider("edit family "+fi.Name, n, ok)
			}
		}
	}
	if cfg.VideoGenScript != "" {
		n, ok := ForRoute(RouteVideo, cfg, f)
		consider("video", n, ok)
		// A named family renders with ITS binding (ResolveVideoFamilyBinding), and that is chosen
		// against the box's own default family, so the config is passed as it is and the family by name:
		// making the name the default family would hand every name the flat videogen_* keys.
		for name := range cfg.VideoGenFamilies {
			n, ok := ForVideo(cfg, name, f)
			consider("video family "+name, n, ok)
		}
	}
	if cfg.AnimateGenScript != "" && cfg.AnimateGenEngine != "sdcpp" {
		n, ok := ForRoute(RouteAnimate, cfg, f)
		consider("animate", n, ok)
	}
	if cfg.MusicGenScript != "" && cfg.MusicGenEngine != "sdcpp" {
		n, ok := ForRoute(RouteMusic, cfg, f)
		consider("music", n, ok)
	}
	if bestLabel == "" {
		return Need{Source: SourceNone, Detail: "this box binds no render family whose weights could be sized"}
	}
	return Need{GiB: best.GiB, Source: SourceClassDefault, Detail: fmt.Sprintf("the largest render family this box binds is %s: %s", bestLabel, best.Detail)}
}

// Request is what the CLI knows about a reservation when it resolves the need.
type Request struct {
	// Explicit is `gpu reserve --ram` when it was given (a pointer, because 0 is a value).
	Explicit *float64
	Class    gpulease.Class
	// Args is the wrapped command (nil for a detached reservation).
	Args []string
}

// Resolve is the order in the package comment: explicit, a recognised render call, the media class
// default, nothing.
func Resolve(r Request, f Facts) Need {
	if r.Explicit != nil {
		v := math.Max(*r.Explicit, 0)
		return Need{GiB: v, Source: SourceExplicit, Detail: "stated with --ram"}
	}
	if call, ok := ParseRenderCall(r.Args); ok {
		if n, ok := call.Estimate(f); ok {
			return n
		}
	}
	if r.Class == gpulease.ClassMedia {
		return ClassDefault(f)
	}
	return Need{Source: SourceNone, Detail: "a " + string(r.Class) + " lease declares no host RAM"}
}

// scriptBase is the lower-cased file name of a path written with either separator.
func scriptBase(p string) string {
	return strings.ToLower(path.Base(strings.ReplaceAll(p, `\`, "/")))
}
