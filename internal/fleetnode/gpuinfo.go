// gpuinfo.go — the GPU memory PROVIDER seam (J3). fleet-serve historically
// hard-required nvidia-smi: the startup probe, the 2s health sampler, and the
// vendor/arch advertisement all shelled to it, so a working AMD/Vulkan box
// (the amd-rdna3 tier) could not join the fleet at all — and the refusal text
// blamed the GPU rather than the probe. This file makes the memory source a
// resolved PROVIDER: nvidia-smi where it works, else the Windows-generic
// WDDM source (registry capacity + PDH adapter usage, vram_windows.go), with
// vendor/arch read from the installer's manifest instead of re-derived from
// NVIDIA product names. The serve gate becomes "no working GPU memory
// source", which is the fact the contract actually cares about.
package fleetnode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MemProbe returns the current GPU memory capacity/usage in GiB. It is the
// unit both the startup gate and the health sampler consume; every provider
// (nvidia-smi, windows-generic) reduces to one of these.
type MemProbe func() (totalGiB, usedGiB float64, err error)

// SmiProbe adapts the nvidia-smi CSV runner (the historical source) to a
// MemProbe via ParseSmiMemory.
func SmiProbe(run func() (string, error)) MemProbe {
	return func() (float64, float64, error) {
		out, err := run()
		if err != nil {
			return 0, 0, fmt.Errorf("nvidia-smi: %w", err)
		}
		return ParseSmiMemory(out)
	}
}

// InstalledInfo is the slice of the installer's manifest ($OFFLOAD_HOME/
// installed.json) the fleet node needs: the hardware profile detect.ps1
// classified (the vendor/arch/UMA truth — no re-derivation from product-name
// regexes) and the serving backend.
type InstalledInfo struct {
	Profile string `json:"profile"`
	Backend string `json:"backend"`
}

// ReadInstalledInfo loads InstalledInfo from an installed.json path. A missing
// or unparsable manifest returns a zero value and the error — callers treat
// that as "no manifest" (pre-installer box), never fatal by itself.
func ReadInstalledInfo(path string) (InstalledInfo, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return InstalledInfo{}, err
	}
	var m InstalledInfo
	if err := json.Unmarshal(b, &m); err != nil {
		return InstalledInfo{}, fmt.Errorf("installed.json: %w", err)
	}
	return m, nil
}

// InstalledJSONPath resolves where the installer wrote its manifest:
// $OFFLOAD_HOME/installed.json, defaulting OFFLOAD_HOME to ~/offload-stack
// exactly like install.ps1 does.
func InstalledJSONPath() string {
	home := os.Getenv("OFFLOAD_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(userHome, "offload-stack")
	}
	return filepath.Join(home, "installed.json")
}

// VendorArchFromProfile maps an installer profile id to the (vendor, arch)
// pair the health payload advertises. Pure; unit-tested. Unknown/empty
// profiles return ("unknown", "unknown") — honest, never a guessed vendor.
func VendorArchFromProfile(profile string) (vendor, arch string) {
	switch {
	case strings.HasPrefix(profile, "amd-rdna3"): // amd-rdna3 + amd-rdna3-dgpu
		return "amd", "rdna3"
	case profile == "amd-gcn":
		return "amd", "gcn"
	case strings.HasPrefix(profile, "blackwell-"):
		return "nvidia", "blackwell"
	case strings.HasPrefix(profile, "ampere-"):
		return "nvidia", "ampere"
	case profile == "volta-16":
		return "nvidia", "volta"
	case profile == "dual-gpu":
		return "nvidia", "blackwell" // the dual rig's primary; refined when measured
	case profile == "cpu":
		return "none", "none"
	default:
		return "unknown", "unknown"
	}
}

// UMAFromProfile reports whether the profile is a unified-memory iGPU class —
// which changes BOTH capacity composition (carve-out + WDDM shared budget)
// and usage composition (Dedicated + Shared). Pure; unit-tested. ok is false
// when the profile carries no UMA signal (caller falls back to a capacity
// heuristic).
func UMAFromProfile(profile string) (uma, ok bool) {
	switch profile {
	case "amd-rdna3", "amd-gcn":
		return true, true
	case "amd-rdna3-dgpu":
		return false, true
	default:
		if strings.HasPrefix(profile, "blackwell-") || strings.HasPrefix(profile, "ampere-") ||
			profile == "volta-16" || profile == "dual-gpu" {
			return false, true // discrete NVIDIA classes
		}
		return false, false
	}
}

// ResolvedProvider is what fleet-serve runs on after resolution: the probe
// that feeds the gate + sampler, the advertisement identity, the source name
// for the operator log line, and the selection probe's own reading (TotalGiB/
// UsedGiB — so the startup banner never re-executes or discards a probe).
type ResolvedProvider struct {
	Probe    MemProbe
	Vendor   string
	Arch     string
	Source   string // "nvidia-smi" | "windows-generic"
	TotalGiB float64
	UsedGiB  float64
}

// ResolveProvider picks the working GPU memory source. Order: nvidia-smi (the
// proven path — keeps every existing NVIDIA node byte-identical in behavior),
// else the injected generic provider (Windows WDDM; nil off-Windows, on a
// cpu-profile box, or in tests that exclude it). smiIdentity is called ONLY
// when nvidia-smi wins (it typically shells nvidia-smi for the product name —
// pointless on a box where smi just failed). Both probes failing is the new
// gate error: "no working GPU memory source". A note on transience: selection
// happens once at startup — a transiently failing nvidia-smi binds the generic
// provider for the process lifetime (restart re-resolves; documented in ADR
// 0014). Pure of I/O — probes, identity, and manifest info are injected, so
// selection is unit-testable.
func ResolveProvider(smi MemProbe, generic MemProbe, smiIdentity func() (vendor, arch string), info InstalledInfo) (ResolvedProvider, error) {
	var smiErr, genErr error
	if smi != nil {
		if total, used, err := smi(); err == nil {
			vendor, arch := "nvidia", "nvidia"
			if smiIdentity != nil {
				vendor, arch = smiIdentity()
			}
			return ResolvedProvider{Probe: smi, Vendor: vendor, Arch: arch, Source: "nvidia-smi", TotalGiB: total, UsedGiB: used}, nil
		} else {
			smiErr = err
		}
	}
	if generic != nil {
		if total, used, err := generic(); err == nil {
			vendor, arch := VendorArchFromProfile(info.Profile)
			return ResolvedProvider{Probe: generic, Vendor: vendor, Arch: arch, Source: "windows-generic", TotalGiB: total, UsedGiB: used}, nil
		} else {
			genErr = err
		}
	}
	return ResolvedProvider{}, fmt.Errorf("no working GPU memory source: nvidia-smi (%v); windows-generic (%v)", errOrAbsent(smiErr, smi == nil), errOrAbsent(genErr, generic == nil))
}

func errOrAbsent(err error, absent bool) string {
	if absent {
		return "not available on this platform"
	}
	if err == nil {
		return "not tried"
	}
	return err.Error()
}
