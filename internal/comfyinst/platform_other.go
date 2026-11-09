//go:build !windows

package comfyinst

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
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

// lookupTimeout bounds each process or socket lookup, so a stuck system call cannot stall a release.
const lookupTimeout = 15 * time.Second

var ssPID = regexp.MustCompile(`pid=(d+)`)

// listenerPIDs lists the pids that listen on a TCP port (`ss -ltnp`); 0 is a listener the system cannot name an owner for.
func listenerPIDs(port int) ([]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ss", "-ltnp").Output()
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, line := range strings.Split(string(out), string(rune(10))) {
		f := strings.Fields(line)
		if len(f) < 4 || !strings.EqualFold(f[0], "LISTEN") || !strings.HasSuffix(f[3], ":"+strconv.Itoa(port)) {
			continue
		}
		ms := ssPID.FindAllStringSubmatch(line, -1)
		if len(ms) == 0 {
			pids = append(pids, 0)
		}
		for _, m := range ms {
			if n, e := strconv.Atoi(m[1]); e == nil {
				pids = append(pids, n)
			}
		}
	}
	return pids, nil
}

// commandLine is the command line of pid, from /proc.
func commandLine(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", err
	}
	return strings.Join(strings.Split(strings.TrimRight(string(b), string(rune(0))), string(rune(0))), " "), nil
}

// parentPID is the parent process id of pid, from /proc/<pid>/stat (counted from the last ")": the name may hold anything).
func parentPID(pid int) (int, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	s := string(b)
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return 0, fmt.Errorf("cannot parse /proc/%d/stat", pid)
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0, fmt.Errorf("cannot parse /proc/%d/stat", pid)
	}
	return strconv.Atoi(f[1])
}
