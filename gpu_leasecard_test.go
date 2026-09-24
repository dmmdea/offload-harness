package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

func TestLeaseCardIdentity(t *testing.T) {
	cases := []struct {
		args          []string
		model, engine string
	}{
		{[]string{"./local-offload", "generate-video", "out.mp4", "in.png", "a prompt"}, "generate-video", "comfyui"},
		{[]string{`D:\offload-stack\bin\local-offload.exe`, "run-graph", "--graph", "g.json"}, "run-graph", "comfyui"},
		{[]string{"offload-harness.exe", "transcribe", "a.wav"}, "transcribe", "whispercpp"},
		{[]string{"C:/Windows/System32/WindowsPowerShell/v1.0/powershell.exe", "-NoProfile", "-File", "D:/Temp/seatbench.ps1", "-Ctx", "65536"}, "seatbench.ps1", "gpu-lease"},
		{[]string{`C:\ComfyUI\.venv\Scripts\python.exe`, `D:\offload-stack\bin\acestep_diag.py`}, "acestep_diag.py", "gpu-lease"},
		{[]string{"/opt/llama-vulkan/llama-bench", "-m", "x.gguf"}, "llama-bench", "gpu-lease"},
		{nil, "gpu-lease", "gpu-lease"},
	}
	for _, tc := range cases {
		m, e := leaseCardIdentity(tc.args)
		if m != tc.model || e != tc.engine {
			t.Errorf("leaseCardIdentity(%q) = %q/%q, want %q/%q", tc.args, m, e, tc.model, tc.engine)
		}
	}
}

// The lease card goes queued -> running -> closed under one identity, and a
// failed command closes it failed with the reason.
func TestLeaseCardLifecycle(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "node-id.json"), []byte(`{"node_uuid":"self-uuid","created_at":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_PAIR_APPDIR", dir)
	var mu sync.Mutex
	var frames []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var f map[string]any
		_ = json.NewDecoder(r.Body).Decode(&f)
		mu.Lock()
		frames = append(frames, f)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cfg := config.Config{PairWorkloadsEnabled: true, PairWorkloadsEndpoint: srv.URL}

	card := newLeaseCard(cfg, []string{"./local-offload", "generate-video", "o.mp4"}, "ai-ecosystem-test")
	if card == nil {
		t.Fatal("an enabled box must open a lease card")
	}
	card.running()
	card.finish(errors.New("exit status 3 (lease lost)"))
	card.finish(nil) // closed once
	mu.Lock()
	defer mu.Unlock()
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want queued, running, failed", len(frames))
	}
	by := map[string]map[string]any{}
	for _, f := range frames {
		by[f["method"].(string)] = f["params"].(map[string]any)["workloadInfo"].(map[string]any)
	}
	q, r, x := by["workload:submitted"], by["workload:started"], by["workload:errored"]
	if q == nil || r == nil || x == nil {
		t.Fatalf("methods wrong: %v", frames)
	}
	for _, k := range []string{"id", "runId", "engine", "model", "createdAt"} {
		if q[k] != r[k] || r[k] != x[k] {
			t.Fatalf("identity field %q differs: %v / %v / %v", k, q[k], r[k], x[k])
		}
	}
	if q["model"] != "generate-video" || q["engine"] != "comfyui" || x["error"] != "exit status 3 (lease lost)" ||
		x["startedAt"] != r["startedAt"] || x["requesterId"] != "offload-harness/ai-ecosystem-test" {
		t.Fatalf("card content wrong: queued %v failed %v", q, x)
	}

	off := newLeaseCard(config.Config{}, []string{"x"}, "")
	off.running()
	off.finish(nil) // a nil card is inert
	if off != nil {
		t.Fatal("reporting off must open no card")
	}
}

// Only a one-shot harness verb run directly is silenced under the lease card;
// a session (MCP, fleet node) or a shell keeps reporting its own calls.
func TestSilencesWrappedOnlyOneShotHarnessVerbs(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"./local-offload", "generate-video", "o.mp4"}, true},
		{[]string{`D:\offload-stack\bin\local-offload.exe`, "--config", "c.json", "run-graph"}, false}, // the harness reads its verb from args[1] only
		{[]string{"offload-harness.exe", "run-graph", "--graph", "g.json"}, true},
		{[]string{"local-offload.exe", "mcp", "--config", "c.json"}, false},
		{[]string{"local-offload", "fleet-serve", "-listen", "x"}, false},
		{[]string{"powershell.exe", "-File", "seatbench.ps1"}, false},
		{[]string{"claude"}, false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := silencesWrapped(tc.args); got != tc.want {
			t.Errorf("silencesWrapped(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}
