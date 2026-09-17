// agentrun_admission_test.go pins the MCP agent_run door's ADMISSION block
// against the delegation door's (registers S-25 / W-25 and S-26 / W-03).
//
// The two doors run the same loop on the same seat and have drifted apart step by
// step; each drift was found in production, not in review. This file holds the two
// steps this change brings across:
//
//   - the llama-swap SWAP PRE-FLIGHT. The delegation door has waited out another
//     session's model swap OUTSIDE the wall since 2026-09-02 (ADR 0032); this door
//     never did, so an agent_run that arrived mid-swap spent its wall queued inside
//     llama-swap and reported a wall timeout.
//   - the FOREIGN FENCE pre-check. Under a lease this process does not hold and
//     that refuses new runs, the cordon below waits the whole admission budget for
//     an answer that was already on disk.

package mcpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// TestAgentRunWaitsOutAnotherModelsSwapBeforeTheWall: another model is mid-swap
// for two polls of the pre-flight. llama-swap QUEUES a request that needs a seat
// it is still re-arranging, with no timeout of its own — so a run that dials
// through the swap spends its wall in that queue. The door must hold here, on the
// admission budget, and only then speak to the seat.
func TestAgentRunWaitsOutAnotherModelsSwapBeforeTheWall(t *testing.T) {
	const seat = "agent-pool"
	var firstChat atomic.Int64
	start := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/running":
			// Two polls of somebody else's swap, then the seat reads ready.
			if time.Since(start) < 5500*time.Millisecond {
				fmt.Fprint(w, `{"running":[{"model":"other-heavy","state":"starting","cmd":"x"}]}`)
				return
			}
			fmt.Fprintf(w, `{"running":[{"model":%q,"state":"ready","cmd":"y"}]}`, seat)
		case "/v1/models":
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model"}]}`, seat)
		case "/upstream/" + seat + "/v1/models":
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"max_model_len":65536}]}`, seat)
		case "/v1/chat/completions":
			firstChat.CompareAndSwap(0, time.Since(start).Milliseconds())
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The answer is 42."},"finish_reason":"stop"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Home = t.TempDir()
	cfg.StateDir = t.TempDir()
	cfg.Endpoint = srv.URL
	cfg.Model = seat
	cfg.AgentModel = seat
	cfg.AgentAdmissionWaitSec = 30
	s := New(pipeline.New(cfg, nil, nil, nil))

	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":2,"timeout_sec":60}`, t.TempDir())))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] == true {
		t.Fatalf("the run deferred: %v", m)
	}
	if ms := firstChat.Load(); ms < 5900 {
		t.Errorf("the first chat request arrived %d ms in, want it held until the swap settled (~6 s): the wall must not pay for another session's swap", ms)
	}
	if wait, _ := m["admission_wait_sec"].(float64); wait < 5.9 {
		t.Errorf("admission_wait_sec = %v, want the pre-flight's wait reported under the delegation door's own field name", m["admission_wait_sec"])
	}
}

// fencedAgentRunServer arms an exclusive text lease in a private state dir, points
// the affinity gate at it, and returns a server configured for that dir plus the
// lease epoch an INHERITED lease is proved by.
//
// `dials` counts the SEAT traffic — residency reads, warm-up and chat — and not
// the served-roster check, which runs earlier for every call and answers from
// llama-swap's model list without touching a card. Under a fence the seat must
// see none of the former.
func fencedAgentRunServer(t *testing.T, dials *atomic.Int64) (*Server, uint64) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/running":
			dials.Add(1)
			fmt.Fprint(w, `{"running":[{"model":"agent-pool","state":"ready","cmd":"y"}]}`)
		case "/v1/models":
			fmt.Fprint(w, `{"object":"list","data":[{"id":"agent-pool","object":"model"}]}`)
		case "/v1/chat/completions":
			dials.Add(1)
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The answer is 42."},"finish_reason":"stop"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	root := t.TempDir()
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	if err := modelaffinity.SetGPULease("", root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = modelaffinity.SetGPULease("", filepath.Join(t.TempDir(), "My Drive", "x")) })
	holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "5070 Ti bench", Exclusive: true, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Release() })

	cfg := config.Default()
	cfg.Home = t.TempDir()
	cfg.StateDir = root
	cfg.Endpoint = srv.URL
	cfg.Model = "agent-pool"
	cfg.AgentModel = "agent-pool"
	cfg.AgentAdmissionWaitSec = 5
	info := delegate.LocalLease(cfg.GPULockPath, cfg.StateDir)
	if !info.Held || !info.Exclusive {
		t.Fatalf("the test lease did not take: %+v", info)
	}
	return New(pipeline.New(cfg, nil, nil, nil)), info.Epoch
}

// TestAgentRunDefersAtOnceUnderAForeignFence: the local card is held by another
// process's exclusive lease. No wait inside this call can change that, so the door
// must answer in milliseconds, name the fence and its holder's declared window,
// and class the defer `capacity` — the class a caller re-places on.
func TestAgentRunDefersAtOnceUnderAForeignFence(t *testing.T) {
	var dials atomic.Int64
	s, _ := fencedAgentRunServer(t, &dials)

	start := time.Now()
	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":2,"timeout_sec":60}`, t.TempDir())))
	spent := time.Since(start)
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true {
		t.Fatalf("want a defer under a foreign fence: %v", m)
	}
	if m["defer_class"] != string(core.DeferClassCapacity) {
		t.Errorf("defer_class = %v, want %q: a fenced card is capacity, and capacity is what a caller re-places on", m["defer_class"], core.DeferClassCapacity)
	}
	if spent > time.Second {
		t.Errorf("the fence's verdict is on disk before the dial: deferred after %s, want milliseconds", spent)
	}
	reason, _ := m["reason"].(string)
	for _, want := range []string{"gpu busy", "exclusive text lease", "gpu lease class=text", `reason="5070 Ti bench"`} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason must carry %q: %s", want, reason)
		}
	}
	if dials.Load() != 0 {
		t.Errorf("a fenced seat must not be probed, warmed or dialled; %d seat request(s) reached the endpoint", dials.Load())
	}
}

// TestAgentRunUnderItsOwnInheritedLeaseRuns: the holder's own session keeps the
// cards its lease cleared (GPU_LEASE_EPOCH). ADR 0032 still governs every hold
// that does not fence this caller.
func TestAgentRunUnderItsOwnInheritedLeaseRuns(t *testing.T) {
	var dials atomic.Int64
	s, epoch := fencedAgentRunServer(t, &dials)
	t.Setenv("GPU_LEASE_EPOCH", strconv.FormatUint(epoch, 10))

	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":2,"timeout_sec":60}`, t.TempDir())))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] == true {
		t.Fatalf("an INHERITED lease is not a fence for its own holder: %v", m)
	}
	if dials.Load() == 0 {
		t.Error("the holder's own run must reach the seat its lease cleared")
	}
}
