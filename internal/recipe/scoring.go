package recipe

import "github.com/dmmdea/offload-harness/internal/core"

// Class says how a rule key may be scored.
type Class string

const (
	// ClassReplay: a recorded trace is enough to read the rule's effect.
	ClassReplay Class = "replay"
	// ClassLiveAB: only a live run on the seat can say what the rule does.
	ClassLiveAB Class = "live_ab"
)

// ClassOf returns the scoring class of a rule key.
func ClassOf(key string) (Class, bool) { return "", false }

// Keys lists the rule keys of the vocabulary.
func Keys() []string { return nil }

// LiveABKeys lists the keys a rule set uses that need a live A/B, in vocabulary order.
func LiveABKeys(r *core.AgentEnvRules) []string { return nil }
