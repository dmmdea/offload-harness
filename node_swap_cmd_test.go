package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
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

func TestScanEarlyOutputFlags(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantResult string
		wantLog    string
	}{
		{"space form", []string{"--staged", "s", "--result", "r.json", "--log", "l.log"}, "r.json", "l.log"},
		{"single-dash space form", []string{"-result", "r2.json", "-log", "l2.log"}, "r2.json", "l2.log"},
		{"equals form", []string{"--result=r3.json", "--log=l3.log"}, "r3.json", "l3.log"},
		{"single-dash equals form", []string{"-result=r4.json", "-log=l4.log"}, "r4.json", "l4.log"},
		{"missing", []string{"--staged", "s"}, "", ""},
		{"dangling flag with no value", []string{"--result"}, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotResult, gotLog := scanEarlyOutputFlags(c.args)
			if gotResult != c.wantResult || gotLog != c.wantLog {
				t.Errorf("scanEarlyOutputFlags(%v) = (%q, %q), want (%q, %q)", c.args, gotResult, gotLog, c.wantResult, c.wantLog)
			}
		})
	}
}

// TestRunNodeSwap_BadFlagStillWritesAResult pins the fix for the SHOULD-FIX
// review finding on PR #476: a flag-parsing failure must not exit silently
// with neither --log nor --result written, because this command is launched
// DETACHED (no attached console) and a poller cannot tell "crashed before
// writing anything" apart from "still running".
func TestRunNodeSwap_BadFlagStillWritesAResult(t *testing.T) {
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "result.json")
	logPath := filepath.Join(dir, "swap.log")

	err := runNodeSwap([]string{
		"--staged", "s", "--target", "t", "--sha256", "h",
		"--this-flag-does-not-exist", "boom",
		"--result", resultPath, "--log", logPath,
	})
	if err == nil {
		t.Fatal("expected an error from an unrecognized flag")
	}
	b, rerr := os.ReadFile(resultPath)
	if rerr != nil {
		t.Fatalf("expected --result to be written despite the parse failure: %v", rerr)
	}
	if !contains(string(b), `"ok": false`) {
		t.Errorf("result.json = %s, want ok:false", b)
	}
	lb, lerr := os.ReadFile(logPath)
	if lerr != nil {
		t.Fatalf("expected --log to be written despite the parse failure: %v", lerr)
	}
	if !contains(string(lb), "startup") {
		t.Errorf("log = %s, want a startup failure line", lb)
	}
}

// TestRunNodeSwap_UnopenableLogStillWritesAResult covers the second early
// failure point: parsing succeeds but --log cannot be opened (a bad
// directory). --result must still be written.
func TestRunNodeSwap_UnopenableLogStillWritesAResult(t *testing.T) {
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "result.json")
	badLogPath := filepath.Join(dir, "no-such-subdir", "swap.log") // parent dir never created

	err := runNodeSwap([]string{
		"--staged", filepath.Join(dir, "staged.exe"), "--target", filepath.Join(dir, "target.exe"),
		"--sha256", "h", "--skip-hash-check",
		"--result", resultPath, "--log", badLogPath,
	})
	if err == nil {
		t.Fatal("expected an error: --log's parent directory does not exist")
	}
	b, rerr := os.ReadFile(resultPath)
	if rerr != nil {
		t.Fatalf("expected --result to be written despite the log-open failure: %v", rerr)
	}
	if !contains(string(b), `"ok": false`) {
		t.Errorf("result.json = %s, want ok:false", b)
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

func TestLoopbackOrWildcardHost(t *testing.T) {
	cases := []struct {
		hostport string
		want     bool
	}{
		{"127.0.0.1:18811", true},
		{"localhost:18811", true},
		{"LOCALHOST:18811", true},
		{"0.0.0.0:18811", true},
		{"[::]:18811", true},
		{"[::1]:18811", true},
		{"", true},
		// A genuine tailnet bind (the deploy-d5207011 incident's shape — a
		// fleet node bound to its tailnet address, not loopback) must NOT be
		// classified as loopback/wildcard. 192.0.2.0/24 (RFC 5737 TEST-NET-1) is
		// this repo's own established placeholder for "a real example address"
		// (already used above and in setup/windows-node-swap-launch.ps1).
		{"192.0.2.10:18811", false},
		{"fleet-node-b.tailnnnnnn.ts.net:18811", false},
		{"192.168.1.50:18811", false},
	}
	for _, c := range cases {
		if got := loopbackOrWildcardHost(c.hostport); got != c.want {
			t.Errorf("loopbackOrWildcardHost(%q) = %v, want %v", c.hostport, got, c.want)
		}
	}
}

// TestResolveNodeSwapDefaults_AutoFillsFromConfig: gap 6a (d5207011 deploy
// record — an ad-hoc deploy script hardcoded 127.0.0.1 while fleet-serve
// bound only its tailnet address, and an unguarded fallback read every
// failed poll as "not idle" for ~46 minutes). --health-url is now
// auto-resolved from THIS node's own config fleet_listen, never guessed.
func TestResolveNodeSwapDefaults_AutoFillsFromConfig(t *testing.T) {
	var logged []string
	log := func(s string) { logged = append(logged, s) }

	plan := resolveNodeSwapDefaults(nodeswap.Plan{}, config.Config{
		FleetListen: "192.0.2.10:18811", GPULockPath: "/lease/lock", StateDir: "/lease/state",
	}, log)
	if plan.HealthURL != "http://192.0.2.10:18811/fleet/health" {
		t.Errorf("HealthURL = %q, want the tailnet address, never loopback", plan.HealthURL)
	}
	if plan.GPULockPath != "/lease/lock" || plan.GPUStateDir != "/lease/state" {
		t.Errorf("GPU lease paths not carried from config: %+v", plan)
	}
	if len(logged) == 0 {
		t.Error("expected the auto-resolve to log what it did")
	}
}

func TestResolveNodeSwapDefaults_ExplicitHealthURLAlwaysWins(t *testing.T) {
	plan := resolveNodeSwapDefaults(nodeswap.Plan{HealthURL: "http://explicit/fleet/health"}, config.Config{
		FleetListen: "192.0.2.10:18811",
	}, nil)
	if plan.HealthURL != "http://explicit/fleet/health" {
		t.Errorf("HealthURL = %q, an explicit flag must never be overridden", plan.HealthURL)
	}
}

func TestResolveNodeSwapDefaults_LoopbackConfigNeverAutoResolves(t *testing.T) {
	// A config that never told this tool its real bind address (still the
	// built-in default, or genuinely loopback) must leave HealthURL empty —
	// never fabricate a URL that will silently fail every poll, which is
	// the exact failure class this fix exists to prevent.
	for _, fl := range []string{"", "127.0.0.1:18811", "0.0.0.0:18811", "localhost:18811"} {
		plan := resolveNodeSwapDefaults(nodeswap.Plan{}, config.Config{FleetListen: fl}, nil)
		if plan.HealthURL != "" {
			t.Errorf("fleet_listen=%q auto-resolved to %q, want left empty (standalone/no-op, not a guess)", fl, plan.HealthURL)
		}
	}
}

func TestScanConfigFlag(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--staged", "s", "--config", "c.json"}, "c.json"},
		{[]string{"--config=c.json", "--staged", "s"}, "c.json"},
		{[]string{"-config", "c.json"}, "c.json"},
		{[]string{"-config=c.json"}, "c.json"},
		{[]string{"--staged", "s"}, ""},
		{[]string{"--config"}, ""}, // dangling flag, no value: tolerant, no panic
	}
	for _, c := range cases {
		if got := scanConfigFlag(c.args); got != c.want {
			t.Errorf("scanConfigFlag(%v) = %q, want %q", c.args, got, c.want)
		}
	}
}

// TestRunNodeSwap_ConfigAutoResolveIsHermeticWithAnExplicitConfigFlag: the
// new config.LoadWithSource call inside runNodeSwap must never reach outside
// an EXPLICIT --config path (this repo's own test machine has a real
// ~/.local-offload/config.json AND a real machine-wide GPU lease directory —
// if runNodeSwap ever fell back to either one here, this test would become
// non-hermetic, and possibly flaky/slow, on exactly that machine). The
// --config here points at a file this test writes itself, pinning both
// fleet_listen (loopback — --health-url must stay unset) and the GPU-lease
// paths to a throwaway temp dir (guaranteed unheld, so the standalone
// GPU-lease wait this same PR adds resolves on its very first poll).
func TestRunNodeSwap_ConfigAutoResolveIsHermeticWithAnExplicitConfigFlag(t *testing.T) {
	dir := t.TempDir()
	staged := filepath.Join(dir, "staged.exe")
	target := filepath.Join(dir, "target.exe")
	if err := os.WriteFile(staged, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	leaseDir := filepath.Join(dir, "lease-state")
	if err := os.MkdirAll(leaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	cfgJSON := `{"fleet_listen": "127.0.0.1:18811", "state_dir": ` + `"` + filepath.ToSlash(leaseDir) + `"}`
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(dir, "result.json")
	err := runNodeSwap([]string{
		"--staged", staged, "--target", target, "--sha256", "h", "--skip-hash-check",
		"--config", cfgPath, // explicit, sandboxed -> never the real machine's config or lease dir
		"--result", resultPath,
	})
	if err != nil {
		t.Fatalf("standalone swap should succeed: %v", err)
	}
	b, rerr := os.ReadFile(resultPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !contains(string(b), `"ok": true`) {
		t.Errorf("result.json = %s, want ok:true", b)
	}
	got, gerr := os.ReadFile(target)
	if gerr != nil || string(got) != "new" {
		t.Errorf("target.exe = %q (err %v), want the staged content swapped in", got, gerr)
	}
}
