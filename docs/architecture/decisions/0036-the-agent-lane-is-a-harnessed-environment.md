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

## Consequences

- A nil/zero table is byte-identical to the pre-key loop; every field is additive and `omitempty`, and a
  pre-0.113.22 node's result reads as "no trace", never as "no calls".
- The corpus gains the per-step axis the rigger needs (`loop`, `long-observation`, `tool-misuse`, `budget`) from
  ordinary traffic — no special measurement mode.
- An operator can now express, per seat, the constraints the bake-offs found by hand — and read them back.
- Cost: one more table to keep honest. Mitigation: validation at load, a summary line in build notes, the
  starter table in `examples/agent-env-rules.json`.

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

## Related docs

- [systems/coding-agent.md](../../systems/coding-agent.md) — "Environment rules"
- [systems/fleet-node.md](../../systems/fleet-node.md) — the wire result fields
- [ADR 0003](0003-policy-broker-and-capability-flags-off-by-default.md) — the risk table this does not touch
- [glossary: Environment rule](../../glossary.md#environment-rule)
