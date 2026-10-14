### Added - a test refuses a speed term in any contract's acceptance array and pins the acceptance verb set as clock-free (H-03, INV-5)

- **The rule and the gap.** INV-5 and ADR 0050 say quality outranks speed and that "Acceptance criteria and pass rules stay
  wall-time-free". Nothing in the repository enforced that for the shipped contracts: `TestContractFixturesAreWellFormed` reads
  three of the twelve JSON files under `contracts/`, and the four templates, the four write-door contracts and the node list were
  parsed by no test. The eleven files that hold an `acceptance` key carry 87 strings and none has a speed term today (a scan of the
  raw text and of the text with regex escapes undone), so this is a tripwire for the next edit, not the fix of a live violation. The
  out-of-repo scan that did exist (a lint of the operator's measurement drivers and plan register) captured an `acceptance` array up
  to its first `]`, so `["regex:[0-9]", "regex:within 90 s"]` showed it only `regex:[0-9` and the speed term went unseen.
- **The gate.** One new root-package test file, `contracts_speedterm_test.go`; tests and docs only, no internal package is touched
  and no node runs different code. `TestContractAcceptanceCarriesNoSpeedTerm` walks every `*.json` under `contracts/` with a real
  JSON parse and reads the key `acceptance` at any depth and in any case (a single template, a top-level array of contracts, an object
  with a nested `subtasks` list; `parseContractFile` decodes with `encoding/json`, which fills the field from `Acceptance` and
  `ACCEPTANCE` too, so those are live lists and a gate that compared the key exactly would not see them). It fails on an `acceptance` value that is
  not a list of strings, on a string `core.ParseAcceptanceCheck` does not
  parse (the closed DSL has no time verb, so a pseudo-check such as `wall_ms:1500` is refused, and the parse coverage of the shipped
  contracts goes from three files to all of them), on a string that carries a speed term, and on a file that is not valid JSON. Only the
  key `acceptance` is read: ADR 0047's "No speed term enters the choice", quoted in a context text, stays legal. Non-vacuity: the
  scanner must have read exactly the files that a separate, case-insensitive byte search for `"acceptance":` finds, and at least one
  string, so a walk that reads nothing cannot pass.
- **The pattern.** `speedTermPattern` is the pattern of the out-of-repo classifier, compiled under RE2 (the 30 vectors of
  `TestSpeedTermVectors`, 16 positive and 14 negative, give the same result under Python's engine): `slower`, `faster`, `speedup`, `tok/s >`, `wall <= 1.5x`, `<= 1.5x
  the`, `2x the K=1`, and a bare figure with a unit after `near`, `within`, `under`, `below` or `at most`. A `regex:` check is also read
  with its escapes undone (`wall\s*<=\s*1\.5x` is `wall <= 1.5x`), so the escaped spellings do not hide. That is best effort and the
  README says so: a figure spelled `\d+`, `[0-9]+` or `(?:a|b)` evades it. The literal is a raw string so the out-of-repo lint can read
  it and fail when the two drift ("change both or neither").
- **The verb pin, which does not depend on spelling.** `TestAcceptanceVerbSetIsReviewed` reads `internal/core/agentwire.go` with
  `go/parser`: the constants typed `AcceptanceKind` and the case labels of `ParseAcceptanceCheck`'s switch (a constant name, a string
  literal or an `AcceptanceKind("x")` conversion) must equal a reviewed set of seven verbs that gives each one the reason it reads no
  clock, and so must the verb list in the unknown-verb message. Names a clock verb would carry (`wall`, `wall_ms`, `max_wall`,
  `latency`, `ttft`, `tok_s`, `within`, `under`, `faster`, `slower`, `elapsed`, `duration`) must be refused as unknown. A label the
  reader cannot resolve is an error, never a skipped verb. A change that adds a verb must add it to `reviewedAcceptanceVerbs` with its
  reason. This replaces parsing the hand-kept error message alone (which is already stale in one place and would pass a verb added
  without updating it).
- **Not done, on purpose.** `core.ParseAcceptanceCheck` does not reject speed words (that would change what a node accepts when it
  ACKs a contract, and rolling-upgrade compatibility with it); `delegate.LintAcceptance` gains no category (its comment says it is kept in
  lockstep with a classifier outside the repository, so a new category would double the surface); there is no allowlist for a content
  check that quotes a speed phrase: it anchors on the figure instead (`regex:90`), which `contracts/README.md` now says.
- **Docs.** `contracts/README.md` states the rule beside the DSL table (no verb reads a clock, what the two tests refuse, that the scan
  is a lint and the closed verb set is the real defence) and in the paragraph on `TestContractFixturesAreWellFormed`.
- **Tests, red first.** A seam commit adds the pattern, the finding type and four helpers that return nothing; a test-only commit
  follows and fails on its assertions (`speedTermIn("contains:faster") = ("", false), want a speed term` for all 16 positive vectors;
  `got 0 findings, want 1` for each fixture; `the scanner read [], the byte search finds an acceptance key in 11 files`;
  `AcceptanceKind constants = [], reviewed = [contains, diff_max_files, ...]`). Four subtests were green from the start by design: a
  file with no `acceptance` key is not counted, only `.json` files are read, the unknown-verb message lists the reviewed set, and
  clock-shaped names are refused (they pin behaviour that already held). The fix commit fills the helpers. A test-only commit
  found while choosing mutants: the scanner and the byte search shared one extension-filter line, so it differs now; the scanner no
  longer sorts a list nothing observed; and a parser with no switch on the kind got its fixture. A review round then found the key was
  compared exactly while the production reader matches it without regard to case: four fixtures (`Acceptance`, an upper-case key in a
  nested subtask, both spellings in one object, a capitalised bare string) failed on `got 0 findings [], want 1` and two byte-search
  cases on `filesWithAcceptanceKey = "", want "c.json"`; `strings.EqualFold` and `(?i)` fixed them, and a premise test that runs the
  real `parseContractFile` on three spellings (green from the start by design) records why.
- **Mutation proofs (39, each one changed line, `scripts/mutation-check.sh`, all RED with the control arm green and the file restored).**
  Pattern: `speed-?up` dropped, `near` dropped, the `tok/s` alternative, the `wall <= 1.5x` alternative, the `<= 1.5x the` alternative,
  the `2x the K=1` alternative, the trailing word boundary of the time term, the trailing boundary of the first group. De-escape: the
  regex argument scanned as written, `\s+` not read, `(?i)` not dropped, `\.` not read. Scanner: nested objects not walked, nested
  arrays not walked, the parse step off, the speed step off, a bare-string `acceptance` not refused, a non-string element
  stringified, the byte order mark not trimmed, invalid JSON skipped, non-`.json` files read, keys other than `acceptance` read, the key
  compared exactly, the byte search without `(?i)`, the live
  walk pointed at the wrong directory, subdirectories skipped, files never counted. Source reader: constants not collected,
  conversion labels unread, an unresolved label accepted, a missing parser function tolerated, a missing switch tolerated. Verb pin on
  the real parser: a literal `case ..., "budget_ms":` (the case arm only), a declared constant with no case (the constant arm only), a
  clock name rewritten before the parse (the name probe only), a verb dropped from the error message (the message arm only). Live data:
  `regex:within 90 s` in a template, `regex:near 22 s` in a nested write-door contract, `wall_ms:1500` in a template.
- **Outside this repository.** The operator-side speed-term lint is now a hard gate on the same pattern (an untagged hit, an
  `acceptance` list with a speed term read with a real JSON walk, or a drift from `speedTermPattern` fails the session-start audit, and
  a surface that scanned zero files, a file it cannot read, or an acceptance directory in which it met no `acceptance` key is
  unchecked, not clean; the key is matched in any case there too). Nothing in this release's code depends on it.
