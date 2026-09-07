package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/contextbudget"
	"github.com/dmmdea/offload-harness/internal/core"
)

// Setup replay (core.AgentSetupAction, ADR 0036 P2): tool calls the loop runs
// BEFORE the model's first turn so the first transcript it sees already holds
// the observations it would otherwise spend its first steps collecting.
//
// The replay is shaped exactly like a turn the seat produces on its own — one
// assistant message carrying the tool calls, then one tool result per call —
// because that is the shape every multi-step run already sends back to the
// seat on step two and later. It is NOT a user message holding the document
// text: to a small seat a user turn is a question (the 2026-09-03 few-shot
// leak), while a tool result is data it asked for.
//
// Accounting, stated once so it is consistent (design council 2026-09-07):
//   - filter_action applies (deny/allow withhold, arg_limits clamp); a
//     max_calls_per_tool cap sees the per-run counter, which setup never
//     spends — the model's own budget under that cap stays whole.
//   - the circuit breakers (exact-repeat, same-name, disabledTools) are NOT
//     consulted and NOT fed: the model never issued these calls, so a later
//     identical model call must not read as a repeat of them.
//   - the observation hooks and the loop-boundary cap apply as on any call.
//   - never charged to maxSteps; charged to the wall like everything else.
//   - the results are PINNED for compaction (the lossy rungs keep them; only
//     emergencyShrink, the pin-blind last resort before a dead run, may still
//     cut them — compaction.go) but sit OUTSIDE the protected preamble, and
//     the whole replay is bounded by
//     setupBudget: past it the remaining actions are recorded as not run
//     (status none, note "setup budget …") rather than growing a preamble the
//     seat cannot fit — the run then proceeds with the model reading for
//     itself, which is exactly the pre-key behaviour.
//   - a failing action (unknown tool, tool error) becomes its error
//     observation, never an abort; the trace's Step-0 entries carry the
//     per-action status and SetupRan counts what actually executed.

// SetupAction is the loop-local form of core.AgentSetupAction: the tool by
// name and its argument object as raw JSON.
type SetupAction struct {
	Tool string
	Args string
}

// WithSetupActions installs the replay list. nil/empty = no replay, and the
// loop is byte-identical to a loop that never had the option.
func (l *Loop) WithSetupActions(actions []core.AgentSetupAction) *Loop {
	l.setup = nil
	for _, a := range actions {
		l.setup = append(l.setup, SetupAction{Tool: a.Tool, Args: a.ArgsJSON()})
	}
	return l
}

// setupBudgetTokens is how much of the compaction budget the replay may fill:
// half. The other half is the objective, the system prompt, the tool specs
// and room for the model's own turns — a replay that fills the window is a
// preamble the seat cannot fit, which the loop's own comment on preambleLen
// names as the one shape compaction cannot rescue.
func (l *Loop) setupBudgetTokens() int {
	b := l.budgetForCompaction() / 2
	if b < 256 {
		b = 256
	}
	return b
}

// SetupRan counts the setup actions that EXECUTED (committed or failed) in an
// effect ledger — the number the wire result and the MCP door report. A
// refused, unknown-tool or over-budget action ran nothing and is not counted;
// its Step-0 record says why.
func SetupRan(effects []EffectRecord) int {
	n := 0
	for _, e := range effects {
		if e.Setup && (e.Status == EffectCommitted || e.Status == EffectFailed) {
			n++
		}
	}
	return n
}

// replaySetup runs the installed setup actions and returns the transcript
// with the replay appended (one assistant turn + one tool result per action
// that was attempted), recording each action in effects/ruleHits and pinning
// each result id. No actions = the transcript untouched.
func (l *Loop) replaySetup(ctx context.Context, msgs []Msg, pinned map[string]bool, effects *[]EffectRecord, ruleHits *[]EnvRuleHit, ruleState *EnvRuleState) []Msg {
	if len(l.setup) == 0 {
		return msgs
	}
	budget := l.setupBudgetTokens()
	spent := 0
	var calls []ToolCall
	var results []Msg
	for i, a := range l.setup {
		id := "setup-" + strconv.Itoa(i+1)
		if ctx.Err() != nil {
			*effects = append(*effects, EffectRecord{Step: 0, CallID: id, Tool: a.Tool, Status: EffectNone, Note: "setup: not run: " + ctx.Err().Error(), Setup: true})
			continue
		}
		if spent >= budget {
			*effects = append(*effects, EffectRecord{Step: 0, CallID: id, Tool: a.Tool, Status: EffectNone,
				Note: fmt.Sprintf("setup budget: %d of %d tokens already replayed; the model reads this one itself", spent, budget), Setup: true})
			continue
		}
		call := ToolCall{ID: id, Name: a.Tool, Args: a.Args}
		act := EnvAction{Step: 0, CallID: id, Tool: a.Tool, Args: a.Args}
		var firedRule string
		var content string
		var isErr bool
		var eff EffectStatus
		var blocked *EnvBlocked
		var argNotes []string
		if l.envRules != nil {
			var hits []EnvRuleHit
			act, blocked, hits = l.envRules.FilterAction(ruleState, act)
			*ruleHits = append(*ruleHits, hits...)
			for _, h := range hits {
				firedRule = h.Rule
				if h.Effect == "rewrote_args" {
					argNotes = append(argNotes, h.Note)
				}
			}
			call.Args = act.Args
		}
		if blocked != nil {
			// A withheld tool is withheld for setup too — but the model's
			// spec list is untouched here: nothing it did was capped.
			content, isErr, eff = blocked.Reason, true, EffectNone
		} else {
			// dispatch, not dispatchOrThrottle: no breaker reads, no breaker
			// writes — see the accounting note at the top of this file.
			content, isErr, eff = l.dispatch(ctx, call)
		}
		if l.envRules != nil && eff != EffectNone {
			obs := EnvObservation{Content: content, IsError: isErr}
			var h1, h2 []EnvRuleHit
			if eff == EffectFailed {
				obs, h1 = l.envRules.ModifyTransition(act, obs)
			}
			obs, h2 = l.envRules.FilterObservation(act, obs)
			*ruleHits = append(*ruleHits, h1...)
			*ruleHits = append(*ruleHits, h2...)
			for _, h := range append(h1, h2...) {
				firedRule = h.Rule
			}
			content, isErr = obs.Content, obs.IsError
		}
		if len(argNotes) > 0 {
			content += "\n\n[note: the seat's env rules adjusted this call's arguments: " + strings.Join(argNotes, "; ") + "]"
		}
		content, _ = contextbudget.Trim(content, l.toolResultCapChars())
		rec := EffectRecord{Step: 0, CallID: id, Tool: a.Tool, Status: eff, Risk: securityRisk(call.Args), ObsChars: len(content), Rule: firedRule, Setup: true}
		if eff != EffectCommitted {
			rec.Note = content
		}
		*effects = append(*effects, rec)
		calls = append(calls, call)
		results = append(results, Msg{Role: "tool", ToolCallID: id, Content: content, IsError: isErr})
		pinned[id] = true
		spent += estimateTokens([]Msg{{Role: "tool", Content: content}})
	}
	if len(calls) == 0 {
		return msgs
	}
	head := Msg{Role: "assistant", ToolCalls: calls,
		Content: fmt.Sprintf("setup: %d action(s) replayed from the contract before the first turn; their results follow. Use them — the documents they return are already in front of you.", len(calls))}
	msgs = append(msgs, head)
	msgs = append(msgs, results...)
	return msgs
}
