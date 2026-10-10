//go:build linux

package gpuprobe

import "os"

// hostRAMSupported tells the test whether a !ok from ReadHostMemory is a
// missing reader (fine) or a broken one (a failure).
const hostRAMSupported = true

// readHostMemory reads /proc/meminfo. MemAvailable is the kernel's own estimate of what can be
// allocated without swapping (page cache that can be dropped counts, unlike MemFree), which is
// what a multi-GB expert load needs; Committed_AS and CommitLimit are the kernel's commit
// accounting (parseMeminfo says why Committed_AS reads high on a CUDA host).
func readHostMemory() (HostMemory, bool) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return HostMemory{}, false
	}
	return parseMeminfo(string(b))
}
