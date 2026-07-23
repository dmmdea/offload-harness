# Coding agent

## Purpose

`local-agent` — a coding agent loop driven by the local model Tiers. It reads, searches, and
optionally writes, executes, and fetches, inside a permission and budget envelope built for models
weak enough to get stuck.

## Questions this doc answers

- What can the agent do out of the box, and what needs a flag?
- What stops a stuck model from looping forever?
- How confined is an executed command, and does that differ by platform?
- What is the difference between a profile and two-tier mode?
- Where is the record of what the agent did?

## Scope

The agent loop, its tools, the policy broker, budgets and circuit breakers, profiles, two-tier
architect/editor mode, transcript compaction, worktree memory, and the OpenAI-compatible server.

## Non-scope

- The cascade the agent's `offload_*` tools call → [offload-pipeline.md](offload-pipeline.md)
- The MCP surface, which exposes `agent_run` but is a separate system →
  [mcp-server.md](mcp-server.md)

## Key concepts

**Worktree** — the directory writes are confined to. **Policy broker** — the gate for effectful
actions. **Profile** — a named narrowing of the toolset plus a tuned prompt. **Two-tier** — an
architect model plans, an editor model executes.

## How the system works

The loop alternates model calls and tool calls until the task completes or the step budget runs out.
Two independent limits keep a weak model from burning the budget on nothing:

- **A step budget** — the loop stops with `StopReason: "budget"`.
- **Tool-call caps** — `dispatchOrThrottle` sits between the model's request and execution. It
  refuses an exact repeat (same tool, byte-identical arguments), and it caps calls per tool name
  (`--max-same-tool`, default 3). A tool that breaches the name cap is *also removed from the tool
  list sent on every later request* — structural enforcement, added after a 9B model re-issued an
  already-refused identical call seventeen times in a row.

The ordering here is load-bearing and documented in the code: the name cap must be checked before the
exact-repeat check, or a model stuck on an identical call matches the repeat branch forever and never
reaches the branch that disables the tool.

Separately, the **policy broker** gates effectful actions — write, overwrite, delete, fetch, shell.
It resolves deny → ask → allow with deny unconditional, converts an `Ask` to a `Deny` under
`--unattended`, and downgrades an `Allow` to `Deny` if the audit record cannot be written.

> These are two distinct chokepoints. The broker decides *may this happen at all*; the loop decides
> *has this happened too often*. They are frequently described as one thing, and they are not.

**Tools.** Read-only by default: `list_dir`, ranged `read_file`, `search_files` (regex/glob, capped
matches), `summarize_file` (an offload digest), and the in-process `offload_*` cascade tools. Each of
the rest sits behind its own flag, all defaulting off: `write_file` / `edit_file` / `delete_file`,
`web_fetch`, `web_search`, `run`, `run_shell`, and the `github_*` tools.

**`run` executes an allowlisted program directly, with no shell** — `go`, `gofmt`, `python`,
`python3`, `pytest`, `npm`, `node`, `cargo`, `git`. Bare name only, resolved on the trusted PATH, and
refused if the resolved binary lives inside the worktree (the `build` profile grants both `write_file`
and `run`, so without that check an agent could write its own `go` and execute it).

**`run_shell` is Linux-only.** An arbitrary command line makes an executable allowlist meaningless,
so it is withheld elsewhere by an explicit platform check.

**Profiles** (`--profile general|edit|build|research|github`) narrow the toolset and add a tuned
prompt with exemplars. A profile can only ever narrow — naming a tool that was not granted is
silently ignored.

**Two-tier mode** (`--two-tier`) runs an architect model (default: the escalation Tier) to plan and an
editor model (default: the workhorse) to execute, with one model swap. The architect gets read and
search only; the editor gets whatever capabilities were granted.

> `--profile` and `--two-tier` conflict only for a *non-default* profile. `--profile general` or an
> empty value coexists with two-tier, because two-tier sets its own toolsets.

**Compaction** keeps the transcript within `-ctx-tokens` (default 16384) — set it to match the served
context size. Its ladder is least-destructive-first: under budget nothing is touched (byte-stable, so
the server's KV prefix cache stays warm); over budget, with `--skeleton-prune` (default off) older
tool bodies are first reduced to deterministic **skeletons** — head/tail windows plus buried
error/failure/warning lines, elided runs replaced by counted markers — then, as pressure rises, to
bare size markers, and finally whole older turns are dropped as assistant+tool units. The skeleton
rung is model-free on purpose: a cascade call costs seconds on the loop's critical path (measured;
see `skeleton.go`), a rules pass costs microseconds and produces identical bytes on every
re-compaction.

## Data and state

- **Audit trail** — append-only JSONL, mode `0600`, at `~/.local-offload/agent-audit.jsonl` by
  default. Resolved only when a mutating capability is enabled.
- **Ask queue** — sibling file for deferred approvals.
- **Worktree memory** — an `AGENT.md` loaded into context on a re-injection cadence.
- **Traces** — optional per-run transcripts.

Both the audit trail and the ask queue **must live outside the worktree**. This is enforced at
construction: the builder fails outright if either path resolves inside, because the agent's own
write and shell tools could otherwise clobber the record of what it did.

## Interfaces and entry points

- One-shot: `local-agent --root . "task"`.
- Queue mode and `--serve` (OpenAI-compatible endpoint, loopback-only — see
  [ADR 0005](../architecture/decisions/0005-loopback-only-serve.md)).
- `agent_run` via the MCP surface.

## Dependencies

`internal/pipeline` (recordless path), `internal/sandbox`, `internal/netguard`, the local completion
endpoint.

## Downstream effects

Loosening a default here changes the safety posture of every consumer, including `agent_run` over
MCP. Capability defaults are an interface, not an implementation detail.

## Invariants and assumptions

1. Every `--allow-*` capability defaults to **off**.
2. Writes are confined to the worktree, enforced via `os.Root` rather than string comparison.
3. `.git` is denied unconditionally by the broker, per path segment, with case and trailing-character
   normalization. `.gitignore` remains writable.
4. The audit trail lives outside the worktree, enforced at build time.
5. An action that cannot be audited does not happen.
6. `--serve` refuses a non-loopback bind without `--listen-trusted-network`.

## Error handling

Tool errors become `is_error` results the model can react to; the loop never panics on tool failure.
Throttle refusals are fed back as ordinary tool results with explicit instructions to move on.

## Security and privacy notes

Confinement is **asymmetric by platform, and the weaker side is disclosed**. Linux uses user,
network, and PID namespaces plus seccomp and Landlock, failing closed if the Landlock ABI floor is
not met rather than running uncaged. Native Windows uses a Job Object plus a low-integrity token:
writes outside the worktree are blocked by MIC, but **network egress is not severed and reads outside
the worktree are not blocked**. The source calls this "HONEST RESIDUAL RISK (documented, not hidden)"
and the tool description the model sees says the same.

> **Known gap:** the read-only `.git` mask that protects the shell path is Linux-only. On native
> Windows the `run` path has no equivalent, while `git` is on the allowlist and the worktree is
> temporarily low-integrity during a run. The broker's `.git` denial still covers the file tools on
> every platform. Recorded in
> [ADR 0004](../architecture/decisions/0004-worktree-confinement-audit-outside.md).

## Observability and debugging

Read the audit trail first — it records what was allowed, denied, and why. `StopReason` distinguishes
budget exhaustion from completion. Throttle refusals appear in the transcript as "NOT executed"
messages.

## Testing notes

`internal/agent/` covers the broker (including `.git` normalization cases), the throttle ordering,
write-tool scoping and TOCTOU behavior, profile narrowing, and two-tier plan handling.
`cmd/local-agent/serve_test.go` covers the loopback guard.

## Common pitfalls

- Expecting the broker to enforce step or tool caps. It does not — that is the loop.
- Assuming `--profile` and `--two-tier` are unconditionally exclusive. Only a non-default profile
  conflicts.
- Assuming the Windows cage is equivalent to the Linux one. It is weaker, deliberately and visibly.
- Pointing `--audit` inside the worktree — a startup error, on purpose.

## Source map

- [`internal/agent/loop.go`](../../internal/agent/loop.go) — loop, budget, `dispatchOrThrottle`
- [`internal/agent/policy.go`](../../internal/agent/policy.go) — broker, `.git` denial, audit append
- [`internal/agent/runtool.go`](../../internal/agent/runtool.go) — allowlist, direct exec
- [`internal/agent/writetools.go`](../../internal/agent/writetools.go) — `os.Root` scoping
- [`internal/agent/builder.go`](../../internal/agent/builder.go) — capability grants, audit-path check
- [`internal/agent/twotier.go`](../../internal/agent/twotier.go), [`profiles.go`](../../internal/agent/profiles.go), [`compaction.go`](../../internal/agent/compaction.go), [`skeleton.go`](../../internal/agent/skeleton.go)
- [`internal/sandbox/`](../../internal/sandbox/) — platform cages
- [`cmd/local-agent/`](../../cmd/local-agent/) — CLI and server

## Related docs

- [../architecture/decisions/0003-policy-broker-and-capability-flags-off-by-default.md](../architecture/decisions/0003-policy-broker-and-capability-flags-off-by-default.md)
- [../architecture/decisions/0004-worktree-confinement-audit-outside.md](../architecture/decisions/0004-worktree-confinement-audit-outside.md)
- [../architecture/decisions/0005-loopback-only-serve.md](../architecture/decisions/0005-loopback-only-serve.md)
- [../OPERATOR-GUIDE.md](../OPERATOR-GUIDE.md)
