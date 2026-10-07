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
	"github.com/dmmdea/offload-harness/internal/core"
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
	return occupiedSwapRoster(t, running, true)
}

// occupiedSwapRoster is occupiedSwap with the roster (/v1/models) answering 503
// when rosterOK is false: the probe then cannot resolve agent-pool, the case the
// seat guard must still answer from the serving config.
func occupiedSwapRoster(t *testing.T, running string, rosterOK bool) string {
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
		if !rosterOK {
			http.Error(w, "roster unavailable", http.StatusServiceUnavailable)
			return
		}
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
	if r := results[0].PlacementReason; !strings.Contains(r, "no eligible remote") || !strings.Contains(r, "would evict the loaded vLLM seat opencode-seat") || !strings.Contains(r, "so the occupant is unloaded") {
		t.Fatalf("placement reason %q must say no remote could take it AND that the occupant is unloaded", r)
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

// TestProbeLocalBusyAsksTheGuardWhenTheRosterIsUnreadable: a failed roster read
// used to return "ambiguous" before the guard was asked, so an occupied box
// dealt local. The guard resolves names from the serving config, so it answers.
// And when the agent seat is itself the one running (listed under its id while
// the box binds its alias), the guard names nothing and the reading stays the
// old ambiguous-idle one.
func TestProbeLocalBusyAsksTheGuardWhenTheRosterIsUnreadable(t *testing.T) {
	ctx := context.Background()
	rd := (&runner{cfg: occupiedCfg(t, occupiedSwapRoster(t, "opencode-seat", false))}).probeLocalBusy(ctx)
	if !rd.busy || rd.occupiedBy != "opencode-seat" {
		t.Fatalf("roster unreadable, opencode's seat loaded: want occupied by opencode-seat, got %+v", rd)
	}
	own := (&runner{cfg: occupiedCfg(t, occupiedSwapRoster(t, "local-seat", false))}).probeLocalBusy(ctx)
	if own.busy || own.occupiedBy != "" || !own.unknown {
		t.Fatalf("roster unreadable, the agent seat itself running: want the ambiguous idle reading, got %+v", own)
	}
}

// occupiedRunner is a route=auto runner whose local seat reads occupied while
// *occupied is true, against the given remotes.
func occupiedRunner(t *testing.T, cfg config.Config, local LocalRunner, remotes []string, occupied *atomic.Bool) *runner {
	t.Helper()
	return &runner{
		cfg: cfg, local: local, route: "auto", remotes: remotes,
		intent: openIntentLedger(cfg),
		localBusyProbe: func(context.Context) busyReading {
			if occupied.Load() {
				return busyReading{busy: true, occupiedBy: "opencode-seat", note: "local seat not loaded; would evict"}
			}
			return busyReading{note: "local seat not loaded"}
		},
	}
}

// TestReplacementAfterARefusalLeavesAnOccupiedSeatAlone is the review's repro:
// the deal sends the contract to the remote, the remote refuses (503, a full
// fleet), and the re-placement used to fall back to the local seat, unloading
// the occupant. Now the occupied seat is a place in line, like a fence.
func TestReplacementAfterARefusalLeavesAnOccupiedSeatAlone(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, url := refusingNode(t, "node-full", http.StatusServiceUnavailable, nil)
	var occupied atomic.Bool
	occupied.Store(true)
	cfg := testCfg(t) // capacity wait off: the place in line ends at once as a refused placement
	pr := occupiedRunner(t, cfg, neverLocal(t), []string{url}, &occupied).runOne(context.Background(), 0, remoteContract())
	// neverLocal fails the test if the seat ran. With no wait the subtask ends as a refused
	// placement, and the refusal names the occupant as the reason the local seat was no last resort.
	if !strings.Contains(pr.Err, "placement refused") || !strings.Contains(pr.Err, "would evict the loaded vLLM seat opencode-seat") {
		t.Fatalf("err=%q placement=%q result=%+v: want a refused placement naming the occupant", pr.Err, pr.PlacementReason, pr.Result)
	}
}

// TestCapacityWaitTakesTheLocalSeatOnlyOnceTheOccupantLeaves: with the wait on,
// a refused subtask stands in line; the occupied local seat is no candidate, and
// it becomes one the moment the occupant leaves (its reason says so).
func TestCapacityWaitTakesTheLocalSeatOnlyOnceTheOccupantLeaves(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 10*time.Millisecond, 20*time.Millisecond)
	_, url := refusingNode(t, "node-full", http.StatusServiceUnavailable, nil)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 10
	var occupied atomic.Bool
	occupied.Store(true)
	var localCalls atomic.Int64
	local := func(ctx context.Context, c core.AgentContract, o LocalOptions) (core.AgentWireResult, error) {
		if occupied.Load() {
			t.Error("the local seat ran while it was occupied")
		}
		return passingLocal(&localCalls)(ctx, c, o)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		occupied.Store(false)
	}()
	pr := occupiedRunner(t, cfg, local, []string{url}, &occupied).runOne(context.Background(), 0, remoteContract())
	if localCalls.Load() != 1 || pr.Result.Deferred {
		t.Fatalf("local=%d result=%+v: once the occupant left, the waiting subtask must run local", localCalls.Load(), pr.Result)
	}
	if !strings.Contains(pr.PlacementReason, "occupied by the vLLM seat opencode-seat, which left after") {
		t.Fatalf("placement reason %q must say the seat was occupied and the occupant left", pr.PlacementReason)
	}
}

// TestRetryNoteNamesTheOccupant: the verification retry's seat check reports an
// occupied local seat by name, not as an anonymous busy seat.
func TestRetryNoteNamesTheOccupant(t *testing.T) {
	var occupied atomic.Bool
	occupied.Store(true)
	r := occupiedRunner(t, testCfg(t), neverLocal(t), nil, &occupied)
	busy, note := r.retrySeatBusy(context.Background(), placement{})
	if !busy || !strings.Contains(note, "would evict the loaded vLLM seat opencode-seat") {
		t.Fatalf("retrySeatBusy = %v, %q: want busy, naming the occupant", busy, note)
	}
}
