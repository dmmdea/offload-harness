### Added - `compaction-eval contextbench`: ContextBench long-horizon tasks driven against one seat and graded from the context the harness kept

- **What it does.** A new subcommand of `compaction-eval` generates tasks from four ContextBench families
  (`kv_store`, `log_triage`, `needle_retention`, `sudoku_sketchpad`), streams their operations to one agent seat,
  keeps the conversation inside a token budget with the production compaction ladder (`agent.CompactReplay`),
  records the context after every step, and grades it. The result is a results JSON whose per-task outcomes use the
  measurement gate's seven words (`pass`, `task_fail`, `cheat`, `infra_error`, `timeout`, `judge_error`, `not_run`).
  It is the instrument for the context-editing decision; the compaction ladder and the context pager are what it
  measures. Flags: `--family`, `--level`, `--tasks`, `--seed`, `--endpoint`, `--model` (`--seat` is an alias),
  `--route local`, `--out`, `--budget`, `--reserve`, `--delivery`, `--gcf`, `--skeleton`, `--max-turns`, `--pass-at`.
- **Ported, not downloaded.** The four generators and graders are ported to Go from ContextBench (Apache-2.0, pinned
  commit named in each ported file; credited in `NOTICE`). Tasks are a pure function of (family, level, seed), so no
  dataset is fetched. The Go generators reproduce CPython's random stream (Mersenne Twister, `choice`, `sample`,
  `shuffle`, `choices`): a test pins the sha256 of each family's operation stream against the original generator's
  output for twelve parameter sets, so seed 42 is the published seed-42 instance. Only digests are committed, no
  upstream data and no upstream prose; the agent's instruction text is written for this repository.
- **Delivery.** Operations arrive as the result of a synthetic `next_op` tool call by default, because the ladder
  shrinks tool-result bodies before it drops turns; `--delivery user` is the upstream shape and reaches only the last
  rung. The report names the delivery, the ladder rungs and the size estimator (the ladder's own, not the original's
  tokenizer), so two reports are never compared by accident.
- **No seat is not a failed task.** A seat that cannot answer (refused connection, no endpoint, a reset mid-run) makes
  the task `infra_error` and it is never graded; a task that reaches `--max-turns` is `timeout`; a grade that cannot be
  computed is `judge_error`. A graded task below `--pass-at` (default: every item right) is `task_fail` and keeps its
  reward. The report has no timestamps and a reward is null unless the task was graded. With nothing graded the verb
  exits non-zero and says nothing was measured.
- **Tests.** Written before the code they cover, committed failing (build failure on the missing symbols), then green:
  the CPython random stream; generator parity digests and per-seed determinism; each grader on a known-good and a
  known-bad snapshot set (needle reads only the last snapshot, the others every one); the driver against fake seats
  (an oracle that passes every family under both deliveries, a seat that is down, one that dies mid-run, a turn cap,
  an invalid gold, a budget wall, an honest seat whose lost values fail the task once the ladder engages); the seven
  words and no clock field in the JSON; and that the bare instruction scores nothing. The verb is covered by root tests
  with an injected seat and by one run against an unreachable endpoint.
- **Docs.** `docs/systems/coding-agent.md` describes what each family measures, how to run it, the snapshot field the
  grade uses and the outcome table.

**What this does not do.** It runs no measurement and downloads nothing; no result is recorded and no decision follows
from the bench alone. In this reply-only setting the agent has no tool to fetch evicted content, so the pager's
stored pager report has basis `not_exercised` and a null re-fetch rate: it shows evictions only. Scores are not comparable with published ContextBench
numbers (different instruction wording and delivery); they compare harness configurations with each other.
