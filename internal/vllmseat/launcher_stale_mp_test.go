//go:build linux

package vllmseat

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSeatLauncherRecoversFromACrashedGenerationsMPServer runs the behavioral
// test of the seat launcher's crash cleanup. After a crash nothing ran
// seat_stop.sh: the dead generation's LMCache MP server still held its HTTP
// port and every restart refused (2026-09-29: the agent seat fully down for 23
// minutes, 59 failed starts), and the engine workers its API server could not
// stop kept the cards. seat_fg.sh now runs the stack's own cleanup once when no
// engine of its stack is alive — whether the MP port is held or only the
// workers are left — seat_stop.sh reaps by rules on the process tree and on the
// stack's own names (workers with no live vLLM API server above them, the MP
// server of this stack's MP port, SIGTERM then SIGKILL, a survivor named), and a
// foreign listener is still refused and never touched. The shell test extracts
// the real code from both scripts, drives every case against stand-in processes
// named like the real ones on scratch ports, and runs two mutants of the
// foreign-listener guard that it must catch.
//
// The shell test sweeps the whole box and uses fixed scratch ports, so it takes
// a lock and a second run steps aside with a SKIP. This test starts the suite,
// starts a second one while the first is running, and requires that second run
// to step aside; it is what makes running the package's tests twice at once
// (two agents, two CI jobs on one machine) a skip, not two red runs.
func TestSeatLauncherRecoversFromACrashedGenerationsMPServer(t *testing.T) {
	script := filepath.Join("..", "..", "setup", "templates", "vllm-seat", "seat_fg.stale-mp.tests.sh")
	var out bytes.Buffer
	first := exec.Command("bash", script)
	first.Stdout, first.Stderr = &out, &out
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- first.Wait() }()
	var err error
	finished := false
	select {
	case err = <-done:
		finished = true // it ended within the wait (a SKIP, say): nothing to overlap
	case <-time.After(3 * time.Second):
		probe, perr := exec.Command("bash", script).CombinedOutput()
		if perr != nil || !strings.Contains(string(probe), "SKIP (another run of this test holds") {
			t.Errorf("a second run that overlapped the first did not step aside (err %v):\n%s", perr, probe)
		}
	}
	if !finished {
		err = <-done
	}
	s := out.String()
	if strings.Contains(s, "SKIP") {
		t.Skip(strings.TrimSpace(s))
	}
	if err != nil || !strings.Contains(s, "ALL PASS") {
		t.Fatalf("seat launcher crash-cleanup behavior (err %v):\n%s", err, s)
	}
	// "ALL PASS" says nothing about a case that was deleted from the script. Name
	// every case the cleanup's contract rests on, so dropping one turns this red.
	for _, want := range []string{
		"PASS stale MP server is cleaned up and the start proceeds",
		"PASS a slow release is waited out and the cleanup's exit code is printed",
		"PASS SEAT_MP_PORT_WAIT_SEC is a decimal number of seconds: 08 is waited out, abc means 10 and is said",
		"PASS a foreign holder is still refused",
		"PASS a live engine is never cleaned up",
		"PASS orphaned engine workers are reaped with the MP port free and the start proceeds",
		"PASS the MP server and the orphaned workers are reaped together",
		"PASS a live engine's workers on another port are never reaped",
		"PASS a live sibling's three-level engine tree is never a leftover",
		"PASS the stop path stops this seat's three-level tree and only that",
		"PASS an orphan under a live wrapper that only mentions vllm is still reaped",
		"PASS an MP server without its HTTP socket is a leftover and is reaped",
		"PASS orphans are reaped but a foreign MP HTTP holder is still refused and left alone",
		"PASS another stack's MP server holding the MP HTTP port is foreign",
		"PASS an MP server that ignores SIGTERM is killed after the grace",
		"PASS an MP server that obeys SIGTERM late is waited out, not killed",
		"PASS a reaped worker that survives SIGKILL is named",
		"PASS a stop that leaves a process of the seat alive fails, a clean one exits 0",
		"PASS a reaped worker left as a zombie by a parent that never waits is not stuck",
		"PASS an MP server that survives SIGKILL is named and fails the stop",
		"PASS a worker that dies 1.2 s after SIGKILL is waited out, not reported stuck",
		"PASS an MP server that dies 1.2 s after SIGKILL is waited out, not reported stuck",
		"PASS SEAT_REAP_WAIT_SEC is a decimal number of seconds: 08, 09 and 0 are honoured, abc means 10 and is said, an empty value means 10",
		"PASS the stop path stops this seat's engine tree and only that",
		"PASS the shared-memory sweep leaves a live sibling's segments alone",
		"PASS the shared-memory sweep leaves the segments of a sibling started as vllm.entrypoints alone",
		"PASS with no engine alive the sweep removes the stack's segments and only those",
		"PASS a named env file that is not there is refused and nothing is reaped",
		"PASS a live sibling started as vllm.entrypoints is never a leftover",
		"PASS the stop path leaves a sibling started as vllm.entrypoints and its workers alone",
		"PASS the stop's output reaches the seat log when nothing else carries it there",
		"PASS attached to the launcher, the stop does not write the seat log a second time",
		"PASS the launcher runs the stop attached, so the seat log is written once",
		"PASS the stop leaves an MP server whose port merely starts with this stack's alone",
		"PASS the launcher does not take an MP server whose port merely starts with its own for a leftover",
		"PASS a process whose name merely contains VLLM:: is no engine process at the start",
		"PASS a process whose name merely contains VLLM:: is left alone by the stop",
		"PASS the stop asks systemd to stop this stack's unit, and a stopped unit is no warning",
		"PASS a failed stop of a live unit is a warning, a unit that is already gone is not",
		"PASS mutation: reaping by port instead of by identity is caught",
		"PASS mutation: dropping the final MP HTTP port refusal is caught",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the shell test no longer reports %q:\n%s", want, s)
		}
	}
}
