---
status: Proposed
date: "2026-10-14"
---

# 0091 — Agent-trace corpus readout: pager re-fetch, compaction-fire and prefill

Release: unreleased

## Context

Two questions stand in front of a planned measurement: would a model that edits its own context (the idea of a
published paper on context language models) beat the harness's fixed compaction ladder? How often does the ladder
actually run in real agent runs, so that a measurement of it has something to measure? And is the pager re-fetch rate
(distinct evicted payloads fetched back, over distinct payloads evicted) above the 10 % gate of R2-13, below which the
pager family closes and above which an `edit_context` tool would be worth building? `edit_context` is the proposed tool
that would let the agent seat drop or rewrite earlier turns of its own transcript; the measurement is its go or no-go.

Neither could be read from anything on disk. The agent loop measured both (the pager report, and now the pass count of
the ladder) and the two records that outlive a run dropped them: the standalone per-goal trace file carried no pager or
prefill object, and the wire result the delegator stores in the delegation log carried no pager object. The pager
family therefore sat at `insufficient_data`, and the compaction items had no baseline.

A readout over those records needs three rules, each learned from reading the real log:

- **A missing measurement is not a zero.** A run recorded before a field existed has no value for it. Averaging that
  as 0 would close the pager family on no evidence.
- **A logged row is not a run.** A deferred subtask (capacity, infrastructure, budget, config, contract) is written to
  the delegation log with zero steps and nothing else. The first harvest counted those as runs: 24 % of the runs it
  counted never ran an agent, which inflated every unrecorded count and would have kept the pager's unrecorded count
  above zero after the recording shipped, because a deferral raised before the loop ran never gets the loop's pager
  report. That those rows never reached the loop is inferred, not observed: before the pager recording existed, a
  deferral raised before the loop ran and a run the loop returned from in zero steps with no trace, no prefill or
  seat-token accounting wrote the same row, so the log cannot tell them apart. The skipped count is an upper bound on
  pre-loop deferrals. Once a log holds the recording, a run the loop returned from carries a pager object and is kept,
  and the two separate.
- **A seat alias is not a seat.** A pool placement logs the pool's alias as the seat, and neither the row nor its result
  holds the node's real seat, so a per-seat reading cannot reach those runs.

## Decision

1. **`local-offload trace-corpus` is the standing readout** for these questions, shaped like `compaction-eval`
   (`harvest`, `readout`, `freeze`, `check`) and kept apart from it. It reads recorded agent runs, from the standalone
   agent traces and from the delegation log, and never the ledger. A file in the traces directory is a run only if it is
   shaped like an agent trace (a transcript, and either steps or a stop reason); any other JSON object there is skipped
   under the named reason `trace record has no transcript, or neither steps nor a stop reason` and counted. `readout`, `freeze` and `check` are deterministic and
   model-free, the report has no timestamps, and `check` demands a frozen report back byte for byte.
2. **Every rate is null when nothing was recorded**, with the count of unrecorded runs beside it. A pager object or a
   pass count that is absent or null is unrecorded, never a recorded zero; an object that contradicts itself is read as
   unrecorded and noted. A run that observed prefill steps but reported no token counts (an engine without prefill
   timings) is counted apart and adds nothing to the token sums.
3. **A delegation row is a run only if an agent executed.** A row with zero steps and no step trace, no prefill or
   seat-token accounting and no pager object is skipped under the named reason `delegation row never ran (zero steps,
   nothing recorded)` and counted in the harvest stats. The reason names what the record shows; that such a row never
   reached the loop is an inference (Context). Any record at all keeps the row a run. That includes a deferred
   row with zero steps whose only record is a setup-action trace (36 runs in the first readout, 0.5 %, all budget or
   infrastructure deferrals that stopped with `error`; counted from the corpus, not a readout field). Those rows count as
   runs because the setup actions execute inside the agent loop, before its first model turn and without spending a
   step, so the row did reach the loop; a budget or infrastructure deferral takes the loop's pager report, so once the
   recording ships such a row carries a pager object and is not unrecorded; and the compaction ladder runs before the first model call and again when that call is rejected for
   context size, so a run can end at zero steps with passes counted.
4. **Seats are named only when the record names one.** `by_seat` holds the runs a seat can be named for: each seat the
   record names, and under the key `(unrecorded)` the runs whose record names no seat (every agent trace; the real corpus
   has none). Runs on a pool alias are not in `by_seat`: they are their own bucket (`pool_alias`, and `pool_alias_by_node`
   keyed by the node id the row carries, `(unrecorded)` when it names none). An `attribution` block counts the three
   classes (`seat_named_runs`, `pool_alias_runs`, `seat_unrecorded_runs`) and gives the unattributed share, the last two
   over all runs, so a reader sees how much of the corpus a per-seat reading cannot reach.
5. **Future runs record what the readout needs.** `PagerReport` counts compaction passes; the standalone trace file
   carries the pager and prefill objects of that goal alone (the loop's instruments are reset between goals); the agent
   wire result carries a pager object, zeros included, on every run the loop returned from, a write-class deferral
   (defer class `write`: the write door did not close cleanly) included, because the object is attached right after the
   loop returns, ahead of the write door. A deferral raised before the loop ran has none, but it has no steps and no
   other record either, and is skipped as never ran. A delegation row holds the object only if both ends carry the
   field: the node that served the run emits it, and the delegator that writes the row decodes the node's answer into
   its own wire result and encodes it again, so a delegator build without the field drops it. Rows logged before the
   recording, rows from a node that predates it and rows written by a delegator that predates it have none. The row's
   `delegator_version` names the delegator build and the result's `harness_version` the node build (absent on a
   deferral raised before the loop).
6. **The corpus is machine-local and redacted at harvest.** The corpus keeps numbers and enums only, redacts its few
   strings with the same redactor and vet table as `compaction-eval`, and replaces a seat, node id or stop reason that is
   a local path with a stable placeholder. This ADR carries aggregates only.

## First readout

Harvested on 2026-10-14 from one machine's delegation log (51 daily shards, 10,537 rows). That machine holds no
standalone agent trace files, so every figure below is from delegation-log rows. These numbers are a snapshot of a log
that is still growing.

| Reading | Value |
|---|---|
| Runs executed | 7,057 |
| Rows skipped | 2,233 with nothing recorded (inferred to have never reached the loop, see Context), 1,003 without a result (dispatch error), 22 capacity refusals, 222 on the harness's own test seat |
| Trace-bearing rows (a step trace) | 5,618 (79.6 % of runs) |
| Steps, summed over all 7,057 runs | 21,391 |
| Zero-step runs kept (a setup-action trace only) | 36 (0.5 % of runs), counted from the corpus |
| Pager: runs that recorded a report | 0 of 7,057 (unrecorded 7,057) |
| Pager re-fetch rate against the 10 % gate | null, verdict `NO_DATA` (0 measured runs) |
| Compaction: runs that recorded a pass count | 0 of 7,057, so the fire rate is null |
| Compaction: transcript lower bound | null (no agent trace file on the machine) |
| Prefill: measured, no token counts, unmeasured | 3,772 (53.5 %), 3,188 (45.2 %), 97 (1.4 %) |
| Prefill: KV reuse over the measured runs | 52.4 % of prompt tokens |
| Prefill: tokens per step over the measured runs | 2,521 prefilled (10,106 steps observed) |
| Prefill: prefilled tokens per measured run | p50 3,548, p90 15,025, max 247,847 (3,772 runs) |
| Mean prompt size per step, per run (seat input tokens over steps) | p50 3,607, p90 12,851, max 86,374 (5,411 runs) |
| Runs a seat can be named for | 4,991 (70.7 %) |
| Runs on a pool alias (cannot be attributed to a seat) | 2,066 (29.3 %), all from one node, `<node-b>` (tier `blackwell-3x16`) |
| Runs whose record names no seat (`by_seat` key `(unrecorded)`) | 0 |

## What the numbers decide

A recommendation for the human decision on the planned context-editing measurement (see Context), not a ruling.

1. **The compaction-fire question is unanswered, not answered "rarely".** No run in the corpus recorded a pass count and
   no trace file exists to give the transcript lower bound. The measurement should not take a fire rate from this
   corpus. It should wait for instrumented runs, or build its own set of long runs.
2. **The re-fetch question is unanswered, so the proposed `edit_context` tool (see Context) should be neither built nor
   shelved on this readout.** The gate reads `NO_DATA`; no run recorded a pager report. The pager family stays open, not closed by a
   zero.
3. **The one compaction-relevant reading available is a proxy and points at long runs.** The ladder runs only when a
   transcript passes the seat's input budget (its context window less the output cap and a margin). Across 5,411 runs the
   mean prompt size per step has a median of 3,607 tokens and a 90th percentile of 12,851. A mean over a run's steps
   understates the last step's prompt, so this is a lower bound on how large prompts got, not a count of fires. Read that
   way, a standing corpus is dominated by short runs, and a measurement of the ladder is better served by a selected set
   of runs that approach the input budget than by sampling the standing corpus.
4. **Prefill figures cover only about half of the runs.** Nearly half (45.2 %) of the runs came from an engine that
   reports no prefill token counts, and the KV-reuse figure is over the 53.5 % that did. Any claim about cache reuse
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
  Until instrumented runs accumulate, the pager gate reads `NO_DATA` and the compaction fire rate is null, which is the
  honest value.
- Runs executed is a smaller, truer denominator than rows logged. For runs from a build that carries the recording, the
  pager and compaction unrecorded counts reach zero: every run that reached the loop gets a pager object, zero-step
  deferred runs and write-class deferrals included. Rows logged before the recording, rows from a node that predates it
  and rows written by a delegator that predates it stay unrecorded (both the delegator that writes the log and the
  serving node must carry the field; `delegator_version` on the row and `harness_version` on the result name the
  builds), so a harvest over mixed logs reads zero only once the older rows are windowed out by day (`--since`) and
  every node and delegator that wrote the rest runs the recording.
- The readout is per machine. Nothing pools delegation logs across machines, and a reader comparing machines compares
  separate readouts.
- The readout carries node ids and seat names as the records write them, so it stays machine-local like the corpus; only
  a path-shaped seat, node id or stop reason is replaced.
- A test seat other than the harness's own stays in `by_seat` until it is named with `--exclude-seat`.

## Alternatives considered

- **Read the ledger.** Its rows carry prefill accounting but no pager report or compaction pass count, and a raw reader
  must also drop the test, bake and measurement rows (a lint enforces it); the delegation log already holds the wire
  result of each agent subtask.
- **Treat an unrecorded pager report as a zero re-fetch rate.** It would close the pager family on no evidence, the exact
  conclusion the gate exists to protect.
- **Count every logged row as a run.** On the first readout's log it counts 9,290 runs where 7,057 ran: 2,233 of the
  counted runs (24 %, about a quarter) had nothing recorded and are read as never having reached the loop (an inference,
  see Context), which makes the denominator 31.6 % (about a third) larger than the true one, and it makes the unrecorded count impossible to bring to zero.
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
