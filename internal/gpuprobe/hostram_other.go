//go:build !windows && !linux

package gpuprobe

// hostRAMSupported tells the test whether a !ok from HostFreeRAMGiB is a
// missing reader (fine) or a broken one (a failure).
const hostRAMSupported = false

// hostFreeRAMGiB has no reader here: the guard that needs it fails closed.
func hostFreeRAMGiB() (float64, bool) { return 0, false }
