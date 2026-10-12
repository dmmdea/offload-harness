package spawner

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

const sleeperEnv = "TRIPWIRE_FIXTURE_SLEEPER"

// TestSleeper is the child: without the variable it is an ordinary skipped test. With it, it sleeps for a bounded time,
// so a copy that nobody kills still ends by itself.
func TestSleeper(t *testing.T) {
	if os.Getenv(sleeperEnv) == "" {
		t.Skip("the child of TestLeavesAProcessRunningFromTheTempFolder")
	}
	time.Sleep(60 * time.Second)
}

// TestLeavesAProcessRunningFromTheTempFolder is the defect: it copies its own test binary into the OS temp dir, starts the
// copy and returns without waiting, so a process runs from an image inside the folder the tripwire watches. The tripwire
// reports it as PROCESSES, with the pid and the image path.
func TestLeavesAProcessRunningFromTheTempFolder(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := "tripwire-sleeper"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	image := filepath.Join(os.TempDir(), name)
	src, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(image, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(image, "-test.run=^TestSleeper$", "-test.timeout=90s")
	cmd.Env = append(os.Environ(), sleeperEnv+"=1")
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Not waited for, on purpose: the copy outlives the test.
}
