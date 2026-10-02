# Documentation style guide

How to write documentation in this repository. The system it describes is indexed in
[README.md](README.md); the shapes to fill in live in [templates/](templates/).

Documentation is written for both humans and AI coding agents. Source code stays the implementation
source of truth — documentation explains the system at a useful level of abstraction: what each
system does, how major flows operate, what assumptions must remain true, where the key source files
are, and what a contributor should understand before changing something.

## Core rules

Good documentation here:

- Uses clear, direct Markdown with stable headings.
- Links to related docs and source files with relative links from the current file.
- Explains behavior, responsibilities, flows, invariants, and pitfalls.
- Avoids restating every implementation detail.
- Avoids unsupported guesses, and marks uncertainty explicitly.
- Includes a source map of the files that matter.
- Stays short enough to actually read before making a change.

The best docs are not exhaustive. They are navigational and explanatory: they help a reader
understand what matters, where the details live, and what could break if the system changes.

## Verify before you write

Every factual claim in a doc must be verified against the source at the time of writing — read the
file, run the command, check the test. Do not carry a claim over from a plan, a spec, an older doc,
or memory without confirming it still holds.

When behavior is genuinely hard to determine, say so rather than inventing an explanation:

```md
> **Unverified:** the retry count here is set by the caller; the default was not
> traced during this pass.
```

An honest gap is useful. A confident wrong statement is worse than no doc at all, because both
humans and agents will act on it.

## Granularity

Create or expand documentation when behavior is important enough that a reader would otherwise have
to inspect several files to understand it.

Keep behavior inside a system doc when it is local and easy to explain there. Promote it to its own
flow doc in `flows/` when it:

- Crosses multiple systems.
- Has several steps or state transitions.
- Is frequently changed or debugged.
- Involves external services or hardware.
- Has important error handling.
- Has security, data-integrity, or user-visible implications.

Do not document every file. Prioritize what is central, risky, frequently changed, difficult to
infer, or important to observable behavior.

## Source maps

Every system and flow doc ends with a source map linking the most important files that implement the
behavior — entry points, state definitions, handlers, services, jobs, tests, integration points.

A source map exists so a reader can inspect details without rediscovering where the implementation
lives. Do not list every file unless the system is small.

## Glossary terms

Terms defined in [glossary.md](glossary.md) use Title Case in finished documentation when it improves
clarity or distinguishes an application-specific concept from an ordinary word — a Defer is a
specific structured result, not just any deferral.

Title Case is a clarity aid for finished docs. The same concept can appear lowercase in drafts,
comments, source identifiers, issues, and informal notes.

## Architecture Decision Records

ADRs live in [architecture/decisions/](architecture/decisions/README.md) and record decisions that
shape the system beyond a single implementation detail.

Every ADR begins with YAML frontmatter. The allowed fields are exactly:

- `status` (required): `Proposed`, `Accepted`, `Superseded`, `Deprecated`, or `Rejected`.
- `date` (required): creation date, `YYYY-MM-DD`, quoted.
- `superseded_by` (required for `Superseded` only): repo-relative path to the replacement ADR.

Do not add other frontmatter fields. Only `Accepted` ADRs are current guidance.

Never rewrite an accepted ADR to describe a different decision. When a decision changes, write a new
ADR, mark the old one `Superseded`, and link it forward with `superseded_by`. Accepted ADRs may still
receive small corrections and links that do not change the recorded decision.

Architectural decisions are human-owned. Agents draft ADR text from decisions that were already made
and keep existing ADRs aligned with the code; they do not decide architecture.

Routine implementation details, small refactors, bug fixes, and temporary workarounds belong in code,
PRs, or ordinary docs — not in ADRs.

## Code comments

Use docs to explain how a system works. Use code comments for implementation-specific context that is
most useful when read directly beside the code.

Good comments explain non-obvious behavior, ordering constraints, side effects, invariants, security
assumptions, external API quirks, retries, caching, concurrency concerns, or a local implementation
detail that affects another system. Prefer a comment when it prevents a real misunderstanding, bug,
or unsafe refactor.

Avoid comments that restate the code, explain obvious names, duplicate system documentation, carry
long architectural explanations that belong in an ADR, or leave stale historical notes.

## Privacy

This is the public canonical repository — everything tracked here is already public. Documentation
(and code) must never contain:

- Real machine hostnames.
- Tailnet IP addresses or the tailnet domain name.
- Local filesystem paths that leak a username or identity.
- The names of the operator's machines, people or brands, in any spelling or encoding.

Use placeholders instead — `<node-a>`, `<tailscale-ip>`, `<repo-root>`. Operator docs that must
describe a two-node setup describe it generically.

A test enforces this: the [leak gate](systems/leak-gate.md) fails the build when a tracked file, or a
tracked file name, holds a denied name. The list it checks is a secret, so the repository carries only
keyed hashes of it, and a failure names the path and line, never the text. The rules below say how to
write what the gate leaves you; fix a finding by rewriting the text, never by asking for an exception.

### Naming machines (V1 to V4)

- **V1. A machine is a node letter.** In docs and comments write `<node-a>` through `<node-f>`, or a role
  phrase that stays true over time ("the workstation", "the Linux edge node", "the RK3588 board"). A role
  phrase never carries a tier id. In test fixtures a machine id is a per-test label (`node-a`, `node-b`,
  optionally with a tier suffix) chosen so that sorted output keeps its order; it names no real machine.
- **V2. A tier id states the hardware class AT THE TIME a dated sentence describes, never a machine's
  name.** `ampere-8` is a class of hardware, not a host. A tier id goes into a file name only when the
  document is about that tier.
- **V3. Dated documents keep their own era.** ADRs, dated specs and plans, changelog entries and the frozen
  roadmap use the letter. Add a tier id only where the original sentence already carried a hardware fact,
  and take it from that date: never upgrade an old sentence to the machine's current tier. The
  [glossary](glossary.md#reference-box) lists which machine sat behind each tier, and when.
- **V4. "Reference box" is never bare.** Write "the `<tier>` reference box" with a tier id, or use the
  letter. The word names a different machine for each tier; the [glossary](glossary.md#reference-box)
  defines it per tier.

A test, fixture or document that needs to show a denied name (to feed it to the gate, or to assert it is
absent) builds it from parts at run time, split inside the word: the gate scans test sources like every
other file.

## Keeping docs current

Documentation is valid only when it accurately describes the intended behavior of the current code.

A change that affects system responsibilities, runtime or observable behavior, interfaces, data,
configuration, error handling, security, invariants, testing expectations, or glossary concepts must
update the affected docs **in the same pull request**. Reviewers treat documentation as part of the
change and verify it against the code before approving.

When docs and code disagree, do one of three things — never nothing:

1. Update the docs to match the code.
2. Update the code to match the documented intent.
3. Call out the mismatch explicitly for review.

This is what makes documentation useful during review: a reviewer comparing docs to code catches
stale assumptions, missed behavior changes, and bugs caused by intent and implementation drifting
apart.

## The lint gate

`go test -run TestDocsLint .` checks structure: scaffold files exist, relative links resolve, ADR
frontmatter is schema-valid, and system/flow docs keep their `## Purpose` and `## Source map`
sections.

It checks structure only. Meaning is a review duty — see [CONTRIBUTING.md](../CONTRIBUTING.md).

The gate deliberately exempts two areas from link resolution: [templates/](templates/), whose
placeholder links are meant to be filled in rather than followed, and `superpowers/`, the dated
process archive whose records are immutable and may cite paths from an earlier checkout location.
