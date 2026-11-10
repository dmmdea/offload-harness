### Fixed - a queued answer's `retry_after_sec` is inside the 30 s its place holds

- **What was wrong.** 0.216.0 added `retry_after_sec` to the `gpu_queued` answer and worked it out as a busy answer does: 30 s under another door call, the holder's remaining declared term under a reserved lease. A place in line holds back later callers for 30 s after its last ask. A caller that slept on the figure came back exactly as its place stopped holding, or, behind a reserved lease, long after the token itself had run out (ten minutes).
- **What it does.** The figure of a queued answer is 10 s at most, a third of the hold. A place is kept by asking again, and the ask itself waits `gpu_wait_ms` in line, so a short figure costs no extra calls. A busy answer's figure is unchanged.
- **Checks.** The existing queued-answer test holds a card for an hour and now also fails when the figure reaches the hold (`TestTheQueuedAnswerStillNamesTheHolderAndWhenToComeBack`).
