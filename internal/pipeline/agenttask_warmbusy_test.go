package pipeline

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// warmFailTestPipeline is the admission test pipeline with enforcement on
// (agent_warm_failure_defer): the tests below guard what enforcement must NOT
// refuse, and what it must.
func warmFailTestPipeline(t *testing.T, base string) *Pipeline {
	t.Helper()
	cfg := config.Config{
		Endpoint:              base,
		Model:                 "workhorse",
		AgentModel:            agentTestSeat,
		FleetNodeID:           "node-t",
		Temperature:           0.1,
		AgentAdmissionWaitSec: 30,
		AgentWarmFailureDefer: true,
	}
	return New(cfg, llamaclient.New(base, "", cfg.Model, 30*time.Second), nil, nil)
}

// warmLoadingRunning scripts /running around a refused warm-up. The admission
// pre-flight and the warm-up's own first read (polls 1 and 2) see nothing loaded;
// the two confirmation polls after the refusal (3 and 4) see loading; the seat
// reads ready afterwards.
func warmLoadingRunning(loading string) func(n int64) string {
	return func(n int64) string {
		switch {
		case n <= 2:
			return `{"running":[]}`
		case n <= 4:
			return loading
		}
		return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"y"}]}`
	}
}

// warmRefusedOnce answers only the FIRST passthrough GET (the warm request) with
// status and body; the served-window probe's later GET falls through to the
// fake's default.
func warmRefusedOnce(status int, body string) (func(n int64) int, func(n int64) string) {
	return func(n int64) int {
			if n == 1 {
				return status
			}
			return 0
		}, func(int64) string {
			return body
		}
}

// llama-swap answers the warm request with a 5xx while PEERS hold the seat or it
// is loading the seat for them: 503 "process is not ready", a 500 of its own, a
// health check that timed out, an empty 502 (seatwait.Retryable names them). That
// is a place in line, never a refusal (INV-4): the seat reads `starting`, so
// admission proceeds and the wall's cold-load hold owns the load. Deferring here
// is terminal and counts as lost work, for a contract that would only have queued.
func TestRunAgentTaskWarmUpBusyAnswersProceedWhileTheSeatLoads(t *testing.T) {
	loading := `{"running":[{"model":"` + agentTestSeat + `","state":"starting","cmd":"y"}]}`
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"503 process is not ready", http.StatusServiceUnavailable, "process is not ready"},
		{"500 from llama-swap itself", http.StatusInternalServerError, `{"error":"swap failed","src":"llama-swap"}`},
		{"500 health check timed out", http.StatusInternalServerError, "health check timed out after 120s"},
		{"502 with no body", http.StatusBadGateway, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, body := warmRefusedOnce(tc.status, tc.body)
			fake := &agentFake{
				rosterIDs:      []string{agentTestSeat},
				loop:           func(int64) string { return doneChat("The answer is 42.") },
				repack:         func(int64) string { return `{"answer":"42"}` },
				running:        warmLoadingRunning(loading),
				upstreamStatus: status,
				upstreamBody:   body,
			}
			srv := fake.server(t)
			defer srv.Close()
			wire := decodeWire(t, warmFailTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
			if wire.Deferred || fake.loopCalls.Load() == 0 {
				t.Fatalf("deferred=%v loop requests=%d class=%q reason=%q: a warm-up refused while the seat loads is a place in line, not a refusal",
					wire.Deferred, fake.loopCalls.Load(), wire.DeferClass, wire.Reason)
			}
			if !strings.Contains(wire.AdmissionNote, "HTTP "+strconv.Itoa(tc.status)) || !strings.Contains(wire.AdmissionNote, "proceeding: ") {
				t.Fatalf("admission_note = %q, want the refusal and why the run proceeds reported on the wire", wire.AdmissionNote)
			}
		})
	}
}

// The seat's OWN row reading starting is a load in flight whatever words the
// refusal used (here the words of a start that died: an attempt died and another
// is on its way), and the wall's cold-load hold owns it.
func TestRunAgentTaskWarmUp5xxWhileTheSeatsOwnRowReadsStartingProceeds(t *testing.T) {
	status, body := warmRefusedOnce(http.StatusInternalServerError, "upstream command exited prematurely")
	fake := &agentFake{
		rosterIDs:      []string{agentTestSeat},
		loop:           func(int64) string { return doneChat("The answer is 42.") },
		repack:         func(int64) string { return `{"answer":"42"}` },
		running:        warmLoadingRunning(`{"running":[{"model":"` + agentTestSeat + `","state":"starting","cmd":"y"}]}`),
		upstreamStatus: status,
		upstreamBody:   body,
	}
	srv := fake.server(t)
	defer srv.Close()
	wire := decodeWire(t, warmFailTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred || fake.loopCalls.Load() == 0 {
		t.Fatalf("deferred=%v loop requests=%d reason=%q, want a run: the seat's own row read starting", wire.Deferred, fake.loopCalls.Load(), wire.Reason)
	}
}

// A 5xx while ANOTHER model is mid-swap is not evidence about this seat either:
// the request may have been aborted by a newer one, or the seat queued behind that
// swap. The wall's own waiting owns it.
func TestRunAgentTaskWarmUp5xxWhileAnotherModelSwapsProceeds(t *testing.T) {
	status, body := warmRefusedOnce(http.StatusInternalServerError, "start aborted")
	fake := &agentFake{
		rosterIDs:      []string{agentTestSeat},
		loop:           func(int64) string { return doneChat("The answer is 42.") },
		repack:         func(int64) string { return `{"answer":"42"}` },
		running:        warmLoadingRunning(`{"running":[{"model":"other-heavy","state":"starting","cmd":"x"}]}`),
		upstreamStatus: status,
		upstreamBody:   body,
	}
	srv := fake.server(t)
	defer srv.Close()
	wire := decodeWire(t, warmFailTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred || fake.loopCalls.Load() == 0 {
		t.Fatalf("deferred=%v loop requests=%d class=%q reason=%q, want a run: another model was mid-swap", wire.Deferred, fake.loopCalls.Load(), wire.DeferClass, wire.Reason)
	}
}

// The evidence rule on its own: a 5xx refusal defers a run only when nothing says
// the seat is busy or on its way, and the death marker outranks a busy-looking body.
func TestWarmStartFailedNeedsPositiveEvidence(t *testing.T) {
	absent := residency{}
	cases := []struct {
		name   string
		status int
		body   string
		res    residency
		err    error
		want   bool
	}{
		{"exited prematurely, seat absent, nothing loading", 500, "upstream command exited prematurely", absent, nil, true},
		{"an unrecognised 5xx, seat absent, nothing loading", 502, "bad gateway", absent, nil, true},
		{"an empty 500 is not one of the busy shapes", 500, "", absent, nil, true},
		{"the seat's row reads stopped", 500, "boom", residency{state: "stopped"}, nil, true},
		{"the death marker outranks a busy-looking body", 500, `{"error":"upstream command exited prematurely","src":"llama-swap"}`, absent, nil, true},
		{"503 process is not ready", 503, "process is not ready", absent, nil, false},
		{"500 from llama-swap itself", 500, `{"src":"llama-swap"}`, absent, nil, false},
		{"500 health check timed out", 500, "health check timed out", absent, nil, false},
		{"empty 502", 502, "", absent, nil, false},
		{"the seat's row reads starting", 500, "upstream command exited prematurely", residency{state: "starting"}, nil, false},
		{"the seat's row reads stopping", 500, "boom", residency{state: "stopping"}, nil, false},
		{"another model is mid-swap", 500, "boom", residency{busy: "other:starting"}, nil, false},
		{"the seat reads ready", 500, "boom", residency{ready: true}, nil, false},
		{"/running could not be read", 500, "upstream command exited prematurely", absent, errors.New("HTTP 500 from GET /running"), false},
		{"not a server error", 404, "not found", absent, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, why := warmStartFailed(tc.status, tc.body, tc.res, tc.err)
			if got != tc.want {
				t.Fatalf("warmStartFailed = %v (%q), want %v", got, why, tc.want)
			}
			if !got && why == "" {
				t.Fatal("a refusal that is not evidence must say why")
			}
		})
	}
}

// With enforcement on, a 502 that is not the empty (busy) shape and a seat that
// never comes up is a seat that did not start, like a 500.
func TestRunAgentTaskWarmUp502WithABodyAndTheSeatNeverReadyDefersAtOnce(t *testing.T) {
	status, body := warmRefusedOnce(http.StatusBadGateway, "unable to start process: upstream command exited prematurely")
	fake := &agentFake{
		rosterIDs:      []string{agentTestSeat},
		loop:           func(int64) string { return doneChat("x") },
		repack:         func(int64) string { return `{"answer":"42"}` },
		running:        func(int64) string { return `{"running":[]}` },
		upstreamStatus: status,
		upstreamBody:   body,
	}
	srv := fake.server(t)
	defer srv.Close()
	wire := decodeWire(t, warmFailTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if !wire.Deferred || wire.DeferClass != core.DeferClassInfrastructure || !strings.HasPrefix(wire.Reason, core.SeatWarmFailedReason) || !strings.Contains(wire.Reason, "HTTP 502") {
		t.Fatalf("deferred=%v class=%q reason=%q, want the seat-warm-up defer for a 502 with a body", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if fake.loopCalls.Load() != 0 {
		t.Fatalf("loop requests = %d, want 0", fake.loopCalls.Load())
	}
}

// Enforcement is off by default (the audit mode of the house security standard):
// the run proceeds as it always did, and the wire says it WOULD have deferred, so
// would-be defers can be counted before anyone turns the rule on.
func TestRunAgentTaskWarmUp5xxIsAuditedWhenEnforcementIsOff(t *testing.T) {
	status, body := warmRefusedOnce(http.StatusInternalServerError, "upstream command exited prematurely")
	fake := &agentFake{
		rosterIDs:      []string{agentTestSeat},
		loop:           func(int64) string { return doneChat("The answer is 42.") },
		repack:         func(int64) string { return `{"answer":"42"}` },
		running:        func(int64) string { return `{"running":[]}` },
		upstreamStatus: status,
		upstreamBody:   body,
	}
	srv := fake.server(t)
	defer srv.Close()
	wire := decodeWire(t, admissionTestPipeline(t, srv.URL, 30).Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred || fake.loopCalls.Load() == 0 {
		t.Fatalf("deferred=%v loop requests=%d reason=%q, want the run to proceed with enforcement off", wire.Deferred, fake.loopCalls.Load(), wire.Reason)
	}
	if !strings.Contains(wire.AdmissionNote, "would have deferred") || !strings.Contains(wire.AdmissionNote, "agent_warm_failure_defer") {
		t.Fatalf("admission_note = %q, want the would-be defer counted on the wire", wire.AdmissionNote)
	}
}
