//go:build !windows

package gpugen

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// termGrace is how long a SIGTERMed process group has to clean up (the runner kills its engine
// and removes its temp dirs on SIGTERM) before the group is SIGKILLed. A var so tests can
// shorten it.
var termGrace = 5 * time.Second

// setProcessGroup makes cmd the leader of a new process group (Spec.OwnProcessGroup).
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killTree terminates p and its descendants. A process that leads its own group (Setpgid at
// spawn) has the whole group SIGTERMed so its runner can kill the engine it spawned and clean
// up, and SIGKILLed after termGrace if the runner is still alive. A process that does not lead
// a group (every lane that does not set Spec.OwnProcessGroup, the browse lane) is killed
// directly, exactly as before.
func killTree(p *os.Process) error {
	if p == nil {
		return nil
	}
	if err := syscall.Kill(-p.Pid, syscall.SIGTERM); err != nil {
		// no group led by p: a pid is never also a foreign group's id, so this is not a
		// stranger's group, just a process that is not a leader
		return p.Kill()
	}
	time.AfterFunc(termGrace, func() {
		// only while p itself is still alive (an exited and reaped p may have a recycled pid)
		if p.Signal(syscall.Signal(0)) == nil {
			_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
		}
	})
	return nil
}
