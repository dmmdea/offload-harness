package recipe

import "github.com/dmmdea/offload-harness/internal/core"

// Class says how a rule key may be scored (ADR 0036, the 2026-10-15 amendment).
type Class string

const (
	// ClassReplay: the rule only removes or caps calls. A recorded trace holds the tool, the fate
	// and the stop class of every step, so it can be read against the rule without the seat
	// answering again: the branch (which tools may be called) and the parallelism (how many calls
	// of one tool a run makes), with the stop class of the run as what such a cut can move.
	ClassReplay Class = "replay"
	// ClassLiveAB: the rule changes an argument or what the seat reads. Every later step of the run
	// changes with it, and no recording says what the seat would have done next, so only a live
	// A/B on the seat can score it. recipe-audit is that A/B.
	ClassLiveAB Class = "live_ab"
)

// vocabulary is the rule vocabulary in the order core.AgentEnvRules declares it, with the class of each key.
var vocabulary = []struct {
	key   string
	class Class
}{
	{"deny_tools", ClassReplay},
	{"allow_tools", ClassReplay},
	{"max_calls_per_tool", ClassReplay},
	{"arg_limits", ClassLiveAB},
	{"max_observation_tokens", ClassLiveAB},
	{"observation_strip", ClassLiveAB},
	{"rewrite_error", ClassLiveAB},
}

// ClassOf returns the scoring class of a rule key, and false for a key outside the vocabulary.
func ClassOf(key string) (Class, bool) {
	for _, v := range vocabulary {
		if v.key == key {
			return v.class, true
		}
	}
	return "", false
}

// Keys lists the rule keys of the vocabulary, in the order core.AgentEnvRules declares them.
func Keys() []string {
	out := make([]string, len(vocabulary))
	for i, v := range vocabulary {
		out[i] = v.key
	}
	return out
}

// LiveABKeys lists the keys a rule set uses that need a live A/B, in vocabulary order.
func LiveABKeys(r *core.AgentEnvRules) []string {
	if r == nil {
		return nil
	}
	used := map[string]bool{
		"deny_tools":             len(r.DenyTools) > 0,
		"allow_tools":            len(r.AllowTools) > 0,
		"max_calls_per_tool":     len(r.MaxCallsPerTool) > 0,
		"arg_limits":             len(r.ArgLimits) > 0,
		"max_observation_tokens": r.MaxObservationTokens > 0,
		"observation_strip":      len(r.ObservationStrip) > 0,
		"rewrite_error":          len(r.RewriteError) > 0,
	}
	var out []string
	for _, v := range vocabulary {
		if v.class == ClassLiveAB && used[v.key] {
			out = append(out, v.key)
		}
	}
	return out
}
