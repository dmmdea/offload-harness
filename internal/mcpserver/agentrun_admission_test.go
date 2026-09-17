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

// TestForeignFenceAndTheCordonShareOnePredicate is why the cordon-timeout defer
// below needs a seam to reach at all, and it is a guard in its own right.
//
// delegate.ForeignFence delegates to modelaffinity.BlocksNewRun, which is
// exactly what AwaitRunSlot waits on — so for every lease shape, "the pre-check
// refuses" and "the cordon would block" are the SAME answer, and the pre-check
// converts the whole 300 s wait into an immediate verdict. If anyone ever
// weakens one of the two, this fails and says so, instead of quietly restoring
// the 47-row, 3.92 h wait S-26 removed.
func TestForeignFenceAndTheCordonShareOnePredicate(t *testing.T) {
	shapes := []struct {
		name string
		cls  gpulease.Class
		opts gpulease.Options
	}{
		{"plain text reservation", gpulease.ClassText, gpulease.Options{Reason: "bench", TTL: time.Hour}},
		{"exclusive text", gpulease.ClassText, gpulease.Options{Reason: "bench", Exclusive: true, TTL: time.Hour}},
		{"draining text", gpulease.ClassText, gpulease.Options{Reason: "bench", Draining: true, TTL: time.Hour}},
		{"media render", gpulease.ClassMedia, gpulease.Options{Reason: "render", TTL: time.Hour}},
	}
	for _, sh := range shapes {
		for _, inherited := range []bool{false, true} {
			root := t.TempDir()
			m, err := gpulease.OpenAt("", root)
			if err != nil {
				t.Fatal(err)
			}
			l, err := m.TryAcquire(sh.cls, sh.opts)
			if err != nil {
				t.Fatal(err)
			}
			info := delegate.LocalLease("", root)
			if inherited {
				t.Setenv("GPU_LEASE_EPOCH", strconv.FormatUint(info.Epoch, 10))
			} else {
				t.Setenv("GPU_LEASE_EPOCH", "")
			}
			fenced, _ := delegate.ForeignFence(info)
			if blocks := modelaffinity.BlocksNewRun(info); fenced != blocks {
				t.Errorf("%s (inherited=%v): ForeignFence=%v but the cordon blocks=%v — the pre-check no longer predicts the cordon it replaces",
					sh.name, inherited, fenced, blocks)
			}
			_ = l.Release()
		}
	}
}

// TestAgentRunPlainReservationIsNotAFenceAndDoesNotHold: ADR 0032's own case on
// this door. A plain text reservation — a benchmark whose holder unloaded
// nothing — steers placement but admits the load, so the run proceeds. It is
// neither a fence nor a cordon wait, which is why it cannot be used to drive the
// cordon-timeout path.
func TestAgentRunPlainReservationIsNotAFenceAndDoesNotHold(t *testing.T) {
	var dials atomic.Int64
	s, root := reservedAgentRunServer(t, &dials, gpulease.Options{Reason: "5070 Ti bench", TTL: time.Hour})
	_ = root

	start := time.Now()
	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":2,"timeout_sec":60}`, t.TempDir())))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] == true {
		t.Fatalf("a plain reservation admits the load (ADR 0032); got a defer: %v", m)
	}
	if spent := time.Since(start); spent > 2*time.Second {
		t.Errorf("a plain reservation must not hold the cordon at all; the run took %s", spent)
	}
}

// TestAgentRunCordonTimeoutReportsItsAdmission: the cordon-timeout defer is the
// pre-check's race window — a fencing lease taken between the pre-check's read
// and AwaitRunSlot's. It is reached here by stubbing the pre-check away, which
// is the only way to reach it (see TestForeignFenceAndTheCordonShareOnePredicate).
//
// Whatever else it is, it is a run that spent its admission budget, and it must
// say so: the pipeline door has stamped admission on this exact path since
// 0.117.0, and a caller that reads a bare "gpu busy" cannot tell a 300 s wait
// from an instant refusal.
func TestAgentRunCordonTimeoutReportsItsAdmission(t *testing.T) {
	var dials atomic.Int64
	s, _ := reservedAgentRunServer(t, &dials, gpulease.Options{Reason: "arm B drain", Draining: true, TTL: time.Hour})
	// Blind the pre-check so the DRAINING hold below reaches the cordon, exactly
	// as a lease taken a microsecond after the pre-check's read would.
	s.foreignFence = func(gpulease.Info) (bool, string) { return false, "" }

	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":2,"timeout_sec":60}`, t.TempDir())))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true {
		t.Fatalf("a run held at the cordon for its whole budget must defer: %v", m)
	}
	wait, _ := m["admission_wait_sec"].(float64)
	if wait < 0.9 {
		t.Errorf("admission_wait_sec = %v, want the cordon wait reported as admission — the pipeline door stamps it on this path", m["admission_wait_sec"])
	}
	note, _ := m["admission_note"].(string)
	if !strings.Contains(note, "cordon") {
		t.Errorf("admission_note = %q, want the cordon named", note)
	}
}

// reservedAgentRunServer is fencedAgentRunServer with the lease shape as an
// argument, and it hands back the state root so a caller can inspect the lease.
func reservedAgentRunServer(t *testing.T, dials *atomic.Int64, opts gpulease.Options) (*Server, string) {
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
	holder, err := m.TryAcquire(gpulease.ClassText, opts)
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
	cfg.AgentAdmissionWaitSec = 1 // a compressed budget: the cordon must not cost the suite 300 s
	return New(pipeline.New(cfg, nil, nil, nil)), root
}

// TestAgentRunCtxWindowNoteNamesTheFallback is the same pin on the other door:
// this one measured ctx_window 8,192 cold and 114,688 warm minutes apart and
// nothing in its result said which it was, or why. The passthrough here outlasts
// the admission budget, so the warm-up spends it and the window probe inherits a
// dead context.
func TestAgentRunCtxWindowNoteNamesTheFallback(t *testing.T) {
	const seat = "agent-pool"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/running":
			fmt.Fprint(w, `{"running":[]}`) // never resident: the warm-up keeps trying
		case "/v1/models":
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model"}]}`, seat)
		case "/upstream/" + seat + "/v1/models":
			time.Sleep(4 * time.Second) // longer than the 3 s admission budget
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"max_model_len":131072}]}`, seat)
		case "/v1/chat/completions":
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
	cfg.AgentAdmissionWaitSec = 3
	s := New(pipeline.New(cfg, nil, nil, nil))

	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":2,"timeout_sec":60}`, t.TempDir())))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] == true {
		t.Fatalf("a spent probe budget falls back, it does not defer: %v", m)
	}
	note, _ := m["ctx_window_note"].(string)
	if !strings.Contains(note, "fallback") {
		t.Fatalf("ctx_window_note = %q, want the conservative fallback named beside ctx_window %v", note, m["ctx_window"])
	}
}
