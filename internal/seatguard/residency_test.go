package seatguard

import (
	"reflect"
	"strings"
	"testing"
)

// matrixYAML is the reference box's routing in miniature: the residents, the
// mutually exclusive interactive set the cascade rungs share with the vLLM
// seats, and a set that runs a small twin BESIDE the three-card seat. The
// shape (vars, evict_costs, "+residents & (a | b)") is llama-swap's matrix
// grammar as the live config writes it.
const matrixYAML = `
models:
  embeddinggemma: {cmd: "x", ttl: 300}
  bge-reranker-v2-m3: {cmd: "x", ttl: 300}
  qwen3.8-27b-vllm-3card:
    proxy: "http://127.0.0.1:18797"
    aliases: [agent-pool]
  qwen3.8-27b-vllm:
    aliases: [agent-pool-2card]
  gemma-4-e4b:
    aliases: [offload-e4b, gemma4-e4b]
  gemma-4-e2b: {aliases: [gemma4-e2b]}
  gemma-4-12b: {}
  gemma-4-26b: {aliases: [gemma4-26b-a4b]}
  gemma-4-e4b-display: {}
  whisper-stt: {}
  loner: {}
matrix:
  vars:
    emb: embeddinggemma
    rer: bge-reranker-v2-m3
    q38v: qwen3.8-27b-vllm
    q38v3: qwen3.8-27b-vllm-3card
    g26: gemma-4-26b
    g12: gemma-4-12b
    ge4: gemma-4-e4b
    ge2: gemma-4-e2b
    ge4d: gemma-4-e4b-display
    wsp: whisper-stt
  evict_costs:
    emb: 1000
    rer: 1000
  sets:
    residents: "emb & rer"
    interactive: "+residents & (q38v | q38v3 | g26 | g12 | ge4 | ge2)"
    display: "+residents & q38v3 & ge4d"
    stt: "+residents & wsp"
`

func mustParse(t *testing.T, text string) Residency {
	t.Helper()
	r, err := ParseResidency([]byte(text))
	if err != nil {
		t.Fatalf("ParseResidency: %v", err)
	}
	return r
}

// TestMatrixRungEvictsTheLoadedSeat is the measured eviction, reproduced from
// the config alone: with the three-card seat loaded beside the embedder, a
// request for the e4b rung makes llama-swap pick the interactive set, whose
// only combination holding e4b does not hold the seat. The live log line was
// `model=gemma-4-e4b set=interactive … evict=[qwen3.8-27b-vllm-3card]
// target=[bge-reranker-v2-m3 embeddinggemma gemma-4-e4b] cost=1`.
func TestMatrixRungEvictsTheLoadedSeat(t *testing.T) {
	r := mustParse(t, matrixYAML)
	ev := r.Evicts("gemma-4-e4b", []string{"embeddinggemma", "qwen3.8-27b-vllm-3card"})
	if !reflect.DeepEqual(ev.Evicted, []string{"qwen3.8-27b-vllm-3card"}) {
		t.Fatalf("Evicted = %v, want [qwen3.8-27b-vllm-3card]", ev.Evicted)
	}
	if ev.Set != "interactive" || ev.Cost != 1 || ev.Alone {
		t.Fatalf("set=%q cost=%d alone=%v, want interactive/1/false", ev.Set, ev.Cost, ev.Alone)
	}
	want := []string{"bge-reranker-v2-m3", "embeddinggemma", "gemma-4-e4b"}
	if !reflect.DeepEqual(ev.Target, want) {
		t.Fatalf("Target = %v, want %v (the llama-swap log's target)", ev.Target, want)
	}
	// An alias names the same model: the harness binds rungs by alias.
	if got := r.Evicts("offload-e4b", []string{"embeddinggemma", "qwen3.8-27b-vllm-3card"}); !reflect.DeepEqual(got.Evicted, ev.Evicted) {
		t.Fatalf("alias offload-e4b: Evicted = %v, want %v", got.Evicted, ev.Evicted)
	}
	// Every cascade rung is in the same exclusive set.
	for _, rung := range []string{"gemma-4-e2b", "gemma-4-12b", "gemma4-26b-a4b"} {
		if got := r.Evicts(rung, []string{"embeddinggemma", "qwen3.8-27b-vllm-3card"}); !reflect.DeepEqual(got.Evicted, []string{"qwen3.8-27b-vllm-3card"}) {
			t.Errorf("%s: Evicted = %v, want the seat", rung, got.Evicted)
		}
	}
}

// TestMatrixCoexistingModelEvictsNothing: a set that names the seat AND the
// model is a combination llama-swap can pick without touching the seat.
func TestMatrixCoexistingModelEvictsNothing(t *testing.T) {
	r := mustParse(t, matrixYAML)
	ev := r.Evicts("gemma-4-e4b-display", []string{"embeddinggemma", "qwen3.8-27b-vllm-3card"})
	if len(ev.Evicted) != 0 {
		t.Fatalf("Evicted = %v, want nothing (the display set runs the twin beside the seat)", ev.Evicted)
	}
	if ev.Set != "display" {
		t.Fatalf("Set = %q, want display", ev.Set)
	}
}

// TestMatrixAlreadyRunningEvictsNothing: llama-swap serves a loaded model
// without consulting the solver at all.
func TestMatrixAlreadyRunningEvictsNothing(t *testing.T) {
	r := mustParse(t, matrixYAML)
	if ev := r.Evicts("agent-pool", []string{"embeddinggemma", "qwen3.8-27b-vllm-3card"}); len(ev.Evicted) != 0 {
		t.Fatalf("the seat's own alias: Evicted = %v, want nothing", ev.Evicted)
	}
}

// TestMatrixModelInNoSetRunsAlone: "A model that appears in no set can only
// run on its own" (llama-swap's config reference).
func TestMatrixModelInNoSetRunsAlone(t *testing.T) {
	r := mustParse(t, matrixYAML)
	ev := r.Evicts("loner", []string{"embeddinggemma", "qwen3.8-27b-vllm-3card"})
	if !ev.Alone {
		t.Fatal("a model in no set must be reported as running alone")
	}
	if !reflect.DeepEqual(ev.Evicted, []string{"embeddinggemma", "qwen3.8-27b-vllm-3card"}) {
		t.Fatalf("Evicted = %v, want every running model", ev.Evicted)
	}
}

// TestMatrixSolverPrefersKeepingTheCostliest reproduces the solver rule —
// among the sets holding the requested model, the one whose evictions cost
// least — and pins that a tie reports the UNION of what the tied sets would
// evict: the guard asks "might this evict the seat", and a tie llama-swap
// breaks the other way is a yes.
func TestMatrixSolverPrefersKeepingTheCostliest(t *testing.T) {
	const cfg = `
models: {m: {}, x: {}, v: {}}
matrix:
  evict_costs: {x: 1000}
  sets:
    a: "m & x"
    b: "m & v"
`
	r := mustParse(t, cfg)
	if ev := r.Evicts("m", []string{"x", "v"}); !reflect.DeepEqual(ev.Evicted, []string{"v"}) || ev.Set != "a" || ev.Cost != 1 {
		t.Fatalf("costly x: got %+v, want set a evicting [v] at cost 1", ev)
	}
	const costlyV = `
models: {m: {}, x: {}, v: {}}
matrix:
  evict_costs: {v: 5000, x: 10}
  sets:
    a: "m & x"
    b: "m & v"
`
	if ev := mustParse(t, costlyV).Evicts("m", []string{"x", "v"}); !reflect.DeepEqual(ev.Evicted, []string{"x"}) || ev.Set != "b" {
		t.Fatalf("costly v: got %+v, want set b evicting [x]", ev)
	}
	const tie = `
models: {m: {}, x: {}, v: {}}
matrix:
  sets:
    a: "m & x"
    b: "m & v"
`
	if ev := mustParse(t, tie).Evicts("m", []string{"x", "v"}); !reflect.DeepEqual(ev.Evicted, []string{"v", "x"}) {
		t.Fatalf("tie: Evicted = %v, want the union [v x]", ev.Evicted)
	}
}

// TestMatrixExpansion covers the grammar llama-swap documents: & is AND, | is
// OR, parentheses group, +ref inlines another set; & binds tighter than |.
func TestMatrixExpansion(t *testing.T) {
	const cfg = `
models: {a: {}, b: {}, c: {}, d: {}, e: {}}
matrix:
  sets:
    base: "e"
    grid: "(a | b) & (c | d)"
    mixed: "a & b | c"
    nested: "+base & (a | +base)"
`
	r := mustParse(t, cfg)
	got := map[string][]string{}
	for _, s := range r.sets {
		got[s.name] = append(got[s.name], strings.Join(s.ids, "+"))
	}
	want := map[string][]string{
		"base":   {"e"},
		"grid":   {"a+c", "a+d", "b+c", "b+d"},
		"mixed":  {"a+b", "c"},
		"nested": {"a+e", "e"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expansion:\n got %v\nwant %v", got, want)
	}
}

// TestMatrixRejectsWhatLlamaSwapRejects: an unknown set reference, a
// reference cycle and a malformed expression are configs llama-swap refuses to
// load, and a guard must never claim to know the co-residency of a config
// that cannot be served. The caller treats the error as "unknown".
func TestMatrixRejectsWhatLlamaSwapRejects(t *testing.T) {
	for name, cfg := range map[string]string{
		"unknown ref": "models: {a: {}}\nmatrix:\n  sets:\n    s: \"a & +nope\"\n",
		"cycle":       "models: {a: {}}\nmatrix:\n  sets:\n    s: \"a & +t\"\n    t: \"+s\"\n",
		"dangling &":  "models: {a: {}}\nmatrix:\n  sets:\n    s: \"a &\"\n",
		"open paren":  "models: {a: {}}\nmatrix:\n  sets:\n    s: \"(a | a\"\n",
		"not yaml":    "models: [\n",
	} {
		if _, err := ParseResidency([]byte(cfg)); err == nil {
			t.Errorf("%s: ParseResidency accepted it", name)
		}
	}
}

// TestNestedRoutingBlockIsRead: current llama-swap nests the routing under
// routing.router.settings and selects the engine with routing.router.use; the
// reference box's build reads the legacy top-level `matrix:`. Both describe
// the same thing.
func TestNestedRoutingBlockIsRead(t *testing.T) {
	const cfg = `
models: {seat: {}, rung: {}, twin: {}}
routing:
  router:
    use: matrix
    settings:
      matrix:
        sets:
          interactive: "seat | rung"
          beside: "seat & twin"
`
	r := mustParse(t, cfg)
	if ev := r.Evicts("rung", []string{"seat"}); !reflect.DeepEqual(ev.Evicted, []string{"seat"}) {
		t.Fatalf("rung: Evicted = %v, want [seat]", ev.Evicted)
	}
	if ev := r.Evicts("twin", []string{"seat"}); len(ev.Evicted) != 0 {
		t.Fatalf("twin: Evicted = %v, want nothing", ev.Evicted)
	}
}

// TestGroupsEngine follows llama-swap's group semantics: swap (one member of
// the group at a time), exclusive (a member unloads every other group), and
// persistent (no other group can unload this one's members). A model in no
// group belongs to the default group, which swaps and is exclusive.
func TestGroupsEngine(t *testing.T) {
	const cfg = `
models: {seat: {}, e4b: {}, e2b: {}, emb: {}, stray: {}}
groups:
  cascade:
    swap: true
    exclusive: true
    members: [e4b, e2b]
  seats:
    swap: true
    exclusive: false
    members: [seat]
  memory:
    persistent: true
    swap: false
    exclusive: false
    members: [emb]
`
	r := mustParse(t, cfg)
	if ev := r.Evicts("e4b", []string{"seat", "emb"}); !reflect.DeepEqual(ev.Evicted, []string{"seat"}) {
		t.Fatalf("exclusive cascade group: Evicted = %v, want [seat] (emb is persistent)", ev.Evicted)
	}
	if ev := r.Evicts("e2b", []string{"e4b"}); !reflect.DeepEqual(ev.Evicted, []string{"e4b"}) {
		t.Fatalf("swap group: Evicted = %v, want [e4b]", ev.Evicted)
	}
	if ev := r.Evicts("seat", []string{"e4b", "emb"}); len(ev.Evicted) != 0 {
		t.Fatalf("non-exclusive seat group: Evicted = %v, want nothing", ev.Evicted)
	}
	if ev := r.Evicts("stray", []string{"seat", "emb"}); !reflect.DeepEqual(ev.Evicted, []string{"seat"}) {
		t.Fatalf("default group: Evicted = %v, want [seat]", ev.Evicted)
	}
}

// TestNoRoutingEverythingIsExclusive: a config with neither block puts every
// model in the default group, so any request evicts whatever is loaded.
func TestNoRoutingEverythingIsExclusive(t *testing.T) {
	r := mustParse(t, "models: {seat: {}, rung: {}}\n")
	if ev := r.Evicts("rung", []string{"seat"}); !reflect.DeepEqual(ev.Evicted, []string{"seat"}) {
		t.Fatalf("Evicted = %v, want [seat]", ev.Evicted)
	}
}

// TestEvictionString is the decision's log shape, which mirrors llama-swap's
// own matrix line so the two can be read side by side.
func TestEvictionString(t *testing.T) {
	r := mustParse(t, matrixYAML)
	ev := r.Evicts("gemma-4-e4b", []string{"embeddinggemma", "qwen3.8-27b-vllm-3card"})
	const want = "model=gemma-4-e4b set=interactive evict=[qwen3.8-27b-vllm-3card] target=[bge-reranker-v2-m3 embeddinggemma gemma-4-e4b] cost=1"
	if got := ev.Describe("gemma-4-e4b"); got != want {
		t.Fatalf("Describe:\n got %q\nwant %q", got, want)
	}
}
