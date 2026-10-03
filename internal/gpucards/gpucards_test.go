package gpucards

import (
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

func cards3() []gpuprobe.Card {
	devs := []gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000-x", Name: "T", TotalGiB: 16, FreeGiB: 15},
		{Index: 1, UUID: "GPU-bbbb0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16, DisplayActive: true},
		{Index: 2, UUID: "GPU-cccc0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
	}
	c, _ := gpuprobe.BuildCards(devs, "")
	return c
}

func TestRowsCardLeaseHoldsOnlyItsCards(t *testing.T) {
	rows := Rows(cards3(), []gpulease.Info{{Held: true, Epoch: 7, Class: gpulease.ClassMedia, Devices: []string{"gpu-cccc0000-x"}, Group: "g"}})
	if rows[2].State != "held" || rows[2].Holder.Epoch != 7 || rows[2].Holder.Scope != "card" || rows[2].Holder.Group != "g" {
		t.Fatalf("card 2: %+v", rows[2])
	}
	if rows[0].State != "free" || rows[0].Holder != nil || rows[1].State != "free" {
		t.Fatalf("cards 0 and 1 must be free: %+v %+v", rows[0], rows[1])
	}
}

func TestRowsWholeNodeLeaseHoldsEveryCardAndSaysSo(t *testing.T) {
	rows := Rows(cards3(), []gpulease.Info{{Held: true, Epoch: 3, Class: gpulease.ClassText}})
	for _, r := range rows {
		if r.State != "held" || r.Holder.Scope != "whole-node" || r.Holder.Epoch != 3 {
			t.Fatalf("card %d: %+v", r.Index, r)
		}
	}
}

func TestSectionHasTheStableKeys(t *testing.T) {
	sec := Section(cards3(), "a note", []gpulease.Info{{Held: true, Epoch: 1, Class: gpulease.ClassMedia, Age: time.Minute}}, nil)
	for _, k := range []string{"cards", "leases", "queued", "cards_note"} {
		if _, ok := sec[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
	if q, _ := sec["queued"].([]map[string]any); q == nil || len(q) != 0 {
		t.Errorf("an empty queue is an empty list, never null: %#v", sec["queued"])
	}
	empty := Section(nil, "", nil, nil)
	if c, _ := empty["cards"].([]Row); c == nil || len(c) != 0 {
		t.Errorf("no card table is an empty list, never null: %#v", empty["cards"])
	}
	if _, ok := empty["cards_note"]; ok {
		t.Error("no note, no key")
	}
}

func TestTableClipsALongCommandAndKeepsTheUUIDWhole(t *testing.T) {
	long := strings.Repeat("x", 300)
	lines := Table(Rows(cards3(), []gpulease.Info{{Held: true, Epoch: 1, Class: gpulease.ClassMedia, Command: long, Devices: []string{"gpu-aaaa0000-x"}}}))
	text := strings.Join(lines, "\n")
	if strings.Contains(text, long) {
		t.Error("a long command must be clipped")
	}
	if !strings.Contains(text, "GPU-aaaa0000-x") {
		t.Error("the UUID is printed whole so it can be copied")
	}
}

// A legacy whole-node lease that the evidence rule scoped says so in the per-lease rows: the
// record is still a whole-node claim (the card table keeps reading it that way), and the seat
// scope is shown beside it with its evidence and whether it has widened.
func TestLeaseRowsShowAnInferredSeatScope(t *testing.T) {
	inferred := gpulease.Info{Held: true, Epoch: 1205, Class: gpulease.ClassMedia,
		Inferred: []string{"gpu-cccc0000-x"}, Scope: gpulease.ScopeInferred,
		ScopeWhy: "inferred from command line; the record itself is a whole-node claim", ScopeWidened: true}
	rows := LeaseRows([]gpulease.Info{inferred})
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	r := rows[0]
	if r["scope"] != "whole-node" {
		t.Errorf("scope = %v: the record is a whole-node claim whatever was inferred", r["scope"])
	}
	if r["seat_scope"] != "inferred" || r["scope_widened"] != true {
		t.Errorf("seat_scope=%v widened=%v, want inferred and widened", r["seat_scope"], r["scope_widened"])
	}
	if got, _ := r["inferred_devices"].([]string); len(got) != 1 || got[0] != "gpu-cccc0000-x" {
		t.Errorf("inferred_devices = %v", r["inferred_devices"])
	}
	if why, _ := r["scope_why"].(string); !strings.Contains(why, "command line") {
		t.Errorf("scope_why = %q", why)
	}

	// No evidence: whole-node, and the reason it stayed so is shown.
	plain := gpulease.Info{Held: true, Epoch: 1206, Class: gpulease.ClassMedia, ScopeWhy: "stays whole-node: no command line names a card"}
	r = LeaseRows([]gpulease.Info{plain})[0]
	if r["seat_scope"] != "whole-node" {
		t.Errorf("seat_scope = %v", r["seat_scope"])
	}
	if _, ok := r["inferred_devices"]; ok {
		t.Error("nothing inferred, no inferred_devices key")
	}
	if why, _ := r["scope_why"].(string); !strings.Contains(why, "stays whole-node") {
		t.Errorf("the reason it stayed whole-node must be shown: %q", why)
	}
	// A whole-node lease nobody tried to scope keeps today's keys exactly.
	bare := LeaseRows([]gpulease.Info{{Held: true, Epoch: 1, Class: gpulease.ClassText}})[0]
	for _, k := range []string{"seat_scope", "scope_why", "scope_widened", "inferred_devices"} {
		if _, ok := bare[k]; ok && k != "seat_scope" {
			t.Errorf("a lease with nothing to say about its seat scope must not grow a %q key", k)
		}
	}
}
