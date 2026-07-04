package pipeline

import (
	"os"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/knn"
	"github.com/dmmdea/offload-harness/internal/router"
)

func baseCfg() config.Config {
	c := config.Default()
	c.CachePath, c.LedgerPath = "", "" // keep New cheap; not used by these methods
	return c
}

// Off-by-default invariant: with knn nil + embed nil, skipSmallEntry ignores it.
func TestKNNOffByDefaultKeepsEntry(t *testing.T) {
	p := &Pipeline{cfg: baseCfg()} // knn nil, embed nil, router nil
	chain := p.modelChain(core.TaskClassify, map[string]float64{}, false)
	if len(chain) == 0 || chain[0] != p.cfg.TriageModel {
		t.Fatalf("default entry must be the E2B triage model, got %v", chain)
	}
}

// A true knnSkip removes the E2B entry from the chain.
func TestKNNSkipDropsE2BEntry(t *testing.T) {
	p := &Pipeline{cfg: baseCfg()}
	chain := p.modelChain(core.TaskClassify, map[string]float64{}, true)
	for _, m := range chain {
		if m == p.cfg.TriageModel {
			t.Fatalf("knnSkip=true must drop E2B (%s) from %v", p.cfg.TriageModel, chain)
		}
	}
	if chain[0] != p.cfg.Model {
		t.Fatalf("knnSkip=true must enter at E4B (%s), got %v", p.cfg.Model, chain)
	}
}

// knnPreferLargerEntry is disabled when knn/embed are nil.
func TestKNNPreferDisabledWhenNil(t *testing.T) {
	p := &Pipeline{cfg: baseCfg()}
	if p.knnPreferLargerEntry(core.TaskClassify, "some input") {
		t.Fatal("nil knn/embed: want false")
	}
}

// With a loaded substrate + a fake embedder that lands in the reject cluster,
// the kNN prefers the larger entry.
func TestKNNPreferUsesSubstrate(t *testing.T) {
	ix := &knn.Index{} // empty; replace below via Load on a temp file
	_ = ix
	tmp := t.TempDir() + "/k.jsonl"
	for _, r := range []knn.Row{
		{Task: "classify", Vec: []float64{0, 1}, Accept: false},
		{Task: "classify", Vec: []float64{0.1, 0.9}, Accept: false},
		{Task: "classify", Vec: []float64{1, 0}, Accept: true},
	} {
		if err := knn.Append(tmp, r); err != nil {
			t.Fatal(err)
		}
	}
	p := &Pipeline{cfg: baseCfg()}
	p.cfg.KNNMinNeighbors = 2
	p.cfg.KNNPreFilterK = 2
	p.knn = knn.Load(tmp)
	p.embed = func(string) ([]float64, error) { return []float64{0.05, 0.95}, nil } // reject cluster
	if !p.knnPreferLargerEntry(core.TaskClassify, "x") {
		t.Fatal("query near reject cluster: want true (skip E2B)")
	}
}

// The kNN yields to a trained router (HasTask true) without embedding.
func TestKNNYieldsToTrainedRouter(t *testing.T) {
	tmp := t.TempDir() + "/k.jsonl"
	for i := 0; i < 3; i++ {
		_ = knn.Append(tmp, knn.Row{Task: "classify", Vec: []float64{0, 1}, Accept: false})
	}
	p := &Pipeline{cfg: baseCfg()}
	p.cfg.KNNMinNeighbors = 2
	p.knn = knn.Load(tmp)
	embedded := false
	p.embed = func(string) ([]float64, error) { embedded = true; return []float64{0, 1}, nil }
	p.router = loadFakeRouterWithClassify(t) // helper below
	if p.knnPreferLargerEntry(core.TaskClassify, "x") {
		t.Fatal("router trained for classify: kNN must yield (false)")
	}
	if embedded {
		t.Fatal("router trained: must NOT pay the embed cost")
	}
}

// Fail-open: an embed error returns false.
func TestKNNEmbedErrorFailsOpen(t *testing.T) {
	tmp := t.TempDir() + "/k.jsonl"
	for i := 0; i < 3; i++ {
		_ = knn.Append(tmp, knn.Row{Task: "classify", Vec: []float64{0, 1}, Accept: false})
	}
	p := &Pipeline{cfg: baseCfg()}
	p.cfg.KNNMinNeighbors = 2
	p.knn = knn.Load(tmp)
	p.embed = func(string) ([]float64, error) { return nil, errBoom }
	if p.knnPreferLargerEntry(core.TaskClassify, "x") {
		t.Fatal("embed error: want false (fail-open)")
	}
}

var errBoom = errTest("boom")

type errTest string

func (e errTest) Error() string { return string(e) }

func loadFakeRouterWithClassify(t *testing.T) *router.Model {
	t.Helper()
	p := t.TempDir() + "/router-weights.json"
	// One trained task "classify" with a single feature so Load parses it.
	const w = `{"tasks":{"classify":{"features":["len_chars"],"weights":[0,0],"means":{"len_chars":0},"stds":{"len_chars":1}}}}`
	if err := os.WriteFile(p, []byte(w), 0o644); err != nil {
		t.Fatal(err)
	}
	return router.Load(p)
}
