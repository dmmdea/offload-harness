# Fleet node — joining the fleet-dispatcher fleet (`fleet-serve`)

Operator guide for running this box as a **fleet node**: a small HTTP server that lets the
Fleet Dispatcher send GPU render jobs (image / video / audio / stt / run-graph) to this
machine through the same pipeline, GPU lock, and zero-always-warm lifecycle every local call
uses — and, since 0.65.0, lets a delegator send **agent** jobs (self-contained sub-agent
contracts, see [The agent task](#the-agent-task-task_type-agent) below). Wire protocol:
fleet-dispatcher `CONTRACT.md` **v2** — three endpoints, JSON everywhere, GiB everywhere,
every failure a non-2xx.

| Endpoint | What it returns |
|---|---|
| `GET /fleet/health` | node id, live global VRAM (total/free GiB), supported task types + model families **derived from this box's actual config**, measured per-family VRAM footprints, queue depth, whether the boot-time GPU power profile took ([`power_profile`](#boot-time-gpu-power-profile-power_profile)), the driver's fault summary per card ([`gpu_faults`](#driver-fault-summary-gpu_faults)). Never blocks on a render. `503` when the VRAM snapshot is unavailable **or older than 30s** (nvidia-smi failing, e.g. a driver reset) — a stale 200 would mislead routing. |
| `POST /fleet/dispatch` | immediate `202 {"job_id": <exact echo>, "status": "accepted"}`; the render runs async through the pipeline. Duplicate `job_id` → 202 re-ack (accepted/running/**done** — poll `/fleet/jobs/{id}` for the state), never a second render; a job that previously **failed** here answers `409` (an explicit refusal, so the dispatcher may try another node). |
| `GET /fleet/jobs/{id}` | `{"state": "accepted\|running\|done\|error", ...}` with `data` on done / `error` on error, plus `err_class` (the class the failed run's lane filed its deferral under, only when it filed one) beside `error`; terminal results retained ~1h; 404 for unknown/evicted ids. |
| `DELETE /fleet/jobs/{id}` | Takes back an **agent** job that has not started ([ADR 0064](architecture/decisions/0064-a-delegator-takes-back-what-it-has-not-started.md)): `200 {"state":"withdrawn","withdrawn":true}` when it had not (it never will; a job the node itself already took back answers the same), `409 {"state":"running"\|"done"\|"error","withdrawn":false}` when it had (the node does not touch it), `404` unknown id, `401` without the agent bearer, `405` for a non-agent job. See [Taking a job back](#taking-a-job-back-withdraw-and-the-poll-lease). |

## Quickstart

```powershell
# Loopback bring-up (default 127.0.0.1:18811)
local-offload fleet-serve

# Verify from the same box
curl http://127.0.0.1:18811/fleet/health

# Production: bind the TAILSCALE address so the dispatcher (on another box) can reach it
local-offload fleet-serve --listen 192.0.2.7:18811 --listen-trusted-network
```

Startup resolves a **GPU memory provider** (J3): `nvidia-smi` first (a working NVIDIA node
behaves exactly as before), else the OS's generic source — on Windows the **windows-generic
WDDM source** (capacity from the display-class registry `qwMemorySize`, usage from the
`\GPU Adapter Memory` PDH counters), on Linux the **linux-amdgpu sysfs source** (since
0.130.6: `/sys/class/drm/card*/device/mem_info_vram_*` plus, on a UMA profile, the driver's
own `mem_info_gtt_*` pool — the real shared budget, not a RAM/2 estimate). A unified-memory
SoC tier (`rockchip-rk3588`) has no VRAM counter at all, so on Linux it gets the
**linux-meminfo source** instead: `/proc/meminfo` less the operator's `uma_reserve_gib` —
capacity is `MemTotal − reserve` and free is `MemAvailable − reserve`, clamped to `[0, capacity]`
— so the home-automation stack that shares the board keeps its memory. Vendor/arch come
from the installer's `installed.json` profile, and the UMA memory model from the profile (an
iGPU advertises carve-out + shared budget as its total, and dedicated+shared as usage). Only
when **no memory source works** does `fleet-serve` refuse to start: the contract treats
`vram_total_gb <= 0` as a broken node, and refusing loudly beats advertising an empty GPU.
The serve log names the resolved source
(`... via nvidia-smi|windows-generic|linux-amdgpu|linux-meminfo, vendor=... arch=...`). A **dual-route node**
(ADR 0054: a tier with `alt_backends`, installed with `--llama-bin-cpu`; no tier declares the CPU route since 2026-09-24) advertises `backends`
in health, primary first (`["vulkan","cpu"]`), and serves the CPU family as `<seat>-cpu` ids —
a caller picks the route by seat id. `backends` is the hardware tag list and nothing else: which serving *engine* answers for
each lane and seat (`llama.cpp`, `vllm`, `rkllm`, ...) is the separate `lane_backends` and `seat_backends` of the same payload,
each row carrying `active_backend`, `backend_chain`, `backend_probe` and `backend_reason`; read from llama-swap's roster and
running list without loading anything, and described in [systems/fleet-node.md](systems/fleet-node.md#which-engine-answers-for-each-lane-and-seat-active_backend). Ctrl-C drains: dispatches for a
job_id this node has never seen get 503; a re-dispatch of a job_id this node already knows
about (running, done, or previously failed) still re-acks 202 — or 409 if it previously
failed — even mid-drain, since that's not new work. On a node with a `fleet_auth_token`, an agent or
vision job's id is answered only to a request that carries the bearer, whatever `task_type` that request
declares: without it the answer is `401` (never the 202 or the 409, which would say the job exists, or
echo what its failure said).

**Backlog and concurrency are two different limits** (0.100.0). Before 0.100.0 they were
one: accepting a job started it, so `accepted` lasted microseconds and the single cap
`fleet_max_queue_depth` bounded *simultaneous execution* while being named and documented as
a backlog. A node reporting "depth 31" was running 31 jobs at once against one llama-swap
endpoint. A dispatch is now **admitted** and waits its turn.

| Key | Bounds | Default | Refuses? |
|---|---|---|---|
| `fleet_max_queue_depth` | `accepted` + `running` (health's `queue_depth`), **all task types** | 2x `fleet_max_concurrent_jobs` (8 with the default 4 workers, 2 with one — register S-04/C-25) | Yes — `503 queue full` |
| `fleet_max_concurrent_jobs` | jobs actually executing, **`agent` and `text` jobs** (every task type that reaches the shared text endpoint, and a task type nobody has listed yet; media, stt, accel, compose and configured pipeline routes are exempt) | 4 (1 on a tier whose agent seat serves one slot: a node publishes the workers its seat really has, see [OPERATOR-GUIDE](OPERATOR-GUIDE.md)) | No — extra jobs WAIT in `accepted` |
| `fleet_stt_max_concurrent` (0.165.0) | stt jobs actually executing, **both stt lanes** (`stt` and `stt-upload`, pushed and pulled) | 1 | No — extra jobs WAIT, in arrival order, inside their run (their card stays queued). Inference stays serialized by the whisper client's process-wide mutex whatever the value, so above 1 it only overlaps conversions and queue time |
| `fleet_stt_upload_max_mb` (0.165.0) | the largest audio file `POST /fleet/stt` takes (decoded MiB; the body is that in base64 plus slack) | 48 | Yes — `400` naming the key |
| `fleet_stt_transcript_ttl_min` | how long an stt upload job's transcript files stay under `media_dir` (minutes); `0` = 30, negative = keep | 30 | No — files are removed at the next sweep after they pass it |

**The concurrency cap governs the text lane only.** It exists to protect one thing — the shared
llama-swap endpoint, where the measured defect was N simultaneous inferences against a single
serving slot. `image-gen`, `video-gen`, `audio-gen`, `run-graph`, `stt` and every configured
pipeline route are **exempt** (stt has its own cap, `fleet_stt_max_concurrent`, see
[The stt upload door](#the-stt-upload-door-post-fleetstt)), because capping them would be both redundant and backwards: media
already serializes itself at capacity **one** (the in-process `mediaSlot`, under the machine-wide
`gpulease` ClassMedia), and a media job blocked waiting for the card would hold a fleet execution
slot while doing no work — four of them would starve the very lane the cap protects. It would also
suppress media's own back-pressure, since a job held in `accepted` never reaches the wait that
produces a `gpu_busy` defer. An unrecognized future task type is capped by default: that way of
being wrong is visible, the other is silent. Media scheduling is therefore unchanged from 0.99.0.

> **Deploy note.** The media dispatcher (a different repository) sets its poll deadline at dispatch
> time and has no queued-time credit. The exemption above is what keeps it correct — media jobs
> never linger in `accepted`, so they never spend a contract's budget waiting for a slot. **Do not
> cap a media task type until that dispatcher credits queued time.**

Both take `0` = built-in default and a negative value = unlimited. `fleet_max_queue_depth`
keeps exactly the meaning it has always had — the ceiling on `queue_depth`, which still
counts `accepted` + `running` — so the refusal boundary did not move; what it no longer does
is double as the concurrency limit. Under any setting, actual concurrency is now less than
or equal to what the same config produced in 0.99.0.

A **busy node is not a full node**: with `fleet_max_concurrent_jobs: 4` and its default
`fleet_max_queue_depth: 8`, the 5th dispatch is accepted and queued, and only the 9th is
refused (a one-worker node, the seed of a tier whose agent seat serves one slot, runs 1, queues 1 and
refuses the 3rd). The refusal check runs before request materialization; re-acks of jobs this node
owns and result polls are never refused by it. The default is sized FROM the concurrency the
node actually has — twice its worker count — rather than a flat number regardless of box
size: a node admitting 32 deep behind 4 workers could pile up 28 jobs with no hope of
starting inside any wall a caller would wait out (register S-04/C-25; 236 measured contracts
died at the delegator's 5-minute queue deadline having never started). An explicit
`fleet_max_queue_depth` still wins outright, and an over-tight cap suppressing real use
remains the worse defect than a generous one.

> **Since 0.101.0 the `503` sheds to a sibling** — within a bound, and only for the refusals
> that are about THIS node. `internal/delegate` re-places a refused subtask: another eligible
> remote first, then the local seat. That covers `503` (queue full, draining, stale VRAM
> snapshot), `409`, `429`, `404`, `408`, any 5xx, and a node the delegator could not reach at
> all. It deliberately does **not** cover `400`/`401`/`403` or any other 4xx — those are the
> node rejecting *this request*, and the next node is handed byte-identical bytes and the same
> bearer token, so re-placing only collects the same answer while spending the contract's
> budget. See [Re-placement on refusal](#re-placement-on-refusal) for the bound, the deadline
> rule and the outcome when every node refuses.
>
> This is NOT the media dispatcher's rule imported. That dispatcher (a different repository)
> treats *any* non-202 as "try elsewhere"; this one classifies first, re-places at DISPATCH
> time only, and never after a `202` — a job the node has acked may be running, and moving it
> would arrange two concurrent runs.
>
> Widening `fleet_max_queue_depth` is still the better lever than lowering it: a job that waits
> gets done on the node that already has the context warm, whereas a re-placed job pays a fresh
> placement out of its own budget.

On shutdown, in-flight renders get up to 30s to finish. Survivors are then marked terminal so
pollers always reach a terminal state, with the two cases kept **distinct**: a job that was
executing becomes `error: "interrupted"`, and a job still sitting in the backlog becomes
`error: "not started: node shut down while this job was queued"` — it never ran, which is a
different fact about the node and much cheaper to act on. Drain finishes what it started; it
does not start what it never began.

Health publishes the split additively: `queue_depth` keeps its original meaning and shape,
and `jobs_running`, `jobs_queued`, `max_concurrent_jobs`, `max_queue_depth` sit alongside it
(always present — `0` is a real value, and for the two limits it means *unlimited*, which a
reader must be able to tell apart from a pre-0.100.0 node that publishes no limit at all). With the agent lane on, health also
publishes `jobs_running_capped` (register C16): the part of `jobs_running` that counts against `max_concurrent_jobs`. `jobs_running`
counts every job on the node, a render or a transcription included, so a node with four renders running and its agent workers free
reads "all busy" by it; the delegator that decides whether a queued job is waiting for a worker reads this one. A pointer, so a
published `0` is not an absent key; absent on a node that predates it or whose lane is off, which a delegator reads as unknown.

## Running as a Windows scheduled task

Linux nodes run under systemd. On Windows, run the shipped template
`setup/templates/start-fleet-node.win.ps1` (replace `__OFFLOAD_HOME__` and `__NODE_ID__`)
from a scheduled task, invoked through a hidden `wscript`/VBS shim so no console window is
ever shown. Registration that actually survives a reboot (proven on the ampere-8 reference
box, 2026-08-25):

- **Principal: `-LogonType S4U -RunLevel Limited`, as the harness user — not SYSTEM, not
  Interactive.** SYSTEM resolves a different `%USERPROFILE%`, so `fleet-serve` silently runs
  on built-in defaults instead of `~/.local-offload/config.json`. Interactive cannot carry a
  boot trigger, so an unattended reboot leaves the node down until someone logs in. S4U runs
  as the user with no interactive session and no stored password.
- **Trigger: `-AtStartup`** (with the launcher's own readiness loop absorbing early-boot races).
- **The config the task reads holds `fleet_auth_token`.** Every lane requires the fleet bearer, and `fleet-serve` will not
  start on the Tailscale address without the key: put it in that config before registering the task (the same value on every
  delegator that calls the node). A node started without it exits at once, and the launcher's "exited immediately" line carries
  the refusal, which names the key and the config file it read.

Three measured gotchas the template encodes — do not "simplify" them away:

1. **Under S4U (session 0) the Tailscale CLI returns nothing.** `tailscale ip -4` produced
   empty output and the old launcher spun silently. Read the address from the interface:
   `Get-NetIPAddress -InterfaceAlias 'Tailscale'`.
2. **At boot the Tailscale address exists before it is bindable.** `fleet-serve` died with
   `bind: The requested address is not valid in its context` even though the resolve
   succeeded. Readiness is a REAL bind test (open + close a `TcpListener` on the exact
   address:port), retried — never a resolve.
3. **Cycling the task does NOT reload config.** The detached `fleet-serve` child survives a
   task stop, and the launcher's duplicate-guard then sees 18811 listening and declines — so
   the OLD process keeps serving the OLD config, while the task reports success. To pick up a
   config change: kill the `local-offload` process, then start the task.

**Swapping this node's binary to a new release: use `local-offload node-swap`, launched via
`setup/windows-node-swap-launch.ps1`, never a hand-adapted one-off script.** It verifies the
staged exe's hash, waits for the queue to be idle, stops the task, renames the old exe to a
backup (diagnosing and clearing only an idle MCP-helper holder if the rename fails — never a
live server), installs the new one, restarts the task, and proves the swap by PID + running
image sha256 + `/fleet/health` within a timeout. **Every failure from the moment the node is
stopped onward — backing up the old exe, installing the new one, the optional render-tree
swap, restarting, or the post-restart proof — triggers automatic rollback**: restore whatever
moved, restart, and re-verify the OLD binary is back up and healthy; the node is never left
stopped with nobody restarting it. Launched through the WMI/CIM path, it survives the
launching SSH session ending, which a `Start-Process`-based script does not (see
[systems/node-swap.md](systems/node-swap.md) for the 2026-09-24 outage this replaces the risk
of repeating).

The advertisement (`/fleet/health` `supported_task_types`) is derived from the node's OWN
config at process start — a route bound in the config after the process started (e.g. adding
`imagegen_script`) does not advertise until the process is restarted the hard way above. Since ADR 0077
the media tasks (`video-gen`, `animate`, `audio-gen`, `run-graph`) are the exception in the other direction: a
bound route is advertised only while `internal/mediacap` reads it CONFIGURED from the disk (re-read at most every
60 seconds), so a weight that goes missing drops the task without a restart and `media_routes` says why.

## Two utilization figures, and which one answers which question

`/fleet/health` publishes both, and they are not interchangeable:

| field | means | who reads it |
|---|---|---|
| `gpu_util_pct` | the busiest card on the WHOLE box (PAIR's multi-GPU rule — the shared card is the one that matters) | the fleet overview, any dashboard |
| `work_util_pct` | the busiest card **the harness can actually run a seat on** | the delegator's placement tie-break |

They differ exactly when someone is using the machine. On 2026-09-20 <node-b> read
`gpu_util_pct: 33` from a game on its display card while every card the harness could use sat at
0%, and because placement broke ties on that number the node lost work it should have won. A
display card is identified by the card's OWN properties, not by config and not by inference:
`nvidia-smi --query-gpu=display_active,display_attached` rides the per-device query the health
sampler already runs, and `gpuprobe.DisplayCardUUIDs` reads `display_active` from it — the SAME
function the GPU lease verdict, the foreign-load guard and the delegator's free-card count use. A
card is the display card FOR LOAD ATTRIBUTION when `display_active` reads `Enabled` (a display is
initialised: a game, a lit screen). `display_attached` (`Yes`: a monitor is plugged into one of its
connectors) is read too, but only by the card table and the allocator (`gpuprobe.ScreenCardUUIDs`),
which must not auto-pick the monitor's card even while `display_active` reads `Disabled` on every
card because the screen sleeps (measured 2026-10-03 on the 3-card box). It is deliberately not part
of the load rule: an attached monitor is true for the whole life of the box, and counting it would
take that card out of `work_util_pct` and the free-card count even while the node's own seat or
lease works on it. Only an exact `Enabled` / `Yes`
counts; `[Not Supported]` and `[N/A]` mean "we do not know", which never reads as "this is the
operator's screen". A driver that refuses the `display_attached` field (an older one, a headless
Linux build) rejects the whole query, so the reader retries without it and the rule rests on
`display_active` alone, as before; the full query is tried again after ten minutes. The node
publishes `display_attached` in `gpu_devices[]` (omitted when false; an older node omits it and
reads as "unknown"). Nothing is refreshed on a separate cadence and nothing is carried forward:
the answer comes from the sample the tick already took. On a headless box every card answers
`Disabled` / `No`, nothing is flagged, and the two figures agree.

A display card is excluded only when a non-display card exists: a single-GPU box runs its seats
on its display card by necessity, and there the two figures are equal by construction.
`work_util_known` is the validity flag. A node that predates the field publishes neither, and the
delegator then ranks THAT node on `gpu_util_pct` — the choice is made per node, never per pair.
Picking the figure per comparison is what made the ordering cycle on a mixed fleet mid-rollout;
see ADR 0057.

## Multi-GPU: `gpu_devices[]` and the headline VRAM numbers

On any node whose VRAM source is `nvidia-smi` or `linux-amdgpu`, `/fleet/health` always adds a per-device
breakdown — **including a single-GPU box**, which reports a one-element array. There is no
single-vs-multi-GPU special case: `gpu_devices[]` is present whenever one of those is the resolved
source, full stop (`chooseSamplerKind` and `startVRAMSampler` in `main.go`, unit-tested at that exact seam in
`fleet_verbs_test.go` and `fleet_sampler_test.go`). A `linux-amdgpu` row carries the card's index, name
(`product_name`, else `amdgpu <pci device id>`) and memory composed as the node's memory probe composes it
(an APU: carve-out plus the GTT pool; a discrete card: VRAM alone); its `uuid` is empty and it carries no utilisation, so
`primary_gpu_uuid` has no effect there and the delegator reads its cards as unknown, never as busy. It is
**absent** on a source that cannot enumerate devices: the Windows PDH/windows-generic path
(`vram_windows.go`), which has no per-adapter identity to report, and `linux-meminfo`, the RK3588 SoC's one
RAM pool, which has no cards to list. A `linux-amdgpu` node whose per-device read fails at startup, though
the gate probe just worked, keeps the single reading and omits the array (it logs why).

```json
"vram_total_gb": 15.93, "vram_free_gb": 15.08,
"gpu_devices": [
  {"index": 0, "uuid": "GPU-1111aaaa-2222-3333-4444-555566667777", "name": "NVIDIA GeForce RTX 5060 Ti", "vram_total_gb": 15.93, "vram_free_gb": 15.08},
  {"index": 1, "uuid": "GPU-8888bbbb-9999-cccc-dddd-eeeeffff0000", "name": "NVIDIA GeForce RTX 5070 Ti", "vram_total_gb": 15.92, "vram_free_gb": 13.46}
]
```

The delegator reads these rows (GPU routing P1): `delegate.NodeView.Devices` carries them, a card
that drives no display in use (`display_active` false; `display_attached` is not read here), a known `util_pct` under 15 and enough `vram_free_gb` (or a loaded seat
that explains its absence) counts as a free card, and placement prefers a node with one over a node whose busiest card reads lower. No
`gpu_devices` key (an older node, or a source that cannot enumerate devices) is unknown, never
"no free card". `offload_status` shows the count as `free_cards` of `cards_total`. See
[systems/fleet-node.md](systems/fleet-node.md), "Free cards".

`gpu_devices` is **additive** — `vram_total_gb`/`vram_free_gb` keep exactly the meaning they
always had, and adding a new field breaks nothing on the consumer side in practice: the
fleet-dispatcher decodes health with a plain `json.Decoder` (no `DisallowUnknownFields`
anywhere in its `internal/`), so an extra field it doesn't know about is silently ignored, not a
wire error, on any node — single-GPU included.

**Temperature and throttle reasons.** On an `nvidia-smi` node each `gpu_devices[]` row may also carry
`temperature_c` (an integer, degrees C, nvidia-smi `temperature.gpu`) and `throttle_reasons` (an array naming the
clock event reasons active on the card). An absent `temperature_c` means "not read", never "cool": a node that
predates the fields, a driver that answers `[N/A]` for the card, a thermal query that failed and an answer older than
90 s all read the same way. `throttle_reasons` has three states, so an absent key is never "not throttled": the names of
the reasons active; `[]` when the card was read and nothing limits its clocks (no reason beyond idle); and no key when
the throttle column was not read (a node that predates it, a driver that answers `[N/A]` or `[Not Supported]` or has
no throttle field, a failed query, an answer older than 90 s). A `temperature_c` outside 1 to 150 is a garbled counter
and is left out. Placement does not read either field; whether a heat-soaked node is demoted is a separate decision.

```json
{"index": 0, "uuid": "GPU-1111aaaa-2222-3333-4444-555566667777", "name": "NVIDIA GeForce RTX 5060 Ti", "vram_total_gb": 15.93, "vram_free_gb": 15.08,
 "temperature_c": 83, "throttle_reasons": ["sw_thermal_slowdown", "hw_thermal_slowdown"]}
```

A card the query read with nothing limiting its clocks reads `"temperature_c": 41, "throttle_reasons": []`; one whose throttle
column was not read carries `temperature_c` alone.

They come from a second, optional query (`index,uuid,temperature.gpu` plus the throttle field), never from the
per-device memory query above, so a driver that refuses a thermal column cannot cost the memory reading; the row is
matched to its card by UUID, never by index. The sampler ticks every 2 s but the thermal query runs **at most once per
30 s** (one extra `nvidia-smi` process per 30 s on each node), a failed query is not retried sooner than 60 s and is
logged once per outage, an answer older than 90 s (counted from when its query started) is dropped rather than
published, and the read is bounded at 10 s. The query runs on a goroutine of its own, one at a time, and the 2 s probe
only reads what the last one stored, so a hung `nvidia-smi` delays neither the memory reading the placement guards use
nor the node's start; its answer reaches the next tick. The throttle field is named `clocks_throttle_reasons.active` on older drivers and `clocks_event_reasons.active` on newer
ones: the reader asks for the older name first, asks for the renamed one when the driver refuses the first (the name
that worked is remembered, so the refusal costs one spawn once), and falls back to the temperature alone when the
driver knows neither. The bits are NVML's, read from its header (`nvml.h`, the `nvmlClocksEventReason*` and
`nvmlClocksThrottleReason*` defines):

`0x2` `applications_clocks_setting`, `0x4` `sw_power_cap`, `0x8` `hw_slowdown`, `0x10` `sync_boost`, `0x20`
`sw_thermal_slowdown`, `0x40` `hw_thermal_slowdown`, `0x80` `hw_power_brake_slowdown`, `0x100` `display_clock_setting`,
`0x200` `board_limit`, `0x400` `reliability`. `0x1` (the clocks dropped because the GPU is idle) is never listed, and a
bit above the table is published as `unknown:0x<bit>` so a header that grows later is not silently dropped.

`board_limit` and `reliability` are in the CUDA 13.4 header and not in 13.3's. `reliability` is the reliability policy
limiting the clocks; it has been read on every card of a healthy three-card box, so read the list beside
`temperature_c`, not as an alarm of its own. The delegator decodes the rows into the same `gpuprobe.Device`
(`delegate.NodeView.Devices`), so both fields reach it with no change on its side, and a delegator built before them
ignores them.

**Why this exists:** `nvidia-smi` enumerates devices in PCI bus order — that order has no
relationship to which device a render actually runs on. A CUDA app (ComfyUI included) can bind
its `cuda:0` to a different physical card via `CUDA_DEVICE_ORDER=FASTEST_FIRST`, so trusting
"index 0" for the headline VRAM numbers can silently advertise the WRONG card's free memory —
an idle donor card looking free while the real compute card is mid-render, which lets the
dispatcher over-admit a second job that then contends or OOMs.

**The default headline rule** (no `primary_gpu_uuid` set): `vram_total_gb`/`vram_free_gb`
describe the device with the **largest total VRAM**; an exact tie in total is broken by
whichever card has more free VRAM right now. A single render binds to one device, so the
admission-relevant number is the biggest device a job could actually land on — never an
arbitrary enumeration index, and deliberately **not a sum** across cards (summing would let the
dispatcher admit a job no single card can hold). This rule is a defensible, deterministic
heuristic, not a way to detect which card CUDA will actually pick — nvidia-smi carries no such
signal. On a box with two near-identical-capacity cards it can pick either one; the full
`gpu_devices[]` breakdown exists precisely so a consumer that needs the real per-card numbers
(or a smarter dispatcher) isn't limited to the headline guess. Implementation:
`fleetnode.ParseSmiMemoryDevices` / `fleetnode.HeadlineDevice` in `internal/fleetnode/vram.go` —
since 0.116.0 aliases over `internal/gpuprobe` (`gpuprobe.Device`, `ParseSmiMemoryDevices`,
`HeadlineDevice`, `NvidiaSmiRunner`), the leaf the placement guards share so one parser serves both.

### `primary_gpu_uuid` — pin the headline device deterministically

The largest-total rule cannot tell two near-identical-capacity cards apart, and total VRAM says
nothing about which card CUDA will actually compute on. **Canonical guidance (CMP tier notes):
pin by GPU UUID, NEVER index.** Set `primary_gpu_uuid` to one of the UUIDs read off this node's
own `gpu_devices[]` (see the JSON example above) and that device — regardless of its total or
free VRAM — becomes the headline `vram_total_gb`/`vram_free_gb`:

```json
{ "primary_gpu_uuid": "GPU-8888bbbb-9999-cccc-dddd-eeeeffff0000" }
```

On <node-b>, `gpu_devices[]` shows the RTX 5060 Ti at index 0 with the marginally larger total
(16311 vs 16303 MiB), so the default largest-total rule headlines it — but ComfyUI's CUDA
ordering actually computes on the RTX 5070 Ti (index 1, verified via ComfyUI's own
`/system_stats`). Pinning `primary_gpu_uuid` to the 5070 Ti's UUID
(`GPU-8888bbbb-9999-cccc-dddd-eeeeffff0000`) makes the health payload finally describe the card
that is actually doing the work.

Behavior: unset (`""`, the default) = the largest-total rule, unchanged. Set and found among the
parsed devices = that device wins outright. The match is case-insensitive, and the config value
is whitespace-trimmed at load — both matter because the UUID is meant to be copy-pasted straight
out of `gpu_devices[]` above, and a copy can pick up a trailing newline or land in a different
case than nvidia-smi's own lowercase output. **Set but not found** (typo, or the card was
removed/reseated) = falls back to the largest-total rule **and** logs one
`[fleet-serve] warning: primary_gpu_uuid "..." not found among N parsed GPU device(s)...` line to
stderr (once per process lifetime, not once per 2s sampler tick) — a silent fallback would hide
a typo'd UUID forever. No effect on a single-GPU node beyond confirming what's already true (its
one `gpu_devices[]` entry is already the headline); no effect at all on the windows-generic
source, which has no `gpu_devices[]` to match against. Implementation:
`fleetnode.SelectHeadlineDevice` in `internal/fleetnode/vram.go`.

## Boot-time GPU power profile: `power_profile`

A box can apply a GPU power limit and a clock lock at boot with a separate tool. When that step is skipped, refused or blocked the card runs at stock limits and nothing else says so, so `/fleet/health` publishes whether it took, in the same words the `power:` row of `doctor` uses (see the [operator guide](OPERATOR-GUIDE.md), "A power profile applied this boot"). Both read one judgement of one file: the state record the boot-apply tool writes on every run, found only by the environment variable `LOCAL_OFFLOAD_POWER_PROFILE_STATE` (the harness knows no tool's install folder, so there is no platform default and no config key).

```json
"power_profile": {"state": "ok", "limit_w": [250, 150], "clock_lock": true, "applied_at": "2026-01-02T14:00:00Z"}
"power_profile": {"state": "skipped", "reason": "cookie_blocked", "applied_at": "2026-01-02T14:00:00Z"}
"power_profile": {"state": "unknown", "reason": "no_state"}
```

| field | means |
|---|---|
| `state` | `ok`: a profile was applied since this boot began, whole (the `power:` row reads OK). `skipped`: the boot apply did not take, or took in part (the row reads WARN: no apply this boot, a dry run, a refusal, a failure, an empty plan, the tool's crash guard, a step short of the plan, a lever skipped other than by design, a driver that differs from the profile's stamp). `unknown`: the node cannot say (the row reads SKIP or UNKNOWN). |
| `reason` | absent when `ok`. Otherwise one token, never free text and never a path: `no_state` (no setting, or no file there), `unreadable`, `bad_state` (not JSON, no usable time, a time in the future), `bad_schema`, `not_due` (the box booted under ten minutes ago and the boot apply has not run yet), `boot_unknown` (applied whole, but the platform cannot say when the box booted, so "at boot" cannot be confirmed; the doctor row reads OK there), `stale_boot` (the newest record is older than this boot), `partial`, `dry_run`, `unrecognised`, or the tool's own outcome word for a run that ended without applying (`failed`, `refused`, `cookie_blocked`, `plan_empty`). |
| `limit_w` | the driver's power limit of each card in watts, in the record's card order, as the record read it at the end of the run. Published only when `state` is `ok`, so a skipped run never advertises a limit it did not set. Absent when the record lists no card. |
| `clock_lock` | `true` when a step that locked the clocks returned success. The record has no field for it, so it is read from the step's description and only that fact leaves the node. Absent means none was recorded, not "unlocked". Published only when `state` is `ok`. |
| `applied_at` | the record's own time (RFC 3339, UTC) of the most recent run, whatever its outcome. Absent when the record could not be read. |

A node that applies no profile (a Linux node with no boot-apply tool, any box with the variable unset) publishes `{"state": "unknown", "reason": "no_state"}`: a published answer, not an error, and not a finding. A node one release behind publishes no `power_profile` key, which a reader takes as unknown. The block carries no path, host name or card id, so a caller without the bearer reads it too.

**It reads the record, not the card.** The record says what the tool did, so a limit changed after the apply still reads `ok` until the harness has a reader of the live limit and clock lock. The node reads the file at most once every 30 seconds.

`offload_status` shows one short token per node in the `fleet` block, and the `brief` form carries it because it carries the whole block: `power_profile` is `ok`, `skipped` or `unknown`, and `power_profile_reason` follows when it is news (not for an `ok` node and not for `no_state`). The key is absent for a node one release behind. The local node's own state is the `power:` row of `doctor`.

## Driver fault summary: `gpu_faults`

What `local-offload gpu faults` reads on a node (the NVIDIA driver's fault records over 30 days, grouped into incidents, the
ones at a restart or sleep transition labelled, NVSentinel's repeat rule applied; see
[gpu-lease.md](systems/gpu-lease.md#gpu-faults-the-fault-history-as-incidents-with-known-errors)), reduced per card and
published so the fleet sees which card has been faulting without a login to each box.

```json
"gpu_faults": {
  "window_days": 30, "read_at": "2026-10-24T08:54:35Z",
  "cards": [
    {"index": 1, "uuid": "GPU-...", "bus": "65:00", "events": 21, "incidents": 11, "incidents_24h": 0, "last_incident": "2026-10-20T14:33:58Z"},
    {"index": 2, "uuid": "GPU-...", "bus": "b5:00", "events": 19, "incidents": 15, "in_context": 2, "incidents_24h": 0, "last_incident": "2026-10-19T04:06:20Z"}
  ],
  "repeats": [{"uuid": "GPU-...", "bus": "65:00", "class": "nvlddmkm 153", "bursts": 5, "from": "2026-10-11T08:43:01Z", "to": "2026-10-11T10:05:04Z"}]
}
```

| field | means |
|---|---|
| `window_days`, `read_at` | the window read and when (UTC). A reading is refreshed every 10 minutes and published for at most 40, so `read_at` is the age a reader judges. |
| `cards[]` | every NVIDIA card in today's table: `index` (nvidia-smi), `uuid`, `bus`; `events` and `incidents` count every record and burst naming the card, `in_context` the bursts among them at a restart or sleep transition; `incidents_24h` counts the bursts outside one that started in the last 24 h, and `last_incident` is when the newest of those ended. |
| `repeats[]` | NVSentinel's RepeatedXIDErrorOnSameGPU met on a card: one class (`nvlddmkm 153`, `Xid 79`, ...) in `bursts` (5 or more) bursts within 24 h, `from` and `to` being the first and last of them. Bursts at a restart or sleep transition are left out. A repeat is LIVE while `to` is within the last 24 h. |
| `note` | why the reading may be short: a log read that stopped part way, a card table that could not be read, a retention file that could not be read or written. |

**What placement does with it** ([ADR 0096](architecture/decisions/0096-a-live-driver-fault-repeat-demotes-a-cards-placement.md)).
A repeat is LIVE while its `to` is within 24 h of the delegator's clock. A node whose every agent seat for the contract sits
on a card with a live repeat (the seats from its layer rows, as for a lease; any live repeat where nothing narrows) ranks
right after saturation, below every node with a seat off such cards, in the auto placement (`betterRemote`) and the spread
deal (`fitPickWith`). It is demoted, never excluded, and a node that lost on it is narrated as
`faulted (nvlddmkm 153 x5 on 65:00, last burst 10-11 10:05Z)`. The delegator's own box reads its summary file: a live
repeat under every local seat of a subtask reads the seat busy for the deal (route=auto's joint reading, route=spread's
local slot) when a remote could take the work, never for route=local; the capacity wait reads the seat afresh, so an
overflow subtask may still come back to it. A node with no `gpu_faults` is never demoted.

**Facts, served from memory.** The read takes seconds (3.4 s on the reference box), so health never runs it: fleet-serve
reads in the background, starts the first read at startup, and keeps the last good reading when a read fails. The key is
absent until the first read lands, on a node with no NVIDIA card, and on a node one release behind; a reader takes absent
as unknown, never as "no faults". The block carries card uuids and buses (already on this payload as `gpu_devices[]`) and
counts, no path, host name or text, so a caller without the bearer reads it too. Each refresh also appends the incidents
that ended to `<state root>/gpu/faults.jsonl` and rewrites `<state root>/gpu/faults-summary.json`
([gpu-lease.md](systems/gpu-lease.md#retention-and-the-fleet-nodes-summary)).

## Config keys

| Key | Default | Purpose |
|---|---|---|
| `fleet_listen` | `127.0.0.1:18811` | Bind address (`--listen` overrides). Port **18811** — the dispatcher owns 18810. |
| `fleet_node_id` | `""` | Node id in `/fleet/health`. Empty = the OS hostname at serve time (`--node-id` overrides). The id `fleet-serve` resolves is also the one its result envelopes (`node_id`) and its queue claims carry (0.185.0; before, a node launched with `--node-id` stamped its hostname on its results). |
| `fleet_sampler` | `auto` | Per-render VRAM footprint source: `auto` \| `pdh` \| `pdh-shared` \| `global` (see [Sampler modes](#sampler-modes)). |
| `primary_gpu_uuid` | `""` | Pins the headline `vram_total_gb`/`vram_free_gb` to one card by nvidia-smi UUID, overriding the largest-total rule (see [`primary_gpu_uuid`](#primary_gpu_uuid--pin-the-headline-device-deterministically) above). Empty = unchanged largest-total behavior. |
| `uma_reserve_gib` | `0` | RAM (GiB) a unified-memory SoC node (the `rockchip-rk3588` tier, whose GPU and NPU share system RAM) holds back from inference for the box's own workload. Health advertises `MemTotal − uma_reserve_gib` as `vram_total_gb` and `MemAvailable − uma_reserve_gib` as `vram_free_gb` (the linux-meminfo source). `0` = advertise all of RAM; the tier seeds `3`. Inert on every other tier, which reads a real VRAM counter. A reserve that leaves no capacity makes `fleet-serve` refuse to start, and a negative one is refused by name. |
| `fleet_agent_enabled` | `false` | Opts this node into executing fleet **agent** jobs (see [The agent task](#the-agent-task-task_type-agent)). Explicit opt-in: the binding (an agent seat) exists on every tier, the worker ROLE is a per-box decision. Off = the task is not advertised and health is byte-identical to a pre-0.65 node. |
| `fleet_poll_lease_sec` | `0` (= 60) | How long a pushed agent job may sit `accepted` with nobody polling it before the node treats its delegator as gone: the job is never started and is reaped, and a run that finishes after its poller left stops feeding `recent_agent_wall_sec` and Retry-After. Negative = off; a value under 15 is raised to 15 (the delegator's own gap between polls must fit inside it). A running job is never reaped. See [Taking a job back](#taking-a-job-back-withdraw-and-the-poll-lease). |
| `fleet_auth_token` | `""` | Bearer token for **every lane** of the node (a dispatch of any task type, the poll and withdraw of any job, the read of any artifact; `401` without it). Same value on every node and in every delegator's config. Empty = open on a loopback listener only; any other listener refuses everything with `403`, and `fleet-serve` will not start. See [Auth](#auth-every-lane-needs-the-fleet-bearer). |
| `fleet_ingress_guard` | `""` (= `warn`) | The ingress guard in front of every route (register SF-16), against a web page in a browser reaching the node (a cross-site `text/plain` POST, DNS rebinding). `warn` (the default) logs a request `enforce` would refuse and serves it; `enforce` refuses a `Host` that is not loopback, a tailnet address, a dotless MagicDNS name or a name under a zone in `tailnet_suffix` / `tailnet_suffixes` (421), any `Origin` header (403) and a POST that is not `application/json` (415); `off` skips the checks. A mistyped mode fails the config load. The node prints `ingress guard: <mode>` at start. Detail and the edges to know before `enforce`: [systems/fleet-node.md](systems/fleet-node.md). |
| `agent_ctx_tokens` | `0` | The agent seat's served context window, advertised in health for the delegator's placement arithmetic. From config, never probed (a live probe could cold-start a multi-GB model on the health cadence). `0` = not advertised = this node is never chosen for remote agent work. |
| `kvslot_cap_gib` | `0` (= 8 GiB) | Cap on the node's `kvslots/` directory, the KV-slot files `POST /fleet/kvslot/save` writes (ADR 0056 Layer 2). An LRU sweep after every save deletes the least recently modified files past the cap. The directory is created by the installer; without it the lane answers `501` and the delegator falls through. |

## Binding guidance (read before exposing anything)

**Every lane requires the fleet bearer** (see [Auth](#auth-every-lane-needs-the-fleet-bearer)):
the media lanes run renders on this GPU and the agent lane executes caller-supplied agent
contracts (details in [The agent task](#the-agent-task-task_type-agent)), so a node that
listens beyond loopback will not start without a `fleet_auth_token`. The same binding
rules as `local-agent --serve` apply, enforced by the same shared guard:

- **Loopback is the default** and needs no flag.
- A non-loopback `--listen` is **refused** unless you pass `--listen-trusted-network`
  (and then `fleet_auth_token` must be set, or the node refuses to start). The flag permits ONE specific address: an all-interfaces
  address (`:18811` with an empty host, `0.0.0.0`, `[::]`) is refused even with it (0.144.1), so a
  unit whose `$(tailscale ip -4)` is still empty at boot fails and its restart retries instead of
  serving on every interface.
- Production binding is the machine's **Tailscale address** (e.g. `192.0.2.7:18811` on
  your workstation) — the tailnet is the network boundary and the fleet bearer is the credential.
  **NEVER bind `0.0.0.0`**, and never expose the port beyond the tailnet.
- Port **18811** per the house port discipline; update the machine's port file in
  the operator's port ledger when you stand a node up.
- **The ingress guard (`fleet_ingress_guard`) is not authentication, and its default does not refuse.** The default,
  `warn`, logs a request that `enforce` would refuse and serves it, so a node you bind beyond loopback is open to a
  browser page until you run `warn`, read the log and set `enforce`. The node prints `ingress guard: <mode>` at start
  (and `doctor` prints it), so read that line. `enforce` stops a browser page and a DNS-rebinding name; a tailnet peer
  that is not a browser still reaches the listener, where the fleet bearer is the check. A node shared in from another tailnet needs that tailnet's zone in
  `tailnet_suffixes` first. See [systems/fleet-node.md](systems/fleet-node.md) for the three rules and the edges.

## Footprints — measured, not guessed

`/fleet/health` advertises `model_footprints[]`: **measured** per-(family, quant, task)
VRAM peaks, including this box's offload strategy, stored at
`~/.local-offload/footprints.json`. The dispatcher uses them for admission, ignoring any
entry with `vram_peak_gb <= 0` (we never write those).

Recording is **passive**: every GPU render through the pipeline — normal harness use, not
just fleet jobs — samples VRAM while the child process runs and folds the observed peak
into the store (max-keep; `vram_peak_gb = observed_max`, rounded to 0.1 — the **raw** peak,
no padding). Footprints therefore stay current when bindings change: a new model family
simply starts a new entry. The node adds **no** margin: the dispatcher owns all routing
margin (CONTRACT v2.1 / ADR 0013). A node that padded its own ×1.2 on top of the
dispatcher's margin double-inflated footprints and made wan2.2/hidream unroutable on a 16 GB
node — so don't pad the store by hand.

The same sampling tick also reads the **host** memory of the render's process tree and folds two more peaks into the same
entry: `host_peak_gb` (private bytes, the commit-side figure) and `host_resident_peak_gb` (resident set, the RAM figure),
with the number of runs they come from. Both are additive and omitted until measured. They are for this node's own
host-RAM guard, which raises a render's declared need to a high percentile (the 90th, nearest rank) of the resident peaks of its last
twenty runs once at least three runs exist, clamped at twice the model files' size (it never lowers one; the fields themselves stay
the all-time maxima)
([GPU lease, "How the numbers get measured"](systems/gpu-lease.md)); a dispatcher may ignore them. Private and resident
are both kept on purpose: on Windows a process's GPU allocations may be charged to its private bytes without occupying
RAM, so the two differ exactly where the question is open.

### Priming an empty store: `fleet-measure`

A freshly-installed node has no footprints, so its health advertises none and the
dispatcher has nothing to admit against. Prime it:

```powershell
local-offload fleet-measure
```

One minimal render per configured task — image (512×512, 8 steps), video (the fast
distilled recipe at 9 frames, reusing the probe image as the still), music (5s) — then the
store's on-disk records print as JSON. Voice and run-graph are skipped (no cheap universal
probe); their footprints accumulate passively during normal use. Renders run through the
normal pipeline, so the store records exactly what fleet jobs will cost.

## Sampler modes

Windows GeForce cards with a display attached run **WDDM**, where NVML's per-process VRAM
accounting returns N/A — `nvidia-smi` can only see **global** memory. So the harness has two
sources:

| Mode | Source | What it measures |
|---|---|---|
| `pdh` | Windows PDH counter `\GPU Process Memory(pid_*)\Dedicated Usage`, summed over the render's **process tree**, sampled every 500ms during renders only | What OUR job costs — uncontaminated by the desktop, browsers, or other apps. The same counter Task Manager and Afterburner surface. |
| `pdh-shared` | The same PDH process tree, summing **Dedicated + Shared** Usage | The UMA/iGPU mode (J3): on unified memory (amd-rdna3 tier) allocations land in SHARED and the Dedicated counter reads ~0 — footprints would silently never record. The `amd-rdna3` config_seed sets this; discrete boxes should not (Shared is noise there). |
| `global` | `nvidia-smi` global `memory.used` delta from a baseline captured at render start | The whole GPU's swing during the render — includes anything else that allocated meanwhile. |
| `auto` (default) | PDH (Dedicated) on Windows, global-delta elsewhere | The right default once PDH is validated (below). |

The PDH counter set has a documented accuracy caveat on some driver/Windows combinations —
hence a one-time validation at bring-up. One honest quirk to expect: the counter set often
shows **bogus values for the `dwm.exe` instance** (the desktop compositor); that is a known
WDDM artifact and harmless here — our tree-sum only includes the render process and its
descendants, never dwm.

### Validation procedure: PDH vs Afterburner (once per box)

1. Install and open **MSI Afterburner**, open its monitor (the graph window), and enable the
   per-process VRAM plot: **"Memory Usage \ Process"** (per-process dedicated VRAM — the
   capability nvidia-smi cannot provide under WDDM).
2. Run `local-offload fleet-measure` (or any real render — `generate-image` works).
3. When it finishes, read the recorded `observed_peak_gb` for that render's entry
   (`fleet-measure` prints it; or open `~/.local-offload/footprints.json`).
4. Compare against the peak Afterburner showed for the render process (the ComfyUI python /
   node child) during the run.
5. **Agreement within 15%** → PDH is trustworthy on this box; leave `fleet_sampler` on
   `auto`. **Disagreement over 15%** → set `"fleet_sampler": "global"` in this machine's
   config: the global-delta source is coarser but never lies about the ceiling.

## Recommended companion: MSI Afterburner

**Recommended, never required** — the harness has no dependency on it, reads nothing from
it, and every fleet feature works without it. But on a WDDM box it is the operator's best
instrument, and we explicitly encourage running it alongside a fleet node:

- **Per-process VRAM under WDDM.** `nvidia-smi` cannot attribute VRAM to processes on a
  WDDM GeForce card; Afterburner's *Memory Usage \ Process* plot can. It is the independent
  reading our PDH sampler is validated against.
- **Validation role.** The bring-up procedure above is the one place the harness asks you
  to look at it; after that it's a live sanity check whenever a recorded footprint looks
  off.
- **Dashboards.** Afterburner exports every plotted metric through the **MAHM shared
  memory** interface (`MAHMSharedMemory`), which third-party dashboards and small scripts
  can read — useful if you want per-process VRAM on a fleet-wide panel. The harness itself
  never reads MAHM (companion only, by design).

## Task surface

Advertised tasks are **derived from this box's config**, never hardcoded — an unbound route
is not advertised, so the dispatcher can't send work the box would defer. A media task
(`video-gen`, `animate`, `audio-gen`, `run-graph`) is bound by its ComfyUI/python script **or** by the native
engine that replaces it (sd.cpp for video and animate, audio.cpp for voice and music, CT-49), and is advertised and
admitted only while `internal/mediacap` reads its route as CONFIGURED (`/fleet/health` `media_routes` says which route
is missing what): a bound script or engine over a missing weight drops the task, and restoring the file brings it back:

| Fleet `task_type` | Pipeline task | Advertised when | Footprint family |
|---|---|---|---|
| `image-gen` | `generate_image` | `imagegen_script`/sdcpp engine bound (the default binding) OR any `imagegen_families` entry configured — a family-only node (no default binding, e.g. an opt-in-family-only node) is advertised too | `imagegen_family` (else `sdxl`, only when the default binding is actually configured); quant `bf16` for the HiDream-O1 binding |
| `video-gen` | `generate_video` | `videogen_script` set, **or** the default family bound to the sd.cpp engine (`videogen_families.<name>.engine: sdcpp`) — and `generate_video` CONFIGURED | the `videogen_family` binding (`ltx25`, …), else `wan2.2` for the runner default; an sd.cpp default family advertises its own name (the `wan22` sentinel keeps `wan2.2`); quant `q8_0` only for the Wan family, when the bound unets are the Q8_0 GGUFs |
| `animate` | `animate_character` | `animategen_script` set, **or** `animategen_engine: sdcpp` — and `animate_character` CONFIGURED | `wan-animate2` (ComfyUI WAN-Animate-2); `wan-vace` for the sd.cpp Wan2.1 VACE lane (`config.AnimateSdcppFootprintFamily`, the family that lane records its footprint under) |
| `stt` | `transcribe` (a path on THIS node's disk) | `stt_model` set | `whisper` (llama-swap-resident — no footprint sampling). Bearer-gated when the node has a `fleet_auth_token` (0.165.0); shares the stt concurrency cap below. |
| `stt-upload` (own route `POST /fleet/stt`, 0.165.0) | `transcribe` over audio bytes the caller sends | `stt_model` set **and** (loopback listener **or** `fleet_auth_token` set) | `whisper`, as `stt` |
| `audio-gen` | `generate_audio` | a voice renderer (`voicegen_script`, `tts_endpoint` or `voicegen_engine: audiocpp`) or a music renderer (`musicgen_script` or `musicgen_engine: audiocpp`) bound — and at least one of `generate_audio:voice`, `generate_audio:voice:endpoint`, `generate_audio:music` CONFIGURED | `acestep` (ComfyUI music, and the only family a box with no audio.cpp engine advertises); with audio.cpp, the engine's own family names: `ace_step` (music) and `chatterbox` (voice) |
| `run-graph` | `run_graph` | `run_graph_script` set (ComfyUI only: no native engine runs a graph) — and `run_graph` CONFIGURED | payload-declared `model_family`, else `comfy-graph` |
| `agent` | `agent` | `fleet_agent_enabled` **and** a resolvable agent seat **and** (loopback listener **or** `fleet_auth_token` set) | none — llama-swap-resident text work, no render footprint |
| `vision` (own route `POST /fleet/vision`) | `vqa` / `ocr` / `assess_image` (narrowed by the node's `vision_tasks`) | `vision_model` set **and** (loopback listener **or** `fleet_auth_token` set) — see [The vision task](#the-vision-task-post-fleetvision) | none — llama-swap-resident VLM, no render footprint |
| `text` (own route `POST /fleet/text`, 0.154.0, dark) | `classify` / `extract` (never summarize or triage), as the node's `text_tasks` names them | `text_tasks` non-empty **and** (loopback listener **or** `fleet_auth_token` set) — see [The text task](#the-text-task-post-fleettext) | none — the node's own cascade seat, no render footprint |
| *(config-driven)* | `pipeline-job` | a valid `pipelines.<task_type>` entry (see below) | none — sizing rides on the task-scoped `Record("", "", task_type, peak)` entry |

run-graph payloads carry `graph` and `manifest` as **raw nested JSON** (no base64) and are
strict-validated at ack time — a malformed fleet job dies at the 400 with a clear reason,
never mid-render. Typed run-graph defers (`VENV_INCOHERENT`, `SATISFIER_SPAWN_FAILED`, `NODE_CLASS_MISSING`, …)
surface in the job's `error` field as `code: detail`.

**A fleet media job's lease window is its route's own timeout, read from the node's config.** A media task takes the
node's machine-wide media lease for the timeout key of the route it runs (`imagegen_timeout_sec`, `videogen_timeout_sec`,
`inpaint_timeout_sec`, `upscale_timeout_sec`, `gen_edit_timeout_sec`, `animategen_timeout_sec`, `audiogen_timeout_sec`; the
payload carries none, so the delegator's keys play no part) and the render is killed at that same value, so a render never
outlives the lease taken for it. The window is granted whole, whatever its length, and the lease is kept by the job's
heartbeat (renewed every 15 s while the render runs): no term check runs on it, so nothing labels it expired or asks it to
yield while the render is inside its window, and `gpu_max_term_min` does not bound it (that cap is the renewal term of a
`gpu reserve` hold, renewed by the hold's own holder). A job that cannot get the card first queues for `gpu_wait_ms` (90 s by
default; a queue wait, not a lease length) and then defers `gpu_busy`. A long seeded timeout therefore costs one thing: how
long a wedged render holds the node's lease. Which keys a tier seeds, the code defaults and the rule for sizing a
`gpu reserve --for` window (from the key of the route it wraps) are in the engine + lease row of the timeout chain section
of [OPERATOR-GUIDE.md](OPERATOR-GUIDE.md).

**A run-graph also checks the files it names, at the door** (ADR 0082, Amendment 1). Before it writes a temp file, takes a lease
or starts ComfyUI, the node reads the graph for the model files its loaders open (any string input, under any key but
`filename_prefix`, whose value ends `.safetensors`, `.gguf`, `.pth`, `.pt`, `.ckpt` or `.sft` and does not contain `://`; those
two are the exclusions the code keeps) and the files its input loaders read (`LoadImage`, `LoadImageMask`,
`LoadImageOutput`, `LoadVideo`, `LoadAudio`). Each weight file must exist **by name** under any class directory of any of the node's
models roots (`<comfy_dir>/models/*`, every `extra_model_paths.yaml` entry; a class directory that is a link counts); each input
file must be in `<comfy_dir>/input` (`output` or `temp` when its `[output]` / `[temp]` annotation says so). A node
that lacks one answers **503** (the re-placeable "this node cannot run this now", the same status as a route that is not CONFIGURED)
naming what is missing, up to six names of each kind (past six weight files it says `7 or more files ... and more` and gives no
count: the node stops looking at the seventh, because any holder of the fleet bearer can send a graph and a graph can name thousands of files; the
delegator's `cluster[]` row quotes the body's `error` sentence cut at 600 bytes, so it shows the first few of a long list), and
creates no job. The body is `{"status":"error","error":"...","refusal":"not_capable"}`: the `refusal` member is the node's word that
it is not full and not out of route but cannot run THIS graph (a route that is not CONFIGURED and a full queue leave it out), so a
delegator marks the node not-capable for that ticket alone and puts no pause on it (ADR 0083 Amendment 1); it places the graph on
the next node instead of this one cold-starting ComfyUI to defer. **Input files do not travel with a graph** (`/fleet/dispatch` carries JSON, and the
media-job door carries no input for `run-graph`), so a file the node does not hold cannot be sent to it; the refusal says so. A
model the graph's manifest tells the node to fetch (a `models[]` entry with a `source_url`) is not refused (a manifest whose models leave `models/<class>/` or come from a host that is not listed is refused at the door, see [Auth](#auth-every-lane-needs-the-fleet-bearer)). The check is by name
(a same-named file of other bytes is held; a delegator therefore never offers a graph that reads an input file to a node, though a
call that must leave its machine anyway, `route remote` or `auto` on a machine with no graph lane, reaches this check with one) and a name that is absolute or climbs with `..` is never looked up.
Health gains no key for it: a delegator requires the node to run its own release, on every remote placement of a graph (the
overflow, `route remote` and a thin client alike), which proves the check only because the release that adds it carries a new
version number. A graph that names no file is admitted as before, on a node with no models root too.

### Video, animation and audio rows (ADR 0082 Amendment 3)

`/fleet/health` carries `media_recipes[]` for a node whose video, animation or music binding is ComfyUI (or whose video family is sd.cpp): one
row `{lane, name, default, engine, digest, files:[{role, name, bytes}], license, commercial_use}` per family, with `bytes` -1 for a
weight the node does not hold. It is additive (a node with no ComfyUI install, `comfy_dir` being no directory on it, and no sd.cpp video
family omits the key, whatever scripts its config binds; one with the install lists every lane its config binds, with -1 for each weight
it lacks) and **a row is not an identity**: no fingerprint, an empty `digest`. A caller that names a node (`node` on `offload_generate_video`, `offload_animate_character`,
`offload_generate_audio`; `--node` on the verbs) is refused before any POST when the row of the family it asks for lacks a file. See
[media-generation.md](systems/media-generation.md#naming-a-node-a-video-animation-or-audio-call-runs-on-the-one-node-you-pick-adr-0082-amendment-3).

### Image recipes carry a content fingerprint (ADR 0082 Amendment 2)

A node that serves `image-gen` publishes `image_recipes[]` in `/fleet/health`: one row per ComfyUI image binding, each with its
weight files (`role`, `name`, `bytes`, `fingerprint`) and a `digest` a delegator compares. The `fingerprint` is a sha256 of the
file's size and a SAMPLE of its bytes (64 evenly spaced blocks of 1 MiB; the whole file when it is 64 MiB or smaller), so two
files of one name and one size with different weights no longer digest alike; a change confined to the stretch between two sampled
blocks (about a sixty-third of the file, roughly 0.3 GB of a 20 GB checkpoint) is not seen, so compare whole-file hashes after you
swap in a layer-wise merge. **Health never waits for it.** For a few seconds after the node starts, or after a weight file changes, that
file reads `"fingerprint":"pending"` (`"unreadable"` if it cannot be read, tried again after 30 s) and the row's `digest` is
empty; it fills in on the next health read once the file has been read (this process reads one weight file at a time, 64 MiB at
most each; a model load in another process is not coordinated with). A read that has not finished in 60 s is given up on: that file
reads `"unreadable"`, the others are read, and the harness logs it once. The node re-checks a `recipe_digest` at admission from the
files then on disk (waiting for each fingerprint it has not got, for 20 s at most per file or until the request is gone; a file still unread is
named `pending`) and answers **412** on a mismatch or on a file it cannot fingerprint; the body names the `cause` (`pending`: files still
being read, so it cannot say yet whether they changed; `unreadable`; `changed`) and the delegator then reads health again. **Every node must run the same release for digests to match**: the
digest is version 2, and a delegator compares recipes only between nodes on its own release, so upgrade the fleet before relying
on a match. The cache is in memory (path, size, modification time); a restart reads each file's sample again (0.17 to 0.87 s per file on the one workstation measured, page cache not controlled; a cold or network disk was not measured).
Details: [media-generation.md](systems/media-generation.md#overflow-a-busy-lane-places-an-image-call-on-the-fleet-adr-0082).

## Pipeline-job task families

`pipelines` (`internal/config` `Config.Pipelines map[string]PipelineSpec`) lets this node
serve an **externally-provided pipeline CLI** as its own fleet `task_type` — 100%
config-driven, unlike every hardcoded row in the table above: adding a new pipeline needs no
code change, only a new `pipelines.<task_type>` entry:

```json
{
  "pipelines": {
    "scene-swap": {
      "script": "D:/pipelines/creative-marketing-pipelines/scripts/run-scene-swap.mjs",
      "workdir": "D:/pipelines/creative-marketing-pipelines",
      "timeout_sec": 2400,
      "artifacts": ["final.png", "qa-report.json"],
      "max_ref_mb": 24
    }
  }
}
```

`script`/`workdir`/`timeout_sec`/`artifacts` are required (an invalid entry is rejected LOUDLY
at config load, never silently at dispatch); `ingress_allow`/`max_ref_mb` are optional (default
allowlist / 24 MiB per ref). The task_type key (`"scene-swap"` above) is dispatched exactly
like any other fleet task — it is advertised in `/fleet/health`'s task list once the entry is
valid, and refused with the usual `unsupported task_type` 400 otherwise.

### Payload

```json
{
  "task_type": "scene-swap",
  "payload": {
    "job_spec": { "id": "web-1a2b3c4d", "background": { "mode": "generate", "...": "..." }, "...": "..." },
    "image_refs": {
      "product": "http://<node>:<port>/api/uploads/<name>.png",
      "logo": "http://<node>:<port>/api/uploads/<name>.png",
      "background": "http://<node>:<port>/api/uploads/<name>.png"
    },
    "tier": "16gb"
  }
}
```

**Ack-time validation** (every rule below is checked, and every ref fetched, BEFORE the job is
accepted — a bad payload or an unreachable ref is a 400, never a mid-render surprise):

- `job_spec` is required and must be a JSON object with a slug-valid `id`
  (`^[A-Za-z0-9_-]{1,64}$` — it becomes the materialization dir name and a filename prefix on
  every published artifact) that does not start with `agent-local-`, the name prefix a
  delegator process keeps its own local runs under in the same `pipeline-jobs/` root.
- `tier` is a required non-empty string (the CLI's own tier resolution is authoritative).
- `image_refs.product` and `image_refs.logo` are required; `image_refs.background` is
  required **iff** `job_spec.background.mode == "stock"`.
- `job_spec` must **not** already contain `product`, `logo`, or `background.path` — the node,
  never the dispatch payload, injects those from the fetched `image_refs` (a payload trying to
  set them itself is rejected outright).
- `job_spec.id` must **not** already have an in-flight job on this node: the materialization
  dir is created with an exclusive `os.Mkdir` (not `MkdirAll`), so a second dispatch sharing an
  id with a still-running job is refused at ack (`"job_spec.id ... already in flight on this
  node"`) rather than sharing the directory — which would otherwise let the first job's
  eventual cleanup delete the second job's still-live assets/`job.json` out from under it.
- Every `image_refs` URL goes through the same allowlisted/capped/magic-sniffed ingress every
  other fetched ref uses (tailnet CGNAT + loopback + this pipeline's `ingress_allow`; PNG/JPEG/
  WebP only; capped at `max_ref_mb`).

On success the node materializes `<base_dir>/pipeline-jobs/<job_id>/`: `assets/` (the fetched
refs), `job.json` (`job_spec` with `product`/`logo`/`background.path` injected as absolute
local paths), and `out/` (the CLI's `--out` root — the CLI itself creates `out/<job_id>/` and
writes its artifacts there). This whole directory is removed once the job reaches a terminal
state (success or failure) — never before, since the CLI reads `job.json` and the fetched
assets for the entire run.

### Result + publishing

```go
type pipelineJobResult struct {
    FinalPath    string  `json:"final_path"`               // FIRST key — becomes artifacts[0]
    QaReportPath string  `json:"qa_report_path,omitempty"` // artifacts[1], when produced
    JobID        string  `json:"job_id"`
    Tier         string  `json:"tier"`
    DurationSec  float64 `json:"duration_sec"`
}
```

`final_path` is marshaled **first on purpose** — the web viewmodel harvests artifacts in
insertion order, so `artifacts[0]` must be the primary render. Every configured `artifacts[i]`
is copied from `out/<job_id>/<name>` to `MediaDir` as **`<job_id>-<name>`** (flat, bare
filenames — `GET /fleet/media/{name}` rejects any separator, same as every other published
render). The **primary** artifact (`artifacts[0]`) missing is an error; any other
(**optional**, e.g. `qa-report.json`) artifact missing is silently omitted from the result,
never an error.

The JSON result only ever **names** `artifacts[0]` (`final_path`) and `artifacts[1]`
(`qa_report_path`) — the binding is by **index**, not by "whichever artifacts happened to be
present": if `artifacts[1]` is missing but a `artifacts[2]` exists, `qa_report_path` stays
absent (never silently reports `artifacts[2]`'s path instead). A 3rd-or-later configured
artifact is still copied to `MediaDir` under its own `<job_id>-<name>` name when present, it is
simply never referenced in the result JSON — a consumer that configures more than two
artifacts must know the extra names out of band (e.g. a fixed convention agreed with the
pipeline's own docs).

A child CLI failure (non-zero exit) is surfaced as a **plain failure**, not a defer — a fleet
job has no interactive caller to hand work back to. When the CLI's stderr carries the CMP CLI
contract's `SCENE-SWAP-FAIL stage=<...>: <message>` line, that line becomes the job's `error`
verbatim; otherwise the generic exec error (including a timeout-kill) is used.

### GPU concurrency caveats (read before dispatching two pipeline jobs at once)

- **No machine-wide GPU lease is taken in Go for a pipeline job** — only this process's
  single in-process media slot. The externally-provided CLI's own nested per-stage calls
  acquire the machine-wide lease themselves, exactly like a manual run of the same pipeline
  today; taking the machine lease again here would self-deadlock against that nested
  acquisition. Practical effect: **a second concurrent pipeline-job dispatch to this node
  waits behind the first for up to `gpu_wait_ms`, then fails as `gpu_busy`** — same shape as
  every other GPU route's busy-card behavior, just arbitrated one level higher (in-process
  only, not across processes).
- **A node restart loses an in-flight pipeline job** exactly like every other fleet task
  (see Known limits below): survivors are marked terminal `error:"interrupted"` on drain: an
  in-flight run may leave partial output under its `pipeline-jobs/<job_id>/out/` — the
  dispatcher's failure handling (re-dispatch elsewhere) is the recovery path, not a node-side
  resume. A **graceful** stop (SIGINT, drained) removes nothing extra — the job's own
  directory is cleaned up by its normal cleanup path once the drain marks it terminal. An
  **ungraceful** stop (crash, `kill -9`, power loss) leaves the directory behind with no
  in-memory record of it at all; since `job_spec.id` collisions are guarded by an exclusive
  directory create (see above), that orphaned directory would otherwise refuse EVERY future
  dispatch reusing the same `job_spec.id`, forever. `fleet-serve` therefore sweeps
  `<base_dir>/pipeline-jobs/` once at startup, **before** it starts listening. Every
  directory it materialized itself (`agent-<n>`, `accel-<n>`, a pipeline job's id) is
  orphaned by definition at that instant (this process has not accepted a single dispatch
  yet) and is removed. The root has a second kind of writer: a delegator process on the same
  box (the MCP server, the `delegate` and `research` commands) keeps the context of its own
  in-process local runs in `agent-local-*` directories there, each with an `.owner` file
  holding the process id (beside `context/`, never inside it), and that process outlives a
  `fleet-serve` restart. The sweep keeps a marked directory while its owner process is alive
  and the directory is under 24 hours old (the bound on a recycled process id), keeps an
  unmarked `agent-local-*` directory (written by a delegator older than the marker) until it
  is 24 hours old, and removes the rest. It logs how many it removed and how many it kept,
  plus one line for each directory it removes for its age alone while an owner may still be
  running (so a run that outlived the 24 hours does not look like a crash's leftover). A
  directory it cannot inspect is kept and reported as a warning, never counted as a run in
  flight. The owner's process id means something only in the process-id space that wrote it,
  so keep one `<base_dir>` per machine: a base directory shared between a Windows host and
  a WSL distribution, a container and its host, or two machines would read a live owner as
  exited and the sweep would remove the directory of a run still going (the machine-wide GPU
  lease and its activity registry make the same presumption of their pid-keyed records).

## The agent task (`task_type: "agent"`)

An **agent** job carries a self-contained delegation contract; the node runs it with its own
local read-only agent loop (planner = this box's `agent_model`, falling back to the workhorse;
no write/run/fetch/github tools, no delegate tool) over the contract's inlined context docs,
then returns a versioned result. Full behavior:
[systems/fleet-node.md](systems/fleet-node.md#the-agent-task-task_type-agent); the decisions:
[ADR 0023](architecture/decisions/0023-agent-lane-tailnet-auth-and-locality.md). Enable recipe
for both roles: [OPERATOR-GUIDE.md](OPERATOR-GUIDE.md#delegate-subtasks-across-fleet-nodes-agent_delegate--delegate).

### Dispatch payload — the contract

The envelope is the normal `{"job_id", "task_type": "agent", "payload": {…}}`; the payload is
one contract. Unknown payload fields are **ignored** (staggered node deploys must not flag-day);
`schema_version` skew and the size caps refuse loudly at ack (400).

| Field | Type | Meaning |
|---|---|---|
| `schema_version` | int | must be `1`; anything else is a 400 at ack |
| `goal` | string | **required** — the self-contained task; the sub-agent sees only this + `context` |
| `context` | `[{name, text}]` | inline docs, ≤ 16, ≤ 256 KiB total; each becomes a file the sub-agent can read, so `name` must be a flat filename: no `/` `\` `:` NUL, not `.`/`..`, **not a reserved Windows device name** (`CON`/`PRN`/`AUX`/`NUL`/`COM1-9`/`LPT1-9`, any case, with or without extensions — a write to one succeeds and reads back EMPTY), and no trailing space or dot (Windows strips them). Duplicates are rejected on a normalized key (trailing space/dot trimmed, case-folded), so `notes.md` / `notes.md ` / `Notes.MD` cannot shadow each other |
| `output_schema` | object | JSON Schema with a `properties` map of string / number / integer / boolean / `enum` fields, lists of strings, and objects and lists of objects nested to any depth. A list of numbers, booleans or enum values is written by the grammar as a list of quoted strings and converted back to the schema's type; a type list, `anyOf`, `oneOf` or `$ref` reads as a string; every property the schema lists is required by the grammar, at every depth. **Required over the wire** — an agent dispatch without one is refused at ack, because the delegator would have no mechanical check before merging. A LOCAL placement (`route: local`, or `auto` with an idle GPU) may omit it: the run then returns its `output` with `structured` empty, and acceptance's text verbs carry the verification |
| `acceptance` | `[string]` | delegator-evaluated checks: `contains:<s>` \| `not_contains:<s>` \| `regex:<re>` \| `min_items:<field>:<n>` \| `nonempty:<field>` \| `nonempty:<f1>\|<f2>` (at least one named field has a value; an empty alternative or a repeated name is malformed); malformed or unfalsifiable checks are a 400 |
| `profile` | string | agent task profile; default = the executing node's configured `agent_profile`, else `general` (small tiers seed `research` in `config_seed`); unknown names defer naming the valid set |
| `max_steps` | int | default 12, clamped to 12 |
| `timeout_sec` | int | default 300, clamped to 900; the wall the run is expected to fit in, not a deadline since 0.131.0 (ADR 0055: a run ends on a stall or the liveness ceiling) |
| `depth` | int | advisory — the node executes anything off the wire at `max(1, depth)`, so a wire "origin" claim is never trusted |

### Job result — what `data` holds on `done`

| Field | Type | Meaning |
|---|---|---|
| `schema_version` | int | `1` |
| `node_id` | string | this node, by the id `/fleet/health` publishes: `--node-id`, else `fleet_node_id`, else the OS hostname (0.185.0; a node on an older build stamped its hostname here whenever it was launched with `--node-id`, and the delegator then publishes that id as `results[].node_reported`) |
| `seat` | string | the planner model that ran |
| `output` | string | final assistant text (kept even when the structured re-pack failed) |
| `structured` | object | present iff `output_schema` given AND the post-loop grammar re-pack validated (one retry). With no `output_schema` the re-pack is **skipped entirely** — not attempted and failed — so a schemaless local run is a plain success with `structured` absent |
| `steps` / `stop_reason` | int / string | loop telemetry |
| `deferred` | bool | the node ran and could not complete the contract — **still a `done` job**, never `error` (`error` = internal wiring bug only) |
| `reason` | string | the defer shape: seat unserved on the roster, `wall timeout after <N>s`, `step budget exhausted (…)`, `output failed schema: …`, `structured re-pack unreachable: …`, build/loop/profile errors |
| `defer_class` | string | `abstention` \| `budget` \| `infrastructure` \| `config` \| `contract` — the machine-branchable WHY (table below). Absent from a pre-0.65 node: treat empty as *unknown*, never as abstention |
| `wall_ms` / `tokens_out` | int | node wall clock / re-pack completion tokens |

### Defer classes — which defers mean "a human must act"

| Class | Meaning | Shapes |
|---|---|---|
| `abstention` | the stack is healthy; the model could not produce a usable answer | `output failed schema: …` — validation failure, a **4xx** from the seat (it refused *this* request), or a 200 with zero choices |
| `budget` | a ceiling stopped it; a bigger budget might succeed | `wall timeout after <N>s`, `step budget exhausted (…)`, `canceled during the structured re-pack …`, delegator-side `poll deadline …` on a node that reported OWNING the job |
| `infrastructure` | something is broken — the contracted output never arrived, and retrying the same contract cannot help | `building agent: …`, `agent loop: …`, `structured re-pack unreachable: …` (dial failure or **5xx**, sticky across the retry), delegator-side `poll deadline …` whose last poll answer was unusable, and "all remotes failed the health probe". **`output` may be POPULATED on this class**: `structured re-pack unreachable` means the agent loop FINISHED and only the re-pack seat was out of reach, so the loop's prose rides along — the CALLER still receives the loop's answer in `output` — with `structured` absent (delegator-side acceptance never reads it: acceptance runs only when `deferred` is false). It still counts into `summary.lost_to_stack` — the checked deliverable the `output_schema` asked for is what did not arrive |
| `config` | this node's configuration can never run this | no seat resolvable, seat not in the served roster, unknown profile, and the delegator's "no remote passed the capability gate" / "no remotes configured" |
| `capacity` (composite, ADR 0052) | a LAYER's guard refused this contract right now — `placed.guard` names which (`display_floor`, `host_ram`, `presence`) and `placed.reason` carries the reading. Not a failure and not a retry-elsewhere: the card is the operator's, and the answer changes when the machine's state does |
| `contract` | the CALLER'S contract cannot be placed anywhere, however healthy the fleet | no `output_schema` for a remote placement, a contract past the origin hop (`depth != 0`), a token estimate too big for every advertised ceiling — and only when **every** agent lane advertised one, since an absent `agent_ctx_tokens` means the ceiling is unknown, not small |

The delegator counts `infrastructure` + `config` defers into `summary.infrastructure` — plus a
local placement taken while every configured remote failed its health probe — and
`local-offload delegate` **exits non-zero** when that count is non-zero: a broken node must not
read as a successful run just because its defers were polite. `contract` is deliberately
excluded: nobody has to touch a box to fix it.

The subset of those defers that LOST a subtask — it **delivered no usable result: the contracted
output never arrived** (a broken stack, not the model abstaining) — is published separately as
`summary.lost_to_stack`, omitted when zero. That is the count the MCP tool sets `isError` on,
alongside `summary.failed`, **when nothing succeeded** (C-75, ADR 0065): a call that delivered at
least one usable result is a successful call whose body says what was lost, so the MCP client does
not cut the digests out of an error-flagged body. A fleet-down run that still delivered every
subtask stays a quiet success there, while the CLI's exit code still reports it. Read it as "the contracted output was
lost", not as "the result is blank": the `structured re-pack unreachable` shape counts here with
`output` **populated** and `structured` absent, because a contract carrying an `output_schema`
asked for a mechanically checked deliverable and unchecked prose is not one.

A poll `404` is a **denial**, not an answer: it says the node never held the job, so it can never
earn the "node accepted the job" defer — after the bounded re-dispatches it is a
`summary.failed` failure.

No transcript field exists — remote reasoning never crosses the wire.

### Auth: every lane needs the fleet bearer

The rule is one rule for every route that does work or returns a result. Three cells, by whether the node has a
`fleet_auth_token` and where it listens:

| token | listener | every dispatch, poll, withdraw and artifact read |
|---|---|---|
| set | any | `Authorization: Bearer <token>` (constant-time compared) is required: `401` with the standard error envelope (`"error": "unauthorized"`) without it |
| empty | loopback | open: the same trust boundary as the local MCP surface |
| empty | anything else | refused `403 fleet lanes require fleet_auth_token on a non-loopback listener`, and `fleet-serve` **will not start** in this state (it names `fleet_auth_token`, the listen address and the config file it read) |

- **Every task type is gated**, not the agent lane alone: `agent`, `vision`, `text`, `stt`, `stt-upload`, `compose-project`,
  `media-job`, the PAIR relay and every media task (`image-gen`, `video-gen`, `animate`, `audio-gen`, `run-graph`, `accel`,
  `compose-video`), and any task type a later release adds (`tokenGated` returns true for all of them). Before this rule
  a media task was admitted with no check at all, so any peer that could reach a listener beyond loopback could dispatch a
  `run-graph` whose manifest names node packs for the node to clone and install.
- **The check runs first.** On `/fleet/dispatch`, `/fleet/vision` and `/fleet/text` it precedes the body read (nothing of
  the body is buffered, decoded or type-checked for a caller without the credential), and on every lane it precedes the
  job_id validation, the known-job lookup and the task validation. A caller without the credential is answered `401` (or
  `403` on a node with no token beyond loopback) and nothing else: a malformed body, a wrong content type and an
  oversized body are all `401`, not `400`, it learns no job state, and a task type the node does not serve is
  refused for the credential before it is refused for being unserved. The `400` answers (and the 1 MiB, vision and text
  body caps) are what a bearer holder gets. The stt upload, chat, kvslot, PAIR relay, compose-project and media-job doors
  checked the credential before their body read already, and so do the queue routes; a test sends a 20 MiB body without
  the bearer to every one of these doors and counts the bytes the node reads (none).
- **Two answers can come before the `401`.** A door that is closed on this node (`compose-project` and `media-job`, which
  need `fleet_compose_projects` or `fleet_media_inputs`, a token and a bound lane) answers `403` "door closed" before it
  looks at the bearer, whatever the caller holds. That order is deliberate: `/fleet/health` already tells anyone whether
  each of them is open (the task is in `supported_task_types` exactly when the door is admitted), so the `403` reveals
  nothing a caller cannot read from health, and a closed door has no body to protect. Second, the ingress guard wraps
  every route: with `fleet_ingress_guard: enforce`, a `POST` whose content type is not `application/json` is answered `415`,
  a request whose `Host` is not an allowed name `421` and one carrying an `Origin` header `403`, all before the route runs
  and so before the `401`; a caller of an enforcing node can tell a wrong content type from a missing bearer. In the default
  `warn` mode the guard only logs what it would refuse, and the `401` comes first.
- **Every refusal of a credential is logged, at every door that checks one.** The dispatch, vision, text, stt upload,
  PAIR relay, chat, kvslot, compose-project and media-job doors, the poll and withdraw of a job, the artifact read and the
  five queue routes each write one line per refused request (subject to the throttle below) and keep the status and body
  they always answered with. The line carries the caller's address, the door (the task type for a lane; `dispatch` for
  `/fleet/dispatch`, whose task type is in a body the node has not read; `poll`, `withdraw`, `chat`, `kvslot` and
  `fleet-queue` for the others) and which of "no bearer", "wrong bearer" or "no token configured" it was; never the
  credential presented, the configured one or the payload; a test compares each whole line against that shape
  (`fleetnode: refused a "<door>" request from "<caller host>": <reason>`) on a request whose headers and path are the
  caller's own text. A door that is closed (compose-project and media-job on a node that has not opted in) answers `403`
  whatever the caller holds; that is not a credential refusal and it writes no line.
  `/fleet/health` and the jobs feed are open on purpose, so a request with no bearer there is not a refusal and writes
  nothing. One that presents a bearer which is not the node's writes the same line (door `health` or `jobs-feed`, reason
  "wrong bearer"), under the same throttle, and is still answered the reduced view.
  What throttles the journal is never a field the caller chooses (the door and the task type are the caller's own text, and so
  is every header; a test varies each of them from one address): one
  line per caller address and reason every ten seconds, shared by all the doors, at most twenty lines a minute across
  callers, and the first refusal after that minute writes one line counting the rest.
- **Jobs and artifacts follow the dispatch.** A poll or withdraw of any job answers `401` without the bearer before any of the
  job's state crosses the wire, a re-dispatch of a known job id gets the same verdict, `GET /fleet/media/{name}` needs the
  bearer for every name (the outputs of a plain dispatch included, and a name the node would refuse as not a bare name;
  the check precedes the name check and the file lookup, so the answer never says whether a name exists), and the jobs feed (`GET /fleet/jobs`) omits every row's `error` text without it.
- **Health stays open**, so a roster read works before it holds a credential and a cascade lane can tell whether a base is a
  fleet node. Without the bearer, on a node that has a token, it still carries the node's identity, versions, capacity, counts,
  lanes and model names, and withholds the lease's free-text `reason`, the store steward's status (its `root` is a path on the
  node's disk) and the weight-file rows (`image_recipes`, `media_recipes`). A holder of the bearer, and any caller of a loopback
  node with no token, reads the whole answer.
- **A run-graph manifest names only pinned node packs.** The bearer lets a caller run renders, not code. A manifest's
  `node_packs` makes the node `git clone` each repository, check out each commit and pip-install the pack's requirements, so
  a pack is taken only when its `repo` AND `commit` are a row of `render/node-packs.json` (exact strings, a full 40-digit
  commit, the row's directory name or the one the repository gives it). Anything else is refused `400`, naming the pack,
  before a job, a temp file or a lease exists (`buildRunGraph`), and again by the runner's satisfier before its first git
  command. The table holds the packs the shipped graph builders need: VideoHelperSuite, ComfyUI-MultiGPU at its documented
  archival commit (upstream and mirror) and ComfyUI-GGUF; adding a pack is a reviewed change to that file and to its copy in
  `internal/mediacap/nodepacks.go`, which a test pins.
- **A run-graph manifest's models land under `models/` and come from a listed host.** A manifest's `models[]` makes the node
  download each `source_url` to `path` inside the ComfyUI tree before ComfyUI starts, so a path that leaves `models/<class>/`
  (a `custom_nodes/` drop, a `..` climb, an absolute path, a drive, a stream name) would place code where ComfyUI loads it. An
  entry is taken only when its path (backslashes read as separators) is `models/<class>/<file>`, no segment empty, `.` or `..`,
  none beginning with a space or ending with a space or a dot, and none holding a control character or one of `:*?"<>|`; and its
  `source_url`, when it has one, is `https://<host>/...` on a plain DNS name that is, or is a subdomain of, a `model_hosts`
  entry of `render/node-packs.json` (no user name, port or IP literal). The node refuses at the door with `400` naming the
  entry (`mediacap.CheckManifestModels`, called by `buildRunGraph`), and the runner refuses again before it looks for, hashes or
  fetches any file (typed defer `MODEL_NOT_ALLOWED`; every entry is read before the first is provisioned). One table of paths and
  sources (`render/testdata/manifest-model-cases.json`) drives both tests. A `sha256` is not required: the manifest carries its
  own hash, so one the caller supplies authenticates nothing. What remains: a holder can still put a file of any type under
  `models/<class>/` (a weight file in a pickle-based format is code when ComfyUI loads it), and the node follows redirects from
  a listed host. A `models[]` entry with no `path` is refused at the door too (the runner's parser refuses it as well).
- **Choose a long random token.** The token is the one secret behind every lane (a render, a project render, a coding-agent
  run), and a node does not slow down repeated wrong bearers, so its strength is what protects it. Generate it with
  `openssl rand -hex 32` (64 hex characters) and use the output as the value of `fleet_auth_token`, the same on the node and
  every delegator. The installer and the config check accept any non-empty value, so a short or guessable one is not caught.
- **Rollout order: delegators first, then nodes.** Every fleet client in this repo sends the bearer whenever
  `fleet_auth_token` is set (a per-package test and a source test pin it). What a node that gains the rule refuses, and so
  what must be in place first:
  - **A delegator needs the same `fleet_auth_token`**, a media-only box included. Media, stt and compose-video calls were
    accepted with no token before; with none (or a wrong one) every dispatch, poll and artifact fetch is now `401`. The
    media client classifies a `401` or `403` as a `config` defer, so the caller sees config defers, and the node's
    `/fleet/health` reaches that delegator with the lease reason, the store and the weight-file rows withheld, so its node
    view degrades without an error.
  - **A delegator must be new enough to send it.** The first release whose client sends the bearer on each lane is:
    withdraw 0.146.0, text 0.154.0, compose 0.163.0, stt 0.165.0, the roster probe's token check 0.167.0 and media 0.173.0.
    A delegator older than that has no client that sends it for the lane.
  - **Software outside this repo that sends no bearer stops working on every lane**, dispatch, poll and artifact fetch alike
    (the only external stt dispatcher known sends none, see the stt-upload section).

  The delegator change is deployed fleet-wide first; before the node change is deployed to a node,
  `fleet_auth_token` must be present in the config its `fleet-serve` reads (the same value on every delegator and every node).
  A node that listens beyond loopback with no token will not start after the upgrade, and its refusal names the key and the
  config file it read. A node installed by `setup/install.sh` takes the key with `--token-file PATH` (stored in the node's
  config, readable by its owner only and handed to the `--user` account with `sudo chown` when that is not the installing user,
  never printed) and the installer refuses to register the unit without one; on Windows put the key in the config the
  scheduled task's `fleet-serve` reads before registering the task.

### Health advertisement (only when the lane is admissible)

```json
"agent_enabled": true, "agent_seat": "offload-e4b",
"agent_ctx_tokens": 16384, "agent_seat_resident": true,
"seat_budget": {"step_tokens": 1024, "final_tokens": 4096, "thinking": "auto"},
"seat_rate": {"tok_s": 24.6, "cold_load_sec": 34.3, "samples": 2, "min_turn_sec": 201,
              "prefill_tok_s": 412.0, "prefill_slow_tok_s": 188.5}
```

A COMPOSITE node (ADR 0052) publishes two more, under the same lane predicate:

```json
"tiers": ["blackwell-16", "blackwell-2x16", "blackwell-3x16"],
"layers": [{"name": "pair", "tier": "blackwell-2x16", "devices": ["0,2"],
  "seats": [{"role": "agent", "model": "agent-pool", "device": "0,2", "ctx_tokens": 163840,
             "max_inflight": 32, "served": true}],
  "admissible": true, "reason": "no guards"}]
```

`tiers` is every hardware tier this node is a COMPLETE instance of. `layers` is one row per
device layer: the declared spec, each seat's `served` flag (the ROSTER fact — a cached health
read knows which seats it can serve, not which are loaded, so no `loaded`/`inflight` is
claimed), and the node's OWN `admissible`/`reason` verdict. The verdict travels because the
display-card guards can only be read where the card is; the delegator runs the same placement
table over these rows and the node re-checks at admission. Both keys are absent on a plain
node and cost no extra probe — they are built from the roster the residency refresh already
fetched and the VRAM snapshot the sampler already holds.

All four fields are published under the SAME predicate that admits a dispatch
(`fleetnode.AgentLaneAdmissible`: `fleet_agent_enabled` + a resolvable seat + a loopback
listener or a token, judged on the RESOLVED listener) — a delegator reads only these fields, so
advertising a lane dispatch would 403 is a mis-route rather than a near miss.

`agent_ctx_tokens` comes from config (`0` = omitted = the delegator never places here).
`seat_budget` (0.117.2) is the completion budget THIS node's agent loop runs a contract at — config
`agent_max_tokens` per tool step, the final answer at 4× (cap 8,192), config `agent_thinking` or `auto`: a
delegator's config does not travel with the contract, so a caller that needs matched budgets across seats (the
standard quality instrument, register H-04) reads it here. `seat_rate` (0.117.2) is the seat's remembered decode
rate and cold load from this node's seat-rates store (`seat-rates.json` under the state root, written by every
agent run since 0.115.21) with `min_turn_sec` = cold load + one final turn at this node's final budget; absent until
the seat has a sample; cached for 30 s. The delegator sizes a RETRY that lands here by it (register D-46).
`prefill_tok_s` and `prefill_slow_tok_s` (register F10) are the seat's prefill rate — the average over runs, and the
slowest of its last five solo samples, which is the rate the prefill stall allowance is sized from (a heat-soaked seat
is judged at the speed it runs now, not the average of the cool ones). Both are omitted until a solo prefill has been
measured, never published as 0, and only a run known to have had the seat to itself moves them (ADR 0066).
`agent_seat_resident` is a cached, alias-aware roster probe refreshed in the background at most
once per 30 s — **fail-closed**: `false` until the first probe lands, and `false` again on any
probe failure (a stale "resident" while llama-swap is down would route work at a node that
cannot run it; `false` only costs a conservative local placement).

### The write door (`agent_allow_write`, default OFF)

A delegation contract may carry `write_root` — a directory, RELATIVE to the run's read root, that the
seat may create and change files under. A node opens that door only when its config says
`"agent_allow_write": true`. Open it, restart the node, then prove it from a delegator with
`scripts/write-door-gate.ps1 -Remote http://<node>:18811` (four staged legs, each applied to a fresh copy
and tested — see `contracts/README.md`). **The default is false, and a node that has not opted in refuses a write
contract at ACK** (an HTTP 400, which makes the delegator RE-PLACE the contract on a node that has
opted in, exactly like any other dispatch refusal). The in-process local path has no ack hop to refuse
at and defers instead, with `defer_class: "write"`.

What opting in grants, and nothing else:

- `write_file` and `edit_file`, confined by `os.Root` to `write_root` inside the job's own throwaway
  copy of the contract's inline context docs. Create AND overwrite — a seat that cannot change an
  existing file cannot do an implementation leg — but the tree it overwrites is the node's copy, which
  is deleted with the job.
- **No `delete_file`, no `run_shell`, no `run`, no `web_fetch`, no github.** A write contract that names
  no profile runs under `edit`, which advertises exactly the six tools the door grants.
- Caps: 8 files, 64 KiB written, 192 KiB of rendered diff. The first two are enforced at the tool (the
  model gets a "NOT performed (write budget)" it can correct); all three are re-counted on the finished
  write set. **A breach publishes no diff at all** — a truncated patch applies as silent damage — and
  defers `write`.

What comes back is `diff` (a unified patch, `a/`+`b/` prefixed so `git apply -p1` takes it),
`diff_files` (the touched paths) and `write_note`. **The harness applies none of it.** Reviewing and
applying the change is the caller's job, and that is the whole reason a small seat can be given this
work at all. A seat that described the change instead of making it reports `write_note: "... the seat
wrote nothing"` rather than an empty-but-green result.

The node keeps no audit file for the door: the job directory (contract docs, write root and all) is
removed when the run ends, so the diff on the wire is the record — which is the point, since it goes to
a human.

### Re-placement on refusal

**A node saying no is no longer the end of the subtask** (0.101.0). Before it, `internal/delegate`
surfaced any non-`202` as an error and stopped: one `503 queue full` from the first-choice node meant
that subtask's work was simply not done, on a healthy fleet, with idle siblings one hop away.

**What counts as a refusal.** Anything that is not the one `202` ack, plus a node the delegator could
not reach at all. Re-placement is decided from the STATUS the node sent, never from the text of its
message — a node's prose is not a protocol.

| Answer | Re-placed? | Why |
|---|---|---|
| dial refused / dropped / request deadline (status `0`) | Yes | a statement about one address, not about the contract |
| `404` | Yes | nothing at this address serves `/fleet/dispatch` |
| `408`, `429` | Yes | about that node's timing right now |
| `409` | Yes | *"job previously failed on **this node**"* — no other node holds that record |
| `500`, `502`, `503`, `504`, any 5xx | Yes | it, or something in front of it, is failing: queue full, draining, stale VRAM snapshot |
| `400`, `401`, `403`, `405`, `413`, `415`, `422`, any other 4xx | **No** | the node is rejecting *this request*. The delegator hands the next node byte-identical bytes and the same bearer token, so it returns the same answer; re-placing collects it N times and spends the contract's budget doing it |

The `401`/`403` line has a stated bound: this delegator sends ONE `fleet_auth_token` to the whole
fleet (that is the documented posture), so a rejected credential is an operator fix, not a routing
one. A fleet running mismatched tokens gets no re-placement out of an auth refusal, deliberately.

**Where it goes.** Another eligible remote first — chosen by the same capability gate and the same
capacity-aware ranking as the original placement, excluding every node already tried — and then the
local seat. A remote PIN never falls back to local: an explicit remote route (`route=remote` with a `pin_reason`, or through a
caller with no reason channel) is not silently overridden, exactly as with "no eligible remote". A reasonless `route=remote` through
`agent_delegate`, `offload_research` or their CLI verbs is a hint (ADR 0078), and does fall back to the idle local seat, saying so in
the result's placement.

**The bound, per SUBTASK.** The bound and the exclusion set live in ONE per-subtask ledger that every
placement consults — the first attempt's re-placement loop and the verification retry's both — so the
retry draws from what the first attempt left rather than starting a fresh copy. The ceiling is **five**
placements, and the arithmetic is worth spelling out:

| | |
|---|---|
| 1 | the first placement |
| 2 | at most `maxRemoteReplacements` re-placements onto further remotes |
| 1 | at most one fall-back to the local seat |
| 1 | at most one verification-retry placement — a **separate** mechanism, bounded at exactly one, deliberately not folded into the number above |

**Every one of those five targets a dial base the subtask has not used before.** Local is not charged
against the numeric bound — it is the one seat always able to take the contract, so a wide roster must
not spend the bound before reaching it — but it is in the exclusion set, which is what holds it to one
use per subtask (and therefore stops a "retry" from landing on the seat the first attempt already
used). Rationale for bounding at all: each refused placement costs up to `dispatchAttempts` × 30 s of
dial time before a transport verdict, so walking a roster unbounded turns one saturated fleet into
minutes of wall clock spent collecting refusals; and a refusal that survives three DISTINCT nodes is a
fleet-wide condition, which a fourth dial does not fix.

**Deadline.** Re-placement lives INSIDE the subtask's `timeout_sec`, the same discipline the
verification retry follows: every placement is handed what is LEFT of the budget (elapsed rounded UP)
and below a 10 s floor there is no placement left to make. The remaining budget is **re-measured
immediately before dispatch**, because choosing a node blocks on a sequential fleet probe and the
number taken before that probe can be badly stale by the time it would be written into a contract;
the probe itself runs under a context carrying whatever the contract has left, so a roster of
blackholing nodes cannot spend a budget the subtask no longer owns.

> **`timeout_sec` is the EXECUTION budget, not an end-to-end wall ceiling.** A re-placed subtask can
> never be given more *execution* time than its contract granted — that part is exact. But two legs
> are bounded without being charged to it, because neither can be known before it is paid: placement
> overhead (the fleet probe, plus up to `dispatchAttempts` × `dispatchRequestTimeout` of dial time
> before a transport verdict) and 0.100.0's queued-time credit, which extends the poll wall past
> `timeout_sec` by design and is bounded by the queue budget derived from the node's ETA (`queueBudgetFor`,
> ADR 0063). So the wall a subtask can consume is
> `timeout_sec` + `pollGrace` + queued credit + placement overhead — all bounded, and the
> re-placement loop converges because every blocking leg is charged to the next measurement.

**Dispatch time only.** A refusal is a NON-ack: the node never took ownership, so no seat can be
running the contract and re-placing it cannot produce two concurrent runs. Everything after a `202` —
a poll `404`, a queue deadline, a poll deadline — leaves a job the node may still hold, and is never
moved. The one exception (ADR 0064): at the queue deadline the delegator asks the node to take the job
back, and only a node that CONFIRMS it withdrew a job that never started makes the result re-placeable
([Taking a job back](#taking-a-job-back-withdraw-and-the-poll-lease)). One honest residual: when BOTH dispatch attempts fail at transport level the first POST may
have landed and had its ack lost, so the abandoned node could still run the contract once. That costs
wasted compute on a node nobody is polling (the agent lane is read-only, so there are no effects to
duplicate) and the alternative is losing the work with certainty.

**A 503 returns at once (ADR 0063).** The delegator no longer sleeps the refusing node's `Retry-After` and retries it: the
subtask is re-placed on a node with room straight away, the fleet is read again first (a snapshot that predates the
refusal is never reused, even on `route=spread`), and the hint becomes a per-node cooldown that only the capacity
wait honours — crediting what it idles. The local seat's own capacity defer (`seat busy`, no step ran) is re-placed
the same way; anything a node reports after a `202` is not.

**When nobody takes it** the subtask is a **FAILURE**, with its own stable prefix:

```
placement refused: <n> node(s) refused this subtask and none of them ran it
(<node>: <what it said>; <node>: <what it said>); <why the delegator stopped asking>
```

Never a defer — no seat ever saw the contract, so there is no report to author on a node's behalf,
and the only class that would fit (`budget`) would teach every consumer that a seat needed more time.
The one exception is a chain the delegator closes after the whole-call deadline has passed: that call ran
out of time, and the subtask is published as the deadline's budget defer with this text quoted behind
the `call deadline reached` marker ([ADR 0065](architecture/decisions/0065-the-whole-call-has-a-deadline-below-the-clients-abort.md)).
The three sentences are deliberately distinct: `placement refused` = nobody took it; `queue deadline`
= one node accepted it and never started it; `poll deadline` = one node started it and never finished.

**What the response publishes.** `summary.replaced` / `summary.replacement_recovered` (subtasks
re-placed, and how many then reached a node that took the work), and per subtask `replacements` +
`replacement_note` naming every node that refused and what it said. All `omitempty`, so a run where
nothing was refused publishes byte-identically to before. Read `replaced: 6` on an otherwise green run
as "the fleet is shedding load" — that is the line that sends you to `fleet_max_queue_depth` before
the next run has nowhere left to go.

### Capacity-aware placement

`queue_depth` alone was never a placement signal: it is a count with no scale. A node at depth 1 of a
1-deep queue is a certain `503`; a node at depth 500 with no published ceiling is not. Since 0.101.0
the delegator ranks eligible remotes on three ordered keys:

1. **Not provably saturated** beats saturated — `max_queue_depth > 0 && queue_depth >= max_queue_depth`
   is precisely the condition that produces `503 queue full`.
2. **A provably free execution slot** beats one that is not provable — `queue_depth == 0`, or
   `jobs_running < max_concurrent_jobs` with `jobs_queued == 0`. The job starts *now* rather than
   waiting in `accepted`, which is the state the `queue deadline` failure reports.
3. **Lower `queue_depth`** — the original rule, original meaning, still deciding everything the two
   keys above do not. Ties keep roster order.

For BOTH limits, `0` means **unknown**, never "unlimited" and never "full": a node publishes `0` for
unlimited and a node too old to publish the field decodes to `0` as well, and the delegator cannot
tell those apart. An unknown node is neither credited nor blamed — which is why key 2 counts
`queue_depth == 0` as proof on its own, so a completely idle node that publishes no limits is not
demoted below a loaded node that does.

Saturation is a RANKING input, not a capability: `remoteEligible` is unchanged, and a saturated node
is still chosen when nothing better exists. The reason is that the **delegator's copy** of these
numbers is stale by construction — the snapshot ages between the health `GET` and the dispatch `POST`
(placement is not atomic with admission), and this run's own concurrent siblings (the call's width, `dealParallelism`)
eat the headroom it measured, since they probe within milliseconds of each other and can all read the
same free slot. Hard-excluding on a number that is stale by construction would strand a node that has
since drained, on evidence that was never current; demoting costs nothing when the reading was right
and forfeits nothing when it was wrong, and re-placement covers the case where it WAS right.

> The node itself does **not** cache these counters: `handleHealth` walks the job store live, in the
> same request, so `queue_depth` / `jobs_queued` / `jobs_running` are current as of the moment it
> answered. What *is* cached on the node side is the VRAM snapshot (max 30 s, `503` past that) and
> `agent_seat_resident`. An earlier draft of this section said otherwise; it was wrong about the node.

### Job ids and polling (what a delegator does)

Agent job ids are delegator-minted (`agd-` + 24 random hex chars). Dispatch doubt is retried
once under the **same id** — the store re-acks `202` idempotently, so a lost ack never buys a
second run. The delegator polls every 3 s; a poll `404` triggers a bounded re-dispatch of the
same id (max 2); past `timeout_sec` + 60 s grace it stops polling — the node may still finish
server-side, and the job id in the delegation log lets you reconcile by hand.

**Queued time does not consume the execution budget** (0.100.0). `timeout_sec` + grace is a
budget for *work*, and since nodes gained a real backlog a job can sit in `accepted` without any
work having started. The delegator credits that wait back: the deadline moves out by the time the
job provably spent queued, so a busy node costs a subtask latency, never its budget. Deadline
messages name the credit (`poll deadline after 5m0s (+42s credited back for time queued on the
node)`) and say nothing extra when there was none.

The wait is **bounded** — a job may wait for a slot at most as long as it was allowed to run, and
the wait it is given is derived from the node's own ETA: `clamp(1.5 x etaStart + 30 s, 60 s, that limit)`, where
`etaStart` is the wait the node publishes for a NEW job (`new_job_wait_sec`; an older node's `queue_wait_estimate_sec`
plus one slot, ADR 0073) or the arithmetic over its jobs and recent wall, read again when
the job is first seen queued; a node that publishes no ETA keeps **5 minutes** (ADR 0063). That bound is where the delegator
*asks* (ADR 0063 amendment 2026-10-10, register C16): it reads the node's health once more, and a job whose node has all its workers
running (a published `max_concurrent_jobs`, that many `jobs_running_capped`: the running jobs that count against it, which `jobs_running` does not say because it includes renders and transcriptions) while no other node, and not the local seat, has room for it
**stays in line** and is asked about again one step later (60 s, chosen), up to the limit above. A node with a free worker that still
has not started the job, a node whose health cannot be read, and a job with a better place on offer are withdrawn at the bound as
before, because withdrawing a queued job only to land it at the back of the same line throws away the wait it has already spent.
Hitting the bound while
the job is *still queued* is a **failure**,
not a defer, with its own stable prefix:

```
queue deadline after <d>: the node accepted the job but never started it — it waited in
the node's backlog and never reached running (<n> poll(s), every one `accepted`)
```

That is deliberately distinct from the `poll deadline` shapes below, which all concern a job that
was *running*. A defer is a report about the work, stamped with the node's id and seat; a job that
never started has no such report to make, and manufacturing one would be the delegator inventing a
sentence no seat earned.

What the deadline produces for a job that DID reach `running` depends on whether the node ever
**answered** about it:

- it answered (any `202`/`200` job state) → the subtask is **deferred**, reason prefixed
  `poll deadline after <d>: node accepted the job but did not reach a terminal state` (class
  `budget`, or `infrastructure` when the last answer was unusable — a 5xx or an unknown state,
  which is then named in the reason).
- it never answered (dial refused, connection dropped, unparseable body) → the subtask is a
  **failure**, `poll deadline after <d>: node never answered (last: …)`, counted in
  `summary.failed`. A node that said nothing never deferred, and reporting a defer stamped with
  its id and seat would be the delegator inventing an answer on its behalf.

### Taking a job back (withdraw and the poll lease)

Until ADR 0064 a delegator that gave up on an acked job left it on the node, where it started when a
slot freed and ran for nobody (measured at 43 % and 59 % of two nodes' agent runs on 2026-09-29), and
its finished wall then fed the node's own Retry-After. Two mechanisms end that.

**Withdraw.** `DELETE /fleet/jobs/{id}` (agent bearer, like the poll) takes back a job that is still
`accepted`. The node decides that under the same mutex its scheduler claims under, so a claim and a
withdraw cannot both succeed; a running or finished job is never touched (Option A, ADR 0028). A
withdrawn job is a terminal `error: "withdrawn: ..."` record, not a deleted one: a poll of the id
reaches a terminal state, a duplicate dispatch of it answers `409` and never runs, and the jobs feed
shows it for the terminal TTL. A repeat of the request answers `200` again, and so does a request for
a job the node already took back itself (reaped, or marked never-started at shutdown): nothing ran, which
is the fact the answer reports.

The delegator calls it once, best-effort (5 s, on a context the caller's cancel does not touch; once the
whole-call deadline of [ADR 0065](architecture/decisions/0065-the-whole-call-has-a-deadline-below-the-clients-abort.md)
has passed, no more than three quarters of its unwind allowance), where it gives a job up:

| Where | A confirmed withdrawal | `409 running` | Anything else (404, 405, 401, 5xx, dropped, timeout) |
|---|---|---|---|
| queue deadline | the result is re-placed on another node, the queued wait is credited back to the contract's budget, the intent closes `withdrawn`, and the failure text ends `; the job was withdrawn from the node, which will never run it` | the job left the backlog just before: keep polling it, do not ask again | today's `queue deadline` failure, not re-placed, the intent left open for recovery, and the failure text ends `; withdraw not confirmed: <why>` |
| caller cancel, or the whole-call deadline (ADR 0065) | the intent closes `withdrawn` | the intent stays open for recovery; the failure text `canceled: ...` ends `; withdraw not confirmed: <why>` | as the `409` cell |
| owned poll deadline | the intent closes `withdrawn`; the defer reason adds `; the job never started and was withdrawn from the node` | the intent stays open; the defer reason ends `; withdraw not confirmed: <why>` | as the `409` cell |
| unowned poll deadline (the node acked the job and no poll ever said it held it) | the intent closes `withdrawn`; the failure is unchanged | the intent stays open; the failure text ends `; withdraw not confirmed: <why>` | as the `409` cell |
| a poll answered `401` | not asked: the request would carry the token that was just refused | not asked | the intent stays open for recovery, so a process holding the right token can still collect the job |

A job the delegator last saw `running` is not asked at all: it has started, and the request could only
be refused. Nor is a job the queue deadline already asked about and the node answered `409`: a give-up
that comes before the next poll has answered (a cancel, the whole-call deadline, a poll deadline) reads
that `409` instead of asking again, and its row ends `; withdraw not confirmed: HTTP 409: the node said
the job had already started`, as it does when the give-up asks for itself. A node that predates the route
answers `405` (its job route is GET-only) or `404`, which is "no withdraw here": every give-up then
behaves exactly as it did.

`<why>` says what the node answered instead of taking the job back, so an old node and an upgraded one
that refused stop reading alike: `HTTP 405: the node has no withdraw route (an older node)`, `HTTP 404: the
node does not hold the job, or has no withdraw route`, `HTTP 401: the node refused this delegator's
fleet_auth_token`, `HTTP 409: the node said the job had already started`, `HTTP 5xx`, `HTTP 200, but the
answer did not say the job was taken back`, `no answer within 5s` or `no answer (<transport error>)`. It
is detail, not a class: the row's `reason_code` stays `queue_deadline` or `canceled`.

**A poll that reads the node's own record.** A delegator that was away for longer than the lease (a
suspended host, a long partition) comes back to `error: "reaped: ..."`, and one that polls a job
another caller withdrew reads `error: "withdrawn: ..."`, and one that polls a job the node marked
never-started when it shut down with it still queued reads `error: "not started: ..."`. All three say the
job never ran (the node's own Withdraw treats them as one fact), so the delegator
files the result as it files a confirmed withdrawal: a capacity refusal the existing re-placement offers
to another node, the queued wait credited back, the intent closed `never started: <what the node said>`,
and `reason_code` `queue_withdrawn` on the abandoned attempt's row. A job that ran and failed stays a
`remote job error`, is never re-placed, and closes its intent `terminal observed`.

**Poll lease.** A node also cleans up after a delegator that left without asking. A job a delegator
PUSHED (an agent dispatch) that sits `accepted` with nobody polling it for `fleet_poll_lease_sec` is
skipped by the scheduler's claim scan at once (so the reaper's tick is never a window in which a ghost
starts) and reaped by a ticker (terminal, `error: "reaped: ..."`). What counts as a poll: an
authorized `GET /fleet/jobs/{id}` (a long poll parked on it counts for the whole wait), or an authorized
duplicate dispatch of the id (the job's own bearer rule applies whatever `task_type` the request
declares, so a re-dispatch without the token answers `401` and counts for nothing). What does not: the unauthenticated `GET /fleet/jobs` feed the fleet overview reads,
and a poll that failed the bearer gate. Never reaped: a running job; a job the pull queue claimed (its
result travels by ack, nobody polls this node for it); a media or vision job (their pollers are other
clients on cadences this node does not control). The log says so in one line per reap pass, and one per
withdraw. The node also counts what it took back, since the process started: `jobs_withdrawn` and
`jobs_reaped` on `/fleet/health` (absent while zero), because a default-on, time-based reaper is the
thing whose false positives an operator has to be able to count. `fleet-serve` prints the lease in force
once at start-up (`poll lease 60 s (the default ...)`, `... was raised to it` for a value under 15 s, or
`poll lease OFF`).

A run that finishes more than a lease after its poller last looked is an abandoned run. Its wall (often
minutes of stall on a seat no caller was using) no longer feeds `recent_agent_wall_sec` or the
Retry-After built from it. A look after the job finished does not change that.

The delegator's own ledger records what it did: `reason_code` `queue_withdrawn` for a confirmed
queue-deadline withdrawal (or a job whose poll read the node's own `reaped`/`withdrawn` record) and
`queue_deadline` for one the node did not confirm, and the intent ledger's close note says `withdrawn`,
`terminal observed`, `never started` (the recovery pass, or a live poll, found the node had withdrawn or
reaped the job) or `recovered`; see [The delegation ledger row](#the-delegation-ledger-row). The fleet
overview does not list a `withdrawn` or `reaped` job as an operator error: it is the cleanup working.

Telemetry never fails the work, but it does not fail silently either. A write to the intent ledger that
fails (a full disk, a bad path) is logged once per ledger handle, naming the first event lost and what
that costs (a lost dispatch event means the job has no intent for recovery to find); a dispatch marker
that cannot be written is logged once per run; and a recovery pass that stops at its two-minute clock
logs how many open intents it did not examine.

### The delegation ledger row

Every `agent_delegate` row in the delegator's `ledger.jsonl` carries (ADR 0064):

| Column | Meaning |
|---|---|
| `door` | the surface that admitted the contract (the MCP tool or CLI verb); the engine's own name, `delegate`, when the caller stamped none, so a row with that door is an entry point that skipped the stamp (`fleet-smoke` stamps `cli:fleet-smoke`) |
| `job_id` / `fleet_job_id` | the delegator's id for the attempt; `fleet_job_id` is the id the fleet node knows it by (the same value, present only when the node acked the dispatch: a refused dispatch left no job on any node, and no marker row). The node's own `agent` row carries the same `fleet_job_id`, so the two ledgers join on one equality. |
| `reason_code` | a closed set saying why the job ended the way it did: `ok`, `failed_verification`, `queue_full`, `queue_deadline`, `queue_withdrawn`, `poll_deadline`, `canceled`, `node_unreachable`, `job_lost`, `dispatch_refused`, `remote_error`, `capacity_wait`, `node_busy`, `shed`, `no_eligible_node`, `seat_down`, `stall_admission` / `stall_cold_load` / `stall_prefill` / `stall_decode` / `stall_tool` / `stall_repack` / `stall_engine` / `stall_other`, the node's defer classes (`budget`, `abstention`, `contract`, `config`, `write`, `infrastructure`), `bad_input` (the call's own input cannot be processed as given: an audio file with no audio stream, a picture that does not load), `started`, `other`. Present on every `agent_delegate` row **and on every deferred row of any task** (F22: the code used to be settled for `agent_delegate` alone, so a deferred transcribe, media or review-lane row carried free text and, on the media doors, an `err_class`): a writer's own code from the set wins, otherwise `Record` classifies the row from its `err_class` (`ledger.DeferCode`: `gpu_busy`, `gpu_queued` and `compose_busy` are `node_busy`; `timeout`, `conn_refused`, `oom`, `disk_full` and the lease and lane classes are `infrastructure`; `not_configured` is `config`; the iGPU runners' typed classes are `config`, `contract` or `failed_verification`; `bad_input` is `bad_input`) and then from the stable prefix of its reason (`gpu busy:` and `seat busy:` are `node_busy`, `no ... configured` and `imagegen_families[` are `config`, `audio generation failed:` and `video generation failed:` are `infrastructure`, `audio convert:` is `bad_input` unless ffmpeg could not be run at all (`err_class` `ffmpeg_unavailable`, `config`) or the temp directory could not be made (`infrastructure`)), and `other` when neither names it: a growing share of `other` means the table needs a member. The agent door's deferred rows carry the node's defer class as their code, because their reasons are prose (`output failed schema`, `structured re-pack skipped`, `step budget exhausted`): `capacity` files as `node_busy` and every other class as the code of the same name. `TestEveryLiteralReasonAPipelineDoorDefersWithHasACode` walks the pipeline's writers, so a new reason sentence cannot arrive uncoded. A row of another task that did not defer carries none. `capacity_wait` is the delegator's own outcome (no node had room; nothing ran the job); `node_busy` is a node, or this box's own seat, answering a capacity defer after admission (a seat at its run cap, a card under a lease or fence, an engine held busy by other work). `seat_down` is the seat itself failing to serve the job (ADR 0066): unreachable, gone down under the run and not back inside the node's recovery wait (`seat down:`), or answering llama-swap's 5xx past the busy-seat wait (`seat not serving:`) — never its peers holding its slots (`seat contended:`, a 429, is `infrastructure`). |
| `reason` | the whole reason (up to 4096 bytes); rows written before ADR 0064 hold the first 120 bytes. Group by `reason_code`, or by its first 120 bytes. |
| `phase` | `"started"` on the dispatch marker row, absent on every finished row |
| `refusal` | `true` on the row of an attempt a node refused at dispatch for capacity (a 503 or a 429): a place in line, not the subtask's outcome, which is another row (the one that landed, or the closing row a lost subtask gets under a job id of its own). The ledger twin of the delegation log's `refusal` mark; absent on every other row and on every row written before the column existed. The failure register counts a marked row as a `refused_attempts`, not a failed job, and no job counter counts it (`ledger.JobRows`, the savings summary and its top defer reasons, the session split, the reports): it did no work |
| `run_class` | `test`, `bake`, `measurement` (or an operator label) when the run was not real work, absent on every production row (register F48 / job-error class C23): the process said so (`LOCAL_OFFLOAD_RUN_CLASS`, a `bake:` origin), or the call did (`pin_reason: measurement`, the `cli:fleet-smoke` and `cli:fleet-measure` doors, the `go-test` origin). The summaries and `loupe` leave these rows out and count them (`--include-test-traffic` counts them again); the inner `agent` row of a labelled job is left out with its parent. A node's own row for work it ran for a labelled asker carries the asker's class too (`X-Offload-Run-Class`, ADR 0087), when the node can authenticate the request and is a build that reads the header; an older node's row stays unlabelled. The same header pair carries the asker's session: the node's row names it as `origin_session` (`X-Offload-Origin`) |

A **started marker** is written when a node acks the job (or a local run begins), so a hang or a ghost
is visible while it happens. A refused dispatch leaves no marker. The marker is never a job (every
counter in this repository skips it: `ledger.JobRows`, the summary, the reports) and it carries no
tokens. A reader that counts rows must skip `phase: "started"`.

## The vision task (`POST /fleet/vision`)

Since 0.116.0 a node runs ONE single-image vision task — `vqa`, `ocr` or `assess_image` — on its
own vision seat for a caller whose card is busy (or who asked for a remote outright). The
delegator side is `internal/visionremote` (the `route` parameter on `offload_vqa` /
`offload_assess_image` / `offload_ocr` and `--route` on the CLI verbs); decisions in
[ADR 0040](architecture/decisions/0040-vision-work-travels-to-a-node-with-an-idle-card.md).

### Why its own route

`/fleet/dispatch` caps its body at 1 MiB — right for every media envelope and agent contract,
and a fifth of one image at the configured cap. `/fleet/vision` sizes its body from THIS node's
`vision_max_image_bytes` (× 4/3 for base64, plus 64 KiB of envelope slack — `VisionBodyCap`) and
then joins the **same** admission path as every other job (`Server.admit`): known-id
re-ack, drain, lease, band, queue cap, then `BuildRequest` and the job store (the bearer was checked before the body was
read; `admit` checks it again as a backstop). It stays
concurrency-CAPPED (it contends for the shared llama-swap endpoint, exactly like agent work).

### Payload

```json
{"job_id": "vision-…", "task": "assess_image",
 "image": "data:image/png;base64,…", "brief": "a beach"}
```

`task` ∈ `vqa` (requires `question`) · `ocr` · `assess_image` (optional `brief`). `image` MUST be a
data URI — the bytes travel with the job; a path on the caller's disk is refused `400`. An image
whose decoded size exceeds the node's cap is refused `400` at ack time naming
`vision_max_image_bytes`; the pipeline's loader re-checks the exact bytes. Unknown fields → `400`.
A node whose config sets `vision_tasks` (0.153.0) serves only the tasks it lists: any other `task` is
refused `400` at ack time with the set named — `vision: task "assess_image" is not served by this node's
vision seat (vision_tasks: vqa, ocr)` — and never reaches the pipeline. Empty or absent = all three.
Two tiers narrow it in their data: the RK3588 NPU seat (`vqa`, `ocr`: the runtime cannot take the grammar
`assess_image` sends) and the `ampere-8` Qwen3-VL-4B Q4 seat (`vqa`, `assess_image`: its tier page records the
omitted `ocr` alias as a quality statement and Q4 as the measured OCR-fidelity cliff, so a remote `ocr` goes to
another node or defers instead of being answered there; a local call is unaffected). The other Q4 vision seats
(`ampere-6`, `amd-gcn`, `amd-rdna3`, `amd-rdna3-dgpu`, `cpu`, `dual-gpu`) keep all three: their pages say nothing
about OCR quality, and silence is not a verdict.

### Result

The done job's `data` is the node's **full `core.Result`** — `ok`, `deferred`, `reason`,
`defer_class`, `result`, `meta` — so a remote judgment and a local one have one shape. A pipeline
defer (`gpu busy`, empty answer, unreadable image) is therefore a `done` job whose data says
`deferred: true`, the agent lane's convention, never an `error` job.

### Auth and advertisement

The lane rides the agent lane's bearer rule verbatim (`tokenGated`): with `fleet_auth_token` set,
dispatch AND job polls require the bearer (`401` otherwise); with no token, a non-loopback listener
refuses `403` and withholds the lane. `VisionLaneAdmissible` (a bound `vision_model` + safe
reachability) is the ONE predicate behind both the advertisement — `"vision"` in
`supported_task_types` and the additive `vision_model` health field — and the ack-time admission,
the `AgentLaneAdmissible` discipline. Vision jobs are token-gated on poll (`JobView.Gated`) but
are never listed as agent runs in `/fleet/jobs`.

A node that narrows the lane also publishes the additive, omitempty health field `vision_tasks` (the
config key of the same name), under the same lane-gated rule as `vision_model`; a node that serves all
three publishes nothing, byte-identical to a node from before 0.153.0. The key is written by the tier's
media seat (`mediaseat.Seat.Tasks`), never by `config_seed`: a seat whose runtime cannot do a task
declares the ones it can. The RK3588 NPU seat declares `vqa` and `ocr` because the RKLLM runtime cannot
constrain sampling and `assess_image` always sends a grammar
([ADR 0062](architecture/decisions/0062-rk3588-soc-tier-serves-from-the-npu-on-a-unified-memory-budget.md)).

### Placement (delegator side)

`route: local` (default) is byte-identical to before the route existed. `auto`: an idle local
card always runs the work; only while the machine-wide GPU lease is held (`delegate.LocalBusy`), or the vision seat the task would
run on is busy (`delegate.LocalSeatBusy`: a request in flight, a load or unload in progress, or a load that would unload a loaded
vLLM seat; ADR 0078 decision 9), is a node considered, ranked by `delegate.PlaceVision` — eligible = advertises the lane, serves THIS
task (`NodeView.ServesVisionTask`: no `vision_tasks` in health = all three, so an older node stays
eligible for every task; a published list serves only what it names) and its
card is not leased (`lease.class: text` or `lease.busy`), ordered by the agent lane's
`betterRemote` (lease rung: clean, then a long lease, then an overdue one → not saturated → no live driver-fault
repeat under every seat ([ADR 0096](architecture/decisions/0096-a-live-driver-fault-repeat-demotes-a-cards-placement.md)) → provably
free slot → a free card → queue depth → GPU utilization → roster order) — and with no eligible node the work still runs local. `remote`: force a node; none
eligible ⇒ `deferred: true, defer_class: capacity` (or `config` with no `delegate_remotes`),
never a local run. `meta.node` / `meta.placement` on the result say where it ran.

Every `delegate_remotes` entry is first judged by the tailnet shape check the agent lane applies
(`netguard.TailnetURL`), through `internal/rosterprobe`; an entry it refuses is named in the defer reason as
`not dialled, refused by the tailnet guard` and the others still serve (the text, stt-upload, compose, media and accelerator
lanes do the same, [ADR 0074](architecture/decisions/0074-every-fleet-client-admits-the-same-roster-under-a-configured-list-of-tailnet-zones.md)).

## The text task (`POST /fleet/text`)

Since 0.154.0 a node can run ONE `classify` or `extract` call on its OWN pipeline for a caller that asked for a remote
outright (or whose card is leased). The lane is **dark**: a node advertises it only when its tier's media seat declares
text tasks, and no shipped tier does yet. The runtime behind an RK3588 NPU seat cannot constrain decoding, so the node's
pipeline runs these two tasks on that seat from a prompt that states the JSON shape and accepts a reply only after strict
schema validation (config `unconstrained_seats`); summarize and triage are never admitted on the lane.
Decision and measurements: [ADR 0069](architecture/decisions/0069-an-unconstrained-seat-runs-classify-and-extract-from-the-prompt-and-the-text-lane-ships-dark.md).

```
POST /fleet/text   Content-Type: application/json   (the agent lane's bearer rule, see below)
{"job_id": "text-…", "task": "classify", "input": "…", "params": {"labels": ["billing", "support"]}}
{"job_id": "text-…", "task": "extract",  "input": "…", "params": {"schema": {"type": "object", "properties": {…}}}}
```

`params` carries only what the task takes (classify: `labels`, at least two; extract: `schema`); anything else, an empty
`input`, and unknown fields are `400`s naming the field. The body cap is dispatch's 1 MiB; this node's `max_input_chars`
trims the text before it reaches the model, as for a local call. The poll is `GET /fleet/jobs/{id}` and the job's `data`
is the node's full `core.Result`, defers included (a schema failure after the retry is a `done` job saying
`deferred: true` with the validator's reason).

The node's config key `text_tasks` (the text subset of the seat's `tasks`, canonical order `classify`, `extract`, written
by `mediaseat.Bindings`, never by `config_seed`) is the set it serves: a task outside it is refused `400` at ack time
(`text: task "extract" is not served by this node's text lane (text_tasks: classify)`), summarize and triage always
(`text: task "summarize" is never served on the text lane …`). `TextLaneAdmissible` (a non-empty `text_tasks` and the
agent lane's reachability rule) is the one predicate behind the advertisement — `"text"` in `supported_task_types` and the
additive, omitempty health field `text_tasks` — and the ack-time admission. Text jobs are token-gated on poll
(`JobView.Gated`), never listed as agent runs, and capped by `fleet_max_concurrent_jobs`: they contend for the node's
text endpoint.

The delegator side is `internal/textremote`, the `route` parameter on `offload_classify` / `offload_extract`:
`local` (default) is byte-identical to before the route existed; `auto` leaves the box only while the machine-wide GPU
lease is held and otherwise, or with no eligible node, stays local; `remote` forces a node and, with none eligible,
returns `deferred: true` with `defer_class: capacity` (or `config` with no `delegate_remotes`). Eligibility
(`delegate.PlaceText`) is stricter than vision's: the node's health must list `text` AND the task in `text_tasks`, so an
older node and a node whose tier declares nothing are never picked, and its card must not be leased. `meta.node` /
`meta.placement` on the result say where it ran. Budget 300 s, vision's. The delegator post-checks a node's OK result
(classify: a label in the request's set and a confidence in 0..1; extract: one object whose keys are all in the requested
schema) and turns a failure into a deferred result naming the node and the reason. A node declares a text task
meaningfully only when its cascade for that task (`model`, `triage_model`, per-task rungs) routes to the unconstrained
seat; measure through that same path.

## The stt upload door (`POST /fleet/stt`)

Since 0.165.0 a node transcribes audio its caller SENDS, so a box whose own whisper is held (a render owns its cards) can
have an idle node do it. The legacy `stt` task of `/fleet/dispatch` takes only a path on the node's own disk under a
1 MiB body cap and cannot be sent bytes. The delegator side is `internal/sttremote` (the `route` parameter on
`offload_transcribe`, `--route` on `local-offload transcribe`); decision and alternatives in
[ADR 0072](architecture/decisions/0072-a-fleet-node-transcribes-audio-its-caller-uploads-so-a-held-card-is-a-place-in-line.md).

```
POST /fleet/stt   Content-Type: application/json   (the vision lane's bearer rule, see below)
{"job_id": "stt-…", "audio_b64": "<base64 of the audio file>", "audio_ext": "ogg", "language": "es", "hq": false}
```

`audio_ext` is 1 to 8 lowercase letters or digits (it only names the node's temp file; ffmpeg sniffs the content),
`language` is `auto` or a language code, `hq` needs the node to have an `stt_model_hq` (else `400 … no stt_model_hq`).
Unknown fields, bad base64, an empty file and a file over the cap are `400`s that name the problem; **nothing here reads a
path on the caller's disk**. The body cap is `STTUploadBodyCap`: the node's `fleet_stt_upload_max_mb` (decoded MiB,
default 48, which is 64 MiB of base64 on the wire plus 64 KiB of slack) in base64. The bearer is checked **before the body
is read**, and a caller that passed it gets a 10-minute read and write window (the blanket 30 s would cut 64 MiB on an
ordinary link). The body is read before the admission gates (a known job id must still re-ack, so the id is read
first), so at most two uploads are in flight (`sttUploadInFlightMax`): a third waits for a slot up to 30 s, then gets a
re-placeable `503` with `Retry-After`. A request that **declares** a `Content-Length` over the body cap is refused `400 request
body too large` at once, right after the bearer check, before it takes or waits for a slot and with its body unread
(`Connection: close`). The audio is parsed in place and decoded once (no string or second buffer of the
base64), so the peak per upload is the body plus the decoded bytes. Everything after the body read is the shared `admit`
path.

The node writes the bytes to a private file (mode 0600) under `<media_dir>/.stt-upload/`, runs the pipeline's transcribe
over it with `Door = fleet`, the fleet job id and the asker (the `X-Offload-Asker` header) as the requester, and removes
the file when the job ends, on every refusal and on a drop; fleet-serve sweeps leftovers older than
`stt_request_timeout_sec` plus an hour at startup. `GET /fleet/media/{name}` refuses any name starting with a dot, so the
directory is never listed or served. The done job's `data` is the node's **full `core.Result`**, defers included
(`defer_class`, `meta.err_class`): a defer is a `done` job saying `deferred: true`, never an `error` job. The node's own
outputs (`.srt`, `.txt`, `.segments.json`) stay in `media_dir` and are fetchable by bare name; the asker fetches the
`.segments.json` when the inline segment list was truncated (`stt_max_inline_segments`). **Retention and read access:** the outputs (`stt-<digits>-<8 hex>.srt`, `.txt`, `.segments.json`) are removed when the
job record is evicted, or once older than `fleet_stt_transcript_ttl_min` (default 30; a negative value keeps them), swept
at fleet-serve start and on the job store's 5-minute janitor tick; a finished job restarts its outputs' clock, so a second
upload of a recording the pipeline's cache already holds keeps the first job's files a full TTL (and the pipeline treats a
cache hit whose files are gone as a miss). On a node that has a `fleet_auth_token`, `GET /fleet/media/{name}` serves those
names only to a bearer holder (a `401` otherwise, before the file is looked up, and for any case, trailing-dot or
trailing-space spelling of the name, which a Windows node would open as the same file); the asker's fetch already sends the
bearer. A loopback node with no token serves them as it always did.

**Advertisement.** `stt-upload` is in `supported_task_types`, with the additive, omitempty health fields `stt_hq` (the
node has an hq model; `false` is published, so an asker can tell it from a node that predates the door) and
`stt_upload_max_mb`, exactly when `STTUploadAdmissible` holds: a bound `stt_model` and the vision lane's reachability rule
(loopback, or a `fleet_auth_token`). `stt` is NOT the signal: every node with a whisper model lists it.

**Auth.** `stt-upload` is token-gated like the vision lane (`tokenGated`), on dispatch and on poll (`JobView.Gated`).
The legacy `stt` task joined the bearer rule **when the node had a `fleet_auth_token`** (0.165.0, D17): a tailnet peer can
no longer make the node convert and transcribe any file it can read. It is now gated like every lane (the special case is
gone: a node with no token that listens beyond loopback refuses it with `403`, a loopback node with no token takes it).
Compatibility: the only external dispatcher known to send `stt` jobs sends no bearer token; no production use through it
was found (its web console form sends an empty payload; the jobs seen on one node on 2026-10-01 were a test burst), so it
needs the bearer against a node that has a token. The legacy lane's transcripts
(`<basename>-<8 hex>.srt|txt|segments.json` in `media_dir`) need the bearer on `GET /fleet/media` like every artifact; unlike an
upload's they are the pipeline's content-keyed cache, shared with local transcriptions, so the node's transcript sweep and
eviction removal never touch them.

**Concurrency.** Both stt lanes, pushed and pulled, share ONE cap: `fleet_stt_max_concurrent` (default 1), a FIFO gate
inside the run closure. A job over the cap waits its turn in arrival order, its PAIR card still queued, and never fails
(before the cap, a burst failed `whisper-server 500 … matrix: model unloaded`); a poller sees such a job as `running`. A
waiter's context ending (shutdown) takes it out of the line. While jobs wait, `sttclient.Queued` keeps the model loaded
for them, so a burst pays one cold start. `stt` and `stt-upload` stay outside `fleet_max_concurrent_jobs` (an upload
parked at the stt gate must not hold a text execution slot while doing no work; `concurrencyCapped` lists both).

### Placement (asker side)

`route: local` is byte-identical to before the route existed, except that a `gpu_busy` defer gains a hint (the reason ends
`(route "auto" or "remote" lets a fleet node transcribe this)`) when `delegate_remotes` is configured. `auto` runs local
unless the local whisper request would actually be held right now: `modelaffinity.WouldBlockUpstream`, the upstream fence's
own first inspection (a lease fencing the cards over the model and the model not already resident), for the model the
call uses (`stt_model_hq` for an hq call). With a held whisper it tries a node (`delegate.PlaceSTT`: advertises
`stt-upload`, has an hq model when hq is asked, publishes an upload cap that fits the file, card not leased; ranked by
`betterRemote` like vision) and with none eligible it still runs local. `remote` forces a node; none eligible ⇒
`deferred: true, defer_class: capacity` (or `config` with no `delegate_remotes`), never a local run.

The asker reads the audio on its own box, converts it to 16 kHz mono Opus at 32 kbps (`audioio.ConvertToOpus16k`, about
14 MB an hour; the original file is sent when conversion is impossible and it fits the node's cap), sends it with the
bearer and the attribution headers, and polls with a budget of 180 s plus half the audio's length, never more than
`stt_request_timeout_sec` (an original sent as is has no known length and gets the whole `stt_request_timeout_sec`). On success it writes its OWN `.srt`, `.txt` and `.segments.json` under its `media_dir` (the
stem is the source's name plus 8 hex of the uploaded bytes and the model) from the full segment list, so every path in
the result is a local file. It validates the node's answer first (at least one segment, none ending before it starts,
starts never running backwards, a fetched list agreeing with the result's `num_segments`); a failure is a deferred result
naming the node. `meta.node` / `meta.placement` say where it ran, and the call is one PAIR card on the serving node plus
one asker ledger row (`route`, `placement`, `node`, `fleet_job_id`).

## The PAIR card relay (`POST /fleet/pair-relay`)

A box that is not a PAIR cluster member (a view-only box, a thin client where PAIR is not installed) has no PAIR identity
to card its own work with. A fleet-serve on a PAIR member relays for it: the box posts each workload frame to the member,
and the member's own emitter posts the card, resolving the node (a view-only node included) itself. The mechanism, the frame
fields and the trust notes are in [pair-workloads.md](systems/pair-workloads.md#the-card-relay-a-box-that-is-not-a-pair-member-d26);
what an operator of the node needs:

- **Advertisement.** `pair_relay: true` in health (additive, omitted when false) exactly when the node has a PAIR identity of
  its own (`pair_workloads_enabled` with PAIR installed here) and the reachability rule holds (a `fleet_auth_token`, or a
  loopback listener). A node that itself relays does not advertise it.
- **Auth.** Token-gated like the vision lane (`tokenGated`): `401` without the bearer, `403` on a tokenless node beyond
  loopback, answered before the body is read. `X-Offload-Asker` is required (`400`). `503` when the node cannot relay.
- **Limits.** Body at most 64 KiB (`413`), strict decode (`400`), and a token bucket per asker (5 frames/s, burst 60) and one
  over all askers (50/s, burst 200): `429` with `Retry-After`, before the body is read. At most 128 relayed cards stay open per
  asker and 512 overall: a frame that would open a new card past either is a `429` too; a terminal frame is always admitted.
- **The relaying box** sets nothing when it has `delegate_remotes` (`pair_workloads_relay` defaults to `auto`); a box with none
  names its members: `"pair_workloads_relay": ["http://<node>:18811"]`.

## Known limits (v1)

- Jobs are **in-memory**: a node restart loses in-flight jobs (the dispatcher's failure
  rule marks them lost). Once acked, a job is never re-dispatched.
- **Drain marks may be unobservable.** Shutdown marks survivors `error:"interrupted"`, but
  if the process exits right after, a poller may never read the mark. The dispatcher's
  lost-contact rule covers it (~50s to declare the job lost).
- **A duplicate dispatch arriving after the ~1h retention re-renders** — the id has been
  evicted, so it looks new. The dispatcher never re-dispatches post-ack, so hitting this
  needs a lost ack *plus* a 1h+ retry. Accepted.
- **`stt` has no measured footprint** (it's llama-swap-resident, not a ComfyUI render), so
  stt jobs route only with caller-supplied `params_b`.
- **The dispatch envelope rejects unknown fields** (strict decode): a future dispatcher
  field addition needs a node upgrade first. (The agent contract **inside** the envelope is
  the deliberate exception — its payload decoder ignores unknown fields so staggered node
  deploys interoperate; version skew is caught by its explicit `schema_version` instead.)
- **A remote media task never chooses where the node writes.** Media dispatch does not take the caller's word for a path,
  so a caller's `out` (image-gen, video-gen, animate, audio-gen, compose-video) or `out_dir`
  (run-graph) is ignored. It is not refused, so an older payload keeps working. The node writes
  its own `<media_dir>/<task>-<hash8>.<ext>` (run-graph: `<media_dir>`), and the result names that
  path. The caller fetches it by bare name from `GET /fleet/media/{name}`, which only ever served
  files directly inside `media_dir`. Before this rule, a caller-named path let any tailnet peer
  make the node overwrite any file its account could write.
- **Payload input paths (`still`, `ref`, `driver`, `clone`, `audio`) are node-local readable
  paths**, taken as given. That's the tailnet-trust posture restated: anyone who holds the token and can
  dispatch can already run renders; don't extend reach beyond the tailnet.
- `priority` is accepted and ignored (contract-reserved).
- **Media lanes need the fleet bearer** like every other lane (see
  [Auth: every lane needs the fleet bearer](#auth-every-lane-needs-the-fleet-bearer)): the dispatch, the poll and the
  artifact read of a media task are `401` without it on a node that has a token, and a node with none refuses them beyond
  loopback. The trusted network still bounds who can reach the listener; it is no longer the only boundary. The accepted
  v1 weaknesses of the bearer — one shared token, no rotation — are recorded in
  [ADR 0023](architecture/decisions/0023-agent-lane-tailnet-auth-and-locality.md).

## Quarantine (0.111.0)

A delegator quarantines a node for **30 minutes** when two or more results inside that window fail the
**document fingerprint** (the single any-of `(?P<docanchor>…)` regex acceptance `offload_research`
attaches to every page, built from prose content words only since 0.141.1) **and those failures outnumber the node's passes of
the same check inside the window** (0.184.0): the node answered about a different document. The fingerprint is lexical and
misfires on an honest answer now and then, so a node that passed most of its work is not quarantined by two misses; an honest
absence (a short statement that the page does not carry what was asked, `delegate.HonestAbsence`) counts as neither a strike nor
a pass. Quarantine is held in the delegator's
process (the MCP server), never persisted; a blocked node is skipped by placement and named in the
run's `probe_errors` as `quarantined until <time>`. Contract-caused acceptance failures (a strict
`contains:`, a thin page's `min_items`) never strike. `summary.quarantined` counts the flips.
