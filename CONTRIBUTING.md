# Contributing to offload-harness

Thanks for your interest in improving offload-harness — a local-first harness that offloads
short-context, low-judgment work to a free local Gemma-family cascade. Contributions of all
sizes are welcome.

## Build & test

This is a single-module Go project. From the repository root:

```bash
go build ./...   # build everything (CLI + MCP server are the same binary)
go vet ./...     # static analysis — keep this clean
go test ./...    # run the full test suite
```

All three must pass before you open a pull request. The tests do not require a running
llama.cpp server.

`go.mod` names the compiler the harness ships with (`toolchain go1.27.2`; the `go` line is only the
oldest one allowed). With `GOTOOLCHAIN` at its default, a Go older than that downloads the release
on the first build in this tree (from the module proxy, into the module cache) and builds with it.
`GOTOOLCHAIN=local` builds with the installed Go instead and ignores the line without a message,
which is the way to build offline. CI does not build that way: its `build` and `installer-windows`
jobs fail unless the go command runs exactly the release the line names. CI also scans the harness
module and both printed CLIs for known vulnerabilities, for linux and windows, in the `govulncheck`
job. To run one scan by hand: `go install golang.org/x/vuln/cmd/govulncheck@v1.8.0`, then
`govulncheck ./...` in the module's directory (`GOOS=windows govulncheck ./...` for the other
system). A finding fails the job; the fix is a newer toolchain pin in `go.mod` (and the floor in
`gomod_toolchain_pin_test.go`) or a newer dependency.

## Guidelines

- **Keep changes scoped.** One focused change per PR. Avoid drive-by refactors, renames, or
  reformatting unrelated code — they make review harder and bury the actual change.
- **Match the existing style.** Run `gofmt` (or `go fmt ./...`) before committing.
- **Add or update tests** for any behavior you change.
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

## Proving a test can fail

A test that has only ever passed proves nothing, so a fix or a new gate is shown to work by making it fail on purpose:
apply one single-line mutation that removes the control and watch the test go red. Do that with
[`scripts/mutation-check.sh`](scripts/mutation-check.sh) (PowerShell twin: [`scripts/mutation-check.ps1`](scripts/mutation-check.ps1)),
not with an ad hoc `sed` plus `go test` that counts any failure as a kill. From the module root it takes the file, the
mutation (`--old`/`--new`, or `--marker` to pick the line) and either a Go package (`--pkg ./internal/x --run TestX`) or your
own `--test` and `--compile` commands, and it runs the unmutated control arm, which must pass; applies the mutation and asserts
it landed, comparing with line endings normalised so a CRLF file cannot fake a pass; compiles the test binary before
running the tests, so a mutant that does not build is reported `BAD-MUTANT` and never counted as a kill; reports `RED` (the
tests caught it) or `SURVIVED` (they did not); and restores the file byte for byte, even when interrupted. Quote the final
`MUTATION-VERDICT:` line, with the mutation it printed, in the pull request. `bash scripts/mutation-check-selftest.sh` (or
`pwsh -File scripts/mutation-check-selftest.ps1`) shows every verdict on a fixture under `scripts/testdata/`.

## Tests that certify a seam

A seam is the place where one part of the harness hands work to another: a function variable or an interface that production
code fills in, a config key that picks an engine, a door that calls a lane. A test that certifies a seam says the two sides
are wired together and behave across it, and it proves nothing if it injects the very thing it certifies. A test that assigns
a stand-in to the seam, runs the code, and asserts on the value it just put there passes with the production wiring deleted;
a mutation of exactly that kind once survived a green seam test here. It matters most in red-first work: the behaviour-free
stub that lets the first test compile is what the test must go red against, not what it certifies.

- **Fake the far side of the seam, never the seam.** Stand in for what lies beyond it (a node, an engine, a clock source) and
  let the production code on the near side run, so the path under test is the path production takes.
- **Assert on something the production path produces,** not on a value the test supplied, and not on a count of calls to a
  stand-in that only the test drives. You must be able to name the production line whose removal turns the test red.
- **Show that it goes red.** Remove or neutralise that line with `scripts/mutation-check.sh` (see "Proving a test can fail")
  and quote the `RED` verdict in the pull request; a seam test whose mutation `SURVIVED` is certifying its stand-in.
  `TestRunNoEligibleRemoteNamesTheRealCause` in `internal/delegate/run_test.go` is a worked example: it drives `Run` against
  fake nodes (or none) and asserts the reason and the `Unplaced` flag that the production path reports.
- **Reviewer's check:** for each test that claims to cover a wiring, ask which production line it goes red without. If the
  answer is none, or only a line inside the test, it does not certify the seam.

## Keeping tests out of the OS temp dir

A test takes its scratch space from `t.TempDir()`, which `testing` removes when the test ends. A test that makes a folder
or file in the OS temp dir (C: on Windows) and does not remove it on every path adds one more every run; a stand-in test
binary once left 22,434 folders and about 70 GB in under two hours. Two things guard this, and neither replaces the other.

- **The lint, `tempdir_lint_test.go` in the root package.** It reads every `*_test.go` file of the module and fails on a
  direct use of the OS temp dir that `tempdir_lint_allowlist_test.go` does not list with its count and the reason its folder
  is gone on every path: `os.MkdirTemp` / `os.CreateTemp` / `ioutil.TempDir` / `ioutil.TempFile` with an empty directory,
  any `os.TempDir`, `TMP`, `TEMP` or `TMPDIR` read from the environment, and `os.MkdirTemp` passed as a value. It also
  requires every package whose tests start a copy of their own test binary to hold
  `TestARelaunchedTestBinaryLeavesNothingInTheTempDir` (see `internal/mediaremote/relaunch_leak_test.go`) or to be listed in
  `relaunchExempt` with the reason a started copy leaves nothing. Prefer `t.TempDir()` to a new entry. To regenerate the
  keys, run `TEMPDIR_LINT_PRINT=1 go test . -run 'TestTestsReachForTheOSTempDirOnlyWhereItsRemovalIsOnEveryPath|TestEveryRelaunchingPackageProvesItLeavesNothing' -v`
  and write the reasons after reading the code.

  Six suites make a throwaway state root in their `TestMain` (the root package, `internal/delegate`, `internal/mcpserver`,
  `internal/mediaremote`, `internal/pairworkloads` and `internal/pipeline`), and the same two rules hold for all six. A
  removal that fails is tried once more after 250 ms; a root that is still there is named on stderr with the error, and a
  suite that passed then exits 1, because `go test ./...` shows the stderr of a package that passed only with `-v` (a
  suite that failed keeps its own exit code). In the root package, a copy of the test binary that a test starts with
  `LOCAL_OFFLOAD_STATE_DIR` taken out of its environment (the `gpu reserve` tests) keeps its own state root inside the
  suite's root, which the suite removes, and says on stderr when it has to fall back to the OS temp dir.
- **The tripwire, `scripts/test-temp-tripwire.sh`** (PowerShell twin `scripts/test-temp-tripwire.ps1`). A lint cannot see a
  temp file made by production code that a test calls, or a copy of the test binary that exits before its cleanup. The
  tripwire points `TMP`, `TEMP`, `TMPDIR` and `GOTMPDIR` at a fresh folder, runs `go test -count=1`, and ends with a
  `TRIPWIRE-VERDICT` line: `CLEAN`, `LEAK` (what is left, by name prefix with a count), `PROCESSES` (an image still running
  from the folder) or `TESTS-FAILED`. `--dir` is required and must not be on the system drive (it refuses with `REFUSED`
  unless `--allow-system-drive` is given). A leaver that is legitimate (there is none today, so the list is empty) is named in
  `scripts/test-temp-tripwire.ignore`, one `NAME-PREFIX`, a tab and a reason per line: the tripwire prints it with its reason and
  does not count it, so an ignore is never silent. A whole-module run loads the machine, so run it on a spare host with
  `nice -n 19 bash scripts/test-temp-tripwire.sh --dir <data drive folder> --go-args "-p 1 -timeout 45m"`, or pass `--pkgs` a subset. The timeout matters: `internal/pipeline` takes
  about ten minutes, which is go test's default timeout, and a test binary that times out is killed before it cleans up, so its temp
  state would read as a leak.
  `bash scripts/test-temp-tripwire-selftest.sh` (or `pwsh -File scripts/test-temp-tripwire-selftest.ps1`) shows every verdict on
  the fixture module `scripts/testdata/tripwirefixture`. When go test fails, the failing tests and their messages are printed
  ahead of the tail of the output, because go test prints one `ok` line per later package; an interrupted run (Ctrl-C, `kill`)
  stops go test and every test binary it started, and exits 130. The tripwire sees only what a child writes under `TMP`, `TEMP`
  and `TMPDIR`: a child started with a scrubbed environment, or a test that names a fixed directory such as `/dev/shm`, writes
  elsewhere and a `CLEAN` says nothing about it (the lint's relaunch-proof test is what covers the scrubbed copy).

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
- **Structural gate:** `go test -run TestDocsLint .` checks that scaffold files exist, relative links
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
2. Make your change, with `go build ./...`, `go vet ./...`, and `go test ./...` all green.
3. If you changed anything under `openspec/` (see [docs/systems/openspec.md](docs/systems/openspec.md)), run the OpenSpec
   validator too: install the pinned CLI with `scripts/openspec-install.sh <prefix>` (a directory on a data drive), then
   `OPENSPEC_BIN=<prefix>/node_modules/.bin/openspec scripts/openspec-validate.sh`. `go test ./...` runs the real validator
   only where `OPENSPEC_BIN` is set, and CI does not set it, so this step is yours.
4. If you changed published behavior, add a changelog fragment under `changes/unreleased/` (see "The
   changelog is one file per change" above); do not edit `CHANGELOG.md`.
5. Open a PR describing what changed and why. Link any related issue.
6. If it removes any tracked file, say so with a `Deletes:` line (see above) — CI refuses an
   undeclared deletion.

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
