# Self-improvement

## Purpose

Say how the harness improves itself on evidence, and where each part of that loop lives. The rule is that a change
to a seat, a cascade rung, a compaction ladder or a skill is measured against what it would replace, judged by one gate
whose rules were written down and hashed before the first candidate ran, and adopted only when the gate says so.
Otherwise the incumbent stays.

This page is the map of that loop at this head. It names the verb or door that runs each part, what the part answers
and what it refuses to claim, and links the code and the page that owns the detail. It does not repeat those pages.
Two parts are proposed and not merged; they get a short paragraph each at the end and no code links.

## Questions this doc answers

- How does the harness decide that a change is an improvement, and who may say so?
- Which readouts feed that decision, and which command or tool runs each one?
- What is the difference between a readout and a verdict?
- What does a review result say about the diff it judged?
- Which parts of the loop are proposed and not on main yet?

## Scope

The discipline and the four parts that exist at this head: the failure register, the trace-corpus readout, the
measurement gate and the SHA-bound evidence on a review result.

## Non-scope

- The detail each part's own page owns: the failure classes ([failure-register.md](failure-register.md)), the agent
  loop and its pager ([coding-agent.md](coding-agent.md)), the gate's statistics ([measurement-gate.md](measurement-gate.md)),
  the review lane ([mcp-server.md](mcp-server.md)).
- The evidence-preserving reducer rung and the recipe store. Neither is merged, so neither is described beyond its
  intent.
- Scheduling. Every command below runs when an operator or an agent calls it. None is a scheduled job, and none edits
  configuration, a seat binding or a rule.
- What each command writes. `ledger --failures`, `trace-corpus check`, `gate check`, `gate verdict` and `gate schema`
  write nothing. `trace-corpus harvest|readout|freeze` (`readout` only when it is given `--out`) and `gate freeze` write
  only the file they are told to. Two do more. `gate selftest` writes its receipt to `--out` when given, and with its real scorer
  (the only one today) it runs that scorer through the production pipeline, so it also writes what any pipeline call
  writes: ledger rows for the scorer's calls and the result cache among them. A review that reaches an agent run writes
  that run's own ledger row the same way, and every sealed review appends to the `review-evidence.jsonl` trace beside
  the ledger.

## Key concepts

- **Incumbent and candidate.** The thing in place and the proposed replacement, compared on the same declared tasks.
  The incumbent keeps every tie.
- **Pre-registration.** The rules of one comparison (mode, primary metric and margin, judge, efficiency metrics, task
  list), written into a baseline file and hashed before the first candidate run. A verdict names that hash.
- **Verdict.** ADOPT, BLOCK or INCONCLUSIVE. Only ADOPT lets a candidate replace the incumbent.
- **Unrecorded.** A value nobody measured. Where the code can say so it reads `null`, never 0, with the count of cases
  that did or did not record it beside it: every rate of the trace-corpus readout, the register's card work
  (`cards_tokens`, beside `rows_with_tokens`) and a gate efficiency metric resolved on fewer than two pairs. The
  readout's totals (passes, evictions, prefill tokens) are sums over the recorded runs, so they read 0 when no run
  recorded them, with `recorded_runs` and `unrecorded_runs` beside them (`unmeasured_runs` for prefill). The gate's
  primary interval reads 0 below two pairs, and the verdict is then INCONCLUSIVE through `min_pairs`.
- **Failure class and kind.** The class of a failed job is its reason code. Its kind says whose failure it is (below).
- **Outcome.** The closed label on a review result: `PASS`, `FINDINGS`, `GATE_FAILED`, `NOT_RUN` or `STALE`.

## How the system works

### The discipline

1. **Measure.** A readout counts something that was recorded: the ledger's failed jobs, the agent loop's pager and
   compaction instruments, the diff a review judged. A readout never decides anything.
2. **Gate.** A candidate and the incumbent are scored on the same declared tasks and the scores go through
   `local-offload gate`, the one place that returns a verdict.
3. **Adopt or keep the incumbent.** ADOPT replaces it. BLOCK and INCONCLUSIVE keep it (`keep_incumbent` is true unless
   the verdict is ADOPT), and the remedy for INCONCLUSIVE is more tasks, not more attempts.

**Quality is the only gate, and no pass rule carries a wall-time term** (INV-5, cited by [ADR 0050](../architecture/decisions/0050-placement-ranks-adequate-seats-by-expected-completion.md)
and [ADR 0088](../architecture/decisions/0088-one-measurement-gate.md)). The gate enforces it at freeze: a primary
or efficiency metric whose name carries a wall-time word (seconds, latency, duration, wall and the like) is refused,
and the only efficiency classes are tokens, money and resident memory in GiB. A timeout is a budget failure counted as
an unfinished attempt, not a duration the gate reads.

| part | runs as | answers | owner page |
|---|---|---|---|
| failure register | `local-offload ledger --failures` | which failures the stack has, how many, whose they are, what is known about why | [failure-register.md](failure-register.md) |
| trace-corpus readout | `local-offload trace-corpus harvest\|readout\|freeze\|check` | how the agent loop behaves in recorded runs: pager re-fetch, compaction passes, prefill | [coding-agent.md](coding-agent.md), [ADR 0091](../architecture/decisions/0091-agent-trace-corpus-readout-pager-refetch-compaction-fire-and-prefill.md) |
| measurement gate | `local-offload gate freeze\|check\|verdict\|schema\|selftest` | whether a candidate is better, or not worse and cheaper | [measurement-gate.md](measurement-gate.md), [ADR 0088](../architecture/decisions/0088-one-measurement-gate.md) |
| SHA-bound review evidence | `offload_review_diff` (MCP tool) | which bytes a review judged, and whether a clean seat judged them | [mcp-server.md](mcp-server.md) |

The first, second and fourth rows only count or label. The third is the only row that adopts.

### The failure register

`local-offload ledger --failures [--since DAYS] [--include-test-traffic] [--table FILE]` prints one JSON object. A
failed job (deferred, carrying a failing reason code, or failing its acceptance check) is filed under its reason code,
a member of the closed set in [`internal/ledger/reasoncode.go`](../../internal/ledger/reasoncode.go). A row written
before the code was stamped is classed at read time and counted in `class_derived_rows`. Rows are counted per door,
seat and model.

Every class has one of six closed **kinds**, decided in code beside the reason-code set
([`failurekinds.go`](../../internal/ledger/failurekinds.go)): `stack_fault`, `capacity`, `caller`, `ceiling`, `work`
and `unclassified`. The kind lets a count answer "how often does the stack itself fail" without counting a full queue
or a caller's bad contract, and an `unclassified` failure is never counted as the stack's.

Three rules keep the count honest:

- A node's capacity refusal at dispatch is a refused attempt, set aside in `refused_attempts`. It is neither a job nor
  a failure, so a subtask refused once and placed again does not count as two.
- A ledger line that does not decode is counted in `unparsed_lines`, and a ledger file that is not there reads
  `ledger_found: false`. Neither reads as a quiet zero.
- Card work (`cards_tokens`) is `null` when no row of the class measured it.

The **cause and the fix are a person's**. No code derives them. Each class is joined by name to the table in
[failure-register.md](failure-register.md), embedded in the binary at build time (`--table` reads another copy). A
class with no row reads `undocumented`, and a class whose row holds neither a root cause nor a structural fix is listed
in `uninvestigated_classes`. At this head no row of the table has been investigated, so the register's first product is
its gap list. A test fails for a non-empty cell until the class is named in it with what the finding rests on.

### The trace-corpus readout

`local-offload trace-corpus` has four modes. `harvest` reads recorded agent runs, from the standalone agent traces and
the delegation log and never the ledger, reduces each to numbers and enums, redacts the few strings left and writes the
corpus to `--out`, printing its sha256, which every report then carries as `corpus_hash`. `readout` prints the report.
`freeze` writes the report as a baseline. `check` demands the report back byte for byte and refuses a baseline taken from
a different corpus. `readout`, `freeze` and `check` are deterministic and model-free, and the report carries no
timestamp.

It reports, for the whole corpus, by source and by seat:

- the **trace-bearing run count** (an agent trace with a transcript, or a delegation row with a step trace);
- the **pager re-fetch rate** (distinct evicted payloads fetched back over distinct payloads evicted, pooled over the
  runs that evicted something) against the 10 % gate `agent.PagerGate`, as `BELOW_GATE`, `ABOVE_GATE` or `NO_DATA`;
- the **compaction pass count** behind the fire rate (runs with at least one pass over runs that recorded a count),
  passes per run and passes per step. One pass is one call of `PagerStats.NoteCompaction`: the proactive pass, the
  harder pass after a server overflow rejection, or the emergency shrink;
- **prefill** tokens, cached tokens and KV reuse, with the per-run spread.

Its rules follow the register's: nothing is guessed. Every rate is `null` when nothing was recorded, with the unrecorded
count beside it, and a total is a sum over the recorded runs, so it reads 0 then. A delegation row is a run only if an
agent executed, so a deferral logged with zero steps and nothing else is skipped under a named reason and counted. A
pager object that contradicts itself is read as unrecorded and noted. A pool placement logs the pool's alias and not the
node's real seat, so those runs are reported apart (`pool_alias`) and `attribution` says how much of the corpus a
per-seat reading cannot reach.

The first readout, in [ADR 0091](../architecture/decisions/0091-agent-trace-corpus-readout-pager-refetch-compaction-fire-and-prefill.md)
(status Proposed at this head, a snapshot of one machine's log on 2026-10-14 and not re-measured here), found no run that
had recorded a pager report or a pass count. The pager verdict read `NO_DATA` and the fire rate `null`, so the questions
were recorded as unanswered rather than answered "rarely".

### The measurement gate

`local-offload gate` is a pure function over a frozen baseline and one comparison, plus its verb. It reads scores that
someone else produced and measures nothing itself, so `freeze`, `check`, `verdict` and `schema` need no model, GPU or
network; `selftest` alone can reach a seat, for the scorer it runs. The adapter that turns runs into arms is the trust
boundary: the gate checks the task ids of each arm against the declared list, not the text of a task.

| subcommand | does |
|---|---|
| `freeze` | validates a pre-registration, hashes the corpus and writes the canonical baseline, created exclusively, then prints its path and hash |
| `check` | refuses a baseline that is not byte for byte what `freeze` writes, and prints its hash (`--expect-hash` also refuses another hash) |
| `verdict` | judges a candidate against the incumbent under the baseline and prints the result as JSON (`--require-adopt` exits 1 unless ADOPT) |
| `schema` | prints the JSON Schema of the gate input |
| `selftest` | ranks control pairs with a named scorer and prints the judge receipt |

Exit codes: 0 the command ran (for `verdict`, a verdict was produced), 1 `--require-adopt` and the verdict is not
ADOPT or a self-test mis-ranked a pair, 2 a refusal or a usage error.

The rules, in short:

- **The statistic.** The mean of the paired per-task differences over the usable pairs, with a two-sided 95 % interval.
  The unit is the task. Efficiency metrics are read by a paired bootstrap, each at `1 - 0.05/k` for `k` declared metrics.
- **Superiority:** ADOPT iff the interval's lower end is above 0, BLOCK iff its upper end is below 0.
  **Non-inferiority:** BLOCK iff the upper end is below minus the margin; ADOPT iff the lower end is above minus the
  margin, a declared efficiency metric improves, and none got significantly worse or went unmeasured.
- **The incumbent wins.** Fewer than `min_pairs` usable pairs (default 16, and it cannot be frozen below 16) or an
  excluded share above `max_excluded_share` (default 0.20) is INCONCLUSIVE.
- **No gain from not finishing.** A candidate's timeout, infrastructure error, judge error and not-run attempts enter at
  the worst score of the primary range, and a cheat enters there on both arms. The incumbent's unfinished attempts are
  skipped.
- **Held-out.** A held-out slice declared at freeze can veto an ADOPT (it becomes BLOCK) and can never rescue a verdict.
- **Pre-registration.** The input names the baseline hash and the gate refuses another one. The gate version is 3, and
  a baseline frozen under another version is refused.
- **Judge integrity.** The judge's kind and identity are part of the baseline. A `model` judge needs a passed receipt,
  `{scorer_id, pairs_hash, passed, build_sha256}`, written by `gate selftest`, which fails on a tie. The one scorer
  `selftest` knows today is `compaction-outcome`. The gate only reads the receipt.
- **Eras.** Results with different era markers are never compared; a comparison re-runs the incumbent in the new era.

[ADR 0088](../architecture/decisions/0088-one-measurement-gate.md) is Proposed at this head. Two older callers,
`agent-trajectory-gate` and the compaction A/B, still read `eval.BootstrapDeltaMean` their own way and are not migrated.

### SHA-bound review evidence

`offload_review_diff` is the clean-context review lane: a diff and a task in, ranked findings out, advisory. Every
payload it returns, deferrals and early refusals included, carries an `outcome` and an `evidence` block
([`internal/reviewlane/evidence.go`](../../internal/reviewlane/evidence.go)).

- **The binding is the diff's sha256** (`diff_sha256`, `diff_bytes`), plus the optional `base_sha` and `head_sha` the
  caller passes. Those two are hex, echoed and never resolved: the lane has no git access, so the head is the caller's
  word and the fingerprint is the binding that holds. `prompt_sha256` binds the prompt the seat saw, and the seat receipt
  names who answered and digests the raw answer. It carries no timing.
- **Gates run first.** `evidence.gates` is built before any seat is contacted: the lane's own `diff-size` and
  `conflict-markers`, then the `gate_receipts` the caller hands over (at most 16), the results of lints and tests it ran
  itself. The lane executes nothing a caller names. A receipt for another head, or naming no commit when the review
  names one, cannot support a PASS.
- **One outcome, chosen in one place** (`reviewlane.Decide`, in a fixed order). `NOT_RUN` means no clean seat reviewed
  the diff: a deferral, a runner error, a refusal before any diff was read, a refused binding, no seat receipt, no
  fingerprint, or a diff over `MaxDiffBytes` (256 KiB). It is never a PASS. `STALE` means the diff was not the same
  bytes at the second check. `FINDINGS` is an advisory list. `GATE_FAILED` is a clean seat beside a red, stale or
  unbound gate. `PASS` needs a seat with a receipt, a fingerprint and a second check equal to it, an in-bound diff, every
  gate green and for the reviewed commit, and the seat's own NONE.
- **A second fingerprint check** runs in `reviewlane.Seal`, right before the result returns. A `diff_path` is re-read and
  compared; a difference, or a file that can no longer be read, makes the result `STALE`. An inline diff is compared
  with itself, so the check cannot catch movement there.
- **An advisory finding always leaves a trace.** Each sealed review appends one `review` row and one `finding` row per
  published finding to `review-evidence.jsonl`, beside the configured ledger file, whether or not anyone reads or applies
  the findings. A failed write is reported as `trace_error` on the result. No ledger row is written for the verdict,
  because every ledger reader counts a row as a call or a job.

The lane stays advisory: an outcome is a label a lead reads, and nothing here blocks a merge or a commit.

### Proposed, not on main yet

**The evidence-preserving reducer rung: not on main yet.** Proposed as a rung of the agent loop's context handling that
shrinks large tool output while keeping the evidence checkable, and falls back to the raw output when it cannot. It stays
off until its own measurement passes the gate above. [ADR 0090](../architecture/decisions/0090-capability-review-2026-10-dispositions-and-reopen-triggers.md)
records it as a port; nothing of it is merged.

**The recipe store and its audit verb: not on main yet.** Proposed to keep recipes (data, never code) distilled from runs
that passed their acceptance checks, with an audit verb that runs a contract with and without a recipe on one seat, so a
recipe is published only when the gate passes. It stays off until its own gate measurement passes. [ADR 0036](../architecture/decisions/0036-the-agent-lane-is-a-harnessed-environment.md)
already holds the proposer side back until there is a trace corpus in the hundreds, and the readout above is what counts
trace-bearing runs.

## Interfaces and entry points

- `local-offload ledger --failures [--since DAYS] [--include-test-traffic] [--table FILE]`
- `local-offload trace-corpus harvest [--traces DIR] [--delegation-log DIR] --out FILE`, then
  `readout|freeze|check --corpus FILE` (`check` also takes `--baseline FILE`)
- `local-offload gate freeze|check|verdict|schema|selftest`
- `offload_review_diff`, with the optional inputs `base_sha`, `head_sha` and `gate_receipts`

## Invariants and assumptions

- Only the gate adopts. A readout, a register row or a review outcome is input to a person or to a gate run, never a
  verdict.
- A missing measurement is `null` for a rate (every trace-corpus rate), for the register's `cards_tokens` and for a gate
  efficiency metric, and is counted beside the value. The trace-corpus totals are sums over the recorded runs and read 0
  when none recorded, with `recorded_runs` and `unrecorded_runs` beside them; read those before reading a 0 as a finding.
- The incumbent keeps every tie and every INCONCLUSIVE.
- No pass rule, verdict or ranking carries a wall-time term.
- The rules of a comparison are frozen and hashed before the first candidate runs, and a verdict names the hash.
- Cause and fix in the register are written by a person. A review is advisory, and its head commit is the caller's word.
- A command writes what Non-scope names: an `--out` file or nothing, except that `gate selftest` and a review's agent
  run write what any pipeline call writes (ledger rows and the result cache among them) and a review also appends its
  trace. None changes configuration, a seat binding or a rule.

## Security and privacy notes

- The trace corpus keeps numbers and enums. It redacts its few strings with the same redactor and vet table as
  `compaction-eval harvest`, replaces a seat, node id or stop reason that is a local path with a placeholder, and is
  machine-local data that is never committed. Seat and node names appear as the records wrote them, so a readout is
  machine-local like the corpus.
- `review-evidence.jsonl` holds short statements about someone's code (each claim and reason cut to 512 bytes). The file
  is created with an owner-only mode.
- A real corpus, ledger or review trace is machine-local data and does not belong in the repository. The tracked-tree
  [leak gate](leak-gate.md) reads this page like any other file, so machines appear here as roles, never by name.

## Error handling

- The register reports a missing ledger (`ledger_found: false`) and unreadable lines (`unparsed_lines`) in its JSON and
  on stderr, and keeps going.
- `trace-corpus harvest` refuses to write when a run still matches a vet class after redaction, and writes nothing when
  the strict re-load of the corpus fails. `check` fails with the first differing line when the report no longer
  reproduces the baseline.
- The gate returns a refusal and no verdict (exit 2) for a wrong baseline hash, a swapped judge, a missing or failed
  receipt, an undeclared task, a wall-time metric name and similar. A refusal is never an INCONCLUSIVE.
- A review whose trace cannot be written is still delivered, with `trace_error` set.

## Testing notes

- [`ledger_failures_test.go`](../../ledger_failures_test.go) runs the register on a synthetic ledger against a golden
  file, and holds the table to the reason-code set. [`internal/ledger/failures_test.go`](../../internal/ledger/failures_test.go)
  pins the reader's rules one by one.
- [`trace_corpus_test.go`](../../trace_corpus_test.go) covers the harvest, the loader, the readout and the byte-exact
  check.
- [`gate_cmd_test.go`](../../gate_cmd_test.go) and [`gate_selftest_cmd_test.go`](../../gate_selftest_cmd_test.go) drive
  the verb and its exit codes. [`internal/eval/gate_test.go`](../../internal/eval/gate_test.go) and
  [`gate_attack_test.go`](../../internal/eval/gate_attack_test.go) hold the verdict table and the attacks a candidate that
  wants to be adopted would try.
- [`internal/reviewlane/evidence_test.go`](../../internal/reviewlane/evidence_test.go) and
  [`internal/mcpserver/reviewevidence_test.go`](../../internal/mcpserver/reviewevidence_test.go) cover the outcomes, the
  fingerprints, the gate receipts and the trace through the real door.
- `go test -run TestDocsLint .` checks that this page's links resolve and that it keeps its Purpose and Source map.

## Common pitfalls

- **`BELOW_GATE` is not ADOPT.** The pager's 10 % gate is a threshold on one rate in the trace-corpus report. It is
  unrelated to the measurement gate's verdicts.
- **`null` and `NO_DATA` are not zero.** The register's `cards_tokens`, a trace-corpus rate or an efficiency delta that
  reads `null` was never measured. A trace-corpus total that reads 0 (a pass count, an eviction count) may only mean no
  run recorded it: read `recorded_runs` beside it. An empty root-cause cell means nobody has looked, not that nothing is
  wrong.
- **A review PASS is not an approval.** It says a clean seat read these bytes and found nothing, with gates green for the
  named head. It does not block or allow a merge, and the lane cannot show the head belongs to the diff.
- **Bind a verdict to the hash `freeze` printed**, not to a later `gate check` of the file: an edit that leaves a valid
  canonical document is another pre-registration, and only the hash tells it from the frozen one.
- **ADR 0088, ADR 0090 and ADR 0091 are Proposed at this head.** Only `Accepted` ADRs are current guidance, so the code
  and the system pages above are what this page rests on.

## Source map

- [`ledger_failures.go`](../../ledger_failures.go): the `--failures` mode, the table parser and the join.
- [`internal/ledger/failures.go`](../../internal/ledger/failures.go),
  [`failurekinds.go`](../../internal/ledger/failurekinds.go) and [`reasoncode.go`](../../internal/ledger/reasoncode.go):
  the reader, the kinds and the closed reason-code set.
- [`trace_corpus.go`](../../trace_corpus.go): the `trace-corpus` verb, the harvest, the corpus format and the readout.
- [`internal/agent/contextpager.go`](../../internal/agent/contextpager.go): the pager report, the compaction pass count
  and `PagerGate`.
- [`gate_cmd.go`](../../gate_cmd.go): the `gate` verb, its exit codes and the self-test.
- [`internal/eval/gate.go`](../../internal/eval/gate.go), [`baseline.go`](../../internal/eval/baseline.go) and
  [`gate_input.go`](../../internal/eval/gate_input.go): the verdict rules, the pre-registration and the input format.
- [`internal/reviewlane/evidence.go`](../../internal/reviewlane/evidence.go): the evidence block, `Decide`, `Seal` and
  the trace.
- [`internal/mcpserver/mcpserver.go`](../../internal/mcpserver/mcpserver.go): `handleReviewDiff`, where the evidence is
  bound and sealed at every exit.
- [`main.go`](../../main.go): the `ledger`, `trace-corpus` and `gate` cases and their usage lines.

## Related docs

- [failure-register.md](failure-register.md): the classes, the kinds and the table of causes and fixes.
- [measurement-gate.md](measurement-gate.md): the rules, the statistic, the formats and the power note.
- [coding-agent.md](coding-agent.md): the agent loop, its pager and the `trace-corpus` section.
- [mcp-server.md](mcp-server.md): the review lane and the rest of the tool surface.
- [ADR 0088](../architecture/decisions/0088-one-measurement-gate.md),
  [ADR 0091](../architecture/decisions/0091-agent-trace-corpus-readout-pager-refetch-compaction-fire-and-prefill.md),
  [ADR 0050](../architecture/decisions/0050-placement-ranks-adequate-seats-by-expected-completion.md),
  [ADR 0036](../architecture/decisions/0036-the-agent-lane-is-a-harnessed-environment.md) and
  [ADR 0090](../architecture/decisions/0090-capability-review-2026-10-dispositions-and-reopen-triggers.md).
