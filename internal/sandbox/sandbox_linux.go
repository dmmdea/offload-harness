//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
// syscall table of every caged arch, amd64 and arm64 (an unknown name fails the whole
// filter load — proven by the live suite). The compat ABIs (i386 on amd64, 32-bit arm
// on arm64) are closed separately by installArchGuard.
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
	"setns",         // re-enter another namespace
	"fanotify_init", // filesystem-wide notification (broad read)
	"quotactl",      // filesystem quota control
	// SF-17 (measured 2026-10-03 against the run allowlist): pidfd_getfd copies a file
	// descriptor out of another process, and no allowlisted program references it.
	// pidfd_send_signal stays ALLOWED: Go's os package signals children through it and
	// probes it on the first child spawn, so denying it kills go build and go test.
	// memfd_create stays ALLOWED because denying it buys no containment: Landlock grants
	// execute wherever it grants read, so a caged command may already write a program into
	// its scratch dir and run it (go test does exactly that), and an image started from a
	// memfd inherits the same cage. With no writable path at all it still adds nothing: a
	// caged process can run any code in its own address space (mmap PROT_EXEC, as the i386
	// probe does), and that code is caged the same way.
	"pidfd_getfd",
}

// unavailableSyscalls fail with EPERM instead of killing the caller. They are optional
// kernel features that runtimes probe at startup and fall back from, so a kill took the
// program down with them: node died in the cage at io_uring_setup (libuv probes it; audit
// syscall=425, 2026-10-03). The io_uring ring must stay unavailable, because its queued
// operations evade syscall filtering, and EPERM keeps it so: there is no ring to retry
// around. Docker's default profile and kernel.io_uring_disabled=2 refuse it the same way.
var unavailableSyscalls = []string{"io_uring_setup", "io_uring_enter", "io_uring_register"}

// Available reports whether the OS cage can be enforced on this host: an
// architecture the cage can guard, Landlock present (ABI >= 1), seccomp supported,
// and unprivileged user namespaces enabled. Used as the capability gate — a future
// shell tool is granted ONLY when this is true (fail-closed).
func Available() (bool, string) { return available(runtime.GOARCH) }

// available is Available for one architecture. The arch is checked first, so an
// architecture the cage cannot guard is refused on any host, Landlock present or not.
func available(goarch string) (bool, string) {
	if ok, why := archSupported(goarch); !ok {
		return false, why
	}
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
	if why := probeUserNamespaces(); why != "" {
		return false, why
	}
	return true, fmt.Sprintf("Landlock ABI %d + seccomp + user namespaces", abi)
}

// userNSProbe holds probeUserNamespaces' answer. Whether the cage's namespaces can be created is a
// property of the host and of this binary's AppArmor confinement, so one probe per process is enough.
var userNSProbe struct {
	once sync.Once
	why  string
}

// probeUserNamespaces creates the namespaces the cage creates, with the same attributes as Run,
// around a system true, and returns why that was refused ("" when it worked). The sysctls above are
// not the whole gate: an AppArmor that restricts unprivileged user namespaces
// (kernel.apparmor_restrict_unprivileged_userns=1, the Ubuntu 24.04 default) refuses userns_create
// to a binary no profile allows, and every caged run then failed at fork/exec with "permission
// denied" while the cage reported itself usable (register SF-46). With no true binary to run there
// is nothing to probe with, and the sysctls' answer stands.
func probeUserNamespaces() string {
	userNSProbe.once.Do(func() {
		for _, bin := range []string{"/usr/bin/true", "/bin/true"} {
			if fi, err := os.Stat(bin); err != nil || !fi.Mode().IsRegular() {
				continue
			}
			cmd := exec.Command(bin)
			cmd.Env = []string{}
			cmd.SysProcAttr = cageSysProcAttr()
			if err := cmd.Run(); err != nil {
				why := fmt.Sprintf("the cage's namespaces cannot be created here (%v)", err)
				if b, e := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); e == nil && strings.TrimSpace(string(b)) == "1" {
					why += "; kernel.apparmor_restrict_unprivileged_userns=1 refuses a user namespace to a binary no AppArmor profile allows"
				}
				userNSProbe.why = why
			}
			return
		}
	})
	return userNSProbe.why
}

// cageSysProcAttr is how Run starts the worker: fresh user, network, pid, ipc, uts and mount
// namespaces, with the invoking uid and gid mapped to 0 inside the user namespace so the worker can
// create the others; outside, it is still the unprivileged user. probeUserNamespaces uses the same.
func cageSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET | syscall.CLONE_NEWPID |
			syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS | syscall.CLONE_NEWNS,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false, // write "deny" to setgroups — required for an unprivileged gid map
	}
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
	// filtering), so mirror the protection here: an empty read-only tmpfs over a .git
	// DIRECTORY, and over a .git FILE (the "gitdir: <path>" pointer of a linked worktree,
	// which is how the harness's own worktrees are made) a read-only bind of the file onto
	// itself, because a command that can rewrite the pointer aims the host's git at a
	// directory the command controls. Only for a writable worktree; the mounts live in this
	// private namespace and never touch the host's tree.
	//
	// A mount the kernel refuses (an unprivileged user namespace that may not mount, as in
	// some container and CI sandboxes), or a .git that no mount can protect (a symbolic link),
	// does not stop the run, and it is not silent either: maskDotGit returns a note, which goes
	// to stderr just before the exec (see below) so the caller learns that the mask it was
	// promised is not there.
	var notes runNotes
	if note := maskDotGit(spec, unix.Mount); note != "" {
		notes.addf("%s", note)
	}

	// (2) chdir into the worktree BEFORE any restriction (no rule blocks it yet).
	if err := os.Chdir(spec.Worktree); err != nil {
		return fmt.Errorf("chdir worktree %q: %w", spec.Worktree, err)
	}

	// (3) seccomp: KILL on any denied syscall, EPERM on the unavailable ones, and
	// set no_new_privs (required before Landlock). TSYNC applies the filter to every
	// thread; default-allow so the Go runtime + the command run normally. None of the
	// worker's own remaining syscalls (landlock_*, prctl, chdir, execve) are on either list.
	if err := seccomp.LoadFilter(seccomp.Filter{
		NoNewPrivs: true,
		Flag:       seccomp.FilterFlagTSync,
		Policy: seccomp.Policy{
			DefaultAction: seccomp.ActionAllow,
			Syscalls: []seccomp.SyscallGroup{
				{Action: seccomp.ActionKillProcess, Names: deniedSyscalls},
				{Action: seccomp.ActionErrno, Names: unavailableSyscalls}, // EPERM
			},
		},
	}); err != nil {
		return fmt.Errorf("install seccomp: %w", err)
	}

	// (3b) Close the compat-ABI bypass of the denylist: a stacked arch-guard that
	// KILLs any syscall whose audit arch is not the native one (the denylist filter
	// covers the native arch only and returns ALLOW for foreign arches). seccomp takes
	// the most-restrictive result across all installed filters, so this hard-blocks
	// the i386 (int 0x80) path on amd64 and the 32-bit arm path on arm64.
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

	// The notes go out now: after the last step that can refuse the run, so a refusal still
	// leads stderr (refusedBy), and right before the exec, so the command's own stderr
	// follows them. The exec replaces this process; nothing could be written after it.
	if len(notes) > 0 {
		fmt.Fprint(os.Stderr, noteLines(notes, "\n"))
	}

	// (5) exec the command inside the cage with a minimal env (cageEnv, the
	// contract shared with the Windows cage — see sandbox.go). syscall.Exec
	// replaces the process, so the cage (namespaces + seccomp + Landlock) is fully
	// in effect for it and every descendant.
	return syscall.Exec(spec.Argv[0], spec.Argv, cageEnv(spec, nil))
}

// The Linux half of the cage's environment contract (sandbox.go). The child gets
// exactly these three entries, as it always has.
//
// envPassthrough is EMPTY and that is the point: nothing is copied out of the
// harness's environment, so no token can reach the caged command however the
// parent was launched. PATH does not need copying either — a caged command may
// only exec what Landlock granted, so the system list is both sufficient and
// honest.
var (
	envFixed       = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	envPassthrough []string
	envHomeNames   = []string{"HOME"}
	envTempNames   = []string{"TMPDIR"}
)

// dirExists reports whether p resolves to a directory (symlinks followed). Used
// before bind-masking <worktree>/.git, which must exist as a real dir to mount over.
func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// isRegularFile reports whether p ITSELF is a regular file (links are not followed). A symlink to a
// file does not count: a mount follows the link, so it would land on the target and leave the link,
// which sits in the writable worktree, free to be replaced.
func isRegularFile(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode().IsRegular()
}

// isSymlink reports whether p ITSELF is a symbolic link, whatever it points at (or at nothing).
func isSymlink(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

// mountFunc has the shape of unix.Mount, so a test can stand in for a kernel that refuses.
type mountFunc func(source, target, fstype string, flags uintptr, data string) error

// maskDotGit makes <worktree>/.git read-only for the command, for a writable worktree (see runWorker
// for why): an empty read-only tmpfs over a .git directory, a read-only bind of the file onto itself
// where .git is a file (maskDotGitFile). A .git that is a symbolic link no mount can protect: a mount
// follows the link, so it lands on the link's target (which is what the tmpfs does for a link to a
// directory, as it always did) and the link itself, in the writable worktree, can be replaced.
//
// It returns "" when the mask is in place or there is nothing to mask (no .git), and otherwise the
// notes, joined into one line: one for each mount that was refused, and one for a symbolic link.
// Then the command still runs, with <worktree>/.git writable like the rest of the worktree, which is
// exactly what the mask exists to prevent, and the caller must be told.
func maskDotGit(spec Spec, mount mountFunc) string {
	if !spec.WorktreeWritable {
		return ""
	}
	gp := filepath.Join(spec.Worktree, ".git")
	var notes []string
	switch {
	case dirExists(gp): // a directory, or a link to one: the mount follows the link
		if err := mount("tmpfs", gp, "tmpfs", unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
			notes = append(notes, fmt.Sprintf("%s is NOT masked read-only (mount: %v): a command in this cage can write into it, hooks included", gp, err))
		}
	case isRegularFile(gp):
		if note := maskDotGitFile(gp, mount); note != "" {
			notes = append(notes, note)
		}
	}
	if isSymlink(gp) {
		notes = append(notes, fmt.Sprintf("%s is a symbolic link: it cannot be masked read-only, so a command in this cage can replace it and point git at a directory of its own", gp))
	}
	return strings.Join(notes, "; ")
}

// dotGitFileRemount is the flag set of the read-only remount in maskDotGitFile. A user namespace
// LOCKS the nosuid, nodev, noexec, read-only and atime flags of every mount it inherits from the
// host, and the kernel refuses (EPERM) a remount that clears one of them, so the remount repeats the
// first four wherever the worktree sits (a shared-memory mount and a systemd /tmp are nosuid and
// nodev) and names no atime flag at all, which makes the kernel keep the mount's own (mount(2): a
// remount naming none preserves them, since Linux 3.17; the cage runs on 6.7 and later).
const dotGitFileRemount = unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC

// maskDotGitFile makes the .git FILE of a linked worktree read-only. The file stays readable (git
// reads it to find its git directory) and cannot be rewritten, truncated, unlinked or renamed over:
// a bind mount of the file onto itself, then a read-only remount of that bind (a bind mount takes
// its flags only through a remount). The read-only flag belongs to the mount, not to the inode, so a
// second name for the file on the worktree's own mount would be a way around it; none can be made,
// because a link across the two mounts is refused (EXDEV). The real git directory sits outside the
// worktree and outside the cage's write roots already; the pointer is what this protects. A refused
// step is a note, and a refused bind is not followed by a remount.
func maskDotGitFile(gp string, mount mountFunc) string {
	if err := mount(gp, gp, "", unix.MS_BIND, ""); err != nil {
		return dotGitFileNote(gp, "bind mount", err)
	}
	if err := mount("", gp, "", dotGitFileRemount, ""); err != nil {
		return dotGitFileNote(gp, "read-only remount", err)
	}
	return ""
}

// dotGitFileNote is the note for a .git file that is not masked: the file, the step the kernel
// refused and why, and what the caller lost.
func dotGitFileNote(gp, step string, err error) string {
	return fmt.Sprintf("%s is NOT masked read-only (%s: %v): a command in this cage can write into it and point git at a directory of its own", gp, step, err)
}

// nativeAuditArch is the seccomp audit arch of the architecture the cage runs on, and
// whether the cage supports it at all. It used to be x86_64 everywhere, which on arm64
// killed every syscall of every caged command (SF-44 probe, 2026-10-03).
func nativeAuditArch(goarch string) (uint32, bool) {
	switch goarch {
	case "amd64":
		return uint32(unix.AUDIT_ARCH_X86_64), true
	case "arm64":
		return uint32(unix.AUDIT_ARCH_AARCH64), true
	}
	return 0, false
}

// archSupported reports whether the cage can guard this architecture, and why not.
func archSupported(goarch string) (bool, string) {
	if _, ok := nativeAuditArch(goarch); !ok {
		return false, fmt.Sprintf("the cage supports amd64 and arm64, not %s (no native seccomp arch guard)", goarch)
	}
	return true, ""
}

// archGuardProgram is the arch guard for goarch as classic BPF. seccomp_data.arch is a
// u32 at byte offset 4: A = arch; if A == native -> ALLOW (the denylist filter then
// decides); else -> KILL_PROCESS.
func archGuardProgram(goarch string) ([]unix.SockFilter, error) {
	native, ok := nativeAuditArch(goarch)
	if !ok {
		_, why := archSupported(goarch)
		return nil, errors.New(why)
	}
	return []unix.SockFilter{
		{Code: 0x20, K: 4},                                     // BPF_LD|BPF_W|BPF_ABS  A = mem[4] (arch)
		{Code: 0x15, Jt: 1, Jf: 0, K: native},                  // BPF_JMP|BPF_JEQ|BPF_K  ==native ? skip the kill
		{Code: 0x06, K: uint32(unix.SECCOMP_RET_KILL_PROCESS)}, // BPF_RET|BPF_K          foreign arch -> kill
		{Code: 0x06, K: uint32(unix.SECCOMP_RET_ALLOW)},        // BPF_RET|BPF_K          native -> allow (denylist applies)
	}, nil
}

// installArchGuard adds a tiny classic-BPF seccomp filter that KILLs any syscall
// whose audit arch is not the NATIVE one (x86_64 on amd64, aarch64 on arm64).
// go-seccomp-bpf builds a single-arch filter that returns its default action (ALLOW)
// for every other arch, so on a kernel with a compat ABI (i386 int 0x80 on amd64,
// 32-bit arm on arm64) the denylist is bypassable through it. The kernel evaluates
// ALL installed seccomp filters and takes the most-restrictive result, so stacking
// this guard hard-blocks the foreign-arch path regardless of what the denylist filter
// returns. Installed AFTER the denylist (which set no_new_privs, required before
// SECCOMP_SET_MODE_FILTER), and with TSYNC like the denylist: without it the guard sat
// on the installing thread only, while the Go runtime may run the final exec on another
// thread, whose image then carried the denylist and no guard (SF-17 review: i386 calls
// survived the cage under load).
func installArchGuard() error {
	filter, err := archGuardProgram(runtime.GOARCH)
	if err != nil {
		return err
	}
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	// With TSYNC the call returns 0 on success; when a thread cannot be synchronized it
	// attaches nothing and returns that thread's id, with no errno.
	tid, _, errno := unix.Syscall(unix.SYS_SECCOMP, uintptr(unix.SECCOMP_SET_MODE_FILTER),
		uintptr(unix.SECCOMP_FILTER_FLAG_TSYNC), uintptr(unsafe.Pointer(&prog)))
	if errno != 0 {
		return fmt.Errorf("seccomp(SET_MODE_FILTER) arch-guard: %w", errno)
	}
	if tid != 0 {
		return fmt.Errorf("seccomp(SET_MODE_FILTER) arch-guard: thread %d could not be synchronized", tid)
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
	// The network guarantee needs ABI >= NetABIFloor (see sandbox.go); a
	// lower request is raised, never honoured (0.115.22, register H-27).
	spec.ABIFloor = EffectiveABIFloor(spec.ABIFloor)
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
	cmd.SysProcAttr = cageSysProcAttr()

	runErr := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	// A prefix test (not Contains): on a real refusal the worker writes the marker and
	// exits WITHOUT exec'ing, so it is the first thing on stderr, behind at most the
	// notes it wrote before an exec that then failed; on success the worker exec's and
	// the command owns stderr after the notes — so a command cannot spoof a refusal
	// unless it deliberately leads its stderr with the marker.
	res.Refused = refusedBy(res.Stderr)
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
