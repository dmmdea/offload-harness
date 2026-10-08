//go:build !windows

package gpugen

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// statField is field n (1-based, as proc(5) numbers them) of /proc/<pid>/stat; the comm field is cut
// at the LAST ")" because it may hold spaces and parentheses.
func statField(t *testing.T, pid, n int) int {
	t.Helper()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Skipf("no /proc here: %v", err)
	}
	rest := strings.Fields(string(b)[strings.LastIndex(string(b), ")")+2:]) // starts at field 3 (state)
	v, err := strconv.Atoi(rest[n-3])
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// SIL3 / TST5: the engine is spawned in the RUNNER's process group (not detached into its own), so
// gpugen's SIGKILL escalation of that group takes it along when the runner cannot answer a SIGTERM.
// A runner that is SIGSTOPped cannot: the cancel's SIGTERM stays pending, the group is SIGKILLed after
// the grace, and the engine (a hanging stub) must be dead with it. TestOwnProcessGroupEscalatesToSIGKILL
// puts its surviving grandchild inside the runner's group, a topology the shipped runner used not to
// have; this one runs the REAL runner.
func TestAWedgedRunnerSIGKILLedWithItsGroupTakesTheEngineWithIt(t *testing.T) {
	old := termGrace
	termGrace = 800 * time.Millisecond
	t.Cleanup(func() { termGrace = old })

	b := newIGPUBox(t)
	pidFile := filepath.Join(b.work, "engine.pid")
	b.spec.Main = stubEngine{Log: goodSDHeader, Hang: true, PidFile: pidFile}
	out := filepath.Join(b.work, "clip.mp4")
	args := b.videoArgs(out, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Generate(ctx, Spec{Exe: "node", Script: runnerScript("sdcpp-video.mjs"), Args: args, Env: b.env(), Out: out,
			Timeout: 2 * time.Minute, SkipFreeComfy: true, OwnProcessGroup: true})
		done <- err
	}()

	enginePid := readPid(t, pidFile)
	runnerPid := statField(t, enginePid, 4) // ppid
	if got := statField(t, enginePid, 5); got != runnerPid {
		t.Fatalf("the engine's process group is %d, want the RUNNER's %d: a detached engine survives the group kill", got, runnerPid)
	}
	if got := statField(t, runnerPid, 5); got != runnerPid {
		t.Fatalf("the runner must lead its own group (OwnProcessGroup), pgrp = %d, pid = %d", got, runnerPid)
	}

	if err := syscall.Kill(runnerPid, syscall.SIGSTOP); err != nil { // wedged: cannot handle the SIGTERM
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || ClassifyErr(err) != "timeout" {
			t.Errorf("a cancelled run must read as a timeout, got %v (class %q)", err, ClassifyErr(err))
		}
	case <-time.After(60 * time.Second):
		_ = syscall.Kill(-runnerPid, syscall.SIGKILL)
		t.Fatal("Generate did not return after the group SIGKILL")
	}
	if !pidGone(enginePid) {
		_ = syscall.Kill(enginePid, syscall.SIGKILL)
		t.Fatal("the engine survived the SIGKILL of the runner's process group and would keep the iGPU after the lease is released")
	}

	// the wedged runner could not clean up: its temp dir is still there, and the NEXT job's makeTempDir sweeps it
	left, _ := filepath.Glob(filepath.Join(b.priv, "sdcpp-video-*"))
	if len(left) != 1 {
		t.Fatalf("want the one leaked temp dir of the killed runner, got %v", left)
	}
	mod := "file://" + filepath.ToSlash(filepath.Join(filepath.Dir(runnerScript("sdcpp-video.mjs")), "igpu-engine.mjs"))
	cmd := exec.Command("node", "--input-type=module", "-e", `import {makeTempDir} from "`+mod+`"; makeTempDir("sdcpp-video-").cleanup();`)
	cmd.Env = append(os.Environ(), "TEMP="+b.priv, "TMP="+b.priv, "TMPDIR="+b.priv)
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the sweeping job failed: %v: %s", err, o)
	}
	if left, _ := filepath.Glob(filepath.Join(b.priv, "sdcpp-video-*")); len(left) != 0 {
		t.Errorf("the next job did not sweep the dead runner's temp dir: %v", left)
	}
}
