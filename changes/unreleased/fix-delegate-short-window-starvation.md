### Fixed - three call-deadline tests no longer fail on a starved CPU, and still fail when the rule they pin is broken

`TestNeverStartedRowCarriesTheJobIDTheCallerWasGiven`, `TestAnAbandonedSubtaskIsPublishedWithAJobIDAndARow` and
`TestRunBatchedDefersLaterChunksAtTheDeadline` failed under load on the untouched base and pass alone. The product was right in all
three; each test read a slow runner as a rule violation. The change is to the tests only.

- **Root cause.** Each case gave its call a 300 ms or 400 ms window and expected the subtasks that are meant to finish to have
  answered by its end. A subtask counts as answered only after its seat returns and its corpus line and ledger row are written (the
  first row of a process also hashes the test binary), and the call freezes its unfinished count the first time it looks at its
  deadline. On a starved runner the fast subtasks were still writing their rows, so they were counted as unfinished, by design.
  Under starvation (24 concurrent copies of one test, 8 runs each, 6 CPU spinners and `GOMAXPROCS=2` on an 8-core idle Windows
  host) the old tests failed 178, 146 and 183 of 192 runs. The batched case read between 3 and 17 unfinished where it expects 2; the
  never-started row read "17 unfinished" in 112 of its failures; the abandoned-subtask case failed 47 times on its summary (two
  defers) and 99 times on finding the call's own row, which carried the words of the published reason with a count of 2 where the
  case expected 1.
- **The change.** The three cases share one call window, `startWindow`, of 4 s. Timing the start with a long window at the same
  shape, the last of 15 pages answered at a median of 0.3 s and a worst of 1.6 s over 192 runs, and the fast subtask of a
  two-subtask call at a median of 0.3 s and a worst of 1.4 s. Nothing the cases assert depends on the window's length, because the
  slow subtask ends only at the deadline. Two assertions were pinned to a count that is a snapshot, so they now compare the ledger
  row with the reason the caller received instead (the count itself stays pinned by the batched case, which has a window sized for
  it). The never-started case is bounded by a context and the other two by a 30 s hang guard, so a call deadline that stops ending
  the call fails with a message and does not hold the suite.
- **Still the same rules.** A page that never started is answered, and its row has the job id and the call-deadline words the caller
  was told. A seat that ignores its context is published with a job id and a row of its own, and its late row is under the same id.
  A batched call's deadline covers every chunk: a later chunk does not start after it, and the count in every reason spans both.
- **Proved under starvation.** At the shape above the new tests passed 192 of 192 runs each, and 216 of 216 each at 36 copies. On
  a Linux host (6 copies, 10 runs, 4 spinners) they passed 60 of 60 each. Four mutants, each committed on a detached head and run at
  the 24-copy shape, failed 192 of 192 runs each, every run on the assertion the mutant breaks: the never-started row recorded under
  a second job id, the same row with different words, the abandoned result published under a fresh id so its late row is under
  another, and a batched call that stops sharing one deadline across its chunks. The full `internal/delegate` package passed on a
  workstation under normal load.
