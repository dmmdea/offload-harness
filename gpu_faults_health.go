package main

// gpu_faults_health.go: the driver fault summary fleet-serve publishes on /fleet/health as gpu_faults, writes to
// <state root>/gpu/faults-summary.json, and retains incidents from (internal/gpuactivity/faultstore.go).
//
// The read is `gpu faults`' own (30 days of the driver log, WER's live dumps on Windows, the restart and sleep
// records, nvidia-smi's card table): about 3 s of hidden child processes on the reference box, far too slow for a
// health call. So health answers from memory and a stale answer starts one background refresh, as the lane backends
// do (internal/fleetnode/activebackend.go): a reading is fresh for 10 minutes and published for 40, and nothing waits
// on a refresh. A refresh that fails keeps the last good reading until that ages out. Each refresh also appends the
// incidents that ended to the retention file and rewrites the summary file: fleet-serve is their one writer.

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuactivity"
)

const (
	// gpuFaultsTTL is how long a reading is fresh; gpuFaultsMaxStale how long it is still published.
	gpuFaultsTTL      = 10 * time.Minute
	gpuFaultsMaxStale = 4 * gpuFaultsTTL
	// gpuFaultsBudget bounds one refresh: three log reads of at most 20 s each, and the card table.
	gpuFaultsBudget = 90 * time.Second
)

// readFaultsRetainedFn is the report reader, a variable so a test serves a synthetic history.
var readFaultsRetainedFn = gpuactivity.ReadFaultsRetained

// gpuFaultsHealthReporter is what fleet-serve hands fleetnode.Options.GpuFaults. The first call starts the first
// read and answers nil (no reading yet), as does a node with no NVIDIA card or no driver log. It returns a copy.
func gpuFaultsHealthReporter(stateDir string) func() *gpuactivity.FaultSummary {
	var (
		mu       sync.Mutex
		val      *gpuactivity.FaultSummary
		valAt    time.Time
		tried    time.Time
		inflight bool
	)
	refresh := func() {
		s, ok := refreshGpuFaults(stateDir, time.Now())
		mu.Lock()
		defer mu.Unlock()
		inflight = false
		if ok {
			val, valAt = s, time.Now()
		}
	}
	return func() *gpuactivity.FaultSummary {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		if (tried.IsZero() || now.Sub(tried) > gpuFaultsTTL) && !inflight {
			inflight, tried = true, now
			go refresh()
		}
		if val == nil || now.Sub(valAt) > gpuFaultsMaxStale {
			return nil
		}
		c := *val
		c.Cards = append([]gpuactivity.FaultSummaryCard(nil), val.Cards...)
		c.Repeats = append([]gpuactivity.FaultSummaryRepeat(nil), val.Repeats...)
		return &c
	}
}

// refreshGpuFaults is one read: retained incidents in, the report, the incidents that ended out to the retention
// file, the summary file rewritten. ok is false when the driver log could not be read at all (keep the last
// reading); a nil summary with ok true is a node with no NVIDIA card to report on.
func refreshGpuFaults(stateDir string, now time.Time) (*gpuactivity.FaultSummary, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), gpuFaultsBudget)
	defer cancel()
	var notes []string
	store, serr := gpuactivity.FaultStorePath(stateDir)
	var retained []gpuactivity.Incident
	if serr != nil {
		notes = append(notes, "retention off: "+serr.Error())
	} else {
		r, bad, lerr := gpuactivity.LoadRetained(store)
		if lerr != nil {
			notes = append(notes, "retention unreadable: "+lerr.Error())
		}
		if bad > 0 {
			notes = append(notes, "retention: lines that do not parse were skipped")
		}
		retained = r
	}
	rep, err := readFaultsRetainedFn(ctx, gpuactivity.FaultWindowMax, retained)
	if err != nil {
		return nil, false
	}
	if len(rep.Cards) == 0 && rep.Events == 0 {
		return nil, true
	}
	if serr == nil {
		if _, werr := gpuactivity.RecordIncidents(store, rep, retained, now); werr != nil {
			notes = append(notes, "retention not written: "+werr.Error())
		}
	}
	s := gpuactivity.Summarize(rep, now)
	if path, perr := gpuactivity.FaultSummaryPath(stateDir); perr == nil {
		if werr := gpuactivity.WriteSummary(path, s); werr != nil {
			notes = append(notes, "summary file not written: "+werr.Error())
		}
	}
	if len(notes) > 0 {
		if s.Note != "" {
			notes = append([]string{s.Note}, notes...)
		}
		s.Note = strings.Join(notes, "; ")
	}
	return &s, true
}
