---
status: Accepted
date: "2026-09-14"
---

# A write-capable delegation door, default off

## Context

The delegation lane could only read. A contract inlined its context docs, a seat reasoned over them,
and a typed result came back; the write tools (`write_file` / `edit_file` / `delete_file`) existed only
behind `local-agent --allow-write`, an operator running a CLI against their own worktree. So every
implementation leg — "fix this off-by-one", "add this table-driven case" — stayed with the delegating
session even when the reading around it did not.

The obstacle was never the tools. It was that a delegated write has nowhere safe to land. The contract
is **self-contained by construction**: the node materializes its own copy of the inline docs and never
reaches back into the delegator's filesystem. That is the property the whole lane's isolation rests on,
and any door that let a remote seat write to the caller's disk would spend it.

The seats are small (a 4B, a 9B, a 27B). They omit rather than invent, which makes them good at bounded
work and bad at unbounded work. A door they can use has to be narrow enough that a wrong answer is
visible and cheap.

## Decision

A contract may carry `write_root`: a directory, **relative to the run's read root**, that the seat may
create and change files under. Absent (the default, and every contract written before this) means
read-only, with no write tool registered at all.

1. **Relative, not absolute.** An absolute delegator-box path names nothing on the executing node. A
   relative root resolves to an absolute directory inside the read root on whichever box runs it — the
   containment the door needs — enforced by `os.Root` at the syscall layer rather than by comparing two
   path strings.
2. **The node opts in: `agent_allow_write`, default false.** A node that has not opted in refuses at
   ACK, which makes the delegator re-place the contract on a node that has. The in-process local path
   has no ack hop and defers with `defer_class: "write"` instead — neither a broken stack (nothing is
   wrong with the box) nor an unplaceable contract (another node may well have opted in).
3. **The write set comes back as a unified diff and the harness applies none of it.** The node
   snapshots the write root before and after the run and diffs the two trees. A tree snapshot, not an
   effect ledger of the tool calls: the ledger records that `write_file` ran, not what the bytes became,
   and a file the seat wrote without being asked to is invisible in it.
4. **Writes only.** `AllowWrite` + `AllowOverwrite` and nothing else — no delete, no shell, no `run`,
   no fetch, no github — narrowed to the `edit` profile, which advertises exactly the six tools the door
   grants and does not list `delete_file`. Overwrite is on because a seat that cannot change an existing
   file cannot do an implementation leg at all; it is safe because the tree it overwrites is the node's
   own throwaway copy.
5. **Fixed caps, enforced twice.** Eight files, 64 KiB written, 192 KiB of rendered diff. `WriteLimit`
   refuses the offending call at the tool — the model sees "NOT performed" and can correct — and the
   finished write set is re-counted before it crosses the wire. A breach publishes **no** diff.
6. **Acceptance learns to read the write set**: `diff_touches:<path-prefix>` and `diff_max_files:<n>`,
   both failing closed on an empty write set.

## Consequences

The delegating session keeps the judgment — is this change right? — and gives away the typing. A seat
that describes the change instead of making it is legible as exactly that (`write_note`: "the seat wrote
nothing") rather than as a green result whose write set is silently empty.

`AcceptanceCheck.Eval` now takes the whole `AgentWireResult` rather than `(structured, output)`. One
non-test caller changed; the alternative — a second method that silently skipped diff checks — is the
shape that lets an acceptance check verify nothing while reading as verified.

The intake lint still calls a diff-only acceptance SHAPE-ONLY, and that warning is accurate: a touched
path is not a correct change. The operator reading the diff is the real gate, and nothing here pretends
otherwise. `verified: true` on a write contract means a path was touched and the seat said something.

Caps are fixed rather than per-contract. A caller cannot raise them — that is what a door is — and does
not need to lower them, because `diff_max_files:1` already says "this leg may touch one file".

A write the seat cannot fit into ONE completion is a BUDGET defect, not a broken node (register D-114): the
engine refuses a tool call whose JSON argument the step budget cut mid-string, the loop re-issues that step
once at the final budget and, cut again, stops on `tool_call_cut` with both budgets and the partial argument
size in `stop_note` - so the fix reads as "ask for a smaller write, or raise the step budget" instead of
blaming the stack. An engine that returns the cut completion instead of refusing it is recognised by the
argument, not the finish reason: vLLM reports a call cut at the cap as `tool_calls` (0.140.2), so a
non-parsing argument at the cap, or one that ends mid-value, takes the same path. `contracts/write-door/t4` is that shape as a gate leg.

A write contract dispatched to a node that has not opted in costs one wasted round trip before it is
re-placed. Advertising `agent_allow_write` on `/fleet/health` would remove that, at the price of
changing a health wire pinned across a staggered fleet; the re-placement path already reaches the right
node, so the advertisement waits until the round trip is measured to matter.

## Alternatives considered

- **Apply the diff harness-side.** Rejected outright. The reason a small seat can be trusted with this
  work is that a human reads what it produced before it touches anything real. Applying it removes the
  only control that matters and adds nothing the caller cannot do with `git apply`.
- **An absolute `write_root`, checked to be inside `read_root`.** The shape the work was briefed with.
  It cannot work on the wire: the contract is self-contained and the node has never seen the
  delegator's filesystem, so the absolute path would name nothing there. The relative form gives the
  same invariant and gets it from the kernel instead of from a string comparison.
- **Confine with Landlock (Linux) and the job object (Windows).** Those cage a CHILD PROCESS. The write
  door spawns none — the writes are in-process — so neither has any bearing on it. `internal/sandbox`
  stays what it has always been: the cage for `run_shell` and `run`, neither of which this door grants.
- **`filepath.EvalSymlinks` + `filepath.Rel` to verify the write root.** Written, tested, and thrown
  away: Go reports a Windows junction as an ordinary directory, so `EvalSymlinks` does not follow it and
  the comparison passed an escape it was written to catch — green on Linux, inert on Windows. `os.Root`
  refuses reparse points on both.
- **Per-contract cap overrides.** Looser is clamped away and tighter is already expressible in
  acceptance. Two knobs for one job is how they drift apart.
- **A dedicated `write` profile.** `edit` already lists exactly the right six tools.

## Related code

- [../../../internal/core/agentwire.go](../../../internal/core/agentwire.go) — `write_root`,
  `ValidateWriteRoot`, the caps, `defer_class: "write"`, the two diff acceptance verbs.
- [../../../internal/pipeline/agentwrite.go](../../../internal/pipeline/agentwrite.go) — the node-side
  door: opt-in check, confinement, snapshot, cap.
- [../../../internal/writedoor/writedoor.go](../../../internal/writedoor/writedoor.go) — tree snapshot
  and unified-diff rendering.
- [../../../internal/agent/writelimit.go](../../../internal/agent/writelimit.go) — the tool-level budget.
- [../../../internal/fleetnode/tasks.go](../../../internal/fleetnode/tasks.go) — the ACK refusal.

## Related docs

- [../../FLEET-NODE.md](../../FLEET-NODE.md) — `agent_allow_write` and what opting in grants.
- [../../OPERATOR-GUIDE.md](../../OPERATOR-GUIDE.md) — delegating an implementation leg.
- [0036-the-agent-lane-is-a-harnessed-environment.md](0036-the-agent-lane-is-a-harnessed-environment.md)
  — the environment this door is a capability of.
