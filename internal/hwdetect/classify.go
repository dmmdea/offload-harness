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
	// Archs lists EVERY detected GPU's architecture, in nvidia-smi row order. Arch
	// above is the primary (largest) card's; a homogeneity decision — "one sm_120
	// build serves both cards" — needs all of them, not the biggest.
	Archs []string `json:"archs,omitempty"`
}

// allArchs reports whether every detected GPU's architecture is exactly want.
// It is deliberately strict: it demands one entry PER counted GPU, so a probe
// that captured the count but not every card's identity cannot satisfy it.
func allArchs(f Facts, want string) bool {
	if len(f.Archs) != f.GPUCount || f.GPUCount == 0 {
		return false
	}
	for _, a := range f.Archs {
		if strings.ToLower(a) != want {
			return false
		}
	}
	return true
}

// Verdict is the classification plus why, so an operator can see the band that
// caught their machine instead of taking the answer on faith.
type Verdict struct {
	Profile string `json:"profile"`
	// BigRAM marks the >=120 GB RAM variant of the multi-GPU tiers (dual-gpu's
	// config #4 and blackwell-2x16): detection cannot see the Optane drive that
	// really distinguishes config #4, so RAM approximates it.
	BigRAM bool   `json:"big_ram"`
	Reason string `json:"reason"`
	// RAMTier gates the RAM-hungry 26B placements and the RAM-gated config_seed
	// overlay. detect.ps1 has always emitted it; this side reported raw ram_gb only,
	// so the Linux installer had nothing to pass and BOTH gates were inert there —
	// a tier served the 26B on a box with no RAM path for it, and the mid/high-only
	// image seed never applied.
	RAMTier string `json:"ram_tier"`
	// Accelerators lists additive devices found BESIDE the GPU (ADR 0024), e.g.
	// "hailo-8l". Classify never fills it — detection is a separate probe
	// (DetectAccelerators) the installers run and merge, so a box with no NPU
	// serialises exactly as before (omitempty).
	Accelerators []string `json:"accelerators,omitempty"`
}

// RAM tier boundaries, in GB. A straight port of detect.ps1's Get-RamTier, asserted
// against the same table its own self-test uses (128 high, 64 mid, 56 mid, 32 low,
// 16 min) so the two cannot drift.
const (
	ramHighGb = 120 // the 128 GB config
	ramMidGb  = 56  // 64 GB configs — this is what unlocks the 26B via --cpu-moe
	ramLowGb  = 28  // 32 GB
)

// RAMTier maps RAM size to the tier name the installers gate on.
func RAMTier(ramGb int) string {
	switch {
	case ramGb >= ramHighGb:
		return "high"
	case ramGb >= ramMidGb:
		return "mid"
	case ramGb >= ramLowGb:
		return "low"
	default:
		return "min"
	}
}

// Classify maps facts to a tier and its RAM tier. The RAM tier is stamped HERE, in
// one place, rather than at each return: the profile logic has early returns and a
// verdict that silently carried an empty ram_tier would leave both RAM gates inert
// without anything failing.
func Classify(f Facts) Verdict {
	v := classifyProfile(f)
	v.RAMTier = RAMTier(f.RAMGb)
	return v
}

// classifyProfile is the profile decision only — a straight port of detect.ps1's
// Get-Profile, same order, same boundaries.
func classifyProfile(f Facts) Verdict {
	vendor := strings.ToLower(f.Vendor)
	arch := strings.ToLower(f.Arch)

	// A homogeneous PAIR of 16GB-class Blackwell cards is its own hardware class,
	// decided BEFORE the generic multi-GPU rule: one sm_120 CUDA build serves both
	// cards (no multi-arch build), and the serving topology is per-card pinning on
	// an asymmetric-bandwidth pair — neither of which the heterogeneous dual-gpu
	// row (5060 Ti + V100, architect/editor) describes. STRICT on all three axes:
	// ALL archs captured and blackwell (a pair whose second card is unknown falls
	// through to dual-gpu), AND the primary card in the 16GB band — the "16" in the
	// tier name is the template's load-bearing assumption (it pins the 26B and a
	// ~10GB vision seat per card), so a 2x 8GB or 2x 32GB pair must not claim it.
	// VRAMGb is the LARGEST card's, so a mixed 16+8 pair passes this band check;
	// catching that fully needs per-GPU VRAM capture — recorded as a known limit.
	if f.GPUCount == 2 && vendor == "nvidia" && allArchs(f, "blackwell") &&
		f.VRAMGb >= 12 && f.VRAMGb < 24 {
		return Verdict{Profile: "blackwell-2x16", BigRAM: f.RAMGb >= 120,
			Reason: "2 GPUs, both Blackwell, 16GB band -> the homogeneous dual-sm_120 tier"}
	}

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

// AcceleratorsFromHailortcli classifies the Hailo device from hailortcli's own
// output: `scan` must list at least one "Device:" line and `identify` must
// report the 8L architecture. A full Hailo-8 is deliberately NOT matched — its
// model-zoo HEFs are a different build (hailo8/ vs hailo8l/) and would be
// rejected at load with an architecture mismatch.
func AcceleratorsFromHailortcli(scanOut, identifyOut string) []string {
	if !strings.Contains(scanOut, "Device:") {
		return nil
	}
	for _, line := range strings.Split(identifyOut, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == "Device Architecture" && strings.TrimSpace(v) == "HAILO8L" {
			return []string{"hailo-8l"}
		}
	}
	return nil
}

// DetectAccelerators probes for accelerators with an injected runner so the
// decision stays pure and testable. run executes `hailortcli <args...>`; any
// error (tool absent, driver down) means "no accelerator" — never a failure,
// because a missing NPU is the normal case on most boxes.
func DetectAccelerators(run func(args ...string) (string, error)) []string {
	scan, err := run("scan")
	if err != nil {
		return nil
	}
	ident, err := run("fw-control", "identify")
	if err != nil {
		return nil
	}
	return AcceleratorsFromHailortcli(scan, ident)
}
