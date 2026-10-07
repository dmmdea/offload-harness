package displaywatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/placement"
)

// box is a workstation as the watcher sees it: what llama-swap has loaded, what the card has free,
// whether the operator is at the desk, and what the watcher did about it.
type box struct {
	mu        sync.Mutex
	now       time.Time
	running   []Model
	runErr    error
	unloadErr map[string]error
	unloaded  []string
	freeGiB   float64 // free VRAM on the display card (index 1)
	noDevices bool    // the device sample is missing or stale
	pres      placement.Presence
	reads     struct{ running, devices, presence int }
	logs      []string
}

func newBox() *box {
	return &box{
		now:       time.Unix(1_700_000_000, 0),
		freeGiB:   6,
		pres:      placement.Presence{Mode: "away", Known: true, Away: true, Note: "operator override: away"},
		unloadErr: map[string]error{},
	}
}

func (b *box) deps(statePath string) Deps {
	return Deps{
		Running: func(context.Context) ([]Model, error) {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.reads.running++
			return append([]Model(nil), b.running...), b.runErr
		},
		Unload: func(_ context.Context, model string) error {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.unloaded = append(b.unloaded, model)
			if err := b.unloadErr[model]; err != nil {
				return err
			}
			// llama-swap takes the model down: it leaves /running.
			kept := b.running[:0]
			for _, m := range b.running {
				if m.ID != model {
					kept = append(kept, m)
				}
			}
			b.running = kept
			return nil
		},
		Devices: func() ([]gpuprobe.Device, bool) {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.reads.devices++
			if b.noDevices {
				return nil, false
			}
			return []gpuprobe.Device{
				{Index: 0, UUID: "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee", FreeGiB: 1},
				{Index: 1, UUID: "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff", FreeGiB: b.freeGiB, DisplayAttached: true},
				{Index: 2, UUID: "GPU-cccc3333-dddd-eeee-ffff-000000000000", FreeGiB: 1},
			}, true
		},
		Presence: func() placement.Presence {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.reads.presence++
			return b.pres
		},
		Now: func() time.Time {
			b.mu.Lock()
			defer b.mu.Unlock()
			return b.now
		},
		Logf: func(format string, args ...any) {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.logs = append(b.logs, fmt.Sprintf(format, args...))
		},
		StatePath: statePath,
	}
}

func (b *box) advance(d time.Duration) {
	b.mu.Lock()
	b.now = b.now.Add(d)
	b.mu.Unlock()
}

func (b *box) log() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.logs, "\n")
}

// watchedCfg is the fixture's composite box with the display layer awake.
func watchedCfg() config.Config {
	cfg := config.CompositeFixture()
	cfg.OperatorPresence = "auto"
	for i := range cfg.Layers {
		if cfg.Layers[i].Name == "display" {
			cfg.Layers[i].Dormant = false
		}
	}
	return cfg
}

const (
	twinE4B = "gemma-4-e4b-display"
	twinE2B = "gemma-4-e2b-display"
)

func ready(ids ...string) []Model {
	var out []Model
	for _, id := range ids {
		out = append(out, Model{ID: id, State: "ready"})
	}
	return out
}

func newWatcher(t *testing.T, cfg config.Config, b *box) *Watcher {
	t.Helper()
	w := New(cfg, b.deps(""))
	if w == nil {
		t.Fatal("the config declares an awake display layer with desktop guards: a watcher is wanted")
	}
	return w
}

// While no display twin is loaded there is nothing to guard, and the check must cost nothing: no
// nvidia-smi sample, no presence probe, and above all nothing unloaded.
func TestCheckDoesNothingWhileNoTwinIsLoaded(t *testing.T) {
	b := newBox()
	b.running = ready("embeddinggemma", "agent-pool") // the memory stack and the pair seat
	b.pres = placement.Presence{Mode: "present", Known: true}
	out := newWatcher(t, watchedCfg(), b).Check(context.Background())
	if len(out.Loaded) != 0 || len(out.Unloaded) != 0 || len(b.unloaded) != 0 {
		t.Fatalf("nothing of the display layer is loaded: %+v unloaded=%v", out, b.unloaded)
	}
	if b.reads.devices != 0 || b.reads.presence != 0 {
		t.Fatalf("an idle check must not probe the card or the desk: %+v", b.reads)
	}
}

// The operator sat down: presence no longer reads away, so every twin on the display card goes, and
// nothing else does.
func TestCheckUnloadsTheDisplayTwinsWhenTheOperatorIsAtTheDesk(t *testing.T) {
	b := newBox()
	b.running = ready("embeddinggemma", "agent-pool", twinE4B)
	b.pres = placement.Presence{Mode: "auto", Known: true, Away: false, IdleSec: 12, Note: "idle 12s < threshold 15m0s"}
	out := newWatcher(t, watchedCfg(), b).Check(context.Background())
	if len(b.unloaded) != 1 || b.unloaded[0] != twinE4B {
		t.Fatalf("only the loaded twin goes, got %v", b.unloaded)
	}
	if len(out.Unloaded) != 1 || len(out.Reasons) == 0 || !strings.Contains(strings.Join(out.Reasons, " "), "presence") {
		t.Fatalf("the outcome says what was unloaded and why: %+v", out)
	}
	log := b.log()
	if !strings.Contains(log, twinE4B) || !strings.Contains(log, "presence") {
		t.Fatalf("an unload is logged with its reason, got %q", log)
	}
}

func TestCheckUnloadsBothTwinsWhenBothAreLoaded(t *testing.T) {
	b := newBox()
	b.running = ready(twinE4B, twinE2B, "agent-pool")
	b.pres = placement.Presence{Mode: "present", Known: true}
	newWatcher(t, watchedCfg(), b).Check(context.Background())
	if len(b.unloaded) != 2 {
		t.Fatalf("both twins leave the display card, got %v", b.unloaded)
	}
	for _, m := range b.unloaded {
		if m != twinE4B && m != twinE2B {
			t.Fatalf("only the layer's twins may be unloaded, got %q", m)
		}
	}
}

// The other door: a game launches and takes the card's memory while the operator is away. Free VRAM on
// the display card under the floor unloads the twin, whatever presence says.
func TestCheckUnloadsTheDisplayTwinWhenTheFloorFalls(t *testing.T) {
	b := newBox()
	b.running = ready(twinE4B)
	b.freeGiB = 3.5 // the layer keeps 4
	out := newWatcher(t, watchedCfg(), b).Check(context.Background())
	if len(b.unloaded) != 1 {
		t.Fatalf("3.5 GiB free under a 4 GiB floor must unload, got %v", b.unloaded)
	}
	joined := strings.Join(out.Reasons, " ")
	if !strings.Contains(joined, "floor") || strings.Contains(joined, "presence") {
		t.Fatalf("the reason names the floor and only the floor: %q", joined)
	}
}

// A healthy twin is not a violation: its own footprint is already out of the free number, so the check
// is free >= floor and not free - footprint >= floor.
func TestCheckKeepsATwinWhoseFloorHolds(t *testing.T) {
	for _, free := range []float64{4, 4.5, 5} { // all under floor + the 6.2 GiB footprint
		b := newBox()
		b.running = ready(twinE4B)
		b.freeGiB = free
		out := newWatcher(t, watchedCfg(), b).Check(context.Background())
		if len(b.unloaded) != 0 || len(out.Unloaded) != 0 {
			t.Fatalf("free %.1f GiB keeps the floor: nothing may be unloaded, got %v", free, b.unloaded)
		}
		if len(out.Loaded) != 1 {
			t.Fatalf("the loaded twin is reported as watched: %+v", out)
		}
	}
}

// In away mode presence never flips, so only the floor can fire: documented, and pinned.
func TestInAwayModeOnlyTheFloorCanFire(t *testing.T) {
	cfg := watchedCfg()
	cfg.OperatorPresence = "away"
	b := newBox()
	b.running = ready(twinE4B)
	b.pres = placement.Presence{Mode: "away", Known: true, Away: true, IdleSec: 0, Note: "operator override: away"}
	w := newWatcher(t, cfg, b)
	w.Check(context.Background()) // the operator is typing; away does not look
	if len(b.unloaded) != 0 {
		t.Fatalf("away is an unconditional override: presence cannot unload, got %v", b.unloaded)
	}
	b.freeGiB = 2
	w.Check(context.Background())
	if len(b.unloaded) != 1 {
		t.Fatalf("the floor still fires in away mode, got %v", b.unloaded)
	}
}

// Every input is fail-closed, as at admission: an unknown presence, a missing device sample and a card
// the probe cannot see each unload a seat that cannot be shown safe.
func TestCheckFailsClosedOnAnUnreadableInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		set    func(*box)
		wantIn string
	}{
		{"unknown presence", func(b *box) { b.pres = placement.Presence{Mode: "auto", Known: false, Note: "no console session"} }, "presence"},
		{"no device sample", func(b *box) { b.noDevices = true }, "unreadable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBox()
			b.running = ready(twinE4B)
			tc.set(b)
			out := newWatcher(t, watchedCfg(), b).Check(context.Background())
			if len(b.unloaded) != 1 || !strings.Contains(strings.Join(out.Reasons, " "), tc.wantIn) {
				t.Fatalf("an unreadable input must unload and say so: unloaded=%v reasons=%v", b.unloaded, out.Reasons)
			}
		})
	}
}

// Fail loud, never recover: an unload that fails is logged and recorded, the twin stays where it is,
// and the next check tries again. Nothing else is touched to make room.
func TestAFailedUnloadIsLoudAndRetried(t *testing.T) {
	b := newBox()
	b.running = ready(twinE4B, "agent-pool")
	b.pres = placement.Presence{Mode: "present", Known: true}
	b.unloadErr[twinE4B] = errors.New("per-model unload route absent on this llama-swap build")
	w := newWatcher(t, watchedCfg(), b)

	out := w.Check(context.Background())
	if len(out.Unloaded) != 0 || len(out.Failed) != 1 {
		t.Fatalf("a failed unload is reported as failed: %+v", out)
	}
	if log := b.log(); !strings.Contains(log, "FAILED") || !strings.Contains(log, "route absent") {
		t.Fatalf("a failure is logged loudly with its error, got %q", log)
	}
	w.Check(context.Background())
	if len(b.unloaded) != 2 || b.unloaded[0] != twinE4B || b.unloaded[1] != twinE4B {
		t.Fatalf("the next check retries the same twin and only it, got %v", b.unloaded)
	}
	delete(b.unloadErr, twinE4B)
	w.Check(context.Background())
	if len(b.unloaded) != 3 || len(b.running) != 1 || b.running[0].ID != "agent-pool" {
		t.Fatalf("the retry succeeds and the pair seat is untouched: unloaded=%v running=%v", b.unloaded, b.running)
	}
}

// A seat that llama-swap is already taking down holds nothing, and one that is starting is about to.
func TestCheckCountsAStartingTwinAndIgnoresAStoppingOne(t *testing.T) {
	b := newBox()
	b.pres = placement.Presence{Mode: "present", Known: true}
	b.running = []Model{{ID: twinE4B, State: "stopping"}, {ID: twinE2B, State: "starting"}}
	newWatcher(t, watchedCfg(), b).Check(context.Background())
	if len(b.unloaded) != 1 || b.unloaded[0] != twinE2B {
		t.Fatalf("the starting twin goes, the stopping one is already going: %v", b.unloaded)
	}
}

// llama-swap unreadable: nothing is known to be loaded, nothing is unloaded, and the operator is told
// (once, not every tick).
func TestAnUnreadableRunningListIsReportedOnceAndUnloadsNothing(t *testing.T) {
	b := newBox()
	b.runErr = errors.New("connection refused")
	w := newWatcher(t, watchedCfg(), b)
	out := w.Check(context.Background())
	if out.ReadErr == nil || len(b.unloaded) != 0 {
		t.Fatalf("an unreadable /running unloads nothing: %+v", out)
	}
	for i := 0; i < 5; i++ {
		b.advance(10 * time.Second)
		w.Check(context.Background())
	}
	if n := strings.Count(b.log(), "could not be read"); n != 1 {
		t.Fatalf("an outage is one line within the quiet window, not one per tick: %d lines\n%s", n, b.log())
	}
	b.advance(10 * time.Minute)
	w.Check(context.Background())
	if n := strings.Count(b.log(), "could not be read"); n != 2 {
		t.Fatalf("a long outage is said again after the quiet window, got %d lines", n)
	}
}

// The watcher guards a twin whoever loaded it: a dormant layer's rendered twin, loaded by another
// client, still comes down when the operator is at the desk, while one loaded while away stays.
func TestADormantLayersTwinLoadedByAnotherClientIsGuardedToo(t *testing.T) {
	cfg := watchedCfg()
	for i := range cfg.Layers {
		if cfg.Layers[i].Name == "display" {
			cfg.Layers[i].Dormant = true
		}
	}
	b := newBox()
	b.running = ready(twinE4B)
	w := newWatcher(t, cfg, b)
	b.pres = placement.Presence{Mode: "auto", Known: true, Away: true, Locked: true, Note: "console session locked"}
	w.Check(context.Background())
	if len(b.unloaded) != 0 {
		t.Fatalf("away with the floor kept: the twin stays, got %v", b.unloaded)
	}
	b.pres = placement.Presence{Mode: "auto", Known: true, Away: false, Note: "operator at the desk"}
	w.Check(context.Background())
	if len(b.unloaded) != 1 {
		t.Fatalf("at the desk the same twin comes down, dormant or not, got %v", b.unloaded)
	}
}

// New builds nothing when there is nothing to watch or the operator switched the check off.
func TestNewReturnsNilWhenThereIsNothingToWatch(t *testing.T) {
	b := newBox()
	if New(config.Config{}, b.deps("")) != nil {
		t.Fatal("a plain box has no display layer: no watcher")
	}
	cfg := watchedCfg()
	cfg.DisplayWatchSec = -1
	if New(cfg, b.deps("")) != nil {
		t.Fatal("display_watch_sec < 0 switches the check off")
	}
	cfg = watchedCfg()
	for i := range cfg.Layers {
		if cfg.Layers[i].Name == "display" {
			cfg.Layers[i].Guards = nil
		}
	}
	if New(cfg, b.deps("")) != nil {
		t.Fatal("a display layer with no desktop guard declares nothing to hold a loaded seat to")
	}
	cfg = watchedCfg()
	cfg.Layers = cfg.Layers[:3] // no display layer at all
	if New(cfg, b.deps("")) != nil {
		t.Fatal("no display layer, no watcher")
	}
	if New(watchedCfg(), b.deps("")) == nil {
		t.Fatal("the awake display layer with its guards is watched")
	}
}

// Only the display layer's own models are ever unloaded: the layer named display, its router's twins.
// A guarded three-card layer that a box declares keeps its admission-only guards.
func TestOnlyTheDisplayLayersModelsAreWatched(t *testing.T) {
	cfg := watchedCfg() // also declares the fixture's guarded three-card layer
	b := newBox()
	b.running = ready("qwen3.8-flash-next-262k", "agent-pool")
	b.pres = placement.Presence{Mode: "present", Known: true}
	out := newWatcher(t, cfg, b).Check(context.Background())
	if len(b.unloaded) != 0 || len(out.Loaded) != 0 {
		t.Fatalf("a model of another layer is never the watcher's to unload: %v %+v", b.unloaded, out)
	}
}

// Run drives Check on every tick, checks once at the start (a twin loaded before fleet-serve came up),
// and returns when its context ends.
func TestRunChecksAtStartAndOnEveryTickAndStopsWithItsContext(t *testing.T) {
	b := newBox()
	w := newWatcher(t, watchedCfg(), b)
	ctx, cancel := context.WithCancel(context.Background())
	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() { w.Run(ctx, tick); close(done) }()

	waitReads := func(want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			b.mu.Lock()
			n := b.reads.running
			b.mu.Unlock()
			if n >= want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("waited for %d /running reads, have %d", want, n)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	waitReads(1) // the check at start
	tick <- time.Now()
	waitReads(2)
	tick <- time.Now()
	waitReads(3)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run must return when its context ends")
	}
}
