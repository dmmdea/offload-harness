---
status: Accepted
date: "2026-07-24"
---

# KV reuse is append-only; the bench measures it and fails closed

## Context

OmniRoute-harvest Phase D asked us to "verify the monotonic compaction ladder preserves llama.cpp's
KV prefix cache". Before building anything we probed the live stack, and the premise did not
survive contact with it.

**Measured on gemma-4-e4b (llama.cpp b50, `-c 8192`, flash-attn on, `--cache-reuse` unset,
2026-07-24):**

| probe | cache_n | prompt_n | prompt_ms | wall_ms |
|---|---|---|---|---|
| cold first request | 0 | 1769 | 465 | 6836 |
| byte-identical resend | 1768 | 1 | 38 | 53 |
| append 119 tokens to a 2321-token prompt | 2316 | 119 | 83 | 96 |
| strict prefix of the cached prompt | 2316 | 5 | 80 | 95 |
| **one line edited at 20% of the prompt** | **1** | 2319 | 558 | 609 |
| **one line edited at 98% of the prompt** | **0** | 2662 | 656 | — |
| identical resend AFTER one `/v1/embeddings` call | 0 | 2320 | 582 | 6873 |

Three facts follow, and they reshape the phase:

1. **Reuse is BINARY, not proportional.** Appends and truncations reuse everything; *any* edit
   discards the *entire* cache. A position sweep (edit at 4/17/33/50/62/75/83/92/98% of a 120-line
   prompt) returned `cache_n` = 1,1,1,1,1,0,0,0,0 — an edit two lines from the end costs exactly as
   much as an edit at the start. Our first hypothesis (a sliding-window horizon, so near-the-end
   edits would survive) is **falsified** by that sweep and is recorded here so it is not re-derived.
2. **Therefore the ladder cannot be KV-friendly by construction.** Every lossy rung edits from
   `protectedEnd` forward (`internal/agent/compaction.go`: dedupe, GCF, skeleton, elide and drop all
   iterate `for i := protectedEnd; i < recentStart`), so a firing step always pays a full re-prefill.
   What monotonicity + the idempotence guards genuinely buy is that steps BETWEEN fires stay pure
   appends, so the cache returns immediately after a fire.
3. **Cache lifetime is a property of the SCHEDULER, not of our code.** A single `/v1/embeddings`
   request evicted the tier and zeroed the cache; a byte-identical resend then cost a 6.9s reload.
   Any measurement that does not bracket itself with controls will attribute that to compaction.

## Decision

1. **The client captures the server's own accounting, additively.** `wireResp` gains optional
   `usage`/`timings`; `Completion.Serve` is a `*ServeStats` that is **nil when the backend reports
   nothing**, so "unmeasured" can never be read as "measured zero". The request body is unchanged
   and pinned by a test — instrumentation must not perturb the thing it measures.
2. **A new `compaction-eval kvbench` mode**, never inside `run`/`freeze`/`check`. Those are
   deterministic, model-free, and ratchet-guarded on integer token counts; wall-clock inputs would
   flap the ±2% band. New mode, new report type, new artifact.
3. **Controls bracket every run and the bench FAILS CLOSED.** A positive control (byte-identical
   resend, must reuse ≥90%) before and after, plus a negative control (unrelated prompt, must reuse
   ≤5%). Any failure ⇒ verdict `INCONCLUSIVE`, non-zero exit, raw samples still emitted as evidence
   — never a headline number. The append and mid-edit controls do not gate; they are measured
   properties that tell the reader how to interpret the body.
4. **Arms run in BLOCKS, never interleaved.** The tier serves `--parallel 1` — one KV slot — so
   round-robin interleaving (the textbook drift control) would make *every* request a cache miss.
   Blocks confound drift with arm; rounds are repeated and per-round values reported instead.
5. **The size effect and the cache effect are reported separately and never summed.** A compacted
   transcript is shorter, so it prefills faster at zero reuse; folding that into one "compaction
   speedup" would confirm the KV hypothesis with evidence entirely explained by size.
6. **The safety margin is tuned from estimator calibration, not from the replay.** `compactionMargin`
   lives in `Loop.inputBudget()`, while corpus replay derives its budget from `entryParams` (60% of
   the entry's own estimate) — the replay never exercises `inputBudget()` at all. The bench therefore
   reports real-vs-estimated token ratios per content kind plus a decision TABLE
   (`S ≥ (C − M)(1 − 1/r)`), and makes no recommendation: raising the margin buys safety by
   unconditionally shrinking usable context on every request, which is the operator's trade.

## Consequences

- The harvest's KV-prefix rationale for the ladder is **corrected in the record**: compaction is
  justified by the size win and by requests completing at all, not by cache friendliness. The
  alternative to a fire is not a warm cache — it is the transcript growing until the server rejects
  it (measured live as `exceed_context_size` 400s).
- A cheap, real optimisation is now visible and is NOT taken here (out of scope, needs its own
  change): fires are full re-prefills, so anything that reduces fire COUNT — a larger effective
  window, or compacting in bigger, rarer steps — buys more than making each fire gentler.
- The bench is live-only by nature; its CI-safe half (metric math, admissibility, relation
  classification, sequencing against a serving simulator that encodes the measured binary rule) runs
  with no GPU and no network.
- the <node-a> laptop is **UNMEASURED**, not projected: it runs 0.22.22, which predates the
  `compaction-eval` verb entirely, and updating an operator's working laptop across the 0.22.25
  default flip is its own decision, never a silent prerequisite of a measurement.
