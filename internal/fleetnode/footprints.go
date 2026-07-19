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
	fi, err := os.Stat(f.path)
	if err != nil {
		return // missing/unreadable: nothing newer to merge
	}
	f.mu.Lock()
	defer f.mu.Unlock()
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
		if cur, ok := f.entries[k]; !ok || r.ObservedPeakGiB > cur.ObservedPeakGiB {
			f.entries[k] = &r
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
	if err := f.persistLocked(); err != nil {
		log.Printf("footprints: persist %s failed (entry kept in memory): %v", f.path, err)
	}
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
			ModelFamily: r.ModelFamily,
			Quant:       r.Quant,
			TaskType:    r.TaskType,
			VramPeakGiB: r.VramPeakGiB,
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
