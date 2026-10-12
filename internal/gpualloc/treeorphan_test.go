package gpualloc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// A card held by a tree-orphan (its wrapper gone, the job it started running) is a claimed card:
// a reservation that asked for N cards is handed a different one, never the card a job is
// running on (plan P10). The lease is a real record in a scratch directory whose holder is a pid
// no process has and whose recorded tree is this test process, which is alive.
func TestAllocatorHandsAWaiterAnotherCardWhileACardIsTreeOrphaned(t *testing.T) {
	m := scratchManager(t)
	cards := threeCards()
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "orphaned", Devices: []string{cards[0].LeaseID()}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	start, _ := gpulease.ProcessStart(self)
	path := filepath.Join(m.Dir(), "e", strconv.FormatUint(l.Epoch(), 10)+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	rec["holder"] = map[string]any{"pid": 2_000_000_000, "start_time_ms": 0} // the wrapper is gone
	rec["tree"] = []map[string]any{{"pid": self, "start_ms": start}}         // its job lives
	b, _ = json.Marshal(rec)
	if err := os.WriteFile(path, b, 0o666); err != nil {
		t.Fatal(err)
	}
	if info := m.Inspect(); !info.Held || !info.TreeOrphan {
		t.Fatalf("the fixture must be a tree-orphan: %+v", info)
	}

	in, err := BuildInput(context.Background(), m, config.Config{}, Need{}, hostDeps(cards)) // no footprint is declared, so the display card stays closed under the cap
	if err != nil {
		t.Fatal(err)
	}
	if !in.Claimed[cards[0].LeaseID()] {
		t.Fatalf("a tree-orphan's card is claimed: %v", in.Claimed)
	}
	in.Min, in.Max = 1, 1
	got, err := gpulease.Allocate(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Devices) != 1 || got.Devices[0] == cards[0].LeaseID() {
		t.Fatalf("the waiter is handed a different card, got %v", got.Devices)
	}
	skipped := ""
	for _, s := range got.Skipped {
		if s.ID == cards[0].LeaseID() {
			skipped = s.Reason
		}
	}
	if skipped != gpulease.ReasonClaimed {
		t.Fatalf("the orphan's card is skipped as claimed, got %q (%+v)", skipped, got.Skipped)
	}
}
