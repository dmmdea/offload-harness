package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

func admissionTestPipeline(t *testing.T, base string, admissionSec int) *Pipeline {
	t.Helper()
	cfg := config.Config{
		Endpoint:              base,
		Model:                 "workhorse",
		AgentModel:            agentTestSeat,
		FleetNodeID:           "node-t",
		Temperature:           0.1,
		AgentAdmissionWaitSec: admissionSec,
	}
	return New(cfg, llamaclient.New(base, "", cfg.Model, 30*time.Second), nil, nil)
}

// Another model is mid-swap on the endpoint: the pre-flight waits OUTSIDE the
// wall (a 1 s wall still succeeds), polls /running until the swap is over, and
// reports the wait on the wire.
func TestRunAgentTaskWaitsForSeatAdmissionOutsideTheWall(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
		running: func(n int64) string {
			if n < 3 {
				return `{"running":[{"model":"other-heavy","state":"starting","cmd":"x"}]}`
			}
			return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"y"}]}`
		},
	}
	srv := fake.server(t)
	defer srv.Close()
	contract := testContract()
	contract.TimeoutSec = 1
	res := admissionTestPipeline(t, srv.URL, 30).Run(context.Background(), agentTestRequest(t, contract))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("the pre-flight must not consume the wall; got: %s", wire.Reason)
	}
	if fake.runningCNT.Load() < 3 {
		t.Fatalf("expected >=3 /running polls while the swap was in progress, got %d", fake.runningCNT.Load())
	}
	if wire.AdmissionWaitSec < 5.9 { // two 3 s polls
		t.Fatalf("admission_wait_sec must report the wait, got %v", wire.AdmissionWaitSec)
	}
}

// Nothing mid-swap and the seat absent: since 0.115.11 the pre-flight WARMS
// the seat (one passthrough GET, then a bounded /running check) instead of
// leaving the cold load to the wall; a seat llama-swap never lists proceeds
// with a note, and the wall is untouched either way.
func TestRunAgentTaskAdmissionIsImmediateWhenNothingIsSwapping(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
		running:   func(int64) string { return `{"running":[]}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	contract := testContract()
	contract.TimeoutSec = 1 // the warm-up must not touch the wall
	res := admissionTestPipeline(t, srv.URL, 30).Run(context.Background(), agentTestRequest(t, contract))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	// 1 admission poll + 1 warm-up check + 2 confirmation polls; the warm GET
	// plus the served-window probe's own GET on the same passthrough route.
	if fake.runningCNT.Load() != 4 || fake.upstreamCNT.Load() != 2 {
		t.Fatalf("polls=%d passthrough GETs=%d, want 4 and 2 (warm-up + window probe)", fake.runningCNT.Load(), fake.upstreamCNT.Load())
	}
	if wire.AdmissionWaitSec < 2.9 || wire.AdmissionWaitSec > 4.5 || !strings.Contains(wire.AdmissionNote, "never listed") {
		t.Fatalf("admission=%v note=%q, want one poll interval and the never-listed note", wire.AdmissionWaitSec, wire.AdmissionNote)
	}
}

// TestRunAgentTaskWarmsAnAbsentSeatOutsideTheWall (0.115.11, D-64): the seat is
// absent, nothing else is swapping; the pre-flight issues the passthrough GET
// (llama-swap loads the seat and answers), /running then lists it ready, the
// time is reported as admission — and a 1 s wall is untouched by it.
func TestRunAgentTaskWarmsAnAbsentSeatOutsideTheWall(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
		running: func(n int64) string {
			if n <= 2 { // the admission poll and the warm-up's own check: absent
				return `{"running":[]}`
			}
			return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"y"}]}`
		},
		upstreamModels: func(n int64) string {
			if n == 1 {
				time.Sleep(1500 * time.Millisecond) // the cold load, longer than the wall; the window probe's later GET is instant
			}
			return `{"object":"list","data":[{"id":"` + agentTestSeat + `","max_model_len":131072}]}`
		},
	}
	srv := fake.server(t)
	defer srv.Close()
	contract := testContract()
	contract.TimeoutSec = 1
	res := admissionTestPipeline(t, srv.URL, 30).Run(context.Background(), agentTestRequest(t, contract))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("the cold load must not consume the wall; got: %s", wire.Reason)
	}
	if fake.upstreamCNT.Load() < 1 {
		t.Fatalf("passthrough GETs = %d, want the warm-up request", fake.upstreamCNT.Load())
	}
	if wire.AdmissionWaitSec < 1.4 || !strings.Contains(wire.AdmissionNote, "cold load") || !strings.Contains(wire.AdmissionNote, "outside the wall") {
		t.Fatalf("admission=%v note=%q, want the cold load reported outside the wall", wire.AdmissionWaitSec, wire.AdmissionNote)
	}
}

// TestRunAgentTaskWarmUpTrustsA200WhenRunningListsAnAlias (0.115.17): the
// passthrough answered 200 (llama-swap proxied, so the upstream is up) but
// /running never lists the seat under the contract's name — an alias. The
// load is reported as confirmed, with no extra poll interval spent.
func TestRunAgentTaskWarmUpTrustsA200WhenRunningListsAnAlias(t *testing.T) {
	fake := &agentFake{
		rosterIDs:      []string{agentTestSeat},
		loop:           func(int64) string { return doneChat("The answer is 42.") },
		repack:         func(int64) string { return `{"answer":"42"}` },
		running:        func(int64) string { return `{"running":[{"model":"the-real-id","state":"ready","cmd":"y"}]}` },
		upstreamModels: func(int64) string { return `{"object":"list","data":[{"id":"` + agentTestSeat + `"}]}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	res := admissionTestPipeline(t, srv.URL, 30).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	if !strings.Contains(wire.AdmissionNote, "outside the wall") || strings.Contains(wire.AdmissionNote, "never listed") || wire.AdmissionWaitSec > 2.5 {
		t.Fatalf("admission=%v note=%q, want the 200 trusted as the confirmation without a poll interval", wire.AdmissionWaitSec, wire.AdmissionNote)
	}
}

// TestRunAgentTaskWarmUpIsBoundedByTheAdmissionBudget: a cold load that
// outlives the budget proceeds into the wall with a note — never blocks.
func TestRunAgentTaskWarmUpIsBoundedByTheAdmissionBudget(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
		running:   func(int64) string { return `{"running":[]}` },
		upstreamModels: func(int64) string {
			time.Sleep(5 * time.Second) // longer than the 3 s admission budget
			return `{"object":"list","data":[]}`
		},
	}
	srv := fake.server(t)
	defer srv.Close()
	res := admissionTestPipeline(t, srv.URL, 3).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("a spent warm-up budget proceeds into the wall; got: %s", wire.Reason)
	}
	if wire.AdmissionWaitSec < 2.9 || wire.AdmissionWaitSec > 4.5 || !strings.Contains(wire.AdmissionNote, "exceeded the admission budget") {
		t.Fatalf("admission=%v note=%q, want the budget named", wire.AdmissionWaitSec, wire.AdmissionNote)
	}
}

// The budget is spent while the swap drags on: proceed into the wall (fail
// open) and say so; never block forever.
func TestRunAgentTaskAdmissionBudgetSpentProceeds(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
		running:   func(int64) string { return `{"running":[{"model":"other-heavy","state":"starting","cmd":"x"}]}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	res := admissionTestPipeline(t, srv.URL, 4).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("a spent admission budget proceeds into the wall; got: %s", wire.Reason)
	}
	if wire.AdmissionWaitSec < 2.9 || wire.AdmissionWaitSec > 4.5 {
		t.Fatalf("admission wait must be bounded by the 4 s budget, got %v", wire.AdmissionWaitSec)
	}
}

// A 404 on /running (an endpoint that is not llama-swap, or an old one) fails
// OPEN: no wait, nothing reported, the contract runs as before.
func TestRunAgentTaskAdmissionProbeFailureProceeds(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	res := admissionTestPipeline(t, srv.URL, 30).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if wire.Deferred || wire.AdmissionWaitSec != 0 {
		t.Fatalf("deferred=%v admission=%v", wire.Deferred, wire.AdmissionWaitSec)
	}
}
