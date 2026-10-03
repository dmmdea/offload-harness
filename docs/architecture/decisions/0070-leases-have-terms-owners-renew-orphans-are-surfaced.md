---
status: Accepted
date: "2026-10-02"
---

# Leases have terms; owners renew; orphans are surfaced

## Context

ADR 0018 made the GPU lease machine-wide and fenced, and judged a holder by two facts: is its process alive,
and has it heartbeated. Both are facts the holder writes about itself. That is enough to stop two jobs sharing a
card; it cannot tell a job that is running from one that was abandoned.

A long render was launched by an assistant session that later ended. Its wrapper process stayed alive and kept
heartbeating, the card showed utilisation, and every status surface read it as working for about twenty hours,
while a second session queued behind it and the declared window had passed. Four things were missing, and each is
a property of the record rather than of the reader:

1. **No owner.** The record named the holder (a wrapper pid) and nothing about who asked for it, so "the session
   that wanted this is gone" could not be said.
2. **No terms the holder is judged by.** A `--for` window is an estimate, and for a wrapper it is not enforced; a
   job that has stopped making progress and one that is making it look identical from the outside.
3. **Verdicts that cannot disagree with the holder.** The verdict was derived from card utilisation, so a hung
   job on a busy card and a working one produced the same word.
4. **A wrong sentence in ADR 0018.** It said a leaked lease "expires". It does not, and must not: a holder that is
   alive and heartbeating keeps its claim past its declared window, because freeing a card under a live job is the
   incident ADR 0018 exists to prevent.

The operator's standing orders bound the answer. Nothing in the harness may kill or release another session's job
on its own: no watchers, no timers that act, fail loud over autonomous recovery, and no destructive action without
a human decision.

## Decision

**A lease records who asked for it, and the terms it is judged by.** Additive and `omitempty`, so every existing
record parses as an UNKNOWN owner:

- `owner`: a session id, a process (pid and start identity, so a recycled pid reads as gone), and `remote` for a
  lease asked for from another host. `tracked` records that the session was in the registry when the lease was
  taken. `gpu reserve` fills it from `--owner-*` flags, else from the session label the ledger already resolves
  (`LOCAL_OFFLOAD_ORIGIN`, then `CLAUDE_CODE_SESSION_ID`), never from the process a wrapper happens to run from.
- `unattended`, and a progress contract (`progress_file`, `stall`). `--unattended` requires an explicit `--for`, a
  progress file and a stall window: a lease nobody watches carries the terms it is judged by, and "its owner is
  gone" is not an escape for it. The progress file is recorded as an ABSOLUTE path: the reserving command resolves a
  relative one against its own working directory and the library refuses a relative path, because every reader of
  the lease (the MCP server, the fleet node, another session's `gpu status`) looks for the same file from its own
  directory, and a path recorded as typed would read `unknown` for all of them.
- `yield_grace_ms` and `on_yield`: the job's own terms for being asked to stop. `on_yield` is recorded verbatim
  (refused above 4096 bytes, never clipped: the takeover will run it) with the directory the lease was taken in
  (`on_yield_dir`), because the takeover that runs it later does not share that directory. Read by nothing yet.

**A session registry tells a present owner from a gone one.** Each session's MCP server writes
`<state root>/owners/<session>.<pid>.json` at start and removes it at exit, with no timer. A session is alive
while any registered process carries its id (by pid and start time), or the pid recorded on the lease is alive. A
resumed session starts a new process under the same id and does not flap. One file per process: the new server of
a resumed session and the old one's exit would otherwise be two writers of one record, and a read-modify-write
between them loses an entry. A registry that cannot be READ (anything but "the directory does not exist") is not a
registry with nobody in it: the owner reads `unknown`, never `gone`, and the reading says why; a recorded process
that is alive still stands.

**The first reader to see an owner gone stamps a marker.** `orphan.<epoch>` is written under the epoch lock with a
second look inside it, so concurrent readers agree on one moment; the owner reappearing clears it. **Only the status
surfaces stamp it: `gpu status`, `offload_status` and the `/fleet/health` handler.** The plain inspectors the text
gate polls every blocked second, the sentence a waiter reads and the text of a refusal (`ErrHeld`, the gate's
`LeaseError`) READ the marker a status surface recorded and never write it or take the epoch lock, so formatting a
refusal is safe anywhere and the gate stays read-only. A marker that cannot be written or cleared is reported, never
swallowed (`orphan marker could not be recorded: ...`), and the lease directory is probed for writability before the
epoch lock is taken, because a permission error reads as contention there.

**Verdicts are derived from those facts, in one place, with a fixed precedence.** `stale-holder` (the record's
holder is gone: not a live lease at all), then `tree-orphan`, `held-stalled` (a progress contract and its file did
not move), `held-orphaned` (an attended lease whose owner has been gone past `gpu_orphan_grace_min`, default 15
minutes), `held-overdue` (the declared window ended, the holder still renews), then `held-working` and
`held-idle`. An unknown owner is never orphaned but is overdue-eligible by its declared window; an unattended or
remote lease is never orphaned. `working` (work in flight on the seat) is still reported first; its note leads with
the lease's standing, and the brief line leads with `STALLED`, `ORPHANED` or `OVERDUE` whatever the verdict word is.
With only card utilisation as evidence the note says `util only, no progress contract`. A progress file that never
appears stays `unknown`, never stalled, and the reading says why it is unknown and, past the stall window, for how long
it has been missing (a wrong path is as likely as a slow job); the verdict does not change.

**These are surfaced, never acted on.** `gpu status`, `offload_status`, `/fleet/health` (`orphaned` and `stalled`;
the lease block's `overdue` is the expiry-based key of the routing change, one source for one wire key) and the
refusal a waiter reads say which lease, what is wrong, for how long, what is running and the
exact command that frees it, `local-offload gpu takeover --epoch N`. Nothing reclaims, releases or kills a lease on
a verdict. **That command is not part of this decision's first delivery**, and the message says so; it is the
explicit, human-authorised takeover of a later change, which will refuse a lease with an unknown owner without
`--force`.

**ADR 0018 is corrected.** A lease does not expire. It is reclaimed when its holder is gone, or when its heartbeat
is stale and its declared window has ended; a live holder keeps it, and the harness says so instead of staying
silent.

## Consequences

- An abandoned long job is visible in one status call, with who to ask and how to free it. A healthy one is not
  accused: a lease with no owner is judged by its window alone, and a missing progress file reads `unknown`.
- Older leases and older MCP servers read `unknown` and are never orphaned. Full stale detection needs a lease
  written by the new wrapper with a progress contract.
- A launcher that names a short-lived process as the owner reads orphaned when it exits. `gpu owner-flags` exists so
  a launcher names the right one; the verdict is information with a grace, not an action.
- The `--for` default is 45 minutes, so a wrapper that never declared a window reads `held-overdue` after that
  while it still renews. That is true (the window did end) and is not an accusation; terms and renewal are a later
  change.
- With several live leases every surface describes the MOST ESCALATED one from its own record: the epoch, the owner,
  the progress contract and the takeover command always belong to the lease the verdict names.
- A waiter's sentence reads the marker a status surface recorded, so a lone waiter on an orphan nobody has looked
  at yet is told what the progress file and the window say, and about the owner's absence once any status call has
  recorded it.
- A waiter is told a command that does not exist yet. That is deliberate: the sentence is the contract the takeover
  change implements, and until then the message names whoever to ask.
- Remaining work, in order: terms and renewal (a window becomes a renewal point, never a release point), the process
  tree record that produces `tree-orphan`, takeover and cooperative yield, `gpu release` routing.

## Alternatives considered

- **Judge by utilisation alone, with a stricter threshold.** A hung job can hold a card at any utilisation, and a
  job in a CPU phase reads idle. The signal has to come from the job.
- **Let a waiter take over an overdue or orphaned lease after a grace.** This is the version that kills a
  twenty-hour job without a human decision. Rejected: fail loud over autonomous recovery.
- **Record the wrapper's parent pid as the owner.** The parent is usually a short-lived shell; every lease would
  read abandoned when the shell exits.
- **One registry file per session.** Two writers of one record lose an entry in the resume window; one file per
  process needs no lock.
- **A timer that expires leases.** No unattended schedulers, and it is the behaviour ADR 0018 rules out.

## Related code

- `internal/gpulease/owner.go`: the record fields, the registry, the orphan marker, `Standing`.
- `internal/gpulease/explain.go`: the waiter's sentence and the installed grace.
- `internal/gpuactivity/snapshot.go`, `holders.go`, `facts.go`: the verdicts and the legacy activity facts.
- `gpu_ownership.go`, `gpu_cmd.go`: the flags, `gpu owner-flags`, `gpu status`.
- `internal/mcpserver/mcpserver.go`: the registry writer. `internal/fleetnode/server.go`: the health keys.

## Related docs

- [GPU lease](../../systems/gpu-lease.md), "Who asked for a lease, and whether they are still there".
- [Fleet node](../../systems/fleet-node.md), the lease block.
- [ADR 0018](0018-machine-wide-fenced-gpu-lease.md), [ADR 0041](0041-the-drain-waits-for-runs-inside-the-queue.md)
  (the verdict vocabulary this extends).
