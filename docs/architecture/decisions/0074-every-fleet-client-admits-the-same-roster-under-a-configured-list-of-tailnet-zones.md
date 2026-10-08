---
status: Accepted
date: "2026-10-07"
---

# ADR 0074 — Every fleet client admits the same roster, under a configured list of tailnet zones

Amends [ADR 0023](0023-agent-lane-tailnet-auth-and-locality.md) (the tailnet guard admits one configured DNS zone).

## Context

The operator's directive of 2026-10-07, given after a research call pinned to one seat queued 32 of 78 pages behind
it until the call's deadline: "The offload harness must be fully built and wired to smartly and dynamically rout work
utilizing the full capabilities of the Cluster's GPUs and systems", and "this needs to be a super SMART, DYNAMIC,
ADAPTABLE, OPTIMIZED, EFFICIENT AND FULLY PARALLEL SYSTEM THAT MAKES THE MOST OUT OF THE HARDWARE AVAILABLE WHILE
DELIVERING THE HIGHEST QUALITY OUTPUT POSSIBLE". Read for membership: a node that joins the cluster must be usable by
every lane the harness has, and a roster entry must mean the same thing to all of them. A review of how the roster
(`delegate_remotes`) is admitted found it did not:

- `netguard.TailnetURL` admitted a dotted hostname only under ONE configured zone (`tailnet_suffix`). A node shared in
  from another tailnet is named by Tailscale under the SHARER's zone, and by that name only (the vendor's sharing
  documentation: a shared machine is reached by its fully qualified name; the short name does not resolve from the
  recipient's tailnet). With one zone slot the operator's own zone and the sharer's could not both be configured, so
  the only form that passed every guard for the shared node was its raw tailnet CGNAT-range address.
  > **Unverified:** the vendor sentence above is the diagnosis's reading of the vendor page, not re-read for this change,
  > and whether MagicDNS on a given delegator answers the sharer-zone name with a tailnet address was not tested; the
  > dial gate (below) is what makes the answer safe either way.
- The shape check ran on the agent lane only. The five single-shot lanes (vision, text, stt upload, compose, accelerator)
  dialled the same roster entries through the dial gate alone, so one entry was refused by one lane and used by five,
  and a bad entry got no message naming its key on those lanes. (The media lane, ADR 0077, arrived after this
  decision and was built on the shared reader below from its first release, so it is the sixth lane that reads the
  roster this way, not one that was changed.)
- ADR 0023 says `TailnetURL` vets every remote base "at config load and at intake". For `delegate_remotes` it ran at
  intake only: a refused entry loaded clean, passed `doctor`, and then failed every `agent_delegate` and
  `offload_research` call, `route:"local"` included.

## Decision

1. **The tailnet guard admits a configured LIST of zones.** `tailnet_suffix` keeps its meaning (the operator's own
   zone) and a new key, `tailnet_suffixes`, lists further zones. Load installs the union, the own zone first, into
   `netguard` before any endpoint is vetted; each entry is normalized like the single key (lowercase, trailing and
   leading dots removed), a blank slot is unset, a duplicate collapses, and a malformed entry refuses the load naming
   `tailnet_suffixes[i]` and leaves the previous list in force. `TailnetURL` admits a hostname under any listed zone and
   nothing else; `TailnetURLIn` is the same body judged against a list the caller supplies, for a verdict that must not
   depend on which config this process loaded last.
2. **Every consumer of the zone judges the whole list.** The research lane refuses by NAME a host under any listed zone
   (it reads the public web only), the cache-store host check admits a name under any listed zone, and `doctor` prints
   one row naming the zones and the key each came from (nothing when none is configured).
3. **The dial gate is unchanged and is still the load-bearing half.** `SafeDialContext` resolves every name at dial time,
   discards any answer outside loopback and the tailnet CGNAT range, and connects to the vetted address literal. A second zone
   therefore adds no reachable address; it only lets the operator spell the name Tailscale gave the node.
4. **Every single-shot lane admits a roster entry the way the agent lane does.** `internal/rosterprobe.Members`
   normalizes the roster (trim, drop blank slots, keep configured order and slot numbers) and applies
   `netguard.TailnetURL` to each entry; the vision, text, stt-upload, compose and accelerator lanes call it in place of
   their own loops, and the media lane (ADR 0077) is built on it. A refused entry is a named miss, `<base>: not dialled, refused by the tailnet guard (<why>)`, never a
   failed call and never a dial; the entries after it still serve.
5. **The single-shot lanes read the roster at once, through one shared cache, at documented timings.**
   `rosterprobe.Probe` reads every admitted entry's `/fleet/health` concurrently, each bounded by the lane's own timeout
   (5 s; the accelerator lane keeps 2 s), and returns the readings in configured order (the accelerator lane's first
   listing node and compose's slot tie-break depend on it). A good answer is memoised for 2 s and a transport failure
   (dial refused, no route, DNS, a reset, the per-member timeout) is negative-cached, both process-wide; the reason is
   replayed into the lane's "probed ..." line with its age. The negative window is the delegator's 30 s, capped at 5 s
   for what a busy or restarting node shows (a timeout, a refused or unroutable dial, classified by error type), so a
   box that is back in seconds is back in rotation in seconds; a name that does not resolve keeps the full 30 s. A lane
   whose dispatch to a node is accepted calls `Cache.Forget`, which drops the verdict held against it, and a probe that
   gets an answer does the same. Callers that find a probe of the same base under the same bound already in flight wait
   for its answer instead of dialling again (their own context still ends their wait; a prober that leaves does not
   hand its cancellation to a waiter whose caller is still there). A node that answered with a status is never cached
   as down, nothing is cached once the caller's context is done, and a timeout is held only against callers who would
   not wait longer than the probe that failed. Before this each lane probed the roster serially (k dead members cost
   k x 5 s, or k x 2 s for the accelerator lane, on every call) with nothing shared.
6. **The cascade lane's probe pays once for a dead base, and only itself.** A `cascade_remote_lanes` base that does not
   answer its health GET at all is not asked for a roster (it cost two probe bounds, not one), and the probe runs outside
   the lane cache's lock with one probe in flight per base, so a call that needs one base never waits on another.
7. **A refused roster entry is a config finding at load, named by slot.** `EndpointWarnings` runs the same
   `netguard.TailnetURLIn` check the lanes run, against the zones THE CONFIG names (not whichever the process installed
   last, so `doctor` and `install client` judge the file they were given), and reports `delegate_remotes[i] is refused by
   the tailnet guard` with how to fix it. It is a warning like every finding there: the entry loads, `doctor` exits
   non-zero, and the operator learns it at load instead of at the first call. A zone list that does not parse is judged
   as no zones (the list is all-or-nothing, as the installed one is), and its error is the first finding, so the
   refused entries beside it have a visible cause. Every message about an entry prints it redacted
   (`netguard.RedactBase`, also `config.RedactBase`: userinfo, query and fragment removed), because `doctor` output is
   pasted into chats: the tailnet guard's own refusal, `Member.Miss`, `Reading.Miss`, the lanes' "probed ..." lines and
   dispatch errors, and the doctor rows all go through it, and a dial error is scrubbed (`rosterprobe.Scrub`) before it
   is cached or replayed, so a token pasted into a roster entry reaches no message.
8. **`doctor` shows whether the nodes accept this box's token.** `/fleet/health` ignores the bearer, so a wrong or missing
   `fleet_auth_token` read healthy on every row and first showed as a 401 on dispatch, which the delegator does not
   re-place. `doctor` reads the roster once, concurrently, through `rosterprobe.Probe` (it was serial, 5 s a remote),
   then asks each reachable node the one question health cannot: an agent dispatch with no `job_id`
   (`rosterprobe.CheckToken`). The node answers after its bearer check and before it creates anything, so 401 is a
   different token, 403 is a node with none beyond loopback, and 400 `job_id required` is a bearer that passed; nothing
   is queued on any of them. Rows are `OK`, `MISMATCH`, `NO-TOKEN`, `UNKNOWN`, `UNCHECKED` (refused or unreachable, not
   asked), each named by its `delegate_remotes` slot; a box with no token prints one `NOT SET` line. The bearer goes
   only to a base the tailnet guard admits, in the `Authorization` header, and neither it nor a node's reply text is
   printed. Informational: the exit code does not change.
9. **The bearer is one token, sent to every admitted node, and never follows a redirect.** `fleet_auth_token` is a single
   value: every request to a roster node (the health read, the token probe, a dispatch, a job poll) carries it, whichever
   node and whichever zone the entry names. The token probe and the six single-shot lane clients return a 3xx to the
   caller instead of following it (`rosterprobe.NoRedirect`), so a node, or a proxy in front of one, cannot have the
   client replay the request and its `Authorization` header at a `Location` it chose. The health read
   (`delegate.healthClient` in `internal/delegate`, which also sends the bearer) is that package's, was not changed, and
   still follows redirects; giving it the same policy belongs to that package's owner.

## Consequences

- A shared-in node can be named by the name Tailscale gives it, once the sharer's zone is listed. The raw `100.x` form
  keeps working and needs no zone.
- The default is unchanged and fail-closed: no list, no extra zone. `tailnet_suffix` alone behaves as before.
- Listing a zone is a deliberate, reviewable act by the operator, which is what ADR 0023's rejected alternative lacked: a
  generic `.ts.net` rule would admit any tailnet's Funnel-published name. This is not that rule.
- A tailnet the operator lists is trusted to name only its own nodes under its zone. A name under that zone which resolves
  outside the tailnet is still refused at dial time.
- **Listing a sharer's zone has a credential consequence, and it is the operator's to weigh.** Because the bearer is one
  token (decision 9), every node admitted under the sharer's zone is sent the same `fleet_auth_token` this box sends its
  own nodes, so whoever administers that node, or the tailnet that names it, receives a token that opens every node on
  this roster. List only nodes you trust with that token, and prefer a node in your own tailnet where there is a choice;
  a shared-in node you do not control is a node whose administrator can read the token. A **per-remote token**, so that a
  node in another tailnet is sent a token that opens that node alone, is future work: it needs a roster entry shape
  that carries a token reference and a rule for which lanes may use it, and it is not in this change.
- The `.local`, `.internal` and zone refusals in the research lane now cover every listed zone, so listing a zone does not
  open the shared node's admin surface to the research lane.
- A call that used to wait k probe bounds for k dead members waits about one, and a second call inside 30 s waits for
  none. The price is staleness: a node's health can be up to 2 s old when a lane places on it, and a node that comes
  back stays out of the single-shot lanes for up to 5 s after a timeout or a refused dial and up to 30 s after another
  transport failure (the delegator's own negative window), or until a dispatch to it is accepted. A placement that lands on a
  node that has since filled is refused by the node and reported as a capacity defer, as before.
- A bad roster entry reads the same on six lanes (vision, text, stt upload, compose, media and the accelerator forwarder): named, and not dialled. One difference remains and is recorded rather
  than hidden: the agent lane (`internal/delegate`) fails a WHOLE call on a refused entry, the single-shot lanes skip it.
  Skipping with a reason at the agent lane too is the consistent end state and belongs to that package's owner.

## Alternatives considered

- **Accept any `.ts.net` name.** Rejected again, for ADR 0023's reason: Funnel publishes names under `.ts.net` to the
  public internet.
- **Leave the zone single and use the raw address for a shared-in node.** It works, and is what the operator did, but
  it leaves the node unnamed in every status surface and makes the zone key a trap for the next join.
- **Resolve the name and admit by address only.** That is the dial gate's job and it already runs; replacing the shape
  check with it would make a config error surface as a dial failure at the first call instead of at load.

## Related code

- [`internal/netguard/tailnet.go`](../../../internal/netguard/tailnet.go) — `ParseZone`, `SetTailnetSuffixes`, `TailnetSuffixes`, `InTailnetZone`, `TailnetURL`, `TailnetURLIn`.
- [`internal/rosterprobe/rosterprobe.go`](../../../internal/rosterprobe/rosterprobe.go) and [`probe.go`](../../../internal/rosterprobe/probe.go) — `Members`, the one admission every single-shot lane calls, and `Probe`, the concurrent, cached read.
- [`internal/llamaclient/lanes.go`](../../../internal/llamaclient/lanes.go) — the cascade lane's probe (`FleetLaneGates`, `probeLane`).
- [`internal/config/tailnetzones.go`](../../../internal/config/tailnetzones.go) — `Config.TailnetZones`, the union Load installs.
- [`internal/research/fetch.go`](../../../internal/research/fetch.go) and [`internal/config/kvcacheserver.go`](../../../internal/config/kvcacheserver.go) — the two readers of the zone besides the guard.
- [`internal/rosterprobe/token.go`](../../../internal/rosterprobe/token.go) — `CheckToken`, `ClassifyToken`, `NoRedirect`; [`internal/rosterprobe/scrub.go`](../../../internal/rosterprobe/scrub.go) — `Scrub`, `Member.Shown`; [`internal/netguard/redact.go`](../../../internal/netguard/redact.go) — `RedactBase`; [`internal/config/endpointshape.go`](../../../internal/config/endpointshape.go) — `EndpointWarnings`.
- [`main.go`](../../../main.go) — `writeTailnetZonesSection`, doctor's zones row; `writeFleetSkewSection` and `writeFleetTokenSection`, the roster rows.

## Related docs

- [ADR 0023](0023-agent-lane-tailnet-auth-and-locality.md) — the guard this amends.
- [ADR 0042](0042-no-bare-http-client-on-a-caller-named-host.md) — the dial-time half.
- [`docs/OPERATOR-GUIDE.md`](../../OPERATOR-GUIDE.md) — operator view of the zone keys.
