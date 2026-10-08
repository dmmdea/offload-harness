// Package displaystate is what the display layer's post-admission watcher (internal/displaywatch,
// ADR 0075) leaves for everyone else: the small state file under the machine-wide state root that
// carries its heartbeat, its last action and whether it can see llama-swap, and the reader that turns
// that file into "is a watcher alive".
//
// It is a leaf package (config and gpulease only) because two packages that cannot import each other
// both need it: internal/displaywatch writes it and imports internal/placement for the guards it
// re-asks, and internal/placement reads it so the display layer's admission can refuse while nothing
// is watching a loaded twin. Putting the file's shape and Alive here is what keeps that out of an
// import cycle.
package displaystate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

const (
	// StateFileName is the file under the machine-wide state root that carries the watcher's last
	// action and heartbeat to offload_status and to the admission guard.
	StateFileName = "display-watch.json"

	// HeartbeatEvery bounds how often a check that found nothing to do rewrites the state file: a
	// reader tells a live watcher from a dead one by it, and a rewrite per 10 s tick would be churn.
	HeartbeatEvery = 30 * time.Second

	// CheckBudget bounds one check: the /running read and every unload it issues.
	CheckBudget = 20 * time.Second
)

// Action is one thing the watcher did: the twins it set out to unload and why.
type Action struct {
	At    time.Time `json:"at"`
	Layer string    `json:"layer"`
	// Models are the layer's models that were loaded and failed a guard; Unloaded are the ones that
	// came down. Error carries every unload that failed, which leaves its twin where it was.
	Models   []string `json:"models"`
	Unloaded []string `json:"unloaded,omitempty"`
	Reasons  []string `json:"reasons"`
	Error    string   `json:"error,omitempty"`
}

// State is what the watcher leaves for offload_status and the admission guard.
type State struct {
	CheckedAt   time.Time `json:"checked_at"`
	IntervalSec int       `json:"interval_sec"`
	LastAction  *Action   `json:"last_action,omitempty"`
	// ReadErr and BlindSince are set while llama-swap's /running cannot be read: a watcher that cannot
	// see what is loaded cannot see a twin to unload, so it is not watching however fresh its
	// heartbeat. BlindSince is when the unreadable streak began; both clear on the first good read.
	ReadErr    string     `json:"read_err,omitempty"`
	BlindSince *time.Time `json:"blind_since,omitempty"`
}

// StatePath is where the watcher leaves its state: under the machine-wide state root, beside the GPU
// lease, never inside a cloud-synced folder.
func StatePath(cfg config.Config) (string, error) {
	root, err := gpulease.ResolveStateRoot(cfg.StateDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, StateFileName), nil
}

// Read loads the state file; !ok when there is none, or it does not parse, or it carries no heartbeat.
func Read(path string) (State, bool) {
	if path == "" {
		return State{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return State{}, false
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil || st.CheckedAt.IsZero() {
		return State{}, false
	}
	return st, true
}

// Write replaces the state file atomically: a reader sees the old file or the new one, never half
// of either. On Windows a rename onto a file a reader holds open can fail for an instant, so it is
// retried briefly.
func Write(path string, st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, StateFileName+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	_ = os.Chmod(name, 0o644)
	var rerr error
	for i := 0; i < 20; i++ {
		if rerr = os.Rename(name, path); rerr == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	os.Remove(name)
	return rerr
}

// StaleAfter is how old a heartbeat may be before the watcher is called dead: three beats, where a
// beat is the longer of the check period and the heartbeat throttle, plus the check's own budget.
func StaleAfter(interval time.Duration) time.Duration {
	beat := interval
	if beat < HeartbeatEvery {
		beat = HeartbeatEvery
	}
	return 3*beat + CheckBudget
}

// Alive says whether a watcher is checking a loaded display twin on this box right now, from the
// heartbeat it leaves, and the reason in words. It is the question the display layer's admission asks
// before it lets a twin onto the operator's card: a twin that is admitted with nothing watching it
// stays there until llama-swap's 300 s idle ttl, however soon the operator is back.
//
// Not alive, each with its own reason: display_watch_sec negative (the operator switched the check off,
// and a layer that leaves only when something watches it does not open without it); no state path; no
// heartbeat; a heartbeat older than StaleAfter; a watcher whose /running cannot be read (it is up and
// blind). Whether the layer guards the desktop at all is the caller's question, not this one's.
//
// The watcher's period is read from cfg as this process knows it. A change to operator_presence or
// display_watch_sec reaches the watcher only when fleet-serve restarts, which re-reads the config: the
// watcher does not re-read it between checks.
func Alive(cfg config.Config, now time.Time) (bool, string) {
	interval := cfg.DisplayWatchInterval()
	if interval <= 0 {
		return false, "display_watch_sec is negative, so nothing re-checks a loaded display twin"
	}
	path, err := StatePath(cfg)
	if err != nil {
		return false, fmt.Sprintf("the watcher's state path cannot be resolved (%v)", err)
	}
	st, ok := Read(path)
	if !ok {
		return false, "no heartbeat from fleet-serve (is it running, and was it started after the layers were seeded?)"
	}
	if age := now.Sub(st.CheckedAt); age > StaleAfter(interval) {
		return false, fmt.Sprintf("the last heartbeat is %s old, so fleet-serve has stopped checking or is not running", age.Round(time.Second))
	}
	if st.ReadErr != "" {
		return false, fmt.Sprintf("the watcher cannot read llama-swap /running (%s), so it cannot see a loaded twin", st.ReadErr)
	}
	return true, fmt.Sprintf("watcher heartbeat %s old", now.Sub(st.CheckedAt).Round(time.Second))
}
