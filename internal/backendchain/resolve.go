package backendchain

import "context"

// Prober judges one candidate. Implementations must not load a model unless they were asked to run a
// completion probe.
type Prober interface {
	Probe(ctx context.Context, c Candidate) Verdict
}

// ProberFunc adapts a function to Prober.
type ProberFunc func(ctx context.Context, c Candidate) Verdict

// Probe calls f.
func (f ProberFunc) Probe(ctx context.Context, c Candidate) Verdict { return f(ctx, c) }

// Resolve walks a lane's chain in order and reports the first candidate whose probe passes.
func Resolve(ctx context.Context, lane Lane, p Prober) Entry {
	return Entry{}
}
