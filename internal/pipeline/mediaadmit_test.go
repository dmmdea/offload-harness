package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/comfyinst"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpualloc"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// Per-card media admission (plan P13). On a host with card-scoped leases a generation call asks
// the allocator for ONE card, holds a lease on it, and runs in a ComfyUI instance bound to that
// card: two calls run on two cards at once, a third queues with a token. Everything below runs
// against a scratch lease root and stub endpoints; no real card, ComfyUI or lease is touched.

// Synthetic ids (repeated-nibble heads, the shape the leak gate treats as placeholders).
const (
	admitUUIDA = "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee" // nvidia index 0
	admitUUIDB = "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff" // nvidia index 1, the monitor is attached
	admitUUIDC = "GPU-cccc3333-dddd-eeee-ffff-000000000000" // nvidia index 2
)

// admitProbe is what the stub runner reports about the process it was spawned as.
type admitProbe struct {
	PID  int               `json:"pid"`
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env"`
}

// admitRunner is a stub for comfy-generate.mjs and the other single-output runners: it records
// its environment, waits until the test says go (so several calls are in flight together), then
// writes its output file and exits.
const admitRunnerSrc = `import {writeFileSync, existsSync} from "node:fs";
import {join} from "node:path";
const dir = process.env.ADMIT_DIR;
const env = {};
for (const k of ["COMFY_CARD_UUID", "COMFY_API", "COMFY_CUDA_DEVICE", "COMFY_DYNAMIC_VRAM", "COMFY_EXTRA_ARGS", "COMFY_DIR",
  "GPU_LEASE_DIR", "GPU_LEASE_EPOCH", "GPU_LEASE_CLASS", "GPU_LEASE_DEVICES", "GPU_LEASE_UNLOAD_MODELS"]) {
  if (k in process.env) env[k] = process.env[k];
}
writeFileSync(join(dir, "started-" + process.pid + ".json"), JSON.stringify({pid: process.pid, argv: process.argv.slice(2), env}));
const deadline = Date.now() + 30000;
while (!existsSync(join(dir, "go")) && Date.now() < deadline) await new Promise((r) => setTimeout(r, 20));
const out = process.argv[2];
if (out && !out.startsWith("--")) writeFileSync(out, "not-an-image");
`

type admitFixture struct {
	t      *testing.T
	p      *Pipeline
	cfg    config.Config
	dir    string
	root   string
	m      *gpulease.Manager
	cards  []gpuprobe.Card
	frees  *admitFrees
	stops  *admitStops
	away   bool
	roster *httptest.Server
}

type admitFrees struct {
	mu   sync.Mutex
	hits []string
	srv  *httptest.Server
}

func (f *admitFrees) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hits...)
}

type admitStops struct {
	mu    sync.Mutex
	calls []stopCall
	m     *gpulease.Manager
}

type stopCall struct {
	dir   string
	epoch uint64
	held  bool
}

func (s *admitStops) list() []stopCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stopCall(nil), s.calls...)
}

// newAdmitFixture builds a pipeline on a card-scoped host: a scratch state root with a green
// reader audit, a three-card table (card 1 is the display card), a roster and a /free recorder.
func newAdmitFixture(t *testing.T, mutate func(*config.Config)) *admitFixture {
	t.Helper()
	requireNodePipeline(t)
	f := &admitFixture{t: t, dir: t.TempDir(), root: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(f.root, "gpu"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "gpu", "reader-audit.json"), []byte(`{"result":"green"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := filepath.Join(f.dir, "admit-runner.mjs")
	if err := os.WriteFile(runner, []byte(admitRunnerSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADMIT_DIR", f.dir)
	t.Setenv("PNG_SRC", "")

	devs := []gpuprobe.Device{
		{Index: 0, UUID: admitUUIDA, Name: "NVIDIA GeForce RTX 5060 Ti", TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
		{Index: 1, UUID: admitUUIDB, Name: "NVIDIA GeForce RTX 5070 Ti", TotalGiB: 16, FreeGiB: 14, UtilKnown: true, DisplayAttached: true},
		{Index: 2, UUID: admitUUIDC, Name: "NVIDIA GeForce RTX 5060 Ti", TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
	}
	f.cards, _ = gpuprobe.BuildCards(devs, "")

	f.frees = &admitFrees{}
	f.frees.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.frees.mu.Lock()
		f.frees.hits = append(f.frees.hits, r.Method+" "+r.URL.Path)
		f.frees.mu.Unlock()
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(f.frees.srv.Close)

	f.roster = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"seat-a"},{"id":"seat-c"},{"id":"seat-unpinned"},{"id":"embeddinggemma"}]}`))
	}))
	t.Cleanup(f.roster.Close)

	cfg := config.Default()
	cfg.MediaDir = f.dir
	cfg.StateDir = f.root
	cfg.ComfyDir = filepath.Join(f.dir, "comfy")
	cfg.ImageGenScript = runner
	cfg.GPUCardScopedLeases = true
	cfg.GPUWaitMs = 300
	cfg.Endpoint = f.roster.URL
	cfg.Layers = []config.LayerSpec{{Name: "single", Seats: []config.LayerSeat{
		{Role: "agent", Model: "seat-a", Device: "0"},
		{Role: "vision", Model: "seat-c", Device: "2"},
	}}}
	if mutate != nil {
		mutate(&cfg)
	}
	f.cfg = cfg

	m, err := gpulease.OpenAt("", f.root)
	if err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	f.m = m
	f.stops = &admitStops{m: m}
	f.p = &Pipeline{cfg: cfg}
	f.p.alloc = gpualloc.Deps{
		Cards:       func(context.Context, config.Config) ([]gpuprobe.Card, string, error) { return f.cards, "", nil },
		HostFreeRAM: func() (float64, bool) { return 64, true },
		Presence:    func(config.Config) (bool, bool) { return true, f.away },
	}
	f.p.instanceAPI = func(c gpuprobe.Card) string { return fmt.Sprintf("%s/card%d", f.frees.srv.URL, c.NvidiaIndex) }
	f.p.stopKept = func(_ context.Context, dir string, epoch uint64) []comfyinst.Outcome {
		f.stops.mu.Lock()
		defer f.stops.mu.Unlock()
		f.stops.calls = append(f.stops.calls, stopCall{dir: dir, epoch: epoch, held: m.Inspect().Held})
		return nil
	}
	return f
}

// image starts one generate_image call in the background and returns its result channel.
func (f *admitFixture) image(extra map[string]any) <-chan core.Result {
	f.t.Helper()
	params := map[string]any{"out": filepath.Join(f.dir, fmt.Sprintf("out-%d.png", time.Now().UnixNano()))}
	for k, v := range extra {
		params[k] = v
	}
	ch := make(chan core.Result, 1)
	go func() {
		ch <- f.p.Run(context.Background(), core.Request{Task: core.TaskGenerateImage, Input: "a calm ocean at dawn", Params: params})
	}()
	return ch
}

// started reads the probes of the runners that are in flight.
func (f *admitFixture) started() []admitProbe {
	f.t.Helper()
	entries, _ := filepath.Glob(filepath.Join(f.dir, "started-*.json"))
	var out []admitProbe
	for _, e := range entries {
		b, err := os.ReadFile(e)
		if err != nil {
			continue
		}
		var pr admitProbe
		if json.Unmarshal(b, &pr) == nil {
			out = append(out, pr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Env["COMFY_CARD_UUID"] < out[j].Env["COMFY_CARD_UUID"] })
	return out
}

func (f *admitFixture) waitStarted(n int) []admitProbe {
	f.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.started(); len(got) >= n {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.t.Fatalf("only %d runner(s) started, want %d", len(f.started()), n)
	return nil
}

func (f *admitFixture) letRunnersGo() {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "go"), []byte("go"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *admitFixture) await(ch <-chan core.Result) core.Result {
	f.t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(60 * time.Second):
		f.t.Fatal("a media call never returned")
		return core.Result{}
	}
}

func leaseIDOf(uuid string) string { return strings.ToLower(uuid) }

// ---------------------------------------------------------------------------------------------

// TestTwoMediaCallsRunOnTwoFreeCards: two generate_image calls in flight together run on two
// different cards, each in the ComfyUI instance bound to its card, under two live leases that
// each hold ONE card, and neither is the card the monitor is attached to.
func TestTwoMediaCallsRunOnTwoFreeCards(t *testing.T) {
	f := newAdmitFixture(t, nil)
	a, b := f.image(nil), f.image(nil)
	probes := f.waitStarted(2)
	if len(probes) != 2 {
		t.Fatalf("%d runners in flight, want 2", len(probes))
	}

	seen := map[string]bool{}
	for _, pr := range probes {
		uuid := pr.Env["COMFY_CARD_UUID"]
		if uuid != admitUUIDA && uuid != admitUUIDC {
			t.Errorf("COMFY_CARD_UUID = %q, want one of the two non-display cards (the monitor is on %s): env %v", uuid, admitUUIDB, pr.Env)
		}
		seen[uuid] = true
		if pr.Env["COMFY_CUDA_DEVICE"] != "" {
			t.Errorf("a card-bound instance is pinned by uuid, never by an index: COMFY_CUDA_DEVICE=%q", pr.Env["COMFY_CUDA_DEVICE"])
		}
		wantAPI := ""
		for _, c := range f.cards {
			if c.UUID == uuid {
				wantAPI = fmt.Sprintf("%s/card%d", f.frees.srv.URL, c.NvidiaIndex)
			}
		}
		if pr.Env["COMFY_API"] != wantAPI {
			t.Errorf("COMFY_API = %q, want the instance of card %s (%q)", pr.Env["COMFY_API"], uuid, wantAPI)
		}
		if pr.Env["GPU_LEASE_DEVICES"] != leaseIDOf(uuid) {
			t.Errorf("GPU_LEASE_DEVICES = %q, want %q", pr.Env["GPU_LEASE_DEVICES"], leaseIDOf(uuid))
		}
		if pr.Env["GPU_LEASE_CLASS"] != "media" || pr.Env["GPU_LEASE_EPOCH"] == "" || pr.Env["GPU_LEASE_DIR"] == "" {
			t.Errorf("lease env incomplete: %v", pr.Env)
		}
		// A render on one card must not empty the seats on the others (register C-86): the unload
		// list is the roster minus the memory stack minus the seats pinned to other cards.
		unload := pr.Env["GPU_LEASE_UNLOAD_MODELS"]
		switch uuid {
		case admitUUIDA:
			if unload != "seat-a,seat-unpinned" {
				t.Errorf("card 0's render may unload %q, want seat-a and the unpinned seat only", unload)
			}
		case admitUUIDC:
			if unload != "seat-c,seat-unpinned" {
				t.Errorf("card 2's render may unload %q, want seat-c and the unpinned seat only", unload)
			}
		}
	}
	if len(seen) != 2 {
		t.Fatalf("both calls ran on the same card: %v", seen)
	}

	leases := f.m.Leases()
	if len(leases) != 2 {
		t.Fatalf("%d live leases, want 2 (one per call): %+v", len(leases), leases)
	}
	held := map[string]bool{}
	for _, l := range leases {
		if len(l.Devices) != 1 {
			t.Errorf("lease %d holds %v, want exactly one card", l.Epoch, l.Devices)
		}
		for _, d := range l.Devices {
			held[d] = true
		}
	}
	if len(held) != 2 || held[leaseIDOf(admitUUIDB)] {
		t.Errorf("held cards = %v, want the two non-display cards", held)
	}

	f.letRunnersGo()
	for i, ch := range []<-chan core.Result{a, b} {
		if r := f.await(ch); !r.OK {
			t.Errorf("call %d did not complete: %+v", i, r)
		}
	}
	if left := f.m.Leases(); len(left) != 0 {
		t.Errorf("leases left after both calls: %+v", left)
	}
}

// TestThirdMediaCallQueuesWithToken: with both free cards in use a third call waits its window
// and answers with a place in line, not a refusal.
func TestThirdMediaCallQueuesWithToken(t *testing.T) {
	f := newAdmitFixture(t, nil)
	a, b := f.image(nil), f.image(nil)
	f.waitStarted(2)

	start := time.Now()
	res := f.await(f.image(nil))
	if time.Since(start) < 250*time.Millisecond {
		t.Errorf("returned after %v: the call must wait its gpu_wait_ms window (300 ms here) before answering", time.Since(start))
	}
	if res.OK || !res.Deferred {
		t.Fatalf("want a deferred result, got %+v", res)
	}
	if res.Meta.ErrClass == "gpu_busy" {
		t.Fatalf("the busy-card refusal is replaced by a place in line, got err class %q: %s", res.Meta.ErrClass, res.Reason)
	}
	if res.Meta.ErrClass != "gpu_queued" || res.DeferClass != core.DeferClassCapacity {
		t.Errorf("err class %q defer class %q, want gpu_queued / capacity", res.Meta.ErrClass, res.DeferClass)
	}
	var payload struct {
		Queued       bool     `json:"queued"`
		WaiterToken  string   `json:"waiter_token"`
		QueuePos     int      `json:"queue_position"`
		ETASec       *int     `json:"eta_s"`
		Devices      []string `json:"devices"`
		ResumeHint   string   `json:"resume"`
		HeldByEpochs []uint64 `json:"held_by"`
	}
	if err := json.Unmarshal(res.Data, &payload); err != nil {
		t.Fatalf("the queued result carries its place as data: %v (%s)", err, res.Data)
	}
	if !payload.Queued || !strings.HasPrefix(payload.WaiterToken, "tk-") || payload.QueuePos != 1 || payload.ETASec == nil || len(payload.Devices) != 1 {
		t.Errorf("payload = %+v, want queued with a token, position 1, an ETA and the one card it waits for", payload)
	}
	if !strings.Contains(res.Reason, payload.WaiterToken) {
		t.Errorf("the reason must carry the token so a reader of the text can resume: %q", res.Reason)
	}
	if _, ok := f.m.ResumeToken(payload.WaiterToken); !ok {
		t.Error("the token must be on disk, resumable")
	}

	// Free the cards, then resume with the token: the call runs and the token is consumed.
	f.letRunnersGo()
	f.await(a)
	f.await(b)
	again := f.await(f.image(map[string]any{"waiter_token": payload.WaiterToken}))
	if !again.OK {
		t.Fatalf("the resumed call must run once a card is free: %+v", again)
	}
	if _, ok := f.m.ResumeToken(payload.WaiterToken); ok {
		t.Error("a token whose call ran is dropped")
	}
	if toks := f.m.Tokens(); len(toks) != 0 {
		t.Errorf("tokens left over: %+v", toks)
	}
}
