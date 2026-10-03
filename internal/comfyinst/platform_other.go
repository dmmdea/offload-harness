//go:build !windows

package comfyinst

import (
	"errors"
	"os"
	"syscall"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// terminateGrace is how long a SIGTERM gets before it becomes a SIGKILL.
const terminateGrace = 5 * time.Second

// terminate stops pid: SIGTERM first, so python can unwind and release its CUDA context, then
// SIGKILL if the process is still there after terminateGrace.
func terminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	deadline := time.Now().Add(terminateGrace)
	for time.Now().Before(deadline) {
		if !gpulease.PIDAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// processStartUnixMs: the start time gpulease reads on Linux is clock ticks since boot, not a
// Unix time, so it cannot be compared with the marker's timestamp. The recycled-pid check is
// defence in depth behind the argv fingerprint, so it is skipped here rather than guessed.
func processStartUnixMs(int) (int64, bool) { return 0, false }
