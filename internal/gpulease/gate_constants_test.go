package gpulease

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// The numbers of the gate are decisions with a measured reason, and every other test of the gate derives its expectation from the
// constant it exercises (a budget of int(absentBudget/absentPause) pauses, a staged file of stagingFresh-1s), so each of them can drift
// with no test red. They are pinned here by value, with the reason, and a change of one is a decision to be made with the measurement in
// hand: the test fails to say so.
func TestTheGateConstantsAreTheOnesTheyWereMeasuredFor(t *testing.T) {
	if absentBudget != 250*time.Millisecond {
		t.Errorf("absentBudget = %v, want 250ms: twice the longest absence a reader saw (75 ms; one rename took 119 ms to return)", absentBudget)
	}
	if absentPause != 5*time.Millisecond {
		t.Errorf("absentPause = %v, want 5ms, the pause of the wait for a refused open", absentPause)
	}
	if stagingFresh != 3*time.Second {
		t.Errorf("stagingFresh = %v, want 3s: the longest a writer retries its rename (2 s) and a second, a retry chain took 2.29 s", stagingFresh)
	}
	if absentMemory != 5*time.Second {
		t.Errorf("absentMemory = %v, want 5s: longer than a staged file stays fresh, so a wait that ran out is paid once for the file", absentMemory)
	}
	if lockGateCap != time.Second {
		t.Errorf("lockGateCap = %v, want 1s: half of what a second acquirer spins for the epoch lock", lockGateCap)
	}

	// The relations that make them work together.
	chain := time.Duration(renameAttempts) * renamePause
	if stagingFresh < chain+time.Second {
		t.Errorf("stagingFresh %v is less than the longest rename retry chain %v plus a second: a staged file of a rename that is still retrying would read as a dead writer's, and a live lease as having no heartbeat", stagingFresh, chain)
	}
	if absentMemory < stagingFresh {
		t.Errorf("absentMemory %v is shorter than stagingFresh %v: a stuck leftover would be waited for again while it is still fresh", absentMemory, stagingFresh)
	}
	if spin := time.Duration(epochLockAttempts) * epochLockPause; lockGateCap >= spin {
		t.Errorf("lockGateCap %v is not under the spin of a second acquirer %v", lockGateCap, spin)
	}
}

// The Node readers have their own copy of the numbers (render/lease-read.mjs), and a reader in either language must wait for the same
// rename. A test of each side that pins its own literal does not tie them to each other.
func TestTheNodeReaderUsesTheSameNumbersAsTheGoGate(t *testing.T) {
	mjs, err := os.ReadFile(filepath.Join("..", "..", "render", "lease-read.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		want time.Duration
	}{
		{"ABSENT_BUDGET_MS", absentBudget},
		{"ABSENT_PAUSE_MS", absentPause},
		{"STAGING_FRESH_MS", stagingFresh},
	} {
		m := regexp.MustCompile(`(?m)^export const ` + c.name + ` = (\d+);`).FindSubmatch(mjs)
		if m == nil {
			t.Errorf("render/lease-read.mjs does not declare %s", c.name)
			continue
		}
		ms, _ := strconv.Atoi(string(m[1]))
		if got := time.Duration(ms) * time.Millisecond; got != c.want {
			t.Errorf("render/lease-read.mjs %s = %v, the Go gate has %v: a reader in one language would give up on a rename the other waits for", c.name, got, c.want)
		}
	}
}

// A staged file is a rename in flight for as long as its writer can still be retrying that rename. The retry chain of a refused rename was
// seen to take 2.29 s, so a staged file of 1.5 s, 2.3 s, 2.5 s or 2.9 s beside an absent name is still being installed, and one of 3.1 s is
// a dead writer's. Every other test stages a file of no age or of a minute or more.
func TestAStagedFileAsOldAsALongRenameRetryChainIsStillAnInstallInFlight(t *testing.T) {
	for _, c := range []struct {
		age      time.Duration
		inFlight bool
	}{
		{1500 * time.Millisecond, true},
		{2300 * time.Millisecond, true},
		{2500 * time.Millisecond, true},
		{2900 * time.Millisecond, true},
		{3100 * time.Millisecond, false},
	} {
		t.Run(c.age.String(), func(t *testing.T) {
			clk := onGateClock(t)
			path := filepath.Join(t.TempDir(), "hb.7")
			stageBeside(t, clk, path, c.age)
			if got := stagedInFlight(path); got != c.inFlight {
				t.Errorf("stagedInFlight with a staged file %v old = %v, want %v", c.age, got, c.inFlight)
			}
			deniedOpenMayClear = false // no wait: a wait moves the gate's clock, and a staged file 2.9 s old is 3.1 s old after it
			_, err := statSettled(path)
			if got := err != nil && errorsIsInFlight(err); got != c.inFlight {
				t.Errorf("statSettled with a staged file %v old beside an absent name = %v, want a rename in flight: %v", c.age, err, c.inFlight)
			}
		})
	}
}

// The lease-level consequence: an overdue live lease whose heartbeat rename has been retrying for 2.3 s, and has just landed its staged file
// under the live name's absence, is kept and not granted to a second job.
func TestAnOverdueLeaseWhoseHeartbeatRenameHasBeenRetryingForTwoSecondsIsKept(t *testing.T) {
	for _, devs := range [][]string{{card0}, nil} {
		kind := "whole-node lease"
		if len(devs) > 0 {
			kind = "card lease"
		}
		t.Run(kind, func(t *testing.T) {
			onGateClock(t)
			m, now := scopedManager(t)
			l := overdueLease(t, m, now, devs...)
			defer l.Release()
			hb := m.heartbeatPath(l.Epoch())
			if err := os.Rename(hb, hb+".tmp"); err != nil {
				t.Fatal(err)
			}
			at := gateNow().Add(-2300 * time.Millisecond)
			if err := os.Chtimes(hb+".tmp", at, at); err != nil {
				t.Fatal(err)
			}
			refusedAsHeld(t, m, "because its heartbeat rename had been retrying for 2.3 s", devs...)
		})
	}
}

// A rename that took 75 ms to land, the longest absence a reader was seen to meet, is waited out and read.
func TestARenameThatEndsAfterSeventyFiveMillisecondsIsWaitedOutAndRead(t *testing.T) {
	c := onGateClock(t)
	path := filepath.Join(t.TempDir(), "hb.7")
	stageBeside(t, c, path, 0)
	endRenameAtOpen(t, path, 16) // fifteen failed opens, a pause after each: 75 ms
	b, err := readSharedFile(path)
	if err != nil || string(b) != "42" {
		t.Fatalf("readSharedFile = %q, %v after a rename that took 75 ms, want the new content 42", b, err)
	}
	if got := gatePauses.Load(); got != 15 {
		t.Errorf("the gate paused %d times, want 15 (75 ms at 5 ms)", got)
	}
}
