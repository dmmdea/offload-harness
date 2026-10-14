### Added - an optional search-results rung in the compaction ladder, default off

- **What it does.** `compactOpts.SearchResults` (internal option, off by default) adds a rung to the agent loop's compaction ladder
  that thins older search listings: the grouped `search_files` output and bare `path:line:text` lines (as `grep -rn` or ripgrep
  print), alone or inside the `exit=` / `--- stdout ---` / `--- stderr ---` framing of `run_shell` and `run`. Each line is scored
  with BM25 against the objective (the last user message of the protected preamble; the line's own path counts as part of the
  line), error-looking lines get a fixed boost, and up to 30 lines, 5 per file and 15 files are kept, chosen round-robin across files
  and written back in listing order under a counted footer (`[... 74 of 100 lines elided, 12 of 30 files elided ...]`). The tool's
  own "more matches" or "truncated" note is kept. Anything that is not, line for line, one of those shapes (the `web_search` list,
  `mode:"files"` output, test output, an error result, binary or non-UTF-8 text) comes back untouched, and so does a result that is
  not strictly shorter.
- **How it sits in the ladder.** One struct field and one call site, after dedupe and GCF and before the generic skeleton, over the
  same older span as the skeleton rung: the protected preamble, the kept-recent tail and pinned bodies are never touched, only a tool
  message's body changes, and pairing is intact. The result opens with the skeleton disclosure line, so the existing idempotence
  check, the re-execute-on-repeat path (ADR 0016), the corpus harvest refusal and the elision marker's original size all treat it as
  the compaction artifact it is, with no change to any of them. There is no CLI flag or loop setter yet; whether the rung ever
  becomes a default is for the measurement gate to decide on a harvested corpus.
- **Why.** A long listing is the commonest bulk tool result on a small window, and the generic skeleton keeps its first and last
  lines whatever the task asks. Ranking by the task keeps the lines the task is about. The idea is documented by the Headroom
  project (Apache-2.0); this is a clean-room implementation of that documented behaviour, credited in `NOTICE`.
- **Tests.** The default ladder is pinned byte for byte before the rung existed (`internal/agent/testdata/ladder_default_golden.json`:
  the mini corpus plus frozen search-shaped and mixed transcripts, three budgets, five option sets, sha256 of messages, tool calls and
  the fit verdict; recorded from the unmodified ladder and unchanged by this change). The rung's tests were committed red first and
  pin determinism, idempotence, monotonicity, failing closed, the artifact contract, ranking (relevance, path, rare terms, line length,
  error boost), the caps, file diversity, the footer counts, the shell tools' real framing and truncation note, and the rung's place in
  the ladder (older tool bodies only, pins and the recent tail untouched, off by default, inert on the mini corpus). A 1,500-input
  mutated-listing sweep (a fuzz target runs the same properties) found one real defect before commit (a flat listing whose files
  interleave was regrouped by file); 21 source mutations were each caught by a test, and every one was proved applied and restored
  byte-clean.
- **Measured, side note only.** On the frozen fixtures the 20 listing bodies keep about 51% of their tokens under the rung and about 46%
  under the generic skeleton, so the rung is not a size win; its case is what it keeps (a planted task-relevant line survived in 120 of
  120 bodies, against 48 of 120 for the skeleton, a synthetic probe and not a corpus verdict). The committed mini corpus holds no
  listing, so the rung changes none of its outputs.
