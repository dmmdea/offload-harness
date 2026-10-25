//go:build linux

package forecastserve

import (
	"os/exec"
	"runtime"
	"syscall"
)

// startBound starts cmd with Pdeathsig SIGKILL: the kernel kills the child when this process
// dies, however it dies. The signal is tied to the thread that started the child, so the calling
// goroutine stays locked to its thread for as long as the launcher runs. stop asks the child to
// terminate.
func startBound(cmd *exec.Cmd) (release, stop func(), err error) {
	runtime.LockOSThread()
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		runtime.UnlockOSThread()
		return nil, nil, err
	}
	return func() {}, func() { _ = cmd.Process.Signal(syscall.SIGTERM) }, nil
}
