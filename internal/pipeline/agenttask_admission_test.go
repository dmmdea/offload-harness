package pipeline

import (
	"context"
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

// Nothing mid-swap and the seat absent: on-demand load is the wall's business.
// The pre-flight returns at once and reports nothing.
func TestRunAgentTaskAdmissionIsImmediateWhenNothingIsSwapping(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
		running:   func(int64) string { return `{"running":[]}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	res := admissionTestPipeline(t, srv.URL, 30).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if wire.Deferred || fake.runningCNT.Load() != 1 || wire.AdmissionWaitSec != 0 {
		t.Fatalf("deferred=%v polls=%d admission=%v", wire.Deferred, fake.runningCNT.Load(), wire.AdmissionWaitSec)
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
