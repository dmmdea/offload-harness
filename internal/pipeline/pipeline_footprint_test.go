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
	if q := videoFootprintQuant(cfg, "wan22"); q != "" {
		t.Errorf("unbound unets: quant = %q, want \"\"", q)
	}
	cfg.VideoGenUnetHigh = "wan2.2_i2v_high_noise_14B_Q8_0.gguf"
	if q := videoFootprintQuant(cfg, "wan22"); q != "q8_0" {
		t.Errorf("Q8_0 high unet: quant = %q, want \"q8_0\"", q)
	}
	cfg.VideoGenUnetHigh = "wan2.2_i2v_high_noise_14B_fp8_scaled.safetensors"
	cfg.VideoGenUnetLow = "wan2.2_i2v_low_noise_14B_q8_0.gguf"
	if q := videoFootprintQuant(cfg, "wan22"); q != "q8_0" {
		t.Errorf("q8_0 low unet (lowercase): quant = %q, want \"q8_0\"", q)
	}
	cfg.VideoGenUnetLow = "wan2.2_i2v_low_noise_14B_fp8_scaled.safetensors"
	if q := videoFootprintQuant(cfg, "wan22"); q != "" {
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
// a sampler, and a callback that records into the shared store (raw peak — the
// node adds no margin; the dispatcher owns it, ADR 0013).
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
	if e.VramPeakGiB != 3.0 { // 3.0 observed, raw (no node padding)
		t.Errorf("vram_peak_gb = %v, want 3.0", e.VramPeakGiB)
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
	if e.VramPeakGiB != 2.0 { // 2.0 observed, raw (no node padding)
		t.Errorf("vram_peak_gb = %v, want 2.0", e.VramPeakGiB)
	}
}

// TestRunGenerateVideo_RecordsFootprint: same E2E proof for the video path.
// The Wan family keeps the STORE's own spelling "wan2.2" — that is the key
// fleetnode.familyFor advertises, and writer and advertiser must intersect. What
// 0.73.1 fixes is that the key is now DERIVED: 0.73.0 and earlier hardcoded
// "wan2.2" for every video render regardless of family, so an ltx25 box wrote its
// LTX renders into Wan's key.
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
	if e.VramPeakGiB != 5.5 { // 5.5 observed, raw (no node padding)
		t.Errorf("vram_peak_gb = %v, want 5.5", e.VramPeakGiB)
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

// The two provenance wiring guards. They exist as a PAIR because each covers a
// half the other structurally cannot:
//
//   - The OVERRIDE arm (seat ltx25, request wan) is the only shape where the
//     LEDGER LABEL differs between fixed and broken — 0.73.0 wrote the seat.
//     But its correct FOOTPRINT answer is "wan2.2", byte-identical to the old
//     hardcode, so it cannot see the footprint half AT ALL.
//   - The SEATED arm (seat ltx25, no override) is the only shape where the
//     FOOTPRINT KEY differs from the hardcode — the round-3 review proved the
//     override arm alone left the literal 0.73.0 footprint hardcode green across
//     the entire suite, while this round's own CHANGELOG claimed it verified red.
//
// Helper-level tests cannot replace either: with every helper correct and
// unit-tested, re-introducing the bug at the CALL SITES is invisible unless a
// test reads the surfaces out of a real Run().

// TestRunGenerateVideo_OverrideProvenanceEndToEnd: seat ltx25, request wan.
// Both surfaces must name WAN, because Wan is what renders. This arm is the
// LABEL half of the pair (mutation-verified red: passing p.cfg to the label
// fails here with model_tier "comfyui-video:ltx25").
func TestRunGenerateVideo_OverrideProvenanceEndToEnd(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.VideoGenScript = writeStub(t, dir)
	cfg.MediaDir = dir
	// The live 2x16 shape: seated to ltx25, with the Wan GGUFs still bound as the
	// recorded fallback family.
	cfg.VideoGenFamily = "ltx25"
	cfg.VideoGenUnetHigh = "wan2.2_i2v_high_noise_14B_Q8_0.gguf"
	p := footprintTestPipeline(t, cfg, 5.5)

	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskGenerateVideo,
		Input:  "a calm ocean at dawn",
		Params: map[string]any{"model": "wan"},
	})
	if !res.OK {
		t.Fatalf("expected ok via stub, got defer: %s", res.Reason)
	}

	// SURFACE 1 — the ledger label. 0.73.0 wrote "comfyui-video:ltx25" here for
	// this exact request: the configured seat, not the family that rendered.
	if res.Meta.Model != "comfyui-video:wan22" {
		t.Errorf("ledger model_tier = %q, want %q — the request overrode the seat, so the label must follow the render, not the config",
			res.Meta.Model, "comfyui-video:wan22")
	}

	// SURFACE 2 — sanity only in THIS arm: the correct answer here equals the old
	// hardcode, so this assertion cannot distinguish fixed from broken. The
	// discriminating footprint assertion lives in the SEATED arm below.
	e := findEntry(t, p.FootprintStore().Entries(), "wan2.2", "video-gen")
	if e.Quant != "q8_0" {
		t.Errorf("footprint quant = %q, want \"q8_0\" — the override rendered the Wan GGUF recipe", e.Quant)
	}
}

// TestRunGenerateVideo_SeatedProvenanceEndToEnd: seat ltx25, NO override — the
// live 2x16 box's everyday render, and the FOOTPRINT half of the pair. This is
// the only shape where the correct key ("ltx25") differs from 0.73.0's hardcode
// ("wan2.2"), so it is the arm that actually goes red when the hardcode returns.
// It also pins the quant scoping: the Wan GGUFs stay bound as the fallback
// family, and 0.73.0 stamped their q8_0 onto LTX renders.
func TestRunGenerateVideo_SeatedProvenanceEndToEnd(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.VideoGenScript = writeStub(t, dir)
	cfg.MediaDir = dir
	cfg.VideoGenFamily = "ltx25"
	cfg.VideoGenUnetHigh = "wan2.2_i2v_high_noise_14B_Q8_0.gguf"
	p := footprintTestPipeline(t, cfg, 5.5)

	res := p.Run(context.Background(), core.Request{
		Task:  core.TaskGenerateVideo,
		Input: "a calm ocean at dawn",
	})
	if !res.OK {
		t.Fatalf("expected ok via stub, got defer: %s", res.Reason)
	}

	if res.Meta.Model != "comfyui-video:ltx25" {
		t.Errorf("ledger model_tier = %q, want %q", res.Meta.Model, "comfyui-video:ltx25")
	}

	// THE discriminating assertion: the key must be the seated family. Under the
	// 0.73.0 hardcode this store holds exactly one entry, keyed wan2.2, and
	// findEntry fails with it in the message.
	e := findEntry(t, p.FootprintStore().Entries(), "ltx25", "video-gen")
	// And the quant must NOT be inherited from the Wan GGUF filenames: this
	// render's transformer is not the Wan recipe, so "unknown" is the honest key.
	if e.Quant != "" {
		t.Errorf("footprint quant = %q, want \"\" — an ltx25 render must not inherit the Wan GGUFs' q8_0", e.Quant)
	}
	// No Wan entry may exist: nothing rendered Wan in this run. Placed HERE, not
	// in the override arm, because here it is reachable — findEntry above passes
	// on fixed code, so this loop actually runs (in the override arm the
	// equivalent loop sat behind a t.Fatalf that fired first under any mutation
	// that could have created the entry, making it dead code as a detector).
	for _, entry := range p.FootprintStore().Entries() {
		if entry.ModelFamily == "wan2.2" {
			t.Errorf("footprint store gained a wan2.2 entry (%+v) for a render that used LTX — the store-poisoning half of the 0.73.0 defect", entry)
		}
	}
}
