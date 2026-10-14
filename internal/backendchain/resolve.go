package backendchain

import (
	"context"
	"fmt"
	"strings"
)

// Prober judges one candidate. Implementations must not load a model unless they were asked to run a
// completion probe.
type Prober interface {
	Probe(ctx context.Context, c Candidate) Verdict
}

// ProberFunc adapts a function to Prober.
type ProberFunc func(ctx context.Context, c Candidate) Verdict

// Probe calls f.
func (f ProberFunc) Probe(ctx context.Context, c Candidate) Verdict { return f(ctx, c) }

// depth orders the probe levels, shallowest first. An unfamiliar level sorts with the shallowest.
func depth(level string) int {
	switch level {
	case ProbeEndpoint:
		return 1
	case ProbeCompletion:
		return 2
	}
	return 0
}

// kindOf is the engine kind a candidate reports: its own, or EngineUnknown when nothing said.
func kindOf(c Candidate) string {
	if strings.TrimSpace(c.Engine) == "" {
		return EngineUnknown
	}
	return c.Engine
}

// seatLabel is "llama.cpp seat X", or just "seat X" when the engine kind is not known.
func seatLabel(kind, seat string) string {
	if kind == EngineUnknown {
		return "seat " + seat
	}
	return kind + " seat " + seat
}

// Resolve reports which backend answers for a lane, and why.
//
// A lane routes to the seat its config binds (the Bound candidate), so the engine that answers is that
// seat's engine and nothing else's: a preferred seat of the chain that is up while the lane is bound to
// another does not answer for it, and the reason says so ("the preferred vllm seat X is available (...)
// but does not answer for this lane, which is bound to Y"). A bound seat that fails is "none", with every
// failure named, even when another seat of the chain is up: the lane would call the bound seat and fail.
// A preferred seat that fails while the lane is bound to a later one is named in front of the answer
// ("vllm seat X unavailable: ...; fell to llama.cpp seat Y: ...").
//
// A candidate whose probe saw nothing (a timeout: Verdict.Unknown) is neither up nor down. Bound to it, the
// lane reads "unknown"; a preferred one is named as not confirmed and does not answer for a lane bound
// elsewhere.
//
// A lane with no binding (a chain whose seats the config does not name) takes the first candidate whose
// probe passes or could not be confirmed. When none does the answer is "none", at the deepest probe level
// any candidate reached.
func Resolve(ctx context.Context, lane Lane, p Prober) Entry {
	chain := make([]string, 0, len(lane.Chain))
	for _, c := range lane.Chain {
		chain = append(chain, kindOf(c))
	}
	e := Entry{Lane: lane.Name, BackendChain: chain, BackendProbe: ProbeProcess}
	if len(lane.Chain) == 0 {
		e.ActiveBackend = EngineNone
		e.BackendReason = "no backend is configured for this lane"
		return e
	}
	boundIdx := -1
	for i, c := range lane.Chain {
		if c.Bound {
			boundIdx = i
			break
		}
	}
	bound := ""
	if boundIdx >= 0 {
		bound = lane.Chain[boundIdx].Seat
	}
	var failed, available []string
	deepest := ProbeProcess
	for i, c := range lane.Chain {
		v := p.Probe(ctx, c)
		level := v.Probe
		if level == "" {
			level = ProbeProcess
		}
		if depth(level) > depth(deepest) {
			deepest = level
		}
		label := seatLabel(chain[i], c.Seat)
		if !v.OK && !v.Unknown {
			failed = append(failed, fmt.Sprintf("%s unavailable: %s", label, v.Detail))
			continue
		}
		if boundIdx >= 0 && i != boundIdx {
			prefix := "the "
			if i < boundIdx {
				prefix = "the preferred "
			}
			if v.Unknown {
				available = append(available, fmt.Sprintf("%s%s could not be confirmed (%s) and does not answer for this lane, which is bound to %s", prefix, label, v.Detail, bound))
			} else {
				available = append(available, fmt.Sprintf("%s%s is available (%s) but does not answer for this lane, which is bound to %s", prefix, label, v.Detail, bound))
			}
			continue
		}
		e.ActiveBackend, e.Seat, e.BackendProbe = chain[i], c.Seat, level
		if v.Unknown {
			e.ActiveBackend = EngineUnknown
		}
		reason := fmt.Sprintf("%s: %s", label, v.Detail)
		if len(failed) > 0 {
			reason = strings.Join(failed, "; ") + "; fell to " + reason
		}
		if len(available) > 0 {
			reason += "; " + strings.Join(available, "; ")
		}
		e.BackendReason = reason
		return e
	}
	seat := lane.Chain[0].Seat
	if boundIdx >= 0 {
		seat = bound
	}
	e.ActiveBackend, e.Seat, e.BackendProbe = EngineNone, seat, deepest
	e.BackendReason = strings.Join(append(failed, available...), "; ")
	return e
}
