//go:build !linux && !windows

package hostsample

func readCPU() (cpuTimes, bool) {
	return cpuTimes{}, false
}

func readRAM() (usedGiB, totalGiB float64, ok bool) {
	return 0, 0, false
}
