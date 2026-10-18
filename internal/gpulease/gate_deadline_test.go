package gpulease

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The budget of the gate is a DEADLINE on the clock and not a count of pauses (gate.go): a timer that sleeps longer than it is asked, as
// Windows' does (a 1 ms sleep takes a whole 15.4 ms tick), must still give up after about the budget. Every other test of the gate moves
// its clock by exactly the pause it is asked for, where a count and a deadline coincide, so they cannot tell the two apart. These sleep
// four times what they are asked.
func slowTimer(c *gateClock) {
	gateSleep = func(d time.Duration) { c.sleep(4 * d) }
}

func wantADeadline(t *testing.T, what string, elapsed time.Duration) {
	t.Helper()
	if elapsed < absentBudget || elapsed >= absentBudget+4*absentPause {
		t.Errorf("%s waited %v of the gate's clock with a timer that sleeps four times what it is asked, want about %v (a deadline), not a count of pauses (%d)", what, elapsed, absentBudget, gatePauses.Load())
	}
}

func TestTheBudgetOfAReadIsADeadlineAndNotACountOfPauses(t *testing.T) {
	c := onGateClock(t)
	slowTimer(c)
	path := filepath.Join(t.TempDir(), "hb.7")
	stuckBeside(t, c, path)
	start := c.now()
	if _, err := readSharedFile(path); !errorsIsInFlight(err) {
		t.Fatalf("readSharedFile = %v, want the I/O failure of a wait that ran out", err)
	}
	wantADeadline(t, "a read beside a stuck leftover", c.now().Sub(start))
}

func TestTheBudgetOfAStatIsADeadlineAndNotACountOfPauses(t *testing.T) {
	c := onGateClock(t)
	slowTimer(c)
	path := filepath.Join(t.TempDir(), "hb.7")
	stuckBeside(t, c, path)
	start := c.now()
	if _, err := statSettled(path); !errorsIsInFlight(err) {
		t.Fatalf("statSettled = %v, want the I/O failure of a wait that ran out", err)
	}
	wantADeadline(t, "a stat beside a stuck leftover", c.now().Sub(start))
}

// The look that finds the name there although the open of it found it absent (the rename ended between the two) is bounded by the same
// deadline, if it keeps happening.
func TestTheBudgetOfAReadThatKeepsFindingTheNameThereIsADeadlineAndNotACountOfLooks(t *testing.T) {
	c := onGateClock(t)
	slowTimer(c)
	path := filepath.Join(t.TempDir(), "hb.7")
	if err := os.WriteFile(path, []byte("42"), 0o666); err != nil {
		t.Fatal(err)
	}
	old := openSharedFile
	openSharedFile = func(p string) (*os.File, error) { return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist} }
	t.Cleanup(func() { openSharedFile = old })
	start := c.now()
	_, err := readSharedFile(path)
	if !errorsIsInFlight(err) {
		t.Fatalf("readSharedFile = %v, want the I/O failure of a wait that ran out: the name is there and every open says it is not", err)
	}
	wantADeadline(t, "a read whose opens keep missing a name that is there", c.now().Sub(start))
}
