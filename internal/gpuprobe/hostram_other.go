//go:build !windows && !linux

package gpuprobe

// hostRAMSupported tells the test whether a !ok from ReadHostMemory is a
// missing reader (fine) or a broken one (a failure).
const hostRAMSupported = false

// readHostMemory has no reader here: the placement guard that needs the available figure fails
// closed, and the lease admission rule (HostRAMAdmits) admits, because it cannot judge.
func readHostMemory() (HostMemory, bool) { return HostMemory{}, false }
