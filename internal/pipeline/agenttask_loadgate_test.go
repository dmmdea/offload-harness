package pipeline

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// slowGauges answers like gaugesOf, after `wait`: an engine that serves /metrics
// only between batches.
func slowGauges(wait time.Duration, running, waiting int) func() string {
	return func() string {
		time.Sleep(wait)
		return gaugesOf(running, waiting)()
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
	defer func(w time.Duration) { loadSettleWait = w }(loadSettleWait)
	loadSettleWait = 300 * time.Millisecond
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
		// The registry says solo, but the engine that could contradict it has not
		// answered when the run ends: the one answer that cannot be trusted then is
		// "nobody else is here".
		{"a registry that says solo and an engine that has not answered in time", false, slowGauges(1200*time.Millisecond, 3, 1), false, false},
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

// slowTimingsLoop is slowFirstDeltaLoop with llama.cpp's own `timings` on the usage
// frame, so the run carries the server's prefill accounting as well as the
// time-to-first-delta sample.
func slowTimingsLoop(wait time.Duration) func(int64, map[string]any, http.ResponseWriter, *http.Request) {
	return func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(s string) { _, _ = w.Write([]byte("data: " + s + "\n\n")); fl.Flush() } // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter — a test fake streaming SSE, not HTML
		select {
		case <-r.Context().Done():
			return
		case <-time.After(wait):
		}
		write(`{"choices":[{"index":0,"delta":{"role":"assistant","content":"The answer is 42."},"finish_reason":null}]}`)
		write(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		write(`{"choices":[],"usage":{"prompt_tokens":20000,"completion_tokens":6},"timings":{"prompt_n":20000,"prompt_ms":10000,"cache_n":0,"predicted_n":6,"predicted_ms":60}}`)
		write(`[DONE]`)
	}
}

// The server's own prefill timings are a SECOND sample the run feeds the store, beside
// the largest time-to-first-delta sample, and it is gated by the run's peak load too:
// a shared seat's timings are what a shared seat gives one request. (The test above
// uses a seat that reports no timings, so it cannot see this line.)
func TestRunAgentTaskConcurrentRunsWithServerTimingsDoNotFeedThePrefillRate(t *testing.T) {
	defer compressLiveness(t, 5*time.Second, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 5*time.Second)()
	slow := func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
		wait := 1500 * time.Millisecond
		if n >= 2 {
			wait = 3 * time.Second
		}
		slowTimingsLoop(wait)(n, body, w, r)
	}
	contract := testContract()
	contract.OutputSchema = nil

	t.Run("control: a run alone records the server's timings", func(t *testing.T) {
		fake := &agentFake{rosterIDs: []string{agentTestSeat}, loopStream: slow}
		srv := fake.server(t)
		defer srv.Close()
		dir := sharedStateDir(t)
		if w := decodeWire(t, pipelineOn(t, srv.URL, dir).Run(context.Background(), agentTestRequest(t, contract))); w.Deferred {
			t.Fatalf("deferred: %s", w.Reason)
		}
		if got := storedSeat(t, dir).PrefillTokS; got == 1000000 {
			t.Fatalf("a solo run did not move the rate (%v): the fixture cannot tell the gate from a dead path", got)
		}
	})
	t.Run("two runs on the seat: neither teaches the rate", func(t *testing.T) {
		fake := &agentFake{rosterIDs: []string{agentTestSeat}, loopStream: slow}
		srv := fake.server(t)
		defer srv.Close()
		dir := sharedStateDir(t)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if i == 1 {
					time.Sleep(300 * time.Millisecond) // joins while the first prefills
				}
				if w := decodeWire(t, pipelineOn(t, srv.URL, dir).Run(context.Background(), agentTestRequest(t, contract))); w.Deferred {
					t.Errorf("run %d deferred: %s", i, w.Reason)
				}
			}(i)
		}
		wg.Wait()
		if got := storedSeat(t, dir).PrefillTokS; got != 1000000 {
			t.Fatalf("prefill_tok_s = %v: a run that shared the seat moved the published rate through the server's timings", got)
		}
	})
}
