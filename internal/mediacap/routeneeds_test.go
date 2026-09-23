package mediacap

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

// installPacks creates active custom-node pack directories (each with __init__.py).
func installPacks(t *testing.T, comfyDir string, packs ...string) {
	t.Helper()
	for _, p := range packs {
		touch(t, comfyDir, "custom_nodes/"+p+"/__init__.py")
	}
}

// installModels creates model files under <comfyDir>/models/<class>/<name>.
func installModels(t *testing.T, comfyDir string, classFiles ...string) {
	t.Helper()
	for _, cf := range classFiles {
		touch(t, comfyDir, "models/"+cf)
	}
}

// wanModels is every file the default Wan graph loads, plus the fast-mode LoRAs.
func wanModels(withLoras bool) []string {
	out := []string{
		"unet/" + wanDefaults.highUnet, "unet/" + wanDefaults.lowUnet,
		"text_encoders/" + wanDefaults.textEncoder, "vae/" + wanDefaults.vae,
	}
	if withLoras {
		out = append(out, "loras/"+wanDefaults.highLora, "loras/"+wanDefaults.lowLora)
	}
	return out
}

// videoBox is a box with the video script bound and a ComfyUI install at comfyDir.
func videoBox(t *testing.T) (cfg config.Config, exeDir, comfyDir string) {
	t.Helper()
	exeDir, comfyDir = t.TempDir(), t.TempDir()
	cfg = bare()
	cfg.VideoGenScript = "render/comfy-video.mjs"
	cfg.ComfyDir = comfyDir
	cfg.NodePath = "node"
	touch(t, exeDir, "render/comfy-video.mjs")
	return cfg, exeDir, comfyDir
}

// TestVideoRouteNeedsTheWanNodePacks is the OptiPlex regression: ComfyUI-GGUF and
// ComfyUI-MultiGPU installed, VideoHelperSuite absent, every model present. The route
// was CONFIGURED; the lane failed on its first POST. It must name the class and pack.
func TestVideoRouteNeedsTheWanNodePacks(t *testing.T) {
	cfg, exeDir, comfyDir := videoBox(t)
	installPacks(t, comfyDir, "ComfyUI-GGUF", "ComfyUI-MultiGPU")
	installModels(t, comfyDir, wanModels(true)...)

	got := byName(routesIn(cfg, exeDir))["generate_video"]
	if got.State != BoundButMissing {
		t.Fatalf("generate_video = %q (%s), want BOUND-BUT-MISSING", got.State, got.Detail)
	}
	for _, want := range []string{"VHS_VideoCombine", "ComfyUI-VideoHelperSuite", filepath.Join(comfyDir, "custom_nodes")} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail must name %q: %s", want, got.Detail)
		}
	}
	if strings.Contains(got.Detail, "UnetLoaderGGUFDisTorch2MultiGPU (") {
		t.Errorf("the installed GGUF DisTorch2 loader must not be reported missing: %s", got.Detail)
	}

	installPacks(t, comfyDir, "ComfyUI-VideoHelperSuite")
	got = byName(routesIn(cfg, exeDir))["generate_video"]
	if got.State != Configured {
		t.Fatalf("with VHS installed generate_video = %q (%s), want CONFIGURED", got.State, got.Detail)
	}
	if !strings.Contains(got.Detail, "nodes UnetLoaderGGUFDisTorch2MultiGPU,VHS_VideoCombine") {
		t.Errorf("a CONFIGURED route must say which node classes it checked: %s", got.Detail)
	}
}

// TestVideoRouteNeedsItsBuilderDefaultFiles: an unset text encoder is not "nothing to
// check" — the graph loads the builder's umt5 default, and the VAE has no key at all.
func TestVideoRouteNeedsItsBuilderDefaultFiles(t *testing.T) {
	cfg, exeDir, comfyDir := videoBox(t)
	installPacks(t, comfyDir, "ComfyUI-VideoHelperSuite", "ComfyUI-MultiGPU", "ComfyUI-GGUF")
	cfg.VideoGenUnetHigh, cfg.VideoGenUnetLow = "Wan2.2-I2V-A14B-HighNoise-Q8_0.gguf", "Wan2.2-I2V-A14B-LowNoise-Q8_0.gguf"
	installModels(t, comfyDir, "unet/"+cfg.VideoGenUnetHigh, "unet/"+cfg.VideoGenUnetLow)

	got := byName(routesIn(cfg, exeDir))["generate_video"]
	if got.State != BoundButMissing {
		t.Fatalf("generate_video = %q (%s), want BOUND-BUT-MISSING", got.State, got.Detail)
	}
	for _, want := range []string{
		"videogen_text_encoder (wan22 builder default)=" + wanDefaults.textEncoder,
		"wan vae (builder default)=" + wanDefaults.vae,
	} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail must name %q: %s", want, got.Detail)
		}
	}
	if strings.Contains(got.Detail, "videogen_unet_high=") {
		t.Errorf("a bound, present expert must not be reported: %s", got.Detail)
	}
}

// TestVideoFastLorasAreReportedNotRequired: the lightx2v LoRAs are loaded only by
// fast=true. Their absence is named, never a route failure.
func TestVideoFastLorasAreReportedNotRequired(t *testing.T) {
	cfg, exeDir, comfyDir := videoBox(t)
	installPacks(t, comfyDir, "ComfyUI-VideoHelperSuite", "ComfyUI-MultiGPU", "ComfyUI-GGUF")
	installModels(t, comfyDir, wanModels(false)...)
	got := byName(routesIn(cfg, exeDir))["generate_video"]
	if got.State != Configured {
		t.Fatalf("generate_video = %q (%s), want CONFIGURED", got.State, got.Detail)
	}
	if !strings.Contains(got.Detail, "not available per request: fast mode high lora") {
		t.Errorf("the missing fast-mode LoRA must be named: %s", got.Detail)
	}
}

// TestDiskNodeCheckActivePacksOnly: a disabled pack (ComfyUI-Manager's .disabled suffix)
// or a directory without __init__.py registers nothing; a renamed pack is found by the
// marker class in its source; the lowercase registry name matches.
func TestDiskNodeCheckActivePacksOnly(t *testing.T) {
	comfy := t.TempDir()
	touch(t, comfy, "custom_nodes/ComfyUI-VideoHelperSuite.disabled/__init__.py")
	touch(t, comfy, "custom_nodes/ComfyUI-GGUF/nodes.py") // no __init__.py
	if got := DiskNodeCheck(comfy, []string{"VHS_VideoCombine", "UnetLoaderGGUF", "KSampler"}); strings.Join(got.Missing, ",") != "VHS_VideoCombine,UnetLoaderGGUF" {
		t.Fatalf("missing = %v, want the disabled and the non-package pack (KSampler is core)", got.Missing)
	}
	touch(t, comfy, "custom_nodes/my-video-nodes/__init__.py")
	if err := os.WriteFile(filepath.Join(comfy, "custom_nodes", "my-video-nodes", "videohelpersuite", "nodes.py"), nil, 0o644); err == nil {
		t.Fatal("setup: expected no such directory yet")
	}
	touch(t, comfy, "custom_nodes/my-video-nodes/videohelpersuite/nodes.py")
	if err := os.WriteFile(filepath.Join(comfy, "custom_nodes", "my-video-nodes", "videohelpersuite", "nodes.py"),
		[]byte(`NODE_CLASS_MAPPINGS = {"VHS_VideoCombine": VideoCombine}`), 0o644); err != nil {
		t.Fatal(err)
	}
	touch(t, comfy, "custom_nodes/comfyui-gguf/__init__.py")
	if got := DiskNodeCheck(comfy, []string{"VHS_VideoCombine", "UnetLoaderGGUF"}); len(got.Missing) != 0 {
		t.Fatalf("a renamed pack (marker) and a lowercase pack name must both count; missing %v", got.Missing)
	}
}

// TestLiveNodeCheckerAsksRunningComfyThenFallsBack: a running ComfyUI answers from
// /object_info (which also sees a pack that failed to import); a server that is not
// there falls back to the disk check and says so.
func TestLiveNodeCheckerAsksRunningComfyThenFallsBack(t *testing.T) {
	comfy := t.TempDir()
	installPacks(t, comfy, "ComfyUI-VideoHelperSuite") // on disk, "failed to import" live
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cls := strings.TrimPrefix(r.URL.Path, "/object_info/")
		if cls == "VHS_VideoCombine" {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"` + cls + `":{"input":{}}}`))
	}))
	live := LiveNodeChecker(srv.URL, 2*time.Second)(comfy, []string{"VHS_VideoCombine", "KSampler"})
	if strings.Join(live.Missing, ",") != "VHS_VideoCombine" || !strings.HasPrefix(live.Via, "live /object_info") {
		t.Fatalf("live check = %+v, want VHS missing via /object_info", live)
	}
	srv.Close()
	down := LiveNodeChecker(srv.URL, 2*time.Second)(comfy, []string{"VHS_VideoCombine"})
	if len(down.Missing) != 0 || !strings.Contains(down.Via, "custom_nodes on disk (ComfyUI not answering") {
		t.Fatalf("a stopped ComfyUI must fall back to the disk check and say so: %+v", down)
	}
}

// TestLtx25NeedsMultiGPUOnlyWhenPooled: the single-card LTX graph is core; the pooled
// one loads through ComfyUI-MultiGPU.
func TestLtx25NeedsMultiGPUOnlyWhenPooled(t *testing.T) {
	cfg := bare()
	cfg.VideoGenFamily = "ltx25"
	if _, _, classes, _ := videoNeeds(cfg); len(classes) != 0 {
		t.Fatalf("single-card ltx25 needs no custom nodes, got %v", classes)
	}
	cfg.VideoGenPoolVvramGB = 30
	if _, _, classes, _ := videoNeeds(cfg); strings.Join(classes, ",") != "UNETLoaderDisTorch2MultiGPU" {
		t.Fatalf("pooled ltx25 needs the MultiGPU loader, got %v", classes)
	}
}

// TestAnimateAndMusicNeedTheirModelFiles: the OptiPlex animate row read CONFIGURED with
// no WAN-Animate-2 weights on the box; music was script-only too.
func TestAnimateAndMusicNeedTheirModelFiles(t *testing.T) {
	exeDir, comfy := t.TempDir(), t.TempDir()
	cfg := bare()
	cfg.ComfyDir, cfg.NodePath = comfy, "node"
	cfg.AnimateGenScript, cfg.MusicGenScript = "render/comfy-animate.mjs", "render/comfy-music.mjs"
	touch(t, exeDir, "render/comfy-animate.mjs")
	touch(t, exeDir, "render/comfy-music.mjs")
	// umt5 + wan VAE are there (the video lane's), the animate DiT and clip vision are not.
	installModels(t, comfy, "text_encoders/"+animateDefaults.textEncoder, "vae/"+animateDefaults.vae)

	got := byName(routesIn(cfg, exeDir))
	a := got["animate_character"]
	if a.State != BoundButMissing {
		t.Fatalf("animate_character = %q (%s), want BOUND-BUT-MISSING", a.State, a.Detail)
	}
	for _, want := range []string{animateDefaults.unet, animateDefaults.clipVision} {
		if !strings.Contains(a.Detail, want) {
			t.Errorf("animate detail must name %q: %s", want, a.Detail)
		}
	}
	if m := got["generate_audio:music"]; m.State != BoundButMissing || !strings.Contains(m.Detail, aceDefaults.unet) {
		t.Fatalf("generate_audio:music = %q (%s), want BOUND-BUT-MISSING naming the ACE-Step DiT", m.State, m.Detail)
	}

	installModels(t, comfy, "diffusion_models/"+animateDefaults.unet, "clip_vision/"+animateDefaults.clipVision,
		"diffusion_models/"+aceDefaults.unet, "text_encoders/"+aceDefaults.clip1, "text_encoders/"+aceDefaults.clip2, "vae/"+aceDefaults.vae)
	got = byName(routesIn(cfg, exeDir))
	for _, n := range []string{"animate_character", "generate_audio:music"} {
		if got[n].State != Configured {
			t.Errorf("%s = %q (%s), want CONFIGURED once its files are there", n, got[n].State, got[n].Detail)
		}
	}
}

// voiceBox binds the voice script under <repo>/render and returns the repo root, with
// PATH emptied so a python on the test machine never answers for the venv.
func voiceBox(t *testing.T) (cfg config.Config, exeDir, repo string) {
	t.Helper()
	exeDir = t.TempDir()
	repo = exeDir
	cfg = bare()
	cfg.VoiceGenScript = "render/tts.mjs"
	cfg.NodePath = "node"
	touch(t, exeDir, "render/tts.mjs")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("TTS_PY", "")
	t.Setenv("HF_HUB_CACHE", filepath.Join(t.TempDir(), "hub"))
	return cfg, exeDir, repo
}

// TestVoiceRouteNeedsVenvPackagesAndWeights is the OptiPlex voice row: the script was
// on disk, so voice read CONFIGURED, while the runner had no venv (it fell back to a
// system python without torch or chatterbox) and no weights were cached.
func TestVoiceRouteNeedsVenvPackagesAndWeights(t *testing.T) {
	cfg, exeDir, repo := voiceBox(t)

	v := byName(routesIn(cfg, exeDir))["generate_audio:voice"]
	if v.State != BoundButMissing || !strings.Contains(v.Detail, "no TTS python") || !strings.Contains(v.Detail, ".tts-venv") {
		t.Fatalf("no venv and no python: voice = %q (%s), want BOUND-BUT-MISSING naming the .tts-venv", v.State, v.Detail)
	}

	touch(t, repo, ".tts-venv/Scripts/python.exe")
	touch(t, repo, ".tts-venv/pyvenv.cfg")
	touch(t, repo, ".tts-venv/Lib/site-packages/torch/__init__.py")
	v = byName(routesIn(cfg, exeDir))["generate_audio:voice"]
	if v.State != BoundButMissing || !strings.Contains(v.Detail, "has no chatterbox package") || strings.Contains(v.Detail, "has no torch package") {
		t.Fatalf("venv without chatterbox: voice = %q (%s), want the chatterbox package named (and only it)", v.State, v.Detail)
	}
	if !strings.Contains(v.Detail, "Chatterbox weights are not in the Hugging Face cache") {
		t.Errorf("uncached weights must be named in the same pass: %s", v.Detail)
	}

	touch(t, repo, ".tts-venv/Lib/site-packages/chatterbox/mtl_tts.py")
	touch(t, os.Getenv("HF_HUB_CACHE"), chatterboxRepo+"/snapshots/abc123/config.json")
	v = byName(routesIn(cfg, exeDir))["generate_audio:voice"]
	if v.State != Configured || !strings.Contains(v.Detail, "(.tts-venv) with chatterbox + torch") {
		t.Fatalf("venv + packages + weights: voice = %q (%s), want CONFIGURED", v.State, v.Detail)
	}
}

// TestVoiceRouteHonoursTTSPY: TTS_PY is the runner's first choice; one that points at
// nothing is not quietly replaced by the venv (tts.mjs would spawn the missing path).
func TestVoiceRouteHonoursTTSPY(t *testing.T) {
	cfg, exeDir, repo := voiceBox(t)
	touch(t, repo, ".tts-venv/Scripts/python.exe")
	t.Setenv("TTS_PY", filepath.Join(t.TempDir(), "nope", "python.exe"))
	v := byName(routesIn(cfg, exeDir))["generate_audio:voice"]
	if v.State != BoundButMissing || !strings.Contains(v.Detail, "no TTS python") {
		t.Fatalf("a TTS_PY that does not exist: voice = %q (%s), want BOUND-BUT-MISSING", v.State, v.Detail)
	}
}

// TestEditRouteNeedsGGUFForAGGUFUnet: the default 2511 edit binding loads a .gguf
// through ComfyUI-GGUF.
func TestEditRouteNeedsGGUFForAGGUFUnet(t *testing.T) {
	exeDir, comfy := t.TempDir(), t.TempDir()
	cfg := bare()
	cfg.ComfyDir, cfg.NodePath = comfy, "node"
	cfg.GenEditScript, cfg.GenEditUnet = "render/comfy-edit.mjs", "qwen-image-edit-2511-Q5_1.gguf"
	touch(t, exeDir, "render/comfy-edit.mjs")
	if e := byName(routesIn(cfg, exeDir))["edit_image_generative"]; e.State != BoundButMissing || !strings.Contains(e.Detail, "UnetLoaderGGUF (custom node pack ComfyUI-GGUF") {
		t.Fatalf("edit without ComfyUI-GGUF = %q (%s), want BOUND-BUT-MISSING naming the pack", e.State, e.Detail)
	}
	installPacks(t, comfy, "ComfyUI-GGUF")
	if e := byName(routesIn(cfg, exeDir))["edit_image_generative"]; e.State != Configured {
		t.Fatalf("edit with ComfyUI-GGUF = %q (%s), want CONFIGURED", e.State, e.Detail)
	}
}

// TestRouteNeedsMirrorTheRenderBuilders: the builder defaults and the custom-node table
// above are copies of render/wf-*.mjs. Parse the builders and fail when they drift — a
// renamed default would otherwise make doctor check a file no graph loads.
func TestRouteNeedsMirrorTheRenderBuilders(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("..", "..", "render", name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	def := func(src, param string) string {
		t.Helper()
		m := regexp.MustCompile(`\b` + param + ` = "([^"]+)"`).FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("no %s default in the builder signature", param)
		}
		return m[1]
	}
	for _, c := range []struct {
		file, param, want string
	}{
		{"wf-wan22-i2v.mjs", "highUnet", wanDefaults.highUnet},
		{"wf-wan22-i2v.mjs", "lowUnet", wanDefaults.lowUnet},
		{"wf-wan22-i2v.mjs", "highLora", wanDefaults.highLora},
		{"wf-wan22-i2v.mjs", "lowLora", wanDefaults.lowLora},
		{"wf-wan22-i2v.mjs", "textEncoder", wanDefaults.textEncoder},
		{"wf-wan22-i2v.mjs", "vae", wanDefaults.vae},
		{"wf-ltx25-i2v.mjs", "transformer", ltxDefaults.transformer},
		{"wf-ltx25-i2v.mjs", "textEncoder", ltxDefaults.textEncoder},
		{"wf-ltx25-i2v.mjs", "videoVae", ltxDefaults.videoVae},
		{"wf-ltx25-i2v.mjs", "audioVae", ltxDefaults.audioVae},
		{"wf-ltx25-i2v.mjs", "latentUpscaler", ltxDefaults.latentUpscaler},
		{"wf-h3-av.mjs", "transformer", h3Defaults.transformer},
		{"wf-h3-av.mjs", "textEncoder", h3Defaults.textEncoder},
		{"wf-h3-av.mjs", "videoVae", h3Defaults.videoVae},
		{"wf-h3-av.mjs", "audioVae", h3Defaults.audioVae},
		{"wf-h3-av.mjs", "turboLora", h3Defaults.turboLora},
		{"wf-hunyuan15-i2v.mjs", "unet", hunyuanDefaults.unet},
		{"wf-hunyuan15-i2v.mjs", "vae", hunyuanDefaults.vae},
		{"wf-hunyuan15-i2v.mjs", "clipVision", hunyuanDefaults.clipVision},
		{"wf-hunyuan15-i2v.mjs", "textEncoder", hunyuanDefaults.textEncoder},
		{"wf-hunyuan15-i2v.mjs", "glyphEncoder", hunyuanDefaults.glyphEncoder},
		{"wf-wan-animate2.mjs", "unet", animateDefaults.unet},
		{"wf-wan-animate2.mjs", "textEncoder", animateDefaults.textEncoder},
		{"wf-wan-animate2.mjs", "clipVision", animateDefaults.clipVision},
		{"wf-wan-animate2.mjs", "vae", animateDefaults.vae},
		{"wf-acestep.mjs", "unet", aceDefaults.unet},
		{"wf-acestep.mjs", "clip1", aceDefaults.clip1},
		{"wf-acestep.mjs", "clip2", aceDefaults.clip2},
		{"wf-acestep.mjs", "vae", aceDefaults.vae},
	} {
		if got := def(read(c.file), c.param); got != c.want {
			t.Errorf("%s %s default: builder %q, mediacap %q", c.file, c.param, got, c.want)
		}
	}

	// Every custom-node class any builder emits has a pack entry (the Go table and the
	// runner's NODE_PACKS in render/comfy-nodes.mjs).
	entries, err := os.ReadDir(filepath.Join("..", "..", "render"))
	if err != nil {
		t.Fatal(err)
	}
	nodes := read("comfy-nodes.mjs")
	classRe := regexp.MustCompile(`class_type: "([^"]+)"`)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "wf-") || strings.HasSuffix(e.Name(), ".test.mjs") {
			continue
		}
		for _, m := range classRe.FindAllStringSubmatch(read(e.Name()), -1) {
			c := m[1]
			if !regexp.MustCompile(`GGUF|MultiGPU|^VHS_`).MatchString(c) {
				continue
			}
			if _, ok := classPacks[c]; !ok {
				t.Errorf("%s emits custom-node class %s, which classPacks does not list", e.Name(), c)
			}
			if !strings.Contains(nodes, "  "+c+":") {
				t.Errorf("%s emits custom-node class %s, which render/comfy-nodes.mjs NODE_PACKS does not list", e.Name(), c)
			}
		}
	}

	// The Wan split's config default is the builder's own value.
	m := regexp.MustCompile(`virtualVramGb = ([0-9.]+)`).FindStringSubmatch(read("wf-wan22-i2v.mjs"))
	if m == nil {
		t.Fatal("no virtualVramGb default in wf-wan22-i2v.mjs")
	}
	if want := config.Default().VideoGenWanVirtualVramGB; m[1] != "7.0" || want != 7 {
		t.Errorf("wf-wan22-i2v.mjs virtualVramGb default %s vs config default videogen_wan_virtual_vram_gb %g: keep them equal", m[1], want)
	}
}
