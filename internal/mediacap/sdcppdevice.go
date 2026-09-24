// sdcppdevice.go — doctor-only diagnostic: which Vulkan device an sdcpp render on
// THIS box will actually use.
//
// render/sdcpp-generate.mjs used to pin GGML_VK_VISIBLE_DEVICES=0 whenever the
// environment left it unset, on the assumption that device 0 is the render GPU.
// On a box with an enabled integrated GPU, ggml-Vulkan enumerates it FIRST: the
// OptiPlex remediation (2026-09-23) measured Vulkan0 = Intel(R) UHD Graphics 630,
// Vulkan1 = NVIDIA GeForce RTX 5060 — every Z-Image render silently ran on the
// iGPU at 565-608 s/step (the same recipe runs 4.85 s/step on the RTX). The
// render script now auto-picks the first discrete adapter `sd-cli --list-devices`
// reports (see that file's header); DetectSdcppDevice mirrors the SAME resolution
// order in Go so `doctor` can report, without guessing, which device a render
// will actually land on and flag it when that device is an iGPU.
//
// This is deliberately NOT part of mediacap.Routes/RoutesChecked: those stay a
// pure config+filesystem derivation for callers that run on every offload_status
// and acceptance pass (mediacap.go's own doc comment: "neither may make a network
// call per route" — a live subprocess spawn is the same class of cost). `doctor`
// already makes a live call for ComfyUI's /object_info (LiveNodeChecker); this is
// the same kind of doctor-only probe, invoked directly from runDoctor.
package mediacap

import (
	"os/exec"
	"regexp"
	"strings"
)

// VulkanDevice is one line of `sd-cli --list-devices` output.
type VulkanDevice struct {
	Index string
	Name  string
}

var (
	vulkanDeviceLineRe = regexp.MustCompile(`(?m)^\s*Vulkan(\d+)\s+(.+?)\s*$`)
	igpuNameRe         = regexp.MustCompile(`(?i)\bintel\b|\buhd\b|\biris\b|\barc\b`)
	discreteNameRe     = regexp.MustCompile(`(?i)\bnvidia\b|\bgeforce\b|\bquadro\b|\bamd\b|\bradeon\b`)
)

// ParseVulkanDeviceList parses `sd-cli --list-devices` output ("VulkanN <adapter
// name>" lines; anything else is ignored) into index/name pairs, in listed order.
// Pure.
func ParseVulkanDeviceList(text string) []VulkanDevice {
	var out []VulkanDevice
	for _, m := range vulkanDeviceLineRe.FindAllStringSubmatch(text, -1) {
		out = append(out, VulkanDevice{Index: m[1], Name: strings.TrimSpace(m[2])})
	}
	return out
}

// IsIntegratedGpuName reports whether name is a known Intel-integrated part —
// never the adapter to pick for a diffusion render (measured 2026-09-23:
// 565-608 s/step on an Intel UHD 630 vs 4.85 s/step on the same box's RTX 5060).
func IsIntegratedGpuName(name string) bool { return igpuNameRe.MatchString(name) }

// IsDiscreteGpuName reports whether name is a discrete NVIDIA/AMD adapter.
func IsDiscreteGpuName(name string) bool {
	return discreteNameRe.MatchString(name) && !igpuNameRe.MatchString(name)
}

// PickDiscreteVulkanDevice returns the first discrete adapter's index in listed
// order, "" when none of devices is discrete.
func PickDiscreteVulkanDevice(devices []VulkanDevice) string {
	for _, d := range devices {
		if IsDiscreteGpuName(d.Name) {
			return d.Index
		}
	}
	return ""
}

// deviceByIndex finds devices[i] with Index == idx, "" name if not listed.
func deviceByIndex(devices []VulkanDevice, idx string) string {
	for _, d := range devices {
		if d.Index == idx {
			return d.Name
		}
	}
	return ""
}

// SdcppDevice is doctor's report of which Vulkan device an sdcpp render will use.
type SdcppDevice struct {
	Index       string // the GGML_VK_VISIBLE_DEVICES value that will be in effect
	Name        string // the adapter name at that index; "" when --list-devices could not say
	EnvOverride bool   // GGML_VK_VISIBLE_DEVICES was already set in the environment
	IsIGPU      bool   // Name matches a known integrated-GPU pattern
	ProbeErr    string // non-empty when --list-devices could not be run or listed nothing
}

// sdcppDeviceLister runs "<bin> --list-devices" and returns its combined output;
// injected so DetectSdcppDevice is unit-testable without a real sd-cli binary.
type sdcppDeviceLister func(bin string) (string, error)

// ExecSdcppDeviceLister is the real lister: `<bin> --list-devices`, combined
// stdout+stderr (sd.cpp release builds have printed device enumeration to
// either stream across versions).
func ExecSdcppDeviceLister(bin string) (string, error) {
	out, err := exec.Command(bin, "--list-devices").CombinedOutput()
	return string(out), err
}

// DetectSdcppDevice resolves which Vulkan device an sdcpp render will use on this
// box, mirroring render/sdcpp-generate.mjs's own resolution order exactly: an
// explicit env override always wins (never guessed around); otherwise the first
// discrete adapter `--list-devices` reports; otherwise device 0 (ggml-Vulkan's own
// default) — flagged as an iGPU when it is one, so the FAIL case an operator cares
// about (a render about to run on the integrated part) is never silent.
func DetectSdcppDevice(bin, envOverride string, list sdcppDeviceLister) SdcppDevice {
	if list == nil {
		list = ExecSdcppDeviceLister
	}
	envOverride = strings.TrimSpace(envOverride)
	out, err := list(bin)
	var devices []VulkanDevice
	var probeErr string
	if err != nil {
		probeErr = err.Error()
	} else {
		devices = ParseVulkanDeviceList(out)
		if len(devices) == 0 {
			probeErr = "no Vulkan devices reported by --list-devices"
		}
	}

	if envOverride != "" {
		info := SdcppDevice{Index: envOverride, EnvOverride: true, ProbeErr: probeErr}
		if name := deviceByIndex(devices, envOverride); name != "" {
			info.Name = name
			info.IsIGPU = IsIntegratedGpuName(name)
		}
		return info
	}

	if idx := PickDiscreteVulkanDevice(devices); idx != "" {
		return SdcppDevice{Index: idx, Name: deviceByIndex(devices, idx)}
	}
	// No discrete adapter found (or the probe failed): the render script falls
	// back to device 0, exactly as it always has.
	info := SdcppDevice{Index: "0", ProbeErr: probeErr}
	if name := deviceByIndex(devices, "0"); name != "" {
		info.Name = name
		info.IsIGPU = IsIntegratedGpuName(name)
	}
	return info
}
