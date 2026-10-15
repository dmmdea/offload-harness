//go:build windows

package winexec

import (
	"os"
	"os/exec"
	"strconv"
)

// KillTreeOnCancel makes the cancellation of cmd's context (cmd comes from CommandContext) end the whole process
// tree the program started, not only the program: a wrapper such as a .cmd file leaves the interpreter it ran as a
// grandchild, and that grandchild keeps running, with the working directory it inherited held, after the wrapper alone
// is killed. It sets cmd.Cancel and returns cmd. Call it before the command starts.
func KillTreeOnCancel(cmd *exec.Cmd) *exec.Cmd {
	cmd.Cancel = func() error { return KillTree(cmd.Process) }
	return cmd
}

// KillTree force-terminates p and every process descended from it, with taskkill /T /F started hidden. The tree is
// found through the parent chain, so it can only be walked while p is still running: a descendant whose parent has
// already exited is out of reach. When taskkill cannot be run, or finds nothing to end, p alone is killed.
func KillTree(p *os.Process) error {
	if p == nil {
		return nil
	}
	if err := Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid)).Run(); err != nil {
		return p.Kill()
	}
	return nil
}
