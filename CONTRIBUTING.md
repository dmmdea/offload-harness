# Contributing to local-offload

Thanks for your interest in improving local-offload — a local-first harness that offloads
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

## Opening a pull request

1. Fork the repo and create a branch off `main`.
2. Make your change, with `go build ./...`, `go vet ./...`, and `go test ./...` all green.
3. Open a PR describing what changed and why. Link any related issue.

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
