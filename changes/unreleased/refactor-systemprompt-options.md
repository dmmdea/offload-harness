### Changed - `agent.SystemPrompt` takes one `PromptCaps` struct instead of seven positional bools

- **What changed.** `SystemPrompt(allowWrite, allowOverwrite, allowFetch, allowShell, runGranted, allowSearch, allowGitHub bool)`
is now `SystemPrompt(caps PromptCaps)`, with one named field per former bool. The one production caller (`builder.go`) and the
existing prompt tests name their fields; the prompt text is unchanged.
- **Why.** A call site with seven bare bools could not be read, and two adjacent bools swapped silently.
- **Check.** `go build ./...`, `go vet` and `go test` on `internal/agent` and `cmd/local-agent` pass with no test edited beyond the call syntax.
