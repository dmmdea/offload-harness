---
status: Proposed
date: "2026-09-22"
---

# ADR 0059 — An external-CLI media tool runs CPU-class, pinned, env-scrubbed, and never via its cloud paths

## Context

HyperFrames (npm `hyperframes`, HeyGen, Apache-2.0) turns an HTML/CSS composition into
deterministic video. It serves each frame from a local file server, seeks headless Chrome one frame
at a time and pipes the frames through FFmpeg. It gives the harness something no lane had before:
designed, text-exact motion graphics. That covers title cards, lower thirds, kinetic type and alpha
overlays to lay over LTX b-roll, and each render can be regression-tested with `framemd5`. It is
also the first third-party CLI the harness runs as a media route, and its defaults assume an
interactive developer machine rather than a harness node. Every behaviour below was read in the
pinned 0.8.61 source:

- **Telemetry.** PostHog is on by default (`telemetry/client.ts`). The opt-outs are
  `HYPERFRAMES_NO_TELEMETRY` and `DO_NOT_TRACK`.
- **Update traffic.** Every command without `--json` fetches `registry.npmjs.org/hyperframes/latest`
  and runs a GitHub skills check (`cli.ts:265-294`). A global install then self-upgrades in a
  detached `npm install -g` (`utils/autoUpdate.ts`). `HYPERFRAMES_NO_UPDATE_CHECK=1` stops the
  install and the notices but **not** the registry fetch. Only `--json` in argv skips the whole
  block. The gates compare the value against exactly `"1"`.
- **Skills installer.** `init` always runs a skills installer. It clones `main` unpinned and writes
  into `~/.claude/skills` and every other agent's skills dir. The router skill claims to be the
  "mandatory entry point" for any video request.
- **Cloud keys.** The CLI loads `./.env` from the cwd on every command (`cli.ts:63-99`). `capture`
  prefers `OPENROUTER_API_KEY`, which the house reserves for Jev alone. `snapshot --describe` calls
  Gemini whenever a key is present.
- **No sandbox.** Chrome launches with `--no-sandbox` and site isolation disabled
  (`browserManager.ts:857-963`).
- **GPU by default.** The browser GPU default is `auto`, which lands on the display card. On
  Windows no flag selects the adapter.
- **Unpinned browser.** The CLI pins chrome-headless-shell 152.0.7977.30, but its own resolution
  prefers a newer build in `~/.cache/puppeteer`.
- **Fonts.** The compiler requests the Google Fonts CSS API, with the page's character set in the
  query, for any font family the page does not declare. This happens even for the fonts it embeds.

The harness already has three rules this lane touches:

- ADR 0001: local only; a failure defers and never falls back to the cloud.
- ADR 0026: a held media lease makes load-triggering text admissions wait.
- ADR 0023: media dispatch on the fleet is not token-gated.

## Decision

An external-CLI media tool enters the harness only through a **harness-owned runner** that pins it,
scrubs it and classifies it. HyperFrames is the first such tool; `render/compose-hyperframes.mjs` is
its runner.

1. **CPU-class.** The lane forces software GL (`--no-browser-gpu`,
   `PRODUCER_BROWSER_GPU_MODE=software`) and CPU encode, and refuses `--gpu`, `--browser-gpu` and
   `--docker`. So it takes no GPU lease and no `withGpuSlot`: a media lease would stall text seats
   (ADR 0026) for work that never touches a card. It is serialized in-process on its own compose
   slot, not `mediaSlot`, with the same bounded wait. It is exempt from the fleet concurrency cap,
   because it never touches the text endpoint the cap protects.
2. **Pinned.** The install is project-local, made with `npm ci --ignore-scripts` from a committed
   lockfile (`setup/hyperframes/`, exact version, integrity verified). `npm audit signatures`
   checks registry signatures and SLSA provenance, and its failure is fatal: an unverifiable install
   is never bound. The installer never runs `npm install -g`. The pinned Chrome is downloaded by
   `browser ensure` into the harness-owned home and bound explicitly (`hyperframes_browser_path`).
   Bumping the pin is a lockfile change that the fleet-update sweep carries.
3. **Env-scrubbed.** The CLI's environment is an allowlist: PATH, the Windows system variables,
   temp and home, and the application-data dirs. The harness sets telemetry, update, auto-install and
   skills off, and supplies the ffmpeg, ffprobe, browser and cache paths. No `*_API_KEY`, token,
   `NODE_OPTIONS` or `GPU_LEASE_*` can reach it. The Go side applies the same allowlist to the
   runner itself (`gpugen.Spec.EnvExact`). HOME is a harness-owned directory, so nothing the CLI
   writes can land in the operator's `~/.claude`. The cwd is a fresh, empty work dir, so no `.env`
   is ever loaded.
4. **Allowlisted and machine-readable.** Only `lint`, `check`, `render`, `snapshot`,
   `browser ensure|path` and `--version` run, and each carries `--json`. `snapshot` always carries
   `--describe false`. `init`, `skills`, `cloud`, `lambda`, `cloudrun`, `capture`, `upgrade` and
   `publish` are refused before any spawn.
5. **Gated and typed.** Every render runs `lint` (0 errors), then `check` (exit 0), then
   `render --batch` (one manifest row), then an ffprobe gate: codec, size, fps, duration, alpha for
   the alpha formats, and audio when the page carries `<audio>`. The returned fields are measured.
   Every failure is one typed class, and the caller gets `deferred:true`. The classes are
   `BAD_INPUT`, `LINT_ERRORS`, `CHECK_FAILED`, `RENDER_FAILED`, `BROWSER_MISSING`, `FFMPEG_MISSING`,
   `CLI_MISSING`, `SPAWN_EBUSY` (retried once, #4058), `DISK_HEADROOM` and `TIMEOUT`. Nothing falls
   back to the cloud.
6. **Trusted code.** Compositions run in an unsandboxed Chrome. `html` and `project_dir` are
   accepted from the local MCP and CLI doors, which are trusted callers like `run_graph`. The fleet
   door, which is not token-gated, accepts **only** the node's vetted templates, with typed and
   escaped variables. For the same reason it drops a caller's `out`, so a tailnet peer can never
   name the file the node writes or overwrites. The output goes to
   `<media_dir>/compose-<hash8>.<ext>`, which `/fleet/media` serves by bare name. Every fleet media
   task follows the same rule. Vetted templates declare their fonts locally and reference no URL,
   so a render is offline.

## Consequences

- The fleet gains a free, deterministic motion-graphics lane that runs beside every render and text
  seat. Measured on the reference box, a 150-frame 1080p card renders in 14-22 s, and the whole
  gated call takes 27-38 s. Repeated renders give identical frames, and so do 1-worker and
  multi-worker renders.
- The lane is only as fresh as its lockfile. HyperFrames ships near-daily, so the pin needs the
  sweep or it goes stale.
- A node needs Node >= 22. The installer skips the lane on older nodes, prints the version it found
  and leaves the route unbound. It does not ship a route that would be bound but missing.
- `html` and `project_dir` from local doors remain arbitrary code in an unsandboxed browser. This
  record accepts that for trusted callers, as `run_graph` accepts arbitrary graphs.
- Cross-machine pixel identity is not promised. Chrome and fonts differ per host, and only
  HyperFrames' Docker mode would promise it. Determinism holds per node.

## Alternatives considered

- **Take the media lease.** Rejected: it would park text-seat loads behind CPU work (ADR 0026). If a
  measured render ever shows real VRAM use, the lane moves to a lease with a `gpuLeaseCases` entry.
- **Call `npx hyperframes`, or install globally.** Rejected: `npx` resolves from the network on
  every call, and a global install self-upgrades in a detached process.
- **Use HyperFrames' Lambda or Cloud Run rendering for long jobs.** Rejected by ADR 0001. Chunked
  distributed rendering across the fleet (`planV2`/`renderChunkV2`) is a possible later step, once
  every node runs a byte-identical ffmpeg.
- **Install the upstream skills.** Rejected: they write into every agent's global skills dir from
  unpinned `main`. A curated house skill points at the harness tool instead.
- **Accept free-form HTML over the fleet behind the token.** Deferred until media dispatch is
  token-gated fleet-wide (ADR 0023).

## Related code

- [`render/compose-hyperframes.mjs`](../../../render/compose-hyperframes.mjs) — the runner and every guard above.
- [`render/compose-templates/`](../../../render/compose-templates/README.md) — the vetted templates and the shared font kit.
- [`internal/pipeline/composevideo.go`](../../../internal/pipeline/composevideo.go) — `runComposeVideo`: the compose slot, the runner allowlist and the typed defers.
- [`internal/gpugen/gpugen.go`](../../../internal/gpugen/gpugen.go) — `Spec.EnvExact`.
- [`internal/fleetnode/compose_task.go`](../../../internal/fleetnode/compose_task.go) — the template-only fleet task.
- [`internal/mediacap/mediacap.go`](../../../internal/mediacap/mediacap.go) — the `compose_video` route verdicts.
- [`setup/hyperframes/`](../../../setup/hyperframes/package.json), plus the Step 7b / 3b hyperframes steps in `setup/install.ps1` and `setup/install.sh`.

## Related docs

- [Media generation → Composition (HyperFrames)](../../systems/media-generation.md#composition-hyperframes)
- [ADR 0001 — defer, never cloud fallback](0001-defer-never-cloud-fallback.md)
- [ADR 0026 — text load admissions wait for the media lease](0026-text-load-admissions-wait-for-the-media-lease.md)
- [ADR 0023 — agent lane tailnet auth and locality](0023-agent-lane-tailnet-auth-and-locality.md)
- [Glossary: Composition](../../glossary.md)
