### Added - `trace-corpus`: a read-only readout over recorded agent runs, with the pager report and compaction pass count now recorded

- **What it does.** `local-offload trace-corpus <harvest|readout|freeze|check>` (RSI-OH-2, new root file `trace_corpus.go`,
  one `case` line in `main.go`) is shaped like `compaction-eval`. `harvest` reads the standalone agent trace files and the
  delegation log (never the ledger, so `ledger_readers_lint_test.go` does not apply), reduces each run to numbers and enums,
  redacts the strings that remain with the redactor and vet table the compaction harvest uses, and writes a hash-stamped,
  machine-local corpus. `readout`, `freeze` and `check` are deterministic and carry no timestamps; `check` demands the frozen
  report back byte for byte and refuses a baseline from another corpus. The readout gives the trace-bearing run count, the
  pager re-fetch rate pooled over the runs that evicted something against the 10 % gate of R2-13 (`agent.PagerGate`, now
  exported so the readout and the instrument judge by one number), the compaction fire rate and passes per run and per step,
  and prefill tokens with KV reuse, for the whole corpus, by source and by seat.
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
  the loop report), which the delegation log stores. Absent means a record from before the field; present means measured,
  zeros included. `internal/compeval` exports its redactor (`NewRedactor`, `PIIClasses`) in a new file so a second harvester
  cannot redact a different set of classes.
- **Fixed on the way.** One loop drains a whole queue and its pager and prefill instruments accumulate for the loop's life, so
  the stderr line and the new trace fields of goal N were the running total of goals 1 to N, and a payload evicted in an
  earlier goal could count as re-fetched in a later one. `Loop.ResetRunInstruments` (serial reuse only; a loop shared by
  concurrent runs keeps one set of accumulators by design) starts them over before each goal.
- **Tests.** Red first for the counter, the trace fields, the wire field and the per-goal reset (each committed failing, then
  fixed). `trace_corpus_test.go` runs harvest, freeze and check over synthetic fixtures in `testdata/tracecorpus` and requires
  byte-identical corpus and report across two harvests and across CRLF endings, pins every derived number against hand-worked
  values, and covers redaction (literals built at run time), the unrecorded-never-zero rule, the gate boundary (the instrument's
  `rate < gate` rule), the strict loader, the seat and day filters and the `main.go` registration. Mutations run and killed: an
  unrecorded rate returned as 0, the gate comparison widened to `<=`, redaction removed (twice: with and without the exit
  gate), the check made to always pass, a self-contradicting pager object trusted, a zero pass count read as a fire, and a
  token-less prefill read as measured.
