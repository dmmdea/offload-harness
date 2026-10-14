### Added - the admitted third-party software table, and the leak gate pinned over the OpenSpec tree

- **What it does.** `docs/systems/security.md` gains the public table "Admitted third-party software": one row per artifact and
  pinned version, with its licence, what it is used for, how its telemetry is switched off before the first run, and where its
  data goes. The first row is the OpenSpec CLI (`@fission-ai/openspec` 1.14.1, tarball sha256 pinned). `.gitignore` keeps the
  per-tool skills and commands that `openspec init` and `openspec update` generate under `.claude/` and `.opencode/` out of the
  tracked tree; the `openspec/` tree itself stays tracked.
- **Why.** OpenSpec writes spec and change prose about this repository, the likeliest place for a machine name to reach the public
  tree. The tree gate already scans every tracked path with no filter, so the gate needed no new scope, only tests that keep it
  that way, and the generated tool copies needed to stay out of the tree (they would add tool prompt text and drift from the
  tool version that wrote them).
- **Tests.** `TestScanTreeCoversTheOpenSpecTree` (`internal/leakgate`): bodies and names under `openspec/` are flagged, YAML and
  Markdown alike; a mutation adding an `openspec/` path filter to `ScanTree` turns it red (the mutation was proved applied by a
  non-empty diff, then restored byte-clean). `TestOpenSpecGeneratedToolFilesStayUntracked` (root): red first on the five generated
  paths, green with the `.gitignore` lines, and it also fails if the `openspec/` tree or another tool file were ignored.
