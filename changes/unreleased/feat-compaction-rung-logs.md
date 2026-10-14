### Added - an optional log-collapse rung for the compaction ladder, default off

- **What it does.** `compactOpts.LogCollapse` (default false) adds a rung between the GCF and skeleton rungs. In an OLDER result of
  a command tool (`run`, `run_shell`) it folds each run of three or more consecutive near-identical lines into the first line plus
  a count. Lines match after every digit-bearing token, UUID, long hex run and terminal control sequence is masked; every blank
  line, every line that looks like an error, warning, failure or test summary, and the first 8 and last 4 lines stay verbatim. The
  body opens with `[log-collapse: original N chars, M lines]` and ends with a footer saying what was folded. The rung is one new file
  (`internal/agent/rung_logs.go`) plus, in `compaction.go`, a four-line field, a call site of three lines of code and one clause in
  `IsCompactionArtifact`. No flag, `Loop` setter, config key or replay option enables it yet, so nothing on a production path
  changes.
- **Why.** Build, test and server output is mostly runs of lines that differ only in a timestamp, an id or a counter, and the line a
  later step needs is rarely inside one. This rung keeps every line the skeleton rung would keep and every distinct line besides, so
  it is gentler than a skeleton; because the ladder stops at the first rung that fits, turning it on keeps more of an older log
  than the rungs after it would at the same budget. The idea of compressing build and test output while keeping the failures and
  the boundary lines is the documented behavior of the Headroom project (Apache-2.0); the mechanism is this repository's own,
  clean-room, and `NOTICE` records it.
- **Properties, each pinned by a test.** Deterministic; idempotent (the output's prefix is recognised by every rung and by
  `IsCompactionArtifact`); never longer (the original string comes back unless the result is strictly shorter, checked per run,
  per body and again in the wrapper); fails closed (too small or too large, invalid UTF-8, a NUL byte, a JSON document, another
  rung's marker or a GCF block, an unknown tool, a panic); confined to the ladder's own window, so the protected preamble, the
  recent turns, pinned results and tool-call pairing are untouched; the header, markers and footer stay out of the `signalLine`
  vocabulary so the drop rung treats a folded body like any other.
- **Tests.** Red first: the default ladder's output over the shipped mini corpus and four synthetic log transcripts is pinned by
  sha256 digests captured on the unmodified ladder (`TestLogsRungDefaultLadderBytesPinned`), then the rung's tests were committed
  against a no-op stub (20 failing), then the rung. A mutation run of 33 edits (guards, windows, thresholds, the masking rules, the
  call site, the pin check, the default) killed all 33; the first run left six survivors (five test gaps, now closed, and one
  redundant early return, now removed).
- **Docs.** `docs/systems/coding-agent.md` describes the rung with its option name and "default off". Whether to adopt it is left to the
  measurement gate.
