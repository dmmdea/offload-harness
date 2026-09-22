---
status: Proposed
date: "2026-09-22"
---

# ADR 0058 — Non-commercial model families ship only as named, license-tagged opt-ins

## Context

[ADR 0011](0011-flux-family-license-prohibition.md) keeps FLUX out of this repository because its
weights are non-commercial. It rejected "allow it for non-commercial or internal use only" for one
reason: **the harness cannot tell which of its outputs will end up in commercial work.** With one
image binding per node, that was true. A request had no way to say "this render is for evaluation",
and a result had no field saying which license produced it.

Qwen-Image-2.1 (released 2026-09-20) is the case that makes this matter. It is a 7B DiT with a
Qwen3-VL-8B encoder and an RGBA VAE. It adds capabilities the fleet lacks: native transparency, up
to 10 edit references, native 2K and strong typography. It is licensed under the **Qwen Research
License Agreement**, quoted verbatim from the model repository's `LICENSE`:

- §1.i: *"'Non-Commercial' shall mean for research or evaluation purposes only."*
- §2.a: *"...to use, reproduce, distribute, copy, create derivative works of, and make modifications
  to the Materials FOR NON-COMMERCIAL PURPOSES ONLY."*
- §2.b: *"You shall not use the Materials for any commercial purpose without obtaining a separate
  commercial license from us."*

The license covers the whole ecosystem around the model: the ComfyUI repack, the GGUF quants and
the prompt-enhancer models. Its predecessors (Qwen-Image 2512 and Qwen-Image-Edit 2511, the edit seat
the tiers seed) are Apache-2.0.

On 2026-09-22 the operator decided that 2.1 should be available as an **opt-in, license-tagged**
family on the nodes where it fits: never a default and never a seed default, with every result
carrying its license.

## Decision

A model whose license does not permit commercial use may be bound in this repository **only as a
named family overlay**. It is never the default binding and never a tier's seed default. Every result
it produces carries its license.

1. **Named overlays, beside the default.** `imagegen_families` and `gen_edit_families` map a family
   name to an overlay of the route's own config keys:
   - `imagegen_*`, `sdcpp_*` and `comfy_*` for image;
   - `gen_edit_*` and `comfy_*` for edit.

   A request selects a family with its `family` param (MCP, `generate-image --family`, the fleet
   `image-gen` payload). A request without one renders exactly what it rendered before families
   existed. An overlay starts from the node's config with every model-binding key cleared, so a
   family never inherits the default's checkpoint, LoRA, preset or pool. It then applies its own
   keys.
2. **The license is part of the binding.** Every overlay must declare `license` (a non-empty string)
   and `commercial_use` (a bool). An overlay that lacks either, or uses an unknown or forbidden key,
   refuses the config load by name. A default binding may declare `imagegen_license` and
   `imagegen_commercial_use` (edit: `gen_edit_*`), both or neither.
3. **Every result is tagged.**
   - Every result carries `family`, and `license` / `commercial_use` whenever the binding declares
     them.
   - A `commercial_use: false` result also carries `license_note`: "research/evaluation use only
     under <license>; not for commercial work".
   - The ledger row carries `license`.
   - `offload_status` (`media.image_families` / `media.edit_families`) and `/fleet/health`
     (`image_families`) list each family with its license flags.

   An undeclared license is published as null, which a reader must treat as unknown, never as
   commercial-safe.
4. **Never a default, never a seed.** The config load warns when `imagegen_family` or
   `gen_edit_family` names `qwen-image-2.1` on the default binding. No tier in
   `setup/templates/profiles.json` seeds a non-commercial family as a default. A tier may seed one
   as a named overlay only with measured values and its license declared.

This amends ADR 0011's reasoning for named, tagged families. It does not reverse ADR 0011's FLUX
decision, and it does not edit that record. The objection "the harness cannot tell which output is
commercial" is answered by construction:
- a non-commercial output exists only because a caller named the family in that request;
- the output says so in its own result.

## Consequences

- The fleet gains RGBA generation, multi-reference editing and native-2K typography without
  changing any node's commercial-safe default (krea2, HiDream-O1, the Apache-2.0 2511 edit seat).
- Responsibility moves to the caller, as it already does for run-graph manifests (ADR 0011's scope
  boundary). A caller that names a research family for brand work is misusing a clearly labelled
  tool. The harness does not decide what "commercial" means for a request.
- Results gain fields, and the default binding's results gain `family` and measured `width`/`height`.
  No existing field changes meaning.
- A family that does not resolve fails the config load. It does not defer at render time. A typo'd
  overlay key cannot silently render the wrong binding.
- A node that serves a business with no research purpose should not bind a research family at all.
  The opt-in is per node, and seeds never carry it.
- The license tag is informational. It cannot stop a caller from republishing an image. The ADR
  records that limit rather than implying enforcement.

## Alternatives considered

- **Evaluation-only through `offload_run_graph`.** The caller supplies the graph and the manifest.
  That is ADR-0011-clean and needs no repo change, but every caller has to rebuild the graph and the
  sigma schedule and track the license by hand. The results carry no tag, the harness's
  display-card placement and launch profile do not apply, and nothing in `offload_status` says the
  model is available. It stays the right door for one-off experiments.
- **Obtain a commercial license** (model-business@notice.qwencloud.com). This would allow a default
  binding, and a new ADR could supersede this one if it is obtained. No price or terms are
  published, so it cannot be the plan of record.
- **Skip the model.** This loses the only open-weights RGBA generator and 10-reference editor
  available today, and it answers ADR 0011's objection by avoidance rather than by construction.
- **Bind it as a default with a warning.** Rejected. That is exactly the case ADR 0011 describes,
  where every unnamed request silently produces research-only output.

## Related code

- `internal/config/families.go`: overlay validation, resolution, `FamilyInfo`, license notes.
- `internal/config/config.go`: the `imagegen_families` / `gen_edit_families` / license keys and the
  default-binding warning.
- `internal/pipeline/pipeline.go`: `runGenerateImage`, `runEditImageGenerative`,
  `imageResultPayload`, `addLicenseData`.
- `internal/ledger/ledger.go`: `Entry.License`.
- `internal/mediacap/families.go`: per-family route verdicts and the family rows.
- `internal/mcpserver/mcpserver.go`, `internal/fleetnode/tasks.go`, `main.go`: the `family` param on
  every door.
- `render/wf-qwen-image-21.mjs`: the Qwen-Image-2.1 graph builder.

## Related docs

- [0011-flux-family-license-prohibition.md](0011-flux-family-license-prohibition.md): the record
  whose reasoning this amends for named, tagged families.
- [../../systems/media-generation.md](../../systems/media-generation.md): named families, launch
  profiles and license tags.
- [../../OPERATOR-GUIDE.md](../../OPERATOR-GUIDE.md): how to add a family overlay.
- [../../glossary.md](../../glossary.md): "Named family".
