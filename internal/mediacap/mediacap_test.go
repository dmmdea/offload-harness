package mediacap

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// touch creates an empty file at exeDir/rel (making parents) and returns its path.
func touch(t *testing.T, dir, rel string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// byName indexes routes for assertions.
func byName(routes []Route) map[string]Route {
	m := map[string]Route{}
	for _, r := range routes {
		m[r.Name] = r
	}
	return m
}

// bare is a config with every media binding cleared — the starting point for
// tests that assert one route at a time (config.Default() ships four script
// bindings, which is the right default and the wrong test fixture).
func bare() config.Config {
	cfg := config.Default()
	cfg.ImageGenScript, cfg.InpaintScript, cfg.InpaintCkpt = "", "", ""
	cfg.VideoGenScript, cfg.VoiceGenScript, cfg.MusicGenScript, cfg.RunGraphScript = "", "", "", ""
	cfg.ImageGenEngine, cfg.SdcppBin, cfg.SdcppModel, cfg.SdcppScript = "", "", "", ""
	cfg.EditPython, cfg.GimpConsolePath, cfg.FFmpegPath, cfg.NodePath = "", "", "", ""
	cfg.ComfyDir = ""
	return cfg
}

// TestSdcppEngineIsReportedAsSdcpp is the regression this package exists for:
// offload_status declared "ComfyUI (local)" as a constant and handed it to a
// planner on a node whose engine is stable-diffusion.cpp and which has no
// ComfyUI at all. The engine must come from the binding, and the ComfyUI prereq
// must not be claimed by a box that never touches it.
func TestSdcppEngineIsReportedAsSdcpp(t *testing.T) {
	exeDir := t.TempDir()
	cfg := bare()
	cfg.ImageGenEngine = "sdcpp"
	cfg.SdcppBin = touch(t, exeDir, "sd-cli")
	cfg.SdcppModel = touch(t, exeDir, "models/sdxl-turbo.gguf")
	cfg.SdcppScript = "render/sdcpp-generate.mjs"
	touch(t, exeDir, "render/sdcpp-generate.mjs")
	cfg.NodePath = "node"

	got := byName(routesIn(cfg, exeDir))
	img := got["generate_image"]
	if img.Engine != "sdcpp" {
		t.Errorf("engine = %q, want sdcpp", img.Engine)
	}
	if img.State != Configured {
		t.Errorf("state = %q (%s), want CONFIGURED", img.State, img.Detail)
	}
	if _, ok := got["comfyui"]; ok {
		t.Error("an sdcpp-only box must not be told it is missing ComfyUI")
	}
	if _, ok := got["node"]; !ok {
		t.Error("the sdcpp runner is a node script, so the node prereq must be reported")
	}
}

// TestBoundButMissingIsNotUnset: the middle verdict is the one worth having —
// a config that names a script which is not on disk must NOT read the same as a
// box that never bound one. The first defers with a runner error nobody
// reported; the second defers by design.
func TestBoundButMissingIsNotUnset(t *testing.T) {
	exeDir := t.TempDir()
	cfg := bare()
	cfg.VideoGenScript = "render/comfy-video.mjs" // bound, absent
	cfg.RunGraphScript = ""                       // never bound

	got := byName(routesIn(cfg, exeDir))
	if got["generate_video"].State != BoundButMissing {
		t.Errorf("generate_video = %q, want BOUND-BUT-MISSING", got["generate_video"].State)
	}
	if !strings.Contains(got["generate_video"].Detail, exeDir) {
		t.Errorf("the verdict must name the absolute path it looked at, got %q", got["generate_video"].Detail)
	}
	if got["run_graph"].State != NotConfigured {
		t.Errorf("run_graph = %q, want NOT CONFIGURED", got["run_graph"].State)
	}
	if n := len(Missing(routesIn(cfg, exeDir))); n != 1 {
		t.Errorf("Missing() = %d routes, want only the bound-but-absent one", n)
	}
}

// TestRelativeScriptResolvesAgainstExeDir: the shipped defaults are relative
// ("render/*.mjs") and the runners resolve them against the EXECUTABLE's dir,
// never the cwd. A verdict computed any other way would be a different question
// than the one the pipeline asks.
func TestRelativeScriptResolvesAgainstExeDir(t *testing.T) {
	exeDir := t.TempDir()
	cfg := bare()
	cfg.VideoGenScript = "render/comfy-video.mjs"
	cfg.ComfyDir = t.TempDir()
	cfg.NodePath = "node"
	touch(t, exeDir, "render/comfy-video.mjs")

	got := byName(routesIn(cfg, exeDir))
	if got["generate_video"].State != Configured {
		t.Fatalf("generate_video = %q (%s), want CONFIGURED", got["generate_video"].State, got["generate_video"].Detail)
	}
	// A ComfyUI-backed route is bound, so the install it drives is reported too.
	if got["comfyui"].State != Configured || !got["comfyui"].Prereq {
		t.Errorf("comfyui prereq = %+v, want a CONFIGURED prereq row", got["comfyui"])
	}
}

// TestNoPrereqRowsWhenNothingNeedsThem: a box with no media bindings gets a
// clean "not configured" list, not a fault report about tools it never uses.
func TestNoPrereqRowsWhenNothingNeedsThem(t *testing.T) {
	routes := routesIn(bare(), t.TempDir())
	got := byName(routes)
	for _, k := range []string{"node", "comfyui"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s prereq must not be reported when nothing is bound", k)
		}
	}
	if n := len(Missing(routes)); n != 0 {
		t.Errorf("an unbound box has nothing missing, got %d", n)
	}
	for _, r := range routes {
		if r.State != NotConfigured {
			t.Errorf("%s = %q, want NOT CONFIGURED on a bare box (%s)", r.Name, r.State, r.Detail)
		}
	}
}

// TestBinaryBindingUsesPathLookup: node/ffmpeg ship as bare names and are found
// on PATH exactly as the runners spawn them — a stat-only check would report a
// working machine as broken.
func TestBinaryBindingUsesPathLookup(t *testing.T) {
	name := "go"
	if runtime.GOOS == "windows" {
		name = "go.exe"
	}
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("no %s on PATH in this environment", name)
	}
	cfg := bare()
	cfg.FFmpegPath = name // stands in for a bare "ffmpeg"
	got := byName(routesIn(cfg, t.TempDir()))
	if got["media"].State != Configured {
		t.Errorf("media = %q (%s), want CONFIGURED via PATH", got["media"].State, got["media"].Detail)
	}

	cfg.FFmpegPath = "definitely-not-a-real-binary-xyz"
	got = byName(routesIn(cfg, t.TempDir()))
	if got["media"].State != BoundButMissing {
		t.Errorf("media = %q, want BOUND-BUT-MISSING for an unresolvable binary", got["media"].State)
	}
}

// TestEditPythonExplicitVsDerived: an explicit edit_python that does not exist
// is a broken promise; an unset one that finds no venv is simply a box without
// the PIL engine.
func TestEditPythonExplicitVsDerived(t *testing.T) {
	cfg := bare()
	cfg.EditPython = filepath.Join(t.TempDir(), "python-that-is-not-there")
	if got := byName(routesIn(cfg, t.TempDir()))["edit_image"]; got.State != BoundButMissing {
		t.Errorf("explicit missing edit_python = %q, want BOUND-BUT-MISSING", got.State)
	}
	cfg.EditPython = ""
	if got := byName(routesIn(cfg, t.TempDir()))["edit_image"]; got.State != NotConfigured {
		t.Errorf("unset edit_python with no venv = %q, want NOT CONFIGURED", got.State)
	}
}

// TestMapCarriesVerdictAndEngine: the JSON offload_status returns must expose
// both, so a planner can tell "this box cannot" from "this box is broken".
func TestMapCarriesVerdictAndEngine(t *testing.T) {
	m := Map([]Route{
		{Name: "generate_image", Engine: "sdcpp", State: Configured, Detail: "sdcpp_bin=/x"},
		{Name: "node", Engine: "runtime", State: BoundButMissing, Detail: "node_path=node", Prereq: true},
	})
	img, _ := m["generate_image"].(map[string]any)
	if img == nil || img["state"] != string(Configured) || img["engine"] != "sdcpp" {
		t.Errorf("generate_image entry = %v", m["generate_image"])
	}
	node, _ := m["node"].(map[string]any)
	if node == nil || node["prereq"] != true {
		t.Errorf("a prereq row must say so: %v", m["node"])
	}
	if _, ok := img["prereq"]; ok {
		t.Error("a task route must not be marked prereq")
	}
}

// TestDefaultConfigNamesEveryShippedRoute guards the derivation against a new
// media binding landing in config without a route here — the silent way this
// map starts lying again.
func TestDefaultConfigNamesEveryShippedRoute(t *testing.T) {
	got := byName(routesIn(config.Default(), t.TempDir()))
	for _, want := range []string{
		"generate_image", "inpaint_image", "generate_video",
		"generate_audio:voice", "generate_audio:music", "run_graph",
		"edit_image", "flatten_design", "media", "upscale_image",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("no route reported for %s", want)
		}
	}
}
