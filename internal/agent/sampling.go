// sampling.go — the planner's decoding policy, per call (register D-95b).
//
// The policy travels on the CONTEXT, exactly like the non-thinking render
// (thinking.go): the Client interface keeps its signature, every fake keeps
// working, and a client that ignores the value simply decodes as it always
// did. The loop sets it once per step — the planner policy on tool steps, the
// final policy on the answer turn and its re-issues — so a seat can be greedy
// while it calls tools and sampled while it writes prose, which is what the
// Qwen3-class non-thinking recommendation actually asks for.
package agent

import (
	"context"

	"github.com/dmmdea/offload-harness/internal/core"
)

// Sampling is the loop's alias for the wire/config type, so callers inside
// this package never need the core import just to name a policy.
type Sampling = core.AgentSampling

type samplingCtxKey struct{}

// ContextWithSampling returns a context whose next Chat call decodes under s.
// A zero policy is carried too: it renders today's request byte for byte, and
// carrying it keeps "unset" and "explicitly default" the same thing.
func ContextWithSampling(ctx context.Context, s *Sampling) context.Context {
	if s.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, samplingCtxKey{}, s)
}

// SamplingFromContext returns the policy for this call, nil when none was set.
func SamplingFromContext(ctx context.Context) *Sampling {
	s, _ := ctx.Value(samplingCtxKey{}).(*Sampling)
	return s
}

// WithSampling installs the seat's decoding policy: planner for every tool
// step, final for the answer turn and its re-issues. A nil final falls back to
// the planner policy; both nil is today's behaviour exactly.
func (l *Loop) WithSampling(planner, final *Sampling) *Loop {
	l.sampling, l.samplingFinal = planner, final
	return l
}

// samplingFor resolves the policy for the call about to be made.
func (l *Loop) samplingFor(isFinal bool) *Sampling {
	if isFinal && !l.samplingFinal.IsZero() {
		return l.samplingFinal
	}
	return l.sampling
}
