package mediacap

import (
	"errors"
	"testing"
)

var errExec = errors.New("exec: \"sd-cli\": executable file not found in $PATH")

const twoDeviceListing = "Vulkan0 Intel(R) UHD Graphics 630\nVulkan1 NVIDIA GeForce RTX 5060\n"

func TestParseVulkanDeviceList(t *testing.T) {
	got := ParseVulkanDeviceList(twoDeviceListing)
	if len(got) != 2 {
		t.Fatalf("got %d devices, want 2: %+v", len(got), got)
	}
	if got[0] != (VulkanDevice{Index: "0", Name: "Intel(R) UHD Graphics 630"}) {
		t.Fatalf("device 0 = %+v", got[0])
	}
	if got[1] != (VulkanDevice{Index: "1", Name: "NVIDIA GeForce RTX 5060"}) {
		t.Fatalf("device 1 = %+v", got[1])
	}
	if got := ParseVulkanDeviceList("garbage\nnot a device line\n"); len(got) != 0 {
		t.Fatalf("garbage input must parse to no devices, got %+v", got)
	}
}

func TestIsIntegratedAndDiscreteGpuName(t *testing.T) {
	igpu := []string{"Intel(R) UHD Graphics 630", "Intel Iris Xe Graphics", "Intel(R) Arc(TM) A750 Graphics"}
	for _, n := range igpu {
		if !IsIntegratedGpuName(n) {
			t.Errorf("%q should be recognized as an iGPU", n)
		}
		if IsDiscreteGpuName(n) {
			t.Errorf("%q must never be classified as discrete", n)
		}
	}
	discrete := []string{"NVIDIA GeForce RTX 5060", "NVIDIA RTX A4000", "AMD Radeon RX 7900 XTX"}
	for _, n := range discrete {
		if !IsDiscreteGpuName(n) {
			t.Errorf("%q should be recognized as discrete", n)
		}
		if IsIntegratedGpuName(n) {
			t.Errorf("%q must never be classified as an iGPU", n)
		}
	}
}

func TestPickDiscreteVulkanDevice(t *testing.T) {
	devices := ParseVulkanDeviceList(twoDeviceListing)
	if got := PickDiscreteVulkanDevice(devices); got != "1" {
		t.Fatalf("PickDiscreteVulkanDevice = %q, want \"1\"", got)
	}
	onlyIgpu := ParseVulkanDeviceList("Vulkan0 Intel(R) UHD Graphics 630\n")
	if got := PickDiscreteVulkanDevice(onlyIgpu); got != "" {
		t.Fatalf("PickDiscreteVulkanDevice with only an iGPU = %q, want \"\"", got)
	}
}

// TestDetectSdcppDevice_AutoPicksDiscrete is the OptiPlex-shaped case: two
// devices, Vulkan0 = iGPU listed first, Vulkan1 = the RTX. No env override —
// auto-detection must land on the discrete adapter, never device 0.
func TestDetectSdcppDevice_AutoPicksDiscrete(t *testing.T) {
	lister := func(bin string) (string, error) { return twoDeviceListing, nil }
	got := DetectSdcppDevice("sd-cli", "", lister)
	if got.Index != "1" || got.Name != "NVIDIA GeForce RTX 5060" || got.IsIGPU || got.EnvOverride {
		t.Fatalf("got %+v, want the discrete Vulkan1 adapter, no override, not flagged as an iGPU", got)
	}
}

// TestDetectSdcppDevice_NoDiscreteFallsBackToZero: a box with only an iGPU (or a
// probe that lists nothing) falls back to device 0, same as the pre-fix default —
// but it must be FLAGGED, not silently reported OK.
func TestDetectSdcppDevice_NoDiscreteFallsBackToZero(t *testing.T) {
	lister := func(bin string) (string, error) { return "Vulkan0 Intel(R) UHD Graphics 630\n", nil }
	got := DetectSdcppDevice("sd-cli", "", lister)
	if got.Index != "0" || !got.IsIGPU || got.EnvOverride {
		t.Fatalf("got %+v, want device 0 flagged as an iGPU", got)
	}
}

// TestDetectSdcppDevice_EnvOverrideWins: an operator's explicit
// GGML_VK_VISIBLE_DEVICES is honoured verbatim even when a discrete adapter sits
// at a different index — never second-guessed.
func TestDetectSdcppDevice_EnvOverrideWins(t *testing.T) {
	lister := func(bin string) (string, error) { return twoDeviceListing, nil }
	got := DetectSdcppDevice("sd-cli", "0", lister)
	if got.Index != "0" || !got.EnvOverride || !got.IsIGPU || got.Name != "Intel(R) UHD Graphics 630" {
		t.Fatalf("got %+v, want the override honoured verbatim and named/flagged", got)
	}
}

// TestDetectSdcppDevice_ProbeFailureFallsBackCleanly: --list-devices erroring
// (missing binary, unsupported flag on an older pin) must never panic or block a
// render — device 0 with the error recorded for doctor to show.
func TestDetectSdcppDevice_ProbeFailureFallsBackCleanly(t *testing.T) {
	lister := func(bin string) (string, error) { return "", errExec }
	got := DetectSdcppDevice("sd-cli", "", lister)
	if got.Index != "0" || got.ProbeErr == "" || got.Name != "" || got.IsIGPU {
		t.Fatalf("got %+v, want device 0 with a recorded probe error and no device name", got)
	}
}
