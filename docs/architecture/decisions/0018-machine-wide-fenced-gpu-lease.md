---
status: Accepted
date: "2026-07-25"
---

# Arbitration moves below the render path: a machine-wide fenced GPU lease

> ADR 0017 is reserved by an open PR (KV/TTFT measurement); this decision is numbered 0018 to
> avoid colliding with it.

## Context

One GPU is shared by llama-swap (the text/vision tiers) and ComfyUI (generation). Only ONE
GPU-heavy job may run at a time, and until now that was enforced by `render/gpu-lock.mjs`
alone.

**That is the defect.** The lock was taken only by the Node generation runners. Text work — a
benchmark, an eval, a measured agent run — took nothing. So when a media job arrived through a
different ingress (`POST /fleet/dispatch` on the fleet node) it acquired the render lock,
found it free, and called `freeLlamaSwap()`, which unloads every GPU-resident llama-swap model.
An in-flight text benchmark lost its tier mid-run.

Measured, not inferred:

| observation | value |
|---|---|
| `POST /api/models/unload/*` calls in the server log | **3,356** |
| ...of those, the text workhorse `gemma-4-e4b` | **330** |
| ...of those, the CPU memory stack (`embeddinggemma`, `bge-reranker`) | **0** |
| cost of one text-tier reload | ~5–7 s |

The zero is the fingerprint: excluding the memory stack is `freeLlamaSwap`'s own documented
invariant, which identifies the caller unambiguously. `freeLlamaSwap` runs *inside*
`withGpuSlot`, i.e. **once per job** — 3,356 is the arithmetic of a per-job side effect that
should have been per-batch.

Two further facts shaped the design:

1. **`gpu-lock.mjs` defaulted to `join(tmpdir(), ...)`, which is PER-USER on Windows.** A process
   in another security context silently took a *different* lock and mutual exclusion evaporated
   with no error anywhere.
2. **The unload route does not honour in-flight requests.** Measured on llama-swap v242: warm the
   model, start a 512-token generation, unload 3 s in — the unload returned in **1,265 ms without
   draining** and the generation died at **4,107 ms with `502 Bad Gateway`**. v242's TTL/model-request
   deadlock fix does not change this.

## Decision

1. **Arbitration moves below the render path into `internal/gpulease`, and every GPU consumer
   takes it regardless of ingress.** A lock only one side takes is not mutual exclusion. This
   deliberately **supersedes `internal/gpulock`'s read-only invariant** ("NEVER creates, reclaims,
   or removes the lock"), which existed only because acquisition lived in the `.mjs`.

2. **Two exclusive classes, `media` and `text`.** The label is not access control, it is intent:
   only a `media` holder may unload models, and a `text` reservation makes a dispatched render
   WAIT rather than destroy a measurement. **Ordinary interactive text calls are NOT lease
   participants** — thousands per day at ~46 ms, and leasing them is untenable. That asymmetry is
   a known limit, recorded here so it is not mistaken for an oversight.

2a. **A busy card QUEUES the job for a BOUNDED window, then defers with the holder's detail.**
   The single GPU slot has always existed to organize concurrent jobs into a serial queue, not to
   cancel them: a render arriving thirty seconds into someone else's render should run thirty
   seconds later. Trying once and dropping the job turns ordinary contention into lost work —
   a `gpu reserve --class text --for 45m` would silently drop 45 minutes of media requests.

   The bound is the other half. The old lock waited up to **30 minutes** (`videogen_wait_ms`
   20 min, `audiogen_wait_ms` 2 min), and the caller is usually a single tool call, where
   blocking that long is indistinguishable from a hang. Past the window the honest answer —
   *held by `<class>`, `<n>`s in, reason `<r>`* — is more useful to a caller that can retry.
   The ceiling is `gpu_wait_ms` (**90 s**, matching `vision_gpu_wait_sec` so the two GPU
   waiters behave alike). Only contention is waited out — an unwritable or cloud-synced lease
   location returns immediately, because waiting cannot fix a configuration fault. Queueing
   lives in Go with the acquisition; the runners never read a wait variable
   (`GPU_LOCK_WAIT_MS` is gone).

   **One knob, not three.** `videogen_wait_ms` / `audiogen_wait_ms` are retired and ignored.
   They existed so a cheap queued TTS was not starved behind a 20-minute video; at a 90 s
   ceiling that distinction buys nothing. Keeping them as overrides would have been worse than
   useless: the installer template shipped `videogen_wait_ms: 1200000` to every machine, so an
   upgrade would have silently restored the exact 20-minute wait this decision replaced —
   correct-looking config doing the rejected thing. Retired keys load cleanly and say so.

3. **The state root is machine-wide and REFUSES rather than degrades.** `%ProgramData%\local-offload`
   on Windows, `/var/lib/local-offload` elsewhere, overridable via `state_dir`. An unwritable root
   refuses to start instead of falling back per-user — the silent fallback *is* the bug. A root
   under a cloud-sync directory is refused outright: a sync client replicating a **lock file**
   between the two machines would hand one GPU to two hosts. The match is segment-aware, so a
   legitimate `dropbox-exporter` directory is not refused for containing the substring.

4. **The lease is FENCED.** A closing laptop lid is not a crash — the process survives and resumes,
   and would otherwise call `freeLlamaSwap()` on top of whoever holds the card now, which is the
   original incident replayed by a lid. Every acquisition bumps a monotonic epoch, stored OUTSIDE
   the lease dir so removing a lease cannot reset it, and `Check()` must precede every irreversible
   action — **unconditionally**, not only on the path that unloads models. Nesting the fence inside
   the unload election skipped it for jobs 2..N of a batch and for every `text` lease, and those
   jobs went on to submit a graph while fenced out; submitting a graph is irreversible GPU work.
   Holder identity records the process START TIME beside the pid, because pid-liveness alone reads
   a recycled pid as a live holder forever.

   **Issuing the token is a critical section, not just an atomic write.** tmp+rename makes each
   write indivisible and does nothing about two acquirers that both read *n* and both write *n+1*;
   measured, two concurrent acquirers were handed the same token. Because the token is threaded to
   children as `GPU_LEASE_EPOCH`, a duplicate lets a straggler from the first lease pass `Check()`
   against the second, unload on top of it, and delete its claim on release. The increment is
   serialized by an exclusive-create lock beside the counter.

5. **Reclaim is a conjunction, and both halves are load-bearing:**

   > *(holder provably gone)* **OR** *(heartbeat stale **AND** the declared window expired)*

   A bare heartbeat timeout is wrong: under the saturating benchmark a `text` reservation exists
   to protect, the holder can be descheduled well past any short TTL, and expiring it there hands
   the card to a render mid-measurement — the incident, reproduced by the fix. Pid-liveness alone
   is also wrong: `reserve --detach` spawns a PROXY holder that outlives the benchmark behind it,
   so a dead run could hold the card for its full declared window. Each half has a test.

6. **`Release()` is epoch-guarded.** A fenced-out straggler must never delete the CURRENT holder's
   lease. Leaking a lease is recoverable — it expires; silently handing the GPU to a third party
   is not.

7. **There is exactly ONE implementation, and Node is not it.** `internal/gpulease` owns
   acquisition, staleness, fencing, the epoch counter, path resolution (`LeaseDir`) and
   inspection (`InspectDir`). `internal/pipeline` takes the `media` lease around every
   generation call site and threads `GPU_LEASE_DIR/EPOCH/CLASS` down; the render runner
   inherits, fences, elects one unloader, and drains. A GPU job with no lease refuses.

   **This replaces the original decision, which was "share the schema".** Sharing the schema
   was not enough, and the record of why is the useful part: with two implementations, every
   review round found a NEW divergence, each fixed before the next surfaced — different atomic
   tokens (both sides holding the lease); `os.FindProcess` vs `process.kill(pid,0)` disagreeing
   on ACCESS_DENIED; a non-atomic epoch write measured restarting the fence at 1 in **24.5%** of
   concurrent reads; one side deleting the other's in-progress claim; and a *third* staleness
   rule in `gpulock` that called a live three-hour holder stale. Every fix was correct and the
   class survived, because the defect was the duplication, not any instance of it.

   The corollary is a rule, not a preference: a second reader that re-derives the judgement is
   the same bug wearing a different hat. When the heartbeat moved into a per-epoch file,
   `gpulock` — which by then shared the path, the record AND the rule — still diverged, because
   it reconstructed the answer from the record. It now calls `InspectDir`.

7a. **The heartbeat is a per-epoch file, not a field in the claim.** Renewing by
   read-modify-writing the record is a read-then-write race: a reclaim plus a fresh acquisition
   between the read and the write lets a stale holder stamp over the live record. `hb.<epoch>`
   makes a stale writer harmless. This is only affordable because the heartbeat is Go-only —
   an example of the collapse paying for itself.

8. **`freeLlamaSwap` is hoisted to once per LEASE, and it DRAINS FIRST.** Under an inherited
   lease `withGpuSlot` skips the acquire and **elects exactly one job to unload** through an
   `O_EXCL` per-epoch marker in the lease directory. The election is the load-bearing half:
   skipping the unload without anyone performing it is not a hoist, it is an omission, and it
   left a leased render running with every model resident.
   `quiesceLlamaSwap` polls `/upstream/<id>/slots`, where `is_processing` was verified true
   throughout a 23 s / 1500-token generation and false on completion. It is fail-SAFE, not
   fail-open-silent: an unreadable `/slots` yields `drained:false` plus the tiers it could not
   observe, and the caller logs that it proceeded without a verified drain. A stuck tier times out
   rather than deadlocking the render queue.

   Rejected: blocking indefinitely until a drain is verified (a stuck tier would freeze all
   generation), and unloading without draining (measured to kill in-flight work).

## Consequences

- The incident is closed at the mechanism level for every consumer that takes the lease, and
  `gpu status` reports an unreserved card explicitly so exposure is visible rather than inferred.
- **The reservation is a CONVENTION, not enforcement.** The lease binds only code paths that take
  it. A raw `curl :11436` benchmark loop, a graph posted straight to ComfyUI on :8188, or a session
  that simply forgets `gpu reserve` is exposed exactly as before. Closing that gap is ergonomics
  (make the bench harness reserve by default), not mechanism.
- **The lease reduces the COUNT of teardowns but does not make any single teardown safe** —
  the drain does. Both are required; neither alone is sufficient.
- Head-of-line blocking is unchanged and structural: one 45-minute video blocks everything behind
  it. ComfyUI cannot suspend a diffusion run, so preemption would be cancel-and-requeue. The
  bounded queue does not fix this — it bounds how long a waiter pretends otherwise.
- Waiting is in-process and unfair: waiters poll, so the job that arrives first is not guaranteed
  to be the one that takes the card. A durable, ordered queue is the follow-up (as is a supervised
  `fleet-serve`, which needs its own approval).
- Running the harness under `gpu reserve --class media -- local-offload …` INHERITS the ambient
  lease rather than acquiring a second one, which would have queued the child behind its own
  parent. An ambient lease that is no longer current refuses the job instead of quietly taking a
  fresh lease, so a lost reservation is visible rather than papered over.
- **Mutual exclusion is therefore two-layered.** The file claim serializes across PROCESSES;
  an in-process slot serializes within one. The second layer is not redundant: jobs that inherit
  one lease have no claim to contend on, so `gpu reserve --class media -- local-offload
  fleet-serve` — which runs `Pipeline.Run` inline in a `net/http` handler goroutine — would
  otherwise put two renders on the card at once. Measured at 256 ms of overlap on 250 ms jobs,
  i.e. fully concurrent. Lock order is always slot → file lease.
- **The design adds no daemon, scheduled task or watchdog, and nothing ticks at idle.** Every
  timer is scoped to work in flight: a 15 s heartbeat while a lease is held, a 1 s probe while a
  job is queued behind another process (capped by `gpu_wait_ms`), and nothing at all when a job
  is queued behind one in the same process — that waiter blocks on a channel. A waiter also no
  longer issues a fencing token per probe, which had it taking a machine-wide lock and doing
  four file operations a second for claims that could not succeed. `gpu reserve --detach` is the
  one continuous poller, exists only on explicit operator command, and exits by itself.
