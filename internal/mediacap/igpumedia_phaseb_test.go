package mediacap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// CT-49 phase B: mediacap and the pipeline must give the same verdict for an sdcpp family
// named like a ComfyUI family (G2, G24, G32), doctor must read the binding the pipeline renders
// with (G41), the untested route-verdict guards (G36) and the byte-identity golden (G40).

func sdcppBound(t *testing.T, exeDir string) config.VideoFamilyBinding {
	t.Helper()
	return config.VideoFamilyBinding{
		Engine: config.EngineSdcpp, SdcppBackend: "vulkan0",
		SdcppBin: touch(t, exeDir, "sd-cli"), SdcppModel: touch(t, exeDir, "m/wan.gguf"),
		SdcppVAE: touch(t, exeDir, "m/vae.safetensors"), SdcppT5xxl: touch(t, exeDir, "m/t5.gguf"),
	}
}

func rowsFor(cfg config.Config, family string) []VideoFamilyBindingRow {
	var out []VideoFamilyBindingRow
	for _, r := range VideoFamilyBindingRows(cfg) {
		if r.Family == family {
			out = append(out, r)
		}
	}
	return out
}

// An sdcpp family named wan22 (the natural name) is the default generate_video when
// videogen_family is unset, "wan22" or spelled "wan" - exactly what the pipeline does - and is
// listed ONCE (no bogus ComfyUI builder-default row, no duplicate generate_video:wan22 route).
func TestSdcppFamilyNamedLikeAComfyFamilyIsTheDefaultRouteAndOneRow(t *testing.T) {
	for _, def := range []string{"", "wan22", "wan"} {
		cfg, exeDir := igpuBox(t)
		cfg.VideoGenSdcppScript = "render/sdcpp-video.mjs"
		cfg.VideoGenFamily = def
		cfg.VideoGenFamilies = map[string]config.VideoFamilyBinding{"wan22": sdcppBound(t, exeDir)}
		got := byName(routesIn(cfg, exeDir))
		r := got["generate_video"]
		if r.Engine != "sdcpp" || r.State != Configured || !strings.Contains(r.Detail, "family=wan22") {
			t.Errorf("videogen_family %q: generate_video = %+v, want the sdcpp route CONFIGURED", def, r)
		}
		if _, dup := got["generate_video:wan22"]; dup {
			t.Errorf("videogen_family %q: the default family must not also be listed as generate_video:wan22", def)
		}
		rows := rowsFor(cfg, "wan22")
		if len(rows) != 1 || rows[0].Files["sdcpp_model"] == "" || !rows[0].Default {
			t.Errorf("videogen_family %q: want exactly one default wan22 row with the sdcpp files, got %+v", def, rows)
		}
		for label := range rows[0].Files {
			if strings.Contains(label, "builder default") {
				t.Errorf("videogen_family %q: a ComfyUI builder-default label leaked into the sdcpp row: %q", def, label)
			}
		}
	}
}

// With a ComfyUI default on the box, an sdcpp family named wan22 never hijacks generate_video:
// the pipeline resolves "" -> ltx25 -> comfy, and the verdict says the same, with the sdcpp
// family as its own generate_video:wan22 route.
func TestSdcppFamilyNamedLikeAComfyFamilyDoesNotHijackAComfyDefault(t *testing.T) {
	cfg, exeDir := igpuBox(t)
	cfg.VideoGenSdcppScript = "render/sdcpp-video.mjs"
	cfg.VideoGenFamily = "ltx25"
	cfg.VideoGenScript = "render/comfy-video.mjs"
	touch(t, exeDir, "render/comfy-video.mjs")
	cfg.VideoGenFamilies = map[string]config.VideoFamilyBinding{"wan22": sdcppBound(t, exeDir)}
	got := byName(routesIn(cfg, exeDir))
	if r := got["generate_video"]; r.Engine != "comfyui" || !strings.Contains(r.Detail, "family=ltx25") {
		t.Errorf("generate_video must stay the ComfyUI ltx25 route: %+v", r)
	}
	if r := got["generate_video:wan22"]; r.Engine != "sdcpp" || r.State != Configured {
		t.Errorf("generate_video:wan22 = %+v, want the sdcpp route", r)
	}
	if rows := rowsFor(cfg, "wan22"); len(rows) != 1 || rows[0].Default || rows[0].Files["sdcpp_model"] == "" {
		t.Errorf("wan22 must be one non-default sdcpp row, got %+v", rows)
	}
	// every other ComfyUI row is untouched
	if len(rowsFor(cfg, "ltx25")) != 1 || len(rowsFor(cfg, "hunyuan")) != 1 || len(rowsFor(cfg, "h3")) != 1 {
		t.Error("the other families keep exactly one row each")
	}
}

// The same for each ComfyUI-known name: an sdcpp ltx25 / hunyuan / h3 is the default exactly
// when videogen_family names it.
func TestEveryComfyKnownNameAsAnSdcppFamily(t *testing.T) {
	for _, name := range []string{"ltx25", "hunyuan", "h3"} {
		cfg, exeDir := igpuBox(t)
		cfg.VideoGenSdcppScript = "render/sdcpp-video.mjs"
		cfg.VideoGenFamilies = map[string]config.VideoFamilyBinding{name: sdcppBound(t, exeDir)}
		// not the default: its own route, and the default stays ComfyUI wan22
		got := byName(routesIn(cfg, exeDir))
		if got["generate_video"].Engine != "comfyui" || got["generate_video:"+name].Engine != "sdcpp" {
			t.Errorf("%s not the default: generate_video=%+v own=%+v", name, got["generate_video"], got["generate_video:"+name])
		}
		cfg.VideoGenFamily = name
		got = byName(routesIn(cfg, exeDir))
		if got["generate_video"].Engine != "sdcpp" || got["generate_video"].State != Configured {
			t.Errorf("%s as the default: generate_video = %+v", name, got["generate_video"])
		}
		if _, dup := got["generate_video:"+name]; dup {
			t.Errorf("%s as the default must not be listed twice", name)
		}
		if rows := rowsFor(cfg, name); len(rows) != 1 || rows[0].Files["sdcpp_bin"] == "" {
			t.Errorf("%s: want one sdcpp row, got %+v", name, rows)
		}
	}
}

// ---- G41: doctor reads the binding the pipeline renders with ------------------------------

// pipelineRenderFamily restates internal/pipeline.resolveVideoFamily for a request that names
// no model: "" when videogen_family is unset, else the runner's canonical family.
func pipelineRenderFamily(cfg config.Config) string {
	fam := strings.TrimSpace(cfg.VideoGenFamily)
	if fam == "" {
		return ""
	}
	return videoRunnerFamily(fam)
}

func needByLabelSuffix(files []needFile, suffix string) (string, bool) {
	for _, f := range files {
		if strings.HasSuffix(f.label, suffix) || strings.Contains(f.label, suffix+" ") {
			return f.name, true
		}
	}
	return "", false
}

// The configuration G41 describes: the box's default family is ltx25, its own entry binds a
// gemma4 text encoder and the flat videogen_text_encoder is umt5. The pipeline resolves
// ResolveVideoFamilyBinding("ltx25"), and doctor must check exactly the file that binding names.
func TestVideoNeedsChecksTheBindingThePipelineRendersWith(t *testing.T) {
	const flat, fam = "umt5_xxl_flat.safetensors", "gemma4-family-bound.safetensors"
	mk := func(def string) config.Config {
		c := bare()
		c.VideoGenFamily = def
		c.VideoGenTextEncoder = flat
		c.VideoGenFamilies = map[string]config.VideoFamilyBinding{
			"ltx25":   {TextEncoder: fam},
			"wan22":   {TextEncoder: fam, UnetHigh: "family-high.gguf", UnetLow: "family-low.gguf"},
			"hunyuan": {TextEncoder: fam},
		}
		return c
	}
	for _, def := range []string{"", "wan22", "wan", "ltx25", "hunyuan", "foo"} {
		cfg := mk(def)
		family, files, _, _ := videoNeeds(cfg)
		want := cfg.ResolveVideoFamilyBinding(pipelineRenderFamily(cfg)).TextEncoder
		got, ok := needByLabelSuffix(files, "videogen_text_encoder")
		if !ok {
			t.Fatalf("videogen_family %q (family %s): no text-encoder need in %+v", def, family, files)
		}
		if got != want {
			t.Errorf("videogen_family %q: doctor checks text encoder %q but the pipeline renders with %q", def, got, want)
		}
	}
	// the exact described box: default ltx25 -> the binding is the flat key for the default family
	// (an entry under the default family's own name is ignored for weights by contract), and doctor
	// agrees with it rather than inventing a second answer
	cfg := mk("ltx25")
	_, files, _, _ := videoNeeds(cfg)
	if got, _ := needByLabelSuffix(files, "videogen_text_encoder"); got != cfg.ResolveVideoFamilyBinding("ltx25").TextEncoder {
		t.Errorf("default ltx25: doctor %q vs binding %q", got, cfg.ResolveVideoFamilyBinding("ltx25").TextEncoder)
	}
}

// videogen_family "wan" (the runner's own word for Wan 2.2) renders through
// videogen_families["wan22"]: its experts and text encoder are what doctor must check, and its
// wan_loader / pool keys decide the node classes.
func TestVideoNeedsFollowsTheWan22EntryWhenVideoGenFamilyIsSpelledWan(t *testing.T) {
	cfg := bare()
	cfg.VideoGenFamily = "wan"
	cfg.VideoGenUnetHigh, cfg.VideoGenUnetLow = "flat-high.gguf", "flat-low.gguf"
	cfg.VideoGenFamilies = map[string]config.VideoFamilyBinding{
		"wan22": {UnetHigh: "bound-high.safetensors", UnetLow: "bound-low.safetensors", TextEncoder: "bound-te.safetensors", WanLoader: "native"},
	}
	family, files, classes, _ := videoNeeds(cfg)
	if family != "wan22" {
		t.Fatalf("family = %q", family)
	}
	names := map[string]string{}
	for _, f := range files {
		names[f.label] = f.name
	}
	if names["videogen_unet_high"] != "bound-high.safetensors" || names["videogen_unet_low"] != "bound-low.safetensors" || names["videogen_text_encoder"] != "bound-te.safetensors" {
		t.Errorf("doctor must check the wan22 entry's files, got %v", names)
	}
	for _, c := range classes {
		if strings.Contains(c, "DisTorch2") {
			t.Errorf("native safetensors experts need no DisTorch class, got %v", classes)
		}
	}
}

// ---- G36: the route-verdict guards ----------------------------------------------------------

func boundEverything(t *testing.T) (config.Config, string) {
	t.Helper()
	cfg, exeDir := igpuBox(t)
	bindVideo(t, &cfg, exeDir)
	cfg.AnimateGenEngine = config.EngineSdcpp
	cfg.AnimateGenSdcppBackend = "vulkan0"
	cfg.AnimateGenSdcppBin, cfg.AnimateGenDepthBin = touch(t, exeDir, "sd-cli2"), touch(t, exeDir, "da3-cli")
	cfg.AnimateGenSdcppModel, cfg.AnimateGenSdcppVAE = touch(t, exeDir, "m/vace.safetensors"), touch(t, exeDir, "m/v2.safetensors")
	cfg.AnimateGenSdcppT5xxl, cfg.AnimateGenDepthModel = touch(t, exeDir, "m/t52.gguf"), touch(t, exeDir, "m/depth.gguf")
	cfg.VoiceGenEngine, cfg.MusicGenEngine = config.EngineAudiocpp, config.EngineAudiocpp
	cfg.AudiocppBackend = "vulkan"
	cfg.AudiocppBin = touch(t, exeDir, "audiocpp_cli")
	cfg.AudiocppVoiceModel, cfg.AudiocppMusicModel = touch(t, exeDir, "m/cb.gguf"), touch(t, exeDir, "m/ace.gguf")
	return cfg, exeDir
}

var igpuRouteNames = []string{"generate_video", "animate_character", "generate_audio:voice", "generate_audio:music"}

func TestAMissingFfmpegPathFlipsEveryIGPURouteToBoundButMissing(t *testing.T) {
	cfg, exeDir := boundEverything(t)
	// a present ffmpeg with its ffprobe beside it: everything CONFIGURED
	bin := filepath.Join(exeDir, "ffbin")
	cfg.FFmpegPath = touch(t, bin, "ffmpeg"+exeExt())
	touch(t, bin, "ffprobe"+exeExt())
	got := byName(routesIn(cfg, exeDir))
	for _, n := range igpuRouteNames {
		if got[n].State != Configured {
			t.Errorf("%s with ffmpeg and ffprobe present: %+v", n, got[n])
		}
	}
	// ffmpeg_path configured but absent
	cfg.FFmpegPath = filepath.Join(bin, "gone-ffmpeg"+exeExt())
	got = byName(routesIn(cfg, exeDir))
	for _, n := range igpuRouteNames {
		if got[n].State != BoundButMissing || !strings.Contains(got[n].Detail, "ffmpeg_path") {
			t.Errorf("%s with a missing ffmpeg_path: %+v", n, got[n])
		}
	}
}

func TestAMissingFfprobeFlipsEveryIGPURouteToBoundButMissing(t *testing.T) {
	cfg, exeDir := boundEverything(t)
	bin := filepath.Join(exeDir, "ffbin")
	cfg.FFmpegPath = touch(t, bin, "ffmpeg"+exeExt())
	t.Setenv("PATH", t.TempDir()) // nothing named ffprobe anywhere
	got := byName(routesIn(cfg, exeDir))
	for _, n := range igpuRouteNames {
		if got[n].State != BoundButMissing || !strings.Contains(got[n].Detail, "ffprobe") {
			t.Errorf("%s with ffmpeg but no ffprobe: %+v", n, got[n])
		}
	}
}

func exeExt() string {
	if os.PathSeparator == '\\' {
		return ".exe"
	}
	return ""
}

// A stale deploy that lacks a runner script must not read CONFIGURED: delete each in turn.
func TestAMissingRunnerScriptIsBoundButMissing(t *testing.T) {
	for _, tc := range []struct {
		script string
		routes []string
		key    string
	}{
		{"sdcpp-video", []string{"generate_video"}, "videogen_sdcpp_script"},
		{"sdcpp-animate", []string{"animate_character"}, "animategen_sdcpp_script"},
		{"audiocpp-generate", []string{"generate_audio:voice", "generate_audio:music"}, "audiocpp_script"},
	} {
		cfg, exeDir := boundEverything(t)
		got := byName(routesIn(cfg, exeDir))
		for _, n := range igpuRouteNames {
			if got[n].State != Configured {
				t.Fatalf("fixture: %s = %+v", n, got[n])
			}
		}
		if err := os.Remove(filepath.Join(exeDir, "render", tc.script+".mjs")); err != nil {
			t.Fatal(err)
		}
		got = byName(routesIn(cfg, exeDir))
		hit := map[string]bool{}
		for _, n := range tc.routes {
			hit[n] = true
			if got[n].State != BoundButMissing || !strings.Contains(got[n].Detail, tc.key) {
				t.Errorf("without %s.mjs: %s = %+v, want BOUND-BUT-MISSING naming %s", tc.script, n, got[n], tc.key)
			}
		}
		for _, n := range igpuRouteNames {
			if !hit[n] && got[n].State != Configured {
				t.Errorf("without %s.mjs the unrelated route %s must stay CONFIGURED: %+v", tc.script, n, got[n])
			}
		}
	}
}

// The voice and music engines are independent: a box that sets only one keeps the other lane's
// ComfyUI / python verdict.
func TestVoiceAndMusicEnginesAreIndependent(t *testing.T) {
	voiceOnly, exeDir := igpuBox(t)
	voiceOnly.VoiceGenEngine = config.EngineAudiocpp
	voiceOnly.AudiocppBackend = "vulkan"
	voiceOnly.AudiocppBin = touch(t, exeDir, "audiocpp_cli")
	voiceOnly.AudiocppVoiceModel = touch(t, exeDir, "m/cb.gguf")
	got := byName(routesIn(voiceOnly, exeDir))
	if got["generate_audio:voice"].Engine != "audiocpp" || got["generate_audio:voice"].State != Configured {
		t.Errorf("voice: %+v", got["generate_audio:voice"])
	}
	if m := got["generate_audio:music"]; m.Engine != "acestep" || m.State != NotConfigured {
		t.Errorf("a voice-only audiocpp box must keep the ComfyUI music verdict, got %+v", m)
	}

	musicOnly, exeDir2 := igpuBox(t)
	musicOnly.MusicGenEngine = config.EngineAudiocpp
	musicOnly.AudiocppBackend = "vulkan"
	musicOnly.AudiocppBin = touch(t, exeDir2, "audiocpp_cli")
	musicOnly.AudiocppMusicModel = touch(t, exeDir2, "m/ace.gguf")
	got = byName(routesIn(musicOnly, exeDir2))
	if got["generate_audio:music"].Engine != "audiocpp" || got["generate_audio:music"].State != Configured {
		t.Errorf("music: %+v", got["generate_audio:music"])
	}
	if v := got["generate_audio:voice"]; v.Engine != "chatterbox-tts" || v.State != NotConfigured {
		t.Errorf("a music-only audiocpp box must keep the python voice verdict, got %+v", v)
	}
}

// audio.cpp's --backend takes vulkan, not sd.cpp's vulkan0 (G13): the route says so.
func TestAudiocppBackendUsesAudioCppsOwnValues(t *testing.T) {
	cfg, exeDir := boundEverything(t)
	cfg.AudiocppBackend = "vulkan0"
	got := byName(routesIn(cfg, exeDir))
	for _, n := range []string{"generate_audio:voice", "generate_audio:music"} {
		if got[n].State != BoundButMissing || !strings.Contains(got[n].Detail, "audio.cpp") {
			t.Errorf("%s with audiocpp_backend vulkan0: %+v", n, got[n])
		}
	}
	// sd.cpp keeps vulkan0
	if got["animate_character"].State != Configured {
		t.Errorf("sd.cpp's vulkan0 must stay valid: %+v", got["animate_character"])
	}
}

// ---- TAE and the VACE model note ------------------------------------------------------------

func TestTAEIsOptionalAndNeverFailsTheRoute(t *testing.T) {
	cfg, exeDir := boundEverything(t)
	fb := cfg.VideoGenFamilies["fastwan"]
	setTAE := func(video, animate string) config.Config {
		c := cfg
		b := fb
		b.SdcppTAE = video
		c.VideoGenFamilies = map[string]config.VideoFamilyBinding{"fastwan": b}
		c.AnimateGenSdcppTAE = animate
		return c
	}
	present := touch(t, exeDir, "m/taew2_2.safetensors")
	got := byName(routesIn(setTAE(present, present), exeDir))
	for _, n := range []string{"generate_video", "animate_character"} {
		if got[n].State != Configured || !strings.Contains(got[n].Detail, "optional") || !strings.Contains(got[n].Detail, "taew2_2") || strings.Contains(got[n].Detail, "MISSING") {
			t.Errorf("%s with a present TAE: %+v", n, got[n])
		}
	}
	gone := filepath.Join(exeDir, "m", "gone-tae.safetensors")
	got = byName(routesIn(setTAE(gone, gone), exeDir))
	for _, n := range []string{"generate_video", "animate_character"} {
		if got[n].State != Configured || !strings.Contains(got[n].Detail, "MISSING") || !strings.Contains(got[n].Detail, "full VAE") {
			t.Errorf("%s with an absent TAE must stay CONFIGURED and name it: %+v", n, got[n])
		}
	}
	got = byName(routesIn(setTAE("", ""), exeDir))
	for _, n := range []string{"generate_video", "animate_character"} {
		if got[n].State != Configured || strings.Contains(got[n].Detail, "tae") {
			t.Errorf("%s with no TAE bound must say nothing about it: %+v", n, got[n])
		}
	}
}

func TestAVACEGGUFIsNotedAndASafetensorsIsNot(t *testing.T) {
	cfg, exeDir := boundEverything(t)
	if r := byName(routesIn(cfg, exeDir))["animate_character"]; r.State != Configured || strings.Contains(r.Detail, "NOTE") {
		t.Errorf("a .safetensors VACE model: %+v", r)
	}
	cfg.AnimateGenSdcppModel = touch(t, exeDir, "m/vace-q8_0.gguf")
	r := byName(routesIn(cfg, exeDir))["animate_character"]
	if r.State != Configured || !strings.Contains(r.Detail, "vace_patch_embedding.weight") {
		t.Errorf("a .gguf VACE model stays CONFIGURED but is noted: %+v", r)
	}
}

// ---- G40: a box with no engine key is byte-for-byte what it was -----------------------------

// goldenRoutes is routesIn(config.Default()) as eaaeb849 (the base before any iGPU engine key
// existed) produced it, with the machine-dependent keys blanked and path separators normalised.
const goldenRoutes = `generate_image | comfyui | NOT CONFIGURED | imagegen_script is unset
inpaint_image | comfyui | NOT CONFIGURED | inpaint_script/inpaint_ckpt is unset
edit_image_generative | comfyui | NOT CONFIGURED | gen_edit_script/gen_edit_unet is unset
upscale_image | comfyui | NOT CONFIGURED | upscale_script/upscale_model is unset (videogen_upscale_model is the fallback)
generate_video | comfyui | BOUND-BUT-MISSING | videogen_script=render/comfy-video.mjs: script not found at <exe>/render/comfy-video.mjs
animate_character | comfyui | BOUND-BUT-MISSING | animategen_script=render/comfy-animate.mjs: script not found at <exe>/render/comfy-animate.mjs
generate_audio:voice | chatterbox-tts | BOUND-BUT-MISSING | voicegen_script=render/tts.mjs: script not found at <exe>/render/tts.mjs
generate_audio:music | acestep | BOUND-BUT-MISSING | musicgen_script=render/comfy-music.mjs: script not found at <exe>/render/comfy-music.mjs
run_graph | comfyui | BOUND-BUT-MISSING | run_graph_script=render/comfy-run-graph.mjs: script not found at <exe>/render/comfy-run-graph.mjs
generate_audio:voice:endpoint | openai-compatible-tts | NOT CONFIGURED | tts_endpoint is unset
edit_image | pil | NOT CONFIGURED | edit_python unset and no <comfy_dir>/.venv python found
flatten_design | gimp | NOT CONFIGURED | gimp_console_path is unset
media | ffmpeg | NOT CONFIGURED | ffmpeg_path is unset
media:ffprobe | ffprobe | NOT CONFIGURED | ffmpeg_path is unset
compose_video | hyperframes | NOT CONFIGURED | compose_script is unset (the installer's hyperframes step binds it when node >= 22)
node | runtime | NOT CONFIGURED | node_path is unset — every render script runs under it
comfyui | runtime | NOT CONFIGURED | comfy_dir is unset — every ComfyUI-backed route above will fail
`

func TestNoEngineKeyLeavesTheFullRouteListByteIdentical(t *testing.T) {
	exeDir := t.TempDir()
	cfg := config.Default()
	cfg.NodePath, cfg.FFmpegPath, cfg.GimpConsolePath, cfg.EditPython, cfg.ComfyDir = "", "", "", "", ""
	var b strings.Builder
	for _, r := range routesIn(cfg, exeDir) {
		d := strings.ReplaceAll(strings.ReplaceAll(r.Detail, exeDir, "<exe>"), "\\", "/")
		fmt.Fprintf(&b, "%s | %s | %s | %s\n", r.Name, r.Engine, r.State, d)
	}
	if got := b.String(); got != goldenRoutes {
		t.Errorf("the route list changed for a box with no iGPU engine key:\n--- got\n%s--- want\n%s", got, goldenRoutes)
	}
}

// ---- TST18: each family entry is read for ITS family --------------------------------------------

// The G41 tests above give every family entry the same text encoder, so they cannot tell WHICH entry
// doctor reads. Here each entry names a different file: doctor must check the file of the family the
// pipeline renders (and the runner's closed family set must map ltx25 / h3 / hunyuan to themselves).
func TestVideoNeedsMapsEachRunnerFamilyToItsOwnEntryAndFiles(t *testing.T) {
	for _, tc := range []struct {
		videogenFamily, wantFamily, wantLabel string
	}{
		{"ltx25", "ltx25", "videogen_text_encoder"},
		{"h3", "h3", "h3 text encoder (builder default)"},
		{"hunyuan", "hunyuan", "videogen_text_encoder"},
		{"wan22", "wan22", "videogen_text_encoder"},
		{"wan", "wan22", "videogen_text_encoder"},
		{"", "wan22", "videogen_text_encoder"},
	} {
		cfg := bare()
		cfg.VideoGenFamily = tc.videogenFamily
		cfg.VideoGenTextEncoder = "flat-te.safetensors"
		cfg.VideoGenFamilies = map[string]config.VideoFamilyBinding{
			"ltx25": {TextEncoder: "ltx-te.safetensors"}, "hunyuan": {TextEncoder: "hunyuan-te.safetensors"},
			"wan22": {TextEncoder: "wan-te.safetensors"}, "h3": {TextEncoder: "h3-te.safetensors"},
		}
		family, files, _, _ := videoNeeds(cfg)
		if family != tc.wantFamily {
			t.Errorf("videogen_family %q: runner family = %q, want %q", tc.videogenFamily, family, tc.wantFamily)
			continue
		}
		found := false
		for _, f := range files {
			if f.label == tc.wantLabel {
				found = true
				// h3 binds no weights (builder defaults only); the others read the binding the pipeline resolves
				if want := cfg.ResolveVideoFamilyBinding(pipelineRenderFamily(cfg)).TextEncoder; tc.wantFamily != "h3" && f.name != want {
					t.Errorf("videogen_family %q: doctor checks %q, the pipeline renders with %q", tc.videogenFamily, f.name, want)
				}
			}
		}
		if !found {
			t.Errorf("videogen_family %q: no %q need in %+v", tc.videogenFamily, tc.wantLabel, files)
		}
	}
}

// videogen_wan_loader decides the node classes: the loader of the entry the pipeline renders with
// (the wan22 entry for videogen_family "wan"), not the flat key.
func TestVideoNeedsReadsTheWanLoaderOfTheBoundEntry(t *testing.T) {
	mk := func(flat, entry string) config.Config {
		cfg := bare()
		cfg.VideoGenFamily = "wan"
		cfg.VideoGenWanLoader = flat
		cfg.VideoGenFamilies = map[string]config.VideoFamilyBinding{
			"wan22": {UnetHigh: "high.safetensors", UnetLow: "low.safetensors", WanLoader: entry},
		}
		return cfg
	}
	hasDisTorch := func(classes []string) bool {
		for _, c := range classes {
			if strings.Contains(c, "DisTorch2") {
				return true
			}
		}
		return false
	}
	// the entry says native, the flat key says distorch: native wins for the family that renders
	if _, _, classes, _ := videoNeeds(mk("gguf-distorch", "native")); hasDisTorch(classes) {
		t.Errorf("the wan22 entry binds the native loader: no DisTorch class expected, got %v", classes)
	}
	// the entry says distorch, the flat key says native: distorch wins
	if _, _, classes, _ := videoNeeds(mk("native", "gguf-distorch")); !hasDisTorch(classes) {
		t.Errorf("the wan22 entry binds gguf-distorch: a DisTorch class is expected, got %v", classes)
	}
}

// SIL13: audio.cpp's other GPU backends have no evidence pattern in the runner's log guard, so a box
// bound to one would read CONFIGURED and then end every call CPU_PLACEMENT. The route is BOUND-BUT-
// MISSING for them, naming the key, exactly as for a cpu backend.
func TestAudiocppRouteRefusesABackendTheEvidenceGuardCannotRead(t *testing.T) {
	for _, backend := range []string{"cuda", "hip", "rocm", "metal", "vulkan0", "cpu", ""} {
		cfg, exeDir := boundEverything(t)
		cfg.AudiocppBackend = backend
		got := byName(routesIn(cfg, exeDir))
		for _, name := range []string{"generate_audio:voice", "generate_audio:music"} {
			r := got[name]
			if r.State != BoundButMissing || !strings.Contains(r.Detail, "audiocpp_backend") {
				t.Errorf("audiocpp_backend %q, %s: state %v detail %q, want BOUND-BUT-MISSING naming audiocpp_backend", backend, name, r.State, r.Detail)
			}
		}
	}
	cfg, exeDir := boundEverything(t)
	cfg.AudiocppBackend = "vulkan"
	if r := byName(routesIn(cfg, exeDir))["generate_audio:music"]; r.State != Configured {
		t.Errorf("vulkan is the one allowed audio.cpp backend, got %v %q", r.State, r.Detail)
	}
}
