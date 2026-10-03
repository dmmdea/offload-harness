package gpuactivity

// What a status call says about a lease's terms (plan P9): an expired lease is a held,
// overdue one that says why its term was not renewed; a lease within its term says what
// renews it. Nothing here is a new verdict word (docs_lint's TestVerdictDocTableComplete).

import (
	"strings"
	"testing"
	"time"
)

func TestExpiredLeaseReadsHeldOverdueAndSaysWhy(t *testing.T) {
	root, _ := leaseFixture(t, func(rec map[string]any) {
		rec["acquired_at_ms"] = time.Now().Add(-9 * time.Hour).UnixMilli()
		rec["expires_at_ms"] = time.Now().Add(-3 * time.Hour).UnixMilli()
		rec["term_ms"] = (6 * time.Hour).Milliseconds()
		rec["requested_ms"] = (20 * time.Hour).Milliseconds()
		rec["max_total_ms"] = (48 * time.Hour).Milliseconds()
		rec["expired"] = true
		rec["expired_why"] = "its owner is gone"
		rec["command"] = "python film.py"
	})
	v := snap(root, 5, nil) // the cards are quiet; the standing is what speaks
	if v.Verdict != VerdictHeldOverdue {
		t.Fatalf("an expired lease that is still held reads held-overdue, got %q (%s)", v.Verdict, v.Note)
	}
	for _, want := range []string{"expired", "its owner is gone", "nothing is reclaimed or killed", "gpu takeover --epoch 41"} {
		if !strings.Contains(v.Note, want) {
			t.Errorf("note lacks %q: %s", want, v.Note)
		}
	}
	h := v.Holder
	if h == nil || !h.Expired || h.ExpiredWhy != "its owner is gone" || !h.Overdue {
		t.Fatalf("holder: %+v", h)
	}
	if h.TermSec != 6*3600 || h.RequestedSec != 20*3600 || h.HardEnd == "" {
		t.Fatalf("holder terms: term %ds requested %ds hard end %q", h.TermSec, h.RequestedSec, h.HardEnd)
	}
	he, err := time.Parse(time.RFC3339, h.HardEnd)
	want := time.Now().Add(-9 * time.Hour).Add(48 * time.Hour)
	if err != nil || he.Sub(want) > 10*time.Second || want.Sub(he) > 10*time.Second {
		t.Errorf("hard end %q (%v) is not 48h after the acquisition (%s)", h.HardEnd, err, want.Format(time.RFC3339))
	}
}

func TestOverdueLeaseThatWasNeverLabelledKeepsItsOldSentence(t *testing.T) {
	// A lease no tick has looked at (a pre-terms wrapper, a holder between ticks) is overdue by
	// its window exactly as before, and does not claim an expiry nobody stamped.
	root, _ := leaseFixture(t, func(rec map[string]any) {
		rec["expires_at_ms"] = time.Now().Add(-30 * time.Minute).UnixMilli()
	})
	v := snap(root, 5, nil)
	if v.Verdict != VerdictHeldOverdue {
		t.Fatalf("verdict %q (%s)", v.Verdict, v.Note)
	}
	if strings.Contains(v.Note, "expired") {
		t.Errorf("an unlabelled overdue lease must not say it expired: %s", v.Note)
	}
	if h := v.Holder; h == nil || h.Expired || h.TermSec != 0 || h.HardEnd != "" {
		t.Errorf("no terms on the record, none on the holder: %+v", h)
	}
}

func TestUtilWorkingReadsOnlyTheLeaseCardsAndSkipsDisplay(t *testing.T) {
	gpus := []GPU{
		{Index: 0, UUID: "GPU-AAAA", UtilPct: 3, UtilKnown: true},
		{Index: 1, UUID: "GPU-BBBB", UtilPct: 90, UtilKnown: true, DisplayActive: true},
		{Index: 2, UUID: "GPU-CCCC", UtilPct: 80, UtilKnown: true},
		{Index: 3, UUID: "GPU-DDDD", UtilKnown: false},
	}
	if !UtilWorking(gpus, nil) {
		t.Error("a whole-node lease is working when any non-display card is busy")
	}
	if UtilWorking(gpus, []string{"gpu-aaaa"}) {
		t.Error("a lease on a quiet card is not working because another card is")
	}
	if !UtilWorking(gpus, []string{"gpu-aaaa", "gpu-cccc"}) {
		t.Error("a lease on a busy card is working (lease ids are lower case, uuids are not)")
	}
	if UtilWorking(gpus, []string{"gpu-bbbb"}) {
		t.Error("the display card's load is the desktop's, never the lease's")
	}
	if UtilWorking(gpus, []string{"gpu-dddd"}) {
		t.Error("an unknown reading is not work")
	}
	if UtilWorking(nil, nil) {
		t.Error("no reading is not work")
	}
	// The threshold is the one the verdict uses.
	edge := []GPU{{Index: 0, UUID: "GPU-AAAA", UtilPct: utilBusyPct, UtilKnown: true}}
	below := []GPU{{Index: 0, UUID: "GPU-AAAA", UtilPct: utilBusyPct - 1, UtilKnown: true}}
	if !UtilWorking(edge, nil) || UtilWorking(below, nil) {
		t.Error("the busy threshold is the verdict's own")
	}
}
