package gpualloc

// QueuedClaimsBefore (P0 plan S2): a caller that RESUMES a place in line is served by arrival time, so only the places
// that arrived strictly before it hold a card against it. A new arrival (a zero time) is behind everyone.

import (
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

func TestQueuedClaimsBeforeCountsOnlyWhatArrivedBeforeThePlace(t *testing.T) {
	cards := threeCards()
	m := scratchManager(t)
	a, c := cards[0].LeaseID(), cards[2].LeaseID()
	now := time.Now()
	leave := func(dev string, at time.Time) gpulease.Token {
		tok, err := m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Devices: []string{dev}}, at)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	older := leave(a, now.Add(-time.Minute))
	leave(c, now) // arrived at the very instant of the place under test: a tie is not ahead of it
	leave(c, now.Add(time.Minute))

	// The place under test arrived at `now`.
	got := QueuedClaimsBefore(m, cards, "", false, now)
	if !got[a] {
		t.Errorf("the place that arrived a minute before is ahead: %v", got)
	}
	if got[c] {
		t.Errorf("a place that arrived at the same instant or later is not ahead (a tie errs toward free): %v", got)
	}
	// A new arrival is behind all of them.
	all := QueuedClaimsBefore(m, cards, "", false, time.Time{})
	if !all[a] || !all[c] {
		t.Errorf("a zero time is a new arrival: everyone is ahead: %v", all)
	}
	if want := QueuedClaims(m, cards, "", false); len(want) != len(all) || want[a] != all[a] || want[c] != all[c] {
		t.Errorf("QueuedClaims is QueuedClaimsBefore for a new arrival: %v vs %v", want, all)
	}
	// The place's own token is never counted.
	if own := QueuedClaimsBefore(m, cards, older.ID, false, now); own[a] {
		t.Errorf("a call's own place is never ahead of it: %v", own)
	}
}
