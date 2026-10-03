// agentrun_wallgate_test.go pins the agent_run door's WALL GATE (register D-102).
//
// This door runs its loop under a context deadline: the wall is a hard kill here,
// not the expectation ADR 0055 made it on the delegation door. It also computed no
// sizing at all, so a caller who named a wall the seat could not hold learned it
// from a `context deadline exceeded` after ten steps (2026-09-15). The door now
// sizes the run from the seat's own rate BEFORE it touches the seat, publishes the
// numbers beside every result, and refuses a wall that cannot hold even the
// smallest answer — the INV-5 rider's floor (ADR 0050), the same one the
// delegator applies, never the seat's max-final min_turn_sec.

package mcpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

const wallGateSeat = "agent-pool"

// wallGateServer is a seat that counts EVERY request it receives, whatever the
// path: a refused run must leave no trace on it, not even the roster read.
func wallGateServer(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	return wallGateServerSeats(t, hits, wallGateSeat)
}

// wallGateServerSeats is wallGateServer over a roster of several served models, for
// a call that names a seat other than the box's agent seat: the roster check runs
// after the wall gate, so a test of the gate's own decision needs the named model
// served or the run defers there for a reason that is not the gate's.
func wallGateServerSeats(t *testing.T, hits *atomic.Int64, models ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/running":
			running := make([]string, 0, len(models))
			for _, m := range models {
				running = append(running, fmt.Sprintf(`{"model":%q,"state":"ready","cmd":"x"}`, m))
			}
			fmt.Fprintf(w, `{"running":[%s]}`, strings.Join(running, ","))
		case r.URL.Path == "/v1/models":
			data := make([]string, 0, len(models))
			for _, m := range models {
				data = append(data, fmt.Sprintf(`{"id":%q,"object":"model"}`, m))
			}
			fmt.Fprintf(w, `{"object":"list","data":[%s]}`, strings.Join(data, ","))
		case strings.HasPrefix(r.URL.Path, "/upstream/") && strings.HasSuffix(r.URL.Path, "/v1/models"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/upstream/"), "/v1/models")
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"max_model_len":65536}]}`, id)
		case r.URL.Path == "/v1/chat/completions":
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The answer is 42."},"finish_reason":"stop"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// wallGateServerFor builds the MCP server over a seat whose configured decode
// rate is tokS (agent_seat_tok_s: the stand-in until the rate store has a sample).
func wallGateServerFor(t *testing.T, endpoint string, tokS float64, mutate func(*config.Config)) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Home = t.TempDir()
	cfg.StateDir = t.TempDir()
	cfg.Endpoint = endpoint
	cfg.Model = wallGateSeat
	cfg.AgentModel = wallGateSeat
	cfg.AgentAdmissionWaitSec = 30
	cfg.AgentSeatTokS = tokS
	if mutate != nil {
		mutate(&cfg)
	}
	return New(pipeline.New(cfg, nil, nil, nil))
}

// fullRunSizing is the estimate the door must publish for a run of maxSteps
// steps on a tokS seat, computed here from the seatrate arithmetic directly.
func fullRunSizing(tokS float64, maxSteps, wallSec int) seatrate.Estimate {
	in := seatrate.InputFor(seatrate.SeatPolicy{Seat: wallGateSeat, TokS: tokS, RateSource: "config agent_seat_tok_s"}, core.AgentContract{MaxSteps: maxSteps})
	in.TimeoutSec = wallSec
	return seatrate.Compute(in)
}

// TestAgentRunRefusesAWallTheSeatCannotHoldBeforeItTouchesTheSeat: a 5 tok/s seat
// needs a 128-token tool step plus its 6 s prefill and a 64-token answer — 45 s —
// for the smallest run that can answer at all. A 20 s wall cannot hold that, so
// the call is refused with the numbers and the seat sees NOTHING.
func TestAgentRunRefusesAWallTheSeatCannotHoldBeforeItTouchesTheSeat(t *testing.T) {
	var hits atomic.Int64
	srv := wallGateServer(t, &hits)
	s := wallGateServerFor(t, srv.URL, 5, nil)

	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"audit the repo","read_root":%q,"max_steps":12,"timeout_sec":20}`, t.TempDir())))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true {
		t.Fatalf("a wall under the smallest answer's floor must be refused, got %v", m)
	}
	if m["defer_class"] != core.DeferClassBudget {
		t.Errorf("defer_class = %v, want %q (the sizing signal)", m["defer_class"], core.DeferClassBudget)
	}
	if steps, _ := m["steps"].(float64); steps != 0 {
		t.Errorf("steps = %v, want 0: the refusal comes before step 1", m["steps"])
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the seat received %d request(s), want 0: a refused run must not load, probe or dial the seat", n)
	}
	est := fullRunSizing(5, 12, 20)
	reason, _ := m["reason"].(string)
	// the wall it was given, the least wall that holds one answer, what the whole
	// run is estimated to need, the published one-turn figure, the argument to change
	for _, want := range []string{
		"20 s",
		"45 s",
		fmt.Sprintf("%d s", est.TotalSec),
		fmt.Sprintf("min_turn %d s", est.MinTurnSec),
		"timeout_sec",
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason lacks %q: %s", want, reason)
		}
	}
	if got, _ := m["wall_estimate_sec"].(float64); int(got) != est.TotalSec {
		t.Errorf("wall_estimate_sec = %v, want %d", m["wall_estimate_sec"], est.TotalSec)
	}
	if got, _ := m["min_turn_sec"].(float64); int(got) != est.MinTurnSec {
		t.Errorf("min_turn_sec = %v, want %d", m["min_turn_sec"], est.MinTurnSec)
	}
	if note, _ := m["wall_note"].(string); !strings.Contains(note, "BELOW the estimate") {
		t.Errorf("wall_note = %q, want the estimate's own arithmetic", note)
	}
	// The floor is one tool step and a minimal final whatever max_steps says
	// (seatrate.MinViableSec fixes the step count), so fewer steps cannot clear
	// this refusal and the reason must not suggest it: a caller who follows the
	// hint would be refused again (review of the D-102 fix).
	if strings.Contains(reason, "max_steps") {
		t.Errorf("the refusal must not suggest max_steps, which never lowers the floor: %s", reason)
	}
}

// TestAgentRunRefusalRemedyClearsTheRefusalAndFewerStepsDoNot: the refusal names one
// remedy, a timeout_sec of at least N s. Following it must get the call admitted,
// and the other lever a caller might reach for, a smaller step budget, must NOT
// change the verdict: the floor ignores max_steps, which is why the reason may
// not offer it.
func TestAgentRunRefusalRemedyClearsTheRefusalAndFewerStepsDoNot(t *testing.T) {
	var hits atomic.Int64
	srv := wallGateServer(t, &hits)
	s := wallGateServerFor(t, srv.URL, 5, nil)
	root := t.TempDir()
	call := func(maxSteps, wall int) map[string]any {
		t.Helper()
		res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
			`{"goal":"audit the repo","read_root":%q,"max_steps":%d,"timeout_sec":%d}`, root, maxSteps, wall)))
		if err != nil {
			t.Fatalf("handleAgentRun: %v", err)
		}
		return decodeResult(t, res)
	}

	first := call(12, 20)
	reason, _ := first["reason"].(string)
	mt := regexp.MustCompile(`timeout_sec of at least (\d+) s`).FindStringSubmatch(reason)
	if first["deferred"] != true || mt == nil {
		t.Fatalf("test premise: a 20 s wall on a 5 tok/s seat is refused with a timeout_sec remedy, got %v", first)
	}
	if fewer := call(1, 20); fewer["deferred"] != true || fewer["defer_class"] != core.DeferClassBudget {
		t.Errorf("one step is under the same floor, so max_steps 1 must be refused too, got %v", fewer)
	}
	ask, _ := strconv.Atoi(mt[1])
	if hits.Load() != 0 {
		t.Fatalf("test premise: the refusals must not reach the seat, %d request(s) did", hits.Load())
	}
	if followed := call(12, ask); followed["deferred"] == true {
		t.Errorf("timeout_sec %d is what the refusal asked for and must be admitted, got %v", ask, followed)
	}
}

// TestAgentRunRefusalNeverAsksForAWallUnderTheFloor: on a very fast seat a one-step
// run can be estimated at less than the floor (the floor charges a tool step's
// 6 s prefill the one-step estimate does not), and the refusal must not advise
// a wall it would itself refuse.
func TestAgentRunRefusalNeverAsksForAWallUnderTheFloor(t *testing.T) {
	var hits atomic.Int64
	srv := wallGateServer(t, &hits)
	s := wallGateServerFor(t, srv.URL, 300, nil)

	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":1,"thinking":"off","timeout_sec":2}`, t.TempDir())))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true {
		t.Fatalf("a 2 s wall is under the one-step floor and must be refused, got %v", m)
	}
	floor, _ := m["wall_estimate_sec"].(float64)
	reason, _ := m["reason"].(string)
	if floor >= 7 {
		t.Fatalf("test premise: the one-step estimate %v s must be under the 7 s floor", floor)
	}
	if !strings.Contains(reason, "need 7 s") || !strings.Contains(reason, "timeout_sec of at least 7 s") {
		t.Errorf("the refusal must ask for the floor (7 s), not the lower estimate: %s", reason)
	}
}

// TestAgentRunRefusesTheBoxDefaultWallToo: with no timeout_sec the wall is the
// box's agent_timeout_sec, and it is just as hard a kill. The refusal says WHERE
// the number came from so the fix lands on the right key.
func TestAgentRunRefusesTheBoxDefaultWallToo(t *testing.T) {
	var hits atomic.Int64
	srv := wallGateServer(t, &hits)
	s := wallGateServerFor(t, srv.URL, 5, func(c *config.Config) { c.AgentTimeoutSec = 20 })

	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"audit the repo","read_root":%q,"max_steps":12}`, t.TempDir())))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true {
		t.Fatalf("a defaulted wall under the floor must be refused too, got %v", m)
	}
	reason, _ := m["reason"].(string)
	if !strings.Contains(reason, "agent_timeout_sec") {
		t.Errorf("the reason must name the box default it came from: %s", reason)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the seat received %d request(s), want 0", n)
	}
}

// TestAgentRunPublishesTheSizingOnARunItAdmits: a wall ABOVE the floor but below
// the full estimate is not refused (ADR 0050: the max-final worst case is never a
// refusal, and a measured-working 900 s tier would otherwise be shut out). The run
// goes ahead and the result carries the numbers, so a caller who then hits the
// wall reads why instead of guessing.
func TestAgentRunPublishesTheSizingOnARunItAdmits(t *testing.T) {
	var hits atomic.Int64
	srv := wallGateServer(t, &hits)
	s := wallGateServerFor(t, srv.URL, 5, nil)

	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":2,"timeout_sec":120}`, t.TempDir())))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] == true {
		t.Fatalf("a wall above the floor must run, got %v", m)
	}
	est := fullRunSizing(5, 2, 120)
	if est.TotalSec <= 120 {
		t.Fatalf("test premise: the full estimate %d s must exceed the 120 s wall", est.TotalSec)
	}
	if got, _ := m["wall_estimate_sec"].(float64); int(got) != est.TotalSec {
		t.Errorf("wall_estimate_sec = %v, want %d", m["wall_estimate_sec"], est.TotalSec)
	}
	if got, _ := m["min_turn_sec"].(float64); int(got) != est.MinTurnSec {
		t.Errorf("min_turn_sec = %v, want %d", m["min_turn_sec"], est.MinTurnSec)
	}
	if note, _ := m["wall_note"].(string); !strings.Contains(note, "wall 120 s is BELOW the estimate") {
		t.Errorf("wall_note = %q, want the below-estimate arithmetic on a run the door admitted", note)
	}
}

// TestAgentRunWithNoRateIsNoOpinion: a seat with no rate sample and no configured
// rate has no floor to compute, so nothing is refused and the note says why there
// are no numbers — the house rule every sizing decision follows.
func TestAgentRunWithNoRateIsNoOpinion(t *testing.T) {
	var hits atomic.Int64
	srv := wallGateServer(t, &hits)
	s := wallGateServerFor(t, srv.URL, 0, nil)

	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":2,"timeout_sec":30}`, t.TempDir())))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] == true {
		t.Fatalf("no rate is no opinion, the run must go ahead: %v", m)
	}
	if note, _ := m["wall_note"].(string); !strings.Contains(note, "no decode-rate sample") {
		t.Errorf("wall_note = %q, want the reason there is no sizing", note)
	}
	if _, ok := m["wall_estimate_sec"]; ok {
		t.Errorf("no rate must publish no estimate: %v", m["wall_estimate_sec"])
	}
}

// TestAgentRunThatDiesMidRunStillCarriesTheSizing: a run the door admitted and
// that then ended in a deferral (here: the caller's context is cancelled while
// the seat is mid-completion, the same exit a wall deadline takes) must carry the
// numbers too — a run that died at its wall is the one whose caller most needs to
// read what the wall was weighed against.
func TestAgentRunThatDiesMidRunStillCarriesTheSizing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{}) // frees the blocked handler so the server can close
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/running":
			fmt.Fprintf(w, `{"running":[{"model":%q,"state":"ready","cmd":"x"}]}`, wallGateSeat)
		case "/v1/models":
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model"}]}`, wallGateSeat)
		case "/upstream/" + wallGateSeat + "/v1/models":
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"max_model_len":65536}]}`, wallGateSeat)
		case "/v1/chat/completions":
			cancel() // the run is mid-completion when its context ends
			select {
			case <-r.Context().Done():
			case <-release:
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	defer close(release) // runs before srv.Close
	s := wallGateServerFor(t, srv.URL, 5, nil)

	res, err := s.handleAgentRun(ctx, callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":2,"timeout_sec":120}`, t.TempDir())))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true {
		t.Fatalf("the cancelled run must defer, got %v", m)
	}
	est := fullRunSizing(5, 2, 120)
	if got, _ := m["wall_estimate_sec"].(float64); int(got) != est.TotalSec {
		t.Errorf("wall_estimate_sec = %v, want %d on a deferred run", m["wall_estimate_sec"], est.TotalSec)
	}
	if note, _ := m["wall_note"].(string); !strings.Contains(note, "wall 120 s is BELOW the estimate") {
		t.Errorf("wall_note = %q, want the arithmetic on a deferred run", note)
	}
}

// TestAgentRunNamedModelIsNotSizedAtTheAgentSeatsRate: agent_seat_tok_s is the
// planner seat's rate. A caller who names ANOTHER model has a seat the box has no
// rate for, so the door has no opinion about it: no floor, no refusal, no
// published estimate — the run goes ahead exactly as it did before the gate.
// Sized at the agent seat's 5 tok/s it was refused on a 20 s wall, with the named
// model in the refusal and the agent seat's rate attributed to it.
func TestAgentRunNamedModelIsNotSizedAtTheAgentSeatsRate(t *testing.T) {
	const named = "some-other-fast-model"
	var hits atomic.Int64
	srv := wallGateServerSeats(t, &hits, wallGateSeat, named)
	s := wallGateServerFor(t, srv.URL, 5, nil)

	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":12,"timeout_sec":20,"model":%q}`, t.TempDir(), named)))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] == true {
		t.Fatalf("a named model with no rate of its own must run, not be refused at the agent seat's rate: %v", m)
	}
	if got, _ := m["output"].(string); !strings.Contains(got, "42") {
		t.Errorf("the run must have reached the seat and answered, output = %q", got)
	}
	for _, k := range []string{"wall_estimate_sec", "min_turn_sec"} {
		if v, ok := m[k]; ok {
			t.Errorf("%s = %v: a seat with no rate of its own must publish no estimate", k, v)
		}
	}
	note, _ := m["wall_note"].(string)
	if !strings.Contains(note, "no decode-rate sample") {
		t.Errorf("wall_note = %q, want the reason there is no sizing", note)
	}
	if strings.Contains(note, "5.0 tok/s") || strings.Contains(note, "(config agent_seat_tok_s") {
		t.Errorf("wall_note = %q: the agent seat's configured rate must not be attributed to the named model", note)
	}

	// Naming the configured agent seat itself keeps its rate and its gate.
	res, err = s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":12,"timeout_sec":20,"model":%q}`, t.TempDir(), wallGateSeat)))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m = decodeResult(t, res)
	if m["deferred"] != true || m["defer_class"] != core.DeferClassBudget {
		t.Fatalf("the agent seat named explicitly is still sized at its own rate and refused on a 20 s wall: %v", m)
	}
}

// TestAgentRunNamedModelWithItsOwnStoreRateIsStillGated: the other half of the
// attribution rule. A named model the seat-rates store HAS measured is sized from
// its own entry — so the gate still refuses it, at the rate that is its own, and
// the refusal names that rate's source.
func TestAgentRunNamedModelWithItsOwnStoreRateIsStillGated(t *testing.T) {
	const named = "some-other-fast-model"
	var hits atomic.Int64
	srv := wallGateServerSeats(t, &hits, wallGateSeat, named)
	// the configured agent seat rate is high (100 tok/s) and the named model's own
	// measured rate is low (5 tok/s): only the store entry can refuse the 20 s wall
	s := wallGateServerFor(t, srv.URL, 100, nil)
	root, err := gpulease.ResolveStateRoot(s.p.Cfg().StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := seatrate.Update(seatrate.Path(root), func(st *seatrate.Store) { st.Observe(named, 5, 200, time.Now()) }); err != nil {
		t.Fatal(err)
	}

	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"audit the repo","read_root":%q,"max_steps":12,"timeout_sec":20,"model":%q}`, t.TempDir(), named)))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true || m["defer_class"] != core.DeferClassBudget {
		t.Fatalf("a named model measured at 5 tok/s cannot hold a 20 s wall, got %v", m)
	}
	reason, _ := m["reason"].(string)
	if !strings.Contains(reason, named) || !strings.Contains(reason, "5.0 tok/s") {
		t.Errorf("the refusal must name the model and its own measured rate: %s", reason)
	}
	if note, _ := m["wall_note"].(string); !strings.Contains(note, "5.0 tok/s (store") {
		t.Errorf("wall_note = %q, want the store's rate for the named model", note)
	}
}

// TestAgentRunPlacedSeatIsNotSizedAtTheAgentSeatsRate: the composite half of the
// attribution rule. A long-context ask is placed on the pair's long seat, a
// seat the box's agent_seat_tok_s (the agent seat's) does not describe, so the
// door sizes nothing for it and the placed run goes ahead on a short wall.
func TestAgentRunPlacedSeatIsNotSizedAtTheAgentSeatsRate(t *testing.T) {
	const long = "qwen3.8-27b-262k"
	var hits atomic.Int64
	srv := wallGateServerSeats(t, &hits, wallGateSeat, long)
	s := wallGateServerFor(t, srv.URL, 5, func(c *config.Config) {
		comp := shippedComposite()
		c.TierProfile, c.Tiers, c.Layers = comp.TierProfile, comp.Tiers, comp.Layers
	})

	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":12,"timeout_sec":20,"context_class":"long"}`, t.TempDir())))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	placed, _ := m["placed"].(map[string]any)
	if placed == nil || placed["seat"] != long {
		t.Fatalf("test premise: a long-context ask must be placed on %s, got %v", long, m)
	}
	if m["deferred"] == true {
		t.Fatalf("a placed seat with no rate of its own must run, not be refused at the agent seat's rate: %v", m)
	}
	if v, ok := m["wall_estimate_sec"]; ok {
		t.Errorf("wall_estimate_sec = %v: the placed seat has no rate of its own, so no estimate", v)
	}
}
