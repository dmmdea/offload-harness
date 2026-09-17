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

// TestCtxWindowNoteNamesTheFallbackWhenTheProbeRanOutOfAdmission: moving the
// served-window probe onto the admission budget (above) bought a new failure
// mode — an admission budget already spent leaves the probe a dead context, and
// the loop then budgets against the 8,192-token conservative fallback on a seat
// that may serve 131,072.
//
// agent.ResolveContextTokens has always returned a line saying exactly which of
// the three windows it picked and why; both doors dropped it on the floor with
// `effCtx, _ :=`. So a run that silently compacted at 8,192 was indistinguishable
// on the wire from a correct one — the same invisibility that let the MCP door
// measure 8,192 cold and 114,688 warm for months. The fallback must be visible
// AS a fallback.
func TestCtxWindowNoteNamesTheFallbackWhenTheProbeRanOutOfAdmission(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		// Another model swaps for the whole budget, so the PRE-FLIGHT spends it
		// exactly — one 3 s sleep against a 3 s budget — and the window probe
		// after it inherits an already-expired deadline. Spending it in the
		// pre-flight rather than the warm-up is deliberate: the warm-up's own
		// confirmation sleep is conditional on what is LEFT of the budget, which
		// makes "did it sleep" a knife-edge at exactly one poll interval.
		running: func(int64) string { return `{"running":[{"model":"other-heavy","state":"starting","cmd":"x"}]}` },
		// The pre-flight's budget here EQUALS one poll interval, so whether it
		// sleeps that interval or returns at once ("budget spent") is a
		// clock-granularity coin flip: on a fast Linux runner it returned at
		// once, the probe had the whole budget left, answered inside it, and
		// the "ran out of admission budget" note correctly did NOT fire (CI on
		// 225d9cf and 2f1c0e0). Stalling the props answer past the ENTIRE
		// admission budget makes the probe's deadline certain either way,
		// which is the premise this test states.
		propsDelay: 4 * time.Second,
		// The fake stalls /props only when it has a props answer to give; the
		// probe asks /props first, so this is the request that outlives the
		// budget.
		props: map[string]any{"default_generation_settings": map[string]any{"n_ctx": 131072}},
		loop:  func(int64) string { return doneChat("The answer is 42.") },
		repack:     func(int64) string { return `{"answer":"42"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	res := admissionTestPipeline(t, srv.URL, 3).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("a spent probe budget falls back, it does not defer: %s", wire.Reason)
	}
	if !strings.Contains(wire.CtxWindowNote, "fallback") {
		t.Fatalf("ctx_window_note = %q, want the conservative fallback named: an 8,192-token run on a 131,072-token seat must be visible as a fallback", wire.CtxWindowNote)
	}
	if !strings.Contains(wire.AdmissionNote, "window probe ran out of admission budget") {
		t.Errorf("admission_note = %q, want the probe's exhausted budget named beside the other admission findings", wire.AdmissionNote)
	}
	if !strings.Contains(wire.AdmissionNote, "budget spent while") {
		t.Errorf("admission_note = %q, want the step that actually spent the budget still named — notes accumulate, they never overwrite", wire.AdmissionNote)
	}
}

// TestCtxWindowNoteNamesTheProbeWhenItAnswers: the note is not a failure
// channel — on the normal path it records which window the run actually
// budgeted against, which is the fact a slow-task diagnosis starts from.
func TestCtxWindowNoteNamesTheProbeWhenItAnswers(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		running:   func(int64) string { return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"y"}]}` },
		props:     map[string]any{"default_generation_settings": map[string]any{"n_ctx": 32768}},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	res := admissionTestPipeline(t, srv.URL, 30).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	if !strings.Contains(wire.CtxWindowNote, "32768") || !strings.Contains(wire.CtxWindowNote, "probed") {
		t.Fatalf("ctx_window_note = %q, want the probed window and its source", wire.CtxWindowNote)
	}
}
