package hwdetect

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Detect reports what THIS machine is. nvidia-smi is authoritative when present
// (it names the card, its dedicated VRAM and the driver); otherwise a per-OS
// fallback finds an AMD or unknown adapter, because a box that cannot be classified
// must not be silently called "cpu" — that would hand an AMD laptop the weakest
// possible profile.
//
// Every probe is read-only and bounded: no model is loaded and nothing is written.
func Detect() Facts {
	f := Facts{Vendor: "none", Arch: "none", OS: runtime.GOOS, RAMGb: ramGb()}
	if smi := probeNvidiaSMI(); smi.GPUCount > 0 {
		smi.OS, smi.RAMGb = f.OS, f.RAMGb
		return smi
	}
	if name, vram := probeFallbackGPU(); name != "" {
		f.GPUName = name
		f.Vendor = VendorFromName(name)
		f.Arch = ArchFromName(name)
		f.VRAMGb = vram
		if f.Vendor != "none" {
			f.GPUCount = 1
		}
	}
	return f
}

// probeNvidiaSMI asks the driver directly. Multi-GPU rigs report one row per card;
// the tier bands on the LARGEST card, which is the one a single-model load lands on.
func probeNvidiaSMI() Facts {
	out, err := run(6*time.Second, "nvidia-smi",
		"--query-gpu=name,memory.total,driver_version", "--format=csv,noheader,nounits")
	if err != nil || strings.TrimSpace(out) == "" {
		return Facts{}
	}
	var f Facts
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		cols := strings.Split(line, ",")
		if len(cols) < 2 {
			continue
		}
		name := strings.TrimSpace(cols[0])
		mib, _ := strconv.ParseFloat(strings.TrimSpace(cols[1]), 64)
		gb := mib / 1024
		f.GPUCount++
		if gb > f.VRAMGb {
			f.VRAMGb, f.GPUName, f.Arch = gb, name, ArchFromName(name)
			if len(cols) >= 3 {
				f.DriverVersion = strings.TrimSpace(cols[2])
			}
		}
	}
	if f.GPUCount > 0 {
		f.Vendor = "nvidia"
	}
	return f
}

// probeFallbackGPU finds a non-NVIDIA adapter. It matters for exactly the case the
// Windows-only detector never had to handle: an AMD box, where guessing "cpu" would
// cost the machine its whole Vulkan serving path.
func probeFallbackGPU() (name string, vramGb float64) {
	if runtime.GOOS == "windows" {
		out, err := run(15*time.Second, "powershell", "-NoProfile", "-Command",
			"Get-CimInstance Win32_VideoController | Sort-Object AdapterRAM -Descending | Select-Object -First 1 -Property Name,AdapterRAM | ForEach-Object { \"$($_.Name)|$($_.AdapterRAM)\" }")
		if err != nil {
			return "", 0
		}
		parts := strings.SplitN(strings.TrimSpace(out), "|", 2)
		if len(parts) == 2 {
			b, _ := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
			return strings.TrimSpace(parts[0]), b / (1 << 30)
		}
		return strings.TrimSpace(parts[0]), 0
	}

	// Linux: the DRM sysfs tree names the vendor by PCI id and, on amdgpu, exposes
	// the dedicated VRAM size — the carve-out an iGPU reports, which is precisely
	// what the AMD band wants.
	cards, _ := filepath.Glob("/sys/class/drm/card[0-9]*/device")
	for _, dev := range cards {
		vendor, err := os.ReadFile(filepath.Join(dev, "vendor"))
		if err != nil {
			continue
		}
		var label string
		switch strings.TrimSpace(string(vendor)) {
		case "0x1002":
			label = "AMD Radeon"
		case "0x10de":
			label = "NVIDIA"
		default:
			continue
		}
		if b, err := os.ReadFile(filepath.Join(dev, "mem_info_vram_total")); err == nil {
			if n, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil {
				vramGb = n / (1 << 30)
			}
		}
		// lspci gives the marketing name the arch rules match on; without it the
		// vendor label alone still yields the right vendor and an "other" arch.
		if out, err := run(6*time.Second, "sh", "-c",
			"lspci -mm 2>/dev/null | grep -iE 'vga|3d|display' | head -1 | cut -d'\"' -f6"); err == nil {
			if s := strings.TrimSpace(out); s != "" {
				label = s
			}
		}
		return label, vramGb
	}
	return "", 0
}

// ramGb is total physical memory, rounded down — the dual-gpu band keys on it.
func ramGb() int {
	if runtime.GOOS == "windows" {
		out, err := run(15*time.Second, "powershell", "-NoProfile", "-Command",
			"(Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory")
		if err == nil {
			if b, err := strconv.ParseFloat(strings.TrimSpace(out), 64); err == nil {
				return int(b / (1 << 30))
			}
		}
		return 0
	}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if !strings.HasPrefix(sc.Text(), "MemTotal:") {
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 {
			kb, _ := strconv.ParseFloat(fields[1], 64)
			return int(kb / (1 << 20))
		}
	}
	return 0
}

func run(timeout time.Duration, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.Output(); close(done) }()
	select {
	case <-done:
		return string(out), err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return "", context_DeadlineExceeded
	}
}

// context_DeadlineExceeded keeps the probe dependency-free while still being a
// distinct, comparable error.
var context_DeadlineExceeded = errTimeout("hwdetect: probe timed out")

type errTimeout string

func (e errTimeout) Error() string { return string(e) }
