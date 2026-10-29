### Changed - the repository reads as a public project: a renderable changelog, a stranger-proof README, no evaluation images in the tree, and the maintainer's private vocabulary glossed or removed

- **What changed.** `CHANGELOG.md` was 2.9 MB, past the size at which GitHub shows "we can't show files that are this
  big" instead of the file (checked live on 2026-10-29); it now holds `[Unreleased]` and the releases from 0.190.0, and
  every older entry moved verbatim (three lines neutralised for the leak scan) into `docs/changelog/CHANGELOG-<newest>-to-<oldest>.md` (eight files, each under
  the render limit, indexed by `docs/changelog/README.md`). `changelog.Combined` appends the archive after the live file and its fragments, so every doc-quote check
  (root, `internal/rig`, `internal/mcpserver`) reads the same history it read before the split, and the no-pins lint treats it as history. `runs/g2-images/` (220 generated evaluation
  PNGs, 363 MB, swept into the tree by an unrelated 2026-08-11 commit and referenced by nothing) is untracked and
  `/runs/` is ignored; the files stay in history until the maintainer decides on a rewrite. The README gained the
  Quickstart prerequisite (a serving llama.cpp), the `local-offload` binary-name note, a Windows long-path note (a clone into a long
  path fails its checkout on two 145-character ADR names), a Linux pointer and an honest
  macOS line, says `go install ...@latest` is not a supported path (a local `replace` and tags that lag the source),
  labels the OpenWebUI script as the legacy WSL convenience it is, adds the `agent_rig`, `agent_delegate` and
  `offload_research` rows, trims the longest tool cells to a summary plus a link, links every ADR it names, and
  drops the maintainer's task labels and calibration provenance. `SECURITY.md` names the three opt-in surfaces that
  can send content off the box instead of claiming nothing does. `CODE_OF_CONDUCT.md` (Contributor Covenant 2.1),
  issue forms and a pull-request template that cannot be read as a deletion declaration were added. The maintainer's
  private vocabulary (nightshift, register ids, a private decision tool, private evidence paths, a personal GitHub
  account in `setup/install.ps1`) is glossed in `docs/glossary.md`, reworded in the comments and tier notes that
  used it, or replaced with the environment-driven lookup the Go updater already uses; `docs/specs/` and
  `docs/superpowers/` carry a README saying they are published working notes. The FFmpeg trac extracts under
  `skill/ffmpeg/reference/` carry an attribution file and a NOTICE entry; `.printing-press.json`'s double-encoded
  dash is fixed; a `.gitleaksignore` names the two synthetic JWT fixtures.
- **Why.** The repository is the canonical public home of the harness and is being shared; a stranger's first read
  and first run were the test, and each item above is something that read or run reached.
- **Check.** `go build ./... && go vet ./...` clean; the root-package lints that read the changelog, the version
  sources, the example config, the tree's identifiers and the pins pass; GitHub's blob view of `CHANGELOG.md`
  renders after the merge (the pre-change view showed the size refusal).
