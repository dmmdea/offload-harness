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
| `gpu_hide_windows.go`, `gpu_hide_other.go` | hidden spawn for the detached holder (a visible console gets closed, killing the hold) |
| `render/gpu-lock.mjs` | READ-ONLY participant: honours + fences an inherited lease, elects one unloader, drains, ComfyUI lifecycle. **Does not acquire.** |
| `internal/gpulock` | the read-only vision gate; delegates wholesale to `gpulease.InspectDir` |
| `internal/modelaffinity/gpuwait.go` | READ-ONLY: text admissions that would make llama-swap load a model wait out a `media` holder (ADR 0026); armed from `config.Load` via `LeaseDir` |
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

Neither is defensive padding: with waiters polling once a second, both races are ordinary
traffic. Measured under six concurrent acquire/release workers, 1 in 48 cycles failed before the
retries were added.

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

## Draining a seat before a window (0.113.16)

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
(`vllm:num_requests_running|waiting`, `llamacpp:requests_processing|deferred`) through `/upstream/<model>/metrics`, two
consecutive zeros required. A llama.cpp seat started without `--metrics` answers that path `501` (older builds `404`); since
0.113.19 the drain then reads llama-server's `GET /slots` and counts `is_processing` slots — llama-server's deferred queue is
not listed there, and the two-consecutive-zeros rule is what covers it (a queued request becomes a processing slot the instant
one frees). Any other non-200 from `/metrics` is still "could not read", never idle: the drain fails at the deadline. At
`--drain-timeout` the wrapper form releases the lease and exits non-zero; the detach form keeps
the lease (the card stays reserved, work keeps routing elsewhere) and exits non-zero so the caller does not start.
`--unload-seat` (requires `--drain`) then frees the cards through `POST /api/models/unload/<model>` (legacy `GET /unload`
as the fallback). The wrapper form warms the seat back (`GET /upstream/<model>/health`) BEFORE releasing, so the first
contract placed here again finds a loaded seat; the detach form's counterpart is `gpu release --warm-seat`.

Why: a gate that unloaded the production seat by hand collided with a delegation that made llama-swap reload it mid-profile
(`No available memory for the cache blocks`, 2026-09-06 15:23), and the Lenovo's measurement windows stopped its fleet node
outright, cutting in-flight remote work. With the lease advertised and enforced, the window is a lease, not an outage.

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
- **Head-of-line blocking is structural** — a 45-minute video blocks everything behind it.
- ~~`internal/pipeline` does not yet take a `media` lease around its own generation calls.~~
  **No longer true, corrected 2026-08-19.** `acquireMediaLease` wraps every generation route in
  `internal/pipeline/pipeline.go`: image-gen, image-gen (sdcpp), inpaint, edit, image-gen batch,
  run-graph, video-gen and audio-gen. The one deliberate exception is `runPipelineJob`, which
  takes the in-process `mediaSlot` only and documents why; its nested per-stage calls do take the
  lease. This bullet had gone stale and then directly contradicted the rule added above it.
