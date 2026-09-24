package mediacap

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpugen"
)

// What a route needs beyond its script (OptiPlex parity audit, 2026-09-23).
//
// generate_video, animate_character and both generate_audio kinds were CONFIGURED as
// soon as their render script existed. On the 8 GB reference box that meant three green
// rows over three routes that fail when called: voice had no TTS venv, no chatterbox
// package and no weights; animate had none of its model files; the Wan video graph ended
// in VHS_VideoCombine, a custom-node class the box did not have. A route is CONFIGURED
// here only when the things it loads are on this machine too:
//
//   - the model files its graph loads — the ones the config binds AND the builder
//     defaults it falls back to (render/wf-*.mjs; TestRouteNeedsMirrorTheRenderBuilders
//     parses those files and fails when the two drift),
//   - the custom-node classes its graph names (core ComfyUI classes need nothing), and
//   - for voice, the python the runner will spawn, its chatterbox + torch packages, and
//     the Chatterbox weights in the Hugging Face cache.
//
// Everything is a filesystem read, except that `doctor` asks a ComfyUI that is already
// running for its /object_info (LiveNodeChecker). Nothing starts ComfyUI, loads a model,
// imports a package or touches the GPU.

// Class directories the loader nodes open (see expectedClasses for the derivation).
var (
	classDiffusion = []string{"diffusion_models", "unet"}
	classTextEnc   = []string{"text_encoders", "clip"}
	classVAE       = []string{"vae"}
	classLoRA      = []string{"loras"}
	classClipVis   = []string{"clip_vision"}
	classLatentUp  = []string{"latent_upscale_models", "upscale_models"}
)

// needFile is one model file a route's graph loads: label says where the name came from
// (a config key, or "<builder> default"), classes where the loader looks for it.
type needFile struct {
	label   string
	classes []string
	name    string
}

// Builder defaults, mirrored from the render/wf-*.mjs signatures. A binding left unset
// renders with these, so an unset key is not "nothing to check" — it is this file.
var (
	wanDefaults = struct{ highUnet, lowUnet, highLora, lowLora, textEncoder, vae string }{
		highUnet:    "wan2.2_i2v_high_noise_14B_Q4_K_S.gguf",
		lowUnet:     "wan2.2_i2v_low_noise_14B_Q4_K_M.gguf",
		highLora:    "Wan_2_2_I2V_A14B_HIGH_lightx2v_4step_lora_260412_rank_64_fp16.safetensors",
		lowLora:     "Wan_2_2_I2V_A14B_LOW_lightx2v_4step_lora_260412_rank_64_fp16.safetensors",
		textEncoder: "umt5_xxl_fp8_e4m3fn_scaled.safetensors",
		vae:         "wan_2.1_vae.safetensors",
	}
	ltxDefaults = struct{ transformer, textEncoder, videoVae, audioVae, latentUpscaler string }{
		transformer:    "ltx-2.5-22b-distilled-transformer-comfy-int8-convrot.safetensors",
		textEncoder:    "gemma4-12b-with-proj-ltx-2.5-comfy-int8-convrot.safetensors",
		videoVae:       "ltx-2.5-video-vae-conv-bf16.safetensors",
		audioVae:       "ltx-2.5-audio-vae-bf16.safetensors",
		latentUpscaler: "ltx-2.5-latent-spatial-upscaler-x2-bf16-1.0.safetensors",
	}
	h3Defaults = struct{ transformer, textEncoder, videoVae, audioVae, turboLora string }{
		transformer: "minimax_h3_fl2va_pruned_int8_convrot.safetensors",
		textEncoder: "qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors",
		videoVae:    "minimax_h3_video_vae_fp16.safetensors",
		audioVae:    "minimax_h3_audio_vae_fp32.safetensors",
		turboLora:   "minimax_h3_fl2v_turbo_8step_v1.0_comfyui_bf16.safetensors",
	}
	hunyuanDefaults = struct{ unet, vae, clipVision, textEncoder, glyphEncoder string }{
		unet:         "hunyuanvideo1.5_480p_i2v_cfg_distilled-Q4_K_S.gguf",
		vae:          "hunyuanvideo15_vae_fp16.safetensors",
		clipVision:   "sigclip_vision_patch14_384.safetensors",
		textEncoder:  "qwen_2.5_vl_7b_fp8_scaled.safetensors",
		glyphEncoder: "byt5_small_glyphxl_fp16.safetensors",
	}
	animateDefaults = struct{ unet, textEncoder, clipVision, vae string }{
		unet:        "wan_animate_2_distill_int8_convrot.safetensors",
		textEncoder: "umt5_xxl_fp8_e4m3fn_scaled.safetensors",
		clipVision:  "clip_vision_h.safetensors",
		vae:         "wan_2.1_vae.safetensors",
	}
	aceDefaults = struct{ unet, clip1, clip2, vae string }{
		unet:  "acestep_v1.5_xl_turbo_bf16.safetensors",
		clip1: "qwen_0.6b_ace15.safetensors",
		clip2: "qwen_4b_ace15.safetensors",
		vae:   "ace_1.5_vae.safetensors",
	}
)

// bound returns the configured value when set, else the builder default, with the label
// that says which one it is.
func bound(key, value, builder, def string, classes []string) needFile {
	if strings.TrimSpace(value) != "" {
		return needFile{label: key, classes: classes, name: value}
	}
	return needFile{label: key + " (" + builder + " builder default)", classes: classes, name: def}
}

func isGGUF(name string) bool { return strings.HasSuffix(strings.ToLower(name), ".gguf") }

// videoNeeds is what the configured video family's graph loads, mirroring
// render/comfy-video.mjs: the family is videogen_family (ltx25 | h3 | hunyuan, matched
// exactly like the runner), anything else renders Wan 2.2. optional lists files only a
// per-request mode loads (Wan fast=true), reported without failing the route.
func videoNeeds(cfg config.Config) (family string, files []needFile, classes []string, optional []needFile) {
	switch fam := strings.TrimSpace(cfg.VideoGenFamily); fam {
	case "ltx25":
		files = []needFile{
			bound("videogen_transformer", cfg.VideoGenTransformer, "ltx25", ltxDefaults.transformer, classDiffusion),
			bound("videogen_text_encoder", cfg.VideoGenTextEncoder, "ltx25", ltxDefaults.textEncoder, classTextEnc),
			bound("videogen_video_vae", cfg.VideoGenVideoVAE, "ltx25", ltxDefaults.videoVae, classVAE),
			bound("videogen_audio_vae", cfg.VideoGenAudioVAE, "ltx25", ltxDefaults.audioVae, classVAE),
			bound("videogen_latent_upscaler", cfg.VideoGenLatentUpscaler, "ltx25", ltxDefaults.latentUpscaler, classLatentUp),
		}
		if cfg.VideoGenPoolVvramGB > 0 {
			// The text encoder and both VAEs pin off ComfyUI's default device too
			// (2026-09-24 fix, wf-ltx25-i2v.mjs) — same pack as the DiT loader.
			classes = []string{"UNETLoaderDisTorch2MultiGPU", "CLIPLoaderMultiGPU", "VAELoaderMultiGPU"}
		}
		return "ltx25", files, classes, nil
	case "h3":
		// The runner's h3 branch passes no per-machine weight flags: builder defaults only,
		// and the turbo LoRA is the default recipe (hero is the per-request opt-out).
		return "h3", []needFile{
			{label: "h3 transformer (builder default)", classes: classDiffusion, name: h3Defaults.transformer},
			{label: "h3 text encoder (builder default)", classes: classTextEnc, name: h3Defaults.textEncoder},
			{label: "h3 video vae (builder default)", classes: classVAE, name: h3Defaults.videoVae},
			{label: "h3 audio vae (builder default)", classes: classVAE, name: h3Defaults.audioVae},
			{label: "h3 turbo lora (builder default)", classes: classLoRA, name: h3Defaults.turboLora},
		}, nil, nil
	case "hunyuan":
		return "hunyuan", []needFile{
			{label: "hunyuan unet (builder default)", classes: classDiffusion, name: hunyuanDefaults.unet},
			{label: "hunyuan vae (builder default)", classes: classVAE, name: hunyuanDefaults.vae},
			{label: "hunyuan clip vision (builder default)", classes: classClipVis, name: hunyuanDefaults.clipVision},
			bound("videogen_text_encoder", cfg.VideoGenTextEncoder, "hunyuan", hunyuanDefaults.textEncoder, classTextEnc),
			{label: "hunyuan glyph encoder (builder default)", classes: classTextEnc, name: hunyuanDefaults.glyphEncoder},
		}, []string{"UnetLoaderGGUF", "VHS_VideoCombine"}, nil
	}
	high := bound("videogen_unet_high", cfg.VideoGenUnetHigh, "wan22", wanDefaults.highUnet, classDiffusion)
	low := bound("videogen_unet_low", cfg.VideoGenUnetLow, "wan22", wanDefaults.lowUnet, classDiffusion)
	files = []needFile{
		high, low,
		bound("videogen_text_encoder", cfg.VideoGenTextEncoder, "wan22", wanDefaults.textEncoder, classTextEnc),
		{label: "wan vae (builder default)", classes: classVAE, name: wanDefaults.vae},
	}
	// The builder picks each expert's DisTorch2 loader by extension (wf-wan22-i2v.mjs).
	seen := map[string]bool{}
	for _, u := range []string{high.name, low.name} {
		c := "UNETLoaderDisTorch2MultiGPU"
		if isGGUF(u) {
			c = "UnetLoaderGGUFDisTorch2MultiGPU"
		}
		if !seen[c] {
			seen[c] = true
			classes = append(classes, c)
		}
	}
	classes = append(classes, "VHS_VideoCombine")
	optional = []needFile{
		{label: "fast mode high lora (builder default)", classes: classLoRA, name: wanDefaults.highLora},
		{label: "fast mode low lora (builder default)", classes: classLoRA, name: wanDefaults.lowLora},
	}
	return "wan22", files, classes, optional
}

// animateNeeds: WAN-Animate-2's four files (the runner passes the animategen_* keys).
func animateNeeds(cfg config.Config) []needFile {
	return []needFile{
		bound("animategen_unet", cfg.AnimateGenUnet, "wan-animate2", animateDefaults.unet, classDiffusion),
		bound("animategen_text_encoder", cfg.AnimateGenTextEncoder, "wan-animate2", animateDefaults.textEncoder, classTextEnc),
		bound("animategen_clip_vision", cfg.AnimateGenClipVision, "wan-animate2", animateDefaults.clipVision, classClipVis),
		bound("animategen_vae", cfg.AnimateGenVAE, "wan-animate2", animateDefaults.vae, classVAE),
	}
}

// musicNeeds: ACE-Step 1.5's files. No config key binds them; the builder defaults are
// what every music render loads.
func musicNeeds() []needFile {
	return []needFile{
		{label: "acestep unet (builder default)", classes: classDiffusion, name: aceDefaults.unet},
		{label: "acestep text encoder 1 (builder default)", classes: classTextEnc, name: aceDefaults.clip1},
		{label: "acestep text encoder 2 (builder default)", classes: classTextEnc, name: aceDefaults.clip2},
		{label: "acestep vae (builder default)", classes: classVAE, name: aceDefaults.vae},
	}
}

// imageNodeClasses / editNodeClasses: the custom-node classes an image / edit binding's
// graph names (qwen-image and the 2511 edit load a .gguf through ComfyUI-GGUF; a pooled
// krea2 seat loads through ComfyUI-MultiGPU). Every other image graph is core.
func imageNodeClasses(cfg config.Config) []string {
	switch {
	case cfg.ImageGenFamily == "qwen-image" && isGGUF(cfg.ImageGenCkpt):
		return []string{"UnetLoaderGGUF"}
	case cfg.ImageGenFamily == "krea2" && cfg.ImagePooled():
		// The text encoder and VAE pin off ComfyUI's default device too
		// (2026-09-24 fix, wf-krea2.mjs) — same pack as the DiT loader.
		return []string{"UNETLoaderDisTorch2MultiGPU", "CLIPLoaderMultiGPU", "VAELoaderMultiGPU"}
	}
	return nil
}

func editNodeClasses(cfg config.Config) []string {
	if cfg.GenEditFamily != config.FamilyQwenImage21 && isGGUF(cfg.GenEditUnet) {
		return []string{"UnetLoaderGGUF"}
	}
	return nil
}

// ---- custom-node classes ----------------------------------------------------------

// nodePack is a custom-node pack a shipped graph depends on. marker is a class the pack's
// python source names literally, used to recognise the pack under an unexpected
// directory name.
type nodePack struct {
	name, url, marker string
	// note carries an operator-facing addendum to the hint (pin/archival/mirror). Empty
	// for a normally-maintained pack.
	note string
}

// pollockjj/ComfyUI-MultiGPU archives 2026-09-30 (issue #223, no successor endorsed).
// The pinned commit is upstream v2.6.4's last code commit (b51c99a525e9607e43545ee2a8b7694c74a4775a,
// pyproject.toml version "2.6.4") PLUS one local fix already carried by the fleet
// (ed1ffaef7cec1a66f35106c6a4c7a40927c2dc83, "fix(p2p): platform-aware cudart load +
// fail-closed P2P on Windows/WDDM" — the first-stage fix for upstream issue #220's
// libcudart.so-on-Windows crash). Mirrored (private) at dmmdea/ComfyUI-MultiGPU-mirror
// since upstream becomes read-only. See docs/systems/media-generation.md "Archival".
const multiGPUPinnedCommit = "ed1ffaef7cec1a66f35106c6a4c7a40927c2dc83"
const multiGPUMirrorURL = "https://github.com/dmmdea/ComfyUI-MultiGPU-mirror"

var multiGPUArchivalNote = " — ARCHIVED upstream 2026-09-30 (no successor); pinned " + multiGPUPinnedCommit[:12] + ", mirror " + multiGPUMirrorURL

var nodePacks = map[string]nodePack{
	"ComfyUI-VideoHelperSuite": {"ComfyUI-VideoHelperSuite", "https://github.com/Kosinkadink/ComfyUI-VideoHelperSuite", "VHS_VideoCombine", ""},
	"ComfyUI-MultiGPU":         {"ComfyUI-MultiGPU", "https://github.com/pollockjj/ComfyUI-MultiGPU", "UNETLoaderDisTorch2MultiGPU", multiGPUArchivalNote},
	"ComfyUI-GGUF":             {"ComfyUI-GGUF", "https://github.com/city96/ComfyUI-GGUF", "UnetLoaderGGUF", ""},
}

// classPacks maps every non-core class a shipped builder emits to the packs that must
// ALL be installed for ComfyUI to register it (MultiGPU registers its GGUF DisTorch2
// loaders only when ComfyUI-GGUF is present). render/comfy-nodes.mjs carries the same
// table for the runner's MISSING_NODE message.
var classPacks = map[string][]string{
	"VHS_VideoCombine":                {"ComfyUI-VideoHelperSuite"},
	"UNETLoaderDisTorch2MultiGPU":     {"ComfyUI-MultiGPU"},
	"UnetLoaderGGUFDisTorch2MultiGPU": {"ComfyUI-MultiGPU", "ComfyUI-GGUF"},
	"UnetLoaderGGUF":                  {"ComfyUI-GGUF"},
	"CLIPLoaderMultiGPU":              {"ComfyUI-MultiGPU"},
	"VAELoaderMultiGPU":               {"ComfyUI-MultiGPU"},
}

// NodeCheck answers "can this ComfyUI build these node classes?".
type NodeCheck struct {
	Missing []string // classes this install cannot build
	Via     string   // how it was decided: live /object_info, or custom_nodes on disk
}

// NodeChecker decides a set of classes for the ComfyUI install at comfyDir.
type NodeChecker func(comfyDir string, classes []string) NodeCheck

// DiskNodeCheck decides from <comfy_dir>/custom_nodes: a class is present when every pack
// it needs is an active pack directory there (not disabled, has __init__.py) — matched by
// name, or by the pack's marker class in its python source when the directory was
// renamed. A class no pack provides is core ComfyUI. Import errors are invisible from
// disk; the live check sees those.
func DiskNodeCheck(comfyDir string, classes []string) NodeCheck {
	packs := activePackDirs(filepath.Join(comfyDir, "custom_nodes"))
	found := map[string]bool{}
	var missing []string
	for _, c := range classes {
		for _, p := range classPacks[c] {
			if _, done := found[p]; !done {
				found[p] = packInstalled(packs, nodePacks[p])
			}
			if !found[p] {
				missing = append(missing, c)
				break
			}
		}
	}
	return NodeCheck{Missing: missing, Via: "custom_nodes on disk"}
}

// activePackDirs lists the pack directories ComfyUI would import: directories under
// custom_nodes carrying an __init__.py, skipping ComfyUI-Manager's disabled forms (a
// ".disabled" suffix, or the .disabled/ holding directory) and caches.
func activePackDirs(dir string) map[string]string {
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() || strings.HasPrefix(n, ".") || strings.HasPrefix(n, "__") || strings.HasSuffix(strings.ToLower(n), ".disabled") {
			continue
		}
		p := filepath.Join(dir, n)
		if fileExists(filepath.Join(p, "__init__.py")) {
			out[strings.ToLower(n)] = p
		}
	}
	return out
}

// packInstalled: by directory name first (case-insensitive — the Manager's registry
// installs the lowercase name), then by the marker class in a pack's .py files at most
// two levels down (VideoHelperSuite keeps its mappings in videohelpersuite/nodes.py).
func packInstalled(packs map[string]string, p nodePack) bool {
	if _, ok := packs[strings.ToLower(p.name)]; ok {
		return true
	}
	needle := []byte(`"` + p.marker + `"`)
	names := make([]string, 0, len(packs))
	for n := range packs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if pyMentions(packs[n], needle, 2) {
			return true
		}
	}
	return false
}

func pyMentions(dir string, needle []byte, depth int) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() {
			if depth > 1 && !strings.HasPrefix(e.Name(), ".") && !strings.HasPrefix(e.Name(), "__") && pyMentions(p, needle, depth-1) {
				return true
			}
			continue
		}
		if !strings.HasSuffix(e.Name(), ".py") {
			continue
		}
		if fi, err := e.Info(); err != nil || fi.Size() > 4<<20 {
			continue
		}
		if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), string(needle)) {
			return true
		}
	}
	return false
}

// LiveNodeChecker asks a RUNNING ComfyUI (GET /object_info/<class>), which also sees a
// pack that is on disk but failed to import. It never starts one: when api does not
// answer (or answers something that is not the object map), it falls back to
// DiskNodeCheck and says so, for this call and every later one in the process.
func LiveNodeChecker(api string, timeout time.Duration) NodeChecker {
	client := &http.Client{Timeout: timeout}
	api = strings.TrimRight(api, "/")
	down := ""
	return func(comfyDir string, classes []string) NodeCheck {
		if down == "" {
			var missing []string
			for _, c := range classes {
				present, err := objectInfoHas(client, api, c)
				if err != nil {
					down = err.Error()
					break
				}
				if !present {
					missing = append(missing, c)
				}
			}
			if down == "" {
				return NodeCheck{Missing: missing, Via: "live /object_info at " + api}
			}
		}
		d := DiskNodeCheck(comfyDir, classes)
		d.Via = "custom_nodes on disk (ComfyUI not answering at " + api + ")"
		return d
	}
}

func objectInfoHas(client *http.Client, api, class string) (bool, error) {
	resp, err := client.Get(api + "/object_info/" + url.PathEscape(class))
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body); err != nil {
		return false, fmt.Errorf("unreadable /object_info: %v", err)
	}
	_, ok := body[class]
	return ok, nil
}

// packHint names the pack(s) a missing class needs, with where to get them.
func packHint(class string) string {
	var parts []string
	for _, p := range classPacks[class] {
		np := nodePacks[p]
		parts = append(parts, np.name+" ("+np.url+")"+np.note)
	}
	if len(parts) == 0 {
		return "a core ComfyUI class — update ComfyUI"
	}
	return "custom node pack " + strings.Join(parts, " + ")
}

// withNeeds upgrades a script-CONFIGURED ComfyUI route with what its graph loads. Every
// problem is listed (an operator fixes them in one pass), and any one of them makes the
// route BOUND-BUT-MISSING. With no comfy_dir the checks cannot run; the detail says so
// and the comfyui prereq row names the cause.
func withNeeds(r Route, comfyDir string, files []needFile, classes []string, optional []needFile, nodes NodeChecker) Route {
	if r.State != Configured || (len(files) == 0 && len(classes) == 0) {
		return r
	}
	if comfyDir == "" || !isDir(comfyDir) {
		r.Detail += "; models and custom nodes not checked (comfy_dir is not a ComfyUI install on this machine)"
		return r
	}
	var bad, good []string
	if len(classes) > 0 {
		chk := nodes(comfyDir, classes)
		if len(chk.Missing) > 0 {
			ms := make([]string, 0, len(chk.Missing))
			for _, c := range chk.Missing {
				ms = append(ms, c+" ("+packHint(c)+")")
			}
			bad = append(bad, fmt.Sprintf("custom node class MISSING: %s [checked: %s, %s]",
				strings.Join(ms, ", "), chk.Via, filepath.Join(comfyDir, "custom_nodes")))
		} else {
			good = append(good, "nodes "+strings.Join(classes, ",")+" ("+chk.Via+")")
		}
	}
	if roots := ModelRoots(comfyDir); len(roots) == 0 {
		if len(files) > 0 {
			good = append(good, "model names not checked (no ComfyUI models root under comfy_dir)")
		}
	} else {
		var names []string
		for _, f := range files {
			if b := resolveBinding(roots, f.label, f.name, f.classes); b.State != BindingFound {
				bad = append(bad, b.Detail)
			} else {
				names = append(names, f.name)
			}
		}
		if len(names) > 0 {
			good = append(good, "models "+strings.Join(names, ", "))
		}
		// Per-request modes: named when absent, never a route failure (the default
		// recipe does not load them).
		for _, f := range optional {
			if b := resolveBinding(roots, f.label, f.name, f.classes); b.State != BindingFound {
				good = append(good, "not available per request: "+b.Detail)
			}
		}
	}
	if len(bad) > 0 {
		r.State = BoundButMissing
		r.Detail += "; " + strings.Join(bad, "; ")
		return r
	}
	if len(good) > 0 {
		r.Detail += "; " + strings.Join(good, "; ")
	}
	return r
}

// ---- voice: the python worker ------------------------------------------------------

// chatterboxRepo is the Hugging Face repo ChatterboxMultilingualTTS.from_pretrained
// downloads into the hub cache on first use (render/tts_chatterbox.py).
const chatterboxRepo = "models--ResembleAI--chatterbox"

// voiceRoute derives generate_audio:voice: the script (and voicegen_ref when bound), then
// what render/tts.mjs will actually run — the python it resolves (TTS_PY, else
// <repo>/.tts-venv next to render/, else `python` on PATH), the chatterbox and torch
// packages in that python's site-packages, and the Chatterbox weights in the hub cache.
// The python and the cache are resolved from THIS process's environment; a server
// started with a different TTS_PY or HF_HOME resolves its own.
func voiceRoute(cfg config.Config, exeDir string) Route {
	const name, engine = "generate_audio:voice", "chatterbox-tts"
	if cfg.VoiceGenScript == "" {
		return Route{Name: name, Engine: engine, State: NotConfigured, Detail: "voicegen_script is unset"}
	}
	bs := []binding{{key: "voicegen_script", value: cfg.VoiceGenScript, kind: scriptBinding}}
	if cfg.VoiceGenRef != "" {
		bs = append(bs, binding{key: "voicegen_ref", value: cfg.VoiceGenRef, kind: fileBinding})
	}
	r := fileRoute(name, engine, exeDir, bs...)
	if r.State != Configured {
		return r
	}
	script, err := gpugen.ResolveScriptIn(cfg.VoiceGenScript, exeDir)
	if err != nil {
		return r
	}
	py, src := ttsPython(filepath.Dir(script))
	if py == "" {
		r.State = BoundButMissing
		r.Detail += "; no TTS python: TTS_PY is unset, there is no " + filepath.Join(filepath.Dir(filepath.Dir(script)), ".tts-venv") +
			", and no python on PATH — build the .tts-venv (render/README.md, Voice setup)"
		return r
	}
	var bad []string
	sites := sitePackages(py)
	if len(sites) == 0 {
		r.Detail += "; tts python=" + py + " (" + src + "), packages not verified: no site-packages found next to it"
	} else {
		for _, pkg := range [][2]string{{"chatterbox", "mtl_tts.py"}, {"torch", "__init__.py"}} {
			if !anyHas(sites, filepath.Join(pkg[0], pkg[1])) {
				bad = append(bad, fmt.Sprintf("tts python %s (%s) has no %s package (looked in %s) — build the .tts-venv (render/README.md, Voice setup)",
					py, src, pkg[0], strings.Join(sites, ", ")))
			}
		}
		if len(bad) == 0 {
			r.Detail += "; tts python=" + py + " (" + src + ") with chatterbox + torch"
		}
	}
	hub := hfHubCache()
	if hasSnapshot(filepath.Join(hub, chatterboxRepo)) {
		r.Detail += "; weights cached in " + filepath.Join(hub, chatterboxRepo)
	} else {
		bad = append(bad, "Chatterbox weights are not in the Hugging Face cache ("+filepath.Join(hub, chatterboxRepo)+
			") — the first call would download them inside the render's timeout; pre-fetch them with the tts python (HF_HOME decides where)")
	}
	if len(bad) > 0 {
		r.State = BoundButMissing
		r.Detail += "; " + strings.Join(bad, "; ")
	}
	return r
}

// ttsPython mirrors render/tts.mjs: TTS_PY, else the .tts-venv at the repo root (one
// level above render/), else `python` on PATH.
func ttsPython(renderDir string) (py, src string) {
	if v := os.Getenv("TTS_PY"); v != "" {
		if p, ok := binaryPresent(v); ok {
			return p, "TTS_PY"
		}
		return "", ""
	}
	venv := filepath.Join(filepath.Dir(renderDir), ".tts-venv")
	for _, p := range []string{filepath.Join(venv, "Scripts", "python.exe"), filepath.Join(venv, "bin", "python")} {
		if fileExists(p) {
			return p, ".tts-venv"
		}
	}
	if p, ok := binaryPresent("python"); ok {
		return p, "python on PATH — the fallback, not a TTS venv"
	}
	return "", ""
}

// sitePackages lists the site-packages directories that exist for a python executable:
// a venv or Windows install (<dir>/Lib/site-packages, <dir>/../Lib/site-packages), a
// POSIX layout (<dir>/../lib/python3*/site-packages|dist-packages), plus the user site
// of a non-venv python.
func sitePackages(py string) []string {
	dir := filepath.Dir(py)
	up := filepath.Dir(dir)
	patterns := []string{
		filepath.Join(dir, "Lib", "site-packages"),
		filepath.Join(up, "Lib", "site-packages"),
		filepath.Join(up, "lib", "python3*", "site-packages"),
		filepath.Join(up, "lib", "python3*", "dist-packages"),
		filepath.Join(up, "lib", "python3", "dist-packages"),
		filepath.Join(up, "local", "lib", "python3*", "dist-packages"),
	}
	if !fileExists(filepath.Join(up, "pyvenv.cfg")) {
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			patterns = append(patterns, filepath.Join(appdata, "Python", "Python3*", "site-packages"))
		}
		if home, err := os.UserHomeDir(); err == nil {
			patterns = append(patterns, filepath.Join(home, ".local", "lib", "python3*", "site-packages"))
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, pat := range patterns {
		matches, _ := filepath.Glob(pat)
		for _, m := range matches {
			if c := filepath.Clean(m); isDir(c) && !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out
}

func anyHas(dirs []string, rel string) bool {
	for _, d := range dirs {
		if fileExists(filepath.Join(d, rel)) {
			return true
		}
	}
	return false
}

// hfHubCache resolves the hub cache the way huggingface_hub does: HF_HUB_CACHE (or the
// legacy HUGGINGFACE_HUB_CACHE), else $HF_HOME/hub, else $XDG_CACHE_HOME/huggingface/hub,
// else ~/.cache/huggingface/hub.
func hfHubCache() string {
	for _, k := range []string{"HF_HUB_CACHE", "HUGGINGFACE_HUB_CACHE"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	if v := os.Getenv("HF_HOME"); v != "" {
		return filepath.Join(v, "hub")
	}
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "huggingface", "hub")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".cache", "huggingface", "hub")
	}
	return filepath.Join(home, ".cache", "huggingface", "hub")
}

// hasSnapshot: a hub repo directory with at least one non-empty snapshot.
func hasSnapshot(repoDir string) bool {
	snaps, err := os.ReadDir(filepath.Join(repoDir, "snapshots"))
	if err != nil {
		return false
	}
	for _, s := range snaps {
		if !s.IsDir() {
			continue
		}
		if files, err := os.ReadDir(filepath.Join(repoDir, "snapshots", s.Name())); err == nil && len(files) > 0 {
			return true
		}
	}
	return false
}
