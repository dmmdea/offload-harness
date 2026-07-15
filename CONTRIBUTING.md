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
- **No cloud calls.** The harness never calls a cloud model and holds no cloud credentials by
  design; please keep it that way.

## Versioning

This project follows [SemVer](https://semver.org/). **Three sources name the version and MUST be
bumped together, in the same commit:**

1. the `VERSION` file
2. the `version` const in `main.go` (advertised in the MCP handshake — a stale value misreports the
   server to clients)
3. the top `## [x.y.z]` entry in `CHANGELOG.md`

`TestVersionSourcesAgree` (in `main_test.go`) fails the build if any of the three disagree, so
`go test ./...` catches a partial bump. When you change published behavior, bump the version **and**
add a CHANGELOG entry in the same PR. Never let the three drift — a mismatch has real cost: it once
made the version look lower than a downstream publish and triggered a false "we lost work" fire drill.

## Opening a pull request

1. Fork the repo and create a branch off `main`.
2. Make your change, with `go build ./...`, `go vet ./...`, and `go test ./...` all green.
3. Open a PR describing what changed and why. Link any related issue.

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
