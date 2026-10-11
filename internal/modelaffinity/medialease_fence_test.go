package modelaffinity

import (
	"context"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// Register F15 said "a media lease is not exclusive, so a text seat reloads under it between
// renders". The first half is true of the FLAG (Meta.Exclusive is stamped on text leases only) and
// not of the fence: blocksLoadOne gates a media lease from the moment its drain is over, to the
// cards it holds, exactly as it gates an exclusive text lease. This pins that equivalence for every
// door the harness loads a model through, and the other half of the rule (a load that lands on a
// card nobody holds proceeds at once), so a change to either lease class cannot open one of them
// without a test saying so.
//
// What the fence does not close is named in docs/systems/gpu-lease.md ("Between renders"): the
// holder's own process tree (insideLease) and clients that are not the harness. The render helper
// answers those from its side (render/gpu-lock.mjs, the re-check before each later job).
func TestMediaLeaseFencesTextLoadsOnItsCardsExactlyLikeAnExclusiveTextLease(t *testing.T) {
	for _, tc := range []struct {
		name      string
		class     gpulease.Class
		exclusive bool
	}{
		{"media", gpulease.ClassMedia, false},
		{"exclusive text", gpulease.ClassText, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := cardScoped(t)
			armSeatScope(t, tripleBoxPins)
			holdLease(t, m, tc.class, gpulease.Options{Reason: "render", Origin: "test", Devices: []string{idCard2}, Exclusive: tc.exclusive})

			// A load that would land on the leased card waits for the card, and says whose it is.
			for _, seat := range []string{"seat-card2", "seat-pair", "seat-triple"} {
				_, err := Admit(context.Background(), "http://fence-"+tc.name+"-"+seat, seat, 300*time.Millisecond)
				if !IsLeaseRefusal(err) {
					t.Errorf("%s: a load of %s onto the leased card must wait, got %v", tc.name, seat, err)
				}
			}
			// The same through the other doors that can make llama-swap load a model.
			stub := newStubSwap(t, nil)
			if _, err := AwaitUpstream(context.Background(), stub.srv.URL, "seat-card2", "/props", time.Now()); !IsLeaseRefusal(err) {
				t.Errorf("%s: an /upstream probe of a leased-card seat must wait, got %v", tc.name, err)
			}
			if err := AwaitRunSlot(context.Background(), "http://fence-run-"+tc.name, "seat-card2", time.Now()); !IsLeaseRefusal(err) {
				t.Errorf("%s: a new run on a leased-card seat must wait, got %v", tc.name, err)
			}

			// A load onto a card nobody holds proceeds at once, through every door.
			tk := admitFast(t, "http://free-"+tc.name, "seat-card0")
			tk.Release()
			if _, err := AwaitUpstream(context.Background(), stub.srv.URL, "seat-card0", "/props", time.Now()); err != nil {
				t.Errorf("%s: an /upstream probe of a free-card seat = %v", tc.name, err)
			}
			if err := AwaitRunSlot(context.Background(), "http://free-run-"+tc.name, "seat-card0", time.Now()); err != nil {
				t.Errorf("%s: a new run on a free-card seat = %v", tc.name, err)
			}
		})
	}
}

// A media lease that is still DRAINING has not cleared anything yet and does not fence: blocking
// the requests of the runs it is waiting for is the 2026-09-14 deadlock. The fence starts when the
// holder clears the stamp, which is when the lease starts to matter to a load.
func TestMediaLeaseFenceStartsWhenTheDrainEnds(t *testing.T) {
	m := cardScoped(t)
	armSeatScope(t, tripleBoxPins)
	l := holdLease(t, m, gpulease.ClassMedia, gpulease.Options{Reason: "render", Devices: []string{idCard2}, Draining: true})
	tk := admitFast(t, "http://draining-media", "seat-card2")
	tk.Release()
	if err := m.Restamp(l.Epoch(), func(meta *gpulease.Meta) { meta.Draining = false }); err != nil {
		t.Fatal(err)
	}
	if _, err := Admit(context.Background(), "http://drained-media", "seat-card2", 300*time.Millisecond); !IsLeaseRefusal(err) {
		t.Fatalf("a media lease whose drain is over must fence its card, got %v", err)
	}
}
