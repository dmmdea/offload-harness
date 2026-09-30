package pipeline

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// The node flags a finished answer whose structuring failed (register C-66,
// PR-4), on every arm of the re-pack's failure switch, and the delegator's
// recognizer reads exactly what the node writes: the two are pinned here
// against each other, so neither can drift from the other's reason prefix.
func TestRunAgentTaskRepackFailureFlagsSchemaMiss(t *testing.T) {
	// The seat answers the loop, then refuses every re-pack request with a 500.
	t.Run("unreachable", func(t *testing.T) {
		fake := &agentFake{
			rosterIDs:          []string{agentTestSeat},
			loop:               func(int64) string { return doneChat("The answer is 42.") },
			repackStatus:       http.StatusInternalServerError,
			chatFallbackStatus: http.StatusInternalServerError,
		}
		srv := fake.server(t)
		defer srv.Close()
		wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
		assertSchemaMiss(t, wire, core.RepackFailedReason)
	})
	// The seat answers every re-pack in the wrong shape.
	t.Run("schema failure", func(t *testing.T) {
		fake := &agentFake{
			rosterIDs:    []string{agentTestSeat},
			loop:         func(int64) string { return doneChat("The answer is 42.") },
			repack:       func(int64) string { return `{"wrong":"shape"}` },
			chatFallback: func(int64) string { return doneChat(`{"wrong":"shape"}`) },
		}
		srv := fake.server(t)
		defer srv.Close()
		wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
		assertSchemaMiss(t, wire, core.SchemaFailedReason)
		if wire.DeferClass != core.DeferClassAbstention {
			t.Fatalf("defer_class = %q, want the abstention it always was", wire.DeferClass)
		}
	})
	// The re-pack goes silent past its allowance.
	t.Run("stall", func(t *testing.T) {
		defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
		defer compressRepackBound(t, 300*time.Millisecond)()
		fake := &agentFake{
			rosterIDs:   []string{agentTestSeat},
			loop:        func(int64) string { return doneChat("The answer is 42.") },
			repack:      func(int64) string { return `{"answer":"42"}` },
			repackDelay: 2 * time.Second,
		}
		srv := fake.server(t)
		defer srv.Close()
		wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
		assertSchemaMiss(t, wire, core.RepackFailedReason)
	})
}

func assertSchemaMiss(t *testing.T, wire core.AgentWireResult, prefix string) {
	t.Helper()
	if !wire.Deferred || !wire.SchemaMiss {
		t.Fatalf("deferred=%v schema_miss=%v reason=%q, want a flagged defer", wire.Deferred, wire.SchemaMiss, wire.Reason)
	}
	if !strings.HasPrefix(wire.Reason, prefix) {
		t.Fatalf("reason = %q, want the prefix %q the delegator's recognizer keys on", wire.Reason, prefix)
	}
	if wire.Output == "" || wire.StopReason != "done" || len(wire.Structured) != 0 {
		t.Fatalf("output=%q stop=%q structured=%s, want the finished answer intact and no object", wire.Output, wire.StopReason, wire.Structured)
	}
	if !delegate.SchemaMissRescuable(wire) {
		t.Fatal("the delegator would not rescue what the node just flagged")
	}
	// The legacy reading (a node with no flag) agrees on what the node writes.
	legacy := wire
	legacy.SchemaMiss = false
	if !delegate.SchemaMissRescuable(legacy) {
		t.Fatalf("the legacy recognizer does not read the node's own reason %q", wire.Reason)
	}
}

// The flag is for a finished answer and nothing else: not a success, not a cut
// answer, not a loop that failed.
func TestRunAgentTaskSchemaMissIsOnlyForAFinishedAnswer(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		fake := &agentFake{rosterIDs: []string{agentTestSeat}, loop: func(int64) string { return doneChat("The answer is 42.") }, repack: func(int64) string { return `{"answer":"42"}` }}
		srv := fake.server(t)
		defer srv.Close()
		if wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))); wire.SchemaMiss || wire.Deferred {
			t.Fatalf("a delivered result carries schema_miss=%v deferred=%v", wire.SchemaMiss, wire.Deferred)
		}
	})
	t.Run("cut answer", func(t *testing.T) {
		fake := &agentFake{
			rosterIDs: []string{agentTestSeat},
			loop:      func(int64) string { return lengthChat(strings.Repeat("The ledger shows pass 4 measured ", 40)) },
			repack:    func(int64) string { return `{"answer":"never asked"}` },
		}
		srv := fake.server(t)
		defer srv.Close()
		wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
		if !wire.Deferred || !wire.OutputTruncated || wire.SchemaMiss {
			t.Fatalf("deferred=%v truncated=%v schema_miss=%v: a partial can never be re-packed into the whole object", wire.Deferred, wire.OutputTruncated, wire.SchemaMiss)
		}
	})
	t.Run("loop failed", func(t *testing.T) {
		fake := &agentFake{
			rosterIDs:  []string{agentTestSeat},
			loop:       func(int64) string { return `{"error":"boom"}` },
			loopStatus: func(int64) int { return http.StatusInternalServerError },
			repack:     func(int64) string { return `{"answer":"42"}` },
		}
		srv := fake.server(t)
		defer srv.Close()
		wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
		if !wire.Deferred || wire.SchemaMiss {
			t.Fatalf("deferred=%v schema_miss=%v: nothing finished, so nothing to rescue", wire.Deferred, wire.SchemaMiss)
		}
	})
}

// RescueRepack: the lossless reading first, then ONE completion on this box's
// own seat, validated against the contract's schema.
func TestRescueRepackDeliversAValidatedObjectInOneCompletion(t *testing.T) {
	fake := &agentFake{
		rosterIDs:    []string{agentTestSeat},
		repack:       func(int64) string { return `{"answer":"42"}` },
		chatFallback: func(int64) string { return doneChat(`{"answer":"never"}`) },
	}
	srv := fake.server(t)
	defer srv.Close()

	got, err := agentTestPipeline(t, srv.URL).RescueRepack(context.Background(), testContract(), "The answer is 42.", 0)
	if err != nil {
		t.Fatalf("RescueRepack: %v", err)
	}
	if string(got.Structured) != `{"answer":"42"}` || got.Seat != agentTestSeat || got.TokensOut != 7 || got.How == "" {
		t.Fatalf("rescued = %+v", got)
	}
	if fake.grammarCNT.Load() != 1 || fake.chatFallbackCNT.Load() != 0 {
		t.Fatalf("seat saw %d grammar + %d chat requests, want exactly one completion", fake.grammarCNT.Load(), fake.chatFallbackCNT.Load())
	}
}

func TestRescueRepackTakesTheLosslessReadingFirst(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		repack:    func(int64) string { t.Error("a completion ran for an answer that is already the object"); return `{}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	got, err := agentTestPipeline(t, srv.URL).RescueRepack(context.Background(), testContract(), "Here it is: {\"answer\": \"42\"}", 0)
	if err != nil || string(got.Structured) != `{"answer": "42"}` || !strings.Contains(got.How, "already the object") {
		t.Fatalf("rescued = %+v err = %v", got, err)
	}
	if fake.grammarCNT.Load() != 0 {
		t.Fatalf("seat saw %d completions, want none", fake.grammarCNT.Load())
	}
}

// One completion, not the re-pack's three attempts: a rescue that gets a shape
// the schema refuses fails at once, without a retry or the chat lane.
func TestRescueRepackSpendsOneCompletionAndFailsHonestly(t *testing.T) {
	fake := &agentFake{
		rosterIDs:    []string{agentTestSeat},
		repack:       func(int64) string { return `{"wrong":"shape"}` },
		chatFallback: func(int64) string { return doneChat(`{"answer":"42"}`) },
	}
	srv := fake.server(t)
	defer srv.Close()
	_, err := agentTestPipeline(t, srv.URL).RescueRepack(context.Background(), testContract(), "The answer is 42.", 0)
	if err == nil {
		t.Fatal("a wrong-shape completion was delivered")
	}
	if fake.grammarCNT.Load() != 1 || fake.chatFallbackCNT.Load() != 0 {
		t.Fatalf("seat saw %d grammar + %d chat requests, want exactly one", fake.grammarCNT.Load(), fake.chatFallbackCNT.Load())
	}
}

// The wait is bounded by the budget the delegator gives it.
func TestRescueRepackIsBoundedByItsBudget(t *testing.T) {
	fake := &agentFake{
		rosterIDs:   []string{agentTestSeat},
		repack:      func(int64) string { return `{"answer":"42"}` },
		repackDelay: 3 * time.Second,
	}
	srv := fake.server(t)
	defer srv.Close()
	start := time.Now()
	_, err := agentTestPipeline(t, srv.URL).RescueRepack(context.Background(), testContract(), "The answer is 42.", 300*time.Millisecond)
	if err == nil {
		t.Fatal("a completion that outlived the budget was delivered")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("the rescue waited %s for a 300 ms budget", el)
	}
}

// A busy seat is a place in line, not a refusal: llama-swap's 429 (peers hold
// its slots) is waited out on the busy-seat budget and the rescue delivers when
// its turn comes. With no budget on the context the first 429 would stand.
func TestRescueRepackWaitsInLineOnABusySeat(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		repack:    func(int64) string { return `{"answer":"42"}` },
		repackStatusFor: func(n int64) int {
			if n == 1 {
				return http.StatusTooManyRequests
			}
			return 0
		},
	}
	srv := fake.server(t)
	defer srv.Close()
	got, err := agentTestPipeline(t, srv.URL).RescueRepack(context.Background(), testContract(), "The answer is 42.", 0)
	if err != nil || string(got.Structured) != `{"answer":"42"}` {
		t.Fatalf("rescued = %+v err = %v, want the object delivered after one busy answer", got, err)
	}
	if fake.grammarCNT.Load() != 2 {
		t.Fatalf("grammar requests = %d, want the refused one and the one that got its turn", fake.grammarCNT.Load())
	}
}

// The rescue is one seat request, not a run: with the box's one slot held by
// another registered run it does not queue for a slot (register C-42/C-60).
func TestRescueRepackTakesNoRunSlot(t *testing.T) {
	p, _, _, fake := seatCapFixture(t, 1)
	done := make(chan error, 1)
	go func() {
		_, err := p.RescueRepack(context.Background(), testContract(), "The answer is 42.", 0)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RescueRepack: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the rescue waited for a run slot it must not take")
	}
	if fake.grammarCNT.Load() != 1 {
		t.Fatalf("grammar requests = %d, want the one completion", fake.grammarCNT.Load())
	}
}

// The rescue holds the object to the schema the node's answer would have been
// held to: a field the acceptance reads is required of it too.
func TestRescueRepackRequiresTheFieldsTheAcceptanceReads(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		repack:    func(int64) string { return `{"verdict":"ok"}` }, // no key_facts
	}
	srv := fake.server(t)
	defer srv.Close()
	c := requiredFieldContract()
	if _, err := agentTestPipeline(t, srv.URL).RescueRepack(context.Background(), c, "The page covers pinned staging memory.", 0); err == nil {
		t.Fatal("an object missing the field the acceptance reads was delivered")
	}
}
