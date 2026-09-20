// vram_linux_amdgpu.go — the Linux GPU memory provider ADR 0014 left as "a
// future seam". Off-Windows, a non-NVIDIA box had NO generic source, so an AMD
// APU that detect classified into a MEASURED tier (amd-gcn) and that llama-swap
// already served could not fleet-serve at all: "no working GPU memory source:
// nvidia-smi (…); windows-generic (windows-generic GPU memory source requires
// WDDM)" — binxarn, Ryzen 5 5625U / Vega 7, 2026-09-20.
//
// The amdgpu kernel driver publishes exactly the two numbers the provider needs,
// per card, under /sys/class/drm/card*/device:
//
//	mem_info_vram_total / mem_info_vram_used   the carve-out ("VRAM") and its use
//	mem_info_gtt_total  / mem_info_gtt_used    the GTT pool: system RAM the GPU
//	                                           may map — on an APU this IS the
//	                                           shared budget ADR 0014 approximates
//	                                           with RAM/2 on Windows
//
// So the UMA composition is the same as the Windows-generic one (capacity =
// carve-out + shared budget, usage = dedicated + shared), but with the driver's
// own budget instead of a heuristic. A discrete AMD card (uma=false) reports its
// VRAM alone, exactly like nvidia-smi does for an NVIDIA card.
//
// The file has no build tag on purpose: it reads plain files under an injected
// root, so the parser and the probe are unit-tested on every OS; main.go wires it
// only on linux.
package fleetnode

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// amdPCIVendor is the PCI vendor id the DRM sysfs tree reports for AMD/ATI.
const amdPCIVendor = "0x1002"

// AmdgpuSysfsProbe returns a MemProbe over the FIRST amdgpu card under drmRoot
// (/sys/class/drm in production; a temp tree in tests). uma selects the memory
// model: true (amd-gcn, amd-rdna3 APUs) adds the GTT pool to both capacity and
// usage; false (amd-rdna3-dgpu) reports VRAM alone. Every read is one small
// sysfs file; the probe is safe to call from the 2 s health sampler.
func AmdgpuSysfsProbe(drmRoot string, uma bool) MemProbe {
	return func() (float64, float64, error) {
		dev, err := firstAmdgpuDevice(drmRoot)
		if err != nil {
			return 0, 0, err
		}
		return amdgpuMemGiB(dev, uma)
	}
}

// AmdgpuSysfsDeviceProbe is the per-device sibling (gpu_devices[] in health):
// every amdgpu card under drmRoot, VRAM-only capacity/usage plus the GTT pool
// folded in when uma is true — the same composition as the MemProbe so the
// headline pair and the device list never disagree.
func AmdgpuSysfsDeviceProbe(drmRoot string, uma bool) DeviceProbe {
	return func() ([]GPUDevice, error) {
		devs, err := amdgpuDevices(drmRoot)
		if err != nil {
			return nil, err
		}
		var out []GPUDevice
		for i, dev := range devs {
			total, used, err := amdgpuMemGiB(dev, uma)
			if err != nil {
				return nil, err
			}
			out = append(out, GPUDevice{Index: i, Name: amdgpuName(dev), TotalGiB: total, FreeGiB: total - used})
		}
		return out, nil
	}
}

// amdgpuDevices lists every /sys/class/drm/card*/device whose PCI vendor is AMD
// AND which exposes mem_info_vram_total (a display-only card node or a render
// node alias without the amdgpu memory files is skipped, not an error).
func amdgpuDevices(drmRoot string) ([]string, error) {
	cards, _ := filepath.Glob(filepath.Join(drmRoot, "card[0-9]*", "device"))
	sort.Strings(cards)
	var devs []string
	for _, dev := range cards {
		vendor, err := os.ReadFile(filepath.Join(dev, "vendor"))
		if err != nil || strings.TrimSpace(string(vendor)) != amdPCIVendor {
			continue
		}
		if _, err := os.Stat(filepath.Join(dev, "mem_info_vram_total")); err != nil {
			continue
		}
		devs = append(devs, dev)
	}
	if len(devs) == 0 {
		return nil, fmt.Errorf("linux-amdgpu: no amdgpu device with mem_info_vram_total under %s", drmRoot)
	}
	return devs, nil
}

func firstAmdgpuDevice(drmRoot string) (string, error) {
	devs, err := amdgpuDevices(drmRoot)
	if err != nil {
		return "", err
	}
	return devs[0], nil
}

// amdgpuMemGiB composes capacity/usage for one device. A zero VRAM total is a
// failed probe (the contract treats vram_total_gb <= 0 as such); used is
// clamped at total so a driver race can never advertise negative free space.
func amdgpuMemGiB(dev string, uma bool) (totalGiB, usedGiB float64, err error) {
	vramTotal, err := sysfsBytes(dev, "mem_info_vram_total")
	if err != nil {
		return 0, 0, err
	}
	vramUsed, err := sysfsBytes(dev, "mem_info_vram_used")
	if err != nil {
		return 0, 0, err
	}
	total, used := vramTotal, vramUsed
	if uma {
		gttTotal, err := sysfsBytes(dev, "mem_info_gtt_total")
		if err != nil {
			return 0, 0, err
		}
		gttUsed, err := sysfsBytes(dev, "mem_info_gtt_used")
		if err != nil {
			return 0, 0, err
		}
		total += gttTotal
		used += gttUsed
	}
	if total <= 0 {
		return 0, 0, fmt.Errorf("linux-amdgpu: %s reports a zero memory total — not a working GPU (contract: total <= 0 = failed probe)", dev)
	}
	if used > total {
		used = total
	}
	const gib = 1 << 30
	return float64(total) / gib, float64(used) / gib, nil
}

func sysfsBytes(dev, name string) (int64, error) {
	b, err := os.ReadFile(filepath.Join(dev, name))
	if err != nil {
		return 0, fmt.Errorf("linux-amdgpu: %w", err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("linux-amdgpu: %s/%s: %w", dev, name, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("linux-amdgpu: %s/%s is negative (%d)", dev, name, n)
	}
	return n, nil
}

// amdgpuName is the best available product string: the driver's product_name
// (newer kernels) else "amdgpu <device-id>" — a label for the device list,
// never a tier decision (that comes from the installer manifest).
func amdgpuName(dev string) string {
	if b, err := os.ReadFile(filepath.Join(dev, "product_name")); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	if b, err := os.ReadFile(filepath.Join(dev, "device")); err == nil {
		return "amdgpu " + strings.TrimSpace(string(b))
	}
	return "amdgpu"
}

// GenericProvider names the OS's generic memory source so the serve banner and
// the gate error say which one was tried — "windows-generic" on Windows,
// "linux-amdgpu" on Linux — instead of hardcoding the Windows name.
type GenericProvider struct {
	Probe  MemProbe
	Source string
}

// ResolveProviderNamed is ResolveProvider with the generic source named by the
// caller. ResolveProvider keeps its signature (and its "windows-generic" label)
// so every existing caller and test stays byte-identical.
func ResolveProviderNamed(smi MemProbe, generic GenericProvider, smiIdentity func() (vendor, arch string), info InstalledInfo) (ResolvedProvider, error) {
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
	name := generic.Source
	if name == "" {
		name = "generic"
	}
	if generic.Probe != nil {
		if total, used, err := generic.Probe(); err == nil {
			vendor, arch := VendorArchFromProfile(info.Profile)
			return ResolvedProvider{Probe: generic.Probe, Vendor: vendor, Arch: arch, Source: name, TotalGiB: total, UsedGiB: used}, nil
		} else {
			genErr = err
		}
	}
	return ResolvedProvider{}, fmt.Errorf("no working GPU memory source: nvidia-smi (%v); %s (%v)", errOrAbsent(smiErr, smi == nil), name, errOrAbsent(genErr, generic.Probe == nil))
}
