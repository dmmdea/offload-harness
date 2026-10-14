### Security - an ingress guard on the fleet node, `fleet-ui` and `local-agent --serve` (register SF-16)

A web page in the operator's browser could reach the harness's own listeners. Measured before the guard:
- The fleet node answered 200 to a DNS-rebinding `Host` (`attacker.example`) and to a foreign `Origin`.
- The queue lane took a cross-site `text/plain` POST, which needs no CORS preflight.
- `local-agent --serve` decoded JSON from any `Content-Type`, so such a POST could drive its tool-using agent.

A new key, `fleet_ingress_guard`, puts one guard (`netguard.IngressGuard`) in front of all three listeners:
- `Host` must pass the rule a configured endpoint passes: loopback, a tailnet address, a dotless MagicDNS name, a
  name under **any** zone in `tailnet_suffix` or `tailnet_suffixes`, or the address the connection arrived on. An
  empty name is refused, and so is the bare zone, which names no node.
- Any `Origin` is refused. `fleet-ui`, a browser page, admits its own origin.
- A POST must be `application/json` (fleet node and `local-agent`).

`warn` (the default, so `enforce` stays opt-in) logs what `enforce` would refuse, once per kind of violation, peer and
offending value in each 5 minutes, with values quoted so a request cannot forge a log line, and serves the request.
`enforce` refuses with 421, 403 or 415, naming the key. `off` is the old behaviour. A mistyped mode fails the load, and
resolves to enforce, never off, when a verb keeps going after a load error. `local-agent --serve` and `fleet-ui`
refuse to start on a config that failed to load.

- **Ported, not merged.** This is the unmerged `feat/sf16-ingress-guard` branch (three commits, written against 0.164.0)
  applied as a patch onto 0.186.0, minus its changelog hunk, with its two RFC 1918 test fixtures moved to the
  documentation range.
- **One zone fix on top of the port.** The branch judged the Host against the primary zone only
  (`TailnetSuffix()`), while a config can list several (`tailnet_suffixes`, ADR 0074: a node shared in from another
  tailnet is named under that tailnet's zone). Under `enforce` such a node got 421, and under `warn` it filled the log
  with false violations, which would have kept the warn window from ever showing the zero unexplained denials that
  promotion to `enforce` waits on.
- **One name test for both guards.** `HostAllowed` and `TailnetURL` now call the same function
  (`tailnetNameAllowedIn`: a suffix match on a label boundary, or a dotless name), so they cannot disagree about a name.
  Review found three shapes where they did: nested zones (`sub.zone` is a host under `zone`, yet the guard refused it
  as the bare inner zone), a second trailing dot (`zone..` passed, the bare zone included) and a port that is not a
  number (`localhost:evil.example` passed as loopback). A browser cannot send any of them and no exploit follows, but
  the parity claim was false. A test now feeds both guards 2394 generated hosts (19 names, 0-2 trailing dots, 7 port
  shapes, 6 zone sets). Two differences remain on purpose: a name made only of dots is refused here and admitted by
  the endpoint rule, and neither guard checks that a port fits in 16 bits.
- **The warn log no longer goes blind.** Its dedupe set was never emptied and any peer could fill it with 4096
  distinct `Host` values, after which a legitimate client's violation was only counted. It now empties every 5
  minutes, gives one peer at most 64 of the 4096 slots in a window, writes a count left by a burst when the minute
  ends instead of waiting for another violation, and keys the dedupe on a hash of the whole value, where two long
  values sharing 200 bytes and a length used to be one offender. Lines are still clipped to 1 KB.
- **The `enforce` refusal does not name the tailnet zones.** The 421 body carried the verdict, which lists every
  configured zone and the keys that set them, to a requester that under DNS rebinding is a page able to read it. It
  now says which `Host` it refused (clipped to 200 bytes) and which key relaxes the guard; the zone list stays in the
  log line. `Origin` and `Content-Type` echoes are clipped too.
- **The mode is said where the operator looks.** `warn` serves what it would refuse, so a stock install is not
  guarded, and only `local-agent --serve` printed the mode. `fleet-serve` and `fleet-ui` now print `ingress guard:
  <mode>` at start and `doctor` prints it. A missing `--config` file still starts a node on the defaults, as before
  (a fresh box is legitimate); it now shows `warn` there beside the built-in-defaults banner.
- **Red first.** Netguard tests (nested zones, a second dot, a bad port, 2394 generated shapes; the log's window,
  per-peer share, pending count and key; the body), a root test of the mode line and its three call sites, and seven
  docs pins were committed failing before each fix. Mutations: dropping the port check, trimming a second dot, admitting
  the bare zone, refusing nested zones again, dropping the colon rule, removing the rotation, the per-peer share, the
  timer or the whole-value key, echoing the verdict in the body, and removing each call site of the mode line each turn
  a test red.
- **Docs.** The `fleet_ingress_guard` row of the operator guide and the fleet-node operator guide, the `--serve`
  section (its sample banner now shows the mode), the troubleshooting tables, the fleet-node system page (the log's
  window, four edges to know before `enforce` including `local-agent --serve` clients such as a containerized GUI) and
  the coding-agent page (invariant 6b and the `--serve` interface) name the key.

**What this does not do.** It is a browser-CSRF and DNS-rebinding guard, not authentication. A tailnet peer that is not
a browser (no `Origin`, a JSON POST, a `Host` naming the node's address or a dotless name) is admitted exactly as
before, and a dotless `Host` still lets a LAN resolver steer a name to the node's GET routes. It does not close the
unauthenticated non-loopback bind: per-node tokens and tailnet grants are separate, later work, and the media lane
stays tokenless until they land.
