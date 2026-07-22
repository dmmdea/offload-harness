# Cascade escalation and defer

## Purpose

The path every text offload task takes: which model answers, what makes the harness try a bigger one,
and what happens when none of them are good enough. This is where the harness decides between
answering and handing the work back.

## Trigger

Any offload task — `summarize`, `classify`, `extract`, `triage` — arriving via the CLI, an MCP tool,
or the coding agent's in-process tools.

## Participants

The pipeline, the configured model Tiers served by llama-swap, the grammar compiler
(`internal/gbnf`), the grounding checker, the confidence estimator, and the ledger.

## Step-by-step flow

1. **Build the chain.** `[triage_model?] → model → escalation_model`. The triage Tier is included only
   for `triage` and `classify`, and may be skipped by the entry-tier router. Duplicates collapse,
   Tiers with an open circuit breaker are skipped, and a fully pruned chain falls back to the
   workhorse model alone.

2. **Compile the grammar.** The task's JSON Schema becomes GBNF, passed as a raw `grammar` field on
   the completion request — never `--json-schema` or `response_format`
   ([ADR 0002](../architecture/decisions/0002-grammar-reliable-serving-flags.md)).

3. **Attempt the current Tier**, then run the gates in order:
   - **Schema validation.** Failure → retry/escalate.
   - **Grounding.** Computed for all tasks, logged always, but *actioned only for extract* —
     summaries legitimately paraphrase.
   - **Confidence.** Classify below `classify_min_confidence` (default 0.45) escalates; a decision
     margin below the task's threshold (learned conformal value, else `confidence_margin_threshold`,
     default 0.35) escalates.
   - **Confhead.** A learned correctness estimate below its threshold escalates.

4. **Branch.** OK → return immediately. Recoverable failure at a non-final Tier → escalate to the
   next. Infrastructure failure (connection refused, timeout, 5xx, OOM) → **defer without
   escalating**, with `err_class` set, because a bigger model against a broken endpoint fails
   identically.

5. **Terminal reasoning attempt.** Once the chain is exhausted, grammar tasks whose output was not
   truncated get one final attempt on the reasoning Tier, with a thinking span supplied by the grammar
   and an extra token budget. It runs once, skips the confidence gate, and is marked
   `Reasoning: true`. Under the shipped default it runs on the same model as the escalation Tier
   (a config may bind them apart — the ≥16GB matrix recommendation does); the flag is what
   distinguishes them in the ledger.

6. **Defer.** If that also fails, build a Defer carrying the last reason, any partial output, and the
   accumulated metadata, and record it.

## Data and state changes

One ledger line per completed call or defer, `fsync`ed. Cache writes on success. Circuit-breaker
counters update on infrastructure failures. **None of this happens on the recordless path** used by
the coding agent — nil cache, nil ledger, no shadow capture.

## Success behavior

A validated, schema-conforming result with metadata describing how it was reached: which model,
how many escalations, the decision margin, whether it was grounded, and whether the reasoning Tier
produced it.

## Failure behavior

A Defer — which is a **success shape**, not an error. `{"deferred": true, "reason": ...}` plus any
partial output. The caller does the task itself. Nothing escalates to a cloud model, ever
([ADR 0001](../architecture/decisions/0001-defer-never-cloud-fallback.md)).

Genuine errors are distinct from defers and carry `err_class`.

## External dependencies

The local completion endpoint serving the configured aliases. Nothing else — no network egress, no
credentials.

## Invariants and assumptions

1. A Defer is a valid outcome; never convert one to an error, and never add a fallback to avoid one.
2. Infrastructure failures do not escalate.
3. The reasoning Tier never fabricates a pass — garbage from it still defers.
4. Grounding gates extract only.
5. The recordless path writes nothing.

## Security and privacy notes

Task content does not enter the ledger; only metadata and token counts do.

## Observability and debugging

The ledger entry answers "why did this defer?" — read `reason`, `err_class`, `escalations`, `margin`,
and `grounded` together. A defer with `err_class` set is an infrastructure problem, not a quality
problem, and the two have completely different remedies.

When defers rise on a given machine, the first question is whether that machine's Tier binding is
right — model, quantization, profile, serving flags — not whether the thresholds should move
([ADR 0010](../architecture/decisions/0010-tier-optimization-before-latency-defer.md)).

## Testing notes

`internal/pipeline/pipeline_reasoning_test.go` (including that garbage output still defers),
`pipeline_confhead_test.go`, `knn_prefilter_test.go`, `runtier_test.go` for the no-side-effect
invariant, and per-task defer suites.

## Source map

- [`internal/pipeline/pipeline.go`](../../internal/pipeline/pipeline.go) — chain, gates, reasoning
  tier, defer sites
- [`internal/core/types.go`](../../internal/core/types.go) — `Result`, `Meta`, `Deferf`
- [`internal/grounding/grounding.go`](../../internal/grounding/grounding.go)
- [`internal/confidence/confidence.go`](../../internal/confidence/confidence.go)
- [`internal/ledger/ledger.go`](../../internal/ledger/ledger.go)

## Related docs

- [../systems/offload-pipeline.md](../systems/offload-pipeline.md)
- [../architecture/decisions/0001-defer-never-cloud-fallback.md](../architecture/decisions/0001-defer-never-cloud-fallback.md)
- [../glossary.md](../glossary.md)
