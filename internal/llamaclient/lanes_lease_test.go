package llamaclient

import (
	"context"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// leasedClient is busyClient with the LEASE gate firing: the machine-wide GPU lease
// is held by someone else, which is exactly when a cascade call should leave the box.
func leasedClient(t *testing.T, laneBase, token string) *Client {
	t.Helper()
	s := newLocalSwap(t)
	resident, route := FleetLaneGates(token)
	return New("http://127.0.0.1:11436", "", "gemma-4-e4b", 5*time.Second).
		WithRemoteLanes(map[string]string{"gemma-4-e4b": laneBase},
			func() bool { return true }, // the local GPU lease is held: the lane must fire
			LocalSwapBusy(s.srv.URL),
			resident).WithLaneRoute(route)
}

// TestFleetLaneSkipsTheLocalLeaseWait is the register C-41c proof: with a MEDIA
// lease held on this box and a node lane that serves the model, Generate must reach
// the node's chat door at once. Before the fix the lane resolved (and logged), and
// sendWithSeatWait then admitted the send through modelaffinity.Admit, whose card
// wait reads THIS box's lease whatever the endpoint — the call waited out the whole
// bound and deferred, and the node never saw a request (measured 2026-09-15, 0.125.0).
func TestFleetLaneSkipsTheLocalLeaseWait(t *testing.T) {
	root := t.TempDir()
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatalf("open lease manager: %v", err)
	}
	if err := modelaffinity.SetGPULease("", root); err != nil {
		t.Fatalf("arm gate: %v", err)
	}
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "hero render", Origin: "test"})
	if err != nil {
		t.Fatalf("acquire media: %v", err)
	}
	defer func() { _ = l.Release() }()

	node := newFleetNode(t, "node-b", true, []string{"gemma-4-e4b"})
	c := leasedClient(t, node.srv.URL, "s3cret")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	res, gerr := c.Generate(ctx, "gemma-4-e4b", "sys", "user", "", 32, 0, 0)
	if gerr != nil {
		t.Fatalf("Generate through the node lane under a local media lease: %v — the lane fired, then the send waited on THIS box's card (C-41c)", gerr)
	}
	if res.Content == "" {
		t.Fatal("no content came back from the node lane")
	}
	if node.chatHits.Load() != 1 {
		t.Fatalf("chat hits = %d, want exactly 1 on the node's door", node.chatHits.Load())
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("the lane call took %s — it waited on the local lease before dialling", el)
	}
}
