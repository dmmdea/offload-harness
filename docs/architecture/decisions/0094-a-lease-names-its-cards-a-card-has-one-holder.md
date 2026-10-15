---
status: Accepted
date: "2026-10-14"
---

# A lease names its cards; a card has one holder

## Context

ADR 0018 made the GPU lease machine-wide and fenced. It was one record that fenced the whole node, so on a box with three
cards a render on one card queued every other job while two cards sat free. The change that fixed that arrived in slices,
and each slice was recorded where its author was working: the record and the grant as an amendment to ADR 0018, the text
gate as an extension of ADR 0026, the place in line and the per-card media door as amendments to ADR 0039, the drain as an
extension of ADR 0041, owners and terms in ADR 0070, a dead wrapper's live process tree in ADR 0084, the request to yield in
ADR 0086, and the principle that liveness is progress in ADR 0055. Each is right for its slice. None states the rule the
slices share, so a reader has to assemble it, and the assembly hides two things: how little the lease forbids (it never
frees a card from a holder that may still be using it, except by an explicit command that checks nothing), and how much of
what the status text promises is not built (a command it tells a waiter to run does not exist).

This record states the rule once, as the code stands at commit `3be61a3c` of the main branch on 2026-10-14 (the 0.187.0 release
commit that followed changes no file cited here), and lists what is not built. It was written from the code first. Where an older ADR, the system guide or a comment disagrees with the code,
the code is what is recorded here, and each disagreement is listed under Consequences for the owner of that text to decide.
Every behaviour below carries `path:line (symbol)`; lines drift, symbols are what to search for.

## Decision

Everything in this section is a behaviour of the code at that commit. It adds none. This is the part of the record that is
Accepted; the next section is not decided here.

**1. A lease names the cards it holds, or the whole node.** A lease carries a list of card ids; an empty list is the whole
node, and a blank entry in a list is refused rather than widened to the whole node (`internal/gpulease/devices.go:82-103`,
`NormalizeDevices`). Two leases conflict when either is whole-node or their lists share a card (`devices.go:112-124`,
`devicesConflict`). A whole-node lease is the original `meta.json` record; a card lease is `e/<epoch>.json` plus one
`cards/<id>.claim` for each card it holds (`devices.go:3-16`, `130-137`).

**2. A card has one holder, and the grant is where that is decided.** A card grant is one critical section under the epoch
lock (`devices.go:525-555`, `tryAcquireDevices`; `561-659`, `grantDevicesLocked`). It sweeps debris, refuses while a live
whole-node record exists (`565-571`) or a live card lease shares a card (`592-595`), refuses while the host cannot meet the
RAM the lease declares (`604-606`), issues the epoch, writes the record as `granting`, creates each card's claim file
exclusively and removes everything it made if any create fails (`622-647`), and only then marks the record `active`
(`649-651`). The exclusive create is the arbiter: a second grant that reaches a claimed card is refused (`634-636`). The
whole-node grant is the other half: after it creates `meta.json` it checks, under the same lock, that no card lease is live,
sweeps those the reclaim rule calls reclaimable, and withdraws its own claim if one is live; a card lease whose record cannot
be read counts as live there (`devices.go:688-731`, `verifyNoDeviceLeases`; `701-707`). A whole-node lease and a card lease
therefore cannot both be granted, from either side.

**3. The fence is per epoch.** A card lease is current while its record is `active` and every card it names carries a claim
naming its epoch (`devices.go:935-956`, `checkV2`; `400-424`, `epochIsCurrent`, `claimsNameEpoch`). Nothing compares one
lease's epoch with another's, so a lease granted later is never fenced out by an earlier one, and the holder's `Check` is the
same test (`internal/gpulease/gpulease.go:2038-2049`). A claim file that cannot be read for any reason reads as "no claim"
(`devices.go:147-153`, `readCardClaim`), so a failed read of a claim is a fence verdict and not a failed read; the holders
guard against that by looking again before they act on a lost fence (`gpu_cmd.go:1685-1719`, `confirmFence`).

**4. Nothing frees a card from a holder that may still be using it, except an explicit release (clause 5).**

- A lease is reclaimable only when its holder is provably gone, or its heartbeat is stale and its declared window has ended
  (`gpulease.go:1152-1172`, `Reclaimable`). The window alone never frees a card: a holder that is alive and heartbeating
  keeps its claim past the window.
- "Provably gone" is about the wrapper, not the job. A lease whose wrapper is dead, or whose pid was recycled, is held while
  any process of the tree the wrapper recorded is still the recorded process by pid and start identity
  (`gpulease.go:1160-1167`; `internal/gpulease/tree.go:10-19`, `124-140`). No timer bounds this; every read looks again.
- Only an acquirer that wants a card removes a dead or stalled lease's files, one lease at a time, under the epoch lock
  (`devices.go:737-760`, `sweepDebrisLocked`).
- A record that exists and cannot be read for an I/O reason is held while its keeper has shown a sign of life within the
  heartbeat TTL of 120 s (`internal/gpulease/keeper.go:71-74`, `87-106`; `gpulease.go:112`).
- A term that ends unrenewed is a label (`expired`, verdict `held-overdue`) and nothing more; a term renews on the owner's
  and the progress file's evidence (`internal/gpulease/term.go:3-15`, `279-341`, `AdvanceTerm`).
- An overdue lease's holder is asked to yield by its own tick, which runs the job's own recorded `--on-yield`; nothing is
  killed, released or taken, and unless the operator sets `gpu_on_overdue = request` nobody is asked
  (`internal/gpulease/yield.go:3-12`, `75-94`).
- The one party that ends a job is the wrapper, and only its own: on a lost fence it stops its command's tree
  (`gpu_cmd.go:986-992`, `gpu_tree.go:121-141`, `stopWrappedTree`).

**5. The one way to end a lease from outside is an explicit release by epoch, and it checks no liveness.** `gpu release
[--epoch N]` removes the lease whose epoch it names; with no epoch it ends the one lease held, and with several card leases
held it refuses and asks for `--epoch` (`gpulease.go:2228-2256`, `ReleaseByEpoch`; `2269-2304`, `ReleaseTarget`;
`devices.go:997-1033`, `1077-1084`). It removes files and looks at neither the holder nor its tree (`devices.go:962-966`,
`releaseV2`); the verb stops only the ComfyUI instances kept under the lease (`gpu_cmd.go:1753-1760`). A wrapper whose
claim was removed loses its fence on its next tick and stops its own command (`gpu_cmd.go:986-992`); a detached job keeps
running on a card that is now free to the next lease.

**6. Writing card leases is a per-host switch behind an audit.** `gpu_card_scoped_leases` is off unless set
(`internal/config/config.go:1366`), and setting it is not enough: the writer turns on only when the host also carries the
green reader-audit marker that `gpu doctor` writes (`gpulease.go:780-793`, `ApplyCardScopedConfig`). An acquisition that names
cards on a host whose writer is off is refused with `ErrCardScopedOff` (`gpulease.go:1391-1396`). Reading is never gated. The
synthetic whole-node record the first design called for, to make an older writer queue, is not written; the audit stands in
for it (`devices.go:17-24`, `44-47`).

**7. Every consumer asks about its own cards, and a doubt reads as every card.**

- The text gate asks whether a live lease sits on the cards a seat is pinned to; an unknown pin, a pin the card table cannot
  place, an unreadable card table and a whole-node lease all read as every card
  (`internal/modelaffinity/seatscope.go:141-167`, `ScopeToPins`, `ScopeToModel`; `internal/modelaffinity/gpuwait.go:227-242`,
  `blocksLoad`). The exemption for "this process runs under the lease" is per lease, compared by epoch
  (`gpuwait.go:284-291`, `insideLease`).
- `gpu reserve --cards N` chooses cards with one pure function: not claimed, not quarantined, the display card only within its
  cap, no foreign compute process, the declared VRAM and host RAM fitting, a card with no resident seat first
  (`internal/gpulease/allocator.go:3-29`, `169`, `Allocate`). Allocating and claiming are one loop, so two reserves that read
  the same free card do not both take it (`gpu_reserve_devices.go:30-32`).
- The drain and the unload act on the seats that sit on the leased cards (`gpu_drain.go:784`, `maintainSeatScoped`; `696`,
  `unloadModelsFor`).
- A media call leases the card or cards it needs when the host writes card leases and the card table can be read: one card
  (the allocator's pick, or the card an explicit pin names), a pooled route's set of two or more cards on a box of at most
  three, or, for the card-only placement, the box's two non-display cards as a named pair
  (`internal/pipeline/mediaadmit.go:453-518`, `planMedia`; `521-553`, `poolCards`, `poolMayBeScoped`;
  `internal/pipeline/mediamulticard.go:145-160`, `planMulticard`; `mediaadmit.go:786-815`, `acquireCardScoped`), for a window
  equal to the route's own timeout (`internal/pipeline/pipeline.go:1683`, and the other `acquireMediaLease` calls). A plan that
  cannot name its cards holds the whole node (see "Not in this build"). The card-only pair is the exception: it is never
  degraded to the whole node, and an unreadable card table or a host that does not write card leases is refused with the
  reason (`mediamulticard.go:145-160`, `171-177`, `noCardLeases`; `mediaadmit.go:795-799`).
- The fleet reads each live lease with the cards it holds, and a node refuses new work only when the leases that refuse it
  hold every card it has (`internal/fleetnode/leases.go:84`, `readLeases`; `173-203`, `closesNode`).

**8. A busy card is a place in line.** `gpu reserve` queues behind a holder for 8 hours unless told otherwise
(`gpu_cmd.go:390-409`, `defaultReserveWait`). Waiters order first come, first served among waiters whose cards conflict: a
waiter ahead on other cards does not hold you back, and a whole-node waiter is a barrier every later waiter queues behind
(`internal/gpulease/waiters.go:287-298`, `318-336`, `351-371`). A media call that waited its window with no card leaves a
token that keeps its place for 30 s after its poller leaves and can be resumed for 10 minutes
(`internal/gpulease/tokens.go:49-52`, `92`, `LeaveToken`).

## Not in this build

Nothing in this section is decided here, and none of it is current guidance. Each item says where the code stops and what the
Accepted clauses above already require of whatever is built, so that the next record starts from the same rule.

- **A takeover that stops a job and then frees its claim.** Three status surfaces tell a waiter to run
  `local-offload gpu takeover --epoch N` and add "not in this build yet" (`internal/gpuactivity/holders.go:128-133`,
  `internal/gpulease/explain.go:64-66`, `148`, `internal/mcpserver/status_section.go:326`); the verb list has no such verb
  (`gpu_cmd.go:56`, `gpuVerbs`). The stop primitives exist and nothing but tests calls them: `proctree.PlanStop` and `Stop`
  (`internal/proctree/stop.go:38`, `111`) and `comfyinst.StopMarker` (`internal/comfyinst/comfyinst.go:356`). Clause 4 requires
  of it: an explicit command only, only the recorded tree or a launch marker's process, each member's identity checked again
  right before it is signalled, and the claim freed only once the tree is verified gone.
- **A release that claims a card until its tree is dead.** `gpu release --epoch N` over a live holder frees the card at once
  (clause 5). No opt-in to abandon a tree exists, and nothing writes the `quarantine.<id>` sidecars the allocator reads to keep
  a card with an unstoppable tree from being handed out (`allocator.go:398-421`, `QuarantinedCards`, a reader only).
- **A policy for an overdue holder that ignores the request.** `gpu_on_overdue` has `request` and `hold` and no automatic mode,
  which a test pins as absent (`yield.go:75-94`; `internal/gpulease/yield_test.go:1180`, `TestNoAutomaticYieldModeExists`).
  Whether and when an unanswered request ends in a takeover is the operator's choice.
- **A media plan that always names its cards.** Some media calls hold the whole node, by design or because the plan cannot
  name a card. By design: the lanes with no ComfyUI instance to bind to a card, which are the audio-gen voice lane, the iGPU
  engines and the sd.cpp lanes (`internal/pipeline/pipeline.go:3489`, `internal/pipeline/igpumedia.go:239`,
  `internal/pipeline/sdcppneed.go:85`, each a `wholeNeed`), and a `run-graph` that declares no device
  (`internal/pipeline/mediaadmit.go:297-302`, `declaredNeed`). Because the plan cannot name a card: an unreadable or empty card
  table (`mediaadmit.go:467-470`); a pin that names several cards, is not a ComfyUI device index, or is in a device order the
  box has not declared (`477-486`); a pool key that is not a device or is in an undeclared order, and a pool of fewer than two
  cards or on a box of more than three (`493`, `521-553`, `poolCards`, `poolMayBeScoped`); several declared devices (`515`);
  and a host that does not write card leases (`795-799`). The card-only pair never falls back to the whole node: it is
  refused (clause 7).
- **A pool of local workers across cards for one batch.** A local image batch renders through one warm session under one lease
  (`internal/pipeline/pipeline.go:2463-2481`, `RunImageBatchAs`); the fleet batch places jobs on nodes
  (`internal/mediaremote/batch.go:246`, `RunBatch`); `gpu reserve --cards N` holds a fixed set while it queues
  (`gpu_reserve_devices.go:25-29`).
- **A scripted acceptance of the whole path** (a batch holding two cards, a text seat loading on a free card, a launcher that
  dies and a waiter that takes the card over, the status screens throughout). No such script is in the tree. Whatever builds
  it runs against a scratch lease root and never the live one.

## Consequences

- Jobs on disjoint cards run side by side; a render on one card no longer queues a text load on another. That needs
  `gpu_card_scoped_leases` and a green audit on the host. Unset, a host behaves as it did before cards were named.
- At most one lease holds a card, enforced by an exclusive file create under a lock. That is a statement about leases, not
  processes: a process that takes no lease, a graph posted straight to ComfyUI, and a forgotten reserve get no protection
  (ADR 0018, Consequences, `0018-machine-wide-fenced-gpu-lease.md:170-173`).
- An explicit release can hand a card to the next lease while the job it was running is still alive (clause 5). The surfaces
  that tell a waiter its next step name the unbuilt takeover for stalled, orphaned and overdue leases
  (`holders.go:128-133`) and name `gpu release --epoch N` for a tree-orphan (`holders.go:135-139`), so the one command a waiter
  can run is the one that leaves the job running. Until the first two items under "Not in this build" are decided, that is the
  trade the harness makes: an explicit, human-initiated release that is unsafe over a live tree, in place of an automatic one.
- A reader that predates the format reads a directory holding only card leases as free. The writer switch is the gate, not a
  compatibility shim (`devices.go:44-47`); a pre-format writer the audit did not find double-books a host with the switch on.
- The windows are chosen figures, not measurements: the fence window of 10 s (`gpu_cmd.go:1675`), the heartbeat TTL of 120 s
  (`gpulease.go:112`), the claim grace of 10 s (`gpulease.go:1818`), the token grace and life (`tokens.go:49-52`).
- The text that disagrees with the code, and what the code says:
  - `devices.go:17-19` still calls dropping the whole-node record "pending acceptance"; ADR 0018 records it accepted on
    2026-10-12, and the code does not write the record.
  - ADR 0026 says `DefaultTTL` (one hour) is stamped on essentially every media lease (`0026-text-load-admissions-wait-for-the-media-lease.md:57`,
    `227-228`). The media routes pass their own timeout as the window (`pipeline.go:1683` and the other calls), and
    `DefaultTTL` is the fallback for an acquisition with no window (`gpulease.go:114-116`, `1835`). The conclusion of ADR 0026
    stands, since the wait is bounded by the caller's budget and not by the holder's window.
  - `term.go:16-18`, ADR 0070 (`0070-leases-have-terms-owners-renew-orphans-are-surfaced.md:128-129`) and the system guide
    say the session registry's writer is "not wired into the session hooks". The MCP server writes the registry when it
    starts (`internal/mcpserver/mcpserver.go:393-423`, `registerSession`). What stays unverified is whether a given Claude
    Code build gives its MCP server the session id.
  - `internal/proctree/stop.go:3` says the stop primitives are used by the yield request. The request uses none of them
    (`yield.go:3-12`); they have no caller.
  - ADR 0055 says the media lease's reclaim rule has no job-liveness input (`0055-walls-are-ceilings-liveness-is-progress.md:449-450`).
    That is still true of the reclaim rule (`gpulease.go:1152-1172`). Progress does decide whether a term renews or is labelled
    (`term.go:3-15`), never whether a card is freed, so ADR 0055's principle reaches the label and not the free.

## Alternatives considered

- **Keep the whole-node lease.** One job at a time on a box with three cards, and a render on one card fencing a text seat on
  another. This is the position ADR 0018 started from and the reason for the change.
- **A synthetic whole-node record over the card leases, so an older writer queues behind it.** Verified to work against two
  older binaries, and dropped as a second source of truth to re-stamp on every grant and release (`devices.go:17-24`; ADR 0018,
  amended 2026-10-02). The audit gate does its work, and the record can return without a format change.
- **Compare epochs across leases.** With several live leases, a compare against one shared epoch would fence out every lease
  but the lowest (`devices.go:28-30`). The fence is per epoch.
- **Free a card when a term ends, or after a grace.** This is the 2026-09-07 incident that terms close: a detached holder
  released the card at its window with the job still running behind it (`internal/gpulease/term.go:23-28`; ADR 0070,
  `0070-leases-have-terms-owners-renew-orphans-are-surfaced.md:108`). A term ends in a renewal or a label.
- **Make the wrapper's death kill its tree by default.** It makes the wrapper a single point of failure for a long render; it
  is an opt-in (`--die-with-wrapper`, ADR 0084).
- **Fold this rule into ADR 0018 by rewriting it.** An accepted ADR is not rewritten to describe a different decision, and the
  slices live in four records already. The four records each get a dated pointer to this one and keep their text.
- **Wait to write this until the takeover and the release routing exist.** The status text already names a command that does
  not exist, and a record of what stands is what lets that gap be decided on its own.
- **Record the unbuilt items as decisions.** Architecture is the operator's to decide, and nothing in that list is built, so
  each stays a named gap with the constraint the Accepted clauses put on it.

## Related code

- [`internal/gpulease/devices.go`](../../../internal/gpulease/devices.go): the card record, the conflict rule, the grant, the
  per-epoch fence, the sweep, the release.
- [`internal/gpulease/gpulease.go`](../../../internal/gpulease/gpulease.go): `Reclaimable`, `Check`, `ReleaseByEpoch`,
  `ReleaseTarget`, `ApplyCardScopedConfig`.
- [`internal/gpulease/tree.go`](../../../internal/gpulease/tree.go), [`term.go`](../../../internal/gpulease/term.go),
  [`yield.go`](../../../internal/gpulease/yield.go), [`keeper.go`](../../../internal/gpulease/keeper.go): what keeps a claim.
- [`internal/gpulease/allocator.go`](../../../internal/gpulease/allocator.go),
  [`waiters.go`](../../../internal/gpulease/waiters.go), [`tokens.go`](../../../internal/gpulease/tokens.go): the choice of
  cards and the place in line.
- [`gpu_cmd.go`](../../../gpu_cmd.go): the verbs, the wrapper's tick, `confirmFence`, `gpu release`.
- [`internal/modelaffinity/seatscope.go`](../../../internal/modelaffinity/seatscope.go),
  [`internal/pipeline/mediaadmit.go`](../../../internal/pipeline/mediaadmit.go),
  [`internal/fleetnode/leases.go`](../../../internal/fleetnode/leases.go): the consumers.

## Related docs

- [GPU lease](../../systems/gpu-lease.md), "Known gaps".
- [ADR 0018](0018-machine-wide-fenced-gpu-lease.md), [ADR 0026](0026-text-load-admissions-wait-for-the-media-lease.md),
  [ADR 0039](0039-a-held-card-is-a-place-in-line.md), [ADR 0041](0041-the-drain-waits-for-runs-inside-the-queue.md): the
  slices, each amended to point here.
- [ADR 0055](0055-walls-are-ceilings-liveness-is-progress.md): liveness is progress, read against the lease above.
- [ADR 0070](0070-leases-have-terms-owners-renew-orphans-are-surfaced.md),
  [ADR 0084](0084-a-dead-wrapper-with-a-live-process-tree-keeps-its-claim.md),
  [ADR 0086](0086-an-overdue-leases-holder-is-asked-to-yield-never-taken-from.md): terms and owners, the live tree, the request
  to yield.
