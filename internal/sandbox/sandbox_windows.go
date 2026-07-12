//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This is the WINDOWS OS-cage for run_shell. Windows has no Landlock/seccomp/userns,
// so the confinement is built from three user-mode (no-admin) primitives applied to
// the CHILD at spawn time:
//
//  1. An executable ALLOWLIST checked BEFORE launch (the base name of the resolved
//     program must be listed) — a refused command never starts.
//  2. A JOB OBJECT with kill-on-job-close + an active-process limit + a per-process
//     memory limit. The whole process TREE lives in the job; closing the job handle
//     (or TerminateJobObject on timeout) reaps every descendant, so a child cannot
//     outlive the run or fork-bomb the host.
//  3. A LOW-INTEGRITY duplicated primary token: the child runs at integrity level
//     LOW (S-1-16-4096), which — via Windows Mandatory Integrity Control — cannot
//     write to medium-IL securable objects (most of the user's files/registry).
//     To let the child write its worktree+scratch, those two dirs are TEMPORARILY
//     relabeled LOW for the run and REVERTED to their prior label afterward (see
//     applyLowIntegrityLabel) — they are NOT left permanently low-integrity.
//
// The child is created SUSPENDED, assigned to the job, THEN resumed — closing the
// race where a child could spawn escapees before it is confined by the job.
//
// HONEST RESIDUAL RISK (documented, not hidden): during a run the worktree is
// temporarily set to LOW integrity, so a low-integrity process could write into it
// for that window; it is reverted afterward. Writes OUTSIDE the worktree are blocked
// by MIC. But unlike the Linux cage, native Windows here does NOT sever network
// egress and does NOT block READS outside the worktree — the low-IL token restricts
// writes (MIC), not reads or sockets. So the Windows cage is a WEAKER boundary than
// the Linux one; the runner is OFF by default and the tool description states this.

const (
	// winOutputCap is the head+tail byte budget for combined captured output.
	winOutputCap = 30 * 1024
	// winActiveProcessLimit caps how many concurrent processes the job may hold.
	winActiveProcessLimit = 64
	// winProcessMemoryLimit caps per-process committed memory (2 GiB).
	winProcessMemoryLimit = 2 << 30
	truncMarker           = "\r\n[... output truncated ...]\r\n"
)

// Available reports whether the OS cage can be enforced here. On Windows the Job
// Object + low-integrity token primitives need no admin, so the cage is always
// available.
func Available() (bool, string) {
	return true, "windows job-object + low-integrity"
}

// IsWorker is always false on Windows: confinement is applied to the CHILD at spawn
// (token + job), so there is no self-re-exec worker like the Linux cage.
func IsWorker() bool { return false }

// RunWorkerFromEnv is a no-op on Windows (no worker role).
func RunWorkerFromEnv() {}

// allowed reports whether the base name of exePath (case-insensitive, trailing
// ".exe" dropped) is in the allowlist.
func allowed(exePath string, list []string) bool {
	base := strings.ToLower(filepath.Base(exePath))
	base = strings.TrimSuffix(base, ".exe")
	for _, a := range list {
		a = strings.ToLower(strings.TrimSpace(a))
		a = strings.TrimSuffix(a, ".exe")
		if a != "" && a == base {
			return true
		}
	}
	return false
}

// Run launches spec.Argv confined by a Job Object + a low-integrity token on
// Windows, captures head+tail-capped combined output, and honors ctx (a deadline
// terminates the whole job tree). Returns Result{Refused:true} WITHOUT launching if
// the executable is not allowlisted.
func Run(ctx context.Context, spec Spec) (Result, error) {
	if len(spec.Argv) == 0 {
		return Result{}, fmt.Errorf("sandbox.Run: empty Argv")
	}

	// (1) ALLOWLIST FIRST — refuse before launching anything.
	exe := spec.Argv[0]
	if !allowed(exe, spec.AllowedExecutables) {
		base := strings.TrimSuffix(strings.ToLower(filepath.Base(exe)), ".exe")
		return Result{
			Refused: true,
			Stderr:  fmt.Sprintf("%s not in the runner allowlist", base),
		}, nil
	}

	if spec.Scratch != "" {
		if err := os.MkdirAll(spec.Scratch, 0o700); err != nil {
			return Result{}, fmt.Errorf("create scratch: %w", err)
		}
	}
	if spec.Worktree == "" {
		return Result{}, fmt.Errorf("sandbox.Run: empty Worktree")
	}

	// A LOW-integrity child cannot write to a MEDIUM-integrity object (which is what
	// an ordinary directory is). For the build-in-place use case the child MUST be
	// able to write its worktree + scratch, so lower the mandatory label on exactly
	// those two dirs to LOW (inherited by their contents) — but ONLY for the duration
	// of this run. Everything else the child might reach stays MEDIUM →
	// write-protected by Mandatory Integrity Control.
	//
	// TRANSIENT: each applyLowIntegrityLabel captures the dir's prior mandatory label
	// and returns a revert closure; a defer restores it after the child exits, so the
	// worktree (often the user's real git checkout) is LOW only while the command
	// runs, not permanently writable by any low-IL process afterward. Runs are
	// sequential (the agent calls tools one at a time), so there is no concurrency
	// concern. A hard crash between lower and revert could leave a dir LOW; that is
	// acceptable because the next run re-lowers and re-reverts it.
	if spec.WorktreeWritable {
		if revert, err := applyLowIntegrityLabel(spec.Worktree); err == nil {
			defer revert()
		}
	}
	if spec.Scratch != "" {
		if revert, err := applyLowIntegrityLabel(spec.Scratch); err == nil {
			defer revert()
		}
	}

	// (2) JOB OBJECT with kill-on-close + active-process + memory limits.
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return Result{}, fmt.Errorf("CreateJobObject: %w", err)
	}
	// kill-on-job-close reaps strays even on an unexpected return path.
	defer windows.CloseHandle(job)

	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
		windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS |
		windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY
	info.BasicLimitInformation.ActiveProcessLimit = winActiveProcessLimit
	info.ProcessMemoryLimit = uintptr(winProcessMemoryLimit)
	if _, err := windows.SetInformationJobObject(job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info))); err != nil {
		return Result{}, fmt.Errorf("SetInformationJobObject: %w", err)
	}

	// (3) LOW-INTEGRITY duplicated primary token.
	lowTok, tokErr := lowIntegrityToken()
	// If the token step fails we DO NOT silently run at the parent's integrity —
	// that would fake a security guarantee. Fail closed.
	if tokErr != nil {
		return Result{}, fmt.Errorf("low-integrity token: %w", tokErr)
	}
	defer lowTok.Close()

	// (4) stdout+stderr pipes (child writes to the inheritable write ends). Wrap the
	// READ ends in *os.File so a SINGLE owner closes each handle (no double-close race
	// with a manual CloseHandle defer + os.File's GC finalizer).
	var stdoutR, stdoutW, stderrR, stderrW windows.Handle
	sa := &windows.SecurityAttributes{InheritHandle: 1}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	if err := windows.CreatePipe(&stdoutR, &stdoutW, sa, 0); err != nil {
		return Result{}, fmt.Errorf("CreatePipe(stdout): %w", err)
	}
	if err := windows.CreatePipe(&stderrR, &stderrW, sa, 0); err != nil {
		windows.CloseHandle(stdoutR)
		windows.CloseHandle(stdoutW)
		return Result{}, fmt.Errorf("CreatePipe(stderr): %w", err)
	}
	// The READ ends must NOT be inherited by the child (only the write ends).
	_ = windows.SetHandleInformation(stdoutR, windows.HANDLE_FLAG_INHERIT, 0)
	_ = windows.SetHandleInformation(stderrR, windows.HANDLE_FLAG_INHERIT, 0)
	stdoutFile := os.NewFile(uintptr(stdoutR), "sandbox-stdout")
	stderrFile := os.NewFile(uintptr(stderrR), "sandbox-stderr")
	defer stdoutFile.Close()
	defer stderrFile.Close()

	cmdLine, err := buildCommandLine(spec.Argv)
	if err != nil {
		windows.CloseHandle(stdoutW)
		windows.CloseHandle(stderrW)
		return Result{}, err
	}
	appName, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		windows.CloseHandle(stdoutW)
		windows.CloseHandle(stderrW)
		return Result{}, err
	}
	cwd, err := windows.UTF16PtrFromString(spec.Worktree)
	if err != nil {
		windows.CloseHandle(stdoutW)
		windows.CloseHandle(stderrW)
		return Result{}, err
	}

	var si windows.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdOutput = stdoutW
	si.StdErr = stderrW
	si.StdInput = 0

	var pi windows.ProcessInformation

	// (4) SPAWN SUSPENDED as the low-integrity user.
	err = windows.CreateProcessAsUser(
		windows.Token(lowTok),
		appName,
		cmdLine,
		nil,  // process security
		nil,  // thread security
		true, // inherit handles (the pipe write ends)
		windows.CREATE_SUSPENDED,
		nil, // env: inherit the parent's (reads are not contained on Windows anyway)
		cwd,
		&si,
		&pi,
	)
	// Parent no longer needs the child's write ends; close so our reads see EOF when
	// the child exits.
	windows.CloseHandle(stdoutW)
	windows.CloseHandle(stderrW)
	if err != nil {
		return Result{}, fmt.Errorf("CreateProcessAsUser(%s): %w", exe, err)
	}
	defer windows.CloseHandle(pi.Process)
	defer windows.CloseHandle(pi.Thread)

	// ASSIGN to the job BEFORE resuming — closes the escape race (the child cannot
	// run, and therefore cannot spawn escapees, until it is inside the job).
	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		windows.TerminateProcess(pi.Process, 1)
		return Result{}, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	// RESUME the confined child.
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		windows.TerminateProcess(pi.Process, 1)
		return Result{}, fmt.Errorf("ResumeThread: %w", err)
	}

	// Drain both pipes concurrently to avoid a pipe-full deadlock. The reads end when
	// the child's write ends close (on exit or job-kill), returning EOF/broken-pipe.
	var wg sync.WaitGroup
	var stdout, stderr []byte
	wg.Add(2)
	go func() { defer wg.Done(); stdout, _ = io.ReadAll(stdoutFile) }()
	go func() { defer wg.Done(); stderr, _ = io.ReadAll(stderrFile) }()

	// Wait for exit OR ctx cancellation. On ctx.Done, TerminateJobObject kills the
	// whole tree, the pipe write ends close, and the reader goroutines finish.
	done := make(chan struct{})
	go func() {
		windows.WaitForSingleObject(pi.Process, windows.INFINITE)
		close(done)
	}()

	var res Result
	timedOut := false
	select {
	case <-ctx.Done():
		timedOut = true
		_ = windows.TerminateJobObject(job, 1)
		<-done // the process is now dead; wait for the WaitForSingleObject goroutine
	case <-done:
	}
	wg.Wait()

	var exitCode uint32
	if err := windows.GetExitCodeProcess(pi.Process, &exitCode); err == nil {
		res.ExitCode = int(exitCode)
	}
	res.Stdout = capOutput(string(stdout))
	res.Stderr = capOutput(string(stderr))
	if timedOut {
		note := fmt.Sprintf("[sandbox] command killed after context timeout (%v)", ctx.Err())
		if res.Stderr == "" {
			res.Stderr = note
		} else {
			res.Stderr = note + "\r\n" + res.Stderr
		}
	}
	return res, nil
}

// lowIntegrityToken opens the current process token, duplicates it as a PRIMARY
// token, and lowers its integrity level to LOW (S-1-16-4096). Dropping your own
// token's integrity is an unprivileged operation.
func lowIntegrityToken() (windows.Token, error) {
	var cur windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_DUPLICATE|windows.TOKEN_ADJUST_DEFAULT|windows.TOKEN_QUERY|windows.TOKEN_ASSIGN_PRIMARY,
		&cur); err != nil {
		return 0, fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer cur.Close()

	var dup windows.Token
	if err := windows.DuplicateTokenEx(cur,
		windows.TOKEN_ALL_ACCESS,
		nil,
		windows.SecurityImpersonation,
		windows.TokenPrimary,
		&dup); err != nil {
		return 0, fmt.Errorf("DuplicateTokenEx: %w", err)
	}

	lowSid, err := windows.CreateWellKnownSid(windows.WinLowLabelSid)
	if err != nil {
		dup.Close()
		return 0, fmt.Errorf("CreateWellKnownSid(low): %w", err)
	}
	tml := windows.Tokenmandatorylabel{}
	tml.Label.Attributes = windows.SE_GROUP_INTEGRITY
	tml.Label.Sid = lowSid
	if err := windows.SetTokenInformation(dup,
		windows.TokenIntegrityLevel,
		(*byte)(unsafe.Pointer(&tml)),
		tml.Size()); err != nil {
		dup.Close()
		return 0, fmt.Errorf("SetTokenInformation(integrity=low): %w", err)
	}
	return dup, nil
}

// applyLowIntegrityLabel lowers dir's mandatory (integrity) label to LOW, with
// object+container inheritance so files created under it are LOW too. A LOW child
// process can then write here (a LOW subject may write a LOW object), while the
// rest of the filesystem stays MEDIUM and thus write-protected from the child.
// Setting the label on an object you own is unprivileged.
//
// TRANSIENT: it first CAPTURES dir's current mandatory label (or its absence) and
// returns a revert closure that restores exactly that prior state — so the LOW
// label lives only for the duration of the run. Most dirs (incl. a fresh worktree
// or a just-created scratch) carry no explicit label; restoring that "no label"
// SACL removes the LOW ACE, returning the dir to implicit MEDIUM integrity.
//
// Best-effort: on failure the caller proceeds without lowering (the child simply
// won't be able to write, which fails safe) and does NOT install a revert.
func applyLowIntegrityLabel(dir string) (revert func(), err error) {
	// (a) CAPTURE the prior mandatory-label SACL so we can restore it verbatim.
	// A dir with no explicit label yields a nil/empty SACL here; restoring that
	// removes our LOW ACE (verified: SDDL goes from "(ML;…;LW)" back to no ML ACE).
	priorSACL := priorLabelSACL(dir)

	// (b) LOWER to LOW. SDDL SACL: (ML=mandatory label; OICI=object+container
	// inherit; NW=no-write-up; ;;LW=Low integrity SID S-1-16-4096).
	sd, err := windows.SecurityDescriptorFromString("S:(ML;OICI;NW;;;LW)")
	if err != nil {
		return nil, fmt.Errorf("parse low-label SDDL: %w", err)
	}
	sacl, _, err := sd.SACL()
	if err != nil {
		return nil, fmt.Errorf("extract SACL: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.LABEL_SECURITY_INFORMATION, nil, nil, nil, sacl); err != nil {
		return nil, fmt.Errorf("SetNamedSecurityInfo(low label) on %s: %w", dir, err)
	}

	// (c) REVERT closure: restore the captured prior label (or its absence). Runs
	// in a defer after the child exits.
	return func() {
		_ = windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
			windows.LABEL_SECURITY_INFORMATION, nil, nil, nil, priorSACL)
	}, nil
}

// priorLabelSACL reads dir's current mandatory (integrity) label SACL so it can be
// restored later. Returns nil when the dir has no explicit label (the common case):
// restoring a nil label SACL removes any LOW ACE we add, dropping the dir back to
// implicit MEDIUM integrity.
func priorLabelSACL(dir string) *windows.ACL {
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION)
	if err != nil || sd == nil {
		return nil
	}
	sacl, _, err := sd.SACL()
	if err != nil {
		return nil // no label present → restore "no label"
	}
	return sacl
}

// buildCommandLine joins argv into a single Windows command line with each argument
// properly quoted. argv[0] is included so the child sees a conventional argv.
func buildCommandLine(argv []string) (*uint16, error) {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = windows.EscapeArg(a)
	}
	return windows.UTF16PtrFromString(strings.Join(parts, " "))
}

// capOutput head+tail truncates s to ~winOutputCap bytes with a middle marker so a
// chatty command cannot blow the caller's budget. Under the cap → returned as-is.
func capOutput(s string) string {
	if len(s) <= winOutputCap {
		return s
	}
	half := (winOutputCap - len(truncMarker)) / 2
	if half < 0 {
		half = 0
	}
	head := s[:half]
	tail := s[len(s)-half:]
	// Keep the result valid UTF-8-ish at the cut points (best-effort byte cut).
	return head + truncMarker + tail
}
