package mcpserver

// agentrun_upstream_fence_test.go: the MCP agent_run door under a GPU lease taken
// AFTER its cordon (2026-09-22). The warm-up and the served-window probe reach
// llama-swap's /upstream/<seat>/…, which starts a seat that is not loaded; under a
// media lease over a cold seat neither may be sent, and the run defers `capacity`
// the way the delegation door does.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

func TestAgentRunUnderALateLeaseLoadsNothingAndDefersCapacity(t *testing.T) {
	const seat = "agent-pool"
	root := t.TempDir()
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	if err := modelaffinity.SetGPULease("", root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = modelaffinity.SetGPULease("", filepath.Join(t.TempDir(), "My Drive", "x")) })

	var once sync.Once
	var upstream, atTaking, chats atomic.Int64
	take := func() {
		once.Do(func() {
			atTaking.Store(upstream.Load())
			l, lerr := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "video render", TTL: time.Hour})
			if lerr != nil {
				t.Errorf("acquire the render's lease: %v", lerr)
				return
			}
			t.Cleanup(func() { _ = l.Release() })
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/running":
			take() // the render starts while this run is being admitted
			fmt.Fprint(w, `{"running":[]}`)
		case r.URL.Path == "/v1/models":
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model"}]}`, seat)
		case strings.HasPrefix(r.URL.Path, "/upstream/"):
			upstream.Add(1)
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"max_model_len":65536}]}`, seat)
		case r.URL.Path == "/v1/chat/completions":
			chats.Add(1)
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The answer is 42."},"finish_reason":"stop"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Home = t.TempDir()
	cfg.StateDir = root
	cfg.Endpoint = srv.URL
	cfg.Model = seat
	cfg.AgentModel = seat
	cfg.AgentAdmissionWaitSec = 2
	s := New(pipeline.New(cfg, nil, nil, nil))

	start := time.Now()
	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":2,"timeout_sec":60}`, t.TempDir())))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	// The defer belongs to ADMISSION (a 2 s budget here): a run that started its
	// loop would instead spend its 60 s wall waiting at the first completion.
	if el := time.Since(start); el > 15*time.Second {
		t.Errorf("deferred after %s: the window probe must defer inside the admission budget, before any wall", el)
	}
	out := decodeResult(t, res)
	if got := upstream.Load() - atTaking.Load(); got != 0 {
		t.Fatalf("%d request(s) reached /upstream after the render took the card — each one starts the seat on it", got)
	}
	if out["deferred"] != true || out["defer_class"] != string(core.DeferClassCapacity) {
		t.Fatalf("want a capacity defer, got %v", out)
	}
	if reason, _ := out["reason"].(string); !strings.Contains(reason, "gpu busy") || !strings.Contains(reason, "video render") {
		t.Errorf("reason must name the held card and the render: %v", out["reason"])
	}
	if chats.Load() != 0 {
		t.Errorf("the loop dialled the seat %d time(s) behind the fence", chats.Load())
	}
}
