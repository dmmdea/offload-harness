//go:build windows

package gpugen

import (
	"os"
	"os/exec"
	"strconv"
)

// setProcessGroup is a no-op on Windows: taskkill /T already reaps the whole tree.
func setProcessGroup(*exec.Cmd) {}

// killTree force-terminates p and ALL descendants. Killing the bare node process leaves the
// spawned ComfyUI python (or an iGPU engine) alive (no process-group semantics), so we
// taskkill the whole tree.
func killTree(p *os.Process) error {
	if p == nil {
		return nil
	}
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid)).Run()
	return nil
}
