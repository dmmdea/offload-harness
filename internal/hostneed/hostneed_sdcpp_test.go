package hostneed

// F60: an sd.cpp-only box declared no host RAM for its lane (the class default is the largest COMFYUI family a box binds and
// such a box binds none), so the weights `--offload-to-cpu` parks in RAM on the small nodes (5.5 to 6.2 GiB, from the review)
// were outside the host-RAM brake on the very boxes whose memory matters. ForSdcpp sizes them from the lane's own bound files.
// It is an ESTIMATE, labelled so: the files' sizes, in full, parked when the extra args carry --offload-to-cpu; the footprint
// store's measured runs raise it after three (internal/pipeline), as they do a ComfyUI family's.

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

const gibBytes = 1 << 30

func sdcppFacts(sizes map[string]float64) Facts {
	return Facts{
		Cfg: config.Config{},
		Stat: func(p string) (int64, bool) {
			g, ok := sizes[p]
			return int64(g * gibBytes), ok
		},
	}
}

var zImageFiles = []SdcppFile{
	{Label: "sdcpp_model", Path: "models/z_image_turbo-Q8_0.gguf"},
	{Label: "sdcpp_llm", Path: "models/Qwen3-4B-Instruct-2507-Q4_K_M.gguf"},
	{Label: "sdcpp_vae", Path: "models/zimage_ae.safetensors"},
}

var zImageSizes = map[string]float64{
	"models/z_image_turbo-Q8_0.gguf":            6.73,
	"models/Qwen3-4B-Instruct-2507-Q4_K_M.gguf": 2.33,
	"models/zimage_ae.safetensors":              0.31,
}

func TestSdcppParksItsBoundFilesWhenItOffloadsToTheCPU(t *testing.T) {
	n, ok := ForSdcpp("z-image", zImageFiles, []string{"--vae-tiling", OffloadToCPUFlag}, sdcppFacts(zImageSizes))
	if !ok {
		t.Fatal("bound files of known size are an estimate")
	}
	if !near(n.GiB, 6.73+2.33+0.31) || n.Source != SourceEstimate {
		t.Fatalf("every bound file is parked in full: %v", n)
	}
	for _, want := range []string{"z-image", "--offload-to-cpu", "estimate", "not a measurement", "z_image_turbo-Q8_0.gguf"} {
		if !strings.Contains(n.Detail, want) {
			t.Errorf("the detail must say %q: %s", want, n.Detail)
		}
	}
}

// Without the flag the weights go straight to the device: nothing is parked, and the estimate says so (it is not unknown).
func TestSdcppWithoutTheOffloadFlagParksNothing(t *testing.T) {
	for name, args := range map[string][]string{"no args": nil, "other args": {"--vae-tiling", "--diffusion-fa"}, "a look-alike": {"--offload-to-cpu-never"}} {
		n, ok := ForSdcpp("z-image", zImageFiles, args, sdcppFacts(zImageSizes))
		if !ok || n.GiB != 0 || !strings.Contains(n.Detail, "straight to the device") {
			t.Errorf("%s: %v ok=%v, want 0 GiB parked and the reason", name, n, ok)
		}
	}
}

// A file whose size cannot be read, or a binding that names none, is no estimate: the caller keeps the class default it had.
func TestSdcppWithAnUnreadableFileOrNoFilesIsNoEstimate(t *testing.T) {
	missing := map[string]float64{"models/z_image_turbo-Q8_0.gguf": 6.73}
	if _, ok := ForSdcpp("z-image", zImageFiles, []string{OffloadToCPUFlag}, sdcppFacts(missing)); ok {
		t.Error("a bound file that cannot be sized makes the estimate unknown, never a smaller number")
	}
	if _, ok := ForSdcpp("z-image", nil, []string{OffloadToCPUFlag}, sdcppFacts(nil)); ok {
		t.Error("no files, no estimate")
	}
	if _, ok := ForSdcpp("z-image", []SdcppFile{{Label: "sdcpp_vae", Path: "  "}}, []string{OffloadToCPUFlag}, sdcppFacts(nil)); ok {
		t.Error("a blank path names no file")
	}
}

// A high-noise expert, a second encoder or a VAE named twice are all parked: each distinct file is counted once.
func TestSdcppCountsEachDistinctFileOnce(t *testing.T) {
	files := []SdcppFile{{Label: "sdcpp_model", Path: "a.gguf"}, {Label: "sdcpp_high_noise_model", Path: "b.gguf"}, {Label: "sdcpp_vae", Path: "v.safetensors"}, {Label: "sdcpp_vae", Path: "v.safetensors"}}
	n, ok := ForSdcpp("wan22", files, []string{OffloadToCPUFlag}, sdcppFacts(map[string]float64{"a.gguf": 5, "b.gguf": 5, "v.safetensors": 0.25}))
	if !ok || !near(n.GiB, 10.25) {
		t.Fatalf("5 + 5 + 0.25 with the repeated VAE counted once: %v ok=%v", n, ok)
	}
}
