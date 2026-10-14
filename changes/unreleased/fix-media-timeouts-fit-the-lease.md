
### Fixed - the media timeouts the repository ships are held to the lease window they run under, and the node page and the guide say where that window comes from

- **What the row claimed, and what the tree does.** The open question was a 5,400 s video timeout against a 3,600 s lease.
  The tree has no such pair: every generation route takes the media lease for its own timeout and the render is killed at
  that value (pinned in `internal/pipeline` by the lease-window fragment already in this release), and the 3,600 s
  `DefaultTTL` is only the fallback for a lease taken with no window. The code defaults (`imagegen_timeout_sec` 720,
  `videogen_timeout_sec` 1,500, `audiogen_timeout_sec` 720, `gpu_wait_ms` 90,000) are what `config.example.json` and
  `setup/templates/config.json` carry, and `profiles.json` raises them per tier on purpose: the 5,400 s video seed is sized
  from a measured 5,136 s film render, so cutting it to the lease library's fallback would kill a render that works. No
  seed value and no code default changed.
- **The one way a timeout can still outgrow its window.** A window longer than one lease term (`gpu_max_term_min`, six
  hours by default) is accepted whole but reaches its end only by renewal, and a term that is not renewed is labelled
  expired while the render runs. Nothing held the seeds to that: a tier seed of `videogen_timeout_sec` 30,000, or a seed
  that lowers `gpu_max_term_min` under its own video timeout, loaded without a word.
- **What changed.** `docs/FLEET-NODE.md` now says a fleet media job's lease window is its route's own timeout key on the
  node that runs it (the payload carries none), that `gpu_wait_ms` (90 s by default) is the queue wait and not a lease
  length, and that the window is one term. The guide's engine + lease row names `gpu_max_term_min` as the bound on a
  timeout and tells a `gpu reserve` wrapper to size `--for` from the key of the route it wraps (a seeded 5,400 s video
  wants `--for 90m`).
- **Tests (root package).** `TestSeededMediaTimeoutsFitTheLeaseWindowTheyRunUnder` holds every media timeout the repository
  ships to one lease term: the code defaults, `config.example.json`, the setup template and every tier seed layer in
  `profiles.json` (base and `config_seed_*` overlays, `imagegen_families` and `gen_edit_families` keys included), plus any
  seeded `gpu_wait_ms` to the doctor's 3x vision-wait rule. `TestTheSetupTemplateRestatesTheCodeDefaultsOrNothing` pins the
  template's clocks to `config.Default()`. `TestTheDocsStateWhereAMediaCallsLeaseWindowComesFrom` pins the two doc
  statements above, quoting the 90 s from `config.Default()`.
- **Red-first, honestly.** Only the doc test was red before the docs changed (the guide row did not name
  `gpu_max_term_min`; the node page had no such paragraph). The seed test and the template test are guards, green on the tree
  as it stands, like the pipeline tests of the lease-window fragment; their negative controls (a 5,400 s video under a 60 min
  term, a 7 h image seed, a 600,000 ms `gpu_wait_ms`) are inside the test, and a hand mutation of `profiles.json` turned the
  seed test red.
- **Not changed, and why.** No `internal/config` default needed a change. Resizing a tier seed (an image seed of 3,600 s
  against a render that finishes in minutes on some cards) stays a measurement decision per tier.
