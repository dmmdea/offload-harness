### Added - authority-graded acceptance: an optional qualifier on contains:, regex: and not_contains:

- **What it does.** `contains:`, `regex:` and `not_contains:` take an optional authority qualifier at the head of the
  argument: `@answer:` (the answer text the model wrote), `@field=<name>:` (the values of one structured field) and
  `@grounded:` (in the answer and verbatim in a context document; `not_contains:` has no grounded form, since documents
  cannot corroborate an absence). The levels are ordered answer < field < grounded. Evidence carries an authority, a
  weaker level's evidence never closes a stronger level's claim, and an empty or absent evidence set grades `unknown`:
  the merge gate counts it as a failed check, and the reason reads `unknown at <level> authority: ...` so an empty
  answer is told apart from a wrong one. `core.AcceptanceCheck.Grade(result, docs)` returns pass, fail or unknown;
  `core.ClosedAt` and `delegate.GradeAcceptance` report the authority a contract closed at (its weakest check, and only
  when all passed). `delegate.EvalAcceptance` now grades through the same path.
- **The closed-at authority is published.** `delegate.EvalAcceptanceGraded` returns it beside the failures, every
  site that grades a result records both through one helper, and the result a caller reads carries
  `acceptance_authority` (`answer`, `field` or `grounded`), as does the corpus row. It is the weakest authority among
  the checks, stated only for a result accepted whole: one unqualified check, a failed or unknown check, a
  list-fidelity failure or an excused document fingerprint leaves the key absent (omitempty), so every result that
  is not closed at an authority publishes byte-identically to before. A weaker authority cannot close a stronger
  claim: a contract of one grounded and one answer check closes at answer.
- **Why a qualifier and not a verb.** The verb set is closed and pinned (`reviewedAcceptanceVerbs`), and every verb
  must be shown to read no clock. The qualifier lives in the argument tail, so no verb is added, a node one release
  behind still parses the line (it only parses acceptance at the ACK), and an older delegator reads it as a literal
  substring (the Skew bullet says what that does to each verb). The intake lint adds `AUTHORITY-UNREACHABLE` for a
  grounded check on a contract with no context documents (a blank document is none, as Grade reads it) and for a
  field check the `output_schema` does not declare; a field-level check joins the schema's `required` list through
  `Fields()`.
- **What did not change.** An unqualified check is byte for byte what it was, failure text included. That includes
  `not_contains:` passing over a fully empty result. No file under `contracts/` (the write-door contracts included)
  uses `not_contains:`, and an empty final answer is deferred before acceptance, but the case is not shown unreachable
  (results also arrive from nodes, the queue and the rescue path) and external contracts cannot be seen, so the
  stricter reading applies to the qualified form only. `min_items:`, `nonempty:` and the diff verbs already fail
  closed on empty evidence and are unchanged. Some previously valid lines now fail to parse, by a rule and not
  a list: the reserved prefixes are exactly `@answer:`, `@grounded:`, `@field=` and `@field:` (the last is refused,
  since the field separator is `=`), and after one a missing field name, a missing colon or an empty tail is a parse
  error, and so is `not_contains:@grounded:` (documents cannot corroborate an absence). A substring that starts with a
  reserved prefix on one of the three verbs reads as qualified; any other leading `@` is still a literal, so
  `contains:@field`, `contains:@fieldname:x` and `contains:@fieldx` are plain substrings.
- **The document-fingerprint excuse.** The research lane excuses a FAILED document fingerprint when the digest is an
  honest absence. That excuse now drops only a failed check, never an unknown (an unknown stays a reported failure),
  and the quarantine reads the fingerprint through the same grading as the merge gate, with the contract's context
  documents: a grounded fingerprint the documents carry is a pass for the node, not an excused absence. A fingerprint
  the contract can never close (the unknown the `AUTHORITY-UNREACHABLE` lint warns about, such as a grounded check on a
  contract with no context documents) is a defect of the contract: it is still reported as a failed check, but it is
  neither a strike nor a pass for the node (no quarantine) and is not retried on a second node. An unknown the
  contract could have closed, such as an empty answer beside the documents, still counts against the node. An unknown
  over an honest-absence digest (a qualified field fingerprint on an empty field, the absence sentence in another
  field) strikes nothing either, since an honest absence is neither a pass nor a strike, and a fingerprint the node
  passed beside a contract-caused unknown is still recorded as its pass.
- **The rig.** The failure-class register keeps classifying a qualified check's failure by its verb prefix, so an
  unknown (empty or absent evidence) is counted as `anchor-miss`, not `schema-miss`; the empty-versus-wrong distinction
  lives in the evidence column (`unknown at <level> authority`), not in the axis (docs/systems/coding-agent.md).
- **Skew.** A node one release behind admits a qualified contract (a literal substring to its parser). A delegator one
  release behind, or a misspelt qualifier, reads the line as a literal: `contains:` and `regex:` fail closed. On a
  delegator one release behind, `not_contains:@answer:SECRET` is read as the literal string "@answer:SECRET", so the
  check passes even when the answer contains SECRET and the forbidden-content check never fires. Qualified checks need
  the whole fleet on 0.187 or later.
- **Tests.** Red first (the tests commits do not compile, or fail, until the implementation lands): parse table
  including the literal-`@` and malformed cases, the verdict table (pass, fail and unknown for every level and
  verb), the ordering and the weaker-never-closes-stronger table, full-evidence pins (every match of a grounded
  pattern, every context document, boolean and number field values), `ClosedAt`, a pin of every unqualified case
  including the vacuous `not_contains:`, the delegator path with the contract's documents, the fingerprint excuse and
  its quarantine reading, the lint warnings (a blank document included), the published `acceptance_authority` on the
  local and remote paths and the corpus row, a source guard that every grading site records it, the rig reading real
  graded failures as anchor misses, and root tests that the verb set is unchanged and a speed term behind a
  qualifier is still found. Docs: `contracts/README.md` (authority-graded checks), the operator guide and the
  `acceptance` row of `docs/systems/fleet-node.md`. The tool descriptions in `internal/mcpserver` still list the verbs
  without the qualifier and the result keys without `acceptance_authority`; they are left to the schema-profile work
  that owns that file.
- **Known limits.**
  - "Not retried" holds only for the research door: a deterministically unreachable authority (such as
    `contains:@grounded:` with no context documents) is still retried on a second node. The better fix is an intake
    refusal with a contract-class defer.
  - A qualified `not_contains` on a legitimately empty field grades unknown, so pair it with a presence check.
  - `Authority.AtLeast` and the exported `delegate.GradeAcceptance` have no production caller yet.
