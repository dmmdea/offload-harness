// Package mediacap DERIVES this machine's media capability from its config,
// using the same bindings the pipeline routes on — so no surface has to DECLARE
// what the box can do.
//
// A declared capability map is not merely stale, it is worse than none: it was
// wrong the moment a box changed engines. `offload_status` hardcoded
// "image_engine": "ComfyUI (local)" and handed that to an autonomous planner on a
// node whose `imagegen_engine` is "sdcpp" and which has no ComfyUI at all; doctor
// checked model aliases only, so it reported green while `generate_image` deferred
// on a render script that was not on disk. Both surfaces now read from here.
//
// Three verdicts, and the middle one is the whole point:
//
//   - CONFIGURED — bound, and every file it names exists.
//   - NOT CONFIGURED — no binding on this box. The task defers by design; that is
//     a legitimate state, not a fault.
//   - BOUND-BUT-MISSING — the config names a file that is not there. The task
//     defers at call time with a runner error, and until now nothing reported it.
//
// Existence is checked the way the runners resolve it: a relative script path
// against the EXECUTABLE's directory (gpugen.ResolveScript's rule — an MCP host
// spawns the server with no meaningful cwd), an executable binding by stat when it
// is a path and by PATH lookup when it is a bare name ("node", "ffmpeg").
package mediacap

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpugen"
	"github.com/dmmdea/offload-harness/internal/mediaops"
)

// State is a route's derived verdict.
type State string

const (
	Configured      State = "CONFIGURED"
	NotConfigured   State = "NOT CONFIGURED"
	BoundButMissing State = "BOUND-BUT-MISSING"
)

// Route is one file-backed media capability on THIS machine. Model-alias routes
// (vision/stt) are deliberately absent: their reachability is a live /v1/models
// question, answered by doctor's alias diff and offload_status's roster probe.
type Route struct {
	// Name is the offload task this route serves ("generate_image",
	// "generate_audio:music", ...) — the vocabulary a caller already has.
	Name string
	// Engine is what actually runs it on this box ("sdcpp", "comfyui",
	// "chatterbox-tts", "acestep", "pil", "gimp", "ffmpeg", "runtime").
	Engine string
	State  State
	// Detail names the bindings behind the verdict: what resolved, or exactly
	// which path is absent / which config key is empty.
	Detail string
	// Prereq marks a shared dependency (the node runtime, the ComfyUI install)
	// rather than a task route. A missing prereq breaks the CONFIGURED routes
	// above it, which is precisely why it is reported next to them.
	Prereq bool
}

// OK reports whether this route can run right now.
func (r Route) OK() bool { return r.State == Configured }

// Routes derives every file-backed media route for cfg against the real
// filesystem, resolving relative script paths against the running executable's
// directory exactly like the pipeline does.
func Routes(cfg config.Config) []Route {
	exeDir := ""
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	return routesIn(cfg, exeDir)
}

// routesIn is Routes with an injectable executable dir (unit-testable).
func routesIn(cfg config.Config, exeDir string) []Route {
	var out []Route
	comfyUsed := false // any bound route that drives a ComfyUI install
	nodeUsed := false  // any bound route that shells out to a render script

	// --- generate_image: the engine seam decides which bindings even apply ---
	if cfg.ImageGenEngine == "sdcpp" {
		switch {
		case cfg.SdcppBin == "" || cfg.SdcppModel == "":
			out = append(out, Route{Name: "generate_image", Engine: "sdcpp", State: NotConfigured,
				Detail: "imagegen_engine is sdcpp but sdcpp_bin/sdcpp_model is unset"})
		default:
			nodeUsed = true
			script := cfg.SdcppScript
			if script == "" {
				script = "render/sdcpp-generate.mjs" // the pipeline's own fallback
			}
			out = append(out, fileRoute("generate_image", "sdcpp", exeDir,
				binding{key: "sdcpp_bin", value: cfg.SdcppBin, kind: binaryBinding},
				binding{key: "sdcpp_model", value: cfg.SdcppModel, kind: fileBinding},
				binding{key: "sdcpp_script", value: script, kind: scriptBinding},
			))
		}
	} else if cfg.ImageGenScript == "" {
		out = append(out, Route{Name: "generate_image", Engine: "comfyui", State: NotConfigured,
			Detail: "imagegen_script is unset"})
	} else {
		comfyUsed, nodeUsed = true, true
		out = append(out, fileRoute("generate_image", "comfyui", exeDir,
			binding{key: "imagegen_script", value: cfg.ImageGenScript, kind: scriptBinding},
		))
	}

	// --- inpaint_image: script AND checkpoint, matching the pipeline's gate.
	// inpaint_ckpt is a ComfyUI model FILENAME, not a path, so it is reported but
	// never stat'd — claiming a verdict on it would be a guess.
	if cfg.InpaintScript == "" || cfg.InpaintCkpt == "" {
		out = append(out, Route{Name: "inpaint_image", Engine: "comfyui", State: NotConfigured,
			Detail: "inpaint_script/inpaint_ckpt is unset"})
	} else {
		comfyUsed, nodeUsed = true, true
		r := fileRoute("inpaint_image", "comfyui", exeDir,
			binding{key: "inpaint_script", value: cfg.InpaintScript, kind: scriptBinding},
		)
		r.Detail += fmt.Sprintf("; inpaint_ckpt=%s (ComfyUI model name, not checked here)", cfg.InpaintCkpt)
		out = append(out, r)
	}

	// --- edit_image_generative: script AND unet, matching the pipeline's gate.
	// edit_unet is a ComfyUI model FILENAME, not a path, so it is reported but never
	// stat'd — same rule as inpaint_ckpt.
	if cfg.GenEditScript == "" || cfg.GenEditUnet == "" {
		out = append(out, Route{Name: "edit_image_generative", Engine: "comfyui", State: NotConfigured,
			Detail: "gen_edit_script/gen_edit_unet is unset"})
	} else {
		comfyUsed, nodeUsed = true, true
		r := fileRoute("edit_image_generative", "comfyui", exeDir,
			binding{key: "gen_edit_script", value: cfg.GenEditScript, kind: scriptBinding},
		)
		r.Detail += fmt.Sprintf("; gen_edit_unet=%s preset=%s (ComfyUI model name, not checked here)",
			cfg.GenEditUnet, cfg.GenEditPreset)
		out = append(out, r)
	}

	// --- upscale_image: script AND an ESRGAN filename, matching the pipeline's gate
	// (config.EffectiveUpscaleModel: upscale_model, else videogen_upscale_model). The
	// filename is a ComfyUI model NAME, reported but never stat'd — same rule as inpaint_ckpt.
	if model := cfg.EffectiveUpscaleModel(); cfg.UpscaleScript == "" || model == "" {
		out = append(out, Route{Name: "upscale_image", Engine: "comfyui", State: NotConfigured,
			Detail: "upscale_script/upscale_model is unset (videogen_upscale_model is the fallback)"})
	} else {
		comfyUsed, nodeUsed = true, true
		r := fileRoute("upscale_image", "comfyui", exeDir,
			binding{key: "upscale_script", value: cfg.UpscaleScript, kind: scriptBinding},
		)
		src := "upscale_model"
		if cfg.UpscaleModel == "" {
			src = "videogen_upscale_model fallback"
		}
		r.Detail += fmt.Sprintf("; upscale_model=%s (%s; ComfyUI model name, not checked here)", model, src)
		out = append(out, r)
	}

	// --- the plain script routes ---
	for _, s := range []struct {
		name, engine, key, value string
		comfy                    bool
	}{
		{"generate_video", "comfyui", "videogen_script", cfg.VideoGenScript, true},
		{"animate_character", "comfyui", "animategen_script", cfg.AnimateGenScript, true},
		{"generate_audio:voice", "chatterbox-tts", "voicegen_script", cfg.VoiceGenScript, false},
		{"generate_audio:music", "acestep", "musicgen_script", cfg.MusicGenScript, true},
		{"run_graph", "comfyui", "run_graph_script", cfg.RunGraphScript, true},
	} {
		if s.value == "" {
			out = append(out, Route{Name: s.name, Engine: s.engine, State: NotConfigured,
				Detail: s.key + " is unset"})
			continue
		}
		nodeUsed = true
		comfyUsed = comfyUsed || s.comfy
		out = append(out, fileRoute(s.name, s.engine, exeDir,
			binding{key: s.key, value: s.value, kind: scriptBinding},
		))
	}

	// --- edit_image (PIL): an explicit python is a binding; an unset one derives
	// <comfy_dir>/.venv, which is a discovery, so its absence is "not configured".
	if py := mediaops.ResolveEditPython(cfg.EditPython, cfg.ComfyDir); py != "" {
		out = append(out, Route{Name: "edit_image", Engine: "pil", State: Configured,
			Detail: "edit_python=" + py})
	} else if cfg.EditPython != "" {
		out = append(out, Route{Name: "edit_image", Engine: "pil", State: BoundButMissing,
			Detail: "edit_python=" + cfg.EditPython + " does not exist"})
	} else {
		out = append(out, Route{Name: "edit_image", Engine: "pil", State: NotConfigured,
			Detail: "edit_python unset and no <comfy_dir>/.venv python found"})
	}

	// --- the CPU tools: flatten_design (GIMP) and offload_media (ffmpeg) ---
	out = append(out,
		binRoute("flatten_design", "gimp", "gimp_console_path", cfg.GimpConsolePath),
		binRoute("media", "ffmpeg", "ffmpeg_path", cfg.FFmpegPath),
	)

	// --- prereqs: reported only when something on this box actually needs them,
	// so an sdcpp-only node is never told it is missing ComfyUI.
	if nodeUsed {
		r := binRoute("node", "runtime", "node_path", cfg.NodePath)
		r.Prereq = true
		r.Detail += " — every render script runs under it"
		out = append(out, r)
	}
	if comfyUsed {
		out = append(out, comfyDirRoute(cfg.ComfyDir))
	}
	return out
}

// bindingKind selects how a configured value's presence is decided.
type bindingKind int

const (
	scriptBinding bindingKind = iota // relative => resolved against the exe dir
	fileBinding                      // a plain path to a model/asset file
	binaryBinding                    // a path, or a bare name found on PATH
)

type binding struct {
	key, value string
	kind       bindingKind
}

// fileRoute stats every binding and returns CONFIGURED only when they all
// resolve; the first absent one explains itself in the verdict.
func fileRoute(name, engine, exeDir string, bindings ...binding) Route {
	var resolved []string
	for _, b := range bindings {
		got, why, ok := b.resolve(exeDir)
		if !ok {
			return Route{Name: name, Engine: engine, State: BoundButMissing, Detail: why}
		}
		resolved = append(resolved, b.key+"="+got)
	}
	return Route{Name: name, Engine: engine, State: Configured, Detail: joinDetail(resolved)}
}

// resolve answers "is this binding satisfied?" and, when it is not, says exactly
// where it looked — a relative script binding is meaningless to an operator
// until the ABSOLUTE path it resolved to is on screen.
func (b binding) resolve(exeDir string) (got, why string, ok bool) {
	switch b.kind {
	case scriptBinding:
		p, err := gpugen.ResolveScriptIn(b.value, exeDir)
		if err != nil {
			return "", fmt.Sprintf("%s=%s: %v", b.key, b.value, err), false
		}
		return p, "", true
	case binaryBinding:
		if p, found := binaryPresent(b.value); found {
			return p, "", true
		}
		return "", fmt.Sprintf("%s=%s not found (no such file, and not on PATH)", b.key, b.value), false
	default:
		if fi, err := os.Stat(b.value); err == nil && !fi.IsDir() {
			return b.value, "", true
		}
		return "", fmt.Sprintf("%s=%s does not exist", b.key, b.value), false
	}
}

// binRoute is a single-binary route (GIMP, ffmpeg, node): unset is a legitimate
// "not configured", set-but-absent is the trap worth naming.
func binRoute(name, engine, key, value string) Route {
	if value == "" {
		return Route{Name: name, Engine: engine, State: NotConfigured, Detail: key + " is unset"}
	}
	if got, ok := binaryPresent(value); ok {
		return Route{Name: name, Engine: engine, State: Configured, Detail: key + "=" + got}
	}
	return Route{Name: name, Engine: engine, State: BoundButMissing,
		Detail: key + "=" + value + " not found (no such file, and not on PATH)"}
}

// comfyDirRoute checks the ComfyUI install every comfy-backed route drives.
func comfyDirRoute(dir string) Route {
	r := Route{Name: "comfyui", Engine: "runtime", Prereq: true}
	switch fi, err := os.Stat(dir); {
	case dir == "":
		r.State, r.Detail = NotConfigured, "comfy_dir is unset — every ComfyUI-backed route above will fail"
	case err == nil && fi.IsDir():
		r.State, r.Detail = Configured, "comfy_dir="+dir
	default:
		r.State, r.Detail = BoundButMissing, "comfy_dir="+dir+" is not a directory on this machine"
	}
	return r
}

// binaryPresent resolves an executable binding the way the runners spawn it: an
// explicit path is stat'd, a bare name (the shipped "node"/"ffmpeg" defaults) is
// looked up on PATH.
func binaryPresent(bin string) (string, bool) {
	if bin == "" {
		return "", false
	}
	if fi, err := os.Stat(bin); err == nil && !fi.IsDir() {
		return bin, true
	}
	if p, err := exec.LookPath(bin); err == nil {
		return p, true
	}
	return "", false
}

func joinDetail(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "; "
		}
		out += p
	}
	return out
}

// Map renders routes as the JSON media block offload_status returns: one entry
// per route name, each carrying its verdict, engine and bindings.
func Map(routes []Route) map[string]any {
	m := make(map[string]any, len(routes))
	for _, r := range routes {
		e := map[string]any{"state": string(r.State), "engine": r.Engine, "detail": r.Detail}
		if r.Prereq {
			e["prereq"] = true
		}
		m[r.Name] = e
	}
	return m
}

// Missing returns the routes whose configured files are absent — the actionable
// subset (a caller's exit code, an operator's fix list). NOT CONFIGURED is never
// in it: deferring a task this box was never bound for is correct behavior.
func Missing(routes []Route) []Route {
	var out []Route
	for _, r := range routes {
		if r.State == BoundButMissing {
			out = append(out, r)
		}
	}
	return out
}
