### Fixed - a refused placement is no longer a job in the savings summary, the top defer reasons or the other job counters, and a literal `null` ledger line is an unparsed line

- **A refusal row is not a job.** A node's capacity refusal at dispatch (a 503 or a 429) did no work: the delegator waited and
  placed the subtask again, and the subtask's outcome is another row. The failure register already set the marked row aside as
  a `refused_attempts`, but its own page said "the savings summary's job counters still count a marked row", and they did: each
  refused placement added one call and one deferred job to the summary, put "dispatch: status 503: queue full" beside the real
  reasons in the top defer reasons, gave a session a row it never ran, and raised the defer rate of the task it was filed under.
  `ledger.CountsAsJob` now returns false for a row carrying the refusal mark, beside the dispatch marker and an inner row whose
  parent is present, and every reader that counts jobs follows it, so they agree. The register reads the same numbers as before
  through its own predicate (a refusal row that is a record of its own is a refused attempt; an inner row carrying the mark is
  still neither a job nor a refused attempt), and the summary now skips a refusal row before its inner-row branch so a refusal
  can never add saved tokens. A ledger written before the `refusal` column existed has none marked, and its refusals still count
  as the jobs they were then recorded as.
- **Readers whose numbers move** (on a ledger that holds refusal rows; nothing moves on one that holds none): the savings summary
  of `local-offload ledger` (`calls`, `deferred`, `by_task`; `excluded_non_production`, where a test-class refusal is no longer an
  excluded job); the top defer reasons it prints (`top_defer_reasons_7d`); `ledger.JobRows` and so `local-offload stats` (`n`, `deferred`, the defer rate
  per task), `loupe` (rows, deferred, the defer reasons, the per-task tallies) and `audit-sample` (a refusal is no longer a hard
  case to sample); `ledger.ProductionRows` and so the count of excluded jobs every production view reports; and the session split
  (`ledger --by-session`, where a session that only had refusals has no row and a session's `rows` drops by its refusals; the token totals
  do not move, a refusal row carries none). Not moved: the failure register itself, and the readers that take the production
  rows without applying the job rule (health, router and calibration, confhead) since they never skipped a marker or an inner row
  either. The earlier failure-register entry chose not to change the other counters in its own pull request; this is that change.
- **A literal `null` line is unparsed.** `encoding/json` decodes `null` into a struct without an error and without touching it, so
  a ledger line holding only `null` read as a row with every field at zero: a job row that did not fail in the register, a
  completed call of no task in the summary. A ledger row is a JSON object, and the register now counts any line that is not one
  (`null`, an array, a bare number or string, garbage) in `unparsed_lines`, so a partly unreadable ledger still does not look like
  a quiet one. The ledger's own file readers (`ReadAll` and the savings summary) skip the same line, so the summary and the
  register agree on what a row is. `{}` is an object and stays a row: it counts as a job that did not fail, in the register and in
  the summary alike, and is not in `unparsed_lines`. Every column of the ledger is optional (a row from before a column existed
  lacks it, and the writer omits what is empty), so a reader cannot tell a minimal old row from an empty one by looking inside,
  and what an object must contain is a schema rule that every new column would have to be remembered in, not a parse rule.
- **Comment only.** `dispatch_refused` files a 401 or 403 answer as the caller's (`internal/ledger/failurekinds.go`). A comment
  at that line now says a credential refusal between the fleet's own nodes, such as a stale fleet token after a rollout, is a
  configuration fault, and that splitting 401 and 403 into a class of their own is a possible later change. No behaviour changes.
- **Tests.** Each fails on the parent commit. `TestARefusalRowIsNotAJobInAnyJobCounter` (a ledger with three real jobs, one
  refusal row and a test-class pair) reports 4 calls and 2 deferred where 3 and 1 are right, lists the 503 among the top defer
  reasons, counts 2 excluded test jobs where 1 is right, and gives the refused-only session a row; the same test holds the
  register's refused-attempt, job and failure counts as they were. `TestStatsDoNotCountARefusalRowAsAJob` is the stats report.
  `TestAnInnerRefusalRowIsNeitherAJobNorARefusedAttempt` holds the inner-row case. `TestALiteralNullLineIsUnparsedAndAnEmptyObjectIsARow`
  (a ledger holding `null`, `{}`, a valid failure row and a truncated line) read `unparsed_lines` 1 where 2 is right and 3 job rows
  where 2 is right; `TestTheOtherLedgerReadersSkipANullLineToo` read 3 calls and 3 rows where 2 is right. `docs/systems/failure-register.md`
  and the `refusal` row of `docs/FLEET-NODE.md` say the new rule.
