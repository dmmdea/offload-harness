# AGENTS.md

Vendor-neutral entry point for any AI coding agent working in this repository. (Claude Code also
auto-loads `CLAUDE.md`, which carries the same orientation in more detail.)

## What this repo is

`offload-harness` — a local-first Go harness that delegates short-context, low-judgment work
(summarize / classify / extract / triage, plus vision / OCR / transcription / media generation) to a
free **Gemma-4 cascade** served by llama.cpp. It ships as a CLI, an **MCP server** (`local-offload`),
and an optional local **coding agent** (`local-agent`). It never calls a cloud model; on low
confidence it returns a structured **defer** so the calling agent does that task itself.

## Documentation map

Detailed documentation lives in `docs/`. This file only routes you there.

- [`docs/README.md`](docs/README.md) — the index. Start here.
- `docs/systems/` — how each part of the harness works (offload pipeline, coding agent, MCP server,
  media generation, fleet node, installer).
- `docs/flows/` — behavior that crosses systems (cascade escalation and defer, run-graph manifest
  satisfaction, fleet job lifecycle, zero-warm generation).
- [`docs/architecture/decisions/`](docs/architecture/decisions/README.md) — Architecture Decision
  Records. **Only `Accepted` status is current guidance.**
- [`docs/glossary.md`](docs/glossary.md) — terms with a specific meaning here (Defer, Tier,
  Zero-Warm, Node Manifest, …).

**Workflow:**

1. Read the system and flow docs for the area you are about to change.
2. Inspect the source for implementation detail.
3. Make the change.
4. Update the affected docs **in the same pull request** when behavior, responsibilities, flows,
   invariants, interfaces, or glossary concepts change.
5. Make sure docs and code agree before you finish. `go test -run TestDocsLint .` checks structure;
   you check meaning. If they disagree and you cannot resolve it, say so explicitly in the PR.

Do not put detailed system behavior in this file — it belongs in `docs/`.

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
