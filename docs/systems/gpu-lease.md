# GPU lease

## Purpose

The machine-wide, fenced, two-class lease that serializes GPU-heavy work across every
ingress — the harness CLI, the Node render runners, the fleet node, and anything else that
takes it. It exists so a text measurement and a media generation can no longer destroy each
other on a single shared card.

- **CLI:** `local-offload gpu status|cards|reserve|release|doctor` (`hold` is the detached holder's own entry)
- **State:** `<state_dir>/gpu/{lease/meta.json, epoch}` — default `%ProgramData%\local-offload` on
  Windows, `/var/lib/local-offload` on Linux (**see the one-time Linux setup below**). A card-scoped lease
  (opt-in, [below](#card-scoped-leases-record-v2)) lives in `lease/e/<epoch>.json` and `lease/cards/<id>.claim`
  instead, and the first one writes `gpu/FORMAT`. The same root holds the media ticket queue (`media-queue/`, ADR 0083), so a
  machine's tickets, node claims and place tokens share the root this resolves, with no config key.
- **Decision record:** [ADR 0018](../architecture/decisions/0018-machine-wide-fenced-gpu-lease.md); the rule that a lease names its cards and a card has one holder, with what is not built, is
  [ADR 0094](../architecture/decisions/0094-a-lease-names-its-cards-a-card-has-one-holder.md)

## Source map

| path | role |
|---|---|
| `internal/gpulease/gpulease.go` | **the only implementation**: `LeaseDir` (the one resolver), acquire/release, epoch fencing, the reclaim conjunction, `InspectDir` (the one read path) |
| `internal/gpulease/devices.go` | **card-scoped leases (record v2)**: device-id normalisation, the conflict rule, the grant (one critical section under the epoch lock), the per-epoch fence, per-lease release and restamp, the debris sweep, and the one reader (`reader`) every inspector shares |
| `internal/gpulease/allocator.go` | the **card allocator** behind `gpu reserve --cards`: a pure function over an input the CLI assembles (cards, claims, quarantine, foreign busy, resident seats, the display cap, host RAM), plus the quarantine sidecar reader; `DisplayFloorGiB` / `DisplayBudgetGiB` are its display-card arithmetic as pure functions, which the card table's `display_cap` row and `audit-yaml`'s note read too |
| `internal/gpulease/cmddevices.go` | the default card set of a wrapped command: `CUDA_VISIBLE_DEVICES`, `--cuda-device`, `COMFY_CUDA_DEVICE` turned into cards, refusing instead of guessing |
| `internal/gpulease/effective.go`, `infer.go`, `proctree*.go` | **what a lease holds, as a seat reads it (plan P4)**: `Info.EffectiveDevices/Touches/For`, `ResolvePins`, the legacy-lease evidence rule (`Scoper`, applied only to a record an older binary wrote: `Info.Legacy`; the `seen.<epoch>` sidecar, the scope ledger) and the process-tree reader it runs on |
| `internal/gpulease/term.go` | **terms (plan P9)**: `PlanTerm` (what a requested window comes to), the installed limits (`SetDefaultTerms`), and `AdvanceTerm`, the one place a term ends in a renewal or the expired label (see [Terms](#terms-a-window-is-a-term-and-a-term-ends-in-a-renewal-or-a-label-adr-0070)) |
| `internal/gpulease/audit.go`, `procimages_*.go` | the **reader audit** behind `gpu doctor`: finds harness binaries, Node readers and running images, and writes the reader-audit marker |
| `internal/gpuprobe/cards.go` | the **card table** (UUID-keyed, index spaces side by side) |
| `internal/gpuprobe/hostmemory.go`, `hostram*.go` | the **host memory reading** (physical, available, commit used and limit; Windows `GlobalMemoryStatusEx`, Linux `/proc/meminfo`), `HostRAMAdmitsLoad` (the ONE admission rule, over the working set a job touches and the commit it is charged, apart), the OK / NEAR / OVER verdict and the `UseHostMemoryReader` test seam |
| `internal/gpulease/hostram.go`, `procmem*.go` | the **host-RAM term of the grant**: `Options.HostRAMGiB` and `Options.HostRAMCommitGiB`, `ErrHostRAM`, the not-yet-loaded part of granted leases, per quantity (the working set less what the processes below the holder hold RESIDENT; the commit less what they hold PRIVATELY), `Manager.HostRAMPendingLoad`; and, for a lease that unloads the seat (F82), `Options.HostRAMWaitGiB` / `Meta.HostRAMWaitGiB` and `Manager.AwaitHostRAM`, the same rule put after the unload |
| `internal/pipeline/mediahostram.go` | **F82 for the media path**: whether a render defers its host RAM until its own unload has run (`hostRAMDeferral`, shared by the grant and the lane probe), the Go-side clear under the lease (`clearForHostRAM`), the bound of the wait after it (`hostRAMWaitEnd`, `mediaHostRAMFloor`) and the step that runs both (`clearThenAdmitHostRAM`); [the media order](#a-render-lease-follows-the-same-order-the-media-path-the-incident-of-2026-10-24) |
| `gpu_hostram_await.go` | **F82**: how a reserve's host-RAM need is split between the grant and the wait after the unload (`hostRAMSplit`), whether the unload can free anything on the lease's cards (`unloadReachesLease`), the grant's screen that refuses a need no state of the box admits and keeps the grant's comparison when host memory cannot be read (`screenDeferredHostRAM`), the wait's deadline and its words (`hostRAMDeadline`, `awaitHostRAMBounded`), the hold bound that ends that wait and releases the lease (`errHostRAMHoldBound`, `releaseAfterRAMWait`, `autoReleaseWarmGuard`; [the hold bound](#the-hold-bound-gpu_hostram_hold_max_sec-the-incident-of-2026-10-14)), `gpu status`'s sentence for a lease that is waiting (`hostRAMAskedStatus`, `holderGaveUpNote`), the line for a release that did not happen and the command that frees it (`leaseNotReleasedLine`, `releaseCommand`), the confirmation of a release's answer (`releaseConfirmed`, `releaseLeaseConfirmed`) and the failure of a wait whose stamp could not be written (`holdStampError`) |
| `internal/hostneed` | **how much host RAM a lease declares**: `--ram`, else the model files of a recognised render helper call that do not fit the card (for its own `--graph`, the files that graph's loaders name), else the media class default (see "Host RAM") |
| `internal/gpucards/hostview.go` | the `host_memory` block and verdict that `gpu status` and `offload_status` render |
| `internal/gpucards` | the per-card view (`RowsWithCap`, `LeaseRows`, `QueueRows`, `Table`, `Section`) that `gpu status`, `gpu cards` and `offload_status` brief all render; the display card's `DisplayCap` verdict is computed in `RowsWithCap` and nowhere else |
| `internal/servingtmpl/audit_displaycap.go` | the **display-cap-unseen note** of `audit-yaml` (`AuditDisplayCapUnseen`, `HasProxyEntry`): names the llama-swap entries that may sit on the display card, judges none (see "The display card is allocatable up to a cap") |
| `internal/gpulease/proc*.go` | per-platform liveness and process-start identity, exported so no consumer keeps a second copy |
| `internal/gpulease/tree.go` | **a dead wrapper with a live tree keeps its claim (plan P10, ADR 0084)**: the recorded tree (`Meta.Tree`, `Lease.RecordTree`), `judge` (the one place a reader decides what a record's tree is), the launch-marker and process-table evidence, and the armed ComfyUI directory (`SetComfyDir`, called from `modelaffinity.SetComfyDir`) |
| `internal/proctree` | **process-tree containment and the stop primitives (plan P10)**: `Start` runs a command in a job object (Windows) or its own session and process group (Unix), `PIDs`, `Terminate`, `TerminateExcept`; `Job` is the job object the agent shell's cage (`internal/sandbox`) is built on too; `PlanStop` and `Stop` stop only a recorded tree or a launch marker's process and report anything else on a card as not ours |
| `gpu_tree.go` | the wrapper's side: `treeRecorder` (writes the tree into the lease record when the set changes), `stopWrappedTree` (the whole tree, sparing an instance another lease took over) |
| `internal/comfyinst` | stops the ComfyUI instances a lease kept (`StopForLease`; `StopForRelease` spares one that ends itself, ADR 0097), and the primitives an explicit takeover will use (`StopMarker`: interrupt, free, kill after the launch fingerprint is verified; `PIDsOfOtherLeases`) |
| `internal/pipeline/pipeline.go` | takes the `media` lease around **every** generation call site (image ComfyUI + sdcpp, inpaint, image batch, run-graph, video, audio) and threads `GPU_LEASE_*` to the runner; inherits an ambient lease instead of re-acquiring; owns the in-process slot (`mediaSlot`) that arbitrates jobs sharing one inherited lease |
| `gpu_cmd.go` | the `gpu status\|reserve\|release\|hold` verbs, wrapper and `--detach` forms |
| `gpu_faults.go` | `gpu faults`: the fault history report, its text and JSON, and the `--jobs` join of the ledger |
| `gpu_faults_health.go` | fleet-serve's fault summary: the background read behind `/fleet/health`'s `gpu_faults`, the retention appends and the summary file ([below](#retention-and-the-fleet-nodes-summary)) |
| `internal/gpulease/yield.go`, `gpu_yield*.go` | **the yield request (plan P11, ADR 0086)**: the policy (`ParseOnOverdue`, `SetDefaultOnOverdue`), the asker, the `yield.<epoch>` sidecar, the holder's ticker, the read-only `YieldViewOf` every surface words from; the root files hold the shell runner (`sh -c`, `cmd.exe /d /s /c`, hidden) and the `--detach` report |
| `gpu_terms.go` | the holders' side of terms: the tick the wrapper form and the detached holder run at the end of a term, the over-cap warning, and the argv of the hidden holder (`--release-at-expiry`) |
| `gpu_progress.go`, `internal/gpulease/progressdir.go`, `render/lease-progress.mjs` | the progress contract of a wrapped render helper (F8): the helpers the wrapper declares one for, the minted file and its sweep, `GPU_LEASE_PROGRESS_FILE`, and the helper-side heartbeat |
| `render/lease-read.mjs`, `render/gpu-lock.mjs` | **the Node readers of the lease**: `readSharedSync` / `readSharedJson` (a refused open waited out to a 100 ms deadline, and an absent name waited for to a 250 ms deadline while a fresh staged file stands beside it, on Windows only; a missing file with nothing beside it read once), behind `readLease`, `readEpochRecord`, `readCardClaim`, the fence `checkInheritedLease`, and the `render/lease-progress.mjs` record reader. Node never writes these files |
| `gpu_reserve_devices.go`, `gpu_cards.go`, `gpu_doctor.go` | which cards a reserve holds (`--devices`, `--cards`, derivation, the queue), `gpu cards` and the status table, `gpu doctor` |
| `internal/gpulease/owner.go` | **ownership (ADR 0070)**: who asked for a lease (`Owner`), the session registry, the first-observer orphan marker, the progress contract and `Standing`, the derived reading every status surface shares |
| `internal/gpulease/explain.go` | the sentence a waiter reads when the lease it is queued behind is stalled, orphaned or overdue, and the installed orphan grace. READ-ONLY: it reads the orphan marker a status surface recorded and never writes it or takes the epoch lock |
| `gpu_ownership.go` | the ownership flags (`--owner-*`, `--unattended`, `--progress-file`, `--stall`, `--yield-grace`, `--on-yield`), the `gpu owner-flags` verb, and the ownership lines of `gpu status` |
| `gpu_drain.go` | `--drain`: waits until the seat's gauge AND the run registry are empty, inside the queue budget; restamps `draining` → `exclusive`; unload / warm-back |
| `internal/gpuactivity` | the RUN REGISTRY (`<state root>/gpu/activity/`, one record per agent loop in flight, and one per single-shot vision call while it is on its seat: kind `vision`, no `devices`) and the activity reading behind `gpu status` / `offload_status.gpu_lease` (`verdict`, `activity`) — ADR 0041; `holders.go` derives each live lease's standing, `facts.go` the activity facts of a legacy lease, `snapshot.go` (`View.Map`, `splitProcesses`) the one JSON shape both surfaces read, with a display card's unsized processes folded into one count per card; `driverevents.go` the **driver's fault events** for the leased cards (C-83-B, [below](#the-drivers-own-fault-events-for-the-leased-cards-c-83-b)); `faults.go`, `knownerrors.go` and the generated `xidcatalog.go` the fault history behind `gpu faults` ([below](#gpu-faults-the-fault-history-as-incidents-with-known-errors)), `faultcontext.go` the restart and sleep context, `faultstore.go` the retention file and the per-card summary |
| `internal/seatload` | the seat's in-flight reading: llama-swap's `/running` (and the roster, for an alias), then the loaded seat's own `/metrics` or `/slots` at the `proxy` `/running` reports — never through `/upstream`, which resets the idle unload timer; a `starting` seat is reported without touching the seat |
| `gpu_hide_windows.go`, `gpu_hide_other.go` | hidden spawn for the detached holder (a visible console gets closed, killing the hold): it always gets a hidden console of its own, whatever terminal launched it (`winexec.HideWindowDetached`); the flags live in `internal/winexec`, the one place every hidden child is built |
| `render/gpu-lock.mjs` | READ-ONLY participant: honours + fences an inherited lease, elects one unloader, drains, ComfyUI lifecycle. **Does not acquire.** |
| `internal/gpulock` | the read-only vision gate; the held rule is `gpulease.InspectDir`'s, read through the caller's own narrowing (`WaitFreeScoped`: the vision pre-check passes the seat-narrowed reading the text gate uses) |
| `internal/modelaffinity/gpuwait.go` | READ-ONLY: text admissions that would make llama-swap load a model wait out a `media` holder (ADR 0026); armed from `config.Load` via `LeaseDir` |
| `internal/modelaffinity/upstream.go` | READ-ONLY: `AwaitUpstream`, the ONE builder of a llama-swap `/upstream/<model>/…` URL, and `AwaitModelRoute`, the builder for a model-dispatched route (`/v1/chat/completions`, `/v1/embeddings`, …) sent without `Admit` — every probe, tokenizer, warm-up, transcription, chat-lane forward and embedding passes the same fence as a generation (see "Probes pass the fence too") |
| `gpu_drain.go` (`leaseScope`, `maintainSeatScoped`, `unloadModelsFor`), `internal/gpualloc/scope.go` (`LeaseScope.Locate`, `Judge`, `UnloadModels`), `internal/gpualloc/serving.go` (`servingState`), `internal/servingcfg` (`Load`, `PinsOf`, `Pins`: the one reader of the serving config's pins, shared with the load fence), `render/gpu-lock.mjs` (`parseUnloadModels`, `leaseDevices`) | plan P5 and F15: the drain waits for the runs on the leased cards, the unload takes the models placed on them or not placed at all and leaves the models placed on other cards (by `layers`, else by the serving config pin), and the render lane unloads the list the wrapper exports (`GPU_LEASE_UNLOAD_MODELS`) |
| `internal/modelaffinity/seatscope.go`, `scoper.go`, `seatyield.go` | the gate's per-seat reading (`SetSeatPins`, `ScopeToPins`, `SeatLease`, `CardsHeld`), the production wiring of the evidence rule (`InspectLease` for the load gate, which remembers what it sees; `PeekLease`, `ScopeInfo`, `ScopeFunc`, `ScopeLeases` for inspectors, which write nothing), and the seat race rule (`YieldIfFenced`) |
| `internal/mcpserver` | registers the session this MCP server serves in the session registry at start and removes it at exit |
| `internal/config` | `state_dir`, `gpu_lock_path`, `gpu_card_scoped_leases`, `gpu_legacy_scope_inference`, `gpu_comfy_order`, `gpu_host_ram_headroom_gib` (default 8, chosen and not measured; `Load` installs it process-wide, `gpulease.SetDefaultHostRAMHeadroom`), `gpu_hostram_hold_max_sec` (F82 hold bound: unset 600, chosen and not measured, `0` = no bound, a negative value or one above the 7-day ceiling `MaxGPUHostRAMHoldMaxSec` refused by name; a `*int` so unset and 0 differ; `GPUHostRAMHoldMax` clamps before it converts to a `Duration`), `agent_seat_host_ram_gib` (the host RAM the agent seat holds once loaded, for the warm-back; unset = `hostneed.DefaultSeatHostGiB`), `gpu_orphan_grace_min`, `gpu_max_term_min`, `gpu_max_total_min`, `gpu_on_overdue` (unset asks nobody; `request` asks an overdue lease's holder to yield; `hold`; installed at `Load` on every exit, an unrecognised value as `hold`); `ModelPins` (a model's device pins: `LayerPins`, the layers' reading, else the serving config's through `internal/servingcfg`), `serving_config_path` (the llama-swap yaml that places the models `layers` do not declare, for the unload list and the load fence) |

## Why it exists

One GPU, two very different consumers: llama-swap wants its tiers resident; ComfyUI wants the
whole card. Before this, only the Node render runners took a lock, so text work had no way to
say "I am using the GPU." A media job dispatched from elsewhere unloaded every GPU-resident
model mid-benchmark. The server log holds **3,356 unload calls, 330 of them the text
workhorse, and 0 for the memory stack** — that last zero identifies `freeLlamaSwap` as the
caller, since excluding the memory stack is its own invariant.

## Using it

`local-offload gpu --help` (also `-h`, `-help`, `help`) prints the subcommands on stdout and exits 0; `local-offload gpu
<subcommand> --help` lists that subcommand's flags. A `--config <path>` (or `--config=<path>`) may come before the
subcommand as well as after it, as it may in front of any other verb (F35: both used to fail as `unknown gpu subcommand`).

Prefer the **wrapper** form. The lease lives exactly as long as the command, so it cannot be
leaked by forgetting to release, and it composes like `timeout` or `nice`:

```
local-offload gpu reserve --class text --for 45m --reason "kv bench" -- <command...>
```

`--detach` holds the card in a hidden background process for an interactive session. It is the
weaker form by design — nothing ties the lease to a command's lifetime, so **`--for` is REQUIRED with
`--detach` (0.113.27)**. Until terms (plan P9) the holder exited at that deadline and released whether or not the
work had finished, and the old 45-minute default silently freed the card mid-job, after which the next render
claimed it and unloaded the seat on top of the running work (2026-09-07). **It no longer releases at the
deadline**: at the end of the term it renews the lease if its owner vouches for the job, else labels it expired
and keeps holding (see [Terms](#terms-a-window-is-a-term-and-a-term-ends-in-a-renewal-or-a-label-adr-0070));
`--release-at-expiry` restores the old ending for a caller that wants exactly that. The window is still the
term the lease is judged by, so declare the real one, or wrap the command:

```
local-offload gpu reserve --class text --for 45m --reason "kv bench" --detach
local-offload gpu release --epoch <N>
```

`gpu status [--json]` reports the holder, its class, age, reason and declared expiry — and says
**"free (unreserved)"** explicitly, because an unreserved card is exactly when work is exposed;
that should be visible, not inferred from silence. When held it ends with the one line that
matters: how to **queue behind it** (`queue_with` in the JSON).

The headline names what holds the card: **a lease of a class**, `GPU: held by a text-class lease  pid 792210  epoch 7  for
3m0s  expires 4:05PM  (exclusive: text loads wait or route elsewhere)` (`a media-class lease` for a render). The bare class
word it used to print (`held by text`) read as a text *seat* holding the card, and on 2026-10-07 two sessions argued over who
held it while the holder was a bench's reservation (the class only says what the reservation is for). One phrase
(`gpulease.Class.LeasePhrase`) is shared by every surface that names a holder: this headline, the refusal and the
`queued behind` line of `gpu reserve` (`GPU held by a text-class lease (pid …, held 3m0s, reason "…")`), the error of a
`gpu reserve --detach --wait 0` that lost the race (`another holder took the GPU first: a text-class lease (pid …, reason "…")`),
the brief `gpu_lease_verdict` line of `offload_status` (`held by a text-class lease (pid 792210, exclusive, 180s): <reason>`)
and the media tools' deferrals ([media-generation.md](media-generation.md), "A call that cannot get a card keeps its place").

### A held card is a place in line, not a refusal (0.115.2)

`gpu reserve` **queues** behind a current holder — `--wait` (default **8h**) is how long it
stands in line before giving up; `--wait 0` restores the old fail-fast. Both forms queue: the
wrapper waits in place, and `--detach` passes the wait to the hidden holder (`gpu hold`), which
is the process that actually takes the card, while the parent reports `reserved:` only once that
has happened (or the holder's exit, when the line did not move in time). The wait prints exactly
two lines on stderr — `queued behind <holder> — waiting up to <wait>` on entry and `acquired after
<n> in the queue` on exit — never one per poll, because the session wrapping this would turn a
poll line into a notification each.

A free card with a waiter already registered for it is that waiter's: a fresh `gpu reserve` never probes
the card bare. `Acquire` registers before its first attempt, with or without `--wait`, and only the front of
the line claims; `--wait 0` on such a card fails fast naming the waiter ahead (`ErrStillQueued`), and a waiter
for other cards is no reason to wait (disjoint backfill). Before this, the CLI's first probe was a bare
`TryAcquire`, so a recipe that chained reserves back to back won every just-freed card ahead of a waiter
registered for over an hour (2026-10-09).

A waiter is on the cards it waits for and no others. A blocked text-load admission (a seat waiting for a render to
clear its card) registers on **its seat's cards** — the cards its wait blocks on, resolved from the layer pins and
the card table — so a fresh `gpu reserve --devices <other card>` on a free card the seat does not sit on still goes
through; a seat whose cards cannot be named (an undeclared model, a pin the table cannot place, an unreadable table)
registers as the whole node, the gate's rule for every doubt. `gpu reserve --cards N` reads the line when it
allocates (`gpualloc.QueuedClaims`, the rule the media admission already used): a card with a waiter or a held place
ahead of the request is not free to it, so it takes a card nobody is queued for, and a claim lost to a waiter is
allocated around exactly like a claim lost to another reserve (N reserves fanning out over N free cards land on N
cards). A refusal says what the request did. `--wait 0` says the card is free but the request is not first in line,
names the waiter ahead (one that is actually ahead: older, on a card the request wants) and the flag that queues; a
window spent in line says it gave up after waiting that long and to pass a longer `--wait`. A `--detach` reserve
whose hidden holder gives up repeats the holder's own last line next to the log path, so that hint reaches the
terminal that asked.

The declared-window short-circuit below does **not** apply to a reservation (`Options.WaitOut`):
the holder's window is printed as information, never treated as a verdict, because holders
release before it as a rule — the wrapper form releases the moment its command ends. Measured
live on 2026-09-09 before this was fixed: a `--wait 2m` waiter behind a `--for 3m` holder was
refused at once, and the holder released six seconds later. Every refusal names the flag that
would have kept queueing.

Why: the CLI called `TryAcquire`, so a held card was an *error*, and every session that hit that
error read it as "the machine is busy — refuse the work". Ten times over, the operator's answer
was the same: the machine has a queue, use it. The pipeline had queued renders behind each other
since ADR 0018; the reservation verb was the one ingress that did not.

### Exclusive text holds (0.115.2)

A text lease that **cleared the cards** (`--drain --unload-seat`, or an explicit `--exclusive`)
is stamped `exclusive` in its record. The text-load admission gate (`internal/modelaffinity`,
ADR 0026) then treats it exactly like a media lease: an admission that would make llama-swap pull
a model onto the cards rides its `cascade_remote_lanes` lane when one serves the model, otherwise
waits its own budget and is told who holds the card. A plain text lease (holder unloaded nothing)
is still ungated, for the reason ADR 0026 gives — a switch under it costs a measurement, not the
machine. The exclusive stamp closes the case that reasoning never covered: a holder that *did*
empty llama-swap, whose measurement the very next interactive text call refilled the card under —
the residency switch that voided two 5070 Ti runs and taught sessions to refuse GPU work rather
than reserve it.

## One-time Linux setup (required)

`%ProgramData%` already exists and is writable on Windows. **`/var/lib/local-offload` does not exist
on Linux and an unprivileged service user cannot create it** — and `setup/install.ps1` is
Windows-only, so nothing creates it for you. Until it exists the lease refuses to start and **every
media job defers**, which is the refusal working correctly and is easy to misread as a broken route.
Measured on a Linux services box the first time it was upgraded past 0.23.0.

```
sudo mkdir -p /var/lib/local-offload && sudo chmod 0777 /var/lib/local-offload
```

World-writable is deliberate: the lease is machine-wide so that a service running as one user and a
render running as another contend on the SAME lease. It is deliberately **not** sticky — reclaiming
a dead holder's lease means removing another user's file, which the sticky bit would forbid.

Alternatively set `state_dir` to a local, unsynced path the user already owns. That works, but it
scopes the lease to whatever can reach that path — if two security contexts do not share it, they do
not contend, which is the failure the machine-wide default exists to prevent.

## What happens when the card is busy

A GPU job **queues behind the current holder for a bounded window, then defers** with the
holder's class, age and reason. Both halves are deliberate:

- **It queues** because the single slot exists to serialize GPU work, not to cancel it. A render
  arriving thirty seconds into someone else's render should run thirty seconds later. Dropping it
  would let a `gpu reserve --class text --for 45m` silently discard 45 minutes of media requests.
- **The queue is bounded** because the caller is usually one tool call, and blocking it for tens
  of minutes is indistinguishable from a hang. Past the window an honest ETA is more useful to a
  caller that can retry.

`gpu_wait_ms` (default **90 s**, matching `vision_gpu_wait_sec`) is the single ceiling for every
GPU task placed by a *tool call*. A *reservation* (`gpu reserve`) is a session with a job to run,
not a tool call that must return, so it queues for `--wait` (default 8h) — see "A held card is a
place in line" above. `videogen_wait_ms` and `audiogen_wait_ms` are **retired and ignored** — they existed so
a cheap TTS was not starved behind a 20-minute video, which buys nothing at 90 s, and every
installed `config.json` still carries `videogen_wait_ms: 1200000`, so honouring them as overrides
would have quietly restored the old 20-minute wait on upgrade. A config carrying them loads
cleanly and prints a note naming the replacement.

Only **contention** is waited out — an unwritable or cloud-synced lease location comes back
immediately, because waiting cannot fix a configuration fault. A busy card is a **defer**, not a
failure: `generate-image --batch` exits 0 with `err_class: gpu_busy` rather than a non-zero error.

Two things keep the wait from becoming friction:

- **A `text` reservation that outlasts your window is answered immediately** — for a tool call.
  `gpu reserve --class text --for 45m` is an operator's *declared* duration, so a 90 s tool call
  gets the ETA now instead of 90 s later. A `media` holder is deliberately not treated this way:
  its expiry is a timeout *ceiling*, and a 25-minute video budget routinely finishes in three, so
  it is waited out. A *reservation* (`gpu reserve`, `Options.WaitOut`) is not a tool call and
  waits its whole `--wait` regardless — see "A held card is a place in line".
- **Two jobs in the SAME process never poll each other.** They queue on an in-process slot and
  the waiter is handed the card the instant the holder releases — no timer, no file reads.

## What it costs when nothing is contending

**Nothing.** The lease adds no daemon, no scheduled task, no watchdog and no background thread.
Every timer below exists only while real work is in flight, and stops with it:

| when | what runs | cost |
|---|---|---|
| idle | nothing at all | zero |
| a render is running | one 15 s heartbeat per held lease, at most one per process | one small file write / 15 s |
| your job is queued behind another process | one probe per second, capped by `gpu_wait_ms` | two small file reads / s, ≤90 s |
| your job is queued behind one in the same process | blocks on a channel | zero — no polling |
| `gpu reserve -- <cmd>` | one 15 s heartbeat for the command's lifetime | one small file write / 15 s |
| `gpu reserve --detach` | a hidden holder polls once a second until released | the only continuous poller; exits on its own |

`--detach` is the sole thing that keeps running after the command that started it, which is why
the wrapper form is preferred. It is an ordinary process, not a registered service: it exits by
itself when released or fenced out (and at its declared deadline only with `--release-at-expiry`;
by default a lease past its term is renewed or labelled expired and the holder stays), and nothing
respawns it.

Running the harness under a reservation **inherits** that lease:

```
local-offload gpu reserve --class media -- local-offload generate-video "…"
```

The child does not acquire a second lease — it would queue behind its own parent for the whole
window and then defer. If the inherited lease is no longer current (the parent was fenced out),
the job refuses instead of quietly taking a fresh one, so a lost reservation is visible.

## Classes

| class | who takes it | may unload models? |
|---|---|---|
| `text` | a benchmark, eval, or measured run | no |
| `media` | image / video / audio / run-graph | **yes**, once per lease |

Both are exclusive per card: a lease that names no cards holds the whole node, a card-scoped lease holds only the cards
it names, and a card never has two holders (see [Card-scoped leases](#card-scoped-leases-record-v2)). The label carries
intent, not access control.

**Ordinary interactive text calls still do not ACQUIRE the lease.** There are thousands a day at
~46 ms and leasing them is untenable. That remains a known limit, not an oversight.

**They do READ it.** Corrected 2026-08-26: the carve-out was read as "a short call inside a media
lease pays a reload", and the real cost is larger. A `media` holder unloads llama-swap once per
lease, so the card is CLEARED for the render — and the next text call pulled a multi-GB model
straight back into the VRAM that render had just been given. Reported symptom: the box becomes
unusable under a render.

So the Model Affinity Gate (`internal/modelaffinity`), which is the one chokepoint that knows which
admissions can change what llama-swap holds resident, now waits for a `media` holder before granting
one — and grants a request that JOINS the resident model's in-flight batch without reading the lease
at all, because that cannot move VRAM. It reads with `InspectDir` and never acquires, so the write-path
cost this lease refused for text is untouched. A `text` reservation does not block text — and since
0.113.27 it no longer makes the VISION lane wait pointlessly either: `gpulock.WaitFree` carries the
holder's declared `ExpiresAt` and short-circuits when a TEXT window outlasts the caller's wait, the
same rule `gpulease.Acquire` already applied. Before that, every `vqa`/`ocr`/`assess_image`/
`video_describe` call burned its full `vision_gpu_wait_sec` (90 s) against a multi-hour hold, for the
hold's whole life. A MEDIA holder is deliberately never short-circuited: its expiry is a timeout
CEILING, not a promise. Only an
INHERITED lease (`GPU_LEASE_EPOCH`) exempts a caller — the holder's own pid deliberately does not, or
`fleet-serve` would un-gate itself. (The vision gate applied that exemption only from 0.169.0, through
`gpulease.Inherited`: before, `gpu reserve ... -- local-offload vqa` waited on its own lease and deferred
`gpu_busy`.) See
[ADR 0026](../architecture/decisions/0026-text-load-admissions-wait-for-the-media-lease.md).

**Delegate placement reads a `text` reservation as "not here" (0.113.14).** `internal/delegate`
resolves the lease through the same `LeaseDir` + `InspectDir` path (`LocalLease`, never acquired) and,
on route=auto and route=spread, a held `text` lease removes the local seat from placement: an eligible
remote takes the contract; with none, the runner waits up to `agent_lease_wait_sec` and then defers
(class `infrastructure`, holder named) rather than loading the reserved cards — the 2026-09-05 case
where three foreign contracts landed on a reserved two-card seat mid-measurement. A pinned `route=local` (one with a `pin_reason`,
or through `offload_ask` and `agent_run`) is not gated; a reasonless `route=local` through `agent_delegate` or `offload_research` is
a hint, placed as `route=auto`, and reads the lease like any other auto call (ADR 0078). A `media` holder only steers (the affinity gate above arbitrates it), so the sentence
before this one still holds for interactive text calls: a `text` reservation does not block them.
Since 0.113.18 that wait is the delegator's **capacity wait** (until the call's deadline less a reserve when the call has one,
ADR 0073, else `agent_placement_wait_sec`, default 120 s, or `agent_lease_wait_sec` when longer): it watches the lease AND every remote's room, so a
remote that frees while the local card is reserved takes the work; the holder-naming deferral is what
remains when nothing frees. The re-placement path's local last resort honours the lease too (it did
not before). See docs/systems/fleet-node.md, "Bands, tenants, saturation and the capacity wait".

The gate's OTHER job — keeping two text lanes from thrashing one serving slot with competing model
names — is in-process only and does not close the cross-process gap named above. See
[ADR 0025](../architecture/decisions/0025-model-residency-is-arbitrated-in-process-by-base.md).

## How it stays correct

- **Machine-wide.** The previous lock defaulted under the OS temp dir, which is per-user on
  Windows — two security contexts held "the" lock simultaneously and nothing errored. An
  unwritable root now refuses to start rather than falling back per-user, and a root under a
  cloud-sync directory is refused outright (a replicated lock file would hand one GPU to two
  machines).
- **Fenced.** Each acquisition bumps a monotonic epoch kept outside the lease dir. Holders call
  `Check()` before anything irreversible. A laptop that slept through a takeover is fenced out
  instead of acting on the current holder's card.
- **Pid-recycle safe.** The holder's process start time is recorded beside its pid.
- **Reclaim needs both halves:** *(holder provably gone)* OR *(heartbeat stale AND declared window
  expired)*. A bare heartbeat timeout would expire a descheduled benchmark under exactly the load
  it exists to protect.
- **An unreadable heartbeat is unknown, and unknown is never stale.** `heartbeatAt` returns the
  heartbeat and whether it could tell. The content of `hb.<epoch>` when it parses and is not earlier
  than the stamp written at the acquisition (a torn write leaves a prefix of the digits, and a prefix of a
  number is a number: it, zero and a negative number say nothing about the holder); the file's
  modification time when the file is there but its content is no use or the read failed for any reason
  but absence (a renewed heartbeat has the time of its write, an unrenewed one ages out like any); the
  acquisition stamp only when the read found the name absent and the file is *settled* absent (no staged
  file younger than 3 s beside the name, and the name still absent on a second look: a holder that never
  renewed); unknown otherwise, which the reader passes to the rule as no stamp. A rename that is installing the heartbeat, or one that cannot
  be examined, therefore never makes a live lease past its window reclaimable. What frees a holder whose
  heartbeat stays unknown for good is the first half of the rule (wrapper gone, by pid and start time) or
  `gpu release`.
- **The same rule holds wherever absence would free a lease.** The keeper's look at a heartbeat (`keeperEvidence`: a record that
  cannot be read is held while its keeper is alive) reads it the way `heartbeatAt` does, and a heartbeat that can be neither read nor
  examined, or whose rename is installing it, is unknown and counts as alive (for a whole-node record, whose epoch is not known, a
  staged heartbeat with no live name in the listing of the directory counts as well); it used to count as no sign of life, so the sweep
  removed a live holder's record and claims. A ComfyUI launch marker that is being installed, which can tie a live instance to a
  lease whose wrapper is gone, is unknown and holds that lease for the moment it lasts (`treeUnknown`, at most the 3 s a staged file
  is fresh); it used to be skipped, and the lease was reclaimed with its instance still on the card. What each other consumer does
  with a lease file it cannot read is listed under *Windows file semantics*.
- **Release is epoch-guarded** — a fenced-out straggler cannot delete the current holder's lease.
- **A reclaim removes only what is still stale (register C-59).** The reclaim rule decides from one read, and
  the claim used to be removed by path: two acquirers that both read the same dead holder's record could
  interleave so that the slower one deleted the claim the faster one had just created — a lease granted and gone
  before its holder did anything with it (its next `Check` or restamp reports "fenced out", or "the lease is
  gone" in the instant before the rival's own claim lands). The removal (`removeStaleClaim`) now runs under the
  epoch lock, reads the record again, and removes it only if it is still stale: a new holder's claim, a claim a
  rival is still writing, or the old holder back from the dead is left alone, and a claim the caller parsed a
  moment ago that now fails to read is left for the next look instead of being treated as debris. This is one
  mechanism by which a draining reserve can lose its epoch to a concurrent acquire — found by reading and
  reproduced deterministically; whether it caused any live loss is unproven. What remains is a window of
  microseconds, open only to a holder (or an operator) releasing at that exact instant.

## The drain

`POST /api/models/unload/<id>` **does not honour in-flight requests.** Measured on llama-swap
v242: an unload fired 3 s into a generation returned in 1,265 ms without draining, and the
generation died at 4,107 ms with `502 Bad Gateway`.

So `freeLlamaSwap` drains before it unloads. `quiesceLlamaSwap` polls each tier's
`/upstream/<id>/slots`, where `is_processing` is true exactly while a slot is generating
(verified across a 23 s / 1500-token run). It is **fail-safe, not fail-open-silent**: if `/slots`
cannot be read (older llama-server, `--no-slots`), it reports `drained:false` and names the
unobservable tiers, and the caller logs that it proceeded without a verified drain instead of
pretending. A stuck tier times out rather than deadlocking the render queue.

**A seat that cannot have a request in flight is not probed (register F81).** `/running` lists every process that
is not stopped, each in one of three states: `starting`, `ready` or `stopping`. The drain reads the state with the
list and skips a seat in `starting` or `stopping`: nothing is in flight on it, so nothing is waited for and nothing is
named unknown. A `starting` seat is not serving: llama-swap holds a request for the whole load (4 min on the
largest seat), so the `/slots` probe sat until its own 5 s deadline, three rounds and a grace in all (17.6 s measured
against a fake llama-swap), and that was added to every render start while any seat was loading. A `stopping` seat is
being killed (an unload does not wait for what it serves), and a request to it is queued behind the stop and then
starts the seat again, the same hazard the `/running` gate above was written against. The unload that follows is
unchanged: a seat that is starting or stopping is still on the list the render clears. A `ready` seat is always
probed, and so is a seat listed with no state (older llama-swap builds) or with a state this code does not know: a
probe of an idle seat costs one read, and a skipped busy seat loses its request. The Go-side drain
(`internal/seatload`) reads the same two states and does not probe the upstream of either; it then waits a load in
progress and a stop out before it counts the seat idle, because the request that triggered a load is waiting on the engine.
The render helper does not wait a load out, by decision. It used to wait only as long as its probes lasted (three 5 s
deadlines): by the code's path (not observed live) a seat whose load finished inside that window and then had a request in flight was waited for, and any other
starting seat was unloaded after the same 17 s with `could not read /slots`. Skipping the probe removes those seconds; the
unload that follows aborts the request that triggered the load, so the drain returns the seats it skipped for a load in
progress as `starting` and `freeLlamaSwap` logs one line for them (`unloading <seat> while its load is in progress ...; a
request waiting on it is aborted`). A seat that is stopping says nothing: it is already leaving.

## Windows file semantics the lease depends on

Two behaviours that do not exist on POSIX shape the implementation, and both were found by a
concurrency test rather than by reading:

- **Delete is pending, not instant.** Removing a file whose handle is still open marks it
  delete-pending; an `O_EXCL` create in that window fails with `ACCESS_DENIED`, not
  `EEXIST`. That window is precisely the moment a holder releases — exactly when every waiter is
  polling — so the acquirer most likely to hit it is the one that should have won. Both the claim
  and the epoch lock retry it instead of treating it as a fault.
- **A reader blocks a delete.** `os.ReadFile` opens without `FILE_SHARE_DELETE`, so `os.Remove`
  on the claim fails while anyone is inspecting it. A failed release *leaks* the lease until both
  halves of the reclaim rule fire, so removal retries.
- **A reader blocks a rename-over, too.** `os.Rename` on Windows is `MoveFileEx` with
  `MOVEFILE_REPLACE_EXISTING`, which must delete the destination's directory entry the same way —
  so it fails with `ERROR_ACCESS_DENIED` / `ERROR_SHARING_VIOLATION` while anyone holds the
  destination open for a plain read, exactly like the delete case above. Every in-place rewrite
  (`Restamp`'s stamp change, the heartbeat's `Renew`, the epoch counter's `writeEpoch`) writes
  beside the file and renames over it, and nothing coordinates that with a concurrent `Inspect()`
  — this process's own drain-renewal loop, another process's `gpu status`, `offload_status`, or
  any other reader. `renameReplacing` (`internal/gpulease/renamesafe.go`) retries both errnos with
  the same bound as the epoch lock's wedge detection (2 s); off Windows it is exactly one
  `os.Rename`, since POSIX `rename(2)` is atomic and never blocked by a concurrent reader. Measured
  directly (`TestRenameReplacingSurvivesAConcurrentReader`, `internal/gpulease/renamesafe_test.go`):
  a bare `os.Rename` against a tight concurrent reader loop failed ~53–58% of iterations; with the
  retry, 0. Before this fix the same race intermittently failed
  `TestReserveRenewsTheLeaseWhileDraining` / `TestReserveRenewsTheLeaseWhileWarmingBack` (~1 run in
  24) and, in production, could leave the `draining` stamp on a held lease (cordoning the seat for
  the rest of its window) or drop a heartbeat renewal.
- **A rename-over blocks a reader, too.** Windows checks sharing both ways. While the rename runs, the
  system holds a handle on the old entry with `DELETE` access, so an open whose own share mode lacks
  `FILE_SHARE_DELETE` fails with `ERROR_SHARING_VIOLATION`, and
  `os.ReadFile` is such an open. A reader that took that failure for "no record" answered "no lease" for
  a lease that was held: `reader.v2` skips a record it cannot read, so `Leases()` came back empty and
  `EpochIsCurrent`, the fence a render child asks, said the lease was not its own. A media route's own
  test saw `want one live lease while the render runs, got []` on a loaded box, 2 runs in 24 under
  30 busy-loop processes. `readRecordFile` (`internal/gpulease/hostram.go`) now opens the lease records
  (`meta.json`, `e/<epoch>.json`) with `OpenShared`, which shares read, write and delete: it does not fail
  against the rename, and it reads the old record whole until the new one is in place, as a POSIX reader
  does. Measured under a concurrent rewrite
  (`TestAHeldLeaseNeverReadsAbsentWhileItsRecordIsRewritten`, `internal/gpulease/readrace_test.go`): with
  `os.ReadFile`, `Leases()` missed 1 read in 30 to 60 and `EpochIsCurrent` about 1 in 15; through
  `OpenShared`, none. A bounded retry of the failed plain read was tried first and dropped: under
  saturating load the sharing violation lasted up to 1.4 s, so a bound short enough for a status call
  left misses, and the open is the cause.
  - **The heartbeat is read the same way, and it matters more.** `hb.<epoch>` is rewritten by `Renew`
    with the same rename over the live name, and `heartbeatAt`, `lastHeartbeat` and `keeperSignOfLife`
    read it through `readSharedFile`. Once a lease is past its declared window the heartbeat is the only
    thing that keeps it: the reclaim rule needs a stale heartbeat AND an expired window. A heartbeat
    read that failed fell back to the stamp written at acquisition (it no longer does: an unreadable heartbeat is
    unknown, and unknown is never stale; see *How it stays correct*), so a live lease past its window (a
    `gpu reserve` command that outran its `--for`, a detached hold past its window) read as reclaimable,
    an acquirer's sweep removed it, and the card was granted to a second job while the first still ran
    on it. A record that cannot be read is only skipped; a heartbeat that cannot be read is the
    dangerous case. `TestALiveLeasePastItsWindowIsNotHandedToAnotherJobWhileItsHeartbeatIsBeingRenewed`
    holds a `DELETE`-access handle on the heartbeat and asks for the card from a second process.
  - **A shared open still meets one more refusal.** For the instant the rename has marked the old entry
    for deletion and the new one has not taken its name, the file is delete-pending and every open of
    the name fails with `ERROR_ACCESS_DENIED`, whoever shares what. `readSharedFile` waits that out: a
    permission error is retried `removeAttempts` times at `removePause` (about 100 ms, the claim
    removal's bound) and any other failure is returned at once (an absent name with a fresh staged file beside it is not a failure
    yet; see the next bullet). Measured on a loaded host with a
    heartbeat renewed in a loop: 5 failed reads in about 124,000 without the wait, none in about
    108,000 with it. A file that stays unreadable is waited out once, not once per read (below).
  - **A rename that was not refused can still leave the live name absent, and that is not an answer yet.**
    For 11 to 75 ms as a reader sees it (one rename took 119 ms to return), about once in 74,000 renames
    on the host that showed it, the live name is absent (`ENOENT`) while the staged file (`stagingPath`:
    `<name>.tmp`, or `meta.json.restamp`, none for a card claim) still stands beside it. A reader that took
    that for "not there" called a held lease gone: the fence failed it, `Leases()` dropped it, a release
    took the whole-node branch. So on Windows an absence is an answer only once it is settled (`gate.go`,
    `settled.go`). `readSharedFile` and `statSettled`, which `statRecord`, the keeper's look at a record,
    `claimIsFresh` and the waiter and request-file stats go through, wait while a staged file younger than
    3 s stands beside the absent name, pausing 5 ms, until 250 ms have passed since they first met one.
    With none they look once at the name itself, taken after the look at the staged file (the rename can
    end between the two), and answer absent only if that finds nothing too. When the 250 ms run out the
    answer is `errRenameInFlight`, an I/O failure and never `ErrNotExist`: `Check` reads it as a record that
    cannot be read (`RecordUnreadableError`, which `confirmFence` retries) and not as "the lease is gone",
    and every stat caller already reads a non-absent error as "cannot be examined". The name whose wait
    ran out is remembered for 5 s, so a staged file left by a writer that died a moment ago costs one wait
    per file and not one per read (a grant reads each heartbeat about seven times under the epoch lock:
    three such heartbeats cost a grant 1050 pauses before the memory and 150 after,
    `TestAFreshLeftoverStagedHeartbeatCostsAGrantAtMostOneWaitPerFile`). A card claim has no staged
    name and costs one open and no stat. The wait is on the same switch as the wait for a refused open
    (`deniedOpenMayClear`): off Windows `readSharedFile` answers an absence at once (one open, no stat, no pause), and `statSettled`
    takes its looks without a pause and answers `errRenameInFlight` beside a staged file younger than 3 s, so a crash leftover that
    young makes a stat site read "cannot be examined" and not "absent" on every platform. The ComfyUI launch markers, which
    Node restamps through a `.tmp` file, are read the same way (`ReadSharedFile`, used by
    `internal/comfyinst`), as are the waiter records and the yield request. The Node readers keep the same
    lists (`STAGING_SUFFIXES` in `render/lease-read.mjs`), and `staging_drift_test.go` fails when a Go
    writer names a staged file without `stagingPath` or stages under a suffix Node does not look for.
    Node returns `null` when its wait runs out (the fence aborts: fail-safe, and Node only reads), and
    keeps no memory of a wait that ran out.
  - **The staged files whose writers hold the epoch lock are swept under it, at any age.** `meta.json.restamp` (the restamp of the
    whole-node record) and `e/<epoch>.json.tmp` (a card record) are staged, and renamed over the live name, only by code that holds
    the epoch lock, from before the file is staged until after the rename returns, and the stall of a Windows rename over a name is
    inside that call (a review's probe saw its exclusive creates succeed at the end of a stalled rename, which then completed over
    them). So a staged file found by someone who holds the lock is a dead writer's leftover whatever its age, and a fresh one would
    make every reader of the absent name wait a whole budget for a rename that will never end. `sweepStagedLeftoversLocked`
    (`devices.go`) removes both: first in the debris sweep (every card grant, and the verification a whole-node grant makes), before any
    record is read, and in the whole-node grant's own section (the restamp file only: one remove, no listing). One try and no retry: a
    file a scanner holds is left for the next sweep, and the lock is not held longer for it. **`hb.<epoch>.tmp` is never swept**: `Renew`
    replaces the heartbeat without the lock, so that file may be a rename landing at that instant.
    `TestAWholeNodeGrantSweepsTheRestampFileAWriterLeft`, `TestACardGrantSweepsTheStagedRecordsAndTheRestampFileWritersLeft` and
    `TestTheWholeNodeVerificationSweepsLeftoversBeforeItReadsAnything` pin the sweep, and
    `TestTheSweepOfLeftoversLeavesTheHeartbeatsStagedFileAndTheLiveFiles` pins what it leaves. Limit: a lock broken as stale after 30 s
    (`epochLockStale`) lets a second holder in beside a first that only stalled, and its sweep could remove the first's staged file,
    which fails that rename with an error and corrupts nothing.
  - **A whole-node grant holds the epoch lock across the epoch, the record and the claim.** The claim is an exclusive create of
    `meta.json`, the name a holder's restamp replaces by a rename under the lock. The grant used to let the lock go as the epoch was
    issued and then build the record and make the claim with nothing held, so a create could land inside a restamp's rename (a
    review's probe on Windows: the create succeeded in 2 of 4 episodes, and the stalled rename then replaced it with the holder's
    record), and "the lock is taken in `bumpEpoch`, so it cannot" was not true. Now `TryAcquire` takes the lock once around
    `bumpEpochLocked`, `finishRecord` and the create and write of the claim, as a card grant does
    (`TestAWholeNodeGrantCreatesItsClaimWhileHoldingTheEpochLock` reads the lock at the instant of the create through
    `beforeClaimHook`; the exclusive create inside a real stalled rename is not reproduced by a test, which would need a production-only
    hook). What the record needs from the process and the session registry (the holder's process start, the owner, the term plan) is read
    BEFORE the lock (`draftRecord`) and finished inside it (`finishRecord`, the one place `RenewedAtMs` and the other instants are
    written), so the section is the epoch's two file operations, the encoding and the create. The verification, the reclaim and the sweep
    of sidecars take the lock themselves and run after it, as before. A grant that loses the create lets the lock go and leaves the
    winner's claim alone (`TestAWholeNodeGrantThatMeetsAnotherClaimLetsTheEpochLockGoAndLeavesTheClaimAlone`).
    **How long the lock is held**, measured on one Windows host with the real gate and the real clock (a scratch test, not in the tree):
    - the grant's own section, 20 grants: 0.5 to 1.6 ms, median 1.1 ms. With a rename that takes 119 ms inside it (the epoch file held
      open without delete sharing, so the rename is refused and retried), five grants: 121.8 to 124.5 ms, and a second acquirer that
      arrived 30 ms in waited 92 to 98 ms and got the lock. That section reads no lease file, so the gate cannot add to it.
    - the verification section, a separate hold, with N live card leases whose heartbeats each have a fresh leftover beside them and no
      live name, and nothing remembered: N=1 253 ms, N=3 761 ms, N=8 2,030 ms, one 250 ms budget per file. With the leftovers already
      waited for outside the lock, as the probe that every grant makes first does: 1.6, 2.7 and 5.9 ms, the probe having paid 256, 760
      and 2,019 ms.
    - a second acquirer's spin gave up after 2,186 ms against a lock that was never released (400 pauses of 5 ms and the file operations
      between them; 2 s nominal).

    So, before the cap, the worst hold was bounded and not removed: N x 250 ms, once per file per 5 s per process, which reaches the nominal
    2 s at eight files and does not fit the measured 2.19 s at nine. What keeps it to milliseconds in practice is the order: the probe judges
    every lease, not only the conflicting ones, before the lock, and a wait that ran out is remembered
    (`TestTheProbeBeforeTheLockPaysTheGateWaitsSoTheLockIsHeldWithoutOne`, for a card grant and for a whole-node grant, counts the gate's
    pauses with the lock held and wants none; `TestTheWholeNodeVerificationPaysAtMostOneWaitPerFile` counts one wait per file). Leftovers
    that appear between the probe and the lock, fresh (younger than 3 s) and not read in the last 5 s, on nine or more leases, held the
    lock past a second acquirer's spin, and it failed loudly (`the epoch lock ... did not clear`) and never granted.
  - **A hold of the epoch lock spends at most one second of the gate** (`lockGateCap`, half of the 2 s a second acquirer spins). `withEpochLock`
    registers the hold for its lease directory (`beginEpochHold`, `gate.go`), and once it has run that long on the gate's clock every
    further read of a lease file in that directory (`readSharedFile`, `statSettled`) that would wait for a rename in flight answers
    `errRenameInFlight` at once, with no wait and no memory. That is the answer a wait that ran out gives, and every decision under the lock
    reads it as held (unknown is never stale), so nothing is decided differently: the wait would only have turned an unknown heartbeat into a
    fresh one. It covers the reads of that directory by whoever makes them in this process, so a read in another thread during a spent hold
    gets the I/O failure instead of a wait (held or unexaminable to every reader); it never applies to a read outside a hold; the launch
    markers, in another directory, are not covered. `TestTheGateWaitsOfOneHoldOfTheEpochLockStayInsideTheSpinOfASecondAcquirer` (three, nine and
    twelve leases) and `TestTheGateWaitsOfACardGrantThatMeetsLeftoversAfterItsProbeStayInsideTheSpin` pin the bound on the gate's clock, and
    `TestTheCapOfAHoldOfTheEpochLockIsPerHoldAndEndsWithIt` its scope. The measurements above are from before the cap.
  - **The wait is Windows only.** Off Windows nothing is delete-pending (`rename(2)` is atomic and an
    open descriptor never holds one up), so a permission error on a record is a lasting refusal and
    `readSharedFile` returns it at once. The choice is the constant `sharedOpenMeetsDeletePending`
    beside `OpenShared`, and `TestOffWindowsAPermissionErrorIsReturnedAtOnce` pins that the open is
    made once. With the wait on, a mode-000 record cost about 108 ms per read: `Leases()` 108 ms per
    such record, and an acquirer, which reads the records under the epoch lock, 4.2 s with eight,
    enough for a second acquirer to give up on the lock's 2 s. Those figures are from a scratch
    measurement in a Linux environment, recorded in the change fragment, not from a test in the tree.
    On Windows the same lasting refusal is waited out once per file (the bullet after next).
  - **The Node readers wait the same refusal out** (`render/lease-read.mjs`). `readLease`, `readEpochRecord` and
    `readCardClaim` (`render/gpu-lock.mjs`; the three behind `checkInheritedLease`, the fence a render runs before an
    irreversible action) and the lease-progress reader read these files through `readSharedSync` / `readSharedJson`.
    libuv opens with `FILE_SHARE_DELETE`, so Node never meets the sharing violation; it does meet the delete-pending
    instant, as `EPERM` (a file held delete-pending reads exactly that), and it meets `ENOENT` twice: once the last handle on a delete-pending file closes, and for the tens
    of milliseconds a rename that was not refused leaves the live name absent (the gate above). A
    `null` record for a card lease falls through to `meta.json`, which a card lease never writes, so a refused open read
    as "this lease is not current" and the render aborted. The helper retries `EPERM` and `EACCES` until a 100 ms deadline,
    pausing 5 ms with `Atomics.wait`, waits for a rename while a fresh staged file stands beside an absent name (the
    bullet above), and returns `null` at once for any other error or for a missing file with nothing beside it; the
    waits are on only on Windows (`policy.waitsOutDenied`). The bound is a deadline and not a count of pauses because
    `Atomics.wait(5)` sleeps a whole timer tick on Windows (15.4 ms measured), so 20 pauses were 308 ms. Plain
    `readFileSync` missed nothing in about a million reads against 55,000 renames issued (half of those runs renamed a link of one
    seed file, which replaces nothing after the first pass; the fragment says so), so the permission wait guards an instant that
    measurement could not provoke. The absent-name window it also waits out was provoked, by a Go writer with the write pattern of
    `renameReplacing` (the release lane's harness; and in a control of this round, one raw `ENOENT` in 295,200 reads with the name
    absent 16 ms), not by a Node writer, and the waits are shown by the injection tests. `render/lease-read.test.mjs` (the helper and the five readers, with the refusal
    injected) is the proof of the permission wait, `render/lease-read-gate.test.mjs` of the wait for an absent name, and
    `heartbeat_unknown_test.go` of the reclaim rule. `render/lease-read-stress.test.mjs` pins none of them: with the permission wait
    taken out it still passed 5 of 5 on Windows, because Node opens with `FILE_SHARE_DELETE` and the refusals it counts are the
    writer's. What it pins is that no reader misses, at its size, under a rename-over. A writer in another process renames a
    different inode over each of two lease files in turn (the record and `meta.json`, two seeds per file, alternated; it does not rename
    over the card claim, which Go never does and no reader waits for) while the five readers loop. The writer
    OBSERVES that each rename replaced something: after it, the live name has the inode it just installed, and the test wants that of
    every rename. It also wants some of the writer's renames refused on Windows (the writer and the readers really met: 3 to 37 per
    612 to 970 renames on one host, 14 to 30 a run in the release lane's runs) and none elsewhere, where `rename(2)` is atomic, so
    it cannot pass without having raced. Its first form linked one seed, which on Linux died on `EEXIST` at the link of the third pass
    (36 renames in, at six lease roots) and on Windows renamed a link onto itself: only the first rename of each file replaced
    anything, 18 of 11,052 at the six committed roots. It ran six times on Windows (every rename a replacement, 612 to 738 renames a run) and seven times on
    Linux under WSL (11,000 to 18,000 renames a run, 0 refused, 0 misses). CI runs it on Linux only (`node --test render/*.test.mjs` is a
    step of the Ubuntu job), so its Windows assertions run on gated hosts only.
  - **A file that is not there is not waited on unless a rename may be installing it, and no read of one under the epoch lock
    multiplies a wait.** `readSharedFile` retries a permission error, and waits for a rename only while a fresh staged file
    stands beside the name; a missing file with nothing beside it is one open, one look at the staged file and one at the name
    (none for a claim), and the error, with no pause (`gatePauses` counts the gate's, `readRetries` the refused open's). After a release,
    `Leases()` opens `meta.json` once, `EpochIsCurrent` opens `e/<epoch>.json` and `meta.json` once each, and the
    heartbeat readers open `hb.<epoch>` once, with no pause
    (`TestAReleasedLeaseReadsAbsentAtOnceAndNoReadWaitsForIt`); a grant whose sweep finds the three records it reads deleted
    under the lock opens each once and pauses never (`TestAMissingOrDeletedRecordCostsOneOpenAndNoWaitUnderTheEpochLock`).
    The reads that wait under the lock are of a file that exists and is refused, and of a name that is absent with a fresh
    staged file beside it (the gate above). A record's writers hold the lock and the sweep removes their leftovers before it
    reads anything (the bullet after the gate), so the second is in practice a heartbeat: for the instant `Renew` swaps a
    live lease's heartbeat (`Renew` does not take the epoch lock) that is one wait that clears; what keeps the sweep from
    judging a live lease stale is the reclaim rule (an unreadable heartbeat is unknown, and unknown is never stale), and the
    wait only narrows the window in which it is unknown. `readRetries` counts the pauses of the refused open and `gatePauses` those of the
    gate, as test seams.
  - **A file refused for good is waited out once, not once per read.** A refusal still there when the wait ends is a
    lasting fault (a deny-read ACL on a lease file, a foreign delete whose handle never closes), not a rename in flight.
    A grant reads each record five times and each heartbeat seven inside one hold of the epoch lock (the sweep, the scan
    for the unreadable and the verify each go over them), so the wait paid per read held the lock for about 500 ms per
    refused record. `readSharedFile` therefore remembers a file that outlasted its wait for `refusedMemory` (5 s): a read
    of it in that time opens it once, so a repair is read at once, and takes no pause if it is still refused; the first
    open that succeeds forgets it. The memory is per file, and a refusal that clears inside the wait is never remembered,
    so the heartbeat of a live lease keeps its whole wait. Pauses for a grant of a free card with three leases held,
    the refusal injected at the open seam: one refused record 100 before and 20 after, one refused heartbeat 140 and 20,
    six refused files 360 and 120 (654 ms). With real deny-read ACLs on a scratch root and two acquirers 30 ms apart: one
    record 541 ms and 958 ms before, 113 ms and 111 ms after; six files 1.97 s and 3.57 s before, 648 ms and 624 ms
    after. What remains is one wait (about 110 ms) for each refused file, so a second acquirer's 2 s spin is reached
    at about eighteen files refused at once. `TestALastingRefusalOfOneRecordIsWaitedOutOnceByAGrant`,
    `TestALastingRefusalOfOneHeartbeatIsWaitedOutOnceByAGrant`,
    `TestEveryRecordAndHeartbeatThatIsLastingRefusedIsWaitedOutOnceByAGrant` and
    `TestAFileRefusedForGoodIsWaitedOutOnceNotOnEveryRead` pin the counts; three more pin that a repair is seen at once,
    that the memory is per file, and that it runs out.
  - **Linux.** Nothing is delete-pending, so no read waits and none is missed.
    `TestOnLinuxConcurrentRenameOverOfARecordAndItsHeartbeatIsNeverMissedAndNeverWaitedFor` rewrites a held lease's record
    (`Restamp`) and heartbeat (`Renew`) in loops while `Leases()`, `EpochIsCurrent` and the heartbeat read poll: 50 runs
    in a Linux environment, 0 misses in about 450,000 reads, 0 refused opens, 0 pauses. The Node readers on Linux are covered by
    `render/lease-read-stress.test.mjs` (above), which wants no refused rename there.
  - The epoch counter and the tokens are still read with `os.ReadFile`; no finding covers
    them yet. The waiter records, the yield request and the ComfyUI launch markers are read through the shared open.

Neither of the first two is defensive padding: with waiters polling once a second, both races are
ordinary traffic. Measured under six concurrent acquire/release workers, 1 in 48 cycles failed
before the retries were added.

### What each consumer does with a lease file it cannot read, or finds mid-rename

A read or a stat that gives up on a rename in flight answers `errRenameInFlight`: a `*fs.PathError`, so `isIOFailure` is true for it, and
never `ErrNotExist`. The rule for every consumer of it, and of any other failure that is not an absence, is one: the lease is **held**, or
the file **cannot be examined**; it is never free and never gone. Every call site that branches on `isIOFailure`, `RecordUnreadableError`
or `fs.ErrNotExist` on a lease path was read against it, and so was every consumer of the gated reads (`readSharedFile`, `statSettled`,
`statRecord`). By what each does:

- **Held or refused (fail closed).**
  - `Lease.Check` and `checkV2` answer `RecordUnreadableError`, not "gone". `confirmFence` retries it for a window; a wrapper that still
    cannot read the record goes on, and never kills its command, releases its lease or queues again on it.
  - `Restamp`, `restampLocked`, `restampV2Locked`, `restampWholeLocked`, `Lease.Release`, `ReleaseByEpoch`, `releaseByEpochV2`,
    `ReleaseTarget`, `unreadableCardRecords` and `RecordState` answer a `RecordUnreadableError` or an error that names the record, and a
    release that cannot tell leaves the lease releasable. `Restamp` and `restampLocked` send a look at `e/<epoch>.json` that fails for any
    reason but absence to the card path, which reads the record itself. `gpu release` and the host-RAM wait read an unreadable record as still
    held (`leaseStillHeld`, `stillHeldError`), and `stopInstancesOfLease` leaves the instances kept under a lease whose record cannot be read.
  - Under the epoch lock: `claimIsFresh` (a claim that cannot be examined is a claim in progress, not debris), `keeperAlive` (an unreadable
    record is held while its keeper is alive, and a heartbeat that cannot be told counts as alive), the claim loop of `TryAcquire`,
    `removeStaleClaim` and `grantDevicesLocked` (each asks `keeperAlive` before it removes a claim that cannot be read),
    `sweepDebrisLocked`, `keptUnreadable` and `verifyNoDeviceLeases` (a card record that cannot be read, whose keeper is alive, stays with
    its claims and refuses a whole-node grant). The claims are the second arbiter: a card claim is created once, exclusively, and never
    renamed over, so a grant over a card whose claim stands is refused whatever the reader could see.
  - The reclaim rule: `heartbeatAt` (unknown is never stale) and `reader.judge` with `Reclaimable` (a launch marker being installed is
    unknown, never "no instance").
  - `EpochIsCurrent`, and the render-child fence and the recipe audit built on it, answer false for a lease they cannot read: the job
    does not start. They read; they never grant. The Node readers answer `null` and the fence aborts: loud, and fail-safe.
- **Left alone (nothing is done).**
  - `Waiters` skips a waiter record it cannot read or stat for this read and never prunes it (`readWaiterFile`, `statWaiterFile`);
    `readToken` leaves a token it cannot read.
  - `ReadYield` reads a request file it cannot read as "not asked", so a holder does not yield on it; an asker whose look at the file
    cannot tell goes on to the re-check under the epoch lock, which refuses to write over a request file that exists and cannot be read.
  - `AdvanceTerm` (`recordOf`) and `Lease.HostRAMHold` return an error for a record they cannot read and write nothing: the holder's next
    tick asks again, and the hold bound releases nothing on an error.
  - `comfyinst.StopForLease`, `StopMarker` and the checks that read a marker again before a stop leave an instance running when its
    marker cannot be read.
- **Skipped by an unlocked reader.** `reader.v2` and `reader.legacy` (behind `Inspect`, `InspectFor`, `Leases`, `InspectDir`,
  `InspectDirDetail` and `EpochIsCurrent`) skip a record they cannot read, and a directory listing taken in the instant of a rename does
  not show the name. For that instant an unlocked caller sees the lease as not live. That is unchanged by this round. What a listing
  no longer decides: a grant or a sweep lists under the epoch lock, where no record can be in flight (the writers of the records hold
  that lock, and the sweep has removed their leftovers first) and the heartbeat is read by its name and not found by listing; the
  keeper's scan of every heartbeat (the case of a whole-node record) counts a fresh staged heartbeat whose live name is not listed as
  unknown; and a release that names no epoch (`gpu release`, `ReleaseTarget`) decides "what is held" from a listing without the lock, and
  counts a fresh staged card record whose live name is not listed as a record that cannot be read (`unreadableCardRecords`), so it
  refuses with the error that names it where it used to release the other card lease, or say that nothing was held. Advisory callers
  (`gpu status`, the admission and routing checks, the vision gate, `gpuLeaseHeld` of the fleet baseline, which says so) can still see
  a lease as free for it; the wait narrows that to the case where it ran out.
- **Not reachable by the new error.** `readEpoch` reads the epoch counter with a plain `os.ReadFile` and reads an absence as the first
  ever acquisition, but its callers hold the epoch lock and its only writer holds it too, so no rename of the counter is in flight at
  that read. The session registry, the orphan marker, the progress file and the reader audit marker are advisory (nothing there kills or
  releases anything): an unreadable one reads as not registered, not orphaned, progress unknown, and "unreadable".
- **Known limits.**
  - `PIDsOfOtherLeases` (the live ComfyUI instances of another lease that a wrapper which lost its lease spares when it stops its own
    tree) skips a marker it cannot read, so that instance is not spared. Neither direction is safe: the other would leave the command
    of a lease that was lost running on the card.
  - `leaseDevicesOf` (the cards a detached hold drains and unloads for its lease) reads a lease whose record is absent as the whole
    node's, and says so on stderr; the drain and the unload then act for the whole node. A lease it does not find in the listing whose
    record cannot be read (a rename is installing it) is not that: it asks `RecordState`, lists again when the record is readable by
    then, and otherwise fails the maintain step with the lease held (`detachedMaintainError` says so, and that `gpu release` frees it),
    instead of widening the drain and the unload to seats on cards the lease does not hold.

## Card-scoped leases (record v2)

*Plan P2 of the per-card routing work, register C-86. Off by default: with `gpu_card_scoped_leases` unset
nothing in this section changes what a host does, and the existing whole-node suite runs unchanged.*

**Why.** The lease was whole-node. A render on one card of a three-card box fenced the other two as well, so the box ran
one job at a time while two cards sat free. A lease may now name the cards it holds, by GPU UUID (never by index: the
three index spaces of `nvidia-smi`, `CUDA_VISIBLE_DEVICES` and ComfyUI disagree). Two leases conflict when either is
whole-node or their card sets intersect; everything else runs side by side.

**What is on disk.** A whole-node lease is exactly what it always was: `meta.json` is its record. A device lease never
writes `meta.json`:

| file (under `lease/`) | meaning |
|---|---|
| `e/<epoch>.json` | the lease record (the whole-node `Meta` plus `devices`, `group`, `wrapper_version`, `state`); authoritative |
| `meta.json`'s `format` | on every record this binary writes, whole-node or device (`RecordFormat` = 2): who wrote it. A record without it was written by an older binary, which could not name cards, and is the only kind the legacy-scope inference may narrow (`Info.Legacy`) |
| `cards/<id>.claim` | one per held card, `{"epoch":N,"at_ms":T}`; created exclusively |
| `hb.<epoch>` | the per-epoch heartbeat, as for a whole-node lease |
| `../FORMAT` | `2`, written with the first device lease on the host |

**The writer is a per-host switch, and the config key alone does not turn it on.** `gpu_card_scoped_leases` (default
`false`) asks for it; the writer is enabled only when the host also carries a **green reader audit**, the marker
`gpu/reader-audit.json` (`{"result":"green"}`) beside the epoch counter, which `gpu doctor --write-audit` is the one writer of
and writes only once no binary or Node reader on the host predates the per-epoch fence (`ApplyCardScopedConfig`). Off, or
asked without the marker, an acquisition that names devices is refused with `ErrCardScopedOff` and the CLI warns, naming the
marker. *Reading* is never gated: every inspector understands a card-scoped directory whether or not this process writes
one.

**The fence is per epoch.** A device lease is current while its record is `active` and every card it names carries a
claim naming its epoch. Nothing compares two leases' epochs, so a higher-epoch lease is never fenced out by a lower one.
The same rule is applied in `Lease.Check/Renew/Release/Restamp`, `ReleaseByEpoch`, `gpulease.EpochIsCurrent` (which
`gpulock` and the pipeline's inherited-lease boundary call) and the `render/gpu-lock.mjs` reader.

**Consumers judge every live lease, and the inherited-lease exemption is per lease.** `Info` describes the lowest live
epoch; when more than one lease is live it also carries each of them (`Info.Leases`, walked with `Info.Each()`). The text
gate (`modelaffinity.BlocksNewRun`), `delegate.Fenced`, `ForeignFence` and `Reserved` ask their question of each lease, and a
process whose `GPU_LEASE_EPOCH` names lease A is exempt from A's fence and from no other lease's. (An earlier draft of this
change read "inside any live lease" as an exemption from all of them, which let a child of one device lease walk through an
exclusive or draining reservation on other cards; `TestForeignFenceExemptsOnlyTheLeaseTheProcessRunsUnder` pins the narrow
rule.) The consumers compare device sets since plan P4: a lease on card 0 no longer fences a text call whose seat sits on card
1 (see "Consumers read a seat's cards, not the node", below).

**Grant.** One critical section under the epoch lock, after a read-only probe so a one-second poller does not spin the
lock: create `e/` and `cards/`; sweep debris; refuse while a live `meta.json` exists; refuse while a live device lease
intersects; issue the epoch; write the record `granting`; create each card claim exclusively (all or nothing: any failure
removes what was made); flip the record to `active`. A whole-node grant keeps its own atomic token (`O_EXCL meta.json`),
created in the same critical section as the epoch bump and the record (a holder's restamp replaces that name by a rename under
the same lock, so a create cannot land inside it; the bullet on the whole-node grant below) and, once a device lease has ever
existed on the host (or whenever `e/` cannot be proven absent), verifies under the epoch lock, in a second section, that no
device lease is live and withdraws its claim if one is. Before it judges, it **sweeps** every device
lease the shared rule calls reclaimable: a holder that is alive but stalled (a suspended or lid-closed session) has its
record, claims and heartbeat removed, so when it resumes its per-epoch fence fails and it cannot renew over cards the
whole-node holder now owns. A whole-node reclaim fenced the old holder by deleting `meta.json`; this is the same act. Because the device grant creates `e/` before it looks at `meta.json`, a whole-node claim
that lands after that look is guaranteed to see the directory: the two paths cannot both win.

**Debris.** A crash mid-grant leaves a `granting` record and/or claims. Past `claimGrace` (10 s) the next acquirer removes
them, one lease at a time and only that lease's files; before it they count as a grant in flight. A dead holder's lease
reads as free to every reader at once (the same reclaim rule as a whole-node lease), and an acquirer that wants its cards
removes it; **expiry never frees a card and nothing here kills a process**.

**Queue.** Waiters carry their device set. FIFO is among waiters that conflict: a waiter ahead of you that wants other
cards does not hold you back (disjoint backfill), and a whole-node waiter wants everything, so it is a barrier that every
later waiter queues behind. A waiter whose process is gone or stopped polling is skipped exactly as before.

**What a reader that predates this format sees.** A directory holding only device leases reads as a **free** card to any
binary older than this change (and to any Node reader that still compares `meta.json`). That is the one cross-version
hazard, and it is gated by a mechanism, not by good intentions: the writer needs the green reader audit marker (above).

**Deviation from the plan, accepted (2026-10-12).** Plan rev 2 (section 2, P2 scope) specified a synthetic pid-0 `meta.json`
"umbrella" over any set of device leases, so that a pre-v2 writer would queue behind it. This change does **not** write
one. The premise of the umbrella was checked against the real binaries on the build host and **holds**: a `meta.json`
with `holder.pid = 0` and a future `expires_at_ms` makes `gpu reserve` of 0.158.3 and of 0.160.0 queue and then refuse
("GPU held by media (pid 0 ...)"), and the same record with a past window is reclaimed and granted. So the umbrella was
dropped for a design reason, not because it would not work: it is a second source of truth that would have to be
re-stamped on every grant, release and extension and be judged by every reader as "not a real lease", and its only
audience is a binary the audit is meant to find and replace. The cost of dropping it is that a pre-v2 writer that is
**not** found by the audit double-books a host that has the switch on (measured with a byte-identical older wrapper in the
review of this change). **The plan owner accepted the deviation (decision of 2026-10-11, recorded here with 0.183.0):
the umbrella stays dropped.** The safety trade-off was checked, not assumed: a `gpu doctor` run on the reference box on
2026-10-11 was green, meaning every reader it reached there (the CLI, the MCP servers, the fleet daemon, the render lock
script) understood the per-epoch fence and nothing older than the record format was found, so the audit gate below is
doing the work the umbrella would have done. The umbrella can be restored later without a format change if a host ever
runs a reader the audit cannot reach. No 0.152.x build is on the build host, so none was tested. The four tests the plan
named for the umbrella (`TestUmbrellaIsPidZeroAndRestampedOnGrantAndRelease`, `TestWrapperOfHigherEpochNotKilledByUmbrella`,
`TestLegacyReclaimDoesNotFreeUmbrellaWhileSiblingLeaseLive`, `TestOldReaderFixtureSeesHeldOverV2Dir`) were never written and
are **superseded**: there is no umbrella to pin, and `TestOldReaderFixtureSeesFreeOverV2DirAndTheFlagIsWhatGatesIt` pins the
hazard that replaces the last of them. Two hard gates stand in for the umbrella, and the second is now code:

1. the switch stays off until the reader audit is green (`gpu doctor` scans every binary copy including the
   node-swap backups an automatic rollback restores, the images of running fleet-node and MCP processes, the deployed
   `local-agent` binary, a media repository's wrapper copy under whatever name it carries, and the Node readers; plan P6 is
   blocked until it is green **and** that green covers `local-agent` and every media repository, see "What green does and
   does not mean" below);
2. `ApplyCardScopedConfig` refuses to enable the writer without the marker, so nothing in the process can write a device
   lease on a host nobody audited. `TestApplyCardScopedConfigNeedsAGreenReaderAudit` and
   `TestOpenLeaseKeepsTheWriterOffWithoutAGreenReaderAudit` pin it, and
   `TestOldReaderFixtureSeesFreeOverV2DirAndTheFlagIsWhatGatesIt` pins the hazard itself against the preserved pre-change
   reader.

**Not in the record change (P2).** The CLI, the allocator, the per-card status and the audit (P3, below); consumers that take
a device set instead of reading any live lease as a held node (the text gate, the delegator and the placement table: P4,
below; drain and unload: P5, below; fleet health: P7, since shipped: see "The fleet reads leases per card"); owners, terms and takeover (P8 to P12). A consumer that is not device-aware
reads any live lease as fencing the whole node, which over-fences and is the safe direction; the one exemption is per lease
(above).

### Reserving cards, the card table and the reader audit (plan P3)

*Everything here is behind the same per-host switch: a host that has not enabled card-scoped leases reads no card table
for a reserve that names nothing, derives nothing from the command, and holds the whole node exactly as before.*

**The card table.** `local-offload gpu cards [--json]` (and the table inside `gpu status`, and `offload_status` with
`section: "brief"`, key `gpu_cards`) lists every card once: nvidia-smi index, name, the display flag, free and total VRAM,
utilisation, the ComfyUI order, and the lease that holds it (epoch, class, group, command, and whether it holds the card
by name or because it is a whole-node lease). The UUID is the key, because it is the only identity the three index
spaces cannot disagree about. **The ComfyUI order is `?` unless it is declared**: nvidia-smi reports no FASTEST_FIRST
position, and two same-model cards cannot be ordered by anything it does report, so the table never guesses; set
`gpu_comfy_order` (every card once, fastest first, as nvidia-smi indices or UUID prefixes) after measuring it with a
`CUDA_VISIBLE_DEVICES=<uuid>` probe. A box with one card needs no declaration. With no nvidia-smi the verbs still print the
leases and say there is no table.

**The display card's row says where it stands against its cap** (`display_cap`; the display card's row, plus every row of a reading that could not say which card the monitor is on (there as `unknown`); otherwise absent on every
other card and on a box that flags no display card). It reads the VRAM in use now, **the desktop and every process counted**,
against the budget `gpulease.DisplayBudgetGiB` gives from `gpu_display_cap_pct` and the display layer's declared floor, which is
the arithmetic the allocator admits by, so the two cannot disagree. The cap is read from the config file as it is when the call
is made (`LiveGPUDisplayCap`), so a `0` written while the MCP server runs shows in the next brief. Keys: `cap_pct`, `budget_gb`,
`used_gb`, `used_pct`, `over_cap`, `over_by_gb` (present exactly when the card is over: four decimals of a GiB, never rounded to nothing,
and the text line says `OVER by less than 0.1 GiB` under 0.05 GiB), plus `closed` at a cap of `0` (the operator's "I need the card": nothing is
judged) and `unknown` with its reason when the card has no total or the reading could not say which card the monitor is on
(`display_unknown` on every row of that reading: the allocator closes such a card too, and the text table says it once). The text
table keeps its header and gains one `display cap:` line under the display card. It is one reading of the card and nothing more:
it cannot say **whose** memory it is, so a llama-swap seat the allocator never placed counts exactly as the desktop does, which is
the point (it covers a seat pinned onto the display card by any means), and a model loading can flash the card over for a moment.
The brief reads the nvidia-smi sample its verdict already took; that sample now carries `gpuactivity.GPU.AttachedUnknown`, so the
brief too says "not judged" on a reading that could not tell the monitor's card, and no longer shows no display card at all
(`TestBriefGPUCardsCarryTheDisplayCap`, `TestBriefReadsTheLiveDisplayCapNotTheOneAtLoad`,
`TestBriefSaysWhenTheReadingCouldNotSayWhichCardTheMonitorIsOn`, `TestDisplayCapAtZeroIsClosedNotOver`).

**Reading the table under load (F24, 0.178.0).** The table is one nvidia-smi exec of `gpuprobe`'s per-device query
(`--query-gpu`: index, uuid, name, memory, utilisation, the two display columns) and nothing else: the allocation never
lists processes, the slow phase of nvidia-smi under load, which is the separate, best-effort foreign-busy reader's call. Two
tests pin it: the query's columns (`TestTheCardTableQueriesTheDeviceFieldsAndNeverListsProcesses`) and, by what the allocation
EXECS against a stand-in nvidia-smi, that it runs that one query and no other (`TestTheAllocationNeverListsProcesses`). The
utilisation column is not needed by the allocator; it is kept because the parser is positional (a query without it reads
display_attached as display_active, so the operator's screen would look free) and the views that print it share the reader. It
adds about 20 ms to an exec of about 115 ms (paired median of 30 runs on a quiet box; not measured under load), so the lean
variant, which would need a header-aware parser, is not worth the risk to the display guard.
A read that takes a tenth of a second on a quiet box has run out five on a loaded one (2026-10-09: a media call's allocation
re-read ran out its 5 s while two cards ran other sessions' renders). Every reader that **decides** something from the table
therefore goes through `gpualloc.Deps.CardTable`: one attempt under `DefaultCardRead` (5 s) and, when that attempt ran out of
time while the caller's own context was still good, **one** more under `DefaultCardReadRetry` (15 s). The reader below it
(`gpualloc.ReadCards`, also the root package's `cardTableFn`) adds no deadline of its own, because one that capped itself at 5 s
would make the 15 s retry a 5 s one; that is pinned by a run of the production reader against the stand-in
(`TestTheProductionReaderIsNotCappedBelowTheRetryDeadline`, and `TestThePatientReaderRunsTheProductionReaderThroughOneSlowNvidiaSmi`
through the root package's wiring). A failure that comes back at once (nvidia-smi not on PATH, a table with no card) is not
retried, since a longer deadline cannot fix it, and the error of a read that failed twice says so (`read twice: no answer within
5s, then none within 15s`). The deciding readers are the allocator's input (the media path and `gpu reserve --cards`), what `gpu
reserve` resolves from the table (`--cards`, `--devices`, a command's own pin, and the wrapper's check of the pin the command
inherited once the lease is held), `node-swap --cards`, and the scope of a drain or an unload (without a table every seat counts
as on the leased cards, so one slow read would unload the seats on the other cards). `gpu cards` and `gpu status` stay at one
attempt: "no table" is a fine answer for a view. The seat admission gate (`modelaffinity`, ADR 0026) keeps one 5 s attempt too,
on purpose: it reads the table under a mutex, remembers the answer (a failure too) for 2 s and is polled every second, and every
doubt there fences (a table it cannot read counts as every card, the answer the gate gave before cards were leased), so a slow
read only holds a text load behind a media lease on another card until a later poll's read answers, whereas an inline 15 s
retry would hold the mutex against every other poller of the gate. What a media call does when even the second attempt fails is in
[media-generation.md](media-generation.md#a-card-table-that-runs-out-of-time-is-not-a-refusal).

**Choosing the cards of a reservation.**

| form | meaning |
|---|---|
| `gpu reserve --devices 0,GPU-aaaa ...` | exactly these cards (an nvidia-smi index or a UUID prefix of at least four characters; ambiguous or unknown is an error). The operator's word: the display card is allowed, and a busy card is a place in line, FIFO behind its holder. |
| `gpu reserve --cards 2` or `--cards 1..3` | the allocator picks (below). The display card is allocatable up to `gpu_display_cap_pct` (default 60 percent of its VRAM, the desktop's own use included) at any presence, only with a declared `--vram`, and never when the cap is 0 ("The display card is allocatable up to a cap", below). |
| `gpu reserve --whole-node ...` | everything, as before. |
| `gpu reserve ... -- <cmd>` | the cards `<cmd>` names itself: `CUDA_VISIBLE_DEVICES` (a UUID, or an index in PCI order when `CUDA_DEVICE_ORDER=PCI_BUS_ID`, else in ComfyUI order), else `--cuda-device N`, else `COMFY_CUDA_DEVICE`. Nothing named means the whole node. |

`--devices`, `--cards` and `--whole-node` are mutually exclusive. On a host without card-scoped leases `--devices` and
`--cards` are a **hard error** (`ErrCardScopedOff`, naming `--whole-node`), because the reservation would not mean what it
says. Evidence from the command that cannot be turned into a card (an index in an order nobody declared, a UUID no card
carries, no nvidia-smi) degrades to a **whole-node lease with a stderr note**, never a guess and never a failed reserve:
the wider fence is the safe direction. The wrapped command is handed `GPU_LEASE_DEVICES` (the lease ids it holds, comma
separated, empty for the whole node). `--group` labels leases taken together for one job.

**A lease holds cards; it does not confine the command.** Nothing in the lease stops a process from using a card it does not
hold, and an unpinned CUDA job runs on the fastest-first card, which on the reference box is the display card (the card the
allocator refuses to hand out) while the held cards sit idle. So when the cards were **named** (`--devices`) or **allocated**
(`--cards`), the wrapper also gives the command `CUDA_VISIBLE_DEVICES=<driver UUIDs of the held cards>` and
`CUDA_DEVICE_ORDER=PCI_BUS_ID`, which pins reliably (plan P13 spike). It does **not** when the lease is the whole node, or when
the set was derived from the command's own pin. A command that pins itself with a variable of the program's own
(`COMFY_CUDA_DEVICE` or `COMFY_CARD_UUID`, which the render layer turns into the card ComfyUI runs on, and ComfyUI resets
`CUDA_VISIBLE_DEVICES` from it, so setting that variable would be futile) is **re-pinned**: when its pin falls outside the held
cards or cannot be resolved, the wrapper rewrites that same variable to the held card, says so on one line, and starts the
command ("A pin outside the lease is moved, never refused", below). The program's own argv (`--cuda-device` on its command line)
is not rewritten; the wrapper warns on stderr when such a pin falls outside the held cards or cannot be resolved. A
`--cuda-device` in `COMFY_EXTRA_ARGS` is not read by the wrapper at all: nothing is printed for it, and the render layer drops it
whenever `COMFY_CUDA_DEVICE` or `COMFY_CARD_UUID` is set (`launchFlags` in `render/comfy-lifecycle.mjs`), so an environment pin
beside it IS re-pinned and wins; a pin in the extra args alone is unconfined, the trespass `gpu status` flags (F34). A `CUDA_VISIBLE_DEVICES` the command merely **inherits** from the operator's shell is replaced with the held cards when it
reaches outside them or cannot be resolved (and the wrapper says so); one inside the held cards is tighter than the lease and
is left alone (`TestConfineWrappedRules`, `TestGPUReserveDevicesPinsTheChildToTheHeldCards`,
`TestGPUReserveReplacesAnInheritedPinThatReachesOutsideTheLease`). Two limits remain: a program that sets its own
pin after it starts is not confined (ComfyUI's `--cuda-device` overwrites `CUDA_VISIBLE_DEVICES`; one instance per card is
plan P13; a launcher that picks its card inside the child, for instance a film recipe whose own code defaults
`COMFY_CUDA_DEVICE` to a fixed index, is invisible to the wrapper and stays what `gpu status` flags as a possible
device-trespass, F34), and `GPU_LEASE_DEVICES` is for a command that wants to read the set itself. `--drain` and `--unload-seat` take
the cards the lease holds and leave the seats on the others (plan P5, below).

**A pin outside the lease is moved, never refused (GPU-F9, 0.183.0).** A reserved lease (`--devices` or `--cards`) names the card the job runs on. A command that carries a card pin of its own
that points elsewhere used to start with a stderr warning and run on the other card. Refusing it (an earlier design) turned a
card the operator named into a failed job; the rule is the one the house applies everywhere else, a busy or wrong card is
a place to move to, never a refusal. The wrapper therefore **rewrites the program's own variable to the held card**, prints
one line naming the old pin, the new pin and the lease, starts the command, and never exits non-zero because of a pin.

| the command carries | outside the held cards, or cannot be resolved | inside the held cards |
|---|---|---|
| `COMFY_CUDA_DEVICE=<index list>` | the variable becomes the held card's **ComfyUI device index** from the card table (`gpu_comfy_order`), never the nvidia-smi index the lease is keyed by; the launch stays an unkeyed one | untouched, nothing said |
| `COMFY_CARD_UUID=<uuid>` | the variable becomes the held card's **driver uuid**; no index is written beside it | untouched; the lease still pins the rest of the command as before |
| `--cuda-device N` on the command line | **not rewritten** (the program's own argv): the stderr warning stays, and an environment pin beside an argv one is not moved either, because the program obeys its argv | untouched |
| `--cuda-device N` in `COMFY_EXTRA_ARGS` | **not read** by the wrapper, so no warning: when `COMFY_CUDA_DEVICE` or `COMFY_CARD_UUID` is set beside it the environment pin is re-pinned as above and wins (the render layer drops the extra args' flag), and with no environment pin it is an unconfined pin the wrapper does not see (F34) | untouched |

Rules, each pinned by a test (`gpu_reserve_repin_test.go`, `internal/gpulease/repin_test.go`):

- **The kind of pin is kept.** The render layer refuses an index beside a card uuid (`COMFY-INSTANCE-CONFLICT`), and a uuid turns
  an unkeyed launch into a keyed per-card instance with its own port and directories. An index pin becomes an index, a uuid
  pin a uuid.
- **No guess.** When the held card's device index is not known (`gpu_comfy_order` is not declared and the box has several
  cards) an index pin is **not** rewritten and the old warning stays, with the reason added (`[not re-pinned: ...]`); the same holds
  for a pin of more cards than the lease holds. When nothing else would have said anything (an inherited `CUDA_VISIBLE_DEVICES`
  that names a held card makes the generic confinement silent), the wrapper prints that sentence itself: a pin it cannot move
  is never a silent trespass. Switching
  an index pin to a uuid to get around that would start a different instance on another port, which is not a move toward the
  leased card but a change of launch. A uuid pin needs no order and is always moved.
- **The order is the child's.** A device index is counted in ComfyUI's fastest-first order unless the child's environment says
  `CUDA_DEVICE_ORDER=PCI_BUS_ID` (the command's own, or the one the wrapper sets when it replaces an inherited
  `CUDA_VISIBLE_DEVICES`), which makes it nvidia-smi's. The pin is judged and rewritten in the order the child will read it.
- **k cards under n >= k.** A pin of k cards under a lease of n >= k cards maps to the first k held cards in the order the index
  is counted in (ascending, as a comma list). A pin of more cards than the lease holds has no card to put the extra ones on and
  keeps the old warning. A repeated index is one card.
- **A program pin is judged on its own.** The shell's inherited `CUDA_VISIBLE_DEVICES` is replaced as before when it reaches
  outside the lease, and left alone when it is inside; a `COMFY_CUDA_DEVICE` or `COMFY_CARD_UUID` beside it is judged and moved
  independently, because the program resets `CUDA_VISIBLE_DEVICES` from its own pin.
- **A blank value is no pin.** A uuid-bound launch blanks `COMFY_CUDA_DEVICE` on purpose; a blank index beside a uuid inside the
  lease starts unchanged, with no index entry added.
- **Unchanged paths.** A host with card-scoped leases off, a `--whole-node` lease and a lease derived from the command's own pin
  never reach the re-pin: the command starts with the pin it had (`TestGPUReserveFlagOffWholeNodeAndDerivedLeasesAreByteForByte`),
  and for every input without a program pin outside the lease the confinement is the one the wrapper computed before
  (`TestConfineWrappedWithoutAnOutsideProgramPinIsTheOldAnswer`).

There is no override flag: nothing is refused, so there is nothing to override.

**The allocator.** A card is allocatable when it is not quarantined (`quarantine.<id>` sidecars, which P12 will write),
not the display card (the card whose `display_active` reads Enabled **or** whose `display_attached` reads Yes: with the screen asleep
`display_active` reads Disabled on every card of the 3-card box while `display_attached` still marks the card that drives the monitor,
measured 2026-10-03; the card table's rule, `gpuprobe.ScreenCardUUIDs`. The lease verdict and the fleet health attribute load by
`display_active` alone, `gpuprobe.DisplayCardUUIDs`, so an attached monitor never hides a holder's own work; and a reading taken
after a transient `nvidia-smi` failure carries no `display_attached` and marks its cards `display-unknown`: the allocator hands out none of
them until the next good reading), not claimed (a
whole-node lease claims every card), not under a foreign compute process
(`foreign-busy`, reported and skipped, never killed), its free VRAM fits `--vram` (GiB per card), and the **host** has the
host's memory admits what `--ram` and `--ram-commit` declare: the same rule the grant applies (`gpuprobe.HostRAMAdmitsLoad`: the
available RAM less the touched need and the not-yet-touched part of granted leases must leave `gpu_host_ram_headroom_gib`, default
8, and the commit charge + the commit need + the not-yet-touched part must stay under physical RAM less the headroom (a commit
nobody stated), or, for a commit stated with `--ram-commit` where the OS enforces a limit, with the not-yet-charged part under that
limit less the headroom; see "Host RAM" below), here as an advisory pre-filter. Among allocatable cards the order is: no resident
seat first, then the cheapest eviction (the footprint of the configured layer seats loaded on it), then the lowest id. An
unreadable host-memory reading refuses only when a RAM need was declared, and a need no state of the host admits ends the
request at once. On Windows (WDDM) nvidia-smi lists no per-process
rows for compute apps, so `foreign-busy` is Linux-only evidence today.

**The display card is allocatable up to a cap, at any presence (`gpu_display_cap_pct`, 0.183.0).** The operator's rule of
2026-10-11: the display card may be used up to 60 percent at all times, unless he says he needs it. The allocator used to read
`operator_presence` (never the card at the desk, the card above the layer's floor when away); it now reads no presence at all.
One key, `gpu_display_cap_pct` (a whole number of percent, default 60 when unset, `0` closes the card to the allocator, `100` is
the whole card, a value outside `0..100` refuses the load by name; unset and `0` are different values): the display card is
allocatable while the VRAM in use now, **the desktop's own included**, plus the request's declared footprint is at most that share
of its total VRAM. The arithmetic is the display layer's own (`free − footprint ≥ floor`) with the floor set to the rest of the
card, so the two doors onto the card cannot disagree about it; where the layers also declare an absolute `display_floor_gib`, the
larger of the two floors holds. On the reference box (the display card with 2.6 GiB in use of 15.9) a 6 GiB request can land
there and a 7.5 GiB one cannot, and the skip names the cap. The cap applies to the display card alone: a pair card is judged by
the footprint, as it always was.

- **`gpu_display_cap_pct: 0` is the operator's "I need the card" switch.** The card is skipped as `display` and the skip names the
  key, whatever the desk is doing. Nothing watches the desk any more: a locked screen, an idle desk and a game on the card are one
  question, answered by the VRAM in use at the moment of the pick.
- **A footprint is needed to be held to a share.** A job that gives no `--vram` is not given the display card under a cap below 100
  (the layer's guard refuses a seat with no display footprint the same way); a media call declares no VRAM footprint, so the typed
  media tools do not take the display card until a measured footprint is fed to the allocator. `--devices` names the card on the
  operator's word and is never second-guessed.
- **An unknown display stays closed.** A reading that could not say which card the monitor is on marks every card `display-unknown`
  and none of them is handed out, whatever the cap.
- **The cap bounds admission only.** Nothing limits a job after it is placed, so the footprint has to be a measured peak, and one
  number per request cannot describe a donor slice beside a compute share. A display card with no resident seat sorts first (nothing
  to evict), so with the pair seat loaded `gpu reserve --cards 1 --vram 6` now prefers the screen's card to an eviction;
  `gpu_display_cap_pct: 0` restores the old order.
- **A seat llama-swap pins onto the display card is not admitted by the cap at all.** The allocator places jobs; a llama-swap model
  that loads onto the card is outside it (a vLLM seat pinned onto the display card once put 13,281 MiB, 81 percent of a 16 GB card,
  there while the audit printed OK). What reports on the card afterwards is the display cap row of `gpu status` / `gpu cards` / the
  brief (a live reading, above), which needs no knowledge of how the seat is pinned, and, for the yaml that pins seats there,
  audit-yaml's `display-cap-unseen` note (next bullet).
- **`audit-yaml` names the entries it cannot judge against the cap (`display-cap-unseen`).** A llama-swap yaml states no VRAM
  footprint (a llama.cpp seat's weights, KV and compute buffers are nowhere in it, and a `proxy:` seat's cards are set in the
  engine's own launcher), so the audit cannot say an entry exceeds the cap, and it does not pretend to. On a box whose driver flags
  a display card it prints ONE `note:` for the file naming the entries that may sit on that card: `proxy:` seats, entries with no
  `CUDA_VISIBLE_DEVICES` pin, entries pinned onto it by UUID or by index under `CUDA_DEVICE_ORDER=PCI_BUS_ID`, and plain indices
  with no PCI order, named as unmappable and never guessed (at most six names per group). It names the display card (index and UUID
  tail of that reading), the default cap and its budget by the same arithmetic, says that nothing is judged and that entries which
  can run together (`matrix:`) are not summed, and points to the live row. It is never a violation and never an exit code; no
  config is read, so the cap it states is the default and says so; `--other-box` skips it. The display card is the driver's reading
  by UUID, never a remembered index, so a monitor moved to another card is followed on the next read. A single-card or headless
  box has no flagged display card and the note is silent, even on a reading that could not say which card the monitor is on (one
  card has nothing to tell apart); on two or more cards such a reading gets one line saying so and naming no entry. The table is
  read for a file that pins a device **or** has a `proxy:`
  seat (once per file, shared with the pin rules); a file with neither never runs nvidia-smi, so an unpinned entry alone is not
  named. **Deferred on purpose:** footprint-based violations (a declared layer footprint, a vLLM `gpu_memory_utilization` times the
  card, a seat-env reader) and a `doctor` row. There is no placement-neutral footprint source yet: declaring the opencode-only
  3-card seat as a layer would expose it to placement, `kv_cache_memory_bytes` voids the utilization figure, and a live yaml with
  no stamp would turn the session-start audit red. The live row is what catches such a seat today.
- A display twin that is loaded counts as a resident seat of the display card, each loaded twin costed at the layer's
  `display_footprint_gib` (both twins loaded cost twice that), so the card is not ranked as an empty one while it holds a twin:
  taking it would evict the twins, and the order prices that. When llama-swap's `/running` cannot be read the display layer's
  models are counted as loaded, so the display card sorts as occupied rather than as the empty card it might be; other layers'
  seats contribute nothing then, as before. A display card that could not clear the cap is not queued on while a lease holds it
  (waiting would not fix it). The llama-swap twin guards (`display_floor` and `presence`, `internal/placement/guards.go`) are
  unchanged: `operator_presence` still decides them and nothing else
  (`TestAllocatorOpensTheDisplayCardWithinTheCapWithoutPresence`, `TestAllocatorRefusesTheDisplayCardPastTheCapAndNamesIt`,
  `TestAllocatorCapZeroKeepsTheDisplayCardClosed`, `TestAllocatorUndeclaredFootprintStaysRefusedOnTheDisplayCard`,
  `TestAllocatorDisplayUnknownStaysClosedUnderTheCap`, `TestALoadedTwinOnTheDisplayCardIsAnEvictionNotAnEmptyCard`).

**A queued request is asked again at the grant.** The cards a `--cards` request or an auto-placed media call queues on
are chosen from the display cap and the display card's VRAM in use as they are at the enqueue, and a place in line
can be hours long. When the cards come free the grant puts the desktop rule again from fresh readings
(`gpulease.Options.GrantCheck`, `gpulease.DesktopRefusals`, built by `gpualloc.GrantCheck`): the display card past its cap
(it grew while the request waited), the display card once the operator has closed it (`gpu_display_cap_pct: 0`), and a card
that may be the display card on a reading that could not say. The check runs with the cards already claimed by the request, because a claim that is checked before it is made can
be skipped by a lease that is mid-release; a refused grant is released at once and the request chooses again from the same
fresh readings, so it takes another card it still fits, queues on what qualifies now, or keeps polling for the display card
to qualify until its `--wait` ends. The second choice keeps the arrival time of the first (`Options.QueuedSince`), so the
place in line is not lost to it. `--devices` is the operator's word and carries no check
(`TestDesktopRefusalsAppliesTheCapAtGrantTime`).

**A lease already running on the display card is not revoked when the operator returns or closes the card.** The post-admission
watcher ([composite-tier.md](composite-tier.md), ADR 0075) unloads the display layer's llama-swap twins and nothing else: it never
stops a process and never releases a lease. A `--cards` job or a media render that took the display card keeps it after the
operator comes back, and after `gpu_display_cap_pct` is set to 0. The cap was checked once, when the card was claimed
(`free - footprint >= floor`), so what protects the desktop afterwards is the footprint the job declared with `--vram`, the
window it declared with `--for` (the lease expires then), and the operator, who can end it with `gpu release`. A job that
grows past its declared footprint, or a game that takes the memory, can break the floor while the lease runs, and nothing
here will notice. Declare `--vram` honestly and keep `--for` short on the display card.

**A place in line for a caller that cannot stay (plan P13, invariant I4).** A media tool call waits its window
(`gpu_wait_ms`, 90 s) and must then answer. On a host that leases cards it answers with a **token** instead of
a refusal: a record of its place (the cards it wants, empty = the whole node, and the arrival time it joined
the line with) that the caller re-presents to resume that place; see "Per-card media admission" in
[media-generation.md](media-generation.md). A token has no process behind it (the MCP server that wrote it is
alive for as long as the client's session is, which says nothing about whether the client is coming back), so
its life is its **last poll**: for `TokenGrace` (30 s) after the poller left it holds its place, and every
later waiter on the same cards queues behind it; after that it is **absent**, skipped by every waiter and
ignored by a whole-node barrier, so a client that wandered off never blocks the line; for `TokenTTL`
(10 min) it can still be resumed, with its original arrival time (the resumed waiter carries it, so it is
ahead of everyone who arrived after it), and then it is pruned. `gpulease.LeaveToken`, `ResumeToken`,
`DropToken`, `Tokens` and `QueuePosition` are the API; `Options.ResumeToken` and `QueuedSince` make a
waiter carry a place. The readers of the line (`ResumeToken`, `Tokens`, `Waiters`) prune a record they find dead,
expired or stale as they read; `Manager.ReadOnly` returns a view of the same root whose readers skip such a record
and leave it on disk, for a question that must not change what it asks about (the media lane probe). Tokens are files in `<state>/gpu/tokens`, never among the waiters in
`<state>/gpu/waiters`: a binary that predates them prunes every waiter record whose process has stopped
refreshing it, and would delete a token it cannot refresh. It does not honour tokens either, so on a host that
mixes versions an older binary can take a card ahead of a token holder; that costs the holder its place and
never exclusivity (the O_EXCL claim is still the only arbiter of who holds a card). A token id arrives from a
tool caller and is checked against `tk-[a-z0-9]{8,32}` before it becomes a path. A queued `Acquire` whose
whole window passed without reaching the front returns `ErrStillQueued`, which names who is ahead (a waiter
that has not claimed, or a place held for another caller); callers that answer with a token treat it like
`ErrHeld`.

**A ticket keeps a place for as long as it waits (ADR 0083).** The media ticket queue
([media-generation.md](media-generation.md#the-ticket-queue-a-busy-image-call-waits-for-the-first-matching-lane-that-frees-anywhere-adr-0083))
waits for a lane anywhere in the cluster, far longer than a window, and must stay senior to every caller that arrives after it. It
leaves a token at creation, with the ticket's arrival time, on the cards the real admission would queue the call on; the owner's
place keeper touches it (`TouchToken`, every scan interval, about 10 s against the 30 s grace; a keeper of its own and not the scan, which visits its tickets one after another and could let the places behind it lapse); a place that lapsed or was spent is left again with the
same arrival time (`LeaveToken`); the ticket drops it when it ends; and the final local run resumes it as a `waiter_token` like any
resumed call. `core.PlaceKeeper` is the seam (the pipeline implements it, `internal/pipeline/medialane_place.go`); it wraps
these calls and adds no admission logic. The lane question for a call that resumes a place counts only the callers that arrived
before that place on every plan (`gpualloc.QueuedClaimsBefore`). The scan also marks the place when its lane reading found only
host RAM in the way (`core.PlaceMarker`), so a place that waits on memory passes a request that declares none, as a waiter on
memory does (the G6 rule in "Host RAM").

**A kept ComfyUI instance is stopped by its lease's holder, unless it ends itself (ADR 0097, 2026-10-24).** A runner that
keeps the ComfyUI it launched leaves a detached instance running after it exits. An explicit `--keep-comfy` always keeps it, so
the items of a batch under one lease load their models once. Since 2026-10-24 every harness runner also keeps it after a GOOD
job by default, with a process bound that ends it after `COMFY_IDLE_EXIT_SEC` (300 s unless set) with no prompt and no holder
(below); `COMFY_IDLE_EXIT_SEC=0` restores the old kill after every job. The launch marker (`.offload-launch-<key>.json`) records
the lease epoch, and `idleExitSec` when the instance carries the process bound.
The **holder** of that lease stops the instance when it lets go, before the release and before any seat
warm-back (both want the VRAM): `gpu reserve` when its wrapped command ends, the detached holder when it
exits, `gpu release` when an operator ends a lease from outside, and the pipeline when a media lease is
released. The pipeline's routine release is the exception: it uses `comfyinst.StopForRelease`, which spares an instance whose
marker records `idleExitSec` (the outcome says `Spared`), because that instance ends itself and the next job on those cards reuses
it. It still stops every other instance of the epoch. A survivor holds about 240 MiB of the card and, on the 16 GB reference box
(keyed single-card instance, ComfyUI 0.38.0, measured on 2026-10-14), about 5.0 to 5.2 GiB of host private bytes, for at most
the bound. Under an outer `gpu reserve`, the wrapper's job object still ends an instance launched inside it when its command ends. `gpu release` settles WHICH lease it is ending first, by the release's own rule (`Manager.ReleaseTarget`:
the epoch named, else the only lease held), so a refusal (several card leases held and no `--epoch`) ends the
command before the instances are stopped or the seat is warmed; if the release itself then fails, the error says
the instances were already stopped. `internal/comfyinst` does it, and only for a keyed marker that names exactly that epoch: the pid
must be alive, must not have begun after the marker was written (a recycled pid), and the endpoint on the
marker's port must report exactly the recorded argv (`GET /system_stats`); then `POST /free` and a stop
(Windows: terminate; elsewhere SIGTERM, then SIGKILL after five seconds). Anything short of that proof is left
running and printed with the reason, never killed: a foreign process on the port is not ours. A marker with no
lease epoch (the default instance, an instance launched outside a lease) is never touched. A holder stops
instances only while its lease is still its own (`Lease.Check`): one that was released from outside or reclaimed
after a suspend is a straggler, leaves them running and says which (the same rule `Lease.Release` follows for the
claim: a fenced-out holder leaks rather than destroys), and `comfyinst` re-reads the marker at the moment of the
stop and leaves an instance whose marker changed hands during its proof. The proof is asked for three times of eight
seconds (it was once of three): an instance in the middle of a long prompt answers late, and a holder that read one slow
answer as "not shown to be ours" left the instance running with its models; if it still does not answer, the report says
how many attempts were made and that the next runner on that card frees the instance before its first job. A lease that ends because its holder died
(no release runs) leaves its instance (a kept instance is detached, so it survives the holder). The next lease on
that card REUSES it, it does not launch over it: a live keyed instance whose marker proves it is ours is reused, and
reusing an instance that was a lease's takes it over (`restampLaunchOwner`: only the marker's `leaseEpoch` changes,
to the reusing lease's), so that lease's release stops it. An instance that was never a lease's (a marker with no
epoch) is not claimed by a lease that happens to reuse it. An instance nobody reuses ends itself when it carries the process
bound (below). One without it (an explicit `--keep-comfy` with `COMFY_IDLE_EXIT_SEC=0`, a hand-started instance) stays up until
an operator stops it: the harness runs no timer or watcher over it (plan I5), and whether the next acquirer or `gpu doctor`
should sweep markers whose epoch is dead is an operator decision (I5 forbids a watcher, not an explicit sweep). What the
harness does carry is the engine's own idle clock, the llama-swap `ttl` analogue: a kept or warm instance is launched with the
idle pack (`render/comfyui-ext/idle-nodes`), which frees its models and executor cache after 300 s with an empty queue (run in
a live ComfyUI on 2026-10-14). The harness's own launch already has ComfyUI unload every model when a prompt ends
(`--disable-smart-memory`), so what that clock bounds is what survives that: a warm session's executor cache in host RAM, and a
hand-started instance's models when it runs with smart memory. The process bound (ADR 0097) is a second clock in the same pack: a
launch that carries `OFFLOAD_IDLE_EXIT_SEC` exits its own process (`os._exit(0)`, after a 2 s grace and a re-check under the
queue mutex) once no prompt is queued or running and no runner holds it (`GET /offload/hold`, a stream the runner keeps open for
its whole job), so the Python process and its CUDA context end too. Both clocks act on their own ComfyUI only. See
[media generation](media-generation.md). A kept launch
whose marker cannot be written logs `COMFY-KEEP-WARN`, because nothing can then stop it by its lease.

**Allocate and claim are one loop.** The allocator reads live state over a window of seconds, so another reserve can take
the card it picked before this one claims it (two simultaneous `--cards 1` over free cards both choose the lowest id). So the
claim is a non-blocking acquire: when it loses, the winner's claim is visible on the next read and the allocator runs again,
and the request queues only when the allocator itself says no qualifying set is free. N simultaneous `--cards 1` reserves
over N free cards therefore land on N distinct cards (`TestGPUReserveConcurrentCardRequestsLandOnDistinctCards`), and the
detached holder (`gpu hold --cards`) runs the same loop in its own process, so the lease is taken by the pid the parent
reports. The loop re-allocates at most 16 times and then queues on the set it last picked.

**A busy card is a place in line.** `--cards N` that cannot be met right now does not refuse while `--wait` lasts: it queues
FIFO on a fixed set of N cards, the cards that are **free right now first**, topped up from the best cards a live lease
claims (so an idle card is held as part of the set, never thrown away while the request waits for busy ones, and the
request has a place in line from the start); the set is registered like any named reservation. It polls every two seconds
and re-runs the allocator only when too few cards qualify at all for a reason waiting does not fix (display, quarantine,
foreign process, host RAM, VRAM). Elastic re-picking while queued, and a worker that takes cards as they free, are the
fan-out work (P13/P14), not this change: the queued set is all-or-nothing. At the deadline, or with `--wait 0`, the error
lists every card's reason and names `--wait`.

**`gpu doctor`: the reader audit.** `local-offload gpu doctor [--scan dir]... [--depth N] [--write-audit] [--json]` finds the
readers of this host's lease directory that it can reach and says which ones understand the format. How each is reached:

| reached | what it finds |
|---|---|
| by **name** | `local-offload*`, `offload-harness*` and `local-agent*` (the agent binary reads the lease through the same package before it loads a seat and is deployed beside the harness): the running binary, the install directory with the node-swap backups `<exe>.bak-<suffix>` a rollback restores, every `PATH` entry, and every scan root |
| by **content** | under a scan root (`--scan`, which is how a media repository's own copy is covered), any other executable of 1 MiB or more that carries the import path of a package that reads the lease: a wrapper copy under a name of its own is found and judged like any other |
| by **image** | every running harness process (the agent binary included) and every lease holder or waiter, whatever its image is called |
| Node | every `gpu-lock.mjs` under a scan root |

The build **version cannot tell**: it is the same string before and after the format change inside one release, so each
file is judged by the literal `gpu-lease-format-2/per-epoch-fence` (`gpulease.FormatSignature`), which `render/gpu-lock.mjs`
exports as `LEASE_FORMAT_SIGNATURE` and which **every binary built from this tree that links the lease package carries**: the
package pins it with an init function, so the linker keeps it whether or not the audit itself is part of that binary
(`TestEveryBinaryThatLinksTheLeaseReaderCarriesTheSignature` builds each such binary, the agent among them, and audits it). A
binary built before the format has no such literal. The build marker `offload-build-version=<version>` is shown beside each
binary that has it. **Nothing found is executed.** **Fail closed:** finding no binary, a root, entry or file that cannot be
read or examined, or a process table that cannot be listed is a finding, not a pass. The exit status is non-zero when the
audit is not green. `--write-audit` records the verdict as `gpu/reader-audit.json`, **red as well as green**, so a stale green
cannot outlive a newer old binary.

**Running processes older than the installed build (D-36).** A rename-swap deploy replaces the installed binary while every
process already running keeps the image it started from. For each running binary the audit reached, `gpu doctor` compares
the version its build marker states with the installed build's, read from the **installed file** (the doctor's own
executable path, through the audit's own marker reader `gpulease.ReadBuildMarkerVersion`, so one file cannot read two
versions). The doctor's own file is the reference only when it is the harness binary PATH resolves (or PATH names none): run
from a worktree or dev build beside a deployed copy on PATH, it compares nothing and a note says why, since a build that is
not installed would send the operator to restart deployed processes for nothing; an unresolved executable path is named as
the cause when that is what stopped the comparison. The older ones carry `older_than_installed` in `--json` and an "older than the installed build" line in the text,
followed by a summary with their pids. It is information and not a finding: the exit status and the reader-audit marker do
not move. The installed file is read, not the doctor's compiled-in version, because the doctor may itself be a pre-swap
process. A binary with no build marker, or a version that is not `major.minor.patch`, is never called older (unknown stays
unknown), and when the installed version cannot be read a note says no comparison was made. The comparison is by version:
two builds of one version are not told apart here (the MCP server's `process` block compares bytes,
[mcp-server.md](mcp-server.md)). The audit opens every image it reads through `gpulease.OpenShared`, which on Windows grants
`FILE_SHARE_DELETE`, so reading a backup or the installed file never makes a rollback or a second swap that renames it fail on a
sharing violation.

**What green does and does not mean.** Green means every reader the audit *reached* is aware. The output lists what it
scanned **and what it did not enter** (directories past the depth cap, 3 levels below a scan root unless `--depth` says
otherwise, and `node_modules` and `.git`), and a green with gaps says so on the verdict line. By design it cannot reach: a
renamed copy that is neither under a scan root nor running as a lease holder, a Node reader that is not called
`gpu-lock.mjs`, a binary a packer has compressed, and a `PATH` entry under a name that is not a harness name (`PATH` is read
by name, not walked). **Plan P6 (enabling the writer on a host) is therefore not unblocked by a green doctor alone**: it needs
the green to include the deployed `local-agent` binary and every media repository's own wrapper copy, which means running
`gpu doctor --scan <repository>` for each repository on the host and reading the "not searched" list.

### Consumers read a seat's cards, not the node (plan P4)

*Register C-86, question 1 of the operator order: a card-2 job stops fencing text on cards 0 and 1. The Go side only; drain and
unload are scoped in the next section, and the fleet's own health is P7 (since shipped: see "The fleet reads leases per card"). Until a host turns card-scoped leases on (plan P6)
the only lease on it is a whole-node one, which fences exactly what it always fenced, with one exception below: a legacy
lease (one an **older binary** wrote) on a host that has turned the inference on (`gpu_legacy_scope_inference`, off by
default) and whose cards the evidence rule can name.*

**One question, asked in one place.** Every consumer that gates a seat used to ask "is a lease held". It now asks "does a
held lease sit on a card **this seat** is pinned to". The seat's pin is the layer seat's `device` (the pin it is launched with:
nvidia-smi indices under `CUDA_DEVICE_ORDER=PCI_BUS_ID`, or a GPU UUID prefix), read by `config.ModelPins(model)`: the pins
of every seat that serves the model (the union when it is declared twice), a router seat's `model_map` twins, and, for the
router seat that declares no model, the cascade's rung models. A model no layer declares is placed by its entry in the serving
config, the reading the unload list makes ("Placement from the serving config" below; until C-86a a box without `layers` placed
nothing and every lease fenced every seat). `config.Load` arms it into the load gate beside the lease
directory (`modelaffinity.SetSeatPins`), with `gpu_comfy_order`, `comfy_dir` and the memory stack. The arithmetic is three
methods on `gpulease.Info`: `EffectiveDevices` (the declared cards, else the inferred ones, else none, which is the whole
node), `Touches(ids)` and `For(ids)`, which narrows an inspection to the leases that touch the cards and leaves every
existing predicate (`Held`, `blocksLoad`, `BlocksNewRun`, `Reserved`, `Fenced`, `ForeignFence`) unchanged and unaware.
`gpulease.ResolvePins` turns a seat's pins into lease ids through the card table.

**The direction of every doubt is "fence".** A model nothing places (an alias a layer does not spell and the serving config does
not list, a `proxy:` seat, an entry that pins no device in env), a pin the card table cannot place (an index no card has, an ambiguous prefix, a card named twice), a
card table that cannot be read, a whole-node lease, and a long-context contract (it runs on a long seat that the agent chain
does not describe) all read as every card, which is the answer the gate gave before card-scoped leases existed. Narrowing
happens only on a positive resolution. The card table is an nvidia-smi exec, so it is read only when a held lease actually
names cards, and memoised for 2 s; an idle box and a whole-node lease never read it
(`TestCardTableNotReadWhenNoLeaseIsHeld`, `TestWholeNodeLeaseStillFencesEverySeat`).

**Where it applies.**

| consumer | what changed |
|---|---|
| the load gate (`modelaffinity`: `Admit`, `AwaitRunSlot`, `AwaitUpstream`, `AwaitModelRoute`) | waits only for a lease on the seat's cards (`TestSeatOnFreeCardLoadsUnderMediaLeaseOnOtherCard`, `TestSeatWithUnknownPinStillBlocked`, `TestTripleSeatFencedByCard2Lease`) |
| the delegation door, the `agent_run` door and the review lane's local loop | the fence pre-check reads the same narrowed lease the cordon does, so it still predicts what the cordon will do |
| the delegator (`delegate`) | `LeaseForContract` narrows the local lease to the seats a contract could run on; the busy formula in the auto deal, the capacity wait, the retry fence and the spread deal read it per contract, and the local box is busy for a contract only when **every** local agent seat it could use sits on a held card (`TestLocalBusyFalseWhenAFreeCardServesTheSeat`). `LocalLeaseFor`, `LocalBusyFor`, `ReservedFor`, `FencedFor` and `ForeignFenceFor` are the same questions for a caller that knows its seat; `Reserved`, `Fenced` and `ForeignFence` keep their signatures and their whole-node reading |
| the text and vision auto routes (`textremote`, `visionremote`) | the "local GPU is busy" trigger reads the cards of the workhorse and vision seats |
| the stt auto route (`sttremote`, 0.165.0) | the "local whisper would be held" trigger is `modelaffinity.WouldBlockUpstream`, the upstream fence's own first inspection without its wait: a lease that fences the whisper model's cards (the same narrowing as above) AND the model not already resident; a resident whisper is served here at once (`TestWouldBlockUpstreamAgreesWithTheFence`, `TestDefaultLocalBusyIsTheWhisperFenceNotTheLease`) |
| the pipeline's vision pre-check (`gpulock.WaitFreeScoped`) | waits, and defers `gpu_busy`, only for a lease on the vision seat's cards, the same reading the auto route made when it chose to run locally (`TestVisionGateIgnoresALeaseOnAnotherCard`, `TestVisionGateStillWaitsForALeaseOnItsOwnCard`); before this it waited `vision_gpu_wait_sec` on **any** live lease and deferred a call the delegator had just kept local |
| the placement table | `Live.CardsHeld` (the local snapshot arms it from the same lease directory and card table) and agent row 5c, below |

**The agent lane falls back to a card that is free.** When the home layer's agent seat fits the contract and a lease this
process does not hold sits on its cards, the table takes the next declared layer, in declared order, that is not opt-in and not
dormant, whose agent seat fits the contract's window and whose cards are free. On the three-card tier the home layer is the
pair (cards 0 and 2; it was the three-card layer, which spans every card, from 0.132.6 until 2026-10-04), so a card-2 render
moves the lane to the single layer's agent seat on card 0
(`TestAgentHomeFallsBackToNonIntersectingLayer`). With none free the contract **keeps the home seat and queues at its gate**,
never a defer, and the reason says no local layer has free cards so the delegator may route it to another node
(`TestNoFallbackQueuesAndRoutesRemote`; the delegator's own routing to the free-card node is plan P1, which is not part of this
change). A contract that names its layer runs there or waits there; a remote row's `Live` carries no reader and so no fallback.
`placement.AgentChain` is the one list the table walks and the delegator reads, so the two cannot disagree about which seats
exist; the delegator's reading ignores nothing but the window of a seat that declares none.

**What a legacy lease can still do: the evidence rule.** A lease written by a binary that predates card-scoped leases is a
whole-node record, and the old wrapper cannot be taught anything, so a render that holds one card of the box for hours fences
the other two for as long as it lives. `gpulease.Scoper` (`infer.go`) scopes such a lease to the cards its process tree
demonstrably uses, on strong evidence only, and otherwise leaves it whole-node and says exactly why. It never rewrites the
record: `Info.Devices` stays what the record declares (the card table, the allocator and every acquire still treat it as the
whole node), and the inferred set lives beside it (`Info.Inferred`, `Scope`, `ScopeWhy`, `ScopeWidened`).

**Which records, and when.** Only a record an **older binary** wrote, which could not have named cards, so its silence is not a
statement. Every record this binary writes carries `Meta.Format` (`gpulease.RecordFormat`, read back as `Info.Legacy`), and a
whole-node record that carries it is the whole node by its writer's word: an explicit `gpu reserve --whole-node`, a reserve on
a host with the writer flag off (byte for byte as before), and the pipeline's own media lease are never narrowed by a command
line (`TestExplicitWholeNodeLeaseIsNeverInferred`, `TestFlagOffWholeNodeFromThisBinaryIsNotInferred`). The rule is also **off
unless the host sets `gpu_legacy_scope_inference`** (default false): the read-only capture of what a real legacy tree shows is
still owed (plan P4 step 1, milestone P6), and nothing narrows a lease on a guess before it. **Transitional case:** a
whole-node record written by a build that has record v2 but predates the `format` stamp (any build before this change; the
pipeline's media lease set neither the stamp nor a wrapper version) also reads as legacy, so turn the switch on only once no
such build still holds a lease on the host (plan P6 replaces the binary copies first). Off, `gpu status` says so and
names the key (`TestLegacyInferenceIsOffUntilTheHostTurnsItOn`, `TestGPUStatusSaysAnOlderBinarysLeaseIsNotInferredWhileTheSwitchIsOff`).

1. **A command line.** The lease's recorded wrapped command and the command line of every process in the wrapper's tree
   (`gpulease.ProcessTree`: a Toolhelp snapshot and the PEB on Windows, `/proc` on Linux, parent links believed only when the
   child began no earlier than its parent, so a recycled parent id cannot adopt a stranger), read for ComfyUI's
   `--cuda-device N`. The environment of a foreign process is not readable, so `COMFY_CUDA_DEVICE` and `CUDA_VISIBLE_DEVICES`
   are not evidence here. N is in ComfyUI's FASTEST_FIRST order, which nvidia-smi does not report: it resolves only on a box
   that declared `gpu_comfy_order`, and an index that cannot be placed is named in the reason, never guessed onto a card. **One
   process naming a card that cannot be placed spoils the whole reading**: the lease stays whole-node even when another process
   names a card that does resolve, and no other evidence may narrow it either, because the card that process uses is unknown
   (`TestPartialCommandLineEvidenceStaysWholeNode`).
2. **The ComfyUI launch marker, only when it is tied to the lease:** its pid or the process that launched it is in the
   wrapper's tree, or it started at or after the lease did and no other live lease claims the card it names. A leftover marker
   proves nothing (`TestMarkerNotTiedToLeaseIsNotEvidence`).
3. **The cards the tree holds memory on, sampled twice at least five minutes apart, on a lease at least ten minutes old.**
   Under WDDM nvidia-smi often names no compute process at all, and an empty answer is no evidence, never "the tree uses no
   card".

If none holds the lease stays whole-node (`TestLegacyLeaseWithoutEvidenceStaysWholeNode`). Rules wrap it. **Sticky,
never shrinking, across processes:** once a card has been inferred it stays in the set, carried by the sidecar
`lease/seen.<epoch>`. Every process that reads the lease shares it, so a writer **reads the sidecar under the epoch lock,
unions what it learned with what is there and writes the union**, never the snapshot it took before it gathered its evidence;
a slow reader cannot shrink a wider set, and the ledger line is written once, by the process that changed the set on disk,
before the sidecar is renamed into place (so an interrupted write repeats a line on the next reading instead of losing it:
`TestInterleavedScopersNeverShrinkTheStickySet`). It is written only when a scope is established or widened or a sample is
taken, and only for a lease that is still live. It is removed when this binary releases the lease; an older binary's release
does not know it, so the **next acquirer** (a whole-node or a device grant) sweeps any `seen.<epoch>` whose lease is gone
(`TestNextAcquirerSweepsASidecarThatAnOlderBinaryLeftBehind`, `TestDeviceGrantSweepsASidecarWhoseLeaseIsGone`). The set may
**widen**, and each establishment and widening appends one line to the scope ledger `gpu/scope-ledger.jsonl`
(`TestInferenceIsStickyAndWidensWithLedgerLine`); a sidecar or ledger that cannot be written is **said** (a stderr warning, and
`not sticky across processes` in the reason), not folded away (`TestUnwritableSidecarIsReportedNotSilent`). **The wrapper is
not trusted blindly:** a record that names no holder pid is never walked (pid 0 is the system process, and every command line
on the box would become evidence: `TestRecordWithoutAHolderPidIsNeverWalked`), and a root that started after the lease was
taken is a recycled pid whose tree is not read (`TestRecycledWrapperPidIsNotBelieved`); an unreadable process table is named in
the reason (`TestUnreadableProcessTableIsNamedInTheReason`). **The display card is never reported free by
an inference while the operator may be at the desk** (the presence reading, unknown = present): its silence is the desktop's
noise, not the job's absence, and the reason says so (`TestDisplayCardNeverInferredFreeWhilePresent`). **Throttled:** the
evidence is gathered at most once a minute per lease per process (concurrent first readers share one gathering,
`TestConcurrentFirstReadsGatherTheEvidenceOnce`), and a lease that declares its devices, or no lease at all,
costs nothing. `gpu status` shows it (`seats: fenced on cards ... (inferred [scope-widened])`, or `seats: the whole node (stays
whole-node: ...)` with the missing evidence; `--json` and the per-lease rows carry `seat_scope`, `inferred_devices`,
`scope_widened` and `scope_why`).

**Which reads write, and which can unload.** Two kinds of reader, and only one of them writes. The **load gate's own reader**
(`modelaffinity.InspectLease`: `Admit`, `AwaitRunSlot`, `AwaitUpstream`, `AwaitModelRoute`, the `agent_run` and delegation-door
pre-checks, the seat yield) reads the lease because a process wants a card, and it remembers what it sees: the sticky sidecar
and one ledger line per establishment or widening (`TestTheLoadGateRemembersTheScopeAndUnloadsNothing`). The **inspectors**
(`gpu status`, `gpu cards`, `offload_status`, the activity snapshot, `CardsHeld`, and the delegator's own reads through
`delegate.LocalLease`) report the same scope from the same evidence and write **nothing**: no sidecar, no ledger line, no call
to llama-swap (`TestStatusReadsNeverWriteOrUnload`, `TestLocalLeaseIsAnInspectionAndWritesNothing`), so a read-only check of a
live lease stays read-only. **No read of the lease, by either kind, unloads a model.**

**The seat race rule: the seat yields, the long job never does.** The lease claims its cards first and then unloads the seats
resident on them, so a text load that passed the gate a moment before the claim finishes loading **after** that unload, onto
cards the lease now holds. The check-then-act window cannot be closed at the gate (the load is llama-swap's, one request
later), so it is closed from the seat's side: once the model is resident the seat re-reads the lease and, if one this process
does not hold now fences loads on its cards, **it unloads itself** (`modelaffinity.YieldIfFenced`). Only a lease that names
cards (declared, or inferred for a legacy lease) moves a seat: a whole-node lease fences what it always fenced and unloads no
seat, so a host with card-scoped leases off behaves as before (`TestAWholeNodeLeaseNeverYieldsASeat`). It runs at the moment the
batch that may have loaded the model drains (`Ticket.Release`, never for a request that joined the resident batch) and after
the delegation door's cold-load warm-up, where the contract then defers as a re-placeable capacity defer
(`TestSeatLoadRacingDeviceGrantSeatYields`, `TestWarmUpRacingADeviceLeaseYieldsTheSeat`). The same rule covers a legacy lease
whose inferred scope later widens onto a card a seat was admitted on, and **only through the seat's own release**: the next
reading of the lease carries the wider scope, the seat re-reads it when its request completes, finds the lease on its card and
unloads itself (`TestInferredScopeWideningEvictsSeatNotLongJob`). Nothing sweeps: establishing or widening a scope unloads
nothing, so a seat that is resident and idle on a card the scope has just spread onto keeps it until its own idle ttl (five
minutes) or its next request. What never yields: the memory stack (mem0 never yields to a lease), a model with requests in
flight (the engine's own gauge; the next release picks it up: `TestWideningNeverPullsABusySeat`), a process running under the
lease, and a model on a card the lease does not touch. A seat whose state cannot be read on a held card is **said** (one
line per seat per five minutes) and left resident (`TestYieldIfFencedSaysWhenTheSeatCannotBeChecked`,
`TestReleaseLogsASeatThatCouldNotBeCheckedOnAHeldCard`). **Scope of the rule, narrower than the plan's wording:** only an
`Admit`-gated batch's release and the delegation door's warm-up re-check the lease after the model is resident. A load that
goes through `AwaitUpstream` or `AwaitModelRoute` (speech, embeddings, the chat lane, tokenize, props, KV slots) passes the
fence and then has no post-load check, and a batch that never drains (steady joiners) never reaches its release; see Known
gaps. Nothing here signals or stops a process.

**What this makes of the three-card tier, measured.** The shipped table declares eight seats (`TestSeatsThatStayPlaceableUnderACardLease`
computes the counts through the code above; it declared nine while the three-card seat was a `triple` layer, 0.132.6 to
2026-10-04). While a lease holds **card 2**, three stay placeable: the single layer's router and agent on card 0, and the
dormant display layer's twin on the display card. The pair's three seats and the single layer's OCR and speech seats (card 2)
are fenced, which is correct. While a lease holds **card 0** three stay placeable (the card-2 single seats and the display
twin); while it holds the **display card** seven do. That closes the C-86 estimate of how much of the box one card-2 job
takes: it used to take every seat, and now it takes five of the eight.

**Not done here.** (1) The live capture on the three-card box. Plan P4 step 1 is a read-only capture of what the sampled
processes and the wrapped command line show for the lease that was running when the plan was written; that lease has ended, so
the rule above is the plan's, built on synthetic fixtures, and the capture of a real legacy tree is deferred to the milestone
that enables card-scoped leases on the host (plan P6); until then `gpu_legacy_scope_inference` stays off. The process-tree
reader was run read-only against a child process the test starts (the Windows path); the Linux path is built and vetted for it
and not run in this session. (2) The cascade-lane and repack busy gates (`llamaclient.WithRemoteLanes`) still read any live
lease as busy: they only choose a remote lane, never a fence. (3) Fleet health publishes every live lease with its cards since plan P7 (see "The fleet reads leases per card"). (4) A presence reader is not armed in the load
gate, so an inferred scope always keeps the display card.

### Drain, unload and the render lane clear the leased cards, not the node (plan P5)

*Register C-86, question 1 of the operator order, the other half: draining one card does not empty the others. A whole-node
lease reads exactly as before.*

**`--drain` and `--unload-seat` take the cards the lease holds.** `gpu reserve --devices|--cards ... --drain --unload-seat`
(and the detached holder's `maintain` step) pass the lease's card ids to `maintainSeatScoped`. A seat is **on the leased
cards** when it is placed on them: by `layers`, else by its pin in the serving config (`LeaseScope.Locate`, resolved
through the card table); a seat nothing places, whose pin the card table cannot resolve, or that is read while the card table
cannot be, is on them (today's behaviour, the direction of every doubt). Then:

- The configured agent seat is drained and unloaded unless it is positively placed on another card (by `layers`, else by its
  serving-config pin). A seat on card 0 under a card-2 lease is neither drained nor unloaded, no warm-back is owed for it, and
  the wrapper says so on stderr (`TestUnloadSeatLeavesAnAgentSeatOnAnotherCard`). A seat nothing places is on the leased cards.
- The other resident models are judged by `LeaseScope.Judge`, from where they are placed (`LeaseScope.Locate`): by `layers`
  first, else by the device pin of the model's entry in the serving config (`serving_config_path`; see "Placement from the
  serving config" below). A model placed on a leased card is unloaded; **a model placed on another card stays** (F15); a model
  whose placement is unknown (nobody declared it, no usable pin in the serving config, a pin the card table cannot place, a
  card table that could not be read) is unloaded, exactly as before F15, because the render needs its card. The memory stack is
  never unloaded. The wrapper names each group with its reason, one line per reason (`gpu reserve: left resident, placed
  elsewhere by the serving config pin: ...`, `gpu reserve: unloaded, placement unknown (...): ...`;
  `TestUnloadSeatOnlyUnloadsIntersectingModels`, `TestUnloadSeatPlacesOtherResidentsByTheServingConfigPin`,
  `TestUnloadSeatTreatsAnUnplaceablePinAsOnTheLeasedCards`). The drain and the fence read a doubt the same way (`LeaseScope.Touches`:
  a doubt waits).
- The legacy `GET /unload` is total (it ignores `?model=`), so it is refused while any model that has to stay is resident,
  the seats left on other cards as well as the memory stack.
- **The drain waits only for runs on the leased cards.** The run registry records each run's seat pins (`Run.Devices`, set by
  the delegation door and the `agent_run` door); a scoped drain waits for registered runs whose pins intersect the lease, by
  the seat they run on when a record predates the field, and skips the agent seat's own gauge when that seat is on other cards
  (`TestDrainWaitsOnlyForIntersectingRuns`). A run on card 0 does not hold a card-2 lease.

**The render lane unloads the list the wrapper hands it.** For a card lease the wrapper (and the pipeline's own per-card media
admission) exports `GPU_LEASE_UNLOAD_MODELS` to the command it runs: every model of the llama-swap roster except the memory stack
and the models positively placed on another card (`gpualloc.UnloadModels`, the verdicts above). A model nothing places is on the
list. The reasons are said on stderr or in the log, one line each. `render/gpu-lock.mjs` `freeLlamaSwap` unloads exactly that list
(drained first, as always) instead of every model off the memory stack, `-` means nothing may leave, the memory stack never leaves
whatever the list names, and the total-unload fallback is refused while a model outside the list is resident
(`render/gpu-lock.test.mjs`, "freeLlamaSwap honours the supplied list"). It logs the models it left (`left resident (not on this
lease's unload list: placed on other cards)`).

A lease on some cards gets a list (F15) when the llama-swap roster was read: with no llama-swap endpoint the list is `-`; with
no card table every model is unknown and goes, as before; a roster that was read and is empty means nothing is resident (the
candidates are then the models `layers` declare and the serving config lists). A roster that could NOT be read is not an empty
roster: nothing says what is resident, and a list built from the config alone is never complete (with `layers` only, a resident
model neither they nor the serving config name would never be judged and would stay on the leased card; with neither the list
would be empty and nothing would be unloaded). `gpualloc.UnloadModels` takes the roster read's error and makes NO list for it,
whatever the config declares, so the variable stays unset and the lane keeps its own rule, every model off the memory stack, exactly
as before F15 (the seats placed on other cards go with the rest: the price of not knowing what is resident). The wrapper and the
media admission say so (`the llama-swap roster could not be read ... the render lane keeps its own rule`;
`TestWrapperUnloadEnvWithAnUnreadableRosterLeavesTheLaneOnItsOwnRule`, `TestAnUnreadableRosterLeavesTheRenderLaneOnItsOwnRule`,
`TestUnloadModelsWithAnUnreadableRosterMakesNoListAndSaysWhy`). The card lease that has no endpoint still gets `-`
(`TestWrapperUnloadEnvForACardLeaseIsNeverUnset`). A whole-node lease has no list either: it holds every card, so the lane keeps
its own rule. A lease that names every card the card table lists is the whole node whatever it is called (a
one-card box's only card, all three of three), so its list is every model off the memory stack (`LeaseScope` `holdsAll`). A card
lease that nevertheless arrives at the render side with no list (a lease whose environment names cards, `GPU_LEASE_DEVICES`, else
`devices` in the epoch record, `leaseDevices`; an older wrapper, a hand-built environment) has no placement for any model, so it
unloads every model off the memory stack, as before, and says that it had no list (`render/gpu-lock.test.mjs`, "a card lease with
NO list").

**Placement from the serving config (F15).** A box that declares no `layers` has no declared card for any seat (the installer's
tier seeds do not write `layers`), so `layers` alone placed nothing there. `serving_config_path` is the llama-swap yaml the box
runs, and nearly every llama.cpp model in it pins its cards with an env entry, `CUDA_VISIBLE_DEVICES=<uuid>[,<uuid>]` (GPU UUIDs,
sometimes indices). `internal/servingcfg` reads that pin for each model id and alias, and `LeaseScope.Locate` matches it against
the card table when `layers` do not declare the model (a model both declare keeps the `layers` reading). Only the env pin counts;
device flags in `cmd` are never read. Unknown, with the reason: an entry whose env names no device (an unpinned dual-card
`-sm layer` / `--tensor-split` entry), a `proxy:` seat (a vLLM engine: its cards are pinned in its own unit, so **a `proxy:` seat
with no `layers` entry is unloaded under any card lease**), a pin that is a macro, an alias two entries claim with different pins, a
bare index when the entry does not also set `CUDA_DEVICE_ORDER=PCI_BUS_ID` (without it a CUDA index is not an nvidia-smi index),
and every model when the file cannot be read or `serving_config_path` is unset (said once). The unload list reads the file per lease,
only when a model needs it (`TestServingPinsReadsOnlyTheEnvPin` in `internal/servingcfg`,
`TestUnloadModelsPlacesWhatLayersDoNotDeclareFromTheServingConfig`, `TestUnloadModelsSaysOnceWhenTheServingConfigCannotBeRead`).

**The load fence places a model the same way (C-86a).** `config.ModelPins`, the function `Load` arms the fence with
(`modelaffinity.SetSeatPins`), asks the layers first (`Config.LayerPins`) and then the same serving config through the same reader
(`servingcfg.PinsOf`), with the same strictness. Until C-86a the fence read `layers` only, so on a box without them every lease
fenced every seat: a media lease on one card held back a text load for a model the serving config pins to another card, which
`gpu reserve` had been leaving resident since F15. Now a media lease on one card fences the models the serving config pins to that
card, the pairs that span it and every model nothing places, and a load for a model pinned to another card goes through at once
(`TestAOneCardLeaseFencesOnlyTheSeatsTheServingConfigPlacesOnThatCard`, in both directions). Everything listed as unknown above
stays fenced by any lease, and so do a whole-node lease, a card table that cannot be read and a serving config that is unset,
missing or not YAML (`TestTheFenceStaysClosedWhereTheServingConfigCannotPlaceAModel`). A serving config that is set and cannot be
read is not left invisible: the refusal a blocked load prints names only the lease holder, so the fence's reader says once per
cause, in the process log (stderr), `servingcfg: the serving config at <path> cannot be read (<cause>): the load fence places no
model by it, so a lease on any card fences every seat the layers do not place`. It says it again when the cause changes or when
the same fault returns after the file read cleanly; an unset path is the default and says nothing
(`TestPinsOfSaysOnceThatTheServingConfigCannotBeRead`, `TestPinsOfSaysNothingWhereThereIsNothingWrong`,
`TestPinsOfSaysItAgainWhenTheSameFaultReturnsAfterTheFileWasRead`, `TestModelPinsSaysOnceThatTheServingConfigCannotBeRead`). The
fence and the unload list cannot disagree about a model: both read one parser and `layers` outrank the file in both
(`TestTheLoadFenceAndTheUnloadListPlaceEveryModelAlike`, `TestLayersOutrankTheServingConfigForTheFence`). The fence's own lookups of a
seat's pins (`ScopeToModel`, which every admission, wait and yield goes through, and `SeatCards`) happen only when a held lease
names cards, so under an idle box or a whole-node lease the fence does not touch the file
(`TestSeatPinsAreNotLookedUpWhenNoLeaseNamesCards`, `TestOnlyTheFencesOwnLookupsWaitForALeaseThatNamesCards`). Every other reader of
`Config.ModelPins` asks on every call, an idle box included: `PinsFor` (the cards an agent run records), the text and vision auto
routes and the delegator's run-cap line each cost a Stat of the file per call. The file is Stat-ed on each lookup and parsed again
when its modification time or size changes, or when it changed less than three seconds before it was read, so a long-lived process
(the MCP server, the fleet node) follows a llama-swap config reload on its next poll without a restart
(`TestTheFenceFollowsAnEditOfTheServingConfigWithoutARestart`, `TestTheServingPinsAreReReadWhenTheYamlChangesSize`,
`TestLoadDoesNotTrustAReadOfAFileChangedInsideTheRacyWindow`). The same pins answer every reader of `Config.ModelPins`: the
waiter's place in line (`SeatCards`), `PinsFor` (the cards a run records), the text and vision auto routes and the delegator's run-cap
line. **Not changed:** a vLLM seat. Its serving entry is a `proxy:` (its cards live in its own unit) and `vllm_seats` carries names,
not cards, so a lease on any card still fences it; placing it needs the unit's pin, a separate change.

**Between renders (F15).** A `media` lease is not stamped exclusive (`Meta.Exclusive` is a text-lease flag), but the load
fence does not read the flag for it: `blocksLoadOne` gates `ClassMedia` from the end of its drain (a draining hold waits for the
runs in flight and fences once the stamp clears), to the cards it holds, through every door a harness load goes through. A load
that would land on a leased card waits (or, with a cascade remote lane, rides the lane), and a load onto a card nobody holds
proceeds at once, exactly as under an exclusive text lease (`TestMediaLeaseFencesTextLoadsOnItsCardsExactlyLikeAnExclusiveTextLease`,
`TestMediaLeaseFenceStartsWhenTheDrainEnds`, with the existing per-card cases in `seatscope_test.go`). Two doors stay open on
purpose or by reach: the holder's own process tree is exempt (`insideLease`: a text call in the render's children must not queue
behind its parent), and a client that is not the harness is not fenced by it at all (a hand-run check straight at llama-swap). A
seat that comes back through either would stay resident for the rest of the lease, because the unload is elected once per
lease. So a job that lost the election under a card lease with a non-empty list reads that list against `/running` again before
it submits (`withGpuSlot`, `reclear`): `freeLlamaSwap` unloads a listed model only when it is resident, so a card that is still
clear costs two reads and no unload, and a seat that came back leaves before the next render (that includes a model whose
placement is unknown, which is on the list). A whole-node lease, a lease with no list and the none sentinel keep the
once-per-lease election. Holding one lease across a batch (`--detach --for`) is still what keeps a text seat from loading between
two SEPARATE leases: with no lease held there is nothing to fence.

**A single-card seat's run-cap line is its card's.** The cap (`fleet_max_concurrent_jobs`, the local run line) counted the runs
registered on the seat by name. For a seat pinned to one card it now counts the runs on that card, whatever seat they run on
(`gpuactivity.Registry.OnSeatPinned`), in the node's gate and in the delegator's reading of its room
(`TestRunCapCountsPerCardForSingleCardSeats`); a seat that spans cards, or whose pin is unknown, keeps the per-seat line.

**`device-trespass` (possible).** When a lease names its cards (declared, or inferred for a legacy lease) and a card **outside**
them is busy with nothing the harness knows of to explain it, the activity view flags it as a **possibility**: the job may be
using a card it did not claim, so a waiter handed that card as free could collide with it (`gpu status`,
`offload_status.gpu_lease`, `device_trespass` in the JSON, `possible device-trespass:` in the note; `TestDeviceTrespassFlagged`).
It is never an accusation. Card-scoped leases make text seats on the free cards legal, and the harness registers the runs of
only two doors (`agent_run` and the delegation door) and watches only the planner seat's gauge, so a cascade call on a router
seat, an OCR or speech seat, or the desktop looks the same from here; the note says it is not attributed to the holder and
names what it cannot rule out (`TestTrespassIsOnlyPossibleAndNeverAccusesTheHolder`). Not flagged: the display card (the desktop's own use; the card table's marking, below), a card
another live lease holds, a card a registered run is pinned to, anything while the seat itself is busy (its card is unknown
here), and a whole-node lease (it has no outside). It is a flag on the existing verdicts, not a new verdict word. With a
bounded lease, `held-working` now means **its** cards are busy: a busy card outside the set is a seat's or another lease's,
never the holder's own job.

**Which card is "the display card" for the flag (F34, 2026-10-10).** The card table's marking: a card whose `display_active` reads
Enabled **or** whose `display_attached` reads Yes (`gpuprobe.ScreenCardUUIDs`, the rule that also keeps new work off the
monitor's card and folds the desktop's processes into one count), and, as a union, a card `display_active` alone marks. The flag
used to read `display_active` alone, which reads Disabled on every card of the reference 3-card box whenever no lit screen or game
is initialised, so 15 % or more of ordinary desktop load on the monitor's card, with a lease holding other cards, raised
`possible device-trespass` against the desktop
(`TestTrespassExemptsTheMonitorsCardWhoseDisplayActiveReadsDisabled`; a busy card that is not marked display is still flagged,
`TestTrespassStillFlagsAnUnleasedCardThatIsNotTheMonitors`; the union, `TestTrespassKeepsTheLitDisplayCardExemptWhereThePlacementRuleMarksNothing`).
A single-card box marks no display card (its only card is its work card), as in every display rule. **Named limit:** the
exemption is the card's marking, not its load, so an UNLEASED heavy job on the display card is not flagged either; from the
activity view the desktop's use and a stray job on that card look the same. The verdict's load reading (`held-working`,
`busy-outside`, the foreign-load sentence) keeps `display_active` alone, as before (`gpuprobe/display.go` says why).

**Not done here.** The activity snapshot trusts the nvidia-smi utilisation of a card, which on Windows includes the desktop
(only the display card is excluded: by `display_active` in the verdict's load reading, by the card table's marking in the
trespass flag); the trespass flag inherits that, and does not exclude a card that hosts a resident or busy
llama-swap seat (the view has no per-seat card map, so it can only soften its wording). A drain or unload whose facts cannot be
read (the card table, the roster, the lease's epoch) falls back to the whole-node behaviour and **says so on stderr**. The pipeline's own media lease exports no unload list
until it holds one card per render (P13). Fleet health reads the cards since P7 (see "The fleet reads leases per card").

## Who asked for a lease, and whether they are still there (ADR 0070)

*Plan P8 of the per-card routing work, registers C-32 and C-33. Everything here changes what is REPORTED. Nothing
reclaims, releases or kills a lease on it: taking a lease from a living holder is a separate, explicit command, and it
is not in this build.*

**The gap.** A lease recorded who HELD it (a wrapper pid and a heartbeat the wrapper writes about itself) and nothing
about who ASKED for it. A wrapper that is alive and heartbeating reads as healthy whether the session that launched the
job is at the desk or died hours ago, so "running" could not be told from "abandoned": a long render whose launching
session was long gone held a card for hours behind a label that read working, because card utilisation was its only
evidence and a heartbeat the wrapper writes about itself proves only that the wrapper is alive.

**What a lease now records** (additive and `omitempty`; a record without an owner is an UNKNOWN owner, never an orphan):

| field | meaning |
|---|---|
| `owner` | who asked: `session` (a session id), `pid` + `start_ms` (a process, judged like a holder: a recycled pid reads as gone), `remote` (asked for from another host), `tracked` (the session was in the registry when the lease was taken) |
| `unattended` | nobody is expected at the desk; the lease is judged by its progress contract and its window, never by its owner. `remote` implies it |
| `progress` | the progress contract: `file` the job appends to (always an ABSOLUTE path: `gpu reserve` resolves a relative `--progress-file` against its own working directory before recording it, and the library refuses a relative one, because every reader looks for the same file from its own directory), `stall_ms` it may stay still |
| `yield_grace_ms`, `on_yield`, `on_yield_dir` | the job's own terms for being asked to stop. The lease's HOLDER reads them when it is asked to yield ([An overdue lease's holder is asked to yield](#an-overdue-leases-holder-is-asked-to-yield-it-is-never-taken-from-plan-p11-adr-0086)); no takeover command reads them. `on_yield` is stored verbatim (refused above 4096 bytes, never clipped: the holder runs it) with the directory the lease was taken in, because a reader of the record does not share it |
| `yields` | `wrapper` or `detached`: the holder runs the yield request. A record without it was written by a build from before the request, or by a holder that is not a `gpu reserve` / `gpu hold` process (the media pipeline's in-process leases never carry it), and its holder is never asked |

**Where the owner comes from.** `gpu reserve --owner-session ID --owner-pid N --owner-start-ms MS [--owner-remote]`,
else the session label the ledger already resolves (`LOCAL_OFFLOAD_ORIGIN`, then `CLAUDE_CODE_SESSION_ID`) with NO pid:
the process a wrapper happens to run from is usually a short-lived shell, and recording it would read every lease as
abandoned the moment the shell exits. With neither, the owner is unknown. A lease with no owner can still carry
`--origin` (a free-text label of who asked, e.g. a launcher's name): it is not an owner and changes no verdict (the lease
stays never-orphaned, judged by its window), but `gpu status` shows it in the owner line, `owner: none recorded — origin
"<label>"; never orphaned, judged by its declared window only`, instead of `owner: unknown — no owner recorded`. A bench
launched from a systemd unit has no session in its environment, so before this its `origin` was in the record and on the
wire and nowhere in the text a person reads, and two sessions mixed up who held the card (F9, 2026-10-07). The origin is
printed quoted, on one line, and only when no session, pid or remote owner is recorded (it never displaces one); on the
wire it is `origin` in `gpu status --json` and in the `offload_status` lease block, and `activity.holder.origin`.
`local-offload gpu owner-flags [--pid N]`
prints the flags for the calling tree on one line (`--owner-session=ID --owner-pid=N --owner-start-ms=MS`), for a
launcher that detaches before it takes the lease (a process created through WMI has no parent to ask and does not
inherit the session). The hidden `gpu hold` child of a `--detach` reserve is passed the owner and the contract as flags
by its parent, because its own parent exits.

**The session registry.** Whether a session is alive cannot be read from its id. Each session's MCP server writes
`<state root>/owners/<session>.<pid>.json` (pid and start identity) when it starts and removes it when it stops (no
timer; a killed server leaves an entry whose pid reads as dead, which is the same answer). A session is alive while ANY
registered process carries its id, or the pid recorded on the lease is alive by pid and start time. A resumed or
compacted session starts a new process under the SAME id, so it does not flap. One file per process, not one per
session, because the new server of a resumed session and the old one's exit would otherwise be two writers of one
record. States: `alive`; `gone` (nothing alive, and the owner could be tracked: it was in the registry when the lease
was taken, or a pid was recorded); `unknown` (it could not be tracked, or the registry could not be read, so it is never
orphaned; `gpu status` says which: a session that was never in the registry is "recorded but cannot be tracked", and
"no owner recorded" is said only when none is, and a lease labelled only with `--origin` says "none recorded — origin ..."
instead); `remote`. A registry that cannot be READ (a permission error, a wrong
mount: anything but "the directory does not exist") is never read as "nobody in it"; a recorded process that is alive
still stands. *Unverified
beyond the environment variable the ledger already relies on: how a given Claude Code build maps a session to a live
process. The registry is the mechanism; a server older than this change simply has no entry, and its leases read
`unknown`.*

**The orphan marker.** The first reader to see an owner gone stamps `orphan.<epoch>` beside the lease, under the epoch
lock with a second look inside it, so concurrent readers write one marker and agree on one moment; the owner reappearing
clears it, so a session that came back starts its grace afresh if it goes again (only on positive evidence the owner is
back: an owner that merely cannot be told does not erase the moment it was seen gone). `held-orphaned` is
`now - marker >= gpu_orphan_grace_min` for an attended lease. **Only the status surfaces stamp it: `gpu status`,
`offload_status` and the `/fleet/health` handler.** It is a sidecar, and the one write in the read path. The plain
inspectors the text gate polls every blocked second (`InspectDir` and its kin), the sentence a waiter reads and the text of
a refusal (`ErrHeld.Error()`, the gate's `LeaseError`) stay read-only: they READ the marker a status surface recorded
(`StandingReadOnly`) and never write it or take the epoch lock, so formatting a refusal is safe anywhere. The consequence
is stated plainly: a gone owner nobody has looked at yet reads as inside its grace to a waiter, so a lone waiter on an
unobserved orphan is told what the progress file and the window say, and about the owner's absence once any status call
has recorded it. A marker that cannot be written or cleared is reported, never swallowed: `gpu status` and the verdict
note say `orphan marker could not be recorded: ...` (and `activity.holder.orphan_marker_error` carries it), and the lease
directory is probed for writability before the epoch lock is taken, because a permission error reads as contention there.
The marker is removed with the lease.

**The bounded-claim contract for unattended jobs.** `--unattended` requires an explicit `--for` (the 45 minute default
is not a declared window), `--progress-file` and `--stall`; `--yield-grace` and `--on-yield` are recorded and used only by the yield request. A job nobody
is watching has nobody to notice it going wrong, so its lease carries the terms it is judged by: `held-stalled` when the
file did not move inside the stall window, `held-overdue` when its declared window ends without its progress having
renewed the term (see [Terms](#terms-a-window-is-a-term-and-a-term-ends-in-a-renewal-or-a-label-adr-0070)). "Its owner is gone" is not an
escape and not a verdict for it. The stall window counts from the later of the file's modification and the lease's
start, so a log left over from an earlier run does not stall a fresh lease; a MISSING file is `unknown`, never stalled
(the job may not have written its first line yet), and the reading says why it is unknown (`does not exist`, `is a
directory`, `cannot be read: <error>`) and, once the lease has outlived its stall window with the file still missing, says
so (`has not appeared in 3h0m0s, past its 2h0m0s stall window: the job has not written to it or the path is wrong`)
without changing the verdict. `gpu reserve` warns when the progress file's directory does not exist. The last line of the file is shown ("clip 4 of 17"): a JSON line
contributes its `detail`, else `done` of `total`, else the raw line.

**A wrapped render helper declares its own contract and keeps it (F8).** A healthy 1,394 s Qwen-Image still run as
`gpu reserve --for ... -- node render/comfy-generate.mjs ...` had its lease labelled "expired ... open to a takeover" at
the end of its first term: the caller declared no progress contract, the lease recorded no owner it could tell apart,
and the helpers never wrote a progress file, so nothing vouched for a job that was rendering normally. The wrapper form
now closes both halves. **The wrapper declares the contract** when the command it wraps *runs* a helper that
heartbeats (`comfy-generate`, `-render`, `-edit`, `-inpaint`, `-video`, `-animate`, `-music`, `-upscale` and
`-run-graph`) and the caller declared none. It finds the helper where the command names it: as an argument behind an
interpreter (either path-separator style), in the inline command line of one (`cmd /c "node ..."`, `bash -c`,
`pwsh -Command`), or in a **script the command runs**: the wrapper reads the script (`.cmd`, `.bat`, `.ps1`, `.sh`,
`.bash`, `.py`, `.mjs`, `.js` or `.cjs`; the first 64 KiB, comment lines skipped) and the scripts that script names (at
most 8 files, 2 scripts deep, a script named by file name alone is looked for beside the one that names it), looking for
a helper's path: `set "R=...\comfy-generate.mjs"` in a batch file, `GEN="...comfy-generate.mjs"` in a shell script,
the two shapes the long renders of this workspace take (`cmd /c render-both.cmd`, `bash render-card-v3.sh`; the first
form of this change recognised only the direct `node comfy-generate.mjs`, and a recipe-wrapped render, the common case,
was still labelled). It reads text and does not parse shell. A script it cannot find or open (a variable the wrapper does
not have, a path it cannot read) is "nothing found", and the lease behaves as before. A script that names a helper but
also runs long steps that are not render helpers (a CPU bake, a download that outlasts the stall window) reads
`held-stalled` during them, as the undeclared job would have been labelled expired at the end of its first term: pass
your own `--progress-file` for such a recipe, or run the helper directly. **`--progress-file auto`** asks for the minted
file for a command the scan cannot read (a compiled driver, a script that builds the helper's name at run time): the
wrapper mints it, records the contract (with your `--stall`, else 15 minutes) and hands the path over in
`GPU_LEASE_PROGRESS_FILE`; whatever the command is, only a render helper that heartbeats writes to it, so one that does not
reads `held-stalled` once the window has passed. `auto` is the wrapper form's: a `--detach` hold wraps no command and
runs several over its life, so it declares nothing by itself and `--detach --progress-file auto` is refused; give it a
file of your own, `--progress-file <path> --stall <window>`, which the helpers find through the lease record
(`GPU_LEASE_DIR`/`GPU_LEASE_EPOCH`). The contract it declares is: a file `<state root>/progress/wrap-<pid>-<nanoseconds>.jsonl`
and a stall window of 15 minutes (chosen, not measured: more than three times the longest quiet stretch a polling
helper has, the 240 s `COMFY_DEAD_SEC` the poll loop itself tolerates), recorded on the lease like a `--progress-file`
the caller passed (so the term cap is the 24 hours a progress contract gets). It prints one line saying so, creates the
file just before the command starts (not at acquisition: the drain that follows can wait out a long seat turn, and a
file that existed through it would read stalled while the wrapper waits), and removes it when the command ends; a
wrapper killed before it could leaves a file the next wrapper that mints one removes after seven days
(`gpulease.SweepMintedProgress`, inline, no timer). Before the file exists and after it is removed (the queue, the drain,
the seat's unload, the warm-back) the lease declares a file that is not there, and `gpu status` says so in those terms:
"does not exist: the wrapper creates it when its command starts and removes it when the command ends, so it is absent
while the lease queues, drains, unloads the seat or warms it back" (not "the job has not written to it or the path is
wrong", which a drain of an hour made it say about a healthy lease); the verdict is unchanged, because a missing file
reads unknown and never stalled. A `--progress-file` of the caller's own is kept as it is and is
never removed. A command that runs none of those helpers (a bench, `sdcpp-generate`, a Go verb, which may overflow to
another node and never poll here) gets nothing declared unless the caller asks with `--progress-file auto`: a contract
that nothing writes to would only read stalled.
**The helper heartbeats it**: the path travels to the command in `GPU_LEASE_PROGRESS_FILE`, and
`render/lease-progress.mjs` appends one JSON line (`{"at":...,"pid":...,"detail":"ComfyUI is running prompt 1a2b3c4d (62s into the wait)"}`)
at most every 30 s, from the poll loop every ComfyUI runner shares (`pollOutputs` in `render/comfy-submit.mjs`), between
the pictures of a `--batch`, when the GPU slot is ready, and (`comfy-run-graph` with a manifest) once per model while it
provisions them, before ComfyUI exists to poll; one download or hash longer than the stall window still reads
`held-stalled` until the next beat. A helper under a lease whose record declares a progress
file but that was not started by the wrapper form (a `gpu reserve --detach --progress-file ...`) finds the file in the
lease record through `GPU_LEASE_DIR`/`GPU_LEASE_EPOCH`. **What a beat is evidence of**: a poll that ComfyUI answered, for
a prompt ComfyUI itself still lists (in `/history`, or running or pending in `/queue`). A hung helper stops polling; a
ComfyUI that stops answering is an unanswered poll; a prompt a restarted ComfyUI lost is listed nowhere; a poisoned CUDA
context ends the wait through the poll loop's own `/system_stats` probe. Each stops the beats, the file stops moving, the
lease reads `held-stalled` after its window, and at the end of its term it is labelled expired, as a lease with a
progress contract that stops moving always was; the command is never killed. **What it does not catch**: a prompt that
sits in ComfyUI's running queue with a wedged sampler while HTTP keeps answering and `/system_stats` stays healthy.
ComfyUI offers no polled step counter (its progress events are websocket-only and this tree is dependency-free), so that
case is bounded by the poll budget (`COMFY_WAIT_SEC`) and the lease's maximum total, not by a stall verdict. The
harness's own media doors take their lease through the library, not the wrapper, hold it for the one call, and are not
judged by this: no tick asks them about a term.

**Legacy leases and "is it stale?".** A lease with no owner and no progress contract (every lease written before this
change) can honestly be judged by one thing: its declared window, plus card utilisation, and the note then says
`util only, no progress contract`. Two read-only activity facts are shown as information and are never a verdict input:
the age of the newest ComfyUI output (or, when the output directory is too large to scan whole, a statement that it could not be determined: a capped scan never names an old file as the newest) and of the ComfyUI log. They are shown only when the ComfyUI launch marker can be
tied to the lease (the marker was written at or after the lease began and it is the only live lease): a marker left by
an earlier job proves nothing about this one. Full stale detection needs the new wrapper and a progress contract.

**Surfaces.** `gpu status` (and `--json`: `owner`, `unattended`, `progress`, and `activity.holder`) prints the owner,
its state, orphaned-since, the progress file's age and last line, and the facts. `offload_status` brief
`gpu_lease_verdict` leads with `ORPHANED`, `OVERDUE` or `STALLED` and names the takeover command, **whatever the verdict
word is**: a stalled, orphaned or overdue holder leads with its word even when work in flight makes the verdict `working`
(`STALLED (working) - the lease itself is not healthy (...), although work is in flight on the seat: ...`), and the takeover
command names the epoch of the lease the verdict is about. `/fleet/health`'s `lease` block carries `orphaned` and `stalled`
(each absent unless true; the worst across live leases) and `expired` (plan P9, absent unless true, read across every live lease; see [Terms](#terms-a-window-is-a-term-and-a-term-ends-in-a-renewal-or-a-label-adr-0070)); its `overdue` key is the expiry-based field of the routing change
(P1), not a second source from the standing. With several live leases (card-scoped) `gpu status` and the `offload_status`
lease view describe the MOST ESCALATED one from its own record (epoch, owner, progress contract), labelled with its epoch
(`gpu status --json` takes its top-level `epoch`, `owner`, `unattended`, `progress` and `devices` from that lease too, so it
never holds one lease's owner beside another's `activity.holder`; `epochs[]` lists every live lease and the text header
is the lowest).
A waiter refused by, or queued behind, an unhealthy lease (`gpu reserve`, `ErrHeld`, a text admission's `LeaseError`) is
told which lease, what is wrong, for how long, what is running and the command that frees it:
`local-offload gpu takeover --epoch N`. **That command does not exist in this build** (it is the explicit takeover of a
later change), and the message says so: until it ships, ask whoever owns the lease or the operator.
`gpu_orphan_grace_min` (default 15) is installed once at config load, so every surface reads the same grace.

**Not in this change, on purpose:** the takeover command (the holder-side yield request is [its own section](#an-overdue-leases-holder-is-asked-to-yield-it-is-never-taken-from-plan-p11-adr-0086)), `gpu release` routing (the process-tree record behind
`tree-orphan` is [its own section](#a-dead-wrapper-with-a-live-tree-keeps-its-claim-plan-p10-adr-0084)). A lease past its window with a live holder is surfaced as `held-overdue` and nothing else happens
to it; what its holder does at the end of a term is the next section.

## Terms: a window is a term, and a term ends in a renewal or a label (ADR 0070)

*Plan P9 of the per-card routing work. Leases still are never freed by expiry: **expired is a label, nothing reclaims.***

**The rule.** `--for` is the lease's declared window and its first term. `gpu_max_term_min` (default 360, six hours) is a
**renewal point, never a release point**: a request above it is recorded (`requested_ms`), warned about on stderr by
`gpu reserve`, **accepted whole and never shortened**, and renews in terms of the cap. A lease that declares a progress
contract (`--progress-file` and `--stall`) may declare up to 24 hours at the start without a word (the cap is the larger
of `gpu_max_term_min` and 24 hours for it): its progress file, not its window, is what judges it. The record carries
three additive keys (`omitempty`, ignored by every older reader): `term_ms` (what one renewal adds), `requested_ms` (only
when the request was over the cap) and `max_total_ms` (how long after acquisition the lease is renewed at all,
`gpu_max_total_min`, default 2880, 48 hours, and never less than the window it declared: a request is never judged past
its end). Both limits are installed at config load, like the orphan grace.

**What happens when a term ends.** Only the holder's own tick asks (the wrapper form's 15 s heartbeat; the detached
holder's renewal, at the same cadence). There is no timer, no watcher and no other process. Once the declared end has
passed, the tick renews the lease by **one term** when

- its owner is alive **and** (its progress file is advancing **or** its cards are working), or
- its owner cannot be told (see below) **and** its progress file is advancing, or
- it is unattended **and** its progress file is advancing,

and the new end stays inside `max_total_ms` from acquisition (it is clipped to the hard end, and a hard end already past
renews nothing). Otherwise it stamps the lease **expired**. The renewal is one term from the old end (terms run back to
back); a holder that slept through whole terms is renewed one term from now, so a lease that was just renewed never reads
overdue. Readings: an owner is alive by the same rule `gpu status` uses (a registered process of its session, or its
recorded pid, by pid and start time). An owner who **cannot be told** (a lease that records no owner, a session the
registry never held, or a registry that could not be read; the MCP server writes the registry when it starts
(`internal/mcpserver/mcpserver.go`, `registerSession`), so this is a session whose server never ran, a server older than the
registry, or a caller that is not a Claude session, and whether a given Claude Code build hands its MCP server the session id
is unverified) is not an objection and is not shown
present: an advancing progress file, which the job itself writes, vouches for the lease alone, as it does for an
unattended one. What such an owner does **not** get is the weaker leg: a busy card proves a process, not that anyone
wants the result, so a lease of an owner who cannot be told, with no progress contract or a stalled one, is labelled at
the end of its `--for` however busy its cards are (it already read `held-overdue` from that moment; the label adds the
sentence). A wrapped render helper is no longer in that position: the wrapper declares its progress contract and the
helper keeps it moving ("A wrapped render helper declares its own contract and keeps it", above). A gone owner is rescued by neither. "Its cards are working" is one `nvidia-smi` sample over the lease's
cards (the whole node for a whole-node lease), the display card excluded, at the verdict's 15 % threshold, taken only
when it can change the answer (an attended lease, a live owner, progress not already advancing); an unattended lease
never counts utilisation, and neither does a gone owner. The look has **three answers, not two**: *working*, *idle*
(every card it is judged by was read and is quiet) and *could not be read* (the sample failed or timed out, which is
likeliest when the GPU is saturated; a card that reports `[N/A]`; a lease card the sample does not list). Only *working*
renews, but *could not be read* is worded as what it is and never as idle cards. A missing progress file is `unknown`,
never advancing.

**What expired means.** `expired: true` and `expired_why` (the sentence: `its owner is gone`, `its owner is still there
but neither its progress file nor its cards show work`, `its owner is still there, but its cards could not be read and
its progress contract is not advancing (...), so nothing vouches for it`, `it is unattended and its progress contract is
not advancing (...)`, `its owner cannot be told apart (<why>) and its progress contract is not advancing (...), so
nothing vouches for it`, `it reached its maximum total of 48h0m0s ...`) on the lease's own record. The sentence is
**compared at every recheck** to decide whether the label changed, so it never quotes a value that moves with the clock:
how long the progress file has been silent is not in it (the progress line of `gpu status` and the stalled sentence say
that, live, from the file), only the stall window the lease declared. It is a **label**:

- the heartbeat goes on, the claim stays, the holder's fence still passes (`Check`, `Renew`, the per-epoch fence of
  `render/gpu-lock.mjs`), and the command under a wrapper is never touched;
- the verdict is `held-overdue` (no new word), the note starts `the lease has expired: its term ended 3h0m0s ago and its
  holder did not renew it because ...`, `gpu status` prints `term: EXPIRED ... ago and not renewed because ...`, and
  `/fleet/health`'s lease block gains `expired` (absent unless true; read across every live lease) beside `busy` and
  `overdue`, which stay true;
- it is takeover-eligible, which is a statement about a command a later change adds;
- it is **not reclaimable**: the reclaim conjunction is untouched (*holder provably gone* OR *heartbeat stale AND window
  expired*), so a heartbeating expired lease keeps its cards against any second job, and a holder that stops
  heartbeating is reclaimed by exactly the rule it was before;
- it clears itself: if a later tick finds the term renewable (the owner is back, the progress file moves) the lease is
  renewed and the label removed. A lease already labelled is asked again only every minute, so a day-long expiry does not
  sample the cards or print every 15 s, and a recheck whose answer has not changed writes nothing. The holder says it
  **once per expiry**: when the lease becomes expired, not each time the label's sentence is rewritten as the evidence
  changes (a look at the cards that failed, then one that did not). A renewal ends the episode, so the next expiry is said
  again.

**Why a label and not `State = "expired"`.** A record's `state` is the fence's word: `checkV2` and `render/gpu-lock.mjs`
fence out any state but `active`, and so does every binary and Node copy built before this change. An `expired` state
would make a render running under an expired lease read itself fenced out and stop, which is the mid-job loss this phase
exists to prevent, and no audit recalls a binary that is already running. A separate key is invisible to them. (The plan
drafted a `State`; this is the deviation, and `TestExpiredLabelNeverChangesTheFenceState` and the JS fence test pin it.)
The same reasoning leaves nothing to restamp on an extension: P2 dropped the pid-0 `meta.json` umbrella, so a renewal
rewrites only the lease's own record (`meta.json` for a whole-node lease, `e/<epoch>.json` for a card lease) and no
sibling lease's record or the legacy file.

**The detached holder.** `gpu hold` (the hidden child of `gpu reserve --detach`) used to exit at its `--for` deadline and
release the card whether or not the work behind it had finished; on 2026-09-07 that freed the card mid-job. It now ends
only when its lease stops being its own (an operator release, a fence-out). At the end of a term it renews or labels the
lease like the wrapper does and carries on holding. `--release-at-expiry` (on `gpu reserve --detach`, passed to the
holder) restores the old ending; it is refused for the wrapper form, which has no deadline to release at. `--for` stays
required with `--detach`: it is the term the lease is judged by. A hold that nothing ever releases stays held until it is
released or taken over, which is the point.

## An overdue lease's holder is asked to yield; it is never taken from (plan P11, ADR 0086)

*Opt-in with `gpu_on_overdue = request`. Everything here is the action of the lease's own holder; the harness never frees a
claim whose holder is alive, never stops a command and never signals a process. The takeover command stays unbuilt.*

**The gap it closes.** A lease past its window with a live holder is `held-overdue` and nothing happens (ADR 0070). When
somebody waits for its cards the only recourse was a human running `gpu release --epoch N`, which for a wrapper makes the
wrapper end its own job within about 15 s while the card is free for a second job (`LEASE LOST ... killing it`), and for a
detached job removes the claim and leaves the job on the card. The job's own `--on-yield` and `--yield-grace` were recorded
and read by nothing.

**The policy.** `gpu_on_overdue` (flat key, installed at config load):

| value | what the harness does |
|---|---|
| unset (the default) | what it did before the key existed: nobody is asked |
| `request` | the lease's holder is asked to yield and runs the job's own recorded `--on-yield` |
| `hold` | nobody is asked, and a surface that explains an overdue lease says so |

Matching trims and lower-cases (`" Request "` is valid). Anything else (`yield`, `kill`, `takeover`, a typo) is refused by name
(`request` or `hold`) when the config loads; a one-shot verb (`gpu reserve`, `gpu hold`) goes on after that warning, and the
value it runs under is `hold`, so an unrecognised word never asks. The holder's first tick says so once. There is no
automatic mode.

**When the holder is asked** (its own tick, every 15 s, right after the term check; decided from the record alone, cheapest
first). All of: the policy is `request`; the record is labelled expired or its declared end has passed (a lease the same tick
renewed has a later end and is not asked); the holder was not asked yet in this epoch; the record carries `yields`
(below); the lease has read overdue, on every tick of this holder, for its settle interval (below); and a live waiter, not the holder's own process and not one waiting only on host RAM, wants a card the lease holds
(`DevicesConflict`; a whole-node lease conflicts with every waiter). The same waiter, by pid and start identity, must be seen on
two consecutive ticks: the ask is once per epoch and cannot be taken back, so a short `gpu reserve --wait` probe or a seat
admission that leaves after seconds is not demand; the count starts on the first tick after the settle interval ends.
**A label has to have survived a look.** The term check looks again at a lease it labelled expired once a minute
(`termRecheckEvery`), and a look at the cards that fails (most likely when they are busiest) labels a lease that a second look
would renew. The ask cannot be taken back, so the holder settles for that same interval: the lease must have read overdue on
every tick of the holder for a minute before it is asked, which puts the earliest ask about a minute and a half after the term
ended. A progressing job is renewed by its owner's term check
([Terms](#terms-a-window-is-a-term-and-a-term-ends-in-a-renewal-or-a-label-adr-0070)), so it is not overdue and is not asked;
only a lease labelled expired, or one that reached `gpu_max_total_min`, is. A seat waiter registered for the whole node (the
card table was unreadable when it queued) conflicts with every lease and can ask an expired lease on a card it does not need;
it must stay two ticks all the same.

**The sidecar, `yield.<epoch>`** (additive; beside `hb.<epoch>` and `orphan.<epoch>`; removed with the lease by both sweeps):

| field | meaning |
|---|---|
| `epoch`, `asked_ms`, `by_pid` | which lease, when, and the holder process that asked |
| `basis` | `overdue by 12m0s`, plus `; not renewed: <why>` for a labelled lease |
| `waiters`, `queued` | up to three waiters (`pid`, `class`, `reason`) and how many there were |
| `policy`, `grace_ms` | `request`, and the job's declared `--yield-grace` (0 = none) |
| `acted_ms`, `action`, `exit` | what the holder did: `on_yield running`, `ran on_yield`, `on_yield failed` (with its exit status), or `no on_yield recorded` |

The ask is written under the epoch lock after the writability probe and a liveness check, with tmp and rename, exactly as the
orphan marker is; the second write takes the same lock and the same checks, so an update that races the release leaves no file.
If the ask cannot be recorded, nothing is run and the holder says so once; the next tick tries again.

**What the holder does.** It runs ITS OWN in-memory `--on-yield` (never the record's copy: a rewritten record cannot make it
execute something else) once, in its own working directory, which is the directory the record names, through `sh -c` (Unix) or
`cmd.exe /d /s /c` (Windows; so **name a script**, shell syntax differs), hidden, with its output discarded (a wrapper's stdout
is the job's), with `GPU_LEASE_EPOCH`, `GPU_LEASE_DIR` and `GPU_LEASE_YIELD_FILE` added to its environment. It runs in a
goroutine, so the heartbeat goes on, and is never waited out or cut short. One stderr line says it was asked
(`gpu reserve: asked to yield (overdue by ...; 1 waiter(s) queued: ...); running its --on-yield "...". The job keeps running
until it stops; nothing is released or killed`), and a failed `on_yield` is said once more. With no `--on-yield` recorded it
says so and points at the file. A waiter's reason is free text typed by any local process, so line breaks and control
characters in it become spaces before it reaches that line. What the holder did (it started its command, how it ended, that it
had none to run) is written to the sidecar; a write that fails is said once (`could not record what its --on-yield did`) and
tried again on the next heartbeat, with the time of the event, and a request file that exists and cannot be read is an error,
not "never asked". The source of this path is pinned to contain no stop, signal or release call.

**What the job sees.** A wrapped command gets `GPU_LEASE_YIELD_FILE`, the path of the sidecar; the file exists only once the
holder was asked, so a job can poll it between its items (the variable is set under every policy and names a file that never
appears unless the policy is `request`). A detached job is not the holder's child, so under `gpu_on_overdue = request`
`gpu reserve --detach` prints `yield request: ... a job can watch <path>` (and `yield_file` with `--json`), saying whether the
holder runs an `--on-yield` or none was recorded, and `gpu status --json` carries `activity.holder.yield_file`. Under the default,
`hold` or an unrecognised value nobody is asked, so the detach report and the status wire are what they were before the key
existed; a status call names the file once a holder was asked, whatever the policy says by then. The detached holder's own stderr is a temporary log, not a notification stream: for it the
sidecar and the status surfaces are the record.

**Which holders can be asked.** The record carries `yields` (`wrapper` or `detached`), stamped at acquire by a holder that runs
this. A wrapper version cannot say it (every 0.18x build stamps one and none of the older ones ticks the request), so a record
without `yields` is never asked and every surface says `its holder does not run the yield request (a build from before it, or
an in-process holder such as the media pipeline's) ... ask whoever owns it`. Two kinds of holder carry no stamp: every job
started before the upgrade, and the leases this build's own media pipeline takes in process, which never tick the request. A
restart helps the first kind and nothing helps the second. A detached holder started with `--release-at-expiry` releases at its
deadline and stamps nothing.

**What a waiter and a status call read** (read-only; nothing here writes):

| state | sentence |
|---|---|
| unset policy | nothing; the sentences are those of before the key existed |
| `hold` (or an unrecognised value) | `gpu_on_overdue=hold: its holder is not asked to yield; it holds until it ends` |
| `request`, holder cannot be asked | `its holder does not run the yield request (a build from before it, or an in-process holder such as the media pipeline's), so no request can be made of it: ask whoever owns it` |
| `request`, not asked yet | nothing |
| asked | `its holder was asked to yield at 14:02 (2 waiter(s) queued) and ran its on_yield at 14:02 (exit 0); it still holds the card 3m0s later` |
| asked, past the job's grace (10 minutes when it declared none, a display-only figure), and the lease is still overdue | `asked 14m0s ago and it has not ended: nothing is killed; ask whoever owns it, or stop its job by hand (wrapper pid 77); the claim frees when the wrapper ends` |
| asked, and the lease was renewed afterwards (the sidecar stays until the lease ends) | `its holder was asked to yield at 14:02 (2 waiter(s) queued) and ran its on_yield at 14:02 (exit 0); the lease has since been renewed and is not overdue now`; `unanswered` is false |

For a **detached** holder the unanswered sentence names the release command with what it does: `have its driver stop the job and
run local-offload gpu release --epoch N (the job is a separate process: that command frees the claim at once, and the job
keeps running on the card until it ends)`. **No sentence about a live wrapper names `gpu release`**: releasing the claim of a
live wrapper makes it end its own job within one tick. The sentences ride `ExplainHeld` (the waiter's refusal), the standing
notes of `gpu status` and `offload_status`, and the brief line; `activity.holder` carries `yield_file`, `yield_note` and, once
asked, `yield` (`asked_at`, `acted_at`, `action`, `exit`, `waiters`, `grace_s`, `unanswered`). `yield_file` is named while the policy
is `request` and, whatever the policy says by then, once the holder was asked; under the default policy a lease nobody asked
carries none of the three. A holder that cannot be asked carries `yield_note` under `request` and no `yield_file`, and
`yield_note` and `yield` are absent until there is something to say.

**When the holder does not yield.** Nothing automatic. The lease stays held-overdue, its heartbeat and fence intact, its
waiters keep their place in line (FIFO unchanged), and the surfaces move from asked to unanswered wording. It frees when the
holder ends, or when a human acts. The request is node-local: the fleet node's lease standing wire does not carry it. The
takeover that stops a tree, the hardening of `gpu release --epoch N` over a live tree (plan P12) and the registers they close
(C-32, GPU-OPORDER) are later changes.

## Host RAM: a lease declares what it will load, and the grant holds back a load that would pass a stated line (2026-10-09)

**The rule (AGENTS.md).** The cards do the inference; RAM is overflow only. Spill is allowed only while it is
bounded, and it never makes the box unstable. **What the guard keeps of that rule** is narrower, and is stated here so it is
not read as more: before a lease that declares host RAM is granted, the guard holds it back if either of two things its
declaration adds would pass a line. The **working set it will touch** is held to the memory that is *available* (less a
headroom): that is what pages when it runs out. The **commit it will be charged** is held to a line that depends on who said what
the commit is. A job that **states its commit** (`--ram-commit`, or a caller that measured one) is held to the OS *commit limit*
(less the same headroom), where the OS enforces one: past it other processes' allocations fail once the page file has reached its
maximum (the limit moves with a system-managed page file: "The commit limit moves", below). A job that **states none** declares one
figure, and its commit is that figure standing in for one: it is held to *physical RAM* (less the headroom), the line every such
caller always had, because a job can commit more than it touches (a coder-model arm touched 27.8 GiB and was charged 58.1) and the
commit limit lies 44 GiB further out than physical RAM on the reference box, so against it two such arms were admitted into commit
exhaustion that the old line refuses ("Why two quantities", below). Committed memory is not memory in use, which is why a job that
has measured its commit is not held to the physical line. The lines are the guard's own, chosen 2026-10-09 as a conservative way to
keep spill bounded; they are a definition, not a measurement of any box's paging. The guard compares a *declaration*, which is an
estimate: it does not bound what a lane then commits and it does not stop a process that holds no lease (the measured case in
which an admitted lane leaves the box close to a line is under "Known limits", below). A card has a hard edge (the driver
refuses the allocation); host RAM does not (the OS pages and every other process stalls), so the harness checks *before* the
grant, on numbers that include what the granted jobs are about to load.

**The incident.** On the reference 3-card Windows box (127.7 GiB physical), two ComfyUI media lanes that stream bf16
weights the card cannot hold ran at once under two card-scoped media leases (`gpu reserve --devices <card> --class media --
node render/comfy-generate.mjs --batch ... --family krea2 --ckpt ...`). A Krea 2 bf16 UNet (24.5 GiB) and its Qwen3-VL-4B
text encoder (8.3 GiB) on a 16 GiB card, and a Qwen-Image 2512 bf16 stream of about 38 GiB. Committed memory reached
162.9 GiB against a 187.7 GiB limit, the system-managed page file grew from 60 to 68 GiB, and free RAM bottomed at 3.2 GiB.
The non-media baseline on that box was about 56 GiB (desktop apps, agent CLIs, browsers, WSL, kernel pools). One keyed
per-card ComfyUI instance held 57 GiB private. The probable cause is that it still cached a Qwen-Image model from an earlier
lease next to the Krea 2 model of the current one (reconstructed from the code and the surviving logs, not observed); the
session that handled the incident reported that POSTing ComfyUI's own `/free` released 52 GiB of it between prompts without
killing a job, and no record of that reading survives here.

**Why the harness did not stop it.** The card allocator's host term (`allocator.go`) applied only to `--cards`; an explicit
`--devices` lease, which is what every owner wrapper takes, bypassed it. It read **free** RAM, which says nothing about jobs
that have been granted and have not loaded yet (both lanes were granted while RAM was still free). And nothing read
**committed** memory at all.

### What a lease declares

A lease carries `Options.HostRAMGiB`, the working set it will touch, stamped on its record as `host_ram_gib` (additive and
omitted when 0: an older binary ignores it, and a record without it declares nothing), and `Options.HostRAMCommitGiB`, the commit
it STATES it will be charged, stamped as `host_ram_commit_gib` (additive; 0 or absent means no commit was stated and the commit is the
same figure as `host_ram_gib`, which is what every caller that declares one number leaves, so such a caller is judged on commit
against physical RAM as it always was; present, even equal to `host_ram_gib`, it is stated and judged against the commit limit).
Where the touched figure comes from, in order
(`internal/hostneed`; `TestAKrea2Bf16CallOn16GiBIsTheUnetPlusTheTextEncoder`, `TestAnExplicitRamBeatsTheEstimate`,
`TestTextAndSeatLeasesDefaultToZero`):

| source | what it is |
|---|---|
| `gpu reserve --ram <GiB>` | the operator's word. **0 is allowed** and means "needs no host RAM". A negative value is refused. |
| a recognised render helper call | `render/comfy-generate.mjs`, `comfy-render`, `comfy-edit`, `comfy-inpaint` or `comfy-video`, wherever it sits in the wrapped command (either path-separator style). The UNet or checkpoint and the text-encoder files the family loads are sized through the configured ComfyUI model paths (`mediacap.ModelRoots`, the same tables `doctor` reads) and counted **in full when together they do not fit the largest card of the lease** less the runner's `--reserve-vram`; a set that fits declares 0. A krea2 bf16 call on a 16 GiB card is 24.48 + 8.27 = 32.75 GiB. A video call is sized from the files its flags name (`--transformer`, `--high-unet`, `--low-unet`, `--text-encoder`) and the builder's default for each it is not given, because a hand-run helper gets nothing from the machine's config: a bf16 LTX-2.5 `--transformer` is 39.13 + 14.32 = 53.45 GiB where the int8 default is 20.03 + 14.32 (`TestAVideoCallIsSizedFromTheFilesItsFlagsName`). `run-graph` (`comfy-run-graph.mjs`) is not recognised and takes the class default below. A helper run with `--graph` (`comfy-render`, `comfy-video`) posts the caller's own workflow, so its family flags say nothing about what it loads and it is never sized as the helper's default family: it is sized by the next row, and takes the class default only when that cannot be done, on a text lease too (`TestAGraphCallIsSizedByTheClassDefaultNotItsDefaultFamily`). |
| a helper's own `--graph <file>` | the **weight files the graph's own loaders name** (`hostneed.ForGraphFile`, source `graph loaders`). The file must be a regular file of at most 8 MiB holding a ComfyUI API-format graph. `mediacap.ScanGraph` (the reader the node door's held-by-name check uses: any string input that ends in a weight extension, whatever the loader calls it) lists the names, each once, and `mediacap.ModelFileSizes` finds each under any class directory of any ComfyUI model root, as that check does (a name held under two class directories counts as the larger). The figure is the **sum of their sizes**, by the family estimate's own rule (`hostneed.fitToCard`, one function both use): a VAE or a LoRA counts with the checkpoint, the sum is declared **in full when together the files do not fit the card** (the largest card of the lease, less the runner's reserve; a card that is not known counts everything) and **0 when they fit**, with the same wording ("does not fit the N GiB card, so the weights stream from RAM", "fits the N GiB card ..., so no weights stream from RAM"). A 512x512 qwen-image graph of a 6.76 GiB checkpoint and a 16.33 GiB text encoder is 23.09 GiB on a 16 GiB card, where the class default of a node that binds a Wan 2.2 video pair is 32.89 GiB (`TestTheIncidentsGraphIsItsOwnFilesNotTheLargestFamilyOfTheBox`, `TestAGraphNamingTwoLoaderFilesDeclaresTheirSum`, `TestAGraphThatFitsTheCardDeclaresZeroAndOneThatDoesNotDeclaresItsSum`, `TestAGraphAndTheFamilyEstimateAgreeOnTheSameFiles`). **Any doubt keeps the class default, exactly as before**, because a size that is not known must never shrink a declaration: a document that is not a JSON object (a UI-format workflow included), a graph that names no weight file, ONE named file that is not in the model tree, is empty, or is not a safe relative path, or a weight-like name anywhere in the document that the sizing did not cover (the whole JSON is walked, nested objects and lists included, for a string ending in a weight extension: the scan's six and `.bin .pt2 .pkl .pickle .onnx .engine .trt .npz .h5`; a partial sum would be low) (`TestAGraphWithOneUnresolvedFileKeepsTheClassDefault`, `TestAGraphThatCannotBeReadKeepsTheClassDefault`, `TestAGraphThatNamesNoLoaderKeepsTheClassDefault`, `TestAGraphNamingAWeightItCouldNotSizeKeepsTheClassDefault`). The card is fitted with the call's own `--reserve-vram` (a helper call) or `reserve_vram` (the `run-graph` lane), a value the runner would not take being the default (`TestAGraphIsFittedWithTheReserveVRAMTheCallStates`, `TestTheGraphLaneFitsTheGraphWithTheReserveVRAMOfTheCall`). **Limits:** it is an estimate from the files' sizes, not a measurement; it counts only weight files, so an input image or a custom node's own data file is not in it; a weight is recognised by its extension, so one named by another extension, or built at run time, is not seen and the figure can be low; and the family routes of the pipeline lane (image, edit, video) are still fitted with the config's reserve, not the request's `reserve_vram`, which only the runner receives. |
| a file whose size cannot be read | the documented per-family size (`hostneed.familySizes`: the files the reference box binds, rounded up; Qwen-Image 2512 bf16 is the incident's ~38 GiB stream). A family with neither has no estimate and takes the class default. |
| the **media class default** | for a media lease with no `--ram` and no recognised call: the largest estimate over the render families **this box binds** (`imagegen_*`, `gen_edit_*`, `videogen_*`, named families; a binding on the sd.cpp engine is not counted, on whichever route it is, a video family included), so a 32 GiB node is not held to a 128 GiB node's numbers; 0 when nothing is bound. It is never clamped to fit the box: a default that cannot be admitted is refused with the reason and `--ram` as the way out. |
| a text lease | 0: the harness cannot size an arbitrary wrapped command, so a text reservation declares nothing unless `--ram` says (the agent seat's own reload is sized where the harness does it, below). |

**The commit, `gpu reserve --ram-commit <GiB>`.** Commit is memory the OS has promised the job, touched or not, and it can be much
more than the job touches. Measured on the reference box (session readings, not recorded in this repository): one
coder-model arm's process tree reached a 27.8 GiB working set and a 58.1 GiB private charge while the host's available memory
fell from 91.5 to 61.1 GiB with no paging; a llama.cpp leg with every layer on the card held a 3.6 GiB working set and a 32.1
GiB charge. So the arm states `--ram 28 --ram-commit 58`. **Stating the commit is what moves it to the commit limit.** Unset,
`--ram-commit` is not stated: the commit is the `--ram` figure standing in for one, **including when `--ram` was estimated**, and
it is held to physical RAM less the headroom, the line a job that declares one number has always been held to (or to an enforced commit limit
less the headroom where that sits below physical RAM: strict-mode Linux with little swap, where the kernel refuses allocations at the limit). A commit stated
equal to `--ram` is still stated (the lease record keeps it and the detached holder is passed it), so a job that measured its
commit and found it equal to its working set is judged on the limit. A commit is declared beside the working set it goes with:
`--ram-commit` needs a `--ram` (given or estimated) above 0, and is itself above 0 and finite (`NaN` and `Inf` are refused by
name, as is a non-finite `--ram`: neither compares against anything; the rule itself and the hidden `gpu hold` verb refuse a figure that is not finite as well,
`TestARuleCallerThatDeclaresAFigureThatIsNotFiniteIsRefused`, `TestHoldRefusesAHostRAMFigureThatIsNotFinite`). The converse is a known limit: a job whose commit is
*smaller* than its working set (mapped files count in the working set and not in the commit) cannot say so by leaving the flag
out, and is judged on the larger figure; `--ram-commit` states a smaller one. A caller that never heard of the flag declares one
figure and states no commit, and keeps the protection it had: the media admission, the seat's warm-back, the read-only per-node
check, and the card allocator unless it is handed a stated one. The footprint store records the private peak of a render's tree,
and the media admission and the warm-back could pass it as a stated commit; that is not done here. A lease that unloads
the seat (`--unload-seat`) carries both figures through the wait (`host_ram_wait_gib` and `host_ram_wait_commit_gib`), and
admission moves both into the declared fields in one step, a stated commit staying stated.

**When the figure is the class default, the verb says why** (F71, `classDefaultHint`;
`TestReserveNamesTheUnrecognisedWrapperWhenItDeclaresTheClassDefault`, `TestReserveNamesTheGraphWhenARecognisedHelperRunsOne`,
`TestReserveNamesTheUnsizedHelperWhenItsFilesCannotBeSized`, `TestTheHintChangesNoFigure`). The class default is a figure for a call the
lease could not read, and the declaring line (`gpu reserve: declaring <N> GiB host RAM (class default: ...)`) does not tell a caller whose
weights sit on the cards that it is theirs to correct: a driver script around card-only renders declared 46.8 GiB for weights that sat on
two cards and waited a queue cycle for host RAM it never used. So when the declared figure is the class default and `--ram` was not given,
ONE more line follows the declaring line and names how the call reached the default: nothing is wrapped (a `--detach` hold); the wrapped
command is not a recognised render helper (a driver script, a Python wrapper, `comfy-run-graph.mjs`; the helpers that are recognised are
named, from the same table that recognises them); a recognised helper runs its own `--graph` that the lease could not read and size in
full (a graph it can is declared from its loaders, above, and gets no hint); or a recognised helper's weight files could not be sized. The line ends with the way out: `--ram <GiB>` states the real figure, and `--ram 0` is the answer when the weights sit on the
cards. It is printed for none of: an explicit `--ram` (0 or any figure), a figure estimated from the helper's own files, a text lease that
wraps no render helper, or a box whose class default is 0. The declaring line stays one line (the new one never contains the word a script
greps it by), and no figure, lease record or queue behaviour changes.

**Why the full file size and not the overflow.** ComfyUI's dynamic VRAM stages the whole file in host memory ("Model Krea2
prepared for dynamic VRAM loading. 24449MB Staged", the encoder "8463MB Staged" in the incident lane's own console log),
and its RAM-pressure cache keeps it there between prompts. The same principle says a set that fits the card streams nothing
from RAM, so it declares nothing. The media admission (`internal/pipeline`) asks the same estimate per route, from the
binding it is about to render with, once the card it will run on is known: image, edit, inpaint, video (by the family that
will render, with the request's own `transformer` put over the family's binding, because that is the file the runner
loads: `TestAVideoRequestsOwnTransformerIsWhatTheAdmissionDeclares`), animate and music read their files; the `run-graph` lane declares the weight files its graph's loaders name, as `gpu reserve` does for a helper's `--graph`, above
(`pipeline.graphRAM`, read from `graph_path`; `TestTheGraphLaneDeclaresTheFilesItsGraphNamesNotTheClassDefault`, and the lane probe's
differential test), and keeps the class default for a graph it cannot read or size; a template run (`template` with `manifest_path`,
no graph file) declares the model files its runtime manifest names, by the same rule and source (`hostneed.ForManifestFile`, read from
`manifest_path`; `TestTheRunGraphLaneIsAdmittedOnTheFilesItsTemplateNames`, and a row in the probe's differential test), on a box
that binds no ComfyUI family too, and keeps the class default for a manifest it cannot read, one that names nothing, or a file it cannot
find or size; the audio.cpp lanes take the class default (the largest ComfyUI
family the box binds, an upper bound that is not their own size; a box that binds no ComfyUI family, an sd.cpp-only node, has
a class default of 0, `TestAnSdcppOnlyBoxDeclaresNothing`). **The sd.cpp lanes (image, video, animate) declare the larger of that
default and the weights they park themselves** (F60, `hostneed.ForSdcpp`, `pipeline.sdcppDeclared`): the bound files in full
(the diffusion model, a Wan 2.2 high-noise expert, the text encoders, the VAE), when the lane's extra args carry
`--offload-to-cpu`, which parks the weights in RAM and stages them to the device (sanctioned spill, 5.5 to 6.2 GiB on the small
nodes per the review); without the flag nothing is parked and the lane declares what it did. So on an sd.cpp-only node the spill
is inside the brake. **It is an estimate, from the files' sizes, and says so; nothing here was measured on a node.** The need
carries the lane's footprint key, so three measured runs raise it like a ComfyUI family's (`calibratedRAM`). A file whose size
cannot be read (missing, empty) is left out of the sum and named: the figure is then worded as a **lower bound** (`Need.Unsized`
lists the binding keys), the lane never declares less than the class default, and it says once per lane and set of files which it
could not size (`media admission:` log line). When none of its files can be sized the class default stands, which is 0 on an
sd.cpp-only node, and the lane says that once too. Not covered: the audio.cpp voice and music lanes, the depth
model of the animate lane, and `gpu reserve` around an sd.cpp helper (the CLI recognises only the ComfyUI helpers, so it still
declares the class default or `--ram`); upscale and voice declare nothing, their weights sit on the card.

**An unknown card counts everything.** A set that fits the card declares 0 only when the card is known. A host that has not
enabled card-scoped leases reads no card table (its leases are whole-node, as before, and it may have no NVIDIA card to
ask), and a table that cannot be read is the same, so there the files count in full: the conservative figure, with `--ram`
as the way to a smaller one (`TestReserveOnAFlagOffHostDeclaresTheFilesInFullWithoutReadingTheCardTable`).

### The admission rule

Every grant path applies **one** rule (`gpuprobe.HostRAMAdmitsLoad`). It reads two counters of the OS, the memory that is
available (`GlobalMemoryStatusEx` `AvailPhys` on Windows, `MemAvailable` in `/proc/meminfo` on Linux) and the commit charge and
limit (Windows: total minus available page file, and the total, which on that call is the commit limit, physical RAM plus the page
file; Linux: `Committed_AS` and `CommitLimit`), and admits a lease iff BOTH conditions hold:

```
touched:  available RAM now - the lease's touched need - the not-yet-touched part                              >= headroom
commit:   commit charge now + the lease's commit need + the not-yet-charged part        <=  commit limit - headroom     (a STATED commit, where the OS enforces a limit)
          commit charge now + the lease's declared figure + the not-yet-touched part    <=  physical RAM - headroom      (a commit nobody stated, on every reading)
          (headroom = gpu_host_ram_headroom_gib, default 8 GiB: chosen, not measured; see "How the numbers get measured")
```

A quantity a lease declares as 0 is not judged (it adds nothing to that counter), a lease that declares neither is admitted
without reading anything, and a touched need above physical RAM less the headroom, or a commit above the line it is judged
against less the headroom, is refused at once (waiting cannot help). A refusal says which condition refused and with whose numbers,
and says both when both did: `needs 28.0 GiB, only 30.0 GiB of 127.7 GiB physical is available, 8.0 GiB headroom`, `needs 58.1 GiB of
commit, committed 150.0 of 171.7 GiB commit limit, 8.0 GiB headroom` for a stated commit, and `needs 46.8 GiB, committed 84.6 of
127.7 GiB physical, 8.0 GiB headroom` for a commit nobody stated, which on a host whose commit limit lies beyond physical RAM adds
that a stated commit is held to that limit instead (`TestEachConditionRefusesAlone`,
`TestAGrantIsRefusedOnItsCommitAlthoughItsTouchedSetFits`, `TestAGrantThatStatesNoCommitIsJudgedAgainstPhysicalRAMLessTheHeadroom`).
A reading from a platform whose limit is always enforced that carries none (a Windows total page file of 0) is not a reading: the
rule waits on it as unreadable and the verdict is `unknown` (`TestAHardReadingWithoutACommitLimitIsUnreadable`), where it used to
skip the commit condition without a word.

**Why two quantities, why those lines, and why a commit nobody stated keeps the old one.** The first version compared the commit
charge plus the need against *physical RAM* less the headroom, for every caller. Committed memory is not memory in use: on the
reference box (127.7 GiB RAM, 171.7 GiB commit limit) it read 70.5 GiB committed while 75.6 GiB were physically free (2026-10-17),
and a coder-model arm that touches 27.8 GiB and is charged 58.1 GiB was refused (70.5 + 58.1 = 128.6 against 119.7). A job that
**states** its commit is judged against the commit limit: the same arm needs 128.6 of 163.7, and against the available memory 27.8
of 67.6: admitted (`TestMeasuredCoderArmIsAdmittedAgainstTheCommitLimit`). The commit charge cannot simply be dropped: on Windows it,
not the physical availability, is what can run out, because a job that commits about twice what it touches can push the charge
past the limit (physical RAM plus the page file) and, once the page file has reached its maximum, other processes' allocations fail
with plenty of RAM free (before that Windows grows a system-managed file: "The commit limit moves", below). And what a
job touches cannot be judged against the commit limit, because the available memory is what pages when it runs out. Each quantity
is held to the line that is real for it.

**That holds for a commit somebody measured. A caller that declares one figure measured nothing:** its commit is its touched figure
standing in, and a job can commit twice what it touches. Against the commit limit the two coder arms declared as 27.8 / 27.8 each
were both admitted: the touched line leaves 20.0 GiB of the available memory and the limit leaves 126.1 projected of 163.7, while
the old line refuses (126.1 > 119.7), with arm 1 still loading and once it had loaded (128.6 committed, 156.4 projected). After
both load the real charge is 70.5 + 2 x 58.1 = 186.7 GiB against the 171.7 GiB limit, 15.0 GiB past it, with the touched set at
55.6 of 75.6 available: commit exhaustion, not paging. A single figure of 49.2 to 67.6 GiB at that reading was newly admitted for
the same reason. So an unstated commit keeps the line it always had, and those arms are refused as one figure
(`TestTwoCoderArmsDeclaredAsOneFigureAreRefusedOnTheCommitCharge`,
`TestTheBandThatTheCommitLimitOpenedForASingleFigureIsRefusedAgain`) and admitted only on commits stated and fitting the limit
(`TestTwoCoderArmsAreAdmittedOnlyOnStatedCommitsThatFitTheLimit`: arm 1 at its measured 58.1; a second arm up to 35.1 GiB of stated
commit once arm 1 has loaded; the measured 58.1 twice is 186.7 against 163.7 and is refused). A job that commits less than it
touches is the safe direction for the default (it over-declares); one under the display driver model can commit 2x to 9x what it
touches (the coder arm 58.1 on 27.8, a llama.cpp leg 32.1 on 3.6), which is why the default cannot be the limit. The incident of 2026-10-09 is refused whatever the
available memory is: with a 56 GiB desktop baseline the first 32.75 GiB lane is admitted and the second is refused while the first
has not loaded, on the commit charge (56 + 65.5 = 121.5 against 120) at any available figure and on the available memory too at 72
GiB (`TestTheTwoLaneIncidentIsRefusedWhateverIsAvailable`, `TestTheTwoLaneIncidentIsStillRefused`).

**The commit limit moves.** The limit read is the *current* commit limit (`ullTotalPageFile`, physical RAM plus the page file as it
is sized now). On a host whose page file is system managed Windows grows the file as the commit charge nears the limit, and the
next reading carries a larger one: the reference box read 187.7 GiB on 2026-10-09 and 10 with the file at 60 to 68 GiB, and 171.7
GiB on 2026-10-17 (read-only, 2026-10-17: the page file is `pagefile.sys` with initial and maximum size 0, the system-managed
form, 45,056 MiB allocated, 6 MiB in use). So for a stated commit the bound is a moving one: it caps the commit at the page file's
current size and does not stop the file growing, and growth was the incident's symptom. **"Other processes' allocations fail past
the limit" is therefore conditional on the page file having reached its maximum**; before that, a charge near the limit makes
Windows grow the file. The rule reads the current limit, the conservative choice: with a system-managed file the configured
maximum is the number 0, there is no figure to read, and a rule that assumed a large maximum would admit what the file has not yet
grown to take. The available figure is not stale: it is read on every attempt and every poll, at most 2 s before the comparison.

**Where a stated commit is judged, and what Linux loses.** Always on Windows. On Linux in strict overcommit mode
(`vm.overcommit_memory` = 2, or a mode that cannot be read, below); in the default heuristic mode (0) and in mode 1 the kernel grants allocations past `CommitLimit`, and
`Committed_AS` counts mappings a process reserved and never touched (CUDA and large `mmap`s do), so refusing a stated commit on it
would turn work away for a limit nothing enforces. The reading says so (`HostMemory.CommitLimitSoft`, read from
`/proc/sys/vm/overcommit_memory` through a seam, `TestTheLinuxReaderMarksTheCommitLimitSoftOnlyInAKnownLaxMode`). **A mode that
cannot be read or parsed is assumed strict**: the limit stays a hard one, failing closed. A `/proc/meminfo` that hides the commit
counters has no limit to enforce and reads soft. So on Linux in modes 0 and 1 a **stated** commit is judged by neither line: it
loses both the commit limit and the physical line, and only the touched condition applies to it
(`TestTheCommitConditionIsAppliedOnlyWhereTheLimitIsHard`). A commit nobody stated is still held to physical RAM less the headroom on every reading,
as it was before the two conditions existed, and to an enforced commit limit less the headroom when that limit sits below physical RAM (strict-mode Linux
with little swap: the limit is about half the RAM plus the swap, and the kernel refuses allocations at it; `TestACommitNobodyStatedIsHeldToAnEnforcedLimitThatSitsBelowPhysicalRAM`) (`TestAnUnstatedCommitIsJudgedWhereTheLimitIsNotEnforced`). Two Linux nodes of the fleet
were read on 2026-10-17: both in mode 0, with `Committed_AS` of 19.5 GiB on a 31.2 GiB `MemTotal` and 3.8 GiB on 30.2 GiB, so there
a stated commit is moot, and one figure declared once is held to `Committed_AS` against physical RAM less the headroom as it was.

**The not-yet-loaded part, per quantity.** What running leases have yet to touch is their declared working set less what the
processes below their holders hold *resident*; what they have yet to be charged is their declared commit less what those
processes hold *privately* (private bytes are the currency of the commit charge: `PrivateUsage` on Windows and, on Linux, the private
writable mappings, `VmData`, which is what `Committed_AS` charges when a mapping is made). Each is never below zero and pooled per holder
(see the bullet below). A declaration is an estimate, so this rule is a brake on declared loads: it does not bound what a lane then
commits, and neither condition sees a process that holds no lease.

* **Everywhere the cards are granted.** A card-scoped grant (`--devices`, `--cards`, the media admission) checks inside
  `grantDevicesLocked`, under the epoch lock, after the cards are shown free and before the epoch is issued; the record that
  carries the declared need is written in the same critical section, so two grants on different cards cannot both read the
  same headroom: the second one's check sees the first one's record
  (`TestConcurrentGrantsOnDifferentCardsCannotBothTakeTheLastHeadroom`: exactly one of four racers fits, 25 rounds).
  **The readings are taken before the lock and only compared inside it** (`readHost`, F57): the host's counters and, for every
  live lease that declared a need, the resident memory below its holder (a walk of the process tree with a memory read per
  process, which the reviewer measured at about 26 ms median and 63 ms worst for one holder, on every grant attempt and every
  waiter poll, inside a lock everyone else spins on for 2 s). The decision is still made under the lock, against the leases
  live then: their declared needs are read from the records there, so a grant that lands between the reading and the lock is
  counted (`TestAGrantThatLandsAfterTheReadingIsStillCountedInsideTheLock`); the early parts err the conservative way (a
  resident figure read a moment early is lower, so more of a declared need is still to load; a holder that appeared after the
  reading counts as having loaded nothing), and a need of 0 reads nothing
  (`TestTheHostReadingsAreTakenBeforeTheEpochLockAndOnlyComparedInside` counts the host and process-tree reads made while the
  lock file exists: none). A
  whole-node grant checks in `TryAcquire` before its epoch is bumped, so a refusal burns none; it needs no lock because a
  whole-node lease conflicts with every other live lease, so nothing else is live to account for. The detached holder
  receives the parent's resolved number as `--ram` and declares exactly that. The card allocator applies the same function
  before it picks cards, advisory (it never refuses what the grant would admit: the media admission passes it the need sized on
  the cards its plan names, a pin, a pool or a declared device, as the lease it then takes declares it, and against the largest
  card on the box when the plan names none, `askRAMFor`, `TestAskRAMIsSizedOnTheCardsAPlanNames`; its third term, what leases
  already granted have yet to load, is `Manager.HostRAMPending` through `gpualloc.BuildInput`, pinned by
  `TestBuildInputCarriesWhatGrantedLeasesHaveYetToLoadAndTheAllocatorUsesIt`); the grant is the authority. **Cards first, memory
  second**, on both grant paths: a held card is reported as held and its waiter is not marked "waiting for host RAM" (that mark
  is what lets a call that declares none pass it), whatever the host reads (`TestAHeldCardIsReportedHeldNotShortOfHostRAM`,
  `TestAWaiterOnAHeldCardIsNotMarkedWaitingForHostRAM`).
* **Asked without taking: `Manager.HostRAMCheck(need)`.** A placement that wants to know whether this node would admit a
  lane before it sends the lane here asks the node's own Manager, which applies the same function the grant calls
  (`hostRAMCheckAgainst`: the host as it reads now, the not-yet-loaded part of the live leases, this Manager's headroom).
  It takes no lease, registers no waiter, spends no epoch and writes nothing; a need of 0 is admitted without reading the
  host. The answer carries the numbers and the refusal sentence, so a queued answer can quote the node's own words. The
  grant stays the authority: a lease can land between the answer and the grant, and the grant re-checks under the epoch
  lock (`TestHostRAMCheckIsTheRuleTheGrantApplies`: the verdict, the sentence and "can waiting cure it" agree with the
  grant's over a table of hosts, needs and running leases, and nothing is written; `TestHostRAMCheckOfNothingReadsNothing`).
* **One headroom, the configured one, on every surface.** `gpu_host_ram_headroom_gib` is installed for the whole process
  by `config.Load` (`gpulease.SetDefaultHostRAMHeadroom`, the way the orphan grace and the term limits are), so the grant, the
  card allocator and the status surfaces read one number whichever constructor built the Manager. It was a per-Manager setter
  that no production path called: the grant kept the built-in 8 GiB while `gpu status` reported the configured key, so an
  operator who raised the headroom to be safer was admitted up to the difference past the limit set, and one who lowered it
  to unblock a lane was still refused (found by the post-implementation review, 2026-10-10;
  `TestLoadInstallsTheHostRAMHeadroomForEveryGrant`, `TestAGrantKeepsTheInstalledHeadroomNotTheBuiltInOne`,
  `TestReserveGrantsAgainstTheConfiguredHeadroom`, `TestTheMediaAdmissionKeepsTheConfiguredHeadroom`).
* **The not-yet-loaded part.** A granted lease's declared working set minus what the processes below its holder hold RESIDENT
  right now (`WorkingSetSize` through `K32GetProcessMemoryInfo` on Windows, `VmRSS` on Linux), and its declared commit minus what
  those processes hold PRIVATELY (`PrivateUsage` on Windows, `VmData` on Linux: the private writable mappings, touched or not), each never below zero. Each against
  its own declaration, because a declaration is in RAM units and committed-but-unoccupied memory (a card allocation charged to the
  process's commit, an untouched reservation) is not RAM that has loaded: a real child that was committed 256 MiB it never
  touched read private 305 MiB and resident 13 MiB on the reference box, 2026-10-10
  (`TestHeldCountsWhatIsResidentNotWhatIsMerelyCommitted`, which runs on Linux too). On Linux the touched part of the mappings
  (`RssAnon` plus `VmSwap`) read 2.1 MiB for such a child while `Committed_AS` moved 333 MiB, so on a strict host the rest would have
  been counted twice, once inside the charge and once as still to be charged; the reader returns `VmData`
  (`TestLinuxHeldCommitCarriesWhatTheKernelAlreadyCharges`). `VmData` leaves out the main thread's stack and shared memory, which the kernel
  charges and it does not, so it can read a little low, which leaves more pending: it can over-refuse and never over-admits. Subtracting the private bytes from the working-set declaration would call a
  lane loaded while most of its RAM was still to come; subtracting the working set from the commit declaration would call a lane
  uncharged while its process already held the charge (`TestWhatARunningLeaseHasYetToBeChargedCountsAgainstTheCommitLimit`).
  Leases that share a holder pid are pooled (the pipeline holds one lease per card in one server process, and all their
  runners are below it), the holder's own memory is not subtracted (a long-lived server's memory is not the job's), and a
  holder whose memory cannot be read counts as holding nothing, so its whole declared need is pending
  (`TestTheNotYetLoadedPartOfAGrantedLeaseCounts`, `TestPendingPoolsLeasesByHolder`). Without this term the second lane
  sees room, and both go: the incident, replayed.
* **A need of 0 is never read against the host.** The lease adds no memory, so a box that is already over is not made to
  wait for it, and the counters are not even read (`TestALeaseThatDeclaresNoNeedIsNeverReadAgainstTheHost`).
* **A shortage waits in the same line.** The request stays a registered waiter, in the same FIFO, in the same place,
  its record marked `waiting_for: host-ram` and carrying what it declared (`gpu status` shows both). A request behind it
  for the same cards stays behind it when that request declares host RAM too (its memory competes with the waiter's, and
  the refusal it gets names what the waiter in front is waiting for: `which is waiting for host RAM (needs 30.0 GiB) and
  has not claimed the card`). A request that declares **nothing** passes it (G6 of the P0 plan,
  `Waiter.BlocksArrival`, `TestARequestThatDeclaresNoHostRAMPassesAWaiterThatWaitsOnlyOnMemory`): it adds none of the
  memory the waiter is short of, so it cannot make the shortage worse, and a waiter that sat first for its whole `--wait`
  (eight hours by default) used to stop a 0 GiB bench on an idle card, or, asking for the whole node, every request on the
  box. The allocator reads the same exception (`gpualloc.QueuedClaims(..., declaresHostRAM)`), so a call that passes is not
  steered off an idle card and a call that cannot wait is not told the card is promised to somebody. The cost is stated:
  the request that passes takes the card the waiter wanted, so when memory recovers the waiter waits for that card. That
  delay is bounded, not a stream, because the moment the waiter finds its card held it stops being a waiter on memory
  (its `waiting_for` clears on `ErrHeld`) and is an ordinary front waiter nothing passes
  (`TestThePassedWaiterIsServedRightAfterThePasserAndNothingElsePassesItMeanwhile`): at most the lease of whoever passed.
  **Named limits.** Nothing reserves the waiter's memory against a declaring request on *disjoint* cards (disjoint backfill
  was always allowed), and a place held for a caller who left (a token) still holds against everyone for its 30 s grace,
  unless it was last marked as waiting only on host RAM (below).
  **A place token follows the same rule.** A token carries `waiting_for` too: the media ticket that keeps it marks it
  `host-ram` on every scan whose lane reading found nothing in the way but host RAM (no lease on the cards, nobody ahead;
  `core.PlaceMarker`, `Manager.MarkToken`), and clears the mark on a scan that found anything else. A marked token does not hold
  its cards against a request that declares no host RAM, in the waiter line, in the timeout's "who is ahead", in
  `gpualloc.QueuedClaims` and in the lane reading (`Token.HoldsItsCardsAgainst`,
  `TestAPlaceWaitingOnHostRAMDoesNotHoldBackARequestDeclaringNone`). Before 0.196.0 it did: on 2026-10-20 two image tickets
  short of 46.8 GiB of host RAM kept their places for three hours, and a 0 GiB two-card render behind them waited 36 minutes
  on idle cards.
  The line `waiting for host RAM: needs 33.4 GiB, only 31.0 GiB of 127.7 GiB physical is available
  (+12.0 GiB still to load by leases already running), 8.0 GiB headroom` (or the commit sentence, above) is printed once per request,
  never per poll. `--wait 0` refuses with that text, the flag that waits, and `--ram` as the way out (`--ram-commit` too when a stated
  commit is what refused); the media admission answers the same
  words as a queued place in line (resumable doors) or a busy defer (the rest). That holds for a call with no time left
  (`gpu_wait_ms` 0, or a window spent) on every plan shape: a pinned card, an allocated card and the whole node all answer
  the guard's sentence, never the line's "promised to callers ahead of this one" (the cards are free and nobody is ahead;
  `TestWaitZeroKeepsTheGuardsSentenceOnAPinnedPlan` and its siblings, G2 of the P0 plan).
* **A need no state of the box admits is refused at once.** If the touched need exceeds physical RAM less the headroom, or the
  commit exceeds the commit limit less the headroom (where it is enforced), waiting cannot help: the request ends with the text (it
  names `--ram` or `--ram-commit`, and `gpu_host_ram_headroom_gib`), and the pipeline classes it
  `gpu_lease_unavailable` (a configuration fault), not `gpu_busy`. `--ram 0` is the escape hatch the operator owns.
* **Unreadable memory.** On a platform that has a reader (Windows, Linux) a lease that declares a need waits until the host
  can be read, the way the other guards fail closed. On a platform with no reader the rule cannot judge and admits.

**Known limits, stated so they are not discovered.** **An admitted lane can still leave the box close to a line.** The rule applied
to a commit reading of 81.6 GiB (the reference box between jobs, 2026-10-10 about 04:12 UTC; 83 GiB available, 127.7 GiB physical,
commit limit 187.7 GiB, headroom 8) admits a 32.8 GiB declaration, the Krea 2 bf16 files: 114.4 GiB of projected commit against a
bound of 119.7 (physical RAM less the headroom, for the one figure; 179.7 against the commit limit for a stated commit), 50.2 GiB
left available, so both conditions pass (`gpuprobe.HostRAMAdmitsLoad` on those numbers, pinned by
`TestTheQuotedCounterexampleIsAdmittedOnBothConditions`). **Provenance:** the readings in this paragraph (81.6 GiB committed with
83 GiB available; 57.7 GiB private bytes with a 40.7 GiB working set; 129.3 GiB committed with 49.1 GiB available and 2,298 MiB of
page file in use) were measured on the reference box on 2026-10-10, from session readings that are not recorded in this
repository; they were not re-measured for this document. At 06:01 UTC the box read 129.5 GiB committed with
one ComfyUI lane running, a process six minutes after launch holding 57.7 GiB private bytes (working set 40.7 GiB); at 06:32 UTC
a second process, launched fresh seven minutes earlier, held the same 57.7 GiB, and the box read 129.3 GiB committed and 49.1 GiB
available with the page file unchanged (2,298 MiB in use): 1.6 GiB above physical RAM and 9.6 GiB past the guard's own line. The
family those lanes ran is not recorded in the readings, and the 81.6 and 129.3 GiB readings are separate observations two hours
apart, so the 47.7 GiB between them is the lane and whatever else changed, not the lane alone. What they establish is narrower: a
lane that holds 57.7 GiB private (24.9 GiB more than a 32.8 GiB declaration), added to a baseline the rule admits, reads 129.3 GiB
committed: 58 GiB under the commit limit and with 49.1 GiB available, so a STATED commit's lines are not reached (that reading is above
physical RAM, which is the line a commit nobody stated is held to, and `OVER` says so). Whether a Krea 2 lane runs at that footprint is the S1 acceptance data. The exposure is (a) the first three runs of every family/quant/task key on a node, which
declare the file sizes, (b) any lane whose private bytes exceed its declared resident need, and (c) any process that holds no
lease (the watcher below saw commit swing 95.3 to 132.1 GiB, more than four times the 8 GiB headroom). What the guard does about it: a
lane already running is counted in the commit reading and gone from the available figure, so the next lease waits when its
declaration would pass its line (at the 129.3 GiB reading with 49.1 GiB available, any figure declared once is refused on the commit
charge, which is already above physical RAM; a 41.2 GiB commit STATED is refused on the available memory: 49.1 - 41.2 = 7.9 against
a headroom of 8, and a stated 41.0 is not, `TestTheDocumentedReadingsAgreeWithTheRule`), and `NEAR` and `OVER` reach `gpu status`
and `offload_status`. What it does not do: stop the lane that took the box over. The lasting fix is to declare the commit from
measurement: the footprint store already records the private peak of a lane's process tree beside its resident peak
(`TreeMemory`), so a declaration could state its commit from three measured runs instead of defaulting to the touched figure; that
is not done here, and until it is the commit defaults to the touched figure, held to physical RAM, unless the caller states one (see
"How the numbers get measured"). On Linux a stated commit is not judged unless the kernel is in strict overcommit mode (see "The
admission rule"). A `/proc/meminfo` that omits the commit counters (a sandbox that virtualises it) falls back to what the box visibly uses
(`MemTotal - MemAvailable`, limit `MemTotal`), marked soft, which keeps the rule working on the available memory alone: no reading
would make every declaring lease wait forever. A
detached lease (`--detach`) has no command of its own below its holder, so its whole declared need counts as still to
load for as long as it is held (a budget reservation). A kept ComfyUI whose runner has exited is reparented away from the
holder's tree, so its loaded weights count both as pending and in the commit charge until that lease ends: that
over-refuses, it never under-refuses. A pipeline job running under its parent's ambient lease (`acquireInherited`,
`GPU_LEASE_DEVICES`) gets no admission of its own: the parent's declaration covers it. A process outside every lease can
still push the box over; the guard sees it in the commit charge and holds new declaring leases back, but it does not stop or
evict anything. **Reach (G7 of the P0 plan).** The guard checks the declared loads of the leases that pass through it. A binary older than the guard
(a pinned copy: its leases declare nothing, so the not-yet-loaded sum undercounts them), a direct llama-swap request (the seat
loads on demand with no lease at all) and a hand-started ComfyUI instance are outside it: it sees what they have committed, never
what they are about to. The closure is to run the live harness everywhere and to make a ComfyUI start refuse without a lease
token; until then the rule rests on those callers going through the lease, and nothing here claims it checks a process that does not.

### How the numbers get measured (G3 of the P0 plan)

Every figure this rule uses is the harness's: chosen or measured, and labelled as which. None is presented as a number anyone else set.

* **The headroom (8 GiB) is chosen, not measured.** It is a floor: the incident box's non-media baseline was about 56 GiB of
  desktop apps, agent CLIs, browsers, WSL and kernel pools and it moves by several GiB inside a minute. What is still owed is
  each node's interactive working-set swing, measured against its own use, recorded here as `measured <date> <node> n=<k>`;
  until a row exists below, 8 is the starting value and `gpu_host_ram_headroom_gib` is the override. Data points so far (these
  describe how the counters move, they do not derive a headroom): on the reference 3-card box, 2026-10-10, a watcher that
  logged only when the verdict changed (11 lines) saw commit range from 95.3 to 132.1 GiB across a stretch of other sessions'
  work, with one fall of 32.0 GiB (127.3 to 95.3) inside 16 seconds, while available RAM stayed between 47.6 and 77.7 GiB and
  the page file in use between 2494 and 2665 MiB (it did not grow); a read-only sample of 60 readings at 3 s intervals in a
  quiet interval (04:41 to 04:45 local) read commit 100.09 to 100.43 GiB and available 72.54 to 72.71 GiB.

  | node | measured | n | working-set swing | headroom adopted |
  |---|---|---|---|---|
  | reference 3-card box | 2026-10-11 | 30 | **lane-run swing, not interactive-use swing.** Per run, over the lane's own samples (every 0.5 s; 89 to 2,272 per run): system commit less the lane process tree's private bytes moved a median of 9.9 GiB (range 0.5 to 44.9) and available RAM plus the tree's working set a median of 5.4 GiB (0.3 to 35.7). The raw system counters, which include the lane's own load, moved commit a median of 27.9 GiB (7.1 to 69.3) and available RAM 16.4 GiB (2.3 to 36.2) | 8 (chosen) |

  The row is the 30 lane timelines (`timeline.csv`, one per Wan 2.x multi-card run) of the 2026-10-11 acceptance work on that box,
  not a run made for this table. The sampler read the system commit and available counters at the same instants as the lane's
  process tree (private bytes, working set), so the swing is that of a box with a lane running; other tenants' jobs are mixed in
  (one run read 154 GiB committed while the lane's own private peak was 26.4 GiB), as are the driver and the display. It is
  therefore an upper bound on what a lane-free interactive session moves and says nothing yet about interactive use. The
  largest per-run swings (44.9 and 35.7 GiB) exceed the 8 GiB headroom; the headroom stays the chosen floor, since the rule
  already counts a running lane in the commit and available readings. The 2026-10-14 before/after snapshot pairs (4 runs) are not
  folded in: they show one vLLM seat's load, not a swing. Rows for the other nodes are still owed.

* **What each render held is recorded, not guessed.** Every sampled GPU render records, beside its VRAM peak, the peak
  PRIVATE and the peak RESIDENT memory of its process tree (`gpulease.TreeMemory`, sampled by gpugen on the same tick) into
  the footprint store as `host_peak_gb` and `host_resident_peak_gb` with the number of runs (`host_runs`); both ride
  `/fleet/health` `model_footprints[]` additively. Two quantities on purpose: they differ exactly where the VRAM charge
  below bites. The media admission reads them back (`Pipeline.calibratedRAM`) and **raises** a declaration to the measured
  resident figure once at least three runs exist (`hostPeakMinRuns`, chosen), for the image and video routes. It only
  raises: the sampled tree can miss memory (a ComfyUI instance kept from an earlier lease and reused is not under the
  runner that is sampled), and a render whose weights fit its card still declares 0. **The figure is not the all-time maximum**
  (F56: one inflated run raised a family's declaration for good, until footprints.json was edited by hand): the store keeps the
  last twenty runs' host peaks of each key (`hostRecentRuns`, `host_recent` in the file) and the calibration reads their 90th
  percentile by nearest rank (`hostPeakPercentile`), so one outlier among ten or more runs is not the answer and a bad run
  leaves the window after twenty more. Under ten runs the 90th percentile of the window is its maximum, so outlier rejection
  starts at ten runs and below that the calibration errs toward declaring more. A store written before the window has the
  maxima and a run count: its maxima stand in, once, for the runs the window does not hold, until the window fills. **The raise
  is clamped** at twice the sum of the model files (`hostPeakMaxRaise`): a tree that reads more than twice its files is an
  estimate that is wrong or a sampler that counts card memory, not a need to declare, and an unclamped figure above what the
  box can ever admit made the lane read "can never admit" for every later call with `gpu reserve --ram` no remedy for a
  pipeline call. The declaration says it was clamped, and says which percentile of how many runs it read; **the note is made
  once per key and figure**, not per render (it used to add a map entry and a log line for every render of a fleet-serve's
  life). Two processes share the file: a process merges what the other wrote before it folds its own run in, and the record with more runs
  supplies the window (a race between two writers can still lose one run's sample). The window (20), the percentile (90) and the clamp (2x) are chosen, not measured; the wire fields `host_peak_gb` and
  `host_resident_peak_gb` stay the all-time maxima, a peak being a peak. Edit, inpaint, animate and music keep the estimate
  from their model files until their sampling keys carry a family.

* **Does a GPU process's allocation count toward its private bytes and the system commit? Yes on the reference driver (measured
  2026-10-11); the brake is unchanged, and the limits are below.**
  *The measurement.* On the reference three-card box (127.7 GiB physical, commit limit 187.7 GiB, Windows driver model), a
  4.0 GiB `cudaMalloc` on a card raised the process's private bytes by 4.0 GiB (and system commit by 4.1 GiB), a 2.0 GiB one on
  the display card by 2.0 GiB, and both fell back when the memory was freed: **every VRAM allocation is charged 1:1 to the
  process's private bytes and to system commit**, on top of a CUDA context of about 2 GiB. A card-only Wan run therefore showed
  private bytes 17 GiB over its baseline and system commit moving 82, 113, 83 GiB (the VRAM charge 23.8 GiB, an idle ComfyUI
  3.2 GiB, host growth 2.3 GiB, the rest other tenants) while the host's available RAM never fell under 45 GiB in 33 recorded
  runs. The stock ComfyUI loader (dynamic VRAM off) maps every safetensors file copy-on-write and Windows charges the whole
  file to commit at map time (14.73 GiB on one Wan expert); a read-only loader adds nothing. This replaces the earlier
  one-process observation (2026-10-10), which was consistent with it and could not settle it.
  *What it means for the brake.* System commit sits tens of GiB above what the host really uses, because card-resident weights
  are charged to it, so commit says nothing about paging by itself. The brake compares a commit nobody stated to physical RAM (kept
  on 2026-10-11 while this was measured, and the line every caller that declares one figure keeps); it judges what a lane touches
  against the available RAM, which is the paging signal, and a commit a caller states against the OS commit limit, which is what
  other processes' allocations fail once the page file has reached its maximum. What keeps
  card-only work moving is the declaration: a recipe whose weights all sit on the cards (card-only placement, the read-only
  loader) declares **`--ram 0`**, and a need of 0 is always admitted. A recipe that streams weights from RAM declares them in
  full; a recipe whose commit exceeds what it touches (the card allocations above are charged 1:1) states both,
  `--ram <touched> --ram-commit <charged>`.
  *Limits.* **One driver on one box:** the 1:1 charge was measured on the reference box's Windows driver and not on any other node
  or driver version, so another host's commit says nothing until it is measured there. **Commit counters are system-wide:** other
  tenants move them by tens of GiB (133 GiB before one run), so only a process tree's own private bytes are attributable to a
  run. **A declaration of 0 is a statement about the recipe, not a measurement of the run:** the figures above are growth of
  1.3 to 3.7 GiB for a card-only run, inside the headroom the brake keeps anyway, and a card-only recipe that loses its
  read-only loader declares the file sizes it would then add to commit. Whether a lane's VRAM moves the commit counter at the
  moment it starts, on a node other than the reference box, is not measured.

* **Pages-in per second is not a paging signal on this box.** `\Memory\Pages Input/sec` read 0 to 1168 pages/s in the quiet
  sample with the page file flat, because it counts every file-backed read as well. Say "paging" only from page-file growth or
  the pages-output rate.

* **To record the data points (read-only, the S1 acceptance runs produce them for free).** Around one lane at a time, on each
  node, sample every 3 s and keep the rows: `\Memory\Committed Bytes`, `\Memory\Available Bytes`,
  `\Paging File(_Total)\% Usage`, `\Memory\Pages Output/sec`, and `\GPU Process Memory(*)\Dedicated Usage`,
  `Shared Usage`, `Total Committed` next to each GPU process's private bytes and working set. The footprint store's
  `host_peak_gb` / `host_resident_peak_gb` / `vram_peak_gb` of the same runs are the other half. Keep commit as a term only if
  it tracks the working set across a lane start; stop the run at NEAR.

### The agent seat and its warm-back (G4 of the P0 plan)

A lease that takes a card unloads the agent seat, and the wrapper warms it back before it releases the lease. That reload
is a load like any other, and it used to be the one load nothing sized: the warm path had no host-RAM check, and the
seat's footprint was declared nowhere. (The 2026-09-10 incident was a seat: 44 GB of host RAM nobody counted.) Now:

* **The seat declares its footprint.** `agent_seat_host_ram_gib` is the host RAM the seat holds once loaded (resident set
  plus any staged KV cache). The node that runs the seat measures it and records `measured <date> <node>` beside the value.
  Unset is not "small": `hostneed.DefaultSeatHostGiB` (21 GiB: a vLLM pair seat's ~13 GiB of process plus 8 GiB of staged KV
  cache, from the project notes of 2026-09-10, not re-measured here) stands in, a **chosen** fail-closed figure, never 0
  (`TestSeatNeedIsTheConfiguredFigureElseTheFailClosedDefault`). It is **not** the largest figure on record: the 2026-09-10
  incident recorded 44 GB of host RAM for a seat, the note that carries it attributes that to LMCache staging plus page cache,
  and the page-cache part is droppable (measured 2026-10-18 on a WSL2 pair seat: loading the INT4 27B left 23 GiB of guest page
  cache, which on WSL2 is host commit; one drop took the VM 68.7 -> 46.8 GiB and host commit 149.7 -> 128.0 GiB with the seat
  serving). On WSL2 the launcher now drops it itself (`SEAT_CACHE_CAP_GIB`, below). So with the key unset the warm-back is
  admitted against a figure below the incident's, and a node that runs a seat sets `agent_seat_host_ram_gib` from its own
  measurement before it relies on the check. Two readers help the node do that (register B-56 / H-08). The vLLM seat launcher
  (`seat_fg.sh`) logs `host rss N GiB (L1 N GiB)` to the seat log at every start: the resident set of the LMCache MP server unit
  and its children, i.e. the pinned L1 staging buffer plus about 1.1 GiB, read before the launcher becomes `vllm serve`, so the
  engine's own resident set (and the VM that carries both) is added by measuring the loaded seat. And `doctor` prints a
  `seat ram:` row on a box that runs a vLLM seat (`vllm_seats`, or the tier table's vLLM seat as `agent_model`): WARN while
  the key is unset, OK once it is declared. The row does not compare the declaration with a live reading, because the seat's
  memory is read inside its own distro and the harness holds no cheap read of it; it never changes doctor's exit code.
* **The warm passes the grant's admission.** The seat's footprint, taken for both quantities and with no commit stated, plus
  what the *other* live leases have yet to load, must leave the headroom of the available RAM and keep the commit charge under
  physical RAM less the headroom: the warm-back states no commit, so it is held to physical RAM and not to the commit limit
  (`TestAWarmBackIsHeldToPhysicalRAMAndNotToTheCommitLimit`). It is `Manager.HostRAMCheckWithout`, the same function
  as the grant, with the lease being released left out, because its command has exited and it loads nothing more. A host
  that reads NEAR or OVER therefore never warms (`TestAWarmBackIsRefusedWhenTheHostCannotTakeTheSeatAndStaysOwed`,
  `TestAWarmBackDoesNotCountTheLeaseItIsReleasing`).
* **Never while a lane streams.** Any other live lease that declared host RAM refuses the warm outright, whatever the
  numbers say: the lane's own growth is in no counter yet (`TestAWarmBackNeverRunsWhileALaneThatDeclaredHostRAMIsLive`).
* **A refused warm stays owed.** The marker is left for the last holder, exactly as for the other refusals, and the seat
  loads on its next request. That request is a llama-swap load the lease system does not gate (see the reach note below):
  refusing the warm keeps the harness from *adding* a load to a tight host, it does not stop a delegator that asks for the
  seat.
* **Not done, on purpose.** The owed warm is not counted as *pending* when another lease is admitted. The seat was unloaded to
  make room for that very lease, and its reload happens after the lease ends, so reserving room for it at the lease's grant
  would count the memory twice; the reload is admitted when it is attempted.

### A lease that unloads the seat is compared to the host after the unload (F82)

**The deadlock.** `gpu reserve --ram N --drain --unload-seat` compared its `N` to the host inside `Acquire`, and drained and
unloaded the seat only after the lease was held (`detachMaintain`, `maintainSeatScoped`). When the memory it needed was the
memory the seat held, the grant waited for commit that its own unload would have freed, and the unload ran only after the
grant: a film lease that declared its host RAM waited about six hours while the 3-card vLLM seat held 67 GiB of the host's commit.
It is the class of the DRAINING stamp ([The drain waits for runs](#the-drain-waits-for-runs-inside-the-queue-budget-0117-0-adr-0041),
`gpu_drain.go`): a gate in front of the step that clears it. The brake must never block the unload that clears it.

**The order now**, for a lease that asks for `--unload-seat`, declares host RAM, whose unload can free memory for it and whose host
can be read (`gpu_hostram_await.go`):

0. **Whether the order applies is decided before the grant, from facts that do not change while the lease queues.** The unload must
   reach a model on the cards the lease holds (`unloadReachesLease`): the agent seat placed on them, or a model the roster lists that
   is placed on them or placed nowhere, with the unload's own resolution of doubt (a card table or a roster that cannot be read, a
   model nothing places and a lease that names no card all count as reaching). A lease whose every listed model is positively placed
   on other cards frees nothing for itself, so it keeps the comparison in the grant, queues without holding its cards, and the
   reserve says so (`TestACardLeaseWhoseUnloadReachesNothingIsComparedToTheHostInTheGrant`; the other half, a resident on the leased
   card that is not the agent seat, keeps the order: `TestACardLeaseWhoseUnloadReachesAnotherResidentStillDefers`). Residency is
   deliberately not asked: a seat that is cold when the reserve starts may be loaded when the grant comes. And the host must be
   readable: a host whose memory cannot be read cannot show that the unload makes room, and the look after the unload would refuse for
   the same reason until its deadline with the seat cycled, so that lease keeps the grant's comparison too
   (`TestAnUnreadableHostKeepsTheGrantsComparisonForALeaseThatUnloadsTheSeat`).
1. **A need no state of the box could ever admit is refused at once**, before anything is taken or unloaded (`screenDeferredHostRAM`,
   `Manager.HostRAMCheckLoad`, with both figures): waiting cannot help, and a seat cycle for a lease that can never run is bought for nothing.
2. **The grant takes the place in line and the cards and compares nothing.** `Options.HostRAMGiB` is 0 for it and the figure
   travels as `Options.HostRAMWaitGiB`, stamped on the record as `host_ram_wait_gib` (additive and omitted when 0: an older reader
   ignores it and reads a lease that declares nothing yet). The card allocator's host term is off for the same reason
   (`reserveDeviceFlags.ramGiB` is the gate figure). The lease is stamped DRAINING, so the cards are fenced and no seat reloads
   onto them.
3. **The drain and the unload run, as they always did**, under the lease and its heartbeat.
4. **Then the host is asked** (`Manager.AwaitHostRAM`), still holding the cards, with the grant's own words (`waiting for host
   RAM: needs ...`, printed once) and inside the same `--wait`: the rest of the queue budget measured from when the reserve began
   queueing, never under the drain floor (`hostRAMDeadline`) **and inside the hold bound** (`gpu_hostram_hold_max_sec`, default
   600 s, counted from now, the moment the unload is done; [below](#the-hold-bound-gpu_hostram_hold_max_sec-the-incident-of-2026-10-14)).
   The check and the stamp that turns `host_ram_wait_gib` into
   `host_ram_gib` are **one critical section under the epoch lock**, so two leases cannot both take the last headroom
   (`TestConcurrentAwaitsCannotBothTakeTheLastHeadroom`).
5. **The command starts.**

**It is a reorder, not a looser check.** `AwaitHostRAM` puts the figure to the same function the grant does
(`gpuprobe.HostRAMAdmitsLoad` through `hostRAMCheckWith`): the same host reading, the same headroom, the not-yet-loaded part of every
lease that has been admitted, the same sentence. Over a table of hosts, needs and running leases it admits and refuses exactly
where the grant does (`TestTheAwaitAdmitsExactlyWhereTheGrantDoes`). If the unload frees too little the lease keeps its cards,
keeps waiting inside `--wait` and the hold bound, and ends with the host-RAM refusal (when `--wait` ends it) or the hold bound's
failure (when the bound does; the lease is then released); the command never runs, the seat was unloaded, so its warm-back
stays owed and is itself refused by the same rule (`TestAnUnloadSeatLeaseStillWaitsWhenTheUnloadDoesNotFreeEnough`). A lease
**without** `--unload-seat` behaves exactly as before, `--drain` alone included: a drain frees no host memory, so cordoning the
seat for a wait it cannot shorten would only cost the seat's callers (`TestALeaseWithoutUnloadSeatKeepsTheOldOrder`).

**Each form.**

| form | where the order is made | why it cannot deadlock |
|---|---|---|
| wrapper (`gpu reserve ... -- <cmd>`) | `runGPUReserve`: `maintainSeatReport`, then `awaitHostRAMBounded` (the wait under the hold bound) under the same heartbeat and the same cancel as the drain | the grant compares nothing; the wait runs after the unload |
| `--detach` | the parent: `detachMaintainReport`, then `awaitHostRAMBounded` against the held epoch (`Manager.AwaitHostRAM`). The hidden holder is started with `--ram 0 --ram-wait N` (`holdArgs`) | the holder compares nothing in its grant (`TestTheDetachedHolderOfAnUnloadSeatLeaseTakesItsCardsAndComparesNothing`); a wait that ends without the host's admission, **whatever ended it** (the hold bound, `--wait`, an impossible figure), **releases the lease** (`releaseAfterRAMWait`: what `gpu release --warm-seat --epoch N` does, except that the seat is warmed back only if the lease owes it, `autoReleaseWarmGuard`) and fails the parent non-zero with what ended the wait (`TestTheHoldBoundReleasesADetachedHoldToo`, `TestADetachedWaitThatWaitEndsAlsoReleasesTheLease`); and if the parent dies first, **the hidden holder releases the lease itself** from the end of the wait the record carries (`releaseHolderAtWaitEnd`), or, if it died during the drain or the unload, from the outer bound the parent stamped as soon as the holder had the lease ([below](#the-hold-bound-gpu_hostram_hold_max_sec-the-incident-of-2026-10-14)) |
| `gpu hold` (the hidden holder verb itself) | none: it has no drain and no unload of its own, only what the parent hands it | it declares exactly what it is given. `--ram N` is compared in its grant, as before; nothing frees host memory for it, so there is no unload to wait behind. The `--detach` parent passes the figure as `--ram-wait` instead |
| `--devices` and `--cards` | the same two code paths; `--cards` allocates with a host term of 0 and the allocator's queue treats the request as one that declares nothing. `--devices` knows its cards before the grant and applies step 0 to them. `--cards` chooses its cards after the grant, so step 0 cannot be asked beforehand and it always defers: an allocation that lands on cards the unload does not reach holds them while the host is short, and the wait line (it says the drain and the unload are done, not that the seat was cleared) is true for it | `TestACardLeaseWithUnloadSeatIsNotHeldByTheHostRAMItsOwnUnloadFrees`, `TestACardsRequestWithUnloadSeatIsNotHeldByTheHostRAMItsOwnUnloadFrees` |

A signal during the wait ends it at once (`stageHostRAM`, "while waiting for host RAM"): the wrapper is released, and the warm
stays owed with the same sentence as a signal during the drain. A lease lost during the wait is taken again on the same cards and
the drain starts over under the new lease, as for a lease lost during the drain.

#### A render lease follows the same order (the media path, the incident of 2026-10-24)

**What happened.** On a 6 GB node a 4B llama-swap seat held 8 GiB of commit. An sd.cpp render declared 6.5 GiB and was told
`waiting for host RAM: needs 6.5 GiB, committed 18.4 of 31.2 GiB physical, 8.0 GiB headroom`; it never ran. Every media lease
(image, video, audio, through `acquireMediaLease`) was compared to the host **inside `Acquire`**, and the llama-swap models its own
runner unloads were unloaded only afterwards, by the node runner (`render/gpu-lock.mjs`: `claimUnload`, `freeLlamaSwap`), so the
admission counted memory the render was about to free as held. It is the F82 defect on the other door.

**The order now** (`internal/pipeline/mediahostram.go`; the CLI path above is untouched, and so are the runner and `internal/gpulease`):

1. **Whether the render defers is decided before the grant** (`hostRAMDeferral`), and the grant and the lane probe ask the same
   function. It defers only when the render declares host RAM, a llama-swap endpoint is configured, the host is short for the figure
   now (not impossibly: a need no state of the box admits is refused by the grant at once, before anything is taken or unloaded; and
   not unreadably: a host whose memory cannot be read keeps the grant's comparison), and at least one model of the render's own
   unload set is **resident** in llama-swap's `/running`. The unload set is the card-scoped `GPU_LEASE_UNLOAD_MODELS` list, or, for the
   whole node (and a list that could not be made), every resident model off the memory stack: the runner's rule. Unlike the CLI,
   residency IS asked, because the wait here is seconds and the media path must not hold every card for a shortage the clear cannot
   cure; when `/running` cannot be read the rule of doubt defers, unless the list is the none sentinel, and a configuration without
   an endpoint never defers. Everything else keeps the old order exactly (the grant compares).
2. **The grant takes the cards declaring nothing**: `Options.HostRAMGiB` is 0 and the figure travels as `Options.HostRAMWaitGiB`
   (`host_ram_wait_gib` on the record). The card allocator's pre-filter and the queue's pass rule see a call that declares nothing
   (`askRAMGate`); the place in line a deferred call leaves keeps the DECLARED figure.
3. **The pipeline unloads, under the lease and its heartbeat** (`clearForHostRAM`): each resident model of the unload set, through
   llama-swap's per-model unload with a drain (30 s per model). A failure is logged and never fatal. No `unloaded.<epoch>` marker is
   written and nothing is owed back: the runner's own unload still runs afterwards as the backstop.
4. **Then the host is asked** (`Lease.AwaitHostRAM`), still holding the cards, with the same rule and the same words as the grant.
   The wait ends at `min(max(start + gpu_wait_ms, clear end + 30 s), clear end + gpu_hostram_hold_max_sec)` (`hostRAMWaitEnd`;
   `0` = no hold bound). The 30 s floor (`mediaHostRAMFloor`) exists because memory freed by a llama-swap unload returns when the
   process exits, in seconds, so a caller that asked to fail fast still gives it that long; a miss holds the cards for the floor and
   no longer. Past the end the lease is released first and the call is refused exactly as before: a whole-node call with the busy
   answer (`gpu busy: waiting for host RAM: ...`), a card call with the host-RAM place in line (`gpu queued`), the same words and
   tokens.
5. **The lane probe follows the same figure** (`MediaLaneFree`, `wholeNode`, `hostRAMFor`, the allocator input), reading roster and
   residency through a 5 s cache because the probe runs on every scan tick, so a ticket the grant would admit is not reported as held
   by host RAM (`ram`).

#### The hold bound (`gpu_hostram_hold_max_sec`, the incident of 2026-10-14)

**What happened.** On the reference box a `gpu reserve --wait 8h --drain --unload-seat --class media --for 40m -- <a command that is
not a recognised render helper>` with no `--ram` declared the media class default (46.8 GiB, the largest render family). The order
above took the cards (exclusive), drained, unloaded the seat and only then asked the host, still holding the cards, for the whole
`--wait`. The seat was not even loaded (the unload freed nothing), committed RAM was 84.6 of 127.7 GiB with 8 GiB of headroom (the rule of that day compared it to physical RAM, as it does for a commit nobody stated), so the
figure could not be met, and the lease held all three cards idle for about 50 minutes while a production lease queued behind it,
renewing its window with no work running. The order is not the defect (it breaks a real deadlock and stays); the defect is that
nothing bounded the time the cards could be held after it.

**The rule.** The wait after the unload ends at the earlier of the `--wait` deadline and the hold bound:

| input | meaning |
|---|---|
| config `gpu_hostram_hold_max_sec` | seconds; unset = 600 (chosen, not measured); `0` = no bound, the old wait inside `--wait`; a negative value or one above the ceiling of 7 days (604800 s) is refused by name (and a verb that goes on with the Config anyway reads a negative value as 600 and a huge one as the ceiling, never as no bound) |
| `gpu reserve --ram-hold-max <duration>` | overrides the key for that reserve; `0` is a value (no bound), a negative duration or one above 7 days is refused before anything is acquired. The hidden `gpu hold` verb has no wait of its own (the `--detach` parent runs it), so it takes no such flag |

The ceiling is a chosen figure: 7 days is above the longest lease life granted by default (`gpu_max_total_min`, 48 h) and the longest
`--wait` anyone uses (8 h by default), so no honest bound is refused, and a value that large that is not 0 is a typo. It is also what
keeps the seconds-to-`Duration` conversion from wrapping: past about 9.2e9 seconds `time.Duration(sec) * time.Second` goes negative or to
zero, which the reserve would read as "no bound" (`TestAHugeHoldBoundIsRefusedByNameAndNeverFailsOpen`).

The bound is counted from the moment the unload is done, not from the grant, and it is not raised to the 2 minute drain floor (the
floor exists for the memory the unload frees to come back; a bound the operator set is an answer to that). When it passes without the
host's admission:

1. **The lease is released.** Wrapper form: the deferred release runs as for any end of a reserve, so the kept ComfyUI instances
   stop, the seat the unload took down is warmed back under the usual guards (only while the card is still ours and nobody is
   queued behind), and the lease goes. `--detach` form: `releaseAfterRAMWait` does what `gpu release --warm-seat --epoch N` does,
   because a detached hold is the form that can sit on the cards longest (a hidden holder renews its term while its owner vouches
   for it); its warm-back is attempted only if the lease owes it (see "Releasing on its own" below).
2. **Nothing runs and nothing re-queues.** The command never starts; the bound's failure is not a lost lease, so the requeue branch
   of the drain loop is never taken.
3. **The reserve exits non-zero with one line** (`errHostRAMHoldBound`): the hold bound that passed; how long the cards were held
   and how much of it was the wait after the unload; the host's own refusal, which carries the need, the figures of the condition that refused and the
   headroom (for a commit nobody stated the wording is still `waiting for host RAM: needs 46.8 GiB, committed 84.6 of 127.7 GiB physical, 8.0 GiB headroom`; the sentence names the condition that refused); that nothing ran
   under it and the lease **is being released** (the text is built before the release runs and claims no more; see "Releasing on its
   own" below for the outcome); what to do (a figure that is only the media class default: pass `--ram <GiB>` with the real figure, `--ram 0` when
   the weights sit on the cards; any other figure: a smaller `--ram`, or wait for the host to free memory); and the lever that
   holds longer. It is deliberately not the `--wait` hint ("pass a longer --wait"): `--wait` did not end this wait.

A wait that `--wait` ends first (a short `--wait`, or no bound) fails with the host-RAM refusal and the `--wait` hint, as it always
did, and **releases the lease too, in both forms**: the wrapper's deferred release always did, and `--detach` used to keep the lease
held, idle and non-exclusive, indefinitely, which is the 2026-10-14 incident from a smaller `--wait`. Cards are never held idle
behind a RAM figure the host cannot meet; the failure names what ended the wait and says the lease is being released.

#### Releasing on its own: the hidden holder, the warm-back, and the outcome

**An outer bound before the drain, kept moving by the live reserve.** A detached reserve killed during the drain or the unload,
before the wait begins, used to leave the lease asking for the host with no stamp, and the holder acts only on a stamp. The first
version stamped the drain's deadline plus the hold bound, and the drain's deadline is `--wait` away (8 h by default): a default
detached reserve killed a minute into its drain, the incident's own command shape, left the holder holding the cards for hours. Now,
as soon as the holder has the lease, a reserve whose lease defers its host RAM stamps **now + the hold
bound + a slack (`outerSlack`, 60 s)** and **re-stamps it every `outerRefreshEvery` (30 s)** while the drain and the unload run (the
unload is under the same ticker); the wait replaces it with its real end when it begins, and admission clears it as before. A reserve
that dies stops re-stamping, so the holder releases the lease within about the bound plus the slack plus its own grace, whatever
`--wait` says, and does not touch a lease whose reserve is alive and still draining
(`TestADetachedReserveWhoseParentDiesDuringALongWaitDrainIsReleasedWithinTheBoundNotAtWait`,
`TestADetachedReserveStampsAnOuterBoundBeforeTheDrainSoAParentThatDiesThereIsStillCovered`). With the hold bound off (`0`) the stamp is
the **`--wait` deadline** (`hostRAMDeadline`), moved ahead of now + the slack whenever it would fall behind that (a long unload can run past
it), so a live reserve is never released by its holder and a dead one ends at `--wait`, the operator's own choice
(`TestAReserveWithNoHoldBoundStampsTheWaitDeadlineAsItsOuterBound`; before, there was no stamp and a dead parent left the holder nothing
to act on until `gpu release`). It is off only when nothing was deferred (`TestNoOuterBoundIsStampedWhenNothingWasDeferred`). A failed drain leaves its lease held (the exit code tells the
caller not to start), and that lease now ends by itself too: the holder releases it at the last moment the reserve stamped, and the
error says when (`detachedMaintainError`); `gpu release --epoch N` frees it sooner. Open, and stated: a reserve that dies in the few
milliseconds between the holder taking the lease and the first stamp leaves no stamp.

**The stamp says which moment it is.** The one field, `host_ram_hold_until_ms`, is three different moments, and `gpu status` read all of
them as "released at HH:MM if the host still lacks the room", which during the drain and the unload is false on both counts (the host has
not been asked, and a live reserve never lets the lease go then). The record now carries `host_ram_hold_phase` (additive, omitempty):
`outer` (the live reserve's rolling bound while it clears the seat), `wait` (the end of the wait for the host) or `release` (the push-out
of a reserve that is letting the lease go), written by `Manager.StampHostRAMHoldPhase` (`StampHostRAMHold` is the wait's) and taken off
with the stamp by admission; a stamp an older binary wrote carries none and is read as the wait's. `gpu status` says, for `outer`, "the seat
is cleared first, then the host is asked; if the reserve dies the holder releases about HH:MM" (and, past the moment, that the reserve has
stopped moving its bound), for `release` "the reserve is releasing the lease; if it dies the holder releases about HH:MM", and only for
`wait` the old sentence; the lease row carries `host_ram_hold_phase` beside `host_ram_hold_until`
(`TestStatusDuringTheDrainAndTheUnloadDoesNotPromiseAReleaseForAHostThatWasNeverAsked`).

The first outer bound was wrong in the other direction too: it was the drain's deadline plus the hold bound, and the UNLOAD is not
bounded by the drain's deadline (the maintenance client allows 15 minutes per request), so a drain that ended near its deadline,
followed by an unload longer than the bound plus the holder's grace, made the holder release a LIVE reserve's lease in the middle of
the unload: the warm-back skipped, the seat left owed and cold, the reserve failing with "no longer this reservation's". The same
re-stamp, which runs under the drain and under the unload, closes it. The margin is exact: a live reserve loses its lease to its
holder only if it cannot re-stamp for more than the hold bound plus the 60 s slack plus the holder's 30 s grace
(`TestAnUnloadLongerThanTheBoundDoesNotLoseALiveReservesLeaseToItsHolder`, the reviewer's probe compressed: `--wait 1s`,
`--ram-hold-max 300ms`, a 1 s holder grace, a 6 s unload; on the tree before the re-stamp the holder released the lease at the
bound and the reserve failed with "the lease is gone"). When a lease is gone after a failed drain or unload and the outer bound this
reserve last set has passed, the error says so (`detachedMaintainErrorAfter`, `TestADrainErrorSaysWhenTheOuterBoundReleasedTheLease`).

**The record carries the end of the wait.** `Manager.AwaitHostRAM` stamps the moment the wait ends (`host_ram_hold_until_ms`) on the
lease's record when the wait BEGINS, before the first look at the host, so a reserve that dies before its first look still leaves it.
The stamp is written by `Manager.StampHostRAMHold`, which does nothing on a lease that no longer asks for the host (an admitted lease
names no bound that is running: `TestAStampOnAnAdmittedLeaseIsANoOp`).
The moment is the earlier of the hold bound and the `--wait` deadline (`awaitHostRAMBounded` passes `HoldUntil: effective`), and it
is taken off the record **only by admission**, in the same critical section that declares the figure. A wait that ends any other way
(its deadline, a cancel, a figure no state can admit) leaves it there, because a lease that still asks for the host and carries no
moment is one nobody is bound to release. The first version cleared it as the wait ended; for the milliseconds until the detached
reserve pushed it out before its own release the record said "asks for the host, no bound", which the holder cannot act on, and a
reserve killed in that instant (a tool timeout lands at exactly the default bound) left the cards held for good
(`TestAWaitThatEndsWithoutAdmissionLeavesTheMomentItStampedOnTheRecord`, and the real holder against a reserve that dies the instant
its wait ends, `TestAReserveThatDiesTheInstantItsWaitEndsLeavesTheHolderAMomentToAct`). A stamp that cannot be written is reported
through `AwaitHostRAMOptions.OnStampError` and the CLI says it once, naming what it costs (`gpu status` cannot show when the lease is
released; the hidden holder cannot enforce the bound if the reserve dies). **The wrapper's wait goes on** (the wrapper is its own
holder) and tries again at the next look. **The detached wait fails closed** (`RequireStamp`): its hidden holder is the only thing that
releases the lease if the reserve dies and it can enforce only what is on the record, so the stamp is tried four times, 250 ms
apart, before the host is looked at, and if it never lands the wait ends with `*ErrHoldStamp` (`holdStampError`: the tries, the cause,
and whether the outer bound stamped before the drain stays on the record, which it does: nothing clears a stamp but admission), and the
reserve releases the lease (`TestADetachedReserveWhoseStampCannotBeWrittenFailsClosedAndReleases`). Failing closed is strict: it
happens even if the first look would have admitted the figure, and the caller reserves again.

**The hidden holder enforces it.** A detached hold's bound used to be enforced only by the foreground reserve: if that died during the
wait (a tool timeout, Ctrl-C, a closed terminal) the hidden `gpu hold` renewed its term for ever. It now reads its own record on every
poll (`Lease.HostRAMHold`) and, once the stamped end has passed by `holdBoundGrace` (30 s, so a live reserve releases first, and a reserve stalled under paging or resumed after a suspend is not given a parallel warm-back: the grace delays only a backstop; it was 5 s, and a 50 ms test grace failed 4 runs in 16 under CPU starvation) while the
lease STILL ASKS for the host's RAM (`host_ram_wait_gib` not yet admitted), releases the lease itself (`releaseHolderAtWaitEnd`): the
instances stopped, the seat warmed back only if the lease owes it, the lease released (`TestTheHiddenHolderReleasesItsOwnLeaseOnceTheStampedWaitHasEndedAndNoReserveDid`).
An admitted lease and a wait that has not ended are left alone (`TestTheHolderLeavesAnAdmittedLeaseAndAWaitThatHasNotEndedAlone`).
The holder also confirms a failed fence before it lets go (`holderLeaseGone`: three further looks, 50 ms apart): a fence read that
lands while a restamp replaces the record fails for a moment on Windows, the holder's exit releases its lease, and the reserve now
restamps the record more often during a wait (the stamp, the admission, the push-out), so one failed read must not release a live lease
(`TestTheHolderDoesNotLetGoOfALiveLeaseBecauseOneFenceReadFailed`, found as a 1-in-60 flake of the admitted-lease test).
`Lease.Check` now tells a verdict (`the lease is gone`, `fenced out`) from a record that could not be READ (`*RecordUnreadableError`: an
I/O failure that is neither absence nor corrupt bytes): a verdict is let go of after the three looks, a failed read is retried for
`holdIORetryFor` (30 s) with one line (`gpu hold: cannot read the record of lease epoch N (...); retrying ...`) before the holder lets
go, and whatever it lets go on it says (`gpu hold: letting go of lease epoch N: <error>`), since the holder's stderr is a log nobody
tails (`TestAHolderRetriesAFenceItCannotReadAndLetsGoOnlyAfterSayingWhy`, `TestAHolderThatLetsGoOfALeaseThatIsGoneSaysWhatItSaw`).

**Every holder confirms a failed fence the same way (`confirmFence`), and a record it cannot read is held while its keeper is alive: the keeper gives up first.** On
Windows under load a heartbeat `Renew`/`Check` that races an atomic replace of the lease record reads the lease as gone; the wrapper's
tick printed `LEASE LOST` and killed the wrapped command, `renewWhile` cancelled the drain, and the drain loop released the lease and
queued again, so a long GPU job could die to a record write. The helper is the holder's: three more looks, `holdConfirmPause` apart; a
verdict (gone, fenced out) on all of them is lost; a record that could not be READ is retried for a window and then is neither lost nor
fine. The hidden holder uses it with its 30 s window and lets go as before; the wrapper's tick, `renewWhile` and the drain loop use a
10 s window (`wrapperFenceWindow`, looking every 250 ms). Within the window nothing is killed, released or requeued; **past it, with the
record still unreadable, each FAILS CLOSED with one line saying why**: the wrapper stops its command (`gpu reserve: the lease record cannot
be read (...) and has not been for 10s ... stopping the command`), `renewWhile` ends the drain like a lost lease (cancelling it, with the
cause), the drain loop ends the reserve without releasing-and-queueing again, and the hidden holder lets go (`gpu hold: letting go of lease
epoch N: its record could not be read for 30s ...`). A heartbeat that cannot be written while the fence passes does not kill the command
(it says so once): a persistent heartbeat-WRITE failure with a readable record is tolerated, and the lease then goes stale and is reclaimed
by the ordinary rule (a stale heartbeat AND an expired window). One out-of-process test replaces a running wrapper's record with a
directory for several ticks and checks that the command, what it started and the wrapper are alive afterwards and no `LEASE LOST` was
said (`TestAWrapperWhoseRecordReadFailsForAMomentDoesNotKillItsCommand`). Left as it was, and stated: the warm-back's guard
(`leaseWarmGuard`) still abandons a warm on a plain `Check` failure (it kills and releases nothing; the seat stays owed).

**The rule, and the two sides that must agree (`keeper.go`).** A record that EXISTS and cannot be READ for an I/O reason (a scanner holds it
open, a sharing violation, an access error while it is replaced; not corrupt bytes, which stay debris once old) is **held while its keeper
is alive, and the keeper gives up first**. Before, the acquirer side and the keeper side disagreed: `sweepDebrisLocked` removed an
unreadable `e/<epoch>.json` older than `claimGrace` (10 s) together with its claims, the whole-node path treated an unreadable `meta.json`
as debris (`reclaimable(nil)`, `removeStaleClaim`), and the orphan-claim sweep removed the claims of an epoch whose record the reader
skipped, while the wrapper in its retry window kept running its command and wrote no heartbeat: the next reserve got the cards with about
25 s of overlap. Now (a) the acquirer side leaves such a record, and its claims, alone while its keeper shows a sign of life within the
heartbeat TTL (`DefaultHeartbeatTTL`, 120 s, the threshold the reader judges every record's heartbeat by): the epoch's `hb.<epoch>` file
(its content is the moment of the last renewal; every `hb.*` counts for a `meta.json`, whose epoch is not known) or the record's own
modification time. A heartbeat is read as the reclaim rule reads it (`keeperEvidence`): its content when it is a time of renewal (a number
not below `minHeartbeatMs`, the earliest instant 13 digits of milliseconds can name; a torn write leaves a prefix of the digits, which parses
and is not one), else the file's modification time; a file settled absent is no sign; one that can be neither read nor examined, or whose
rename is installing it, is unknown, and unknown is alive. For a `meta.json`, whose epoch is not known, the heartbeats are found by listing the
lease directory, and a listing does not show a name a rename is installing: a staged `hb.<epoch>.tmp` that is fresh and whose live name is not
listed counts as an unknown heartbeat (as a launch marker's does), and so does a listing that fails. The whole-node grant, the card grant over a whole-node record and `verifyNoDeviceLeases` (a whole-node grant over a
card lease whose record cannot be read) refuse with "the lease record cannot be read, and its holder is alive" when a heartbeat or a write within the TTL was seen, and with "... its heartbeat cannot be told ... so its holder
is not shown to be gone" when none was and the heartbeat is unknown (that one ends when the heartbeat can be told, when a staged one ages past `stagingFresh`, or at `gpu release`;
`TestTheRefusalForAnUnreadableRecordSaysWhatItRestsOn`); the sweep takes the record only once that liveness is gone. (b) The keepers above fail closed once their window passes, and every keeper's longest silence
(its tick or renewal interval plus its window plus the confirming looks, and for the holder its release attempts) is shorter than that
TTL, so a keeper has always stopped before an acquirer can be handed its cards: wrapper 15 s + 10 s, the drain heartbeat 15 s + two
10 s windows, the hidden holder 15 s + 30 s + 4 s (`TestEveryKeepersSilenceIsShorterThanTheHeartbeatStaleThreshold`).
`TestAnUnreadableRecordWithALiveWrapperIsNotSweptAndTheWrapperStopsBeforeAnyoneElseGetsTheCards` runs a real wrapper whose record
cannot be read (a read fault on a flag file, installed in the wrapper and in the test; the record is old and removable) and a second
reserve that tries every 100 ms: it is never granted, the record is never swept, and the command is stopped;
`TestAnUnreadableCardRecordWhoseHolderIsAliveIsNotSweptAndItsClaimsStay` does the card form and then ages the keeper's heartbeat until the
record is debris; `TestTheDrainHeartbeatEndsTheDrainOnceTheRecordHasBeenUnreadableForItsWindow` the drain's. Stated limits: the reader
still reads an unreadable record as absent, so `gpu status` shows the card free while a reserve is refused for it; a keeper that dies
leaves the record held for up to the heartbeat TTL (120 s) after its last sign of life; the windows and the TTL are chosen figures, not
measurements.

Which writers atomically replace a held lease's record (`meta.json`, or `e/<epoch>.json`), read from the callers of `renameReplacing`
and `writeEpochRecord`: every `Manager.Restamp` (the temp file renamed over the record) and so (a) the holder's own restamps (the
draining and exclusive stamps of the drain, the tree recorder, the term ticker's renewals and expiry labels in `AdvanceTerm`), (b) the
detached parent's drain stamps, a different process from the holder that polls the same record, (c) `AwaitHostRAM`'s admission and
`StampHostRAMHold` (the wait's stamp, the outer bound's 30 s re-stamp, the push-out before a release), (d) `MarkHolderGaveUp`, and (e)
once, the grant that turns a granting card record active. The queue's waiter files, the yield request file (`gpu_on_overdue=request`),
the heartbeat file `hb.<epoch>`, the orphan marks, the owner registry and the epoch counter are other files: they replace nothing the
fence reads, and only add to the directory's I/O at the moment a fence is read.

**The holder's own release is retried and its failure is not silent.** It is tried four times, a second apart (a lease off the record
counts as released whatever the answers were, and one that reports success while still on the record is a failure); when it gives up
the holder prints `LEASE NOT RELEASED`, marks the record (`holder_gave_up_ms` and `holder_gave_up_why`, additive, best effort), and
**exits non-zero** instead of exiting 0 after writing to a log in the temp directory. `gpu status` reads the marker: `the holder gave up
releasing it at HH:MM (why) and nothing polls the lease now; gpu release --epoch N frees it`, for a waiting lease and an admitted one;
the lease row carries `holder_gave_up` (`TestAHolderWhoseOwnReleaseKeepsFailingRetriesReportsExitsNonZeroAndMarksTheRecord`,
`TestStatusSaysTheHolderGaveUpAndNeverPromisesItsNextPoll`). Once the holder that wrote the marker has exited, the record is
reclaimable and `gpu status` reads the lease as stale (`GPU: free (unreserved)` and the stale-holder verdict); that line carries the
marker too: `the holder gave up releasing it at HH:MM (why), so the release failed: the next gpu reserve reclaims it, or gpu release
--epoch N frees it now` (`TestAStaleHolderThatGaveUpReleasingItSaysSoAndWhy`).

**The parent that is alive releases first and is not raced.** When a wait ends the parent pushes the stamped end out by
`detachedReleaseGrace` (15 minutes) before the slow part of its own release (a seat warm-back takes minutes), so the holder neither
steps in nor doubles the warm-back (`TestAReserveThatIsAliveReleasesFirstAndTheHolderNeitherDoublesTheWarmNorStepsIn`). If the parent
dies in the middle of that release (a tool timeout lands at exactly the default bound), the pushed stamp is the moment the holder takes
over (`TestAReserveThatDiesInTheMiddleOfItsOwnReleaseIsStillCoveredByTheHolder`). Both releasing is epoch-fenced and harmless: a lease
already released by the other party is not a failure. These two tests hold the reserve AT its push-out
(`pushedOutHook`, a nil seam in production) with the holder's clock held back (`holdClock`) until they have decided what the holder may
see, and do not poll the record for a window a starved test goroutine can miss (3 failures in 54 starved runs); the holder reads its
clock BEFORE its record, so a stale stamp can only be compared with an older now. The reserve measures its own wait on a monotonic
clock and the holder compares wall-clock milliseconds, so after a suspend the holder can release while the reserve still has time
left: the safe direction, since the lease is then released sooner and never later than the stamp says.

**An automatic release warms only what it owes.** `gpu release --warm-seat` is the operator's explicit request and warms
regardless; the release the harness makes on its own (the detached parent's, the holder's) uses `autoReleaseWarmGuard` /
`leaseWarmGuard`, which set `onlyIfOwed`, as the wrapper form always did. A seat that was never loaded (the incident's own shape) is
not loaded by the release of a lease that never unloaded it, the 2026-09-23 defect class (`TestADetachedReleaseAtTheBoundNeverWarmsASeatThatWasNeverLoaded`).

**The outcome of a release is reported after it happens.** A release that did not happen prints one line,
`gpu reserve: LEASE NOT RELEASED (epoch N): <error>; run gpu release [--warm-seat] --epoch N` (the hidden holder's says `gpu hold:`),
from the wrapper's deferred release, the detached parent and the holder; the failure text never claims a release that has not happened
(`TestAReleaseThatFailsAtTheBoundIsSaidAndNeverClaimedAsDone`), and **the error the command returns ends with the failure**: `...; the
release then failed and the lease is STILL HELD (epoch N): <why>` (`TestWhenTheReleaseFailsTheReturnedErrorEndsByNamingTheFailureAndTheLeaseStillHeld`).
The command names `--warm-seat` only when a warm-back is owed: `gpu release --warm-seat` has no `onlyIfOwed` guard (it is the
operator's explicit request), so advising it for a lease that unloaded nothing could load a seat nothing asked for
(`TestTheNotReleasedLineAdvisesAWarmBackOnlyWhenOneIsOwed`).

**A record that exists but cannot be read is a held lease, never a gone one (`Manager.RecordState`).** `Inspect()` and `Leases()`
read through the reader, which judges who holds the cards and, for that, skips a record it cannot read (`legacy` returns nil on any
read error, `v2` continues past it). Every decision that a lease is GONE therefore comes from `Manager.RecordState(epoch)`, which
looks at the record itself (a card lease's `e/<epoch>.json`, a whole-node `meta.json` naming the epoch) and answers absent, held or
unreadable, with the cause for the last; unreadable counts as held. (A card record that exists with bytes that are not a record counts
as unreadable too, since its claims may still stand; a whole-node `meta.json` with such bytes counts as absent, the debris an acquirer
sweeps, as every other reader treats it.) It decides `leaseStillHeld`, `confirmReleased`,
`releaseLeaseConfirmed`, the holder's `releaseHolderLease` (which used to believe `Inspect` and return nil, so the holder exited 0 over
a lease it could not touch; it now retries, prints `LEASE NOT RELEASED`, tries to mark the record, and exits non-zero) and `gpu
release`'s post-check; the whole-node `Lease.Release` returns a wrapped `*RecordUnreadableError` instead of nil "already gone"
(`TestRecordStateSaysAbsentHeldOrUnreadableAndUnreadableIsNeverGone`, `TestTheHoldersReleaseOverARecordItCannotReadIsAFailureNeverSuccess`).
Bare `gpu release` with no `--epoch` enumerates the card records directly: one that exists and cannot be read is an error, `epoch N: its
record cannot be read; run again or pass --epoch N`, not "no lease is held" (`TestBareGPUReleaseNeverSaysNoLeaseIsHeldOverAnUnreadableCardRecord`).
`Restamp` returns a wrapped `*RecordUnreadableError` for a record it cannot read and wraps the real cause (`%w`) when the lease is
gone; `AwaitHostRAM` looks again three times, 100 ms apart, before it says a missing lease is gone, and says "the record cannot be read,
so the lease is not known to be gone" when it is the record that failed; `releaseAfterRAMWait` treats an unreadable record as held and
proceeds to stop the instances, warm the seat back and release
(`TestAnUnreadableRecordIsNotTheLeaseIsGoneInRestampAndInTheWait`, `TestTheDetachedReleaseProceedsOverARecordItCannotRead`). The holder's
release also keeps its error while the card lease is half released: a card lease removes its record first and its claims after, so a
failure on a claim leaves the record absent, and "the record is absent" alone is not "released" (`releaseHolderLease` keeps the
release error unless the record is absent AND `Manager.ClaimsOf(epoch)` is empty; the moved-on refusal, `IsLeaseMovedOn`, is the one
error that clears it: `TestTheHoldersReleaseKeepsTheErrorWhenTheRecordIsGoneButAClaimRemains`). Over a record it cannot read,
`stopInstancesOfLease` says "its record cannot be read" and stops nothing, instead of "the lease is no longer ours"
(`TestStopInstancesOverAnUnreadableRecordSaysSoAndNotThatTheLeaseIsNoLongerOurs`). Stated: the holder cannot write its give-up marker
onto a record it cannot read, so over such a record the exit status and the log are the only word.

**A release's "nothing was held" is believed only if nothing is held.** `Manager.ReleaseByEpoch` answers `(false, nil)` for a lease
that is gone, and used to answer it for a lease whose record it could not even examine (a stat that fails with an access error while
the record is being replaced), and its callers ignored the bool: the detached reserve printed nothing and left the cards for the
15 minute grace, and `gpu release` printed `no lease is held` and exited 0 over a lease that was held. Only an ABSENT record is "nothing
held" now (any other failure to examine or read it is an error that names why), and the detached reserve, the wrapper's final release,
the holder and `gpu release` take the answer from the record: a release that did nothing is believed only if the epoch is off it
(`confirmReleased`; `TestAReleaseWhoseRecordCannotBeExaminedIsAnErrorNotNothingHeld`,
`TestADetachedReleaseThatReportsNothingHeldWhileTheLeaseIsHeldIsAFailureAndIsSaid`, `TestGPUReleaseNeverSaysNoLeaseIsHeldWhileOneIs`).

**Where instances may be stopped.** The ComfyUI instances kept under a lease are stopped only by a caller that fenced the epoch first,
and `TestInstancesAreStoppedOnlyByCallersThatFencedTheirEpoch` pins each caller by the function that holds the call, across every
non-test file of the package: through a `*Lease` (`stopInstancesOfLease`: the wrapper, the holder, the holder's release at the end of
a wait; the holder stops twice, before the warm-back for the VRAM and in its deferred function, which finds no marker of an instance the
first stop took, so it is redundant and harmless) or by an epoch the caller resolved or checked (`stopKeptInstances`: the release
verb for the target `ReleaseTarget` named, the detached reserve's release for the epoch `HoldsEpoch` confirmed, which never holds the
lease itself; an epoch is never reused and each marker names its lease, so a stop by epoch leaves an instance a later lease took over).
These callers stop with `StopForLease`. The media pipeline's routine release of its own lease (`internal/pipeline/mediaadmit.go`,
`stopKeptInstances`) uses `StopForRelease` and spares an instance that ends itself (ADR 0097).

**A failed unload is not "freed nothing".** `unloadOthers` logs an unload that failed and goes on; the outcome counts them, and the
waiting line and the bound's failure then say `N unloads of other resident models failed (see above), so what the unload freed is not
known` instead of claiming that nothing was resident or that the shortage is not the seat's.

**The waiting line says when the unload freed nothing.** `maintainSeatReport` reports what the unload took down (`unloadOutcome`):
when the seat was not loaded, or sits on cards the lease does not hold, and nothing else resident was on the lease's cards, the line
printed once when the wait begins says `the unload freed nothing on these cards (the seat was not loaded), so it did not shorten
this wait`, and it names the bound (`hold bound 10m0s: --ram-hold-max, config gpu_hostram_hold_max_sec`). The bound still applies:
a cold seat is not special-cased into an immediate failure, because the screens before the grant cannot tell this apart from the
film shape (the third consequence, below), and one reading of residency taken after the unload says nothing about how the host will
move in the next minutes. A reading that cannot tell (an ambiguous `/running`) counts the seat as unloaded, never as freed-nothing.

**The class-default figure is warned about when it is declared.** A wrapped command that is not a recognised render helper (and the
other three ways to the class default: no command, a `--graph` the lease could not read and size in full, an unsizable helper) prints a `gpu reserve: WARNING:` line right after
the declaring line: the figure is a guess about a command the lease could not read, it may hold the cards while the host cannot meet
it, and `--ram <GiB>` states the real one. The figure itself is unchanged (host RAM is never overloaded).

**Records.** F82 is recorded in no ADR: a search of `docs/architecture/decisions` for `F82`, `AwaitHostRAM`, `host_ram_wait` and
"asked, not yet admitted" finds nothing, and the only ADR passages that put host RAM and an unload in one sentence (ADR 0009's
2026-10-13 ComfyUI idle-free paragraph, ADR 0080's RAM-spill seat) are about other mechanisms. So this section is its record and
there is no ADR amendment: the hold bound is documented here, in the OPERATOR-GUIDE (config row and host-RAM paragraph)
and in the changelog fragment.

**What a lease shows while it waits.** It is held, not queued: its record carries `host_ram_wait_gib`, it declares nothing
(`host_ram_gib` is absent), and it is not marked `waiting_for: host-ram` (that mark belongs to a request that does not hold its
cards yet). `gpu status` says `host RAM N GiB asked, not yet admitted` on the lease line, followed by how long the cards have been
held (`held the cards for 12m3s`) and, once the wait for the host has begun, when the lease is released if the host still lacks the
room (`the lease is released at 8:55AM, in 9m59s, if the host still lacks the room (the hold bound, or --wait if that ends first)`).
During the drain and the unload, when the stamp is the reserve's rolling outer bound, it says instead `the seat is cleared first, then the
host is asked; if the reserve dies the holder releases about 8:55AM` (see "The stamp says which moment it is").
Past that moment, while the lease is still there, it says who releases it: `the wait ended at 8:55AM: the holder, if one is running,
releases the lease at its next poll; `gpu release --epoch N` frees it now` for a `--detach` lease (the reserve that took it may be gone,
and so may the holder, so the text promises neither; `--warm-seat` appears only when a warm-back is owed), `the wrapper that holds the
lease releases it when it exits` for a wrapper, and, when the holder gave up, `the holder gave up releasing it at ... and nothing polls
the lease now`. The moment is on the
record as `host_ram_hold_until_ms` (the outer bound stamped before the drain, then `Manager.AwaitHostRAM`'s stamp when the wait begins;
taken off when the figure is admitted; additive and omitted otherwise), because the bound can be a flag on one reserve and no other process could read it from anywhere
else. `offload_status` carries `host_ram_wait_gib` and, while the lease still asks, `host_ram_hold_until` (RFC 3339) on the lease row.
Once admitted the row shows `host_ram_gib` alone.

**Three consequences, stated.**

- *A lease that declares nothing in the grant passes a front waiter that waits only on host RAM* (the G6 rule above), and it must:
  that waiter is short of memory the seat holds, and a lease that unloads the seat, queued behind it, could never clear it. The
  cost is G6's own: when the memory recovers the waiter waits for the cards the lease took. The passing lease still asks the host
  for its own figure before its command starts, so nothing is admitted past the line
  (`TestADeferredLeasePassesAWaiterThatWaitsOnlyOnMemoryAndStillAsksTheHost`).
- *While it waits the lease claims no host RAM*, so a grant on other cards that fits is admitted ahead of it, and the lease then
  waits for what that grant has yet to load (`TestAGrantThatFitsIsAdmittedAheadOfAWaitingLeaseAndTheLeaseThenCountsIt`). This
  is the "nothing reserves a waiter's memory against a declaring request on *disjoint* cards" limit above, unchanged. The
  alternative, keeping the figure on the record as declared from the grant on, makes two such leases count each other's figure as
  still to load, and a host that fits one of them would admit neither (`TestTwoWaitingLeasesAreAdmittedOneAfterTheOther`).
- *A shortage the unload does not cure costs the cards held and the seat cold, for at most the hold bound (600 s unless the operator
  says otherwise); before the bound existed, until `--wait` ended it, 8 h by default.* When the memory is held by
  something the unload does not touch (another lane, a foreign process), or the seat was already cold and there was nothing to
  free (residency is not asked before the grant), the lease has done its unload and holds its cards,
  exclusive, until the hold bound passes (then it is released, and fails with the bound's line) or `--wait` ends the wait (8 h by default,
  which only a bound of 0 or a short `--wait` reaches; `--wait 0` still gets the drain floor, `hostRAMDeadline`; the lease is released
  then too, in both forms), its command never
  run and, when the seat had been loaded, its warm-back attempted at the release and refused by the same rule while the host stays
  short (then it stays owed). The 2026-10-14 incident was this consequence with no bound: about 50 minutes idle. Before
  F82 such a lease queued with the seat untouched and the cards free. Nothing before the grant can tell this apart from the film
  shape, and the reason is stated so nobody "fixes" it by screening: how much of the host's commit the seat holds is not
  measured (`agent_seat_host_ram_gib` is a floor the operator states and its default, `hostneed.DefaultSeatHostGiB`, is a chosen
  21 GiB; the film seat held 67 GiB), so subtracting a seat figure from the commit before the grant would refuse exactly the case
  this reorder exists for on a node that states no figure, and any prediction made when the reserve starts is stale when the grant
  comes, hours later, so one that sent the lease back to the grant would bring the deadlock back whenever the cause changed in
  between. The comparison after the unload is the decision. The only screens before the grant are the ones that do not depend
  on the host's state: a need above physical RAM less the headroom, a commit above the commit limit less the headroom (where the OS
  enforces one), an unload that reaches nothing on the lease's cards, a host that cannot be read. The levers are the hold bound (`--ram-hold-max`, `gpu_hostram_hold_max_sec`), `--wait`, and `--ram` when the
  figure is a guess (and Ctrl-C, which ends the wait and releases the wrapper's lease).

**Not covered.** The media admission in `internal/pipeline` takes its lease with the declared figure compared in the grant, and the
render lane unloads the seat from inside that lease (`claimLeaseUnload`, `freeLlamaSwap`); it asks for no `--unload-seat`, so this
reorder does not apply to it.

### What `gpu status` and `offload_status` show

`gpu status` (text and `--json`) and `offload_status`'s `gpu_lease` block carry `host_memory`: `physical_gib`,
`available_gib`, `commit_used_gib`, `commit_limit_gib`, `commit_limit_enforced` (whether a commit a lease states is
judged against the commit limit on this host: always on Windows, on Linux in strict overcommit mode or when the mode cannot be read; a commit nobody stated is
held to physical RAM less the headroom on every host, or to an enforced commit limit less the headroom when that sits below physical RAM), `headroom_gib`, `declared_live_gib` and `declared_live_commit_gib` (the sums of the working set and the commit the live
leases declared), `pending_gib` and `pending_commit_gib` (the part of each still to load), `admits_up_to_gib` (the most a touched
set could ever be admitted at), `touch_room_gib`, `commit_room_gib` and `unstated_commit_room_gib` (what a new lease could touch now,
what a lease that STATES a commit could be charged now, and what a lease that states none could declare now: physical RAM (or an enforced commit limit that sits below it) less the
commit charge, what running leases have yet to touch and the headroom; each less the headroom and never below 0),
`verdict_why` (which line a NEAR or OVER verdict rests on, with its numbers), `stated_commit_note` (on an OVER that rests on the commit charge above physical RAM alone: what a lease that states its commit is judged on instead) and one **verdict**, built by `gpucards.HostView` for
all three surfaces so they cannot disagree. The verdict judges the lines the grant applies:

| host verdict | meaning |
|---|---|
| `OK` | the available RAM, the room under physical RAM for the commit charge and, where the OS enforces a commit limit, the room under it all exceed the headroom |
| `NEAR` | one of them is inside the headroom: a lease judged on that line waits (any lease that touches host RAM when the available RAM is; one that states no commit when the room under physical RAM is; one that states its commit when the room under the commit limit is). A lease that states its commit is not held to the physical-RAM line, so a host that reads `NEAR` on that line alone still admits it (`TestAHostThatReadsNearOnThePhysicalCommitLineStillAdmitsALeaseThatStatesItsCommit`) |
| `OVER` | one of them is gone: nothing is available, the commit charge is above physical RAM (the line a commit nobody stated is held to: the paging incident's own reading, 162.9 GiB committed on 127.7 GiB with 3.2 GiB available, reads `OVER` and leads the brief line, as it did before the two conditions), or it has reached an enforced commit limit. The reading cannot say whether the box is paging now (that takes page-file growth or the pages-out rate, which no surface here reads), so none of them claims it. New leases that declare host RAM and state no commit wait until there is room again, and one that states a commit is judged on the limit; end a lease or stop a kept ComfyUI instance to free it. An admitted stated commit can itself take the box above physical RAM (the coder arm's measured 58.1 GiB does: 128.6 on 127.7), and the verdict then reads `OVER` by design |
| `unknown` | the reading could not be taken, or came from a platform whose commit limit is always enforced and carried none |

The brief verdict line of `offload_status` leads with `HOST RAM OVER (3.2 of 127.7 GiB physical RAM is available; committed 187.9 of
the 187.7 GiB commit limit, 8.0 GiB headroom)` in capitals, ahead of the lease verdict word, because `free` at the head of that line reads as "the box has room";
NEAR trails it. When the verdict rests on the commit charge above physical RAM alone, the lead goes on to say what a lease that states its commit is judged on
(the commit limit, or nothing where the OS enforces none), because the rule admits such a lease while the host reads OVER; the block carries the same words as
`stated_commit_note` (`TestTheBriefLeadSaysWhatALeaseThatStatesItsCommitIsJudgedOn`, `TestTheOverLeadSaysWhatALeaseThatStatesItsCommitIsJudgedOn`). The lease rows show what each lease declared (`host RAM 10.0 GiB touched, 60.0 GiB commit`), and the queue rows what a waiter waits for
(`TestGPUStatusJSONCarriesTheHostBlockAndAnOverVerdict`, `TestStatusNamesOverLoudlyOnTheBriefLine`).

### The driver's own fault events for the leased cards (C-83-B)

A card that faults under a lease leaves one line in the driver's log, and until 0.184.0 nothing showed it (the register counted
181 `nvlddmkm` id 153 events since 2026-09-22 on the reference box, found by hand): on Windows an event in the **System** log from
provider `nvlddmkm` (id 153 reads `Error occurred on GPUID: b500`, or `UCodeReset TDR occurred on GPUID:100`), on Linux an
`NVRM: Xid (PCI:0000:b5:00): 79, ...` line in the kernel log. `gpu status` (text, and `--json` under `activity`) and `offload_status.gpu_lease.activity` now carry the last 24 h of
them for **the cards the lease holds** (every card for a whole-node lease; the union of the leases' cards when several are live):

| key (`activity.`) | meaning |
|---|---|
| `driver_events` | newest first, at most 20: `{at, source (nvlddmkm \| Xid), id, bus, card, uuid, text}`; present only when there is one |
| `driver_events_total` | how many the leased cards had before the list was capped (only when capped) |
| `driver_events_unattributed` | events in the window that name no card this box can place, so no card's list holds them: said, so an empty list is not read as "no faults". On the reference box the Windows id 13 `Graphics Exception` records are the bulk of these (420 in 96 h against 14 id-153 ones): they carry no GPUID |
| `driver_events_note` | why the events could not be read (`driver events unavailable: ...`) or why the list may be short or unplaced (`driver events incomplete: ...`: a log read that stopped part way, a journalctl that could not be read so only this boot's `dmesg` was, a card table that could not be read so the events are listed nowhere): a reader failure is a note, never an error |

**Facts, never a verdict.** A driver fault and a kill look alike in the log (A-128's kill test is what tells them apart, and it
is queued, not built), so an event never moves the lease's verdict, the brief line or any gate. A box with no source (an OS with
no log reader, or the reader not wired) prints byte-identically to before: no key, no line.

**How it is read.** `gpuactivity.ReadDriverEvents` is the production reader, handed in as `Options.DriverEvents` (nil = off;
`activityOptions` wires it for `gpu status`, `offload_status` for the lease block): it is called only while a lease is held and
the GPU sample is on. Windows: **one** `powershell.exe` from System32 (`-NoProfile -NonInteractive`), a read-only `Get-WinEvent`
over `LogName System`, `ProviderName nvlddmkm`, the last 24 h, the newest 1000, each record as XML; a log with no matching event is
an empty answer. Linux: `journalctl --no-pager -o short-unix --since "24 hours ago" _TRANSPORT=kernel` (the kernel transport as a
match, not `-k`: `-k` implies `-b`, the current boot only, and a fault such as Xid 79 often ends in a reboot; on a gate host with
four boots in its journal, seven days read 2528 lines with `-k` and 4226 across every boot), then `dmesg --time-format iso` when
journalctl cannot be read, which holds this boot's ring buffer only and says so in the note. **Every child starts through
`internal/winexec`** (no console window on Windows). The log read is bounded at 8 s and the card-table query has its own 4 s,
so a slow log cannot leave `nvidia-smi` nothing. The answer is cached for 60 s in the process, failures included, so a poll of a
**long-lived process (the MCP server)** starts no child per call; **`gpu status` is a fresh process on every call, so each one
pays a read** (about 0.6 s on Windows, once per call, only while a lease is held). An answer whose events could not be
placed because the card table was unreadable is held 10 s, not 60. The record's host name, user and device object path are not
carried. The card behind an event is found by **PCI bus and device**: the record's GPUID is that pair as one hex number,
`(bus << 8 | device)`, **printed without leading zeros** (`b500` = bus `b5`, device `00`; a card on bus `01` logs `100`, so a
one-card box's events are placed by reading one to four digits as a number and splitting it), the kernel line says
`PCI:0000:b5:00`, and `nvidia-smi --query-gpu=index,uuid,pci.bus_id` (a second small query made inside `gpuactivity`, so the
card sample is unchanged) ends `B5:00.0`; the PCI domain is ignored. A table that cannot be read leaves the events unplaced
(not lost) and the note says the table was the cause, with `nvidia-smi`'s reason; those events are then not counted as
`driver_events_unattributed`. `offload_status` computes the events only for the `gpu_lease` section and the full answer: the
`brief` returns none of them and starts no read.

**Limits, said plainly.** The Linux `Xid` shape is the documented one and was not captured from a live Linux driver (neither
Linux gate host has an NVIDIA card); the Windows shape was read from two boxes' logs (the fixtures in
`internal/gpuactivity/testdata/nvlddmkm-153.xml`, a multi-card box on buses `17`, `65` and `b5`, and
`nvlddmkm-153-lowbus.xml`, a one-card laptop on bus `01` whose records read `GPUID: 100`, hand-reduced, host name replaced).
`ReadDriverEvents` caps the Windows read at the newest 1000 records: a storm of id-13 records older than that pushes older
id-153 ones out of the window. A read costs one PowerShell start (0.56 to 0.65 s over three runs on an empty 24 h window): paid
once a minute by the MCP server and on every `gpu status` call.

### `gpu faults`: the fault history as incidents, with known errors

The lease view above answers "did the cards I hold fault in the last day". `local-offload gpu faults [--days N] [--jobs]
[--json]` answers the operator's question: what has the driver logged on this machine, on which card, how often, what it
is, and what NVIDIA would do about it.

- It is lease-independent: every card, whether a lease is held or not.
- It reads up to 30 days (`--days`, default 7, 1 to 30).
- It is **facts only** for the lease: nothing it reads moves a verdict, a gate, a drain or a release (C-83-B holds). The one
  use beyond reading is placement: a LIVE repeat (last burst within 24 h) demotes the cards it names, never excludes them
  ([ADR 0096](../architecture/decisions/0096-a-live-driver-fault-repeat-demotes-a-cards-placement.md)).

The idea comes from two NVIDIA products: the "supervised log errors" and job-centric incident view of the AIOps product,
and the incident rules of NVSentinel. Neither runs on this fleet (one is sales-gated, the other needs Kubernetes), so their
data and rules are carried and the reading is local.

**What it reads.** Every read is read-only, starts its child through `internal/winexec`, is bounded at 20 s and takes at most
5000 records per log. A read that hits the cap or stops part way says so in `notes`.

- **The driver log for the window.** These are the same readers as above, with the window and the cap as parameters.
  Windows reads `nvlddmkm` in the System log; Linux reads the journal's kernel transport with `--since "<hours> hours ago"`.
- **The card table**, with names and `gpu_recovery_action`, from
  `nvidia-smi --query-gpu=index,uuid,pci.bus_id,name,gpu_recovery_action`. A driver that does not know the field refuses the
  whole query, which is then made without it. GeForce Blackwell under WDDM reports `None` on the reference box.
- **On Windows, the live kernel dumps Windows Error Reporting filed.** These come from the Application log: provider
  `Windows Error Reporting`, id 1001, event name `LiveKernelEvent`, selected with an XPath so other applications' reports are
  not read. Dumps name no card.
  - WER files one report many times. On the reference box, 2464 records in 30 days were 59 reports of 25 dump files, so a
    dump is counted once per dump file.
  - The record time is when the report was queued, not when the fault happened (dumps of 2026-09-22 were first queued on
    09-25). The fault time is read from the dump's file name instead (`WATCHDOG-20261013-1921.dmp`, local time to the minute).
  - A report with no dump file keeps its queue time and is labelled `report-queue`.

**Known errors** (`internal/gpuactivity/knownerrors.go`). Each event gets a class, a name, a domain (app, card, driver,
display, link, system), a severity (info, warn, fault) and its basis:

| source | basis | what is carried |
|---|---|---|
| Linux `Xid N` | `nvidia-catalog` | NVIDIA's row from the Xid catalog (`xidcatalog.go`, generated, 109 used codes, see NOTICE): mnemonic, description, the immediate and investigatory resolution buckets, and the immediate bucket's meaning. Domain and severity follow the immediate bucket (`RESTART_APP` app/warn, `RESET_GPU` card/fault, `RESTART_BM` system/fault, `IGNORE` info) |
| Windows `nvlddmkm 13` | `observed`, read as Xid 13 | the same "Graphics Exception" / "Graphics SM Warp Exception" text Linux prints for Xid 13, so Xid 13's row and buckets (`RESTART_APP`, `WORKFLOW_XID_13`) |
| Windows `nvlddmkm 153`, `14` | `observed` | the engine reset (`Error occurred on GPUID`) and the microcode reset after a fault or a timeout (TDR): driver/fault, with no NVIDIA bucket (NVIDIA publishes no Windows table, and none is invented) |
| `LiveKernelEvent 117`, `141`, `193` | `microsoft-bugcheck` | `VIDEO_TDR_TIMEOUT_DETECTED`, `VIDEO_ENGINE_TIMEOUT_DETECTED`, `VIDEO_DXGKRNL_LIVEDUMP` |
| `LiveKernelEvent 1a8`, `1b8` | `user-reports` | the black-screen live-dump names that forum reports give; Microsoft does not document them |

The buckets are **NVIDIA's guidance for datacenter parts** (the catalog's applicability columns name A100, H100, B100 and
GB200), and they are printed as such. A wrong class costs a mislabelled line, never a changed verdict. An unmapped id is
`unknown`, not guessed.

**Incidents.** Records less than 3 minutes apart are one burst (NVSentinel's burst window). Each burst lists:

- the cards its GPUID-bearing records name, by bus as above (a bus not in today's table is printed as such);
- its classes, and how many of its records name no card (Windows id 13 names none);
- the live dumps whose time falls between a minute before it and two minutes after.

Two readings are carried from NVIDIA. They are labelled readings, never actions:

- **NVIDIA's Xid 13 workflow**, over the GPC/TPC units the graphics exceptions name (`(GPC 1, TPC 0, SM 1)`). One unit, or one
  unit holding four records in five, is the repeat NVIDIA sends to diagnostics as a possible hardware fault. Records spread
  over many units it reads as the application's fault (`CHECK_APP/CUDA`). On the reference box both id-13 bursts of the 30
  days were spread (280 unit-naming records over 35 GPC/TPC units, and 10 over 3), so both got the application reading.
- **NVSentinel's RepeatedXIDErrorOnSameGPU**: the same class in 5 or more bursts on one card within 24 h. Xid 31 and 45 are
  left out, as NVSentinel leaves them out, and so is a burst at a restart or sleep transition (next section). NVSentinel
  answers this rule with `CONTACT_SUPPORT`, which is printed beside it. The rule counts per card and class, so each burst
  keeps its classes per card (`cards[].classes`), which is what lets a retained burst count too.

**Restart and sleep context** (`internal/gpuactivity/faultcontext.go`). A driver logs faults while the machine shuts down,
boots, sleeps or wakes. Such a burst is still listed, labelled `context: restart` or `sleep-wake` with the OS records that
placed it there (source, id and time only: a restart record names the machine and the user who asked for it, so no text is
kept), and it does not count towards a repeat. Five restarts in a day are not a card failing five times.

- **Windows** (System log, one more read of the same shape): a restart opens at User32 1074 (a process asked for it),
  EventLog 6006 (clean stop) or Kernel-General 13 (OS shutdown) and closes at the next EventLog 6005 or Kernel-General 12
  (the boot). EventLog 6008 and Kernel-Power 41 are logged at boot after an unexpected shutdown, so they are points. A sleep
  opens at Kernel-Power 42 and closes at Kernel-Power 107 or Power-Troubleshooter 1.
- **Kernel-Power 566 is not read.** It is a session transition (unlock, input, display on): 244 of them in the reference
  box's 30 days, beside 87 records of every kind above, and it would label unrelated faults.
- **Linux** (journal): each boot's first entry is a point (`journalctl --list-boots -o json`), and a suspend or hibernation
  opens at the kernel's `PM: suspend entry` and closes at `PM: suspend exit` (`--grep`, which exits 1 with
  `-- No entries --` when nothing matches: that is an empty answer). The end of a boot is not read as a shutdown, because
  the journal does not tell a clean stop from a crash, and a fault just before a crash is the one to keep.
- A burst is in context when it overlaps a span widened by a minute on each side. Opening records less than 10 minutes
  apart are one opening, so a restart asked for an hour ahead (1074 with a timer) does not cover that hour.
- A context read that fails or stops part way is a note ("an incident at one may count towards a repeat").

On the reference box this labels 2 of the 19 incidents as restarts: 2026-10-15 22:02 (cards `17` and `b5`, 3 s after
User32 1074) and 2026-10-21 08:43 (card `b5`, 1 s after User32 1074). Neither was part of the repeat on card `65`, which
still fires.

### Retention and the fleet node's summary

The OS log is not a record: the Windows System log rolls over at its size cap and a Linux journal is vacuumed. fleet-serve
keeps the incidents, and publishes a per-card summary.

- **Retention** (`<state root>/gpu/faults.jsonl`, `internal/gpuactivity/faultstore.go`). Each fleet-serve refresh appends the
  incidents that have ended (no record for a burst gap and a minute, so a running burst is not cut in two) and overlap no
  retained one. One JSON line per incident, append-only, never pruned: the reference box made 19 incidents in 30 days.
  fleet-serve is the one writer.
- **`gpu faults` merges it.** A retained incident that overlaps none read from the log is added, marked `retained`, and
  counts like the others: on its card's counts, and towards the repeat rule. A store that cannot be read is a note; the
  report from the log stands. On a machine with no fleet-serve the file does not exist and nothing changes.
- **The summary** (`gpuactivity.FaultSummary`) is what `/fleet/health` publishes as `gpu_faults`
  ([FLEET-NODE.md](../FLEET-NODE.md#driver-fault-summary-gpu_faults)) and what fleet-serve writes whole (temp file, then
  rename) to `<state root>/gpu/faults-summary.json`, for this machine's short-lived processes that must not pay a 30-day
  read per call.
- **The refresh** runs in the background (`gpu_faults_health.go`), as the lane backends do: a reading is fresh for 10
  minutes and published for 40, nothing waits on it, and fleet-serve starts the first one at startup. A read that fails
  keeps the last good reading until it ages out. A node with no NVIDIA card publishes no `gpu_faults`. One read took
  3.4 s on the reference box (three log reads and the card table, all hidden children).

The real run of the retention path, on the reference box with a fresh state root: the first refresh appended the 19
incidents and the second appended none. Five bursts written by hand for a day the log does not cover (2026-10-06, card
`b5`) were then listed as `[retained]`, raised card `b5`'s counts from 19 records and 15 incidents to 29 and 20, and fired
the repeat rule on `b5`; the 19 incidents the log still holds were not counted twice.

**`--jobs`** adds the ledger rows whose work **ran on this node** during each burst, 2 minutes either side. It reads
production rows only (`ledger.ReadProduction`), and a row ran from `ts - latency` to `ts`. A row counts as run here by these
rules:

- An inner row is skipped; its outer row stands for the job.
- A row that took no time did no work.
- A delegate row names its node in its tier (`<node>:<seat>`), compared with `fleet_node_id`, else the host name.
- A row that names a remote node ran there.
- Any other row ran here unless its route was remote.

A deferral that ran for a while is kept and marked `deferred`, because a job a fault ended is recorded that way. A burst with
no matching row says `no ledger row ran here in this window`. That is common: callers that reach the seats directly
(llama-swap clients, ComfyUI) write no ledger row.

**The real run** (0.199.0) was on the reference box, with three Blackwell GeForce cards on buses `17`, `65` and `b5`, over 30
days, taking 5.9 s:

- 479 driver records (435 id 13, 44 id 153) in 19 incidents. Card `65` was in 11 of them and card `b5` in 15.
- The repeat rule fired on card `65`: 5 bursts of `nvlddmkm 153` in 82 minutes.
- 11 live dumps fell in the window.
- One burst (98 s, cards `65` and `b5`, with `141`, `117` and `141` live dumps) coincided with ten agent-pool jobs on those
  cards' seat, all of which ended deferred.

`--json` carries the same report (`cards`, `incidents[].jobs`, `incidents[].context`, `repeats`, `known_errors`,
`live_kernel_events`, `retained`, `notes`).

**Limits.**

- The card table is today's. A card moved to another slot since an event would be named by its new position.
- The Linux path shares the lease view's limit: the `Xid` shape is the documented one, not captured from a live Linux driver.
  Linux also has no live-dump source here.
- Retention starts when a fleet-serve of 0.200.0 first runs: what the OS log had already lost by then is not recovered.
- A retained incident keeps the card by bus. A card moved since is named by today's table, like a live one.

### A kept instance is freed when its family changes

The paging incident's 57 GiB instance probably held two families' weights (reconstructed, not observed).
`render/comfy-family.mjs` closes the ways the code allowed that: the launch marker remembers whose weights a kept instance may
still hold, a runner that finds another family there frees it before its first job, and the end-of-run `/free` is awaited,
retried and loud (a free that fails is said, and leaves the old family recorded so the next runner tries again); see "A kept
instance (`--keep-comfy`)" in [media-generation.md](media-generation.md). It closes one way an instance can carry more than a
lane's own weights; it does not explain the whole figure. A ComfyUI process launched fresh on 2026-10-10 held the same 57.7 GiB
private about seven minutes after launch (a reading on the reference box, from a session, not recorded in this repository), so the footprint of a single lane is unmeasured here and the S1 acceptance runs
record it (see "Known limits").

## A dead wrapper with a live tree keeps its claim (plan P10, ADR 0084)

*A wrapper that is gone proves the wrapper is gone, not the job.* Until this change the reclaim rule read "the holder's pid
is dead" as "the card is free": a wrapper that was killed, crashed or lost its session left the command it started running
on a card that was truly in use, and the next acquirer was handed that card at once (two jobs on one card). When the lease
was lost the wrapper stopped only its direct child, so a grandchild kept the card the same way.

**The rule.** The wrapper starts its command inside a containment (below) and records the processes of the tree it started,
pid and start identity of each, in the lease record (`tree`, additive and `omitempty`; an older reader ignores it). A lease
whose wrapper is **gone** (its pid dead, or alive but a different process: a recycled pid) is **not reclaimable while any
recorded member is alive by pid AND start identity**. The lease then reads **`tree-orphan`**: it stays held, fenced and
claimed on its cards, and a waiter is told so. The reclaim rule for a lease whose wrapper is alive is untouched (the
heartbeat-and-window conjunction). A lease that recorded no tree and whose wrapper is gone has no record to hold it: it is free at
once **unless** the reader can tie a live process to it by the other evidence below (a launch marker, or on Windows a child of the
dead wrapper that began after the lease). With none alive it is reclaimed exactly as before.

**Bounded by observation, never by a timer.** Every read of the lease checks the members again, by pid and start identity.
A pid recycled for an unrelated process cannot hold a card (its start identity differs), a member whose start identity cannot
be read stays held (a safety check does not turn itself off), and the card frees itself at the first read after its members
are gone, with no clock involved: hours past its declared window and heartbeat a tree-orphan is still held while its members
live (invariant I3, expiry never frees a card). Nothing here kills a process.

**Where the members come from**, in order, and the later sources only while the wrapper is gone and no recorded member is alive:

1. **The record.** The wrapper writes its tree at start and on its 15 s tick, rewriting the record (under the epoch lock)
   only when the set of processes changed. The tree is what the containment lists: on Windows, with a contained command, the job
   object's processes and nothing else (the job is exact, descendants that outlived their parent included; the console host the OS
   starts as a child of a command that has a console of its own is not in it, and recording it could keep a claim for a job that is
   gone); on Linux the process group plus the descendants reachable through live parent links, and for a command that runs
   uncontained the process table below the command is the only source. The wrapper itself is not a member. The record is frozen
   when the wrapper dies: the reader does not consult the process table for a lease that recorded a tree.
2. **A launch marker.** A ComfyUI instance can sit outside the wrapper's job. Its launch marker (`.offload-launch-<key>.json`
   in the ComfyUI directory, armed from `comfy_dir` at config load by `modelaffinity.SetComfyDir`, the one call that also arms
   the marker reader of the seat scoper) names the lease epoch it was launched under; a marker that names this epoch, whose
   process is alive and did not begin after the marker was written (a recycled pid), keeps the claim. A marker that names no
   lease (the default instance) ties only to a lease that recorded no tree, when it began at or after that lease and no other
   lease record exists in the directory (it cannot be told apart from another lease's otherwise); one that began before the
   lease is a leftover and proves nothing. Node restamps a marker through a staged `.tmp` file and a rename over the live name, and a
   marker that is being installed cannot be told: its name is listed and the read ends as a rename in flight, or finds it gone with
   a fresh staged file beside it, or its name is not listed and a fresh staged file stands where it was. That is **unknown**, and
   the reader marks its copy of the record (`treeUnknown`); `Reclaimable` does not free a lease whose wrapper is gone while it is
   set. It lasts as long as the staged file is fresh (3 s at most), and a marker that is unreadable for another reason is skipped
   as before (`TestALaunchMarkerBeingInstalledHoldsTheLeaseOfADeadWrapper`).
3. **The process table, for a lease that recorded no tree** (a wrapper that predates this change). Windows keeps a child's
   parent link when the parent dies, so the children of the dead wrapper's pid that began at or after the lease are found; Linux
   reparents them, so the walk finds none. A wrapper pid that is alive but recycled is never walked: its children are another
   process's.

The driver's per-process compute listing is **not** evidence. Under WDDM it names no process for most of them, and a process
on a card cannot be attributed to a lease by itself; `proctree.PlanStop` reports such a process as `card still busy by pid N
(not ours)` and never touches it.

**The wrapper.** `gpu reserve ... -- <command>` now starts its command through `internal/proctree`:

- *Windows:* the child is created suspended, assigned to a job object and resumed, so it cannot spawn a process that escapes the
  job before it is inside; every descendant joins the job, including a process started detached. If the OS refuses the job (an
  enclosing job that cannot be nested, an access failure) the command still runs, uncontained, and the wrapper says so on
  stderr; a lost lease then stops the command and what the process table shows below it. Under `--die-with-wrapper` the same
  failure is a **refusal** (the suspended child is stopped and the reserve returns an error), because the caller asked for a
  guarantee the command cannot get; running it uncontained would leave a render alive after the wrapper dies.
- *Unix:* the child leads its own session and process group, and the group is signalled as a whole (`kill(-pgid)`). (`Pdeathsig`
  is not used: it is delivered per thread and covers the direct child only.)
- *A lost lease or an interrupt stops the whole tree*, not just the command, **except** a live ComfyUI instance whose launch
  marker names a lease other than this one: a kept instance another lease reused was re-stamped to it, and killing it would kill
  that lease's job. The wrapper says what it left running. (`stopInstancesOfLease` already leaves such an instance alone when
  the lease is not ours; the tree stop follows the same rule.) A signal that ends the wrapper (the console closing, a logoff)
  stops the tree the same way, inside the few seconds the OS grants.
- *Letting go of the tree leaves it running.* If the wrapper exits or dies, the job keeps running and its lease becomes a
  tree-orphan, unless the reserve was started with **`--die-with-wrapper`** (Windows: the job is created kill-on-close, so the
  wrapper exiting or dying ends the tree). It is **opt-in and off by default** because it makes the wrapper a single point of
  failure for a long render. A platform with no handle whose closing ends a tree (Linux) refuses the flag before the reserve
  queues for a card, and so does `--detach` (a detached holder runs no command). **One carve-out:** a ComfyUI instance whose launch
  marker names another lease (a kept instance a later lease reused) is spared by the lost-lease stop, and a kill-on-close job
  would end it a moment later when the wrapper lets go of the job. So once the wrapper has spared a process it ends everything
  else of the tree first, clears the job's kill-on-close, says so (`--die-with-wrapper no longer applies to this command's tree`),
  and only then lets go; the same is done when the wrapper finishes and finds such an instance in the job without the lease
  having been lost on its last tick (`proctree.Tree.DisarmKillOnClose`).

**What a reader sees.** The verdict `tree-orphan` (it leads the precedence; the brief line leads with `ORPHANED`),
`gpu status` (`wrapper: pid N is GONE, but the job it started is still running (pid ...): TREE-ORPHAN`, then the evidence),
`activity.holder` (`tree_orphan`, `tree_pids`, `tree_why`), the sentence a waiter reads (`lease epoch N is tree-orphan: its
wrapper (pid P) is gone but the job it started is still running ...`), and `/fleet/health` (`tree_orphan` on the lease entry
and the singular block, verdict `tree-orphan`; the lease is `busy` like any held lease: inside its declared window it closes its
cards to new work, and past the window it is ranked last and is not a refusal, as every overdue lease has been since 0.113.27,
because dispatch queues behind a lease and a node that refused here would keep the capacity wait from asking the one remote that
could take the work). The host-RAM guard measures what a tree-orphan has loaded from its live members, not from the dead
wrapper's pid, so the declared need of a job that is running is not charged a second time as still to load. The allocator treats a tree-orphan's
cards as claimed, so a `--cards N` reserve is handed other cards, and a seat warm-back does not load over them (it waits for
the last live lease on the seat's cards).

**What ends a tree-orphan.** It ends by itself at the first read after its members are gone. To end it sooner, the one command
that does it today is `local-offload gpu release --epoch N`, and every surface above names it (the takeover verb the other
escalated verdicts point to does not exist in this build and is not offered here). `gpu release --epoch N` ends the claim at
once and stops only the ComfyUI instances kept under the lease; **the rest of the job keeps running on the card**, unfenced, so it
is run by a human who has seen that the job is dead or disposable. Nothing in the harness runs it on its own. What should happen
to an overdue or orphan tree by policy (ask the holder to yield, or hold) is a decision for after this ships.

**The stop primitives a takeover will use** are in this change and have no caller yet. `proctree.PlanStop` decides what may be
stopped (the recorded tree and launch-marker processes, nothing else) and `proctree.Stop` re-checks each member by pid and
start identity right before it signals it, polls until it is gone and returns one outcome per member, so a caller that frees
a claim can require the tree verified dead (`AllGone`). `comfyinst.StopMarker` asks a marker's instance to stop its running
prompt (`POST /interrupt`), drops its models (`POST /free`) and kills the marker's pid, only after the proof a lease's own
holder gives (alive, did not begin after the marker, the endpoint reports exactly the argv the launcher recorded). On Windows
the job's stop confirms on the process handle that a pid it was listed is still a member of the job before it ends it.

**Cross-version (invariant I7).** A reader built before this change ignores `tree` and reclaims a dead wrapper's lease at once,
as it always did (`TestOldReaderStillReclaimsATreeOrphanRecord` runs the preserved old reader against one). The rule holds on a
host where every reader (CLI, MCP server, fleet daemon, a media repo's separate binary copy) is this build; `gpu doctor` lists
the readers older than the record format, and this change does not move the audit's line (it is a reader improvement, and a
reader that lacks it is no worse than before), so **a host is protected only once every reader on it has been restarted on this
build**: after the deploy run `gpu doctor` and cycle the MCP servers, and compare build versions, because the audit's
`FormatSignature` is not bumped and cannot tell. Nothing is written that an older reader misreads: the key is additive.

**Limits, said plainly.**

- The recorded tree is at most one tick (15 s) behind while the wrapper lives, and **frozen when it dies**. A lease that recorded a
  tree is held by that record and by a launch marker that names its epoch; the process table is read only for a lease that
  recorded none. So a process the command started after the last record, and that outlives every recorded member, is not held
  (unless a marker names the lease): the card can be handed on under it. The window is one tick wide, it needs the wrapper to
  die inside it and the recorded members to exit before that process, and it is the accepted limit of recording rather than
  enumerating; a wrapper that dies in its first seconds still has its command recorded (the tree is written right after the
  start).
- A wrapper that is alive but wedged (alive pid, stale heartbeat, window over) is still reclaimed by the conjunction, tree or no
  tree: this change is about a wrapper that is gone.
- A lease taken by the harness's own pipeline (a media call) records no tree. A render child that outlives a crashed server is
  found by the process-table walk on Windows and by the launch marker for a keyed instance; on Linux only the marker.
- A Unix wrapper cannot make its tree die with it (`--die-with-wrapper` is refused there); the tree-orphan rule is the protection.
  Its command also leads its own session, so a terminal hang-up that used to reach the command (it shared the wrapper's session)
  now ends only the wrapper: the job keeps running and its lease keeps the cards as a tree-orphan.
- The wrapper holds the lease exactly as long as its COMMAND runs. A command that exits and leaves processes running (a daemon it
  started) releases the lease at its exit as it always did; those processes are not stopped (a kept ComfyUI instance is, by the
  release) and not claimed. `--die-with-wrapper` ends them with the wrapper on Windows.
- Free-by-observation means the claim ends when the members do, which for a job that has hung is never: that is what
  `gpu release --epoch N` is for. A takeover that also stops the job, and the policy that would run it, are later changes.
- A Windows command that runs in a console of its own (its wrapper has none) is given a console host (`conhost.exe`) by the OS, a
  child of the command that is not in the job. The recorder used to union the job's list with the process table, so it recorded
  that console host too, and a recorded member that is not the job can keep a dead wrapper's claim after the job is gone. That is
  the extra member the first measurement saw; on Windows, with a contained command, the record is now the job's exact list. A
  console host the OS makes for a descendant's own console is in the job and exits a moment after its last client, so a card
  frees a moment after the last real process, never later.

## The fleet reads leases per card (plan P7)

What the lease says about a box used to stop at the box: another node saw one `lease` block, the lowest epoch's, and read the
whole node as spoken for. Now `/fleet/health` carries `leases[]` (one entry per live lease: its cards as lower-cased GPU UUIDs,
absent for the whole node; where they came from; its term; `busy`, `overdue`, `exclusive`, `draining`, `orphaned`, `stalled`; one
verdict word) and each seat's cards as `device_ids`. The delegator fences a node only for a contract whose seats ALL sit on a card a
fencing lease holds, because the node's placement table falls back to a seat whose cards are free, and the node's own closed
reading and text-reservation refusal follow the same cards. The singular block stays, as the worst across the live leases, so a
reader one release behind is never told less than is true; a node that publishes no `leases[]` is read as the whole node. On the
local box the delegator does not dial a seat that a lease it does not hold fences for the contract (the seat's own fence pre-check
would turn the run away): it waits in line, and a wait that ends with nothing taken names the places it stood in. A standalone
deploy still waits for every lease unless the operator names the cards it touches (`node-swap --cards`). The whole account, with
the wire shapes and the limits, is in [fleet-node.md](fleet-node.md), "Per-card lease truth (GPU routing P7)".

## Node interop

**Node does not acquire.** `internal/gpulease` is the only implementation of acquisition,
staleness, fencing and the epoch counter. The Go caller takes the lease and threads
`GPU_LEASE_DIR`, `GPU_LEASE_EPOCH` and `GPU_LEASE_CLASS` down; `withGpuSlot` honours what it is
given.

A GPU job with **no** lease REFUSES, naming the fix, rather than grabbing the card:

```
local-offload gpu reserve --class media -- node render/comfy-generate.mjs ...
```

`--no-lock` remains the deliberate escape hatch for "nothing else can touch this GPU".

### Why the Node implementation was deleted rather than repaired

Two languages independently implementing one concurrency rule produced a **new** divergence in
every review round, and each was individually fixed before the next was found:

| round | divergence | symptom |
|---|---|---|
| 1 | different atomic tokens (dir vs `meta.json`) | both sides held the lease |
| 1 | `os.FindProcess` vs `process.kill(pid,0)` | ACCESS_DENIED read as dead here, alive there |
| 2 | non-atomic epoch write | **24.5%** of concurrent reads saw a torn counter and restarted the fence at 1 |
| 2 | no claim-freshness grace on one side | Node deleted Go's in-progress claim |
| 3 | a third staleness rule in `gpulock` | a live 3-hour holder read as free |

The defect was never any single bug — it was the duplication. What Node keeps is what is
genuinely its job: honour and **fence** against an inherited lease, elect **one** unloader per
lease, **drain** before unloading, and run the ComfyUI lifecycle.

The heartbeat lives in a per-epoch `hb.<epoch>` file rather than in the record, so a stale holder
renewing after a takeover updates something nothing reads. Read-only consumers call
`gpulease.InspectDir` rather than re-deriving the judgement — a second reader is how this
diverges, and it did: when the heartbeat moved, a record-only view briefly called a live,
renewing holder stale the moment its declared window lapsed.

## Draining a seat before a window (0.113.16; rebuilt 0.117.0)

> **0.117.0 (ADR 0041, register D-93).** The paragraph below describes the mechanism as it shipped in
> 0.113.16–0.113.20; three things changed on 2026-09-14 and are described in the next section: the drain's
> deadline is now the queue budget (`--drain-timeout` defaults to the rest of `--wait`, floor 2 min); the
> lease is stamped `draining` during the drain and `exclusive` only after it; and the drain waits for the
> REGISTERED RUNS on the seat, not only the engine's gauge.

```
local-offload gpu reserve --class text --for 30m --reason "arm B" --drain [--drain-timeout 2m] [--unload-seat] -- <command>
local-offload gpu reserve --class text --for 30m --reason "arm B" --detach --drain --unload-seat
local-offload gpu release --warm-seat
```

**Alias-bound seats (0.113.20).** llama-swap's `/running` lists CANONICAL ids, while `agent_model` is normally an alias (`agent-pool` → `qwen3.8-27b-vllm`). The drain's reader matched `/running` by the configured name, so on an alias-bound seat it read "not loaded" and returned at once — a silent no-op from 0.113.16 to 0.113.19 on the reference workstation (<node-c>, whose seat is bound by its id, drained correctly, which is why the live proofs passed). The reader now lives in `internal/seatload` and resolves the name through the roster before consulting `/running`; an unreadable roster falls back to the bare name.

Taking a text lease already makes the node a non-target (health `lease`, dispatch 503, the delegator's gate), but work placed
before the lease can still be in flight. `--drain` waits, after the lease is taken, until the agent seat reports nothing
running or waiting: llama-swap's `/running` is read first — an unloaded seat is idle by definition, and its `/upstream/<model>/…`
path is NEVER probed on an unloaded seat because that path loads the model on demand — then the loaded seat's own gauges
(`vllm:num_requests_running|waiting`, `llamacpp:requests_processing|deferred`) read at the seat's own address — the
`proxy` llama-swap's `/running` reports for it, never `/upstream/<model>/metrics`, because llama-swap counts every
`/upstream` request as activity and a polled read there would keep an idle seat loaded — two consecutive zeros required.
The seat address is read where llama-swap runs: a loopback `proxy` stays loopback when the endpoint is this machine (by
loopback, hostname or an address on one of its interfaces); behind a llama-swap on ANOTHER machine it is re-pointed at that
host, which reaches only a seat bound to a routable address there — the reference seat binds 127.0.0.1, so that drain
reports `seatload.ErrRemoteSeatUnreachable` and fails at the deadline instead of calling the seat idle. A llama-swap whose
`/running` carries no `proxy` is reported the same way (`ErrNoSeatAddress`: upgrade llama-swap). A llama.cpp seat started without `--metrics` answers that path `501` (older builds `404`); since
0.113.19 the drain then reads llama-server's `GET /slots` and counts `is_processing` slots — llama-server's deferred queue is
not listed there, and the two-consecutive-zeros rule is what covers it (a queued request becomes a processing slot the instant
one frees). Any other non-200 from `/metrics` is still "could not read", never idle: the drain fails at the deadline. At
`--drain-timeout` the wrapper form releases the lease and exits non-zero; the detach form keeps
the lease (the card stays reserved, work keeps routing elsewhere) and exits non-zero so the caller does not start.
`--unload-seat` (requires `--drain`) then frees the cards through `POST /api/models/unload/<model>` (legacy `GET /unload`
as the fallback): the agent seat, and every other model `/running` lists, except the config's `memory_stack` (the mem0
embedder and reranker). The stack stays resident and the run prints `kept the memory stack resident`. The operator's
rule is that mem0 never yields. On the three-card reference box it sits on the utility card, so unloading it freed nothing a render could use; on a single-card tier it shares the render card (register
C-87, 2026-10-01). `render/gpu-lock.mjs` keeps the same set. An empty `memory_stack` means the default set: `embeddinggemma`,
`bge-reranker-v2-m3`, `embeddinggemma-ams`, the id the memory authority node serves its embedder under (register A-122b:
the first two did not name it, so a lease cleared it), and `embeddinggemma2`, the EmbeddingGemma-2 entry the memory stack is
moving to (appended 2026-10-09; the serving templates render it on the tiers that set `include_embeddinggemma2`, with its
projector on the memory authority's tier and text-only on the replicas). A name the
box does not serve is inert, and `internal/config` and `render/gpu-lock.mjs` carry the same list, kept equal by a test. A non-empty list replaces the default rather than adding
to it, and the installer's seeded `config.json` names the first two, so a node whose embedder or reranker has another
model name must list every member (the tiers that render `embeddinggemma2`, ampere-6, ampere-8 and blackwell-3x16 (the last
two text-only), seed `memory_stack` with it in `config_seed`, projector or not; the template stays at two entries because `llamaswap bind check` reports a listed name
the roster does not serve as dangling): an unlisted one is unloaded by `--unload-seat` like any other resident model, and by
the render free step like any other tier. Fleet reclaim (`fleet_reclaim.go`) keeps the same set: a loaded `memory_stack`
member (the default set when the list is empty, read by the same `effectiveMemoryStack` as `--unload-seat`) is baseline, never
reclaimable capacity, whatever its ttl is, so the house rule's 300 s idle ttl no longer makes the embedder reclaimable there. It
is checked by name before the keep-set (llama-swap's ttl -1/0 seats) and also when no keep-set could be read (register C-94,
2026-10-02; before that fix this was a known gap). If the
per-model route fails, the legacy `GET /unload` is used only when no stack member is resident. It unloads everything,
whatever `?model=` says, so when the stack is resident or `/running` cannot be read, the reserve fails and names the
stack. The wrapper form warms the seat back (`GET /upstream/<model>/health`) BEFORE releasing, so the first
contract placed here again finds a loaded seat, unless its command was ended on `SIGTERM` (C02): that is how a console
close, a logoff and a shutdown reach a Go program on Windows (a plain kill sends the same signal, so the wrapper says the
cause is likely, never known), and the OS ends the process about five seconds later, so
a reload of minutes inside that grace would stand in the warm-back with the lease held while the machine goes down. The
lease is released right after the ComfyUI instances kept under it are stopped (a step bounded at 60 s, usually
seconds), the wrapper says the warm-back was skipped, and the warm stays owed (the marker below;
`gpu release --warm-seat` or the seat's next request pays it). A signal that arrives earlier, while the wrapper is queued
(or queued again after a lease lost during the drain), choosing cards, draining, or between the end of the drain and the
command's start, is answered too (the handler is registered before anything else, `pair-workloads.md`): the
queue wait ends (`Options.Cancel`), the lease is released, the command never starts, the seat is not warmed, and the wrapper
claims a warm owed only when the seat was unloaded (and then says the wrapper was ended at that stage, never that a command
was: none ran). A signal that cancels the unload request itself leaves the warm owed
whenever the seat was resident, because llama-swap may have finished the unload the client stopped waiting for (a warm of a seat
that is in fact still loaded is one health request). A Ctrl-C has no such deadline and still warms. A second signal is not acted
on: the OS ends the process after its grace for a console close, a logoff and a shutdown only, so a second Ctrl-C or a plain kill
during a stage that never consults the signal (the plan's card-table read, up to 20 s; stopping the kept ComfyUI instances, up to
60 s; the seat's warm-back after a Ctrl-C) waits for that stage to end. The detach
form's counterpart is `gpu release --warm-seat`. That request
is the load, so a llama-swap reload or restart can interrupt it: the warm re-sends it for up to 60 s (see "A warm-back
interrupted by a llama-swap reload is re-sent" below), and the owed marker is cleared once the seat is observed loaded.

**The warm-back belongs to the LAST holder (0.129.2, register D-124).** On 2026-09-18 02:57 a wrapper whose command
had been cut warmed the seat while the next queued lease had already taken the card, drained an "idle" seat and unloaded
it; the unload killed the engine, the seat unit's `Restart=on-failure` brought it back 20 s later on the new holder's
exclusive card, and three measurement rows read the seat's 10 GiB as their own fit. Three rules now order the hand-off:

- **A queued `Acquire` is visible.** While it polls it keeps a record under `<state>/gpu/waiters/` (pid, class, reason,
  since; pruned on read when the pid is gone); `gpu status` lists them as `queued:` and `offload_status` carries the count.
  `meta.json` stays the sole arbiter of who HOLDS the card — a stale or missing waiter record can never grant
  possession, only affect who is allowed to TRY next (see FIFO below) or defer one warm to the next holder.
- **The queue is FIFO (register D-13x, 2026-09-22).** Measured live 2026-09-22: a text reservation queued at
  ~18:40 was still waiting at 20:24 while two media reservations — one queued seconds after it (~18:40) and one
  25 minutes later (19:06) — each took the card ahead of it — every waiting process polled `TryAcquire` once a second with no ordering between them,
  so whichever process's poll tick landed first after a release won, and a waiter could lose that race
  indefinitely. Each poll now checks whether the caller is the OLDEST live waiter recorded under
  `<state>/gpu/waiters/`; only that one attempts the claim, so the instant the holder releases, the front of the
  line takes it uncontested by the others. A dead waiter's record is pruned on read (as before) and never blocks
  the line; a waiter whose own `--wait` expires removes its record and leaves the line for whoever is behind it.
  Class carries no priority in the queue — text and media waiters interleave in pure arrival order; no ADR
  documents a queue-level class priority (0026 gates text LOADS behind a media lease, 0041 sizes the drain
  budget, neither says anything about acquisition order). This governs ordering among REGISTERED waiters, and
  since 0.178.0 every production claim is one: `Acquire` registers BEFORE its first attempt, with or without a
  `Wait` (a zero `Wait` is one gated attempt), so a fresh claim no longer lands in the window between a release
  and the front waiter's next poll. The one door that still skips the line is a bare `TryAcquire`, which setup
  and tests use and no production path calls.
- **Accepted residual — a forward wall-clock jump.** A waiter's heartbeat is its record's mtime, compared with
  the reader's wall clock (file times carry no monotonic reading). A forward clock step larger than the staleness
  window (10× the poll interval, floor 15 s) — an NTP correction after a laptop resumes, a manual clock change —
  makes every live waiter look stale for ONE poll, so on that tick they all try the claim as they did before FIFO.
  It is bounded and self-healing: `meta.json`'s O_EXCL claim still grants exactly one holder, the refreshed
  records restore the order on the very next poll, and no waiter loses its place (its `SinceMs` is unchanged).
- **A waiter must keep proving it is still polling, not merely alive.** Pid liveness alone cannot tell "queued
  and actively polling" from "queued, still alive, and never polling again" — a suspended process, one wedged in
  another goroutine, or an OLDER harness binary whose `Acquire` loop exited without unregistering (it only ever
  raced `TryAcquire`, so a queue-ordering bug in the old code cost it nothing there, but would make its leftover
  record an unbreakable head of the new FIFO line). Every poll, win-or-lose, a waiter re-stamps its own record's
  mtime (beside-then-rename-over, the same Windows-safe pattern as `Renew`/`Restamp` — this file is read by every
  OTHER waiter on every one of ITS ticks). A reader treats a record not refreshed within 10x the poll interval
  (floor 15 s) exactly like a dead pid: skipped for ordering, pruned best-effort. This is also what keeps a
  MIXED-VERSION rollout safe — an older binary's record is the identical JSON shape (no schema change), so a
  newer reader parses it fine, but the older code never refreshes it, so it goes stale on the same clock and
  stops affecting anyone's order; it cannot wedge the line, it just keeps racing exactly as it always did. Pid
  RECYCLING is covered the same way the lease holder itself is: `StartTimeMs`, stamped from `procStart` at
  registration, is compared against the current process behind that pid on every read, so a pid handed to an
  unrelated process reads as dead. Same-process concurrency (two goroutines in one process each calling `Acquire`
  independently — not the in-process `mediaSlot` path, which never touches `<state>/gpu/waiters/` at all) shares
  one real pid across two+ waiter records; a same-millisecond tie resolves to exactly one front-of-queue via the
  random-token filename tie-break, never both (a livelock) and never neither.
- **A waiter is kept for the wait it declared (0.178.0).** Its record carries `deadline_ms` (the registration time
  plus the `--wait` it passed). The flat 12 h debris cap — a live, heartbeating record older than the longest
  default wait is stuck, not queued — still judges a record that declared none (a seat admission, an older binary's),
  but a `gpu reserve --wait 20h` is kept to its own deadline plus an hour of slack. It used to be reaped at hour 12,
  and its next heartbeat re-created a record the next reader reaped again, so it stood outside the line for the last
  eight hours of a wait it had been told it had.
- **A request that cannot take a place says so (0.178.0).** The gate is fail-soft: a record that could not be written
  (an unwritable `waiters/` directory) answers "front of the queue", so a bookkeeping fault never refuses GPU work.
  That made the loss silent: the request claimed like a bare claim, ahead of waiters it should queue behind. It now
  prints one warning on stderr per process, and the claim itself is untouched.
- **The heartbeat itself needed the SAME read-side retry the write side already had.** Once every waiter started
  rewriting its own record every poll tick, `-count=10` caught a real, non-jitter race:
  `Waiters()`'s plain `os.ReadFile`/`os.Stat` had no retry, and on Windows a read can transiently fail while a
  concurrent rename is in flight over the same path (the same class of ephemeral error `renameReplacing`/
  `removeClaim` already retry elsewhere). Treating that as "this waiter isn't here" silently excluded a live,
  correctly-refreshing, genuinely-earlier waiter from ONE reader's view — one unlucky tick was enough for a
  later-arrived waiter to see itself, wrongly, as front-of-queue and win the race (measured: a diagnostic
  confirmed the recorded `SinceMs` values stayed perfectly ordered every time — the algorithm, not the clock, was
  the defect). Worse for the `os.Stat` call specifically: the code deleted the file on ANY stat error, not only a
  confirmed absence, so a transient failure could permanently destroy a live waiter's queue position rather than
  merely skip it for one read. `readWaiterFile`/`statWaiterFile` now retry a transient failure (the same budget as
  `removeClaim`) and return `os.IsNotExist` immediately unretried, and `Waiters()` only prunes on a CONFIRMED
  absence or a confirmed stale/dead/recycled record — never on a retry-exhausted transient error.
- **An unload stamps `<state>/gpu/seat-warm-owed`**, and a warm-back runs only when (1) the card is still ours — the
  wrapper form checks its own epoch (`Lease.Check`), `gpu release --warm-seat --epoch N` checks the record is still N —
  and (2) nobody is queued behind us. With a waiter the warm is skipped and said so (`NOT warming … back: N lease(s)
  queued`): the successor unloads the seat again anyway, and the marker makes the LAST releaser pay the warm. A holder
  that lost the card (an operator `gpu release`, a reclaim) never warms; the marker stays for the next last holder, and a
  plain `gpu release` prints a note when a warm is owed.
- **With card-scoped leases, (3) no other live lease sits on the seat's cards.** Several holders share a box, and the
  warm loads the seat on ALL its cards: the first of three per-card renders to finish would load the 3-card agent seat
  over the two cards still rendering. The warm is skipped and said so (`NOT warming … back yet: <class> lease epoch N on
  cards … still sits on its cards`) and stays owed; the next holder that warms (a later `--unload-seat` wrapper, `gpu
  release --warm-seat`) pays it, else the seat loads on its next request. The seat's cards are its declared pins
  (`modelaffinity.ScopeToModel`); a seat that declares none is on every card. A lease on a card the seat does not use
  leaves the warm alone. `gpu release --warm-seat` with no epoch counts the whole box, as the release does: one live
  lease is the one being released, more than one and the release refuses, so the warm does not run either.
  **Known limits, both in the cold direction:** two leases ending in the same instant can each see the other and both
  skip, and a last lease that ran without `--unload-seat` never warms; either way the seat stays cold until its next
  request (an idle seat unloads after 5 minutes anyway, so a warm-back only saves that one load). Tests:
  `gpu_warm_lastholder_test.go`.
- **The warm is heartbeat for its length** (`drainRenewEvery`, 15 s), so a 27B load of several minutes cannot go stale
  under the 120 s heartbeat TTL; losing the lease mid-warm cancels the request and is reported, while a heartbeat
  write that fails with the lease still ours is reported once and retried on the next tick (register C-59).
- **A warm-back interrupted by a llama-swap reload is re-sent (0.177.0).** The warm's health request IS the load, and
  llama-swap started with `-watch-config` reloads on ANY write to its config: it builds a brand-new server (every model
  cold; no running process is adopted), swaps it in, and only then shuts the old one down. The request parked in the old
  router is answered HTTP 500 with a body saying `<router> is shutting down` (the router is `matrix` or `group`) while
  `/running` already speaks for the new server, empty. A full restart instead drops the connection, or whatever fronts
  llama-swap answers 502/503/504. The warm used to read that empty `/running`, call the seat "not loading" and give up,
  leaving the seat cold with the marker standing. It now tells the interruption from a failed start by what the server
  said (`warmSeatGated`, `gpu_drain.go`):
  - *Retried:* a transport error other than a timeout (not the caller's cancellation, not any deadline: the client's
    own, or a dial that timed out), a 502, 503 or 504, and a 500 whose first 512 bytes hold ` is shutting down` or whose
    body was cut short by such a dropped connection (the old router dies while it writes the answer, so the words never
    arrive; the read error is kept and named in the failure line). The first one opens a recovery window of 60 s
    (`warmReloadGrace`). Inside it, whenever `/running` reads the seat cold (or unreadable, the server being down), the
    load is re-sent after a back-off of 1, 2, 4, 8, 8... s (`TestWarmBackoffDoublesUpToItsCap`). While the seat is
    `starting` (another client's request is already loading it on the new server) the warm waits and never sends a
    second load, and one `/running` poll that fails in the middle of such a load does not end the wait: the watch keeps
    waiting for a seat it last saw starting, recovery or not. Any further 5xx or transport error other than a timeout
    inside the window is part of the recovery too, unless its body says the start died (next bullet). Past the window
    the warm fails with `status N (<snippet>) and the seat is not loading; no recovery within 1m0s of a llama-swap
    reload/restart` (`and the seat's state could not be read` when `/running` never answered).
  - *Not retried, inside the window or outside it:* an answer whose body says the start died, `unable to start process`
    (the 502 `unable to start process: upstream command exited prematurely` this repo records for a failed start) or
    `upstream command exited` (the same words in a 500), whatever the status code. It is a refusal, not a server going
    away: it neither opens the window nor is re-sent, because a re-send is a second engine launch while the lease stays
    held and a successor waits
    (`TestWarmDoesNotRetryAnAnswerThatSaysTheStartDied`, `TestWarmStopsAtAnAnswerThatSaysTheStartDiedEvenInsideTheRecoveryWindow`).
    The wording lives in `seatwait.StartFailed`; the contract path's classifier reads the same death marker. A seat that
    another client is loading outranks the body: the warm watches it.
  - *Not retried either:* a bare 500, or a 5xx other than 502/503/504, over a seat that is not loading while no reload
    has been seen. That is how llama-swap reports a start that failed, and re-sending it is a second failed load, so it
    fails at once, as before (`TestWarmDoesNotRetryAPlainFiveHundredWithNothingLoading`); once a window is open the same
    answer is part of it, unless it says the start died. A timeout is not retried even inside the window, whether the
    client's own deadline or a dial that gave up: it is a load that outlasted the client, or a connection that never came
    up, and the final reading below decides what it was.
  - *Re-checked before each re-send:* the guards the warm started under (the card is still ours, nobody is queued behind
    it, no other lease sits on the seat's cards) are read again before EVERY re-send, with the same sentences. A refusal
    stops the retry, prints `warm-back of <seat> stopped during its retry` (plus `; the warm stays owed` when a marker is
    owed), and leaves the marker where it is: a re-send after a successor queued is the warm landing on somebody else's
    lease (register D-124).
  - *Said:* the first re-send prints one line on stderr (`gpu: warm of <seat> hit a llama-swap reload or restart ...`),
    and every failure names the status and the first 120 bytes of the answer, which the old message left out.
  - *A status below 500 is no longer success by itself.* A 404 "model not found" (the config edit renamed or removed the
    seat), a 409 or a 429 used to print `warmed back` and clear the marker over a seat that never loaded. A non-2xx
    answer below 500 is now decided by one `/running` read: loaded and ready is a warm, `starting` is watched like any
    load, anything else is the failure, naming the status and the snippet, and it is never retried. A 2xx is the load and
    stays success.

  After a warm that failed, one last `/running` reading decides whether the failure stands: when the seat is loaded and
  not starting AND the guards above still allow it (they are read once more before the marker is touched, because the
  reading takes a moment and the heartbeat that would notice a lost lease ticks every 15 s), the tool prints `the health
  request failed (...) but the seat is loaded; treating it as warmed`, clears the marker (a compare by seat name,
  `gpulease.ClearSeatWarmOwedIfSeat`) and reports `warmed back`. A warm cancelled because the lease was lost is
  excluded: the card is no longer ours, so a reading taken after that proves nothing. A seat that reads loaded while a
  guard refuses says so (`the seat reads loaded, but the marker is left alone (<the guard's sentence>)`), and a
  confirming read that itself fails says `could not confirm the seat's state (<error>)`. The failure line ends by saying
  the warm stays owed, and that `gpu status` clears the marker once the seat is observed loaded, only when a marker is
  owed: an explicit `gpu release --warm-seat` over a seat nothing was owed to has no debt to keep. Tests:
  `gpu_warm_reload_test.go`, `gpu_warm_watch_test.go`.

  **Known limits.** A retried warm holds the lease longer: up to the 60 s window plus the load (and the 15 minute watch of
  a load that outlasts llama-swap's health wait), so a successor queued behind the holder waits that long; the heartbeat
  runs across the back-off (`TestTheWarmBacksHeartbeatRunsAcrossARetriedWarm`). A transport error other than a timeout is
  retried, so a llama-swap that is simply down now holds the lease for the window before the warm fails (it used to fail
  at once). The retry class keys on llama-swap's message text: if upstream rewords ` is shutting down` the 500 leg
  silently reverts to failing at once, while the 502/503/504, transport and final-reading legs still apply, and the
  snippet in the failure makes the drift visible. The cost of not re-sending a start that died: the new server's first
  start can fail because the old process still holds its port or card, and that answer reads like any failed start, so
  the warm fails at once and the marker stays; the seat then loads on its next request or at the next warm, instead of the
  warm relaunching an engine that may be broken for good.

  > **Unverified:** the mechanism is read from llama-swap's source, not observed against a running instance, and the
  > body of the 500 in the incident that prompted this change was never captured. What the source says (upstream's
  > main branch at its latest release, v262, read 2026-10-09; the build deployed here was not checked against it):
  > `llama-swap.go`'s reload handler assigns `activeSrv = newSrv` (the new server is swapped in), then calls
  > `old.Shutdown(shutdownTimeout)`, and only then `proxyLog.Info("configuration reloaded")`; the routers in
  > `internal/router/base.go` answer a request they were holding with `fmt.Errorf("%s is shutting down", b.name)`, which
  > `internal/swaputil` maps, for an unclassified error, to HTTP 500 `unspecific error: <err>`. A failed start is
  > reported by `internal/process/process_command.go` as `upstream command exited prematurely` or `health check timed out
  > after ...`. The `unable to start process:` prefix of the 502 is the shape this repo's tests record (the contract
  > path's tests use it as a refused warm-up), not something found in that source. Whether a re-send during the old
  > server's teardown can clash with the old process still holding its port or card was not measured; the back-off and
  > the window are the mitigation.

- **`seat_warm_owed`: what the marker means and who clears it (0.177.0).** `<state>/gpu/seat-warm-owed` holds
  `<seat> <RFC 3339 time>` (whole seconds, UTC) and means "this seat was cleared for a lease and nobody has loaded it
  back". An unload stamps it (`gpu reserve --unload-seat`, the detached holder's included). `gpu status` prints it as
  `seat warm-back owed:` and carries it as the `seat_warm_owed` key of `--json` (a seat name, or `""`); `offload_status`,
  the plain `gpu release` note and the wrapper's skip-unless-owed check read it too. Three things clear it: a warm that
  finishes, a warm that fails but whose seat is then observed loaded (above), and `gpu status`, which removes a marker
  it can prove stale. Before this, only a successful warm cleared it, so a warm that failed while the load went through
  anyway, or any client's request that loaded the seat, left a debt that made the next `--unload-seat` wrapper warm a
  seat that was cold when its lease began.

  What `gpu status` needs to remove the marker (`warmOwedIsStale`, then `gpulease.ClearSeatWarmOwedIfStale`): no live
  lease holds the card, the activity read agrees, and the seat the marker names (compared case-insensitively) is read
  loaded, settled (not starting or stopping) and without a read error; several live card leases keep the marker, since
  `info.Held` is true whenever ANY lease is live. Then, at the moment of the remove, the marker must have been stamped
  before status began its readings (the stamp is whole seconds, so a marker from the second the readings began stays, as
  does one with no stamp that parses), no lease may be live right then, and the record must still be the one it read. The
  readings take as long as the whole activity snapshot (a lease read, a seat read, a GPU sample), so a lease that takes
  the card, unloads the seat and stamps a fresh marker for the same seat while status is still reading would otherwise
  lose it to a compare by name, the one agent seat making every such marker match. When the clear declines, status
  reports the marker as it is now. When it clears one it says so, once: `cleared the stale warm-back marker for <seat>:
  the seat is loaded and no lease holds the card`, and `--json` carries `seat_warm_owed_cleared: "<seat>"` on that run
  only (the key is absent otherwise, and `seat_warm_owed` is `""` after the clear). `gpu status` is already a write in
  the read path (it stamps the orphan marker, see the ownership section), and `offload_status` stays read-only: it never
  clears, so the two views can disagree until a `gpu status` runs.

  **Known limits.** The clear fires only if a `gpu status` run lands while the seat is loaded and no lease holds the
  card. The house idle ttl is 300 s, so after a seat loads that window is at most about five minutes; once the seat has
  idled out, a stale marker survives with the seat cold (`maintainSeatScoped`'s `!wasLoaded` branch leaves the marker
  alone), and the next `--unload-seat` wrapper over that cold seat passes the skip-unless-owed check and warms it at
  release: the 2026-09-23 outcome (a 27B up on all the cards after a render), silently. Only a warm clears it by then
  (`gpu release --warm-seat` does, by loading the seat). What remains of the status race is the gap between the clear's
  last re-read of the marker and its remove, which a lease cannot cross without first unloading the seat over HTTP; the
  price of losing it would be one skipped warm, and the seat then loads on its next request. Tests:
  `TestGPUStatusClearsAnOwedMarkerForASettledLoadedSeat`, `TestGPUStatusKeepsAnOwedMarkerUnlessTheCardIsFreeAndTheSeatIsSettledLoaded`,
  `TestGPUStatusKeepsAMarkerStampedWhileItWasStillReading`, `TestClearSeatWarmOwedIfStale`.
- **Do not edit llama-swap's config inside a lease and release in the same breath.** With `-watch-config`, ANY write to
  the config reloads llama-swap, and a reload unloads EVERY model, the memory stack's embedders included, because the new
  server adopts no running process (see the note under the reload bullet: read from source, not observed). The retried
  warm loads only the agent seat; everything else stays cold until its next request. Wait for llama-swap's
  `configuration reloaded` log line before `gpu release --warm-seat`: upstream logs it (`proxyLog.Info`, `llama-swap.go`)
  after the old server's shutdown has returned, and `reloading configuration` when the reload starts. Both strings were
  read in upstream's main-branch source (release v262), not against the deployed build, so if the line never appears,
  check the build's version before waiting longer.
- **A queued `--unload-seat` acquire finds the card empty because of the ORDER, not because it waits (register D-124
  clause b, validated 2026-10-01).** The live failure of 2026-09-19 — after `gpu reserve --unload-seat` the seat was
  still loaded, the previous holder's deferred warm-back having landed between the new holder's unload and its first
  load — is the unordered warm this section fixed. The orderings that stand today, each pinned from the acquirer's
  side (`gpu_acquire_warm_test.go`: what the acquirer's command finds when it starts, and whose lease was held when
  the warm landed):
  1. *Wrapper form.* The warm runs before `Release()`, under the holder's lease, heartbeat for its length. A queued
     acquirer is either seen as a waiter (the warm is skipped; it belongs to the last holder) or waits for the
     release that follows the warm, so its drain and unload run after the warm settled. A warm whose health request
     is answered 5xx while the load carries on is watched to completion under the same lease, and one interrupted by a
     llama-swap reload or restart is re-sent under it for up to 60 s (the reload bullet above), so the order holds.
  2. *Owed, not in flight* (a holder lost its lease before it could warm): the marker stays; the next
     `--unload-seat` holder drains a cold seat, runs its command on the cleared card, and pays the warm at its own
     release, never ahead of its command.
  3. *`gpu release --warm-seat --epoch N` with the detached holder alive:* the warm runs under that holder's lease and
     the release follows it.
  4. *`gpu release --warm-seat` on a FREE card* (the detached holder was started with `--release-at-expiry` and its
     `--for` window ended first, or it was released by someone else): nothing holds the
     card, so the warm runs unleased and an acquirer is not ordered behind it. This is the operator-explicit path and
     is left as it is; the acquirer's drain still waits out a load that llama-swap lists as `starting`
     (`TestDrainWaitsThroughAStartingSeatWithoutTouchingTheUpstream`), so what is unordered there is only the
     moment before llama-swap lists the load.

  There is deliberately NO acquire-side wait: the releasing lease already outlives its warm, and a second wait on top
  of it would only double the queue.
- **A drainer that loses its lease queues again instead of dying at the restamp (register C-59).** The wrapper form
  used to keep draining a card it no longer owned, die at the restamp with `stamping the lease after the drain:
  restamp: the lease is gone` (or `fenced out`), and never start its command. Now the drain's heartbeat ends the
  drain the moment the lease is confirmed gone (`maintainSeatCtx` takes a context; the restamp comes before the
  unload, so a drain that lost its lease never reaches the unload), the reserve says `the lease was lost during the
  drain`, releases what is left, and takes its place in the line again with what remains of `--wait` (never under
  the 2-minute drain floor; `--wait 0` stays one try, and a card another holder has then fails with that holder
  named). It drains and clears the seat afresh under the new lease and only then runs its command; after five
  re-queues the next loss gives up loudly. A seat fault with the lease still ours (a drain that misses its deadline) is not a
  loss and is returned as before. The heartbeat itself ends only for a lease that is actually gone: one failed
  heartbeat write with the record still ours is reported once and retried, where it used to end the loop and leave a
  multi-hour drain without a heartbeat. The detach form cannot re-queue (a lease it lost is gone: released by an
  operator, or with `--release-at-expiry` let go by the holder at `--for`), so its error now says whether the lease is
  still held instead of always pointing at `gpu release`.

**A failed drain is not a cordon, and a stuck run is not a wait (register C-50).** A drain that misses its deadline
clears the `draining` stamp before returning — the detach form keeps the lease held and non-exclusive, so new runs are
admitted again instead of the seat staying cordoned for the rest of the window. And the overall deadline (the queue
budget) is joined by a no-progress bound: when the busy state — in-flight count, registered runs at the same step and
phase — has not changed for two seat turns plus the cold load (from the seat's own rate sample, floor 2 min, disabled
without a sample), the drain gives up as stuck and says so, distinct from the deadline error; a step advance, an
in-flight change or a load finishing resets it. `--drain-timeout` still wins as a fixed window.

The seat unit template no longer restarts on failure (`Restart=no`): `vllm-seat-cmd.sh` detaches the moment the unit's
invocation changes, so a systemd relaunch is a seat llama-swap does not track and no lease can order. A crashed seat is
reloaded by llama-swap on the next request, which the lease gate orders like any other load.

Why: a gate that unloaded the production seat by hand collided with a delegation that made llama-swap reload it mid-profile
(`No available memory for the cache blocks`, 2026-09-06 15:23), and <node-c>'s measurement windows stopped its fleet node
outright, cutting in-flight remote work. With the lease advertised and enforced, the window is a lease, not an outage.

## The drain waits for runs, inside the queue budget (0.117.0, ADR 0041)

What went wrong on 2026-09-14, in one line each: the drain's fixed two minutes could not outlast one
legitimate 27B step (3m27s at 23.6 tok/s); `--unload-seat` stamped the lease exclusive at acquire, so the
admission gate blocked the very run the drain was waiting for; and the engine's gauge reads zero between a
run's steps, so the third attempt unloaded the seat in that gap and the run's next step died on its wall.

**Deadline.** `--drain-timeout` defaults to the rest of `--wait`, measured from when the reservation began
queueing, never under two minutes; an explicit value wins. A reservation queues behind in-flight work
exactly as it queues behind a holder. The deadline error names what was in flight, the seat's own turn
arithmetic from `seat-rates.json` ("one seat turn is ≈ 174 s at 23.6 tok/s"), and how to wait longer.
Work in flight is never interrupted.

**Stamps.** With `--drain` the record carries `draining: true` from acquire until the seat is idle, then
`Lease.Restamp` turns it into `exclusive: true` (when `--unload-seat` or `--exclusive` asked for it) in
place, under the epoch lock, without moving the epoch. `draining` CORDONS the seat: `modelaffinity.BlocksNewRun`
refuses a NEW agent run — the launcher holds at the cordon for its admission budget (never inside its wall)
and defers `capacity` with the holder's reason — while `blocksLoad`, which every request passes, is
unchanged, so runs already in flight finish their steps. `--exclusive` without `--drain` stamps at acquire as
before. Since 2026-09-22 the same holds for `--class media --drain`: the record used to drop the draining
stamp on a media lease, so the media class fenced the runs in flight from acquire and the drain waited on work
it was itself blocking. A draining lease of either class blocks no load; the media class fences once the
drain clears the stamp.

**Runs.** Every agent loop launcher registers its run in `<state root>/gpu/activity/` BEFORE admission
(seat, kind, origin, goal excerpt, phase, step, tokens; updated per step and every 15 s; removed at the end;
stale = dead/recycled pid or heartbeat > 120 s, swept by readers). A run is one record,
`run-<pid>-<unix nanos>-<seq>.json`: the process-wide sequence keeps two runs begun in the same clock tick apart
(a coarse clock, Windows' 0.5-1 ms steps, once gave them the same id, and the second record overwrote the first, so
the seat's run count read low; register C-82). The drain is done when the engine's gauge
AND the registry are empty on two consecutive reads; a seat listed `starting`/`stopping` counts as busy and
its upstream is never probed. Registering before the warm-up is also what puts the load path behind the
fence: the pre-0.117.0 warm-up loaded the seat straight past an exclusive hold.

**Reading it.** `gpu status [--json]` and `offload_status.gpu_lease` carry a one-word `verdict` and an
`activity` block:

| verdict | meaning |
|---|---|
| `working` | a request or a registered run is in flight on the seat (lease held or not) |
| `held-working` | a lease is held, the seat is idle, and the cards are busy under it (≥ 15 % utilization) or its progress file is advancing — the holder's own job. With only card utilisation as evidence the note says so (`util only, no progress contract`): a live job and a hung one look alike. A lease whose progress file is advancing reads `held-working` with its cards quiet: a batch between two pictures is quiet by nature, and the note says so (F38) |
| `held-idle` | a lease is held and NOTHING is running: seat idle, cards quiet — the holder is waiting (a drain, a queue), loading, or stalled. When busy cards exist that are not the holder's work, the note names each for what it is: **the display card** only for a card that is driving a screen right now (`display_active` in the card table; never a card's index), and a card **outside this lease's cards** for any other busy card (a seat, another lease, another process). That is narrower than the placement rule: the allocator and `gpu cards` also mark a card display when a monitor is attached to it and its screen is asleep (`display_attached`), so a busy card of that kind outside the lease reads "outside this lease's cards" here while `gpu cards` marks it display; both are true and neither calls the load the holder's. It used to call whichever excluded card was busiest "the display card" (F38). A lease with no progress contract that is running a render helper also says that its cards are quiet between two pictures too and that it declared no contract that would say which this is |
| `held-stalled` | the lease carries a progress contract (`--progress-file` and `--stall`) and the file did not move inside its window. The holder is alive and heartbeating; nothing is reclaimed or killed |
| `held-orphaned` | an ATTENDED lease whose owner (session or process) has been gone for longer than `gpu_orphan_grace_min` (default 15 minutes): nobody is expected back for it. Never produced for an unattended, remote or unknown-owner lease. Nothing is reclaimed or killed |
| `held-overdue` | the declared window ended and the holder is still alive and heartbeating. Informational: a declared window is not a ceiling for a live holder, so nothing is reclaimed. A holder whose owner vouches for the job renews its term instead (see [Terms](#terms-a-window-is-a-term-and-a-term-ends-in-a-renewal-or-a-label-adr-0070)); when it does not, the lease is labelled **expired** and the note says why (the `--for` default is 45 minutes, so a wrapper that never declared a window and has no live owner reads overdue after that while it still heartbeats). With `gpu_on_overdue = request` the note also says whether the holder was asked to yield and what it did |
| `tree-orphan` | the wrapper is gone but the job it started is still running, so the card is in use and the claim is kept (see [A dead wrapper with a live tree](#a-dead-wrapper-with-a-live-tree-keeps-its-claim-plan-p10-adr-0084)): a recorded member of its process tree is alive by pid and start identity, or a launch marker or the process table ties a live process to it. It leads the precedence. Nothing is reclaimed or killed; it ends when the members are gone, or at once by `gpu release --epoch N`, which leaves the job running |
| `loaded-idle` | no lease; the seat is resident with nothing in flight (unloads at its ttl) |
| `busy-outside` | no lease, seat idle, cards busy — work the harness does not own (the processes are listed; a display card's unsized desktop is counted, not named) |
| `stale-holder` | a lease record whose holder is gone; the next acquirer reclaims it |
| `free` | no lease, nothing in flight, cards quiet |

Precedence among a live lease's verdicts: `stale-holder` (the record's holder is gone, so it is not live at all),
`tree-orphan`, `held-stalled`, `held-orphaned`, `held-overdue`, then `held-working` and `held-idle`. `working` (a
request or registered run in flight on the seat) is reported first because it says what is happening right now; its note
LEADS with the lease's own standing (`the lease itself is not healthy (... past its declared window by 3h0m0s), although
work is in flight on the seat: ...`) and the brief line leads with the capitalised word, so an escalation is never hidden
behind it. With several live leases (card-scoped leases) the verdict is about the most
escalated one and `activity.leases[]` lists them all. The verdict words are pinned: `TestVerdictDocTableComplete` fails
a change that adds one here without a row in this table.

`activity` carries `seat` (name, loaded, starting, inflight, source), `runs[]` (kind, pid, origin, goal,
phase, step, tokens_out, age), `gpus[]` (index, name, util_pct, mem), `gpu_processes[]` (with `display_card_processes_unknown[]`, below), and `holder`
(pid, alive, command, heartbeat_age_s, draining, exclusive, and the derived standing: `owner_state`,
`owner_session`, `owner_note` (why a recorded owner cannot be told apart), `orphan_marker_error`, `orphaned`,
`orphaned_since`, `orphaned_for_s`, `overdue`, `overdue_by_s`, `stalled`, `unattended`,
`progress{file,state,age_s,stall_s,detail,problem}`, `activity_facts[]`). The drain's progress line is built from the same
reading and printed on CHANGE (count, load state, a run's step), with a reminder every five minutes.

**A display card's desktop is one count, not a list (F18, 2026-10-09).** On Windows nvidia-smi types every window of the
desktop as a process on the monitor's card and sizes none of them (`used_memory` `[N/A]`, `used_known: false`): the
reference 3-card box listed 31 such rows, and the one process that mattered, a python on a work card (unsized as well),
was a needle in them. `gpu_processes[]` therefore lists every process EXCEPT those that are both on a card the card
table marks `display` and unsized; those are counted once per card in `display_card_processes_unknown[]` (`index`,
`gpu_uuid`, `name`, `count` of distinct pids). "Display" is the card table's rule (`display_active` or `display_attached`,
so the monitor's card counts with the screen asleep, when `display_active` reads Disabled on every card), and a one-card box
has no display card, so it folds nothing: its only card is its work card. A process with a known size, a process on any
other card (the lease holder's unsized python among them) and a row that names no card are listed individually, however
many there are. The shape is the same in `gpu status --json` and in every `offload_status` section that carries the lease
block (`gpu_lease`, and the default `all`, which builds the same block); the `busy-outside` note counts the desktop the same
way (`30 desktop processes on the display card (card 1, <name>), memory unknown (WDDM)`) instead of naming the first six
by name. The key `gpu_processes` stays; it is `[]` when a sample was taken and every row folded, and absent only when no
process sample was taken, and `display_card_processes_unknown` is present only when something folded. The complete list is
not kept anywhere else in the harness; it is nvidia-smi's own, `nvidia-smi
--query-compute-apps=pid,used_memory,gpu_uuid,process_name --format=csv` (the query the harness runs), and the desktop's windows
are in Task Manager. `gpu status` text prints no process list, so it has nothing to fold outside that `busy-outside` note.

## Probes pass the fence too (2026-09-22)

**The defect.** llama-swap starts any model a request under `/upstream/<model>/…` names (v251
`handleUpstream`; only a path matching the operator's `upstream.ignorePaths` is refused with 409). The
generation path waited for the card (ADR 0026), but the served-window probe (`/props`, `/v1/models`), the
seat-pin probe (`/props`, `/version`, `/v1/models`), the tokenizer (`/tokenize`), the warm-up, the whisper
transcription and the KV-slot lane each built the route themselves, outside every gate. The cordon and the
fence pre-check see only a lease that exists when a run is admitted, so an agent run admitted just before a
video render kept sending them: the 3-card seat started repeatedly on the render's cards (`starting` /
`failed: aborted` when a short-timeout client hung up mid-load / finally `Health check passed`), 14.3 GB landed on a card the
render held, and the render ran 895 s against its usual 228-324 s.

**The invariant.** While `blocksLoad` holds — a media lease, or a text lease stamped exclusive, not inherited
through `GPU_LEASE_EPOCH` — no harness request makes llama-swap load a model. `modelaffinity.AwaitUpstream`
is the one builder of an `/upstream` URL (`TestUpstreamURLsAreBuiltOnlyBehindTheFence` fails on a string
literal that spells the route anywhere else), and it:

- returns at once when nothing fences the card (one lease `ReadFile`, as before);
- under a fence, reads llama-swap's `/running` (never `/upstream`) and lets a model listed **ready** through —
  a request to a resident model starts nothing; absent, `starting` or `stopping` is not resident, and an
  unreadable view is not resident either;
- otherwise waits for the fence to lift until the caller's deadline or ctx, then returns the same typed
  `*LeaseError` a generation admission returns (holder named, "timeout" in the text).

| caller | deadline under a fence | outcome when the fence holds |
|---|---|---|
| served-window probe (both run doors) | the admission budget | the run defers `capacity` with the holder named, before any wall; no bare-root fallback |
| warm-up | the warm-up budget | nothing loaded, the note says so; the window probe then defers |
| cascade per-tier re-pack (window probe + tokenizer) | none | the tier re-packs from the entry cut; the fenced answer is **not** cached; the tier's generation is the request that waits (`Admit`) |
| CLI window probes (`local-agent`, compaction eval) | ctx and the 10-minute cold-start budget | falls back to the configured window, as an unanswered probe always has |
| seat-pin probe | none (one inspection) | no pin — the honest answer for telemetry |
| tokenizer (`/tokenize`) | none | fails open for that step and is **not** counted toward the sticky downgrade (`LastFailFenced`); the completion that follows is the request that waits |
| whisper transcription | `gpu_wait_ms` (90 s by default; zero is one inspection), capped by the client's own timeout (`stt_request_timeout_sec`, 1,800 s) — register C-89, 2026-10-01 | a `capacity` defer (`gpu busy: …`, error class `gpu_busy`) naming the holder |
| KV-slot lane | none | `409 seat-cold` |
| fleet chat lane (`POST /fleet/chat` → `/v1/chat/completions`) | the caller's own budget (the request context) and `ChatProxyTimeout` | `503` whose body is the lease refusal ("gpu-lease timeout …"): the caller files it as congestion (`timeout`), never as a broken stack |
| embedder (`/v1/embeddings`: the kNN pre-filter, `shadow-label`) | the embedder's own timeout | an error the kNN pre-filter fails open on, as on a slow embedder |
| `gpu reserve` warm-back | — | the holder's own sanctioned load: `HolderUpstreamURL`, the one unfenced builder, callable only from `gpu_drain.go` |

A run that took the card's refusal mid-run — its next completion waited at `Admit` and ran out — is filed
`capacity` ("gpu busy: …") on both doors, before the stall and ceiling branches: the cause is the held card,
whichever clock ended the wait.

**Model-dispatched routes.** llama-swap also loads the model a request names in its BODY on
`/v1/chat/completions`, `/v1/embeddings`, `/v1/completions`, `/completion`, `/infill`, `/v1/rerank`,
`/v1/audio/*`, `/v1/images/*` and `/sdapi/*` (v251 `modelPostJSONRoutes` and siblings). The harness's
generation clients (`llamaclient`, `agent.LLMClient`) take `Admit` there already; the fleet chat lane forwarded
another box's cascade call with no gate, and the embedder posted outside `Admit`. Both now build the URL with
`modelaffinity.AwaitModelRoute`. The chat lane WAITS rather than refusing, because the lane leaves queueing to
the node by design, its caller has no lane-to-local fallback, and a short render clears inside the caller's
budget; it does not take `Admit`'s per-base arbitration, which the lane has always left to llama-swap.
`TestModelDispatchedRoutesAreBuiltOnlyBehindAGate` fails on a literal that spells one of these routes outside
a gated builder call or a listed file (each listed with its reason; the two `Admit` clients are checked to
still take it).

**What is not covered.** The same one-read check-then-act window ADR 0026 names: a lease taken microseconds
after the fence's read, or a ready model evicted by its own ttl between the `/running` read and the request.
`render/gpu-lock.mjs`'s drain probe already reads `/running` first and probes only loaded models. A generation
request posted straight to llama-swap by anything outside the harness is outside the lease, as before.

## Known gaps

*Read against the code at commit `3be61a3c` (2026-10-14) for [ADR 0094](../architecture/decisions/0094-a-lease-names-its-cards-a-card-has-one-holder.md). Where this section and the code disagree, the code is right and the section is corrected. Each gap says where it lives. First what is not built, then the windows the 0.187.0 changelog entry for the host-RAM hold bound lists as open, then text that disagrees with the code; the older gaps follow and still stand.*

**Not built**

- **The status text names a command that does not exist.** A waiter is told to run `local-offload gpu takeover --epoch N`, with
  "not in this build yet", for a stalled, orphaned or overdue lease (`internal/gpuactivity/holders.go:128-133`,
  `internal/gpulease/explain.go:64-66` and `148`, `internal/mcpserver/status_section.go:326`; the end-of-term line in
  `gpu_terms.go:90` says "open to a takeover"). The verb list has no such verb (`gpu_cmd.go:56`, `gpuVerbs`). The stop primitives
  a takeover would use have no caller outside tests: `proctree.PlanStop`, `Stop` and `AllGone` (`internal/proctree/stop.go:38`,
  `111`, `158`) and `comfyinst.StopMarker` (`internal/comfyinst/comfyinst.go:356`).
- **`gpu release --epoch N` checks no liveness.** It removes the lease and stops only the ComfyUI instances kept under it
  (`gpu_cmd.go:1734-1802`, `internal/gpulease/gpulease.go:2228-2256`, `internal/gpulease/devices.go:962-966` and `997-1033`). A wrapper
  whose claim was removed stops its own command on its next tick (`gpu_cmd.go:986-992`); a detached job keeps running on a card that
  is free to the next lease; and for a tree-orphan the waiter's one named command is this one (`internal/gpuactivity/holders.go:135-139`).
  There is no release that waits for a dead tree and no opt-in to abandon one. Nothing writes the `quarantine.<id>` sidecars the
  allocator reads to keep such a card from being handed out (`internal/gpulease/allocator.go:398-421`, a reader only).
- **No policy for an overdue holder that ignores the request.** `gpu_on_overdue` is `request` or `hold`; there is no automatic mode,
  and a test pins that none exists (`internal/gpulease/yield.go:75-94`, `internal/gpulease/yield_test.go:1180`). Whether an
  unanswered request ends in a takeover is the operator's choice.
- **Several media paths hold the whole node.** By design: the lanes with no ComfyUI instance to bind to a card, which are the
  audio-gen voice lane, the iGPU engines and the sd.cpp lanes (`internal/pipeline/pipeline.go:3489`,
  `internal/pipeline/igpumedia.go:239`, `internal/pipeline/sdcppneed.go:85`, each a `wholeNeed`), and a `run-graph` that declares
  no device (`internal/pipeline/mediaadmit.go:297-302`, `declaredNeed`). Because the plan cannot name a card: an unreadable or
  empty card table (`mediaadmit.go:467-470`); a pin that names several cards, is not a ComfyUI device index, or is in a device
  order the box has not declared (`477-486`); a pool key that is not a device or is in an undeclared order, and a pool of fewer
  than two cards or on a box of more than three (`493`, `521-553`, `poolCards`, `poolMayBeScoped`); several declared devices
  (`515`); and a host that does not write card leases (`795-799`). The card-only pair is the one placement that never falls back
  to the whole node: a table it cannot read or a host that does not lease cards is refused with the reason
  (`internal/pipeline/mediamulticard.go:145-160`, `171-177`, `planMulticard`, `noCardLeases`; `mediaadmit.go:795-799`).
- **No pool of local workers across cards for one batch.** A local image batch renders through one warm session under one lease
  (`internal/pipeline/pipeline.go:2463-2481`); the fleet batch places jobs on nodes (`internal/mediaremote/batch.go:246`);
  `gpu reserve --cards N` queues on a fixed set and does not re-pick while it waits (`gpu_reserve_devices.go:25-29`).
- **No scripted acceptance of the whole path.** A batch holding two cards, a text seat loading on a free card, a launcher that dies
  and a waiter that takes the card over, and the status screens throughout have no script in the tree. One would run against a
  scratch lease root and never the live one.

**Open windows of the host-RAM hold bound**

- **Readers treat an unreadable record as absent.** `gpu status` shows a card free while a reserve is refused for it
  (`internal/gpulease/devices.go:232-234` and `276-279`); the grant side holds such a record while its keeper has shown life
  within the 120 s heartbeat TTL, so a keeper that dies leaves it held for up to that long (`internal/gpulease/keeper.go:71-74`,
  `87-106`).
- **A failed read of a claim file is a fence verdict.** `readCardClaim` does not tell an I/O error from "no claim"
  (`internal/gpulease/devices.go:147-153`, `935-956`), and the holders' confirmation is three looks 50 ms apart
  (`gpu_cmd.go:1594`, `1685-1697`), so a claim that cannot be read for about 150 ms reads as lost and the wrapper stops its
  command (`gpu_cmd.go:986-992`).
- **`confirmFence` has no cancel channel.** A window runs to its end when the process is told to stop (`gpu_cmd.go:1685-1719`).
- **A holder cannot write its give-up marker onto a record it cannot read.** `MarkHolderGaveUp` goes through `Restamp`
  (`internal/gpulease/hostram.go:803-812`, `internal/gpulease/devices.go:975-981`), so over such a record the exit status and the
  log are the only word.
- **A reserve that dies before its first outer stamp leaves none**, and with the hold bound at 0 the backstop is `--wait` itself
  (`gpu_cmd.go:1521-1551`, `startOuterBound`).
- **The reserve measures its wait on a monotonic clock and the holder compares wall-clock milliseconds**, so after a suspend the
  holder can release sooner, never later, than the stamp says (`gpu_cmd.go:1478`).
- **The warm-back abandons a warm on any failed check.** It kills and releases nothing and the seat stays owed
  (`gpu_drain.go:1237-1256`).
- **The hidden holder is exercised in process, not as a spawned child** (`gpu_hostram_holder_test.go`).
- **The windows are chosen figures, not measurements:** the fence window of 10 s (`gpu_cmd.go:1675`), the holder's grace of 30 s
  (`gpu_cmd.go:1590`), the heartbeat TTL of 120 s (`internal/gpulease/gpulease.go:112`), the claim grace of 10 s
  (`gpulease.go:1818`), the token grace of 30 s and life of 10 minutes (`internal/gpulease/tokens.go:49-52`).

**Text that disagrees with the code** (the code wins; this change edits only documents)

- **Code comments.** `internal/gpulease/devices.go:17-19` calls dropping the whole-node record "pending acceptance"; ADR 0018
  records it accepted on 2026-10-12 and the code does not write it. `internal/gpulease/term.go:16-18` says the session registry's
  writer is not wired into the session hooks (see the Terms bullet below). `internal/proctree/stop.go:3` says the stop primitives
  are used by the yield request; it uses none.
- **ADRs.** ADR 0026 says `DefaultTTL` is stamped on essentially every media lease; the routes pass their own timeout and
  `DefaultTTL` is the fallback (`internal/pipeline/pipeline.go:1683` and the other `acquireMediaLease` calls,
  `internal/gpulease/gpulease.go:114-116`, `1835`). ADR 0070 carries the registry sentence of `term.go:16-18`. ADR 0055 says the
  lease's reclaim rule has no job-liveness input: true of the reclaim rule (`gpulease.go:1152-1172`), while progress decides a
  term's renewal or label (`internal/gpulease/term.go:3-15`) and never frees a card.

**Older gaps**

- **A tree-orphan is held until its members are gone, and nothing yet stops one.** The tree record, the `tree-orphan` verdict and the
  stop primitives exist (see [A dead wrapper with a live tree](#a-dead-wrapper-with-a-live-tree-keeps-its-claim-plan-p10-adr-0084)),
  but the takeover that stops a tree and frees the claim is a later change (`gpu_on_overdue = request` asks the holder to yield and
  stops nothing, see [An overdue lease's holder is asked to yield](#an-overdue-leases-holder-is-asked-to-yield-it-is-never-taken-from-plan-p11-adr-0086)), so a hung job whose wrapper died is freed by an operator stopping its processes or running
  `gpu release --epoch N`, which ends the claim and leaves the job running. A reader built before the change still reclaims a dead
  wrapper's lease at once, and the recorded tree is at most one tick behind.
- **The warm-back's reload retry rests on llama-swap's source, not on a captured incident.** The 500 body of the incident
  that prompted it was never recorded, the retry class keys on llama-swap's message text, and a re-send during the old
  server's teardown was not measured against a live instance. The failure line now carries the status and a snippet of the
  answer, so the next incident documents itself; see the reload bullet under "Draining a seat before a window". A retried
  warm also holds the lease up to the 60 s window longer, and a llama-swap that is down holds it for that window before the
  warm fails.
- **Terms: expiry is a label that only reports, and an attended lease with no progress contract is judged on one
  sample.** Nothing consumes the expired label yet except `gpu status`, `offload_status`, the waiter's sentence and
  `/fleet/health` (the takeover that acts on it is a later change). A lease whose owner the registry never held (an MCP
  server older than the registry, a non-Claude caller; plan section 7 finding 5) has an owner who cannot be told. It is
  not shown present, so a busy card alone does not renew it (a card in use proves a process, not that anyone wants the
  result) and it is labelled at the end of its `--for` unless it declares a progress contract that is advancing: that
  alone keeps it renewing, as it does an unattended lease (operator decision 4: a progress contract renews by progress).
  The MCP server writes the registry when it starts (`internal/mcpserver/mcpserver.go`, `registerSession`), so that is a
  session whose server never ran or a caller that is not a Claude session, and whether a given Claude Code build hands its MCP
  server the session id is not verified. For such an owner an attended job without a progress file reads `expired` at the
  end of its first term even while its cards are busy
  (the `held-overdue` verdict was already true from that moment; the label adds the sentence and the takeover
  eligibility, and a takeover of an unknown owner will need `--force` anyway). The wrapper form now closes that for a
  wrapped render helper, or a script that runs one, which is the case that was accused while healthy (F8): it declares
  and heartbeats a progress contract, so such a lease renews for as long as the render lives. Any other attended job
  without a progress file (a bench, a Go verb, a `--detach` hold, a recipe the scan cannot read and that did not ask
  with `--progress-file auto`) still reads `expired` at the end of its first term. And for an attended lease with a live owner
  and no progress contract, "its cards are working" is a single `nvidia-smi` sample taken at the end of the term: a job in
  a quiet CPU phase at that instant is labelled, and the next check (a minute later) renews it if the cards are busy by
  then; a sample that could not be taken is labelled as unreadable, not as idle, and is asked again the same way. Neither
  gap frees or kills anything.
- **Ownership is only as good as the registry.** A lease whose owner is a session id the registry never held (an MCP
  server older than the registry, a non-Claude caller) reads `unknown` and is never orphaned; a lease whose owner is a
  pid recorded by `--owner-pid` is judged by that process alone, so a launcher that names a short-lived shell reads
  orphaned when the shell exits. The orphan verdict is information with a 15 minute grace, never an action. How a Claude
  Code session id maps to a live process is not verified beyond the environment variable the ledger already uses.
- **Card-scoped leases can be written, an older reader cannot see them, and not every consumer takes a device set yet.** The
  record, the fence, `gpu reserve --devices|--cards`, the allocator, the per-card status and the audit exist (see
  [Card-scoped leases](#card-scoped-leases-record-v2)), and the text gate, the delegator and the placement table read a seat's
  cards (see [Consumers read a seat's cards](#consumers-read-a-seats-cards-not-the-node-plan-p4)); drain, unload and the render
  lane's unload take the leased cards (see [Drain, unload and the render lane](#drain-unload-and-the-render-lane-clear-the-leased-cards-not-the-node-plan-p5));
  fleet health (P7) publishes every live lease with its cards and a delegator fences a node per contract (see "The fleet
  reads leases per card"); a node or delegator one release behind reads the one lease block as the whole node (over-fencing,
  the safe direction). A binary or Node reader that predates the format reads a directory holding only device leases as a free
  card, which is why `gpu_card_scoped_leases` stays off on a host until `gpu doctor --write-audit` is green.
- **A legacy whole-node lease is scoped only on evidence, only for a record an older binary wrote, only when the host turns
  it on, and the live evidence has not been captured.** The rule (a command line, a tied launch marker, two sampled readings
  five minutes apart) is built and tested on synthetic fixtures; what a real legacy tree shows on the three-card box is
  captured at plan P6. Until then `gpu_legacy_scope_inference` is off and a legacy lease fences every seat, and `gpu status`
  names the key; with it on, a legacy lease stays whole-node unless its process tree names a card the box's declared ComfyUI
  order can place.
- **The seat race rule covers `Admit`-gated batches and the delegation door's warm-up, not every load.** A seat loaded through
  `AwaitUpstream` or `AwaitModelRoute` (speech, embeddings, the chat lane, tokenize, props, KV slots) has no post-load check, so
  it can stay resident on a card a lease claimed in the same instant until its idle ttl; a batch that never drains also never
  reaches its release. The plan's wording ("a seat load that passed the gate re-checks after the model is resident") is wider
  than what is built.
- **An idle seat on a card an inferred scope has just spread onto is not swept.** It leaves at its own idle ttl or when its next
  request completes; no read of the lease unloads anything.
- **The cascade-lane and repack busy gates still read any live lease as busy.** They only choose a remote lane, never a fence.
- **The ComfyUI order is not derivable.** Turning `--cuda-device N` / `COMFY_CUDA_DEVICE` into a card needs `gpu_comfy_order`
  (or a one-card box); until it is declared such a command reserves the whole node and says why.
- **`--cards N` queues on a fixed set.** It does not re-pick while queued; that is the fan-out work.
- **The reservation is a convention.** A raw `curl :11436` loop, a graph posted straight to
  ComfyUI, or a forgotten `gpu reserve` gets no protection. Because the gap cannot be closed
  at the mechanism level, it is closed by RULE: posting to `:8188` directly is forbidden —
  see [OPERATOR-GUIDE](../OPERATOR-GUIDE.md) ("Never post a graph to `:8188` directly"). The
  rule covers any tool that drives ComfyUI directly, the official Comfy-MCP server included.
  `run-graph` and `gpu reserve --class media` are the supported ways to do the same work
  while holding the lease.
- ~~A `text` reservation did not gate `agent_delegate` placement.~~ **Closed 0.113.14** — see
  "Delegate placement reads a `text` reservation" under Classes. Interactive text calls (the
  ~46 ms ones) still neither acquire nor honour it; that limit stands.
- **The lease reduces the number of teardowns; the drain is what makes one safe.** Both needed.
- **The run registry is advisory.** A launcher that cannot write the state root logs once and runs
  unregistered (the drain then relies on the engine's gauge and says so); interactive single-shot text
  calls never register — the gauge covers them. A run registered by a process that hangs without
  exiting stays visible for the 120 s heartbeat TTL.
- **Head-of-line blocking is structural** — a 45-minute video blocks everything behind it.
- ~~`internal/pipeline` does not yet take a `media` lease around its own generation calls.~~
  **No longer true, corrected 2026-08-19.** `acquireMediaLease` wraps every generation route in
  `internal/pipeline/pipeline.go`: image-gen, image-gen (sdcpp), inpaint, edit, image-gen batch,
  run-graph, video-gen and audio-gen. The one deliberate exception is `runPipelineJob`, which
  takes the in-process `mediaSlot` only and documents why; its nested per-stage calls do take the
  lease. This bullet had gone stale and then directly contradicted the rule added above it.
