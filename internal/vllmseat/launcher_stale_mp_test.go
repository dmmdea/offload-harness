//go:build linux

package vllmseat

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSeatLauncherRecoversFromACrashedGenerationsMPServer runs the behavioral
// test of the seat launcher's crash cleanup. After a crash nothing ran
// seat_stop.sh: the dead generation's LMCache MP server still held its HTTP
// port and every restart refused (2026-09-29: the agent seat fully down for 23
// minutes, 59 failed starts), and the engine workers its API server could not
// stop kept the cards. seat_fg.sh now runs the stack's own cleanup once when no
// engine of its stack is alive — whether the MP port is held or only the
// workers are left — seat_stop.sh reaps only what is provably the seat's own
// (workers with no live `vllm serve` ancestor, the MP server of this stack's MP
// port, SIGTERM then SIGKILL, a survivor named), and a foreign listener is still
// refused and never touched. The shell test extracts the real code from both
// scripts, drives every case against stand-in processes named like the real
// ones on scratch ports, and runs two mutants of the foreign-listener guard
// that it must catch.
func TestSeatLauncherRecoversFromACrashedGenerationsMPServer(t *testing.T) {
	script := filepath.Join("..", "..", "setup", "templates", "vllm-seat", "seat_fg.stale-mp.tests.sh")
	out, err := exec.Command("bash", script).CombinedOutput()
	s := string(out)
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
		"PASS a foreign holder is still refused",
		"PASS a live engine is never cleaned up",
		"PASS orphaned engine workers are reaped with the MP port free and the start proceeds",
		"PASS the MP server and the orphaned workers are reaped together",
		"PASS a live engine's workers on another port are never reaped",
		"PASS orphans are reaped but a foreign MP HTTP holder is still refused and left alone",
		"PASS another stack's MP server holding the MP HTTP port is foreign",
		"PASS an MP server that ignores SIGTERM is killed after the grace",
		"PASS an MP server that obeys SIGTERM late is waited out, not killed",
		"PASS a reaped worker that survives SIGKILL is named",
		"PASS the stop path stops this seat's engine tree and only that",
		"PASS mutation: reaping by port instead of by identity is caught",
		"PASS mutation: dropping the final MP HTTP port refusal is caught",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the shell test no longer reports %q:\n%s", want, s)
		}
	}
}
