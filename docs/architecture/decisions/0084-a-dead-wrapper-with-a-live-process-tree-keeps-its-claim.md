---
status: Accepted
date: "2026-10-12"
---

# A dead wrapper with a live process tree keeps its claim

## Context

ADR 0018 judges a lease holder by two facts the holder writes about itself: its pid is alive, and its heartbeat is
recent. A holder whose pid is gone is reclaimed at once, because "provably gone" is the one case in which taking the
card cannot hurt the holder. For a wrapper (`gpu reserve ... -- <command>`) that inference is wrong. The wrapper is
not the job: the command it started, and what that command started, run on the cards. A wrapper that is killed,
crashes, or loses its machine's session leaves the job running on a card that is truly in use, and the next acquirer
was handed that card the moment the pid disappeared. Two jobs on one card is the incident the lease exists to
prevent, reached through the one door the reclaim rule left open.

The same gap showed up on the way out. When a wrapper lost its lease it stopped its direct child and nothing else, so
a grandchild (a launcher's render server, a script's worker) kept the card the same way. And nothing could enumerate
what a wrapper had started, so a wrapper that died took with it the only record of what held the card.

The operator's standing orders bound the answer. Nothing in the harness may kill a job on its own: no watchers, no
timers that act, fail loud over autonomous recovery, destructive action only on an explicit command. A claim must
also never be held by a guess: a pid recycled for an unrelated process must not keep a card, and a card must not stay
claimed after its job is gone. Reader-first rollout (invariant I7): a change to what a reader concludes must not make
an older reader silently wrong in a new way.

## Decision

**A lease whose wrapper is gone is not reclaimable while a process of the tree it started is alive, by pid and start
identity.** The wrapper records that tree in its lease record (`tree`: pid and start identity of each member, additive
and `omitempty`), when the set changes. A reader that finds the wrapper gone (its pid dead, or alive but a different
process) keeps the claim while any recorded member is still the recorded process, and the lease reads `tree-orphan`,
the verdict that leads the precedence. The heartbeat-and-window conjunction for a wrapper that is alive is untouched.
A lease that recorded no tree has no record to hold it: it is free at once unless the other evidence below ties a live
process to it, and with none alive it is reclaimed exactly as before.

**The claim is bounded by observation, never by a timer.** Every read checks the members again. A recycled pid cannot
hold a card (its start identity differs); a card frees itself at the first read after its members are gone; hours past
its window and heartbeat a tree-orphan is still held while its members live. Nothing in this change kills a process.

**The evidence is the record first, then what ties a process to the lease.** Only while the wrapper is gone and no
recorded member is alive, a reader also takes a ComfyUI launch marker that names the lease epoch (an instance can sit
outside the wrapper's job), a default-instance marker for a lease that recorded no tree when it began at or after the
lease and no other lease record exists, and, for a lease that recorded no tree, the children of the dead wrapper's pid in
the process table (Windows keeps the parent link; a recycled wrapper pid is never walked). The driver's per-process
compute listing is not evidence: under WDDM it names no process for most of them, and a process on a card is not
attributable to a lease by itself. Such a process is reported (`card still busy by pid N (not ours)`) and never touched.

**The wrapper contains what it starts.** A shared `internal/proctree` runs the command in a job object (Windows: the
child is created suspended, assigned and resumed, so nothing escapes before it is inside) or in its own session and
process group (Unix: `kill(-pgid)`; `PR_SET_PDEATHSIG` is per thread and covers the direct child only, so it is not
used). A lost lease or an interrupt stops the whole tree, except a live ComfyUI instance whose launch marker names
another lease: a kept instance another lease reused is that lease's job now. The agent shell's cage builds on the same
job object type, so there is one implementation.

**Letting go of the tree leaves it running, unless asked.** `--die-with-wrapper` (kill-on-close on Windows) is opt-in and
off by default, because it makes the wrapper a single point of failure for a long render, and a platform with no handle
whose closing ends a tree refuses it up front. A command the OS will not put in the job is refused too when the flag was
asked for (the guarantee cannot be given), never run uncontained with a warning. One carve-out: a process the wrapper
spares because another lease took it over (above) is not ended by the close either, so the job stops killing on close once
something is spared, after everything else of the tree has been ended, and the wrapper says so.

**The stop primitives a takeover needs are built here and wired to nothing.** `proctree.PlanStop` and `Stop` stop only a
recorded tree or a launch marker's process, re-check each member's start identity right before signalling it, and return
one outcome per member so a caller that frees a claim can require the tree verified dead. `comfyinst.StopMarker`
interrupts, frees and kills a marker's instance after the proof a lease's own holder gives.

## Consequences

- A dead wrapper with a live job no longer double-books a card. A waiter is told which lease, what still runs and the
  one command that frees it today, `gpu release --epoch N` (it ends the claim at once and stops only the ComfyUI
  instances kept under the lease; the rest of the job keeps running on the card, so it is run by a human who has seen
  the job is dead or disposable). The explicit takeover that also stops the job is a later change and is not offered for
  a tree-orphan. `/fleet/health` carries `tree_orphan` on the lease entry (busy like any held lease: past its declared window it is ranked last and
  is not a refusal, the rule every overdue lease keeps), the host-RAM guard measures what it has loaded from its live members, and a
  `--cards N` waiter is handed another card.
- The rule holds where every reader is this build. A reader that predates it ignores `tree` and reclaims a dead wrapper's
  lease at once, as it always did, and the preserved old-reader fixture pins that. The change adds a key an older reader
  ignores and moves no audit line: an older reader is no worse than before, and `gpu doctor` already lists the readers
  older than the record format.
- A hung job whose wrapper died keeps its card until it ends or an operator runs `gpu release --epoch N`. That is
  deliberate: freeing a card under a live process is the incident, so the release is an explicit command and the job is
  left running by it; what happens to an overdue or orphan tree by policy (`gpu.on_overdue`) is a decision for after this
  ships.
- The recorded tree is at most one tick (15 s) behind while the wrapper lives, and frozen when it dies. A lease that recorded a
  tree is held by that record and by a marker that names its epoch; the process table is read only for a lease that recorded
  none, so a process started after the last record that outlives every recorded member is not held. That window is the accepted
  limit of recording rather than enumerating. A wrapper alive but wedged (alive pid, stale heartbeat, window over) is still
  reclaimed by the conjunction: this change is about a wrapper that is gone.
- A host is protected only once every reader on it is this build: restart the MCP servers and the fleet daemon after the
  deploy and compare build versions with `gpu doctor` (the audit's format signature is not bumped, because the change is
  a reader improvement and an older reader is no worse than before).
- A lease taken by the harness's own pipeline records no tree; a render child that outlives a crashed server is found by the
  process-table walk on Windows and by the marker for a keyed instance, and on Linux by the marker only.
- On Windows a lost lease now ends the grandchild too. If another lease took over a kept instance in the 15 s window, the
  wrapper spares it and says so; without that rule the stop would have killed that lease's job.

## Alternatives considered

- **Record the tree in a sidecar file.** It avoids the epoch lock, but a reader would then read two files for one fact and
  a legacy reader's view of the record would diverge from the sidecar's. The record is rewritten only when the set changes.
- **Make the wrapper's death kill the tree.** It is the simplest way to avoid an orphan and the wrong default: a wrapper
  that is a single point of failure for a twenty-hour render loses the render to a crashed shell. It exists as an opt-in.
- **Hold a claim for a bounded time after the wrapper dies.** A timer that decides is the behaviour ADR 0018 rules out, and
  it frees a card under a live job at the end of the timer.
- **Use the driver's compute listing as the evidence.** It cannot attribute a process to a lease, and under WDDM it names
  none. It is used to report a process that is not ours, never to hold a claim.
- **Assign the wrapper itself to the job.** Descendants would inherit, but terminating the job would end the wrapper, and
  with kill-on-close the wrapper's own exit would be the trigger. The command is started suspended and assigned instead.
- **A named job object a reader opens.** It would list late-spawned members exactly, but a name in another session's
  namespace needs a privilege across security contexts, which is the boundary the lease readers must work across.

## Related code

- `internal/gpulease/tree.go`, `gpulease.go` (`Reclaimable`), `devices.go` (the reader), `explain.go`, `owner.go`: the rule, the evidence and the standing.
- `internal/proctree`: the containment (`job_windows.go`, `job_unix.go`) and the stop primitives (`stop.go`).
- `gpu_cmd.go`, `gpu_tree.go`, `gpu_ownership.go`: the wrapper's containment, recording and tree stop, and `gpu status`.
- `internal/modelaffinity/scoper.go` (`SetComfyDir`): arms the ComfyUI directory the launch markers are read from, for the
  lease readers as well, from the one call `config.loadArmed` already makes.
- `internal/comfyinst`: `StopMarker`, `PIDsOfOtherLeases`. `internal/sandbox/sandbox_windows.go`: the cage on the shared job object.
- `internal/gpuactivity/holders.go`, `snapshot.go`, `internal/fleetnode/leases.go`: the verdict and the fleet entry.

## Related docs

- [GPU lease](../../systems/gpu-lease.md), "A dead wrapper with a live tree keeps its claim".
- [ADR 0018](0018-machine-wide-fenced-gpu-lease.md), [ADR 0070](0070-leases-have-terms-owners-renew-orphans-are-surfaced.md)
  (the verdict vocabulary this completes).
