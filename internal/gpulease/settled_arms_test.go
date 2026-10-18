package gpulease

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// statSettled has two arms (settled.go): where a rename can leave the live name absent it waits for the rename to end (gate.go), and
// elsewhere it answers at once. Which one a test runs depends on the build, and the settled_test.go tests do not choose, so on a Linux
// gate runner the arm that Windows runs was not exercised and on Windows the other was not. These choose.

// straddlingStat makes the first look at path find nothing and every later one find the file, which is there: the rename ends between
// the first look and the look at the staged file. It returns the count of looks at the name.
func straddlingStat(t *testing.T, path string) *int {
	t.Helper()
	if err := os.WriteFile(path, []byte("1"), 0o666); err != nil {
		t.Fatal(err)
	}
	looks := 0
	old := statFile
	statFile = func(p string) (os.FileInfo, error) {
		if p == path {
			looks++
			if looks == 1 {
				return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist} // the rename had not landed yet
			}
		}
		return old(p)
	}
	t.Cleanup(func() { statFile = old })
	return &looks
}

// The rename ends between the look at the name and the look at the staged file: the second look at the name finds it, on the arm that
// waits as well as on the one that does not.
func TestAStatThatMeetsTheRenameEndingBetweenTheLooksReturnsTheFileOnTheArmThatWaits(t *testing.T) {
	onGateClock(t) // the gate on, whatever the build
	path := filepath.Join(t.TempDir(), "hb.1")
	looks := straddlingStat(t, path)
	fi, err := statSettled(path)
	if err != nil || fi == nil {
		t.Fatalf("statSettled with the gate on = %v, %v, want the file that appeared between the looks", fi, err)
	}
	if *looks != 2 {
		t.Errorf("the name was looked at %d times, want 2 (before and after the look at the staged file)", *looks)
	}
}

func TestAStatThatMeetsTheRenameEndingBetweenTheLooksReturnsTheFileOnTheArmThatDoesNotWait(t *testing.T) {
	onGateClock(t)
	deniedOpenMayClear = false // the gate off: where rename(2) is atomic
	path := filepath.Join(t.TempDir(), "hb.1")
	looks := straddlingStat(t, path)
	fi, err := statSettled(path)
	if err != nil || fi == nil {
		t.Fatalf("statSettled with the gate off = %v, %v, want the file that appeared between the looks", fi, err)
	}
	if *looks != 2 {
		t.Errorf("the name was looked at %d times, want 2 (before and after the look at the staged file)", *looks)
	}
}

// Where there is nothing to wait for the stat answers at once even beside a fresh leftover: no pause, and the answer is the I/O failure
// of a rename in flight (the staged file says the name is not settled), not an absence. With the gate on the same leftover is waited for.
func TestStatSettledTakesNoPauseWhereTheGateIsOffAndWaitsWhereItIsOn(t *testing.T) {
	c := onGateClock(t)
	path := filepath.Join(t.TempDir(), "hb.1")
	stuckBeside(t, c, path)

	deniedOpenMayClear = false
	if _, err := statSettled(path); !errorsIsInFlight(err) {
		t.Errorf("statSettled with the gate off = %v, want the I/O failure of a rename in flight", err)
	}
	if got := gatePauses.Load(); got != 0 {
		t.Errorf("statSettled with the gate off paused %d times, want 0: nothing can be installing the file where rename(2) is atomic", got)
	}

	deniedOpenMayClear = true
	if _, err := statSettled(path); !errorsIsInFlight(err) {
		t.Errorf("statSettled with the gate on = %v, want the I/O failure of a wait that ran out", err)
	}
	if got, want := gatePauses.Load(), int64(absentBudget/absentPause); got != want {
		t.Errorf("statSettled with the gate on paused %d times, want %d (one budget)", got, want)
	}
}
