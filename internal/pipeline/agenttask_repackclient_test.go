package pipeline

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// agentTestPipelineWithLanes is agentTestPipeline plus a configured
// cascade_remote_lanes entry for agentTestSeat — the config shape that arms
// repackClient's lane-probe closures (llamaclient.LocalSwapBusy /
// FleetLaneGates), so a test can count how many times they actually hit the
// wire. The lane base is never dialled in these tests: the local roster
// carries no OTHER busy occupant, so laneWhyBusy always answers "" and the
// lane itself is never taken — only the LOCAL probe (/v1/models + /running)
// that decides that runs.
func agentTestPipelineWithLanes(t *testing.T, base string) *Pipeline {
	t.Helper()
	cfg := config.Config{
		Endpoint:           base,
		Model:              "workhorse",
		AgentModel:         agentTestSeat,
		FleetNodeID:        "node-t",
		Temperature:        0.1,
		CascadeRemoteLanes: map[string]string{agentTestSeat: "http://192.0.2.10:11434"},
	}
	return New(cfg, llamaclient.New(base, "", cfg.Model, 30*time.Second), nil, nil)
}

// TestRepackStructuredHoistsOneClientAcrossAllAttempts (S-21/W-19, register
// D-85/D-108): repackClient used to build a FRESH FleetLaneGates cache and a
// fresh LocalSwapBusy closure on every one of up to three attempts (two
// grammar completions + the chat fallback, each via its own repackClient
// call), so every attempt re-paid a live /v1/models + /running + per-model
// gauge read of the LOCAL seat before spending a token — measured fleet-wide
// at 1,301 rows, median 18 s, max 581 s, 15.13 h total, 244 rows at all three
// attempts. The probe closures now live for the whole repackStructured call
// and are threaded into every client it builds, so their own 5 s TTL cache
// (llamaclient.laneBusyTTL) does what it was always meant to: one probe per
// window, not one per attempt.
func TestRepackStructuredHoistsOneClientAcrossAllAttempts(t *testing.T) {
	fake := &agentFake{
		rosterIDs:    []string{agentTestSeat},
		running:      func(int64) string { return `{"running":[]}` },
		repack:       func(int64) string { return `not json at all` },          // both grammar attempts fail validation
		chatFallback: func(int64) string { return doneChat(`still not json`) }, // the chat fallback fails too
	}
	srv := fake.server(t)
	defer srv.Close()

	p := agentTestPipelineWithLanes(t, srv.URL)
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`)
	_, _, _, attempts, err := p.repackStructured(context.Background(), agentTestSeat, schema, "The answer is 42.", 0)
	if err == nil || attempts != 3 {
		t.Fatalf("attempts=%d err=%v, want all 3 lanes tried and a final error", attempts, err)
	}
	if got := fake.grammarCNT.Load(); got != 2 {
		t.Fatalf("grammar attempts = %d, want 2", got)
	}
	if got := fake.chatFallbackCNT.Load(); got != 1 {
		t.Fatalf("chat-fallback attempts = %d, want 1", got)
	}
	if got := fake.runningCNT.Load(); got != 1 {
		t.Fatalf("/running hits across the re-pack = %d, want exactly 1 — the lane-probe cache must survive every attempt, not rebuild per attempt", got)
	}
}

// TestRepackAttemptDeadlineSplitsRemainingWall (W-19): one re-pack attempt is
// bounded by the SMALLER of its own seat allowance (repackTimeout) and an
// even split of what is left of the wall across the attempts still owed a
// turn — so a nearly spent wall can no longer let one attempt consume what
// the later attempts need. No deadline on ctx leaves the seat allowance
// alone, and a generous wall never narrows below it either.
func TestRepackAttemptDeadlineSplitsRemainingWall(t *testing.T) {
	cfg := config.Config{} // repackTimeout floors at agentRepackChatTimeout (120s)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if d := repackAttemptDeadline(ctx, cfg, agentRepackMaxTokens, 3); d > 10*time.Second {
		t.Fatalf("first-of-3 attempt deadline = %v, want <= 10s (30s wall / 3 attempts left)", d)
	}

	if got, want := repackAttemptDeadline(context.Background(), cfg, agentRepackMaxTokens, 3), repackTimeout(cfg, agentRepackMaxTokens); got != want {
		t.Fatalf("no-deadline attempt = %v, want the plain repackTimeout %v", got, want)
	}

	longCtx, cancel2 := context.WithTimeout(context.Background(), time.Hour)
	defer cancel2()
	if got, want := repackAttemptDeadline(longCtx, cfg, agentRepackMaxTokens, 1), repackTimeout(cfg, agentRepackMaxTokens); got != want {
		t.Fatalf("attempt deadline = %v, want the unnarrowed seat allowance %v — a generous wall must never widen past it", got, want)
	}
}
