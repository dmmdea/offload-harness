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

## Cascade

The ordered set of model Tiers an offload task walks, entering at the smallest capable tier and
escalating only when a result fails validation or lands below a confidence threshold. Exhausting the
Cascade produces a Defer rather than an error. The Cascade never calls a remote model.

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

## Ledger

The append-only JSONL record of offload calls and their savings, `fsync`ed per entry. Carries
`tokens_saved` — input tokens kept out of the calling model's context — plus the metadata that
explains each outcome. The recordless path writes nothing to it.

## Mirror

The public repository (`dmmdea/offload-harness`), updated by squash-publishing from the private
canonical repository. Its history is release snapshots, not a replay of development. See
[architecture/decisions/0006-private-canonical-public-squash-mirror.md](architecture/decisions/0006-private-canonical-public-squash-mirror.md).

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

## Policy broker

The single gate for effectful agent actions — write, overwrite, delete, fetch, shell. Distinct from
the loop's step and tool-call budgets, which are a separate mechanism. See
[architecture/decisions/0003-policy-broker-and-capability-flags-off-by-default.md](architecture/decisions/0003-policy-broker-and-capability-flags-off-by-default.md).

## Profile

Two unrelated meanings, distinguished by context:

- **Hardware profile** — a machine class (`ampere-8`, `blackwell-48`, `cpu`, …) chosen by
  `detect.ps1`, selecting a serving template and a Config seed.
- **Agent profile** — a named narrowing of the coding agent's toolset plus a tuned prompt
  (`general`, `edit`, `build`, `research`, `github`). An agent profile can only narrow, never widen.

## Recordless path

The pipeline construction the coding agent uses: nil cache, nil ledger, no shadow capture, no
escalation. It exists so an agent's internal offload calls leave no trace in savings accounting.

## Run-graph

The generic primitive that executes a caller-supplied ComfyUI graph against a Node Manifest,
returning node-addressed outputs. It is the boundary that lets a workflow repository own graph
authoring while the harness owns execution.

## Squash-publish

Publishing to the Mirror as a single squashed snapshot rather than replaying private commits. Keeps
the publishable surface small enough to review for the privacy rules in [STYLE.md](STYLE.md).

## Tier

One model seat in the Cascade, referred to by a stable alias (`gemma4-e2b`, `offload-e4b`,
`gemma4-26b-a4b`, …) rather than by a file or vendor. Configuration binds a role to an alias; the
serving layer binds the alias to actual weights, so the same config works across backends.

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

The default GPU posture: nothing GPU-resident persists between media jobs. The card is cleared before
a render and returned afterward, so text inference remains usable. See
[flows/zero-warm-generation.md](flows/zero-warm-generation.md).
