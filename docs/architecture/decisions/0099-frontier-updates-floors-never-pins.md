---
status: Accepted
date: "2026-10-28"
---

# Frontier updates: floors, never pins

## Context

The harness carried a pin store. `setup/install.ps1` held a table of exact release tags (llama.cpp, llama-swap,
stable-diffusion.cpp), exact model files with byte counts and sha256, and a doctrine written around it: assets held at their
tags, no swapping of a build, the table as a snapshot of the frontier refreshed by a human on a cadence, and "take the blessed
tag, never the newest nightly" (the 2026-08-27 policy that followed a llama.cpp regression). Runtime refusals enforced exact versions too: the
HyperFrames runner refused any install that was not its own pin, the media-capability check compared model files to byte-count
literals, node packs were admitted by commit, and the dev tooling (OpenSpec, lockfiles, the CI toolchain) was held equal to
literals by tests.

Pins decay. The fleet-update sweep reported the inference engines as `manual` because their registry rows had no update
command, so llama.cpp on live nodes climbed past the installer's tag while a fresh install would still have laid down the old
build. The installer's skip rule compared against the literal, so a re-run over a newer hand-placed build re-extracted the old
one: a silent downgrade. The same literals sat in the runbook, the system docs, the tier notes, the specs and the plans, each
one a statement that a version is a decision a human re-makes on a cadence.

ADR 0081 grew out of that shape: with one pinned build per node, a box whose seats needed an older engine and an entry that
needed a newer one could only be served by a second llama.cpp build per entry.

The operator's standing rule (2026-08-23) is that every library, framework and dependency of the harness is always updated to
the latest version, never pinned into staleness, and that the engineer in the loop absorbs update breakage instead of freezing
versions. The order of 2026-10-28 made the harness unpinnable on purpose.

## Decision

1. **Versions are never pinned.** No release tag, commit, byte count or digest literal for any dependency lives in the
   repository, in code or in prose. The installers and the update verb resolve the newest complete published build from the
   publisher at run time. "Latest" means the newest build that is complete and verifiable, not a curated release cut. This
   reverses the 2026-08-27 "blessed tag, never the newest nightly" policy: the measured gate and the rollback below replace
   curation. The `release` channel (the build llama.cpp's own release stub names) stays selectable per node in `config.json`
   (`update.channel`, `nightly` by default).
2. **Floors, not pins.** A seat may need a minimum build (`gemma-embedding2` needs llama.cpp b11452 or newer; FLUX 3 needs
   stable-diffusion.cpp master-968 or newer). Floors live in exactly one file, `setup/floors.json`, and only rise. A ceiling or
   an exact version is unrepresentable there. A candidate under a floor is never selected; a node above a floor is never moved
   down. The CUDA major an asset family needs (12.x for Maxwell, sm_50 to sm_53) is a per-node capability read from the driver,
   not a pin.
3. **Integrity comes from the publisher at install time.** GitHub `assets[].digest` (and `gh attestation verify` where the
   project publishes attestations and `gh` is present: fail closed when present and failing), Hugging Face `lfs.oid` and size,
   npm `dist.integrity`. Nothing hashed lives in the repository. A mismatch against the publisher's digest deletes the
   `.part` and is retried once; a recurrence stops and is surfaced. An installer or an agent must not swap in a different
   file to get past it.
4. **Generations with rollback.** Per component the node keeps the current and the previous generation and nothing else.
   `<home>/<component>` is the live path every rendered config and task already uses and becomes a junction (Windows) or a
   symlink (Linux) to a write-once `<home>/<component>-versions/<id>` directory; a candidate is staged in `.staging`,
   verified, and promoted by an atomic switch. A rejected candidate is deleted at once and recorded in the node-local
   `installed.json` (`components.<name>.{current, previous, rejected[], etag, checked_at, floor}`) and is never retried; a newer
   id is a fresh candidate. Models stay in the flat `<home>/models/` layout; a re-fetched file is staged beside the old one,
   swapped by rename, and the old one kept as `<name>.prev` until the next promotion. A newer hand-placed build is kept,
   never downgraded.
5. **The update verb.** `local-offload update [--component ...] [--channel ...] [--dry-run] [--json] [--rollback <component>]
   [--repoint <yaml>] [--resolve <component>]` is safe to run at any time. It takes a lock, resolves with a conditional
   request (a repeat costs one 304 and is a no-op), checks the floor, downloads to staging, verifies size, digest and
   attestation, runs the offline self-check (version, device list, the rendered config's flags against the staged server's
   `--help`), gates the candidate under the GPU lease on a side port against the previous generation measured back to back
   (rejected on no load, wrong output, decode or prefill more than 20 percent slower, or VRAM more than 10 percent higher; a
   lease timeout is "inconclusive, retry later"), promotes, reloads the consumer through llama-swap's own unload and reload,
   and rolls back by itself on any failure after the switch. `--repoint` makes a one-time, backed-up edit of a hand-kept
   config that names a build directory (setting `LD_LIBRARY_PATH` to the executable's own directory on Linux); it never
   re-renders one. The fleet-update registry rows for the engines carry this verb as their update command, so the sweep
   applies them and both tools share one definition of "latest". No scheduler is created; the operator or the sweep runs it.
6. **Runtime refusals become floors or records.** The HyperFrames runner records the installed version and refuses only
   below the floor; the media-capability check compares a file with the size recorded when it was downloaded, not a literal;
   node packs are admitted by repository with the commit recorded at install; the vendored llama-swap schema is refreshed
   from upstream by `go generate`; lockfiles go (npm and uv verify against their registries at install, with
   `--ignore-scripts`).
   *Amendment 2026-10-28:* an installed node pack is moved only by `update --component comfy-packs`, never by a render: it
   takes the newest head of the pack's default branch, resolves all packs together under the host-torch constraints, gates the
   move on `pip check` and ComfyUI's own import pass, and undoes a failed move in git and in pip, recording the commit in the
   pack's `rejected[]` (docs/systems/update.md, "Node packs").
7. **The lint.** One root test, `no_version_pins_test.go`, fails when an installer, a render script, a dependency manifest or
   an accelerator recipe carries a release tag, a build number, a 40- or 64-hex digest, an `==` requirement or an exact
   version in a dependency map, naming file and line. `setup/floors.json` is the one place a build number may appear. The
   repository's `AGENTS.md` and `CLAUDE.md` carry the rule in one sentence each.
8. **Words.** A *floor* is a minimum. A *version pin* is forbidden. A *placement pin* (ADR 0078: a model pinned to a GPU
   card, `pin_reason`) is about where a model sits and is unrelated to versions; it stays. A *generation* is one installed
   build of one component. Dated measurements ("measured on b11490 on 2026-10-09"), ADR bodies, `CHANGELOG.md` entries and
   `contracts/` are history and are not rewritten; every sentence that stated pinning as a rule, a design choice, a cadence
   or a prohibition on a newer build is rewritten to this rule.

## Consequences

- A fresh install lands on the newest complete build, and a re-run over a newer build changes nothing. The repository never
  needs an edit for an upstream release to reach a node.
- The harness absorbs update breakage by design: the gate and the kept previous generation are the backstop, not a frozen
  version. A subtle regression under the gate's thresholds can ship; the previous generation is one pointer write away.
- A hijacked publisher that serves a matching digest is not caught by the digest alone; the gate, the attestation where one
  exists, and `--ignore-scripts` for package installs are the mitigations.
- A floor set too high blocks a node until it is lowered; a floor is a one-line edit in one file.
- Disk cost is bounded at two generations per component; a rollback past one generation re-downloads.
- A box whose live path is still a real directory, and the hand-kept configs of the two boxes that name build directories,
  need the one-time migration and repoint the verb performs.
- The golden that hashed every unset render of the ten serving templates, and the test that held it, are gone: it forced a
  re-hash on every template edit and guarded nothing the render audit's MATCH/STALE re-render check does not. That was the
  "re-hash on nothing" cost this ADR removes.
- CI stays offline: the resolver has an offline seam (`OFFLOAD_RESOLVE=offline` with a fixture) so installer tests load
  without the network.
- vLLM and LMCache patches are not pins but carry a re-base duty: the update path re-applies them on the newest version and
  fails loud, keeping the previous generation, when they do not apply.
- Older documents that describe a pinned build do so as a record of what was measured then. Where an ADR summary or an old
  record states a pin as policy, this ADR is the rule.

## Supersedes

- [ADR 0081](0081-the-embeddinggemma2-entry-may-run-from-a-second-llama-build.md): its second-build doctrine (one pinned
  build per node, so a per-entry second llama.cpp build for an entry whose seat needs a newer engine than the node's
  pinned one). With every node on the newest complete build, the second build is unnecessary; the b11452 floor check it
  introduced stays, now fed by `setup/floors.json`. ADR 0081's body is kept as the record of the decision it made.
- The setup runbook's rule that a held asset is never swapped for a newer build, the table-as-a-snapshot-refreshed-on-a-cadence
  doctrine, and the 2026-08-27 "blessed tag, never the newest nightly" policy.

## Alternatives considered

- **Keep the pins and automate the bump.** A pin set is a second source of truth that is wrong the day after it is written;
  automating its refresh keeps the cadence and the staleness window.
- **Pin on the curated release, float within it.** A release cut is another person's cadence; the measured gate answers the
  actual question (does this build work on this node) in minutes.
- **A lockfile for integrity.** Integrity is the publisher's to state at install time; a lockfile is a pin set.
- **A scheduler that updates unattended.** Refused by the house rule on unattended schedulers; a scheduled task is offered
  to the operator, not created.

## Related docs

- [Frontier updates design](../../specs/2026-10-28-frontier-updates-design.md)
- [Frontier updates plan](../../superpowers/plans/2026-10-28-frontier-updates.md)
- [Setup and installer](../../systems/setup-installer.md)
- [ADR 0078](0078-a-placement-pin-needs-a-reason-without-one-it-is-a-hint.md) — the placement pin, unrelated to versions
- [ADR 0081](0081-the-embeddinggemma2-entry-may-run-from-a-second-llama-build.md) — superseded
