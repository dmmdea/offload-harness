package displaywatch

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/displaystate"
)

// A watcher that cannot read llama-swap's /running cannot see a twin to unload. Its heartbeat is fresh
// (it is running), so a status that reads only the heartbeat would call it watching. It records the
// streak in the state file, and the status view and the admission guard both read it.

func TestABlindWatcherIsRecordedAndStopsReadingAsWatching(t *testing.T) {
	path := filepath.Join(t.TempDir(), "display-watch.json")
	b := newBox()
	w := New(watchedCfg(), b.deps(path))
	w.Check(context.Background()) // a healthy check leaves the heartbeat, so the next write is throttled
	b.advance(5 * time.Second)
	began := b.now
	b.mu.Lock()
	b.runErr = errors.New("connection refused")
	b.mu.Unlock()
	w.Check(context.Background())

	st, ok := readState(path)
	if !ok || st.ReadErr != "connection refused" || st.BlindSince == nil || !st.BlindSince.Equal(began) {
		t.Fatalf("the first unreadable /running is written at once (not at the next heartbeat), with when it began: %+v %v", st, ok)
	}
	// Later ticks keep the original start of the streak.
	b.advance(40 * time.Second)
	w.Check(context.Background())
	st, _ = readState(path)
	if st.BlindSince == nil || !st.BlindSince.Equal(began) {
		t.Fatalf("a streak keeps the time it began: %+v", st)
	}
	if !st.CheckedAt.Equal(b.now) {
		t.Fatalf("the heartbeat still carries the streak: %+v", st)
	}

	cfg := watchedCfg()
	cfg.StateDir = t.TempDir()
	statePath, _ := StatePath(cfg)
	if err := writeState(statePath, st); err != nil {
		t.Fatal(err)
	}
	v := StatusView(cfg, st.CheckedAt.Add(2*time.Second))
	if v == nil || v["watching"] != false || v["read_err"] != "connection refused" || v["blind_since"] == nil {
		t.Fatalf("a fresh heartbeat on a blind watcher is not watching: %v", v)
	}
	if note, _ := v["note"].(string); !strings.Contains(note, "/running") || !strings.Contains(note, "would not be seen") {
		t.Fatalf("the note must say what the watcher cannot see: %q", note)
	}
}

func TestTheWatcherSeesAgainAndClearsTheBlindFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "display-watch.json")
	b := newBox()
	b.runErr = errors.New("connection refused")
	w := New(watchedCfg(), b.deps(path))
	w.Check(context.Background())
	if st, _ := readState(path); st.ReadErr == "" {
		t.Fatalf("setup: blind: %+v", st)
	}

	b.advance(5 * time.Second) // well inside the 30 s heartbeat throttle
	b.mu.Lock()
	b.runErr = nil
	b.mu.Unlock()
	w.Check(context.Background())
	st, _ := readState(path)
	if st.ReadErr != "" || st.BlindSince != nil {
		t.Fatalf("the first good read clears the streak at once, not at the next heartbeat: %+v", st)
	}
	if !strings.Contains(b.log(), "readable again") {
		t.Errorf("recovery is said: %s", b.log())
	}
}

// A blind watcher is not alive for the admission guard either.
func TestABlindWatcherIsNotAliveForAdmission(t *testing.T) {
	cfg := watchedCfg()
	cfg.StateDir = t.TempDir()
	statePath, _ := StatePath(cfg)
	b := newBox()
	b.runErr = errors.New("503 service unavailable")
	New(cfg, b.deps(statePath)).Check(context.Background())
	if alive, why := displaystate.Alive(cfg, b.now); alive || !strings.Contains(why, "503") {
		t.Fatalf("up but blind is not alive: %v %q", alive, why)
	}
}

// C6: the state path error is neither dropped nor silent.
func TestAStatePathThatCannotBeResolvedIsLoggedAtStartAndShownInTheStatusNote(t *testing.T) {
	b := newBox()
	deps := b.deps("")
	deps.StatePathErr = errors.New("no usable state root")
	w := New(watchedCfg(), deps)
	w.announce()
	if !strings.Contains(b.log(), "status file path could not be resolved") || !strings.Contains(b.log(), "no usable state root") {
		t.Fatalf("start must say the path is unresolved and why:\n%s", b.log())
	}

	// A StateDir that cannot be resolved as the machine-wide state root: a cloud-synced folder is refused.
	cfg := watchedCfg()
	cfg.StateDir = filepath.Join(t.TempDir(), "My Drive", "state")
	if _, err := StatePath(cfg); err == nil {
		t.Fatal("setup: a state root inside a cloud-sync folder is refused")
	}
	v := StatusView(cfg, time.Now())
	note, _ := v["note"].(string)
	if v == nil || v["watching"] != false || !strings.Contains(note, "cannot be resolved") {
		t.Fatalf("the status note must carry the path error: %v", v)
	}
}

func TestProductionCarriesTheStatePathError(t *testing.T) {
	isolate(t)
	cfg := watchedCfg()
	cfg.StateDir = filepath.Join(t.TempDir(), "My Drive", "state")
	if _, perr := StatePath(cfg); perr == nil {
		t.Fatal("setup: a state root inside a cloud-sync folder is refused")
	}
	d := Production(cfg, healthyDevices(time.Now()))
	if d.StatePathErr == nil || d.StatePath != "" {
		t.Fatalf("Production must carry the error StatePath returned, not drop it: path %q err %v", d.StatePath, d.StatePathErr)
	}
}
