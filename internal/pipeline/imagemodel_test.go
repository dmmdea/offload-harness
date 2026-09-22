package pipeline

// TestImageModelFromConfig pins the ONE config -> imagegen.Model mapping used
// by both the single and batch render paths. The reflect check is the point:
// when a field is added to imagegen.Model but not mapped here, this test fails
// — the exact drift that let the batch path silently drop five fields
// (review-caught pre-merge, 2026-08-10).

import (
	"reflect"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

func TestImageModelFromConfig(t *testing.T) {
	cfg := config.Config{
		ImageGenCkpt:         "ck.gguf",
		ImageGenVAE:          "v.safetensors",
		ImageGenSteps:        4,
		ImageGenCFG:          1.0,
		ImageGenSampler:      "euler",
		ImageGenScheduler:    "simple",
		ImageGenReserveVRAM:  0.6,
		ImageGenFamily:       "qwen-image",
		ImageGenPreset:       "lightning4",
		ImageGenCLIP:         "te.safetensors",
		ImageGenLoRA:         "l.safetensors",
		ImageGenLoRAStrength: -0.5, // negative is a legitimate inverse weight
		ImageGenShift:        3.1,
		ImageGenPoolVvramGB:  12,
		ImageGenPoolCompute:  "cuda:0",
		ImageGenPoolDonor:    "cuda:1",
		ImageGenSchedule:     "official",
		ComfyCudaDevice:      "2",
		ComfyDynamicVRAM:     "off",
		ComfyExtraArgs:       "--verbose",
	}
	m := imageModelFromConfig(cfg)

	if m.Ckpt != cfg.ImageGenCkpt || m.VAE != cfg.ImageGenVAE || m.Steps != cfg.ImageGenSteps ||
		m.CFG != cfg.ImageGenCFG || m.Sampler != cfg.ImageGenSampler ||
		m.Scheduler != cfg.ImageGenScheduler || m.Family != cfg.ImageGenFamily ||
		m.Preset != cfg.ImageGenPreset || m.CLIP != cfg.ImageGenCLIP ||
		m.LoRA != cfg.ImageGenLoRA || m.LoRAStrength != cfg.ImageGenLoRAStrength ||
		m.Shift != cfg.ImageGenShift || m.PoolVvramGB != cfg.ImageGenPoolVvramGB ||
		m.PoolCompute != cfg.ImageGenPoolCompute || m.PoolDonor != cfg.ImageGenPoolDonor ||
		m.Schedule != cfg.ImageGenSchedule || m.Launch.DynamicVRAM != cfg.ComfyDynamicVRAM || m.Launch.ExtraArgs != cfg.ComfyExtraArgs {
		t.Fatalf("field mapping mismatch: %+v", m)
	}

	// Drift guard: with every input above non-zero, every Model field must be
	// non-zero. A newly added Model field left unmapped stays zero and fails
	// here until it is both mapped and given a value above.
	v := reflect.ValueOf(m)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Fatalf("imagegen.Model field %q is zero — added to the struct but not "+
				"mapped in imageModelFromConfig (and/or not set in this test's cfg)",
				v.Type().Field(i).Name)
		}
	}
}

// The device pin is for the SINGLE-CARD shape only: a pooled seat is placed by its pool
// keys, and a --cuda-device pin would hide the donor card from the DisTorch2 loader.
func TestImageModelLaunchPinsOnlyTheSingleCardShape(t *testing.T) {
	pooled := config.Config{ImageGenPoolVvramGB: 12, ImageGenPoolCompute: "cuda:1", ImageGenPoolDonor: "cuda:2",
		ComfyCudaDevice: "1", ComfyDynamicVRAM: "off"}
	if got := imageModelFromConfig(pooled).Launch; got.CudaDevice != "" || got.DynamicVRAM != "off" {
		t.Fatalf("pooled seat: launch = %+v, want no pin and the launch-wide keys kept", got)
	}
	single := config.Config{ComfyCudaDevice: " 1 ", ComfyDynamicVRAM: "on"}
	if got := imageModelFromConfig(single).Launch; got.CudaDevice != "1" || got.DynamicVRAM != "on" {
		t.Fatalf("single-card seat: launch = %+v, want the pin", got)
	}
}
