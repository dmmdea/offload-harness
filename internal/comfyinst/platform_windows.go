//go:build windows

package comfyinst

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// lookupTimeout bounds each process or socket lookup, so a stuck system call cannot stall a release.
const lookupTimeout = 15 * time.Second

// terminate stops pid and everything it started (taskkill /T /F): ComfyUI's venv launcher runs the real python as a child, and a
// hung instance is often the child, so ending the one pid left the port held. Falls back to TerminateProcess on the pid when
// taskkill cannot run.
func terminate(pid int) error {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Run(); err == nil {
		return nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// processStartUnixMs is when pid began, in Unix milliseconds. On Windows the creation time
// gpulease records is exactly that.
func processStartUnixMs(pid int) (int64, bool) { return gpulease.ProcessStart(pid) }

func run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// listenerPIDs lists the pids that listen on a TCP port (0 = an owner the system cannot name). It asks the NetTCPConnection cmdlet
// by port and keeps the sockets with no remote end: -State Listen matched nothing for a live listener on a current Windows 11
// build, which would read as "no listener". If PowerShell cannot answer, the LISTENING rows of netstat -ano are parsed instead.
func listenerPIDs(port int) ([]int, error) {
	script := fmt.Sprintf("try { Get-NetTCPConnection -LocalPort %d -ErrorAction Stop | Where-Object { $_.RemotePort -eq 0 } | ForEach-Object { $_.OwningProcess } } catch { if ($_.CategoryInfo.Category -ne 'ObjectNotFound') { throw } }; exit 0", port)
	out, err := run("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	if err != nil {
		nout, nerr := run("netstat", "-ano", "-a", "-p", "tcp")
		if nerr != nil {
			return nil, fmt.Errorf("powershell: %v; netstat: %v", err, nerr)
		}
		return parseNetstatListeners(nout, port), nil
	}
	var pids []int
	for _, f := range strings.Fields(out) {
		if n, e := strconv.Atoi(f); e == nil {
			pids = append(pids, n)
		}
	}
	return pids, nil
}

// parseNetstatListeners reads the LISTENING rows of `netstat -ano -a -p tcp` whose local address ends in :port.
func parseNetstatListeners(out string, port int) []int {
	var pids []int
	for _, line := range strings.Split(out, string(rune(10))) {
		f := strings.Fields(line)
		if len(f) < 5 || !strings.EqualFold(f[0], "TCP") || !strings.EqualFold(f[3], "LISTENING") || !strings.HasSuffix(f[1], ":"+strconv.Itoa(port)) {
			continue
		}
		if n, e := strconv.Atoi(f[4]); e == nil {
			pids = append(pids, n)
		}
	}
	return pids
}

func cimField(pid int, field string) (string, error) {
	out, err := run("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		fmt.Sprintf("$p = Get-CimInstance Win32_Process -Filter 'ProcessId=%d'; if (-not $p) { exit 3 }; [string]$p.%s", pid, field))
	return out, err
}

// commandLine is the command line of pid.
func commandLine(pid int) (string, error) { return cimField(pid, "CommandLine") }

// parentPID is the parent process id of pid.
func parentPID(pid int) (int, error) {
	out, err := cimField(pid, "ParentProcessId")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out)
}
