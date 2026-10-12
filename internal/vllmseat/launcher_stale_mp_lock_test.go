//go:build linux

package vllmseat

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const staleMPLockName = "seat_fg.stale-mp.tests.lock.d"

// holdLock takes the flock(1) lock on dir for the rest of the test and returns once the lock is really held.
func holdLock(t *testing.T, flock, dir string) {
	t.Helper()
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	holder := exec.Command(flock, "-o", dir, "sleep", "600") // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command — flock from LookPath, a fixed argument list over a folder the test made
	holder.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // the sleep under flock goes with it
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-holder.Process.Pid, syscall.SIGKILL)
		_ = holder.Wait()
	})
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if exec.Command(flock, "-n", "-E", "99", dir, "true").Run() != nil { // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command — same fixed probe
			return
		}
	}
	t.Fatalf("the lock on %s was never taken", dir)
}

// TestSeatShellTestLocksAtOneBoxWidePathWhateverTMPDIRIs pins where the seat shell test takes the lock that makes a second run on
// the same box step aside. The test sweeps the whole box and uses fixed scratch ports, so the lock is only worth anything when
// every run finds the same folder: a lock keyed to TMPDIR is a different lock for each run that has its own temp dir (a gate run
// under a scratch dir beside another agent's gate, or a run under the temp-folder tripwire), and both runs then go ahead on the
// same ports. SEAT_FG_TEST_LOCK_ROOT names the folder (default /tmp); TMPDIR must play no part.
//
// The lock is held at both candidate paths before the script starts, so wherever it looks it finds a held lock and steps aside
// at once instead of running the sweep; the SKIP line names the folder it looked at.
func TestSeatShellTestLocksAtOneBoxWidePathWhateverTMPDIRIs(t *testing.T) {
	flock, err := exec.LookPath("flock")
	if err != nil {
		t.Skip("needs flock(1), which the shell test needs as well")
	}
	script := filepath.Join("..", "..", "setup", "templates", "vllm-seat", "seat_fg.stale-mp.tests.sh")
	lockRoot, someTmp := t.TempDir(), t.TempDir()
	holdLock(t, flock, filepath.Join(lockRoot, staleMPLockName))
	holdLock(t, flock, filepath.Join(someTmp, staleMPLockName))

	cmd := exec.Command("bash", script) // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command — a repository script, a fixed argument list
	cmd.Env = append(os.Environ(), "SEAT_FG_TEST_LOCK_ROOT="+lockRoot, "TMPDIR="+someTmp, "SEAT_FG_TEST_LOCKED=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the script was meant to step aside with a SKIP and exit 0 (err %v):\n%s", err, out)
	}
	want := "SKIP (another run of this test holds " + filepath.Join(lockRoot, staleMPLockName)
	if !strings.Contains(string(out), want) {
		t.Errorf("the lock was not taken at SEAT_FG_TEST_LOCK_ROOT, want %q in:\n%s", want, out)
	}
}
