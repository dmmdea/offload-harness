package fleetnode

// footprints.go is the measured-footprint store behind /fleet/health's
// model_footprints[] (~/.local-offload/footprints.json). Entries are MEASURED
// per-render peaks including our offload strategy: Record keeps the max
// observed GiB per (family, quant, task) key and stores vram_peak_gb = that
// RAW max observed peak, rounded to 0.1 — no node-side padding. The DISPATCHER
// owns all routing margin (CONTRACT v2.1 / ADR 0013): a node advertising its
// own ×1.2 on top of the dispatcher's margin double-inflated footprints and
// made wan2.2/hidream unroutable on a 16GB node. We never write a
// vram_peak_gb <= 0 entry (the contract has dispatchers ignore those).
//
// HOST peaks (G3 of the P0 plan). Beside the VRAM peak the store keeps, per key, the MAX private and the MAX
// resident memory (GiB) of the render's process tree and how many runs they come from, and the last hostRecentRuns
// runs' peaks (RecordHost / HostPeak). They are the measurement the host-RAM guard calibrates a declaration with
// (internal/pipeline: a render whose weights stream from RAM declares what its files add up to, raised to the
// hostPeakPercentile-th percentile of the recent runs' resident peaks, clamped, once at least hostPeakMinRuns
// (internal/pipeline) runs exist; it only raises). The percentile and the window, not the all-time maximum, are what the
// calibration reads (F56): one inflated run is not the answer. The maxima ride the wire as host_peak_gb (private)
// and host_resident_peak_gb (resident), additive and omitted when unmeasured. Two quantities on purpose: on
// Windows a process's GPU allocations may be charged to its private bytes and not appear in its working set,
// so recording only one would leave the question "which of these is RAM?" unanswerable.

import (
	"encoding/json"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// FootprintEntry is the wire shape /fleet/health advertises (contract v2).
type FootprintEntry struct {
	ModelFamily string  `json:"model_family"`
	Quant       string  `json:"quant,omitempty"`
	TaskType    string  `json:"task_type,omitempty"`
	VramPeakGiB float64 `json:"vram_peak_gb"`
	// HostPeakGiB / HostResidentPeakGiB are the measured peaks of the render's process tree (private bytes,
	// resident set). Additive: a reader that does not know them ignores them.
	HostPeakGiB         float64 `json:"host_peak_gb,omitempty"`
	HostResidentPeakGiB float64 `json:"host_resident_peak_gb,omitempty"`
}

// footprintRecord is the on-disk shape: the wire fields plus the raw
// observation state that lets max-keep survive restarts.
type footprintRecord struct {
	ModelFamily     string    `json:"model_family"`
	Quant           string    `json:"quant,omitempty"`
	TaskType        string    `json:"task_type,omitempty"`
	VramPeakGiB     float64   `json:"vram_peak_gb"`
	ObservedPeakGiB float64   `json:"observed_peak_gb"`
	Samples         int       `json:"samples"`
	Updated         time.Time `json:"updated"`
	// The host side: the max private and max resident peak of the render's process tree and the runs they
	// come from (RecordHost). Additive; an older store has none.
	HostPeakGiB         float64 `json:"host_peak_gb,omitempty"`
	HostResidentPeakGiB float64 `json:"host_resident_peak_gb,omitempty"`
	HostRuns            int     `json:"host_runs,omitempty"`
	// HostRecent is the last hostRecentRuns runs' peaks (the window the calibration's percentile is taken over). Additive: a
	// store written before it has none, and then the maxima stand in for the runs the window does not hold (HostPeak).
	HostRecent []hostRun `json:"host_recent,omitempty"`
}

type footprintKey struct {
	family, quant, task string
}

// Footprints is a concurrency-safe, file-backed footprint store. Every Record
// persists synchronously via an atomic temp+rename write; a corrupt or missing
// file opens as empty (logged once, never a crash) so a bad disk state can
// only cost history, never availability.
type Footprints struct {
	mu        sync.Mutex
	path      string
	entries   map[footprintKey]*footprintRecord
	loadedMod time.Time // mtime of the last load/merge (ReloadIfChanged gate)
}

// OpenFootprints loads the store at path (missing → empty; corrupt → empty
// with one log line).
func OpenFootprints(path string) *Footprints {
	f := &Footprints{path: path, entries: map[footprintKey]*footprintRecord{}}
	b, err := os.ReadFile(path)
	if err != nil {
		// Missing file is the normal first run; anything else is worth a line
		// but never fatal.
		if !os.IsNotExist(err) {
			log.Printf("footprints: cannot read %s (starting empty): %v", path, err)
		}
		return f
	}
	var recs []footprintRecord
	if err := json.Unmarshal(b, &recs); err != nil {
		log.Printf("footprints: corrupt store %s (starting empty): %v", path, err)
		return f
	}
	for i := range recs {
		r := recs[i]
		f.entries[footprintKey{r.ModelFamily, r.Quant, r.TaskType}] = &r
	}
	return f
}

// Path returns the store's backing file path (fleet-measure prints the
// on-disk records — vram_peak_gb plus the raw observed_peak_gb/samples state
// the Afterburner validation procedure compares against).
func (f *Footprints) Path() string {
	return f.path
}

// ReloadIfChanged re-reads the backing file when its mtime moved past the last
// load, MAX-MERGING disk records into memory (higher observed peak wins per
// key) so a record written by ANOTHER process — fleet-measure priming the
// store while fleet-serve is up was the live-found case — becomes visible to
// this process's Entries() without a restart, and an in-memory observation is
// never regressed by an older on-disk one. Cheap (one stat) when nothing
// changed; safe to call from the health accessor (no locks beyond the store's
// own, no spawns).
func (f *Footprints) ReloadIfChanged() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloadMergeLocked()
}

// reloadMergeLocked is the mtime-gated max-merge of the on-disk store into memory
// (higher observed peak wins per key). The caller holds f.mu. It runs on BOTH the
// read path (ReloadIfChanged, so health surfaces another process's records) AND
// the write path (Record, BEFORE persisting) — the latter is what stops a write
// from CLOBBERING a record another process just wrote: fleet-measure and the MCP
// share ~/.local-offload/footprints.json, and without this a Record would
// overwrite the file with only this process's in-memory entries (the live-found
// "only comfy-graph advertised"). A partially-written file mid-rename skips
// cleanly; the next call retries.
func (f *Footprints) reloadMergeLocked() {
	fi, err := os.Stat(f.path)
	if err != nil {
		return // missing/unreadable: nothing newer to merge
	}
	if !fi.ModTime().After(f.loadedMod) {
		return
	}
	f.loadedMod = fi.ModTime()
	b, err := os.ReadFile(f.path)
	if err != nil {
		return
	}
	var recs []footprintRecord
	if err := json.Unmarshal(b, &recs); err != nil {
		return // partially-written file mid-rename race: skip, next stat retries
	}
	for i := range recs {
		r := recs[i]
		k := footprintKey{r.ModelFamily, r.Quant, r.TaskType}
		cur, ok := f.entries[k]
		if !ok {
			f.entries[k] = &r
			continue
		}
		if r.ObservedPeakGiB > cur.ObservedPeakGiB {
			// The other process saw a higher VRAM peak: its VRAM side wins, whole. The host side below is
			// merged on its own, so a higher VRAM peak never drops a higher host peak (or the reverse).
			cur.VramPeakGiB, cur.ObservedPeakGiB, cur.Samples, cur.Updated = r.VramPeakGiB, r.ObservedPeakGiB, r.Samples, r.Updated
		}
		cur.HostPeakGiB = math.Max(cur.HostPeakGiB, r.HostPeakGiB)
		cur.HostResidentPeakGiB = math.Max(cur.HostResidentPeakGiB, r.HostResidentPeakGiB)
		if r.HostRuns > cur.HostRuns {
			// The other process has seen more runs, and its window holds them (and ours as far as it had read the file): take it
			// whole. RecordHost merges BEFORE it folds its own run, so the run it adds lands in the merged window.
			if len(r.HostRecent) > 0 {
				cur.HostRecent = r.HostRecent
			}
			cur.HostRuns = r.HostRuns
		}
	}
}

// roundToTenth rounds up-or-nearest to one decimal (12.34 → 12.3, 3.9996 → 4.0).
func roundToTenth(v float64) float64 {
	return math.Round(v*10) / 10
}

// Record folds one observed per-render peak (GiB) into the (family, quant,
// task) entry: max-keep on the observation, vram_peak_gb = raw max observed
// rounded 0.1 (no padding), samples++, then persist. Non-positive observations are dropped — a
// zero/negative "peak" is a sampling failure, and the contract forbids
// advertising vram_peak_gb <= 0.
func (f *Footprints) Record(family, quant, task string, observedGiB float64) {
	if observedGiB <= 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := footprintKey{family, quant, task}
	rec, ok := f.entries[key]
	if !ok {
		rec = &footprintRecord{ModelFamily: family, Quant: quant, TaskType: task}
		f.entries[key] = rec
	}
	if observedGiB > rec.ObservedPeakGiB {
		rec.ObservedPeakGiB = observedGiB
		rec.VramPeakGiB = roundToTenth(observedGiB) // RAW peak — the dispatcher owns all margin (ADR 0013)
	}
	rec.Samples++
	rec.Updated = time.Now().UTC()
	// Merge any record another process wrote since we loaded, BEFORE we persist —
	// otherwise this write overwrites the file with only our in-memory entries and
	// clobbers e.g. fleet-measure's records while the MCP is running (max-keep means
	// our just-folded observation still wins its own key unless disk saw higher).
	f.reloadMergeLocked()
	if err := f.persistLocked(); err != nil {
		log.Printf("footprints: persist %s failed (entry kept in memory): %v", f.path, err)
	}
}

// HostPeaks is what the renders of one key were measured to hold on the host: the private and the resident memory of
// the render's process tree (GiB).
//
// PrivateGiB and ResidentGiB are the CALIBRATION figures: the hostPeakPercentile-th percentile of the last hostRecentRuns runs'
// peaks, so one inflated run is not the answer and a bad run leaves the window after hostRecentRuns more (F56). The
// all-time maxima ride beside them (MaxPrivateGiB, MaxResidentGiB: what the wire still advertises as host_peak_gb and
// host_resident_peak_gb). Runs counts every run ever recorded and Window the ones the store still holds.
type HostPeaks struct {
	PrivateGiB, ResidentGiB       float64
	Runs                          int
	MaxPrivateGiB, MaxResidentGiB float64
	Window                        int
}

// hostRecentRuns is how many of a key's most recent host peaks the store keeps, and hostPeakPercentile (nearest rank, in
// percent) the order statistic the calibration reads from them. Both are CHOSEN, not measured: twenty runs are far more than
// the three the calibration waits for and still few enough that a bad run (a kept ComfyUI that was not under the sampled
// runner, a driver charging card memory to the process) is gone after a day of use; the 90th percentile of fewer than ten
// runs is their maximum (rank ceil(0.9 n) = n), so outlier rejection starts at ten runs, where there is something to reject
// an outlier against, and below that the calibration errs toward declaring more, as the all-time max always did.
const (
	hostRecentRuns     = 20
	hostPeakPercentile = 90
)

// HostPeakPercentile is hostPeakPercentile for the callers that word it (the media admission's declaration says which
// percentile it read).
const HostPeakPercentile = hostPeakPercentile

// hostRun is one run's host peaks, as the window keeps them.
type hostRun struct {
	Private  float64 `json:"private_gb"`
	Resident float64 `json:"resident_gb"`
}

// nearestRank is the p-th percentile (0 < p <= 100) of xs by the nearest-rank method; 0 for no samples.
func nearestRank(xs []float64, p int) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	rank := (n*p + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return s[rank-1]
}

// RecordHost folds one render's host peaks into the (family, quant, task) entry: the max of each, runs++, then
// persist. A non-positive private peak is a sampling failure (an unreadable process table, a runner that exited
// before the first tick) and is dropped, so a run that measured nothing is never counted as a run that held
// nothing. The resident peak is kept as the max of whatever was read.
func (f *Footprints) RecordHost(family, quant, task string, privateGiB, residentGiB float64) {
	if privateGiB <= 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// Merge what another process wrote BEFORE the run is folded in (the max-merge below does not care about the order, the
	// window does: merging after would replace our fresh run with the other process's window, which has not seen it).
	f.reloadMergeLocked()
	key := footprintKey{family, quant, task}
	rec, ok := f.entries[key]
	if !ok {
		rec = &footprintRecord{ModelFamily: family, Quant: quant, TaskType: task}
		f.entries[key] = rec
	}
	rec.HostPeakGiB = math.Max(rec.HostPeakGiB, roundToTenth(privateGiB))
	if residentGiB > 0 {
		rec.HostResidentPeakGiB = math.Max(rec.HostResidentPeakGiB, roundToTenth(residentGiB))
	}
	rec.HostRuns++
	run := hostRun{Private: roundToTenth(privateGiB)}
	if residentGiB > 0 {
		run.Resident = roundToTenth(residentGiB)
	}
	rec.HostRecent = append(rec.HostRecent, run)
	if n := len(rec.HostRecent); n > hostRecentRuns {
		rec.HostRecent = append([]hostRun(nil), rec.HostRecent[n-hostRecentRuns:]...)
	}
	rec.Updated = time.Now().UTC()
	f.reloadMergeLocked()
	if err := f.persistLocked(); err != nil {
		log.Printf("footprints: persist %s failed (entry kept in memory): %v", f.path, err)
	}
}

// HostPeak reports what the key's renders were measured to hold on the host, and false when none was measured.
func (f *Footprints) HostPeak(family, quant, task string) (HostPeaks, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloadMergeLocked()
	r, ok := f.entries[footprintKey{family, quant, task}]
	if !ok || r.HostPeakGiB <= 0 {
		return HostPeaks{}, false
	}
	var priv, res []float64
	for _, h := range r.HostRecent {
		priv = append(priv, h.Private)
		if h.Resident > 0 {
			res = append(res, h.Resident)
		}
	}
	// Runs the window does not hold because it never could (a store written before the window existed: it has the maxima and
	// a run count) are stood in for, once, by the maxima, so a legacy outlier keeps speaking until the window fills with real
	// runs. Runs that LEFT a full window are forgotten on purpose, and the window being full is how they are told apart.
	if len(r.HostRecent) < hostRecentRuns && r.HostRuns > len(r.HostRecent) {
		priv = append(priv, r.HostPeakGiB)
		if r.HostResidentPeakGiB > 0 {
			res = append(res, r.HostResidentPeakGiB)
		}
	}
	return HostPeaks{
		PrivateGiB:     nearestRank(priv, hostPeakPercentile),
		ResidentGiB:    nearestRank(res, hostPeakPercentile),
		Runs:           r.HostRuns,
		MaxPrivateGiB:  r.HostPeakGiB,
		MaxResidentGiB: r.HostResidentPeakGiB,
		Window:         len(r.HostRecent),
	}, true
}

// Entries returns the wire-shaped entries with vram_peak_gb > 0, sorted by
// (family, quant, task) for stable health output.
func (f *Footprints) Entries() []FootprintEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FootprintEntry, 0, len(f.entries))
	for _, r := range f.entries {
		if r.VramPeakGiB <= 0 {
			continue
		}
		out = append(out, FootprintEntry{
			ModelFamily:         r.ModelFamily,
			Quant:               r.Quant,
			TaskType:            r.TaskType,
			VramPeakGiB:         r.VramPeakGiB,
			HostPeakGiB:         r.HostPeakGiB,
			HostResidentPeakGiB: r.HostResidentPeakGiB,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.ModelFamily != b.ModelFamily {
			return a.ModelFamily < b.ModelFamily
		}
		if a.Quant != b.Quant {
			return a.Quant < b.Quant
		}
		return a.TaskType < b.TaskType
	})
	return out
}

// persistLocked writes the full store atomically (temp file in the same
// directory + rename, the pattern every store in this repo uses so a crash
// mid-write can never leave a torn footprints.json). Caller holds f.mu.
func (f *Footprints) persistLocked() error {
	recs := make([]footprintRecord, 0, len(f.entries))
	for _, r := range f.entries {
		recs = append(recs, *r)
	}
	sort.Slice(recs, func(i, j int) bool {
		a, b := recs[i], recs[j]
		if a.ModelFamily != b.ModelFamily {
			return a.ModelFamily < b.ModelFamily
		}
		if a.Quant != b.Quant {
			return a.Quant < b.Quant
		}
		return a.TaskType < b.TaskType
	})
	b, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.path), ".footprints-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, f.path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
