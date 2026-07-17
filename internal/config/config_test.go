package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestDefaultVideoFields(t *testing.T) {
	c := Default()
	if c.VideoFPS != 2.0 {
		t.Errorf("VideoFPS = %v, want 2.0", c.VideoFPS)
	}
	if c.VideoMaxFrames != 12 {
		t.Errorf("VideoMaxFrames = %d, want 12", c.VideoMaxFrames)
	}
	if c.VideoFrameWidth != 512 {
		t.Errorf("VideoFrameWidth = %d, want 512", c.VideoFrameWidth)
	}
	if c.FFmpegPath != "ffmpeg" {
		t.Errorf("FFmpegPath = %q, want \"ffmpeg\"", c.FFmpegPath)
	}
}

func TestDefaultSTTFields(t *testing.T) {
	c := Default()
	if c.STTModel != "whisper-stt" {
		t.Errorf("STTModel = %q, want \"whisper-stt\"", c.STTModel)
	}
	if c.STTModelHQ != "" {
		t.Errorf("STTModelHQ = %q, want \"\" (opt-in; no phantom default)", c.STTModelHQ)
	}
	if !c.STTVAD {
		t.Error("STTVAD should default true")
	}
	if c.STTMaxInlineSegments != 120 {
		t.Errorf("STTMaxInlineSegments = %d, want 120", c.STTMaxInlineSegments)
	}
	if !c.STTUnloadAfter {
		t.Error("STTUnloadAfter should default true (zero-always-warm)")
	}
	if c.STTRequestTimeoutSec != 1800 {
		t.Errorf("STTRequestTimeoutSec = %d, want 1800", c.STTRequestTimeoutSec)
	}
	if c.MediaDir == "" {
		t.Error("MediaDir should default to a non-empty path")
	}
}

// TestDefaultGenerationFields: the video/audio generation defaults match the
// brief verbatim — VideoGenScript=render/comfy-video.mjs, VoiceGenScript=render/tts.mjs,
// MusicGenScript=render/comfy-music.mjs (the B3 ACE-Step worker). Per-task timeouts and
// waitMs (so a queued TTS isn't starved by a 20-min video job) are present and positive.
func TestDefaultGenerationFields(t *testing.T) {
	c := Default()
	if c.VideoGenScript != "render/comfy-video.mjs" {
		t.Errorf("VideoGenScript = %q, want \"render/comfy-video.mjs\"", c.VideoGenScript)
	}
	if c.VoiceGenScript != "render/tts.mjs" {
		t.Errorf("VoiceGenScript = %q, want \"render/tts.mjs\"", c.VoiceGenScript)
	}
	if c.MusicGenScript != "render/comfy-music.mjs" {
		t.Errorf("MusicGenScript = %q, want \"render/comfy-music.mjs\" (B3 worker)", c.MusicGenScript)
	}
	if c.VideoGenTimeoutSec <= 0 {
		t.Errorf("VideoGenTimeoutSec = %d, want > 0", c.VideoGenTimeoutSec)
	}
	if c.AudioGenTimeoutSec <= 0 {
		t.Errorf("AudioGenTimeoutSec = %d, want > 0", c.AudioGenTimeoutSec)
	}
	if c.VideoGenWaitMs <= 0 {
		t.Errorf("VideoGenWaitMs = %d, want > 0", c.VideoGenWaitMs)
	}
	if c.AudioGenWaitMs <= 0 {
		t.Errorf("AudioGenWaitMs = %d, want > 0", c.AudioGenWaitMs)
	}
}

func TestInpaintFieldsRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cfg.json")
	js := `{"inpaint_script":"render/comfy-inpaint.mjs","inpaint_ckpt":"some-sdxl.safetensors",` +
		`"inpaint_vae":"builtin","inpaint_steps":34,"inpaint_cfg":6.5,"inpaint_sampler":"dpmpp_2m",` +
		`"inpaint_scheduler":"karras","inpaint_timeout_sec":1200}`
	if err := os.WriteFile(p, []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.InpaintScript != "render/comfy-inpaint.mjs" || got.InpaintCkpt != "some-sdxl.safetensors" ||
		got.InpaintVAE != "builtin" || got.InpaintSteps != 34 || got.InpaintCFG != 6.5 ||
		got.InpaintSampler != "dpmpp_2m" || got.InpaintScheduler != "karras" || got.InpaintTimeoutSec != 1200 {
		t.Fatalf("inpaint fields did not round-trip: %+v", got)
	}
}

func TestInpaintDefaults(t *testing.T) {
	c := Default()
	if c.InpaintScript != "" || c.InpaintCkpt != "" {
		t.Errorf("inpaint route must default UNCONFIGURED (empty = defer); got script=%q ckpt=%q",
			c.InpaintScript, c.InpaintCkpt)
	}
	if c.InpaintTimeoutSec != 900 {
		t.Errorf("InpaintTimeoutSec = %d, want 900", c.InpaintTimeoutSec)
	}
}

// TestDefaultMemoryStack: the CPU memory stack the GPU-free helper must never unload
// is sourced from config (not a buried const) so a renamed/added member is honored.
// Default carries the two canonical CPU-only members.
func TestDefaultMemoryStack(t *testing.T) {
	c := Default()
	want := map[string]bool{"embeddinggemma": true, "bge-reranker-v2-m3": true}
	if len(c.MemoryStack) != len(want) {
		t.Fatalf("MemoryStack = %v, want %v", c.MemoryStack, want)
	}
	for _, m := range c.MemoryStack {
		if !want[m] {
			t.Errorf("MemoryStack has unexpected member %q", m)
		}
	}
}

func TestKNNDefaults(t *testing.T) {
	c := Default()
	if c.KNNPreFilterEnabled {
		t.Fatal("kNN must be OFF by default")
	}
	if c.KNNPreFilterK != 7 {
		t.Fatalf("KNNPreFilterK default: want 7, got %d", c.KNNPreFilterK)
	}
	if c.KNNMinNeighbors != 20 {
		t.Fatalf("KNNMinNeighbors default: want 20, got %d", c.KNNMinNeighbors)
	}
	if c.KNNPreFilterThreshold != 0.5 {
		t.Fatalf("KNNPreFilterThreshold default: want 0.5, got %v", c.KNNPreFilterThreshold)
	}
	if c.KNNEmbedTimeoutMs != 2000 {
		t.Fatalf("KNNEmbedTimeoutMs default: want 2000, got %d", c.KNNEmbedTimeoutMs)
	}
	if filepath.Base(c.KNNIndexPath) != "knn-index.jsonl" {
		t.Fatalf("KNNIndexPath default basename: want knn-index.jsonl, got %q", c.KNNIndexPath)
	}
}

// TestEmbedModelResolution guards that the embedder model is unambiguous and
// reorder-proof: explicit embed_model wins; else MemoryStack[0] (back-compat); else
// the "embeddinggemma" fallback. A stack that lists the reranker first must NOT make
// the embedder request the reranker when embed_model is set.
func TestEmbedModelResolution(t *testing.T) {
	cases := []struct {
		name  string
		cfg   Config
		want  string
	}{
		{"explicit embed_model wins over stack order", Config{EmbedModelName: "my-embedder", MemoryStack: []string{"bge-reranker-v2-m3", "my-embedder"}}, "my-embedder"},
		{"falls back to MemoryStack[0]", Config{MemoryStack: []string{"embeddinggemma", "bge-reranker-v2-m3"}}, "embeddinggemma"},
		{"empty everything falls back to the literal", Config{}, "embeddinggemma"},
		{"empty first stack element falls back to the literal", Config{MemoryStack: []string{""}}, "embeddinggemma"},
	}
	for _, tc := range cases {
		if got := tc.cfg.EmbedModel(); got != tc.want {
			t.Errorf("%s: EmbedModel() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestExpandTilde pins the home-shorthand expansion rules (LO-4): "~/x" and a
// bare "~" expand; "~user/x" and plain relative/absolute paths do not.
func TestExpandTilde(t *testing.T) {
	home := filepath.Join("C:", "Users", "u")
	cases := []struct{ in, want string }{
		{"~/x/y.json", filepath.Join(home, "x", "y.json")},
		{"~", home},
		{"~user/x", "~user/x"},   // ambiguous on Windows — untouched
		{"render/tts.mjs", "render/tts.mjs"},
		{"", ""},
	}
	// The `~\` (backslash) form is a Windows path convention. On non-Windows a
	// backslash is a literal filename character, not a separator, so filepath.Join
	// does not normalise it to "x/y.json" — only assert this form on Windows.
	if runtime.GOOS == "windows" {
		cases = append(cases, struct{ in, want string }{`~\x\y.json`, filepath.Join(home, "x", "y.json")})
	}
	for _, tc := range cases {
		if got := ExpandTilde(tc.in, home); got != tc.want {
			t.Errorf("ExpandTilde(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := ExpandTilde("~/x", ""); got != "~/x" {
		t.Errorf("empty home must leave the path untouched, got %q", got)
	}
}

// TestLoadExpandsTildeInEveryPathField: a config file using "~/" in every
// path-typed field loads with each expanded to the real home dir — the
// shipped config.example.json convention now actually works (LO-4).
func TestLoadExpandsTildeInEveryPathField(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir on this runner")
	}
	// Build a config JSON that sets EVERY enumerated path field to a "~/" path.
	var c Config
	fields := pathFields(&c)
	names := pathFieldJSONNames(t)
	if len(fields) != len(names) {
		t.Fatalf("pathFields has %d entries but %d json names enumerated", len(fields), len(names))
	}
	m := map[string]string{}
	for i, n := range names {
		m[n] = "~/probe/" + n
		_ = i
	}
	b, _ := json.Marshal(m)
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	for i, ptr := range pathFields(&got) {
		want := filepath.Join(home, "probe", names[i])
		if *ptr != want {
			t.Errorf("field %s = %q, want %q", names[i], *ptr, want)
		}
	}
}

// pathFieldJSONNames returns the json key for each pathFields entry, in the
// same order. Kept as an explicit list so a drift between the two enumerations
// fails the length check above.
func pathFieldJSONNames(t *testing.T) []string {
	t.Helper()
	return []string{
		"ffmpeg_path", "media_dir", "svg_dir",
		"imagegen_script", "node_path", "comfy_dir",
		"inpaint_script",
		"videogen_script", "run_graph_script", "voicegen_script", "musicgen_script", "gpu_lock_path",
		"voicegen_ref", "voicegen_ft_model", "voicegen_ft_base_dir", "voicegen_ft_ref",
		"edit_python", "gimp_console_path",
		"cache_path", "ledger_path",
		"thresholds_path", "tier_overrides_path", "router_weights_path",
		"confhead_path", "router_labels_path", "confhead_labels_path",
		"confhead_thresholds_path", "exemplars_dir",
		"shadow_queue_path", "agent_trajectory_queue_path", "agent_trajectory_labels_path",
		"knn_index_path",
	}
}

// TestPathFieldsCoverEveryPathTypedStructField guards the enumeration: every
// Config field whose json tag looks path-typed (*_path/_dir/_script or the
// known executable fields) must be in pathFields, so a new path field cannot
// silently miss tilde expansion.
func TestPathFieldsCoverEveryPathTypedStructField(t *testing.T) {
	var c Config
	enumerated := map[*string]bool{}
	for _, p := range pathFields(&c) {
		enumerated[p] = true
	}
	v := reflect.ValueOf(&c).Elem()
	tp := v.Type()
	for i := 0; i < tp.NumField(); i++ {
		f := tp.Field(i)
		if f.Type.Kind() != reflect.String {
			continue
		}
		tag := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
		if tag == "completion_path" {
			continue // an HTTP route ("/v1/chat/completions"), not a filesystem path
		}
		pathish := strings.HasSuffix(tag, "_path") || strings.HasSuffix(tag, "_dir") ||
			strings.HasSuffix(tag, "_script") || tag == "comfy_dir"
		if !pathish {
			continue
		}
		ptr := v.Field(i).Addr().Interface().(*string)
		if !enumerated[ptr] {
			t.Errorf("path-typed field %s (json %q) missing from pathFields — tilde expansion would skip it", f.Name, tag)
		}
	}
}

func TestVoiceConfigDefaultsInert(t *testing.T) {
	c := Default()
	if c.VoiceGenRef != "" || c.VoiceGenFTModel != "" || c.VoiceGenFTBaseDir != "" || c.VoiceGenFTRef != "" {
		t.Errorf("FT/ref path defaults must be empty (model-agnostic; empty=defer); got ref=%q model=%q base=%q ftref=%q",
			c.VoiceGenRef, c.VoiceGenFTModel, c.VoiceGenFTBaseDir, c.VoiceGenFTRef)
	}
	if c.VoiceGenFTTemperature != 0 || c.VoiceGenFTCFGWeight != 0 || c.VoiceGenFTExaggeration != 0 || c.VoiceGenFTRepetitionPenalty != 0 {
		t.Error("FT recipe knobs must default 0 (worker default; tuned values live in per-machine config)")
	}
}

func TestVoiceConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.json")
	js := `{"voicegen_ref":"/r/gen.wav","voicegen_ft_model":"/m/merged.safetensors",` +
		`"voicegen_ft_base_dir":"/m/base","voicegen_ft_ref":"/r/dan.wav","voicegen_ft_lang":"es",` +
		`"voicegen_ft_temperature":0.6,"voicegen_ft_cfg_weight":0.5,"voicegen_ft_exaggeration":0.6,` +
		`"voicegen_ft_repetition_penalty":1.2}`
	if err := os.WriteFile(p, []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.VoiceGenRef != "/r/gen.wav" || c.VoiceGenFTModel != "/m/merged.safetensors" ||
		c.VoiceGenFTBaseDir != "/m/base" || c.VoiceGenFTRef != "/r/dan.wav" || c.VoiceGenFTLang != "es" {
		t.Errorf("string keys did not round-trip: %+v", c)
	}
	if c.VoiceGenFTTemperature != 0.6 || c.VoiceGenFTCFGWeight != 0.5 ||
		c.VoiceGenFTExaggeration != 0.6 || c.VoiceGenFTRepetitionPenalty != 1.2 {
		t.Errorf("recipe knobs did not round-trip: %+v", c)
	}
}
