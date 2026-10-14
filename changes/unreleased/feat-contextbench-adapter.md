### Added - `compaction-eval contextbench`: ContextBench long-horizon tasks driven against one seat and graded from the context the harness kept

- **What it does.** A new subcommand of `compaction-eval` generates tasks from four ContextBench families
  (`kv_store`, `log_triage`, `needle_retention`, `sudoku_sketchpad`), streams their operations to one agent seat,
  keeps the conversation inside a token budget with the production compaction ladder (`agent.CompactReplay`),
  records the context after every step, and grades it. The result is a results JSON whose per-task outcomes use the
  measurement gate's seven words (`pass`, `task_fail`, `cheat`, `infra_error`, `timeout`, `judge_error`, `not_run`).
  It is the instrument for the context-editing decision; the compaction ladder and the context pager are what it
  measures. Flags: `--family`, `--level`, `--tasks`, `--seed`, `--endpoint`, `--model` (`--seat` is an alias),
  `--route local`, `--out`, `--budget`, `--reserve`, `--delivery`, `--gcf`, `--skeleton`, `--max-turns`, `--pass-at`,
  `--max-out` (reply cap in tokens), `--request-timeout` (transport deadline of one seat request) and `--config`
  (the config file that supplies the default endpoint and model). A value the run would silently replace with a
  default is refused with the flag's name and range: `--budget`, `--reserve`, `--max-out` and `--request-timeout`
  must be at least 1, `--pass-at` must be above 0 and at most 1, `--max-turns` at least 0.
- **Ported, not downloaded.** The four generators and graders are ported to Go from ContextBench (Apache-2.0, pinned
  commit named in each ported file; credited in `NOTICE`). Tasks are a pure function of (family, level, seed), so no
  dataset is fetched. The Go generators reproduce CPython's random stream (Mersenne Twister, `choice`, `sample`,
  `shuffle`, `choices`): a test pins the sha256 of each family's operation stream against the original generator's
  output for twelve parameter sets, so seed 42 is the published seed-42 instance, and `scripts/contextbench_parity_regen.py`
  regenerates those digests from a checkout of the pinned upstream commit. The operation mix is ported, so the
  streamed operation texts are the original's byte for byte; only digests are committed, no upstream data, and the
  agent's instruction text is written for this repository.
- **Levels.** Without `--level`, each family runs the smallest published level whose uncompacted transcript (the
  instruction, every operation and, for sudoku, the board panel the agent prints after each move) exceeds the usable
  budget: `R1000`, `L1600`, `K4-C24` and `t60`. The sudoku stream alone never overflows (about 12k estimated tokens at
  `t150`), so its default is the first level whose panels do; a test pins each default as the smallest one over.
- **Delivery.** Operations arrive as the result of a synthetic `next_op` tool call by default, because the ladder
  shrinks tool-result bodies before it drops turns; `--delivery user` is the upstream shape and reaches only the last
  rung. The report names the delivery, the ladder rungs and the size estimator (the ladder's own, not the original's
  tokenizer), so two reports are never compared by accident.
- **No seat is not a failed task.** A seat that cannot answer (refused connection, no endpoint, a reset mid-run) makes
  the task `infra_error` and it is never graded; a task that reaches `--max-turns` is `timeout`; a grade that cannot be
  computed is `judge_error`. A graded task below `--pass-at` (default: every item right) is `task_fail` and keeps its
  reward. The report has no timestamps and a reward is null unless the task was graded. With nothing graded the verb
  exits non-zero and says nothing was measured. A budget too small for the instruction plus the first operation is
  refused before any seat call (as a library call it ends `infra_error`, never a graded `task_fail`), an interrupt
  that lands while a seat request is in flight is `not_run`, and a context over the budget is never recorded as a
  snapshot, so the needle grader cannot credit operations the seat never received at a budget wall.
- **A wall before any query is not a grade.** When the context stops fitting the budget after the seat has answered
  but before it was handed a single query (`kv_store`, `log_triage`), no frame can hold an answer, so the task ends
  `timeout` (the attempt hit its budget) with `term_reason` `budget_wall_before_query` and no reward, not a graded
  `task_fail` of zero. A wall once a query has been handed over, and a wall in the two families that ask nothing, is
  graded on the frames the task has. The task record also carries `ops_sent`, the operations the seat was handed,
  beside `ops_delivered`, which counts the over-budget one. One published cell ends this way for a perfect agent, on
  purpose: under the default delivery the production ladder cannot hold `log_triage` at `L12800`, because its
  preserve guard keeps every tool result that still matches a signal word and the synthetic log lines carry
  `[WARN]`, `timeout` and `refused`. The level is left as published, the oracle ends there as `timeout`, and a test
  pins it as a signal about the ladder. `--delivery user` passes that cell.
- **Graders.** A `kv_store` answer is matched on its key without quotes or case, and its value must stand as a whole
  (the characters around it are not letters or digits), so the same words with a longer tag no longer count. A
  `log_triage` count answer is read after a repeated `Query N` or `qid=N` label is removed and digit groups such as
  `12,800` are joined, and it still must state exactly one number.
- **Tests.** Written before the code they cover, committed failing, then green: the CPython random stream; generator
  parity digests and per-seed determinism; each grader on a known-good and a known-bad snapshot set (needle reads only
  the last snapshot, the others every one; each key, tag-boundary, label and separator rule on its own rows); the
  driver against fake seats (an oracle that passes every family under both deliveries, the same oracle on every
  family's largest level under the default delivery with the one documented exception, a seat that is down, one that
  dies mid-run, a turn cap, an invalid gold, a wall before and inside the query block, an honest seat whose lost
  values fail the task once the ladder engages); the seven words and no clock field in the JSON; and that the bare
  instruction scores nothing. The verb is covered by root tests with an injected seat and by one run against an
  unreachable endpoint, by flag tests that fail if `--delivery`, `--budget` or `--reserve` stop reaching the run or a
  silently defaulted value is accepted, and by a test that every flag the verb defines is in its docs. Every published
  level at seed 42 is pinned by operation and gold digest, so a knob change in the level table fails a test.
- **Docs.** `docs/systems/coding-agent.md` describes what each family measures, how to run it, the snapshot field the
  grade uses and the outcome table.

**What this does not do.** It runs no measurement and downloads nothing; no result is recorded and no decision follows
from the bench alone. In this reply-only setting the agent has no tool to fetch evicted content, so the stored
pager report has basis `not_exercised` and a null re-fetch rate: it shows evictions only. Scores are not comparable with published ContextBench
numbers (different instruction wording and delivery); they compare harness configurations with each other.
