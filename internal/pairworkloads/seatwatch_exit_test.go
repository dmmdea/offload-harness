package pairworkloads

// C04 (2026-10-09): a seat-watch card opened at 12:19:49 stayed open for 642 s and was closed "harness
// process exited before the job finished" by the first sweep of the fleet-serve that came after a restart.
// The watcher closes a card when the seat leaves llama-swap's list and when the seat's metrics stay
// unreadable for a minute; two paths left a card open for as long as the watcher lived: llama-swap itself
// not answering (Poll returned at once, "nothing is known, nothing changes", with no bound), and a harness
// load that could not be resolved (the card was held untouched, with no bound). Both are bounded now. And a
// seat card orphaned by the death of its WATCHER is closed as the stretch it was, not as a failed harness
// job, because there was no harness job: see orphans_seat_test.go.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/swapclient"
)

func TestSeatWatcherClosesAnOpenCardWhenLlamaSwapStopsAnswering(t *testing.T) {
	rig := newSeatRig(t)
	rig.swap.set(func(f *fakeSwap) { f.running = 1 })
	rig.poll()
	rig.poll()
	if got := rig.states(); len(got) != 1 || got[0] != "running" {
		t.Fatalf("frames = %v, want [running]", got)
	}
	firstID := rig.pair.info(0)["id"]

	rig.swap.set(func(f *fakeSwap) { f.swapDown = true })
	for i := 0; i < seatUnreadableLimit-1; i++ {
		rig.poll()
	}
	if n := rig.pair.count(); n != 1 {
		t.Fatalf("the card changed after %d unanswered polls, inside the limit of %d: %v", seatUnreadableLimit-1, seatUnreadableLimit, rig.states())
	}
	rig.poll()
	if got := rig.states(); len(got) != 2 || got[1] != "failed" {
		t.Fatalf("frames = %v, want [running failed] once llama-swap has been silent for the limit", got)
	}
	if e, _ := rig.pair.info(1)["error"].(string); !strings.Contains(e, "llama-swap") || !strings.Contains(e, "stopped answering") {
		t.Errorf("the failure must say what was unreadable: %q", e)
	}
	if rig.pair.info(1)["id"] != firstID {
		t.Errorf("the terminal frame must close the SAME card: %v != %v", rig.pair.info(1)["id"], firstID)
	}
	for i := 0; i < 5; i++ {
		rig.poll()
	}
	if n := rig.pair.count(); n != 2 {
		t.Fatalf("exactly one terminal frame; more polls of a silent llama-swap sent %d frame(s) in all: %v", n, rig.states())
	}

	// llama-swap comes back with the seat serving direct traffic again: a NEW stretch, a new card.
	rig.swap.set(func(f *fakeSwap) { f.swapDown = false })
	rig.poll()
	rig.poll()
	if got := rig.states(); len(got) != 3 || got[2] != "running" {
		t.Fatalf("frames = %v, want [running failed running]", got)
	}
	if rig.pair.info(2)["id"] == firstID {
		t.Errorf("a new stretch must be a new card, got the old id again")
	}
}

func TestSeatWatcherDoesNotCountAnAnsweringLlamaSwapAsSilent(t *testing.T) {
	rig := newSeatRig(t)
	rig.swap.set(func(f *fakeSwap) { f.running = 1 })
	rig.poll()
	rig.poll()
	// Alternate silence and answers: the count is of CONSECUTIVE silent polls, so a flapping llama-swap that
	// keeps answering between failures never reaches the limit.
	for i := 0; i < seatUnreadableLimit*3; i++ {
		rig.swap.set(func(f *fakeSwap) { f.swapDown = i%2 == 0 })
		rig.poll()
	}
	if got := rig.states(); len(got) != 1 {
		t.Fatalf("a llama-swap that answers every other poll closed the card: %v", got)
	}
}

func TestSeatWatcherCompletesACardItCanNoLongerFollow(t *testing.T) {
	rig := newSeatRig(t)
	rig.swap.set(func(f *fakeSwap) { f.running = 1 })
	rig.poll()
	rig.poll()
	lastBusy := rig.clock.UnixMilli()
	// From here the harness's own load on the seat cannot be resolved (the roster is unreadable and a marker
	// names an alias): the watcher can neither subtract it nor tell the end of the stretch.
	rig.w.fetchRoster = func(context.Context, string, time.Duration) (swapclient.Roster, error) {
		return swapclient.Roster{}, fmt.Errorf("gone")
	}
	rig.w.rosterOK = false
	rig.harness["agent-pool"] = 1
	for i := 0; i < seatUnreadableLimit-1; i++ {
		rig.poll()
	}
	if n := rig.pair.count(); n != 1 {
		t.Fatalf("the open card changed inside the limit: %v", rig.states())
	}
	rig.poll()
	got := rig.states()
	if len(got) != 2 || got[1] != "completed" {
		t.Fatalf("frames = %v, want [running completed]: a stretch that can no longer be followed ends where it was last seen, it is not a failure", got)
	}
	if at := int64(rig.pair.info(1)["completedAt"].(float64)); at != lastBusy {
		t.Errorf("completedAt = %d, want the last busy poll %d", at, lastBusy)
	}
	for i := 0; i < 5; i++ {
		rig.poll()
	}
	if n := rig.pair.count(); n != 2 {
		t.Fatalf("exactly one terminal frame, got %d frames: %v", n, rig.states())
	}
}

func TestSeatWatcherResetsTheBlindCountWhenTheLoadResolvesAgain(t *testing.T) {
	rig := newSeatRig(t)
	// Two requests on the seat, one of them the harness's: once the load resolves, one is still direct.
	rig.swap.set(func(f *fakeSwap) { f.running = 2 })
	rig.poll()
	rig.poll()
	good := rig.w.fetchRoster
	for round := 0; round < 3; round++ {
		rig.w.fetchRoster = func(context.Context, string, time.Duration) (swapclient.Roster, error) {
			return swapclient.Roster{}, fmt.Errorf("gone")
		}
		rig.w.rosterOK = false
		rig.harness["agent-pool"] = 1
		for i := 0; i < seatUnreadableLimit-2; i++ {
			rig.poll()
		}
		rig.w.fetchRoster = good // resolves again: one poll that knows
		rig.poll()
	}
	if got := rig.states(); len(got) != 1 {
		t.Fatalf("blind stretches shorter than the limit, each ended by a poll that knew, closed the card: %v", got)
	}
}
