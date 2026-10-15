### Fixed - offload_ask's docs say what a named `route:"remote"` does with the engine's retry

- **A named remote keeps the local fallback.** The cross-seat retry change kept an ask's own retry off this box, and an ask
  that names `route:"remote"` still reaches the engine as an unreasoned call: its first placement is a fleet node and the
  engine's retry may come back to this box's seat, like every other door without a reason channel (ADR 0078). The
  `offload_ask` section of `docs/systems/mcp-server.md` now says so, and points a caller that needs a strictly remote run
  at `agent_delegate` with a `pin_reason`. Docs only.
