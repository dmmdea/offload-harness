### Fixed - The operator guide's engine + lease row says what the code does: a generation route's lease window is its own timeout (register C-33)

- **What the row said.** "A 5,400 s video run outlives the default lease; size the lease `--for` window to the job", with
  the 3,600 s lease TTL set "against" the media timeouts, one deployed machine's figures (`imagegen_timeout_sec` 600,
  `videogen_timeout_sec` 5,400, `gpu_wait_ms` 600,000) quoted as if they were the defaults, and `gpu_wait_ms` read as a
  lease length. The doctor paragraph called the 600,000 ms "the deployed" value. The first rewrite fixed that but was
  still wider than the code in three ways, which review caught: it said every "media call" leases for its own timeout
  (two lanes take no media lease), it named two of the seven timeout keys, and it quoted only the code defaults, where a
  seeded install runs much longer ones.
- **What the code does** (read in `config.go`, `pipeline.go`, `igpumedia.go`, `ttsendpoint.go`, `mediaadmit.go` and
  `gpulease.go`, not taken from the row). Every generation route that takes the media lease (image, sdcpp image, edit,
  inpaint, upscale, batch, run-graph, video, animate, audio, and the iGPU video, animate and audiocpp lanes) acquires it
  for the call's own timeout (`timeout` is passed to `acquireMediaLease` as the TTL, on the card-scoped writer and on the
  whole-node one) and the render is killed at that same value (`gpugen.Spec.Timeout`), so a render cannot outlive the lease
  the pipeline took for it. Each route reads its own key: `imagegen_timeout_sec` (image, sdcpp image, batch, run-graph),
  `videogen_timeout_sec`, `inpaint_timeout_sec`, `upscale_timeout_sec`, `gen_edit_timeout_sec`, `animategen_timeout_sec`,
  `audiogen_timeout_sec`. A batch takes the per-image timeout times its jobs. The two exceptions take no media lease: the
  `tts_endpoint` voice lane (the speech server owns its card) and a `pipeline-job` (only the in-process media slot; the
  stages it runs lease for themselves). The lease library's `DefaultTTL` (3,600 s) is only the fallback for a lease
  acquired with no window, which no generation route does. `gpu_wait_ms` (code default 90,000) is the queue wait, not a
  lease length. The code defaults are `imagegen_timeout_sec` 720 and `videogen_timeout_sec` 1,500. The one place a window
  can be shorter than the job is a `gpu reserve` wrapper: a call run inside it inherits the wrapper's lease and its
  `--for` window (default 45 minutes), and a window that ends first is renewed or labeled expired, never a release.
- **What a seeded install runs.** The tier seeds in `setup/templates/profiles.json` are not the code defaults:
  `imagegen_timeout_sec` is 3,600 on nine seeds (blackwell-16, blackwell-32, blackwell-2x16, blackwell-3x16,
  blackwell-48, blackwell-72, ampere-16, volta-16 and the high-RAM arm of ampere-8), 2,400 on blackwell-8 and 900 on
  ampere-6 and the AMD tiers; `videogen_timeout_sec` is 5,400 on those tiers except ampere-8 (which seeds no video), 4,500 on
  blackwell-8 and 7,200 on amd-gcn. Because the window equals the timeout, the seeded value is how long a wedged render holds the
  machine-wide media lease: an hour for an image on those nine seeds. The repo records no decision that makes that a lease
  length. The 3,600 s and 5,400 s seeds first appear in v0.17.0's quality-first seed, whose changelog entry is about the render
  poll ceiling (`COMFY_WAIT_SEC`) not killing legitimate quality renders, not about a lease length; and the frozen
  roadmap's T6 called the same 3,600 a defect when it found it deployed on one machine (a 60-minute machine-wide lease for a job that finishes in one to two
  minutes; cut to 600 there). Whether to resize a seed is a measurement decision, owed separately, between how long a
  render legitimately runs and how long a wedge may hold the lease; this change does not make it. The guide row now
  quotes the seeded maxima (read from the profile file by a test), so the number a reader sizes from is the seeded one.
- **What changed.** The row and the doctor sentence now say all of that, in code defaults and seeded maxima, with the
  `gpu reserve` wrapper window named as the one place a window can be shorter than the job. Nothing in the code
  changed, and the tier seeds in `setup/templates/profiles.json` are untouched. `docs/tiers/*.md` were read for the same
  claim and carry none.
- **Tests.** `TestOperatorGuideLeaseRowStatesTheMediaLeaseIsTheTimeout`, `TestOperatorGuideQuotesCodeDefaultsForMediaTimeouts`,
  `TestOperatorGuideLeaseRowNamesEveryRoutesTimeoutKey`, `TestOperatorGuideLeaseRowNamesTheLanesThatTakeNoLease` and
  `TestOperatorGuideLeaseRowQuotesTheSeededTimeouts` (internal/pipeline) were red on the earlier row and are green here:
  the row must exist (blind-gate control), refuse the retired and over-wide claims, say what the code does, name every
  route's timeout key (the table in `mediaadmit_lease_routes_test.go`, held to `gpuLeaseCases` and to real config keys),
  name the two lanes that take no lease, quote defaults that equal `config.Default()` and `gpulease.DefaultTTL` (the
  fallback in its own sentence), and quote seeded maxima read from the profile file. The window and the kill are guards,
  green on the code as it stands. `TestAVideoCallsLeaseWindowIsItsOwnTimeout` and `TestAnImageCallsLeaseWindowIsItsOwnTimeout`
  pin the lease half for ComfyUI image and video on both lease paths.
  `TestEveryMediaRouteLeasesForItsOwnTimeoutAndIsKilledThere` pins both halves on all 17 GPU tasks: the route's own key is
  set to 2 s and every other timeout key to 90 s, a stub runner holds the render, the one live lease's declared window must
  be 2 s and the render must be killed at "timeout after 2s" (gpugen names the Timeout it ran under).
  `TestAnImageBatchLeasesAndIsKilledAtThePerImageTimeoutTimesItsJobs` holds a three-job batch at 1 s per image to a 3 s
  window and a 3 s kill. (`COMFY_WAIT_SEC` is the render script's poll budget, not what kills, and the guide does not
  claim it; the kill is pinned where it happens.)
- **Mutations, each restored byte for byte (sha256 compared) and each turning exactly the matching tests red.** Lease and
  kill: video `Spec.Timeout` plus ten minutes; audio lease TTL to zero; run-graph reading the upscale key; batch
  multiplier removed; the iGPU lease TTL set to an hour and the iGPU `Spec.Timeout` doubled; the ComfyUI image
  `Spec.Timeout` doubled; the batch `Spec.Timeout` doubled; the sdcpp image lease TTL to zero and its `Spec.Timeout`
  doubled; edit reading the inpaint key. Earlier rounds: image TTL to zero, video TTL to zero, a one-hour clamp in
  `acquireMediaLease`, the card-scoped TTL to zero and the whole-node TTL to zero. Row: each of the three code defaults
  replaced by a deployed figure, the 3,600 s fallback sentence reworded, each retired or over-wide claim put back, the
  sentences on the call's own timeout, the queue wait, the fallback and `gpu reserve` removed, one route key removed, each
  lane that takes no lease un-named, the seeded maximum changed in the row and (the other direction) in the profile file,
  and the doctor sentence reverted to "the deployed".
