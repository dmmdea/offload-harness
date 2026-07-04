//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	seccomp "github.com/elastic/go-seccomp-bpf"
	"github.com/landlock-lsm/go-landlock/landlock"
	llsys "github.com/landlock-lsm/go-landlock/landlock/syscall"
	"golang.org/x/sys/unix"
)

// deniedSyscalls are never-legitimate-for-a-task syscalls; a caged process that
// invokes one is KILLED (SIGSYS) — a clean, definitive deny that a program
// cannot ignore and retry around (unlike an errno). Every name must exist in the
// amd64 syscall table (an unknown name fails the whole filter load — proven by
// the live suite). The i386/compat ABI is closed separately by installArchGuard.
var deniedSyscalls = []string{
	"ptrace",                                       // debugger attach / memory poke
	"mount", "umount2", "pivot_root", "move_mount", // mount-table escapes
	"keyctl", "add_key", "request_key", // kernel keyring
	"bpf", "kexec_load", // load kernel programs / new kernel
	"perf_event_open",                              // perf side channels
	"init_module", "finit_module", "delete_module", // kernel modules
	"process_vm_readv", "process_vm_writev", // cross-process memory
	"userfaultfd",                                                           // a classic exploit primitive
	"fsopen", "fsconfig", "fsmount", "fspick", "open_tree", "mount_setattr", // the new mount API
	"kexec_file_load",                        // load a new kernel (image-fd variant)
	"open_by_handle_at", "name_to_handle_at", // by-handle open bypasses Landlock path resolution
	"io_uring_setup", "io_uring_enter", "io_uring_register", // async ring whose queued ops evade syscall filtering
	"setns",         // re-enter another namespace
	"fanotify_init", // filesystem-wide notification (broad read)
	"quotactl",      // filesystem quota control
}

// Available reports whether the OS cage can be enforced on this host: Landlock
// present (ABI >= 1), seccomp supported, and unprivileged user namespaces
// enabled. Used as the capability gate — a future shell tool is granted ONLY
// when this is true (fail-closed).
func Available() (bool, string) {
	abi, err := llsys.LandlockGetABIVersion()
	if err != nil || abi < 1 {
		return false, fmt.Sprintf("Landlock unavailable (abi=%d, err=%v)", abi, err)
	}
	if !seccomp.Supported() {
		return false, "seccomp(2) not supported by this kernel"
	}
	if b, e := os.ReadFile("/proc/sys/user/max_user_namespaces"); e == nil {
		if strings.TrimSpace(string(b)) == "0" {
			return false, "unprivileged user namespaces are disabled (max_user_namespaces=0)"
		}
	}
	// Debian/Ubuntu gate unprivileged userns creation behind this sysctl; "0" means
	// clone(CLONE_NEWUSER) will EPERM. Absent on most kernels (== allowed).
	if b, e := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone"); e == nil {
		if strings.TrimSpace(string(b)) == "0" {
			return false, "unprivileged user namespaces are disabled (unprivileged_userns_clone=0)"
		}
	}
	return true, fmt.Sprintf("Landlock ABI %d + seccomp + user namespaces", abi)
}

// IsWorker reports whether this process is a re-exec'd sandbox worker.
func IsWorker() bool { return os.Getenv(workerEnv) != "" }

// RunWorkerFromEnv, when this process IS the worker, applies the cage and execs
// Spec.Argv — it never returns on success. Call it at the very top of main()
// (and TestMain) so the worker path runs before any normal startup. A no-op
// when this is not a worker.
func RunWorkerFromEnv() {
	if !IsWorker() {
		return
	}
	if err := runWorker(); err != nil {
		fmt.Fprintln(os.Stderr, refusalMarker+"sandbox worker: "+err.Error())
		os.Exit(127)
	}
	fmt.Fprintln(os.Stderr, refusalMarker+"sandbox worker: exec returned without error (bug)")
	os.Exit(126) // runWorker execs on success; reaching here is a bug
}

// runWorker is the CHILD side after the parent re-exec'd this binary into fresh
// namespaces. ORDER IS LOAD-BEARING: floor check → chdir → seccomp(+no_new_privs)
// → Landlock → exec. seccomp installs no_new_privs (required before Landlock) and
// is set before Landlock so neither can be dropped; both are inherited by every
// child of the exec'd command and cannot be removed (kernel guarantee).
func runWorker() error {
	spec, err := decodeSpec(os.Getenv(workerEnv))
	if err != nil {
		return fmt.Errorf("decode spec: %w", err)
	}
	if len(spec.Argv) == 0 {
		return fmt.Errorf("empty argv")
	}

	// (1) FAIL-CLOSED Landlock floor. BestEffort() silently no-ops on a kernel
	// without Landlock and returns nil, so trusting it is unsafe — assert the
	// achieved ABI meets the floor first, else refuse to run uncaged.
	abi, err := llsys.LandlockGetABIVersion()
	if err != nil || abi < spec.ABIFloor {
		return fmt.Errorf("landlock floor not met (have abi=%d err=%v, need >=%d) — refusing to run uncaged", abi, err, spec.ABIFloor)
	}

	// (1b) In the private mount namespace (CLONE_NEWNS): stop mount propagation to
	// the host, then mount a FRESH /proc scoped to THIS pid namespace so it exposes
	// only the cage's own processes, never the host's. This is process-isolation
	// defense-in-depth ONLY; /proc is deliberately NOT Landlock-granted below,
	// because a procfs mount still exposes the GLOBAL, non-pid-namespaced surface
	// (/proc/sys, /proc/modules, /proc/kallsyms). The P4 cage's proven posture is
	// /proc-denied and P4.6 keeps it. These mount syscalls MUST run here, before
	// seccomp denies them.
	_ = unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, "")
	_ = unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "")

	// Mask the repo's .git so a caged shell with a WRITABLE worktree cannot plant a
	// git hook or rewrite .git/config to execute LATER, on the HOST, when the human
	// runs git outside the cage. The P2 write broker denies .git writes for exactly
	// this reason; the shell path's containment is the cage (not path-string
	// filtering), so mirror the protection here with an empty read-only tmpfs over
	// the real .git. Only for a writable worktree holding a real .git directory; the
	// mount lives in this private namespace and never touches the host's tree.
	if spec.WorktreeWritable {
		if gp := filepath.Join(spec.Worktree, ".git"); dirExists(gp) {
			_ = unix.Mount("tmpfs", gp, "tmpfs", unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "")
		}
	}

	// (2) chdir into the worktree BEFORE any restriction (no rule blocks it yet).
	if err := os.Chdir(spec.Worktree); err != nil {
		return fmt.Errorf("chdir worktree %q: %w", spec.Worktree, err)
	}

	// (3) seccomp: KILL on any denied syscall, and set no_new_privs (required
	// before Landlock). TSYNC applies the filter to every thread; default-allow
	// so the Go runtime + the command run normally. None of the worker's own
	// remaining syscalls (landlock_*, prctl, chdir, execve) are on the denylist.
	if err := seccomp.LoadFilter(seccomp.Filter{
		NoNewPrivs: true,
		Flag:       seccomp.FilterFlagTSync,
		Policy: seccomp.Policy{
			DefaultAction: seccomp.ActionAllow,
			Syscalls: []seccomp.SyscallGroup{{
				Action: seccomp.ActionKillProcess,
				Names:  deniedSyscalls,
			}},
		},
	}); err != nil {
		return fmt.Errorf("install seccomp: %w", err)
	}

	// (3b) Close the i386/compat-ABI bypass of the denylist: a stacked arch-guard
	// that KILLs any non-x86_64 syscall (the denylist filter is x86_64-only and
	// returns ALLOW for foreign arches). seccomp takes the most-restrictive result
	// across all installed filters, so this hard-blocks the i386 (int 0x80) path.
	if err := installArchGuard(); err != nil {
		return fmt.Errorf("install arch-guard: %w", err)
	}

	// (4) Landlock FS confinement. V4 = paths (V1-3) + TCP net handling (V4): with
	// FS rules but NO ConnectTCP/BindTCP rule, ALL TCP bind+connect is denied —
	// defense-in-depth on top of the empty network namespace (which already has no
	// route). Default-deny: any path not granted below is inaccessible — so /home,
	// ~/.ssh, ~/.claude, the memory store, /proc, /dev, /tmp are all unreadable.
	roDirs := append(append([]string{}, defaultReadDirs...), spec.ReadDirs...)
	rules := []landlock.Rule{
		landlock.RODirs(roDirs...).IgnoreIfMissing(),
		landlock.RWDirs(spec.Scratch).IgnoreIfMissing(),
		// a minimal /dev so ordinary commands work (write to /dev/null, read randomness)
		landlock.RWFiles("/dev/null", "/dev/zero").IgnoreIfMissing(),
		landlock.ROFiles("/dev/urandom", "/dev/random").IgnoreIfMissing(),
	}
	// The worktree is the working directory: RW when the caller must build/edit in
	// place (a shell), else RO (read-only inspection).
	if spec.WorktreeWritable {
		rules = append(rules, landlock.RWDirs(spec.Worktree).IgnoreIfMissing())
	} else {
		rules = append(rules, landlock.RODirs(spec.Worktree).IgnoreIfMissing())
	}
	// Grant specific files (read + execute) WITHOUT opening their parent dir — e.g.
	// a static helper binary that lives outside the granted dirs. Granting the dir
	// instead would expose its siblings.
	if len(spec.ReadFiles) > 0 {
		rules = append(rules, landlock.ROFiles(spec.ReadFiles...).IgnoreIfMissing())
	}
	if err := landlock.V4.BestEffort().Restrict(rules...); err != nil {
		return fmt.Errorf("landlock restrict: %w", err)
	}

	// Basic resource caps (best-effort): no core dumps (avoid spilling memory into
	// the worktree) and a bounded max file size. Fork-bomb / CPU exhaustion is
	// bounded by the PID namespace + the caller's context timeout (which tears the
	// whole pidns down on expiry); finer cgroup limits are future work.
	_ = unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0})
	_ = unix.Setrlimit(unix.RLIMIT_FSIZE, &unix.Rlimit{Cur: 1 << 30, Max: 1 << 30})

	// (5) exec the command inside the cage with a minimal env. syscall.Exec
	// replaces the process, so the cage (namespaces + seccomp + Landlock) is fully
	// in effect for it and every descendant.
	env := []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + spec.Scratch,
		"TMPDIR=" + spec.Scratch,
	}
	return syscall.Exec(spec.Argv[0], spec.Argv, env)
}

// dirExists reports whether p resolves to a directory (symlinks followed). Used
// before bind-masking <worktree>/.git, which must exist as a real dir to mount over.
func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// installArchGuard adds a tiny classic-BPF seccomp filter that KILLs any syscall
// whose audit arch is not x86_64. go-seccomp-bpf builds a single-arch (x86_64)
// filter that returns its default action (ALLOW) for every other arch, so on a
// CONFIG_IA32_EMULATION kernel the denylist is bypassable via the i386 (int 0x80)
// ABI. The kernel evaluates ALL installed seccomp filters and takes the
// most-restrictive result, so stacking this guard hard-blocks the foreign-arch
// path regardless of what the denylist filter returns. Installed AFTER the
// denylist (which set no_new_privs, required before SECCOMP_SET_MODE_FILTER).
func installArchGuard() error {
	// seccomp_data.arch is a u32 at byte offset 4. Classic BPF:
	//   A = arch; if A == AUDIT_ARCH_X86_64 -> ALLOW; else -> KILL_PROCESS
	filter := []unix.SockFilter{
		{Code: 0x20, K: 4}, // BPF_LD|BPF_W|BPF_ABS  A = mem[4] (arch)
		{Code: 0x15, Jt: 1, Jf: 0, K: uint32(unix.AUDIT_ARCH_X86_64)}, // BPF_JMP|BPF_JEQ|BPF_K  ==x86_64 ? skip the kill
		{Code: 0x06, K: uint32(unix.SECCOMP_RET_KILL_PROCESS)},        // BPF_RET|BPF_K          foreign arch -> kill
		{Code: 0x06, K: uint32(unix.SECCOMP_RET_ALLOW)},               // BPF_RET|BPF_K          x86_64 -> allow (denylist applies)
	}
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, uintptr(unix.SECCOMP_SET_MODE_FILTER), 0, uintptr(unsafe.Pointer(&prog))); errno != 0 {
		return fmt.Errorf("seccomp(SET_MODE_FILTER) arch-guard: %w", errno)
	}
	return nil
}

// Run launches cmd (spec.Argv) inside the OS cage: a fresh UNPRIVILEGED
// user+network+pid+ipc+uts namespace (the user namespace grants the namespaced
// privilege to make the others; the empty net namespace severs ALL egress,
// including the loopback model/memory services), then — in the re-exec'd worker
// — seccomp + Landlock. Returns the command's captured output and exit code.
func Run(ctx context.Context, spec Spec) (Result, error) {
	if len(spec.Argv) == 0 {
		return Result{}, fmt.Errorf("sandbox.Run: empty Argv")
	}
	// Fail-closed on caller misuse: go-landlock opens rule paths relative to cwd
	// and the worker chdir's into Worktree, so a relative path would grant or exec
	// the WRONG subtree. Require every path absolute.
	absPaths := append([]string{spec.Worktree, spec.Scratch, spec.Argv[0]}, spec.ReadDirs...)
	absPaths = append(absPaths, spec.ReadFiles...)
	for _, p := range absPaths {
		if p != "" && !filepath.IsAbs(p) {
			return Result{}, fmt.Errorf("sandbox.Run: all paths must be absolute, got %q", p)
		}
	}
	if spec.ABIFloor < 1 {
		spec.ABIFloor = 1
	}
	if spec.Scratch != "" {
		if err := os.MkdirAll(spec.Scratch, 0o700); err != nil {
			return Result{}, fmt.Errorf("create scratch: %w", err)
		}
	}
	js, err := spec.encode()
	if err != nil {
		return Result{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return Result{}, err
	}

	cmd := exec.CommandContext(ctx, exe)
	cmd.Args = []string{"offload-sandbox-worker"} // cosmetic argv[0]; cmd.Path is the real exe
	cmd.Env = []string{workerEnv + "=" + js}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET | syscall.CLONE_NEWPID |
			syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS | syscall.CLONE_NEWNS,
		// Map the invoking uid/gid to 0 INSIDE the user namespace so the worker can
		// create the other namespaces; outside, it is still the unprivileged user.
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false, // write "deny" to setgroups — required for an unprivileged gid map
	}

	runErr := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	// HasPrefix (not Contains): on a real refusal the worker writes the marker and
	// exits WITHOUT exec'ing, so it is the first thing on stderr; on success the
	// worker exec's (writes nothing) and the command owns stderr — so a command
	// cannot spoof a refusal unless it deliberately leads its stderr with the marker.
	res.Refused = strings.HasPrefix(res.Stderr, refusalMarker)
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.Signal = int(ws.Signal())
		}
	}
	if runErr != nil {
		return res, fmt.Errorf("sandbox run: %w (stderr: %s)", runErr, strings.TrimSpace(stderr.String()))
	}
	return res, nil
}
