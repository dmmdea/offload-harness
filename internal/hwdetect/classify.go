// Package hwdetect classifies a machine into a hardware tier.
//
// The classifier lived in setup/detect.ps1, which begins by refusing to run:
//
//	if ($os -ne 'windows') { Write-Error 'This detector targets Windows.' ; exit 1 }
//
// So a Linux box could not be told what it IS, and everything downstream — the
// serving template, the resident tier, the media seed — had to be hand-derived. The
// two hand-derivations on the measured Linux node were both wrong in ways that broke
// chat, which is what a classifier exists to prevent.
//
// Classify is a straight port of Get-Profile, boundaries included, and is verified
// against the SAME table detect.tests.ps1 asserts (classify_test.go). Porting the
// rules rather than reinventing them keeps one answer per machine while both
// implementations exist; detect.ps1 stays the Windows install path until the wrapper
// work lands.
package hwdetect

import "strings"

// Facts are what a machine reports about itself. Everything the classifier needs and
// nothing it does not — so it stays pure and testable against synthetic tuples.
type Facts struct {
	// Vendor is nvidia|amd|none. Arch is blackwell|ampere|ada|volta|rdna3|gcn|other|none.
	Vendor string `json:"vendor"`
	Arch   string `json:"arch"`
	// VRAMGb is the DEDICATED VRAM of the primary GPU. On an AMD iGPU this is the
	// BIOS carve-out (0.5-4 GB), not the usable UMA pool — which is exactly why the
	// AMD branch bands on it rather than trusting it as capacity.
	VRAMGb   float64 `json:"vram_gb"`
	GPUCount int     `json:"gpu_count"`
	RAMGb    int     `json:"ram_gb"`
	// GPUName and DriverVersion are carried for the report; they never decide a tier.
	GPUName       string `json:"gpu_name,omitempty"`
	DriverVersion string `json:"driver_version,omitempty"`
	OS            string `json:"os,omitempty"`
}

// Verdict is the classification plus why, so an operator can see the band that
// caught their machine instead of taking the answer on faith.
type Verdict struct {
	Profile string `json:"profile"`
	// BigRAM is meaningful only for dual-gpu (the 128 GB variant): detection cannot
	// see the Optane drive that really distinguishes it, so RAM approximates it.
	BigRAM bool   `json:"big_ram"`
	Reason string `json:"reason"`
}

// Classify maps facts to a tier. A straight port of detect.ps1's Get-Profile —
// same order, same boundaries.
func Classify(f Facts) Verdict {
	vendor := strings.ToLower(f.Vendor)
	arch := strings.ToLower(f.Arch)

	// Multi-GPU with at least one NVIDIA outranks any single-card band: the
	// dual-resident rig serves two models at once rather than swapping.
	if f.GPUCount >= 2 && vendor == "nvidia" {
		big := f.RAMGb >= 120
		reason := "2+ GPUs with NVIDIA present -> the dual-resident rig"
		if big {
			reason += "; RAM >= 120 GB approximates the 128 GB variant (detection cannot see the Optane drive)"
		}
		return Verdict{Profile: "dual-gpu", BigRAM: big, Reason: reason}
	}

	if vendor == "nvidia" {
		switch arch {
		case "blackwell":
			switch {
			case f.VRAMGb >= 64:
				return band("blackwell-72", "blackwell, VRAM >= 64 GB (covers 72 GB and 96 GB until measured separately)")
			case f.VRAMGb >= 40:
				return band("blackwell-48", "blackwell, VRAM >= 40 GB")
			case f.VRAMGb >= 24:
				return band("blackwell-32", "blackwell, VRAM >= 24 GB")
			case f.VRAMGb >= 12:
				return band("blackwell-16", "blackwell, VRAM >= 12 GB")
			}
			return band("blackwell-8", "blackwell, VRAM < 12 GB")
		case "volta":
			return band("volta-16", "volta (V100-class) has its own serving profile")
		default:
			// ampere / ada / anything else NVIDIA share the ampere-* bands.
			switch {
			case f.VRAMGb >= 12:
				return band("ampere-16", "nvidia "+orUnknown(arch)+", VRAM >= 12 GB (3090-class)")
			case f.VRAMGb >= 7:
				return band("ampere-8", "nvidia "+orUnknown(arch)+", VRAM >= 7 GB (8 GB band)")
			}
			return band("ampere-6", "nvidia "+orUnknown(arch)+", VRAM < 7 GB (6 GB band)")
		}
	}

	if vendor == "amd" {
		if arch == "rdna3" {
			// An iGPU reports a small dedicated carve-out (0.5-4 GB) while its real
			// capacity is shared UMA; a discrete RDNA3 card reports true VRAM. Before
			// this band a 24 GB RX 7900 XTX silently took the iGPU floor profile.
			if f.VRAMGb >= 12 {
				return band("amd-rdna3-dgpu", "rdna3 with >= 12 GB dedicated VRAM -> discrete card, not an iGPU carve-out")
			}
			return band("amd-rdna3", "rdna3 with a small dedicated carve-out -> iGPU (real capacity is shared UMA)")
		}
		return band("amd-gcn", "amd "+orUnknown(arch)+" -> the weakest Vulkan path")
	}

	return band("cpu", "no usable GPU detected")
}

func band(profile, reason string) Verdict {
	return Verdict{Profile: profile, Reason: reason}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown arch"
	}
	return s
}
