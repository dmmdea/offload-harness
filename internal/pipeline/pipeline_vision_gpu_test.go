package pipeline

// LO-1 tests: the vision path's GPU-lock gate + http_5xx retry + breaker
// accounting. Evidence: 295 of the 337 all-time defers were http_5xx landing
// in ONE hour while generate_image jobs held the single-slot GPU lock —
// llama-swap could not (re)load the VLM, so every vision call burned a doomed
// HTTP call and deferred to the expensive cloud model. Temp lock dirs + fake
// servers only; no real GPU work.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/breaker"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// holdGPULock creates a held lock exactly the way render/gpu-lock.mjs
// acquireGpuLock does: mkdir + meta.json{pid,startedAt}, with OUR (live) pid so
// the lock is genuinely held, not stale.
func holdGPULock(t *testing.T) string {
	t.Helper()
	lock := filepath.Join(t.TempDir(), "gpu.lock")
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"pid":%d,"startedAt":%d}`, os.Getpid(), time.Now().UnixMilli())
	if err := os.WriteFile(filepath.Join(lock, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	return lock
}

// vqaReq is the canonical vision request used across these tests.
func vqaReq() core.Request {
	return core.Request{
		Task:   core.TaskVQA,
		Image:  minimalPNGDataURI(),
		Params: map[string]any{"question": "What number is shown?"},
	}
}

// TestVisionGPULockHeldDefersWithDistinctReason: with a generation job holding
// the lock, a vision call must wait the FULL bounded window without ever
// calling the model, then defer with the distinct "gpu busy" reason + the
// gpu_busy err class (not a burned http_5xx).
func TestVisionGPULockHeldDefersWithDistinctReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the model must NOT be called while the gen lock is held")
	}))
	defer srv.Close()

	cfg := baseVisionCfg(srv, "fake-vlm")
	cfg.GPULockPath = holdGPULock(t)
	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	p := New(cfg, client, nil, nil)
	p.visionGPUWait = 150 * time.Millisecond // shrink the 90s default for the test
	p.visionGPUPoll = 20 * time.Millisecond

	begin := time.Now()
	res := p.Run(context.Background(), vqaReq())
	if el := time.Since(begin); el < 150*time.Millisecond {
		t.Errorf("returned after %v — must poll the full bounded wait before deferring", el)
	}
	if res.OK || !res.Deferred {
		t.Fatalf("expected defer while the gen lock is held, got OK=%v", res.OK)
	}
	if !strings.HasPrefix(res.Reason, "gpu busy: generation job holds the lock (") || !strings.HasSuffix(res.Reason, "s)") {
		t.Errorf("reason = %q, want the distinct gpu-busy reason with the holder age", res.Reason)
	}
	if res.Meta.ErrClass != "gpu_busy" {
		t.Errorf("err class = %q, want gpu_busy", res.Meta.ErrClass)
	}
	// No model call happened, so the vision breaker must be untouched (closed).
	if st := p.breakers.State("fake-vlm"); st != "closed" {
		t.Errorf("breaker state = %q, want closed (no call was made)", st)
	}
}

// TestVisionGPULockReleasedMidWaitProceeds: the gen job finishing (lock
// released) inside the wait window lets the vision call proceed and succeed.
func TestVisionGPULockReleasedMidWaitProceeds(t *testing.T) {
	srv := visionServer(t, fakeChat{content: "released and answered", finishReason: "stop", promptTokens: 50})
	defer srv.Close()

	lock := holdGPULock(t)
	cfg := baseVisionCfg(srv, "fake-vlm")
	cfg.GPULockPath = lock
	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	p := New(cfg, client, nil, nil)
	p.visionGPUWait = 5 * time.Second
	p.visionGPUPoll = 10 * time.Millisecond

	go func() {
		time.Sleep(80 * time.Millisecond)
		_ = os.RemoveAll(lock) // the gen job's release()
	}()
	begin := time.Now()
	res := p.Run(context.Background(), vqaReq())
	if !res.OK {
		t.Fatalf("expected OK after the lock released mid-wait, got defer: %s", res.Reason)
	}
	if el := time.Since(begin); el >= 5*time.Second {
		t.Errorf("took the full wait window (%v) despite the release", el)
	}
}

// vision5xxServer replies 503 for the first n calls, then a valid 200 chat
// response; it counts calls.
func vision5xxServer(t *testing.T, fail5xx int, calls *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(calls, 1)
		if int(n) <= fail5xx {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"failed to load model"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fakeChat{content: "second try worked", finishReason: "stop", promptTokens: 50}.marshal())
	}))
}

// TestVision5xxRetriesOnceThenSucceeds: one transient 5xx (llama-swap mid-load)
// must NOT defer — the single backoff retry lands and the call succeeds.
func TestVision5xxRetriesOnceThenSucceeds(t *testing.T) {
	var calls int32
	srv := vision5xxServer(t, 1, &calls)
	defer srv.Close()

	cfg := baseVisionCfg(srv, "fake-vlm")
	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	p := New(cfg, client, nil, nil)
	p.visionRetryWait = 10 * time.Millisecond         // shrink the 3s backoff for the test
	p.breakers = breaker.NewGroup(1, 10, time.Minute) // hair-trigger: 1 failure trips

	res := p.Run(context.Background(), vqaReq())
	if !res.OK {
		t.Fatalf("expected OK after one 5xx + retry, got defer: %s", res.Reason)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("model calls = %d, want exactly 2 (original + one retry)", got)
	}
	// The FINAL outcome (success) is what the breaker records.
	if st := p.breakers.State("fake-vlm"); st != "closed" {
		t.Errorf("breaker state = %q, want closed after a successful retry", st)
	}
}

// TestVision5xx5xxDefersAndBreakerCounts: two 5xx in a row exhaust the single
// retry — the call defers with err class http_5xx and the FINAL failure counts
// against the vision tier's breaker (a 5xx is never cold-swap-exempt, LO-9).
func TestVision5xx5xxDefersAndBreakerCounts(t *testing.T) {
	var calls int32
	srv := vision5xxServer(t, 99, &calls) // every call 5xxs
	defer srv.Close()

	cfg := baseVisionCfg(srv, "fake-vlm")
	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	p := New(cfg, client, nil, nil)
	p.visionRetryWait = 10 * time.Millisecond
	p.breakers = breaker.NewGroup(1, 10, time.Minute) // one failure trips

	res := p.Run(context.Background(), vqaReq())
	if res.OK || !res.Deferred {
		t.Fatalf("expected defer after 5xx+5xx, got OK=%v", res.OK)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("model calls = %d, want exactly 2 (one retry, no more)", got)
	}
	if res.Meta.ErrClass != "http_5xx" {
		t.Errorf("err class = %q, want http_5xx", res.Meta.ErrClass)
	}
	if st := p.breakers.State("fake-vlm"); st != "open" {
		t.Errorf("breaker state = %q, want open — the final 5xx failure must count", st)
	}
}
