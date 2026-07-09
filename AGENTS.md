# AGENTS.md

Vendor-neutral entry point for any AI coding agent working in this repository. (Claude Code also
auto-loads `CLAUDE.md`, which carries the same orientation in more detail.)

## What this repo is

`offload-harness` — a local-first Go harness that delegates short-context, low-judgment work
(summarize / classify / extract / triage, plus vision / OCR / transcription / media generation) to a
free **Gemma-4 cascade** served by llama.cpp. It ships as a CLI, an **MCP server** (`local-offload`),
and an optional local **coding agent** (`local-agent`). It never calls a cloud model; on low
confidence it returns a structured **defer** so the calling agent does that task itself.

## Installing the stack

Point yourself at **`setup/SETUP-AGENT.md`** and follow it exactly. It is an agent-executable
runbook: run `setup/detect.ps1` → `setup/install.ps1` → `setup/selftest.ps1` (Windows, PowerShell),
read each script's final JSON line, and branch on the decision tables. Do **not** substitute pinned
assets, install ROCm/CUDA, or start the unauthenticated agent server beyond loopback without asking
the human.

## Operating the stack

- **Orientation map** (ports, model tiers, golden commands, do-not-break invariants, where things
  live): `CLAUDE.md` in the repo root.
- **Task walkthroughs** (start/stop the stack, chat, run an offload task, drive the coding agent,
  add a model, diagnose failures, update): `docs/OPERATOR-GUIDE.md`.

## Working in the code

- Build: `go build ./...`  ·  Test: `go test ./...`  ·  Vet: `go vet ./...` — all must stay green.
- Go 1.26+. TDD for Go changes (failing test first). Keep changes scoped and minimal.
- Do not break the serving invariants or the agent safety defaults — see `CLAUDE.md` → "Invariants".
