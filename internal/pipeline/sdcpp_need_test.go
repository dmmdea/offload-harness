package pipeline

// F60: the sd.cpp lanes declare the weights they park. The class default is the largest COMFYUI family a box binds, which an
// sd.cpp-only box (the shape of the small nodes) does not have, so the image, video and animate lanes there declared nothing and
// the 5.5 to 6.2 GiB `--offload-to-cpu` parks in RAM were outside the host-RAM brake. Each lane now declares the larger of
// the class default (unchanged: on a mixed box it is still the proxy it was) and its own bound files when it offloads, as an
// estimate; the footprint store's measured runs raise it after three, like a ComfyUI family's.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpugen"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/hostneed"
)

// withSdcppSizes sizes the sd.cpp files of a test by base name, through hostneed's stat seam (a 6.7 GiB model is not a fixture).
func withSdcppSizes(t *testing.T, sizes map[string]float64) {
	t.Helper()
	restore := hostneed.UseStat(func(path string) (int64, bool) {
		g, ok := sizes[filepath.Base(path)]
		return int64(g * (1 << 30)), ok
	})
	t.Cleanup(restore)
}

func sdcppOnlyCfg(extra ...string) config.Config {
	cfg := config.Default()
	cfg.ComfyDir = "" // no ComfyUI install: the class default is nothing
	cfg.ImageGenScript, cfg.ImageGenEngine = "render/sdcpp-generate.mjs", "sdcpp"
	cfg.ImageGenFamily = "z-image"
	cfg.SdcppBin, cfg.SdcppModel = "sd-cli", filepath.Join("models", "z_image_turbo-Q8_0.gguf")
	cfg.SdcppLLM, cfg.SdcppVAE = filepath.Join("models", "Qwen3-4B-Instruct-2507-Q4_K_M.gguf"), filepath.Join("models", "zimage_ae.safetensors")
	cfg.SdcppExtraArgs = extra
	return cfg
}

var zImageSizes = map[string]float64{
	"z_image_turbo-Q8_0.gguf":            6.73,
	"Qwen3-4B-Instruct-2507-Q4_K_M.gguf": 2.33,
	"zimage_ae.safetensors":              0.31,
}

func TestAnSdcppOnlyLaneDeclaresTheBytesItParks(t *testing.T) {
	withSdcppSizes(t, zImageSizes)
	cfg := sdcppOnlyCfg("--vae-tiling", "--offload-to-cpu")
	p := &Pipeline{cfg: cfg}
	need := p.sdcppImageNeed(cfg, "z-image", "q8_0", "")
	got := need.RAM(16)
	if !strings.Contains(got.String(), "estimate") || got.GiB < 9.3 || got.GiB > 9.4 {
		t.Fatalf("an sd.cpp-only box that offloads declares the 9.37 GiB its files park, as an estimate: %v", got)
	}
	if need.Foot != (footKey{"z-image", "q8_0", "image-gen"}) {
		t.Errorf("the declaration is raised by measured runs of the same key the lane records under: %+v", need.Foot)
	}
	// Without the flag nothing is parked: the lane declares what it did, nothing.
	cfg2 := sdcppOnlyCfg("--vae-tiling")
	if got := (&Pipeline{cfg: cfg2}).sdcppImageNeed(cfg2, "z-image", "q8_0", "").RAM(16); got.GiB != 0 {
		t.Errorf("no --offload-to-cpu, nothing parked: %v", got)
	}
}

// On a box that also binds a ComfyUI family the class default is still the proxy it was; the lane's own bytes raise it only
// when they are larger.
func TestAnSdcppLaneOnAMixedBoxDeclaresTheLargerOfTheClassDefaultAndItsOwnBytes(t *testing.T) {
	sizes := map[string]float64{"z_image_turbo-Q8_0.gguf": 6.73, "Qwen3-4B-Instruct-2507-Q4_K_M.gguf": 2.33, "zimage_ae.safetensors": 0.31}
	withSdcppSizes(t, sizes)
	cfg := sdcppOnlyCfg("--offload-to-cpu")
	cfg.ComfyDir = filepath.Join(t.TempDir(), "comfy") // no files here: the documented per-family sizes stand
	cfg.GenEditScript, cfg.GenEditFamily, cfg.GenEditUnet = "render/comfy-edit.mjs", "", "qwen_image_edit_2511_fp8mixed.safetensors"
	proxy := classDefaultRAM(cfg)(16)
	if proxy.GiB < 20 {
		t.Fatalf("control: the box binds a ComfyUI edit family, so the class default is large: %v", proxy)
	}
	p := &Pipeline{cfg: cfg}
	if got := p.sdcppImageNeed(cfg, "z-image", "q8_0", "").RAM(16); got.GiB != proxy.GiB || got.Source != proxy.Source {
		t.Errorf("9.4 GiB of its own does not lower the proxy %v: %v", proxy, got)
	}
	sizes["z_image_turbo-Q8_0.gguf"] = 60 // a model larger than the proxy
	if got := p.sdcppImageNeed(cfg, "z-image", "q8_0", "").RAM(16); got.GiB < 62 {
		t.Errorf("its own bytes raise the proxy when they are larger: %v", got)
	}
}

// A file that cannot be sized does not take the spill back outside the brake (0.180 hardening): the lane declares the files it
// could size, as a lower bound, and says once which it could not. The class default is 0 on this box, so before the fix one
// mistyped VAE path declared nothing at all, silently.
func TestAnSdcppLaneWithAnUnsizedFileDeclaresTheLowerBoundAndSaysSoOnce(t *testing.T) {
	withSdcppSizes(t, map[string]float64{"z_image_turbo-Q8_0.gguf": 6.73, "Qwen3-4B-Instruct-2507-Q4_K_M.gguf": 2.33}) // the VAE cannot be sized
	cfg := sdcppOnlyCfg("--offload-to-cpu")
	buf := captureLogLines(t)
	need := (&Pipeline{cfg: cfg}).sdcppImageNeed(cfg, "z-image-lb", "q8_0", "")
	got := need.RAM(16)
	if got.GiB < 9.05 || got.GiB > 9.07 {
		t.Fatalf("the two sized files (6.73 + 2.33 = 9.06 GiB) are declared, not nothing: %v", got)
	}
	for _, want := range []string{"lower bound", "sdcpp_vae"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("the declaration says what it could not size (%q): %s", want, got.Detail)
		}
	}
	need.RAM(16) // the admission asks again on every poll: the line is made once
	need.RAM(16)
	lines := linesWith(buf, "could not size")
	if len(lines) != 1 {
		t.Fatalf("exactly one line says a weight file could not be sized, got %d:\n%s", len(lines), buf.String())
	}
	for _, want := range []string{"sdcpp_vae", "z-image-lb", "lower bound"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the line %q lacks %q", lines[0], want)
		}
	}
}

// Nothing could be sized: there is no number to declare, so the class default stands (0 here), and the lane says so once
// instead of passing unbraked in silence.
func TestAnSdcppLaneWhoseFilesCannotBeSizedKeepsTheClassDefaultAndSaysSo(t *testing.T) {
	withSdcppSizes(t, map[string]float64{})
	cfg := sdcppOnlyCfg("--offload-to-cpu")
	buf := captureLogLines(t)
	need := (&Pipeline{cfg: cfg}).sdcppImageNeed(cfg, "z-image-none", "q8_0", "")
	if got := need.RAM(16); got.GiB != 0 {
		t.Errorf("no file could be sized: the class default stands, never a made-up number: %v", got)
	}
	need.RAM(16)
	lines := linesWith(buf, "could not size")
	if len(lines) != 1 {
		t.Fatalf("exactly one line says no weight file could be sized, got %d:\n%s", len(lines), buf.String())
	}
	for _, want := range []string{"sdcpp_model", "class default", "z-image-none"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the line %q lacks %q", lines[0], want)
		}
	}
	// A lane that does not offload reads no file and says nothing.
	cfg2 := sdcppOnlyCfg("--vae-tiling")
	buf2 := captureLogLines(t)
	(&Pipeline{cfg: cfg2}).sdcppImageNeed(cfg2, "z-image-noflag", "q8_0", "").RAM(16)
	if lines := linesWith(buf2, "could not size"); len(lines) != 0 {
		t.Errorf("a lane that parks nothing has no file to size: %v", lines)
	}
}

// THE BRAKE: with the host nearly full, the lane is held by the guard instead of loading its parked weights on top of it.
func TestAnSdcppOnlyLaneIsHeldByTheHostRAMBrake(t *testing.T) {
	withSdcppSizes(t, zImageSizes)
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	cfg := sdcppOnlyCfg("--offload-to-cpu")
	restore := gpuprobe.UseHostMemoryReader(func() (gpuprobe.HostMemory, bool) { return shortHost(88), true }) // 88 committed of 100, 8 headroom
	t.Cleanup(restore)

	need := f.p.sdcppImageNeed(cfg, "z-image", "q8_0", "")
	if g, err := f.p.acquireMediaLease(context.Background(), "image-gen (sdcpp)", time.Hour, 0, need); err == nil {
		g.Release()
		t.Fatal("88 committed + 9.4 parked is over 92: the lane must wait for the room")
	} else if !strings.Contains(err.Error(), "host RAM") {
		t.Fatalf("the answer must say it is the host's memory: %v", err)
	}
	// Control: the same lane as it was declared before (nothing) is let through the same host, which is the hole.
	old := wholeNeed("").declaresRAM(classDefaultRAM(cfg))
	g, err := f.p.acquireMediaLease(context.Background(), "image-gen (sdcpp)", time.Hour, 0, old)
	if err != nil {
		t.Fatalf("control: the old declaration (0) is admitted whatever the host reads: %v", err)
	}
	g.Release()
}

// The video and animate lanes read their own bindings.
func TestTheSdcppVideoAndAnimateBindingsAreSizedFromTheirOwnFiles(t *testing.T) {
	withSdcppSizes(t, map[string]float64{"wan-low.gguf": 5, "wan-high.gguf": 5, "wan_vae.safetensors": 0.25, "umt5.gguf": 6, "vace.safetensors": 7})
	cfg := sdcppOnlyCfg()
	fb := config.VideoFamilyBinding{SdcppBin: "sd-cli", SdcppModel: "wan-low.gguf", SdcppHighNoiseModel: "wan-high.gguf", SdcppVAE: "wan_vae.safetensors", SdcppT5xxl: "umt5.gguf", SdcppExtraArgs: []string{"--offload-to-cpu"}}
	got := sdcppDeclared(cfg, "wan22", videoSdcppFiles(fb), fb.SdcppExtraArgs)(16)
	if got.GiB < 16.2 || got.GiB > 16.3 {
		t.Errorf("video: 5 + 5 + 0.25 + 6 = 16.25 GiB parked, got %v", got)
	}
	cfg.AnimateGenSdcppModel, cfg.AnimateGenSdcppVAE, cfg.AnimateGenSdcppT5xxl = "vace.safetensors", "wan_vae.safetensors", "umt5.gguf"
	cfg.AnimateGenSdcppExtraArgs = []string{"--offload-to-cpu"}
	got = sdcppDeclared(cfg, "wan-animate", animateSdcppFiles(cfg), cfg.AnimateGenSdcppExtraArgs)(16)
	if got.GiB < 13.2 || got.GiB > 13.3 {
		t.Errorf("animate: 7 + 0.25 + 6 = 13.25 GiB parked, got %v", got)
	}
}

// runIGPU is where a lane's declaration meets the lease: the lease record carries what the lane's own sizing said, a lane with
// none keeps the class default it always had, and the footprint key rides only with a lane's own sizing.
func TestRunIGPUTakesTheLeaseWithTheLanesOwnDeclaration(t *testing.T) {
	f := newAdmitFixtureWith(t, admitSpec{order: admitOrder})
	declaredAtRun := func(ram func(float64) hostneed.Need) float64 {
		var got float64
		old := generateIGPU
		generateIGPU = func(ctx context.Context, spec gpugen.Spec) (string, error) {
			for _, l := range f.m.Leases() {
				got += l.HostRAMGiB
			}
			return spec.Out, nil
		}
		defer func() { generateIGPU = old }()
		meta := core.Meta{}
		_, dres := f.p.runIGPU(context.Background(), core.Request{Task: core.TaskGenerateVideo, Input: "x"}, &meta, time.Now(), igpuRun{
			leaseReason: "video-gen (sdcpp)", failVerb: "video generation failed", fpFamily: "wan22", fpQuant: "q8_0", fpTask: "video-gen",
			script: "render/sdcpp-video.mjs", out: filepath.Join(t.TempDir(), "o.mp4"), timeout: time.Minute, ram: ram,
		})
		if dres != nil {
			t.Fatalf("the lane was deferred: %+v", *dres)
		}
		return got
	}
	own := func(float64) hostneed.Need {
		return hostneed.Need{GiB: 7, Source: hostneed.SourceEstimate, Detail: "its own"}
	}
	if got := declaredAtRun(own); got < 6.99 || got > 7.01 {
		t.Errorf("the lease carries the lane's own 7 GiB: %.2f", got)
	}
	want := classDefaultRAM(f.p.cfg)(16).GiB // this fixture binds ComfyUI families: the class default is large
	if want < 20 {
		t.Fatalf("control: the fixture's class default should be large, got %.2f", want)
	}
	if got := declaredAtRun(nil); got < want-0.01 || got > want+0.01 {
		t.Errorf("a lane with no sizing of its own keeps the class default %.2f, got %.2f", want, got)
	}
}

// Where the three sd.cpp lanes are built is where they must ask for their own bytes (a lane that fell back to the class default
// on an sd.cpp-only box would be admitted whatever the host reads again).
func TestTheThreeSdcppLanesAskForTheirOwnBytes(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	pl, ig := read("pipeline.go"), read("igpumedia.go")
	if !strings.Contains(pl, "p.sdcppImageNeed(cfg,") {
		t.Error("the sd.cpp image lane no longer builds its need from its own files (sdcppImageNeed)")
	}
	for _, want := range []string{"ram: sdcppDeclared(p.cfg, renderFamily, videoSdcppFiles(fb), fb.SdcppExtraArgs)", "ram: sdcppDeclared(p.cfg, config.AnimateSdcppFootprintFamily, animateSdcppFiles(cfg), cfg.AnimateGenSdcppExtraArgs)"} {
		if !strings.Contains(ig, want) {
			t.Errorf("igpumedia.go no longer contains %q", want)
		}
	}
}
