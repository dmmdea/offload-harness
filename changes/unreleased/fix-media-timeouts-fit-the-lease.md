
### Fixed - the guide and the node page say where a media call's lease window comes from and what holds it

- **The open question.** A 5,400 s video timeout against a 3,600 s lease. The tree has no such pair: every generation
  route takes the media lease for its own timeout and the render is killed at that value (pinned in `internal/pipeline` by
  the lease-window fragment already in this release), and the 3,600 s `DefaultTTL` is only the fallback for a lease taken
  with no window. The code defaults (`imagegen_timeout_sec` 720, `videogen_timeout_sec` 1,500, `audiogen_timeout_sec` 720,
  `gpu_wait_ms` 90,000) are what `config.example.json` and `setup/templates/config.json` carry, and `profiles.json` raises
  them per tier on purpose. No seed value and no code default changed.
- **What holds a media lease, read in the code.** The acquirer stamps the lease's end as now plus its whole window: for a
  generation call that is the route's own timeout, or the per-image timeout times the jobs of a batch
  (`record` in `internal/gpulease`, `RunImageBatchWaiting` in `internal/pipeline`). The window is granted whole, whatever its length.
  `gpu_max_term_min` is not a bound on it. Taking a lease only records the term (`stampTerm` writes `TermMs`,
  `RequestedMs` and `MaxTotalMs`), and the term is read only as renewal arithmetic (`nextEnd`) by the term check
  (`AdvanceTerm`), whose one caller is the tick of a `gpu reserve` or `gpu hold` holder. A pipeline media lease never runs
  that check, so nothing labels it expired, and the yield asker, which also lives only in those holders and needs an
  expired or due term, never asks it to yield. What keeps it is its heartbeat, renewed every 15 s while the render runs:
  a lease whose holder is alive is reclaimable only when its heartbeat is stale and its window has ended. Past the window the same timeout is the
  render's own deadline, so the render is killed and the lease released. Only an explicit takeover, or the death of the
  process that holds the lease, frees the card sooner.
- **The cost of a long timeout.** How long a wedged render holds the machine-wide media lease: the seeded figure, up to
  3,600 s for an image and 7,200 s for a video. That is the only cost, and the guide already says to size a resize from
  the seeded figure. An image batch is no different: its window is the per-image timeout times its jobs, granted whole,
  kept by the same heartbeat. Whether to resize a tier seed stays a measurement decision per tier.
- **What changed.** `docs/FLEET-NODE.md` says a fleet media job's lease window is its route's own timeout key on the node
  that runs it (the payload carries none), that the window is granted whole and kept by the job's heartbeat, that
  `gpu_wait_ms` (90 s by default) is the queue wait and not a lease length, and that `gpu_max_term_min` does not bound it.
  The guide's engine + lease row says the same, names `gpu_max_term_min` only as the renewal term of a `gpu reserve` hold
  (renewed by that hold's own holder), and tells a `gpu reserve` wrapper to size `--for` from the key of the route it wraps
  (a seeded 5,400 s video wants `--for 90m`). The node page points at the guide's timeout chain section in prose, not
  through an anchor.
- **Tests (root package).** `TestTheDocsStateWhereAMediaCallsLeaseWindowComesFromAndWhatHoldsIt` requires the statements
  above in the guide row and in the node page paragraph (quoting the 90 s from `config.Default()` and all seven route
  keys) and refuses, by name, the retired claim that a media timeout must fit one lease term.
  `TestTheMediaTimeoutKeyListMatchesTheConfig` reflects over `config.Config` and fails when a `*_timeout_sec` key is in
  neither the media list nor the list of clocks that are not lease windows, or when a listed key is gone from the config, so
  a new generation route cannot slip past the docs and template checks. `TestTheSetupTemplateRestatesTheCodeDefaultsOrNothing`
  pins the template's clocks to `config.Default()`. `TestSeededGPUWaitsPassTheDoctorsVisionWaitRule` holds every shipped
  `gpu_wait_ms` to the doctor's 3x vision-wait rule, with a blind-gate count and its negative controls.
- **Red-first.** The docs test was committed red against the docs as they stood before this fix (the guide row and the
  node page lacked "granted whole" and the heartbeat, and still carried the term-bound and batch-cap wording), with the other
  three tests of the file green in that commit; the docs commit that follows turns it green.
