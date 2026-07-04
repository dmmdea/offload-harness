//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TestMain dispatches the two re-exec roles before running any test:
//   - WORKER (workerEnv set): apply the cage and exec Argv — never returns.
//   - PROBE (argv[1]=="__probe__"): we are now INSIDE the cage; run the named
//     escape attempt, print a result line, and exit.
//
// Only a normal invocation (neither role) runs the test suite.
func TestMain(m *testing.M) {
	RunWorkerFromEnv() // no-op unless this process is the worker
	if len(os.Args) >= 3 && os.Args[1] == "__probe__" {
		os.Exit(runProbe(os.Args[2], os.Args[3:]))
	}
	os.Exit(m.Run())
}

// runProbe performs one escape attempt from INSIDE the cage and prints a result
// line the parent test asserts on.
func runProbe(name string, args []string) int {
	switch name {
	case "read":
		b, err := os.ReadFile(args[0])
		if err != nil {
			fmt.Printf("read DENIED: %v\n", err)
		} else {
			fmt.Printf("read OK: %q\n", string(b))
		}
	case "write":
		if err := os.WriteFile(args[0], []byte("pwned"), 0o644); err != nil {
			fmt.Printf("write DENIED: %v\n", err)
		} else {
			fmt.Printf("write OK\n")
		}
	case "dial":
		c, err := net.DialTimeout("tcp", args[0], 2*time.Second)
		if err != nil {
			fmt.Printf("dial BLOCKED: %v\n", err)
		} else {
			_ = c.Close()
			fmt.Printf("dial OK\n")
		}
	case "dial-udp":
		// Landlock V4 net rules are TCP-only, so a blocked UDP send is attributable
		// SOLELY to the empty network namespace (no route → ENETUNREACH). The UDP
		// "connect" plus the first write triggers the route lookup.
		c, err := net.Dial("udp", args[0])
		if err != nil {
			fmt.Printf("udp BLOCKED: %v\n", err)
			break
		}
		_, werr := c.Write([]byte("x"))
		_ = c.Close()
		if werr != nil {
			fmt.Printf("udp BLOCKED: %v\n", werr)
		} else {
			fmt.Printf("udp OK\n")
		}
	case "ptrace":
		// PTRACE_TRACEME = 0. Under the seccomp KILL denylist this never returns:
		// the process dies by SIGSYS, so the parent sees a killed exit and NO
		// "ptrace NOT-BLOCKED" line. Reaching the print means the cage failed.
		_, _, errno := syscall.Syscall(syscall.SYS_PTRACE, 0, 0, 0)
		fmt.Printf("ptrace NOT-BLOCKED: errno=%d\n", int(errno))
	case "openhandle":
		// open_by_handle_at is on the denylist → seccomp KILLs (SIGSYS) before the
		// syscall runs; reaching the print means it was NOT blocked.
		_, _, errno := syscall.Syscall6(unix.SYS_OPEN_BY_HANDLE_AT, 0, 0, 0, 0, 0, 0)
		fmt.Printf("openhandle NOT-BLOCKED: errno=%d\n", int(errno))
	case "i386":
		// Execute `int 0x80` (the i386 syscall ABI) from this x86_64 process via a
		// tiny JIT page. The arch-guard must SIGSYS-kill it; if fn() returns, the
		// guard is missing and the x86_64 denylist is i386-bypassable.
		code := []byte{0xB8, 0x14, 0x00, 0x00, 0x00, 0xCD, 0x80, 0xC3} // mov eax,20(getpid); int 0x80; ret
		m, err := unix.Mmap(-1, 0, len(code), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
		if err != nil {
			fmt.Printf("i386 SETUP-FAIL: mmap %v\n", err)
			break
		}
		copy(m, code)
		if err := unix.Mprotect(m, unix.PROT_READ|unix.PROT_EXEC); err != nil {
			fmt.Printf("i386 SETUP-FAIL: mprotect %v\n", err)
			break
		}
		fp := &struct{ addr uintptr }{addr: uintptr(unsafe.Pointer(&m[0]))}
		fn := *(*func())(unsafe.Pointer(&fp))
		fn()
		fmt.Printf("i386 NOT-BLOCKED\n")
	default:
		fmt.Printf("unknown probe %q\n", name)
		return 2
	}
	return 0
}

func selfExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

// requireCage skips the suite unless the OS cage can actually be enforced here
// (so it no-ops on a host without Landlock+seccomp+userns rather than failing).
func requireCage(t *testing.T) {
	t.Helper()
	if ok, why := Available(); !ok {
		t.Skipf("OS cage unavailable here: %s", why)
	}
}

// probe runs one in-cage escape attempt and returns its captured result. It does
// NOT fail on a non-zero exit (a killed ptrace probe and the fail-closed floor
// test both exit non-zero by design); tests assert on Stdout/ExitCode.
func probe(t *testing.T, worktree, scratch, name string, args ...string) Result {
	t.Helper()
	argv := append([]string{selfExe(t), "__probe__", name}, args...)
	// Grant read+exec of the (static) test binary itself so the probe can re-exec
	// inside the cage — WITHOUT opening its /tmp parent (which holds the secret).
	res, _ := Run(context.Background(), Spec{Argv: argv, Worktree: worktree, Scratch: scratch, ReadFiles: []string{selfExe(t)}, ABIFloor: 1})
	t.Logf("probe %q -> exit=%d stdout=%q stderr=%q", name, res.ExitCode, strings.TrimSpace(res.Stdout), strings.TrimSpace(res.Stderr))
	return res
}

// TestCageRunsBenignCommand proves the cage RUNS a real command (not just breaks
// everything) — a positive control.
func TestCageRunsBenignCommand(t *testing.T) {
	requireCage(t)
	wt := t.TempDir()
	res, err := Run(context.Background(), Spec{
		Argv:     []string{"/bin/echo", "hello-from-cage"},
		Worktree: wt,
		Scratch:  filepath.Join(wt, ".scratch"),
		ABIFloor: 1,
	})
	if err != nil {
		t.Fatalf("benign run failed: %v (stderr=%s)", err, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "hello-from-cage") {
		t.Errorf("stdout=%q, want the echo output", res.Stdout)
	}
}

// TestCageConfinesFilesystem proves Landlock: read inside the worktree OK, write
// to scratch OK, but read OUTSIDE and write to the RO worktree are DENIED.
func TestCageConfinesFilesystem(t *testing.T) {
	requireCage(t)
	wt := t.TempDir()
	scratch := filepath.Join(wt, ".scratch")
	if err := os.WriteFile(filepath.Join(wt, "inside.txt"), []byte("WORKTREE-DATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a secret OUTSIDE the worktree+scratch, readable by the user via unix perms.
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	if out := probe(t, wt, scratch, "read", filepath.Join(wt, "inside.txt")).Stdout; !strings.Contains(out, "WORKTREE-DATA") {
		t.Errorf("worktree read should succeed; got %q", out)
	}
	if out := probe(t, wt, scratch, "write", filepath.Join(scratch, "out.txt")).Stdout; !strings.Contains(out, "write OK") {
		t.Errorf("scratch write should succeed; got %q", out)
	}
	if out := probe(t, wt, scratch, "read", secret).Stdout; strings.Contains(out, "TOPSECRET") || !strings.Contains(out, "DENIED") {
		t.Errorf("SECURITY: reading a secret OUTSIDE the worktree must be DENIED; got %q", out)
	}
	if out := probe(t, wt, scratch, "write", filepath.Join(wt, "evil.txt")).Stdout; !strings.Contains(out, "DENIED") {
		t.Errorf("SECURITY: writing into the RO worktree must be DENIED; got %q", out)
	}
}

// TestCageSeversEgress proves egress is severed by TWO independent layers. TCP is
// blocked by BOTH Landlock V4's connect-deny (which returns EACCES — the error
// observed here) AND the empty network namespace. UDP, which Landlock V4 does NOT
// cover (its net rules are TCP-only), is blocked SOLELY by the netns (ENETUNREACH)
// — isolating and proving the network-namespace layer, and that DNS/UDP exfil is
// severed too.
func TestCageSeversEgress(t *testing.T) {
	requireCage(t)
	wt := t.TempDir()
	scratch := filepath.Join(wt, ".scratch")
	for _, addr := range []string{"127.0.0.1:11436", "127.0.0.1:6333", "127.0.0.1:18791", "1.1.1.1:443"} {
		out := probe(t, wt, scratch, "dial", addr).Stdout
		if !strings.Contains(out, "BLOCKED") || strings.Contains(out, "dial OK") {
			t.Errorf("SECURITY: TCP egress to %s must be blocked (Landlock V4 + netns); got %q", addr, out)
		}
	}
	// The UDP block is attributable to the netns ALONE (Landlock V4 is TCP-only).
	out := probe(t, wt, scratch, "dial-udp", "1.1.1.1:53").Stdout
	if !strings.Contains(out, "udp BLOCKED") || strings.Contains(out, "udp OK") {
		t.Errorf("SECURITY: UDP egress must be blocked by the netns; got %q", out)
	}
}

// TestCageKillsOnDeniedSyscall proves the seccomp denylist: ptrace kills the
// caged process (SIGSYS) — non-zero exit and no NOT-BLOCKED line.
func TestCageKillsOnDeniedSyscall(t *testing.T) {
	requireCage(t)
	wt := t.TempDir()
	res := probe(t, wt, filepath.Join(wt, ".scratch"), "ptrace")
	if strings.Contains(res.Stdout, "NOT-BLOCKED") {
		t.Errorf("SECURITY: ptrace was NOT blocked by seccomp; got %q", res.Stdout)
	}
	if res.ExitCode == 0 {
		t.Errorf("a denied syscall must kill the process (non-zero exit); got exit=0 stdout=%q", res.Stdout)
	}
	if res.Signal != int(unix.SIGSYS) {
		t.Errorf("a denied syscall must be killed by SIGSYS (seccomp KILL); got signal=%d exit=%d", res.Signal, res.ExitCode)
	}
}

// TestCageBlocksOpenByHandleAt proves the expanded denylist covers the Landlock
// path-resolution bypass: open_by_handle_at is killed by seccomp (SIGSYS).
func TestCageBlocksOpenByHandleAt(t *testing.T) {
	requireCage(t)
	wt := t.TempDir()
	res := probe(t, wt, filepath.Join(wt, ".scratch"), "openhandle")
	if strings.Contains(res.Stdout, "NOT-BLOCKED") {
		t.Errorf("SECURITY: open_by_handle_at was NOT blocked (Landlock path-bypass); %q", res.Stdout)
	}
	if res.Signal != int(unix.SIGSYS) {
		t.Errorf("open_by_handle_at must be killed by SIGSYS (seccomp); got signal=%d exit=%d", res.Signal, res.ExitCode)
	}
}

// TestCageBlocksI386ABI proves the arch-guard: a syscall issued via the i386
// (int 0x80) compat ABI is SIGSYS-killed, so the x86_64 denylist cannot be
// bypassed by switching ABIs. SETUP-FAIL (mmap/mprotect blocked) skips rather
// than false-passes; checking the SIGSYS signal rejects a JIT segfault.
func TestCageBlocksI386ABI(t *testing.T) {
	requireCage(t)
	wt := t.TempDir()
	res := probe(t, wt, filepath.Join(wt, ".scratch"), "i386")
	if strings.Contains(res.Stdout, "SETUP-FAIL") {
		t.Skipf("i386 proof setup unavailable (mmap/mprotect): %q", res.Stdout)
	}
	if strings.Contains(res.Stdout, "NOT-BLOCKED") {
		t.Errorf("SECURITY: i386 (int 0x80) ABI was NOT blocked — arch-guard missing; %q", res.Stdout)
	}
	if res.Signal != int(unix.SIGSYS) {
		t.Errorf("i386 syscall must be killed by SIGSYS (arch-guard), not a segfault; got signal=%d exit=%d stdout=%q", res.Signal, res.ExitCode, res.Stdout)
	}
}

// TestCageFailsClosedBelowABIFloor proves the fail-closed gate: if the achieved
// Landlock ABI can't meet the floor, the worker REFUSES to run the command.
func TestCageFailsClosedBelowABIFloor(t *testing.T) {
	requireCage(t)
	wt := t.TempDir()
	res, err := Run(context.Background(), Spec{
		Argv:     []string{"/bin/echo", "SHOULD-NOT-RUN"},
		Worktree: wt,
		Scratch:  filepath.Join(wt, ".scratch"),
		ABIFloor: 99, // impossibly high → floor not met
	})
	if err == nil {
		t.Errorf("expected the worker to refuse (floor not met), got nil error")
	}
	if strings.Contains(res.Stdout, "SHOULD-NOT-RUN") {
		t.Errorf("SECURITY: command ran despite the ABI floor not being met: %q", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "floor") {
		t.Errorf("stderr should explain the floor failure; got %q", res.Stderr)
	}
	if !res.Refused {
		t.Errorf("Result.Refused should be true on a cage-setup failure (the refusal marker); stderr=%q", res.Stderr)
	}
}

// TestCageWorktreeWritableAllowsBuild proves WorktreeWritable: a shell can write
// in its working directory (to build/edit in place) while everything else stays
// confined.
func TestCageWorktreeWritableAllowsBuild(t *testing.T) {
	requireCage(t)
	wt := t.TempDir()
	argv := []string{selfExe(t), "__probe__", "write", filepath.Join(wt, "built.txt")}
	res, _ := Run(context.Background(), Spec{
		Argv: argv, Worktree: wt, Scratch: filepath.Join(wt, ".scratch"),
		ReadFiles: []string{selfExe(t)}, WorktreeWritable: true, ABIFloor: 1,
	})
	if !strings.Contains(res.Stdout, "write OK") {
		t.Errorf("WorktreeWritable should allow writing in the worktree; got stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

// TestCageDevNullWritable proves the minimal /dev: ordinary commands can write to
// /dev/null (otherwise almost everything breaks).
func TestCageDevNullWritable(t *testing.T) {
	requireCage(t)
	wt := t.TempDir()
	res := probe(t, wt, filepath.Join(wt, ".scratch"), "write", "/dev/null")
	if !strings.Contains(res.Stdout, "write OK") {
		t.Errorf("/dev/null should be writable in the cage; got %q", res.Stdout)
	}
}

// TestCageDeniesProc proves the restored P4 posture: /proc is NOT readable in the
// cage. It is mounted fresh for pid-namespace process isolation but deliberately
// NOT Landlock-granted, so the global procfs surface (/proc/modules, /proc/sys/...)
// stays denied — a P4.6 review caught a regression that had granted it.
func TestCageDeniesProc(t *testing.T) {
	requireCage(t)
	wt := t.TempDir()
	out := probe(t, wt, filepath.Join(wt, ".scratch"), "read", "/proc/modules").Stdout
	if strings.Contains(out, "read OK") || !strings.Contains(out, "DENIED") {
		t.Errorf("SECURITY: /proc must be denied in the cage (proven posture); got %q", out)
	}
}

// TestCageMasksDotGit proves the shell cannot plant a git hook in the REAL repo:
// with a writable worktree, <worktree>/.git is masked by an ephemeral read-only
// tmpfs, so a write into .git never reaches the host's .git — closing the deferred
// host-side persistence vector (a hook firing when the human later runs git).
func TestCageMasksDotGit(t *testing.T) {
	requireCage(t)
	wt := t.TempDir()
	hooks := filepath.Join(wt, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(hooks, "post-checkout")
	argv := []string{selfExe(t), "__probe__", "write", planted}
	res, _ := Run(context.Background(), Spec{
		Argv: argv, Worktree: wt, Scratch: filepath.Join(wt, ".scratch"),
		ReadFiles: []string{selfExe(t)}, WorktreeWritable: true, ABIFloor: 1,
	})
	if res.Refused {
		t.Fatalf("cage refused unexpectedly: %q", res.Stderr)
	}
	if _, err := os.Stat(planted); !os.IsNotExist(err) {
		t.Errorf("SECURITY: a write into <worktree>/.git reached the real repo (stat err=%v); .git must be masked. probe stdout=%q", err, res.Stdout)
	}
}
