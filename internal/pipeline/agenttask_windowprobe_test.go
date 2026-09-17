// agenttask_windowprobe_test.go pins where the SERVED-WINDOW probe is paid from
// (register S-24 / W-08), and that a warm-up which settles nothing says so.
//
// Two defects, one admission block:
//
//  1. agent.ProbeServedWindow ran on the WALL context. The probe carries a
//     ten-minute cold-start budget on purpose — it is allowed to absorb a seat's
//     load — so on a cold or slow seat it ate the contract's whole wall before the
//     first token, and the run was filed as a wall timeout. Every other pre-token
//     step (the cordon, the swap pre-flight, the cold-load warm-up, the coherence
//     probe) is already paid out of the ADMISSION budget; this one was not.
//  2. warmSeat returned (0, "") when it could not read GET /running at all, so a
//     seat that was still cold left `admission_note` empty and the wire said
//     "nothing was loading" where the truth was "the gate could not tell".

package pipeline

import (
	"strings"
	"testing"
	"time"

	"context"
)

// TestWindowProbeIsPaidFromAdmissionNotFromTheWall: /running answers 500 (so the
// warm-up settles nothing) and the served-window probe blocks for two seconds on
// /props. The contract's wall is three seconds. If the probe is charged to the
// wall the loop never gets to speak; charged to admission, the wall starts whole
// and the one-second chat step lands inside it.
func TestWindowProbeIsPaidFromAdmissionNotFromTheWall(t *testing.T) {
	fake := &agentFake{
		rosterIDs:     []string{agentTestSeat},
		runningStatus: 500, // llama-swap is up but answering nothing useful
		props:         map[string]any{"default_generation_settings": map[string]any{"n_ctx": 32768}},
		propsDelay:    2 * time.Second,
		loop: func(int64) string {
			time.Sleep(time.Second) // the loop's one real step, inside the wall
			return doneChat("The answer is 42.")
		},
		repack: func(int64) string { return `{"answer":"42"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	contract := testContract()
	contract.TimeoutSec = 3 // the wall the probe must not spend
	res := admissionTestPipeline(t, srv.URL, 30).Run(context.Background(), agentTestRequest(t, contract))
	wire := decodeWire(t, res)

	if wire.Deferred {
		t.Fatalf("the window probe must not consume the contract's wall; the run deferred: %s (%s)", wire.Reason, wire.DeferClass)
	}
	if wire.AdmissionWaitSec < 1.9 {
		t.Errorf("admission_wait_sec = %v, want the probe's own ~2 s reported as admission", wire.AdmissionWaitSec)
	}
	if !strings.Contains(wire.AdmissionNote, "warm-up") {
		t.Errorf("admission_note = %q, want the failed warm-up named: a still-cold seat must be visible on the wire", wire.AdmissionNote)
	}
}

// TestWarmSeatSpeaksWhenItCannotReadRunning: the narrow unit. An endpoint whose
// GET /running fails leaves the seat's residency UNKNOWN, which is not the same
// fact as "the seat is ready" — and it is the one the wire used to swallow.
func TestWarmSeatSpeaksWhenItCannotReadRunning(t *testing.T) {
	fake := &agentFake{rosterIDs: []string{agentTestSeat}, runningStatus: 500}
	srv := fake.server(t)
	defer srv.Close()

	spent, note, attempted := warmSeat(context.Background(), srv.URL, agentTestSeat, 30*time.Second)
	if note == "" {
		t.Fatalf("warm-up spent=%v note=%q, want a note naming the failed residency read", spent, note)
	}
	if !strings.Contains(note, "warm-up") {
		t.Errorf("note = %q, want the warm-up named so admission_note reads as a warm-up finding", note)
	}
	if attempted {
		t.Errorf("no load was attempted, so the coherence probe must not be told one was")
	}
}

// TestWarmSeatSpeaksWhenTheBudgetIsAlreadySpent: the other silent exit — a
// budget cut short by everything admission already spent. Proceeding into the
// wall on a possibly-cold seat is correct; doing it silently is not.
func TestWarmSeatSpeaksWhenTheBudgetIsAlreadySpent(t *testing.T) {
	fake := &agentFake{rosterIDs: []string{agentTestSeat}, running: func(int64) string { return `{"running":[]}` }}
	srv := fake.server(t)
	defer srv.Close()

	spent, note, attempted := warmSeat(context.Background(), srv.URL, agentTestSeat, admissionPoll/3)
	if spent != 0 {
		t.Errorf("spent = %v, want 0: there was no budget to spend", spent)
	}
	if attempted {
		t.Errorf("no load was attempted, so the coherence probe must not be told one was")
	}
	if !strings.Contains(note, "admission budget") {
		t.Fatalf("note = %q, want the spent budget named", note)
	}
}

// TestWarmSeatReportsAnAttemptedLoadEvenWhenItMeasuresZero: the fact the D-118
// coherence probe keys on is "was this seat loaded FOR THIS RUN", and neither of
// the other two return values can carry it. A sub-tick load measures 0 — the
// shape a fast box produces routinely — so deriving it from the duration
// un-fires the probe at random; deriving it from the note over-fires it on the
// exits above, which warmed nothing.
func TestWarmSeatReportsAnAttemptedLoadEvenWhenItMeasuresZero(t *testing.T) {
	fake := &agentFake{
		rosterIDs:      []string{agentTestSeat},
		running:        func(n int64) string { return `{"running":[]}` },
		upstreamModels: func(int64) string { return `{"object":"list","data":[{"id":"` + agentTestSeat + `"}]}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	_, note, attempted := warmSeat(context.Background(), srv.URL, agentTestSeat, 30*time.Second)
	if !attempted {
		t.Fatalf("the passthrough GET was issued (note %q) — that IS the load, however long it took", note)
	}
}
