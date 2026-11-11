//go:build windows

package nodeswap

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestFinderSeesAProcessStartedThroughAJunction is the 2026-11-11 swap failure on a Windows node:
// llama-swap's live directory is a junction into its generation directory, the restarted process
// ran from the junction's path, and the finder, asked for the generation's path, saw nothing, so
// both the post-restart check and the rollback's check failed against a running node.
func TestFinderSeesAProcessStartedThroughAJunction(t *testing.T) {
	if os.Getenv("NODESWAP_JUNCTION_HELPER") == "1" {
		time.Sleep(60 * time.Second)
		return
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	gen := filepath.Join(root, "probe-versions", "v1")
	if err := os.MkdirAll(gen, 0o755); err != nil {
		t.Fatal(err)
	}
	genExe := filepath.Join(gen, "nodeswap-junction-probe.exe")
	copyExe(t, self, genExe)
	live := filepath.Join(root, "probe")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", live, gen).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v: %s", err, out)
	}
	cmd := exec.Command(filepath.Join(live, "nodeswap-junction-probe.exe"), "-test.run=^TestFinderSeesAProcessStartedThroughAJunction$")
	cmd.Env = append(os.Environ(), "NODESWAP_JUNCTION_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	for _, asked := range []string{genExe, filepath.Join(live, "nodeswap-junction-probe.exe")} {
		procs, err := findProcessesByExeWindows(asked)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, p := range procs {
			if p.PID == cmd.Process.Pid {
				found = true
			}
		}
		if !found {
			t.Errorf("asked for %s: pid %d (started through the junction) not found in %+v", asked, cmd.Process.Pid, procs)
		}
	}
}

func copyExe(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
