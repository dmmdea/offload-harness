package visionremote

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
	"github.com/dmmdea/offload-harness/internal/rosterprobe/rostertest"
)

// A roster entry the tailnet guard refuses is a named miss, never a dial and never a failed
// call: the agent lane refuses it by shape, and the vision lane used to dial the same entry
// through the dial gate alone, so one entry was refused by one lane and used by another
// (ADR 0074). The entries after it still serve.
func TestPickNodeNamesARosterEntryTheTailnetGuardRefusesAndDoesNotDialIt(t *testing.T) {
	rostertest.Zones(t) // no zone listed: a dotted tailnet name is refused by shape
	refused := "http://node-x.tailkkkkkk.ts.net:18811"
	live := newFakeNode(t, "node-b", []string{"vision"}, remoteOK)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{refused, live.srv.URL}

	base, node, err := pickNode(context.Background(), cfg, "vqa")
	if err != nil || base != live.srv.URL || node != "node-b" {
		t.Fatalf("pickNode = (%q, %q, %v), want the live node past the refused entry", base, node, err)
	}

	cfg.DelegateRemotes = []string{refused}
	_, _, err = pickNode(context.Background(), cfg, "vqa")
	var pe *placementError
	if !errors.As(err, &pe) || pe.class != core.DeferClassCapacity {
		t.Fatalf("err = %v, want a capacity placementError", err)
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
	live := newFakeNode(t, "node-b", []string{"vision"}, remoteOK)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{holes[0].URL(), holes[1].URL(), holes[2].URL(), live.srv.URL}

	start := time.Now()
	base, node, err := pickNode(context.Background(), cfg, "vqa")
	wall := time.Since(start)
	if err != nil || base != live.srv.URL || node != "node-b" {
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
	if _, _, err := pickNode(context.Background(), cfg, "vqa"); err != nil {
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

// A roster entry may have been pasted with a token or user:password in it. The "no fleet node is eligible
// ... probed <list>" error is printed to the caller and ends up in chats, so every cause in the list names the
// entry redacted: a refused entry, a transport failure, and a node that answered but lacks the lane.
func TestPickNodeNeverPrintsACredentialFromARosterEntry(t *testing.T) {
	rostertest.Zones(t)
	rosterprobe.Default.Reset()
	t.Cleanup(rosterprobe.Default.Reset)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := l.Addr().String()
	_ = l.Close()
	noLane := newFakeNode(t, "node-c", []string{"text"}, remoteOK) // answers health, serves no vision lane
	withCreds := strings.Replace(noLane.srv.URL, "http://", "http://user:pw-s3cret@", 1)

	cfg := config.Default()
	cfg.DelegateRemotes = []string{
		"http://user:pw-s3cret@node-x.tailkkkkkk.ts.net:18811/?token=tok-s3cret", // refused by the guard
		"http://user:pw-s3cret@" + closed + "/?token=tok-s3cret",                 // refused at the transport
		withCreds, // answers, no vision lane
	}
	_, _, err = pickNode(context.Background(), cfg, "vqa")
	if err == nil {
		t.Fatal("no node serves vision here; pickNode must fail")
	}
	for _, mark := range []string{"s3cret", "user:"} {
		if strings.Contains(err.Error(), mark) {
			t.Errorf("the error carries %q:\n%s", mark, err)
		}
	}
	for _, want := range []string{"not dialled", closed, "no vision lane"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the redacted error must still say %q:\n%s", want, err)
		}
	}
}
