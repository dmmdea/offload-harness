# GPU lease

## Purpose

The machine-wide, fenced, two-class lease that serializes GPU-heavy work across every
ingress — the harness CLI, the Node render runners, the fleet node, and anything else that
takes it. It exists so a text measurement and a media generation can no longer destroy each
other on a single shared card.

- **CLI:** `local-offload gpu status|reserve|release`
- **State:** `<state_dir>/gpu/{lease/meta.json, epoch}` — default `%ProgramData%\local-offload`
- **Decision record:** [ADR 0018](../architecture/decisions/0018-machine-wide-fenced-gpu-lease.md)

## Source map

| path | role |
|---|---|
| `internal/gpulease/gpulease.go` | **the only implementation**: `LeaseDir` (the one resolver), acquire/release, epoch fencing, the reclaim conjunction, `InspectDir` (the one read path) |
| `internal/gpulease/proc*.go` | per-platform liveness and process-start identity, exported so no consumer keeps a second copy |
| `internal/pipeline/pipeline.go` | takes the `media` lease around all five generation call sites and threads `GPU_LEASE_*` to the runner |
| `gpu_cmd.go` | the `gpu status\|reserve\|release\|hold` verbs, wrapper and `--detach` forms |
| `gpu_hide_windows.go`, `gpu_hide_other.go` | hidden spawn for the detached holder (a visible console gets closed, killing the hold) |
| `render/gpu-lock.mjs` | READ-ONLY participant: honours + fences an inherited lease, elects one unloader, drains, ComfyUI lifecycle. **Does not acquire.** |
| `internal/gpulock` | the read-only vision gate; delegates wholesale to `gpulease.InspectDir` |
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
weaker form by design — nothing ties the lease to a command's lifetime:

```
local-offload gpu reserve --class text --for 45m --reason "kv bench" --detach
local-offload gpu release --epoch <N>
```

`gpu status [--json]` reports the holder, its class, age, reason and declared expiry — and says
**"free (unreserved)"** explicitly, because an unreserved card is exactly when work is exposed;
that should be visible, not inferred from silence.

## Classes

| class | who takes it | may unload models? |
|---|---|---|
| `text` | a benchmark, eval, or measured run | no |
| `media` | image / video / audio / run-graph | **yes**, once per lease |

Both are exclusive — one card, one holder. The label carries intent, not access control.

**Ordinary interactive text calls are deliberately NOT lease participants.** There are thousands
a day at ~46 ms and leasing them is untenable. This is a known limit, not an oversight: a short
interactive call can still land inside a media lease and pay a reload.

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

## Known gaps

- **The reservation is a convention.** A raw `curl :11436` loop, a graph posted straight to
  ComfyUI, or a forgotten `gpu reserve` gets no protection.
- **The lease reduces the number of teardowns; the drain is what makes one safe.** Both needed.
- **Head-of-line blocking is structural** — a 45-minute video blocks everything behind it.
- `internal/pipeline` does not yet take a `media` lease around its own generation calls; the Node
  runner does, on the same path, so arbitration holds today.
