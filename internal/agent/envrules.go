package agent

// Environment rules — the harness's port of envharness's `Rules` interceptors
// (google-research/envharness; adopted 2026-09-07, item 7 of the operator order,
// ADR 0036). Three hooks wrap every tool call the loop executes:
//
//	filter_action      — may BLOCK the call (the model reads why) or REWRITE its
//	                     arguments (numeric caps) before it runs;
//	modify_transition  — rewrites the RESULT of an errored call into a short line
//	                     the model can act on;
//	filter_observation — strips and bounds what the model gets to read.
//
// Rules are DATA (core.AgentEnvRules: a closed, validated vocabulary), never
// generated code: envharness lets the rigger LLM write Python `Rules`
// subclasses and executes them in-process; here the rigger proposes edits to
// the same table an operator reviews. The interface exists so a test (or a
// future rule source) can stand in for the compiled table; production uses
// CompileEnvRules.
//
// These are NOT the structural risk rules (rules.go: deny/ask on effectful
// actions, tighten-only, security). An env rule never grants or denies an
// EFFECT — it shapes a weak seat's behaviour inside the loop. The loop's own
// circuit breakers (exact-repeat refusal, max_same_tool) stay in force
// underneath; an env table can only add constraints.
//
// Per-run state (call counters) lives in EnvRuleState, created by Run — the
// compiled table itself is immutable and shared (--serve shares one *Loop
// across concurrent handlers; a counter on the table would leak across runs).

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/contextbudget"
	"github.com/dmmdea/offload-harness/internal/core"
)

// EnvAction is one requested tool call as the hooks see it.
type EnvAction struct {
	Step   int
	CallID string
	Tool   string
	Args   string // raw JSON arguments; a rewrite replaces this
}

// EnvBlocked is a filter_action veto: the call never executes and Reason is
// what the model reads as the (is_error) result.
type EnvBlocked struct {
	Rule   string
	Reason string
}

// EnvObservation is one tool result before it enters the transcript.
type EnvObservation struct {
	Content string
	IsError bool
}

// EnvRuleHit records one rule firing — the telemetry the rigger reads.
type EnvRuleHit struct {
	Step   int    `json:"step"`
	Tool   string `json:"tool"`
	Rule   string `json:"rule"`   // vocabulary key: max_calls_per_tool, arg_limits, rewrite_error, observation_strip, max_observation_tokens
	Effect string `json:"effect"` // blocked | rewrote_args | rewrote_error | stripped | truncated
	Note   string `json:"note,omitempty"`
}

// EnvRules is the interceptor interface. DeniedTools is the STATIC half of
// filter_action: it decides which tool specs are withheld from the model for
// the whole run (structural, like disabledTools), given the tools the loop
// actually registered. Every method must be safe for concurrent use; per-run
// mutable state travels in *EnvRuleState.
type EnvRules interface {
	DeniedTools(available []string) []string
	FilterAction(st *EnvRuleState, a EnvAction) (EnvAction, *EnvBlocked, []EnvRuleHit)
	ModifyTransition(a EnvAction, o EnvObservation) (EnvObservation, []EnvRuleHit)
	FilterObservation(a EnvAction, o EnvObservation) (EnvObservation, []EnvRuleHit)
}

// EnvRuleState is the per-run mutable state the hooks may keep: how many
// times each tool has EXECUTED (blocked calls do not count — a blocked call
// changed nothing, and counting it would make a fixated model exhaust a cap
// it never spent).
type EnvRuleState struct {
	executed map[string]int
}

// NewEnvRuleState returns fresh per-run state.
func NewEnvRuleState() *EnvRuleState { return &EnvRuleState{executed: map[string]int{}} }

// Executed records that tool ran once (Run calls it after a non-blocked dispatch).
func (s *EnvRuleState) Executed(tool string) {
	if s != nil {
		s.executed[tool]++
	}
}

// CompiledEnvRules is core.AgentEnvRules compiled: regexps built once,
// lookups indexed. Immutable after CompileEnvRules.
type CompiledEnvRules struct {
	src       core.AgentEnvRules
	deny      map[string]bool
	allow     map[string]bool
	maxCalls  map[string]int
	argLimits map[string]map[string]float64
	maxObs    int // chars; 0 = off
	strip     []*regexp.Regexp
	rewrites  []compiledRewrite
}

type compiledRewrite struct {
	re   *regexp.Regexp
	text string
}

// envTokenChars is the loop's own estimate (estimateTokens: chars/4) — the
// observation cap is expressed in tokens for the operator and applied in
// chars, on the same yardstick the compaction budget uses.
const envTokenChars = 4

// CompileEnvRules validates and compiles a table. A nil or zero table
// compiles to nil (no rules) so callers can pass config through unchanged.
func CompileEnvRules(r *core.AgentEnvRules) (*CompiledEnvRules, error) {
	if r.IsZero() {
		return nil, nil
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	c := &CompiledEnvRules{
		src:       *r,
		deny:      map[string]bool{},
		allow:     map[string]bool{},
		maxCalls:  map[string]int{},
		argLimits: map[string]map[string]float64{},
	}
	for _, t := range r.DenyTools {
		c.deny[strings.TrimSpace(t)] = true
	}
	for _, t := range r.AllowTools {
		c.allow[strings.TrimSpace(t)] = true
	}
	for t, n := range r.MaxCallsPerTool {
		c.maxCalls[strings.TrimSpace(t)] = n
	}
	for t, args := range r.ArgLimits {
		m := make(map[string]float64, len(args))
		for a, cap := range args {
			m[strings.TrimSpace(a)] = cap
		}
		c.argLimits[strings.TrimSpace(t)] = m
	}
	if r.MaxObservationTokens > 0 {
		c.maxObs = r.MaxObservationTokens * envTokenChars
	}
	for _, p := range r.ObservationStrip {
		c.strip = append(c.strip, regexp.MustCompile(p)) // Validate compiled it already
	}
	for _, rw := range r.RewriteError {
		c.rewrites = append(c.rewrites, compiledRewrite{re: regexp.MustCompile(rw.Match), text: rw.Text})
	}
	return c, nil
}

// Source returns the table this was compiled from (for notes and status).
func (c *CompiledEnvRules) Source() core.AgentEnvRules { return c.src }

// DeniedTools applies deny_tools and allow_tools to the registered set:
// denied wins, allow (when set) keeps only the listed names. Names the loop
// never registered are ignored — an env table can never add a tool.
func (c *CompiledEnvRules) DeniedTools(available []string) []string {
	var out []string
	for _, n := range available {
		if c.deny[n] || (len(c.allow) > 0 && !c.allow[n]) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// FilterAction blocks a call over its per-tool executed cap, else clamps its
// numeric arguments to arg_limits. Both are recorded as hits; a clamp keeps
// the call running with the rewritten args.
func (c *CompiledEnvRules) FilterAction(st *EnvRuleState, a EnvAction) (EnvAction, *EnvBlocked, []EnvRuleHit) {
	var hits []EnvRuleHit
	if capN, ok := c.maxCalls[a.Tool]; ok && st != nil && st.executed[a.Tool] >= capN {
		reason := fmt.Sprintf("NOT executed: %s has already run %d time(s) in this task, which is its limit here. Continue with what you already have, or use a different tool.", a.Tool, st.executed[a.Tool])
		hits = append(hits, EnvRuleHit{Step: a.Step, Tool: a.Tool, Rule: "max_calls_per_tool", Effect: "blocked", Note: fmt.Sprintf("cap %d", capN)})
		return a, &EnvBlocked{Rule: "max_calls_per_tool", Reason: reason}, hits
	}
	if limits, ok := c.argLimits[a.Tool]; ok && len(limits) > 0 {
		if rewritten, changed := clampArgs(a.Args, limits); len(changed) > 0 {
			a.Args = rewritten
			hits = append(hits, EnvRuleHit{Step: a.Step, Tool: a.Tool, Rule: "arg_limits", Effect: "rewrote_args", Note: strings.Join(changed, ", ")})
		}
	}
	return a, nil, hits
}

// clampArgs rewrites numeric arguments above their caps. Only a JSON object
// with the named keys holding numbers is touched; anything else (non-object
// args, strings, absent keys) is returned unchanged so a rule can never turn
// a valid call into an invalid one. Keys are re-encoded in sorted order — the
// tool reads a map, so order is not part of the contract.
func clampArgs(raw string, limits map[string]float64) (string, []string) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil || obj == nil {
		return raw, nil
	}
	var changed []string
	for key, capV := range limits {
		rv, ok := obj[key]
		if !ok {
			continue
		}
		var n float64
		if err := json.Unmarshal(rv, &n); err != nil {
			continue // not a number: leave it
		}
		if n > capV {
			b, _ := json.Marshal(capV)
			obj[key] = b
			changed = append(changed, fmt.Sprintf("%s %v→%v", key, trimFloat(n), trimFloat(capV)))
		}
	}
	if len(changed) == 0 {
		return raw, nil
	}
	sort.Strings(changed)
	out, err := json.Marshal(obj)
	if err != nil {
		return raw, nil
	}
	return string(out), changed
}

func trimFloat(f float64) string {
	s := fmt.Sprintf("%g", f)
	return s
}

// ModifyTransition rewrites an ERRORED result whose text matches a
// rewrite_error pattern into the operator's short line; first match wins.
// Non-error observations pass untouched.
func (c *CompiledEnvRules) ModifyTransition(a EnvAction, o EnvObservation) (EnvObservation, []EnvRuleHit) {
	if !o.IsError || len(c.rewrites) == 0 {
		return o, nil
	}
	for i, rw := range c.rewrites {
		if rw.re.MatchString(o.Content) {
			o.Content = rw.text
			return o, []EnvRuleHit{{Step: a.Step, Tool: a.Tool, Rule: "rewrite_error", Effect: "rewrote_error", Note: fmt.Sprintf("rewrite_error[%d]", i)}}
		}
	}
	return o, nil
}

// FilterObservation strips every observation_strip pattern, then bounds the
// result to max_observation_tokens (head+tail with the loop's elision marker).
// Strip runs first so a banner does not eat the budget the cap protects.
func (c *CompiledEnvRules) FilterObservation(a EnvAction, o EnvObservation) (EnvObservation, []EnvRuleHit) {
	var hits []EnvRuleHit
	if len(c.strip) > 0 {
		before := len(o.Content)
		for _, re := range c.strip {
			o.Content = re.ReplaceAllString(o.Content, "")
		}
		if removed := before - len(o.Content); removed > 0 {
			hits = append(hits, EnvRuleHit{Step: a.Step, Tool: a.Tool, Rule: "observation_strip", Effect: "stripped", Note: fmt.Sprintf("%d chars", removed)})
		}
	}
	if c.maxObs > 0 {
		if trimmed, did := contextbudget.Trim(o.Content, c.maxObs); did {
			hits = append(hits, EnvRuleHit{Step: a.Step, Tool: a.Tool, Rule: "max_observation_tokens", Effect: "truncated", Note: fmt.Sprintf("%d→%d chars", len(o.Content), len(trimmed))})
			o.Content = trimmed
		}
	}
	return o, hits
}

// WithEnvRules installs the environment-rule interceptors. Denied tools are
// removed from the offered specs AND the executable set right here (the
// narrow-only invariant, same as WithProfile), so a denied name is never
// advertised and a call to it lands as "unknown tool". nil clears the hooks.
// Call it AFTER WithProfile: a profile narrows first, env rules narrow again.
func (l *Loop) WithEnvRules(r EnvRules) *Loop {
	l.envRules = r
	if r == nil {
		return l
	}
	names := make([]string, 0, len(l.specs))
	for _, s := range l.specs {
		names = append(names, s.Name)
	}
	denied := r.DeniedTools(names)
	if len(denied) == 0 {
		return l
	}
	drop := make(map[string]bool, len(denied))
	for _, n := range denied {
		drop[n] = true
	}
	newSpecs := make([]ToolSpec, 0, len(l.specs))
	newTools := make(map[string]Tool, len(l.specs))
	for _, s := range l.specs {
		if drop[s.Name] {
			continue
		}
		newSpecs = append(newSpecs, s)
		newTools[s.Name] = l.tools[s.Name]
	}
	l.specs = newSpecs
	l.tools = newTools
	// Exemplars that show a now-denied tool would teach a call the model
	// cannot make (see WithProfile) — narrow them the same way.
	l.exemplars = exemplarsFor(l.exemplars, l.tools)
	return l
}

// EnvRulesDenied reports which registered tools the installed env rules
// withheld — for builder notes and status.
func (l *Loop) EnvRulesDenied(before []string) []string {
	if l.envRules == nil {
		return nil
	}
	return l.envRules.DeniedTools(before)
}
