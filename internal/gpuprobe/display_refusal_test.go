package gpuprobe

// When the per-device query is allowed to degrade to the one without display_attached.
//
// display_attached is the only signal that marks the operator's card while the screen sleeps
// (display_active reads Disabled on every card then), so dropping it is not free: for as long as
// it is dropped, the card the monitor is plugged into reads as an ordinary card. Two consequences
// are pinned here. The degrade is armed only when the driver REFUSES the field, not on any
// failure of the full query, so one transient nvidia-smi error cannot cost ten minutes of the
// protection. And a reading taken without the field says so (AttachedUnknown), so the allocator
// can decline to hand out a card it cannot vouch for instead of treating "unknown" as "not the
// monitor".

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// realRefusal is what nvidia-smi does with a field it does not know (measured, read-only query
// with a made-up field): the message goes to STDOUT, the exit status is 2, and the error the exec
// layer returns says nothing but the status. The text that names the field is in the output.
const realRefusal = "Field \"display_attached\" is not a valid field to query.\n\n"

var errExit2 = errors.New("exit status 2")

// transient is an nvidia-smi failure that has nothing to do with the field.
var errTransient = errors.New("exit status 9")

const transientOut = "Unable to determine the device handle for GPU0000:65:00.0: Unknown Error\n"

type smiLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *smiLog) printf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *smiLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func captureSmiLog(t *testing.T) *smiLog {
	t.Helper()
	l := &smiLog{}
	old := smiLogf
	smiLogf = l.printf
	t.Cleanup(func() { smiLogf = old })
	return l
}

// One transient failure of the full query, with the fallback working, used to arm a ten minute
// window in which the full query was never tried: at +9 minutes the driver was healthy again and
// the calls were still [false].
func TestATransientFailureOfTheFullQueryIsNotRemembered(t *testing.T) {
	clock := withAttachedClock(t)
	captureSmiLog(t)
	var calls []bool
	failOnce := true
	run := func(with bool) (string, error) {
		calls = append(calls, with)
		if with && failOnce {
			failOnce = false
			return transientOut, errTransient
		}
		if with {
			return "full", nil
		}
		return "legacy", nil
	}
	out, err := RunDisplayAware(run)
	if err != nil || out != "legacy" {
		t.Fatalf("the failing call still answers from the fallback: out=%q err=%v", out, err)
	}
	calls = nil
	clock.advance(9 * time.Minute)
	out, err = RunDisplayAware(run)
	if err != nil || out != "full" {
		t.Fatalf("nine minutes on, with the driver healthy, the full query must run again: out=%q err=%v calls=%v", out, err, calls)
	}
}

// The real refusal (message on stdout, status 2 as the whole error) is recognised and arms the
// window; the field is not asked for again until it elapses.
func TestARefusalNamingTheFieldArmsTheWindow(t *testing.T) {
	clock := withAttachedClock(t)
	lg := captureSmiLog(t)
	var calls []bool
	run := func(with bool) (string, error) {
		calls = append(calls, with)
		if with {
			return realRefusal, errExit2
		}
		return "legacy", nil
	}
	if _, err := RunDisplayAware(run); err != nil {
		t.Fatal(err)
	}
	calls = nil
	clock.advance(time.Minute)
	if _, err := RunDisplayAware(run); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] {
		t.Fatalf("inside the window only the fallback may run, calls = %v", calls)
	}
	logged := lg.all()
	if len(logged) != 1 || !strings.Contains(logged[0], "display_attached") || !strings.Contains(logged[0], "display_active") {
		t.Fatalf("the downgrade is logged once, naming the field and what the rule rests on: %q", logged)
	}
}

// A refusal can also arrive as the exec error's own text, or in an exec.ExitError's stderr; the
// recogniser reads all three carriers.
func TestRefusalIsRecognisedWhereverTheDriverSaysIt(t *testing.T) {
	cases := map[string]struct {
		out string
		err error
	}{
		"stdout":     {realRefusal, errExit2},
		"error text": {"", errors.New(`exit status 2: Field "display_attached" is not a valid field to query.`)},
		"invalid":    {"", errors.New("nvidia-smi: invalid field display_attached")},
	}
	for name, c := range cases {
		if !refusesAttached(c.out, c.err) {
			t.Errorf("%s: %q / %v was not recognised as the driver refusing display_attached", name, c.out, c.err)
		}
	}
	for name, c := range map[string]struct {
		out string
		err error
	}{
		"a transient":        {transientOut, errTransient},
		"another field":      {"Field \"bogus\" is not a valid field to query.\n", errExit2},
		"no output at all":   {"", errExit2},
		"field only in name": {"display_attached: 1 device busy\n", errExit2},
	} {
		if refusesAttached(c.out, c.err) {
			t.Errorf("%s: %q / %v was taken for a refusal of display_attached", name, c.out, c.err)
		}
	}
}

// An unexplained failure that keeps happening is, in effect, a driver that cannot answer the full
// query: after a few in a row the window is armed so a 2 s sampler stops paying a doomed call.
// A success in between starts the count again.
func TestRepeatedUnexplainedFailuresEventuallyArmTheWindow(t *testing.T) {
	withAttachedClock(t)
	lg := captureSmiLog(t)
	var calls []bool
	failing := true
	run := func(with bool) (string, error) {
		calls = append(calls, with)
		if with && failing {
			return transientOut, errTransient
		}
		return "ok", nil
	}
	fail := func(n int) {
		for i := 0; i < n; i++ {
			_, _ = RunDisplayAware(run)
		}
	}
	next := func() []bool {
		calls = nil
		_, _ = RunDisplayAware(run)
		return append([]bool(nil), calls...)
	}

	fail(attachedRefusalLimit - 1)
	failing = false
	fail(1) // a success: the count restarts
	failing = true
	fail(attachedRefusalLimit - 1)
	if got := next(); len(got) == 0 || !got[0] {
		t.Fatalf("one success in between must restart the count, but the full query was skipped: %v", got)
	}
	// That call was the limit-th failure in a row: the window is armed now.
	if got := next(); len(got) != 1 || got[0] {
		t.Fatalf("after %d unexplained failures in a row only the fallback may run, calls = %v", attachedRefusalLimit, got)
	}
	if got := lg.all(); len(got) != 1 {
		t.Errorf("arming is logged exactly once, got %q", got)
	}
}

// What a reading knows about display_attached, for a caller that has to decide whether to trust
// "this card is not the monitor".
func TestTheReportSaysWhatTheReadingKnowsAboutTheField(t *testing.T) {
	withAttachedClock(t)
	captureSmiLog(t)
	_, st, _ := RunDisplayAwareReport(func(bool) (string, error) { return "full", nil })
	if st != AttachedRead {
		t.Errorf("a healthy full query: state = %v, want AttachedRead", st)
	}
	_, st, _ = RunDisplayAwareReport(func(with bool) (string, error) {
		if with {
			return transientOut, errTransient
		}
		return "legacy", nil
	})
	if st != AttachedUnknown {
		t.Errorf("a transient failure of the full query: state = %v, want AttachedUnknown (the reading has no display_attached)", st)
	}
	resetAttachedGate()
	_, st, _ = RunDisplayAwareReport(func(with bool) (string, error) {
		if with {
			return realRefusal, errExit2
		}
		return "legacy", nil
	})
	if st != AttachedUnsupported {
		t.Errorf("the driver refused the field: state = %v, want AttachedUnsupported (the rule rests on display_active, as before)", st)
	}
	_, st, _ = RunDisplayAwareReport(func(bool) (string, error) { return "legacy", nil })
	if st != AttachedUnsupported {
		t.Errorf("inside the armed window: state = %v, want AttachedUnsupported", st)
	}
}

// Read marks every device of a reading that carries no display_attached because the full query
// failed for another reason; BuildCards carries the mark onto the card table.
func TestAReadingWithoutTheFieldMarksItsCardsDisplayUnknown(t *testing.T) {
	withAttachedClock(t)
	captureSmiLog(t)
	row := func(attached string) string {
		line := "0, GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee, NVIDIA GeForce RTX 5060 Ti, 16311, 900, 0, Disabled"
		if attached != "" {
			line += ", " + attached
		}
		return line + "\n"
	}
	devs, err := readDisplayAware(func(with bool) (string, error) {
		if with {
			return transientOut, errTransient
		}
		return row(""), nil
	})
	if err != nil || len(devs) != 1 || !devs[0].AttachedUnknown {
		t.Fatalf("devs=%+v err=%v, want one device marked AttachedUnknown", devs, err)
	}
	cards, _ := BuildCards(devs, "")
	if len(cards) != 1 || !cards[0].DisplayUnknown {
		t.Fatalf("cards = %+v, want the mark carried onto the card", cards)
	}

	resetAttachedGate()
	devs, err = readDisplayAware(func(bool) (string, error) { return row("No"), nil })
	if err != nil || len(devs) != 1 || devs[0].AttachedUnknown {
		t.Fatalf("a full reading is known: devs=%+v err=%v", devs, err)
	}
	if cards, _ = BuildCards(devs, ""); cards[0].DisplayUnknown {
		t.Errorf("a full reading must not mark its cards: %+v", cards)
	}
}
