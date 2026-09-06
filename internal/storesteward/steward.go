// Package storesteward keeps a persistent KV page store under a disk budget the
// box computes, pruning oldest-first BETWEEN the fleet node's turns.
//
// The store it exists for: LMCache fs_native pages the production seat writes
// over SMB into a dataset on the node that owns the disk. LMCache's own
// eviction counts only pages the RUNNING MP server wrote (upstream F10), and
// the seat wrapper's prune runs at seat start only — so under real fan-out
// the store filled 28 → 99 GB against a 100 GB quota in 75 minutes
// (2026-09-06, ~1 GB/min at peak), and a ZFS dataset AT its quota can refuse
// the very deletes that would free it. The cap therefore has to be enforced by
// something that runs while the seat runs, on the box that holds the disk,
// without a scheduler: the fleet node already takes turns (jobs, health polls),
// and each turn is a chance to look.
//
// Budget: cap = min(configured ceiling, 0.8 × (used + free)) — the store may
// use what the volume can spare, never more than the operator allowed; with no
// ceiling configured the volume rule alone applies. high = 95 % of cap, low =
// 85 %: above high the steward prunes oldest-first (mtime) down to low, so a
// prune buys a real gap instead of running every turn. Files younger than
// MinAge are never removed — a page still being written over SMB has a fresh
// mtime and no reader yet.
//
// A tick is one directory scan plus at most one prune, and at most one tick
// runs at a time: a health poll that arrives mid-scan reads the last status
// and does not start a second walk. Errors are carried in Status.Error, never
// swallowed: a store that cannot be scanned is advertised as such.
package storesteward

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	// BudgetFraction is the share of (used + free) the store may occupy.
	BudgetFraction = 0.8
	// HighFraction of cap triggers a prune; LowFraction is the prune target.
	HighFraction = 0.95
	LowFraction  = 0.85
	// MinAge protects pages still being written (a fresh mtime, no reader yet).
	MinAge = 60 * time.Second
	// DefaultEveryJobs is how many completed fleet jobs pass between ticks
	// when the config sets nothing: one spread.
	DefaultEveryJobs = 8
	// MarkerFile names the file that proves a directory IS a KV page store the
	// steward may prune. Validate writes it into an EMPTY root; a populated
	// root without it is refused — a mistyped fleet_store_root (a parent of the
	// store, any other populated tree) must never become an oldest-first purge.
	MarkerFile = ".storesteward"
)

// Status is what /fleet/health publishes under "store" and what offload_status
// prints. All sizes are decimal gigabytes, the unit every store number in the
// ledger uses.
type Status struct {
	Root          string  `json:"root"`
	UsedGB        float64 `json:"used_gb"`
	CapGB         float64 `json:"cap_gb"`
	HighGB        float64 `json:"high_gb"`
	LowGB         float64 `json:"low_gb"`
	Files         int     `json:"files"`
	LastScan      string  `json:"last_scan,omitempty"`
	LastPrune     string  `json:"last_prune,omitempty"`
	LastRemoved   int     `json:"last_removed"`
	LastFreedGB   float64 `json:"last_freed_gb"`
	Prunes        int     `json:"prunes"`
	JobsSinceTick int     `json:"jobs_since_tick"`
	Error         string  `json:"error,omitempty"`
}

// File is one regular file under the store root as the scan saw it.
type File struct {
	Path  string
	Size  int64
	MTime time.Time
}

// Steward owns one store root. Zero-value is not usable; use New.
type Steward struct {
	root      string
	capBytes  int64 // configured ceiling; <= 0 = volume rule only
	everyJobs int

	// seams for tests: free-space source and clock.
	free func(path string) (uint64, error)
	now  func() time.Time
	// logf receives one line per removed file (nil = silent). The fleet node
	// wires log.Printf so a prune is forensically recoverable from the journal.
	logf func(string, ...any)

	mu      sync.Mutex
	running bool
	jobs    int
	st      Status
}

// New builds a steward for root with capGB as the operator's ceiling (0 = no
// ceiling beyond the volume rule) and everyJobs completed jobs per tick
// (<= 0 = DefaultEveryJobs). free reports the volume's available bytes; nil
// uses the OS.
func New(root string, capGB float64, everyJobs int, free func(string) (uint64, error)) *Steward {
	if everyJobs <= 0 {
		everyJobs = DefaultEveryJobs
	}
	s := &Steward{root: root, capBytes: int64(capGB * 1e9), everyJobs: everyJobs, free: free, now: time.Now}
	s.st.Root = root
	return s
}

// SetLogf sets where per-file removal lines go (nil = silent).
func (s *Steward) SetLogf(logf func(string, ...any)) { s.logf = logf }

// Budget computes the cap and its watermarks from what the volume holds and
// can spare. Pure: used and free are the caller's numbers.
func Budget(configuredCap int64, used, free uint64) (capBytes, high, low int64) {
	vol := int64(float64(used+free) * BudgetFraction)
	capBytes = vol
	if configuredCap > 0 && configuredCap < vol {
		capBytes = configuredCap
	}
	return capBytes, int64(float64(capBytes) * HighFraction), int64(float64(capBytes) * LowFraction)
}

// Scan lists every regular file under root (recursive) with size and mtime.
func Scan(root string) ([]File, int64, error) {
	var files []File
	var used int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() || d.Name() == MarkerFile {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files = append(files, File{Path: p, Size: info.Size(), MTime: info.ModTime()})
		used += info.Size()
		return nil
	})
	return files, used, err
}

// Prune removes the oldest files (by mtime) until used <= target, skipping any
// file younger than minAge at now. It returns how many it removed and how many
// bytes that freed; a file that cannot be removed is skipped and the first such
// error is returned after the pass (the pass does not stop on it — the point is
// to get under the mark, and the error is reported, not hidden).
func Prune(files []File, used, target int64, now time.Time, minAge time.Duration, logf func(string, ...any)) (removed int, freed int64, err error) {
	if used <= target {
		return 0, 0, nil
	}
	sorted := make([]File, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].MTime.Before(sorted[j].MTime) })
	for _, f := range sorted {
		if used-freed <= target {
			break
		}
		if now.Sub(f.MTime) < minAge {
			continue // still being written; also everything after it is newer
		}
		if rerr := os.Remove(f.Path); rerr != nil {
			if err == nil {
				err = rerr
			}
			continue
		}
		removed++
		freed += f.Size
		if logf != nil {
			logf("storesteward: removed %s (%d bytes, mtime %s)", f.Path, f.Size, f.MTime.UTC().Format(time.RFC3339))
		}
	}
	return removed, freed, err
}

// Tick runs one scan and, when the store is above its high mark, one prune to
// the low mark. It returns the resulting status. At most one Tick runs at a
// time; a concurrent caller gets the last status back without scanning.
func (s *Steward) Tick() Status {
	s.mu.Lock()
	if s.running {
		st := s.st
		s.mu.Unlock()
		return st
	}
	s.running = true
	s.jobs = 0
	s.mu.Unlock()

	st := s.tick()

	s.mu.Lock()
	s.running = false
	st.JobsSinceTick = s.jobs // jobs that completed during the scan are not lost
	s.st = st
	s.mu.Unlock()
	return st
}

func (s *Steward) tick() Status {
	s.mu.Lock()
	st := s.st
	s.mu.Unlock()
	st.Root = s.root
	st.Error = ""
	now := s.now()
	st.LastScan = now.UTC().Format(time.RFC3339)

	files, used, err := Scan(s.root)
	if err != nil {
		st.Error = "scan: " + err.Error()
		return st
	}
	free, err := s.freeBytes()
	if err != nil {
		st.Error = "free space: " + err.Error()
		return st
	}
	capB, high, low := Budget(s.capBytes, uint64(used), free)
	st.UsedGB, st.CapGB, st.HighGB, st.LowGB = gb(used), gb(capB), gb(high), gb(low)
	st.Files = len(files)
	if used <= high {
		return st
	}
	removed, freed, perr := Prune(files, used, low, now, MinAge, s.logf)
	st.Prunes++
	st.LastPrune = now.UTC().Format(time.RFC3339)
	st.LastRemoved = removed
	st.LastFreedGB = gb(freed)
	st.UsedGB = gb(used - freed)
	st.Files = len(files) - removed
	if perr != nil {
		st.Error = "prune: " + perr.Error()
	}
	return st
}

func (s *Steward) freeBytes() (uint64, error) {
	if s.free != nil {
		return s.free(s.root)
	}
	return osFree(s.root)
}

// JobDone counts one completed fleet job; every everyJobs of them it runs a
// Tick in the background (never on the caller's goroutine — the job store's
// finish path must not walk a directory). It reports whether a tick was started.
func (s *Steward) JobDone() bool {
	s.mu.Lock()
	s.jobs++
	s.st.JobsSinceTick = s.jobs
	due := s.jobs >= s.everyJobs && !s.running
	s.mu.Unlock()
	if due {
		go s.Tick()
	}
	return due
}

// Status returns the last known status without scanning. When the last scan
// found the store above its high mark, a background Tick is started so a
// health poll acts as a turn too — the caller still gets an immediate answer.
func (s *Steward) Status() Status {
	s.mu.Lock()
	st := s.st
	kick := !s.running && st.CapGB > 0 && st.UsedGB > st.HighGB
	s.mu.Unlock()
	if kick {
		go s.Tick()
	}
	return st
}

// ErrNoRoot is returned by Validate when the store root does not exist;
// ErrNoMarker when it holds files but not the MarkerFile.
var (
	ErrNoRoot   = errors.New("store root does not exist")
	ErrNoMarker = errors.New("store root holds files but no " + MarkerFile + " marker: create that file in the directory to confirm it is a KV page store the steward may prune")
)

// Validate checks that root is a directory the steward may prune: it must
// exist, and it must carry MarkerFile. An EMPTY root gets the marker written
// (a fresh store); a populated root without it is refused — that is the
// mistyped-config case, and the only answer is an operator creating the
// marker on purpose. Called once at fleet-serve start so the refusal is loud
// there, never silent in a tick.
func Validate(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		return ErrNoRoot
	}
	if !info.IsDir() {
		return errors.New("store root is not a directory")
	}
	marker := filepath.Join(root, MarkerFile)
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return ErrNoMarker
	}
	return os.WriteFile(marker, []byte("KV page store managed by local-offload storesteward; delete this file to stop the steward pruning here\n"), 0o644)
}

func gb(b int64) float64 { return float64(b) / 1e9 }
