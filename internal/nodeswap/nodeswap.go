// Package nodeswap is the one reusable engine behind every Windows fleet-node
// binary swap. It replaces the family of per-deploy, per-node .ps1 scripts
// (aorus-swap-<sha>.ps1, deploy-node-exe.ps1, fleet-node-restart.ps1 stitched
// together by hand each release) with a single, unit-tested sequence that
// every future deploy calls the same way.
//
// It exists because of a real outage: the 2026-09-24 Aorus 0.140.8 deploy ran
// its restart-and-verify phase over an interactive SSH session. Windows
// OpenSSH kills the whole remote process tree on disconnect (see
// docs/systems/node-swap.md and the windows-ssh-remote-ops-patterns house
// memory), so when the session dropped at the 120s mark the script had
// already swapped the binary but never reached its restart/verify/rollback
// lines — the node sat down for over two hours with nobody watching. This
// package's Run is meant to be launched DETACHED (see setup/windows-node-
// swap-launch.ps1), so an SSH drop mid-run cannot orphan a half-finished swap:
// the process keeps running, keeps writing its log and result file, and still
// reaches its own rollback path if something goes wrong.
//
// Run is a pure sequence over the Deps seam: every OS operation (hashing,
// health reads, process enumeration, renames, tarball extraction, shelling
// out to Start/Stop-ScheduledTask or a restart script) is a function value,
// so the whole state machine — including every rollback branch — is
// unit-testable with fakes, with no real Windows box required.
package nodeswap

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Plan is every input to one node-swap run. It is plain data (no live
// handles), so it can be captured verbatim into the result file and used to
// reconstruct exactly what a run was asked to do.
type Plan struct {
	// Staged is the path to the already-staged new binary. Target is the
	// live binary it replaces.
	Staged string
	Target string

	// ExpectedSHA256 is the hex sha256 the staged binary must hash to before
	// anything is touched. SkipHashCheck exists for tests and dry runs of the
	// plumbing only — production callers always set ExpectedSHA256.
	ExpectedSHA256 string
	SkipHashCheck  bool

	// BackupSuffix names the backup file: "<Target>.bak-<BackupSuffix>".
	// Defaults to a timestamp when empty.
	BackupSuffix string

	// HealthURL is this node's own /fleet/health. Empty means a standalone
	// node with no fleet-serve endpoint (the OptiPlex pattern): the
	// fleet-serve idle-wait and the health half of post-restart verification
	// are both skipped — instead the GPU LEASE is waited on (GPULockPath/
	// GPUStateDir below), since a standalone node has no queue depth to read
	// but can still be mid-render under a caller's own `gpu reserve`. PID +
	// image-hash verification still runs whenever a restart mechanism is
	// configured, regardless of which wait path applied.
	HealthURL        string
	WaitIdleTimeout  time.Duration // default 10m
	IdlePollInterval time.Duration // default 5s

	// GPULockPath / GPUStateDir override the GPU lease directory a STANDALONE
	// node (HealthURL == "") waits to clear before swapping — the check the
	// deploy record for d5207011 notes the operator had to do by hand ("gpu
	// status" before and right before the swap). Empty = the harness's own
	// built-in defaults (internal/gpulease.LeaseDir), the same resolution
	// `local-offload gpu status` uses. Ignored when HealthURL is set (a
	// fleet-serve node's own wait-idle already covers this).
	GPULockPath string
	GPUStateDir string

	// Exactly one of RestartTaskName / RestartCommand should be set for a
	// fleet node; both empty means a standalone binary-only swap (no restart
	// at all — the OptiPlex case named in the task).
	//
	// RestartTaskName: a Windows scheduled task Stop/Start wraps the swap
	// (the Aorus "\offload-fleet-node" pattern).
	//
	// RestartCommand: an arbitrary command that performs its own stop+launch
	// (the Qube's fleet-node-restart.ps1, which launches via WMI/CIM because
	// this box has no scheduled task for fleet-serve). Run does not trust its
	// exit code alone — verifyRunning re-derives PID/hash/health
	// independently after it returns.
	RestartTaskName string
	RestartCommand  string
	RestartTimeout  time.Duration // default 60s, applied to RunCommand for the restart step

	VerifyTimeout      time.Duration // default 90s
	VerifyPollInterval time.Duration // default 2s

	// RenderTarball/RenderDir make the render-tree swap (tarball + backup +
	// extract + hash-verify) an OPTION alongside the binary swap, same as
	// every deploy record's render-tree step.
	RenderTarball      string
	RenderDir          string
	RenderBackupSuffix string

	// ProcessMatch is the substring a fleet-serve command line must contain
	// (default "fleet-serve"). MCPMatch identifies an idle MCP-server helper
	// sharing the same exe (default " mcp") — the Windows-only class of
	// holder documented in the 2026-09-24 OptiPlex deploy: an idle `... mcp`
	// process can hold an OS-level handle on the exe and block the rename
	// even with no active job. Only a process matching MCPMatch and NOT also
	// matching ProcessMatch is ever stopped to clear a rename; any other
	// holder is reported, never touched.
	ProcessMatch string
	MCPMatch     string

	// DryRun validates the hash and (if HealthURL is set) reads health once,
	// then stops — no file or process is touched.
	DryRun bool

	// NoRollback is for tests exercising the failure path itself; production
	// callers never set it.
	NoRollback bool
}

// Outcome is the full, JSON-serializable record of one run — written to the
// --result file so a caller who launched this detached can poll for it over
// a fresh connection after an SSH drop.
type Outcome struct {
	OK               bool         `json:"ok"`
	DryRun           bool         `json:"dry_run"`
	StartedAt        time.Time    `json:"started_at"`
	FinishedAt       time.Time    `json:"finished_at"`
	DurationSec      float64      `json:"duration_sec"`
	Steps            []StepResult `json:"steps"`
	Error            string       `json:"error,omitempty"`
	RolledBack       bool         `json:"rolled_back"`
	RollbackOK       bool         `json:"rollback_ok,omitempty"`
	OldSHA256        string       `json:"old_sha256,omitempty"`
	NewSHA256        string       `json:"new_sha256,omitempty"`
	BackupPath       string       `json:"backup_path,omitempty"`
	RenderSwapped    bool         `json:"render_swapped,omitempty"`
	RenderBackupPath string       `json:"render_backup_path,omitempty"`
	FinalPID         int          `json:"final_pid,omitempty"`
	FinalImageSHA256 string       `json:"final_image_sha256,omitempty"`
	HealthVersion    string       `json:"health_version,omitempty"`
}

// StepResult is one line of the run's audit trail.
type StepResult struct {
	Name   string    `json:"name"`
	OK     bool      `json:"ok"`
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

// ProcessInfo is one CIM/Win32_Process row: enough to identify a holder and
// decide whether it is safe to stop.
type ProcessInfo struct {
	PID            int
	CommandLine    string
	ExecutablePath string
}

// HealthInfo is the subset of GET /fleet/health this package reasons about.
type HealthInfo struct {
	OK          bool
	NodeID      string
	Version     string
	RunningJobs int
	QueuedJobs  int
}

// GPULeaseInfo is the minimal GPU-lease signal a standalone node's swap needs
// before touching the binary — a package-local mirror of gpulease.Info
// (Held/Reason only) so this package and its tests never need the full
// gpulease.Info shape or a real lease directory.
type GPULeaseInfo struct {
	Held   bool
	Reason string
}

// Deps is the OS/network seam. Every field is a plain function so Run is
// fully unit-testable with fakes; DefaultDeps (deps.go, plus the
// platform-suffixed deps_windows.go / deps_other.go files) wires the real
// implementations.
type Deps struct {
	Hash               func(path string) (string, error)
	ReadHealth         func(ctx context.Context, url string) (HealthInfo, error)
	InspectGPULease    func(lockPath, stateDir string) (GPULeaseInfo, error)
	FindProcessesByExe func(exePath string) ([]ProcessInfo, error)
	StopProcess        func(pid int) error
	RenameFile         func(oldPath, newPath string) error
	RemoveAll          func(path string) error
	Exists             func(path string) bool
	MkdirAll           func(path string) error
	RunCommand         func(ctx context.Context, timeout time.Duration, command string) (string, error)
	ExtractTarGz       func(tarGzPath, destDir string) (filesWritten int, err error)
	Sleep              func(d time.Duration)
	Now                func() time.Time
}

// Logger is the minimal --log sink: every step and every rollback action is
// appended here as it happens, so a caller polling mid-run (or after a
// simulated SSH drop) sees real progress, not just the final result file.
type Logger struct {
	w func(line string)
}

// NewLogger wraps a write func (nodeswap_cmd.go supplies one that appends to
// a file and optionally echoes to stderr).
func NewLogger(w func(line string)) *Logger {
	if w == nil {
		w = func(string) {}
	}
	return &Logger{w: w}
}

func (l *Logger) Printf(format string, args ...any) {
	l.w(fmt.Sprintf(format, args...))
}

// backupPathFor builds "<target>.bak-<suffix>". A caller-supplied suffix that
// ITSELF already starts with "bak-" (case-insensitive) is trimmed of that
// prefix first — the exact doubled "bak-bak-" mistake the d5207011 Qube
// deploy record flagged (an operator passed --backup-suffix
// "bak-2026-09-24-pre-d5207011", not knowing this function prepends its own
// "bak-" too). Cosmetic-only either way (the file is still found and
// restored correctly by its exact name), but a suffix that already reads
// "bak-..." should never become "bak-bak-...".
func backupPathFor(target, suffix string) string {
	trimmed := suffix
	if len(trimmed) >= 4 && strings.EqualFold(trimmed[:4], "bak-") {
		trimmed = trimmed[4:]
	}
	return target + ".bak-" + trimmed
}

// Run executes the full swap sequence. It never panics on a bad Plan; every
// failure is returned inside Outcome (OK=false, Error set) so a caller can
// always write the result file, even when Run's own precondition checks
// reject the Plan before touching anything.
func Run(ctx context.Context, plan Plan, deps Deps, log *Logger) Outcome {
	if log == nil {
		log = NewLogger(nil)
	}
	start := deps.Now()
	out := Outcome{DryRun: plan.DryRun, StartedAt: start}

	step := func(name string, ok bool, detail string) {
		out.Steps = append(out.Steps, StepResult{Name: name, OK: ok, Detail: detail, At: deps.Now()})
		log.Printf("[%s] ok=%v %s", name, ok, detail)
	}
	finish := func() Outcome {
		out.FinishedAt = deps.Now()
		out.DurationSec = out.FinishedAt.Sub(start).Seconds()
		return out
	}
	fail := func(name, detail string) Outcome {
		step(name, false, detail)
		out.OK = false
		out.Error = detail
		return finish()
	}
	// mergeAndFail folds a rollback's own StepResults into the SAME out that
	// step()/fail() above close over (never a copy — Outcome is a value
	// type, and returning a locally-merged copy from a helper would discard
	// every step already recorded on out, including the ones rollback
	// itself just appended through this same closure family).
	mergeAndFail := func(r Outcome) Outcome {
		out.Steps = append(out.Steps, r.Steps...)
		out.RolledBack = r.RolledBack
		out.RollbackOK = r.RollbackOK
		if out.Error == "" {
			out.Error = r.Error
		}
		out.OK = false
		return finish()
	}

	if err := validatePlan(plan); err != nil {
		return fail("validate", err.Error())
	}

	// 1. Verify the staged binary's hash before anything else is touched.
	if !plan.SkipHashCheck {
		got, err := deps.Hash(plan.Staged)
		if err != nil {
			return fail("verify-hash", "reading staged binary: "+err.Error())
		}
		if !strings.EqualFold(got, plan.ExpectedSHA256) {
			return fail("verify-hash", fmt.Sprintf("staged sha256 %s does not match expected %s — refusing to swap", got, plan.ExpectedSHA256))
		}
		step("verify-hash", true, got)
	} else {
		step("verify-hash", true, "skipped (SkipHashCheck)")
	}

	if oldHash, err := deps.Hash(plan.Target); err == nil {
		out.OldSHA256 = oldHash
	}

	// 2. Wait for the node to be idle (queue 0, nothing running) before
	// stopping it. A standalone node (no HealthURL) has no queue to read, so
	// it waits on the GPU LEASE instead — it can still be mid-render under a
	// caller's own `gpu reserve` even with no fleet-serve to ask.
	if plan.HealthURL != "" {
		if err := waitIdle(ctx, plan, deps, log); err != nil {
			return fail("wait-idle", err.Error())
		}
		step("wait-idle", true, "queue depth 0")
	} else if deps.InspectGPULease != nil {
		if err := waitGPUFree(ctx, plan, deps, log); err != nil {
			return fail("wait-idle", err.Error())
		}
		step("wait-idle", true, "standalone node (no --health-url); waited for the GPU lease to clear")
	} else {
		step("wait-idle", true, "standalone node (no --health-url); skipped")
	}

	if plan.DryRun {
		step("dry-run", true, "plan validated (hash + idle check only); no file or process was touched")
		out.OK = true
		return finish()
	}

	backupSuffix := plan.BackupSuffix
	if backupSuffix == "" {
		backupSuffix = "swap-" + deps.Now().Format("20060102-150405")
	}
	backupPath := backupPathFor(plan.Target, backupSuffix)

	// 3. Stop whatever is currently serving Target so its file lock clears —
	// necessary on Windows, where a live-image rename fails outright.
	if err := stopForSwap(ctx, plan, deps, log); err != nil {
		return fail("stop-node", err.Error())
	}
	step("stop-node", true, "")

	// 4. Rename the old exe to a backup. On failure, diagnose the holder and
	// — ONLY for an idle MCP helper sharing the exe, never anything else —
	// stop it and retry once.
	//
	// A failure here happens AFTER stop-node (step 3) has already stopped the
	// node, so simply returning would leave a stopped node with nobody ever
	// restarting it — the exact outage class this tool exists to prevent.
	// renameWithRetry either fully succeeds or leaves Target untouched (a
	// Windows same-volume rename is atomic), so recovery is: restart the
	// binary that is still sitting at Target. Passing "" as the backup path
	// tells rollback there is nothing to restore FROM — Target was never
	// moved — so it goes straight to restart+verify.
	if err := renameWithRetry(plan, deps, log, plan.Target, backupPath); err != nil {
		r := rollback(ctx, plan, deps, log, "", "", "backing up the old binary failed: "+err.Error())
		return mergeAndFail(r)
	}
	out.BackupPath = backupPath
	step("backup-old", true, backupPath)

	// 5. Move the new binary into place. A failure here ALSO happens after
	// stop-node, so it routes through the same rollback (which restores
	// backupPath -> Target, then restarts and verifies) instead of the
	// restore-only-no-restart handling this used to have.
	if err := deps.RenameFile(plan.Staged, plan.Target); err != nil {
		r := rollback(ctx, plan, deps, log, backupPath, "", fmt.Sprintf("moving staged binary into place: %v", err))
		return mergeAndFail(r)
	}
	step("install-new", true, plan.Target)

	// 6. Optional render-tree swap: tarball + backup + extract + hash-verify
	// (the file-count check is the caller's job via the extraction result;
	// this package proves extraction succeeded and rolls back cleanly if not).
	var renderBackupPath string
	if plan.RenderTarball != "" {
		bp, err := swapRenderTree(plan, deps, log)
		renderBackupPath = bp
		if err != nil {
			r := rollback(ctx, plan, deps, log, backupPath, renderBackupPath, "render tree swap failed: "+err.Error())
			return mergeAndFail(r)
		}
		out.RenderSwapped = true
		out.RenderBackupPath = renderBackupPath
		step("swap-render", true, plan.RenderDir)
	}

	newHash, err := deps.Hash(plan.Target)
	if err != nil {
		r := rollback(ctx, plan, deps, log, backupPath, renderBackupPath, "hashing the installed binary failed: "+err.Error())
		return mergeAndFail(r)
	}
	out.NewSHA256 = newHash

	// 7. Restart (or, for a standalone node, do nothing — that is a
	// legitimate configuration, not a failure).
	if err := restartNode(ctx, plan, deps, log); err != nil {
		r := rollback(ctx, plan, deps, log, backupPath, renderBackupPath, "restart failed: "+err.Error())
		return mergeAndFail(r)
	}
	step("restart-node", true, "")

	// 8. Prove the swap: a NEW process, its running image's sha256 equal to
	// what was just installed, and (when configured) a healthy /fleet/health.
	pid, imageSHA, healthVersion, err := verifyRunning(ctx, plan, deps, newHash)
	if err != nil {
		r := rollback(ctx, plan, deps, log, backupPath, renderBackupPath, "post-restart verification failed: "+err.Error())
		return mergeAndFail(r)
	}
	out.FinalPID = pid
	out.FinalImageSHA256 = imageSHA
	out.HealthVersion = healthVersion
	step("verify-running", true, fmt.Sprintf("pid=%d sha256=%s version=%q", pid, imageSHA, healthVersion))

	out.OK = true
	return finish()
}

func validatePlan(p Plan) error {
	if p.Staged == "" || p.Target == "" {
		return errors.New("--staged and --target are required")
	}
	if !p.SkipHashCheck && p.ExpectedSHA256 == "" {
		return errors.New("--sha256 is required (or pass --skip-hash-check, testing only)")
	}
	if p.RestartTaskName != "" && p.RestartCommand != "" {
		return errors.New("--restart-task and --restart-command are mutually exclusive")
	}
	if p.RenderTarball != "" && p.RenderDir == "" {
		return errors.New("--render-dir is required with --render-tarball")
	}
	return nil
}

func waitIdle(ctx context.Context, plan Plan, deps Deps, log *Logger) error {
	timeout := plan.WaitIdleTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	interval := plan.IdlePollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := deps.Now().Add(timeout)
	var lastErr error
	var lastRunning, lastQueued int
	for {
		h, err := deps.ReadHealth(ctx, plan.HealthURL)
		if err == nil && h.OK && h.RunningJobs == 0 && h.QueuedJobs == 0 {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastRunning, lastQueued = h.RunningJobs, h.QueuedJobs
		}
		if deps.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("node never went idle within %s: last health read failed: %w", timeout, lastErr)
			}
			return fmt.Errorf("node never went idle within %s: running=%d queued=%d", timeout, lastRunning, lastQueued)
		}
		log.Printf("wait-idle: not yet (running=%d queued=%d err=%v); retrying", lastRunning, lastQueued, lastErr)
		deps.Sleep(interval)
	}
}

// waitGPUFree is waitIdle's counterpart for a STANDALONE node (no fleet-serve
// queue depth to read): it polls the GPU lease instead, refusing to proceed
// while it is held — the check a standalone-node deploy previously left to
// the operator's own judgment ("gpu status" by hand, before and right before
// the swap). Same timeout/interval/logging shape as waitIdle on purpose, so
// the two paths read alike in --log.
func waitGPUFree(ctx context.Context, plan Plan, deps Deps, log *Logger) error {
	if deps.InspectGPULease == nil {
		// No dep wired (an older caller, or a test that only exercises the
		// fleet-serve path): behave exactly as before this field existed —
		// nothing to wait on, standalone swap proceeds unchecked.
		return nil
	}
	timeout := plan.WaitIdleTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	interval := plan.IdlePollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := deps.Now().Add(timeout)
	var lastErr error
	var lastReason string
	for {
		info, err := deps.InspectGPULease(plan.GPULockPath, plan.GPUStateDir)
		if err == nil && !info.Held {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastReason = info.Reason
		}
		if deps.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("GPU lease never cleared within %s: last read failed: %w", timeout, lastErr)
			}
			return fmt.Errorf("GPU lease never cleared within %s: held (%s)", timeout, lastReason)
		}
		log.Printf("wait-gpu-free: still held (%s) err=%v; retrying", lastReason, lastErr)
		deps.Sleep(interval)
	}
}

func procMatch(plan Plan) string {
	if plan.ProcessMatch != "" {
		return plan.ProcessMatch
	}
	return "fleet-serve"
}

func mcpMatch(plan Plan) string {
	if plan.MCPMatch != "" {
		return plan.MCPMatch
	}
	return " mcp"
}

func stopForSwap(ctx context.Context, plan Plan, deps Deps, log *Logger) error {
	if plan.RestartTaskName != "" {
		_, _ = deps.RunCommand(ctx, 15*time.Second, fmt.Sprintf("Stop-ScheduledTask -TaskName %s -ErrorAction SilentlyContinue", psQuote(plan.RestartTaskName)))
	}
	match := procMatch(plan)
	procs, err := deps.FindProcessesByExe(plan.Target)
	if err != nil {
		return fmt.Errorf("enumerating processes on %s: %w", plan.Target, err)
	}
	var stopped []int
	for _, p := range procs {
		if !strings.Contains(p.CommandLine, match) {
			continue
		}
		if err := deps.StopProcess(p.PID); err != nil {
			log.Printf("stop-node: pid %d did not stop cleanly: %v", p.PID, err)
		}
		stopped = append(stopped, p.PID)
	}
	if len(stopped) == 0 {
		log.Printf("stop-node: no running %q process found on %s", match, plan.Target)
		return nil
	}
	deadline := deps.Now().Add(20 * time.Second)
	for {
		still, _ := deps.FindProcessesByExe(plan.Target)
		alive := 0
		for _, p := range still {
			if strings.Contains(p.CommandLine, match) {
				alive++
			}
		}
		if alive == 0 {
			return nil
		}
		if deps.Now().After(deadline) {
			return fmt.Errorf("stopped pid(s) %v but %d matching process(es) still alive after 20s", stopped, alive)
		}
		deps.Sleep(500 * time.Millisecond)
	}
}

// renameWithRetry implements requirement 2's holder-diagnosis rule exactly:
// on a rename failure, identify who holds the file; stop ONLY a process that
// is an idle MCP helper on the same exe (matches MCPMatch, does NOT also
// match ProcessMatch — a live fleet-serve holder is never touched here, it
// was already handled by stopForSwap); anything else is reported and left
// running.
func renameWithRetry(plan Plan, deps Deps, log *Logger, from, to string) error {
	err := deps.RenameFile(from, to)
	if err == nil {
		return nil
	}
	log.Printf("backup-old: rename failed (%v); diagnosing holders of %s", err, from)
	holders, herr := deps.FindProcessesByExe(from)
	if herr != nil {
		return fmt.Errorf("rename failed (%v) and holder diagnosis also failed: %v", err, herr)
	}
	if len(holders) == 0 {
		return fmt.Errorf("rename failed (%v) and no CIM-visible process holds %s — likely an external handle (antivirus, Explorer preview); not stopping anything blind", err, from)
	}
	mm, pm := mcpMatch(plan), procMatch(plan)
	var stopped []int
	var left []string
	for _, h := range holders {
		if strings.Contains(h.CommandLine, mm) && !strings.Contains(h.CommandLine, pm) {
			if serr := deps.StopProcess(h.PID); serr == nil {
				stopped = append(stopped, h.PID)
			} else {
				left = append(left, fmt.Sprintf("pid %d (idle mcp, stop failed: %v)", h.PID, serr))
			}
		} else {
			left = append(left, fmt.Sprintf("pid %d cmdline=%q", h.PID, h.CommandLine))
		}
	}
	if len(stopped) == 0 {
		return fmt.Errorf("rename failed (%v); holder(s) found but none is a stoppable idle mcp process: %s", err, strings.Join(left, "; "))
	}
	log.Printf("backup-old: stopped idle mcp holder(s) %v (left running: %s); retrying rename", stopped, strings.Join(left, "; "))
	deps.Sleep(time.Second)
	if rerr := deps.RenameFile(from, to); rerr != nil {
		return fmt.Errorf("rename still failed after stopping idle holder(s) %v: %v (also holding: %s)", stopped, rerr, strings.Join(left, "; "))
	}
	return nil
}

func swapRenderTree(plan Plan, deps Deps, log *Logger) (backupPath string, err error) {
	suffix := plan.RenderBackupSuffix
	if suffix == "" {
		suffix = "render-" + deps.Now().Format("20060102-150405")
	}
	backupPath = backupPathFor(plan.RenderDir, suffix)
	hadExisting := deps.Exists(plan.RenderDir)
	if hadExisting {
		if err := deps.RenameFile(plan.RenderDir, backupPath); err != nil {
			return "", fmt.Errorf("backing up render dir: %w", err)
		}
	}
	if err := deps.MkdirAll(plan.RenderDir); err != nil {
		if hadExisting {
			_ = deps.RenameFile(backupPath, plan.RenderDir)
		}
		return "", fmt.Errorf("creating render dir: %w", err)
	}
	n, exErr := deps.ExtractTarGz(plan.RenderTarball, plan.RenderDir)
	if exErr != nil || n == 0 {
		_ = deps.RemoveAll(plan.RenderDir)
		if hadExisting {
			if rerr := deps.RenameFile(backupPath, plan.RenderDir); rerr != nil {
				return backupPath, fmt.Errorf("extracting render tarball failed (%v) and restoring the backup also failed: %v", exErr, rerr)
			}
		}
		if exErr != nil {
			return "", fmt.Errorf("extracting render tarball: %w", exErr)
		}
		return "", errors.New("render tarball extracted 0 files")
	}
	log.Printf("swap-render: extracted %d files into %s (backup at %s)", n, plan.RenderDir, backupPath)
	if !hadExisting {
		// Nothing was actually backed up (there was no prior render dir), so
		// there is nothing for a later rollback to restore from — returning
		// the computed-but-never-written path here would make rollback
		// attempt (and fail) a restore from a backup that was never made.
		// RenderDir being freshly created is what rollback's plain RemoveAll
		// undoes correctly on its own.
		return "", nil
	}
	return backupPath, nil
}

func restartNode(ctx context.Context, plan Plan, deps Deps, log *Logger) error {
	timeout := plan.RestartTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	switch {
	case plan.RestartCommand != "":
		out, err := deps.RunCommand(ctx, timeout, plan.RestartCommand)
		log.Printf("restart-node: command output: %s", truncate(out, 4000))
		if err != nil {
			return fmt.Errorf("restart command failed: %w", err)
		}
		return nil
	case plan.RestartTaskName != "":
		out, err := deps.RunCommand(ctx, timeout, fmt.Sprintf("Start-ScheduledTask -TaskName %s", psQuote(plan.RestartTaskName)))
		log.Printf("restart-node: Start-ScheduledTask output: %s", truncate(out, 1000))
		if err != nil {
			return fmt.Errorf("Start-ScheduledTask %s failed: %w", plan.RestartTaskName, err)
		}
		return nil
	default:
		log.Printf("restart-node: no --restart-task/--restart-command given; standalone swap, nothing to restart")
		return nil
	}
}

// verifyRunning proves the swap: a process matching ProcessMatch on Target
// whose own running image hashes to expectedHash, and — whenever a restart
// mechanism or health URL is configured — a healthy /fleet/health. A
// standalone swap with no restart mechanism at all is proven by hash alone
// (there is nothing to restart or poll).
func verifyRunning(ctx context.Context, plan Plan, deps Deps, expectedHash string) (pid int, imageSHA, healthVersion string, err error) {
	if plan.RestartTaskName == "" && plan.RestartCommand == "" {
		h, herr := deps.Hash(plan.Target)
		if herr != nil {
			return 0, "", "", fmt.Errorf("reading installed binary for verification: %w", herr)
		}
		if !strings.EqualFold(h, expectedHash) {
			return 0, "", "", fmt.Errorf("installed binary sha256 %s does not match what was just written (%s)", h, expectedHash)
		}
		return 0, h, "", nil
	}

	timeout := plan.VerifyTimeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	interval := plan.VerifyPollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	deadline := deps.Now().Add(timeout)
	match := procMatch(plan)
	var lastErr error
	for {
		procs, perr := deps.FindProcessesByExe(plan.Target)
		if perr != nil {
			lastErr = perr
		}
		for _, p := range procs {
			if !strings.Contains(p.CommandLine, match) {
				continue
			}
			imgHash, herr := deps.Hash(plan.Target)
			if herr != nil {
				lastErr = herr
				continue
			}
			if !strings.EqualFold(imgHash, expectedHash) {
				lastErr = fmt.Errorf("pid %d image sha256 %s does not match installed %s", p.PID, imgHash, expectedHash)
				continue
			}
			if plan.HealthURL == "" {
				return p.PID, imgHash, "", nil
			}
			h, herr2 := deps.ReadHealth(ctx, plan.HealthURL)
			if herr2 != nil || !h.OK {
				lastErr = fmt.Errorf("health check failed: %v", herr2)
				continue
			}
			return p.PID, imgHash, h.Version, nil
		}
		if deps.Now().After(deadline) {
			if lastErr != nil {
				return 0, "", "", fmt.Errorf("no verified new %q process within %s: %w", match, timeout, lastErr)
			}
			return 0, "", "", fmt.Errorf("no %q process appeared within %s", match, timeout)
		}
		deps.Sleep(interval)
	}
}

// rollback restores the previous binary (and render tree, if it was
// swapped), restarts, and re-verifies — the automatic recovery every failure
// branch above routes through. It never leaves the box worse than it found
// it: even when a step here fails, whatever succeeded (e.g. the binary
// restore) stays applied rather than being undone again.
func rollback(ctx context.Context, plan Plan, deps Deps, log *Logger, backupPath, renderBackupPath, reason string) Outcome {
	out := Outcome{Error: reason}
	step := func(name string, ok bool, detail string) {
		out.Steps = append(out.Steps, StepResult{Name: name, OK: ok, Detail: detail, At: deps.Now()})
		log.Printf("[rollback:%s] ok=%v %s", name, ok, detail)
	}
	log.Printf("ROLLING BACK: %s", reason)
	if plan.NoRollback {
		step("rollback", false, "disabled by --no-rollback (testing only); box left as-is: "+reason)
		out.RolledBack = false
		return out
	}

	// Stop whatever is currently running (the bad new binary, if it managed
	// to start at all) before touching files again.
	_ = stopForSwap(ctx, plan, deps, log)

	if renderBackupPath != "" {
		_ = deps.RemoveAll(plan.RenderDir)
		if err := deps.RenameFile(renderBackupPath, plan.RenderDir); err != nil {
			step("restore-render", false, err.Error())
		} else {
			step("restore-render", true, plan.RenderDir)
		}
	}

	// backupPath is "" (or does not exist on disk) when the failure that
	// triggered this rollback happened BEFORE anything was ever moved (the
	// backup-old step itself failing — see Run's step 4) — Target still
	// holds the original binary untouched, so there is nothing to restore
	// FROM, and restoring would only fail on a missing source file. Only a
	// genuinely broken state (no backup AND Target missing) is reported as
	// a real restore failure.
	switch {
	case backupPath != "" && deps.Exists(backupPath):
		if err := deps.RemoveAll(plan.Target); err != nil {
			log.Printf("rollback: clearing %s before restore: %v (continuing — RenameFile below will surface a real failure if this matters)", plan.Target, err)
		}
		if err := deps.RenameFile(backupPath, plan.Target); err != nil {
			step("restore-backup", false, err.Error())
			out.RolledBack = false
			out.RollbackOK = false
			return out
		}
		step("restore-backup", true, plan.Target)
	case deps.Exists(plan.Target):
		step("restore-backup", true, "no backup to restore — "+plan.Target+" was never moved")
	default:
		step("restore-backup", false, fmt.Sprintf("no backup at %q and %q is missing — cannot recover the binary", backupPath, plan.Target))
		out.RolledBack = false
		out.RollbackOK = false
		return out
	}
	out.RolledBack = true

	if err := restartNode(ctx, plan, deps, log); err != nil {
		step("restart-after-rollback", false, err.Error())
		out.RollbackOK = false
		return out
	}
	step("restart-after-rollback", true, "")

	oldHash, _ := deps.Hash(plan.Target)
	if _, _, _, err := verifyRunning(ctx, plan, deps, oldHash); err != nil {
		step("verify-after-rollback", false, err.Error())
		out.RollbackOK = false
		return out
	}
	step("verify-after-rollback", true, "old binary confirmed running again")
	out.RollbackOK = true
	return out
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// psQuote wraps a value in single quotes for a PowerShell command string,
// escaping embedded single quotes by doubling them (PowerShell's own
// escape). Task/host names here always come from Plan fields the caller
// controls, never untrusted input, but this keeps the generated command
// well-formed regardless.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
