package mediacap

// The per-lane family rows a fleet node publishes for video, animation and audio (/fleet/health media_recipes, ADR 0082
// Amendment 3). A row says which family the node's binding NAMES for a lane, the weight files it loads and how big each one is
// on the node (-1: not there), so a delegator that names the node can refuse before a POST when the weights are missing.
//
// A row is NOT an identity in this release: it carries no content fingerprint and no digest, so two nodes' rows never match
// each other, and a reader that wants to match must treat an empty digest as "cannot be matched". The image Recipe (recipe.go)
// is untouched.

import (
	"path"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
)

// The lanes of a row: the fleet task types they belong to.
const (
	LaneVideo   = "video-gen"
	LaneAnimate = "animate"
	LaneAudio   = "audio-gen"
)

// LaneRow is one row of /fleet/health media_recipes.
type LaneRow struct {
	// Lane is the fleet task type (LaneVideo, LaneAnimate, LaneAudio).
	Lane string `json:"lane"`
	// Name is the name a caller reaches the family by: a video row carries the value of the `model` parameter (wan22, ltx25,
	// h3, hunyuan, or an sd.cpp family's own name), an animation row the family the node advertises for it (wan-animate2), an
	// audio row the family of its music lane (acestep).
	Name    string `json:"name"`
	Default bool   `json:"default"`
	// Engine is comfyui or sdcpp.
	Engine string `json:"engine"`
	// Digest is empty in this release: a row built from sizes is never an identity.
	Digest string `json:"digest"`
	// Files are the weight files, in the order the family's graph loads them. Name is the ComfyUI-relative name (subfolders
	// included) for a ComfyUI row and the bare file name for an sd.cpp row; never an absolute path.
	Files         []RecipeFile `json:"files"`
	License       string       `json:"license,omitempty"`
	CommercialUse *bool        `json:"commercial_use,omitempty"`
}

// fileMissing is the bytes of a file the node does not hold.
const fileMissing int64 = -1

// The names of the animation and music rows: the families the node advertises for those lanes.
const (
	AnimateRowName = "wan-animate2"
	MusicRowName   = "acestep"
)

// LaneRows lists the rows of this machine's video, animation and audio bindings: video first (the default family, then the other
// ComfyUI families, then the sd.cpp ones), then animation, then music. A lane is listed when its binding NAMES a renderer this
// release can describe: a ComfyUI script on a machine that HAS a ComfyUI install (comfy_dir is a directory here), or an sd.cpp
// video family. The shipped default config binds the three ComfyUI scripts on every machine, so a script alone says nothing about
// this one, and a machine with no install publishes no ComfyUI row. Whether the node can serve a lane right now is the route
// state's business (media_routes), and a row of a binding whose weights are gone is still listed, with bytes -1, so that a
// refusal can name them. The audio.cpp engines, the sd.cpp animation lane and voice have no row: a reader takes that as unknown.
func LaneRows(cfg config.Config) []LaneRow {
	roots := ModelRoots(cfg.ComfyDir)
	comfy := isDir(cfg.ComfyDir)
	var rows []LaneRow
	rows = append(rows, videoLaneRows(cfg, roots, comfy)...)
	if comfy && strings.TrimSpace(cfg.AnimateGenScript) != "" && cfg.AnimateGenEngine != config.EngineSdcpp {
		rows = append(rows, LaneRow{Lane: LaneAnimate, Name: AnimateRowName, Default: true, Engine: EngineComfyUI, Files: laneFiles(roots, AnimateModelFiles(cfg))})
	}
	if comfy && strings.TrimSpace(cfg.MusicGenScript) != "" && cfg.MusicGenEngine != config.EngineAudiocpp {
		rows = append(rows, LaneRow{Lane: LaneAudio, Name: MusicRowName, Default: true, Engine: EngineComfyUI, Files: laneFiles(roots, MusicModelFiles())})
	}
	return rows
}

func videoLaneRows(cfg config.Config, roots []ModelRoot, comfy bool) []LaneRow {
	var rows []LaneRow
	if comfy && strings.TrimSpace(cfg.VideoGenScript) != "" {
		// The default ComfyUI family: none when the box's default video family is an sd.cpp one.
		comfyDefault := ""
		if _, sdcpp := cfg.DefaultVideoSdcppFamily(); !sdcpp {
			comfyDefault = videoRunnerFamily(strings.TrimSpace(cfg.VideoGenFamily))
		}
		for _, name := range videoFamilyNames {
			// An sd.cpp family named like a ComfyUI one is the sd.cpp row below, not a listing of files its renders never load.
			if cfg.SdcppVideoFamily(name) {
				continue
			}
			_, bound := cfg.VideoGenFamilies[name]
			fam, fb := VideoBinding(cfg, name)
			files := laneFiles(roots, VideoModelFilesFor(fam, fb))
			// The default family is always listed. Another one only when the node has bound it or holds every file its graph loads:
			// otherwise it is not a family this node renders.
			if name != comfyDefault && !bound && !allPresent(files) {
				continue
			}
			row := LaneRow{Lane: LaneVideo, Name: name, Default: name == comfyDefault, Engine: EngineComfyUI, Files: files, License: fb.License, CommercialUse: fb.CommercialUse}
			if row.Default {
				rows = append([]LaneRow{row}, rows...)
			} else {
				rows = append(rows, row)
			}
		}
	}
	defaultSdcpp, _ := cfg.DefaultVideoSdcppFamily()
	for _, name := range sdcppVideoFamilyNames(cfg) {
		fb := cfg.VideoGenFamilies[name]
		files := make([]RecipeFile, 0, 4)
		for _, f := range []struct{ role, path string }{
			{"model", fb.SdcppModel}, {"high_noise_model", fb.SdcppHighNoiseModel}, {"vae", fb.SdcppVAE}, {"t5xxl", fb.SdcppT5xxl},
		} {
			if strings.TrimSpace(f.path) == "" {
				continue
			}
			size := fileMissing
			if n, ok := osStatSize(f.path); ok {
				size = n
			}
			files = append(files, RecipeFile{Role: f.role, Name: path.Base(publishedName(f.path)), Bytes: size})
		}
		row := LaneRow{Lane: LaneVideo, Name: name, Default: name == defaultSdcpp, Engine: config.EngineSdcpp, Files: files, License: fb.License, CommercialUse: fb.CommercialUse}
		if row.Default {
			rows = append([]LaneRow{row}, rows...)
		} else {
			rows = append(rows, row)
		}
	}
	return rows
}

// laneFiles describes the weight files of a family on this node: the name the loader opens, and the size where it finds the file
// (-1 when it does not). No content is read.
func laneFiles(roots []ModelRoot, files []ModelFile) []RecipeFile {
	out := make([]RecipeFile, 0, len(files))
	for _, f := range files {
		size := fileMissing
		if _, n, ok := ResolveModelFilePath(roots, f, nil); ok {
			size = n
		}
		out = append(out, RecipeFile{Role: laneRole(f.Label), Name: publishedName(f.Name), Bytes: size})
	}
	return out
}

func allPresent(files []RecipeFile) bool {
	for _, f := range files {
		if f.Bytes == fileMissing {
			return false
		}
	}
	return len(files) > 0
}

// laneRole is a file's role in its row: its label without the "(<builder> builder default)" note and without the config key
// prefix the doctor labels carry.
func laneRole(label string) string {
	if i := strings.Index(label, " ("); i >= 0 {
		label = label[:i]
	}
	label = strings.TrimPrefix(label, "videogen_")
	return strings.TrimPrefix(label, "animategen_")
}

// publishedName is the name a row carries for a file. A relative name keeps its subfolders, as the loader opens it (forward
// slashes whatever the config spelled). An absolute path, a drive-lettered one or one that climbs out with `..` is cut to its file
// name: rows leave the machine on /fleet/health, and a path is the machine's own business.
func publishedName(name string) string {
	n := strings.ReplaceAll(strings.TrimSpace(name), `\`, "/")
	abs := strings.HasPrefix(n, "/")
	if len(n) >= 2 && n[1] == ':' {
		n, abs = n[2:], true
	}
	parent := false
	for _, el := range strings.Split(n, "/") {
		if el == ".." {
			parent = true
		}
	}
	if abs || parent {
		return path.Base(n)
	}
	return n
}
