package pairworkloads

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/swapclient"

	"llamaswap-pp-cli/pkg/llamaswap"
)

// fakeSwap is a llama-swap with one controllable vLLM seat (and one llama.cpp
// seat that must never be watched). The vLLM seat's /metrics is served by a
// SEPARATE server — the seat's own address, reported as `proxy` in /running
// exactly as llama-swap does — and every path either server is asked for is
// recorded, so a test can prove the watcher never goes through /upstream.
type fakeSwap struct {
	mu       sync.Mutex
	loaded   bool
	running  int
	waiting  int
	badStats bool
	seatURL  string   // the vLLM seat's own server (its `proxy:`)
	swapHits []string // every path asked of llama-swap
	seatHits []string // every path asked of the seat
}

func (f *fakeSwap) set(fn func(*fakeSwap)) {
	f.mu.Lock()
	fn(f)
	f.mu.Unlock()
}

func (f *fakeSwap) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.swapHits = append(f.swapHits, r.URL.Path)
	switch {
	case r.URL.Path == "/running":
		models := `{"model":"qwen3.5-9b-agent","state":"ready","proxy":"http://127.0.0.1:1"}`
		if f.loaded {
			models = fmt.Sprintf(`{"model":"qwen3.8-27b-vllm-3card","state":"ready","proxy":%q},`, f.seatURL) + models
		}
		fmt.Fprintf(w, `{"running":[%s]}`, models)
	default:
		t := "unexpected " + r.URL.Path
		http.Error(w, t, http.StatusNotFound)
	}
}

// seatHandler is the vLLM seat itself, at the address /running reports.
func (f *fakeSwap) seatHandler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seatHits = append(f.seatHits, r.URL.Path)
	switch {
	case r.URL.Path == "/metrics":
		if f.badStats {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprintf(w, "# HELP vllm:num_requests_running x\n"+
			"vllm:num_requests_running{engine=\"0\",model_name=\"m\"} %d.0\n"+
			"vllm:num_requests_waiting{engine=\"0\",model_name=\"m\"} %d.0\n"+
			"vllm:num_requests_waiting_by_reason{engine=\"0\",model_name=\"m\",reason=\"capacity\"} %d.0\n",
			f.running, f.waiting, f.waiting)
	default:
		t := "unexpected " + r.URL.Path
		http.Error(w, t, http.StatusNotFound)
	}
}

type seatRig struct {
	swap    *fakeSwap
	pair    *capture
	w       *SeatWatcher
	harness map[string]int
	clock   time.Time
}

func newSeatRig(t *testing.T) *seatRig {
	t.Helper()
	rig := &seatRig{swap: &fakeSwap{loaded: true}, pair: &capture{}, harness: map[string]int{}, clock: time.UnixMilli(1_000_000)}
	swapSrv := httptest.NewServer(http.HandlerFunc(rig.swap.handler))
	t.Cleanup(swapSrv.Close)
	seatSrv := httptest.NewServer(http.HandlerFunc(rig.swap.seatHandler))
	t.Cleanup(seatSrv.Close)
	rig.swap.seatURL = seatSrv.URL
	pairSrv := httptest.NewServer(http.HandlerFunc(rig.pair.handler))
	t.Cleanup(pairSrv.Close)

	cfg := config.Config{Endpoint: swapSrv.URL, VLLMSeats: []string{"qwen3.8-27b-vllm", "qwen3.8-27b-vllm-3card"},
		PairSeatActivityEnabled: true, PairWorkloadsEndpoint: pairSrv.URL}
	wc := SeatWatchConfig(cfg)
	wc.AppDir = writePairAppDir(t)
	rig.w = NewSeatWatcher(New(wc), cfg)
	rig.w.harness = func() map[string]int {
		out := map[string]int{}
		for k, v := range rig.harness {
			out[k] = v
		}
		return out
	}
	rig.w.fetchRoster = func(context.Context, string, time.Duration) (swapclient.Roster, error) {
		return swapclient.NewRoster([]llamaswap.Model{
			{ID: "qwen3.8-27b-vllm-3card", Aliases: []string{"agent-pool", "agent-pool-3card"}},
			{ID: "qwen3.5-9b-agent"},
		}), nil
	}
	rig.w.now = func() time.Time { return rig.clock }
	return rig
}

// poll advances the clock one interval and polls, then waits for the frames.
func (r *seatRig) poll() {
	r.clock = r.clock.Add(seatPollInterval)
	r.w.Poll(context.Background())
	r.w.em.Wait()
}

func (r *seatRig) states() []string {
	var out []string
	for i := 0; i < r.pair.count(); i++ {
		out = append(out, fmt.Sprint(r.pair.info(i)["state"]))
	}
	return out
}

// TestSeatWatcherReportsDirectTraffic: a busy stretch nobody in the harness
// asked for is ONE card — running after two agreeing polls, completed after
// two idle polls, stamped at the last busy poll, on this box, engine vllm.
func TestSeatWatcherReportsDirectTraffic(t *testing.T) {
	rig := newSeatRig(t)
	rig.poll() // idle
	rig.swap.set(func(f *fakeSwap) { f.running = 1 })
	rig.poll()
	if n := rig.pair.count(); n != 0 {
		t.Fatalf("a card opened on ONE busy poll (%d frames); the debounce must hold it", n)
	}
	start := rig.clock.UnixMilli() // the first busy poll
	rig.poll()
	if got := rig.states(); len(got) != 1 || got[0] != "running" {
		t.Fatalf("frames after two busy polls = %v, want [running]", got)
	}
	info := rig.pair.info(0)
	if info["model"] != "qwen3.8-27b-vllm-3card" || info["engine"] != "vllm" || info["scheduledOn"] != "self-uuid" ||
		info["requesterId"] != SeatRequester {
		t.Fatalf("running card = %v", info)
	}
	if got := int64(info["startedAt"].(float64)); got != start {
		t.Fatalf("startedAt = %d, want the FIRST busy poll %d", got, start)
	}
	rig.swap.set(func(f *fakeSwap) { f.running, f.waiting = 0, 2 }) // queued work is still work
	rig.poll()
	lastBusy := rig.clock.UnixMilli()
	rig.swap.set(func(f *fakeSwap) { f.waiting = 0 })
	rig.poll()
	if n := rig.pair.count(); n != 1 {
		t.Fatalf("card closed after ONE idle poll (%d frames)", n)
	}
	rig.poll()
	if got := rig.states(); len(got) != 2 || got[1] != "completed" {
		t.Fatalf("frames = %v, want [running completed]", got)
	}
	done := rig.pair.info(1)
	if done["id"] != info["id"] {
		t.Fatalf("terminal frame id %v != running frame id %v: two cards", done["id"], info["id"])
	}
	if got := int64(done["completedAt"].(float64)); got != lastBusy {
		t.Fatalf("completedAt = %d, want the last busy poll %d", got, lastBusy)
	}
}

// TestSeatWatcherNeverDuplicatesHarnessWork is the reason the register
// exists: a seat busy ONLY with harness requests — named by the alias the
// harness used — raises no card, and direct traffic beside them is counted
// on its own.
func TestSeatWatcherNeverDuplicatesHarnessWork(t *testing.T) {
	rig := newSeatRig(t)
	rig.harness["agent-pool"] = 2 // the harness's own two requests, by alias
	rig.swap.set(func(f *fakeSwap) { f.running = 2 })
	for i := 0; i < 5; i++ {
		rig.poll()
	}
	if n := rig.pair.count(); n != 0 {
		t.Fatalf("harness-only load raised %d frame(s): %v", n, rig.states())
	}
	// A curl joins the two harness requests.
	rig.swap.set(func(f *fakeSwap) { f.running = 3 })
	rig.poll()
	rig.poll()
	if got := rig.states(); len(got) != 1 || got[0] != "running" {
		t.Fatalf("direct request beside harness work: frames = %v, want [running]", got)
	}
}

// TestSeatWatcherIgnoresOnePollBlip: a harness request that ends between the
// two reads looks direct for exactly one poll — never a card.
func TestSeatWatcherIgnoresOnePollBlip(t *testing.T) {
	rig := newSeatRig(t)
	for i := 0; i < 4; i++ {
		rig.swap.set(func(f *fakeSwap) { f.running = 1 })
		rig.poll()
		rig.swap.set(func(f *fakeSwap) { f.running = 0 })
		rig.poll()
	}
	if n := rig.pair.count(); n != 0 {
		t.Fatalf("alternating one-poll blips raised %d frame(s)", n)
	}
}

// TestSeatWatcherFailsCardWhenSeatExits: the seat leaving llama-swap's
// running list with a card open is a crash under its callers.
func TestSeatWatcherFailsCardWhenSeatExits(t *testing.T) {
	rig := newSeatRig(t)
	rig.swap.set(func(f *fakeSwap) { f.running = 1 })
	rig.poll()
	rig.poll()
	rig.swap.set(func(f *fakeSwap) { f.loaded = false })
	rig.poll()
	got := rig.states()
	if len(got) != 2 || got[1] != "failed" {
		t.Fatalf("frames = %v, want [running failed]", got)
	}
	if e, _ := rig.pair.info(1)["error"].(string); !strings.Contains(e, "exited") {
		t.Fatalf("failed card error = %q", e)
	}
}

// TestSeatWatcherUnreadableMetrics: unreadable stats change nothing for a
// while, then fail an open card rather than leave it running forever.
func TestSeatWatcherUnreadableMetrics(t *testing.T) {
	rig := newSeatRig(t)
	rig.swap.set(func(f *fakeSwap) { f.running = 1 })
	rig.poll()
	rig.poll()
	rig.swap.set(func(f *fakeSwap) { f.badStats = true })
	for i := 0; i < seatUnreadableLimit-1; i++ {
		rig.poll()
	}
	if n := rig.pair.count(); n != 1 {
		t.Fatalf("card closed early on unreadable metrics (%d frames)", n)
	}
	rig.poll()
	if got := rig.states(); len(got) != 2 || got[1] != "failed" {
		t.Fatalf("frames = %v, want [running failed]", got)
	}
}

// TestSeatWatcherShutdownClosesCards: fleet-serve stopping completes an open
// card instead of leaving it running in PAIR.
func TestSeatWatcherShutdownClosesCards(t *testing.T) {
	rig := newSeatRig(t)
	rig.w.interval = time.Hour // Run polls once, then waits on ctx
	rig.swap.set(func(f *fakeSwap) { f.running = 1 })
	rig.poll()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rig.w.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for rig.pair.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if got := rig.states(); len(got) != 2 || got[0] != "running" || got[1] != "completed" {
		t.Fatalf("frames = %v, want [running completed]", got)
	}
}

// TestSeatWatcherDisabledIsInert: the key off means no poll, no frame.
func TestSeatWatcherDisabledIsInert(t *testing.T) {
	rig := newSeatRig(t)
	rig.w.em.cfg.Enabled = false
	rig.swap.set(func(f *fakeSwap) { f.running = 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rig.w.Run(ctx)
	if n := rig.pair.count(); n != 0 {
		t.Fatalf("disabled watcher sent %d frame(s)", n)
	}
}

func TestParseVLLMLoad(t *testing.T) {
	body := "# HELP vllm:num_requests_running Number of requests\n" +
		"vllm:num_requests_running{engine=\"0\",model_name=\"a\"} 1.0\n" +
		"vllm:num_requests_running{engine=\"1\",model_name=\"a\"} 2.0\n" +
		"vllm:num_requests_waiting{engine=\"0\",model_name=\"a\"} 3.0\n" +
		"vllm:num_requests_waiting_by_reason{engine=\"0\",model_name=\"a\",reason=\"capacity\"} 3.0\n" +
		"vllm:num_requests_running_total 99\n"
	if n, ok := parseVLLMLoad([]byte(body)); !ok || n != 6 {
		t.Fatalf("parseVLLMLoad = %d, %v; want 6, true", n, ok)
	}
	if _, ok := parseVLLMLoad([]byte("llamacpp:requests_processing 1\n")); ok {
		t.Fatal("a body without the vLLM gauges parsed as a reading")
	}
}

// TestSeatWatcherReportsNothingWhenHarnessLoadIsUnresolvable is the review
// finding this guards (2026-09-22): a marker names the ALIAS the request used
// (`agent-pool`), /running names the canonical seat, and the translation needs
// the llama-swap roster. A roster read that fails — most likely exactly when
// the seat is hammered — used to leave the alias unresolved, subtract zero and
// publish the harness's own request as a direct card.
func TestSeatWatcherReportsNothingWhenHarnessLoadIsUnresolvable(t *testing.T) {
	rig := newSeatRig(t)
	rig.w.fetchRoster = func(context.Context, string, time.Duration) (swapclient.Roster, error) {
		return swapclient.Roster{}, fmt.Errorf("llama-swap busy")
	}
	rig.harness["agent-pool"] = 1
	rig.swap.set(func(f *fakeSwap) { f.running = 1 })
	for i := 0; i < 5; i++ {
		rig.poll()
	}
	if n := rig.pair.count(); n != 0 {
		t.Fatalf("an unresolvable harness marker raised %d frame(s): %v", n, rig.states())
	}
	// A marker the roster does not know is equally unattributable.
	rig.w.fetchRoster = func(context.Context, string, time.Duration) (swapclient.Roster, error) {
		return swapclient.NewRoster([]llamaswap.Model{{ID: "some-other-seat"}}), nil
	}
	rig.harness = map[string]int{"a-seat-nobody-lists": 1}
	for i := 0; i < 3; i++ {
		rig.poll()
	}
	if n := rig.pair.count(); n != 0 {
		t.Fatalf("an unknown marker name raised %d frame(s)", n)
	}
}

// TestSeatWatcherKeepsLastRosterAcrossAFailedRefresh: aliases do not change
// while llama-swap is busy, so one failed refresh must not blind the
// subtraction (and must not stop direct traffic being reported).
func TestSeatWatcherKeepsLastRosterAcrossAFailedRefresh(t *testing.T) {
	rig := newSeatRig(t)
	good := rig.w.fetchRoster
	rig.harness["agent-pool"] = 1
	rig.swap.set(func(f *fakeSwap) { f.running = 1 })
	rig.poll() // warms the roster; harness-only load
	rig.w.fetchRoster = func(context.Context, string, time.Duration) (swapclient.Roster, error) {
		return swapclient.Roster{}, fmt.Errorf("timeout")
	}
	rig.clock = rig.clock.Add(2 * seatRosterTTL) // force a refresh, which fails
	rig.poll()
	rig.poll()
	if n := rig.pair.count(); n != 0 {
		t.Fatalf("a failed roster REFRESH lost the cached aliases: %d frame(s)", n)
	}
	// Direct traffic beside the harness request is still reported.
	rig.swap.set(func(f *fakeSwap) { f.running = 2 })
	rig.poll()
	rig.poll()
	if got := rig.states(); len(got) != 1 || got[0] != "running" {
		t.Fatalf("frames = %v, want [running]", got)
	}
	rig.w.fetchRoster = good
}

// TestSeatWatcherHoldsAnOpenCardWhileHarnessLoadIsUnknown: an unresolvable
// poll must not close or fail a card either — it says nothing, so nothing
// about the card changes.
func TestSeatWatcherHoldsAnOpenCardWhileHarnessLoadIsUnknown(t *testing.T) {
	rig := newSeatRig(t)
	rig.swap.set(func(f *fakeSwap) { f.running = 1 })
	rig.poll()
	rig.poll()
	if got := rig.states(); len(got) != 1 || got[0] != "running" {
		t.Fatalf("frames = %v, want [running]", got)
	}
	rig.w.fetchRoster = func(context.Context, string, time.Duration) (swapclient.Roster, error) {
		return swapclient.Roster{}, fmt.Errorf("gone")
	}
	rig.harness["agent-pool"] = 1
	rig.swap.set(func(f *fakeSwap) { f.running = 0 })
	for i := 0; i < 4; i++ {
		rig.poll()
	}
	if n := rig.pair.count(); n != 1 {
		t.Fatalf("the open card changed state on unresolvable polls: %v", rig.states())
	}
}

// TestSeatWatcherNeverRequestsUpstream: the watcher reads the seat's /metrics
// at the seat's own address (the `proxy` /running reports), never through
// llama-swap's /upstream/<model>/… — llama-swap counts every /upstream request
// as activity, so a 2-second poll there kept the seat loaded forever and its
// 5-minute idle unload never fired. Asked of llama-swap: /running only.
func TestSeatWatcherNeverRequestsUpstream(t *testing.T) {
	rig := newSeatRig(t)
	rig.poll()
	rig.swap.set(func(f *fakeSwap) { f.running = 1 })
	rig.poll()
	rig.poll()
	rig.swap.set(func(f *fakeSwap) { f.running = 0 })
	rig.poll()
	rig.poll()
	if got := rig.states(); len(got) != 2 || got[0] != "running" || got[1] != "completed" {
		t.Fatalf("frames = %v, want [running completed] (the metrics must still be read)", got)
	}
	rig.swap.mu.Lock()
	defer rig.swap.mu.Unlock()
	for _, p := range rig.swap.swapHits {
		if strings.HasPrefix(p, "/upstream") || p != "/running" {
			t.Fatalf("the watcher asked llama-swap for %q; only /running is allowed (an /upstream read resets the seat's idle timer)", p)
		}
	}
	if len(rig.swap.seatHits) != 5 {
		t.Fatalf("seat reads = %v, want one /metrics per poll (5)", rig.swap.seatHits)
	}
	for _, p := range rig.swap.seatHits {
		if p != "/metrics" {
			t.Fatalf("the watcher asked the seat for %q", p)
		}
	}
}

// TestSeatWatcherWithoutAProxyReadsNothing: a /running entry with no proxy
// leaves no address — the read is "unreadable" (no card), never a fall-back
// through /upstream.
func TestSeatWatcherWithoutAProxyReadsNothing(t *testing.T) {
	rig := newSeatRig(t)
	rig.swap.set(func(f *fakeSwap) { f.seatURL, f.running = "", 1 })
	rig.poll()
	rig.poll()
	rig.poll()
	if n := rig.pair.count(); n != 0 {
		t.Fatalf("%d frame(s) with no seat address; want none", n)
	}
	rig.swap.mu.Lock()
	defer rig.swap.mu.Unlock()
	for _, p := range rig.swap.swapHits {
		if p != "/running" {
			t.Fatalf("the watcher asked llama-swap for %q", p)
		}
	}
}
