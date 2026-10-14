### Added - an optional log-collapse rung for the compaction ladder, default off

- **What it does.** `compactOpts.LogCollapse` (default false) adds a rung between the GCF and skeleton rungs. In an OLDER result of
  a command tool (`run`, `run_shell`) it folds each run of three or more consecutive near-identical lines into the first line plus
  a count. Lines match after every digit-bearing token, UUID, long hex run and terminal control sequence is masked (the footer says
  digits are ignored); every blank line, every line that looks like an error, warning, failure or test summary, and the first 8 and
  last 4 lines stay verbatim. "Looks like" includes CamelCase exception names (`TypeError`, `AssertionError`,
  `NullPointerException`), a killed process, a segfault, a non-zero `exit status` and npm's `ERR!`, and it is tested on the line as
  written and with its terminal colour removed. The body opens with `[log-collapse: original N chars, M lines]` and ends with a
  footer saying what was folded. A result whose tool-call id is declared by more than one call has no known producer and is left
  alone. The rung is one new file (`internal/agent/rung_logs.go`) plus, in `compaction.go`, a field, a call site and the
  original-size reporting described below. No flag, `Loop` setter, config key or replay option enables it yet, so nothing on a
  production path changes.
- **A rung that is off changes nothing.** `IsCompactionArtifact` keeps its three clauses; the always-on dedupe rung and the live
  loop's re-execute decision both ask it, so recognising the new header there would change the default ladder for any tool body that
  quotes it (a saved collapsed log read back with `read_file`). A separate `IsLogCollapseArtifact` recognises the header, and the
  trace harvest asks both so it still refuses a transcript the rung has compacted. Every parser of the new header is gated on the
  option. Wiring the rung into the live loop later must also teach `resultDestroyed` the new predicate.
- **Honest markers.** With the rung on, the skeleton prefix, the elide marker, the reactive last-resort marker and the dedupe
  reference of a folded body name the size the body had before the fold (parsed from the header, as the skeleton's size already was),
  so a marker never says that less was lost than was. `emergencyShrinkWith` carries the ladder options to the last resort;
  `emergencyShrink` keeps its signature and means "no optional rung". The header is believed only when the rung could have
  written it: its exact spelling, a size above the body's own length and within the sizes the rung folds, and a line count a fold
  needs. A result that merely quotes a header with a negative, zero, oversized or too-small number is named by its own length;
  a quoted skeleton header gets the same test (exact spelling, a size above the body's own length and between the smallest body a
  skeleton prunes and the largest the rung folds), so it cannot set a marker's size either. A
  quoted header with merely plausible numbers is indistinguishable from the real one in the text and is still believed.
- **Why.** Build, test and server output is mostly runs of lines that differ only in a timestamp, an id or a counter, and the line a
  later step needs is rarely inside one. This rung keeps every line the skeleton rung would keep and every distinct line besides, so
  it is gentler than a skeleton; because the ladder stops at the first rung that fits, turning it on keeps more of an older log
  than the rungs after it would at the same budget. The idea of compressing build and test output while keeping the failures and
  the boundary lines is the documented behavior of the Headroom project (Apache-2.0); the mechanism is this repository's own,
  clean-room, and `NOTICE` records it.
- **Properties, each pinned by a test.** Deterministic; idempotent (the output's prefix is recognised by the rung itself and by
  `IsLogCollapseArtifact`); never longer (the original string comes back unless the result is
  strictly shorter, checked per run, per body and again in the wrapper); fails closed (too small or too large, invalid UTF-8, a NUL
  byte, a JSON document, another rung's marker or a GCF block, an unknown or ambiguous producer, a panic); confined to the ladder's
  own window, so the protected preamble, the recent turns, pinned results (also through a dedupe reference) and tool-call pairing
  are untouched; placed after the GCF rung and before the skeleton rung (a budget the fold alone meets leaves no skeleton, a budget
  GCF alone meets leaves the logs raw); the header, markers and footer stay out of the `signalLine` vocabulary so the drop rung treats
  a folded body like any other.
- **Tests.** Red first: the default ladder's output over the shipped mini corpus, four synthetic log transcripts and two transcripts
  of bodies that only quote the header is pinned by sha256 digests captured on the unmodified ladder
  (`TestLogsRungDefaultLadderBytesPinned`, which fails if the digest map is empty), then the rung's tests were committed against a
  no-op stub, then the rung. A mutation run of 33 edits (guards, windows, thresholds, the masking rules, the call site, the pin
  check, the default) killed all 33 of the first version; each later fix was mutation-checked on its own (16 further edits: the
  ladder position both ways, the pin set, the stripped-line test, the error-name and failure-shape patterns, each size-reporting
  site, the option gate on the size parser, the header predicate in the harvest, the reused-id check, the restored artifact clause),
  and the one that first survived (the last-resort pass over a body beyond the skeleton rung) got a test. The size parser's bounds
  are pinned at each edge, and the skeleton branch of the size helper, which only the last resort's pass over a newest body that is
  already a skeleton reaches, has a test that fails when the branch is deleted.
- **What the evidence does and does not cover.** The savings figures printed by `TestLogCollapseMeasuredSavings` come from fixtures
  whose tool calls are renamed to `run`; the shipped mini replay corpus records call ids only, so the producer gate leaves it
  untouched and the rung is a no-op there. `ReplayOpts` does not expose the option, so the compaction eval cannot measure the rung
  yet. The later measurement-gate run needs a corpus whose tool calls carry names and a replay option. Digits being masked means a
  lone ` 500 ` access-log line can fold into a run of ` 200 ` lines; that is the specified matching, and the footer states it.
- **Docs.** `docs/systems/coding-agent.md` describes the rung with its option name and "default off". Whether to adopt it is left to the
  measurement gate.
