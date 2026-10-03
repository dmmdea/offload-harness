package placement

import (
	"context"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// The local box's Live carries the held-card reader (plan P4): the table's fallback row is
// wired to the same armed lease directory and card table the load gate reads.
func TestSnapshotLiveCarriesTheHeldCardReader(t *testing.T) {
	root := t.TempDir()
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	if err := modelaffinity.SetGPULease("", root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(modelaffinity.DisarmGPULease)
	restore := modelaffinity.SetCardTableReader(func(context.Context, string) ([]gpuprobe.Card, string, error) {
		return []gpuprobe.Card{
			{UUID: "GPU-aaaa0000", NvidiaIndex: 0, ComfyOrder: -1},
			{UUID: "GPU-bbbb0000", NvidiaIndex: 1, Display: true, ComfyOrder: -1},
			{UUID: "GPU-cccc0000", NvidiaIndex: 2, ComfyOrder: -1},
		}, "", nil
	})
	t.Cleanup(restore)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render", Devices: []string{"gpu-cccc0000"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })

	live := NewSnapshot(config.FlagshipFixture(), 0).Live()
	if live.CardsHeld == nil {
		t.Fatal("the local snapshot's Live must carry the held-card reader")
	}
	if held, why := live.CardsHeld([]string{"2"}); !held || why == "" {
		t.Fatalf("card 2 is held by the render: %v %q", held, why)
	}
	if held, _ := live.CardsHeld([]string{"0"}); held {
		t.Fatal("card 0 is free")
	}
	// And the table, end to end through that reader, moves the agent lane off the held card.
	d := Decide(agentReq(1000), flagshipLayers(), live)
	if d.Layer != "single" || d.Defer {
		t.Fatalf("a card-2 render must move the flagship agent lane to the single layer, got %+v", d.Placed)
	}
}

// The remote-row Live (a delegator reading a node's health) has no reader and so no fallback.
func TestLiveFromReadingsHasNoHeldCardReader(t *testing.T) {
	if LiveFromReadings(nil, nil, nil).CardsHeld != nil {
		t.Fatal("a Live built from a health row must not guess at another box's leases")
	}
}
