# Changelog archive: 0.165.0 to 0.140.8

Verbatim entries moved out of the root `CHANGELOG.md` so the file GitHub renders stays under its size limit.
History, never edited. Relative links inside the entries are written from the repository root.
Newest first; the live changelog is [`../../CHANGELOG.md`](../../CHANGELOG.md).

## [0.165.0] - 2026-10-03 - PAIR shows every job where it ran: remote calls card, fleet nodes card for askers that cannot, transcription spills to an idle node

An audit of PAIR's Jobs list against every node's ledger (2026-10-03) found it drew every card it received correctly, and
that whole classes of work never reached it: 13 jobs a fleet node served for a thin client, every remote compose / vision /
text call, everything on a box that is not a PAIR member, and one card left "queued" for 31.9 h. It also found that a box
whose three cards were leased refused five transcriptions while two nodes' whisper seats sat idle. Defect ids are those of
the fixes plan; the release notes name each.

### Fixed — a test run no longer eats real PAIR cards (D1-D4)

- `TestLeaseCardLifecycle` built an enabled emitter with no state root, so its sweep claimed the machine-wide
  `pair-open` register's real orphan markers, posted their close to its own test server and deleted them: the real card
  stayed open (the 31.9 h ghost; reproduced exactly as the recorded `frames = 4` flake). The root, `internal/delegate`
  and `internal/mcpserver` suites now point `LOCAL_OFFLOAD_STATE_DIR` at a throwaway dir in a TestMain that fails
  closed (about 55 mcpserver tests and one delegate test touched the real root; a per-package sweep with `ProgramData`
  redirected now leaves the scratch root empty for all eight packages checked).
- An orphan marker records the endpoint it was posted to; a sweeper closes only markers of its own endpoint (a legacy
  marker counts as the default endpoint), so no test server or second ingress can consume a real card.
- A close PAIR rejects (400 / 413 / 422) drops that marker and the pass continues; a marker whose frame cannot be built
  is dropped the same way; transport errors, 5xx, 408 and 429 keep the marker. One bad marker no longer starves the rest.

### Fixed — remote work is carded where it ran (D5-D11)

- Remote compose, vision and text calls (`route: remote` or an auto spill) wrote no asker ledger row and no PAIR card.
  They now emit exactly one card (queued → running → terminal) whose node is the dispatch host with the fleet node id as
  an alias, plus one asker ledger row (`node`, `node_id`, `route`, `placement`, `fleet_job_id`, `card_by_caller`).
- New wire headers: `X-Offload-Asker` (the asking box's PAIR member name or short host name) on every work-creating
  fleet request, and `X-Offload-Pair-Card: node` ONLY when the asker will not card the job itself. On that signal the
  serving node cards the job (requester `offload-harness/fleet:<asker>`) and records the asker on its ledger row
  (`requester`). Old askers send nothing and keep today's behaviour, so a rolling deploy never double-cards.
- Pulled (claim-loop) jobs get `Door = fleet` and carry the same signal; a duplicate admission can no longer leave a
  node card open on the push or the pull path; fleet-serve drains its emitter on every shutdown branch.
- `install client` seeds `pair_workloads_enabled: true` (inert without a PAIR identity).
- Cards for work on a view-only PAIR node resolve through `<appdir>/configs/view-only-nodes.json`.

### Fixed — one card per call (D12-D15)

- A `video_watch` call wrote one ledger row per window and PAIR showed every row as a card (13 cards for one call on
  2026-10-03). Window rows, video_describe's overflow retries, the text cascade's escalating attempts, extract_image's
  sub-calls and inpaint's auto-text vqa are now inner rows (`parent_job_id`) of their call: one card, one job in the
  savings summary, each token counted once.
- `video_watch` stops waiting after the first window that defers `gpu_busy` (each window had re-waited the 90 s gate:
  18 minutes for one refused call); the defer carries `err_class` and the latest reason.
- A call's card is closed by its own ledger row (a `call_id` stamped on the row), not by the oldest open card of the
  task: concurrent transcriptions no longer produce a third card.

### Added — transcription can run on an idle fleet node (D16-D20, ADR 0072)

- New token-gated node route `POST /fleet/stt` (task `stt-upload`, advertised in `/fleet/health` with `stt_hq` and
  `stt_upload_max_mb`): the asker uploads 16 kHz mono Opus (or the original file under the cap), the node transcribes
  it through its own pipeline and returns the whole result. Config `fleet_stt_upload_max_mb` (default 48),
  `fleet_stt_max_concurrent` (default 1: stt jobs on a node queue in order across both lanes instead of failing
  "model unloaded").
- `offload_transcribe` gains `route` (`local` | `auto` | `remote`, default `auto`: every node serves the same whisper
  family, so a spill costs no quality); `auto` spills only when the local whisper admission would actually block
  (`modelaffinity.WouldBlockUpstream`). The asker writes its own `.srt/.txt/.segments.json`. CLI `transcribe --route`
  (default local); `classify` / `extract` gain `--route` like their MCP twins. A local gpu-busy defer now names the route.
- Behaviour change: on a node WITH a `fleet_auth_token` the legacy path-taking `stt` dispatch lane now needs the bearer.
  The only known external dispatcher sends no token, but no real stt use through it exists.
- Transcripts a node writes for an upload are removed after `fleet_stt_transcript_ttl_min` / when the job is evicted;
  `/fleet/media` needs the bearer for gated lanes' outputs (stt uploads, legacy stt transcripts, compose projects) on a
  node with a token, matching names fail-closed.
- `internal/accelremote` dials through `netguard.SafeTransport` like every other fleet client.

### Added — boxes outside the PAIR cluster report through a member (D24, D26)

- Identity fallback: when `node-id.json` is unreadable (the harness runs as another OS user than PAIR), the emitter
  reads this node's UUID from PAIR's loopback node-info and uses it only when the loopback ingress answers.
- Card relay: a member advertises `pair_relay` and serves token-gated `POST /fleet/pair-relay`; a box with no PAIR
  identity relays its cards there (`pair_workloads_relay`, default auto from `delegate_remotes`). The member namespaces
  the id, names the asker, resolves the node itself (including view-only nodes), rate-limits and caps open cards per
  asker; relayed markers are `.remote` files a pre-relay sweeper skips. `offload_status` gains a `pair` block naming
  the emitter's mode (local ingress / node-info fallback / relay / off).

## [0.164.0] - 2026-10-03 - Media calls take one card, leases have bounded terms, the fleet reports per card

**GPU routing, milestone 2, wave A.** Three phases of the routing plan, each built test-first, reviewed and fixed:

- **Per-card media admission (P13b).** A harness media call (image, edit, inpaint, upscale, animate, music, un-pooled
  video) now leases ONE card through the allocator and runs on that card's own ComfyUI instance, instead of fencing the
  whole box. Waiters keep their place with a waiter token instead of a `gpu_busy` defer. A kept instance no longer holds
  its runner open, and the lease holder stops it at release. The desktop card counts as a display card when a monitor is
  attached, so it is never auto-picked. An unkeyed runner given a non-default `--api` no longer starts a stray default
  instance. Hosts without card-scoped leases keep the whole-node behaviour byte for byte.
- **Bounded terms (P9).** A lease is granted one term at a time and renewed while its owner or its progress vouches for
  it. A lease past its window is labelled expired and the queue is told why; nothing is ever freed, reclaimed or killed
  by a deadline. A detached holder keeps holding when its window ends unless it was started with `--release-at-expiry`.
- **Fleet per-card truth (P7).** Nodes report their leases per card, so a remote lease fences only the contracts whose
  cards it sits on; `node-swap --cards` never stops a process that holds a lease on a card it would touch.

### Changed — a lease on one card fences a remote node only for the contracts that run on it (GPU routing P7, register C-86)

With card-scoped leases several leases are live at once, but a node's health described only the lowest epoch in one
block, so a long render on one card of three made the whole node a non-target and the delegator routed nothing to the
other two. This change carries the per-card truth across the fleet. Old readers keep working in both directions.

- **The node publishes `leases[]`** in `/fleet/health`: one entry per live lease with its cards (lower-cased GPU UUIDs;
  absent means the whole node), where they came from, its term, `busy`, `overdue`, `exclusive`, `draining`, its own
  `orphaned`/`stalled` and one `verdict` word. Each layer seat gains `device_ids`, the lease ids of the cards its pin
  names, resolved on the node. The singular `lease` block, `lease_exclusive` and `lease_draining` stay and are now the
  WORST across the live leases (busy, orphaned, stalled, exclusive and draining if any lease is; `overdue` only when no
  live lease is a long hold of its own, because every reader computes busy-and-not-overdue and an abandoned lease on one
  card must not hide a live render on another), so a delegator at 0.160.0 or 0.161.0 is never told less than is true; with one lease the
  block is byte for byte what it was. A node's lease reader now includes the cards a legacy lease was scoped to by
  evidence (when `gpu_legacy_scope_inference` is on), as the box's own gates already read it.
- **The node's own gates follow the cards.** `saturation.high` and `idle_slot` close the node only when the leases that
  refuse new work are whole-node or together hold every card it has, and `/fleet/dispatch` refuses a text reservation
  for an agent contract only when every seat that contract could run on sits on a card a reservation holds (every
  live text lease counts, not only the lowest epoch). Other task types keep the whole-node refusal.
- **The delegator fences per contract.** `NodeView.Leases` carries the entries; `leaseFenceReason` fences a node only for
  a contract whose seats ALL sit on a card a fencing lease holds, reading the seats off the node's own rows, and
  `leaseDemotionRank` ranks by the leases between that contract and its seats (an overdue lease is ranked last and never
  excluded, as before). A node that publishes no `leases[]`, no layer rows, no seat ids or a lane with no contract is read
  as the whole node. The chain rule moved to `placement.LeasesAgainstContract`, shared by the delegator, the node and the
  remote reading.
- **The wasted local leg is gone.** A lease that fences every local seat of a contract's chain turns the local run away at
  the seat's own pre-check, after the delegator had spent an attempt, a ledger row and an intent record on it. The
  delegator reads the same verdict first, at the route=auto fallback, the auto deal, the spread rotation, the
  re-placement's last resort and the capacity wait's local tick, and waits in line instead (only when another node exists,
  never for `route=local`; the holder's own child is exempt). A media lease on the seat's cards now takes the local slot out
  of a spread deal, as a text reservation did; this reverses the 2026-09-06 review note that a media lease changes nothing
  about a spread, whose premise (the gate waits for the render) the S-26 pre-check ended.
- **A capacity wait ends in a kept place.** When nothing took the work, the defer (class `capacity`, unchanged) names the
  places the subtask stood in: `results[].place_keeping` (`node`, `on`: lease, queue, backlog or cooldown, `detail`,
  `eta_sec`) and `retry_after_sec`, the soonest known end. The token that resumes a place across calls is the media-admission
  change and is not part of this.
- **Deploy.** A binary swap touches the executable and the processes it may stop, not a card, so a standalone node's wait
  stays every GPU lease unless the operator names the cards (`node-swap --cards`, forwarded by both launchers); the lease it did not wait
  for is recorded in the outcome (`cards`, `leases_left_alone`) and a refusal names the lease that held it. `fleet_reclaim`
  stays whole-node on purpose (docs/systems/fleet-node.md, "Per-card lease truth"). Three guards make the flag safe to
  use on a box that runs renders:
  - A lease held by a process the deploy would stop holds it whatever its cards. The fleet node and an MCP helper take a
    lease in-process while they render, the lease records its holder's pid, and the rename retry stops any helper that
    holds the exe: with `--cards` a lease on another card used to be left alone while the helper holding it was stopped,
    ending the render. Now such a lease holds the deploy, the rename retry never stops a process that holds a lease (nor
    any helper when the leases cannot be read), and the wait does not narrow when the processes running the image cannot
    be listed.
  - Each `--cards` entry is resolved against the box's card table (an nvidia-smi index, a GPU UUID or an unambiguous UUID
    prefix, as `gpu reserve --devices` does). An entry that places on no card, or a box with no card table, refuses the
    deploy before anything is touched; before, a typo or an index matched no lease and the deploy went ahead past the
    render the operator meant.
  - `--cards` with a health URL (given, or read from the node's config) is refused; it was silently ignored.
- **The singular `lease` block no longer hides a live render behind an abandoned lease.** `overdue` in the folded block
  is set only when no live lease is a long hold of its own. Every reader of the singular fields computes busy-and-not-overdue,
  so "overdue if any lease is" made an abandoned lease on one card mask a live long render on another from the text and
  vision remotes, the MCP door's fleet view and `leasedLanes` too, not only from a delegator one release behind.
- **A dead fleet is still reported when the local seat is fenced.** Before the wasted-local-leg change, route=auto with
  every remote failing its health probe and the local seat busy under somebody else's lease fell through to the local
  placement, which counted the run as Infrastructure; the fenced wait dropped that class, so the same dead fleet ended as a
  plain capacity defer and exited zero. The class now rides the wait sentinel (the fenced and the text-reservation ones, on
  every placement path) and is stamped on whatever ends the wait, unless a remote answered while it waited.
- **`fleet-ui` and `top` show one tile per card** with the lease that holds it (class, epoch, verdict, time left).
- Tests: `TestHealthLeaseEntryOverdueIsPerLease` used to assert the defect (the folded block reads overdue beside a live
  3 h lease) and now asserts the fold above. The existing delegate fixtures that held a media lease to make the local GPU read busy while expecting the local
  fallback now run under that lease (`runUnderLease`, `busyLocalAsHolder`), since a lease held by somebody else fences the
  seat and the delegator no longer dials it; `TestRunAutoMediaLeaseStillRunsLocal` and `TestRunSpreadMediaLeaseDealsAsBefore`
  became the tests of the new rule.

### Changed — a lease has a term; a term ends in a renewal or a label, never a release; the detached holder stops releasing at `--for` (GPU routing P9)

**The operator-visible change.** `gpu reserve --detach` used to hold the card until its `--for` deadline and then
release it whether or not the work behind it had finished; on 2026-09-07 that freed a card under a running job. It no
longer does. `--for` is now the lease's declared window and its first term, and when a term ends the holder's own tick
(the wrapper form's 15 s heartbeat; the detached holder's renewal) does one of two things, never a release:

- **renews it by one term** when its owner is alive and (its progress file is advancing or its cards are working), or
  when its owner cannot be told (no owner recorded, a session the registry never held) and its progress file is
  advancing, or when it is unattended and its progress file is advancing, up to a maximum total from acquisition;
- otherwise **labels it expired**, with the reason. An expired lease is still held and heartbeating, still passes its
  holder's fence, reads `held-overdue` (no new verdict word) with a note that says why the term was not renewed, and is
  open to a takeover (the command is a later change). It is **not reclaimable**: the reclaim rule is unchanged, nothing
  is freed or killed, and the label clears itself if the term becomes renewable again.

A request above the cap on a term is recorded, warned about on stderr, accepted whole and never shortened. The record
gains `term_ms`, `requested_ms`, `max_total_ms`, `expired` and `expired_why` (all `omitempty`; an older reader ignores
them). **`expired` is its own key and not a `State` value**, a deviation from the plan's wording: `state` is what
`checkV2` and `render/gpu-lock.mjs` fence on, and every binary and Node copy built before this change fences out any
state but `active`, so an `expired` state would stop a render running under an expired lease, which is the loss this
change exists to prevent. The plan's umbrella `meta.json` was dropped in P2, so a renewal rewrites only the lease's own
record.

New config keys `gpu_max_term_min` (default 360) and `gpu_max_total_min` (default 2880), installed at config load. New
flag `gpu reserve --detach --release-at-expiry` restores the old ending (refused without `--detach`). `/fleet/health`'s
lease block gains `expired` (absent unless true; read across every live lease, with `busy` and `overdue` unchanged).
`gpu status` prints the term and, for an expired lease, `term: EXPIRED ... ago and not renewed because ...`.

**The label is stable and does not state what nobody saw.** The reason a lease was labelled is compared at every recheck
(once a minute while it stays expired) to decide whether anything changed, so it never quotes a value that moves with the
clock: a stalled progress file used to put "no activity for 1h2m0s" into it, a different sentence every minute, so the
record was rewritten and the wrapper printed "not renewed" again, indefinitely, into the stream the wrapping session reads
as notifications. It now holds only the stall window the lease declared, and the holder says it once per expiry (a
renewal ends the episode). The cards the check reads have three answers, working, idle and could not be read: a failed or
timed-out `nvidia-smi` sample (likeliest when the GPU is saturated) is worded "its cards could not be read", never "no
work", and the reason the owner could not be told (the session was not in the registry, or the registry could not be
read) reaches the label instead of "no owner is recorded as present". An owner who cannot be told is not an objection:
an advancing progress file renews its lease, as it does an unattended one; a busy card alone still does not.

Three existing holder tests ended by waiting for the old deadline exit; they now release the lease, which is what ends a
holder. See [GPU lease](docs/systems/gpu-lease.md), "Terms", and ADR 0070.

### Fixed — review pass on per-card media admission (GPU routing P13b)

- **`gpu release` no longer kills a live job's ComfyUI instance and then refuses.** With several card leases
  held and no `--epoch` it stopped the kept instance of the LOWEST epoch (and warmed the seat) before the release
  refused with "pass --epoch N", so an operator's slip cost a clip of up to 80 minutes and left the lease held.
  The verb now settles which lease it is ending first, by the release's own rule (`gpulease.Manager.ReleaseTarget`),
  and a refusal ends it before anything is touched. The order is resolve, stop the kept instances, warm the seat
  back, release: the instances go before the seat is loaded onto their cards (the documented order; it had been
  warm first). A release that fails after the stop says the instances were already stopped.
- **A kept instance that outlived its lease is taken over by the lease that reuses it.** A kept instance is
  detached, so it survives a holder that crashed or was fenced out, and the next lease on its card reuses it (a live
  keyed instance with a marker that proves it is ours). The marker still named the dead lease, so that lease's release
  (which stops by epoch) matched nothing and the instance outlived every later lease with its models resident, past the
  5-minute idle-unload rule; and a fenced-out straggler of the old lease still matched it and could stop the instance
  the new lease was using. `docs/systems/gpu-lease.md` said the next holder "launches over it"; the code reuses it,
  and now takes it over: `ensureComfy` re-stamps the marker's `leaseEpoch` (only an instance that already belonged
  to a lease; an epochless marker is not claimed), a re-stamp that fails and a kept launch whose marker cannot be
  written both log `COMFY-KEEP-WARN`. A holder stops instances only while its lease is still its own (`gpu reserve`'s
  wrapper, the detached holder and the pipeline's grant; they leave a lost lease's instances running and say which),
  and `comfyinst` re-reads the marker at the moment of the stop. Left for the operator, as the plan marks it: whether
  the next acquirer or `gpu doctor` should sweep markers whose epoch is dead (an explicit sweep, not a watcher).
- **An unkeyed runner given a non-default `--api` no longer launches ComfyUI on 8188.** (Plan section 7, finding
  7, assigned to this phase.) With no `COMFY_INSTANCE` or `COMFY_CARD_UUID` the instance is unkeyed, `withGpuSlot`
  called `ensureComfy` with no endpoint, and an unkeyed launch has no `--port`: a blog batch run with
  `--api http://127.0.0.1:8189` started a stray ComfyUI on 8188 inside its card-0 lease while every job went to
  8189, and the stray holds a CUDA context on the leased card and makes a later card-bound default binding refuse with
  `COMFY-PROFILE-MISMATCH` (it broke a real batch). The runner now passes the endpoint down; `ensureComfy` reuses it
  when it answers and otherwise fails with `COMFY-ENDPOINT-DOWN`, naming the endpoint and the fix, launching
  nothing. The operator's own `COMFY_EXTRA_ARGS` carrying that `--port` still launches, and an orphan that needs
  replacing is refused before it is stopped when the relaunch could not land on its endpoint. `run-graph` shared the
  defect (its start and its free went to 8188); `instanceDeps` follows the same rule. Two older tests that pinned
  the old launch now pin the new rule.
- **A resumed place no longer ages while its call waits.** The resumed call looked its token up (without
  consuming it) and then waited in-process for the card's slot, up to the whole window, with no waiter record and no
  refresh of the token's last poll, so after the 30 s grace the place read as absent and other processes' waiters and
  newcomers skipped it: a caller that did everything right lost its turn while standing in it (the common case on a
  box with a pinned card, where every single-card call contends on one in-process slot). The wait now re-asserts the
  place every ten seconds (`gpulease.Manager.TouchToken`, which refreshes a token that exists and never writes one,
  and is ordered against a drop by an in-process lock so a touch cannot revive a spent place). The few milliseconds
  between a lease wait's waiter unregistering and the queued answer re-leaving the token remain; they cost
  fairness, never exclusivity.
- **A call the allocator can find no card for gets a place in line, not a lease fault.** The host short of RAM,
  a reading whose display state is unknown, every card the screen: the raw allocator error left through
  `gpu_lease_unavailable` after the whole wait. A call that can resume keeps a place on the cards that would qualify
  but for what is short (never the whole node, which would make its place a barrier) with the reason; a call no card
  could ever take, and every call from a door that cannot resume, gets the `gpu_busy` defer carrying the reason.
- **P13 acceptance that is still OPEN after this review pass** (they need a live box or the operator, and are listed
  in `docs/systems/media-generation.md`, "Open acceptance of P13"): the seeded `comfy_cuda_device` on the 3x16 tier
  (the operator decides whether a pin equal to the seed is treated as unset); a VRAM footprint and a host-RAM need
  for the allocator, and the N-instance RAM measurement; the live two-card acceptance; a foreign-process reader on
  the media path (Linux hosts only). The plan's "enqueue on the node daemon's queue" spike is recorded as not
  adopted, with the reason.
- **`offload_run_graph` with several `devices` holds the whole node.** The field was documented operator-only and
  nothing enforced it (the MCP door and the fleet dispatch both feed it for any caller), and several devices ran the
  graph in the DEFAULT instance, which sees every card, under a lease on only those cards: an arbitrary graph could
  place work on a card another call's own instance held, or on the display card, the two-jobs-on-one-card case the
  lease exists to prevent. One device still runs in that card's own instance, pinned by uuid, which sees no other card, so
  a caller can no longer reach past its lease with this field; several devices, and any a card table cannot
  resolve, hold the whole node. (Naming the display card stays the explicit word it always was.) The tool text no
  longer claims an enforcement it does not have.
- **A refused call that can never resume a place no longer leaves one.** The queued answer was unconditional, so
  the `generate-image` CLI verb, the fleet-node dispatch (the delegator re-places, it never resumes) and the image
  batch each left a token nobody could claim, and a live token holds its cards back from later callers for the 30 s
  grace: a busy node retried every few seconds by a delegator kept newcomers behind a rolling set of ghosts, and a
  free card could read as claimed for half a minute. A place is now kept only for a request that came through a door
  that can hand the token back and take it again (`core.Request.Resumable`, set by the MCP server, never decoded from
  or encoded to the wire, so a remote payload cannot ask for one); every other caller gets the `gpu busy` defer
  it always had, and the batch's `IsGPUBusy` refusal is unchanged.
- **`display_attached` no longer changes what a host with card-scoped leases OFF advertises.** The OR rule
  (`display_active` Enabled or `display_attached` Yes) had been put behind `gpuprobe.DisplayCardUUIDs`, which the load
  figures read too, so on any host whose monitor is plugged in the monitor's card left `work_util_pct`, the
  delegator's free-card count, the lease verdict's "that load is the operator's game" and the foreign-load guard for
  good, not only while a game ran: a node whose only busy card was the monitor card, working for its own seat or
  lease, advertised itself idle. The rule is now two: `gpuprobe.ScreenCardUUIDs` (either column) for the card table
  and the allocator, which is where plan invariant I6 lives, and `DisplayCardUUIDs` (`display_active` alone, as it
  was) for every load figure. The change to the load figures was not asked for by anyone; if the operator wants the
  attached monitor out of them, that is a fleet-visible change to decide, with a test that pins the new
  `work_util_pct`.
- **One transient `nvidia-smi` error no longer switches `display_attached` off for ten minutes.** The fallback
  to the query without the field was armed by ANY failure of the full query, silently, and the full query was then
  not tried again for ten minutes. `display_attached` is the only signal that marks the monitor's card while the
  screen sleeps, so in that window the allocator could hand the desktop card to a render with the operator at the
  desk. The window is now armed only when the failure names the field (nvidia-smi prints `Field
  "display_attached" is not a valid field to query.` on stdout, status 2; the recogniser reads stdout, the error
  text and an ExitError's stderr) or after three unexplained failures in a row, and it logs one line when it
  arms. Any other failure answers that call from the fallback and is forgotten, and the reading says so
  (`Device.AttachedUnknown`, `Card.DisplayUnknown`): the allocator skips every card of such a reading
  (`display-unknown`, not a card to wait on) until the next good one, unless the operator is away.
- **A queued answer for a call that needs the whole node names the lease in its way.** It skipped every card
  lease for such a call (an empty card set intersects nothing), so it reported `held_by: null`, `eta_s: 0` and
  "card(s)  are promised to callers ahead of this one ... at most 0s" behind a lease with a declared hour. A
  whole-node request is now blocked by every lease, the reason says "the whole node", `eta_s` is the longest
  declared end among the leases in the way, and an answer with no declared end says it has no estimate instead of
  printing "at most 0s".

### Added — per-card media admission, and a place in line instead of "gpu busy" (GPU routing P13b)

**The operator-visible change.** On a host that leases cards (`gpu_card_scoped_leases` set and a green
`gpu doctor --write-audit`), a generation call no longer takes the whole-node media lease. A single-card route
(image, edit, inpaint, upscale, animate, music, un-pooled video) asks the allocator for ONE card, holds a lease on
it and runs in the ComfyUI instance bound to that card (port 8189 plus the card's nvidia-smi index, pinned by GPU
uuid, `COMFY_CUDA_DEVICE` blank), so two calls run on two cards at once and a render on one card no longer fences
the text seats on the others. The display card is never auto-assigned while the operator is at the desk.

- **An explicit `comfy_cuda_device` is a hard constraint.** Resolved through `gpu_comfy_order` to one card, the
  call queues FIFO for that card however many others are free and is never re-picked. A pin that cannot be turned
  into a card (no declared order, a comma list, not an index) keeps today's whole-node lease and `--cuda-device`,
  with one logged reason. **On a box whose config pins `comfy_cuda_device`** (the 3x16 seed does) concurrent
  renders therefore need the pin cleared: with the order declared the pinned calls serialise on that card while the
  others stay free for text; without it they keep the whole node.
- **Pooled routes** lease the cards their pool keys name and keep their launch unchanged (the default instance,
  every card visible), only when every two such jobs must overlap (a pool of at least two cards on a box of at most
  three); on a larger box they hold the whole node. `offload_run_graph` holds the whole node unless the operator
  declares `devices` (one device: that card's instance). sd.cpp, voice, a host that does not lease cards and an
  unreadable card table keep the whole-node lease byte for byte.
- **The runner is told what it may unload.** The lease env carries `GPU_LEASE_DEVICES` and a
  `GPU_LEASE_UNLOAD_MODELS` scoped to the leased cards (the roster minus the memory stack minus seats pinned to
  other cards), so a render on one card stops emptying the seats on the others (register C-86). A process running
  under its parent's multi-card lease (`gpu reserve --devices ... --`) runs each call on a free card of it.
- **A place in line replaces the refusal.** A call that waited its `gpu_wait_ms` with no card answers deferred,
  `err_class: gpu_queued`, `defer_class: capacity`, with a `waiter_token`, `queue_position` and `eta_s` (a
  ceiling); re-sending the request with the token resumes the place with its original arrival time. A call that holds
  the whole node leaves a token too. Every media MCP tool takes `waiter_token` (and `offload_run_graph` an
  operator-only `devices`). Hosts that do not lease cards keep `gpu_busy`. Only MCP calls leave a place (see the
  review pass above): the CLI verbs, the fleet dispatch and the image batch keep `gpu_busy`.
- **`gpulease` place-keeping tokens** (`Options.ResumeToken`/`QueuedSince`, `LeaveToken`, `ResumeToken`,
  `Tokens`, `QueuePosition`, `ErrStillQueued`): a token has no process behind it, so its life is its last poll. It
  holds its place for 30 s, is then skipped by every waiter (a whole-node barrier included) so an absent client
  never blocks the line, and can be resumed for 10 minutes. They live in `<state>/gpu/tokens`, apart from the waiters
  an older binary prunes; on a mixed-version host an older binary can take a card ahead of a token holder (the holder
  loses its place, exclusivity is never at risk).
- **`internal/pipeline` media slots are a set, one per card** (`takeMediaSlot`/`releaseMediaSlot` keep the
  whole-node meaning); two jobs on disjoint cards hold theirs together, waiters are served in arrival order with
  disjoint backfill and a whole-node waiter is a barrier.
- **`internal/gpualloc`**: the allocator's input builder, the pick-or-queue decision and the unload-scope rule moved
  out of package main so `gpu reserve --cards` and the media path read one rule (no behaviour change for the verb).
- **The post-run `/free` goes to the instance the runner was given** (it read the parent process's `COMFY_API`
  when a route handed its instance through the Spec's env, which most do).
- Deferred to the operator (live box): the two-card acceptance, host RAM with N concurrent instances, the Port
  Directory entry for 8189 to 8191, and clearing `comfy_cuda_device` on the live config.

### Added — the holder of a lease stops the ComfyUI instances kept under it (GPU routing P13b)

- **`internal/comfyinst`.** A kept instance (see the fix below) lives no longer than the lease it was launched
  under. Its launch marker records the lease epoch; the holder stops it on release: `gpu reserve` when the wrapped
  command ends, the detached holder when it exits, `gpu release` when an operator ends a lease, and the pipeline
  when a media lease is released. Only a keyed marker naming exactly that epoch is touched, and only after the
  instance is shown to be the harness's own (pid alive, not started after the marker, and the endpoint on the
  marker's port reporting exactly the recorded argv); then `POST /free` and a stop. Anything short of that is left
  running and reported, never killed. A marker with no lease epoch is never touched.

### Fixed — a runner given --keep-comfy exits (GPU routing P13b)

- **A runner that launched ComfyUI itself and was told to keep it no longer hangs.** The instance was a
  non-detached child with piped stdout/stderr, so the runner's event loop stayed attached to a process meant to
  outlive it; in a real film run the per-shot timeout then killed the tree and deleted the finished clip. A kept
  instance (`withGpuSlot` passes `keep` to `ensureComfy`) is now spawned detached, its console going to its own
  log file, unref'd, with its window hidden: it holds nobody's event loop and has no pipe to close under it. The
  non-kept path is unchanged. The log is bounded by rotation (each launch rotates; each archive is cut to its
  last 5 MB), not by a capture that stops at 5 MB, and `tailComfyLog` reads only the last 256 KB. A kept instance
  lives no longer than its lease, whose holder stops it on release (see media-generation.md, "A kept instance").
  A node test runs a real runner against a stub ComfyUI process and asserts the runner exits while the instance
  stays up and keeps writing.

### Fixed — the display card is found with the screen asleep (GPU routing P13b)

- **The allocator's "never auto-pick the display card" rule now fires at the desk.** On the 3-card box
  `nvidia-smi` read `display_active` Disabled on every card while the screen slept, and `display_attached` Yes on the
  card that drives the monitor, so the one shared rule (`gpuprobe.DisplayCardUUIDs`) found no display card and the
  allocator could hand that card to a render. The per-device query now carries `display_attached`, and the CARD TABLE
  marks a card as the display card when `display_active` reads Enabled **or** `display_attached` reads Yes
  (`gpuprobe.ScreenCardUUIDs`, read by the allocator, `gpu cards` and `offload_status`'s card table). The load
  figures are NOT moved onto it: the fleet node's `work_util_pct`, the lease verdict's "that load is the operator's
  game", the foreign-load guard and the delegator's free-card count keep reading `display_active` alone
  (`gpuprobe.DisplayCardUUIDs`), because an attached monitor is true for the whole life of the box and counting it
  there would hide a node's own work on that card and change what every host with card-scoped leases OFF
  advertises. Only an exact `Yes` counts; `[Not Supported]`,
  `[N/A]` and a missing column are "unknown", never the operator's screen. A driver that refuses the field (an older one,
  a headless Linux build) rejects the whole query, so both readers (`gpuprobe.Read` and the lease verdict's sampler)
  retry without it through one fallback (`RunDisplayAware`) and the rule rests on `display_active` alone, as before; the
  full query is tried again after ten minutes, so a 2 s sampler does not pay a doomed call every tick. The node
  publishes `display_attached` in `gpu_devices[]` (omitted when false; a node that predates it reads as "unknown").
  Explicit `--devices` naming the display card is unchanged. A box whose only card is the display card still runs its
  seats there.

## [0.163.1] - 2026-10-03 - A warm-back never loads a seat over another card's lease

### Fixed — a warm-back waits for the last lease on the seat's cards (register C-86, plan P5 follow-up)

A lease that unloaded the agent seat owes it a warm-back, and the warm loads the seat on all its cards. With card-scoped
leases several holders share a box, and the warm checked only its own lease and the queue behind it: the first of three
per-card film renders to finish would have loaded the 3-card agent seat over the two cards still rendering (found while
launching the SS-31 motion clips on three per-card leases, 2026-10-03; that run started with the seat cold, so nothing
was owed). The warm now also skips while another live lease sits on the seat's cards, says which, and leaves the warm
owed for the next holder that warms (a later `--unload-seat` wrapper, `gpu release --warm-seat`); otherwise the seat
loads on its next request. The seat's cards are its declared pins; a seat that declares none is on every card, and a
lease on a card the seat does not use leaves the warm alone. `gpu release --warm-seat` takes the same rule; with no
epoch it counts the whole box, as the release itself does, so over two live leases it neither warms nor releases.
Known limits, both in the cold direction (the seat loads on its next request; an idle seat unloads after 5 minutes
anyway): two leases ending in the same instant can each see the other and both skip, and a last lease without
`--unload-seat` never warms. Tests: `TestAWarmBackWaitsForTheLastLeaseOnTheSeatsCards`,
`TestAWarmBackWaitsForALeaseOnADeclaredSeatCard`, `TestAWarmBackIgnoresALeaseOffTheSeatsCards`,
`TestReleaseWarmSeatWaitsForTheOtherLeaseOnTheSeatsCards`, `TestReleaseWarmSeatWithNoEpochWarmsOverTheOneLiveLease`,
`TestReleaseWarmSeatWithNoEpochAndTwoLiveLeasesNeverWarms`, `TestReleaseWarmSeatAfterTheLeaseEndedWarmsOnTheFreeCard`
(8 of 8 mutants killed, across the first cut and the review fix).

### Fixed — a drain test no longer races the lease acquire

`TestReserveRenewsTheLeaseWhileDraining` ended its fake in-flight request 400 ms after the test started, so on a loaded
Windows box (about 900 processes; a scratch-root reserve plus release measured 850-960 ms) the request was over before
the drain had renewed once, and the test failed on main as well. The request now lasts until the heartbeat is seen to
move, capped at 5 s; with the drain's renew removed the test still fails. Test-only.

## [0.163.0] - 2026-10-03 - Thin clients render on the fleet: the compose-project door, `install client`, and a box with no seat never takes work itself

### Added — the compose-project door (ADR 0071)

A machine with no composition lane (a 2-core laptop, an arm64 board) can now have a whole HyperFrames project
rendered on a node that has one. The template door (`compose-video` over `/fleet/dispatch`) is unchanged.

- **`POST /fleet/compose-project`** takes a gzip-compressed tar of the project (base64, with its sha256), the
  composition and the render options. It is closed (403) unless the node sets `fleet_compose_projects: true`, holds a
  `fleet_auth_token` and has the lane bound, and it is never open on a tokenless node. The bearer is checked before the
  body is read; only then does the request get a 15-minute read and write window (the server's blanket 30 seconds cut a
  large bundle on an ordinary link). Bundles are capped by `fleet_compose_bundle_max_mb` (64 MiB compressed by default).
- **Extraction** writes regular files and directories only, under safe names (no absolute path, drive letter, UNC path,
  `..`, backslash, NUL, colon, Windows device name, trailing dot or space), with caps on entries (directories
  included), files, each file and the total counted on the bytes written, and the decompressed stream bounded as a whole.
  A conflict in the bundle itself is refused as the bundle's; a write the node cannot make answers 500, so the caller
  reports infrastructure, never "refused".
- **The reference check** (Confine) follows how HyperFrames' compiler reads a project: references anywhere in the
  text (an attribute after `/` too), every file a `data-composition-src` names whatever its extension, a climb out of the
  project from the file or, unless it starts with `../`, from the project root; UNC and protocol-relative paths,
  `file:`, `<base>` and `srcdoc`; and hosts that are loopback in any numeric form a browser accepts, private, tailnet,
  dotless or non-ASCII. Public `https` references stay allowed. The client runs the same check before sending.
- **The jobs** are masked from tokenless polls and feeds, exempt from `fleet_max_concurrent_jobs` like `compose-video`,
  and their extracted trees are removed when the job ends; fleet-serve sweeps any a crash left behind at startup.
- New config keys: `fleet_compose_projects`, `fleet_compose_bundle_max_mb`.

### Added — `offload_compose_video` places a render on the fleet

- **`route`** (`local` | `auto` | `remote`; `--route` on the CLI). `auto`, the default, renders here when this machine
  has the lane and on the least-queued node from `delegate_remotes` that advertises the door when it has none. A template
  travels by name; a project directory or inline HTML travels as a bundle. The video and snapshots come back into the
  media dir (or `out`) under plain file names only. With neither a lane nor remotes the call defers naming both fixes.

### Added — the delegation client

- **`local-offload install client`** writes a client's config (mode 0600, through a fresh temp file): the remotes, the
  fleet token read from a file and never printed, delegation on, every model route and default script binding empty,
  and ffmpeg bound only where the machine has it. It refuses remotes that are not fleet node bases (off the fleet port,
  loopback, a `/v1` suffix) and removes what it wrote.
- **`setup/install.sh --client --remotes ... --token-file ...`** installs the binary and that config, registers the MCP
  server for Claude Code and runs `acceptance`: no tier detection, no llama.cpp, no model, no service.
- `acceptance` and `doctor` skip their local-model checks on a box that names no model.

### Fixed — a box with no agent seat is never a placement

Placement treated a box with no agent seat as an idle one: route=auto (an idle local seat wins) and route=spread
(subtask 0 stays local) put contracts on a seat that could only defer them, so a client's `agent_delegate` and
`offload_research` deferred "no agent seat resolvable" with the fleet idle. Now every contract from such a box goes to
the fleet or waits in line for it, and the reasons say the box has no seat.

### Fixed — token-gated lanes mask their jobs on every path

Jobs of every token-gated lane but the agent lane (vision, text, compose-project) are masked from tokenless pollers and
feed readers whichever path admitted them; a vision or text job claimed from the pull queue was readable before.

### Docs

`docs/systems/delegation-client.md` (new), ADR 0071 and its amendment 1, ADR 0059 decision 6, the operator guide (the
route, the bundle checks, seatless placement), `setup/SETUP-AGENT.md` (the client install).

### Tests

The door (closed states, bearer before body, caps, sha256, extraction, Confine, cleanup, masking, deadlines, node-side
500, the sweep), the bundle package (every name, type, cap and reference rule, with the reason each case must give, the
kit's projects and templates passing), the client (template and project round trips against a real node server,
refusals before upload, plain output names), `install client` (rendering, refusals, token hiding, 0600 on `--force`,
ffmpeg binding) and seatless placement on auto and spread. Every new guard was mutated at its call site and seen failing.

## [0.162.0] - 2026-10-03 - The compose lane pins HyperFrames 0.8.114, and `install hyperframes` puts a moved pin on every node

### Fixed — a pin move can no longer strand a node's composition lane

After 0.159.0 moved the HyperFrames pin to 0.8.108, the deploy swapped each node's binary and render tree but
not the install under `hyperframes_dir`. The new runner refuses any install that is not its pin, so a node that
missed the hand re-install answered every composition with `CLI_MISSING` while `doctor` and `offload_status`
read `compose_video` CONFIGURED: they checked that the files existed, never which version they held.

- **`doctor` and `offload_status` now give the runner's answer.** mediacap reads the installed version and the
  runner's `PINNED_VERSION`; when they differ the route is BOUND-BUT-MISSING with both versions and the fix
  (`run local-offload install hyperframes`).
- **`local-offload install hyperframes` (new)** is the installers' HyperFrames step for an installed node, from
  the `package.json` and lock the binary embeds, so a node that runs from a binary drop needs no checkout. It never
  leaves a node worse off: it refuses up front when the runner beside the binary pins another version or the
  directory is not a harness install; keeps a healthy install (committed package files, the pinned version, a
  tree `npm ls --all` accepts) and only re-verifies it; otherwise builds the new tree in
  `<dir>/.install-staging`, where `npm ci --ignore-scripts` and `npm audit signatures` (fatal) run, and swaps in
  only a verified tree, keeping the replaced one as `node_modules.prev` (with `package*.json.prev`) for a
  rollback; then runs `npm rebuild esbuild` and the runner's `browser` and `version` ops on the new tree and puts
  the previous install back if one fails. npm runs with the configured node first on PATH; a config bound to
  another Chrome build than the pin resolves is an error that names the value to set; `--timeout` bounds the
  whole step (45 minutes).
- The release procedure runs it, then `acceptance`, on every node with the lane right after that node's swap.

### Changed — HyperFrames 0.8.108 -> 0.8.114

- The lock's integrity equals the registry's; `npm audit signatures` passes; esbuild stays 0.25.12, so the
  `allowScripts` approval holds; chrome-headless-shell stays 152.0.7977.30 (the second Chrome string in the
  package is macOS 12's). The guards the runner depends on were re-read on the package: every environment gate
  and the `--json` skip of the update and skills checks appear as in 0.8.108, the 45 hosts the package names are
  identical, and `--help` for lint, check, render, snapshot and browser differs only in `snapshot --at` (each
  timestamp is now captured at that exact instant).
- **Measured through the runner.** captions-bar's default sample: lint 0/0, check passed, VP9 `yuva420p`
  1920x1080, 240 frames, 15/15 identical decoded-frame pairs across 1, 2, 4, 6 and `auto` (twice) workers, and 0
  of 240 frames different from a 0.8.108 render; the frame at 1.5 s reads "Captions follow the words". All seven
  templates at their defaults and with every string variable at its maxLength gave the same lint and check
  verdicts on both versions (14/14).
- **One behaviour change reaches the templates:** since 0.8.112 an alpha export keeps the composition root's
  background. The four opaque templates exported to `webm`, `mov` or `png-sequence` are now opaque, as their
  `alpha: false` says (title-card `webm`: alpha plane mean 10-14 on 0.8.108, 255 everywhere on 0.8.114); the
  alpha templates are unchanged.
- A Linux node rendered through the lane on 0.8.114 after installing it with the new subcommand into an empty
  directory (title-card mp4, lower-third webm with alpha), so the setup guide's "no Linux node has been
  verified" line is gone.

### Changed — the house skill and the kit's provenance

- `skill/hyperframes-compose` keeps its ban on skill installers (`npx skills add`, `claude plugin install`,
  `hyperframes skills`/`init`) and no longer forbids an operator-kept, pinned copy of a teaching kit wired to
  this lane with its paid and cloud steps removed. The kit's `PROVENANCE.md` says the same for this repository.

### Docs

`setup/SETUP-AGENT.md` (the subcommand, the integrity and Chrome lines, Linux), ADR 0059 (the pin history and
how a moved pin reaches a node), `docs/OPERATOR-GUIDE.md` (the bump procedure ends with `install hyperframes` and
`acceptance` per node), the templates README, `_shared/README.md` (the font chunk is byte-identical in both
versions), captions-bar's Measured section.

### Tests

`internal/hfinstall` (stage, swap and rollback paths, the ownership guard, the runner argv, stdout-only result
parsing through a real child process, PATH), mediacap's drift verdict, and a root test that the embedded
`package.json`, the embedded lock and the runner's pin agree. Fourteen mutants across the new guards were each
seen failing a test. With 0.161.0 merged in: Node 885 pass / 0 fail / 1 skipped (886); Go all packages green.

## [0.161.0] - 2026-10-02 - GPU leases name their cards, free cards take queued work; agent audit trail, read floor and audit chain

**The operator-visible change (GPU routing, milestone 1).** One job used to fence a whole box: a lease named no cards,
so a render on one card of a three-card node held every card, queued every seat load and every other session, and a
lease whose owner had gone read as a green "working" for as long as its wrapper lived. This release makes the lease
card-scoped and owned:

- A lease can name its cards (`gpu reserve --devices`, `--cards N`, the allocator), and every consumer that used to
  read "a lease exists" now reads "a lease sits on THIS seat's cards": the text-load gate, placement, the agent-lane
  fallback, drain, unload, the render lane and the per-card run cap. Free cards keep taking work.
- `gpu status`, `gpu cards` and `offload_status` show a per-card table; the delegator ranks nodes on free cards and
  ranks an overdue lease last.
- A lease records its owner and a progress contract; a lease whose attended owner is gone reads `held-orphaned`, one
  whose progress stopped reads `held-stalled`, one past its window `held-overdue`. These are reported, never acted on.
- ComfyUI can run one instance per card (port, output and temp directory, ownership marker per card, pinned by GPU
  uuid); nothing launches them yet (plan P13b).
- **Off by default.** Card-scoped writing is behind `gpu_card_scoped_leases` and inferring the cards of a lease an
  older binary wrote is behind `gpu_legacy_scope_inference`; both stay off on a host until `gpu doctor --write-audit`
  is green there (plan P6). With both off, a whole-node lease fences what it always fenced and unloads no seat.
- **Always on, by design** (with both switches off): the ownership verdicts (`held-orphaned`, `held-stalled`,
  `held-overdue`, `tree-orphan`) and the `orphan.<epoch>` marker the first observer writes; fleet health publishes
  `overdue` (and reads an overdue held lease as busy) plus `orphaned` and `stalled`, and the delegator ranks such a node
  last (a 0.160.0 delegator, which decodes `busy`, excludes a node whose overdue lease is non-text); the per-card run cap
  counts every seat's runs on a single-card seat's card on a box with layers; a placement reason names a lease holding the
  home seat's cards; a wrapped command sees `GPU_LEASE_DEVICES` (empty for a whole-node lease).

Also in this release: the broker decision audit trail on every agent door (`audit_all_doors`), a read floor for secret
material on every agent read tool (`agent_read_floor`), a per-run hash chain over the audit trail with `agent-audit
verify` (`audit_chain`), `agent_run` sizing its wall before it starts (D-102, partial), and Windows nodes keeping the
harness's data off the OS drive (C-92).

### Fixed — the tailnet zone a config load installs is safe for two loads at once

Every config load installs `tailnet_suffix` into a process-wide value that the endpoint checks read. It was a plain string,
so two loads in one process (a server re-reading its config while a request handler loads it; the card-lease tests that
drive two `gpu reserve` calls in-process) raced on it. The Linux race gate on this release found it eight times in one run,
all on that value, through `TestGPUReserveConcurrentCardRequestsLandOnDistinctCards`,
`TestGPUHoldCardsAllocatesAndClaimsInTheHolder` and `TestGPUReserveDevicesRunsTwoDisjointLeasesAtOnce`. The zone is now
an atomic value and `TailnetURL` reads it once per call, so its checks and its error message judge one value. Every other
setter a config load calls was already behind a mutex or an atomic. Test: `TestTheTailnetSuffixIsSafeForConcurrentLoads`
(a data race before the fix, clean after).

### Added — a hash chain over each agent run's audit rows, and `agent-audit verify` (register SF-08)

- `audit_chain` (default `false`): every row a run writes on the broker audit trail carries `run_id`, `seq` and
  `prev_sha256` (the SHA-256 of the run's previous row exactly as written), and a run that recorded anything closes
  with one `run_end` row carrying its row count and head (a run with no decision writes nothing; a second close
  writes nothing). One chain per run; concurrent runs interleave in the file and keep valid chains (eight
  concurrent writers, and eight callers sharing one trail, verified). Off, the rows keep today's shape byte for
  byte.
- `local-offload agent-audit verify [--file PATH] [--json] [--strict]` checks every chained run and exits 1 naming
  the run and the seq where a chain breaks inside a run, with the cause (a row edited; or removed, reordered or
  inserted). A run with no `run_end` is reported OPEN (a crash, or a cut tail, which the chain alone cannot tell
  apart) and `--strict` fails it; chained rows after a `run_end` are reported LATE (a tool that kept running after
  its run closed), not broken.
- What it cannot see, by design of an unkeyed per-run chain: a whole run removed, edits to a `run_end`'s other
  fields, and an edit re-chained from that row onward. An off-node witness of run heads (S-08) closes that and is
  not built.
- Every door closes its run: `agent_run`, `agent_delegate`, fleet contracts, the local-agent CLI (single, two-tier
  and queue runs before any exit; a `--serve` process is one run for its lifetime, closed when serving ends).
- Tests: a clean chain; edit, delete, reorder and last-row edit each named by run, seq and cause; an edited
  `run_end`; a failed write that does not advance the chain; eight concurrent writers and one shared trail; an
  empty run and a second close writing nothing; a late row; a cut tail reported open and failed by `--strict`;
  the off shape (no chain field); Build and the contract door chaining and closing a run; the verify verb.

### Added — a read floor for secret material on every agent tool that reads files (register SF-07)

The rule table gated writes, deletes, fetches and browse, but reads went through `os.Root` confinement alone: any
agent run could read `.env`, private keys or credential files inside its worktree and hand them to the model.

- One read gate, shared by every tool that reads file content: `read_file`, `summarize_file`, `search_files`,
  `edit_file` (its old-content match would otherwise answer whether a guess is in a secret file) and
  `github_upload_file` (it sends the file off the box).
- The built-in floor: `.env*` (except `.env.example`, `.env.sample`), `*.pem`, `*.key`, `*.p12`, `*.pfx`, `id_rsa*`,
  `id_ed25519*`, `id_ecdsa*`, `id_dsa*`, `.npmrc`, `.netrc`, `.pypirc`, `.git-credentials`, `.claude.json`, and
  everything under a `.ssh`, `.aws`, `.gnupg`, `.kube` or `.env` directory, folded like the write floor. Each
  path is checked as given and resolved, so a symlink to a secret and a Windows 8.3 short name (`ENV~1.PRO`,
  `SSH~1`) are caught; under `enforce` a `~N` segment is refused outright too.
- `agent_read_floor`: `warn` (the default) lets the read through unchanged and records a `warn` row and a log line,
  and refuses a read an ENFORCING trail cannot record; `enforce` refuses (the broker's not-performed convention)
  and records a `deny` row; `off` checks nothing. Under `enforce`, `search_files` never opens a floored file and
  says nothing about what it skipped, so a pattern cannot probe a secret's content. `--rules off` turns the floor
  off with the rest of the table, and the build says so; read rules loaded under `warn` are flagged as recorded,
  not refused. An unknown value refuses the config load. Every door passes the key: `agent_run`,
  `agent_delegate`, fleet contracts and the local-agent CLI.
- Not covered, and said so: the run/shell cage (allowlisted interpreters can read any file they reach), hard
  links, and the media tools that read an absolute image path. Tool sets built outside Build (the read-only
  replay agent) check nothing.
- Fixed with it: `search_files` on the ripgrep backend skipped dotfiles while the Go walk searched them; both now
  search dotfiles (`rg --hidden`, `.git` excluded).
- Tests: the floor's hits and misses, each mode per tool, the search oracle (a matching and a non-matching probe
  give identical output), a search rooted in or above a credential directory, an 8.3 short name under enforce and
  under warn (Windows), a symlink to a secret (where links can be made), the strict trail, the build notes, the
  contract and MCP doors, and the config set; 23 mutants, each killed.

### Added — the broker decision audit trail on every agent door, behind `audit_all_doors` (register SF-02)

The agent doors attached the broker audit trail only when a run asked for browse, so a write-door contract
(`agent_delegate`, fleet contracts) decided allow and deny on its writes with no record, and the trail had been an
empty file for weeks. The new key `audit_all_doors` closes that, default off. Rows come only from brokered actions
(write, delete, fetch, shell, run, browse): a read-only run attaches the trail and leaves no row.

- `off` (the default, also an unset key): unchanged, a trail only when browse asks for one; zero new rows.
- `warn`: every agent door attaches the trail and writes every decision; a failed write is reported once on the log
  and never turns an allow into a deny (the doors keep their failure modes).
- `enforce`: a failed write denies the action, as a browse run's trail always has, and the denial names the key and
  the way out; a door whose trail cannot resolve defers by class `config`, naming the key and the way out. It adds a
  failure mode, so it is the operator's call after a clean window under `warn`.
- A browse run keeps its enforcing trail in every mode. An unknown value refuses the config load, naming the key,
  and a one-shot verb that runs on past the load error gets a refusing door instead of a door with no trail.
- The local-agent CLI keeps its own `--audit` behaviour and does not read the key.
- Tests: config validation and resolution, advisory versus enforcing write failures (warned once), the door plan
  per mode, a browse build never made advisory, a writing contract leaving rows under warn and enforce and none
  under off, enforce with no home deferring before any seat call, a broken trail per mode at the contract door
  (warn keeps the diff, enforce denies the write with the key named), the MCP `agent_run` handler refusing a
  mistyped or unresolvable enforce, and the MCP door following the config. Mutants listed in the PR.
### Fixed — `agent_run` sizes its wall before it starts and refuses one that cannot hold any answer (register D-102, PARTIAL)

`agent_run` runs its loop under a context deadline (the delegation door's walls became expectations in 0.131.0, ADR 0055;
this door never adopted the liveness monitor) and computed no sizing, so a 420 s wall against an estimate of 928 s
(`min_turn_sec` 807 s) ran ten steps and died at the seat's deadline (2026-09-15).

- **Sizing, before anything touches the seat.** `Pipeline.SizeRun` prices the call from the seat's remembered rate (the
  rate store, else `agent_seat_tok_s` for the box's own agent seat only) with the delegation door's own arithmetic. It reads
  one local file and dials nothing, so it runs ahead of the fence check, the cordon, the warm-up and every probe.
- **A rate is never lent to another seat.** `agent_seat_tok_s` is the planner seat's rate. A model the caller names, or the
  seat composite placement picks, that is not the box's agent seat is sized from its own seat-rates entry or not at all: no
  rate means no floor, no refusal and no published estimate (`wall_note` says there is no sample). Before this a named model
  was priced at the agent seat's rate, so a wall the named model could hold was refused with that model in the refusal.
  The delegation door's node is NOT changed: `SeatPolicyFor` still applies `agent_seat_tok_s` to any seat the node runs (a
  placed or overridden one) that the store has no entry for, so the node and this door still price such a seat differently.
  Register C-56 plans a node-side refusal on `est.Below`; it must stop lending the rate first, or it refuses a wall at the
  wrong seat's rate.
- **The refusal.** A wall that cannot hold even one tool step and a 64-token final at the seat's rate (`seatrate.MinViableSec`)
  is refused before step 1: `deferred: true`, `defer_class: budget`, `steps: 0`, zero requests to the seat, and a reason with the
  wall given (`timeout_sec`, or the box default `agent_timeout_sec`), the floor it needs, the run's estimate and `min_turn_sec`,
  and the `timeout_sec` that fits. The floor is one tool step plus the final whatever `max_steps` says, so a smaller step
  budget never clears the refusal and the reason does not offer one. A seat with no rate has no floor and is never refused.
- **The published numbers.** Every local `agent_run` call that gets past argument validation and seat placement carries `wall_estimate_sec`,
  `min_turn_sec` and `wall_note` under the delegation wire's names, answered or deferred: the wall refusal, every deferral after
  the gate (the served-roster check, the GPU fence, the cordon, the seat cap, both coherence deferrals, the window probe, the
  profile) and a run that dies at its wall all say what the wall was weighed against, which is what a caller re-placing a
  capacity defer needs. One closure (`deferSized`) stamps them, and a source guard fails when a deferral builds its own
  result. A call refused before a seat is chosen (an empty goal, bad `setup_actions`, a bad `read_root`, the browse options, or
  a composite placement guard refusing the seat: there is no seat to size) and a call sent through `route` (the node sizes that
  contract, where the wall is an expectation) carry none.
- **Deviation from the row, and why.** The row asks for the refusal when `timeout_sec < min_turn_sec`. That figure is a cold
  load plus the configured max final, and the INV-5 rider (ADR 0050, recorded two days after the row) forbids refusing on it:
  it would shut out every measured-working tier whose wall sits under its worst case (a 900 s wall on a 7 tok/s 27B seat).
  The floor is therefore the rider's minimum viable final, the same one the delegator applies to a remote placement
  (`feasibleFinal`); `TestMinViableSecMatchesTheDelegatorsFeasibilityFloor` fails when the two drift. A wall between the floor
  and the estimate runs, with `wall_note` naming the gap.
- **What this does NOT cure — D-102 stays PARTIAL.** The row's own case is still admitted: a 420 s wall against a
  `min_turn_sec` of 807 s and an estimate of 928 s is above the floor (about 45 s at 5 tok/s, 13 s at 30 tok/s, and the
  floor only exceeds 420 s below roughly 0.46 tok/s), so it runs and can still die at the hard deadline; the refusal fires
  only on walls below the floor, which no default wall is. What the change adds for that case is the diagnosis, in
  `wall_estimate_sec` / `min_turn_sec` / `wall_note`. The cure for a hard-deadline door is either to install the
  final-budget fit (D-95: the final answer's token budget is fitted to the wall that is left, as the node's loop does) so a
  short wall becomes usable,
  or an operator decision to accept a literal `min_turn_sec` refusal and amend the INV-5 rider; this door does neither yet
  (register D-134 carries the cure).
- **Tool description and docs.** The `agent_run` description and its `timeout_sec` field now state the default chain, that the
  wall is a hard deadline here, that slow seats defer on short walls, and the refusal; OPERATOR-GUIDE (the timeout chain),
  `docs/systems/mcp-server.md` and the README row carry the same.
### Fixed — Windows nodes no longer keep the harness's data on the OS drive (register C-92)

Operator rule: C: holds Windows and program installs, never data. With no `home` key the harness keeps its cache, ledger,
media and svg output, delegation log, pipeline jobs, footprints and the coding agent's audit trail under the user profile,
which on Windows is the OS drive. `install.sh` has always written `home` from `install volumes`; `install.ps1` never did, the
config template spelled sixteen data paths as `~/.local-offload/...`, and `doctor` said nothing.

- **The default is decided once, by the installer, and recorded.** `install.ps1` Step 8 writes `home` into a fresh
  `config.json` from `local-offload install volumes --data` (the same most-free-space, never-the-OS-drive rule `install.sh`
  uses, plus the data-volume rule below), through the pure `Get-DataHome`. `OFFLOAD_DATA_HOME` names another directory; `OFFLOAD_ALLOW_OS_DATA=1` is the explicit,
  recorded decision to keep the data on the OS drive. With neither and no qualifying volume the install FAILS at Step 8 and
  names both, never a silent fallback to C:. The choice and its reason go into `installed.json` (`data_home`,
  `data_home_because`). The runtime default does not change: a default that depended on the mounted volumes would re-point
  every existing node at an empty tree on upgrade, which is moving live data silently. An existing `config.json` is never
  rewritten and an explicit `home` is kept as written; the installer prints a NOTE when an existing config names none.
- **`doctor` FAILs it.** A new `data volume` section (before the health probe, so a down serving layer cannot hide it) lists
  every data location that resolves onto the OS drive while a data volume qualifies to hold it (see the data-volume rule below), prints the install root once
  with the paths that follow it folded in, and gives the way off. A box with no other qualifying volume gets one note and no
  FAIL; a non-Windows host prints nothing. `state_dir` and `gpu_lock_path` (the machine-wide lease root) are not data and are
  never listed. New packages: `internal/datahome` (the audit and the copy), `internal/volumes` gains `DriveOf`, `OnDrive`,
  `DriveRootOf` and `OSDriveRoot`.
- **`local-offload data status` and `data migrate`.** `status` is the audit as a table or `--json`; it exits 1 exactly when
  doctor would FAIL. `migrate [--from DIR] [--to DIR] [--apply] [--stopped]` is the way off C:: a COPY (never a move, never a
  junction, the source tree is not touched), a dry run until `--apply`, to a fixed directory on the volume the install-volume
  rule picks when `--to` is not given, and never onto the OS drive. The bbolt stores (`*.db`) are held back until `--stopped`
  says fleet-serve and the MCP doors are stopped. Every file goes to a temp name, is verified by re-reading it, takes the
  source's mtime and only then replaces the destination; a file that changes under the reader is retried and then reported;
  a destination newer than its source is kept, so re-running after the switch cannot clobber live data; links are reported
  and not followed; `config.json` is not carried. An apply that held back or could not read anything exits non-zero, and the
  output ends with the exact `home` line to set.
- **The template follows `home`.** `setup/templates/config.json` no longer spells its sixteen `~/.local-offload/...` data
  paths. A path written in the file is an explicit value that `home` does not rebase; the literals only equalled the defaults,
  so they rebased by luck and stopped doing so when `$LOCAL_OFFLOAD_HOME` was set. The defaults supply the same values.
- **The coding agent's files follow `home`.** `local-agent`'s audit trail, ask queue and traces, and the browse grant's audit
  path (`agent.DefaultAuditPath`, now taking the install root), were pinned to `~/.local-offload`; they hang off the install
  root now (`agent.StateFile`), with the old location as the fallback when no root is known. **Behaviour change on nodes
  that already set `home`** (every `install.sh` node): the trail, the ask queue and the traces start new files under `home`
  and the old ones stay under `~/.local-offload`. Nothing moves or merges them, and `data migrate --from <home>` cannot carry
  them because they sit outside that tree. `local-agent` (for a default path, not for `--audit`, `--ask-queue` or
  `--traces`) and the browse audit path of `agent_run` / `agent_delegate` now write one note to stderr per process, naming
  both paths, when the old file or directory exists; merge it into the new one by hand if you want one history, then delete it
  to silence the note. A node with no `home` is unchanged.
- **The data-volume rule: never a cloud-synced virtual drive or a FAT volume.** The FAIL text, the default `data migrate`
  target and the install-time `home` all took the install-volume rule's most-free-space answer, and a sync client's virtual
  drive (Google Drive for desktop reports FAT32, is neither removable nor a network share, and shows a cloud quota's free
  space) can outrank every real disk. `volumes.PickData` is `Pick` over the volumes `DataVolumeReason` allows: it skips a
  label or mount-path segment naming a sync client (the names the GPU lease already refuses as a state directory, kept in step
  by a test that reads that source) and any FAT-family filesystem, records what it passed over in the choice's reason, and
  names those volumes when nothing is left. `datahome.DataTarget` is the one filter doctor, `data status` and `data migrate`
  share. `install volumes --data` applies it (marks the rows it will not use, puts `"data_target": true` in the JSON), and
  `install.ps1` always passes it through `Get-DataVolumeArgs` while `Get-DataHome` refuses a choice without the marker.
  Without `--data` the answer is unchanged (`install.sh`, model placement). `Pick` itself is untouched.
- **Two defects found in review, fixed.** `DriveOf` and `DriveRootOf` stripped a long-path prefix spelled with one backslash
  per side; Windows writes two before the question mark or the dot, so a config path or `--to` written
  with a real verbatim or device prefix was not seen as being on the OS drive, and the tests passed because their fixtures
  carried the same mistake (the UNC fixtures too). `data migrate` ran the OS-drive guard on the typed `--to`, which `DriveOf`
  reads as drive-less when it is relative or rooted without a letter, so `--to relhome` from a directory on the OS drive was
  accepted and, with `--apply`, copied onto it; `--from` and `--to` are now resolved to absolute paths before any check, and
  the `home` line the verb prints is absolute.
- **Out of scope, still open:** the ComfyUI `input`/`output` folders under the ComfyUI install (media-lane feedback item 4),
  and the stack directory `$OFFLOAD_HOME` (default under the user profile), which `install volumes` does not yet place on
  Windows.
- **Proof.** 71 new Go tests and 35 new PowerShell assertions (`internal/volumes`, `internal/datahome`, the root package,
  `internal/agent`, `internal/mcpserver`, `internal/pipeline`, `cmd/local-agent`, `setup/tests/install-config-seed.test.ps1`).
  The drive helpers, the audit, the copy, the doctor section, the template test and the `StateFile` tests were written
  before the code and failed by assertion on the pre-change tree: the template test lists the sixteen pinned paths and, with
  `LOCAL_OFFLOAD_HOME` set, the media, cache and ledger landing on the user profile. The `data` verb tests and the three
  wiring tests for the browse audit path and the agent's files were written after the code; they fail on the base tree too
  (they call functions that are not there or lint for call sites that are not there). 61 mutants of the guarded lines
  (exact-count replacement, restored by bytes and sha256, compile errors re-run as compile-safe mutants) were each killed by a
  test except one that is equivalent (a guard `OnDrive` already makes redundant); two survivors at first, a same-size rewrite
  the copy did not recopy and an unpinned `OFFLOAD_ALLOW_OS_DATA` line, each drove a test. An existing pipeline test caught
  the one regression: with no user home `config.DefaultBase()` answers a bare `.local-offload`, so `StateFile` treats a
  relative root as no root and the browse grant is still refused.
  A second pass fixed what review found (22 of the 71 tests, 11 of the 35 assertions). Each fix has a test that failed by
  assertion before it: the long-path cases against the one-backslash code (the fixtures are now built from the backslash code
  point and pinned byte by byte), the relative and rooted `--to` against the unresolved guard (through a seam on any platform
  and through the real working directory on Windows), a Google Drive FAT32 volume with the most free space against `Pick` in
  the audit, `data migrate`, `install volumes --data` and the PowerShell gate (run against the pre-fix `install.ps1`), and the
  split-trail note against a stub (that one, unlike the others, did not fail against the pre-fix code). 41 more mutants of
  the new lines (exact-count replacement, restored by bytes and sha256) were each killed by a test in the end: 38 died on the
  first run, one of those by a nil-writer panic rather than an assertion and one by a compile error that was re-run
  compile-safe; three survived at first (a qualifier with no space before it, the error when the cloud drive is the only
  volume, an unresolved `--from`) and each drove a case. A first red run of the relative-target test copied a fixture
  tree to a drive root and into the package directory; the guard tests now run from a scratch directory and only plan a
  rooted target, so a mutant cannot write outside it.
- **Docs.** `docs/systems/setup-installer.md`, `docs/OPERATOR-GUIDE.md`, `docs/systems/coding-agent.md`, `setup/SETUP-AGENT.md`
  and `README.md`; the four places that still gave the audit trail's location as fixed (`README.md`, `CLAUDE.md`,
  `docs/OPERATOR-GUIDE.md`, `docs/systems/browse-lane.md`) and a dated amendment each to ADR 0004 and ADR 0060.
### Added — per-card ComfyUI instances, render layer (plan P13a; nothing sets them yet)

- **A keyed ComfyUI instance, one per card.** `COMFY_INSTANCE` (an explicit key) or `COMFY_CARD_UUID` (the card it is
  bound to) names an instance that owns its port (`--api` / `COMFY_API`, else `COMFY_PORT_BASE` plus
  `COMFY_INSTANCE_INDEX`), its own folder `<comfy dir>/instances/<key>` (`output` for `--output-directory`, and the
  `--temp-directory` value whose `/temp` ComfyUI appends itself, so it can never sit under the default instance's
  `temp`, which that instance wipes on every start), its launch marker (`.offload-launch-<key>.json`, with the owner pid
  and the lease epoch from `GPU_LEASE_EPOCH`) and its console log (`offload-comfyui-<key>.log`). The port is bind-checked
  before launch on the addresses ComfyUI will listen on (127.0.0.1 unless its `--listen` says otherwise, not the host the
  api spells): a port held by something that is not ComfyUI is refused (`COMFY-PORT-TAKEN`) and the holder is never killed.
- **Card pin by uuid.** A card-bound instance gets `CUDA_VISIBLE_DEVICES=<uuid>` in its child env and no `--cuda-device`
  (that flag counts in CUDA's fastest-first order, not the driver's, so an index lands on the wrong card). A device index
  with any instance key, explicit or card-derived, is refused (`COMFY-INSTANCE-CONFLICT`); an explicit key with no card is
  allowed but logs that nothing pins it. A running instance is reused only on proof that it is that instance: a keyed
  marker (this key and port, live pid, the exact argv, the instance's own `--port` and `--output-directory`) and, for a
  card, the uuid it was launched on. A ComfyUI the harness did not launch (another key's, another launcher's, a foreign
  one) is refused (`COMFY-PROFILE-MISMATCH`) and never stopped; one the harness launched on the wrong card is stopped and
  relaunched only when its spawner is gone.
- **Runners and Go.** Every render runner resolves its endpoint through the instance. For a keyed instance `withGpuSlot`
  launches, frees and log-tails that instance (the launch and the post-run `/free` ignored `--api` before); an unkeyed
  instance on the default endpoint is unchanged, and one on another endpoint is reused when it answers and refused
  otherwise (see the review pass above). `run-graph` starts
  and frees its own ComfyUI the same way (`instanceDeps`, tested by behaviour), and a bad instance env is a typed
  `RUN_ERROR` defer, not an untyped exit 1. Runners stage their inputs under names unique across processes
  (`render/comfy-input.mjs`: clock, pid, random suffix, sequence), since concurrent instances share one input directory.
  `imagegen.ComfyLaunch` gains `API` and `CardUUID`, `gpugen.Spec` gains `CardUUID`; both reach the runner env and the
  `/free` goes to the instance that ran.
- **Unchanged by default.** With no instance env the default instance's argv, marker, log, spawn env and calls are the same
  as before (pinned by tests). Docs: `docs/systems/media-generation.md` (per-card instances). The admission, per-card slot
  and waiting-place token that give a caller a card to ask for are the next phase (P13b).
### Fixed — review of the per-card consumers (plan P4 and P5, register C-86)

The legacy-lease inference and the seat yield did more than the plan asked and less than they promised. What changed, all
test-first, with the reads that write and the reads that can unload stated exactly.

- **Inference reads only what an older binary wrote, and only when the host asks.** Every record this binary writes now carries
  `Meta.Format` (`gpulease.RecordFormat`; `Info.Legacy` is the reading), and a whole-node record that carries it is the whole
  node by its writer's word: an explicit `gpu reserve --whole-node`, a reserve on a host with the writer flag off and the
  pipeline's own media lease were being narrowed by a `--cuda-device` in their command line or tree, which contradicted "a
  whole-node lease fences what it always fenced". The inference is also **off by default**: new `gpu_legacy_scope_inference`
  (`config.example.json`) turns it on per host, and `gpu status` names the key while it is off. The read-only capture of a real
  legacy tree (plan P4 step 1) is still owed, so nothing narrows a lease on a guess before it.
- **The sticky sidecar is shared state and is merged.** `seen.<epoch>` is read, unioned and written under the epoch lock, never
  overwritten from a snapshot taken before the evidence was gathered, so a slow reader can no longer shrink a wider set; the
  ledger line is written once, by the process that changed the set on disk; concurrent first readers share one gathering; no
  sidecar is written for a lease that has ended; `seen.<epoch>` left behind by an older binary's release is swept by the next
  acquirer (whole-node or device grant), which is why the old "removed when the lease is released" held only for this binary.
- **Evidence is not trusted past what it proves.** One command line naming a card that cannot be placed keeps the lease
  whole-node whatever else resolves (and no marker or sample may narrow it either); a record with no holder pid is never walked
  (pid 0 is the system process); a wrapper root that started after the lease is a recycled pid and its tree is not read; an
  unreadable process table is named in the reason, and an unwritable sidecar or ledger is warned and reported (`not sticky
  across processes`), not folded into "inferred".
- **Reads write no scope state, and nothing a read does unloads a model.** The inspectors (`gpu status`, `gpu cards`,
  `offload_status`, the activity snapshot, `CardsHeld`, `delegate.LocalLease`) report the scope from the same evidence and
  write no `seen.<epoch>` sidecar, no scope-ledger line and make no call to llama-swap. (One write remains on these read
  paths by design, from lease ownership (P8, ADR 0070): the first reader that sees a lease's owner gone writes the
  `orphan.<epoch>` marker that starts the orphan grace clock; it records an observation and never changes a lease.) (`modelaffinity.PeekLease`, `ScopeInfo`); only the load gate's own reader
  (`InspectLease`) remembers what it sees. The widening sweep (`yieldOnScope`) is **removed**: it unloaded seats on a plain
  status read and on scope *establishment* (the plan says only widening), and it was armed with the config's llama-swap
  endpoint even for a config aimed at another box. A seat the scope has spread onto now yields on its own release, which is the
  plan's seat race rule; an idle resident seat waits for its idle ttl. The seat-yield endpoint is no longer armed from config at
  all (`SetYieldProtect` keeps only the memory stack).
- **The vision pre-check is scoped.** `gpulock.WaitFreeScoped`: the pipeline waits, and defers `gpu_busy`, only for a lease on
  the vision seat's cards. Before, the delegator's auto route ran a call locally because the lease did not touch the vision seat
  and the pipeline then waited `vision_gpu_wait_sec` (90 s) on any live lease and deferred it, a call the fleet used to serve.
- **`device-trespass` says "possible".** Card-scoped leases make text seats on the free cards legal, and the registry knows only
  two doors' runs, so the note no longer states that the holder strayed; it names what it cannot rule out.
- **Failures are said.** A seat whose state cannot be read on a held card, a card table, a roster or a lease epoch that the
  scoped drain/unload cannot read (it falls back to the whole node), and a sidecar or ledger that cannot be written each print
  one line instead of falling back silently.
- **Known narrower than the plan.** The seat race rule re-checks after residency only for an `Admit`-gated batch's release and the
  delegation door's warm-up, not for loads through `AwaitUpstream` or `AwaitModelRoute`; an idle seat on a card a scope just
  spread onto is not swept. Both are listed in the gpu-lease Known gaps.

### Added — drain, unload and the render lane clear the leased cards, not the node; per-card run cap; `device-trespass` (register C-86, plan P5)

Draining one card no longer empties the others. A whole-node lease reads exactly as before.

- **`--drain` and `--unload-seat` take the cards the lease holds.** `maintainSeatScoped` (the old entry points keep their
  signatures) receives the lease's card ids from the wrapper and from the detached holder's maintain step. The configured agent
  seat is drained and unloaded only when it is on the leased cards; the other resident models on the leased cards, and the ones
  nobody declared a pin for, are unloaded; the rest are left resident and named; the memory stack is never unloaded; the
  total `GET /unload` fallback is refused while a model that must stay is resident. A pin that cannot be placed, or a card table
  that cannot be read, reads as "on the leased cards" (today's behaviour).
- **The drain waits only for runs on the leased cards.** `gpuactivity.Run` gains `Devices` (the pins of the seat it runs on, set
  by the delegation door and the `agent_run` door); a scoped drain waits for registered runs whose pins intersect the lease,
  by seat name for a record that predates the field, and skips the agent seat's own gauge when that seat is on other cards.
- **The render lane unloads the list the wrapper hands it.** `gpu reserve` exports `GPU_LEASE_UNLOAD_MODELS` to a command it
  wraps under a card lease (the roster minus the memory stack minus the seats pinned to other cards). `render/gpu-lock.mjs`
  `freeLlamaSwap` unloads that list (new `parseUnloadModels`: unset or empty is today's rule, `-` is none) instead of every
  model off the memory stack; the stack never leaves whatever the list names, and the total-unload fallback is refused while a
  model outside the list is resident. A whole-node lease or an unreadable roster exports nothing.
- **A single-card seat's run-cap line is its card's.** `gpuactivity.Registry.OnSeatPinned` counts the runs on the card whatever
  seat they run on, in the node's gate and in `delegate.localRunCapRoom`; a seat that spans cards keeps the per-seat line.
- **`device-trespass`.** The activity view (`gpu status`, `offload_status.gpu_lease`) flags a card outside a bounded lease's cards
  that is busy with nothing the harness knows of to explain it (not the display card, not a card another lease holds, not a card
  a registered run is pinned to, nothing while the seat itself is busy); `device_trespass` in the JSON, `possible device-trespass:`
  in the note, no new verdict word. With a bounded lease `held-working` now means its own cards are busy.
- **Not in this change.** The pipeline's own media lease is whole-node until plan P13, so it exports no unload list; fleet health
  per card is P7; nvidia-smi utilisation on Windows includes the desktop on every card but the display card, and the trespass
  flag inherits that.

### Added — consumers read a seat's cards, not the node: text gate, busy formula, agent-lane fallback, legacy-lease scope, seat yield (register C-86, plan P4)

A lease on one card no longer fences the seats on the others. Until a host turns card-scoped leases on (plan P6) the leases on
it are whole-node ones and fence what they always fenced, with one exception: a legacy lease (one an older binary wrote) on a
host that has set `gpu_legacy_scope_inference` (off by default), whose cards the evidence rule below can name. Nothing narrows on a guess: a model nobody declared a pin for, a pin the card table cannot place, an
unreadable card table, a whole-node lease and a long-context contract all read as every card, as before.

- **The load gate asks about the seat's cards.** `config.ModelPins` reads a model's device pins from the layers (a seat's
  model, a router seat's twins, the cascade rungs of the router seat that declares no model); `config.Load` arms them with
  `gpu_comfy_order`, `comfy_dir` and the memory stack. `gpulease.Info` gains `EffectiveDevices`, `Touches` and `For`, and
  `gpulease.ResolvePins` turns pins into lease ids; `Admit`, `AwaitRunSlot`, `AwaitUpstream` and `AwaitModelRoute` wait only
  for a lease on the seat's cards. The card table is read only when a held lease names cards, memoised for 2 s.
  `delegate.Fenced`, `ForeignFence`, `Reserved` and `modelaffinity.BlocksNewRun` keep their signatures; `...For` forms and
  `modelaffinity.ScopeToPins` are the narrowed questions. The delegation door, the `agent_run` door and the review lane's local
  loop pre-check the same narrowed lease the cordon reads.
- **The delegator is busy for a contract only when every local agent seat it could use is on a held card.**
  `delegate.LeaseForContract` and `placement.AgentChain` (the list the table walks) narrow the local lease per contract; the
  busy formula in the auto deal, the capacity wait, the retry fence and the spread deal read it, and the text and vision auto
  routes read the cards of their own seats. `LocalBusyFor` and `LocalLeaseFor` are the same questions for a caller that knows
  its seat.
- **The agent lane falls back to a free card.** Placement row 5c: when the home layer's agent seat fits and a lease holds its
  cards, the next declared layer that is not opt-in or dormant, whose agent seat fits the window and whose cards are free,
  takes the contract (the single layer's card-0 seat when a card-2 render holds the flagship's cards). With none free the
  contract keeps the home seat and queues at its gate: a busy card is still a place in line, never a refusal. New
  `placement.Live.CardsHeld`, armed from the local snapshot; a remote row's `Live` has none.
- **Legacy whole-node leases are scoped on evidence.** `gpulease.Scoper` infers the cards of a lease an old binary wrote from
  (a) the recorded command and the command lines of the wrapper's process tree (`--cuda-device N`, resolved through the
  declared `gpu_comfy_order`, never guessed), (b) the ComfyUI launch marker only when it is tied to the lease (its pid or
  launcher in the tree, or started after the lease and claimed by no other), (c) the tree's cards sampled twice five minutes
  apart on a lease at least ten minutes old; otherwise it stays whole-node and says exactly why. Sticky and never shrinking
  (`lease/seen.<epoch>`, removed on release by this binary and swept by the next acquirer otherwise), widening writes a line to `gpu/scope-ledger.jsonl`, the display card is never
  reported free by an inference while the operator may be at the desk, and the record itself is never rewritten. New
  `gpulease.ProcessTree` (Toolhelp and the PEB on Windows, `/proc` on Linux). `gpu status` (text and `--json`), the per-lease
  rows and `offload_status` show `seat_scope`, `inferred_devices`, `scope_widened` and `scope_why`.
- **The seat yields a race.** A load that passed the gate before a lease claim lands after the lease's unload, on its cards:
  when the batch that may have loaded the model drains, and after the delegation door's cold-load warm-up (which then defers
  `capacity`, re-placeable), the seat re-reads the lease and unloads itself if one it does not hold fences loads on its cards
  and the engine shows nothing in flight. A widened inferred scope reaches a seat the same way, through its own release (no read
  of the lease unloads anything). Never the memory stack, never the long job, never a process.
- **Measured.** Of the three-card tier's nine declared seats, three stay placeable while a lease holds card 2 (the single
  layer's router and agent on card 0, and the dormant display twin), three under card 0, seven under the display card
  (`TestSeatsThatStayPlaceableUnderACardLease`).
- **Not in this change.** The read-only capture on the three-card box of what a real legacy lease's tree shows (plan P4 step
  1): the lease that prompted it has ended, so the rule is built on synthetic fixtures and the capture is deferred to the
  milestone that enables card-scoped leases on a host (P6). The Windows process-tree reader was run against a child process the
  tests start; the Linux reader is built and vetted, not run. Drain and unload scoped to the lease's cards are P5 (next entry above); fleet health
  per card is P7; the cascade-lane busy gates still read any lease as busy; the load gate arms no presence reader, so an
  inferred scope always keeps the display card.

### Added — `gpu reserve --devices|--cards`, the per-card status, the card allocator and `gpu doctor` (register C-86, plan P3)

A reservation can now name its cards, ask for N of them, or take the ones its command pins itself to, and `gpu doctor` is the
audit that has to be green before a host may write a card-scoped lease. All of it sits behind the per-host switch from the
change below: a host without it reads no card table for a reserve that names nothing, derives nothing from the command, and
holds the whole node exactly as before (`TestPlanFlagOffHostNeitherReadsCardsNorDerivesDevices`,
`TestGPUReserveFlagOffKeepsTheWholeNodeLeaseWhateverTheCommandSays`).

- **Choosing cards.** `--devices` (nvidia-smi indices or UUID prefixes), `--cards N|MIN..MAX` (the allocator), `--whole-node`,
  `--group`, `--vram` and `--ram` (per-card VRAM and host RAM the job needs, for the allocator). Mutually exclusive; on a host
  without the switch the first two are `ErrCardScopedOff`, naming `--whole-node`, never a silent whole-node lease. With none of
  them, the cards the wrapped command names itself become the lease: `CUDA_VISIBLE_DEVICES` (a UUID, or an index in PCI order
  under `CUDA_DEVICE_ORDER=PCI_BUS_ID`, else in ComfyUI order), else `--cuda-device`, else `COMFY_CUDA_DEVICE`. Evidence that
  cannot be resolved falls back to the whole node with a stderr note. The command is handed `GPU_LEASE_DEVICES`, and a command
  that names no card of its own is pinned to the cards it named or was allocated (`CUDA_VISIBLE_DEVICES` by driver UUID and
  `CUDA_DEVICE_ORDER=PCI_BUS_ID`); a lease alone confines nothing (see the review fixes below). The detached
  holder (`gpu hold`) takes `--devices` and `--group`, and `--detach` finds its own lease by pid among every live lease instead
  of reading the lowest epoch as the holder.
- **The card table.** `gpu cards [--json]`, a card table in `gpu status`, and `gpu_cards` in `offload_status` with
  `section: "brief"` (a new key; `gpu_lease_verdict` is unchanged and the full default answer is byte-identical): per card the
  UUID, nvidia-smi index, name, display flag, free and total VRAM, the ComfyUI order, and the holding lease (epoch, class,
  group, command, scope). `gpu status --json` gains `cards`, `leases`, `card_scoped_leases`, and its `queued` rows gain
  `devices` and `scope`; no existing key changes type. Without nvidia-smi the verbs print the leases and say there is no table.
- **The ComfyUI order is declared, not guessed.** nvidia-smi reports no FASTEST_FIRST position, so the table shows `?` and a
  command naming `--cuda-device N` reserves the whole node until `gpu_comfy_order` names every card, fastest first (or the box
  has one card). New config keys `gpu_comfy_order` and `gpu_host_ram_headroom_gib` (0 = 4 GiB).
- **The allocator** (`internal/gpulease/allocator.go`, a pure function): unclaimed, not quarantined, not the display card
  (unless the operator is away), not under a foreign compute process, VRAM fits, and the host has the declared RAM plus the
  headroom (`gpuprobe.RAMHeadroom`; an unreadable counter refuses only when a need was declared). Order: no resident seat, then
  the cheapest eviction, then the lowest id. `--cards` that cannot be met queues FIFO on a fixed set (the cards free right now,
  topped up from the claimed ones that would fit), or polls until enough cards qualify for a reason a lease does not explain;
  it refuses only at the end of `--wait`. Foreign-busy is Linux-only evidence (WDDM lists no
  per-process rows). Elastic re-picking is the fan-out work.
- **`gpu doctor [--scan dir]... [--write-audit] [--json]`** audits every reader of the lease directory: harness binaries (the
  running one, the install directory's copies and node-swap backups, `PATH`, `--scan` directories, the images of running
  harness processes and of lease holders and waiters) and every Node `gpu-lock.mjs`, by the literal
  `gpu-lease-format-2/per-epoch-fence` (the build version cannot tell: it is the same string inside one release). Nothing found
  is executed; finding nothing, an unreadable root or an unlistable process table is not green; exit status is non-zero when
  not green; `--write-audit` records the verdict as the reader-audit marker, red as well as green, which is what enables the
  writer. `render/gpu-lock.mjs` exports `LEASE_FORMAT_SIGNATURE`; a binary built from this tree is tested to carry the
  signature and a build marker (`TestBuiltBinaryCarriesTheFormatSignatureAndBuildMarker`).
- **Not in this change.** Consumers that take a device set (the text gate, drain and unload scoped to the lease's cards, fleet
  health: P4 to P7), so a card-2 job still fences text work on card 0 until then; `--drain` and `--unload-seat` still act on the
  agent seat as a whole. The acceptance checks on the three-card box (read-only `gpu status` and `gpu doctor` output) have not
  been run from this branch.
- **Review fixes: the audit now reaches every reader it claims to.**
  - **The signature is in every binary that links the lease package.** It was kept only when the audit was reachable, so a
    build of the agent binary (`cmd/local-agent`, which reads the lease through `modelaffinity` and is deployed beside the
    harness) carried none and a pre-format agent and a current one read the same. An init function in `gpulease` pins it;
    `TestEveryBinaryThatLinksTheLeaseReaderCarriesTheSignature` builds every `cmd/` binary that links the package and audits
    it.
  - **Discovery is by content as well as by name.** `local-agent*` joins `local-offload*` and `offload-harness*`, and under a
    scan root any other executable of 1 MiB or more that carries the import path of a package that reads the lease is judged
    by the signature whatever it is called (`TestAuditFindsAHarnessBinaryUnderAnyName`,
    `TestARenamedCopyOfABuiltBinaryIsFoundAndJudged`). A running `local-agent` is audited by image.
  - **Gaps are visible and unreadable entries are findings.** An entry whose stat fails, or an executable that cannot be read
    to tell whether it is a harness binary, is now a reason (it was skipped silently). The report lists what the walk did not
    enter (`not_searched`: past the depth cap, `node_modules`, `.git`), a green with gaps says so, and `gpu doctor --depth N`
    widens the walk. The depth cap now means what it says: 3 levels below a scan root are searched (it was 2).
  - **Documented honestly.** The audit header, `docs/systems/gpu-lease.md` and the operator guide say what is reached and what
    is not, and that plan P6 needs a green that covers `local-agent` and every media repository's own wrapper copy.
  - A fixture name that belonged to a private repository was replaced with a neutral one in the tests.
- **Review fixes: `--cards` allocates and claims in one loop, and queues on the free card too.**
  - **A queued `--cards N` no longer throws away idle cards.** The queue set was built from the claimed cards only, so with some
    cards free and some claimed it queued on claimed cards while a free one sat idle, and with fewer claimed cards than N it
    polled and never registered a place in line (a later `--devices` or whole-node request could jump it). The set is now the
    cards that are free right now first, topped up from the claimed ones that would fit, and it is registered FIFO whenever it
    is at least N large; it polls only for a shortfall a lease does not explain (display, quarantine, foreign process, host
    RAM, VRAM). `TestAllocatorWaitableListsFreeCardsBeforeClaimedOnes`,
    `TestPickAutoCardsQueuesOnTheFreeCardPlusTheFirstClaimedOne`, `TestPickAutoCardsQueuesWhenOneCardIsFreeAndOneIsClaimed`.
  - **Allocation is atomic with the grant.** The allocator reads nvidia-smi, the compute processes, llama-swap and presence over
    seconds and then picked the lowest-id card, so two simultaneous `--cards 1` reserves chose the same card and the loser
    queued, or with `--wait 0` was refused, while another card was free (measured with the real binary: 3 of 3 runs). The claim
    is now a non-blocking acquire inside a loop: a lost claim re-runs the allocator with the winner's claim visible, and the
    request queues only when the allocator itself says no set is free (bounded at 16 re-allocations).
    `TestGPUReserveConcurrentCardRequestsLandOnDistinctCards`, `TestAcquireAutoCardsRetriesWhenACompetitorWinsTheRace`. The
    detached holder takes the request itself (`gpu hold --cards --vram --ram`) and runs the same loop in its own process, so its
    lease is taken by the pid the parent reports.
- **Review fixes: a lease holds cards, so the command is pinned to them.** A `--devices` or `--cards` lease held its cards while the
  command ran wherever CUDA's default put it (on the reference box the display card, the one the allocator refuses to hand out),
  and nothing read `GPU_LEASE_DEVICES`. The wrapper now also gives a command that names no card of its own
  `CUDA_VISIBLE_DEVICES=<driver UUIDs of the held cards>` and `CUDA_DEVICE_ORDER=PCI_BUS_ID`. A command that pins itself keeps its
  pin when it is the program's own (`--cuda-device`, `COMFY_CUDA_DEVICE`) and the wrapper says on stderr when that pin falls
  outside the held cards or cannot be resolved, but a `CUDA_VISIBLE_DEVICES` merely inherited from the shell that reaches outside
  the held cards (or cannot be resolved) is replaced with them; a whole-node lease and a set derived from the command's own pin
  are not pinned. A program that sets its own pin after it starts is still not confined
  (ComfyUI's `--cuda-device` overwrites the variable; one instance per card is plan P13). The `--devices` and `--cards` help and
  `docs/systems/gpu-lease.md` say so. `TestGPUReserveDevicesPinsTheChildToTheHeldCards`, `TestConfineWrappedRules`.
- **Review fixes: the per-card verbs are tested as verbs.** `gpu status` (text and `--json`), `gpu cards` (text and `--json`) and
  the card table printer had only helper-level tests: dropping the per-card merge, the `card_scoped_leases` key, the text table,
  the no-nvidia-smi path or `gpu cards`' failure without a table broke nothing. New verb-level tests (through new read seams
  `statusActivityFn` and `statusForeignFn`, so no nvidia-smi is called) assert the per-card rows, that `queued` keeps its four
  keys and gains `devices` and `scope`, that a held card shows its holder, and that the table is absent with a note when there
  is no card table; each was mutation-checked.

### Added — card-scoped GPU leases: lease record v2, per-epoch fence, conflict-aware queue (register C-86, plan P2)

A lease may now name the cards it holds (by GPU UUID) instead of fencing the whole node, so a job on one card no longer
queues work that could run on another. **Off by default** (`gpu_card_scoped_leases`, per host): with it unset no device
lease is ever written, the whole-node record is unchanged but for the optional `wrapper_version` the wrapper now stamps,
and the existing suite runs as it did. Nothing in this change takes a lease on a device yet; `gpu reserve --devices`, the
allocator and the per-card status are the next change.

- **Record v2.** `lease/e/<epoch>.json` (the record, with `devices`, `group`, `wrapper_version`, `state`) and one
  exclusively created `lease/cards/<id>.claim` per card; `gpu/FORMAT` is `2` once the first one is written. A device lease
  never writes `meta.json`. Two leases conflict when either is whole-node or their card sets intersect.
- **Grant.** One critical section under the epoch lock: sweep debris, refuse on a live whole-node record or an
  intersecting device lease, issue the epoch, write the record `granting`, claim every card exclusively (all or nothing),
  flip to `active`. A whole-node grant verifies under the same lock that no device lease is live and withdraws its claim if
  one is, so the two kinds can never both be granted. It first sweeps device leases the reclaim rule calls reclaimable, so
  a stalled but living holder is fenced when the node is granted over it (its `Check` and `Renew` fail, its record, claims
  and heartbeat are gone); and it verifies whenever `e/` cannot be proven absent, never skipping on a stat error.
- **Per-epoch fence.** A device lease is current while its record is active and each card names its epoch. Applied in
  `Lease.Check/Renew/Release/Restamp`, `ReleaseByEpoch`, `gpulease.EpochIsCurrent` (used by `gpulock` and the pipeline's
  inherited-lease boundary) and `render/gpu-lock.mjs`. A higher-epoch lease is never fenced out by a lower one; releasing
  one lease removes only its own heartbeat, unload marker and claims, and a record or claim that cannot be removed is a
  failed release (reported, and the lease can be released again), as for a whole-node lease.
- **Consumers judge every live lease; the inherited-lease exemption is per lease.** `Info` gains `Leases` and `Each()`;
  the text gate, `Fenced`, `ForeignFence` and `Reserved` ask each live lease, and a process under lease A is exempt from A
  alone (a first draft exempted it from every live lease). Known gap until plan P4: no device-set comparison, so a lease
  on one card still fences a text call on another (over-fencing, the safe direction).
- **Queue.** Waiters carry their device set: FIFO among waiters that conflict, disjoint backfill, a whole-node waiter is a
  barrier. **Debris:** a `granting` record or a claim with no record, older than the 10 s claim grace, is removed by the
  next acquirer; nothing expires a card and nothing kills a process.
- **Readers.** `Inspect`, `InspectDir`, `InspectDirDetail` and `gpulock` report any live lease as held (the conservative
  reading until the consumers take a device set); `Info` gains `Devices`, `Epochs`, `Group` and `WrapperVersion`;
  `InspectLeases` and `Manager.InspectFor` list per lease and per request. `gpu status` shows the cards and the other live
  epochs when there are any.
- **Cross-version hazard, gated by code.** A binary that predates this reads a directory holding only device leases as a
  free card. `gpu_card_scoped_leases` alone no longer enables the writer: it also needs the green reader audit marker
  `gpu/reader-audit.json`, which `gpu doctor` (next change) writes once every binary copy (node-swap backups and running
  images included), the media repository's wrapper copy and every Node reader is per-epoch-fence aware; without it the
  writer stays off and the CLI warns. The pre-change reader is kept as a test fixture
  (`internal/gpulease/oldreader_fixture_test.go`) and the hazard and its gate are pinned by test.
- **Deviation from plan rev 2, pending the plan owner's or operator's acceptance.** The synthetic pid-0 `meta.json`
  umbrella is not written. Its premise holds (verified with the real 0.158.3 and 0.160.0 binaries: a pid-0 record is held
  for its declared window, reclaimed after it), so it was dropped as a second source of truth, not because it failed; a
  pre-v2 writer the audit misses would double-book a switched-on host. P2's acceptance is not met until the deviation is
  accepted; the audit marker is the interim mechanism.
- **Config.** `gpu_card_scoped_leases` (default false) in `config.example.json` and `internal/config`; it takes effect only with the reader audit marker above.

### Added — lease ownership: who asked, whether they are still there, orphaned and stalled leases surfaced (register C-32, C-33, plan P8, ADR 0070)

A lease recorded who HELD it and nothing about who ASKED for it, so a wrapper that was alive and heartbeating read as working
whether the session that launched the job was at the desk or long gone. A long render held a card for about twenty hours behind
a label that read healthy. Everything below changes what is REPORTED: nothing reclaims, releases or kills a lease on it.

- **Record.** `owner` (session, pid + start identity, `remote`, `tracked`), `unattended`, a progress contract (`progress`: file
  and stall window) and `yield_grace_ms` / `on_yield` / `on_yield_dir`, all additive and `omitempty`: a record without an owner is an UNKNOWN
  owner and is never orphaned. The progress file is recorded as an ABSOLUTE path (`gpu reserve` resolves a relative
  `--progress-file` against its own working directory and the library refuses a relative one: every reader looks for the same
  file from its own directory, and a path recorded as typed read `unknown` for the MCP server, the fleet node and another
  session's `gpu status`). `on_yield` is stored verbatim, refused above 4096 bytes and never clipped, with the directory the
  lease was taken in, because the takeover that runs it later does not share that directory. `gpu reserve` takes `--owner-session`, `--owner-pid`, `--owner-start-ms`, `--owner-remote`,
  `--unattended`, `--progress-file`, `--stall`, `--yield-grace`, `--on-yield`; with no flags the owner is the session label the
  ledger already resolves (`LOCAL_OFFLOAD_ORIGIN`, then `CLAUDE_CODE_SESSION_ID`) with no pid, so a short-lived shell is never
  recorded as the owner. `--unattended` requires an explicit `--for`, a progress file and a stall window. `gpu owner-flags`
  prints the flags for the calling tree (for a launcher that detaches before it takes the lease), and the detached `gpu hold`
  child is passed the owner and the contract by its parent.
- **Session registry.** Each MCP server writes `<state root>/owners/<session>.<pid>.json` at start and removes it at exit (no
  timer). A session is alive while any registered process carries its id, by pid and start time, so a resumed or compacted
  session under a new pid does not flap. One file per process rather than per session, so a resume does not have two writers of
  one record. How a Claude Code session id maps to a live process is not verified beyond the environment variable the ledger
  already relies on.
- **Orphan marker.** The first reader to see an owner gone stamps `orphan.<epoch>` under the epoch lock with a second look
  inside it (concurrent readers write one marker and agree on one moment); the owner reappearing clears it; release removes it.
  Only the status surfaces stamp it (`gpu status`, `offload_status`, the `/fleet/health` handler): the inspectors the text gate
  polls every second and the sentence a waiter or a refusal reads (`ErrHeld.Error()`, the gate's `LeaseError`) READ the marker
  and never write it or take the epoch lock, so a lone waiter on an orphan nobody has looked at yet is told what the progress
  file and the window say, and about the owner's absence once a status call has recorded it. Failures are reported, not
  swallowed: a session registry that cannot be read reads the owner `unknown` (never `gone`), a marker that cannot be written
  or cleared says so (`activity.holder.orphan_marker_error`, a `gpu status` warning, the verdict note), and the lease directory
  is probed for writability before the epoch lock is taken (a permission error reads as contention there and cost about two
  seconds per status call). `gpu_orphan_grace_min` (default 15) is installed at config load.
- **Verdicts.** `held-stalled`, `held-orphaned`, `held-overdue` and `tree-orphan` join the vocabulary, in the order
  `stale-holder`, `tree-orphan`, `held-stalled`, `held-orphaned`, `held-overdue`, `held-working`, `held-idle`; an unattended,
  remote or unknown-owner lease is never `held-orphaned`; `working` is still reported first and its note leads with the lease's own
  standing. With only card utilisation as evidence the note says `util only, no progress contract`. A progress file that does
  not exist is `unknown`, never stalled, and the stall window counts from the lease's start at the earliest; the reading says why
  it is unknown (not found, a directory, a permission error) and, once the lease outlives its stall window with the file still
  missing, how long it has been missing, without changing the verdict. `gpu reserve` warns when the file's directory does not exist. `tree-orphan` is in
  the vocabulary and is **not produced by this build**: it needs the wrapper to record its process tree.
- **Legacy leases.** A lease with no owner and no contract shows two read-only facts, the age of the newest ComfyUI output and of
  the ComfyUI log, only when the launch marker can be tied to the lease; they are never a verdict input.
- **Surfaces.** `gpu status` and `--json` (owner, owner state, orphaned-since, progress age and last line, facts);
  `offload_status` brief `gpu_lease_verdict` leads with `ORPHANED`, `OVERDUE` or `STALLED` whatever the verdict word is (a stalled
  lease with work in flight reads `STALLED (working)`) and names the takeover command of the lease the verdict is about, and its
  verdict JSON schema is pinned by a golden; with several live leases the epoch, owner and progress on `gpu status` and the
  offload_status lease view (and the top-level fields of `gpu status --json`) are the most escalated lease's own; `/fleet/health` `lease` gains `orphaned` and `stalled` (absent
  unless true, worst across live leases; `overdue` stays the expiry-based key of the routing change, one source for one wire key,
  so merge that change first); a waiter refused by or queued behind an unhealthy lease (`gpu reserve`, a text admission) is told which
  lease, what is wrong, for how long, what is running and `local-offload gpu takeover --epoch N`. **That command does not exist
  yet** (the explicit takeover is a later change) and the message says so.
- **Docs.** `TestVerdictDocTableComplete` fails a verdict added to `internal/gpuactivity/snapshot.go` without a row in the verdict
  table of `docs/systems/gpu-lease.md`, and a row for a verdict that does not exist. New ADR 0070; ADR 0018's "a leaked lease
  expires" is corrected (a live, heartbeating holder keeps its claim past its window).
### Fixed — a held lease past its declared window no longer reads as free (GPU routing P1)

`leaseHealthOf` clamped the remaining time to zero before comparing it with `fleet_busy_lease_sec`, and zero is below every threshold, so a
lease whose holder was still alive and heartbeating but whose declared window had ended published `busy: false`: the node looked free for
exactly the lease that was demonstrably still using the cards. The rule is now `busy = held AND (remaining > threshold OR overdue)`. The 120 s
short-lease rule is the first clause and is unchanged, and a negative `fleet_busy_lease_sec` still turns the duration rule off. The lease block
gains an additive, omitempty `overdue` flag (an unchanged lease publishes the same bytes). The delegator decodes it as `LeaseOverdue`, not as
`LeaseBusy`, so the node is ranked last (a rung below a genuinely long lease) and never hard-excluded: an abandoned lease cannot make a node
unroutable for longer than before, and the node still queues what it is sent: the overdue busy is not folded into the node's own refusing flag, so
an overdue media-lease node keeps publishing `saturation.high: false` and its idle slot, and the capacity wait still asks it when it is the only remote.
A lease record with no declared end (missing, zero or negative `expires_at_ms`) now reads as an unset end in `gpulease.Info` instead of 1970, so it is
never busy or overdue. `offload_status` shows `gpu_lease_overdue` and reads such a node as `held-idle`. The fleet deploy does
not read this verdict (its wait-idle step takes job counts from health and the lease from the lease directory); `healthwire_compat_test.go` now
pins the raw JSON, the delegator's decode and the deploy's real health decoder against one overdue node.

### Added — placement ranks on free cards, not one utilisation scalar (GPU routing P1)

The delegator dropped a node's `gpu_devices[]` and ranked nodes on the busiest card, so a box with one busy card and two idle ones lost to a box
with three cards at 30 % each. `NodeView` now carries the rows. A card is free when it is not a display card, its utilisation is known and under
15 %, and it has the VRAM (the placement seat's published footprint split across its cards, else a quarter of the card); a layer whose seat spans
N cards needs N free ones, and no seat pin is matched to a card by index. A seat the node says is loaded vouches for the VRAM of the idle cards it
spans, so a warm node does not lose to a cold one for holding its own seat. `betterRemote` gains a per-node tier after the ETA key (free card, then a
node that published no per-card truth, then none), `route=auto/remote` spends a node's free cards as it deals, and `route=spread` uses the tier as the first
key inside a deal cycle without breaking the one-subtask-per-seat-per-cycle invariant. It ranks and never gates: `max_concurrent_jobs` stays
the hard ceiling and a node with no free card still takes work. `offload_status` shows `free_cards` and `cards_total` per node. ADR 0057 amended.
Per-card leases, which let the local box do the same, are later phases of the plan.

## [0.160.0] - 2026-10-02 - The public tree carries roles, not machine names, behind a keyed leak gate; blackwell-8 seeds its vision extras and Z-Image Turbo

### Added — the leak gate: real machine, person and brand names stay out of the tracked tree (register H-55)

The tree named the operator's machines, people and brands in about 1,500 places across 313 files. Every one is now a role
(`<node-a>` ... `<node-f>`, "the workstation", "the editing rig") or a tier id of the era a dated sentence describes, and a
gate keeps them out.

- **The gate.** `internal/leakgate` and `cmd/leakdigest` hold the matcher (word, exact, sub, case-sensitive and phrase
  entries; camel, digit, underscore, hyphen, escape and percent glue; UTF-16, PNG text chunks, file names), the keyless shape
  rules (real-shaped GPU ids, WSL paths naming a real distro) and the fail-closed scan set (an unknown binary, a long run, an
  oversize file, a symlink or a path that cannot be read is a FATAL unless the digest file exempts it by blob). The list
  itself is never in the tree: `testdata/leak-gate-digests.json` holds keyed digests of it, a MAC over the whole file and
  three exempt rows (the Inter fonts). A canary entry is always appended and every scan must flag it.
- **Tests.** `TestTrackedTreeCarriesNoDeniedNames` runs the keyed scan when a key is present (and SKIPs with its reason when
  none is and none is required); `TestTrackedTreeCarriesNoShapedIdentifiers` needs no key, so forks are covered too;
  `TestLeakGateKeyResolution` and `TestLeakGateScannerBehaviour` pin the key rules (missing, empty, malformed, wrong,
  rotation through a previous key) with synthetic keys and names.
- **CI.** The existing workflow's `Test` step reads the key from a repository secret and requires it on pushes and
  same-repository pull requests; a fork's pull request runs keyless and its summary says `skipped`. The run summary carries
  `leak gate: enforced (N files, M entries)`. The workflow gains `permissions: contents: read`; no trigger, job or workflow
  was added.
- **A byte-order mark is never a hiding place.** A marked file is scanned as decoded text, as raw bytes and in the other byte
  order, and a mark whose payload is not UTF-16 is a FATAL.
- **Proof.** On the pre-sweep base the keyed test fails with 1,494 findings in 309 files and the shape test with 39 in 9
  (1,533 in total); the final tree passes both with the real key. Real names planted in a scratch copy (bare, glued, escaped,
  upper case, a file name, UTF-16, a PNG text chunk, the canary, a GPU id) each fail at their own path, and the negatives
  (`37060`, `15px`, a longer word that only contains a listed one, the product spelling of a listed distro) pass.
- **Docs.** `docs/systems/leak-gate.md` (new), `docs/STYLE.md` (the naming rules), `docs/glossary.md` ("reference box" per
  tier, "leak gate"), `CONTRIBUTING.md` (forks skip the gate; a maintainer runs it with `-count=1` and the key file),
  `AGENTS.md`, `docs/README.md`.

### Changed — the sweep behind the gate

- Docs, ADRs, the CHANGELOG, tier pages, templates, setup strings, test fixtures and comments use the roles above. Machine
  vendor and model names are gone; GPU, RAM and OS facts stay. Test fixture node ids are per-test labels chosen so sorted
  output keeps its order; no assertion was edited.
- Two ADR file names and one spec file name carried a machine name and were renamed; every link was updated.
- The `kv_cache_server` `key_prefix` values lose their machine prefix (`seat-tp2-fp8`, `seat-3card-fp8`, ...).
  `audit-config` now treats a `key_prefix` difference between two lists of bindings as node-owned, not drift (a seeded node
  keeps the prefix it was seeded with).
- The WSL seat scripts derive their directory from their own location, and the Go default seat directory is `/opt/seat`
  (no production config sets the field, so served behaviour does not change).
- The three skill libraries were regenerated at the source: the private scrub script's map was extended and re-run over a
  pinned copy of the libraries as they stood in the tree, so no newer private measurement was imported.
- The contract fixtures were rebuilt from the swept ADR text: every acceptance array is byte-identical and the intake lint
  says the same as before. Context entries that were already a stale snapshot keep their old tail after the swept head.
  sha256 of the committed blobs, old -> new: `digest-8.json` `5f90291d0e09b9e1b053d60095e360cbf21651d457df79af703baa785544383a`
  -> `3aa399254e94f7c1a0e8d03a44ebe8cde3d6dcb5532d2a04db4fe2cead569a12`; `digest-8-grounded.json`
  `86cea33822535e354d3564ad08ef6094fcd6e9ab61d1f972618ca3ce8b4ce199` ->
  `f537df93ca571f269be4daf34761761559d96a18727a97ab344b63cf449fbf8a`; `digest-adr-hard-8.json`
  `982cff36fb4dd2b8b462691bc56294dc09fa971bd6ee8e1b6969f85f81e07bc4` ->
  `d84741352f0a60a82812e4def39f00883dd6a4e982bf5ec627a6693eeab7eb22`. `TestContractFixturesAreWellFormed` guards them.
- Profile notes cite operator records by description rather than by file names that no longer exist.

### Fixed — the GPU env injection keeps a key a model block already sets

`setup/tests/install-cuda-build.test.ps1` still called a PowerShell helper that was deleted when the render moved to Go, so the
suite failed and nothing tested the injection. Its assertions now live in Go (`inject_gpu_env_test.go`), and porting them
showed the Go injection was not idempotent: a block that already set a key gained it again, so a seat's own device pin would be
followed by the tier-wide value. An existing key now keeps its value and a second pass is a no-op. No shipped tier combines the
two today, so rendered output does not change.

### Fixed — fleet reclaim never reclaims a memory-stack member, whatever its ttl (register C-94)

The reclaimable-VRAM advertisement (`fleet_reclaim.go`) protected llama-swap's ttl -1/0 seats and the configured keep-set, but
never `memory_stack`. The house rule gives every model a 300 s idle ttl, so a mem0 stack member (including the memory
authority's `embeddinggemma-ams`, added to the default stack in A-122b) was neither a ttl -1/0 seat nor in the keep-set, and a
loaded one counted as reclaimable capacity: a job placed on that figure could take the embedder off the card. `gpu reserve
--unload-seat` and `render/gpu-lock.mjs` already kept the stack; reclaim now keeps the same set.

- **`anyReclaimable` checks the memory stack first, in both keep-set modes.** A loaded member is baseline by name (canonical id,
  case and padding ignored, as `otherResidentModels` matches), whatever its ttl, and also on a box where no keep-set could be
  read and the classifier falls back to the ttl field. A non-member at ttl 300 is still reclaimable.
- **One reading of the set.** `effectiveMemoryStack(cfg)` (the configured `memory_stack`, or the default when empty; a configured
  list replaces the default) now serves both `gpu reserve --unload-seat` and fleet reclaim, so the two cannot drift.
- **Docs.** ADR 0049 Amendment 5 and `docs/systems/gpu-lease.md` no longer describe this as a known gap; `docs/systems/fleet-node.md`
  documents the rule.
- **Tests.** `TestFleetReclaimNeverReclaimsAMemoryStackMember` (every default member at ttl 300, keep-set known and unknown, a
  non-member still reclaimable) and `TestFleetReclaimHonoursAConfiguredMemoryStack`, driven through `oursLoaded` against a fake
  `/running` and a hermetic llama-swap YAML.

### Fixed — the agent seat's source and the compared RAM overlay are named (C-95, I-40)

- **Every config-loading verb says which source supplied the agent seat (register C-95).** A
  config copy run with `LOCAL_OFFLOAD_CONFIG` honoured its `endpoint` but sent every subtask to
  the seat of the node's own layers and deferred `agent seat "..." is not in the endpoint's
  served roster`. Confirmed cause: a copied config that kept the node's `layers`. The explicit
  file was in fact the only one read; on a composite box the placement table picks the seat from
  the layers and `agent_model` applies only when a decision names none (ADR 0052, unchanged), so
  the copy's `agent_model` was shadowed by a layer's agent seat, which the repointed
  endpoint did not serve. The harness now prints a stderr note naming the config file when its
  `agent_model` is not a seat of any of its layers, and a roster-miss defer on a placed (or
  delegator-set) seat ends with its source (layer and role, versus this config's `agent_model`).
  The note is emitted from the shared config-loading path, once per process and on stderr only
  (the MCP stdout stays JSON-RPC), so `agent_delegate` and `agent_run` through `local-offload
  mcp`, `delegate`, `acceptance` and `report` all say it, and `local-agent` calls the same
  helper. New: `Config.AgentModelShadowNote`, `Config.LayerSeatModels`,
  `config.WarnOnShadowedAgentModel`, `config.WarnOnShadowedAgentModelOnce`. README and
  `docs/systems/fleet-node.md` updated.
- `audit-config` defaults `--ram-tier` to the RAM tier the machine detects (`auto`) instead of comparing the base seed alone, so the keys a RAM overlay carries on a mid or high RAM box no longer read as drift (23 of 38 rows on a blackwell-8 box with 64 GB were false positives). `--ram-tier none` keeps the base-seed comparison, `min`/`low`/`mid`/`high` name a tier explicitly, and an unknown value is refused. The text header and `--json` (`ram_tier`, `ram_tier_source`, `ram_overlay`) now say which RAM overlay was compared. (I-40) A node's config read from another machine (`--config`, `--home` or a foreign `--goos`) with no named `--ram-tier` prints a stderr warning that the overlay compared is this machine's RAM tier, and the header names the RAM the probe read in GB. A RAM probe that reads 0 GB is refused with a pointer to `--ram-tier`, where it used to pass for tier `min` and pick the base seed silently.

### Added — non-binding extra media seats; blackwell-8 seeds its two served vision extras (A-131)

- **Non-binding media seats (`extra`).** A `media_seats` entry may set `"extra": true` (vision, ocr or stt seats). It renders into llama-swap with its aliases and matrix var like any seat, but `Seat.BindingKey()` answers empty: no config key is written and the one-writer check does not count it, so a tier can serve a second vision-class model beside the bound one. `setup/install.ps1` `Get-MediaSeatBindings` mirrors the skip (the Linux installer goes through `install seed`, the Go path). An extra is validated like any seat; `extra` on an `rkllm` seat and vision `tasks` on an extra are refused. Tier docs mark extras in the seat table and count them apart in the index. (register A-131)
- **blackwell-8 seeds its two served vision extras**: `lfm2.5-vl` (aliases `lfm-vl`, `screen-vl`) and `gemma4-e4b-vision` (alias `gemma-vision`), ctx 8192, swappable, ttl 300. Caveat recorded in each seat's `measured` note: the rendered form adds `--reasoning off`, `-ngl 99` and the tier's q8_0 KV, which the hand-wired entries do not run, so it is not yet re-measured; an EN/ES OCR and spatial VQA pass comes before anyone swaps it in for a hand-wired entry.

### Fixed — the statements the extra seats falsified (A-131)

- **Two statements the extra seats falsified are reworded.** ADR 0020 said a tier may declare at most one seat per kind, and the `seatVarID` doc comment said the kind makes the matrix var id unique. Neither holds once a tier can declare an extra or a text-only `rkllm` seat beside the bound one: the kind picks the base id (`vis`, `stt`, `ocr`, `rkllm`) and a second or later seat of the same kind takes the numbered ids (`vis2`, `vis3`, ...) in declaration order. ADR 0020 carries a dated amendment rather than a rewrite; no behaviour changed. (register A-131)

### Added — seed placeholders expand inside objects, and blackwell-8 seeds Z-Image Turbo as a named sdcpp image family (register A-132)

`tierseed.expand` and the installer's `Expand-SeedValue` substituted `__OFFLOAD_HOME__` and `__EXE__` in strings and string
arrays only, never inside an object. A named image family is an object, and it must carry its own model paths (a named
family clears the flat `sdcpp_*` model keys and the runner passes `sdcpp_model` to `sd-cli` verbatim), so a family whose
paths lived under the install home could not be seeded: the token shipped literally, a config that loads and fails at render.

- Both expanders now recurse into objects (any depth, arrays and scalars inside included) and into every array element. The Go
  side builds a new object and never rewrites the profile's own; the PowerShell side mirrors it. The installer's array
  branch had expanded string elements only, so an object or an array inside an array (`[{"p": "<token>/x"}]`, `[["<token>/x"]]`)
  kept its token even though Go expanded it; it now calls `Expand-SeedValue` on each element and keeps the `,([object[]]...)`
  wrapper that stops PowerShell flattening a one-element or nested array. Both suites load one shared fixture
  (`internal/tierseed/testdata/nested-expand-parity.json`, with array-of-object, array-of-array, one-element and mixed
  arrays), so the two implementations cannot drift apart unnoticed.
  Recursing exposed one producer: `Get-CompositeSeed` wrapped `tiers` and `layers` in a second array with a stray leading comma
  (a hashtable value is never unrolled), which the old string-only branch had flattened by accident; the comma is gone, and
  `setup/render.tests.ps1` (composite-tier parity) holds it.
- The overlay gate (`TestEveryShippedOverlayLoadsAndValidates`) already failed on a surviving token inside a family; the
  installer suite now holds the same line (no tier layer may leave a placeholder in the config an install writes).
- `blackwell-8` seeds `z-image-turbo` in `imagegen_families` (Q8_0 diffusion GGUF, `zimage_ae` VAE, Qwen3-4B text encoder,
  the measured 6.5 GB graph-cut arm, 8 steps, cfg 1, Apache-2.0), so a fresh install keeps the per-request
  `family:z-image-turbo`. The default image binding (`hidream-o1-dev`) is unchanged; weights stay out-of-band like every seed.
- The accelerator device tokens (`__HAILO_HOME__` and its siblings) are still substituted in top-level strings only; no
  accelerator seed carries a family object.

### Added — video families carry the licence pair (CT-47)

- `videogen_families[name]` takes `license` (string) and `commercial_use` (bool), the same pair the image and edit
  families declare. Both or neither: the config load refuses a lone one, naming the family. Nothing requires the
  pair and nothing gates on it; video family selection stays open through `model`. Licence fields only, no warning
  text (operator order 2026-10-01).
- The pair is read from the family's own entry whichever way its weights resolve, so this box's default family is
  tagged too (its weights still come from the flat `videogen_*` keys). A family with no entry carries none.
- Surfaces, following the image families: a `generate_video` result carries `license` and `commercial_use` when
  the rendering family declares them (an override request gets the overridden family's pair, not the seat's), the
  ledger row carries `license`, `offload_status` `media.video_family_bindings` rows carry `license` and
  `commercial_use` (null when undeclared, which a reader treats as UNKNOWN), and `local-offload doctor` prints a
  `license:` line under a family that declares one. A family that declares none changes nothing in a result or in
  the doctor output; the status rows gain the two null keys.
- No value ships as a default. The repo's only sourced statements of the HunyuanVideo 1.5 and MiniMax-H3 terms are
  the names and conditions in `render/templates-license-map.json`, read from hosting-card tags and digests and not
  from the licence texts in full, and its `conditional` class is not a boolean. Operators declare the pair
  themselves once the texts are read.
- Tests: config (both-or-neither, default-family tagging), pipeline (result, ledger meta, undeclared adds no keys),
  mediacap and mcpserver status rows, doctor; each mutation-checked. The status golden gains the two null keys.
- The `offload_generate_video` tool description, its README row and the `TaskGenerateVideo` comment now say the result
  carries `license`/`commercial_use` when the family declares them, as the image and edit descriptions do.
- Docs: `docs/systems/media-generation.md`, `docs/systems/mcp-server.md`, ADR 0058 amendment 2026-10-02.
- Files: `internal/config/families.go`, `internal/pipeline/pipeline.go`, `internal/mediacap/routeneeds.go`, `main.go`.

## [0.159.0] - 2026-10-02 - The ampere-16 vLLM seats move to vLLM 0.30.0 with a pinned KV pool; the compose lane pins HyperFrames 0.8.108

### Fixed — the ampere-16 lane seat starts with a fixed KV pool, so a cold compile cache or a memory-stack model loading mid-start can no longer fail its start; both seats move to vLLM 0.30.0 (registers A-119, A-129)

The lane seat's start failed in production three times (`To serve at least one request...`). Its KV pool was sized by
utilization (util 0.84), and vLLM measures such a pool during startup profiling, device-wide, so anything that moves the card's
memory in that window changes it: a cold compile cache profiles a larger activation (0.77 GiB against 0.41 GiB warm), and a
memory-stack model loading beside the engine shrinks the pool likewise. Either took it below one 32,768-token request. The
cause was reproduced on an empty card by firing the embedder about 50 s into the start. The seat is now started with the pool
its warm start sized, and a Linux seat can declare that pool at all (the field existed but only the windows-wsl launch rendered
it). ADR 0049 Amendment 6 records the evidence.

- **The linux-systemd run script renders `kv_cache_memory_bytes`.** `vllm-seat-run.sh` gains a `__KV_POOL__` token on the
  launch line: `--kv-cache-memory-bytes N` when the seat declares a pool, empty otherwise, so a seat that declares none renders
  exactly the flags it did (plus one space). `Validate` used to refuse `kv_cache_memory_bytes` on the linux-systemd launch
  along with a pipeline; it now refuses only a pipeline there, which that script still renders no flags for. vLLM ignores
  `gpu_memory_utilization` while a pool is pinned, so the field's comment, the script's header and the linux-systemd README now
  say so and give the A-129 reason beside the display-card one.
- **`ampere-16` seeds the pin.** The lane seat declares `kv_cache_memory_bytes` 1,524,713,390 (1.42 GiB, 45,472 tokens as vLLM
  0.29.0 sized it warm) and `engine_min_version` 0.30.0. Its util 0.84, 4 sequences and 2,048-token batch are unchanged and
  stay declared as the point the engine's workspace and the support models were measured at. Measured on vLLM 0.30.0 with the
  pin, four concurrent ~20k-token requests beside the embedder and the reranker: card peak 13,951 of 15,356 MiB (1,405 MiB
  free), digest-8 8/8 at the 900 s wall, and greedy output identical to 0.29.0 on 20 prompts.
- **The 35B fast seat declares util 0.855** (was 0.85). vLLM 0.30.0 carries about 0.07 GiB more non-torch memory than 0.29.0
  and util 0.85 lost 3 KV blocks; 0.855 gives a 96,416-token pool with 1,027-1,037 MiB free under load and digest-8 8/8. It
  stays sized by utilization: no pin has been measured for it. Its unit is the operator's, so
  [composite-tier.md](docs/systems/composite-tier.md) now says what the hand copy of the lane seat's run script must change:
  drop `--kv-cache-memory-bytes` (the pool is the lane seat's) and set `--gpu-memory-utilization 0.855`.
- **The tier pages print what the table declares.** The seat tables formatted utilization with `%.2f`, which turns 0.855 into
  0.85; a value that needs a third digit now prints it, and every page that printed two digits is byte-identical. The pool row
  says the utilization beside it is ignored. `docs/tiers/ampere-16.md` and `docs/tiers/blackwell-3x16.md` are regenerated.
- **Guards.** `TestAmpere16VLLMSeatDeclaresItsMeasuredBoundLane` pins the pool; a new
  `TestAmpere16LaneSeatDeclaresTheEngineItWasMeasuredOn` pins the engine version; `ampere16CoResidency` carries an exact pool per
  seat (0 for a seat measured util-sized), because a smaller or absent pool is the failure and a pool nobody measured beside the
  support models is the other direction; `TestAmpere16FastSeatDeclaresItsVLLM030Utilization` pins 0.855 exactly (the
  co-residency ceiling alone would accept a seed left at 0.85); `TestAmpere16LaneSeatRendersItsPinnedPoolIntoTheLaunchLine`
  renders the committed lane seat through the real template and requires the flag on the `exec vllm serve` command, not in a
  comment. `vllmseat` tests cover both directions (a pinned seat renders the flag once, an unpinned seat renders none, a pool
  validates on linux-systemd, a pipeline is still refused there, a negative pool is refused by name) and `tierdocs` tests cover
  the exact utilization and the pool row on both tables. Each new or tightened test was mutation-checked: the mutant applied,
  the test failed, and the file was restored byte for byte.
- **What this does and does not reach.** A fresh install renders the pin. An existing box keeps its unit until
  `local-offload install vllm-seat` is re-run and the two root steps it prints are taken. The 0.30.0 figures above come from the
  acceptance arms run on the reference box; this change re-ran none of them, and its gates prove that the table declares them
  and the template renders them.
- **Thermal caveat (register A-130).** Sustained decode on the A2 throttles the card, so a tok/s declared for either seat
  (`agent_seat_tok_s` 7.17, the 35B's 40.8 single-stream and 170 eight-stream) is a cool-card figure. No sustained rate was
  measured here, and no declared rate changes.

### Changed — the compose lane's pinned HyperFrames moves from 0.8.61 to 0.8.108, with the runner unchanged and one stricter gate

`setup/hyperframes/package.json`, its lockfile and `PINNED_VERSION` in `render/compose-hyperframes.mjs` now name
HyperFrames 0.8.108 (npm `latest` when measured, published 2026-10-02; 0.8.109 followed 1h40 later) instead of 0.8.61 (2026-09-22). The 47 releases in between were
read (the GitHub release notes, then the two packages themselves) for anything the runner, its tests or the seven
vetted templates depend on, and nothing in them needed a runner change. The one behaviour that moves is in the CLI's own
gate, below. A node keeps working on 0.8.61 only until it runs the new runner: `assertPinnedInstall` refuses any install
that is not the pin (`CLI_MISSING`, and the `hyperframes (compose runner)` row of `local-offload acceptance`), so each
node needs the installer's hyperframes step (`npm ci --ignore-scripts` from the new lock, in place; the browser and the
home directory are kept) before it takes the new runner. The fleet deploy does not run that step: it is done per
node alongside the deploy that ships this runner (the release's deploy record lists each node's re-install).

- **The pin and its supply chain.** The lock was regenerated from the committed one with `npm install --package-lock-only
  --ignore-scripts`, so exactly the `hyperframes` entry changed (4 lines): every other entry keeps its version and
  integrity. The new integrity (`sha512-RvHMA6…80xw==`) equals `npm view hyperframes@0.8.108 dist.integrity`.
  `npm ci --ignore-scripts` from the committed files installs 72 packages, `npm audit signatures` verified all 72
  registry signatures and 21 attestations (hyperframes' own SLSA provenance among them) and exits 0, and `npm audit`
  finds 0 vulnerabilities. `allowScripts` stays `esbuild@0.25.12` alone: it is still the only package in the tree with an
  install script, and hyperframes itself declares none. The dependency ranges of the two versions are identical. An
  in-place upgrade of a 0.8.61 install (copy of a scratch install, new `package.json` and lock, `npm ci`, `npm audit
  signatures`, `npm rebuild esbuild`, `browser`) ended at 0.8.108 with `matches_pin: true` and the existing Chrome kept.
- **No breaking change reaches the runner.** `lint`, `check`, `render`, `snapshot` and `browser --help` are identical
  between the versions apart from one sentence of `--page-side-compositing`. The JSON documents of `lint`, `check` and
  `render --batch` have the same key paths and types on a clean and on a flawed composition (lint findings gain `line` and
  `column`, which the runner does not read), `snapshot` writes the same file names (`frame-00-at-1s.png`, …) and
  `browser path` / `--version` print the same shapes. All ten environment variables the runner sets are still read, the
  update and skills block still sits behind `!hasJsonFlag` (the runner puts `--json` on every call), and
  chrome-headless-shell is still 152.0.7977.30, so no node downloads a new browser. New upstream commands (`history`,
  `clean`, `models`) are not on the allowlist and stay refused before a spawn.
- **Still offline, still no phone-home.** The set of hosts named in the package differs by one placeholder
  (`document.invalid`). A logging preload on the CLI's Node process (name lookups, `net` connects, `fetch`,
  `http(s).request`, under the runner's own scrubbed environment) saw only loopback traffic for `lint`, `check`, `render`
  and `snapshot` of all seven templates and for `--version` and `browser path`, on both versions. The same preload, set to
  refuse instead of log, caught the `fonts.googleapis.com` request an undeclared family makes (a `title-card` with
  `Roboto`), so the silence is not a blind spot on that side. Chrome's own page requests are outside the preload's reach;
  that side rests on the templates' contract (no URL in the page, and a scan of each materialized project found none). The Inter faces the shared font kit vendors are still byte-identical to the producer's
  embedded ones (all three hashes match; the data moved from `dist/cli.js` to a chunk file).
- **Determinism holds inside the new version.** The 150-frame `title-card` (1920×1080, 30 fps, `mp4`, software GL) rendered
  three times per version (`auto`, `auto`, 1 worker): 150 of 150 frames and the whole file identical across the three on
  0.8.61, and the same on 0.8.108. `captions-bar` (`webm`, 240 frames) at 1, 2, 4, 6, `auto` and `auto` gave 15 of 15
  identical pairs of decoded frames. Across versions: the three alpha overlays (`callout-label`, `captions-bar`,
  `lower-third`) decode to identical frames (0 of 180, 240 and 180 differ; the files differ by 2 bytes, because the
  `hyperframes_version` tag now holds the real version, where 0.8.61 wrote `0.0.0-dev`), and the four opaque `mp4`
  templates do not (every frame differs: on the `title-card`, PSNR 53.0 to 57.0 dB, mean 54.0, at most 7 levels of a channel
  on frame 0 and 41 on a later one at the glyph edges, and the radial glow's banding rings land on other pixels). Nothing
  visible changes; a golden that pins the bytes of an opaque render has to be regenerated.
- **Render time is flat.** Summed over nine renders (all seven templates, plus the `title-card` three times), the
  renderer's own time is 212.5 s on 0.8.61 and 221.2 s on 0.8.108 (+4.1 %; per pair of renders from −4.8 % to +16.4 %, the
  largest being one `title-card` run whose repeat was +0.7 %), and the whole runner call is 330.3 s against 329.7 s. The `title-card` takes 15.4 s
  (mean of three) against 16.5 s, and 27.6 s against 28.1 s through the whole gated call.
- **One gate is stricter: `check` fails text that spills out of its box.** `layout/text_box_overflow` is an error on 0.8.108
  where 0.8.61 reported `canvas_overflow` as a warning and passed (`check` exit 0), so a composition whose text leaves its
  box now defers `CHECK_FAILED: check exit 1: layout/text_box_overflow: Text extends outside its nearest visual/container
  box.` where it used to render. It only matters for `html` and `project_dir` (the local doors) and for a template
  pushed to its limits: the seven shipped templates pass strict `lint` (0 errors, 0 warnings) and `check` on both versions,
  at their defaults and at every string variable's `maxLength` filled with ordinary prose (the same verdicts on both
  versions, `lower-third` already fails the check at its limits on both). Only the `title-card` at 80 and 140 characters of
  `WWWWWWWW` moves (0.8.61 rendered it, and read back the video shows the headline cut at the top and the subtitle at the
  bottom of the frame; 0.8.108 refuses it). Pass `strict: false` to render such a page anyway.
- **`captions-bar` was measured again, as the bump step requires.** Its README's Measured section now reads 0.8.108: 0
  lint findings, `check` passed, VP9 `yuva420p`, 240 frames, 24.0 s at one worker and 27.2 s and 26.7 s at `auto`, 15
  of 15 identical frame pairs across 1/2/4/6/`auto`/`auto` and identical to the 0.8.61 render, the frame at 1.5 s reads
  "Captions follow the words", the alpha plane is 0 at 0.3 s, 2.8 s and 7.9 s and tops out at 255 inside each group, and the
  bar, text and accent line are intact over flat grey and flat white. `hf-seek`, the undocumented event the template reads, is
  still dispatched.
- **Tests and docs.** The runner's test fixtures follow `PINNED_VERSION` instead of a literal, the lock-integrity assertion
  carries the new value, and the pin, the SETUP-AGENT excerpt, ADR 0059 (the gates it depends on were checked again on the new
  package), the shared font README and the `captions-bar` README name 0.8.108.

Gates: `node --test render/*.test.mjs` 817 tests, 816 pass, 1 skipped (a ComfyUI-checkout test, as before), 0 fail (the 61
compose tests all pass); `go build ./...` clean; `go test -count=1 ./internal/mediacap/... ./internal/pipeline/...
./internal/fleetnode/... ./internal/config/...` and the root package pass; `pwsh -File setup/render.tests.ps1` with a freshly
built binary: 182 PASS, 0 FAIL.

Not in this change: updating the installs already on the nodes (the installer's hyperframes step does it, and the runner
refuses a node until it has run), a pixel-golden test for the templates, and the Docker render mode.

## [0.158.3] - 2026-10-02 - The blackwell-8 media seed carries the roster the box runs: fp8 Wan pair on the native loader, fp8mixed edit unet, Qwen-Image-2.1 as an edit family

### Fixed — a fresh `blackwell-8` install seeds the media roster its reference box runs, not the 2026-08-23 first cut (register A-120)

The `blackwell-8` RAM layer (`config_seed_ram_mid_high`) still seeded the first cut of the 8 GB media roster: the Q8_0
Wan 2.2 pair through DisTorch2, the Q5_1 edit unet, and no edit family. The reference box had moved on (operator
decision 2026-09-24) to the fp8_scaled pair on the native loader and the fp8mixed edit unet, and served Qwen-Image-2.1
as an edit family from hand-written config. A fresh install of the tier therefore got a roster the box no longer runs,
and the live `audit-config --ram-tier mid` of 2026-10-01 read three of those keys as DIFFERENT. The layer now seeds what ran, and the
tier notes state the evidence level of each value. Existing configs are untouched, as ever: seeds apply only to a
fresh `config.json`.

- **Video: `videogen_unet_high`, `videogen_unet_low` and the new `videogen_wan_loader`.** The seed binds
  `wan2.2_i2v_{high,low}_noise_14B_fp8_scaled.safetensors` with `videogen_wan_loader: "native"`. In the 2026-09-24 fp8
  study, same still, prompt and seed 42 at 832x480x81 on the 20-step recipe, the fp8_scaled pair on the native loader
  took 2,029 s at a 6,675 MiB VRAM peak (82% of the card) and about 24.5 GiB of host RAM; the Q8_0 pair through
  DisTorch2 (virtual VRAM 12) took 2,939.4 s at 7,509 MiB (92%). That is 31% less wall time and more VRAM headroom,
  with a PSNR of 27.7 dB and an SSIM of 0.926 between the two outputs and 7 frames inspected clean. The production CLI
  measured 2,024.7 s after the config switch the same day. The loader moves with the pair: `native` refuses a `.gguf`
  expert by name at render time, so going back to the Q8_0 pair is three keys (both experts, and the loader back to
  empty or `gguf-distorch`). `videogen_wan_virtual_vram_gb` 12 is inert under `native` and stays for the GGUF arm;
  `videogen_timeout_sec` stays 4500 (2.2 times the fp8 wall). The lightx2v 4-step `fast` opt-in is not measured on the
  fp8 pair (520 s when baked 2026-08-23, 1,193.1 s when the Q8_0 pair was re-measured 2026-09-23).
- **Edit: `gen_edit_unet`.** The seed binds `qwen_image_edit_2511_fp8mixed.safetensors`, preset `lightning8`
  unchanged. The evidence level, plainly: on the reference box the fp8mixed file passed the sign-text instruction with
  the text exact (2026-09-24) but was not timed there, and every timed edit figure from that box is the Q5_1 GGUF
  (245 s at 17.9 s per step and 7,193 MiB on 2026-09-22; 268.1 s with `lightning8` and 113.8 s with `lightning4` on
  2026-09-23). The roughly 2x (56.5 s against 107.0 s, PSNR 39.4 dB, SSIM 0.989, text exact on both) is a measurement
  on a 16 GB-class card. The operator approved the switch on 2026-09-24, and the 8 GB timing is owed at the next
  acceptance render. Going back is one key (`gen_edit_unet` = `qwen-image-edit-2511-Q5_1.gguf`; the builder picks its
  loader by file extension).
- **`gen_edit_families`.** `qwen-image-2.1` is seeded as a named opt-in edit family beside the default edit binding,
  mirroring the image family the tier already seeds: unet `qwen_image_2.1_bf16.safetensors`, clip
  `qwen3vl_8b_bf16.safetensors`, vae `qwen_image_2.1_vae_bf16.safetensors`, 40 steps, cfg 1, timeout 2,400 s, dynamic
  VRAM on (a two-reference edit through it measured 375.0 s on 2026-09-23). Its licence is recorded as fields only
  (`license`, `commercial_use: false`), with no warning text (operator order 2026-10-01); a live config that still
  carries the older `(non-commercial)` wording reads DIFFERENT in `audit-config` until it is realigned. `audit-config`
  did not see this key or `videogen_wan_loader` before (their suffixes are not binding suffixes); both are seed-owned
  now, and audited when the RAM overlay is selected (`--ram-tier mid|high`): the default audit still compares the base
  seed only (register I-40).
- **New gates.** `internal/tierseed/overlay_load_test.go` exists because nothing validated the CONTENT of an overlay:
  `TestEveryShippedSeedIsValid` stops at "every key is a `Config` field", and the closure gates resolve the base seed
  only. `TestEveryShippedOverlayLoadsAndValidates` resolves every tier on windows and linux at ram tiers mid and high,
  fails on a placeholder that survives expansion (expansion reaches strings and string arrays, never the inside of an
  object; superseded by A-132), runs `config.Load` on what an install writes and fails on any load warning but the installer-owned
  `compose_script` one, and resolves every image and edit family with its licence pair.
  `TestSeededWanLoaderMatchesTheExperts` fails when a seed layer, alone or merged with the RAM overlay, pairs `native`
  with a `.gguf` expert. The config door cannot see that (it depends on which unet applies), so the first render would
  have been the first to say so. `setup/tests/install-config-seed.test.ps1` pins the new values and checks that the
  nested family survives `Merge-ConfigSeed` as an object with its types intact. It pins them with `-is`, because
  PowerShell's `-eq` coerces its right operand (`"False" -eq $false` and `"40" -eq 40` are both true). The seed suite
  grows from 286 to 292 asserts and passes on pwsh 7 and Windows PowerShell 5.1; `setup/render.tests.ps1` passes on both.
- **Mutants.** Fourteen mutants of the seed and of the installer, each turning the intended test red and each restored
  byte for byte (sha256 checked): a `.gguf` expert beside `native` (the table lint and the PowerShell suite); `native`
  replaced by `auto` (PowerShell only, and the Go lint correctly stays green, because `auto` with safetensors experts is
  coherent); `native` only in the base layer with `.gguf` experts in the overlay (caught only by the merged-layer view);
  the edit family with its licence removed, with `commercial_use` as the string `"false"`, with `gen_edit_steps` as the
  string `"40"`, with `gen_edit_cfg` removed (a half-bound recipe, which is a load warning), with a `__OFFLOAD_HOME__`
  token inside the family object, with the warning text back in the licence, and with 20 steps; the whole family
  removed; the edit unet back to Q5_1 (PowerShell red, and `TestTierDocsAreCurrent` red until the page is regenerated);
  and two installer mutants (`Expand-SeedValue` flattening a nested object, `Merge-ConfigSeed` serializing at depth 1).
  The two string-typed mutants pass a PowerShell assert that compares with `-eq` alone, in both editions.
- **Docs.** `docs/tiers/blackwell-8.md` is regenerated (the only generated page that changes; `config.example.json`
  is unchanged), and its notes gain the D10 paragraph with the weight pins. `setup/SETUP-AGENT.md` tells a provisioner
  to stage the fp8 pair, the fp8mixed unet and the two Wan builder defaults (neither pair is installer-pinned, so a box
  that staged only the Q8_0 pair and the Q5_1 file reports `BOUND-BUT-MISSING` until they are there);
  `docs/systems/media-generation.md` states the blackwell-8 exception to the Q8_0 and Q5_1 rows and what the loader key
  does. The `hailo-8l` accelerator note says `HAILO_HOME` defaults to `<OFFLOAD_HOME>/hailo`, so an install whose
  sidecar checkout lives elsewhere sets it before the installer runs.

Not seeded, and why:
- **Z-Image Turbo as a named image family.** A named family clears the flat `sdcpp_*` keys and must carry its own full
  model paths, but `tierseed.expand` and `Expand-SeedValue` replace `__OFFLOAD_HOME__` in strings and string arrays
  only, never inside an object, so the block would ship literal tokens. It waits for recursive expansion in both (superseded by A-132: both expand inside objects now and
  `blackwell-8` seeds it). The
  flat `sdcpp_*` arm stays reachable by flipping `imagegen_engine`, and the image roster is still three models
  (`hidream-o1-dev`, `qwen-image`, `qwen-image-2.1`) (superseded by A-132: four, with `z-image-turbo`).
- **The second general vision seat and its siblings** (LFM2.5-VL, Gemma-4-E4B-vision, the high-quality whisper seat).
  `mediaseat` allows one writer per bound key and has no non-binding seat kind, so they stay hand-wired until it does.
  A rendered vision seat is also not byte-identical to the hand-wired flags, so adopting one needs a measurement.
- **The five default script keys** (`videogen_script`, `voicegen_script`, `musicgen_script`, `run_graph_script`,
  `sdcpp_script`): `config.Default()` already carries the same `render/` forms, so seeding them adds no capability.
- **The HunyuanVideo-1.5 and MiniMax-H3 video families:** builder-default routes with no config key, and
  `VideoFamilyBinding` has no licence field to hold their terms.
- **Every other tier.** The 16 GB-class rows still seed Q5_1 (pinned by
  `TestSixteenGBComfyTiersSeedTheMeasuredEditInpaintAnimateRoutes`) and the Q8_0 Wan pair (pinned by
  `install-config-seed.test.ps1`). Moving them to fp8 is a separate decision.
- The Q8_0 pair, the Q5_1 file and the lightx2v LoRAs stay on disk as the documented opt-in arms, and the weights are
  provisioned out-of-band like every other ComfyUI model.

## [0.158.2] - 2026-10-02 - Concurrent transcriptions keep whisper loaded and wait `gpu_wait_ms` for a held card; the ampere-16 seat leaves the memory stack's reranker room

### Fixed — concurrent transcriptions no longer unload whisper from under each other, and "no speech" is never guessed from a failure something else may have caused (register C-91)

`offload_transcribe` sent its zero-always-warm unload after every call and held nothing while it did: the single-slot
inference mutex is released when `Transcribe` returns, so a call that finished while others were in line unloaded whisper
onto the next call's inference. llama-swap answered the victim `matrix: model unloaded` with a 500, and every call queued
behind it paid a cold start. The same unload fed a second defect: an answer cut off by it read as an empty body, an
empty-body 5xx was filed as `ErrUpstreamNoSpeech`, and a recording with speech in it reached its caller as `empty
transcript (no speech detected)`.

- `Transcribe` and `TranscribeOAI` count themselves into a process-wide in-line count on their first statement (before the
  fenced URL wait and the queue on the mutex) and out when they return. The new `Client.UnloadIfIdle(ctx)` replaces
  the bare `Unload` in `runTranscribe` (one call site, shared by the whisper and the hq/OpenAI protocols). It unloads
  holding the mutex, only when no call is waiting or running and a model was used since the last unload: a burst of
  concurrent calls pays one cold start, and the last call out, even a failed or fence-refused one, is the one that
  unloads. A call that never reached the upstream (a fence refusal, an unreadable wav) unloads nothing.
- It frees every model the burst warmed on that upstream, not only the last call's own. A burst can mix `stt_model` and
  `stt_model_hq` (or the two protocols on two models), they share the one slot, and the call that finishes last knows only
  its own model, so unloading just that one left the first finisher's model loaded until llama-swap's ttl with nothing to
  say so. Each model is unloaded on its own: a failure on one does not stop the others, stays marked warm for the next call
  out, and is returned naming its model. `runTranscribe` logs it (`zero-always-warm unload after a <model> call failed ...`)
  and never fails the transcription for it; the error used to be discarded.
- `UnloadIfIdle` never blocks a finished call on the next call's inference. It checks the in-line count first and takes the
  slot with `TryLock`, because a blocking lock would hold a ten-second transcription's answer back for the length of a
  30-minute one's.
- `ErrUpstreamNoSpeech` ("the audio had no speech") is produced for an upstream that answered 200 with an empty transcript
  (both protocols) and, on the whisper protocol only, for the F-35 crash: an empty-body 5xx in a call that ran alone, with no
  other transcription in line from the moment it joined and no unload sent or going out. This process's own unloads no longer
  land on a call in flight, so such a call has no cause of its making left, and a retry would only crash the server again and
  cost the restart. Any other empty-body 5xx, or an answer cut off mid-read (the read error was discarded), is the new typed
  `ErrUpstreamVanished` (`the upstream vanished mid-request`) on both protocols, whose message says it was unloaded,
  swapped, restarted or crashed under the call: whisper.cpp's exit on audio with no speech content leaves the same bare status
  an unload leaves, and with another call or an unload overlapping it this process cannot say which it was, so it is a failed
  call a caller can retry, never a claim that the audio is silent. **This narrows the 2026-09-23 mapping**, which read every
  empty-body 5xx as no speech. What the rule cannot see is another process doing the same to the same upstream (another
  session's harness, a `gpu reserve` drain, llama-swap swapping the slot for another model): an alone call that dies that way
  still reads as no speech. The OpenAI path never reads no-speech off a failure (nothing says that path crashes on such
  audio). "Alone" is an arrival count and the in-line count taken together under one small mutex, plus an unload count and an
  unload-in-flight count kept by `Client.Unload`, read when a call joins and when it fails.
- The comment on `Unload` that said its caller holds the single-slot mutex is corrected: no caller did. The `runTranscribe`
  doc comment that said it "force-unloads the upstream after the call" now describes the last-call-out unload.
- The `sttclient` tests reset the client's package-global state per test (`resetClientState`, called by all 19 tests that
  transcribe). The set of warmed models is keyed by llama-swap base and httptest reuses ports now and then, so
  `TestACallThatNeverReachedTheUpstreamUnloadsNothing` was flaky under `-count` (twenty repeated runs of the unfixed package
  happened to pass, so the frequency is unmeasured). A regression test shares one base across its subtests, so the leak
  reproduces on every run; `go test -count=20 ./internal/sttclient/` passes.
- Tests: 21 new, 2 rewritten (the two tests that pinned the 2026-09-23 mapping for every empty-body 5xx now pin it for a
  call that ran alone, with sibling tests for the calls that did not). The burst test (six concurrent calls against a
  llama-swap stand-in that aborts what an unload finds in flight) failed on the old behaviour with five calls answered
  `matrix: model unloaded` and six unloads for six calls, and the pipeline test with a failed call and two unloads for two.
  The review's findings were each seen failing before their fix: the mixed burst unloaded only the last call's model, the
  "ran alone" tests answered `vanished mid-request` against a stub that never reads a call as alone, the cut-off 200 came back
  as an untyped decode error, the leak test sent an unload for a model it never warmed, and the unlogged unload failure through
  the mutant that restores the discarded error. 33 mutants measured against the fixed code each turn the intended test red (each
  term of the alone check, each typed return, the OpenAI asymmetry, the line accounting, the unload set and its failure
  handling, the log line, the pipeline mapping, both halves of the reset helper). Three of the first round's mutants (either
  waiter check alone, a blocking lock beside the waiter check) stay green because each of those guards covers the other.
- Docs: `docs/systems/offload-pipeline.md` (Transcription), the `stt_unload_after` comment in `internal/config`.

### Fixed — `offload_transcribe` waits `gpu_wait_ms` for a held card, defers it as capacity, and tells the client it is alive (register C-89)

A transcription under a held GPU waited up to `stt_request_timeout_sec` (1,800 s) for the card: the whisper client handed its
HTTP timeout to the lease fence as the lease-wait budget, while every other GPU door waits `gpu_wait_ms` (90 s by default).
The MCP client aborts an idle call at the same 1,800 s, so the two raced, and when the wait did run out the refusal reached
the caller as `transcribe call failed: gpu-lease timeout …`, error class `timeout` and no defer class, which a delegator
cannot re-place. A long transcription that did run was silent to the client for as long as its audio took.

- `sttclient.Client` gains `WithFenceWait(d)`, the lease-wait budget, and `fencedURL` waits `min(fence wait, HTTP timeout)`.
  Zero is a value, one inspection, as `gpu_wait_ms: 0` is for every other door; only a client built without the option
  waits its HTTP timeout. `pipeline.New` sets it from `gpu_wait_ms`.
- `runTranscribe` files a lease refusal with error class `gpu_busy`, reason `gpu busy: <holder, how long it has held the card,
  its declared window>` and `defer_class: capacity`, the class a delegator re-places and a caller can retry. No door files it
  exactly that way: the agent doors set the reason and the defer class but no error class, and the vision tier sets the error
  class but no defer class (whenever the lease is still held when its wait ends, by any live holder, a text reservation included; a lease refusal on its model call is filed as
  `timeout`). It is checked before the no-speech verdict and before the generic call-failed wording.
- `offload_transcribe` sends progress notifications when the request carries a progress token: an opening one
  (`offload_transcribe started`) and a heartbeat every 30 s (`still working: offload_transcribe running for 4m0s`). The
  delegation doors' reporter serves it: `progressReporter` gains a label for a call with no subtasks, and `startHeartbeat`
  mirrors `startProgress`'s guards (no token, no session, nothing sent). Whether the reference client restarts its timeout on
  progress is still unverified (ADR 0065), so the bounded wait is the fix and the heartbeat is a courtesy.
- Tests: five new, one rewritten (`TestTranscribeUnderAFenceNeverReachesUpstream` pinned "waits inside the client's timeout").
  Four of the six failed on the old code: the fence tests waited out a 2 s client timeout instead of a 300 ms fence wait
  (and instead of one inspection for zero), the pipeline test deferred after 4.1 s as `timeout` with no defer class, and
  the heartbeat test saw no notification at all. The other two guard the cap by the client's timeout and the opt-in on a
  progress token, and pass on the old code by design. Twelve mutants each turn one red.
- Docs: `docs/systems/gpu-lease.md` (the whisper row), ADR 0026 (Extended 2026-10-01), `docs/systems/mcp-server.md`
  (progress notifications), `docs/systems/offload-pipeline.md`, the `stt_request_timeout_sec` comment in `internal/config`.

### Fixed — the ampere-16 seat leaves the memory stack's reranker room too, and the reranker is sized for the pair it scores (register A-122b)

The ampere-16 seed (util 0.87, `max_num_seqs` 8, `max_num_batched_tokens` 4096, ADR 0049 Amendment 4) kept the memory
embedder resident but not the reranker. On 2026-10-01, on the ampere-16 reference box under a GPU lease, with four
concurrent ~20k-token requests, a cold rerank 8 s into the load and rerank + embed every 2 s, the card peaked at 14,717 of
15,356 MiB (seat 13,860, embedder 460, reranker 378), 639 MiB free, and the reranker failed to start beside the loaded seat
7 times (`upstream command exited prematurely` in the node's llama-swap log). The operator's order is that the embedder AND
the reranker stay resident beside the agent seat with about 1 GiB free. At util 0.84, 4 sequences and 2,048 batched tokens,
with the reranker at `--ctx-size 4096 --batch-size 2048 --ubatch-size 2048`, the card peaked at 13,987 MiB (seat 13,204,
embedder 458, reranker 306), 1,369 MiB free; the KV pool is 45,472 tokens (29 blocks of 1,568, 1.39x the unchanged 32,768
window), and the four requests finished in 544 s where the old point needed 900+ s. util 0.85 with 4 sequences did not start
in that arm (unexplained, probably the previous engine still releasing VRAM; not re-run). That point has run on the
reference box since 2026-10-01 19:31 (seat warm in 52 s, `/v1/rerank` 3 of 3 with the seat loaded, 13,565 of 15,356 MiB at
idle); this change makes it what a fresh install or re-render seeds. The 35B fast-layer seat (util 0.85 / 8 / 4096, 969 MiB
free beside the smaller reranker) is unchanged.

- `setup/templates/profiles.json`: the ampere-16 `vllm_seat` declares util 0.84, `max_num_seqs` 4 and
  `max_num_batched_tokens` 2048; the window and `agent_ctx_tokens` stay 32768. Its `measured` note marks the 2026-09-30
  point superseded and carries the A-122b measurement.
- The reranker in the four llama-swap templates that render the memory stack. The memory stack's client truncates each
  document to 6,000 characters and sends every (query, document) pair as one non-causal sequence, at most about 1,500-2,000
  tokens, which must fit one ubatch and one slot. `--batch-size 2048 --ubatch-size 2048` is applied to all four (it was
  4096/4096). `--ctx-size` becomes 4096 on `linux-cuda` and `linux-vulkan`, where the reranker has one slot, and
  **stays 8192 on `win-dual-blackwell` and `win-triple-blackwell`**, which start it with `--parallel 4`: llama.cpp divides
  `--ctx-size` among its slots, so 4096 there would leave 1,024 tokens per slot, below a worst-case pair. The two Windows
  templates therefore take the measured batch and ubatch but not the measured context; their 2,048-token slots are
  unchanged. `linux-cuda` is the template the measured tier renders; `linux-vulkan` and the two Windows templates take
  these numbers on the slot rule and the pair size, not on runs of their own.
- The default `memory_stack` is now `embeddinggemma`, `bge-reranker-v2-m3` and `embeddinggemma-ams`. The memory authority
  node serves its embedder under that id, which the default did not name, so `gpu reserve --unload-seat` (0.158.1, register
  C-87), and the render runner's helper, would have cleared it. `config.Default()` and `DEFAULT_MEMORY_STACK` in
  `render/gpu-lock.mjs` both gain it, appended so `EmbedModel()` still falls back to `embeddinggemma`; a test reads the
  Node literal and requires it to equal the Go default; `config.example.json` is regenerated, since it must round-trip to
  the default. A name a box does not serve is inert: the set only filters what llama-swap reports.
  - `setup/templates/config.json` keeps its explicit pair on purpose. It is written into every Windows install, a
    configured `memory_stack` replaces the default rather than extending it, and `llamaswap-pp-cli bind check` reports a
    `memory_stack` entry that resolves to no roster model as dangling and exits with the typed model-not-found code
    (`tools/llamaswap/internal/cli/bind_check.go`), so a node-specific id in the seed would flag every other box. The same
    holds for anyone who copies `config.example.json` verbatim onto a box that does not serve `embeddinggemma-ams`: `bind
    check` lists `memory_stack[2]` as dangling there. A node that serves its embedder under its own id names it in its
    config.
  - `fleet_reclaim.go` and `tools/llamaswap` hold no literal memory-stack list, so there was nothing to extend there. Their
    keep-set is the llama-swap YAML's resident seats (`ttl` -1 or 0), the CLI config's `keep_set` and
    `LLAMASWAP_KEEP_SET` (the CLI adds `--keepset`). This default does not reach them: an embedder seat with `ttl: 300`
    stays reclaimable to fleet reclaim unless it is named through one of those. Not changed here.
- Guards. `TestAmpere16VLLMSeatDeclaresItsMeasuredBoundLane` pins 0.84 / 4 / 2048 (it still fails on 0.87 / 8 and on
  0.90 / 32, and now on the batch budget too); `ampere16_coresidency_test.go` gains a `maxBatched` ceiling (27B 2048, 35B
  4096) and requires the A-122b measurement in the seat's note; new `TestEveryTemplateServesTheRerankerAtTheMeasuredSizing`
  pins each template's reranker batch, ubatch, context and slot count, exact in both directions; new
  `TestSeatTablesStateTheBatchBudget`; new `TestMemoryStackDefaultsAgreeBetweenGoAndTheRenderHelper`.
- Docs: ADR 0049 Amendment 5 (header note, the measured table, the decision, the costs, a Consequences bullet, the index
  row), `docs/tiers/` regenerated (the seat tables gain a `max_num_batched_tokens` row, so `ampere-16`, `blackwell-16`,
  `blackwell-2x16` and `blackwell-3x16` change), `docs/systems/composite-tier.md`, `docs/systems/gpu-lease.md`. The tier
  matrix workbook's carried live-seat cells (the ampere-16 lane row and the two roster rows) were updated by hand; its
  generated sheets regenerate cell-for-cell identical from this `profiles.json`.
- Costs: the lane seat serves at most 4 sequences at once (it declared 8, and 32 before) and its `concurrencyLimit`
  follows; its batch halves. Single-stream TTFT, the digest-8 gate and the 4-stream aggregate were measured at the
  Amendment 3 point and are not re-run here. The single layer's `footprint_gib` (13.9) is now conservative against the
  measured 13,204 MiB seat and was left alone.
- Tests: three new (`TestEveryTemplateServesTheRerankerAtTheMeasuredSizing`, `TestSeatTablesStateTheBatchBudget`,
  `TestMemoryStackDefaultsAgreeBetweenGoAndTheRenderHelper`) and six existing ones tightened (the bound-lane pin, the
  co-residency ceiling, its note check, `TestDefaultMemoryStack`, `TestUnloadSeatNeverUnloadsTheMemoryStack`, the Node
  default). Fourteen mutants, each applied by an exact-count replacement and each restored with a clean `git diff`
  afterwards, turn the intended guard red: the seat back at util 0.87, at 8 sequences, at 4096 batched tokens, at the
  Amendment 4 point (0.87 / 8 / 4096) and at the Amendment 3 point (0.90 / 32); a batch of 1024 (the exact pin, not the
  ceiling); the 35B above its ceiling; the reranker back at 8192/4096/4096 in `linux-cuda`, `linux-vulkan` and
  `win-triple-blackwell`, and at a literal 4096 context with `--parallel 4` in `win-dual-blackwell`; and the third
  memory-stack member dropped from the Go default, and from the Node default (caught by the drift test and by the Node
  test). `node --test render/gpu-lock.test.mjs render/withgpuslot.test.mjs` passes 39 of 39 and
  `pwsh -File setup/render.tests.ps1` passes in full.

### Docs — the memory stack is not CPU-only on every tier (documentation sweep, round 2)

Round 2 of the 2026-10-01 sweep corrected the places that still described the memory stack as CPU-only, or every
GPU-resident model as unloaded before a render (register C-87).
- `docs/systems/gpu-lease.md`: mem0 sits on the utility card on the three-card reference box and shares the render
  card on a single-card tier.
- `docs/systems/coding-agent.md`, `docs/glossary.md`, `docs/systems/media-generation.md`: the render free step and the
  zero-warm posture keep the memory stack (the mem0 embedder and reranker) resident.
- ADR 0018: a dated note that the Context table's "CPU memory stack" no longer describes every box, and that
  `gpu reserve --unload-seat` keeps the configured `memory_stack`.
- `internal/pipeline/pipeline.go`: a comment called the never-unload set "CPU-only models".

### Docs — the docs describe 0.158.1 as it runs

A documentation sweep after 0.158.1 checked each surface against the shipped behaviour and corrected what had gone stale.
- The memory stack is no longer described as CPU-only with zero GPU VRAM. On the reference box it runs on the utility
  card, not the render card. Neither the render runner's free step nor `gpu reserve --unload-seat` unloads it (register
  C-87): `docs/systems/media-generation.md`, `docs/flows/zero-warm-generation.md`, ADR 0009 (dated note),
  `docs/systems/gpu-lease.md`, and the comments in `render/gpu-lock.mjs` and `internal/gpugen`.
- A warm batch stops when ComfyUI becomes unusable, and the later jobs get `not run:` rows (register C-83):
  `README.md`, `docs/OPERATOR-GUIDE.md`, `render/README.md` (`COMFY_WAIT_SEC` is wall-clock time), the zero-warm flow.
- `docs/systems/fleet-node.md`: in the capacity wait, a local seat whose load cannot be read keeps the deal's busy
  answer (register C-88).
- `docs/OPERATOR-GUIDE.md` and `docs/systems/media-generation.md`: a family binding with no `imagegen_timeout_sec`
  inherits the top-level one, so a slow family must set its own.

### Docs — the last stale sentence filing a three-card Blackwell rig as `dual-gpu` (register I-07)

`docs/systems/setup-installer.md`'s fleet history still said the three-card reference box was "a shape no tier id
covers, which `Get-Profile` files as `dual-gpu`". That contradicts its own classification table and `setup/detect.ps1`,
which file it as `blackwell-3x16` (shipped 0.113.32). Every doc that mentions `dual-gpu` was re-checked against the
detector, and this was the last stale sentence.

## [0.158.1] - 2026-10-01 - A ComfyUI that answers HTTP but cannot render ends the wait and the batch; `gpu reserve` keeps the mem0 stack; an unreadable busy seat keeps the overflow off it

### Fixed — a ComfyUI that answers HTTP but can no longer render ends the wait and the batch (register C-83)

On 2026-10-01 at 12:00:10 a sticky CUDA error on a 16 GB render card killed ComfyUI's prompt worker while its HTTP
server kept answering (register A-128 tracks the fault itself). `/history/<id>` returned `200 {}`, which the poll loop
read as a slow render, and `GET /system_stats` returned HTTP 500. The batch waited 2,894 s on that job against a
1,500 s `COMFY_WAIT_SEC`, then failed the next three jobs at about 3 min each. For those 57 minutes the media lease
held all three cards with nothing running on any of them: a whisper load and the text seat queued behind it, and
delegations overflowed to another node.

- `pollOutputs`, used by every ComfyUI runner, measures `waitSec` in wall-clock time. It used to count polls, so
  slow answers stretched the budget. A suspend still moves the deadline by the time slept, now also when the
  machine sleeps during a request. Before, a request in flight across a suspend counted the whole nap as dead time
  and ended the job on a healthy server.
- While the prompt is missing from `/history`, or `/history` answers with an error status, a `GET /system_stats`
  probe runs every 15 s, starting after 15 s. Two HTTP-error answers in a row end the wait with a *server unusable*
  error that names the probe, the status and the restart. A probe that gets no answer counts for nothing. The
  dead-server watchdog's abort is a server-unusable error too.
- `comfy-render.mjs` exits 3 on a server-unusable error (1 for any other failure, 2 for a caller mistake). When a
  `--no-lifecycle` child waits for its parent's server, three error answers now end the wait instead of ~3 min of
  polling, and the "not reachable" ending is server-unusable as well.
- `comfy-generate.mjs --batch` stops when a job exits 3. The failed job and every later job get a result row (the
  later ones `not run: ComfyUI became unusable at job N/M (...)`), the batch exits non-zero, and its teardown frees
  the card and the lease. An ordinary failure is still recorded and the batch goes on with exit 0, which is what
  the Go side reads job by job. A failed job's `error` now carries the child's own `RENDER FAILED:` reason instead
  of only `comfy-render exited N`.
- `comfy-inpaint.mjs --batch` stops at once on a server-unusable error. Before, it stopped only after three
  consecutive failures, each a full wait. Its `_row: "aborted"` line gains `reason`.
- When `withGpuSlot` adds ComfyUI's console tail to an error, it keeps the server-unusable flag, so a standalone
  render exits 3 too.
- Tests: 18 new; each of the 16 mutants of the new logic turns one red. Two existing poll tests whose mocks answered
  5xx on every URL now answer `/system_stats` as a healthy server, since they test the `/history` watchdog. An end-to-end run of the real
  `comfy-generate.mjs --batch` against a fake ComfyUI that goes poisoned on job 2 stopped in 37 s with a row for
  each of the four jobs. Unchanged main took 631 s on the same run, exited 0, and recorded
  `comfy-render exited 1` as every error.
- Docs: `docs/systems/media-generation.md` (the poll loop and the warm batch).

### Fixed — `gpu reserve --unload-seat` keeps the mem0 stack resident (register C-87)

`--unload-seat` cleared the agent seat and every other model llama-swap held, including the config's `memory_stack`
(the mem0 embedder and reranker). On 2026-10-01 a media lease unloaded the reference box's mem0 embedder. That embedder
sits on the utility card while the render ran on another card, so the unload freed nothing the render could use.
`render/gpu-lock.mjs` has always kept that set; the Go side now matches it. The stack stays resident, and the run
prints `kept the memory stack resident (mem0 never yields to a lease)`.

- `otherResidentModels` returns the configured `memory_stack` as kept rather than as foreign residents; the agent seat
  and every other resident are still cleared. An empty `memory_stack` means the default pair, as it already did on the
  render side.
- When the per-model unload route fails, the legacy `GET /unload` fallback is refused while the stack is resident, or
  while `/running` cannot be read. That route unloads everything whatever its `?model=` says. The reserve then fails and
  names the stack; before, the fallback tore the stack down. This matches `render/gpu-lock.mjs` and the llamaswap tool's
  `unload-all`.
- Tests: five new (memory stack kept, configured stack kept, empty stack means the default, no total fallback with
  the stack resident or residency unreadable). Each failed on the old code, and four mutants each turn one red. `TestUnloadSeatAlsoUnloadsOtherResidentModels` named
  `embeddinggemma` as a foreign resident and now uses `whisper-stt`.
- The `memory_stack` comments in `internal/config` and `render/gpu-lock.mjs` no longer call the stack CPU-only:
  it runs on a GPU, the box's utility card. Docs: `docs/systems/gpu-lease.md`.

### Fixed — a seat whose load cannot be read no longer takes the overflow it was kept off (register C-88)

When a deal finds the local seat busy, it keeps the overflow subtask off it and hands that subtask to the capacity
wait. The wait re-reads the seat on every tick and may run the subtask locally once the seat reads idle. A failed or
ambiguous read was folded into idle (the deal's documented fail-open), so on a box too loaded to answer the metrics
probe in time, the first failed read put the overflow on the seat the deal had just found busy. CI caught it as a
flaky `TestAutoOverflowIsNotStackedOnTheBusyLocalSeat` (run 36937589311).

- `busyReading` gains `unknown`, set when the probe fails or the read is ambiguous. The deal still treats it as idle.
- `localStillBusy`, which only the wait uses, keeps the deal's busy answer while the reading is unknown, on both
  `spread` and `auto`. A seat that never reads again leaves the subtask to the remotes or to the wait's own end.
- Tests: `TestOverflowStaysOffABusySeatWhoseLoadBecomesUnreadable` fails the seat's metrics after the deal's own
  reads. It went red 3 of 3 runs on both routes before the fix. Three mutants each turn it red. The `internal/delegate`
  package passes.

## [0.158.0] - 2026-10-01 - Qwen-Image-2.1 licence warnings removed from results, status and docs

### Changed — no licence warning text anywhere (operator order 2026-10-01)

The operator asked for the repeated Qwen-Image-2.1 licence warnings to go: they had become noise in every session.

- Results no longer carry `license_note`: single renders, edits, `generate-image --batch` items and the batch top level.
  `license` and `commercial_use` stay on results, the ledger, `offload_status` and `/fleet/health`, because the binding
  still declares them (the loader requires both) (ADR 0058, amended).
- The `unknown image family` list no longer appends `[non-commercial: <license>]`. `doctor`'s family route reads
  `license <name>` instead of leading with `NON-COMMERCIAL (<license>)`.
- The config-load warning for `qwen-image-2.1` on a DEFAULT binding stays (ADR 0058 rule 4) without its licence clause. The
  `commercial_use is required` error no longer explains itself in licence terms.
- The `blackwell-8` seed declares `"license": "Qwen Research License"` (was `"… (non-commercial)"`). `docs/tiers` regenerated.
- The warning prose is gone from the `offload_generate_image` / `offload_edit_image_generative` descriptions, the
  `family` property, the `generate-image --family` flag help, README, `docs/OPERATOR-GUIDE.md`, `setup/SETUP-AGENT.md`,
  `docs/systems/media-generation.md`, `docs/systems/mcp-server.md` and `docs/FLEET-NODE.md`. SETUP-AGENT and
  media-generation now say correctly that `blackwell-8` seeds the family in its RAM layer (Copilot review).
- Tests: the family tests now assert the note, the label and the prefix are ABSENT, with `license` / `commercial_use` still
  present. `TestImageFamilySurfaceIsAdvertised` fails if `non-commercial`, `research/evaluation`, `brand or client` or
  `license_note` comes back into either tool description.

## [0.157.1] - 2026-10-01 - research results are labelled and sanitized; an ended run no longer lingers on Windows

### Fixed — `offload_research` labels and sanitizes what it returns (register SF-45, security standard gate G13)

A research digest is a local seat's reading of someone else's page, so an instruction planted on the page could survive into
the text the calling session reads, unlabelled and unbounded; the agent's own fetch and browse tools already fenced page
content, the research door did not.

- The body gains `untrusted` right after `summary`: a one-line notice that `results` and `sources` are third-party page content,
  data to weigh and never instructions (the head of the body is what a truncating client keeps).
- Every string in `results` and `sources` (object keys too) goes through the new `internal/untrusted` package: zero-width,
  format and line-separator runes dropped, chat-template and instruction-header markers neutralized, each string capped at
  16,000 characters with a note. The cap is twice the longest digest (7,952) and prose answer (8,317) over 970 finished
  research runs, so it never cuts a real one. A part that cannot be walked is withheld with a note instead of published
  unfenced; numbers keep their exact text. Inside each result, fields now marshal in key order.
- `internal/agent`'s fetch, browse and search tools use the same package, so the markers and the rune rules live once.
- Tests: the notice and the sanitize-and-cap tests failed on the old handler; six mutants each turn one red; the C-75 field
  order and digests-before-sources pins still pass. Docs: `docs/systems/security.md` (G13 shipped), `docs/systems/mcp-server.md`.

### Fixed — an ended run's record is removed even while a reader holds it (register C-84)

`gpuactivity.Handle.End` ignored `os.Remove`'s error. On Windows a delete fails with a sharing violation while a reader (a
drain, a status call) has the record open, so an ended run stayed registered with its heartbeat stopped until `HeartbeatTTL`
(120 s) aged it out: a drain waited on a run that had ended, and the local run cap counted it. `End` now retries the delete for
the same bounded window `write()` gives its rename (20 x 5 ms); a path already gone counts as removed.
`TestEndRemovesTheRecordEvenWhileAReaderHoldsItOpen` failed 5 of 5 on the old code. The drain test that toggled a seat's
in-flight count from a timer goroutine against an 80 ms stuck bound now changes it on every probe, and the drain's give-up
message prints a sub-second span in milliseconds (it read "0s" for 80 ms). The twelve drain tests passed 60 runs each.

## [0.157.0] - 2026-10-01 - the structured re-pack is bounded by the wall; a named layer is honoured on every deal; the Hailo-8L stays on its box; read_file is metered in characters

### Fixed — the structured re-pack is bounded by the wall, resends a cut request only when it can help, and says what it did (register C-80, ADR 0055 item 9)

On 2026-09-30 a 9B agent seat at about 5.6 tok/s ended its loop 219 s into a 600 s wall and then spent 1,670 s re-packing a 2,782-byte answer: the grammar request, sized at 1,439 tokens, ended `length`; the same request, resent unconditionally at the 8,192-token cap, ended `length` again; the chat lane answered JSON numbers where the schema wanted strings. Nothing compared what an attempt asked for with the time left, and the wire kept a count of attempts and nothing about what any of them wrote.

- **The wall bounds each attempt, by arithmetic.** Before every attempt the node takes the time left before `min(the run's ceiling, the wall's end + the liveness slack)` and the seat's decode rate (the seat-rates store, else this run's streamed rate, else the rate its own completions measured, else `agent_seat_tok_s`). An attempt whose `max_tokens` fits is sent as sized; one that does not is sent with the tokens the time buys, down to the answer's own size (`len/3 + 64`); under that it is skipped, and so is every later attempt. A skip with no attempt before it is a `budget` defer (`structured re-pack skipped: ...`) carrying the finished answer, flagged `schema_miss`. It is a token budget and a deadline check, never a transport timeout: a request in flight is not cut. With no known rate there is no arithmetic, and a failed re-pack says `re-pack time bound off` in `repack_note`. A loop that ends after the wall plus its grace therefore gets no node-side re-pack, and its structured result depends on the delegator's rescue.
- **A cut grammar request is resent at the cap only when the budget was the problem and the time left buys it.** The retry used to go on every truncation, and a greedy seat answers the same request byte for byte. It now needs a budget under the cap, a tail that is not a runaway (whitespace, or a block of up to 64 bytes repeated back from the end of the output across at least half of its last 256 bytes, or a block of lines repeated), an answer that needs more tokens than the budget held at the density the seat wrote, a budget the wall did not set, and enough time left to carry what the answer needs. Otherwise the loop goes to the chat lane. A re-pack attempt the clock narrowed and the seat cut, with no runaway tail, is a `budget` defer and no longer an `abstention` (which the delegator retries on another node).
- **The prompts and the grammar stop permitting the suspected runaway.** Both prompts name each field's type and a list's item type; the grammar lane names what its grammar enforces (every array is a list of strings, an object a string), the chat and vLLM lanes the schema's own. The grammar's whitespace rule is llama.cpp's own bounded `ws ::= | " " | "\n" [ \t]{0,20}`, which moves every grammar's text and so each seat's prompt-prefix cache key once (the four grammar-seat golden digests were re-taken; with the old rule swapped back they reproduce exactly). The scalar coercion turns a bare JSON number into the string a string field or list item asks for, keeping its text, reads array items both ways, and says which fields it re-typed (`coerced to the schema's types: ...`).
- **Every attempt is on the wire.** `repack_attempts_detail` (additive, omitted when empty) carries one record per attempt: lane, `max_tokens` and `clamped_from`, tokens generated, finish reason, wall, the first and last 80 bytes of what the seat wrote, and why it failed or was skipped. `llamaclient.GenResult` gains `FinishReason`. `tokens_out` counts failed attempts too, including a stream that died mid-body (`llamaclient.BodyError` now carries what it had delivered). The row an `agent_delegate` caller reads carries the first four records.
- **A finished answer is not dropped by a bare defer.** A deferred `offload_ask` (local or routed) carries the loop's prose as `output`, flagged `schema_miss`; `offload_review_diff` stays bare (its grounding filters never saw the raw prose). The rescue stays unwired on those doors, as `docs/systems/coding-agent.md` records.
- Unverified when this was written: that whitespace was what the incident's seat wrote (the request was not replayed; the attempt records will show it), and that a live llama.cpp compiles the bounded `{0,20}` repetition. The bound reaches llama.cpp grammar lanes only: a vLLM seat's `structured_outputs` carries no whitespace control.
- Docs: ADR 0055 item 9, `docs/systems/fleet-node.md`, `docs/systems/mcp-server.md`, `docs/OPERATOR-GUIDE.md`, and a corrected `timeout_sec` row in `docs/FLEET-NODE.md` (the wall is not a deadline).

### Fixed — the standalone Hailo-8L is never published, advertised or accepted over the fleet (ADR 0038 Amendment, register E-08)

The box that carries the Hailo-8L is standalone by operator decision (register J-13): it is never delegated to, never a fleet node and
sends nothing to another node. That held only by omission, because nothing in the harness kept `fleet-serve` from advertising the device
or running an `accel` job for it. One filter, `config.FleetVisibleAccelerators` (keyed on `config.LocalOnlyAccelerator`, which names
`hailo-8l`, compared trimmed and case-folded, and nothing else), now sits at the three fleet-visible points. The decision is per device:
the Coral Edge TPU and the RKNPU are published, advertised and accepted exactly as before. Every local path (the MCP tools, the agent
loop, the pipeline lanes, `config.HasAccelerator`) is untouched, because the device never leaves its box and is not switched off.

- `/fleet/health` `accelerators`: `fleetAccelerators` (main.go) filters the list it hands the server, and `fleetnode.New` filters again into
  the list health publishes, so an `Options` built by hand cannot publish the device either. The source choice is unchanged: the installer
  manifest still wins when it lists anything, else the config's `accelerators`, and the filter then applies to what the winner listed. A manifest
  that names only the Hailo-8L therefore publishes nothing, even beside a config that lists another device.
- The `accel` lane: `taskConfiguredFor("accel")` keys on `config.FleetVisibleAccelerators`, so `supported_task_types` and the pull door's claim
  name `accel` only for a node that carries a device the fleet may use. A node whose only device is the Hailo-8L runs no `accel` lane.
- `accel` job acceptance: `buildAccel` refuses a local-only id by what it is, before it consults the node's own list, with the same text whether
  or not the box carries the device, and its refusal lists only the devices the fleet may use. The push door answers 400 and the pull door nacks,
  because both reach `buildAccel` through `BuildRequest`.
- `fleet_accelerators` naming `hailo-8l` is a `doctor` FAIL row and a startup warning. It loads and nothing refuses, because no node can answer it
  and every forwarded call would defer.
- `fleet-ui` (`refuseHailoPort`): its documented default listen, `127.0.0.1:18813`, is also the Hailo sidecar's documented endpoint, so on a box that
  lists `hailo-8l` the page would squat the sidecar's port. Such a box refuses a listen port equal to `hailo_endpoint`'s (the documented default when the
  key is unset, the scheme's own port when the base has none; the port only, whatever the host half) and names the way out, `--listen` with another port.
  The check runs after `refuseListen` and before anything binds or polls. Neither documented port moves: a box with no accelerator, or with only the
  Coral or the RKNPU, binds the default exactly as before.
- Not in this change: refusing `fleet-serve` on a standalone box, and knowing which other boxes name the device in `delegate_remotes`. The harness
  cannot read another box's config, so that stays an operating rule.

Tests: 18 new top-level tests (`internal/config/localonly_test.go` 5, `internal/fleetnode/accel_localonly_test.go` 7, `internal/tierseed` 1, and 5 in
the root package across `fleet_accelerators_test.go`, `fleet_ui_cmd_test.go` and `doctor_findings_test.go`). Red first against the 3ce9fbf5 behaviour, with
the new symbols stubbed to no-ops so the tests compile: 16 of the 18 failed, and the other two are controls that pass before and after
(`TestDoctorAcceptsAFleetServedAcceleratorInFleetAccelerators`, `TestFleetVisibleAcceleratorsDoesNotMutateItsInput`). 11 of 11 guards reverted on their
own turned a test red, and 9 more mutations of parts the first set left alone were each caught in review. Two older fixtures
(`TestHealthAdvertisesAccelerators` and the first case of `TestFleetAcceleratorsManifestThenConfig`) used the Hailo-8L as their advertised device; they now
use the Coral and the RKNPU with the same assertions, and the Hailo-8L cases are new tests. `TestEveryDeclaredAcceleratorHasAFleetVisibilityDecision`
(`internal/tierseed`) fails when `profiles.json` declares an accelerator with no fleet-visibility decision.

Docs: ADR 0038 gains an Amendment (2026-10-01) and its index row notes the exception; `docs/systems/accelerators.md` (Fleet routing, the manifest-advertised,
`accel` and config-fallback sentences, a source-map entry), `docs/systems/fleet-overview.md` (the port refusal) and the operator guide's `doctor` findings row
follow. The install runbook (`setup/SETUP-AGENT.md`) told an operator hand-seeding an existing box to append `hailo-8l` to `installed.json` "so `/fleet/health`
advertises the device"; that list's only reader is the health list, which now drops the device, so the runbook says the config keys enable it and
`installed.json` needs no edit.

### Fixed — a contract that names a layer is never handed to the local seat of a box that does not declare it (register A-108)

A subtask whose contract names a `layer` (register A-100) that the delegator's own box does not declare ended as `layer fast requested; this box declares no layers` while a node that declares the layer sat idle. The rule lived in `gate.Place` alone, and neither joint deal calls `Place`: `route=spread` dealt the local rotation slot without looking at the contract's layer (a 2026-09-19 ledger shows six such defers, all on slot 1 of 2), and `route=auto`'s idle-local shortcut kept the subtask unconditionally. One predicate, `runner.localServesLayer` (the delegator's own `layers` config, and a view that carries rows), is now asked at every place the delegator chooses its own seat:

- `route=spread`: the local slot leaves the rotation for that subtask, exactly as a text lease takes it out, and the subtask is dealt among the remotes that declare the layer. When every one of them is dealt to its headroom or holds a backlog past the caller's patience, the overflow waits in line for them (the capacity wait), never as a defer from the seat, and it spends no slot of the seat's run cap. With no node declaring the layer it defers naming it.
- `route=auto`: the idle-local shortcut is for the work the box can run, and an idle box reads the roster only when some contract names a layer it does not declare. For `route=auto` and `route=remote` alike, when the remotes answer and none declares the layer, the verdict says `layer fast requested and no answering remote declares it`, where it used to claim `placement and gate disagree — please report` (the line meant for a bug in this package).
- The capacity wait never offers the seat to such a contract, and it closes as the capacity defer it is, not as a defer naming a lease holder on a seat that could never run it. A re-placement has no local last resort for it, and a verification retry goes to another node that declares the layer, or there is no retry.
- A refusal that frees nothing (anything but a 503 or 429: a 409, a 404, a dial failure) from the only node that declares the layer no longer waits on a text lease held on the local seat. It used to spend the whole `agent_placement_wait_sec` and end as a capacity defer saying no node had room; it now fails at once as `placement refused`, naming the refusal and the layer, which is what it already did without a lease. A box that declares the layer keeps the lease wait.

A contract that names no layer, and a box that declares the layer, are dealt exactly as before. `internal/delegate/deal_layer_test.go` pins every place above and the controls (the lease case was found in review, and its test failed first); the `agent_delegate` schema text, the `delegate --route` help and `docs/systems/{fleet-node,composite-tier,mcp-server}.md` state the exception.

### Fixed — `read_file` is metered in characters, so paging one file is no longer a call count (register D-103)

Live 2026-10-01 00:16: a seat whose window probe had fallen back to 8,192 tokens (the loop's cap on ONE tool result was
therefore 1,090 characters) read a 12,005-character context document in pages, as `read_file`'s own hint tells it to
("use offset=N to continue"). Eight reads of at most 1,090 characters each (six of them exactly 1,090) held 8,720 of the
document's 12,005 characters, and the ninth was refused "now DISABLED" by the same-tool cap, which counts calls (default 8).
Under a count cap of eight no seat could have read that document to its end.

- `read_file` is charged to a per-run character budget at the length the transcript keeps (after the loop-boundary trim:
  the figure the effect ledger records as `obs_chars`). Once the charge reaches the budget the next `read_file` call is
  refused and the tool is withdrawn through the same `disabledTools` path the same-tool cap uses; the call that crosses the
  line completes. The default is the larger of the step budget and the same-tool cap, times the per-result cap (13,080
  characters at the incident's 12 steps and 1,090 cap): it cannot refuse a seat that reads once per step, and it bounds the
  rest, many reads in ONE step. Measured on the delegation log (2026-09-07 to 2026-10-01): finished runs read 6.4 KB at the
  median and 95 KB at p99 through `read_file`; 214 of 3,374 runs issued parallel reads, up to 12 in a step.
- The same-tool cap counts `read_file` by FILE: the first read of each path counts (a six-file reconnaissance still costs six
  and the ninth distinct file is still refused), and a new page of a file the run has already read costs characters, not a
  call. An exact repeat, a read that failed, a call whose path cannot be read and every other tool count as before. So does a
  page that gave nothing new (the tool failed or refused the call as not performed, or the page began past the end of the
  file), once it has run: it costs the budget next to nothing, so without that a seat paging past a file's end or re-sending
  a failing page was bounded only by the step budget (found in review: 30 or 31 executions against the cap's 8; now 9, the
  call that crosses the line completes). The same lines asked for under another spelling of the call stay new pages, bounded
  by the budget and the steps.
- Unchanged: the exact-repeat refusal, the withdrawal of a tool refused twice for one call (D-49), the setup replay (D-48; a
  replay is not charged to the budget), the forced final step (D-89), `max_calls_per_tool` env rules (they still count every
  call) and every tool but `read_file`. API: `Loop.WithReadBudgetChars` sets the budget (a non-positive value restores the
  derived default) and `Loop.WithoutReadBudget` restores the pure call count; neither has a flag.
- Docs: `docs/systems/coding-agent.md` (the budget; "Three limits"), `docs/OPERATOR-GUIDE.md` (the `--max-same-tool` bullet,
  which now states its default of 8, and the loop-budgets row), `CLAUDE.md`.
- Tests: `internal/agent/loop_readbudget_test.go` (red against the call-counting cap; the page-that-gave-nothing-new tests
  were red against the first D-103 commits; mutation-checked with `go test -overlay`).

### Fixed — a retry respects a plain text reservation of the local seat (register C-81)

`alternativeNode` read the lease only through `Fenced` (exclusive, draining, media) and, for a seat-down defer,
`localSlotAhead`, never `Reserved`. A plain `text` lease does not fence (the affinity gate admits the load), yet it keeps every
first placement off the local seat, so a verification retry or a seat-down re-issue whose first attempt ran on a fleet node could
fall through to a forced local placement and run on cards a measurement had reserved. The retry now asks `Reserved` right after
`Fenced`: with the local seat reserved it goes to the best untried remote, and with none it is skipped with a `retry_note` that
names the holder ("a retry placed there would run on the cards the holder reserved"), the same outcome D-94 chose for a fence. The
holder's own child stays exempt, and the fence's wording and behaviour are unchanged. `internal/delegate/retry_reserved_test.go`
failed on the old code (the retry ran on the reserved seat with an untried node idle); ADR 0063 decision 10, the operator guide and
`docs/systems/fleet-node.md` say why a reservation is skipped and not queued.

### Fixed — a draining `gpu reserve` that loses its lease queues again instead of dying at the restamp (register C-59)

- The wrapper form kept draining a card it no longer owned and exited with `stamping the lease after the drain: restamp: the lease
  is gone`, its command never started. The drain's heartbeat now ends the drain as soon as the lease is confirmed gone (the
  restamp still comes before the unload), the reserve says `the lease was lost during the drain`, releases what is left and takes
  its place in the line again with what remains of `--wait` (never under the 2-minute drain floor; `--wait 0` stays one try),
  then drains and clears the seat afresh under the new lease. After five re-queues the next loss gives up loudly. A seat fault with
  the lease still held is returned as before.
- The drain's and the warm-back's heartbeats ended on one failed write, including a write that failed with the lease record still
  ours, which left a multi-hour drain without a heartbeat until the next acquirer could reclaim it (and abandoned a warm that was
  not lost). Now only a lease that is actually gone ends them; a failed write is reported once and retried.
- A stale-claim reclaim could delete a live claim: `TryAcquire` judged a claim stale from one read and removed it by path, so two
  acquirers reclaiming the same dead holder's record could interleave and the slower one deleted the claim the faster one had just
  created. The removal now runs under the epoch lock, reads the record again and removes it only if it is still stale. Found by
  reading and reproduced deterministically; whether it caused a live loss is unproven.
- The `--detach` drain error says whether the lease is still held, instead of always pointing at `gpu release` (which would
  release another holder's lease).
- Register D-124 clause (b), an `--unload-seat` acquire while a warm-back is owed or in flight, was already green on main (the
  warm-back runs under the releasing holder's own lease and is skipped when a lease is queued); four tests now pin it from the
  acquirer's side, and `docs/systems/gpu-lease.md` writes down the orderings. No acquire-side wait was added.

### Tests — one job's PAIR frames are not asserted in arrival order (register C-77)

`TestFailedLocalPlacementReportsErrored` read the terminal frame by position, but the emitter posts each frame on its own
goroutine, so once a process is warm the terminal frame can reach the ingress first (it failed 69 of 500 runs in one process at
`GOMAXPROCS=1`, 0 of 300 fresh processes). Production tolerates the reorder by design: the open-card markers follow the frames in
order, and PAIR merges a job's frames by lifecycle rank. The test now checks the frames present and the card they describe, and a
held case asserts the terminal frame landed first. `docs/systems/pair-workloads.md` says which runs can deliver the terminal frame
first and that a subtask refused before placement emits none.

### Fixed — the `offload_nim` base audit writes its row on a host with no `state_dir` (security standard L5, S-30)

Under `nim_base_policy` `audit` (the default) a caller-named base outside the allowlist runs, the result carries
`base_policy`, and a would-refuse row is appended to `nim-base-audit.jsonl` for the audit-to-enforce decision (ADR 0067).
The handler passed `cfg.StateDir` straight to the writer, which refuses an empty directory, so on every host whose config
leaves `state_dir` unset no row was ever written and the note said "the would-refuse row was not written: no state_dir".
The row now goes to the machine-wide state root the GPU lease uses (`gpulease.ResolveStateRoot`: `state_dir`, else
`LOCAL_OFFLOAD_STATE_DIR`, else `%ProgramData%\local-offload` or `/var/lib/local-offload`). A root the resolver refuses (a
cloud-sync folder) still lets the audit-mode call run, and the note names the reason. Two handler tests fail on the old code
with the message seen live; the three existing base-policy tests still pass. `docs/systems/mcp-server.md` names the root.

### Fixed — the pair tier seeds its measured L1 staging (register B-02)

`blackwell-2x16`'s pair seat seeded `cache_server.l1_staging_gb` 2, the default for a seat with no measured value. The pair
has one: a 2 GB cut starved its staging under production load (the stores came up short by 34 blocks and nothing reached
L2), so it runs 8. A fresh install of the tier now seeds 8; the three-card flagship already seeds 16, and
`TestDualBlackwellSeedsThePairSeatWithTheCacheServer` pins both (it failed on the old profile with "l1_staging_gb = 2, want
8"). The 2 GB launcher and spec default stays for unmeasured seats; the comments that called 2 GB measured for every seat
now say which seats measured what (`seat_fg.sh`, `internal/vllmseat`, `internal/config/kvcacheserver.go`,
`docs/systems/cache-server.md`). `docs/tiers/blackwell-2x16.md` is regenerated.

### Docs — the installer table classifies a 3-card Blackwell rig as `blackwell-3x16`

`docs/systems/setup-installer.md` still said a 3x Blackwell rig files as `dual-gpu`, quoting a self-test assert that now
reads `3x blackwell -> blackwell-3x16`. The classification table gains the `blackwell-3x16` row, the `dual-gpu` row names
what still lands there, and the `big_ram` note says all three multi-GPU rules set it.

### Docs — the security standard's admission checklist and the research-digest surface

`docs/systems/security.md` gains the L8 admission checklist for third-party tools, model weights, plugins, MCP servers and
desktop apps: ten read-only checks by the reviewer and one for the operator alone (a malware-scanner lookup by hash, and the
install approval). Nothing in it downloads anything new or approves anything (I2). MCP servers and desktop apps join the
provenance inventory's scope. A new "Named surfaces" section names the research-digest channel (digests are third-party
content written by a seat, and the research door does not yet label or bound them as the agent's fetch and browse tools do)
with gate G13, open.

### Changed — the copyright holder is the handle, not a person (register H-55)

The two tool modules' generated Apache headers (462 files), their LICENSE and NOTICE lines, the skill author fields, the generator's own config (which would have re-stamped the headers) and two patch files name the holder as `dmmdea`, as the root NOTICE already did; prose in a few older docs and the embedded ADR text of two digest contracts says "the operator". No code changes.

### Docs — two defaults described as the code runs them

`internal/agent` has defaulted the per-run same-tool cap to 8 since 0.80.0; the operator guide, the README and two `builder.go` comments still said 3. A placement under a held text lease waits for the larger of `agent_placement_wait_sec` (default 120 s) and `agent_lease_wait_sec`; the config comment and the operator guide still said the latter's default 0 meant "defer at once".

## [0.156.1] - 2026-09-30 - the ampere-16 seats leave the memory embedder room; run ids are unique on a coarse clock

### Fixed — the ampere-16 vLLM seats leave the memory embedder room on the 16 GB card (ADR 0049 Amendment 4)

Operator order 2026-09-30: the memory embedder has absolute priority, so the seats make room for it instead of excluding it.
On the reference ampere-16 box (NVIDIA A2, 15,356 MiB) with the embedder (452 MiB) and the reranker (372 MiB) resident, the
lane seat declared at util 0.90 with `max_num_seqs` 32 ran the engine at 14,788 MiB under four concurrent 8k-token requests
(graph capture and workspaces sized for 32 sequences), so the embedder could not load and every memory write failed for 35
minutes.

- `setup/templates/profiles.json`, `ampere-16` only: the lane seat `qwen38-27b-gsq-vllm` declares `gpu_memory_utilization`
  0.87 (was 0.90) and `max_num_seqs` 8 (was 32), window 32,768 unchanged: engine peak 13,984 MiB, card peak 14,833 MiB with
  both support models resident, embedder answering in 25 ms, KV pool about 34k tokens. util 0.82 is refused (KV 0.64 GiB <
  1.29 GiB for one 32,768-token request) and util 0.87 with 32 sequences is refused (28 Mamba cache blocks), so the sequence
  count comes down with the util. The 35B fast-layer seat `qwen36-35b-a3b-gsq-vllm` declares util 0.85 (was 0.90): KV
  0.91 GiB = 67,025 tokens (2.05x the window), engine peak 13,630 MiB. `blackwell-16` seeds the same lane seat id for its own
  card and is untouched. An installed box keeps its rendered unit until `install vllm-seat` is re-run; llama-swap's
  `concurrencyLimit` for the lane seat follows `max_num_seqs` (32 to 8; the default local run cap is 4).
- `ampere16_coresidency_test.go`: a ceiling guard fails when either seat's util or `max_num_seqs` rises above its measured
  point (or a seat of the tier is not registered); the measured notes must carry the 2026-09-30 re-measure; the bound-lane
  pin is 0.87 / 8.
- The tier pages state the lane seat's `max_num_seqs` beside its util; `docs/tiers/ampere-16.md` and the three other vLLM
  tier pages are regenerated. ADR 0049 gains Amendment 4.

### Fixed — the run registry no longer loses a run begun in the same clock tick as another (register C-82)

`gpuactivity.Begin` named a run `<pid>-<unix nanos>`. On a coarse clock (Windows reads the clock in 0.5-1 ms steps) two runs
begun in one tick of one process got the same id, the second record overwrote the first, and the registry undercounted the
runs on a seat, which feeds the local run cap (`modelaffinity.AwaitSeatSlot`) and the seat-load figure. The id is now
`<pid>-<unix nanos>-<seq>` with a process-wide atomic sequence, zero-padded to six digits so same-tick ids sort in begin
order (the seat's line breaks a same-millisecond tie by comparing ids as strings). `internal/delegate`'s
`TestSpreadDealCountsWhatIsAlreadyRegisteredOnTheLocalSeat` failed 38 of 150 runs alone on Windows before, 0 of 180 after.

## [0.156.0] - 2026-09-30 - the whole call has a deadline below the client's abort (ADR 0065)

### Changed — `agent_delegate` and `offload_research` answer before the client's abort instead of losing finished work behind one slow subtask (ADR 0065)

Both doors now own a whole-call deadline, `agent_call_deadline_sec` (default 1,500 s, measured from handler entry; `0` is the default and a negative value means none; a value at or above the client's 1,800 s abort, or a negative that was meant as a number, is a `doctor` finding and a startup warning). At the deadline the call returns what has finished and publishes every unfinished subtask as a budget-class defer whose reason opens `call deadline reached; N unfinished` and says what that subtask was doing. The outstanding work is cancelled and nothing further starts, later research chunks included. The cut changes what an outcome is called, never what was measured: steps, tokens, stop reason and trace are kept, and what the outcome itself reported is quoted. It is applied where an outcome is produced (an attempt's end, a wait's end, and a refusal chain the delegator closes), so the result, the ledger row and the corpus row agree, and every such row carries `reason_code` `budget`. A remote job the deadline cuts is given up through the single withdraw path of ADR 0064, the one every cancel uses: one `DELETE /fleet/jobs/{id}`, best effort, never for a job last seen running and never a second time for the same job, bounded by the unwind allowance once the deadline has passed. A confirmed withdrawal closes the intent as `withdrawn`; anything else leaves the job to the recovery pass and the reason says what the node answered. The unwind is bounded (a twentieth of the time left, 250 ms to 10 s): a seat that ignores its context is abandoned and published as `did not stop` under the job id of the attempt that had not returned, which also gets its own row, and `route=queue` takes one last look at each job whose poll it cancelled. A deadline defer is a result shape, not a failure: `isError` is now set only when nothing succeeded and something failed, was lost to the stack or was skipped, and `offload_research` puts `summary` first and the digests before the sources, so a client that keeps only a reply's head and tail no longer cuts the digests out (register C-75). A request with a progress token gets `notifications/progress`, and the rigger classifies a cut on its own `call-deadline` axis. The deadline composes with ADR 0063's holds without blaming a node, a seat or a page: a capacity-wait tick the call's context cuts keeps the previous tick's state, a retry's wait for a busy seat or its choice of a node that the deadline ends says `call deadline reached` in its `retry_note`, the per-page retry cap does not count a cut, and a refusal chain closed after the deadline is the deadline's defer rather than `placement refused`. The CLI verbs take no deadline, and `agent_run` and `offload_ask` with a route and the review lane's fleet path still carry none (ADR 0065, "Not solved here").

### Changed — the call deadline composes with the seat-down re-placement (ADR 0065 with ADR 0066)

The call deadline and the seat-down re-placement (ADR 0065 with ADR 0066) compose, and the deadline is worded first wherever they meet. The credit a `seat down:` defer carries (the node's admission, its wait on the dead seat and the wall of a failed delegator-side rescue, at most one contract wall together) lengthens the re-placement's budget on the contract's clock and never on the call's: a re-placement still running, or waiting for capacity, when the deadline passes is cut with its own `budget` row (a wait gets a closing row under a job id of its own), the first attempt (the seat-down defer, produced in time) is published as it was with the cut in its `retry_note`, and a retry the deadline cut counts for nothing against the per-page retry cap. A `seat down:` defer produced after the deadline (a local run answering from the unwind, or the finished answer of a seat lost in the structured re-pack: the delegator tries to re-pack it itself first, that rescue runs on the call's context, and the deadline ends it) is the deadline's budget defer: it quotes what the node reported (`the run itself reported infrastructure: seat down: ...`), keeps `seat_recoveries` and `seat_down_wait_sec`, is not re-placed and is not counted in `lost_to_stack`. Its `reason_code` is `budget` and its rigger axis `call-deadline`, so a count of `seat_down` rows undercounts outages that surfaced after the deadline; the reason text keeps the `seat down:` words. When the deadline ends the choice of a retry node, `retry_note` leads with `call deadline reached before a retry node was chosen` and, when the local seat is fenced, names the fence behind it without claiming that no other node is eligible. A re-placement the deadline cuts before it begins is filed like a chain nobody took (one re-placement fewer than refusals, `N refusal(s) and no node took it`) instead of `re-placed after N refusal(s)`, so a lone refusal with nowhere to go no longer counts in `summary.replaced`. A job the node had already answered (a finished answer the rescue was still re-packing) or ended in failure when the deadline passed is cut as `had been answered by <node>` / `had ended on <node>`, never as still on the node, and its intent is closed as the terminal observation instead of left open for the recovery pass to file as an orphan.

## [0.155.0] - 2026-09-30 - a seat that goes down is waited for and the failed step re-issued (ADR 0066)

### Fixed - a dead or wedged engine no longer takes its runs with it (ADR 0066, register C-72)

On 2026-09-29 the three-card flagship engine on the reference workstation died ten times in one evening (its engine core waits 120 s
for a hung pipeline step, then the API server exits). Every death took every run in flight with it as an independent
`stalled:` although the agent loop holds its whole transcript and needed only the seat back; the delegator then treated
each such defer as terminal; and while the seat could not start, every request got llama-swap's `500 src=llama-swap`,
which the client waited out on the 90 s contention budget and the run reported as `seat contended ... (peers hold its
slots - raise concurrencyLimit)`.

- A seat is DOWN on positive evidence. **Wedged**: the busy hold's engine-flat verdict (counters flat for the flat bound
  while llama-swap lists the seat ready), retyped as `SeatDownError` - it wraps the stall it replaces, so a reader that
  only knows the stall still sees the arithmetic. **Died**: the seat left `/running` inside an established hold (two
  reads running), or a model call failed like a dead seat (a cut stream, a 5xx, a refused connection) and a fresh read
  shows llama-swap listing the seat starting, stopping or not at all, or the engine's own local address refusing
  connections. A failure alone is never the evidence, a refusal of llama-swap's own address is not the engine's, a
  thrash (the engine steps but produces no token) stays ADR 0061's plain stall, and a cut tool call (the engine
  answered) is a budget defect, never a seat check.
- The monitor cancels only the model call in flight and the run's context stays alive. The run then waits as ONE
  bounded episode in phase `cold-load`, counted from the first verdict, and the loop re-issues the SAME step (same
  transcript, budget and thinking mode) without spending a step. A wedge must show a change (the counters moving, or a
  restart) first. A seat llama-swap does not list is started by the re-issue itself; when that start fails (on
  2026-09-29 the launcher refused to start the flagship for 18 minutes and every request got an instant 500) the run
  waits one poll and triggers again until the bound, instead of turning each request into two quick 500s and a defer,
  and a start that never succeeds ends the run typed at the bound naming the attempts and what the seat last looked
  like. A seat that was seen serving again earns exactly one re-issue: a second failure before the step's answer ends
  the run. A recovery is counted when the re-issued call's first byte arrives, never for a re-issue that recovered
  nothing; a run recovers at most twice.
- What does not recover ends `seat down: ...` (infrastructure), also when the seat is lost during the structured re-pack
  (`(during the structured re-pack)`, the finished answer kept in `output`). New wire fields `seat_recoveries` and
  `seat_down_wait_sec` (the whole episode, including the time llama-swap held the re-issued call while it started the
  seat), carried to the delegator's published result too.
- The delegator re-places a `seat down:` defer on another node - the second retryable infrastructure defer, after the
  coherence defer - crediting the retry's budget with the node's admission plus its wait on the dead seat (capped at one
  contract wall). The dead seat's own `min_turn_sec` does not gate its re-placement (the retry seat's own floor still
  does), and an alternative node at its ceiling does not refuse it (the node's queue is the line); with nowhere to go,
  `retry_note` says so. The retry is not checked for a busy alternative, but it can still wait in ADR 0063's bounded,
  credited capacity wait and end as a capacity defer after `agent_placement_wait_sec`. While the local seat's run-cap
  line has no free slot, the retry goes to an untried remote instead of joining that line; it stays local only when no
  remote is left, because joining the line beats losing the job. A verification retry is unchanged.
- The per-page retry cap (ADR 0063) counts a page issue whose seat-down retry also failed on its retry seat, so a page
  that keeps killing seats backs off like any other failing page.
- `seat contended:` is a 429 only; a 5xx that outlives the budget is `seat not serving:`. The run's seat check is a gate
  on the contract's shared contention budget: while the seat is not serving no client sleeps on a 5xx, the loop's chat
  calls and the structured re-pack alike.
- The node says once per run what its seat did to it (a recovery, or that the seat could not be read from here, so a
  dead seat would have read as an ordinary error).

### Fixed - the stall allowance is sized for the load, and the rates store hears only the seat's own numbers (register C-66)

- `StallPolicy.AllowanceLoad(phase, pending, load)`: prefill = pending / (rate / load) x 1.5 + slack, the re-pack bound x
  load (load 1 is `Allowance`, exactly; the 60 s floor still defines a silent seat). The load is sampled from this box's
  run registry when a prefill or re-pack begins and at its first delta, and from the engine's own running + waiting
  gauges (read by the busy hold, and once at each first delta when the registry saw nobody else - the only way to see a
  peer this box does not know of); the stall reason prints it (`24000 tok / (400 tok/s / load 4) x 1.5 + 30s`).
- The busy hold's wedge bound follows the load only on an engine that cannot see a prefill (llama-server on `/slots`, a
  vLLM exposition with no KV-usage gauge); any other engine keeps ADR 0061's solo bound. The load that sizes it is the
  one the engine had when it last did work: a frozen engine whose queue keeps growing used to push its own verdict out
  forever and was never declared down.
- `prefill_tok_s` is folded in only from a run KNOWN to have had the seat to itself. A run nothing could answer for (no
  run registry, no engine gauges, an engine read still pending when it ended) is unknown, not solo, and an unopenable
  registry is logged. A run that waited out a seat going down teaches neither rate (its re-issued call waits out
  llama-swap's load).
- Cold loads: only a load the warm-up CONFIRMED is recorded (a failed start was recorded as a few-second "cold load" for
  every admission of the 18-minute outage). One load seen by several runs is one entry: an observation ending within
  ten seconds of ANY entry of the window of five merges into it and keeps the longest measurement, because runs report
  when they end, not when the load did; a load older than everything in a full window is dropped instead of pushing a
  newer one out (`seat-rates.json` gains `cold_load_ends`; a file without it still loads). A run that waited out a load
  it saw start in its pre-flight records the wait from the first sighting to ready (a lower bound of that load).

### Changed

- A wedged engine is now held for up to the cold-load ceiling (10 minutes by default) before it is filed, in case the
  seat restarts; the ceiling is a bound, not a promise (the 18-minute outage outlasted it, and the run then ends typed
  and is re-placed with the wait credited). On a shared seat whose engine cannot see a prefill a wedge is called later
  (for a 13,000-token prefill at 280 tok/s with five requests sharing the seat, about 380 s and 380 s instead of about
  100 s and 120 s); a death that breaks the stream is caught at once by the failed call.
- Operator-visible wording: a wedged engine now files `seat down: the seat's engine did no work ...` (was `stalled:`);
  `seat contended:` is a 429 only, a 5xx that outlives the budget is `seat not serving:`. Greps and dashboards keyed on
  the old text should count the new prefixes. `scripts/parallel-sessions-gate.ps1` now counts `seat not serving:` and
  `seat down:` and refuses PASS over them and over any other defer.
- Nodes without this change keep filing `stalled:` for the same outage, so the delegator half is inert until they are
  upgraded.

### Not in this release

The root cause of the engine hang; the seat launcher's crash cleanup (0.146.1; its deployment on a two-seat WSL box is
pending, register C-72); a seat-down recovery in the MCP `agent_run` door (it builds no liveness monitor);
`seat_recoveries` / `seat_down_wait_sec` on ledger rows (the wire and the delegator's result carry them); a load that
outlasts the admission budget is not recorded in the cold-load figure; a retry (verification or seat-down) still does
not check a plain, non-cordoning text reservation of the local cards (register C-81).

## [0.154.2] - 2026-09-30 - an opt-in browse setting activates the lane's own tab, so a dialog renders at once in a dedicated agent browser

### Added — `browse_activate_tab`, for a dedicated agent browser

The lane opens its tab with `Target.createTarget(background: true)`. That tab reports
`document.visibilityState` `"visible"` but is not the window's active tab, so Chromium produces almost no
frames for it, and 0.152.1 and 0.153.2 (the animation finish and the settle) work around that. Measured in the
operator's dedicated agent browser (a separate profile nobody looks at, attached through `browse_cdp_url`) on a
production web app's confirm dialog opened from a menu item, with nothing finished by the lane: the dialog
mounted at opacity 0 at +0.1 s; its opacity transition started at +0.2 s in one run but, in the lane's own
run, not within +0.33 s (the settle saw no DOM mutation and no animation and stopped at 0.3 s, and the read was
an empty page, because the modal hides everything else); left alone the dialog reached opacity 1 only at
+2.1 s. After `Target.activateTarget` for the lane's tab the same flow read opacity 0.957 at +0.2 s and 1 at
+0.4 s, with nothing finished, and the model's read after the menu click showed the whole dialog in every
instrumented run. End to end, deleting a record through the real UI (each run checked independently): 5 of 5
with the activation and a goal that describes the dialog's question and says the task is not done and not
blocked while it is open; about half with the older step-list wording, where the failures were the model
choosing BLOCKED with the dialog fully visible, not rendering; 0 of 3 on the MCP path with only the 0.153.2
settle.

- **New config key `browse_activate_tab`** (bool, default `false`, in `config.example.json`). The harness sends
  `activate_tab: true` in the sidecar's start line only when the key is true AND `browse_cdp_url` names an
  endpoint the lane accepts (`Config.EffectiveBrowseActivateTab`); otherwise it sends `false`. Activating a tab
  switches the window's active tab, and in the operator's everyday browser that is the tab someone is looking
  at, so the setting is opt-in and only meaningful with a dedicated endpoint.
- **A true key without `browse_cdp_url` is ignored, never an error.** The lane still runs exactly as before.
  The config load prints a warning naming both keys, and `offload_status remote` reports
  `browse_activate_tab` (the effective value, `false` there) with a `browse_activate_tab_note` that says why.
  `remote.browse_activate_tab` is new in the status block (the default golden gains that one field).
- **The sidecar** (`setup/browse/runner.py`): `Run` reads `activate_tab` from the start message (only a JSON
  `true` counts; absent, `false`, `null`, a string or a number is off). In the `Browser.observe` wrapper, at
  the first observe of the run and only then, before the settle and before jev's own read, it calls
  `Target.activateTarget` for the Browser's own target id through jev's browser-level `cdp()` (no session, so
  no other tab is touched) and logs `activated the lane's tab (activate_tab)`. The attempt is flagged before it
  is made and never retried; a failure, or a Browser without a tab id, logs `activate_tab skipped: ...` and the
  run continues exactly as before. The finish and the settle stay on; the measured runs had both in place.
- Tests: Go (`internal/config`: default off, JSON parse, effective only with an accepted `browse_cdp_url`, the
  ignored report and the warning; `internal/pipeline`: the start line carries `activate_tab` for all four
  combinations of opt-in and endpoint; `internal/mcpserver`: the status block, its note, the default golden) and
  Python (`setup/browse/test_runner.py`: exactly one activation at the first observe when on, never when off or
  absent or not a JSON `true`, a browser-level call with the lane's own target id, before the settle and jev's
  read, a failure swallowed and logged once and not retried, a missing tab id skipped). Each was shown to fail
  with its code path removed or broken (mutants run against a scratch copy or restored after each run: the
  once-per-run guard, the session argument, a constant target id, a narrowed `except`, the activation moved
  after the settle or after jev's read, a default-on start line, a truthy-value parse, a flag set only on
  success, both log lines, the no-tab-id guard; the effective predicate without the endpoint, without the
  loopback check, the ignored report, the load warning, the JSON tag, the start line's field, the raw key sent
  or reported, the status key and its note).
- **An activating run stops the lane's own daemon first** (`setup/browse/runner.py`, `main()`). jev's
  `ensure_daemon` reuses any live daemon of the lane's name and never compares it with the endpoint the run
  pins, so a lane daemon left by a crashed run, still attached to the operator's everyday browser, would have
  had its active tab switched by the activation. When `activate_tab` is true the sidecar now calls
  `stop_lane_daemon()` (the lane's own daemon name only; it changes nothing when none runs; a failure is logged and the
  run goes on) before the `Agent` starts a daemon on the pinned endpoint. A run that does not activate is
  unchanged. Pinned by tests that drive `main()` against fake jev and browser-harness modules: the stop comes
  before the `Agent`, only when the start line carries a JSON `true`, and a failed stop does not end the run.
- **The ignored note names the reason that applies.** A `browse_activate_tab` next to a `browse_cdp_url` that
  is set but refused (not loopback, no port) used to say it "needs browse_cdp_url", and the load warning ended
  "the lane runs as before" right beside the warning that `offload_browse` stays unregistered. The note is now
  `Config.BrowseActivateTabIgnoredNote()`: "set but refused" when an endpoint is set, "needs browse_cdp_url"
  when none is, and the warning promises "the lane runs as before" only for a registered lane.
- Tests added with those: the refused endpoint is reported as ignored (`BrowseActivateTabIgnored`), the note and
  the warning for a refused endpoint and a half-bound lane, the status note for a refused endpoint, and the
  activation on the attended door as well as the unattended one (the measured failure was on the attended MCP
  door). Each was shown to fail against a mutant: `Ignored` only when no endpoint is set, the note always
  saying "needs", the warning always promising the lane runs as before, the daemon stop removed, run for every
  run, or moved after the `Agent`, and the activation restricted to unattended runs.

### Docs

- `docs/systems/browse-lane.md`: the config row, the lifecycle step, the start-line row in the protocol table,
  invariant 10, the status and observability lines, and, under Background tab rendering, the measured
  activation timeline, the end-to-end counts and why the setting is opt-in and needs a dedicated endpoint
  (recommended for a dedicated agent browser). Common pitfalls: the goal-writing guidance for a
  menu-then-confirm flow now gives the better measured form (describe the dialog's question and state that
  the task is not done and not blocked while the dialog is open: 5 of 5, against about half for the step
  list). `docs/OPERATOR-GUIDE.md`: how to enable it.
- The fix is in the sidecar (`setup/browse/runner.py`) and the harness (config, start line, status); the
  setting does nothing until the sidecar is reinstalled with `setup/browse/install.ps1` and the key is set.
  An older `runner.py` ignores `activate_tab` without a log line, and `offload_status` cannot see which one is
  installed: `remote.browse_activate_tab` is what the harness will send, so the operator guide and the lane
  doc say to rerun the installer after upgrading and that no log line can also mean an older sidecar.

## [0.154.1] - 2026-09-30 - a fleet-serve restart no longer deletes the context of local runs in flight

### Fixed — a fleet-serve restart deleted the context of every local delegation in flight on its box

fleet-serve sweeps `pipeline-jobs/` once at every start, before it listens, and it removed every entry there on the
premise that it was the only writer in that root. It is not: the delegator's in-process local placement
(`RunAgentContract`, behind the MCP server's `agent_delegate`, `offload_research`, `offload_ask` and
`offload_review_diff` doors, the `delegate` and `research` commands and the fleet smoke) materializes each run's
`agent-local-*` directory, with the context documents the seat reads, in the same root, from processes that outlive
fleet-serve restarts. So every restart (each deploy, each crash-restart) deleted the documents of every local run in
flight on that box, and the seat answered "workspace root unavailable" or that it could not read the file, which failed
acceptance. Measured on 2026-09-30: 23 local runs spanned one fleet-serve restart, the node logged 23 swept directories,
and 6 of those runs lost their documents mid-run.

A local run now writes an owner marker (`.owner`, its process id and a newline) into its job directory, beside
`context/` and outside the seat's read root, before it writes any document. The startup sweep decides per entry: a
marked directory is removed only when its owner has exited or the directory has outlived any run (24 hours, against a
longest local run of about 4.3 hours by the code's own caps); an unmarked `agent-local-*` directory, which a delegator
binary older than this fix still writes, is kept until that same age; fleet-serve's own directories are removed as
before. An entry it cannot inspect is kept and reported, never counted as a live run; a directory removed for its age
while its owner lives is logged by name; every removal failure is reported, not only the first; and the start-up log
states what was swept, what was kept and what failed as separate lines. A local run whose job directory cannot be removed
at its end logs it. A pipeline `job_spec.id` may no longer start with `agent-local-` (refused at the ack), so no
fleet-serve directory can fall under the owner rule. The marker assumes, as the machine-wide GPU lease and the activity
registry already do, that the delegator and fleet-serve share one process-id space per base directory.

## [0.154.0] - 2026-09-30 - a seat that cannot constrain decoding runs classify and extract from the prompt, and a fleet text lane ships dark

### Added — unconstrained seats, the `text` fleet lane and `route` on `offload_classify` / `offload_extract` (ADR 0069, amends ADR 0062)

The RKLLM runtime on an RK3588 NPU cannot constrain decoding: it answers HTTP 400 `constrained_decoding_unsupported` to any `grammar`, `json_schema` or `response_format`, and ignores logprobs. Every structured cascade lane always sends a constraint, so on that seat every classify, extract, summarize and triage call deferred (`model call failed: ... 400 ...`, err_class `other`) before the model produced a token (measured 2026-09-30). The node also had no fleet door for text work. A blind check on the 2B seat with a prompt and no grammar scored classify 4/4, extract 4/4, summarize 0/4, triage 0/4.

- **`unconstrained_seats` node config key** (`config.Config.UnconstrainedSeats`, `DeclaresUnconstrainedSeat`, case-insensitive): written by `mediaseat.Bindings` from every `rkllm` media seat (its name and its aliases), never by `config_seed` (`tierseed` treats it as a bound key; `audit-config` compares it as a seed-owned key). The rockchip-rk3588 tier now binds it, so the node's own pipeline works on its seat.
- **No constraint, the shape in the prompt, strict acceptance.** For a declared seat `Pipeline.attempt` (and the terminal reasoning attempt) send no `grammar`, no `json_schema` / `structured_outputs` and no logprobs request. `tasks.BuildFor(req, Caps{Unconstrained: true})` / `Built.ForUnconstrained` append to the system prompt the exact JSON shape (keys in order, each type, an enum's allowed values, "no other key", one compact example) and set `Built.Strict`, the schema the grammar would have forced (`gbnf.JSONSchema`). The reply is parsed leniently (code fences and leading prose stripped) and accepted only if it satisfies that schema, on top of extract's own: a foreign object (`{"foo":1}`, `{"summary":"x"}`), a label outside the set, a missing key, a wrong type and an extra key are all refused. A failure takes the existing correction retry (counted in `meta.retries`), then defers with the validator's words naming what failed. Grounding still applies to extract and classify's self-reported confidence gate is unchanged. **The decision-margin gate needs logprobs and is inert on these seats**: no margin is recorded and nothing escalates on it.
- **Every other seat is byte-identical.** `tasks.Build` is unchanged and the branch is taken only for a declared seat, pinned by a golden digest of the grammar-seat prompts (taken from the pre-change `Build`), a test that a grammar seat's request body carries exactly `Build`'s system, user and grammar while another seat on the box is unconstrained, and a zero-`Caps` equality test.
- **The ledger row carries the prompt that was sent.** `meta.prompt_prefix_sha256` is stamped in `Run` from the grammar-bearing build before a rung is chosen; for an unconstrained seat `attempt` restamps it from the prompt with the shape instruction, so a row never reports the fingerprint of a prompt that was not sent (a measurement through this pipeline joins on it).
- **Review fixes (same release).** (1) The reply of an unconstrained seat is read by `parser.ExtractOne`, not `Extract`: exactly ONE top-level JSON object (a top-level array, a second `{`/`[` after the object, and a repeated key at any depth are refused and take the correction retry, then a defer; leading prose, a fence and trailing prose without a bracket stay accepted; grammar seats keep `Extract` byte for byte). (2) A model declared in both `vllm_seats` and `unconstrained_seats` is pinned to be unconstrained (no `json_schema`, `structured_outputs` or grammar). (3) On an unconstrained seat classify's `confidence` is bounded 0..1 in the strict schema (7, -5 and 1.0001 are refused; the prompt example stays 0.5, below the 0.88 accept threshold); `gbnf.JSONSchema` and every grammar seat are unchanged. (4) `textremote` post-checks a node's OK result (classify: label in the request's set, confidence in 0..1; extract: one object with keys inside the schema's properties) and defers naming the node and the reason on failure. (5) ADR 0069, FLEET-NODE.md and the tier note state that declaring a text task only means something if the node's cascade routes that task to the unconstrained seat, measured through that path. Left as is: the ledger fingerprint of a repacked climbed tier (`promptFingerprintSent`) differs from Run's entry stamp only where Run itself fell back to the whole user turn; not a one-line fix.
- **Two seats may not both declare text tasks** (`mediaseat.Validate`: `text_tasks` is one node key, like `vision_model`).
- **summarize and triage** on an unconstrained seat run the same validated path for a local cascade call (they used to defer as an infrastructure error and now validate, then climb on a quality failure) and are never admitted on the fleet lane.
- **`text_tasks` node config key and the `POST /fleet/text` lane** (`internal/fleetnode/text_task.go`, task type `text`): a media seat's `tasks` may now name `classify` and `extract` (a text task on a non-rkllm seat is refused at validation), `mediaseat.Bindings` writes their canonical-order subset as `text_tasks`, and the node runs ONE classify or extract on its OWN pipeline for a delegator: token-gated on dispatch and poll like the vision lane, dispatch's 1 MiB body, the shared `admit` path, the node's full `core.Result` as the job's data (defers included), concurrency-capped. `TextLaneAdmissible` (a non-empty `text_tasks` and the agent lane's reachability rule) is the one predicate behind the advertisement (`text` in `supported_task_types`, the additive omitempty health field `text_tasks`) and the ack-time admission. A task outside `text_tasks` is a `400` naming the set; summarize and triage are refused whatever `text_tasks` says.
- **The lane ships DARK.** The rockchip-rk3588 seat declares no text task, so no shipped tier advertises the lane and `text_tasks` is empty everywhere. A later data-only change adds `classify` and `extract` to the seat's `tasks` once at least 30 cases per lane through the node's own pipeline score at least 90 % correct with zero off-schema outputs accepted (`TestRK3588TextDoorShipsDark` pins the dark state). The route default stays local, idle-local-wins and "never a downgrade" stand.
- **`route` on `offload_classify` and `offload_extract`** (`local` default, `auto`, `remote`; `internal/textremote`, `visionremote`'s shape, Budget 300 s). `local` is byte-identical to before. `auto` leaves the box only while the machine-wide GPU lease is held, else or with no eligible node it runs local. `remote` forces a node and defers (`capacity`, or `config` with no `delegate_remotes`) when none is eligible, naming every miss. `delegate.PlaceText` picks the node: its health must list `text` AND the task in `text_tasks` and its card must not be leased, so a node that predates the lane is never picked (unlike the vision lane, an absent list is "none", not "all"). `meta.node` and `meta.placement` say where it ran. summarize and triage take no `route`.
- **`tools/list` changes on EVERY box** (deliberate): `offload_classify` and `offload_extract` gain an optional `route` property. The tests that pin it are updated on purpose: `TestTextToolsAdvertiseTheRoute` (route present on the two tools, absent on summarize and triage, never required) and the new `TestTextDefaultRouteRunsLocalUnstamped`. Callers that omit `route` are unchanged.
- **Docs and generated pages:** ADR 0069 (and a row in the ADR index), an ADR 0062 "Amendment (0.154.0)", `docs/systems/fleet-node.md`, `docs/FLEET-NODE.md` (new "The text task" section and lane-table row), `docs/systems/offload-pipeline.md`, `docs/systems/mcp-server.md`, `docs/systems/accelerators.md`, `docs/OPERATOR-GUIDE.md`; `docs/tiers/rockchip-rk3588.md` and `config.example.json` are regenerated (only that tier page changes).
- **Upgrading an installed RK3588 node:** `install.sh` leaves an existing `config.json` untouched, so add `"unconstrained_seats": ["<the seat's model id>"]` by hand (or regenerate the file); `local-offload audit-config` lists it as SEED-ONLY until then. Nothing else changes for a node that declares no text task.
- **Not done here:** `setup/install.ps1`'s `Get-MediaSeatBindings` (the PowerShell mirror of `mediaseat.Bindings`) still does not know the `rkllm` kind and writes neither new key; the RK3588 tier is Linux only. The margin gate's absence on these seats means a valid-but-wrong label is not caught by anything but classify's self-reported confidence: that is what the measured gate for opening the lane is for.

## [0.153.2] - 2026-09-30 - the browse lane waits for the page to settle after an action, so a dialog that mounts late is seen

### Fixed — a confirm dialog opened from a menu item was read before it existed

0.152.1 finished the pending CSS animations before every page read, and a dropdown menu then showed up. A
confirm dialog opened from a menu item still did not: the run ended `blocked` ("the model or the loop
reported no progress") three times out of three on a production web app in the operator's browser. jev reads
the page about 50 ms after an input, and at that moment the dialog is either not mounted yet or mounted at
opacity 0 with its open-state style change not applied yet, because the page applies it on its own timer. The
finish had nothing to finish (the animation did not exist yet), so the snapshot was the page as it was before
the click; 1.5 s later one finish made the dialog's confirm button visible (measured).

- **The observe that follows an action now waits for the page to settle** (`settle_after_input`, new in
  `setup/browse/runner.py`). Every 100 ms it finishes newly started animations (the 0.152.1 script) and reads
  a counter of DOM mutations that a `MutationObserver` keeps in the page; a poll is quiet when no animation was
  finished and the counter did not move. It stops after two quiet polls in a row once 0.3 s have passed, and at
  1.5 s of page time whatever the page does (plus the calls in flight, see below). Measured with it: the read after the menu click showed
  exactly the dialog (its text, `Cancel` and the confirm button), and once the goal named the confirm click as
  a step of its own the run clicked it and the record was deleted, checked independently. Launching the browser
  with its background-throttling and occlusion-detection switches off did not remove the need (measured: still
  `blocked` without the settle).
- **Cost and bounds.** One settle per action, before jev's own wait and read: about 0.3 s on a quiet page on
  top of jev's wait (plus two CDP calls per poll, more on a slow daemon), and the full 1.5 s on a page that
  never stops changing (a live ticker, a timer that rewrites the DOM). It does not run before the first observe, after a `wait` action, before an action, or again
  for the retries of one read. A page that is completely silent for its first 0.3 s counts as settled, so a
  dialog whose first DOM change comes later is picked up by the following observe, as before. The cap is
  checked between polls, so it bounds the page time and not the calls in flight: the harness can hold one CDP
  call for several seconds on a page whose JS thread is frozen (a native `confirm()` or `alert()` open), and a
  poll makes two; jev's own read fails the same way right after, and the run's own timeout still bounds it.
- **A capture run drains between the polls.** The daemon's shared event buffer holds 500 events and drops the
  oldest, and the settle starts right after the input, when the action's own requests fire; `settle_after_input`
  takes a `between` hook that the wrapper points at the capture drain (a no-op without `capture`), so the
  undrained window stays what it was before the settle existed. A hook that fails is logged and does not end
  the settle.
- **It never ends a run.** The counter is read as an observer id plus a count, so a navigation (a new document
  has a new observer) reads as a change even when the count matches. An unreadable page (a navigation in
  progress, a dead session, an IPC timeout) also counts as a change and is polled until the cap; the read that
  follows reports a real error. The calls go straight to jev's `cdp()` in the observed session, like the
  finish, so none of the run's bookkeeping is touched. The runner logs `settled N.Ns after an input (...)` to
  stderr when the page was active.
- **Finding, documented for goal writers: name the confirm click as its own step.** When a menu item and the
  confirm button of the dialog it opens carry the same label, the decision model reads the second click as a
  repeat of the step it just took and chooses BLOCKED; the run ended `blocked` until the goal named the confirm
  click as a separate required step (measured on one menu and dialog in one app). `docs/systems/browse-lane.md`
  (Common pitfalls) has the wording. This is guidance, not code.
- Tests (`setup/browse/test_runner.py`): the settle is driven with a scripted page, a fake clock and a fake
  sleep (stops after two quiet polls once the minimum has passed, runs at least the minimum on a quiet page, a
  burst holds it, never past the cap on a page that keeps changing, finishes animations on every poll, an
  unreadable page or a navigation counts as a change, swallows every exception); the counter script runs
  under `node` against a fake `MutationObserver`; the wrapper tests pin that it runs once per observe after an
  action of any kind (click, fill, select, scroll), before jev's wait and read, in the observed session, and
  never on the first observe (whose fake Browser, like jev's, has no `after_input` until its first act), after
  a `wait` action or in act. They also pin that the counter is read by value through `Runtime.evaluate`, that
  the production defaults use the real clock and sleep, and that a capture run drains between the polls and
  a run without capture drains nothing. Each was shown to fail with its code path removed, broken or dropped
  (mutants run in a scratch copy: a dropped or altered `returnByValue`, a constant clock, a no-op sleep, a
  direct `after_input` read, a settle for clicks only, no drain hook, a hook that is never called, called
  after the reads, or only on the first poll, and a hook whose failure ends the settle).
- **The settle's finishes are quiet.** On a navigating page every poll's finish fails, and each failure used
  to log `finish animations skipped`, several lines per settle that crowd the stderr tail a defer reports.
  `finish_animations(send, quiet=True)` drops that line for the settle; observe's own finish still logs it.
- **Neutral fixtures.** Site names from earlier releases' examples, comments and test fixtures
  (`setup/browse`, `internal/pipeline/browse_test.go`, this file's 0.142.1 entry) are replaced with
  neutral ones; behaviour is unchanged.
- The fix is in the sidecar (`setup/browse/runner.py`; the Go binary changes only its version string);
  reinstall with `setup/browse/install.ps1` to take it.

## [0.153.1] - 2026-09-30 - grounding compares numbers by value: a correct amount in either locale is grounded, a substring of another number is not

### Fixed — a correct number was judged ungrounded when the source wrote it with separators, and a wrong one was grounded by a substring

- `internal/grounding` compared a JSON number as TEXT. It rendered 2354.4 and looked for that string among the source's numbers with commas deleted (`2.35440`, `2354.40`, `18550`), so "Total: 2.354,40 EUR", "Total: 2,354.40 USD" and "Importe: 185,50 EUR" all came back UNGROUNDED for the right value. The pipeline treats an ungrounded extract as a retry and then a defer, so a perfect model deferred on every such amount, and `eval.Grade`, which grades extract by grounding alone, scored the same answers wrong. On a bilingual 48-case extract corpus 16 gold answers were judged ungrounded; all 48 are grounded now (measured 2026-09-30, the corpus is kept out of the repository).
- The opposite failure: the verbatim shortcut accepted a number whose digits occurred inside another number, so `{"amount": 0}` was grounded by "Amount: 4200 USD" and `{"w": 7}` by "7.5".
- Numbers are now compared as values. Each numeric token of the source is read as every value it can denote: a plain integer; English grouping (`2,354.40`); Spanish grouping (`2.354,40`); one separator followed by exactly three digits (`1,234`) as both 1234 and 1.234; one separator followed by one, two or four-plus digits as a decimal mark; English commas also as Indian lakh/crore grouping (`1,23,456`). A token that is no number in either locale (a date `18.06.2026`, a list `3,4,5`) reads as its digit groups, and a separated token is never read as its digits joined (`2.354,40` does not denote 235440). A trailing `.` or `,` is sentence punctuation. Values are compared exactly, with no tolerance (a tolerance made two different 13-digit ids, epoch timestamps or phone numbers equal). A plain-digit string token of 16 or more digits is an identifier and is compared digit for digit, because a float64 cannot tell 19-digit ids apart; a JSON number of 17 or more digits is a float64 and inherits that limit. A two-part token (`3.1`, `9.30`, `5,10`) is a decimal only, so `{"major":3,"minor":1}` is not grounded by "Release 3.1" (precision over recall, the same rule as 7 against 7.5).
- A JSON number leaf is grounded iff its value is one of the source's, by magnitude (a sign is not checked, as before). The substring shortcut no longer applies to numbers. A string leaf keeps the shortcut; one that is not a phrase of the source is grounded when every number in it is a value the source writes, every identifier-like word in it (a digit or hyphen) is in the source, and a zero-padded integer (`007`) appears as written, so `"2,354.40"` grounds against "2.354,40". Summaries (`Check` for summarize) and `CheckFields` use the same rule. Booleans, empty strings (absence) and every non-numeric verdict are unchanged, and no signature changed.
- **Extract eval scores are not comparable across this release.** `eval.Grade` grades extract (and summarize) by grounding alone, so the same answers score differently before and after 0.153.1: correct locale-formatted amounts now pass, and wrong numbers that matched as a substring now fail. Compare extract/summarize eval results only within one side of this change.
- Tests: `internal/grounding/numbers_test.go` pins each case both ways (locale grouping, decimal comma, ambiguous three-digit groups, integers, 0 against 4200, absent and distractor numbers, string leaves, currency and percent tokens, trailing punctuation, dates and lists), the token parser directly, and a table of the verdicts the string implementation gave on non-locale input, which are unchanged. Not read as one number: space or apostrophe thousands (`1 234,56`, two tokens).

## [0.153.0] - 2026-09-30 - the RK3588 NPU seat serves the vision tasks its runtime can do, and says so

### Changed — the RKLLM seat starts with a repeat penalty, and the vision lane is limited to the tasks the seat declares (ADR 0062 amendment)

Measured on the reference RK3588 board (2026-09-30): greedy decoding with the seat's default `repeat_penalty` 1.0 loops on VQA and runs to the 256-token cap (the vision lane defers "vision output truncated"); with `repeat_penalty` 1.1 a blind four-question VQA check scored 3/4. The seat also refuses constrained decoding (`grammar`, `json_schema`) with HTTP 400, so it can serve `vqa` and `ocr` and never `assess_image`, which always sends a grammar; the fleet vision lane admitted all three and the delegator placed them without looking at the task, so such a node would have taken an `assess_image` and deferred after a 400. The NPU also runs one generation at a time, while a node admits four jobs by default.

- **`--repeat-penalty` on `rkllm_server.py`** (float, default 1.0, refused at startup outside 0.01..10, the range a request's value has). A request that sends neither `repeat_penalty` nor `repetition_penalty` gets the seat's value; a value the request sends always wins. `rkllm-serve.sh` passes it through with every other argument.
- **`repeat_penalty` on an `rkllm` media seat** (`mediaseat.Seat.RepeatPenalty`, rkllm only, 0.01..10), rendered as `--repeat-penalty <v>` between `--cpu-mask` and `--served-name` only when set, so every seat that names none renders byte for byte as before. It is the one sampling default an rkllm seat may carry: `temp`, `top_p` and `top_k` stay refused. The `rockchip-rk3588` seat sets 1.1.
- **`tasks` on a media seat** (`mediaseat.Seat.Tasks`, vision and rkllm seats): the task names the seat serves, from `vqa`, `ocr`, `assess_image`, `classify`, `extract`. `classify` and `extract` are validated now for the text door that will read them; only the vision subset is bound in this release. A vision task on a seat that cannot read an image, a declaration naming no vision task on a seat that binds `vision_model`, an unknown or repeated name, and `tasks` on an stt or ocr seat are refused when the tier table is validated. The `rockchip-rk3588` seat declares `["vqa", "ocr"]`.
- **`vision_tasks` node config key** (`config.Config.VisionTasks`): the vision subset of the seat's `tasks`, in canonical order (`vqa`, `ocr`, `assess_image`), written by `mediaseat.Bindings` beside `vision_model` and refused in a `config_seed` like it (`tierseed` treats it as a bound key; `audit-config` compares it as a seed-owned key). Empty or absent = all three, which is every other tier's behaviour.
- **The node enforces it, publishes it, and the delegator places by it.** `POST /fleet/vision` refuses a task outside `vision_tasks` at ack time with a `400` naming the allowed set (`vision: task "assess_image" is not served by this node's vision seat (vision_tasks: vqa, ocr)`), and `/fleet/health` gains an additive, omitempty `vision_tasks` beside `vision_model`. `delegate.NodeView` decodes it (absent = all three, so a node that predates the field is exactly as eligible as before) and `NodeView.ServesVisionTask` is the one predicate; `delegate.PlaceVision` now takes the task (`PlaceVision(remotes, task)`) and skips a node that does not list it, and `visionremote` passes the request's task and names the miss when no node serves it.
- **Serving limits seeded for a one-generation NPU** (`rockchip-rk3588` `config_seed`): `fleet_max_concurrent_jobs` 1 (the node default is 4, which would queue three jobs behind the NPU's single generation), `request_timeout_sec` 240 (below the delegator's 300 s fleet vision budget, `visionremote.Budget`; the cold load measured 13 s), `max_input_chars` 8000, `ocr_max_tokens` 512. `fleet_max_queue_depth` is left at its default, twice the concurrency, which is 2 here (one running, one waiting), inside the NPU server's own window of one running and two waiting.
- **Docs and generated pages:** ADR 0062 gains an "Amendment (0.153.0)"; `docs/systems/fleet-node.md`, `docs/FLEET-NODE.md` and `docs/systems/accelerators.md` describe the new key, the health field, the placement rule and the seat default; `docs/tiers/rockchip-rk3588.md` and `config.example.json` are regenerated.
- **Upgrade of an installed RK3588 node: refresh `RKNPU_HOME` first, then re-render, and reseed the config by hand.** The binary does not ship `rkllm_server.py`; it lives in the `accelerators/rknpu` copy under `RKNPU_HOME`, which `install.sh` never refreshes, and `rkllm-serve.sh` execs its arguments straight into it. A 0.153.0 render emits `--repeat-penalty 1.1`, which a 0.151.1 server refuses (`unrecognized arguments`, exit 2), so the seat never starts. Copy or check out the new `accelerators/rknpu` (at least `rkllm_server.py`) into `RKNPU_HOME` before re-rendering `llama-swap.yaml`. `install.sh` also leaves an existing `config.json` untouched, so `vision_tasks` and the four seeded limits do not reach it: add `"vision_tasks": ["vqa","ocr"]`, `"fleet_max_concurrent_jobs": 1`, `"request_timeout_sec": 240`, `"max_input_chars": 8000` and `"ocr_max_tokens": 512` by hand, or delete the file to regenerate it (`local-offload audit-config` lists them as SEED-ONLY). Until then the node keeps accepting `assess_image` and deferring after the 400.
- **Not done here:** `setup/install.ps1`'s `Get-MediaSeatBindings` (the PowerShell mirror of `mediaseat.Bindings`) still does not know the `rkllm` kind and so does not write `vision_tasks`; the RK3588 tier is Linux only and the mirror already warns on the kind.

## [0.152.1] - 2026-09-30 - the browse lane un-sticks fade-in dialogs and menus in its background tab

### Fixed — a dialog or dropdown menu that fades in was invisible to a browse run

A run on a site whose dropdown menu fades in could not use the menu: the lane's tab is opened in the
background, a hidden tab produces no rendering frames, and a CSS `@keyframes` animation or `transition`
that starts there never advances, so the menu kept its start-state opacity 0. The page snapshot drops
every element whose computed opacity is 0, so the model never saw the menu it had just opened and the
run ended `blocked` ("the model or the loop reported no progress"). The sidecar now jumps the finite
animations in `document.getAnimations()` that the snapshot cannot see (hidden by opacity or `visibility`,
empty, or outside the viewport) to their end state, immediately before every page read: each read attempt,
so also the read after jev's short post-action wait and the retries after a stale read or a navigation.
Infinite ones, such as spinners, are skipped, and so is an animation on an element that is already visible
and on screen (a toast that a CSS animation fades out after a delay stays in the snapshot); the call
swallows every error, so a navigating page never becomes a failed run.
Measured in a background tab with a dialog opened by a real mouse click and read 400 ms later: a
`@keyframes` fade and a `transition` fade both read opacity 0 and not visible until finished, then
opacity 1 and visible; a production web app's dropdown menu showed 0 of 7 items visible and 7 of 7 after.
A fade driven by `requestAnimationFrame` was already visible to the snapshot (opacity 0.0055 at 400 ms,
above 0; whether it reaches 1 was not measured) and is not targeted. The finish runs before page reads
only, never before an action: jev's pre-click check compares the observed page and the target's guard
with the live page, and a finish in between could make it fail. Content the page mounts after the read
itself (a debounce or a network response that lands later) is finished at the next observe, and the `wait`
action triggers one. Forcing frames with `Page.captureScreenshot` is not an option (it hung for more than
15 s in the hidden tab). `date` and `datetime-local` inputs are still absent from the snapshot. The fix is
in the sidecar (`setup/browse/runner.py`; the Go binary changes only its version string); reinstall with
`setup/browse/install.ps1` to take it.

## [0.152.0] - 2026-09-30 - placement holds instead of sleeping or refusing (ADR 0063)

### Fixed - placement holds instead of sleeping or refusing (ADR 0063)

On 2026-09-29 one delegating session (648 of the day's 836 delegated attempts, a median of 21 and up to 37 open at once) fed about 13 execution slots, and every capacity signal at the delegator became "sleep or wait a bounded time, then refuse": 232 dispatch 503s cost about 16 caller-hours asleep on the nodes that had refused while others had room, 58 jobs were abandoned at a fixed five-minute queue deadline that the nodes then ran anyway (ghost jobs that held 38 to 96 percent of the slow nodes' wall), 26 of 52 jobs re-placed onto the local seat died in its line, and the same research page was issued up to 24 times. Ten changes in `internal/delegate`, one arithmetic behind them:

- **A dispatch 503 returns at once.** `runRemote` no longer sleeps the node's `Retry-After` (up to the 300 s the node clamps it to), retries that node and charges the sleep to the contract. The subtask is re-placed on a node with room; the hint becomes a per-node cooldown (jittered once, never longer than the delegator's own 300 s queue ceiling - the excess of the jitter is folded back under it - and compared as a number, never against the node's wording) that only the capacity wait consumes and credits. A wait that ends with a node still cooling down names it in its defer.
- **Re-placement reads the fleet again** for every route, `route=spread` included (a snapshot older than the refusal is never reused), and asks only nodes that are eligible, have room, can start the job inside the caller's patience, are not cooling down and have a free process-gate slot, all judged against the wall the re-placement will carry. The local seat is a candidate only when its run-cap line has a free slot ahead of a newcomer. With no candidate the subtask goes to the capacity wait **whatever kind of refusal started the chain**: a node held out only for being busy and a full local run-cap line are places in line, so a 500 or a 404 followed by a node that is full for a moment no longer ends as `placement refused` (a node the contract can never run on is not worth waiting for). Each re-placement reads the fleet once; siblings that refuse together each read it themselves.
- **A local-leg capacity defer is re-placeable** (class capacity, zero steps; the wait it spent in the seat's line is credited). `route=local` waits in place, a defer a remote files after its 202 never moves, and with nothing else free the defer is published as a defer, never as `placement refused`. The defer alone never holds the subtask: only an eligible node that is merely busy is worth waiting for.
- **The queue budget follows the node's ETA.** How long a job may sit queued is `clamp(1.5 x etaStart + 30 s, 60 s, the caller's patience)` from the node's own `queue_wait_estimate_sec` (or its jobs and recent wall), read again when the job is first seen queued (a read that fails is logged, tried again on the next queued poll up to three times, and named in the queue-deadline message if it never succeeded); a node that publishes no ETA, or an impossible negative one, keeps `min(patience, 5 min)`.
- **The backlog gate.** A node whose ETA to start a job exceeds the caller's patience is held out of the deal, the capacity wait and re-placement: a placement feasibility refusal that prints its arithmetic (`backlog (a new job would wait ~444 s to start (1 running + 0 queued - 1 worker(s) + 1 = 1 ahead x 443.6 s recent wall / 1 worker(s)), past the 360 s this contract will wait for a start)`, a 300 s wall plus the 60 s grace), never a speed preference, re-read every tick.
- **The spread deal counts capacity** - never more subtasks per node per run than `max_concurrent_jobs - jobs_running`, and the **local seat is counted too** (`fleet_max_concurrent_jobs` minus the runs already registered on it, only while some remote could run the contract); fit order unchanged. The overflow goes to the capacity wait as what it is: neither a lease (it takes the local seat only once the seat stops reading busy by the deal's own reading, and with the wait off it is a capacity defer, not an infrastructure defer naming a lease nobody holds) nor a refusal (not counted as a replacement). The deal names every node it passed over with its arithmetic, and a remote that failed its health probe keeps the run flagged. A **process-wide gate** keeps concurrent Runs inside each node's `max_queue_depth` (a turn-away is no refusal), and a **per-page retry cap** backs a research page off for 15 minutes after three issues that a seat ran and failed to produce a verified digest (a full node, a lease, a bad token, a cancel or a queue deadline never counts against a page).
- **Research digests are routed by contract shape** (register C-76): a research-door contract with one context page and a schema is mechanical whatever its goal says (`architecture`, `why`, `how`, `compare`, `trace` no longer send it to the roomiest seat first), and **the second chance for a failed digest queues for a busy seat** instead of being skipped - never on the seat whose answer it corrects, a caller's cancel is reported as a cancel, and a failed fresh health read is named and logged.

Visible surface changes: the Retry-After courtesy note is gone from `placement_reason`; `backlog` joins the placement verdict vocabulary; a capacity defer gains `held out by the backlog gate` and `not asked again inside the wait ... cooling down after its own refusal` clauses and, for a gate turn-away or a deal's overflow, `waiting because: ...`; a `route=spread` overflow reason says `already dealt to its headroom` and names the local seat's spent run cap; the queue-deadline failure prints the derived budget (and that it was never re-checked when the refresh failed); `retry_note` says how long a retry waited in line or that the caller canceled; research digests show `fit=mechanical/research-digest`. No new config keys, no wire change.

Not in this release: the whole-call deadline and the capacity wait running to it (`agent_placement_wait_sec` still bounds the wait, 120 s by default), node-side withdraw of never-started jobs and atomic admission, seat-side queue limits, and the placement rate floor.

### Fixed during the merge with 0.146.0-0.151.1 — the placement changes compose with the withdraw route and the rescue

- The queue deadline's timing is ADR 0063's (a budget derived from the node's ETA, refreshed when the job first sits queued) and
  what happens there is ADR 0064's (one withdraw request; a confirmed withdrawal is filed as a capacity refusal, re-placed with
  the queued wait credited exactly once; a 409 "running" keeps polling). ADR 0063 decision 3 names the one job re-placed after a
  202: one the node says it never ran.
- The process-gate slot is released when the node's part of a subtask is over, before the delegator's own rescue of a finished
  answer (a rescue could otherwise hold a slot for minutes and keep a sibling out of an idle node).

## [0.151.1] - 2026-09-30 - the Coral feeds EfficientNet-EdgeTPU-S a correctly quantised input

### Fixed — the Coral fed EfficientNet-EdgeTPU-S raw pixels (classify imagenet and embed)

- `accelerators/coral/server.py::_set_input` passed uint8 pixels straight to every uint8 input. EfficientNet-EdgeTPU-S quantises its
  input with scale 0.012566 and zero point 131, so it saw a 1.6x contrast-stretched image: 41.6 % top-1 on 1000 ImageNetV2 images, against
  63.9 % once the input is rescaled with pycoral's rule `q = (px - 128) / (128 * scale) + zp` (the same as its CPU twin; measured
  2026-09-30). The rescale applies only where the quantisation is not the raw pixel to within one step, so the iNat classifiers,
  DeepLab and EfficientDet-Lite get exactly the bytes they got before. The `embed` tool used the same broken input: its vectors from
  before this fix are not comparable with new ones (the harness stores none; a caller that kept some must re-embed).

## [0.151.0] - 2026-09-30 - the RKNPU sidecar serves permissively licensed models, measured on the NPU

### Changed — the RKNPU sidecar serves permissively licensed models: PP-YOLOE+ s and ResNet-50 replace YOLOv8n and ResNet18

Operator order (2026-09-30): no licence costs, so no AGPL or GPL model ships. The Ultralytics YOLOv8n (AGPL-3.0) and the Rockchip ResNet18 prebuilt (untraced provenance) are removed from `accelerators/rknpu/models.json`; `object_detect` now runs PP-YOLOE+ s (PaddleDetection, Apache-2.0; 43.7 COCO mAP claimed upstream, host FP32 0.436 on the 500-image evaluation list) and `classify` runs ResNet-50 with torchvision's `IMAGENET1K_V2` weights via timm `resnet50.tv2_in1k` (BSD-3-Clause; 80.9 % ImageNet-val top-1 claimed upstream, host FP32 69.9 % top-1 on 1000 ImageNetV2 images). CLIP is unchanged. The manifest gains `ppyoloe_s`, `resnet50tv2-i8` and `resnet50tv2-fp16` and loses `resnet18` and `yolov8n`; `CLASSIFY_MODEL` in `server.py` names the served classifier build (INT8; the FP16 build is one constant away once the board has measured both).

- **No `.rknn` is downloaded any more.** Every model is built by `fetch-models.sh --convert` from the recipe under its `convert` key (a converted file's `sha256` pins the exact bytes built; a rebuild is not bit-reproducible, and the ResNet INT8 pin was calibrated on 200 ImageNetV2 images while its recipe names COCO, recorded in `calibrated_on`). The two ResNet-50 files use a new `export` recipe: the timm weights are downloaded at a pinned Hugging Face revision and sha256-checked, `torch.onnx.export` writes the ONNX at opset 12 (TorchScript exporter; `dynamo=False` wherever torch has the argument), and the exported ONNX itself has no hash. Verified 2026-09-30 in a scratch venv built from the documented install line (torch 2.4.0 CPU, timm 1.0.30): the export's outputs equal those of the ONNX exported earlier with torch 2.14 (maximum absolute difference 0.0 on three random inputs). The documented convert-venv line now adds `torchvision`, `timm` and `safetensors`.
- **Calibration list in the repository.** A dataset may name `list_file` (relative to the manifest) instead of `list` (a URL): `accelerators/rknpu/calib/coco_val2017_calib_200.txt` holds the 200 COCO val2017 file names, fetched from `images.cocodataset.org` over plain http (its https certificate does not match its name). Entries of a `list_file` pass the same absolute-path and `..` refusal as a downloaded list. `coco_subset_20` and the unused `space_shuttle_224.jpg` are removed; the COCO label list and `bus.jpg` now come from the zoo's ppyoloe example (byte-identical files, same sha256).
- **Decoder.** The DFL decode reads the bin count from the box tensor (PP-YOLOE: 68 channels, 17 bins; a 64-channel YOLOv8-shaped head still decodes), the three stride branches may come in any order, and the score-sum branch is ignored, as the zoo's Python demo does. The score is the best class probability with no box-confidence factor. Defaults stay the zoo demo's: score 0.25, NMS IoU 0.45. A head the decoder cannot read (box channels not a multiple of 4, channel counts that differ between branches, a three-tensor YOLOX-style head, or a grid that disagrees between box and class) is the structured `unexpected detector output layout` refusal instead of a 500.
- **Classifier preprocessing.** A spec carrying `resize_short` and `crop` gets the torchvision evaluation recipe (short side to 232, bilinear, centred 224 crop, cut from the source and resampled once, so an extreme aspect ratio builds no huge intermediate); a spec without them is squashed to the input size as before.
- **A missing label file no longer says to convert.** The `model_missing` hint for a label file is `run fetch-models.sh`; only the model file itself says `--convert`.
- **Not yet run on the NPU.** The three new files were converted and checked on rknn-toolkit2's simulator; `test_server.py` decodes the simulator's own tensors when `RKNPU_SIM_DIR` names the benchmark's `sim/` folder (PP-YOLOE on `bus.jpg`: person 0.950 / 0.935 / 0.923, bus 0.893, person 0.473, handbag 0.411; ResNet-50 on the zoo's dog photo: Shih-Tzu in both builds). Latency, memory and INT8 accuracy on the board are unmeasured; `docs/systems/accelerators.md` records the host FP32 baselines the board's INT8 figures are to be checked against. A box that already runs the sidecar needs the three new `.rknn` files copied to its models directory (`fetch-models.sh` reports them MISSING until then); the old `resnet18` and `yolov8n` files are no longer served.
- **Measured on the NPU (`<node-d>`, 2026-09-30), same 500 COCO / 1000 ImageNetV2 images as the host baselines:** ppyoloe_s INT8 mAP@[.5:.95] 0.4255 (host FP32 0.4360; 56 ms per inference on one NPU core); resnet50tv2-i8 top-1 69.7 % (FP32 69.9; 13 ms), resnet50tv2-fp16 69.9 % (30 ms), so the INT8 build is served. The Coral Edge TPU on the same COCO list: EfficientDet-Lite0/1/2 0.270 / 0.313 / 0.356. Evidence: the operator's benchmark records (outside this repository).

## [0.150.0] - 2026-09-30 - a read-only catalog of the ComfyUI workflow templates a node carries (Comfy templates phase A)

### Added — a read-only catalog of the ComfyUI workflow templates a node carries (phase A of the Comfy templates work)

`render/templates-catalog.mjs` lists and classifies the workflow templates of the `comfyui-workflow-templates` package that ComfyUI pins on a node, of a checkout of the upstream repository, or of a `pip download` extract of a candidate version. It is one dependency-free file (`node:` builtins only) that never runs a template, downloads a model, installs a package, changes a config or opens the network; the only file it writes is the one named by `--out`, and a test fails if that stops being true. The whole file can be piped to `node --input-type=module - snapshot --comfy-dir <tree>` on a node with nothing deployed; a piped file has no licence map beside it, so its other verbs count every repo as unknown unless they are given `--license-map FILE`. There is still no MCP tool, config key, fleet task or route for it: a template is a UI-format graph and `run-graph` takes API-format graphs only, so running one waits for a converter. Per template it derives the active nodes (subgraphs are expanded; muted and bypassed nodes, and everything inside a bypassed instance, are inactive), the model files the active nodes need (class, URL and hash from `properties.models`, plus loader widget files nobody annotated; files that only bypassed nodes reference are reported apart), the parameter surface (`proxyWidgets`), whether it is a paid API template (a node type in the node's own `comfy_api_nodes` ids, read from `node_id="X"`, a `NODE_ID = "X"` class attribute and the first argument of `_cloud_schema`; the `api_` name prefix; or the index's `openSource: false`; any one is enough, and the summary counts where they disagree) and a version stamp whose label goes on every count. Against the installed package (comfyui-workflow-templates 0.11.68, json 0.1.94) it reproduces the research figures: 566 entries, 326 API (the node-id scan and the index's `openSource: false` each see all 326 and the `api_` prefix 276; the ids-and-prefix view and the index flag describe the same set, 0 disagreements), 240 local, 235 of them needing weights and 5 needing none, 197 with subgraphs, 1129 model annotations of which 10 carry a hash; against the upstream HEAD e7849852: 660 entries, 367 API, 243 local, 50 needing custom nodes, 262 with subgraphs, 155 with a parameter surface (103 of them local). Trimmed real templates (MIT, attributed in `NOTICE`) and hand-written edge cases are the test fixtures; a golden test reproduces the counts against a real installed package when `TEMPLATES_CATALOG_DIR` names one. The suite is 264 tests (263 run in CI), checked by a mutation pass over about 1,660 one-line edits of the module: of the 56 that survived, ten were real gaps and now have tests, and the rest are equivalent or dead code.

### Added — a harness-owned licence map and a run gate for the templates (`render/templates-license-map.json`)

The map is keyed by Hugging Face repo id; each entry carries a class (`permissive`, `conditional`, `non_commercial`, or an explicit `unknown` for a repo whose licence nobody has read), its source URL, date and basis, and a repo that is not listed is `unknown`. An entry whose basis begins `inferred from` was copied from the sibling repo it names, not read: it carries no source URL or date and the summary's evidence range ignores it. A template takes the worst class among the repos its active nodes download from (non_commercial, then conditional, then unknown, then permissive), so an unlisted repo never hides a known restriction; a file with no Hugging Face source counts as unknown and a template with no weights is `none`. The gate blocks paid API templates (ADR 0001) and FLUX-family ones (ADR 0011: a FLUX name, a repo id containing "flux" or under the model maker's organisation, or a map flag, with the shared text-encoder repo exempt by its entry) and asks for an explicit acknowledgement naming the class for anything else that is not permissive. A repo whose id merely contains "flux" does not bar a template that takes only text encoders from it (a text encoder is another maker's model wherever it is hosted; the gate warns instead), while any other class from that repo, a file with no class and the maker's own organisation still bar; on the installed package that changes exactly one of 566 verdicts, a plain Qwen3 text-generation template that was blocked and now needs the unknown-licence acknowledgement. A FLUX-named weights file from another repo is a warning, not a verdict. `list` hides blocked templates unless `--include-hidden`; naming `--kind api` lists the paid API ones without it, and the FLUX-family ones stay hidden. The 50 seeded entries are the 2026-09-30 research digests of repo-level licence tags, none re-fetched; on the installed package they give 75 permissive, 63 conditional, 29 non-commercial and 68 unknown among the 235 local templates that need weights, and 25 FLUX-family templates among them (23 by name). Per-repo licence texts are still to be read: 83 of the 133 repos the installed package's local templates download from are unlisted.

### Added — directory-aware readiness across `extra_model_paths.yaml`, node snapshots and a candidate diff

A model file is usable by a template only if ComfyUI would offer it to the loader that reads it, so readiness follows ComfyUI 0.37.0's own rules (`folder_paths.py`, `utils/extra_config.py` and the directory registrations of `main.py`) instead of matching file names: default directories per class including the dual ones (`text_encoders` also reads `clip/`, `diffusion_models` also reads `unet/`, `controlnet` also reads `t2i_adapter/`), the yaml keys `unet` and `clip` mapped to their classes, `base_path` joined to each entry with an absolute entry winning and `is_default` first, the output directory's `checkpoints`, `clip`, `vae`, `diffusion_models` and `loras` added last (`--output-directory` moves it, `--base-directory` moves it and the models directory), network-share (UNC) roots kept intact, the yaml shapes ComfyUI's loader accepts (a provider named with a space or in quotes, quoted class keys, `|` and `>` block scalars, an empty entry skipped), and a recursive listing that follows links, skips `.git` and keeps the extensions the class's core loaders accept, matched by exact relative path. Compared with ComfyUI's own loader run on those shapes, the directory lists agree, for every model class and in order, apart from the one gap noted below. A requirement is `present` or says why not: `missing`, `wrong_class` (and where the file is), `in_subfolder`, `case_mismatch`, `extension_not_listed` or `class_unregistered`; `--mode basename-only` keeps the older name-matching method so its numbers reproduce and can be compared. The verbs `snapshot` (one node's package versions and fingerprint, API node ids, resolved roots and files, and hashes of the two ComfyUI rule files with line endings normalised; it exits 2 on a directory that is not a ComfyUI tree, and the file can be piped to `node -` on a node and writes nothing), `readiness` (per node and on any node; it refuses a node whose package differs from the catalog's unless `--candidate`, and reports `rules_check`, whether the node's rule files are the 0.37.0 ones the table was checked against, as an advisory that scores the node either way) and `diff` (what a candidate package adds, removes and changes, and which templates it would break on each node; `--candidate-basis` says what the candidate directory is) work from those. Read-only on the four ComfyUI nodes, which carry an identical package fingerprint and identical rule files: 8, 5, 6 and 1 templates are ready per node and 12 on any node, by both methods with no template flipping between them (measured before the output-directory and yaml-reader fixes; the one node re-run afterwards still reads 8); the drift from json 0.1.94 to 0.1.96 adds 14 templates, removes 2, changes the required files of 10 and would break 5 that are ready today.

Known gaps: the run-graph satisfier's presence check (`modelCandidates` in `render/manifest-satisfy.mjs`) does not look under `models/unet` for a `models/diffusion_models/...` path that ComfyUI itself lists, so on a node with no `extra_model_paths.yaml` it would download two or three diffusion-model files a second time (measured on two of the four nodes; not changed here); 83 of the 133 repos the installed package's local templates download from have no licence entry; two templates that take a FLUX.2 VAE from a FLUX-named repo stay blocked by the repo signal, and whether ADR 0011 reaches a reused FLUX VAE is the operator's call; `main.py` is not hashed, so a change confined to it is not seen by `rules_check`; and a yaml entry that is rooted but has no drive letter under a drive-lettered `base_path` resolves onto that drive under Windows path join and is left as written here.

## [0.149.0] - 2026-09-30 - kit-derived compose templates, captions-bar and the hyperframes-compose skill

### Added — four kit-derived compose templates, `captions-bar`, a caption grouping helper and the `hyperframes-compose` skill

`render/compose-templates/` ships five new vetted templates, all 1920×1080 at 30 fps, all discovered with no route
change (`local-offload doctor` and `offload_status` list them; the fleet `compose-video` task accepts them by name):
`stat-card`, `section-title` and `checklist-card` (opaque) and `callout-label` and `captions-bar` (alpha overlays for
`webm` and `mov`). The first four are adapted from card designs of the hyperframes-student-kit at the pinned commit
`0d30152` and rewritten in CSS keyframes: no script library, no count-up, Inter from the shared kit only, declared
variables with explicit limits, emphasis split into `*_pre` / `*_em` / `*_post`, neutral copy and a recoloured palette.
The kit is a reference, never an install: its MIT `LICENSE` and use permission are kept verbatim (hash-tested, and
pinned byte for byte by a `.gitattributes` rule) under `render/compose-templates/_third_party/hyperframes-student-kit/`
with a `PROVENANCE.md`, the root `NOTICE` credits it, and each README says where its template was adapted from.
`captions-bar` is written for the harness.

`render/captions-groups.mjs` is a pure helper from `offload_transcribe`'s `<base>.segments.json` to `captions-bar`
chunks. The `words[]` in that file are whisper-server's tokens, not words (a word-initial token keeps its leading
space; a continuation such as an apostrophe suffix, the digits after a currency sign or punctuation has none), so the
helper merges tokens into words before it groups anything. Over 489 real transcripts (55,473 tokens, 40,345 words)
every file regroups into exactly the words of its own segment text and no group has a space before punctuation; a
first version that trimmed every token and joined them with a space gave "It 's only", "$ 1 a" and "'t worry ." and a
space before punctuation in a third of all groups. Beyond that: pace presets (3, 5 or 6 words), breaks at sentence
ends, pauses of 0.15 s or more and a 42-character cap, holds that never overlap two groups, and chunks that fit the
template (at most 16,000 characters and 600 s), cut at 300 s by default, with `--chunk-sec` (up to the template's
600 s) and `--chunk-chars` to set another cap.

`skill/hyperframes-compose/SKILL.md` is the house skill ADR 0059 promised: hard bans on `npx hyperframes` and its
`init`, `skills`, `upgrade` and `add`, which tool to pick, the seven-template catalog (kept in step with the templates
by a test), the verification loop, the captions flow and the authoring contract. Installing it into an agent's skills
folder is the operator's step. The tool description, CLI help, README, operator guide, glossary and
`docs/systems/media-generation.md` name all seven templates, and tests fail when one of those lists, the skill's ban
list or a number the docs quote drifts from the code.

### Fixed — a `$` in a template variable corrupted the composition (every template)

`applyTemplateVariables` handed the rebuilt `data-composition-variables` attribute to `String.replace` as a
replacement string, and JavaScript reads `$1`, `$&`, `` $` ``, `$'` and `$$` in one. Ordinary text tripped it: a
caption "over $100 million" made the attribute invalid JSON, so `check` failed the render with `words_json is not JSON:
"undefined"` (a message that never mentions the dollar sign), and a `stat-card` figure of `$1.2M` rendered but was
judged by `lint` and `check` as placeholder text. `$&` spliced the old attribute, closing quote included, into the new
one, so caller text could become extra attributes on the composition's root tag. The declaration now goes in through a
replacer function. Tests cover every pattern through the fixture and four shipped templates, show that a hostile value
adds no attribute to the root, and pin the escaping of `'`, `&`, `<` and `>` (the runner's tag scanning, like
HyperFrames', ends a tag at the first `>`). Rendered through the runner before and after: the same three-group caption
list fails `CHECK_FAILED` before and renders after (VP9 `yuva420p`, alpha, all three groups read back exactly,
`$&` included).

### Measured — what the lane does with worker counts, with a long caption list and with a long clip

- **A frame must be a pure function of time; scaling above 1 is a rule of thumb, the measurement is the gate.** A
  keyframe that scales an element above its resting size (a halo breathing to 1.06, a ring popping to 1.1) gave frames
  that depended on the render worker count on two of the ported cards (80 of 180 and 172 of 210 frames differed between
  1 worker and 4 or `auto`, by a few pixels of gradient, and two runs at 4 workers disagreed); with opacity instead they
  matched, and with the blur removed and the scale kept they still differed, so blur was not the cause. The ports and
  `captions-bar` never scale above 1 (a test guards it) and each gives 15 of 15 identical pairs of decoded frames across
  1, 2, 4, 6 and `auto` (twice) workers. The older `title-card` drifts a large glow out to `scale(1.12)` and also gave 15
  of 15, so the rule is not a law; the gate is rendering at those worker counts and comparing decoded frames.
- **One CSS-animated element per caption group does not scale.** The same 8 s clip renders in about 24 s with up to 80
  groups in the page and 34 s at 150, and a 374-group, 12.9 KB list had written about 290 of its 450 frames after ten
  minutes when it was stopped. `captions-bar` therefore holds one element and derives the frame from the time of
  HyperFrames' `hf-seek` event (450 frames of that list in 39.6 s at draft quality). A 15,968-character string variable
  passed `lint`, `check` and `--strict-variables`. The event is in the pinned 0.8.61 bundle but not in its documentation,
  so the README names the version it was measured on, a test fails when the pin moves past it, and the operator guide's
  bump step names `captions-bar`.
- **A long caption chunk needs one worker and about 300 s, so the helper cuts 300 s chunks by default.** At the lane's
  default `auto` workers HyperFrames stores every frame and budgets 8.3 MB at 1080p, and refuses a render when that
  passes 90 % of the free space: a 300 s chunk (9,017 frames) was refused `DISK_HEADROOM` with 33 GB free (it would
  store about 75 GB; a 600 s chunk about 150 GB). At `workers` 1 the frames stream and the same chunk rendered in
  1,038 s (115 ms a frame on a busy box), so a 600 s chunk, which was not rendered, takes 27 to 35 minutes and
  passes the 1,800 s default `compose_timeout_sec`. The helper therefore cuts 300 s chunks by default
  (`DEFAULTS.variableSec`); the template's own 600 s is only the upper bound `--chunk-sec` is checked against
  (`TEMPLATE_LIMITS`, refused with the same text as before), so a caller who raises `compose_timeout_sec` can ask for
  longer chunks. A test reads the template's declared limits and pins the default and the limit in the skill, the
  captions README and the media doc, so neither number can drift from the code. The captions README, the skill, the
  media doc and the operator guide's timeout note carry the measured figures.

Not in this change: deploying the templates to the fleet, the `assets` parameter (an image variable type), GSAP
vendoring or a HyperFrames pin bump, and a test that renders the templates and checks their pixels.

## [0.148.0] - 2026-09-30 - the ampere-16 fast layer and the 35B seat are in the tier table; install render gates for the MoE spill and a storeless tier

### Added — the ampere-16 `fast` layer and the 35B seat are in the tier table (register A-113, ADR 0048 Amendment 2)

The `fast` digest layer and the Qwen3.6-35B-A3B seat behind it existed only in the ampere-16 reference box's
hand-edited config and in two Go test files, never in `setup/templates/profiles.json`, so a fresh install of the
tier lost both (the capability-loss class ADR 0048 was written against, one level up). The table had one `vllm_seat`
per tier and a card cannot hold two heavy seats, so the schema grew by the smallest steps that carry what the box runs.

- `extra_vllm_seats` beside `vllm_seat`: further vLLM seats served on demand on the same card, never the agent lane.
  Validated as non-lane seats (`Spec.ValidateExtra`: no fallback, every lane field refused), with unique ids, aliases
  and units and the lane seat's card. `vllm_seat.storeless_reason` (and the same on an extra seat) is the measured
  reason a seat has no cache server, seeded verbatim into its `kv_cache_server` binding; refused beside a `cache_server`.
- `ampere-16` now declares `layers` `single` (the 27B GSQ lane seat) and `fast` (the 35B, 32,768 at 8 in flight), the
  35B as its extra seat, and both seats' B-01 storeless reasons with the local paths dropped. The layer values are the
  reference node's own (the two placement and delegate tests pin the same ones), so `audit-config` reports MATCH for
  `layers`, `tiers` and `tier_profile` against a fixture that carries them: a live extract of that node redacts each
  layer seat's `ctx_tokens`, so the 32,768 is the value those tests pin, not a live reading. A layer seat naming a vLLM
  seat must equal that seat's `max_model_len` (and `max_num_seqs`, when set) or the table is refused at parse.
- Seeding is per seat and never advertises what the box cannot serve: an extra seat the box can run (the venv, its
  own weights and the wrapper scripts the operator installs for it) joins `vllm_seats` with its own binding; a layer
  whose vLLM seat is absent is dropped; a layer set that lost `single` (the planner default, placement row 5b) is not
  seeded at all; a box with no vLLM prerequisites seeds exactly what it did before (`tierseed.ResolveLayers`,
  `Options.ExtraVLLMSeatsActive`).
- `install render` emits every vLLM seat of a tier as an ALTERNATIVE of the others inside the residents set
  (`emb & rer & (vagt | vagt2)`): the two seats cannot share the card, and co-resident members would have llama-swap
  load the second beside the first. The extra seat's entry names its wrappers after its own unit, and a box that runs
  it without the lane seat (the 27B's weights absent, or two snapshots under them mid-upgrade) renders it with the
  box's own runtime; `servingtmpl.Render` refuses any vLLM seat whose runtime is incomplete, because such an entry
  names no seat directory and no address and every gate that reads the text passes it. The composition check now runs
  for any tier that declares layers, not only one that composes. `ParamsBasis` mirrors `Params.ExtraVLLMSeats`, and a
  replay pins it.
- `extra_vllm_seats` keys are strict, like `layers`: a key that is not a seat field is refused by tier and JSON path at
  parse (`tierseed.ParseDoc`, so the installer's embedded copy too), never silently dropped. A misspelt `storeless_reason`
  used to seed the generic reason in its place.
- Not rendered, on purpose: the extra seat's systemd unit, wrapper scripts and polkit rule. Its production launch line
  carries `--language-model-only`, which the shared linux-systemd run script cannot express, so
  `docs/systems/composite-tier.md` lists what the operator installs by hand. Those scripts are also the seat's
  prerequisite (`Spec.DetectExtra`): llama-swap does not check that an entry's `cmd` exists when it loads its config,
  so a box with the venv and the weights but not the scripts would list the seat, seed its layer and fail only when a
  contract asked for it. Until they are in the seat directory (`--vllm-seat-dir`, default `<home>/seat`, now a flag of
  `install seed` and `audit-config` as well as `install render`), the seat and its layer are left out and the note
  names the missing file.
- The layer regression floor: `TestEveryTierKeepsItsDeclaredLayerSet` (`layerSetTiers`: each composite tier's layers and
  the seat roles each serves) fails by name when a tier stops declaring one, and `extraSeatFloor` pins the extra seat.
  Each was made red against the real regression and the table restored byte for byte: deleting the `fast` layer,
  deleting the seat while keeping its layer, an extra seat with no tool parser, a layer window that drifts from its
  seat's, deleting every layer, moving the layer's seat to another role. `docs/tiers/ampere-16.md` is regenerated
  (layers, the 35B, both storeless reasons); `tierdocs` heads a layers-only tier "Layers", not "Composes".

### Added — H-01's two remaining render gates: the spill ceiling and INV-16

- `install render` refuses `--n-cpu-moe` (any spelling, and `LLAMA_ARG_N_CPU_MOE`) above the tier's measured spill,
  the new `n_cpu_moe_max` (`servingtmpl.AuditSpill`, in the write gate `renderGate` that now also hosts the H-01 rule
  audit and the layer check). It is a separate number from `n_cpu_moe`, because one field cannot check itself; 0 means
  the tier recorded no measured spill, so none is sanctioned. A tier that names `moe_26b: n_cpu_moe` with no N is
  refused too, since that renders the every-expert `--cpu-moe`. No shipped tier declares a spill.
- `TestInstallRendersOnAnyTierWithoutACacheServer` (INV-16): every tier renders with no vLLM prerequisites, and a
  tier whose vLLM seat declares no store renders the seat, its unit and wrappers with no cache-server piece and
  seeds an explicit storeless binding that `doctor` accepts. A spill of 20 against a measured 14 and a hard-coded
  `--n-cpu-moe 30` in a rendered command are refused.
- Both serving-config audits (`Audit` and `AuditSpill`, so `install render`, `audit-yaml` and the template gate) now read
  each entry the way llama-swap runs it, with its `${name}` macros substituted (nested macros too, bounded, and
  llama-swap's own `${PORT}` left alone). The templates keep their shared flags in `macros:`, so a `-ngl 0` or an
  `--n-cpu-moe 30` placed there used to pass every rule. Every shipped template still passes.

### Fixed (tests) — the streamed re-pack tests had a 5x timing margin

`TestRepackSlowStreamingSeatIsNotStalled` streamed a delta every 40 ms against a 200 ms stall allowance, so one scheduling pause of a loaded CI runner filed the stream as a stall (seen once on this PR's merged head; 20/20 locally, green on main). The allowance is now 600 ms (the 2 s stream still outlasts it, so a stream that stops counting as progress still fails: checked with 700 ms gaps), and `TestRepackProgressReachesTheJobRecord` gets the same headroom (600 ms / 900 ms). `TestRepackSilentSeatStillStalls` keeps 200 ms.

## [0.147.0] - 2026-09-30 - a finished answer is rescued instead of deferred when its re-pack fails; the re-pack is sized, streamed and held by the busy hold; research acceptance

### Fixed — a finished answer whose structured re-pack failed is re-packed by the delegator, not lost (C-66, PR-4)

A node whose agent loop finished but whose structured re-pack failed (stalled, unreachable, cut, or a shape the schema refused) deferred, and a deferred result is never offered to acceptance, so the finished answer reached the caller as prose inside an error envelope and was counted as lost work. Every one of the 80 re-packs the fleet's nodes killed on 2026-09-29 carried a finished answer, and about 15 job-hours had been discarded that way since 2026-09-20. The delegator holds the answer, so it now re-packs it.

- The node marks such a defer with the new `schema_miss` wire field (omitempty); the defer itself is unchanged, so an older delegator reads what it always did. A node that predates the field is recognized from `stop_reason: done` plus the re-pack failure reason prefix.
- A cut answer (`output_truncated`), an empty one, a caller cancel (a defer whose reason starts `canceled during the structured re-pack`, whether or not the node flagged it) and every other defer are never rescued.
- The rescue runs inside `finish`, before the ledger row is written, on the local, remote and queue routes: the lossless reading first (the answer may already be the object), then ONE completion on the delegator's own agent seat, held to the schema the node's answer would have been (every field the acceptance reads required) and bounded by the re-pack allowance sized from the answer and that seat's rate. It is one request, not a run: no run registration and no slot of the run cap, and it waits in line on a busy seat like any other request.
- A seat that is not resident is warmed first, on the box's admission budget (`agent_admission_wait_sec`, -1 turns it off) and outside the allowance, as a run's own admission does. The delegator's seat is idle-unloaded after five minutes, so the rescue routinely finds it cold, and a cold load (125-250 s on a vLLM seat) is longer than the whole allowance: without the warm-up the completion was cut by its own load. The rescue may therefore wait up to `agent_admission_wait_sec` (300 s by default) behind a GPU-lease fence or another model's swap before its own allowance starts; the delegation context has no deadline of its own until the whole-call deadline (PR-6), so those two bound it.
- The object is validated again on the delegator (a JSON object that satisfies the schema) and the ordinary acceptance runs over it. A success reads `repack_note: rescued on <seat> ...`; a rescue that cannot produce a validated object leaves the defer exactly as the node sent it, still counted in `lost_to_stack`, with a note that a rescue was tried. The failed rescue's log line names the job.
- Wired on `agent_delegate`, `offload_research` and the CLI verbs `delegate` and `research` (a source scan fails the suite when any production `RunWith` / `RunBatched` call stops passing one). Deliberately not wired, for three different reasons: `agent_run` carries no `output_schema`; the review lane's fenced-seat fallthrough exists because the local seat is fenced by another session's lease, and a rescue there would queue behind it; `offload_ask` at a named remote route keeps its own handling of a finished answer (its prose still reaches the caller), which the security review records as deliberate, so it is outside this change. The seam is `RunOptions.Rescue`.

### Fixed — the structured re-pack streams, is sized from the answer and the seat's rate, and stops sending attempts on a dead context (C-66, PR-5, PR-12; ADR 0055 item 8, ADR 0061 item 8)

The re-pack was the one completion still sent non-streamed under a flat 120 s allowance, so a slow seat decoding a long object looked silent for its whole decode: 119 of the 838 node-side re-packs since 2026-09-20 were killed at 120 s, `repack_attempts` read 3 on 119 of those 119 stalled rows (attempts sent after the monitor had cancelled), and on 2026-09-29 the kills were 70 % of the busiest remote seat's re-packs.

- The re-pack streams (SSE) on both the grammar arm and the chat lane, falls back to the plain JSON body when a seat or proxy ignores the request, and retries as plain JSON, remembered per seat for 30 minutes, when a server answers a streamed request with 400 or 422. On a vLLM seat the streamed request carries `structured_outputs`.
- A seat that refuses the stream is no longer a silent mode switch: `repack_note` reads `streaming refused by this seat` (on a success too), because on such a seat the re-pack is one silent request under its allowance again and nothing else would show the fix did not apply to it. A JSON retry that fails in a different way than the refusal logs both halves.
- Progress reaches the liveness monitor and the job record, and the allowance is published when the phase starts, so a slow seat that is producing is not a stalled seat; a silent one still is.
- The allowance is `max(120 s, 1.5 x expected answer tokens / decode rate + 30 s)`, where expected is the answer's size and never the completion cap (685 tokens at 2.7 tok/s allows 410 s). A seat with no measured rate keeps the flat 120 s, and a stalled re-pack's reason names the arithmetic, or the flat bound and why it is the flat bound (no measured rate, or an unknown size).
- Under the monitor the re-pack's requests carry no transport bound: the busy hold (ADR 0061) governs a request whose seat is working for others, and once the monitor has cancelled, no further attempt starts.
- Not verified live: vLLM 0.29 with `stream: true` together with `structured_outputs` is documented but has not been exercised against a live seat; the plain-JSON retry is the safe default.

### Fixed — `offload_research` no longer fails correct digests (C-74, PR-3)

On 2026-09-29, 72 rows (8.6 %) failed verification and 59 of them were `min_items`, 36 through the direct path (an object that never reached the re-pack): the schema asked for nothing while the acceptance demanded an item from the alphabetically-first array.

- Presence is declared: the default research schema lists all four fields (`key_facts`, `numbers`, `quotes`, `verdict`) as required, a caller's schema gains every field its acceptance checks, and the node holds any contract to it (an object missing a field the acceptance reads goes to the re-pack; on a vLLM seat the fields are required in `structured_outputs` too).
- Non-emptiness is asked for only where the caller marked it: the first array of the caller's own `required`, or the caller's own acceptance. A faithful digest of a page with nothing to extract is a success. The default digest owes one statement on every page, anchored or not: `nonempty:verdict`, so a page too thin to anchor never carries an empty acceptance and a digest that said nothing (every list empty, no verdict) is not delivered as a success. Empty lists with a verdict that says so still pass.
- An acceptance-only research failure is no longer retried on another node (the page is the same; the retry note says so), except a failed document fingerprint (`docanchor`): an off-document answer is a fact about the node that wrote it (a node is quarantined after two), and another node is the cure. Abstentions and every other door's acceptance failures are still retried.

### Fixed — a warm-up refused with a server error is a seat that did not start only when nothing says it is busy (C-76, admission half)

Admission proceeded into the wall on any non-200 warm-up answer and reported it as a load.

- A non-200 that loaded nothing is no longer counted as an attempted load (no cold-load sample in the seat-rate store, no coherence probe); a 404 or a timeout still proceeds. `agent_run` is unchanged.
- A 5xx is evidence the seat's process did not start only when llama-swap's answer is not one of its busy shapes (429, 503 `process is not ready`, a 500 of its own, a health-check timeout, an empty 502), `/running` lists no row for the seat and nothing else is mid-swap, and `/running` could be read. A seat that reads starting or stopping, another model's swap, an unreadable `/running` and every busy shape proceed with the reason in `admission_note`: a busy card is a place in line, never a reason to refuse. The body's `upstream command exited` outranks a busy-looking answer.
- The defer ships in audit mode (new config `agent_warm_failure_defer`, default `false`, the house standard's audit-then-enforce): the run proceeds as it always did and `admission_note` says `this run would have deferred at once`, so would-be defers can be counted before anyone turns the rule on. With `true` the run defers as infrastructure with the reason prefix `seat warm-up failed: ` within one poll, before any wall exists, and the delegator gives that defer one retry on another node and credits back the admission the node spent (like the coherence defer), so it is not terminal.

### Changed — `repack_attempts` counts requests sent

It used to read 3 whenever the re-pack ran out its attempts, including attempts that never left because the monitor had already cancelled. A stalled re-pack now reports the requests it actually sent (usually one), so anything keyed on `repack_attempts == 3` as exhaustion must change.

## [0.146.1] - 2026-09-30 - the vLLM seat launcher reaps what a crashed seat left behind

### Fixed — a crashed vLLM seat's engine workers and MP server are reaped at crash time, not only when the MP HTTP port is held; an unload is not mistaken for a crash

When a vLLM engine dies nothing runs a stop: the API server exits, but the engine workers it could not stop and the LMCache
MP server outlive it. 0.143.0 taught `seat_fg.sh` to run `seat_stop.sh` when the MP HTTP port was held; workers left behind
with the MP port free were still not reaped, so the next engine loaded onto cards that were still held, and nothing cleaned up
until a request started the next generation (2026-09-29: three workers survived SIGTERM and held 14.5-16 GB on every card
while the MP server kept its HTTP port: 23 minutes down, 59 failed starts, 75 HTTP 500s).

- `seat_fg.sh`: with no engine of its port alive, the start looks for three kinds of leftover (a listener on the MP HTTP
  port, an `lmcache server` of its MP port, an orphaned engine process: named `VLLM::*`, found by the start of its command
  line and never by `pgrep -f`, with no live vLLM API server above it, `vllm serve` or `python -m vllm.entrypoints...`, so a
  sibling seat's live workers do not count) and runs `seat_stop.sh` once, attached (its output is already the seat log), then
  waits up to `SEAT_MP_PORT_WAIT_SEC` (10 s) for the port. A holder that is not the stack's own is still refused, and that
  refusal is the last check in the block.
- `seat_stop.sh`: reaps by rule, never by the port a process holds (an engine process with no live vLLM API server
  ancestor; the MP server of THIS stack's MP port and unit). The engine rule reads the process tree: it is a rule, not a
  proof of ownership, and an engine a script builds in-process would look orphaned, so such jobs belong where no seat lives
  (the scripts and ADR 0035 now say so instead of claiming proof). An MP server that ignores SIGTERM gets SIGKILL after
  `SEAT_REAP_WAIT_SEC` (10 s). A reaped worker or MP server that survives SIGKILL (in real life a process stuck in the GPU
  driver, state D) is named with its process state and the stop exits 1 (`INCOMPLETE`); a zombie is not stuck. A unit that is
  already gone is no longer a warning, and a cleanup with no process tree no longer waits 3 s.
- `seat_stop.sh` appends its own output to the seat log unless `seat_fg.sh` runs it attached: the stop task and llama-swap's
  `cmdStop` have no stdout anyone reads, so a survivor of SIGKILL and an `INCOMPLETE` stop used to vanish. A named env file
  that does not exist (argument or `SEAT_ENV`) exits 2 and stops nothing; it used to fall back to the default seat's ports
  and unit and reap whatever held them. The shared-memory sweep also stays off while a `python -m vllm.entrypoints...`
  engine is alive.
- `SEAT_REAP_WAIT_SEC` and `SEAT_MP_PORT_WAIT_SEC` are read as decimal seconds. They reached `$(( ))` as octal: `08` and `09`
  turned every wait into a no-op, and `0` skipped the survivor check, so a stop reported "gone" and exited 0 with a worker still
  alive. A value that is not a number is now said in the log and means 10.
- `seat-cmd.ps1` (Windows/WSL stub): its 30 s "seat stopped answering" exit ends every unload as well as every crash (about nine
  exits in ten in the live stub logs were unloads), so a cleanup on every exit would have started a second stop about 40 s after
  each unload, woken a distro that had powered itself off, and delayed the exit that completes the unload against an
  `unloadTimeout` of 60 s. `seat-cmdstop.ps1` now leaves a marker file (`seat-stop-requested-<seat>`) in the seat directory
  before it starts the stop task; the stub removes any marker when the seat comes up and skips the cleanup when it is there.
  A crash leaves no marker, so its exit starts the operator-session stop task, waits for it (90 s of the clock at most; a task
  never seen running gets 30 s to appear: both were counts of polls that a 1.1 s Windows PowerShell 5.1 read stretched to about
  140 s and 47 s), reads and logs the task's result (a stop that did not finish is named), and exits 0 as before. It does
  nothing while the seat's own start task is still running (a live launcher owns the seat), never relaunches the seat, and a
  stop task that will not start or never finishes only logs. No watchdog, no scheduler: both points run inside processes
  llama-swap already supervises.

Tests: `seat_fg.stale-mp.tests.sh` drives the real code of both scripts against stand-in processes named like the real ones
(44 checks as root, 43 without: the real transient-unit case needs root and systemd; including two mutants of the
foreign-listener guard that it must catch); it takes a directory lock and steps aside when another run holds it (a lock file
created by one user could not be opened by another under `fs.protected_regular=2`), and the Go wrapper starts a second
overlapping run and requires it to step aside. `launcher_seatcmd_crash_test.go` runs the rendered stubs for real in Windows
PowerShell 5.1 and PowerShell 7 against a driver with a simulated clock, the scheduled-task cmdlets stubbed, and `Set-Content`
stood in for with the non-terminating error the real cmdlet raises for a missing directory (a directory in the way, a lock or a
read-only file raise terminating ones, so they cannot tell `-ErrorAction Stop` from its absence). The behavior of every guard
the review found pinned only by text is now driven (port boundary of the MP
pattern, anchored engine name, the unit stop through a stand-in `systemctl`, a three-level engine tree, zombies, survivors of
SIGKILL, slow-dying processes, the shared-memory sweep, the missing env file, the seat log written once). ADR 0035 gets a dated
update (the marker, what is reaped and how sure that is, the alternatives rejected, the limits); `docs/systems/cache-server.md`
items 4d and 4g and the operator guide describe it.

Deploying is a seat-side action: copy `seat_fg.sh` and `seat_stop.sh` into the distro's seat directory and re-render and deploy
BOTH `seat-cmd.ps1` and `seat-cmdstop.ps1` (the stub reads the marker the stop stub writes; with only the new `seat-cmd.ps1` no
marker exists and every unload is cleaned up after as before). Diff a deployed stub first: one may have been edited by hand.
The `seat-stop-requested-<seat>` file next to the stubs is the marker, not litter. Not yet seen on a live crash.

## [0.146.0] - 2026-09-30 - a delegator takes back a job it has not started, and a node cleans up after one that left (ADR 0064)

### Fixed — a delegator takes back a job it has not started, and a node cleans up after one that left (ADR 0064; register C-70 ghost-job half, C-68, C-63)

Every give-up on an accepted job (the queue deadline, a canceled caller, a poll deadline) left the job on the fleet node, where it started when a slot freed and ran for nobody. On 2026-09-29, 43 % and 59 % of two nodes' agent runs (50 % and 73 % of their run wall) had no delegator result row, and the finished walls fed each node's own Retry-After, which sent callers away for longer and produced more abandoned jobs. The recovery pass meant to collect such jobs closed 61 of 61 open intents on a 404 or a 401 and recovered none.

- **`DELETE /fleet/jobs/{id}`** (agent bearer, like the poll) withdraws a job that is still `accepted`, under the job store's mutex the scheduler claims under: exactly one of a claim and a withdraw wins, and a running or finished job is never touched (running work stays recoverable). Answers: `200 {"state":"withdrawn"}` (also for a repeat, and for a job the node already took back itself: reaped, or marked never-started at shutdown), `409` with the job's own state, `404`, `401`, `405` for a non-agent job. A withdrawn job is a terminal `error: "withdrawn: ..."` record rather than a deleted one: a poll reaches a terminal state, a duplicate dispatch of the id answers `409` and never runs, and the jobs feed shows what became of it. A node without the route answers `405`/`404`, and every delegator give-up then behaves exactly as before.
- **The delegator asks once, best-effort** (5 s, on a context the caller's cancel does not touch) at the queue deadline, on cancel, and at a poll deadline, owned or not. At the queue deadline only a confirmation makes the result re-placeable on another node (the queued wait is credited back to the contract's budget) and closes the intent as `withdrawn`; `409 running` means the job left the backlog just before, so it keeps polling. Anything else (404, 405, 401, 5xx, a dropped connection, a timeout) leaves today's failure with the intent open, and the row now says why: `; withdraw not confirmed: HTTP 405: the node has no withdraw route (an older node)`, `HTTP 401: the node refused this delegator's fleet_auth_token`, `no answer within 5s`. An older node and an upgraded one that refused no longer leave identical rows; the clause is detail, and `reason_code` stays what the give-up was. A cancel that landed while the delegator slept used to skip the orphanable mark and close its intent as "terminal observed"; fixed.
- **A poll that reads the node's own record.** A delegator that was away for longer than the lease finds `error: "reaped: ..."`; another caller's withdraw reads `withdrawn: ...`; a node that shut down with the job still queued marks it `not started: ...`. All three say the job never ran, so the subtask is re-placed on another node (queued wait credited back), the intent closes `never started: <what the node said>` and the abandoned attempt's row reads `queue_withdrawn`. A job that ran and failed stays a remote job error and is never re-placed.
- **Poll lease** (`fleet_poll_lease_sec`, default 60 s, negative = off, a value under 15 is raised to 15, and `fleet-serve` prints which lease is in force at start-up): a pushed agent job that sits `accepted` with nobody polling it is skipped by the scheduler at once and reaped by a ticker (`error: "reaped: ..."`). Never a running job, never a job the pull queue claimed, never a media or vision job. What counts as a poll is decided by the JOB's own marker, not by the `task_type` a request declares: an authorized poll, an authorized duplicate dispatch and a parked long poll keep a job alive; the unauthenticated jobs feed, a poll or a re-dispatch without the bearer (`401`, whatever type it declares) do not, so a peer that can read ids off the feed can no longer keep a ghost alive or read a failed agent job's error. A run that finishes after its poller left no longer feeds `recent_agent_wall_sec` or the Retry-After built from it. An upgraded node also cleans up after an older delegator. The reaper reads its tick on the constructing goroutine and hands it to its goroutine by value (a package variable read from the goroutine the constructor spawned made `go test -race ./internal/fleetnode/` fail; it is clean now). The node counts what it took back: `jobs_withdrawn` and `jobs_reaped` on `/fleet/health` (since the process started, absent while zero), and logs one line per withdraw.
- **Recovery and the intent ledger.** The recovery pass leaves an intent open on a 401 (one log line per pass instead of a permanent close) and closes a job the node never ran as `never started` instead of filing it as a recovered result. A poll answered 401 mid-run, and a poll deadline that no answer ever owned, no longer close their intent as "terminal observed" either: the 401 leaves it open, and the unowned deadline gives up like the others (one best-effort withdraw; confirmed closes it, otherwise it stays open). Every intent event carries the unix second and pid of the process that wrote it. An intent write that fails, a dispatch marker that cannot be written, and a recovery pass cut off by its clock now log once instead of vanishing.
- **The fleet overview** no longer lists a `withdrawn` or `reaped` job as an operator error (dozens a day on a busy node would fill its 200-entry ring).

### Added — the delegation ledger sees its own failure shapes (PR-14)

- Every `agent_delegate` row carries `door` (the contract's, else `delegate`; `fleet-smoke` stamps `cli:fleet-smoke`), `fleet_job_id` (when the node ACKED the dispatch; the node's own `agent` row carries the same id, stamped by both doors a node admits work through, so the orphan join is one equality instead of a guess on latency) and a closed-set `reason_code` (32 members: `ok`, `failed_verification`, `queue_full`, `queue_deadline`, `queue_withdrawn`, `poll_deadline`, `canceled`, `node_unreachable`, `job_lost`, `dispatch_refused`, `remote_error`, `capacity_wait`, `node_busy`, `shed`, `no_eligible_node`, `seat_down`, the `stall_*` phases, the node's defer classes, `started`, `other`) set by a total classifier and normalized by `Record`. `capacity_wait` is the delegator's own outcome (no node had room); `node_busy` is a node, or the local seat, answering a capacity defer after admission (a seat at its run cap, a card under a lease or fence): two different causes, told apart by structure and not by prose. A stall a node filed during the structured re-pack (`structured re-pack unreachable: stalled: ...`) is coded `stall_*` like any other.
- A `phase:"started"` marker row is written when a node acks a job or a local run begins, so a hang or a ghost is visible while it happens. It is never a job: every counter skips it, and `ledger.ParentJobIDs` skips it too (a marker looks exactly like a parent row, so without the skip an orphan inner row would vanish from every count). The C-62 rule is unchanged.

### Changed

- `ledger.Entry.Reason` is stored whole (bounded at 4096 bytes on a rune boundary) instead of cut to 120 bytes. `ledger.ShortReason` is the 120-byte form the defer report, the loupe report and atlas, and the PAIR error text group by, so a class is not split by every job-specific number. Rows written before this keep their cut reason and carry no code, fleet id or marker.
- New config key `fleet_poll_lease_sec` (`config.example.json` regenerated); two additive, omitempty health fields, `jobs_withdrawn` and `jobs_reaped`.
- Compatibility: withdraw and the lease need a node redeploy. Ledger readers outside this repository that count rows must skip `phase:"started"` (one extra `agent_delegate` row per job, `cards_tokens` 0).

## [0.145.0] - 2026-09-30 - the operator names the device that serves a shared accelerator tool

### Added — `accelerator_tool_owners` (ADR 0068)

- A box carrying a Coral that reaches the RK3588 NPU through `fleet_accelerators` gave every shared tool
  name to the Coral, because local devices are walked first (ADR 0037). `accelerator_tool_owners` maps one tool
  name to the device that serves it — `{"offload_object_detect": "rknpu"}` — and every other name is decided as
  before. An entry applies only when its device is listed in `accelerators` or `fleet_accelerators` and has that
  tool; otherwise it is logged at startup and ignored, so a typo never removes a tool. A fleet device may take a
  name from a local one.
- Both surfaces apply the same walk: `mcpserver.accelOwnerPlan` computes every owner without registering
  anything, and MCP registration and status read it; the agent loop's lanes carry the same claims
  (`AccelLane.Claims`, from `config.ToolOwnerClaims`). `TestToolOwnersLoopMatchesMCP` builds the loop from the
  real `NewLoopAccel` and checks every name the plan serves is registered once and routed to the same device,
  for local orders of three devices, local-plus-fleet shapes, spaced keys and duplicate keys.
- A device id listed twice no longer registers its tools twice on the MCP surface (the registration walk skips
  a name it has already added instead of relying on the SDK replacing it).
- Status: each accelerator entry adds `serves`, the tools that device actually registered, beside `owns`.

## [0.144.2] - 2026-09-30 - offload_nim's base is allowlisted, audit first

### Security — a caller-named offload_nim base is checked against an allowlist (security standard L5)

Since 0.143.1 the NVIDIA key reaches only NVIDIA's hosts, but `offload_nim` still sent the PROMPT to any `base`
a caller named: one prompt-injected call could post session text to a URL of its choosing. The base must now be
NVIDIA's hosted API, `nim_endpoint`, or an entry of the new `nim_bases` list (scheme, host and port equal; the
entry's path a prefix on a segment boundary; userinfo never matches). The new `nim_base_policy` follows the house
standard's audit-then-enforce rule (ADR 0067): `"audit"` (the default) lets any other base run, marks the result
with `base_policy` and appends a would-refuse row — scheme, host and port only, never the prompt, path or query —
to `<state_dir>/nim-base-audit.jsonl`; `"enforce"` defers the call before any request leaves. The CLI `nim` verbs
are operator-typed and unchanged. Tests: the allowlist rules (20 cases), enforce sends zero requests to an unlisted
base and writes an enforce row without the path or query, audit runs and counts, a listed base runs cleanly under
both policies; making the allowlist accept everything turns them red.

### Added — doctor shows fleet version skew (security standard L0)

`local-offload doctor` prints one row per `delegate_remotes` node: OK, SKEW (with both versions and the redeploy
command), UNKNOWN (the node publishes no version) or UNREACHABLE. Informational only, never an exit-code change.
Parity broke twice on 2026-09-30 within an hour because a merge landed between deploys; the session-start audit
caught it, and now any session that runs doctor does too. `delegate.NodeView` carries `HarnessVersion` for it.

### Added — the house security standard (ADR 0067) and the bare-client lint

`docs/systems/security.md` is the standard every part of the harness is held to: seven invariants, ten layers
plus a reliability track, AARM v1.0 R1-R9 as the checklist, promotion from audit to enforce only on counted data,
and twelve gates that fail when their control is removed. Gate G2 ships with it: `bare_http_client_lint_test.go`
fails the suite on any new bare HTTP client (ADR 0042 made executable) until the site is reviewed and listed with
its reason; the first review lists 43 sites, one of them open (`offload_nim`'s caller-named base, next in L5).
The ADR index row for 0061 now describes the rolling allowance that shipped, not the first draft.


## [0.144.1] - 2026-09-30 - the trusted-network flag permits one address, never every interface

### Security — a fleet node could serve its unauthenticated endpoints on every interface after a boot race

`--listen-trusted-network` returned before any check, so it permitted ANY listen address. A Linux unit's
`--listen "$(tailscale ip -4)":18811` that ran before tailscaled had an address expanded to `:18811` and
bound every interface instead of failing — one fleet node served its unauthenticated fleet endpoints that
way until its next restart, and the unit's `Restart=on-failure`, written for exactly that boot race, never
fired because nothing failed. The flag now permits one specific address: an all-interfaces address (empty
host, `0.0.0.0`, `[::]` and their spellings) and an address that does not parse are refused with or
without it (`netguard.AllInterfaces`, now also the rule fleet-ui uses). fleet-serve, local-agent and
fleet-ui share it. `setup/install.sh`'s unit gains `RestartSec=15`, so the failed start retries until the
tailnet address exists without hitting systemd's default start limit (a unit without it gives up after five
quick failures). Tests: the boot-race shape through fleet-serve's own parameter seam, the validator table
with and without the flag, local-agent's listen guard — each red against the previous validator.

## [0.144.0] - 2026-09-30 - Rockchip RK3588 boards join the fleet as their own tier, serving from the NPU

### Added — the `rockchip-rk3588` tier (ADR 0062)

An RK3588 board (reference: `<node-d>`) used to classify as `cpu`, and `fleet-serve` refused to start
on it: no PCI GPU, no GPU memory source. `hwdetect` now recognises the SoC from the device tree (vendor
and mainline spellings), a `linux-meminfo` provider advertises MemTotal less `uma_reserve_gib` as the
node's capacity, and the tier renders from its own template with no CPU seat. Measured on the reference
board, mainline 7.0 kernel with the out-of-tree rknpu 0.9.8 driver: Qwen3.5-2B W8A8 on the NPU prefills
22.7 tok/s and decodes 7.71 tok/s with the runtime on the A55 cluster (126.4 / 7.14 on the A76 cluster),
peak 2,260 MB.

### Added — the `rkllm` seat kind and `accelerators/rknpu/rkllm_server.py`

An OpenAI-compatible server over Rockchip's RKLLM runtime (text and vision, streaming, stop strings, one
generation at a time; its ctypes structs are checked byte for byte against the vendor header), launched by
llama-swap per seat with the seat's `cpu_mask`. It guards the host it shares: grammar / JSON-schema requests are
refused with a 400 (RKLLM cannot constrain decoding, so the client fails over instead of receiving free text),
at most two requests wait behind the running one (then 503 `busy`), bodies are capped at 16 MiB, and both NPU
servers raise their `oom_score_adj` so the kernel's OOM killer takes them before the host's own stack. The seat
ships an 8192 window until its resident size at 16384 is measured.

### Added — the `rknpu` accelerator

A Coral-style sidecar on the RKNN runtime serves `offload_classify_image`, `offload_object_detect` and
`offload_image_embed` on the NPU (ResNet18 3.58 ms, YOLOv8n and a CLIP image tower; models pinned by
sha256). Detection reads `DRIVER=RKNPU` from the DRM card on the vendor kernel and from the NPU platform
devices on mainline, and never matches the in-tree `rocket` driver.

### Changed — a template may leave every model to the tier's seats

The serving audit accepts an empty `models:` map in a raw template that carries an `# offload-seats:`
directive, and `Render` refuses a result that still serves no model. A set made only of seats no longer
renders with a leading operator.

### Not added — a GPU entry on the RK3588 Mali

llama.cpp b11270 on Mesa 25.2.8 panvk loads on the Mali-G610, but its first compute submission hits a
panthor job timeout and `vk::DeviceLostError` for every model and batch size tried. The tier renders no
llama.cpp entry until that measures clean.
## [0.143.1] - 2026-09-30 - the NVIDIA key goes only to NVIDIA's hosts

### Security — `offload_nim` could send `NVIDIA_API_KEY` to a caller-supplied host

`nimclient.IsHostedNVIDIA`, which decides who receives the key, was a substring test on the whole base URL
(`api.nvidia.com` or `integrate.api` anywhere in it), and the `offload_nim` MCP tool takes a caller-supplied
`base`. One prompt-injected call with a base such as `https://attacker.example/integrate.api/v1` or
`https://api.nvidia.com.attacker.example/v1` sent the key as a Bearer token to that host. The key is now bound to
the parsed host: https, no userinfo, the default port, and a host that is `api.nvidia.com` or a DNS subdomain of
it (`integrate.api.nvidia.com`, `ai.api.nvidia.com`), case-insensitive with a trailing root dot dropped. Every
caller (the MCP tool, the CLI's `nim` verbs, the status line) resolves the key through this one function. Tests
cover 15 hostile bases (path, query, fragment, attacker subdomains, userinfo in both directions, plain http,
a non-default port, case and trailing-dot variants), NVIDIA's own hosts, and the wire itself: through a fake
dialer, no hostile base puts the key in any header, NVIDIA's host receives it as the Bearer token, and a keyless
self-hosted NIM sends no Authorization. Restoring the substring test turns them red.

Not in this release: `offload_nim` still accepts an arbitrary base, so a session can still send prompt text to a
URL of its choosing (no key). An allowlist of bases is a separate change.

## [0.143.0] - 2026-09-30 - liveness judges the seat, not the request

### Fixed — busy, preempting or throttled seats no longer kill the runs they serve (ADR 0061)

A request silent past its phase allowance was declared stalled on its own silence alone, against allowances
sized from one uncontended request on a cool seat. Waiting its turn in the engine's queue, time-sharing a busy
card, being preempted by vLLM or running on a clock-capped card all read like a hung engine: on 2026-09-29 the
fleet's agent jobs succeeded 15 % of the time — 96 stall kills, 41 finished answers discarded in their re-pack,
four decode "stalls" in one second on one seat — while the engines produced throughout.

The liveness monitor now reads the seat's ENGINE before calling silence a stall (`seatload.ReadActivity`, at
the seat's own address, never `/upstream`): a work fingerprint from vLLM's step, token and preemption counters
plus its KV usage gauge (the only signal that moves through a solo prefill), from llama-server's
`n_decode_total` and token counters, or from `/slots` (task, processing flag, decoded and prompt-processed
counts; `next_token` read as the array current builds return). Request gauges and the finished-request counter
(it counts aborts) never enter it, so a wedged engine that still accepts requests cannot look alive.

- The engine moved since the last look: the run is held in the new phase `queued` and re-reads every 10 s for
  as long as the engine keeps working, bounded by the run's ceiling. The same check runs in the post-ready
  window after a cold load.
- The engine did no work for max(120 s, the waiting phase's own allowance): that is the stall, and the reason
  names the engine's silence, the phase and the last reading.
- The engine keeps stepping but produces no token for 3× that bound: a preempt-and-recompute thrash stall.
- The engine cannot be read at the first look: the ADR 0055 rule, and the reason says so. Inside a hold an
  unreadable read is no new evidence (a llama-server answers only between batches); the hold lasts until the
  flat bound runs out since the engine last moved, then files an unreadable stall that says it was held. One
  read may take 45 s. A loading seat goes to the cold-load hold, whose clock starts when the load is seen.
- On entering the hold and on every read that sees the engine move, the node publishes the flat bound plus
  one poll as the run's allowance, anchored at that read, so the delegator keeps polling a held remote run
  while the engine is seen working — and gives a node that stops re-publishing up one allowance plus grace
  later, never at the ceiling.
- `queued_ms` reports the wall a run spent held up to the engine's last movement, on the wire, the caller's
  result, both ledger rows and the call meta. A held run no longer feeds the seat's single-stream rates, which
  size every later allowance.
- A run whose ceiling passes while it is held behind the seat's other work (held at the ceiling, or held for
  at least half its time) is a CAPACITY defer that says so, not a budget defer "while producing (0 tok)": a
  larger budget would not have helped, and a budget class inflates the delegator's contract sizing.

### Fixed — local seat-cap waiters no longer block each other (C-60)

Every run registers in phase `admission` before the local seat gate, and the gate counted every other
registered run, waiters included: stacked waiters blocked each other with the seat free (92 local runs refused
on 2026-09-29 after the full admission budget). A run now counts only runs past admission plus waiters that
registered before it (FIFO), so a freed slot admits exactly the next in line. The wait in line is bounded by the
run's own wall (never less than the admission budget, never past the caller's deadline) instead of the shared
300 s admission budget, which refused 88 of those 92 after exactly 5m0s; the admission deadline moves out by
the time spent in line, so the pre-flight, the cold load and the probes keep their whole budget. Both doors
(the pipeline and `agent_run`) apply it. A run waiting in line publishes phase `admission` with a rolling 30 s
allowance (refreshed at least every 5 s and whenever the line moves), so a remote delegator follows a job the node
still holds in line instead of abandoning it, and gives up a node that stops reporting one allowance later. `agent_run` registered its run
without the admission phase, so each one held a slot while it waited; it now registers in admission like the
pipeline, and a test fails any `gpuactivity.Start` site that registers a gated run outside it.

### Fixed — a delegator-local job is one ledger job, not two (C-62)

A route=local job wrote the pipeline's inner `agent` row beside the delegator's `agent_delegate` row and every
reader counted both (job counts, success rates, `cards_tokens`, `TokensOut` ~2x for local work). The delegator
now hands its job id to the runner (`parent_job_id`); the inner row carries `cards_tokens` 0, and every job
counter — the summary, the defer tally, `report`, `loupe` and the audit sample — counts the job once on its
parent while keeping the inner row's savings. An inner row whose parent row never landed (a failed write, a
process killed between the two) is that job's only record and counts as the job.

### Fixed — `node-swap` restarts and proves Linux nodes; the PS 5.1 launcher detects support

A `--restart-command` ran through `powershell` on every OS ("executable file not found in $PATH" on Linux) and
the post-restart proof polled a process finder that is empty off Windows, so a Linux swap with a restart
mechanism could never verify. Restart commands now run through `/bin/sh` off Windows and verification reads
`/proc/<pid>/exe` (another user's node, whose exe link is unreadable, is matched by its cmdline; one that
cannot be ruled out is an error, never "no node"). `windows-node-swap-launch.ps1`'s support probe ran under
`$ErrorActionPreference = 'Stop'`, where Windows PowerShell 5.1 turns the supporting build's own stderr usage
line into a terminating error and read the build as unsupported — the launcher then ran the OLD installed
engine; the probe now runs under a function-local `'Continue'`, and the launcher's JSON reports
`runner_exe`, `runner_fell_back` and the probe's evidence so a deploy can fail on a fallback.

### Fixed — `gpulease.Lease.Release` is safe to call concurrently

`Release`, `Check` and `Restamp` read and wrote the lease's released flag unsynchronized; a holder's deferred
release beside a cleanup path raced on it (`go test -race`, reproduced on the previous release). The flag is
atomic and exactly one caller releases.

### Fixed — a crashed vLLM seat restarts instead of refusing on its own stale MP server

When a vLLM engine died, llama-swap restarted the seat through `seat_fg.sh` without running `seat_stop.sh`, so
the dead generation's LMCache MP server still held its HTTP port and every restart refused on it (2026-09-29:
23 minutes with an agent seat down, 59 failed starts, 75 HTTP 500s). With no engine of the stack serving its
port, `seat_fg.sh` now runs the stack's own `seat_stop.sh` once, prints its exit code, waits up to 10 s for the
port to free and starts; a holder that survives the cleanup is still refused.

### Changed — the 8 GB agent seats get a 2,048-token step budget

`ampere-8` and `blackwell-8` seeded no `agent_max_tokens`, so their MiMo agent seats ran the loop default
(1,024 per step, 4,096 final) and 10 of 200 runs on the ampere-8 reference box ended with the final answer cut
at those caps. Both tiers now seed 2,048 (final 8,192), the geometry the same model already runs on `amd-gcn`.

## [0.142.1] - 2026-09-29 - the browse lane waits for the page to finish saving before it closes its tab

### Fixed — an edit made by a run's last action was lost when the tab closed

A live run on a production web editor typed a post title, reported `done`, and the title was never stored: the editor
autosaves on a debounce (measured 2.3 s after a keystroke) and the sidecar closed its tab at DONE, before
the save left. The sidecar now holds the tab open until the page's XHR/fetch traffic has been quiet for
3.5 s (capped at 15 s) and keeps feeding any capture while it waits. Measured on the same draft: with the
settle the typed title was stored (one request watched, 6.1 s); without it the identical step reported
`done` and the title was not stored.

## [0.142.0] - 2026-09-28 - `browse_cdp_url` pins the browse lane to a dedicated agent browser

### Added — `browse_cdp_url`

The first run against a real main browser profile timed out in the WebSocket handshake: recent Chromium
builds' per-instance remote-debugging toggle serves no `/json/version` and asks the operator to approve
every new debugging connection, so an unattended lane can never attach. `browse_cdp_url` (loopback
`http://` or `ws://` with an explicit port, validated like `browse_decision_url`; anything else leaves the
lane unregistered with a named warning) pins the sidecar to one endpoint — a dedicated agent profile
started with `--remote-debugging-port` and its own `--user-data-dir`, which does not prompt and keeps the
agent out of the operator's everyday profile. It wins over `browse_browser`; the start line carries it
as `cdp_url` and the sidecar maps it to browser-harness's `BU_CDP_URL` / `BU_CDP_WS`.

## [0.141.1] - 2026-09-28 - research acceptance stops being built from page chrome (register C-65)

### Fixed

- `offload_research` acceptance no longer fails faithful digests. The page check used to tokenize the
  whole stripped page and rank identifier-shaped tokens first, so CSS classes, sidebar ids, script
  names, handles and URL slugs (one cut mid-word at 40 characters) became the anchor, and the two
  fingerprint halves on forum pages were UI words; all three checks had to pass. Over a replay of 457
  completed research runs from the delegation log, 35.0 % passed the recorded checks and 71.1 %
  pass the new one on the same outputs (the rest are shape-check failures and answers that restate
  none of the page's words). `internal/research` now builds ONE any-of `docanchor` regex from
  prose lines and headings only: no token with an underscore, digit, camelCase or two-plus hyphens,
  no UI or markup vocabulary, no goal word, never a truncated token, the top 24 words. Quarantine
  semantics are unchanged (it keys on the `docanchor` tag). `research.DocFingerprint` is removed.

## [0.141.0] - 2026-09-28 - the opt-in browse lane: `offload_browse` and the agent `browse` tool drive the operator's own browser

### Added — the browse lane (ADR 0060)

An opt-in lane that drives the operator's already-running, logged-in Chromium browser toward a
natural-language goal. Off by default: no `browse_*` key is seeded, and `offload_browse` is registered
only when `browse_python`, `browse_script` and a loopback `browse_decision_url` are all set.

- **MCP `offload_browse`** (`url`, `goal`, `max_actions`, `allow_hosts`, `allow_labels`, `capture`,
  `route=local`). Typed decisions are proxied to the loopback decision endpoint (plain `http` on a
  loopback host only; anything else leaves the lane unconfigured and warns at load); field values are
  written by the local seat under a raw GBNF grammar. The harness holds no key: the endpoint's bearer
  comes only from the `LOCAL_OFFLOAD_BROWSE_BEARER` environment variable and never reaches the sidecar.
- **Agent `browse` tool** (`url`, `goal`, `max_actions`, `security_risk`). CLI door:
  `local-agent --allow-browse --browse-hosts host1,host2`. Only the MCP tool is attended. Every agent
  door is unattended for browse: the CLI (which always builds unattended), `agent_run` and
  `agent_delegate` (`allow_browse` and `browse_hosts`) each require a non-empty host list and an audit
  path, and `allow_labels` can never lift the deny-list. For contracts, `route` must be `local` (intake rejects otherwise),
  delegation placement never sends the contract to a remote node, and a fleet node refuses it at ACK
  unless its config has `agent_allow_browse: true`. The policy broker gains the action kind `browse`
  (subject: the start URL's lowercased host), and every agent-door call is audited.
- **Config keys:** `browse_python`, `browse_script`, `browse_decision_url`, `browse_browser`
  (`chrome`|`edge`|`brave`|`chromium`|empty), `browse_timeout_sec` (300), `browse_max_actions` (30,
  ceiling 60), `browse_capture_dir`, and the node opt-in `agent_allow_browse` (default false).
- **Sidecar `setup/browse/`:** browser-use's `jev-ultrafast` loop over `browser-harness` (both pinned,
  `uv sync --frozen`), an allowlisted environment with telemetry forced off, a lane-only daemon name
  stopped after every run, a deny-list applied before the model sees a control and again at execution,
  redacted network capture, and typed defers (`BAD_INPUT`, `CLI_MISSING`, `BROWSER_UNAVAILABLE`,
  `DECISION_UNAVAILABLE`, `TEXT_UNAVAILABLE`, `HOST_NOT_ALLOWED`, `DENIED`, `TIMEOUT`,
  `RUNNER_FAILED`). Installed with `setup/browse/install.ps1`.
- **What leaves the machine:** the visible page text and element labels go to the decision endpoint,
  which the operator runs and which may front a hosted decision model. ADR 0060 amends ADR 0001's
  "never a cloud call" reading for this lane only; `offload_nim` stays the only remote model surface.
- Docs: ADR 0060, `docs/systems/browse-lane.md`, and the MCP server, coding-agent, operator-guide,
  README, CONTRIBUTING and glossary entries.

### Changed — the `offload_status` golden changed on purpose

`offload_status`'s `remote` block gains `browse_configured` and `browse_decision_url`, and its `note`
now names `offload_browse` beside `offload_nim`. The full-payload golden
(`TestStatusDefaultIsByteIdenticalToTheGolden`) was regenerated deliberately with
`OFFLOAD_UPDATE_GOLDEN=1`.

### Removed — the amd-gcn tier's CPU alt route (`alt_backends: ["cpu"]`)

Operator order, 2026-09-24: no model runs on CPU. `amd-gcn` no longer declares the CPU seat
family, so `install render --llama-bin-cpu` refuses on it by name and a re-render cannot bring
`offload-e4b-cpu` / `gemma4-e2b-cpu` back. The node that carried them had the two entries, the
`ldcpu` macro and their matrix vars removed from its live config and `alt_backends` dropped from
its manifest; its `/fleet/health` now reports `backends: ["vulkan"]`. ADR 0054 is amended, not
superseded: the declare-render-advertise mechanism stays for a non-CPU alternate.

### Added — Wan 2.2 native loader (fp8-scaled safetensors, no MultiGPU) as a supported variant

`render/wf-wan22-i2v.mjs` used to wrap EVERY Wan expert (GGUF *and* safetensors) in the
DisTorch2/MultiGPU loader. `videogen_wan_loader` ("" / "auto" | "native" | "gguf-distorch")
now selects per box (or per `videogen_families[wan22]` override): "auto" keeps a `.gguf`
expert on the historical DisTorch2/MultiGPU wrapper (DynamicVRAM streaming cannot load GGUF)
but a `.safetensors` expert now loads through the plain, un-wrapped `UNETLoader` — no
`virtual_vram_gb`, ComfyUI's own dynamic-VRAM streaming does the offload instead (measured
~27.6% faster wall-clock than the GGUF/DisTorch2 path, "Interim Phase 2 round 2" item 1,
`bigger-models-2026-09-24.md`). "native" forces the plain loader (refused on a `.gguf` expert
with a named error); "gguf-distorch" forces the historical wrapper on both experts regardless
of extension — the escape hatch for a card too small for native streaming. `--fast` and
`--upscale` work identically on both paths. `internal/mediacap/routeneeds.go`'s doctor
preflight now reports the correct node-class requirement (none, for a native safetensors
expert) instead of always demanding the MultiGPU pack.

### Added — LTX-2.5 transformer per-request override (`--transformer` / MCP `transformer`)

The study's recommendation (int8 default, bf16 opt-in for a hero render) needed a way to pick
the transformer file per request, not just per config edit. `generate-video`'s CLI and the
`offload_generate_video` MCP tool now accept `transformer`, which wins over this box's (or the
resolved family's) bound file for that one render; a no-op on any family other than `ltx25`.

### Fixed — `videogen_text_encoder` and its siblings were not family-scoped

Every `videogen_*` weight/behavior key (text encoder, transformer/unet high/low, VAEs, latent
upscaler, wan_virtual_vram_gb, frames/fps/width/height, the upscale model) was a single
machine-wide value applied to WHICHEVER family actually rendered — so a request's documented
`model` override (e.g. `wan` on an `ltx25`-bound box) could silently hand that other family's
transformer/text-encoder to the graph it built ("Interim Phase 2 round 2", `bigger-models-
2026-09-24.md`: exactly this, worked around by hand-patching the graph builder and passing
`--text-encoder` explicitly). Added a `videogen_families` config block (keyed like
`imagegen_families`, but without the licensing overlay — video family selection has never
been a licensing feature): an EXPLICIT `videogen_families[name]` entry, when present, now
wins outright for any family other than this box's own default — the opt-in fix for a
specific cross-family leak. Absent one, resolution falls back to the flat `videogen_*` keys
unchanged, which is deliberate back-compat: it preserves the pre-existing, documented pattern
of a box's flat keys intentionally carrying a DIFFERENT family's weights as a fallback (e.g.
Wan GGUF files left bound on `videogen_unet_high`/`_low` on an `ltx25`-seated box specifically
so a bare `model:"wan"` override still renders with them). A config that has never set
`videogen_families` is therefore byte-for-byte unaffected; an operator closes a leak by
adding only the keys that need their own family-distinct value. `doctor` and `offload_status`
(`media.video_family_bindings`) now report the resolved per-family bindings by name.
`internal/config/families.go` (`VideoFamilyBinding`, `ResolveVideoFamilyBinding`),
`internal/mediacap/routeneeds.go` (`VideoFamilyBindingRows`), `internal/pipeline/pipeline.go`.

### Fixed — a ComfyUI boot that made no progress could hang up to ~10 minutes with no signal

`ensureComfy` (`render/comfy-lifecycle.mjs`) spawned `python main.py` and then only ever
polled the HTTP health check — a child that failed to start (bad cwd/python path — reproduced
via the `<node-e>` fp8-native study's direct `comfy-video.mjs --graph` invocation, "Interim
Phase 2 round 2" item 4) or crashed on the way up looked identical to a slow cold boot: no
process tree, 0% GPU, port never open, the GPU lease held the whole time, and the loop kept
silently polling for the entire `COMFY_START_WAIT_SEC` budget (default 10 min) before failing
at all. `ensureComfy` now listens for the child's own `error`/early `exit` and fails within one
poll interval, naming the spawn command, the failure detail, and the tail of this run's own
ComfyUI console log (PR #471) — a boot that makes no progress now fails loudly and fast
instead of reading like an unusually slow one.

### Added — `render/comfy-render.mjs` self-manages its ComfyUI lifecycle (launch, reuse, GPU slot, teardown)

Unlike `comfy-video.mjs`/`comfy-edit.mjs`, this runner only ever called `waitServer()` and
required an already-running ComfyUI — the qwen-image-2512 bf16-vs-GGUF A/B (`bigger-models-
2026-09-24.md` item 3) failed twice through the normal path with "RENDER FAILED: ComfyUI not
reachable" before ComfyUI was booted by hand. It now wraps its render in the same
`withGpuSlot` lifecycle every other runner uses (launch on demand, reuse an already-fit
instance, take the shared GPU lease, tear down after) when run standalone. A new
`--no-lifecycle` flag keeps `comfy-generate.mjs`'s existing wrapped-child invocation
byte-behavior-identical (its own `withGpuSlot` already owns the lifecycle for that call path —
without this flag, this file's own teardown would free/unload the model after every job in a
`--batch` session, defeating the warm-session optimization).

### Added — `local-offload node-swap` gaps closed: Linux launcher, standalone GPU-lease wait, auto-resolved health URL, doubled backup-suffix fix

(`deploy-d5207011.md` findings.) (a) `setup/linux-node-swap-launch.sh`, the detached-launch
sibling of the Windows PowerShell launcher (`setsid nohup ... &`, the same
`local-offload node-swap` engine, no separate hand-rolled polling script) — `<node-c>`'s own
ad-hoc deploy script hardcoded `curl http://127.0.0.1:.../fleet/health` while fleet-serve
there binds only its tailnet address, and an unguarded fallback read every failed poll as
"still busy" for ~46 minutes; `--health-url` is now also auto-resolved from this node's own
config `fleet_listen` when left unset (`resolveNodeSwapDefaults`, `node_swap_cmd.go`) — never
from a guess, and never when that address is still loopback/wildcard. (b) A standalone node
(no `--health-url`) now waits for its own GPU lease to clear before swapping
(`internal/nodeswap.waitGPUFree`, the same `internal/gpulease` resolution `gpu status` uses) —
previously the caller had to check `gpu status` by hand. (c) `backupPathFor` strips one
redundant leading `bak-` from an operator-supplied `--backup-suffix` before prepending its
own, so a suffix that already reads `bak-...` (the exact d5207011 `<node-b>` deploy mistake) no
longer doubles into `<target>.bak-bak-...`.

### Fixed — pooled krea2/LTX-2.5 text encoder and VAE no longer load onto the display card

The stock `CLIPLoader`/`VAELoader` nodes' `device` input only ever offers `"default"`/`"cpu"`;
`"default"` is ComfyUI's own fastest-first device pick, entirely independent of
`imagegen_pool_compute`/`imagegen_pool_donor` (or the `videogen_*` equivalents) — on the 3x16
reference tier that IS the display card. Measured A/B (2026-09-24,
`multigpu-archival-actions-2026-09-24.md`): every pooled krea2 render put the ~12 GiB
Qwen3-VL-4B encoder there regardless of which two cards the DiT's own pool keys named. Fixed
in `render/wf-krea2.mjs` and `render/wf-ltx25-i2v.mjs`: when pooled, the text encoder and
VAE(s) now load through ComfyUI-MultiGPU's `CLIPLoaderMultiGPU`/`VAELoaderMultiGPU`, pinned to
the pool's donor device — never the unnamed default. Unpooled builds are unchanged.
`render/comfy-nodes.mjs` (`NODE_PACKS`) and `internal/mediacap/routeneeds.go`
(`nodePacks`/`classPacks`/`imageNodeClasses`/`videoNeeds`) now name the two new node classes.

### Fixed — foreign-GPU-memory warning no longer fires on an ordinary desktop session

`gpu status`/`gpu reserve`'s foreign-GPU-memory warning (PR #469) flagged the operator's own
desktop apps (Code.exe, chrome.exe, SnippingTool.exe, explorer.exe, ...) on `<node-b>` because
the Windows PDH source it reads has no per-card identity and the noise floor (64 MiB) let
ordinary desktop residents through. Fixed: the per-process floor rises to 512 MiB (configurable
via `foreign_gpu_min_mib`), the OS shell/desktop denylist grows (csrss, explorer, and the
shell-experience hosts), and — the actual multi-card fix — a resident whose every known GPU is
a display card (`gpuprobe.DisplayCardUUIDs`, cross-referenced against nvidia-smi's compute-apps
`gpu_uuid` per pid) is dropped, but only on a box that has a non-display card to actually score
(a single-GPU box still warns on a Resolve-class hog on its only card). `gpu_foreign.go` /
`gpu_foreign_test.go`.

### Changed — ComfyUI-MultiGPU pinned + mirrored ahead of its 2026-09-30 archival

`pollockjj/ComfyUI-MultiGPU` archives 2026-09-30 (issue #223, no successor endorsed) with no
native ComfyUI-core replacement for donor+compute `virtual_vram_gb` weight-sharding (the Wan
GGUF lane and the pooled krea2/LTX-2.5 seats depend on it). Pinned the fleet to
`ed1ffaef7cec1a66f35106c6a4c7a40927c2dc83` (upstream v2.6.4's last code commit `b51c99a5` plus
an already-deployed local Windows P2P/cudart fix) and created a private fallback mirror,
`dmmdea/ComfyUI-MultiGPU-mirror`, for when upstream goes read-only. `render/comfy-nodes.mjs`
and `internal/mediacap/routeneeds.go` now name the pin + mirror in the `MISSING_NODE` hint.
Docs: `docs/systems/media-generation.md` "ComfyUI-MultiGPU archival"; full research and the
per-node alignment/A-B record live on the operator's Drive under
`Ecosystem/Benchmarks and Optimizations/2026-09-22-qwen-image-21-hyperframes/`
(`research/comfyui-multigpu-archival-2026-09-24.md`, `infra/multigpu-archival-actions-2026-09-24.md`).

## [0.140.12] - 2026-09-24 - blackwell-8 and ampere-16 tier seeds carry the 2026-09-23/24 reference-box measurements

### Changed — blackwell-8 and ampere-16 tier seeds carry the 2026-09-23/24 reference-box measurements

Both reference boxes (`<node-e>` / blackwell-8, `<node-c>` A2 / ampere-16) got a harness
remediation pass on 2026-09-23/24; a fresh install of either tier now gets what was measured
instead of the prior provisional defaults. `docs/tiers/ampere-16.md` and `docs/tiers/blackwell-8.md`
regenerated (`go run ./cmd/gentiers`).

- **blackwell-8** (`config_seed_ram_mid_high` unless noted): `imagegen_timeout_sec` 900 -> 2400
  (the qwen-image-2.1 binding's own measured max wall is 678.1s, so 900 left only ~1.3x margin);
  `sdcpp_extra_args` added for z-image-turbo, the measured-working graph-cut arm
  (`--vae-tiling --offload-to-cpu --diffusion-fa --max-vram 6.5 --stream-layers`, 56-69s/1024²
  image) after two failed arms (bare `--vae-tiling` silently ran on the Intel iGPU; an all-VRAM
  placement OOM'd at 565-608s/step); `videogen_wan_virtual_vram_gb` added at 12 (measured: 7 OOMs
  at lower values under the no-sysmem-fallback policy); `videogen_timeout_sec` 3600 -> 4500 =
  ceil(2,970.3 × 1.5) from a measured cold native 81-frame render; `audiogen_timeout_sec` added
  at 1500 in `config_seed` (was unbound), sized from a measured cold ACE-Step render + its
  dead-air retry + loudnorm (~870s measured, ×1.5 rounded up); `imagegen_families` adds two named,
  opt-in bindings beside the default pair (ADR 0058): `qwen-image-2.1` (the canonical bf16 overlay,
  A2 evaluation CLEARED 16/16) and `qwen-image` / Qwen-Image-2512 (Apache-2.0, commercial; measured
  190.9s at 1328²).
- **ampere-16**: `videogen_wan_virtual_vram_gb` added at 9 (the tier declared Wan I2V keys but
  never this one, so a fresh install silently rendered the graph script's own default of 7); a
  live smoke render measured a peak of 9.6 of 15.0 GiB across the full render at 9 GiB, 5.4 GiB
  headroom, zero OOM — a measured-safe floor, not yet a measured optimum.

Confirmed unchanged, already correct: blackwell-3x16's `comfy_cuda_device` `"2"` and
`videogen_pool_compute` `"cuda:0"`. Not touched: ampere-8's `comfy_extra_args` temp/output-dir
redirect (measured, but a literal per-box drive path, not a tier-general rule) and `ffmpeg_path`
on any tier (PR #471 already makes a bare `"ffmpeg"` resolve via PATH).

### Fixed — compact-desktop parity: doctor checks what a route loads, the 26B download honours the tier, `generate-video --fast`, the Wan lane defers cleanly, a BOM config loads

Five harness defects from `<node-e>` (blackwell-8) media parity audit, 2026-09-23.

- **`doctor` (and `offload_status` / `acceptance`) no longer call a route CONFIGURED because its script
  exists.** `generate_video`, `animate_character` and both `generate_audio` kinds now also check what
  the script loads (`internal/mediacap/routeneeds.go`): the model files of the route's graph (bound keys
  AND the builder defaults an unset key falls back to, per `videogen_family`), the custom-node classes
  the graph names (`VHS_VideoCombine`, the DisTorch2 loaders, `UnetLoaderGGUF`; also on the image/edit
  routes), and for voice the python `tts.mjs` spawns, its `chatterbox` + `torch` packages and the
  Chatterbox weights in the HF cache. Every missing item is named on the route's line (class + pack +
  URL, file + class directory). `doctor` asks a ComfyUI that is already running (`/object_info`) and
  falls back to `<comfy_dir>/custom_nodes` on disk; nothing starts ComfyUI or loads a model.
  `TestRouteNeedsMirrorTheRenderBuilders` pins the Go copies to the `render/wf-*.mjs` defaults. The
  default `offload_status` payload changes deliberately: `media.routes` details carry the same checks
  (golden `offload_status_default.golden.json` regenerated for the `generate_video` detail only).
- **`install.ps1` no longer downloads the Gemma 26B on a tier that drops it.** Step 5 added
  `model-26b` on the family gate alone, so blackwell-8 (`include_26b: false`) fetched 14.25 GB the
  rendered yaml never serves. The profile is resolved before the download set, and
  `Get-FamilyModelKeys` adds the 26B only when the resolved `include_26b` (RAM gate included) is true.
- **`local-offload generate-video --fast`**: the CLI now carries every `offload_generate_video`
  option; the distilled Wan recipe was MCP-only. Stale help text (`wan 4 fast / 20 hero`) corrected.
- **`render/comfy-video.mjs` parsed `--fast` as a value flag**, so the harness's trailing `--fast`
  read as undefined and rendered the native recipe, and `--fast --upscale-model X` swallowed the
  upscale flag. It is a boolean now; the runner is import-safe with tests (`comfy-video.test.mjs`).
- **The Wan lane defers cleanly on a missing custom node.** Before the POST the runner asks the running
  ComfyUI for every class the graph names (`render/comfy-nodes.mjs`); a missing one is a one-line
  `MISSING_NODE: … VHS_VideoCombine (custom node pack ComfyUI-VideoHelperSuite …)` defer and nothing is
  submitted. The failure path sets `process.exitCode` instead of `process.exit()`, which tripped the
  Windows libuv `UV_HANDLE_CLOSING` abort (exit 0xc0000409) on the reg3 run.
- **New key `videogen_wan_virtual_vram_gb`** (default 7 = the builder's old constant): the Wan DisTorch2
  `virtual_vram_gb` is per card, so each node sets its measured value; the pipeline passes it as
  `--wan-vvram-gb`. 7 OOMs an 8 GB card under "Prefer No Sysmem Fallback". A negative value is a config
  finding. Separate from `videogen_pool_vvram_gb` (LTX-2.5: VRAM borrowed from a donor card).
- **A config.json saved with a UTF-8 byte-order mark loads, and a config that cannot be parsed says the truth.**
  PowerShell 5.1 writes a BOM; `encoding/json` refused it, and the warning then said "the file's other settings
  ARE in effect" while the process ran on built-in defaults (a run-graph on `<node-e>` did exactly that). The
  loader strips a leading BOM (`config.StripBOM`, also used by `audit-config` and local-agent's `--env-rules` /
  `--setup` files). A file that still does not decode is a `config.ParseError`: the value is exactly the built-in
  defaults (a JSON type error used to leave a half-read file), and the stderr warning, doctor's `config:` rows and
  fleet-serve's refusal all say nothing from the file is in effect. Validation failures keep their own wording.

### Fixed — the launcher's dxg warning reports what is new, not everything since the distro started

- `seat_fg.sh` counted `make_resident: Ioctl failed: -12` / `reserve_gpu_va … -75` over the whole kernel log, so after
  one failed WSL2 start every later start repeated "the kernel log holds N failures … restart the distro", including
  starts that serve normally (measured 2026-09-23: 56 failures from three failed vLLM 0.30 starts, then a healthy 0.28
  start that still printed the warning). The count now lives in `$WORK/.dxg-failures`; a start warns only about
  failures new since the previous start (which is when a failed start leaves them), resets when the log shrinks (the
  distro restarted), and otherwise prints a one-line note. The comment now says what the lines mean: Windows refused
  residency, and CUDA reports it as "out of memory" at an unrelated call.

### Added — the LMCache overlay kit ships with the seat template; two launcher knobs

- **`setup/templates/vllm-seat/lmcache-patches/`: the patch set a pipeline-parallel vLLM seat loads through
  `SEAT_LMCACHE_PYTHONPATH`, and the script that builds it.** Base LMCache 0.5.5: #4253 (fp8 stores on hybrids),
  the per-rank L2 layout patch, register-time layout binding, and backports of #4709 and #5249, plus a CPU-only
  smoke test. `repatch-lmcache-overlay.sh` builds beside the live overlay, skips a patch already in the base,
  checks one import marker per patch and the smoke test, records each patch's sha256, and swaps only while no
  MP server has the old tree loaded. The kit is named apart from the overlay it builds (default
  `<seat dir>/lmcache-overlay`) so a build never swaps its own kit away. `.gitattributes` keeps it LF.
- **`SEAT_KV_LOAD_FAILURE_POLICY` (seat env, optional):** `recompute` or `fail`, spliced into
  `--kv-transfer-config`; empty leaves vLLM's default (`fail`). An invalid value refuses the start before the MP
  server is touched. The kit's deploy gate now reads it from each seat env that names the overlay (it used to
  grep the launcher for a hard-coded policy).
- **`SEAT_MP_EXTRA_ARGS` (seat env, optional):** extra `lmcache server` arguments, appended after the launcher's.
- **The launcher reports two hazards at start, without refusing:** dxg residency / VA failures in the distro's
  kernel log (`make_resident: Ioctl failed: -12`, `reserve_gpu_va … -75`), and the count of `Store task … failed`
  lines the unit's previous MP generation left in `lmcache-mp.log` (a per-unit offset file).

### Fixed — the 0.28 V2-runner record

- The launcher comment, ADR 0048 and the runner test said the production pair seat logs `Using V2 Model Runner` on
  vLLM 0.28.0. Those lines are `gpu_worker.py:429`, which exists only in 0.29.0; the production seats serve on
  0.28's V1 runner. The version gate stands: 0.28's V2 runner does run on WSL2 (the TP2 DFlash spec-decode arms
  logged `gpu_worker.py:396`, the 0.28 line, and served).

### Added — opencode context instrument

- **`go run ./cmd/opencode-context`: the before/after gate for what opencode sends a seat.** It copies
  opencode's session db with its `-wal` and `-shm` (copying again when a live writer lands mid-copy;
  `quick_check` on the copy; the source is never opened by SQLite), then reports per session (primary or
  child, by `parent_id`) and agent: first-call prompt, per-call prompt / cached / output / reasoning,
  cache-read % overall, excluding session-first calls and excluding cold first calls, the split of
  prompt growth into replayed reasoning, output, tool output and user text, compaction events with the
  prompt before and after, TTFT and the model id — as a table or `--json`, with no session content.
  `--cache-valid-since [MODEL=]TIME` drops reporting artifacts (a seat logs cached 0 until
  `--enable-prompt-tokens-details` is live) and `--cache-block` checks whole-block cached counts.
  Read from the 2026-09-22 db it reproduces the hand audit: first calls 11,773 / 12,265 (main) and
  24,680 (offload), 66.0% / 92.2% cached. Tests run on a synthetic fixture built on opencode's DDL
  (`internal/occontext/testdata/schema.sql`); `modernc.org/sqlite` becomes a direct dependency (it
  was already in `go.sum`). Docs: `docs/systems/opencode-integration.md` → "Measuring context".

### Fixed — seat gauges never reset the idle timer; opencode sends one system message; seat renderer catches up

- **The PAIR seat watcher kept every vLLM seat loaded forever.** It polled
  `/upstream/<seat>/metrics` through llama-swap every 2 s, and llama-swap counts every `/upstream`
  request as activity, so the seat's 300 s idle unload never fired. The watcher now reads the
  seat's `/metrics` at the seat's own address — the `proxy` that llama-swap's `/running` reports
  for the model it lists as ready (never a port from config: two vLLM seats can share one port).
  The same fix lands in `seatload.Inflight`, the shared gauge reader behind the drain, the spread
  deal, `gpu status` / `offload_status`, the cascade lane busy probe and the placement snapshot;
  the `/slots` fallback reads the seat directly too, and a `/running` entry with no `proxy` is
  "could not read" (`seatload.ErrNoSeatAddress`), never a fall-back through `/upstream`. The warm
  watch after a lease reads `/running` only. Audit of every other `/upstream` reader: the agent
  window/pin probes, the KV slot lane, the tokenizer and STT clients, the tier re-pack probe and
  the warm-up all act for the request about to run on that seat; `offload_status`'s context-window
  read is one-shot and operator-initiated (documented at the call). Tests assert the watcher and
  `Inflight` never request an `/upstream` path.
- **opencode sent two leading system messages.** The plugin's `experimental.chat.system.transform`
  pushed the protocol as a new element; opencode folds the array only when it holds more than two,
  so a vLLM seat serving the model family's upstream chat template answered 400. The protocol is
  now appended to the last existing element (pushed only into an empty array); the length is
  unchanged and the marker appears once.
- **vLLM seat schema: `chat_template` and `enable_prompt_tokens_details`.** A seat can name a
  chat template the repo ships (rendered as `templates/<name>` beside the seat env, passed as
  `--chat-template <installed path>`) or an absolute path on the serving box, and can enable
  `--enable-prompt-tokens-details`. Ships `setup/templates/vllm-seat/chat-templates/qwen3-fold-system.jinja`:
  the Qwen3-family template plus a block that folds 2+ leading system messages into one; checked
  with the transformers Jinja environment to render single-system and no-system conversations
  byte-identically to the upstream template. `*.jinja` is LF by `.gitattributes`.
- **blackwell-3x16 tier text caught up with the 3-card agent seat:** `l1_staging_gb` 8 -> 16, the
  seat renders the fold template and prompt-token details, and the stale notes (an OPEN 0-hit
  pipeline note, `agent_ctx_tokens 163840`, "the agent seat stays the 2-card pair", "the TRIPLE
  layer ships no seat") now say what runs: the 3-card pipeline seat is the agent seat at 262,144
  context, L1 16 GB, serving L2 hits after the per-rank layout overlay patch, with the first
  request after an MP server start still getting 0 L2 hits until register-time binding lands.
  OPERATOR-GUIDE and `docs/systems/cache-server.md` say the same, and correct the attention layers
  per rank to 7/3/6 for a 28,13,23 split (not "6/5/5 in any split").
- **Launcher (`setup/templates/vllm-seat/seat_fg.sh`):** the VRAM precheck runs after the old MP
  server unit is stopped and before the new one starts (it measured the new server's own CUDA
  contexts and burned the full wait every start), and honours a per-device floor
  `SEAT_VRAM_FLOOR_MIB_<index>`; `seat_stop.sh`'s VRAM readback honours it too. The seat env
  template exports `VLLM_EXECUTE_MODEL_TIMEOUT_SECONDS=120` and names a per-seat
  `SEAT_L2_STATUS_FILE` (`seat-l2-<seat id>.status` beside the env). **Migration:** after
  re-rendering a seat, repoint its `kv_cache_server` binding's `status_file` from `seat-l2.status`
  to the per-seat file. The old file is no longer written, so status would show a frozen verdict
  or "unreadable". The `status_file` comment, the `offload_status` `reachable_note` and the docs
  name the per-seat file; the `seat_fg.sh` header now describes the pipeline seat serving L2
  through the per-rank overlay (the first request after an MP start still gets 0 hits) and fp8 KV
  at 262k on three cards.
- **Gauge reads on a box whose endpoint names itself.** `seatload.SeatURL` re-points a loopback
  seat `proxy` at the endpoint's host only when that host is ANOTHER machine. An endpoint that is
  this machine's hostname, or resolves to one of its interface addresses (a MagicDNS name for
  itself), keeps the loopback proxy: the reference seat binds 127.0.0.1 only. The same-machine
  answer is cached per host (5 min; a failed lookup 30 s), so the 2 s seat-watcher poll never pays
  a lookup timeout on every read.
- **Gauge reads behind a llama-swap on another machine say why they fail.** The re-pointed address
  reaches only a seat bound to a routable address there; a loopback-bound seat refuses, and
  `Inflight` now reports `seatload.ErrRemoteSeatUnreachable` (the drain prints it at its deadline,
  `gpu status` / `offload_status` show it as the seat's error) instead of a bare "connection
  refused". `/upstream` stays excluded as a fallback: it resets the idle unload timer.
  `ErrNoSeatAddress` now says the llama-swap build is too old to report `proxy` and to upgrade it.
- **The alias roster is read over the caller's HTTP client** (`swapclient.FetchRosterWith`), like
  `/running` and the gauge — one transport per observation.
- **`seat_fg.sh` refuses a missing `--chat-template` file** before it touches the MP server, naming
  the file; the rendered env header and OPERATOR-GUIDE say to copy the render's `templates/` into
  the WSL seat directory. `docs/systems/gpu-lease.md` and `docs/systems/pair-workloads.md` no longer
  describe the gauge read as `/upstream/<seat>/metrics`. The OPERATOR-GUIDE binding examples name the
  rendered per-seat status files (`seat-l2-<seat id>.status`).
- Comments: the default L1 staging is 2 GB (`EffectiveL1StagingGB`), not 8; the 3-card box's
  agent alias is `agent-pool`.

### Changed — opencode integration 0.2.0: the primary agent sees only the Tier-1 tools

- **opencode loaded every harness tool schema into every local session.** opencode has no
  deferred tool search, so the ~25 harness MCP tools cost a 64k local primary ~18k tokens before
  the first message. Measured on opencode 1.18.31, first-turn input tokens: all tools **30,856**,
  Tier-1 only **12,528**, no harness MCP **11,661**.
- New plugin option `primaryTools` (default `"tier1"`). The `config` hook writes `permission`
  rules — `<mcp>_*` deny, the four Tier-1 tools allow (broad deny first: rules apply in order),
  and `<mcp>_*` allow on the offload subagent, including a user-defined one the plugin did not
  create. Keys the user already set are never touched. `"all"` restores the previous behaviour.
- The injected protocol has a Tier-1 variant: the four tools directly, everything else as `task`
  legs to the offload subagent. It never names a tool the primary cannot see.
- Fixed while wiring it: the protocol de-duplication keyed on the literal
  `three-lane dispatch (house protocol`; a variant without it was injected twice per turn. Both
  variants now carry the exported `PROTOCOL_MARKER`, and the test asserts once-only in both modes.
- Tests: 55 pass. Five mutants, each confirmed to typecheck first, are all caught — including one
  that first slipped past a vacuous test comparing `protocolText()` with itself (replaced by fixed
  expectations).

## [0.140.10] - 2026-09-24 - offload_edit_image loses Spanish accents on both the PIL and GIMP routes; the class behind it also mangles PowerShell 5.1's captured output

`offload_media`'s own `ffmpeg_path` handling was checked against origin/main and found already
fixed (0.140.7, F-38) — `internal/mediaops.RunMedia`/`ResolveBinary` and `render/audio-qa.mjs`'s
`resolveFfmpeg`/`resolveFfprobe` already honour a configured path and fall back to PATH only for
a bare name, with regression tests (`TestResolveBinary_ExplicitPathMissing`,
`TestRunMedia_BareFFmpegOnPath`) already in place and green. No change needed there.

### Fixed

- **`offload_edit_image`'s "text" op garbles accented copy on the PIL route** (e.g. "VOLVERÁ",
  "ROTACIÓN", "¿"). Root cause, reproduced live: `render/edit_image.py`'s `json.load(sys.stdin)`
  decodes the UTF-8 bytes `internal/mediaops.runEditWorker` writes using the HOST'S DEFAULT LOCALE
  ENCODING (confirmed `cp1252` on a plain Windows box), not UTF-8 — the two-byte UTF-8 sequence for
  "Á" decodes to an unrelated character plus a lone surrogate for cp1252's undefined 0x81. New
  `_load_request()` calls `stream.reconfigure(encoding="utf-8")` before decoding, making the
  request plumbing locale-independent. Separately, even once decoded correctly, `ImageFont.
  load_default()` — Pillow's own bundled "Aileron" subset, used whenever a caller does not pass
  `font` — has NO Latin-1 Supplement glyphs at all (measured: Á Ñ Ü É á é ñ ü ¿ all render the
  identical empty/tofu glyph bbox), so accented text silently disappears regardless of encoding.
  `_default_text_font()` now tries a short list of common system fonts (Arial/Segoe UI on Windows,
  DejaVu Sans/Liberation Sans on Linux, Arial/Helvetica on macOS) before falling back to Pillow's
  glyph-less default. Both fixes are covered by an extended `--selftest` (run by the existing
  `TestWorkerSelftest`/`TestRunEditImage_Renditions`), proven able to fail by reverting each in
  turn.
- **The GIMP route (`instantiate_design`'s `set_text`) put caller-supplied copy directly on
  gimp-console's own argv** (`GimpArgs` embedded the whole script-fu program, text included, as one
  `-b` argument), which depends on that specific GIMP build's own Windows argv decoding being
  correct — an assumption the harness does not control. `GimpArgs` now takes a script FILE path and
  loads it (`(load "path")`); the new `WriteScriptFile` writes the script to a UTF-8 temp `.scm`
  file first, so no text ever reaches argv. `TestWriteScriptFile_RoundTripsUTF8Text` and the
  updated `TestGimpArgs` cover it; live-verified end to end against gimp-console-3.2.6 with
  "¿VOLVERÁ EL ROTATIVO? ñ ü é".
- **New package `internal/winexec`** documents and fixes the class the GIMP defect was suspected to
  share: spawning `powershell.exe` (Windows PowerShell 5.1) and capturing its stdout. Isolating the
  input and output hops separately showed the corruption is NOT in argv decoding (a non-ASCII
  `-Command` argument reaches PowerShell's parser intact — confirmed by having a script write what
  it received straight to a file) but in OUTPUT capture: WinPS 5.1's default `Console.
  OutputEncoding`, when stdout is redirected to a pipe (which `os/exec.Cmd.Output`/`CombinedOutput`
  always is), follows the host's legacy code page rather than UTF-8. `winexec.SafeScriptArgs`
  prepends `[Console]::OutputEncoding = [System.Text.Encoding]::UTF8;` and carries the script via
  `-EncodedCommand` (UTF-16LE base64, also closing off a separate script-injection/quoting hazard
  for embedded caller text). `internal/hwdetect/probe.go`'s two `powershell` call sites (GPU name,
  total RAM) now use it. `TestSafeScriptArgs_SurvivesPowerShell5OutputMangling` is a live
  regression against a real `powershell.exe` (skips cleanly off Windows, without `powershell.exe`
  on PATH, or when the current console code page happens not to reproduce the defect); proven able
  to fail by reverting `SafeScriptArgs` to skip the `Console.OutputEncoding` prefix.

## [0.140.11] - 2026-09-24 - mimo-9b-agent becomes amd-gcn's bound agent seat, and the 4B/mimo pairing that PR #472 refused now renders instead

### Changed — amd-gcn `agent_seat_tok_s` 10 -> 6.57 (the rate follows the seat)

`agent_seat_tok_s` sizes every agent contract's wall until the seat has recorded a measured rate of its
own. 10 was the qwen3.5-4b-agent seat's rate; the new seat `mimo-9b-agent` has no recorded rate yet,
so a fresh install would have sized MiMo's walls from a rate this iGPU does not reach (~1/3 too short —
the exact failure the seed exists to prevent). 6.57 is MiMo-9B's llama-bench tg128 on the reference
box (Vulkan, llama.cpp b11153). `TestAmdGcnSeedsTheSeatItWasMeasuredOn` pins it (proven red at 10).

### Changed — `include_mimo_9b` + `include_qwen35_4b` render TOGETHER; the refusal moves to a rollback alias drop

PR #472 shipped `include_mimo_9b` refusing outright alongside `include_qwen35_4b` (both claim the
shared `agent-seat` alias). The amd-gcn onboarding needed the SAME shape #472 already gave the
mimo/9B pair: render both, and strip the alias from whichever smaller entry is also present.

- **`servingtmpl.Params.validate()` no longer refuses `IncludeMimo9B && IncludeQ354B`.** A new
  `__Q354B_AGENT_ALIAS__` token (mirroring `__Q359B_AGENT_ALIAS__`) drops qwen3.5-4b-agent's own
  claim on `agent-seat` precisely when mimo-9b-agent also renders, so the smaller entry stays in the
  config as an un-aliased ROLLBACK seat instead of vanishing. The pre-existing `include_qwen35_4b` +
  `include_qwen35_9b` mutual exclusion is untouched — that pair still refuses, independent of mimo.
- Replaced `TestMimoAndQwen354BAreRefused` with `TestMimoAndQwen354BRenderTogetherQwenLosesTheAlias`
  (mirrors the existing 9B test) plus `TestMimoWithBothSmallerSeatsStillRefusedOnTheirOwnRule` (pins
  that the 4B/9B rule itself did not loosen).
- Wired the new token into every template carrying `qwen3.5-4b-agent`:
  `llama-swap.{win,linux}-cuda.yaml`, `llama-swap.{win,linux}-vulkan.yaml`.

### Added — amd-gcn (Ryzen 5 5625U / Vega 7 iGPU, Vulkan) seats mimo-9b-agent as its agent lane

MEASURED 2026-09-24 on the amd-gcn reference box (llama.cpp b11153 Vulkan, engine
upgrade from b9934 replayed clean with no regression across the whole roster): mimo-9b-agent ties
qwen3.5-4b-agent on quality within the agent-seat bake's resolution (shape B 18/18 x2 vs 17/18 x2;
shape C 4/5 x2 vs 5/5 x2) at roughly HALF the wall-clock (333 s vs 614 s total, fewer agent-loop
steps despite a slower raw decode rate — llama-bench tg128 6.57 tok/s). `amd-gcn` now sets
`include_mimo_9b: true` (keeping `include_qwen35_4b: true` as the rollback) and binds
`config_seed.agent_model` to `mimo-9b-agent`; `agent_ctx_tokens` stays 32768 (unchanged — it
already matched `ctx_size`, and continues to on the tier-driven entry below).

- **`llama-swap.linux-vulkan.yaml`'s mimo-9b-agent block now serves the TIER's own
  window/KV/flash-attn/cache-ram** (`__CTX__`/`__KV_K__`/`__KV_V__`/`__FLASH_ATTN__`/`__CACHE_RAM__`,
  the same tokens that template's `qwen3.5-4b-agent` block already uses) instead of the CUDA 8GB
  tiers' literal `65536`/`q8_0` pin: a Vulkan UMA box has no fixed 8GB-class fit to pin to, unlike
  the fixed-VRAM cards that literal was measured on. `--reasoning off --jinja` stay explicit. The
  `win-cuda` and `linux-cuda` mimo-9b-agent blocks are UNCHANGED (still the literal CUDA pin).
- **`llama-swap.win-vulkan.yaml` gained a mimo-9b-agent entry** (tier-token-driven, matching the new
  linux-vulkan shape) it never had before, plus the `__Q354B_AGENT_ALIAS__` token on its existing
  `qwen3.5-4b-agent` block. Required for OS parity, not originally scoped: `TestEveryBoundAliasIsServed`
  and `TestVulkanAndCPUTiersRenderOnLinux` both assert every "vulkan"-backend tier renders on BOTH
  operating systems ("a tier is a hardware class; the OS it boots must not decide whether it
  exists") — amd-gcn setting `include_mimo_9b` without this addition made the Windows render of the
  tier refuse outright, the exact OS-decides-capability failure those two gates exist to catch.
  `amd-rdna3` and `amd-rdna3-dgpu` (the other two `vulkan`-backend tiers) are unaffected: neither sets
  `include_mimo_9b`, so the new block strips out of their renders exactly as before.
- **REQUIRES llama.cpp >= b11102** (upstream llama.cpp #29319, same floor #472 documented for the
  CUDA 8GB tiers) — amd-gcn's pinned Vulkan build (b11153) already satisfies it.
- `TestAgentWindowMatchesWhatTheAgentSeatServes` (agent_window_test.go) now resolves a tier's agent
  seat window from the template its OWN `backend` actually renders (`llama-swap.linux-vulkan.yaml`
  for `vulkan`, `llama-swap.win-cuda.yaml` for everything else) instead of always reading
  `win-cuda.yaml` — a latent bug the mimo/vulkan combination exposed: `win-cuda.yaml`'s mimo entry
  is a LITERAL 65536 while amd-gcn's own linux-vulkan entry is `__CTX__` (32768), so the old
  always-win-cuda lookup would have demanded the wrong number for any vulkan-backend tier bound to
  mimo-9b-agent.
- `docs/tiers/amd-gcn.md` regenerated (`go generate ./...`).

### Fixed — a table-wide test assertion that pinned the refusal this PR removed

`setup/tests/install-config-seed.test.ps1` asserted `include_qwen35_4b` and `include_mimo_9b` were
mutually exclusive across the WHOLE tier table — exactly the rule this PR changes. Replaced with
amd-gcn-specific assertions (mirroring the existing blackwell-8/ampere-8 ones) and dropped the
now-false blanket check; the `include_qwen35_4b`/`include_qwen35_9b` blanket check is untouched.

## [0.140.9] - 2026-09-24 - a second 8GB-class agent seat, mimo-9b-agent, joins qwen3.5-9b-agent on blackwell-8 and ampere-8

### Added — `mimo-9b-agent`: a coin-flip-tie second 8GB agent seat, encoded exactly like `include_qwen35_9b`

A new tier flag `include_mimo_9b` gates the mimo-9b-agent seat (XiaomiMiMo/MiMo-V2.6-Distill-Qwen-9B,
bartowski GGUF Q4_K_M) into the serving yaml, mirroring `include_qwen35_9b`'s mechanism end to end
(`servingtmpl.Params.IncludeMimo9B`, `install_render.go`'s `servingProfile.IncludeMimo9B`, the
`__MIMO9B_ALT__` matrix-set token, `warnMissingGatedModels`, `install.ps1`'s `$PINNED`/`Get-GatedModelKeys`
gate). `blackwell-8` and `ampere-8` now set `include_mimo_9b: true` alongside `include_qwen35_9b: true`,
bind `config_seed.agent_model` to `mimo-9b-agent`, and raise `agent_ctx_tokens` 32768 -> 65536 to match
the seat's own literal `--ctx-size` (`TestAgentWindowMatchesWhatTheAgentSeatServes`).

- **Exclusivity, decided explicitly and enforced in the renderer.** mimo-9b-agent claims the SAME
  `agent-seat` alias as qwen3.5-4b-agent and qwen3.5-9b-agent. `include_mimo_9b` with
  `include_qwen35_4b` is refused by name (both would claim the alias) — but `include_mimo_9b` WITH
  `include_qwen35_9b` is explicitly ALLOWED: the two 8GB-class seats render TOGETHER, with
  qwen3.5-9b-agent kept as the un-aliased ROLLBACK (a new `__Q359B_AGENT_ALIAS__` token drops its own
  claim on `agent-seat` precisely when mimo-9b-agent is included, so the two entries never collide).
  `include_mimo_9b` alone (no qwen3.5-9b-agent) is also a valid shape.
- **Wired into `setup/templates/llama-swap.win-cuda.yaml`, `llama-swap.linux-cuda.yaml` and
  `llama-swap.linux-vulkan.yaml`** — the exact three templates that already carry qwen3.5-9b-agent.
  The seat pins its measured configuration as LITERALS (`--ctx-size 65536 --flash-attn on
  --cache-type-k q8_0 --cache-type-v q8_0 --reasoning off`), not the tier's `__CTX__`/`__FLASH_ATTN__`/
  `__KV_K__`/`__KV_V__` macros — the same convention qwen3.5-9b-agent uses for its own literal ctx.
  Unlike qwen3.5-9b-agent, MiMo pins `--reasoning off` directly on the command line rather than the
  `LLAMA_ARG_CHAT_TEMPLATE_KWARGS` env-twin workaround: measured byte-identical on all 12 quality runs,
  so the CLI flag is the intended pin for this family.
- **REQUIRES llama.cpp >= b11102.** On b10964 the chat-template auto-detector misroutes MiMo's compact
  tool-call XML (`<tool_call><function=...>` with no newlines) to the Qwen3-Coder parser, which waits on
  a literal `\n</parameter>` that never comes, so every tool call runs to the token cap (upstream
  llama.cpp #29319, fixed in `common/chat.cpp` by excluding that template, present in b11153). MiMo
  failed every agent run on b10964 for exactly this reason; documented in the template comment, the
  tier notes and `docs/tiers/{blackwell-8,ampere-8}.md`.
- **MEASURED 2026-09-24 on both 8GB reference boxes** (RTX 5060 8GB, RTX 3070 Laptop 8GB; llama.cpp
  b10964 for fit, b11153 for quality/engine smoke): fit at ctx 65536/q8_0/flash-attn-on is smaller than
  qwen3.5-9b-agent's on both boxes (6,926/7,034 MiB vs 7,044/7,154 on the 5060; 6,683/6,707 vs 6,801/6,827
  on the 3070 Laptop). Quality (agent-seat bake instrument, gold.py grader, fresh server per rep, n=3):
  shape B 18/18 x6 both models both boxes; shape C is a genuine coin-flip tie — MiMo 5/5 x3 / qwen 4/5 x3
  on blackwell-8, MiMo 4/5 x3 / qwen 5/5 x3 on ampere-8 (q2, cross-file arithmetic, is the item that
  flips). vLLM was ruled out for this 9B on 8GB: text-only W4A16 weights alone (7.51-9.35 GiB) exceed
  the card before KV. Full figures in `setup/templates/profiles.json`'s tier notes.
- Tests: new `internal/servingtmpl/mimo9b_invariant_test.go` (measured-invariant pins, entryless-template
  refusal, the 4B/mimo mutual exclusion, the mimo+9B co-render with the alias handoff, mimo-alone).
  Extended `q354b_deadtoken_test.go`, `provenance_test.go`, `agent_window_test.go`, `seat_closure_test.go`,
  `install_gatedmodels_test.go`/`_contract_test.go`, `setup/render.tests.ps1` and
  `setup/tests/install-config-seed.test.ps1` the same way the 9B pair's own tests are shaped.

## [0.140.8] - 2026-09-24 - the GPU lease queue is strictly FIFO for a returning holder, a blocked seat/text-load admission gets a fair turn, `--unload-seat` clears every resident model, and `gpu reserve`/`gpu status` name foreign VRAM holders

Four GPU-lease fairness defects measured live during the `<node-e>` remediation (2026-09-23,
`REMEDIATION-2026-09-23.md` sections R2-R4 and the noise-repro notes), each pinned by a test broken
then restored at its real call site.

- **The lease queue is now strictly FIFO for a returning holder.** `gpulease.Acquire` used to try
  once, UNREGISTERED, before ever consulting the waiters queue — so a process that had just released
  the card could call `Acquire` again and win a just-freed card ahead of an already-registered waiter,
  indefinitely, because that first attempt was never gated by `isFrontOfQueue`. Measured: waiter pid
  7864 queued 15:30:28 behind holder pid 13832 (epoch 111); the holder released and immediately
  re-acquired as epoch 113 (~15:31) and epoch 114 (~16:01) while 7864 sat "queued ... 41m in line",
  only getting the card at 16:12:20. `Acquire` now registers BEFORE its first claim attempt and gates
  that attempt through the same `isFrontOfQueue` check as every later poll — fixing this also exposed
  (and fixed) a `NEVER WAIT FOR SOMETHING THAT CANNOT HAPPEN` short-circuit that must fire once, against
  the holder at CALL time, never against whatever a later, gated attempt happens to see once the waiter
  reaches the front of the queue — and a real `(nil, nil)` bug: a waiter blocked the WHOLE wait window
  behind another registered, non-acquiring entry (see the seat-fairness fix below) used to fall through
  Acquire's timeout path with no error ever captured, returning a nil lease alongside a nil error.
- **A blocked seat/text-load admission now gets a fair turn instead of starving behind chained media
  leases.** `modelaffinity.awaitLease` (the gate a `transcribe`/`assess_image`/any llama-swap load waits
  on while a media lease holds the card) never itself held a place in gpulease's waiters queue, so two
  back-to-back media leases from the SAME client left it no gap it could ever observe as free — measured
  15+ minutes, never admitted between epochs 111 and 112. A blocked admission now registers a
  `gpulease.ClassSeat` waiter (`gpulease.RegisterSeatWaiter`) for as long as it polls; it never itself
  calls `TryAcquire` (so it cannot starve the lease outright), but the FIFO fix above makes a fresh
  media/text `Acquire` queue behind it exactly like a real lease waiter, guaranteeing the admission is
  noticed and released before the next chained lease can retake the card.
- **`gpu reserve --unload-seat` now unloads every OTHER resident llama-swap model, not only the
  configured agent seat.** Measured: the `<node-e>` vision seat `qwen3.5-9b-vl`, loaded by another
  client, stayed resident through an entire media lease on an 8 GB card and only aged out at its ttl.
  `maintainSeat` now reads `/running` before unloading and unloads every other model it lists (best-
  effort: an unreadable `/running` never blocks the agent seat's own unload), and reports what it
  unloaded. The PR #464 rule is unchanged: only the configured agent seat is ever warmed back
  automatically, and only if it was loaded.
- **`gpu reserve` and `gpu status` now name non-harness processes holding significant dedicated VRAM.**
  Idle DaVinci Resolve held 1,450 MiB of an 8 GB card and nothing reported it: nvidia-smi's
  per-process memory is `[N/A]` on Windows/WDDM, so the existing `gpuactivity.SampleProcesses` source
  had nothing to show there. A new Windows source reads the `\GPU Process Memory(*)\Dedicated Usage`
  perf counter by pid (`internal/fleetnode.AllProcessDedicatedMiB`, alongside a `ProcessNames` pid->exe
  lookup); nvidia-smi's compute-apps query remains the Linux/NVIDIA path, where it does report real
  per-process memory. Both are best-effort and time-bounded (never fatal, never gate GPU work). A
  denylisted-name/floor-MiB filter keeps the harness's own processes and small residents out of the
  warning: `gpu reserve: WARNING foreign GPU memory holders: Resolve pid 9416 1450 MiB`. Nothing is
  ever killed.
