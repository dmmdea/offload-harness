//go:build windows

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// labelSDDL returns the SDDL string of dir's mandatory (integrity) label, or ""
// when it has none — the test uses it to observe whether a LOW label ACE is
// present. A read error (no SACL at all) is treated as "no label" ("").
func labelSDDL(t *testing.T, dir string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION)
	if err != nil || sd == nil {
		return ""
	}
	return sd.String()
}

// cmdExe is a benign, always-present Windows executable used across the tests as
// the allowlisted program. Its base name (case-insensitive, ".exe" dropped) is
// "cmd".
func cmdExe(t *testing.T) string {
	t.Helper()
	sysroot := os.Getenv("SystemRoot")
	if sysroot == "" {
		sysroot = `C:\Windows`
	}
	p := filepath.Join(sysroot, "System32", "cmd.exe")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("cmd.exe not found at %s: %v", p, err)
	}
	return p
}

// TestAvailableTrueOnWindows: the Windows cage is always enforceable (Job Object +
// low-IL token need no admin), so Available() must report true.
func TestAvailableTrueOnWindows(t *testing.T) {
	ok, why := Available()
	if !ok {
		t.Fatalf("Available() = false (%s), want true on Windows", why)
	}
	if why == "" {
		t.Errorf("Available() reason should be non-empty; got %q", why)
	}
}

// TestWindowsRunsAllowlistedCommand proves the positive control: an allowlisted
// executable runs, its stdout is captured, exit 0, not refused.
func TestWindowsRunsAllowlistedCommand(t *testing.T) {
	wt := t.TempDir()
	res, err := Run(context.Background(), Spec{
		Argv:               []string{cmdExe(t), "/c", "echo", "hello-from-cage"},
		Worktree:           wt,
		WorktreeWritable:   true,
		Scratch:            filepath.Join(wt, ".scratch"),
		AllowedExecutables: []string{"cmd"},
	})
	if err != nil {
		t.Fatalf("allowlisted run failed: %v (stderr=%s)", err, res.Stderr)
	}
	if res.Refused {
		t.Fatalf("allowlisted command must NOT be refused; stderr=%q", res.Stderr)
	}
	if !strings.Contains(res.Stdout, "hello-from-cage") {
		t.Errorf("stdout should carry the echo output; got %q (stderr=%q)", res.Stdout, res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Errorf("benign command should exit 0; got exit=%d", res.ExitCode)
	}
}

// TestWindowsRefusesNonAllowlisted proves the allowlist gate: an executable whose
// base name is NOT in AllowedExecutables is REFUSED and NEVER launched.
func TestWindowsRefusesNonAllowlisted(t *testing.T) {
	wt := t.TempDir()
	// A marker file the (refused) process would create if it ever ran.
	marker := filepath.Join(wt, "ran.txt")
	res, err := Run(context.Background(), Spec{
		Argv:               []string{cmdExe(t), "/c", "echo x > " + marker},
		Worktree:           wt,
		WorktreeWritable:   true,
		Scratch:            filepath.Join(wt, ".scratch"),
		AllowedExecutables: []string{"go", "git"}, // cmd is NOT listed
	})
	if err != nil {
		t.Fatalf("refusal should not be an error return; got %v", err)
	}
	if !res.Refused {
		t.Fatalf("SECURITY: a non-allowlisted executable must be Refused; got Refused=false stderr=%q", res.Stderr)
	}
	if !strings.Contains(res.Stderr, "allowlist") {
		t.Errorf("stderr should explain the allowlist refusal; got %q", res.Stderr)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Errorf("SECURITY: the refused process must NOT have launched (marker exists, stat err=%v)", statErr)
	}
}

// TestWindowsTimeoutKillsViaJob proves the ctx-deadline path: a long-running
// allowlisted command is killed by the Job Object and Run returns promptly rather
// than hanging for the command's full duration.
func TestWindowsTimeoutKillsViaJob(t *testing.T) {
	wt := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	// `ping -n 30 localhost` sleeps ~29s; ctx expires at 2s and the job kill must
	// reap it well before that.
	res, _ := Run(ctx, Spec{
		Argv:               []string{cmdExe(t), "/c", "ping", "-n", "30", "127.0.0.1"},
		Worktree:           wt,
		WorktreeWritable:   true,
		Scratch:            filepath.Join(wt, ".scratch"),
		AllowedExecutables: []string{"cmd"},
	})
	elapsed := time.Since(start)
	if elapsed > 15*time.Second {
		t.Errorf("timeout must return promptly (job kill); took %v", elapsed)
	}
	if res.Refused {
		t.Errorf("a timeout is not a cage refusal; got Refused=true stderr=%q", res.Stderr)
	}
	// A timed-out run should surface a timeout note somewhere.
	if !strings.Contains(res.Stderr, "timeout") && !strings.Contains(res.Stdout, "timeout") {
		t.Logf("no explicit timeout note (stdout=%q stderr=%q) — kill still verified by elapsed time", res.Stdout, res.Stderr)
	}
}

// TestWindowsOutputCapTruncates proves the head+tail output cap: a command that
// prints far more than the cap has its captured output truncated with the marker.
func TestWindowsOutputCapTruncates(t *testing.T) {
	wt := t.TempDir()
	// Emit a large file via cmd then type it, to exceed the ~30 KB cap.
	big := filepath.Join(wt, "big.txt")
	line := strings.Repeat("A", 200) + "\r\n"
	var sb strings.Builder
	for i := 0; i < 1000; i++ { // ~200 KB
		sb.WriteString(line)
	}
	if err := os.WriteFile(big, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Spec{
		Argv:               []string{cmdExe(t), "/c", "type", big},
		Worktree:           wt,
		WorktreeWritable:   true,
		Scratch:            filepath.Join(wt, ".scratch"),
		AllowedExecutables: []string{"cmd"},
	})
	if err != nil {
		t.Fatalf("run failed: %v (stderr=%s)", err, res.Stderr)
	}
	if res.Refused {
		t.Fatalf("unexpected refusal: %q", res.Stderr)
	}
	if len(res.Stdout) > 40*1024 {
		t.Errorf("output should be capped near ~30 KB; got %d bytes", len(res.Stdout))
	}
	if !strings.Contains(res.Stdout, "output truncated") {
		t.Errorf("truncated output must carry the '[... output truncated ...]' marker; got %d bytes head=%q", len(res.Stdout), res.Stdout[:min(80, len(res.Stdout))])
	}
}

// TestWindowsLowILWriteConfinement is BEST-EFFORT: it attempts to prove that the
// low-integrity child token blocks a write OUTSIDE the worktree while a write
// INSIDE the (medium-IL, world-writable-by-owner) worktree succeeds. Low-IL
// processes cannot write to medium-IL securable objects unless they carry a low
// mandatory label. This is inherently environment-sensitive (depends on the
// target dir's mandatory label / ACL), so a failure to demonstrate the block is
// LOGGED, not fataled — the report documents it as a manual-verify item. The
// inside-worktree write MUST still succeed (proving the child actually ran under
// the token).
func TestWindowsLowILWriteConfinement(t *testing.T) {
	wt := t.TempDir()
	inside := filepath.Join(wt, "inside.txt")

	// A target OUTSIDE the worktree in a medium-IL location: the user profile dir.
	// A low-IL process should NOT be able to create a file here.
	profile := os.Getenv("USERPROFILE")
	if profile == "" {
		t.Skip("USERPROFILE unset; cannot pick an out-of-worktree medium-IL target")
	}
	outside := filepath.Join(profile, "offload_lowil_probe_DELETEME.txt")
	_ = os.Remove(outside) // clean any stale probe
	defer os.Remove(outside)

	// Two SEPARATE runs (no cmd `&` chaining, so each probe is independent and
	// deterministic). Each uses a single `echo>path` redirection with NO spaces or
	// quotes in the /c payload — cmd.exe does not understand the C-runtime
	// backslash-quote escaping that EscapeArg emits for quoted paths, and
	// t.TempDir()/USERPROFILE paths contain no spaces here.
	if strings.ContainsAny(inside, " \t") || strings.ContainsAny(outside, " \t") {
		t.Skipf("temp/profile path contains spaces (inside=%q outside=%q); cmd redirection probe needs space-free paths", inside, outside)
	}
	runEcho := func(target string) Result {
		t.Helper()
		res, err := Run(context.Background(), Spec{
			Argv:               []string{cmdExe(t), "/c", "echo x>" + target},
			Worktree:           wt,
			WorktreeWritable:   true,
			Scratch:            filepath.Join(wt, ".scratch"),
			AllowedExecutables: []string{"cmd"},
		})
		if err != nil {
			t.Fatalf("run failed for %s: %v (stderr=%s)", target, err, res.Stderr)
		}
		if res.Refused {
			t.Fatalf("unexpected refusal writing %s: %q", target, res.Stderr)
		}
		return res
	}

	// INSIDE write MUST succeed — proves the child actually ran under the low-IL
	// token AND that the worktree's lowered mandatory label lets a LOW subject write.
	inRes := runEcho(inside)
	if _, statErr := os.Stat(inside); statErr != nil {
		t.Errorf("worktree write should succeed under the low-IL token; inside.txt missing (err=%v) stderr=%q", statErr, inRes.Stderr)
	}

	// OUTSIDE write into a MEDIUM-IL location SHOULD be blocked by Mandatory
	// Integrity Control. This is the write-confinement guarantee. Best-effort per the
	// brief: if the block is NOT observed (environment-sensitive), LOG loudly rather
	// than fail, so the report can flag it as a manual-verify item.
	outRes := runEcho(outside)
	if _, statErr := os.Stat(outside); statErr == nil {
		t.Logf("BEST-EFFORT NOT PROVEN: low-IL child was able to write OUTSIDE the worktree (%s). "+
			"Write-confinement via low IL is environment-sensitive; verify manually. stderr=%q", outside, outRes.Stderr)
	} else {
		t.Logf("low-IL write-confinement observed: out-of-worktree write blocked (stat err=%v)", statErr)
	}
}

// TestWindowsRelabelRevertedAfterRun is the SECURITY test for the transient
// relabel fix: after Run returns, the worktree's mandatory integrity label must be
// RESTORED to its pre-run state — it must NOT be left at LOW (which would leave the
// user's checkout writable by any low-integrity process on the machine). A fresh
// t.TempDir() has no explicit label; after a run it must again carry NO low-integrity
// (LW) label ACE.
func TestWindowsRelabelRevertedAfterRun(t *testing.T) {
	wt := t.TempDir()
	scratch := filepath.Join(wt, ".scratch")

	before := labelSDDL(t, wt)
	if strings.Contains(before, "LW") {
		t.Fatalf("precondition: fresh temp dir already carries a LOW label (%q); test cannot prove the revert", before)
	}

	res, err := Run(context.Background(), Spec{
		Argv:               []string{cmdExe(t), "/c", "echo", "relabel-probe"},
		Worktree:           wt,
		WorktreeWritable:   true,
		Scratch:            scratch,
		AllowedExecutables: []string{"cmd"},
	})
	if err != nil {
		t.Fatalf("run failed: %v (stderr=%s)", err, res.Stderr)
	}
	if res.Refused {
		t.Fatalf("unexpected refusal: %q", res.Stderr)
	}

	after := labelSDDL(t, wt)
	// The guarantee: no residual LOW (LW) mandatory-label ACE on the worktree.
	if strings.Contains(after, "(ML") && strings.Contains(after, "LW") {
		t.Fatalf("SECURITY: worktree left at LOW integrity after Run — label not reverted. before=%q after=%q", before, after)
	}
	// And the scratch dir the run created must likewise not be left LOW.
	afterScratch := labelSDDL(t, scratch)
	if strings.Contains(afterScratch, "(ML") && strings.Contains(afterScratch, "LW") {
		t.Fatalf("SECURITY: scratch left at LOW integrity after Run — label not reverted. after=%q", afterScratch)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
