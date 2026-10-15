//go:build !windows

package research

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// processAlive: a process that has ended but is not yet reaped (state Z) is not running.
func processAlive(pid int) bool {
	if raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		s := string(raw)
		if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
			return s[i+2] != 'Z'
		}
	}
	return syscall.Kill(pid, 0) == nil
}

// treeWrapper writes a shell script that backgrounds a sleeper and waits for it, so the program the harness starts
// (the shell) has a child that holds the output pipe. The script records the sleeper's pid beside itself.
func treeWrapper(t *testing.T) (wrapper, pidFile string) {
	t.Helper()
	wrapper = filepath.Join(t.TempDir(), "hang.sh")
	pidFile = wrapper + ".pid"
	body := "#!/bin/sh\nsleep 60 &\necho $! > \"$0.pid.tmp\" && mv \"$0.pid.tmp\" \"$0.pid\"\nwait\n"
	if err := os.WriteFile(wrapper, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return wrapper, pidFile
}
