---
status: Accepted
date: "2026-09-07"
---

# 0036 — The agent lane is a harnessed environment: rules are data, the trace is telemetry, the rigger proposes

Decision provenance: the operator ordered on 2026-09-07 that google-research/envharness and axolotl "get
integrated — see how to best do it and what components might not be worth it for us or what components need
patching/upgrading/porting". This ADR records the envharness half (P1 of the build order; the rigger and the
training lane follow under their own records). The research and the component-by-component verdicts live in the
workspace record `Ecosystem/2026-09-07-envharness-axolotl-integration/DESIGN.md`.

## Context

envharness frames an agent task as an **environment**: a frozen policy (the LLM) acts through tools; a `Rules`
object intercepts every step in three places — `filter_action` (block or rewrite the call), `modify_transition`
(rewrite the result), `filter_observation` (bound what the policy reads); a `Trace` of typed `Step`s is the
episode's record; an **EnvRigger** loop reads recent traces, diagnoses failure axes, writes new rules, and
validates them before/after on the same policy. It is the right vocabulary for this harness: the coding agent
loop already IS that environment (contract + tools over `read_root`; `evaluate` = acceptance DSL + schema), and its
seats are exactly the weak policies envharness was written for.

What the harness had before this ADR, in that vocabulary:

- `filter_action`, partially: the profile's tool subset (narrow-only), the circuit breakers (exact-repeat refusal,
  `max_same_tool`), the unattended risk park, the structural risk rules (deny/ask on effects).
- `filter_observation`, partially: one window-derived cap on a tool result at the loop boundary.
- `modify_transition`: nothing.
- `Trace`: the delegation-log corpus records the CONTRACT and the RESULT, not the steps. Measured on 2026-09-07
  (1,049 rows, six days): 51 of the 58 failed 4B rows stop at exactly two steps with empty schema fields, and 24
  of the 27B pool's defers are "step budget exhausted (12 steps)" — and the corpus cannot say what any of those
  steps did.
- Rigger: none. Profiles were tuned by hand from bake-offs (the "profile lever": 0 → 72 % on the ampere-6 seat).

envharness also lets the rigger LLM **write Python `Rules` subclasses and execute them in-process**
(`rules_code`, `load_rules_subclass`). That is the one component this harness refuses: generated code running
inside the loop is out of policy here, and a rule nobody can read as data cannot be reviewed, diffed, or tested.

## Decision

1. **Environment rules are a CLOSED, typed vocabulary in config** — `agent_env_rules` (`core.AgentEnvRules`), a
   property of the SEAT (per box / per fleet node), never of the contract: `deny_tools`, `allow_tools`,
   `max_calls_per_tool`, `arg_limits`, `max_observation_tokens`, `observation_strip`, `rewrite_error`. Validated
   wherever a loop is built, so a bad rule fails by name at every door (CLI exit 2, MCP `agent_run` defer, fleet
   `config`-class defer) and never silently no-ops. `local-agent --env-rules <file>` replaces the table for one
   run — the rigger's scratch validation — and `off` runs without one.
2. **The three interceptors wrap every tool call in envharness's order** (`internal/agent/envrules.go`):
   `FilterAction` runs BEFORE the loop's own breakers (a blocked call spends no execution count; a rewritten
   argument is what the breakers and the tool both see, and the clamp is named on the result the model reads);
   `ModifyTransition` then `FilterObservation` run on TOOL OUTPUT only, before the loop-boundary cap — a
   loop-authored line (a block reason, a breaker refusal, `unknown tool`) is never rewritten or stripped. Denied
   tools are withheld structurally (the spec is not sent), like a profile's narrowing, and a tool that reaches
   its `max_calls_per_tool` cap is withheld the same way for the rest of the run — a weak model does not reliably
   read a text refusal, and a capped tool it kept re-calling would burn the step budget on blocked calls. Per-run
   counters live in per-run state: one `Loop` serves concurrent handlers under `--serve`.
3. **Env rules and risk rules stay two tables.** Risk rules (`--rules`, ADR 0003's broker) decide what an effectful
   action may DO to the world — tighten-only, security. Env rules shape how a WEAK SEAT behaves inside the loop.
   An env rule never grants or denies an effect; a risk rule never rewrites an observation.
4. **The trace is corpus telemetry.** Every wire result (and `agent_run` response) carries `trace`: per tool call
   the tool, its effect status, the size the model actually read (`obs_chars`), and the env rule that decided —
   plus `rules_fired`. No transcript bytes. Set BEFORE the defer branches, like the prefill accounting: the
   budget/timeout runs are the ones a diagnosis needs most.
5. **The rigger will PROPOSE, never apply.** Its output (P3) is an edit to this same table plus a before/after
   measurement on the same seat; an operator (or a standing rule) applies it. Fail-loud over autonomous recovery.

### Amendment 2026-09-07 — E3, the setup replay (0.113.24)

envharness's `Setup` replays a fixed action list on `reset()` and does not charge the replay to the
episode budget. The harness's equivalent is `setup_actions` on the contract (`core.AgentSetupAction`,
`agent/setup.go`) plus a node-side key, `agent_seed_context_reads`, that prepends one `read_file` per
context doc — the node, not the delegator, knows the file names it materializes. Decided, with the
reasons on record:

- **Tool-result shape, not a user message.** The replay is one assistant turn carrying the calls and
  one tool result per call — the shape the seat itself produces on every later step. The council's
  alternative (inline the document text as a fenced user message before the objective, like
  `AGENT.md`) was rejected on a measured fact, not taste: to a small seat a user turn is a question
  (2026-09-03, the few-shot leak), and grounded contracts already ship system + objective only.
- **Consistent accounting.** No step is charged (`Steps` counts model turns); the wall is. The circuit
  breakers are neither consulted nor fed (the model never issued these calls; its later identical
  call is a first call). `filter_action` applies, but the `max_calls_per_tool` counter is not spent by
  the replay. The observation hooks and the boundary cap apply. The results are pinned for compaction
  but sit OUTSIDE the protected preamble, and the replay is bounded to half the compaction budget:
  past it the rest are recorded not-run, and the model reads for itself — the pre-key behaviour,
  never a preamble the seat cannot fit.
- **Per-action truth.** Every action is a Step-0 effect record marked `setup`; `setup_ran` counts
  only what executed. A node one release behind ignores the field and reports nothing — the
  decoder keeps unknown fields for exactly this mixed-fleet case (`TestDecodeAgentContractUnknownFieldIgnored`).
- **Acceptance is pass rate, not step count.** The corpus's failed 4B contracts (117 in six days, all
  grounded, 89 stopping at `list_dir` + `read_file`) re-run on the same seat with and without seeding.
  A step count of 1 is guaranteed by construction and proves only that the plumbing ran.

### Amendment 2026-09-07 — E4, the rigger's first slice (0.113.26)

The full rigger (observe → diagnose → write → validate, plus envharness's difficulty-zone and red-team objectives)
was roasted before it was built and RESHAPED: the closed rule vocabulary has no lever for ~70 % of the day's
failures (wall timeouts, seat errors), only 16 corpus rows carried a trace, the row kept a status but not the error
text a `rewrite_error` rule would need, and the digest-8 gate cannot fail for axes it never exercises — a proposer
on top of an unvalidated diagnosis would inherit every ambiguity. Decided:

- **A classifier first, as a contract.** `internal/rig` puts every failed row on exactly one axis in a PUBLISHED
  precedence order (`seat-infra → timeout → budget → abstention → schema-miss → anchor-miss → loop →
  long-observation → tool-misuse → unclassified`), with a fixture of one row per boundary case pinning the order.
  Weights are over the rows ELIGIBLE for an axis (trace axes: rows with a trace), never over all rows.
- **Remedies are a lookup table, said so.** The rigger's value is the weight and the evidence list; the remedy per
  axis is pre-authored, its value derivation stated in words, and "not a rule matter" is a first-class answer.
  It proposes nothing on its own and applies nothing.
- **The trace carries `note`** (≤ 160 bytes of what the model was told) on non-committed calls, so the corpus
  accumulates the evidence the later proposer needs.
- **Acceptance is agreement, not a tautology.** A fresh-context reader applied the rules as written to a
  stratified 41-row sample of real rows: 41/41 agreement; the three coverage gaps that pass surfaced (placement
  failures with no defer class, "output failed schema" under abstention, contains/regex anchor misses) became rules
  and were re-labelled blind (41/41 again). A validated remedy remains a measured A/B on the seat (P2's
  `setup_ab.py` is the model); P3b — proposer, validation gate, objectives — waits for a trace corpus in the hundreds.

### Amendment 2026-10-15 — recipes: the rules P3b will be built under, and the store its output lands in

P3b (the proposer, its validation gate, the objectives) is parked until the trace corpus numbers in the hundreds.
Before any of it exists, this amendment fixes the rules it will be built under, and builds the two pieces that do
not need the corpus: the record a proposal lands in (the recipe store, `internal/recipe`) and the audit that judges
one (`local-offload recipe-audit`). Decided, with the reasons on record:

- **Candidates are closed-vocabulary data, never code.** A candidate is an edit to `agent_env_rules`
  (`core.AgentEnvRules`, the seven keys of decision 1) and nothing else. No rule class, expression language,
  template or generated function enters the vocabulary for a proposer's sake, and `rules_code` stays refused. The
  store holds a candidate as a typed record and decodes it as strictly as the `--env-rules` file: an unknown key
  anywhere is refused, and so is every string that is not shaped like rule data (a lower-case identifier for a tool
  or argument name, one line of plain ASCII for a `rewrite_error` text, a bounded pattern that does not match the
  empty string). The shapes keep the store from holding anything a reviewer could not read as data; what keeps a
  recipe harmless is decision 3: an env rule never grants or denies an effect.
- **The proposer proposes and never applies.** Decision 5, now binding on any proposer: its whole output is a
  `proposed` recipe in the store. Publishing is the audit's result and nothing else; applying a published recipe to
  a seat's `agent_env_rules` is an operator's edit. A `published` record that does not carry the audit that earned
  it does not decode, so a record cannot be published by editing one word.
- **Only the branch, the parallelism and the stop class are replay-scorable.** A recorded trace holds, per step,
  the tool, its fate, the bytes the model read and (for the run) the stop reason. A rule that only removes or caps
  calls (`deny_tools`, `allow_tools`, `max_calls_per_tool`) can be read against it: which branch would have been
  cut, how many calls of one tool a run made, and whether the run's stop class (budget, loop) is one such a cut can
  move. Every other key changes an argument or what the seat reads, so every later step of the run changes with it
  and no recording says what the seat would have done next. `internal/recipe` classifies each key of the vocabulary,
  and a test fails when `core.AgentEnvRules` gains a key nobody classified.
- **Rules that rewrite observations need a live A/B.** `max_observation_tokens`, `observation_strip` and
  `rewrite_error` rewrite what the seat reads, and `arg_limits` rewrites what a call returns; none is scored by
  replay. They are scored by running the seat with and without the recipe on the same contracts, which is what
  `recipe-audit` does (below), under a GPU lease.
- **Recipes are distilled only from acceptance-passing runs and carry a seat-capability tag.** A recipe names the
  runs it was distilled from (`provenance`), all of which passed their contract's acceptance checks, and the
  capability class of the seat they ran on (`seat_tag`, an operator-chosen slug: the harness checks its shape and
  that an audit's `--seat-tag` equals it, it does not define a vocabulary). A recipe is audited only on a seat of
  its class; `recipe-audit` refuses a mismatch before it asks the seat anything. The store checks the shape of the
  provenance, not the runs: the delegation log is the evidence, and a proposer that distils from a failing run is
  out of contract.
- **The audit.** `recipe-audit` takes a frozen pre-registration (`gate freeze`, judge
  `{deterministic, acceptance-dsl}`, the primary on the unit interval because every run scores 1 or 0, tokens-class
  efficiency metrics only because nothing else can be read off a run, no frozen incumbent) and a file of contracts, one
  per declared task. It runs
  every task on one seat twice: the incumbent arm is the seat as it is, the candidate arm has the recipe's rule set
  in place of the box's table. The acceptance DSL is the judge. The decision is the measurement gate's, built as
  `gate verdict` builds it. The recipe is published only when the verdict is ADOPT and the candidate's mean steps,
  over the tasks both arms finished, are no worse than the incumbent's; anything else demotes it, an INCONCLUSIVE and
  a tie included, because the incumbent keeps every tie. Steps are the verb's own check beside the gate, because the
  gate's efficiency classes are tokens, money and memory and never a count of steps. A demoted recipe stays in the
  store with the audit that demoted it: nothing is deleted. Both arms are stamped with the pin the harness takes of the
  seat that served the runs (weights, quant, engine build, window, template), beside the rule table each arm ran under,
  so the gate's era is the seat and not the alias it was asked for; a seat whose pin moves mid-audit ends the audit and
  nothing is recorded. The verb never takes a lease: with no `GPU_LEASE_EPOCH`
  in its environment, or an epoch no live lease carries, it refuses with exit 2 before the seat is asked, and it asks
  again before every seat call.
- **No proposer is built until trace-bearing rows number in the hundreds.** The E4 count was 16, and a proposer over
  that would inherit every ambiguity that stopped the full rigger. This amendment builds the store, the audit and
  the rules; it builds none of the proposer, the replay scorer or the objectives. Reopen trigger: `trace-corpus
  readout` reports trace-bearing runs in the hundreds, and the seat the proposer is for has a frozen pre-registration
  to be audited against.

## Consequences

- A nil/zero table is byte-identical to the pre-key loop; every field is additive and `omitempty`, and a
  pre-0.113.22 node's result reads as "no trace", never as "no calls".
- The corpus gains the per-step axis the rigger needs (`loop`, `long-observation`, `tool-misuse`, `budget`) from
  ordinary traffic — no special measurement mode.
- An operator can now express, per seat, the constraints the bake-offs found by hand — and read them back.
- Cost: one more table to keep honest. Mitigation: validation at load, a summary line in build notes, the
  starter table in `examples/agent-env-rules.json`.
- The recipe store and the audit add one typed record and one verb. Nothing reads a published recipe: an
  operator copies its rule set into the seat's table, so a store that is empty or absent changes nothing.

## Alternatives considered

- **Port `rules_code` (LLM-written rule classes).** Rejected: in-process generated code; unreviewable; the
  harness's never-cloud, fail-loud posture has no place for it.
- **Put env rules on the contract.** Rejected: what a 4B needs is not what a 27B needs, and a contract runs on
  whichever seat placement picks; the profile precedent (ADR-less, but measured) already made this a seat property.
- **Fold env rules into the risk table.** Rejected: different questions (security vs behaviour), different
  reviewers, different failure costs; one table with two authorities is how the risk table's tighten-only line
  gets eroded.
- **Keep the transcript in the corpus instead of a trace.** Rejected: bytes the corpus cannot afford (context docs
  are already capped), and the transcript is the one thing a diagnosis does not need first.

## Related code

- [`internal/core/agentenvrules.go`](../../../internal/core/agentenvrules.go) — the vocabulary, `Validate`, the trace step
- [`internal/agent/envrules.go`](../../../internal/agent/envrules.go) — the interceptors, `CompileEnvRules`, `Loop.WithEnvRules`
- [`internal/agent/loop.go`](../../../internal/agent/loop.go) — the call sites around `dispatchOrThrottle`
- [`internal/agent/builder.go`](../../../internal/agent/builder.go) — `BuildConfig.EnvRules`, the build note
- [`internal/pipeline/agenttask.go`](../../../internal/pipeline/agenttask.go) — `TraceFromEffects`, the wire fields
- [`cmd/local-agent/main.go`](../../../cmd/local-agent/main.go) — `--env-rules`
- [`examples/agent-env-rules.json`](../../../examples/agent-env-rules.json)
- [`internal/recipe/recipe.go`](../../../internal/recipe/recipe.go) — the recipe record, strict decode, the closed-vocabulary shapes
- [`internal/recipe/store.go`](../../../internal/recipe/store.go), [`scoring.go`](../../../internal/recipe/scoring.go) — the store; which keys replay can score
- [`recipe_audit_cmd.go`](../../../recipe_audit_cmd.go) — the `recipe-audit` verb

## Related docs

- [systems/coding-agent.md](../../systems/coding-agent.md) — "Environment rules"
- [systems/fleet-node.md](../../systems/fleet-node.md) — the wire result fields
- [systems/measurement-gate.md](../../systems/measurement-gate.md) — the gate `recipe-audit` decides with
- [ADR 0003](0003-policy-broker-and-capability-flags-off-by-default.md) — the risk table this does not touch
- [glossary: Environment rule](../../glossary.md#environment-rule)
