package gpuactivity

// faultstore.go: retention of fault incidents, and the per-card summary a fleet node publishes.
//
// RETENTION. The OS log is not a record: the Windows System log rolls over at its size cap and a Linux journal is
// vacuumed, so a month of incidents can thin out under a storm. fleet-serve (the one writer) appends each incident
// that has ended to <state root>/gpu/faults.jsonl, one JSON line per incident, and `gpu faults` and the summary merge
// the retained ones the log no longer holds (ReadFaultsRetained). Append-only, never pruned: the reference box made
// 19 incidents in 30 days, a line each.
//
// THE SUMMARY is what /fleet/health carries as gpu_faults, and what fleet-serve writes to
// <state root>/gpu/faults-summary.json (whole file, temp and rename) for this machine's short-lived processes, which
// must not pay a 30-day log read per call. Facts, as the report is: each card's counts and NVSentinel's repeat rule
// where it is met. What placement does with a repeat is ADR 0096's.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// FaultStorePath is the retention file under the machine's state root (config state_dir).
func FaultStorePath(stateDir string) (string, error) {
	root, err := gpulease.ResolveStateRoot(stateDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "gpu", "faults.jsonl"), nil
}

// FaultSummaryPath is the summary file under the machine's state root.
func FaultSummaryPath(stateDir string) (string, error) {
	root, err := gpulease.ResolveStateRoot(stateDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "gpu", "faults-summary.json"), nil
}

// LoadRetained reads the retained incidents. A missing file is none; a line that does not parse is skipped and
// counted in bad.
func LoadRetained(path string) (incs []Incident, bad int, err error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var in Incident
		if json.Unmarshal(sc.Bytes(), &in) != nil || in.Start.IsZero() {
			bad++
			continue
		}
		in.Retained = false
		incs = append(incs, in)
	}
	return incs, bad, sc.Err()
}

// RecordIncidents appends the report's incidents that were read from the log, have ended (no record for longer than
// a burst gap and a minute, so a burst still running is not cut in two), and overlap no retained one. It returns how
// many it appended. One writer only: fleet-serve's refresh.
func RecordIncidents(path string, rep FaultReport, retained []Incident, now time.Time) (int, error) {
	var add []Incident
	for _, in := range rep.Incidents {
		if in.Retained || now.Sub(in.End) <= faultBurstGap+time.Minute {
			continue
		}
		dup := false
		for _, r := range retained {
			if !in.Start.After(r.End) && !in.End.Before(r.Start) {
				dup = true
				break
			}
		}
		if !dup {
			add = append(add, in)
		}
	}
	if len(add) == 0 {
		return 0, nil
	}
	// Oldest first in the file, as they happened.
	for i, j := 0, len(add)-1; i < j; i, j = i+1, j-1 {
		add[i], add[j] = add[j], add[i]
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, in := range add {
		b, err := json.Marshal(in)
		if err != nil {
			continue
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			f.Close()
			return n, err
		}
		n++
	}
	return n, f.Close()
}

// FaultSummary is the per-card fault reading a fleet node publishes (gpu_faults on /fleet/health).
type FaultSummary struct {
	WindowDays int                  `json:"window_days"`
	ReadAt     time.Time            `json:"read_at"`
	Cards      []FaultSummaryCard   `json:"cards"`
	Repeats    []FaultSummaryRepeat `json:"repeats,omitempty"`
	// Note says why the reading may be short (a log read that stopped, a card table that could not be read).
	Note string `json:"note,omitempty"`
}

// FaultSummaryCard is one card's counts over the window.
type FaultSummaryCard struct {
	Index int    `json:"index"`
	UUID  string `json:"uuid"`
	Bus   string `json:"bus"`
	// Events and Incidents count every record and burst naming the card; InContext the bursts at a restart or sleep
	// transition among them. Incidents24h counts the bursts outside one that started in the last 24 h, and
	// LastIncident is when the newest of those ended.
	Events       int       `json:"events"`
	Incidents    int       `json:"incidents"`
	InContext    int       `json:"in_context,omitempty"`
	Incidents24h int       `json:"incidents_24h"`
	LastIncident time.Time `json:"last_incident,omitempty"`
}

// FaultSummaryRepeat is NVSentinel's repeat rule met on one card: one class in 5 or more bursts within 24 h, From
// to To being the first and last of those bursts.
type FaultSummaryRepeat struct {
	UUID   string    `json:"uuid,omitempty"`
	Bus    string    `json:"bus"`
	Class  string    `json:"class"`
	Bursts int       `json:"bursts"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
}

// Summarize reduces a report to the summary, read at now.
func Summarize(rep FaultReport, now time.Time) FaultSummary {
	s := FaultSummary{WindowDays: rep.WindowDays, ReadAt: now.UTC(), Cards: make([]FaultSummaryCard, 0, len(rep.Cards))}
	for _, c := range rep.Cards {
		sc := FaultSummaryCard{Index: c.Index, UUID: c.UUID, Bus: c.Bus, Events: c.Events, Incidents: c.Incidents, InContext: c.InContext}
		for _, in := range rep.Incidents {
			if in.Context != "" || !namesBus(in, c.Bus) {
				continue
			}
			if now.Sub(in.Start) <= 24*time.Hour {
				sc.Incidents24h++
			}
			if in.End.After(sc.LastIncident) {
				sc.LastIncident = in.End
			}
		}
		s.Cards = append(s.Cards, sc)
	}
	for _, r := range rep.Repeats {
		s.Repeats = append(s.Repeats, FaultSummaryRepeat{UUID: r.UUID, Bus: r.Bus, Class: r.Class, Bursts: r.Bursts, From: r.From, To: r.To})
	}
	if len(rep.Notes) > 0 {
		s.Note = rep.Notes[0]
		if len(rep.Notes) > 1 {
			s.Note += fmt.Sprintf(" (and %d more note(s): gpu faults shows them)", len(rep.Notes)-1)
		}
	}
	return s
}

func namesBus(in Incident, bus string) bool {
	for _, c := range in.Cards {
		if c.Bus == bus {
			return true
		}
	}
	return false
}

// WriteSummary writes the summary file whole: a temp file beside it, then a rename, so a reader never sees half.
func WriteSummary(path string, s FaultSummary) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".faults-summary-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// ReadSummary reads the summary file fleet-serve wrote. A missing file is (nil, nil): no reading, not "no faults".
func ReadSummary(path string) (*FaultSummary, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s FaultSummary
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("the fault summary %s is not JSON: %w", path, err)
	}
	return &s, nil
}
