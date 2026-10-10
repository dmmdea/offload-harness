package gpualloc

// F24 review: the card-table read run for real. Every other test of the retry injects a reader, so the
// production reader (the one Deps{} falls back to: the PATH lookup, exec.CommandContext, the kill at the
// deadline, the parse) never ran under test, and what makes the 15 s retry real, that no reader below
// CardTable adds a deadline of its own, rested on a comment. Here it runs against a stand-in nvidia-smi
// (internal/gpuprobe/smitest: this test binary, first on PATH) that answers as slowly as a loaded box does.
// No card, no GPU, nothing rendered.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuprobe/smitest"
)

// The first call outlasts its deadline and is killed (the deadline is generous enough for the stand-in to
// START and record itself first, even on a loaded box: a call killed before it started would not be counted);
// the second takes 5.5 s: longer than the 5 s the base kept inside the default reader, shorter than the
// retry's default 15 s. A reader that caps itself (or
// ignores the context it is given) fails this: the retry dies at the cap, or the first call is never killed.
func TestTheProductionReaderIsNotCappedBelowTheRetryDeadline(t *testing.T) {
	st := smitest.Install(t, smitest.Step{Delay: 20 * time.Second}, smitest.Step{Delay: 5500 * time.Millisecond})
	d := Deps{ReadDeadline: 3 * time.Second} // no Cards and no RetryDeadline: the production reader, the default 15 s
	start := time.Now()
	cards, _, err := d.CardTable(context.Background(), config.Config{})
	took := time.Since(start)
	if err != nil {
		t.Fatalf("a read that outlasts 5 s but not the retry's 15 s must answer: %v (after %v)", err, took.Round(time.Millisecond))
	}
	if len(cards) != 3 {
		t.Fatalf("%d cards, want the stand-in's 3", len(cards))
	}
	if calls := st.Calls(); len(calls) != 2 {
		t.Errorf("%d nvidia-smi calls %q, want exactly 2: the one that ran out and its retry", len(calls), calls)
	}
	// The stand-in really was slow (the first attempt's 3 s, then the retry's 5.5 s), so the pass above
	// was not a fast answer by accident.
	if took < 8400*time.Millisecond {
		t.Errorf("the whole read took %v, want at least 8.5 s: the stand-in's delays were not served", took.Round(time.Millisecond))
	}
}

// A nvidia-smi that never answers is killed at each deadline and the read says it was read twice: the kill
// is real (a process that outlives its context would hold the caller for the stand-in's 20 s).
func TestTheProductionReaderIsKilledAtEachDeadlineAndGivesUpAfterTwoAttempts(t *testing.T) {
	st := smitest.Install(t, smitest.Step{Delay: 20 * time.Second})
	d := Deps{ReadDeadline: 3 * time.Second, RetryDeadline: 5 * time.Second}
	start := time.Now()
	_, _, err := d.CardTable(context.Background(), config.Config{})
	took := time.Since(start)
	if err == nil {
		t.Fatal("a nvidia-smi that never answers is an error")
	}
	if !strings.Contains(err.Error(), "read twice: no answer within 3s, then none within 5s") {
		t.Errorf("the error must say how the table was tried: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the cause stays in the chain: %v", err)
	}
	if calls := st.Calls(); len(calls) != 2 {
		t.Errorf("%d nvidia-smi calls %q, want exactly 2", len(calls), calls)
	}
	if took > 15*time.Second {
		t.Errorf("the read took %v: a process that outlives its deadline held the caller", took.Round(time.Millisecond))
	}
}

// The allocation's own read is the per-device query alone. Asserted on what it EXECS, not on what it
// returns: the default foreign-busy reader sees nothing on a host with no foreign GPU process (every
// WDDM row is dropped, and so is any host where nothing else runs), so a check on its result passes with
// a process listing joined to the path. The stand-in records every invocation and refuses all but the
// per-device query, so a listing (--query-compute-apps, pmon, -q) shows up as a call here.
func TestTheAllocationNeverListsProcesses(t *testing.T) {
	st := smitest.Install(t)
	in, err := BuildInput(context.Background(), scratchManager(t), config.Config{}, Need{}, Deps{})
	if err != nil {
		t.Fatalf("the allocation's input must build against a nvidia-smi that answers: %v", err)
	}
	if len(in.Cards) != 3 {
		t.Fatalf("%d cards, want the stand-in's 3", len(in.Cards))
	}
	calls := st.Calls()
	if len(calls) != 1 {
		t.Fatalf("the allocation made %d nvidia-smi calls %q, want exactly the one per-device query", len(calls), calls)
	}
	for _, a := range calls[0] {
		low := strings.ToLower(a)
		if strings.Contains(low, "compute-apps") || strings.Contains(low, "pmon") || a == "-q" || a == "--query" {
			t.Errorf("the allocation's read lists processes: %q", calls[0])
		}
	}
	if !strings.HasPrefix(calls[0][0], "--query-gpu=") {
		t.Errorf("the allocation's read is %q, want the per-device --query-gpu query", calls[0])
	}
}
