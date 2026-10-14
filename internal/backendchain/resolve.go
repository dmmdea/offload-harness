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

// Resolve walks a lane's chain in order and reports the first candidate whose probe passes. The failures
// that came before it are named in the reason ("vllm seat X unavailable: ...; fell to llama.cpp seat Y:
// ..."), so a lane served by its second backend says why it is not served by its first. When none passes
// the answer is "none", at the deepest probe level any candidate reached, with every failure named.
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
	bound := ""
	for _, c := range lane.Chain {
		if c.Bound {
			bound = c.Seat
			break
		}
	}
	var failed []string
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
		if v.OK {
			e.ActiveBackend, e.Seat, e.BackendProbe = chain[i], c.Seat, level
			reason := fmt.Sprintf("%s seat %s: %s", chain[i], c.Seat, v.Detail)
			if len(failed) > 0 {
				reason = strings.Join(failed, "; ") + "; fell to " + reason
			}
			if bound != "" && !c.Bound {
				reason += fmt.Sprintf(" (the lane is bound to %s, not to this seat)", bound)
			}
			e.BackendReason = reason
			return e
		}
		failed = append(failed, fmt.Sprintf("%s seat %s unavailable: %s", chain[i], c.Seat, v.Detail))
	}
	e.ActiveBackend, e.Seat, e.BackendProbe = EngineNone, lane.Chain[0].Seat, deepest
	e.BackendReason = strings.Join(failed, "; ")
	return e
}
