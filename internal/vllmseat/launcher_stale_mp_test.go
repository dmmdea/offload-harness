//go:build linux

package vllmseat

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSeatLauncherRecoversFromACrashedGenerationsMPServer runs the behavioral
// test of seat_fg.sh's port-refusal blocks (0.143.0): after a crash nothing
// ran seat_stop.sh, the dead generation's LMCache MP server still held its
// HTTP port, and every restart refused (2026-09-29: the agent seat fully down
// for 23 minutes, 59 failed starts). The script now runs the stack's own
// cleanup once when no engine of its stack is alive — and still refuses a
// foreign holder, and never touches a live engine. The shell test extracts the
// real blocks from seat_fg.sh and drives all three cases on scratch ports.
func TestSeatLauncherRecoversFromACrashedGenerationsMPServer(t *testing.T) {
	script := filepath.Join("..", "..", "setup", "templates", "vllm-seat", "seat_fg.stale-mp.tests.sh")
	out, err := exec.Command("bash", script).CombinedOutput()
	s := string(out)
	if strings.Contains(s, "SKIP") {
		t.Skip(strings.TrimSpace(s))
	}
	if err != nil || !strings.Contains(s, "ALL PASS") {
		t.Fatalf("seat_fg.sh stale-MP behavior (err %v):\n%s", err, s)
	}
}
