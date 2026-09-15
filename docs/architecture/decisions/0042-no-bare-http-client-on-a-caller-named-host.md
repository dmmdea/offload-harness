---
status: Accepted
date: "2026-09-14"
---

# 0042 — No bare HTTP client on a caller-named host

Release: 0.117.3 (register K-01)

## Context

The research lane (`offload_research` / `local-offload research`) fetches pages the CALLER names.
`internal/research.ValidateURL` did the right first half: http(s) only, `localhost` / `.local` /
`.internal` / the tailnet zone refused by name, then `net.DefaultResolver.LookupIPAddr` and a
public-address check on every answer.

Then it threw the answer away. The fetch reconnected **by name** through a bare `&http.Client{}` —
default transport, no dial-time hook — so the operating system resolved the host a second time, a
few milliseconds later, entirely outside the guard. That is a textbook DNS-rebinding window, and
the attacker controls both sides of it: they serve their own name with a one-second TTL, answer
`93.184.216.34` for the lookup the harness validates, and answer `127.0.0.1` for the lookup the
transport makes. Redirects were re-validated the same way — by name — so hop two had the identical
window, reachable through any open redirect on a legitimate host.

A test written against the real fetch path (`internal/research/fetch_rebind_test.go`) made it
concrete rather than theoretical:

```
--- FAIL: TestFetchRefusesDNSRebindToLoopback (0.00s)
    the loopback listener accepted 1 connection(s): the rebound address was dialed
    dialer was asked to connect to [rebind-target.example:65239]
    fetch returned page text "fleet admin secret" from a loopback service
--- FAIL: TestFetchRefusesRebindOnARedirectHop (0.00s)
    the loopback listener accepted 1 connection(s) on the redirect hop
    redirect rebind not refused: {... Text:fleet admin secret}
```

Two aggravating facts. First, the harness already knew the answer twice over: the agent lane's
`safeControl` dial-time hook and `netguard.SafeDialContext`'s resolve-and-pin both close exactly
this window, and the research lane sat between them without either. Second, the research lane's
own address predicate was a weaker second copy: it missed `0.0.0.0/8`, `240.0.0.0/4`, the
TEST-NETs, `192.0.0.0/24`, `192.88.99.0/24`, NAT64 (`64:ff9b::/96`) and 6to4 (`2002::/16`) — so
even at validate time it graded `64:ff9b::7f00:1` as a public address. Two predicates meant two
answers to one question, and the lane with the weaker one was the lane facing the open web.

## Decision

**A client that dials a host named by a caller goes through netguard's pinned transport. No bare
`http.Client`, no default transport, no `http.Get`.** House rule, tree-wide.

Concretely:

1. `internal/netguard/publicnet.go` holds the public-web guard, the third sibling of the listen
   guard (`netguard.go`) and the tailnet guard (`tailnet.go`):
   - `LookupIP` — the ONE resolution seam in the tree. Both dial guards and the research lane's
     URL validation call it, so a single test hook pins DNS for all three, and a rebinding test
     can answer differently per call.
   - `CheckPublicIP` / `PublicIP` — the ONE public-address predicate in the tree, carrying the
     full reserved-range set. `internal/agent`'s `isDisallowedIP`/`blockedNets` moved here
     verbatim-plus-additions; `internal/research`'s `publicIP` is deleted.
   - `PublicDialControl` — the `net.Dialer.Control` hook (fires after DNS, before `connect(2)`).
     This IS the agent lane's old `safeControl`, moved.
   - `PublicDialContext` — resolve-and-pin around any `DialFunc`: the name is resolved here, every
     answer is judged, and the wrapped dialer receives the vetted **IP literal**.
   - `PublicTransport` — clones a caller's transport (or `http.DefaultTransport`), routes
     `DialContext` and, when set, `DialTLSContext` through `PublicDialContext`, and strips `Proxy`.
2. `research.Fetch` builds its client with `netguard.PublicTransport`, so the first hop and every
   redirect hop dial the address that was validated.
3. `ValidateURL` stays, and its doc comment now says what it is: the NAME half. It refuses scheme,
   `.local`, `.internal` and the tailnet zone — things an address cannot express — and it refuses
   without spending a connection. It is explicitly documented as insufficient alone.

A mixed DNS answer (one public record beside one reserved record) is refused **whole** on the
public lane. That matches what `ValidateURL` already did with a mixed answer, and on the public web
a mixed answer is an attack. The tailnet guard deliberately does the opposite — it skips the
poisoned record and dials the good one — because there one injected record would otherwise let an
attacker DoS a real seat.

## Consequences

- The research lane can no longer be aimed at a loopback service, a fleet node's admin endpoint, a
  cloud metadata address, or an RFC1918 host, by any DNS trick, on any hop.
- One predicate, one seam, one transport: a range added to `reservedNets` protects every lane at
  once, and `internal/netguard/publicnet_test.go` is the spec for what "public" means.
- `Options.Client`'s contract changed: the transport is no longer used as-is, it is wrapped. A test
  that injects a stub `RoundTripper` (not an `*http.Transport`) now gets `http.DefaultTransport`'s
  settings with the gate, because the dial gate cannot be optional. Injecting a `DialContext` is
  the supported seam.
- A caller that sets `DialTLSContext` receives the vetted IP literal instead of the hostname and
  must carry its own `TLSClientConfig.ServerName`. Nothing in the tree does; the alternative — an
  unwrapped dial path — is a hole.
- Connection reuse is unaffected: a pooled connection was vetted at the dial that created it.
- Cost: two lookups per fetch (validate, then dial). That is the price of the property, and it is
  the same price `SafeDialContext` already pays on the tailnet side.

## Alternatives considered

- **Re-validate the name on every redirect and call it done.** That is what the code did. It closes
  nothing: the window is between the last validation and the socket, not between hops.
- **Only the `Control` hook, no resolve-and-pin.** `Control` fires after the transport's own DNS
  and does defeat rebinding — but only when the transport builds its dialer from a `net.Dialer` we
  own. A caller-supplied `DialContext` (or a future `DialTLSContext`) escapes it. Pinning the
  address is the property that does not depend on what sits below.
- **A single allowlist of permitted hosts, as the agent lane has.** Right for the agent lane, wrong
  here: the research lane's whole purpose is that the operator names arbitrary public URLs.
- **Leave the two predicates and just widen the research one.** Rejected on the record: the reason
  the research copy was weak is that it was a copy. Two answers to one question is the defect.

## Related code

- [`internal/netguard/publicnet.go`](../../../internal/netguard/publicnet.go) — the guard
- [`internal/netguard/publicnet_test.go`](../../../internal/netguard/publicnet_test.go) — the predicate's spec
- [`internal/research/fetch.go`](../../../internal/research/fetch.go) — the lane that was open
- [`internal/research/fetch_rebind_test.go`](../../../internal/research/fetch_rebind_test.go) — the regression
- [`internal/agent/fetchtool.go`](../../../internal/agent/fetchtool.go) — the lane the guard came from
- [`internal/netguard/tailnet.go`](../../../internal/netguard/tailnet.go) — the same shape, pointed inward

## Related docs

- [ADR 0031 — Harness-first is enforced, and web research is a harness lane](0031-harness-first-is-enforced.md)
- [ADR 0036 — The agent lane is a harnessed environment](0036-the-agent-lane-is-a-harnessed-environment.md)
- [systems/mcp-server.md — the research lane](../../systems/mcp-server.md)
- [systems/coding-agent.md](../../systems/coding-agent.md)
