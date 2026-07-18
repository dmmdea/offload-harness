package pipeline

// Passive fleet footprint glue (docs/FLEET-NODE.md): the GPU render paths must
// thread a non-nil gpugen sampling hook keyed by THIS machine's bindings, and
// a successful sampled render must land in the shared footprint store. The
// E2E tests drive the real runGenerateImage/runGenerateVideo paths with the
// GPU-free node stub + an injected sampler (p.fleetSample), proving the hook
// is wired all the way through imagegen/gpugen — not just composed.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
)

// footprintTestPipeline builds a pipeline whose footprint store is isolated in
// a temp dir (the store path derives from LedgerPath's directory) and whose
// per-render sampler is a fake reporting a constant peak.
func footprintTestPipeline(t *testing.T, cfg config.Config, peakGiB float64) *Pipeline {
	t.Helper()
	cfg.LedgerPath = filepath.Join(t.TempDir(), "ledger.jsonl")
	p := &Pipeline{cfg: cfg}
	p.fleetSample = func(childPid int) (float64, error) { return peakGiB, nil }
	return p
}

func findEntry(t *testing.T, entries []fleetnode.FootprintEntry, family, task string) fleetnode.FootprintEntry {
	t.Helper()
	for _, e := range entries {
		if e.ModelFamily == family && e.TaskType == task {
			return e
		}
	}
	t.Fatalf("no (%s, %s) entry in %#v", family, task, entries)
	return fleetnode.FootprintEntry{}
}

// TestImageFootprintKey: the image render's footprint identity follows the
// machine binding — imagegen_family (else sdxl), quant bf16 only for the
// HiDream-O1 checkpoint binding.
func TestImageFootprintKey(t *testing.T) {
	cases := []struct {
		family     string
		wantFamily string
		wantQuant  string
	}{
		{"", "sdxl", ""},
		{"hidream-o1", "hidream-o1", "bf16"},
		{"hidream-o1-dev", "hidream-o1-dev", "bf16"},
		{"some-other-dit", "some-other-dit", ""},
	}
	for _, c := range cases {
		cfg := config.Default()
		cfg.ImageGenFamily = c.family
		fam, quant := imageFootprintKey(cfg)
		if fam != c.wantFamily || quant != c.wantQuant {
			t.Errorf("imageFootprintKey(family=%q) = (%q, %q), want (%q, %q)",
				c.family, fam, quant, c.wantFamily, c.wantQuant)
		}
	}
}

// TestVideoFootprintQuant: q8_0 only when the bound Wan expert weights are the
// Q8_0 GGUFs (either unet; case-insensitive), else node default.
func TestVideoFootprintQuant(t *testing.T) {
	cfg := config.Default()
	if q := videoFootprintQuant(cfg); q != "" {
		t.Errorf("unbound unets: quant = %q, want \"\"", q)
	}
	cfg.VideoGenUnetHigh = "wan2.2_i2v_high_noise_14B_Q8_0.gguf"
	if q := videoFootprintQuant(cfg); q != "q8_0" {
		t.Errorf("Q8_0 high unet: quant = %q, want \"q8_0\"", q)
	}
	cfg.VideoGenUnetHigh = "wan2.2_i2v_high_noise_14B_fp8_scaled.safetensors"
	cfg.VideoGenUnetLow = "wan2.2_i2v_low_noise_14B_q8_0.gguf"
	if q := videoFootprintQuant(cfg); q != "q8_0" {
		t.Errorf("q8_0 low unet (lowercase): quant = %q, want \"q8_0\"", q)
	}
	cfg.VideoGenUnetLow = "wan2.2_i2v_low_noise_14B_fp8_scaled.safetensors"
	if q := videoFootprintQuant(cfg); q != "" {
		t.Errorf("fp8 binding: quant = %q, want \"\" (node default)", q)
	}
}

// TestRunGraphFootprintFamily: payload-declared model_family wins; absent =
// the generic comfy-graph bucket.
func TestRunGraphFootprintFamily(t *testing.T) {
	if f := runGraphFootprintFamily(map[string]any{"model_family": "flux-dev"}); f != "flux-dev" {
		t.Errorf("declared family = %q, want \"flux-dev\"", f)
	}
	if f := runGraphFootprintFamily(nil); f != "comfy-graph" {
		t.Errorf("absent family = %q, want \"comfy-graph\"", f)
	}
}

// TestFootprintSampling_Composition: the composed hook carries the exact key,
// a sampler, and a callback that records into the shared store with the x1.2
// margin.
func TestFootprintSampling_Composition(t *testing.T) {
	p := footprintTestPipeline(t, config.Default(), 0)
	s := p.footprintSampling("sdxl", "", "image-gen")
	if s == nil || s.Footprint == nil || s.SampleFunc == nil || s.OnFootprint == nil {
		t.Fatalf("sampling incomplete: %#v", s)
	}
	if s.Footprint.Family != "sdxl" || s.Footprint.Quant != "" || s.Footprint.Task != "image-gen" {
		t.Fatalf("key = %#v", s.Footprint)
	}
	s.OnFootprint(3.0)
	e := findEntry(t, p.FootprintStore().Entries(), "sdxl", "image-gen")
	if e.VramPeakGiB != 3.6 { // 3.0 x 1.2
		t.Errorf("vram_peak_gb = %v, want 3.6", e.VramPeakGiB)
	}
}

// TestRunGenerateImage_RecordsFootprint: an E2E render through the real image
// path (stub script) with an injected sampler lands a (family, task) entry in
// the store — the imagegen call passes a non-nil Footprint with the right
// family when the route is configured.
func TestRunGenerateImage_RecordsFootprint(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.ImageGenScript = writeStub(t, dir)
	cfg.MediaDir = dir
	p := footprintTestPipeline(t, cfg, 2.0)

	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateImage, Input: "a gray sphere"})
	if !res.OK {
		t.Fatalf("expected ok via stub, got defer: %s", res.Reason)
	}
	e := findEntry(t, p.FootprintStore().Entries(), "sdxl", "image-gen")
	if e.Quant != "" {
		t.Errorf("quant = %q, want \"\" (no HiDream binding)", e.Quant)
	}
	if e.VramPeakGiB != 2.4 { // 2.0 observed x 1.2
		t.Errorf("vram_peak_gb = %v, want 2.4", e.VramPeakGiB)
	}
}

// TestRunGenerateVideo_RecordsFootprint: same E2E proof for the video path —
// family wan2.2, quant from the unet binding.
func TestRunGenerateVideo_RecordsFootprint(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.VideoGenScript = writeStub(t, dir)
	cfg.VideoGenUnetHigh = "wan2.2_i2v_high_noise_14B_Q8_0.gguf"
	cfg.MediaDir = dir
	p := footprintTestPipeline(t, cfg, 5.5)

	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateVideo, Input: "a calm ocean at dawn"})
	if !res.OK {
		t.Fatalf("expected ok via stub, got defer: %s", res.Reason)
	}
	e := findEntry(t, p.FootprintStore().Entries(), "wan2.2", "video-gen")
	if e.Quant != "q8_0" {
		t.Errorf("quant = %q, want \"q8_0\"", e.Quant)
	}
	if e.VramPeakGiB != 6.6 { // 5.5 x 1.2
		t.Errorf("vram_peak_gb = %v, want 6.6", e.VramPeakGiB)
	}
}

// TestGlobalDeltaSampleFunc: the fallback sampler's baseline is captured on
// the first call (render start) and later samples report the positive delta.
func TestGlobalDeltaSampleFunc(t *testing.T) {
	outputs := []string{"16384, 1024", "16384, 5120", "16384, 512"}
	i := 0
	sample := globalDeltaSampleFunc(func() (string, error) {
		out := outputs[i]
		if i < len(outputs)-1 {
			i++
		}
		return out, nil
	})
	if g, err := sample(1); err != nil || g != 0 {
		t.Fatalf("baseline call = (%v, %v), want (0, nil)", g, err)
	}
	if g, _ := sample(1); g != 4.0 { // (5120-1024) MiB = 4 GiB
		t.Errorf("delta = %v GiB, want 4.0", g)
	}
	if g, _ := sample(1); g != 0 { // below baseline clamps to 0, never negative
		t.Errorf("below-baseline delta = %v, want 0", g)
	}
}
