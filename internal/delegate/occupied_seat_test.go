// occupied_seat_test.go: an agent seat that is NOT loaded is idle only while
// loading it would push nothing off the cards. On the reference box the
// three-card vLLM seat is opencode's model and shares the agent seat's cards
// and llama-swap set, so a contract dealt to the "idle" agent seat there made
// llama-swap unload the operator's session (2026-10-06). probeLocalBusy now
// asks the seat guard (internal/seatguard, the reading the cascade already
// uses) and reads that box as busy, naming the seat a load would evict; the
// deal then prefers a remote with room exactly as it does for a busy seat.

package delegate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

// occupiedMatrix is the reference box's routing in miniature: the agent seat
// (local-seat, bound as agent-pool) and opencode's seat in ONE mutually
// exclusive set, so loading either unloads the other.
const occupiedMatrix = `
models:
  local-seat:
    aliases: [agent-pool]
  opencode-seat: {}
matrix:
  vars:
    a: local-seat
    o: opencode-seat
  sets:
    interactive: "(a | o)"
`

// occupiedSwap is a llama-swap stand-in whose /running lists `running` (none
// when empty) and whose roster resolves agent-pool to local-seat, so the
// probe reads the agent seat as NOT loaded without any ambiguity.
func occupiedSwap(t *testing.T, running string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/running", func(w http.ResponseWriter, r *http.Request) {
		if running == "" {
			_, _ = w.Write([]byte(`{"running":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"running":[{"model":"` + running + `","state":"ready","proxy":"http://` + r.Host + `/direct/` + running + `"}]}`))
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[` +
			`{"id":"local-seat","object":"model","meta":{"llamaswap":{"aliases":["agent-pool"]}}},` +
			`{"id":"opencode-seat","object":"model"}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// occupiedCfg is testCfg on a box whose agent seat is agent-pool, which declares
// both seats as vLLM seats and serves occupiedMatrix.
func occupiedCfg(t *testing.T, endpoint string) config.Config {
	t.Helper()
	cfg := testCfg(t)
	cfg.Endpoint = endpoint
	cfg.AgentModel = "agent-pool"
	cfg.VLLMSeats = []string{"local-seat", "opencode-seat"}
	path := filepath.Join(t.TempDir(), "llama-swap.yaml")
	if err := os.WriteFile(path, []byte(occupiedMatrix), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.ServingConfigPath = path
	return cfg
}

func TestProbeLocalBusyReadsAnotherLoadedVLLMSeatAsOccupied(t *testing.T) {
	ctx := context.Background()

	rd := (&runner{cfg: occupiedCfg(t, occupiedSwap(t, "opencode-seat"))}).probeLocalBusy(ctx)
	if !rd.busy || rd.occupiedBy != "opencode-seat" || rd.inflight != 0 || rd.unknown {
		t.Fatalf("opencode's seat holds the cards and loading agent-pool would unload it: want busy, occupiedBy opencode-seat; got %+v", rd)
	}
	if why := rd.why(); !strings.Contains(why, "would evict the loaded vLLM seat opencode-seat") {
		t.Fatalf("why() = %q, must name the seat a load would evict", why)
	}

	// Control arms: each must read exactly as before this change (idle, nothing named).
	for name, cfg := range map[string]config.Config{
		"nothing loaded": occupiedCfg(t, occupiedSwap(t, "")),
		"guard off": func() config.Config {
			c := occupiedCfg(t, occupiedSwap(t, "opencode-seat"))
			off := false
			c.CascadeSeatGuard = &off
			return c
		}(),
		"occupant is not a declared vLLM seat": func() config.Config {
			c := occupiedCfg(t, occupiedSwap(t, "opencode-seat"))
			c.VLLMSeats = []string{"local-seat"}
			return c
		}(),
	} {
		got := (&runner{cfg: cfg}).probeLocalBusy(ctx)
		if got.busy || got.occupiedBy != "" {
			t.Errorf("%s: want the idle reading, got %+v", name, got)
		}
		if got.why() != "local seat busy: 0 in flight" {
			t.Errorf("%s: why() = %q, the busy wording must not change for an unoccupied seat", name, got.why())
		}
	}
}

// TestRunAutoDealsAwayFromAnOccupiedLocalSeat is the operator's case through Run:
// route=auto (agent_delegate's default) with opencode's seat loaded and one
// eligible remote runs NOTHING local, and the reason names the seat it spared.
func TestRunAutoDealsAwayFromAnOccupiedLocalSeat(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	node, url := eligibleNode(t, "node-a", "zorblax from A")
	cfg := occupiedCfg(t, occupiedSwap(t, "opencode-seat"))
	var localCalls atomic.Int64
	results, sum, err := Run(context.Background(), cfg, passingLocal(&localCalls), contracts(1), "auto", []string{url})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 1 || localCalls.Load() != 0 || node.dispatches.Load() != 1 {
		t.Fatalf("summary=%+v local=%d remote=%d: the contract must run on the remote, never load the agent seat over opencode's", sum, localCalls.Load(), node.dispatches.Load())
	}
	if r := results[0].PlacementReason; !strings.Contains(r, "would evict the loaded vLLM seat opencode-seat") {
		t.Fatalf("placement reason %q must say the local seat was occupied and by what", r)
	}

	// Control: the same box with nothing loaded keeps the idle-local deal.
	node2, url2 := eligibleNode(t, "node-b", "must not be used")
	var localCalls2 atomic.Int64
	_, sum2, err := Run(context.Background(), occupiedCfg(t, occupiedSwap(t, "")), passingLocal(&localCalls2), contracts(1), "auto", []string{url2})
	if err != nil {
		t.Fatal(err)
	}
	if sum2.Succeeded != 1 || localCalls2.Load() != 1 || node2.dispatches.Load() != 0 {
		t.Fatalf("idle box: summary=%+v local=%d remote=%d, want the local seat", sum2, localCalls2.Load(), node2.dispatches.Load())
	}
}

// TestRunSpreadDealsAwayFromAnOccupiedLocalSeat: route=spread skips the local
// rotation slot for the same reason, so two contracts land one per remote.
func TestRunSpreadDealsAwayFromAnOccupiedLocalSeat(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "zorblax from A")
	nodeB, urlB := eligibleNode(t, "node-b", "zorblax from B")
	cfg := occupiedCfg(t, occupiedSwap(t, "opencode-seat"))
	var localCalls atomic.Int64
	results, sum, err := Run(context.Background(), cfg, passingLocal(&localCalls), contracts(2), "spread", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 2 || localCalls.Load() != 0 || nodeA.dispatches.Load() != 1 || nodeB.dispatches.Load() != 1 {
		t.Fatalf("summary=%+v local=%d A=%d B=%d, want 0/1/1", sum, localCalls.Load(), nodeA.dispatches.Load(), nodeB.dispatches.Load())
	}
	for i, pr := range results {
		if !strings.Contains(pr.PlacementReason, "would evict the loaded vLLM seat opencode-seat") {
			t.Errorf("subtask %d: placement reason %q must name the occupant", i, pr.PlacementReason)
		}
	}
}

// TestRunAutoOccupiedWithNoFleetStillRunsLocal pins the last resort: with no
// remote that could take the contract the busy rule never loses work, so the
// contract runs on the local seat (evicting the occupant) and says why.
func TestRunAutoOccupiedWithNoFleetStillRunsLocal(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	cfg := occupiedCfg(t, occupiedSwap(t, "opencode-seat"))
	var localCalls atomic.Int64
	results, sum, err := Run(context.Background(), cfg, passingLocal(&localCalls), contracts(1), "auto", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 1 || localCalls.Load() != 1 {
		t.Fatalf("summary=%+v local=%d: with no fleet the work must still run", sum, localCalls.Load())
	}
	if r := results[0].PlacementReason; !strings.Contains(r, "no eligible remote") {
		t.Fatalf("placement reason %q must say no remote could take it", r)
	}
}

// TestRunOneAutoTreatsAnOccupiedSeatAsBusy covers the per-subtask placement
// (attempt's route=auto read, cached on the runner) beside the joint deal: an
// occupied reading with nothing in flight is busy there too.
func TestRunOneAutoTreatsAnOccupiedSeatAsBusy(t *testing.T) {
	remote, url := eligibleNode(t, "node-a", "remote answered")
	cfg := testCfg(t)
	var localCalls atomic.Int64
	r := &runner{
		cfg: cfg, local: passingLocal(&localCalls), route: "auto", remotes: []string{url},
		intent: openIntentLedger(cfg),
		localBusyProbe: func(context.Context) busyReading {
			return busyReading{busy: true, occupiedBy: "opencode-seat", note: "local seat not loaded; would evict"}
		},
	}
	pr := r.runOne(context.Background(), 0, remoteContract())
	if localCalls.Load() != 0 || remote.dispatches.Load() != 1 || pr.Node != "node-a" {
		t.Fatalf("local=%d remote=%d node=%q: an occupied seat must send the subtask to the remote", localCalls.Load(), remote.dispatches.Load(), pr.Node)
	}
	if !strings.Contains(pr.PlacementReason, "would evict the loaded vLLM seat opencode-seat") {
		t.Fatalf("placement reason %q must name the occupant", pr.PlacementReason)
	}
}
