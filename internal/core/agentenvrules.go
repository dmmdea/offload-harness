package core

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// ErrAgentEnvRules wraps every Validate failure so a door can classify it
// (a fleet node defers by CONFIG class: the table is the box's, not the
// contract's) without matching on message text.
var ErrAgentEnvRules = errors.New("agent_env_rules")

// AgentEnvRulesMinObservationTokens is the floor for MaxObservationTokens:
// below ~64 tokens (256 chars) the head+tail cut has no room for its elision
// marker and degrades to a silent head-only hard cut — a truncated result the
// model cannot tell was truncated is worse than no cap.
const AgentEnvRulesMinObservationTokens = 64

// AgentEnvRules is the CLOSED vocabulary of environment rules a seat's agent
// loop runs under (config key `agent_env_rules`, per box / per fleet node;
// `local-agent --env-rules <file>` overrides it for a scratch validation).
//
// The shape is envharness's (google-research/envharness, adopted 2026-09-07,
// item 7 of the operator order): three interceptors around every tool call —
// filter_action (may block or rewrite the call) → modify_transition (rewrites
// the result of an errored call) → filter_observation (strips and bounds what
// the model gets to read). What is deliberately NOT adopted is envharness's
// LLM-written rule code (`rules_code` executed in-process): a rule here is
// DATA — typed, validated at load, diffable, and testable — never generated
// code. The rigger (P3 of the same order) proposes changes to this table; an
// operator applies them.
//
// This is a different table from the structural RISK rules (`--rules`,
// internal/agent/rules.go): those decide what the broker may let an effectful
// action DO to the world (deny/ask, tighten-only, security). These shape how a
// WEAK SEAT behaves inside the loop (what it may call, how often, how much of
// a result it reads). The two never overlap: a risk rule never rewrites an
// observation; an env rule never grants an effect.
//
// Every field is additive and optional: a nil/zero table is byte-identical to
// the pre-key loop.
type AgentEnvRules struct {
	// DenyTools removes these tools from what the model is offered — a
	// structural constraint (the spec is not sent), not a text refusal. Names
	// not among the enabled tools are ignored (narrow-only, like profiles).
	DenyTools []string `json:"deny_tools,omitempty"`
	// AllowTools, when non-empty, keeps ONLY these tools (everything else is
	// denied). It can never add a tool the capability flags / profile did not
	// enable. DenyTools wins over AllowTools on a conflict.
	AllowTools []string `json:"allow_tools,omitempty"`
	// MaxCallsPerTool caps how many times ONE named tool may EXECUTE in a run;
	// the N+1th call is blocked with a reason the model reads. Finer than the
	// loop's global same-name cap (max_same_tool), which stays in force.
	MaxCallsPerTool map[string]int `json:"max_calls_per_tool,omitempty"`
	// ArgLimits clamps NUMERIC arguments per tool: tool → argument → cap. An
	// argument above the cap is rewritten to the cap (the call still runs);
	// non-numeric or absent arguments are untouched. Typical: read_file.limit.
	ArgLimits map[string]map[string]float64 `json:"arg_limits,omitempty"`
	// MaxObservationTokens bounds ONE tool result before it enters the
	// transcript (chars ≈ tokens×4, the loop's own estimate), head+tail with
	// an elision marker — the per-seat version of the loop's window-derived
	// cap, for seats whose window is big but whose attention is not. Minimum
	// AgentEnvRulesMinObservationTokens; the loop-boundary cap still applies
	// after it, so the effective bound is the smaller of the two.
	MaxObservationTokens int `json:"max_observation_tokens,omitempty"`
	// ObservationStrip is a list of Go regexps removed from every tool result
	// (banners, cookie notices, progress bars, base64 blobs). Each is
	// validated at load; a pattern that fails to compile fails the config.
	ObservationStrip []string `json:"observation_strip,omitempty"`
	// RewriteError maps a raw tool ERROR (is_error results only) whose text
	// matches `match` to a short, model-readable `text`. First match wins.
	RewriteError []AgentErrorRewrite `json:"rewrite_error,omitempty"`
}

// AgentErrorRewrite is one modify_transition entry: an error matching Match
// (Go regexp, applied to the raw error text) is replaced by Text.
type AgentErrorRewrite struct {
	Match string `json:"match"`
	Text  string `json:"text"`
}

// IsZero reports whether the table would change nothing.
func (r *AgentEnvRules) IsZero() bool {
	if r == nil {
		return true
	}
	return len(r.DenyTools) == 0 && len(r.AllowTools) == 0 && len(r.MaxCallsPerTool) == 0 &&
		len(r.ArgLimits) == 0 && r.MaxObservationTokens == 0 && len(r.ObservationStrip) == 0 &&
		len(r.RewriteError) == 0
}

// Validate rejects a table that could never be applied as written: negative
// caps, empty tool names, regexps that do not compile, a rewrite with no text.
// It runs wherever the table is loaded (config, the CLI override, the rigger's
// proposals) so a bad rule fails by name, never silently no-ops.
func (r *AgentEnvRules) Validate() error {
	if r == nil {
		return nil
	}
	var errs []error
	// Names are taken LITERALLY: a name with surrounding whitespace is a typo
	// that would otherwise silently collapse onto (or miss) the trimmed one.
	name := func(where, n string) {
		switch {
		case strings.TrimSpace(n) == "":
			errs = append(errs, fmt.Errorf("%s: empty name", where))
		case strings.TrimSpace(n) != n:
			errs = append(errs, fmt.Errorf("%s: %q has surrounding whitespace", where, n))
		}
	}
	for i, t := range r.DenyTools {
		name(fmt.Sprintf("deny_tools[%d]", i), t)
	}
	for i, t := range r.AllowTools {
		name(fmt.Sprintf("allow_tools[%d]", i), t)
	}
	for t, n := range r.MaxCallsPerTool {
		name("max_calls_per_tool", t)
		if n < 1 {
			errs = append(errs, fmt.Errorf("max_calls_per_tool[%s]: %d, want ≥ 1 (use deny_tools to remove a tool)", t, n))
		}
	}
	for t, args := range r.ArgLimits {
		name("arg_limits", t)
		if len(args) == 0 {
			errs = append(errs, fmt.Errorf("arg_limits[%s]: no arguments listed", t))
		}
		for a, cap := range args {
			name(fmt.Sprintf("arg_limits[%s]", t), a)
			switch {
			case cap < 0:
				errs = append(errs, fmt.Errorf("arg_limits[%s][%s]: negative cap %v", t, a, cap))
			case cap != math.Trunc(cap) || cap > 1<<53:
				// A tool's numeric argument is an int on the Go side (read_file
				// limit/offset); a fractional or exponent-formatted cap would
				// turn a valid call into one the tool cannot decode.
				errs = append(errs, fmt.Errorf("arg_limits[%s][%s]: cap %v must be a whole number ≤ 2^53", t, a, cap))
			}
		}
	}
	if r.MaxObservationTokens < 0 {
		errs = append(errs, fmt.Errorf("max_observation_tokens: %d, want ≥ 0", r.MaxObservationTokens))
	} else if r.MaxObservationTokens > 0 && r.MaxObservationTokens < AgentEnvRulesMinObservationTokens {
		errs = append(errs, fmt.Errorf("max_observation_tokens: %d, want ≥ %d (below that the cut has no room for its elision marker)", r.MaxObservationTokens, AgentEnvRulesMinObservationTokens))
	}
	for i, p := range r.ObservationStrip {
		if p == "" {
			errs = append(errs, fmt.Errorf("observation_strip[%d]: empty pattern", i))
			continue
		}
		if _, err := regexp.Compile(p); err != nil {
			errs = append(errs, fmt.Errorf("observation_strip[%d]: %v", i, err))
		}
	}
	for i, rw := range r.RewriteError {
		if rw.Match == "" {
			errs = append(errs, fmt.Errorf("rewrite_error[%d]: empty match", i))
		} else if _, err := regexp.Compile(rw.Match); err != nil {
			errs = append(errs, fmt.Errorf("rewrite_error[%d]: %v", i, err))
		}
		if strings.TrimSpace(rw.Text) == "" {
			errs = append(errs, fmt.Errorf("rewrite_error[%d]: empty text (a rewrite must say something)", i))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrAgentEnvRules, errors.Join(errs...))
}

// Summary is the one-line operator description used in notes and status:
// which knobs are set, in a stable order, nothing about their values that
// could grow unbounded.
func (r *AgentEnvRules) Summary() string {
	if r.IsZero() {
		return "none"
	}
	var parts []string
	if n := len(r.DenyTools); n > 0 {
		parts = append(parts, fmt.Sprintf("deny_tools=%d", n))
	}
	if n := len(r.AllowTools); n > 0 {
		parts = append(parts, fmt.Sprintf("allow_tools=%d", n))
	}
	if n := len(r.MaxCallsPerTool); n > 0 {
		parts = append(parts, fmt.Sprintf("max_calls_per_tool=%d", n))
	}
	if n := len(r.ArgLimits); n > 0 {
		parts = append(parts, fmt.Sprintf("arg_limits=%d", n))
	}
	if r.MaxObservationTokens > 0 {
		parts = append(parts, fmt.Sprintf("max_observation_tokens=%d", r.MaxObservationTokens))
	}
	if n := len(r.ObservationStrip); n > 0 {
		parts = append(parts, fmt.Sprintf("observation_strip=%d", n))
	}
	if n := len(r.RewriteError); n > 0 {
		parts = append(parts, fmt.Sprintf("rewrite_error=%d", n))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

// AgentTraceStep is one tool call of a run as the CORPUS sees it (wire
// result `trace`): enough for the rigger to diagnose a failure axis — which
// tool, what became of it, how much the model was given to read, and which
// environment rule (if any) decided — without the transcript bytes.
type AgentTraceStep struct {
	Step     int    `json:"step"`
	Tool     string `json:"tool"`
	Status   string `json:"status"`              // committed | failed | unknown | none (agent.EffectStatus)
	ObsChars int    `json:"obs_chars,omitempty"` // size of the result the model saw, after every rule and cap
	Rule     string `json:"rule,omitempty"`      // env rule that fired on this call, e.g. "max_calls_per_tool"
	Setup    bool   `json:"setup,omitempty"`     // replayed from the contract's setup_actions before turn 1 (step 0)
	// Note (0.113.26, the rigger's evidence): for a call that did NOT commit —
	// failed, none, unknown — the first AgentTraceNoteMax bytes of what the
	// model was told (the tool's error, the breaker's refusal, the rule's
	// reason). Empty on committed calls: the corpus keeps facts about the
	// run, never result bytes. Without it a `rewrite_error` rule could not be
	// authored from the corpus (design council 2026-09-07).
	Note string `json:"note,omitempty"`
}

// AgentTraceNoteMax bounds AgentTraceStep.Note.
const AgentTraceNoteMax = 160
