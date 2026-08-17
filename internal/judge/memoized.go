package judge

import (
	"time"

	"github.com/dmmdea/offload-harness/internal/embedmemo"
)

// MemoOptions carries the embed-memo settings a caller has resolved from config.
// Primitives rather than a config.Config so this package keeps its zero-config
// dependency (judge is imported by the pipeline, the drains and the CLI alike).
type MemoOptions struct {
	Path       string // "" disables the memo
	Epoch      string
	MaxEntries int
}

// Memoized bundles a memoized embed function with the memo behind it and the
// reason there is no memo, when there isn't one.
//
// Reason is not decoration. Disabled-by-config, feature-off, lock-timeout,
// permission-denied and corrupt-file are five different answers, and a caller
// that collapses them into "enabled: false" cannot tell an operator which one
// happened. The error is carried here precisely so the reporting surfaces can
// say which.
type Memoized struct {
	Embed  func(string) ([]float64, error)
	Memo   *embedmemo.Memo // nil when memoization is not active
	Reason string          // "" when Memo != nil
}

// NewMemoizedEmbedder builds the usual Embedder and wraps its Embed in the
// process-wide embed memo.
//
// The memo is a strict optimisation: when opts.Path is empty, or the store
// cannot be opened, Embed is the plain live embedder. Callers therefore never
// branch on whether memoization succeeded — there is exactly one code path for
// embedding, and a separate one for REPORTING what happened.
func NewMemoizedEmbedder(endpoint, model string, timeout time.Duration, opts MemoOptions) Memoized {
	e := NewEmbedder(endpoint, model, timeout)
	if opts.Path == "" {
		return Memoized{Embed: e.Embed, Reason: "disabled (embed_memo_enabled=false or embed_memo_path empty)"}
	}
	// The embedder id is part of the memo key AND of the shared-handle identity,
	// so a model switch can neither serve nor be served the other model's vectors.
	m, err := embedmemo.Shared(opts.Path, model, opts.Epoch, opts.MaxEntries)
	if err != nil || m == nil {
		reason := "store unavailable"
		if err != nil {
			reason = "store unavailable: " + err.Error()
		}
		return Memoized{Embed: e.Embed, Reason: reason}
	}
	return Memoized{Embed: m.Wrap(e.Embed), Memo: m}
}

// Similar embeds a and b through the memoized path and returns their cosine
// similarity.
//
// The plain Embedder.Similar sends both texts in ONE batched request. That is
// cheaper on a total miss but cannot memoize, and this call site (the shadow
// drain grading a counterfactual summary against a reference) re-scores the SAME
// reference text across every item of every run — the single most repetitive
// embedding workload in the harness. Two memoized single calls therefore beat
// one batched call in the case that actually dominates, and on a double hit the
// cost is zero calls instead of one.
func (mz Memoized) Similar(a, b string) (float64, error) {
	va, err := mz.Embed(a)
	if err != nil {
		return 0, err
	}
	vb, err := mz.Embed(b)
	if err != nil {
		return 0, err
	}
	return cosine(va, vb), nil
}
