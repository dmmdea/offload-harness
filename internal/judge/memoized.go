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

// NewMemoizedEmbedder builds the usual Embedder and returns its Embed function
// wrapped in the process-wide embed memo, plus the memo itself for stats.
//
// The memo is a strict optimisation: when opts.Path is empty, when the store
// cannot be opened (another process holds it), or on any read problem, the
// returned function is the plain live embedder. Callers therefore never branch
// on whether memoization succeeded — there is exactly one code path.
//
// The returned *Memo may be nil; every Memo method tolerates a nil receiver.
func NewMemoizedEmbedder(endpoint, model string, timeout time.Duration, opts MemoOptions) (func(string) ([]float64, error), *embedmemo.Memo) {
	e := NewEmbedder(endpoint, model, timeout)
	if opts.Path == "" {
		return e.Embed, nil
	}
	// The embedder id is part of the memo key, so a model switch can never serve
	// the previous model's vectors — see internal/embedmemo's package doc.
	m, err := embedmemo.Shared(opts.Path, model, opts.Epoch, opts.MaxEntries)
	if err != nil || m == nil {
		return e.Embed, nil
	}
	return m.Wrap(e.Embed), m
}
