---
status: Accepted
date: "2026-07-18"
---

# Public repository is canonical; private is the development and moat repository

## Context

[ADR 0006](0006-private-canonical-public-squash-mirror.md) made the private repository canonical and
published a squash-mirror to the public one. That arrangement had a standing failure mode and a
standing cost: a merged contributor pull request on the mirror was destroyed by the next publish
unless someone remembered to port it into private first, and every publish required a privacy scrub
because the private working tree carried operator-specific values (hostnames, tailnet addresses,
local paths) that could not go public.

Two things changed and removed the reason for that model:

- The code is now **operator-neutral at rest.** Machine-specific values live outside tracked code —
  gitignored local config (`config.json`) and configuration/environment inputs rather than compiled-in
  constants (for example the mem0 recall namespace, de-hardcoded in 0.22.2). The tracked tree is
  publishable directly.
- The moat — the engineering record that genuinely should stay private (specs, plans, evidence
  ledgers with real machine detail) — is cleanly separable at `docs/superpowers/`.

Once the code is publishable directly, keeping the public repo as a downstream mirror is pure
overhead: it inverts the natural direction of a project that accepts outside contributions.

## Decision

The roles are inverted.

- **`dmmdea/offload-harness` (public) is canonical.** Development happens here. Contributors work here
  directly — normal fork/branch/PR/review/merge — with **no port-to-private step**. `VERSION` and
  `CHANGELOG` authority live here.
- **`dmmdea/local-offload-harness` (private) is the development and moat repository.** It keeps the
  full pre-inversion history, the engineering record (`docs/superpowers/` specs, plans, evidence), and
  serves as the working home for machine-specific operator configuration (gitignored / local). It no
  longer holds the canonical code.

Rules that keep the inverted arrangement coherent:

1. **Tracked code stays operator-neutral.** No real hostnames, tailnet addresses, identity-leaking
   paths, or personal namespaces in tracked files — those live in gitignored config or environment
   inputs. The privacy rules in [STYLE.md](../../STYLE.md) still apply, now to the canonical repo
   itself rather than to a publish step.
2. **The moat stays private.** `docs/superpowers/` is not published; the public `docs/README.md`
   points contributors to the private development repository for the engineering record.
3. **Contributions land once, on public.** There is no mirror to reconcile and nothing to port.

## Consequences

- The two failure modes of the mirror model are gone: no merged contribution is destroyed by a
  publish, and there is no scrub-and-port maintenance burden.
- Contributors get canonical status and full credit for work merged on the public repo.
- The engineering record and the full development history remain private, deliberately.
- Machine-specific configuration lives in no tracked file — gitignored locally, present only on the
  operator's machine.
- Version numbers are authoritative on public. The private repo's `VERSION`, once it becomes the moat,
  is historical.
- This supersedes [ADR 0006](0006-private-canonical-public-squash-mirror.md).

## Alternatives considered

- **Keep the mirror model of ADR 0006.** Rejected: its port-or-lose failure mode and per-publish
  scrub cost are exactly what operator-neutral code lets us eliminate.
- **Fresh-init the public repo as a clean-history snapshot** (the pattern a sibling project used).
  Rejected here: the public repo already exists with release tags and an active outside contributor's
  branches and pull requests; a fresh init would orphan them. Keeping and evolving the existing public
  history preserves that work.
- **Publish the moat too.** Rejected: the evidence ledgers carry real machine detail, and the
  specs/plans are the project's development reasoning — legitimately private.

## Related code

- The operator-neutral memory namespace change (`agent.ReadUsers`, `--mem-shared-namespace`) in 0.22.2
  is the representative example of moving operator values out of tracked code.

## Related docs

- [0006-private-canonical-public-squash-mirror.md](0006-private-canonical-public-squash-mirror.md) —
  superseded by this ADR
- [../../STYLE.md](../../STYLE.md) — privacy rules, now applied to the canonical repo
- [../../../CONTRIBUTING.md](../../../CONTRIBUTING.md)
