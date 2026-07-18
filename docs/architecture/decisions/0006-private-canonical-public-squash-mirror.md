---
status: Accepted
date: "2026-07-18"
---

# Private canonical repository with a public squash-published mirror

## Context

This project is developed in the open enough to accept outside contributions, but its working history
is not publishable as-is. Commits, evidence ledgers, and operator notes accumulated while building it
on specific machines contain hostnames, tailnet addresses, and local paths — operational detail about
a private network that has no business being on a public repository, and which cannot be recalled
once pushed.

At the same time, a collaborator needs a normal GitHub workflow: fork, branch, pull request, review,
credit.

## Decision

Two repositories with distinct roles:

- **`dmmdea/local-offload-harness` (private) is canonical.** All development happens here. It holds
  the full history.
- **`dmmdea/offload-harness` (public) is a mirror**, updated by squash-publishing — the public
  history is a series of release snapshots, not a replay of private commits.

Rules that keep the arrangement coherent:

1. **The private version is always greater than or equal to the public one.** The mirror never leads.
2. **Collaborator pull requests land on the mirror first**, so the contributor keeps normal GitHub
   flow and credit, and are then **ported into the private repository before the next
   squash-publish**. Skipping the port means the next publish overwrites the contribution.
3. **Anything destined for the mirror passes a privacy scrub**: no real hostnames, no tailnet IP
   addresses or domain, no identity-leaking local paths. Placeholders are used instead.
4. Porting adapts to the version delta. A contribution written against the public snapshot may need
   adjusting for private changes that have not been published yet.

## Consequences

- Contributors get a normal workflow and visible credit without the project exposing its operating
  environment.
- **There is a real failure mode**: a merged mirror PR that is not ported gets destroyed by the next
  publish. Porting is a step in landing a contribution, not an afterthought.
- Public history is coarse. Someone reading the mirror sees releases, not the reasoning between them
  — which is part of why the documentation in `docs/` matters: it is the reasoning, published
  deliberately.
- Version numbers are a synchronization signal. Public behind private is normal; public ahead is a
  bug.
- Every doc written here is written knowing it may be published, which is why the privacy rule lives
  in the style guide rather than in a release checklist.

## Alternatives considered

- **Single public repository.** Rejected: the working history cannot be published, and rewriting it
  retroactively is both unreliable and destructive to the record.
- **Single private repository, no mirror.** Rejected: it forecloses outside contribution, which the
  project wants.
- **Mirroring commit-for-commit with a scrubbing filter.** Rejected: a filter that must catch every
  hostname in every future commit is a guarantee nobody can honor. Squash-publishing makes the
  publishable surface small enough to review.

## Related docs

- [../../STYLE.md](../../STYLE.md) — the privacy rules that apply to every doc
- [../../../CONTRIBUTING.md](../../../CONTRIBUTING.md)
