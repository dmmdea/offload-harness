package gpulease

// What a running lease's job has been OBSERVED to commit, and what that does to the part of its declaration that is still to load.
//
// pendingLoad charges a granted lease with declared less what its processes hold NOW. That is right while the job loads, and
// wrong once it has loaded and let go: a media lane that declared 36 GiB of commit, peaked at about 25 GiB while the weights moved
// onto its card and then fell back to 8 GiB was still charged 28 GiB, "still to load", for as long as it ran, and a second lane
// that needed the same room waited behind memory the first one had shown it would never take (the host RAM hold bound passed
// after 10 minutes and the second lane's card sat idle).
//
// The remedy is a floor under what the lease is credited with, taken from the job itself and only from it: the highest commit
// (private bytes) its process tree has been seen to hold, counted from the moment that figure has PLATEAUED. Until then the
// credit is the current reading alone, exactly as before, because a tree still loading may be on its way to the declared figure.
// A plateau is a run of samples, at least observedPlateauSamples of them spread over at least observedPlateauSpan, in which the
// commit never rose more than observedGrowthTolerance above where the run began; growth past that starts a new run and the credit
// falls back to the current reading. The peak is credited at most up to what was declared (pendingLoad never goes below zero), and
// a tree that cannot be read, or is gone, forgets its history, so nothing that was not seen just now is ever credited.
//
// The history lives in this process (the samples are taken by whoever is looking: a waiting `gpu reserve`, a card allocator, the
// fleet node), so a reader that has just started, such as `gpu status`, sees the conservative figure until it has watched the
// tree for the plateau span. Only the commit half is credited: a working set can be trimmed by the OS under pressure and say
// nothing about what the job will be charged.

import (
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// The plateau rule. Named so the rule reads as a rule: four spaced samples over a minute, no rise past half a GiB.
const (
	// observedPlateauSamples is how many consecutive samples (each at least observedSampleGap after the one before) a commit must hold
	// still for before its peak is credited.
	observedPlateauSamples = 4
	// observedPlateauSpan is how long that run must last, first sample to last.
	observedPlateauSpan = 60 * time.Second
	// observedGrowthTolerance is how far above the first sample of a run the commit may rise before the run starts over (GiB).
	observedGrowthTolerance = 0.5
	// observedSampleGap is the least time between two samples that count toward the run: a grant poll and a waiter poll a few
	// milliseconds apart are one look, not two.
	observedSampleGap = 2 * time.Second
	// observedForgetAfter drops the history of a lease nobody has looked at for this long (it ended, or nothing is waiting any more).
	observedForgetAfter = 6 * time.Hour
)

// observedKey is whose commit is being watched: a lease directory, the holder that lease is measured from, and the epoch of the
// lease that was asked about, so a later lease of a long-lived holder starts a fresh history.
type observedKey struct {
	root   string
	holder holderKey
	epoch  uint64
}

type observedRun struct {
	peak     float64   // the highest commit seen since the history began (GiB)
	runStart time.Time // first sample of the current run
	runBase  float64   // commit at the first sample of the current run
	runCount int       // spaced samples in the current run
	lastAt   time.Time // last sample that counted
	seenAt   time.Time // last time anything was sampled, for forgetting
}

var observed = struct {
	sync.Mutex
	runs map[observedKey]*observedRun
}{runs: map[observedKey]*observedRun{}}

// observedNow is the clock the samples are stamped with (the Manager's test seam, else the wall clock).
func (m *Manager) observedNow() time.Time {
	if m.observedClock != nil {
		return m.observedClock()
	}
	return time.Now()
}

// creditObserved takes one reading of a lease's job (held, ok: what heldOf measured) and returns the reading the pending
// arithmetic should use: held itself, with its commit raised to the observed peak once that peak has plateaued. A reading that
// could not be taken is returned as it came and wipes the history.
func (m *Manager) creditObserved(l Info, held gpuprobe.HostLoad, ok bool) gpuprobe.HostLoad {
	key := observedKey{root: m.root, holder: holderOf(l), epoch: l.Epoch}
	now := m.observedNow()
	observed.Lock()
	defer observed.Unlock()
	for k, r := range observed.runs {
		if now.Sub(r.seenAt) > observedForgetAfter {
			delete(observed.runs, k)
		}
	}
	if !ok {
		delete(observed.runs, key)
		return held
	}
	if peak, plateaued := sampleCommit(observed.runs, key, now, held.CommitGiB); plateaued && peak > held.CommitGiB {
		held.CommitGiB = peak
	}
	return held
}

// sampleCommit records one commit reading and says whether the peak has plateaued.
func sampleCommit(runs map[observedKey]*observedRun, key observedKey, now time.Time, commit float64) (peak float64, plateaued bool) {
	r := runs[key]
	if r == nil {
		r = &observedRun{peak: commit, runStart: now, runBase: commit, runCount: 1, lastAt: now}
		runs[key] = r
	} else {
		r.peak = max(r.peak, commit)
		switch {
		case commit > r.runBase+observedGrowthTolerance:
			r.runStart, r.runBase, r.runCount, r.lastAt = now, commit, 1, now
		case now.Sub(r.lastAt) >= observedSampleGap:
			r.runCount++
			r.lastAt = now
		}
	}
	r.seenAt = now
	return r.peak, r.runCount >= observedPlateauSamples && now.Sub(r.runStart) >= observedPlateauSpan
}
