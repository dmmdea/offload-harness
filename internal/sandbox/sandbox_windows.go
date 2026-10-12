//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/dmmdea/offload-harness/internal/proctree"
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
// A label step that fails does not stop the run, and it is never silent either. The caller asked
// for a writable worktree (or scratch) and a restored label; when the cage cannot give either it
// says so ahead of the command's stderr, in the error of a run that fails, and in the log (see Run
// and runNotes), because the alternative is a child's unexplained "Access is denied".
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

// The Windows half of the cage's environment contract (sandbox.go).
//
// envPassthrough is the ONLY set copied out of the harness's environment, and it
// is closed: a Windows process cannot start without SystemRoot (the loader and
// every CRT read it) or find a program without PATH/PATHEXT, and none of those
// values can be synthesised the way Linux names its own PATH. Nothing else is
// copied — GITHUB_TOKEN, MEM0_API_KEY, the fleet token, AWS_*, every credential
// a shell exported into this process stays on this side of the cage.
//
// Deliberately NOT here: APPDATA / LOCALAPPDATA / USERNAME / USERDOMAIN. They
// point at (or name) the operator's real profile, which is precisely what
// envHomeNames redirects into the run's scratch dir.
var (
	envFixed       []string // Windows cannot name its own SystemRoot or PATH; see envPassthrough
	envPassthrough = []string{
		"SystemRoot",             // image loader, CRT, winsock: a process may not start without it
		"SystemDrive",            // resolved by many installers/scripts before SystemRoot
		"windir",                 // the legacy spelling; some tooling reads only this one
		"PATH",                   // finding a program and its DLLs
		"PATHEXT",                // cmd.exe needs it to resolve a bare command name
		"COMSPEC",                // cmd.exe's own path, for a nested shell
		"NUMBER_OF_PROCESSORS",   // build tools size their parallelism from it
		"PROCESSOR_ARCHITECTURE", // toolchains branch on it
		"OS",                     // "Windows_NT"; scripts branch on it
	}
	envHomeNames = []string{"USERPROFILE", "HOME"}
	envTempNames = []string{"TEMP", "TMP", "TMPDIR"}
)

// envBlock renders an environment as a CreateProcess lpEnvironment block: a
// sequence of NUL-terminated UTF-16 "K=V" strings closed by one extra NUL.
//
// Sorted case-insensitively by name because CreateProcess documents a sorted
// block, and an EMPTY environment still renders as two NULs — never as nil,
// which is the value that means "inherit the parent's block" and is the whole
// defect this replaces.
func envBlock(env []string) (*uint16, error) {
	sorted := append([]string(nil), env...)
	sort.Slice(sorted, func(i, j int) bool {
		return strings.ToUpper(sorted[i]) < strings.ToUpper(sorted[j])
	})
	block := make([]uint16, 0, 64)
	for _, e := range sorted {
		u, err := windows.UTF16FromString(e) // rejects an embedded NUL, and NUL-terminates
		if err != nil {
			return nil, fmt.Errorf("environment entry %q: %w", e, err)
		}
		block = append(block, u...)
	}
	block = append(block, 0) // the block's own terminator (two NULs when env is empty)
	if len(block) == 1 {
		block = append([]uint16{0}, block...)
	}
	return &block[0], nil
}

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
//
// What the cage could not do for the run is told to the caller, not swallowed: a folder whose
// low-integrity label could not be applied (so the command cannot write it), one whose label could
// not be restored afterwards (so other low-integrity processes can write it), one whose prior label
// could not be read (so the restore can only clear it), a command whose exit status could not be
// read (its ExitCode is -1, never the 0 that every caller reads as success). Each is a line starting
// with "[sandbox] " ahead of the command's own stderr in Result.Stderr, is appended to the error of a
// run that fails, and is logged once. None of them changes whether the command runs.
func Run(ctx context.Context, spec Spec) (Result, error) {
	var notes runNotes
	res, err := run(ctx, spec, &notes)
	if len(notes) == 0 {
		return res, err
	}
	for _, n := range notes {
		log.Printf("sandbox: %s", n)
	}
	res.Stderr = noteLines(notes, "\r\n") + res.Stderr
	if err != nil {
		err = fmt.Errorf("%w (%s)", err, strings.Join(notes, "; "))
	}
	return res, err
}

// run is Run without the reporting. It records what the caller must hear in notes; Run delivers
// them once run has returned, which is after the deferred label restores have run and had their
// say.
func run(ctx context.Context, spec Spec, notes *runNotes) (Result, error) {
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
	// concern. A hard crash between lower and revert could leave a dir LOW, and that does NOT
	// heal itself: the next run reads the leftover LOW label as the folder's own and restores it
	// as it finds it. This is why a restore that fails is reported to the caller (below) instead
	// of being left for a later run to put right.
	//
	// Neither step can stop the run, and neither is silent: a label that cannot be lowered leaves
	// a folder the command cannot write (it runs at low integrity), a label that cannot be restored
	// leaves a folder every low-integrity process can write. Both go into notes.
	//
	// lowerFolder lowers dir's label and returns the func that puts it back (to be deferred), or
	// nil when the label was not lowered. role names the folder for the caller.
	lowerFolder := func(role, dir string) (restore func()) {
		revert, err := applyLowIntegrityLabel(dir, notes)
		if err != nil {
			notes.addf("the %s %s could not be made writable for this run: its low-integrity label was not applied (%v); "+
				"the command runs at low integrity, so its writes there are expected to be refused", role, dir, err)
			return nil
		}
		return func() {
			if err := revert(); err != nil {
				notes.addf("the low-integrity label on the %s %s was not restored after the run (%v); "+
					"until it is removed, every low-integrity process can write there, and the next run will take that label for the folder's own",
					role, dir, err)
			}
		}
	}
	if spec.WorktreeWritable {
		if restore := lowerFolder("worktree", spec.Worktree); restore != nil {
			defer restore()
		}
	}
	if spec.Scratch != "" {
		if restore := lowerFolder("scratch folder", spec.Scratch); restore != nil {
			defer restore()
		}
	}

	// (2) JOB OBJECT with kill-on-close + active-process + memory limits. The job object
	// itself is the shared primitive (internal/proctree), the same one a lease wrapper puts
	// its command in.
	job, err := proctree.NewJob(proctree.JobLimits{
		KillOnClose:        true,
		ActiveProcessLimit: winActiveProcessLimit,
		ProcessMemoryLimit: uintptr(winProcessMemoryLimit),
	})
	if err != nil {
		return Result{}, err
	}
	// kill-on-job-close reaps strays even on an unexpected return path.
	defer job.Close()

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

	// The child's environment is BUILT, never inherited. A nil lpEnvironment means
	// "give the child the caller's block", and the caller here is the harness: its
	// environment carries GITHUB_TOKEN, MEM0_API_KEY, the fleet auth token and
	// whatever else a shell exported. The Linux cage has always exec'd with a
	// hand-built env (sandbox_linux.go) and this side simply did not, so an
	// untrusted Windows command was handed every credential the delegator held.
	// Reads are less contained on Windows than under Landlock, but that was never
	// a reason to HAND the child the secrets — cageEnv is the shared allowlist.
	envp, err := envBlock(cageEnv(spec, os.Environ()))
	if err != nil {
		windows.CloseHandle(stdoutW)
		windows.CloseHandle(stderrW)
		return Result{}, err
	}

	var pi windows.ProcessInformation

	// (4) SPAWN SUSPENDED as the low-integrity user.
	err = windows.CreateProcessAsUser(
		windows.Token(lowTok),
		appName,
		cmdLine,
		nil,  // process security
		nil,  // thread security
		true, // inherit handles (the pipe write ends)
		// CREATE_UNICODE_ENVIRONMENT is not optional with a UTF-16 block: without
		// it CreateProcess reads the same bytes as ANSI and the child's environment
		// comes out as one truncated entry.
		windows.CREATE_SUSPENDED|windows.CREATE_UNICODE_ENVIRONMENT,
		envp, // the explicit allowlist block (cageEnv) — NEVER nil, which means "inherit"
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
	if err := job.Assign(pi.Process); err != nil {
		windows.TerminateProcess(pi.Process, 1)
		return Result{}, err
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
	var killErr error
	select {
	case <-ctx.Done():
		timedOut = true
		// A kill that fails is not a kill: the wait below then lasts as long as the command
		// chooses, the deadline the caller set is not in force, and the caller is told so.
		if killErr = terminateJob(job, 1); killErr != nil {
			notes.addf("the command outlived its time limit (%v): the cage could not end its job (%v) and waited for the command to finish by itself",
				ctx.Err(), killErr)
		}
		<-done // the process is now dead (or, if the kill failed, ends by itself); wait for the WaitForSingleObject goroutine
	case <-done:
	}
	wg.Wait()

	// How the command ended. A read that fails leaves a command that has run, its output in hand, and
	// an outcome the cage does not know. ExitCode left at its zero value would report that as success,
	// which is what every caller takes 0 to mean, so it is noExitStatus and the caller is told why.
	var exitCode uint32
	if err := readExitCode(pi.Process, &exitCode); err != nil {
		res.ExitCode = noExitStatus
		notes.addf("the command's exit status could not be read (GetExitCodeProcess: %v): its exit code is reported as %d, not 0, "+
			"because a run whose outcome is unknown is not a success", err, noExitStatus)
	} else {
		res.ExitCode = int(exitCode)
	}
	res.Stdout = capOutput(string(stdout))
	res.Stderr = capOutput(string(stderr))
	if timedOut && killErr == nil { // a failed kill has its own note; "killed" would be a claim the cage cannot make
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

// The Win32 calls whose failure the cage has to report, held in variables so a test can make
// them fail (or succeed) on demand instead of depending on the ACLs and the state of the machine
// that runs it. Production never reassigns them; tests that do must not run in parallel.
var (
	// setLabelSACL writes dir's mandatory label: sacl is the label to set, nil removes it.
	setLabelSACL = func(dir string, sacl *windows.ACL) error {
		return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
			windows.LABEL_SECURITY_INFORMATION, nil, nil, nil, sacl)
	}
	// getLabelSD reads dir's security descriptor with its mandatory label.
	getLabelSD = func(dir string) (*windows.SECURITY_DESCRIPTOR, error) {
		return windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION)
	}
	// terminateJob ends every process of the job; the deadline a caller sets depends on it.
	terminateJob = func(j *proctree.Job, exitCode uint32) error { return j.Terminate(exitCode) }
	// readExitCode reads a finished process's exit status; the ExitCode of every Result depends on it.
	readExitCode = windows.GetExitCodeProcess
)

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
// won't be able to write, which fails safe) and does NOT install a revert. The
// failure is returned, and Run turns it into a note for ITS caller (it is the
// caller that asked for a writable folder). The revert closure returns the error
// of the restore for the same reason: a restore that fails leaves dir LOW.
//
// One caveat is recorded here, in notes, because only this function knows it: when
// the prior label could not be READ, the restore can only clear the label (a read
// error is not a dir without a label, and the old code took it for one).
func applyLowIntegrityLabel(dir string, notes *runNotes) (revert func() error, err error) {
	// (a) CAPTURE the prior mandatory-label SACL so we can restore it verbatim.
	// A dir with no explicit label yields a nil/empty SACL here; restoring that
	// removes our LOW ACE (verified: SDDL goes from "(ML;…;LW)" back to no ML ACE).
	priorSACL, priorErr := priorLabelSACL(dir)

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
	if err := setLabelSACL(dir, sacl); err != nil {
		return nil, fmt.Errorf("SetNamedSecurityInfo(low label): %w", err)
	}
	if priorErr != nil {
		// Only now: a label that was never lowered is never restored, so there is nothing to say
		// about how the restore will go.
		notes.addf("the prior label of %s could not be read (%v); after the run its low-integrity label is removed, "+
			"not restored to what it was", dir, priorErr)
	}

	// (c) REVERT closure: restore the captured prior label (or its absence). Runs
	// in a defer after the child exits. Its error is the caller's to report.
	return func() error {
		return setLabelSACL(dir, priorSACL)
	}, nil
}

// priorLabelSACL reads dir's current mandatory (integrity) label SACL so it can be
// restored later. Returns (nil, nil) when the dir has no explicit label (the common
// case): restoring a nil label SACL removes any LOW ACE we add, dropping the dir
// back to implicit MEDIUM integrity. A label that cannot be READ is not "no label",
// so that is an error, and the (nil) SACL returned with it is what a restore will
// write.
func priorLabelSACL(dir string) (*windows.ACL, error) {
	sd, err := getLabelSD(dir)
	if err != nil {
		return nil, err
	}
	if sd == nil {
		return nil, nil
	}
	sacl, _, err := sd.SACL()
	if err != nil {
		return nil, nil // no label present (x/sys reports an absent SACL as an error) → restore "no label"
	}
	return sacl, nil
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
