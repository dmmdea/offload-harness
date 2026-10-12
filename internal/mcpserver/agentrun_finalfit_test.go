// agentrun_finalfit_test.go pins the agent_run door's FINAL-BUDGET FIT (register
// D-134, option (a), the cure D-102 left open).
//
// This door runs its loop under a context deadline, so the wall is a hard kill
// here. D-102 sized the run, published the numbers and refused a wall that cannot
// hold one tool step and a minimal answer; it left the row's own case admitted: a
// 420 s wall against a min_turn_sec of 807 s and an estimate of 928 s, which the INV-5
// rider (ADR 0050) forbids refusing. That run then spent its tool steps and opened
// the forced final turn at the configured 8,192-token budget, 476 s of decode at
// the seat's rate against a wall that had at most 420 s in it, and was cut at the
// deadline with no answer. The delegation door's node has installed the D-95 fit
// (agent.Loop.WithFinalBudgetFit) for months; this door never did.
//
// The fixture below is that case, rebuilt from the seatrate arithmetic rather than
// copied: a seat measured at 17.2 tok/s with a 330 s cold load, an 8,192-token final
// (a 2,048-token step budget), thinking off, ten steps, a 420 s wall. The premise
// checks assert the two published numbers, so a drift in the arithmetic fails loudly
// instead of quietly turning these into tests of another run.

package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

const (
	finalFitTokS     = 17.2 // the seat's measured decode rate
	finalFitColdSec  = 330  // its measured cold load
	finalFitStepTok  = 2048 // agent_max_tokens: the tool-step budget
	finalFitFinalTok = 8192 // 4 x the step budget: the configured final
	finalFitSteps    = 10
	finalFitWallSec  = 420
	finalFitMinTurn  = 807 // published min_turn_sec for this run
	finalFitEstimate = 928 // published wall_estimate_sec for this run
)

// finalFitCall is one completion the seat was asked for.
type finalFitCall struct {
	forced    bool // the loop's forced final step: its last message is the answer-now turn
	maxTokens int
}

// finalFitSeat is a seat that records the budget of every completion it is asked for.
// Unless eager, it answers every turn with a tool call (a different directory each
// time, so the loop's exact-repeat refusal never reshapes the run) until the loop
// forces the final step, which it answers.
type finalFitSeat struct {
	srv     *httptest.Server
	mu      sync.Mutex
	calls   []finalFitCall
	release chan struct{} // frees a hung completion so the server can close
}

type finalFitSeatOpts struct {
	eager bool // answer the first completion instead of calling tools
	// cutFinal answers the forced final with a half-sentence that ended on the completion budget (finish_reason
	// "length"), the shape a final cut at a narrowed budget takes.
	cutFinal bool
	models   []string // the served roster; default: the box's agent seat
	// onChat runs when a completion arrives, before it is answered; hang then holds
	// the request open until its context ends (a run cut mid-completion).
	onChat func()
	hang   bool
}

func newFinalFitSeat(t *testing.T, o finalFitSeatOpts) *finalFitSeat {
	t.Helper()
	models := o.models
	if len(models) == 0 {
		models = []string{wallGateSeat}
	}
	seat := &finalFitSeat{release: make(chan struct{})}
	seat.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			var body struct {
				MaxTokens int `json:"max_tokens"`
				Messages  []struct {
					Role    string `json:"role"`
					Content any    `json:"content"`
				} `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			forced := false
			if n := len(body.Messages); n > 0 {
				last := body.Messages[n-1]
				forced = last.Role == "user" && last.Content == agent.FinalAnswerTurn
			}
			seat.mu.Lock()
			seat.calls = append(seat.calls, finalFitCall{forced: forced, maxTokens: body.MaxTokens})
			n := len(seat.calls)
			seat.mu.Unlock()
			if o.onChat != nil {
				o.onChat()
			}
			if o.hang {
				select {
				case <-r.Context().Done():
				case <-seat.release:
				}
				return
			}
			if forced && o.cutFinal {
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The answer begins with 42, and then"},"finish_reason":"length"}]}`)
				return
			}
			if forced || o.eager {
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The answer is 42."},"finish_reason":"stop"}]}`)
				return
			}
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call-%d","type":"function","function":{"name":"list_dir","arguments":"{\"path\":\"d%d\"}"}}]},"finish_reason":"tool_calls"}]}`, n, n)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(seat.srv.Close)
	t.Cleanup(func() { close(seat.release) }) // runs before srv.Close
	return seat
}

func (s *finalFitSeat) recorded() []finalFitCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]finalFitCall(nil), s.calls...)
}

// finalFitDoorOpts says how the door under test is configured.
type finalFitDoorOpts struct {
	seedStore bool    // the seat-rates store holds the fixture seat's measured rate and cold load
	cfgTokS   float64 // agent_seat_tok_s: the stand-in the box configures for its own agent seat
}

// finalFitDoor builds the agent_run door over the fixture box: a 2,048-token step budget
// (so an 8,192-token final), thinking off, and, when asked, the fixture seat's measured
// numbers in the seat-rates store under the box's state root.
func finalFitDoor(t *testing.T, endpoint string, o finalFitDoorOpts) *Server {
	t.Helper()
	s := wallGateServerFor(t, endpoint, o.cfgTokS, func(c *config.Config) {
		c.AgentMaxTokens = finalFitStepTok
		c.AgentThinking = "off"
	})
	if o.seedStore {
		root, err := gpulease.ResolveStateRoot(s.p.Cfg().StateDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := seatrate.Update(seatrate.Path(root), func(st *seatrate.Store) {
			st.Observe(wallGateSeat, finalFitTokS, finalFitColdSec, time.Now())
		}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// callFinalFitRun is the call every test here makes: ten steps under wallSec, extra JSON
// members spliced in.
func callFinalFitRun(t *testing.T, ctx context.Context, s *Server, wallSec int, extra string) map[string]any {
	t.Helper()
	res, err := s.handleAgentRun(ctx, callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":%d,"timeout_sec":%d%s}`, t.TempDir(), finalFitSteps, wallSec, extra)))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	return decodeResult(t, res)
}

// requireTheRegistersCase fails unless the run was sized as the register's own case: a wall
// under the seat's min_turn_sec, which is under the whole-run estimate.
func requireTheRegistersCase(t *testing.T, m map[string]any) {
	t.Helper()
	if got, _ := m["wall_estimate_sec"].(float64); int(got) != finalFitEstimate {
		t.Fatalf("test premise: wall_estimate_sec = %v, want %d (the register's case)", m["wall_estimate_sec"], finalFitEstimate)
	}
	if got, _ := m["min_turn_sec"].(float64); int(got) != finalFitMinTurn {
		t.Fatalf("test premise: min_turn_sec = %v, want %d (the register's case)", m["min_turn_sec"], finalFitMinTurn)
	}
	if note, _ := m["wall_note"].(string); !strings.Contains(note, fmt.Sprintf("wall %d s is BELOW the estimate %d s", finalFitWallSec, finalFitEstimate)) {
		t.Fatalf("test premise: wall_note = %q, want the below-estimate arithmetic", note)
	}
	if finalFitWallSec >= finalFitMinTurn {
		t.Fatalf("test premise: the wall %d s must be under min_turn_sec %d s", finalFitWallSec, finalFitMinTurn)
	}
	// The defect in one line: the configured final asks the seat for longer than the whole wall.
	if sec := float64(finalFitFinalTok) / finalFitTokS; sec <= finalFitWallSec {
		t.Fatalf("test premise: a %d-token final is %.0f s of decode, which must exceed the %d s wall", finalFitFinalTok, sec, finalFitWallSec)
	}
}

// requireNoFinalFit fails when a result carries the fit's two published fields.
func requireNoFinalFit(t *testing.T, m map[string]any) {
	t.Helper()
	for _, k := range []string{"final_budget_fit", "budget_note"} {
		if v, ok := m[k]; ok {
			t.Errorf("%s = %v: a run whose final was never narrowed must publish no fit", k, v)
		}
	}
}

// forcedFinalOf returns the budget of the loop's forced final step and the budgets of the
// tool steps before it, and fails unless the run was ten steps ending on the forced final.
func forcedFinalOf(t *testing.T, seat *finalFitSeat) (final int, steps []int) {
	t.Helper()
	calls := seat.recorded()
	if len(calls) != finalFitSteps {
		t.Fatalf("the seat saw %d completion(s), want %d (nine tool steps and the forced final): %+v", len(calls), finalFitSteps, calls)
	}
	for i, c := range calls[:len(calls)-1] {
		if c.forced {
			t.Fatalf("completion %d was the forced final too early: %+v", i+1, calls)
		}
		steps = append(steps, c.maxTokens)
	}
	last := calls[len(calls)-1]
	if !last.forced {
		t.Fatalf("the last completion is not the forced final: %+v", calls)
	}
	return last.maxTokens, steps
}

// TestAgentRunFitsTheFinalAnswerToTheWallThatIsLeft is the cure, on the register's own case:
// a 420 s wall against a seat whose min_turn_sec is 807 s. The tool steps run at the step
// budget, and the forced final opens at the largest budget the wall that is LEFT can decode
// at the seat's rate (a tenth of the wall, at least 6 s, held back), not at the configured
// 8,192, and the result says so under the delegation wire's own names.
func TestAgentRunFitsTheFinalAnswerToTheWallThatIsLeft(t *testing.T) {
	seat := newFinalFitSeat(t, finalFitSeatOpts{})
	s := finalFitDoor(t, seat.srv.URL, finalFitDoorOpts{seedStore: true})

	m := callFinalFitRun(t, context.Background(), s, finalFitWallSec, "")
	requireTheRegistersCase(t, m)
	if m["deferred"] == true {
		t.Fatalf("the run must answer inside its wall, got %v", m)
	}
	if got, _ := m["output"].(string); !strings.Contains(got, "42") {
		t.Errorf("output = %q, want the seat's answer", got)
	}
	if steps, _ := m["steps"].(float64); int(steps) != finalFitSteps {
		t.Errorf("steps = %v, want %d: nine tool steps and the forced final", m["steps"], finalFitSteps)
	}

	final, toolSteps := forcedFinalOf(t, seat)
	for i, got := range toolSteps {
		if got != finalFitStepTok {
			t.Errorf("tool step %d ran at %d tokens, want the step budget %d: the fit narrows the final answer only", i+1, got, finalFitStepTok)
		}
	}
	// The run's tool steps are instant here, so nearly the whole wall is left at the forced
	// final: the fit is (wall - max(10 %, 6 s)) x tok/s = 6,501 tokens. The lower bound allows
	// twenty seconds of a loaded machine's delay; the upper bound is the whole wall.
	fit := func(remainingSec float64) int {
		return seatrate.FitFinalBudget(seatrate.FinalFit{ConfiguredFinal: finalFitFinalTok, RemainingSec: remainingSec, TokS: finalFitTokS}).Budget
	}
	lo, hi := fit(finalFitWallSec-20), fit(finalFitWallSec)
	if final >= finalFitFinalTok {
		t.Fatalf("the forced final opened at %d tokens, the configured budget: %.0f s of decode against a %d s wall, the run is cut at the deadline with no answer (want %d..%d)",
			final, float64(final)/finalFitTokS, finalFitWallSec, lo, hi)
	}
	if final < lo || final > hi {
		t.Errorf("the forced final opened at %d tokens, want the fit of the wall that is left, %d..%d", final, lo, hi)
	}
	if sec := float64(final) / finalFitTokS; sec >= finalFitWallSec {
		t.Errorf("the final budget %d is %.0f s of decode at %.1f tok/s, which does not fit the %d s wall", final, sec, finalFitTokS, finalFitWallSec)
	}

	// What the run did is published: the budget it opened at and the arithmetic.
	if got, _ := m["final_budget_fit"].(float64); int(got) != final {
		t.Errorf("final_budget_fit = %v, want %d, the budget the forced final was sent", m["final_budget_fit"], final)
	}
	note, _ := m["budget_note"].(string)
	mt := regexp.MustCompile(`^final 8192 → (\d+) to fit \d+ s at 17\.2 tok/s$`).FindStringSubmatch(note)
	if mt == nil || mt[1] != fmt.Sprint(final) {
		t.Errorf("budget_note = %q, want the fit's arithmetic ending on the %d tokens that were sent", note, final)
	}
}

// TestAgentRunSizesTheRunStartFinalLikeTheNodeDoes: the two agent doors must size one run
// alike. The delegation door's node and this door are handed the same contract (ten steps,
// a 420 s wall) on the same seat with the same measured numbers; the seat answers at once,
// so no forced final is reached and what each door publishes is its run-start sizing: the
// estimate, the min_turn and the final-budget fit. They must match to the byte. (Past the
// forced final the doors differ on purpose: the node's wall is an expectation and its loop
// runs to the liveness ceiling, ADR 0055; this door's is the deadline the fit reads.)
func TestAgentRunSizesTheRunStartFinalLikeTheNodeDoes(t *testing.T) {
	seat := newFinalFitSeat(t, finalFitSeatOpts{eager: true})
	s := finalFitDoor(t, seat.srv.URL, finalFitDoorOpts{seedStore: true})

	door := callFinalFitRun(t, context.Background(), s, finalFitWallSec, "")
	requireTheRegistersCase(t, door)
	if door["deferred"] == true {
		t.Fatalf("the door must answer, got %v", door)
	}
	wire, err := s.p.RunAgentContract(context.Background(), core.AgentContract{
		SchemaVersion: core.AgentWireSchemaVersion,
		Goal:          "what is the answer",
		MaxSteps:      finalFitSteps,
		TimeoutSec:    finalFitWallSec,
		Depth:         1,
	}, pipeline.AgentContractOptions{})
	if err != nil {
		t.Fatalf("RunAgentContract: %v", err)
	}
	if wire.Deferred {
		t.Fatalf("the node must answer, got a deferral: %s", wire.Reason)
	}

	// Test premise: the node sized the run as the register's case too, so what follows
	// compares the doors on the case that matters and not on a default.
	if wire.WallEstimateSec != finalFitEstimate || wire.MinTurnSec != finalFitMinTurn {
		t.Fatalf("test premise: the node sized the run at %d s / min_turn %d s, want %d / %d", wire.WallEstimateSec, wire.MinTurnSec, finalFitEstimate, finalFitMinTurn)
	}
	if got, _ := door["wall_estimate_sec"].(float64); int(got) != wire.WallEstimateSec {
		t.Errorf("wall_estimate_sec: door %v, node %d", door["wall_estimate_sec"], wire.WallEstimateSec)
	}
	if got, _ := door["min_turn_sec"].(float64); int(got) != wire.MinTurnSec {
		t.Errorf("min_turn_sec: door %v, node %d", door["min_turn_sec"], wire.MinTurnSec)
	}
	if got, _ := door["wall_note"].(string); got != wire.WallNote {
		t.Errorf("wall_note differs:\n door: %s\n node: %s", got, wire.WallNote)
	}

	// The fit. The 420 s wall cannot hold the run's own tool steps and a cold load's worth of
	// other terms, so the node's run-start fit is the 1,024-token floor and says the wall
	// will be the stop. The door must publish exactly that, not nothing and not another figure.
	if wire.FinalBudgetFit != 1024 {
		t.Fatalf("test premise: the node's run-start fit is %d, want the 1,024 floor", wire.FinalBudgetFit)
	}
	if got, _ := door["final_budget_fit"].(float64); int(got) != wire.FinalBudgetFit {
		t.Errorf("final_budget_fit: door %v, node %d", door["final_budget_fit"], wire.FinalBudgetFit)
	}
	if got, _ := door["budget_note"].(string); got != wire.BudgetNote {
		t.Errorf("budget_note differs:\n door: %s\n node: %s", got, wire.BudgetNote)
	}
	for _, want := range []string{"final 8192 → floor 1024", "420 s of wall at 17.2 tok/s", "the wall will be the stop"} {
		if !strings.Contains(wire.BudgetNote, want) {
			t.Errorf("the node's budget_note lacks %q: %s", want, wire.BudgetNote)
		}
	}
}

// TestAgentRunThatDiesMidRunCarriesTheRunStartFit: a run cut before it reaches the forced
// final (here the caller's context ends mid-completion, the exit a wall deadline takes)
// never narrowed anything, and its deferral is the one a caller most needs the arithmetic
// on: it carries the run-start fit, as the node's result does.
func TestAgentRunThatDiesMidRunCarriesTheRunStartFit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seat := newFinalFitSeat(t, finalFitSeatOpts{onChat: cancel, hang: true})
	s := finalFitDoor(t, seat.srv.URL, finalFitDoorOpts{seedStore: true})

	m := callFinalFitRun(t, ctx, s, finalFitWallSec, "")
	if m["deferred"] != true {
		t.Fatalf("the cancelled run must defer, got %v", m)
	}
	requireTheRegistersCase(t, m)
	if got, _ := m["final_budget_fit"].(float64); int(got) != 1024 {
		t.Errorf("final_budget_fit = %v, want the run-start fit 1024 on a run cut before its forced final", m["final_budget_fit"])
	}
	if note, _ := m["budget_note"].(string); !strings.Contains(note, "final 8192 → floor 1024") || !strings.Contains(note, "the wall will be the stop") {
		t.Errorf("budget_note = %q, want the run-start fit's arithmetic", note)
	}
}

// TestAgentRunLeavesTheFinalBudgetAloneWhenThereIsNothingToFit: the fit is a ceiling. With no
// rate to compute from, with a rate that belongs to another seat, or with a wall that has
// room, the forced final opens at the configured budget exactly as it did before the fit,
// and nothing is published: an un-narrowed run must not carry a narrowing note.
func TestAgentRunLeavesTheFinalBudgetAloneWhenThereIsNothingToFit(t *testing.T) {
	const named = "some-other-fast-model"
	cases := []struct {
		name    string
		door    finalFitDoorOpts
		wallSec int
		extra   string
		models  []string
	}{
		{name: "a seat with no rate", door: finalFitDoorOpts{}, wallSec: finalFitWallSec},
		// agent_seat_tok_s is the agent seat's rate: a model the caller names has none of its own
		// here, so the fit must not borrow it (the attribution rule the D-102 gate already keeps)
		{name: "a named model with no rate of its own", door: finalFitDoorOpts{cfgTokS: finalFitTokS}, wallSec: finalFitWallSec,
			extra: fmt.Sprintf(`,"model":%q`, named), models: []string{wallGateSeat, named}},
		{name: "a wall with room", door: finalFitDoorOpts{seedStore: true}, wallSec: 3600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seat := newFinalFitSeat(t, finalFitSeatOpts{models: tc.models})
			s := finalFitDoor(t, seat.srv.URL, tc.door)

			m := callFinalFitRun(t, context.Background(), s, tc.wallSec, tc.extra)
			if m["deferred"] == true {
				t.Fatalf("the run must answer, got %v", m)
			}
			final, _ := forcedFinalOf(t, seat)
			if final != finalFitFinalTok {
				t.Errorf("the forced final opened at %d tokens, want the configured %d: there was nothing to fit", final, finalFitFinalTok)
			}
			requireNoFinalFit(t, m)
		})
	}
}

// TestAgentRunWallRefusalCarriesNoFinalFit: the D-102 refusal says the run was not started,
// and a note about the final answer's budget on it would contradict that. The fit rides only
// on a run that reached the loop.
func TestAgentRunWallRefusalCarriesNoFinalFit(t *testing.T) {
	var hits atomic.Int64
	srv := wallGateServer(t, &hits)
	s := wallGateServerFor(t, srv.URL, 5, nil)

	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"audit the repo","read_root":%q,"max_steps":12,"timeout_sec":20}`, t.TempDir())))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true || m["defer_class"] != core.DeferClassBudget {
		t.Fatalf("test premise: a 20 s wall on a 5 tok/s seat is refused, got %v", m)
	}
	requireNoFinalFit(t, m)
}

// TestAgentRunPublishesTheRunStartFitWhenItsOwnFinalWasNotNarrowed pins the sentence the docs
// carry about final_budget_fit: it is the budget the forced final opened at when the loop
// narrowed it, and otherwise the fit PREDICTED when the wall started, exactly as the delegation
// door's node publishes it. The two differ whenever the estimate is more pessimistic than the
// live clock: it charges the seat's remembered cold load (330 s here) against the wall, while
// admission pays that load outside the wall. So a wall just under the estimate, 900 s against
// 928 s, predicts a 6,174-token final ((900 - 451 other - 90 safety) x 17.2 tok/s); the loop
// reaches its forced final with about 900 s left and opens it at the configured 8,192; and the
// result still carries the prediction, which the note words as a fit to the wall as it stood at
// the start, not as a report of the budget that was sent. Making the field a report of the
// budget sent on every run would change both doors, and is left for a decision of its own.
func TestAgentRunPublishesTheRunStartFitWhenItsOwnFinalWasNotNarrowed(t *testing.T) {
	const (
		wall     = 900
		wantFit  = 6174
		wantNote = "final 8192 → 6174 to fit 900 s at 17.2 tok/s"
	)
	seat := newFinalFitSeat(t, finalFitSeatOpts{})
	s := finalFitDoor(t, seat.srv.URL, finalFitDoorOpts{seedStore: true})

	door := callFinalFitRun(t, context.Background(), s, wall, "")
	if door["deferred"] == true {
		t.Fatalf("the run must answer, got %v", door)
	}
	// test premise: the wall is under the estimate, so the run-start fit narrows...
	if got, _ := door["wall_estimate_sec"].(float64); int(got) != finalFitEstimate {
		t.Fatalf("test premise: wall_estimate_sec = %v, want %d", door["wall_estimate_sec"], finalFitEstimate)
	}
	if note, _ := door["wall_note"].(string); !strings.Contains(note, fmt.Sprintf("wall %d s is BELOW the estimate %d s", wall, finalFitEstimate)) {
		t.Fatalf("test premise: wall_note = %q, want the below-estimate arithmetic", note)
	}
	// ...and the wall that is left at the forced final has room, so the loop leaves the final alone
	if final, _ := forcedFinalOf(t, seat); final != finalFitFinalTok {
		t.Fatalf("test premise: the forced final opened at %d tokens, want the configured %d (the wall that was left had room)", final, finalFitFinalTok)
	}
	if got, _ := door["final_budget_fit"].(float64); int(got) != wantFit {
		t.Errorf("final_budget_fit = %v, want the run-start prediction %d", door["final_budget_fit"], wantFit)
	}
	if got, _ := door["budget_note"].(string); got != wantNote {
		t.Errorf("budget_note = %q, want %q", got, wantNote)
	}

	// The node, handed the same contract on the same seat, publishes the same prediction: it is what
	// "as the node's result does" in the docs rests on.
	eager := newFinalFitSeat(t, finalFitSeatOpts{eager: true})
	node := finalFitDoor(t, eager.srv.URL, finalFitDoorOpts{seedStore: true})
	wire, err := node.p.RunAgentContract(context.Background(), core.AgentContract{
		SchemaVersion: core.AgentWireSchemaVersion,
		Goal:          "what is the answer",
		MaxSteps:      finalFitSteps,
		TimeoutSec:    wall,
		Depth:         1,
	}, pipeline.AgentContractOptions{})
	if err != nil {
		t.Fatalf("RunAgentContract: %v", err)
	}
	if wire.Deferred {
		t.Fatalf("the node must answer, got a deferral: %s", wire.Reason)
	}
	if wire.FinalBudgetFit != wantFit || wire.BudgetNote != wantNote {
		t.Errorf("the node published %d %q, want %d %q: the door and the node must publish one prediction", wire.FinalBudgetFit, wire.BudgetNote, wantFit, wantNote)
	}
}

// TestAgentRunSaysWhenItsFinalAnswerWasCutAtItsBudget: a forced final that ended on the completion budget (finish_reason
// "length") is a partial, and the loop marks it (Result.OutputTruncated) while still ending "done". The fit makes that end
// likelier, because it deliberately opens the final at a budget narrower than the configured one, and the door published
// the half-sentence as a clean answer: deferred false, stop_reason done, nothing saying it was cut. The answer is still the
// caller's to use, so it stays an answer; the flag, the loop's own stop note and a sentence naming the budget it was cut at
// are what the caller was owed. A complete answer carries none of the three. The sentence names a budget only when the LOOP
// opened the final there: a run whose own final was not narrowed carries the fit predicted at run start (the 900 s row), and
// a cut at the configured budget must not be worded as a cut at that prediction.
func TestAgentRunSaysWhenItsFinalAnswerWasCutAtItsBudget(t *testing.T) {
	const prediction = 6174 // the 900 s wall's run-start fit, which its own forced final never opens at
	cases := []struct {
		name      string
		cut       bool
		wallSec   int
		loopFit   bool // the loop narrowed the forced final
		published bool // the result carries final_budget_fit (the loop's narrowing, else the run-start prediction)
	}{
		{name: "cut at a budget the fit narrowed", cut: true, wallSec: finalFitWallSec, loopFit: true, published: true},
		{name: "cut at the configured budget", cut: true, wallSec: 3600},
		{name: "cut at the configured budget beside a run-start prediction", cut: true, wallSec: 900, published: true},
		{name: "a complete answer", cut: false, wallSec: finalFitWallSec, loopFit: true, published: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seat := newFinalFitSeat(t, finalFitSeatOpts{cutFinal: tc.cut})
			s := finalFitDoor(t, seat.srv.URL, finalFitDoorOpts{seedStore: true})

			m := callFinalFitRun(t, context.Background(), s, tc.wallSec, "")
			if m["deferred"] == true {
				t.Fatalf("the run must answer, got %v", m)
			}
			final, _ := forcedFinalOf(t, seat)
			if tc.loopFit != (final < finalFitFinalTok) {
				t.Fatalf("test premise: the forced final opened at %d tokens, which is not the case this row names (loop narrowed it: %v)", final, tc.loopFit)
			}
			if _, has := m["final_budget_fit"]; has != tc.published {
				t.Fatalf("test premise: final_budget_fit present = %v, this row names %v: %v", has, tc.published, m["final_budget_fit"])
			}
			if !tc.cut {
				for _, k := range []string{"output_truncated", "stop_note", "truncation_note"} {
					if v, has := m[k]; has {
						t.Errorf("a complete answer must not carry %s, got %v", k, v)
					}
				}
				return
			}
			if got, _ := m["output"].(string); got != "The answer begins with 42, and then" {
				t.Errorf("the partial answer is still published as output, got %q", got)
			}
			if m["output_truncated"] != true {
				t.Fatalf("output_truncated = %v, want true: a final cut at its budget is a partial and the caller must be told: %v", m["output_truncated"], m)
			}
			if note, _ := m["stop_note"].(string); !strings.Contains(note, "forced final answer") {
				t.Errorf("stop_note = %q, want the loop's own note on the forced final", note)
			}
			note, _ := m["truncation_note"].(string)
			if !strings.Contains(note, "cut") || !strings.Contains(note, "partial") {
				t.Errorf("truncation_note = %q, want a sentence saying the answer was cut and is partial", note)
			}
			if tc.loopFit {
				if got, _ := m["final_budget_fit"].(float64); int(got) != final {
					t.Errorf("final_budget_fit = %v, want %d, the budget the cut final was sent", m["final_budget_fit"], final)
				}
				if !strings.Contains(note, fmt.Sprint(final)) || !strings.Contains(note, "budget_note") {
					t.Errorf("truncation_note = %q, want it to name the %d-token budget the fit opened the final at and point at budget_note", note, final)
				}
				return
			}
			if strings.Contains(note, "budget_note") || strings.Contains(note, "opened at") || strings.Contains(note, fmt.Sprint(prediction)) {
				t.Errorf("truncation_note = %q: the loop did not narrow the final (it opened at the configured %d), so the note must not word the cut as a cut at the fit", note, final)
			}
		})
	}
}
