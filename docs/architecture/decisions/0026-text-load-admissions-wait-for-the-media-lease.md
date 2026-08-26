---
status: Accepted
date: "2026-08-26"
---

# Text admissions that would load a model wait for the media lease

## Context

ADR [0018](0018-machine-wide-fenced-gpu-lease.md) made the GPU lease machine-wide and fenced, and
deliberately left ordinary interactive text outside it: "thousands per day at ~46 ms, and leasing
them is untenable. That asymmetry is a known limit, not an oversight."

That limit was read as *a short text call inside a media lease pays a reload*. The observed cost is
larger. A `media` holder calls `freeLlamaSwap` once per lease, so the card is **cleared** for the
render — and the very next text call then makes llama-swap pull a multi-GB model straight back into
the VRAM the render was just given. The render and the text tier end up resident together on one
card. Reported symptom: the box becomes unusable under a render.

Nothing on the text side observed the lease. `internal/gpulock` (the vision gate) reads it,
`delegate.LocalBusy` reads it to consider remote placement, `llamaclient`'s `cascade_remote_lanes`
reads it to fail a *configured* lane off-box — but a plain text call on a box with no lanes
configured went straight to the local base. A lease that one side reads and the other ignores is not
mutual exclusion, which is the sentence ADR 0018 exists to stop being true.

ADR [0025](0025-model-residency-is-arbitrated-in-process-by-base.md) built the one chokepoint that
knows which admissions can move VRAM: `internal/modelaffinity`. A request joining an in-flight batch
of the model llama-swap is already serving cannot change residency. An idle-base take, or a promoted
switch, can. That distinction is what makes this affordable.

## Decision

**An admission that can change llama-swap residency waits while a `media` lease is held; one that
provably cannot is not gated at all.**

- The gate **reads** the lease with `gpulease.InspectDir` — the one inspection path, the same one
  the vision gate and `delegate.LocalBusy` use. It never acquires, never bumps an epoch, never
  writes. ADR 0018's arithmetic was about the *write* path; one `ReadFile` of a path that usually
  does not exist is not that cost.
- **Only load-triggering admissions pay it.** `tryJoin` takes the "same model, in flight, nobody
  parked" case before the lease is ever read, so a burst against the resident model costs nothing.
- **A promoted batch is not "resident" until its request goes out.** Promotion raises the in-flight
  count before anything has been sent, so while a promoted admission is still waiting for the card,
  a newcomer naming that same model must NOT be handed a join — it would slip past this gate and
  force exactly the load the promoted waiter is avoiding. The gate counts those `pending` admissions
  and refuses joins while any exist. Every other window here is an unavoidable check-then-act race
  against a lease taken a microsecond later; this one is not, because the gate knows the card is held.
- **One deadline per admission.** All lease waiting for a single `Admit` — before parking and again
  after promotion — shares one wall-clock deadline derived from the caller's budget, so a caller that
  waits, parks, and is promoted onto a card that has been taken again cannot spend that budget twice.
- **A promotion is re-checked at wake.** The park is a switch by definition, and a render can start
  during it; trusting the park-time read would leave a hole exactly one batch-drain wide, pointing
  the wrong way.
- **Waiting, not refusing.** A held card is congestion: an image render clears in tens of seconds
  and the caller then gets its answer. The wait is bounded by the caller's OWN budget (its resolved
  `http.Client.Timeout`) and by `ctx` — the same two bounds ADR 0025's in-process park uses. It is
  deliberately **not** bounded by the holder's declared TTL: `gpulease` stamps `DefaultTTL = 1h` as a
  reservation, not an estimate, so reading it as an ETA would turn every wait into an instant refusal.
- **Armed from `config.Load`**, the one funnel every entry point that can make a text call passes
  through, resolving via `gpulease.LeaseDir` so a second resolution order cannot appear. Same wiring,
  same reason, as `netguard.SetTailnetSuffix` beside it.
- Exhaustion returns a `*modelaffinity.LeaseError` naming the holder's class, pid, reason and how
  long it has held the card. Its wording carries the substring `pipeline.classifyErr` buckets
  congestion by, so the ledger files it as `timeout` rather than `other`.

**`media` only.** A `text` reservation is a benchmark holding the tier steady; its holder unloads
nothing, so a switch underneath it costs a measurement rather than the machine — while blocking every
interactive call for the length of an eval run would be a larger regression than the one it prevents.

**The holder's own process is NOT exempt.** Exempting by pid reads as obviously safe and would
silently reopen the incident in the deployment where it happened: `fleet-serve` and the MCP server
run a render and serve text tool calls in ONE process. Only an **inherited** lease exempts — the
`GPU_LEASE_EPOCH` that `pipeline.ambientLeaseEnv` reads, compared by value so a stale variable cannot
exempt anything — because `gpu reserve --class media -- local-offload …` runs the harness as the
holder's child and a text call there must not queue behind its own parent. There is no in-process
text call to deadlock: the one text step on the media path, the image prompt refiner, is hoisted
above `acquireMediaLease` on both the single and batch routes precisely so "the text call never
contends with our own render", and `runPipelineJob` takes only the in-process `mediaSlot`.

## Consequences

- Text no longer loads a model on top of a running render. Under a media lease a text call waits for
  the card, and on a short render it then succeeds instead of failing.
- **A render now costs text latency.** Under a long media lease, text admissions that need a load
  spend their whole budget waiting and then return a `*LeaseError`. That is the intended trade: the
  alternative is what made the box unusable. `delegate` already routes away from a busy local GPU
  (`LocalBusy`), so fleet-placed work reaches a remote node before it ever reaches this gate.
- **Hot-path cost** on an idle box: one `os.ReadFile` of a lease record that does not exist, per
  admission that is not a join. Tens of microseconds against a call budgeted at ~46 ms.
- The cross-process limit named in ADR 0025 is unchanged for text-vs-text. Text-vs-media is now
  machine-wide, because the lease it reads is.
- Two routes that can force a load still bypass this, exactly as they bypass ADR 0025's gate:
  `internal/agent`'s `/upstream/{model}/props` probes (`ProbeSeatPin` exists to warm a seat) and
  `internal/tokclient`'s `/upstream/{model}/tokenize`.
- **A check-then-act window remains, and is inherent.** A render started in the microseconds between
  the lease read and the request going out is not caught. Closing it would mean holding the lease
  across the call, which is the cost ADR 0018 refused for text. The window is one lease read wide and
  points the same way the pre-0.103.0 behaviour did, so it strictly improves on it.
- A test process is a process: `go test` on a box with a live render will see text admissions wait.
  That is the mechanism working, not a flake.

## Alternatives considered

- **Probe llama-swap for residency and gate only true loads.** Rejected. A `media` holder unloads
  once per lease, so during one the text tier is cold by construction and the probe would answer
  "not resident" almost every time — an HTTP round trip per call to learn what the lease already
  implies, plus the staleness race ADR 0025 rejected it for.
- **Refuse immediately instead of waiting.** Rejected: an image render clears in tens of seconds and
  the caller's budget covers it, so refusing would throw away answers that waiting delivers.
- **Bound the wait by the holder's `ExpiresAt`.** Rejected: `DefaultTTL = 1h` is stamped on
  essentially every media lease as a reservation, so this degenerates into never waiting.
- **Gate `ClassText` too.** Rejected for now — see the `media` only rationale above. Revisit if a
  measurement is ever destroyed by a text-side switch under a reservation.
- **Take a real `text` lease per request.** Rejected: ADR 0018 weighed and refused exactly this, and
  nothing about the volume has changed. Reading is not acquiring.
- **Thread the lease dir through client constructors.** Rejected: ~60 construction sites, where the
  one that forgot would be an ungated text lane with nothing to report it.
- **Exempt the holder's own pid.** Rejected — see above; it would un-gate the one-process deployment
  the defect was reported from.

## Related code

- `internal/modelaffinity/gpuwait.go` — the lease read, the bounded wait, `LeaseError`, `SetGPULease`.
- `internal/modelaffinity/affinity.go` — `tryJoin` (the not-a-load case) and `admitPromoted` (the
  re-check at wake).
- `internal/config/config.go` — `Load` arms the gate; pinned by `TestLoadArmsTheGPULoadGate`.
- `internal/gpulease/gpulease.go` — `LeaseDir`, `InspectDir`, the classes.
- `internal/pipeline/pipeline.go` — `acquireMediaLease`, `ambientLeaseEnv`, and the refiner hoisted
  above the lease.

## Related docs

- [ADR 0018](0018-machine-wide-fenced-gpu-lease.md) — the machine-wide fenced lease and its text exclusion.
- [ADR 0025](0025-model-residency-is-arbitrated-in-process-by-base.md) — the in-process affinity gate this extends.
- [`docs/systems/gpu-lease.md`](../../systems/gpu-lease.md)
- [`docs/systems/offload-pipeline.md`](../../systems/offload-pipeline.md)
- [`docs/glossary.md`](../../glossary.md)
