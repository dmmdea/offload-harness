---
status: Accepted
date: "2026-07-18"
---

# Loopback-only serving unless explicitly opted out

## Context

Two components in this repository listen on a socket: `local-agent --serve`, which exposes an
OpenAI-compatible endpoint backed by the coding agent, and `fleet-serve`, which exposes the fleet
node contract so a dispatcher can send it work.

Neither is authenticated. The agent endpoint in particular drives write and GitHub tools when those
capabilities are granted, so an unauthenticated listener reachable from the network is a remote code
execution surface.

The convenient default — bind everything, sort out access later — is exactly the one that gets
shipped and forgotten.

## Decision

Both servers refuse to bind anywhere but loopback unless the operator explicitly passes
`--listen-trusted-network`, which prints a loud warning when used.

The check is one shared implementation, `internal/netguard`, extracted so the two servers cannot
drift apart. It is deliberately strict in two ways worth knowing:

- **An unparseable address is refused**, on the grounds that if we cannot prove it is loopback, it is
  not loopback.
- **An empty host is refused.** `:18811` and `""` look local but make Go bind every interface, so
  they are treated as non-loopback. This is the case that would otherwise slip through.

`localhost` by name, any address in `127.0.0.0/8`, and `[::1]` pass.

`fleet-serve` additionally refuses to start without a working GPU probe, because advertising a
zero-VRAM node would make the dispatcher treat the box as broken rather than absent.

## Consequences

- Exposing either endpoint is a deliberate, visible act with a warning attached, not a default.
- Cross-machine fleet work requires passing the flag on each node. This is friction on purpose; the
  fleet contract assumes a trusted network (a tailnet), and the flag is where that assumption is
  acknowledged.
- `:PORT` shorthand does not work for binding beyond loopback, which surprises people. The refusal
  message explains it.
- Adding a third listener means reusing `netguard.Validate`, not writing a third check.

## Alternatives considered

- **Bind loopback with no override at all.** Rejected: the fleet node genuinely must be reachable
  from another machine, so an escape hatch has to exist. Making it explicit and loud is the
  compromise.
- **Adding authentication instead of a bind restriction.** Not rejected on the merits — it is simply
  a larger change, and a bind restriction is the cheap correct default in the meantime. If these
  endpoints ever need to cross an untrusted network, authentication becomes the prerequisite and this
  ADR should be superseded rather than quietly relaxed.
- **Treating `:PORT` as loopback** for convenience. Rejected: it is the single most likely way to
  accidentally expose the endpoint.

## Related code

- [`internal/netguard/netguard.go`](../../../internal/netguard/netguard.go) — the shared check
- [`cmd/local-agent/serve.go`](../../../cmd/local-agent/serve.go) — agent endpoint
- [`main.go`](../../../main.go) — `fleet-serve` parameter resolution and GPU probe

## Related docs

- [0003-policy-broker-and-capability-flags-off-by-default.md](0003-policy-broker-and-capability-flags-off-by-default.md)
- [../../FLEET-NODE.md](../../FLEET-NODE.md) — operating a fleet node
