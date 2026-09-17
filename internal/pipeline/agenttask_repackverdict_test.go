package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestRepackStructuredEarlierCutoffsAreNotesNotTheVerdict (register D-108,
// PR #366 correctness review, rule a+b): grammar attempts 1 and 2 are both
// cut by THEIR OWN per-attempt bound (a self-imposed timeout, indistinguishable
// on the wire from a real hang), but the chat fallback — the re-pack's LAST,
// DECISIVE attempt — answers fast and simply fails validation. The verdict
// must be the validation failure, not a transport/cutoff signal borrowed from
// an earlier attempt, and the earlier cutoffs must still be readable as notes
// in the error text (an operator's evidence, never the published class).
func TestRepackStructuredEarlierCutoffsAreNotesNotTheVerdict(t *testing.T) {
	fake := &agentFake{
		rosterIDs:   []string{agentTestSeat},
		repack:      func(int64) string { return `{"wrong":"shape"}` }, // never read: both grammar attempts are cut before a body arrives
		repackDelay: 2500 * time.Millisecond,                           // past each grammar attempt's own ~2s share of the wall
		chatFallback: func(int64) string {
			return doneChat(`{"wrong":"shape"}`) // answers FAST, fails validation
		},
	}
	srv := fake.server(t)
	defer srv.Close()

	p := agentTestPipeline(t, srv.URL)
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	_, _, transport, attempts, err := p.repackStructured(ctx, agentTestSeat, schema, "The answer is 42.", 0)

	if ctx.Err() != nil {
		t.Fatalf("ctx.Err() = %v, want the wall NOT exhausted — this test is about the LAST attempt's own nature, not a wall timeout", ctx.Err())
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (2 self-cut grammar attempts + the chat fallback)", attempts)
	}
	if transport {
		t.Fatalf("transport = true, want false — the DECISIVE (chat) attempt answered and failed validation, it was never unreachable")
	}
	if err == nil {
		t.Fatal("err = nil, want the chat fallback's validation failure")
	}
	if _, isCutoff := asRepackCutoff(err); isCutoff {
		t.Fatalf("err classified as a self-cutoff (%q), want the LAST attempt's own validation failure — the earlier cut grammar attempts must not decide the verdict", err.Error())
	}
	if !strings.Contains(err.Error(), "missing property 'answer'") && !strings.Contains(err.Error(), "required") {
		t.Fatalf("err = %q, want the chat fallback's own validation error named", err.Error())
	}
	if !strings.Contains(err.Error(), "attempt 1/3") || !strings.Contains(err.Error(), "attempt 2/3") {
		t.Fatalf("err = %q, want BOTH earlier cut attempts named as notes", err.Error())
	}
	if got := fake.grammarCNT.Load(); got != 2 {
		t.Fatalf("grammar attempts = %d, want 2", got)
	}
	if got := fake.chatFallbackCNT.Load(); got != 1 {
		t.Fatalf("chat-fallback attempts = %d, want 1", got)
	}
}

// TestRepackStructuredLastAttemptCutoffIsBudgetNotInfrastructure (register
// D-108, PR #366 correctness review, rule a+c): the re-pack's LAST attempt —
// the chat fallback — is cut by ITS OWN per-attempt bound while the wall
// still has room to spare. This must never read as "the seat could not be
// reached": a client.Timeout firing on a request THIS BOX deliberately
// narrowed is not evidence of a broken box, the exact defect this PR already
// fixes for a canceled parent context (S-22/W-16), recreated one arm over by
// the per-attempt bound itself. The two grammar attempts fail fast on
// validation (not a cutoff), so the ONLY cutoff in this run is the decisive
// one.
func TestRepackStructuredLastAttemptCutoffIsBudgetNotInfrastructure(t *testing.T) {
	fake := &agentFake{
		rosterIDs:         []string{agentTestSeat},
		repack:            func(int64) string { return `{"wrong":"shape"}` }, // fast validation failures, no delay
		chatFallback:      func(int64) string { return doneChat(`{"answer":"too late"}`) },
		chatFallbackDelay: 3500 * time.Millisecond, // past the chat lane's own narrowed bound
	}
	srv := fake.server(t)
	defer srv.Close()

	p := agentTestPipeline(t, srv.URL)
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, transport, attempts, err := p.repackStructured(ctx, agentTestSeat, schema, "The answer is 42.", 0)

	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (2 fast grammar validation failures + the cut chat fallback)", attempts)
	}
	if transport {
		t.Fatalf("transport = true, want false — a self-imposed cutoff is never the seat being unreachable")
	}
	cutoff, isCutoff := asRepackCutoff(err)
	if !isCutoff {
		t.Fatalf("err = %v, want a *repackCutoffErr — the LAST attempt was cut by its own bound", err)
	}
	if !strings.Contains(cutoff.Error(), "attempt 3/3") || !strings.Contains(cutoff.Error(), "share of the wall") {
		t.Fatalf("cutoff message = %q, want the decisive attempt and its own bound named", cutoff.Error())
	}
	if got := fake.grammarCNT.Load(); got != 2 {
		t.Fatalf("grammar attempts = %d, want 2", got)
	}
	if got := fake.chatFallbackCNT.Load(); got != 1 {
		t.Fatalf("chat-fallback attempts = %d, want 1", got)
	}
}
