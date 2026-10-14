### Added - `trace-corpus`: a read-only readout over recorded agent runs, with the pager report and compaction pass count now recorded

- **What it does.** `local-offload trace-corpus <harvest|readout|freeze|check>` (new root file `trace_corpus.go`,
  one `case` line in `main.go`) is shaped like `compaction-eval`. `harvest` reads the standalone agent trace files and the
  delegation log (never the ledger, so `ledger_readers_lint_test.go` does not apply), reduces each run to numbers and enums,
  redacts the strings that remain with the redactor and vet table the compaction harvest uses, and writes a hash-stamped,
  machine-local corpus. `readout`, `freeze` and `check` are deterministic and carry no timestamps; `check` demands the frozen
  report back byte for byte and refuses a baseline from another corpus. The readout gives the trace-bearing run count, the
  pager re-fetch rate pooled over the runs that evicted something against the 10 % gate of R2-13 (`agent.PagerGate`, now
  exported so the readout and the instrument judge by one number), the compaction fire rate and passes per run and per step,
  and prefill tokens with KV reuse (with the per-run spread, nearest-rank p50, p90 and max, of prefilled tokens over the
  measured runs and of the mean prompt size per step), for the whole corpus, by source and by seat.
- **Only runs that executed.** A deferred subtask (capacity, infrastructure, budget, config, contract) is logged with zero
  steps and nothing else. Harvest skips such a row under the named reason `delegation row never ran (zero steps, nothing
  recorded)`; any step, step trace, prefill or seat-token accounting, or pager object keeps a row a run. The first real
  readout had counted rows that never ran an agent as runs: 24 % of the runs it counted, a denominator about a third
  larger than the true one, which inflated every unrecorded count and would have kept pager.unrecorded_runs above zero
  after the recording shipped. A deferred row with zero steps and only a setup-action trace stays a run: the setup actions
  execute inside the agent loop before its first model turn, a budget or infrastructure deferral takes the loop's pager
  report, and the compaction ladder can run before the first model call, so such a run is not unrecorded once the recording
  ships. From a build with the recording, the one kind of run that has a trace and no pager object is a write-class
  deferral (the write door refused the run's write set): it is a run and counts as unrecorded, and the first log holds
  none.
- **Seat attribution is explicit.** A pool placement logs the pool's alias as the seat, so those runs cannot be attributed to
  a seat. `by_seat` holds the runs a seat can be named for (each seat the record names, and under the key `(unrecorded)`
  the runs whose record names no seat, every agent trace) and not the alias runs; alias runs are the `pool_alias` bucket
  and `pool_alias_by_node` (keyed by the node id the row carries, `(unrecorded)` when none), and the new `attribution`
  block gives `seat_named_runs`, `pool_alias_runs`, `seat_unrecorded_runs` and `unattributed_share`. The corpus gains an
  optional `node` field (an older corpus without it still loads).
- **Paths are redacted.** The shared vet table has no path class, so a seat, node id or stop reason that is a local path (a
  drive path, a rooted path, anything with a backslash) is replaced at harvest with a stable `[redacted-path-N]`
  placeholder (counted under `redactions.path`), and the strict loader refuses a corpus that still carries one.
- **Why.** The gate that closes or opens the whole pager family, and the rate at which the ladder fires, could not be read from
  anything on disk. The loop measured both and the trace file and the wire result dropped them, so the family sat at
  `insufficient_data` and the compaction items had no baseline. The fields below make future runs record them; the verb reads
  them back.
- **Unrecorded is never zero.** Every rate is a pointer that serializes as `null` when nothing was recorded, with the count of
  unrecorded runs beside it. A corpus of rows written before this change reads `NO_DATA` on the gate and null on the fire
  rate; a run that recorded a pass count of 0 is a real 0. A pager object that contradicts itself is read as unrecorded and
  noted. A run that observed steps but reported zero prefilled and zero cached tokens (an engine with no prefill timings) is
  counted apart as `no_token_counts_runs` and adds nothing to the token sums, so it cannot read as a measured zero.
- **Recording (additive, `omitempty`).** `PagerReport` gains `compaction_passes` (the number of ladder passes in the run: a
  proactive pass, the harder pass after a context-overflow rejection and the emergency shrink each count one). The standalone
  per-goal trace gains `pager` and `prefill` objects, and `AgentWireResult` gains `pager` (`core.AgentPager`, same key names as
  the loop report), which the delegation log stores. Present means measured, zeros included. Absent means a record from
  before the field or from a node that predates it, a run that never reached the loop, or a write-class deferral (the write
  door refused the run's write set, and that return comes before the object is attached). `internal/compeval` exports its
  redactor (`NewRedactor`, `PIIClasses`) in a new file so a second harvester cannot redact a different set of classes.
- **Fixed on the way.** One loop drains a whole queue and its pager and prefill instruments accumulate for the loop's life, so
  the stderr line and the new trace fields of goal N were the running total of goals 1 to N, and a payload evicted in an
  earlier goal could count as re-fetched in a later one. `Loop.ResetRunInstruments` (serial reuse only; a loop shared by
  concurrent runs keeps one set of accumulators by design) starts them over before each goal.
- **Tests.** Red first for the counter, the trace fields, the wire field, the per-goal reset, the never-ran skip, the path
  redaction (seat, stop reason and node id), the pool-alias bucket and the per-run spread (each committed failing, then fixed). `trace_corpus_test.go` runs
  harvest, freeze and check over synthetic fixtures in `testdata/tracecorpus` and requires byte-identical corpus and report
  across two harvests and across CRLF endings, pins every derived number against hand-worked values, and covers redaction
  (literals built at run time, and the fixture check builds its banned words at run time too), the unrecorded-never-zero rule,
  the gate boundary (the instrument's `rate < gate` rule), the strict loader, the seat and day filters and the `main.go`
  registration. The pager-object validator is pinned at every guard (null and absent pass count, unknown and empty basis,
  each negative count, insufficient data with evictions or distinct payloads, refetched above evicted), an unusable object is
  one note and one unrecorded run from a delegation row and from a trace, only tool turns count as ladder artifacts, and
  `agentPagerWire` keeps an unrecorded pass count absent on the wire and maps every field when it is recorded.
- **Mutations run and killed.** An unrecorded rate returned as 0, the gate comparison widened to `<=`, redaction removed
  (twice: with and without the exit gate), the check made to always pass, a self-contradicting pager object trusted, a zero
  pass count read as a fire, a token-less prefill read as measured; each term of the never-ran predicate removed in turn;
  each alternative of the path pattern removed, the path redaction removed for the seat, for the stop reason and for the node id, the loader's
  path refusal weakened to one field; the pool-alias pattern loosened three ways, the node-less bucket key changed, the
  unattributed share dropped to aliases only, the node field dropped or left unredacted; a nil pass count read as a recorded
  zero, the negative-count terms and each of the other guards removed, the tool-role filter dropped; the wire mapping
  returning an object of zeros for an absent count, and each field of it swapped; the quantile rank rounded the wrong way, the
  p90 taken as the p50, the maximum taken one short, the sort removed, a zero-step run divided into the prompt size, the empty
  set not reported as such. One mutant is equivalent and survives by construction: removing only the `distinct_evicted < 0`
  term, because a negative distinct count is always also caught by the refetched checks beside it.
