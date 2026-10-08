package pipeline

// The amd-gcn tier seeds the animate geometry (288x512) and its latent-token cap (5800, stride 8) to the
// measured 33-frame envelope. The lane's default frame count used to be the runner's own 49, and 288x512x49
// plus the VACE reference frame is 8,064 tokens, so on a node seeded from that tier every animate call that
// named no frames (offload_animate_character, the animate-character verb, a delegator's animate job) was
// refused token_cap_exceeded before the runner started (release 0.172.0 review, REL1). animategen_frames is
// the key that carries the default clip. These tests resolve the REAL seed, so the cap and the default
// cannot drift apart without a failure here.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/tierseed"
)

// seededAmdGcnAnimate resolves the amd-gcn config_seed the way a fresh Linux install does, loads it as a
// config, places the two engine binaries the lane stats under the install home and points the runner at a
// stub that records its argv. dir is the media directory; the lease state is private to the test.
func seededAmdGcnAnimate(t *testing.T) (config.Config, string) {
	t.Helper()
	profiles, err := tierseed.Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	prof, ok := profiles["amd-gcn"]
	if !ok {
		t.Fatal("tier amd-gcn not found - this gate went blind")
	}
	seed, err := tierseed.Resolve(prof, "amd-gcn", tierseed.Options{Home: t.TempDir(), GOOS: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("the resolved amd-gcn seed does not load as a config: %v", err)
	}
	if cfg.AnimateGenEngine != config.EngineSdcpp || cfg.AnimateGenSdcppMaxTokens == 0 {
		t.Fatalf("the amd-gcn seed no longer binds the sdcpp animate lane with a token cap (engine %q cap %d): this gate went blind",
			cfg.AnimateGenEngine, cfg.AnimateGenSdcppMaxTokens)
	}
	for _, bin := range []string{cfg.AnimateGenSdcppBin, cfg.AnimateGenDepthBin} {
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	cfg.MediaDir = dir
	cfg.AnimateGenSdcppScript = writeIGPUArgStub(t, dir)
	isolateLease(&cfg, dir)
	return cfg, dir
}

// A default animate request on the seeded box passes the seeded cap and the runner is told the seeded clip
// length, not left to render its own 49 frames.
func TestTheSeededAmdGcnDefaultAnimateRequestFitsItsTokenCap(t *testing.T) {
	requireNodePipeline(t)
	cfg, dir := seededAmdGcnAnimate(t)
	if cfg.AnimateGenFrames != 33 {
		t.Fatalf("the seed's animategen_frames = %d, want the measured 33", cfg.AnimateGenFrames)
	}
	p := &Pipeline{cfg: cfg}
	// no frames, no width, no height: everything comes from the seed
	out, _, _ := decodeVideo(t, p.Run(context.Background(), animateReq(dir, nil)))
	args := readArgs(t, out)
	for k, want := range map[string]string{"frames": "33", "width": "288", "height": "512", "max-tokens": "5800", "vae-stride": "8"} {
		if got := igpuFlag(args, k); got != want {
			t.Errorf("--%s = %q, want %q (args %v)", k, got, want, args)
		}
	}
	// a request that names its own frames wins over the default, and is normalised like any other (4k+1)
	out2, _, _ := decodeVideo(t, p.Run(context.Background(), animateReq(dir, map[string]any{"frames": 18})))
	if got := igpuFlag(readArgs(t, out2), "frames"); got != "17" {
		t.Errorf("a request for 18 frames reached the runner as --frames %q, want the nearest 4k+1 (17)", got)
	}
}

// Frames over the cap are still refused, and before the media lease is taken: with the node's lease held by
// another job the over-cap request is answered token_cap_exceeded, where a request that fits is answered
// gpu_busy by that same held lease.
func TestAnAnimateRequestOverTheSeededCapIsStillRefusedBeforeTheLease(t *testing.T) {
	requireNodePipeline(t)
	cfg, dir := seededAmdGcnAnimate(t)
	cfg.GPUWaitMs = 50 // the control request below waits this long for the lease it will not get
	m, err := gpulease.OpenAt(cfg.GPULockPath, cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "another job", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()
	p := &Pipeline{cfg: cfg}

	res := p.Run(context.Background(), animateReq(dir, map[string]any{"frames": 49}))
	mustDeferWith(t, res, "token_cap_exceeded", "TOKEN_CAP_EXCEEDED", "288x512x49", "needs 8064 latent tokens", "cap is 5800", "up to 33 frames fit")
	if _, err := os.Stat(filepath.Join(dir, "a.mp4")); err == nil {
		t.Error("the runner ran although the request was over the cap")
	}
	// control: the lease really is held, so the order above is the order the lane checks in
	control := p.Run(context.Background(), animateReq(dir, nil))
	if control.OK || control.Meta.ErrClass != "gpu_busy" {
		t.Fatalf("a request that fits must meet the held lease (gpu_busy), got ok=%v class=%q: %s", control.OK, control.Meta.ErrClass, control.Reason)
	}
}

// A box that does not set the key behaves exactly as before: the runner is sent no --frames, the cap is
// checked on the runner's own default of 49, and so the seeded geometry without the key is refused for it.
func TestABoxWithoutAnimategenFramesKeepsTheRunnersDefault(t *testing.T) {
	requireNodePipeline(t)
	// no cap, no key: the argv carries no --frames at all
	dir := t.TempDir()
	cfg := animateCfg(t, dir)
	out, _, _ := decodeVideo(t, (&Pipeline{cfg: cfg}).Run(context.Background(), animateReq(dir, nil)))
	if args := readArgs(t, out); hasFlag(args, "frames") {
		t.Errorf("a box without animategen_frames must leave the frame count to the runner, got %v", args)
	}
	// the seeded geometry and cap, key removed: the default is the runner's 49 and the cap refuses it
	seeded, sdir := seededAmdGcnAnimate(t)
	seeded.AnimateGenFrames = 0
	res := (&Pipeline{cfg: seeded}).Run(context.Background(), animateReq(sdir, nil))
	mustDeferWith(t, res, "token_cap_exceeded", "288x512x49", "needs 8064 latent tokens", "cap is 5800")
}

// The ComfyUI animate route does not read the key (it has its own 81-frame chunk): setting it on a box that
// animates through ComfyUI changes nothing.
func TestAnimategenFramesDoesNotTouchTheComfyUIAnimateRoute(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.MediaDir = dir
	isolateLease(&cfg, dir)
	cfg.AnimateGenScript = writeArgStub(t, dir)
	cfg.AnimateGenFrames = 33
	out := filepath.Join(dir, "c.mp4")
	res := (&Pipeline{cfg: cfg}).Run(context.Background(), core.Request{
		Task: core.TaskAnimateCharacter, Input: "a knight", Image: "ref.png", Video: "drive.mp4",
		Params: map[string]any{"out": out, "seed": 4},
	})
	if path, _, _ := decodeVideo(t, res); path != out {
		t.Fatalf("video_path = %q, want %q", path, out)
	}
	// the exact argv the route produces with no engine key (TestEveryRouteWithNoEngineKeyKeepsItsExactArgv)
	want := []string{out, "ref.png", "drive.mp4", "a knight", "--seed", "4"}
	if got := readArgs(t, out); !reflect.DeepEqual(got, want) {
		t.Errorf("the ComfyUI animate argv changed with animategen_frames set:\n got %v\nwant %v", got, want)
	}
}
