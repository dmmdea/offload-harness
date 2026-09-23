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

1b. **Guard the chain** (`internal/seatguard`, on by default). This step applies only while a
   seat named in `vllm_seats` is loaded on this box. Every rung, and the terminal reasoning Tier,
   is checked against llama-swap's own routing (read from `serving_config_path`). A rung whose
   load would unload the seat keeps its model and rides its `cascade_remote_lanes` lane when the
   lane serves it. Otherwise the loaded seat replaces it as the rung (a D-129 vLLM rung). A
   protected reasoning Tier with no lane runs its attempt on the loaded seat (step 5's shape, with
   the seat's `structured_outputs` instead of the think-wrapped grammar), unless that seat already
   answered the call. An unknown reading fails toward the seat.
   With no vLLM seat loaded, the chain is untouched. See
   [../systems/offload-pipeline.md](../systems/offload-pipeline.md).

2. **Compile the grammar.** The task's JSON Schema becomes GBNF, passed as a raw `grammar` field on
   the completion request — never `--json-schema` or `response_format`
   ([ADR 0002](../architecture/decisions/0002-grammar-reliable-serving-flags.md)).

3. **Attempt the current Tier**, then run the gates in order:
   - **Schema validation.** Failure → retry/escalate.
   - **Grounding.** Computed for all tasks, logged always, but *actioned only for extract* —
     summaries legitimately paraphrase.
   - **Confidence.** Classify below `classify_min_confidence` (default 0.88) escalates; a decision
     margin below the task's threshold (learned conformal value, else `confidence_margin_threshold`,
     default 0.65) escalates. (Defaults calibrated 2026-08-14 — the prior 0.45/0.35 sat below the
     entire observed support and never fired.)
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
how many escalations, **which gate caused the first escalation** (`esc_source`), the decision margin,
whether it was grounded, and whether the reasoning Tier produced it.

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
6. While a declared vLLM seat is loaded and `cascade_seat_guard` is on, no rung whose load would
   evict it is sent to this box's llama-swap. That includes the window probe of a climb, whose
   `/upstream/<rung>/props` read would load the rung.

## Security and privacy notes

Task content does not enter the ledger; only metadata and token counts do.

## Observability and debugging

The ledger entry answers "why did this defer?" — read `reason`, `err_class`, `escalations`,
`esc_source`, `margin`, and `grounded` together. A defer with `err_class` set is an infrastructure
problem, not a quality problem, and the two have completely different remedies.

The **escalating attempt writes its own row** (register D-127): a call the seat answered and the
confidence gate sent up lands in the ledger as a non-deferred row on ITS tier, with ITS margin and the
`esc_source` that fired, before the defer that climbs — so a margin escalation is one row per attempt
(entry tier: `escalations: 0`, `margin` set; successor: `escalations: 1`, the carried `esc_source`).
Until this the defer returned before the record and the only trace of a margin escalation was the
successor's row and the labels sidecar (4 `esc_source` rows in 9,217 for 43 real firings).

`esc_source` names WHICH gate sent a call up a tier, and it is the one field written on **successful**
escalations too — that was the measured gap it closed: a call that escalated and then succeeded
recorded no reason at all, so "which gate fired" was unreadable from telemetry and no change to the
gating could be evaluated after the fact. It is a **closed seven-value set**
(`internal/core/types.go`), which is what makes it groupable where the free-text `reason` is not:
`self_confidence` (the model's own `confidence` field fell below `classify_min_confidence` — the only
self-declared gate in the cascade), and the six structural ones — `margin` (logprob decision margin
below the per-task threshold), `confhead` (the learned p(correct) head below its conformal
threshold), `schema` (parsed but failed validation), `grounding` (extract produced values absent from
the source), `verifier` (malformed or truncated output), and `retries` (exhausted without a valid
answer). The gates are evaluated verifier → schema → grounding → self-confidence → margin →
confhead, and the FIRST to fire is recorded, so the values are mutually exclusive per call; a call
that never escalated carries the empty `EscNone`. Across tiers it is carried only-if-unset, so it
keeps naming the gate that started the climb rather than the last one to agree.

The field is **omitted entirely on non-escalating rows** (`omitempty` — the append-only JSONL's
small-line atomicity is load-bearing), and rows written before the field was added still parse (an
absent field decodes to empty). See `internal/ledger/ledger.go`.

When defers rise on a given machine, the first question is whether that machine's Tier binding is
right — model, quantization, profile, serving flags — not whether the thresholds should move
([ADR 0010](../architecture/decisions/0010-tier-optimization-before-latency-defer.md)).

## Testing notes

`internal/pipeline/seatguard_test.go` for step 1b (a loaded seat, the no-seat control, the two
unknown readings, the no-lane case served by the seat, the lane, the climb's window probe and the
reasoning Tier), `internal/pipeline/pipeline_reasoning_test.go` (including that garbage output still defers),
`pipeline_confhead_test.go`, `knn_prefilter_test.go`, `runtier_test.go` for the no-side-effect
invariant, `escsource_test.go` for the `esc_source` stamping and its only-if-unset carry across
tiers, and per-task defer suites.

## Source map

- [`internal/pipeline/pipeline.go`](../../internal/pipeline/pipeline.go) — chain, gates, reasoning
  tier, defer sites
- [`internal/pipeline/seatguard.go`](../../internal/pipeline/seatguard.go) — step 1b, the seat
  guard over one call's chain
- [`internal/seatguard/guard.go`](../../internal/seatguard/guard.go) and
  [`internal/seatguard/residency.go`](../../internal/seatguard/residency.go) — the readings, the
  verdict, and llama-swap's routing solver
- [`internal/core/types.go`](../../internal/core/types.go) — `Result`, `Meta`, `EscalationSource`,
  `Deferf`
- [`internal/grounding/grounding.go`](../../internal/grounding/grounding.go)
- [`internal/confidence/confidence.go`](../../internal/confidence/confidence.go)
- [`internal/ledger/ledger.go`](../../internal/ledger/ledger.go)

## Related docs

- [../systems/offload-pipeline.md](../systems/offload-pipeline.md)
- [../architecture/decisions/0001-defer-never-cloud-fallback.md](../architecture/decisions/0001-defer-never-cloud-fallback.md)
- [../glossary.md](../glossary.md)
