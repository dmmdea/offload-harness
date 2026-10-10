package main

// F24 review, root package: the patient reader run for real. cardTablePatient is what the verbs that
// decide from the table read through (gpu reserve, node-swap --cards, the drain scope, the wrapper's pin),
// and every test of it injects cardTableFn, so its production wiring (the default reader, the deadlines
// cardTableDeps hands the retry) never ran under test. Here cardTableFn stays at its default and the
// process behind it is a stand-in nvidia-smi (internal/gpuprobe/smitest: this test binary, first on PATH)
// that answers as slowly as a loaded box does. No card, no GPU, nothing rendered.

import (
	"context"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuprobe/smitest"
)

// The first call outlasts its deadline and is killed; the second takes 5.5 s: longer than the 5 s the base
// kept inside the default reader, shorter than the retry's default 15 s. A default reader that caps itself
// fails this on the retry, and one that ignores its context never lets the first call be killed.
func TestThePatientReaderRunsTheProductionReaderThroughOneSlowNvidiaSmi(t *testing.T) {
	st := smitest.Install(t, smitest.Step{Delay: 20 * time.Second}, smitest.Step{Delay: 5500 * time.Millisecond})
	oldFirst, oldRetry := cardReadFirst, cardReadRetry
	cardReadFirst, cardReadRetry = 3*time.Second, 0 // no retry deadline set: gpualloc's default 15 s
	t.Cleanup(func() { cardReadFirst, cardReadRetry = oldFirst, oldRetry })

	start := time.Now()
	cards, _, err := cardTablePatient(context.Background(), config.Config{})
	took := time.Since(start)
	if err != nil {
		t.Fatalf("one slow nvidia-smi must not leave the verbs that decide from the table without one: %v (after %v)", err, took.Round(time.Millisecond))
	}
	if len(cards) != 3 {
		t.Fatalf("%d cards, want the stand-in's 3", len(cards))
	}
	if calls := st.Calls(); len(calls) != 2 {
		t.Errorf("%d nvidia-smi calls %q, want exactly 2: the one that ran out and its retry", len(calls), calls)
	}
	if took < 8400*time.Millisecond {
		t.Errorf("the whole read took %v, want at least 8.5 s: the stand-in's delays were not served", took.Round(time.Millisecond))
	}
}
