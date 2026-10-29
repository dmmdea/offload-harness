# Changelog

All notable changes to `offload-harness` are documented in this file.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versioning: [SemVer](https://semver.org/).
Releases before 0.190.0 are archived verbatim under [docs/changelog/](docs/changelog/README.md).

## [Unreleased]

## [0.206.0] - 2026-10-29 - the repository reads as a public project, and three fixes from the desloppify pass

### Changed - the repository reads as a public project: a renderable changelog, a stranger-proof README, no evaluation images in the tree, and the maintainer's private vocabulary glossed or removed

- **What changed.** `CHANGELOG.md` was 2.9 MB, past the size at which GitHub shows "we can't show files that are this
  big" instead of the file (checked live on 2026-10-29); it now holds `[Unreleased]` and the releases from 0.190.0, and
  every older entry moved verbatim (three lines neutralised for the leak scan) into `docs/changelog/CHANGELOG-<newest>-to-<oldest>.md` (eight files, each under
  the render limit, indexed by `docs/changelog/README.md`). `changelog.Combined` appends the archive after the live file and its fragments, so every doc-quote check
  (root, `internal/rig`, `internal/mcpserver`) reads the same history it read before the split, and the no-pins lint treats it as history. `runs/g2-images/` (220 generated evaluation
  PNGs, 363 MB, swept into the tree by an unrelated 2026-08-11 commit and referenced by nothing) is untracked and
  `/runs/` is ignored; the files stay in history until the maintainer decides on a rewrite. The README gained the
  Quickstart prerequisite (a serving llama.cpp), the `local-offload` binary-name note, a Windows long-path note (a clone into a long
  path fails its checkout on two 145-character ADR names), a Linux pointer and an honest
  macOS line, says `go install ...@latest` is not a supported path (a local `replace` and tags that lag the source),
  labels the OpenWebUI script as the legacy WSL convenience it is, adds the `agent_rig`, `agent_delegate` and
  `offload_research` rows, trims the longest tool cells to a summary plus a link, links every ADR it names, and
  drops the maintainer's task labels and calibration provenance. `SECURITY.md` names the three opt-in surfaces that
  can send content off the box instead of claiming nothing does. `CODE_OF_CONDUCT.md` (Contributor Covenant 2.1),
  issue forms and a pull-request template that cannot be read as a deletion declaration were added. The maintainer's
  private vocabulary (nightshift, register ids, a private decision tool, private evidence paths, a personal GitHub
  account in `setup/install.ps1`) is glossed in `docs/glossary.md`, reworded in the comments and tier notes that
  used it, or replaced with the environment-driven lookup the Go updater already uses; `docs/specs/` and
  `docs/superpowers/` carry a README saying they are published working notes. The FFmpeg trac extracts under
  `skill/ffmpeg/reference/` carry an attribution file and a NOTICE entry; `.printing-press.json`'s double-encoded
  dash is fixed; a `.gitleaksignore` names the two synthetic JWT fixtures.
- **Why.** The repository is the canonical public home of the harness and is being shared; a stranger's first read
  and first run were the test, and each item above is something that read or run reached.
- **Check.** `go build ./... && go vet ./...` clean; the root-package lints that read the changelog, the version
  sources, the example config, the tree's identifiers and the pins pass; GitHub's blob view of `CHANGELOG.md`
  renders after the merge (the pre-change view showed the size refusal).

### Fixed - the ledger's `tok_per_s` falls back to the observed rate as 0.131.1 said, a fleet door fails closed on an empty token, and one positional splitter replaces four

- **`tok_per_s` on agent rows.** 0.131.1 said the column prefers the calibrated seat rate and falls back to the run's
  observed decode rate when no calibrated sample exists; the fallback sat inside the `SeatTokS > 0` branch, where it could
  never fire, so rows with no calibrated sample stayed empty. The two cases are now an `if` / `else if`.
- **`bearerOK` with no configured token.** `Authorization: Bearer ` (nothing after the scheme) hashes equal to an empty
  token, so a door that called `bearerOK` directly on a node with no `fleet_auth_token` would have accepted it; every such
  door ran an admissibility check first, so no real request reached it. The primitive now returns false on an empty token
  and no longer depends on caller ordering. The tokenless-loopback doors short-circuit before it and are unchanged.
- **One splitter.** `splitArgs`, `splitTwoArgs`, `splitThreeArgs` and `splitFourArgs` were four copies of one loop that
  differed only in slot count; they are now wrappers over `splitPositionals(args, valueFlags, n)`, which the existing
  splitter tests cover. Two unused Python imports and one unused variable in the render self-test are gone.
- **Check.** `go build ./... && go vet` clean; `go test -run 'split|Split|Positional|Overflow' .`, `go test
  ./internal/fleetnode/` and the pipeline package pass; the three Python files byte-compile. Found by a desloppify scan
  and its blind review (strict score 22 before the review, 75.6 after; the remaining findings are API-shape refactors
  listed in the pull request).

## [0.205.1] - 2026-10-29 - comfy-packs matches a pack origin that ends in .git

### Fixed - comfy-packs matches a pack origin that ends in .git or /

- **What it does.** `update --component comfy-packs` compares an installed pack's git origin with its `render/node-packs.json` row after one trailing `/` and one trailing `.git` are dropped from each side; nothing else is normalised (case, scheme and host must match exactly), an empty origin never matches, and `ls-remote` and `fetch` still use the origin as the checkout reports it.
- **Why.** A node whose packs were cloned with a `.git` suffix had every pack skipped as `ORIGIN_NOT_ALLOWED`, so the verb covered none of them there.
- **Check.** The patched `render/` in `--dry-run --json` on that node matched ComfyUI-GGUF and ComfyUI-VideoHelperSuite (`current`, before `ORIGIN_NOT_ALLOWED`). Details: `docs/systems/update.md`.

## [0.205.0] - 2026-10-29 - No version pins left in the harness: ComfyUI packs update to latest, models verified against the publisher, HyperFrames browser auto, the 6 GB tier fits its seats

### Changed - the last "pinned" wording and version ranges are gone, and a guard keeps them out

- **What changed.** The HyperFrames install, the browse sidecar, sd.cpp and llama.cpp were still described as pinned in
  Go comments, docs and two client-visible strings, though ADR 0099 made every one of them "the newest complete build,
  floored". The `offload_browse` tool description and its tools-profile sentence now say "a sidecar" instead of "a
  pinned sidecar", and the compose binding label in route and doctor output reads `hyperframes_dir (project-local CLI)`
  instead of `hyperframes_dir (pinned CLI)`. `integrations/opencode/package.json` takes `latest` for its two ranges, the
  reproduction recipe in the hardware-scope spec downloads the newest llama.cpp release, and the chatterbox setup text in
  `render/README.md` installs torch without a version. The llama-swap README no longer names the surface version the
  CLI was verified against; it points at the vendored schema snapshot that carries it.
- **The guard.** `no_version_pins_test.go` now also reads every tracked `package.json` (a dependency must be `latest`,
  `*`, empty or a non-registry spec; an exact version, a `~` or `^` range and an x-range fail) and scans tracked live
  text (`*.md`, `*.mjs`, `*.js`, non-test `*.go`, `*.ps1`, `*.sh`, `*.py`) for an install command that names a version:
  `pip install` / `uv pip install` / `uv add` with `==`, `npm install` and `npx` with `@<digit>`, `git clone --branch v<digit>`
  and `gh release download b<digits>`. History paths are skipped. A hit names `file:line` and says "ADR 0099: floors
  never pins".
- **Check.** `go test -run NoVersionPins .` fails on the old `package.json` ranges and the old recipe and passes on the
  tree.

### Added - `update --component comfy-packs` moves installed ComfyUI node packs to the newest head, behind an import gate, with rollback

- **What it does.** The update verb gains a component that moves each installed, allowlisted ComfyUI node pack to the head of
  its repository's default branch (the allowlisted publishers release and tag nothing, so the head is the one channel they
  share). It resolves `git ls-remote` for the pack's origin, fetches that exact commit and checks `FETCH_HEAD`, takes
  `pip freeze --all` as a snapshot, fast-forwards the pack, runs one unified resolve over all installed packs under the
  host-torch constraints, and gates the result on `pip check`, the host constraints staying put, and
  `python main.py --quick-test-for-ci` exiting 0 with no `(IMPORT FAILED)` line for a moved pack. A pass writes the
  schema-2 record (`previous` = the commit held before) and the dependency marker. A failure resets the pack to its previous
  commit, restores the pip snapshot with `--no-deps`, requires the freeze and `pip check` to equal the baseline, appends the
  commit to `rejected[]` and exits 1. `--rollback comfy-packs` moves each pack back to its record's `previous` the same way,
  gate included. The work is `render/comfy-packs-update.mjs`; `update_cmd.go` starts it with the node's `node_path`,
  `comfy_dir` and render directory and relays its `--json` line and exit code.
- **What it leaves alone.** A tree with modified tracked files, or whose HEAD is not an ancestor of the remote head (local
  commits), is reported `kept` and never reset. A ComfyUI answering on its port makes the run `refused`. A remote that cannot
  be read, a fetch error, a gate that timed out or found the card busy, and a resolve that lost the network are inconclusive:
  everything is put back, nothing is marked rejected, exit 0. A render request still never moves an installed pack.
- **How to run it.** Under the GPU lease wrapper, because the gate initializes torch:
  `local-offload gpu reserve --wait <window> --drain --unload-seat --for <window> --reason "node packs" -- local-offload update --component comfy-packs --json`.
  `--dry-run` and `--resolve` list each pack, the head its repository names and what would happen, and change nothing.
- **Docs.** `docs/systems/update.md` ("Node packs"), ADR 0099 section 6 (one amendment line) and the node-pack row of the
  frontier-updates spec.
- **Rollback state on disk.** Before the first move the run writes `custom_nodes/.packs-update-journal.json` (the `pip freeze --all` and `pip check` baselines and each pack's from/to); the next start undoes a killed run from it first, SIGINT, SIGTERM and SIGBREAK run the same undo, and a pass or a finished undo deletes it. Every reset is `git reset --keep` after a fresh `git status`: a pack edited meanwhile stays where it is, is reported `kept` as `DIRTY_AT_UNDO` and the run exits 2. A quick test that exits non-zero without naming a moved pack in an `(IMPORT FAILED)` line is inconclusive (nothing rejected, exit 0, `QUICK_TEST_NO_PACK_NAMED`).

### Fixed - the 6 GB tier's support tier no longer starves its two heavy seats, and two tiers stop seeding an image model the installer never installs

- **The defect.** On the 6 GB reference box (RTX 3050) the support tier held embeddinggemma (300M, 464 MiB),
  bge-reranker-v2-m3 (350 MiB) and embeddinggemma2 (1,216 MiB with its projector). With all three resident, both heavy
  seats failed to load: `gemma4-e4b-vision` and `qwen3.5-4b-agent` died with a cudaMalloc out of memory. Nothing used the
  300M: the memory authority and the wiki embed with embeddinggemma2, and the harness kNN pre-filter is off by default
  (`knn_prefilter_enabled` false); the 300M was only loaded by the memory-stack warm-back.
- **The fix.** A new tier flag `omit_embeddinggemma` (inverted polarity, absent = false, requires
  `include_embeddinggemma2`) makes the render drop the 300M entry, its `emb` matrix var, its evict row and its place in
  the residents set, and hand its `text-embedding` / `local-embed` aliases to embeddinggemma2 (`servingtmpl.Params.OmitEmbed300M`,
  hashed into the render stamp only when set). ampere-6 sets it and seeds `memory_stack` as
  `[embeddinggemma2, bge-reranker-v2-m3]`, so `EmbedModel()` falls to embeddinggemma2, the model a kNN index is built with
  when an operator turns the pre-filter on. Every other tier renders as before: ampere-8 and blackwell-3x16 keep the 300M
  because their recorded co-residency budgets count it and nothing there measured it removed.
- **Measured.** With the 300M gone, 4,283 MiB stayed free beside the support tier and `gemma4-e4b-vision` loads and
  answers (3,986 MiB). `qwen3.5-4b-agent` at ctx 65536 with q8_0 K and V still does not load (compute buffer 380 MiB at
  the default ubatch, 318 MiB at ubatch 256). Beside the support tier on a 29,914-token prompt, ctx 49152 loads with 216
  MiB free, ctx 57344 leaves 40 MiB, and ctx 65536 with a q4_0 V cache leaves 120 MiB but changes the cache the bake-off
  measured. 49152 with q8_0 K and V is the ceiling; the tier's own `ctx_size` (32768) is inside it and is unchanged. The
  template and the tier notes carry the figures.
- **Image seed.** ampere-6 and maxwell-3 seeded `sdcpp_model` as the SDXL-Turbo Q4_0 file, which the installer excludes on
  license grounds (non-commercial) and therefore never installs. Neither tier gets the Z-Image set the installer does fetch
  (only amd-rdna3 tiers, or `OFFLOAD_WITH_MEDIA=1`), and that set does not fit 3 GB and was not measured on the 6 GB card, so
  the seed now names no image model (`sdcpp_model`, `sdcpp_model_kind` and the SDXL-specific family, steps and cfg are
  gone); the sd.cpp binary binding and ampere-6's flux3-klein family stay, and an operator binds a model they are licensed
  to run.
- **Docs.** The tier pages (regenerated), the setup installer page and the field docs in `setup/templates/profiles.json`
  say the same.
- **Check.** `install render --profile ampere-6` on linux and windows renders no `embeddinggemma` entry, the residents
  set reads `rer & eg2` (linux) and `eg2` (windows), and the linux entry for embeddinggemma2 carries the handed-over aliases.

### Fixed - HyperFrames always runs against the browser its installed release wants, not the Chrome version the config named

- **The defect.** `hyperframes_browser_path` held the absolute path of one chrome-headless-shell version
  (`.../win64-152.../chrome-headless-shell.exe`) and the runner exported it as `HYPERFRAMES_BROWSER_PATH`. `local-offload
  update` moved HyperFrames to the newest npm release and carried the old browser cache over, but never ran
  `browser ensure`, so a new release kept rendering against the old Chrome. The installers wrote the resolved versioned
  path, which is the pin.
- **`auto`.** `--browser auto` (config `hyperframes_browser_path: "auto"`, now what `install.ps1` and `install.sh`
  write) makes `render/compose-hyperframes.mjs` ask the installed release for its managed browser at render time:
  `browser path` under the harness-owned HOME, and when that file is not on disk `browser ensure` then `browser path`
  again. The render result carries `browser_path` and `chrome_version`. An explicit path works as before.
  `config.BrowserAuto` and `Config.HyperframesBrowserAuto()` name the value; `doctor`/`offload_status` report
  `browser=auto (<newest downloaded browser, or "none downloaded yet; the first composition fetches it">)`, and
  `install hyperframes` notes only an explicit path that differs from the release's own browser, suggesting `auto`.
- **`update`.** The staged HyperFrames generation now runs the install check (`npm rebuild esbuild`, the runner's
  `browser` and `version` ops) before it can go live, so the browser its release wants is fetched while the live
  generation still serves; a failure rejects the stage and leaves the live generation untouched. Browser versions the
  new release no longer resolves are then removed from the staged generation's own cache (the previous generation keeps
  its copy for rollback). With `compose_script` unset the browser is not refreshed and `update` logs that.
  `hfinstall.Verify` is the shared check; `Install` uses it unchanged.
- **Docs.** The operator guide, the media-generation system page and the setup agent guide say the same.
- **Check.** `render --browser auto` on a real install rendered a title card with the release's browser (chrome
  152.0.7977.30); the `browser` op returned the same path.

### Fixed - a manifest model is checked against what its publisher publishes, and a download never leaves a partial file

- **The gap.** A run-graph manifest's `models[].sha256` was supplied by the caller. It pinned one build and authenticated
  nothing against the publisher, a present file with no hash was trusted as it lay, and a download was streamed straight onto
  its final path with no size check, so an interrupted download left a truncated file that every later run trusted.
- **The publisher's digest.** `render/manifest-satisfy.mjs` reads it at run time (`publisherDigest`): for a Hugging Face
  `resolve` URL the `X-Linked-ETag` and `X-Linked-Size` of an unfollowed HEAD, else the tree API's `lfs.oid` and size; for a
  GitHub release asset the release API's `digest` and size. A Hugging Face file outside LFS carries a size only; any other
  host publishes nothing the node reads. What cannot be checked against a publisher digest is listed in `unverified_models`.
- **Downloads.** A model downloads to `<file>.part`, is checked for size and sha256, and is renamed onto the destination
  only then. A mismatch deletes the `.part`, retries once, and then defers `MODEL_SHA_MISMATCH` naming both digests. A
  download that ends short of the server's stated length fails instead of being kept. A missing model whose digest could not be
  read (publisher unreachable, no caller assertion) defers `MODEL_DOWNLOAD_FAILED` (retryable) rather than fetching a file
  nothing can check.
- **Files already on disk.** `<file>.sha-ok` is now JSON `{oid, size, mtime}`. A file whose size and modification time are the
  recorded ones and whose oid is the publisher's is kept without being read; a changed size or time, or a publisher that moved
  to a newer build, hashes it once and replaces it when it differs. The replacement lands where the file was found (an extra
  model root stays on its own drive), and the previous file is kept as `<file>.prev`, one generation, never deleted. A record in the older format (a bare hash) reads as none,
  so each such file is hashed once more. With the publisher unreachable a file with a fitting record is kept and the result
  carries a warning; one with no record is listed unverified and never deleted.
- **The caller's `sha256`** is an optional assertion: when it differs from a published digest the entry defers naming both,
  and it never overrides the publisher. `manifestHash` now keys a model on `path` and `source_url` only, so the first request
  after the upgrade provisions (and so verifies) every manifest once.
- **Docs.** The comments in `render/node-packs.mjs` and `internal/mediacap/manifestmodels.go`, the README, the operator guide,
  `docs/systems/mcp-server.md` and `docs/flows/run-graph-manifest-satisfaction.md` say the same.

### Fixed - node packs carry no pin text, the run-graph cache notices a moved pack, and the install record says where a pack came from

- **The gap.** The 0.204.0 entry says no document describes a pin as policy any more, but `README.md` and the
  `offload_run_graph` tool description and `manifest_path` schema (shown to every MCP client) still read "custom node
  packs @ pinned commits via cm-cli". Nothing is pinned and cm-cli is not used: a pack is cloned from an allowlisted
  repository at the head of its default branch, and the satisfier drives git and uv directly. Those texts, the allowlist
  identifiers (`PINNED_NODE_PACKS`, `loadPinnedPacks`, `NodePackPin` and the like, now `ALLOWED_NODE_PACKS`,
  `loadAllowedPacks`, `AllowedNodePack`), the "pin-set" and "host-pin drift" wording, and the dead `cmCli` argument now say
  what the code does. The `VENV_INCOHERENT` detail for a moved host package now begins "host constraint drift"; callers
  branch on `code`.
- **The cache ignored a moved pack.** The run-graph cache skipped provisioning whenever the `.offload-owned.json` manifest
  hash matched, and since 0.204.0 that hash holds no commit, so a pack an operator moved, replaced or deleted was never
  resolved again while the marker matched, against `docs/flows/run-graph-manifest-satisfaction.md`. The marker now also
  holds the sorted `name@commit` key of the packs on disk, read with one `git rev-parse HEAD` per named pack; the cache
  applies only when the hash and the key both match. The first request after the upgrade provisions once.
- **The install record.** `custom_nodes/<name>.installed.json` is now schema 2: `{schema, repo, commit, installed_at,
  checked_at, previous, rejected}`, where `repo` is the origin the checkout reports, `previous` is the commit and time
  the pack held before, and `rejected` lists commits an install failed on. It is a record of what is installed, not an
  instruction. When the resolve, `pip check` or the host-constraint check fails in the same satisfy that cloned a pack,
  that directory is removed again (a pack that was already there is never touched) and its commit is added to
  `rejected`; a later request that finds the repository's newest commit there defers `NODE_PACK_REJECTED` instead of
  cloning and failing again, and a newer commit is tried afresh. The pack leg now beats the lease progress file around
  checkout, resolve and `pip check`.
- **Installer.** `install.ps1` no longer clones ComfyUI-Manager or installs GitPython for run-graph, which uses neither; a
  Manager clone failure used to abort the installer. `uv` stays required and `comfy-cli` stays optional.
- **Docs.** The setup, operator, fleet-node, glossary, manifest-flow and media-generation pages say the same; the
  MultiGPU archival note says the mirror's default branch carries the Windows/WDDM P2P fix.
- **Tests.** The pack-checkout tests in `manifest-satisfy.test.mjs` wrote their record into the real ComfyUI directory of
  the machine running them; they now use a temporary directory.
- **Fewer false rejections.** A fresh pack whose install failed on the network, a timeout, a busy card or a subprocess that would not start is removed again but its commit is not added to `rejected` (the defer is retryable); `pip check` is read before the clone and a fresh pack is rejected only when it gains lines, and a `pip check` failure whose lines all predate the satisfy no longer defers the run: it goes on and the result carries `warnings` naming them (a conflict between two packs' own requirements, such as albumentations 2.0.8 against albumentationsx, used to defer every graph on the box); a `rejected` entry carries a short sha256 of the venv's `pip freeze` and is ignored once that has changed.

### Fixed - `go test ./internal/pipeline/` no longer sits out the fake seats' simulated hangs

- **The gap.** The package ran close to Go's default 10-minute package timeout (CI runs `go test ./...` with no
  `-timeout`; a contended Windows run was killed at 10 minutes). Production timers were already compressed per test, but the
  fake servers that model "the seat accepts the request and never answers" slept a fixed 3 to 10 seconds after the
  pipeline under test had already given up, and `httptest.Server.Close` waits for every handler, so each such test paid the
  whole simulated hang.
- **What changed.** Test code only. The scripted llama fake gains `loopCtx` and `upstreamModelsCtx`, which are handed the
  request's context, and the fixed sleeps that only simulate a hang (cold-load, silent-seat, busy-hold, ceiling, cancel,
  admission and breaker tests) now wait with `holdUntilHungUp`, which returns the moment the client disconnects. The fake's
  `repackDelay`, `chatFallbackDelay` and `propsDelay` hold the same way. Sleeps that model a real duration an assertion
  depends on are unchanged, and no production timer or timeout moved.
- **The 429 case.** `TestRunAgentTaskRepackWireFailuresAreInfrastructure` took 90 s in its rate-limiter case, because a 429
  retries on the 90 s default contention budget. The test now sets `SeatContentionWaitSec` to 1; the class it asserts does not
  depend on how long the retries ran. The default is unchanged.
- **Also.** The fake's grammar lane answers 500 when a test never scripted `repack`, instead of panicking inside the
  handler (seen in `TestRescueRepackRefusesAHollowObjectFromTheNode`).
- **Docs.** No `docs/` page describes the pipeline test fakes, so none changed.

### Fixed - `gpu reserve` for a ComfyUI run-graph declares what the graph loads, and a graph with no weight files declares the ComfyUI process floor, not the largest family's size

- **The gap.** `gpu reserve --class media -- node render/comfy-run-graph.mjs --graph g --manifest m` was not recognised as a
  render helper (the helper table left `comfy-run-graph.mjs` out, with a comment that it takes the graph positionally; it
  parses `--graph`, `--manifest` and `--template` as flags). The call fell to the media class default, the largest ComfyUI
  family the box binds, 32.9 GiB for a node that binds Wan 2.2, and a small graph was refused on a box whose commit charge was
  high unless the operator passed `--ram`. The sizing itself also gave up on a graph that names no weight file
  (`ForGraph`, `ForManifestModels`), so the run-graph lane's own admission fell to the same default.
- **What changed.** `comfy-run-graph.mjs` is recognised (route `run-graph`): `--graph FILE` is sized by the weight files its loaders
  name, and `--template NAME --manifest FILE` by the manifest's model files. A graph, or a manifest with an empty `models` list,
  that names no weight file is the measured ComfyUI process floor: 3 GiB touched and 4 GiB of commit (a small VHS GIF graph
  measured 2.63 GiB private and 3.64 GiB of system commit, rounded up), source `measured`. A graph with a loader node that names no
  file, a graph with no node, a weight under another extension and a graph whose weights are not on disk yet keep the class default.
- **Commit is stated.** The floor's 4 GiB is a stated commit (`Need.CommitGiB`, `Need.CommitStated`): the grant admits it against
  the OS commit limit like a `--ram-commit`, not against physical RAM less the headroom as an unstated commit is. `gpu reserve`
  (`resolveReserveHostLoad`), the pipeline's media admission, the whole-node acquire and the run-graph lane probe all carry it; an
  operator's `--ram` or `--ram-commit` still wins. The class-default hint now names `comfy-run-graph.mjs` and no longer says a graph
  that names no weight file takes the class default. Described in `docs/systems/gpu-lease.md` (what a lease declares).

### Fixed - `update` run inside `gpu reserve` no longer waits out the lease queue behind its own parent

- **The gap.** `local-offload gpu reserve --wait 30m --drain --for 20m -- local-offload update --component llama` hung.
  The llama smoke gate takes its own lease (`gpu reserve --drain --unload-seat -- update --gate-child`), and that inner
  reserve queued behind the outer lease its own parent held, for the whole `update.gate.lease_wait`. The update then
  ended inconclusive with nothing changed. The house rule that every GPU job goes through `gpu reserve` sent every agent
  into it.
- **What it does.** The gate checks whether this process already runs under a live lease (`GPU_LEASE_EPOCH` equals the
  epoch of a live lease in `GPU_LEASE_DIR`). When that lease drained the seat and cleared it (`--drain --unload-seat`),
  the gate runs its measurement under it and logs "running under the caller's GPU lease (epoch N)". When it did not, the
  gate answers inconclusive at once, saying to run `update` without the wrapper or to wrap it with
  `--drain --unload-seat`. Outside a lease nothing changes. No other component's gate takes a lease.

## [0.204.3] - 2026-10-28 - sd.cpp video and animate lanes run on every build from master-1000, and a short fast clip decodes with the full VAE

### Changed - the last version-pin wording is gone from the CLI help, the OpenSpec rules, a Proposed ADR and the notices

- **The gap.** After ADR 0099 some text still described pins the harness no longer has. `install forecast` help said
  "the pinned timestral package" (it installs the head of the default branch and checks the weights against the
  publisher's digest). The OpenSpec project rules told a change that third-party software enters with a "pinned version
  and sha256", the opposite of `docs/systems/security.md`. Proposed ADR 0089 (Strata) asked its spike for a pinned
  release tag. Two `servingtmpl` comments, the doctor's model-bindings legend and an `install_render.go` comment said
  "pinned build" or "pinned download" for the installed build and the recorded download size.
- **What it says now.** The newest release, checked against the publisher's digest at install time, never a pinned
  version; the installed build; the size the installer recorded. The vendored HyperFrames kit and the ContextBench port
  name the commit they were taken from as their source commit (provenance, not a pin; the kit test still checks the
  same hashes).
- **Engine build instructions.** `setup/SETUP-AGENT.md` told an agent to build audio.cpp "from the v0.9.1 tag (the
  measured release)" and named sd.cpp master-945 and audio.cpp v0.9.1 as "the measured releases"; it now says to take
  the newest release of each, keeps the measured ones as a record, and asks to check the ACE-Step prefill patch against
  the tag being built (`patch -p1 --dry-run`). `docs/systems/media-generation.md` says the same.
- **Also.** A llama-swap swap on a node that stops its scheduled task first no longer logs "no running process found"
  after ending the task: it says no process was left after ending the task.

### Fixed - the sd.cpp video and animate lanes run on every build from master-1000 on

- **The gap.** From master-1000, stable-diffusion.cpp prints, right after each module's compute buffer on the device, a
  small one on the CPU for the same module (`taehv compute buffer size: 35.50 MB(VRAM) on Vulkan0`, then
  `taehv compute buffer size: 0.76 MB(RAM) on CPU`). The CPU-placement guard failed every video and animate run on such a
  build with `CPU_PLACEMENT`, although no op runs on the CPU: with `GGML_SCHED_DEBUG=2` the measured master-1024 FastWan
  run had 10 graph splits, all on Vulkan0, and the parameters on Vulkan0. A node therefore kept older builds for its video
  lanes beside a newer one for its image lanes.
- **What it does.** A CPU compute buffer is read as that host-side companion only when the same module already printed a
  compute buffer on the device in the run and the CPU one is no larger; a module whose only compute buffer is on the CPU,
  whose CPU line comes first, or whose CPU buffer is the larger still fails `CPU_PLACEMENT`. The real master-1024 log is a
  new healthy fixture (`render/testdata/sdcpp1024-video-healthy.log`). The video and animate runners pass
  `--flash-attn off,diffusion=on` instead of `--diffusion-fa`, which master-1000 deprecated, and the sd.cpp floor in
  `setup/floors.json` rises from 968 to 1000, the first build with that flag.

### Fixed - a short `fast` clip no longer decodes through the tiny autoencoder

- **The gap.** On `<node-f>` (FastWan 832x480, 3 steps, seed 42, 2026-10-28), a 9-frame clip (3 latent frames) decoded
  through `taew2_2` into blocks of false colour over the subject, on master-945 and master-1024 alike. The full VAE
  decoded the same latents clean (141.8 s against 1.6 s), and 49 frames decoded clean through the tiny one (8.1 s).
- **What it does.** The video and animate runners pass `--taesd` only from 17 frames (`TAE_MIN_FRAMES` in
  `render/igpu-engine.mjs`, the shortest clip measured clean, SSIM 0.95 against the full VAE). A shorter `fast` clip
  decodes with the full VAE, and the runner says so on stderr. The runner comments that still said "pin bump" or
  "pinned CLI" now say an engine update and the installed CLI.

## [0.204.2] - 2026-10-28 - update takes over what each node runs, as it is laid out

### Fixed - `update` adopts the nodes as they are: sd.cpp's `sd-cli`, a build family per component, repoint of unnumbered and source-tree directories, the root from any installer layout

- **The gaps.** A read-only pass over every node, with the verb's own dry runs, found five things that stopped the
  verb from taking over what the nodes run:
  - sd.cpp's program was looked up as `sd`. Every release above the floor ships `sd-cli` and `sd-server`, so the
    offline self-check would reject the newest build, and a rejected id is never retried.
  - The build family came from the node's backend for every component. A maxwell-3 node serves llama.cpp on CUDA and
    sd.cpp on Vulkan (the only family measured to render on its card), and its sd.cpp would have been replaced by a
    CUDA build with no code for the card.
  - `--repoint` matched only build-numbered directory names, so a llama.cpp kept as `/opt/llama-vulkan` could not be
    repointed. It also kept the `bin` segment of a source tree (`llamacpp-b<N>/bin/llama-server`), which pointed at
    a path the flat publisher archive does not have.
  - Repoint's `LD_LIBRARY_PATH` pass replaced a whole repointed list, dropping `:${LD_LIBRARY_PATH:-}`.
  - The install root was found beside the binary only when it sat in `<home>/bin`. The Windows installer puts it in
    `<home>/harness`, so a node set up by the installer needed `--home` on every call.
- **What it does.**
  - sd.cpp's program is `sd-cli`.
  - The family is chosen per component: `update.variant.<component>`, else the `ggml-cuda` / `ggml-vulkan` library
    beside the live build, else the node's backend. The verb says which one it used.
  - `--repoint` takes `--from <dir>`, which matches whole path components only, and drops a `bin` segment when the
    live build is flat.
  - A line the directory pass rewrote keeps the rest of its `LD_LIBRARY_PATH` list.
  - The root is `--home`, `$OFFLOAD_HOME`, `update.home` in `config.json`, then the nearest folder holding
    `installed.json` among the binary's folder and the three above it, then `~/offload-stack`. The verb prints which
    of these it used.
- **Check.** This branch's binary, on the nodes themselves:
  - A Windows maxwell-3 node, binary placed in `<home>/harness`, no `--home`, no update config: `install root ...
    (installed.json above this binary)`. `sdcpp: build family vulkan (the live build carries the ggml Vulkan
    backend)` resolved `sd-master-<sha7>-bin-win-vulkan-x64.zip`. `llama: build family cuda12`. whisper (a house
    build outside the verb) read `not-installed`.
  - On two Linux nodes, `--repoint` on copies of their yamls:
    - `--from /opt/llama-vulkan` rewrote all 8 seat lines and kept `:${LD_LIBRARY_PATH:-}` on the macro.
    - A source-tree yaml rewrote 16 lines to `<live>/llama-server` with the `bin` segment dropped.
  - The publisher's newest sd.cpp Windows build prints `stable-diffusion.cpp version unknown, commit <sha7>` from
    `sd-cli.exe --version` and exits 0.

  Docs: `docs/systems/update.md` (Usage, Algorithm step 2, the config table, Repoint).

### Fixed - `update` updates what a node runs: it finds a hand-laid live path from its record and never installs on `all`

- **The gap.** The verb found a component only through `--live`, `update.paths` in `config.json` or `<home>/<component>`.
  A node whose llama-swap lives outside `<home>` (a plain `C:/llama-swap` folder) was updated once with `--live`, and
  the next bare run failed with `llama-swap: <home>/llama-swap/llama-swap.exe is not there`, although `installed.json`
  already held the live file as the current generation. And `--component all` (the default) installed every component
  the node did not have: a dry run on a node with no stable-diffusion.cpp planned `sdcpp ... install`.
- **What it does.** `installed.json` records each component's `live` path whenever the verb finds the live build there,
  and the live path is now `--live`, then `update.paths`, then the recorded `live`, then `<home>/<component>`. A record
  written before `live` existed still finds a single-file component through its current generation's path when that
  is a file. On `all`, a component with no record and nothing at its live path is reported `not-installed` (exit 0,
  nothing written); naming it with `--component` is the one way to install it. `--rollback` resolves the live path the
  same way.
- **Check.** This branch's binary, dry run on a Windows node with `update.paths` removed from a copy of its config:
  llama-swap was found from its record (`v268  none`) where the released binary stopped with `is not there`; `sdcpp`
  read `none  -  -  not-installed` with the hint line; `--component sdcpp --dry-run` still planned `install`. Docs:
  `docs/systems/update.md` (Usage, Layout).

## [0.204.1] - 2026-10-28 - Vision calls are visible: gpu status, offload_status and node health name a single-shot vision call

### Fixed - a vision call outside a lease is visible: `gpu status`, `offload_status` and node health name it

- **The gap.** A single-shot vision call (`vqa`, `ocr`, `assess_image`, a `video_describe` window) holds no lease and is no
  job of a node's store, so nothing recorded it. A script that ran one `local-offload vqa` per image kept a node's vision
  seat busy for hours, one call every few seconds, while `gpu status` read `free`, the node's health read `jobs_running: 0`
  and every delegator's `offload_status` showed the node `in_flight: 0, verdict: cold`. Finding the caller took a walk of
  the process table.
- **What it does.** `runVisionGen`, where every vision door converges (CLI, MCP, `/fleet/vision`), registers the call in
  the activity registry while it is on its seat (`gpuactivity.KindVision`: the seat, the door and the parent pid, the
  question or the file name). It registers in admission while a cold seat loads and is running from the request on, like
  every run (register C-60). A cache hit and a lease defer never reach the seat and are not registered. `gpu status` and
  `offload_status.gpu_lease` then read `working` and list the call. Node health gains `vision_calls_running`: the vision
  calls other processes on the box have on a seat (the node's own `/fleet/vision` jobs are already in `jobs_running`). The
  delegator's `offload_status` adds it to the node row's `in_flight` and publishes it beside it.
- **What it leaves alone.** A vision record carries no `devices`, so the run-cap line of a seat pinned to one card does not
  count it and an agent contract never waits behind it; a card-scoped drain still sees it through the seat's placement
  (`LeaseScope.TouchesRun`). No placement rule reads `vision_calls_running`, and the CLI's default `--route local` is
  unchanged.
- **Check.** One CLI `vqa` with the built binary on a real vision seat, beside a loopback `fleet-serve` of the same build
  reading the same registry, polled every 0.5 s: `vision_calls_running` 0 → 1 → 0 and `gpu status` `free` → `working`
  (seat, pid, `origin: cli:vqa, parent pid N`, the question) → `free`. Docs: `docs/systems/fleet-node.md`,
  `docs/systems/gpu-lease.md`.

## [0.204.0] - 2026-10-28 - Frontier updates: the harness pins no version, and `local-offload update` moves every engine to its publisher's newest build behind a measured gate with rollback

### Changed - the harness never pins a version: installers resolve the newest complete build from the publisher, floors live in one file, and a lint keeps it that way

- **What it does.** No release tag, commit, byte count or digest literal for a dependency lives in the repository any
  more. `setup/install.ps1` and `setup/install.sh` resolve each component (llama.cpp, llama-swap, whisper.cpp,
  stable-diffusion.cpp, the models) from its publisher at run time, take the newest complete build, and verify the
  publisher's own digest: the GitHub release `assets[].digest` (and `gh attestation verify` where a project publishes
  attestations), the Hugging Face `lfs.oid` and size, the npm `dist.integrity`. A mismatch deletes the partial file and
  retries (three attempts); a verified file is swapped in once, outside the retry loop, the old one kept as `.prev`. A fresh install lands on the newest build, a re-run over a newer
  build changes nothing, and a newer hand-placed build is kept, never downgraded.
- **Floors, not pins.** The minimum build a seat needs (llama.cpp b11452 for `gemma-embedding2`, stable-diffusion.cpp
  master-968 for FLUX 3) lives in exactly one file, `setup/floors.json`. A floor only rises; a ceiling or an exact
  version is unrepresentable there; a candidate under a floor is never selected. The placement pin of ADR 0078 (a model
  held to a card) is unrelated and stays.
- **Generations with rollback.** Per component a node keeps the current and the previous generation. The live path
  (`<home>/llama`, `<home>/sdcpp`, ...) becomes a junction or symlink to a write-once `<component>-versions/<id>`
  directory, so no rendered config, task or probe changes; a candidate is staged, verified and switched atomically, and
  a failed gate or first load points the live path back at the previous generation. An engine whose live directory
  a running process holds is staged, reported and recorded as `staged`, never swapped out from under it. `installed.json`
  (schema 2) records per component the version, the publisher's digest, the size, the source and the install time, `dest`
  for a model file and `staged` for a generation waiting for a restart, each written the moment its component is live;
  the update verb (its own change) adds `current`, `previous`, `rejected[]`, the ETag and the check time.
- **The lint.** `no_version_pins_test.go` fails when an installer, a render script, a dependency manifest or an
  accelerator recipe carries a release tag, a build number, a 40- or 64-hex digest or an exact dependency version, naming
  file and line; `setup/floors.json` is the one place a build number may appear. The repository `AGENTS.md` and
  `CLAUDE.md` carry the rule in one sentence each.
- **Docs.** ADR 0099 (`docs/architecture/decisions/0099-frontier-updates-floors-never-pins.md`) records the rule, the
  publisher-integrity source, the generations layout, the update verb and the lint, and reverses the 2026-08-27 "blessed
  tag, never the newest nightly" policy: a measured gate with automatic rollback replaces curation, and the `release`
  channel stays selectable per node. ADR 0081's second-build doctrine is superseded (its status reads Superseded by 0099;
  the body is kept as history). The setup runbook (Hard rule 2 and the integrity-mismatch rows), the system docs, the root
  docs, the glossary (Floor, Version pin, Placement pin, Generation), the maxwell-3 design and plan, the older specs and
  plans, the profile notes and the regenerated `docs/tiers/` pages no longer state pinning as policy; dated measurements
  ("measured on b11490 on 2026-10-28") stay as records. The frozen 2026-08-26 roadmap gains one line marking it a
  historical snapshot.
- **Check.** `go generate ./...` regenerated `docs/tiers/*.md` from `setup/templates/profiles.json`; `go run
  ./cmd/changelog check` and the tier-docs, operator-identity and changelog-tree tests pass; a search of the docs, the
  runbook, the profile notes and the root docs for the old pin wording returns nothing.

### Added - the `maxwell-3` tier: a 3 GB NVIDIA card (GTX 970M-class, sm_52) gets its own band, with the E4B workhorse omitted and the E2B resident

- **What it does.** `hwdetect` and `detect.ps1` classify an NVIDIA card with less than 4 GB of VRAM (and no
  Blackwell or Volta label) as `maxwell-3` instead of letting it fall through to `ampere-6`, whose E4B workhorse
  needs 3,100 MiB at 16K and cannot load on a 3,072 MiB card. The arch label `maxwell` (GTX 900 desktop and
  mobile, 750/750 Ti/745, the 8x0M and 9x0M(X) mobile parts, Quadro M, the original TITAN X) keeps the card out
  of the "unrecognized NVIDIA" warning; the band keys on VRAM, so a 4 GB card stays `ampere-6` and an unknown
  VRAM (0) keeps the old fall-through. The tier renders the cuda template with the E2B resident, 32768 ctx, f16
  KV, flash-attn on, no 26B, no spill seat, no embeddinggemma2, one fleet worker and the research agent profile;
  the 12.4 prebuilt serves the card through its embedded sm_50 PTX (a one-time JIT the install caches).
- **`omit_e4b`.** A new profile field in the other polarity of the `include_*` family: `true` removes the
  `offload-e4b` entry, its matrix var and its set membership (new `__E4B_LEAD__` / `__E4B_AND__` tokens in every
  template, rendering their old literals by default) and drops `model-e4b` from the Windows download set.
  Absent or false renders every existing tier byte for byte as before (the render goldens did not move). The
  renderer and `Resolve-ProfileParams` refuse the flag without `resident_tier` `gemma4-e2b`, and together with
  the CPU seat family, which mirrors the workhorse. `selftest.ps1` benches the first served tier when
  `offload-e4b` is not served. The spec stamp carries the field (`omit_e4b`, omitted when false).
- **Media seats, measured on the box.** The tier declares a vision seat (`gemma4-e2b-vision`: the resident E2B
  weights plus the ggml-org Q8_0 projector at an 8192 window, reasoning off, an image capped at 768 tokens;
  2,578 MiB loaded, 2,819 peak on a receipt and 2,896 on a 4K screenshot under the cap, a screenshot and a
  receipt read correctly; a 512 cap misreads receipt lines, which is why 768 is the bound) and an STT seat (`whisper-stt`: large-v3-turbo q8_0 with the silero
  v5.1.2 VAD and flash attention OFF per the house Spanish-audio rule; 1,263 / 1,323 MiB, a 759 KB Spanish clip
  in 3.2 s warmed, the same transcript as q5_0 and as flash attention on). Both are swappable, because either
  one beside the chat seat overflows a 3 GB card. The image lane binds the sd.cpp Vulkan build the installer resolves with
  sdxl-turbo Q4_0 exactly as ampere-6 does: a correct 512x512 in 45 s on the card, the weights parked in host
  RAM and staged per step (1,964 MiB peak on the card).
- **Agent seat and spill, measured.** The six-arm bake on the box (G1 fit and decode, G2 ten strict tool
  calls, G3 the grounded digest-8 contract through `local-offload delegate`, G4 a Jev judge over the
  survivors) keeps the resident E2B as the agent seat: 8/8 grounded digests, every one format-clean, the
  lowest fabrication signal of the three survivors, 926 MiB free at peak, and one weight file shared with
  chat and vision, so an agent call never evicts the chat seat. LFM3-1.7B Q6_K and Q8_0 also reach 8/8
  (Q6_K leads on coverage) but need a newer llama.cpp build than the one installed at bake time, so Q6_K
  is recorded as the runner-up to bind once the newest build carries it; Qwen3.5-2B and LFM3 QAD-Q4_0 fall at G3, the 4B
  quants and MiniCPM5-2B at G1. The Qwen3.6-35B-A3B RAM spill loads cleanly (ADR 0080 gates 2 and 3)
  but decodes at 9.4-10.9 tok/s on this single-channel host, under the 10 tok/s bar, so it is not carried.
- **Sources.** `model-e2b` and `model-e4b` follow the current upstream LFS objects (unsloth re-uploaded
  both QAT GGUFs, 2,016 bytes larger with new oids); stale recorded sizes failed the check on a fresh install. The
  three media-seat files get source rows (`model-e2b-mmproj`, `model-whisper-turbo-q8`, `model-silero-v5`):
  recorded bytes and sha256, placed by the operator, warned about by `install render` when absent.
- **Check.** `detect.ps1 -SelfTest` and `go test ./internal/hwdetect/` with the new rows (970M 3 GB ->
  `maxwell-3`, 3.9 -> `maxwell-3`, 4 -> `ampere-6`, unknown VRAM -> `ampere-6`, 1050 Ti -> `other`);
  `setup/render.tests.ps1` with a `maxwell-3` block on the min and high RAM bands (no `offload-e4b` entry or var,
  the interactive set opens on `e2b`, f16, no spill); an `ampere-6` render unchanged; the design and the
  measurement plan in `docs/specs/2026-10-28-maxwell-3-tier-design.md`, the tier page in
  `docs/tiers/maxwell-3.md`. Installed end to end on the reference box `<node-g>` on 2026-10-28: `install.ps1`
  classified it `maxwell-3` and exit 0 in 409 s, `selftest.ps1` passed (E2B 37.1 tok/s, the depth-7000 canary), a
  boot task serves llama-swap on 11436, and one real call per lane came back correct through it (summarize, vqa on
  a receipt, transcribe a Spanish clip, generate-image through sd.cpp); the scrubbed report is
  `docs/tiers/reports/maxwell-3-node-g.md`. Nothing in the profile is PROJECTED any more.

### Added - `local-offload update`: every engine moves to its publisher's newest complete build, with generations, a kept previous build and automatic rollback

- **What it does.** A new verb resolves llama.cpp, llama-swap, whisper.cpp, stable-diffusion.cpp and HyperFrames from
  their publishers at run time (GitHub releases, npm), picks the newest build whose every required asset for this node
  is uploaded with a digest (a half-uploaded release steps back one tag), verifies the publisher's own sha256 (and the
  checksums file and `gh attestation verify` when they exist), stages it, runs an offline self-check (the program
  starts; for llama, every `--flag` the rendered llama-swap yaml passes to it must exist in the staged
  `llama-server --help`), and promotes it as a write-once generation. The component's live path keeps its name and
  becomes a junction (Windows) or symlink (Linux) to `<live>-versions/<id>`, so no rendered yaml, task or probe changes.
  Exactly the current and the previous generation are kept; a failure after the flip re-points to the previous one and
  records the id in `rejected` so it is never retried.
- **A free no-op.** The listing is fetched with `If-None-Match` from `installed.json`; a `304`, the same id, a rejected
  id or an older one ends the run with exit 0 and a timestamp, so running it every five minutes downloads, hashes and
  extracts nothing.
- **llama-swap** is swapped through `node-swap` (hash verify, drain of the running seats through llama-swap's own
  unload, stop, swap, restart, verify `/running` and `-version`, rollback); `--rollback <component>` returns to the
  previous generation. The layout is decided once (`internal/update/layout.go`): where `<home>/llama-swap` is the
  installer's junction or symlink into `llama-swap-versions` the build is staged as a write-once generation and the link
  is flipped (rollback flips it back); a hand layout (one file in an ordinary directory) keeps the file swap with the
  previous build as `<file>.bak-<id>`. Both share the drain, stop, restart and verification.
- **Floors, not pins.** `setup/floors.json` (embedded in the binary) holds the minimum build a seat needs; the verb never
  selects a candidate under a floor and never downgrades. No tag, build number or digest literal exists in the code.
- **`installed.json` schema 2** adds `{current, previous, rejected[], etag, checked_at, floor}` per component, written
  atomically; a missing manifest is bootstrapped by probing `--version`. The installer's own flat record
  (`{version, digest, size, source, installed_at, dest?, staged?}`, keyed by build family such as `llama-cuda13`) is read
  as the current generation, `dest` is carried through, and the live path's own `<id>.meta.json` settles a disagreement.
  A generation the installer left `staged` behind a running engine is promoted without a download when its meta file's
  digests equal the publisher's for the newest build (a newer build ignores it); the verb's meta files now carry the
  installer's `tag`, `digests` and `sizes` fields too, so the installer reads and reuses them.
- **Config.** `update` (`channel`, `paths`, `restart`, `llama_swap_yaml`, `gate`) is a declared `config.json` key: no
  more `unknown config key "update"` warning.
- **`--repoint <yaml> --component llama`** backs up a hand-kept llama-swap yaml, replaces every build-named directory
  with the live path and (Linux) points `LD_LIBRARY_PATH` at the live `bin`, without re-rendering.
- **Side-port smoke gate for llama.** Before a staged llama.cpp build is promoted the verb takes the node's GPU lease
  (the `gpu reserve --wait --drain --unload-seat` path, `update.gate.lease_wait`, default 30m), starts the staged
  `llama-server` on a loopback side port with one of the node's own seat command lines, measures load time, prefill and
  decode tok/s and VRAM, then does the same with the previous generation and rejects on no load, an empty or non-200
  answer, a rate more than 20 percent lower, or VRAM more than 10 percent higher. A lease timeout is "inconclusive,
  retry later", never a rejection. The numbers go in the generation's meta file. After the flip llama-swap reloads its
  seats through its own API and the first real seat load is watched (`update.gate.post_flip_timeout`); a failure rolls
  back with "post-flip load failed". `--no-gate` and `update.gate.enabled: false` opt out.
- **Hardening.** The restart mechanism is probed before any drain (a missing task or unit skips the component,
  nothing stopped; on Windows the one task that runs the live llama-swap is found when the default name is absent);
  the verb reads its config only from `--config`, `$LOCAL_OFFLOAD_CONFIG` or the home config, never the current
  directory's; pruning deletes only meta-file-backed generations inside the live path's own `-versions` folder and
  never the live target; the Windows junction flip makes the new link first and restores the old one by rename; the
  attestation result is reported and ratchets (a once-attested publisher that stops attesting is rejected); the npm
  version must be a semver; a `--repoint` backup keeps the yaml's file mode and the gate's job file carries no seat
  environment; a lock older than three hours is taken over; timeouts, out-of-memory answers and busy seats are
  inconclusive (rolled back, retried), never a rejection. The post-flip watch loads the seat through the GPU-lease
  fence (`modelaffinity.AwaitModelRoute`), so a render that took the cards after the gate never gets a model loaded
  onto them; the gate's own side-port requests live in `internal/update/sideport.go`, admitted by the fence lint.
  With no `--home` and no `$OFFLOAD_HOME`, the install root is the folder above the running binary's `bin` when it
  holds the installer's `installed.json`, else `~/offload-stack` when that holds one; no root found is exit 2, never a
  fresh install in a guessed folder.
- **Not in this change.** The installers and the fleet registry rows.
- **Known gap.** After a rejected llama build the gate's `gpu reserve` wrapper still warms the old seat back and the next
  flip unloads it again; a "no warm-back on reject" option belongs to `gpu reserve` (follow-up).
- **Check.** Real run on the ampere-8 reference box: `update --resolve llama` and `--dry-run` against the live publisher,
  and on a scratch installer layout a llama-swap update (junction flipped, previous kept, record rewritten) and a
  rollback (see `docs/systems/update.md`).

### Fixed - the HyperFrames lane no longer refuses to run unless one exact version is installed: the harness installs the newest npm release and the runner requires only a floor

- **What was wrong.** The composition lane was welded to one HyperFrames release. The runner
  (`render/compose-hyperframes.mjs`) threw `CLI_MISSING` on any version but its exact-version constant, the
  Go installer (`internal/hfinstall`) embedded a committed `package-lock.json` and compared the runner's
  pin, the lock and `package.json` for equality, `local-offload install hyperframes` compared the
  chrome-headless-shell build, and capability status read `BOUND-BUT-MISSING` whenever the install
  differed from the pin. A newer upstream release was therefore refused by every layer until someone
  edited five files in step, and a node that fell one release behind answered `CLI_MISSING` to every
  composition. Nothing about a newer release is wrong in itself.
- **The fix.** The installed HyperFrames is a record with a floor. `setup/hyperframes/package.json`
  depends on `latest` and the lockfile is gone; the installers run `npm install --ignore-scripts` in a
  staging copy (npm verifies each tarball against the registry's integrity field), run
  `npm audit signatures`, read the staged `node_modules/hyperframes/package.json`, check its version
  against the `hyperframes` floor in `setup/floors.json`, and swap it in with the previous tree kept as
  `node_modules.prev`. Every swap is a whole directory rename: PowerShell's `Move-Item` is not one (when a
  render holds a file it moves what it can and splits the tree), so `setup/install.ps1` renames through
  `Move-DirAtomic`, which also carries the engine installer's legacy-folder migration and staged-generation
  move. A refused swap changes nothing; one that fails after the old tree moved aside puts it back. A lockfile an
  older installer left is retired to `package-lock.json.prev`, and a rollback restores only the `.prev` files its
  own swap wrote. An install at or above the registry's newest is kept, never downgraded. The
  version and time are recorded in `installed.json` under `components.hyperframes.current`
  (`{id, version, installed_at}`; a flat schema-1 entry is replaced by the record). The runner's
  `assertInstalled` throws `CLI_MISSING` only when the CLI is absent or below the floor, and capability
  status (`BOUND-BUT-MISSING`) agrees. The exact-version constant, its assertion, the drift, pin-match and lock-pin helpers and the
  chrome-headless-shell comparison are removed: HyperFrames fetches its own browser and
  the harness only reports where it put it. The `version` op now answers `{version, floor, at_floor}`.
- **What an operator sees.** `local-offload install hyperframes` moves a node to the newest release (or
  says it already is), prints the browser path, and `installed.json` names the version. A release that
  changes a flag the runner relies on is absorbed in the runner's argument builders, not pinned away.

### Fixed - the Windows installer ships the render runners beside the harness, so the sd.cpp and hyperframes routes resolve on a fresh install

- **What was wrong.** Every relative render binding the config carries (`render/sdcpp-generate.mjs` on
  the tiers that seed the sd.cpp image lane, `render/compose-hyperframes.mjs` wherever the installer
  binds the compose lane) resolves against the harness executable's directory, and `install.sh` copies
  the `render/` tree to `<prefix>/bin/render` for that reason. `install.ps1` never did: a fresh Windows
  install built `harness\local-offload.exe`, installed hyperframes, bound both scripts, and `doctor`
  then reported both routes `BOUND-BUT-MISSING` (`script not found at <home>\harness\render\...`),
  found on the `maxwell-3` reference box on 2026-10-28; `generate_image` deferred on its first call.
- **The fix.** A new Step 7a copies the repo's `render/` tree to `<home>\harness\render` on every
  install (never skipped, like a rebuilt exe), fails loudly when no runner lands, and
  `SETUP-AGENT.md` lists the directory among what the installer lays down. Boxes that bound an absolute
  `compose_script` by hand keep working; a relative binding now resolves on Windows as it does on Linux.
- **Check.** `install.ps1` re-run on the reference box: `doctor` reports `generate_image` and
  `compose_video` `CONFIGURED`, and `local-offload generate-image` renders a 512x512 through sd.cpp.

### Fixed - the dev tooling and runtime refusals carry no version pins: ComfyUI node packs are admitted by repository, the forecast lane resolves at install time, CI reads its floors and tracks the newest releases

- **What changed.** ComfyUI node packs are admitted by repository (`render/node-packs.json` has no commit column);
  a node clones a pack it does not hold at the head of the default branch and records the commit beside the pack. A
  render request never moves an installed pack: updating one is the operator's (`git pull --ff-only` in its folder,
  or the fleet's update tooling), so a holder of the render bearer cannot make a node fetch an upstream's newest push.
  The dependency marker is keyed on the recorded commit, so a pack the operator moved is re-resolved. The forecast
  installer (`install forecast`) resolves the Hugging Face revision of `main` and the weights' digest and size from
  the publisher's API at install time, and installs the timestral package from the head of its default branch;
  python stays a floor (`>=3.13`). The browse sidecar takes `browser-harness>=0.1.13` and `jev-ultrafast` from its git
  default branch, installed with `uv sync --upgrade`, so every install run re-resolves to the newest whatever
  `uv.lock` the install folder already holds. CI takes the newest stable Go (`go-version: stable`, `check-latest`; `go.mod`'s `go` line stays the floor), tracks the
  current Node, installs `govulncheck@latest`, and no longer demands the compiler equal a `toolchain` line, which is
  gone from `go.mod`. The vendored llama-swap config schema is a refreshable snapshot
  (`scripts/refresh-llamaswap-schema.sh`), and `version drift` treats it as a floor: only a server older than the
  snapshot is drift. The vLLM and LMCache patch notes say the patches target the version they were written against and
  carry a re-base duty. `no_version_pins_test.go` now also scans `render/`, `install_*.go`, `scripts/openspec*`,
  `accelerators/`, the browse and HyperFrames manifests and the CI workflow.
- **Accelerators.** `accelerators/coral/models.json` and `accelerators/rknpu/models.json` carry no `sha256` and no
  version. Both `fetch-models.sh` scripts resolve at run time and verify against the publisher: Coral reads the git
  blob id of each file at the tip of `google-coral/test_data`; RKNPU takes the newest non-draft release of
  `airockchip/rknn-toolkit2` (runtime library and the wheel for the venv's python) and `airockchip/rknn_model_zoo`
  (labels, test images, the ONNX download links) that carries every file asked of it, and the timm weights' `lfs.oid`
  from the Hugging Face tree API at `main`. Each file that passes is recorded as `{file, digest, publisher_digest,
  size, source, fetched_at}` in `models.installed.json` beside the weights, and the sidecars refuse a model whose
  bytes differ from that record (`model_sha_mismatch`); a file with no record is served and listed under
  `models_unverified` in `/health`. `fetch-models.sh --dry-run` prints the resolved digests and downloads nothing. The
  conversion recipe's pip lines are floors.
- **OpenSpec.** `scripts/openspec-install.sh` installs `@fission-ai/openspec` at the registry's `latest` with
  `npm install --ignore-scripts`, then checks the installed package's recorded integrity against a second
  `npm view ... dist.integrity` and runs `npm audit signatures`; `OPENSPEC_VERSION`, `OPENSPEC_SHA256` and
  `scripts/openspec/package-lock.json` are gone, and the admitted-software table in `docs/systems/security.md`
  names a source instead of a version and hash.
- **ComfyUI rule files.** The two rule-file hashes `render/templates-catalog.mjs` compared against are no longer
  literals in the runner. `templates-catalog.mjs record-rules --comfy-dir <dir>` records them in
  `.offload-rule-files.json` in the ComfyUI folder when a checkout is registered or refreshed; readiness compares a
  node's current files with that record and says "not recorded" when there is none.
- **Why.** A pin freezes every install at one release and rots against the fleet; floors live in `setup/floors.json`
  and `go.mod`'s `go` line only (ADR 0099).
- **Upgrade notes.** A manifest that still names a `commit` for a node pack is accepted and the commit is not read.
  Re-run `fetch-models.sh` on each accelerator box once: a model with no record in `models.installed.json` is served as
  unverified until then, and a converted RKNPU model is recorded by `fetch-models.sh --convert` on the build host.
  `version drift --json` keeps its `cli_pinned_surface` field name for schema compatibility; `comfy features --json`
  renames `pinned_api_version` to `known_api_version`.
- **Docs.** No document describes a pin, a lockfile, a blessed version or an exact sha as policy any more. The accelerator
  page has a section on how the Coral and RKNPU artifacts are fetched and verified (resolve at fetch time, the publisher's
  digest, the `models.installed.json` record, `models_unverified`, `--dry-run`); the media-generation, operator, setup,
  fleet-node, run-graph and glossary pages say a node pack is admitted by repository and its commit is recorded, that
  HyperFrames is the newest npm release at or above the floor, and that the ComfyUI rule files are compared with the
  record `templates-catalog.mjs record-rules` writes (`.offload-rule-files.json`, "not recorded" when absent). The Coral
  design and the browse-lane plan carry a dated line saying their pins were replaced by resolve-at-fetch (ADR 0099); the
  ADRs that decided a pin (0007, 0023, 0059, 0060, 0062, 0081, 0090, 0098) keep their text and carry a
  `Superseded by ADR 0099 for the pinning mechanism` status line, mirrored in the ADR index.

## [0.203.0] - 2026-10-25 - The forecast lane: Timestral-209M time-series forecasting on any box, locally or through the fleet

### Added - the forecast lane: `offload_forecast` forecasts time series on Timestral-209M, on any box, locally or through the fleet

- **What it does.** A new model type, Mistral AI's Timestral-209M (zero-shot, probabilistic: 21 quantiles from 0.01 to 0.99,
  univariate or multivariate, past and known-future covariates, `null` for a missing value), runs as a llama-swap media seat of
  kind `forecast` that binds the config key `forecast_model`. The seat is resident (beside the agent seat) on `blackwell-16`,
  `blackwell-32`, `blackwell-2x16`, `blackwell-3x16`, `blackwell-48`, `blackwell-72`, `ampere-16` and `dual-gpu`, swappable on
  `ampere-8`, `blackwell-8` and `ampere-6`, and not seated on `volta-16`, the AMD tiers, `cpu` and `rockchip-rk3588`. It is not an
  accelerator: it uses the GPU, so it gets llama-swap's `ttl` 300, card pins, the GPU-lease fence and `/running`. The tool
  `offload_forecast` (also `local-offload forecast`, and a local-only agent-loop tool) takes `series` or a local `.csv` / `.json`
  `series_path` with `prediction_length`, `quantiles`, covariates, `out_path`, `route` and `select`. An answer of more than 2,000
  numbers is written to a file and the inline result keeps `result_path` and the first and last 8 median steps of each series.
- **Auto forwards.** `route` defaults to `auto`: this box's model when it binds one and its card is free, otherwise a fleet node
  that advertises the lane. This is new: the stt, vision and text tools defer on a model-less box in `auto`. A box that cannot run
  torch on a card (an NPU board, an AMD or CPU-only machine) reaches the model through the fleet. The node side is the dedicated
  door `POST /fleet/forecast` (bearer before body, 32 MiB cap, job id `forecast-<16 hex>`, the node's full `core.Result` as job
  data), advertised as `forecast` in `supported_task_types` with the additive health field `forecast_model`, and exempt from
  `fleet_max_concurrent_jobs` (a 0.2 s forecast must not queue behind a four-minute agent contract). Dispatch's 1 MiB cap is below
  one long multi-series request, which is why it has its own door.
- **Install.** `local-offload install forecast` (needs `uv`) builds `<home>/forecast/venv` (python 3.13, torch through
  `--torch-backend=auto`), installs the `timestral` package at a pinned commit, fetches the weights at a pinned Hugging Face
  revision into `<models>/timestral-209m` with a sha256 check, and writes the python server, which is embedded in the binary.
  The seat runs `<bin> forecast-serve`, which holds the python child under a kill-on-close job (Windows) or `Pdeathsig` (Linux).
  Design and alternatives: ADR 0098 (Proposed). Docs: `docs/systems/offload-pipeline.md`, `docs/FLEET-NODE.md`,
  `docs/systems/mcp-server.md`.
- **Measured.** On a 16 GB RTX 5060 Ti (sm_120, torch 2.14.1 with CUDA 13.2), on 2026-10-24: cold load 19 s, 804 MiB of fp32
  weights, a warm batch of 8 to 20 windows in 0.2 to 0.7 s at a 0.87 to 1.08 GiB peak, and 2.46 GiB with six covariate rows on
  20 series of 4,096 steps. Rolling-origin windows at the end of each series, MASE scaled by the context's seasonal-naive error,
  every output finite and every quantile set monotone:

  | Series | Windows x horizon, context | MASE Timestral / seasonal naive / naive | WQL Timestral / seasonal naive | 80 % coverage |
  |---|---|---|---|---|
  | ETTh1 OT, hourly | 20 x 96, 4,096 | 1.087 / 1.559 / 1.529 | 0.136 / 0.299 | 0.81 |
  | ETTh1 OT + 6 past covariates | 20 x 96, 4,096 | 1.087 / 1.559 / 1.529 | 0.134 / 0.299 | 0.82 |
  | ETTh1 OT, long context | 8 x 96, 16,384 | 0.639 / 0.893 / 1.028 | 0.108 / 0.204 | 0.91 |
  | ETTh1 OT, long horizon | 10 x 336, 4,096 | 1.494 / 2.028 / 2.439 | 0.242 / 0.463 | 0.73 |
  | ETTm1 OT, 15 minutes | 20 x 96, 8,192 | 0.520 / 0.910 / 0.692 | 0.077 / 0.183 | 0.86 |
  | The harness's own ledger, calls per hour (private, after the model's release) | 10 x 48, 1,901 | 1.442 / 2.122 / 1.711 | 1.096 / 4.231 | 0.79 |

  The median beats both baselines on every series (26 to 43 percent lower MASE than seasonal naive) and the quantile loss is
  about half of seasonal naive's or better. ETT is a public benchmark that may sit in the pretraining corpus, so those rows say
  "works and is calibrated", not "held out"; the ledger row is the clean one. Covariates did not move ETTh1 OT at horizon 96.
  The model card's GIFT-Eval figures and bf16 or fp16 were not run.
- **Weights licence.** The code is Apache-2.0. The weights are Modified MIT: the attribution "2026 - Mistral AI" is kept in
  copies, and there are no rights for a company whose global consolidated monthly revenue exceeds USD 20M (derivatives
  included). The harness surfaces this and does not gate on it.
- **Check.** The evaluation above, run on the real weights on a leased card, and the embedded server's every route hit once
  (`/health`, univariate, ragged batch, covariates, joint variates, and the 400 refusals). Then the built binary on a leased
  RTX 5060 Ti, with the seat served by llama-swap exactly as the tier renders it (ttl 300, card pin, `checkEndpoint /health`):
  `local-offload forecast` on the last 4,096 ETTh1 OT steps before a held-out 96 answered ok in 41.3 s cold (llama-swap start,
  load and warm-up) and 0.17 s warm, MAE 0.716 against the held-out steps, 0.97 of them inside the 10-90 band; the seat took
  1.25 GiB on the card; a box with no forecast model forwarded the same call through `POST /fleet/forecast` to a node serving
  the seat (job `forecast-<16 hex>`, placement `remote: no local forecast model`) and got the same answer; and llama-swap's
  `/running` was empty 330 s after the last call.

## [0.202.2] - 2026-10-24 - A harness-launched ComfyUI ends itself once idle instead of dying with its job

### Changed - a harness render keeps the ComfyUI it launched, and that ComfyUI ends itself once it is idle (ADR 0097)

- **What it does.** A harness runner leaves the ComfyUI it launched running after a GOOD job, holds it for the whole job with
  `GET /offload/hold` (a streamed response, one byte every 2 s, ended by the runner's exit), and the idle pack exits the process
  with `os._exit(0)` after `COMFY_IDLE_EXIT_SEC` seconds (unset is 300) with no prompt queued or running and no runner holding it.
  The next render inside the bound reuses the instance and skips the launch. A job that FAILED stops the instance it used, launched
  or reused, so a sticky CUDA error is still cured by the next launch. `COMFY_IDLE_EXIT_SEC=0` or `off` restores the zero-warm
  lifecycle of ADR 0009 (kill what the runner launched after every job). An explicit `--keep-comfy` keeps its old meaning. A reused
  self-bounded instance that was launched for another shape (a warm session against `--cache-none`, another `--reserve-vram`) is
  restarted. `GET /offload/idle` also answers `exit_limit_sec`, `exit_idle_sec` and `holds`; the launch marker records
  `idleExitSec`. In Go, `comfyinst.StopForRelease` is `StopForLease` that spares an instance with `idleExitSec`, and the media
  pipeline's routine lease release uses it, so keyed per-card and multicard instances also survive between jobs. `gpu reserve` when
  its command ends, its detached holder and `gpu release` still stop them.
- **Why.** Every render relaunched ComfyUI. On the 8 GB laptop card (ComfyUI 0.42.5, FLUX 3 Image Klein) two renders back to back
  took 135 s and 66.5 s, of which the launch to the first card allocation was 89 s (cold file cache) and 31 s (warm), while
  ComfyUI's own prompt ran 22.78 s and ComfyUI-Manager's prestartup took 5.2 to 11.5 s of each launch. After the end-of-job `/free`
  the bare process held 239 MiB of the card. The operator ruled on 2026-10-24 that this should not happen. The idle pack had no
  self-exit on purpose, because a runner in a long step that is not ComfyUI's looks abandoned; the hold is what tells the two apart.
  The exit is the engine's own idle clock (the llama-swap `ttl` analogue), read as the operator's yes under plan I5: the harness
  runs no timer or watcher.
- **Not changed.** Models still leave the card after every prompt (`--disable-smart-memory` and the end-of-job `/free`), so
  zero-warm for models stands; the model free (`OFFLOAD_IDLE_FREE_SEC`, 300 s) is unchanged and a hold does not block it. A hand-started
  instance that loads the pack for its model free never exits. Expected costs, not defects: a survivor holds about 240 MiB of the card
  and, measured live on 2026-10-14 on the 16 GB reference box (keyed single-card instance, ComfyUI 0.38.0), about 5.0 to 5.2 GiB of
  host private bytes, for at most the bound; under an outer `gpu reserve` the wrapper's job object still ends an instance launched
  inside it.
- **Check.** FLUX 3 Image Klein renders back to back, then 330 s idle, run live on 2026-10-24 with this change. On the 8 GB
  laptop card (default instance) three renders took 68.8 s, 36.4 s (66.5 s before) and 36.3 s on one ComfyUI process, `holds` read 1 during
  each job and 0 between, and the process ended itself at 304 s idle with the card back to 0 MiB. On the 16 GB reference box
  (keyed multicard instance, card-scoped leases) they took 74.4 s and 50.0 s on one process, each lease release logged "left the
  ComfyUI instance ... running", and the process ended itself at 302 s. ADR 0097 records the decision and supersedes in part the
  per-job process teardown of ADR 0009; the live check for the process bound is case E in `docs/systems/media-generation.md`.
- **Known gap.** Host-RAM admission still declares a render's full need while a reused survivor already holds about 5 GiB of it,
  so near the host's limit a render can wait when it would fit. It never over-admits.

## [0.202.1] - 2026-10-24 - A render is no longer held back by the host RAM its own unload frees

### Fixed - a render is no longer held back by the host RAM that its own unload frees

- **What was wrong.** A media lease (image, video and audio renders) was compared to the host's memory inside the lease grant,
  and the llama-swap models the render unloads were unloaded only afterwards, by the node runner. The comparison counted memory
  the render was about to free as held, waited for it, and deferred the render. On a 6 GB node a 4B llama-swap seat held 8 GiB of
  commit; an sd.cpp render that declared 6.5 GiB was told `waiting for host RAM: needs 6.5 GiB, committed 18.4 of 31.2 GiB
  physical, 8.0 GiB headroom` and never ran. `gpu reserve --unload-seat` had the same defect and was fixed first (F82).
- **What it does now.** When the host is short for a render's figure and at least one llama-swap model its own unload takes is
  resident, the lease takes the cards declaring nothing, the pipeline unloads those models (with a drain, under the lease), and
  only then asks the host. The wait is bounded by the call's `gpu_wait_ms` but never under 30 s after the unload, and never past
  `gpu_hostram_hold_max_sec`; past it the lease is released and the call is refused with the same words and place in line as
  before. A render with nothing of its own resident, an impossible figure, an unreadable host or no llama-swap endpoint keeps the
  old order exactly. The lane probe uses the same decision, so a ticket the grant admits is not reported as held by host RAM.
- **Check.** The incident's shape through `acquireMediaLease`: a resident seat, a host short by less than its footprint; the lease
  is granted declaring nothing, the seat is unloaded through llama-swap (the memory stack stays), then the figure is admitted and
  the record shows it as the declared need with the wait fields cleared. Details: `docs/systems/gpu-lease.md`, "A render lease
  follows the same order".

## [0.202.0] - 2026-10-24 - FLUX 3 Image Klein: the flux3-klein family on both engines, BFL's upsampler in two forms, four tier seeds

### Added - FLUX 3 Image Klein as the named image family `flux3-klein`, with BFL's prompt upsampler

- **What it does.** `generate-image --family flux3-klein` (and `offload_generate_image` with `family`) renders BFL's
  5.6B FLUX 3 Image Klein DiT on either engine. ComfyUI: the official template's graph (`render/wf-flux3-klein.mjs`:
  `CLIPLoader` type `flux3`, `EmptyFlux2LatentImage`, 16 steps / cfg 1 / euler / simple; a `base` checkpoint takes 50 /
  3.5), with card-only multicard rows `flux3-klein/bf16` and `flux3-klein/nvfp4` that free the upsampler and the text
  encoder before the DiT loads. sd.cpp: the leejet GGUF with the Qwen3-VL-4B text encoder and the FLUX.2 VAE.
  License FLUX Kommunity License v1.0, bound `commercial_use: true` by operator ruling.
- **The prompt upsampler is part of the model** (BFL: "do not use the model without a prompt upsampler").
  `imagegen_upsampler` takes BFL's upsampler file (ComfyUI runs it in-graph with BFL's t2i system prompt) or a
  llama-swap model id (either engine: the harness writes the caption before the media lease, then unloads the seat so
  the render gets its card and its host RAM). The result carries `upsampled` and `upsampled_prompt`, or
  `upsample_fallback`; `refine: false` renders the prompt verbatim. The model form is the fast one: BFL's PUP as a Q6_K
  GGUF on llama.cpp wrote a caption at 86-87 tok/s on a 5070 Ti, the same model in-graph at 4.3-7.6 tok/s, and a 16 GB
  multicard BF16 render took 63-70 s instead of 132-151 s.
- **Seeds.** `ampere-6` (sd.cpp, Q4_K + Qwen3-VL-4B Q4_K_M, `--params-backend disk`), `amd-gcn` (sd.cpp Vulkan, Q8_0 /
  Q8_0, its own master-1000 build), `blackwell-8` (ComfyUI, int8 DiT and encoder, the w4a8 upsampler in-graph, RAM
  mid/high) and `ampere-8` (ComfyUI, int8 DiT and encoder, the upsampler in the model form on the tier's
  `qwen3.5-9b-agent` seat, dynamic VRAM off, RAM mid/high) seed the family; the default image binding of every tier is
  unchanged. File set with sizes and sha256: `setup/SETUP-AGENT.md`.
- **Fixed on the way.** `generate-image --steps` help and the MCP schema said "default 30" (the default is the
  binding's); sd.cpp failures now name sd-cli's first `[E]` line and the `[W]` before it in the runner's error, where the
  cause used to fall off the 400 bytes the harness keeps.

### Changed - the Windows installer pins stable-diffusion.cpp master-1000; blackwell-8's z-image arm drops `--stream-layers`

- **Why.** FLUX 3 Klein needs sd.cpp master-968 or later and the pin was master-789. master-1000 has no
  `--stream-layers` (sd-cli exits with its usage text) and deprecates `--diffusion-fa`, so blackwell-8's z-image-turbo
  arm (flat keys and named family) is now `--vae-tiling --offload-to-cpu --flash-attn off,diffusion=on --max-vram 6.5`.
  A live node keeps its installed build and its old flags together until it reinstalls.
- **Check.** Every number through the harness route, 1024², fixed seeds (the full record is in the benchmark folder
  `2026-10-24-flux3-klein-fleet`): z-image-turbo on master-1000 on the 8 GB Blackwell reference box 54.0 s warm, sign text
  exact. FLUX 3 Klein: 6 GB Ampere (sd.cpp CUDA, Q4_K) 108.9 s; Vega 7 iGPU (sd.cpp Vulkan, Q8_0) 1102 s; 8 GB Blackwell
  (ComfyUI int8) 67-69 s warm; 8 GB Ampere laptop (ComfyUI int8) 64-78 s with the model-form upsampler against 304-488 s
  in-graph (with dynamic VRAM off the in-graph upsampler's generation buffer took the card and its weights streamed for
  every token); 16 GB Blackwell multicard BF16 132-155 s and NVFP4 110-126 s with the in-graph upsampler.
  NVFP4 is not better than BF16 on 16 GB cards (the DiT is 3 s of the wall; LPIPS 0.07-0.14 against BF16, small text
  degrades), and on 8 GB cards NVFP4's streamed DiT is 17-29 % slower than INT8's for the same image.

## [0.201.1] - 2026-10-24 - gpu_faults omits a zero last_incident

### Fixed - `gpu_faults` and `gpu faults --json` no longer print `0001-01-01T00:00:00Z` for a card with no incident

- **What it does.** A card with no incident used to print `last_incident` (on `/fleet/health`) and `last` (in `gpu faults
  --json`) as `0001-01-01T00:00:00Z`, because `omitempty` never omits a `time.Time`. Both fields now use `omitzero`, so
  they are absent when there is nothing to report, as the docs describe.
- **Why.** The first fleet readings after 0.201.0 showed the zero date on every incident-free card. It reads as data,
  not as "none".
- **Check.** Marshalling a card with no incident now gives
  `{"index":0,"uuid":"GPU-x","bus":"01:00","events":0,"incidents":0,"incidents_24h":0}` on the wire and
  `{"index":0,"uuid":"GPU-x","bus":"01:00","events":0,"incidents":0}` in the report.

## [0.201.0] - 2026-10-24 - a live driver-fault repeat demotes a card's placement

### Changed - a live driver-fault repeat demotes a card's placement (ADR 0096)

- **What it does.** A repeat on a card is live while its last burst is within 24 h. A repeat here is NVSentinel's rule
  as published in 0.200.0's `gpu_faults`: one fault class in 5 or more bursts within 24 h, restart and sleep bursts left
  out. A remote whose every agent seat for the contract sits on a card with a live repeat ranks right after saturation,
  in the auto placement (`betterRemote`) and in the spread deal (`fitPickWith`). Its seats come from the node's layer
  rows, as for a lease. It is demoted, never excluded, and the placement reason names it `faulted (<class> x<bursts> on
  <bus>, last burst <time>)`.
- **Local seat.** The delegator's own seat reads busy for the deal when a live repeat sits under every local seat of a
  subtask. That reading comes from the summary fleet-serve writes (`<state root>/gpu/faults-summary.json`), and the
  seats are resolved by `placement.LocalSeatCards`. It applies only when a remote could take the work and never for
  route=local. The capacity wait reads the seat afresh, so an overflow subtask may still come back to it.
- **Why.** A card that keeps resetting ends the contexts on it, so work placed there is cut off, deferred and placed again.
  The operator accepted the change on 2026-10-24. Driver events still move no lease verdict, gate, exclusion, drain or
  release: this amends register C-83-B for placement only.
- **Check.** I fetched real health views from three fleet nodes. Clean, node B beat node A. With a seeded live repeat on
  B's card, A won and B's reason read `faulted (nvlddmkm 153 x5 ...)`. With the same repeat ending 30 h ago, the order
  was unchanged.
  - On the reference box's own config (agent seat on cards 1 and 2) with its real summary:
    - a seeded live repeat on card 1 read the local seat faulted;
    - one on display card 0 alone did not;
    - the real repeat of 2026-10-11 (stale) did not;
    - route=local never did.
  - Details: [ADR 0096](docs/architecture/decisions/0096-a-live-driver-fault-repeat-demotes-a-cards-placement.md).

## [0.200.0] - 2026-10-24 - the fleet node publishes each card's driver faults

### Added - the fleet node publishes each card's driver faults (`gpu_faults`), retains incidents, and labels restart and sleep bursts

- **What it does.** `/fleet/health` gains `gpu_faults`: per card, the driver records and incidents of the last 30 days, the
  bursts at a restart or sleep transition, the bursts of the last 24 h, and NVSentinel's repeat rule where it is met. It
  is read in the background every 10 minutes (one read took 3.4 s on the reference box), served from memory, absent
  until the first read lands and on a node with no NVIDIA card. fleet-serve also appends each ended incident to
  `<state root>/gpu/faults.jsonl` and writes the summary to `<state root>/gpu/faults-summary.json`; `gpu faults` merges
  the retained incidents the OS log no longer holds, marked `retained`. Facts only: nothing reads them for placement in
  this release.
- **Restart and sleep context.** `gpu faults` labels a burst that overlaps a restart (User32 1074, EventLog 6005/6006/6008,
  Kernel-General 12/13, Kernel-Power 41 on Windows; a boot's first journal entry on Linux) or a sleep transition
  (Kernel-Power 42/107, Power-Troubleshooter 1; the kernel's `PM: suspend entry/exit`), and leaves it out of the repeat
  rule. Kernel-Power 566 is not read: it is a session transition (244 in the reference box's 30 days). Each burst now
  keeps its classes per card, so the repeat rule counts retained bursts too.
- **Why.** The 0.199.0 report was local and forgot what the OS log dropped. The fleet had no way to see which card has
  been faulting, and two of the reference box's 19 incidents were restart teardown counted like faults under work.
- **Check.** On the reference box, `gpu faults --days 30` labels 2026-10-15 22:02 and 2026-10-21 08:43 as restarts
  (User32 1074 then Kernel-General 12) and still fires the repeat on card `65`. The retention path with a fresh state
  root appended 19 incidents, then 0; five hand-written bursts on a day the log does not cover were listed as retained,
  counted on card `b5` and fired the repeat rule there, without counting the 19 live incidents twice. Details:
  [gpu-lease.md](docs/systems/gpu-lease.md#retention-and-the-fleet-nodes-summary),
  [FLEET-NODE.md](docs/FLEET-NODE.md#driver-fault-summary-gpu_faults).

## [0.199.0] - 2026-10-24 - gpu faults: the driver's fault history as incidents, with NVIDIA's known errors

### Added - `gpu faults`: the driver's fault history as incidents, with known-error classes and NVIDIA's action

- **What it does.** `local-offload gpu faults [--days N] [--jobs] [--json]` covers every card and up to 30 days.
  - It reads the NVIDIA driver's fault log (Windows `nvlddmkm`, Linux `Xid`) and, on Windows, the live kernel dumps
    Windows Error Reporting filed.
  - It classifies each event against a known-error table and groups records into 3-minute incidents, placed on cards
    by PCI bus.
  - It flags NVSentinel's repeat rule (one class in 5 or more bursts on one card within 24 h) and applies NVIDIA's Xid 13
    workflow to the GPC/TPC units that graphics exceptions name.
  - `--jobs` adds the ledger rows that ran on this node during each incident.
  - Facts only: nothing moves a lease verdict (C-83-B).
- **Why.** Driver faults were found by hand, weeks late, and read without context. The pattern comes from NVIDIA's AIOps
  "supervised log errors" and NVSentinel's Xid catalog and incident rules. Neither product runs here, so this carries their
  data (the Xid catalog, Apache-2.0, see NOTICE) and their rules. Windows ids are read as the Xid they match, and the output
  says so.
- **Check.** One real 30-day run on a three-card Blackwell GeForce box:
  - 479 records in 19 incidents, placed on buses `17`, `65` and `b5`.
  - The repeat rule fired on one card.
  - Both id-13 bursts read as the application's fault (spread over 35 and 3 units).
  - 11 live dumps, deduplicated by dump file from WER's re-queued records.
  - One incident joined to ten agent-pool jobs that all ended deferred.
  - `--json` parses.

  Details: [GPU lease](docs/systems/gpu-lease.md#gpu-faults-the-fault-history-as-incidents-with-known-errors).

## [0.198.0] - 2026-10-21 - vLLM seats keep Humming's JIT kernels in their own directories; the harness ships with go1.27.3 for GO-2026-6809

### Added - vLLM seats keep Humming's JIT kernels in their own directories, and ADR 0095 records where Humming serves

- **What it does.** `seat_fg.sh` exports `HUMMING_CACHE_DIR` and `HUMMING_TMP_DIR` under the seat's work directory (`humming-cache`, `humming-tmp`) and caps `HUMMING_MAX_PARALLEL_BUILD_WORKERS` at 8; `SEAT_HUMMING_CACHE`, `SEAT_HUMMING_TMP` and `SEAT_HUMMING_BUILD_WORKERS` override them, and each start logs `seat_fg: Humming kernel cache <dir> (at most N parallel builds)`. `linux-systemd/vllm-seat-run.sh` sets the same under the stack's `cache` directory. The WSL seat env template documents the `--linear-backend humming` opt-in, commented out, with the measured warning below.
- **Why.** vLLM 0.32 installs Humming 0.1.17 as a pinned dependency and serves every 2, 3, 5, 6 and 7-bit checkpoint on it. Left at its defaults it kept compiled kernels and the modules the engine imports in root's home, keyed to the venv path so nothing reused or cleaned them, and compiled with one process per CPU on a first forward.
- **Not changed.** 4-bit seats stay on Marlin. In the proving run (two 16 GB SM120 cards, 27B INT4, utilization 0.90) `--linear-backend humming` gave the same output and footprint, and its engine stopped during a 23.7k-token prefill that Marlin served, ended by the 120 s step watchdog. Cause not established; ADR 0095 has the table.
- **Check.** Both arms of that run started through the changed launcher and logged the cache line; the Humming arm compiled 94 kernels (51 MB) into `humming-cache` on its first start (291 s) and reused them on the next (175 s). The security doc admits Humming 0.1.17 (Apache-2.0, no network code).

### Security - the harness ships with go1.27.3, which carries the crypto/x509 fix for GO-2026-6809

- **What it does.** `go.mod`'s toolchain line names go1.27.3 (was go1.27.2), so every build, the release binaries and the govulncheck job run under it; the pin test's floor moves to go1.27.3, so a downgrade fails the test.
- **Why.** GO-2026-6809 (crypto/x509 distrusts compromised intermediate certificates) was published on 2026-10-21 against go1.27.2 and is fixed in go1.27.3. govulncheck found the harness reaches it through `cliutil.ProbeReachable` (an HTTPS probe), in the root module and in `tools/llamaswap`, and failed CI on every branch.
- **Not changed.** No module requirement moves: the two module advisories the scan also lists are not called.

## [0.197.0] - 2026-10-20 - The qwen-image-2.1 edit runs pooled on the two non-display cards, measured; the multicard launch notes move to doctor

### Added - the qwen-image-2.1 edit takes the card-only two-card placement, and the multicard launch notes move from every command to `doctor`

- **What it does.** `--placement multicard` on `render/comfy-edit.mjs` now serves `--family qwen-image-2.1` (`render/multicard-recipes.json`, `render/multicard-recipes.mjs`, `render/wf-qwen-image-21.mjs`, `render/comfy-edit.mjs`, `internal/mediacap/multicard.go`, `internal/config/config.go`). Two rows, `qwen-image-2.1-edit/bf16` and `qwen-image-2.1-edit/int8_convrot`, take the loaders, resident shares and compute-first card order of the measured 2.1 image rows. The graph reads the conditioning through node 14, `OffloadFreeAfterEncode`, after the text encode (the vision tower and the reference VAE encode run inside it), so the 16.33 GiB text encoder leaves both cards before the DiT lands. `--cache-device` defaults to `gpu` under the placement and takes `gpu` or `off` only: `auto` and `cpu` can pin the prefix cache in host RAM, and the runner and the builder refuse them by name (a bound `gen_edit_cache_device` of `auto` or `cpu` on a multicard 2.1 edit binding is also warned of at load). The edit canvas is judged on the DiT sequence (the target counted twice plus every reference, each `--resolution` squared) against the rows' `maxPixels` of 4,194,304, and a refusal names the largest `--resolution` that fits. The bf16 row, the one the live 2.1 edit bindings name, is `measured` from one real render (below; Go `Measured: true`); the int8 convrot row stays `predicted`, its peaks ceilings until a render reads them. The load-time recipe-family check (`editRecipeFamilies`) now includes the family, and `docs/systems/media-generation.md` has the rows and a paragraph on the edit specifics.
- **What it fixes.** After the media placements went multicard, every `local-offload` command (even `gpu status`) printed the same twelve `comfy_cuda_device` / `comfy_dynamic_vram` warning lines, and printed them more than once per process. Root cause: `warnImageGenBindingTraps` runs from `load` (`internal/config/config.go`) on every `config.Load`, and each consumer loads the config again (`gpu status` loads it five times through `loadCfg`), so every binding warning was said once per load; and `warnMulticardLaunchTo` repeated the flat-setting warning for the default binding and again for every named family that only inherits it. Now the binding-trap warnings are written once per process (a line already said is not said again), and the two informational notes are not warnings at all: `Config.MulticardLaunchNotes` names the multicard seats (default image and edit bindings and the families that resolve to multicard) once per setting, and `local-offload doctor` prints them as `multicard:  NOTE` rows (`main.go`). The real misconfiguration warnings (no recipe for the family, card-scoped leases off, pooled-seat traps) are unchanged.
- **Why.** The 2.1 edit was the one family that had a card-only image placement and no edit one, so it could not run on the two non-display cards without host RAM. The flat `comfy_cuda_device` and `comfy_dynamic_vram` still serve the single-card routes (inpaint, upscale, the single-pinned edit families), so a config that keeps them next to multicard seats is deliberate and warning of it on every command taught the operator to stop reading the warnings.
- **Check.** One real card-only render with the bf16 files: one 1024x1024 reference and the prompt to change the blazer's colour, `--resolution 1024`, 40 steps, cache device `gpu`, on a lease of the two non-display cards with 0 GiB host RAM declared: 197.0 s at 1.64 s per step. The encoder peaked at 9.65 GiB on the compute card and 9.69 GiB on the storage card and was freed in 0.8 s before the DiT. The sampler peaked at 11.49 and 6.06 GiB, and host free memory came back to within 2.5 GiB of its start. Viewed at full size, the image is the asked edit, with the woman, the pose, the slide and the office kept. The row's compute figure (14.5 GiB) is the sampler scaled to the cap, as the 2511 rows' are, and its storage figure keeps the predicted 11.0. Other real runs: `planEdit` on two real PNG headers (1024x1024 and 2048x2048) built the bf16, int8 convrot and Turbo edit graphs under the placement (loader classes, node 14 and the sampler's conditioning read through it, cache device `gpu`), refused `--cache-device auto`, refused four images at 1024 (5.24 MP against the 4.19 MP cap) and one 2048x2048 image at `--resolution 0`, and refused a 2.1 DiT file with no row. A built binary run with a copy of a config that binds multicard on the image seat, the edit seat and one family, with `comfy_cuda_device "2"` and `comfy_dynamic_vram "on"`, printed neither note on `gpu status` (every remaining warning once, where it had printed five times) and printed each once under `doctor`. `go build ./...`, `go vet` and `go test` on the media packages, the render suite (`node --test render/*.test.mjs`, 0 failures), `go run ./cmd/changelog check` and the leak gate pass. Tests edited to the new truth, none added: the recipe table, the measured list and the 2.1 edit refusal cases in `render/multicard-recipes.test.mjs` and `render/multicard-builders.test.mjs`, the row count, the measured count, the lookup case and the node-class map in `internal/mediacap/multicard_test.go`, and the placement and launch-warning cases in `internal/config/placement_test.go`.

## [0.196.0] - 2026-10-20 - Large media bindings run pooled on the two non-display cards: qwen-image 2512 Q5_1, krea2, qwen-image-2.1, the 2511 edit, Wan 2.2 Q8_0 and LTX-2.5

### Added - the card-only two-card placement runs LTX-2.5: `--wan-placement multicard --model ltx25`, and the typed `video` door can choose it from config

- **What it does.** `--wan-placement multicard` (`render/wan-multicard.mjs`, `render/wf-ltx25-i2v.mjs`, `render/comfy-video.mjs`) also places the LTX-2.5 family (`--model ltx25`); every other non-Wan model and a `--graph` file are still refused. The 20.03 GiB int8 convrot DiT and the 14.32 GiB Gemma text encoder run pooled across the lease's two non-display cards through DisTorch2, the donor being the other card and never system RAM. The roles' device numbers are the reverse of the Wan placement's, because an int8 DiT that computes on any device other than cuda:0 faults with an illegal memory access (ComfyUI-MultiGPU issue #220, archived upstream, no fix coming): the launch (`multicardLaunch`, `order: "compute-first"`) lists the compute card first in `CUDA_VISIBLE_DEVICES`, so cuda:0 is the compute card and cuda:1 the storage card. That launch has its own instance key, `mcc-<compute uuid head>-<storage uuid head>`, so a kept storage-first ComfyUI (`mc-...`) is never reused for LTX-2.5 or the other way round, and the device check after the start (`checkComfyDevices`) compares cuda:0 with the compute card and cuda:1 with the storage card. The Wan and image launches are unchanged. The graph (`multicard: { residentGb, teAllocation }` on `buildLtx25I2V`; the single-card and `poolVvramGb` paths are untouched) loads the DiT through `UNETLoaderDisTorch2MultiGPU` (`compute_device` `cuda:0`, `expert_mode_allocations` `cuda:0,<R>gb;cuda:1,*`, `virtual_vram_gb` 0, `donor_device` `cpu`, `eject_models` false, `weight_dtype` `default`), the encoder through `OffloadCLIPLoaderDisTorch2` (`type` `ltxv`, `device` `cuda:0`, split `cuda:0,7gb;cuda:1,*`) and both VAEs through `VAELoaderMultiGPU` on `cuda:1`. The DiT and the encoder (34.4 GiB) do not fit the two cards (31.8 GiB) together, so a new node of the harness's pack, `OffloadFreeAfterEncode`, sits between the prompt's conditioning and the two guiders: it drops the encoder from ComfyUI's loaded-model list, empties every weight it holds on a card, returns the cached blocks to the driver, logs each card's memory before and after, and stops the run with `OFFLOAD-TE-STILL-RESIDENT` if any of the encoder's tensors is still on a CUDA device, all before the first sampler places the DiT (the pack runs with `--cache-none`, so the next prompt builds and places the encoder again). Two more pack changes keep the split DiT on its cards. The **split-model eviction guard** (installed with the read-only reader, under `--gpu-only` only) wraps `LoadedModel.model_unload` so that a model DisTorch2 placed, or whose weights sit on a card other than its offload device, is kept where it is and logged instead of being gathered onto its offload card: under `--gpu-only` that device is the card itself, so loading the latent upscaler between the two samplers used to copy the whole split DiT onto cuda:0 and run it out of memory. `OffloadFreeForDecode` takes a new `check` input (default true, so the Wan graph is unchanged); the LTX-2.5 graph puts it between the second sampler and the audio-video split with `check` false, which empties both cards before the two tiled decodes and skips the plain-decode fit test. The pack's weight sizes now count a quantized tensor's real storage (`nbytes`), where they counted an int8 weight at its logical bf16 size and logged the 14.32 GiB encoder as 24.49 GiB. `--wan-resident-gb` is, for LTX-2.5, the GiB of the DiT kept on the compute card (default 9, so the storage card holds the other 11 GiB and both VAEs). The planner refuses before ComfyUI starts a share the compute card plainly cannot hold (more than its size less the 1 GiB latent upscaler and 0.5 GiB of context, 14.4 GiB on a 15.9 GiB card), a share that leaves the storage card more than it holds, a non-`.safetensors` or bf16 (about 39 GiB) `--transformer`, and `--pool-vvram-gb`; it keeps the display-card refusal, the lease cross-check and the pack check (the MultiGPU pack and the harness's own, now including `OffloadFreeForDecode` for LTX-2.5). Typed door: `videogen_placement: multicard` (flat key or `videogen_families["ltx25"].placement`, with `videogen_wan_resident_gb` for the share) leases the box's two non-display cards as a set, passes `--wan-placement multicard` (and `--wan-resident-gb` when configured), drops the single-card pin and dynamic VRAM and the pool flags, and declares the stage-rule host RAM through `hostneed.ForVideoCardOnly` (0 GiB when the DiT's share, the latent upscaler and the context fit the compute card and the rest of the DiT, both VAEs and the context fit the storage card; the encoder is gone before the DiT lands); a `.gguf` or bf16 transformer is refused before any lease. `doctor` lists the card-only graph's node classes for the pack check.
- **Why.** The house rule is that every model bigger than one 16 GB card runs pooled across the two non-display cards with DisTorch2, with the other GPU as donor and never system RAM. LTX-2.5 had only the older ratio-mode pooled path (`poolVvramGb`, a virtual-VRAM donor), which is not that placement, and the Wan one could not be reused as is: its storage-first card order puts the int8 DiT's compute on cuda:1, the case that faults upstream.
- **Check.** One real render on the two non-display cards through the runner, with the pipeline's own arguments (an image-to-video clip of a presenter at 1280x704x121, 24 fps, the int8 DiT and encoder, the default shares). The first attempt ran out of memory at the latent upscaler, with the compute card at 15.3 GiB, when ComfyUI's unload gathered the split DiT onto cuda:0. That failure is why the eviction guard and the decode release exist. The second attempt finished in 124.0 s of ComfyUI time (146 s runner wall), declaring 0 GiB of host RAM.
  - The encoder was placed in 9.8 s, 7.07 GiB on the compute card and 7.26 GiB on the storage card by the allocator, and released in 0.8 s, leaving 14.51 and 13.40 GiB free.
  - The console shows `kept LTXAV on its cards` when the upscaler loaded. The first sampler ran 8 steps at 2.88 s/it and the second 3 steps at 5.66 s/it.
  - The decode release took the cards from 3.34 and 2.21 GiB free to 14.46 and 13.40 GiB in 0.9 s.
  - The per-card peaks (NVML) were 11.54 GiB on the compute card and 12.71 GiB on the storage card. Host free memory dipped from 84.7 to 63.4 GiB while the encoder was staged on its way to the cards, and was 81.3 GiB at the end.
  - The mp4 was probed (1280x704, 121 frames, 24 fps, 5.04 s, AAC 48 kHz stereo with speech-level audio after the first second) and its frames viewed (a coherent presenter scene).
  - The pack's Python tests, the render suite and the Go packages pass. A larger share or clip (1920x1088) is unmeasured, and the `LTX-PLACEMENT-NOTE` says so on every run.

### Fixed - a place in line that waits only on host RAM no longer holds its cards against a request that declares none

- **What it does.** A media ticket's place token now carries `waiting_for` as a waiter does: the ticket's scan marks it `host-ram` when its lane reading found nothing in the way but host RAM (no lease on the cards, nobody ahead) and clears the mark otherwise (`core.PlaceMarker`, `gpulease.Manager.MarkToken`). A marked token does not hold its cards against a request that declares no host RAM, in the waiter line, in the queue-timeout's "who is ahead", in `gpualloc.QueuedClaims` and in the media lane reading (`Token.HoldsItsCardsAgainst`). A waiter on host RAM already passed such a request (the G6 rule); a token did not.
- **Why.** On 2026-10-20 two image tickets short of 46.8 GiB of host RAM held their places for three hours on an idle box, and a two-card render that declared 0 GiB waited 36 minutes behind them for cards nobody was using.
- **Check.** One test leaves a token, marks it as waiting on host RAM, and requires a request that declares none to be at the front of the line (`TestAPlaceWaitingOnHostRAMDoesNotHoldBackARequestDeclaringNone`). Details: `docs/systems/gpu-lease.md`, "Host RAM".

### Added - krea2 bf16 runs card-only across two 16 GB cards, and an unquantized DiT is no longer built whole on one card first

- **What it does.** `render/multicard-recipes.json` gains the `krea2/bf16` row (Go mirror in `internal/mediacap/multicard.go`): the 24.48 GiB bf16 DiT through DisTorch2 with 11 GiB resident on the compute card and the rest on the storage card, beside the Qwen3-VL-4B bf16 text encoder (built whole on the storage card for the encode and freed before the DiT's blocks land), measured, with a 1.5 MP canvas cap. The CLIP loader's `type` comes from the row (`clipType`), and `imagegen_placement: multicard` takes the krea2 binding. The harness's ComfyUI pack (`render/comfyui-ext/nodes/offload_multicard`) builds a diffusion model whose every tensor is a plain float, and that names no operations of its own, with ComfyUI's mixed-precision layers and an empty quantization config, under the same `OFFLOAD_RO_LOAD=1` as its read-only reader: the model is built without weights, takes the read-only map's tensors without a copy, and its blocks reach the cards only through DisTorch2's allocation. GGUF and quantized checkpoints keep ComfyUI's own path.
- **Why.** Stock ComfyUI under `--gpu-only` builds an unquantized DiT with real weight tensors and moves it whole to the offload card before DisTorch2 splits it (`comfy.sd.load_diffusion_model_state_dict`), so the first card-only krea2 run ran out of memory on the storage card 23 s in, with 15.03 GiB allocated. The mixed-precision path is the one every unquantized layer of an fp8mixed checkpoint already took in the measured 2511 edit.
- **Check.** Real two-card renders on 2026-10-20 (RTX 5070 Ti computing, RTX 5060 Ti storing): 1024x1024 with 12 GiB resident in 82.1 s, peaks 14.30 and 12.10 GiB; 1536x1024 with 11 GiB resident in 68.5 s, peaks 13.15 and 13.10 GiB. Host physical memory came back to within 2.5 GiB of its start after the weights streamed in. The 2511 fp8mixed edit (lightning8, 1024x1024) still ran on the same pair in 110.6 s, peaks 11.40 and 10.60 GiB.

### Changed - the qwen-image 2512 Q5_1 card-only row keeps 10 GiB of its DiT on the compute card and is measured, so the family default canvas runs

- **What it does.** The `qwen-image/gguf-q5_1` row's resident share is 10 GiB (was 3), its peaks are the measured 11.9 GiB on the compute card and 8.3 GiB on the storage card, its basis is `measured` (so a card-only call declares no host RAM), and its canvas cap is 1,769,472 pixels (1536x1152), which takes the family default 1328x1328 and 928x1664.
- **Why.** At a 3 GiB share the storage card held most of the DiT beside the text encoder and ran out of memory at 928x1664, and the cap that kept the default canvas off the row left the qwen-image binding with no card-only canvas a caller would ask for.
- **Check.** Real two-card renders on 2026-10-20, full preset (50 steps, cfg 4): 1328x1328 in 415.2 s, peaks 11.84 and 8.26 GiB; 928x1664 in 401.3 s, peaks 11.74 and 8.26 GiB; host physical memory within 3.5 GiB of its start through both.

### Added - qwen-image-2.1 bf16 and int8_convrot run pooled on two cards: compute-first launch, the 16.33 GiB text encoder split over both cards and freed before the sampler, both measured at 2048x2048

- **What it does.** The `qwen-image-2.1/bf16` and `qwen-image-2.1/int8_convrot` rows of `render/multicard-recipes.json` (and their Go mirror in `internal/mediacap/multicard.go`) are no longer held back, and both are now `measured`, so each declares 0 GiB of host RAM on the lease. Its text encoder loads through `OffloadCLIPLoaderDisTorch2`, a new node of the harness's own pack (`render/comfyui-ext/nodes/offload_multicard`), named by the new `distorch2-host` clip loader kind. The node runs ComfyUI-MultiGPU's `CLIPLoaderDisTorch2MultiGPU` with ComfyUI's text-encoder initial device answered with the CPU for that one load, then places the encoder at once through DisTorch2's own load path. It stops with `OFFLOAD-TE-HOSTWEIGHT` if more than 64 MiB of weights are left in host RAM. Both 2.1 rows carry `"cardOrder": "compute-first"`. Their pair instance starts with the compute card listed first (`cuda:0` computes, `cuda:1` stores, instance key `mcc-...`, the LTX-2.5 order), and `rowDevices` in `render/multicard-recipes.mjs` puts every loader on the matching device. The 2.1 multicard graph routes its conditioning through `OffloadFreeAfterEncode`, which frees the encoder from both cards before the DiT loads. `qwen-image-2.1` joins the families with a launchable image recipe (a 2048x2048 canvas cap, the family default), and `imagegen_placement: multicard` no longer warns for it. The int8 convrot row uses the same encoder loader and release, and its int8 DiT loads whole on `cuda:0`, where comfy_kitchen's int8 kernels run; no row is held back now (the hold mechanism stays in the code).
- **Why.** Under `--gpu-only` ComfyUI builds the whole text encoder on one card before the DisTorch2 allocation applies, so the stock loader ran out of memory 1.4 s in (14.10 GiB allocated). With the encoder fixed, a storage-first render then aborted at the first sampler step (`Fatal Python error: Aborted`, 406 s in). The 2.1 model calls comfy_kitchen fused kernels, which run on `cuda:0` only, and ComfyUI-MultiGPU CPU-staged tensors from `cuda:1` to `cuda:0` through host RAM (upstream issue 220). The row's old evidence line also called the encoder's share 13.3 GiB. That is the bf16 DiT file's size; the encoder is 16.33 GiB, and the row says so.
- **Check.** One real render on the 5070 Ti (compute) and a 5060 Ti (storage), with 0 GiB host RAM declared on the lease: 2048x2048, 40 steps, 280.0 s at 5.57 s per step.
  - The encoder placed in 7.3 s (`cuda:0` 8.33 GiB, `cuda:1` 8.00 GiB, 0 on the CPU). The release freed both cards in 0.8 s (14.48 and 14.80 GiB free).
  - The DiT was built empty and landed 8 GiB on `cuda:0` and 5.25 GiB on `cuda:1`. Peaks were 10.90 GiB on the compute card and 8.12 GiB on the storage card.
  - Host free memory dipped only while weights streamed to the cards (64.6 and 75.0 GiB, from 83.9) and held at 81 GiB through sampling.
  - No CPU-staging line appeared in the console.
  - The image was read back at full size: coherent, sharp, on the prompt.

  The int8 convrot row, run the same way (2026-10-20, 0 GiB host RAM declared): 2048x2048, 40 steps at 2.57 s per step, 179.9 s.
  - The encoder was placed `cuda:1,5gb;cuda:0,*` (`cuda:0` 11.36 GiB, `cuda:1` 4.97 GiB) and freed by `OffloadFreeAfterEncode` before the 6.76 GiB int8 DiT loaded whole on `cuda:0`.
  - Peaks were 12.27 GiB on the compute card (encoder phase; 9.04 GiB while sampling) and 5.84 GiB on the storage card (VAE decode). Host RAM only dipped while the encoder was staged.

  `node --test` on the render suite (2,272 tests, 0 failed), the pack's Python node tests, `go build ./...`, `go vet`, and the `mediacap`, `hostneed`, `config` and `pipeline` tests. Details: `docs/systems/media-generation.md`, "Card-only image and edit".

### Added - the card-only Wan 2.2 placement runs the Q8_0 GGUF experts, and the typed `video` door can choose it from config

- **What it does.** `--wan-placement multicard` (`render/wan-multicard.mjs`, `render/wf-wan22-i2v.mjs`) no longer refuses a `.gguf` expert. A Q8_0 expert (14.349 GiB each) loads through `UnetLoaderGGUFDisTorch2MultiGPU` with the same compute card and allocation string as the fp8 safetensors experts (`cuda:1,<resident>gb;cuda:0,*`), `virtual_vram_gb` 0, `eject_models` false and no `weight_dtype`; the donor is the storage card, never system RAM. The planner sizes a `*Q8_0.gguf` expert at 14.4 GiB in its resident-share check (13.4 for fp8), adds ComfyUI-GGUF to the launch's custom-node whitelist, and a kept ComfyUI launched without that folder is replaced (or refused when it is not the harness's own) instead of being reused. `--fast` and an unnamed expert (the builder's defaults are the Q4_K files) are still refused before anything starts. New config keys `videogen_placement` (`""`, `single`, `multicard`; an unknown value is a config error) and `videogen_wan_resident_gb` (0 = the runner's default), also `placement` / `wan_resident_gb` in a `videogen_families` entry: for the Wan 2.2 graph the `video` door then leases the box's two non-display cards as a set (the edit door's lease path), passes `--wan-placement multicard` and `--wan-resident-gb`, drops the single-card pin and dynamic VRAM, and declares the stage-rule host RAM (`hostneed.ForVideoCardOnly`; 0 GiB for a Q8_0 pair on two 15.9 GiB cards). A multicard binding that names no expert is refused before any lease.
- **Why.** The Q8_0 pair is the quality weight binding but fits no single 16 GB card beside the sampler, and the placement that spends a second card instead of host RAM only took the fp8 experts, so a Q8_0 box had to stream through host RAM. The typed `video` door had no multicard door, so a harness call ran single-card plus RAM whatever the runner could do.
- **Check.** The multicard builder test now asserts the GGUF loader node for a Q8_0 pair (class, compute device, allocation, no `weight_dtype`, `eject_models` false) and the planner test that a GGUF expert is accepted with ComfyUI-GGUF in the launch's packs; the single-placement golden is unchanged. The host-RAM figure and the config validation were read back from a real run of `hostneed.ForVideoCardOnly` and `config.Load`. One real render (2026-10-20) of the Q8_0 pair on a 300 W compute card and a 180 W storage card, both 15.9 GiB, under `gpu reserve --devices` with 0 GiB host RAM declared: 1280x720, 81 frames at 16 fps, 2 GiB of each expert resident, plain decode, 3,330.6 s from execution start to success; per-card peaks 10.40 GiB on the compute card and 13.01 GiB on the storage card (`nvidia-smi` at 1 s), the display card untouched. Host free physical RAM dipped for a few seconds while each expert streamed onto the cards and recovered at once; it held steady through sampling. The clip was inspected frame by frame: coherent motion, no jumps (largest scene-change score 0.09), no brightness flicker.

## [0.195.0] - 2026-10-20 - The Linux cage guards every thread and arm64 and reports AppArmor refusals, refusals name their rule, keyed ComfyUI instances get their own database

### Fixed - each keyed ComfyUI instance has its own database, so a second instance no longer waits 5 s on a locked file (register GPU-F8)

- **What it does.** A keyed instance now launches with `--database-url sqlite:///<comfy dir>/instances/<key>/comfyui.db`, beside its `output` and `temp` folders and inside neither. The default instance's argv is byte for byte what it was, and its own `--database-url` in `COMFY_EXTRA_ARGS` still passes through; a keyed instance drops one with a `COMFY-INSTANCE-WARN:` line, as it does a `--port`, `--output-directory` or `--temp-directory`. A running instance keeps the argv it started with and is still reused (the database flag is not an ownership criterion). There is no per-instance `--user-directory`: the flag requires the directory to exist already (ComfyUI exits at argument parsing otherwise), and the user directory also holds the settings, workflows and ComfyUI-Manager configuration that every instance should keep sharing; the database is the only process-exclusive file there.
- **Why.** ComfyUI's `--database-url` (`comfy/cli_args.py`: `type=str`, `default=None`, "Defaults to 'comfyui.db' in the effective user directory") left every instance on a box on one file, and `app/database/db.py` holds a process-exclusive file lock on it: every instance after the first waited 5 s at start, logged "Database is locked. Another ComfyUI process is already using this database." and ran without a database (with `--enable-assets` it would have exited). Only the asset system uses the database, so the lock had disabled no feature the harness uses; this removes the 5 s start-up wait and the error, and makes `--enable-assets` workable per instance.
- **Check.** Checked against ComfyUI's own argument parser and `init_db` in scratch folders (two keyed URLs came up in about 0.5 s each; one shared file made the second instance fail after 5 s), then live on one node: two keyed instances, the second started while the first was up. Under the previous argv the second logged "Database is locked"; under this one neither did, and each had its own `<comfy dir>/instances/<key>/comfyui.db`. The evidence, the user-directory decision and what to check are in `docs/systems/media-generation.md`.

### Fixed - the browse lane names the Allow prompt when a CDP handshake times out (register C-93)

- **What it does.** `setup/browse/runner.py` `_classify` treats a CDP WebSocket handshake that times out or is rejected with HTTP 403, or browser-harness's `permission-blocked`, as the browser's "Allow remote debugging" prompt waiting on the operator. The run ends `BROWSER_UNAVAILABLE` with a reason that names the prompt and the dedicated browser (`browse_cdp_url`) and keeps browser-harness's first clause as the evidence, instead of its hint about network policy and cloud browsers. A run pinned with `browse_cdp_url` gets a hedged reason (the endpoint did not finish the handshake; check that it is running and responsive; a browser that asks per connection is waiting on Allow), since a dedicated browser does not prompt. An unreachable endpoint, a hung `/json/version` probe, a refused handshake and a daemon that never came up keep their own text. Rerun `setup/browse/install.ps1` to get it.
- **Why.** The browser was up and waiting for a click an unattended run never gives, and the lane uses no cloud browser, so the old text sent the operator to the wrong place. `docs/systems/browse-lane.md`, the browse section of `docs/OPERATOR-GUIDE.md` and ADR 0060 (a dated amendment) now lead with the dedicated agent browser, describe the toggle as the attended alternative, and write down the outcomes: a named browser ends `BROWSER_UNAVAILABLE` after about 10 s, a `browse_cdp_url` pin gets the hedged reason, and a lane left to discovery has no handshake deadline, so an unanswered prompt ends `TIMEOUT` at `browse_timeout_sec` (behaviour unchanged, now documented).
- **Check.** One test drives `_classify` with the text browser-harness raises for a hung http:// pin, pinned and unpinned, and requires it to stay out of the Allow-prompt class.

### Fixed - the Linux cage no longer reports itself usable where AppArmor refuses its user namespace (register SF-46)

- **What it does.** `sandbox.Available()` now creates the cage's namespaces once per process, with the same attributes the cage uses, around a system `true`, and reports a refusal by name. Where `kernel.apparmor_restrict_unprivileged_userns=1` (the Ubuntu 24.04 default) refuses a user namespace to an unprofiled binary, the reason says so. The agent then logs `shell requested but OS sandbox unavailable (...) — NOT granted (fail-closed)` and advertises neither `run_shell` nor `run`, where before it logged `shell ON` and every caged call failed with `fork/exec ...: permission denied`.
- **Not changed.** Nothing ran uncaged before either: the cage failed closed. A host that should run caged commands needs an AppArmor profile that allows a user namespace for the harness binary, or the sysctl lowered; this change does neither.
- **Why.** The two sysctls `Available()` read were not the whole gate, so a model spent steps on tools that could not run, and the log said the cage was on.

### Security - the Linux cage guards the compat ABI on every thread, denies `pidfd_getfd`, lets node run, and guards arm64 (register SF-17, SF-44)

- **The architecture guard reaches every thread.** The guard that kills a foreign-arch syscall (i386 `int 0x80` on amd64) was installed on the installing thread only, while the deny list used TSYNC. The Go runtime may run the final exec on another thread, and the caged image then carried the deny list without the guard. Measured in WSL under a 24-way CPU load: 102 of 1500 i386 calls escaped the old cage, 0 of 1500 with the guard installed with TSYNC. A thread that cannot be synchronized now fails the install instead of passing silently.
- **`pidfd_getfd`** copies a file descriptor out of another process; it is now on the deny list. `pidfd_send_signal` stays allowed, because Go's `os` package signals children through it on the first child spawn, so denying it kills `go build` and `go test` in the cage. `memfd_create` stays allowed, because denying it buys no containment: Landlock grants execute wherever it grants read, so a caged command may already write a program into its scratch dir and run it, and an image started from a memfd inherits the same cage.
- **io_uring fails with EPERM instead of killing.** libuv probes `io_uring_setup` at startup, so the kill took node down with it: `node -e`, node file I/O and `npm --version` all died by SIGSYS in the cage. The ring stays unavailable, as Docker's default profile and `kernel.io_uring_disabled=2` keep it.
- **arm64.** The architecture guard allowed only the x86_64 audit arch, so on an arm64 node, where `Available()` reported the cage usable, every syscall of every caged command was killed. The guard now allows the native arch (x86_64 on amd64, aarch64 on arm64) and kills every other, which still closes the compat ABIs. `Available()` checks the arch first, so an architecture the cage cannot guard is refused on any host instead of caged wrongly. Every filtered name is in both the amd64 and arm64 syscall tables, since an unknown name fails the whole filter load.
- **Why.** A caged command could reach the deny list's blind spot through the compat ABI on a thread the guard missed, an arm64 node could not run anything caged, and node could not run caged at all.

### Security - a refused agent action says which rule refused it and what to do instead (register SF-25)

- **What it does.** A deny or an ask from the agent broker used to reach the model as one line, `NOT performed (deny): <reason>`, naming neither the rule that fired nor a way forward, so a model tended to retry the same action. `Policy.Explain` returns the same decision and reason as `Decide`, with the same single audit record, and adds the rule (a table rule as `<kind> <glob> [<severity>]`, or `built-in`) and a concrete next step. Every refusal from a tool's own broker call now returns three lines: the original first line, then `rule:` and `next:`. That covers the write, edit, delete, run, shell, fetch, search and browse tools, and browse's allowlist miss. The read floor keeps its `read refused:` first line and gains the same two lines. A line break inside a rule's reason is flattened, so the body stays three lines.
- **Still one line.** A fetch redirect to a host off the allowlist (it reaches the model as a failed fetch), the GitHub tools' egress refusals, and the URL checks the model can fix itself (scheme, port, embedded credentials).
- **The next step follows the cause first, then the kind.** The audit trail cannot be written: every action is refused alike, so the model is told to stop retrying and report it. An unattended ask: the step names an operator decision outside the run. A deny rule fired (the secret floor or the operator's table): leave the path as it is and do not put the same content under another name. The built-in floor or a capability the run lacks: an alternative for that kind of action. No next step offers an approval verb; the approval verb (S-23) is a separate decision.
- **Why.** A model that only reads "NOT performed" loops on the refused action or routes around a protection rule; naming the rule and the next step stops both.

## [0.194.0] - 2026-10-20 - New installs seed the 28 values five tiers' live nodes run, and ADR 0086 (an overdue lease's holder is asked to yield) is Accepted

### Changed - ADR 0086 is Accepted

- **What it does.** ADR 0086 (an overdue lease's holder is asked to yield, never taken from) moves from Proposed to Accepted, in its frontmatter and in the decisions index, with an acceptance section dated 2026-10-14. No code changes: the `gpu_on_overdue` key shipped in 0.186.0 and its behaviour is unchanged.
- **Why.** The operator accepted `request` on 2026-10-14 (ask the holder, never kill), and the record still read Proposed.

### Fixed - tier seeds now carry the 28 values five tiers' live nodes measurably run

- **What it does.** `setup/templates/profiles.json` gains or corrects 28 `config_seed` keys, so a NEW install of each tier seeds what the live nodes run. blackwell-3x16: the fp8mixed edit unet (was Q5_1), `inpaint_vae` builtin, the `wan22` entry in `videogen_families` (fp8_scaled native pair) and `videogen_upscale_model` 4x-UltraSharp. ampere-6: the animate, music, run-graph, video and voice scripts seeded blank, so a 6 GB card no longer advertises ComfyUI routes it cannot run. amd-gcn (17 keys): the audio.cpp binary moves to v0.9.1, the same five scripts go blank, and the z-image-turbo stable-diffusion.cpp image lane is seeded (engine, family, 8 steps, cfg 1, model, text encoder, VAE, Vulkan flags, `imagegen_timeout_sec` 1800). rockchip-rk3588: `compose_script` blank. ampere-8 `config_seed_ram_mid_high`: `gen_edit_script`. Paths use the seed's own `__OFFLOAD_HOME__` and `__EXE__` tokens. It changes only what a new install seeds and never touches an existing config. The tier pages under `docs/tiers/` are regenerated, and the 16 GB comfy-tier seed test now pins the fp8mixed edit unet for blackwell-3x16.
- **Not seeded.** The ampere-8 node also runs a wan22 video recipe, but the ampere-8 overlay keeps no video or music keys: the 2026-07-23 decision waits for an ampere-8 bake, `install-config-seed.test.ps1` pins it, and no ampere-8 Wan render is on record.
- **Why.** An audit of six live nodes' configs against their tiers' seeds found these keys measurably right on the node and missing or stale in the seed, so a fresh install would have started from a worse config than the node it was meant to reproduce.
- **Check.** `go run ./cmd/gentiers .` regenerated the pages; `go test` on the root package, `internal/tierseed` and `internal/config` passes, including `TestTierDocsAreCurrent` and the seed-rule tests; `setup/tests/install-config-seed.test.ps1` passes.

## [0.193.0] - 2026-10-19 - agent_delegate lint_only previews the acceptance lint and placement without running, and a multicard image call above the canvas cap is refused before the lease

### Changed - docs record LMCache PR #4253 as merged upstream, with the overlay still required

- **What it does.** `docs/systems/cache-server.md` (the overlay kit bullet, plus a new bullet on the upstream state) and `docs/OPERATOR-GUIDE.md` (the `--lmcache-overlay` flag row and the `SEAT_LMCACHE_PYTHONPATH` entry) described LMCache PR #4253, the fp8 / GDN store fix, as an open upstream change. They now state the upstream state read from the GitHub API on 2026-10-19: merged on 2026-09-28 (merge commit `1847435fa4`), contained in the v0.5.6rc5 tags (published 2026-10-12 and 2026-10-13, a release candidate, no final 0.5.6 seen), and issue #4247 closed. The overlay stays required until the seat runs an LMCache build that contains the merge, and the rules of the `--lmcache-overlay` flag are unchanged.
- **Why.** The pages gave no sign that the fix had merged, so a reader could not tell whether the overlay was still needed or when it would retire. The seat move that retires it is tracked elsewhere; this change only brings the description up to the upstream state and dates the reading.
- **Check.** One real run: `go run ./cmd/changelog check` prints `changelog: ok`, and the three changed places read back as written.

### Changed - gpu-lease doc carries its first measured working-set swing row

- **What it does.** The per-node working-set swing table in `docs/systems/gpu-lease.md` ("How the numbers get measured") had no
  row. It now has one, `measured 2026-10-11` on the reference 3-card box with n=30, taken from the 30 existing lane timelines of
  that day. The row is labelled lane-run swing, not interactive-use swing, and the headroom stays `8 (chosen)`.
- **Why.** The doc promised a measured swing per node before the 8 GiB headroom is revisited, and the data was already on disk
  but unentered. Entering it, with its limits stated, keeps the headroom claim honest: the figure bounds what a running lane
  moves, and the interactive figure is still owed.
- **Check.** Per run, over the lane's samples: system commit less the tree's private bytes moved a median of 9.9 GiB (0.5 to
  44.9) and available RAM plus the tree's working set 5.4 GiB (0.3 to 35.7), across 9,227 samples in 30 runs.

### Changed - the blackwell-3x16 and blackwell-2x16 tier pages state the card roles as they stand today

- **What it does.** The tier notes in `setup/templates/profiles.json`, and the pages `docs/tiers/blackwell-3x16.md` and `docs/tiers/blackwell-2x16.md` generated from them, now say that on the three-card composite the two-card tensor-parallel seat is the harness agent seat (agent-pool, 163,840 window) and the default, that the three-card pipeline-parallel seat is the most powerful configuration but is opencode-only and never named by the harness, and that the display card may carry leased work up to `gpu_display_cap_pct` of its VRAM (default 60, 0 closes it). They also say that the reference box enumerates the pair as devices 1 and 2 since the 2026-10-12 display move while the seed keeps its original indices. Prose only: no seeded device index, window, utilization or seat binding changed.
- **Why.** The pages predated the 2026-10-04 order and the display move, so a reader was told the pair was devices 0 and 2 with the display card at 1, and the display-card rule read as absolute.
- **Check.** The pages were regenerated with `go run ./cmd/gentiers .`, and `go test -run TestTierDocsAreCurrent .` passes on the result.

### Added - agent_delegate lint_only: the acceptance lint and the placement a contract would get, without running it

- **What it does.** `agent_delegate` takes an optional `lint_only: true`. The call runs intake and the acceptance lint
  as before, then returns before any dispatch, lease, queue or seat warm-up, and runs no model. Per subtask it answers
  `acceptance_lint` and a `placement` (`route`, `where`, `node`, `reason`) read from the same readings the joint deal
  takes before it deals (the lease file, the local seat's in-flight count, the run line and one health read per fleet
  node, bounded at 4 s), through the new `delegate.PlanPlacement`. Routes `spread` and `queue` answer `not_computed`.
  Documented in `docs/OPERATOR-GUIDE.md` and `docs/systems/mcp-server.md`; the tool description gains one sentence,
  pinned in `toolsprofile_keep.go`.
- **Why.** A caller learned that its acceptance was PARROT-PASSABLE or UNGROUNDED only by paying for the full run
  (register D-109).
- **Check.** The built binary as an MCP stdio server answered an `agent_delegate` call with `lint_only: true` and one
  trivial subtask in 68 ms on 2026-10-19: both lint findings, `placement.where: local` ("local idle"), no job id, no
  seat output.

### Changed - composite-tier citations in internal/delegate and internal/mcpserver point at ADR 0052

- **What it does.** 28 comments and strings that said "ADR 0039" for the composite tier now say "ADR 0052", including
  the `agent_run` and `offload_status` description sentences (and their pins in `toolsprofile_dropped_test.go`).
- **Why.** The composite tier was drafted as 0039 and recorded as 0052; 0039 is the held-card lease queue. The earlier
  sweep left these two packages to the tracks editing them. Mentions that mean the lease queue (ADR 0041, `gpuactivity`,
  `mediaremote`, `pipeline/mediamulticard.go`, the lease docs) are untouched. Comments and strings only.
- **Check.** `grep "ADR 0039" internal/delegate internal/mcpserver` finds nothing; the description-sentence gate tests pass.

### Fixed - doctor's tailnet-literal advice covers a node shared in from another tailnet

- **What it does.** The `lan ip:` row told the operator to use the MagicDNS name for every tailnet CGNAT literal. The row now
  names both ways out for a node shared in from another tailnet: list that tailnet's zone in `tailnet_suffixes` and use the
  node's full name, or keep the 100.x address. It stays informational and never changes doctor's exit code.
- **Why.** A node shared in from another tailnet has no dotless name on this one, and its full name sits under the other
  tailnet's zone, which the tailnet guard refuses unless `tailnet_suffixes` lists it. Following the old advice on a live
  delegator config dropped the node from every fleet lane (doctor printed the guard refusal and exited 1); the config was
  restored from its backup.
- **Check.** One real run: the patched binary's `doctor --config <live delegator config>` prints the new row and exits 0.

### Fixed - a multicard image call above the row's canvas cap is refused before any lease

- **What it does.** The typed generate_image door now holds a card-only (`imagegen_placement: multicard`) call to the image row's canvas cap before it takes the GPU lease. The Go copy of the row carries
  `MaxPixels` (1472700, read from the `maxPixels` field of `render/multicard-recipes.json`, pinned by `TestTheMulticardRowsAreTheRunnersRecipes`). A request whose width times height is above it, a default-size
  call (1328x1328, 1.76 MP) included, is answered with a refusal that names the cap, the asked size and the way out (a smaller width and height, or the single-card placement). The cap stays labelled chosen, not measured.
  `render/README.md` and `docs/systems/media-generation.md` now state the cap and both refusals (this one and the `COMFY-PLACEMENT-MISSING` one for a call with no placement).
- **Why.** The runner already refused such a canvas, but only after the harness had taken the lease on the pair and drained the seats, so a refused call still cost the pair a drain. The size is known at the door.
- **Check.** Build and vet of `internal/mediacap` and `internal/pipeline`, and the existing multicard and lane tests of both; the lane probe's own output showed the refusal text for a 1328x1328 request. No render was run. The batch door does not judge its jobs' sizes; the runner still refuses an over-cap job.

## [0.192.0] - 2026-10-19 - A reasoned local pin is bounded by the seat's line; offload_status publishes agent_slots

### Fixed - a reasoned local pin no longer queues without bound on the local seat

- **What it does.** A subtask pinned with `route=local` and a `pin_reason` joins the local seat's run-cap line only while the line is
  shallower than `fleet_max_queue_depth` (default twice `fleet_max_concurrent_jobs`, the bound every fleet node applies to its own
  backlog). Past that depth it defers at once as `capacity`, the pin still holds (it is never re-placed), and the reason names the
  line's depth, the fleet nodes with a free worker right now, and that a call without the pin (route spread) would use them.
  `offload_research` now refuses `privacy` and `locality` (its pages are public and travel inline), and the `operator` reason's text in
  both schemas says the human operator typed the placement, which a subagent, workflow stage or script never has.
- **`offload_status` publishes `fleet.agent_slots`** `{total, local, fleet}`: the local run cap plus every reachable agent node's
  `max_concurrent_jobs`, the most agent jobs the cluster runs at once, so a fan-out is cut to the cluster instead of to the task.
- **Why.** On 2026-10-19 a workflow's agents each pinned `offload_research` to the local seat under a claimed `operator` reason: 36 runs
  registered on one seat, 184 subtasks deferred `node_busy`, the median latency was the 600 s wall, and four fleet nodes had free
  workers. ADR 0078 made a reasonless pin a hint; a claimed reason still bought an unbounded line.
- **Check.** One real `offload_research` call pinned `route:"local", pin_reason:"operator"` against a full local line, read back:
  see the pull request. Details: ADR 0078, amendment 2026-10-19.

## [0.191.3] - 2026-10-19 - node-swap refuses a node it cannot see instead of rolling back against it, and the rendered WSL2 seat env exports HOUSE_KV_SPLIT=1

### Fixed - node-swap refuses a node it cannot see instead of rolling back against it

- **What.** On Windows, `node-swap` now lists a process on the target's file name even when CIM will not give it the process's path or command line, and marks it unreadable. A swap that stops and restarts the node refuses on such a process with the step `process-access`. It names the pid and says to run the swap at the node's elevation. The refusal comes before anything is hashed or touched, on a dry run as on a real one. A restart that starts the node at another elevation than the swap now fails verification with a message that names the unreadable pid, instead of "no process appeared".
- **Why.** A fleet node restarted over `ssh localhost` runs elevated in session 0. An unelevated swap read its CIM row with an empty path and command line and filtered it out. On 2026-10-19 such a swap found no node to stop. It installed the new exe, and the restart script replaced the node. The swap then saw no new node within 2 minutes and rolled back against the running node; the rollback failed only because that node locked the new exe. The dry run before it had reported OK.
- **Check.** On that node, the branch's `node-swap --dry-run` refused unelevated (`process-access`, naming the running node's pid) and passed through `ssh localhost` (verify-hash, wait-idle, dry-run ok). One test (`TestSwapRefusesANodeItCannotRead`) drives `Run` with an unreadable holder and checks that nothing was touched.
- **Docs.** `docs/systems/node-swap.md` covers the step and the pitfall.

### Changed - the rendered WSL2 seat env exports HOUSE_KV_SPLIT=1

- **What.** `setup/templates/vllm-seat/windows-wsl/seat.env` now carries `export HOUSE_KV_SPLIT=1`, with a comment that says what it does. The vLLM patches README and the operator guide no longer tell the operator to add the line after rendering.
- **Why.** A WSL2 seat on vLLM 0.29 or later dies at start unless the house KV-split patch is applied and this variable is set. The renderer did not write it, so every rendered seat needed a hand edit. An unpatched vLLM never reads the variable, so the line is safe on any version.
- **Check.** The rendered template contains the export, and `seat_fg.sh` sources the env file, so the variable reaches vLLM the same way `VLLM_EXECUTE_MODEL_TIMEOUT_SECONDS` does. The production pair seat has run with the same line since 2026-10-18.

## [0.191.2] - 2026-10-19 - the vLLM seat kit builds a WSL2 seat on vLLM 0.32 and LMCache 0.5.6, and the nightly offload-dream run finishes

### Added - the vLLM seat kit can build a WSL2 seat on vLLM 0.32 and LMCache 0.5.6

- **What.**
  - `setup/templates/vllm-seat/vllm-patches/house-kv-split-0.32.0.patch` (HOUSE-KV-SPLIT) and its README. With `HOUSE_KV_SPLIT=1` in the seat env, a worker's KV cache is allocated as one buffer per region instead of one buffer for the whole pool. The byte layout is the same.
  - `setup/templates/vllm-seat/lmcache-patches/wsl2-0.5.6/07-house-wsl2-no-direct-copy.diff`. It turns off LMCache 0.5.6's copy-engine direct path.
  - `repatch-lmcache-overlay.sh` now checks the marker of each patch file present in the set, and reads the new `no_direct_copy` marker from the source. It used to demand the 01 and 02 markers whatever the set held, so it could not build any set without them.
  - The operator guide and the LMCache patches README document both fixes. `.gitattributes` keeps the new patch directory LF.
- **Why.** A seat on WSL2 fails twice on these versions.
  - **At start.** vLLM 0.29 and later back the KV cache with one allocation, which WSL2 will not export through CUDA IPC (`dxgkio_make_resident: Ioctl failed: -12`).
  - **On LMCache's first store.** The direct path (LMCache PR #5308) hands the pinned pool's host addresses to the copy engine. On WSL2, `cudaDevAttrCanUseHostPointerForRegisteredMem` is 0 on every device, and `batch_memcpy_supported()` checks that attribute only on ROCm. The MP server then logs an illegal memory access, and requests hang.
  - **In the old builder.** The shipped 0.5.5 set does not build on 0.5.6: patch 01 (#4253) is upstream there under other names, and it neither applies nor reverse-applies. So the kit could not reproduce a working 0.5.6 seat.
- **Check.**
  - **A tp2 seat** (two 16 GB Blackwell cards, fp8 KV, a GDN hybrid, a 163,840-token window) ran on vLLM 0.32.0 and LMCache 0.5.6 with both fixes:
    - 16 KV regions of 217.4 MiB per rank, 0 new `make_resident` failures, a 209,597-token pool;
    - a 23,764-token needle served from the L2 store after eviction (23,520 external hit tokens, TTFT 3.0 s against 24.1 s cold) and again after a full restart;
    - tool calls and structured output ok;
    - 0 illegal-access lines, then the same in production behind the harness after one `agent_run`.
  - **The builder, run on the node:**
    - it built the `wsl2-0.5.6` set against LMCache 0.5.6 (1 patch applied, `no_direct_copy=True`, rc 0);
    - it still builds the default set against LMCache 0.5.5 as before (5 patches applied, the same patch hashes, smoke 99 passed and 0 failed, rc 0).
  - **The vLLM patch** applies forward to a pristine 0.32.0 `vllm/v1/worker/utils.py` and gives the patched file's exact hash.
- **Not in this change.**
  - The renderer does not emit `HOUSE_KV_SPLIT=1` yet; the operator adds it after rendering.
  - Patches 02 and 04 (pipeline-parallel layouts) are not ported to 0.5.6, so a pipeline seat stays on a 0.5.5 overlay.

### Fixed - the nightly offload-dream run finishes, and adopts into the files the harness reads

- **What.** `scripts/offload-dream.ps1` changes in two ways.
  - It asks the binary where each file lives (`local-offload data status --json`) instead of assuming `%USERPROFILE%\.local-offload`. Its ledger check and the five files it adopts into (`thresholds_path`, `tier_overrides_path`, `router_weights_path`, `confhead_path`, `confhead_thresholds_path`) are the paths the harness reads. Staging, backups, the proposal and the log go under the configured `home`.
  - Passing `-DataDir` still points the whole run at that folder. A binary without `data status` keeps the old default.
  - Every call to the binary now goes through one helper that keeps stderr apart from stdout.
- **Why.** Two defects.
  - Under Windows PowerShell 5.1 with `$ErrorActionPreference = 'Stop'`, a stderr line from a native command ends the script. `confhead-calibrate` exits 0 but prints a note on stderr about the test traffic it left out, so on a node with such traffic the run stopped there with exit 1, before `optimize` and before any adoption. The fleet node that showed it had stopped at that step on every run in its log.
  - A node that moves its data off C: by setting `home` would have had its nightly thresholds written beside a tree the harness no longer reads.
- **Check.** The script ran under Windows PowerShell 5.1 on a fleet node whose home had just moved to its data drive, with `-DryRun`.
  - It exited 0 and ran every stage through `confhead-eval`, whose JSON parsed.
  - It named the data-drive paths as the adoption targets.
  - It wrote its staging, backup, proposal and log there and nothing under the old home.

## [0.191.1] - 2026-10-18 - a vLLM seat on WSL2 no longer holds its checkpoint twice in host RAM, and ADR 0090's pending rows are settled

### Changed - ADR 0090's pending rows are settled

- **What.** Section 4 of ADR 0090 (capability review 2026-10) gives a terminal state to every row that was waiting on a gate verdict:
  - the Headroom offload rungs are PARKED (merged, default off, not measured);
  - the CLM paper is DECLINED as measured with no need (the ladder fired on 0 of 5 recorded agent runs in the registered window; no run evicted anything);
  - GitSwarm is DECLINED as replicated and measured with no gain;
  - the acceptance-hygiene ADR is DECLINED.

  ADR numbers 0092 and 0093 are not used.
- **Why.** Since 2026-10-18 a model or loop is measured only on the operator's order, with one run, so no further pre-registered A/B will settle these rows.

### Fixed - a vLLM seat on WSL2 no longer holds its checkpoint twice in host RAM

- **What.** `setup/templates/vllm-seat/seat_fg.sh` starts a watcher before it becomes `vllm serve`. On WSL2 only, once the engine answers `/health` the watcher drops the guest's clean page cache, and it drops it again whenever the cache passes `SEAT_CACHE_CAP_GIB` (default 4), until the engine exits. Each drop logs `seat_fg: page cache dropped (A GiB -> B GiB; cap C GiB)` to the seat log. Native Linux is unchanged: there, page cache is reclaimable kernel memory, not host commit.
- **Why.** vLLM reads the checkpoint through the page cache and keeps none of it mapped once the weights are on the cards. On WSL2 that cache is VM memory, which the host cannot take back until the guest frees it. On a 128 GiB WSL2 box, loading the INT4 27B pair seat moved host commit from 76.6 to 150 of 171.7 GiB, and 23 GiB of that was page cache. Over hours of long-context work the same growth reached 168 GiB, which pushes the host into its page file (register B-56). The operator rule is no swapping and no out-of-memory.
- **Operator step on a WSL2 seat box.** In `.wslconfig`, set `swap=0`, and set `autoMemoryReclaim=dropCache` instead of `gradual`. Both take effect at the next `wsl --shutdown`; `swapoff -a` inside the distro turns swap off at once.
- **Check.** The seat was installed on the 3-card reference box and loaded through its normal launcher with one request. The seat log printed `page cache dropped (23 GiB -> 1 GiB; cap 4 GiB)`. Host commit went from 149.7 to 128.0 GiB, the VM from 68.7 to 46.8 GiB, and available physical memory from 31 to 51 GiB, with the seat still answering.

## [0.191.0] - 2026-10-18 - the hosted NVIDIA client dials public addresses only, nodes publish their GPU power-profile state, a node shared in by another account takes public-data work only, the vLLM seat logs its host memory and doctor flags an undeclared one, and the doc-wording tests are gone

### Removed - tests that only checked the wording of docs

- **What.** Ten root-package test files that asserted sentences, links and claims in Markdown docs are deleted: `delegation_contract_docs`, `dispatch_log_contract_docs`, `docs_lint`, `fleet_door_claims_lint`, `fleet_ingress_docs`, `gpu_ram_docs`, `review_policy_docs`, `run_template_docs`, `self_improvement_docs` and `upgrade_note` (`_test.go`). They tested no behaviour, and every doc edit had to keep them green. `AGENTS.md` no longer points at the docs lint.
- **Why.** The testing rule of 2026-10-18 (`AGENTS.md`): CI as it stands is the gate, and tests that pin wording are cut. Tests of behaviour, the identity and leak scans, the changelog fragment check, the tier layer-set and generated-tier-docs checks and `fence_cost_docs` (its helper is shared) stay.
- **Check.** `go vet .` compiles the root package without the deleted files.

### Added - /fleet/health and offload_status say whether a node's boot-time GPU power profile took

- **What it does.** `/fleet/health` carries a `power_profile` block, `{"state": "ok"|"skipped"|"unknown", "reason", "limit_w", "clock_lock", "applied_at"}`, built from the same state record and the same judgement as the `power:` row of `doctor` (`LOCAL_OFFLOAD_POWER_PROFILE_STATE` names the file). `ok` is a whole apply since this boot began (the row's OK); `skipped` is any apply that did not take or took in part (the row's WARN: no apply this boot, a dry run, a refusal, a failure, an empty plan, the tool's crash guard, a step or lever short of the plan, a driver that differs from the profile's stamp); `unknown` is everything the row calls SKIP or UNKNOWN. `reason` is one short token, never free text and never a path (`no_state`, `unreadable`, `bad_state`, `bad_schema`, `not_due`, `boot_unknown`, `stale_boot`, `partial`, `dry_run`, `unrecognised`, or the applier's own outcome word such as `cookie_blocked`). `limit_w` is the driver's power limit of each card as the record read it at the end of the run, and `clock_lock` is true when a clock-lock step returned success; both are published only when the state is `ok`, so a skipped run never advertises a limit it did not set. `applied_at` is the record's own time in UTC.
- **In offload_status.** Each node of the `fleet` block gains `power_profile` (`ok`, `skipped` or `unknown`), and `power_profile_reason` when the reason is news (not for an `ok` node and not for `no_state`). The brief form carries both, because it carries the whole fleet block. A node one release behind publishes no `power_profile`, and the key is then absent: unknown, never "no profile". Placement never reads it.
- **A node with nothing to report is not in trouble.** A node with no state file, or no setting (a Linux node with no boot-apply tool), publishes `{"state": "unknown", "reason": "no_state"}`: an answer, not an error, and `offload_status` shows a bare `unknown` for it. One stricter case than the row: a whole apply on a platform that cannot say when the box booted is `unknown` (`boot_unknown`), because the field claims "applied at boot".
- **One judgement, two surfaces.** The doctor row was split into `readPowerProfile` (the judgement) and the printer, so the row and the health field cannot disagree; the row's wording is unchanged. The record's `boards` and `steps` are read leniently, so an unexpected shape costs `limit_w` or `clock_lock` and never the verdict. The clock lock is matched in the step's description and only that fact leaves the node; the record's host, path, detail and step text are never published. `fleet-serve` reads the file at most once every 30 seconds. Like the row, it reads the record, not the card: a limit changed after the apply still reads `ok`.
- **Why.** A card that ran at stock limits for days looked fine on every row of the fleet view (register A-103). The doctor row only helped on the box you ran it on; this puts the answer on every node's roster entry.
- **Check.** `go build ./...` and `go vet` on the root package and `internal/fleetnode`, `internal/delegate`, `internal/mcpserver`; no tests added. One real run with the built binary: `doctor` against 20 sample state records, old binary and new, printed the same `power:` row for every one; `fleet-serve` on loopback with an isolated config and state published the expected `power_profile` for nine scenarios (a whole apply, with `limit_w` and `clock_lock`; a stale boot; the crash guard; a partial apply; an unknown schema; hostile outcome text; odd `boards`/`steps` shapes; a missing file; no setting); the MCP server's `offload_status`, brief and fleet, showed `skipped` with `cookie_blocked`, `ok` and a bare `unknown` for three such nodes.
- **Docs.** `docs/FLEET-NODE.md` (the field table), `docs/systems/fleet-node.md` and the power paragraph of `docs/OPERATOR-GUIDE.md`.

### Added - `delegate_remote_classes`: a node shared in by another account takes public-data work only (register SF-40)

- **What it does.** A new config key beside `delegate_remotes`, `delegate_remote_classes: {"<base url>": "public"}`, declares a fleet node whose admin is not the operator. A node that is not listed is `private-ok`, so a config without the key places exactly as before. A node declared `public` is placed public-data work only: the page digests `offload_research` builds from the public web (the guarded fetcher refuses tailnet, private and loopback hosts) and the `fleet-smoke` probe. Every other contract is ineligible for it, decided in `eligibilityVerdict` right after the agent-lane check, so the idle-local shortcut, the joint auto/remote deal, the spread deal, the capacity wait, a retry and a replacement all skip the node whatever its capacity. The discriminator is the contract's door, which each surface stamps itself after intake (`agent_delegate` writes its own name, research stamps after prepare, the CLI verbs stamp theirs), so a caller cannot claim `offload_research` from a subtask. It is deliberately stricter than "carries a context document": by placement time `context_paths` are already inlined as documents, and a goal typed into `agent_delegate` can quote a plan as easily as a file does, so a goal-only contract is private too. A contract pinned `route: "remote"` with only public nodes eligible defers (`defer_class: contract`, not a broken stack) with a reason that names `delegate_remote_classes` and the two ways out, and the exclusion is recorded in the placement reason (`<node>: private (...)`, a new word in the per-node verdict line; the spread deal appends `excluded by data class`). `route: "queue"` is checked at `fleet_queue_holder`, the one node it submits to.
- **Why.** One fleet node is shared into the operator's tailnet by a third-party account and its admin is not the operator. `agent_delegate` could place any contract there, and on 2026-09-27 a private handoff plan went out as a context document.
- **Fail-closed.** A value other than `public` or `private-ok`, and a key that is not a `delegate_remotes` entry (case and a trailing slash ignored), fail the config load by name, because a typo would otherwise read as `private-ok`. On a hand-built config a listed node with any value but `private-ok` reads as public. The class is stamped from the config in `probeRemotes`, never from the node's health. Read once per process, like `delegate_remotes`: restart the MCP server after changing it.
- **Not covered.** The single-shot lanes (text, vision, speech, media, accelerator) choose a node through `rosterprobe`, not this gate, and `route: "queue"` cannot see which node later claims a job.
- **Check.** `go build ./...` and `go vet` on `internal/config`, `internal/delegate` and `internal/mcpserver`. One real run of the built binary against a local stand-in node declared `public`: a private `delegate` contract pinned `--route remote --pin-reason operator` deferred (`contract`) with the rule's sentence and `<node>: private (...)` in the placement, and the stand-in saw no request; the unpinned hint fell back to the local seat with the same exclusion in its placement. A misspelled value and a key outside `delegate_remotes` each failed validation naming the key. Docs: `docs/OPERATOR-GUIDE.md` (config table and the `agent_delegate` section), `docs/systems/fleet-node.md`, `docs/systems/mcp-server.md`; `config.example.json` regenerated.

### Added - the vLLM seat's host RAM is logged at every start, and doctor warns while it is undeclared

- **What.** `setup/templates/vllm-seat/seat_fg.sh` logs one line to the seat log just before it becomes `vllm serve`, for example `seat_fg: host rss 9.3 GiB (L1 8 GiB) ...`: the resident set of the LMCache MP server unit and its children (the pinned L1 staging buffer plus about 1.1 GiB of process overhead). `doctor` gets a `seat ram:` row on a box that runs a vLLM seat (`vllm_seats`, or the tier table's vLLM seat as `agent_model`): WARN while `agent_seat_host_ram_gib` is unset, OK once it is declared. It is informational and never changes doctor's exit code. The `agent_seat_host_ram_gib` docs (`docs/systems/gpu-lease.md`, `docs/systems/cache-server.md`, the config field comment) say where the figure comes from.
- **Why.** The seat's host memory (the VM that carries it, the staging buffer, the process overhead) was declared nowhere and bounded by nothing; on 2026-10-18 a host's commit reached 168 of 171.7 GiB. The warm-back of a seat a lease unloaded is admitted against `agent_seat_host_ram_gib`, and unset it is a chosen 21 GiB, not a measurement.
- **Limits.** The launcher `exec`s into `vllm serve`, so nothing of it survives to log later, and the line carries the MP server's share only (the engine is not loaded yet); no loop or sampler was added. The doctor row compares nothing with a live reading: the seat's memory is read inside its own distro and the harness holds no cheap read of it.
- **Check.** The new block ran against a stand-in process in a Linux distro and summed the stand-in's resident set (0.3 GiB for 317,652 KiB); `local-offload doctor` printed the WARN row for a config with `vllm_seats` and no `agent_seat_host_ram_gib`, the OK row with `agent_seat_host_ram_gib: 24`, and no row without a vLLM seat; `go build ./...` and `go vet .` pass.

### Fixed - the hosted NVIDIA client dials public addresses only

- **What.** `nimclient.New` routes a hosted NVIDIA base (`IsHostedNVIDIA`: https, `api.nvidia.com` or a subdomain) through `netguard.PublicTransport`, so the connection that carries the NVIDIA key can only reach a public address. Self-hosted NIM bases on the LAN or the tailnet keep the plain transport and stay keyless.
- **Why.** Register SF-01: the key was bound to the hosted host name in 0.156.1, but a DNS answer for that name that pointed at a private, loopback or tailnet address would still have been dialled with the key attached. The base allowlist (S-30) narrowed this; the dial gate closes it.

## [0.190.0] - 2026-10-18 - offload_run_template runs ComfyUI workflow templates behind a licence gate, an optional evidence-preserving reducer for build and test results, the vLLM seat picks the faster store path and one rendered stub serves every Windows seat, and changes carry no new tests

### Added - two contract pages (the `delegate --contract` file, the dispatch log), stale vLLM debt reasons corrected, a labelled-agent-seat gate (A-20), and an `install render` refusal of a llama.cpp 9B agent seat on ampere-16 (A-40)

- **`docs/systems/delegate-contract-file.md` (register D-70).** The file `local-offload delegate --contract` reads is a bare subtask
  object or an array of at most 8, never the `{"subtasks": [...]}` wrapper the MCP tool takes. A wrapper file does not fail to parse:
  unknown fields are ignored, so it becomes one subtask with a blank goal and intake refuses it with `goal is required`. The page also
  maps the tool's call-level arguments to the flags that carry them, and records the seat id rule behind three failed launches:
  `agent_model` must equal an id the endpoint's `/v1/models` publishes, or every subtask comes back deferred as `infrastructure`
  (counted in `lost_to_stack`) while the contract itself is fine. Held by `TestDelegateContractFilePageNamesWhatTheCLIReads` (the page
  names every field of the `--contract` help's own `(fields: ...)` list) and `TestContractFileWrapperIsNotUnwrapped` (the wrapper trap,
  against the real parser and intake).
- **`docs/systems/dispatch-log.md` (register D-74).** `state/dispatch-log.jsonl` is written by two harnesses (the Claude Code hooks, which
  also rotate it at 1 MiB, and the opencode plugin, append-only, tagged `"harness":"opencode"`) and read by one scoreboard that divides by
  `readonly_spawn` and checks a `session` heartbeat. The page lists every event with its writer, fields and reader, and names the rows a
  rewrite of either writer must keep emitting. `TestDispatchLogContractPageNamesEveryEventTheWritersEmit` reads the `DispatchEvent`
  type in `integrations/opencode/src/instrument.ts` and fails when the page leaves out an event the plugin may write;
  `TestDispatchLogContractIsLinkedFromItsWriters` holds the links from `instrument.ts`, `opencode-integration.md` and `docs/README.md`.
- **vLLM coverage debt reasons (register A-07, coverage).** Four `vllmSeatDebt` reasons were stale: ampere-8 said "9B w4a16 arm never
  run" and blackwell-8 "vLLM arm never measured" although both profiles record that vLLM is not a path for the 9B on 8 GB (the W4A16
  weights alone are 7.51 to 9.35 GiB), and amd-gcn and cpu said nothing of the 2026-09-20 vLLM-CPU run (Qwen3.5-4B bf16 only, 3.1 tok/s
  against 9.27 for llama.cpp CPU, not seated). `TestVLLMDebtReasonsAreCurrent` ties each debt reason to its own profile's recorded
  measurement, so the next one cannot go stale unnoticed.
- **Agent seats are measured or labelled (register A-20).** `TestEveryTierIsMeasuredOrLabelledProjectedForItsAgentSeat` makes every tier
  either MEASURED or carry a PROJECTED label naming the seat it seeds; a projected seed that ends up with a seat a measurement failed
  (the 26B that scored 1/8 on the A2, offload-e4b that failed on amd-gcn) must record the failure. It does not decide whether the A2
  finding generalises to other card sizes; it makes the open question countable. No profile changed.
- **`install render` refuses a llama.cpp 9B agent seat on ampere-16 (register A-40).** The prohibition was measured (the 9B under llama.cpp
  fails on the A2, under vLLM it only ties the 4B) and recorded, never gated. `renderGate` now reads the rendered models and refuses a
  `mimo-9b-agent` or `qwen3.5-9b-agent` entry for that tier, naming the tier and the register row. The 8 GB tiers, which ship those entries, are
  untouched (`TestRenderGateLeavesTheNineBSeatsOfTheEightGBTiersAlone`).
- **Red-first.** Every test above was committed before its page or fix and failed on the unmodified tree: the two page tests on the missing
  pages, `TestVLLMDebtReasonsAreCurrent` on the four stale reasons, `TestAgentSeatLabelCheckRefusesEachDefect` on the empty gate, and
  `TestRenderGateRefusesALlamaCppNineBAgentSeatOnAmpere16` on "the write gate passed it".
- **Not done here.** A-86 (an `audit-yaml` check that residents and singles sit on the intended cards) needs a machine-readable layout
  declaration and the layout it would protect moved when the display card changed, so it waits for that declaration.

### Changed - docs: new changes carry no new tests, CI as it stands is the gate and one real run is the check

- **The rule.** `AGENTS.md` now carries a "Testing rule" (the maintainer's order of 2026-10-18): a change adds no new tests; CI is the gate and no full-suite, multi-host or remote-gate run is made beyond it; the check is one real run of the change, read back; review is at most one pass and only for credentials/security or data loss; a test a change turns red is fixed or deleted, never replaced.
- **What was rewritten to match.** `CONTRIBUTING.md` (build and check, the testing rule, the pull request list), `CLAUDE.md`, `README.md`, `docs/README.md`, `docs/STYLE.md`, the system and flow doc templates, `changes/README.md`, the invariant I5 row of `docs/systems/security.md`, the OpenSpec config, schema and page, and one line of the operator guide. The five older implementation plans carry a one-line pointer saying their test, red-first, mutation and review steps are superseded.
- **What did not change.** No test, script or CI file is edited or deleted here; the maintainer prunes them in a separate change. The leak gate, the identity scrub and the other security guards stay.

### Added - an optional evidence-preserving reducer for the agent's build and test results, default off

- **What it does.** With the config key `agent_evidence_reducer` set, a `run` or `run_shell` result that is a build or test
  invocation and is 4 KiB to 64 KiB is no longer carried into the transcript whole. The cascade seat the agent's own offload
  tools already use (`Build`'s `Offload`: the recordless offload, one named tier, the `extract` task, no new client and no ledger
  row) is asked for at most five claims, each with a `claim` and a one-line `quote`. Every quote is looked up in the output with
  `quotecheck.Locate`, the lookup behind the research digest's quote check (the output's bytes first, then case, spacing, quote
  marks and punctuation set aside, publishing the output's own span), and the transcript keeps a receipt in place of the log: an opening
  line `[evidence-receipt: original N chars, M lines; ...]`, the tool's `exit=` line copied by code and never written by the
  seat, each claim over its verified quote, and a closing line saying the rest is not kept and the command can be run again.
  Unset, the key is off and the loop is byte-identical to before.
- **When the raw output is kept, unchanged.** Output under 4 KiB or over 64 KiB; output with no `exit=` first line; any tool that
  is not `run` or `run_shell`; a command the producer gate (the log-collapse rung's `logsCallIsBuild`) does not read as a build or
  test invocation, which is what bypasses reads and searches (`read_file`, `search_files`, `list_dir`, `grep`, `cat`, `sed -n`,
  `git grep`, a chained command, a fetch, an unknown tool: the agent needs their exact text); a call that did not run or
  errored; a seat error, timeout (60 s), deferral or panic; an answer that is not the requested JSON, has no claims or more than
  eight; a claim with no text or a quote that is not one line of 8 to 200 characters; any quote that is not in the output as one contiguous span, an elided `first ... last` quote
  included (all or nothing); output the corpus vet's table says looks like a credential or an email address, checked before the seat is shown
  anything, and the receipt checked again; a receipt that is not strictly shorter than the output. A claim is collapsed to one line,
  so a seat cannot forge a receipt line (an exit status, a disclosure line) from inside a claim.
- **The credential check reuses an existing detector and moves it.** The only credential-shape detector in the tree was the
  compaction-eval corpus vet's class table (`piiPatterns`, `PIIClasses` in `internal/compeval`), and `internal/compeval` imports
  `internal/agent`, so the agent could not call it. The table and the question "which classes match this text" move, unchanged
  (same six classes, order and expressions), to the new `internal/piishape` (`Classes`, `Matching`). `compeval` derives its
  `piiPatterns` from it, `PIIClasses` delegates to `Matching`, and `VetPII`, the redactor and the residual gate behave as before
  (a test pins that the two tables are one). The reducer calls `piishape.Matching`. It flags an email address, so a log that prints
  one stays raw. The table was widened in the same change (next bullet).
- **The credential table is widened for both users.** `api-key-assignment` now also matches a key word that follows an underscore
  or other non-word character and may be followed by `access`/`key`/`secret`/`token` parts (`GITHUB_TOKEN=`, `OPENAI_API_KEY=`,
  `DB_PASSWORD=`, `AWS_SECRET_ACCESS_KEY=`), with the 12-character value floor kept; two classes are appended after the existing six,
  `provider-key` (bare `sk-` followed by 20 or more letters and digits, or `sk-proj-`, `sk-ant-`, `sk-svcacct-`, `sk-admin-` followed by 20
  or more key characters) and `jwt` (three base64url segments of 8 or more characters, the first starting `eyJ`). The corpus vet and the
  harvest redactor, which derive from the table, refuse and redact the same shapes. Settings and prose stay clean (`MAX_TOKENS=4096`,
  `token_count: 12`, "the password field is required", a git sha, a UUID, a base64 blob, `sk-learn`, `/tmp/secret_dir`), pinned by
  tests. Still not covered (stated, not detected): a camel-case name (`githubToken=`), a key name the table does not hold
  (`dbpassword=`, `sid=`), a command-line flag with a space (`--token value`, `curl -u user:pass`),
  URL userinfo (`scheme://user:pass@host`), an authorization scheme other than Bearer and Basic, a provider prefix the table does not list
  (`sk_live_`, `xoxb-`), an unsigned JWT, a high-entropy string with no key name or known prefix, a value that starts with `/`,
  `.`, `[` or `]`, the part of a value after a backslash inside it, an unquoted value after `:=`, `==` or `=>`, a value passed apart
  from its name (`fmt.Sprintf("password=%s", v)`), and a terminal control sequence other than escape, bracket, parameters and a letter.
- **A credential value is matched and redacted whole.** The first widening stopped a value at the first `.`, `/`, `+`, `=`, `@`, `~`
  or `:`, so the compaction-eval harvest redactor, which derives from the table, wrote `access_token=<jwt>` out as the key name, a
  placeholder and the JWT's payload and signature, `AWS_SECRET_ACCESS_KEY=<key>` with everything after its first slash, and a
  bearer value with a `/` or `+` the same way, and the residual gate saw nothing because a tail matches no class. The value of
  `api-key-assignment` and `bearer-token` now runs to the next whitespace, quote, comma, semicolon, ampersand or backslash (12 or more
  characters, 16 for a bearer token; a bearer value starts with a letter, digit, underscore or hyphen, an assignment value with any
  character except those and `/`, `.`, `[`, `]`, after an optional quote), so a value such as `ab/cdef...` or `!cdef...` after a secret name is recognised too, and
  the redactor replaces the value as a whole (the key name stays, as for every class). A value that starts with `/` or `.` (a path
  after `token_store=`) is not read as a secret; a relative path that starts with a letter is, the safe direction. Across the 3,959
  tracked files (3,736 read as text) the two classes matched 16 times before and 44 now (assignment 9 to 37, bearer 7 to 7); every new match is source
  code or a test that assigns something to a name containing `token` (a local variable or struct field whose name contains `token`) or a redaction
  test's own fixture, none in a corpus or testdata file.
- **The redactor substitutes the email class last.** An address-shaped piece inside an assignment's value used to be
  replaced first, leaving a placeholder where the credential class needs the value to start. The vet table keeps its order.
- **The residual gate is stronger than the vet.** `Redactor.Residual` (the trace harvest's and the trace corpus verb's exit gate)
  reports the vet classes that still match and `value-tail` when 12 or more consecutive bytes of any credential value the redactor
  replaced (assignment, bearer, bare provider keys, JWT; not email or the PEM header, whose pieces are ordinary text elsewhere)
  are still in the text. With whole-value replacement a tail cannot occur, which a 400-value property test pins, so the check is a
  second wall against the next widening that stops short; a mention of a replaced secret elsewhere in the same corpus, or ordinary code
  that reuses a piece of a value taken from `token = f(x)`, leaves that one trace out of the harvest with a note (the others are
  kept), the safe direction. In the trace corpus verb the same gate refuses the harvest.
- **A value stops at a backslash.** Tool-call arguments are JSON text. A value class that ran through a backslash took the closing
  quote or the next escaped command with it, so a redacted `Args` was no longer valid JSON and replay code that parses it lost the
  call (`export TOKEN=<value>\nls` became `export <placeholder> -la`). The value now ends at a backslash and may start with a
  quote or a JSON-escaped quote (with any number of backslashes in front of it, see below), and tests pin that the redacted `Args`
  stay valid JSON with the text after the value intact. A
  secret that itself holds a backslash leaves the part after it, which the limits above name.
- **A value may start with a symbol.** `DB_PASSWORD=!...`, `password: $...`, `secret=#...` and `token=(...` were neither
  redacted nor seen by the vet, because the value had to start with a letter, digit, underscore or hyphen. A value that starts with
  `/`, `.`, `[` or `]` is still not read as a secret (a path after a key name, an earlier placeholder).
- **A key word right after a JSON escape is recognised.** Tool-call arguments and JSON tool output are JSON text, so the newline
  before a line that starts with `PASSWORD=` is a backslash and an `n`, and a word boundary does not see one between them. Every
  class that starts with a word (assignment, bearer, AWS, GitHub, provider key, JWT) therefore missed a secret at the start of a
  line in an escaped write_file body or a heredoc: the redactor returned the Args unchanged, the residual gate and the corpus vet
  reported nothing, and the harvest wrote the password to the corpus. The classes now also accept a JSON escape (backslash and
  `n`, `r`, `t`, `b` or `f`, or backslash-`u` and four hex digits) in front of the first word, and the email class may start with one,
  so `\nbob@example.com` no longer loses the `n`. The match then carries the escape, so the redactor keeps the escape and puts the
  placeholder after it: redacted Args stay valid JSON and the lines around the secret are intact (pinned by a test that decodes
  the redacted Args and compares the lines, and by a harvest test through `HarvestTraces` and `WriteCorpus`). The reducer, which
  sees real newlines in tool output, was not affected on that path, but tool output that is itself JSON (a `go test -json` line)
  was.
- **More credential names.** The assignment class also reads `PGPASSWORD`, `passphrase`, `credential(s)`, `private_key`,
  `access_key`, `auth_key`, a plural `passwords`, a trailing digit (`password1=`) and a trailing `_value` or `_id` part
  (`client_secret_value=`, `secret_id=`), and `_PASS=` and `_PWD=` (`DB_PASS=`, `MYSQL_PWD=`). The short words need the
  underscore and an equals sign: bare `pass` would read the test runner's `PASS: TestName` line and bare `pwd` the shell's
  `PWD=`, and a test pins that those stay quiet along with `private_key_path=/etc/...` and `token_count:`.
- **Basic authorization, registry auth and cookies.** Three classes are appended after the existing eight: `basic-auth` (an
  `Authorization` or `Proxy-Authorization` header, or a quoted JSON key, with the Basic scheme and a value of 12 or more
  characters), `auth-field` (a quoted `auth` key with an encoded quoted value, the shape of a container registry's config) and
  `cookie-header` (a `Cookie` or `Set-Cookie` header holding a value of 12 or more characters, replaced as one whole span: see
  the cookie bullet below). Each match ends before
  the closing quote or backslash, so redacted text keeps its quoting and Args stay valid JSON, and the residual gate remembers the
  credential part of each (the encoded value, or every cookie value in the span, a quoted one whole, but not its attributes, not the header name). The words alone ("Basic
  authentication", `Authorization: Basic` with no value, `{"auth":"none"}`, `Cookie: theme=dark`) stay quiet. The corpus vet and
  the harvest redactor derive from the table, so both refuse and redact the new shapes.
- **A terminal control sequence before a key word is recognised.** The `m` that ends a color code is a word character, so a
  word boundary never saw the key word after it: `ESC[32mpassword=<value>`, `ESC[1;34mpassword: <value>`, a colored
  `Bearer`, `ghp_`, `sk-`, `Authorization: Basic` or `Cookie` line passed the table, the redactor, the residual gate and the
  reducer's seat check, and colored build and test output (`FORCE_COLOR`, `--color=always`, CI) is what the reducer targets. Every
  class that accepted a JSON escape before its first word now also accepts a control sequence (the escape byte, a bracket,
  parameters and a letter, as the raw byte or as its JSON escape `\u001b`); the assignment class also accepts one between the name
  and the colon (`DB_PASSWORD ESC[0m: <value>`) and after the operator, and a flag letter in front of a key word (`-Dpassword=`,
  `-DTOKEN=`). A value ends at the escape byte, so the color reset behind it is not eaten, and the redactor keeps the sequence
  that opens a match, like a JSON escape (`piishape.Lead` names it; the redactor no longer carries its own copy of the escape
  pattern). The cookie class does not stop at the escape byte: a color reset inside a cookie line goes with the rest of the line, as
  that class already takes the whole line. Quiet cases are pinned: `PASS: TestName`, `token_count=1234`, `MAX_TOKENS=4096` and the word `password` in colored prose.
- **A quote may carry any number of backslashes.** The assignment, Basic and registry auth classes took one optional
  backslash before a quote. JSON text inside a double-quoted shell string doubles them again, so `bash -c "export API_KEY=\"<value>\""`
  as Args has three backslashes in front of the value's quote and the value was neither redacted nor seen by the residual gate. The
  prefix is now any number of backslashes; the redacted Args stay valid JSON with the text after the value intact. The cookie class
  has no back-references to count with either, so it writes out the levels it reads (none, one, three, seven and fifteen), as its bullets below say.
- **More assignment shapes, quoted literals only.** The assignment class put nothing but `=` or `:` between the key word and a
  value, so Go's `password := "<value>"`, `const token: string = "<value>"`, `var token string = "<value>"`, `password: str = "<value>"`,
  `if password == "<value>"`, `password = b"<value>"` and `r'<value>'`, `os.environ["API_TOKEN"] = "<value>"`, `{'password' => '<value>'}` and a quoted
  JSON key `"token": "<value>"` matched nothing, and a type annotation such as `token: Record<string, string> = "<value>"` was
  matched in place of the value, which left the value in the text. A second alternative (tried first) takes `:=`, `==`, `!=`, `=>`
  (also as Go's encoder writes it, `=>`), a type annotation, a closing quote or bracket after the name and a `b`, `r`, `u` or `f` string
  prefix, and requires a QUOTED literal of 12 or more characters: an unquoted value after `:=` or `==` is an identifier or a call
  (`token := strings.TrimSpace(line)`), which would flood. A third takes a call that passes the name and the literal as two arguments
  right behind an opening paren (`t.Setenv("API_TOKEN", "<value>")`, `setProperty("password", "<value>")`); the match starts at the paren, and a
  list of strings (`["token", "client_identifier"]`) stays quiet. The harvest now remembers the literal itself and not the type
  annotation or call in front of it, so ordinary code that shares a long type name with a redacted line is not read as a leftover.
  Measured on the tracked tree: the assignment class matched 37 places before and 64 after (counted before this fragment was written); the
  27 added are quoted fake values in test files and one config example, none an identifier. A type annotation counts only when an `=`
  follows it, so prose such as `password required: "..."` stays quiet. Not covered and now named: `fmt.Sprintf("password=%s", v)`, where the value is a
  separate argument, and an unquoted value after the wider operators. The table costs time: about 1.5 s per MiB for all classes
  (three runs, 1.51, 1.54 and 1.54 s over 1 MiB that matches no class; the figure the first version of this note gave, about 1 s,
  understated it).
- **A Basic credential in a printed header or a call is recognised.** The class took a colon or an equals sign straight after the
  header name, so a Go `http.Header` dump (`map[Authorization:[Basic <value>]]`), `req.setHeader('Authorization', 'Basic <value>')`,
  `req.Header.Set("Authorization", "Basic <value>")`, `con.setRequestProperty(...)`, `headers["Authorization"] = "Basic <value>"`, a
  hash rocket, `http.Header{"Authorization": []string{"Basic <value>"}}` and `[Authorization] => Basic <value>` were not matched (the
  Bearer form of the same dump was). Between the name and the scheme the class now takes a run of up to eight punctuation characters
  that holds at least one of `: = , > [` (a JSON escape and Go's `[]string{` opener count as punctuation); a word in between, or the bare
  words with no separator, stay quiet. The value ends before a closing bracket, paren or brace, which base64 never holds, so
  the redactor does not eat the closers of a dump or a call.
- **A quoted credential literal runs to its closing quote.** A quoted value ended at the first space, comma, semicolon or
  ampersand inside it, so a quoted literal holding an ampersand, a comma or a space after the name (an assignment, a JSON field, a single-quoted shell value, an `export`)
  were total misses whenever each side was shorter than 12 characters, in the plain assignment, in the wider-operator forms and in the
  call form. A literal that starts with a quote now has its own body: it runs to the closing quote of the same kind, takes in the other
  kind of quote and Go's JSON escape for `&`, `<` and `>` (so an ampersand in Args does not end it), and never passes a backslash (an
  escaped quote in JSON text), a line break or the escape byte of a control sequence. The closing quote stays out of the match, and the
  first-character rule (no whitespace, none of `/ . [ ]`) holds inside the quote, so a quoted path after a key name stays quiet. An
  unterminated quote runs to the end of the line. The harvest redactor remembers the value the class took (`piishape.AssignmentValue`
  names it from the same pieces) and no longer cuts the match at its last space, comma, semicolon or ampersand, so the value-tail gate
  covers the whole quoted value. The cost is precision, measured on the tracked tree: the assignment class matched 67 places before and
  82 after (a scan of the tracked text files, counted before this fragment was updated). The 15 added are prose in quotes after a key
  name (a status map entry for `credentials`, a test string that starts with `secret = "the goal named`), placeholders with spaces in doc
  examples, and a Go string literal's closing quote read as an opening one (`... token: ", tailJWT(), ...`); none is a credential, and the
  cookie class matched nowhere in the tree. Quiet cases are pinned (a literal under 12 characters, a name that only contains the key word,
  a leading `/`, blank padding, a bare word run with no quote, a literal that continues on the next line).
- **The cookie header takes a whole span, not a cookie at a time.** The release lane's reading found seven defects of one design, three of
  them high: the class walked the line cookie by cookie, with a fine-grained rule for what a cookie value may hold, and every value
  outside that alphabet ended the match with the credentials behind it going to the seat and the corpus verbatim. A quoted value with a
  space, an apostrophe, an ampersand, a comma, a semicolon, a tab, a shell escape (an escaped dollar sign or backtick at the third level)
  or one of Go's HTML escapes for `&`, `<` and `>`; a Set-Cookie list whose first element was short or whose later elements held the
  credential; a header folded after the colon; and a cookie named like a credential (`refresh_token=`, `password=`) that the assignment
  class half-took first were each a way out. The design changes instead of a seventh patch. Once the header name and an opener fire (the
  Basic header's punctuation, a comma followed by a quote as in `req.Header.Set("Cookie", "...")`, a header fold on either side) and the
  span holds a cookie value of 12 or more characters, the match runs from the header name to the first of the end of the line (a line
  break, its JSON escape or the end of the text), the closing quote of the string the header sits in (a quote that opens no quoted
  cookie value: bare, or with 1, 3, 7 or 15 backslashes, the levels of escaping after none to four layers; a single quote ends it too, so
  a curl command's header ends before the shell's closing quote) and the closing bracket of a list or the closing brace of a Go slice
  literal (`"Set-Cookie": [ ... ]`, a printed Go header, the Go-syntax print of one, a Python list: one span however many elements and
  lines it has, a bracket inside a string element not ending it). The closing quote and the bracket stay out of the match, so the
  redacted Args stay valid JSON (a JSON document that held the header as a key does not, since the match starts at the key's name).
  Nothing inside the span is read for an alphabet. Over-redacting a cookie line is accepted by design: it is evidence of a header, not of
  the build. What starts a match stays precise: some value in the span has 12 characters with no white space from its start (an escape
  counts as one), so `http.Cookie{...}` literals, `Header.Get("Cookie")`, a Go string concatenation such as `"Cookie: sid=" + f(x)`,
  Django's short messages and the HTTP reference's example header stay quiet. The fine-grained value grammar now lives only in the harvest
  redactor, which remembers every value in the span for the value-tail gate (a quoted body whole, with its spaces, quotes, escapes, line
  breaks and the characters above, the closing quote found by the number of backslashes in front of it). The apostrophe gap stays and
  applies to an unquoted value only (`note=don't` cannot be told from the quote that closes a header written in single quotes); quotes
  that are no level of escaping, a header folded again after its first cookie and a cookie passed apart from its header (`curl -b`) are
  named gaps. Measured on the first version of this class: a generated check of 17,057 lines (48 cookie shapes, 18 wrappings) broke the
  contract in all 18 wrappings with the old class and in none with the new one. The next bullet is the review of that version.
- **The whole-span cookie class after its review: seven findings fixed, red first.** A review of the whole-span class (verified one by
  one, each reproduced) found one high and six medium defects, every one a way a value of 12 or more characters reached the corpus with
  the residual gate clean, or the span ran into the next field. (1) The high one was the class order: run first, the cookie class took the
  key word of a quoted credential on the same line into its span (an unquoted header, then a credential whose value is quoted) and stopped
  in front of the quote, leaving the value with no key word for any class and no memory in the value-tail gate. A matrix of 8 cookie forms,
  12 credential forms and 12 layouts, as text and as Args, left 1,584 of 2,304 values in the text with the old order and none with the
  table's order; the redactor ran the cookie class in the table's order for a time (the next bullet says where it runs now). (2) A
  last cookie that ends in an equals sign (base64 padding, or no value) in front of the closing quote of the string made that quote the
  opener of a value: the span ran through the next field, key or argument, broke the Args JSON, and when the next key was a second header's
  name its value was left outside every match. A quote now opens a quoted value only when a value can start behind it (not white space, a
  separator, a closer or the escape of a tab, an ampersand or an angle bracket). (3) A header printed with the Go-syntax verb is a slice
  literal; the span stopped at its first element, or matched nothing when that was short: it is now a list that ends at the closing brace.
  (4) The quotes of JSON in a shell string in JSON carry seven backslashes (fifteen with a fourth layer); the class knew none, one and three:
  the levels are now 0, 1, 3, 7 and 15. (5) A line break inside a quote that is still open ended the span; the value now goes on to its
  closing quote when that is within three more line breaks and ends at its line when it is not. (6) A shell escape (an escaped dollar sign,
  backtick, space or semicolon) or a backslash path in an unquoted value ended the span or stopped any match, and a quoted value cut by the end
  of the text behind a long bare one was left; both are inside the span now. (7) A table of single-guard mutants showed guards no test
  reached; tests pin them and the dead alternatives (the hash rocket's greater-than sign in the openers of a list and of a slice literal (the one in the line opener was live and came back, see the next bullet), a lower bound that could not be
  reached, a SameSite entry that could not hold 12 characters) are deleted.
  Red first: 13 failing tests and a generator axis were committed before any fix and run against the previous class, then the guard tests (some red,
  some green by design and red against the mutant that removes the guard, each said so in its commit), then the fix. The generator grew from
  17,057 to 74,119 cases (60 cookie shapes, 46 wrappings: header last in its string, a document, a command followed by data, lists opened by
  an equals sign or a hash rocket, lists cut in their last element, the Go-syntax print of a header in four places, and 16 chains of two to
  four layers of JSON and shell escaping); against the previous class 34 of the 46 wrappings break the contract (29,271 lines with the wrong
  span, 28,024 leaking a value, 67 breaking the Args JSON) and none does with the new one. Measured with the new class: it matches nowhere in
  `internal/` and `docs/` (2,276 files, 27 MB), and in Go's `net/http` source (158 files, 2.8 MB) the same four places as before, each with a
  real cookie value in it; it scans 1 MiB of Go source in about 0.1 s, as before (the next bullet has the cost of text built to be slow for it and the size of the pattern). A mutation pass after the fixes (47 mutants of the class, 29 of the harvest scanner: each level, break limit,
  opening rule, escape bound, closer, list opener, count and stop character) killed all but one, which is equivalent (the index at which the
  scanner resumes after a closing quote, which is only re-read as a quote that is not followed by an equals sign). Named gaps after the review: a raw JSON value (a bare double quote inside an unquoted value) ends the span at the
  quote, and so does an apostrophe; a backslash and a lowercase n or r in an unquoted value is the JSON escape of a line break and ends the span (a raw path such as `dir=C:\new\x` ends it there); a quoted value that starts with white space or a separator is not a pair; quotes with two, four to six or
  eight to fourteen backslashes are no level; a Go slice literal with its type left out is read as a line.
- **The whole-span cookie class after its third review, red first.** A third review (every finding reproduced three times, most of them
  compared with the commit before the second review's fixes) found that those fixes had traded some defects for others, and some older ones. Most let
  a value of 12 or more characters through with the residual gate clean. (1) Order: with the credential classes run before the cookie class,
  a cookie named like a credential with a double-quoted value was taken by the assignment class, which leaves the closing quote behind its
  placeholder, and the span stopped at that stray quote, so every cookie behind it was written out; a JWT, a Bearer value or a key-word
  cookie inside an unquoted printed list (a header printed with the plain verb, a Java map) left a placeholder that holds the bracket that
  closes the list, and the span ended in it; and redacted Args lost their closing quote when an empty quoted cookie came ahead of a key-word
  cookie in such a list. The cookie class runs first again, so the span sees the text as written, and the reason it had moved is answered
  another way: when the cookie class has substituted anything, the credential classes also run over the text as it was written, and every
  credential value of 12 or more characters they find is substituted wherever it still stands (longest first, with its own placeholder,
  remembered for the value-tail gate); the 2,304-case matrix of an unquoted header followed by a quoted credential stays green. (2) A header
  printed with the Go-syntax verb holds its keys in sorted order, so a Cookie with short values stands in front of a Set-Cookie with long
  ones, and a string of a list that could stay open let the pairing of quotes shift by one: the match ran from the first key over the
  closing brace of its list to the long value of the second and on past the quote that closes an Args string. A JSON object of two lists, a
  Python dict and a Python list with a quoted value inside Args (the single-quoted string looked for the quote of the value at one level
  only) did the same. A string of a list is now closed in the middle of it and open only as its last element, and a single-quoted string
  looks for a quoted value at every level. (3) The count that starts a match took a JSON unicode escape as one character and nothing else,
  so a shell escape, an escaped slash or a backslash path inside the first 12 characters of the only long value left it whole; a capital N
  or R after a backslash (a Windows path) ended the span as if it were the escape of a line break. A bare value counts the escapes the span
  takes now, and only a lowercase n or r is the escape of a line break. (4) The greater-than sign right behind the header name (an XML
  element, an arrow) was live, and the second review had deleted it as dead; it is back. (5) Cost: a text that repeats a header over and
  over, each one opening a list that never closes, was scanned to its end once per match and took the square of its length (16 KiB, 0.6 s; 1
  MiB, hours), because a match cannot be reported while a thread of higher priority is alive. A header line now takes no opening bracket (a
  list opens behind the punctuation, a space or the name), the line alternative goes first, and a quoted value starts a match by the rule of
  a pair (a quote followed by a closer or a separator closes what came before). (6) Three guards that a mutant table found unreached are
  pinned: the bound on the backslash run in the value scanner (a text cut in a backslash run made the redactor read past its end and panic),
  the quoted value that starts with a backslash-u escape of U+0100 to U+0FFF (Python's json.dumps writes one for every non-ASCII letter;
  without its alternative the long cookie behind it was written out), and the resume of the value walk after a closing quote.
  Red first: the redactor tests of each defect and the generator axes (the bare escape inside the 12 characters; a two-key header in four
  wrappings, a JSON object of two lists in three, a Python dict in two and a Python list in Args; in the redactor generator the cookies
  behind a key-word cookie, a JWT and a Bearer value in three more wrappings that keep an earlier placeholder inside an unquoted list) were
  committed before any fix and run against the class as it stood, and so were the scan-time tests (4 KiB 41 ms, 16 KiB 594 ms, a factor of
  15 for 4 times the text); the guard tests are green by design and red against the mutant that removes the guard, each run as `go test -overlay` of the one test. The sweep of credential values is pinned by the 2,304-case matrix of an unquoted header followed by a quoted credential, which leaves 1,589 failing lines (values still in the text) when the sweep is removed. The generator grew from 77,417 to 145,862 cases (74 cookie shapes, 56 wrappings). Measured: the pattern is
  132,494 program instructions (54,527 before this review) and compiles in 26 ms; the class alone scans 1 MiB of Go source in 0.1 s, and 1 MiB of each text that reviews found to be slow takes 0.4 to 1.0 s and grows in step with its length; a full redaction (every class) of 1 MiB of those texts takes 1.4 to 2.1 s against 1.2 s for source text. A random search over 4,900 units of up to a dozen pieces (header
  names, quotes at two levels of escaping, backslash runs, brackets, equals signs) and a hill climb on the slowest eight, run on a gate
  runner against the class after the first cost fix, still found four families whose time grew with the square of the length (16 KiB, 0.3 to
  1.2 s); the line-first order and the pair rule for a long value remove them, and two searches with new seeds against the final class
  (9,800 units and 16 hill climbs) found none: the units that looked slow at 32 KiB scale linearly at 0.7 to 1.3 s per MiB from 8 KiB to 512
  KiB. A new named gap: a bare value with a shell escape inside four layers of escaping (more than sixteen backslashes) is left whole.
- **A fourth review of the cookie class: what it takes, and a gate that fails closed.** Each defect below was a value of 12 or more
  characters left in the text with the residual gate clean, or a document the redaction broke; each has a test that was red before its
  fix (the failure lines are in the commits). (1) In an unquoted printed list (`map[Cookie:[a=b c=d]]`) a closing bracket is a character
  of the cookie when a character of a value follows it and the one in front of it is no opening bracket (`sid=ab]cd`, the PHP-style name
  `user[id]=v`); it used to end the span, and the tail of the value, or the whole cookie, stayed. The empty list of a typed slice literal
  (`[]string{...}`) still closes, and the slice literal is tried before the bracketed list. (2) The open single-quoted string a cut list
  ends in stops at a closing bracket, so an apostrophe in a value (`note=don't`) no longer runs through the closers of the Args around it
  and the redacted Args stay valid JSON. (3) The Go-syntax print of decoded JSON (`[]interface {}{...}`, `[]interface{}{...}`, `[]any{...}`)
  opens a list like `[]string{...}`. (4) A bytes or raw prefix in front of a quote (`b'cookie'`, `r"..."`) is part of the gap (ASGI scope
  headers, aiohttp raw headers, dicts of bytes). (5) An underscore in front of the name is no obstacle (`HTTP_COOKIE`, `http_cookie`; the
  test of the opener limits that pinned `my_Cookie` as a non-match changes with it). (6) A value that starts with an ampersand or a closing
  bracket is counted, in a cookie, and an ampersand (or Go's escape for one) in the assignment class; the case-sensitive island that keeps a
  capital N, R or U after a backslash a single character in the count is pinned. (7) The assignment class takes a credential header
  (`X-Api-Key`, `Api-Key`, `X-Auth-Token`, `X-Amz-Security-Token`) printed with a list of values (`map[X-Api-Key:[...]]`, a JSON or Python
  list), and a placeholder behind the name is not read as the list's value. (8) The Basic header's gap takes a line break, indentation, a
  YAML dash and a placeholder left by an earlier class (`json.MarshalIndent`, jq, a YAML list, a header with two values). (9) A cookie match
  whose header name is a key (`{"Cookie": ...}`, `{'Set-Cookie': [...]}`, `Header.Set("Cookie", "...")`) keeps the name, its closing quote
  and the punctuation, replaces the cookies and puts back the closing quote of a list's last string, so redacted JSON is still JSON (the
  other classes still start a match at their key word). (10) The credential sweep reads the text once: the distinct values are collected into
  a set, grouped by their first 12 bytes and tried longest first at each position of one pass over the output, and the credential classes
  read the lines of the cookie matches only. It used to search the whole text for each value (one `Cookie` line among many lines with a
  credential each: 12 s for 1 MiB, 44 s for 2 MiB on one gate runner, 4 times the time for twice the text); one `Cookie` line now costs nothing
  measurable (1.56 s against 1.55 s without it for 1 MiB, 3.1 s for 2 MiB, on the same runner). The longest-first order, the memory of every swept value
  for the value-tail gate and the count of the places replaced are pinned by tests that go red against a mutant of each.
  (11) The residual gate fails closed (`piishape.KeywordRun`, named `credential-keyword-run` by `Redactor.Residual`): a credential header
  keyword (cookie, authorization, api key, auth token, security token; any case, inside any longer word or name) with a run of 12 or more
  characters with no white space, quote, comma, semicolon or backslash starting within 32 characters behind it. The harvest leaves such a
  trace out with a note that does not echo the text, and the evidence reducer keeps the output raw and does not call the seat. A YAML block
  list item, a backtick-quoted value, a HAR name/value pair, an XML attribute and a scheme other than Bearer and Basic are covered by it
  and not by new openers: the text is skipped, not redacted. A placeholder is not read and ends the reach of the keyword in front of it; a
  line break ends the reach once a word has been read, and goes on only behind a colon, an equals sign, an opening bracket or brace, a comma
  or a dash; another keyword is no part of a run; the placeholder of a cookie header followed by a YAML dash and a long run (the items
  behind the first of a block list) fires it too. **The trade:** the check is wide on purpose. A test or an identifier whose name starts
  with a keyword (`TestCookieJarKeepsTheValues`, `authorizationHeaderValue`) is a long run right behind it. Over the Go sources, tests and
  Markdown of this repository it fires on 5.4 % of the files (125 of 2,311: 34 of 595 Go sources, 76 of 1,544 Go tests, 15 of 172
  Markdown files), nearly all with a long identifier or word behind a keyword, so a trace that holds such text is skipped and a build
  output that prints it stays raw; it costs 0.08 s per MiB of redacted text. Still open: a value in doubled apostrophes after a password
  name, and a credential behind a keyword that is not in the list.
- **A Go module version is not an email address.** The email pattern read `toolchain@v0.0.1-go1.27.2.linux-amd64`, which every Go stack
  trace and module-cache path carries, as an address. Measured on 9.4 MB of real Go build and test logs cut into 4, 16 and 64 KiB pieces,
  it matched 67.6 % of the 4 KiB pieces (55.7 % of the 64 KiB ones), so the reducer would have kept most Go logs raw; a domain may no
  longer start with a lowercase `v` and a digit, and the same logs now match 0.3 % (all real addresses). No credential class matched
  any piece, before or after the widening. The coding-agent page now says what the check is for: defense in depth on the seat path, the
  only automatic credential layer on the corpus path, and a best-effort shape list in both.
- **The quote lookup moves as well.** `internal/agent` could not import `internal/research` to call `research.VerifyQuotes`:
  research imports `internal/delegate`, and delegate's own tests import the agent, so the import is a cycle in delegate's
  test build. The lookup (`locateQuote` with the fold code under it) moves, verbatim apart from exported names, to the new
  `internal/quotecheck` (`Locate`, `NewFolds`, `How`). `research.VerifyQuotes` keeps its signature, its report and its
  behaviour and calls it; research's tests, which exercise it through `VerifyQuotes` and `AnnotateWire`, are unchanged and
  green, and `quotecheck` has tests of its own. The reducer calls `quotecheck.Locate` directly, so the agent's dependency
  closure does not grow by research, delegate, config or the ledger.
- **Where it sits.** At the tool-result boundary in the loop, after the per-result cap and the environment rules and before the
  read budget, the effect record and the pager, so each sees the bytes the transcript really carries, and only for a call that ran.
  It is not a rung of the compaction ladder, which rewrites older bodies under budget pressure and is model-free by design: this
  runs once, when a result is first produced, and costs one seat call on the loop's critical path (seconds per qualifying result).
  The ladder, its options and its default bytes are untouched.
- **A repeat is honoured.** A receipt is not the output. With the reducer on, an exact repeat of a reduced call is re-run once
  (the receipt counts as a destroyed result for the repeat breaker, the same recovery path the compaction rungs use) and that
  result is pinned and not reduced; any further repeat is refused as before. With the reducer off the breaker is unchanged, even
  for a body that merely opens like a receipt. `IsEvidenceReceipt` recognises a receipt by its opening line and nothing that only
  mentions it; the trace harvest asks it, so a transcript that holds a receipt is refused like any other already-shortened body.
- **Config and doors.** `agent_evidence_reducer` (bool, `false`), documented in the operator guide and in `config.example.json`
  (regenerated). `BuildConfig.EvidenceReducer` installs `Loop.WithEvidenceReducer(Offload)`; with the key on and no seam, `Build`
  says so in its notes and installs nothing. The local agent CLI (its three `Build` calls), the MCP front door and the fleet-node
  agent task read it from the node's config; a source-level test requires every `BuildConfig` literal that takes the node's agent
  policy to hand over the key. The prompt-replay builder takes no node policy and is left alone, so a prompt A/B is not perturbed.
- **Tests.** Red first, in four red commits each followed by its green one: the shared credential table and the shared quote
  lookup against stubs, the reducer's
  properties against a no-op stub (the tests that need a receipt fail), then the key's wiring (a missing key in the example and
  five doors without it failed first). The properties are tested through the loop with a scripted planner and a scripted seat: the 4 KiB edge (4095
  stays, 4096 is reduced), the 64 KiB ceiling, the receipt's claims, quotes and verbatim exit line (including `exit=137 (killed by
  signal 9)` against a seat that claims success), reads and searches bypassed with the seat never asked, a missing exit line,
  an unverifiable quote, a credential shape (the seat not even called), a receipt that is not smaller, twelve ways the seat can
  fail, forged lines, a refused call, the repeat path with the reducer on and off, and the harvest refusal. A run of 22 mutations
  (each guard, threshold, the producer gate, the pinned bypass, the repeat recovery, the exit check, the credential checks, the
  quote shape, the claim cap, the panic guard) killed all 22; two initial survivors (a quote-shape check made redundant by the
  span check, and the exit-status post-condition) led to removing the redundancy and to a direct test of the check. The four
  fixes that followed the release lane's delta read (quoted values, cookie quoted pairs, cookie openers, cookie escapes) are red first as
  well, a red commit before each fix commit; the value-tail test for every cookie value landed with the harvest code of the first fix, and
  was run red against the older harvest file. The value-tail test for the session value behind a quoted cookie with escapes was already
  green at its red commit (the harvest finds that value once it is inside a match); the red one is the redactor test for the
  session cookie behind a quoted cookie with escapes. The whole-span change of the cookie class is red first as well, one red commit
  with the generator and the probes, run against the old class: all 18 wrappings broke the contract, with 10,130 of the generator's
  17,057 lines matched to the wrong span, 8,642 leaking a value and 326 matching with no long value in them; then the change. The
  redactor probe for a key-word cookie was red on its class-order assertion only; that assertion was given up in the review below, whose
  order finding showed it protected the accounting and not the values.
- **Not verified here.** Nothing in this change ran against a live seat. The acceptance measurement is a seat A/B on the 9B and
  27B agent seats through the gate's non-inferiority mode (tool-call reliability and pass rate), with forwarded prefill tokens as
  the efficiency metric and never seconds; until it passes the key stays off. Also open: how often real seats return a usable
  answer (the `extract` task's fixed 512-token completion budget holds about five claims with short quotes, and an answer cut by
  it falls back to the raw output, which would make the step nearly inert); whether a seat quotes single verbatim lines as asked;
  the receipt's effect on the agent's later steps (the thing the A/B exists to find out); the seat-call latency on the critical
  path; and the planner-seat render of the cascade call (the offload closure renders on the planner seat without thinking when
  the planner is not the workhorse). Fall-back reasons are logged per attempt (`agent evidence reducer: kept the raw output of ...`)
  and not yet counted on the run result.
  The whole-span cookie class was tested on fixtures built from pieces, on this repository's tracked text and on the Go standard
  library's `net/http` source; no real harvested trace went through it, and the review's fixes were not run against a live seat either.
  The opening rule (a quote followed by white space or a separator opens no value) and the three-line-break bound are design choices
  pinned by tests, not measured against real traffic; the timings are one series on a noisy shared machine. The widened value class and the tail gate were tested on fixtures only; no real harvested trace was re-run through the redactor here.
  The same holds for the control-sequence, quoted-literal and header shapes: the fixtures are built from pieces, and no real colored
  build log or real harvested trace was run through them. The scan cost (about 1.5 s per MiB; 1.51, 1.54 and 1.54 s) is one series of three runs on a noisy shared machine, and so are the timings of the third review's slow texts (a desk machine at two threads, and a gate runner for the random searches), and the 67-to-82 count of assignment matches is one scan of this repository's tracked text.
  The fourth review's fixes and the keyword check were tested on fixtures built from pieces, on this repository's text (the 5.4 %
  figure) and on a census of 825 planted cases of the review's leak families (705 left a value in the text with the gate quiet before,
  30 after, and 120 are named by the gate; the 30 are a value in doubled apostrophes after a password name, which no class and no keyword covers); no real harvested
  trace or build log went through the keyword check, so how often it skips a real trace is unmeasured beyond the repository's own files.
  The sweep's cost was measured on two gate runners, one series each.

### Added - offload_run_template: list, inspect and run the ComfyUI workflow templates a node carries, behind a licence gate, with trust on first use for model files

- **What it does.** One MCP tool, `offload_run_template`, with an `action`: `list` (filterable, paged, sorted, the same bytes for the
  same tree), `get` (one template's manifest and licence class), `readiness` (each model present or missing, directory-aware; each
  input file; node packs; ComfyUI's version against the template's minimum; the licence verdict) and `run`. `run` asks
  `render/run-template.mjs resolve` for the gate, and only for a template it passes sends a `run_graph` task through the same call
  `offload_run_graph` makes (route `local`: the same lease, ComfyUI lifecycle, satisfier and envelope), with the template **name**, a
  runtime manifest and optional `overrides` and `seed`. The runner (`comfy-run-graph.mjs --template NAME`) converts the template with the
  converter once ComfyUI is up, with that ComfyUI's own `/object_info` and version, and before preflight; a refusal is the typed defer
  `TEMPLATE_CONVERT_REFUSED`, and a model file the converted graph loads that the manifest does not name is `TEMPLATE_MODEL_UNLISTED`. An
  override sets values and never retargets a file or names an embedding: one that targets a model or input loader node, an input
  that holds a model file name, a value that is a model file name, or a string value with `embedding:` in it is refused as `override_is_file`,
  because the licence gate, the FLUX bar and the trust-on-first-use check read only the models the template names (one limit is stated with the embeddings, below: text that string nodes assemble while the graph runs is not seen). The manifest generator is `render/template-manifest.mjs` (templates to manifest entries: models as
  `models/<class>/<name>` with the source and the hash a template declares, node packs, licence class, API or not, input files read or not).
- **The gate.** This release runs a template only if it is not a paid-API one, not FLUX-family, needs no custom node pack, is readable and
  has a permissive licence class (or no weights). A paid-API template is a Defer (`deferred:true`, class `api_paid`, never cloud). Every
  other case is refused with a typed class: `flux_barred`, `custom_nodes`, `license_unknown` (a repository the map does not list, a weights
  file with no Hugging Face source, or a map entry that was inferred from a sibling and not read: unknown is not permissive),
  `license_conditional`, `license_non_commercial`, `template_unreadable`, `subgraph_unexpanded`, `bad_model_path`, and what the node lacks with the exact list:
  `models_missing`, `embedding_unverified` (a prompt embedding the harness does not verify, below), `inputs_missing`, `comfyui_too_old`. Nothing
  is leased, started or sent for a refused template. A run never downloads a
  model: the runtime manifest names each model by the path it was found at and carries no source.
- **Where it runs.** On this machine's lane only. The fleet door takes an API-format graph and a template is converted on the node that
  runs it, so a call that names a route beyond local, remotes or a node is refused as `fleet_not_offered`. A template that reads input files
  (an active `LoadImage`, `LoadImageMask`, `LoadImageOutput`, `LoadVideo` or `LoadAudio`) is a local-only ticket for its own reason, and the
  answer says so (`local_only`, `fleet_note`): a node can match an input file only by name.
- **Host RAM.** A template run carries `template` and `manifest_path` and no graph file, so the run-graph lane read nothing and declared
  the media class default: the largest family the box binds (33.1 GiB on a box that binds a Wan 2.2 pair) for a template whose files
  are 23.1 GiB, held by the host-RAM brake though the box could run it, and on a box that binds no ComfyUI render family, 0 (source
  `none`), so a big template got no host-RAM check at all. The lane now declares the model files the runtime manifest names
  (`models/<class>/<name>`, sized under the ComfyUI model roots; `hostneed.ForManifestModels` and `ForManifestFile`), by the rule
  a graph's loader files get: the sum, each file once, in full when together they do not fit the card (less the call's
  `reserve_vram`), 0 when they fit, source `graph loaders`. Any doubt keeps the class default exactly as before: no manifest, one that
  cannot be read, one that names nothing, an entry that is not `models/<class>/<name>`, or one file the box cannot find or size (no
  partial sum). The lane probe reads the same figure through the same function, so it agrees with the admission. A name is looked up
  under every class directory, as a graph's is, so a name held under two classes is sized as the larger. An estimate from the files'
  sizes, not a measurement.
- **Subgraph depth.** The catalog expands eight levels of subgraph instances and the converter up to sixty-four, and a template's models, licence
  class, FLUX bar and host-RAM figure are read from the catalog's walk. A template with one loader within eight levels and another at a deeper
  level got a manifest naming only the shallow file while the converted graph loaded both (reproduced at nine nested instances), and the host-RAM
  figure summed the short list. A template with an instance the walk left unexpanded (nested past eight levels, or a subgraph that contains
  itself) is now refused as `subgraph_unexpanded`, ahead of the checks that read the graph, in `list`, `get`, `readiness` and `run`. The two depth
  limits are kept apart on purpose: the converter bounds a branching nest by counting nodes, and the catalog walk has no such count, so its depth is
  what keeps it small; a template nested deeper than eight is refused rather than run, though the converter could convert it. No template of a
  602-template copy of a newer package (`json 0.1.108`, 203 with subgraphs) has an unexpanded instance; that is evidence for the counts below, not a
  re-scan of the package they were measured on. The runtime
  manifest carries no completeness mark and the Go side has no backstop for one; the refusal stops a short list from the case above, and the
  runner's comparison (next bullet) stops it whatever its cause (`hostneed/manifest.go` says so).
- **Model files only the converter reads.** The manifest is the catalog's static reading of a template and the graph is the converter's, two
  readers of one file, and the licence gate, the FLUX bar, trust on first use and the host-RAM figure read only the manifest. Three ways they
  differed: an active subgraph instance's own `widgets_values` (the converter routes them into an inner loader, through `proxyWidgets` or as
  the literal a subgraph input takes), a loader whose `widgets_values` is an object keyed by input name (the converter reads both forms, the
  list was read only in its list form), and a loader that stores fewer values than it has widgets and takes the listing's default. A template
  with the first or the second resolved to `run` with the licence class of its other files (or none), and loaded the file the list lacked.
  Two changes close them. The catalog now reads both forms of `widgets_values` and the values of an active instance, so such a file reaches
  the licence class, the FLUX bar and readiness before a run (an unmapped one refuses the template as `license_unknown`; a value of a muted
  or bypassed instance is not read, and one an annotation covers is not listed twice). And the runner compares the converted graph with the
  manifest before preflight (`unlistedModels`, `template-run.mjs`): a model file the graph loads that the manifest does not name defers
  `TEMPLATE_MODEL_UNLISTED` (the first such file is the `ref`) and nothing is posted. The comparison reads the graph that will run, so it
  covers the third way, which the catalog cannot see, and any other. A file matches an entry as the catalog matches an annotation to a widget
  (by the name under the class directory or by its base name, ignoring case and slash direction, `modelCovered`); a graph run is not compared.
  The proxied value is listed with no class, and its precedence over the inner loader's own annotation is not modelled: when both name
  a file, both are required, which can ask for one file more than the graph loads and never one fewer. Measured on the installed package
  (`json 0.1.96`, 578 templates, read only): the catalog lists the same 897 model entries, with the same licence class, FLUX flag and verdict
  per template, before and after this change.
- **Embeddings named by the prompt text.** ComfyUI's CLIP tokenizer loads `models/embeddings/<name>`, then `<name>.safetensors`, `.pt` and
  `.bin`, from a word of the prompt that starts with `embedding:` (`comfy/sd1_clip.py`, `load_embed`, read in ComfyUI 0.38.0). No loader node
  and no file extension is involved, so a text override such as `embedding:evil_ti, a cat`, `embedding:evil_ti.pt, a cat` or
  `(embedding:evil_ti:1.2) cat` was accepted (only a value ending in `.safetensors` was refused), the runner's comparison saw no file, and the
  licence gate and trust on first use never read it, which contradicted "an override never retargets a file". One reading of the reference
  (`embeddingRefs` and `embeddingCovered` in `templates-catalog.mjs`) serves the catalog and the runner's comparison; the override check keeps
  its own, wider matcher (`mentionsEmbedding`), so that what it lets through is never a text the reader finds a file in.
  - Where ComfyUI looks, and what the harness does about it. Not in the directories of the class one after the other: `expand_directory_list`
    puts every embeddings root and every subfolder below it (links followed) into a Python `set`, and `load_embed` opens the file of the first
    directory of that set that holds the name, so the directory that answers is chosen by hash order and differs from one process to the next.
    Two things follow. A file that sits only in a subfolder (`sd15/foo.safetensors`, a common layout) is the file `embedding:foo` loads. And a
    name that more than one directory holds (the root and a subfolder, two subfolders, two registered roots) is not one file: the trust record,
    the hash and the licence approval would attach to a file ComfyUI may not open. The harness does not replicate
    that choice (see the fail-closed bullet below).
  - Override: any string value that contains `embedding:`, in any letter case, is refused as `override_is_file`. ComfyUI matches a lower-case
    word that starts with the identifier; matching it anywhere and in any case is the fail-closed side.
  - Runner: a string input of the converted graph that names an embedding the way ComfyUI reads it is read as text and no longer as a file
    name. Each name it asks for is reported as `models/embeddings/<name>` (the first is the `ref` of `TEMPLATE_MODEL_UNLISTED`) unless the
    manifest names, under `models/embeddings/`, a file for it: the name as written or with `.safetensors`, `.pt` or `.bin` appended, at the top
    of the directory or in any subfolder (whole path segments, never a base name that only ends alike; case and slash direction ignored), and a
    name that is empty, absolute or has a `..` segment is never covered. A manifest entry under another class does not cover it.
  - Catalog: a prompt widget of an active node (or an active subgraph instance's own values; not a note, not a muted or bypassed node) that
    names an embedding is a requirement under the `embeddings` class with no source, unless an annotation of that class covers it by the
    name or with one of the three extensions, in the root or a subfolder; the annotation then records the name it covers (`embedding_refs`). A
    template whose own prompt names an unannotated embedding is therefore `license_unknown` and refused; an annotated one is read by the
    licence gate like any weights file. Text that only looks like a reference to the override's wider matcher (`Embedding: a primer`,
    `wordembedding:vec`) names nothing here.
  - Fail closed, deliberately narrower than ComfyUI. ComfyUI warns and skips an embedding it cannot find (`comfy/sd1_clip.py` logs
    `embedding:<name> does not exist, ignoring`, drops the word and runs the prompt without it). The harness refuses instead: a missing or
    different embedding silently changes the output of a vetted template, and the licence class, the hash and the trust record attach to the
    file the manifest names. Readiness therefore accepts a name a prompt gives only when exactly one physical file answers it, case-exactly:
    the name as written, then with `.safetensors`, `.pt` and `.bin` appended, on whole path segments, in the roots and every subfolder,
    `.git` directories included: ComfyUI's embedding search walks them, so a file under one is an answer, while the listing a loader widget
    reads skips them (the scan walks the `.git` of an embeddings root that is a git work tree, as ComfyUI's search does). A file
    the class does not list answers the name as written too (no extension, or a whole name that is the reference), because `load_embed` asks
    `isfile` for it first with no extension filter; the inventory records those files (`unlisted_files`) for this class, with the files below a
    `.git` directory, and `unlisted` counts only what an extension keeps out. One such file is
    `present`, and the runtime manifest names it (`models/embeddings/<sub>/<name>.<ext>` for one in a subfolder, which the host-RAM figure can
    size and the satisfier and the trust check can find). A file reached by several paths is one file, by the file system's own word: the
    inventory records where each embeddings file really is (`reals`) and what the file system calls it (`ids`, so two hard links of one file
    are one file), and answers that open the same file are one answer. A root that is a link to another registered root (a model drive reached
    through the default directory), a link inside a root, a root registered inside another and one file linked into two directories (symbolic
    or hard) are each one file; two copies are two files. A file system that gives no file identity falls back to the real path, so there a
    hard link reads as two files; a drive that keeps the case it is given, or derives its file index from the path, can show one folder
    registered under two spellings as two files, and that is refused too. Everything else with a file in it is refused as the new status
    `embedding_unverified`, with a `reason` and the files in `found`:
    - a case variant answers it: the only file that answers differs in letter case. Whether ComfyUI finds it depends on what the volume does
      with a name in another case, and the harness does not ask the volume;
    - more than one file could answer it: the root and a subfolder, two subfolders, two registered roots, two extensions in one directory, a
      file with no extension beside a listed one, an exact file beside a case variant of it, and an annotated file (`embedding_refs`) beside
      another file that answers the same name. ComfyUI would pick one by set order or by extension, and a manifest can name only one file;
    - a probe name is answered by a file of the node (below).
    A node with no file for the name is `missing`, listed by the name the text gives (`models_missing` at the gate). The gate answers
    `models_missing` first when both apply, then `embedding_unverified` (the class, the code `TEMPLATE_EMBEDDING_UNVERIFIED`, the detail
    `embedding lookup not verified by the harness: <name>: <reason> (<files>)`, and the list in `embedding_unverified`); `readiness` blocks on
    both, in that order, and the readiness table, `nodeReadiness` and `summarizeReadiness` do not count such a template as ready.
    They do not count it as files short either: a template held back only by an embedding the harness cannot verify is in no bucket of the files-short histogram (nor of the
    table's nearest-node histogram), and is counted in `embedding_unverified` (`nearest_embedding_unverified`), which the text table prints as
    `embedding not verified (local)`. Deliberately
    not replicated: ComfyUI's choice between the directories that hold a name, its preference of one extension over another within a
    directory, and the case rule of the volume. The full replica is a later row, to be taken when the first real template needs it; no template
    of the installed package names an embedding in an active prompt.
  - A loader widget's embeddings file is not a prompt name. An annotated entry of the embeddings class that records no prompt name is the
    file a `ConditioningLoader` widget carries (three templates of the installed package, the Marigold depth, albedo and surface-normal
    ones). That node lists the directories by relative path (a subfolder file is `sd15/x`) and opens its value with the exact path under the
    first root that holds it, so it is met like a file of any other class: a subfolder-only file is `in_subfolder`, and a root file with a copy in a
    subfolder is `present`. The prompt lookup applies to an unannotated requirement and to an annotation that records the prompt names it
    covers (`embedding_refs`); `readinessFor` forwards `annotated` so the run gate makes the same split.
  - Other names ComfyUI may try (`embedding_probes`). Two readings of a prompt open files the requirement does not name. `_try_get_embedding`
    asks `load_embed` for the name as written first (`embedding:foo, a cat` asks for `foo,`) and strips the commas only when nothing
    answers; and the LLM-based text encoders (Z-Image, Flux.2, ERNIE and others) hand the same directory on with `disable_weights`, so the
    text is not cut at parentheses and no group weight is read (`embedding:foo(bar)` names `foo(bar)`, `a cat ( embedding:1.2 )` names
    `1.2`). A template does not say which encoder runs, and the two readings differ in both directions, so the CLIP reading stays the
    requirement. `embeddingProbes` returns the other names; they are not requirements (listing them would refuse every prompt with a comma
    after the name), the catalog row carries them as `embedding_probes` only when there are some, and readiness adds an entry
    (`embedding_unverified`, `probe: true`, the reason, the files in `found`) after the requirements when a file of the node answers one, in
    any letter case and however many files answer. Every probe of a prompt is asked, the second as well as the first. The template is then
    not ready and the run gate refuses it as `embedding_unverified` (nothing is missing), naming the probe name, because ComfyUI may open a
    file that no manifest names.
  - Grammar: the reader is a port of the tokenizer path of `comfy/sd1_clip.py` (`token_weights`, `tokenize_with_weights`,
    `_try_get_embedding`), compared with the extracted functions of that file on 150,000 generated texts with no difference. The text is cut
    at parentheses and a closed group loses a numeric tail, so `(embedding:name:1.2)` is `name` and a bare `embedding:name:1.2` is the name
    `name:1.2`, which ComfyUI fails to find; the identifier is lower case and starts the text or follows whitespace or a parenthesis, so
    `Embedding:`, `wordembedding:` and `a,embedding:x` are not references, while `an embedding: soft light` is (ComfyUI looks for a file named
    `soft`); whitespace after the identifier is skipped; the name ends at whitespace, `(`, `<` or `[` (an unescaped `)` with no group to
    close stays in it); commas come off both ends; `\(` and `\)` are characters of the name. Deviations from ComfyUI, each named: the name
    returned is the one after the comma retry and the reading is the CLIP tokenizer's, the one embeddings are made for, and the names the
    other tries (the name as written, the reading without weights) are probes, not requirements (above); `embedding:` with no readable
    name, and a text nested more than 500 groups deep (ComfyUI reads about 900), are the empty name, which every caller refuses; the
    class listing skips `.git` directories, as ComfyUI's listing does, and the prompt lookup walks them, as its embedding search does (above).
  - Limit: the override check and the runner's comparison both read the string values of the graph, so an embedding assembled while the graph
    runs by string nodes (a concatenation of two overrides, for example) is neither refused nor compared with the manifest, and the licence
    gate, the hash and the trust record never see the file it loads. "An override never retargets a file or names an embedding" is true of the
    values an override sets, not of text built from them.
  - Measured read only on the installed package (`json 0.1.96`: 594 `.json` files, which are the 578 templates of the index, `fuse_options.json` and 15 index-family files, `index.json`,
    twelve localized or mcp indexes, the schema and the logo): the package holds
    897 model entries, 3 of them annotations of the embeddings class (the loader-widget conditioning files that the prompt lookup does not
    touch), and no row carries `embedding_probes` or an annotation `embedding_refs`, so the prompt lookup has nothing to read in it. The
    per-template verdicts and the readiness per template on a snapshot of the measuring machine (`readiness --snapshot <file> --json --detail`,
    run before the simplification and at its head) are the same: 649 requirements missing, 214 present; the snapshot itself differs only by the
    dropped `case_insensitive_dirs` field. The only `embedding:` text in the package is in the
    markdown notes of four `video_minimax_h3_*` templates, which are not read (they are refused as `license_conditional`). That snapshot
    holds no embedding file, so the lookup was tested on fixture trees and on real files, links and hard links in a scratch directory (see
    the tests below).
- **Why a tool and not a `template=` argument on `offload_run_graph`.** Measured at 0.186.0: an average tool costs 1,019 tokens in the full
  profile and 672 compact, so one more tool is about +3.2% of the 31-tool full list. The registered design accepts that cost, and the
  `template=` argument was only its fallback for a cost that proved unacceptable. The new tool's own entry is 3,092 bytes of `tools/list` in
  the full profile and 2,930 compact, against means of 4,110 and 2,639 over the 32 tools of the default box (bytes, not tokens; the
  `tools/list` dump of `toolsprofile_dump_test.go`). It has a reviewed compact keep-list entry (four sentences: what a run is allowed for and
  refused with, that it never downloads a model, that an embedding the harness cannot verify is refused as `embedding_unverified`, where it
  runs).
- **Trust on first use for model files.** Most template annotations carry no hash (10 of 1,135 do). The first run that meets a model with no
  declared hash records its sha256, size and modification time in `<file>.tofu` beside it; a later run with the same size and time trusts the
  record without hashing, a changed file is hashed again, and a hash that is not the recorded one defers `MODEL_SHA_MISMATCH`, naming both. The
  check (`verifyTofuModels`) runs on every run, a cached one included, before ComfyUI starts. It is a different record from the `.sha-ok`
  sentinel, which means "matched a pinned hash". A record that cannot be read or written defers `MODEL_TOFU_RECORD`. The first sighting of a
  large file pays the hash once (minutes for 20 GB); bytes replaced under an unchanged size and time are not seen.
- **Also changed.** The manifest parser carries an optional `trust` per model. The satisfier now looks for a `diffusion_models` file under
  `models/unet` and a `text_encoders` file under `models/clip` too, as ComfyUI does (a run-graph manifest that names the modern class was
  reported missing for a file in the legacy directory). The catalog row names the input files its active loaders read (`input_files`).
  ADR 0058 is Accepted, dated 2026-10-14, with the third licence state `conditional`; its index row says so.
- **Measured catalog at this head** (installed package `comfyui-workflow-templates 0.11.70`, `json 0.1.96`; the research's 246 was a heuristic):
  578 templates in the index; 337 are paid-API (all three API signals agree); 80 are runnable by this release (5 with no weights; 75 with 247
  model entries, 237 of them trusted on first use and 10 with a declared hash; 55 of the 80 read input files); the others are refused as
  25 `flux_barred`, 63 `license_conditional`, 10 `license_non_commercial` and 63 `license_unknown` by first class (39, 64, 30 and 71 templates carry
  those reasons, counted over every kind, paid-API included; among the 236 local templates that need weights the catalog's tally is 75
  permissive, 63 conditional, 29 non-commercial and 69 unknown). `custom_nodes` is 0 for this package. 85 of 135 repositories of the local templates that need weights are not in the licence
  map (83 of 133 in the research). On one node (<node-a>, ComfyUI 0.38.0), 4 of the 80 were ready to run that day (73 lacked a model file, 52
  lacked an input file they read).
- **Tests.** Written red first: the red commit failed 39 + 12 + 16 tests of the three new Node files, 11 of the satisfier's and 6 of the
  run-graph flow's, 5 of `internal/rungraph`, 2 of the pipeline, 19 of the MCP door (a stub answered a bare deferral) and the ADR test.
  `node --test render/template-manifest.test.mjs render/run-template.test.mjs render/template-run.test.mjs` (a paid-API template is a Defer with
  a typed class; an unlisted licence is refused; a template that reads input files is local-only; `list`, `get` and `readiness` are
  deterministic on a fixture catalog), the satisfier tests (a hash mismatch is refused; the check runs on a cached run; a real-file round trip),
  `internal/mcpserver/runtemplate_test.go` and `toolsprofile_runtemplate_test.go` (the compact keep-list test covers the tool), and a Go test
  that runs the real script on a tree built from the catalog fixtures. Docs: `docs/systems/mcp-server.md` ("Workflow templates"),
  `docs/flows/run-graph-manifest-satisfaction.md` ("Template runs"), `docs/systems/media-generation.md`, `render/README.md`.
- **Subgraph depth tests.** Written red first: 4 of 5 new tests of `render/template-manifest.test.mjs` failed (a template nested nine deep, and one whose
  subgraph contains itself, were run instead of refused; resolve and list agreed); the fifth, a template nested exactly eight deep, passed before the
  change and pins the boundary.
- **Model-file tests.** Written red first: the runner test (a converted graph that loads a file the manifest does not name was posted, not
  deferred) and the three `unlistedModels` tests of `render/comfy-run-graph.test.mjs` and `render/template-run.test.mjs`; then two tests of
  `render/templates-catalog.test.mjs` and two of `render/template-manifest.test.mjs` (the list lacked the instance's file and the object-form
  file, and both templates were `run`). The guards that passed before the change pin what must not move: a graph run is not compared, a file
  is matched by path, base name and case, a muted or bypassed instance is not read, and on the two hand-written templates the converter's graph
  loads only files the catalog names.
- **Embedding tests.** Written red first: 18 tests failed on the code before the change (the three override shapes and the letter-case one,
  six of `unlistedModels`, `embeddingRefs`, the catalog requirement, annotation coverage and the subgraph instance, three readiness ones,
  and the verdict of a template with a baked-in embedding). The guards that already passed pin what must not move: a prompt that only
  talks about embeddings goes through, a muted node and a note add nothing, and an annotated embedding runs. The reading and the
  resolution those tests first pinned were corrected afterwards; their tests and mutation checks are in the next bullet.
- **Embedding lookup and grammar tests.** Written red first, one per behaviour: 13 tests of `render/templates-catalog.test.mjs` (seven of the
  grammar, two of the catalog, three of readiness and one of `embeddingCovered`), 4 of `render/template-manifest.test.mjs` (an embedding held
  only in a subfolder, an annotated one in a subfolder, a name held in the root and in a subfolder, an annotated file another file shadows), 2 of
  `render/template-run.test.mjs`, and the root prose test `run_template_docs_test.go` (the docs say how ComfyUI finds an embedding and name the
  string-node limit; it refuses the first wording of the lookup, a walk through the class directories in order that matched a whole path
  only). The test that pinned "a nested listed name is not matched by its base name alone" became its opposite. Guards written with them pass
  on the new code and bite under mutation: the override check is at least as wide as the reader (a corpus), Python's whitespace set and
  float() rules, a text of 150,000 groups, a text nested 600 deep, a model file name that holds an identifier-like word, and a file under
  another class. The reader was compared with the extracted functions of `comfy/sd1_clip.py` on 150,000 generated texts (27,440 with a
  reference), no difference (it found one: float() does not strip U+001C, which `str.split()` does); that comparison ran in a scratch
  directory and is not committed. Mutations of the reader, one per scratch copy of `render/`, each fail at least one test: a case-insensitive
  identifier (twice), a bare weight stripped, no cut at parentheses, no skip of whitespace, any group tail as a weight, float() without `inf`,
  `nan` or digit underscores or with the wrong whitespace, JavaScript's whitespace, escapes not honoured, no depth bound, a spread of a
  group's pieces, and a gate on the wide matcher in the catalog and in the runner.
- **Earlier review rounds of the embedding lookup (what still stands).** Each fix had a commit of failing tests before it. A loader widget's
  embeddings file (a `ConditioningLoader`) is met by its exact path under the first root that holds it, and the prompt lookup does not apply
  to it (`readinessFor` forwards `annotated` so the run gate makes the same split). A physical file is one file however it is reached: the
  inventory records the file system's id and real path of each embeddings file, so a junction to another registered root, a link inside a
  root, a root registered inside another and a hard link are one file, and two copies stay two. A file the class does not list answers the
  name as written (`unlisted_files`). The probe names are asked against the node, and a template with no model rows that a probe blocks is
  not counted as ready by `summarizeReadiness`. The real file system tests make their folders with `scratchDir` (they had turned
  `test-scratch.test.mjs` red).
- **Embedding lookup simplified (round six).** Four review rounds had made the prompt lookup a replica of ComfyUI's set-order choice on every
  file system, and every finding was on synthetic input: in the installed package (`json 0.1.96`) the catalog holds no embedding
  requirement and no probe. The lookup is now the narrow, fail-closed one above. Gone: the case-folding probe and the inventory's
  `case_insensitive_dirs`, every rule that depended on whether a volume folds case, the choice within one directory between extensions, and the
  statuses `ambiguous` and `shadowed` (and `case_mismatch` for a prompt embedding); the loader-widget rule is unchanged. Written red first (a
  commit of tests only): 8 tests of `render/templates-catalog.test.mjs` and 7 of `render/template-manifest.test.mjs` failed on the code before
  the change: a case variant, an exact file beside a case variant, two files in one or several directories, a file beside an annotated one
  that ComfyUI tries first, a probe hit (refused as `models_missing` before), a file that answers only the SECOND probe of a prompt (the
  earlier tests lacked it: a mutant that asked only the first probe passed all three test files), the inventory's case rule and probe, and
  the order of `models_missing` before the new class. The class list of the Go door passed at once. The two real-NTFS layouts (a
  case-sensitive root holding `Foo.pt` with a junction first in the walk to a folder that folds case; a root holding `EasyNeg.safetensors`
  beside a case-sensitive subfolder holding `Zed.pt`) are tests that skip where the per-directory case flag or the link cannot be made; their
  synthetic twins run everywhere. Removed, because they pinned the dropped behaviour: thirteen tests that asked a volume or a root to fold case
  (the inventory's case rule, the probe against a real volume, a case variant answered on a folding volume, two variants in two directories, a
  root with no letter in its top-level names, the swap of each letter, the folder that holds both spellings, the per-root case rule, the
  earlier candidate winning on a folding volume, a name with capitals), the one that pinned the extension order within a directory, two
  superseded by the new tests (a name held in several directories; an annotation met only when it is the file ComfyUI opens), and in the
  manifest tests the folding io and three superseded tests of the old statuses; the rest that named the old statuses were converted. Twenty
  mutations of the new code, one per scratch copy of `render/`, each fail at least one test: the exact-case compare lowered, no identity
  dedup, a case variant read as present, more than one file read as present, a probe hit ignored, only the first probe asked, probe findings
  not added, the whole-segment match loosened to a suffix, the case flag of a file under two names taken from the first, annotation names
  not checked, the two reasons swapped, `.bin` not tried, unlisted files not candidates, the class missing from `blocking`, the gate not
  refusing the class, the gate checking it before `models_missing`, unverified entries also listed as `models_missing`, probe hits missing
  from the list, the reason missing from the entries, and the class constant misspelled; one of them (the case flag of a file under two
  names) survived the first set of tests and has a test of its own.
- **Proof gaps found by the next read of the simplified lookup.** A one-token change of each of these left every test file green and gave a
  wrong `ready` or a wrong label, and each has tests now, shown red against the change in a scratch copy of `render/`: a probe loop capped at
  the first two or three probes (a prompt with four probes and a file that answers only the last, in the catalog and the manifest test
  files, with each position walked); the annotated file compared with the file that answers a name it covers, by identity (an annotation
  `foo.pt` that covers `foo`, with `foo.pt.safetensors` and `foo.bin` on the node: ComfyUI loads `foo.bin`); a walk that stopped following
  links for the embeddings class (a link inside the root: a different file of the name behind it is a second answer, and a name held only
  behind it is found; on an in-memory io that lists one entry as a link and on real files, the latter skipping where no link can be made);
  and
  the name reported for a file reached under several names (the first in path order, not the last). The readiness summary no longer counts an
  unverified embedding as a file short (see above).
- **Manifest-side normalisation tests.** `unlistedModels` lower-cases the manifest path, turns its backslashes into slashes and drops the
  `models/<class>/` prefix, and every test manifest was lower case with forward slashes, so no test could fail on that step. Both fixtures
  (`LISTED_MODELS` in `template-run.test.mjs`, `LISTED` in `comfy-run-graph.test.mjs`) now hold a mixed-case entry
  (`Qwen-Image-Lightning-8steps-V1.0.safetensors`) and a backslash entry under a mixed-case folder, with a graph that names each exactly,
  as four separate tests. Production was already right, so they pass; each was shown to bite by mutating the step in a scratch copy of
  `render/`: without the lower-casing 5 fail, without the slash normalisation 2 fail (both backslash ones), and no pre-existing test fails
  under either.
- **Not verified here.** The package the counts below were measured on, re-scanned for unexpanded subgraph instances (only the newer copy was).
  The runner's comparison against the converter's output for the real templates under a live ComfyUI: it is tested on injected graphs, on the
  two hand-written templates with their saved listing, and by unit, and the real package was only read by the catalog (a template the
  converter loads a file for that the catalog did not name would show as a `TEMPLATE_MODEL_UNLISTED` defer on a live run).
  An embedding loaded by a live ComfyUI: the directory set and the tokenizer path are read from ComfyUI's source (0.38.0) and compared with
  its extracted functions in Python, not run in a live ComfyUI, and no template of the installed package names one in an active prompt, so the
  catalog path has no real template to be measured on. The lookup on the real model tree of a node: the measuring machine's snapshot holds
  no embedding file. The junction layout of a model drive was reproduced with a real junction in a scratch tree, not on a node. The two
  real-NTFS tests (a root with the per-directory case flag) skip where the flag cannot be set; on the machine they were written on, setting it
  was refused (access denied), so they have not run on a real volume, and their synthetic twins did. A folder registered under two spellings
  on a virtual or cloud drive (one that keeps the case it is given, or derives its file index from the path) was read as two files by the
  earlier code of this branch on a drive of that kind; at this head it is refused for the same reason by reading the code, not by a run
  on such a drive. The probe names (the name as written, the reading without weights) were not run through a live
  encoder: ComfyUI's extracted functions give the names, and which text encoder a template uses is not asked.
  One template running end to end under a GPU lease, the acceptance measurement of this tool, queued after this
  change. The production wiring of the conversion against a live ComfyUI (the `/object_info` and `/system_stats` reads in
  `comfy-run-graph.mjs`; the flow and the converter are tested with injected values, and the converter on saved listings). The time of a
  first sighting hash on a 20 GB file. The Go door's production path on a box's real tree (tested on a fixture tree and a canned script). The template run's host-RAM
  declaration against a real template under a lease on a node (tested on stat tables and sparse files through the real admission).
- **Not built.** Fleet placement of a template; model downloads (a missing model is a refusal that lists it); an acknowledgement path for a
  conditional or non-commercial template; staging a template's sample input files; custom node packs; a CLI verb and a fleet task for the tool. Later, with its trigger: the full replica of ComfyUI's embedding
  lookup (its choice between the directories that hold a name, its extension preference and the case rule of the volume), taken when the first
  real template needs it.

### Fixed - the host-RAM rule holds a commit nobody stated to an enforced commit limit below physical RAM and refuses figures that are not numbers; the staged-file, route and decision-record checks pin what they say

- **A commit nobody stated, and a strict Linux limit.** A lease that declares one figure (the media admission, the seat warm-back, a bare `gpu reserve --ram N`)
  states no commit, so its commit is the touched figure standing in for one and was judged against physical RAM less the headroom on every reading. On a reading
  whose commit limit is enforced and sits below physical RAM (Linux with `vm.overcommit_memory=2` and little swap: the limit is about half the RAM plus the swap)
  that line lies further out than the one the kernel enforces, so such a lease was admitted past it while the verdict of the same reading was OVER. The bound of a
  commit nobody stated is now the nearer of the two, `min(physical RAM, commit limit)` less the headroom, when the limit is enforced. Windows (the limit is physical
  RAM plus the page file, so above it) and a limit the OS does not enforce keep the physical line. A commit above that bound is impossible and says which line.
- **The surfaces follow the line.** The refusal and the impossible sentence name the commit limit when it is the line that refused. `unstated_commit_room_gib`
  in the `host_memory` block is the room under the same line. The loud OVER lead (`gpu status`, the brief line of `offload_status`) said less than the note under it:
  when the verdict rests on the commit charge above physical RAM alone, it now adds that a lease that states its commit is judged on the commit limit instead (or
  not at all where the OS enforces none), which the rule admits; the block carries the same words as `stated_commit_note`. A reading at or near an enforced limit,
  or OVER on the available memory, adds nothing.
- **Figures that are not numbers.** `NaN` compares false against everything and Go's `max(NaN, 0)` is `NaN`, so a declaration of it fell through every test of the
  rule and was admitted (and `-Inf` was read as "declares nothing"). The rule now refuses a declared figure that is not finite as impossible, makes a lease wait
  when a figure it compares against (the reading, the headroom, what running leases have yet to load) is not finite, reads such a reading as `unknown`, and counts a
  `NaN` load as declaring host RAM so that it reaches the rule. The hidden `gpu hold` verb checks `--ram`, `--ram-commit`, `--ram-wait` and `--ram-wait-commit`
  itself, as `gpu reserve` does; a `-Inf` commit used to be held.
- **Wording.** The `--ram-commit` help and the operator guide said a stated commit is judged on Linux only with `vm.overcommit_memory=2`; a mode that cannot be read
  counts as strict, so it is judged there too, and both now say so. The lease guide read "fail at once the page file has reached its maximum" and reads "fail once".
  The scan that refuses retired wordings gained these.
- **The refusal for an unreadable lease record says what it rests on.** When the heartbeat of the holder cannot be told (a rename is installing it, or it can be
  neither read nor examined) no sign of life was seen: the holder is only not shown to be gone. The refusal claimed a heartbeat or a write within the heartbeat TTL
  and said the lease is held until the heartbeat goes stale. It now says the heartbeat cannot be told and that the hold ends when it can, when a staged one ages out,
  when the holder lets go, or at `gpu release`. A sign of life within the TTL keeps its sentence.
- **Pins on the staged-file handling, no behaviour change.**
  - A staged heartbeat with no live name beside an unreadable whole-node record counts as a heartbeat that cannot be told only while the staged file is younger than
    `stagingFresh`: just under the bound the record is held, just over it and at ten minutes it is debris and the node is free. The card-record twin does the same
    for `gpu release` with no epoch and for the listing of records that cannot be read. A staged heartbeat beside a live one that reads is not unknown.
  - Every rename of a lease file goes through `renameReplacing`; a direct `os.Rename` anywhere else in the package is named (the drift test read the
    `renameReplacing` call sites only, so a new writer with a direct rename went unseen).
  - The Node launch-marker writer stages under the name `stagingPath` gives a marker, checked from the Go side against its source and from the Node side against the
    names the readers look for, so a marker mid-rename cannot read as no marker and a dead wrapper's lease be reclaimed with its instance running.
- **The fleet node's route check covers a handler wrapped around the ingress guard.** The tables of the credential tests are complete by construction only if no
  handler answers for a path without registering it. The source test now also holds what `Handler` returns (the one return, the `netguard.IngressGuard` call
  around the mux) and what the node's `http.Server` serves (`s.Handler()` exactly, built by `httpServer`); a second `http.Server`, one with no handler, a handler
  around either, and `http.ListenAndServe` and its siblings are named.
- **A decision record.** The record for the overflow of a media call said the graph door is open to any peer. It carries the dated amendment, pointing at the
  decision for the fleet bearer, that every record calling a lane open carries, and the lint that requires the amendment now matches that wording too.
- **How it was proved.** Red first, with the failure lines in the commit messages: the admission rule at a limit below physical RAM (refusal, boundary,
  pending, impossible, and the Windows and unenforced controls), the room key and the OVER lead (`internal/gpucards`, `internal/mcpserver`), the non-finite figures
  (rule, verdict, hidden holder), the help and guide wording, the refusal sentence, the route check rows on the fixtures and on the node's real sources, and the
  amendment lint. The pins that pass on the code before this change (the staged-file bound, the card record, the staged heartbeat beside a live one, the direct
  rename, the marker writer, the stated-commit flag in the deferred-reserve split and in the card allocator) were each shown to survive a one-line edit that removes
  what they pin and to go red once they exist.
- **Not verified here.**
  - No lease was taken and no seat was loaded: every test runs against a stand-in host reading.
  - The strict Linux path has no live host: both Linux nodes read are in mode 0, so the new bound is exercised through the rule's inputs and the reader seam only.
  - The Windows-only staged-file behaviour (the rename gate on a real clock) ran on the local Windows host through targeted tests, not on a loaded runner.

### Fixed - the amd-gcn seed records the ACE-Step 1.5 licence as the MIT its source states instead of "not verified", and the licence map says what each ACE-Step repack states (register ACE-LICENCE)

The amd-gcn profile's notes said "ACE-STEP LICENCE IS NOT VERIFIED" and left the music lane's licence unset, a plan page assumed Apache-2.0, and the licence map tagged both Comfy-Org ACE-Step repacks `apache-2.0`
without saying how that relates to the model they repackage. Read at the sources on 2026-10-18:

- **The ACE-Step 1.5 project.** The `LICENSE` file of its GitHub repository is the MIT text ("Copyright (c) 2026 ACEStep"), and its README says the project is licensed under MIT.
- **The model repository `ACE-Step/Ace-Step1.5`.** The Hugging Face API returns `cardData.license` `mit` (tag `license:mit`), the card's own line reads "License: [MIT]", and the card says the generated music may be used commercially.
  The model repository holds no `LICENSE` file (the raw URL answers 404), so the card and the GitHub file are the sources.
- **The package the seed binds.** `audio-cpp/audio.cpp-gguf` is tagged `license:other` at the repository level, and its README table lists the original-model licence of `ACE-Step1.5-GGUF` (family `ace_step`) as MIT;
  audio.cpp's model licence reference lists `ace_step` as MIT with commercial use allowed.
- **The repacks the map names.** `Comfy-Org/ace_step_1.5_ComfyUI_files` states `apache-2.0` and names `ACE-Step/Ace-Step1.5` as its base model, so it states a different licence from the model it repackages.
  `Comfy-Org/ACE-Step_ComfyUI_repackaged` states `apache-2.0` and repackages the ACE-Step v1 checkpoint (`ACE-Step/ACE-Step-v1-3.5B`), whose card states `apache-2.0` too.

What changed:

- **`setup/templates/profiles.json` (amd-gcn notes) and the generated `docs/tiers/amd-gcn.md`.** The unverified clause is now the verified record: MIT, with the three sources above. The page is generated from the notes
  (`TestTierDocsAreCurrent`); the same text was written to both.
- **`render/templates-license-map.json`.** Each repack is tagged exactly as it states itself (`apache-2.0`, class `permissive`, unchanged), `fetched_on` moves to the re-fetch (2026-10-18), and a `note` on each says what its
  base model states; the 1.5 repack's note says the tag is the repo's statement and not the base model's. The top-level `evidence` text says the two entries were re-fetched.
- **`setup/install.ps1`.** The comment above the amd-gcn media pins no longer says the licence is unverified.
- **`internal/tierseed/amdgcn_media_seed_test.go`.** The assertion that the notes call the licence unverified now asserts the verified MIT record (and that the old clause is gone); it fails on the unmodified profile.

What did not change, and why: **no `commercial_use` key is seeded for the music lane.** The audio lanes (`audiocpp_*`, `musicgen_engine`, `voicegen_engine`) have no `license` or `commercial_use` key in the config schema, and the
seed test forbids a licence key on them, so the record lives in the notes. Adding a key would be a schema change with its own design (ADR 0058 tags image, edit and video families only); it is not part of this change.
The two repack tags stay `apache-2.0` because that is what each states. Not verified: the Hugging Face API and the raw files were read once, on one day; a later change of any of them is not seen until the entries are fetched again.

### Fixed - a structured re-pack that counts upward is read as a loop, and the retry is told not to repeat

- **What was wrong.** A seat writing a grammar-constrained re-pack can run out of real items and count: `"349", "350",
  "351", ...`. Every item differs from the one before, so neither the byte-level repeat test nor the line-block test in
  `degenerateTail` had a block to find, and the cut was filed as "the budget was too small". On a delegated run of
  2026-10-15 (a 9B seat, a 4,729-character answer) that sent the second request to the 8,192-token cap with the same
  sampling; the seat counted on to 1483 and that request spent 145 s of a 197 s re-pack, after which the attempt told
  not to repeat finished the object in 14 s.
- **What it does now.** `degenerateTail` reads one more shape, last, so every output the earlier tests named keeps its
  words: a run of numbers that rise by exactly one, with the same text between every pair, that spans at least
  128 bytes, has at least 12 numbers, and reaches the end of the output (only the start of the next item follows the
  last number). It is filed as `degenerate (counting up one at a time, N numbers from A to B)`, so the retry is the
  conditioned one (frequency penalty and the do-not-repeat sentence) at the budget the first request had, not an
  escalation to the cap. The 12 and the 128 are chosen, not measured. The limit is stated in the code: it counts by one,
  so a loop that counts by two, counts down, or carries other digits in each item is read as before.
- **Tests, red first.** `TestDegenerateTailReadsAMonotoneCountingLoop` (seven loop shapes: quoted numbers on one line cut
  inside an item, inside the separator and exactly after a number, across a power of ten, a label and a number on each
  line, bare numbers on each line, unquoted numbers; and eight spared shapes: three years closing a list, nine
  consecutive numbers, twenty-five years that close the list, a count followed by prose, a numbered list of distinct
  items, a count that restarts, numbers that jump, ordinary prose with digits) and
  `TestRunAgentTaskRepackAfterACountingLoopIsNotEscalatedToTheCap`
  (end to end against a fake seat, with a fixture that is first shown to be escalated when the loop is not recognised).
  On the unmodified code both failed: the seven loop shapes read `degenerateTail = ""`, and the second attempt was a
  plain request to the cap (`the attempt after a counting loop is not conditioned against repeating`). Six mutations of
  the detector were then run against the table (no end-of-output check, no same-text rule, no span floor, a count floor
  of 5 for 12, the cut item not dropped, a step other than one); two survived the first table (the same-text rule and the
  count floor) and three cases were added that fail under each (eleven numbered lines are spared and twelve are read, and
  a numbered list of distinct items that ends on three identical ones is spared), after which all six are caught.

### Changed - every tier that binds an agent model states its agent completion budget (D-24)

- **What changed.** `TestAReasoningSeatSeedsItsCompletionBudget` walked only the tiers whose vLLM seat declares a
  `reasoning_parser`; four more tiers that bind an agent model (`blackwell-32`, `blackwell-48`, `blackwell-72`,
  `ampere-6`) seeded no `agent_max_tokens` and inherited the loop's 1,024 without saying so. The new
  `TestEveryAgentSeatingTierStatesItsCompletionBudget` walks every tier that binds an agent model, in its seed or in a
  RAM overlay, and requires one of three statements: a seeded `agent_max_tokens`, `agent_thinking: "off"` (the
  exemption), or the new tier-level marker `agent_budget_policy: "loop-default-chosen"`. A tier whose vLLM seat declares
  a `reasoning_parser` may use neither the marker nor the exemption; the marker is refused beside a number, beside the
  exemption, on a tier with no agent model, and with any other value. `TestBudgetGuardRules` pins each rule on a
  synthetic tier.
- **The marker is chosen, not measured** (operator decision D8). The four tiers above carry it, their pages in
  `docs/tiers/` say so in a "Completion budget" paragraph (rendered by `internal/tierdocs`), and the field is
  documented in `profiles.json` `_fields` and in `docs/systems/setup-installer.md`. It is not a `config_seed` key and
  never reaches a node's `config.json`. A seat window that measures a budget replaces it with a seeded
  `agent_max_tokens`.
- **Rollout note.** The marker changes the four tiers' `profiles.json` entries, so a serving config rendered from the old
  entry reads `STALE(profiles_entry_sha256)` in `audit-yaml --against-render` until it is re-rendered.
- **Tests, red first.** On the unmodified code the widened guard named exactly the four tiers
  (`tier "blackwell-32" binds an agent model and states no completion budget`, and `-48`, `-72`, `ampere-6`), and
  `TestAMarkedTierPageSaysItsBudgetIsChosenNotMeasured` failed for each marked tier before the page rendered the note.

### Changed - `ledger` says a total that reaches back before 0.117.7 undercounts (D-96)

- Before 0.117.7 the ledger priced the tokens a local seat generated at zero (the estimated Opus value was the input
  rate only), so a dollar figure summed over older rows is an undercount. The history is not restated, by design, which
  makes the line under the figure the place to say it. `local-offload ledger` now prints, under `tokens kept local
  (est.)`, `note: rows written before 0.117.7 (2026-09-14) priced generated tokens at zero, so a total that reaches back
  before then undercounts; those rows are not restated` whenever the window can reach back before the fix: a lifetime
  read, or a `--since` long enough to cross 2026-09-16 (the cut-off is one UTC day after the release, chosen, because a
  ledger row does not record the build that wrote it). A `--since 1` read prints nothing extra.
- **Test, red first.** `TestLedgerSaysLifetimeTotalsUndercountRowsBeforeOutputWasPriced` failed on the unmodified code
  (`a lifetime ledger read does not carry the pricing caveat`).

### Changed - the docs say which instrument answers which question (D-73)

- `docs/systems/opencode-integration.md` (the instrument row) now says that the dispatch-log scoreboard measures the
  lead's effort and has the lead's own turns as its denominator, and that the share gate and every savings figure read
  the ledger (`origin_session`, `cards_tokens`) with tokens as the denominator, so the two are never compared as one
  number.

### Fixed - the card-only render runners refuse two calls up front and say why: an unkeyed runner under a two-card lease with no `--placement multicard` (PF-4), and a canvas above the 2512 Q5_1 recipe's pixel cap (PF-3)

Two misleading or missing refusals of the render runners, found in the media session of 2026-10-15 (registers PF-3 and PF-4). Both are in `render/`; no Go code changes.

- **PF-4, the defect.** A still run under `gpu reserve --devices <a>,<b>` without `--placement multicard`, aimed at a non-default ComfyUI endpoint (`--api` or `COMFY_API`) that nothing answered on, failed as
  `COMFY-ENDPOINT-DOWN`. That line tells the operator to start ComfyUI on the port or to key the instance (`COMFY_INSTANCE`, `COMFY_CARD_UUID`), and a card key pins ONE card, so it does not say what is missing:
  only `--placement multicard` keys the pair's own instance and launches it on its own port.
- **PF-4, the fix.** `ensureComfy` (`render/comfy-lifecycle.mjs`) answers `COMFY-PLACEMENT-MISSING`, which names the two leased cards (`GPU_LEASE_DEVICES`) and `--placement multicard`, when the lease holds exactly two cards, the
  call carries no multicard launch and the endpoint it was aimed at does not answer. Nothing else moves: an endpoint that answers is reused as before, one or three leased cards and a keyed instance keep their lines, the
  default endpoint launches as before, and nothing is launched in the refused case (it never was). The refusal still comes after the single probe of the endpoint, not before it, so a ComfyUI someone started there is reused.
  A caller that matched the old text on this case matches `COMFY-PLACEMENT-MISSING` now (nothing in the harness matches either).
- **PF-3, the defect.** The `qwen-image/gguf-q5_1` row of `render/multicard-recipes.json` (the 2512 Q5_1 GGUF across two cards, basis `predicted`) ran out of memory in the sampler on its first live run, at 928x1664
  (1.54 MP, on the storage card, 56 s in). The row held any canvas, so a call at that canvas, or at no size at all (the qwen-image default is 1328x1328, 1.76 MP), took both cards and failed late.
- **PF-3, the fix.** The row carries `maxPixels: 1472700` (1.47 MP) and `maxPixelsNote`; a new `canvasRefusal` (`render/multicard-recipes.mjs`) turns a canvas above it into a refusal that names the row, both sizes, the
  cap, that it is chosen, and the flags to change (`--width` and `--height`, or the single placement). It is enforced where the size is known: `buildRenderGraph` (`render/comfy-render.mjs`, which every path to a
  card-only render passes, one job of a `--batch` included, with the family default resolved) and, for a single `comfy-generate` call that names its canvas, `planImageMulticard` (`render/multicard-image.mjs`),
  before the lease's cards are read and before ComfyUI starts. The single placement is not held to the cap, the edit rows keep their `maxMegapixels`, and the row stays `predicted`.
- **The cap is chosen, not measured.** No canvas of this row has been measured on two cards. 1.47 MP is the largest canvas the row's evidence shows a same-size Q5_1 GGUF running on two cards (the 2511 edit, every weight on
  the storage card, 0.3 GiB free: "too thin for production"), and it sits below the canvas that failed. The OOM was on the storage card, which argues that the row's storage figure, not its activations, is what is wrong
  at that canvas. A measured arm at a larger canvas, or a re-split that fits 928x1664, replaces the cap; the existing default-size multicard call is refused until then (`--width`/`--height` below the cap).
- **Not done.** There is no refusal at the typed door (`internal/pipeline`, `internal/hostneed`): a request that names its size reaches the runner, which refuses it after the pair was leased. `render/README.md` and
  `docs/systems/media-generation.md` do not describe either refusal yet.
- **Tests (red first).** `render/comfy-unkeyed-api.test.mjs` (4 tests: the refusal names the placement and both cards; an endpoint that answers is reused; one or three cards, a keyed instance and a multicard launch are
  unchanged) and the new `render/multicard-canvas.test.mjs` (13 tests, 7 of the first 10 red on the unmodified code; the others are guards). Mutants of the fix that the tests catch: the launch exemption dropped, "exactly two" turned
  into "two or more", no pair at all, the cap check dropped from `buildRenderGraph`, from the planner and from `comfy-generate`, `<=` turned into `<`, the no-cap guard dropped, the batch given the shared canvas, and the
  non-number guard dropped. `render/multicard-builders.test.mjs` names a 1024x1024 canvas in the one test that built the multicard graph at the default size.

### Fixed - the vLLM seat launcher mounts the cache-server share at the store address with the lowest ping round-trip time when several of its addresses lie on the pinned network

Residual of register B-55, in `setup/templates/vllm-seat/seat_fg.sh` (shell rendered verbatim into the seat, so one file; no renderer and no installer changes). The previous B-55 fix mounted
the FIRST mDNS answer inside the pinned source address's IPv4 network. When the store host has two answers on the SAME network, a wired address and a Wi-Fi address, "first inside the network" is
arrival order, and the Wi-Fi path is about ten times slower for writes, so the cache tier degraded.

- **The fix.** With more than one distinct answer inside the pinned network, `l2_pick_address` measures each candidate's minimum round-trip time with
  `ping -I <source address> -c 5 -i 0.2 -q -W 1 <address>` (the first number after `=` on the `min/avg/max` line, so it reads iputils and busybox summaries) and mounts the lowest. Each ping runs under
  `timeout -s KILL 3`, one after the other, so the measurement takes at most 3 s per candidate and a start can never hang on it. Equal minimums go to the earlier answer. With exactly one answer inside the
  network nothing changes and no ping runs; an answer outside the network is never pinged, whatever its time would be.
- **Why the minimum.** Measured on the reference workstation with `ping -c 5 -i 0.2 -q`, three runs each: the wired address had a minimum RTT of 0.425-0.477 ms (average 0.57-0.62 ms), the Wi-Fi address a
  minimum of 0.983-2.288 ms (average 1.5-26 ms). The minimum separates them by more than 2x on every run; the average is noisy on the Wi-Fi path.
- **Fallback.** A candidate whose ping fails (no reply, cut off by the timeout) shows `no reply` and is never chosen over one that replied. When ping is missing (`not measured`), every ping fails, or no time
  can be read from its output (`RTT unreadable`), the choice is the first answer inside the network by arrival order, the rule before this change. The `seat_fg:` line says which rule chose and lists
  each candidate, for example `mDNS answers: A B C, inside N/P: A (min RTT 0.43 ms) B (min RTT 1.10 ms); chose A, the lowest round-trip time`; the fallback reads
  `chose A, the first inside the network in arrival order (round-trip times could not be compared: <why>)`, with three reasons (ping is not available on this box; no candidate replied to ping; no round-trip time
  could be read from ping's output), and a single replier reads `chose B, the only candidate that replied to ping`. A one-answer line is byte-identical to before.
- **Limit, stated.** The round-trip time is a heuristic for the better path, not a throughput measurement: a store whose fast path has a higher latency than its slow path would be picked wrong, and a store whose
  fast path drops ICMP loses to a slow path that answers it. `SEAT_L2_MOUNT_SRC` naming a reserved or static address (an IPv4 literal) or a name that is not `.local` still overrides everything: nothing is looked up
  and nothing is pinged. A share that is already mounted is still left alone (the address is chosen only when the share is mounted), so a start after this change does not remount a share an earlier start put on
  the Wi-Fi address; unmount it while no seat uses it and start the seat again.
- **Test seam.** `SEAT_FG_TEST_PING_CMD` names a command that stands in for ping (called as `<cmd> <source address> <address>`; it prints a minimum RTT in ms, or exits non-zero for no reply). It is documented in the
  script with the other two seams, is read nowhere else, and nothing in a rendered seat sets it.
- **Tests (red first).** `seat_fg.kv-and-store.tests.sh` gains 24 cases against two ping stand-ins (the seam, and a `ping` found on `PATH` that prints what iputils or busybox print; the real `ping` is never reached, and the
  case without ping runs on a `PATH` that has none): the second or the first or the last or the middle candidate lowest, times compared as numbers (12.5 above 9), a tie, a no-reply first, second or one of three, every
  ping failing, a ping command that does not exist, no ping on `PATH`, unreadable output, a hanging ping cut off at 3 s (both shapes), the exact ping command line and the minimum taken instead of the average
  (a candidate with minimum 0.45 and average 26.0 beats 1.00 and 1.20), a busybox summary, one answer inside the network (no ping, both shapes), answers outside the network never pinged, and `SEAT_L2_MOUNT_SRC` as an
  address literal or a non-`.local` name (no lookup, no ping). On `origin/main`'s launcher 20 of the 24 fail; the other 4 are guards that pass on both sides (the one-answer and the `SEAT_L2_MOUNT_SRC` cases), and all
  49 earlier cases pass on both. The existing case "the FIRST answer inside the network wins" keeps its assertions and is renamed to say what it now proves: with no round-trip time to compare, the first one wins.
  Mutants of the launcher, each caught: the highest time taken, a tie going to the later answer, no source address on ping, 3 probes instead of 5, the average read instead of the minimum, no timeout on ping, no timeout on
  the seam, every answer pinged and not only those inside the network, a failed ping counted as 0, the fallback taking the last answer, unreadable output and a missing ping both reported as no reply, a single in-network answer pinged too, the single-replier wording
  dropped. The Go wrappers (`launcher_kv_store_run_test.go`, `launcher_kv_store_pins_test.go`) name all 73 cases and pin the ping line as one whole command, its timeout, the strict comparison, the one-answer branch, the
  minimum read and that ping is started in exactly one place.
- **Docs.** `docs/OPERATOR-GUIDE.md` and `docs/systems/cache-server.md` describe the tie-break, the fallback and the limit in place of the old "cannot tell them apart" paragraph.
- **Not verified here.** No live store and no real network: every ping in the tests is a stand-in, and the figures above are the reference workstation's own measurement. The real `ping` output shape was read on a
  loopback address only.

### Fixed - the rendered Windows seat stubs serve every Windows seat of the tier, so a box with two Windows seats no longer needs a hand-made stub (register B-57)

`seat-cmd.ps1` and `seat-cmdstop.ps1` (`setup/templates/vllm-seat/windows-wsl/`) are one pair of files for every Windows seat on a box: each seat's llama-swap entry
runs the same file from the same seat directory and passes its own seat id. The render baked ONE seat's values into them, so the stub could not serve a second seat.

- **The defect.** `seat-cmd.ps1` waited for `__SEAT_ID__` (the model id the seat serves) in `/v1/models` on `__PORT__`, both filled in for the seat the render was run for.
  A second seat's `cmd` passes its own seat id to the same file, so its stub waited for the first seat's id for its whole load budget, then exited 4. A box with two Windows
  seats on one port therefore ran a hand-made copy of the stub, keyed by a map from the seat argument to the served id and exiting 5 on an unknown seat. That copy has none of
  the template's later fixes: the crash cleanup, the stop marker and the stop-result read.
- **The change.** Both stubs carry a table, one row per Windows seat: seat id (the argument the entry passes) to the id served, the port and the load budget (the seat's
  health-check timeout). `Spec.ArtifactsWith(templatesDir, runtime, seats)` renders it from the rendered seat plus the other Windows seats given; `Spec.Artifacts` is unchanged
  and renders a table of one. `install vllm-seat` passes the tier's `extra_vllm_seats` that start through WSL. The rows are sorted by seat id and do not depend on which seat the
  render was run for, so rendering the seats one after another writes byte-identical stubs and the last render drops nobody. The file's other behaviour is untouched: crash
  cleanup, stop marker, bounds, exit codes 2, 3 and 4.
- **A seat the table does not hold is refused.** The stub writes `unknown seat '<arg>': this stub serves <ids> - nothing started` to `seat-cmd.log` (a fixed name, because the
  argument is not trusted into a file name) and exits 5, before it starts the task or asks a port. `seat-cmdstop.ps1` does the same before it writes the stop marker or starts the
  stop task, so a misnamed entry cannot leave a marker that makes a later crash look like an unload. The render refuses, before reading anything from disk, another seat that
  starts through systemd, an id that is not a seat argument, a port out of range, an id given twice with different values, and any other seats for a seat that has no stub.
- **Deploying it.** Re-render and deploy BOTH stubs (they agree on the table and on the marker name). The seat argument must be the seat id the entry passes, as the rendered
  entries do; a hand-made entry that passes a short name now exits 5 instead of waiting for an id it was never given. `install vllm-seat` still writes the env file and the
  registration script of the lane seat only: an extra Windows seat's own are not rendered by it, and the note it prints for such a seat now says so instead of asking for a systemd unit,
  wrapper scripts and polkit rule that a Windows seat does not have (an extra seat that starts through systemd keeps its note).
- **Limit, stated.** The table holds the seats the tier declares. A tier cannot declare a seat on other cards than its lane seat (`extra_vllm_seats` are alternatives of the lane
  seat), so a seat of that kind on a box still needs its row added by hand until the tier can declare it. Not verified: the stubs under llama-swap as SYSTEM on a real Windows box.
  The PowerShell-driven tests run under whichever PowerShell the test host has and skip without one; the Linux gate runners used had none, so those tests skipped there and only
  the Go-only tests ran. The rendered stubs were instead run by hand, under Windows PowerShell 5.1 and PowerShell 7, with the same drivers.
- **Tests (red first).** `internal/vllmseat/seat_stub_seat_table_test.go` renders two seats on two ports with different load budgets and runs the RENDERED stubs under a PowerShell
  driver with a simulated clock: each seat argument probes its own port, waits for its own id (another seat's id on its port is not it) and keeps its own load budget; an unknown
  seat exits 5 with nothing started, nothing asked and no file named after it; the stop stub watches its own seat's port and writes only its own marker. A Go-only test pins the table
  rows in both files, byte-identical output whichever seat the render is for, the sort order, and the refusals. `install_vllmseat_wsl_siblings_test.go` runs the command over a tier with a
  second Windows seat and a systemd seat: the stubs hold the first two and not the third, and a one-seat tier's stubs hold one row. The existing crash-cleanup driver now renders the seat it
  runs the stub for (`pp3`) instead of passing a seat argument the stub was not rendered for. On `origin/main` the new Go-only tests fail (the root-package ones as they stand; the vllmseat ones against a shim that gives the new
  entry point main's behaviour). By hand, the stubs rendered from `origin/main` for one seat: a second seat probed the first seat's port and either came up on the first seat's id or waited
  out its load budget and exited 4, an unknown seat started a task and exited 0, and the stop stub wrote a marker for it; the new stubs do what the tests above assert. The 12 scenarios
  of the existing crash-cleanup driver (crash, unload marker, task ended early, a live launcher, every stop-task bound and failure, the three marker failures) give call logs identical to
  `origin/main`'s stubs, under both PowerShells and with the template's line endings either LF or CRLF.
