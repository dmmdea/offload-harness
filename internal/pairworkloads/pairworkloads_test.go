package pairworkloads

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

func writePairAppDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "node-id.json"), []byte(`{"node_uuid":"self-uuid","created_at":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "cluster"), 0o755); err != nil {
		t.Fatal(err)
	}
	members := `[{"id":"node-b","nodeUuid":"node-b-uuid","name":"node-b"},{"id":"node-a","nodeUuid":"self-uuid","name":"node-a"}]`
	if err := os.WriteFile(filepath.Join(dir, "cluster", "members.json"), []byte(members), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

type capture struct {
	mu     sync.Mutex
	frames []map[string]any
}

func (c *capture) handler(w http.ResponseWriter, r *http.Request) {
	var f map[string]any
	_ = json.NewDecoder(r.Body).Decode(&f)
	c.mu.Lock()
	c.frames = append(c.frames, f)
	c.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (c *capture) info(i int) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.frames[i]["params"].(map[string]any)["workloadInfo"].(map[string]any)
}

func (c *capture) method(i int) any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.frames[i]["method"]
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.frames)
}

func TestEngineFor(t *testing.T) {
	cases := []struct{ task, seat, want string }{
		{"agent_delegate", "qwen3.5-9b-agent", "llamacpp"},
		{"agent_delegate", "qwen3.5-4b-vllm", "vllm"},
		{"agent_delegate", "qwen38-27b-gsq-vllm", "vllm"},
		{"agent_delegate", "agent-pool", "llamacpp"},
		{"transcribe", "whisper-stt", "whispercpp"},
		{"transcribe", "", "whispercpp"},
		{"generate_image", "", "comfyui"},
		{"generate_video", "", "comfyui"},
		{"edit_image_generative", "", "comfyui"},
		{"run_graph", "", "comfyui"},
		{"summarize", "gemma-4-e4b", "llamacpp"},
		{"vqa", "qwen3-vl-8b", "llamacpp"},
	}
	for _, c := range cases {
		if got := EngineFor(c.task, c.seat); got != c.want {
			t.Errorf("EngineFor(%q,%q) = %q, want %q", c.task, c.seat, got, c.want)
		}
	}
}

func TestMethodFor(t *testing.T) {
	for state, want := range map[string]string{"queued": "workload:submitted", "running": "workload:started", "completed": "workload:completed", "failed": "workload:errored", "bogus": "workload:errored"} {
		if got := MethodFor(state); got != want {
			t.Errorf("MethodFor(%q) = %q, want %q", state, got, want)
		}
	}
}

func TestFromConfigDefaults(t *testing.T) {
	c := FromConfig(config.Config{PairWorkloadsEnabled: true})
	if !c.Enabled || c.Endpoint != DefaultEndpoint {
		t.Fatalf("FromConfig = %+v", c)
	}
	c = FromConfig(config.Config{PairWorkloadsEndpoint: " http://127.0.0.1:9/x "})
	if c.Enabled || c.Endpoint != "http://127.0.0.1:9/x" {
		t.Fatalf("FromConfig = %+v", c)
	}
}

func TestSendResolvesNodesAndShape(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	e := New(Config{Enabled: true, Endpoint: srv.URL, AppDir: writePairAppDir(t)})
	if !e.Enabled() {
		t.Fatal("emitter should be enabled")
	}
	err := e.Send(context.Background(), Event{JobID: "agd-1", Model: "qwen3.5-9b-agent", Engine: "llamacpp", Node: "NODE-B", State: "running", CreatedAt: 1000, StartedAt: 1000, Requester: "offload-harness/s1"})
	if err != nil {
		t.Fatal(err)
	}
	if c.method(0) != "workload:started" {
		t.Fatalf("method = %v", c.method(0))
	}
	wi := c.info(0)
	if wi["originatedFrom"] != "self-uuid" || wi["scheduledOn"] != "node-b-uuid" || wi["runId"] != "agd-1" || wi["id"] != "agd-1" {
		t.Fatalf("bad workloadInfo: %v", wi)
	}
	if wi["completedAt"] != nil || wi["error"] != nil {
		t.Fatalf("nullable fields must be null: %v", wi)
	}
	if wi["requesterId"] != "offload-harness/s1" || wi["createdAt"].(float64) != 1000 || wi["startedAt"].(float64) != 1000 {
		t.Fatalf("bad attribution/timestamps: %v", wi)
	}
	// Local node ("") schedules on self; an unknown node name also falls back to self.
	if err := e.Send(context.Background(), Event{JobID: "led-2", Model: "gemma-4-e4b", Engine: "llamacpp", State: "completed", CreatedAt: 1, StartedAt: 1, CompletedAt: 2}); err != nil {
		t.Fatal(err)
	}
	wi = c.info(1)
	if wi["scheduledOn"] != "self-uuid" || c.method(1) != "workload:completed" || wi["completedAt"].(float64) != 2 {
		t.Fatalf("local terminal frame wrong: %v %v", c.method(1), wi)
	}
	if err := e.Send(context.Background(), Event{JobID: "x", Model: "m", Engine: "llamacpp", Node: "no-such-node", State: "failed", Error: "boom", CreatedAt: 1, CompletedAt: 3}); err != nil {
		t.Fatal(err)
	}
	wi = c.info(2)
	if wi["scheduledOn"] != "self-uuid" || wi["error"] != "boom" || c.method(2) != "workload:errored" {
		t.Fatalf("failed frame wrong: %v %v", c.method(2), wi)
	}
}

func TestDisabledWithoutPairInstall(t *testing.T) {
	e := New(Config{Enabled: true, Endpoint: "http://127.0.0.1:1", AppDir: t.TempDir()})
	if e.Enabled() {
		t.Fatal("no node-id.json must disable the emitter")
	}
	if err := e.Send(context.Background(), Event{JobID: "x"}); err != nil {
		t.Fatalf("disabled Send must be a silent no-op, got %v", err)
	}
	e.Emit(Event{JobID: "x"})
	e.Wait()
	off := New(Config{Enabled: false, Endpoint: "http://127.0.0.1:1", AppDir: writePairAppDir(t)})
	if off.Enabled() {
		t.Fatal("config off must disable the emitter even when PAIR is installed")
	}
}

func TestEmitSurvivesUnreachableEndpoint(t *testing.T) {
	e := New(Config{Enabled: true, Endpoint: "http://127.0.0.1:1/v1/workloads/events", AppDir: writePairAppDir(t)})
	e.Emit(Event{JobID: "x", Model: "m", Engine: "llamacpp", State: "completed", CreatedAt: 1, CompletedAt: 2})
	e.Emit(Event{JobID: "y", Model: "m", Engine: "llamacpp", State: "completed", CreatedAt: 1, CompletedAt: 2})
	e.Wait() // must return; a hang here is the defect this test exists for
}

func TestLedgerObserverEmitsTerminalRows(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	e := New(Config{Enabled: true, Endpoint: srv.URL, AppDir: writePairAppDir(t)})
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	e.AttachLedger(l)
	rows := []ledger.Entry{
		{TS: 100, Task: "summarize", ModelTier: "gemma-4-e4b", LatencyMs: 2500},
		{TS: 101, Task: "agent_delegate", ModelTier: "node-b:qwen3.5-9b-agent", JobID: "agd-9"}, // the runner emits these
		{TS: 102, Task: "agent", ModelTier: "qwen3.5-9b-agent"},                                 // node-side row of someone else's job
		{TS: 103, Task: "classify", ModelTier: "gemma-4-e2b", CacheHit: true},                   // no GPU work
		{TS: 104, Task: "transcribe", ModelTier: "whisper-stt", LatencyMs: 900, Deferred: true, Reason: "timeout"},
	}
	for _, r := range rows {
		if err := l.Record(r); err != nil {
			t.Fatal(err)
		}
	}
	e.Wait()
	if c.count() != 2 {
		t.Fatalf("frames = %d, want 2", c.count())
	}
	// Order of background sends is not fixed; find each by model.
	var byModel = map[string]int{}
	for i := 0; i < c.count(); i++ {
		byModel[c.info(i)["model"].(string)] = i
	}
	i, ok := byModel["gemma-4-e4b"]
	if !ok {
		t.Fatalf("summarize frame missing")
	}
	wi := c.info(i)
	if wi["engine"] != "llamacpp" || wi["createdAt"].(float64) != 97500 || wi["completedAt"].(float64) != 100000 || wi["state"] != "completed" || c.method(i) != "workload:completed" {
		t.Fatalf("summarize frame wrong: %v %v", c.method(i), wi)
	}
	j, ok := byModel["whisper-stt"]
	if !ok {
		t.Fatalf("transcribe frame missing")
	}
	wi = c.info(j)
	if c.method(j) != "workload:errored" || wi["engine"] != "whispercpp" || wi["error"] != "timeout" || wi["state"] != "failed" {
		t.Fatalf("deferred row must be errored: %v %v", c.method(j), wi)
	}
	if !contains(wi["requesterId"], "offload-harness") {
		t.Fatalf("requesterId must name the harness: %v", wi["requesterId"])
	}
}

func contains(v any, s string) bool {
	str, ok := v.(string)
	return ok && len(str) >= len(s) && str[:len(s)] == s
}
