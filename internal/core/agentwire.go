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
	"path"
	"path/filepath"
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

	// --- The write door (register D-06). Fixed ceilings, NOT contract fields.
	// A caller cannot raise them (that is the point of a door) and does not
	// need to lower them: `diff_max_files:<n>` already expresses "this leg may
	// touch one file", and two knobs for one job is how they drift apart.
	//
	// AgentWriteMaxFiles caps the distinct paths one contract may touch. Eight
	// is a small implementation leg — a file, its test, and room to be wrong —
	// and anything bigger is a leg that should have been split before it was
	// handed to a 4B.
	AgentWriteMaxFiles = 8
	// AgentWriteMaxBytes caps the TOTAL bytes a contract may write. Generous
	// for an edit and nowhere near enough to matter to the node's disk.
	AgentWriteMaxBytes = 64 << 10
	// AgentWriteDiffMaxBytes caps the rendered unified diff that crosses back.
	// Larger than the write cap because a diff carries context lines and both
	// sides of every change; a diff past this is REFUSED, never truncated — a
	// truncated patch applies as silent damage.
	AgentWriteDiffMaxBytes = 192 << 10
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
	// DeferClassWrite (0.119.0, register D-06): the contract asked for the
	// WRITE door and this node will not open it — `agent_allow_write` is false
	// here, or the finished write set broke a door cap. Neither a broken stack
	// (nothing is wrong with the box) nor an unplaceable contract (another node
	// may well have opted in), so it is its own class: the fix is an opt-in on
	// a node or a smaller change, and re-placing the contract is the right
	// delegator reflex.
	DeferClassWrite = "write"
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
	// WriteRoot (0.119.0, register D-06) opens the WRITE door: the directory,
	// RELATIVE to the run read root, that the seat may create and change files
	// under. "" (the default and every pre-0.119 contract) is read-only, with
	// no write tool registered at all. "." is the whole read root.
	//
	// Relative, not absolute, on purpose. The contract is self-contained: the
	// node materializes its OWN copy of the inline context docs and has never
	// seen the delegator filesystem, so an absolute delegator-box path would
	// name nothing on the executing node. A relative root resolves to an
	// absolute directory inside the read root on whichever box runs it — the
	// containment the door needs — and it is enforced by os.Root at the
	// syscall layer rather than by comparing two path strings.
	//
	// The write set is NEVER applied by the harness. It comes back as a
	// unified diff (AgentWireResult.Diff) for the caller to review and apply.
	WriteRoot string `json:"write_root,omitempty"`
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
	// Wall sizing (0.115.21, register D-03). SeatTokS is this run's effective
	// decode rate (completion tokens per second of call wall over completions
	// of ≥ 1,024 tokens; 0 = no qualifying completion). WallEstimateSec is the
	// wall the contract was estimated to need on this seat BEFORE the loop ran
	// (cold load + think block + tool steps + final answer at the seat's
	// remembered rate); MinTurnSec is a cold load plus one turn at the final
	// budget — the least wall a retry is worth (D-46); WallNote names the
	// arithmetic, or why there is none. Published, never imposed: the run
	// proceeds under the contract's timeout_sec exactly as before.
	// Re-pack accounting (0.115.23, register D-91): how long the structured
	// re-pack ran, how many seat completions it spent, and why it stopped or
	// was skipped. A re-pack of a 12 KB length-cut answer on the 4B ran three
	// full re-generations (~690 s) into the 900 s wall on 2026-09-10 and
	// turned a finished loop into a budget defer with nothing on the wire to
	// say where the time went.
	RepackMs       int64  `json:"repack_ms,omitempty"`
	RepackAttempts int    `json:"repack_attempts,omitempty"`
	RepackNote     string `json:"repack_note,omitempty"`
	SeatTokS        float64 `json:"seat_tok_s,omitempty"`
	WallEstimateSec int     `json:"wall_estimate_sec,omitempty"`
	MinTurnSec      int     `json:"min_turn_sec,omitempty"`
	WallNote        string  `json:"wall_note,omitempty"`
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
	// ResponseShape (0.115.13, register D-45) is the seat's OBSERVED answer
	// shape on this run — which wire key carried hidden reasoning (`reasoning`
	// on vLLM, `reasoning_content` on llama.cpp, none), whether reasoning
	// tokens were reported, how many tool calls parsed, how many completions
	// ran. The pin above says what the seat IS; this says how it answered —
	// the two seat facts (parser mismatch 2026-09-04, reasoning-key blind spot
	// 2026-09-10) the corpus could not show. Omitempty: absent when no
	// completion ran or on a pre-0.115.13 node.
	ResponseShape string `json:"response_shape,omitempty"`

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
	// --- The write door (0.119.0, register D-06). Present only on a contract
	// that opened it. Diff is the unified diff of everything that changed under
	// write_root during the run, a/ and b/ prefixed so it applies with
	// `git apply -p1`; DiffFiles lists the touched paths in the same order the
	// diff renders them. The harness applies NEITHER — reviewing and applying
	// the change is the caller's job, and that is the whole safety story of
	// letting a 4B write anything at all.
	//
	// WriteNote says what the door did when there is no diff to read: the node
	// has not opted in, the write set broke a cap, nothing was written. Absent
	// on a read-only contract, which is how a reader tells "no write door" from
	// "write door, nothing written".
	Diff      string   `json:"diff,omitempty"`
	DiffFiles []string `json:"diff_files,omitempty"`
	WriteNote string   `json:"write_note,omitempty"`
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
	ForcedFinal      bool   `json:"forced_final,omitempty"`
	// Ms (0.115.21): the client-measured wall of the completion.
	Ms int64 `json:"ms,omitempty"`
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
	if err := ValidateWriteRoot(c.WriteRoot); err != nil {
		return fmt.Errorf("agent contract: %w", err)
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

// ValidateWriteRoot holds a contract's write_root to a safe RELATIVE directory
// path. "" is the read-only default and always valid.
//
// The rules are the strictest platform's, applied on every platform, for the
// same reason validDocName's are: a contract is a WIRE object validated on the
// delegator and again on a node that may be a different operating system, so a
// Linux-only check would accept a path the Windows node then resolves somewhere
// else. os.Root is the real containment; this is the layer that refuses the
// shapes whose DAMAGE is done before os.Root ever sees them (a caller believing
// it scoped a write to one directory when it did not).
func ValidateWriteRoot(root string) error {
	if root == "" {
		return nil
	}
	if strings.ContainsAny(root, "\x00") {
		return fmt.Errorf("write_root %q contains NUL", root)
	}
	if filepath.IsAbs(root) || filepath.VolumeName(root) != "" || strings.HasPrefix(root, "/") || strings.HasPrefix(root, "\\") {
		return fmt.Errorf("write_root %q must be RELATIVE to the run's read root (the executing node never sees the delegator's filesystem, so an absolute path names nothing there)", root)
	}
	clean := path.Clean(strings.ReplaceAll(root, "\\", "/"))
	if clean == "." {
		return nil // the whole read root
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("write_root %q escapes the read root", root)
	}
	// path.Clean above already collapsed empty and "." segments and folded
	// every interior "..", and a leading ".." was refused just now — so the
	// loop below only has to police the shapes Clean does NOT normalize away.
	for _, seg := range strings.Split(clean, "/") {
		if trimmed := strings.TrimRight(seg, " ."); trimmed != seg {
			return fmt.Errorf("write_root %q has a segment with a trailing space or dot (Windows strips those, so the door would open somewhere else than it reads)", root)
		}
		if strings.ToLower(seg) == ".git" {
			return fmt.Errorf("write_root %q points into a .git directory", root)
		}
		stem := seg
		if i := strings.Index(stem, "."); i >= 0 {
			stem = stem[:i]
		}
		if windowsDeviceNames[strings.ToLower(stem)] {
			return fmt.Errorf("write_root %q has the reserved Windows device name %q as a segment", root, seg)
		}
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
//	diff_touches:<prefix> the write set holds a path starting with <prefix>
//	diff_max_files:<n>    the write set is NON-EMPTY and touches <= n files
//
// The two diff verbs read the run's write set (write_root, D-06). Both fail
// CLOSED on a read-only or empty write set, diff_max_files included: a cap
// assertion that passes because nothing was written would make a contract that
// verified nothing read as verified, which is the exact shape this DSL exists
// to refuse.
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
	// The write-door verbs (0.119.0, register D-06) read the RESULT'S WRITE
	// SET — the paths the seat actually changed — not its prose. They are the
	// only acceptance a write contract can carry that a talkative seat cannot
	// satisfy by talking.
	AccDiffTouches  AcceptanceKind = "diff_touches"
	AccDiffMaxFiles AcceptanceKind = "diff_max_files"
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
	case AccDiffTouches:
		if rest == "" {
			return AcceptanceCheck{}, fmt.Errorf("acceptance check %q: empty path prefix matches every write", s)
		}
		// Normalized once, here, so a Windows-authored contract
		// ("internal\foo") matches a diff whose paths are always slashed.
		c.Arg = strings.ReplaceAll(rest, "\\", "/")
	case AccDiffMaxFiles:
		n, err := strconv.Atoi(rest)
		if err != nil {
			return AcceptanceCheck{}, fmt.Errorf("acceptance check %q: %q is not an integer", s, rest)
		}
		if n < 0 {
			return AcceptanceCheck{}, fmt.Errorf("acceptance check %q: a negative file count is not a bound", s)
		}
		c.Arg, c.N = "", n
	default:
		return AcceptanceCheck{}, fmt.Errorf("acceptance check %q: unknown verb %q (want contains, not_contains, regex, min_items, nonempty, diff_touches, diff_max_files)", s, kind)
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
func (c AcceptanceCheck) Eval(res AgentWireResult) (pass bool, reason string) {
	structured, output := res.Structured, res.Output
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
	case AccDiffTouches:
		if len(res.DiffFiles) == 0 {
			return false, fmt.Sprintf("%s: the run produced NO write set (no write_root, or nothing was written)", c.raw)
		}
		for _, f := range res.DiffFiles {
			if strings.HasPrefix(f, c.Arg) {
				return true, ""
			}
		}
		return false, fmt.Sprintf("%s: no changed path starts with %q (changed: %s)", c.raw, c.Arg, strings.Join(res.DiffFiles, ", "))
	case AccDiffMaxFiles:
		if len(res.DiffFiles) == 0 {
			return false, fmt.Sprintf("%s: the run produced NO write set (no write_root, or nothing was written)", c.raw)
		}
		if len(res.DiffFiles) > c.N {
			return false, fmt.Sprintf("%s: the run changed %d files, want <= %d (changed: %s)", c.raw, len(res.DiffFiles), c.N, strings.Join(res.DiffFiles, ", "))
		}
		return true, ""
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
