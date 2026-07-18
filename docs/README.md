# offload-harness documentation

The documentation index. [AGENTS.md](../AGENTS.md) routes you here; this page tells you which door to
open.

Source code stays the implementation source of truth. These docs explain the system at the level you
need before changing it: what each part does, how the important flows work, what must stay true, and
where the code lives.

- **How to write docs here:** [STYLE.md](STYLE.md) · **Shapes to fill in:** [templates/](templates/)
- **Structural gate:** `go test -run TestDocsLint .` — links must resolve, ADR frontmatter must be
  schema-valid, system and flow docs must keep their navigational sections.

## Systems — how each part works

- [systems/offload-pipeline.md](systems/offload-pipeline.md) — the Cascade: tiers, escalation gates,
  grounding, confidence thresholds, defers, the ledger
- [systems/coding-agent.md](systems/coding-agent.md) — `local-agent`: the loop, tools, policy broker,
  budgets, profiles, two-tier mode, sandboxing
- [systems/mcp-server.md](systems/mcp-server.md) — the MCP tool surface, the manifest and its drift
  test
- [systems/media-generation.md](systems/media-generation.md) — image, video, audio, SVG, and editing;
  the GPU lifecycle and model bindings
- [systems/fleet-node.md](systems/fleet-node.md) — `fleet-serve` / `fleet-measure`: the node contract,
  job semantics, VRAM sampling
- [systems/setup-installer.md](systems/setup-installer.md) — hardware detection, profiles, serving
  templates, the install runbook

## Flows — behavior that crosses systems

- [flows/cascade-escalation-and-defer.md](flows/cascade-escalation-and-defer.md) — how a task walks
  the tiers and when it hands back
- [flows/run-graph-manifest-satisfaction.md](flows/run-graph-manifest-satisfaction.md) — provisioning
  node packs and models, then executing a caller's graph
- [flows/fleet-job-lifecycle.md](flows/fleet-job-lifecycle.md) — dispatch, ack, run, poll, and the
  duplicate-dispatch semantics
- [flows/zero-warm-generation.md](flows/zero-warm-generation.md) — the GPU lock, teardown, and warm
  batch

## Architecture and decisions

- [architecture/README.md](architecture/README.md) — cross-system structure and durable constraints
- [architecture/decisions/](architecture/decisions/README.md) — Architecture Decision Records; only
  `Accepted` status is current guidance

## Glossary

- [glossary.md](glossary.md) — domain terms that carry a specific meaning here (Defer, Tier,
  Zero-Warm, Node Manifest, …)

## Operations

Task-oriented operator documentation. These are canonical paths with inbound links from outside the
repo — they keep their locations.

- [OPERATOR-GUIDE.md](OPERATOR-GUIDE.md) — start and stop the stack, chat, run an offload task, drive
  the coding agent, add a model, diagnose failures, update
- [FLEET-NODE.md](FLEET-NODE.md) — running a fleet node (`fleet-serve` / `fleet-measure`), including
  the VRAM-footprint validation procedure
- [ROADMAP.md](ROADMAP.md) — planned direction

Installing the stack is its own runbook: [../setup/SETUP-AGENT.md](../setup/SETUP-AGENT.md).

## Engineering process

The dated working record — design specs, implementation plans, and run/benchmark evidence ledgers —
lives in the **private development repository**, not here. Public releases carry the changelog and
these docs; the reasoning behind them stays in the development record.

## Historical reports

Kept for the record, not maintained. Read them as dated findings.

- [2026-06-26-dependency-model-audit.md](2026-06-26-dependency-model-audit.md)
- [PHASE-S-ik_llama-benchmark-2026-06-16.md](PHASE-S-ik_llama-benchmark-2026-06-16.md)
- [PRINTING-PRESS-EVAL-2026-06-16.md](PRINTING-PRESS-EVAL-2026-06-16.md)
- [MORNING-SUMMARY-2026-06-16.md](MORNING-SUMMARY-2026-06-16.md)

## Viewing

These are plain Markdown files — read them in an editor, on the repo host, or with any Markdown
viewer. `npx mdts` from the repo root serves a clickable browser view of the tree, which makes source
maps and cross-links easier to follow during onboarding or review. It downloads and executes a
package from npm, so review and trust it first; it also cannot yet render non-Markdown link targets,
so source-file links will not open inside it.
