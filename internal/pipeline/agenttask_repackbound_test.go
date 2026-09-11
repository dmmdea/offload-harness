package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// lengthChat is an assistant turn cut at max_tokens (finish_reason "length").
func lengthChat(content string) string {
	b, _ := json.Marshal(content)
	return `{"choices":[{"message":{"role":"assistant","content":` + string(b) + `},"finish_reason":"length"}]}`
}

// TestRunAgentTaskTruncatedAnswerSkipsTheRepack (0.115.23, register D-91):
// a final answer cut at the completion budget — twice, so the loop accepts it
// as OutputTruncated — is a partial no re-pack can complete. The run must
// name that at once (abstention, "output_truncated" in the reason, the
// partial still in output) and spend ZERO seat completions on re-packing:
// the 2026-09-10 Lenovo run spent ~690 s of re-generation on exactly this
// shape and deferred "wall timeout" on a loop that was done in four minutes.
func TestRunAgentTaskTruncatedAnswerSkipsTheRepack(t *testing.T) {
	partial := strings.Repeat("The ledger shows pass 4 measured ", 40)
	fake := &agentFake{
		rosterIDs:    []string{agentTestSeat},
		loop:         func(int64) string { return lengthChat(partial) },
		repack:       func(int64) string { return `{"answer":"never asked"}` },
		chatFallback: func(int64) string { return doneChat(`{"answer":"never asked"}`) },
	}
	srv := fake.server(t)
	defer srv.Close()
	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if !wire.Deferred || wire.DeferClass != "abstention" || !strings.Contains(wire.Reason, "output_truncated") {
		t.Fatalf("deferred=%v class=%q reason=%q, want an abstention naming output_truncated", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if !wire.OutputTruncated || !strings.Contains(wire.Output, "pass 4 measured") {
		t.Fatalf("output_truncated=%v output=%q, want the partial flagged and kept for the caller", wire.OutputTruncated, wire.Output[:40])
	}
	if got := fake.grammarCNT.Load() + fake.chatFallbackCNT.Load(); got != 0 {
		t.Fatalf("re-pack completions = %d, want 0 — a partial is not re-packed", got)
	}
	if !strings.Contains(wire.RepackNote, "re-pack skipped") {
		t.Fatalf("repack_note = %q, want the skip named on the wire", wire.RepackNote)
	}
}

// TestRepackStopsAttemptsTheWallCannotHold (D-91): with the wall nearly spent
// the re-pack does not start another full re-generation — the error names
// the wall, and the attempts count says how many ran. With no deadline every
// lane runs, as before.
func TestRepackStopsAttemptsTheWallCannotHold(t *testing.T) {
	fake := &agentFake{
		rosterIDs:    []string{agentTestSeat},
		repack:       func(int64) string { return `not json at all` },
		chatFallback: func(int64) string { return doneChat(`still not json`) },
	}
	srv := fake.server(t)
	defer srv.Close()
	p := agentTestPipeline(t, srv.URL)
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`)
	// A deadline under the attempt floor: no attempt may start.
	ctx, cancel := context.WithTimeout(context.Background(), agentRepackAttemptFloor/2)
	defer cancel()
	_, _, _, attempts, err := p.repackStructured(ctx, agentTestSeat, schema, "The answer is 42.", agentRepackAttemptFloor)
	if err == nil || !strings.Contains(err.Error(), "of the wall left") || attempts != 0 {
		t.Fatalf("attempts=%d err=%v, want 0 attempts and the wall named", attempts, err)
	}
	if got := fake.grammarCNT.Load() + fake.chatFallbackCNT.Load(); got != 0 {
		t.Fatalf("seat completions = %d, want 0 under a spent wall", got)
	}
	// No deadline: both grammar attempts and the chat lane run and fail honestly.
	_, _, _, attempts, err = p.repackStructured(context.Background(), agentTestSeat, schema, "The answer is 42.", agentRepackAttemptFloor)
	if err == nil || attempts != 3 {
		t.Fatalf("attempts=%d err=%v, want all 3 lanes tried without a deadline", attempts, err)
	}
	if fake.grammarCNT.Load() != 2 || fake.chatFallbackCNT.Load() != 1 {
		t.Fatalf("grammar=%d chat=%d, want 2 + 1", fake.grammarCNT.Load(), fake.chatFallbackCNT.Load())
	}
	// A deadline that holds one attempt but not two: the floor check runs
	// BEFORE each attempt, so what starts is decided by the wall, not by a
	// fixed count. Give the fake a delay so the first attempt eats the wall.
	fake.repackDelay = agentRepackAttemptFloor / 4
	fake.grammarCNT.Store(0)
	fake.chatFallbackCNT.Store(0)
	ctx2, cancel2 := context.WithTimeout(context.Background(), agentRepackAttemptFloor+agentRepackAttemptFloor/8)
	defer cancel2()
	_, _, _, attempts, err = p.repackStructured(ctx2, agentTestSeat, schema, "The answer is 42.", agentRepackAttemptFloor)
	if attempts != 1 || err == nil || !strings.Contains(err.Error(), "of the wall left") {
		t.Fatalf("attempts=%d err=%v, want exactly the one attempt the wall could hold", attempts, err)
	}
	// The floor scales with the contract: a tenth of the wall, capped.
	if repackAttemptFloor(900*time.Second) != agentRepackAttemptFloor || repackAttemptFloor(300*time.Second) != 30*time.Second || repackAttemptFloor(30*time.Second) != 3*time.Second {
		t.Fatalf("repackAttemptFloor: 900 s → %v, 300 s → %v, 30 s → %v", repackAttemptFloor(900*time.Second), repackAttemptFloor(300*time.Second), repackAttemptFloor(30*time.Second))
	}
}
