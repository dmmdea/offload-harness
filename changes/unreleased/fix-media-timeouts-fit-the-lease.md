
### Fixed - the media timeouts the repository ships are held to the lease window they run under, and the node page and the guide say where that window comes from

- **What the row claimed, and what the tree does.** The open question was a 5,400 s video timeout against a 3,600 s lease.
  The tree has no such pair: every generation route takes the media lease for its own timeout and the render is killed at
  that value (pinned in `internal/pipeline` by the lease-window fragment already in this release), and the 3,600 s
  `DefaultTTL` is only the fallback for a lease taken with no window. The code defaults (`imagegen_timeout_sec` 720,
  `videogen_timeout_sec` 1,500, `audiogen_timeout_sec` 720, `gpu_wait_ms` 90,000) are what `config.example.json` and
  `setup/templates/config.json` carry, and `profiles.json` raises them per tier on purpose: the 5,400 s video seed is sized
  from a measured 5,136 s film render, so cutting it to the lease library's fallback would kill a render that works. No
  seed value and no code default changed.
- **How a window can outgrow its term.** A window longer than one lease term (`gpu_max_term_min`, six hours by default) is
  accepted whole but reaches its end only by renewal, and a term that is not renewed is labelled expired while the render
  runs. Nothing held the seeds to that: a tier seed of `videogen_timeout_sec` 30,000, or a seed that lowers
  `gpu_max_term_min` under its own video timeout, loaded without a word. That is the single-job case, and it is what the
  test below holds. The other case is an image batch: its window is the per-image timeout times its jobs and nothing caps
  the job count, so a batch of more than `gpu_max_term_min` divided by the per-image timeout images (more than six at the
  seeded 3,600 s image timeout) asks for a window above the term whatever any seed says. This change states that limit in
  the guide; it does not cap the job count, which is a pipeline change.
- **What changed.** `docs/FLEET-NODE.md` now says a fleet media job's lease window is its route's own timeout key on the
  node that runs it (the payload carries none), that `gpu_wait_ms` (90 s by default) is the queue wait and not a lease
  length, and that the window is one term, as a single job's timeout (a node renders one job per ticket and there is no
  batch task on the fleet, so the batch product never reaches a node). The guide's engine + lease row names
  `gpu_max_term_min` as the bound on a single job's timeout, names the image batch and its uncapped job count as the case
  that bound does not cover, and tells a `gpu reserve` wrapper to size `--for` from the key of the route it wraps (a seeded
  5,400 s video wants `--for 90m`). The node page points at the guide's timeout chain section in prose, not through an
  anchor.
- **Tests (root package).** `TestSeededMediaTimeoutsFitTheLeaseWindowTheyRunUnder` holds every single-job media timeout the
  repository ships to one lease term: the code defaults, `config.example.json`, the setup template and every tier seed layer in
  `profiles.json` (base and `config_seed_*` overlays, `imagegen_families` and `gen_edit_families` keys included), plus any
  seeded `gpu_wait_ms` to the doctor's 3x vision-wait rule. `TestTheSetupTemplateRestatesTheCodeDefaultsOrNothing` pins the
  template's clocks to `config.Default()`. `TestTheMediaTimeoutKeyListMatchesTheConfig` reflects over `config.Config` and
  fails when a `*_timeout_sec` key is in neither the media list nor the list of clocks that are not lease windows, or when a
  listed key is gone from the config, so a new generation route cannot slip past the seed test (the control that used to
  claim this could not fail, because the code-default entries marked every key as seen; it is gone).
  `TestTheDocsStateWhereAMediaCallsLeaseWindowComesFrom` pins the doc statements above, quoting the 90 s from
  `config.Default()`.
- **Red-first, honestly.** The doc test was red before the docs changed (the guide row did not name `gpu_max_term_min` or the
  batch case; the node page had no such paragraph), and the key-list test was red against a stub (its two controls, a new
  `*_timeout_sec` key and a dropped listed key, found nothing). The seed test and the template test are guards, green on the tree
  as it stands, like the pipeline tests of the lease-window fragment; their negative controls (a 5,400 s video under a 60 min
  term, a 7 h image seed, a 600,000 ms `gpu_wait_ms`) are inside the test, and a hand mutation of `profiles.json` turned the
  seed test red.
- **Not changed, and why.** No `internal/config` default needed a change. Resizing a tier seed (an image seed of 3,600 s
  against a render that finishes in minutes on some cards) stays a measurement decision per tier. A job-count ceiling for the
  image batch (clamp or split it at `gpu_max_term_min` divided by the per-image timeout) is not added here: it belongs in
  `internal/pipeline`, and until then the limit is stated, not enforced.
