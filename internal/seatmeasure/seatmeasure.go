// Package seatmeasure measures what one cold load of the agent seat costs the host's memory.
//
// The seat's host need used to be two numbers an operator typed into the config (agent_seat_host_ram_gib and
// agent_seat_host_commit_gib). On the reference 3-card Windows box three cold loads of the vLLM seat raised the
// system commit charge by 55.3, 53.4 and 54.0 GiB at the peak and lowered the available memory by 32.2, 30.7 and
// 33.4 GiB at the peak, in 281 to 399 s, against a typed seed of 36 and 61: the seat's need is a thing to read, not
// to guess. A Run reads the host's memory before the load and every tick during it and answers with the two peaks.
//
// The reading is the host's, not the seat's: nothing here knows which process grew. That is why a result is only
// clean when the load stands out of the host's own movement. The desk's commit charge moves by several GiB a
// minute on that box, so a single raw sample is not a peak:
//   - the baseline is the median of six readings 100 ms apart, and its spread (max less min) is the noise;
//   - a peak is taken on the median of the last five samples, so a one-tick spike of the desk is not one;
//   - a load whose commit peak is under three times the noise was not seen above the host's own movement.
//
// The package imports gpuprobe only. Where a clean result is recorded, and when, is the caller's (internal/pipeline,
// seatmeasure.go).
package seatmeasure

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

const (
	// baselineReads and baselineEvery: the baseline is the median of this many readings this far apart (0.6 s in all,
	// short against a load of 281 to 399 s measured on the reference box).
	baselineReads = 6
	baselineEvery = 100 * time.Millisecond
	// window is how many samples the rolling median covers. Chosen, not measured: five is the fewest in which one
	// spiked tick cannot be the median, and at 500 ms a tick it spans 2.5 s, far under a load's minutes.
	window = 5
	// minSamples is the fewest samples a clean result rests on (5 s at 500 ms).
	minSamples = 10
	// gapFactor: a gap between two samples above this many intervals means the process was suspended or starved, and
	// the samples around it say nothing of the host in between.
	gapFactor = 5
	// noiseFactor: the commit peak must exceed this many times the baseline noise to count as seen.
	noiseFactor = 3
	// minCommitGiB: a load that raised the commit by less than this was not a cold load of a model seat, whatever the noise
	// says. Chosen, not measured: a 7 s read of an idle desk on the reference box showed a 0.13 GiB commit peak over 0.04 GiB
	// of noise, which the noise rule alone passes, and a few such records would pull the store's 90th percentile down.
	minCommitGiB = 1.0
)

// Result is what one measured load cost the host, in GiB.
type Result struct {
	// TouchedPeakGiB is the baseline available memory less the lowest rolling-median available memory of the load.
	TouchedPeakGiB float64
	// CommitPeakGiB is the highest rolling-median commit charge of the load less the baseline commit charge.
	CommitPeakGiB float64
	// NoiseGiB is the larger of the baseline's spreads (available, commit): the host's own movement.
	NoiseGiB float64
	Samples  int
	Elapsed  time.Duration
	// Clean says the figures may be recorded; Why says what stopped them when it is false.
	Clean bool
	Why   string
}

// Run is one measurement in progress. Begin starts it, Finish ends it.
type Run struct {
	read  func() (gpuprobe.HostMemory, bool)
	every time.Duration
	now   func() time.Time
	// sleep waits d or until stop closes. A field so a test can drive the sampler without real time.
	sleep func(d time.Duration, stop <-chan struct{})

	baseAvail, baseCommit float64
	noise                 float64
	baseFailed            bool
	started               time.Time

	stop chan struct{}
	done chan struct{}
	once sync.Once

	// Written by the sampler goroutine only, read after done closes.
	samples             int
	failed              bool
	gapped              bool
	minAvail, maxCommit float64
	haveWindow          bool
	lastAt              time.Time
	availWin, commitWin []float64
}

// Begin takes the baseline and starts sampling every `every` (the harness passes 500 ms). It never returns nil and
// never blocks beyond the 0.6 s the baseline takes. A baseline that cannot be read makes the run unclean, not absent.
func Begin(read func() (gpuprobe.HostMemory, bool), every time.Duration) *Run {
	return begin(read, every, time.Now, realSleep)
}

func realSleep(d time.Duration, stop <-chan struct{}) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-stop:
	}
}

func begin(read func() (gpuprobe.HostMemory, bool), every time.Duration, now func() time.Time, sleep func(time.Duration, <-chan struct{})) *Run {
	if every <= 0 {
		every = 500 * time.Millisecond
	}
	r := &Run{read: read, every: every, now: now, sleep: sleep, stop: make(chan struct{}), done: make(chan struct{})}
	var avail, commit []float64
	for i := 0; i < baselineReads; i++ {
		if i > 0 {
			sleep(baselineEvery, nil)
		}
		m, ok := read()
		if !ok {
			r.baseFailed = true
			break
		}
		avail, commit = append(avail, m.AvailableGiB), append(commit, m.CommitUsedGiB)
	}
	r.started = now()
	if r.baseFailed {
		close(r.done)
		return r
	}
	r.baseAvail, r.baseCommit = median(avail), median(commit)
	r.noise = max(spread(avail), spread(commit))
	r.minAvail, r.maxCommit = r.baseAvail, r.baseCommit
	r.lastAt = r.started
	go r.loop()
	return r
}

func (r *Run) loop() {
	defer close(r.done)
	for {
		r.sleep(r.every, r.stop)
		select {
		case <-r.stop:
			return
		default:
		}
		m, ok := r.read()
		at := r.now()
		if !ok {
			r.failed = true
			continue
		}
		if at.Sub(r.lastAt) > gapFactor*r.every {
			r.gapped = true
		}
		r.lastAt = at
		r.samples++
		r.availWin, r.commitWin = push(r.availWin, m.AvailableGiB), push(r.commitWin, m.CommitUsedGiB)
		if len(r.availWin) < window {
			continue
		}
		r.minAvail = min(r.minAvail, median(r.availWin))
		r.maxCommit = max(r.maxCommit, median(r.commitWin))
		r.haveWindow = true
	}
}

// Finish stops the sampler, waits for it and answers. loaded says the load completed: a load that did not is no
// measurement of a cold load. Finish may be called more than once; every call answers the same.
func (r *Run) Finish(loaded bool) Result {
	r.once.Do(func() { close(r.stop) })
	<-r.done
	res := Result{NoiseGiB: r.noise, Samples: r.samples, Elapsed: r.now().Sub(r.started)}
	if r.baseFailed {
		res.Why = "the host's memory could not be read for the baseline"
		return res
	}
	if r.haveWindow {
		res.TouchedPeakGiB = max(r.baseAvail-r.minAvail, 0)
		res.CommitPeakGiB = max(r.maxCommit-r.baseCommit, 0)
	}
	switch {
	case !loaded:
		res.Why = "the load did not complete"
	case r.failed:
		res.Why = "a reading of the host's memory failed during the load"
	case r.gapped:
		res.Why = fmt.Sprintf("a gap between two samples above %d times the %s interval: the process was suspended or starved", gapFactor, r.every)
	case r.samples < minSamples:
		res.Why = fmt.Sprintf("%d samples, fewer than %d", r.samples, minSamples)
	case res.CommitPeakGiB < max(noiseFactor*r.noise, minCommitGiB):
		res.Why = "the load was not seen above the host's own movement"
	default:
		res.Clean = true
	}
	return res
}

// push appends v to a window of the last `window` values.
func push(w []float64, v float64) []float64 {
	w = append(w, v)
	if len(w) > window {
		w = w[len(w)-window:]
	}
	return w
}

func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func spread(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	lo, hi := xs[0], xs[0]
	for _, x := range xs {
		lo, hi = min(lo, x), max(hi, x)
	}
	return hi - lo
}
