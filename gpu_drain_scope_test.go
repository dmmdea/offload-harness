package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// Plan P5 (register C-86): `--drain` and `--unload-seat` clear the cards the lease holds, not the
// node. A lease on card 2 neither drains nor unloads a seat that sits on card 0, and waits only
// for runs on card 2. The synthetic box is the flagship fixture's: single layer (router and agent
// on card 0, OCR on card 2), the pair and the flagship on every card.

const (
	leaseCard2ID = "gpu-cccc0000-x" // statusCards(): index 2
	leaseCard0ID = "gpu-aaaa0000-x" // index 0
)

// scopedDrainConfig writes a config declaring the flagship layers, the given agent seat and the
// swap endpoint, and arms the synthetic card table. It returns the loaded config.
func scopedDrainConfig(t *testing.T, endpoint, agentSeat string) config.Config {
	t.Helper()
	root := t.TempDir()
	fx := config.FlagshipFixture()
	doc := map[string]any{
		"state_dir": root, "endpoint": endpoint, "agent_model": agentSeat,
		"layers": fx.Layers, "tier_profile": fx.TierProfile, "tiers": fx.Tiers,
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	old := cardTableFn
	cardTableFn = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) { return statusCards(), "", nil }
	t.Cleanup(func() { cardTableFn = old })
	return loadCfgPath(path)
}

func sortedCalls(f *multiSeatSwap) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.unloadCalls...)
	sort.Strings(out)
	return out
}

func TestUnloadSeatOnlyUnloadsIntersectingModels(t *testing.T) {
	// Resident: the flagship agent seat (every card), the card-2 OCR seat, the card-0 agent seat
	// of the single layer, a model no layer declares (unknown pin) and a memory-stack member.
	f := newMultiSeatSwap("agent-pool", "qwen3-vl-8b", "gemma-4-26b-agent", "mystery-model", "embeddinggemma")
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	old := maintenanceClient
	maintenanceClient = srv.Client()
	t.Cleanup(func() { maintenanceClient = old })
	cfg := scopedDrainConfig(t, srv.URL, "agent-pool")

	var owed string
	out := captureStderr(t, func() {
		if err := maintainSeat(context.Background(), cfg, maintainOpts{unload: true, owed: func(s string) { owed = s }, devices: []string{leaseCard2ID}}); err != nil {
			t.Fatalf("maintainSeatScoped: %v", err)
		}
	})
	got := sortedCalls(f)
	want := []string{"agent-pool", "mystery-model", "qwen3-vl-8b"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("a card-2 lease unloads the seats placed on card 2 and the model nothing places, got %v want %v", got, want)
	}
	f.mu.Lock()
	resident := f.loaded["gemma-4-26b-agent"] && f.loaded["embeddinggemma"]
	f.mu.Unlock()
	if !resident {
		t.Fatal("the seat placed on card 0 and the memory stack must stay resident")
	}
	for _, line := range []string{
		"gpu reserve: left resident, placed elsewhere by layers: gemma-4-26b-agent",
		"gpu reserve: unloaded, placed on a leased card by layers: qwen3-vl-8b",
		"gpu reserve: unloaded, placement unknown (serving_config_path is unset, so the serving config places no model): mystery-model",
	} {
		if !strings.Contains(out, line) {
			t.Fatalf("the lane must say %q:\n%s", line, out)
		}
	}
	if owed != "agent-pool" {
		t.Fatalf("the configured agent seat was on the leased cards and is owed its warm-back, got %q", owed)
	}
}

func TestUnloadSeatLeavesAnAgentSeatOnAnotherCard(t *testing.T) {
	f := newMultiSeatSwap("gemma-4-26b-agent", "qwen3-vl-8b")
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	old := maintenanceClient
	maintenanceClient = srv.Client()
	t.Cleanup(func() { maintenanceClient = old })
	cfg := scopedDrainConfig(t, srv.URL, "gemma-4-26b-agent") // the agent seat is the single layer's, on card 0

	var owed string
	if err := maintainSeat(context.Background(), cfg, maintainOpts{unload: true, owed: func(s string) { owed = s }, devices: []string{leaseCard2ID}}); err != nil {
		t.Fatalf("maintainSeatScoped: %v", err)
	}
	if got := sortedCalls(f); strings.Join(got, ",") != "qwen3-vl-8b" {
		t.Fatalf("only the card-2 seat unloads; the configured agent seat on card 0 stays, got %v", got)
	}
	if owed != "" {
		t.Fatalf("a seat that was never unloaded is owed no warm-back, got %q", owed)
	}
}

func TestUnloadSeatOnAWholeNodeLeaseStillUnloadsEverythingButTheStack(t *testing.T) {
	f := newMultiSeatSwap("agent-pool", "qwen3-vl-8b", "gemma-4-26b-agent", "embeddinggemma")
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	old := maintenanceClient
	maintenanceClient = srv.Client()
	t.Cleanup(func() { maintenanceClient = old })
	cfg := scopedDrainConfig(t, srv.URL, "agent-pool")

	if err := maintainSeat(context.Background(), cfg, maintainOpts{unload: true}); err != nil {
		t.Fatalf("maintainSeatScoped: %v", err)
	}
	if got := sortedCalls(f); strings.Join(got, ",") != "agent-pool,gemma-4-26b-agent,qwen3-vl-8b" {
		t.Fatalf("a whole-node lease clears the whole node (memory stack aside), got %v", got)
	}
}

func TestUnloadSeatFailsClosedWhenTheCardTableCannotBeRead(t *testing.T) {
	f := newMultiSeatSwap("agent-pool", "gemma-4-26b-agent")
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	old := maintenanceClient
	maintenanceClient = srv.Client()
	t.Cleanup(func() { maintenanceClient = old })
	cfg := scopedDrainConfig(t, srv.URL, "agent-pool")
	cardTableFn = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) {
		return nil, "", context.DeadlineExceeded
	}
	if err := maintainSeat(context.Background(), cfg, maintainOpts{unload: true, devices: []string{leaseCard2ID}}); err != nil {
		t.Fatalf("maintainSeatScoped: %v", err)
	}
	if got := sortedCalls(f); strings.Join(got, ",") != "agent-pool,gemma-4-26b-agent" {
		t.Fatalf("with no card table every seat is treated as on the leased cards (today's behaviour), got %v", got)
	}
}

func TestDrainWaitsOnlyForIntersectingRuns(t *testing.T) {
	f := &drainSwap{}
	f.loaded.Store(true)
	f.inflight.Store(3) // the card-0 seat is busy: it must not hold a card-2 lease's drain
	srv := httptest.NewServer(f.handler("gemma-4-26b-agent"))
	defer srv.Close()
	old := maintenanceClient
	maintenanceClient = srv.Client()
	t.Cleanup(func() { maintenanceClient = old })
	cfg := scopedDrainConfig(t, srv.URL, "gemma-4-26b-agent")

	startRun := func(seat string, pins ...string) *gpuactivity.Handle {
		t.Helper()
		h := gpuactivity.Start(cfg.GPULockPath, cfg.StateDir, gpuactivity.Run{Seat: seat, Kind: "contract", Goal: "x", Phase: gpuactivity.PhaseRunning, Devices: pins})
		if h == nil {
			t.Fatal("fixture: could not register a run")
		}
		return h
	}
	onCard0 := startRun("gemma-4-26b-agent", "0")
	t.Cleanup(onCard0.End)
	// A run whose seat nobody declared a pin for (it reads as on every card by name) but whose
	// own record says card 0: the record wins.
	strayOnCard0 := startRun("a-seat-no-layer-declares", "0")
	t.Cleanup(strayOnCard0.End)

	// Only a run on card 0 (and a busy card-0 seat): a card-2 lease has nothing to wait for.
	start := time.Now()
	if err := maintainSeat(context.Background(), cfg, maintainOpts{drain: true, deadline: time.Now().Add(5 * time.Second), devices: []string{leaseCard2ID}}); err != nil {
		t.Fatalf("a drain with nothing on the leased cards must return, got %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("the drain waited %s for work that is not on the leased cards", el)
	}

	// A run on card 2 is exactly what the lease waits for.
	onCard2 := startRun("qwen3-vl-8b", "2")
	ended := make(chan struct{})
	go func() {
		time.Sleep(400 * time.Millisecond)
		onCard2.End()
		close(ended)
	}()
	start = time.Now()
	if err := maintainSeat(context.Background(), cfg, maintainOpts{drain: true, deadline: time.Now().Add(10 * time.Second), devices: []string{leaseCard2ID}}); err != nil {
		t.Fatalf("drain: %v", err)
	}
	<-ended
	if el := time.Since(start); el < 300*time.Millisecond {
		t.Fatalf("the drain returned after %s while a run on the leased card was still registered", el)
	}
}

func TestGPULeaseUnloadModelsListsOnlyWhatMayLeave(t *testing.T) {
	// The render lane's freeLlamaSwap unloads the list the wrapper exports: the roster minus the
	// memory stack minus the seats pinned to cards the lease does not hold.
	roster := []string{"agent-pool", "qwen3-vl-8b", "gemma-4-26b-agent", "mystery-model", "embeddinggemma"}
	cfg := scopedDrainConfig(t, "http://127.0.0.1:1", "agent-pool")
	got, ok := unloadModelsFor(context.Background(), cfg, roster, nil, []string{leaseCard2ID})
	if !ok {
		t.Fatal("a roster and a card table: the list is known")
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "agent-pool,mystery-model,qwen3-vl-8b" {
		t.Fatalf("list = %v, want the card-2 seats and the model it cannot place", got)
	}
	got, ok = unloadModelsFor(context.Background(), cfg, roster, nil, []string{leaseCard0ID})
	sort.Strings(got)
	if !ok || strings.Join(got, ",") != "agent-pool,gemma-4-26b-agent,mystery-model" {
		t.Fatalf("card-0 list = %v ok=%v", got, ok)
	}
	// A whole-node lease exports nothing: the render lane keeps its own rule.
	if got, ok := unloadModelsFor(context.Background(), cfg, roster, nil, nil); ok || got != nil {
		t.Fatalf("a whole-node lease has no list, got %v %v", got, ok)
	}
	if env := unloadModelsEnv([]string{"a", "b"}, true); env != "GPU_LEASE_UNLOAD_MODELS=a,b" {
		t.Fatalf("env = %q", env)
	}
	if env := unloadModelsEnv(nil, true); env != "GPU_LEASE_UNLOAD_MODELS=-" {
		t.Fatalf("an empty list is exported as the explicit none sentinel, got %q", env)
	}
	if env := unloadModelsEnv(nil, false); env != "" {
		t.Fatalf("an unknown list exports nothing, got %q", env)
	}
	_ = strconv.Itoa
}

// ---- the wrapper form, end to end ---------------------------------------------------------

// TestHelperDumpUnloadEnv is the wrapped command of the wrapper-form test: it writes the
// GPU_LEASE_UNLOAD_MODELS it was handed to the file LO_HELPER_ENV_OUT names.
func TestHelperDumpUnloadEnv(t *testing.T) {
	out := os.Getenv("LO_HELPER_ENV_OUT")
	if out == "" {
		return
	}
	v, set := os.LookupEnv("GPU_LEASE_UNLOAD_MODELS")
	body := "unset"
	if set {
		body = "set:" + v
	}
	_ = os.WriteFile(out, []byte(body), 0o644)
}

// swapWithRoster is multiSeatSwap plus the roster route the wrapper reads.
func swapWithRoster(f *multiSeatSwap, ids ...string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		var data []map[string]any
		for _, id := range ids {
			data = append(data, map[string]any{"id": id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	})
	// Every seat answers its own gauge at its proxy address: nothing in flight.
	mux.HandleFunc("/direct/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/metrics") {
			_, _ = w.Write([]byte("vllm:num_requests_running{engine=\"0\"} 0\nvllm:num_requests_waiting{engine=\"0\"} 0.0\n"))
			return
		}
		http.NotFound(w, r)
	})
	mux.Handle("/", f.handler())
	return httptest.NewServer(mux)
}

func cardLeaseWrapperFixture(t *testing.T, srvURL, agentSeat string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "gpu"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "gpu", "reader-audit.json"), []byte(`{"result":"green"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fx := config.FlagshipFixture()
	b, err := json.Marshal(map[string]any{
		"state_dir": root, "endpoint": srvURL, "agent_model": agentSeat, "gpu_card_scoped_leases": true,
		"layers": fx.Layers, "tier_profile": fx.TierProfile, "tiers": fx.Tiers,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	old := cardTableFn
	cardTableFn = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) { return statusCards(), "", nil }
	t.Cleanup(func() { cardTableFn = old })
	return path
}

func TestReserveACardScopesTheUnloadAndHandsTheRenderLaneItsList(t *testing.T) {
	f := newMultiSeatSwap("agent-pool", "qwen3-vl-8b", "gemma-4-26b-agent", "mystery-model", "embeddinggemma")
	f.agentSeat = "agent-pool"
	srv := swapWithRoster(f, "agent-pool", "qwen3-vl-8b", "gemma-4-26b-agent", "mystery-model", "embeddinggemma")
	defer srv.Close()
	old := maintenanceClient
	maintenanceClient = srv.Client()
	t.Cleanup(func() { maintenanceClient = old })
	cfgPath := cardLeaseWrapperFixture(t, srv.URL, "agent-pool")
	envOut := filepath.Join(t.TempDir(), "env.txt")
	t.Setenv("LO_HELPER_ENV_OUT", envOut)

	args := []string{"--config", cfgPath, "--wait", "10s", "--class", "media", "--devices", "2", "--drain", "--unload-seat", "--reason", "card 2 render",
		"--", os.Args[0], "-test.run=^TestHelperDumpUnloadEnv$", "-test.timeout=30s"}
	if err := runGPUReserve(args); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if got := sortedCalls(f); strings.Join(got, ",") != "agent-pool,mystery-model,qwen3-vl-8b" {
		t.Fatalf("a card-2 reservation unloads the card-2 seats and the one it cannot place, got %v", got)
	}
	f.mu.Lock()
	stays := f.loaded["gemma-4-26b-agent"] && f.loaded["embeddinggemma"]
	warms, warmed := f.warms, f.loaded["agent-pool"]
	f.mu.Unlock()
	if !stays {
		t.Fatal("the card-0 seat and the memory stack must stay resident")
	}
	// The wrapper warms the agent seat back when its command ends. This test used to pass
	// over a stand-in that answered that request 418, because a status below 500 counted as a
	// warm; it is now a failure, so the stand-in serves the agent seat's warm and the test says
	// the warm happened.
	if warms != 1 || !warmed {
		t.Fatalf("the wrapper must warm the agent seat back once its command ends: warms=%d loaded=%v", warms, warmed)
	}
	raw, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatalf("the wrapped command did not run: %v", err)
	}
	if got := string(raw); !strings.HasPrefix(got, "set:") || !strings.Contains(got, "qwen3-vl-8b") || strings.Contains(got, "gemma-4-26b-agent") || strings.Contains(got, "embeddinggemma") || !strings.Contains(got, "mystery-model") {
		t.Fatalf("the render lane is handed the models that may leave (not the card-0 seat, not the stack; the model nothing places is on it), got %q", got)
	}
}

func TestReserveAWholeNodeLeaseHandsTheRenderLaneNoList(t *testing.T) {
	f := newMultiSeatSwap("agent-pool", "embeddinggemma")
	srv := swapWithRoster(f, "agent-pool", "embeddinggemma")
	defer srv.Close()
	old := maintenanceClient
	maintenanceClient = srv.Client()
	t.Cleanup(func() { maintenanceClient = old })
	cfgPath := cardLeaseWrapperFixture(t, srv.URL, "agent-pool")
	envOut := filepath.Join(t.TempDir(), "env.txt")
	t.Setenv("LO_HELPER_ENV_OUT", envOut)

	args := []string{"--config", cfgPath, "--wait", "10s", "--class", "media", "--whole-node", "--reason", "legacy-shaped render",
		"--", os.Args[0], "-test.run=^TestHelperDumpUnloadEnv$", "-test.timeout=30s"}
	if err := runGPUReserve(args); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	raw, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatalf("the wrapped command did not run: %v", err)
	}
	if string(raw) != "unset" {
		t.Fatalf("a whole-node lease leaves the render lane on its own rule: no list, got %q", raw)
	}
}

func TestLeaseDevicesOfFindsTheEpochsCards(t *testing.T) {
	_, m := scopedLeaseFixture(t)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "r", Devices: []string{leaseCard2ID}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	if got, err := leaseDevicesOf(m, l.Epoch()); err != nil || len(got) != 1 || got[0] != leaseCard2ID {
		t.Fatalf("leaseDevicesOf = %v, %v", got, err)
	}
	if got, err := leaseDevicesOf(m, l.Epoch()+99); err != nil || got != nil {
		t.Fatalf("an epoch that is not live has no cards, got %v, %v", got, err)
	}
}

// The detached holder's maintain step reads the cards of ITS lease by epoch.
func TestDetachedMaintainScopesToTheEpochsCards(t *testing.T) {
	f := newMultiSeatSwap("agent-pool", "qwen3-vl-8b", "gemma-4-26b-agent")
	srv := swapWithRoster(f, "agent-pool", "qwen3-vl-8b", "gemma-4-26b-agent")
	defer srv.Close()
	old := maintenanceClient
	maintenanceClient = srv.Client()
	t.Cleanup(func() { maintenanceClient = old })
	cfgPath := cardLeaseWrapperFixture(t, srv.URL, "agent-pool")
	cfg := loadCfgPath(cfgPath)
	m, err := gpulease.OpenAt("", cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "detached card-2 render", Devices: []string{leaseCard2ID}, Draining: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })

	if err := detachMaintain(m, l.Epoch(), cfg, true, time.Now().Add(10*time.Second), true, true); err != nil {
		t.Fatalf("detachMaintain: %v", err)
	}
	if got := sortedCalls(f); strings.Join(got, ",") != "agent-pool,qwen3-vl-8b" {
		t.Fatalf("the detached holder clears its own cards only, got %v", got)
	}
}

// A seat pinned to a card the table does not have cannot be placed: it is on the leased cards
// (placement unknown goes, as before F15), and the lane says why.
func TestUnloadSeatTreatsAnUnplaceablePinAsOnTheLeasedCards(t *testing.T) {
	f := newMultiSeatSwap("agent-pool", "ghost-seat")
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	old := maintenanceClient
	maintenanceClient = srv.Client()
	t.Cleanup(func() { maintenanceClient = old })
	root := t.TempDir()
	fx := config.FlagshipFixture()
	layers := append([]config.LayerSpec(nil), fx.Layers...)
	layers[0].Seats = append(append([]config.LayerSeat(nil), layers[0].Seats...), config.LayerSeat{Role: "stt", Model: "ghost-seat", Device: "7"})
	b, _ := json.Marshal(map[string]any{"state_dir": root, "endpoint": srv.URL, "agent_model": "agent-pool", "layers": layers, "tier_profile": fx.TierProfile, "tiers": fx.Tiers})
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	oldCards := cardTableFn
	cardTableFn = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) { return statusCards(), "", nil }
	t.Cleanup(func() { cardTableFn = oldCards })
	out := captureStderr(t, func() {
		if err := maintainSeat(context.Background(), loadCfgPath(path), maintainOpts{unload: true, devices: []string{leaseCard2ID}}); err != nil {
			t.Fatal(err)
		}
	})
	if got := sortedCalls(f); strings.Join(got, ",") != "agent-pool,ghost-seat" {
		t.Fatalf("a pin the card table cannot place reads as on the leased cards, got %v", got)
	}
	if !strings.Contains(out, "unloaded, placement unknown (layers pin it to a card the card table cannot place): ghost-seat") {
		t.Fatalf("the seat unloaded on a doubt is named with the reason:\n%s", out)
	}
}

// A model layers do not declare is placed by its entry's device pin in the serving config
// (serving_config_path, register F15): pinned to another card it stays, pinned to a leased card it
// goes, and a proxy: seat (its cards live in its own engine) is unknown and goes with the reason.
func TestUnloadSeatPlacesOtherResidentsByTheServingConfigPin(t *testing.T) {
	f := newMultiSeatSwap("agent-pool", "qwen3-vl-8b", "gemma-4-26b-agent", "yaml-elsewhere", "yaml-here", "yaml-proxy", "embeddinggemma")
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	old := maintenanceClient
	maintenanceClient = srv.Client()
	t.Cleanup(func() { maintenanceClient = old })
	cfg := scopedDrainConfig(t, srv.URL, "agent-pool")
	yamlPath := filepath.Join(t.TempDir(), "llama-swap.yaml")
	body := `models:
  yaml-elsewhere:
    env: ["CUDA_VISIBLE_DEVICES=GPU-aaaa0000-x"]
  yaml-here:
    env: ["CUDA_VISIBLE_DEVICES=GPU-cccc0000-x"]
  yaml-proxy:
    proxy: http://127.0.0.1:1234
    env: ["CUDA_VISIBLE_DEVICES=GPU-aaaa0000-x"]
`
	if err := os.WriteFile(yamlPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.ServingConfigPath = yamlPath
	out := captureStderr(t, func() {
		if err := maintainSeat(context.Background(), cfg, maintainOpts{unload: true, devices: []string{leaseCard2ID}}); err != nil {
			t.Fatalf("maintainSeatScoped: %v", err)
		}
	})
	if got := sortedCalls(f); strings.Join(got, ",") != "agent-pool,qwen3-vl-8b,yaml-here,yaml-proxy" {
		t.Fatalf("unloaded %v: the card-2 models and the proxy: seat go, the models placed on card 0 stay", got)
	}
	f.mu.Lock()
	stays := f.loaded["gemma-4-26b-agent"] && f.loaded["yaml-elsewhere"] && f.loaded["embeddinggemma"]
	f.mu.Unlock()
	if !stays {
		t.Fatal("the seat layers put on card 0, the model the serving config pins to card 0 and the memory stack must stay resident")
	}
	for _, line := range []string{
		"gpu reserve: left resident, placed elsewhere by the serving config pin: yaml-elsewhere",
		"gpu reserve: unloaded, placed on a leased card by the serving config pin: yaml-here",
		"proxy: seat",
	} {
		if !strings.Contains(out, line) {
			t.Fatalf("the lane must say %q:\n%s", line, out)
		}
	}
}

// The legacy GET /unload is total. Beside a seat that has to stay it is refused, the same rule as
// the memory stack's.
func TestUnloadSeatNeverFallsBackToTheTotalUnloadWhileASeatOnAnotherCardIsResident(t *testing.T) {
	f := newMultiSeatSwap("agent-pool", "gemma-4-26b-agent")
	f.perModelMissing = true
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	old := maintenanceClient
	maintenanceClient = srv.Client()
	t.Cleanup(func() { maintenanceClient = old })
	cfg := scopedDrainConfig(t, srv.URL, "agent-pool")
	err := maintainSeat(context.Background(), cfg, maintainOpts{unload: true, devices: []string{leaseCard2ID}})
	if f.bulkCalls != 0 {
		t.Fatalf("GET /unload (total) ran %d time(s) beside a card-0 seat that has to stay", f.bulkCalls)
	}
	f.mu.Lock()
	stays := f.loaded["gemma-4-26b-agent"]
	f.mu.Unlock()
	if !stays {
		t.Fatal("the card-0 seat was torn down")
	}
	if err == nil {
		t.Fatal("the seat could not be unloaded selectively: the reserve must fail and say why")
	}
}

// `gpu reserve --detach ... --drain --unload-seat`: the call site hands the hidden holder's cards
// to the maintain step. The holder is in-process here (detachHolderFn); the real one is a hidden
// child process.
func TestReserveDetachScopesTheMaintainToTheHoldersCards(t *testing.T) {
	f := newMultiSeatSwap("agent-pool", "qwen3-vl-8b", "gemma-4-26b-agent")
	srv := swapWithRoster(f, "agent-pool", "qwen3-vl-8b", "gemma-4-26b-agent")
	defer srv.Close()
	old := maintenanceClient
	maintenanceClient = srv.Client()
	t.Cleanup(func() { maintenanceClient = old })
	cfgPath := cardLeaseWrapperFixture(t, srv.URL, "agent-pool")

	var held *gpulease.Lease
	oldFn := detachHolderFn
	detachHolderFn = func(_ *flag.FlagSet, class string, _, _ time.Duration, opts gpulease.Options, _ *reserveDeviceFlags, _ bool, m *gpulease.Manager) (uint64, error) {
		l, err := m.TryAcquire(gpulease.Class(class), opts)
		if err != nil {
			return 0, err
		}
		held = l
		return l.Epoch(), nil
	}
	t.Cleanup(func() {
		detachHolderFn = oldFn
		if held != nil {
			_ = held.Release()
		}
	})
	args := []string{"--config", cfgPath, "--detach", "--for", "1h", "--wait", "5s", "--class", "media", "--devices", "2", "--drain", "--unload-seat", "--reason", "detached card-2 render"}
	if err := runGPUReserve(args); err != nil {
		t.Fatalf("reserve --detach: %v", err)
	}
	if held == nil {
		t.Fatal("the in-process holder never took the lease")
	}
	if got := sortedCalls(f); strings.Join(got, ",") != "agent-pool,qwen3-vl-8b" {
		t.Fatalf("a detached card-2 hold clears card 2 only, got %v", got)
	}
}

// `gpu status` and offload_status hand the activity snapshot the evidence rule's scope.
func TestActivityOptionsCarryTheLeaseScope(t *testing.T) {
	cfg := scopedDrainConfig(t, "http://127.0.0.1:1", "agent-pool")
	opts := activityOptions(cfg)
	if opts.Scope == nil {
		t.Fatal("the activity snapshot needs the scope function to see a legacy lease's inferred cards")
	}
}

// The drain and the unload fall back to today's whole-node behaviour when the facts they narrow
// by cannot be read. Falling back is right (the direction of every doubt is "fence"); doing it
// silently is not, because the operator then believes the card-0 seat was spared.
func TestScopedMaintainSaysWhenTheCardTableCannotBeRead(t *testing.T) {
	f := newMultiSeatSwap("agent-pool", "gemma-4-26b-agent")
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	old := maintenanceClient
	maintenanceClient = srv.Client()
	t.Cleanup(func() { maintenanceClient = old })
	cfg := scopedDrainConfig(t, srv.URL, "agent-pool")
	oldCards := cardTableFn
	cardTableFn = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) {
		return nil, "", context.DeadlineExceeded
	}
	t.Cleanup(func() { cardTableFn = oldCards })
	out := captureStderr(t, func() {
		if err := maintainSeat(context.Background(), cfg, maintainOpts{unload: true, devices: []string{leaseCard2ID}}); err != nil {
			t.Fatalf("maintainSeatScoped: %v", err)
		}
	})
	if !strings.Contains(out, "card table could not be read") || !strings.Contains(out, "every seat") || !strings.Contains(out, "deadline exceeded") {
		t.Fatalf("the operator must be told the scoped unload fell back to every seat, and why:\n%s", out)
	}
}

// A roster that could NOT be read is not an empty roster. The wrapper then has no idea what is
// resident, so it hands the render lane NO list, and the lane keeps its own rule: every model off the
// memory stack (register F15; the old answer judged only the models the config declares, so with no
// layers and no serving config a roster timeout unloaded nothing, and with layers a resident model
// neither layers nor the yaml name stayed on the leased card). The wrapper says why.
func TestWrapperUnloadEnvWithAnUnreadableRosterLeavesTheLaneOnItsOwnRule(t *testing.T) {
	for name, mk := range map[string]func(t *testing.T) config.Config{
		"no layers and no serving config": func(t *testing.T) config.Config {
			cfg := config.Default()
			cfg.Endpoint = "http://127.0.0.1:1"
			return cfg
		},
		"layers only": func(t *testing.T) config.Config {
			return scopedDrainConfig(t, "http://127.0.0.1:1", "agent-pool")
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := mk(t)
			old := rosterIDsFn
			rosterIDsFn = func(context.Context, string) ([]string, error) { return nil, errors.New("roster refused") }
			t.Cleanup(func() { rosterIDsFn = old })
			var got string
			out := captureStderr(t, func() { got = wrapperUnloadEnv(context.Background(), cfg, []string{leaseCard2ID}) })
			if got != "" {
				t.Fatalf("a roster that could not be read leaves the variable unset, so the lane keeps its own rule (every model off the memory stack); got %q", got)
			}
			for _, want := range []string{"roster could not be read", "roster refused", "own rule"} {
				if !strings.Contains(out, want) {
					t.Fatalf("the wrapper must say it fell back because the roster could not be read (%q missing):\n%s", want, out)
				}
			}
		})
	}
}

// A roster that WAS read and is empty means nothing is resident: a card lease still gets a list (the
// none sentinel on a box that places nothing), never the unset variable.
func TestWrapperUnloadEnvWithAnEmptyRosterThatWasReadIsTheNoneSentinel(t *testing.T) {
	cfg := config.Default()
	cfg.Endpoint = "http://127.0.0.1:1"
	old := rosterIDsFn
	rosterIDsFn = func(context.Context, string) ([]string, error) { return nil, nil }
	t.Cleanup(func() { rosterIDsFn = old })
	if got := wrapperUnloadEnv(context.Background(), cfg, []string{leaseCard2ID}); got != "GPU_LEASE_UNLOAD_MODELS=-" {
		t.Fatalf("an empty roster that was read: want the none sentinel, got %q", got)
	}
}

// A card lease on a box that cannot reach llama-swap at all (no endpoint configured) has nothing to
// unload: the list is the explicit none sentinel, not an unset variable.
func TestWrapperUnloadEnvForACardLeaseIsNeverUnset(t *testing.T) {
	cfg := config.Default()
	cfg.Endpoint = ""
	if got := wrapperUnloadEnv(context.Background(), cfg, []string{leaseCard2ID}); got != "GPU_LEASE_UNLOAD_MODELS=-" {
		t.Fatalf("no endpoint, a card lease: want the none sentinel, got %q", got)
	}
	if got := wrapperUnloadEnv(context.Background(), cfg, nil); got != "" {
		t.Fatalf("a whole-node lease keeps the lane's own rule: no variable, got %q", got)
	}
}

func TestLeaseDevicesOfSaysWhenTheEpochIsNotLive(t *testing.T) {
	_, m := scopedLeaseFixture(t)
	var got []string
	var err error
	out := captureStderr(t, func() { got, err = leaseDevicesOf(m, 4242) })
	if err != nil {
		t.Fatalf("an epoch that is not live is not an error, got %v", err)
	}
	if got != nil {
		t.Fatalf("an epoch that is not live has no cards, got %v", got)
	}
	if !strings.Contains(out, "4242") || !strings.Contains(out, "not live") || !strings.Contains(out, "whole node") {
		t.Fatalf("the holder must say its drain and unload run for the whole node because the lease is not live:\n%s", out)
	}
}
