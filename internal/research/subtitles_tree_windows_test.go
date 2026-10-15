//go:build windows

package research

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

const stillActive = 259 // STILL_ACTIVE, the exit code Windows reports for a process that has not ended

func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// treeWrapper writes the documented Windows shape: a .cmd file, so cmd.exe is the program the harness starts, and
// the program it runs (a copy of the test binary asleep for a minute) is the grandchild. The grandchild inherits the
// per-call directory as its working directory, which is what keeps Windows from deleting that directory.
func treeWrapper(t *testing.T) (wrapper, pidFile string) {
	t.Helper()
	dir := t.TempDir()
	wrapper = filepath.Join(dir, "hang.cmd")
	pidFile = filepath.Join(dir, "grandchild.pid")
	body := "@echo off\r\n\"" + fakeBinary(t) + "\" " + sleeperFlag + " 60 \"%~dp0grandchild.pid\"\r\n"
	if err := os.WriteFile(wrapper, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return wrapper, pidFile
}
