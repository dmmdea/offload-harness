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
// placement and checks the PAIR frames: a running frame when the run starts
// and a completed frame when it finishes, both under the SAME id, model and
// engine (PAIR's store keys a card on that identity — two identities would be
// two cards, one stuck running).
func TestLocalPlacementReportsOneCardToPAIR(t *testing.T) {
	pairAppDir(t)
	c := &pairCapture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	cfg := testCfg(t)
	cfg.PairWorkloadsEnabled = true
	cfg.PairWorkloadsEndpoint = srv.URL
	local := LocalRunner(func(ctx context.Context, ac core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
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
	for len(c.snapshot()) < 2 && deadline > 0 {
		deadline--
		waitABit()
	}
	frames := c.snapshot()
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2 (running, completed): %v", len(frames), frames)
	}
	if frames[0]["method"] != "workload:started" || frames[1]["method"] != "workload:completed" {
		t.Fatalf("methods = %v, %v", frames[0]["method"], frames[1]["method"])
	}
	a, b := pairInfo(frames[0]), pairInfo(frames[1])
	for _, k := range []string{"id", "runId", "model", "engine", "originatedFrom", "scheduledOn", "createdAt"} {
		if a[k] != b[k] {
			t.Fatalf("identity field %q differs between frames: %v vs %v", k, a[k], b[k])
		}
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
