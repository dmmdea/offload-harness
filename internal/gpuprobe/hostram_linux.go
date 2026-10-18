//go:build linux

package gpuprobe

import "os"

// hostRAMSupported tells the test whether a !ok from ReadHostMemory is a
// missing reader (fine) or a broken one (a failure).
const hostRAMSupported = true

// readHostMemory reads /proc/meminfo and vm.overcommit_memory (readHostMemoryLinux). MemAvailable is the kernel's own estimate
// of what can be allocated without swapping (page cache that can be dropped counts, unlike MemFree), which is what a multi-GB
// expert load needs; Committed_AS and CommitLimit are the kernel's commit accounting (parseMeminfo says why Committed_AS reads
// high on a CUDA host), a limit only in strict overcommit mode.
func readHostMemory() (HostMemory, bool) { return readHostMemoryLinux(os.ReadFile) }
