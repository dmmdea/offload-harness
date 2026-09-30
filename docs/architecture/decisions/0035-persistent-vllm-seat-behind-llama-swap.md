---
status: Accepted
date: "2026-09-06"
---

# 0035 — A persistent vLLM agent seat lives behind llama-swap as a systemd unit the entry starts and stops

> ## AMENDED 2026-09-08 — THE SEAT IS NOT PERSISTENT. IT IDLES OUT LIKE EVERY OTHER SEAT.
>
> This ADR's residency decision was wrong and shipped a seat that occupies a GPU forever. On the reference A2 that
> meant **10,338 of 15,356 MiB held with the engine idle and llama-swap reporting no models running at all** — the
> harness's own idle rule could not reach it.
>
> **Operator ruling:** *"NO MODEL GETS TO BE LOADED FOR MORE THAN 5 MINUTES IF IT GOES UNUSED, AS THE REST OF THE
> HARNESS… 30 MINUTES OF LOADING TIME IS WAY BETTER THAN NO USE, AND NO USE IS WHAT YOU CAUSE BY LEAVING A MODEL
> LOADED AND UNUSED."* A long cold load is a cost paid once, by one request. A pinned card is a cost paid
> continuously, by everything else that wanted the hardware.
>
> **Four independent mechanisms were in force, each sufficient on its own** — removing any three changes nothing:
>
> | mechanism | why it defeats the idle rule | now |
> |---|---|---|
> | `ttl: 0` on the entry | llama-swap never idles it out | `ttl: 300` |
> | `groups: {persistent: true}` | nothing may evict it | no `groups:` block |
> | `hooks.on_startup.preload` | loaded the moment llama-swap starts, wanted or not | no `hooks:` block |
> | unit `[Install] WantedBy=` + `systemctl enable` | **a boot-enabled unit outlives its own front door** — llama-swap's TTL can only unload what llama-swap started | no `[Install]` section |
>
> The last one is the subtle one and it is why the other three were not enough: the engine is a *system* unit, so
> once enabled it comes back at boot and stays up regardless of what the entry says.
>
> **What survives from this ADR:** the front-door design itself. llama-swap stays the one endpoint the node talks
> to, `cmd`/`cmdStop` still drive the unit through the polkit rule, and `gpu reserve --drain --unload-seat` still
> frees the card. Only the *residency* claims below are superseded. Gated by
> `TestVLLMSeatRendersAsAResidentMatrixMember` (positive TTL, no groups, no hooks) and `TestArtifactsLeaveNoTokens`
> (no `[Install]` section).



Decision provenance: the operator decided GO on 2026-09-06 16:49 (the measured arm: 8/8 on the harness's
8-digest set at a 47 s median against the llama.cpp 4B seat's 159 s) and ordered the build the same evening;
this ADR records the shape the build took and why, per the ownership rule in the index README.

## Update 2026-09-16 — this tier no longer declares the seat

[0047](0047-ampere-16-agent-seat-reaudit.md) re-audited the `ampere-16` agent seat blind and the 4B lost 24 of 24 to Qwen3.8-27B UD-IQ3_S + MTP. `profiles.json` therefore no longer declares a `vllm_seat` for `ampere-16`, and a fresh install of that tier builds no vLLM venv or unit. **The pattern below is unchanged and still correct** — it is the reference for any tier that seats a vLLM engine behind llama-swap, and the reference box's own unit is left installed (unbound) so the lane can be reverted with one config edit.

## Update 2026-09-30 — a crashed generation is cleaned up by the next start and by the stub's crash exit

`cmd` starts a seat and `cmdStop` stops it, and llama-swap runs `cmdStop` only for a stop it asked for. When the engine
**crashes** the `cmd` process exits, llama-swap marks the seat stopped, and nothing runs a stop. On the Windows/WSL shape
(`setup/templates/vllm-seat/windows-wsl/`, `seat_fg.sh` and `seat_stop.sh` in the distro) that left the dead generation's
engine workers (the API server exits, but a worker stuck in a GPU call ignores its SIGTERM) and its LMCache MP server
behind. On 2026-09-29 one such crash kept a seat down for 23 minutes: 59 failed starts and 75 HTTP 500s, because the MP
server still held its HTTP port and `seat_fg.sh` refuses to start on a port that is bound, while the workers held 14.5–16 GB
on every card.

**Decision.** The dead generation's leftovers are reaped at two points, both inside processes the harness already runs, and
by one script, `seat_stop.sh`:

1. **The next start.** `seat_fg.sh`, when no `vllm serve` of its own port is alive and it finds a leftover (a listener on the
   MP HTTP port, an `lmcache server` of its MP port, an engine process named `VLLM::*`), runs `seat_stop.sh` once, waits a
   bounded time for the MP HTTP port to free, and only then judges the port. The refusal on a port held by anything else is
   unchanged, and it is the last check in that block.
2. **The stub's crash exit.** `seat-cmd.ps1` exits 0 when the seat has stopped answering for 30 s. Before it does, it starts
   the operator-session stop task (the task `cmdStop` uses), waits for it (at most 90 s), and only then exits — unless the
   seat's own start task is still running, which means a live launcher owns the seat.

**Identity, not port.** `seat_stop.sh` reaps an engine process only when no ancestor of it is a live `vllm serve`, and the MP
server only by this stack's unit and its MP port (SIGTERM, then SIGKILL after `SEAT_REAP_WAIT_SEC`). It never picks a victim
by the port it holds, so a foreign listener on the MP HTTP port is refused and left running. A worker that survives SIGKILL
(a process stuck in the GPU driver stays in state D) is named in the log with its state, and the stop exits non-zero.

**Alternatives rejected.**

- *A resident watchdog or a guardian process per generation.* It is one more long-lived process on the operator's machine
  to poll for a death, and both points above already run at the moment a cleanup is needed.
- *Dropping `exec` so `seat_fg.sh` outlives the API server and cleans up at once.* The engine is the foreground process of
  the client llama-swap started so that ending the client reaps the engine (the swap-out); a wrapper that outlives it needs
  signal forwarding and changes that contract, and the two points above make it unnecessary.
- *Relaunching the seat at the crash.* A seat that comes back on its own is loaded with nobody asking, which is what the
  2026-09-08 amendment above ruled out (`ttl: 300`). The cleanup only frees the cards; llama-swap starts the seat again on
  the next request, as before.
- *Reaping whatever holds the MP HTTP port.* It frees the port in the case that hurt and reaps a foreign service in the
  next one.

**Consequences.** The stub change reaches a box only when the stub is re-rendered and deployed (a hand-edited deployed
stub is not replaced by a template edit), and `seat_fg.sh` and `seat_stop.sh` only when they are copied into the distro.
The stop task must never run late: it is waited for so the next start cannot overlap it, and it is skipped while the start
task is running (if the start task's client lingers after the API server died, the stub cannot tell that from a live
launcher and skips the cleanup, as it did before this update). The stub's other exits are unchanged: a start that fails
before the seat answers leaves its leftovers to the next start's cleanup. That SIGKILL
reaps a worker stuck in the GPU driver under WSL2 is not verified; the log names one that survives. The live check is
the next natural engine death: the seat ready again within 6 minutes, no refusal whose holder is the seat's own MP
server, and no `VLLM::Worker` process 60 s after the API server exits.

## Context

The `ampere-16` reference box (NVIDIA A2 16 GB at a 40 W / 1200 MHz lock) served its agent lane from a
llama.cpp 4B seat with a 32k advertised window and `--parallel 1` — under three concurrent delegating sessions
its subtasks queued 222–258 s behind four jobs on one slot (K×8 gate, 2026-09-06). vLLM on the same card
fans out to 293 tok/s at 32 streams and, with `fp8_e5m2` KV (free on Ampere), holds a 262k-token pool at
util 0.90. [ADR 0029](0029-lenovo-agent-lane-stays-on-the-4b-seat.md) had pinned the lane to the 4B seat after a
FreeToken finding; that finding is about FreeToken, not about vLLM.

The plan's phrase "point the fleet node at it (`endpoint`/`agent_model`)" hid a wrong turn: the node's
`endpoint` cannot move to vLLM. STT, image-gen, vision and the embedder live behind llama-swap on the same
box, and the agent task's admission wait (`/running`), roster probe (`/v1/models`), served-window probe and
grammar-free chat re-pack all read `cfg.Endpoint`. The seat had to appear INSIDE llama-swap's roster.

Two ways to do that were on the table:

1. **llama-swap spawns vLLM as a plain `cmd`** (the Qube's own vLLM seat pattern, born of WSL constraints).
   The engine would inherit llama-swap's sandbox (`ProtectHome`, `ProtectSystem=strict`, private `/tmp`) —
   a different JIT/compile-cache environment from the one every arm was measured in — every llama-swap
   restart would be a 2–4 min engine reload, and nothing would bring the seat up at boot before the first
   request.
2. **A persistent systemd unit, fronted by a thin llama-swap entry** — decided.

## Decision

- The engine is a **system unit** (`vllm-<seat>.service`): ~~enabled at boot~~ **NOT enabled at boot (amended 2026-09-08 — a boot-enabled unit outlives llama-swap's idle window)**, ~~`Restart=on-failure`~~ **`Restart=no` (amended 2026-09-18, register D-124 — `vllm-seat-cmd.sh` detaches on a new invocation, so a systemd relaunch is a seat llama-swap no longer tracks; it put the 27B on a card another lease held exclusively)**,
  `KillMode=mixed` (the engine's worker processes die with the cgroup), binding the **Tailscale IPv4** on
  its port (the tailnet is the trust boundary, as for the fleet node). Its launch line lives in one script
  (`ExecStart`) that waits for the Tailscale address at boot instead of failing into the restart budget.
- llama-swap gets a **thin entry**: `cmd` = `systemctl reset-failed` + `systemctl start`, then block while the
  unit is active or activating and its **InvocationID is unchanged** (an explicit or crash restart detaches the
  wrapper, so llama-swap re-attaches and health-waits instead of proxying `ready` to a reloading engine);
  `cmdStop` = `systemctl stop` — a REAL stop, so an unload through llama-swap frees the card. `proxy` is the
  literal address the engine binds; `useModelName` rewrites every alias to the served id (vLLM 404s unknown
  names); ~~`ttl: 0`~~ **`ttl: 300` (amended)**; `concurrencyLimit` = the engine's `--max-num-seqs`; ~~the entry sits in a `persistent`,
  non-swapping, non-exclusive group and is preloaded at llama-swap start.~~ **No group and no preload (amended).** `healthCheckTimeout` is raised for
  the cold load.
- llama-swap runs unprivileged under `NoNewPrivileges=yes`, so the start/stop is authorized by a **polkit
  rule scoped to that one unit and that user** (`manage-units`, verbs start/stop/restart/reset-failed) — never
  sudo, never a broader rule.
- The fleet node's config binds `agent_model` to the entry's id and `agent_ctx_tokens` to the engine's
  `--max-model-len`; nothing else in the config changes. The previous llama.cpp seat stays defined as the
  fallback (one config edit + node restart to revert).
- `--gpu-memory-utilization` is chosen by MEASUREMENT for coexistence, not from arithmetic: the pool is read
  from the engine's `GPU KV cache size` banner at the intended utilization, and the small seats' peaks are
  checked against what is left. Text-only serving (`--limit-mm-per-prompt` all zero) is part of the trim on a
  multimodal checkpoint.

## What this preserves, measured on the reference box (harness 0.113.19, 2026-09-06)

| invariant | evidence |
|---|---|
| `served_models` / `agent_seat_resident` stay roster-derived | health lists the id + aliases + the fallback; `agent_seat_resident: true` |
| `gpu reserve --drain --unload-seat` frees the card | drain read vLLM's `/metrics` through llama-swap; unload → `cmdStop` → unit inactive; card 0 MiB in 6 s |
| `gpu release --warm-seat` reloads it | `/upstream/<seat>/health` → `cmd` → seat `ready` in 35 s warm (126 s cold) |
| reclaim accounting stays honest | the seat is llama-swap-resident ("ours"), so the idle baseline is sampled only with the card empty; after a crash-restart the unit is briefly not-ours and the baseline reads high → under-promise, never over |
| the lane is faster and roomier | 8 digests through the fleet node: 8/8, 45 s median, 100 s spread (llama.cpp 4B: 159 s, 32k) at a 131,072 window; K×8 gate 88/88 → K=1 96.7 s, K=2 150.3 s, K=3 190.0 s (was 121.2 / 187.8 / 353.7 s) with 0 refusals, 0 queue-deadline losses |

## Consequences

- A llama-swap restart runs `cmdStop` on shutdown: every config edit costs one engine reload (35–250 s);
  the preload hook re-attaches on start. Accepted — config edits are rare on a services box.
- Heavier opt-in seats that no longer fit beside the engine cannot co-reside; the `persistent` group means
  nothing evicts the seat automatically. The entry's comment names them; a measurement window takes the
  text lease (`--unload-seat`) first, which is the same verb the launcher scripts already use.
- The harness's llama-server seat-pin probe (`/props`) 404s on vLLM — logged, non-fatal, `seat_config_*`
  absent on those rows; the same as the Qube's vLLM seat.
- The tier SEED is unchanged: the seat is a hand-installed venv and unit, not something the installer
  renders. The tier page and the operator guide point here; the reference files live in
  `setup/templates/vllm-seat/linux-systemd/`.

## Alternatives considered

- **Move the node's `endpoint` to vLLM** — rejected: breaks STT/media/embedding on the node and every
  llama-swap-shaped probe in the agent task.
- **A harness `agent_endpoint` config key** (a second base URL for the agent seat) — rejected for now: a
  multi-file harness change (residency probe, roster union, admission wait, drain/unload/warm verbs, docs)
  to reproduce what llama-swap already provides; revisit only if a seat must live outside llama-swap.
- **An exclusive group so the opt-in heavy seats evict the engine automatically** — rejected: `persistent`
  protects the seat from every accidental eviction; the opt-ins are measurement tools and already take the
  lease.

## Related

- [0029](0029-lenovo-agent-lane-stays-on-the-4b-seat.md) — the seat binding it replaces (FreeToken finding unchanged)
- [0023](0023-agent-lane-tailnet-auth-and-locality.md) — why the seat binds the Tailscale address
- [`docs/systems/fleet-node.md`](../../systems/fleet-node.md) — `served_models`, residency, the lease verbs
- [`setup/templates/vllm-seat/linux-systemd/`](../../../setup/templates/vllm-seat/linux-systemd/README.md) — the reference files
