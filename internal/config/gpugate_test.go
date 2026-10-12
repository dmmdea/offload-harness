package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/seatload"
)

// Load is the ONE place the model-affinity load gate is armed, because it is the
// one place every entry point that can make a text call passes through. If that
// wiring is ever dropped, the gate goes quiet rather than loud — text resumes
// loading models on top of running renders and nothing reports it. So the wiring
// itself is pinned here, on the honest observation point (modelaffinity.GPULeaseDir)
// rather than on a re-derivation that would pass even with the call removed.
func TestLoadArmsTheGPULoadGate(t *testing.T) {
	root := t.TempDir()
	body, err := json.Marshal(map[string]string{"state_dir": root})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The gate must resolve through gpulease.LeaseDir — THE one resolver — so it
	// reads the SAME directory the media path writes. Comparing against LeaseDir's
	// own answer is what makes a second resolution order impossible to introduce
	// here without this test noticing.
	want, err := gpulease.LeaseDir("", root)
	if err != nil {
		t.Fatalf("LeaseDir: %v", err)
	}
	if got := modelaffinity.GPULeaseDir(); got != want {
		t.Fatalf("gate armed at %q after loading state_dir=%q, want %q", got, root, want)
	}
}

// The empty path is the fresh-install case: Load returns built-in defaults. It
// takes a DIFFERENT exit out of the function, and the gate must still be armed on
// it — a box with no config file still renders and still serves text.
func TestLoadArmsTheGPULoadGateOnDefaults(t *testing.T) {
	if _, err := Load(""); err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	want, err := gpulease.LeaseDir("", "")
	if err != nil {
		t.Skipf("platform lease dir unresolvable here: %v", err)
	}
	if got := modelaffinity.GPULeaseDir(); got != want {
		t.Fatalf("gate armed at %q on default config, want the platform default %q", got, want)
	}
}

// A config whose endpoint is ANOTHER box's engine (a bench config aimed at a remote node) leaves
// this box's lease with nothing to protect, and nothing it loads may make a lease on THIS box
// reach that engine: the seat race rule asks the lease directory first, and the directory is
// disarmed under a remote endpoint. The unload goes through the endpoint the seat's own
// admission named, never one armed from config.
func TestRemoteEndpointConfigNeverReachesTheEngineThroughTheSeatYield(t *testing.T) {
	root := t.TempDir()
	body, err := json.Marshal(map[string]any{"state_dir": root, "endpoint": "http://other-box.example:8080"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	var calls int
	t.Cleanup(modelaffinity.SetYieldSeams(modelaffinity.YieldSeams{
		Read: func(context.Context, string, string) (seatload.Reading, error) {
			calls++
			return seatload.Reading{Loaded: true}, nil
		},
		Unload: func(context.Context, string, string) error { calls++; return nil },
	}))
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := modelaffinity.GPULeaseDir(); got != "" {
		t.Fatalf("a remote-endpoint config disarms this box's lease gate, got %q", got)
	}
	if yielded, why := modelaffinity.YieldIfFenced(context.Background(), "http://other-box.example:8080", "some-seat"); yielded || why != "" {
		t.Fatalf("a remote endpoint must not be reachable through the seat yield: %v %q", yielded, why)
	}
	if calls != 0 {
		t.Fatalf("the seat yield made %d call(s) to an engine on another box", calls)
	}
}

// ---- Register C-86a: a one-card lease and the load fence, end to end ----------------------------
//
// A box with no `layers` key (the installer's tier seeds write none) used to fence EVERY card for a
// harness text load whenever one card was leased, because the fence is armed from Config.ModelPins and
// ModelPins placed a model through layers only. The serving config (serving_config_path) is where
// those models are really pinned; F15 already read it on the unload side. These tests drive the
// real path: Load arms the fence, a real card-scoped lease is held, and a real Admit asks.

// fenceCards is the synthetic three-card table the fence resolves pins against; card 1 is the display
// card, which nothing in the fixture serving config is pinned to.
func fenceCards() []gpuprobe.Card {
	devs := []gpuprobe.Device{
		{Index: 0, UUID: fenceCard0, Name: "T", TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
		{Index: 1, UUID: fenceCard1, Name: "T", TotalGiB: 16, FreeGiB: 14, UtilKnown: true, DisplayAttached: true},
		{Index: 2, UUID: fenceCard2, Name: "T", TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
	}
	cards, _ := gpuprobe.BuildCards(devs, "")
	return cards
}

// fenceBox arms the process the way a box with no layers is armed: Load of a config that sets
// serving_config_path (servingPath; "" leaves the key out) over a fresh state root, the synthetic card
// table (tableErr makes the read fail), and a card-scoped lease manager on the same root.
func fenceBox(t *testing.T, servingPath string, tableErr error) (*gpulease.Manager, []gpuprobe.Card) {
	t.Helper()
	root := t.TempDir()
	cfg := map[string]any{"state_dir": root}
	if servingPath != "" {
		cfg["serving_config_path"] = servingPath
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cards := fenceCards()
	t.Cleanup(modelaffinity.SetCardTableReader(func(context.Context, string) ([]gpuprobe.Card, string, error) {
		if tableErr != nil {
			return nil, "", tableErr
		}
		return cards, "", nil
	}))
	t.Cleanup(func() {
		modelaffinity.SetSeatPins(nil)
		modelaffinity.DisarmGPULease()
	})
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatalf("open lease manager: %v", err)
	}
	m.SetCardScoped(true)
	return m, cards
}

// holdMedia takes a media lease on devs (none = the whole node) and returns its release.
func holdMedia(t *testing.T, m *gpulease.Manager, devs ...string) (release func()) {
	t.Helper()
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render", Origin: "test", Devices: devs})
	if err != nil {
		t.Fatalf("acquire the media lease on %v: %v", devs, err)
	}
	return func() { _ = l.Release() }
}

// fenceVerdicts asks the fence the question a harness text load asks, for every model at once:
// true = the load is admitted at once, false = it waits for the lease (and is refused when its budget
// ends). tag keeps each call's admission bases apart.
func fenceVerdicts(t *testing.T, tag string, models []string) map[string]bool {
	t.Helper()
	out := make(map[string]bool, len(models))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, model := range models {
		wg.Add(1)
		go func(model string) {
			defer wg.Done()
			tk, err := modelaffinity.Admit(context.Background(), "http://fence-"+tag+"-"+model, model, 250*time.Millisecond)
			if err == nil {
				tk.Release()
			} else if !modelaffinity.IsLeaseRefusal(err) {
				t.Errorf("Admit(%s) = %v, want admitted or a lease refusal", model, err)
			}
			mu.Lock()
			out[model] = err == nil
			mu.Unlock()
		}(model)
	}
	wg.Wait()
	return out
}

func wantFence(t *testing.T, got map[string]bool, admitted, fenced []string) {
	t.Helper()
	for _, m := range admitted {
		if !got[m] {
			t.Errorf("%s waits for the lease; its cards are not leased, so it must be admitted at once", m)
		}
	}
	for _, m := range fenced {
		if got[m] {
			t.Errorf("%s was admitted under a lease that touches its cards, or that the serving config cannot place off them", m)
		}
	}
}

func joinModels(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

// THE OPERATOR'S COMPLAINT, as a test ("routing is per CARD and fleet-wide"): a media lease on one
// card fences the seats the serving config pins to that card and the seats it cannot place, and
// lets a text load for a seat pinned to another card through at once. Both directions: the
// mirror lease fences the other half.
func TestAOneCardLeaseFencesOnlyTheSeatsTheServingConfigPlacesOnThatCard(t *testing.T) {
	m, cards := fenceBox(t, writeFenceServing(t, fenceServingYAML), nil)

	cardZero := []string{"gemma-4-26b", "gemma-4-12b", "workhorse-12b", "gemma-4-e4b", "gemma-4-e2b", "embeddinggemma", "bge-reranker"}
	cardTwo := []string{"whisper-stt", "qwen3-vl-8b", "embeddinggemma2", "index-ordered"}
	pairs := []string{"qwen3.8-27b-262k", "agent-pool-2card"}
	unplaced := []string{"qwen3.8-27b-vllm", "qwen3.8-27b-vllm-3card", "unpinned-dual", "cmd-only-pin", "macro-pin", "index-no-order", "claimed", "not-in-the-yaml"}
	everyone := joinModels(cardZero, cardTwo, pairs, unplaced)

	release := holdMedia(t, m, cards[0].LeaseID())
	wantFence(t, fenceVerdicts(t, "c0", everyone), cardTwo, joinModels(cardZero, pairs, unplaced))
	// The seats admitted at once queued nowhere: a blocked admission would hold a place in line.
	fenceVerdicts(t, "c0-alone", cardTwo)
	if w := m.Waiters(); len(w) != 0 {
		t.Errorf("a card-2 seat under a card-0 lease registered %d waiter(s): %+v", len(w), w)
	}
	// The place in line a blocked load takes is the seat's own cards.
	if got := modelaffinity.SeatCards("whisper-stt"); !reflect.DeepEqual(got, []string{cards[2].LeaseID()}) {
		t.Errorf("SeatCards(whisper-stt) = %v, want card 2 only", got)
	}
	if got := modelaffinity.SeatCards("qwen3.8-27b-262k"); !reflect.DeepEqual(got, []string{cards[0].LeaseID(), cards[2].LeaseID()}) {
		t.Errorf("SeatCards(pair) = %v, want cards 0 and 2", got)
	}
	if got := modelaffinity.SeatCards("qwen3.8-27b-vllm"); got != nil {
		t.Errorf("SeatCards(a proxy: seat) = %v, want nil (the whole node)", got)
	}
	// The upstream door reads the same placement.
	swap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"running":[]}`))
	}))
	t.Cleanup(swap.Close)
	if _, err := modelaffinity.AwaitUpstream(context.Background(), swap.URL, "whisper-stt", "/props", time.Now()); err != nil {
		t.Errorf("AwaitUpstream(whisper-stt) under a card-0 lease = %v, want the URL at once", err)
	}
	if _, err := modelaffinity.AwaitUpstream(context.Background(), swap.URL, "gemma-4-26b", "/props", time.Now()); !modelaffinity.IsLeaseRefusal(err) {
		t.Errorf("AwaitUpstream(gemma-4-26b) under a card-0 lease = %v, want a lease refusal", err)
	}
	release()

	release = holdMedia(t, m, cards[2].LeaseID())
	defer release()
	wantFence(t, fenceVerdicts(t, "c2", everyone), cardZero, joinModels(cardTwo, pairs, unplaced))
}

// The direction of every doubt is "fence": where the serving config cannot place a model, the
// answer is what it was before the serving config was read at all.
func TestTheFenceStaysClosedWhereTheServingConfigCannotPlaceAModel(t *testing.T) {
	good := writeFenceServing(t, fenceServingYAML)
	for i, tc := range []struct {
		name      string
		serving   string
		tableErr  error
		wholeNode bool
	}{
		{name: "a whole-node lease", serving: good, wholeNode: true},
		{name: "a card table that cannot be read", serving: good, tableErr: errors.New("no card table")},
		{name: "serving_config_path unset", serving: ""},
		{name: "serving config missing", serving: filepath.Join(t.TempDir(), "gone.yaml")},
		{name: "serving config not yaml", serving: writeFenceServing(t, "models: [unclosed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, cards := fenceBox(t, tc.serving, tc.tableErr)
			var release func()
			if tc.wholeNode {
				m.SetCardScoped(false)
				release = holdMedia(t, m)
			} else {
				release = holdMedia(t, m, cards[0].LeaseID())
			}
			defer release()
			// whisper-stt is pinned to card 2 in the good yaml, gemma-4-26b to card 0.
			models := []string{"whisper-stt", "gemma-4-26b"}
			wantFence(t, fenceVerdicts(t, fmt.Sprint("closed", i), models), nil, models)
		})
	}
}

// The serving config is read again when llama-swap's config is edited, so a long-lived process (the
// MCP server, fleet-serve) does not keep fencing by the placement it read at start. The file keeps one
// old modification time through every edit, so only its size can say that it changed (a file edited
// moments ago is read again at every call anyway, which would prove nothing about the memo).
func TestTheFenceFollowsAnEditOfTheServingConfigWithoutARestart(t *testing.T) {
	servingPath := writeFenceServing(t, "models:\n  movable:\n    env: [\"CUDA_VISIBLE_DEVICES="+fenceCard0+"\"]\n")
	old := time.Now().Add(-time.Hour)
	settle := func() {
		t.Helper()
		if err := os.Chtimes(servingPath, old, old); err != nil {
			t.Fatal(err)
		}
	}
	edit := func(body string) {
		t.Helper()
		if err := os.WriteFile(servingPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		settle()
	}
	settle()
	m, cards := fenceBox(t, servingPath, nil)
	release := holdMedia(t, m, cards[0].LeaseID())
	defer release()

	wantFence(t, fenceVerdicts(t, "edit0", []string{"movable"}), nil, []string{"movable"})

	// The operator moves the model to card 2 and llama-swap reloads: the file differs in size.
	edit("models:\n  movable:\n    env: [\"CUDA_VISIBLE_DEVICES=" + fenceCard2 + "\"]\n    # moved off the leased card\n")
	wantFence(t, fenceVerdicts(t, "edit1", []string{"movable"}), []string{"movable"}, nil)

	// And back onto it.
	edit("models:\n  movable:\n    env: [\"CUDA_VISIBLE_DEVICES=" + fenceCard0 + "\"]\n")
	wantFence(t, fenceVerdicts(t, "edit2", []string{"movable"}), nil, []string{"movable"})
}
