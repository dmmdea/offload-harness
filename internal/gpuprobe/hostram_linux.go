//go:build linux

package gpuprobe

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// hostRAMSupported tells the test whether a !ok from HostFreeRAMGiB is a
// missing reader (fine) or a broken one (a failure).
const hostRAMSupported = true

// hostFreeRAMGiB reads /proc/meminfo MemAvailable — the kernel's own estimate
// of what can be allocated without swapping (page cache that can be dropped
// counts, unlike MemFree), which is what a multi-GB expert load needs.
func hostFreeRAMGiB() (float64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || fields[0] != "MemAvailable:" {
			continue
		}
		kb, err := strconv.ParseFloat(fields[1], 64)
		if err != nil || kb <= 0 {
			return 0, false
		}
		return kb / (1 << 20), true
	}
	return 0, false
}
