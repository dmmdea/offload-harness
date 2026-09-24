package nodeswap

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeState is the tiny in-memory world a fake Deps operates on: which path
// holds which "binary" (identified by a hash string), which processes are
// "running", and how health answers. It lets every branch of Run — happy
// path, hash mismatch, busy node, rename failure with an idle mcp holder,
// rename failure with an unstoppable holder, restart failure + rollback,
// verify failure + rollback, rollback-itself-fails — be driven deterministically
// with no real filesystem or process.
type fakeState struct {
	files map[string]string // path -> content hash string
	procs []ProcessInfo
	// health is called on each ReadHealth; nil = not configured (test doesn't set HealthURL)
	health func() (HealthInfo, error)
	// gpuHeld is called on each InspectGPULease; nil leaves Deps.InspectGPULease
	// nil too (byte-identical to every test written before it existed — only a
	// test that explicitly sets this one exercises the standalone GPU-lease wait).
	gpuHeld func() (GPULeaseInfo, error)

	restartCalled   int
	restartFail     bool
	stopFail        map[int]bool // pid -> StopProcess should fail
	renameFailOnce  map[string]bool
	clock           time.Time
	launchOnRestart func()
}

// launchOnRestartFn registers a callback fired every time RunCommand runs a
// Start-ScheduledTask (i.e. every restart attempt, including rollback's own
// restart) — tests use it to simulate the task actually launching a fresh
// fleet-serve process.
func (s *fakeState) launchOnRestartFn(fn func()) { s.launchOnRestart = fn }

func newFakeState() *fakeState {
	return &fakeState{
		files:          map[string]string{},
		stopFail:       map[int]bool{},
		renameFailOnce: map[string]bool{},
		clock:          time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func (s *fakeState) deps() Deps {
	d := Deps{
		Hash: func(path string) (string, error) {
			h, ok := s.files[path]
			if !ok {
				return "", errors.New("no such file: " + path)
			}
			return h, nil
		},
		ReadHealth: func(ctx context.Context, url string) (HealthInfo, error) {
			if s.health == nil {
				return HealthInfo{}, errors.New("health not configured")
			}
			return s.health()
		},
		FindProcessesByExe: func(exePath string) ([]ProcessInfo, error) {
			var out []ProcessInfo
			for _, p := range s.procs {
				if p.ExecutablePath == exePath {
					out = append(out, p)
				}
			}
			return out, nil
		},
		StopProcess: func(pid int) error {
			if s.stopFail[pid] {
				return errors.New("access denied")
			}
			var kept []ProcessInfo
			for _, p := range s.procs {
				if p.PID != pid {
					kept = append(kept, p)
				}
			}
			s.procs = kept
			return nil
		},
		RenameFile: func(oldPath, newPath string) error {
			if s.renameFailOnce[oldPath] {
				delete(s.renameFailOnce, oldPath)
				return errors.New("Access is denied")
			}
			h, ok := s.files[oldPath]
			if !ok {
				return errors.New("rename: source missing: " + oldPath)
			}
			delete(s.files, oldPath)
			s.files[newPath] = h
			return nil
		},
		RemoveAll: func(path string) error {
			delete(s.files, path)
			return nil
		},
		Exists: func(path string) bool {
			_, ok := s.files[path]
			return ok
		},
		MkdirAll: func(path string) error { return nil },
		RunCommand: func(ctx context.Context, timeout time.Duration, command string) (string, error) {
			if strings.Contains(command, "Start-ScheduledTask") {
				s.restartCalled++
				if s.restartFail {
					return "boom", errors.New("task failed to start")
				}
				// Simulate the task launching a fresh process with the
				// current image at Target — tests set this up via
				// launchOnRestart.
				if s.launchOnRestart != nil {
					s.launchOnRestart()
				}
				return "ok", nil
			}
			return "", nil
		},
		ExtractTarGz: func(tarGzPath, destDir string) (int, error) {
			return 1, nil
		},
		Sleep: func(time.Duration) {},
		Now: func() time.Time {
			s.clock = s.clock.Add(time.Second)
			return s.clock
		},
	}
	if s.gpuHeld != nil {
		d.InspectGPULease = func(lockPath, stateDir string) (GPULeaseInfo, error) { return s.gpuHeld() }
	}
	return d
}

func TestRun_HappyPath(t *testing.T) {
	s := newFakeState()
	s.files["staged.exe"] = "NEWHASH"
	s.files["target.exe"] = "OLDHASH"
	s.procs = []ProcessInfo{{PID: 100, CommandLine: "target.exe fleet-serve", ExecutablePath: "target.exe"}}
	s.health = func() (HealthInfo, error) { return HealthInfo{OK: true}, nil }
	nextPID := 200
	s.launchOnRestartFn(func() {
		s.procs = append(s.procs, ProcessInfo{PID: nextPID, CommandLine: "target.exe fleet-serve", ExecutablePath: "target.exe"})
	})

	plan := Plan{
		Staged:          "staged.exe",
		Target:          "target.exe",
		ExpectedSHA256:  "NEWHASH",
		HealthURL:       "http://node/fleet/health",
		RestartTaskName: "offload-fleet-node",
		BackupSuffix:    "test",
	}
	out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
	if !out.OK {
		t.Fatalf("expected OK, got error=%q steps=%+v", out.Error, out.Steps)
	}
	if out.FinalPID != nextPID {
		t.Errorf("FinalPID = %d, want %d", out.FinalPID, nextPID)
	}
	if out.NewSHA256 != "NEWHASH" {
		t.Errorf("NewSHA256 = %q, want NEWHASH", out.NewSHA256)
	}
	if out.OldSHA256 != "OLDHASH" {
		t.Errorf("OldSHA256 = %q, want OLDHASH", out.OldSHA256)
	}
	if s.files[out.BackupPath] != "OLDHASH" {
		t.Errorf("backup at %s = %q, want OLDHASH", out.BackupPath, s.files[out.BackupPath])
	}
	if s.files["target.exe"] != "NEWHASH" {
		t.Errorf("target.exe = %q, want NEWHASH", s.files["target.exe"])
	}
}

func TestRun_HashMismatchRefusesToSwap(t *testing.T) {
	s := newFakeState()
	s.files["staged.exe"] = "ACTUAL"
	s.files["target.exe"] = "OLDHASH"
	plan := Plan{Staged: "staged.exe", Target: "target.exe", ExpectedSHA256: "EXPECTED"}
	out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
	if out.OK {
		t.Fatal("expected failure on hash mismatch")
	}
	if !strings.Contains(out.Error, "does not match expected") {
		t.Errorf("Error = %q, want a hash-mismatch message", out.Error)
	}
	// Nothing should have been touched.
	if s.files["target.exe"] != "OLDHASH" {
		t.Errorf("target.exe was touched despite the hash mismatch: %q", s.files["target.exe"])
	}
}

func TestRun_DryRunTouchesNothing(t *testing.T) {
	s := newFakeState()
	s.files["staged.exe"] = "NEWHASH"
	s.files["target.exe"] = "OLDHASH"
	s.health = func() (HealthInfo, error) { return HealthInfo{OK: true}, nil }
	plan := Plan{
		Staged: "staged.exe", Target: "target.exe", ExpectedSHA256: "NEWHASH",
		HealthURL: "http://node/fleet/health", DryRun: true,
	}
	out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
	if !out.OK {
		t.Fatalf("dry run should succeed, got error=%q", out.Error)
	}
	if !out.DryRun {
		t.Error("DryRun flag not carried into Outcome")
	}
	if s.files["target.exe"] != "OLDHASH" || s.files["staged.exe"] != "NEWHASH" {
		t.Errorf("dry run touched files: target=%q staged=%q", s.files["target.exe"], s.files["staged.exe"])
	}
}

func TestRun_WaitsForIdleBeforeSwapping(t *testing.T) {
	s := newFakeState()
	s.files["staged.exe"] = "NEWHASH"
	s.files["target.exe"] = "OLDHASH"
	calls := 0
	s.health = func() (HealthInfo, error) {
		calls++
		if calls < 3 {
			return HealthInfo{OK: true, RunningJobs: 1}, nil
		}
		return HealthInfo{OK: true}, nil
	}
	plan := Plan{
		Staged: "staged.exe", Target: "target.exe", ExpectedSHA256: "NEWHASH",
		HealthURL: "http://node/fleet/health", DryRun: true,
		IdlePollInterval: time.Millisecond, WaitIdleTimeout: time.Minute,
	}
	out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
	if !out.OK {
		t.Fatalf("expected eventual idle, got error=%q", out.Error)
	}
	if calls < 3 {
		t.Errorf("expected at least 3 health polls, got %d", calls)
	}
}

func TestRun_NeverIdleTimesOut(t *testing.T) {
	s := newFakeState()
	s.files["staged.exe"] = "NEWHASH"
	s.files["target.exe"] = "OLDHASH"
	s.health = func() (HealthInfo, error) { return HealthInfo{OK: true, RunningJobs: 1}, nil }
	plan := Plan{
		Staged: "staged.exe", Target: "target.exe", ExpectedSHA256: "NEWHASH",
		HealthURL:       "http://node/fleet/health",
		WaitIdleTimeout: 3 * time.Second, IdlePollInterval: time.Second,
	}
	out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
	if out.OK {
		t.Fatal("expected timeout failure when the node never idles")
	}
	if !strings.Contains(out.Error, "never went idle") {
		t.Errorf("Error = %q, want an idle-timeout message", out.Error)
	}
	if s.files["target.exe"] != "OLDHASH" {
		t.Error("target.exe was touched despite never reaching idle")
	}
}

func TestRun_RenameFailureStopsIdleMCPHolderAndRetries(t *testing.T) {
	s := newFakeState()
	s.files["staged.exe"] = "NEWHASH"
	s.files["target.exe"] = "OLDHASH"
	// The fleet-serve process itself is already stopped by stopForSwap
	// (no ProcessMatch entry left), but an idle `mcp` helper on the same
	// exe still holds an OS handle and blocks the rename once.
	s.procs = []ProcessInfo{{PID: 300, CommandLine: "target.exe mcp", ExecutablePath: "target.exe"}}
	s.renameFailOnce["target.exe"] = true

	plan := Plan{Staged: "staged.exe", Target: "target.exe", ExpectedSHA256: "NEWHASH", BackupSuffix: "t"}
	out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
	if !out.OK {
		t.Fatalf("expected the idle mcp holder to be stopped and the rename retried, got error=%q steps=%+v", out.Error, out.Steps)
	}
	for _, p := range s.procs {
		if p.PID == 300 {
			t.Error("the idle mcp holder (pid 300) should have been stopped")
		}
	}
}

// TestRenameWithRetry_LeavesALiveFleetServeHolderAlone unit-tests
// renameWithRetry directly (rather than through Run/stopForSwap, which would
// already have stopped a fleet-serve match beforehand in the real sequence)
// to pin the holder-diagnosis rule itself: a process matching ProcessMatch
// is a live server, never a stoppable idle helper, and renameWithRetry must
// report it and leave it running rather than guess.
func TestRenameWithRetry_LeavesALiveFleetServeHolderAlone(t *testing.T) {
	s := newFakeState()
	s.files["target.exe"] = "OLDHASH"
	s.procs = []ProcessInfo{{PID: 400, CommandLine: "target.exe fleet-serve", ExecutablePath: "target.exe"}}
	s.renameFailOnce["target.exe"] = true

	plan := Plan{Target: "target.exe"}
	err := renameWithRetry(plan, s.deps(), NewLogger(nil), "target.exe", "target.exe.bak")
	if err == nil {
		t.Fatal("expected failure: the only holder is a live fleet-serve process, which must never be stopped by the rename-retry path")
	}
	if !strings.Contains(err.Error(), "none is a stoppable idle mcp process") {
		t.Errorf("err = %q, want it to explain the holder was left alone", err)
	}
	found := false
	for _, p := range s.procs {
		if p.PID == 400 {
			found = true
		}
	}
	if !found {
		t.Error("pid 400 (a live fleet-serve holder) must not have been stopped")
	}
}

// TestRenameWithRetry_StopsOnlyTheIdleMCPHolder covers the companion case in
// the same direct-unit style: a holder that matches MCPMatch and does NOT
// also match ProcessMatch is the one class renameWithRetry may stop.
func TestRenameWithRetry_StopsOnlyTheIdleMCPHolder(t *testing.T) {
	s := newFakeState()
	s.files["target.exe"] = "OLDHASH"
	s.procs = []ProcessInfo{
		{PID: 300, CommandLine: "target.exe mcp", ExecutablePath: "target.exe"},
		{PID: 400, CommandLine: "target.exe fleet-serve", ExecutablePath: "target.exe"},
	}
	s.renameFailOnce["target.exe"] = true

	plan := Plan{Target: "target.exe"}
	err := renameWithRetry(plan, s.deps(), NewLogger(nil), "target.exe", "target.exe.bak")
	// The fake's rename only fails on the FIRST attempt (renameFailOnce), so
	// the retry succeeds once pid 300 is cleared — this test's point is WHICH
	// pid got stopped to get there, never whether the retry itself succeeds.
	if err != nil {
		t.Fatalf("retry after stopping the idle mcp holder should have succeeded: %v", err)
	}
	stillThere := map[int]bool{}
	for _, p := range s.procs {
		stillThere[p.PID] = true
	}
	if stillThere[300] {
		t.Error("pid 300 (idle mcp holder) should have been stopped")
	}
	if !stillThere[400] {
		t.Error("pid 400 (live fleet-serve holder) must not have been stopped")
	}
}

// TestRun_BackupOldFailureRestartsOldBinary pins the fix for the blocking
// review finding on PR #476: a failure at the backup-old step (step 4)
// happens AFTER stop-node (step 3) has already stopped the node. Before the
// fix, this returned straight to the caller with the node left DOWN and
// nobody ever restarting it — the exact outage class this tool exists to
// prevent. renameWithRetry never moves anything on failure (Target still
// holds the original binary untouched), so recovery is a restart+verify
// with no file restore needed.
func TestRun_BackupOldFailureRestartsOldBinary(t *testing.T) {
	s := newFakeState()
	s.files["staged.exe"] = "NEWHASH"
	s.files["target.exe"] = "OLDHASH"
	s.health = func() (HealthInfo, error) { return HealthInfo{OK: true}, nil }
	// No holders at all: renameWithRetry fails immediately with "no
	// CIM-visible process holds ..." and never touches target.exe.
	s.renameFailOnce["target.exe"] = true
	newPID := 900
	s.launchOnRestartFn(func() {
		s.procs = append(s.procs, ProcessInfo{PID: newPID, CommandLine: "target.exe fleet-serve", ExecutablePath: "target.exe"})
	})

	plan := Plan{
		Staged: "staged.exe", Target: "target.exe", ExpectedSHA256: "NEWHASH",
		HealthURL: "http://node/fleet/health", RestartTaskName: "offload-fleet-node",
		BackupSuffix: "t", RestartTimeout: time.Second,
	}
	out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
	if out.OK {
		t.Fatal("expected failure: backup-old itself failed")
	}
	if !out.RolledBack || !out.RollbackOK {
		t.Fatalf("expected a successful recovery restart, got RolledBack=%v RollbackOK=%v error=%q steps=%+v", out.RolledBack, out.RollbackOK, out.Error, out.Steps)
	}
	if s.files["target.exe"] != "OLDHASH" {
		t.Errorf("target.exe = %q, want OLDHASH (it was never moved by the failed backup)", s.files["target.exe"])
	}
	if s.files["staged.exe"] != "NEWHASH" {
		t.Errorf("staged.exe should be untouched (install-new never ran), got %q", s.files["staged.exe"])
	}
	if s.restartCalled == 0 {
		t.Error("expected the node to be restarted after the failed backup — this is the bug the fix closes: the node must never be left down")
	}
	found := false
	for _, st := range out.Steps {
		if st.Name == "restore-backup" && st.OK {
			found = true
		}
	}
	if !found {
		t.Error("expected a passing restore-backup step noting there was nothing to restore")
	}
}

// TestRun_InstallNewFailureRestartsOldBinary is the install-new (step 5)
// counterpart: before the fix, a failure moving the staged binary into
// place restored the backup FILE inline but never restarted the node,
// leaving it down with the correct binary back in place but no running
// process. It must now restart and re-verify like every other failure from
// step 4 onward.
func TestRun_InstallNewFailureRestartsOldBinary(t *testing.T) {
	s := newFakeState()
	s.files["target.exe"] = "OLDHASH"
	// staged.exe deliberately absent: the RenameFile(staged, target) call
	// fails because the source does not exist, exactly like a staged file
	// that vanished or was already consumed.
	s.health = func() (HealthInfo, error) { return HealthInfo{OK: true}, nil }
	newPID := 901
	s.launchOnRestartFn(func() {
		s.procs = append(s.procs, ProcessInfo{PID: newPID, CommandLine: "target.exe fleet-serve", ExecutablePath: "target.exe"})
	})

	plan := Plan{
		Staged: "staged.exe", Target: "target.exe", ExpectedSHA256: "OLDHASH", SkipHashCheck: true,
		HealthURL: "http://node/fleet/health", RestartTaskName: "offload-fleet-node",
		BackupSuffix: "t", RestartTimeout: time.Second,
	}
	out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
	if out.OK {
		t.Fatal("expected failure: install-new itself failed (staged.exe missing)")
	}
	if !out.RolledBack || !out.RollbackOK {
		t.Fatalf("expected a successful recovery restart, got RolledBack=%v RollbackOK=%v error=%q steps=%+v", out.RolledBack, out.RollbackOK, out.Error, out.Steps)
	}
	if s.files["target.exe"] != "OLDHASH" {
		t.Errorf("target.exe = %q, want OLDHASH restored from the backup", s.files["target.exe"])
	}
	if s.restartCalled == 0 {
		t.Error("expected the node to be restarted after the failed install — the bug this fix closes left it down with the file restored but nothing running")
	}
}

// TestRun_RollbackRestoreItselfFails: when the rollback's OWN restore
// (backupPath -> Target) fails, Outcome must surface RolledBack=false /
// RollbackOK=false rather than silently reporting a clean recovery — the
// caller (a human or the next deploy) needs to know the box may be in a
// broken state, not just that the swap itself failed.
func TestRun_RollbackRestoreItselfFails(t *testing.T) {
	s := newFakeState()
	s.files["staged.exe"] = "NEWHASH"
	s.files["target.exe"] = "OLDHASH"
	s.health = func() (HealthInfo, error) { return HealthInfo{OK: true}, nil }
	s.restartFail = true // forces the MAIN restart-node step to fail, triggering rollback
	backupPath := "target.exe.bak-t"
	// The rollback's OWN restore attempt (RenameFile(backupPath, target))
	// is the SECOND time this exact rename pair is invoked in the run (the
	// first was backup-old's target->backupPath rename, a different
	// direction) — renameFailOnce is keyed by source path, so failing the
	// source "target.exe.bak-t" only affects the restore call.
	s.renameFailOnce[backupPath] = true

	plan := Plan{
		Staged: "staged.exe", Target: "target.exe", ExpectedSHA256: "NEWHASH",
		HealthURL: "http://node/fleet/health", RestartTaskName: "offload-fleet-node",
		BackupSuffix: "t", RestartTimeout: time.Second,
	}
	out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
	if out.OK {
		t.Fatal("expected failure")
	}
	if out.RolledBack {
		t.Error("RolledBack should be false: the rollback's own restore-backup step failed")
	}
	if out.RollbackOK {
		t.Error("RollbackOK should be false: the box may be left without the old binary in place")
	}
	found := false
	for _, st := range out.Steps {
		if st.Name == "restore-backup" && !st.OK {
			found = true
		}
	}
	if !found {
		t.Error("expected a FAILING restore-backup step recorded in the rollback's own steps")
	}
}

func TestRun_RestartFailureRollsBack(t *testing.T) {
	s := newFakeState()
	s.files["staged.exe"] = "NEWHASH"
	s.files["target.exe"] = "OLDHASH"
	s.restartFail = true
	s.health = func() (HealthInfo, error) { return HealthInfo{OK: true}, nil }

	plan := Plan{
		Staged: "staged.exe", Target: "target.exe", ExpectedSHA256: "NEWHASH",
		HealthURL: "http://node/fleet/health", RestartTaskName: "offload-fleet-node",
		BackupSuffix: "t", RestartTimeout: time.Second,
	}
	out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
	if out.OK {
		t.Fatal("expected failure when the restart command fails")
	}
	if !out.RolledBack {
		t.Fatal("expected an automatic rollback attempt")
	}
	if s.files["target.exe"] != "OLDHASH" {
		t.Errorf("target.exe after rollback = %q, want OLDHASH restored", s.files["target.exe"])
	}
}

func TestRun_VerifyFailureRollsBackAndRestartCalledTwice(t *testing.T) {
	s := newFakeState()
	s.files["staged.exe"] = "NEWHASH"
	s.files["target.exe"] = "OLDHASH"
	s.health = func() (HealthInfo, error) { return HealthInfo{OK: true}, nil }
	// Restart "succeeds" both times but never actually relaunches a
	// process — verifyRunning then finds nothing and fails, forcing a
	// rollback whose own restart+reverify DOES relaunch (old image).
	restartCount := 0
	s.launchOnRestartFn(func() {
		restartCount++
		if restartCount >= 2 { // only the ROLLBACK's restart relaunches
			h := s.files["target.exe"]
			s.procs = append(s.procs, ProcessInfo{PID: 500 + restartCount, CommandLine: "target.exe fleet-serve", ExecutablePath: "target.exe"})
			_ = h
		}
	})

	plan := Plan{
		Staged: "staged.exe", Target: "target.exe", ExpectedSHA256: "NEWHASH",
		HealthURL: "http://node/fleet/health", RestartTaskName: "offload-fleet-node",
		BackupSuffix: "t", VerifyTimeout: 2 * time.Second, VerifyPollInterval: time.Millisecond,
		RestartTimeout: time.Second,
	}
	out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
	if out.OK {
		t.Fatal("expected failure: the first restart never produced a live process")
	}
	if !out.RolledBack || !out.RollbackOK {
		t.Fatalf("expected a successful rollback, got RolledBack=%v RollbackOK=%v error=%q", out.RolledBack, out.RollbackOK, out.Error)
	}
	if s.files["target.exe"] != "OLDHASH" {
		t.Errorf("target.exe after rollback = %q, want OLDHASH", s.files["target.exe"])
	}
	if s.restartCalled < 2 {
		t.Errorf("expected restart to be called again during rollback, calls=%d", s.restartCalled)
	}
}

func TestRun_RenderTreeSwapOptionalAndRolledBackOnLaterFailure(t *testing.T) {
	s := newFakeState()
	s.files["staged.exe"] = "NEWHASH"
	s.files["target.exe"] = "OLDHASH"
	s.files["render.tar.gz"] = "TARBALL"
	s.restartFail = true // force a failure AFTER the render swap so rollback must undo both

	plan := Plan{
		Staged: "staged.exe", Target: "target.exe", ExpectedSHA256: "NEWHASH",
		RestartTaskName: "offload-fleet-node", RestartTimeout: time.Second,
		RenderTarball: "render.tar.gz", RenderDir: "render-dir", BackupSuffix: "t",
	}
	out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
	if out.OK {
		t.Fatal("expected failure from the forced restart failure")
	}
	if !out.RenderSwapped {
		t.Error("expected the render tree to have been swapped before the restart step failed")
	}
	if !out.RolledBack {
		t.Fatal("expected rollback")
	}
	// render-dir should be back to nonexistent/original after rollback
	// (RemoveAll then restore from backup — here the backup never existed
	// since render-dir had no prior content, so it should be gone again).
	if _, ok := s.files["render-dir"]; ok {
		t.Errorf("render-dir should have been rolled back, still has content: %q", s.files["render-dir"])
	}
}

func TestRun_StandaloneNodeSkipsRestartAndVerifiesByHashAlone(t *testing.T) {
	s := newFakeState()
	s.files["staged.exe"] = "NEWHASH"
	s.files["target.exe"] = "OLDHASH"
	plan := Plan{Staged: "staged.exe", Target: "target.exe", ExpectedSHA256: "NEWHASH", BackupSuffix: "t"}
	out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
	if !out.OK {
		t.Fatalf("standalone swap (no restart mechanism) should succeed by hash alone, got error=%q", out.Error)
	}
	if s.restartCalled != 0 {
		t.Errorf("no restart mechanism was configured; RunCommand should not have been asked to start anything, got %d calls", s.restartCalled)
	}
	if out.FinalPID != 0 {
		t.Errorf("standalone swap has no process to report a PID for, got %d", out.FinalPID)
	}
	if out.FinalImageSHA256 != "NEWHASH" {
		t.Errorf("FinalImageSHA256 = %q, want NEWHASH", out.FinalImageSHA256)
	}
}

// TestRun_StandaloneNodeWaitsForGPULeaseToClear: gap 6b (d5207011 deploy
// record, OptiPlex) — a standalone node has no fleet-serve queue depth to
// read, but it CAN still be mid-render under a caller's own `gpu reserve`;
// this used to be the operator's own manual "gpu status" check before the
// swap. Same shape as TestRun_WaitsForIdleBeforeSwapping, GPU-lease flavored.
func TestRun_StandaloneNodeWaitsForGPULeaseToClear(t *testing.T) {
	s := newFakeState()
	s.files["staged.exe"] = "NEWHASH"
	s.files["target.exe"] = "OLDHASH"
	calls := 0
	s.gpuHeld = func() (GPULeaseInfo, error) {
		calls++
		if calls < 3 {
			return GPULeaseInfo{Held: true, Reason: "media render in flight"}, nil
		}
		return GPULeaseInfo{Held: false}, nil
	}
	plan := Plan{
		Staged: "staged.exe", Target: "target.exe", ExpectedSHA256: "NEWHASH", BackupSuffix: "t",
		IdlePollInterval: time.Millisecond, WaitIdleTimeout: time.Minute,
	}
	out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
	if !out.OK {
		t.Fatalf("expected the swap to proceed once the lease clears, got error=%q steps=%+v", out.Error, out.Steps)
	}
	if calls < 3 {
		t.Errorf("expected at least 3 lease polls, got %d", calls)
	}
	if s.files["target.exe"] != "NEWHASH" {
		t.Error("standalone swap did not touch target.exe after the lease cleared")
	}
}

// TestRun_StandaloneNodeGPULeaseNeverClearsTimesOut: the failure twin —
// a lease held the whole timeout window must refuse the swap, never touch
// the binary. Same shape as TestRun_NeverIdleTimesOut.
func TestRun_StandaloneNodeGPULeaseNeverClearsTimesOut(t *testing.T) {
	s := newFakeState()
	s.files["staged.exe"] = "NEWHASH"
	s.files["target.exe"] = "OLDHASH"
	s.gpuHeld = func() (GPULeaseInfo, error) { return GPULeaseInfo{Held: true, Reason: "media render in flight"}, nil }
	plan := Plan{
		Staged: "staged.exe", Target: "target.exe", ExpectedSHA256: "NEWHASH", BackupSuffix: "t",
		WaitIdleTimeout: 3 * time.Second, IdlePollInterval: time.Second,
	}
	out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
	if out.OK {
		t.Fatal("expected timeout failure when the GPU lease never clears")
	}
	if !strings.Contains(out.Error, "GPU lease never cleared") {
		t.Errorf("Error = %q, want a GPU-lease timeout message", out.Error)
	}
	if s.files["target.exe"] != "OLDHASH" {
		t.Error("target.exe was touched despite the GPU lease never clearing")
	}
}

// TestRun_StandaloneNodeSkipsGPUWaitWhenDepsHasNone: a caller using the older
// Deps shape (InspectGPULease left nil, e.g. a not-yet-updated test or tool)
// must keep today's exact behavior — skip straight through, never a nil-
// function-pointer panic.
func TestRun_StandaloneNodeSkipsGPUWaitWhenDepsHasNone(t *testing.T) {
	s := newFakeState()
	s.files["staged.exe"] = "NEWHASH"
	s.files["target.exe"] = "OLDHASH"
	plan := Plan{Staged: "staged.exe", Target: "target.exe", ExpectedSHA256: "NEWHASH", BackupSuffix: "t"}
	out := Run(context.Background(), plan, s.deps(), NewLogger(nil)) // s.gpuHeld left nil
	if !out.OK {
		t.Fatalf("expected success with InspectGPULease unset, got error=%q", out.Error)
	}
}

func TestBackupPathFor(t *testing.T) {
	cases := []struct{ suffix, want string }{
		{"2026-09-24-pre-d5207011", "target.exe.bak-2026-09-24-pre-d5207011"},
		// The d5207011 Qube deploy record's exact mistake: a suffix that ALREADY
		// carries the tool's own "bak-" prefix must not double it.
		{"bak-2026-09-24-pre-d5207011", "target.exe.bak-2026-09-24-pre-d5207011"},
		{"BAK-2026-09-24", "target.exe.bak-2026-09-24"},
		// "bakery-run" starts with "bake", not the literal 4-char "bak-" (hyphen,
		// not 'e') — must NOT be trimmed; only an exact "bak-" prefix is stripped.
		{"bakery-run", "target.exe.bak-bakery-run"},
	}
	for _, c := range cases {
		if got := backupPathFor("target.exe", c.suffix); got != c.want {
			t.Errorf("backupPathFor(target.exe, %q) = %q, want %q", c.suffix, got, c.want)
		}
	}
}

func TestValidatePlan(t *testing.T) {
	cases := []struct {
		name string
		plan Plan
		want string
	}{
		{"missing target", Plan{Staged: "s", ExpectedSHA256: "h"}, "required"},
		{"missing hash", Plan{Staged: "s", Target: "t"}, "sha256"},
		{"conflicting restart", Plan{Staged: "s", Target: "t", ExpectedSHA256: "h", RestartTaskName: "a", RestartCommand: "b"}, "mutually exclusive"},
		{"render without dir", Plan{Staged: "s", Target: "t", ExpectedSHA256: "h", RenderTarball: "r.tar.gz"}, "render-dir"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validatePlan(c.plan)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("validatePlan(%+v) = %v, want an error containing %q", c.plan, err, c.want)
			}
		})
	}
}
