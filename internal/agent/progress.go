package agent

import "context"

// ProgressFunc hears a completion's progress as the seat streams it: the
// tokens generated so far for THIS call. Content, reasoning and tool-call
// argument deltas all count — a token is a token to a liveness rule, and a
// model that is thinking is producing.
type ProgressFunc func(tokensSoFar int)

type progressKey struct{}

// ContextWithProgress installs fn for the Chat calls made under ctx — the
// same pattern as ContextWithSampling: per-call behaviour rides the context so
// Chat's signature never changes. A nil fn removes an inherited one.
func ContextWithProgress(ctx context.Context, fn ProgressFunc) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

// ProgressFromContext returns the installed ProgressFunc, or nil.
func ProgressFromContext(ctx context.Context) ProgressFunc {
	fn, _ := ctx.Value(progressKey{}).(ProgressFunc)
	return fn
}
