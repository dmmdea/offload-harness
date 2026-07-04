package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/confhead"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// writeAtomic writes a complete file via tmp+rename, mirroring how the artifact
// writers behave, so the test never stages a torn file for the reloader.
func writeAtomic(t *testing.T, path string, b []byte) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename: %v", err)
	}
}

// reloadCfg returns a Default config with the self-learning paths pointed at a
// fresh temp dir and all the live network/file dependencies disabled, so a
// Pipeline can be constructed without a server and the reloader can be driven
// directly via reloadOnce().
func reloadCfg(t *testing.T) config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Endpoint = "http://127.0.0.1:0" // never actually called in these tests
	cfg.CachePath = ""
	cfg.LedgerPath = ""
	cfg.ThresholdsPath = filepath.Join(dir, "thresholds.json")
	cfg.RouterWeightsPath = filepath.Join(dir, "router-weights.json")
	cfg.TierOverridesPath = filepath.Join(dir, "tier_overrides.json")
	cfg.ConfHeadPath = filepath.Join(dir, "confhead-weights.json")
	cfg.ConfHeadThresholdsPath = filepath.Join(dir, "confhead-thresholds.json")
	cfg.ConfHeadLabelsPath = ""
	return cfg
}

// TestReloadOnce: a thresholds.json rewrite goes live on the next reloadOnce(),
// and a garbage rewrite keeps the last-good value (fail-open).
func TestReloadOnce(t *testing.T) {
	cfg := reloadCfg(t)
	writeAtomic(t, cfg.ThresholdsPath, []byte(`{"triage":0.5}`))

	p := New(cfg, nil, nil, nil)
	if got := p.marginThreshold(core.TaskTriage); got != 0.5 {
		t.Fatalf("initial load: marginThreshold(triage)=%v, want 0.5", got)
	}

	// Rewrite to 0.7 and reload -> must go live.
	writeAtomic(t, cfg.ThresholdsPath, []byte(`{"triage":0.7}`))
	p.reloadOnce()
	if got := p.marginThreshold(core.TaskTriage); got != 0.7 {
		t.Fatalf("after reload: marginThreshold(triage)=%v, want 0.7", got)
	}

	// Garbage bytes -> loader returns nil -> last-good (0.7) kept.
	writeAtomic(t, cfg.ThresholdsPath, []byte(`{not valid json`))
	p.reloadOnce()
	if got := p.marginThreshold(core.TaskTriage); got != 0.7 {
		t.Fatalf("after garbage: marginThreshold(triage)=%v, want 0.7 (last-good)", got)
	}

	// A self-heal: a transient bad read must not advance the recorded hash, so
	// once a NEW good value lands it is picked up on the next tick.
	writeAtomic(t, cfg.ThresholdsPath, []byte(`{"triage":0.9}`))
	p.reloadOnce()
	if got := p.marginThreshold(core.TaskTriage); got != 0.9 {
		t.Fatalf("after recovery: marginThreshold(triage)=%v, want 0.9", got)
	}
}

// genConfhead builds a confhead weights file + thresholds file for a sentinel
// generation. The head is trained so Predict("triage", ...) is deterministic and
// the per-task threshold pairs with it. We don't assert the head's exact value;
// the consistency check is structural: head and thresholds must come from the
// SAME generation.
func writeConfheadGen(t *testing.T, cfg config.Config, gen string) {
	t.Helper()
	// Train a real head on synthetic rows so confhead.Load yields a non-nil model
	// with a "triage" task. The two generations differ only by the synthetic
	// margin band, which is enough for Load to succeed and Predict to be stable.
	var es []ledger.Entry
	band := 0.95
	if gen == "B" {
		band = 0.05
	}
	for i := 0; i < 200; i++ {
		good := i%2 == 0
		m := band
		g := good
		if !good {
			m = 1 - band
		}
		es = append(es, ledger.Entry{
			Task: "triage", Margin: m, InputChars: 200,
			Feat:     map[string]float64{"len_chars": 200, "n_words": 30},
			Grounded: bptrP(g),
		})
	}
	// Persist the head by training to disk: write the synthetic ledger, then call
	// confhead.Train so the on-disk file matches confhead.Load's exact schema.
	dir := filepath.Dir(cfg.ConfHeadPath)
	lp := filepath.Join(dir, "gen-ledger.jsonl")
	f, err := os.Create(lp)
	if err != nil {
		t.Fatalf("gen ledger: %v", err)
	}
	enc := json.NewEncoder(f)
	for _, e := range es {
		if err := enc.Encode(e); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	f.Close()
	if _, err := confhead.Train(lp, "", cfg.ConfHeadPath); err != nil {
		t.Fatalf("gen %s: train: %v", gen, err)
	}
	// Pair the thresholds to the same generation via a sentinel value so a crossed
	// pairing is detectable from the THRESHOLD side: gen A => 0.10, gen B => 0.90.
	tau := genTau(gen)
	tb, _ := json.Marshal(map[string]float64{"triage": tau})
	writeAtomic(t, cfg.ConfHeadThresholdsPath, tb)
}

// genTau is the sentinel per-task threshold for a generation: gen A => 0.10,
// gen B => 0.90. The head's generation is identified independently (genOfHead);
// a consistent snapshot must have head-gen == threshold-gen.
func genTau(gen string) float64 {
	if gen == "B" {
		return 0.90
	}
	return 0.10
}

// genOfThresholds maps a thresholds snapshot back to its generation via the
// sentinel tau (0.10 => A, 0.90 => B). Returns "" if neither.
func genOfThresholds(thr map[string]float64) string {
	switch thr["triage"] {
	case 0.10:
		return "A"
	case 0.90:
		return "B"
	}
	return ""
}

// genProbe is a fixed feature vector with a HIGH margin. Gen A learned
// high-margin => correct, so it predicts a HIGH p(correct) on this probe; gen B
// learned high-margin => wrong, so it predicts a LOW p(correct). The two bands
// are well separated, so a midpoint split cleanly identifies the head's gen.
var genProbe = map[string]float64{"len_chars": 200, "n_words": 30, "margin": 0.95}

// genOfHead identifies which generation a head came from by its prediction on
// genProbe: gen A predicts high (>=0.5), gen B predicts low (<0.5). Returns ""
// for a nil/untrained head.
func genOfHead(head *confhead.Model) string {
	if head == nil {
		return ""
	}
	pc := head.Predict("triage", genProbe)
	if pc < 0 {
		return ""
	}
	if pc >= 0.5 {
		return "A"
	}
	return "B"
}

// TestConfheadSnapConsistency: while a reload swaps the confhead head AND its
// thresholds from gen A to gen B (and back), a concurrent reader calling
// confheadSnap() must NEVER observe a crossed generation — i.e. the head's gen
// (identified by its prediction band) must always equal the thresholds' gen
// (identified by the sentinel tau). Because confheadSnap returns both under ONE
// RLock AND reloadOnce swaps the pair under ONE write lock, a crossed
// (gen-A head, gen-B thresholds) pair is impossible.
func TestConfheadSnapConsistency(t *testing.T) {
	cfg := reloadCfg(t)
	cfg.ConfHeadEnabled = true

	// Teeth guard: confirm the two generations' heads are actually
	// distinguishable on genProbe (gen A high, gen B low). If they weren't, the
	// crossed-generation check would be vacuous.
	writeConfheadGen(t, cfg, "B")
	if g := genOfHead(confhead.Load(cfg.ConfHeadPath)); g != "B" {
		t.Fatalf("teeth: gen B head classified as %q, want B (probe bands not separated)", g)
	}
	writeConfheadGen(t, cfg, "A")
	if g := genOfHead(confhead.Load(cfg.ConfHeadPath)); g != "A" {
		t.Fatalf("teeth: gen A head classified as %q, want A (probe bands not separated)", g)
	}

	p := New(cfg, nil, nil, nil)
	if head, thr := p.confheadSnap(); genOfHead(head) != "A" || genOfThresholds(thr) != "A" {
		t.Fatalf("initial: head-gen=%q thr-gen=%q, want both A", genOfHead(head), genOfThresholds(thr))
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Writer: flip between gen A and gen B on disk and reload, repeatedly. Each
	// flip rewrites BOTH the head and the thresholds files to the same gen.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			writeConfheadGen(t, cfg, "B")
			p.reloadOnce()
			writeConfheadGen(t, cfg, "A")
			p.reloadOnce()
		}
	}()

	// Readers: snapshot head+thresholds together; the head's gen and the
	// thresholds' gen must always agree. A mismatch is a torn/crossed read.
	var crossed atomic.Bool
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20000; i++ {
				head, thr := p.confheadSnap()
				hg, tg := genOfHead(head), genOfThresholds(thr)
				if hg == "" || tg == "" {
					continue // a not-yet-classifiable in-flight state; not a cross
				}
				if hg != tg {
					crossed.Store(true)
					return
				}
			}
		}()
	}

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	if crossed.Load() {
		t.Fatal("observed a crossed confhead generation (head and thresholds from different reloads)")
	}
}

// TestConfheadSnapAtomicUnderLock is the DETERMINISTIC teeth-check for P1: it
// performs the head+thresholds swap while HOLDING learnMu and sleeps between the
// two field writes, deliberately widening the mid-swap window. Because
// confheadSnap takes the same RLock, a reader cannot observe the intermediate
// (gen-B head, gen-A thresholds) state — it blocks until the writer releases the
// lock, then sees the fully-swapped pair. A reader that re-read p.confhead /
// p.confThresholds in two separate RLocks (the bug P1 forbids) WOULD see the
// cross; confheadSnap by construction cannot. The wide window guarantees teeth.
func TestConfheadSnapAtomicUnderLock(t *testing.T) {
	cfg := reloadCfg(t)
	cfg.ConfHeadEnabled = true
	writeConfheadGen(t, cfg, "A")
	p := New(cfg, nil, nil, nil)

	headA, _ := p.confheadSnap()
	writeConfheadGen(t, cfg, "B")
	headB := confhead.Load(cfg.ConfHeadPath)
	if genOfHead(headA) != "A" || genOfHead(headB) != "B" {
		t.Fatalf("setup: headA gen=%q headB gen=%q, want A and B", genOfHead(headA), genOfHead(headB))
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var crossed atomic.Bool

	// Readers loop on confheadSnap and assert the pair is never crossed.
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				head, thr := p.confheadSnap()
				hg, tg := genOfHead(head), genOfThresholds(thr)
				if hg != "" && tg != "" && hg != tg {
					crossed.Store(true)
					return
				}
			}
		}()
	}

	// Writer: flip the in-memory pair A<->B under a SINGLE held lock, with a wide
	// sleep between the two field assignments to maximize the cross window.
	for round := 0; round < 50; round++ {
		toHead, toThr := headB, genTau("B")
		if round%2 == 1 {
			toHead, toThr = headA, genTau("A")
		}
		p.learnMu.Lock()
		p.confhead = toHead
		time.Sleep(50 * time.Microsecond) // widen the mid-swap window
		p.confThresholds = map[string]float64{"triage": toThr}
		p.learnMu.Unlock()
		time.Sleep(20 * time.Microsecond)
	}

	close(stop)
	wg.Wait()
	if crossed.Load() {
		t.Fatal("confheadSnap returned a crossed (head, thresholds) pair despite the single-lock swap")
	}
}

// TestConfheadPairPartialFailKeepsBoth: the confhead head + thresholds are a
// COUPLED generation. If a reload sees both files changed but ONE side is
// garbage (a torn/mid-write thresholds file while the head is complete), the
// reloader must swap NEITHER — keeping the prior good PAIR — rather than
// installing (new-head, old-thresholds), which would be a crossed steady state.
// This is fail-open at the pair granularity; the next tick (when both are good)
// adopts the new generation.
func TestConfheadPairPartialFailKeepsBoth(t *testing.T) {
	cfg := reloadCfg(t)
	cfg.ConfHeadEnabled = true
	writeConfheadGen(t, cfg, "A")
	p := New(cfg, nil, nil, nil)

	if head, thr := p.confheadSnap(); genOfHead(head) != "A" || genOfThresholds(thr) != "A" {
		t.Fatalf("initial: head-gen=%q thr-gen=%q, want both A", genOfHead(head), genOfThresholds(thr))
	}

	// Stage gen B for BOTH files, but corrupt the thresholds file so only the head
	// loads cleanly this tick.
	writeConfheadGen(t, cfg, "B")
	writeAtomic(t, cfg.ConfHeadThresholdsPath, []byte(`{garbage`))
	p.reloadOnce()

	// Neither side may have advanced: the pair stays gen A (no crossed state).
	if head, thr := p.confheadSnap(); genOfHead(head) != "A" || genOfThresholds(thr) != "A" {
		t.Fatalf("after partial-fail: head-gen=%q thr-gen=%q, want both still A (no crossed swap)",
			genOfHead(head), genOfThresholds(thr))
	}

	// Self-heal: once the thresholds file is whole again (gen B), the next tick
	// adopts the full gen-B pair.
	writeConfheadGen(t, cfg, "B")
	p.reloadOnce()
	if head, thr := p.confheadSnap(); genOfHead(head) != "B" || genOfThresholds(thr) != "B" {
		t.Fatalf("after heal: head-gen=%q thr-gen=%q, want both B", genOfHead(head), genOfThresholds(thr))
	}
}

// TestReloaderRace: 100 goroutines hammer marginThreshold/modelChain while a
// loop atomically rewrites the watched files and reloadOnce() runs. Must be
// clean under -race.
func TestReloaderRace(t *testing.T) {
	cfg := reloadCfg(t)
	writeAtomic(t, cfg.ThresholdsPath, []byte(`{"triage":0.5,"classify":0.5}`))
	writeAtomic(t, cfg.RouterWeightsPath, []byte(`{"tasks":{}}`))
	writeAtomic(t, cfg.TierOverridesPath, []byte(`{"tier_timeouts_ms":{},"degraded":[]}`))

	p := New(cfg, nil, nil, nil)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Writer + reloader loop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			i++
			v := 0.5 + float64(i%5)/10.0
			writeAtomic(t, cfg.ThresholdsPath, []byte(`{"triage":`+jsonFloat(v)+`}`))
			writeAtomic(t, cfg.TierOverridesPath, []byte(`{"tier_timeouts_ms":{"x":`+jsonInt(i%10)+`},"degraded":["e2b"]}`))
			p.reloadOnce()
		}
	}()

	feat := map[string]float64{"len_chars": 100}
	for g := 0; g < 100; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				_ = p.marginThreshold(core.TaskTriage)
				_ = p.modelChain(core.TaskClassify, feat, false)
			}
		}()
	}

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func jsonFloat(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func jsonInt(v int) string {
	b, _ := json.Marshal(v)
	return string(b)
}
