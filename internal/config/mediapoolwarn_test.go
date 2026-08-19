package config

// Coverage for the pooled-media launch-flag warnings. Both pooled seats depend on
// ComfyUI being launched with --disable-dynamic-vram: without it DynamicVRAM
// shadows the DisTorch2 hook (MultiGPU #191) and the graph loads single-card
// WHILE the pool allocation banner still prints, so a misconfigured box is
// indistinguishable from a pooled one at every observable surface.
//
// The image half has warned since 0.59.0 but was never covered by a test (it was
// hand-fired once against a built binary). The video half did not exist: the 2x16
// tier seeds videogen_pool_vvram_gb=30 and had no check at all.
//
// Every case below asserts BOTH directions. A warning that cannot stay silent is
// as useless as one that never fires, and only the silent arm can prove the
// condition is actually being read.

import (
	"bytes"
	"strings"
	"testing"
)

const (
	imgWarn = "pooled image seat configured but COMFY_EXTRA_ARGS"
	vidWarn = "pooled video seat configured but COMFY_EXTRA_ARGS"
)

func warnOut(t *testing.T, c Config) string {
	t.Helper()
	var buf bytes.Buffer
	warnMediaGenBindingTrapsTo(c, &buf)
	return buf.String()
}

func TestPooledVideoSeatWarnsWithoutDisableDynamicVRAM(t *testing.T) {
	t.Setenv("COMFY_EXTRA_ARGS", "")
	out := warnOut(t, Config{VideoGenPoolVvramGB: 30})
	if !strings.Contains(out, vidWarn) {
		t.Fatalf("pooled video seat with the flag absent must warn, got %q", out)
	}

	// Silent arm: the flag present must silence it, or the check is not reading
	// the env at all.
	t.Setenv("COMFY_EXTRA_ARGS", "--disable-dynamic-vram")
	if out := warnOut(t, Config{VideoGenPoolVvramGB: 30}); strings.Contains(out, vidWarn) {
		t.Fatalf("flag present must silence the video warning, got %q", out)
	}
}

func TestPooledImageSeatWarnsWithoutDisableDynamicVRAM(t *testing.T) {
	t.Setenv("COMFY_EXTRA_ARGS", "")
	// family krea2 keeps the unrelated wrong-family warning quiet so this test
	// asserts on the launch-flag warning alone.
	out := warnOut(t, Config{ImageGenPoolVvramGB: 12, ImageGenFamily: "krea2", ImageGenSteps: 8, ImageGenCFG: 1.0})
	if !strings.Contains(out, imgWarn) {
		t.Fatalf("pooled image seat with the flag absent must warn, got %q", out)
	}

	t.Setenv("COMFY_EXTRA_ARGS", "--disable-dynamic-vram")
	out = warnOut(t, Config{ImageGenPoolVvramGB: 12, ImageGenFamily: "krea2", ImageGenSteps: 8, ImageGenCFG: 1.0})
	if strings.Contains(out, imgWarn) {
		t.Fatalf("flag present must silence the image warning, got %q", out)
	}
}

// The two seats are independent: pooling video only must NOT emit the image
// warning and vice versa. Before this change the video seat borrowed nothing from
// the image check, and a naive fix that ORed the two vvram values together would
// have emitted the wrong seat's warning — which sends the reader to the wrong
// config key.
func TestPooledSeatWarningsAreIndependent(t *testing.T) {
	t.Setenv("COMFY_EXTRA_ARGS", "")

	videoOnly := warnOut(t, Config{VideoGenPoolVvramGB: 30})
	if strings.Contains(videoOnly, imgWarn) {
		t.Fatalf("video-only pooling must not warn about the IMAGE seat, got %q", videoOnly)
	}

	imageOnly := warnOut(t, Config{ImageGenPoolVvramGB: 12, ImageGenFamily: "krea2", ImageGenSteps: 8, ImageGenCFG: 1.0})
	if strings.Contains(imageOnly, vidWarn) {
		t.Fatalf("image-only pooling must not warn about the VIDEO seat, got %q", imageOnly)
	}

	// Both pooled, flag absent => both warnings. This is the live 2x16 shape
	// (imagegen_pool_vvram_gb=12 + videogen_pool_vvram_gb=30).
	both := warnOut(t, Config{
		ImageGenPoolVvramGB: 12, ImageGenFamily: "krea2", ImageGenSteps: 8, ImageGenCFG: 1.0,
		VideoGenPoolVvramGB: 30,
	})
	if !strings.Contains(both, imgWarn) || !strings.Contains(both, vidWarn) {
		t.Fatalf("both seats pooled with the flag absent must warn twice, got %q", both)
	}
}

// Unpooled is the common case for every single-GPU tier: no pool keys, no
// warnings, regardless of the env. A warner that fires on an unpooled box would
// train the operator to ignore it.
func TestUnpooledSeatsNeverWarnAboutTheLaunchFlag(t *testing.T) {
	t.Setenv("COMFY_EXTRA_ARGS", "")
	out := warnOut(t, Config{ImageGenFamily: "krea2", ImageGenSteps: 8, ImageGenCFG: 1.0})
	if strings.Contains(out, imgWarn) || strings.Contains(out, vidWarn) {
		t.Fatalf("no pooling configured must produce no launch-flag warning, got %q", out)
	}
}
