package displaywatch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/placement"
)

func atTheDesk() placement.Presence {
	return placement.Presence{Mode: "auto", Known: true, Away: false, IdleSec: 12, Note: "idle 12s < threshold 15m0s"}
}

// offload_status answers from another process, so the watcher leaves what it did in a file: the last
// action, with why, and a heartbeat that says it is alive.
func TestTheStateFileCarriesTheLastActionAndAHeartbeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "display-watch.json")
	b := newBox()
	b.running = ready(twinE4B)
	b.pres = atTheDesk()
	w := New(watchedCfg(), b.deps(path))
	w.Check(context.Background())

	st, ok := readState(path)
	if !ok {
		t.Fatal("an action must leave the state file")
	}
	if st.CheckedAt.IsZero() || st.IntervalSec != 10 {
		t.Fatalf("the heartbeat and the period are recorded: %+v", st)
	}
	a := st.LastAction
	if a == nil || a.Layer != "display" || len(a.Unloaded) != 1 || a.Unloaded[0] != twinE4B || a.Error != "" {
		t.Fatalf("the last action names the layer and the twin it unloaded: %+v", a)
	}
	if len(a.Reasons) == 0 || !strings.Contains(a.Reasons[0], "presence") || a.At.IsZero() {
		t.Fatalf("the last action says why, and when: %+v", a)
	}
	// The wire shape offload_status reads.
	raw, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("the state file is JSON: %v", err)
	}
	for _, k := range []string{"checked_at", "interval_sec", "last_action"} {
		if _, ok := m[k]; !ok {
			t.Errorf("state file lacks %q: %s", k, raw)
		}
	}
}

// A failed unload is recorded as the last action with its error: the one fact an operator needs from
// offload_status is "the twin is still there, and why".
func TestAFailedUnloadIsTheRecordedLastAction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "display-watch.json")
	b := newBox()
	b.running = ready(twinE4B)
	b.pres = atTheDesk()
	b.unloadErr[twinE4B] = os.ErrPermission
	New(watchedCfg(), b.deps(path)).Check(context.Background())
	st, _ := readState(path)
	if st.LastAction == nil || len(st.LastAction.Unloaded) != 0 || !strings.Contains(st.LastAction.Error, twinE4B) {
		t.Fatalf("the failure and the twin it left are recorded: %+v", st.LastAction)
	}
}

// A check that finds nothing must not rewrite the file every tick (a heartbeat every 30 s is enough to
// tell a live watcher from a dead one), but must when the beat is due.
func TestAQuietCheckRewritesTheStateFileOnlyOnTheHeartbeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "display-watch.json")
	b := newBox()
	w := New(watchedCfg(), b.deps(path))
	w.Check(context.Background())
	first, _ := readState(path)

	b.advance(10 * time.Second)
	w.Check(context.Background())
	if again, _ := readState(path); !again.CheckedAt.Equal(first.CheckedAt) {
		t.Fatalf("a quiet tick 10 s later must not rewrite the file: %v -> %v", first.CheckedAt, again.CheckedAt)
	}
	b.advance(25 * time.Second) // 35 s after the first
	w.Check(context.Background())
	if later, _ := readState(path); !later.CheckedAt.After(first.CheckedAt) {
		t.Fatalf("the heartbeat is due after 30 s: %v -> %v", first.CheckedAt, later.CheckedAt)
	}
}

// A restart of fleet-serve keeps the last action visible: the watcher reads what the previous run left.
func TestARestartKeepsTheLastAction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "display-watch.json")
	b := newBox()
	b.running = ready(twinE4B)
	b.pres = atTheDesk()
	New(watchedCfg(), b.deps(path)).Check(context.Background())

	b2 := newBox() // a fresh process: nothing loaded, the operator is away again
	b2.now = b.now.Add(time.Hour)
	w := New(watchedCfg(), b2.deps(path))
	w.Check(context.Background()) // first beat of the new process
	st, _ := readState(path)
	if st.LastAction == nil || len(st.LastAction.Unloaded) != 1 {
		t.Fatalf("the previous run's last action must survive the restart: %+v", st)
	}
	if !st.CheckedAt.Equal(b2.now) {
		t.Fatalf("and the heartbeat is the new process's: %v", st.CheckedAt)
	}
}

// Run writes the first state file at start, so a box that has just come up does not read as "no
// heartbeat" until the first tick.
func TestRunLeavesAHeartbeatAtStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "display-watch.json")
	b := newBox()
	w := New(watchedCfg(), b.deps(path))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx, make(chan time.Time)); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := readState(path); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no state file after start")
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done
	if !strings.Contains(b.log(), "armed every 10s") {
		t.Fatalf("the start says what is armed: %q", b.log())
	}
}

// The announcement warns, once, when presence cannot be read from this process: in auto mode a probe
// that reads Known=false (a process outside the console session) makes every check unload the twin.
func TestTheStartWarnsWhenPresenceCannotBeReadFromThisProcess(t *testing.T) {
	b := newBox()
	b.pres = placement.Presence{Mode: "auto", Known: false, Note: "WTS reports no last-input time and the caller runs in session 0"}
	w := New(watchedCfg(), b.deps(""))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Run(ctx, make(chan time.Time))
	if !strings.Contains(b.log(), "WARNING presence cannot be read from this process") {
		t.Fatalf("an unreadable desk is said at start: %q", b.log())
	}
	b2 := newBox() // away: nothing to warn about
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	New(watchedCfg(), b2.deps("")).Run(ctx2, make(chan time.Time))
	if strings.Contains(b2.log(), "WARNING") {
		t.Fatalf("a readable presence has no warning: %q", b2.log())
	}
}

// The state file is replaced atomically: a reader that races the writer sees the old file or the new
// one, never half of either.
func TestTheStateFileIsNeverReadHalfWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "display-watch.json")
	if err := writeState(path, State{CheckedAt: time.Unix(1_700_000_000, 0), IntervalSec: 10}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	bad := make(chan string, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			b, err := os.ReadFile(path)
			if err != nil {
				continue // the instant a rename holds the name: not a torn read
			}
			var st State
			if err := json.Unmarshal(b, &st); err != nil {
				select {
				case bad <- string(b):
				default:
				}
				return
			}
		}
	}()
	long := strings.Repeat("x", 4096)
	for i := 0; i < 60; i++ {
		st := State{CheckedAt: time.Unix(1_700_000_000+int64(i), 0), IntervalSec: 10,
			LastAction: &Action{Layer: "display", Models: []string{long}, Reasons: []string{long}}}
		if err := writeState(path, st); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	select {
	case torn := <-bad:
		t.Fatalf("a reader saw a half-written state file (%d bytes)", len(torn))
	default:
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("no temp file may be left behind, got %d entries", len(entries))
	}
}

// The status field is omitted when there is nothing to say, says plainly when nothing is watching an
// awake layer, and shows the last action.
func TestStatusViewSaysWhetherAnAwakeDisplayLayerIsBeingWatched(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cfgFor := func(t *testing.T) config.Config {
		cfg := watchedCfg()
		cfg.StateDir = t.TempDir()
		return cfg
	}
	put := func(t *testing.T, cfg config.Config, st State) {
		t.Helper()
		path, err := StatePath(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeState(path, st); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a plain box says nothing", func(t *testing.T) {
		if v := StatusView(config.Config{StateDir: t.TempDir()}, now); v != nil {
			t.Fatalf("a box with no display layer has no field: %v", v)
		}
	})
	t.Run("a dormant layer that never acted says nothing", func(t *testing.T) {
		cfg := cfgFor(t)
		for i := range cfg.Layers {
			if cfg.Layers[i].Name == "display" {
				cfg.Layers[i].Dormant = true
			}
		}
		if v := StatusView(cfg, now); v != nil {
			t.Fatalf("nothing to report: %v", v)
		}
	})
	t.Run("an awake layer with no heartbeat says nothing is watching", func(t *testing.T) {
		v := StatusView(cfgFor(t), now)
		if v == nil || v["watching"] != false || !strings.Contains(v["note"].(string), "no heartbeat") {
			t.Fatalf("an awake layer nobody re-checks must say so: %v", v)
		}
		if _, has := v["last_action"]; has {
			t.Fatalf("no state, no last action: %v", v)
		}
	})
	t.Run("a fresh heartbeat is watching", func(t *testing.T) {
		cfg := cfgFor(t)
		put(t, cfg, State{CheckedAt: now.Add(-20 * time.Second), IntervalSec: 10})
		v := StatusView(cfg, now)
		if v == nil || v["watching"] != true || v["interval_sec"] != 10 || v["checked_at"] == nil {
			t.Fatalf("a recent heartbeat is a live watcher: %v", v)
		}
		if _, has := v["note"]; has {
			t.Fatalf("a live watcher needs no note: %v", v)
		}
	})
	t.Run("the last action rides along", func(t *testing.T) {
		cfg := cfgFor(t)
		act := &Action{At: now.Add(-time.Minute), Layer: "display", Models: []string{twinE4B}, Unloaded: []string{twinE4B}, Reasons: []string{"presence: operator at the desk"}}
		put(t, cfg, State{CheckedAt: now.Add(-5 * time.Second), IntervalSec: 10, LastAction: act})
		v := StatusView(cfg, now)
		got, _ := v["last_action"].(*Action)
		if got == nil || got.Unloaded[0] != twinE4B || !strings.Contains(got.Reasons[0], "desk") {
			t.Fatalf("the last action is surfaced: %v", v)
		}
	})
	t.Run("a stale heartbeat is not watching", func(t *testing.T) {
		cfg := cfgFor(t)
		put(t, cfg, State{CheckedAt: now.Add(-10 * time.Minute), IntervalSec: 10})
		v := StatusView(cfg, now)
		if v == nil || v["watching"] != false || !strings.Contains(v["note"].(string), "old") {
			t.Fatalf("a dead watcher must read as dead: %v", v)
		}
	})
	t.Run("a dormant layer keeps its history visible", func(t *testing.T) {
		cfg := cfgFor(t)
		for i := range cfg.Layers {
			if cfg.Layers[i].Name == "display" {
				cfg.Layers[i].Dormant = true
			}
		}
		put(t, cfg, State{CheckedAt: now.Add(-5 * time.Second), IntervalSec: 10, LastAction: &Action{Layer: "display", Models: []string{twinE4B}}})
		if v := StatusView(cfg, now); v == nil || v["last_action"] == nil {
			t.Fatalf("an action already taken stays visible after the layer is put back to sleep: %v", v)
		}
	})
	t.Run("the switch off is said", func(t *testing.T) {
		cfg := cfgFor(t)
		cfg.DisplayWatchSec = -1
		v := StatusView(cfg, now)
		if v == nil || v["watching"] != false || v["interval_sec"] != 0 || !strings.Contains(v["note"].(string), "off") {
			t.Fatalf("a check that is switched off is a field that says so: %v", v)
		}
	})
}
