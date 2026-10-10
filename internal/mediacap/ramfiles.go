package mediacap

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
)

// The weight files a route loads, for the one reader that needs their SIZES: the host-RAM
// estimate of a lease (internal/hostneed). doctor asks "is the file there"; the guard asks "how
// much host memory will these files cost", and both must name the same files, so this reads the
// tables above (expectedClasses, builderCompanions, the video/animate/music defaults) instead of
// keeping a second copy of them.

// ModelRole says what a weight file is to the host-RAM estimate.
type ModelRole string

const (
	// RoleDiffusion is the denoiser: a checkpoint, a UNet, a transformer, an expert.
	RoleDiffusion ModelRole = "diffusion"
	// RoleTextEncoder is the text (or text+vision) encoder the graph loads beside it.
	RoleTextEncoder ModelRole = "text-encoder"
	// RoleOther is everything small: VAEs, LoRAs, upscalers, vision towers. Not counted.
	RoleOther ModelRole = "other"
)

// ModelFile is one weight file a route's graph loads.
type ModelFile struct {
	// Label says where the name came from (a config key, or "<builder> default").
	Label string
	// Name is the ComfyUI-relative filename.
	Name string
	// Classes are the class directories the loader opens it from.
	Classes []string
	Role    ModelRole
}

func roleOf(classes []string) ModelRole {
	switch {
	case intersects(classes, classDiffusion):
		return RoleDiffusion
	case intersects(classes, classTextEnc):
		return RoleTextEncoder
	}
	return RoleOther
}

func fromNeeds(files []needFile) []ModelFile {
	out := make([]ModelFile, 0, len(files))
	for _, f := range files {
		out = append(out, ModelFile{Label: f.label, Name: f.name, Classes: f.classes, Role: roleOf(f.classes)})
	}
	return out
}

// ImageModelFiles is what the image route's graph loads for this (effective) binding: the
// checkpoint or UNet the binding names and the text encoder, bound or the builder's default for a
// family that has one (qwen-image, krea2). A binding that names nothing contributes nothing: the
// caller falls back to a per-family default size.
func ImageModelFiles(cfg config.Config) []ModelFile {
	var out []ModelFile
	if n := strings.TrimSpace(cfg.ImageGenCkpt); !builtinName(n) {
		out = append(out, ModelFile{Label: "imagegen_ckpt", Name: n, Classes: expectedClasses["imagegen_ckpt"], Role: RoleDiffusion})
	}
	clip, label := strings.TrimSpace(cfg.ImageGenCLIP), "imagegen_clip"
	if builtinName(clip) {
		clip = ""
		if c, ok := builderCompanions[cfg.ImageGenFamily]; ok {
			clip, label = c.clip, "imagegen_clip ("+cfg.ImageGenFamily+" default)"
		}
	}
	if clip != "" {
		out = append(out, ModelFile{Label: label, Name: clip, Classes: expectedClasses["imagegen_clip"], Role: RoleTextEncoder})
	}
	return out
}

// EditModelFiles is ImageModelFiles for the generative edit route: the UNet and the text encoder
// (the 2511 graph has a builder default for it; the 2.1 graph has none and requires the binding).
func EditModelFiles(cfg config.Config) []ModelFile {
	var out []ModelFile
	if n := strings.TrimSpace(cfg.GenEditUnet); !builtinName(n) {
		out = append(out, ModelFile{Label: "gen_edit_unet", Name: n, Classes: expectedClasses["gen_edit_unet"], Role: RoleDiffusion})
	}
	clip, label := strings.TrimSpace(cfg.GenEditCLIP), "gen_edit_clip"
	if builtinName(clip) {
		clip = ""
		if cfg.GenEditFamily != config.FamilyQwenImage21 {
			clip, label = builderCompanions[config.EditFamily2511].clip, "gen_edit_clip (2511 default)"
		}
	}
	if clip != "" {
		out = append(out, ModelFile{Label: label, Name: clip, Classes: expectedClasses["gen_edit_clip"], Role: RoleTextEncoder})
	}
	return out
}

// InpaintModelFiles is the SDXL inpaint checkpoint (inpaint_ckpt).
func InpaintModelFiles(cfg config.Config) []ModelFile {
	if n := strings.TrimSpace(cfg.InpaintCkpt); !builtinName(n) {
		return []ModelFile{{Label: "inpaint_ckpt", Name: n, Classes: expectedClasses["inpaint_ckpt"], Role: RoleDiffusion}}
	}
	return nil
}

// VideoModelFiles is what the video route loads for the named runner family ("" = this box's
// default family), by the same binding the render resolves (config.ResolveVideoFamilyBinding):
// wan22, ltx25, h3 and hunyuan; anything else is Wan 2.2, as the runner treats it.
func VideoModelFiles(cfg config.Config, family string) []ModelFile {
	family = strings.TrimSpace(family)
	if family == "" {
		family = strings.TrimSpace(cfg.VideoGenFamily)
	}
	fam := videoRunnerFamily(family)
	render := ""
	if family != "" {
		render = fam
	}
	return fromNeeds(videoFamilyFilesLabeled(fam, cfg.ResolveVideoFamilyBinding(render), "videogen_"))
}

// AnimateModelFiles is WAN-Animate-2's four files.
func AnimateModelFiles(cfg config.Config) []ModelFile { return fromNeeds(animateNeeds(cfg)) }

// MusicModelFiles is ACE-Step 1.5's files (no config key binds them).
func MusicModelFiles() []ModelFile { return fromNeeds(musicNeeds()) }

// ResolveModelFile finds f under the class directories of roots and returns its size in bytes. The
// name is tried as the ComfyUI-relative path the loader would open (subfolders included); it never
// walks a tree looking for the basename, because the host-RAM estimate runs at every grant and a
// walk of a 70 GiB model tree is not free. stat is os.Stat unless a test supplies a table.
func ResolveModelFile(roots []ModelRoot, f ModelFile, stat func(path string) (size int64, ok bool)) (int64, bool) {
	if stat == nil {
		stat = osStatSize
	}
	rel := filepath.FromSlash(f.Name)
	for _, r := range roots {
		for _, cd := range classDirs(r) {
			if !contains(f.Classes, cd[0]) {
				continue
			}
			if n, ok := stat(filepath.Join(cd[1], rel)); ok {
				return n, true
			}
		}
	}
	return 0, false
}

func osStatSize(path string) (int64, bool) {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return 0, false
	}
	return fi.Size(), true
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
