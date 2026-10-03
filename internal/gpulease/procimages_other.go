//go:build !windows && !linux

package gpulease

import "errors"

// RunningImages is not implemented on this platform. The audit treats that as an
// unreadable process table, which keeps the host off green: it must not read as "no
// harness process is running".
func RunningImages(want func(pid int, exeName string) string) ([]ProcImage, error) {
	return nil, errors.New("listing running process images is not supported on this platform")
}
