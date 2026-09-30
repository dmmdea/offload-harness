package mcpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// The agent_run door waits at the local seat cap by the same rule as the
// delegation door (register C-60): in line for as long as the run's own wall,
// not the admission budget. Cap 1, the slot held by another registered run
// for good, a 1 s admission budget and a 3 s wall: the run defers as capacity
// after about 3 s, naming the cap, without one chat request.
func TestAgentRunWaitsAtTheSeatCapForItsWall(t *testing.T) {
	var chats atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/running":
			fmt.Fprint(w, `{"running":[{"model":"agent-pool","state":"ready","cmd":"y"}]}`)
		case "/v1/models":
			fmt.Fprint(w, `{"object":"list","data":[{"id":"agent-pool","object":"model"}]}`)
		case "/v1/chat/completions":
			chats.Add(1)
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The answer is 42."},"finish_reason":"stop"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	root := t.TempDir()
	if err := modelaffinity.SetGPULease("", root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = modelaffinity.SetGPULease("", filepath.Join(t.TempDir(), "My Drive", "x")) })
	reg, err := gpuactivity.Open("", root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := reg.Begin(gpuactivity.Run{Seat: "agent-pool", Kind: "contract"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.End)

	cfg := config.Default()
	cfg.Home = t.TempDir()
	cfg.StateDir = root
	cfg.Endpoint = srv.URL
	cfg.Model = "agent-pool"
	cfg.AgentModel = "agent-pool"
	cfg.AgentAdmissionWaitSec = 1
	cfg.FleetMaxConcurrentJobs = 1
	s := New(pipeline.New(cfg, nil, nil, nil))

	start := time.Now()
	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":2,"timeout_sec":3}`, t.TempDir())))
	spent := time.Since(start)
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	reason, _ := m["reason"].(string)
	if m["deferred"] != true || !strings.Contains(reason, "seat busy") || !strings.Contains(reason, "cap 1") {
		t.Fatalf("a full seat must defer naming the cap: %v", m)
	}
	if n := chats.Load(); n != 0 {
		t.Fatalf("%d chat requests reached the engine while the seat was at its cap", n)
	}
	if spent < 2500*time.Millisecond || spent > 10*time.Second {
		t.Fatalf("the wait in line must be the run's 3 s wall, not the 1 s admission budget: %s", spent)
	}
	if note, _ := m["admission_note"].(string); !strings.Contains(note, "seat cap") {
		t.Errorf("admission_note = %q, want the seat cap named", note)
	}
}
