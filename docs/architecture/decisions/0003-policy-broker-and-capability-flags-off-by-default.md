---
status: Accepted
date: "2026-07-18"
---

# Single policy broker; capability flags off by default

## Context

`local-agent` is a coding agent driven by small local models. It can read files, search, write files,
delete them, fetch URLs, and execute programs. The models driving it are far weaker than a frontier
model and demonstrably get stuck: one 9B model re-issued an already-refused, byte-identical tool call
seventeen times in a row.

A weak model with write and exec capability, looping, is a bad combination. The design question is
where the limits live and what the default posture is.

## Decision

**Every effectful action goes through one policy broker**, and **every capability is off by default.**

Two distinct chokepoints, deliberately not merged — this is the part most easily misread:

1. **The policy broker** (`internal/agent/policy.go`) gates *effectful actions* — write, overwrite,
   delete, fetch, shell. `classify` resolves deny → ask → allow, first match wins, deny
   unconditional. Under `--unattended` an `Ask` becomes a `Deny`. An `Allow` that cannot be written
   to the audit trail is downgraded to `Deny` ("refusing to proceed: audit write failed"), so an
   unauditable action does not happen.

2. **The loop** (`internal/agent/loop.go`) enforces *budgets* — the step limit and the tool-call
   caps. `dispatchOrThrottle` is the only path to `dispatch`, which is the only place `t.Exec` runs,
   so a throttled call never executes. The name cap is checked before the exact-repeat check, and the
   code says why: a model stuck retrying an identical call would otherwise match the exact-repeat
   branch forever and never reach the branch that disables the tool. A tool that breaches its cap is
   also stripped from the spec list sent on every later request — structural enforcement a weak model
   cannot argue with.

**Capabilities are opt-in, one flag each, all defaulting to `false`:** `--allow-write`,
`--allow-overwrite`, `--allow-delete`, `--allow-fetch`, `--allow-shell`, `--allow-run`,
`--allow-search`, `--allow-github`. Profiles narrow the toolset and can never widen it — a tool named
in a profile that was not already granted is silently ignored.

**The `run` tool executes an allowlisted program directly, with no shell**: `go`, `gofmt`, `python`,
`python3`, `pytest`, `npm`, `node`, `cargo`, `git` — bare name only, resolved on the trusted PATH,
refused if the resolved binary lives inside the worktree (otherwise the `build` profile, which grants
both `write_file` and `run`, would let the agent plant its own `go` and execute it).

**`run_shell` is Linux-only**, because an arbitrary command line makes an executable allowlist
meaningless. It is withheld on other platforms by an explicit `runtime.GOOS == "linux"` check.

**Confinement is asymmetric by platform, and the weaker side is stated rather than glossed.** Linux
uses a real cage: user, network, and PID namespaces, seccomp with `no_new_privs`, and Landlock, with
a fail-closed ABI floor that refuses to run uncaged rather than silently degrade. Native Windows uses
a Job Object plus a low-integrity token, which blocks writes outside the worktree via MIC — but does
**not** sever network egress and does **not** block reads outside the worktree. The source comment
labels this "HONEST RESIDUAL RISK (documented, not hidden)", and the tool description the model sees
says the same thing.

## Consequences

- The default agent is read-only and safe to point at a repository without thinking about it.
- Granting capability is a deliberate, visible act in the command line — and it is auditable, because
  the audit trail is what makes an `Allow` valid.
- A stuck model burns its budget and stops, rather than looping until interrupted.
- Windows users get a genuinely weaker boundary than Linux users. This is accepted and documented
  rather than papered over; the runner is off by default and the tool description states the limits.
- Two chokepoints means two places to look when something is refused. The refusal message
  distinguishes them: broker denials name the policy, throttle refusals say "NOT executed".

## Alternatives considered

- **One combined gate for budgets and permissions.** Rejected: they answer different questions ("may
  this happen at all?" vs "has this happened too often?") and have different failure modes. Merging
  them would make the exec-ordering guarantee harder to prove.
- **Capabilities on by default with a `--safe` opt-out.** Rejected: the safe posture must be the one
  you get by forgetting to think about it.
- **A shell tool with a command-string allowlist.** Rejected: quoting, chaining, and substitution
  make command-string allowlisting unsound. Direct exec of an allowlisted binary with argv is
  checkable.
- **Refusing to run on Windows at all**, given the weaker cage. Rejected as disproportionate — the
  runner is off by default, and the asymmetry is disclosed at the point of use.

## Related code

- [`internal/agent/policy.go`](../../../internal/agent/policy.go) — the broker
- [`internal/agent/loop.go`](../../../internal/agent/loop.go) — step budget, `dispatchOrThrottle`
- [`internal/agent/runtool.go`](../../../internal/agent/runtool.go) — allowlist and direct exec
- [`internal/agent/builder.go`](../../../internal/agent/builder.go) — capability grants, Linux-only
  shell
- [`internal/sandbox/`](../../../internal/sandbox/) — Linux cage and Windows job object

## Related docs

- [0004-worktree-confinement-audit-outside.md](0004-worktree-confinement-audit-outside.md)
- [0005-loopback-only-serve.md](0005-loopback-only-serve.md)
