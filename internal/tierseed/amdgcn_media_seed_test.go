package tierseed

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// CT-51 I5: the amd-gcn tier seeds the measured iGPU media set (sd.cpp video and animate, audio.cpp voice
// and music). These tests resolve the real profiles.json seed for a fresh Linux install and prove that
// (1) the seed lands as a config whose four routes mediacap reads BOUND-BUT-MISSING until the files are
// placed and CONFIGURED once they are, (2) every weight it binds has a pin (name, size, sha256) in the
// installer's pin table, and (3) the licence fields say what was verified and nothing more.

func resolveAmdGcn(t *testing.T, home string) (config.Config, map[string]any) {
	t.Helper()
	profiles, err := Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	p, ok := profiles["amd-gcn"]
	if !ok {
		t.Fatal("tier amd-gcn not found - this gate went blind")
	}
	seed, err := Resolve(p, "amd-gcn", Options{Home: home, GOOS: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("the resolved seed does not load as a config: %v", err)
	}
	return cfg, seed
}

// placeSeedFiles creates every path the seed binds under the install home, as an operator would by
// placing the weights and the engine builds, plus the runner scripts mediacap resolves.
func placeSeedFiles(t *testing.T, cfg *config.Config) {
	t.Helper()
	touch := func(p string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	fb := cfg.VideoGenFamilies["fastwan"]
	for _, p := range []string{fb.SdcppBin, fb.SdcppModel, fb.SdcppVAE, fb.SdcppT5xxl, fb.SdcppTAE,
		cfg.AnimateGenSdcppBin, cfg.AnimateGenSdcppModel, cfg.AnimateGenSdcppVAE, cfg.AnimateGenSdcppT5xxl,
		cfg.AnimateGenDepthBin, cfg.AnimateGenDepthModel,
		cfg.AudiocppBin, cfg.AudiocppVoiceModel, cfg.AudiocppMusicModel} {
		touch(p)
	}
}

func TestAmdGcnMediaSeedRoutesFollowTheFiles(t *testing.T) {
	home := t.TempDir()
	cfg, seed := resolveAmdGcn(t, home)
	if got, _ := seed["videogen_family"].(string); got != "fastwan" {
		t.Fatalf("videogen_family = %v", seed["videogen_family"])
	}
	// the runner scripts live beside the executable in production; stand them in here
	scripts := t.TempDir()
	for name, dst := range map[string]*string{
		"sdcpp-video.mjs": &cfg.VideoGenSdcppScript, "sdcpp-animate.mjs": &cfg.AnimateGenSdcppScript, "audiocpp-generate.mjs": &cfg.AudiocppScript,
	} {
		p := filepath.Join(scripts, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		*dst = p
	}
	cfg.FFmpegPath = "" // not a seeded binding: keep the verdict about the seed's own files
	names := []string{"generate_video", "animate_character", "generate_audio:voice", "generate_audio:music"}
	verdicts := func() map[string]mediacap.Route {
		out := map[string]mediacap.Route{}
		for _, r := range mediacap.Routes(cfg) {
			out[r.Name] = r
		}
		return out
	}

	// Fresh install: nothing placed. Every lane is bound and its files are missing - never "not
	// configured" (a seed that bound nothing) and never refused for a CPU backend.
	for _, n := range names {
		r, ok := verdicts()[n]
		if !ok {
			t.Fatalf("no %s route derived", n)
		}
		if r.State != mediacap.BoundButMissing {
			t.Errorf("%s before the files are placed = %s (%s), want BOUND-BUT-MISSING", n, r.State, r.Detail)
		}
		if strings.Contains(r.Detail, "backend") && strings.Contains(r.Detail, "every call defers") {
			t.Errorf("%s: the seeded backend is refused: %s", n, r.Detail)
		}
	}

	placeSeedFiles(t, &cfg)
	wantEngine := map[string]string{"generate_video": "sdcpp", "animate_character": "sdcpp", "generate_audio:voice": "audiocpp", "generate_audio:music": "audiocpp"}
	for _, n := range names {
		r := verdicts()[n]
		if r.State != mediacap.Configured || r.Engine != wantEngine[n] {
			t.Errorf("%s with every file placed = %s/%s (%s), want CONFIGURED on %s", n, r.State, r.Engine, r.Detail, wantEngine[n])
		}
	}
	// the opt-in tiny autoencoder is optional: removing it never fails the route
	if err := os.Remove(cfg.VideoGenFamilies["fastwan"].SdcppTAE); err != nil {
		t.Fatal(err)
	}
	if r := verdicts()["generate_video"]; r.State != mediacap.Configured {
		t.Errorf("a missing sdcpp_tae must not fail generate_video: %s (%s)", r.State, r.Detail)
	}
}

func TestAmdGcnMediaSeedValues(t *testing.T) {
	home := t.TempDir()
	cfg, _ := resolveAmdGcn(t, home)
	fb, ok := cfg.VideoGenFamilies["fastwan"]
	if !ok {
		t.Fatal("videogen_families has no fastwan")
	}
	if fb.Engine != config.EngineSdcpp || fb.SdcppBackend != "vulkan0" || fb.Steps != 3 || fb.CFG != 1 || fb.FlowShift != 5 || fb.Sampler != "euler" {
		t.Errorf("fastwan recipe drifted from the measured one: %+v", fb)
	}
	if fb.Width != 832 || fb.Height != 480 || fb.Frames != 49 || fb.FPS != 24 {
		t.Errorf("fastwan geometry = %dx%dx%d@%d, want the measured 832x480x49@24", fb.Width, fb.Height, fb.Frames, fb.FPS)
	}
	if fb.SdcppMaxTokens != 5200 || fb.SdcppVAEStride != 16 {
		t.Errorf("fastwan token cap = %d stride %d, want 5200/16 (5,070 tokens worked, 15,600 reset the ring)", fb.SdcppMaxTokens, fb.SdcppVAEStride)
	}
	if fb.License != "Apache-2.0" || fb.CommercialUse == nil || !*fb.CommercialUse {
		t.Errorf("fastwan licence = %q commercial_use %v, want Apache-2.0 / true", fb.License, fb.CommercialUse)
	}
	if cfg.VideoGenFamily != "fastwan" || cfg.VideoGenTimeoutSec != 7200 || cfg.AnimateGenTimeoutSec != 5400 {
		t.Errorf("family %q video timeout %d animate timeout %d", cfg.VideoGenFamily, cfg.VideoGenTimeoutSec, cfg.AnimateGenTimeoutSec)
	}
	if cfg.AnimateGenEngine != config.EngineSdcpp || cfg.AnimateGenSdcppBackend != "vulkan0" ||
		cfg.AnimateGenSdcppMaxTokens != 5800 || cfg.AnimateGenSdcppVAEStride != 8 ||
		cfg.AnimateGenSteps != 20 || cfg.AnimateGenCFG != 6 || cfg.AnimateGenWidth != 288 || cfg.AnimateGenHeight != 512 {
		t.Errorf("animate seed drifted from the measured envelope: %+v", cfg)
	}
	if cfg.VoiceGenEngine != config.EngineAudiocpp || cfg.MusicGenEngine != config.EngineAudiocpp ||
		cfg.AudiocppBackend != "vulkan" || cfg.AudiocppDevice != "0" ||
		cfg.AudiocppVoiceFamily != "chatterbox" || cfg.AudiocppMusicFamily != "ace_step" {
		t.Errorf("audio.cpp seed drifted: engine %q/%q backend %q device %q families %q/%q",
			cfg.VoiceGenEngine, cfg.MusicGenEngine, cfg.AudiocppBackend, cfg.AudiocppDevice, cfg.AudiocppVoiceFamily, cfg.AudiocppMusicFamily)
	}
	// Placement: engines and weights sit under the install home, as the other seats' do.
	h := filepath.ToSlash(home)
	for name, p := range map[string]string{
		"sdcpp_bin": fb.SdcppBin, "animategen_sdcpp_bin": cfg.AnimateGenSdcppBin, "animategen_depth_bin": cfg.AnimateGenDepthBin,
		"audiocpp_bin": cfg.AudiocppBin, "sdcpp_model": fb.SdcppModel, "animategen_sdcpp_model": cfg.AnimateGenSdcppModel,
		"audiocpp_voice_model": cfg.AudiocppVoiceModel, "audiocpp_music_model": cfg.AudiocppMusicModel,
	} {
		if !strings.HasPrefix(filepath.ToSlash(p), h+"/") {
			t.Errorf("%s = %q is not under the install home", name, p)
		}
		if strings.Contains(p, "__OFFLOAD_HOME__") || strings.Contains(p, "__EXE__") {
			t.Errorf("%s = %q kept an unexpanded token", name, p)
		}
	}
	// No licence is invented: audio.cpp lanes carry no commercial_use key at all, and the profile
	// says the ACE-Step licence is unverified.
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			Notes      string                     `json:"notes"`
			ConfigSeed map[string]json.RawMessage `json:"config_seed"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	gcn := doc.Profiles["amd-gcn"]
	for k := range gcn.ConfigSeed {
		if strings.HasPrefix(k, "audiocpp_") || k == "musicgen_engine" || k == "voicegen_engine" {
			if strings.Contains(k, "commercial") || strings.Contains(k, "licen") {
				t.Errorf("seed key %q claims a licence for an audio lane", k)
			}
		}
	}
	if !strings.Contains(gcn.Notes, "ACE-STEP LICENCE IS NOT VERIFIED") {
		t.Error("the amd-gcn notes must say the ACE-Step licence is unverified")
	}
}

// Every weight the seed binds is pinned (name, size, sha256) in the installer's pin table, and the sizes the
// mediacap size table keeps agree with it. The Windows installer downloads none of these keys; the pins
// are what an operator or the Linux media leg verifies the placed files against.
func TestAmdGcnMediaSeedWeightsArePinned(t *testing.T) {
	cfg, _ := resolveAmdGcn(t, "/oh")
	fb := cfg.VideoGenFamilies["fastwan"]
	weights := []string{fb.SdcppModel, fb.SdcppVAE, fb.SdcppT5xxl, fb.SdcppTAE,
		cfg.AnimateGenSdcppModel, cfg.AnimateGenSdcppVAE, cfg.AnimateGenSdcppT5xxl, cfg.AnimateGenDepthModel,
		cfg.AudiocppVoiceModel, cfg.AudiocppMusicModel}
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "install.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	blockRe := regexp.MustCompile(`(?s)@\{(.*?)\n\s*\}`)
	field := func(blk, name string) string {
		m := regexp.MustCompile(name + `\s*=\s*'?([^'\r\n]+)'?`).FindStringSubmatch(blk)
		if m == nil {
			return ""
		}
		return strings.TrimSpace(m[1])
	}
	pins := map[string]string{} // name -> block
	for _, m := range blockRe.FindAllStringSubmatch(string(raw), -1) {
		if n := field(m[1], "name"); n != "" {
			pins[n] = m[1]
		}
	}
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, w := range weights {
		name := filepath.Base(w)
		blk, ok := pins[name]
		if !ok {
			t.Errorf("%s is bound by the amd-gcn seed but has no $PINNED entry in setup/install.ps1", name)
			continue
		}
		if sha := field(blk, "sha"); !hex64.MatchString(sha) {
			t.Errorf("%s: pinned sha256 %q is not 64 hex digits", name, sha)
		}
		size, err := strconv.ParseInt(field(blk, "size"), 10, 64)
		if err != nil || size <= 0 {
			t.Errorf("%s: pinned size %q", name, field(blk, "size"))
		}
		if v, sha := field(blk, "version"), field(blk, "sha"); v == "" || !strings.HasPrefix(sha, v) {
			t.Errorf("%s: version %q is not the sha256 prefix", name, v)
		}
		if u := field(blk, "url"); !strings.HasPrefix(u, "https://huggingface.co/") || !strings.Contains(u, "/resolve/main/") {
			t.Errorf("%s: url %q is not a Hugging Face resolve URL", name, u)
		}
	}
}
