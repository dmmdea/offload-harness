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
  instead, and the first one writes `gpu/FORMAT`.
- **Decision record:** [ADR 0018](../architecture/decisions/0018-machine-wide-fenced-gpu-lease.md)

## Source map

| path | role |
|---|---|
| `internal/gpulease/gpulease.go` | **the only implementation**: `LeaseDir` (the one resolver), acquire/release, epoch fencing, the reclaim conjunction, `InspectDir` (the one read path) |
| `internal/gpulease/devices.go` | **card-scoped leases (record v2)**: device-id normalisation, the conflict rule, the grant (one critical section under the epoch lock), the per-epoch fence, per-lease release and restamp, the debris sweep, and the one reader (`reader`) every inspector shares |
| `internal/gpulease/allocator.go` | the **card allocator** behind `gpu reserve --cards`: a pure function over an input the CLI assembles (cards, claims, quarantine, foreign busy, resident seats, presence, host RAM), plus the quarantine sidecar reader |
| `internal/gpulease/cmddevices.go` | the default card set of a wrapped command: `CUDA_VISIBLE_DEVICES`, `--cuda-device`, `COMFY_CUDA_DEVICE` turned into cards, refusing instead of guessing |
| `internal/gpulease/effective.go`, `infer.go`, `proctree*.go` | **what a lease holds, as a seat reads it (plan P4)**: `Info.EffectiveDevices/Touches/For`, `ResolvePins`, the legacy-lease evidence rule (`Scoper`, applied only to a record an older binary wrote: `Info.Legacy`; the `seen.<epoch>` sidecar, the scope ledger) and the process-tree reader it runs on |
| `internal/gpulease/term.go` | **terms (plan P9)**: `PlanTerm` (what a requested window comes to), the installed limits (`SetDefaultTerms`), and `AdvanceTerm`, the one place a term ends in a renewal or the expired label (see [Terms](#terms-a-window-is-a-term-and-a-term-ends-in-a-renewal-or-a-label-adr-0070)) |
| `internal/gpulease/audit.go`, `procimages_*.go` | the **reader audit** behind `gpu doctor`: finds harness binaries, Node readers and running images, and writes the reader-audit marker |
| `internal/gpuprobe/cards.go` | the **card table** (UUID-keyed, index spaces side by side) |
| `internal/gpuprobe/hostmemory.go`, `hostram*.go` | the **host memory reading** (physical, available, commit used and limit; Windows `GlobalMemoryStatusEx`, Linux `/proc/meminfo`), `HostRAMAdmits` (the ONE admission rule), the OK / NEAR / OVER verdict and the `UseHostMemoryReader` test seam |
| `internal/gpulease/hostram.go`, `procmem*.go` | the **host-RAM term of the grant**: `Options.HostRAMGiB`, `ErrHostRAM`, the not-yet-loaded part of granted leases (private memory of the holder's descendants), `Manager.HostRAMPending` |
| `internal/hostneed` | **how much host RAM a lease declares**: `--ram`, else the model files of a recognised render helper call that do not fit the card, else the media class default (see "Host RAM") |
| `internal/gpucards/hostview.go` | the `host_memory` block and verdict that `gpu status` and `offload_status` render |
| `internal/gpucards` | the per-card view (`Rows`, `LeaseRows`, `QueueRows`, `Table`, `Section`) that `gpu status`, `gpu cards` and `offload_status` brief all render |
| `internal/gpulease/proc*.go` | per-platform liveness and process-start identity, exported so no consumer keeps a second copy |
| `internal/pipeline/pipeline.go` | takes the `media` lease around **every** generation call site (image ComfyUI + sdcpp, inpaint, image batch, run-graph, video, audio) and threads `GPU_LEASE_*` to the runner; inherits an ambient lease instead of re-acquiring; owns the in-process slot (`mediaSlot`) that arbitrates jobs sharing one inherited lease |
| `gpu_cmd.go` | the `gpu status\|reserve\|release\|hold` verbs, wrapper and `--detach` forms |
| `gpu_terms.go` | the holders' side of terms: the tick the wrapper form and the detached holder run at the end of a term, the over-cap warning, and the argv of the hidden holder (`--release-at-expiry`) |
| `gpu_reserve_devices.go`, `gpu_cards.go`, `gpu_doctor.go` | which cards a reserve holds (`--devices`, `--cards`, derivation, the queue), `gpu cards` and the status table, `gpu doctor` |
| `internal/gpulease/owner.go` | **ownership (ADR 0070)**: who asked for a lease (`Owner`), the session registry, the first-observer orphan marker, the progress contract and `Standing`, the derived reading every status surface shares |
| `internal/gpulease/explain.go` | the sentence a waiter reads when the lease it is queued behind is stalled, orphaned or overdue, and the installed orphan grace. READ-ONLY: it reads the orphan marker a status surface recorded and never writes it or takes the epoch lock |
| `gpu_ownership.go` | the ownership flags (`--owner-*`, `--unattended`, `--progress-file`, `--stall`, `--yield-grace`, `--on-yield`), the `gpu owner-flags` verb, and the ownership lines of `gpu status` |
| `gpu_drain.go` | `--drain`: waits until the seat's gauge AND the run registry are empty, inside the queue budget; restamps `draining` → `exclusive`; unload / warm-back |
| `internal/gpuactivity` | the RUN REGISTRY (`<state root>/gpu/activity/`, one record per agent loop in flight) and the activity reading behind `gpu status` / `offload_status.gpu_lease` (`verdict`, `activity`) — ADR 0041; `holders.go` derives each live lease's standing, `facts.go` the activity facts of a legacy lease, `snapshot.go` (`View.Map`, `splitProcesses`) the one JSON shape both surfaces read, with a display card's unsized processes folded into one count per card |
| `internal/seatload` | the seat's in-flight reading: llama-swap's `/running` (and the roster, for an alias), then the loaded seat's own `/metrics` or `/slots` at the `proxy` `/running` reports — never through `/upstream`, which resets the idle unload timer; a `starting` seat is reported without touching the seat |
| `gpu_hide_windows.go`, `gpu_hide_other.go` | hidden spawn for the detached holder (a visible console gets closed, killing the hold) |
| `render/gpu-lock.mjs` | READ-ONLY participant: honours + fences an inherited lease, elects one unloader, drains, ComfyUI lifecycle. **Does not acquire.** |
| `internal/gpulock` | the read-only vision gate; the held rule is `gpulease.InspectDir`'s, read through the caller's own narrowing (`WaitFreeScoped`: the vision pre-check passes the seat-narrowed reading the text gate uses) |
| `internal/modelaffinity/gpuwait.go` | READ-ONLY: text admissions that would make llama-swap load a model wait out a `media` holder (ADR 0026); armed from `config.Load` via `LeaseDir` |
| `internal/modelaffinity/upstream.go` | READ-ONLY: `AwaitUpstream`, the ONE builder of a llama-swap `/upstream/<model>/…` URL, and `AwaitModelRoute`, the builder for a model-dispatched route (`/v1/chat/completions`, `/v1/embeddings`, …) sent without `Admit` — every probe, tokenizer, warm-up, transcription, chat-lane forward and embedding passes the same fence as a generation (see "Probes pass the fence too") |
| `gpu_drain.go` (`leaseScope`, `maintainSeatScoped`, `unloadModelsFor`), `render/gpu-lock.mjs` (`parseUnloadModels`) | plan P5: the drain and unload take only the seats on the leased cards, and the render lane unloads the list the wrapper exports (`GPU_LEASE_UNLOAD_MODELS`) |
| `internal/modelaffinity/seatscope.go`, `scoper.go`, `seatyield.go` | the gate's per-seat reading (`SetSeatPins`, `ScopeToPins`, `SeatLease`, `CardsHeld`), the production wiring of the evidence rule (`InspectLease` for the load gate, which remembers what it sees; `PeekLease`, `ScopeInfo`, `ScopeFunc`, `ScopeLeases` for inspectors, which write nothing), and the seat race rule (`YieldIfFenced`) |
| `internal/mcpserver` | registers the session this MCP server serves in the session registry at start and removes it at exit |
| `internal/config` | `state_dir`, `gpu_lock_path`, `gpu_card_scoped_leases`, `gpu_legacy_scope_inference`, `gpu_comfy_order`, `gpu_host_ram_headroom_gib` (default 8; `Load` installs it process-wide, `gpulease.SetDefaultHostRAMHeadroom`), `gpu_orphan_grace_min`, `gpu_max_term_min`, `gpu_max_total_min`; `ModelPins` (a model's device pins, read from the layers) |

## Why it exists

One GPU, two very different consumers: llama-swap wants its tiers resident; ComfyUI wants the
whole card. Before this, only the Node render runners took a lock, so text work had no way to
say "I am using the GPU." A media job dispatched from elsewhere unloaded every GPU-resident
model mid-benchmark. The server log holds **3,356 unload calls, 330 of them the text
workhorse, and 0 for the memory stack** — that last zero identifies `freeLlamaSwap` as the
caller, since excluding the memory stack is its own invariant.

## Using it

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

Neither of the first two is defensive padding: with waiters polling once a second, both races are
ordinary traffic. Measured under six concurrent acquire/release workers, 1 in 48 cycles failed
before the retries were added.

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
removes what was made); flip the record to `active`. A whole-node grant keeps its own atomic token (`O_EXCL meta.json`)
and, once a device lease has ever existed on the host (or whenever `e/` cannot be proven absent), verifies under the same
epoch lock that no device lease is live and withdraws its claim if one is. Before it judges, it **sweeps** every device
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

**Deviation from the plan, pending acceptance.** Plan rev 2 (section 2, P2 scope) specified a synthetic pid-0 `meta.json`
"umbrella" over any set of device leases, so that a pre-v2 writer would queue behind it. This change does **not** write
one. The premise of the umbrella was checked against the real binaries on the build host and **holds**: a `meta.json`
with `holder.pid = 0` and a future `expires_at_ms` makes `gpu reserve` of 0.158.3 and of 0.160.0 queue and then refuse
("GPU held by media (pid 0 ...)"), and the same record with a past window is reclaimed and granted. So the umbrella was
dropped for a design reason, not because it would not work: it is a second source of truth that would have to be
re-stamped on every grant, release and extension and be judged by every reader as "not a real lease", and its only
audience is a binary the audit is meant to find and replace. The cost of dropping it is that a pre-v2 writer that is
**not** found by the audit double-books a host that has the switch on (measured with a byte-identical older wrapper in the
review of this change). Until the plan owner or the operator accepts the deviation, plan P2's acceptance is **not**
met; the umbrella can be restored later without a format change. No 0.152.x build is on the build host, so
none was tested. Two hard gates stand in for the umbrella, and the second is now code:

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
| `gpu reserve --cards 2` or `--cards 1..3` | the allocator picks (below). Never the display card while the operator is at the desk; with `operator_presence` reading away it may take that card, but only while the display layer's desktop floor stays free after `--vram`, and never with no `--vram` ("The display card, once the operator is away", below). |
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
the set was derived from the command's own pin. A command that pins itself keeps its pin when the pin is the program's own
(`--cuda-device` on its command line, `COMFY_CUDA_DEVICE`, which ComfyUI turns into `CUDA_VISIBLE_DEVICES` itself, so an
override would be futile): the wrapper warns on stderr when that pin falls outside the held cards or cannot be resolved. A
`CUDA_VISIBLE_DEVICES` the command merely **inherits** from the operator's shell is replaced with the held cards when it
reaches outside them or cannot be resolved (and the wrapper says so); one inside the held cards is tighter than the lease and
is left alone (`TestConfineWrappedRules`, `TestGPUReserveDevicesPinsTheChildToTheHeldCards`,
`TestGPUReserveReplacesAnInheritedPinThatReachesOutsideTheLease`). Two limits remain: a program that sets its own
pin after it starts is not confined (ComfyUI's `--cuda-device` overwrites `CUDA_VISIBLE_DEVICES`; one instance per card is
plan P13), and `GPU_LEASE_DEVICES` is for a command that wants to read the set itself. `--drain` and `--unload-seat` take
the cards the lease holds and leave the seats on the others (plan P5, below).

**The allocator.** A card is allocatable when it is not quarantined (`quarantine.<id>` sidecars, which P12 will write),
not the display card (the card whose `display_active` reads Enabled **or** whose `display_attached` reads Yes: with the screen asleep
`display_active` reads Disabled on every card of the 3-card box while `display_attached` still marks the card that drives the monitor,
measured 2026-10-03; the card table's rule, `gpuprobe.ScreenCardUUIDs`. The lease verdict and the fleet health attribute load by
`display_active` alone, `gpuprobe.DisplayCardUUIDs`, so an attached monitor never hides a holder's own work; and a reading taken
after a transient `nvidia-smi` failure carries no `display_attached` and marks its cards `display-unknown`: the allocator hands out none of
them until the next good reading), not claimed (a
whole-node lease claims every card), not under a foreign compute process
(`foreign-busy`, reported and skipped, never killed), its free VRAM fits `--vram` (GiB per card), and the **host** has the
host's memory admits what `--ram` declares: the same rule the grant applies (`gpuprobe.HostRAMAdmits`: committed memory +
the need + the not-yet-loaded part of granted leases must stay under physical RAM less `gpu_host_ram_headroom_gib`, default
8; see "Host RAM" below), here as an advisory pre-filter. Among allocatable cards the order is: no resident
seat first, then the cheapest eviction (the footprint of the configured layer seats loaded on it), then the lowest id. An
unreadable host-memory reading refuses only when a RAM need was declared, and a need no state of the host admits ends the
request at once. On Windows (WDDM) nvidia-smi lists no per-process
rows for compute apps, so `foreign-busy` is Linux-only evidence today.

**The display card, once the operator is away.** `operator_presence` is one key with two readers: the display layer's
`presence` guard ([composite-tier.md](composite-tier.md)) and this allocator. Set to `auto` (the console locked, or idle
past `operator_idle_sec`, and nothing fullscreen) or to `away` (the operator's override, which reads away whatever the desk is
doing), it lets the allocator take the display card too, and an opened card is still the desktop's, so it is held to the floor
the display layer keeps: allocatable only while its free VRAM, less `--vram`, still leaves `display_floor_gib` (the largest
floor declared by a layer guarded by `display_floor`; a box that declares none is judged as it was before this rule). The
arithmetic is the layer's own (`free − footprint ≥ floor`), so the two doors onto the card cannot disagree about it, and it
applies to the display card alone: a pair card is judged by the footprint, as it always was. A job that gives no `--vram`
never takes the display card, because a footprint nobody declared cannot be shown to leave the floor (the layer's guard
refuses a seat with no display footprint the same way); a media call whose `comfy_cuda_device` is empty declares none, so
it picks among the other cards. `--devices` names the card on the operator's word and is never second-guessed. The skip
reads `vram` and names the floor, and a display card that could not clear the floor is not queued on while a lease holds it
(waiting would not fix it). A display twin that is loaded counts as a resident seat of the display card, each loaded
twin costed at the layer's `display_footprint_gib` (both twins loaded cost twice that), so the card is not ranked as an
empty one while it holds a twin: taking it would evict the twins, and the order prices that. When llama-swap's `/running`
cannot be read the display layer's models are counted as loaded, so the display card sorts as occupied rather than as the
empty card it might be; other layers' seats contribute nothing then, as before
(`TestAnOpenedDisplayCardIsHeldToTheDesktopFloor`, `TestALoadedTwinOnTheDisplayCardIsAnEvictionNotAnEmptyCard`,
`TestResidentFromCountsALoadedModelMapTwinOnItsCard`, `TestPickAutoDoesNotHandOutTheDisplayCardBelowTheLayersFloor`).

**A queued request is asked again at the grant.** The cards a `--cards` request or an auto-placed media call queues on
are chosen from the operator's presence and the display card's free VRAM as they are at the enqueue, and a place in line
can be hours long. When the cards come free the grant puts the desktop rule again from fresh readings
(`gpulease.Options.GrantCheck`, `gpulease.DesktopRefusals`, built by `gpualloc.GrantCheck`): the display card, or a card
that may be it on a reading that could not say, while the operator is back at the desk, and the display card under its
floor. The check runs with the cards already claimed by the request, because a claim that is checked before it is made can
be skipped by a lease that is mid-release; a refused grant is released at once and the request chooses again from the same
fresh readings, so it takes another card it still fits, queues on what qualifies now, or keeps polling for the display card
to qualify until its `--wait` ends. The second choice keeps the arrival time of the first (`Options.QueuedSince`), so the
place in line is not lost to it. `--devices` is the operator's word and carries no check
(`TestAQueuedLeaseIsNotGrantedTheDisplayCardOnceTheOperatorIsBack`, `TestAQueuedMediaCallIsNotGrantedTheDisplayCardOnceTheOperatorIsBack`).

**A lease already running on the display card is not revoked when the operator returns.** The post-admission watcher
([composite-tier.md](composite-tier.md), ADR 0075) unloads the display layer's llama-swap twins and nothing else: it never
stops a process and never releases a lease. A `--cards` job or a media render that took the display card while the operator
was away keeps it after they come back. The desktop floor was checked once, when the card was claimed
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
waiter carry a place. Tokens are files in `<state>/gpu/tokens`, never among the waiters in
`<state>/gpu/waiters`: a binary that predates them prunes every waiter record whose process has stopped
refreshing it, and would delete a token it cannot refresh. It does not honour tokens either, so on a host that
mixes versions an older binary can take a card ahead of a token holder; that costs the holder its place and
never exclusivity (the O_EXCL claim is still the only arbiter of who holds a card). A token id arrives from a
tool caller and is checked against `tk-[a-z0-9]{8,32}` before it becomes a path. A queued `Acquire` whose
whole window passed without reaching the front returns `ErrStillQueued`, which names who is ahead (a waiter
that has not claimed, or a place held for another caller); callers that answer with a token treat it like
`ErrHeld`.

**A kept ComfyUI instance lives no longer than its lease.** A runner that keeps the ComfyUI it launched
(`--keep-comfy`) leaves a detached instance running after it exits, so the items of a batch under one
lease load their models once; its launch marker (`.offload-launch-<key>.json`) records the lease epoch.
The **holder** of that lease stops the instance when it lets go, before the release and before any seat
warm-back (both want the VRAM): `gpu reserve` when its wrapped command ends, the detached holder when it
exits, `gpu release` when an operator ends a lease from outside, and the pipeline when a media lease is
released. `gpu release` settles WHICH lease it is ending first, by the release's own rule (`Manager.ReleaseTarget`:
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
epoch) is not claimed by a lease that happens to reuse it. An instance nobody reuses stays up until an operator stops
it: nothing here is a timer or a watcher (plan I5), and whether the next acquirer or `gpu doctor` should sweep
markers whose epoch is dead is an operator decision (I5 forbids a watcher, not an explicit sweep). A kept launch
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
router seat that declares no model, the cascade's rung models. `config.Load` arms it into the load gate beside the lease
directory (`modelaffinity.SetSeatPins`), with `gpu_comfy_order`, `comfy_dir` and the memory stack. The arithmetic is three
methods on `gpulease.Info`: `EffectiveDevices` (the declared cards, else the inferred ones, else none, which is the whole
node), `Touches(ids)` and `For(ids)`, which narrows an inspection to the leases that touch the cards and leaves every
existing predicate (`Held`, `blocksLoad`, `BlocksNewRun`, `Reserved`, `Fenced`, `ForeignFence`) unchanged and unaware.
`gpulease.ResolvePins` turns a seat's pins into lease ids through the card table.

**The direction of every doubt is "fence".** A model nobody declared a pin for (an alias a layer does not spell, a seat on a
box with no layers), a pin the card table cannot place (an index no card has, an ambiguous prefix, a card named twice), a
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
| the stt auto route (`sttremote`, 0.164.0) | the "local whisper would be held" trigger is `modelaffinity.WouldBlockUpstream`, the upstream fence's own first inspection without its wait: a lease that fences the whisper model's cards (the same narrowing as above) AND the model not already resident; a resident whisper is served here at once (`TestWouldBlockUpstreamAgreesWithTheFence`, `TestDefaultLocalBusyIsTheWhisperFenceNotTheLease`) |
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
cards** when its declared pin (`config.ModelPins`, resolved through the card table) intersects them; a seat whose pin is
unknown, cannot be placed, or is read while the card table cannot be, is on them (today's behaviour, the direction of every
doubt). Then:

- The configured agent seat is drained and unloaded only when it is on the leased cards. A seat on card 0 under a card-2
  lease is neither drained nor unloaded, no warm-back is owed for it, and the wrapper says so on stderr
  (`TestUnloadSeatLeavesAnAgentSeatOnAnotherCard`).
- The other resident models are split the same way: those on the leased cards and those nobody declared a pin for are
  unloaded; the rest are left resident and named (`gpu reserve: left resident (not on the leased cards): ...`)
  (`TestUnloadSeatOnlyUnloadsIntersectingModels`). The memory stack is still never unloaded.
- The legacy `GET /unload` is total (it ignores `?model=`), so it is refused while any model that has to stay is resident,
  the seats left on other cards as well as the memory stack.
- **The drain waits only for runs on the leased cards.** The run registry records each run's seat pins (`Run.Devices`, set by
  the delegation door and the `agent_run` door); a scoped drain waits for registered runs whose pins intersect the lease, by
  the seat they run on when a record predates the field, and skips the agent seat's own gauge when that seat is on other cards
  (`TestDrainWaitsOnlyForIntersectingRuns`). A run on card 0 does not hold a card-2 lease.

**The render lane unloads the list the wrapper hands it.** For a card lease the wrapper exports `GPU_LEASE_UNLOAD_MODELS` to
the wrapped command: the llama-swap roster, minus the memory stack, minus the seats pinned to cards the lease does not hold
(`unloadModelsFor`). `render/gpu-lock.mjs` `freeLlamaSwap` unloads exactly that list (drained first, as always) instead of every
model off the memory stack. Unset or empty keeps today's rule, `-` means nothing may leave, the memory stack never leaves
whatever the list names, and the total-unload fallback is refused while a model outside the list is resident
(`render/gpu-lock.test.mjs`, "freeLlamaSwap honours the supplied list"). A whole-node lease, a host that cannot read the roster
and a command that was not started by the wrapper export nothing, so the lane keeps its own rule. The pipeline's own media
lease is whole-node until plan P13 takes one card per render, so it exports no list yet.

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
names what it cannot rule out (`TestTrespassIsOnlyPossibleAndNeverAccusesTheHolder`). Not flagged: the display card (the desktop's own use), a card
another live lease holds, a card a registered run is pinned to, anything while the seat itself is busy (its card is unknown
here), and a whole-node lease (it has no outside). It is a flag on the existing verdicts, not a new verdict word. With a
bounded lease, `held-working` now means **its** cards are busy: a busy card outside the set is a seat's or another lease's,
never the holder's own job.

**Not done here.** The activity snapshot trusts the nvidia-smi utilisation of a card, which on Windows includes the desktop
(only the display card is excluded); the trespass flag inherits that, and does not exclude a card that hosts a resident or busy
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
| `yield_grace_ms`, `on_yield`, `on_yield_dir` | the job's own terms for being asked to stop; recorded for the takeover command and read by nothing yet. `on_yield` is stored verbatim (refused above 4096 bytes, never clipped: the takeover will run it) with the directory the lease was taken in, because that takeover does not share it |

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
is not a declared window), `--progress-file` and `--stall`; `--yield-grace` and `--on-yield` are recorded. A job nobody
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

**Not in this change, on purpose:** the process-tree record behind `tree-orphan`, the takeover and yield commands,
`gpu release` routing. A lease past its window with a live holder is surfaced as `held-overdue` and nothing else happens
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
registry never held, or a registry that could not be read; today that is every attended lease taken from a Claude
session, because the registry writer is not wired into the session hooks yet) is not an objection and is not shown
present: an advancing progress file, which the job itself writes, vouches for the lease alone, as it does for an
unattended one. What such an owner does **not** get is the weaker leg: a busy card proves a process, not that anyone
wants the result, so a lease of an owner who cannot be told, with no progress contract or a stalled one, is labelled at
the end of its `--for` however busy its cards are (it already read `held-overdue` from that moment; the label adds the
sentence). A gone owner is rescued by neither. "Its cards are working" is one `nvidia-smi` sample over the lease's
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

## Host RAM: a lease declares what it will load, and the box never promises more memory than it has (2026-10-09)

**The rule (AGENTS.md).** The cards do the inference; RAM is overflow only. Spill is allowed only while it is
bounded, and it never makes the box unstable. **The guard's reading of that rule** is that the box never promises more
memory than it has: committed memory stays under physical RAM less a headroom. That line is the guard's own, chosen
2026-10-09 as the conservative way to keep spill bounded; it is a definition, not a measurement of any box's paging. A
card has a hard edge (the driver refuses the allocation); host RAM does not (the OS pages and every other process
stalls), so the harness refuses *before* the grant, on the number that includes what the granted jobs are about to load.

**The incident.** On the reference 3-card Windows box (127.7 GiB physical), two ComfyUI media lanes that stream bf16
weights the card cannot hold ran at once under two card-scoped media leases (`gpu reserve --devices <card> --class media --
node render/comfy-generate.mjs --batch ... --family krea2 --ckpt ...`). A Krea 2 bf16 UNet (24.5 GiB) and its Qwen3-VL-4B
text encoder (8.3 GiB) on a 16 GiB card, and a Qwen-Image 2512 bf16 stream of about 38 GiB. Committed memory reached
162.9 GiB against a 187.7 GiB limit, the system-managed page file grew from 60 to 68 GiB, and free RAM bottomed at 3.2 GiB.
The non-media baseline on that box was about 56 GiB (desktop apps, agent CLIs, browsers, WSL, kernel pools). One keyed
per-card ComfyUI instance held 57 GiB private because it still cached a Qwen-Image model from an earlier lease next to the
Krea 2 model of the current one; POSTing ComfyUI's own `/free` released 52 GiB of it between prompts without killing a job.

**Why the harness did not stop it.** The card allocator's host term (`allocator.go`) applied only to `--cards`; an explicit
`--devices` lease, which is what every owner wrapper takes, bypassed it. It read **free** RAM, which says nothing about jobs
that have been granted and have not loaded yet (both lanes were granted while RAM was still free). And nothing read
**committed** memory at all.

### What a lease declares

A lease carries `Options.HostRAMGiB`, stamped on its record as `host_ram_gib` (additive and omitted when 0: an older
binary ignores it, and a record without it declares nothing). Where the number comes from, in order
(`internal/hostneed`; `TestAKrea2Bf16CallOn16GiBIsTheUnetPlusTheTextEncoder`, `TestAnExplicitRamBeatsTheEstimate`,
`TestTextAndSeatLeasesDefaultToZero`):

| source | what it is |
|---|---|
| `gpu reserve --ram <GiB>` | the operator's word. **0 is allowed** and means "needs no host RAM". A negative value is refused. |
| a recognised render helper call | `render/comfy-generate.mjs`, `comfy-render`, `comfy-edit`, `comfy-inpaint` or `comfy-video`, wherever it sits in the wrapped command (either path-separator style). The UNet or checkpoint and the text-encoder files the family loads are sized through the configured ComfyUI model paths (`mediacap.ModelRoots`, the same tables `doctor` reads) and counted **in full when together they do not fit the largest card of the lease** less the runner's `--reserve-vram`; a set that fits declares 0. A krea2 bf16 call on a 16 GiB card is 24.48 + 8.27 = 32.75 GiB. A video call is sized from the files its flags name (`--transformer`, `--high-unet`, `--low-unet`, `--text-encoder`) and the builder's default for each it is not given, because a hand-run helper gets nothing from the machine's config: a bf16 LTX-2.5 `--transformer` is 39.13 + 14.32 = 53.45 GiB where the int8 default is 20.03 + 14.32 (`TestAVideoCallIsSizedFromTheFilesItsFlagsName`). `run-graph` is not recognised, and neither is a helper run with `--graph` (`comfy-render`, `comfy-video`): both post the caller's own workflow, so what they load is unknown and they take the class default below, on a text lease too (`TestAGraphCallIsSizedByTheClassDefaultNotItsDefaultFamily`). |
| a file whose size cannot be read | the documented per-family size (`hostneed.familySizes`: the files the reference box binds, rounded up; Qwen-Image 2512 bf16 is the incident's ~38 GiB stream). A family with neither has no estimate and takes the class default. |
| the **media class default** | for a media lease with no `--ram` and no recognised call: the largest estimate over the render families **this box binds** (`imagegen_*`, `gen_edit_*`, `videogen_*`, named families), so a 32 GiB node is not held to a 128 GiB node's numbers; 0 when nothing is bound. It is never clamped to fit the box: a default that cannot be admitted is refused with the reason and `--ram` as the way out. |
| a text lease | 0: the harness cannot size an arbitrary wrapped command, so a text reservation declares nothing unless `--ram` says (the agent seat's own reload is sized where the harness does it, below). |

**Why the full file size and not the overflow.** ComfyUI's dynamic VRAM stages the whole file in host memory ("Model Krea2
prepared for dynamic VRAM loading. 24449MB Staged", the encoder "8463MB Staged" in the incident lane's own console log),
and its RAM-pressure cache keeps it there between prompts. The same principle says a set that fits the card streams nothing
from RAM, so it declares nothing. The media admission (`internal/pipeline`) asks the same estimate per route, from the
binding it is about to render with, once the card it will run on is known: image, edit, inpaint, video (by the family that
will render, with the request's own `transformer` put over the family's binding, because that is the file the runner
loads: `TestAVideoRequestsOwnTransformerIsWhatTheAdmissionDeclares`), animate and music read their files; `run-graph`, sd.cpp and the iGPU engines take the class default; upscale
and voice declare nothing, their weights sit on the card.

**An unknown card counts everything.** A set that fits the card declares 0 only when the card is known. A host that has not
enabled card-scoped leases reads no card table (its leases are whole-node, as before, and it may have no NVIDIA card to
ask), and a table that cannot be read is the same, so there the files count in full: the conservative figure, with `--ram`
as the way to a smaller one (`TestReserveOnAFlagOffHostDeclaresTheFilesInFullWithoutReadingTheCardTable`).

### The admission rule

Every grant path applies **one** rule (`gpuprobe.HostRAMAdmits`), with committed memory read from the OS
(`GlobalMemoryStatusEx`: total minus available page file on Windows; `/proc/meminfo` `Committed_AS` on Linux):

```
projected = committed memory now + the lease's declared need + the not-yet-loaded part of leases already granted
admit iff projected <= physical RAM - gpu_host_ram_headroom_gib        (default 8 GiB)
```

* **Everywhere the cards are granted.** A card-scoped grant (`--devices`, `--cards`, the media admission) checks inside
  `grantDevicesLocked`, under the epoch lock, after the cards are shown free and before the epoch is issued; the record that
  carries the declared need is written in the same critical section, so two grants on different cards cannot both read the
  same headroom: the second one's check sees the first one's record
  (`TestConcurrentGrantsOnDifferentCardsCannotBothTakeTheLastHeadroom`: exactly one of four racers fits, 25 rounds). A
  whole-node grant checks in `TryAcquire` before its epoch is bumped, so a refusal burns none; it needs no lock because a
  whole-node lease conflicts with every other live lease, so nothing else is live to account for. The detached holder
  receives the parent's resolved number as `--ram` and declares exactly that. The card allocator applies the same function
  before it picks cards, advisory (it never refuses what the grant would admit: the media admission passes it the need
  against the largest card); the grant is the authority.
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
* **The not-yet-loaded part.** A granted lease's declared need minus what the processes below its holder hold privately
  right now (`PrivateUsage` through `K32GetProcessMemoryInfo` on Windows, `RssAnon` + `VmSwap` on Linux), never below zero.
  Leases that share a holder pid are pooled (the pipeline holds one lease per card in one server process, and all their
  runners are below it), the holder's own memory is not subtracted (a long-lived server's memory is not the job's), and a
  holder whose memory cannot be read counts as holding nothing, so its whole declared need is pending
  (`TestTheNotYetLoadedPartOfAGrantedLeaseCounts`, `TestPendingPoolsLeasesByHolder`). Without this term the second lane
  reads 20 GiB committed, sees room, and both go: the incident, replayed.
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
  was always allowed), and a place held for a caller who left (a token) still holds against everyone for its 30 s grace.
  The line `waiting for host RAM: needs 33.4 GiB, committed 96.4 of 127.7 GiB physical
  (+12.0 GiB still to load by leases already running), 8.0 GiB headroom` is printed once per request, never per poll.
  `--wait 0` refuses with that text, the flag that waits, and `--ram` as the way out; the media admission answers the same
  words as a queued place in line (resumable doors) or a busy defer (the rest). That holds for a call with no time left
  (`gpu_wait_ms` 0, or a window spent) on every plan shape: a pinned card, an allocated card and the whole node all answer
  the guard's sentence, never the line's "promised to callers ahead of this one" (the cards are free and nobody is ahead;
  `TestWaitZeroKeepsTheGuardsSentenceOnAPinnedPlan` and its siblings, G2 of the P0 plan).
* **A need no state of the box admits is refused at once.** If the need exceeds physical RAM less the headroom, waiting
  cannot help: the request ends with the text (it names `--ram` and `gpu_host_ram_headroom_gib`), and the pipeline classes it
  `gpu_lease_unavailable` (a configuration fault), not `gpu_busy`. `--ram 0` is the escape hatch the operator owns.
* **Unreadable memory.** On a platform that has a reader (Windows, Linux) a lease that declares a need waits until the host
  can be read, the way the other guards fail closed. On a platform with no reader the rule cannot judge and admits.

**Known limits, stated so they are not discovered.** Linux's `Committed_AS` counts mappings a process reserved and never
touched (CUDA and large `mmap`s do), so it reads high there: the safe direction for a guard whose failure is paging. A
`/proc/meminfo` that omits the commit counters (a sandbox that virtualises it) falls back to what the box visibly uses
(`MemTotal - MemAvailable`, limit `MemTotal`), which under-counts reserved memory but keeps the rule working: no reading
would make every declaring lease wait forever. A
detached lease (`--detach`) has no command of its own below its holder, so its whole declared need counts as still to
load for as long as it is held (a budget reservation). A kept ComfyUI whose runner has exited is reparented away from the
holder's tree, so its loaded weights count both as pending and in the commit charge until that lease ends: that
over-refuses, it never under-refuses. A pipeline job running under its parent's ambient lease (`acquireInherited`,
`GPU_LEASE_DEVICES`) gets no admission of its own: the parent's declaration covers it. A process outside every lease can
still push the box over; the guard sees it in the commit charge and holds new declaring leases back, but it does not stop or
evict anything. **Reach (G7 of the P0 plan).** The guard bounds the leases that pass through it. A binary older than the guard
(a pinned copy: its leases declare nothing, so the not-yet-loaded sum undercounts them), a direct llama-swap request (the seat
loads on demand with no lease at all) and a hand-started ComfyUI instance are outside it: it sees what they have committed, never
what they are about to. The closure is to run the live harness everywhere and to make a ComfyUI start refuse without a lease
token; until then the rule rests on those callers going through the lease, and nothing here claims it bounds a process that does not.

### The agent seat and its warm-back (G4 of the P0 plan)

A lease that takes a card unloads the agent seat, and the wrapper warms it back before it releases the lease. That reload
is a load like any other, and it used to be the one load nothing sized: the warm path had no host-RAM check, and the
seat's footprint was declared nowhere. (The 2026-09-10 incident was a seat: 44 GiB of host RAM nobody counted.) Now:

* **The seat declares its footprint.** `agent_seat_host_ram_gib` is the host RAM the seat holds once loaded (resident set
  plus any staged KV cache). The node that runs the seat measures it and records `measured <date> <node>` beside the value.
  Unset is not "small": `hostneed.DefaultSeatHostGiB` (21 GiB, the largest seat footprint on record: a vLLM pair seat's
  ~13 GiB of process plus 8 GiB of staged KV cache, measured 2026-09-10 and recorded in the project notes, not re-measured
  here) stands in, a **chosen** fail-closed figure, never 0 (`TestSeatNeedIsTheConfiguredFigureElseTheFailClosedDefault`).
* **The warm passes the grant's admission.** Committed memory now, plus the seat's footprint, plus what the *other* live
  leases have yet to load, must stay under physical RAM less the headroom (`Manager.HostRAMCheckWithout`: the same function
  as the grant, with the lease being released left out, because its command has exited and it loads nothing more). A host
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

### What `gpu status` and `offload_status` show

`gpu status` (text and `--json`) and `offload_status`'s `gpu_lease` block carry `host_memory`: `physical_gib`,
`available_gib`, `commit_used_gib`, `commit_limit_gib`, `headroom_gib`, `declared_live_gib` (the sum of the host RAM the
live leases declared), `pending_gib` (the part of it still to load), `admits_up_to_gib` and one **verdict**, built by
`gpucards.HostView` for all three surfaces so they cannot disagree:

| host verdict | meaning |
|---|---|
| `OK` | committed memory is more than the headroom below physical RAM |
| `NEAR` | committed memory is within the headroom of physical RAM: the next declaring lease waits |
| `OVER` | committed memory is **above** physical RAM. The reading cannot say whether the box is paging now (that takes page-file growth or the pages-out rate, which no surface here reads), so none of them claims it. New leases that declare host RAM wait until commit is back under physical less the headroom; end a lease or stop a kept ComfyUI instance to free it |
| `unknown` | the reading could not be taken |

The brief verdict line of `offload_status` leads with `HOST RAM OVER (committed memory 162.9 GiB exceeds the 127.7 GiB of
physical RAM)` in capitals, ahead of the lease verdict word, because `free` at the head of that line reads as "the box has room";
NEAR trails it. The lease rows show what each lease declared, and the queue rows what a waiter waits for
(`TestGPUStatusJSONCarriesTheHostBlockAndAnOverVerdict`, `TestStatusNamesOverLoudlyOnTheBriefLine`).

### An instance never holds two families' weights

The paging incident's 57 GiB instance is closed at its source in `render/comfy-family.mjs`: the launch marker remembers
whose weights a kept instance may still hold, a runner that finds another family there frees it before its first job, and
the end-of-run `/free` is awaited, retried and loud; see "A kept instance (`--keep-comfy`)" in
[media-generation.md](media-generation.md).

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
contract placed here again finds a loaded seat; the detach form's counterpart is `gpu release --warm-seat`. That request
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
| `held-working` | a lease is held, the seat is idle, and the cards are busy under it (≥ 15 % utilization) or its progress file is advancing — the holder's own job. With only card utilisation as evidence the note says so (`util only, no progress contract`): a live job and a hung one look alike |
| `held-idle` | a lease is held and NOTHING is running: seat idle, cards quiet — the holder is waiting (a drain, a queue), loading, or stalled |
| `held-stalled` | the lease carries a progress contract (`--progress-file` and `--stall`) and the file did not move inside its window. The holder is alive and heartbeating; nothing is reclaimed or killed |
| `held-orphaned` | an ATTENDED lease whose owner (session or process) has been gone for longer than `gpu_orphan_grace_min` (default 15 minutes): nobody is expected back for it. Never produced for an unattended, remote or unknown-owner lease. Nothing is reclaimed or killed |
| `held-overdue` | the declared window ended and the holder is still alive and heartbeating. Informational: a declared window is not a ceiling for a live holder, so nothing is reclaimed. A holder whose owner vouches for the job renews its term instead (see [Terms](#terms-a-window-is-a-term-and-a-term-ends-in-a-renewal-or-a-label-adr-0070)); when it does not, the lease is labelled **expired** and the note says why (the `--for` default is 45 minutes, so a wrapper that never declared a window and has no live owner reads overdue after that while it still heartbeats) |
| `tree-orphan` | the wrapper is gone but the job it started still holds the cards. In the vocabulary so the precedence is complete; **not produced by this build** (it needs the wrapper to record its process tree) |
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
  Until the registry writer is wired into the session hooks, that is every attended lease taken from a Claude session, so
  an attended job without a progress file reads `expired` at the end of its first term even while its cards are busy
  (the `held-overdue` verdict was already true from that moment; the label adds the sentence and the takeover
  eligibility, and a takeover of an unknown owner will need `--force` anyway). And for an attended lease with a live owner
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
