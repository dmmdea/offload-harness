# Changelog

All notable changes to `offload-harness` are documented in this file.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versioning: [SemVer](https://semver.org/).

## [Unreleased]

## [0.107.0] - 2026-08-27

### Added — `h3`: MiniMax-H3 joint-AV as an opt-in videogen family

Operator-approved on the T4 bake-off verdict (matched still/prompt/seed vs the
seated LTX-2.5): H3 turbo-8 wins prompt adherence (full scripted action arcs,
multi-shot storyboards with hard cuts), i2v source fidelity, and audio design,
at ~2x the wall. LTX-2.5 remains the default family; pass `model:"h3"`.

- `render/wf-h3-av.mjs` (+ tests): the official `video_minimax_h3` template's
  subgraph in API format. Turbo-8 is the DEFAULT (the verdict recipe;
  `hero:true` = the template's non-LoRA 20-step alternative). The still is
  OPTIONAL — t2v without, i2v with. NATIVE single-card loader only, no
  DisTorch option: pooling upcasts the int8 DiT 20.97 GB → 37.46 GB (measured),
  while the plain loader keeps native convrot-W4A4 with partial offload.
- Family allowlists extended in lockstep (runner dispatch, pipeline
  provenance/footprint, fleet advertisement) per the writer/advertiser
  one-namespace rule; MCP schema + CLI help document the new enum value.

## [0.106.1] - 2026-08-27

### Fixed — video_watch tail windows and ffmpeg 9 mjpeg strictness

- **`videoio`: a tail window shorter than one sampling interval no longer defers.**
  Found live on a 44-min sweep: the final 1.25 s window at fps 0.2 starved the
  `fps` filter (zero frames), and ffmpeg 9's lazily-opened mjpeg encoder then
  failed at EOF-flush on limited-range YUV (exit -1). Such windows now sample as
  ONE plain frame; both samplers also pass `-strict unofficial` — ffmpeg 9
  rejects limited-range YUV (normal camera footage) at default mjpeg strictness
  where 8.x only warned. Reproduced and fix-verified against the real file.
- **`gpulock`: the two flaky WaitFree window tests are deterministic** (closes #81).
  `TestWaitFreeReleasedMidWait`'s release goroutine did a one-shot `os.Remove`
  with the error swallowed — under full-tree parallel load on Windows a
  concurrent poll's open handle can fail that single remove with a sharing
  violation, and the test then honestly reported a lease nothing had released.
  The release now retries (bounded) and the test fails loudly if the release
  itself never lands. `TestWaitFreeBoundedWait` gets a 15 ms epsilon for the
  ~15.6 ms Windows system-timer granularity.
- **docs**: ROADMAP corrections — Phase 3's Resolve-spend gate was stale (Studio
  is purchased + activated on the editor rig; only the cut-list/cleanup
  automation half remains); Docker-leftovers resolved on the reference box.

## [0.106.0] - 2026-08-27

### Added — `offload_animate_character`: WAN-Animate-2 motion retargeting as a first-class media route

The T4 validation (2026-08-27) proved WAN-Animate-2 distilled as a genuinely new
capability — identity-preserving retargeting of a driver video's motion onto a
reference character image, native int8 on one 16 GB card. The operator approved
routing it; this release wires it through every layer the video route has:

- **MCP tool `offload_animate_character`** + **CLI `animate-character`**
  (`<out.mp4> <ref.png> <driver.mp4> "<prompt>"`): new task
  `animate_character`, its own pipeline branch (media lease class `animate`,
  footprint family `wan-animate2`, defer-on-any-failure).
- **`render/comfy-animate.mjs` + `render/wf-wan-animate2.mjs`** (+ unit tests):
  the official `video_wan_animate2_distilled` template's Motion Transfer
  subgraph, flattened to API format with the subgraph's node ids, single
  81-frame chunk. The distilled recipe is pinned (10-step lcm, cfg 1, shift 5);
  the pose cache is pinned **cpu/default** — the template's shipped `gpu/int8`
  cache hard-kills ComfyUI mid-step-1 on the reference box (silent process
  death; the watchdog then aborts at 240 s). Output keeps the driver's audio.
- **Config**: `animategen_script` (default `render/comfy-animate.mjs`),
  `animategen_timeout_sec` (default 1800), per-machine weight bindings
  `animategen_unet` / `animategen_text_encoder` / `animategen_clip_vision` /
  `animategen_vae`, and `animategen_width`/`animategen_height` working-res
  defaults (0 = the template's 482×854).
- **Fleet**: task `animate` advertised when `animategen_script` is set
  (family `wan-animate2`), with the same payload the MCP tool takes.
- **mediacap**: `animate_character` route row in `offload_status` media.

## [0.105.0] - 2026-08-27

### Fixed — ComfyUI >= 0.34 hid every GPU but the first on Windows, breaking the pooled tiers

ComfyUI 0.34.0 defaults Windows to `CUDA_VISIBLE_DEVICES=0` when the operator passed no
device selection (upstream #15737 "Limit Windows multi-GPU visibility" + #15813). On the
`blackwell-2x16` tier every DisTorch2 pooled graph then failed prompt validation —
`donor_device: 'cuda:1' not in ['cpu', 'cuda:0']` — so image and video generation
deferred on a box with two healthy cards.

`ensureComfy` now spawns ComfyUI through `cudaVisibleEnv()`: on Windows, when the
operator has not already scoped devices (env `CUDA_VISIBLE_DEVICES`, or a
`--cuda-device` in `COMFY_EXTRA_ARGS`) and more than one NVIDIA GPU is present, the
child gets every device listed. Env-based rather than `--cuda-device all` so older
ComfyUI versions (integer-only flag) keep working. Multi-GPU spawns also carry
`--disable-pinned-memory` — the second half of upstream's own guidance, avoiding the
Windows CUDA host-transfer failures that motivated their change. Single-GPU boxes and
non-Windows nodes are byte-identical. Live-verified on the 2-card box: the DisTorch2
donor enum returns `["cpu","cuda:0","cuda:1"]` and the krea2 pooled graph validates.

## [0.104.0] - 2026-08-27

### Security — dependency bumps (5 Go-stdlib-adjacent advisories closed)

- `landlock-lsm/go-landlock` v0.9.0 → v0.10.0 — GHSA-vv6c-69r6-chg9: best-effort mode
  had silently stopped restricting TCP bind/connect; we had been running the vulnerable
  version since adoption.
- `golang.org/x/sys` v0.46.0 → v0.47.0 — CVE-2026-39824, integer overflow in
  `windows.NewNTUnicodeString`; directly in scope on the Windows fleet.
- `golang.org/x/text` v0.38.0 → v0.41.0 — CVE-2026-56852, infinite loop on invalid input.
- `golang.org/x/net` v0.41.0 → v0.58.0, `modernc.org/sqlite` v1.37.0 → v1.57.0,
  `santhosh-tekuri/jsonschema/v6` v6.0.2 → v6.0.3 (transitives came along).
- `modelcontextprotocol/go-sdk` deliberately HELD at v1.6.1: we are already past its four
  advisories, and v1.7.0 is a breaking protocol change that gets its own migration.
- Toolchain: built and tested on Go 1.26.7 (five stdlib CVEs fixed vs 1.26.5; 1.26.6
  skipped — it broke unencrypted HTTP/2). Go 1.27.0 deferred: its encoding/json v2
  switch is not gated on go.mod and this harness is JSON-heavy.

### Fixed — the root-package seam test no longer deadlocks under a live render

- `TestAgentSeatAndCascadeSeatDoNotThrashOneBase` inherited the machine-wide GPU-lease
  gate armed at the REAL state root by earlier tests in the same binary. On a box where
  another session held a live media lease, the agent seat's admission waited out the
  full lease budget and the test hung on an unbounded channel receive until the
  package's 10m limit (observed live: "a media job holds the GPU (pid 40588)").
  The test now arms the gate at an empty `t.TempDir()` — it is about the process-local
  seam between the two text lanes, not the lease — and the first-request wait is
  bounded at 30s so a regression fails fast instead of timing out the suite.

## [0.103.0] - 2026-08-26

### Fixed - text no longer loads a model into VRAM a running render is using

- **The defect.** ADR 0018 left ordinary interactive text outside the machine-wide GPU lease -
  "thousands per day at ~46 ms, and leasing them is untenable" - and that was read as *a short text
  call inside a media lease pays a reload*. The real cost is larger: a `media` holder calls
  `freeLlamaSwap` **once per lease**, so the card is CLEARED for the render, and the very next text
  call made llama-swap pull a multi-GB model straight back into the VRAM the render had just been
  given. Render and text tier resident together on one card. Reported symptom: the box becomes
  unusable under a render.
- **Nothing on the text side observed the lease.** The vision gate reads it, `delegate.LocalBusy`
  reads it to place work remotely, `cascade_remote_lanes` reads it to fail a *configured* lane
  off-box - but a plain text call on a box with no lanes configured went straight to the local base.
  A lease one side reads and the other ignores is not mutual exclusion.
- **The fix: gate the LOAD, not the request** (`internal/modelaffinity/gpuwait.go`). An admission
  that can change what llama-swap holds resident - an idle base, or a promoted switch - waits while a
  `media` lease is held. An admission that JOINS the in-flight batch of the model already being
  served cannot move VRAM and is not gated at all, so a burst against the resident model still costs
  nothing. Hot-path cost on an idle box: one `os.ReadFile` of a lease record that does not exist.
- **Reads the lease, never acquires it.** Via `gpulease.InspectDir`, the one inspection path (the
  vision gate and `delegate.LocalBusy` use the same one). ADR 0018's arithmetic was about the WRITE
  path - epoch bump, claim file, heartbeat, release - and none of that happens here.
- **A promotion is re-checked at wake.** The park IS the model switch, and a render can start during
  it; trusting the park-time read left a hole exactly one batch-drain wide, pointing the wrong way.
  Pinned by `TestPromotedSwitchRechecksTheCard` and by a mutant.
- **A promoted batch is not resident until its request goes out.** Promotion raises the in-flight
  count before anything is sent, so a newcomer naming that model would otherwise be handed a join and
  slip past the gate — forcing the exact load the promoted waiter was waiting to avoid. The gate now
  counts `pending` admissions and refuses joins while any exist. Found by the clean-context review
  lane; pinned by `TestJoinIsRefusedWhileAPromotedBatchWaitsForTheCard` and by a mutant.
- **One deadline per admission.** All lease waiting for a single `Admit` shares one wall-clock
  deadline derived from the caller's budget, so waiting, parking, then being promoted onto a card
  that has been taken again cannot spend that budget twice.
- **Waits, does not refuse.** An image render clears in tens of seconds, so a blocked caller polls
  until the card frees, bounded by its OWN budget (the resolved `http.Client.Timeout`) and by `ctx` -
  the same two bounds the in-process park uses. Deliberately NOT bounded by the holder's declared
  TTL: `DefaultTTL = 1h` is a reservation, not an estimate, so reading it as an ETA would turn every
  wait into an instant refusal.
- **`media` only.** A `text` reservation is a benchmark holding the tier steady; its holder unloads
  nothing, so a switch underneath it costs a measurement rather than the machine, while blocking
  every interactive call for the length of an eval run would be a bigger regression than the one it
  prevents.
- **Only an INHERITED lease exempts a caller** (`GPU_LEASE_EPOCH`, compared by value so a stale
  variable cannot exempt anything), because `gpu reserve --class media -- local-offload ...` runs the
  harness as the holder's child. The holder's own **pid is deliberately not exempt**: `fleet-serve`
  and the MCP server render and serve text in ONE process, so a pid exemption would un-gate exactly
  the calls that trampled the render.
- **Armed from `config.Load`**, the one funnel every entry point that can make a text call passes
  through, resolving via `gpulease.LeaseDir` so a second resolution order cannot appear - the same
  wiring, for the same reason, as `netguard.SetTailnetSuffix` beside it. Pinned by
  `TestLoadArmsTheGPULoadGate` on `modelaffinity.GPULeaseDir()` and by a mutant.
- **Named outcome.** Exhaustion returns a `*modelaffinity.LeaseError` carrying the holder's class,
  pid, reason and how long it has held the card; its wording carries the substring
  `pipeline.classifyErr` buckets congestion by, so the ledger files it as `timeout`, not `other`.
- **Cost, stated.** Under a LONG media lease, text admissions needing a load now spend their budget
  waiting and then fail. That is the intended trade. `delegate.LocalBusy` already routes fleet work
  away from a busy local GPU before it reaches this gate.
- ADR [0026](docs/architecture/decisions/0026-text-load-admissions-wait-for-the-media-lease.md);
  `docs/systems/gpu-lease.md` corrected where it described the carve-out.

## [0.102.0] - 2026-08-26

### Fixed - two text lanes no longer thrash one llama-swap by asking it for different models

- **The defect, in one line.** Every text seat in the default config points at one endpoint
  (`agent_model`, `model`, `reasoning_model`, `escalation_model`, `triage_model`, `vision_model`, all
  on `http://127.0.0.1:11436`), llama-swap serializes model residency, and **nothing in the harness
  serialized by model**. Two lanes naming different models forced an evict-and-reload each time they
  interleaved. Measured cost: the agent seat (`qwen3.8-27b`) degraded ~4x, 72 s to 307 s per call.
- **Not fixed by the fleet queue (0.100.0) or re-placement (0.101.0).** Both competing calls were on
  the SAME box; a queue that distributes across machines has nothing to distribute away from.
- **The gate: `internal/modelaffinity`.** An in-process admission gate every llama-swap generation
  request passes through. Same model on the same base -> concurrent, untouched (llama-swap already
  queues those harmlessly; the expensive event is the SWITCH). A different model on a base that has
  in-flight requests -> parks until they drain, then proceeds. N interleaved switches become one
  switch per batch.
- **Keyed on the RESOLVED BASE, never on the model.** `llamaclient.resolveEndpoint` can hand two
  models two different bases (a `seat_endpoints` pin, a `cascade_remote_lanes` failover, or the
  default), and two models on two llama-swap instances do not contend at all. The gate consumes the
  base that function already decided and never re-decides it. Keying on the model instead would
  serialise lanes that never conflicted - pinned by a mutant.
- **Both lanes take it, because a lock only one side takes is not mutual exclusion.** The gate is
  wired into `llamaclient.Generate` / `GenerateVision` / `GenerateVisionInterleaved` **and** into
  `agent.LLMClient.Chat`, which posts to `/v1/chat/completions` on its own and never went through
  `llamaclient`. The agent seat is the lane the incident degraded; gating only the cascade side would
  have left it exactly as it was.
- **Bounded, with an honest outcome.** A park is bounded by `(batches ahead + 1) x budget`, where
  budget is the resolved `http.Client`'s own request timeout (120 s by default) - fixed at park time,
  never extended by progress. Each batch ahead drains within one budget because every member is a
  single request bounded by that timeout and the barrier stops a batch taking new members once
  someone parks. Exhaustion returns a `*modelaffinity.WaitError` naming the base, the model wanted,
  the model that held the slot, the in-flight count and the switches queued ahead - not a bare
  timeout. The caller's own `ctx` bounds the wait too and is usually the tighter of the two.
- **No starvation.** Once a caller parks, later arrivals naming the RESIDENT model park behind it
  instead of joining the running batch, so a steady stream of the resident model cannot hold the slot
  forever. Promotion always takes the model at the HEAD of the queue, so every parked caller reaches
  the head in bounded time.
- **Cost on the hot path**: two uncontended mutex acquisitions and zero allocation for an admission
  that does not block. This is deliberately NOT a `gpulease` participant - that package excludes
  ordinary interactive text ("thousands per day at ~46 ms, and leasing them is untenable"), and that
  judgement stands: a filesystem lease per text call remains off the table.

### Known limits (stated, not worked around)

- **Process-local.** The registry lives in one harness process. Two harness processes on one box - an
  MCP server plus a CLI invocation, or two MCP servers under two editors - still thrash each other
  exactly as before, because neither can see the other's in-flight set. Making it machine-wide would
  require the same fenced, pid-recycle-safe, machine-wide state root `internal/gpulease` uses, i.e.
  an acquire/release round trip through the filesystem on **every** text call. That is precisely the
  cost gpulease refused for text, so it is named here rather than built.
- **Two load-triggering routes stay outside the gate**: `internal/agent`'s
  `/upstream/{model}/props` probes (`ProbeSeatPin` exists to WARM a seat, so gating it would fight
  its purpose) and `internal/tokclient`'s `/upstream/{model}/tokenize`. Both are affine to their own
  caller's seat and neither is a burst source; a tokenize is also a separate admission from the
  generation that follows it, so gating it could not batch the two anyway. Holding one admission
  across tokenize-then-generate is a larger change through `internal/pipeline`.
- **STT and media are untouched by design** - `internal/sttclient` talks to whisper-server in a
  different process behind its own `inferMu`, and the media path is arbitrated by `mediaSlot` plus
  the `gpulease` media class.

### Docs

- New ADR [0025](docs/architecture/decisions/0025-model-residency-is-arbitrated-in-process-by-base.md).
- `docs/systems/offload-pipeline.md`, `docs/systems/coding-agent.md`, `docs/systems/gpu-lease.md` and
  `docs/glossary.md` updated in the same change.

### Repo hygiene

- `internal/agent/client.go` had a pre-existing `gofmt` deviation (a missing blank line before
  `type wireReq`); fixed while editing that file.

## [0.101.0] - 2026-08-26

### Fixed — a node refusing a job no longer loses the work

- **The defect, in one line.** `internal/delegate/run.go`'s `retryable` returned `false` for any
  result carrying an `Err`, so a **refusal was terminal for that subtask**: a `503 queue full`, a
  `409`, or a node the delegator could not dial ended the subtask, no other node was tried, local was
  never asked, and the work was simply not done. On a healthy fleet, with a single delegator, with
  idle siblings one hop away. `alternativeNode` — which already does the right thing — was reachable
  only from `failed_verification` / `abstention`, never from a refusal.
- **Re-placement.** A refused subtask is now offered to another eligible remote (the same capability
  gate, the same ranking, every already-tried node excluded) and then to the local seat.
  `route=remote` still never falls back to local — an explicit route is not silently overridden.
- **The refusal class, decided from the STATUS and never from message text.** Re-placed: status `0`
  (never reached the node), `404`, `408`, `409` (*"job previously failed on **this node**"*), `429`,
  and every 5xx — all statements about that node right now, which another node answers differently.
  NOT re-placed: `400`, `401`, `403` and every other 4xx — the node rejecting *this request*. The
  delegator hands the next node byte-identical bytes and the same bearer token, so re-placing a 4xx
  only collects the same answer N times while spending the contract's budget. The `401`/`403` line
  has a stated bound: one shared `fleet_auth_token` per fleet is the documented posture, so a
  rejected credential is an operator fix, not a routing one.
- **Bounded, per SUBTASK.** The bound and the exclusion set live in one per-subtask ledger that every
  placement consults — the first attempt's re-placement loop and the verification retry's both. The
  ceiling is **five** placements: the first choice, at most `maxRemoteReplacements` = 2 re-placements
  onto further remotes, at most one fall-back to the local seat, and at most one verification-retry
  placement (a separate mechanism with its own bound of exactly one, deliberately not folded in).
  **Every one of the five targets a dial base the subtask has not used before.** Local is not charged
  against the numeric bound — it is the one seat always able to take the work, so a wide roster must
  not spend the bound before reaching it — but it is in the exclusion set, which is what holds it to
  one use. A refusal that survives three DISTINCT nodes is a fleet-wide condition, and each refused
  placement can cost up to `dispatchAttempts` × 30 s of dial time before a transport verdict.
- **Deadline-safe, and stated precisely.** Every placement is handed what is LEFT of the contract's
  `timeout_sec` (elapsed rounded UP), **re-measured immediately before dispatch** — selecting a node
  blocks on a sequential fleet probe, so the number taken before selection can be badly stale by the
  time it would be written into a contract. That probe is itself bounded by a context carrying
  whatever the contract has left, so a roster of blackholing nodes cannot spend a budget the subtask
  no longer owns. A re-placed subtask can never be given more **execution** time than its contract
  granted. `timeout_sec` is the execution budget and **not** an end-to-end wall ceiling: placement
  overhead (the fleet probe, and up to `dispatchAttempts` × `dispatchRequestTimeout` of dial time)
  and 0.100.0's queued-time credit are both bounded but neither is charged to it. Anything that
  called `timeout_sec` a wall ceiling — including a comment added earlier in this release — was
  wrong and has been corrected.
- **Dispatch time ONLY, and that is a safety property.** A refusal is a NON-ack: the node never took
  ownership, so no seat can be running the contract and re-placing it cannot produce two concurrent
  runs. Nothing after a `202` is ever moved — a poll `404`, a queue deadline, a poll deadline all
  leave a job the node may still hold. One honest residual is recorded in the code and the docs: when
  BOTH dispatch attempts fail at transport level the first POST may have landed with its ack lost, so
  the abandoned node could still run the contract once (wasted compute on a read-only lane, against
  losing the work with certainty).
- **A distinct outcome when nobody takes it**, with its own stable prefix and never a manufactured
  defer: `placement refused: <n> node(s) refused this subtask and none of them ran it (<node>: <what
  it said>; ...); <why the delegator stopped asking>`. It is a **failure** (`summary.failed`,
  non-zero CLI exit, `IsError` on the MCP surface) for the same reason 0.100.0's `queue deadline` is:
  a defer manufactures a payload shaped like something a seat produced, and no seat was ever asked.
  Three sentences, three facts — `placement refused` (nobody took it), `queue deadline` (one node
  accepted it and never started it), `poll deadline` (one node started it and never finished it).
- **The two pre-existing bounded retries are untouched and stay separate.** `dispatchAttempts` (2,
  same job id) is about transport doubt at ONE node; `maxRedispatches` (2, same job id) is about a
  poll `404`. Neither was folded in or duplicated, and a re-dispatch refusal inside the polling
  window is deliberately NOT re-placeable.

### Added — placement is capacity-aware

- **`queue_depth` alone was never a placement signal**: it is a count with no scale. `Place`
  tie-broke on it without ever comparing it to a limit, so a node at depth 1 of a 1-deep queue — a
  certain `503` — beat a node at depth 500 with no published ceiling. 0.100.0 published the numbers
  that fix this; `internal/delegate/nodeview.go` decoded three of the four, and `max_queue_depth` —
  the one the whole rule rests on — was not among them. It is decoded now.
- **Three ordered ranking keys**, all boolean-or-int so the ordering stays transitive on a mixed
  fleet: (1) not provably saturated beats saturated (`max_queue_depth > 0 && queue_depth >=
  max_queue_depth` is exactly the condition that produces `503 queue full`); (2) a provably free
  execution slot beats one that is not provable (`queue_depth == 0`, or `jobs_running <
  max_concurrent_jobs` with `jobs_queued == 0`) — the job starts now rather than waiting in
  `accepted`; (3) lower `queue_depth`, the original rule with its original meaning, still deciding
  everything the first two do not. Ties keep roster order.
- **`0` means UNKNOWN for both limits**, never "unlimited" and never "full": a node publishes `0` for
  unlimited and a node too old to publish the field decodes to `0` as well, and the delegator cannot
  tell them apart. Unknown is neither credited nor blamed — which is why key (2) treats
  `queue_depth == 0` as proof on its own, so a completely idle node publishing no limits is not
  demoted below a loaded node that does.
- **Saturation is a RANKING input, not a capability.** `remoteEligible` is unchanged and a saturated
  node is still chosen when nothing better exists, because the DELEGATOR'S COPY of these numbers is
  stale by construction: the snapshot ages between the health GET and the dispatch POST, and this
  run's own concurrent siblings (`runConcurrency`) eat the headroom it measured. Hard-excluding on a
  number that is stale by construction would strand a node that has since drained; re-placement
  covers the case where the reading was right. (Note for anyone who read an earlier draft: the node
  does **not** cache these counters — `handleHealth` walks the job store live in the same request.
  What is cached over there is the VRAM snapshot and `agent_seat_resident`.)

### Added — the response says when the fleet is shedding load

- `summary.replaced` / `summary.replacement_recovered`, and per subtask `replacements` +
  `replacement_note` naming every node that refused and what it said. All `omitempty`: a run where
  nothing was refused publishes byte-identically to before. Without them a fleet quietly shedding
  every subtask onto one box reads exactly like a healthy one — four green buckets and nothing
  anywhere saying the roster is saturated.
- `replacement_recovered` is stated on the REFUSAL, not on the answer: it means a node took the work,
  not that the answer was good. The four outcome buckets say what the answer was.

### Docs

- `docs/FLEET-NODE.md` gains **Re-placement on refusal** and **Capacity-aware placement**, and its
  "the `503` is terminal for this repo's delegator" note — corrected in 0.100.0 and made false by
  this release — is rewritten to what is now true rather than reverted to the old overclaim (which
  described a different repository's media dispatcher).
- `docs/flows/fleet-job-lifecycle.md` and `docs/systems/fleet-node.md` (the pitfall, the job protocol
  and the health-advertisement paragraph) updated to match.


## [0.100.0] - 2026-08-26

### Added — the fleet node has an actual job queue; backlog and concurrency are now two limits
- **`Accept` used to BE `start`.** It wrote the record and immediately launched
  `go func(){ … markRunning … }`, so `JobAccepted` lasted microseconds, there was no pending
  list, no worker pool and no scheduler anywhere in `internal/fleetnode/`. The consequence is
  the part that matters: `fleet_max_queue_depth` (default 32) did not bound a backlog, it
  bounded **simultaneously executing jobs**. A node reporting "depth 31" was running 31 jobs
  at once against ONE llama-swap endpoint, and `QueueDepth()` — counting `accepted || running`
  — was really reporting "in flight".
- **Admit-then-schedule.** `Accept` now admits: it writes the record and appends the id to a
  FIFO. One scheduler goroutine claims a job only while an execution slot is free, and
  dequeue plus `accepted → running` happen in a **single critical section**. `accepted` is a
  real state a job can sit in. Everything a poller can observe is otherwise unchanged — same
  ids, same idempotent re-ack and `409`-on-failed asymmetry, same write-once terminal states,
  same TTL janitor — except that `accepted` may now legitimately last a while.
- **Two independent limits**, both configurable, both taking `0` = built-in default and a
  negative value = unlimited (one rule for both, matching the existing `FleetQueueLimit`
  convention):
  - `fleet_max_queue_depth` (32) — the admission ceiling on `accepted + running`. The only
    thing that produces `503 queue full`.
  - `fleet_max_concurrent_jobs` (**new**, 4) — how many admitted jobs execute at once. Never
    refuses anything; extra jobs wait. This is what protects the shared endpoint.
- **A busy node is no longer a full node.** With the defaults, the 5th dispatch is accepted
  and queued; only the 33rd is refused.

### Changed — the config-key decision, and why `fleet_max_queue_depth` was NOT redefined
- A config key that quietly changes meaning between versions is worse than an awkward name,
  so the existing key keeps the meaning its own name, its own doc comment and the health
  field it caps all already stated: **the ceiling on `queue_depth`, which counts
  `accepted + running`**. That sentence was true in 0.99.0 and is true now. What changed is
  what it no longer *also* does — before this release it doubled as the concurrency limit,
  because with no waiting state "accepted+running" and "executing right now" were the same
  number. Concurrency now has its own key.
- The alternative — keeping the old key as the *concurrency* limit and adding a new key for
  the backlog — was rejected because it would break the one invariant that makes the pair
  legible: `fleet_max_queue_depth` would no longer bound `queue_depth`. Two names sharing a
  word would have meant two different things, on the same payload.
- **Migration is a no-op, and provably safe in the conservative direction.** The refusal
  boundary is byte-identical: the same expression over the same `QueueDepth()` against the
  same key. And under *any* existing setting, actual concurrency after this release is **less
  than or equal to** what that setting produced before it (`fleet_max_queue_depth: 2` ran at
  most 2 concurrently and still does; the default 32 now runs at most 4). No node starts doing
  more at once than it did in 0.99.0, and nothing that was admitted before is refused now.
- **OPERATORS WILL SEE FEWER CONCURRENT JOBS PER NODE, and that is the point.** Stated plainly
  because it is a real throughput change, not a footnote: before this release a node ran up to
  `fleet_max_queue_depth` jobs simultaneously — 32 by default. It now runs 4. That is an **8x
  reduction in node parallelism** at the default settings, and anyone who tuned
  `fleet_max_queue_depth` upward expecting it to buy parallelism will get backlog instead.
- Why 4, rather than leaving it to be discovered: it is the concurrency this fleet already
  operates at — the delegator dispatches a fan-out four subtasks at a time — so it preserves the
  observed steady state rather than inventing a new one. And the parallelism being removed was
  mostly fictitious: a node fronts ONE llama-swap, which serializes model residency, so the 5th
  through 32nd concurrent jobs were manufacturing contention (swap thrash, VRAM pressure, longer
  wall clock for every job in the set) rather than doing more work. A job that waits is strictly
  better than a job that thrashes, and far better than one that is refused. Operators whose box
  genuinely runs more at once should set `fleet_max_concurrent_jobs` explicitly — it is now the
  key that means what they want, which was never true of the one they were reaching for before.

### Added — `/fleet/health` publishes capacity, not just depth
- `queue_depth` keeps its **exact** meaning and shape for existing readers (the delegator's
  placement tie-break at `internal/delegate/gate.go`, and the media dispatcher in its own
  repo). It is now computed as `jobs_queued + jobs_running` from one walk of the store, so the
  three numbers cannot disagree.
- Added alongside it: `jobs_running`, `jobs_queued`, `max_concurrent_jobs`, `max_queue_depth`.
  A delegator could previously see how loaded a node was but never how much it could take.
- These four are **always present, never `omitempty`**: `0` is a meaningful value for each —
  and for the two limits it means *unlimited* — so omitting it would make "unlimited"
  indistinguishable from "a node too old to publish a limit", and those route in opposite
  directions. Wire compatibility is pinned by an external-package test that runs the
  delegator's real `FetchNodeView` decoder against the real health handler.
- `offload_status` publishes `jobs_running` / `jobs_queued` / `max_concurrent_jobs` per node
  alongside `queue_depth` — the exact surface a `queue deadline` failure sends an operator to.
- **`queue_depth`'s meaning is unchanged but its DISTRIBUTION shifts sharply.** It always counted
  `accepted` + `running`; before this release those were all executing, so the number topped out
  near what a box could sustain and a high reading was a real alarm. Now most of it can be
  backlog, and a healthy node can legitimately read 31. Placement is unaffected (lower is still
  better, and the tie-break compares like with like), but an operator meeting it cold will
  misjudge it — read `jobs_running` / `jobs_queued` beside it.

### Changed — drain tells "never started" from "interrupted"
- `DrainAndStop` marked every non-terminal survivor `error:"interrupted"`. With a real backlog
  that would have the node claim it began work it never touched. Survivors are now marked by
  state — which the scheduler's atomic claim makes trustworthy: `running` → `"interrupted"`,
  still `accepted` → `"not started: node shut down while this job was queued"`.
- Drain **finishes what it started and does not start what it never began**: the backlog is
  dropped when drain begins rather than worked through, so shutdown never launches an
  inference it cannot finish.
- **`TestJobsDrainWaitsForFastJobs` was adjusted, and here is exactly what changed.** Before: it
  called `Accept` with a 30 ms run and immediately called `DrainAndStop(5s)`, asserting the job
  ended `done` with its data. After: it waits for the job to reach `running` (via a channel the
  run closes plus `waitJobState`) and only then drains, asserting the same `done` + data. Nothing
  else about the assertion changed. The reason the old form had to go is that it silently relied
  on `Accept` being `start` — with admission and execution separated, `Accept` and `DrainAndStop`
  contend for the same mutex and neither ordering is guaranteed, so "was this job claimed before
  drain began?" became a genuine coin flip *in production as much as in the test*: the same code
  would legitimately produce `done` sometimes and `not started` other times. Keeping the old
  assertion would have made the test flaky rather than made the behaviour deterministic. The
  test's intent — a job that completes inside the drain timeout drains clean rather than being
  marked `interrupted` — is preserved exactly; what it no longer *incidentally* covers is "a
  just-accepted job gets started", which was an artifact of the old design and is now covered
  deliberately (and correctly) by the queue tests instead.

### Fixed — docs asserted a load-shedding behaviour this repo does not have
- `docs/FLEET-NODE.md` claimed "Any non-202 already means 're-dispatch elsewhere' to the
  dispatcher, so a full node sheds load to its siblings with no dispatcher change." That is
  **false here**. `internal/delegate/run.go` is the only POST to `/fleet/dispatch` in this
  repo and it surfaces any non-`202` as an error without re-placing the subtask: the work
  simply does not get done. The described behaviour belongs to the **media dispatcher, which
  lives in a different repository**. The claim is corrected (not fixed — delegator-side
  re-placement is a separate change) in `docs/FLEET-NODE.md`, `docs/systems/fleet-node.md`,
  `docs/flows/fleet-job-lifecycle.md` and the same sentence copied into `server.go`'s
  admission comment.

### Fixed — a job that never started leaked its materialized request
- `handleDispatch` parks its temp-file cleanup in a `defer` **inside** the run closure. That was
  airtight while `Accept` was `start` — every admitted job's closure ran, so every
  materialization was released. Once a job can be admitted and then marked terminal without its
  closure ever executing (drain's never-started arm), the deferred cleanup never fires.
- Scope, checked per builder: `run-graph` materializes `os.CreateTemp("", "fleet-run-graph-*.json")`
  in the **OS temp dir, which nothing ever reclaims**; `agent` and pipeline jobs land under
  `pipeline-jobs/`, which `SweepOrphanedPipelineJobs` wipes at next start. With this release's own
  worked example of 28 queued jobs at shutdown, that is 28 skipped cleanups.
- Fixed with `AcceptSpec.OnDropped`, carried on the record beside `run`, **cleared at claim** (the
  run's own defer owns cleanup from then on, and firing both would double-free) and invoked by
  drain's never-started arm — outside the store mutex, since it touches the filesystem. It is
  deliberately NOT fired when admission is REFUSED: a refusal never took ownership, and
  `handleDispatch` already releases its own materialization there.

### Fixed — queued time no longer consumes a subtask's execution budget (delegator side)
- **This regression was caused by the queue above and is fixed in the same release.** The
  delegator sets its poll deadline at DISPATCH time (`pollBudget = timeout_sec + 60s grace`).
  That was harmless while `Accept` was `start` — dispatch → running was microseconds, so the whole
  budget went to execution by construction. With a real backlog, a job waiting for a slot spent
  the contract's own timeout doing nothing, which converted a node's **loud, immediate
  `503 queue full`** into a **slow timeout that burned the budget and then manufactured a defer**
  ("node accepted the job but did not reach a terminal state", stamped with that node's id and
  seat). Same lost work, less signal, more wall clock — strictly worse than the refusal it
  replaced.
- **Queued time is credited back.** While a job is observed in `accepted`, the delegator moves the
  deadline out by the wait, so the contract still gets the execution budget it asked for. Only
  intervals bracketed by two CONSECUTIVE `accepted` observations are banked — the partial spans
  either side are at most one poll each, and under-crediting is the safe direction (over-crediting
  would hand a job more budget than the contract granted, which is the same class of defect in the
  other direction). A node with no queue accrues no credit and behaves byte-identically.
- **The "consecutive" part is a fix in its own right.** The first implementation cleared its span
  marker only in the `running` arm, so the transport-error, `404` and `5xx` arms all left it open:
  an interval bracketed by two `accepted` answers was banked IN FULL even when the node spent the
  middle of it returning 503s — or denying, with a `404`, that it had ever held the job. Measured:
  a node scripted `accepted` / 503 for 500 ms / `accepted` credited the whole 500 ms as backlog
  wait. Both harms point the wrong way — more execution budget than the contract granted, and a
  give-up message *asserting* the job "waited in the node's backlog" across a window the node
  spent failing. Every observation now closes the span and only `accepted` re-opens it, so a
  future poll arm cannot silently inherit an open one.
- **The wait is bounded**: `min(timeout_sec + grace, 5 minutes)`. An unbounded wait is its own
  failure mode. A subtask that has not been given a worker in five minutes is behind a backlog no
  fan-out will clear in time, and the caller is better served by a loud failure than a longer wait
  it did not ask for.
- **Giving up while QUEUED is distinguishable from giving up while RUNNING.** A never-started job
  yields a FAILURE with the stable prefix `queue deadline after <d>: the node accepted the job but
  never started it — it waited in the node's backlog and never reached running (<n> poll(s)
  answered `accepted`)`. The count is of polls that ACTUALLY answered `accepted`, not the total —
  a run that also saw 404s must not have them folded into a claim about queueing. Deliberately a failure and not a defer, for three reasons — and note that
  "a defer carries the node id and seat" is **not** one of them, since the failure path is stamped
  with those too: (1) a defer manufactures an `AgentWireResult{Deferred:true}`, a payload shaped
  like something the *seat* produced, and no seat ever saw this contract; (2) the only honest class
  would be `budget`, which teaches every consumer of that class — the sizing path especially —
  that the seat needed more time, when the seat was never asked; and (3) decisively, **what this
  replaces was already a failure** (a saturated node answered `503` at dispatch → `pr.Err` →
  `summary.failed`), so filing it as a defer would quietly downgrade the severity of a refusal
  while changing nothing about the work not getting done.
  A job that reached `running` still produces the existing `poll deadline` defer, unchanged.
- Deadline messages now name the credit (`poll deadline after 5m0s (+42s credited back for time
  queued on the node): …`) and render nothing at all when there was none, so every pre-queue
  node's message — and the tests pinning that wording — stay byte-identical.
- Scope note: this is the budget-accounting half only. **Re-placement on refusal and
  capacity-aware placement are still a separate change** — a `503 queue full` remains terminal.

### Changed — the concurrency cap governs the TEXT lane only; media is exempt
- **An earlier draft of this entry claimed "the media/`gpulease` path is untouched". That was
  false.** `handleDispatch` routes every task type through one store, so media, video, STT,
  run-graph and pipeline dispatches were admitted, queued and capped exactly like agent jobs —
  the `gpulease` *code* was untouched, but media *scheduling* took the same 8x cut. Corrected
  here rather than quietly dropped, because a media operator reading the old sentence would
  have had no reason to look.
- **DECISION: `fleet_max_concurrent_jobs` applies to `agent` only.** `image-gen`, `video-gen`,
  `audio-gen`, `run-graph`, `stt` and every configured pipeline route are exempt. An
  unrecognized (future) task type is **capped by default** — of the two ways to be wrong, that
  one fails loudly (queue latency, then a visible `queue deadline`) while the other silently
  reinstates the unbounded-inference defect this release exists to remove.
- Why exempt, in order of weight:
  1. **Redundant.** Media goes through `Pipeline.acquireMediaLease`, which takes the in-process
     `mediaSlot` — capacity **one** — and then the machine-wide `gpulease` ClassMedia. A cap of
     4 above a cap of 1 changes nothing about how much media runs at once.
  2. **Actively harmful, and precisely backwards.** A media job blocked inside `takeMediaSlot`
     holds a fleet execution slot while doing NO work. With `mediaSlot` at one, four queued
     media dispatches occupy all four slots while three sit parked — starving the agent lane,
     which is the lane the cap was written to protect.
  3. **It destroys media's own back-pressure.** A media job that cannot get the card waits
     `gpu_wait_ms` and defers `gpu_busy` — bounded, well-tested, and immediately actionable.
     Held in `accepted` it never reaches `takeMediaSlot`, so `gpu_busy` never fires and the
     caller gets queue latency instead of a fast, honest defer.
  4. **It makes "media untouched" true again.** With the exemption, media scheduling is
     byte-identical to 0.99.0: admitted up to `fleet_max_queue_depth`, goroutine-per-job,
     arbitrated by `mediaSlot` + `gpulease`.
- The store keeps arrival order **within** a lane but scans past a blocked entry across lanes,
  so an uncapped media job never queues behind four running agent jobs — otherwise FIFO
  position would reinstate the coupling the exemption removes.

### DEPLOY NOTE — the out-of-repo media dispatcher
- The deployed media dispatcher (0.62.1, a **different repository**) is an ack-then-poll client
  that sets its poll deadline at dispatch time, exactly as `internal/delegate` did before the
  fix below. It has no queued-time credit and cannot get one from this repo.
- **The media exemption is what keeps it correct**, not a documentation note: media jobs never
  linger in `accepted`, so that dispatcher never spends a contract's budget waiting for a slot.
- **Therefore: do not cap a media task type until that dispatcher credits queued time.** If a
  future change adds a media type to the capped set, it inherits precisely the regression that
  made this merge-blocking here — a loud immediate `503` replaced by a slow timeout that burns
  the budget — with no fix available from this side. `Server.concurrencyCapped` carries the
  rule; this is the reason behind it.

### Notes
- No durability was added: the store stays in-memory, and a node restart still loses its
  backlog (now reported honestly as never-started). No pull/claim endpoint — placement stays
  PUSH. The `gpulease` / reclaim code is untouched, and with the exemption above media
  SCHEDULING is unchanged from 0.99.0 too.
- `-race` could not be run: it requires cgo and this box has no C toolchain
  (`go: -race requires cgo`). Compensated with single-mutex lock discipline (one `sync.Cond`
  over the same mutex that guards the store, so deciding and claiming are one critical
  section), a 24-job concurrent burst test sampling peak overlap from inside the runs, the
  pre-existing 32-goroutine concurrent-`Accept` test now running through the scheduler,
  repeated `-count` runs, and `go vet ./...`. The delegator-side change adds no shared mutable
  state (the queued accumulator is local to one `runRemote` call, on the goroutine that owns it).

## [0.99.0] - 2026-08-26

### Changed — `route:"spread"`: the remote slots are fit-scored instead of dealt blind
- `placeSpread` dealt every slot by `k := i % len(nodes)` and nothing else. Across heterogeneous
  seats that is a coin flip on the thing that matters: a mechanical triage contract and a
  cross-file reasoning contract had exactly the same chance of landing on the biggest seat.
  The remote slots now go to the seat that suits the contract's **shape**, inferred from the
  contract's own goal text — chosen from the seats not yet dealt in the current cycle, so the
  fan-out itself is untouched.
- **Shape is a deterministic ORDERED pre-filter (`internal/delegate/fit.go`), not a classifier
  call.** `mechanical` (extraction, listing, counting, filtering, digesting) or `reasoning`
  (explanation, causation, cross-file interaction, tracing, comparison), decided by a quantity
  rule, then an explanation rule, then a mechanical-verb rule. Asking a model which seat to use
  would spend a round-trip per subtask to save one — a net loss on exactly the cheap contracts
  this lane exists for — so nothing here leaves the process, and a placement is reproducible from
  the recorded contract alone.
- **Rule ORDER is the fix, not a cleverer pattern.** A bare `how ` alternative inside the
  explanation pattern reads "how many files changed" as reasoning — a trivial count sent to the
  expensive seat. RE2 has no lookahead, so "how, but not how many" cannot be written as one
  expression; the quantity rule runs first and takes every quantity phrasing off the table, which
  is what leaves the bare `how ` alternative safe for genuine how-questions. Pinned by a
  table-driven routing test over realistic goal strings.
- **An unmatched goal is `mechanical` — the CHEAP seat.** A harness whose purpose is moving grunt
  work off the expensive seat must let ambiguity fall toward cheap, never toward capable:
  defaulting the unmatched case to the big seat silently rebuilds the round-robin problem for
  every goal the vocabulary does not cover. A wrong cheap placement costs a retry, which the
  engine already runs on a different seat; a wrong expensive placement costs the capable seat,
  which is the whole resource being protected. The unmatched branch is reported honestly
  (`matchShape` returns `ok=false`) and is the single seam a better fallback would plug into.
- **"Smallest adequate seat" is now arithmetic, not a phrase.** `adequate()` is
  `est_tokens + specReserve <= agent_ctx_tokens` — the same check the hard gate makes, extracted
  so `remoteEligible` and `scoreFit` share ONE definition of "fits" and cannot drift. Reasoning
  takes the roomiest adequate seat; mechanical takes the smallest adequate one. An unadvertised
  ceiling is never adequate: unknown is not a capacity, and a seat that published no number must
  not win the mechanical contest by looking like the smallest on the roster.
- **Unchanged, deliberately: subtask 0 still lands local**, whatever its shape — and so does every
  later local rotation slot. A single-subtask spread is the riskiest case for a shape heuristic,
  and one regex match must not be able to send an entire run off-box. The local seat also
  advertises no context ceiling in a delegator run, so scoring it would mean inventing a number
  for it. The documented pair shape (a 2-contract spread = one local + one remote) therefore
  still holds, and the existing spread distribution test passes unchanged.
- **Fit chooses WITHIN a deal cycle; it never re-picks freely.** A subtask takes the best-fitting
  seat *among those not yet dealt in the current cycle*, and a local slot reshuffles the deck, so
  each aligned window of `len(nodes)` slots still gives every eligible seat at most one subtask.
  This is not a refinement, it is the whole safety property: an unconstrained re-pick sends the
  smallest seat every mechanical subtask and the roomiest every reasoning one. Measured on a
  `{local, qube 131k, aorus 32k, lenovo 32k}` roster — 8 mechanical subtasks landed
  `local 2 / aorus 4 / lenovo 2 / qube 0` and 8 reasoning subtasks landed `local 2 / qube 6`, with
  `runConcurrency = 4` dispatching siblings to the same seat while another sat idle. That is the
  stacking `route:"spread"` was built to remove. Constrained, both deal `2/2/2/2`, mechanical
  dispatching the small seats first and reasoning the roomiest first.
- **The deal is JOINT — computed for the whole run in one pass before dispatch — because it cannot
  be anything else.** No per-subtask function of (index, own shape, roster) can hold the invariant:
  distinctness within a cycle forces the slot-to-seat map to be a bijection for each shape, and
  distinctness within a MIXED-shape cycle then forces both shapes' bijections to be identical —
  which is to say, forces the shape to have no effect at all. Fit scoring and one-per-seat coexist
  only when the deal can see its siblings. Computing it up front also keeps placement deterministic
  and free of shared mutable state: the dispatch goroutines read the deal, they never build it.
- **Ties keep rotating** (the comparison is strict), so an all-equal roster deals exactly as it did
  before fit scoring existed.
- `results[].placement` now names the deciding RULE as well as the shape
  (`route=spread → node-a (slot 2 of 4, fit=mechanical/mechanical-verb)`). `slot N of M` remains the
  rotation slot, not the winner's roster index — an operator reading placements counts subtasks —
  and `fit=mechanical/default` versus `fit=mechanical/mechanical-verb` is the difference between
  "rephrase the goal" and "the vocabulary read it correctly". The local slot's reason is
  byte-identical to before.
- **Mutation-proven, twice.** (a) Make `placeSpread` ignore the fit score — a compiling mutant that
  leaves the rotation's pick in place — and the placement test goes red in both directions, the
  mechanical contract landing on the big seat and the reasoning one on the small seat. (b) Drop the
  cycle bookkeeping so the fit score re-picks freely, and the fan-out test goes red reproducing the
  collapse above exactly (`qube 0` under mechanical, `qube 6` under reasoning). The fan-out test
  uses an UNEQUAL-ceiling roster on purpose: with equal ceilings every implementation passes, which
  is how such a collapse ships green.

## [0.98.0] - 2026-08-26

### Added — `offload_ask` result cache: an identical repeat stops costing a seat run
- An IDENTICAL `offload_ask` call — same question, same `read_root`, and the same file **bytes**
  — now returns the answer the seat already produced instead of running the seat again, and the
  response says which it was via a new `cache_hit` field. The repeat returns without touching
  the seat at all — pinned by the wired front-door test (`TestAskSecondIdenticalCallSkipsTheSeatAndSaysSo`,
  which asserts the seat ran exactly once across two identical calls) plus the two mutation
  proofs below, not by a timed measurement: there is no run log to attribute a number to, and
  the only number ever in hand was a cold-swap-degraded figure (llama-swap loading a different
  model into the seat's slot mid-measurement) that does not belong here.
- **The limitation, stated plainly, because it is easy to oversell.** This pays on an EXACT
  repeat and on nothing else. A *different* question over the same files still pays full seat
  time (46–75 s measured), because the seat has to reason about the new question. The only
  mechanism that would fix that is keeping a model context resident between calls, which
  requires llama-swap slot pinning — trading a seat's availability for cache warmth, and
  explicitly declined by the operator. **Nothing here is a general speedup**, and the tool
  description says so at the decision point rather than only here.
- **Keyed on CONTENT, never on path**, and that is the entire safety argument. The key covers the
  question, the resolved `read_root`, and each resolved doc's name plus the SHA-256 of its bytes,
  so a file edited between two otherwise-identical calls is a *different key* and the seat runs
  again. A stale answer is not merely unlikely, it is unreachable. Mutation-proven: key on the
  path instead and both the unit test and the wired front-door test go red — the live e2e shows
  the same thing, an edited file going straight back to the seat.
- **Wired, not decorative.** The cache is consulted inside `handleAsk` and a hit is observable in
  the response; a result cache that nothing calls is dead code with a test suite. The lookup sits
  *after* `askjob.BuildContract`, because the key IS the resolved file content and `BuildContract`
  is what resolves it — which also means every refusal (no anchor, over a cap, outside
  `read_root`) still happens on every call. Only a finished answer is short-circuited.
- **Only successful, non-deferred results are stored.** A defer, a refusal or a runner error is a
  statement about this minute, not about these files; caching one would turn a transient seat
  failure into a lane that stays dead for the rest of the connection. A deferred result therefore
  carries no `cache_hit` at all — by design, not omission: it is always a fresh run. A
  `verified: false` answer IS cached (the seat ran and answered; only the citation check missed),
  so an identical repeat returns the same unverified answer rather than re-rolling the seat.
- **Bounded at 32 entries, oldest out, scoped to the process.** The MCP server is spawned per
  client over stdio, so one connection is one process is one cache — born and destroyed with the
  connection. That is why there is **no `session_id` argument**: it would be a second, weaker
  spelling of a boundary the process already draws exactly, and adding a required input to the
  one-call tool would undercut the friction removal `offload_ask` exists for.
- New package `internal/askcache`, with a concurrent Put/Get/Len test (`TestConcurrentUseIsSafe`)
  guarding the mutex around every map access. `-race` was **not** run against it — this build
  box has no cgo toolchain (`CGO_ENABLED=1 go test -race` fails with `C compiler "gcc" not
  found`) — so the mutex rests on code inspection plus Go's runtime concurrent-map-write
  detector, not on an instrumented run.

## [0.97.0] - 2026-08-26

### Added — `offload_review_diff`: the clean-context review lane
- Every other lane in this harness competes with "read the file yourself" on cost, and that
  competition is measurably lost: organic `agent_delegate` adoption is ~0 and three rounds of
  steering pressure moved it not at all. This lane is different in kind. It offers something a
  lead agent **cannot produce from inside its own context at all** — a reviewer that never saw
  the work. You pass a diff and a task statement; a free local seat returns severity-ranked
  findings with file/line/claim/why.
- **The isolation is the mechanism, not a side effect.** A reviewer that has not accumulated the
  author's context catches defects the author's own judgement has stopped seeing. The supporting
  evidence is worth separating carefully. Cognition **reports** a dedicated reviewer in their
  Fusion setup catching ~2 bugs per PR, ~58% of them severe — that is the vendor's own published
  figure, with no sample size, no A/B baseline and no external audit, so it is cited here as a
  claim and nothing more. What IS independently supported is the underlying effect: long-context
  degradation, measured across 18 SOTA models by Chroma's context-rot study and by Stanford's
  lost-in-the-middle work. The lane rests on the mechanism, not on the vendor's number.
- **Advisory, and the tool description says so in as many words.** It never gates a merge and
  never substitutes for the final does-it-actually-work verification, which stays with the lead —
  as do security review, architecture judgement, and any call the lead is accountable for. The
  findings are TRIAGE INPUT: a `severe` label from a small local model is a prompt to go read
  those lines, never proof, and nothing is ever applied unread.
- Design decisions worth knowing, each forced by something measurable:
  - **The diff rides in the GOAL, not in a context doc.** A context doc becomes a file the seat
    must find with `list_dir` and open with `read_file`, and the measured failure mode of a small
    planner is calling no tool at all — which would produce confident findings about a diff never
    read. The cost is that `core.AgentContract.Validate`'s 256 KiB context cap never sees the
    diff, so `internal/reviewlane` owns that bound itself (`MaxDiffBytes`) and refuses early with
    the real numbers instead of overflowing a seat's window.
  - **No acceptance check, deliberately.** An empty findings list is a CORRECT outcome here, so
    any content check would either punish a clean diff or pass anything — exactly the decorative
    acceptance `delegate.LintAcceptance` exists to name. What replaces it is a check the harness
    can actually make: a finding naming a file the diff never touched is dropped and REPORTED as
    `dropped_ungrounded`, because an invented path is how a small seat fails here.
  - **A findings array of strings, parsed delegator-side.** `gbnf.FromJSONSchema` compiles any
    array to an array of strings, so a schema of `{severity,file,line,...}` objects would have
    become strings anyway. The prompt asks for one `severity | file:line | claim | why` line per
    defect and `ParseFindings` reads them back tolerantly — keeping anything it cannot parse as
    an unranked claim rather than dropping it, because a discarded line turns a reviewer that did
    work into a clean bill of health nobody issued.
  - **An empty findings list is never published unless the seat EARNED it.** "No findings" is
    the one result a lead might read as reassurance, so it must never be what a broken run
    collapses into — and a broken run reaches exactly that shape. Traced: `agent/loop.go` returns
    `stop_reason:"done"` the moment the model stops requesting tools, with no check that the
    final message carries content (empty content is live-measured in this codebase — see the
    re-pack's own comment on a GBNF + thinking seat stranding its answer in
    `reasoning_content`); `agenttask.go` special-cases only `"budget"`, so `"done"` with an
    empty `Output` reaches `repackStructured`, which extracts findings from an empty string and
    returns a schema-valid `{"findings":[]}`. `steps:1` and `stop_reason:"done"` describe both
    cases; `wire.Output` was the one field that differed and it was being discarded. So a
    zero-finding result is now cross-checked against the seat's own raw answer for the explicit
    `NONE` verdict the prompt already asks for, and defers with a distinct reason when it is
    absent. This checks for a signal, not for quality — no judgement is made about findings.
    When the list IS genuinely empty, the response says in words that it is not a verification.
    The gate requires an AFFIRMATIVE all-clear — the bare `NONE` token alone on a line, or an
    explicit no-defects statement — and never a length test. Its first version accepted any
    answer of 16+ characters, and **the lane caught that itself**: run live against this very
    diff, the seat reported that `"I could not read the diff"` (25 characters, a seat reporting
    FAILURE) would have published an empty findings list instead of deferring, and that a bare
    `none` match would read `"I tried but none of the tools worked"` as clean. Both verified
    by running the function on those exact strings, both now pinned as not-clean.
  - **Three counts, published on the same terms**, because a short findings list is the shape a
    reader most easily misreads and each has a different meaning: `dropped_ungrounded` (named a
    file the diff never touched), `dropped_echo` (handed the prompt's own template back), and
    `truncated_by_cap` (more was found than `max_findings` asked to see). Counting drops loudly
    while truncating silently just moved the blind spot. The `note` on an empty list is now
    gated on those counts too: "found nothing" printed beside a non-zero drop count was simply
    false — the reviewer found things and the harness discarded them.
  - **The worked example cannot be republished as a finding.** It is parseable and grounds
    against any diff touching a file of that base name, and echoing it is MEASURED behaviour of
    this seat. Choosing a neutral defect class for the example makes an echo distinguishable to
    a person; it does nothing for the harness. `dropTemplateEchoes` drops any line
    byte-identical (after the parser's own normalisation) to the field spec or the example, and
    counts it. Byte equality, never resemblance — a finding that merely resembles the example is
    a finding.
  - **An unrecognised severity label no longer shreds the line.** "critical", "high", "blocker",
    "P0" — small seats drift to these routinely. The label failed the known-severity test, then
    failed the path test, and so became the *claim*, with the real claim, path and why rejoined
    into `why`. `File` came out empty, so grounding skipped the wreckage and it reached the
    caller UNCOUNTED, looking like an ordinary finding. Now an unknown label in the severity
    slot (recognised by the NEXT field being path-shaped) is kept as an unranked severity and
    the rest parses normally — so an invented path riding with it is visible to grounding again.
  - **The format spec is restated AFTER the diff.** At `MaxDiffBytes` the first statement sits a
    quarter of a megabyte from the point where it must be applied. Resting the whole design on
    lost-in-the-middle and then burying the one instruction that has already failed live at the
    far end of the context would be incoherent.
  - **The format spec carries a filled-in example**, found the only way it could be: a live run.
    With an abstract `severity | file:line | claim | why` template the 27B seat located both
    planted defects and then wrote back `severe | file:16` — it had copied the placeholder name
    instead of the path, and lost claim and why entirely. An angle-bracketed spec plus one worked
    example (of a DIFFERENT defect class, so an echo of it is distinguishable from a real finding)
    produced fully-formed findings on the same diff. Seven passing unit tests did not and could
    not catch this.
- Registered unconditionally beside `offload_ask`; runs on the local seat through
  `Pipeline.RunAgentContract`, the same entry a local delegation placement takes. Like every
  other tool here, any failure comes back as `deferred: true` with a reason, never an MCP error.

## [0.96.0] - 2026-08-25

### Added — `offload_ask`: the one-call delegation entry point
- Measured organic adoption of `agent_delegate` is ~0, and three rounds of steering pressure
  (prose rules, a nudge hook, a blocking gate) moved it not at all. The diagnosis is
  arithmetic, not discipline: at the moment of deciding, authoring a contract (goal +
  context + output schema + a non-parrot acceptance check) costs far more than opening the
  files and reading them. `offload_ask` removes that cost — **question + paths in,
  `{answer, evidence}` out** — with the harness (`internal/askjob`) authoring the entire
  contract and running it on the local agent seat through `Pipeline.RunAgentContract`, the
  same entry a local delegation placement takes. Registered unconditionally, beside
  `agent_run`: a cheap path behind a config flag is one more reason not to take it.
- The hard part is the acceptance check, and it is the whole of this package. A caller-free
  check must still be GROUNDED — anchored to content appearing only in the supplied files —
  or it is exactly the PARROT-PASSABLE / UNGROUNDED pathology `delegate.LintAcceptance`
  exists to catch, and the answer reads as verified while nothing verified it. So the anchor
  is mined from the files: the tokens that appear NOWHERE in the built goal, MOST FREQUENT
  first, rendered as one `regex:(A|B|C)` alternation over the top three. Three corrections
  proved out while building it, each from a measurement rather than a preference:
  (1) the disqualifier is the full GOAL, not the question — the goal's own boilerplate
  carries ≥8-character words (`QUESTION`, `attached`, `inferring`) and the lint measures
  against `c.Goal`, so a question-only exclusion trips the very lint the feature exists to
  satisfy; (2) identifier-shaped tokens (underscore, digit, or internal capital) fill
  the slots first, because ranking alone picked `delegate` out of a code comment — ordinary
  words top up whatever slots are left over (see below), so attaching markdown or a log
  still works; (3) the ranking is
  MOST-frequent, not rarest. Rarest-wins was the original rule and measurement contradicted
  it three independent times, most starkly on this package's own fixture, where the pool was
  `{ErrQueueSaturated:3, defaultMaxQueueDepth:3, dispatchRetryBackoff:1}` and rarity picked
  the retry-backoff constant while the two tokens a right answer must quote both LOST for
  being more frequent. Within one file centrality and frequency correlate — a name the file
  repeats is a name the file is ABOUT — so rarity ranks away from what an answer will cite,
  and with no cross-seat retry on this lane a false `verified:false` is terminal. The
  alternation is the safety net: a right answer passes if ANY of the three appears. When
  nothing survives, `BuildContract` REFUSES (`ErrNoAnchor`) rather than emit a check that
  would pass garbage. Pinned by `LintAcceptance(BuildContract(...))` returning zero warnings
  on a realistic source file, and by a check that a right answer PASSES while the goal text
  itself does not.
- The verdict grades the text the caller RECEIVES. `core.evalText` prefers `wire.Output`
  whenever it is non-empty and `runAgentTask` always sets it before the re-pack, so grading
  the wire as it arrives would grade the loop's final prose — which this lane never
  publishes, since the caller gets only the condensed `{answer, evidence}` pair. The
  divergence is one-directional (prose is longer, so likelier to contain a frequent token),
  which made the error mode `verified:true` beside a published answer citing nothing from
  the files: the "reads as verified while nothing verified it" pathology, one layer up from
  where the anchor design closes it. `handleAsk` now blanks `Output` when `Structured` is
  present so `evalText` falls through to the bytes that actually ship. `delegate.Run` grades
  prose and publishes prose, so it is coherent and untouched — this is the first lane that
  publishes only the structured pair. Grading the raw JSON cannot self-match on its own
  field names: `answer` is 6 characters (under the 8-character anchor bound) and `evidence`
  is 8 but appears in the goal.
- Anchor candidates are bounded at 40 characters (`anchorMaxLen`). `identRe` has no upper
  bound and identifier-shaped says yes to anything carrying a digit, so a lockfile hash,
  checksum table or minified bundle could seat a 40-to-500-character blob as a "name" — an
  acceptance check no answer could cite, and an absurd `acceptance` field. Reachable from an
  ordinary input (a config directory holding a lockfile).
- The identifier-shaped tier TOPS UP instead of REPLACING. Shaped tokens fill the three
  alternation slots first; any spare slots go to the best plain tokens rather than being
  left empty. Those passed the same goal exclusion, and a spare alternative is one more
  branch of an OR, and that cuts BOTH ways: it eases passing for a citing answer AND for
  an uncited one, so a plain token must clear a higher bar than an identifier —
  `plainAnchorMinLen` (12 characters), or it must NAME one of the attached files. Measured
  without that bar, the check became `regex:(FleetMaxQueueDepth|accepted)`, and "accepted"
  is ordinary English that a wrong answer contains for free. Replacing outright was
  silently dropping good candidates: `buildinfo` is nine characters and not question-named,
  yet never reached the pool on the live run purely because that file had shaped tokens.
- Known limitation, stated rather than hidden: a question whose answer turns on a SHORT
  (<8-character) or question-named identifier can leave nothing anchorable — `verified` then
  reads false on a correct answer. `verified` is a CITATION check, not a correctness verdict:
  it asks whether the published answer quoted one of a few distinctive tokens taken from the
  files, never whether the answer is right — and the graded text is built from the decoded
  `answer` + `evidence` the caller is SHOWN, so what is graded is exactly what is published.
- The ask lane writes **no delegation ledger or corpus row**: `delegate.Run` records one per
  subtask, `Pipeline.RunAgentContract` on its own does not. The pipeline's own task ledger
  still sees the run. Recorded so an empty delegation corpus is never misread as "nobody used
  the tool".
- Contract hygiene the caller no longer has to think about: files are inlined through the
  delegator's one confined reader (`delegate.InlineContextPaths` — `os.Root` containment,
  128 KiB per file), duplicate base names are de-collided deterministically
  (`config.go` + `config.go` → `config.go` + `config-2.go`) instead of hitting `Validate`'s
  silent-overwrite refusal, and the 16-doc / 256 KiB wire caps are refused UP FRONT with
  typed errors naming the numbers and the fix (`ErrTooManyPaths`, `ErrContextTooLarge`).
  `profile` is deliberately left empty so the executing box's `agent_profile` decides.
- The generated acceptance is EVALUATED, not decorative: this lane runs one local seat
  rather than going through `delegate.Run`, so the handler evaluates it and publishes
  `verified` plus `acceptance` and `acceptance_failures`. `delegate.evalAcceptance` and
  `delegate.inlineContextPaths` are exported for this (`EvalAcceptance`,
  `InlineContextPaths`) — visibility only, no behavior change, so the two lanes cannot drift
  into two sets of rules.

## [0.95.0] - 2026-08-25

### Added — `offload_status` publishes the LIVE fleet roster (`fleet` section)
- Capability was asserted in documents and the documents went stale in a way that silently
  suppressed delegation: three independent sources (a rules file, the tier matrix, and the
  config the MCP loads) each described a live, resident 9B agent seat as absent or
  half-capable, and no runtime surface could contradict them. `offload_status` now probes
  every configured `delegate_remotes` node concurrently and publishes `node_id`,
  `agent_seat`, `agent_ctx_tokens`, `agent_enabled`, `agent_seat_resident`, `queue_depth`,
  plus `agent_capable_nodes` / `idle_agent_nodes` and the local planner seat. A written
  figure is now always the weaker source. The local-seat probe is NON-loading (the
  `ErrNotLoaded` Props path): a status question must never cost a multi-GB model load. A
  probe failure is reported per node, never a defer — and the node probes hold their own
  timeout budget so a local stall cannot be published as a fleet outage.

### Changed — delegation contracts resolve the per-seat profile (was: hardcoded `research`)
- `RunAgentContract` now resolves contract `profile` > the executing box's configured
  `agent_profile` > `general` — the same `AgentTaskProfile` chain `agent_run` uses. The old
  hardcoded `research` default forced every spread's subtask 0 (always the local seat) into
  the narrowed shape the 2026-08-19 D3 bake measured worse on big planners (27B: general
  100%, research 94% and 5x slower on one shape); small tiers keep `research` via their
  `config_seed`, which is where a per-seat decision belongs. `agent_delegate`'s description
  now also directs callers to size contracts from the live `agent_ctx_tokens` instead of
  remembered figures.

### Added — fleet node queue-depth back-pressure (`fleet_max_queue_depth`)
- `POST /fleet/dispatch` refuses NEW work 503 ("queue full") once accepted+running reaches
  the cap — the dispatch contract already defines any non-202 as "re-dispatch elsewhere",
  so a full node sheds load to siblings with no delegator change. Deliberately generous:
  default 32 (a full `agent_delegate` call is 8 subtasks), configurable, negative =
  unlimited; enforced before request materialization; re-acks of owned jobs and result
  polls are never refused by it. Guards the measured failure mode (unbounded queue LATENCY
  behind the single inference slot), not a crash.

## [0.94.3] - 2026-08-25

### Fixed — a fresh ampere-8 install shipped no image route (seed lacked `imagegen_script`)
- `ampere-8`'s `config_seed_ram_mid_high` bound the HiDream-O1 family/ckpt/vae but not
  `imagegen_script`, and `ImageRouteConfigured()` keys on the script — so a fresh install
  bound a family it could not serve: doctor showed no image route and the fleet node never
  advertised `image-gen`. Found live on the reference box (which had ComfyUI + the bf16
  checkpoint on disk the whole time). The seed now carries
  `imagegen_script: __OFFLOAD_HOME__/render/comfy-generate.mjs`, mirroring blackwell-8.
  Fix verified live: after binding + a process restart the node advertises
  `[image-gen, ...]` / family `hidream-o1`, and a real 1024² HiDream render passed (278s).

### Added — Windows fleet-node launcher template + runbook section
- `setup/templates/start-fleet-node.win.ps1` (tokenized: `__OFFLOAD_HOME__`, `__NODE_ID__`)
  encodes the three launcher facts measured on the ampere-8 box: the Tailscale CLI returns
  nothing under an S4U session-0 task (read `Get-NetIPAddress -InterfaceAlias Tailscale`
  instead); at boot the address exists before it is BINDABLE (readiness = a real TcpListener
  bind, retried); an immediately-exiting child is detected and surfaced (the task's own
  lastResult is a false liveness signal for a detached child).
- `docs/FLEET-NODE.md` gains "Running as a Windows scheduled task": S4U-not-SYSTEM (profile
  config resolution) and S4U-not-Interactive (boot trigger), plus the config-reload gotcha —
  cycling the task does NOT reload config; the detached child survives and the duplicate
  guard declines, so a config change needs the process killed.

## [0.94.2] - 2026-08-25

### Fixed — the qwen3.5-9b agent seat shipped with thinking ON (measured live, both 8GB tiers)
- Both CUDA serving templates omitted any thinking pin on `qwen3.5-9b-agent`, on the
  documented premise that "llama.cpp's default for this template is thinking OFF".
  **That premise is FALSE on llama.cpp b10615** — verified live on BOTH 8GB reference
  boxes: the seat served with `reasoning_content` populated and, at a small
  `max_tokens`, returned **EMPTY content** (all budget consumed by reasoning).
  This is the configuration the 2026-08-22 bake measured as strictly WORSE (89%
  extraction with prose leaking into content, 3.5x the wall) — silently shipped as
  the seat every 8GB tier binds.
- Thinking is now pinned OFF via the ENV twin
  `LLAMA_ARG_CHAT_TEMPLATE_KWARGS={"enable_thinking":false}` in
  `llama-swap.win-cuda.yaml` and `llama-swap.linux-cuda.yaml`. The CLI form cannot be
  used: llama-swap's tokenizer strips the inner quotes of `--chat-template-kwargs`
  (json parse_error) and `--reasoning-budget 0` is inert for this template.
  The tier's `gpu_env` still merges into the same line (render-verified on both
  ampere-8 and blackwell-8).
- `TestQ359BSeatKeepsItsMeasuredInvariants` inverted accordingly: it now REQUIRES the
  env pin, still forbids the broken CLI form, and forbids `enable_thinking:true`.
  Mutation-proven — deleting the pin fails the guard.
- Measured after the fix on both boxes: 174 -> **2** completion tokens, `content` "OK",
  no `reasoning_content`.

## [0.94.1] - 2026-08-25

### Fixed — run-graph preflight deferred a VALID empty `min:0` autogrow group
- `satisfies()` in `render/preflight-graph-file.mjs` returned false on `children === 0`
  BEFORE reading the group's declared `min`, so an OPTIONAL autogrow group (`min: 0`,
  e.g. an `images` group serialised with no wires) was reported as a missing required
  input and the whole graph deferred. Found live 2026-08-24 running the official
  Mage-Flow T2I template on the blackwell-8 reference box; the field workaround was a
  dummy `"images": {}` key in the graph, which is no longer needed.
- `min` is now read before the zero-children early-out: zero children satisfy `min: 0`,
  while `min: 1` (the default) and `min: 2` groups still require their wires.
- Three regression tests pinned: empty `min:0` group passes, a genuinely missing
  non-group input on the same node still flags, and `min:1`-with-zero-children still defers.

## [0.94.0] - 2026-08-25

### Changed — ampere-8 agent seat: qwen3.5-9b adopted on its own reference bake (twin parity restored)
- Operator-approved 2026-08-25 after the tier's OWN on-reference leg-2 quality bake (Aorus RTX 3070,
  2026-08-24; blackwell-8 protocol replicated exactly — same fixtures/grader, fresh server per rep,
  /props-verified, n=2 per shape, thinking OFF): **9B think-off 100%x2 extraction + 5/5x2
  search+reason at 3 steps (6s reasoning wall)** vs the E4B incumbent's **0%x2 extraction** (zero
  tool calls — replicating blackwell) and 4B at 100%/5-5 but 10 steps (106-112s wall). Fit
  on-reference: 5759 MiB @16K / 6111 @32K q8_0, tg 56.6 t/s.
- `ampere-8` now sets `include_qwen35_9b: true` + seeds `agent_model: "qwen3.5-9b-agent"`
  (both-halves rule, mirroring blackwell-8), and raises `ctx_size`/`agent_ctx_tokens`
  16384 -> 32768 on the same measurements (E4B 3187 MiB @32K; ampere-6 already serves 32K on 6GB).
  Same seat rules as blackwell-8: default thinking OFF (never enable without a re-bake), never
  fold into the reasoning-off cascade macro. Twin FIELD-PARITY with blackwell-8 restored on the
  agent seat; the image-seat split stays deliberate.
- H1 ("serving params unmeasured-on-reference") RESOLVED in the tier notes: 0.93.2 ran
  detect/plan/render/acceptance on the reference box (acceptance 6/6 PASS; the box's BOM-corrupt
  home config was repaired bytewise, backup kept).
- Tier-matrix xlsx regenerated FIRST and row-verified (ground-truth rule). Evidence:
  `Benchmarks and Optimizations/2026-08-24-aorus-ampere8-standup.md`.
- `install-config-seed.test.ps1` pin flipped: asserts the adopted seat (both halves + 32K).

## [0.93.2] - 2026-08-25

### Fixed — `video_watch` synthesis: shots, not seconds; a clipped answer says so
- The 0.93.1 live run produced a full 4/4-window sweep (30 frames, 0 deferred) but the final
  answer stopped at ~22 s of 30: the synthesis enumerated every second and hit its 700-token
  budget silently. Now the synthesis groups consecutive seconds into spans, the budget is 1400,
  and a truncated answer carries `answer_truncated:true` plus an in-band marker pointing at
  `windows[]` (which always holds the complete notes).

## [0.93.1] - 2026-08-25

### Fixed — `video_watch`: a truncated window is kept as partial evidence
- First live sweep on the OptiPlex (30-s short, 4 windows) lost 2 windows to `vision output
  truncated`: 8 verbose per-frame notes overflowed the 400-token per-window budget and the
  window was dropped, so the synthesis reported those seconds as unverified. Now: the per-window
  prompt asks for COMPACT one-line notes (`<T s> …`, `same` for unchanged frames), the budget is
  768, and a note that still hits the limit is kept with `truncated:true` (the frames count, the
  synthesis treats the tail of that window as unverified) instead of being discarded.

## [0.93.0] - 2026-08-25

### Added — `video-watch` / `offload_video_watch`: watch a video END TO END
- `video_describe` samples `video_max_frames` at `video_fps` from the HEAD of a file — on the
  defaults (12 @ 2 fps) it sees the first six seconds of a thirty-second short and answers
  "I cannot tell" about the rest (measured 2026-08-24 on the OptiPlex 7060 while reviewing a
  Danmar RX-8 short). `video_watch` removes that ceiling: it probes the duration (ffprobe next
  to `ffmpeg_path`), plans fixed windows (`window_sec`, default 8), samples each window at `fps`
  (default 1 frame per second of the ENTIRE file, `max_frames`/`frame_width` per window), runs
  every window through the SAME per-call machinery as `video_describe` (cache keyed on the
  file's content digest + window, GPU-lock gate, breaker, 5xx retry, context-overflow
  halve-and-retry, ledger) with ABSOLUTE `<T seconds>` frame labels, then synthesizes the
  timestamped per-window notes into one answer on the text seat (`WithoutThinking`). Returns
  `{answer, duration_sec, windows_total, windows_deferred, frames_total, windows[...]}`; a
  window that defers is reported in place, the call defers only when every window did; a
  file that would exceed the 240-window cap fails LOUDLY (raise `window_sec`) instead of
  truncating. `start`/`end` narrow the sweep; `synthesize:false` returns notes only.
- New `internal/videoio.SampleFramesWindow` (input-side `-ss`/`-t`), `videoio.Duration`,
  `videoio.ProbePath`; `internal/tasks.buildVideoWatch`; `pipeline_videowatch.go` with the
  pure `planVideoWatchWindows` under test.

### Fixed — vision calls no longer die on THINKING vision seats (#168)
- `GenerateVision` / `GenerateVisionInterleaved` accept `...GenOption`; `vqa`, `ocr`,
  `assess_image`, `video_describe` and `video_watch` now pass `WithoutThinking()`
  (`chat_template_kwargs.enable_thinking:false`). Measured 2026-08-24 on `qwen3.5-9b-vl`: the
  default template spent all 512 tokens in `reasoning_content` and every vision verb deferred
  with `empty vision output`; with the kwarg the same seat answers in 8 tokens. Non-thinking
  templates ignore the key, so `qwen3vl-4b`-class seats are byte-for-byte unaffected in
  behaviour.

## [0.92.0] - 2026-08-25

### Changed — blackwell-8 image seat: HiDream-O1-dev is the quality DEFAULT (Z-Image -> speed opt-in)
- Operator quality-first ruling (2026-08-24): the `blackwell-8` `config_seed_ram_mid_high` image
  seat now defaults to **HiDream-O1-Image-Dev-2604** (MIT, arena Elo 1174 — the highest
  permissively-licensed image model) via the ComfyUI engine (`imagegen_engine:""`,
  `imagegen_family:"hidream-o1-dev"`, `imagegen_ckpt:"hidream_o1_image_dev_mxfp8.safetensors"`,
  `imagegen_script:comfy-generate.mjs`, 28 steps / cfg 1; VAE + text-encoder come from the
  checkpoint). **Validated on-box** 2026-08-25 on the OptiPlex 7060 reference box: 2048^2 render,
  ~118 s incl. cold load, editorial quality, ~7.8 GB peak (fits 8 GB with minimal RAM offload).
- **Z-Image Turbo demoted to the operator-selectable SPEED opt-in** — the `sdcpp_*` keys are
  retained in the seed; flip `imagegen_engine` to `"sdcpp"` + `imagegen_family` to `"z-image-turbo"`
  + `imagegen_steps` to `8` for fast GPU-resident drafts. Quality is the default.
- Rationale: the 2026-08-20 "resident in 8 GB / speed" choice violated the 2026-08-02 canonical
  media-quality-only rule (quality is the whole purpose of every media/generation seat; speed is
  never a reason to pick a lower-quality model — offload to system RAM instead). HiDream mxfp8
  weights are staged out-of-band (LAN), as with the >=16 GB HiDream bf16 seed.
- `setup/tests/install-config-seed.test.ps1` updated to assert the new blackwell-8 family.

## [0.91.0] - 2026-08-24

### Added — `integrations/opencode/`: the harness's opencode integration path, in-tree

The opencode plugin that gives the harness full feature parity inside opencode now lives IN
this repo (operator rule: there is ONE offload harness — every agent/CLI integration is an
integration path here, versioned and released with the harness, never a separate repo).
`integrations/opencode/` = the TypeScript plugin (`src/`: plan-time three-lane protocol
injection, `task` description rewrite, read-only task reroute to the local `offload`
subagent, H14 nudge + `agent_delegate` placement digest with honest failure/escalation
notes, idempotent agent/command/small_model provisioning, the cross-harness dispatch
instrument, `offload_plugin_status`), its bun:test suite (47 tests), and an example
`opencode.jsonc`. New CI job `integrations-opencode` (typecheck + bun test). Install = a
one-line loader in `~/.config/opencode/plugins/` pointing at the checkout — see
`docs/systems/opencode-integration.md`.
## [0.90.2] - 2026-08-24

### Changed — `tools/comfyui`: re-vendored from the Printing Press 4.31.1 reprint

Clean tree replacement of the vendored `comfyui-pp-cli` (press 4.30.2, run
`20260812-123958` → press 4.31.1, run `20260823-231507-1cd84666`), copied from the
library tree that passed the press's own shipcheck. The four post-publish patches in
`.printing-press-patches/` (cross-platform host paths, MCP code orchestration, the
structured error envelope, the code-orch gate + binary budget) are carried into the
reprint rather than re-applied by hand; their `.patch` files only refresh context lines.
Same module name (`comfyui-pp-cli`), same standalone-module contract: the root
`./...` still never sees it, and no harness package imports it.

What the reprint brings, as visible in the diff (71 files, +2042/−826):

- **MCP intents** (`internal/mcp/intents.go`, new) — composed higher-level tools
  registered beside the endpoint mirror; the hand-maintained `mcp-descriptions.json`
  is retired with it, so `mcp-sync` no longer has a locked description file to fight.
- **Typed shell-out** — `cobratree.RunCLICommand` returns a `CLICommandResult`
  (stdout, stderr hint lines, exit code) instead of a bare string, and MCP tool results
  are built from it, so a companion-CLI failure surfaces its stderr instead of an empty
  success.
- **`--agent` no longer implies `--yes`** — a mutating command run with `--agent`
  needs the explicit confirmation flag. Harness-detection helpers also land
  (`cliutil.CurrentHarness`/`IsAnyHarness`, `writeHarnessRefusal`) but nothing wires
  them into a command yet — they are latent in this reprint, not a behaviour change.
  The harness's own render path (`render/comfy-submit.mjs`) shells out as
  `submit - --json --skip-lint [--force]`, then `attach` / `wait`, never `--agent`,
  so it is unaffected by either.
- **Honest pagination and error bodies** — repeated-page and stuck-cursor guards on
  `--all`/sync (`paginationEndUnprovable`, `syncPaginationPageIsStuck`), a named
  `liveAllUnsupportedError` instead of a silent partial read, and HTML error pages
  collapsed to their title before they reach the envelope.

Verified in the worktree with the exact `tools-comfyui` CI contract: `go build ./...`,
`go vet ./...`, `go test ./... -count=1` — 17 packages ok, 0 failing (Go 1.26.6). Semgrep
`p/default` reports 17 findings on the new tree and the identical 17 on the unmodified
4.30.2 tree (same files, rules, and sites; only line offsets moved) — zero introduced,
all in press-generated SQLite identifier interpolation / companion-CLI exec sites that
the press's gosec pass already annotates. Library-side evidence for the reprint itself:
phase-5 acceptance `status: pass`, 161/161 matrix tests passed (167 skipped as
unverifiable offline); scorecard 91/A with `live_api_verification` the one unverified
dimension.

**Two press-side regressions found by fresh-context review, fixed at the source and carried
as patch 0005** (`.printing-press-patches/0005-verify-noop-contract-and-classifier-recarry.patch`),
both confirmed by reading the code and by A/B-running the 4.30.2 and 4.31.1 binaries:

*(1) Carried patch 0003 was only partly carried.* `classifyAPIErrorOnly`'s default branch
(`internal/cli/helpers.go`) had lost the `DELIBERATE MIGRATION` mapping of a dial failure to
`unreachableErr` (exit 4) and an HTTP 5xx to `upstream5xxErr` (exit 26). Re-carried
verbatim. (The harness render runner was never affected — `render/comfy-submit.mjs` treats
4/5/26 identically and its `submit` path exited 5 in both versions — but standalone agent
use regains the 4-vs-5 discrimination by exit code.)

*(2)* The press 4.31.x `writeNoop` returns a typed `*cliError` sentinel (callers unwrap
with `successfulNoop`); the seven hand-authored verify short-circuits (`submit`, `replay`,
`exp`, `history` ×2, `server free`, `upload mask`) returned it raw and so printed the noop
document *and* an `api_error` envelope, exiting 5 under `PRINTING_PRESS_VERIFY=1`. A
`noopOK()` adapter in the hand-authored `errenvelope.go` wraps all seven sites. Same patch:
`upload mask` now short-circuits `--dry-run` before its `--original` requirement (the press
verifier's dry-run probe) and declares `pp:happy-args`. Result at the library: press
`verify` 66/67 WARN → **67/67 PASS**; live dogfood matrix **163/163** (re-run after the
fixes; acceptance marker re-embedded).

Observed and left as-is (generator scaffolding, not behaviour): `store.ensureSQLiteJournalPrivate`
has no caller (new in 4.31.1, not a lost patch); the generated `Makefile` `test` target
dropped `-count=1 -race -shuffle=on` (CI calls `go test -count=1` directly);
`Config.StoreScopeCredential` / `defaultDBPathInDir`'s `unscoped` are unused scaffolds;
`.printing-press.json` has no trailing newline.

Harness version bumped for the changelog gate only — no harness code path changed.

## [0.90.1] - 2026-08-24

### Added — `docs/systems/opencode-integration.md` (doc-only)

The harness now has full support inside opencode, documented here: the same MCP launch
registered as `mcp.harness` (24 tools as `harness_<tool>`), house-rules parity via
`instructions: ~/.claude/rules/*.md`, and the [`opencode-local-offload`](https://github.com/dmmdea/opencode-local-offload)
plugin — plan-time injection of the three-lane dispatch protocol, automatic rerouting of
read-only subagent legs to a free local `offload` subagent, H14-parity nudges, an
`agent_delegate` placement digest, and the cross-harness dispatch instrument. Every surface
was verified live on 2026-08-24 with local primaries (the doc records each proof). One
measured caveat recorded: opencode's `mcp.*.timeout` bounds tool CALLS as well as tool
listing (agent_run died at 20 s, completed at 600 s) despite the SDK type comment.

## [0.90.0] - 2026-08-24

### Added — `residency` and `saturation` analytics verbs (llamaswap CLI)

The two deferred llamaswap-pp-cli analytics verbs, defined from 10 days of
accumulated mirror data and reshaped by a five-persona roast (which killed the
premises of the original three-verb design):

- **`residency`** (alias `replay`) — reconstructs each seat's load/eviction
  timeline from mirrored request gaps and costs a different idle-TTL
  (`--ttl model=seconds`). Correct completion-stamped interval math
  (`idle = next.ts − next.duration − prev.ts`), per-model TTL from config with
  the `-1` keep-set sentinel special-cased, structured keep-set / eviction-group
  safety fields, and correctly-signed bounds (reloads-avoided = optimistic
  ceiling, resident-minutes-added = upper bound). Print-only; simulates over
  history, never re-issues traffic or edits config.
- **`saturation`** — per-seat 429/5xx counts + rates, request volume, busiest
  hour, and hourly load curve. In-flight concurrency is deliberately NOT
  reported: llama-swap timestamps are whole-second, so depth would be a clock
  artifact (a measured slots=1 seat "showed" depth 6).

Both carry an honest coverage block distinguishing mirrored rows from prepoll
loss, with `coverage_pct` null (not a fabricated 100%) when a hole makes it
unsound. The dropped third verb (`overflow`) folded its one real signal
(walk-cap vs genuine loss) into that coverage block. New files:
`internal/cli/{residency,saturation,analytics_common}.go` + acceptance tests;
`which` index and reprint-guards updated. Interval-direction guard mutation-
verified red.

## [0.89.1] - 2026-08-23

### Changed — spread's pair guarantee stated at the decision point (doc-only)

The `agent_delegate` `route` description and `docs/systems/fleet-node.md` now
state what the scheduler already pins by test (`placeSpread`,
`TestRunSpreadDealsAcrossLocalAndEveryEligibleRemote`): the spread deal is
positional with subtask 0 always on the local seat, so a 2-contract spread with
an eligible remote is guaranteed one local + one remote — the local+server pair.
Per-subtask eligibility (missing `output_schema`, contract over-size for every
node) silently deals local, so `results[].placement` is the pair's confirmation. No behavior
change. Motivation: the measured all-cloud dispatch anti-pattern — sessions
spawned cloud subagents for read legs while the fleet idled; the in-context tool
schema is the one surface always present at the moment the dispatch is composed,
so the guarantee has to be readable there, not only in the scheduler's tests.

## [0.89.0] - 2026-08-23

### Added — `ocr` media-seat kind: the OCR specialist becomes declarable

`media_seats` gains kind `"ocr"` — a llama-server VLM seat bound to `ocr_model`
(so it coexists with the tier's general vision seat), carrying the two knobs the
PaddleOCR-VL class needs that plain vision does not: `chat_template` (a models-dir
template file rendered as `--chat-template-file`; the model transcribes DEGRADED
without it) and `temp` (a JSON-pointer field so the vendor-required `0` survives
omitempty). Validation mirrors vision (mmproj + own ctx required, llama-server
only, one per tier) plus temp-range and template-path checks; stt seats refuse
both knobs by name. Rendered flags are declared-only — every existing vision seat
renders byte-identically (pinned by test, mutation-verified red).

blackwell-8 declares the first one: **paddleocr-vl** (PaddleOCR-VL-1.6, measured
on the reference box 2026-08-23: ~2 s crop/doc answers at 4 GB loaded, ES factura
exact) — closing the recorded follow-up from 0.87.0; the box's hand-wired live
entry and the seed now agree. Fresh installs bind `ocr_model` automatically
(`Get-MediaSeatBindings` mirror extended).

## [0.88.0] - 2026-08-23

### Added — warn-at-intake acceptance lint on the delegate lane (`results[].acceptance_lint`)

Acceptance is the lane's verifiability gate (failed_verification vs success, and whether the
cross-seat retry fires), and three authoring shapes — each MEASURED in the standing corpus —
quietly disable it: PARROT-PASSABLE (every content check also satisfied by the goal text; an
echoed question passes as verified and the retry never fires — 5/5 of the first organic
contracts), UNGROUNDED (`contains:`/`regex:` matching nothing in the contract's own context
docs — `contains:OptiPlex` failed both seats on a task both did right), and SHAPE-ONLY
(`nonempty:`/`min_items:` alone — "the docs directory does not exist" passed
`nonempty:summary`).

- `delegate.LintAcceptance` classifies a PREPARED contract (context_paths already inlined) with
  the same `ParseAcceptanceCheck` the evaluator uses; `not_contains:<s>` counts parrot-passable
  exactly when `s` is absent from the goal (an echoed question trivially lacks it).
- WARN-ONLY on both surfaces (CLI + MCP): the warnings ride the response per subtask so the
  calling model sees them beside the result they weaken and fixes the acceptance for the next
  call. A single grounded, non-echoable check beside an echoable one suppresses the parrot
  warning — acceptance is a conjunction and still discriminates.
- Authoring rule documented (contracts/README.md, OPERATOR-GUIDE): anchor ≥1 check to content
  that appears in the docs but not in the goal.
- Mirrored into the memory-frontier Python A3 classifier (lockstep noted in both).

## [0.87.0] - 2026-08-23

### Changed — blackwell-8 media roster: measured seats for the editor box (operator-approved)

The blackwell-8 tier encodes the roster MEASURED on its reference box (OptiPlex 7060,
2026-08-23 nightshift; full results: the ecosystem benchmarks doc
`2026-08-23-dell-media-roster-research.md` §0-R):

- **Video (REVERSAL of the 2026-07-23 "IMAGE ONLY on 8GB" decision, by the operator, for
  this tier's editor role):** `config_seed_ram_mid_high` seeds the wan22 I2V lane —
  A14B Q8_0 two-stage unets at 832×480×81f (measured 520 s with the lightx2v 4-step LoRAs
  via the per-request `fast` param, peak 7751 MiB; distilled path stays opt-in), plus
  `videogen_upscale_model`. Wan2.2-TI2V-5B (720p, 567 s E2E) is validated as the
  run_graph/manual lane; VAEDecodeTiled is mandatory for video on 8 GB (plain VAEDecode
  measured non-completing). Low-RAM boxes still seed no video path.
- **Edit:** `gen_edit_unet` qwen-image-edit-2511 Q5_1 + `gen_edit_preset` lightning8
  (text-fix exact, removal professional; Q3_K_S measured quality-equal — the 9.2 GB
  option). Inpaint seeds RealVisXL.
- **Vision seat → `qwen3.5-9b-vl`:** the tier's agent-seat GGUF + its mmproj at ctx 8192
  (7660 MiB measured with vision; EN+ES OCR 100 %, VQA 3/3) — supersedes qwen3vl-4b.
  Seat weights stay out-of-band (SETUP-AGENT LAN pre-seed; warnMissingSeatModels covers
  absence).
- Recorded follow-ups (not encoded): PaddleOCR-VL OCR-specialist seat needs a mediaseat
  schema extension (chat-template-file + temp); LFM2.5-VL / gemma-E4B-mmproj extra
  registrations; ampere-8 media parity pending its own bake.

## [0.86.0] - 2026-08-23

### Added — the frontier NPU tools reach both harness surfaces

The Hailo sidecar grew five capabilities (Hailo repo PRs #7/#8: pose, instance
segmentation + FastSAM, on-NPU text embeddings, zero-shot classification,
whisper transcription); the harness now exposes them, gated on the accelerator
exactly like the first seven:

- **MCP + agent loop**: `offload_pose`, `offload_segment` (everything=true =
  FastSAM), `offload_text_embed` (same 512-d space as `offload_image_embed`;
  siglip2 optional), `offload_zero_shot` — registered on accelerator boxes
  only; the research profile carries them; `handleHailoTool` / the loop's
  adapter gained a per-tool required-arg (text_embed takes `text`).
- **`offload_transcribe` gains the universal `engine` (gpu|npu) param** —
  ownership mirrors OCR's: the GPU whisper seat stays the quality primary
  (timestamps, long-form); `engine:npu` is the caller's explicit fast preview
  path; unrecognized engines defer. NOTE: whisper-on-NPU is PLATFORM-BLOCKED
  on Windows HailoRT 4.24 (both HEF builds time out; Linux-validated upstream)
  — the sidecar returns a typed diagnosis, so engine:npu is honest, not lying.
- Accelerator `owns` list + profiles.json + the tier-matrix Accelerators sheet
  regenerated FIRST per the house rule.

## [0.85.1] - 2026-08-23

### Fixed — the default delegation profile stripped the loop's senses

Live verification on the accelerator box caught what no unit test could: the
`research` profile's tool allowlist predates every media/NPU tool, so the
0.85.0 loop-NPU wiring was unreachable under the DEFAULT profile — the loop
advertised 3 tools. The research allowlist now carries the read-only senses
(`offload_vqa`/`offload_ocr`/`offload_transcribe` + the 7 NPU tools) and its
system prompt names them. `WithProfile` intersects with the tools actually
enabled, so boxes without a vision seat or accelerator advertise exactly what
they did before — the measured small-seat narrowing is untouched there.
Pinned end-to-end through the real narrowing (with and without an NPUFunc).

## [0.85.0] - 2026-08-23

### Added — the agent loop gets the NPU (ADR 0024 completed for the loop surface)

The local agent loop (`agent_run` / `agent_delegate` / `local-agent` / `agent` CLI — all
via the shared builder) can now use the Hailo-8L accelerator, closing the gap where the
MCP surface had 7 NPU tools while a loop on the same box had zero:

- **`agent.NPUFunc`** — accelerator capability injected like `OffloadFunc` (the agent
  package stays free of hailoclient imports); `pipeline.NewLoopNPU` wires it to the same
  on-demand sidecar the MCP tools use (own `Sidecar` per holder is safe: `Ensure`
  health-checks before spawning). Defer-not-crash: transport/spawn failures come back as
  `{"deferred":true,...}` strings, never tool errors.
- **7 NPU tools in the loop** (face_detect, face_embed, object_detect, person_embed,
  depth, enhance_low_light, image_embed), registered ONLY when the box lists the
  accelerator — a box without the device advertises a byte-identical tool list (pinned).
- **Loop `offload_ocr` gains the `engine` param** (gpu|npu) — universal like the MCP
  surface's 0.82.0 decision; `engine:npu` without the accelerator defers honestly, never
  silently falls back (the two engines read stylised text differently). Mutation-tested.

## [0.84.0] - 2026-08-23

### Added — blackwell-8 agent seat: qwen3.5-9b-agent (measured on-box, operator-approved)

The blackwell-8 tier seats **Qwen3.5-9B UD-Q4_K_XL** as its agent planner
(`include_qwen35_9b` + `config_seed.agent_model: qwen3.5-9b-agent`), replacing the
E4B-by-fallback lane that the 2026-08-22 on-box quality bake measured at **0% extraction
×2** (zero tool calls) on the tier's own reference box (OptiPlex 7060, RTX 5060 8GB).
The seated configuration swept the same bake: **100% extraction ×2 + 5/5 search+reason ×2**
under default (off) thinking, at **6344 MiB @16K / 6696 MiB @32K** — the Gated-DeltaNet
hybrid's small KV is why a 9B fits an 8GB card at 32K, and the entry serves an explicit
`--ctx-size 32768` for that measured fit. Thinking ON was measured strictly worse (89% ×2,
reasoning prose leaking into content, 3.5× wall) — the seat ships llama.cpp's default
thinking-off and its invariant tests refuse `--reasoning off`, `${common}`, and
`--chat-template-kwargs` alike. Evidence:
`Benchmarks and Optimizations/2026-08-22-blackwell8-agent-bake/` (fixtures included) +
nightshift8-notes §25.

Mechanism mirrors the ampere-6 4B seat end-to-end: new `include_qwen35_9b` gate →
`IncludeQ359B` in `servingtmpl` (entry + `q359` matrix var + `__Q359B_ALT__` set
membership strip/expand together, refusal-by-name on an entryless template), the
`model-qwen35-9b` pin in `install.ps1` (5.6GB, HF LFS oid verified against the staged
reference copy; deliberately NOT family-gated — it is the only thing between the 8GB agent
lane and a 0% planner), and `warnMissingGatedModels`. The 4B and 9B seats are **mutually
exclusive** (both claim the `agent-seat` alias): `Render` refuses a tier setting both, and
the profile table is lint-pinned. SECOND deliberate twin-parity break: **ampere-8 keeps its
E4B fallback lane** pending its own on-box bake (Aorus offline; seat-lifecycle rule) —
recorded in both tiers' notes. Deploys to existing boxes are config-regen + weight
download; the Dell (powered off today) picks the seat up at its next config regen.

## [0.83.0] - 2026-08-22

### Fixed — fresh Windows installs never bound the media seats they rendered

`install.ps1` Step 8 (the fresh-config path) is a parity copy of `tierseed.Resolve` that
mirrored every seed layer EXCEPT the final one: `mediaseat.Bindings`, the sole writer of
`vision_model`/`stt_model`. So a seat-declaring tier rendered its vision/STT seats into
llama-swap.yaml while writing a config.json that never routed to them — `offload_vqa`/
`offload_ocr`/`offload_transcribe` deferred "no route" with the seats sitting in the roster
(field case: OptiPlex 7060 blackwell-8, 2026-08-22, wired by hand). New
`Get-MediaSeatBindings` mirrors the Go rule (change `tierseed.Resolve` FIRST, then the copy)
and merges as the last tier layer, before the accelerator seed. Closure-tested across every
seat-declaring tier in `install-config-seed.test.ps1`.

### Added — host-tool discovery for the complementary media routes (fresh config only)

Best-effort, Windows-installer-only probing for the two host-tool routes that previously
required hand-wiring: `gimp_console_path` (newest `Program Files\GIMP*\bin\gimp-console*.exe`,
unversioned name preferred — versioned paths rot on GIMP updates) and `edit_python` (PATH
python / `py -3`, Store-shim excluded, seeded ONLY when it can `import PIL` and no ComfyUI
venv exists — a ComfyUI box keeps deriving it at runtime, and a Pillow-less python would make
`edit_image` report CONFIGURED then fail at call time). Nothing found → config byte-identical;
existing configs are never touched, as with every seed layer.

## [0.82.0] - 2026-08-22

### Added — hailo-8l accelerator (ADR 0024)

Accelerators are ADDITIVE to the GPU tier: `profile` stays the one tier id, and
`accelerators: []` lists devices beside it. First device: the Hailo-8L M.2 NPU, served through
an on-demand loopback HTTP sidecar (port 18813) the harness spawns and that self-exits idle.
Everywhere the list is empty, tool REGISTRATION is unchanged — no tool is added or removed —
with one universal exception: `offload_ocr`'s schema gains an optional `engine` parameter
(`gpu`/`npu` enum) on every box.

- **Declaration + matrix**: `setup/templates/profiles.json` gains an `accelerators` map beside
  `profiles` — per device: `kind`, exclusively-`owns` capability list, detection rule,
  `config_seed`; the hardware/tier matrix gains an Accelerators sheet.
- **Config**: `accelerators`, `hailo_endpoint`, `hailo_sidecar_cmd`, `hailo_timeout_sec`,
  `hailo_idle_sec` keys (defaults inert) + `Config.HasAccelerator` — THE registration gate.
- **Detection**: `hwdetect.DetectAccelerators` (Go) / `Get-Accelerators` (detect.ps1) —
  `hailortcli scan` + `fw-control identify` → `Device Architecture: HAILO8L`; a full Hailo-8
  deliberately does not match (different HEF build). `OFFLOAD_ACCELERATORS` overrides the
  probe for benches without the device.
- **Seeding**: `tierseed.ResolveAccelerators` (authority; `Get-AcceleratorSeed` is the
  install.ps1 parity copy) merges accelerator seeds AFTER the tier seed with `__HAILO_HOME__`
  expansion (installer env `HAILO_HOME`, default `<OFFLOAD_HOME>/hailo`).
- **Installer + manifest + fleet**: install detects, seeds, and writes `accelerators` into
  `installed.json`; `fleet-serve` advertises the list in `/fleet/health` so a delegator can
  route NPU-owned work to the box.
- **`internal/hailoclient`**: loopback client + on-demand `Sidecar` (spawn once, shared across
  concurrent first calls, poll `/health`, idle self-exit; a 200 `{"error":true,...}` is a
  structured result, not a transport error).
- **MCP tools**: 7 NPU tools registered only on a box listing the device —
  `offload_face_detect`, `offload_face_embed`, `offload_object_detect`,
  `offload_person_embed`, `offload_depth`, `offload_enhance_low_light`,
  `offload_image_embed` — plus `offload_ocr engine:"npu"` (explicit caller-selected PaddleOCR
  path; GPU stays primary) and an `offload_status.accelerators` block whose health probe never
  spawns the sidecar. NPU calls are not in the savings ledger in v1 (recorded follow-up).
## [0.81.0] - 2026-08-22

### Added — A1 config pinning for the delegate lane; `arm` on the ledger row; prefill on the wire result

Tier 2 of the Phase 2 re-aim (delegate seat-quality experiment) requires that paired cross-seat
runs REFUSE to compare rows produced under differing serving configs — the 2026-08-17 corpus was
invalidated by exactly that (config unpinned across the `enable_thinking` fix). Pinning is now
recorded at the source, on every delegate run, both arms:

- **`seat_config_sha256` / `seat_config_basis` on the agent wire result** — the SERVER-side pin:
  `agent.ProbeSeatPin` reduces the seat's live `/upstream/{model}/props` answer (build, weights
  path, quant, `n_ctx`, slots, server sampler defaults, `reasoning_format`, chat-template hash,
  modalities) to a stable hash over a closed field set, plus a one-line human-readable basis.
  Stamped by `runAgentTask` only when the seat demonstrably served (the loop completed); pre-loop
  defers stay unpinned — probing `/props` on a non-resident seat would cold-start a model as a
  telemetry side effect, so the probe also gives up in 3 s rather than ever waiting out a load.
- **`harness_version` / `harness_build_sha256` on the agent wire result** — the REQUEST-side pin:
  per-call temperature, the re-pack's `enable_thinking:false`, and profile toolsets are code, so
  the exact binary is named (self-SHA-256, computed once per process). A version string alone
  cannot pin code identity — two checkouts can both say "0.81.0" while one carries uncommitted
  changes. This is the half the 2026-08-17 defect actually lived on; the `/props` hash alone
  would not have caught it, and is not sold as if it would.
- **`delegator_version` / `delegator_build_sha256` on the delegation-log row** — acceptance
  evaluation, retry policy and placement run delegator-side; a pairable row pins both ends.
- **`arm` on the LEDGER row** (`OFFLOAD_DELEGATE_ARM`, read at record time like the delegation
  log's field) — the prefill/cache columns shipped in 0.65.0 were measured under experiment arms
  and recorded without them: "computed then discarded", one file over from the 0.79.0 fixes.
- **`prefill_steps`/`prefill_tokens`/`cache_tokens`/`prefill_ms` on the agent wire result** —
  previously node-ledger-only, so a REMOTE run's prefill economics never reached the delegator's
  standing corpus. Budget-stopped runs carry them too (they burn the most steps).
- The delegate CLI/MCP response surfaces the four pins per subtask.
- All fields additive + omitempty: a pre-0.81 row or node decodes with empty pins, which readers
  MUST treat as "unknown — refuse to pair", never as a value.
- `internal/buildinfo`: new home of the version const (main.go aliases it; the VERSION-file
  agreement test still binds) plus the executable self-hash.

## [0.80.0] - 2026-08-21

### Added — `route: spread`, retry on a different seat, `delegate_remotes`; the agent-loop cap raised

- **`route: "spread"`** for `agent_delegate` / `delegate`: one call deals its subtasks round-robin across
  the local seat AND every fleet node that passes the hard gate, concurrently. Measured on 2026-08-21
  before this existed: four concurrent contracts under `auto` all ran on Qube's 27B seat ("local
  idle"), and under `remote` all four ran on Lenovo — the fleet never ran in parallel from one call.
- **Retry on a different node** after `failed_verification` or an `abstention` defer: one more attempt
  (local → best eligible remote, remote → local) under a fresh job id; the better attempt is published
  with `retried_on` / `retry_note`, the summary gains `retried` / `retry_recovered`. Transport
  failures and infrastructure/config/contract defers are not retried. The retry runs INSIDE the
  subtask's `timeout_sec` (what the first attempt left; skipped with a note under 10 s), so the
  documented per-subtask wall ceiling still holds.
- **`delegate_remotes`** config key: the fleet nodes the delegator considers when a call passes none —
  fleet membership as durable config instead of per-call knowledge. A call's own `remotes` replaces it.
- **Agent-loop same-tool cap 3 → 8** (`defaultMaxSameTool`): at 3 the cap starved legitimate multi-file
  reconnaissance (the 27B planner hit "read_file is now DISABLED" twice in one day while doing what it
  was asked); the exact-repeat refusal remains the loop guard.
- Docs: fleet-node.md gains "Placement routes and the retry"; coding-agent.md cap text; README row.

## [0.79.0] - 2026-08-21

### Added

Four fixes with one theme: **stop discarding values the harness already computes.** Each closes
a case where something is calculated on every call and thrown away, leaving a question
unanswerable while the data flows past. Same defect class as the agent-prefill accrual; these
are the remaining instances found by the memory-frontier Phase 2 re-aim pass.

- **`agent_profile` on agent ledger rows.** Resolved in `runAgentTask` (from `contract.Profile`,
  defaulting to `research`) and previously used to configure the loop and discarded. It matters
  because profile is the largest measured lever on small seats — **0 → 72% recall on a 6 GB tier
  came from changing nothing else** — so any prefill or quality figure aggregated across profiles
  averages materially different configurations. Set **before `Loop.Run`** so the defer branches
  carry it: a run that times out is when knowing its profile matters most. Records the
  **resolved** name, so defaulted runs are not misattributed to the empty string.
- **`labelDrops` counter on `labelAgreement`.** It previously returned silently whenever
  `answersAgree` could not judge a candidate. That silence is not neutral: an unparseable
  candidate is disproportionately an **extreme disagreement**, so dropping it quietly biases the
  published agreement rate **upward** — in the direction that argues against acting. Exposed via
  `LabelDrops()` so the rate can state its own coverage instead of implying it is total.
- **`arm` on the delegation log**, from `OFFLOAD_DELEGATE_ARM`. An **enabler, not a convenience**:
  the log is append-only and written concurrently by whatever sessions are running, so once arms
  interleave unlabelled they cannot be separated afterwards — timestamps do not distinguish an
  experiment from ordinary traffic. Empty for ordinary traffic, so existing rows read as *not part
  of any arm* rather than as a missing value.
- **Escalation-agreement view in `loupe`.** This reads a counterfactual that was **already running
  in production and merely had no reader**: `confhead-labels.jsonl` records, at 1:1 coverage of
  escalated rows, whether the entry tier agreed with the final tier. The Counterfactual-replay
  gate was held for a month as "blocked on identity coverage" — it never was, and could not have
  been: identity hashes are one-way, so the ledger could not replay an input at any coverage.
  - The view **refuses to publish an unconditional flip rate.** Labels exist only for escalated
    calls, and escalation is triggered by low confidence, so they are drawn from the calls most
    likely to disagree — an **upper bound**. The unconditional rate is reported as
    `insufficient_data` alongside the conditional one so the two cannot be confused.
- **Gate reopen tripwires** (`internal/config/gate_tripwire_test.go`). Several Phase 2 gates were
  closed on arguments that hold only while a specific default is off. Those arguments were written
  down; the conditions that would invalidate them were enforced nowhere. Flipping `exemplar_shots`,
  `shadow_enabled` or `knn_prefilter_enabled` now fails a test that **names the gate to reopen**.
  It is a tripwire, not a lock — the correct response is to make the change, update the test, and
  reopen that gate.

### Fixed

- The escalation view's entry-tier column is labelled **"as recorded"**, not "pooled". On the live
  corpus the two distinct values are `gemma-4-e2b` and `gemma4-e2b`, which `llama-swap.yaml`
  declares to be **one seat and its alias**, split cleanly by task. Calling that "pooled tiers"
  would imply different models were averaged and manufacture a caveat that does not apply — the
  entry tier was constant. It is still printed, because an alias masquerading as a second identity
  is the same defect that makes the result-cache key machine-specific.

## [0.78.0] - 2026-08-21

### Changed — `upscale_model` seeded on every image-capable tier

- Operator decision (2026-08-21): every tier that has an image route now seeds
  `upscale_model: 4x-UltraSharp.pth` (64 MB, ComfyUI `upscale_models/`) — blackwell-16/32/2x16/48/72,
  ampere-16, volta-16, ampere-8, blackwell-8, ampere-6, amd-rdna3, amd-rdna3-dgpu. A fresh install no
  longer depends on the `videogen_upscale_model` fallback for stills; the three tiers with no image
  route (amd-gcn, dual-gpu, cpu) stay unseeded and the route defers there by design. Seeded in the
  BASE `config_seed` (ESRGAN is RAM-tier independent), so the 8 GB tiers carry it on low RAM too.
- The route still needs a ComfyUI install on the box (the runner is the ComfyUI graph): on an
  sd.cpp-primary tier without ComfyUI, `offload_status` shows the `comfyui` prereq missing and the
  call defers. An sd.cpp arm for the upscale route (stable-diffusion.cpp has native ESRGAN support)
  is a documented follow-up, not part of this seed.
- Tier docs regenerated (`go generate .`), tier matrix xlsx regenerated from the seeded profiles
  (ground truth updated first, per house rule).

## [0.77.0] - 2026-08-21

### Added — `offload_upscale_image` / `upscale-image`: ESRGAN enlargement as a first-class route

- A still can now be AI-upscaled on the local ComfyUI through the harness (GPU lease, ledger,
  `offload_status` route) instead of a hand-built `run_graph`. Graph: `LoadImage` →
  `UpscaleModelLoader` → `ImageUpscaleWithModel` → optional `ImageScaleBy` / `ImageScale` →
  `SaveImage` (`render/wf-upscale.mjs`; runner `render/comfy-upscale.mjs`, standard
  `withGpuSlot` lifecycle).
- **Binding:** `upscale_model` (a ComfyUI `upscale_models/` filename). Empty falls back to
  `videogen_upscale_model` — the same ESRGAN file the video route already binds — via
  `config.EffectiveUpscaleModel`, which the pipeline gate and `mediacap` both read, so a box
  that upscales video upscales stills on day one with no new key. `upscale_script` ships as a
  default like `run_graph_script`; `upscale_timeout_sec` defaults to 600 (the render is seconds,
  the budget is the cold ComfyUI start).
- **Params:** `scale` (overall factor relative to the SOURCE, made EXACT: the runner measures the
  source header and pins the output size, so it holds for any model regardless of what its
  filename claims; the filename-derived factor is only the fallback for an unreadable header),
  `width`+`height` (exact, ≤ 16384, win over `scale`), `method` (the five core resamplers),
  `model` (per-request bare-filename override), `out`. The pipeline defers before any GPU work on
  a half-given/negative/oversized size, a non-positive scale, a bad method, or a path-shaped
  model; the runner pre-flights the builder (incl. ComfyUI's `scale_by` 0.01–8) before the slot.
- **Post-render verification:** the written file must decode (an undecodable file defers instead
  of returning a size-less success) and, when a size was requested, match it within 2 px — a
  mismatch defers naming produced-vs-expected — and whether the renderer ignored a pinned size or
  the runner could not measure the source (GIF) and used the model's filename factor. Returns
  `{image_path, model, width, height, factor}` with the size read back from the file and `factor`
  the measured output/source ratio (`factor_x`/`factor_y` for a non-uniform pinned size).
- Review rounds (fresh-context code-reviewer + silent-failure-hunter) caught, before ship: a
  negative `width`+`height` pair passing the "given together" gate and rendering at native factor
  with `OK:true`; the `nativeFactor` regex reading `2xLexicaRRDBNet_Sharp` as 4 (so `scale:2`
  would have returned a 1× image); `scale: 0` silently treated as unset by the MCP handler;
  ComfyUI's resolution/`scale_by` limits only discovered inside the GPU slot.
- **Why a route and not Upscayl:** the operator asked whether Upscayl was in the harness. It is
  not, and installing it would duplicate models already on disk (`RealESRGAN_x4plus`,
  `4x-UltraSharp`, `HAT_SRx4`) outside the queue/lease/ledger discipline. The gap was a tool.
- Tests: builder (`wf-upscale.test.mjs`), `upscaleArgs` + `OutputSize`, pipeline defer reasons,
  GPU-lease coverage case, tools/list advertisement + bad-args, `mediacap` route, `tierdocs`
  media-key classification.

## [0.76.0] - 2026-08-21

### Added

- **Agent-loop prefill now lands in the ledger** (`prefill_steps`, `prefill_tokens`,
  `cache_tokens`, `prefill_ms` on `core.Meta` and `ledger.Entry`), plus an `agent_prefill`
  view in `loupe` that carries its own verdict.
  - **Why this was needed even though the instrument already existed.** The prefill
    aggregator shipped in 0.65.0 and worked correctly — but its output went nowhere durable.
    `PrefillReport` rode home on the in-process `Result` and on a `local-agent` stderr line,
    neither of which survives the call. So the question it exists to answer (memory-frontier
    T2-B: *does the agent loop have a repeated prefix worth stabilising?*) still could not be
    answered from real traffic, while the numbers arrived on every step and were dropped.
    **An instrument whose output nothing captures cannot decide anything.**
  - Captured **immediately after `Loop.Run`, before the defer branches**. Every one of those
    branches still records a ledger row, and a budget-exhausted or timed-out run is precisely
    where prefill is largest. Reading it only on the success path would systematically exclude
    the expensive runs and bias the number this exists to produce.
  - **`prefill_steps` is the discriminator, not a statistic.** Present (>0) means the row was
    measured; absent means unobserved. Without it `prefill_tokens: 0` is ambiguous between
    "the server reused everything" and "nothing was ever observed" — opposite conclusions, and
    the second one is the only result that would justify building prefix-stability work.
  - The `loupe` view refuses a verdict below a 20-row floor, and reports `insufficient_data`
    rather than a fabricated 0% when nothing has been observed. Verdict bands: HIGH REUSE
    (≥80%, work not justified) · PARTIAL · LOW REUSE (<40%, the case that justifies it).
  - `kv_reuse_pct` is `CacheN/(CacheN+PromptN)`, never `CacheN/PromptN` — the latter leaves the
    cached tokens out of its own denominator and reports past 100%.

### Notes

- Verified **end-to-end on a real agent contract**, not only in unit tests: a live
  `delegate --contract` run against an isolated scratch ledger produced
  `{"task":"agent","prefill_steps":2,"prefill_tokens":988,"cache_tokens":854,"prefill_ms":2231}`
  — 46.4% KV reuse. The outer `agent_delegate` row correctly carries no prefill, since that is
  the dispatch layer rather than the loop.

## [0.75.0] - 2026-08-20
### blackwell-8 image seat: Z-Image + SDXL (operator decision 2026-08-20)
The tier's reference box exists now (OptiPlex 7060, RTX 5060 8GB @ Gen3 x4, rebuilt 2026-08-19),
and the operator decided its image lane: **Z-Image Turbo primary** via the sdcpp engine — the
amd-rdna3-proven binding (Q8_0 diffusion GGUF + zimage_ae VAE + Qwen3-4B text encoder,
steps 8 / cfg 1) — with **SDXL pre-staged** on the ComfyUI route (`imagegen_ckpt`
RealVisXL_V5.0_fp16 seeded inert; the comfy route's unknown-family fallthrough IS the SDXL
builder with that exact default). The flip to SDXL is three keys and the notes say so
(clear `imagegen_engine` AND `imagegen_steps`/`imagegen_cfg` — 8/1 is Z-Image's turbo recipe and
renders mush on RealVis). `vae_mode` is `tiling`, not the AMD seed's `cpu`: the seed lint
rejected the verbatim copy with its measured reason (cpu = 7.8x slower on CUDA backends) —
the gate working as designed. This DELIBERATELY breaks ampere-8/blackwell-8 field parity on
hardware grounds (sm_120 FP8-class model); ampere-8 keeps HiDream-O1, its one verified
measurement, and both tiers' notes record the split. The Dell's LIVE config is owned by its
own session; this seed is the durable tier encoding for fresh installs.

## [0.74.0] - 2026-08-19
Nightshift-9 batch: one pre-existing render defect fixed, plus the measurement-free half of the
operator-tasked 8GB-class re-evaluation (full proposal + bake plan:
`Benchmarks and Optimizations/2026-08-19-8gb-tier-reeval-proposal.md`).

### FIXED — `model:"ace"` with a still rendered music conditioned on an image file path
The ace (text-to-music) arm read its style-tags prompt from `pos[1]` unconditionally, but the
pipeline builds `<out> <still> <prompt>` whenever a still is supplied — for every model. Found
by 0.73.1's round-3 reviewer, pre-existing since the arm shipped. Prompt now resolves
`pos[2] || pos[1] || --prompt`, a "prompt" naming an existing file refuses loudly, and the
resolved style prompt is logged (output audio alone cannot show which text conditioned it).
E2E under a real lease: still + tags rendered 30.0 s FLAC with
`ace style prompt: upbeat corporate, 120 bpm, bright synth` in the log; mutation red pasted in
the commit.

### 8GB tiers (ampere-8 / blackwell-8) — hygiene from the re-evaluation
- **`agent_profile: "research"` seeded on both** — the measured 0%→72% small-seat lever
  (ampere-6 parity); the class previously fell to `general` by omission, the configuration the
  house measured as broken. Seed-lint typo mutation verified red.
- **`gpu_env` parity**: ampere-8 gains blackwell-8's `CUDA_VISIBLE_DEVICES=0` +
  `CUDA_MODULE_LOADING=LAZY` — its reference box is a hybrid-graphics laptop, the exact failure
  class the field exists for.
- **Notes corrected**: the class's serving params are UNMEASURED-ON-REFERENCE (the reference box
  never ran the installer); the missing `ocr` alias is BY DESIGN pending ocrprobe measurement.
- **Deliberately NOT shipped**: the low-RAM sdcpp image seed (the mid/high overlay MERGES over
  `config_seed`, so it needs a `config_seed_ram_low` schema arm — build work); `ctx_size`
  16384→32768 and every seat candidate (Z-Image Turbo, Qwen3.5-9B agent, Qwen3VL-8B, …) wait for
  the measured bake on the reference box per the seat-lifecycle rule.

## [0.73.1] - 2026-08-19
Follow-up to 0.73.0, entirely from its own post-merge adversarial review. Five
fresh-context reviewers ran; three INDEPENDENTLY found the same defect below, and six
refutation passes upheld it. 0.73.0 is not reverted — nothing in it changes a render path —
but its headline feature was wrong on an opt-in path, and two of its new guards were vacuous.

### FIXED — the video family label could assert a family that did not render
0.73.0 computed `model_tier` from config while the runner argument was resolved 30 lines later
with the OPPOSITE precedence: an explicit per-request `model` wins. So any caller using the
documented override got a ledger row naming this box's configured seat for a render that used
a different family — a **false** provenance value, strictly worse than the vague label it
replaced, and the exact opposite of the change's stated purpose. Both ingresses plumb `model`
verbatim with no validation (MCP `offload_generate_video`, CLI `--model`), and the MCP schema
advertised `wan` as the default while omitting `ltx25`, which made the broken call the
*likeliest* one rather than an edge case.

Two call sites re-deriving one precedence rule is what caused it, so they are now **one**:
`resolveVideoFamily` returns both the runner argument and the canonical render family, and `videoModelLabel` takes the
RESOLVED family rather than the config. `wan` normalises to `wan22` so one family never keys
two tiers. Guarded by a test comparing the two derived provenance surfaces against each other
rather than each against its own input — the assertion whose absence let this ship.

### FIXED — the footprint store was recording every video render as Wan
Same defect class, one surface over, and this one feeds decisions rather than reports:
`footprintSampling` hardcoded family `"wan2.2"` and derived the quant from the **Wan** GGUF
filenames, which stay bound on a box whose seat is another family. Measured on the reference
box: an LTX-2.5 render logged 24.1 GiB into `wan2.2/q8_0/video-gen`, a store the fleet reads
for placement. Both keys now come from the resolved family, and the quant is scoped to the Wan
family instead of guessed for the others. The Wan family keeps the store's own `wan2.2` spelling,
so no key is orphaned and the accumulated Wan history stays attached (see the second fix round
below — an earlier attempt renamed it and split the store from the advertiser). **Operator note:**
the existing `wan2.2/q8_0` entry still holds a few samples from LTX renders mis-attributed before
this fix; its VRAM figure is a mix until enough real Wan renders re-converge it.

### FIXED — two 0.73.0 guards were vacuous
- The gate-token guard asserted no literal `__TOKEN__` survived rendering, but `Render`'s own
  `uniqueTokens` gate already errors on that, so the assertion was **unreachable**. It was
  blind to the mutation a reader would really make — emptying a substitution's VALUE instead of
  removing its key — which leaves no token behind and kept the suite green. Rewritten to compare
  the token's own group-expression line with its gate flipped, one gate at a time. (Two
  intermediate rewrites were ALSO vacuous, for the same reason each time: flipping a gate adds
  or removes the model block too, so any whole-document comparison is satisfied by that churn.
  Both were caught by re-running the mutation rather than by inspection.)
- The gated-weight filename consts claimed to pin "the templates' own cmd contract", but both
  sides of every comparison were Go literals in this repo — a template-side rename passed green.
  Now asserted against the real contracts: the shipped templates AND `install.ps1`.
- Both 0.73.0 `*To` refactors left the PRODUCTION call unpinned — deleting
  `warnMissingGatedModels(...)` or `warnImageGenBindingTraps(c)` left every test green while the
  warning stopped existing. Finding I-2 was "this function has zero tests"; covering the body
  while the call can vanish reproduced it one layer out. Both call sites are now pinned.

### SECOND fix round — 0.73.1's own review found more, and it was right again
0.73.1 was reviewed before merging (4 lenses, 12 refutation passes, 1 arbiter) precisely because
the ledger says fix rounds reintroduce the class they fix. The arbiter returned **DO-NOT-SHIP**
and proved three of the new guards vacuous by mutation. All corrected here before merge:

- **The Wan footprint key kept the store's own spelling `wan2.2`, not `wan22`.** The first
  attempt unified on the config spelling — which split the writer from
  `fleetnode.familyFor`, the function that ADVERTISES the family on `/fleet/health`, fleet-wide
  and on the happy path. `familyFor` now derives the video family too (it hardcoded Wan while
  the image arm already derived from config), and the store's accumulated Wan history is
  preserved. This also makes the earlier "prune the orphaned entry" note unnecessary — there is
  no orphan. **Round-3 correction:** `familyFor`'s first derivation was a PASSTHROUGH, so
  writer and advertiser agreed only on the recognized family set and still split for
  unrecognized config values (`wan`, `LTX25`, a typo) — the writer folded them to Wan while the
  advertiser echoed them verbatim, measured across 10 inputs. `familyFor` is now the same
  allowlist the writer uses (`ltx25`/`hunyuan`/`ace`, else `wan2.2`), and the cross-package
  guard asserts against the REAL `fleetnode.Families` over that full input space — its first
  version compared the writer to three hardcoded strings, behind a comment claiming an import
  cycle that does not exist, and reverting the `familyFor` fix left every package green.
- **An unrecognized `model` override no longer echoes the caller's string.** The runner matches
  its families EXACTLY and case-sensitively and silently falls through to Wan, so recording the
  caller's spelling recreated the false-provenance class one layer out: `model:"LTX25"` rendered
  Wan while the ledger claimed `comfyui-video:LTX25` and the footprint store gained an `LTX25`
  key holding Wan's VRAM profile. Unrecognized input now records Wan, the family that runs, and
  `TestVideoRunnerFamiliesMatchTheRunner` pins the allowlist against the runner's own dispatch
  literals so a family added there cannot silently drift.
- **The call-site pins were text greps and were vacuous twice over.** A bare `name(` check is
  satisfied by the func DEFINITION; the full argument list is satisfied by a COMMENTED-OUT call
  (both measured). They now parse the AST, which excludes comments and string literals by
  construction — the first version that can actually fail.
- **The template weight guard concatenated all seven templates**, so renaming a weight in a
  SUBSET stayed green while the header claimed that mutation closed. Now asserted per template
  that DEFINES the model, which is the real contract (`templateFor` picks exactly one template
  per goos/backend), with a guard against the probe matching nothing.
- **Added the wiring-level guards the helper tests could not provide.** With every helper correct
  and unit-tested, re-introducing the original defect at the CALL SITES left `go test ./...`
  fully green, because nothing read either provenance value through a real `Run()`. Two arms,
  because each covers a half the other structurally cannot:
  `TestRunGenerateVideo_OverrideProvenanceEndToEnd` (seat `ltx25`, request `wan`) is the LABEL
  half — verified red under `meta.Model = videoModelLabel(p.cfg…)` with
  `model_tier = "comfyui-video:ltx25", want "comfyui-video:wan22"`.
  `TestRunGenerateVideo_SeatedProvenanceEndToEnd` (seat `ltx25`, no override) is the FOOTPRINT
  half — verified red under the literal 0.73.0 hardcode with
  `no (ltx25, video-gen) entry in […ModelFamily:"wan2.2"…]`.
  **Correction from the round-3 review:** an earlier revision shipped only the override arm and
  claimed it was "verified red against both re-introduction mutations". That was FALSE for the
  footprint mutation — the override arm's correct footprint answer is `"wan2.2"`, byte-identical
  to the hardcode, so it structurally cannot see that half; the whole suite stayed green under
  the literal 0.73.0 footprint hardcode. The seated arm is the fix, and the red lines above are
  pasted from the actual mutation runs rather than asserted.
- **Corrected a false claim of our own:** `argModel` was described as byte-identical to the old
  router. It is not — the resolver trims where the old block did not, which changes the rendered
  graph for `model:" "` on a bound box and for a config family with stray whitespace (both
  previously fell through to Wan silently). The trim is corrective and stays; the claim is now
  accurate in the code comment, the test docstring, and above.

### Docs corrections
- `docs/systems/gpu-lease.md` still listed "`internal/pipeline` does not yet take a `media`
  lease" as a known gap. False — `acquireMediaLease` wraps all eight generation routes — and it
  directly contradicted the rule 0.73.0 added four lines above it.
- The new `:8188` prohibition named the external Comfy-MCP server but not **this repo's own**
  `tools/comfyui` CLI, which defaults to `127.0.0.1:8188` and POSTs graphs without the lease —
  making the rule one the repo appeared to violate. Now named, with its supported posture (the
  harness's submit backend, where the lease is already held) stated.
- 0.73.0 edited a RELEASED 0.68.0 changelog entry, erasing the record of when the dead token was
  introduced. Restored; the removal is recorded here instead.

## [0.73.0] - 2026-08-19
### Media telemetry records WHICH family rendered
`model_tier` on a video row was a flat `comfyui-video`, so the ledger recorded only THAT a
render happened. That is why the 2026-08-12 LTX-2.5 seat binding was still being questioned
six days later while config, code, binary and the tier matrix all carried it: the record
could not distinguish an `ltx25` render from a `wan22` one, so the binding was unprovable
from telemetry. Rows now carry `comfyui-video:<family>`, mirroring the image route's
checkpoint label. An unbound family keeps the historic label so health tiers don't fragment;
`wan22` is labeled rather than folded into the unbound case, because "rendered with wan22"
and "family unbound" are different facts.

### The pooled VIDEO seat now warns without `--disable-dynamic-vram`
The pooled IMAGE seat has warned since 0.59.0. The video seat carries the identical
ComfyUI-MultiGPU #191 requirement — the `blackwell-2x16` tier seeds
`videogen_pool_vvram_gb=30` — and had **no check at all**. Un-pooled, an int8 DiT that
upcasts to ~39 GB at compute no longer fits the virtual pool it was measured against, and
DynamicVRAM prints the pool allocation banner either way, so a misconfigured box was
indistinguishable from a pooled one at every observable surface. The two seats warn
independently so the message names the config key actually at fault. Warn-only, as before: a
manually-started server may carry the flag, so an absent env var is a smell, not proof.
Live-fired three arms on real binaries against the live config — new binary without the flag
emits both warnings, with the flag emits neither, and the previous 0.72.0 binary without the
flag emits the image warning only (the control proving the video branch is new behavior).

### Review follow-ups from the 0.72.0 round
- **`warnMissingGatedModels` had zero tests** (finding I-2). It is the last line of defence
  for a specific silent failure: llama-swap lists models from the CONFIG, so a gated entry
  whose weights were never downloaded makes `doctor` and `acceptance` PASS while the route
  fails only when called. Now covered, each case with its silent arm — including the
  cross-machine skip, whose correctness is invisible in production because it produces no
  output either way.
- **`__Q354B_AND__` was substituted but consumed by no template** (finding I-3), while its
  siblings are live (`__M26_AND__` in 2 templates, `__Q38_AND__` in 1) — the small-tier agent
  seat is never a member of an interactive `&` group. Removed, with guards on BOTH halves of
  the asymmetry, because the obvious cleanup in either direction is wrong: re-adding the dead
  one restores dead code, while deleting a live one leaves an unexpanded literal that
  llama-swap rejects at startup. The rendered-output guard derives each template's gates the
  way `Render` does, so it covers all 7 shipped templates rather than the 2 a hardcoded gate
  set would reach — the same blind spot that let a Linux-template regression pass a
  fully-green suite.

### Docs — direct `:8188` posts are now forbidden by rule
ADR 0018 recorded the lease bypass as a *consequence* but no document forbade it, and the
operator guide had no prohibition. Since the gap cannot be closed at the mechanism level (the
lease binds only code paths that take it), it is closed by rule in the three places a reader
lands, with the `run-graph` and `gpu reserve --class media` escape hatches that keep the
lease. Names the official Comfy-MCP server as the same bypass class: it drives ComfyUI
directly, so it is adoptable only behind the harness.

## [0.72.0] - 2026-08-18
### Security/privacy — the operator's tailnet zone was compiled into this PUBLIC repo
`internal/netguard/tailnet.go` carried `const houseTailnetSuffix = "<a real tailnet>.ts.net"`
— one operator's private Tailscale DNS zone, hardcoded in an Apache-licensed public
repo, and load-bearing in a security gate. It is now the config key `tailnet_suffix`.

- **The default is FAIL-CLOSED, not the old value.** Empty = the suffix branch is skipped
  entirely, so the admitted set is strictly NARROWER than before: loopback,
  `100.64.0.0/10` literals and dotless MagicDNS names still pass; a dotted tailnet FQDN
  does not until the operator sets their own zone. A generic `.ts.net` fallback was
  deliberately NOT used — it would admit ANY tailnet's Funnel-published hostname, i.e. a
  public-internet endpoint wearing a tailnet-looking name, which is the exact hazard the
  original constant existed to prevent. Widening a security boundary to solve a privacy
  problem would have been the wrong trade.
- `SetTailnetSuffix` normalizes (lowercase, strips a leading dot and a trailing root dot)
  and REFUSES a value that is not a plausible DNS zone, because a typo there fails open in
  the reader's mind ("I set it, so my host is allowed") while failing closed in the gate.
- Installed BEFORE endpoint validation in the config loader — setting it afterwards would
  judge this load's endpoints against the previous value.
- New tests pin all of it: the fail-closed default (a real tailnet FQDN must be refused
  with no suffix configured, while loopback/CGNAT/dotless still pass), and
  normalization + refusal.

### Also stripped from the public tree
Operator identity that had no business in a public repo, replaced with examples:
a tailnet zone, a Tailscale CGNAT IP, a node hostname, two real GPU UUIDs, and private
`/srv/...` filesystem paths. RFC1918 literals in `fetchtool_test.go` were KEPT — they are
generic addresses used to test the private-IP block, not the operator's network.

## [0.71.0] - 2026-08-17
### Fixed — a fresh Windows install wrote a broken sdcpp binding on three tiers
`Merge-ConfigSeed` is the PowerShell parity copy of `tierseed.Resolve` and writes the
FRESH `~/.local-offload/config.json` at Step 8, bypassing Go. When `vae_mode` was
introduced on the Go side, this copy was never taught either half of it:

- **`__EXE__` was never expanded.** Only `__OFFLOAD_HOME__` was substituted, so
  `sdcpp_bin` shipped as a literal `.../sdcpp/sd-cli__EXE__` — a path that does not
  exist. Affects every tier whose seed carries the token: **ampere-6, amd-rdna3,
  amd-rdna3-dgpu**. Now expanded unconditionally (this script only runs on Windows, and
  the token is OS-dependent, not install-root-dependent — it must expand even with no
  `-OffloadHome`).
- **`vae_mode` was copied through verbatim.** It is a seed-only DIRECTIVE that Go
  translates into an `sdcpp_extra_args` flag and never emits. So a fresh install wrote a
  meaningless `vae_mode` key AND never got the flag — on `amd-rdna3` that flag is
  `--vae-on-cpu`, which the tier notes record as REQUIRED on an AMD iGPU or the VAE
  renders all black (sd.cpp #563/#1621). Now mirrored from Go's `vaeArgs` map.

**These were not new bugs.** `setup/tests/install-config-seed.test.ps1` had been
reporting them for five assertions — it was simply never wired into CI, so it sat red
and nobody read it. Four of the five "stale failures" were the suite correctly reporting
real defects; only one assertion was genuinely out of date (it read `sdcpp_extra_args`
off the raw seed, which stopped existing when `vae_mode` replaced it) and is rewritten to
assert the translation end-to-end instead.

### Added — the download half of the gate mechanism is now testable and tested
The gate→download-set mapping lived inline in the main flow, BELOW the dot-source test
seam, so nothing could reach it: deleting the `model-qwen35-4b` download line left the
entire suite green while the seat still rendered into the yaml and `config_seed` still
named it — the exact split-brain the gate exists to prevent. Extracted as
`Get-GatedModelKeys` and pinned, including the DELIBERATE asymmetry in both directions
(qwen3.8-27b rides the `OFFLOAD_WITH_FAMILY` gate at 18.8GB; the 2.9GB qwen3.5-4b agent
seat does not, because a lean install that dropped it would ship the tier's weakest
configuration while the yaml still named the seat). Mutation-verified: deleting the
download line turns three assertions red.

Note the extracted function returns WITHOUT the `,@()` no-unroll wrapper, deliberately:
that guard is right where a 1-element array must survive JSON serialization, but on an
empty result it produces a 1-element array holding an empty array.

### Added — `install-config-seed.test.ps1` runs in CI
Previously only `detect.tests.ps1` and `render.tests.ps1` ran, so the suite covering what
an install SEEDS and DOWNLOADS had no gate at all.


## [0.70.0] - 2026-08-17
### Added — `agent_profile`: a per-tier DEFAULT tool profile for the agent loop
Closes the standing question "should a non-general agent profile be enforced as a per-tier
config default?" The measured answer is yes, as a **caller-overridable** default.

- **Why.** `general` (every enabled tool, no tuned prompt, no exemplars) is a measured trap on
  a small planner. On the ampere-6 reference box (RTX 3050 6GB) the SAME model on the SAME
  contract scored **0% under `general`** — twelve steps burned calling tools, zero output bytes
  — and **72% narrowed**. That is a larger factor than the choice of model. The house recorded
  it qualitatively in July 2026 as ampere-6 "decision 8, never enforced in config"; this is the
  enforcement. The `agent_run` tool schema already DOCUMENTED the hazard while still defaulting
  to it.
- **New config key `agent_profile`** (`internal/config`), with
  `Config.AgentTaskProfile(explicit)` resolving **explicit > `agent_profile` > `general`** —
  deliberately the same shape as `AgentPlannerModel`. The chain stays LIVE at rest: an unset key
  is never materialized into the config file (pinned by a round-trip test), so a later change to
  the default still reaches existing installs.
- **Every front door honours it.** `agent_run` (MCP) now resolves through the config instead of
  applying a profile only when the caller named one, and always calls `WithProfile` — `general`
  is a documented no-op on the tool set, so a box that does not set the key is byte-identical.
  `local-agent`'s `--profile` default changes from `"general"` to `""` so "unset" is
  distinguishable from "explicitly general", and its stderr notice now names the SOURCE
  (`--profile` vs `config agent_profile`) — silently changing which tools a run advertises
  should never be invisible.
- **`--two-tier` is exempt**, deliberately: it builds its own architect/editor loops and sets
  their toolsets itself, so a box default must not bleed in. `validateFlagCombo` already refused
  an EXPLICIT profile there; the new `resolveProfileName` refuses the implicit one. Mutation-
  verified: deleting the exemption turns `TestResolveProfileName/two-tier_IGNORES_the_box_default`
  red with `"research", want "general"`.
- **Seeded on `ampere-6`** (`config_seed.agent_profile: "research"`) — the profile the bake-off
  actually measured. No other tier is seeded: the effect is measured on this silicon only, and a
  benchmark result is not a mandate elsewhere. `tierseed`'s `configKeys()` reflects over the
  Config struct, so the new key validates as a legal seed with no per-key plumbing.
- **Review round (independent, pre-merge) — 2 critical + 4 important, all fixed:**
  - `agent_run` reported `len(BuildResult.Tools)`, a snapshot taken BEFORE narrowing, so a
    seeded box advertised 3 tools while the response said 11 — and carried no `profile`
    field at all. That is the exact hazard this entry claims to fix, fixed on the CLI half
    and missed on the MCP half. Now reports the post-narrowing count via a new
    `Loop.AdvertisedTools()` plus the applied `profile`.
  - The `agent_run` TOOL description still promised "plus the offload_* cascade", which a
    narrowed profile drops entirely. An MCP client reads that to decide whether to
    delegate. Reworded; only the `profile` PROPERTY description had been updated.
  - `agent_profile` was not trimmed (only the explicit argument was), so
    `"research "` would fail every lookup and brick the lane on a trailing space.
  - The doc comment claimed an unknown name is "a loud startup-time error" — true for
    `local-agent`, false for the MCP door, which returns a per-call defer. Corrected to
    state each door precisely, and `tierseed.validate` now checks the VALUE against
    `agent.LookupProfile` at tier-authoring time (it previously validated only that the
    KEY was a real config field, so `"reserch"` would ship in a template).
  - Docs claimed "every front door honours it" while listing the delegation lane, which
    hardcodes `research` and never reads the key. That is deliberate — the lane's default
    describes the TASK SHAPE, not the box — so the doc now says so instead of overclaiming.
  - `docs/OPERATOR-GUIDE.md`'s profile table and the repo's own `CLAUDE.md` still called
    `general` "the default"; both corrected to "fallback" with the resolution order.
  - New tests: `TestAdvertisedToolsReflectsProfileNarrowing` and
    `TestAdvertisedToolsUnchangedByGeneral` — the latter pins the "byte-identical for an
    unseeded box" claim on the tool set AND the system prompt.
- Docs updated in the same change: `docs/systems/coding-agent.md` (new "Tool-profile seat"
  section), `docs/OPERATOR-GUIDE.md`, the regenerated `docs/tiers/*.md`, `config.example.json`,
  the `agent_run` schema text, and the ampere-6 tier note — whose previous wording called this
  "an open per-tier config question" and is now corrected to say it is closed.

## [0.69.0] - 2026-08-17
### Fixed — an UPGRADE install downloaded a gated seat's weights and never rendered it
Found by independent review of 0.68.0, reproduced against the real predicate.

- **The defect.** Step 6's render SKIP test had no invalidation signal for a gated
  seat. On a box with an existing `llama-swap.yaml`, upgrading to a version that adds
  one (0.68.0's `qwen3.5-4b-agent` on `ampere-6`) ran: Step 5 **downloads 2.9 GB** →
  Step 6 **SKIPs** (yaml exists, no leftover tokens, path fine) → Step 8 SKIPs (config
  exists). The installer printed all-OK while the yaml never gained the seat and the
  agent lane silently kept the previous, measurably weaker planner. The whole feature
  was a no-op on the upgrade path — on exactly the tier it was measured for.
- **Why the existing guards missed it.** Adding the seat's `__TOKEN__` pair to the
  staleness regex does NOT help: `Render` refuses to emit a leftover token, so a
  rendered file can never contain one — that regex only detects hand-edited files.
  This needs a CONTENT probe, the same shape as the existing `GGML_VK_VISIBLE_DEVICES`
  probe (which exists because a vulkan render without the device pin is stale).
- **The fix.** A `$gatedSeats` table (model id → does this tier enable it) drives one
  extra SKIP condition: a gated seat the tier enables but the rendered yaml does not
  contain means that yaml predates the seat, so re-render. Covers `qwen3.8-27b` as
  well as `qwen3.5-4b-agent` — the same latent gap existed for the Q38 gate, it simply
  was not load-bearing there yet (its tiers were newly declared, with no prior render).
- **Deliberately NOT probed: the 26B agent.** `gemma-4-26b-agent` is legitimately
  absent from the `win-cpu` and `win-cuda-resident` templates while those backends
  still set `include_26b`, so probing for it would force a re-render on every install
  run. The probe is only sound for EXPLICITLY gated seats, where `Render` refuses a
  gate set against a template lacking the entry — so gate-on provably implies the
  entry exists in a current render.
- **Behavior-verified** against the real predicate under Windows PowerShell 5.1, with a
  negative control: current render → SKIP stays true (no churn); simulated pre-0.68
  render → SKIP true BEFORE the fix (the bug, reproduced) and false AFTER; the same
  file with the gate OFF → SKIP true (tiers that do not enable a seat are unaffected).

### Added — the measured reasoning invariant is now guarded on BOTH templates
`TestQ354BSeatKeepsItsMeasuredReasoningInvariant` renders `llama-swap.linux-cuda.yaml`
AND `llama-swap.win-cuda.yaml` with the gate on and asserts the seat block pins neither
`--reasoning off` nor `${common}`.

The existing assertions live in `setup/render.tests.ps1`, which renders for the HOST OS,
and CI pins that job to `windows-latest` — so every one of them covered the WINDOWS
template only. **Proven, not assumed:** injecting `--reasoning off` into the LINUX seat
block leaves `render.tests.ps1` reporting `ALL PASS` while the new Go test fails on its
`linux-cuda` subtest. The `ampere-6` reference box is a Linux server, so the unguarded
template was the likelier deployment target for the regression that halves this tier's
agent recall (67% -> 28%, n=3, zero spread).

The two checks are a non-redundant pair: folding the seat into `${common}` does not put
the literal `--reasoning off` in the rendered block (`${common}` is a llama-swap runtime
macro the renderer never expands), so only the second check catches that edit.

### Fixed — the ampere-8 strip assertion could not catch an unsubstituted token
`setup/render.tests.ps1` gated on `-notmatch 'q354'`, but in `__Q354B_ALT__` the
character after `354` is `B`, a word character, so `q354` finds no boundary and a
leftover token would pass. ampere-6 already had a `__Q354B_` check; ampere-8 now does too.

### Added — a Go regression test for the `include_qwen35_4b` refusal
`TestIncludeQ354BOnAnEntrylessTemplateIsRefused`, mirroring the Q38 tripwire. Nothing
in Go locked this refusal before, so a change weakening it would have made the gate a
silent no-op while the installer still downloaded the weights. Uses
`win-cuda-resident`, which defines `qwen3.8-27b` but NOT `qwen3.5-4b-agent`, so it also
proves the two gates are independent. Mutation-verified: forcing the `definesModel`
guard false still COMPILES and turns the test red with `got <nil>`.


## [0.68.0] - 2026-08-17

### Added — the `ampere-6` agent seat: Qwen3.5-4B, the first MEASURED sub-27B agent-seat decision

The 6GB tier's agent planner was the workhorse by default (`offload-e4b`, via the
workhorse chain) because no sub-27B agent-capability data existed anywhere in the house.
It exists now: 15 candidates across families were served on the reference RTX 3050 and
scored on TWO task shapes with FABRICATION as the disqualifying axis. **Qwen3.5-4B
(UD-Q4_K_XL) is the only candidate strong on BOTH** — 67% extraction recall against the
incumbent's 50%, and 4/5 on search+reason where the higher-scoring 2B collapses to 1/5.

- New profile field **`include_qwen35_4b`** mirrors the `include_qwen38` mechanism
  end-to-end: `servingtmpl.Params.IncludeQ354B` strips the model block, its matrix var
  and its `__Q354B_ALT__`/`__Q354B_AND__` set membership together; `Render` refuses a
  tier that sets the flag against a template with no `qwen3.5-4b-agent` entry (rendering
  a config without the seat while the installer downloads its weights is the
  silent-capability-loss failure the refusal exists to end); `install.ps1` gains the
  pinned `model-qwen35-4b` download behind the same STRICT JSON-boolean gate.
- `ampere-6` sets the flag and seeds `config_seed.agent_model = "qwen3.5-4b-agent"`.
  Only the AGENT seat moves — `offload-e4b` remains the resident tier and cascade
  workhorse.
- The seat renders into **both** CUDA templates (linux + windows): a tier is a hardware
  class, so a capability cannot exist on one OS and not the other.
- **The download does NOT ride the `OFFLOAD_WITH_FAMILY` gate.** At 2.9GB it is not in
  the class that gate exists to skip, and on this tier it is the only thing between the
  agent lane and a planner measured at 50%.

**Two configuration facts are load-bearing and MEASURED — do not "tidy" them away.**

1. The seat writes its llama-server flags **explicitly instead of using `${common}`**,
   because `${common}` pins `--reasoning off` and this is a thinking model. Measured at
   the deployed ctx 32768: **67%/67% under llama.cpp's default reasoning, 28%/44% with
   `--reasoning off`.** Folding this entry into `${common}` — the obvious future cleanup
   — would silently halve the tier's agent quality, so `render.tests.ps1` now asserts
   both that the seat omits `--reasoning off` and that it does not use `${common}`.
2. No sampling flags. The agent loop sends `temperature: 0` on every request
   (`internal/agent/client.go` `wireReq` — the field has no `omitempty` and is never
   assigned), so a server-side `--temp`/`--top-p` is inert on this lane.

### Fixed — stale `ampere-6` row and an incomplete `agent_model` rule in the operator guide

The per-profile table still described `ampere-6` as resident `gemma4-e2b` at ctx 16384
with "q8_0 **mandatory** for 16K on 6 GB" — all three contradicted by the tier's own
measured 2026-07-26 pass (resident is `offload-e4b`, ctx is 32768, and the mandatory-q8_0
claim was measured FALSE). The neighbouring paragraph also described `agent_model` as
purely *derived* from `resident_tier`, which was already incomplete for every seeded tier
and would have been actively wrong for `ampere-6` after this change, whose resident tier
equals its workhorse.

## [0.67.0] - 2026-08-17

### Added

- **CPU proof farm** (`internal/grounding/proofs.go`, memory-frontier R2-10) — two
  deterministic validators needing no GPU and no model. **Its own precondition reassigned the
  first slot:** R2-10 said to check whether `extract` is GBNF-constrained and, if so, spend
  that slot on citation/path-existence instead. It is (`gbnf.Object(fields)`), so a
  JSON-validity validator could only ever pass. The two slots are therefore `PathsExist` (a
  path-shaped value must exist on disk — the source cannot tell you a copied-from-a-stale-doc
  path is wrong, only the filesystem can) and `CitedSpans` (a quoted span must appear
  *contiguously* in the source — a quote assembled from words that each appear separately
  passes per-value grounding while being invented).
  - **`Applicable` is distinct from passing.** A validator that found nothing of its kind
    reports `OK() == false`, so "0 failures" over 0 candidates can never be read as a clean
    bill of health.
- **Context pager instrument** (`internal/agent/contextpager.go`, R2-13) — **instrument only.**
  Records evicted payload hashes and whether identical content is re-fetched later in the run,
  and reports its own gate verdict (<10% re-fetch closes the whole pager family for free, and
  also proves compaction discards the right things). No eviction store, no paging, no
  retrieval — building those first would be building the thing the measurement exists to
  justify. Hashes and sizes only, never payloads.
  - A run that evicted **nothing** reports `insufficient_data`, never a confident 0% — the
    failure that would close the item on a question it never asked.
- **Decision-path golden tests** (`decisionpath_golden_test.go`, R2-11) — the narrowed form
  that survives the objection which killed round 1's Golden-State Fixtures. CUDA decode is
  non-deterministic here, so fixtures asserting model OUTPUT are flaky by construction; these
  assert **decisions** (defer classification, gate reachability in both directions, the
  reliability sample-floor boundary, grounding verdicts, and the division of labour between
  grounding and the proof validators). Each case asserts a *relation*, not a hard-coded hash —
  a hash fixture must be regenerated on every legitimate change, and regenerating a fixture is
  indistinguishable from silencing it.

### Fixed

- **Quoted-span extraction mispaired quotes.** A minimum length *inside* the regex
  (`"([^"]{3,})"`) made the engine skip a short quoted fragment and pair its closing quote
  with the NEXT fragment's opening quote — capturing the text *between* quotes and flagging it
  as a fabricated citation. Pairing is now decided by adjacency and the length filter applied
  afterwards, with a regression test.

## [0.66.1] - 2026-08-17

### Fixed — the structured re-pack must not think (found by LIVE end-to-end testing, after 0.66.0 merged)

**What was broken:** on the DEFAULT idle-local agent path, a finished and correct answer was thrown
away whenever the agent seat is a thinking model. `repackStructured` (`internal/pipeline/agenttask.go`)
re-packs the agent loop's final text into the contract's `output_schema` with one GBNF-constrained
completion on the same seat. A THINKING chat template (Qwen3-class) emits that grammar-constrained
output into `reasoning_content` and returns `content` EMPTY — so the re-pack failed, retried, failed
again, and the run deferred as `abstention` with `output failed schema: invalid json: unexpected end
of JSON input`, discarding a result the loop had already produced correctly. Qube's own agent seat
(`qwen3.8-27b`, the Leg-1 winner) is a thinking model, so this broke the quality-first default path.

**The trigger is the grammar, not the budget.** Measured live against `http://127.0.0.1:11436`, same
request, temp 0, `max_tokens: 512`:

| seat | grammar | len(content) | len(reasoning) |
|---|---|---|---|
| `qwen3.8-27b` | none | 73 (valid JSON) | 326 |
| `qwen3.8-27b` | GBNF | **0** | 67 |
| `gemma-4-e4b` | none | 85 | 0 |
| `gemma-4-e4b` | GBNF | 67 (valid JSON) | 0 |

The obvious wrong hypothesis was falsified first: at 512 / 1024 / 2048 / 4096 `max_tokens` the seat
returns the identical completion (`completion_tokens=140`, `finish_reason=stop`), so
`agentRepackMaxTokens = 512` was never the problem.

**The fix:** the re-pack now sends `chat_template_kwargs: {"enable_thinking": false}` alongside the
grammar. The re-pack is a mechanical shape transformation over text the loop has already finished
reasoning about — it should never think. Validated on both seat types: `qwen3.8-27b` + grammar +
the flag returns 67 chars of parseable JSON in `content` (control with grammar alone: 0 chars), and
`gemma-4-e4b` + grammar + the flag is identical to its own control, so the flag is harmless on a
non-thinking template and rides every re-pack rather than a seat guess we cannot make. Two
alternatives were measured and rejected: `reasoning_format: "none"` leaks a literal `<think>` prefix
into `content` (not valid JSON), and a `json_schema` `response_format` works but abandons the house's
gbnf seam and still burns 326 reasoning tokens.

**Why no unit test caught it:** they all fake the seat. The interaction lives in the model's own chat
template, which a scripted httptest server does not have — only a real seat could produce it. The
regression test added here encodes the live shape (a fake seat that answers EMPTY content for a
grammar completion with thinking on, and valid JSON when the flag is present) so the defect cannot
return.

- `internal/llamaclient`: `Generate` gained a variadic `...GenOption` and `WithoutThinking()`
  (`thinking.go`). It serializes as `chat_template_kwargs: {"enable_thinking": false}` and is
  OMITTED ENTIRELY when not requested — a pointer field with `omitempty`, pinned byte-for-byte by
  `TestGenerateWithoutOptionOmitsChatTemplateKwargs`, so every existing call site's wire payload is
  unchanged. A variadic option rather than a tenth positional parameter or a `GenerateNoThink` twin:
  no existing call site changes, and this package already carries three near-duplicate `Generate`
  methods that a second axis of boolean variants would multiply.
- `docs/systems/fleet-node.md`: the `structured` field row now documents the non-thinking re-pack.


## [0.66.0] - 2026-08-17

### Added

- **Reliability bands** (`loupe`, memory-frontier R2-14) — per-(task, tier) success/defer/
  escalate counts, with a rate published only above a 20-sample floor. **Report half only:
  the routing half is deliberately not built.** At this call volume most cells hold well under
  a sample a day; smoothing would make noisy cells look confident, and a mis-ordered
  escalation rung is a quality regression on a quality-first stack. Cells below the floor
  report their count and `insufficient_data` — never a rate. Against the live ledger:
  **15 cells measured, 32 suppressed**, which is exactly the thinness that killed the routing
  half.
- **Failure atlas** (`loupe`, memory-frontier R2-16) — defer classes histogrammed with a
  per-month recurrence and a **self-stated verdict**, so the gate ("a non-obsolete class
  recurring >= 5/month") cannot be quietly reinterpreted later. Obsolete classes are excluded
  from the gate but still **reported with their counts**, so the exclusion is auditable rather
  than a silent filter.
  - **The exclusion pattern had to be measured, not assumed — twice.** The first version
    matched `"exceeds the available context size"`, which never fires: **the ledger truncates
    reasons**, so the stored string is cut mid-word to `"(10532 tokens) exceeds the availa"`.
    It reported *0 obsolete* against a ledger holding **12**. Matching `"tokens) exceeds"`
    survives truncation.
  - **And the opposite direction is the more dangerous one.** `context deadline exceeded` /
    `context canceled` are Go HTTP timeout errors, not context-*window* overflow. A loose
    `"context ..."` pattern would classify them obsolete and silently drop a **live** failure
    class out of the gate. Four such rows exist in the live ledger across three tiers; a test
    guards that direction specifically.
  - **Obsolescence is evidenced, not asserted:** all 12 occurrences are on `gemma-4-26b`,
    dated 2026-07-23/24, at 8.6k-11.4k tokens — before the cascade seats moved to 131k
    windows. Checked by tier, date and request size.

## [0.66.0] - 2026-08-17

Multi-node sub-agent delegation. Released as 0.65.0 rather than 0.63.0 because a
concurrent session shipped 0.63.0 and 0.64.0 from the same repo while this branch
was in adversarial review; the entries below were written across that review cycle.

### Fixed — delegation lane, round-7 adversarial review (the prose is preserved for the CALLER, not for acceptance)

A seventh round confirmed the logic clean with a full state-transition table and mutation-verified
that round 6's new end-to-end test binds. Every finding is **wording only** — no predicate,
condition or assertion changed, and no behavioral rule moved. The round is two shapes: round 6
replaced one false justification with another, and it missed the sites where the wording it existed
to eliminate mattered most.

- **The replacement justification asserted something the code does not do.** Round 6 explained the
  preserved `output` on the `structured re-pack unreachable` shape as "so the delegator's text-verb
  acceptance reads it" — it never does. `evalAcceptance` is reached only under `if !wire.Deferred`
  on both placement paths (`runLocal`, `runRemote`), every re-pack failure branch returns
  `deferWire`, which sets `Deferred`, and `TestRunLocalDeferSkipsAcceptance` pins that a deferred
  result is skipped on purpose — running checks over an answer that was never claimed manufactures
  verification failures. The true reason is stronger for the argument round 6 was making: the prose
  is preserved because it is **published to the caller** in the result's `output` field, which is
  exactly what `TestDelegationEndToEndRepackUnreachableIsLostWork` asserts. Corrected at all eight
  sites carrying the claim — the five round 6 introduced (`internal/delegate/run.go`,
  `docs/FLEET-NODE.md`, `docs/systems/fleet-node.md`, `internal/mcpserver/integration_test.go`, this
  changelog) and the three that predate it (`internal/pipeline/agenttask.go`, the `output` field row
  in `docs/systems/fleet-node.md`, `internal/pipeline/agenttask_test.go`) — each now stating the
  negative outright, so the claim is not re-derived a third round running.
- **The round-6 rewording missed the predicate line itself.** `run.go`'s comment directly above
  `lost := pr.Result.Deferred && BrokenStackDefer(...)` still read "this subtask produced NOTHING",
  160 lines below the `Summary.LostToStack` doc that round 6 corrected — the literal wording the
  round existed to remove, in the same file. Same defect in the doc comments of `delegateIsError`'s
  own table test (`internal/mcpserver/delegate_test.go`). All now say "delivered no usable result:
  the contracted output never arrived", and say outright that this is not "no bytes".
- **This changelog's round-6 entry miscounted and overclaimed.** It said "Six places" and listed
  seven paths, omitting `docs/systems/fleet-node.md`, which the same commit also reworded — eight
  files carry the change. "All six sites now say what is counted" was falsified by the two sites
  above. Both corrected.
- **The round-5 entry still repeated the falsehood it documents as corrected.** Its
  `Summary.Infrastructure` bullet said the flag conflated a successful local placement with "a defer
  that produced NOTHING" — in the same bullet whose later sentence notes that round 6 corrected
  exactly that wording. Two occurrences were fixed there and a third left six lines earlier.

Test coverage, same round: `TestRunContractSideGateRejectionIsNotABrokenStack`'s ctx-fit row
asserted only the substring `"context"`, which both the scoped and the unscoped ceiling wording
contain — so mutating `if unadvertised > 0` to `>= 0` in `contractIneligible` (always the scoped
phrasing) left the whole tree green. The healthy-fleet row now pins the unscoped "the roomiest
agent-enabled remote advertises 4096" wording, mirroring the `wantCtxFit` assertion its mixed-fleet
sibling already carries, and that mutation is red.

### Fixed — delegation lane, round-6 adversarial review (say what the flag counts, and name both causes)

A sixth round found no Criticals and confirmed by mutation that the round-5 fixes bind. Its one
Important finding, and two below-bar ones, are all the same shape: **a justification that describes
something the code does not do.** No behavioral rule changed except where noted.

- **`lost_to_stack`'s stated premise was false for a reachable member of the set it counts.** Eight
  files (`internal/delegate/run.go`, `wire.go`, `internal/mcpserver/mcpserver.go`, `main.go`,
  `docs/systems/coding-agent.md`, `docs/systems/fleet-node.md`, `docs/FLEET-NODE.md`,
  `docs/OPERATOR-GUIDE.md`) defined it as "the subtasks that PRODUCED NOTHING … came back EMPTY",
  but the predicate is
  `deferred && (infrastructure|config)` — and one infrastructure defer carries a **populated**
  `output`: `structured re-pack unreachable` fires after the agent loop has FINISHED, and
  `agenttask.go` keeps `wire.Output` set on every re-pack failure branch so the CALLER still
  receives the loop's answer. So a run that returned real prose was counted as lost and flagged
  `isError`, under a comment saying it came back empty. **The predicate is correct and unchanged;
  the wording was wrong.** A contract carrying an `output_schema` asked for a mechanically checked
  deliverable, and prose with no `structured` is not one — the contracted output genuinely did not
  arrive, and a calling model must not merge an unchecked answer as if the schema had passed. Those
  eight files now say what is counted ("delivered no usable result: the contracted output never
  arrived"), and the `infrastructure` rows of both defer-class tables state outright that `output`
  may be populated on this class. Pinned end to end (only the seat faked) by
  `TestDelegationEndToEndRepackUnreachableIsLostWork`. (Round 7 finished the sweep: the `lost`
  predicate's own inline comment in `run.go` and the `delegateIsError` table test still carried the
  old wording — see below.)
- **The ctx-fit sentence was suppressed rather than merged on a mixed fleet.** With one lane silent
  and every ADVERTISED lane genuinely too small, `contractIneligible` returned "" — so
  `noEligibleRemote`'s both-causes path never fired and the operator was told "set `agent_ctx_tokens`
  on node-C", fixed it, and only discovered on the NEXT run that the contract needs ~13096 against
  the 4096 the others advertise. Suppression was the safe reading of a real hazard (the old phrasing,
  "the roomiest agent-enabled remote advertises 4096", is a fleet-wide MAX claim that implies the
  silent node is smaller — a ceiling authored on behalf of a node that published none). The sentence
  is now **scoped instead of suppressed**: "every remote that DID advertise a ceiling tops out at
  4096", spoken only over lanes that sent a number, so both true causes reach the operator in one run
  and neither is a claim about the silent box. The class is untouched — a silent lane still makes it
  the loud `config`, never `contract`.
- **The internal rationale for the per-lane rule named a path that cannot happen.** `run.go`,
  `docs/systems/fleet-node.md` and the round-5 test's own comment motivated it with "a peer predating
  the agent lane / the mixed fleet a staggered rollout produces" — but such a peer sends no
  `agent_enabled` either, decodes as `AgentEnabled:false`, and is filtered out of `lanes` before any
  ceiling is considered, so it can never reach that branch. The genuinely reachable producer is a
  node running the lane with `agent_ctx_tokens` unset: `AgentLaneAdmissible` gates on
  `fleet_agent_enabled` + a resolvable planner seat + a safely reachable listener, never on a
  ceiling, and health advertises whatever is configured (0 included) — which is also the state the
  round-5 fixture actually builds (both nodes `agent_enabled: true`). The operator-facing message was
  already accurate and is unchanged.

### Fixed — delegation lane, round-5 adversarial review (an unknown ceiling is not a small one; a lost subtask is always loud)

A fifth round found both remaining defects in the same seam round 4 restructured — the boundary
between "the node side is fine" and "the caller is at fault" — and both were cases where a
statement about ONE node was quietly extended to cover another.

- **The ctx-fit guard was FLEET-WIDE where its own doc said per-lane, so an unadvertised ceiling
  still produced a quiet contract verdict — and a fabricated claim about the silent node.** The
  round-4 fix rejected the ctx-fit contract class when `roomiest == 0`, but `roomiest` is a
  fleet-wide MAX: one node advertising a real ceiling supplied it for every peer advertising none,
  so a mixed fleet (`agent_ctx_tokens` 4096 and unset) published `defer_class: "contract"`,
  "the roomiest agent-enabled remote advertises 4096", exit 0 — the operator sent to rewrite a
  contract when the fix was to set one field on the silent box. Worse, the sentence asserted a
  ceiling for a node that had published none: docs define an absent `agent_ctx_tokens` as
  **unknown**, not small, and it may be a 128k machine — the same authoring-a-claim-on-a-node's-
  behalf defect as the invented 404 denial two rounds earlier. This is a reachable state because
  the agent lane is admitted without any ceiling, so an opted-in node whose operator never set the
  `omitempty` field advertises the lane and nothing else (round 6 corrected the original wording
  here, which credited it to a peer predating the lane — such a peer sends no `agent_enabled`
  either and never reaches the ceiling logic at all). The rule is
  now per lane: any lane with no advertised ceiling yields the LOUD `config` verdict and the reason
  NAMES those nodes, and the ctx-fit contract branch requires that EVERY lane supplied a real
  number to be too big for. The round-4 test covering this was itself vacuous — it exercised only
  the all-zero fleet and passed with the guard deleted; it now carries the mixed-fleet row and
  asserts that no ceiling verdict is spoken over a fleet with a silent lane.
- **A subtask genuinely lost to a broken box was silenced on the MCP surface whenever a sibling
  succeeded.** `Summary.Infrastructure` conflates a local placement that SUCCEEDED while the fleet
  was down with a defer that DELIVERED NO USABLE RESULT — its contracted output never arrived
  (which is not the same as no bytes) — and the round-4 `isError` rule gated on
  `Succeeded == 0` — justified only by the first, but silencing the second too. Two subtasks, one
  finished and one eaten by a dead llama-server, returned to the calling model as a clean tool
  call, while `local-offload delegate` exited NON-ZERO on the identical run: the two surfaces
  disagreed, and the quiet one belonged to the caller with no exit code to read. **New
  `summary.lost_to_stack`** counts exactly the subtasks that delivered no usable result because of
  the stack — the contracted output never arrived (published, `omitempty`, so a healthy run is
  byte-identical; round 6 corrected the original "came back empty" wording, which was false for the
  `structured re-pack unreachable` shape) — and `isError` is now
  `failed > 0 || lost_to_stack > 0`. The original motivation is untouched — a fleet-down run that
  delivered every subtask is still a quiet success on MCP — and the surfaces now agree wherever
  work was lost. (`deferred > 0 && infrastructure > 0` is NOT a safe substitute: a contract-classed
  defer beside a fleet-down local success satisfies it with nothing lost.)
- **Eight "since 0.63.0" claims in shipped docs, plus the spec's acceptance item, were falsified by
  the 0.65.0 renumbering** — and main shipped a real, unrelated 0.63.0, so a reader following them
  landed in the wrong changelog section. Corrected across `docs/FLEET-NODE.md`,
  `docs/systems/{fleet-node,coding-agent,offload-pipeline}.md`, ADR 0023 and the spec, along with
  the `pre-0.63` spellings of the same boundary. Main's own 0.63.0 section is untouched.

### Fixed — delegation lane, round-4 adversarial review (quiet classes now require positive evidence)

A fourth review round found that every previous round's new defects clustered in one place: the
loud/quiet classification, which encodes "whose fault is this" and grew a new wrong case with each
added nuance. This round applies a governing rule instead of another patch — **default to loud; a
result may be classed quiet (contract / abstention / budget) only when the quiet explanation is
POSITIVELY ESTABLISHED, meaning every configured remote answered and the node side is demonstrably
fine. Absence of evidence about the fleet is never evidence the caller is at fault.** The classifier
is now two independent verdicts (`contractIneligible` and `nodeSideVerdict`) composed by that rule,
where an empty node-side class is the single positive statement that makes a quiet class honest.

- **A caller-contract property masked a totally dead fleet.** The contract check ran ahead of the
  probe verdict, so a contract merely missing `output_schema` — optional in the `agent_delegate`
  input schema and legal for local runs — reported `Summary{Infrastructure:0}` and exit 0 while
  every remote was refusing connections; the identical state WITH a schema correctly reported
  infrastructure. A regression from the previous round. Both verdicts are now computed
  independently; when both are true the reason names both and the class is the loud one.
- **A node-side misconfiguration was blamed on the caller.** `agent_ctx_tokens` is `omitempty` and
  documented as "0 = not advertised, set it when opting a node in", i.e. an operator fix on a box —
  yet a node advertising 0 made a 30-token goal report `defer_class: "contract"`, "the contract
  needs ~3102 tokens and the roomiest remote advertises 0", quietly, at exit 0. No contract can
  clear a 0 ceiling because `specReserve` alone is 3072. Ctx-fit is contract-side only when some
  lane advertises a ceiling a contract could actually clear.
- **Genuine wire failures were filed as the model failing the schema.** `decodeGenResult` returns
  the decoder's error after `client.Do` already succeeded, so no `*url.Error`/`net.Error` is in the
  chain: a captive-portal 200 with an HTML body, a connection cut mid-body, and a proxy 429 all
  read as abstention at exit 0. A non-JSON body from something claiming to be llama-server means
  something else answered — that is infrastructure, and is now classed as such.
- **The failure message asserted a 404 denial that never happened.** Any "answered but never owned"
  case printed the 404 sentence, so a node that returned only 503s was reported as having DENIED
  holding the job, with zero re-dispatches — the same authoring-a-claim-on-the-node's-behalf defect
  as the round-3 fix, reappearing inside the message that fix wrote.
- **A job-LOSING node read as a budget defer.** The 404 arm recorded no history and healthy answers
  now clear the poll error, so a node that dropped the job twice and then timed out reported
  "ran out of clock" at exit 0 with nothing saying it lost the work. Re-dispatches are carried into
  the defer reason and class it infrastructure.
- **TOTAL ledger loss published as NO loss** (a nil ledger skipped the counters entirely, so the
  worst outcome was byte-identical to a healthy run), **the poll-log shape cap dropped counts and
  never printed shapes past the cap**, and **MCP `IsError` fired on runs where every subtask
  succeeded** (a successful local placement taken while the fleet was down set the flag, telling
  the calling model that completed, schema-valid, acceptance-passing work had failed).
- **Two more tests were vacuous** and are now assertions of the behavior their guards produce
  rather than of what they avoid; the residency latch publishes its fail-closed answer from a
  `defer`, so a panic in the probe seam can no longer freeze residency or park every waiter.

### Fixed — delegation lane, round-3 adversarial review

Two fresh-context reviews of the previous round's fixes found several of them reintroduced or
half-did the thing they were meant to fix. All reproduced before being fixed.

- **A poll `404` is a DENIAL, and was being treated as acceptance.** The 404 arm set the same
  "the node answered" flag a live `running` answer sets, so a poll deadline landing inside the
  bounded re-dispatch window (≤2 404s) took the ANSWERED path and published
  `{deferred:true, class:"budget", reason:"…node accepted the job but did not reach a terminal
  state"}` stamped with that node's id and seat, at exit 0 — the exact fabrication the previous
  round's fix exists to forbid, reached through a different door (reproduced: polls=2,
  dispatches=3, `Summary{Deferred:1}`). Reachable in production because `timeout_sec` has no
  lower bound and a re-dispatch burns up to 60s. The flag is now split: reachability
  (`sawNodeAnswer`, message text only) versus OWNERSHIP (`sawJobOwned`, set only by a `200` whose
  state is accepted/running/done/error). Defer-vs-failure gates on ownership, so a run whose
  every answer was a 404 — or a 503 — lands in `summary.failed` with the status named.
- **The agent lane's advertisement and admission still keyed on different inputs.** The previous
  round unified them on one predicate but left `BuildRequest` deriving its own listener answer
  with `ConfigLoopbackListen`, while health used the RESOLVED listener. With
  `fleet_listen: "0.0.0.0:18811"`, `--listen 127.0.0.1:18811` and no token, health advertised
  `agent_enabled:true` with `agent` in `supported_task_types` while dispatch answered
  `400 unsupported task_type "agent" (supported: )`. `BuildRequest` now takes the resolved
  listener as an argument, the dispatch handler consults `AgentLaneSafelyReachable` instead of
  re-implementing condition 3, and the parity test gained the mismatch row its four original
  rows could not express (all four left `fleet_listen` unset). The docs that claimed a single
  predicate over the resolved listener are corrected to describe what the code now does.
- **A poll error was sticky forever.** One 503 followed by fifty clean `running` answers still
  ended `defer_class: infrastructure`, exited non-zero, and quoted an error fifty polls stale. A
  healthy answer now retires it; the failure history survives in the poll summary line.
- **`route=auto` discarded the placement class.** `route=remote` classes a totally unreachable
  fleet `infrastructure` and exits non-zero; `route=auto` with a busy local GPU threw the
  identical verdict away, so a fleet down for a week read green forever. A local placement taken
  while every configured remote failed its health probe now counts into `summary.infrastructure`.
  Ordinary idle-local placements and "they answered and did not qualify" stay quiet.
- **Every non-200 from the seat was filed as broken infrastructure.** The structured re-pack
  treats a Generate error as transport only when it genuinely is one (`*url.Error` / `net.Error`
  / 5xx). A 4xx ("context length exceeded", an uncompilable grammar) and a 200 with zero choices
  are the box ANSWERING, so they are abstentions under the stable `output failed schema:` prefix
  — previously they told the operator a machine was broken when the fix was a smaller context or
  a flatter schema. The inverse is fixed too: the transport flag is sticky across the one retry
  (a 5xx then a wrong-shape retry stays `infrastructure`) instead of last-wins, and the reported
  error is the transport one. A PARENT cancellation mid-re-pack — a `*url.Error` like any dial
  failure — is now its own `budget` shape instead of accusing the node.
- **Caller-contract gate rejections were classed as a broken stack.** Three of the placement
  gate's five conditions are properties of the CALLER'S contract (no `output_schema` — legal per
  `Validate` and legal locally; `depth != 0`; a token estimate no advertised ceiling can hold),
  and all three produced `summary.infrastructure ≥ 1`, so `--route remote` exited non-zero and
  told the delegating model a node was broken on a run where every node was healthy. New
  `defer_class: "contract"`, excluded from `BrokenStackDefer`, with a reason naming the property.
- **Unbounded logging, in the commit that added `sync.Once` for exactly this hazard.** Both poll
  failure arms fired once PER POLL (~120 lines per subtask in production, ~1000 for an 8-way
  fan-out at a dead node) and the per-remote probe warning fired once per remote PER SUBTASK. Now:
  the first occurrence of each distinct failure shape, one summary line per job reporting the
  totals, and one probe warning per base per run.
- **Telemetry loss is counted and published.** "This run's corpus rows are LOST" read identically
  whether 1 of 8 or 8 of 8 failed, and the MCP caller — this lane's primary consumer — could not
  learn it at all. Atomic counters, one end-of-run "N of M rows lost" line, and
  `corpus_rows_lost` / `ledger_rows_lost` (`omitempty`) on the published summary.
- **The loud-exit contract existed only on the CLI.** `handleAgentDelegate` never set `IsError`,
  so `summary.failed > 0` and `summary.infrastructure > 0` both returned as successful tool calls.
  Both now flag the call while leaving the JSON body — summary and every per-subtask reason —
  untouched.
- **An inert assertion from the previous round.** `TestAgentDispatchAdversarialBodies`' leftover-
  job-dir check never ran a comparison: all nine rows failed BEFORE materialization, so the jobs
  root never existed and `ReadDir` always errored. A row that fails AFTER materialization (a doc
  name no filesystem can hold) was added and the assertion made explicit — mutation-verified:
  deleting the `os.RemoveAll` it guards now fails the test.
- **`RefreshAgentResidency` bypassed its own single-flight** (it called the refresher without
  taking the latch the refresher clears unconditionally), permitting a duplicate probe and a
  staler answer overwriting a fresher one. It now claims the latch, or waits for the probe
  already running.
- **`delegateExitErr` named only the first non-zero class**, so a run with both failures and
  infrastructure defers hid one of them until the next run. Both counts are reported.

Multi-node agent delegation: the fleet gains an **agent lane** — a delegator hands a
self-contained contract (goal + inline context docs + output JSON Schema + acceptance
checks) to a fleet node on the operator's tailnet, the node runs it with its own local
read-only agent loop, and the delegator mechanically verifies the result before anything
counts as done. Quality-first by construction: an idle local box always runs the work, and
no transcript ever crosses the wire. Decisions recorded in ADR 0023.

### Added

- **Wire contract v1** (`internal/core/agentwire.go`): `AgentContract` /
  `AgentWireResult`, tolerant reader (unknown fields ignored; `schema_version` skew and
  size/count caps refuse loudly), MaxSteps/TimeoutSec clamps (12 / 900s), and the
  machine-checkable acceptance DSL (`contains:` / `not_contains:` / `regex:` /
  `min_items:<field>:<n>` / `nonempty:<field>`) — evaluated by the DELEGATOR before merge,
  with unfalsifiable checks rejected at parse.
- **Per-model seat endpoints** (`seat_endpoints` config): any model seat can resolve to a
  remote OpenAI-compatible base over the tailnet with zero job machinery
  (`llamaclient.BaseFor`), guarded twice — `netguard.TailnetURL` at config load (loopback,
  `100.64.0.0/10` literals, dotless MagicDNS names, house-tailnet-zone hosts only) and the
  resolve-and-pin `SafeTransport` dial gate on every request (DNS-rebinding defense).
- **Busy-aware cascade remote lanes** (`cascade_remote_lanes` config): the DAILY lane's
  invisible failover — while the local machine-wide GPU lease is held
  (`delegate.LocalBusy`), a cascade text call (summarize/classify/extract/triage) whose
  model a configured lane roster-serves routes to that lane instead of queueing behind the
  local card; logged per rerouted call. Distinct from `seat_endpoints`' static
  always-remote pin, and quality-identical by construction: the lane must serve the SAME
  model id/alias (verified by a 30s-cached alias-aware roster probe, fail-closed to
  local), so routing never changes WHICH model answers, only WHERE. Same double guard as
  seat endpoints: `TailnetURL` at load naming the key, `SafeTransport` at every dial.
- **Agent-lane bearer auth** (`fleet_auth_token`): agent dispatches and polls of
  agent-created jobs require the token (SHA-256 + constant-time compare); a tokenless
  non-loopback listener refuses agent dispatches 403 at ack and withholds the task from
  advertisement. v1 scope is the agent lane ONLY — every media path stays tokenless and
  byte-identical (pinned by test); whole-fleet enforcement is a recorded follow-up.
- **Node-side `agent` fleet task**: contract materialized to a job-scoped context dir, run
  through the same read-only `agent.Build` loop as `agent_run` (no write/run/fetch/github
  tools, no delegate tool), wall deadline from `timeout_sec`, structured re-pack via one
  grammar-constrained completion (one retry), depth derived `max(1, wire)` on arrival. A
  defer is a `done` job carrying `deferred:true` — never `error`.
- **Health agent fields + placement gate**: `agent_enabled` / `agent_seat` /
  `agent_ctx_tokens` / `agent_seat_resident` (cached alias-aware roster probe,
  fail-closed), consumed by `delegate.Place` — idle-local-always-wins, and a remote is
  eligible only when enabled + resident + the conservative token estimate plus the
  3072-token loop reserve fits the advertised ceiling + the contract carries a schema +
  requester depth 0.
- **Defer classes** (`defer_class` on `AgentWireResult`): every defer says WHY in a
  machine-branchable word — `abstention` (the model answered wrongly), `budget` (step or wall
  ceiling), `infrastructure` (the stack failed: agent build, loop transport, an unreachable
  structured re-pack), `config` (no seat resolvable, seat unserved, unknown profile, no
  eligible remote). The delegator counts `infrastructure` + `config` defers into
  `summary.infrastructure` and the `delegate` CLI **exits non-zero** on it: a node with a dead
  llama-swap defers every subtask, and that must not read as a clean run. Additive and
  `omitempty` — a pre-0.65 node's empty class means *unknown*, never abstention.
- **Delegator surfaces**: MCP `agent_delegate` (registration gated on
  `agent_delegation_enabled`, so tools/list is byte-identical when off; summary-first
  response) and the `local-offload delegate` CLI verb. Both accept `context_paths` inlined
  DELEGATOR-side under `read_root` confinement (≤128 KiB/file). Job protocol: `agd-` ids
  delegator-minted, 202-reack idempotent re-dispatch, poll deadline = timeout + 60s grace.
  Every subtask writes a ledger row (`task=agent_delegate`) plus a full
  contract/placement/result/verdict line under `delegation-log/` — the standing
  small-model agent-task corpus.
- **`contracts/` template library**: four canned contract shapes (docs-drift-scan,
  bench-log-digest, schema-extraction, research-digest) matching the `--contract` file
  shape, with the field and DSL guidance in `contracts/README.md`.
- **Docs**: ADR 0023 (tailnet auth, never-cloud carve-out, quality-first placement, hop
  limit, N-node door); contract/result wire tables in `docs/systems/fleet-node.md` and
  `docs/FLEET-NODE.md`; the operator enable recipe for both roles plus the honest
  context-budget table (at an 8k seat, ~2–4k tokens of practical contract content) in
  `docs/OPERATOR-GUIDE.md`; delegation surfaces in `docs/systems/coding-agent.md`.

Diagnostics hardening (adversarial silent-failure review of this arc, same release):

- A poll that never reaches the node is no longer laundered into a defer. The delegator
  tracks the last poll error and whether the node ever answered: a node that answered but
  never finished defers honestly (`poll deadline after <d>: node accepted the job but did not
  reach a terminal state`, plus the last error when one exists), while a node that **never**
  answered — dial refused, connection dropped, unusable body — is a `summary.failed` failure
  with a non-zero exit. Previously any of those produced a fabricated
  `{deferred:true, reason:"poll deadline"}` stamped with the chosen node's id and seat, and
  the CLI exited 0. Unrecognized poll answers (5xx, unknown states) are captured and logged
  instead of falling through the switch unread.
- "No eligible remote" now names the real cause instead of always blaming the gate: health
  probe errors are collected and logged, and the reason distinguishes *no remotes configured*
  (class `config`), *every remote failed its health probe* (listed; class `infrastructure`),
  and *remotes answered but none passed the gate* (class `config`).
- Telemetry failures are loud once per run: a `delegation-log` corpus write failure or a
  ledger open/write failure warns (once, never per subtask) instead of being discarded — a
  delegator writing nothing looked identical to one writing everything.
- Probe failures that only ever produced silence now log: the cascade remote lane's roster
  probe (once per TTL window per base, so a lane that never engages is diagnosable), the node
  agent-seat roster probe (which fails open by design), and the node's `agent_seat_resident`
  residency probe (the one field that stops every remote placement).
- A deferred LOCAL result no longer has acceptance checks run against it — matching the remote
  path, so a defer can never be reported as a verification failure.

Correctness hardening (adversarial code + test review of this arc, same release):

- **A schemaless contract no longer defers on the default local path.** The node-side run
  called the structured re-pack unconditionally, so with no `output_schema` the re-pack's
  `json.Unmarshal(nil, …)` errored and a finished, correct answer was thrown away as
  `deferred: output failed schema: … unexpected end of JSON input`. That broke `route: local`
  and `route: auto` on an idle GPU — the quality-first path the design centers on, and the one
  place a schemaless contract is explicitly legal. The re-pack is now SKIPPED when no schema
  was asked for: `structured` stays empty, the run is a plain success.
- **Health can no longer advertise an agent lane dispatch would refuse.** The advertisement
  keyed on `fleet_agent_enabled` alone while the ack-time guard keyed on enabled + seat +
  (loopback-or-token), and the two read different notions of "loopback" (`fleet_listen` vs the
  resolved listener). A node with the lane on, a non-loopback listener and no token therefore
  published a placeable lane, the delegator placed on it, and every dispatch 403'd into
  `summary.failed`. Both sides now call one exported predicate, `fleetnode.AgentLaneAdmissible`,
  over the RESOLVED listener; advertisement-equals-admission is asserted across all four
  (listener, token) combinations.
- **Hostile context-doc names are refused instead of silently losing data.** A doc named `NUL`
  (or `CON`/`PRN`/`AUX`/`COM1-9`/`LPT1-9`, any case, with or without extensions) passed
  validation on Windows, "wrote" successfully, and read back EMPTY — a context doc vanishing
  with no error anywhere. Names with a trailing space or dot bypassed the duplicate guard the
  same way (`notes.md ` and `notes.md` are two Go strings and one Windows file), so one doc
  could shadow another. Both shapes are now rejected, and duplicates are detected on a
  normalized key (trailing space/dot trimmed, case-folded).
- **The cascade remote lane's roster probe now rides the dial guard.** It was the one outbound
  path on Go's default transport — proxy live, no dial-time address check — which made ADR
  0023's "re-checked at every dial" false for it: `TailnetURL` admits a dotless MagicDNS name
  on shape alone, so only a per-dial check can prove where it still resolves. New
  `swapclient.FetchRosterGuarded` (used for LANE probes only; the node's own loopback
  llama-swap keeps the plain reader).

Test coverage this release adds, for gaps that let the above through:

- The first test that crosses a package boundary: MCP handler → `delegate.Run` → real HTTP →
  the real fleetnode mux → a real pipeline → `runAgentTask`, faking only the leaf llama seat.
  It asserts the node a result NAMES is the node health ADVERTISED and the node that RAN it,
  that the quarantine holds end to end (a marker emitted only inside a remote tool turn never
  appears in the caller's bytes), and that a dropped delegator token is a 401 FAILURE, not a
  defer. Renaming the job wire's `data` field — invisible to `internal/delegate`'s and
  `internal/pipeline`'s own suites, which each fake the other side — fails it.
- The agent-seat residency tests now drive the health path only, so the background probe
  production actually uses is covered; deleting that one line previously left the suite green
  while, in production, `agent_seat_resident` would be false forever and every delegation would
  silently fall local.
- Fan-out coverage (8 subtasks: distinct job ids, submission order, the concurrency bound, one
  well-formed corpus line each), adversarial agent-lane HTTP with the lane ON (malformed and
  oversize contracts through the mux; bearer-header edge shapes including the case-insensitive
  scheme), and the `delegate` CLI's disabled-role refusal.

Config keys added: `seat_endpoints`, `cascade_remote_lanes`, `agent_ctx_tokens`,
`fleet_auth_token`, `fleet_agent_enabled`, `agent_delegation_enabled` — all default
off/empty; every existing path behaves byte-identically when they are absent (pinned by
test).

Deliberately parked to v2: the in-loop `delegate_subtask` tool. v1's surfaces are the MCP
tool and the CLI; the hop limit holds structurally meanwhile — no delegate tool is
registered for any caller, and a wire contract executes at derived depth ≥ 1.

## [0.65.0] - 2026-08-17

### Added

- **Agent-loop prefill instrument** (`internal/agent/prefillstats.go`, memory-frontier
  T2-B *re-aimed*). Aggregates the SERVER's own prefill accounting across an agent run
  and reports it on `Result.Prefill` and on the `local-agent` stderr summary: KV reuse
  %, tokens prefilled, prefill ms, and per-step averages.
  - **Why it replaced what T2-B originally proposed.** T2-B was ranked "highest
    leverage/effort in track" — restructure task prompts so the BM25 exemplar injection
    stops mutating the FRONT of consecutive prompts and defeating llama.cpp's prefix
    reuse. Then the ledger was measured: the text cascade's prompts have a **median of
    177 tokens**, and the *entire* 34.5-day prefill is ~412 s, i.e. **~12 seconds per
    day**. Eliminating 100% of it saves 12 s/day, against a restructure carrying a
    stated accept-rate-parity (quality) risk. Falsified by arithmetic.
  - **But the ledger never sees the agent loop**, which re-sends a long system prompt
    plus tool schemas plus a growing transcript on *every step* — structurally the one
    workload here with a large repeated prefix, and entirely unmeasured. So the
    instrument ships first and decides whether the expensive change is worth making.
  - The per-call half already existed (`Completion.Serve`); what was missing was any
    **aggregation** — `Serve` was consumed only by the token calibrator and
    `compaction_eval`, so the question could not be answered from a real run at all.
  - `KVReusePct` is `CacheN/(CacheN+PromptN)`, **not** `CacheN/PromptN` — the latter
    excludes the cached tokens from its own denominator and runs past 100%.
  - **Unmeasured is not zero.** A backend reporting no `timings` yields
    `basis: "insufficient_data"` and a `null` rate, never a fabricated 0% reuse. This
    is the same defect class as the `duplicate_input_rate` fix in 0.62.x, where an
    unmeasured 0 would have closed a gate that was never measured.
  - **Always-on, and therefore mutex-guarded.** `--serve` shares one `*Loop` across
    concurrent HTTP handlers — the race that gated the token calibrator OFF by default.
    An instrument that must be switched on measures a special mode rather than real
    traffic, so this one is unconditional and pays for that with a lock.

## [0.64.0] - 2026-08-17

### Fixed

- **Audio and video cache keys are now content-addressed** (`internal/mediahash`,
  memory-frontier T2-A2). The image path has always keyed on the loaded bytes
  (`"img:"+sha256hex(...)`); audio keyed on (path, size, mtime) and video on the **path
  string**, unhashed. Both failed in two directions: a file replaced at the same path could
  produce a **false hit** — serving the previous file's transcript or description — and an
  identical file at a second path always missed, which is the reuse an artifact cache
  exists to capture. New config `media_hash_max_full_bytes` (default `0` = always hash the
  whole file).
  - **Migration:** every existing audio and video cache entry is invalidated once, by
    design — those entries were keyed on an identity that could be wrong.
  - **A TOCTOU window remains and is DETECTED, not prevented.** The digest and ffmpeg are
    two independent opens of a path, so ordering alone cannot close it — hashing first
    merely transposes which side is misattributed. The file is re-`stat`ed **after** the
    consuming read; on a difference the call is treated as unidentifiable and nothing is
    stored. The detector is (size, mtime), so a same-size overwrite inside one mtime tick
    is invisible to it — 1–2 s granularity is common on FAT/SMB/FUSE and Drive-backed
    mounts. This narrows the window; it does not eliminate it.
  - **No identity, no cache.** `mediahash.Digest` returns an **error** rather than a
    synthetic key. An earlier design returned `media:staterr:<hash(path+error)>` — a *path*
    key — so a transient read failure wrote a durable entry a different file at that path
    later hit, reintroducing the exact false hit this change removes.
  - **A bypassed cache is observable**: `cache_bypass` on the ledger row names why, so a
    permanently unidentifiable input is no longer byte-identical in telemetry to an
    ordinary cold miss.
  - **Cost of an unidentifiable input:** it is never cached, so it re-runs the model on
    every call and writes a fresh nonce-salted `.srt`/`.txt`/`.segments.json` triple.
    Nothing reaps `media_dir`, so a file on a persistently flaky mount accumulates three
    files per invocation — a deliberate trade (a wrong cached transcript is worse than a
    repeated one), but a real cost.
- **Corrected the 0.63.0 embed-memo justification, on both surfaces.** The feature was
  documented as existing because "the shadow-label drain re-embeds the same stored inputs
  on every run and re-scores the same reference summaries". Reading the code refutes all
  three parts: `shadow.Drain` is **destructive**, so each run consumes a fresh item set;
  `label.go` calls `Similar` on summaries derived from *that item's own* output, so there
  is no shared reference set; and the drain's `Embed` path is itself gated on
  `knn_prefilter_enabled`. The genuine repeat sources are the request-path pre-filter and
  within-run repeats inside one drain. **With `knn_prefilter_enabled` at its default of
  `false` the memo is close to inert** — correct, cheap and free when idle, but its value
  is gated on a separate decision. No code changed; the claim did.

## [0.63.0] - 2026-08-17

### Added

- **Embed memo** (`internal/embedmemo`, memory-frontier T2-C) — a bbolt store that
  memoizes embedding vectors by exact input bytes plus the embedder id. Embedding is a
  pure function of (model, text). Consumers are the kNN pre-filter on the request path and
  the shadow-label drain; exemplar selection is **not** one (`internal/exemplars` retrieves
  lexically and contains no embedder). The
  larger payoff is the swap, not the compute: the embedder carries `ttl=300` like every
  other seat, so the first embed after an idle gap pays a ~1–2 s cold load, and a memo
  hit skips the HTTP call entirely. New config: `embed_memo_enabled` (default on),
  `embed_memo_path`, `embed_memo_max_entries` (50000 ≈ **640 MB on disk** — bbolt costs
  ~12.8 KB/entry once fill factor and overflow pages are counted, not the ~6 KB the
  payload arithmetic suggests, and bbolt never shrinks the file after a prune),
  `embed_memo_epoch`.
  - Keys are **exact bytes, never normalized**. Normalizing for a higher hit rate would
    let two different texts share a key and return a vector computed for the other one —
    a silent correctness bug in a semantic quantity, not a cache miss.
  - Vectors are stored as verbatim `float64`; a hit is bit-identical to what the
    embedder returned. Narrowing to `float32` would perturb cosine scores near a
    decision threshold.
  - Every failure degrades to a plain live call. Embedder errors are never stored, and a
    malformed record reads as a miss rather than an empty vector (an empty `[]float64`
    scores 0 against everything, which is indistinguishable from a legitimate "nothing
    similar" answer).
  - `embed_memo_epoch` is the manual lever for the one case the id cannot see: a model
    re-quantized or re-trained and republished under an unchanged name.
- **Memo counters on two surfaces.** `local-offload loupe` reports the memo read-only
  (short timeout, so it never contends with a live server), and `offload_status` reports
  the live counters from the process that owns the handle. Both distinguish
  "unavailable" and "never consulted" from a measured zero.

### Changed

- **The agent loop now shares the result cache** (memory-frontier T2-D). "Recordless"
  bundled two unrelated guarantees: (a) in-loop offloads must not pollute the savings
  ledger, and (b) they must not touch the result cache. (a) is a real accounting
  invariant and is unchanged; (b) was collateral, and made the loop re-run the model on
  byte-identical input. New `NewInLoopPipeline` / `NewInLoopOffload` keep the nil ledger
  and share the cache; the MCP front door and the `local-agent` CLI use them.
  `NewRecordlessPipeline` / `NewRecordlessOffload` are unchanged and still used where no
  shared state is wanted (the shadow-labelling flywheel, prompt A/B arms).
- **Cache participation is a property of the pipeline, not of `RunTier`.** The
  shadow-labelling flywheel drives `RunTier` on the main pipeline — which has an open
  cache — to evaluate counterfactual tiers; a hit there would grade a stored answer
  instead of the tier, and a write would fill the store with counterfactual results.
  Only `NewInLoopPipeline` opts in.

### Fixed

- **`RunTier` now has its OWN cache keyspace** (`cacheKeyForTier`), disjoint from `Run`'s.
  Its key was previously a hand-rolled `cache.Key` call with the pre-0.62 shape: keyed on
  the primary model rather than the tier actually run, and missing the template tag. It was
  dead code (nothing read or wrote that key), so nothing was ever mis-served — but reviving
  it for T2-D reinstated both defects on a live path. Routing it through `Run`'s
  constructor fixed the ingredients but not the collision: with `exemplar_shots` at its
  default of 0 the two paths computed the **same key** whenever the pinned tier was the
  primary model, so `Run` cached an E2B answer, `RunTier` refused it and overwrote the
  entry with the workhorse's, and the two ping-ponged one key.
  - **User-visible consequence:** an in-loop agent offload no longer reuses a cascade
    answer, or vice versa. That sharing was never sound — a pinned tier must get *that
    tier's* output — but it does mean the two populate the cache independently.

## [0.62.1] - 2026-08-16

### Fixed

- Image-prompt refiner system prompt now states the no-new-quotes rule its own
  guard enforces — verbatim: If the user's prompt contains no "double-quoted"
  text, your output must contain no quotation marks at all. Without it, span-rule-only
  instructions taught Gemma-class refiners to wrap the prompt's SUBJECT in
  quotes, and the added-quote guard then rejected nearly every span-less
  refinement — `imagegen_refiner_model` silently self-cancelled on exactly the
  prompts it exists for. Measured on GenEval2 span-less prompts (2026-08-16):
  gemma-4-12b 2% -> 87% refine rate with the sentence (gemma-4-31b measured 0%
  without it); quoted-span prompts unaffected (90-100% both before and after).
  The prompt now states the rule the guard enforces; no guard behavior changed.

## [0.62.0] - 2026-08-16

The tier-doctrine parity pass (operator doctrine, 2026-08-16): flagship seats trickle
to every tier they FIT; all tiers are first-class citizens; same-VRAM twin-arch tiers
are kept in capability parity (the arch split is a build-time concern only).

### Changed — profiles, templates, installer (all 15 tiers reviewed, notes updated)

- Agent seat seeded EXPLICITLY per tier (no tier seeded one before; derivation pointed
  16GB+ tiers at the reasoning-off cascade seat, measured 0% as an agent):
  `qwen3.8-27b` on blackwell-2x16/48/72 (the measured Leg-1 winner — it existed in NO
  template and NO download manifest until now); the validated thinking-on
  `gemma-4-26b-agent` (12/15 D1) on ampere-16/blackwell-16/volta-16, dual-gpu and
  amd-rdna3-dgpu; workhorse-by-design below that. blackwell-32 stays derived
  (27B + 26B cannot both sit all-resident in 32 GB — recorded open bake-off item).
- blackwell-48/72 inherit the 32GB-class frontier seats they always fit (PROJECTED,
  selftest-gated): krea2 image single-card, LTX-2.5 video at FULL 1920x1088 (the 39.11GB
  loaded transformer fits without the 2x16's pooled-resolution compromise), qwen3.8-27b
  resident. blackwell-32: krea2 + ltx25 at 1280x704, and its vision seat fixed from
  e4b-vision (worse than the 16GB tiers — pure drift) to qwen3-vl-8b.
- New `include_qwen38` profile field mirrors the include_26b mechanism end-to-end:
  internal/servingtmpl gains the `__Q38_ALT__`/`__Q38_AND__` tokens + block strip with
  the same leftover-token refusal; install.ps1 gains the model-qwen38 (+ renamed mmproj)
  pins (HF tree-API oids) and the download gate.
- `gemma-4-26b-agent` entries (26B weights, `--reasoning on`, no new download) render on
  win-cuda / win-vulkan / win-dual-cuda / win-dual-blackwell / linux-cuda, riding the
  26B's include gate; matrix membership alternates with the 26B (never double-loads).
  cuda-resident deliberately gets none (all-resident premise).
- Hardening from the three review rounds: the harness renderer is VERSION-GATED
  (a stale installed exe silently dropped newer profile fields — re-installs now rebuild
  on mismatch, and the render step refuses a wrong-version renderer outright; ADR 0021
  updated); `Render` refuses `include_qwen38` on a template with no qwen entry; missing
  gated weights (26B/qwen gguf+mmproj) now WARN with the full file list (the installer's
  warning filter no longer truncates payload lines); the qwen download rides the
  `OFFLOAD_WITH_FAMILY` lean-install gate and its profile flag is strictly boolean
  (fail-loud BEFORE the 18.8 GB download); tier docs partition media vs non-media seed
  keys (a text-only seed no longer renders as "media bindings" — dual-gpu and amd-gcn
  pages corrected) with the media key set pinned against mediacap's route keys.
- Twin-arch parity (ampere-8≡blackwell-8, ampere-16≡blackwell-16≡volta-16) verified
  field-identical and now STATED in each profile's notes; the single-`8gb`/`16gb` tier-id
  merge is recorded as a follow-up migration (classifier + installed.json compat).
  dual-gpu documented as the heterogeneous/other-pairs catch-all (not a 2x16 duplicate);
  amd-rdna3 vs -dgpu kept separate (iGPU/UMA vs discrete differ in every load-bearing
  field). docs/tiers regenerated.

## [0.61.0] - 2026-08-15

The NIM shadow-label oracle (Seat Frontier plan Leg 7, item 1): the self-learning
flywheel can now judge queued shadow items against a frontier remote model instead of
the local escalation tier, producing higher-trust counterfactual labels for confhead
and router training. Explicit, opt-in, provenance-tagged — the local cascade and its
GBNF grammar path are untouched, and NIM calls still never enter the savings ledger.

### Added — `shadow-label --oracle nim`

- `internal/nimoracle`: the free-text→`core.Result` adapter the seam analysis called
  out as the non-trivial core. Per task type it builds a JSON-only instruction prompt
  (classify label set / triage yes-no-unsure / extract schema / summarize) and
  parser-extracts + validates the remote reply into exactly the Data shape the shadow
  judges (`pipeline.AnswersAgree`, `grounding.Check`, the B2 summarize judge) consume.
  Completion budgets are sized per task: summarize gets the local tier's
  384+160/bullet headroom ON TOP of `nim_max_tokens` (which reasoning models
  largely spend thinking); other tasks run at the flat `nim_max_tokens`. Malformed oracle answers (label outside the set, decision
  outside yes/no/unsure, non-object extraction, truncated reply) are rejected as
  un-judgeable — never recorded as disagreement.
- `WrapRunTier` dispatch: only the ESCALATION slot goes remote; the B1 E2B
  router-counterfactual rerun keeps its local provenance. A nil local RunTier panics
  at wiring time (it would silently starve the B1 feed).
- Provenance: ledger label rows whose judgment derives from the remote oracle (the
  confhead row and the A4 E2B-entry router row) carry `"oracle":"nim"`
  (`ledger.Entry.Oracle`, omitempty — historic rows parse unchanged). B1 rows stay
  untagged. The A4 kNN append is SKIPPED under the nim oracle — `knn.Row` has no
  provenance field, so remote-derived accept labels would mix irreversibly into the
  local substrate; the B1 kNN feed (local rerun) is unaffected.
- Fail-loud batch semantics (the queue drain is destructive): `nimoracle.Preflight`
  runs BEFORE the drain using the SUMMARIZE shape — the structurally largest
  instructed reply — at its real budget, through the same adapt path items take, so
  missing/invalid key, dead endpoint, wrong model id, or a model that truncates or
  ignores the JSON-only instruction abort with nothing drained. The paid preflight
  is skipped when no queue (or crashed-drain claim) file has content. Per-cause skip
  counters (`remote/chat_err/truncated/empty/unadaptable/unpromptable` + the first
  transport error verbatim) print in the run summary — unadaptable (paid replies
  rejected) is deliberately separate from unpromptable (free skips of unserved
  tasks) — and a zero-label run over a non-empty drain exits non-zero after the
  router/kNN summary lines print.
- Oracle prompts mirror the local tiers where the judge compares fields: summarize
  instructs the same 1-2 sentence `"summary"` + up-to-N `"bullets"` split (default
  5, explicit values clamped to >=1) as `tasks.buildSummarize`; extract stays
  neutral about groundedness (no verbatim coaching of `grounding.Check`, which IS
  the label); classify without a label set is rejected before any paid call;
  inline reasoning is handled before JSON extraction (the text after the LAST
  `</think>` is taken as the answer; a reply with an unclosed `<think>` span is
  un-judgeable). The preflight also fails when the probe's completion spent more
  than the flat `nim_max_tokens` (classify/triage/extract run at that budget and
  would truncate mid-run), and names a reasoning-only reply (empty content with
  `reasoning_content`) as its own condition.
- Config reuse: endpoint/model/timeout/max-tokens come from the existing
  `nim_endpoint`/`nim_model`/`nim_timeout_sec`/`nim_max_tokens` keys; the API key
  comes from `$NVIDIA_API_KEY`/`$NGC_API_KEY` only (never config), with the same
  hosted-endpoint guard as `nim`.

## [0.60.0] - 2026-08-14

The 32GB-class VIDEO seat: LTX-2.5 22B distilled, pooled, with joint audio. Executes the
measured 2026-08-12 three-way verdict (Seat Frontier plan Leg 3): LTX-2.5 beat Wan 2.2 and
MiniMax-H3 on every axis at once — 1920×1088 @ 24 fps WITH a generated soundtrack in 247 s per
5 s clip, vs Wan's silent 1280×720 @ 16 fps in 1134 s (which also OOMs at its shipped vv).

### Added — `ltx25` render family

- `render/wf-ltx25-i2v.mjs`: the official `video_ltx2_5_i2v` template's two-pass joint-AV
  recipe (half-res base pass + ×2 latent spatial upscale + refine; fixed distilled sigmas;
  dual CFG 1/1; euler_ancestral) as a param-driven API-format builder. Bench-proven deltas
  baked in: the gemma4_e2b prompt-enhancer branch is DELETED (the harness planner does prompt
  expansion), the convrot int8 transformer pairs with the conv video VAE, duration/dims are
  computed in the builder (seconds×fps+1; /32-aligned; stage 1 at half resolution), and both
  sampler seeds are caller-pinned (`seed`, `seed+1`).
- Pooled DiT loading (32GB-pool doctrine): `videogen_pool_vvram_gb/pool_compute/pool_donor`
  route the 20 GB transformer through DisTorch2 ratio mode, the shape proven for the krea2
  image seat (same `--disable-dynamic-vram` launch requirement until MultiGPU #191).
- Config: `videogen_family` (`""`/`wan22` = Wan, unchanged; `ltx25` = this family) plus
  `videogen_transformer/video_vae/audio_vae/latent_upscaler/fps`. Pipeline threads them to
  `comfy-video.mjs --model ltx25`; a per-request `model` param still wins over the family.
- `comfy-video.mjs`: `--model ltx25` with family-native defaults (1920×1088, 121 frames,
  24 fps) and the new binding flags.
- blackwell-2x16 tier seed flipped to the ltx25 family (Wan weights stay on disk as the
  `--model wan` fallback); tier docs + media-generation docs updated.

## [0.59.0] - 2026-08-14

The 32GB-class image seat: Krea 2 Turbo, pooled. Chosen by the operator's blind bake-off
verdict (2026-08-14: won both decided pairs against Qwen-Image-2512, including the
text-rendering probe; two ties) under the house pool doctrine (operator-reaffirmed the same
day: quality/doctrine outrank speed — speed is reported as fact, never a decision axis).

### Added — `krea2` render family

- **`render/wf-krea2.mjs`** (+ 7-test suite): the official `image_krea2_turbo_t2i` template
  shape, cross-checked against the bake-off graphs recovered from /history — NO
  ModelSamplingAuraFlow shift node (unlike qwen-image), regular `EmptyLatentImage`, turbo
  recipe baked in (8 steps / cfg 1.0 / euler / simple), `CLIPLoader type "krea2"` with the
  family-standard `qwen3vl_4b_bf16` encoder and the shared `qwen_image_vae`. Dims /16.
- **Pooled loading (32GB-pool doctrine)**: `poolVvramGb > 0` loads the DiT through
  ComfyUI-MultiGPU's `UNETLoaderDisTorch2MultiGPU` in RATIO mode — the shape proven on the
  reference dual-GPU box; the byte-expert allocation string is deliberately unused (the
  node's reservation half reads only the post-'#' segment expert mode leaves empty, so
  expert silently collapses to one card). `0` = plain `UNETLoader` (single-GPU fleet
  shape). Pooled safetensor serving additionally requires launching ComfyUI with
  `--disable-dynamic-vram` until ComfyUI-MultiGPU #191 lands — a per-box launch concern
  carried by the `COMFY_EXTRA_ARGS` seam, documented at the config keys.
- **`--family krea2` in comfy-render.mjs**: mirrors the qwen-image branch — steps/cfg
  travel together or not at all (a half-override of a distilled recipe renders burned-out
  mush), "builtin" VAE is a config error for a split-file family, template-native 1024
  default dims (proven at 2048×1024 in the bake-off).
- **Config/Go plumbing**: `imagegen_pool_vvram_gb` / `imagegen_pool_compute` /
  `imagegen_pool_donor` → `imagegen.Model.PoolVvramGB/PoolCompute/PoolDonor` →
  `--pool-vvram/--pool-compute/--pool-donor` (zero/empty = no flags, existing machines
  byte-identical; the `TestImageModelFromConfig` reflect drift-guard covers the new
  fields).

### Review hardening (two-specialist round, 2026-08-14)

- **CRITICAL (both reviewers, empirically proven): the pool flags were collected by
  `comfy-generate.mjs` and silently DROPPED by `batch-jobs.mjs`'s whitelist** — the hop
  BOTH the single and batch paths route through — so `imagegen_pool_vvram_gb: 12`
  produced a plain single-GPU `UNETLoader` on every harness render while all 249
  per-hop tests stayed green (each hop was tested; the composition never was). Fixed
  STRUCTURALLY: `batch-jobs.mjs` now exports `JOB_PARAM_FLAGS`/`SHARED_BINDING_FLAGS`
  and `comfy-generate.mjs` DERIVES its collector from them — one source of truth — plus
  a composed-chain test that feeds every exported flag through `jobArgs` and asserts
  emission (mutation-proven red on a list regression).
- **Load-time binding-trap warnings** (`warnImageGenBindingTraps`): every one of these
  renders pixel-plausible output with zero client-side symptom — krea2 steps/cfg
  half-bound (the pair guard would defer every render); pool devices without vvram, or
  negative vvram (single-GPU with pool keys in the config); pool vvram under a family
  with no pooled loader; pooled seat without `--disable-dynamic-vram` in
  `COMFY_EXTRA_ARGS` (DynamicVRAM un-pools every safetensor DisTorch2 load while the
  allocation banner still prints — MultiGPU #191).
- `.gguf` bindings on `--family krea2` are rejected LOUD at dispatch (no GGUF loader is
  wired for the bf16 seat) instead of dying as an unnamed ComfyUI node error.
- Recorded, no code: a lone per-request `steps` on a zero-bound krea2 seat defers by
  design (pair guard, identical to qwen-image; the live seat binds 8/1.0 explicitly so
  request-steps compose with the bound cfg); `Number("")` on a hand-typed empty
  `--pool-vvram` renders single-GPU silently — manual-invocation-only edge.

## [0.58.0] - 2026-08-14

TO-3 (plan 2026-08-07): tier-aware repacking at the escalation boundary — a climbed-to tier
re-reads the ORIGINAL source against its own served window instead of inheriting the entry
tier's lossy cut.

### Added — the escalation and reasoning tiers re-read the source

- **`internal/pipeline/tierpack.go`**: when a request climbs past the entry tier, the callee's
  input is re-packed from the retained original against `n_ctx(callee) − task max_tokens −
  reserve − tokenized(scaffold)` — `n_ctx` probed live per model (`/props`, the agent window
  probe, cached with a 10-min TTL; failures cached too so a dead route costs one probe per TTL,
  not one per climb) and every count measured by the callee's own served tokenizer
  (`internal/tokclient`, the TO-4 path — `Count` gains its first production callers). A source
  that fits the bigger window arrives WHOLE; an over-window source is cut token-exact, head+tail
  (2/3–1/3, mirroring the entry trim's bias), on piece boundaries backed off to rune boundaries
  (the LO-13 mojibake class is unrepresentable). The terminal reasoning tier repacks the same way.
- **Observability**: new `tier_pack` field on `core.Meta` and the ledger row — empty on entry
  rows; `token-exact (full source)` / `token-exact (cut K/N tokens)` / `entry-inherited (<why>)`
  on climbed rows. The fail-open is recorded, never inferred (the TO-4 review rule).
- **Recursion half of TO-3**: verified structural, nothing to remove — the chain walk is bounded,
  the confhead gate excludes the escalation tier, `attemptReasoning` has no escalation gate, and
  no "escalate tool" exists anywhere in this codebase (the plan text's phrasing maps to an
  architecture this cascade never had; recorded in the nightshift-4 notes).

### Changed

- The cascade cache key is now the ORIGINAL input — the logical request's identity — not the
  entry packing. Under-cap inputs (the overwhelming majority) key byte-identically, so cache
  continuity holds; an oversized input re-keys once, and the old key COLLIDED two different
  originals sharing a trim (a wrong cache hit) — strictly more correct.
- Per-tier prompt rebuilds re-apply the Phase 6 exemplar injection (`withExemplars`, factored
  from the entry path) so a climbed tier does not silently lose its few-shot shots.
- Fail-open contract: any probe/tokenize/build failure on the repack path leaves the climbed
  tier's input BYTE-IDENTICAL to the entry packing (the pre-TO-3 behavior), with the reason in
  `tier_pack`.
- The agent half of TO-3(a) — tool-schema tokens reserved out of every loop budget — shipped in
  0.57.0 (`specReserve`) because TO-4's fit verdict required it; recorded here for the plan's
  paper trail.

### Review hardening (three-specialist round, 2026-08-14)

- **CRITICAL — think-budget reservation**: the reasoning tier generates with
  `MaxTokens+reasoningThinkBudget` (512), but its repack budgeted bare `MaxTokens` — a
  ~384-token window overshoot on the default config, on exactly the large inputs TO-3
  serves. `packForTier` now takes the callee's REAL completion request (`genBudget`).
- **View-shrink inversion closed**: a callee served with a small window could pass the
  fixed 256-token floor yet see LESS than the entry tier. The cut path now compares its
  allowance against the ENTRY view's own token count and falls open ("buys no view")
  when repacking would shrink the view.
- **Upstream-only probes and tokenize** (`ProbeUpstreamWindow`, `tokclient.NewUpstreamOnly`):
  the bare-root fallback routes answer for whatever model is CURRENTLY loaded — mid-cascade,
  the previous tier — so a stale per-model alias would have budgeted and cut one tier with
  another tier's window and vocabulary, cached under the callee's key. On a bare
  llama-server the repack now stays entry-inherited (honest; a single-model server cannot
  meaningfully repack per-tier).
- **Tokenize failures TTL-cached** per model (the doc claimed stickiness the code lacked):
  a dead /tokenize route costs one probe per TTL window, not two 60s round-trips per climb;
  the cached disposition is labeled `tokenize (cached)`.
- **Exemplars measured and stable**: shots are retrieved ONCE per request (keyed on the
  entry view) and the SAME shots decorate every tier's rebuild AND the repack's fit
  measurement — previously the injection landed after the measurement (unbudgeted) and a
  per-tier re-retrieval could silently hand a climbed tier different or zero shots.
- **Rebuild-failure restore**: a failed per-tier rebuild now restores the ENTRY prompt
  outright (retained `entryBuilt`) — previously a 3-chain rebuild failure left the
  PREVIOUS tier's packing in place while the label claimed the entry packing.
- **`input_chars` keeps entry-view semantics on every row**: it feeds the confhead
  `loginput` feature whose label stream is entry-scale; letting it follow the repacked
  view desynchronized `len_chars` and `loginput` in the same feature row (train/serve
  skew). Exemplar harvest is gated to entry rows (no sidecar bloat, no marker text as
  future few-shot content).
- **Honest under-cap label**: an input under the entry cap never probes anything, and its
  disposition now says so (`full source (under entry cap)`) instead of claiming a
  token-exact verification that never ran.
- New ADR 0022 records the decision (provenance: the operator-approved master plan);
  ADR index rows 0018–0022 registered.

## [0.57.0] - 2026-08-14

TO-4 (plan 2026-08-07): `cut_middle_turns` — token-exact whole-message history compaction
in the agent loop.

### Added — the drop rung cuts whole middle messages on REAL token counts

- **`internal/tokclient`** — the harness's real-tokenizer path: POST `/tokenize`
  (llama-swap `/upstream/{model}/tokenize`, bare llama-server `/tokenize`) with
  `with_pieces`, fail-open on any failure, piece accounting verified to reconstruct the
  input byte-for-byte before any caller may cut on it. Built once here so the escalation
  repacking work (TO-3) reuses the same path.
- **`cut_middle_turns`** (`internal/agent/cutmiddle.go`): when the loop has a tokenizer,
  the compaction ladder's whole-turn-drop rung is REPLACED by a token-exact middle cut —
  each message sentinel-indexed by byte span in the serialized transcript, spans mapped to
  real token positions via the pieces, and whole assistant+tool units dropped from the
  middle so the head (protected preamble) and tail (recent state) always survive. A
  message is never split: the mid-JSON tool-result truncation that breaks the next parse
  on small local models is unrepresentable on this path. Pinned (H8) and signal-residue
  units keep their existing drop exemptions.
- Wired identically in all drive modes (CLI single/two-tier — each tier cuts with its own
  served tokenizer — and the MCP `agent_run` front door).

### Changed

- The legacy estimate-driven oldest-first drop remains ONLY as the explicit fail-open
  fallback for endpoints with no `/tokenize`; the failure is sticky per Loop (one failed
  probe, no per-step network stalls). One truncation mechanism at a time, by explicit gate.
- Exhausted-compaction telemetry follows the yardstick that measured the transcript: the
  token-exact rung's REAL verdict when it ran (an under-counting estimate can no longer
  hide a real forced-keep overflow, and a pessimistic estimate can no longer report a
  verified-fitting request as exhausted on every step), the estimate otherwise. When the
  token-exact rung measures the reactive-retry transcript as fitting, the pin-blind
  `emergencyShrink` pass is skipped — it would destroy pinned bodies on evidence the real
  tokenizer refutes.

### Review hardening (adversarial round, 2026-08-14)

- The sticky downgrade is OBSERVABLE: `Result.TokenizerPath` reports `token-exact` vs
  `legacy (degraded: <why>)` with per-route failure detail (`tokclient` records why each
  candidate route failed), `agent_run` returns `tokenizer_path`, the CLI prints a stderr
  note. Fail-open, never fail-unobservable.
- A tokenizer failure under an already-cancelled context does not trip the sticky
  downgrade — a `--serve` client hang-up says nothing about the endpoint, and the Loop
  (and its sticky bit) lives for the process in `--serve`/`--queue`.
- `tokclient.Count` fails on a 200 JSON body without a `tokens` array instead of
  returning a confident zero; token-accounting serialization includes tool-call ids.

### Review hardening (round 2 — three-specialist pass, 2026-08-14)

- **Tool-spec reservation (`specReserve`)**: the tool-spec block ships with every chat
  request but no yardstick counted it — on a full `--allow-*` build it runs 4-6× the fixed
  512-token margin, so the token-exact `fitReal` verdict could veto the `emergencyShrink`
  last resort on a prompt the server rejects (the run died where it previously recovered).
  Every budget now reserves the block's REAL tokenized cost (measured once per Loop via the
  same tokenizer seam; conservative chars/3 estimate when no tokenizer answers; zero for
  tool-less Loops — their arithmetic is byte-identical to before).
- **Classified sticky downgrade**: a single transient failure (cold-start timeout, 503
  mid-swap, reset) no longer degrades a healthy endpoint for the process life. Definitive
  route absence (all candidates 404/405, classified by `tokclient.LastFailDefinitive`)
  still downgrades immediately; transient failures take two consecutive misses, and a
  success resets the streak.
- **Input-scaled read cap**: the flat 8 MiB response cap silently truncated `with_pieces`
  responses for large transcripts (~26 bytes/token on the wire), tripping the downgrade
  with a reason blaming the server — deterministic self-disable in exactly the
  long-transcript regime the feature targets. The cap now scales (`16×input + 1 MiB`).
- **Garbage-200 route attribution**: a candidate answering 200 with a non-tokenize body
  (interposing proxy) now counts as that candidate failing — URL-named in the reason — and
  the fallback route actually runs instead of being masked.
- **Two-tier telemetry**: `RunTwoTier` no longer drops the architect's verdicts — new
  `Result.ArchTokenizerPath` on all three return paths (each tier degrades independently),
  and both fallback paths now aggregate the architect's `CompactionsExhausted` too. The
  CLI prints per-tier degrade notes.
- **`--serve`/`--queue` visibility**: the modes where the sticky bit outlives a single run
  previously surfaced it nowhere. Both now print a once-per-process transition note, and
  every queue trace records the goal's `tokenizer_path`.
- **Contract-check honesty**: a tokenizer whose pieces do not reconstruct the transcript
  now trips the sticky downgrade with a recorded reason (previously a silent per-step
  refusal while `TokenizerPath` kept claiming token-exact).
- **Shared degrade prefix**: `agent.TokenizerDegradedPrefix` is the one contract between
  the producer and every consumer that branches on degradation (CLI notes, queue traces) —
  two independent string literals could drift and silently kill the operator note.
- Verdict-yardstick tightening: the survivors' separator tokens are now counted in the
  `fits` verdict (every asymmetry leans conservative). Docs qualify honestly that the
  estimate still decides whether the ladder ENGAGES (the reactive retry is the net for the
  under-count regime), and that `token-exact` means configured-and-not-degraded.
- Dispositions recorded without code: `tokclient.Count` stays staged (TO-3 consumes it in
  0.58.0 the same night); the post-`emergencyShrink` exhausted count may over-count in
  estimate space (honest direction — never hides an overflow).

### Review hardening (round 3, 2026-08-14)

- Read cap multiplier corrected 16×→32×: llama.cpp's real `with_pieces` entry
  (`{"id":N,"piece":…},`, ≤ ~27 bytes) exceeds 16× at the ~1 token/byte regime the cut
  targets (CJK/base64/byte-fallback), so 16× re-opened the truncation self-disable it
  claimed to close. The proving fixture now emits the real id-bearing entry shape and goes
  red at 16×.
- `specReserve` now tokenizes the WIRE serialization via `wireToolsJSON` — the same
  producer `Chat` ships (`tools` array + `tool_choice`) — instead of a marshal of
  `ToolSpec` itself, which used different keys and 34 fewer fixed bytes per tool
  (under-counting on exactly the path advertised as exact).
- The CLI degrade note now also prints on both error-exit paths (single and two-tier) —
  the run that degraded and then died on a context overflow is where the note carries the
  most diagnostic value.

## [0.56.0] - 2026-08-14

Master-plan step-4 remainder: the ComfyUI submission/timing plumbing is re-expressed through the
vendored `comfyui-pp-cli` (Node render path only; `gpu-lock.mjs` and the model-family `wf-*.mjs`
builders untouched by design).

### Added — one shared ComfyUI submission layer (`render/comfy-submit.mjs`)

Before this, six runners (`comfy-render`, `comfy-edit`, `comfy-inpaint`, `comfy-video`,
`comfy-music`, `comfy-run-graph`) each carried their own copy of the raw `POST /prompt` → poll
`/history` → `GET /view` block, and only `comfy-render.mjs` had the 2026-07-30 dead-server
watchdog — the siblings would burn their whole poll budget on a wedged server while holding the
exclusive GPU slot.

- **Submission prefers the vendored CLI** when a binary is resolvable (`COMFYUI_PP_CLI` env —
  loud-fail if wrong — then a local `tools/comfyui/bin` build, then PATH): idempotent submission
  lease (an identical in-flight graph attaches instead of double-rendering; stale/unverifiable
  leases force-resubmit = the raw path's always-POST behavior), typed accept/reject/partial
  outcomes with `node_errors` verbatim, run-row provenance, and post-render finalization that
  records the authoritative `execution_start -> execution_success` duration (printed as one
  `timing …` line; neither side ever parses the server log's "Prompt executed" text). Local
  pre-POST CLI failures (usage/config/generic) fall back to raw loudly; server-verdict codes stay
  fatal so a raw retry can never double-render.
- **No binary → raw HTTP, byte-identical** to the previous runners (same POST body with the
  per-run `client_id`, same outputs, same error surfaces). CI and binary-less fleet nodes run
  exactly this path.
- **Polling stays harness-side by design** (a surfaced CLI gap): every runner now shares the
  hardened loop — dead-server watchdog (`COMFY_DEAD_SEC`, default 240 s), suspend/resume fence,
  per-poll 30 s abort, HTTP-error-status-counts-as-answer — and `/view` retrieval stays raw for
  exact byte fidelity.
- 37 new offline `node:test` cases (spawn/HTTP doubles + one real-spawn EPIPE regression); the
  render suite is 240 tests total.

### Fixed

- `comfy-run-graph.mjs` `fetchToDir` no longer writes a non-OK `/view` body to disk as an
  "output"; it throws and lands as a typed `RUN_ERROR` defer.
- `rawSubmit` rejects a 200 `/prompt` reply with no `prompt_id` immediately instead of polling
  `/history/undefined` for the entire budget.

## [0.55.0] - 2026-08-14

TO-1 rescoped step 2 (plan 2026-08-07, measurement 2026-08-11): stop treating the model's
self-declared `security_risk` as the unattended safety gate, and put the cascade's two
confidence thresholds inside the distributions they are supposed to police.

### Added — unattended agent runs load a default structural rule table

The 2026-08-11 measurement (pooled over BOTH production agent seats, five arms, 137 effectful
calls) showed the self-declared `security_risk` annotation is a literal constant: 83/83 emitted
declarations `low`, park-gate recall **0/81** on structurally destructive calls, and emission
runs INVERSE to blast radius (`web_fetch`: 0/6 annotated). PR #87 shipped the structural rules
mechanism tighten-only and fail-closed, but `--rules` defaulted to empty, so the constant was
the only per-call gate above the capability flags on an unattended run. 0.48.0 warned about
that state (`UNGATED` note); warning an absent operator is exactly the mechanism the
measurement showed does not work.

- **`internal/agent/unattended-rules.json`** — a 25-rule default table, embedded in the binary
  (`unattendedrules.go`) and loaded by `agent.Build` for every UNATTENDED run that passes no
  `--rules` argument. Every `delete` queues for operator review (`delete *` → ask), behind hard
  write-AND-delete denies for append-only evidence (`*.jsonl`), model weights
  (`*.gguf`/`*.safetensors`) and worktree-root CI workflows (`.github/workflows/*`);
  lockfile/`go.sum` hand-edits deny (their deletes queue like any other delete); config and
  dependency-manifest writes queue. Ordinary source writes stay governed by the posture flags —
  a default that queued every source edit would only push operators to opt out. The table gates
  the write/delete tools only; file operations inside the shell/run cage are the OS cage's
  jurisdiction, and the build note says so.
- **Escape hatches, explicit only:** `--rules <path>` REPLACES the default with the operator's
  own table (replacement is what lets an operator loosen the delete catch-all — rules
  themselves only tighten); `--rules off` (`agent.RulesOff`) restores the pre-0.55.0 ungated
  posture, and the builder then emits the `UNGATED` note. An empty `--rules` can no longer
  produce an ungated destructive run.
- **Deliberately absent:** no shell-command rules (rules.go rejects `ActShell` rules by design —
  command lines are not structurally matchable; the OS cage owns shell containment) and no
  default fetch rule (the egress allowlist is itself the operator's pre-authorization).
- Scope: the CLI/queue/serve/two-tier paths (all funnel through `agent.Build`). The MCP
  `agent_run` front door grants no write/delete/shell/fetch capability and is unaffected.
- Tests (`unattendedrules_test.go`): the embedded table loads tighten-only; a delete parks on
  an unattended run WITHOUT any reliance on model-declared risk, with the fired rule as
  structured ask-queue fields; deny rules outrank the delete catch-all (order pinned);
  config/manifest writes gate while ordinary source writes do not; `--rules off` is ungated and
  announced; an operator table replaces the default. Mutation-tested: disabling the default
  load turns all four gate tests red.

### Changed — cascade confidence thresholds calibrated onto the observed distributions

Both deployed gates were DEAD by threshold: `classify_min_confidence` 0.45 vs a lowest observed
self-confidence of 0.850 anywhere, `confidence_margin_threshold` 0.35 vs a lowest observed
margin of 0.372 — below the entire support of their signals, they never fired on the confcal
probe (120 difficulty-graded items) or on 1487 production ledger rows (0 rows under either).

- `classify_min_confidence` **0.45 → 0.88.** The only observations below 0.88 anywhere in the
  probe are the two accepted-WRONG escalated rows (self-confidence 0.85, no margin available at
  the answering tier): the floor now refuses exactly the acceptances measured wrong, and fires
  on 0/97 non-escalated rows (every entry-tier emission sat at ≥ 0.90).
- `confidence_margin_threshold` **0.35 → 0.65.** The margin is the stronger signal on the same
  120 decisions (AUC 0.930 vs 0.874, 118 distinct values vs 5). 0.65 sits above all three
  observed wrong-row margins (0.382 / 0.492 / 0.618): the gate now catches 3/3 entry-tier
  errors at a 17/97 escalation rate on the deliberately hard probe — and 0/43 fires on observed
  easy production classify traffic (min margin there 0.985). Jointly the two gates address 5/5
  observed probe errors. On production triage (no error labels measured), 22/119 observed
  margin rows sit under 0.65 and would now climb one local tier — a cheap, quality-first climb;
  per-task conformal thresholds (`calibrate` / thresholds.json) still override the constant.
- The escalation-reason prerequisite for verifying this change post-hoc (`esc_source`, the
  closed seven-value set on effect/escalation ledger rows) already shipped in 0.48.0 (PR #94)
  and is verified live; no further ledger change was needed.
- **Upgrading an existing install:** `Load` overlays your config file onto the defaults, so a
  `config.json` written before 0.55.0 pins the dead constants (`0.45`/`0.35`) and keeps both
  gates inert. Remove the two keys (or set the calibrated values) to pick up the fix. The
  harness now warns at load time when either threshold sits at/below the floor of its signal's
  observed distribution — a gate that structurally cannot fire should never again be silent.

### Fixed — adversarial-review round on the above (same PR)

- The ask-queue path is now defaulted (`~/.local-offload/agent-asks.jsonl`) for EVERY
  `local-agent` run holding a mutating capability, not just `--queue` mode — the default table
  queues deletes by design, and a run without a queue denied the calls with nowhere to record
  them.
- `Policy.Decide` no longer claims "denied & queued" when nothing was queued: with no ask queue
  attached (or a failed queue write) the reason now says "NOT queued". The deny outcome was
  always safe; the record now tells the truth too.
- A matching Ask rule is recorded as the fired rule even when classify already answered Ask
  (the default posture, where an Ask rule is not strictly stricter) — previously the rule's
  severity/glob/reason never reached the ask queue in exactly the most common configuration. A
  classify Deny keeps its own more precise reason.
- `--rules off` + `--allow-write` alone now raises the UNGATED note (the default table gates
  write-new paths too, so opting out with write-only capability is a real downgrade); the
  ACTIVE note fires only when `--allow-write` grants the tools the table can actually see.
- `examples/agent-rules.json` had its `delete *` ask catch-all FIRST, which shadowed the
  table's three critical delete denies into dead code (first match wins). The catch-all now
  sits last, with the ordering rule documented in the rule itself; the embedded default table's
  loader test now pins deny-above-catch-all ordering structurally.
- Two-tier mode now prints the architect build's notes too (previously silently dropped).
- The default broker-audit path now also covers `--allow-github`-only runs (they get a worktree
  and `github_upload_file` — an outward-facing write surface that must not run without an audit
  trail); previously only write/fetch/shell/run triggered the default.
- Stale 0.45/0.35 references in pipeline test comments updated.

## [0.54.2] - 2026-08-13

### Fixed — llamaswap-pp-cli MCP code orchestration was dead for the same reason as comfyui's

`llamaswap-pp-cli` carried the identical tenant-gate defect fixed in comfyui's copy one release
earlier (0.54.1, PR #109). The generated `internal/mcp/platform_gate.go` is the same file in both
trees, so the same branch was wrong in both: `cli.VerifyMCPInvocation` returns `(nil, nil)` on
exactly one path — its first statement, `if registeredPlatformSource == nil` — meaning "this CLI
registers no tenant-gated platform source, so there is nothing to gate". `requireFreshTenantGate`
read that as a misconfiguration and answered `MCP tenant gate is not configured`.

llamaswap's spec is `auth: type: none`, so `registeredPlatformSource` is nil by construction and
**every** in-process tool hit that branch: `llamaswap_search`, `llamaswap_get`, `llamaswap_execute`
and the intents — the whole code-orchestration surface — while the 67 cobra mirrors kept working
because they carry `pp:tenant-gate: child-cli` and bypass the wrapper. As with comfyui, that made it
look like a partial failure rather than a gate bug, and no static check caught it. The fix is the
gate half of comfyui's `0004` patch, ported so the two files stay semantically identical: a
`(nil, nil)` pair now continues ungated, and a real error still denies.

The binary-budget half of that patch is **not applicable** here and was deliberately not ported.
It exists because ComfyUI's `/view` serves file bytes; llamaswap's 27 code-orchestration endpoints
all answer JSON or `text/*` (Prometheus exposition, plain-text logs), and
`isBinaryResponseContentType` returns `false` for every one of those media types, so no `_pp_binary`
envelope can be produced by this CLI. Porting it would have added unreachable code and a test that
could only assert against a fabricated response.

Two regression tests, both verified to fail with the fix reverted. The two pre-existing gate
conformance tests each replace `verifyFreshMCPInvocation` with a stub returning an error or a
session, so neither could ever reach the real function's `(nil, nil)` branch: the first new test
calls `cli.VerifyMCPInvocation` directly, and the second drives all six real in-process handlers
through the real wrapper with nothing stubbed. Both run without a live server.

Live evidence, against llama-swap v249 on `127.0.0.1:11436` with `embeddinggemma` already resident
and no unloads: the rebuilt MCP binary answered `llamaswap_search` (returned `upstream.props`),
`llamaswap_get` (`server.version` → `/api/version`), and `llamaswap_execute` on
`GET /upstream/{model}/props` with `model=embeddinggemma`, which came back with the real
`model_path`, `model_ftype: Q8_0`, `n_ctx: 2048` and `total_slots: 4` — also re-proving positional
path-placeholder substitution on the wire. A binary built from the pre-fix source returned
`isError: true`, `MCP tenant gate is not configured`, for all three.

This is a **generator-template** defect, not a per-CLI one: the emitted `platform_gate.go` carries
the wrong branch on any printed CLI whose spec declares no auth. Two independent trees have now
been bitten by it. Filed upstream on `mvanhorn/cli-printing-press`; until that lands, a reprint of
either tree silently reverts the fix.

Detail: `tools/llamaswap/.printing-press-patches/internal-mcp-platform_gate.go.md`.

## [0.54.1] - 2026-08-13

### Fixed — comfyui-pp-cli MCP code orchestration, from the first live-server run

The deferred live smokes from the comfyui wave (`tools/comfyui/LIVE-SMOKES-DEFERRED.md`) were run
against a real ComfyUI 0.32.0 server for the first time. §1 and the safe half of §2 passed, and two
defects surfaced that no fake-server test could reach.

**The tenant gate rejected the entire code-orchestration surface.** `cli.VerifyMCPInvocation`
returns `(nil, nil)` to mean "this CLI registers no tenant-gated platform source, so there is
nothing to gate" — and nothing registers one for `comfyui-pp-cli`. `requireFreshTenantGate` read
that nil session as a misconfiguration and answered every call to `comfyui_search`, `comfyui_get`
and `comfyui_execute` with `MCP tenant gate is not configured`. The 56 cobra-mirror tools were
unaffected because they carry `pp:tenant-gate: child-cli` and skip the gate, so the surface added in
0.53.0 was dead while everything around it worked — a partial outage, not an obvious one.
`cli.BindMCPClient` already read the same `(nil, nil)` as "proceed"; the gate now agrees with it.
Once a platform source IS registered the function returns a session or an error and never that pair,
so no real gate can be skipped.

**An oversized binary body was truncated into corrupt base64.** ComfyUI's `/view` returns file
bytes, which the client layer base64-wraps into a `_pp_binary` envelope. A 2.7 MB render becomes a
3.6 MB envelope against a 60 000 byte budget, and the generic bounding turned that into a `preview`
holding a base64 string cut mid-value — unparseable, and corrupt if an agent decoded it anyway. The
note attached to it advised narrowing the request with filters or `--select`, neither of which can
shrink an image. `comfyui_execute` now refuses an oversized binary envelope and names the route to
the bytes, matching what the typed endpoint-mirror path already did.

Three regression tests close the gaps, each verified to fail with its fix reverted. The existing
gate conformance tests both stub `verifyFreshMCPInvocation`, so neither exercised the real
function's no-platform-source branch; the new one calls it directly. No fake-server test had served
a binary body at all; two new ones cover the round-trip and the refusal.

Also confirmed live and recorded in the smokes file: `progress_state` is **not** among the
capabilities ComfyUI 0.32.0 serves (`features --json` reports `verdict: MATCH` with an empty
`added` list, and a real run's `status.messages` carries only `execution_start`,
`execution_cached`, `execution_success`), so the documented message set is the whole protocol and
nothing should be built against a `progress_state` assumption.

Detail: `tools/comfyui/.printing-press-patches/0004-mcp-code-orch-gate-and-binary-budget.patch`.

## [0.54.0] - 2026-08-13

### Added — llamaswap-pp-cli perfection wave 1

Ten scored backlog rows against the vendored `tools/llamaswap` CLI, plus one cross-tool defect the
comfyui wave surfaced. The organising principle throughout: a measurement command may refuse, but it
may never emit a number it cannot stand behind.

**GGUF correctness (row 9 + the GGUF-parity report).** `internal/gguf` now reads the tensor-info
table — names, dims and type tags only, never a byte of weights — and derives from it what the
metadata alone cannot say:

- **Measured bits-per-weight**, as a per-type histogram (Σ bytes / Σ elements). Block geometry is
  transcribed from the `static_assert(sizeof(block_*))` lines in `ggml/src/ggml-common.h`, and a
  ground-truth test asserts the summed tensor bytes account for 98%+ of every real model file on
  disk and never exceed it. A file whose `general.file_type` names a ggml type that is entirely
  absent from its own tensors is flagged as mislabelled; the claim is withheld for the IQ ftypes,
  whose ftype→type correspondence is not one-to-one.
- **Shard awareness** (`split.count` / `split.no`). A shard header describes a FRACTION of a model:
  on the reference box, shard 1 of the DeepSeek-V4-Flash set is 5 MiB against 90.18 GiB for the
  whole set. `gguf` resolves and sums the siblings; `fit` sums them too and **refuses (28)** when
  any member is missing, because judging capacity against one shard turns a does-not-fit into a
  fits. `split.no` is zero-based while the filename it generates is one-based; when the two
  disagree the reader says so rather than picking a side.
- **MoE total-vs-active parameters**, classified from the stacked `*_exps` tensors. Independently
  confirmed against a model that carries the answer in its name: Qwen3-Coder-**30B-A3B** measures
  30.53B total / 3.35B active. With no expert tensors present the active count is withheld, not
  estimated.
- **RoPE scaling**, so a YaRN-extended model reports its native (trained) window beside its declared
  one. DeepSeek-V4-Flash: trained at 65 536, declared 1 048 576.
- **Refusal guards** for architectures whose cache is not `n_kv_heads × head_dim × 2`: MLA
  (`attention.kv_lora_rank` / `key_length_mla` / `q_lora_rank`) and SSM/Mamba (`ssm.*`). `fit` and
  `ctx` exit 28 with the measurement that would settle it instead of applying the wrong formula.
- **`general.type`** (adapter / imatrix / mmproj identified, never sized as models), `pooling_type`
  (RANK ⇒ reranker), and the `LLAMA_FTYPE_GUESSED` bit masked and labelled `(guessed)`. The
  `llama_ftype` and `ggml_type` enums were refreshed from current `llama.h` / `ggml.h`, adding
  NVFP4, Q1_0 and Q2_0 and keeping every removed format's gap unmapped.

**Bench methodology (rows 10, 11).** `bench` reports prompt processing and generation **separately**,
each as `mean ± sample standard deviation (n-1)`, and flags a spread over 3% of the mean as
`UNSTABLE` rather than averaging two machine states into one rate. New `--depth N[,N...]` mirrors
`llama-bench -d`: tokens are prefilled before the timed window opens, the prefill is excluded from
the timing, and the observed `cache_n` is reported so a prefill that did not stick is called out as
`PARTIAL depth` instead of published as a deep-context number. New `--standard` emits the canonical
pp512/tg128 markdown row with build, hardware and provenance. Every row now carries a 38-field
**comparability key** over the llama.cpp build, host, weights and seat flags; new `bench compare`
diffs two rows and **refuses (29)** when their keys differ, naming the fields that moved, and reports
a delta inside the two rows' combined spread as noise rather than as a regression.

**New `metrics <model>`** parses llama-server's Prometheus exposition into typed telemetry against
the CURRENT upstream field names, with `--delta` sampling twice so counters are reported as windowed
rates rather than lifetime totals. `requests_deferred > 0` surfaces as a `slots_too_low` finding.
`kv_cache_usage_ratio` no longer exists upstream; its absence is reported as a removal, not a fault.

**Server-surface completeness (rows 16, 17, 22).** New `version drift` compares the llama-swap
surface this CLI was verified against with the live server (exit 25 when the server is older) and
reports **which backend answered** — llama.cpp's native router mode serves a similarly shaped
`/models`, and pointing these admin commands at one produced 404s that read as faults. Both checks
also run inside `doctor`. `ctx` and `ps` use `meta.n_ctx` from the roster as a fast path where the
server exposes it, falling back with an explicit note rather than substituting a configured value
for a measured one.

**Agent contracts (rows 3, 4).** Every non-zero exit under `--json`/`--agent` now emits one
structured envelope — `{ok, error:{code, category, retryable, http_status, message, remediation,
exit_code}}` — byte-compatible with the shape `comfyui-pp-cli` shipped in 0.53.0. Coverage is total:
cobra's pre-parse flag errors are caught by scanning argv (a parse failure aborts before flags are
bound), and the envelope moves to stderr once a command has written a result document to stdout, so
stdout is always exactly one JSON document. MCP tools with a stable typed envelope now advertise
`outputSchema` and return `structuredContent`; the schemas are reflected from the Go result structs
into `tools/llamaswap/testdata/schema/` and gated by a golden test.

**Hygiene (row 15).** `make test` is now `-race -shuffle=on -count=1` with cgo enabled, plus a
`test-fast` inner-loop target. No races were exposed.

### Fixed

- **`llamaswap_execute` could not reach any templated endpoint.** Every entry in the generated
  code-orchestration registry shipped `Positional: []string{}`, and the execute handler substitutes
  path placeholders by iterating exactly that slice — so `/upstream/{model}/props` went out as
  `/upstream/%7Bmodel%7D/props?model=…` and all **14** templated endpoints were unreachable through
  the tool. Positionals are now backfilled from the path template at init, which survives a reprint;
  an httptest substitution test pins the resolved path. Same defect and fix shape as the comfyui
  twin.
- **`bench` cold-load timing** no longer reports a load that a preceding tokenize call already paid.
- **`seat log`'s corpus acceptance gate** asserted frozen counts against a live, growing backup
  directory and went red on 2026-08-13 when a new backup landed. It now asserts floors and
  structural invariants (non-glob outlier discovery, content-addressed dedupe, label/mtime
  detection), which is what the gate was for.

## [0.53.0] - 2026-08-13

### Added — comfyui-pp-cli perfection wave 1

Seven scored backlog rows against the vendored `tools/comfyui` CLI. Sibling parity with
`llamaswap-pp-cli` was the organising principle: where that CLI already solved a problem, this
wave ports the solution rather than inventing a second one.

**Typed domain exit codes (row 1).** `internal/cli/exitcodes.go` is now the single registry of
every non-zero code, mirroring the sibling's contract. The domain codes were previously scattered
across four files with no shared view, which is how 12/13 and 21/22 each came to carry two
meanings unnoticed; nothing is renumbered, but every value is named and every reuse documented and
command-scoped via `pp:typed-exit-codes`. A compile-time guard fails the build if
`internal/comfy/submit`'s own constants ever drift from the registry. `set` and `validate` gained
the annotations they were raising codes without.

Three new codes, and two deliberate migrations that a caller branching on the old values will see:
`24` execution-interrupted and `25` upstream-OOM split out of the generic `21` (classification
reuses `exp.ClassifyFailure`, so the sweep runner and the wait path agree on what an OOM is), and
a dial failure now exits `4` rather than `5` — "the server is down" and "the server refused this
request" had been sharing a code. `26` upstream-5xx separates a broken server from a refused
request.

**Structured error envelope (row 3).** Under `--json`/`--agent`, every failing exit now emits
`{ok:false, error:{code, category, retryable, http_status, message, remediation, exit_code}}`.
Previously only the HTTP-409 branch emitted anything structured — and it emitted a different,
poorer shape — so every typed exit, dial failure and usage error reached a machine caller as bare
prose. The worst case was a malformed invocation under `--agent`, where flag parsing fails before
`--agent` can imply `--json`; that path is covered by scanning argv. **Field names are identical
to `llamaswap-pp-cli`'s**, so one parser reads both twins. The envelope goes to stdout, or to
stderr when the command already wrote a result document there, so stdout stays exactly one JSON
document.

**Code-orchestration MCP surface (row 2).** The 13 endpoint-mirror tools are replaced by
`comfyui_search` / `comfyui_get` / `comfyui_execute` over an in-binary registry — the sibling's
shape, and the pattern Anthropic documented for large MCP surfaces. Endpoint summaries are carried
over verbatim because they encode findings that cost real render time to learn. One deliberate
divergence from the sibling: `Positional` is populated for real, so `{prompt_id}`, `{class_type}`
and `{file}` actually substitute — the sibling ships empty slices and sends literal templates for
its `{model}` endpoints, a bug owed a separate fix there.

**MCP output schemas (row 4).** The three code-orchestration tools declare `outputSchema` and
return real `structuredContent`. Schemas are committed under `internal/mcp/testdata/schema/` and
the goldens are checked against what the server actually advertises AND against live handler
output validated through a schema-validating server — proven non-vacuous by injecting drift in
both directions.

**API surface completeness (rows 8/18/23/29).** `free` (POST /free) is the cross-tool VRAM handoff
primitive for a box where ComfyUI and llama-swap share cards. `features` (GET /features) turns the
0.32.0 pin from an invisible assumption into a reported fact, comparing key SHAPE only because the
values follow the server's own CLI args. `history clear`/`delete` close the one queue-family verb
the history group was missing. `upload mask` adds the multipart endpoint, sharing one multipart
implementation with `stage` rather than a second copy.

All four contracts were read from the ComfyUI server source rather than inferred, which is how
they document behaviour the route list does not: `/free` and `/history` answer with EMPTY 200
bodies, `/free` acts asynchronously via a queue flag, and `/upload/mask` composites the posted
file's ALPHA CHANNEL onto an existing image — so an opaque PNG silently writes an opaque mask, and
a missing original writes nothing while still returning 200. Every side-effecting command prints
by default and requires `--execute`, with an `IsVerifyEnv` short-circuit and no `mcp:read-only`.

**Reproducibility intelligence (rows 12/13).** `deps <graph.json>` reports which pack provides each
class and which classes nothing installed provides, resolving provenance through `/object_info`'s
`python_module` and recovering pack names for missing classes from the ComfyUI Manager hints a
UI-format workflow carries. It complements `validate` rather than overlapping it: validate asks
"is this graph well-formed here", deps asks "what would this box need installed at all".
`provenance` now reports the node set a run was submitted against — server identity alone cannot
answer "was this the same environment", because a custom pack installed between two runs changes
what a `class_type` means while every server field stays identical. Capture and report only; no
restore. Storage uses new tables rather than new columns, because the idempotent migration replays
`CREATE TABLE IF NOT EXISTS` and would never add a column to an existing database.

**Test target (row 15).** `make test` is now `-count=1 -race -shuffle=on`. No races were exposed.

### Verification
`gofmt` clean, `go build ./...` and `go vet ./...` clean, and `go test ./... -count=1 -race
-shuffle=on` green across all 17 packages, in both the canonical library tree and this vendored
copy. The error envelope was additionally live-checked against the real binary with the server
down. **No live-server verification was possible** — the box's ComfyUI was down and another
session owned both GPUs — so every check needing a real server is written up as a runnable
procedure in the library tree's `LIVE-SMOKES-DEFERRED.md`, ordered by risk.

### Fixed — documentation drift: the whole verified backlog (#99 + the remaining 13)
A four-track audit of the 0.44.0–0.48.1 arc confirmed 17 drifts against **both** the doc line and
the code line (3 further claims were checked and rejected as not-real). Four were fixed in #99,
which went green on 2026-08-12 and then sat unmerged while main moved four PRs ahead; it is folded
in here rather than re-authored. The remaining thirteen were re-verified against 0.51.0 before being
applied — all thirteen were still live.

**The governance cluster (5 of the 13, one defect across four surfaces).** `docs/OPERATOR-GUIDE.md`,
`docs/systems/coding-agent.md`, `docs/glossary.md` and `README.md` all described the agent's
`security_risk` self-annotation as a live safety control. #99 corrected the two loudest surfaces;
the rest of the cluster lands now, with the precision the earlier pass left out:

- `--rules` **defaults to empty**, so the documented rule table describes an opt-in path, not the
  default state. With no table the rules in force are exactly the built-in `defaultRules()`
  secret-material floor — secret globs and nothing else.
- The `UNGATED` build note is documented where an operator meets it, with its **exact** trigger
  (`Unattended` AND one of `--allow-delete`/`--allow-overwrite`/`--allow-shell`/`--allow-github` AND
  no `--rules`; `--allow-write` and `--allow-run` alone do not raise it), its status (a note, never
  an error — refusing would break existing unattended callers), and its **scope** (the CLI/queue
  path; the MCP `agent_run` front door passes no write/delete/shell/fetch capability and is
  unaffected). It had been operator-visible output documented nowhere.
- `examples/agent-rules.json` — named in the shipped warning string since #94 and referenced by no
  document outside this changelog — is now on the operator surface, summarized accurately, and
  flagged as a repo file no installer packages, so the path differs between a checkout and an
  installed binary.
- The advisory judge's verdict space is named: **WARRANTED / EXPECTED FRICTION / BLOCKER**, plus the
  sanctioned all-clear. An operator reading a `judge_report` needs to know BLOCKER is not
  misbehaviour and that an all-EXPECTED-FRICTION report is the normal outcome.

**The rest.**

- `docs/flows/run-graph-manifest-satisfaction.md` said a caller-supplied `out_dir` is **not** created
  and would ENOENT into an opaque `RUN_ERROR`. `resolveOutDir` has created it since v0.22.4 — which
  the same file already recorded twelve lines earlier, so the document contradicted itself. A path
  that cannot be created defers early with `cannot create out_dir: …`.
- `docs/systems/media-generation.md` + `README.md`: inpainting does **not** leave the unmasked area
  pixel-identical. `grow_mask` dilates and feathers by 16 px by default and the graph has no
  composite-back node, so the whole frame is VAE round-tripped on decode. Preserved in intent, not in
  pixels.
- `docs/systems/media-generation.md`: the generative-edit route was credited with a half-override
  guard it does not have. The builder does throw on a missing steps or cfg, but the runner fills the
  missing half from the preset first, so the throw is unreachable and `--steps 8` alone runs silently
  at the preset's cfg. The sibling qwen-image **generation** route does hard-reject (exit 2) — and
  the same page describes both, so the asymmetry read as protection.
- `docs/flows/cascade-escalation-and-defer.md`: `esc_source` was absent from both the success-metadata
  and the "why did this defer?" field lists. Documented with its closed seven-value set, the
  first-gate-wins ordering, the only-if-unset carry across tiers, and the reason omitted rows still
  parse (an absent field decodes to empty — `omitempty` governs writing, and exists because the
  append-only JSONL's small-line atomicity is load-bearing).
- `render/README.md`: "Two modes" for a dispatcher that has grown a third. `--family
  hidream-o1|hidream-o1-dev|qwen-image` — added by #90/#91, which updated the systems doc and left
  the directory's own front page at the pre-family state — is now documented, along with `--prompt`,
  `--wait-sec`, `COMFY_WAIT_SEC` and `COMFY_DEAD_SEC`, which the flag list presented itself as
  complete without.
- `render/README.md`: the poller's "~6 min" ceiling has been 30 min since quality-first renders
  started legitimately running past it. Documented with the harness's `COMFY_WAIT_SEC` alignment and
  the dead-server watchdog (`COMFY_DEAD_SEC`, default 240 s), which was undocumented on every
  surface.
- `README.md`: `config.example.json` was said to carry "commented defaults". It is strict JSON with
  zero comments — generated by `go generate .` and round-tripped through `encoding/json`, which
  rejects comments outright. The per-key rationale lives in the doc comments on `Config`.

Docs-only: no `VERSION` bump, matching the repo's precedent for #93 and #99.

### Changed — dual-Blackwell template: reranker moves to GPU
`setup/templates/llama-swap.win-dual-blackwell.yaml`: `bge-reranker-v2-m3` now serves
with `-ngl 99` on the utility card (operator decision 2026-08-13). The `-ngl 0` pin was
an 8 GB-era carryover that cost 2.4–4.6 s per 20-doc rerank against ~143 ms typical on
GPU (measured on the reference box). Small-VRAM tiers deliberately keep CPU: the
linux (Lenovo-class) template is unchanged. Raw-logit scores are device-independent,
so downstream threshold calibration (mem0 admission gate) is unaffected. Brings the
template back in parity with the live <node-b> config, which was flipped the same day —
template regeneration would previously have silently reverted the fix.

## [0.52.0] - 2026-08-13

### Fixed — `doctor`, `acceptance` and `report` called every correctly-served alias MISSING
llama-swap publishes CANONICAL model ids in `/v1/models` `data[].id` and the names the harness
actually binds only inside `meta.llamaswap.aliases`. The harness had **three** independent readers of
that endpoint with **two** different response schemas: `main.go`'s `fetchModelRoster` (id-only, used
by `doctor`, `acceptance` and `report`), `mcpserver`'s `probeServedModels` (id-only), and
`plannerUnserved` (the only alias-aware one). So on a healthy reference box the shipped default
config — which binds `offload-e4b`, `gemma4-e2b`, `gemma4-26b-a4b`, every one of them an alias —
made `doctor` print `FAIL — not in the live /v1/models roster` for four of its eight bindings and
exit non-zero, `acceptance` refuse the node work, and `report` hand a collaborator a table of
`**MISSING**` beside seats that were serving the whole time. Verified before/after against the live
endpoint: same four bindings, `FAIL` → `OK`.

All three readers are now one adapter, [`internal/swapclient`](internal/swapclient/swapclient.go),
over the vendored public client `tools/llamaswap`'s `pkg/llamaswap` — matching ids first, then
aliases, case-insensitively, with one endpoint-normalization rule instead of the three that had
grown (one of which would have fetched `/v1/v1/models` from a `/v1`-suffixed endpoint).

### Fixed — the fleet node read seat residency off a field llama-swap misreports
`fleet_reclaim.go` decided which loaded seats were reclaimable from each `/running` row's `ttl`.
llama-swap publishes `ttl: 0` for a seat CONFIGURED `ttl: -1` (verified live on v249 — both support
seats read `0` there today), so the rule rested on a value the server gets wrong. It survived the
common case by accident and got the opposite one wrong: a support seat given a real TTL was counted
as reclaimable, over-stating advertised capacity by the size of an embedder. Residency now comes
from `pkg/llamaswap`'s `KeepSet()`/`IsProtected()`, which parses the llama-swap YAML and never asks
the server. The bounded error documented in that file's own comment is gone. A box where no YAML and
no keep-set config can be read falls back to the old `ttl` reading rather than to "nothing is
protected" — the permissive answer would fold a resident embedder into the idle baseline and make
that node under-advertise forever.

### Fixed — the roster table `doctor` gates on and the one `offload_status` publishes had drifted
Two hardcoded lists of the same thing, 8 keys against 10: `ocr` and `embed` were advertised to an
autonomous planner by `offload_status` and unknown to every other surface. Both now read
`config.Config.ModelRoutes`. `offload_status`'s payload is unchanged (asserted by test); the doctor
gate is unchanged too — `ocr` and `embed` are reported, not gated, because both resolve to a
non-empty fallback and gating them would fail nodes whose serving tier declares no such seat.

### Changed — STT's zero-always-warm unload goes through `pkg/llamaswap`
`internal/sttclient`'s `Unload` posted `/api/models/unload/{model}` raw with whatever name it was
handed. It now resolves the name through the roster first (the harness binds `whisper`/`stt`, and
only the canonical id is guaranteed to key that route) and REFUSES a seat the llama-swap config
marks resident. Drain stays off, preserving the previous unconditional behavior; it is one option
away for a future caller that unloads a seat it does not own.

### Changed — the harness now imports one package from a printed CLI
Root `go.mod` gains `require llamaswap-pp-cli` + `replace llamaswap-pp-cli => ./tools/llamaswap`.
This is a deliberate, documented exception to the `tools/` isolation rule
([docs/systems/printed-clis.md](docs/systems/printed-clis.md#the-one-exception-the-harness-consumes-pkgllamaswap)),
and it is not free: `pkg/llamaswap` sits on the tool's `internal/mirror`, which also carries the
epoch mirror engine, so `modernc.org/sqlite` comes with it. Measured: the harness binary went
18.0 MB → 23.2 MB and the root `go.mod` gained ten indirect requirements (`cobra` and `mcp-go` stay
out). The follow-up that removes the cost is an upstream reprint splitting the mirror engine off the
client type. The harness still never shells out to a printed CLI, and no printed CLI is in the
serving path.

`internal/agent`'s `ProbeServedWindow` is the one llama-swap call deliberately NOT routed through
the package: `Client.Props` refuses to auto-start an unloaded model, which is right for an operator
probe and wrong here — the planner seat is normally cold at that point, so a refusing probe would
drop every cold run back to the 8192-token fallback and re-open finding F4. It shares the package's
endpoint normalization and nothing else. (Alias resolution buys it nothing: llama-swap resolves
aliases on `/upstream` itself, verified live.)

## [0.51.1] - 2026-08-13

### Fixed — `tools/comfyui`: `validate` failed good graphs from a stale cached schema
Found in live use. `comfyui-pp-cli validate` rejected a valid graph with
`combo-value-not-in-options` for a checkpoint the server had had for hours: it read a
5h27m-old **cached** `/object_info` that predated the file, and `--data-source live` did
not override that cache. The live `nodes options` call on the same input was right the
whole time, which is what made the failure so misleading — two commands, same server,
opposite answers.

Two independent defects, both fixed:

- **`--data-source live` was inert here.** `validate` resolved its schema through a
  cache-only loader that never looked at the flag, so the one escape hatch from a stale
  cache silently returned the stale cache. It now reads `/object_info` off the running
  server, and a failure there is fatal rather than degraded — quietly falling back would
  hand back exactly the answer the operator was trying to get away from. `--object-info
  <dump>` combined with `--data-source live` is now refused as the contradiction it is,
  using the same `validateDataSourceStrategy` refusal every other read command issues.

- **A cached verdict did not say it was cached.** Membership findings
  (`combo-value-not-in-options`, `unknown-class`, `model-class-unregistered`) are the ones
  staleness can invent out of nothing: the option set only ever grows between syncs, as
  model files are dropped in and node packs installed. The report now carries
  `schema_synced_at` and `schema_age` for any cache-sourced schema, and a membership
  finding read from cache gets a hint naming the age and both ways out (`sync --resources
  objectinfo`, or `--data-source live`). Graph-local findings — dangling links, host
  paths — get no such hint: no resync will change them, and suggesting one sends the operator
  chasing the wrong thing. A live-sourced miss gets none either; the server is the authority.

`auto` deliberately stays on the cache. `validate` is the offline preflight — it is
documented to need no server, agents run it before every submit, and turning the default
into a network round trip would change what the command is. The cost of that choice is
paid by reporting the age instead of hiding it.

Audited for the same defect class elsewhere: `slotsLoadCachedObjectInfo` was the only
cached-schema reader in the module. `nodes` and `models` always fetch live, and submit's
preflight (`submit.Lint`) is purely graph-local — its COMBO diagnosis annotates the
**server's own** rejection, not a cache. Nothing else to fix.

`comfy_slots.go` is a preserved hand-written file, not generated, so this needs no
`.printing-press-patches/` entry. Covered by table-driven tests over all three
`--data-source` values, the three cache-age states (backdated, fresh, never synced), and
the hint's negative space.

## [0.51.0] - 2026-08-13

### Added — `tools/llamaswap`: the second printed CLI
`llamaswap-pp-cli` lands at `tools/llamaswap/`, completing the pair `tools/` was set up
for in 0.50.0. Where `comfyui-pp-cli` covers the media half of what the harness talks to,
this covers the other half: the llama-swap server that actually serves the cascade. It
reports what is loaded and what a swap cost, resolves seat and model bindings, reads GGUF
header facts straight off the model files rather than trusting a config, and reads the
llama-swap config's own backup history. It ships an MCP server (`llamaswap-pp-mcp`), the
`pp-llamaswap` agent skill, and — unique among the printed CLIs so far — an importable Go
client at `pkg/llamaswap`.

Same contract as the first: separate Go module (`llamaswap-pp-cli`), invisible to the
harness's `./...`, no harness package imports it, no behavior change to the harness. It
follows the layout contract in `docs/systems/printed-clis.md` exactly, which is what the
contract was written for.

**It needed no portability patch.** `GOOS=linux go vet ./...` — which compiles test files,
unlike `go build` — passed as vendored, and the platform-split files (`*_windows.go` /
`*_other.go`) were already correct. That is the opposite of 0.50.0's experience with
`comfyui-pp-cli`, and the reason to keep running the check rather than assume either result.

### Added — `tools-llamaswap` CI job
Mirrors `tools-comfyui`: `go build`, `go vet`, `go test ./... -count=1`, nothing excluded.

Some of this suite is deliberately coupled to a **real deployment** — `internal/gguf` reads
real GGUF files off the model volume, and the seat-log tests assert independently verified
facts about a reference llama-swap backup corpus. Those are machine-specific acceptance
gates, not portable unit tests, and they already `t.Skip` when the resource is absent.
Verified rather than assumed: with the model volume, the corpus, and the server all missing,
the module runs **750 pass, 7 skip, 0 fail** on Linux. So CI excludes nothing, and a failure
in those suites means a skip guard regressed rather than that the runner lacks hardware.

`LLAMASWAP_BASE_URL` is left unset in CI: the default points at a loopback port that is
closed on a runner, which is what the offline paths expect.

### Fixed — real hostnames and the tailnet domain in vendored fixtures
The vendored tree arrived with real node hostnames and the real tailnet domain in three
`internal/cli` test files, used as sample "named remote" values. `docs/STYLE.md` forbids
both in this public repository, in code as well as prose. Replaced with neutral placeholders
(`node-a`, `node-b`, `node-b-host`, `tailnet-example`); the names carried no test meaning, so
behavior is identical.

This is now a documented step of the adoption cycle rather than a one-off: a printed CLI is
generated on a real machine against a real deployment, so **sweep every tree for identities
before committing it**. Deployment paths that are part of the tool's documented interface
(`C:\llama-swap`, `V:/models`) are fine; hostnames, tailnet domains, and usernames are not.

### Changed — `.gitignore` excludes live captures
`llamaswap-pp-cli captures export` writes `captures.jsonl` into the module directory: real
request and response bodies from whatever server was running. It was 3.5 MB on the printing
machine. Runtime output and a data-leak hazard in a public repo — never vendored, now
ignored via `tools/*/captures.jsonl`.

## [0.50.0] - 2026-08-13

### Added — `tools/`: printed CLIs are vendored here as repo tooling
`comfyui-pp-cli` lands at `tools/comfyui/` — a standalone Go CLI generated by
[CLI Printing Press](https://github.com/mvanhorn/cli-printing-press) that drives a local
ComfyUI render server from the shell. ComfyUI keeps its render history in RAM and loses
it on restart; the CLI submits graphs, reads timings from the server's own execution
timestamps, and keeps runs, graphs, node schemas, and outputs in local SQLite so
comparisons survive the restarts that tuning requires. It also answers questions the
server cannot: why a model file is invisible, what values a loader input will actually
accept, and which configuration produced a given output file. It ships an MCP server
(`comfyui-pp-mcp`) and an agent skill (`pp-comfyui`).

**No behavior change to the harness.** This release adds tooling beside the harness, not
inside it. No harness package imports `tools/`, the harness does not shell out to the CLI
at runtime, and the harness's dependency graph is untouched.

**It is a separate Go module, deliberately.** `tools/comfyui/go.mod` keeps the module name
the Printing Press gave it (`comfyui-pp-cli`) rather than a package path under
`github.com/dmmdea/offload-harness`. Go excludes nested modules from a parent's `./...`,
so a root `go build ./...` / `go vet ./...` / `go test ./...` does not see it — which is
the point: the CLI pulls `cobra`, `mcp-go`, and `modernc.org/sqlite`, none of which belong
in the harness binary. The cost of that isolation is that the root commands cannot vouch
for it, so CI gets a dedicated `tools-comfyui` job (below). A module invisible to `./...`
*and* absent from CI is vendored code nothing ever compiles.

**Generated code is adopted, not authored here.** Fixes go upstream to the Printing Press
and the tree is re-vendored; hand-editing a generated file works until the next reprint
overwrites the whole tree, then vanishes with no diff to explain it — the same failure
mode as editing a generated tier page instead of `setup/templates/profiles.json`.

### Added — `tools-comfyui` CI job
Runs `go build ./...`, `go vet ./...`, and `go test ./... -count=1` in `tools/comfyui`,
parallel to the harness `build` job. The suite is offline by construction: it reaches the
network only to assert a *failure* path (`ProbeReachable` against a closed port), so an
ubuntu runner with no ComfyUI installed is a valid host for every package. Nothing is
excluded, and no mock-mode env var is set — the `PRINTING_PRESS_VERIFY` /
`PRINTING_PRESS_VERIFY_LIVE_HTTP` / `PRINTING_PRESS_DOGFOOD` vars govern the Printing
Press's own verify and dogfood runners, not `go test`, and CI leaves them unset. 17 test
packages, all green.

### Added — `docs/systems/printed-clis.md`
What printed CLIs are, why they are separate modules, the one-directional
regenerate/re-vendor cycle, what is vendored and what is not, and the layout contract the
next printed CLI follows (`llamaswap-pp-cli` is expected at `tools/llamaswap/`). `AGENTS.md`
routes agents to `tools/comfyui/SKILL.md` for ComfyUI work and states the
no-hand-editing rule; `README.md` gains a Tools section.

### Fixed — the vendored ComfyUI CLI handled host paths Windows-only
Adopting the CLI put it on an `ubuntu-latest` runner for the first time, and
`internal/comfy/media` failed immediately. Both host-path functions delegated to Go's
`filepath`, which honors only the *running* OS's separator — so on Linux
`filepath.Base(`​`D:\refs\portrait.png`​`)` returns the entire string, and
`filepath.ToSlash` is a no-op.

Two real defects, not cosmetic ones:

- **`StagedName` broke its own guarantee across a mixed fleet.** Its doc comment calls the
  content hash load-bearing: identical bytes must stage under one name or an archived run's
  provenance splits. On Linux the same file behind a Windows-style path staged as
  `D_refs_portrait-<sha>.png` instead of `portrait-<sha>.png` — the exact collision the
  design exists to prevent.
- **`ValidateComfyFilename` failed OPEN on every non-Windows node.** Its job is to reject
  host paths before they reach `LoadImage`; on Linux it silently stopped rejecting UNC and
  backslash-separated `..` traversal.

Both now handle `/` and `\` explicitly, independent of `runtime.GOOS` (new `hostBase`, and
`strings.ReplaceAll` in place of `filepath.ToSlash`). Windows behavior is unchanged — only
the non-Windows path moves, from wrong to correct. Verified by running the cross-compiled
test binaries under Linux, not just on CI.

This is the same defect class `crossplatform_lint_test.go` catches in the harness, and it is
recorded as a patch against the generated tree in
`tools/comfyui/.printing-press-patches/` — a debt owed upstream, since a reprint would
otherwise revert it.

A generated test fixture also carried a real local username in a path
(`/home/<user>/refs/portrait.png`); replaced with `/home/user/...` per the `docs/STYLE.md`
privacy rule, which covers code as well as docs in this public repository.

### Changed — `.gitignore` covers `tools/*/` build output
The existing `/bin/` rule is anchored to the repo root and does not cover a nested
module's `bin/`. Added `tools/*/bin/` and `tools/*/build/` (the ComfyUI `.mcpb` bundle
alone is ~20 MB), plus the per-run Printing Press reports —
`dogfood-results.json`, `workflow-verify-report.json`, `apify-actor-audit-report.json`,
`.printing-press-*-polish.json` — which are one machine's evidence, superseded by the next
run. The durable provenance summary stays in the vendored `.printing-press.json`.

## [0.49.0] - 2026-08-13

### Fixed — the generative edit seat was capped at ~1 MP by a Flux-Kontext node
`render/wf-qwen-image-edit.mjs` built its input scaler as `FluxKontextImageScale`,
copying ComfyUI's shipped `image_qwen_image_edit_2511` template. That node takes no
size argument: it lanczos-resamples every input to the nearest-aspect entry of
Flux-Kontext's fixed 17-entry resolution table, whose LARGEST entry is 1024x1024
(1.05 MP). A 2048x1024 source came back **1456x720**, every time, and no config could
raise it.

The cap landed on the OUTPUT, which is why it cost real pixels. That node feeds two
places and only one of them was ever listening:

- the two `TextEncodeQwenImageEditPlus` encoders, which do **not** care — that node
  rescales its own inputs internally (384x384 for the vision tokens, ~1 MP snapped to
  8 for the reference latents), so whatever it is handed, it resizes anyway;
- `VAEEncode` -> `KSampler.latent_image` at `denoise: 1.0`, which decides the rendered
  resolution outright.

So the scaler was never buying the encoders anything — it was only ever setting, and
capping, the canvas.

Node 5 is now `ImageScaleToTotalPixels` with `resolution_steps: 16`. The snap is
load-bearing, not cosmetic: Qwen-Image's VAE downscales 8x and its DiT patch size is 2,
so pixel dimensions must be multiples of 16 for the latent to tile without padding —
that is the one thing the removed node was doing for us, by accident.

**Deviation from the template, deliberately.** The shipped 2511 and 2509 templates do
use `FluxKontextImageScale` (the original `image_qwen_image_edit` template used
`ImageScaleToTotalPixels`). Mirroring the template exactly is what produced the bug, so
this graph now follows the v1 template's scaler instead. Everything else — the
`ModelSamplingAuraFlow` shift, the dual edit encoders, the Lightning-as-LoRA binding —
still tracks 2511.

### Added — `gen_edit_megapixels`, and a default that follows the source
New float config key, `0.01`-`16.0`, in the same `megapixels * 1024 * 1024` units as the
ComfyUI node (so `2.0` is exactly 2048x1024). Flows
`config.GenEditMegapixels` -> `imagegen.EditModel.Megapixels` -> `--megapixels` ->
`buildQwenImageEdit({megapixels})` -> node 5. Omitted from the argv entirely when unset,
so an unbound machine keeps the runner's default.

Unset (the default) means **follow the source, held within 0.9-2.0 MP**.
`render/comfy-edit.mjs` measures the source before staging it and targets its actual
megapixels, so an in-band source renders at exactly its own size — the harness's own
2048x1024 output round-trips at native resolution (2048x1024 is exactly 2.0 MP under this
arithmetic, and both dimensions are already multiples of 16, so scale factor 1.0).

Both ends of the band are load-bearing. The 2.0 ceiling bounds VRAM and time on a seat
running a ~15 GB unet on 16 GB cards. The 0.9 floor scales a small source *up* onto the
model's working canvas — which is what `FluxKontextImageScale` did anyway (512x512 in,
1024x1024 out) and what both official templates do; they normalise rather than preserve.
0.9 specifically, because it sits just under the whole 1-MP-class grid — 1536x640 at
0.9375, 1216x832 at 0.965, 1344x768 and 1152x896 at 0.984, 1024x1024 at 1.0 — so every
one of those keeps scale factor 1.0. A floor of 1.0 would have quietly stretched a
1344x768 source to 1360x768.

The floor also closes two holes that only showed up when ComfyUI's own
`ImageScaleToTotalPixels` arithmetic was replayed over real files rather than reasoned
about: a 97x53 thumbnail resolves to 0.0049 MP, under the node's declared 0.01 minimum,
and a pathological aspect ratio can snap a dimension to **0** — a graph ComfyUI cannot
execute. Both are now regression-tested.

New `render/image-size.mjs` reads pixel dimensions off PNG, JPEG and WebP headers — no
decode, no dependency, and no guessing: an unreadable buffer reports `0x0` and the seat
falls back to the cap, which is the non-destructive direction. Checked against 24
PIL-written files (including a progressive JPEG, a 50 KB-EXIF JPEG, and all three WebP
payload headers); it agreed with PIL on every one.

## [0.48.0] - 2026-08-11

### Added — the ledger can finally say WHY a call escalated (#94)
The ledger carried a free-text `reason` on DEFERRED rows only, so a call that
escalated and then SUCCEEDED recorded nothing — and those are exactly the rows
that matter. The one question the telemetry needed to answer, "did the model's
self-report send this up, or did a structural signal?", was unanswerable in
aggregate, which meant no change to the gating could be evaluated after the fact.

New `core.EscalationSource`: a CLOSED set of seven — `self_confidence` (the only
self-declared gate) plus `margin`, `confhead`, `schema`, `grounding`, `verifier`,
`retries` (all structural). Closed because bounded cardinality is what makes it
groupable; a sentence is not. Stamped where each gate fires and carried across
tiers only-if-unset, so the value means "the gate that first sent this call up"
rather than "the last one it tripped". Persisted as `esc_source`, omitted
entirely on non-escalating rows (the append-only JSONL's small-line atomicity is
load-bearing), and rows written before the field still parse.

Verified on production traffic, not only in tests: a real classify climbing
e2b → e4b now writes `{"escalations":1,"margin":0.925,"esc_source":"margin"}`.

### Added — `examples/agent-rules.json`, and an UNGATED warning (#94)
MEASURED on the production agent seat over 48 runs and 66 effectful calls: the
model's own `security_risk` annotation is a literal constant — **54 of 54**
emitted declarations are `low`, including **all 36** structurally destructive
calls. Park-gate recall: **0%**. It holds under an escalated-severity arm, where
deleting every source file in a tree is also declared `low`. The built-in
`defaultRules()` floor covers secret-material globs only and would not have
stopped one of them.

`Build` now appends an `UNGATED` note when an unattended run is granted
destructive capability with no `--rules` table, and the shipped example table
turns the probe's own call (`delete src/notify.py`, self-declared `low`) into a
non-Allow. A note rather than an error, because refusing to run would break
existing callers — but silence is what let a 0%-recall mechanism sit where a
safety control appears to be.

Scope: this is the CLI/queue path, which grants `--allow-*` and sets
`Unattended`. The MCP `agent_run` front door passes no write/delete/shell/fetch
capability at all and is unaffected.

### Changed — the advisory judge got somewhere to put ordinary work (#95)
Same failure shape as the annotation above: a grader asked only about trouble
saturates toward trouble. Every record the judge sees is ALREADY flagged, and the
framing question was "was the flag warranted" — uninformative, and biased toward
yes. The prompt now partitions WARRANTED / EXPECTED FRICTION / BLOCKER, with the
friction cases enumerated concretely (a call corrected on a later turn, a test
written to expose a defect, an exploration dead-end, a fallback after a refusal,
a command that failed before a service existed, a policy refusal the run worked
around, and zero-count results — which are CLEAN outcomes, not failures), plus an
explicitly sanctioned all-clear.

`flaggedForJudge`'s comment claimed the model's self-assessment "is signal". It
is not, per the measurement above, so that clause selects zero records in
practice. It stays because it fails SAFE, but no longer advertises coverage it
does not provide.

Pattern credit: NVIDIA-NeMo/Switchyard (Apache-2.0), whose escalation judge prompt
spends more words on what must NOT escalate than on what must. No code vendored.

## [0.47.0] - 2026-08-10

### Added — opt-in prompt refiner on the image-generation path
Arena scoring showed a prompt-refiner agent is worth a free quality bump, so
the harness prompt path now replicates it. New config key
`imagegen_refiner_model` (a llama-swap model id, e.g. `"gemma-4-12b"`; empty =
OFF, path byte-identical to today — pinned by test) has `generate_image`
expand the raw prompt with concrete photographic detail (lighting,
composition, materials, mood, lens vocabulary) on the free local text tier
before the render — temperature 0.4, ~256 tokens, bounded by
`imagegen_refiner_timeout_sec` (default 30). One shared decision point
(`internal/pipeline/refiner.go`) serves the single ComfyUI path, the sdcpp
engine, and warm batch — the same drift class `imageModelFromConfig` deletes
for the model binding.

**Fail-safe by construction:** any refiner problem — transport error, timeout
(annotated "cold model swap?" on a deadline hit), truncated or empty output,
output shorter than the input, a prompt already over the ~200-token refiner
budget (skipped up front), or a computed quoted-span guard violation — falls
back to the RAW prompt, records the reason, and renders anyway. Refinement
never makes a render fail. The span guard runs in BOTH directions and in
normalized-quote space (curly `“”` count as straight): every `"double-quoted"`
span of the raw prompt must survive verbatim (with distinct
`altered (glyphs/whitespace)` vs `dropped` reasons), and the refiner may not
ADD quoted text — a whole-output quote wrap is stripped, anything beyond that
is rejected (net-new quotes are a draw-this-text instruction on this model
family). An odd raw quote count drops the trailing quote before span pairing
(inch-mark tolerance). Batches get a refiner circuit breaker: 3 consecutive
transport/timeout-class failures disable refinement for the remaining jobs
(marked `refiner disabled after N consecutive failures`) instead of stalling
timeout-by-timeout before the first render; the batch summary reports
`refine_fallbacks`. Output paths still derive from the raw prompt with the
`refine` knob stripped from the hash, so identical requests keep reusing one
file, and batch jobs/results stamps hash the RAW jobs. Results gain `refined`
(+ `refined_prompt` / `refine_fallback`) only when a refiner is configured;
batch items then always carry `refined` true/false. Request-level opt-out:
`refine=false` on the MCP tool / CLI (`--refine=false`; the `=form` is
enforced — a space-form boolean now errors loudly instead of silently
dropping flags) / a batch job's `"refine": false`, with the CLI flag
propagating onto batch jobs that set no per-job value.

## [0.46.0] - 2026-08-10

### Added — harness binding for the qwen-image preset knobs
0.45.0 shipped `--preset/--clip/--lora/--lora-strength/--shift` as CLI-layer
flags with no config path, so a harness-driven `qwen-image` seat could only
render the `full` recipe. New config keys `imagegen_preset` / `imagegen_clip` /
`imagegen_lora` / `imagegen_lora_strength` / `imagegen_shift` now thread
through `imagegen.Model` → `bindingArgs` to the render scripts — a bound seat
can run `lightning4` (4-step LoRA) directly. Zero-value fields emit no flag,
so every existing binding renders a byte-for-byte identical command
(compatibility pinned by test). An empty `imagegen_lora` means UNSET, not
"strip the preset's LoRA" — bind preset `full` for a LoRA-free run. No script
changes: comfy-render/comfy-generate/batch-jobs already read these flags.

## [0.44.x interim] - 2026-08-10

Merged inside the 0.44.x line with no separate version bump (recorded here
retroactively so the changelog carries the whole arc; details live in each
PR and the system docs):

- **#84** `ocr_model` capability — dedicated OCR seat binding routing ONLY the
  ocr task, falls back to `vision_model`; deliberately unbound.
- **#85** CI un-broken — POSIX-only test-fixture pipe-flush bug; `main` had
  been red since 08-04.
- **#86** Effect ledger — per-call EffectRecord (committed|failed|unknown|none)
  on every Result path + the `NotPerformed` sentinel across ~25 refusal sites
  (denied writes no longer ledger-identical to landed ones).
- **#87** Broker risk-rule table — structural, tighten-only, secret-material
  floor, versioned JSON via `--rules`, severity+rule audit fields.
- **#88** `security_risk` self-annotation — unattended high/unrecognized PARKS
  the call (fail closed, ask-queue record).
- **#89** Advisory end-of-run batch judge — opt-in, fresh context, bounded
  inputs, never fatal.

## [0.45.0] - 2026-08-10

### Added — `--family qwen-image`: the 2512 prompt-adherence preset
Qwen-Image 2512 (the DiT with the strongest instruction/text rendering on disk)
was reachable only through `run_graph` with a hand-built workflow — no named
preset, so nothing could route to it. Now `comfy-render.mjs --family qwen-image`
selects the model-correct graph, exactly like `--family hidream-o1` does for the
default seat. No Go changes: the harness already threads `imagegen_family` and
`imagegen_ckpt` through as `--family`/`--ckpt`.

- `render/wf-qwen-image.mjs` — graph follows ComfyUI's shipped
  `image_qwen_Image_2512` template (and its Lightning sibling), with the same two
  fleet departures as the edit builder: switchable UnetLoaderGGUF/UNETLoader, and
  Lightning applied as a **LoRA** so it composes with any quantisation.
  Family-correct details the generic graph gets wrong: `EmptySD3LatentImage`
  (16-channel; the SDXL latent is the wrong format), `ModelSamplingAuraFlow`
  shift 3.1, native 1328x1328 with /16 dim snapping, and an EMPTY negative
  becomes `ConditioningZeroOut` of the positive (the template's own "no
  negative"), never an encoded empty string.
- `QWEN_IMAGE_PRESETS` pairs steps+cfg+LoRA as a matched pair of recipes
  (`full` = 50 steps/cfg 4, `lightning4` = 4 steps/cfg 1 + LoRA), template-widget
  verified; the builder throws on unpaired steps/cfg, same rationale as the edit
  presets.
- `comfy-generate.mjs`/`batch-jobs.mjs` forward the new flags (`--preset`,
  `--clip`, `--lora`, `--lora-strength`, `--shift`; shared-only, a batch job
  cannot override them). These are **CLI-layer only for now**: no harness config
  key binds them, so a harness-driven `qwen-image` seat renders the `full`
  recipe until that wiring lands (recorded follow-up). `--lora ""` forwards
  through the wrapper deliberately — it is the escape hatch that strips a
  preset's LoRA.
- Guard rails, all fail-loud: `--steps`/`--cfg` are accepted only together
  (the harness forwards per-request steps but never per-request cfg, so a lone
  steps override would render the base model at draft steps — technically
  successful, actually noise); unknown/prototype `--preset` names, a missing
  `--ckpt`, and a `builtin` VAE binding (the UNET carries no VAE weights) all
  exit 2 with the fix in the message; the builder rejects non-finite dims and
  unpaired steps/cfg.

### Fixed — finished renders can no longer die on a missing output directory
`comfy-render.mjs` now creates the output file's parent directory before
writing, and `comfy-generate.mjs --batch` does the same for its results file. An
ENOENT at the write site used to discard a fully rendered image after all the
GPU work had succeeded — this wasted a complete A/B render arm on 2026-08-10.

## [0.44.0] - 2026-08-10

### Added — `offload_edit_image_generative`: instruction edits with no mask
The fleet had three edit-shaped things and none of them was a generative,
instruction-following edit. `edit_image` is deterministic PIL ops; `inpaint_image`
re-denoises inside a mask you supply. Neither can do "make it snowing heavily" or
"turn the leather into fur", where the change is global or diffuse and there is no
region to draw. A Qwen-Image-Edit 2511 GGUF had been sitting on disk bound to
nothing because no workflow builder existed for it.

New route (`gen_edit_*` config — named `gen_edit`, not `edit`, because `edit_*` is
already the PIL route and a shared prefix would collide in the JSON config):

- `render/wf-qwen-image-edit.mjs` — graph follows ComfyUI's shipped
  `image_qwen_image_edit_2511` template, with a switchable loader (UnetLoaderGGUF
  for `.gguf`, UNETLoader for `.safetensors`) so a GGUF binding works, and Lightning
  applied as a **LoRA** rather than requiring a pre-merged few-step checkpoint —
  same result, composes with any quantisation, and saves a 20 GB download.
- `render/comfy-edit.mjs` — standard lifecycle (single-slot GPU lock, on-demand
  ComfyUI, zero-always-warm teardown).
- `QWEN_EDIT_PRESETS` pairs steps+cfg+LoRA as a matched triple (`full` |
  `lightning8` | `lightning4`). The builder throws rather than default them: a
  Lightning LoRA at 40 steps/cfg 3 produces mush and the base at 4 steps/cfg 1
  produces noise, and either renders "successfully".

Verified end to end, same source/prompt/seed on a 2-card 16 GB box:
`lightning8` 8 steps 5m26s, `full` 40 steps 9m15s. Both preserve composition and
add convincing snow. Marginal cost ~7.2 s/step, so **~4.5 min of both runs is fixed
overhead** (ComfyUI cold start + a 15.4 GB GGUF load) — the LoRA saves ~4 min of a
9 min job, but the overhead is the bigger target.

Known limitation: `FluxKontextImageScale` snaps to ~1 MP, so a 2048x2048 source
returns ~1024x1024.

## [0.43.2] - 2026-08-05

### Fixed — agent client no longer drops answers that arrive in `reasoning_content`
Reasoning/harmony models (DeepSeek V4 thinking, gpt-oss) can return `message.content` EMPTY with
the entire completion in `message.reasoning_content`. The NIM client has decoded that field since
its introduction; the local agent client never did, so the agent loop read such turns as silence.
This exact blind spot produced the "content comes back empty" evidence that disqualified
gpt-oss-20b's free-text role (2026-08-03 round-2 record) and hid one correct eval answer on
2026-08-05. The client now falls back to `reasoning_content` when content is empty AND the turn
made no tool calls — tool-call turns keep their (normal) empty content, and populated content
always wins. Decode-only: outgoing requests are unchanged (`omitempty`). Three tests, each
mutation-verified red against the pre-fix client at the real call site.

## [0.43.1] - 2026-08-03

### Fixed — output budgets sized from ledger evidence, not from a guess
A 984-call ledger audit on the reference box found that **every** local-cascade deferral was a
budget failure, not a model-quality failure: 16 of 34 were context overflow and 11 were output
truncation. Zero were "the model was not capable enough". Two truncation sources are fixed here:

- **`summarize` now scales its budget with `max_points`** (`384 + 160*n`, so the default 5-point
  request budgets 1184 instead of a flat 512). The old flat budget could not fit the number of
  bullets the caller explicitly asked for: the model wrote them, the budget ran out, the JSON was
  cut mid-structure, and the whole call deferred to cloud. 8 of the 34 deferrals were this.
- **`triage` raised 256 → 768.** Its `reason` field is free text and was measured truncating at
  exactly 256, which throws away a `decision` the model had already emitted (the enum comes first
  in the grammar) and defers the call. The decision is the product; it must not be lost to the
  rationale.

`ocr` truncation is already addressable per-machine via the existing `ocr_max_tokens` config key
(a strong VLM transcribing a dense page exceeds the built-in 1024) — no code change needed.

### Changed — corrected a stale premise in the verifier
Truncation is still `Terminal` (defer, never escalate), but the comment explaining *why* said
"every local tier shares the same context window". That is no longer true — seats are now served
at different `-c` values and the workhorse holds the largest, so escalating on truncation would
move to a **smaller** window. The behavior was right; the reasoning is now right too.

## [0.43.0] - 2026-08-02

### Added — `agent_model`: a dedicated planner seat for the single-loop coding agent
The agent's judgment wants the strongest model the tier serves; the cascade's economics want the
small fast workhorse. One `model` key could not serve both, so the planner seat is now its own
config key with a live fallback chain.

- **Resolution (`config.AgentPlannerModel`): per-call/flag override > config `agent_model` > config
  `model`.** An empty `agent_model` is the pre-seat behavior, and nothing materializes the fallback
  at rest, so the chain stays live.
- **Derived at install-seed time (`internal/tierseed`):** `agent_model` seeds from the tier row's
  `resident_tier` when it differs from the effective workhorse; an explicit
  `config_seed.agent_model` (including a blank) always wins; matching tiers materialize nothing.
- **`agent_timeout_sec`** — tier-seedable default wall-clock for `agent_run` when the call passes no
  timeout (0 = the built-in 180s). `blackwell-2x16` seeds `agent_timeout_sec=600`: a cold
  big-planner load plus low tok/s inside 180s is a timeout machine, not an agent.
- **`agent_run`** now defaults its planner to the agent seat, reports the resolved planner `model`
  in its result (visibility is the cure for a silent seat), and fails loud (`deferred: true`) when
  the resolved planner is not in the endpoint's served roster — never a silent fall back to the
  workhorse.
- **The in-loop offload cascade stays on the workhorse (`model`)**, preserving cascade economics; an
  explicit per-call model still drives both. Two-tier seats are unchanged
  (architect=`escalation_model`, editor=`model`).

## [0.42.0] - 2026-08-02

### Added — the `blackwell-2x16` tier: a homogeneous pair of 16GB Blackwell cards (config #16)
The workstation's second GPU (RTX 5070 Ti 16GB, ~896 GB/s) joined its 5060 Ti (~448 GB/s, x8),
and neither existing multi-GPU row describes that shape: `dual-gpu` is the heterogeneous
5060 Ti + V100 plan (multi-arch build), `blackwell-32` assumes ONE 32GB card. Per the ampere-6
precedent, a new hardware class gets its own id. Full design: the 2026-08-02 dual-Blackwell tier
research (Drive, Local-Offload-Harness).

- **Classifier (Go + PS, parity-tested):** `Facts.Archs` now lists EVERY GPU's architecture
  (nvidia-smi row order; the fallback probe keeps the one-entry-per-GPU invariant), and a STRICT
  rule above the generic dual-gpu rule claims the tier only when all three axes hold: exactly 2
  GPUs, every captured arch Blackwell, and the primary card in the 16GB band (>=12, <24 GB) — the
  "16" is the template's load-bearing assumption (it pins the 26B and a ~10GB vision seat per
  card), so a 2x 8GB or 2x 32GB pair falls through to dual-gpu. A pair whose second card was not
  captured also falls through — never claim a same-build tier on partial evidence. Known limit:
  VRAM is the largest card's, so a mixed 16+8 pair passes the band; catching it needs per-GPU
  VRAM capture. detect.ps1 emits `gpu_archs` in its JSON; both live probes on the reference box
  agree: `blackwell-2x16 (big_ram, ram_tier=high)`.
- **Template `llama-swap.win-dual-blackwell.yaml` — PIN, DON'T SPLIT.** Every model fits one card
  and is pinned whole: splitting a fits-on-one-card model costs 20-33% tg (sequential per-token
  pipeline; harmonic-mean arithmetic, verified). Fast card (device 1): latency primaries + the
  vision seat, kept clean. Utility card (device 0): the memory-stack residents — including
  `bge-reranker-v2-m3`, in a win template for the FIRST time (memory-stack parity with the live
  box) — plus the STT seat and the desktop/DWM tax (no iGPU on this platform). Matrix set
  `"+residents & (e4b | e2b | m26 | vis) & stt"`: one fast-card model at a time, STT CONCURRENT
  from the other card — the second card's payoff for this route. Device indices follow
  PCI_BUS_ID and the reference box; the header instructs nvidia-smi verification + GPU-UUID
  substitution after any slot change or BIOS reset (this board wipes settings on AC loss).
- **Profile row:** ctx 32768, q8_0 KV, 26B full-GPU (`moe_26b: gpu`), uniform
  `CUDA_DEVICE_ORDER=PCI_BUS_ID` + `CUDA_MODULE_LOADING=LAZY` (deliberately NO uniform device
  pin — that would hide a card), the PROVEN blackwell-16 media seed (hidream-o1 bf16 + Wan2.2 Q8
  at 81 frames), and incumbent-only seats per the 2026-08-02 seat-lifecycle house rule: vision
  `qwen3-vl-8b` Q8+F16 pinned fast, STT `whisper large-v3-turbo` pinned utility,
  residency=resident so it runs concurrently. Every challenger from the research (Gemma-4-31B
  QAT, Qwen3.6-27B, V4-Flash IQ3 RAM-GATED, Qwen3.5-122B-MTP, gpt-oss-120b, VL-32B, whisper
  large-v3 full, Qwen3-TTS/Higgs v3, Wan fp16 + HiDream-O1-resident via DisTorch2) is NAMED in
  the notes and enters only via documented bake-off.
- **Adversarial review corrected three MAJORs before merge:** the 16GB band was enforced nowhere
  (added to both classifiers + 7 table rows, mutation-tested red); `videogen_frames: 121` had
  smuggled an unmeasured escalation into the seed (81, like every proven sibling); and
  `resident_tier` had a copy-slip downgrade to e4b (now the 26B, like every 16GB-class sibling).
- The live <node-b> serving config remains hand-maintained and untouched; switching it to the
  rendered tier plus the model downloads stay gated on the research report's §7 hard gates
  (RAM-concurrency arithmetic, V: capacity plan with Exos archiving, PSU/thermal burn-in, UUID
  pins, RTC battery, wired Ethernet).

## [0.41.0] - 2026-07-28

### Added — media seats for the remaining 8 tiers: all 14 now declare vision + STT
`blackwell-16/32/48/72`, `ampere-16`, `volta-16`, `amd-rdna3`, `amd-rdna3-dgpu`. Sizing follows the
TEMPLATE, not just the card, because that is what decides whether VRAM is additive.
- **`blackwell-16` is informed by a LIVE REFERENCE, not a runtime measurement.** This tier is the
  workstation, whose hand-built llama-swap serves the same `qwen3-vl-8b` (Q8_0 weights + FULL-PRECISION
  F16 mmproj, ctx 16384) plus whisper large-v3-turbo. The Q8+F16-tower pairing is the 8→16 GB unlock;
  Q4 is where small-screenshot-text fidelity died, and this one seat owns screenshots/GUI/document OCR.
  **Deltas from live, deliberate:** the seat inherits the tier's `q8_0` KV (live sets no cache-type and
  so runs F16) and adds `--image-max-tokens 2048` + `--no-context-shift`. So: same weights, same window,
  a tighter KV and an image bound — not a byte-for-byte replica of a measured run.
- **`ampere-16` / `volta-16`** — same 8B seat, ctx trimmed to 8192 because the ampere-16 band FLOOR is
  12 GB ("3090-class defensive") and KV has to fit there too. The seat is swappable, so it is the only
  heavy seat loaded and ~10 GB fits a 12 GB card. volta-16 inherits that tier's open sm_70 flash-attn
  risk for the vision seat as well as chat.
- **`blackwell-32` takes the SMALL vision seat on purpose.** The `cuda-resident` template places only
  `resident` seats, so VRAM is ADDITIVE with the ~21 GB roster (that figure is the tier's own
  pre-existing note, not a new measurement). E4B+mmproj at ctx 4096 plus whisper is ~+6 GB, which spends
  roughly half of the ~11 GB the original design left for 64K q8_0 KV, leaving ~5 GB. The 8B (~10 GB)
  would not fit at all. Nothing swaps on this template, so an operator who finds KV tight should drop a
  seat rather than expect recovery. **`blackwell-48`/`72`** have real room for the 8B (~33 of 48/72 GB).
- **`amd-rdna3` (Juan's tier, the D9 audit gap) and `amd-rdna3-dgpu`** — STT via the whisper.cpp Vulkan
  build; vision reuses the tier's resident E4B+mmproj and keeps the CLIP encoder on CPU
  (`--no-mmproj-offload`). Both pin the Vulkan ICD. The dgpu variant keeps the smaller seat as a
  CONSERVATIVE choice for an unmeasured Vulkan part — not because a 10 GB seat could not fit: it is
  swappable, so it would load alone. That asymmetry with ampere-16 (same 12 GB floor, CUDA, gets the 8B)
  is deliberate: CUDA vision is measured-adjacent here, Vulkan vision is not.

**Why the CLIP encoder stays on CPU on Vulkan:** llama.cpp
[#20081](https://github.com/ggml-org/llama.cpp/issues/20081) reports mmproj quality degradation on the
Vulkan backend; the maintainer could not reproduce it and the reporter closed it inconclusively, so that
alone is weak. The load-bearing reason is in the same thread: a hard `vk::DeviceLostError` crash with
mmproj on the Vulkan backend of the **780M iGPU this tier targets**. Keeping the encoder on CPU avoids
the crash path and costs only encoder throughput; the LLM still decodes on the GPU.

**Verified:** all 8 rendered configs ACCEPTED by real llama-swap v242; resident tiers place seats in the
co-resident set (`&`), swappable tiers as alternatives (`|`). PROJECTED — structural validity plus a
live reference, not a runtime measurement; each tier's notes carry its VRAM arithmetic so an operator can
check it. Seat weights stay out-of-band as with every seed; `install render` warns by path.
`blackwell-32/48/72` have no Linux template (`cuda-resident` is Windows-only) — pre-existing, now noted.

**Not expressible here:** the workstation also serves `qwen3-asr` as an HQ STT tier, but that runs on
llama-server (mtmd) rather than whisper-server and binds `stt_model_hq`, which `media_seats` does not
write. Bind it by hand where it exists.

## [0.40.0] - 2026-07-28

### Added — media seats for the last four unseeded tiers, each chosen for its hardware
`blackwell-8`, `cpu`, `amd-gcn`, `dual-gpu` now declare vision + STT seats. These are
**PROJECTED, not measured** — none of these exact cards is in the fleet — but every
rendered config was verified to LOAD in real llama-swap v242, and each tier's seats were
chosen for what its hardware can actually serve rather than copied blindly:
- **`blackwell-8`** — CUDA twin of the proven `ampere-8`; its seats verbatim (the tier's
  `gpu_env` pins the device on both).
- **`cpu`** — reuses the resident E4B + mmproj (no separate VLM download, `ampere-6`'s
  approach) and CPU whisper (whisper.cpp is CPU-native). The vision seat renders with
  neither `-ngl` nor `--flash-attn`, matching the cpu template's own chat models.
- **`amd-gcn`** — STT via the whisper.cpp Vulkan build (solid). Vision keeps the CLIP
  encoder on CPU (`--no-mmproj-offload`) because mmproj on the Vulkan backend is degraded
  ([llama.cpp #20081](https://github.com/ggml-org/llama.cpp/issues/20081)); the LLM still
  decodes on the iGPU. Both pin the Vulkan ICD.
- **`dual-gpu`** — resident seats (the template has no swap group) pinned to the **editor
  card** (`CUDA_VISIBLE_DEVICES=1`) so they never contend with the 26B architect on device 0.

### Added — two per-seat schema fields the above needed (ADR 0019 extended)
- **`gpu_env`** (per-seat) — env vars for a seat that must sit on a SPECIFIC device the
  tier's other models do not (the dual-gpu editor card; a Vulkan ICD). Distinct from the
  tier-level `gpu_env`, which applies to every model uniformly.
- **`no_mmproj_offload`** (vision only) — keeps the CLIP encoder on CPU where the LLM
  backend degrades it. Refused on an stt seat.
- The vision seat is now **backend-aware**: on the `cpu` backend it omits `-ngl` and
  `--flash-attn`, so it renders the same shape as the cpu template's chat models instead
  of a cosmetically-wrong one a GPU-less build would merely ignore.

## [0.39.0] - 2026-07-28

### Fixed — both RAM gates were inert on Linux, and one had never worked there at all
`ram_tier` gates two things: the 26B placement (`--cpu-moe` puts EVERY expert in RAM, so
it is dropped without a real RAM path) and the RAM-gated media seed
(`config_seed_ram_mid_high`). detect.ps1 has always emitted a ram tier; the Go side
reported raw `ram_gb` only, so `install.sh` had nothing to pass and **neither gate ran**:
- a Linux install of `ampere-8` / `blackwell-8` / `amd-rdna3` / `cpu` served the 26B on a
  box with no RAM path for it;
- and `install seed` never received a ram tier either, so the mid/high-only image seed had
  **never applied on Linux** — a capability the tier promises and the OS silently withheld.

- **`hwdetect.RAMTier`** ports detect.ps1's `Get-RamTier` thresholds (>=120 high, >=56 mid,
  >=28 low, else min). `TestRAMTierMatchesTheInstallerTable` asserts the SAME values
  detect.ps1's own self-test does, including the boundaries — 56 GB is what unlocks the
  26B and 55 must not.
- **`Classify` stamps `ram_tier` in one place**, not per return path: an empty value means
  "do not gate", so a verdict that forgot it would disable both gates without failing.
  `TestClassifyAlwaysStampsARAMTier` covers every profile branch including zero facts.
- **`install detect --json` carries `ram_tier`** on the verdict, and **`install.sh` passes
  it to both `install seed` and `install render`** — and **dies** if detect returns none
  rather than installing with the gates off.
- Verified on the real 6 GB Linux node: `ampere-6, ram_tier=low, 31 GB`, installer dry-run
  clean. Verified on the Linux path for a gated tier: `ampere-8 --ram-tier low` drops the
  26B and withholds the image seed; `mid` applies both.

### Added — the installer's own test suites are in CI
`render.tests.ps1` and `detect.tests.ps1` drifted silently for two releases (0.36.0 moved
the templates to `matrix:`; three assertions kept asserting `groups:`) because nothing ran
them. A `windows-latest` job now runs both, and the ubuntu job gained the ~170 `node --test`
render-runner assertions that were never in CI either. Both suites are synthetic — no
hardware detection, no downloads — so a runner is a valid host.

## [0.38.0] - 2026-07-28

### Fixed — a fresh Windows install rendered a config llama-swap REFUSES to load
Both verified against real llama-swap v242, not inferred. Live nodes were spared only
because Step 6 skips when a config already exists; a **fresh** install would not have started.
- **0.36.0 onward:** the PowerShell renderer removed the 26B model block but left its matrix
  `m26` var behind on any tier that drops the 26B → `matrix: var key "m26" references unknown
  model "gemma4-26b-a4b"`.
- **0.37.0 onward:** additionally left literal `__SEATS_RESIDENT__` / `__SEATS_SWAPPABLE__` /
  `__M26_ALT__` inside matrix set expressions, on **every** tier.
Root cause of both: `setup/install.ps1` had its own renderer, which never learned what the
templates had become.

### Changed — `install.ps1` delegates the render; there is now ONE renderer (ADR 0021)
- Step 6 calls `local-offload install render`. The wrapper keeps only what it alone knows —
  resolved `ram_tier`, physical-core count, install paths — and passes them as flags. Its
  renderer internals are gone: `Remove-26bFromYaml` and `Add-GpuEnvToYaml` deleted.
- **`gpu_env` is now a TIER FIELD.** The Blackwell CUDA env (`CUDA_VISIBLE_DEVICES=0`,
  `CUDA_MODULE_LOADING=LAZY`) was an `if ($profileId -match '^blackwell-')` branch inside
  install.ps1, so a **Linux** install of the same tier silently went without it. Same tier,
  same treatment, on any OS — which is what a tier is for.
- **`--ram-tier` implements the 26B RAM gate once.** `cpu_moe` puts EVERY expert in RAM, so
  it is dropped on low/min. The Go renderer had no ram-tier input at all, so Linux served the
  26B on boxes with no RAM path for it. An empty value means "do not gate", so nothing changes
  until a caller passes one — `install.sh` still does not, and that is the next gap.
- **`--fallback-backend`** carries the off-matrix defaults install.ps1 used to hold, so an
  unrecognized box still renders a valid config instead of failing.
- `-RenderOnly` is no longer build-free (it resolves a renderer: `$env:OFFLOAD_HARNESS_EXE`,
  else the installed exe, else a `go build` into a temp dir). Still touches no install
  artifact. `render.tests.ps1` builds once and points every case at it.

### Added — `ampere-8` declares its measured vision + STT seats
Parked in 0.37.0 because install.ps1 refused them; the delegation unblocks it. Reproduces the
8 GB reference laptop's proven live config, including `--image-max-tokens 1024` (the VRAM
bound) and whisper's `-nfa`. Verified: `install.ps1 -RenderOnly` on `ampere-8` now succeeds and
the config it writes is **accepted** by llama-swap v242, serving `qwen3vl-4b` + `whisper-stt`.
`ampere-6` is likewise installable on Windows again.

### How the delegation was proven safe
Every profile × backend × ram-tier rendered by BOTH renderers — 34 configs — and compared
line by line ignoring comments: **zero unexplained differences across all 32 comparable
pairs**. That gate caught three regressions before merge: the renderer classifying the LOCAL
machine when asked for off-matrix defaults, `--cpu-moe` emitted without its `-ngl 999`, and
`blackwell-8` missing the CUDA env. Also fixed along the way: `drop26B`'s post-check refused a
valid config because a template COMMENT names the 26B, and three `render.tests.ps1` assertions
had gone stale against `groups:` in 0.36.0 (that suite is not in CI, so it drifted silently).

## [0.37.0] - 2026-07-28

### Added — every serving template can place media seats (the Windows half of W4)
- **All five `win-*` templates carry an `# offload-seats:` directive and `__SEATS_*__` markers.**
  Until now only `llama-swap.linux-cuda.yaml` could place a seat, so a tier declaring one was
  refused on Windows — which meant no further tier could declare seats at all. A tier is a
  HARDWARE class, so this is the invariant that makes that true in practice, and
  `TestEveryTemplateCanPlaceSeats` now fails CI if a template loses the ability.
- **The two all-resident templates accept only the `resident` role.** `win-cuda-resident` and
  `win-dual-cuda` exist because nothing swaps on those tiers; a `swappable` seat contradicts that,
  so it is refused BY NAME rather than quietly reshaped into something the operator did not ask for.
- **Three measured seat knobs the schema could not express:** `image_max_tokens` (a VRAM bound, not
  a quality knob — it is what keeps one large screenshot from pushing the seat past an 8 GB card),
  `no_context_shift`, and `no_flash_attn` (whisper.cpp has flash attention default-ON since v1.8.0
  and it DEGRADES non-English and noisy audio, so a tier serving Spanish asks for `-nfa`). Each is
  refused on the kind it does not apply to, rather than silently ignored.
- **`install render` now warns, by exact path, when a declared seat's weights are not on the box.**
  llama-swap builds `/v1/models` from the CONFIG, not the filesystem — so a seat whose `.gguf` was
  never downloaded still appears in the roster, `doctor` and `acceptance` both PASS on the alias,
  and the route fails only when called. Install time is when that is still cheap to fix.

### Fixed — the tier pages understated two tiers' media
- **`ampere-8` and `blackwell-8` claimed "This tier ships no media configuration"** while the
  installer bound an image seat on any mid/high-RAM box: `tierdocs` read `config_seed` but not
  `config_seed_ram_mid_high`. One of those two is the 8 GB reference laptop, and the pages are the
  collaborator-facing artifact. Both now state the RAM gate explicitly.

### Deliberately NOT shipped
- **`ampere-8`'s seat declaration is parked, not live** (`_media_seats_pending_installer_delegation`
  in the tier table). The seats are measured and reproduce the reference laptop's live config
  exactly, and `local-offload install render --profile ampere-8 --os windows` renders them correctly
  today — verified accepted by real llama-swap v242, serving `qwen3vl-4b` + `whisper-stt`. But
  `setup/install.ps1` renders llama-swap with its own PowerShell substitution and never reads
  `media_seats`, so declaring them makes it refuse: confirmed with `install.ps1 -RenderOnly` on this
  exact tier, which throws. Shipping the declaration would have made the 8 GB tier uninstallable on
  Windows. **Next task: make install.ps1 delegate the render to the Go verb** — which requires
  Step 7 (build) to precede Step 6 (render) and changes `-RenderOnly`'s documented no-build
  contract, so it is its own reviewed slice rather than a tail-end edit.

## [0.36.0] - 2026-07-28

### Changed — residency is declared with `matrix:`, not legacy `groups:` (all six templates)
- **The semantics did not hold.** On the workstation node, under `groups:`, an
  `exclusive: true` group evicted a `persistent: true` group anyway — verified live on
  2026-07-26. `bge-reranker-v2-m3` could not start, a rerank returned HTTP 000 after 90 s, and
  the memory stack **silently fell back to dense-only ordering** while its admission thresholds
  stayed calibrated to the reranker's logits. That node was hand-migrated the same day; the repo
  templates were not, so every tier still rendered the topology that had just been disproven.
  llama-swap also replaced groups with matrix upstream in v202.
- **Sets are the valid CONCURRENT COMBINATIONS, and residents appear in every set** — so there
  is no combination in which the memory stack is absent, and no request can be satisfied by
  evicting it. `evict_costs` is now a second line of defence rather than the first.
- **Three templates were strictly worse than the node that broke.** `win-cuda`, `win-vulkan` and
  `win-cpu` declared `offload-family` as `swap: true` + **`exclusive: true`** with
  `embeddinggemma` in **no group at all** — an exclusive group unloads everything outside
  itself, so loading any chat model evicted the embedder. Fixed.
- **`win-cuda-resident` declared no residency whatsoever.** The ALL-RESIDENT tiers
  (`blackwell-32/48/72`) never stated that they are all-resident; it was left to llama-swap's
  default for ungrouped models. Now declared explicitly.
- **Constraints verified against the real binary, none of them in the upstream README:** a
  matrix must define at least one var; a var key must be **alphanumeric and 1-8 characters**
  (`embeddinggemma` is rejected outright); and a set expression may reference **only var ids**,
  never a model id. A seat's var key is therefore derived from its KIND (`vis`, `stt`) — stable
  and unique because a tier may declare at most one seat per kind.
- **26B and seat membership are tokens, not expression edits** (`__M26_ALT__` / `__M26_AND__`,
  `__SEATS_SWAPPABLE__` / `__SEATS_RESIDENT__`): a swappable member joins as an ALTERNATIVE
  (`|`), a resident one as a CO-RESIDENT (`&`), and only the template knows which it means.
- **`TestNoTemplateStillDeclaresLegacyGroups`** fails CI if a template regresses to `groups:`
  or ships no `matrix:`.
- **Nothing changes on a running node.** These render at INSTALL time; the live configs on both
  active nodes are hand-maintained and the repo never writes their paths. ADR 0020 records this,
  including the one way it could bite (rendering a template over a hand-tuned live config).
- Validated before merge on real binaries: all five Windows templates on **v242**, and the Linux
  template on **v208** — the oldest in the fleet, chosen as the floor test — which served the
  full roster including both media seats.

## [0.35.0] - 2026-07-28

### Fixed — a phantom binding on every node in the fleet (the defect W4 slice 2 uncovered)
- **`stt_model` no longer defaults to `"whisper-stt"`.** Not one of the six serving templates
  defines a whisper seat, so the default bound EVERY rendered node to an alias its own config could
  not serve: the node advertised `stt` to the fleet dispatcher (`internal/fleetnode` gates that task
  on `STTModel != ""`) while `local-offload acceptance` failed it on the same alias. Measured with
  the new closure gate: **20 of 20 (tier, OS) pairs red; `stt_model` was the only binding that
  failed.** `vision_model` was fixed this way already ("opt-in … no phantom"); this finishes that
  pass. **Migration:** a node whose llama-swap genuinely serves a hand-provisioned `whisper-stt`
  must now say so in its own config — the binding is explicit or it is absent.
- **`TestEveryBoundAliasIsServed`** — the cross-artifact gate that was missing. For every tier and
  every OS it renders for, every non-empty alias the harness config binds must be a seat the
  rendered serving config defines. The two artifacts are written by two commands from one table;
  nothing had ever checked them against each other.

### Added — tier-declared media seats (W4 slice 2)
- **`internal/mediaseat`** — typed, validated declarations of the ALIAS-backed media capabilities a
  hardware tier serves (`kind: vision|stt`), and the single source of the config binding that routes
  to each. One declaration produces the llama-swap seat AND `vision_model`/`stt_model`, so the state
  "bound to a seat that does not exist" is no longer representable from the tier table. `config_seed`
  is now refused if it writes either key — two writers is how they drifted apart.
- **`internal/servingtmpl` renders seats** into the models map *and* the group its residency role
  maps to. Both, always: llama-swap rejects a config whose group names an undefined model, and a
  model in no group joins the implicit default group, which swaps and evicts.
- **Residency is a ROLE (`swappable`/`resident`), never a group name.** Group names are per-template
  (`heavy`/`support` here, `offload-family` in three win-* templates, `architect`/`editor` in
  win-dual-cuda, none in win-cuda-resident) while a tier is a hardware class that renders into
  several of them. Templates map roles to their own groups with `# offload-seats:`.
- **A tier declaring seats against a template that maps no roles is REFUSED BY NAME**, never
  silently dropped — silent capability loss across an OS boundary is the failure this workstream
  exists to end. Windows tiers therefore refuse loudly until a win-* template maps roles.
- **`ampere-6` declares its vision + STT seats**, reproducing the 6 GB reference node's proven live
  seats: the vision seat carries its own 8K window (not the chat tier's 32K) and whisper-server
  carries its own `LD_LIBRARY_PATH`, because it is a separate self-built binary.
- **`install render --home`** supplies the install root for seat paths (`__OFFLOAD_HOME__`), and
  refuses rather than rendering an empty root into an absolute path that fails at exec.
- **`docs/tiers/` surfaces seats** — the per-tier page and the summary table were computed from
  `config_seed` alone, so a tier's alias-backed capability was invisible where collaborators read it.
- **Both halves of an install refuse together.** `install seed` now asks whether the target's
  template can place the tier's seats BEFORE writing their bindings. Without this, `install seed
  --os windows` happily wrote `stt_model`/`vision_model` for a target whose `install render`
  refuses — reproducing the phantom binding from the same table by a different route.
- **`setup/install.sh` passes `--home`**, without which the Linux install of a seat-declaring tier
  aborted at the render step *after* having already written a config bound to those seats' aliases.
- **`setup/install.ps1` refuses a seat-declaring tier by name.** It is a second, older renderer that
  does not read `media_seats`, so it would have installed a Windows node silently missing the vision
  and STT seats its own tier row promises — silent capability loss across an OS boundary.
- **Seat names and aliases are charset-validated.** A name becomes a YAML key *and* an inline
  flow-sequence member: `a,b` split into a member naming no model (a config llama-swap rejects at
  startup) and `a:b` produced a malformed document. Kind-irrelevant fields (`bin` on a vision seat,
  `mmproj` on an stt seat) and `__OFFLOAD_HOME__` in a models-dir-relative field are refused rather
  than silently ignored.
- **Two string-surgery fragilities fixed in `internal/servingtmpl`:** an indented comment containing
  a colon between a group key and its `members:` line made seat placement report the group had no
  members list (and the shipped template's own house style is full of such comments); and dropping a
  26B declared LAST swallowed `groups:` and the seat directive with it.
- **ADR 0019** records the decision, the measurement, and the four rejected alternatives —
  including why image/video/music stay out of this schema (there is no `sd-server` client in the
  repo, and hosting one in llama-swap would put its group machinery and the ADR 0018 lease in charge
  of the same card at once).

## [0.34.0] - 2026-07-28

### Added — a supported Linux install path (W3, final slice)
- **`setup/install.sh`** — thin by design: `install detect` picks the tier, `install volumes` the
  disk, `install seed` the media bindings, `install render` the serving config, and `acceptance`
  gates the result **as the service identity**. Every decision lives in the binary; the script only
  fetches, places and registers. `--dry-run` prints all of it and changes nothing.
- **The tier table is now EMBEDDED** alongside the serving templates. An install starts by fetching
  one binary onto a machine with no checkout, so a repo-relative `profiles.json` is the development
  case. This was found by dry-running: the lookup failed silently and the script wrote a config with
  **no media bindings at all**, saying nothing. A seed failure is now fatal.

### Fixed
- **Tiers that DROP the 26B no longer route escalation at it.** `ampere-6` and `amd-gcn` set
  `include_26b: false`, but their seeds left `escalation_model`/`reasoning_model` at the built-in
  `gemma4-26b-a4b` default — so a fresh install escalated to a model the node does not serve, and
  every escalation failed. The measured Linux node only avoided this because a human noticed once and
  cleared it by hand; that knowledge now lives in the tier. Caught by running the installer end to
  end: the acceptance gate reported `2 alias(es) not served`, and reports them live after the fix.
- **`.gitattributes` pins `*.sh`, the serving templates and generated docs to LF.** A shell script
  checked out with CRLF fails at exec with `env: 'bash
': No such file or directory`, naming
  neither the script nor the cause. This repo is developed on Windows and deployed to Linux, so that
  is the normal path, not an edge case — hit twice while testing this slice.
- `install.sh` warns loudly when no `render/` tree sits beside the binary, instead of leaving the
  acceptance gate to explain why every media route is BOUND-BUT-MISSING.

## [0.33.0] - 2026-07-28

### Added — `local-offload acceptance`, the gate a node passes before it is handed work (W5)
- Both 2026-07-27 fleet failures **passed `doctor` cleanly** while every dispatched job died, because
  doctor stats configured files. A Windows node's venv `python.exe` existed and was readable but is a
  **uv trampoline** re-execing a base interpreter in another account's roaming profile; a Linux
  node's GPU lease directory existed and was readable but was owned by a different user. Neither is
  detectable by stat — only by ATTEMPTING the thing as the running identity.
- So every check **exercises** the capability: it writes a probe file into the lease directory and
  removes it, and it EXECUTES each bound interpreter (`node`, `ffmpeg`, the PIL python, `sd-cli`).
  It also requires that no derived media route is `BOUND-BUT-MISSING` and that every configured
  model alias is in the live roster. Unbound capabilities `SKIP` and never make a node look unready.
  Non-zero exit means the node must not be handed work.
- **The report leads with the identity**, because in both failures the binary, config and files were
  all correct and only the account was wrong.
- **Capability is identity-dependent, and the gate makes that visible.** Verified on the measured
  Linux node: run as the install owner it reports the PIL engine `PASS`; run as the service account
  (`sudo -u fleet`) the same binary and config report `SKIP`, because that user cannot see the
  ComfyUI venv. Both identities are otherwise READY — which also independently confirms the lease
  ownership fix applied on that node.

## [0.32.0] - 2026-07-28

### Added — a node advertises what it can actually deliver (`vram_reclaimable_gb`)
- **`/fleet/health` gains `vram_reclaimable_gb`, `vram_schedulable_gb`, `vram_reclaim_source` and
  `harness_version`.** Requested by the fleet-dispatcher side, which was otherwise going to
  approximate it: neither published number is a safe divisor. `vram_total_gb` over-counts every
  shared card (the measured workstation's desktop plus its always-resident support tier hold ~3 GiB
  that cannot be reclaimed at any price) and `vram_free_gb` under-counts a WARM node, whose loaded
  swappable model looks like lost capacity. `vram_schedulable_gb` (free + reclaimable) is the
  number to divide by.
- **Measured on a 16 GiB workstation, before/after loading one 4 GiB seat:** free 12.77 → 8.74,
  reclaimable 0 → 4.04, **schedulable 12.77 → 12.78**. Free collapses; schedulable stays flat.
- **How it is derived, since two obvious mechanisms fail.** Per-process GPU memory
  (`nvidia-smi --query-compute-apps`) returns `[N/A]` on Windows — exactly the node with the shared
  desktop — and the footprint store records what a RENDER task peaks at, not what the text tiers
  hold. So the node measures an **idle baseline** (used VRAM while no swappable seat is loaded and
  the GPU lease is free) and reports what sits above it. The baseline IS the unreclaimable share,
  measured rather than assumed.
- **Always-resident seats are baseline, not capacity.** The support tier is co-resident on purpose —
  unloading it is what made one RAG query pay three model loads — so seats llama-swap reports with
  `ttl 0` are counted into the baseline. Found before shipping: without this rule a *correctly*
  configured node never reaches "nothing loaded" and would advertise `unknown` forever.
- **Unknown is published as absence.** Before any baseline is observed both numbers are omitted and
  only the source string is sent, so a consumer falls back to free VRAM instead of acting on a
  guess. The node never claims reclaim capacity while holding nothing, so a third party's
  allocation cannot be mistaken for our own model.
- Sampling runs in the background (5 s); the health handler never blocks on llama-swap.

## [0.31.0] - 2026-07-27

### Added — a Linux node can render its own serving config (W3, second slice)
- **`setup/templates/llama-swap.linux-cuda.yaml` + `internal/servingtmpl` + `install render`.**
  Rendering lived in `install.ps1`, so a Linux node could not produce a serving config at all and
  every Linux deployment hand-wrote one. The templates are **embedded in the binary**, so a fetched
  binary renders a config on a machine with no checkout.
- **The template is not a translation of the Windows one.** Two Linux-specific things are
  load-bearing: `LD_LIBRARY_PATH` on every seat (a self-built `llama-server` links its own shared
  objects; without it the process dies at exec with a loader error that reads nothing like a config
  problem), and the group topology.
- **The topology is MEASURED, and now enforced by a test.** `heavy` is `swap:true,
  exclusive:false`; `support` is `swap:false`. On the 6 GB node `exclusive:true` on a swapping tier
  meant the loaded seat evicted everything and nothing evicted it — every chat request returned 502
  for the full 5-minute TTL after any render; and with the embedder inside the swapping tier a
  single RAG query paid three full model loads (free VRAM 3655 → 1005 MiB, because loading the
  embedder had evicted the chat model). `TestHeavyGroupIsNeverExclusive` keeps both.
- **Rendering refuses to emit a config that still contains a token.** `install.ps1` has a comment
  about that exact failure; a llama-swap started with a literal `--ctx-size __CTX__` fails looking
  like a model problem. A tier that drops the 26B loses its model block AND its group membership
  together — llama-swap rejects a config whose group names a model that does not exist.
- **Verified end to end on the Linux node:** the rendered `ampere-6` config was handed to that
  node's own `llama-swap` on a throwaway port, which accepted it and listed exactly `offload-e4b`,
  `gemma4-e2b`, `embeddinggemma`, `bge-reranker-v2-m3` — the 26B correctly absent. The live service
  on `:11436` was untouched.

## [0.30.0] - 2026-07-27

### Added — a machine can be classified without PowerShell (W3, first slice)
- **`internal/hwdetect` + `local-offload install detect` / `install plan`.** `setup/detect.ps1`
  refuses to run off Windows (`if ($os -ne 'windows') { Write-Error ...; exit 1 }`), so a Linux box
  could never be told what it IS — and its serving topology, resident tier and media bindings had to
  be hand-derived. On the measured Linux node the first two hand-derived topologies were both wrong
  in ways that broke chat.
  - `Classify` is a **straight port of `Get-Profile`** and `ArchFromName` of `Get-Arch`, rule order
    included (the order IS the logic: "RTX PRO 5000 Blackwell" must not fall through to the RTX-50xx
    rule it does not match). Both are asserted against the SAME table `detect.tests.ps1` uses — 14
    arch cases and 20 profile cases — so a machine's tier cannot depend on which implementation asked.
  - Detection prefers `nvidia-smi`, then falls back per OS (CIM on Windows, DRM sysfs + `lspci` on
    Linux). An AMD box that cannot be identified must never be silently called `cpu`: that would
    strip it of the entire Vulkan serving path. Covered by a test.
  - `install plan` composes the two halves that already existed — the classified tier and the media
    seed that tier renders for this OS — into "what would an install do here", read-only.
  - **Verified on the whole fleet:** <node-b> → `blackwell-16` (RTX 5060 Ti, 15.9 GB), <node-a> →
    `ampere-8` (RTX 3070 Laptop, 8 GB), Linux node → `ampere-6` (RTX 3050, 6 GB). Each matches the
    tier that box actually runs, and the Linux one had no way to answer at all before.

## [0.29.0] - 2026-07-27

### Added — a tier is a hardware class, not a Windows class (W4, first slice)
- **`internal/tierseed` + `local-offload install seed`** resolve a tier's `config_seed` for a
  TARGET machine: `__OFFLOAD_HOME__` expands to the install root and the new `__EXE__` token to
  `.exe` on Windows and nothing elsewhere, so one table row renders on every OS. `--os` lets a
  Windows box render a Linux node's fragment. The seeds previously lived only inside
  `install.ps1` — Windows-shaped and unreachable from any non-Windows install.
- **`vae_mode: tiling|cpu|none`** replaces free-text `sdcpp_extra_args` for the VAE lever, and is
  **refused as `cpu` on a CUDA backend** with the measurement in the error (7.8× slower: 58.2 s vs
  7.5 s). It is correct on an AMD/UMA part — free text is exactly how it would have spread to a
  tier it is wrong for.
- **Seed keys are validated against the real `config.Config` fields.** A typo'd key is dropped by
  the loader with only a warning, on every install of that tier; it is now refused at authoring.
- **`ampere-6` finally has a media seat** — modelled on the configuration proven on the measured
  6 GB node (sdcpp + SDXL-Turbo Q4_0, 4 steps, cfg 1.0, VAE tiling, since the VAE and not the UNet
  is the 6 GB wall). 9 of 14 tiers now ship media configuration, up from 8.

### Changed
- The two `amd-rdna3*` seeds carry `sd-cli__EXE__` instead of `sd-cli.exe`, so
  **`crossplatform_lint_test.go` has no grandfathered exceptions left** — the "no `.exe` in a tier
  seed" rule is now absolute.
- `TestEveryShippedSeedIsValid` resolves every tier for both platforms: the gate that would have
  caught `sd-cli.exe` in the first place.

## [0.28.0] - 2026-07-27

### Added — an install can move to the volume that has room
- **`home` (config) / `$LOCAL_OFFLOAD_HOME` (env)** is the install root every DERIVED path hangs
  off: cache, ledger, media and svg output, exemplars, thresholds, the router and confhead stores.
  Precedence: an explicit value for a key always wins, then `home`, then the env var, then
  `~/.local-offload`.
  - This is the missing half of `install volumes`. Choosing a volume is pointless if living there
    means hand-writing a dozen absolute paths into the config — the hand-patching that put a model
    tree on an OS drive and let bindings drift from the binary. Moving an install is now: copy the
    tree, set `home`.
  - **The machine-wide state root is deliberately NOT rebased.** `state_dir` / `gpu_lock_path` stay
    unset so `internal/gpulease` resolves them machine-wide; putting the GPU lease under a home
    directory is the per-user trap that silently un-serializes the GPU (0.24.1 added a warning for
    it). Covered by a test, not just a comment.
  - Paths whose defaults do not hang off the install root — `node`, `ffmpeg`, `comfy_dir` — are
    untouched by construction.

## [0.27.2] - 2026-07-27

### Added — the prompt shape that makes prefix reuse work is now enforced
- **`internal/tasks/prefixorder_test.go`** locks the property every text task depends on: the
  variable input must be the SUFFIX of the user message (exactly once), and the system prompt must
  be byte-identical across two different inputs. Measured on <node-b> against `gemma-4-e4b`: a
  2037-token prompt re-sent with the same leading instructions and a different payload re-prefilled
  only **41** tokens (`cache_n` 1996), turning a 7.1 s call into **0.30 s**. That saving is one
  `fmt.Sprintf` argument order away from being lost SILENTLY — nothing would fail, every call would
  just quietly pay full prefill again.

### Documentation
- `docs/systems/offload-pipeline.md` gains the measurement, the invariant, and two explicit
  non-conclusions: it is **not** a reason to add `--swa-full` to the small tiers (that flag is
  load-bearing for the large iSWA models and is set for them; the table above was measured against
  a `gemma-4-e4b` entry WITHOUT it, and forcing a full window costs KV memory on the tiers with
  least to spare), and it re-tunes **no** defer or escalation gate, since those are
  confidence-driven rather than latency-driven. Also records the benchmarking rule: use the
  server's `timings.prompt_n`, never process RSS — an mmap'd model's weights live in the page
  cache, so an RSS-based "did it restart?" check reports phantom restarts.

## [0.27.1] - 2026-07-27

### Added — every hardware tier is documented IN THE REPO
- **`docs/tiers/`**: an index plus one page per tier (14 today), so anyone who downloads the repo can
  read what their own machine's class gets — served window, KV type, backend template, resident
  model, 26B placement, whether the tier ships a media `config_seed`, and the operator notes recorded
  against it (many of which are measurements from real hardware, including the reasons a tempting
  change was deliberately NOT made). Until now that lived only as a JSON blob inside the installer,
  which is a large part of why collaborators' installs drifted.
- **The capability report is part of that documentation.** Every tier page ends with how to produce
  one for a specific machine (`local-offload report`), what its three verdicts mean, and links to any
  checked-in example for that tier. `docs/tiers/reports/` carries three real ones — <node-b>
  (`blackwell-16`), the <node-a> laptop (`ampere-8`) and the <node-c> node (`ampere-6`) — committed verbatim, and says
  plainly that all three report `hardware tier: UNKNOWN` because none was installed by `install.ps1`
  into the default `$OFFLOAD_HOME`.
- **The pages are GENERATED and gated.** `cmd/gentiers` renders them from
  `setup/templates/profiles.json` (`go generate ./...`), and `TestTierDocsAreCurrent` fails the build
  when the checked-in tree is stale — including an orphan page for a tier that no longer exists. This
  is LO-17's `config.example.json` lesson applied to documentation; the gate was verified by injecting
  drift into a page and watching it fail, then pass after regeneration.
- The index makes one gap visible at a glance that was previously buried: **6 of 14 tiers ship no
  media seat at all** (`amd-gcn`, `ampere-6`, `ampere-8`, `blackwell-8`, `cpu`, `dual-gpu`), so those
  machines serve text only until an operator binds media by hand.

## [0.27.0] - 2026-07-27

### Added — the installer can stop defaulting onto the OS drive
- **`local-offload install volumes`** decides where a machine's harness, models and media should
  live, from one policy: never removable or network media; never below a floor (20 GiB free by
  default — a tier's media set alone exceeds 12 GiB); prefer ANY qualifying volume over the OS
  volume, which is selectable only with `--allow-os-volume` and is then recorded as a deliberate
  choice; among the rest most free space wins.
  - **Ties break on path depth, then name.** On ZFS every dataset of a pool reports the same free
    space, so a name-only tie-break puts the harness under whatever sorts first — measured on the
    Lenovo, that is `/srv/apps/adventurelog` rather than the pool root.
  - Selection (`internal/volumes.Pick`) is **pure and unit-tested**; only enumeration is
    platform-specific (kernel32 on Windows, `/proc/mounts` + `statfs` on Unix), so the policy cannot
    drift between operating systems. Free space uses the UNPRIVILEGED figure on Unix (`Bavail`, not
    `Bfree`) — reserved blocks are not space an install can use.
  - Verified on both: <node-b> picks `V:` (its weights volume, 308 GiB free) over `C:` with 640 GiB free
    because `C:` is the OS volume; the <node-c> node picks `/srv/apps` out of **39** enumerated
    filesystems.
  - `--json` carries the full enumeration plus `{volume, because}` for wrapper consumption; the
    console view shows the roomiest few and says how many it withheld.
- `install` is a verb GROUP: the installer's decisions move into the binary so PowerShell and shell
  wrappers only fetch and place files. Wiring the choice into `install.ps1` and `installed.json`
  needs the bootstrap to fetch the binary before it picks a target and lands with the detection
  move — this release is the decision engine those wrappers will call, and the answer an operator
  can already act on.

## [0.26.0] - 2026-07-27

### Added
- **`local-offload report` — the answer to "what can that machine actually do?", generated by the
  machine.** That answer has been assembled by hand over chat every time a collaborator hit trouble,
  and the hand-assembled version was wrong more than once: one node reported a ComfyUI it did not
  have, another had a media route bound to a file that was not there. `report` emits one read-only
  Markdown document — harness version and platform, the config file ACTUALLY loaded (BUILT-IN
  DEFAULTS is disclosed, since a report built on defaults describes bindings that are inactive), the
  hardware tier from the installer manifest, every configured alias measured against the live
  `/v1/models`, and every media route with its derived verdict.
  - It renders from the SAME code paths the harness routes on (`doctor`'s alias diff,
    `mediacap`'s derivation), so a report cannot claim a capability the box would defer.
  - A **Needs attention** section lists ONLY `BOUND-BUT-MISSING` routes. A route this box never
    bound is a legitimate machine, and telling someone otherwise is how a real defect gets lost in
    a wall of non-problems.
  - Read-only by construction: no model load, no GPU work, one health probe and one `/v1/models`
    GET. A dead endpoint still produces a usable document, because the media verdicts are pure
    config + filesystem.
  - `--out FILE` writes it; otherwise it goes to stdout. An unknown hardware tier says UNKNOWN with
    the reason rather than guessing — the tier drives every serving decision downstream.

## [0.25.1] - 2026-07-27

### Fixed — the harness can drive its engines on Linux, not just Windows
- **ComfyUI was unlaunchable on any Linux node.** `render/comfy-lifecycle.mjs` probed the venv
  interpreter at Windows paths ONLY (`.venv/Scripts/python.exe`, `venv/Scripts/python.exe`,
  `python_embeded/python.exe`) and otherwise fell back to a bare `python` — absent on Ubuntu, or the
  system interpreter without torch. `resolveComfyPy` now probes both families, **Windows candidates
  first** so Windows resolution is byte-identical, and falls back to `python3` off Windows.
- **`run_graph` spawned a path that cannot exist there.** `comfy-run-graph.mjs` hardcoded
  `.venv/Scripts/python.exe` with no candidate list and no existence check; it now shares
  `resolveComfyPy`/`resolveComfyDir` with the lifecycle module.
- **`comfy_dir` defaulted to `C:/ComfyUI` on every OS** (both the Go config and the runner). A Linux
  node therefore reported a ComfyUI install it cannot have — and, because the ComfyUI-backed scripts
  are bound by default, its `/fleet/health` advertised `video-gen`, `audio-gen` and `run-graph`, all
  of which fail on arrival. The default is now `C:/ComfyUI` on Windows and **unbound** elsewhere, so
  `doctor`/`offload_status` report NOT CONFIGURED (a legitimate machine) instead of a promise the box
  cannot keep. `ensureComfy` refuses an unbound `comfy_dir` with that reason rather than spawning
  into a nonexistent cwd.

### Added
- **`crossplatform_lint_test.go` — the class fails in CI now, not on a node six weeks later.** Three
  rules: a runner probing a Windows venv interpreter must probe the POSIX one too; a drive-letter
  literal in shared Go needs a `runtime.GOOS` branch; a tier `config_seed` may not name a `.exe`.
  Verified against the pre-fix blobs — rules 1 and 2 fire on exactly the code this release fixes.
  The two `amd-rdna3*` seeds that still carry `sd-cli.exe` are recorded with their reason (per-OS
  binary rendering is the tier-schema workstream), so a NEW offender fails immediately.

## [0.25.0] - 2026-07-27

### Changed — two contracts moved, which is why this is a minor bump
- `offload_status`'s media block **no longer carries** `image_engine`, `video_engine`,
  `audio_voice_engine`, `audio_music_engine`, `edit_pil`, `edit_gimp` or `media_ffmpeg`. Everything
  they claimed now lives in `media.routes`, derived per machine. `image_ckpt`,
  `video_upscale_model` and `svg_engine` stay.
- `doctor` **exits non-zero on a media route bound to a file that does not exist**, where it
  previously ignored media entirely. `setup/selftest.ps1` already treats a non-zero doctor with a
  healthy `health:` line as a warning rather than a gate, so an install does not fail on it.

### Fixed — capability is DERIVED now, on both surfaces that report it
- **`offload_status` no longer declares an engine it may not have.** Its media block hardcoded
  `"image_engine": "ComfyUI (local)"`, `"video_engine": "ComfyUI Wan 2.2 I2V …"` and the rest as
  constants, and handed them to an **autonomous planner** on a node whose `imagegen_engine` is
  `sdcpp` and which has no ComfyUI installed at all. It now reports `media.routes`, derived from
  this machine's bindings by the new `internal/mediacap` — the right pattern already existed 30
  lines away in `fleetnode.taskConfigured` and `config.ImageRouteConfigured`. Exposing a *declared*
  capability map to something that acts on it is strictly worse than exposing none.
- **`doctor` gained a media section — the change that ends "doctor is green but `generate_image`
  defers".** It checked model aliases only, so a box whose render script was not on disk passed.
  Each route is now `CONFIGURED` / `NOT CONFIGURED` / `BOUND-BUT-MISSING`, and only the last is a
  failure (non-zero exit): a box that never bound a route is a legitimate machine, a box that bound
  one to a file that does not exist is a promise it cannot keep.
  - Verdicts are computed with the runners' own rule — a relative script binding resolves against
    the **executable's** directory (`gpugen.ResolveScriptIn`, exported so the check cannot drift
    from `ResolveScript`), an executable binding by stat when it is a path and by PATH lookup when
    it is a bare name (`node`, `ffmpeg`) — so a verdict answers the same question the runner asks.
  - `node` and `comfy_dir` are reported as prereq rows only when a bound route needs them: an
    sdcpp-only node is never told it is missing ComfyUI.
  - The section prints **before** the health probe and independent of it. A doctor that is loud
    about llama-swap and silent about a broken media binding is the same blind spot in a new
    disguise.

### Fixed — docs
- **`--ctx-tokens` is documented as `0 = AUTO` again** (README, `docs/OPERATOR-GUIDE.md`,
  `CLAUDE.md`). They still claimed a default of 16384 — the stale assumption ADR 0015 removed in
  0.22.x. The code was right; three docs told the operator to reason from a number the flag has not
  used in months.

## [0.24.1] - 2026-07-26

### Fixed
- **A `GPU_LOCK` / `gpu_lock_path` pointing inside a home directory is now reported.** Found while
  bringing a fleet node current: it carried a **legacy `GPU_LOCK`** from before this package
  existed, aimed at `~/.local-offload/gpu.lock`. It was honoured silently, so the "machine-wide"
  lease was per-**USER** on a box that also runs a scheduled task — the exact split this package was
  written to end. Nothing reported it; `gpu status` printed a state root under the user profile and
  looked entirely plausible.
  - It **warns rather than refuses**, deliberately. An unwritable root and a cloud-synced root are
    always wrong and still refuse. A per-user path is merely *risky* and can be a deliberate choice
    on a single-user box — so the operator keeps the override and gains the one thing they lacked:
    knowing what it costs. Fires once per process, so a polling waiter cannot spam the log.
  - The detector is tested against a genuinely machine-wide path rather than `t.TempDir()` — on
    Windows that is `C:\Users\<u>\AppData\Local\Temp`, i.e. *inside* the home directory, which is
    the very per-user trap `gpu-lock.mjs`'s original `join(tmpdir(), ...)` default fell into.

## [0.24.0] - 2026-07-26

### Added — the agent loop can finally see and listen (Stage B)
- **`offload_vqa`, `offload_ocr`, `offload_transcribe`.** The loop could reach **4 of the harness's
  18 task types**: it could not see, listen or read a page even though the same binary does all
  three. These are the READ-ONLY senses — they consume a file the workspace already has and cost at
  most one model swap. The image/audio never enters the agent's context.
- **Media requests now route through `Pipeline.Run` instead of `RunTier`.** `RunTier` does
  `tasks.Build` + a **text** generate and never enters the 12-branch media dispatch — and
  `tasks.Build` SUCCEEDS for `vqa`/`ocr`/`assess_image`/`video_describe`. So a visual task sent that
  way did not error: it returned a confident answer about an image the model never saw. Handing a
  hallucination to an autonomous planner is worse than any error. `Run` fails closed at two gates
  (no vision model configured; image/audio load failed), both now covered by tests.
  - The routing default is **fail-safe**: only the four text tasks may take the single-tier path,
    so any task added later goes through the full dispatch rather than the text-hallucination path.
  - Text tasks deliberately stay on `RunTier` — it runs ONE named tier, so an agent offload call
    cannot trigger a cascade that swaps the planner's own model off a shared GPU mid-run.
  - `image`/`video`/`audio` params are lifted onto `core.Request`'s fields, which were never
    assigned. They ride `params` rather than a wider closure signature so `internal/agent` keeps its
    zero-import-of-pipeline invariant, and each key is declared in the tool's JSON Schema — a
    published contract, not a convention.
  - The recordless guarantee is unchanged: `Run` on a nil-cache/nil-ledger pipeline still writes no
    ledger, cache, shadow store or exemplars.

### Not included, deliberately
- **Generation tools** (`generate_image`/`video`/`audio`). On a single-GPU box a generation job
  holds a machine-wide lease entitled to unload llama-swap models — **including the planner running
  the loop**. An agent tool that can evict its own planner is a design error no timeout fixes; that
  needs an HTTP/OpenAI image seat (a third `imagegen_engine` value) rather than the spawn-per-job
  CLI path.

## [0.23.5] - 2026-07-26

### Fixed
- **The GPU lease's refusal on Linux now names the remedy.** Found in the field: a Linux services
  box upgraded past 0.23.0 and **every media job began deferring**. `/var/lib/local-offload` does not
  exist on Linux and an unprivileged service user cannot create it, and `setup/install.ps1` is
  Windows-only so nothing creates it. The refusal itself is correct — the lease is machine-wide on
  purpose and must not fall back to a per-user path — but `permission denied` alone left the
  operator unable to tell which of two very different fixes applied. The error now spells out both
  (`sudo mkdir -p … && sudo chmod 0777 …`, or set `state_dir`), including why the directory must
  **not** be sticky: reclaiming a dead holder's lease means removing another user's file.
  `docs/systems/gpu-lease.md` gains the one-time Linux setup step.

## [0.23.4] - 2026-07-26

### Added
- **Per-tool timeouts in the agent loop.** `Loop.dispatch` handed `t.Exec` the whole run context
  with **no deadline**, so a single tool could consume the entire run budget and the loop had no way
  to continue past it. Only `run_shell` self-capped. That is a hard prerequisite for wiring the
  harness's media routes into the agent: they default to **720 s image / 1500 s video / 1800 s STT
  against a 180 s `agent_run` budget**, so one call would have swallowed the run whole.
  - Default `120 s` (matching the existing `run_shell` cap), overridable per tool via `Tool.Timeout`
    so a genuinely long media route can be granted more without loosening the cap for everything,
    and per loop via `WithToolTimeout`.
  - An expired tool is a **reactable `is_error` result** the planner can route around, not a fatal
    run — the same contract as any other tool error.
  - **The cap is hard, not merely cooperative.** Cancelling a context preempts nothing, so a tool
    that ignores `ctx` would block `dispatch` forever and the deadline would be decoration. Proven:
    with a cooperative-only implementation the regression test does not fail, it *hangs*
    (`panic: test timed out after 20s`). The trade-off is stated in the code — an uncooperative
    tool's goroutine outlives the call and its result is discarded.
  - The run's own deadline is reported distinctly from a tool overrun, so a cancelled run never
    sends the planner chasing an innocent tool.

## [0.23.3] - 2026-07-26

### Fixed
- **The `edit` profile taught the planner a call that always hard-errors.** Its only search
  exemplar passed `{"query":...}` while `search_files` requires `{"pattern":...}`, so the decoder
  rejected it with *"search_files requires a pattern"*. Profile exemplars live in the
  **never-compacted** protected preamble, so the wrong call shape was taught for the entire run and
  could not age out. `prompt.go`'s tool line had the same wrong argument name, and also omitted
  `glob` and `mode` — the two cheapest narrowing levers, invisible to the model until now.
- **Profile exemplars are now narrowed with the tools, not copied wholesale.** `WithProfile`
  correctly restricted the tool set but assigned every exemplar regardless, so applying `build` to a
  loop without `run_shell` (the read-only MCP front door) demonstrated a tool the planner could not
  call — the same defect class as a wrong argument name. Exemplar cycles are dropped whole
  (assistant turn *and* its tool results), because a dangling `tool_calls` is rejected outright by
  strict `--jinja` templates.
- **A live data race under `--serve`.** `tokenCal.Observe` was called unconditionally and appends to
  a shared slice, while `--serve` shares one `*Loop` across concurrent HTTP handlers. It is now
  gated on `tokenCalOn` like the package's two other call sites — so an off-by-default feature has
  stopped mutating shared state from several goroutines at once.

### Added
- **`profile` on the MCP `agent_run` tool.** The front door could previously only produce bare
  `general` — the one configuration MEASURED to fail, because a 4B planner handed every tool calls
  none of them. Unknown names return a clean defer listing the valid ones rather than silently
  falling back to the configuration known not to work.
- **Two contract tests that turn this defect class into a build failure.** Every profile exemplar's
  arguments are validated against the named tool's real JSON Schema, every exemplar tool name must
  be registered *and* granted by its own profile, and applying a profile to a loop missing one of
  its tools must leave no impossible example and no orphan tool result. The suite previously checked
  exemplar *structure* and no *argument* anywhere.

## [0.23.2] - 2026-07-26

### Fixed — `search_files` never told the planner its pattern was case-sensitive
- **Measured failure (ampere-6, RTX 3050 6 GB, gemma-4-E4B and granite-4.0-h-tiny planners):** a
  documentation lookup failed on **every model/profile combination**. The planner searched
  `"rate limit"` against a file reading `"Rate limiting: …"`, got a bare `no matches for "rate limit"`,
  then retried with a **longer, more specific** query (`"gateway rate limit"`) and concluded the text
  was absent. `search_files` compiles with `regexp.Compile` (case-sensitive) and neither its
  description nor its schema ever mentioned the `(?i)` inline flag.
- **The tool spec now states the contract**: `pattern` is a CASE-SENSITIVE regex and `(?i)` makes it
  case-insensitive — in both the description and the `pattern` schema field.
- **The zero-match result now names the concrete retry at the point of failure**, which is where a
  small planner actually decides whether to give up: `no matches for "rate limit" (the pattern is a
  CASE-SENSITIVE regex). Before concluding it is absent, retry with "(?i)rate limit" …`. The spec
  competes with every other tool's spec; the failure message does not.
- **Already-case-insensitive patterns never get the suggestion re-offered.** Detection parses the
  pattern via `regexp/syntax` rather than substring-testing for `"(?i)"`, because a substring test is
  wrong in both directions: it misses `(?i:…)`, `(?is)…` and `a|(?i)b` (so it would suggest a retry
  that cannot change the result, burning one of the planner's `MaxSameTool` calls), and it fires on a
  literal `[(?i)]` character class that is genuinely case-sensitive.
- **Verified on the failing case, not just in unit tests:** the originally failing goal now passes on
  `offload-e4b/general`, `offload-e4b/build` and `granite-4-h-tiny/general` — 3 steps each, correct
  answer, source file cited. Previously 0/4 combinations succeeded.
- `(?i)` is honored identically by both grep backends (Go `regexp` walk and ripgrep, which receives
  `re.String()` verbatim); confirmed with `rg` present and absent from `PATH`.

## [0.23.1] - 2026-07-26

### Added — OmniRoute harvest Phase D: `compaction-eval kvbench` (KV reuse + real-token measurement, ADR 0017)
- **Server accounting is now visible to the harness**: `Completion.Serve` (`*agent.ServeStats`)
  carries llama.cpp's `timings.cache_n`/`prompt_n`/`prompt_ms` and `usage.prompt_tokens[_details]`
  when the backend reports them, and is **nil when it does not** — "unmeasured" is never readable as
  "measured zero". Additive only: the wire REQUEST is unchanged, pinned by a test.
- **`compaction-eval kvbench`** replays a corpus step-by-step through the production client and
  measures, per request: KV reuse fraction, real vs estimated tokens, prefill/wall time, which ladder
  rungs fired, and the byte-stream relation to the previous request (extension / truncation /
  divergent). Raw per-request rows are always emitted so every headline is re-derivable.
- **Fails closed.** Positive controls (byte-identical resend, ≥90% reuse) bracket the run and a
  negative control (unrelated prompt) plus a SEPARATION gate (pos−neg ≥ 0.40), because the real tool specs create a legitimate ~17% framing floor calibrates the metric; any failure ⇒ verdict
  `INCONCLUSIVE` + non-zero exit, because on this box an unrelated media-generation job evicts the
  tier and would otherwise turn a scheduler artifact into a false "compaction destroys the cache".
- **Arms run in blocks, never interleaved** (the tier serves `--parallel 1`: one KV slot, so
  interleaving would make every request a cache miss), and the **size effect and cache effect are
  reported separately and never summed**.
- **Safety-margin decision table** (`S ≥ (C − M)(1 − 1/r)`) computed from measured real/estimated
  token ratios per content kind — a table, not a recommendation, since raising the margin
  unconditionally shrinks usable context on every request. The probe sends the REAL read-only tool
  specs production sends, because `estimateTokens` counts tool specs as zero and omitting them
  would understate every implied margin by that constant.
- **Comparisons are PAIRED**: arms are compared only over steps that succeeded in BOTH (the
  no-compaction arm loses steps to overflow rejections). Per-arm totals remain as
  `*_unpaired_diagnostic` fields with a note; only `paired_totals` carries a delta.
- **Every failure is classified and counted**: `overflow` / `timeout` / `other`, with timeouts
  checked FIRST — a client timeout reads "context deadline exceeded" and must never be filed as
  the phase's own headline finding.
- **Eviction is detected from the data, no extra requests**: on a prefix EXTENSION the server must
  still hold what it held before, so a sample whose `cache_n` collapsed relative to the PREVIOUS
  prompt's real length is a scheduler artifact — flagged per row, excluded from rates, kept in
  token totals. (Comparing against the *new* prompt's reuse fraction instead would discard
  legitimate large appends; that error was caught in review and is regression-tested.)
- **`--budget-mode`**: `production` budgets the ladder exactly as `Loop.inputBudget()` does, so
  fire counts describe the corpus at that window; `pressure` (60% of each entry's estimate)
  guarantees the ramp is observable but makes the fire COUNT a property of the fixture. The mode
  is stamped in every report.
- `Completion.Serve` distinguishes `Measured` (any accounting) from `KVMeasured` (a `timings`
  block): a backend that reports only `usage` has no reuse to report, and its structural zero is
  excluded from every rate rather than averaged in as "no reuse".

### Added (OPT-IN, default OFF) — budget calibration against the server's REAL token counts
- **The estimator defect Phase D found, fixed.** The ladder decided whether to compact by comparing
  a flat `chars/4` estimate against `inputBudget()`. Measured real/estimated on the shipped bench
  (tool specs included, as production sends them) was p50 **2.15** / p95 **2.81**; net of the fixed
  spec payload the density component alone is **1.3–1.8** on the failing transcripts, against a
  shipped margin covering only 1.077 — and three real transcripts were
  rejected by the server with `exceed_context_size` while **the ladder declined to compact**, because
  by its own estimate they fit (`hv-json-ledger` estimated 6,224 vs 11,369 real; `hv-docs-readme`
  6,219 vs 8,749; `hv-deep-compeval` 5,693 vs 9,190). Compaction was gated on the wrong number.
- **`internal/agent/tokencal.go`**: an online two-term fit, `real ≈ intercept + slope·estimate`,
  learned from `usage.prompt_tokens` on every response (available since this release's client
  instrumentation) and applied to the BUDGET — not to `estimateTokens`, so every rung's internal
  comparisons stay in one space. The two terms are separated on purpose: the intercept absorbs the
  fixed per-request tool-spec payload (~528 tokens here, invisible to `estimateTokens`, which is why
  small requests show ratios up to 2.81), the slope absorbs content density. A single multiplicative
  factor fitted across sizes mis-corrects at both ends.
- **DEFAULT OFF (`WithTokenCalibration(true)` to enable), and that default is measured.** A live
  A/B over dense multi-file goals at shipped defaults: the fit was sound (`real ≈ 963 + 1.31·est`)
  but it cut the budget to 56%, retained **52% less tool content** (10,060 → 4,857 chars) and turned
  a correct answer ("275 lines") into a wrong one ("1 line") — while NEITHER arm hit a server
  rejection. At `--max-tokens 4096` the output reservation already absorbs the estimator error, so
  enabling it by default would spend real quality on a risk that does not materialise at those
  settings. Enable it where the output reservation is small relative to the window (the regime the
  defect was found in) or where overflow rejections are actually observed.
- **Uncalibrated behaviour is byte-identical to before**: fewer than two *distinct* observations ⇒
  the budget is returned unchanged. Slope/intercept are clamped to a credible range, the fit is a
  median over a sliding window (outlier-resistant, and mistakes age out), and the total correction
  is bounded at half the allowance.
- **`Result.TokenCal`** reports what the calibration learned (observations, slope, intercept, raw vs
  final budget), surfaced per goal by the standalone runner — a self-tuning mechanism that cannot be
  inspected is one nobody can debug.
- Chosen over the alternatives deliberately: a per-kind chars/token table is a guess (and a guess is
  what produced this defect), while a `/tokenize` round-trip would add a network dependency and a new
  failure mode to every step of the loop's critical path.
- **Scope of the evidence, stated plainly**: the defect is demonstrated on real harvested transcripts
  via replay plus a loop-level differential test (uncalibrated peak 11,540 real tokens — over the
  allowance — vs 7,728 calibrated), in the SMALL-output-reservation regime (`--max-out 1024`,
  budget 6,656). Re-measured at the shipped `--max-tokens 4096` (budget 3,584) the ladder fires on
  its own and compaction prevents one of the three overflows without calibration; the remaining two
  are ladder exhaustion (oversized newest turn), which no estimator fix addresses. That is why this
  ships opt-in rather than on.

### Changed — the harvest's KV rationale for compaction is corrected in the record
- Measured (ADR 0017): KV reuse on gemma-4-e4b is **binary** — appending a TURN reuses everything
  the server held (2,971 kept, only the 19 new tokens prefilled; corroborated by ~80 append steps
  per arm at a median 0.88), while ANY edit discards the ENTIRE cache; a position sweep showed a
  one-line edit at 98% of the prompt costing as much as one at 4%. Since every lossy rung edits from
  `protectedEnd` forward, a compaction fire always pays a full re-prefill. Compaction is justified by
  the size win and by requests completing at all — not by cache friendliness.

## [0.23.0] - 2026-07-26

### Added
- **Machine-wide fenced GPU lease (`internal/gpulease`) + `local-offload gpu` verb.** One
  exclusive card is shared by llama-swap and ComfyUI, but only the Node render runners ever
  took a lock — text work took nothing. A media job arriving through another ingress therefore
  called `freeLlamaSwap()` and unloaded every GPU-resident model out from under an in-flight
  benchmark; the server log holds **3,356 unloads, 330 of them the text workhorse**. The lease
  is taken by every consumer regardless of ingress: `gpu reserve --class text` lets a bench hold
  the card, and a render then WAITS instead of tearing the tier down.
  - **A busy card queues the job for a bounded window, then defers with the holder's detail.**
    The slot exists to serialize GPU work, not to cancel it — a render arriving mid-render should
    run afterwards, and dropping it would let a `gpu reserve --class text --for 45m` silently
    discard 45 minutes of media requests. The bound matters too: the caller is usually one tool
    call, so the old 20-minute video wait was indistinguishable from a hang. New `gpu_wait_ms`
    (**90 s**, matching `vision_gpu_wait_sec`) is the single ceiling for every GPU task. Only
    contention is waited out: an unwritable or cloud-synced lease location still returns
    immediately. A busy card at the `generate-image --batch` CLI is now a clean **defer**
    (exit 0, `err_class: gpu_busy`) rather than a hard failure.
  - Running the harness itself under `gpu reserve --class media -- local-offload …` **inherits**
    the ambient lease instead of acquiring a second one and queueing behind its own parent. An
    inherited lease that is no longer current refuses the job rather than quietly taking a fresh
    one, so a lost reservation is visible. Jobs sharing an inherited lease are serialized by an
    **in-process slot** — they have no file claim to contend on, so without it
    `gpu reserve --class media -- local-offload fleet-serve` (which runs the pipeline inline in a
    `net/http` handler goroutine) put two renders on the card at once: measured 256 ms of overlap
    on 250 ms jobs. A waiter there blocks on a channel rather than polling.
  - **A `text` reservation that outlasts your wait window is answered immediately** instead of
    after 90 s of pointless polling — `--for 45m` is a declared duration, so there is nothing to
    wait for. A `media` holder is not treated this way: its expiry is a timeout ceiling and a
    25-minute video budget routinely finishes in three.
  - **Nothing ticks at idle.** No daemon, scheduled task or watchdog is added; every timer is
    scoped to work in flight (a 15 s heartbeat while a lease is held, a 1 s probe while queued
    behind another process). A waiter no longer issues a fencing token per probe, which had it
    taking a machine-wide lock and doing four file operations a second for claims that could not
    succeed. `gpu reserve --detach` is the only continuous poller and exits on its own.
  - `local-offload gpu status|reserve|release`. The **wrapper** form
    (`gpu reserve ... -- <command>`) is preferred — the lease lives exactly as long as the
    command and cannot be leaked by forgetting to release. `--detach` spawns a hidden holder for
    interactive use and is the weaker form by design.
  - **Machine-wide**, not per-user: the old default was `join(tmpdir(), ...)`, which on Windows
    is per-user, so a process in another security context silently took a *different* lock and
    mutual exclusion evaporated with no error. An unwritable or **cloud-synced** root is now
    refused at startup instead of silently falling back — a sync client replicating a lock file
    between machines would hand one GPU to two hosts. The refusal is segment-aware, so
    `dropbox-exporter` is fine while `.../Dropbox/...` is not.
  - **Fenced.** A closing laptop lid is not a crash: the process resumes and would act on top of
    whoever holds the card now. Every acquisition bumps a monotonic epoch (kept outside the lease
    dir so reclaim cannot reset it) and `Check()` must precede every irreversible action —
    **unconditionally**, including for jobs that do not unload (batch jobs 2..N and `text`
    leases), because submitting a graph is irreversible GPU work too.
  - **Windows delete-pending semantics are handled on both sides of the claim.** Removing a file
    whose handle is still open marks it delete-PENDING, and an `O_EXCL` create in that window
    fails with `ACCESS_DENIED` rather than "already exists" — which read as a hard fault. The
    window is exactly the moment a holder releases, i.e. exactly when every waiter is polling,
    so the acquirer most likely to hit it was the one that should have won: measured, 1 in 48
    acquire/release cycles under six workers failed with *"could not claim the lease: Access is
    denied"*. Symmetrically, `os.Remove` on the claim fails while any reader has it open, and a
    failed release LEAKS the lease until both halves of the reclaim rule fire. Both retry.
  - **Epoch issuance is serialized, not merely written atomically.** tmp+rename makes each write
    indivisible and does nothing about two acquirers that both read *n* and both write *n+1* —
    measured, two concurrent acquirers were handed the same token. Since the token is threaded to
    children as `GPU_LEASE_EPOCH`, a duplicate lets a straggler pass `Check()` against a later
    lease and delete its claim on release.
  - **Pid-recycle safe** — the holder's process start time is recorded and compared, because
    pid-liveness alone reads a recycled pid as a live holder forever.
  - Reclaim is a deliberate conjunction: *(holder provably gone)* OR *(heartbeat stale AND the
    declared window expired)*. Each half alone is wrong — a bare heartbeat timeout expires a
    descheduled benchmark under exactly the load it exists to protect, and pid-liveness cannot
    see a `--detach` proxy outliving the run behind it.
  - `Release()` is epoch-guarded: a fenced-out straggler can never delete the *current* holder's
    lease. Leaking one is recoverable; silently handing the GPU to a third party is not.
- New `state_dir` config field (machine-wide state root; default `%ProgramData%\local-offload`
  on Windows, `/var/lib/local-offload` elsewhere).

### Removed
- **`videogen_wait_ms` and `audiogen_wait_ms` are retired**, replaced by the single
  `gpu_wait_ms`. They existed so a cheap queued TTS was not starved behind a 20-minute video;
  at a 90 s ceiling that distinction buys nothing. **This matters on upgrade:** the installer
  template shipped `videogen_wait_ms: 1200000` to every machine, so keeping them as per-task
  overrides would have silently restored the exact 20-minute wait the bounded queue replaces.
  A config still carrying them loads cleanly and prints a note naming the replacement — no
  action needed, and no "unknown key — typo?" warning for a key your own installer wrote.

### Changed
- **`freeLlamaSwap` is hoisted from per-JOB to per-LEASE.** It ran inside `withGpuSlot`, i.e.
  once per job — that is the arithmetic behind 3,356 unloads. Under an inherited lease
  (`GPU_LEASE_DIR` / `GPU_LEASE_EPOCH` / `GPU_LEASE_CLASS`) `withGpuSlot` skips the acquire (it
  would contend with its own holder) and elects **exactly one** job per lease to perform the
  unload, via an O_EXCL per-epoch marker inside the lease. N renders under one lease therefore
  cost ONE teardown. Note the difference from merely skipping: an earlier revision skipped the
  unload under an inherited lease while nothing performed it, so a leased render ran with every
  model still resident.
- **BREAKING for direct runner invocation: `render/*.mjs` no longer acquires the GPU.**
  `internal/gpulease` is the single implementation; the harness takes the lease and the runner
  inherits it. A GPU job started with no lease now refuses and names the fix:
  `local-offload gpu reserve --class media -- node render/comfy-generate.mjs ...`
  (`--no-lock` remains the escape hatch). This deleted `acquireGpuLock`, `isStale`, `bumpEpoch`,
  `machineStateRoot`, `ensureStateRoot` and `defaultLockPath` — roughly a third of that file.
  Two implementations of one concurrency rule produced a new divergence in every review round;
  the class is gone by construction rather than by patch. See ADR 0018 §7.
- **The default lock path moved** from `<os-tmpdir>/local-offload-gpu.lock` to
  `<state_dir>/gpu/lease`, and `gpu_lock_path`/`GPU_LOCK` are now honoured by every consumer
  through one resolver. Previously some honoured them and some did not, so setting that field put
  the reservation verb and the render path on different directories where they never contended.
- **Unloading now drains first.** Measured on llama-swap v242: an unload issued during a
  generation returned in **1,265 ms without draining** and the in-flight request died at
  **4,107 ms with `502 Bad Gateway`**. The unload route does not honour in-flight work, so the
  caller must. `quiesceLlamaSwap` polls llama-server's `/slots` via `/upstream/<id>/slots`
  (`is_processing` verified true throughout a 23 s / 1500-token generation). It is fail-safe,
  not fail-open-silent: an unreadable `/slots` reports `drained:false` and names the tiers it
  could not observe, so the caller logs that it proceeded without a verified drain rather than
  pretending. A stuck tier times out instead of deadlocking the render queue.
- `render/gpu-lock.mjs` now shares the lease **schema** with `internal/gpulease`, not just the
  path. Had it kept its flat `{pid,startedAt}` shape, the Go reader would find no holder pid,
  treat the lease as ownerless and reclaim one actively held.
- **`GPU_LOCK_WAIT_MS` is gone.** Queueing moved to Go alongside the acquisition, so the harness
  can refuse a job before spawning a process that could only have waited and timed out. No render
  runner ever read the variable.
- **Every GPU task takes the lease at its Go call site** — image (ComfyUI and sdcpp), inpaint,
  image batch, run-graph, video and audio (voice and music). Coverage is asserted by a table over
  the GPU tasks plus a guard that fails when a new `withGpuSlot` runner appears with no case,
  because wiring call sites by hand is how two of them were missed.

## [0.22.26] - 2026-07-24

### Added — OmniRoute harvest Phase C: dedupe rung, re-request pinning, FORCE_PRESERVE, fit telemetry (ADR 0016)
- **Content-addressed dedupe rung** (always on, the ladder's cheapest, before GCF): an OLDER tool
  body byte-identical to a LATER result collapses to a reference marker naming the later call id —
  the newest copy stays authoritative, pairing intact, information reachable by reference. Bodies
  under 64 chars are skipped.
- **H8 re-request pinning**: when the circuit breaker refuses an exact-repeat call, the ORIGINAL
  result's call id is pinned for the rest of the run — exempt from dedupe/skeleton/elide, its unit
  from drop (lossless GCF still applies; `emergencyShrink` stays pin-blind). Pins resolve
  transitively through dedupe references. If the original result was ALREADY destroyed by
  compaction, the call is re-executed once per pair and the fresh result pinned — a refusal
  pointing at destroyed bytes had no recovery path. Content the model re-reads stops being
  lossily compacted.
- **FORCE_PRESERVE guards**: the elision rung keeps a bounded residue (≤5 lines/≤400 chars) of a
  body's signal lines (errors/failures/warnings/test summaries) under the marker; the drop rung
  refuses to drop units still carrying signal residue or pinned results. The mini-corpus eval pin
  evolved accordingly: the buried error entity now survives BOTH ladders (pinned both directions).
- **fit=false telemetry**: `Result.CompactionsExhausted` counts steps whose ladder could not fit
  the budget; surfaced by the standalone runner (stderr) and `agent_run` (result JSON) — a
  best-effort over-budget request is never silent.
- **Monotonicity invariant, test-pinned**: compaction is idempotent at a fixed budget and never
  regresses a compacted turn at a gentler one (KV-prefix stability doctrine made explicit).
- Deliberate deviation, documented in ADR 0016: the harvested projected-fit driver is NOT ported —
  rungs here are microsecond-cheap and applied-and-measured, strictly more accurate than static
  projection factors; the exhaustion telemetry delivers the factors' remaining value.
- **Baseline note**: the base ladder's bytes change for signal-bearing bodies, so pre-Phase-C
  compaction-eval ratchet baselines correctly BREACH — re-freeze after adopting.

## [0.22.25] - 2026-07-24

### Changed — compaction defaults ON + the budget targets the SERVED window (ADR 0015)
- **`--skeleton-prune` and `--gcf-compact` default ON** (CLI, and now wired on the MCP `agent_run`
  path, which previously enabled neither); pipeline `gcf_compact` defaults ON (explicit `false`
  still wins). Flipped on MEASUREMENT per the Phase-B gate: real-corpus replay (retention never
  worse, better where the ladders differ) + control-pair-gated live A/Bs (base +0.119
  CI[+0.047,+0.194], skeleton +0.090 CI[+0.029,+0.165] — no outcome cost at production pressure).
  Flip decision approved 2026-07-24 from `COMPACTION-FLIP-DECISION-2026-07-24`.
- **`--ctx-tokens` defaults to 0 = AUTO**: probe the serving endpoint's live `n_ctx`
  (`/upstream/{model}/props` for llama-swap, `/props` for bare llama-server; conservative 8192
  fallback), replacing the stale 16384 assumption that killed two real runs with
  `exceed_context_size` 400s before the budget engaged. An explicit flag overrides the probe and
  warns when it exceeds the served window.

### Fixed
- **Huge-newest-tool-body overflow no longer kills the run**: when the server rejects for
  overflow and the harder-compaction retry cannot help (the oversized body sits inside
  keep-recent, where every ladder rung is forbidden — the retry used to re-send the same bytes),
  `emergencyShrink` reduces tool bodies as a last resort: skeleton first, then elision markers,
  oldest-first, finally trimming the one body that still overflows. The preamble is never touched
  and no turn is dropped; pinned by a loop-level test reproducing the live failure shape.

## [0.22.24] - 2026-07-24

### Added — `compaction-eval harvest`: real replay corpora from agent traces, redaction-at-harvest
- **`compaction-eval harvest --traces DIR --out corpus.jsonl`** builds a REAL replay corpus from the
  standalone agent's trace files (`cmd/local-agent` per-goal JSON, full transcript): converts the
  `[]agent.Msg` transcript to the strict corpus wire format, classifies each entry's kind by
  byte-weighted majority (≥60%) of its TOOL payloads (no majority → `mixed`; no tool payloads →
  `prose`), and mirrors PRODUCTION replay pressure: `protected_prefix` = the real preamble (turns
  before the first assistant turn) and `keep_recent` = the live loop's exported
  `agent.DefaultKeepRecent` (review finding: leaving it at the harness default of 1 replayed every
  entry under systematically harsher pressure than production). Transcripts the ladder already
  compacted mid-run (elision markers / skeletons in tool bodies, detected via the new
  `agent.IsCompactionArtifact`) are REFUSED with a note and counted (`pre_compacted`) — replaying
  compaction-of-compacted text would measure the ladder against its own output.
- **Redaction-at-harvest**: deterministic placeholder substitution over the exact VetPII refusal
  classes BEFORE the corpus file exists (git output alone carries author emails, which the vet
  refuses). Same matched text → same numbered placeholder, so distinctness survives for entity
  retention and re-harvesting is byte-identical. The private-key class redacts the WHOLE block —
  including truncated blocks (BEGIN without END) through end-of-string — because the vet only
  recognizes the header and a header-only redaction would leave key material behind.
- **Defense-in-depth gate**: after redaction the VetPII gate re-runs on the built entries; any
  residual finding refuses the whole harvest (redactor/vet drift must fail loudly, never write a
  file the loader would refuse). The redaction table is DERIVED from the vet's own class table
  (parity by construction — a future vet class is automatically redacted with its own regex
  unless an override widens it, as private-key-block does). The written corpus is written
  atomically (temp + rename) and round-trip-proven through the strict loader (`Load`) BEFORE it
  exists at the destination; malformed traces (e.g. a tool turn without `tool_call_id`) degrade
  to per-file skip notes at harvest time instead of aborting the run at write time; skipped trace
  files (corrupt, too short, pre-compacted, capped) are each named with a reason — silent drops
  would read as coverage.

## [0.22.23] - 2026-07-24

### Added — OmniRoute harvest Phase B: the compaction eval harness + ratchet (`internal/compeval`)
- **`compaction-eval` verb** (run | freeze | check | ab) over a pinned transcript corpus: JSONL of
  transcript slices, sha256 corpus hash stamped into every artifact, deterministic PII vetting
  that REFUSES a corpus (never scrubs in place — scrubbing would change the pinned bytes).
- **Replays the PRODUCTION ladder** — a new exported seam (`agent.CompactReplay` /
  `agent.EstimateTokens`) means measured numbers are the shipping behavior, never a
  reimplementation. Per content kind (tool-json/tool-text/logs/code/prose/mixed): compression
  ratio, budget-fit share, and **entity retention with explicit lost-entity lists** (numbers,
  paths, URLs, key=value, UPPER_SNAKE, hex — the FORCE_PRESERVE classes). Test-pinned property:
  on a logs corpus the skeleton rung retains the buried error entities the marker-only ladder
  destroys.
- **Tokens ratchet**: `freeze` pins per-entry compacted tokens; `check` fails loudly beyond ±2%
  and REFUSES cross-corpus/cross-ladder/missing-entry comparisons (a ratchet against a different
  artifact is not a ratchet). Verified live: freeze → hold → ladder-mismatch refusal (exit 1).
- **Gated task-outcome A/B**: full-vs-compacted scored through the live pipeline (summarize +
  entity-recall outcome scorer — two dead ends measured live and documented in-code: pure
  self-grounding passes entity-free garbage, and grounding-as-gate inverts on benign numeric
  paraphrase), behind the **control-pair self-test gate** — the scorer must rank a
  known-good/known-degraded pair first or the A/B ABORTS (no confident numbers from a blind
  judge; the gate fired twice for real during bring-up). Paired bootstrap delta via the existing
  eval machinery; partial runs abort. First live run on the mini corpus: delta −0.251
  (CI −0.473..−0.033) at the aggressive 60% replay budget — the harness's own first verdict is
  that default flips need the real-corpus run, not optimism.
- Committed deterministic mini-corpus at `testdata/compeval/` (6 kinds); real replay corpora are
  machine-local. Trajectory-trace harvesting into corpus entries is a recorded follow-up.
  This harness is what decides the pending default flips (`--skeleton-prune`, `gcf_compact`) —
  savings claims exist only as measured mean ratios stamped with the corpus hash.

## [0.22.22] - 2026-07-24

### Added — J4: 8GB-tier first-class sub-item + per-box device seams (Juan-tier Q0, phase 4 of 5)
- **RAM-conditional 8GB media seed**: `ampere-8`/`blackwell-8` carry `config_seed_ram_mid_high` —
  install.ps1 merges it on top of the base seed ONLY when `ram_tier` is mid/high (the same RAM
  gate as the 26B cpu-moe path): the VERIFIED quality-first HiDream-O1 bf16 IMAGE seat (5.9
  min/render on the 8GB 3070 + 64GB reference box) that previously needed manual binding.
  Image only — no video/music seat on 8GB by the 2026-07-23 decision. Low-RAM boxes unchanged;
  existing configs never touched.
- **Per-box device/launch seams** (all default-preserving, audit seams 4): `COMFY_COMPUTE_DEVICE`
  env overrides the Wan graph's DisTorch2 `compute_device` (kills the hardcoded `cuda:0`);
  `COMFY_EXTRA_ARGS` appends verbatim flags to the managed ComfyUI launch; `TTS_DEVICE` overrides
  the Chatterbox worker's torch device auto-pick (the ft skeleton gains it when its engine lands).
  Node tests pin the two COMFY seams (env set → applied; unset → byte-identical); TTS_DEVICE
  rides the worker's plain env read (no torch in the test environment to pin it against).

## [0.22.21] - 2026-07-24

### Added — J3: fleet-node citizenship for non-NVIDIA GPUs (Juan-tier Q0, phase 3 of 5)
- **GPU memory provider seam** (ADR 0014): `fleet-serve` resolves its memory source instead of
  assuming `nvidia-smi` — nvidia-smi first (existing NVIDIA nodes byte-identical), else the
  **windows-generic WDDM provider**: capacity from the display-class registry
  (`HardwareInformation.qwMemorySize`, the same source detect.ps1 uses), usage from the
  vendor-agnostic `\GPU Adapter Memory(*)` PDH counters. The startup gate refuses only on
  "no working GPU memory source" — never on GPU brand — and the serve log names the resolved
  source. `gpu_vendor`/`gpu_arch` now come from `installed.json`'s profile (VendorArchFromProfile)
  instead of NVIDIA product-name regexes; unknown stays "unknown", never guessed.
- **UMA memory model**: an iGPU's carve-out is not its capacity — the generic provider advertises
  carve-out + the WDDM shared budget (~RAM/2) as `vram_total_gb` and Dedicated+Shared as usage
  (profile-keyed via UMAFromProfile; small-carve-out heuristic only when no manifest exists).
- **`fleet_sampler: "pdh-shared"`** — the per-process PDH tree summing Dedicated **+ Shared**
  Usage: on unified memory allocations land in Shared and the Dedicated counter reads ~0, so
  footprints on the amd-rdna3 tier would silently never record (the audit's "invisible" break).
  The `amd-rdna3` config_seed sets it (dgpu seeds plain `pdh`); `auto` is unchanged.
- Sampler generalized to `StartProbeSampler(MemProbe)` (`StartGlobalSampler` kept as the
  nvidia-smi wrapper); new env-gated live receipt probe `TestLiveWindowsProbes`
  (FLEET_LIVE_SMOKE=1) — verified on real hardware this session (registry 15.93 GiB on a 5060 Ti,
  adapter counters live, UMA composition 79.77 GiB on 128 GB RAM). Docs: FLEET-NODE.md +
  systems/fleet-node.md updated; ADR 0008 cross-referenced.

## [0.22.20] - 2026-07-24

### Added — J2: the sd.cpp media tier (Juan-tier Q0, phase 2 of 5)
- **Second image engine.** `imagegen_engine:"sdcpp"` routes `generate_image` to
  **stable-diffusion.cpp** via the new `render/sdcpp-generate.mjs` — a single native Vulkan
  binary, spawn-per-job under the shared GPU lock, zero-warm by construction, no ComfyUI/Python
  anywhere on the path (`gpugen.Spec` with `SkipFreeComfy:true`, the shape the TTS path proved).
  The ComfyUI default path stays byte-for-byte unchanged. New config surface: `sdcpp_bin`,
  `sdcpp_script`, `sdcpp_model` (+`_kind`), companions `sdcpp_vae`/`sdcpp_clip_l`/`sdcpp_clip_g`/
  `sdcpp_t5xxl`/`sdcpp_llm` (Z-Image's Qwen3 encoder), and `sdcpp_extra_args` (canary-decided
  stability toggles). The runner owns the mapping to sd.cpp's real CLI (verified against the
  pinned release: `--diffusion-model` vs `-m`, `--clip_l`/`--clip_g` underscores, `-p/-n/-W/-H`,
  `-s`, `--cfg-scale`, `--sampling-method`) — flag drift on a pin bump is fixed in one .mjs.
- **Installer media leg (Step 5b)** — pins **sd.cpp `master-789-5114672` win-vulkan**
  (GitHub-digest SHA; ships `sd-cli.exe` + `sd-server.exe`, the recorded warm-swap upgrade path)
  and the **Apache-2.0 lead roster (~10.1 GB)**: Z-Image-Turbo Q8_0 (**jayn7 build** — leejet's
  own low-bit quants render blank on Vulkan+AMD, sd.cpp #1031), Qwen3-4B-Instruct Q4_K_M
  (`--llm`), and the ungated Comfy-Org Flux AE. Default-on for `amd-*` profiles;
  `OFFLOAD_WITH_MEDIA=1|0` forces anywhere; `OFFLOAD_MEDIA_EXTRAS=1` adds SD1.5 + SDXL base +
  the fp16-fix VAE (all-black-on-AMD-iGPU guard, sd.cpp #563). Excluded on license grounds:
  SDXL-Turbo (non-commercial) and the whole FLUX family (ADR 0011 — Apache schnell included).
- **`config_seed` for `amd-rdna3`/`amd-rdna3-dgpu`** — engine, model paths (via the new
  `__OFFLOAD_HOME__` token `Merge-ConfigSeed` now expands, arrays included), turbo sampling
  (steps 8, cfg 1), and the VAE stability workaround (`--vae-on-cpu` iGPU / `--vae-tiling` dgpu).
- **First media selftest leg** (`receipt.media`): a fixed-prompt/seed **reference render**
  gated on a non-blank check (sampled distinct-colors ≥ 8, >20 KB, timed against the ZLUDA
  anchors) plus the **gpu_vae promotion trial** (drop the CPU-VAE workaround only on a measured
  clean-and-faster run) — the media mirror of the H3 canary pattern, with the same
  `does_not_prove` honesty when skipped.
- **Runbook**: SETUP-AGENT.md AMD chapter gains the Media tier section (roster + licenses,
  receipt gates, timing calibration, allowed autonomous decisions, media forbidden list:
  `--vae-conv-direct`, SD2-class, fp8, quant substitution).

## [0.22.19] - 2026-07-24

### Added — J1: the AMD RDNA3 text tier goes first-class (Juan-tier Q0, phase 1 of 5)
- **H3 canary suite in `selftest.ps1`** (new `receipt.canaries` block; runs by default on a
  Vulkan backend, `OFFLOAD_SELFTEST_CANARIES=1|0` forces on/off; never verdict-changing —
  each PASS *authorizes* one config promotion the installing agent applies per the runbook):
  `fa_q8kv` (fixed-prompt temp-0 f16-vs-q8_0 word overlap ≥0.80 **plus** server-log proof FA is
  actually ON — probe servers now launch `-lv 10` because default verbosity omits the
  `flash_attn` state line, verified live on a Jul-2026 build), `moe_full_offload` (the 26B
  `-ngl 99` trial — the upward mirror of the standing `--cpu-moe` downshift; promotes on a
  measured full-offload decode number that beats the cpu-moe baseline where one was measured,
  and on the measured number alone where the baseline probe was skipped), `ctx_sweep` (8/16/32K load+gen; 32K pass authorizes the ctx
  promotion), `bench` (llama-bench pp512/tg128 regression-gate numbers), `swap_leak` (eviction
  cycle leaves ≤1 llama-server), `embedder` (cosine ordering through the real endpoint),
  `whisper` (honest skip carrying the whisper.cpp ≥1.8.3 AMD-iGPU floor). Skipped canaries land
  in `does_not_prove` — no promotion without a measurement.
- **`SETUP-AGENT.md` AMD RDNA3 chapter** — a complete install→test→validate→report runbook
  written for the agent driving the box: expected perf envelope (bandwidth-bound tg, pp as the
  UX pain, small dedicated-VRAM is normal UMA), per-canary proves/does-not-prove tables, the
  exact autonomous decisions allowed (f16→q8_0 KV, ctx 16K→32K, 26B cpu-moe→full-offload,
  Vulkan device index — each gated on its named canary), the hard forbidden list (driver
  updates, ROCm/HIP/WSL2, pin substitution, vision-seat binding), the 5 hardware questions, and
  the receipt (installed.json + final selftest JSON) that promotes every PROJECTED number to
  MEASURED upstream.
- **`amd-rdna3-dgpu` profile + AMD VRAM banding in `detect.ps1`** — a discrete RDNA3 card
  (≥12 GB dedicated) no longer falls to the iGPU floor: it renders 32K/q8_0/26B `-ngl 99`
  resident (PROJECTED; 12 GB cards fall back via the standing OOM→cpu-moe remediation).
- **Adrenalin version is now READ, not guessed** — `detect.ps1` reads `RadeonSoftwareVersion`
  from the registry, classifies it against the deep-context Vulkan crash class (≤ 25.11.1,
  llama.cpp #17432), emits `amd_adrenalin` in the JSON, and only warns when it actually matches
  (or is unreadable). Pure classifier + 6 new self-test assertions.
- **Vulkan serving hardening** — `GGML_VK_VISIBLE_DEVICES=0` on every model entry of the Vulkan
  template (deterministic adapter on multi-ICD boxes) and frontier-floor comments (llama.cpp
  win-vulkan ≥ Mar-2026: the AMD scalar-FA Wave32 + graphics-queue tuning window; FA explicitly
  on/off, never auto).
- **Frontier surfacing in `install.ps1`** (`Show-FrontierNote`, standing principle 2026-07-24):
  best-effort GitHub check that prints a NOTE when the llama.cpp/llama-swap pin is behind the
  latest release — pins are snapshots of frontier refreshed via the canary suite, never silently
  stale, never substituted mid-install. `OFFLOAD_SKIP_UPDATE_CHECK=1` disables; offline is a
  silent skip, never fatal.
- **`amd-rdna3` profile notes rewritten from the 2026-07-23 research** — the "cpu-moe = very
  slow" claim was wrong for dual-channel DDR5 boxes (26B-A4B full-offload measures ~20–25 t/s tg
  on a 780M, faster than dense 7B); the floor stays conservative and the canaries earn the
  promotions. New PS test suite `setup/tests/selftest-canaries.test.ps1` (23 assertions incl.
  live-captured llama-server FA log lines).

### Changed
- `docs/systems/setup-installer.md` + `docs/OPERATOR-GUIDE.md` profile tables: fourteen
  profiles, AMD banding, canary-floor annotations.

## [0.22.18] - 2026-07-23

### Added — GCF lossless columnar compaction (`internal/gcf`) — OmniRoute harvest Phase A
- New codec: eligible JSON arrays (≥8 flat objects, scalar values only) re-encode columnar —
  keys stated once in a `[N]{fields}` header, pipe-joined rows, `~`=missing/`-`=null sentinels,
  ambiguous strings JSON-quoted. **Losslessness is the contract**: the decoder ships as the
  round-trip ORACLE (production never decodes; the model reads the compact form) and every
  encoder path must deep-equal back through it in tests and fuzz — three defects were found and
  fixed BEFORE first ship (fuzz: Sscanf header fragility on field names with spaces; review,
  reproduced: all-empty-objects arrays decoding a phantom field; by test: trailing-content
  acceptance that would have eaten text after the array), crashers kept in the fuzz corpus.
  Anything outside the contract fails closed to the original text: under min rows, non-object
  elements, nested values, duplicate keys, unsafe field names, or no strict size win.
  ≥30% savings test-asserted on typical tool-output arrays. Format attribution (gcf-typescript
  via OmniRoute, both MIT) added to NOTICE; independent reimplementation, no code copied.
- **Agent ladder**: new LOSSLESS rung 2a via `--gcf-compact` (default off) — over budget, older
  JSON-array tool bodies re-encode before the skeleton/marker/drop rungs run. `compact()` now
  takes a `compactOpts` struct; zero value = the original pinned ladder.
- **Pipeline context assembly**: new `gcf_compact` config field (default off) — an over-budget
  input's eligible JSON is compacted losslessly BEFORE the head/tail context trim, converting
  would-be truncations into full-fidelity completions. In-budget inputs are never touched
  (happy-path bytes stable, pinned by test).

## [0.22.17] - 2026-07-23

### Added — skeleton rung in the agent's compaction ladder (`--skeleton-prune`, default off)
- The transcript compactor jumped straight from "full tool body" to "bare size marker", so the
  first budget crossing in a long agent task destroyed ALL information in every older tool
  result at once. A new flag-gated rung sits between the two: older verbose bodies are reduced
  to deterministic **skeletons** — the head/tail windows plus buried
  error/failure/warning/test-summary lines, elided runs replaced by counted
  `[... n lines elided ...]` markers, opened by a disclosure prefix that also makes the pass
  idempotent. Bare markers and whole-turn drops remain the fall-through when skeletons alone
  cannot reach the budget (a fallen-through marker reports the ORIGINAL body size, parsed from
  the skeleton prefix); with the flag off the older rungs run untouched (pinned by test on the
  off-path's shape). Surface: the `local-agent` CLI (`--skeleton-prune`), all drive modes incl.
  two-tier. The MCP `agent_run` path has no per-call knob yet — it gains the rung only if the
  default flips after broader measurement.
- Deliberately model-free: the local cascade costs ~4 s warm / ~11 s cold per ~2k-token call
  (measured 2026-07-23 on the 16 GB box), which would serialize every over-budget step; the
  rules pass costs microseconds, is unit-testable, and produces identical bytes on every
  re-compaction (KV-prefix friendly). A cascade-refined or lossless-structural skeletonizer can
  slot into the same seam later if measurement earns it (the queued OmniRoute study).
- Origin: the swe-pruner-pro *policy* (prune consumed tool outputs into skeletons) adopted
  WITHOUT its mechanism (SGLang hidden-state serving + per-backbone trained heads — verified
  against its README 2026-07-23); advisory proposal reconciled against this repo's existing
  compaction design rather than the proposal's insertion-time sketch.

## [0.22.16] - 2026-07-22

### Fixed — TTS venv setup docs generalized beyond the original machine
- `render/README.md`'s `.tts-venv` setup note documented one machine's path (ComfyUI's
  Python 3.12, `torch==2.6.0` cu124) — a recipe that fails outright on Blackwell GPUs
  (RTX 50xx, sm_120: cu124 ships no sm_120 kernels; torch ≥2.7.0 cu128 required) and
  omitted a universal trap: `resemble-perth` (chatterbox's watermarker dep) imports
  `pkg_resources`, removed in setuptools 81+ and absent from uv venvs, so
  `perth.PerthImplicitWatermarker` silently resolves to `None` and model init crashes with
  `TypeError: 'NoneType' object is not callable`. The note now covers both GPU generations,
  the `uv pip` install path (uv venvs ship no pip), and the setuptools<81 requirement, with a
  second VERIFIED line from the Blackwell machine (<node-b>, RTX 5060 Ti: real 4.0 s 24 kHz WAV
  end-to-end through `generate-audio`). Stale one-machine comment in `render/tts.mjs` retired
  with it.

## [0.22.15] - 2026-07-22

### Added — the HQ STT tier can be an llama-server mtmd model (`stt_hq_api: "openai"`)
- The HQ transcribe path spoke only whisper-server's `/inference` multipart protocol, so binding
  an llama-server-served STT model (Qwen3-ASR, mtmd) to `stt_model_hq` deferred with a
  whisper-endpoint 404 (live finding 2026-07-21, binding rolled back). The client now also speaks
  the OpenAI `/v1/audio/transcriptions` shape through the same `/upstream/<model>/` passthrough —
  verified live on <node-b> before building (HTTP 200 on `qwen3-asr` with a correct transcription).
- New config field `stt_hq_api`: `""`/`"whisper"` = the existing whisper protocol; `"openai"` =
  the transcriptions endpoint. Selection only affects the `hq=true` path; the default STT tier is
  untouched.
- Adapter honesty: mtmd ASR models emit no timestamps, so the result carries ONE synthesized
  full-span segment (duration derived from the 16 kHz WAV size); Qwen3-ASR's
  `language X<asr_text>` prefix is parsed into the result's language field (free detection);
  whisper decode knobs (VAD/beam/language forcing) do not apply on this path. The single-slot
  serialization mutex covers both protocols.
- Matrix updated: the v0.22.12 "cannot bind qwen3-asr yet" non-binding is now a HOW-TO (set both
  `stt_model_hq` and `stt_hq_api`); the MCP transcribe tool's `hq` description no longer
  hardcodes "large-v3".

## [0.22.14] - 2026-07-22

### Changed — running on built-in defaults is now LOUD (`config:` disclosure in doctor)
- When the effective config is built-in defaults, every config-loading command prints one stderr
  warning naming the state and the escape hatches. All THREE defaults shapes warn (the last two
  were review findings on the first draft of this change): nothing resolves anywhere; an explicit
  `--config`/`$LOCAL_OFFLOAD_CONFIG` path that does NOT exist (`Load` maps IsNotExist to defaults
  with a nil error by design — previously totally silent); and a file that exists but fails to
  parse. Silent fallback was the trap (live incident 2026-07-20): a box whose real config lived at
  a non-conventional path served every bare CLI call from built-in defaults — empty `vision_model`
  and all — and the only symptom was a misleading "no vision model configured" defer inside a
  consumer's pipeline.
- `doctor` prints a `config:` first line that is TRUTHFUL: it names the file only when that file
  was actually read; not-found and failed-to-load both disclose `BUILT-IN DEFAULTS` with the
  reason (crediting an unread file would be worse than no disclosure).
- Resolution + disclosure moved to `internal/config` (`ResolvePath`/`LoadWithSource`/
  `WarnOnDefaults`/`SourceLine`) and **`local-agent` now shares it** — review found that binary
  silently ran on defaults AND never discovered the conventional `~/.local-offload/config.json`
  at all.

## [0.22.13] - 2026-07-22

### Fixed — a spawn failure is not a venv problem (`SATISFIER_SPAWN_FAILED`)
- A satisfier subprocess (git / uv / pip) that failed to **start** — Node spawn errors like
  `spawn UNKNOWN` / `ENOENT` — was caught and relabeled `VENV_INCOHERENT`, in the worst case as
  "pip check reported conflicting installed dependencies: spawn UNKNOWN": a wrong diagnosis
  pointing the operator at healthy torch pins (live creative-marketing-pipelines report,
  2026-07-20, transient after a long render batch). Spawn failures are now classified apart
  (`isSpawnFailure`), **retried once** after 500 ms (the observed failure was transient), and then
  defer with the new typed code **`SATISFIER_SPAWN_FAILED`** naming the stage (`pack name` /
  `uv-resolve` / `pip-check`). `VENV_INCOHERENT` now always means the tool RAN and found a real
  problem.

### Changed — proven-satisfied re-runs skip the resolve; one deliberate fail-open
- The common re-run stops paying the expensive `uv` resolve no-op — but the skip is authorized by
  a **persisted marker** (`<venv>/.offload-deps-satisfied` — it attests venv state so it lives
  inside the venv and a recreated venv takes it down; the sorted `name@commit` pin-set key), written ONLY after a fully successful resolve+check. "Git didn't move this run" alone is
  NOT proof (adversarial-review finding: a run that checks packs out and fails before installing
  would otherwise skip the install forever and report ok on an unprovisioned env). A stale or
  missing marker, or any moved checkout, still runs the full resolve. The cheap `pip check`
  coherence gate always runs.
- Fail-open, narrowly: only a marker-proven, unchanged pin-set survives a coherence check whose
  subprocess fails to spawn (after the retry) — satisfy then succeeds with a `warning` (stderr
  `SATISFY WARN`). Anything unproven fails closed with `SATISFIER_SPAWN_FAILED`.
- The git checkout stage is deliberately NOT retried: a whole-step retry recomputes HEAD-before
  against its own surviving side effects (fresh clone / moved checkout) and misreports
  `changed=false` (adversarial-review repro). A git spawn failure defers typed; retrying the whole
  satisfy is the caller's idempotent recovery. The lazily-captured host-pins snapshot also no
  longer memoizes a REJECTED promise, so the 500 ms retry genuinely re-runs `pip freeze`.
- Docs de-staled in the same change: the flow doc and ADR 0007 still described the pre-v0.22.5
  "drift diagnostic only on stderr" gap as open; both now record the staged fixes. The v0.22.12
  changelog's "old shape / tier-agnostic" phrasing is also clarified below (the shipped default
  binds `gemma4-26b-a4b` to both escalation and reasoning and is unchanged; the 4-rung ladder is
  a ≥16GB matrix recommendation on top of it).

## [0.22.12] - 2026-07-21

### Changed — model matrix: validated 4-rung cascade ladder for ≥16GB tiers
- `setup/SETUP-AGENT.md` gains a **Text-cascade matrix** section recording the roster validation
  run on `ampere-16` (operator benchmark archive `2026-07-20_<node-b>-roster-validation/`): the
  recommended ≥16GB binding is the 4-rung ladder — e2b triage (154.5 tok/s) → e4b workhorse
  (95.7) → **a 12B-MTP escalation rung (`offload-12b`, 82.1 — 2.5× the 26B it offloads from,
  grammar-clean, task-level A/B showed zero regressions)** → 26b terminal reasoning (32.5). The
  old shape bound escalation AND reasoning to the 26B, wasting a slot. 8GB tiers keep the 3-rung
  ladder; code defaults stay tier-agnostic (this is a matrix recommendation, not a default change).
- Two validated **non-bindings** recorded alongside: `gpt-oss-20b` must never take a
  grammar-constrained slot (harmony format vs GBNF = hard 500; reasoning phase eats small budgets
  → empty content; it stays the free-text/throughput model, 4-slot admission proven with aggregate
  throughput halving under 4-way load), and `stt_model_hq` cannot bind `qwen3-asr` yet (the HQ
  transcribe client speaks whisper-server HTTP; Qwen3-ASR is served by llama-server mtmd — binding
  defers with a whisper-endpoint 404, verified live and rolled back; needs an llama-server audio
  path for the HQ tier first).
- `docs/systems/offload-pipeline.md` and `docs/flows/cascade-escalation-and-defer.md` are
  cross-referenced with the matrix and their "reasoning runs on the same model as escalation"
  statements conditioned on the binding (they were flatly false under the new recommendation).

## [0.22.11] - 2026-07-20

### Fixed — model presence spans ComfyUI's full search path (`extra_model_paths.yaml`)
- The satisfier resolved every model as `join(comfyDir, model.path)` and knew nothing about
  `extra_model_paths.yaml`, so on any machine that keeps its model tree off the OS drive — e.g. a
  `V:` Optane tree, the documented <node-b> layout — a model that ComfyUI loads fine read as **MISSING**
  and was re-downloaded to `C:` on every run (tens of GB for a big GGUF). Presence now checks the
  canonical location first and then every directory registered for that model class. The YAML is
  read by a minimal, dependency-free parser (this repo ships no npm deps) handling `base_path`,
  `category: dir`, and block scalars (`unet: |` listing several dirs); it is **fail-safe** —
  anything unparseable yields no extra roots, i.e. exactly the previous behavior. Verified against
  the live <node-b> file: 17 categories parsed, `unet` correctly expanded to both `diffusion_models` and
  `unet`, and the real Qwen-Image-Edit GGUF resolved on `V:`.

### Fixed — a pre-provisioned model is adopted instead of re-downloaded
- The skip gate trusted the `.sha-ok` sidecar alone, so a model placed by hand or by `curl` — with a
  byte-correct hash but no sidecar — failed the gate and fell into the **download** branch, re-fetching
  a file that was already correct. A present file with a pinned sha is now hashed **once** and, on a
  match, adopted by writing the sentinel beside the file that was actually found (which may be on the
  secondary tree, not `comfyDir`). On a mismatch it is replaced when a `source_url` exists; with no
  `source_url` it defers `MODEL_SHA_MISMATCH` — naming the real problem instead of the misleading
  "missing on disk and no source_url".
- Both defects were reported by the creative-marketing-pipelines session from its live 16 GB
  scene-swap run on <node-b>, and both are covered by new tests (43 in the file, up from 31).

## [0.22.10] - 2026-07-20

### Fixed — model matrix: Qwen-Image-Edit-2511 must pin a `_1` GGUF quant, not a `_K_` one
- The v0.22.7 designation of Qwen-Image-Edit-2511 as the ≥16 GB image-edit primitive named no quant,
  which is actively misleading: 2511 **K-quants** (`Q5_K_S` and friends, including the common unsloth
  default) fail ComfyUI-GGUF's `UnetLoaderGGUF` with `cannot reshape array` even on a byte-perfect
  file (disk sha == upstream LFS oid, gguf 0.19.0, pack at HEAD) — see city96/ComfyUI-GGUF issue #247.
  Only `Q4_1`/`Q5_1` load. Following the old wording cost a 15 GB download that then fails at load.
  Both the setup guide and the media-generation system doc now pin the `_1` quants explicitly and
  record the live `ampere-16` measurement from the creative-marketing-pipelines session (2026-07-19):
  Q5_1 (15.4 GB) + fp8 Qwen2.5-VL encoder fits 16 GB with block-swap, composite peak **15,757 MiB**
  (HiDream for comparison: 15,688 MiB) — the first real footprint datum for the Qwen edit family.

## [0.22.9] - 2026-07-18

### Fixed — run-graph model download streams to disk (models >2GB now work)
- The manifest satisfier's `download` buffered the entire model file in memory via
  `Buffer.from(await r.arrayBuffer())`, which throws a ">2GB length" RangeError on Node's
  Buffer/ArrayBuffer cap — so any model over ~2GB (Qwen-Image-Edit GGUF ~14GB, RealVisXL 6.94GB)
  could never be satisfied and the run-graph manifest deferred. It now streams the response body
  straight to disk (`pipeline(Readable.fromWeb(r.body), createWriteStream(dest))`), which has no size
  limit. The sha256-verify path already streamed, so only the download was affected. Reported by the
  creative-marketing-pipelines session hitting it live on RealVisXL. Regression test asserts the
  streaming path (a mock exposing only `body`, no `arrayBuffer`).

## [0.22.8] - 2026-07-18

### Fixed — footprint store merges on write (no cross-process clobber)
- A fleet node's footprint store `Record` overwrote `~/.local-offload/footprints.json` with only the
  writing process's in-memory entries. When `fleet-measure` and the MCP server ran as separate
  processes sharing that file, whichever wrote last **clobbered** the other's records — the live <node-a>
  symptom "only comfy-graph advertised" was the MCP's write wiping fleet-measure's freshly-measured
  entries. (The earlier "cache lock" diagnosis was a misread; footprints.json is not the cache.) The
  merge (`ReloadIfChanged`) previously ran only on the read path; `Record` now reload-merges the
  on-disk state **before** persisting, so a write folds in another process's records instead of
  overwriting them. Reproduction test added (two stores on one path → both records survive). Residual:
  two writes in the same mtime tick can still race — negligible in practice since renders are
  GPU-lock-serialized; a cross-process file lock would fully serialize if ever needed.

## [0.22.7] - 2026-07-18

### Added — Qwen-Image-Edit-2511 designated the ≥16GB image-edit primitive (model matrix)
- Documented Qwen-Image-Edit-2511 (Apache-2.0, commercial-safe) as the recommended ≥16GB image-*edit*
  primitive in `setup/SETUP-AGENT.md` and `docs/systems/media-generation.md`. It is a matrix
  *designation*, not a `config_seed` binding — edit workflows run through `run-graph` with the model
  set in the caller's node manifest, so no edit checkpoint is seeded into `config.json`. HiDream-O1
  (t2i) and Wan (video) stay the config_seed bindings; FLUX-family remains prohibited (ADR 0011).
  Aligns the harness model matrix with the creative-marketing-pipelines 16GB tier pick.

## [0.22.6] - 2026-07-18

### Changed — fleet nodes advertise the RAW footprint; the dispatcher owns all margin (CONTRACT v2.1)
- A fleet node used to pad its advertised footprint by ×1.2 (`vram_peak_gb = round(observed × 1.2)`).
  The dispatcher independently applies its own margin, so the two compounded (`observed × 1.2 × 1.2 +
  offset`) and pushed wan2.2/hidream past a 16 GB node's capacity — **unroutable** even though they
  run there. `Record` now stores the **raw** observed peak (`round(observed, 0.1)`); the dispatcher
  owns all routing margin. The `vram_peak_gb` contract field's meaning changes from padded footprint
  to raw measured peak (CONTRACT v2.1). New **ADR 0013** records the decision and supersedes the
  padding part of ADR 0008 (the PDH sampling core of 0008 is unchanged). Footprint tests updated to
  the raw values.

## [0.22.5] - 2026-07-18

### Changed — VENV_INCOHERENT defers say WHY (host-pin drift vs dependency conflict)
- The run-graph pack satisfier's `pipCheck` tripwire returned a bare boolean for both a genuine
  dependency conflict and host-pin drift (a pack moving `torch`/`torchvision`/`torchaudio`/`numpy`),
  so every `VENV_INCOHERENT` defer read `"conflicting installed dependencies"` and the actionable
  drift diagnostic — which pinned package moved, expected vs observed — was written only to stderr.
  `pipCheck` now returns `{ok, reason}` and the defer surfaces that reason, so a consuming workflow
  can tell drift from a conflict and see the exact pins. Makes a Qwen-image-edit (or any node-pack)
  manifest-satisfaction defer actionable instead of opaque.

## [0.22.4] - 2026-07-18

### Fixed — run-graph creates a caller-supplied out_dir instead of ENOENT-ing
- A caller-supplied `out_dir` that did not yet exist failed at first output write with an opaque
  `RUN_ERROR` — only the *defaulted* media dir was created, never a caller's directory. The Go side
  now resolves and `MkdirAll`s the output directory in either case (new `resolveOutDir` helper, unit
  tested); a directory that genuinely cannot be created defers typed ("cannot create out_dir") rather
  than failing later. The standalone `render/comfy-run-graph.mjs` write path `mkdirSync`s the target
  as well, so a fresh out-dir works there too.

## [0.22.3] - 2026-07-18

### Fixed — run-graph model satisfier crashed with "require is not defined"
- `render/manifest-satisfy.mjs`'s `defaultSatisfyDeps` called `require("node:fs")` /
  `require("node:path")` inside this ESM module, which throws `ReferenceError: require is not
  defined`. Two real failures resulted: a model **present on disk with a manifest `sha256` but no
  `.sha-ok` sentinel** fell through to the download branch and deferred `MODEL_DOWNLOAD_FAILED`
  ("require is not defined") even though nothing was wrong with the file, and a **fresh download**
  threw from `writeSentinel` (which sat outside any try/catch) and crashed the whole run with an
  untyped exit. Replaced the four `require()` calls (one in `writeSentinel`, three in `download`) with
  the existing ESM imports (adding `mkdirSync`).
- Hardened the post-download path in `satisfyModels`: the hash read and sentinel write are now
  guarded, so a genuine filesystem failure defers typed (`MODEL_DOWNLOAD_FAILED`) instead of escaping
  as a process crash.
- Added regression tests exercising the real `defaultSatisfyDeps.writeSentinel` / `.download`
  closures (the production glue was previously untested — the gap that hid this).

## [0.22.2] - 2026-07-18

### Changed — operator-neutral memory namespace (multitenancy)
- **The optional `--memory` recall namespace is no longer hardcoded.** The agent previously always
  recalled from a compiled-in `"dmmdea"` namespace alongside its own; it now recalls only its own
  namespace by default and appends an operator/shared namespace only when one is configured via the
  new `--mem-shared-namespace` flag or the `MEM0_SHARED_NAMESPACE` environment variable. This makes
  the public build operator-neutral (multitenant) — no personal namespace is baked into tracked code.
  New helper `agent.ReadUsers` builds the list; behavior is unchanged for an operator who sets the
  namespace. First step of the repo-model inversion (public becomes the canonical, multitenant source).

### Fixed
- **`TestDocsLint` is now line-ending agnostic.** The ADR frontmatter check anchored on `\n`, so a
  Windows checkout with `autocrlf` (CRLF working tree) failed every ADR. The regex now accepts
  `\r?\n`, so the gate passes for any contributor regardless of line-ending configuration.

## [0.22.1] - 2026-07-18

### Added — repo-local documentation system
- **`docs/` is now a navigable knowledge base** for humans and coding agents, with `AGENTS.md`
  as the routing layer: `systems/` (offload pipeline, coding agent, MCP server, media generation,
  fleet node, setup/installer), `flows/` (cascade escalation and defer, run-graph manifest
  satisfaction, fleet job lifecycle, zero-warm generation), `architecture/decisions/` (ADRs
  0001–0011, backfilled from decisions that previously lived only in `CLAUDE.md` invariants and
  session records), `glossary.md`, `templates/`, and `STYLE.md`.
- **`TestDocsLint`** — a structural gate run by `go test ./...`: scaffold files exist, relative
  links resolve, ADR frontmatter is schema-valid, and system/flow docs keep their navigational
  sections. Scoped to the durable documentation surface; `docs/templates/` and the dated
  `docs/superpowers/` archive are exempt from link resolution by design.
- **`CONTRIBUTING.md` documentation section** — read before changing, update in the same PR, and
  the three legal ways to resolve a docs/code disagreement.

### Fixed — documentation accuracy
- Corrected four `CLAUDE.md` claims that disagreed with the code, each re-verified against source:
  KV cache type is profile-driven (`q8_0` on 9 of 13 profiles) rather than `f16` everywhere, with
  an `amd-gcn` flash-attn exception, a cpu template that omits the flag, an `embeddinggemma` entry
  that bypasses the shared macro, and no STT carve-out (no whisper entry is templated); the policy
  broker gates effectful actions while the **loop** owns step and tool budgets (two mechanisms,
  previously described as one); `--profile` and `--two-tier` conflict only for a non-default
  profile; and the **cascade** never calls cloud while `offload_nim` is an explicit opt-in side
  channel.

### Known issues documented (not yet fixed)
- **run-graph model leg** — `render/manifest-satisfy.mjs` calls `require()` in an ESM module.
  A model present on disk with a declared `sha256` but no `.sha-ok` sentinel re-enters the download
  branch and defers as `MODEL_DOWNLOAD_FAILED: "require is not defined"`; a *successful* fresh
  download throws out of `writeSentinel` outside the try/catch and exits untyped. `sha256: null`
  works around both by skipping the branch entirely. `defaultSatisfyDeps` has no test coverage,
  which is why the suite stayed green. See `docs/flows/run-graph-manifest-satisfaction.md`.
- **`VENV_INCOHERENT` diagnosability** — host-pin drift and an ordinary dependency conflict share
  one defer detail; the drift diagnostic reaches stderr only.
- **`.git` mask asymmetry** — the read-only `.git` protection for the shell path is Linux-only; the
  native-Windows `run` path has no equivalent. The broker's `.git` denial still covers file tools on
  every platform. See ADR 0004.

## [0.22.0] - 2026-07-17

### Added — fleet-node server (`fleet-serve` / `fleet-measure`)
- **`fleet-serve`**: this box can now join the Fleet Dispatcher fleet (CONTRACT.md v2) —
  `GET /fleet/health` (live GiB VRAM, derived task/family lists, measured footprints, queue
  depth), `POST /fleet/dispatch` (immediate 202 ack with exact `job_id` echo; idempotent on
  duplicates; async execution through the existing pipeline + single-slot GPU lock), and
  `GET /fleet/jobs/{id}` (accepted→running→done|error; terminal results retained ~1h).
  New `internal/fleetnode` package: contract-exact HTTP server, drain-safe ack-then-poll
  job store, task-type mapping with strict raw-JSON run-graph payload validation at ack
  time, and a two-source VRAM sampler (nvidia-smi global snapshot every 2s; Windows PDH
  `\GPU Process Memory` per-process-tree source for footprints). Startup GPU probe refuses
  to stand up a zero-VRAM node; SIGINT drains for 30s and marks survivors
  `error:"interrupted"`. Loopback by default via the shared `internal/netguard` guard
  (extracted from local-agent, behavior identical); production binds the Tailscale address
  behind `--listen-trusted-network` on port 18811.
- **Passive measured footprints**: every image/video/audio/run-graph render through the
  pipeline now records its observed VRAM peak into `~/.local-offload/footprints.json`
  (max-keep; advertised `vram_peak_gb` = observed × 1.2), keyed by this machine's actual
  bindings — so footprints accumulate during normal harness use and stay current when
  bindings change. Implemented as a nil-gated `gpugen.Spec` sampling hook: the non-fleet
  render path is byte-identical when unset. **`fleet-measure`** primes an empty store (one
  minimal render per configured task) and prints the recorded entries.
- Config: `fleet_listen` (default `127.0.0.1:18811`), `fleet_node_id` (default hostname at
  serve time), `fleet_sampler` (`auto|pdh|global`).
- Docs: new `docs/FLEET-NODE.md` (config, Tailscale binding guidance, sampler modes, the
  PDH-vs-Afterburner validation procedure, and the recommended — never required — MSI
  Afterburner companion setup) + README/OPERATOR-GUIDE/SETUP-AGENT fleet sections.

## [0.21.1] - 2026-07-17

### Added — auto-text inpaint chain enabled (grounding eval passed)
- `inpaint-image --auto-text` now runs by default: the Task-9 grounding eval passed 3/3
  (text-stamped renders; qwen3vl found, boxed, and erased the text with zero wrong-region
  repaints; oversized images defer cleanly on the vqa load limit). The always-defer gate
  was removed per its recorded unlock condition.

### Fixed — run-graph pack satisfier drives uv directly
- The installed ComfyUI-Manager cm-cli has no `--uv` flag (live scene-swap satisfy run
  deferred VENV_INCOHERENT, typed defer + host torch untouched — exactly as designed).
  The satisfier now checks out packs first (git), then runs ONE `uv pip compile` across
  all packs on-disk requirements under the host-torch constraints and installs the lock.
  `uv` in the ComfyUI venv is the required satisfier tool (install.ps1 provisions it);
  cm-cli is no longer load-bearing for run-graph.

## [0.21.0] - 2026-07-17

### Added — edit-image op pack (deterministic post-production suite)
- **`grade`** `{levels{black,white,gamma}?, curve{points}?, wb{gray_world|scale}?, luminance_only?}`:
  tone/color grading with compose-once LUT discipline — every transform composes into ONE
  256-entry float LUT per channel and quantizes in a SINGLE `Image.point()` call (chained
  8-bit passes band visibly); the alpha band is never remapped.
- **`lut_cube`** `{path, strength?}`: `.cube` 3D LUT looks via Pillow's built-in `Color3DLUT`
  (vendored pure-python parser; 1D cubes and non-standard domains rejected); `strength` 0–1
  blends graded over original.
- **`perspective_composite`** `{overlay, quad:[[x,y]×4]}`: mockup placement — pure-python
  homography (partial-pivot Gauss, no numpy) warps the overlay into the destination quad
  (UL,UR,LR,LL winding), BICUBIC, alpha-composited.
- **`finish`** `{sharpen{radius,percent,threshold}?, median 3|5?}`: delivery sharpening with
  post-AI-upscale web defaults (1.2/80/3 — Pillow's 150% default over-crisps upscaler
  output). MUST run as the LAST op, after any resize (resampling undoes earlier
  sharpening). Real NLM/BM3D-class denoise is documented as out of PIL's scope, not faked.
- **`renditions`** (Go-side export matrix): optional `renditions[]`/`--renditions`
  `[{width/height, format png|jpg|webp, suffix}]` — after the master `out`, each entry
  re-runs the worker (resize+convert) writing `<out-stem><suffix>.<format>`; result gains
  `renditions[]`.
- **`instantiate_design`** `{set_text{Layer: copy}, replace_image{Layer: path}}` (FIRST op
  only, like `flatten_design`): GIMP layered-template factory — generated Script-Fu sets
  named text layers' copy, swaps named pixel layers at the old offsets, flattens, and feeds
  the raster to the PIL pipeline (one-call brand-variant factory). PDB calls verified
  against the installed gimp-console-3.2 (`gimp-drawable-get-offsets` 3.x naming); headless
  GIMP invocations are now serialized process-wide (profile-lock contention), and a
  no-raster script failure surfaces GIMP's stderr (layer-name mismatch is THE common case).
- Docs: README op table/CLI examples, OPERATOR-GUIDE "Deterministic post-production"
  section, `render/gimp-instantiate.scm.tmpl` (reviewable batch contract), MCP
  `offload_edit_image` description enumerates the pack + both ordering rules
  (instantiate_design first; finish last).
- Existing five ops, mask_boxes, flatten_design, and all generate/batch/run-graph/inpaint
  paths are unchanged (locked by the full suites).

## [0.20.0] - 2026-07-17

### Added — generative inpainting route (`offload_inpaint_image` / `inpaint-image`)
- SDXL-family **masked re-denoise** on the local ComfyUI (core nodes only:
  `LoadImage → ImageToMask(red) → VAEEncodeForInpaint → KSampler → VAEDecode`): re-renders
  ONLY the white region of a same-size white-on-black mask from a prompt, leaving every
  other pixel untouched. New `render/wf-sdxl-inpaint.mjs` graph builder +
  `render/comfy-inpaint.mjs` runner (staged inputs, single-slot GPU lock, zero-always-warm
  teardown), `imagegen.Inpaint` exec wrapper, `inpaint_image` pipeline task, MCP tool
  `offload_inpaint_image` `{image,mask,prompt,negative?,denoise?,grow_mask?,steps?,seed?,out?}`
  → `{image_path, seed}`, and the `inpaint-image` CLI.
- Per-machine binding via new config `inpaint_script` / `inpaint_ckpt` / `inpaint_vae` /
  `inpaint_steps` / `inpaint_cfg` / `inpaint_sampler` / `inpaint_scheduler` /
  `inpaint_timeout_sec` (default 900). The binding must be **SDXL-class** (e.g. RealVisXL):
  `VAEEncodeForInpaint` is latent-space — a pixel-space DiT (HiDream) cannot drive it, so a
  HiDream box keeps HiDream for generation and binds inpaint separately. Unbound = clean defer.
- New deterministic `mask_boxes` edit op (`edit_image` pipeline):
  `{op:"mask_boxes",boxes:[{x,y,width,height}],pad?,feather?,invert?}` replaces the working
  image with a white-on-black inpaint mask at its size — the manual mask workflow.
- EXPERIMENTAL `inpaint-image --auto-text`: vision-detected text boxes chained through
  `mask_boxes` into the inpaint; hard validation (unparseable/empty/absurd >60%-coverage
  boxes) defers with the manual `mask_boxes` workflow named. Grounding acceptance on real
  gibberish renders is still pending live eval — treat as opt-in sugar.
- Note: diffusion cannot WRITE specific legible text — inpaint-to-clean, then add real
  type with the `edit_image` `text` op.

## [0.19.0] - 2026-07-17

### Added — warm batch mode (`generate-image --batch`)
- N prompts through ONE warm ComfyUI session (checkpoint loads once): `generate-image
  --batch <jobs.jsonl>` with per-job `{prompt, negative?, width?, height?, steps?, seed?,
  out?}` overrides and a per-job result report `{count,succeeded,failed,items[]}`.
  Measured on the 16GB box: 32.6s first job (absorbs the checkpoint load) → **22.4s warm
  floor**; the old zero-warm cadence re-paid the load on every render. **Zero-warm stays
  the single-render default** — warmth exists only inside an explicit batch, and the full
  teardown (freeComfy + kill + lock release) is restored at the batch boundary, verified
  live (ComfyUI down, VRAM idle, memory stack intact, lock released). GPU lock held across
  the whole batch; per-job ledger records with ErrClass parity.
- Operational: ComfyUI-Manager on a satisfier box should run `network_mode = offline`
  (its first-start registry fetch regressed cold-start ~40s→>150s; offline verified 19s).

## [0.18.0] - 2026-07-17

### Added — run-graph primitive (`offload_run_graph` / `run-graph`)
- Generic execution of an **arbitrary ComfyUI API-format graph** in the proven GPU-lock /
  zero-always-warm lifecycle, with a per-workflow **node manifest** (custom node packs @
  pinned commits + model files with optional sha256) satisfied as part of the contract:
  unified `uv` dependency resolve via cm-cli (never sequential per-pack pip), `pip check`
  coherence gate, models downloaded + sha-verified (null-sha → reported in
  `unverified_models[]`), packs **provisioned to disk BEFORE ComfyUI starts** so they load
  on first start. Node-addressed outputs `{node_id:[{path,type,kind,width,height}]}` +
  `image_path` alias; every failure a **typed DEFER** `{deferred,code,ref,detail}` — never
  cloud. New config: `run_graph_script`. Spec:
  `docs/superpowers/specs/2026-07-17-run-graph-primitive-design.md`.
- Setup: `Ensure-RunGraphDeps` provisions ComfyUI-Manager (cm-cli, required) + GitPython
  (required by cm-cli itself) + comfy-cli (optional, best-effort).

### Fixed / hardened
- **Host-constraints (v1 protection):** every pip/uv the satisfier spawns runs under a
  constraints file pinning the host's `torch/torchvision/torchaudio/numpy`
  (`PIP_CONSTRAINT`/`UV_CONSTRAINT`), plus a post-install drift tripwire → a pack set that
  cannot install additively around the CUDA stack defers `VENV_INCOHERENT` instead of
  silently replacing ComfyUI's torch (live finding: the scene-swap lock resolved
  torch 2.11.0+cu128 → 2.13.0, which would have broken the Wan video path).
- Empty/models-only manifests skip the pack satisfier entirely (no cm-cli invocation).
- git arg-injection hardening in pack provisioning (`--` clone guard + commit-ref charset).

## [0.17.0] - 2026-07-16

### Added
- **`offload_edit_image`** — deterministic image-edit PIPELINE in one call (ops applied
  in order: crop / resize / convert / composite / text via a PIL worker on the ComfyUI
  venv python, auto-derived; `flatten_design` as the first op opens `.xcf`/`.psd` via
  GIMP, flattens, and returns the layer list — script-fu template live-verified on
  gimp-console 3.2, no raw script pass-through ever). New config: `edit_python`,
  `gimp_console_path`, `edit_timeout_sec`.
- **`offload_media`** — one ffmpeg av operation per call: `trim` (keyframe-snapped
  stream-copy default, `reencode` for exact cuts), `concat`, `extract_frames` (fps or
  count-via-probe), `convert`, `mux_audio`, `probe` (parses `ffmpeg -i` stderr —
  imageio_ffmpeg ships no ffprobe; fixture-tested against the real 7.1 banner).
- Both tools are **CPU-only and never take the GPU lock** — they run in parallel with
  renders and never evict llama-swap. Engines surface in `offload_status.media`
  (`edit_pil` / `edit_gimp` / `media_ffmpeg`); every failure class defers cleanly.
  MCP + CLI (`edit-image`, `media`); NOT registered in the read-only agent loop.
  Spec: `docs/superpowers/specs/2026-07-16-edit-media-tools-design.md`.

## [0.16.0] - 2026-07-16

### Added — quality-first generation (root-cause fix, all hardware tiers)
_Directive (operator, 2026-07-16): highest-quality deliverables always, on all hardware; speed variants
opt-in only. Spec + measured evidence:
`docs/superpowers/specs/2026-07-16-quality-first-generation-design.md`._
- **HiDream-O1 official graph** (`render/wf-hidream-o1.mjs`, selected via new
  `imagegen_family` config): ModelNoiseScale, patch-seam smoothing (kills the measured
  3× 32px patch blocking of the generic-graph path), the SamplerCustom stack, native
  2048 resolution, base (40/5/SDE) + dev (28/1/LCM) variants. Generic-graph machines
  byte-for-byte unchanged when unset.
- **Per-machine Wan weight binding** (`videogen_unet_high/low`, `videogen_text_encoder`):
  extension-keyed GGUF/safetensors DisTorch2 loaders, never down-cast — unblocks Q8_0/
  fp16 weights over the Q4 defaults.
- **Video native recipe is now the DEFAULT** (no distill LoRA, 20 steps, cfg 3.5, the
  official Wan training negative); `fast:true` opts into the improved 8-step asymmetric
  lightx2v distill (HIGH LoRA 0.7 + cfg 3, LOW 1.0 + cfg 1). `hero` = deprecated no-op.
- **Quality-first `config_seed` on every ≥16GB CUDA tier**: bf16 O1 Base + family graph +
  Q8_0 Wan experts + umt5 fp16 + 720p×81 (proven on the 16GB tier: 3.9 min/2048 render).
- `comfy-render.mjs` poll ceiling now `COMFY_WAIT_SEC`-driven (the hardcoded ~6-min
  ceiling would kill legitimate quality renders); Go aligns it to `imagegen_timeout_sec`.

### Fixed
- **LO-19: `offload_generate_video` advertised `hero`/`upscale` but never mapped them** —
  MCP callers requesting the quality pass silently got the 4-step draft. `fast`/`hero`/
  `upscale` now flow through.

## [0.15.0] - 2026-07-16

### Added
- **Blackwell profile tiers above 16GB (configs #13–15).**
  `detect.ps1` now classifies RTX PRO Blackwell workstation cards (their names — e.g.
  "NVIDIA RTX PRO 5000 Blackwell" — matched NO arch rule and fell into the ampere bands)
  and bands Blackwell VRAM above 16GB: `blackwell-32` (≥24GB), `blackwell-48` (≥40GB),
  `blackwell-72` (≥64GB; 96GB cards share it until measured). The new tiers render a new
  `cuda-resident` template: every model standalone, **no swap group, no ttl** — the whole
  roster stays hot concurrently (zero swap latency). 48/72 serve 128K ctx with
  **full-precision f16 KV**; 32 serves 64K q8_0. All values PROJECTED until an H3-style
  selftest runs on real ≥24GB hardware. Spec:
  `docs/superpowers/specs/2026-07-16-blackwell-profile-tiers-design.md`.
- **Profile `config_seed` (seed-on-create media defaults).** A profile may carry
  `config_seed` in profiles.json; install Step 8 overlays it onto the template config
  ONLY when creating `~/.local-offload/config.json` fresh (an existing per-machine
  config is never touched). The big tiers seed 720p-class video defaults.
- **`offload_status` MCP capability-discovery tool (LO-18).** Fixes the NIM-vs-local
  asymmetry: `offload_nim` was the only tool that named or listed models, so agents
  inspecting the harness concluded the text/LLM capability was the cloud NIM catalog and
  never discovered the LOCAL cascade. `offload_status` (registered first) reports the
  configured local roster, live-probes the endpoint's `/v1/models`, lists the machine's
  media engines, and names NIM as the only remote surface; local tool descriptions
  de-anonymized ("the LOCAL model cascade" instead of "a free local model").

## [0.14.0] - 2026-07-15

### Added
- **H4: flexible CUDA-keyed llama.cpp build selection (workstation/Blackwell).**
  `setup/install.ps1` now picks the CUDA build from the *detected* CUDA (never a fixed
  assumption): Blackwell (sm_120) profiles on a CUDA-13 driver install a new pinned,
  SHA-verified **cuda-13.3** prebuilt (`llama-cuda13`/`llama-cudart13`) — the SERVE tier
  (MMQ→cuBLAS fallback, ~5.6× slower prefill; peak = documented source-build vs a
  12.8/12.9 toolkit, noted when one is detected). Blackwell on a 12.x/undetected driver
  refuses loudly with driver-upgrade-or-source-build guidance; `dual-gpu` refuses with the
  multi-arch recipe (`-DCMAKE_CUDA_ARCHITECTURES="70;120"`, 12.8/12.9 toolkit — CUDA 13
  cannot compile sm_70); all other CUDA profiles keep the verified 12.4 prebuilt.
  `installed.json` records `cuda_build` under the selected component key so a driver
  upgrade or the V100 arriving forces the correct re-install on re-run. New synthetic-box
  overrides: `OFFLOAD_CUDA_DRIVER` / `OFFLOAD_CUDA_TOOLKIT`.
- **Blackwell runtime env auto-injection.** `blackwell-*` renders now carry
  `CUDA_VISIBLE_DEVICES=0` + `CUDA_MODULE_LOADING=LAZY` on every model block of the
  rendered `llama-swap.yaml` (idempotent injection; the 26B's `GGML_CUDA_DISABLE_GRAPHS`
  list is extended in place).
- **Tests:** `setup/tests/install-cuda-build.test.ps1` (dot-source seam
  `OFFLOAD_INSTALL_DOT_SOURCE=1`; pwsh 7 + PS 5.1) + Blackwell env assertions in
  `setup/render.tests.ps1`.

### Fixed
- **detect.ps1 missed the driver CUDA on newer drivers.** Drivers like 610.62 print
  `CUDA UMD Version: 13.3` instead of the classic `CUDA Version:` banner; the parse is now
  a self-tested pure function covering both formats. (Found live on the workstation —
  `cuda_driver` reported null on the exact box H4 keys its selection off.)

## [0.13.0] - 2026-07-15

### Added
- **Config-selectable voice paths (wiring).** `generate_audio kind=voice` now takes a
  `voice` selector (`generalist` | `finetuned`). The generalist path is the stock
  Chatterbox multilingual worker (a new `voicegen_ref` supplies a default es-MX reference
  clip). The `finetuned` path is a per-machine voice located entirely by config
  (`voicegen_ft_model` / `voicegen_ft_base_dir` / `voicegen_ft_ref` / `voicegen_ft_lang` +
  the `voicegen_ft_{temperature,cfg_weight,exaggeration,repetition_penalty}` recipe knobs);
  empty config → clean defer, so shared code carries no model name or personal path.
  `render/tts.mjs` branches on `--engine` to dispatch to the stock `tts_chatterbox.py` or the
  new fine-tuned worker `tts_chatterbox_ft.py`, exposed on the MCP tool + CLI as `voice`.
- **Fine-tuned worker skeleton** `render/tts_chatterbox_ft.py` — arg contract + path
  validation; defers (exit 3) until its vendored-engine load path is built + tuned in a
  later session. See `docs/superpowers/specs/2026-07-15-config-selectable-voice-wiring-design.md`.

## [0.12.0] - 2026-07-15

### Added
- **Video quality options (universal, param-driven — never hardware-baked):**
  - **`hero` mode** — a native no-LoRA quality pass for the Wan builder (skips the distill LoRAs, native
    steps/cfg). Slower, but restores the camera/subject motion the 4-step LoRA trades for speed — the
    per-research win for realistic b-roll. `--hero` (CLI) / `hero:true` (tool). Fast 4-step stays default.
  - **`upscale`** — an optional post-decode ESRGAN upscale (`UpscaleModelLoader` → `ImageUpscaleWithModel`
    → optional resize), e.g. 720p→1080p. The upscale model + target are **per-machine config**
    (`videogen_upscale_model` / `videogen_upscale_width` / `videogen_upscale_height`) so no model name is
    baked into shared code; a machine with no upscale model just skips it. `--upscale` (CLI) / `upscale:true`.
  - (Frame count remains the per-machine `videogen_frames` knob — 81 ≈ a real 5s clip.)

## [0.11.0] - 2026-07-15

### Fixed
- **Video (`render/comfy-video.mjs` + `render/wf-wan22-i2v.mjs`): the default I2V path now works and is fast.**
  Default model flipped Hunyuan→**Wan 2.2** (Hunyuan needs 3 files absent on this box), with the JS-scoping
  bug fixed. Wired the on-disk **4-step lightx2v LoRAs** (HIGH on the high-noise expert, LOW on the low-noise
  expert) — ~91s for a 25-frame 480p clip vs ~12-25min native. Fixed a pre-existing wrong VAE default
  (`wan2.2_vae` is the 48-ch 5B VAE; the 14B A14B I2V needs the 16-ch `wan_2.1_vae` → the `patch_embed`
  36-vs-64-channel error). Live-verified end-to-end at 480p AND 720p.
- **Music (`render/wf-acestep.mjs`): rewritten from the retired v1 all-in-one to the ACE-Step v1.5 split
  stack** (UNETLoader + DualCLIPLoader type `ace` + VAELoader + the 1.5 encoder/latent/AuraFlow nodes), all
  on disk; every input verified against the live `/object_info`. Live-verified (10s FLAC).

### Added
- **Per-machine video resolution** (`videogen_width` / `videogen_height` / `videogen_frames` config): a 16GB
  box defaults to 720p while an 8GB box stays at the builder's 480p default (a per-request value still wins).
  Keeps the harness hardware-agnostic — resolution is config, not a constant. The 16GB box is set to 1280×720.

## [0.10.2] - 2026-07-15

### Changed
- **`Default()` no longer ships phantom model names** (opt-in defaults): `vision_model` and
  `stt_model_hq` default to `""` instead of `qwen3vl-4b` / `whisper-stt-hq` (aliases served on no
  current machine). A machine that omits them now cleanly disables that route instead of inheriting a
  model that errors → cloud-defers. Configured machines are unaffected (they set both). Template +
  `config.example.json` updated.

## [0.10.1] - 2026-07-15

### Fixed
- **whisper-stt crash resilience** (`internal/sttclient`): the "whisper-server 502" was a whisper-server
  SIGSEGV, not a request bug (real speech transcribes fine). Two harness-side mitigations:
  - A real process-global serialization mutex around the inference request — whisper-server is
    single-slot and crashes on overlapping requests (the `Client` doc-comment claimed serialization but
    no mutex existed).
  - An empty-body 5xx (the crash signature) now yields a descriptive, diagnostic error (crash /
    near-silent audio / cold-restart) instead of a bare status code, so the defer reason is accurate.
  - (Machine-local `config.json`: `stt_unload_after:false` keeps whisper warm between back-to-back calls
    so no-speech input returns 200-empty instead of cold-crashing — not part of this repo diff.)
  - The full fix (whisper `ttl:-1` in the live llama-swap so it never cold-loads) needs operator
    approval and is not included. See docs/superpowers/evidence/2026-07-15-whisper-crash-resilience.md.

## [0.10.0] - 2026-07-15

### Fixed
- **Model-roster hardcodes removed from shared code** (keeps the harness hardware/model-agnostic — the
  roster is per-machine config, never baked in):
  - `internal/judge/embed.go` no longer hardcodes `"embeddinggemma"` — `NewEmbedder` takes the model,
    threaded from a new `Config.EmbedModel()` accessor (`MemoryStack[0]`, with the fallback living only
    in config). The genuine model-agnosticism violation.
  - `internal/mcpserver` `agent_run` planner default and `cmd/local-agent` `--model` / `--architect-model`
    / `--editor-model` now fall back to the configured model (`cfg.Model` / `cfg.EscalationModel`) instead
    of the literals `offload-e4b` / `gemma4-26b-a4b`.

### Added
- **`ocr_max_tokens` config** (default 1024): a machine with a strong VLM can raise the OCR output cap so
  a dense document page transcribes locally instead of hitting the 1024 ceiling (`finish_reason=length`)
  and deferring the whole OCR to cloud. Threaded into the vision dispatch; covers `extract_image` too.
- Guard tests: `TestEmbedUsesConfiguredModel`, `TestOCRHonorsConfiguredMaxTokens`, `TestModelFlagFallsBackToConfig`.

## [0.9.0] - 2026-07-14

### Fixed
- **Router entry tier is no longer hardcoded** (`internal/router`): `Train` takes the entry-tier
  model from config (`cfg.TriageModel`) instead of the literal `"gemma4-e2b"`. On any machine whose
  hardware-optimized roster names its triage model differently (e.g. `gemma-4-e2b`), the ledger rows
  never matched, so the self-optimization router silently collected 0 rows and never trained. The
  harness stays hardware/model-agnostic: the roster is per-machine config, never baked into shared code.

### Added
- **Per-machine image-model binding** (`internal/config`, `internal/imagegen`, `render/comfy-render.mjs`):
  `imagegen_ckpt` / `imagegen_vae` / `imagegen_steps` / `imagegen_cfg` / `imagegen_sampler` /
  `imagegen_scheduler`. A machine renders with the checkpoint its hardware can run — SDXL on an 8 GB
  box, an all-in-one DiT such as HiDream on a 16 GB box via `--vae builtin` (decodes with the VAE the
  checkpoint loader supplies; HiDream ships no VAE weights). Every field is optional and a zero binding
  emits no flags, so an unbound machine renders exactly as before.
- **Version-consistency guard** (`main_test.go` `TestVersionSourcesAgree`): the `VERSION` file, the
  compiled-in `version` const (advertised in the MCP handshake), and the top `CHANGELOG.md` entry must
  all name the same version — a build failure now catches drift. They had drifted to
  `VERSION` 0.7.0 / const 0.6.2 / CHANGELOG 0.7.0.

### Changed
- Version reconciled to **0.9.0** so this canonical private repo sits ahead of the public mirror
  (`offload-harness`, published at 0.8.0), per the versioning invariant. Folds in the 0.8.0 line already
  present in this tree — local coding-agent capabilities + the per-hardware optimization matrix — plus
  the CUDA-toolkit / Blackwell `detect` work the 0.8.0 publish did not carry.

## [0.7.0] - 2026-07-09

### Added
- **Cross-vendor Windows setup** (`setup/`): hardware detector (NVIDIA→CUDA, AMD→Vulkan incl. RDNA3 iGPUs like the Radeon 780M, CPU fallback), idempotent installer with pinned+SHA-verified assets and models, and a receipt-emitting selftest (per-tier swap-group exercise, deep-context Vulkan canary at ~7K depth, automatic `--cpu-moe` remediation, honest proves/does-not-prove scoping).
- **Local coding agent published** (`cmd/local-agent`): plan→tool loop over a local model with policy-brokered write/edit/search/GitHub tools, OpenAI-compatible `--serve` mode (loopback-only by default; `--listen-trusted-network` override), same-tool circuit breaker, and `--max-tokens` control.
- **Agent-facing docs**: `setup/SETUP-AGENT.md` install runbook for AI agents, `AGENTS.md`, `CLAUDE.md` orientation map, `docs/OPERATOR-GUIDE.md`.
- **Serving templates** for llama-swap on Windows (Vulkan / CUDA / CPU) with grammar-reliable flags.

### Changed
- README: cross-vendor requirements, agent setup entry, security posture section.
- `config.example.json`: escalation/reasoning tiers now reference the served `gemma4-26b-a4b`.

## [0.6.0] — 2026-06-29

### Added
- **Remote NVIDIA NIM tool** (`nim` CLI subcommand + `offload_nim` MCP tool) — an explicit, opt-in path to any OpenAI-compatible NVIDIA NIM endpoint: NVIDIA's hosted [build.nvidia.com](https://build.nvidia.com) catalog (dozens of **free** models — nemotron-3-ultra-550b, llama-3.3-70b, gpt-oss, qwen, deepseek, glm, kimi…) by default, or a **self-hosted** NIM container via `--base http://host:8000/v1`. It lets the harness reach frontier models a small local GPU can't run, used deliberately rather than for routine grunt work.
  - Model-agnostic: pick any model per call (`--model` / `model`), or browse the catalog with `nim --list-models` (`list_models:true`).
  - **The API key is read from `$NVIDIA_API_KEY` (or `$NGC_API_KEY`) only — never a config field**, so a secret never lands in a tracked file or the public repo. A self-hosted NIM is keyless.
  - **Stays out of the cascade and the savings ledger:** NIM calls are deliberate remote experiments/escalations, not local defer-avoidance, so they are never recorded as Opus-tokens-saved. The local Gemma cascade and its sacred GBNF grammar path are untouched.
  - Defers-not-crashes: an unset key on the hosted endpoint, a down endpoint, or a bad model id returns a clean error (CLI) / `{deferred:true, reason}` (MCP), never a panic.
- New `internal/nimclient` package (pure `net/http`, fully unit-tested) and read-only `Pipeline.Cfg()` accessor for side-channel tools.

### Changed
- Config gains `nim_endpoint` / `nim_model` / `nim_max_tokens` / `nim_timeout_sec` (all defaulted; the hosted endpoint + nemotron-3-ultra-550b are the defaults). No existing behavior changes.

## [0.5.0] — 2026-06-29

### Added
- **Local media generation** on the single 8 GB GPU, behind a generalized single-slot scheduler (each is opt-in; the default text/vision/STT runtime is unchanged, and every path defers cleanly when its model/ComfyUI/script is absent):
  - **Voice / TTS** — `offload_generate_audio` `kind=voice` (CLI `generate-audio`): Chatterbox Multilingual (commercial-safe, default Spanish, zero-shot voice cloning via `clone=`). Verified end-to-end (a real 2.84 s WAV through the harness).
  - **Music** — `offload_generate_audio` `kind=music`: ACE-Step 1.5 text-to-music (style prompt + optional lyrics, seeded). Verified end-to-end (a real 7.99 s FLAC).
  - **Video** — `offload_generate_video` (CLI `generate-video`): Hunyuan 1.5 480p image-to-video. Wiring complete and the ComfyUI graph executes cleanly. **Caveat:** a quality render (`steps=50`) is throughput-gated on the 8 GB 3070 — it exceeds the worker's ~20-min window — so video is wired but impractical on this card until a step-distilled checkpoint / a fast tier (LTX) / a larger-VRAM GPU.
- **Generalized GPU single-slot scheduler** (`render/gpu-lock.mjs` `withGpuSlot` + shared `render/comfy-lifecycle.mjs`): one cross-process lock serializes image/video/audio generation; the guarded lifecycle (freeLlamaSwap → ensureComfy → guarded teardown + signal handlers) is centralized; new `internal/gpugen` adds a Windows process-tree-kill on timeout so a gen run can't orphan a VRAM-pinning ComfyUI child. `MEMORY_STACK` (the CPU models never unloaded) is now config/env-sourced.

### Changed
- `internal/imagegen` is now a thin caller of `internal/gpugen`; image-generation behavior is unchanged (byte-equivalent).

## [0.4.2] — 2026-06-29

### Added
- **Live hot-reload of self-learning artifacts.** The long-running MCP server now picks up nightly-retrained weights/thresholds/overrides without a restart — a stdlib content-hash poll reloader (fail-open last-good; the confhead head+thresholds are swapped atomically as one snapshot; the append-grown kNN index is excluded; all artifact writers are atomic tmp+rename). Starts only in `mcp` mode; CLI one-shots are byte-identical.
- **`offload eval --confhead-ab`** — a paired A/B decision-gate that replays a held-out gold set with the confidence head OFF vs ON (staged weights via a temp config, never touching live) and reports per-task selective-accuracy / cost / AUDC frontier dominance plus a calibrated-margin baseline. The reusable gate for deciding whether enabling the head is a net win.
- **Calibration diagnostics:** AUGRC + ECE reported alongside the confhead-eval AURC verdict; realized-accepted-error vs target in confhead-calibrate. Diagnostics only — they never change the adoption verdict.
- A larger, **unambiguous, consistently-labeled** classify/triage eval gold corpus (162/158 train + 45/40 disjoint held-out) with an explicit `testdata/eval/LABELING-RUBRIC.md`.

### Fixed
- **Router/kNN label feeder revived.** The shadow drain's router-label + kNN-substrate synthesis was structurally dead (it only fired for non-E2B-entry rows, which capture never produces). It now derives router + kNN labels from the escalation-agreement signal already computed for E2B-entry rows — zero extra inference, savings ledger untouched.

### Changed
- Confhead/calibration emission floor `minRows` 100 → 60 (emission gate only; the OOF paired-bootstrap CI remains the adoption guard). `alpha`, `target_error_rate`, and the conformal CRC are unchanged.

### Notes
- **The confidence head was evaluated end-to-end and deliberately left DISABLED (`confhead_enabled=false`).** On the current local classify/triage workload the small E2B tier is ~98–100% accurate, so there are almost no "should-escalate" negatives, and a label-validity probe found the self-agreement label (E2B vs the larger local tier) is ~77% backwards on disagreements (the larger tier is the *less* accurate one here). The adoption gates correctly returned REJECT. The plumbing is built, reviewed, and ready for a workload where escalation actually pays off (e.g. cloud-vs-local quota routing); it changes no default behavior today.

## [0.4.1] — 2026-06-28

### Fixed
- **The shadow-labeling flywheel now actually manufactures counterfactual labels.** Two compounding bugs had left it producing ~0 labels:
  - **Config silently ignored by the MCP server.** A bare `local-offload mcp` (host passes neither `--config` nor `$LOCAL_OFFLOAD_CONFIG`) ran on built-in defaults with shadow capture **off**. `loadCfg` now also auto-discovers `~/.local-offload/config.json` when both are unset (new `resolveCfgPath`; precedence: flag → env → conventional path → defaults).
  - **Healthy entry tiers were route-skipped.** `internal/health` flagged tiers DEGRADED on margin/throughput **drift** (CUSUM/Page-Hinkley) or throughput collapse, and the cascade routed *around* any DEGRADED tier — so an accurate small entry tier that was merely non-stationary (single-GPU throughput variance) got skipped to a larger, slower one, starving the flywheel of entry-tier data. Health now separates a `route_skip` signal (true only on a genuine **quality collapse** — confidence margin far below the tier's own baseline) from the observability `Status` (any drift/throughput anomaly); only `route_skip` populates the routing skip-list. Drift/throughput remain visible for timeout tuning.
- The CLI `version` string now matches the `VERSION` file (was a stale `0.1.0`).

## [0.4.0] — 2026-06-28

### Added
- First public release. 0.4.0 reflects the already-mature harness (core text offload + full self-learning cascade + flywheel + kNN + vision/STT/video understanding + image & SVG generation); media generation, DaVinci editing, and the capstone remain on the roadmap.
- Text offload tools — **summarize, classify, extract, triage** — on a free local Gemma-4 cascade via llama.cpp; never calls a cloud model (returns a structured **defer** on low confidence).
- **MCP server** (stdio) exposing 12 tools, plus a Go CLI.
- **Vision**: VQA, OCR, image field-extraction, and render QA (`assess-image`).
- **Speech-to-text** via whisper.cpp (`transcribe`) and **video understanding** (`video-describe`).
- **Image generation** (SDXL via ComfyUI) and a dependency-free **SVG component kit** (gauge / comparison-bar / chromatogram / icons).
- **Self-learning cascade**: confidence-gated escalation, conformal thresholds, a logistic entry-tier router, health/circuit-breakers, few-shot exemplars, and an offline shadow-labeling flywheel — all inference-free over the token ledger.
- Append-only JSONL **token-savings ledger**.
