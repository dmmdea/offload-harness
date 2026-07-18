# Architecture Decision Records

Proposed, active, and historical technical decisions — their context, tradeoffs, consequences, and
rejected alternatives.

**Only ADRs with `Accepted` status are current guidance.** When reviewing, planning, or changing
code, treat everything else as background.

## Index

| ADR | Status | Title |
|---|---|---|
| [0001](0001-defer-never-cloud-fallback.md) | Accepted | Defer instead of falling back to a cloud model |
| [0002](0002-grammar-reliable-serving-flags.md) | Accepted | Grammar-constrained output via a raw GBNF field, not a schema parameter |
| [0003](0003-policy-broker-and-capability-flags-off-by-default.md) | Accepted | Single policy broker; capability flags off by default |
| [0004](0004-worktree-confinement-audit-outside.md) | Accepted | Worktree confinement with the audit trail stored outside it |
| [0005](0005-loopback-only-serve.md) | Accepted | Loopback-only serving unless explicitly opted out |
| [0006](0006-private-canonical-public-squash-mirror.md) | Accepted | Private canonical repository with a public squash-published mirror |
| [0007](0007-host-torch-pinned-additive-provisioning.md) | Accepted | Pinned-additive provisioning that never moves host torch |
| [0008](0008-pdh-primary-vram-sampling.md) | Accepted | Per-process PDH counters as the primary footprint source |
| [0009](0009-zero-warm-gpu-lifecycle.md) | Accepted | Zero-warm GPU lifecycle for media generation |
| [0010](0010-tier-optimization-before-latency-defer.md) | Accepted | Fix the tier binding before adding latency-based defers |
| [0011](0011-flux-family-license-prohibition.md) | Accepted | FLUX-family models are prohibited |


## Lifecycle

An ADR starts as `Proposed` while the decision is under discussion, and becomes `Accepted` once
approved. A decision that later changes does **not** get rewritten: write a new ADR, set the old one
to `Superseded`, and link it forward with `superseded_by`. Accepted ADRs may still receive small
corrections and added links that do not change the recorded decision.

Statuses:

- `Proposed` — under discussion, not current guidance.
- `Accepted` — the current decision.
- `Superseded` — replaced by a newer ADR (must carry `superseded_by`).
- `Deprecated` — discouraged, still historically relevant.
- `Rejected` — considered and intentionally not adopted.

## Ownership

Architectural decisions are human-owned. Agents may draft ADR text from decisions that have already
been made, and keep existing ADRs aligned with the code — but an agent does not decide architecture,
and does not move an ADR to `Accepted` on its own.

## Writing a new one

Copy [../../templates/adr.md](../../templates/adr.md), name it `NNNN-<slug>.md` with the next free
number, and add a row to the index above. The frontmatter schema is fixed — see
[../../STYLE.md](../../STYLE.md).
