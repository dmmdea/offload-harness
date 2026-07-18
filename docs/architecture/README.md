# Architecture

Cross-system structure, long-lived technical constraints, and the decision record.

## What belongs here

- System-wide patterns that hold across more than one system.
- Durable technical constraints (serving-flag requirements, sandboxing model, repository topology).
- Cross-system boundaries and integration strategies.
- Architectural tradeoffs.
- [Architecture Decision Records](decisions/README.md).

## What does not belong here

- How one system currently works → `../systems/`
- Behavior that moves across systems → `../flows/`
- Ordinary implementation notes, feature documentation, or task walkthroughs → the relevant system
  doc or [../OPERATOR-GUIDE.md](../OPERATOR-GUIDE.md).

## Current top-level structure

The component map — binaries, ports, model tiers, and which directory owns what — lives in
[CLAUDE.md](../../CLAUDE.md) at the repo root, which every Claude Code session auto-loads. It is the
practical orientation map and is kept current as part of normal work; this directory does not
duplicate it.

In short: `local-offload` is one Go binary exposing a CLI and an MCP server, plus `local-agent`, a
separate coding-agent binary. Both talk to model tiers served by llama-swap over a local HTTP
endpoint, and both share the same cascade and policy code in `internal/`. Media generation shells out
to a Node renderer that drives a local ComfyUI instance. No component calls a cloud model.

The durable constraints behind that shape — why there is no cloud fallback, why the agent is
read-only by default, why serving is loopback-only — are recorded in
[decisions/](decisions/README.md).

## Related docs

- [../README.md](../README.md) — documentation index
- [../STYLE.md](../STYLE.md) — how to write docs here, including the ADR schema
