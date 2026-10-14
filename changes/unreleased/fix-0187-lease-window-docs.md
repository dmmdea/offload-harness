### Fixed - The operator guide's engine + lease row says what the code does: a media call's lease window is its own timeout (register C-33)

- **What the row said.** "A 5,400 s video run outlives the default lease; size the lease `--for` window to the job", with
  the 3,600 s lease TTL set "against" the media timeouts, one deployed machine's figures (`imagegen_timeout_sec` 600,
  `videogen_timeout_sec` 5,400, `gpu_wait_ms` 600,000) quoted as if they were the defaults, and `gpu_wait_ms` read as a
  lease length. The doctor paragraph called the 600,000 ms "the deployed" value.
- **What the code does** (read in `config.go`, `pipeline.go`, `mediaadmit.go` and `gpulease.go`, not taken from the
  row). A media call acquires its lease for the call's own timeout (`timeout` is passed to `acquireMediaLease` as the
  TTL, on the card-scoped writer and on the whole-node one) and the render is killed at that same value, so a render
  cannot outlive the lease the pipeline took for it; a batch takes the per-image timeout times its jobs. The lease
  library's `DefaultTTL` (3,600 s) is only the fallback for a lease acquired with no window, which no media call is.
  `gpu_wait_ms` (code default 90,000) is the queue wait, not a lease length. The code defaults are `imagegen_timeout_sec`
  720 and `videogen_timeout_sec` 1,500. The one place a window can be shorter than the job is a `gpu reserve` wrapper: a
  call run inside it inherits the wrapper's lease and its `--for` window (default 45 minutes), and a window that ends
  first is renewed or labeled expired, never a release.
- **What changed.** The row and the doctor sentence now say that, in code defaults, with the wrapper window named as the
  one exception. Nothing in the code changed, and the tier seeds in `setup/templates/profiles.json` are untouched
  (`imagegen_timeout_sec` 3600 on the 16 GB-class tiers is a deliberate quality length; because the window equals the
  timeout, any resize is a measurement decision about how long a wedged render may hold the machine-wide media lease).
  `docs/tiers/*.md` were read for the same claim and carry none.
- **Tests.** `TestOperatorGuideLeaseRowStatesTheMediaLeaseIsTheTimeout` and
  `TestOperatorGuideQuotesCodeDefaultsForMediaTimeouts` (internal/pipeline) were red on `main` and are green here: the
  row must exist (blind-gate control), refuse the four retired claims, say what the code does, and quote only figures
  that equal `config.Default()` and `gpulease.DefaultTTL`. `TestAVideoCallsLeaseWindowIsItsOwnTimeout` and
  `TestAnImageCallsLeaseWindowIsItsOwnTimeout` are green guards on both lease paths (video 5,400 s gives a 5,400 s
  window, image 100 s gives a 100 s window, neither the 3,600 s fallback). Mutations, each restored byte for byte: image
  TTL to zero, video TTL to zero, a one-hour clamp in `acquireMediaLease`, the card-scoped TTL to zero and the whole-node
  TTL to zero each turn exactly the matching subtests red; the row mutations (a code default replaced by the old figure,
  the queue-wait sentence removed) turn the doc pins red.
