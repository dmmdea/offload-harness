package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// makeTreeOrphan turns a card lease taken in a scratch directory into a tree-orphan: its holder
// becomes a pid no process has (the wrapper is gone) and its recorded tree this test process,
// which is alive (the job it started still runs).
func makeTreeOrphan(t *testing.T, m *gpulease.Manager, l *gpulease.Lease) {
	t.Helper()
	path := filepath.Join(m.Dir(), "e", strconv.FormatUint(l.Epoch(), 10)+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	start, _ := gpulease.ProcessStart(self)
	rec["holder"] = map[string]any{"pid": 2_000_000_000, "start_time_ms": 0}
	rec["tree"] = []map[string]any{{"pid": self, "start_ms": start}}
	b, _ = json.Marshal(rec)
	if err := os.WriteFile(path, b, 0o666); err != nil {
		t.Fatal(err)
	}
}

// A tree-orphan is a live lease on its cards: the first lease to finish does not load the seat's
// warm-back over cards a job is still running on, whose wrapper happens to be gone (rollout
// finding 2 meets P10). The warm stays owed for the last lease on them.
func TestAWarmBackWaitsForATreeOrphanOnTheSeatsCards(t *testing.T) {
	cfgPath, m, warms := warmCardFixture(t)
	a := acquireCard(t, m, "film card 0", "gpu-aaaa0000-x")
	b := acquireCard(t, m, "film card 2", "gpu-cccc0000-x")
	makeTreeOrphan(t, m, b)
	if info := m.Inspect(); len(info.Each()) != 2 {
		t.Fatalf("both leases are live: %+v", info.Each())
	}
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	warmBackGuarded(loadCfgPath(cfgPath), leaseWarmGuard(m, a), &out)
	_ = a.Release()
	if n := warms.Load(); n != 0 {
		t.Fatalf("the seat was loaded over a card a tree-orphan's job still uses (%d warm(s)): %s", n, out.String())
	}
	if m.SeatWarmOwed() != "seat" {
		t.Fatalf("a skipped warm stays owed, owed=%q", m.SeatWarmOwed())
	}
	if !strings.Contains(out.String(), "NOT warming seat back yet") || !strings.Contains(out.String(), "epoch "+strconv.FormatUint(b.Epoch(), 10)) {
		t.Errorf("the skip names the tree-orphan still on the seat's cards: %s", out.String())
	}
}
