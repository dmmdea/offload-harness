---
status: Accepted
date: "2026-10-07"
---

# ADR 0075 — The display layer opens under the presence guard and leaves when the operator returns

## Context

[ADR 0052](0052-a-box-is-the-union-of-its-tiers-and-placement-is-a-per-task-decision.md) declared a **display layer** on
the three-card reference box: two small router twins (`gemma-4-e4b-display`, `gemma-4-e2b-display`, about 6.2 GiB each) on
the card that drives the monitor, guarded by `display_floor` (a free-VRAM floor of 4 GiB on that card) and `presence`
(the operator is away), and shipped it **dormant**. It stayed closed because of an earlier operator rule, written when the
box gained its third card, that single-card seats stay off the card driving the desktop; and because that card starving
the desktop is a measured failure (2026-09-04: a three-card engine left it under 1 GB, Windows fell to a 720p-class mode
and the box needed a reboot).

The cost of keeping it closed was also measured. With the pair seat saturated, a small mechanical call time-shared the
pair's cards and paid the agent seat's reload, while the display card sat near-idle; and in one spread run a 6 GB node
held 4 running requests with 4 more queued at 99 % and its thermal limit while the reference workstation's display card sat
at 8 % with about 12 GiB free. A card that is free whenever the operator is not at the desk is capacity the placement
could use.

Reading the code before opening it found five gaps, any of which would have turned "open the display layer" into
"starve the desktop":

- **The card allocator had no desktop floor.** `operator_presence` is one key with two readers. It also opened the
  display card to `gpu reserve --cards N` and to an auto-placed media call, with no floor, and it counted no loaded twin
  as a resident, so the display card sorted first as "nothing to evict" and a 10 GiB job could take it to about 2 GiB free.
- **The floor guarded the card the config names, not the card that drives the monitor.** The seed pins the display card by
  index; a board that re-enumerates after a power loss (the reference box's does) and a cable that moves both leave the floor
  protecting the wrong card. And `CheckComposite` read only digits after `CUDA_VISIBLE_DEVICES=`, so a render pinned by GPU
  UUID, which is how such a box must pin, read as "pins nothing" and was refused.
- **Nothing re-checked after admission.** The guards decide once. A twin then sat on the desktop's card until
  llama-swap's 300 s idle ttl, however soon the operator was back or a game took the card's memory.
- **A rendered twin is loadable by any llama-swap client**, past every guard, because the guards gate the harness's
  placement and nothing sits in front of llama-swap.
- **`operator_presence: away` admits at the desk.** It never reads the session (ADR 0052 D4), so it is an unconditional
  override, not a presence test.

## Decision

1. **The display layer's seat may take work under its presence guard.** On 2026-10-07 the operator was asked whether to open
   the display-card seat under its presence guard, with a recommendation of yes, and answered: **"yes"**. The seat
   therefore takes work only while the console is locked or idle and nothing is fullscreen (`operator_presence: auto`), with
   at least 4 GiB free on the display card after the load (`display_floor_gib`). The desktop stays first: the floor is the
   operator's, not a tunable the placement trades against throughput.
2. **This amends the earlier rule keeping single-card seats off the display card, for the display layer only.** Every other
   single-card seat, and every seat of every other layer, stays off the display card exactly as before. The layer's router
   twins are the one thing that may sit there, and only on these terms.
3. **The gaps close before the layer opens, not after.** The card allocator holds an opened display card to the layer's own
   floor arithmetic and counts a loaded twin as a resident (`config.DisplayFloorGiB()`, `gpualloc.ResidentFrom`); the
   `display_floor` guard refuses, naming both cards, when the driver says the monitor is on a card other than the declared
   one; `CheckComposite` reads UUID pins and compares one with an index declaration only through this machine's card table,
   refusing (and saying to declare UUID pins in `layers`, or to render on the box) when it cannot, rather than passing a pin
   it cannot show to be the declared card. A job that declares no footprint cannot be shown to leave the floor and is not
   given the card.
4. **A loaded twin leaves when the operator returns or the desktop's memory goes.** While a model of the display layer is
   loaded, `fleet-serve` re-asks the layer's `presence` and `display_floor` guards every `display_watch_sec` (default 10 s;
   negative turns it off; above 300 is refused) and unloads the layer's loaded models when either refuses
   (`internal/displaywatch`, `placement.ResidentVerdict`):
   - It asks the same question the admission guards ask, with the same readers and the same fail-closed reading of anything
     unreadable, but tests `free ≥ floor` and does not subtract the footprint a second time (a resident twin's footprint is
     already out of the free number), and does not ask `host_ram`, which bounds a load.
   - It unloads **only the display layer's models**, through llama-swap's **per-model** route, never the total one. Another
     layer's seat, the pair seat and the memory stack are never its to touch. An unreadable `/running` unloads nothing.
   - It does **not drain**: it unloads because the desktop needs the memory now. A request in flight on the twin ends with
     llama-swap's 502, and a mechanical call answers that as a structured defer. The display layer declares no agent seat, so
     the agent loop's wait-and-reissue ([ADR 0066](0066-a-seat-that-goes-down-is-waited-for-and-the-failed-step-reissued.md))
     is not what recovers it.
   - It **fails loud and recovers nothing else**: every unload is logged with its reason, a failed unload stays on the
     card and is recorded and retried at the next check, and nothing else is unloaded to make room.
   - `offload_status` shows it: `local.display_guard` (`watching` from a heartbeat, the period, `last_action`), omitted on a
     box with no display layer and on a dormant layer that never acted; `local.operator_presence` carries the reading and
     mode. A watcher that is running but cannot read `/running` is not watching: the state file records `read_err` and
     `blind_since`, and the field reads `watching: false` with a note while it is blind. An unresolvable state path is
     logged at start and carried in the note.
   - **It covers only the layer named `display`.** The watcher and the liveness gate below match the layer by that name; a
     layer of another name with the same guards and twins is guarded at admission and not re-checked afterwards.
   - **Admission asks whether it is alive.** The display layer's `presence` guard refuses, naming the reason, when the
     watcher's heartbeat is absent or stale, when the watcher is blind, or when `display_watch_sec` is negative
     (`internal/displaystate.Alive`, read through `placement.Live.WatcherAlive`; a leaf package so `placement` can read what
     `displaywatch` writes without an import cycle). A twin admitted with nothing watching it would stay until the idle ttl.
     The post-admission check does not ask: it must not unload twins because of its own heartbeat. Switching the check off
     therefore closes the layer.
   - **Closing the card needs a `fleet-serve` restart.** The watcher is built from the config `fleet-serve` started with and
     does not re-read it, so setting `operator_presence` back to `present` (or changing `display_watch_sec`) reaches it only
     on a restart; until then it applies the old mode to a twin already loaded.
5. **`auto` is the supported presence mode; `away` is documented as an unconditional override.** `away` admits at the desk,
   and `offload_status` prints that caution beside it. The default stays `present`.
6. **A rendered twin is loadable by any llama-swap client, and that is documented rather than hidden.** The post-admission
   check asks about every loaded model of the layer, whoever loaded it, so a twin a non-harness client loaded is unloaded
   within `display_watch_sec` like one the harness placed. It ends such a load; it cannot prevent one.
7. **Opening stays the operator's.** The layer ships `dormant: true` and `operator_presence` defaults to `present`. The
   enable sequence (seed the layers, UUID-pinned twins in the llama-swap yaml, restart llama-swap with the vLLM seat stopped
   cleanly, `auto`, `dormant: false`) is written once, in [composite-tier.md](../../systems/composite-tier.md), and is
   measured by the G1b recipe in the composite-tier plan before the flag flips on a live box.

## Consequences

- With the pair saturated, a small mechanical call has somewhere to run that is neither the pair's cards nor the cloud, and
  the display card stops being the idle one of the three. The measurement that says how much (G1b) is still to be taken.
- The operator can be back at the desk with a twin loaded for up to one check period plus the unload: about 10 s by default,
  not up to 300 s. During that window the card is still under the desktop. The floor, not the check, is what protects the
  desktop at the moment of admission.
- A sudden drop in free VRAM from something other than the twin (a game) unloads the twin; the work is re-placed or defers.
  That is the intended direction of the trade, and it means the layer is lossy under desktop load by design.
- The check is a periodic unloader, a posture this harness did not have (a read never unloaded anything). It is confined
  to one layer's declared models, one route and one state file, and a negative `display_watch_sec` switches it off.
- The watcher lives in `fleet-serve`, so a box where `fleet-serve` is not running, or was started before the layers were
  seeded, has no post-admission check. `local.display_guard.watching: false` says so, and the layer does not admit new
  work while it is so.
- A request in flight on the twin when the operator returns is cut, not drained.
- **A lease already running on the display card is not revoked when the operator returns.** The watcher unloads llama-swap
  models and nothing else. A `gpu reserve --cards` job or a media render that was granted the display card while the operator
  was away keeps it: the floor was checked once, at the claim, and afterwards the desktop is protected by the footprint the
  job declared (`--vram`), the window it declared (`--for`, after which the lease expires) and the operator's `gpu release`.
  A job that outgrows its declared footprint, or a game that takes the memory, can break the floor while it runs, and no
  check here will notice. Revoking a lease means stopping someone's process, which no guard in this harness does; the honest
  statement is that the post-admission guarantee covers the display layer's twins, not every lease on that card.
- **A request that queued while the operator was away is asked again at the grant.** The card it was queued on was chosen
  against the presence and floor readings of the enqueue. When the card comes free the same desktop rule is put again from
  fresh readings (`gpulease.Options.GrantCheck`); a card that no longer qualifies is released at once and the request
  chooses again, keeping its arrival time ([GPU lease](../../systems/gpu-lease.md), "A queued request is asked again at
  the grant"). This closes the gap between enqueue and grant; it does not reach a lease already granted, above.

## Alternatives considered

- **Leave the layer dormant.** Safe, and it leaves the card idle while the pair saturates; the operator decided to open it.
- **Rely on llama-swap's 300 s idle ttl.** The ttl is a per-model idle timer with no notion of the operator or of the card's
  free memory; it is what left the twin on the desktop's card in the first place.
- **Drain before unloading.** It would let a request finish, at the price of holding the memory the desktop needs back; the
  display layer's work is short mechanical calls that defer cleanly.
- **Run the check in the MCP server.** It is one process per editor session and exists only while an editor is open;
  `fleet-serve` is the node's one always-on process, already holds the 2 s device sampler, and runs in the console session
  where `auto` can read the desk.
- **Refuse to render the twins until the layer is opened.** It would remove the loadable-by-any-client case, and with it the
  operator's ability to measure the layer before flipping the flag; the check covers the case instead.
- **Make `away` read the session.** It is the operator's override by design (ADR 0052 D4); changing it would take the
  override away from the operator who wants it.

## Related code

- `internal/displaywatch/` and `fleet_displaywatch.go`: the post-admission check, its state file and the status view.
- `internal/displaystate/`: the state file's shape and `Alive`, which admission (`placement.Live.WatcherAlive`) reads.
- `internal/placement/guards.go`: `ResidentVerdict`, `PresenceAllows`, the display cross-check.
- `internal/config/layers.go`: `DisplayWatchInterval`, `DisplayFloorGiB`, `ValidateLayers`.
- `internal/mcpserver/layers_view.go`: `local.operator_presence` and `local.display_guard`.
- `internal/gpualloc/`, `internal/gpulease/`: the allocator's desktop floor and the twin residents.
- `internal/servingtmpl/composite.go`: `CheckComposite` and UUID pins.

## Related docs

- [Composite tier](../../systems/composite-tier.md): the guards, "Opening the display layer", the post-admission check.
- [GPU lease](../../systems/gpu-lease.md): the display card, once the operator is away.
- [ADR 0052](0052-a-box-is-the-union-of-its-tiers-and-placement-is-a-per-task-decision.md): the composite tier and the
  display layer's declaration.
- [ADR 0066](0066-a-seat-that-goes-down-is-waited-for-and-the-failed-step-reissued.md): the agent loop's recovery from a seat
  that goes down.
