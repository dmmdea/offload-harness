package mcpserver

import (
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuactivity"
)

// The yield request (plan P11, ADR 0085) reaches the one-line brief a session reads first: an
// overdue lease whose holder was asked to yield says so, and the sentence is not the part a fixed
// width cuts off.
func TestBriefVerdictOverdueNamesAskedYield(t *testing.T) {
	const sentence = "its holder was asked to yield at 14:02 (2 waiter(s) queued) and ran its on_yield at 14:02 (exit 0); asked 14m0s ago and it has not ended: nothing is killed; ask whoever owns it, or stop its job by hand (wrapper pid 77); the claim frees when the wrapper ends"
	h := &gpuactivity.Holder{PID: 77, Alive: true, Class: "media", Epoch: 44, Overdue: true, OverdueBySec: 3 * 3600, Expired: true,
		ExpiredWhy: "its owner is gone and its progress contract is not advancing (its progress file has been silent for longer than its 30m0s stall window)",
		OwnerState: "gone", OwnerSession: "00000000-0000-4000-8000-000000000001", OrphanGraceS: 900,
		YieldFile: "/state/gpu/lease/yield.44",
		YieldNote: sentence,
		Yield:     &gpuactivity.YieldState{AskedAt: time.Now().Add(-14 * time.Minute).UTC().Format(time.RFC3339), Action: "ran on_yield", Waiters: 2, Unanswered: true}}
	verdict, note := gpuactivity.Assess(gpuactivity.View{At: time.Now(), Held: true, Holder: h, Seat: gpuactivity.SeatState{Name: "agent-pool", Loaded: true}})
	if verdict != gpuactivity.VerdictHeldOverdue {
		t.Fatalf("setup: %s (%s)", verdict, note)
	}
	if !strings.Contains(note, sentence) {
		t.Fatalf("the assessed note carries the yield sentence verbatim:\n%s", note)
	}
	view := map[string]any{"verdict": verdict, "held": true, "pid": 77, "class": "media", "age_s": 1, "reason": "film",
		"activity": map[string]any{"note": note, "holder": h}}
	line := gpuLeaseVerdictLine(view)
	for _, want := range []string{"OVERDUE", "was asked to yield", "has not ended", "the claim frees when the wrapper ends", "gpu takeover --epoch 44"} {
		if !strings.Contains(line, want) {
			t.Errorf("the brief line lacks %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "gpu release") {
		t.Errorf("a live wrapper's lease is never sent to `gpu release`:\n%s", line)
	}
	// A lease with nothing to say about the request keeps the line it had.
	h.YieldNote, h.Yield = "", nil
	_, plain := gpuactivity.Assess(gpuactivity.View{At: time.Now(), Held: true, Holder: h, Seat: gpuactivity.SeatState{Name: "agent-pool", Loaded: true}})
	if strings.Contains(plain, "yield") {
		t.Errorf("no yield state, no yield word: %s", plain)
	}
}
