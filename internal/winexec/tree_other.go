//go:build !windows

package winexec

import (
	"os"
	"os/exec"
	"syscall"
)

// KillTreeOnCancel makes the cancellation of cmd's context (cmd comes from CommandContext) end the whole process
// group the program started, not only the program: a wrapper script leaves what it backgrounded running, with the
// output pipe it inherited held, after the script alone is killed. The program is started as the leader of a group
// of its own (Setpgid) so the group can be signalled without touching this process's. It sets cmd.Cancel and returns
// cmd. Call it before the command starts.
func KillTreeOnCancel(cmd *exec.Cmd) *exec.Cmd {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error { return KillTree(cmd.Process) }
	return cmd
}

// KillTree sends SIGKILL to the process group p leads, which is every process it started that has not left the
// group. When p leads no group (it was not started with Setpgid) p alone is killed.
func KillTree(p *os.Process) error {
	if p == nil {
		return nil
	}
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil {
		return p.Kill()
	}
	return nil
}
