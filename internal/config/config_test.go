package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
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
	// Opt-in, like VisionModel: no serving template defines a whisper seat, so a
	// non-empty default bound every node to an alias it could not serve.
	if c.STTModel != "" {
		t.Errorf("STTModel = %q, want \"\" (a tier earns this binding via an stt media seat)", c.STTModel)
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

	// One wait ceiling for every GPU task. The per-task videogen_wait_ms /
	// audiogen_wait_ms are gone: at 90s the distinction bought nothing, and every
	// shipped config carries videogen_wait_ms=1200000, so honouring it as an override
	// would have restored the 20-minute wait on upgrade.
	if c.GPUWaitMs <= 0 {
		t.Errorf("GPUWaitMs = %d, want > 0", c.GPUWaitMs)
	}
}

// Every install shipped with videogen_wait_ms=1200000 / audiogen_wait_ms=120000 in its
// config.json, so those keys are still sitting on disk everywhere. They must LOAD
// CLEANLY and change nothing — honouring them would silently restore the 20-minute
// video wait on upgrade, which is the behaviour the bounded queue replaced.
func TestRetiredPerTaskWaitKeysAreIgnored(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cfg.json")
	js := `{"videogen_wait_ms":1200000,"audiogen_wait_ms":120000}`
	if err := os.WriteFile(p, []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("a config carrying the retired keys must still load: %v", err)
	}
	if c.GPUWaitMs != Default().GPUWaitMs {
		t.Errorf("GPUWaitMs = %d, want the default %d — a stale per-task key changed the ceiling",
			c.GPUWaitMs, Default().GPUWaitMs)
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
// TestFleetDefaults: fleet-serve binds loopback:18811 by default (the
// dispatcher owns 18810), the node id resolves to the hostname at serve time
// (never baked into a shareable config), and the footprint sampler is "auto"
// (PDH tree on Windows, global-delta elsewhere).
func TestFleetDefaults(t *testing.T) {
	c := Default()
	if c.FleetListen != "127.0.0.1:18811" {
		t.Errorf("FleetListen = %q, want \"127.0.0.1:18811\"", c.FleetListen)
	}
	if c.FleetNodeID != "" {
		t.Errorf("FleetNodeID = %q, want \"\" (hostname at serve time)", c.FleetNodeID)
	}
	if c.FleetSampler != "auto" {
		t.Errorf("FleetSampler = %q, want \"auto\"", c.FleetSampler)
	}
	if c.FleetAuthToken != "" {
		t.Errorf("FleetAuthToken = %q, want \"\" (no auth by default — the agent lane is then loopback-only)", c.FleetAuthToken)
	}
}

// TestFleetFieldsRoundTrip: the fleet keys load from JSON (a Tailscale binding
// + explicit node id + forced global sampler — the FLEET-NODE.md fallback —
// + the agent-lane bearer token).
func TestFleetFieldsRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cfg.json")
	js := `{"fleet_listen":"100.64.0.10:18811","fleet_node_id":"node-a","fleet_sampler":"global","fleet_auth_token":"s3cret"}`
	if err := os.WriteFile(p, []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.FleetListen != "100.64.0.10:18811" || got.FleetNodeID != "node-a" || got.FleetSampler != "global" {
		t.Fatalf("fleet fields did not round-trip: listen=%q node=%q sampler=%q",
			got.FleetListen, got.FleetNodeID, got.FleetSampler)
	}
	if got.FleetAuthToken != "s3cret" {
		t.Fatalf("FleetAuthToken = %q, want \"s3cret\"", got.FleetAuthToken)
	}
}

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
		name string
		cfg  Config
		want string
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
		{"~user/x", "~user/x"}, // ambiguous on Windows — untouched
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

// TestLoadTrimsPrimaryGPUUUID locks the copy-paste safety net for
// primary_gpu_uuid (docs/FLEET-NODE.md tells an operator to copy this
// straight out of a running node's /fleet/health gpu_devices[]): a value
// picked up with surrounding whitespace (a trailing newline from a terminal
// copy, a stray leading space) must be trimmed at Load — not left to silently
// fail SelectHeadlineDevice's match and drop to the fallback rule + warning.
func TestLoadTrimsPrimaryGPUUUID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"primary_gpu_uuid": "  GPU-8888bbbb-9999-cccc-dddd-eeeeffff0000\n"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.PrimaryGPUUUID != "GPU-8888bbbb-9999-cccc-dddd-eeeeffff0000" {
		t.Errorf("PrimaryGPUUUID = %q, want the whitespace trimmed off", got.PrimaryGPUUUID)
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
		"sdcpp_script", "sdcpp_bin", "sdcpp_model", "sdcpp_vae", "sdcpp_clip_l", "sdcpp_clip_g", "sdcpp_t5xxl", "sdcpp_llm",
		"inpaint_script", "gen_edit_script", "upscale_script",
		"videogen_script", "animategen_script", "run_graph_script", "voicegen_script", "musicgen_script", "gpu_lock_path", "state_dir",
		"voicegen_ref", "voicegen_ft_model", "voicegen_ft_base_dir", "voicegen_ft_ref",
		"edit_python", "gimp_console_path",
		"cache_path", "ledger_path",
		"thresholds_path", "tier_overrides_path", "router_weights_path",
		"confhead_path", "router_labels_path", "confhead_labels_path",
		"confhead_thresholds_path", "exemplars_dir",
		"shadow_queue_path", "agent_trajectory_queue_path", "agent_trajectory_labels_path",
		"knn_index_path", "embed_memo_path",
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

// TestCompactionFlipDefaults pins the 2026-07-24 flip decision: pipeline GCF
// compaction defaults ON (lossless + fail-closed; measured), while an explicit
// false in a config file still wins (Load unmarshals over Default()).
func TestCompactionFlipDefaults(t *testing.T) {
	if !Default().GCFCompact {
		t.Fatal("Default().GCFCompact must be true (flip decision 2026-07-24)")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(`{"gcf_compact": false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.GCFCompact {
		t.Fatal("an explicit gcf_compact:false in the config file must still disable it")
	}
}

// TestComfyDirDefaultIsOSAware: comfy_dir defaulted to "C:/ComfyUI" on EVERY platform,
// so a Linux node claimed a ComfyUI install it cannot have and — because the
// ComfyUI-backed scripts ARE bound by default — advertised video-gen / audio-gen /
// run-graph to the fleet, which fail on arrival. Off Windows the honest default is
// unbound; the installer writes the real path per machine.
func TestComfyDirDefaultIsOSAware(t *testing.T) {
	got := Default().ComfyDir
	if runtime.GOOS == "windows" {
		if got != "C:/ComfyUI" {
			t.Fatalf("the Windows default must not change: got %q", got)
		}
		return
	}
	if got != "" {
		t.Fatalf("comfy_dir default on %s = %q; a non-Windows box must be UNBOUND rather than "+
			"pointed at a path that cannot exist", runtime.GOOS, got)
	}
}

// TestHomeRebasesDerivedPathsOnly: "home" exists so an install can live on the volume
// that has room WITHOUT hand-writing a dozen absolute paths — the hand-patching that
// put a model tree on an OS drive and then drifted. A path the operator typed must
// survive untouched; only defaults follow.
func TestHomeRebasesDerivedPathsOnly(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	explicit := filepath.Join(dir, "somewhere-else", "ledger.jsonl")
	body := `{"home":"D:/offload-stack","ledger_path":` + strconv.Quote(explicit) + `}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	wantCache := filepath.Join("D:/offload-stack", "cache.db")
	if got.CachePath != wantCache {
		t.Errorf("cache_path = %q, want %q (a default must follow home)", got.CachePath, wantCache)
	}
	if got.MediaDir != filepath.Join("D:/offload-stack", "media") {
		t.Errorf("media_dir = %q, want it rebased onto home", got.MediaDir)
	}
	if got.LedgerPath != explicit {
		t.Errorf("ledger_path = %q, want the operator's value %q untouched", got.LedgerPath, explicit)
	}
}

// TestHomeNeverRebasesTheMachineWideState: the GPU lease root is machine-wide ON
// PURPOSE. Moving it under a home directory is exactly the per-user trap that made
// mutual exclusion evaporate once already (0.24.1 added a warning for it).
func TestHomeNeverRebasesTheMachineWideState(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"home":"D:/offload-stack"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.StateDir != "" || got.GPULockPath != "" {
		t.Errorf("state_dir=%q gpu_lock_path=%q — the machine-wide lease root must stay unset so "+
			"internal/gpulease resolves it machine-wide", got.StateDir, got.GPULockPath)
	}
	// Binaries resolved by name or by an absolute platform path are not ours to move.
	if got.NodePath != "node" {
		t.Errorf("node_path = %q, want the bare name untouched", got.NodePath)
	}
	if runtime.GOOS == "windows" && got.ComfyDir != "C:/ComfyUI" {
		t.Errorf("comfy_dir = %q, want the platform default untouched", got.ComfyDir)
	}
}

// TestDefaultBaseHonorsTheEnvVar: the same knob has to work before any config file
// exists — that is the bootstrap case an installer runs in.
func TestDefaultBaseHonorsTheEnvVar(t *testing.T) {
	t.Setenv("LOCAL_OFFLOAD_HOME", filepath.Join("V:", "offload-stack"))
	if got := DefaultBase(); got != filepath.Join("V:", "offload-stack") {
		t.Errorf("DefaultBase() = %q, want the env value", got)
	}
	if got := Default().CachePath; got != filepath.Join("V:", "offload-stack", "cache.db") {
		t.Errorf("Default().CachePath = %q, want it under the env home", got)
	}
	t.Setenv("LOCAL_OFFLOAD_HOME", "")
	home, _ := os.UserHomeDir()
	if got := DefaultBase(); got != filepath.Join(home, ".local-offload") {
		t.Errorf("with the env cleared DefaultBase() = %q, want ~/.local-offload", got)
	}
}

// --- Task 4: pipelines config surface (config-driven fleet "pipeline job" family) ---

// TestPipelinesDefaultEmpty: pipelines are opt-in per box, exactly like
// ImageGenScript/InpaintScript — a shared config must never bind a box to a
// pipeline CLI that only exists on one machine.
func TestPipelinesDefaultEmpty(t *testing.T) {
	c := Default()
	if len(c.Pipelines) != 0 {
		t.Errorf("Pipelines default must be empty (opt-in per box); got %v", c.Pipelines)
	}
}

// TestPipelinesLoadValidation: a config JSON with a valid pipelines entry loads
// cleanly and PipelineNames() reports it; an entry missing a required field
// fails Load with an error naming BOTH the field and the pipeline key — a bad
// entry must fail at config-load time, never silently at dispatch time.
func TestPipelinesLoadValidation(t *testing.T) {
	// Real, genuinely-absolute (per THIS OS) paths — a hand-typed "/abs/..."
	// literal is absolute on POSIX but NOT on Windows (filepath.IsAbs requires
	// a drive letter or UNC prefix there), so validatePipelines' new
	// filepath.IsAbs(script) check needs an actually-absolute fixture.
	// filepath.ToSlash keeps the JSON literal escape-free; Go's filepath.IsAbs
	// accepts forward slashes after a Windows drive letter too.
	absScript := filepath.ToSlash(filepath.Join(t.TempDir(), "run-scene-swap.mjs"))
	absWorkdir := filepath.ToSlash(t.TempDir())

	cases := []struct {
		name    string
		json    string
		wantErr string // substring the Load error must contain; "" = must load cleanly
	}{
		{
			name: "valid scene-swap entry loads",
			json: fmt.Sprintf(`{"pipelines":{"scene-swap":{"script":%q,"workdir":%q,`+
				`"timeout_sec":2400,"artifacts":["final.png","qa-report.json"]}}}`, absScript, absWorkdir),
		},
		{
			name: "missing timeout_sec fails naming field and key",
			json: fmt.Sprintf(`{"pipelines":{"scene-swap":{"script":%q,"workdir":%q,`+
				`"artifacts":["final.png"]}}}`, absScript, absWorkdir),
			wantErr: `pipelines["scene-swap"]: timeout_sec`,
		},
		{
			name: "zero timeout_sec fails naming field and key",
			json: fmt.Sprintf(`{"pipelines":{"scene-swap":{"script":%q,"workdir":%q,`+
				`"timeout_sec":0,"artifacts":["final.png"]}}}`, absScript, absWorkdir),
			wantErr: `pipelines["scene-swap"]: timeout_sec`,
		},
		{
			name: "empty script fails naming field and key",
			json: fmt.Sprintf(`{"pipelines":{"scene-swap":{"script":"","workdir":%q,`+
				`"timeout_sec":60,"artifacts":["final.png"]}}}`, absWorkdir),
			wantErr: `pipelines["scene-swap"]: script`,
		},
		{
			name: "relative script fails naming field and key",
			json: fmt.Sprintf(`{"pipelines":{"scene-swap":{"script":"relative/run.mjs","workdir":%q,`+
				`"timeout_sec":60,"artifacts":["final.png"]}}}`, absWorkdir),
			wantErr: `pipelines["scene-swap"]: script must be an absolute path`,
		},
		{
			name: "empty workdir fails naming field and key",
			json: fmt.Sprintf(`{"pipelines":{"scene-swap":{"script":%q,"workdir":"",`+
				`"timeout_sec":60,"artifacts":["final.png"]}}}`, absScript),
			wantErr: `pipelines["scene-swap"]: workdir`,
		},
		{
			name: "empty artifacts fails naming field and key",
			json: fmt.Sprintf(`{"pipelines":{"scene-swap":{"script":%q,"workdir":%q,`+
				`"timeout_sec":60,"artifacts":[]}}}`, absScript, absWorkdir),
			wantErr: `pipelines["scene-swap"]: artifacts`,
		},
		{
			name: "artifact with forward slash fails naming field and key",
			json: fmt.Sprintf(`{"pipelines":{"scene-swap":{"script":%q,"workdir":%q,`+
				`"timeout_sec":60,"artifacts":["sub/final.png"]}}}`, absScript, absWorkdir),
			wantErr: `pipelines["scene-swap"]: artifact "sub/final.png" must be a bare filename`,
		},
		{
			name: "artifact with backslash fails naming field and key",
			json: fmt.Sprintf(`{"pipelines":{"scene-swap":{"script":%q,"workdir":%q,`+
				`"timeout_sec":60,"artifacts":["sub\\final.png"]}}}`, absScript, absWorkdir),
			wantErr: `must be a bare filename`,
		},
		{
			name: "negative max_ref_mb fails naming field and key",
			json: fmt.Sprintf(`{"pipelines":{"scene-swap":{"script":%q,"workdir":%q,`+
				`"timeout_sec":60,"artifacts":["final.png"],"max_ref_mb":-5}}}`, absScript, absWorkdir),
			wantErr: `pipelines["scene-swap"]: max_ref_mb must be >= 0 (got -5)`,
		},
		{
			name: "zero max_ref_mb loads cleanly (means default-24 via RefCapMB)",
			json: fmt.Sprintf(`{"pipelines":{"scene-swap":{"script":%q,"workdir":%q,`+
				`"timeout_sec":60,"artifacts":["final.png"],"max_ref_mb":0}}}`, absScript, absWorkdir),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "cfg.json")
			if err := os.WriteFile(p, []byte(tc.json), 0o644); err != nil {
				t.Fatal(err)
			}
			c, err := Load(p)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected a clean load, got error: %v", err)
				}
				if names := c.PipelineNames(); len(names) != 1 || names[0] != "scene-swap" {
					t.Fatalf(`PipelineNames() = %v, want ["scene-swap"]`, names)
				}
				return
			}
			if err == nil {
				t.Fatal("expected a load error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestValidatePipelinesMultiErrorDeterministic: when MORE THAN ONE pipelines
// entry is invalid, the error returned must be deterministic — the entry
// visited FIRST in SORTED key order — never dependent on Go's randomized map
// iteration. Both entries here are invalid (empty script) for the identical
// reason, so the only thing distinguishing which error surfaces is key sort
// order: "aaa-first" sorts before "zzz-second" and must be the one named,
// every time, across repeated loads (map iteration order is re-randomized
// per run, so a single pass proves little).
func TestValidatePipelinesMultiErrorDeterministic(t *testing.T) {
	absWorkdir := filepath.ToSlash(t.TempDir())
	body := fmt.Sprintf(`{"pipelines":{`+
		`"zzz-second":{"script":"","workdir":%q,"timeout_sec":60,"artifacts":["final.png"]},`+
		`"aaa-first":{"script":"","workdir":%q,"timeout_sec":60,"artifacts":["final.png"]}`+
		`}}`, absWorkdir, absWorkdir)
	for i := 0; i < 5; i++ {
		p := filepath.Join(t.TempDir(), "cfg.json")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Load(p)
		if err == nil {
			t.Fatal("expected a load error")
		}
		if !strings.Contains(err.Error(), `pipelines["aaa-first"]`) {
			t.Fatalf("run %d: error = %q, want it to name the sorted-first key aaa-first (not zzz-second)", i, err.Error())
		}
	}
}

// TestPipelineNamesSortedAndSkipsInvalid: PipelineNames() returns sorted keys
// and — for a Config assembled directly rather than through Load() (tests,
// future callers) — silently skips any entry that fails the same validation
// rules Load() enforces, so a caller never advertises a route it cannot run.
func TestPipelineNamesSortedAndSkipsInvalid(t *testing.T) {
	c := Config{Pipelines: map[string]PipelineSpec{
		"zzz-last":  {Script: "/a", Workdir: "/b", TimeoutSec: 10, Artifacts: []string{"out.png"}},
		"aaa-first": {Script: "/a", Workdir: "/b", TimeoutSec: 10, Artifacts: []string{"out.png"}},
		"bad-entry": {Script: "", Workdir: "/b", TimeoutSec: 10, Artifacts: []string{"out.png"}},
	}}
	got := c.PipelineNames()
	want := []string{"aaa-first", "zzz-last"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("PipelineNames() = %v, want %v", got, want)
	}
}

// TestPipelinesConfigured mirrors ImageRouteConfigured's simple gate, scaled to
// a variable-size set of pipeline routes.
func TestPipelinesConfigured(t *testing.T) {
	var c Config
	if c.PipelinesConfigured() {
		t.Fatal("no pipelines configured by default")
	}
	c.Pipelines = map[string]PipelineSpec{
		"scene-swap": {Script: "/a", Workdir: "/b", TimeoutSec: 10, Artifacts: []string{"out.png"}},
	}
	if !c.PipelinesConfigured() {
		t.Fatal("a valid pipeline entry should report configured")
	}
}

// TestPipelineSpecRefCapMB: max_ref_mb <= 0 reads as 24 (a scene-swap job pulls
// a handful of PNGs, not a video); a positive value passes through unchanged.
func TestPipelineSpecRefCapMB(t *testing.T) {
	cases := []struct {
		name string
		spec PipelineSpec
		want int
	}{
		{"zero defaults to 24", PipelineSpec{}, 24},
		{"negative defaults to 24", PipelineSpec{MaxRefMB: -5}, 24},
		{"positive passes through", PipelineSpec{MaxRefMB: 48}, 48},
	}
	for _, tc := range cases {
		if got := tc.spec.RefCapMB(); got != tc.want {
			t.Errorf("%s: RefCapMB() = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestPipelinePathsTildeExpand: script/workdir live inside a variable-size map
// (Config.Pipelines), so they cannot join the flat pathFields list — they get
// their own tilde-expansion pass, exercised here the same way
// TestLoadExpandsTildeInEveryPathField exercises the fixed fields.
func TestPipelinePathsTildeExpand(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir on this runner")
	}
	js := `{"pipelines":{"scene-swap":{"script":"~/pipelines/run.mjs","workdir":"~/pipelines",` +
		`"timeout_sec":60,"artifacts":["final.png"]}}}`
	p := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(p, []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	spec := c.Pipelines["scene-swap"]
	wantScript := filepath.Join(home, "pipelines", "run.mjs")
	wantWorkdir := filepath.Join(home, "pipelines")
	if spec.Script != wantScript {
		t.Errorf("pipeline script = %q, want %q", spec.Script, wantScript)
	}
	if spec.Workdir != wantWorkdir {
		t.Errorf("pipeline workdir = %q, want %q", spec.Workdir, wantWorkdir)
	}
}

func TestAcceleratorDefaultsAreInert(t *testing.T) {
	c := Default()
	if len(c.Accelerators) != 0 {
		t.Fatalf("Accelerators default = %v, want empty (a box with no NPU is byte-identical to today)", c.Accelerators)
	}
	if c.HasAccelerator("hailo-8l") {
		t.Fatal("HasAccelerator(hailo-8l) true on a default config")
	}
	if c.HailoEndpoint != "http://127.0.0.1:18813" {
		t.Errorf("HailoEndpoint = %q, want loopback :18813", c.HailoEndpoint)
	}
	if c.HailoSidecarCmd != "" {
		t.Errorf("HailoSidecarCmd = %q, want \"\" (an installer seeds it; never a baked path)", c.HailoSidecarCmd)
	}
	if c.HailoTimeoutSec != 60 || c.HailoIdleSec != 300 {
		t.Errorf("timeout/idle = %d/%d, want 60/300", c.HailoTimeoutSec, c.HailoIdleSec)
	}
}

func TestAcceleratorFieldsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	body := `{"accelerators":["hailo-8l"],"hailo_endpoint":"http://127.0.0.1:19999","hailo_sidecar_cmd":"D:/x/hailo-http.cmd","hailo_timeout_sec":7,"hailo_idle_sec":9}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.HasAccelerator("hailo-8l") || c.HasAccelerator("tpu") {
		t.Fatalf("HasAccelerator: got %v", c.Accelerators)
	}
	if c.HailoEndpoint != "http://127.0.0.1:19999" || c.HailoSidecarCmd != "D:/x/hailo-http.cmd" || c.HailoTimeoutSec != 7 || c.HailoIdleSec != 9 {
		t.Fatalf("round-trip lost a field: %+v", c)
	}
}
