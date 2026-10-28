# Contributing to offload-harness

Thanks for your interest in improving offload-harness — a local-first harness that offloads
short-context, low-judgment work to a free local Gemma-family cascade. Contributions of all
sizes are welcome.

## Build & check

This is a single-module Go project. From the repository root:

```bash
go build ./...   # build everything (CLI + MCP server are the same binary)
go vet ./...     # static analysis — keep this clean
```

Both must pass before you open a pull request. CI runs the test suite and is the gate: do not run the full
suite by hand, and see "Testing rule" below for what a change carries instead of tests.

The `go` line in `go.mod` is a floor, the oldest compiler the harness builds with; there is no `toolchain`
line, so nothing holds the compiler to an exact release. With `GOTOOLCHAIN=auto` a Go older than the floor
downloads a newer one on the first build in this tree (from the module proxy, into the module cache), and
CI's `setup-go` reads the floor from `go.mod` and takes the newest release (`check-latest`).
`GOTOOLCHAIN=local` builds with the installed Go instead, which is the way to build offline. CI also scans
the harness module and both printed CLIs for known vulnerabilities, for linux and windows, in the
`govulncheck` job. To run one scan by hand: `go install golang.org/x/vuln/cmd/govulncheck@latest`, then
`govulncheck ./...` in the module's directory (`GOOS=windows govulncheck ./...` for the other
system). A finding fails the job; the fix is a newer dependency or a newer Go (ADR 0099: versions are never
pinned, floors live in `go.mod` and `setup/floors.json`).

## Guidelines

- **Keep changes scoped.** One focused change per PR. Avoid drive-by refactors, renames, or
  reformatting unrelated code — they make review harder and bury the actual change.
- **Match the existing style.** Run `gofmt` (or `go fmt ./...`) before committing.
- **No new tests.** A change carries none; see "Testing rule" below.
- **Conventional-ish commit messages.** Prefix with the kind of change — `feat:`, `fix:`,
  `docs:`, `test:`, `refactor:`, `chore:` — followed by a short imperative summary.
- **No cloud calls in the cascade.** The cascade never calls a cloud model and holds no cloud
  credentials by design — on low confidence it returns a structured defer and the caller does the
  task. Please keep it that way. (`offload_nim` is the single, explicit, caller-invoked remote model
  tool; nothing escalates or falls back into it. The one other exception is the opt-in browse lane:
  it calls only a loopback decision endpoint the operator runs, holds no key, and that endpoint may
  front a hosted model, so visible page text can leave the machine. See
  [ADR 0001](docs/architecture/decisions/0001-defer-never-cloud-fallback.md) and
  [ADR 0060](docs/architecture/decisions/0060-opt-in-browse-lane-drives-the-operators-browser.md).)

## Testing rule

Canonical text: [`AGENTS.md`](AGENTS.md#testing-rule). It replaces the earlier red-first, mutation-check, seam-test and
review-round practice (superseded 2026-10-18). In short:

- **A change carries no new tests** (unit, regression, golden, fixture, lint, self-test, mutant or test plan). The one
  exception is a change that cannot be seen working any other way: it gets the single smallest check that shows it works.
- **CI as it stands is the gate.** Do not run mutation checks, red-first proofs, coverage hunts, repeat-until-clean
  cycles, or full-suite, multi-host or remote-gate runs beyond what CI already runs.
- **The check is one real run of the change** (the real command, the deployed binary, the live seat, the rendered file),
  run once and read back. Say in the pull request what you ran and what you saw.
- **Review is at most one pass, and only for credentials/security or data loss.** Fix only what a real input reaches;
  drop low-severity findings and findings that need an input no real system produces.
- **A test your change turns red** is fixed in the code if real behaviour broke; otherwise edit or delete the test.
  Never replace it with a new one.
- Existing tests, scripts (`scripts/mutation-check.sh` and its self-test, `scripts/test-temp-tripwire.sh`) and CI config
  are left as they are until the maintainer prunes them in a separate change.

Two lints in the root package run in CI and still bind any test that is edited; nothing here asks for a new test.
`tempdir_lint_test.go` fails on a test that reaches for the OS temp dir (`os.MkdirTemp` or `os.CreateTemp` with an empty
directory, `os.TempDir`, `TMP`, `TEMP` or `TMPDIR` read from the environment) unless `tempdir_lint_allowlist_test.go` lists
it; use `t.TempDir()`. `clock_lint_test.go` fails on an upper bound under one second on a measured elapsed time unless
`clock_lint_allowlist_test.go` lists it; wait on the event the product signals, not on a short clock window.

## Documentation

Documentation lives in [`docs/`](docs/README.md) and is part of the change, not a follow-up.

- **Read before you change.** Start at [`docs/README.md`](docs/README.md): `systems/` explains how
  each part works, `flows/` covers behavior crossing systems, `architecture/decisions/` records why
  things are the way they are (only `Accepted` ADRs are current guidance), and
  [`glossary.md`](docs/glossary.md) defines terms that mean something specific here.
- **Update in the same PR.** A change affecting responsibilities, observable behavior, interfaces,
  data, configuration, error handling, security, invariants, testing expectations, or a glossary
  concept must update the affected docs in that same pull request.
- **Docs and code must agree.** When they disagree, do one of three things — never nothing: update
  the docs to match the code, update the code to match the documented intent, or call out the
  mismatch explicitly in the PR. Reviewers compare the two; that is what makes the docs useful during
  review.
- **Verify what you write.** Check each factual claim against the source at the time of writing. Mark
  genuine uncertainty with `> **Unverified:**` rather than guessing.
- **Structural gate:** CI's `TestDocsLint` checks that scaffold files exist, relative links
  resolve, ADR frontmatter is schema-valid, and system/flow docs keep their required sections. It
  checks structure only — meaning is a review duty.
- Conventions, the ADR schema, and the privacy rules for published files are in
  [`docs/STYLE.md`](docs/STYLE.md).

## The leak gate

This repository is public, and the names of the maintainer's machines, people and brands must never reach
it. `go test ./...` includes a gate that scans every tracked file and file name against a list of denied
names. The list is secret: the repository carries only keyed hashes of it (`testdata/leak-gate-digests.json`),
and a failure prints the path, the line and an opaque entry id, never the text.

- **If it fails on your change,** rewrite the text with the repository's vocabulary (a node letter or a role
  phrase; [`docs/STYLE.md`](docs/STYLE.md#privacy), rules V1 to V4) and push again. There is no allow list.
  A test or fixture that must show a denied name builds it from parts at run time, because the gate scans test
  sources too.
- **From a fork, the keyed test skips.** GitHub withholds the key from a fork's pull request, so `go test
  ./...` there says `leak gate: skipped` in the run summary. The keyless tests (the shape rules, the key table,
  the digest file's own checks) still run. A maintainer runs the keyed test on your checkout before merging,
  and the push run on `main` is the backstop.
- **A maintainer runs it with the key file:** `OFFLOAD_LEAK_GATE_KEY_FILE=<key file> go test -count=1 -run
  TestTrackedTreeCarriesNoDeniedNames .` (`-count=1`, because the key file is outside the module and Go's test
  cache cannot see it change).

How it works, how the key is handled and what it does not cover:
[`docs/systems/leak-gate.md`](docs/systems/leak-gate.md).

## Versioning

This project follows [SemVer](https://semver.org/). **Four sources name the version and MUST be
bumped together, in the same commit:**

1. the `VERSION` file
2. the `Version` const in `internal/buildinfo/buildinfo.go` (advertised in the MCP handshake — a
   stale value misreports the server to clients). `main.go` aliases it as `version`; the fleet and
   pipeline stampers read `buildinfo.Version` directly, so that file is the one to edit.
3. the top `## [x.y.z]` entry in `CHANGELOG.md`, produced by `go run ./cmd/changelog assemble` from the
   changelog fragments (below), never typed by hand
4. the `version` field in `.printing-press.json` (the MCP manifest — it also declares the tool list,
   which a drift test checks against what the code actually registers)

`TestVersionSourcesAgree` (in `main_test.go`) fails the build if any of these disagree, so
`go test ./...` catches a partial bump. When you change published behavior, add a changelog fragment in
the same PR (below). The version is bumped where the change is released: a direct pull request to `main` is
its own release, so it bumps and assembles the changelog as well; a feature branch that is merged into a
release branch leaves the bump and the assembly to that branch. This repository is the canonical
source of version authority.
Never let the sources drift — a mismatch once made the version look lower than a separately-published
copy and triggered a false "we lost work" fire drill.

## The changelog is one file per change

A change adds **one new file**, `changes/unreleased/<slug>.md` (the lower-case branch name with `/` written as
`-`), and never edits `CHANGELOG.md`. The body is exactly the text that will appear in the changelog and opens
with `### <Kind> - <title>`; no line may start with `# ` or `## `, and no line may carry a release date between
separators. Two branches add two files, so they cannot
conflict on the changelog, and the `## [Unreleased]` block of `CHANGELOG.md` stays empty (`go test` fails if
anything is written there). The format, the rules and the recovery steps are in
[`changes/README.md`](changes/README.md); `go run ./cmd/changelog check` runs the same rules the tests do.

There are two ways a change reaches `main`, and the changelog takes part in the release step of both:

* **A direct pull request to `main` that is also a release.** Add your fragment, bump the version in
  `VERSION`, `internal/buildinfo/buildinfo.go` and `.printing-press.json`, and run
  `go run ./cmd/changelog assemble -title "<one line>"` as the **last** commit of the pull request. Your own fragment
  is added and consumed inside the same pull request, so it needs no `Deletes:` line. But `assemble` consumes every
  file in `changes/unreleased/`: if a fragment-only pull request left a fragment on `main` earlier, paste the
  `Deletes:` line that `assemble` prints into the commit message, as in the next item.
* **A release branch (`release/X.Y.Z`).** Feature branches add fragments only. On the release branch, merge them,
  then `git merge origin/main`, renumber the three version carriers, and run `assemble` **last**. Commit it with the
  printed `Deletes:` line only if some fragment reached `main` earlier through a fragment-only pull request that did
  not bump the version (the deletion guard below then sees `main`'s file go).

`assemble` is last because it writes the top of `CHANGELOG.md`: a merge of `main` or a renumbered version after it
conflicts in the hunk the tool wrote. If `main` moves after the assembly, revert the release commit (the fragments
come back), merge `main`, renumber, and assemble again; `changes/README.md` has the commands. This removes the
conflicts between feature branches and the overwritten release heading; two *releases* still conflict on the
version carriers and the top of the changelog.

## Deleting a file is a decision you have to declare

`go build`, `go vet` and `go test` all read the tree they are handed. A tree that is simply
**missing** a merged feature passes every one of them, so nothing in this repo used to notice work
disappearing. In 0.123.x a branch was rebuilt by replaying its trees onto an older base and merged:
its first parent was the release before it, its tree was not, and six files from two merged,
reviewed, tested pull requests vanished in one commit with CI green (register H-51; the repair is
0.123.2).

`scripts/check-deletions.sh` (run by the CI `build` job, and exercised by
`TestDeletionGuardRefusesAnUndeclaredDeletion`) now reads what a change **lost**. It runs twice, for
the two different failure shapes:

| when | compares | catches |
|---|---|---|
| pull request | the base branch tip → your branch | a branch that removes a file `main` has |
| push to `main` | the merge commit's **first parent** → the merge commit | a merge whose tree drops what its own parent carried — the shape above |

If a deletion is intended, declare it — any one of these is enough, and they cost one line:

* a `Deletes: path/one.go, path/two.go` line in the pull request body,
* a `Deletes: path/one.go` line in a commit message on the branch,
* the paths listed in a `DELETIONS` file at the repo root (for a large, deliberate removal).

If the guard fires and you did *not* mean to delete anything, your branch is missing work that is
already on `main`. Merge `main` into it — never force the merge, never replay the branch onto an
older base — and run again. Run it locally the same way CI does:

```sh
bash ./scripts/check-deletions.sh origin/main HEAD
```

On Windows run it from Git Bash. The test above does not depend on which shell started `go test`: it finds Git's own
bash from `git --exec-path`, puts Git's `usr/bin` on the guard's PATH (a plain PowerShell has only git's `cmd`
directory there, and the script calls `cut` and `grep`) and passes the script as a forward-slash path, because on a
plain PowerShell PATH the name `bash` is the WSL launcher, which drops the backslashes of a Windows path and so never
finds the script. When the only bash on the machine is that launcher, the test skips and says so.

## Opening a pull request

1. Fork the repo and create a branch off `main`.
2. Make your change, with `go build ./...` and `go vet ./...` clean. CI runs the tests and is the gate; do not add
   tests (see "Testing rule").
3. If you changed anything under `openspec/` (see [docs/systems/openspec.md](docs/systems/openspec.md)), run the OpenSpec
   validator too: install the newest CLI with `scripts/openspec-install.sh <prefix>` (a directory on a data drive), then
   `OPENSPEC_BIN=<prefix>/node_modules/.bin/openspec scripts/openspec-validate.sh`. `go test ./...` runs the real validator
   only where `OPENSPEC_BIN` is set, and CI does not set it, so this step is yours.
4. If you changed published behavior, add a changelog fragment under `changes/unreleased/` (see "The
   changelog is one file per change" above); do not edit `CHANGELOG.md`.
5. Open a PR describing what changed and why, and the one real run you made and what you saw. Link any related issue.
6. If it removes any tracked file, say so with a `Deletes:` line (see above) — CI refuses an
   undeclared deletion.

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
