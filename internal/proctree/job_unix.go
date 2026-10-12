//go:build !windows

package proctree

import (
	"errors"
	"os/exec"
	"syscall"
)

// supportsDieWithWrapper: no. A process group holds no handle whose closing ends it, and
// PR_SET_PDEATHSIG is delivered per thread and covers the direct child only.
const supportsDieWithWrapper = false

// platform is the Unix containment of one Tree: the command leads its own session, so its pid is
// also the id of the process group every descendant that does not leave it belongs to.
type platform struct {
	pgid int
}

func (p *platform) contained() bool { return p.pgid > 0 }

func (p *platform) mechanism() string {
	if p.pgid > 0 {
		return "process group"
	}
	return "none"
}

func start(cmd *exec.Cmd, opts Options) (*Tree, error) {
	if opts.DieWithWrapper {
		return nil, ErrDieWithWrapperUnsupported
	}
	// Setsid, not just Setpgid: a command in a background process group that reads the
	// terminal is stopped by SIGTTIN, and a wrapper started from a shell would hang its
	// command. Its own session has no controlling terminal to be stopped by; the wrapper
	// relays an interrupt by stopping the tree itself.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	t := &Tree{cmd: cmd}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	t.p.pgid = cmd.Process.Pid
	return t, nil
}

func (p *platform) pids(t *Tree) ([]int, error) {
	if p.pgid <= 0 {
		return nil, ErrNotContained
	}
	return tableMembers(p.pgid, t.cmd.Process.Pid)
}

func (p *platform) terminate(t *Tree) error {
	if p.pgid <= 0 {
		return ErrNotContained
	}
	// The group first (everything that stayed in it), then any member found through the parent
	// chain that left it.
	err := syscall.Kill(-p.pgid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		err = nil
	}
	if pids, perr := tableMembers(p.pgid, t.cmd.Process.Pid); perr == nil {
		for _, pid := range pids {
			if kerr := syscall.Kill(pid, syscall.SIGKILL); kerr != nil && !errors.Is(kerr, syscall.ESRCH) && err == nil {
				err = kerr
			}
		}
	}
	return err
}

func (p *platform) terminateExcept(t *Tree, protect map[int]bool) ([]int, error) {
	if p.pgid <= 0 {
		return nil, ErrNotContained
	}
	pids, err := tableMembers(p.pgid, t.cmd.Process.Pid)
	if err != nil {
		return nil, err
	}
	var left []int
	var firstErr error
	for _, pid := range pids {
		if protect[pid] {
			left = append(left, pid)
			continue
		}
		if kerr := syscall.Kill(pid, syscall.SIGKILL); kerr != nil && !errors.Is(kerr, syscall.ESRCH) && firstErr == nil {
			firstErr = kerr
		}
	}
	return left, firstErr
}

// killPID ends one process by pid with SIGKILL. A process that is already gone is not an error.
func killPID(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// close has nothing to release: a process group holds no handle, which is also why a Unix
// wrapper cannot make its tree die with it.
func (p *platform) close() error { return nil }

// KillsOnClose is always false on Unix: closing a process group ends nothing.
func (t *Tree) KillsOnClose() bool { return false }
