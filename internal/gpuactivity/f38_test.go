package gpuactivity

// F38 (2026-10-09): `gpu status` read HELD-IDLE on a live krea2 batch between two pictures and called
// card 0 "the display card". Two defects: the sentence named the busiest excluded card the display card
// whenever the lease's work cards were quiet, whatever that card was; and a batch between items, whose
// cards are quiet by nature, had nothing to read as anything but idle. The first is fixed by naming a
// card the display card only when the card table marks it so; the second by the progress contract a
// wrapped render helper now keeps (see render/lease-progress.mjs), which reads held-working while it moves.

import (
	"strings"
	"testing"
	"time"
)

const (
	f38CardA = "GPU-aaaa0000-1111-2222-3333-444455556666"
	f38CardB = "GPU-bbbb0000-1111-2222-3333-444455556666"
	f38CardC = "GPU-cccc0000-1111-2222-3333-444455556666"
)

func f38Cards(utilA, utilB, utilC int, displayOn string) []GPU {
	mk := func(i int, uuid string, u int) GPU {
		return GPU{Index: i, UUID: uuid, Name: "RTX " + uuid[4:8], UtilPct: u, UtilKnown: true, MemTotalMiB: 16303, DisplayActive: uuid == displayOn}
	}
	return []GPU{mk(0, f38CardA, utilA), mk(1, f38CardB, utilB), mk(2, f38CardC, utilC)}
}

func f38Holder(devices ...string) *Holder {
	return &Holder{PID: 1234, Alive: true, Class: "media", AgeSec: 600, Reason: "krea2 batch", Devices: devices,
		Command: "node D:\\x\\render\\comfy-generate.mjs --batch jobs.jsonl"}
}

func f38Assess(h *Holder, gpus []GPU) (string, string) {
	return Assess(View{At: time.Now(), Held: true, Holder: h, Seat: SeatState{Name: "agent-pool"}, GPUs: gpus})
}

func TestABusyCardOutsideTheLeaseIsNotCalledTheDisplayCard(t *testing.T) {
	// The lease holds card C. Card A is busy with something that is not this lease's (a seat, another lease or
	// another process) and drives no screen; the monitor is on card B, idle.
	h := f38Holder(strings.ToLower(f38CardC))
	h.Progress = &ProgressState{State: "unknown", Problem: "does not exist"}
	verdict, note := f38Assess(h, f38Cards(90, 0, 0, f38CardB))
	if verdict != VerdictHeldIdle {
		t.Fatalf("verdict = %q, want held-idle (the lease's own card is quiet)\n%s", verdict, note)
	}
	if strings.Contains(note, "display card") {
		t.Errorf("card 0 drives no screen; the note must not call it the display card:\n%s", note)
	}
	for _, want := range []string{"90% on card 0", "outside this lease's cards", "not the holder's work"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note must say %q:\n%s", want, note)
		}
	}
}

func TestTheDisplayCardIsWhicheverCardTheTableMarksNeverAnIndex(t *testing.T) {
	// The monitor on card A (index 0) and on card B (index 1): the same busy 33% is the display card exactly
	// where the card table says so.
	for _, c := range []struct {
		display   string
		wantIndex string
	}{{f38CardA, "33% on card 0"}, {f38CardB, "33% on card 1"}, {f38CardC, "33% on card 2"}} {
		utils := map[string]int{f38CardA: 0, f38CardB: 0, f38CardC: 0}
		utils[c.display] = 33
		// The lease holds a quiet card that is not the display card.
		held := f38CardA
		if c.display == f38CardA {
			held = f38CardB
		}
		_, note := f38Assess(f38Holder(strings.ToLower(held)), f38Cards(utils[f38CardA], utils[f38CardB], utils[f38CardC], c.display))
		if !strings.Contains(note, c.wantIndex) || !strings.Contains(note, "that is the display card") {
			t.Errorf("display on %s: the busy display card must be named as such:\n%s", c.display, note)
		}
		if strings.Contains(note, "outside this lease's cards") {
			t.Errorf("display on %s: a display card is not 'outside this lease':\n%s", c.display, note)
		}
	}
}

func TestBothKindsOfBusyCardAreNamedForWhatEachIs(t *testing.T) {
	// Lease on card C; the game on card B (display, 33%); a seat on card A (90%).
	_, note := f38Assess(f38Holder(strings.ToLower(f38CardC)), f38Cards(90, 33, 0, f38CardB))
	for _, want := range []string{"33% on card 1 (RTX bbbb) is the display card", "90% on card 0 (RTX aaaa) is outside this lease's cards"} {
		if !strings.Contains(note, want) {
			t.Errorf("missing %q:\n%s", want, note)
		}
	}
}

func TestABatchBetweenItemsReadsWorkingWhileItsProgressContractMoves(t *testing.T) {
	// The cards are quiet between two pictures. The contract moved 20 s ago inside a 15 minute window.
	h := f38Holder(strings.ToLower(f38CardC))
	h.Progress = &ProgressState{File: "/state/progress/wrap-1-2.jsonl", State: "advancing", AgeSec: 20, StallSec: 900, Detail: "batch job 3/9 starting"}
	verdict, note := f38Assess(h, f38Cards(0, 0, 0, f38CardB))
	if verdict != VerdictHeldWorking {
		t.Fatalf("verdict = %q, want held-working for a batch whose progress file moves\n%s", verdict, note)
	}
	for _, want := range []string{"progress file is advancing", "batch job 3/9 starting", "between two items"} {
		if !strings.Contains(note, want) {
			t.Errorf("missing %q:\n%s", want, note)
		}
	}
	for _, bad := range []string{"NOTHING is running", "stalled", "waiting (a drain"} {
		if strings.Contains(note, bad) {
			t.Errorf("a batch whose contract is fresh must not read %q:\n%s", bad, note)
		}
	}
}

func TestABatchWhoseProgressStoppedReadsStalledNotWorking(t *testing.T) {
	h := f38Holder(strings.ToLower(f38CardC))
	h.Progress = &ProgressState{File: "/state/progress/wrap-1-2.jsonl", State: "stalled", AgeSec: 1800, StallSec: 900}
	h.Stalled = true
	verdict, note := f38Assess(h, f38Cards(0, 0, 0, f38CardB))
	if verdict != VerdictHeldStalled {
		t.Fatalf("verdict = %q, want held-stalled: the contract is the evidence either way\n%s", verdict, note)
	}
}

func TestAHelperLeaseWithNoContractSaysWhyItCannotTellIdleFromStalled(t *testing.T) {
	verdict, note := f38Assess(f38Holder(strings.ToLower(f38CardC)), f38Cards(0, 0, 0, f38CardB))
	if verdict != VerdictHeldIdle {
		t.Fatalf("verdict = %q: with no contract nothing has changed", verdict)
	}
	for _, want := range []string{"it is running comfy-generate.mjs", "quiet between two pictures too", "declared no progress contract"} {
		if !strings.Contains(note, want) {
			t.Errorf("missing %q:\n%s", want, note)
		}
	}
	// A command that is not a render helper gets no such sentence.
	h := f38Holder(strings.ToLower(f38CardC))
	h.Command = "bash bench.sh"
	if _, n := f38Assess(h, f38Cards(0, 0, 0, f38CardB)); strings.Contains(n, "it is running") {
		t.Errorf("a bench is not a render helper:\n%s", n)
	}
	// And a lease that declares a contract (unknown: not written yet) is read by that contract, not by the guess.
	h2 := f38Holder(strings.ToLower(f38CardC))
	h2.Progress = &ProgressState{File: "/state/progress/wrap-1-2.jsonl", State: "unknown", Problem: "does not exist"}
	if _, n := f38Assess(h2, f38Cards(0, 0, 0, f38CardB)); strings.Contains(n, "declared no progress contract") {
		t.Errorf("a lease that declares a contract must not be told it declared none:\n%s", n)
	}
}
