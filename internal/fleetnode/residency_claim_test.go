// residency_claim_test.go: the seat-proof write reaches the PULL door too
// (0.128.2). The push dispatch and the pull-queue claim loop each hand-write
// their own run sequence around s.runner.Run; a post-run side effect landed
// on one alone is the drift class the register's "agent doors" entry
// records, and a queue-claiming node would have kept charging cold loads
// for warm seats with nothing to say so.

package fleetnode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetqueue"
	"github.com/dmmdea/offload-harness/internal/netguard"
)

// pullOneAgentContract serves one agent job from a fake queue holder, lets the
// node claim it through claimOne (the production pull door), and waits for it
// to finish. The holder answers every settle with 200.
func pullOneAgentContract(t *testing.T, s *Server, jobs *Jobs, cfg configForClaim, id string) {
	t.Helper()
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fleet/queue/claim" {
			_ = json.NewEncoder(w).Encode(fleetqueue.Job{
				ID: id, TaskType: string(core.TaskAgentRun),
				Payload: json.RawMessage(`{"schema_version":1,"goal":"g","output_schema":` + agentSchemaJSON + `}`),
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer holder.Close()
	client := &http.Client{Timeout: 5 * time.Second, Transport: netguard.SafeTransport(nil)}
	if _, ok := s.claimOne(context.Background(), client, holder.URL, "testnode", cfg.cfg); !ok {
		t.Fatalf("claimOne reported no claim for %s", id)
	}
	waitJobTerminal(t, jobs, id)
}

// TestAPulledContractsCompletedCallWritesTheSeatState is the pull-door twin of
// TestACompletedCallOnTheSeatWritesTheSeatStateWithoutAProbe: /running says
// nothing is loaded; a contract the node PULLED from the queue holder
// completes calls on the advertised seat; the next health read reports
// loaded without touching /running.
func TestAPulledContractsCompletedCallWritesTheSeatState(t *testing.T) {
	swap := newMutableSwap(t, "gemma-4-e4b", "offload-e4b")
	runner := newAgentResultRunner()
	cfg := claimCfg(t, swap.srv.URL)
	s, jobs := newTestServer(t, cfg.cfg, runner, cfg.opts)
	if m := healthAfterProbe(t, s); m["seat_loaded"] != false {
		t.Fatalf("fixture: seat_loaded = %v, want false with nothing running", m["seat_loaded"])
	}

	runner.will(resultRanOnSeat)
	pullOneAgentContract(t, s, jobs, cfg, "pulled-ran")
	before := swap.running.Load()
	m := healthOnce(t, s)
	if m["seat_loaded"] != true || m["seat_starting"] != false {
		t.Fatalf("after a PULLED contract completed 2 calls on the seat: seat_loaded=%v seat_starting=%v, want true/false — the claim door must write the same proof the dispatch door does (payload %v)", m["seat_loaded"], m["seat_starting"], m)
	}
	if swap.running.Load() != before {
		t.Fatal("the read after a pulled contract probed /running: the job's own proof must be served without a probe")
	}

	setCachedSeatLoaded(s, false)
	runner.will(resultZeroSteps)
	pullOneAgentContract(t, s, jobs, cfg, "pulled-defer")
	if m := healthOnce(t, s); m["seat_loaded"] != false {
		t.Fatalf("seat_loaded = %v after a pulled ZERO-STEP defer, want false: the pull door must apply the same proof rule", m["seat_loaded"])
	}
}

// TestAnUndecodableAgentResultIsSaidOnce: the only producer of the result
// JSON is this process's own agenttask, so a decode failure is a wire-shape
// drift — logged once per process, never silently costing the fast path
// with nothing to distinguish it from a node that never ran agent work.
func TestAnUndecodableAgentResultIsSaidOnce(t *testing.T) {
	swap := newMutableSwap(t, "gemma-4-e4b", "offload-e4b")
	runner := newAgentResultRunner()
	s, jobs := newTestServer(t, agentHealthCfg(swap.srv.URL), runner, authOpts(true))
	healthAfterProbe(t, s)
	buf := captureLog(t)

	runner.will(`{"seat":"offload-e4b","steps":"two"}`) // steps is not a number: not this process's shape
	runAgentContract(t, s, jobs, "agd-bad-1")
	runAgentContract(t, s, jobs, "agd-bad-2")
	const said = "could not be decoded for the seat-state write"
	if n := strings.Count(buf.String(), said); n != 1 {
		t.Fatalf("the decode failure was logged %d times after two undecodable results, want exactly 1 (log: %s)", n, buf.String())
	}
	if m := healthOnce(t, s); m["seat_loaded"] != false {
		t.Fatalf("seat_loaded = %v after undecodable results, want the probe's false: nothing may be written from a result that could not be read", m["seat_loaded"])
	}
}

// configForClaim bundles the config claimOne needs (a Home for the job dir,
// the agent lane on, the bearer token) with the matching server Options.
type configForClaim struct {
	cfg  config.Config
	opts *Options
}

func claimCfg(t *testing.T, endpoint string) configForClaim {
	t.Helper()
	cfg := agentHealthCfg(endpoint)
	cfg.Home = t.TempDir()
	cfg.FleetAuthToken = "tok"
	return configForClaim{cfg: cfg, opts: &Options{
		NodeID:           "testnode",
		Snapshot:         goodSnapshot,
		Footprints:       func() []FootprintEntry { return nil },
		LoopbackListener: true,
	}}
}
