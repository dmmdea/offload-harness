### Added - authority-graded acceptance: an optional qualifier on contains:, regex: and not_contains: (ENH-13b)

- **What it does.** `contains:`, `regex:` and `not_contains:` take an optional authority qualifier at the head of the
  argument: `@answer:` (the answer text the model wrote), `@field=<name>:` (the values of one structured field) and
  `@grounded:` (in the answer and verbatim in a context document; `not_contains:` has no grounded form, since documents
  cannot corroborate an absence). The levels are ordered answer < field < grounded. Evidence carries an authority, a
  weaker level's evidence never closes a stronger level's claim, and an empty or absent evidence set grades `unknown`:
  the merge gate counts it as a failed check, and the reason reads `unknown at <level> authority: ...` so an empty
  answer is told apart from a wrong one. `core.AcceptanceCheck.Grade(result, docs)` returns pass, fail or unknown;
  `core.ClosedAt` and `delegate.GradeAcceptance` report the authority a contract closed at (its weakest check, and only
  when all passed). `delegate.EvalAcceptance` now grades through the same path.
- **Why a qualifier and not a verb.** The verb set is closed and pinned (`reviewedAcceptanceVerbs`), and every verb
  must be shown to read no clock. The qualifier lives in the argument tail, so no verb is added, a node one release
  behind still parses the line (it only parses acceptance at the ACK), and an older delegator reads it as a literal no
  answer carries, which fails closed. The intake lint adds `AUTHORITY-UNREACHABLE` for a grounded check on a contract
  with no context documents and for a field check the `output_schema` does not declare; a field-level check joins the
  schema's `required` list through `Fields()`.
- **What did not change.** An unqualified check is byte for byte what it was, failure text included. That includes
  `not_contains:` passing over a fully empty result: a write-door leg and a schema-only result reach acceptance with no
  output, no shipped contract under `contracts/` uses `not_contains:`, but the documented write-door contracts pair it
  with `diff_touches:` and external contracts cannot be seen, so the stricter reading applies to the qualified form
  only. `min_items:`, `nonempty:` and the diff verbs already fail closed on empty evidence and are unchanged. The one
  string that changes meaning is a substring that starts with a reserved spelling (`@answer:`, `@grounded:`,
  `@field=`) on one of the three verbs; any other leading `@` is still a literal.
- **Tests.** Red first (the tests commit does not compile until the implementation lands): parse table including the
  literal-`@` and malformed cases, the verdict table (pass, fail and unknown for every level and verb), the ordering
  and the weaker-never-closes-stronger table, `ClosedAt`, a pin of every unqualified case including the vacuous
  `not_contains:`, the delegator path with the contract's documents, the lint warnings, the rig still reading a
  qualified failure as an anchor miss, and root tests that the verb set is unchanged and a speed term behind a
  qualifier is still found. Docs: `contracts/README.md` (authority-graded checks) and the `acceptance` row of
  `docs/systems/fleet-node.md`. The tool descriptions in `internal/mcpserver` still list the verbs without the
  qualifier; they are left to the schema-profile work that owns that file.
