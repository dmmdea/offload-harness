package smitest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	MaybeRun()
	os.Exit(m.Run())
}

// The stand-in is what nvidia-smi is to everything that looks it up on PATH: it answers the per-device query
// with the fields asked for, in the order asked for, and every call is recorded.
func TestTheStandInAnswersThePerDeviceQueryInTheFieldsAskedFor(t *testing.T) {
	st := Install(t)
	out, err := exec.Command("nvidia-smi", "--query-gpu=index,uuid,display_attached", "--format=csv,noheader,nounits").Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(string(out), "\r\n", "\n")), "\n")
	want := []string{"0, " + UUIDA + ", No", "1, " + UUIDB + ", Yes", "2, " + UUIDC + ", No"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Errorf("rows = %q, want %q", lines, want)
	}
	calls := st.Calls()
	if len(calls) != 1 || len(calls[0]) != 2 || calls[0][0] != "--query-gpu=index,uuid,display_attached" {
		t.Errorf("calls = %q, want the one query", calls)
	}
}

// Anything but that query is recorded and refused, so a test that asserts on Calls sees what a code path ran,
// not only what succeeded. This is what makes "the allocation never lists processes" checkable.
func TestTheStandInRecordsAndRefusesAnythingButThePerDeviceQuery(t *testing.T) {
	st := Install(t)
	asked := [][]string{{"-L"}, {"--query-compute-apps=pid,name", "--format=csv"}, {"pmon", "-c", "1"}, {"--query-gpu=index", "--format=csv"}}
	for _, args := range asked {
		var ee *exec.ExitError
		if err := exec.Command("nvidia-smi", args...).Run(); !errors.As(err, &ee) || ee.ExitCode() != 99 {
			t.Errorf("%q: err = %v, want exit 99", args, err)
		}
	}
	calls := st.Calls()
	if len(calls) != len(asked) {
		t.Fatalf("recorded %d calls, want %d: %q", len(calls), len(asked), calls)
	}
	for i := range asked {
		if strings.Join(calls[i], " ") != strings.Join(asked[i], " ") {
			t.Errorf("call %d = %q, want %q", i, calls[i], asked[i])
		}
	}
}

// A driver that does not know a field refuses the whole query, on stdout, status 2: the shape the
// display_attached fallback in gpuprobe keys on.
func TestTheStandInRefusesAFieldItDoesNotKnowTheWayNvidiaSmiDoes(t *testing.T) {
	Install(t)
	out, err := exec.Command("nvidia-smi", "--query-gpu=index,no_such_field", "--format=csv,noheader,nounits").Output()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("err = %v, want exit 2", err)
	}
	if !strings.Contains(string(out), `Field "no_such_field" is not a valid field to query.`) {
		t.Errorf("stdout = %q", out)
	}
}

// Steps are taken in order and the last one repeats; a call its caller's deadline kills was recorded when it
// started, so it still counts.
func TestACallTheDeadlineKillsStillCountsAndTheLastStepRepeats(t *testing.T) {
	st := Install(t, Step{Delay: 20 * time.Second}, Step{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=index", "--format=csv,noheader,nounits").Output(); err == nil {
		t.Fatal("the first call is a hang: its deadline must have killed it")
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the killed call held its caller for %v", took)
	}
	for i := 0; i < 2; i++ {
		if _, err := exec.Command("nvidia-smi", "--query-gpu=index", "--format=csv,noheader,nounits").Output(); err != nil {
			t.Fatalf("call %d after the hang: %v", i+2, err)
		}
	}
	if n := len(st.Calls()); n != 3 {
		t.Errorf("%d calls recorded, want 3 (the killed one included)", n)
	}
}
