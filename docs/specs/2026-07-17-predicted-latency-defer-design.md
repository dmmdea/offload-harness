# Predicted-latency defer

**Status:** proposal — for decision
**Date:** 2026-07-17
**Author:** afelopez
**Decides:** whether the harness gains a fourth defer path — refusing work it *can* do correctly but
not *fast enough* — and how it predicts "fast enough" without a model call.
**Follows from:** D3 in `docs/specs/2026-07-17-hardware-scope-linux-and-low-end-design.md`.

---

## 1. The problem

The harness's contract is "a verified result, or a structured defer". Today "can't" means one of
three things:

| Existing defer | Fires when |
|---|---|
| Low confidence | the class-mass margin at the decision token is below threshold |
| Truncation | output hit the token limit — `verifier.Check` marks it `Terminal` |
| Infra health | a tier is OOMing, timing out, or its breaker is open |

None of them means **"this will take 39 seconds and you wanted it in two."**

That gap is not theoretical. Measured on config #12 (`amd-gcn` — Ryzen 5500U / Radeon Vega), driven
through the real CLI against a real llama.cpp server:

| Task | Input | Wall clock | Deferred? | Correct? |
|---|---|---|---|---|
| `classify` | 500 chars | 3 s | no | yes |
| `triage` | 19,162 chars | **38 s** | no | yes |
| `summarize` | 24,000 chars | **55 s** | no | yes |

**0 of 6 calls deferred and every answer was correct.** The model is slow but *right*, so no existing
defer path fires, and the harness spends a minute on work the caller wanted in two seconds. The
caller has no way to express that it wanted it in two seconds, and the harness has no way to notice
it can't.

These numbers were re-measured with the desktop quiesced and did not move — the bottleneck is the
silicon, not the workload on it. See the hardware spec, §3.3.

## 2. The precedent that makes this uncontroversial

The harness **already has a pre-flight economic gate**. In `pipeline.Run`:

```go
if contextbudget.IsTrivial(req.Input) {
    return core.Deferf("input too small to offload", "", meta)
}
```

That is not a defer about doubt. It is a defer about **cost/benefit**: the input is so small that
offloading isn't worth the round trip, so the harness refuses before spending anything.

This proposal is that gate's mirror image. `IsTrivial` refuses at the **bottom** end ("too small to
be worth it"); a latency gate refuses at the **top** end ("too slow to be worth it"). Same category
of judgement, same point in the pipeline, same shape:

```
        too small                    just right                    too slow
    ──────────────────┼──────────────────────────────────┼──────────────────────
      IsTrivial()                  the harness runs           NEW: latency gate
      (exists today)                                          (this proposal)
```

The design adds no new *kind* of decision. It completes one the codebase already makes.

## 3. Design

### 3.1 Budget ownership — config default, per-request override

```jsonc
// config.json
{ "max_latency_ms": 8000 }   // 0 = off (the repo's convention: see
                             // confidence_margin_threshold, exemplar_shots)
```

```jsonc
// an MCP call may override it
{ "text": "...", "question": "...", "max_latency_ms": 2000 }
```

Resolution order: `req.Params["max_latency_ms"]` → `cfg.MaxLatencyMs` → `0` (no gate).

**Why both.** Only the caller knows whether it is interactive, so a caller-supplied budget is the
conceptually correct source. But agents rarely pass optional arguments, so a caller-only design
would almost never fire. A config default makes the gate real on a slow machine even when nothing
passes the argument; the override lets a caller that *does* know say so.

**Why `0` = off.** It matches `confidence_margin_threshold: 0` and `exemplar_shots: 0`. Upgrading
changes no behaviour until an operator opts in.

### 3.2 The predictor — a linear fit over the ledger

The ledger already records, per call: `task`, `model_tier`, `input_chars`, `latency_ms`. That is
exactly the training set. For each `(task, tier)` pair, fit by least squares:

```
latency_ms ≈ A + B · input_chars
```

**Why linear is defensible here — and would not be elsewhere.** `B · input_chars` is prompt
processing, which *is* linear in tokens, and chars→tokens is near-linear. `A` absorbs generation,
which is roughly constant per task **because the outputs are grammar-bounded**: a label, a yes/no, N
bullets. In an open-ended chat this model would be worthless, because output length would dominate
and would be unknowable in advance. In *this* harness the GBNF grammar caps the output, which is
precisely what makes a one-line model honest.

This follows the flywheel's existing grain: offline, inference-free, pure Go statistics over the
ledger, spending zero cloud tokens — the same thing `calibrate`, `health` and `train-router` already
do.

### 3.3 Chain accounting — worst case over the tiers that exist

A request's latency is not one number: if the confidence gate escalates, it pays for two tiers.

The budget is compared against the **worst case**: the sum of the predictions for every tier in the
chain `modelChain()` returns.

```
budget = 8000 ms
pred(gemma4-e2b)     =  6000 ms
pred(gemma4-26b-a4b) = 36000 ms
worst case           = 42000 ms  >  8000  →  defer, before spending anything
```

**Why worst case and not expected value.** A budget honoured "on average" is not a promise. The
alternative — predicting only the entry tier, or weighting escalation by its historical probability
— accepts the call, runs 6 seconds, escalates, and returns at 45. That is the worst outcome
available: the caller pays the latency *and* gets nothing, because it still has to do the work
itself. An upfront defer costs 0 ms. If the harness accepts, it keeps its word.

The price is real and should be stated plainly: **the gate is conservative and will refuse work it
would usually have completed in time.** When most calls don't escalate, worst-case accounting turns
some of them away. That is the cost of never breaking the promise.

**Why "the tiers that exist" matters.** Summing over whatever `modelChain()` returns needs no
special case for weak hardware. On config #12 the config is `triage_model = model = gemma4-e2b` and
`escalation_model = ""`, and `add()` de-duplicates, so the chain is `[gemma4-e2b]` — a single entry.
Worst case = that one prediction. The same code is exact on a `blackwell-72` box with three distinct
tiers.

### 3.4 Scope — the text cascade only

Gated: **`summarize`, `classify`, `extract`, `triage`.**

Not gated (unchanged): `vqa`, `ocr`, `extract_image`, `assess_image`, `video_describe`,
`transcribe`, `generate_*`, `edit_image`, `media`, `nim`, `agent_run`.

Two reasons. First, the linear-in-`input_chars` model only describes the text tasks; vision costs
scale with pixels, STT with audio duration, generation with diffusion steps. Second, and decisively,
**media generation is slow by design** — `offload_generate_video`'s own description says "the NATIVE
recipe is the default (tens of minutes)". A latency budget there would defer everything, which
inverts the feature's entire purpose.

## 4. Components

| Unit | Responsibility | Depends on |
|---|---|---|
| **`internal/latency`** (new) | `Fit(entries) map[key]Coeffs`, `Predict(task, tier, chars) (ms, ok)`, `PredictChain(task, tiers, chars) (ms, ok)`. Pure Go, no I/O, no deps. | `internal/ledger` (the `Entry` type only) |
| **`health`** (extended) | Also fits the latency model in its existing ledger scan and writes `latency_model.json` | `internal/latency` |
| **`pipeline`** (hooked) | Resolves the budget, predicts the chain, defers before spending | `internal/latency` |
| **`config`** | `max_latency_ms`, `latency_model_path` | — |
| **`mcpserver`** | optional `max_latency_ms` arg on the four text tools | — |

### 4.1 Pipeline integration

Immediately after the existing economic gate, so the two sit together:

```go
if contextbudget.IsTrivial(req.Input) {
    return core.Deferf("input too small to offload", "", meta)   // exists: refuse from below
}
req.Input, _ = contextbudget.Trim(req.Input, p.cfg.MaxInputChars)
meta.Feat = featurize(req.Task, req.Input)

// NEW: refuse from above.
if budget := p.latencyBudget(req); budget > 0 {
    chain := p.modelChain(req.Task, meta.Feat, false)
    if pred, ok := p.latencySnap().PredictChain(string(req.Task), chain, len(req.Input)); ok && pred > budget {
        return core.Deferf(fmt.Sprintf("predicted %.0fs exceeds the %.0fs budget",
            pred/1000, budget/1000), "", meta)
    }
}
```

Placement is load-bearing:

- **After `Trim`** — predict on the text that will actually be sent, not the text that arrived.
- **After `featurize`** — `modelChain()` needs `feat` for the learned router's `skipSmallEntry`.
- **Before `tasks.Build`** — nothing has been spent yet.

Loaded via a snapshot accessor (`latencySnap()`) mirroring the existing `routerSnap()` /
`overridesSnap()` / `knnSnap()` pattern, so a hot reload can swap the model without a lock on the
request path.

### 4.2 Data flow

```
  OFFLINE (health, no inference, no cloud)          REQUEST PATH (no model call)
  ────────────────────────────────────────          ────────────────────────────
  ledger.jsonl                                      req.Input, req.Params
    │  (task, tier, input_chars, latency_ms)          │
    ▼                                                 ▼
  latency.Fit  ──►  latency_model.json  ──────────► latencySnap().PredictChain
    per (task,tier): A + B·chars                      │  worst case over modelChain()
                                                      ▼
                                              pred > budget ?
                                               ├── yes → Deferf("predicted 39s …")
                                               └── no  → the cascade, as today
```

## 5. Failure behaviour

**Fail open, always.** The gate can only ever *add* a defer; it must never be the reason a healthy
request breaks.

| Condition | Behaviour |
|---|---|
| `max_latency_ms` unset / 0 | no gate — byte-for-byte today's behaviour |
| No `latency_model.json` (fresh install) | `ok=false` → no gate; the ledger accrues and the model appears after the first `health` run |
| A tier in the chain has no fit, or fewer than `minSamples` (proposed: 20) | `ok=false` → no gate for that request |
| Model file corrupt / unparseable | warn to stderr, no gate |
| Prediction succeeds and fits the budget | run the cascade unchanged |

This mirrors the codebase's existing stance — `knnPreferLargerEntry` is explicitly "fail-open: any
miss => false", and `routerSnap()` is nil-safe.

## 6. What this gives the project for free

Latency defers are recorded like any other, via `recordDefer`, carrying their `reason` in the field
LO-8 already added to the ledger. That means `stats` and `ledger` can then report:

> *40% of your calls deferred on latency.*

That is a **diagnostic the project does not currently have**, and it is exactly the signal that would
have made the hardware spec unnecessary: a machine where the harness cannot pay its way says so, in
the operator's own ledger, instead of silently taking a minute per call. The hardware profile matrix
classifies what *fits*; nothing today measures what it *costs in time*. This closes that axis.

## 7. Testing

`internal/latency` is pure and needs no server:

- `Fit`: synthetic points on an exact line recover `A` and `B`; noisy points land within tolerance;
  a group below `minSamples` yields no usable coefficients.
- `Predict`: interpolates a known fit; returns `ok=false` for an unknown `(task, tier)`.
- `PredictChain`: sums a multi-tier chain; a **single-tier chain returns that tier's prediction**
  (the config #12 case); an unknown tier anywhere in the chain returns `ok=false` (fail open —
  never guess a chain you can't fully price).

`pipeline`, table-driven against the existing `httptest` fake upstream:

- budget exceeded → deferred, with the predicted and budget figures in `reason`, and **no HTTP call
  made** (assert the fake was never hit — the whole point is not spending the time)
- budget met → runs, result unchanged
- `max_latency_ms: 0` → no gate; today's behaviour preserved exactly
- non-text task with a budget set → never gated
- no model / cold start → no gate

`health`: its report gains the fit; existing `tier_overrides.json` output is unchanged.

## 8. Risks and limitations

These are real and stated rather than hidden.

1. **Model-swap cost is invisible to `input_chars`.** llama-swap evicts and loads models; escalating
   to a cold 26B costs seconds that have nothing to do with input size. The fit sees that only as
   variance in `A`, and *bimodally* (resident vs not). **Predicting a non-resident tier
   under-predicts.** A later refinement could condition on the resident tier — out of scope here.
2. **GPU contention is invisible too.** If a `generate_video` holds the `gpulock`, real latency
   explodes and the prediction lies. `gpulock` exists and the predictor does not consult it.
3. **Conservatism is by construction** (§3.3). Worst-case accounting refuses work that would usually
   have finished in time. If that proves too blunt in practice, the honest fix is a better predictor,
   not a softer promise.
4. **Fit quality is unvalidated.** Nothing here checks R² or rejects a bad fit. A machine with
   bimodal latency (see 1) could get a confidently wrong line. Proposed mitigation: record `N` and
   the residual in `latency_model.json` so a human can see it; refuse the fit below `minSamples`.
5. **`health` needs a signature change.** `health.Run(ledgerPath, outPath)` takes one output path,
   and `groupByTier` groups by tier only — the fit needs `(task, tier)`. Extending it means a new
   parameter and a second grouping in the same scan. It is `internal/`, so no external consumers
   break, but it is not a free reuse. See §9.

## 9. Alternatives considered

| Alternative | Why not |
|---|---|
| **Static throughput in config** (`pp_tok_per_s`, `tg_tok_per_s`) | Deterministic and trivial to test, but it asks the operator to measure and to keep the numbers current — and this repo's own profiles have sat marked `PROJECTED` for months. A self-calibrating fit asks nothing of anyone. |
| **Expected-value accounting** (`pred(entry) + P(esc)·pred(esc)`) | Accepts far more work, and `escalations` is already in the ledger so `P` is measurable — but a budget honoured on average is not a promise. Rejected in favour of a contract that holds. |
| **Entry-tier-only prediction** | Simplest, and dishonest: promises 6 s, escalates, returns at 45. |
| **Re-check the budget before each escalation** | Elegant, and avoids the conservatism of §3.3 — but a mid-cascade defer is the worst outcome for the caller: it pays 6 s *and* still does the work itself. An upfront defer costs nothing. |
| **A separate `fit-latency` subcommand** → `latency_model.json` | Matches the existing one-job-one-file pattern (`train-router` → `router-weights.json`) and avoids the §8.5 signature change. Rejected only because `health` already scans the same ledger for the same purpose (per-tier timing) and a second pass buys nothing. **Reasonable to overrule.** |
| **Trim the input until it fits the budget** | Silently degrades quality to hit a number. The harness's whole value is that it never silently degrades. |

## 10. Open questions for the meeting

1. **`health` or a new job?** (§8.5, §9) — the signature change is small but real.
2. **What is a sane default `max_latency_ms`?** The proposal ships `0` (off), so the feature is inert
   until an operator opts in. Should a *profile* seed it — e.g. `amd-gcn` seeding `8000` via the
   `config_seed` mechanism the Blackwell tiers already use? That would make the gate arrive already
   useful on exactly the hardware that needs it.
3. **Should `doctor` surface the predicted latency** for a nominal input, so an operator sees
   "triage of 20k chars ≈ 38 s on this box" at install time instead of discovering it in production?
   This is cheap and pairs naturally with D2's "single-tier: no escalation available" warning.
4. **`minSamples = 20`** — a guess. `calibrate` already has a considered stance on sample sizes for
   conformal thresholds; this should probably borrow it rather than invent one.
