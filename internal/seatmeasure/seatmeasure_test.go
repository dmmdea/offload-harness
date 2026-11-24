package seatmeasure

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

type reading struct {
	avail, commit float64
	fail          bool
}

// scripted runs one measurement over a fixed series of readings on a fake clock (the sleep advances it by exactly the
// asked time and never waits), and finishes it once the sampler has read the last one.
func scripted(t *testing.T, script []reading) Result {
	t.Helper()
	var i int
	drained := make(chan struct{})
	clock := time.Unix(0, 0)
	read := func() (gpuprobe.HostMemory, bool) {
		r := script[i]
		i++
		if i == len(script) {
			close(drained)
		}
		return gpuprobe.HostMemory{AvailableGiB: r.avail, CommitUsedGiB: r.commit}, !r.fail
	}
	sleep := func(d time.Duration, stop <-chan struct{}) {
		if i >= len(script) {
			<-stop
			return
		}
		clock = clock.Add(d)
	}
	run := begin(read, 500*time.Millisecond, func() time.Time { return clock }, sleep)
	<-drained
	return run.Finish(true)
}

// A cold load that raises the commit by 54 GiB and lowers the available memory by 32 GiB reads as those peaks with one
// single-sample spike of +20 GiB on the commit (the desk) not counted, and a reading that fails makes the run unclean.
func TestPeaksRejectASingleSampleSpikeAndAFailedReadingIsNotClean(t *testing.T) {
	var script []reading
	for i := 0; i < 6; i++ { // the baseline: 100 GiB available, 80 GiB committed, with a little movement
		script = append(script, reading{avail: 100 + 0.1*float64(i%2), commit: 80 + 0.2*float64(i%3)})
	}
	for i := 1; i <= 40; i++ {
		f := math.Min(float64(i)/10, 1) // a ramp over ten ticks, then the plateau
		r := reading{avail: 100 - 32*f, commit: 80 + 54*f}
		if i == 30 {
			r.commit += 20 // one tick of the desk
		}
		script = append(script, r)
	}
	got := scripted(t, script)
	if !got.Clean {
		t.Fatalf("not clean: %s", got.Why)
	}
	if math.Abs(got.CommitPeakGiB-54) > 1 || math.Abs(got.TouchedPeakGiB-32) > 1 {
		t.Fatalf("peaks commit %.1f touched %.1f, want about 54 and 32 (the spike rejected)", got.CommitPeakGiB, got.TouchedPeakGiB)
	}

	script[6+20].fail = true
	if got := scripted(t, script); got.Clean || !strings.Contains(got.Why, "failed") {
		t.Fatalf("a failed reading left the run clean: %+v", got)
	}
}
