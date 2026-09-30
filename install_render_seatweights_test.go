package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/mediaseat"
)

// TestSeatWeightWarningNamesTheVisionEncoder: llama-swap lists a seat from the CONFIG, so an
// rkllm VLM whose vision encoder was never downloaded passes every alias check and answers
// image questions blind or not at all. The install-time warning must look for the encoder
// as it does for an mmproj.
func TestSeatWeightWarningNamesTheVisionEncoder(t *testing.T) {
	seat := mediaseat.Seat{Kind: mediaseat.KindRKLLM, Name: "npu-vlm", Model: "m.rkllm", VisionEncoder: "enc.rknn",
		CtxSize: 4096, Residency: mediaseat.Swappable}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, seat.Model), []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	warnMissingSeatModelsTo([]mediaseat.Seat{seat}, dir, runtime.GOOS, &out)
	if !strings.Contains(out.String(), "vision_encoder") || !strings.Contains(out.String(), seat.VisionEncoder) {
		t.Errorf("a missing vision encoder was not reported:\n%s", out.String())
	}
	if strings.Contains(out.String(), seat.Model) {
		t.Errorf("the model file is present but was reported missing:\n%s", out.String())
	}
	if err := os.WriteFile(filepath.Join(dir, seat.VisionEncoder), []byte("encoder"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	warnMissingSeatModelsTo([]mediaseat.Seat{seat}, dir, runtime.GOOS, &out)
	if out.Len() != 0 {
		t.Errorf("every weight is present, yet a warning was printed:\n%s", out.String())
	}
	// Another machine's render: a local miss means nothing, so nothing is said.
	out.Reset()
	warnMissingSeatModelsTo([]mediaseat.Seat{seat}, t.TempDir(), "plan9", &out)
	if out.Len() != 0 {
		t.Errorf("a render for another OS warned about this machine's disk:\n%s", out.String())
	}
}
