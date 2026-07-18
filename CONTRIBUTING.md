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

## Guidelines

- **Keep changes scoped.** One focused change per PR. Avoid drive-by refactors, renames, or
  reformatting unrelated code — they make review harder and bury the actual change.
- **Match the existing style.** Run `gofmt` (or `go fmt ./...`) before committing.
- **Add or update tests** for any behavior you change.
- **Conventional-ish commit messages.** Prefix with the kind of change — `feat:`, `fix:`,
  `docs:`, `test:`, `refactor:`, `chore:` — followed by a short imperative summary.
- **No cloud calls in the cascade.** The cascade never calls a cloud model and holds no cloud
  credentials by design — on low confidence it returns a structured defer and the caller does the
  task. Please keep it that way. (`offload_nim` is the single, explicit, caller-invoked remote tool;
  nothing escalates or falls back into it. See
  [ADR 0001](docs/architecture/decisions/0001-defer-never-cloud-fallback.md).)

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

## Versioning

This project follows [SemVer](https://semver.org/). **Four sources name the version and MUST be
bumped together, in the same commit:**

1. the `VERSION` file
2. the `version` const in `main.go` (advertised in the MCP handshake — a stale value misreports the
   server to clients)
3. the top `## [x.y.z]` entry in `CHANGELOG.md`
4. the `version` field in `.printing-press.json` (the MCP manifest — it also declares the tool list,
   which a drift test checks against what the code actually registers)

`TestVersionSourcesAgree` (in `main_test.go`) fails the build if any of these disagree, so
`go test ./...` catches a partial bump. When you change published behavior, bump the version **and**
add a CHANGELOG entry in the same PR. This repository is the canonical source of version authority.
Never let the sources drift — a mismatch once made the version look lower than a separately-published
copy and triggered a false "we lost work" fire drill.

## Opening a pull request

1. Fork the repo and create a branch off `main`.
2. Make your change, with `go build ./...`, `go vet ./...`, and `go test ./...` all green.
3. Open a PR describing what changed and why. Link any related issue.

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
