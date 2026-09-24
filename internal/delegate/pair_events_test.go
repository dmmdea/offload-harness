package delegate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// pairCapture is a stand-in for PAIR's loopback ingress: it records every
// frame the runner posts.
type pairCapture struct {
	mu     sync.Mutex
	frames []map[string]any
}

func (c *pairCapture) handler(w http.ResponseWriter, r *http.Request) {
	var f map[string]any
	_ = json.NewDecoder(r.Body).Decode(&f)
	c.mu.Lock()
	c.frames = append(c.frames, f)
	c.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (c *pairCapture) snapshot() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.frames...)
}

func pairInfo(f map[string]any) map[string]any {
	return f["params"].(map[string]any)["workloadInfo"].(map[string]any)
}

// waitABit is the poll interval for the asynchronous emitter in these tests.
func waitABit() { time.Sleep(50 * time.Millisecond) }

// TestPairNodeNameUsesTheDispatchHost: the in-flight frames of a remote
// placement must name the node by its dispatch host (the PAIR member name),
// never by the fleet node id, which PAIR cannot resolve.
func TestPairNodeNameUsesTheDispatchHost(t *testing.T) {
	cases := map[[2]string]string{
		{"http://NODE-B:18811", "node-b-ampere8"}:    "node-b",
		{"http://node-b:18811/", "node-b-ampere8"}:   "node-b",
		{"http://192.0.2.7:18811", "node-b-ampere8"}: "192.0.2.7",
		{"", "node-b-ampere8"}:                       "node-b-ampere8",
		{"::not a url::", "node-b-ampere8"}:          "node-b-ampere8",
		{"http://node-b.tail.ts.net:18811", "x"}:     "node-b.tail.ts.net",
	}
	for in, want := range cases {
		if got := pairNodeName(in[0], in[1]); got != want {
			t.Errorf("pairNodeName(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

// pairAppDir writes the two PAIR identity files the emitter reads and points
// the emitter at them for the test's lifetime.
func pairAppDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "node-id.json"), []byte(`{"node_uuid":"self-uuid","created_at":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "cluster"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cluster", "members.json"), []byte(`[{"name":"node-a","nodeUuid":"self-uuid"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_PAIR_APPDIR", dir)
}

// TestLocalPlacementReportsOneCardToPAIR drives one contract through a local
// placement and checks the PAIR frames: queued when it is handed to the
// runner, running when the run reports the seat working (0.140.5), completed
// when it finishes, all under the SAME id, model and engine (PAIR's store keys
// a card on that identity — two identities would be two cards, one stuck
// running).
func TestLocalPlacementReportsOneCardToPAIR(t *testing.T) {
	pairAppDir(t)
	c := &pairCapture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	cfg := testCfg(t)
	cfg.PairWorkloadsEnabled = true
	cfg.PairWorkloadsEndpoint = srv.URL
	local := LocalRunner(func(ctx context.Context, ac core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		core.ReportProgress(ctx, core.LiveProgress{Phase: "decoding", TokensOut: 4, LastProgressMs: time.Now().UnixMilli()})
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: "done", Seat: "gemma-4-e4b"}, nil
	})
	res, sum, err := RunWith(context.Background(), cfg, local, []core.AgentContract{{Goal: "say done"}}, "local", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 1 || len(res) != 1 {
		t.Fatalf("succeeded=%d results=%d", sum.Succeeded, len(res))
	}
	// Emission is asynchronous; the runner is gone, so wait through the capture.
	deadline := 50
	for len(c.snapshot()) < 3 && deadline > 0 {
		deadline--
		waitABit()
	}
	frames := c.snapshot()
	byMethod := framesByMethod(frames)
	if len(frames) != 3 || byMethod["workload:submitted"] == nil || byMethod["workload:started"] == nil || byMethod["workload:completed"] == nil {
		t.Fatalf("frames = %d, want queued, running, completed: %v", len(frames), frames)
	}
	q, a, b := pairInfo(byMethod["workload:submitted"]), pairInfo(byMethod["workload:started"]), pairInfo(byMethod["workload:completed"])
	for _, k := range []string{"id", "runId", "model", "engine", "originatedFrom", "scheduledOn", "createdAt"} {
		if a[k] != b[k] || q[k] != a[k] {
			t.Fatalf("identity field %q differs between frames: %v / %v / %v", k, q[k], a[k], b[k])
		}
	}
	if q["startedAt"] != nil {
		t.Fatalf("the queued frame must carry no start: %v", q)
	}
	if a["id"] != res[0].JobID || a["engine"] != "llamacpp" || a["scheduledOn"] != "self-uuid" {
		t.Fatalf("card identity wrong: %v (job %s)", a, res[0].JobID)
	}
	if a["startedAt"] == nil || b["completedAt"] == nil || b["error"] != nil {
		t.Fatalf("timestamps/verdict wrong: running=%v completed=%v", a, b)
	}
}

// TestPAIRDisabledEmitsNothing is the default: no config opt-in, no frames.
func TestPAIRDisabledEmitsNothing(t *testing.T) {
	pairAppDir(t)
	c := &pairCapture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	cfg := testCfg(t)
	cfg.PairWorkloadsEndpoint = srv.URL
	local := LocalRunner(func(ctx context.Context, ac core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: "done"}, nil
	})
	if _, _, err := RunWith(context.Background(), cfg, local, []core.AgentContract{{Goal: "say done"}}, "local", nil, nil); err != nil {
		t.Fatal(err)
	}
	waitABit()
	if n := len(c.snapshot()); n != 0 {
		t.Fatalf("frames = %d, want 0 when pair_workloads_enabled is off", n)
	}
}

// TestFailedLocalPlacementReportsErrored: a deferred local run ends the card
// as failed with the defer reason.
func TestFailedLocalPlacementReportsErrored(t *testing.T) {
	pairAppDir(t)
	c := &pairCapture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	cfg := testCfg(t)
	cfg.PairWorkloadsEnabled = true
	cfg.PairWorkloadsEndpoint = srv.URL
	local := LocalRunner(func(ctx context.Context, ac core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Deferred: true, Reason: "seat busy"}, nil
	})
	if _, _, err := RunWith(context.Background(), cfg, local, []core.AgentContract{{Goal: "say done"}}, "local", nil, nil); err != nil {
		t.Fatal(err)
	}
	deadline := 50
	for len(c.snapshot()) < 2 && deadline > 0 {
		deadline--
		waitABit()
	}
	frames := c.snapshot()
	if len(frames) != 2 || frames[1]["method"] != "workload:errored" {
		t.Fatalf("frames = %v", frames)
	}
	if info := pairInfo(frames[1]); info["error"] != "seat busy" || info["state"] != "failed" {
		t.Fatalf("terminal frame wrong: %v", info)
	}
}

// TestRunWithFlushesPAIRFramesBeforeReturning: the delegate CLI is a
// short-lived process, and a frame still in flight when RunWith returns dies
// with it — PAIR kept the card "running" until its staleness sweep failed it
// (a deferred CLI run's terminal frame never arrived). Every frame must be
// delivered by the time RunWith returns, with no polling by the caller.
func TestRunWithFlushesPAIRFramesBeforeReturning(t *testing.T) {
	pairAppDir(t)
	c := &pairCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // a slow ingress, well inside the 2 s send budget
		c.handler(w, r)
	}))
	defer srv.Close()
	cfg := testCfg(t)
	cfg.PairWorkloadsEnabled = true
	cfg.PairWorkloadsEndpoint = srv.URL
	local := LocalRunner(func(ctx context.Context, ac core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Deferred: true, Reason: "stalled"}, nil
	})
	if _, _, err := RunWith(context.Background(), cfg, local, []core.AgentContract{{Goal: "say done"}}, "local", nil, nil); err != nil {
		t.Fatal(err)
	}
	// Sends are concurrent, so arrival order is not fixed; PAIR's store is
	// monotonic (queued < running < terminal) and ignores a late "running".
	frames := c.snapshot()
	methods := map[any]bool{}
	for _, f := range frames {
		methods[f["method"]] = true
	}
	// The run reported no progress, so the card never turned running.
	if len(frames) != 2 || !methods["workload:submitted"] || !methods["workload:errored"] {
		t.Fatalf("frames delivered by return = %d %v, want submitted + errored", len(frames), frames)
	}
}

// framesByMethod indexes frames by JSON-RPC method (one frame per method).
func framesByMethod(frames []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, f := range frames {
		if m, ok := f["method"].(string); ok {
			out[m] = f
		}
	}
	return out
}

// TestSeatWorking pins when a card may read "running": the seat is serving
// the request, never while the run is admitting or its seat is loading.
func TestSeatWorking(t *testing.T) {
	now := time.Now()
	ago := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
	cases := []struct {
		name  string
		p     *core.LiveProgress
		since time.Time
		want  bool
	}{
		{"admission", &core.LiveProgress{Phase: "admission", LastProgressMs: ago(time.Minute)}, now.Add(-time.Minute), false},
		{"cold-load, however long", &core.LiveProgress{Phase: "cold-load", LastProgressMs: ago(3 * time.Minute)}, now.Add(-3 * time.Minute), false},
		{"fresh prefill may still be a load", &core.LiveProgress{Phase: "prefill", LastProgressMs: ago(2 * time.Second)}, time.Time{}, false},
		{"prefill past the grace", &core.LiveProgress{Phase: "prefill", LastProgressMs: ago(pairPrefillGrace + time.Second)}, time.Time{}, true},
		{"decoding", &core.LiveProgress{Phase: "decoding"}, time.Time{}, true},
		{"tool", &core.LiveProgress{Phase: "tool"}, time.Time{}, true},
		{"tokens already streamed", &core.LiveProgress{Phase: "cold-load", TokensOut: 12}, time.Time{}, true},
		{"no progress yet, just started", nil, now.Add(-time.Second), false},
		{"no progress ever (old node)", nil, now.Add(-pairNoProgressGrace - time.Second), true},
		{"no progress, not started", nil, time.Time{}, false},
	}
	for _, tc := range cases {
		if got := seatWorking(tc.p, tc.since, now); got != tc.want {
			t.Errorf("%s: seatWorking = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestLocalColdLoadStaysQueued: a local run that only ever admitted and
// loaded its seat never reads running (the 2026-09-23 "Running" card over a
// 0 % card), and its terminal frame carries no start.
func TestLocalColdLoadStaysQueued(t *testing.T) {
	pairAppDir(t)
	c := &pairCapture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	cfg := testCfg(t)
	cfg.PairWorkloadsEnabled = true
	cfg.PairWorkloadsEndpoint = srv.URL
	local := LocalRunner(func(ctx context.Context, ac core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		now := time.Now().UnixMilli()
		core.ReportProgress(ctx, core.LiveProgress{Phase: "admission", LastProgressMs: now})
		core.ReportProgress(ctx, core.LiveProgress{Phase: "cold-load", LastProgressMs: now})
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Deferred: true, Reason: "stalled: seat still loading"}, nil
	})
	if _, _, err := RunWith(context.Background(), cfg, local, []core.AgentContract{{Goal: "say done"}}, "local", nil, nil); err != nil {
		t.Fatal(err)
	}
	frames := c.snapshot()
	byMethod := framesByMethod(frames)
	if len(frames) != 2 || byMethod["workload:submitted"] == nil || byMethod["workload:errored"] == nil {
		t.Fatalf("frames = %v, want queued + errored and no running", frames)
	}
	if info := pairInfo(byMethod["workload:errored"]); info["startedAt"] != nil {
		t.Fatalf("a run that never worked must carry no start: %v", info)
	}
}

// TestRemoteCardTurnsRunningOnWork: past the node's ack the card stays
// queued while the node reports admission / cold-load, and turns running on
// the first poll that shows the seat working.
func TestRemoteCardTurnsRunningOnWork(t *testing.T) {
	for _, tc := range []struct {
		name        string
		workAt      int64 // poll number that first reports decoding; 0 = never
		wantStarted bool
	}{
		{"loads then decodes", 3, true},
		{"only ever loading", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compressPolls(t, 5*time.Millisecond, time.Second)
			pairAppDir(t)
			c := &pairCapture{}
			pairSrv := httptest.NewServer(http.HandlerFunc(c.handler))
			defer pairSrv.Close()
			node := &fakeNode{
				t: t, token: "sekrit", agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
				pollState: func(n int64) (map[string]any, int) {
					if n >= 5 {
						return doneWire(t, remoteWire("the qube answer", `{"answer":"42"}`)), http.StatusOK
					}
					phase := "cold-load"
					if n <= 1 {
						phase = "admission"
					}
					if tc.workAt > 0 && n >= tc.workAt {
						phase = "decoding"
					}
					return map[string]any{"state": "running", "progress": map[string]any{
						"phase": phase, "last_progress_ms": time.Now().UnixMilli(), "allowance_ms": 60000}}, http.StatusOK
				},
			}
			srv := node.server()
			cfg := testCfg(t)
			cfg.FleetAuthToken = "sekrit"
			cfg.PairWorkloadsEnabled = true
			cfg.PairWorkloadsEndpoint = pairSrv.URL
			if _, _, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{srv.URL}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			frames := c.snapshot()
			byMethod := framesByMethod(frames)
			if byMethod["workload:submitted"] == nil || byMethod["workload:completed"] == nil {
				t.Fatalf("frames = %v, want queued and completed", frames)
			}
			started := byMethod["workload:started"] != nil
			if started != tc.wantStarted {
				t.Fatalf("running frame sent = %v, want %v: %v", started, tc.wantStarted, frames)
			}
			done := pairInfo(byMethod["workload:completed"])
			if (done["startedAt"] != nil) != tc.wantStarted {
				t.Fatalf("terminal startedAt = %v, want set only when the seat worked", done["startedAt"])
			}
		})
	}
}
