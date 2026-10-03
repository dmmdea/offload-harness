# Glossary

Terms that carry a specific meaning in this repository. Where a term also has an ordinary English
sense, the entry says what makes the local meaning narrower — a Defer is a specific structured
result, not just any deferral.

Finished documentation uses Title Case for these terms when it improves clarity. Drafts, comments,
source identifiers, and informal notes need not.

## Ack

A fleet node's `202` response accepting a dispatched job. It means "this job is mine now", not "this
job is finished". Duplicate dispatches of a non-failed job re-ack rather than starting a second run —
see [flows/fleet-job-lifecycle.md](flows/fleet-job-lifecycle.md).

## Backlog gate

The delegator's rule that holds a fleet node out of a placement when a new job would wait longer to START
there than the caller will wait (`startsWithinPatience`): the node's own `queue_wait_estimate_sec`, or the
arithmetic over its jobs and recent wall, against the contract's poll budget. A placement feasibility
refusal that prints its arithmetic, in the same class as the wall check in `feasibleFinal`, never a
preference for a faster seat; a held-out node is read again every tick and never refused for good. The same
ETA sizes a job's queue budget. See [ADR 0063](architecture/decisions/0063-placement-holds-instead-of-sleeping-or-refusing.md).

## Browse lane

The opt-in lane that drives the operator's own running, logged-in Chromium browser toward a
natural-language goal: the MCP tool `offload_browse` and the agent tool `browse` (ADR 0060). A pinned
Python sidecar owns the browser; the harness owns every model call and reaches only a loopback
decision endpoint, which may front a hosted model. Off by default; agent doors are unattended and
local-only. Not a headless scraper: it uses the operator's real session. See
[systems/browse-lane.md](systems/browse-lane.md).

## Capacity wait

The delegator's queue (`agent_placement_wait_sec`, 120 s by default): a subtask every node that could run it
refused for capacity, or whose only placements are nodes that are merely busy, a reserved local seat or a full
local run-cap line, waits here, re-reading the
fleet's health every few seconds, and lands on the first node that has room. The time it idles is credited,
never charged to the contract's `timeout_sec`; a node inside its `Retry-After` cooldown or held out by the
backlog gate is skipped. See [ADR 0063](architecture/decisions/0063-placement-holds-instead-of-sleeping-or-refusing.md).

## Cascade

The ordered set of model Tiers an offload task walks, entering at the smallest capable tier and
escalating only when a result fails validation or lands below a confidence threshold. Exhausting the
Cascade produces a Defer rather than an error. The Cascade never calls a remote model.

## Composition

An HTML/CSS page that HyperFrames renders to video frame by frame: a root element carrying
`data-composition-id`, `data-width`, `data-height`, `data-fps` and `data-duration`, with its
animation seeked rather than played, so the same inputs give the same frames. The
`offload_compose_video` lane renders one, CPU-class and with no GPU lock (ADR 0059). A Composition
is code that runs in an unsandboxed Chrome, so the fleet door renders only the vetted templates under
`render/compose-templates/`: `title-card`, `lower-third`, `stat-card`, `section-title`,
`callout-label`, `checklist-card` and `captions-bar`. Not a ComfyUI graph, and not generation: nothing
is sampled.

## Config seed

The default model bindings a hardware Profile supplies at install time — which image checkpoint,
which video experts, at which quantization. Distinct from the serving template, which supplies the
flags.

## Defer

A structured, successful result meaning "this harness declined to answer; do it yourself" — shaped
`{"deferred": true, "reason": ...}`. A Defer is a valid outcome, not a failure, and never triggers a
cloud fallback.

Text-cascade defers carry a free-form `reason` plus an `err_class` for infrastructure failures.
Run-graph defers are **typed**, carrying a machine-readable `code`, a `ref` identifying the offending
item, and a `detail`. See
[architecture/decisions/0001-defer-never-cloud-fallback.md](architecture/decisions/0001-defer-never-cloud-fallback.md).

## Deny-list

The word-bounded, case-insensitive list of control labels (publish, send, post, delete, pay, buy,
checkout, subscribe, confirm, sign out, ...) the Browse lane never offers to the model and rechecks at
execution, so a run ends `denied` rather than clicking one. `allow_labels` lifts it for exact labels on
the attended MCP door only. Distinct from a policy-broker deny rule, which gates an agent action by
kind and subject.

## Effect record

The coding agent's per-tool-call execution accounting: `committed`, `failed`, `unknown` (the loop
stopped waiting — effects genuinely unknown), or `none` (never executed: refusal, denial, Park).
Distinct from the Ledger, which counts token savings — the effect record answers "what did this
call do to the world?". See [systems/coding-agent.md](systems/coding-agent.md).

## Environment rule

One entry of a seat's `agent_env_rules` table (ADR 0036): a closed-vocabulary, validated constraint the
coding-agent loop applies around every tool call — withhold a tool, cap its executions, clamp a numeric
argument, bound or strip what the model reads, rewrite an error into a line it can act on. Data, never
generated code; a property of the seat, not of the task. Distinct from a **risk rule** (`--rules`), which
gates what an effectful action may do to the world. The per-call record of what ran and which rule
decided is the result's `trace`.

## Escalation

Moving a task to the next, larger Tier after a recoverable failure — a schema violation, ungrounded
extraction, or low confidence. Infrastructure failures deliberately do **not** escalate, since a
larger model against a broken endpoint fails identically.

## Fleet contract

The HTTP interface a node exposes to a compute-fleet dispatcher: health, dispatch, and job polling.
Published and versioned; the node implements it, the dispatcher consumes it.

## Footprint

A measured VRAM cost for a model family and task, advertised so a dispatcher can place work. Derived
from observed peaks with a 1.2 padding factor and kept at the maximum observation rather than
averaged. See
[architecture/decisions/0008-pdh-primary-vram-sampling.md](architecture/decisions/0008-pdh-primary-vram-sampling.md).

## GPU Lock

A single-slot, cross-process lock ensuring only one GPU-heavy job runs per machine. Implemented as a
directory because `mkdir` is atomic everywhere. A lock whose owner is dead is reclaimed immediately.

## Grounding

Checking that values in a model's output actually appear in its input. Computed and logged for all
tasks, but *actioned* only for extraction — summaries legitimately paraphrase, so gating them on
grounding would be noise.
Text is matched as a phrase; numbers are compared by value across locales (`2.354,40` and `2,354.40`
are one amount), never as substrings of other numbers.

## Leak gate

The test that keeps the names of the operator's machines, people and brands out of this public repository.
It scans every tracked file and every tracked file name against a list of denied names. The list is a
secret, so the repository holds only keyed hashes of it (`testdata/leak-gate-digests.json`) and the key lives
outside the repository: an Actions secret in CI, a key file on a maintainer's machine. Without the key the
keyed test skips visibly, unless the run requires it (a push, or a pull request from this repository). Not
to be confused with the older shape-only `TestTrackedTreeCarriesNoOperatorIdentity`, which names no value.
See [systems/leak-gate.md](systems/leak-gate.md).

## Inner row

A ledger row that is a step of a call, not the call: it carries `parent_job_id`, the `job_id` of the
call's own row (register C-62, extended to every call that writes several rows: a `video_watch`
window, an escalating cascade attempt, an `extract_image` sub-call). Job counters count the call's own
row and skip its inner rows, PAIR cards only the call's own row, and an orphan inner row (the call's
own row never landed) still counts as a call. See
[systems/pair-workloads.md](systems/pair-workloads.md).

## Ledger

The append-only JSONL record of offload calls and their savings, `fsync`ed per entry. Carries
`tokens_saved` — input tokens kept out of the calling model's context — plus the metadata that
explains each outcome. The recordless path writes nothing to it. Not to be confused with the coding
agent's per-call Effect record ("the effect ledger"), which accounts for what tool calls did, not
what tokens they saved.

## Mirror

Historical term. Under the earlier repository model the public repository was a squash-published
mirror of a private canonical one. That was **inverted** on 2026-07-18 — the public repository
(`dmmdea/offload-harness`) is now canonical, and the private repository is the development and moat
repository. See
[architecture/decisions/0012-public-canonical-repository.md](architecture/decisions/0012-public-canonical-repository.md)
(which supersedes 0006).

## Model Affinity Gate

The in-process admission gate that keeps one llama-swap serving slot on one model at a time. Requests
naming the same model on the same base run concurrently; a request naming a different model parks
until the in-flight batch drains, so N interleaved model switches become one switch per batch. Keyed
on the **resolved base URL** — two models served by two llama-swap instances do not contend — and
taken by both the cascade lane (`internal/llamaclient`) and the agent seat
(`internal/agent.LLMClient.Chat`). It is process-local: two harness processes on one box still
contend. Distinct from the GPU Lock, which arbitrates whole GPU-heavy jobs across processes — but not
independent of it: an admission that would make llama-swap LOAD a model (an idle base, a promoted
switch) waits out a `media` lease holder, while one joining the resident model's in-flight batch is
not gated at all. The gate reads that lease, never acquires it. See
[architecture/decisions/0025-model-residency-is-arbitrated-in-process-by-base.md](architecture/decisions/0025-model-residency-is-arbitrated-in-process-by-base.md)
and [architecture/decisions/0026-text-load-admissions-wait-for-the-media-lease.md](architecture/decisions/0026-text-load-admissions-wait-for-the-media-lease.md).

## Named family

An opt-in media binding beside a node's default one (`imagegen_families` / `gen_edit_families`),
selected per request by `family`. Narrower than "model family": it is a whole binding (script,
engine, model files, launch keys) that must declare `license` and `commercial_use`, and every result
it produces carries its license. The only way a non-commercial model ships in this repository — see
[ADR 0058](architecture/decisions/0058-non-commercial-model-families-ship-only-as-named-license-tagged-opt-ins.md).

## Node Manifest

The declaration accompanying a run-graph request: which ComfyUI custom node packs are required (at
pinned commits) and which model files must be present (with optional hashes). The harness satisfies
it before executing the graph, or defers explaining what it could not satisfy.

## Node Pack

A ComfyUI custom-node repository, pinned to an exact commit in a Node Manifest. Packs supply the node
classes a graph references.

## Op

One image-editing operation inside the `edit-image` verb — the set is `crop`, `resize`, `convert`,
`composite`, `text`, `mask_boxes`, `grade`, `lut_cube`, `perspective_composite`, `finish`,
`flatten_design`, `instantiate_design`. Ops are list items, not separate commands. `finish` should
come last by convention, but the validator does not enforce ordering.

## Park

The unattended-run refusal of an effectful agent tool call that the model itself flagged
`security_risk: high` (or with an unrecognized value — fail closed). A parked call is never
executed: it records `none` on the Effect record, lands durably in the ask queue for operator
review, and does not consume the loop's same-name tool budget.

It is a **model self-report, not a boundary**: measured on the production agent seat, the
annotation was a constant `low` across 54/54 emitted declarations — including all 36 structurally
destructive calls — so park-gate recall was 0%. The structural gate is the risk-rule table
(`--rules`); parking is the fail-safe residue around it. See
[systems/coding-agent.md](systems/coding-agent.md).

## Policy broker

The single gate for effectful agent actions — write, overwrite, delete, fetch, shell. Its classify
step includes a structural, tighten-only risk-rule table (rules may deny or ask, never allow, with
a built-in secret-material floor). Distinct from the loop's step and tool-call budgets, which are a
separate mechanism. See
[architecture/decisions/0003-policy-broker-and-capability-flags-off-by-default.md](architecture/decisions/0003-policy-broker-and-capability-flags-off-by-default.md).

## Process gate

The delegator's process-wide count, per fleet node, of the dispatches this process holds open across every
concurrent Run (`internal/delegate/processgate.go`). A dispatch that would take a node past its published
`max_queue_depth` is not sent: the subtask waits in the capacity wait for the first node that frees. It
counts a job until a terminal answer or until the delegator gives up on it. See
[ADR 0063](architecture/decisions/0063-placement-holds-instead-of-sleeping-or-refusing.md).

## Profile

Two unrelated meanings, distinguished by context:

- **Hardware profile** — a machine class (`ampere-8`, `blackwell-48`, `cpu`, …) chosen by
  `detect.ps1`, selecting a serving template and a Config seed.
- **Agent profile** — a named narrowing of the coding agent's toolset plus a tuned prompt
  (`general`, `edit`, `build`, `research`, `github`). An agent profile can only narrow, never widen.
- **Cache server** — the optional second-device KV tier for a vLLM seat: a store (Valkey or a
  filesystem export) on another machine's RAM behind LMCache MP's L1 staging buffer, declared by
  `kv_cache_server`, off by default, scored on capacity at parity cost (ADR 0033).

## Recordless path

The pipeline construction the coding agent uses: nil cache, nil ledger, no shadow capture, no
escalation. It exists so an agent's internal offload calls leave no trace in savings accounting.

## Reference box

The machine whose measurements a hardware tier's values come from: the seed values in
`setup/templates/profiles.json`, the figures on the tier's page and its capability report were measured on
it. The word names a different machine for each tier, so it is never used bare: write "the `<tier>`
reference box" with a tier id, or use the node letter ([STYLE.md](STYLE.md#privacy), rule V4).

| tier | its reference box |
|---|---|
| `ampere-8` | `<node-a>`, the RTX 3070 laptop (8 GB) |
| `blackwell-16`, `blackwell-2x16`, `blackwell-3x16` | `<node-b>`, the workstation, in three eras: one RTX 5060 Ti until 2026-08-02 (`blackwell-16`, now historical), two cards until 2026-08-31 (`blackwell-2x16`), three cards since (`blackwell-3x16`) |
| `ampere-6`, `ampere-16` | `<node-c>`, the Linux edge node: an RTX 3050 6 GB until 2026-09-04 (`ampere-6`, now historical), an NVIDIA A2 16 GB since (`ampere-16`) |
| `blackwell-8` | `<node-e>`, the compact desktop (RTX 5060 8 GB), since 2026-08-19 |
| `rockchip-rk3588` | `<node-d>`, the RK3588 board |
| `amd-gcn` | `<node-f>`, the AMD APU mini-PC (Ryzen 5 5625U, Vega 7) |
| any other tier | none: no machine of that class is in the fleet, so its figures are projected or contributed rather than measured on a fleet machine |

A dated document keeps the era of its own date (rule V3): a sentence from August 2026 about the workstation
names the tier the workstation was then, not the one it is now.

## Rigger

The seat rigger (ADR 0036 P3a, 0.113.26): `local-offload rig` / MCP `agent_rig`, a deterministic classifier over
the delegation-log corpus that puts every failed or deferred row of a seat on exactly one failure axis in a
published precedence order and reports weights over the rows eligible for each axis, evidence job ids and the
pre-authored remedy where the closed rule vocabulary has a lever — or "not a rule matter" where it has none. It
proposes nothing on its own and applies nothing; the envharness objectives (difficulty-zone, red-team) and a
validated proposer are P3b, after the trace corpus grows.

## Run-graph

The generic primitive that executes a caller-supplied ComfyUI graph against a Node Manifest,
returning node-addressed outputs. It is the boundary that lets a workflow repository own graph
authoring while the harness owns execution.

## Seat guard

The cascade's rule that a Tier-1 rung never evicts a loaded vLLM seat (`internal/seatguard`,
`cascade_seat_guard`, on by default). While a seat named in `vllm_seats` is loaded, a rung whose
load llama-swap's own routing (`serving_config_path`) says would unload it rides its cascade lane
(the same model elsewhere). Otherwise the loaded seat serves as the rung. With no vLLM seat
loaded it changes nothing. See [systems/offload-pipeline.md](systems/offload-pipeline.md).

## Setup action

One entry of a contract's `setup_actions` (ADR 0036 P2, 0.113.24): a `{tool, args}` call the coding-agent
loop replays before the model's first turn, through the seat's env rules and dispatch, landing in the
transcript as a tool call plus its result — so the first turn already holds what the model would have
spent its first steps fetching (a `read_file` of the document it must digest). Charged to the wall, never
to `max_steps`; bounded to half the compaction budget; a failure is an observation, not an abort. A node
with `agent_seed_context_reads` on prepends one read per context doc itself. Reported as `setup_ran`
and as step-0 trace entries marked `setup`.

## Squash-publish

Historical term, retired with the Mirror model on 2026-07-18. It named the act of publishing to the
old public mirror as a single squashed snapshot. With the public repository now canonical
(ADR 0012), development happens there directly and there is nothing to publish. Retained here only so
older references resolve.

## Tier

Two different things wear this word, and the difference decides where to look. **Model tier**
(this entry) is one model seat in the Cascade. **Hardware tier** is the install profile a machine
classifies as (`blackwell-2x16`, `ampere-8`) — see Hardware Tier below.

One model seat in the Cascade, referred to by a stable alias (`gemma4-e2b`, `offload-e4b`,
`gemma4-26b-a4b`, …) rather than by a file or vendor. Configuration binds a role to an alias; the
serving layer binds the alias to actual weights, so the same config works across backends.

## Hardware Tier

The hardware class a machine installs as (`blackwell-2x16`, `ampere-8`, `cpu`), declared in
`setup/templates/profiles.json`. It decides the served window, the KV type, the serving template,
the resident model and the media seats. One page per tier lives in `docs/tiers/`. Distinct from
the Cascade's model Tiers above.

## Composite Tier

A hardware tier that COMPOSES others: the box is a complete instance of each at once, declares
them in `composes`, and routes work across device Layers. See
[systems/composite-tier.md](systems/composite-tier.md) and ADR 0052.

## Launch profile

The ComfyUI launch flags one media binding needs — `comfy_cuda_device` (`--cuda-device`, in
ComfyUI's device order), `comfy_dynamic_vram` and `comfy_extra_args` — handed to the render runner as
env. The device pin applies to single-card routes only, never to a pooled seat or run-graph, and a
running ComfyUI whose argv contradicts the profile is never reused silently
(`COMFY-PROFILE-MISMATCH`). See [systems/media-generation.md](systems/media-generation.md).

## Layer

One device set of a composite box with its own seats, guards and tier identity (`single`,
`pair`, `display`). Placement chooses a layer and a seat per task; a layer marked `dormant` is
declared but never routed to until the operator enables it.

## Extra vLLM seat

A vLLM seat a tier serves on demand beside its agent-lane seat (`vllm_seat`), declared in the tier's
`extra_vllm_seats`: the same card, never the agent lane, reached by name through the Layer that names it
(the `ampere-16` fast layer's 35B). Every vLLM seat of a tier renders as an alternative of the others, and
the box seeds a seat, its roster entry, its cache-server binding and its layer only while it can run it.
See [systems/composite-tier.md](systems/composite-tier.md) and ADR 0048 Amendment 2.

## Placed

The placement block every result carries on a composite box: which tier, layer, role, seat and
devices ran the work, the reason, and — on a refusal — the guard that said no. Absent on a box
with no layers. Not to be confused with `results[].placement`, the delegation lane's free-text
string, which is unchanged.

## Two-tier

The coding agent mode where an architect model plans and an editor model executes, with one model
swap. The architect gets read and search only.

## Warm Batch

The opt-in `generate-image --batch` session where the checkpoint loads once for N renders. Teardown
still happens exactly once, at the batch boundary — Zero-Warm moves from per-render to per-batch.

## Worktree

The directory the coding agent's writes are confined to, enforced via `os.Root`. The audit trail must
live outside it.

## Zero-Warm

The default GPU posture: nothing GPU-resident persists between media jobs, except the memory stack (the mem0 embedder and reranker stay resident). The card is cleared before
a render and returned afterward, so text inference remains usable. See
[flows/zero-warm-generation.md](flows/zero-warm-generation.md).
