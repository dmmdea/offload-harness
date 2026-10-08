package delegate

// LocalSeatBusy is the delegator's "is this local seat busy" reading offered to the lane doors (ADR 0078): the vision
// lane's auto route used to consult the machine-wide GPU lease alone, so a seat serving other requests with no lease
// anywhere still queued the next image behind them. These tests pin the reading against a fake llama-swap, that it is
// the SAME reading the delegator makes of its agent seat, and that it fails open.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/seatguard"
)

// stateSwap is a llama-swap stand-in with one seat (run_spread_test.go has busySwap for the always-ready case): /running lists it when loaded (as `starting` while a load is in
// progress), with the seat's own address as `proxy`, where /metrics serves a vLLM-shaped gauge with the in-flight count.
func stateSwap(t *testing.T, id string, loaded, starting bool, inflight int) string {
	t.Helper()
	url, _ := countedSwap(t, id, loaded, starting, inflight)
	return url
}

// countedSwap is stateSwap that also counts every request the reading makes to it.
func countedSwap(t *testing.T, id string, loaded, starting bool, inflight int) (string, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"` + id + `"}]}`))
	})
	mux.HandleFunc("/running", func(w http.ResponseWriter, r *http.Request) {
		var running []map[string]string
		if loaded {
			state := "ready"
			if starting {
				state = "starting"
			}
			running = append(running, map[string]string{"model": id, "state": state, "proxy": "http://" + r.Host + "/direct/" + id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"running": running})
	})
	mux.HandleFunc("/direct/"+id+"/metrics", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "vllm:num_requests_running{engine=\"0\"} %d\nvllm:num_requests_waiting{engine=\"0\"} 0.0\n", inflight)
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &hits
}

func noOccupant(context.Context, string) seatguard.Verdict { return seatguard.Verdict{} }

func TestLocalSeatBusyReadsTheNamedSeatsOwnLoad(t *testing.T) {
	for _, tc := range []struct {
		name             string
		loaded, starting bool
		inflight         int
		seat             string
		wantBusy         bool
		wantWhy          string
	}{
		{"a seat that is not loaded is idle", false, false, 0, "vlm", false, ""},
		{"a loaded seat with nothing in flight is idle", true, false, 0, "vlm", false, ""},
		{"a request in flight is busy", true, false, 1, "vlm", true, "1 in flight"},
		{"several requests in flight are busy", true, false, 3, "vlm", true, "3 in flight"},
		{"a load in progress is busy", true, true, 0, "vlm", true, "a load or unload is in progress"},
		{"another seat's load is not this seat's", true, false, 3, "some-other-seat", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{Endpoint: stateSwap(t, "vlm", tc.loaded, tc.starting, tc.inflight)}
			got := localSeatBusy(t.Context(), cfg, tc.seat, "vision seat", noOccupant)
			if got.Busy != tc.wantBusy || got.Why != tc.wantWhy {
				t.Fatalf("reading = %+v, want busy=%v why=%q", got, tc.wantBusy, tc.wantWhy)
			}
		})
	}
}

// TestLocalSeatBusyNamesTheSeatALoadWouldEvict: an unloaded seat whose load would unload another loaded vLLM seat is as
// unavailable as a busy one (the delegator's rule since 0.165.2: the operator's own seat is never evicted for a call).
func TestLocalSeatBusyNamesTheSeatALoadWouldEvict(t *testing.T) {
	cfg := config.Config{Endpoint: stateSwap(t, "vlm", false, false, 0)}
	guard := func(_ context.Context, model string) seatguard.Verdict {
		if model != "vlm" {
			t.Errorf("guard asked about %q, want the seat the call would load", model)
		}
		return seatguard.Verdict{Protect: true, Seat: "other-vllm-seat", Reason: "it holds the cards"}
	}
	got := localSeatBusy(t.Context(), cfg, "vlm", "vision seat", guard)
	if !got.Busy || !strings.Contains(got.Why, "loading it would evict the loaded vLLM seat other-vllm-seat") {
		t.Fatalf("reading = %+v, want busy with the occupant named", got)
	}
}

// TestLocalSeatBusyFailsOpenToIdle: the reading is a reason to place a call elsewhere, never a gate. No seat, no
// endpoint, and an endpoint that does not answer all read idle, so the call runs where it always ran.
func TestLocalSeatBusyFailsOpenToIdle(t *testing.T) {
	live, hits := countedSwap(t, "vlm", true, false, 4)
	if got := LocalSeatBusy(t.Context(), config.Config{Endpoint: live}, "", "vision seat"); got.Busy {
		t.Errorf("an empty seat read busy: %+v", got)
	}
	if got := LocalSeatBusy(t.Context(), config.Config{Endpoint: live}, "  ", "vision seat"); got.Busy {
		t.Errorf("a blank seat read busy: %+v", got)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("llama-swap was asked %d time(s) about a seat with no name: there is nothing to read", n)
	}
	if got := LocalSeatBusy(t.Context(), config.Config{}, "vlm", "vision seat"); got.Busy {
		t.Errorf("a box with no endpoint read busy: %+v", got)
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	if got := LocalSeatBusy(t.Context(), config.Config{Endpoint: url}, "vlm", "vision seat"); got.Busy {
		t.Errorf("an endpoint that does not answer read busy: %+v", got)
	}
}

// TestTheLaneReadingIsTheReadingTheDelegatorMakesOfItsAgentSeat: probeLocalBusy and the lane doors' LocalSeatBusy go
// through one probeSeatBusy, so for the same seat on the same llama-swap they agree on busy and on the count.
func TestTheLaneReadingIsTheReadingTheDelegatorMakesOfItsAgentSeat(t *testing.T) {
	for _, inflight := range []int{0, 1, 4} {
		endpoint := stateSwap(t, "seat-x", true, false, inflight)
		cfg := config.Config{Endpoint: endpoint, AgentModel: "seat-x"}
		r := &runner{cfg: cfg, seatGuardCheck: noOccupant}
		agent := r.probeLocalBusy(t.Context())
		lane := localSeatBusy(t.Context(), cfg, "seat-x", "vision seat", noOccupant)
		if agent.busy != lane.Busy || (lane.Busy && lane.Why != fmt.Sprintf("%d in flight", agent.inflight)) {
			t.Errorf("%d in flight: the delegator read busy=%v inflight=%d, the lane door read %+v", inflight, agent.busy, agent.inflight, lane)
		}
	}
}

// TestTheExportedLaneReadingNamesTheVLLMSeatALoadWouldEvict: the occupant branch was tested only through the unexported
// localSeatBusy with an injected guard, so the production wiring behind LocalSeatBusy (seatguard.Shared(cfg), the call
// visionremote makes) could be replaced by a guard that never protects anything with every test green, and the vision lane
// would load its seat over a loaded vLLM seat again and evict the operator's session. This calls the exported function
// against a box whose declared vLLM seat holds the cards (occupiedCfg: the agent seat and another seat in one mutually
// exclusive set), with the controls that read idle: nothing loaded, the guard switched off, and an occupant that is not a
// declared vLLM seat.
func TestTheExportedLaneReadingNamesTheVLLMSeatALoadWouldEvict(t *testing.T) {
	got := LocalSeatBusy(t.Context(), occupiedCfg(t, occupiedSwap(t, "opencode-seat")), "agent-pool", "vision seat")
	if !got.Busy || !strings.Contains(got.Why, "loading it would evict the loaded vLLM seat opencode-seat") {
		t.Fatalf("reading = %+v, want busy with the occupant named: the production guard must be the one asked", got)
	}
	for name, cfg := range map[string]config.Config{
		"nothing is loaded": occupiedCfg(t, occupiedSwap(t, "")),
		"the seat guard is switched off": func() config.Config {
			c := occupiedCfg(t, occupiedSwap(t, "opencode-seat"))
			off := false
			c.CascadeSeatGuard = &off
			return c
		}(),
		"the occupant is not a declared vLLM seat": func() config.Config {
			c := occupiedCfg(t, occupiedSwap(t, "opencode-seat"))
			c.VLLMSeats = []string{"local-seat"}
			return c
		}(),
	} {
		if got := LocalSeatBusy(t.Context(), cfg, "agent-pool", "vision seat"); got.Busy || got.Why != "" {
			t.Errorf("%s: reading = %+v, want idle", name, got)
		}
	}
}
