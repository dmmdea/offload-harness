// Agent delegation wire contract v1 (multi-node sub-agent delegation, §S2).
// AgentContract is the ONLY thing a delegator sends a fleet node for an
// "agent" task, and AgentWireResult is the ONLY thing that crosses back — no
// transcript ever crosses the wire, so remote reasoning is quarantined from
// the caller's context by construction, not by policy.
//
// Reader posture (roast delta 4): TOLERANT on unknown fields — nodes deploy
// staggered, and a strict decoder would make every additive field a
// flag-day upgrade across the fleet. Version skew is caught instead by the
// explicit schema_version check, which fails loudly. Size/count caps stay
// strict: tolerance is for VOCABULARY, never for resource ceilings.

package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/gbnf"
)

const (
	// AgentWireSchemaVersion is the one contract shape this binary speaks.
	// Any other version is refused at decode — a mismatched peer must defer
	// loudly rather than half-understand a contract.
	AgentWireSchemaVersion = 1

	// AgentContextMaxBytes caps the TOTAL inline context (name+text bytes
	// across all docs) at 256 KiB. This is a TRANSPORT bound only (roast
	// delta 5) — whether the docs actually fit the remote seat's context
	// window is the placement gate's token arithmetic, not this cap.
	AgentContextMaxBytes = 256 << 10

	// AgentContextMaxDocs caps the doc count. Each doc becomes a file in a
	// job-scoped dir on the receiving node; a bounded count keeps that
	// materialization (and the sub-agent's read fan-out) bounded too.
	AgentContextMaxDocs = 16

	// AgentMaxStepsDefault/Cap: default 12, cap 12 for remote contracts
	// (roast delta 5 tightened the pre-roast cap of 24 — ctx honesty: at an
	// 8k seat there is no budget for 24 transcript-growing steps anyway).
	AgentMaxStepsDefault = 12
	AgentMaxStepsCap     = 12

	// AgentTimeoutSecDefault/Cap: wall-clock ceiling for the remote loop,
	// enforced as a context deadline node-side. 300s default, 900s hard cap.
	AgentTimeoutSecDefault = 300
	AgentTimeoutSecCap     = 900
)

// Defer classes: WHY a defer happened, in the four kinds that call for
// different operator action. The reason string is prose for a human; the class
// is what a delegator, a dashboard, or an exit code can branch on — without it
// "the model produced the wrong shape" and "llama-swap is down" arrive as the
// same quiet green defer, and a broken node reads as a working one.
const (
	// DeferClassAbstention: the model was asked, ran, and could not produce a
	// usable answer. The stack is healthy; the work is what failed.
	DeferClassAbstention = "abstention"
	// DeferClassBudget: a ceiling stopped it — step budget or wall clock. The
	// answer may exist behind a larger budget.
	DeferClassBudget = "budget"
	// DeferClassInfrastructure: something in the stack failed — endpoint
	// unreachable, agent build failed, transport died mid-run. Nothing about
	// the task was learned, and NO amount of retrying the same contract helps
	// until an operator fixes the box.
	DeferClassInfrastructure = "infrastructure"
	// DeferClassConfig: the node/contract combination can never work as
	// configured — no seat resolvable, the seat is not served, an unknown
	// profile. Also an operator fix, but of config rather than of a service.
	DeferClassConfig = "config"
	// DeferClassContract: the CALLER'S CONTRACT is what cannot be placed or
	// run, on any node, however healthy the fleet is — no output_schema for a
	// remote placement, a contract already past the origin hop, a token
	// estimate no advertised ceiling can hold. Split out of `config` because
	// `config` counts as a broken stack (delegate.BrokenStackDefer): a caller
	// mistake was exiting non-zero and telling the delegating model that a node
	// was down. The fix belongs to whoever wrote the contract, and it is one
	// they can make without touching a box.
	DeferClassContract = "contract"
	// DeferClassCapacity (0.113.18): every node that could run the contract was
	// full for as long as the delegator was willing to wait (agent_placement_
	// wait_sec), or the contract was SHEDDABLE (priority -1) and no node had an
	// idle slot to give it. The fleet is healthy and the contract is sound —
	// it was simply not this contract's turn. Re-running it later is the fix;
	// no box and no contract needs touching, so it is not a broken stack
	// (delegate.BrokenStackDefer) and not a budget the seat ran out of.
	DeferClassCapacity = "capacity"
)

// Scheduling bands (0.113.18) — the delegator stamps one on every dispatch
// (`priority` in the fleet envelope) and the node's job store orders its
// backlog by it. Shared here because both sides must agree on the vocabulary
// and neither package may import the other.
//
//	BandSheddable (-1)  measurement / gate traffic: admitted only into an idle
//	                    execution slot, claimed after band 0, shed when no node
//	                    is idle; ages into band 0 after 60 s in a backlog.
//	BandNormal (0)      production digests, reviews, research — the default and
//	                    what a pre-0.113.18 delegator sends.
//	BandUrgent (+1)     reserved for an interactive caller; claimed first.
const (
	BandSheddable = -1
	BandNormal    = 0
	BandUrgent    = 1
)

// ClampBand folds any integer into the three bands.
func ClampBand(b int) int {
	if b < BandSheddable {
		return BandSheddable
	}
	if b > BandUrgent {
		return BandUrgent
	}
	return b
}

// TenantHeader carries the delegator's tenant id on a dispatch (0.113.18).
const TenantHeader = "X-Offload-Tenant"

// AgentContract is the versioned, self-contained delegation request (§S2).
// Self-contained means: everything the remote loop may read is INLINE in
// Context — the remote node never reaches back into the delegator's
// filesystem or session.
type AgentContract struct {
	SchemaVersion int             `json:"schema_version"`          // AgentWireSchemaVersion
	Goal          string          `json:"goal"`                    // required, self-contained
	Context       []ContextDoc    `json:"context,omitempty"`       // inline docs, total ≤ AgentContextMaxBytes
	OutputSchema  json.RawMessage `json:"output_schema,omitempty"` // JSON Schema for structured output (gbnf subset)
	Acceptance    []string        `json:"acceptance,omitempty"`    // AcceptanceCheck DSL strings, delegator-evaluated
	Profile       string          `json:"profile,omitempty"`       // agent profile name; default "research"-class read-only
	// Thinking (0.115.8) is the planner think-block policy for this contract:
	// "" / "auto" (think; an empty final is re-issued once with thinking off),
	// "off" (every planner call in non-thinking mode — grounded extraction on
	// a seat whose think block starves the answer), "on" (never send the
	// non-thinking kwarg). Overrides the executing node's `agent_thinking`.
	Thinking   string `json:"thinking,omitempty"`
	MaxSteps   int    `json:"max_steps,omitempty"`   // default AgentMaxStepsDefault, clamped to AgentMaxStepsCap
	TimeoutSec int    `json:"timeout_sec,omitempty"` // default AgentTimeoutSecDefault, clamped to AgentTimeoutSecCap
	Depth      int    `json:"depth"`                 // 0 = origin; ≥1 ⇒ delegate tool NEVER registered
	// SetupActions (agentsetup.go, 0.113.24): tool calls the loop replays before
	// the model's first turn; ≤ AgentSetupActionsMax, charged to the wall and
	// never to max_steps. Optional; a node one release behind ignores it (the
	// decoder keeps unknown fields) and reports no setup_ran.
	SetupActions []AgentSetupAction `json:"setup_actions,omitempty"`
}

// ContextDoc is one inline context document. Name is a future FILENAME on the
// receiving node (materialized under the job's context dir), which is why
// Validate holds it to flat-filename shape — no separators, no traversal.
type ContextDoc struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

// AgentWireResult is the versioned result envelope — the only thing that
// crosses back from a remote agent run (§S2). Deferred is a SUCCESS shape at
// the job level (the node did its job: it reported it could not complete the
// contract), mirroring the cascade's defer semantics.
type AgentWireResult struct {
	SchemaVersion int             `json:"schema_version"` // AgentWireSchemaVersion
	NodeID        string          `json:"node_id"`
	Seat          string          `json:"seat"`                 // resolved planner model
	Output        string          `json:"output"`               // final assistant text
	Structured    json.RawMessage `json:"structured,omitempty"` // present iff OutputSchema given AND validated
	Steps         int             `json:"steps"`
	StopReason    string          `json:"stop_reason"`
	// StopNote (0.115.8): the one-line evidence behind a `reasoning_starved` /
	// `empty` stop — the starvation arithmetic of the last empty completion
	// (finish reason, reasoning vs completion tokens, which wire key). Empty
	// on every other stop.
	StopNote string `json:"stop_note,omitempty"`
	// OutputTruncated (0.115.8): the final answer ended on finish_reason
	// "length" — a correct PARTIAL the caller must not read as the whole.
	OutputTruncated bool   `json:"output_truncated,omitempty"`
	Deferred        bool   `json:"deferred"`
	Reason          string `json:"reason,omitempty"`
	// DeferClass is the machine-branchable WHY behind Deferred (one of the
	// DeferClass* constants). Additive and omitempty: a pre-0.65 node's result
	// decodes with an empty class, which readers must treat as "unknown", never
	// as abstention.
	DeferClass string `json:"defer_class,omitempty"`
	WallMs     int64  `json:"wall_ms"`
	TokensOut  int    `json:"tokens_out,omitempty"`
	// SeatTokensIn is the prompt-token count the seat processed over the whole
	// run (every turn, cache hits included) — work the cards did, never a saving:
	// the ledger keeps it apart from tokens_in, which the summary counts as tokens
	// saved. Additive, omitempty (0.115.5); carried on DEFERRED results too.
	SeatTokensIn int `json:"seat_tokens_in,omitempty"`
	// ContentionWaitSec is the wall this contract spent waiting on a peer-held
	// seat (seatwait: llama-swap 429 / 503 not-ready / 500 src=llama-swap).
	// Counted, never silent: the number that says whether the fix traded
	// defers for latency. Omitted when zero; a pre-0.111 node's result reads as
	// "not measured".
	ContentionWaitSec float64 `json:"contention_wait_sec,omitempty"`
	// AdmissionWaitSec is the pre-flight spent BEFORE the wall started
	// (RunAgentTask's admission gate): waiting for llama-swap to finish another
	// model's swap and, since 0.115.11, loading the seat itself when it was not
	// resident (the cold-load warm-up). Zero = omitted = nothing was swapping,
	// the seat was already loaded, or the gate is disabled.
	AdmissionWaitSec float64 `json:"admission_wait_sec,omitempty"`
	// AdmissionNote names what the admission gate could NOT settle or what it
	// did — a probe failure (fail-open), a spent budget, the seat's cold load
	// ("cold load Ns outside the wall"), or a warm-up llama-swap never
	// confirmed — so a wire reader can tell "nothing was swapping" from "the
	// gate could not tell" from "the seat was loaded here".
	AdmissionNote string `json:"admission_note,omitempty"`

	// --- A1 config pinning (0.81.0, Tier 2 of the Phase 2 re-aim). Stamped by
	// runAgentTask only when the seat DEMONSTRABLY SERVED this run (the loop
	// completed its chat traffic) — a config/infrastructure defer that fired
	// before any model call carries no pins, because probing /props on a
	// non-resident seat would cold-start a model as a telemetry side effect.
	// All omitempty: a pre-0.81 node's result decodes with every pin empty,
	// which readers MUST treat as "unknown config — refuse to pair", never as
	// a pinned value (same additive rule as DeferClass above).
	//
	// Two halves, deliberately: SeatConfig* pins the SERVER (weights, quant,
	// build, window, template, server sampler defaults) via agent.ProbeSeatPin;
	// HarnessVersion + HarnessBuildSHA256 pin the REQUEST-side code (per-call
	// temperature, the re-pack's enable_thinking:false, profile toolsets) —
	// the side the 2026-08-17 corpus-invalidating defect actually lived on.
	HarnessVersion     string `json:"harness_version,omitempty"`
	HarnessBuildSHA256 string `json:"harness_build_sha256,omitempty"`
	SeatConfigSHA256   string `json:"seat_config_sha256,omitempty"`
	SeatConfigBasis    string `json:"seat_config_basis,omitempty"`

	// --- Node-side prefill accounting (T2-B), previously ledger-only. The
	// node's ledger row already carried these; the DELEGATOR could not see
	// them, so the standing delegation-log corpus recorded a remote run's
	// prefill economics nowhere ("computed then discarded", the Tier 1 defect
	// class). Zero values are omitted — a pre-0.81 node's result reads as
	// "not measured", never as "zero prefill".
	PrefillSteps  int     `json:"prefill_steps,omitempty"`
	PrefillTokens int64   `json:"prefill_tokens,omitempty"`
	CacheTokens   int64   `json:"cache_tokens,omitempty"`
	PrefillMS     float64 `json:"prefill_ms,omitempty"`

	// --- Step trace (ADR 0036, 0.113.22). One entry per tool call the loop
	// handled: tool, fate, how much the model read, which environment rule
	// decided. Bounded by max_steps × calls-per-step, no transcript bytes.
	// This is what the corpus lacked for a per-step diagnosis (2026-09-07: 51
	// of 58 failed 4B rows stop at exactly 2 steps with empty schema fields,
	// and the corpus could not say what those two steps did). Omitempty: a
	// pre-0.113.22 node's result reads as "no trace", never as "no calls".
	Trace []AgentTraceStep `json:"trace,omitempty"`
	// RulesFired counts environment-rule hits on this run (0 = none / no table).
	RulesFired int `json:"rules_fired,omitempty"`
	// SetupRan counts the contract's setup actions (agentsetup.go) that were
	// EXECUTED before the model's first turn (committed or failed — a refused,
	// unknown-tool or over-budget action is not counted; the trace's setup
	// entries carry the per-action status). 0 on a node that predates the
	// field, so a mixed-version fleet shows where the replay did not happen.
	SetupRan int `json:"setup_ran,omitempty"`
	// Calls (0.115.8, D-47): one entry per planner completion — finish reason,
	// completion and reasoning tokens, visible and hidden chars, tool-call
	// count, whether thinking was off. Set before the defer branches, on every
	// result shape, so a starved run's arithmetic is in the corpus rather than
	// reconstructed from token totals. Omitempty: a pre-0.115.8 node emits none.
	Calls []AgentCallRecord `json:"calls,omitempty"`
}

// AgentCallRecord is one planner completion as the corpus keeps it (the wire
// projection of agent.CallRecord). No transcript bytes — counts only.
type AgentCallRecord struct {
	Step             int    `json:"step"`
	MaxTokens        int    `json:"max_tokens"`
	FinishReason     string `json:"finish_reason,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
	ReasoningTokens  int    `json:"reasoning_tokens,omitempty"`
	ContentChars     int    `json:"content_chars,omitempty"`
	ReasoningChars   int    `json:"reasoning_chars,omitempty"`
	ToolCalls        int    `json:"tool_calls,omitempty"`
	ThinkingOff      bool   `json:"thinking_off,omitempty"`
	ReasoningKey     string `json:"reasoning_key,omitempty"`
}

// ValidateThinking accepts the closed vocabulary of the planner think-block
// policy ("" / auto / on / off, case-insensitive). Shared by the contract
// validator and the config loader so both doors refuse the same strings.
func ValidateThinking(s string) error {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto", "on", "off":
		return nil
	}
	return fmt.Errorf("thinking %q: want auto, on or off", s)
}

// DecodeAgentContract reads one contract from r, tolerating unknown fields
// (roast delta 4), refusing any schema_version but AgentWireSchemaVersion,
// clamping MaxSteps/TimeoutSec into their remote ceilings (roast delta 5),
// then running the full Validate. Errors here are caller mistakes — the fleet
// server maps them to 400s at ACK time, so a malformed contract dies with a
// clear reason before any model loads.
func DecodeAgentContract(r io.Reader) (AgentContract, error) {
	var c AgentContract
	if err := json.NewDecoder(r).Decode(&c); err != nil {
		return AgentContract{}, fmt.Errorf("agent contract: %w", err)
	}
	if c.SchemaVersion != AgentWireSchemaVersion {
		return AgentContract{}, fmt.Errorf("agent contract: unsupported schema_version %d (this node speaks %d)", c.SchemaVersion, AgentWireSchemaVersion)
	}
	// Ceilings CLAMP rather than reject: an over-ask is an intent mismatch,
	// not a malformed contract — the delegator asked for more loop than a
	// remote seat is allowed to spend, and the answer is "you get the cap".
	if c.MaxSteps <= 0 {
		c.MaxSteps = AgentMaxStepsDefault
	} else if c.MaxSteps > AgentMaxStepsCap {
		c.MaxSteps = AgentMaxStepsCap
	}
	if c.TimeoutSec <= 0 {
		c.TimeoutSec = AgentTimeoutSecDefault
	} else if c.TimeoutSec > AgentTimeoutSecCap {
		c.TimeoutSec = AgentTimeoutSecCap
	}
	if err := c.Validate(); err != nil {
		return AgentContract{}, err
	}
	return c, nil
}

// Validate checks everything about a contract that does not depend on where
// it runs: required goal, depth sign, context caps and doc-name hygiene,
// OutputSchema gbnf-compilability, and Acceptance DSL well-formedness. Both
// sides run it — the delegator before dispatch (fail before the network) and
// the node inside DecodeAgentContract (never trust the wire).
func (c AgentContract) Validate() error {
	if strings.TrimSpace(c.Goal) == "" {
		return errors.New("agent contract: goal is required")
	}
	if c.Depth < 0 {
		return fmt.Errorf("agent contract: depth %d is negative", c.Depth)
	}
	if err := ValidateThinking(c.Thinking); err != nil {
		return fmt.Errorf("agent contract: %w", err)
	}
	if err := ValidateAgentSetupActions(c.SetupActions); err != nil {
		return err
	}
	if len(c.Context) > AgentContextMaxDocs {
		return fmt.Errorf("agent contract: %d context docs exceeds the max of %d", len(c.Context), AgentContextMaxDocs)
	}
	total := 0
	// Keyed on the NORMALIZED name, not the raw one: two names that differ only
	// by case or by trailing space/dot are distinct Go strings that name the
	// SAME file on Windows (and, for case, on macOS). Comparing raw strings let
	// {"notes.md"} and {"notes.md "} both through, and the second write then
	// shadowed the first — exactly the silent overwrite the check below exists
	// to prevent. Those shapes are ALSO rejected outright by validDocName; the
	// normalized key is the second layer, so no future relaxation there can
	// re-open shadowing here.
	seen := make(map[string]bool, len(c.Context))
	for i, d := range c.Context {
		if err := validDocName(d.Name); err != nil {
			return fmt.Errorf("agent contract: context doc %d: %w", i, err)
		}
		key := normalizeDocName(d.Name)
		if seen[key] {
			// Duplicates would silently overwrite each other at
			// materialization — the sub-agent would read ONE of them and
			// nobody would know which.
			return fmt.Errorf("agent contract: context doc name %q collides with an earlier doc (names that differ only by case or by trailing spaces/dots are the same file)", d.Name)
		}
		seen[key] = true
		total += len(d.Name) + len(d.Text)
	}
	if total > AgentContextMaxBytes {
		return fmt.Errorf("agent contract: context total %d bytes exceeds the %d-byte cap", total, AgentContextMaxBytes)
	}
	if len(c.OutputSchema) > 0 {
		if err := validateOutputSchema(c.OutputSchema); err != nil {
			return fmt.Errorf("agent contract: output_schema: %w", err)
		}
	}
	for i, a := range c.Acceptance {
		if _, err := ParseAcceptanceCheck(a); err != nil {
			return fmt.Errorf("agent contract: acceptance[%d]: %w", i, err)
		}
	}
	return nil
}

// validateOutputSchema confirms the schema compiles under the gbnf package's
// supported subset — the SAME path the extract task takes (internal/tasks
// buildExtract: properties map → gbnf.FromJSONSchema → require ≥1 field).
// This closes the wrong-valid-schema hole (roast delta 3): a schema that is
// valid JSON Schema but yields zero grammar fields would pass a naive check
// here and then defer on EVERY remote run, after the network round-trip and
// the loop budget were already spent.
func validateOutputSchema(raw json.RawMessage) error {
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return fmt.Errorf("not a JSON object: %w", err)
	}
	if len(gbnf.FromJSONSchema(schema)) == 0 {
		return errors.New("no gbnf-compilable properties (needs a \"properties\" map of string/number/integer/boolean/string-array/enum fields)")
	}
	return nil
}

// windowsDeviceNames is the set of legacy DOS device names Windows still
// resolves in EVERY directory. Opening one of these — by any case, with or
// without extensions — opens the DEVICE, not a file: a write to "NUL"
// succeeds and reads back empty, so a context doc named that way vanishes
// with no error anywhere. COM0/LPT0 are deliberately absent (not reserved),
// as are COM10+/LPT10+, so "com10.txt" stays an ordinary file.
var windowsDeviceNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// validDocName holds a ContextDoc name to flat-filename shape. Ban list, not
// allow list, kept minimal: separators and ".." are traversal, ":" is a
// Windows drive/ADS hazard ("C:evil" is drive-relative and escapes Join),
// and NUL truncates paths in C-heritage syscalls.
//
// The Windows-specific rules below are enforced on EVERY platform on purpose.
// A contract is a WIRE object: it is validated on the delegator and again on
// the receiving node, and those two can be different operating systems. A
// Linux-only check would accept a name the Windows node then swallows, which
// is the silent-data-loss shape this whole function exists to prevent — so the
// strictest platform's rules are the contract's rules.
func validDocName(name string) error {
	if name == "" {
		return errors.New("name is required")
	}
	if strings.ContainsAny(name, "/\\:\x00") {
		return fmt.Errorf("name %q must be a flat filename (no path separators, colons, or NUL)", name)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("name %q is a directory reference", name)
	}
	// Trailing spaces and dots are STRIPPED by the Windows filesystem layer, so
	// "notes.md " and "notes.md" name one file while staying two distinct Go
	// strings — the proven bypass of the duplicate guard above.
	if trimmed := strings.TrimRight(name, " ."); trimmed != name {
		if trimmed == "" {
			return fmt.Errorf("name %q is only spaces and dots", name)
		}
		return fmt.Errorf("name %q has a trailing space or dot (Windows strips those, so it would silently become %q)", name, trimmed)
	}
	// The device rule matches the stem — everything before the FIRST dot —
	// because "nul.md.txt" opens the null device just as "nul" does.
	stem := name
	if i := strings.Index(stem, "."); i >= 0 {
		stem = stem[:i]
	}
	if windowsDeviceNames[strings.ToLower(stem)] {
		return fmt.Errorf("name %q is a reserved Windows device name (writes to it succeed and read back EMPTY — the doc would vanish silently)", name)
	}
	return nil
}

// normalizeDocName is the duplicate-detection key: two names that normalize
// alike would be ONE file on at least one platform the fleet runs on, so the
// contract must refuse the pair rather than let one shadow the other.
// Trailing spaces/dots go because Windows strips them; case folds because
// Windows and macOS filesystems are case-insensitive.
func normalizeDocName(name string) string {
	return strings.ToLower(strings.TrimRight(name, " ."))
}

// ---- Acceptance check DSL (roast delta 3) ----
//
// Acceptance is machine-checkable, delegator-side: free prose no longer
// counts toward the verifiability gate because nothing can EVALUATE prose
// before merging weak-node output. v1 vocabulary:
//
//	contains:<s>          output contains substring s
//	not_contains:<s>      output does not contain substring s
//	regex:<re>            output matches Go regexp re
//	min_items:<field>:<n> structured.<field> is an array with ≥ n items
//	nonempty:<field>      structured.<field> is present and non-empty
//
// The text verbs read the final assistant Output; when Output is empty and a
// Structured result exists they fall back to its raw bytes, so a schema-only
// result is still text-checkable. The field verbs REQUIRE Structured and
// fail closed without it — quality-first: an unmet precondition is a failed
// check, never a skipped one.

// AcceptanceKind is the closed verb set of the v1 acceptance DSL.
type AcceptanceKind string

const (
	AccContains    AcceptanceKind = "contains"
	AccNotContains AcceptanceKind = "not_contains"
	AccRegex       AcceptanceKind = "regex"
	AccMinItems    AcceptanceKind = "min_items"
	AccNonempty    AcceptanceKind = "nonempty"
)

// AcceptanceCheck is one parsed acceptance assertion. Construct via
// ParseAcceptanceCheck — a zero AcceptanceCheck fails every Eval.
type AcceptanceCheck struct {
	Kind AcceptanceKind
	Arg  string // substring, or field name for the field verbs
	N    int    // min_items threshold
	raw  string // the original DSL string, echoed in failure reasons
	re   *regexp.Regexp
}

// ParseAcceptanceCheck parses one DSL string. Unfalsifiable shapes are parse
// ERRORS, not no-ops: "contains:" matches everything and "min_items:f:0" can
// never fail, so accepting them would silently weaken the verifiability gate
// that Acceptance exists to enforce (a contract with only such checks would
// count as "verifiable" while verifying nothing).
func ParseAcceptanceCheck(s string) (AcceptanceCheck, error) {
	kind, rest, found := strings.Cut(s, ":")
	if !found {
		return AcceptanceCheck{}, fmt.Errorf("acceptance check %q: want <verb>:<args> with verb one of contains, not_contains, regex, min_items, nonempty", s)
	}
	c := AcceptanceCheck{Kind: AcceptanceKind(kind), Arg: rest, raw: s}
	switch c.Kind {
	case AccContains:
		if rest == "" {
			return AcceptanceCheck{}, fmt.Errorf("acceptance check %q: empty substring matches everything", s)
		}
	case AccNotContains:
		if rest == "" {
			return AcceptanceCheck{}, fmt.Errorf("acceptance check %q: empty substring can never pass", s)
		}
	case AccRegex:
		if rest == "" {
			return AcceptanceCheck{}, fmt.Errorf("acceptance check %q: empty pattern matches everything", s)
		}
		re, err := regexp.Compile(rest)
		if err != nil {
			return AcceptanceCheck{}, fmt.Errorf("acceptance check %q: %w", s, err)
		}
		c.re = re
	case AccMinItems:
		// Split on the LAST colon so the count parses even if a field name
		// ever carries a colon (it should not — but the count is the
		// unambiguous tail either way).
		i := strings.LastIndex(rest, ":")
		if i < 0 {
			return AcceptanceCheck{}, fmt.Errorf("acceptance check %q: want min_items:<field>:<n>", s)
		}
		field, nstr := rest[:i], rest[i+1:]
		if field == "" {
			return AcceptanceCheck{}, fmt.Errorf("acceptance check %q: field name is required", s)
		}
		n, err := strconv.Atoi(nstr)
		if err != nil {
			return AcceptanceCheck{}, fmt.Errorf("acceptance check %q: %q is not an integer", s, nstr)
		}
		if n < 1 {
			return AcceptanceCheck{}, fmt.Errorf("acceptance check %q: a minimum of %d can never fail", s, n)
		}
		c.Arg, c.N = field, n
	case AccNonempty:
		if rest == "" {
			return AcceptanceCheck{}, fmt.Errorf("acceptance check %q: field name is required", s)
		}
	default:
		return AcceptanceCheck{}, fmt.Errorf("acceptance check %q: unknown verb %q (want contains, not_contains, regex, min_items, nonempty)", s, kind)
	}
	return c, nil
}

// Pattern returns the compiled regex for an AccRegex check (nil for every
// other kind, and for a zero AcceptanceCheck). Exposed so consumers that
// analyze checks — the intake lint — reuse the parse-time compilation instead
// of recompiling, which would re-introduce a can-never-fail error branch
// whose correctness silently depends on staying dead.
func (c AcceptanceCheck) Pattern() *regexp.Regexp { return c.re }

// Eval runs the check against a result. reason is empty exactly when pass is
// true; on failure it names the check and what was observed, because these
// reasons surface verbatim in the delegator's merge decision and the
// delegation ledger — "failed" without a why is unactionable telemetry.
func (c AcceptanceCheck) Eval(structured json.RawMessage, output string) (pass bool, reason string) {
	switch c.Kind {
	case AccContains:
		if strings.Contains(evalText(structured, output), c.Arg) {
			return true, ""
		}
		return false, fmt.Sprintf("%s: %q not found in output", c.raw, c.Arg)
	case AccNotContains:
		if !strings.Contains(evalText(structured, output), c.Arg) {
			return true, ""
		}
		return false, fmt.Sprintf("%s: forbidden %q found in output", c.raw, c.Arg)
	case AccRegex:
		if c.re != nil && c.re.MatchString(evalText(structured, output)) {
			return true, ""
		}
		return false, fmt.Sprintf("%s: pattern did not match output", c.raw)
	case AccMinItems:
		field, reason := structuredField(structured, c.Arg, c.raw)
		if reason != "" {
			return false, reason
		}
		var items []json.RawMessage
		if err := json.Unmarshal(field, &items); err != nil {
			return false, fmt.Sprintf("%s: field %q is not an array", c.raw, c.Arg)
		}
		if len(items) < c.N {
			return false, fmt.Sprintf("%s: field %q has %d items, want ≥ %d", c.raw, c.Arg, len(items), c.N)
		}
		return true, ""
	case AccNonempty:
		field, reason := structuredField(structured, c.Arg, c.raw)
		if reason != "" {
			return false, reason
		}
		return nonemptyValue(field, c.Arg, c.raw)
	}
	return false, fmt.Sprintf("%s: unknown check (not built by ParseAcceptanceCheck?)", c.raw)
}

// evalText picks what the text verbs read: the final assistant Output, or the
// raw structured bytes when Output is empty (a schema-only result must still
// be checkable — an empty target would make contains: unpassable and
// not_contains: vacuously green, both wrong).
func evalText(structured json.RawMessage, output string) string {
	if output == "" && len(structured) > 0 {
		return string(structured)
	}
	return output
}

// structuredField pulls one top-level field out of the structured result,
// with a precise failure reason at each layer (absent result / non-object /
// absent field). v1 addresses top-level fields only — the gbnf subset only
// produces flat objects anyway, so nesting cannot occur in a validated result.
func structuredField(structured json.RawMessage, name, raw string) (json.RawMessage, string) {
	if len(structured) == 0 {
		return nil, fmt.Sprintf("%s: no structured output to check", raw)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(structured, &obj); err != nil {
		return nil, fmt.Sprintf("%s: structured output is not a JSON object", raw)
	}
	field, ok := obj[name]
	if !ok {
		return nil, fmt.Sprintf("%s: field %q absent from structured output", raw, name)
	}
	return field, ""
}

// nonemptyValue decides emptiness by JSON kind: null and "" and [] and {}
// are empty; a present scalar — INCLUDING 0 and false — is a value.
// nonempty guards against omission, not against zero: a count of 0 is an
// answer, an absent count is a non-answer.
func nonemptyValue(field json.RawMessage, name, raw string) (bool, string) {
	trimmed := strings.TrimSpace(string(field))
	switch {
	case trimmed == "" || trimmed == "null":
		return false, fmt.Sprintf("%s: field %q is null", raw, name)
	case strings.HasPrefix(trimmed, `"`):
		var s string
		if err := json.Unmarshal(field, &s); err != nil || s == "" {
			return false, fmt.Sprintf("%s: field %q is an empty string", raw, name)
		}
	case strings.HasPrefix(trimmed, "["):
		var items []json.RawMessage
		if err := json.Unmarshal(field, &items); err != nil || len(items) == 0 {
			return false, fmt.Sprintf("%s: field %q is an empty array", raw, name)
		}
	case strings.HasPrefix(trimmed, "{"):
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(field, &obj); err != nil || len(obj) == 0 {
			return false, fmt.Sprintf("%s: field %q is an empty object", raw, name)
		}
	}
	return true, ""
}
