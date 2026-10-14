---
status: Proposed
date: "2026-10-14"
---

# 0091 — Agent-trace corpus readout: pager re-fetch, compaction-fire and prefill

Release: unreleased (RSI-OH-2)

## Context

Two questions stand in front of the context-language-model measurement. How often does the compaction ladder actually
run in real agent runs, so that a measurement of it has something to measure? And is the pager re-fetch rate (distinct
evicted payloads fetched back, over distinct payloads evicted) above the 10 % gate of R2-13, below which the pager
family closes and above which an `edit_context` tool would be worth building?

Neither could be read from anything on disk. The agent loop measured both (the pager report, and now the pass count of
the ladder) and the two records that outlive a run dropped them: the standalone per-goal trace file carried no pager or
prefill object, and the wire result the delegator stores in the delegation log carried no pager object. The pager
family therefore sat at `insufficient_data`, and the compaction items had no baseline.

A readout over those records needs three rules, each learned from reading the real log:

- **A missing measurement is not a zero.** A run recorded before a field existed has no value for it. Averaging that
  as 0 would close the pager family on no evidence.
- **A logged row is not a run.** A deferred subtask (capacity, infrastructure, budget, config, contract) is written to
  the delegation log with zero steps and nothing else. The first harvest counted those as runs: 24 % of its rows, which
  inflated every unrecorded count and would have kept the pager's unrecorded count above zero after the recording
  shipped, because such a row never reaches the loop that writes the pager report.
- **A seat alias is not a seat.** A pool placement logs the pool's alias as the seat, and neither the row nor its result
  holds the node's real seat, so a per-seat reading cannot reach those runs.

## Decision

1. **`local-offload trace-corpus` is the standing readout** for these questions, shaped like `compaction-eval`
   (`harvest`, `readout`, `freeze`, `check`) and kept apart from it. It reads recorded agent runs, from the standalone
   agent traces and from the delegation log, and never the ledger. `readout`, `freeze` and `check` are deterministic and
   model-free, the report has no timestamps, and `check` demands a frozen report back byte for byte.
2. **Every rate is null when nothing was recorded**, with the count of unrecorded runs beside it. A pager object or a
   pass count that is absent or null is unrecorded, never a recorded zero; an object that contradicts itself is read as
   unrecorded and noted. A run that observed prefill steps but reported no token counts (an engine without prefill
   timings) is counted apart and adds nothing to the token sums.
3. **A delegation row is a run only if an agent executed.** A row with zero steps and no step trace, no prefill or
   seat-token accounting and no pager object is skipped under the named reason `delegation row never ran (zero steps,
   nothing recorded)` and counted in the harvest stats. Any record at all keeps the row a run.
4. **Seats are named only when the record names one.** `by_seat` holds only those runs. Runs on a pool alias are their own
   bucket (`pool_alias`, and `pool_alias_by_node` keyed by the node id the row carries), and an `attribution` block gives
   the three classes and the unattributed share, so a reader sees how much of the corpus a per-seat reading cannot reach.
5. **Future runs record what the readout needs.** `PagerReport` counts compaction passes; the standalone trace file
   carries the pager and prefill objects of that goal alone (the loop's instruments are reset between goals); the agent
   wire result carries a pager object, present on every run that reached the loop (zeros included) and absent otherwise.
6. **The corpus is machine-local and redacted at harvest.** The corpus keeps numbers and enums only, redacts its few
   strings with the same redactor and vet table as `compaction-eval`, and replaces a seat or stop reason that is a local
   path with a stable placeholder. This ADR carries aggregates only.

## First readout

Harvested on 2026-10-14 from one machine's delegation log (51 daily shards, 10,527 rows). That machine holds no
standalone agent trace files, so every figure below is from delegation-log rows. These numbers are a snapshot of a log
that is still growing.

| Reading | Value |
|---|---|
| Runs executed | 7,049 |
| Rows skipped | 2,231 never ran an agent, 1,003 without a result (dispatch error), 22 capacity refusals, 222 on the harness's own test seat |
| Trace-bearing rows (a step trace) | 5,617 (79.7 % of runs), 21,372 steps in all |
| Pager: runs that recorded a report | 0 of 7,049 (unrecorded 7,049) |
| Pager re-fetch rate against the 10 % gate | null, verdict `NO_DATA` (0 measured runs) |
| Compaction: runs that recorded a pass count | 0 of 7,049, so the fire rate is null |
| Compaction: transcript lower bound | null (no agent trace file on the machine) |
| Prefill: measured, no token counts, unmeasured | 3,764 (53.4 %), 3,188 (45.2 %), 97 (1.4 %) |
| Prefill: KV reuse over the measured runs | 52.4 % of prompt tokens |
| Prefill: tokens per step over the measured runs | 2,515 prefilled (10,087 steps observed) |
| Prefill: prefilled tokens per measured run | p50 3,547, p90 14,954, max 247,847 (3,764 runs) |
| Mean prompt size per step, per run (seat input tokens over steps) | p50 3,603, p90 12,843, max 86,374 (5,403 runs) |
| Runs a seat can be named for | 4,983 (70.7 %) |
| Runs on a pool alias (cannot be attributed to a seat) | 2,066 (29.3 %), all from one node, `<node-b>` (tier `blackwell-3x16`) |
| Runs whose record names no seat | 0 |

## What the numbers decide

A recommendation for the human decision on the context-language-model measurement, not a ruling.

1. **The compaction-fire question is unanswered, not answered "rarely".** No run in the corpus recorded a pass count and
   no trace file exists to give the transcript lower bound. The measurement should not take a fire rate from this
   corpus. It should wait for instrumented runs, or build its own set of long runs.
2. **The re-fetch question is unanswered, so an `edit_context` tool should be neither built nor shelved on this
   readout.** The gate reads `NO_DATA`; no run recorded a pager report. The pager family stays open, not closed by a
   zero.
3. **The one compaction-relevant reading available is a proxy and points at long runs.** The ladder runs only when a
   transcript passes the seat's input budget (its context window less the output cap and a margin). Across 5,403 runs the
   mean prompt size per step has a median of 3,603 tokens and a 90th percentile of 12,843. A mean over a run's steps
   understates the last step's prompt, so this is a lower bound on how large prompts got, not a count of fires. Read that
   way, a standing corpus is dominated by short runs, and a measurement of the ladder is better served by a selected set
   of runs that approach the input budget than by sampling the standing corpus.
4. **Prefill figures cover only about half of the runs.** Nearly half (45.2 %) of the runs came from an engine that
   reports no prefill token counts, and the KV-reuse figure is over the 53.4 % that did. Any claim about cache reuse
   should state that coverage.
5. **A per-seat reading reaches 70.7 % of the runs.** The pool-alias runs (29.3 %) all came from one node and cannot be
   attributed to a seat from the log; attributing them needs the node's own record of which seat served the run, which is
   a separate change.
6. **Next measurement.** After a release that carries the recording, harvest the delegation logs of the nodes that run
   it and read `pager.measured_runs`, `pager.distinct_evicted`, `compaction.recorded_runs` and `compaction.fire_rate`,
   stating the counts beside any verdict. This ADR sets no minimum sample size; whoever reads the gate states how many
   runs it rests on.

## Consequences

- The two questions now have a reproducible answer path: a hash-stamped corpus, a frozen readout and a byte-exact check.
  Until instrumented runs accumulate, both gates read `NO_DATA`, which is the honest value.
- Runs executed is a smaller, truer denominator than rows logged, so every unrecorded count can reach zero once the
  recording is in place.
- The readout is per machine. Nothing pools delegation logs across machines, and a reader comparing machines compares
  separate readouts.
- The readout carries node ids and seat names as the records write them, so it stays machine-local like the corpus; only
  a path-shaped seat or stop reason is replaced.
- A test seat other than the harness's own stays in `by_seat` until it is named with `--exclude-seat`.

## Alternatives considered

- **Read the ledger.** Its rows carry prefill accounting but no pager report or compaction pass count, and a raw reader
  must also drop the test, bake and measurement rows (a lint enforces it); the delegation log already holds the wire
  result of each agent subtask.
- **Treat an unrecorded pager report as a zero re-fetch rate.** It would close the pager family on no evidence, the exact
  conclusion the gate exists to protect.
- **Count every logged row as a run.** It makes the denominator 24 % larger with rows that never reached the loop and
  makes the unrecorded count impossible to bring to zero.
- **Fold this into `compaction-eval`.** That verb replays harvested transcripts through the ladder, a different input and
  a different question; the two share a redactor and a vet table and nothing else.
- **Guess the seat behind a pool alias from the current configuration.** A node's seat binding changes over time and the
  log does not record which binding served a run.

## Related code

- [`trace_corpus.go`](../../../trace_corpus.go), [`trace_corpus_test.go`](../../../trace_corpus_test.go): the verb and its tests.
- [`internal/agent/contextpager.go`](../../../internal/agent/contextpager.go): the pager instrument, the pass count and `PagerGate`.
- [`internal/core/agentpager.go`](../../../internal/core/agentpager.go), [`internal/pipeline/agentpager_wire.go`](../../../internal/pipeline/agentpager_wire.go): the wire object and its mapping.
- [`internal/compeval/redactor_export.go`](../../../internal/compeval/redactor_export.go): the shared redactor and vet table.

## Related docs

- [`docs/systems/coding-agent.md`](../../systems/coding-agent.md): the `trace-corpus` section.
- [`docs/systems/fleet-node.md`](../../systems/fleet-node.md): the `pager` field of the agent wire result.
