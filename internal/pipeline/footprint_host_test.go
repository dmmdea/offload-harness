package pipeline

// G3 of the P0 plan: the host-RAM guard sizes a render from what renders were MEASURED to hold, not only from the
// size of their model files. Every sampled GPU render records the peak private and the peak resident memory of its
// process tree beside its VRAM peak (gpugen's HostSampleFunc, the footprint store's RecordHost), and the media
// admission raises a declaration to the measured resident peak once enough runs agree. It only ever RAISES: a
// measurement can be low (a kept ComfyUI instance reused from an earlier lease is not under the runner that is
// sampled), and a measurement must never be what waves a 33 GiB stream through as 5.

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/hostneed"
)

// The composed hook carries the host sampler and a callback that records the two peaks into the shared store.
func TestFootprintSamplingCarriesTheHostSamplerAndRecordsHostPeaks(t *testing.T) {
	p := footprintTestPipeline(t, config.Default(), 0)
	s := p.footprintSampling("krea2", "", "image-gen")
	if s == nil || s.HostSampleFunc == nil || s.OnHostFootprint == nil {
		t.Fatalf("the sampling hook has no host sampler: %#v", s)
	}
	s.OnHostFootprint(57.7, 40.7)
	hp, ok := p.FootprintStore().HostPeak("krea2", "", "image-gen")
	if !ok || hp.PrivateGiB != 57.7 || hp.ResidentGiB != 40.7 || hp.Runs != 1 {
		t.Fatalf("host peaks in the store = %+v ok=%v, want private 57.7, resident 40.7, 1 run", hp, ok)
	}
}

// An E2E render through the real image path records its host peaks under the same key as its VRAM peak.
func TestRunGenerateImage_RecordsHostPeaks(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.ImageGenScript = writeStub(t, dir)
	cfg.MediaDir = dir
	p := footprintTestPipeline(t, cfg, 2.0)
	p.hostSample = func(int) (float64, float64, error) { return 6.5, 5.0, nil }

	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateImage, Input: "a gray sphere"})
	if !res.OK {
		t.Fatalf("expected ok via stub, got defer: %s", res.Reason)
	}
	hp, ok := p.FootprintStore().HostPeak("sdxl", "", "image-gen")
	if !ok || hp.PrivateGiB != 6.5 || hp.ResidentGiB != 5.0 || hp.Runs != 1 {
		t.Fatalf("host peaks = %+v ok=%v, want private 6.5, resident 5.0 over 1 run", hp, ok)
	}
}

// calibrationFixture is a pipeline whose footprint store is isolated, holding n measured runs of the krea2 image
// lane at the given private and resident peaks.
func calibrationFixture(t *testing.T, runs int, private, resident float64) (*Pipeline, config.Config) {
	t.Helper()
	cfg := config.Default()
	cfg.ImageGenFamily, cfg.ImageGenCkpt = "krea2", "krea2_turbo_bf16.safetensors"
	cfg.ComfyDir = filepath.Join(t.TempDir(), "comfy") // no model files here: the documented per-family sizes stand (24.5 + 8.3 = 32.8 GiB), not this machine's tree
	cfg.LedgerPath = filepath.Join(t.TempDir(), "ledger.jsonl")
	p := &Pipeline{cfg: cfg}
	for i := 0; i < runs; i++ {
		p.FootprintStore().RecordHost("krea2", "", "image-gen", private, resident)
	}
	return p, cfg
}

func declaredFor(t *testing.T, p *Pipeline, cfg config.Config, cardGiB float64) hostneed.Need {
	t.Helper()
	need := imageNeed(cfg, "")
	return p.calibratedRAM(need.RAM(cardGiB), need.Foot)
}

// Three runs that held more than the files add up to raise the declaration to the measured resident peak.
func TestAMeasuredResidentPeakRaisesTheDeclaredNeed(t *testing.T) {
	p, cfg := calibrationFixture(t, 3, 57.7, 40.7) // krea2 bf16 on a 16 GiB card: the files say 32.8
	got := declaredFor(t, p, cfg, 16)
	if math.Abs(got.GiB-40.7) > 1e-9 || got.Source != hostneed.SourceMeasured {
		t.Fatalf("three runs peaked at 40.7 GiB resident: declared %+v, want 40.7 GiB from a measurement", got)
	}
	for _, want := range []string{"measured", "40.7 GiB", "3 runs", "32.8 GiB"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("the declaration must say where its number came from (%q): %s", want, got.Detail)
		}
	}
}

// Fewer than hostPeakMinRuns runs are not a measurement yet.
func TestAMeasurementFromTooFewRunsDoesNotMoveTheDeclaration(t *testing.T) {
	p, cfg := calibrationFixture(t, hostPeakMinRuns-1, 57.7, 40.7)
	if got := declaredFor(t, p, cfg, 16); math.Abs(got.GiB-32.8) > 0.01 || got.Source == hostneed.SourceMeasured {
		t.Fatalf("%d runs are not enough: declared %+v, want the files' 32.8 GiB", hostPeakMinRuns-1, got)
	}
}

// A measurement never LOWERS a declaration: a low reading may come from a sampler that could not see the whole
// tree (a kept ComfyUI reused from an earlier lease is not under the sampled runner), and the files still say what
// must stream.
func TestAMeasurementNeverLowersTheDeclaration(t *testing.T) {
	p, cfg := calibrationFixture(t, 5, 8.0, 5.0)
	if got := declaredFor(t, p, cfg, 16); math.Abs(got.GiB-32.8) > 0.01 || got.Source == hostneed.SourceMeasured {
		t.Fatalf("a run that read 5 GiB resident must not lower the 32.8 GiB the files add up to: %+v", got)
	}
}

// A render whose weights fit its card declares nothing, and a measurement does not turn it into a host user: the
// measured tree includes the runtime's own baseline and (on a driver that charges card allocations to the process)
// the card's memory, neither of which is RAM the guard is for.
func TestAMeasurementDoesNotMakeACardResidentRenderAHostUser(t *testing.T) {
	p, cfg := calibrationFixture(t, 5, 57.7, 40.7)
	if got := declaredFor(t, p, cfg, 48); got.GiB != 0 {
		t.Fatalf("krea2's 32.8 GiB fit a 48 GiB card, so nothing streams from RAM: declared %+v, want 0", got)
	}
}

// With no store (no ledger or cache path resolves) or a key nobody measured, the files stand.
func TestWithoutMeasurementsTheFilesStand(t *testing.T) {
	cfg := config.Default()
	cfg.ImageGenFamily, cfg.ImageGenCkpt = "krea2", "krea2_turbo_bf16.safetensors"
	cfg.ComfyDir = filepath.Join(t.TempDir(), "comfy")
	cfg.LedgerPath = filepath.Join(t.TempDir(), "ledger.jsonl")
	p := &Pipeline{cfg: cfg}
	if got := declaredFor(t, p, cfg, 16); math.Abs(got.GiB-32.8) > 0.01 || got.Source == hostneed.SourceMeasured {
		t.Fatalf("an empty store leaves the files' 32.8 GiB: %+v", got)
	}
}
