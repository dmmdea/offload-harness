---
status: Accepted
date: "2026-08-22"
---

# Accelerators are additive to the GPU tier

Decision provenance: decided by the operator 2026-08-22 (hailo-8l accelerator plan); this ADR
records those already-made decisions per the ownership rule in the index README.

## Context

The harness classified a box into exactly **one** `profile` string, and that string traveled
end-to-end: detection picked it, the installer rendered serving from it, `installed.json` and
`/fleet/health` advertised it, and every tier-id consumer keyed on it. A second compute device
had no representation anywhere in that pipeline — a box with an NPU beside its GPU was, as far
as the harness could say, just its GPU tier.

Vision work had the same single-lane shape: every vision-flavored task routed to llama-swap by
model id (the tier's VLM seat), so capabilities a small NPU serves natively and fast — face
detection, identity embeddings, object detection, depth, image embeddings — either ran as
prompt-shaped VLM approximations or were not offered at all.

The concrete device forcing the question: a Hailo-8L M.2 NPU on an OptiPlex 7060, with a
working HTTP inference sidecar in its own repository (Hailo-8L-Analysis-Pipelines).

## Decision

### 1. Accelerators are ADDITIVE — `profile` stays one string

`profile` remains the single GPU tier id. An accelerator never changes it; instead an
`accelerators: []` list rides beside it in `installed.json`, in `hwdetect.Verdict`, and in the
harness config (`config.Accelerators`). An empty list serializes to nothing (`omitempty`
everywhere), so a box with no accelerator is byte-identical to one from before the field
existed — in its manifest, its health payload, and its `tools/list`.

### 2. Each accelerator declares the capabilities it OWNS exclusively

`setup/templates/profiles.json` gains an `accelerators` map beside `profiles`. Each entry
declares its `kind`, its detection rule, its `config_seed`, and `owns` — the capability list
that accelerator serves **exclusively** when present. Ownership when both devices are present:

- **NPU owns structured vision outputs** — face detect and identity embeddings, object
  detection, person re-identification, depth, low-light enhancement, image embeddings: results
  that are boxes, vectors, and maps.
- **GPU owns language about images** — VQA and description stay on the tier's VLM, untouched.
- **OCR is GPU-primary** with an explicit caller-selected `engine:"npu"` fast path — never an
  automatic fallback, because the two engines read stylised text differently and a silent
  switch would change results.

### 3. Runtime is an on-demand loopback sidecar, spawned by the harness, self-exiting idle

The accelerator's runtime is an HTTP sidecar on loopback (`127.0.0.1:18813`) that the harness
spawns on the first NPU call (`internal/hailoclient.Sidecar.Ensure`) and that exits itself
after a configured idle window. No scheduled task, no always-on service: a clean box stays
clean, and the device costs nothing while unused. Wire contract: `GET /health` → status dict
(200); `POST /v1/<tool>` → the tool's dict (200; a `{"error":true,...}` dict is a structured
result, not a transport error); 404 `unknown_tool`; 400 `bad_request`.

## Alternatives considered

- **Composite tier ids** (e.g. `blackwell-16+hailo-8l`) — rejected: every consumer of the tier
  id (templates, matrix, tier docs, fleet placement, capability reports) would break or need
  parsing rules; the additive list costs none of them anything.
- **A second scalar** (`npu_profile` beside `profile`) — rejected: it hard-codes exactly one
  accelerator per box and one device class, re-creating today's problem one device later. A
  list of declared ids generalizes for free.
- **An always-on sidecar service** (scheduled task / service manager) — rejected: a scheduler
  on a clean box, idle RAM cost, and a visible operational surface for a device used in
  bursts. On-demand spawn plus idle self-exit gives the same availability one cold start
  (~2 s + HEF load) later.

## Consequences

- `tools/list` grows only on boxes that list the device — registration is gated on
  `config.HasAccelerator`, so every other box's MCP surface is byte-identical.
- Tier docs and templates are untouched: no tier page changes, no serving template changes,
  and `docs/tiers/` needs no regeneration for an accelerator.
- The hardware/tier matrix gains an **Accelerators** sheet (declared in `profiles.json`,
  mirrored in the Arquitechture xlsx per the matrix-first house rule).
- One sidecar process serialises NPU access: single in-flight inference by construction, which
  is correct for the device (one PCIe queue) and free of locking code.
- A second accelerator later is a `profiles.json` entry, a detection probe, and its own client
  — no schema change anywhere in the pipeline.

## Related code

- [`internal/config/config.go`](../../../internal/config/config.go) — `Accelerators`,
  `HasAccelerator`, the `hailo_*` keys
- [`internal/hwdetect/classify.go`](../../../internal/hwdetect/classify.go) —
  `Verdict.Accelerators`, `DetectAccelerators`, `AcceleratorsFromHailortcli`
- [`internal/tierseed/tierseed.go`](../../../internal/tierseed/tierseed.go) —
  `ResolveAccelerators`, `__HAILO_HOME__`
- [`internal/hailoclient/`](../../../internal/hailoclient/hailoclient.go) — client, sidecar
  spawn/ensure
- [`internal/mcpserver/mcpserver.go`](../../../internal/mcpserver/mcpserver.go) — gated tool
  registration, `offload_ocr` engine switch, status block
- [`setup/templates/profiles.json`](../../../setup/templates/profiles.json) — the
  `accelerators` map
- [`setup/detect.ps1`](../../../setup/detect.ps1), [`setup/install.ps1`](../../../setup/install.ps1)
  — `Get-Accelerators`, `Get-AcceleratorSeed`, manifest

## Related docs

- [systems/accelerators.md](../../systems/accelerators.md) — the system doc
- [ADR 0001](0001-defer-never-cloud-fallback.md) — the never-cloud rule the sidecar lane
  honors (loopback, free, local)
- [ADR 0005](0005-loopback-only-serve.md) — the loopback posture the sidecar inherits
