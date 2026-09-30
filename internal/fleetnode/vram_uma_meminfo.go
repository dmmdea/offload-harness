// vram_uma_meminfo.go — the GPU memory provider for a unified-memory SoC (the
// rockchip-rk3588 tier). fleet-serve refused to start there: the only Linux source
// was AmdgpuSysfsProbe, which needs a PCI vendor id and mem_info_vram_total, and an
// RK3588's Mali GPU and NPU expose neither — they allocate from the same RAM as the
// CPU, so there is no VRAM counter to read.
//
// The honest capacity of such a node is not MemTotal. The box has another job (a
// home-automation stack on the reference board) that must keep its memory, so the
// operator names a reserve (uma_reserve_gib) and the node advertises what is left:
//
//	total = MemTotal     - reserve   the inference budget
//	free  = MemAvailable - reserve   clamped to [0, total]
//	used  = total - free
//
// free is conservative on purpose. Memory the host workload already holds is
// subtracted from MemAvailable AND the reserve stays set aside, so it is counted
// twice: the node may under-advertise what inference could take, and can never
// over-advertise it. Nothing here caps what a seat actually allocates — the budget
// is what /fleet/health tells the dispatcher, as with every other provider.
//
// The file has no build tag on purpose: it reads one plain file under an injected
// root, so the parser and the probe are unit-tested on every OS, like
// AmdgpuSysfsProbe; main.go names the source only for the rockchip-rk3588 tier.
package fleetnode

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// MeminfoSource is the provider's label in the serve banner and the gate error.
const MeminfoSource = "linux-meminfo"

// MeminfoUMAProbe returns a MemProbe over <root>/proc/meminfo (root is "/" in
// production, a temp tree in tests) with reserveGiB held back for the host. A zero
// reserve advertises all of RAM. Every read is one small procfs file, so the probe
// is safe to call from the 2 s health sampler.
func MeminfoUMAProbe(root string, reserveGiB float64) MemProbe {
	return func() (float64, float64, error) {
		b, err := os.ReadFile(filepath.Join(root, "proc", "meminfo"))
		if err != nil {
			return 0, 0, fmt.Errorf("%s: %w", MeminfoSource, err)
		}
		return meminfoUMAGiB(string(b), reserveGiB)
	}
}

// meminfoUMAGiB composes capacity and usage from a /proc/meminfo body. A total at or
// below zero — a reserve that swallows the box — is a failed probe (the contract
// treats vram_total_gb <= 0 as one), and a missing MemAvailable is an error, never
// zero: zero would read as "every byte in use".
func meminfoUMAGiB(meminfo string, reserveGiB float64) (totalGiB, usedGiB float64, err error) {
	if reserveGiB < 0 {
		return 0, 0, fmt.Errorf("%s: uma_reserve_gib %v is negative — a reserve holds memory back, it cannot add any", MeminfoSource, reserveGiB)
	}
	memTotal, err := meminfoKiB(meminfo, "MemTotal")
	if err != nil {
		return 0, 0, err
	}
	memAvailable, err := meminfoKiB(meminfo, "MemAvailable")
	if err != nil {
		return 0, 0, err
	}
	const kibPerGiB = 1 << 20 // /proc/meminfo publishes kB, which the kernel means as KiB
	total := float64(memTotal)/kibPerGiB - reserveGiB
	if total <= 0 {
		return 0, 0, fmt.Errorf("%s: MemTotal %.2f GiB minus a %.2f GiB reserve leaves nothing to advertise (contract: total <= 0 = failed probe)",
			MeminfoSource, float64(memTotal)/kibPerGiB, reserveGiB)
	}
	free := float64(memAvailable)/kibPerGiB - reserveGiB
	if free < 0 {
		free = 0 // the host already holds more than its reserve: nothing is free for inference
	}
	if free > total {
		free = total // a MemAvailable above MemTotal is a driver race, not spare capacity
	}
	return total, total - free, nil
}

// meminfoKiB reads one "Key:   <n> kB" line. A missing key, a non-numeric value or a
// negative one is an error naming the key.
func meminfoKiB(meminfo, key string) (int64, error) {
	for _, line := range strings.Split(meminfo, "\n") {
		rest, ok := strings.CutPrefix(line, key+":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, fmt.Errorf("%s: %s has no value", MeminfoSource, key)
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %s %q: %w", MeminfoSource, key, fields[0], err)
		}
		if n < 0 {
			return 0, fmt.Errorf("%s: %s is negative (%d)", MeminfoSource, key, n)
		}
		return n, nil
	}
	return 0, fmt.Errorf("%s: no %s line in /proc/meminfo", MeminfoSource, key)
}
