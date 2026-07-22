---
status: Accepted
date: "2026-07-18"
---

# Pinned-additive provisioning that never moves host torch

## Context

The run-graph primitive accepts a Node Manifest naming the ComfyUI custom node packs a graph needs,
pinned to exact commits, and installs them into the ComfyUI virtual environment before running the
graph.

Custom node packs declare their own Python dependencies, and those declarations are not written with
our environment in mind. During the scene-swap bring-up, a pack's dependency resolution wanted to move
`torch` from `2.11.0+cu128` to `2.13.0` — which would have replaced ComfyUI's CUDA-linked build with
a generic one and broken GPU rendering for every workflow on the machine, not just the one being
provisioned.

There is a second, subtler problem. Locally-built CUDA wheels carry a local version suffix
(`+cu128`) and live on the PyTorch index, not PyPI. A resolver handed `torch==2.11.0+cu128` as a
constraint reports that no such version exists, so the naive fix — constrain to exactly what is
installed — fails outright.

## Decision

Provisioning is **pinned-additive**: it may add packages, never move the host's foundational ones.

Four packages are protected: `torch`, `torchvision`, `torchaudio`, `numpy`.

The mechanism has three parts:

1. **Constraints captured from reality.** `buildHostConstraints` reads `pip freeze` and extracts the
   `==` pins for the protected packages, preserving the local version suffix.

2. **`publicPin` strips the local suffix for resolvers.** It rewrites `2.11.0+cu128` to `2.11.0`
   before handing constraints to `uv`/`pip`. Per PEP 440 the installed `+cu128` build satisfies a
   public `==2.11.0` pin, so the resolver neither reinstalls nor upgrades it — the constraint binds
   without being unsatisfiable. These stripped pins are exported as `PIP_CONSTRAINT` and
   `UV_CONSTRAINT` for both the resolve and install steps.

3. **A tripwire verifies the outcome with full pins.** After `pip check` passes, the satisfier
   re-reads `pip freeze`, rebuilds the constraint set with the **complete** local-version pins, and
   compares. Drift means something moved torch despite the constraints, and provisioning fails rather
   than proceeding on a broken environment.

**`uv` is driven directly** rather than through `cm-cli`, because the installed `cm-cli` has no
`--uv` flag. `uv` resolves; `python -m pip` installs. Packs are cloned at their pinned commits
*before* the resolve, because the unified `uv pip compile` reads their on-disk `requirements.txt`.

Failures are typed defers, not crashes — `VENV_INCOHERENT`, `SATISFIER_UNAVAILABLE`,
`SATISFIER_SPAWN_FAILED` (v0.22.13: a subprocess that failed to *start*, retried once, classified
apart from a venv problem), `COMFY_VERSION_BELOW_MIN`, and the model-leg codes.

## Consequences

- A workflow can bring its own node packs without any risk of breaking rendering for every other
  workflow on the box.
- Provisioning can fail. That is the intended trade: a typed defer beats a silently degraded
  environment, and the calling workflow layer handles the defer.
- Adding a fifth protected package means editing one list.
- **Diagnosability (closed):** the original gap — drift and ordinary conflicts both reading
  `conflicting installed dependencies`, with the real diagnostic only on stderr — was closed in
  v0.22.5 (the defer detail names the drifted pin or pip's own message). v0.22.13 closed the
  remaining mislabel: a subprocess that failed to *spawn* now defers `SATISFIER_SPAWN_FAILED`
  instead of `VENV_INCOHERENT`.

## Alternatives considered

- **Let packs resolve freely.** Rejected outright — this is the failure that prompted the decision.
- **A separate virtual environment per pack.** Rejected: ComfyUI loads custom nodes in-process, so
  they must share one interpreter.
- **Constrain with the full local pin (`==2.11.0+cu128`).** Rejected because it does not work —
  resolvers cannot find local-version builds on PyPI. This is precisely why `publicPin` exists.
- **Trusting the constraints without a tripwire.** Rejected: constraints are a request to a resolver,
  not a guarantee. The tripwire checks what actually happened.

## Related code

- [`render/manifest-satisfy.mjs`](../../../render/manifest-satisfy.mjs) — `buildHostConstraints`,
  `publicPin`, `hostPins`, `pipCheck` tripwire
- [`render/manifest.mjs`](../../../render/manifest.mjs) — manifest parsing and hashing
- [`render/comfy-run-graph.mjs`](../../../render/comfy-run-graph.mjs) — provisioning before start
- [`internal/rungraph/`](../../../internal/rungraph/) — the Go side of the envelope

## Related docs

- [../../flows/run-graph-manifest-satisfaction.md](../../flows/run-graph-manifest-satisfaction.md)
- [0001-defer-never-cloud-fallback.md](0001-defer-never-cloud-fallback.md) — typed defers as valid
  outcomes
