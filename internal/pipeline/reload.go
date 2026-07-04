package pipeline

// A2: fail-safe hot-reload of the self-learning artifacts.
//
// The long-running MCP server (`local-offload mcp`) loads the self-learning
// fields once at pipeline construction. A nightly retrain rewrites those files,
// but without a reloader the new weights never go live until a manual restart.
// This file adds a background poll reloader that picks up changed artifacts
// within one tick — WITHOUT adding any IO/parse to the request hot path.
//
// Design (roast-hardened):
//   - P1 no torn read: confheadSnap() returns the head AND its thresholds as ONE
//     snapshot under a single RLock. The gate uses only those two locals.
//   - P2 kNN excluded: knn-index.jsonl is APPEND-grown and knn.Load returns a
//     truncated-but-valid index on a torn line, so "fail-open last-good" does not
//     protect it and its size grows on every append (constant re-swap). It is
//     therefore NOT in the watched set — knn/embed reload ONLY on MCP restart.
//   - P3 content-hash detection: a file changes iff its sha256 changes. (mtime,
//     size) is rejected — tiny fixed-shape JSON can change value at the same size.
//   - P4 atomic writers: every whole-file writer does tmp+rename, so the reloader
//     only ever reads a complete file (see calibration/router/confhead/health/main).
//   - P5 lifecycle: the tick body recovers from any loader panic (must never kill
//     the server) and stops via a stop channel on cleanup.
//   - Fail-open last-good: a loader returning nil/empty keeps the prior value AND
//     does not advance the recorded hash, so a transient bad read self-heals.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"time"

	"github.com/dmmdea/offload-harness/internal/confhead"
	"github.com/dmmdea/offload-harness/internal/knn"
	"github.com/dmmdea/offload-harness/internal/router"
)

// defaultReloadInterval is the poll cadence used by runMCP when it starts the
// reloader. ~30s: a nightly-adopted weight goes live within one tick, and the
// cost (a few KB-scale sha256 reads) is trivially off the request path.
const defaultReloadInterval = 30 * time.Second

// ---- read-path snapshot accessors (uncontended RLock; zero IO/parse) --------

// thresholdsSnap returns the current per-task margin-threshold map. The map
// header is returned by value under RLock; the map is only ever REPLACED (never
// mutated in place) by a reload, so the caller reads a stable snapshot.
func (p *Pipeline) thresholdsSnap() map[string]float64 {
	p.learnMu.RLock()
	defer p.learnMu.RUnlock()
	return p.thresholds
}

// routerSnap returns the current entry-tier router (nil = static rule).
func (p *Pipeline) routerSnap() *router.Model {
	p.learnMu.RLock()
	defer p.learnMu.RUnlock()
	return p.router
}

// overridesSnap returns the current health-driven tier overrides (nil = none).
func (p *Pipeline) overridesSnap() *tierOverrides {
	p.learnMu.RLock()
	defer p.learnMu.RUnlock()
	return p.overrides
}

// confheadSnap returns the correctness head AND its per-task thresholds together
// as ONE snapshot under a single RLock (P1: no torn read). A reload that changes
// both swaps them under the write lock atomically, so this never returns a
// crossed (old-head, new-thresholds) pair. The confhead gate MUST use only these
// two locals — never re-read p.confhead / p.confThresholds afterward.
func (p *Pipeline) confheadSnap() (*confhead.Model, map[string]float64) {
	p.learnMu.RLock()
	defer p.learnMu.RUnlock()
	return p.confhead, p.confThresholds
}

// knnSnap returns the kNN index and embedder together. kNN is NOT poll-reloaded
// (P2), so this only ever changes on MCP restart; the accessor still takes the
// RLock so reads are consistent with the rest of the learn-state.
func (p *Pipeline) knnSnap() (*knn.Index, func(string) ([]float64, error)) {
	p.learnMu.RLock()
	defer p.learnMu.RUnlock()
	return p.knn, p.embed
}

// ---- content hashing --------------------------------------------------------

// fileContentHash returns the hex sha256 of the file at path, or "" if the file
// is absent/unreadable. KB-scale artifacts make this cheap. An empty hash for a
// missing file means a later first-write is detected as a change.
func fileContentHash(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// watchedPaths is the set of artifact files the poll reloader watches, gated on
// the SAME cfg flags as New(): the confhead pair is watched only when
// ConfHeadEnabled. knn-index.jsonl is deliberately EXCLUDED (P2: append-file).
func (p *Pipeline) watchedPaths() []string {
	paths := []string{p.cfg.ThresholdsPath, p.cfg.RouterWeightsPath, p.cfg.TierOverridesPath}
	if p.cfg.ConfHeadEnabled {
		paths = append(paths, p.cfg.ConfHeadPath, p.cfg.ConfHeadThresholdsPath)
	}
	out := paths[:0:0]
	for _, pth := range paths {
		if pth != "" {
			out = append(out, pth)
		}
	}
	return out
}

// ---- the reload tick --------------------------------------------------------

// reloadOnce runs one poll pass: for each watched artifact whose content hash
// changed, it re-runs that file's EXISTING loader and atomically swaps the
// in-memory value under the write lock. Fail-open: a loader returning nil/empty
// (bad/partial file) keeps the prior value AND does not advance the recorded
// hash, so a transient bad read self-heals on the next tick. confhead + its
// thresholds are reloaded as a PAIR so the head and thresholds never cross
// generations on the read path.
//
// reloadOnce is the unit-testable body of one tick; StartReloader calls it on a
// timer. It never touches the grammar path, never blocks the request path, and
// never reloads the append-grown kNN index (P2).
func (p *Pipeline) reloadOnce() {
	cfg := p.cfg

	// thresholds.json -> p.thresholds
	if cfg.ThresholdsPath != "" {
		if h, changed := p.hashChanged(cfg.ThresholdsPath); changed {
			if m := loadThresholds(cfg.ThresholdsPath); len(m) > 0 {
				p.learnMu.Lock()
				p.thresholds = m
				p.learnMu.Unlock()
				p.commitHash(cfg.ThresholdsPath, h) // advance only on a good load
			}
		}
	}

	// router-weights.json -> p.router
	if cfg.RouterWeightsPath != "" {
		if h, changed := p.hashChanged(cfg.RouterWeightsPath); changed {
			if r := router.Load(cfg.RouterWeightsPath); r != nil {
				p.learnMu.Lock()
				p.router = r
				p.learnMu.Unlock()
				p.commitHash(cfg.RouterWeightsPath, h)
			}
		}
	}

	// tier_overrides.json -> p.overrides
	if cfg.TierOverridesPath != "" {
		if h, changed := p.hashChanged(cfg.TierOverridesPath); changed {
			if o := loadOverrides(cfg.TierOverridesPath); o != nil {
				p.learnMu.Lock()
				p.overrides = o
				p.learnMu.Unlock()
				p.commitHash(cfg.TierOverridesPath, h)
			}
		}
	}

	// confhead pair: reload the head and its thresholds TOGETHER as ONE atomic
	// swap (P1). Gated on ConfHeadEnabled, exactly as New() gates the initial
	// load. A nightly retrain rewrites BOTH files, so if either content hash
	// changed we re-load both and swap them under a SINGLE write lock — never two
	// separate locks. That guarantees the read path (confheadSnap) can never
	// observe a crossed (old-head, new-thresholds) pair, even mid-reload. Each
	// side is fail-open independently: a side whose fresh load is nil/empty keeps
	// its prior value and does NOT advance its recorded hash (self-heals).
	if cfg.ConfHeadEnabled {
		hHead, headChanged := "", false
		hThr, thrChanged := "", false
		if cfg.ConfHeadPath != "" {
			hHead, headChanged = p.hashChanged(cfg.ConfHeadPath)
		}
		if cfg.ConfHeadThresholdsPath != "" {
			hThr, thrChanged = p.hashChanged(cfg.ConfHeadThresholdsPath)
		}
		if headChanged || thrChanged {
			// Load the CHANGED side(s) OUTSIDE the lock (IO/parse off the lock-hold
			// path). The head and thresholds are a COUPLED generation, so a changed
			// side that fails to load (nil/empty — a torn/mid-write file) aborts the
			// WHOLE pair swap: we keep the prior good pair and advance NO hash, so the
			// next tick (when both files are whole) adopts the new generation. This
			// avoids ever installing a crossed (new-head, old-thresholds) steady
			// state, while staying fail-open.
			var newHead *confhead.Model
			var newThr map[string]float64
			ok := true
			if headChanged {
				if m := confhead.Load(cfg.ConfHeadPath); m != nil {
					newHead = m
				} else {
					ok = false
				}
			}
			if thrChanged {
				if m := confhead.LoadThresholds(cfg.ConfHeadThresholdsPath); len(m) > 0 {
					newThr = m
				} else {
					ok = false
				}
			}
			if ok {
				// Single write lock: swap both changed sides together so a reader
				// never sees the head from one generation paired with thresholds from
				// another (P1, write-side).
				p.learnMu.Lock()
				if headChanged {
					p.confhead = newHead
				}
				if thrChanged {
					p.confThresholds = newThr
				}
				p.learnMu.Unlock()
				if headChanged {
					p.commitHash(cfg.ConfHeadPath, hHead)
				}
				if thrChanged {
					p.commitHash(cfg.ConfHeadThresholdsPath, hThr)
				}
			}
		}
	}
}

// hashChanged computes the current content hash of path and reports whether it
// differs from the last recorded hash. It does NOT record the new hash — the
// caller advances the recorded hash only after a successful load (so a transient
// bad read self-heals). Reads the recorded hash under RLock.
func (p *Pipeline) hashChanged(path string) (string, bool) {
	h := fileContentHash(path)
	p.learnMu.RLock()
	prev := p.learnHashes[path]
	p.learnMu.RUnlock()
	return h, h != prev
}

// commitHash records the content hash of a successfully reloaded file.
func (p *Pipeline) commitHash(path, h string) {
	p.learnMu.Lock()
	p.learnHashes[path] = h
	p.learnMu.Unlock()
}

// StartReloader launches a background goroutine that polls the watched artifacts
// every interval and hot-swaps any that changed (via reloadOnce). It returns a
// stop func that halts the goroutine and blocks until it has exited. It is
// started ONLY from runMCP (never from New()), so CLI one-shots never leak a
// goroutine and stay byte-identical. The tick body recovers from any loader
// panic so a corrupt artifact can never kill the long-running server (P5).
func (p *Pipeline) StartReloader(interval time.Duration) (stop func()) {
	if interval <= 0 {
		interval = defaultReloadInterval
	}
	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-t.C:
				p.safeReloadOnce()
			}
		}
	}()
	var once bool
	return func() {
		if once {
			return
		}
		once = true
		close(stopCh)
		<-done
	}
}

// safeReloadOnce runs reloadOnce with a recover() guard so a panic in any loader
// (a pathological artifact) is contained to one tick and never crashes the MCP
// server (P5).
func (p *Pipeline) safeReloadOnce() {
	defer func() { _ = recover() }()
	p.reloadOnce()
}
