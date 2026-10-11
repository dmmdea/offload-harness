package pipeline

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// coldVisionSwap is a llama-swap whose vision seat starts cold. It does what the real
// router does on the two routes that matter: ANY request for the model (the passthrough
// the warm-up sends, or the chat completion) starts the load, and the request is HELD
// open until the seat is up. A client that hangs up in the meantime does not stop the
// load (the register's 499 at 2m0.016s was followed by "Health check passed"). /running
// lists the seat as absent, then "starting", then "ready".
type coldVisionSwap struct {
	model   string
	loadFor time.Duration

	mu        sync.Mutex
	startedAt time.Time
	ready     bool

	chatPosts       atomic.Int32 // /v1/chat/completions requests that arrived
	chatPostsCold   atomic.Int32 // ...of which arrived while the seat was not yet ready
	passthroughGets atomic.Int32 // /upstream/<model>/... requests (the warm-up)
	runningReads    atomic.Int32
}

func newColdVisionSwap(model string, loadFor time.Duration, startReady bool) *coldVisionSwap {
	return &coldVisionSwap{model: model, loadFor: loadFor, ready: startReady}
}

// trigger starts the load once, as a request for the model does.
func (f *coldVisionSwap) trigger() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ready || !f.startedAt.IsZero() {
		return
	}
	f.startedAt = time.Now()
	time.AfterFunc(f.loadFor, func() {
		f.mu.Lock()
		f.ready = true
		f.mu.Unlock()
	})
}

func (f *coldVisionSwap) isReady() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ready
}

// hold blocks until the seat is ready or the request is abandoned.
func (f *coldVisionSwap) hold(r *http.Request) bool {
	for !f.isReady() {
		select {
		case <-r.Context().Done():
			return false
		case <-time.After(10 * time.Millisecond):
		}
	}
	return true
}

func (f *coldVisionSwap) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/running":
			f.runningReads.Add(1)
			f.mu.Lock()
			state := ""
			switch {
			case f.ready:
				state = "ready"
			case !f.startedAt.IsZero():
				state = "starting"
			}
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if state == "" {
				_, _ = w.Write([]byte(`{"running":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"running":[{"model":"` + f.model + `","state":"` + state + `","cmd":"x"}]}`))
		case r.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []map[string]any{{"id": f.model}}})
		case strings.HasPrefix(r.URL.Path, "/upstream/"+f.model+"/"):
			f.passthroughGets.Add(1)
			f.trigger()
			if !f.hold(r) {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"` + f.model + `"}]}`))
		case r.URL.Path == "/v1/chat/completions":
			_, _ = io.Copy(io.Discard, r.Body) // read the body so a hang-up cancels r.Context(), as llama-swap sees it
			f.chatPosts.Add(1)
			if !f.isReady() {
				f.chatPostsCold.Add(1)
			}
			f.trigger()
			if !f.hold(r) {
				return // the client gave up; llama-swap logs a 499
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fakeChat{content: "A red square.", finishReason: "stop", promptTokens: 50}.marshal())
		default:
			http.NotFound(w, r)
		}
	}))
}

// coldVisionPipeline wires a pipeline to the fake with a SHORT request timeout (the
// stand-in for request_timeout_sec's 120 s) so a load longer than it reproduces the
// register's cancel without a two-minute test.
func coldVisionPipeline(t *testing.T, srv *httptest.Server, requestTimeout time.Duration, coldLoadWaitSec int) *Pipeline {
	t.Helper()
	cfg := baseVisionCfg(srv, "fake-vlm")
	cfg.VisionColdLoadWaitSec = coldLoadWaitSec
	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", requestTimeout)
	return New(cfg, client, nil, nil)
}

func coldVisionVQA() core.Request {
	return core.Request{
		Task:   core.TaskVQA,
		Image:  minimalPNGDataURI(),
		Params: map[string]any{"question": "What is shown?"},
	}
}

// F49 (C24): the first vision call after the seat's idle unload hits a COLD seat. llama-swap
// holds the request open for the whole load, and the request's own timeout then has to cover
// the load plus the inference, so a load longer than the timeout cancelled the call
// ("Client.Timeout exceeded while awaiting headers") while the seat was still coming up.
// The fix waits for the load in a separate bounded request BEFORE sending, so the vision
// request itself rides its timeout for inference alone: here the load (1.2 s) is three times
// the request timeout (0.4 s) and the call must still succeed, with the request arriving only
// after the seat is ready.
func TestVisionColdSeatIsWaitedForBeforeTheRequestIsSent(t *testing.T) {
	swap := newColdVisionSwap("fake-vlm", 1200*time.Millisecond, false)
	srv := swap.server(t)
	defer srv.Close()
	p := coldVisionPipeline(t, srv, 400*time.Millisecond, 10)

	res := p.Run(context.Background(), coldVisionVQA())
	if !res.OK {
		t.Fatalf("a cold vision seat must be waited for, not cancelled: deferred with %q (err_class %q)", res.Reason, res.Meta.ErrClass)
	}
	if got := swap.passthroughGets.Load(); got != 1 {
		t.Errorf("warm-up passthrough requests = %d, want 1 (the one that triggers and holds for the load)", got)
	}
	if got := swap.chatPosts.Load(); got != 1 {
		t.Errorf("chat completions = %d, want exactly 1 (no retry: the wait, not a second attempt, absorbs the load)", got)
	}
	if got := swap.chatPostsCold.Load(); got != 0 {
		t.Errorf("%d chat completion(s) arrived while the seat was still loading; the request must be sent only once the seat is ready", got)
	}
}

// A seat that is already resident costs one /running read and nothing else: no passthrough
// request, no wait.
func TestVisionResidentSeatIsNotWarmed(t *testing.T) {
	swap := newColdVisionSwap("fake-vlm", time.Second, true)
	srv := swap.server(t)
	defer srv.Close()
	p := coldVisionPipeline(t, srv, 5*time.Second, 10)

	start := time.Now()
	res := p.Run(context.Background(), coldVisionVQA())
	if !res.OK {
		t.Fatalf("resident seat deferred: %s", res.Reason)
	}
	if got := swap.passthroughGets.Load(); got != 0 {
		t.Errorf("a resident seat was warmed: %d passthrough request(s)", got)
	}
	if got := swap.runningReads.Load(); got < 1 {
		t.Errorf("/running reads = %d, want the residency read that told the seat was ready", got)
	}
	if d := time.Since(start); d > 800*time.Millisecond {
		t.Errorf("a resident seat took %s; nothing should have waited", d)
	}
}

// vision_coldload_wait_sec <= 0 turns the wait off: the request carries the load as it did
// before, so a load longer than the request timeout still cancels the call. This pins that
// the knob is what produces the fix (and is the oracle the mutation check flips).
func TestVisionColdLoadWaitOffLeavesTheLoadToTheRequest(t *testing.T) {
	swap := newColdVisionSwap("fake-vlm", 1200*time.Millisecond, false)
	srv := swap.server(t)
	defer srv.Close()
	p := coldVisionPipeline(t, srv, 400*time.Millisecond, 0)

	res := p.Run(context.Background(), coldVisionVQA())
	if res.OK {
		t.Fatal("with the wait off, a load longer than the request timeout must still defer (the pre-fix behaviour)")
	}
	if res.Meta.ErrClass != "timeout" || !strings.Contains(res.Reason, "vision model call failed") {
		t.Fatalf("defer = %q (err_class %q), want the request-timeout defer", res.Reason, res.Meta.ErrClass)
	}
	if got := swap.passthroughGets.Load(); got != 0 {
		t.Errorf("wait off, but %d passthrough request(s) were sent", got)
	}
}

// The wait is BOUNDED: a load that outlasts it does not hold the caller beyond it. The call
// goes on (the load carries on behind the request) and, with the request timeout shorter than
// what is left of the load, defers with the timeout, naming the wait it already spent.
func TestVisionColdLoadWaitIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("spends the wait's 3 s floor")
	}
	swap := newColdVisionSwap("fake-vlm", 8*time.Second, false)
	srv := swap.server(t)
	defer srv.Close()
	p := coldVisionPipeline(t, srv, 400*time.Millisecond, 3)

	start := time.Now()
	res := p.Run(context.Background(), coldVisionVQA())
	took := time.Since(start)
	if res.OK {
		t.Fatal("the load (8 s) outlasts the wait (3 s) and the request timeout (0.4 s); the call cannot succeed")
	}
	if took > 5*time.Second {
		t.Errorf("the call held its caller %s; the wait is bounded at 3 s plus one request timeout", took)
	}
	if took < 2900*time.Millisecond {
		t.Errorf("the call returned after %s, before the wait's 3 s bound: it did not wait", took)
	}
	if !strings.Contains(res.Reason, "waited") || !strings.Contains(res.Reason, "load") {
		t.Errorf("reason = %q, want it to name the cold-load wait already spent", res.Reason)
	}
}

// A seat routed to another box (seat_endpoints) is not this box's llama-swap to warm: the
// local router must never be asked to load a model that its own calls do not use.
func TestVisionSeatRoutedElsewhereIsNotWarmedLocally(t *testing.T) {
	local := newColdVisionSwap("fake-vlm", 500*time.Millisecond, false)
	localSrv := local.server(t)
	defer localSrv.Close()
	remote := newColdVisionSwap("fake-vlm", 0, true)
	remoteSrv := remote.server(t)
	defer remoteSrv.Close()

	cfg := baseVisionCfg(localSrv, "fake-vlm")
	cfg.VisionColdLoadWaitSec = 10
	cfg.SeatEndpoints = map[string]string{"fake-vlm": remoteSrv.URL}
	client := llamaclient.New(localSrv.URL, cfg.CompletionPath, "", 5*time.Second).WithSeatEndpoints(cfg.SeatEndpoints)
	p := New(cfg, client, nil, nil)

	res := p.Run(context.Background(), coldVisionVQA())
	if !res.OK {
		t.Fatalf("deferred: %s", res.Reason)
	}
	if got := local.passthroughGets.Load() + local.chatPosts.Load(); got != 0 {
		t.Errorf("the local router saw %d request(s) for a seat that is routed elsewhere", got)
	}
	if got := remote.chatPosts.Load(); got != 1 {
		t.Errorf("remote chat completions = %d, want 1", got)
	}
}

// The default is on: a stock config carries the wait, so the fix is live without an operator
// setting it.
func TestVisionColdLoadWaitDefaultsOn(t *testing.T) {
	if got := config.Default().VisionColdLoadWaitSec; got != 180 {
		t.Fatalf("Default().VisionColdLoadWaitSec = %d, want 180", got)
	}
}

// A backend that is not a llama-swap (a dedicated llama-server: no /running) cannot say whether
// its seat is loaded. The wait declines at once, the vision request is sent exactly as before and
// no passthrough request is made: the pre-send wait never adds a failure to a box that has no
// router to warm.
func TestVisionColdLoadWaitDeclinesWhenRunningIsUnreadable(t *testing.T) {
	var chat, other atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			other.Add(1)
			http.NotFound(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		chat.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fakeChat{content: "A red square.", finishReason: "stop", promptTokens: 50}.marshal())
	}))
	defer srv.Close()
	p := coldVisionPipeline(t, srv, 5*time.Second, 10)

	res := p.Run(context.Background(), coldVisionVQA())
	if !res.OK {
		t.Fatalf("deferred: %s", res.Reason)
	}
	if chat.Load() != 1 {
		t.Errorf("chat completions = %d, want 1", chat.Load())
	}
	if got := other.Load(); got != 1 {
		t.Errorf("non-chat requests = %d, want exactly the one /running read that could not be answered", got)
	}
}
