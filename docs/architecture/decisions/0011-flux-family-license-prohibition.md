---
status: Accepted
date: "2026-07-18"
---

# FLUX-family models are prohibited

## Context

FLUX is a strong open-weights image model family and an obvious candidate whenever image quality
comes up. It was evaluated here twice, and ruled out for two independent reasons.

**Licensing.** The FLUX.1 [dev] weights ship under Black Forest Labs' non-commercial licence.
Commercial work — which is what this harness feeds — is not permitted under it. This reason alone is
disqualifying, and it does not expire with better hardware.

**Hardware, historically.** On the 8 GB card this project started on, FLUX was measured as a no-go:
two large modules (a T5 text encoder and a 12B DiT) contend for the same 8 GB and page-thrash to
roughly 40 seconds per step. That reason *would* expire on a larger card, which is precisely why it
must not be the reason on record.

## Decision

No FLUX-family model is added to this repository — not to a serving template, not to a config
default, not to a hardware profile's model seed, and not as an example in documentation.

The binding reason is the licence, not the VRAM. A bigger GPU does not re-open the question.

The current state matches: FLUX appears nowhere in executable code, configuration, or templates. The
only occurrences in the repository are documentation recording this decision and two test fixtures
using `"flux-dev"` as an arbitrary model-family string to prove that a caller-declared family value
round-trips — which is a string-handling test, not a model reference.

**Scope boundary worth stating precisely.** The run-graph primitive executes graphs supplied by a
caller against a Node Manifest that the caller controls. The harness never adds models of its own
there. So this ADR constrains what *this repository* ships and recommends; it is not a technical
enforcement mechanism against a caller who supplies their own manifest. Compliance at that layer
belongs to whoever authors the workflow.

## Consequences

- Image quality work happens within the licence-clean model set — the HiDream-O1 and SDXL-class
  bindings the profiles seed.
- Anyone proposing FLUX gets a documented answer instead of a re-litigation, and the answer does not
  change when the hardware does.
- A future licence change by the vendor is the only thing that reopens this. That would warrant a new
  ADR superseding this one, not an edit to this file.
- The distinction between "we ship it" and "a caller's manifest could request it" is explicit, so
  nobody mistakes this ADR for a technical guarantee it does not provide.

## Alternatives considered

- **Allow FLUX for non-commercial or internal use only.** Rejected: the harness cannot tell which of
  its outputs will end up in commercial work, so a per-use distinction is unenforceable in practice.
- **Recording the VRAM finding as the reason.** Rejected precisely because it is temporary. Filing
  the decision under the hardware reason would invite an automatic reversal on the first larger card,
  which would land on the licence problem anyway.
- **Saying nothing and relying on nobody adding it.** Rejected: it was proposed more than once, which
  is what an ADR is for.

## Related docs

- [../../ROADMAP.md](../../ROADMAP.md) — the original 8 GB no-go finding
- [../../systems/media-generation.md](../../systems/media-generation.md) — the models actually bound
