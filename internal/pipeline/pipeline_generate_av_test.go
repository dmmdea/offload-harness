package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

func requireNodePipeline(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; generate av routing test needs the verified toolchain")
	}
}

// writeStub writes a tiny node script that creates its first CLI arg (the out path)
// then exits 0 — a GPU-free stand-in for a render/*.mjs runner, so routing can be
// verified without a live ComfyUI/Chatterbox render.
func writeStub(t *testing.T, dir string) string {
	t.Helper()
	stub := filepath.Join(dir, "stub.mjs")
	if err := os.WriteFile(stub, []byte(`import {writeFileSync} from "node:fs";
const out = process.argv[2];
writeFileSync(out, "stub-output");
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return stub
}

// TestRunGenerateVideo_EmptyScriptDefers: no video route configured => clean defer,
// never a crash (invariant 4: defer-not-crash).
func TestRunGenerateVideo_EmptyScriptDefers(t *testing.T) {
	cfg := config.Default()
	cfg.VideoGenScript = ""
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{
		Task:  core.TaskGenerateVideo,
		Input: "a calm ocean at dawn",
		Image: filepath.Join(t.TempDir(), "still.png"),
	})
	if res.OK || !res.Deferred {
		t.Fatalf("empty VideoGenScript must defer, got ok=%v", res.OK)
	}
}

// TestRunGenerateVideo_EmptyPromptDefers: a blank prompt defers cleanly.
func TestRunGenerateVideo_EmptyPromptDefers(t *testing.T) {
	cfg := config.Default() // VideoGenScript is the non-empty default
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateVideo, Input: "   "})
	if res.OK || !res.Deferred {
		t.Fatalf("empty video prompt must defer, got ok=%v", res.OK)
	}
}

// TestRunGenerateVideo_RoutesToScript: a configured (stub) video script runs and the
// result carries the produced video_path + seed. Proves the Go path wires through.
func TestRunGenerateVideo_RoutesToScript(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.VideoGenScript = writeStub(t, dir)
	cfg.MediaDir = dir
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateVideo, Input: "a calm ocean at dawn"})
	if !res.OK {
		t.Fatalf("expected ok via stub, got defer: %s", res.Reason)
	}
	var out struct {
		VideoPath string `json:"video_path"`
		Seed      int    `json:"seed"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.VideoPath == "" {
		t.Fatal("result missing video_path")
	}
	if out.Seed <= 0 {
		t.Fatal("a seed should have been minted")
	}
	if _, err := os.Stat(out.VideoPath); err != nil {
		t.Fatalf("video file not produced: %v", err)
	}
}

// TestRunGenerateAudio_VoiceEmptyScriptDefers: kind=voice with no VoiceGenScript defers.
func TestRunGenerateAudio_VoiceEmptyScriptDefers(t *testing.T) {
	cfg := config.Default()
	cfg.VoiceGenScript = ""
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskGenerateAudio,
		Input:  "hola, esto es una prueba",
		Params: map[string]any{"kind": "voice"},
	})
	if res.OK || !res.Deferred {
		t.Fatalf("empty VoiceGenScript must defer, got ok=%v", res.OK)
	}
}

// TestRunGenerateAudio_MusicEmptyScriptDefers: kind=music with an empty MusicGenScript
// defers cleanly (defer-not-crash). After B3 the default is non-empty, so this clears it
// explicitly to assert the empty-route defer still holds.
func TestRunGenerateAudio_MusicEmptyScriptDefers(t *testing.T) {
	cfg := config.Default()
	cfg.MusicGenScript = ""
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskGenerateAudio,
		Input:  "upbeat corporate, 120 bpm",
		Params: map[string]any{"kind": "music"},
	})
	if res.OK || !res.Deferred {
		t.Fatalf("empty MusicGenScript (kind=music) must defer, got ok=%v", res.OK)
	}
}

// TestRunGenerateAudio_DefaultKindIsVoice: no kind param defaults to voice; with an
// empty VoiceGenScript that defers (proving the default route is voice, not music).
func TestRunGenerateAudio_DefaultKindIsVoice(t *testing.T) {
	cfg := config.Default()
	cfg.VoiceGenScript = ""
	cfg.MusicGenScript = "render/should-not-be-used.mjs" // non-empty: if default were music this would route, not defer
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateAudio, Input: "default kind test"})
	if res.OK || !res.Deferred {
		t.Fatalf("default kind must be voice and defer on empty VoiceGenScript, got ok=%v", res.OK)
	}
}

// TestRunGenerateAudio_VoiceRoutesToScript: kind=voice runs the configured (stub)
// VoiceGenScript and returns {audio_path, kind:"voice", seed}.
func TestRunGenerateAudio_VoiceRoutesToScript(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.VoiceGenScript = writeStub(t, dir)
	cfg.MediaDir = dir
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskGenerateAudio,
		Input:  "hola mundo",
		Params: map[string]any{"kind": "voice"},
	})
	if !res.OK {
		t.Fatalf("expected ok via voice stub, got defer: %s", res.Reason)
	}
	var out struct {
		AudioPath string `json:"audio_path"`
		Kind      string `json:"kind"`
		Seed      int    `json:"seed"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Kind != "voice" {
		t.Fatalf("kind = %q, want voice", out.Kind)
	}
	if out.AudioPath == "" {
		t.Fatal("result missing audio_path")
	}
	if _, err := os.Stat(out.AudioPath); err != nil {
		t.Fatalf("audio file not produced: %v", err)
	}
}

// writeArgStub writes a node stub that records its full argv to "<out>.args" (one arg
// per line) then writes the out file and exits 0 — so a routing test can assert which
// CLI flags the pipeline passed to the worker, without a live render.
func writeArgStub(t *testing.T, dir string) string {
	t.Helper()
	stub := filepath.Join(dir, "argstub.mjs")
	if err := os.WriteFile(stub, []byte(`import {writeFileSync} from "node:fs";
const argv = process.argv.slice(2);
const out = argv[0];
writeFileSync(out + ".args", argv.join("\n"));
writeFileSync(out, "stub-output");
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return stub
}

// readArgs reads the sidecar written by writeArgStub back into a slice.
func readArgs(t *testing.T, outPath string) []string {
	t.Helper()
	b, err := os.ReadFile(outPath + ".args")
	if err != nil {
		t.Fatalf("arg sidecar not written: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// hasFlagVal reports whether args contains "--<flag>" immediately followed by want.
func hasFlagVal(args []string, flag, want string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--"+flag && args[i+1] == want {
			return true
		}
	}
	return false
}

// hasFlag reports whether args contains "--<flag>" at all.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == "--"+flag {
			return true
		}
	}
	return false
}

// TestRunGenerateAudio_MusicRoutesToScriptWithSeed: kind=music routes to MusicGenScript
// and PASSES --seed (the B1 music seed gap — ACE-Step is seed-reproducible), plus
// --seconds and --lyrics. The minted/echoed seed must match the --seed value passed.
func TestRunGenerateAudio_MusicRoutesToScriptWithSeed(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.MusicGenScript = writeArgStub(t, dir)
	cfg.MediaDir = dir
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{
		Task:  core.TaskGenerateAudio,
		Input: "calm lo-fi piano, soft rain",
		Params: map[string]any{
			"kind":    "music",
			"seed":    42,
			"seconds": 8,
			"lyrics":  "la la la",
		},
	})
	if !res.OK {
		t.Fatalf("expected ok via music stub, got defer: %s", res.Reason)
	}
	var out struct {
		AudioPath string `json:"audio_path"`
		Kind      string `json:"kind"`
		Seed      int    `json:"seed"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Kind != "music" {
		t.Fatalf("kind = %q, want music", out.Kind)
	}
	if out.Seed != 42 {
		t.Fatalf("seed = %d, want 42 (caller-supplied honored)", out.Seed)
	}
	args := readArgs(t, out.AudioPath)
	if !hasFlagVal(args, "seed", "42") {
		t.Fatalf("music branch must pass --seed 42 (B1 gap); args=%v", args)
	}
	if !hasFlagVal(args, "seconds", "8") {
		t.Fatalf("music branch must pass --seconds 8; args=%v", args)
	}
	if !hasFlagVal(args, "lyrics", "la la la") {
		t.Fatalf("music branch must pass --lyrics; args=%v", args)
	}
}

// TestRunGenerateAudio_MusicMintsAndPassesSeed: with no caller seed, the music branch
// MINTS one and passes the SAME value as --seed (reported seed == the flag passed).
func TestRunGenerateAudio_MusicMintsAndPassesSeed(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.MusicGenScript = writeArgStub(t, dir)
	cfg.MediaDir = dir
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskGenerateAudio,
		Input:  "ambient drone",
		Params: map[string]any{"kind": "music"},
	})
	if !res.OK {
		t.Fatalf("expected ok via music stub, got defer: %s", res.Reason)
	}
	var out struct {
		AudioPath string `json:"audio_path"`
		Seed      int    `json:"seed"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Seed <= 0 {
		t.Fatal("a seed should have been minted for music")
	}
	args := readArgs(t, out.AudioPath)
	if !hasFlagVal(args, "seed", strconv.Itoa(out.Seed)) {
		t.Fatalf("minted seed %d must be passed as --seed; args=%v", out.Seed, args)
	}
}

// TestRunGenerateAudio_VoicePassesNoSeedFlag: the voice path is UNCHANGED — it mints a
// seed for reporting but does NOT pass a --seed flag (Chatterbox takes no seed).
func TestRunGenerateAudio_VoicePassesNoSeedFlag(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.VoiceGenScript = writeArgStub(t, dir)
	cfg.MediaDir = dir
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskGenerateAudio,
		Input:  "hola mundo",
		Params: map[string]any{"kind": "voice", "seed": 99},
	})
	if !res.OK {
		t.Fatalf("expected ok via voice stub, got defer: %s", res.Reason)
	}
	var out struct {
		AudioPath string `json:"audio_path"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	args := readArgs(t, out.AudioPath)
	if hasFlag(args, "seed") {
		t.Fatalf("voice path must NOT pass --seed (Chatterbox has no seed); args=%v", args)
	}
}

// TestMusicGenScriptDefaultWired: the B3 wiring — MusicGenScript now defaults to the
// comfy-music worker (no longer "" → defer). This is what activates kind=music.
func TestMusicGenScriptDefaultWired(t *testing.T) {
	if got := config.Default().MusicGenScript; got != "render/comfy-music.mjs" {
		t.Fatalf("MusicGenScript default = %q, want render/comfy-music.mjs", got)
	}
}

// TestRunGenerateAudio_UnknownKindDefers: an unrecognized kind defers cleanly.
func TestRunGenerateAudio_UnknownKindDefers(t *testing.T) {
	cfg := config.Default()
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskGenerateAudio,
		Input:  "x",
		Params: map[string]any{"kind": "podcast"},
	})
	if res.OK || !res.Deferred {
		t.Fatalf("unknown audio kind must defer, got ok=%v", res.OK)
	}
}

// TestRunGenerateVideo_MissingScriptDefersDistinctly (LO-2): the shipped
// RELATIVE default script resolves against the exe dir (not the cwd), and when
// the file is absent the defer reason is the distinct "script not found at
// <absolute-path>" — not a generic node MODULE_NOT_FOUND failure. The test
// binary's exe dir has no render/ tree, so the default must miss.
func TestRunGenerateVideo_MissingScriptDefersDistinctly(t *testing.T) {
	cfg := config.Default() // VideoGenScript = "render/comfy-video.mjs" (relative)
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateVideo, Input: "a calm ocean at dawn"})
	if res.OK || !res.Deferred {
		t.Fatalf("missing script must defer, got ok=%v", res.OK)
	}
	if !strings.HasPrefix(res.Reason, "script not found at ") {
		t.Fatalf("reason = %q, want prefix 'script not found at '", res.Reason)
	}
	if !filepath.IsAbs(strings.TrimPrefix(res.Reason, "script not found at ")) {
		t.Fatalf("reason must carry the ABSOLUTE tried path, got %q", res.Reason)
	}
}

// TestRunGenerateAudio_MissingVoiceScriptDefersDistinctly (LO-2): same distinct
// defer for the voice route's relative default (render/tts.mjs).
func TestRunGenerateAudio_MissingVoiceScriptDefersDistinctly(t *testing.T) {
	cfg := config.Default() // VoiceGenScript = "render/tts.mjs" (relative)
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskGenerateAudio,
		Input:  "hola, esto es una prueba",
		Params: map[string]any{"kind": "voice"},
	})
	if res.OK || !res.Deferred {
		t.Fatalf("missing voice script must defer, got ok=%v", res.OK)
	}
	if !strings.HasPrefix(res.Reason, "script not found at ") {
		t.Fatalf("reason = %q, want prefix 'script not found at '", res.Reason)
	}
}

// TestRunGenerateAudio_FinetunedRoutesWithArgs: voice=finetuned, fully configured →
// argv carries --engine finetuned + model/base-dir/clone(ft ref)/recipe. Model-agnostic:
// the paths come from config, never hardcoded.
func TestRunGenerateAudio_FinetunedRoutesWithArgs(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.VoiceGenScript = writeArgStub(t, dir)
	cfg.MediaDir = dir
	cfg.VoiceGenFTModel = "/m/merged.safetensors"
	cfg.VoiceGenFTBaseDir = "/m/base"
	cfg.VoiceGenFTRef = "/r/dan.wav"
	cfg.VoiceGenFTTemperature = 0.6
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskGenerateAudio,
		Input:  "hola mundo",
		Params: map[string]any{"kind": "voice", "voice": "finetuned"},
	})
	if !res.OK {
		t.Fatalf("expected ok via ft stub, got defer: %s", res.Reason)
	}
	var out struct {
		AudioPath string `json:"audio_path"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	args := readArgs(t, out.AudioPath)
	if !hasFlagVal(args, "engine", "finetuned") {
		t.Fatalf("ft path must pass --engine finetuned; args=%v", args)
	}
	if !hasFlagVal(args, "model", "/m/merged.safetensors") {
		t.Fatalf("ft path must pass --model from config; args=%v", args)
	}
	if !hasFlagVal(args, "base-dir", "/m/base") {
		t.Fatalf("ft path must pass --base-dir from config; args=%v", args)
	}
	if !hasFlagVal(args, "clone", "/r/dan.wav") {
		t.Fatalf("ft path must pass the ft ref via --clone; args=%v", args)
	}
	if !hasFlagVal(args, "temperature", "0.6") {
		t.Fatalf("ft path must pass configured recipe knobs; args=%v", args)
	}
}

// TestRunGenerateAudio_FinetunedUnconfiguredDefers: voice=finetuned with no FT model
// configured defers cleanly (never cloud), even though the (generalist) script exists.
func TestRunGenerateAudio_FinetunedUnconfiguredDefers(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.VoiceGenScript = writeStub(t, dir) // present, but FT model/base unset
	cfg.MediaDir = dir
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskGenerateAudio,
		Input:  "hola",
		Params: map[string]any{"kind": "voice", "voice": "finetuned"},
	})
	if res.OK || !res.Deferred {
		t.Fatalf("finetuned with no config must defer, got ok=%v", res.OK)
	}
	if !strings.Contains(res.Reason, "no fine-tuned voice configured") {
		t.Fatalf("reason = %q, want 'no fine-tuned voice configured'", res.Reason)
	}
}

// TestRunGenerateAudio_GeneralistInjectsConfigRef: generalist (default) with no request
// clone injects the machine's VoiceGenRef as --clone.
func TestRunGenerateAudio_GeneralistInjectsConfigRef(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.VoiceGenScript = writeArgStub(t, dir)
	cfg.MediaDir = dir
	cfg.VoiceGenRef = "/r/gen.wav"
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskGenerateAudio,
		Input:  "hola",
		Params: map[string]any{"kind": "voice"}, // no voice, no clone
	})
	if !res.OK {
		t.Fatalf("expected ok, got defer: %s", res.Reason)
	}
	var out struct {
		AudioPath string `json:"audio_path"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	args := readArgs(t, out.AudioPath)
	if !hasFlagVal(args, "clone", "/r/gen.wav") {
		t.Fatalf("generalist must inject VoiceGenRef as --clone; args=%v", args)
	}
}

// TestRunGenerateAudio_UnknownVoiceDefers: an unrecognized voice value defers.
func TestRunGenerateAudio_UnknownVoiceDefers(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.VoiceGenScript = writeStub(t, dir)
	cfg.MediaDir = dir
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskGenerateAudio,
		Input:  "hola",
		Params: map[string]any{"kind": "voice", "voice": "podcast"},
	})
	if res.OK || !res.Deferred {
		t.Fatalf("unknown voice must defer, got ok=%v", res.OK)
	}
}
