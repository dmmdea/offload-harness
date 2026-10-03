//go:build linux

package gpulease

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// RunningImages lists the image of every running process for which want returns a
// non-empty reason (see the Windows variant). The image is read through /proc/<pid>/exe,
// which opens a binary that was replaced or deleted after the process started: that is
// exactly the copy that matters.
func RunningImages(want func(pid int, exeName string) string) ([]ProcImage, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("reading /proc: %w", err)
	}
	var out []ProcImage
	for _, e := range entries {
		pid, perr := strconv.Atoi(e.Name())
		if perr != nil {
			continue
		}
		link := filepath.Join("/proc", e.Name(), "exe")
		target, lerr := os.Readlink(link)
		name := filepath.Base(target)
		if lerr != nil {
			// A process we may not inspect: judge it by its comm name alone.
			if b, cerr := os.ReadFile(filepath.Join("/proc", e.Name(), "comm")); cerr == nil {
				name = string(b)
			}
		}
		why := want(pid, name)
		if why == "" {
			continue
		}
		im := ProcImage{PID: pid, Why: why}
		if lerr == nil {
			im.Path, im.ReadPath = target, link
		}
		out = append(out, im)
	}
	return out, nil
}
