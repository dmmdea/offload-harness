---
status: Accepted
date: "2026-07-18"
---

# Worktree confinement with the audit trail stored outside it

## Context

When the coding agent is granted write capability, two things need protecting: everything outside the
directory it was pointed at, and the record of what it did.

The second is easy to get wrong. An audit trail stored inside the working directory is writable by
the very tools it is auditing — the agent could truncate its own log, and a compromised or merely
confused run would erase the evidence of what happened.

Repository metadata needs separate protection. `.git` sits inside the worktree, so plain worktree
confinement does not cover it, and writing there has consequences that outlive the run: a planted
hook or a rewritten `config` executes later, on the host, outside any cage.

## Decision

**Writes are confined to the worktree** (defaulting to `--root`), enforced with `os.Root` rather than
string comparison — the kernel rejects `..`, absolute paths, and symlink or junction escapes,
fail-closed. File creation uses `O_EXCL` and re-brokers as an overwrite if the file races into
existence, closing the TOCTOU window.

**`.git` is denied unconditionally by the broker**, checked per path segment with normalization
(lowercased, trailing spaces and dots stripped), so `.GIT`, `.git.`, and `sub/../.GIT` are all
refused. This check sits before the allow/ask decision and is unaffected by how permissive the run's
posture is. `.gitignore` remains writable. The rule exists because of a real regression: a
fresh-context review wrote files into `.git/hooks/`.

**The audit trail lives outside any worktree** — by default
`~/.local-offload/agent-audit.jsonl` *(amended 2026-10-02: under the harness install root, see the
amendment below)*, append-only JSONL at mode `0600`. This is enforced at
construction, not by convention: `Build()` computes the audit path's location relative to the
worktree and **fails the build outright** if it resolves inside, with a message naming the problem
("the agent could clobber it"). The ask-queue is held to the same rule.

The two protections reinforce each other: the broker downgrades an `Allow` to `Deny` when the audit
write fails, so an action that cannot be recorded does not happen.

## Consequences

- A run cannot tamper with its own log, even with write capability and a shell on Linux.
- Pointing `--audit` inside the worktree is a startup error, not a silent weakness. This surprises
  people occasionally; the error message explains why.
- The audit trail accumulates outside the repository, so it survives worktree deletion — and must be
  managed separately if it grows.
- On Linux, the shell path gets a second layer: a read-only tmpfs is mounted over `.git` inside the
  private namespace, so even a shell command cannot plant a hook.
- **Known asymmetry:** that `.git` tmpfs mask is Linux-only. On native Windows the `run` path has no
  equivalent, `git` is on the exec allowlist, and the worktree is temporarily low-integrity during a
  run. The broker's `.git` denial still covers the file tools on every platform, but the Windows
  `run` path does not get the second layer. Recorded here as a known gap rather than left implicit.

## Alternatives considered

- **String-prefix path checking** instead of `os.Root`. Rejected: it has a long history of bypasses
  via `..`, symlinks, junctions, and volume-qualified paths. Kernel-enforced containment is checkable
  in a way string logic is not.
- **Audit inside the worktree, protected by broker rules.** Rejected: it relies on every future tool
  respecting a rule, rather than making the unsafe arrangement impossible to construct.
- **Blocking `.gitignore` along with the rest of `.git`.** Rejected: legitimate and harmless, and
  blocking it would generate false refusals.
- **String-matching `.git` for shell and fetch actions too.** Rejected deliberately: it would
  spuriously reject ordinary commands like `find . -path '*/.git/*'`. The Linux cage handles the
  shell path structurally instead.

## Related code

- [`internal/agent/writetools.go`](../../../internal/agent/writetools.go) — `os.Root` scoping,
  `O_EXCL`
- [`internal/agent/policy.go`](../../../internal/agent/policy.go) — `.git` segment denial, audit
  append
- [`internal/agent/builder.go`](../../../internal/agent/builder.go) — audit-outside-worktree build
  check
- [`internal/sandbox/`](../../../internal/sandbox/) — Linux `.git` tmpfs mask

## Related docs

- [0003-policy-broker-and-capability-flags-off-by-default.md](0003-policy-broker-and-capability-flags-off-by-default.md)

## Amendment 2026-10-02 (register C-92, the audit trail follows the install root)

The default location in the Decision no longer holds. The audit trail, the ask queue and the per-goal
traces are data that grows, and on a node whose install root (`home` in the config) sits on a data drive
they kept landing under `~/.local-offload` on the OS drive. They now default to `agent-audit.jsonl`,
`agent-asks.jsonl` and `agent-traces` under the install root: `home` when it is set, else
`~/.local-offload`, which is where every earlier release wrote them, so a node with no `home` is
unchanged. What this ADR protects is unchanged: the path is still outside any worktree and `Build()` still
fails outright when it resolves inside one; a relative install root counts as none, because it could sit
inside the worktree. For a node that already had a `home`, the old trail stays at the old path and new
entries start a new file: nothing moves or merges them, and the agent names both paths once per process
when it finds the old one.
