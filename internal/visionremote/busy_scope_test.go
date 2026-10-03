package visionremote

import (
	"context"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// The auto route's trigger is read for the cards the vision seat sits on (plan
// P4): a render on another card must not send an image off the box.
func TestDefaultLocalBusyIsPerSeat(t *testing.T) {
	cfg := config.CompositeFixture() // the single layer's ocr seat (qwen3-vl-8b) is on device 2
	cfg.VisionModel = "qwen3-vl-8b"
	root := t.TempDir()
	cfg.StateDir, cfg.GPULockPath = root, root+"/lease"
	m, err := gpulease.OpenAt(cfg.GPULockPath, "")
	if err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	t.Cleanup(modelaffinity.SetCardTableReader(func(context.Context, string) ([]gpuprobe.Card, string, error) {
		return []gpuprobe.Card{
			{UUID: "GPU-aaaa0000", NvidiaIndex: 0, ComfyOrder: -1},
			{UUID: "GPU-bbbb0000", NvidiaIndex: 1, Display: true, ComfyOrder: -1},
			{UUID: "GPU-cccc0000", NvidiaIndex: 2, ComfyOrder: -1},
		}, "", nil
	}))
	if localBusy(cfg) {
		t.Fatal("nothing is held")
	}
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render", Devices: []string{"gpu-aaaa0000"}})
	if err != nil {
		t.Fatal(err)
	}
	if localBusy(cfg) {
		t.Fatal("a render on card 0 must not read as busy for a vision seat pinned to card 2")
	}
	_ = l.Release()
	l, err = m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render", Devices: []string{"gpu-cccc0000"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	if !localBusy(cfg) {
		t.Fatal("a render on card 2 is on the vision seat's card: busy")
	}
}
