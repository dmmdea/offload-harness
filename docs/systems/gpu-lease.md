# GPU lease

## Purpose

The machine-wide, fenced, two-class lease that serializes GPU-heavy work across every
ingress — the harness CLI, the Node render runners, the fleet node, and anything else that
takes it. It exists so a text measurement and a media generation can no longer destroy each
other on a single shared card.

- **CLI:** `local-offload gpu status|reserve|release`
- **State:** `<state_dir>/gpu/{lease/meta.json, epoch}` — default `%ProgramData%\local-offload` on
  Windows, `/var/lib/local-offload` on Linux (**see the one-time Linux setup below**)
- **Decision record:** [ADR 0018](../architecture/decisions/0018-machine-wide-fenced-gpu-lease.md)

## Source map

| path | role |
|---|---|
| `internal/gpulease/gpulease.go` | **the only implementation**: `LeaseDir` (the one resolver), acquire/release, epoch fencing, the reclaim conjunction, `InspectDir` (the one read path) |
| `internal/gpulease/proc*.go` | per-platform liveness and process-start identity, exported so no consumer keeps a second copy |
| `internal/pipeline/pipeline.go` | takes the `media` lease around **every** generation call site (image ComfyUI + sdcpp, inpaint, image batch, run-graph, video, audio) and threads `GPU_LEASE_*` to the runner; inherits an ambient lease instead of re-acquiring; owns the in-process slot (`mediaSlot`) that arbitrates jobs sharing one inherited lease |
| `gpu_cmd.go` | the `gpu status\|reserve\|release\|hold` verbs, wrapper and `--detach` forms |
| `gpu_drain.go` | `--drain`: waits until the seat's gauge AND the run registry are empty, inside the queue budget; restamps `draining` → `exclusive`; unload / warm-back |
| `internal/gpuactivity` | the RUN REGISTRY (`<state root>/gpu/activity/`, one record per agent loop in flight) and the activity reading behind `gpu status` / `offload_status.gpu_lease` (`verdict`, `activity`) — ADR 0041 |
| `internal/seatload` | the seat's in-flight reading: llama-swap's `/running` (and the roster, for an alias), then the loaded seat's own `/metrics` or `/slots` at the `proxy` `/running` reports — never through `/upstream`, which resets the idle unload timer; a `starting` seat is reported without touching the seat |
| `gpu_hide_windows.go`, `gpu_hide_other.go` | hidden spawn for the detached holder (a visible console gets closed, killing the hold) |
| `render/gpu-lock.mjs` | READ-ONLY participant: honours + fences an inherited lease, elects one unloader, drains, ComfyUI lifecycle. **Does not acquire.** |
| `internal/gpulock` | the read-only vision gate; delegates wholesale to `gpulease.InspectDir` |
| `internal/modelaffinity/gpuwait.go` | READ-ONLY: text admissions that would make llama-swap load a model wait out a `media` holder (ADR 0026); armed from `config.Load` via `LeaseDir` |
| `internal/modelaffinity/upstream.go` | READ-ONLY: `AwaitUpstream`, the ONE builder of a llama-swap `/upstream/<model>/…` URL, and `AwaitModelRoute`, the builder for a model-dispatched route (`/v1/chat/completions`, `/v1/embeddings`, …) sent without `Admit` — every probe, tokenizer, warm-up, transcription, chat-lane forward and embedding passes the same fence as a generation (see "Probes pass the fence too") |
| `internal/config` | `state_dir`, `gpu_lock_path` |

## Why it exists

One GPU, two very different consumers: llama-swap wants its tiers resident; ComfyUI wants the
whole card. Before this, only the Node render runners took a lock, so text work had no way to
say "I am using the GPU." A media job dispatched from elsewhere unloaded every GPU-resident
model mid-benchmark. The server log holds **3,356 unload calls, 330 of them the text
workhorse, and 0 for the CPU memory stack** — that last zero identifies `freeLlamaSwap` as the
caller, since excluding the memory stack is its own invariant.

## Using it

Prefer the **wrapper** form. The lease lives exactly as long as the command, so it cannot be
leaked by forgetting to release, and it composes like `timeout` or `nice`:

```
local-offload gpu reserve --class text --for 45m --reason "kv bench" -- <command...>
```

`--detach` holds the card in a hidden background process for an interactive session. It is the
weaker form by design — nothing ties the lease to a command's lifetime, so **`--for` is REQUIRED with
`--detach` (0.113.27)**: the holder exits at that deadline and releases whether or not the work has
finished, and the old 45-minute default silently freed the card mid-job, after which the next render
claimed it and unloaded the seat on top of the running work. Declare the real window, or wrap the
command:

```
local-offload gpu reserve --class text --for 45m --reason "kv bench" --detach
local-offload gpu release --epoch <N>
```

`gpu status [--json]` reports the holder, its class, age, reason and declared expiry — and says
**"free (unreserved)"** explicitly, because an unreserved card is exactly when work is exposed;
that should be visible, not inferred from silence. When held it ends with the one line that
matters: how to **queue behind it** (`queue_with` in the JSON).

### A held card is a place in line, not a refusal (0.115.2)

`gpu reserve` **queues** behind a current holder — `--wait` (default **8h**) is how long it
stands in line before giving up; `--wait 0` restores the old fail-fast. Both forms queue: the
wrapper waits in place, and `--detach` passes the wait to the hidden holder (`gpu hold`), which
is the process that actually takes the card, while the parent reports `reserved:` only once that
has happened (or the holder's exit, when the line did not move in time). The wait prints exactly
two lines on stderr — `queued behind <holder> — waiting up to <wait>` on entry and `acquired after
<n> in the queue` on exit — never one per poll, because the session wrapping this would turn a
poll line into a notification each.

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
| `gpu reserve --detach` | a hidden holder polls once a second until released or expired | the only continuous poller; exits on its own |

`--detach` is the sole thing that keeps running after the command that started it, which is why
the wrapper form is preferred. It is an ordinary process, not a registered service: it exits by
itself when released, when fenced out, or at its declared deadline, and nothing respawns it.

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

Both are exclusive — one card, one holder. The label carries intent, not access control.

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
`fleet-serve` would un-gate itself. See
[ADR 0026](../architecture/decisions/0026-text-load-admissions-wait-for-the-media-lease.md).

**Delegate placement reads a `text` reservation as "not here" (0.113.14).** `internal/delegate`
resolves the lease through the same `LeaseDir` + `InspectDir` path (`LocalLease`, never acquired) and,
on route=auto and route=spread, a held `text` lease removes the local seat from placement: an eligible
remote takes the contract; with none, the runner waits up to `agent_lease_wait_sec` and then defers
(class `infrastructure`, holder named) rather than loading the reserved cards — the 2026-09-05 case
where three foreign contracts landed on a reserved two-card seat mid-measurement. `route=local` is
not gated, and a `media` holder only steers (the affinity gate above arbitrates it), so the sentence
before this one still holds for interactive text calls: a `text` reservation does not block them.
Since 0.113.18 that wait is the delegator's **capacity wait** (`agent_placement_wait_sec`, default
120 s, or `agent_lease_wait_sec` when longer): it watches the lease AND every remote's room, so a
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

**Alias-bound seats (0.113.20).** llama-swap's `/running` lists CANONICAL ids, while `agent_model` is normally an alias (`agent-pool` → `qwen3.8-27b-vllm`). The drain's reader matched `/running` by the configured name, so on an alias-bound seat it read "not loaded" and returned at once — a silent no-op from 0.113.16 to 0.113.19 on the reference workstation (the Lenovo, whose seat is bound by its id, drained correctly, which is why the live proofs passed). The reader now lives in `internal/seatload` and resolves the name through the roster before consulting `/running`; an unreadable roster falls back to the bare name.

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
as the fallback). The wrapper form warms the seat back (`GET /upstream/<model>/health`) BEFORE releasing, so the first
contract placed here again finds a loaded seat; the detach form's counterpart is `gpu release --warm-seat`.

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
  budget, neither says anything about acquisition order). This governs ordering among REGISTERED waiters only: a
  brand-new `Acquire`'s very first, pre-registration probe (and any bare `TryAcquire` that never sets `Wait`) can
  still land in the narrow window between a release and the front waiter's next poll — the same residual race
  every poll-based queue has, bounded by one poll interval, and unrelated to the hours-long starvation this fixes.
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
- **The warm is heartbeat for its length** (`drainRenewEvery`, 15 s), so a 27B load of several minutes cannot go stale
  under the 120 s heartbeat TTL; losing the lease mid-warm cancels the request and is reported.

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
(`No available memory for the cache blocks`, 2026-09-06 15:23), and the Lenovo's measurement windows stopped its fleet node
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
stale = dead/recycled pid or heartbeat > 120 s, swept by readers). The drain is done when the engine's gauge
AND the registry are empty on two consecutive reads; a seat listed `starting`/`stopping` counts as busy and
its upstream is never probed. Registering before the warm-up is also what puts the load path behind the
fence: the pre-0.117.0 warm-up loaded the seat straight past an exclusive hold.

**Reading it.** `gpu status [--json]` and `offload_status.gpu_lease` carry a one-word `verdict` and an
`activity` block:

| verdict | meaning |
|---|---|
| `working` | a request or a registered run is in flight on the seat (lease held or not) |
| `held-working` | a lease is held, the seat is idle, and the cards are busy under it (≥ 15 % utilization) — the holder's own job |
| `held-idle` | a lease is held and NOTHING is running: seat idle, cards quiet — the holder is waiting (a drain, a queue), loading, or stalled |
| `loaded-idle` | no lease; the seat is resident with nothing in flight (unloads at its ttl) |
| `busy-outside` | no lease, seat idle, cards busy — work the harness does not own (the processes are listed) |
| `stale-holder` | a lease record whose holder is gone; the next acquirer reclaims it |
| `free` | no lease, nothing in flight, cards quiet |

`activity` carries `seat` (name, loaded, starting, inflight, source), `runs[]` (kind, pid, origin, goal,
phase, step, tokens_out, age), `gpus[]` (index, name, util_pct, mem), `gpu_processes[]`, and `holder`
(pid, alive, command, heartbeat_age_s, draining, exclusive). The drain's progress line is built from the same
reading and printed on CHANGE (count, load state, a run's step), with a reminder every five minutes.

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
| whisper transcription | the client's timeout | a `timeout`-class defer naming the holder |
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
