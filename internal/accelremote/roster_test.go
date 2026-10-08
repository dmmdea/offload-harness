package accelremote

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
	"github.com/dmmdea/offload-harness/internal/rosterprobe/rostertest"
)

// A roster entry the tailnet guard refuses is a named miss, never a dial and never a failed
// call (ADR 0074): the accelerator lane used to dial it through the dial gate alone while the
// agent lane refused it by shape. The first entry that lists the device still wins, and a
// refused entry before it is skipped, not tried.
func TestPickNodeNamesARosterEntryTheTailnetGuardRefusesAndDoesNotDialIt(t *testing.T) {
	rostertest.Zones(t)
	refused := "http://node-x.tailkkkkkk.ts.net:18811"
	live := newFakeNode(t, "node-c", []string{"coral-edgetpu"}, `{}`)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{refused, live.srv.URL}

	base, node, err := pickNode(context.Background(), cfg, "coral-edgetpu")
	if err != nil || base != live.srv.URL || node != "node-c" {
		t.Fatalf("pickNode = (%q, %q, %v), want the live node past the refused entry", base, node, err)
	}

	cfg.DelegateRemotes = []string{refused}
	_, _, err = pickNode(context.Background(), cfg, "coral-edgetpu")
	if err == nil {
		t.Fatal("a roster of only a refused entry must not place")
	}
	for _, want := range []string{refused, "not dialled", "tailnet guard"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must contain %q", err, want)
		}
	}
	for _, dialled := range []string{"refusing dial", "no such host", "connection"} {
		if strings.Contains(err.Error(), dialled) {
			t.Errorf("error %q reads as though the refused entry was dialled (%q)", err, dialled)
		}
	}
}

// k dead roster members used to cost k health bounds IN SERIES before this lane chose a node, on every call
// (the 2026-10-07 membership review). The roster is now probed at once through the shared cache: the call
// costs about one bound, each hung node is dialled once, and a second call inside the negative window
// dials it zero more times.
func TestPickNodeProbesAHungRosterOnceAndInParallel(t *testing.T) {
	rostertest.Zones(t)
	rosterprobe.Default.Reset()
	t.Cleanup(rosterprobe.Default.Reset)
	prev := healthTimeout
	healthTimeout = time.Second
	t.Cleanup(func() { healthTimeout = prev })

	holes := []*rostertest.Hole{rostertest.NewBlackHole(t), rostertest.NewBlackHole(t), rostertest.NewBlackHole(t)}
	live := newFakeNode(t, "node-c", []string{"coral-edgetpu"}, `{}`)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{holes[0].URL(), holes[1].URL(), holes[2].URL(), live.srv.URL}

	start := time.Now()
	base, node, err := pickNode(context.Background(), cfg, "coral-edgetpu")
	wall := time.Since(start)
	if err != nil || base != live.srv.URL || node != "node-c" {
		t.Fatalf("pickNode = (%q, %q, %v), want the live node past three hung ones", base, node, err)
	}
	// In series this is at least 3 x the bound (3 s) before the live node is asked; at once it is one bound plus overhead. The ceilings leave a loaded runner generous room (the counts below are the real check).
	if wall >= 5*healthTimeout/2 {
		t.Errorf("a roster with 3 hung members took %s to place, want about one bound (%s), not the sum", wall, healthTimeout)
	}
	for i, h := range holes {
		if h.Dials() != 1 {
			t.Errorf("hung member %d dialled %d times, want once", i, h.Dials())
		}
	}

	start = time.Now()
	if _, _, err := pickNode(context.Background(), cfg, "coral-edgetpu"); err != nil {
		t.Fatalf("second pickNode: %v", err)
	}
	if again := time.Since(start); again >= healthTimeout {
		t.Errorf("the second call took %s: it waited on a node the first call had already found dead", again)
	}
	for i, h := range holes {
		if h.Dials() != 1 {
			t.Errorf("hung member %d dialled %d times across two calls, want still once (negative cache)", i, h.Dials())
		}
	}
}
