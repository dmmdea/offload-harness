package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Setup actions (ADR 0036, P2 of the envharness port): tool calls a contract
// asks the loop to REPLAY before the model's first turn, so the transcript the
// model first sees already holds the observations it would otherwise spend its
// first steps collecting. envharness's Setup component replays a fixed action
// list on reset, not charged to the episode budget; here the replay is charged
// to the wall (it runs inside the contract's deadline) and never to max_steps.
//
// Why this exists: the delegation-log corpus (2026-09-01…07, 1,172 rows) holds
// 117 failed or deferred 4B-seat rows, every one grounded on context docs, and
// 89 of them stop at exactly two steps — list_dir then read_file, the two
// calls it takes to find the document the node itself wrote into the read
// root. A seeded transcript starts the model where those two steps end.
//
// The shape is deliberately the one the seat produces on its own (an
// assistant turn carrying tool calls, then one tool result per call) and NOT
// a user message holding the document text: to a small seat a user turn is a
// question (measured 2026-09-03 — the few-shot exemplar's user turn became the
// goal the 4B answered), while a tool result is data it asked for.

// AgentSetupActionsMax bounds a contract's replay list. Eight is more than a
// grounded contract needs (one read per context doc, ≤ AgentContextMaxDocs
// would be 16 — but a contract that needs sixteen pre-reads is asking the
// seat to hold sixteen documents at once, which no small seat does well).
const AgentSetupActionsMax = 8

// AgentSetupArgsMaxBytes bounds one action's argument object on the wire.
const AgentSetupArgsMaxBytes = 4 << 10

// ErrAgentSetupActions wraps every setup-action validation failure so a door
// can classify it by identity (a contract-class defer: the contract, not the
// box, is what needs fixing).
var ErrAgentSetupActions = errors.New("agent contract: setup_actions")

// AgentSetupAction is one replayed tool call: the tool by name and its
// argument object as the model would have sent it.
type AgentSetupAction struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args,omitempty"`
}

// ArgsJSON returns the argument object as the loop dispatches it: "{}" for an
// absent/empty args so a tool's decoder always sees an object.
func (a AgentSetupAction) ArgsJSON() string {
	s := strings.TrimSpace(string(a.Args))
	if s == "" || s == "null" {
		return "{}"
	}
	return s
}

// ValidateAgentSetupActions holds a replay list to the wire rules: count,
// tool-name shape (a bare identifier — the same characters a tool spec name
// carries; whitespace inside or around it is a bug, not a name), and a
// bounded JSON OBJECT per action (an array or scalar is rejected here, not
// discovered as a decoder error inside the tool on the node). Whether the
// tool EXISTS on the executing seat is a run-time fact (profiles narrow the
// set per box), so it is answered by the loop as an observation, never here.
func ValidateAgentSetupActions(actions []AgentSetupAction) error {
	if len(actions) > AgentSetupActionsMax {
		return fmt.Errorf("%w: %d actions exceeds the max of %d", ErrAgentSetupActions, len(actions), AgentSetupActionsMax)
	}
	for i, a := range actions {
		if a.Tool == "" || strings.TrimSpace(a.Tool) != a.Tool || !validToolName(a.Tool) {
			return fmt.Errorf("%w: action %d: tool name %q is not a bare tool identifier", ErrAgentSetupActions, i, a.Tool)
		}
		raw := strings.TrimSpace(string(a.Args))
		if raw == "" || raw == "null" {
			continue
		}
		if len(raw) > AgentSetupArgsMaxBytes {
			return fmt.Errorf("%w: action %d (%s): args %d bytes exceeds the max of %d", ErrAgentSetupActions, i, a.Tool, len(raw), AgentSetupArgsMaxBytes)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			return fmt.Errorf("%w: action %d (%s): args must be a JSON object: %v", ErrAgentSetupActions, i, a.Tool, err)
		}
	}
	return nil
}

// validToolName accepts what tool specs are named with: letters, digits,
// underscore, dash, dot — nothing that a chat template or a JSON key would
// need to escape.
func validToolName(s string) bool {
	if len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// SeedContextReads builds the replay list a node can prepend for a grounded
// contract it is about to run: one read_file per context doc, in contract
// order, named exactly as the node materializes them (the doc name IS the
// file name under the read root). The node knows the names; the delegator
// would only be guessing them — which is why this is a node-side key
// (agent_seed_context_reads) and not something every contract author types.
// nil for a contract with no docs.
func SeedContextReads(docs []ContextDoc) []AgentSetupAction {
	if len(docs) == 0 {
		return nil
	}
	out := make([]AgentSetupAction, 0, len(docs))
	for _, d := range docs {
		if len(out) == AgentSetupActionsMax {
			break
		}
		args, _ := json.Marshal(map[string]string{"path": d.Name})
		out = append(out, AgentSetupAction{Tool: "read_file", Args: args})
	}
	return out
}
