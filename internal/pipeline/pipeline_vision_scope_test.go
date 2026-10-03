package pipeline

// The vision pre-check reads the lease the way the seat's own load gate does (plan P4, review
// fix): a render on a card the vision seat is not pinned to is no reason to wait for it, or to
// defer. Before the fix the delegator's auto route narrowed to the seat's cards and ran the
// call locally, and this gate then waited the full vision_gpu_wait_sec on ANY live lease and
// deferred gpu_busy: a call that used to be served by the fleet was lost.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// armVisionSeat pins the "fake-vlm" seat to card 2 of a synthetic three-card box.
func armVisionSeat(t *testing.T) {
	t.Helper()
	modelaffinity.SetSeatPins(func(model string) ([]string, bool) {
		if model == "fake-vlm" {
			return []string{"2"}, true
		}
		return nil, false
	})
	t.Cleanup(func() { modelaffinity.SetSeatPins(nil) })
	t.Cleanup(modelaffinity.SetCardTableReader(func(context.Context, string) ([]gpuprobe.Card, string, error) {
		return []gpuprobe.Card{
			{UUID: "GPU-aaaa0000", NvidiaIndex: 0, ComfyOrder: -1},
			{UUID: "GPU-bbbb0000", NvidiaIndex: 1, Display: true, ComfyOrder: -1},
			{UUID: "GPU-cccc0000", NvidiaIndex: 2, ComfyOrder: -1},
		}, "", nil
	}))
}

func deviceLease(t *testing.T, lockDir string, devices ...string) {
	t.Helper()
	m, err := gpulease.OpenAt(lockDir, "")
	if err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render", Devices: devices})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
}

func TestVisionGateIgnoresALeaseOnAnotherCard(t *testing.T) {
	var calls atomic.Int32
	srv := visionServer(t, fakeChat{content: "served locally", finishReason: "stop", promptTokens: 50})
	defer srv.Close()
	counted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer counted.Close()

	armVisionSeat(t)
	lock := filepath.Join(t.TempDir(), "gpu", "lease")
	deviceLease(t, lock, "gpu-aaaa0000") // a render on card 0; the vision seat is on card 2

	cfg := baseVisionCfg(counted, "fake-vlm")
	cfg.GPULockPath = lock
	client := llamaclient.New(counted.URL, cfg.CompletionPath, "", 10*time.Second)
	p := New(cfg, client, nil, nil)
	p.visionGPUWait = 3 * time.Second
	p.visionGPUPoll = 10 * time.Millisecond

	begin := time.Now()
	res := p.Run(context.Background(), vqaReq())
	if !res.OK {
		t.Fatalf("a lease on card 0 must not defer a call whose seat is on card 2, got: %s", res.Reason)
	}
	if el := time.Since(begin); el >= time.Second {
		t.Fatalf("the gate waited %v for a lease that does not touch its card", el)
	}
	if calls.Load() == 0 {
		t.Fatal("the model was never called")
	}
}

func TestVisionGateStillWaitsForALeaseOnItsOwnCard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the model must NOT be called while a lease holds the seat's card")
	}))
	defer srv.Close()

	armVisionSeat(t)
	lock := filepath.Join(t.TempDir(), "gpu", "lease")
	deviceLease(t, lock, "gpu-cccc0000") // card 2 is the vision seat's

	cfg := baseVisionCfg(srv, "fake-vlm")
	cfg.GPULockPath = lock
	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	p := New(cfg, client, nil, nil)
	p.visionGPUWait = 120 * time.Millisecond
	p.visionGPUPoll = 20 * time.Millisecond

	res := p.Run(context.Background(), vqaReq())
	if res.OK || !res.Deferred || !strings.HasPrefix(res.Reason, "gpu busy: ") {
		t.Fatalf("a lease on the seat's own card still defers gpu busy, got OK=%v %q", res.OK, res.Reason)
	}
}
