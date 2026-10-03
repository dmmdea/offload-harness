package nodeswap

// A lease held by a process the deploy would stop (GPU routing P7, review finding on --cards).
//
// --cards lets a deploy leave a lease on another card alone. The stop paths do not look at cards:
// stopForSwap stops the fleet-serve process, and renameWithRetry stops any MCP helper that holds
// the rename. Both kinds of process take a lease IN-PROCESS while they render (the MCP server and
// the fleet node serve foreign calls and hold leases in one process), so a lease on card C held by
// pid 300 is ended by stopping pid 300 even when the deploy "touches" card A. The lease records
// its holder's pid, and the deploy already lists the processes running the image it replaces, so
// the tool CAN tell which leases it would end: a lease whose holder is a process the deploy may
// stop holds the deploy whatever its cards, and the rename retry never stops such a process.
//
// A process the deploy never stops (a `gpu reserve` wrapper keeps running its old image) does not
// hold the deploy: that is the whole point of naming cards.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// helperHoldsRename is the world the finding probed: an MCP helper on the target exe blocks the
// rename once, so the deploy reaches the stop path.
func helperHoldsRename(pid int) *fakeState {
	s := stagedState()
	s.procs = []ProcessInfo{{PID: pid, CommandLine: "target.exe mcp", ExecutablePath: "target.exe"}}
	s.renameFailOnce["target.exe"] = true
	return s
}

// renderHeldBy is a media render on the given cards whose lease the process pid holds.
func renderHeldBy(epoch uint64, pid int, devices ...string) GPULeaseInfo {
	info := renderOn(epoch, devices...)
	info.Leases[0].PID = pid
	return info
}

func procAlive(s *fakeState, pid int) bool {
	for _, p := range s.procs {
		if p.PID == pid {
			return true
		}
	}
	return false
}

func shortWait(p Plan) Plan {
	p.WaitIdleTimeout, p.IdlePollInterval = 3*time.Second, time.Second
	return p
}

// The blocker: the helper holds the lease on card C in-process, the deploy names card A, and the
// rename is blocked by that very helper. Stopping it would end the render, so the deploy waits
// for the lease instead of narrowing it away, and the helper is never stopped.
func TestDeployWaitsForALeaseHeldInProcessByTheHelperItWouldStop(t *testing.T) {
	s := helperHoldsRename(300)
	s.gpuHeld = func() (GPULeaseInfo, error) { return renderHeldBy(7, 300, deployCardC), nil }
	out := Run(context.Background(), shortWait(standalonePlan(deployCardA)), s.deps(), NewLogger(nil))
	if out.OK {
		t.Fatalf("the deploy went ahead: a render on card C holds the MCP helper the rename retry would stop (steps %+v)", out.Steps)
	}
	if !procAlive(s, 300) {
		t.Fatal("pid 300 was stopped while it held a render's lease: the render died with it")
	}
	if s.files["target.exe"] != "OLDHASH" {
		t.Fatal("the binary was touched with the render's holder still running")
	}
	for _, want := range []string{"GPU lease never cleared", "epoch 7", "pid 300"} {
		if !strings.Contains(out.Error, want) {
			t.Errorf("error = %q, want it to name the lease and the holder (%q missing)", out.Error, want)
		}
	}
	if len(out.LeasesLeftAlone) != 0 {
		t.Fatalf("leases left alone = %v: the lease the deploy would end was left alone", out.LeasesLeftAlone)
	}
}

// The same helper, but the lease on card C is held by a different process: the helper holds
// nothing, so it is stopped to clear the rename as always, and the render on card C is left alone.
func TestDeployStillStopsAnIdleHelperBesideALeaseItDoesNotHold(t *testing.T) {
	s := helperHoldsRename(300)
	s.gpuHeld = func() (GPULeaseInfo, error) { return renderHeldBy(7, 400, deployCardC), nil }
	out := Run(context.Background(), standalonePlan(deployCardA), s.deps(), NewLogger(nil))
	if !out.OK {
		t.Fatalf("an idle helper and a render held by another process on card C must not stop the deploy: %q", out.Error)
	}
	if procAlive(s, 300) {
		t.Fatal("the idle helper was left running: the rename cannot have been cleared")
	}
	if s.files["target.exe"] != "NEWHASH" {
		t.Fatal("the swap did not happen")
	}
	if len(out.LeasesLeftAlone) != 1 || !strings.Contains(out.LeasesLeftAlone[0], "epoch 7") {
		t.Fatalf("leases left alone = %v, want the render named", out.LeasesLeftAlone)
	}
}

// A `gpu reserve` wrapper runs the same exe but is never stopped by a deploy (it keeps running
// its old image), so its lease on another card does not hold a deploy that names card A.
func TestDeployLeavesAloneALeaseHeldByAProcessItNeverStops(t *testing.T) {
	s := stagedState()
	s.procs = []ProcessInfo{{PID: 400, CommandLine: "target.exe gpu reserve --devices c -- render", ExecutablePath: "target.exe"}}
	s.gpuHeld = func() (GPULeaseInfo, error) { return renderHeldBy(7, 400, deployCardC), nil }
	out := Run(context.Background(), standalonePlan(deployCardA), s.deps(), NewLogger(nil))
	if !out.OK {
		t.Fatalf("a wrapper on card C runs its old image and is never stopped: %q", out.Error)
	}
	if !procAlive(s, 400) || len(out.LeasesLeftAlone) != 1 {
		t.Fatalf("wrapper alive=%v left alone=%v, want the wrapper untouched and its lease recorded", procAlive(s, 400), out.LeasesLeftAlone)
	}
}

// The fleet node is the other process the deploy stops (stopForSwap), and it also holds leases
// in-process while it serves a job.
func TestDeployWaitsForALeaseHeldInProcessByTheFleetNodeItWouldStop(t *testing.T) {
	s := stagedState()
	s.procs = []ProcessInfo{{PID: 100, CommandLine: "target.exe fleet-serve", ExecutablePath: "target.exe"}}
	s.gpuHeld = func() (GPULeaseInfo, error) { return renderHeldBy(7, 100, deployCardC), nil }
	out := Run(context.Background(), shortWait(standalonePlan(deployCardA)), s.deps(), NewLogger(nil))
	if out.OK || !strings.Contains(out.Error, "GPU lease never cleared") {
		t.Fatalf("ok=%v error=%q: stopping the fleet node would end the render it holds on card C", out.OK, out.Error)
	}
	if !procAlive(s, 100) {
		t.Fatal("the fleet node was stopped while it held a lease")
	}
}

// If the deploy cannot list the processes running the image it replaces, it cannot show a lease
// is not held by one it would stop, so it does not narrow: the whole-node wait, as before --cards.
func TestDeployDoesNotNarrowWhenItCannotListTheImagesHolders(t *testing.T) {
	s := stagedState()
	s.gpuHeld = func() (GPULeaseInfo, error) { return renderHeldBy(7, 400, deployCardC), nil }
	deps := s.deps()
	deps.FindProcessesByExe = func(string) ([]ProcessInfo, error) { return nil, errors.New("CIM unavailable") }
	out := Run(context.Background(), shortWait(standalonePlan(deployCardA)), deps, NewLogger(nil))
	if out.OK || !strings.Contains(out.Error, "GPU lease never cleared") {
		t.Fatalf("ok=%v error=%q: with the holders unknown the lease on card C must still hold the deploy", out.OK, out.Error)
	}
	if s.files["target.exe"] != "OLDHASH" {
		t.Fatal("the binary was touched with the lease unverified")
	}
}

// The belt: a lease can appear between the wait and the rename, and a node with a health URL
// never ran the lease wait at all. The rename retry reads the leases itself and never stops a
// process that holds one, whatever cards it sits on.
func TestRenameRetryNeverStopsAHelperThatHoldsALease(t *testing.T) {
	s := helperHoldsRename(300)
	s.gpuHeld = func() (GPULeaseInfo, error) { return renderHeldBy(7, 300, deployCardA), nil }
	err := renameWithRetry(standalonePlan(deployCardA), s.deps(), NewLogger(nil), "target.exe", "target.exe.bak-t")
	if err == nil {
		t.Fatal("the rename went ahead after stopping a helper that holds a render's lease")
	}
	if !procAlive(s, 300) {
		t.Fatal("the helper holding the lease was stopped")
	}
	if !strings.Contains(err.Error(), "epoch 7") || !strings.Contains(err.Error(), "pid 300") {
		t.Fatalf("error = %q, want it to say which lease the helper holds", err)
	}
	if s.files["target.exe"] != "OLDHASH" {
		t.Fatal("the binary moved")
	}
}

// With the leases unreadable the retry cannot show the helper holds none, so it stops nothing.
func TestRenameRetryStopsNothingWhenTheLeasesCannotBeRead(t *testing.T) {
	s := helperHoldsRename(300)
	s.gpuHeld = func() (GPULeaseInfo, error) { return GPULeaseInfo{}, errors.New("lease directory unreadable") }
	err := renameWithRetry(standalonePlan(), s.deps(), NewLogger(nil), "target.exe", "target.exe.bak-t")
	if err == nil || !procAlive(s, 300) {
		t.Fatalf("err=%v alive=%v: an unreadable lease directory must leave the helper running", err, procAlive(s, 300))
	}
	if !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("error = %q, want the read failure named", err)
	}
}

// Control: the retry still stops an idle helper (one that holds no lease), as it always did.
func TestRenameRetryStillStopsAnIdleHelper(t *testing.T) {
	s := helperHoldsRename(300)
	s.gpuHeld = func() (GPULeaseInfo, error) { return renderHeldBy(7, 400, deployCardC), nil }
	if err := renameWithRetry(standalonePlan(deployCardA), s.deps(), NewLogger(nil), "target.exe", "target.exe.bak-t"); err != nil {
		t.Fatalf("an idle helper must still be stopped to clear the rename: %v", err)
	}
	if procAlive(s, 300) || s.files["target.exe.bak-t"] != "OLDHASH" {
		t.Fatalf("alive=%v files=%v, want the helper stopped and the binary moved", procAlive(s, 300), s.files)
	}
}

// The real inspector carries the process that holds each lease.
func TestInspectGPULeaseCarriesTheHoldersPID(t *testing.T) {
	dir := t.TempDir()
	release := acquireScratchLease(t, dir, "render A", deployCardA)
	defer release()
	info, err := inspectGPULease(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Leases) != 1 || info.Leases[0].PID != os.Getpid() {
		t.Fatalf("leases = %+v, want the one lease held by this process (pid %d)", info.Leases, os.Getpid())
	}
}

// --cards narrows the standalone wait. A node with a health URL waits on its job count, which
// is node-wide, so --cards could only be ignored there: it is refused, not silently ignored,
// and nothing is touched. (A health URL the command read from the node's own config counts.)
func TestCardsWithAHealthURLAreRefusedNotIgnored(t *testing.T) {
	s := stagedState()
	s.health = func() (HealthInfo, error) { return HealthInfo{OK: true}, nil }
	plan := standalonePlan(deployCardA)
	plan.HealthURL = "http://node.invalid/fleet/health"
	out := Run(context.Background(), plan, s.deps(), NewLogger(nil))
	if out.OK || !strings.Contains(out.Error, "--cards") || !strings.Contains(out.Error, "health") {
		t.Fatalf("ok=%v error=%q, want the refusal to say --cards does not apply to a node with a health URL", out.OK, out.Error)
	}
	if s.files["target.exe"] != "OLDHASH" || len(out.Steps) != 1 || out.Steps[0].Name != "validate" {
		t.Fatalf("files=%v steps=%+v, want the plan refused at validation with nothing touched", s.files, out.Steps)
	}
}

// A lease is held and its holder is not named (a caller that reports only Held): no helper can
// be shown to hold none, so none is stopped.
func TestRenameRetryStopsNothingWhenAHeldLeaseNamesNoHolder(t *testing.T) {
	s := helperHoldsRename(300)
	s.gpuHeld = func() (GPULeaseInfo, error) { return GPULeaseInfo{Held: true, Reason: "old fake"}, nil }
	err := renameWithRetry(standalonePlan(), s.deps(), NewLogger(nil), "target.exe", "target.exe.bak-t")
	if err == nil || !procAlive(s, 300) {
		t.Fatalf("err=%v alive=%v: a held lease with no named holder must leave the helper running", err, procAlive(s, 300))
	}
	if !strings.Contains(err.Error(), "not named") {
		t.Fatalf("error = %q, want it to say the holder is not named", err)
	}
}
