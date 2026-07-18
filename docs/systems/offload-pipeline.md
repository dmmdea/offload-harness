# Offload pipeline

## Purpose

The Cascade: the code that takes a grunt-work task, walks it up a ladder of local model Tiers, and
either returns validated structured output or a Defer. This is the core of the harness — everything
else is a surface onto it.

## Questions this doc answers

- Which model runs first, and what makes the harness try a bigger one?
- What exactly triggers a Defer, and what does a caller do with it?
- Where do the confidence thresholds come from, and what are the defaults?
- Why does the coding agent get a different path through the same code?
- Where are token savings recorded?

## Scope

Task dispatch, the Tier chain and escalation, output validation, grounding, confidence gating, the
terminal reasoning Tier, defer construction, and ledger accounting. Covers the text tasks
(summarize, classify, extract, triage) and the dispatch layer for vision, OCR, speech, and media
tasks.

## Non-scope

- How models are served, and with which flags → [setup-installer.md](setup-installer.md)
- How media generation actually renders → [media-generation.md](media-generation.md)
- The tool surface callers see → [mcp-server.md](mcp-server.md)
- The agent loop that consumes the recordless path → [coding-agent.md](coding-agent.md)

## Key concepts

**Tier** — one model seat, named by a stable alias. **Cascade** — the ordered set of Tiers a task
walks. **Defer** — a structured "you do it" result. **Escalation** — moving to the next Tier after a
recoverable failure. **Grounding** — checking that output values actually appear in the input.

## How the system works

A request names a task and carries input. The pipeline builds a Tier chain for that task, then walks
it:

```
chain = [triage_model?] → model → escalation_model
```

The triage Tier is only included for `triage` and `classify` tasks, and can be skipped by the
entry-tier router. Duplicate aliases collapse, Tiers whose circuit breaker is open are skipped, and
if everything is pruned the chain falls back to the workhorse model alone.

For each Tier the pipeline generates under a GBNF grammar, then applies a series of gates. Each gate
answers "is this good enough, and if not, is it worth trying a bigger model?"

- **Schema validation.** Output that does not satisfy the compiled schema is a retry/escalate.
- **Grounding.** Extract output whose values do not appear in the source escalates. Grounding is
  *computed and logged* for other tasks but only *actioned* for extract — summarization paraphrases
  legitimately, so acting on it would be noise.
- **Confidence gate.** For classify, a self-reported confidence below `classify_min_confidence`
  (default **0.45**) escalates. For decision tasks, a logprob decision margin below the task's
  threshold escalates — a learned per-task conformal value when one exists, otherwise
  `confidence_margin_threshold` (default **0.35**).
- **Confhead gate.** A learned correctness head below its threshold escalates.

An OK result returns immediately. A recoverable failure at a non-final Tier escalates. Infrastructure
failures — connection refused, timeouts, 5xx — do **not** escalate, because a bigger model on a
broken endpoint fails the same way; they defer with `err_class` set.

When the whole chain has deferred, one last attempt runs on the **terminal reasoning Tier**, for
grammar tasks whose output was not truncated. It gets a thinking span supplied by the grammar
(`WrapThinking`) and an extra token budget, runs once, and is not subject to the confidence gate.
Results from it are marked `Reasoning: true`, which is what distinguishes them from ordinary
escalation results in the ledger — both run on the same model.

If that also fails, the pipeline returns a Defer and records it.

## Important flows

- [../flows/cascade-escalation-and-defer.md](../flows/cascade-escalation-and-defer.md) — the walk in
  detail.

## Data and state

- **Ledger** — append-only JSONL at the configured `ledger_path`, `fsync`ed per entry so a crash
  cannot lose recorded savings. Carries `tokens_saved` (input tokens kept out of the calling model)
  and per-call metadata.
- **Cache** — keyed result reuse; bypassed entirely on the recordless path.
- **Learned thresholds** — per-task conformal values loaded from `thresholds.json` when present,
  falling back to config defaults.
- **Circuit breakers** — per-Tier, consulted during chain construction.

## Interfaces and entry points

- `Run` — the full cascade with recording.
- `RunTier` — one specific Tier, no escalation, no recording.
- `NewRecordlessPipeline` / `NewRecordlessOffload` — the agent-facing construction. This is the single
  place the nil-store invariant is built: nil cache and nil ledger, so agent-internal offload calls
  leave no trace in savings accounting. A defer here is returned as a *successful tool result*
  (`{"deferred": true, "reason": ...}`) rather than an error, because the agent loop should read it
  and move on.

## Dependencies

`internal/llamaclient` (local completion endpoint), `internal/gbnf` (schema→grammar),
`internal/grounding`, `internal/confidence`, `internal/ledger`, `internal/config`. Media, vision, and
speech tasks dispatch out to their own backends.

## Downstream effects

Every caller — CLI, MCP tools, the coding agent, fleet jobs — goes through here. Changing gate
behavior changes the defer rate everywhere at once, which is why thresholds are config-driven rather
than compiled in.

## Invariants and assumptions

1. **A Defer is a success signal.** Never convert one into an error, and never add a cloud fallback
   to avoid one — see
   [ADR 0001](../architecture/decisions/0001-defer-never-cloud-fallback.md).
2. **Structured output comes from a raw GBNF grammar field**, never `--json-schema` or
   `response_format` — see
   [ADR 0002](../architecture/decisions/0002-grammar-reliable-serving-flags.md).
3. The recordless path writes nothing — no ledger, no cache, no shadow capture.
4. Infrastructure failures do not escalate.
5. The reasoning Tier never fabricates a pass: garbage from it still defers.

## Error handling

Recoverable model-quality failures escalate. Infrastructure failures defer with `err_class`
(`oom`, `timeout`, `http_5xx`, `conn_refused`, `gpu_busy`). Exhausting the chain defers with the last
reason and any partial output preserved in `Partial`.

## Security and privacy notes

The cascade holds no credentials and reaches no network beyond the configured local endpoint. Task
input passes through the ledger only as metadata and token counts, not as content.

## Observability and debugging

- `local-offload doctor` — endpoint health and per-alias reachability.
- `local-offload models` — the resolved Tier routing table.
- `local-offload ledger --since N` — savings accounting.
- The ledger's per-entry metadata (`escalations`, `margin`, `grounded`, `err_class`, `reasoning`) is
  the primary debugging signal for "why did this defer?"

## Testing notes

`internal/pipeline/` carries focused suites per concern: `runtier_test.go` (the no-side-effect
invariant), `pipeline_reasoning_test.go`, `pipeline_confhead_test.go`, `knn_prefilter_test.go`
(entry-tier selection), plus per-task defer tests. `internal/grounding/` and `internal/ledger/` have
their own unit tests.

## Common pitfalls

- **Treating a defer as a bug.** It is the designed outcome when confidence is low.
- **Expecting grounding to gate summaries.** It is logged for summaries, actioned only for extract.
- Assuming escalation happens on any failure — infrastructure failures deliberately do not escalate.
- Assuming `Reasoning` means a different model. It is the same model as the escalation Tier; the flag
  is what tells them apart.
- Reading logprobs under an active grammar as if they were unconstrained. They are pre-mask.

## Source map

- [`internal/pipeline/pipeline.go`](../../internal/pipeline/pipeline.go) — chain construction, the
  walk, gates, reasoning tier
- [`internal/pipeline/recordless.go`](../../internal/pipeline/recordless.go) — the nil-store
  construction
- [`internal/core/types.go`](../../internal/core/types.go) — `Result`, `Meta`, `Deferf`
- [`internal/grounding/grounding.go`](../../internal/grounding/grounding.go)
- [`internal/ledger/ledger.go`](../../internal/ledger/ledger.go)
- [`internal/config/config.go`](../../internal/config/config.go) — tier aliases and threshold defaults

## Related docs

- [../architecture/decisions/0001-defer-never-cloud-fallback.md](../architecture/decisions/0001-defer-never-cloud-fallback.md)
- [../architecture/decisions/0002-grammar-reliable-serving-flags.md](../architecture/decisions/0002-grammar-reliable-serving-flags.md)
- [../architecture/decisions/0010-tier-optimization-before-latency-defer.md](../architecture/decisions/0010-tier-optimization-before-latency-defer.md)
- [../glossary.md](../glossary.md)
