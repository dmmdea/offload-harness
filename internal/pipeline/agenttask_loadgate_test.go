package pipeline

import (
	"bytes"
	"context"
	"log"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// pipelineNoRegistry is pipelineOn with a run registry that cannot be opened (a
// cloud-synced lease root is refused), so the run has no sampler of its own and
// the seat's load rests on the engine's gauges alone.
func pipelineNoRegistry(t *testing.T, base, dir string) *Pipeline {
	t.Helper()
	p := pipelineOn(t, base, dir)
	p.cfg.GPULockPath = filepath.Join(dir, "OneDrive", "lease")
	return p
}

// engineSeatFake is a seat whose engine reports the given request gauges (nil =
// the engine's /metrics is not served, so it cannot be read) and whose first
// request streams its first delta after `wait`, reporting 5,000 uncached prompt
// tokens.
func engineSeatFake(t *testing.T, gauges func() string, wait time.Duration) (*agentFake, *httptest.Server) {
	t.Helper()
	var base atomic.Value
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		running: func(int64) string {
			return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"x","proxy":"` + base.Load().(string) + `/seat"}]}`
		},
		loopStream: slowFirstDeltaLoop(wait),
	}
	if gauges != nil {
		fake.seatMetrics = func(n int64) string {
			return gauges() +
				"vllm:iteration_tokens_total_count " + strconv.FormatInt(100*n, 10) + "\n" +
				"vllm:generation_tokens_total " + strconv.FormatInt(40*n, 10) + "\n" +
				"vllm:prompt_tokens_total 9000\nvllm:num_preemptions_total 0\nvllm:kv_cache_usage_perc 0.5\n"
		}
	}
	srv := fake.server(t)
	base.Store(srv.URL)
	return fake, srv
}

func gaugesOf(running, waiting int) func() string {
	return func() string {
		return "vllm:num_requests_running " + strconv.Itoa(running) + "\nvllm:num_requests_waiting " + strconv.Itoa(waiting) + "\n"
	}
}

// PR-13's fail condition is "a concurrent sample moves the published rate". The run
// registry only knows the runs of THIS box's state root: a peer it cannot see (a
// cascade call on the same seat model, another process) is visible to the engine
// alone, and the run reads the engine's own gauges at its first delta. A run with
// no registry and no readable engine has nobody to vouch that it was solo: unknown
// is not solo, and the failure is logged, not silent.
func TestRunAgentTaskLoadThatOnlyTheEngineSeesKeepsTheRateHonest(t *testing.T) {
	defer compressLiveness(t, 5*time.Second, 100*time.Millisecond, core.AgentCeilingSecCap)()
	contract := testContract()
	contract.OutputSchema = nil
	for _, tc := range []struct {
		name         string
		noRegistry   bool
		gauges       func() string // nil: the engine cannot be read
		wantRecorded bool
		wantLog      bool // the run registry could not be opened, and the log says so
	}{
		{"a peer only the engine sees (the registry says solo)", false, gaugesOf(3, 1), false, false},
		{"no registry, a peer on the engine", true, gaugesOf(2, 0), false, true},
		{"no registry and an engine that cannot be read: unknown is not solo", true, nil, false, true},
		{"control: no registry, the engine shows the run alone", true, gaugesOf(1, 0), true, true},
		{"control: a registry and an engine that show the run alone", false, gaugesOf(1, 0), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			oldOut, oldFlags := log.Writer(), log.Flags()
			log.SetOutput(&buf)
			t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })
			_, srv := engineSeatFake(t, tc.gauges, 600*time.Millisecond)
			defer srv.Close()
			dir := sharedStateDir(t)
			p := pipelineOn(t, srv.URL, dir)
			if tc.noRegistry {
				p = pipelineNoRegistry(t, srv.URL, dir)
			}
			wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, contract)))
			if wire.Deferred {
				t.Fatalf("deferred: %s (%s)", wire.Reason, wire.DeferClass)
			}
			if wire.QueuedMs != 0 {
				t.Fatalf("queued_ms = %d: the hold, not the load gate, would explain a skipped sample", wire.QueuedMs)
			}
			got := storedSeat(t, dir).PrefillTokS
			if tc.wantRecorded && got == 1000000 {
				t.Fatalf("a run known to be solo did not record its prefill sample (rate still %v): this test cannot tell the gate from a dead observation path", got)
			}
			if !tc.wantRecorded && got != 1000000 {
				t.Fatalf("prefill_tok_s = %v: a run that shared the seat, or that nothing could vouch for, moved the published rate (want the stored 1000000)", got)
			}
			logged := strings.Contains(buf.String(), "run registry not readable")
			if logged != tc.wantLog {
				t.Fatalf("registry-not-readable logged = %v, want %v: an unopenable run registry must not fail open silently\n%s", logged, tc.wantLog, buf.String())
			}
		})
	}
}
