package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/nodeswap"
)

func buildTestOutcome() nodeswap.Outcome {
	return nodeswap.Outcome{OK: true, NewSHA256: "abc123"}
}

func TestParseNodeSwapFlags_Defaults(t *testing.T) {
	plan, out, err := parseNodeSwapFlags([]string{
		"--staged", "staged.exe", "--target", "target.exe", "--sha256", "ABCD",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Staged != "staged.exe" || plan.Target != "target.exe" || plan.ExpectedSHA256 != "ABCD" {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.WaitIdleTimeout != 10*time.Minute {
		t.Errorf("WaitIdleTimeout default = %v, want 10m", plan.WaitIdleTimeout)
	}
	if plan.VerifyTimeout != 90*time.Second {
		t.Errorf("VerifyTimeout default = %v, want 90s", plan.VerifyTimeout)
	}
	if plan.ProcessMatch != "fleet-serve" {
		t.Errorf("ProcessMatch default = %q, want fleet-serve", plan.ProcessMatch)
	}
	if plan.MCPMatch != " mcp" {
		t.Errorf("MCPMatch default = %q, want %q", plan.MCPMatch, " mcp")
	}
	if plan.RestartTaskName != "" || plan.RestartCommand != "" {
		t.Errorf("expected no restart mechanism by default (standalone), got task=%q command=%q", plan.RestartTaskName, plan.RestartCommand)
	}
	if out.resultPath != "" || out.logPath != "" || out.asJSON {
		t.Errorf("output flags should default empty/false, got %+v", out)
	}
}

func TestParseNodeSwapFlags_AllFlagsThread(t *testing.T) {
	plan, out, err := parseNodeSwapFlags([]string{
		"--staged", "s.exe", "--target", "t.exe", "--sha256", "DEAD",
		"--skip-hash-check",
		"--backup-suffix", "pre-x",
		"--health-url", "http://192.0.2.10:18811/fleet/health",
		"--wait-idle-timeout", "2m",
		"--restart-task", "offload-fleet-node",
		"--restart-timeout", "30s",
		"--verify-timeout", "45s",
		"--render-tarball", "render.tar.gz",
		"--render-dir", "D:/render",
		"--process-match", "fleet-serve",
		"--mcp-match", " mcp",
		"--dry-run",
		"--no-rollback",
		"--result", "out.json",
		"--log", "out.log",
		"--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.SkipHashCheck {
		t.Error("SkipHashCheck should be true")
	}
	if plan.BackupSuffix != "pre-x" {
		t.Errorf("BackupSuffix = %q", plan.BackupSuffix)
	}
	if plan.HealthURL != "http://192.0.2.10:18811/fleet/health" {
		t.Errorf("HealthURL = %q", plan.HealthURL)
	}
	if plan.WaitIdleTimeout != 2*time.Minute {
		t.Errorf("WaitIdleTimeout = %v", plan.WaitIdleTimeout)
	}
	if plan.RestartTaskName != "offload-fleet-node" {
		t.Errorf("RestartTaskName = %q", plan.RestartTaskName)
	}
	if plan.RenderTarball != "render.tar.gz" || plan.RenderDir != "D:/render" {
		t.Errorf("render fields = %q / %q", plan.RenderTarball, plan.RenderDir)
	}
	if !plan.DryRun || !plan.NoRollback {
		t.Errorf("DryRun/NoRollback not threaded: %+v", plan)
	}
	if out.resultPath != "out.json" || out.logPath != "out.log" || !out.asJSON {
		t.Errorf("output flags = %+v", out)
	}
}

func TestParseNodeSwapFlags_RestartTaskAndCommandBothSet(t *testing.T) {
	// parseNodeSwapFlags itself does not reject this (that is
	// nodeswap.validatePlan's job, exercised inside Run before anything is
	// touched) — this test just pins that both values are threaded through
	// unmodified so the later validation actually sees the conflict.
	plan, _, err := parseNodeSwapFlags([]string{
		"--staged", "s", "--target", "t", "--sha256", "h",
		"--restart-task", "a", "--restart-command", "b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.RestartTaskName != "a" || plan.RestartCommand != "b" {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestOpenSwapLog_AppendsTimestampedLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "swap.log")
	write, closeFn, err := openSwapLog(p)
	if err != nil {
		t.Fatal(err)
	}
	write("hello")
	write("world")
	closeFn()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	content := string(b)
	if !contains(content, "hello") || !contains(content, "world") {
		t.Fatalf("log content = %q, want both lines", content)
	}
	// A second open must APPEND, not truncate — a poller re-reading a
	// detached run's log must never lose earlier progress.
	write2, closeFn2, err := openSwapLog(p)
	if err != nil {
		t.Fatal(err)
	}
	write2("again")
	closeFn2()
	b2, _ := os.ReadFile(p)
	if !contains(string(b2), "hello") || !contains(string(b2), "again") {
		t.Fatalf("reopened log lost earlier content: %q", string(b2))
	}
}

func TestOpenSwapLog_EmptyPathIsANoop(t *testing.T) {
	write, closeFn, err := openSwapLog("")
	if err != nil {
		t.Fatal(err)
	}
	write("should not panic or write anywhere")
	closeFn()
}

func TestWriteSwapResult_AtomicViaRename(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "result.json")
	if err := writeSwapResult(p, buildTestOutcome()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file should not remain after a successful write")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(b), `"ok": true`) {
		t.Errorf("result.json = %s, want ok:true", b)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}
