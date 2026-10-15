### Fixed - the context-bench adapter's outcome words are pinned to the measurement gate's taxonomy

- `internal/compeval` writes the gate's seven outcome words (`pass`, `task_fail`, `cheat`, `infra_error`, `timeout`,
  `judge_error`, `not_run`) as its own strings, because the adapter landed before the gate's enum did. A class the gate
  added, dropped or reordered would have left the adapter's results unreadable by the gate while every adapter test stayed
  green. `TestContextBenchOutcomesAreTheGatesTaxonomy` now compares `CBOutcomes` with `eval.AllOutcomes()` in order, and with
  the literal the run tests grade results against. Test only; no behaviour changes. A mutant that renames one word fails it.
