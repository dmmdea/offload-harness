### Changed - a host short of memory for the agent seat's cold load is a place in line, not a refusal

- **What was wrong.** A contract that finds the agent seat unloaded asks the host first (the seat's host-RAM gate). When the
  host was short the gate looked once, or settled for at most a minute and stopped after 10 s without progress, and the
  contract came back deferred. The memory comes back: on the reference 3-card Windows box a contract pinned to the local seat
  spent its 300 s admission budget behind another session's model swap, met the host 2.9 GiB short (`needs 36.0 GiB, only
  41.1 GiB of 127.7 GiB physical is available, 8.0 GiB headroom`), was refused after one look, and four minutes later the
  host had 66.5 GiB available. A live lane that declared host RAM refused the seat outright in the same way. A busy,
  reserved, fenced or occupied seat was already a place in line; a host short of memory was the one shortage that was a
  verdict.
- **What it does now.** Who waits depends on whether the caller has a line (`delegate.hasLine`: not `route=local`, remotes
  configured, the capacity wait not switched off, the run not sheddable).
  - With a line, the delegator asks the host the door's own question before it deals the seat a subtask
    (`delegate.localHostShort`, the same need the door reads, measured loads included). A seat the host cannot load now
    reads like an occupied one: the subtask goes to a remote that can take it, or stays in the capacity wait, which re-reads
    the host every tick and takes the seat the moment it admits. The door then looks once. If the two readings part (the
    host moved between the looks) the door's host-RAM defer is one more tick of the wait, at most four per subtask and not
    more often than the refusal cooldown.
  - Without a line (a pinned local route, a box with no remotes, a sheddable run, a run on a decided seat, the `agent_run`
    door) the door itself is the line: it re-reads the host every tick for as long as the admission budget lasts, with no
    no-progress stop (`gpulease.Manager.HostRAMAwait`), and a live lane that declared host RAM is a reason to keep waiting.
    When the swap pre-flight had already spent the budget the host gets one budget of its own. The admission deadline moves
    out by the time the door waited, so the cold load that follows keeps its whole budget instead of running inside the
    contract's wall.
  - Both doors write the defer with one constant (`core.HostRAMReason`), and the delegator recognises it by that constant.
- **What did not change.** The rule itself: the seat is loaded only when the available memory, less the seat's need, less
  what live leases have yet to touch, leaves the headroom, and never while a lane that declared host RAM is live. A fleet
  node serving a remote delegator settles as before (the option is absent on the wire). A need no state of the host could
  meet is still answered at once.
- **Check.** On the reference box, the released build, earlier the same day: a contract pinned local was deferred after one
  look with the sentence above. With this change, on a host staged short for 75 s (a process holding 23 to 29 GiB so that
  38 GiB stayed available for a 36 GiB need with 8 GiB of headroom), seat unloaded:
  - pinned local (the door is the line): after 75 s short the contract was still waiting; then `succeeded 1, deferred 0`,
    `admission_note: waited 1m16s for host RAM before the seat's cold load; cold load exceeded the admission budget after
    300s (proceeding into the wall)`, output `pong`. The cold load kept its whole 300 s after the wait.
  - route auto with no remote able to take it (the delegator's line, the capacity wait set to the order of the MCP door's
    call deadline): `succeeded 1, deferred 0`, `capacity_wait_sec 62.4`, placement `local seat could not take its cold load
    for want of host RAM, which came back after 1m2s, running local (capacity wait)`, output `pong`.
  - the same contract through the command-line door's default two-minute capacity wait, while the first contract's cold
    load was still running, was deferred at two minutes: a wait shorter than a cold load is the door's own bound and is
    not changed here.
  The two route-auto runs predate the last review fixes of this change (one answer for "has a line", the admission
  deadline moving out); the pinned-local run is on the final code. Before the deadline moved, the same pinned-local run
  left the warm-up 224 s.
