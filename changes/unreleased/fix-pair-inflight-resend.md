### Fixed - a PAIR card refused while PAIR is down is sent again, and a job that finished reads "Ran on", not "Sent to"

- **What it does.**
  - **Resend.** The workload emitter keeps the latest `queued` or `running` frame of each job whose post
    failed. It posts that frame again every 10 s, and at once after any post that lands. It stops when
    the frame is delivered, when the job's next frame supersedes it, or after `OpenMaxAge`. A frame
    PAIR rejects (400/413/422) is not retried.
  - **Delegation start.** A delegation that completed carries a start time even when no poll caught
    its seat working. The start is the completion time minus the node's `wall_ms`, never before the
    card was created, and the creation time when the node reports no wall.
  - **Transcription start.** A successful transcription card carries its creation time as its start,
    because its lane never marks the card working. Held-back calls and failures keep a null start.
- **Why.** On 2026-11-09 PAIR was started while a delegation was already running on a remote node.
  Both in-flight frames had been refused while PAIR was down, and nothing sent them again. The node
  ran at full load for 13 minutes with no card, no line and no activity ring. Separately, PAIR's
  desktop labels a card with a null start "Sent to <node>", so completed transcriptions, and
  delegations short enough to finish between two polls, read as jobs that never ran.
- **Check.** `TestInFlightFrameRefusedWhilePairIsDownIsResent` reproduces the outage. It posts a
  running frame to an ingress that answers 503, then lets the ingress recover. It asserts the frame
  arrives exactly once, unchanged, and that the job still closes normally. The delegate PAIR card
  tests pass with `TestRemoteCardTurnsRunningOnWork` updated: a job that only ever reported loading
  and then answered sends no running frame, but its terminal frame now carries a start. Details:
  `docs/systems/pair-workloads.md`.
