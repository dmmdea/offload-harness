// seatguard_test.go pins the cascade seat guard end to end through Run: a
// Tier-1 call (summarize / classify / extract / triage) that lands while a
// vLLM seat is loaded never asks this box's llama-swap for a rung whose load
// would evict that seat. The fakes are this box's llama-swap (roster,
// /running, completions, and a trap on /upstream) and a remote lane; the
// co-residency rule is a real llama-swap config on disk, read through
// serving_config_path exactly as production reads it.
package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/seatguard"
)

const guardSeat = "qwen3.8-27b-vllm-3card"

// guardYAML is the reference box's routing in miniature: every cascade rung
// shares the mutually exclusive interactive set with the vLLM seat.
const guardYAML = `
models:
  embeddinggemma: {}
  qwen3.8-27b-vllm-3card: {aliases: [agent-pool]}
  gemma-4-e2b: {}
  gemma-4-e4b: {aliases: [offload-e4b]}
  gemma-4-12b: {}
  gemma-4-26b: {}
matrix:
  vars:
    emb: embeddinggemma
    q38v3: qwen3.8-27b-vllm-3card
    ge2: gemma-4-e2b
    ge4: gemma-4-e4b
    g12: gemma-4-12b
    g26: gemma-4-26b
  evict_costs: {emb: 1000}
  sets:
    interactive: "emb & (q38v3 | ge2 | ge4 | g12 | g26)"
`

// validAnswer satisfies summarize, classify (labels animal/finance) and extract
// (field "title") at once; extra keys are not what any gate checks.
const validAnswer = `{"summary":"a summary","bullets":["one"],"label":"animal","confidence":0.95,"title":"a cat"}`

// guardFake is one llama-swap stand-in. As THIS box it serves the roster,
// /running and completions, and records every /upstream request (the tier
// re-pack probes a rung's window there, which LOADS the rung on demand). As a
// LANE it serves the roster and completions only.
type guardFake struct {
	srv *httptest.Server

	mu            sync.Mutex
	roster        []string // model ids the roster serves
	running       []string // "<id>:<state>"
	runningBroken bool
	bodies        []map[string]any
	upstream      []string
	answer        map[string]string // per-model content; default validAnswer
	delay         time.Duration
}

func newGuardFake(t *testing.T, roster []string, running ...string) *guardFake {
	t.Helper()
	f := &guardFake{roster: roster, running: running, answer: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/models":
			data := make([]any, 0, len(f.roster))
			for _, m := range f.roster {
				entry := map[string]any{"id": m}
				if m == guardSeat {
					entry["meta"] = map[string]any{"llamaswap": map[string]any{"aliases": []string{"agent-pool"}}}
				}
				data = append(data, entry)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
		case r.URL.Path == "/running":
			if f.runningBroken {
				http.Error(w, "down", http.StatusInternalServerError)
				return
			}
			rows := make([]map[string]string, 0, len(f.running))
			for _, e := range f.running {
				id, state, _ := strings.Cut(e, ":")
				rows = append(rows, map[string]string{"model": id, "state": state, "proxy": "http://127.0.0.1:1"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"running": rows})
		case strings.HasPrefix(r.URL.Path, "/upstream/"):
			f.upstream = append(f.upstream, r.URL.Path)
			http.NotFound(w, r)
		case r.URL.Path == "/v1/chat/completions":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			f.bodies = append(f.bodies, body)
			model, _ := body["model"].(string)
			content := validAnswer
			if a, ok := f.answer[model]; ok {
				content = a
			}
			if f.delay > 0 {
				f.mu.Unlock()
				time.Sleep(f.delay)
				f.mu.Lock()
			}
			_ = json.NewEncoder(w).Encode(json.RawMessage(fakeChat{content: content, finishReason: "stop", promptTokens: 40}.marshal()))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// models lists the model each recorded completion named, in order.
func (f *guardFake) models() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.bodies))
	for _, b := range f.bodies {
		m, _ := b["model"].(string)
		out = append(out, m)
	}
	return out
}

func (f *guardFake) bodyFor(model string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.bodies {
		if m, _ := b["model"].(string); m == model {
			return b
		}
	}
	return nil
}

func (f *guardFake) upstreamHits() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.upstream...)
}

var guardRungs = []string{"gemma-4-e2b", "gemma-4-e4b", "gemma-4-12b", "gemma-4-26b"}

// guardPipeline builds a pipeline over local (this box) and, when lane is
// non-nil, a cascade lane for every rung — with production's residency gate
// (RosterResident) and busy gates that never fire, so any lane call seen is
// the guard's doing.
func guardPipeline(t *testing.T, local, lane *guardFake, mutate func(*config.Config)) *Pipeline {
	t.Helper()
	return guardPipelineWith(t, local, lane, llamaclient.RosterResident(), mutate)
}

// guardPipelineWith is guardPipeline with the lane residency gate supplied.
func guardPipelineWith(t *testing.T, local, lane *guardFake, resident func(base, model string) bool, mutate func(*config.Config)) *Pipeline {
	t.Helper()
	cfg := config.Default()
	cfg.Endpoint = local.srv.URL
	cfg.Model = "gemma-4-e4b"
	cfg.TriageModel = "gemma-4-e2b"
	cfg.EscalationModel = "gemma-4-12b"
	cfg.ReasoningModel = "gemma-4-26b"
	cfg.MaxRetries = 0
	cfg.ThresholdsPath, cfg.RouterWeightsPath, cfg.TierOverridesPath = "", "", ""
	cfg.ConfHeadLabelsPath, cfg.CachePath = "", ""
	cfg.ShadowEnabled = false
	cfg.VLLMSeats = []string{guardSeat}
	yamlPath := filepath.Join(t.TempDir(), "llama-swap.yaml")
	if err := os.WriteFile(yamlPath, []byte(guardYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.ServingConfigPath = yamlPath
	client := llamaclient.New(local.srv.URL, cfg.CompletionPath, "", 10*time.Second)
	if lane != nil {
		cfg.CascadeRemoteLanes = map[string]string{}
		for _, r := range guardRungs {
			cfg.CascadeRemoteLanes[r] = lane.srv.URL
		}
		client = client.WithRemoteLanes(cfg.CascadeRemoteLanes, func() bool { return false }, nil, resident)
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return New(cfg, client, nil, nil)
}

func guardLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })
	return &buf
}

var (
	guardSummarizeReq = core.Request{Task: core.TaskSummarize, Input: strings.Repeat("The cat sat on the mat and looked at the dog. ", 6)}
	guardClassifyReq  = core.Request{Task: core.TaskClassify, Input: strings.Repeat("the cat sat on the mat and looked at the dog ", 4),
		Params: map[string]any{"labels": []string{"animal", "finance"}}}
)

func noRungAsked(t *testing.T, f *guardFake, where string) {
	t.Helper()
	rungs := append(append([]string(nil), guardRungs...), "offload-e4b")
	for _, m := range f.models() {
		for _, r := range rungs {
			if m == r {
				t.Errorf("%s was asked for rung %s while the seat was loaded: that request is the eviction", where, m)
			}
		}
	}
}

// TestSeatGuardLoadedSeatTier1CallNeverPicksAnEvictingRung: the seat is
// loaded, no lane exists, and every Tier-1 task is served by the loaded seat
// (a D-129 cascade rung: vLLM's structured_outputs, no grammar) — this box's
// llama-swap is never asked for a rung.
func TestSeatGuardLoadedSeatTier1CallNeverPicksAnEvictingRung(t *testing.T) {
	logs := guardLog(t)
	local := newGuardFake(t, append([]string{guardSeat}, guardRungs...), "embeddinggemma:ready", guardSeat+":ready")
	p := guardPipeline(t, local, nil, nil)

	for name, req := range map[string]core.Request{"summarize": guardSummarizeReq, "classify": guardClassifyReq} {
		res := p.Run(context.Background(), req)
		if !res.OK || res.Meta.Model != guardSeat {
			t.Fatalf("%s: ok=%v model=%q reason=%q, want an answer from %s", name, res.OK, res.Meta.Model, res.Reason, guardSeat)
		}
	}
	noRungAsked(t, local, "this box")
	b := local.bodyFor(guardSeat)
	if b == nil {
		t.Fatal("the seat received no completion")
	}
	if _, ok := b["grammar"]; ok {
		t.Error("the seat was sent llama.cpp's grammar field, which vLLM discards (D-129)")
	}
	if _, ok := b["structured_outputs"]; !ok {
		t.Error("the seat was not constrained by structured_outputs (D-129)")
	}
	const want = "cascade seat guard: model=gemma-4-e4b set=interactive evict=[" + guardSeat + "]"
	if out := logs.String(); !strings.Contains(out, want) || !strings.Contains(out, "served by the loaded seat "+guardSeat) {
		t.Errorf("serve log lacks the decision line %q ... served by the loaded seat:\n%s", want, out)
	}
}

// TestSeatGuardNoSeatLoadedLeavesTheCascadeUnchanged is the control: with no
// vLLM seat loaded the chain, the models asked and the request bodies are
// exactly what the guard-off build produces.
func TestSeatGuardNoSeatLoadedLeavesTheCascadeUnchanged(t *testing.T) {
	run := func(guardOn bool) ([]string, map[string]any) {
		local := newGuardFake(t, append([]string{guardSeat}, guardRungs...), "embeddinggemma:ready")
		p := guardPipeline(t, local, nil, func(c *config.Config) {
			on := guardOn
			c.CascadeSeatGuard = &on
		})
		for _, req := range []core.Request{guardSummarizeReq, guardClassifyReq} {
			if res := p.Run(context.Background(), req); !res.OK {
				t.Fatalf("guard=%v: %s deferred: %s", guardOn, req.Task, res.Reason)
			}
		}
		return local.models(), local.bodyFor("gemma-4-e4b")
	}
	onModels, onBody := run(true)
	offModels, offBody := run(false)
	if strings.Join(onModels, ",") != strings.Join(offModels, ",") {
		t.Fatalf("models asked: guard on %v, guard off %v — they must not differ with no seat loaded", onModels, offModels)
	}
	if want := "gemma-4-e4b,gemma-4-e2b"; strings.Join(onModels, ",") != want {
		t.Fatalf("models asked = %v, want the configured entry rungs %s", onModels, want)
	}
	on, _ := json.Marshal(onBody)
	off, _ := json.Marshal(offBody)
	if !bytes.Equal(on, off) {
		t.Fatalf("the workhorse body differs with the guard on:\n on %s\noff %s", on, off)
	}
}

// TestSeatGuardUnknownSeatStateFailsTowardNotEvicting covers both unknowns.
// A stale verdict that last saw the seat loaded keeps the call on the seat;
// a /running that cannot be read at all names no seat, so the call rides the
// rung's resident lane — off this box, where it cannot evict anything.
func TestSeatGuardUnknownSeatStateFailsTowardNotEvicting(t *testing.T) {
	t.Run("stale reading of a loaded seat", func(t *testing.T) {
		local := newGuardFake(t, append([]string{guardSeat}, guardRungs...), "embeddinggemma:ready", guardSeat+":ready")
		p := guardPipeline(t, local, nil, nil)
		p.seatVerdictFn = func(_ context.Context, model string) seatguard.Verdict {
			if model == guardSeat {
				return seatguard.Verdict{}
			}
			return seatguard.Verdict{Protect: true, Seat: guardSeat, Stale: true, Reason: "model=" + model + " evict=[" + guardSeat + "] (from a stale reading)"}
		}
		if res := p.Run(context.Background(), guardSummarizeReq); !res.OK || res.Meta.Model != guardSeat {
			t.Fatalf("ok=%v model=%q, want the seat", res.OK, res.Meta.Model)
		}
		noRungAsked(t, local, "this box")
	})
	t.Run("unreadable /running with a resident lane", func(t *testing.T) {
		local := newGuardFake(t, append([]string{guardSeat}, guardRungs...))
		local.runningBroken = true
		lane := newGuardFake(t, guardRungs)
		p := guardPipeline(t, local, lane, nil)
		res := p.Run(context.Background(), guardSummarizeReq)
		if !res.OK || res.Meta.Model != "gemma-4-e4b" {
			t.Fatalf("ok=%v model=%q reason=%q, want gemma-4-e4b answered by the lane", res.OK, res.Meta.Model, res.Reason)
		}
		if got := lane.models(); len(got) != 1 || got[0] != "gemma-4-e4b" {
			t.Fatalf("lane asked %v, want [gemma-4-e4b] (the SAME model, elsewhere)", got)
		}
		noRungAsked(t, local, "this box")
	})
}

// TestSeatGuardNoAlternativeIsServedByTheSeatNotRefused: the lane exists but
// does not serve the rungs, and the seat is slow (it is working a long
// session). The call waits its turn in the seat and is answered — never a
// refusal, never a defer from the guard.
func TestSeatGuardNoAlternativeIsServedByTheSeatNotRefused(t *testing.T) {
	local := newGuardFake(t, append([]string{guardSeat}, guardRungs...), "embeddinggemma:ready", guardSeat+":ready")
	local.delay = 300 * time.Millisecond
	lane := newGuardFake(t, []string{"some-other-model"})
	p := guardPipeline(t, local, lane, nil)
	res := p.Run(context.Background(), guardSummarizeReq)
	if !res.OK || res.Deferred || res.Meta.Model != guardSeat {
		t.Fatalf("ok=%v deferred=%v model=%q reason=%q, want an answer from the seat", res.OK, res.Deferred, res.Meta.Model, res.Reason)
	}
	if n := len(lane.models()); n != 0 {
		t.Fatalf("a lane that does not serve the rung received %d call(s)", n)
	}
	noRungAsked(t, local, "this box")
}

// TestSeatGuardRidesAResidentLaneWithTheSameModel: with a lane that serves
// the rung, the rung keeps its model and leaves this box — the existing fleet
// routing rule (routing changes WHERE, never WHICH).
func TestSeatGuardRidesAResidentLaneWithTheSameModel(t *testing.T) {
	logs := guardLog(t)
	local := newGuardFake(t, append([]string{guardSeat}, guardRungs...), "embeddinggemma:ready", guardSeat+":ready")
	lane := newGuardFake(t, guardRungs)
	p := guardPipeline(t, local, lane, nil)
	res := p.Run(context.Background(), guardSummarizeReq)
	if !res.OK || res.Meta.Model != "gemma-4-e4b" {
		t.Fatalf("ok=%v model=%q, want gemma-4-e4b from the lane", res.OK, res.Meta.Model)
	}
	if got := local.models(); len(got) != 0 {
		t.Fatalf("this box was asked for %v; the lane serves the rung", got)
	}
	if out := logs.String(); !strings.Contains(out, "cascade remote lane: gemma-4-e4b -> "+lane.srv.URL) || !strings.Contains(out, "cascade seat guard:") {
		t.Errorf("serve log lacks the guard's lane line:\n%s", out)
	}
}

// TestSeatGuardEscalationNeverProbesTheLocalWindow: a climb re-packs an
// over-long source against the callee's window, probed at this box's
// /upstream/<rung>/props — a request that LOADS the rung here. A rung the
// lane serves must not be probed on this box.
func TestSeatGuardEscalationNeverProbesTheLocalWindow(t *testing.T) {
	local := newGuardFake(t, append([]string{guardSeat}, guardRungs...), "embeddinggemma:ready", guardSeat+":ready")
	lane := newGuardFake(t, guardRungs)
	lane.answer["gemma-4-e4b"] = "no JSON in this reply" // the verifier rejects it: the call climbs
	p := guardPipeline(t, local, lane, func(c *config.Config) { c.MaxInputChars = 400 })
	long := core.Request{Task: core.TaskSummarize, Input: strings.Repeat("A long source sentence about cats and dogs. ", 60)}
	res := p.Run(context.Background(), long)
	if !res.OK || res.Meta.Model != "gemma-4-12b" {
		t.Fatalf("ok=%v model=%q reason=%q, want the escalation rung from the lane", res.OK, res.Meta.Model, res.Reason)
	}
	if hits := local.upstreamHits(); len(hits) != 0 {
		t.Fatalf("this box's /upstream was probed %v: each of those loads a rung and evicts the seat", hits)
	}
	if !strings.Contains(res.Meta.TierPack, "seat guard") {
		t.Errorf("tier_pack = %q, want the row to say why the window was not probed", res.Meta.TierPack)
	}
	noRungAsked(t, local, "this box")
}

// TestSeatGuardReasoningTierNeverEvicts: the terminal reasoning tier is a
// rung too. With no lane it would load the 26B over the seat, so it is not
// run; the seat — already the chain's last rung — is the final attempt.
func TestSeatGuardReasoningTierNeverEvicts(t *testing.T) {
	local := newGuardFake(t, append([]string{guardSeat}, guardRungs...), "embeddinggemma:ready", guardSeat+":ready")
	local.answer[guardSeat] = "no JSON in this reply" // the verifier rejects it: the chain is exhausted
	p := guardPipeline(t, local, nil, nil)
	res := p.Run(context.Background(), guardSummarizeReq)
	if res.OK {
		t.Fatalf("the seat's invalid answer passed: %+v", res)
	}
	if got := local.models(); strings.Join(got, ",") != guardSeat {
		t.Fatalf("models asked = %v, want the seat exactly once and no reasoning rung", got)
	}
}

// residentOnce answers "resident" the FIRST time it is asked about each model
// and "not resident" ever after: the lane is there when the pipeline plans the
// door and gone when the send re-checks it.
func residentOnce() func(base, model string) bool {
	var mu sync.Mutex
	asked := map[string]bool{}
	return func(_, model string) bool {
		mu.Lock()
		defer mu.Unlock()
		first := !asked[model]
		asked[model] = true
		return first
	}
}

// TestSeatGuardLaneGoneBetweenPlanAndSend (review HIGH 2): the plan chose the
// lane from its cached residency; at the send the lane no longer serves the
// rung. The call must not fall through to this box's llama-swap for the rung
// (the eviction): it is served by the loaded seat, and both logs say why.
// With no seat known to substitute, the configured rung runs — logged, never
// refused — which is the same residual the guard documents for an unknown
// reading.
func TestSeatGuardLaneGoneBetweenPlanAndSend(t *testing.T) {
	t.Run("seat known: the seat serves the rung", func(t *testing.T) {
		logs := guardLog(t)
		local := newGuardFake(t, append([]string{guardSeat}, guardRungs...), "embeddinggemma:ready", guardSeat+":ready")
		lane := newGuardFake(t, guardRungs)
		p := guardPipelineWith(t, local, lane, residentOnce(), nil)
		res := p.Run(context.Background(), guardSummarizeReq)
		if !res.OK || res.Meta.Model != guardSeat {
			t.Fatalf("ok=%v model=%q reason=%q, want the loaded seat", res.OK, res.Meta.Model, res.Reason)
		}
		if n := len(lane.models()); n != 0 {
			t.Fatalf("the lane received %d call(s) after it stopped serving the rung", n)
		}
		noRungAsked(t, local, "this box")
		out := logs.String()
		for _, want := range []string{"its cascade lane no longer serves it", "lane gone at send", "served by the loaded seat " + guardSeat} {
			if !strings.Contains(out, want) {
				t.Errorf("serve log lacks %q:\n%s", want, out)
			}
		}
	})
	t.Run("no seat known: the configured rung runs, not refused", func(t *testing.T) {
		logs := guardLog(t)
		local := newGuardFake(t, append([]string{guardSeat}, guardRungs...))
		lane := newGuardFake(t, guardRungs)
		p := guardPipelineWith(t, local, lane, residentOnce(), nil)
		p.seatVerdictFn = func(_ context.Context, model string) seatguard.Verdict {
			return seatguard.Verdict{Protect: true, Stale: true, Reason: "model=" + model + ": unknown"}
		}
		res := p.Run(context.Background(), guardSummarizeReq)
		if !res.OK || res.Meta.Model != "gemma-4-e4b" {
			t.Fatalf("ok=%v model=%q reason=%q, want the configured rung answered here", res.OK, res.Meta.Model, res.Reason)
		}
		if out := logs.String(); !strings.Contains(out, "lane gone at send") || !strings.Contains(out, "no loaded seat is known") {
			t.Errorf("serve log lacks the divergence line:\n%s", out)
		}
	})
}

// TestSeatGuardReasoningLaneGoneAtSend: every chain rung rides its lane and
// fails the verifier; the reasoning tier's lane was resident at the plan and
// is gone at the send. The seat takes the final attempt — the 26B is never
// asked for on this box.
func TestSeatGuardReasoningLaneGoneAtSend(t *testing.T) {
	local := newGuardFake(t, append([]string{guardSeat}, guardRungs...), "embeddinggemma:ready", guardSeat+":ready")
	lane := newGuardFake(t, guardRungs)
	lane.answer["gemma-4-e4b"] = "no JSON in this reply"
	lane.answer["gemma-4-12b"] = "no JSON in this reply"
	var mu sync.Mutex
	asked := 0
	resident := func(_, model string) bool {
		if model != "gemma-4-26b" {
			return true
		}
		mu.Lock()
		defer mu.Unlock()
		asked++
		return asked == 1
	}
	p := guardPipelineWith(t, local, lane, resident, nil)
	res := p.Run(context.Background(), guardSummarizeReq)
	if !res.OK || res.Meta.Model != guardSeat {
		t.Fatalf("ok=%v model=%q reason=%q, want the seat as the final attempt", res.OK, res.Meta.Model, res.Reason)
	}
	if got := strings.Join(lane.models(), ","); got != "gemma-4-e4b,gemma-4-12b" {
		t.Fatalf("lane asked %s, want the two chain rungs only", got)
	}
	if got := strings.Join(local.models(), ","); got != guardSeat {
		t.Fatalf("this box asked %s, want the seat once and never the 26B", got)
	}
}

// reasoningExclusiveYAML is the review's reproduction (PR #448 round 2): the
// fast rungs run BESIDE the seat, and only the reasoning tier is exclusive
// with it. The live config masks this shape, because there every rung shares
// the exclusive set; here the chain rungs pass the guard untouched and only the
// terminal reasoning tier is protected.
const reasoningExclusiveYAML = `
models:
  embeddinggemma: {}
  qwen3.8-27b-vllm-3card: {aliases: [agent-pool]}
  gemma-4-e2b: {}
  gemma-4-e4b: {}
  gemma-4-12b: {}
  gemma-4-26b: {}
matrix:
  vars:
    emb: embeddinggemma
    q38v3: qwen3.8-27b-vllm-3card
    ge2: gemma-4-e2b
    ge4: gemma-4-e4b
    g12: gemma-4-12b
    g26: gemma-4-26b
  sets:
    coexist: "emb & q38v3 & (ge2 | ge4 | g12)"
    reasoning_exclusive: "emb & (q38v3 | g26)"
`

// TestSeatGuardReasoningSubstituteIsAReasoningAttempt (review round 2, item
// 1): the reasoning tier would evict the seat and no lane serves it, so the
// seat takes the terminal attempt. It must take it AS the reasoning tier — one
// attempt with the reasoning budget (task budget + reasoningThinkBudget),
// marked Reasoning, behind the grammar/truncation gate — not as a plain rung
// appended to the chain, which is what the first cut did: the seat ran with
// the rung budget and Reasoning=false.
func TestSeatGuardReasoningSubstituteIsAReasoningAttempt(t *testing.T) {
	local := newGuardFake(t, append([]string{guardSeat}, guardRungs...), "embeddinggemma:ready", guardSeat+":ready")
	local.answer["gemma-4-e4b"] = "no JSON in this reply"
	local.answer["gemma-4-12b"] = "no JSON in this reply"
	p := guardPipeline(t, local, nil, func(c *config.Config) {
		if err := os.WriteFile(c.ServingConfigPath, []byte(reasoningExclusiveYAML), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	res := p.Run(context.Background(), guardSummarizeReq)
	if got, want := strings.Join(local.models(), ","), "gemma-4-e4b,gemma-4-12b,"+guardSeat; got != want {
		t.Fatalf("models asked %s, want %s (the rungs beside the seat, then the seat; never the 26B)", got, want)
	}
	if !res.OK || res.Meta.Model != guardSeat {
		t.Fatalf("ok=%v model=%q reason=%q, want the seat's answer", res.OK, res.Meta.Model, res.Reason)
	}
	if !res.Meta.Reasoning {
		t.Error("the seat answered the reasoning tier's attempt but the result is not marked Reasoning")
	}
	rung, _ := local.bodyFor("gemma-4-e4b")["max_tokens"].(float64)
	seat, _ := local.bodyFor(guardSeat)["max_tokens"].(float64)
	if seat != rung+reasoningThinkBudget {
		t.Errorf("seat max_tokens = %v, want the reasoning budget %v (rung %v + reasoningThinkBudget)", seat, rung+reasoningThinkBudget, rung)
	}
	if b := local.bodyFor(guardSeat); b != nil {
		if _, ok := b["structured_outputs"]; !ok {
			t.Error("the seat was not constrained by structured_outputs: a vLLM seat cannot take the think-wrapped GBNF")
		}
	}
}
