package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpucards"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

func statusCards() []gpuprobe.Card {
	devs := []gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000-x", Name: "Test Card A", TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
		{Index: 1, UUID: "GPU-bbbb0000-x", Name: "Test Card B", TotalGiB: 16, FreeGiB: 12, UtilKnown: true, DisplayActive: true},
		{Index: 2, UUID: "GPU-cccc0000-x", Name: "Test Card A", TotalGiB: 16, FreeGiB: 16, UtilKnown: true},
	}
	cards, _ := gpuprobe.BuildCards(devs, "1,0,2")
	return cards
}

func rowByIndex(t *testing.T, rows []gpucards.Row, idx int) gpucards.Row {
	t.Helper()
	for _, r := range rows {
		if r.Index == idx {
			return r
		}
	}
	t.Fatalf("no row for card %d in %+v", idx, rows)
	return gpucards.Row{}
}

// A card-scoped lease holds its cards and ONLY its cards: every other card reads free.
func TestStatusJSONHasPerCardRows(t *testing.T) {
	_, m := scopedLeaseFixture(t)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{
		Reason: "batch", Devices: []string{"gpu-cccc0000-x"}, TTL: time.Hour, Group: "batch-7",
		Command: "python run.py --cuda-device 2", WrapperVersion: "9.9.9",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()

	sec := statusLeaseSection(m, statusCards(), "", m.Waiters())
	b, err := json.Marshal(sec)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Cards  []gpucards.Row `json:"cards"`
		Leases []struct {
			Epoch   uint64   `json:"epoch"`
			Devices []string `json:"devices"`
			Scope   string   `json:"scope"`
			Group   string   `json:"group"`
		} `json:"leases"`
	}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Cards) != 3 {
		t.Fatalf("one row per card, got %d: %s", len(back.Cards), b)
	}
	held := rowByIndex(t, back.Cards, 2)
	if held.State != "held" || held.Holder == nil || held.Holder.Epoch != l.Epoch() || held.Holder.Scope != "card" ||
		held.Holder.Group != "batch-7" || held.Holder.Class != "media" || !strings.Contains(held.Holder.Command, "run.py") {
		t.Fatalf("card 2 must show its holder: %+v", held)
	}
	for _, idx := range []int{0, 1} {
		if r := rowByIndex(t, back.Cards, idx); r.State != "free" || r.Holder != nil {
			t.Errorf("card %d is not held by a card-scoped lease on card 2: %+v", idx, r)
		}
	}
	if d := rowByIndex(t, back.Cards, 1); !d.Display {
		t.Errorf("the display flag must be carried: %+v", d)
	}
	if r := rowByIndex(t, back.Cards, 0); r.UUID != "GPU-aaaa0000-x" || r.Name != "Test Card A" || r.VRAMFreeGiB != 15 || r.VRAMTotalGiB != 16 || r.ComfyOrder != 1 {
		t.Errorf("row fields: %+v", r)
	}
	if len(back.Leases) != 1 || back.Leases[0].Scope != "card" || len(back.Leases[0].Devices) != 1 || back.Leases[0].Devices[0] != "gpu-cccc0000-x" {
		t.Fatalf("per-lease device set: %+v", back.Leases)
	}
}

// A whole-node lease is legible per card too: every card row names the holder and says
// the scope is the whole node (this is how a legacy lease, like a long-running render
// started before card-scoped leases, shows up).
func TestStatusWholeNodeLeaseShowsOnEveryCardRow(t *testing.T) {
	_, m := leaseFixture(t)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "film", TTL: time.Hour, Command: "python film.py"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	rows := gpucards.Rows(statusCards(), m.Leases())
	if len(rows) != 3 {
		t.Fatalf("rows: %d", len(rows))
	}
	for _, r := range rows {
		if r.State != "held" || r.Holder == nil || r.Holder.Epoch != l.Epoch() || r.Holder.Scope != "whole-node" {
			t.Errorf("card %d: %+v", r.Index, r)
		}
	}
	text := strings.Join(gpucards.Table(rows), "\n")
	if !strings.Contains(text, "whole-node") || !strings.Contains(text, "epoch") {
		t.Fatalf("the text table must say whole-node and name the epoch:\n%s", text)
	}
}

func TestStatusQueueEntriesShowTheirDeviceSet(t *testing.T) {
	_, m := scopedLeaseFixture(t)
	rows := gpucards.QueueRows([]gpulease.Waiter{
		{PID: 11, Class: gpulease.ClassMedia, Reason: "a", SinceMs: time.Now().UnixMilli(), Devices: []string{"gpu-cccc0000-x"}},
		{PID: 12, Class: gpulease.ClassText, Reason: "b", SinceMs: time.Now().UnixMilli()},
	})
	_ = m
	if len(rows) != 2 {
		t.Fatalf("rows: %v", rows)
	}
	if d, _ := rows[0]["devices"].([]string); len(d) != 1 || d[0] != "gpu-cccc0000-x" {
		t.Errorf("a card-scoped waiter shows its cards: %v", rows[0])
	}
	if rows[1]["scope"] != "whole-node" {
		t.Errorf("a waiter with no device set is a whole-node waiter: %v", rows[1])
	}
	// The keys the existing consumers read are still there, with their old types.
	for _, k := range []string{"pid", "class", "reason", "since"} {
		if _, ok := rows[0][k]; !ok {
			t.Errorf("queued row lost key %q", k)
		}
	}
}

func TestRenderCardTableContainsTheFacts(t *testing.T) {
	rows := gpucards.Rows(statusCards(), nil)
	text := strings.Join(gpucards.Table(rows), "\n")
	for _, want := range []string{"GPU-aaaa0000-x", "GPU-bbbb0000-x", "display", "free", "15.0/16.0", "Test Card A"} {
		if !strings.Contains(text, want) {
			t.Errorf("the table must contain %q:\n%s", want, text)
		}
	}
	// The ComfyUI order column shows "?" for a card whose order is not known.
	unknown, _ := gpuprobe.BuildCards([]gpuprobe.Device{
		{Index: 0, UUID: "GPU-x1", Name: "A", TotalGiB: 16, FreeGiB: 16, UtilKnown: true}, {Index: 1, UUID: "GPU-x2", Name: "A", TotalGiB: 16, FreeGiB: 16, UtilKnown: true},
	}, "")
	if text := strings.Join(gpucards.Table(gpucards.Rows(unknown, nil)), "\n"); !strings.Contains(text, "?") {
		t.Errorf("an unknown ComfyUI order must read ?:\n%s", text)
	}
}
