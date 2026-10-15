### Fixed - the compose "running card" tests no longer race the client's first poll: the fake node holds the render until a poll has found it running

`TestRemoteComposeCardTurnsRunningWhenTheNodeSaysTheJobStarted` failed once in CI on an unrelated change
(`cards = map[completed:... queued:...], want queued, running, completed`) and passed 40 of 40 on a workstation, on main and
on that branch. The product was right: a job that begins and ends between two polls is never seen running, and its card
rightly never turns running. The tests were wrong.

- **Root cause.** The fake node's render took a fixed time (600 ms) and the client polls once straight after dispatch, then
  every second. When that first poll reaches the node before its scheduler has claimed the job it reads "queued"; the next
  poll, a second later, finds the render over, so `h.Running()` never fires and no running frame is written. The node claims a
  job in a goroutine of its own, so a loaded box can lose that order and a quiet one almost never does. The held-defer test
  had the same shape with a 300 ms render, but its assertions accepted both outcomes: it passed either way and checked a
  different branch from run to run (the "no running frame" half only ran when the race was lost).
- **The change (test code only; no production code changed).**
  - The node's render no longer takes a fixed time. It waits until the node has sent one poll answer saying "running" (the job
    is already running on the node's books while it is held), bounded at 20 s so a client that never gets there fails its test
    and does not hang it. The render-time field of the fake runner is gone.
  - The node can answer the client's first poll as a node whose scheduler had not claimed the job yet would (queued, whatever
    the job's real state). The running-card test runs that case on every run, beside the poll order it gets by itself, so the late
    claim is covered and not left to the load.
  - The held-defer test runs each of its three err_classes in both orders and asserts the one that applies: a poll found the job
    running (a running frame is required, and the close keeps its start), or none did (the node reports the running job as queued
    throughout: no running frame, no start for a held call, a start for a call that ran and broke). The second order was already
    asserted but ran only by accident; it now runs by construction.
- **Other tests of the same shape.** None. The text, vision and stt lanes answer "running" from a fake server that decides it
  (`runningFirst`), and the media lane already holds its render on a channel until the client has polled.
- **Proved on a Linux node (the compiled test binary run in stress mode: W concurrent copies by N runs each).**
  - **Not reproduced by load alone.** The unfixed tests passed 400 of 400 runs (8 workers by 50 runs, the two tests) and 600 of
    600 under starvation (24 workers by 25 runs, four CPU spinners, `GOMAXPROCS=1`, 16-core idle node). The failure needs the
    first poll to beat the scheduler, which a CI runner did once and this node never did.
  - **Red, deterministic.** The unfixed running-card test with the first poll answered queued: 80 of 80 runs failed (8 workers by 10
    runs, `-test.run=^TestRemoteComposeCardTurnsRunningWhenTheNodeSaysTheJobStarted$`), each with the CI message (queued and
    completed frames, no running one).
  - **Green.** The same command on the fixed tests: 0 failures in 80 runs. The two tests under the starvation shape above: 0
    failures in 600 runs.
  - **Mutant.** Only the fix reverted (the held render goes back to a fixed 600 ms), hook and assertions kept: the same two
    commands failed 80 of 80 and 600 of 600 runs, each on the subtest where the first poll reads queued, with no running poll
    served.
