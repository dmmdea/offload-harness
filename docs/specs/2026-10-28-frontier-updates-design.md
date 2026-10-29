# Frontier updates: no version pins, publisher integrity, generations with rollback

Status: DESIGN, 2026-10-28. Operator order of 2026-10-28 (verbatim, trimmed of expletives): "make sure the harness
is version unpinnable in the future ... make sure this pinning virus never comes back ... every single offload
harness component gets updated and gets redesigned for full flexibility in updatability and upgradeability ... if I
want to update the harness every 5 minutes that should not be a re-hash of anything ... it should always work no
matter what modern versions we are running ... no trace of the harness having any pins or outdated shit."

Standing rule (the operator's canonical memory record, 2026-08-23): every library, framework and dependency of the
harness is always updated to the latest version; never pinned into staleness; the engineer in the loop absorbs
update breakage instead of freezing versions.

## 1. What I understood, what I assumed

- Understood: the harness carries a pin store (`setup/install.ps1` `$PINNED`: llama.cpp b11490, llama-swap v236,
  sd.cpp master-1000, 31 model files with byte counts and sha256), runtime refusals that enforce exact versions
  (HyperFrames `PINNED_VERSION`, `mediacap` byte equality, node packs by commit), pinned dev tooling (OpenSpec,
  lockfiles, CI toolchain equality) and a written doctrine (held assets are never swapped, a table that is a
  snapshot of the frontier refreshed by a human on a cadence). The fleet-update sweep reports the engines as `manual` because
  the registry rows have no update command, so llama.cpp sat at b11669 to b11721 while the installer would still
  lay down b11490 on a fresh box. That is the defect.
- Understood: a FLOOR (the minimum build a seat needs, such as b11452 for `gemma-embedding2`) is not a pin. A
  PLACEMENT pin (`pin_reason`, which card a model sits on, ADR 0078) is not a version pin. Both stay.
- Assumed: "latest" means the newest published build that is complete and verifiable, not a curated release cut.
  This reverses the operator-prompted 2026-08-27 policy "take the blessed tag, never the newest nightly" (the b10435
  regression lesson). The measured gate with automatic rollback replaces that curation. The `release` channel stays
  available per node. Ruling R1 below; the final report states the reversal in one line so the operator can flip it.
- Assumed: integrity still matters. It moves from literals in the repo to what the publisher serves at install
  time: GitHub `assets[].digest`, Hugging Face `lfs.oid` and size, npm `dist.integrity`, Sigstore attestations where
  a project publishes them (llama.cpp does). Nothing hashed lives in the repository.
- Assumed: "every 5 minutes" is the operator running the update verb at will, or the fleet-update sweep. No new
  scheduler is created (house rule: no unattended schedulers without asking). A scheduled task is offered, not made.

## 2. Intent and the why

Keep every node of the fleet, and <node-g>, on the newest working build of every component with one command that is
safe to run at any time: it is a no-op when nothing changed, it never downgrades, it verifies what it downloads
against the publisher, it gates the candidate on a measured smoke, and it rolls back by itself when the candidate is
worse. The repository never needs an edit for an upstream release to reach a node.

## 3. Goals, non-goals, anti-goals

Goals
- No release tag, commit, byte count or digest literal for any dependency anywhere in the repository. Floors live in
  exactly one file (`setup/floors.json`) and a lint keeps every other place clean.
- `install.ps1` and `install.sh` resolve each component from the publisher at run time and verify with the publisher's
  digest. A fresh install lands on the newest complete build.
- `local-offload update` (new verb): per-component generations, an atomic switch, a health gate, automatic rollback,
  a node-local manifest, a 5-minute re-run that costs one conditional request.
- The fleet-update registry rows for llama.cpp, llama-swap, whisper.cpp, sd.cpp, ComfyUI and hyperframes carry an
  update command (the verb), so the sweep applies them; "latest" has one definition shared by both tools.
- Every live node and <node-g> upgraded and read back.
- Docs, ADRs, specs, tier notes, runbook, memory, plans and the wiki no longer state pinning as policy. Measured
  numbers stay as dated records.

Non-goals
- Replacing the fleet-update sweep (it is the fleet lever; the harness verb is what it calls).
- A per-tier second llama.cpp build (ADR 0081's `--llama-bin-eg2`): always-latest makes it unnecessary; the floor
  check stays.
- Rewriting dated measurement records, ADR bodies, CHANGELOG entries or `contracts/digest-*.json`.

Anti-goals (what would make this a failure even if it works)
- A node left on a broken build with no way back. Every switch keeps the previous generation.
- A silent downgrade (the old `SKIP`-by-literal re-extracted b11490 over a newer build).
- A new pin in disguise: a `toolchain go1.x.y` line, a lockfile, an `==` in a requirements file, a ceiling.
- A watchdog or scheduler nobody asked for.

## 4. Constraints

- CI stays offline: `installer-windows` and `build` download nothing today. The resolver gets a seam
  (`OFFLOAD_RESOLVE=offline` with a fixture) so `setup/render.tests.ps1`, which dot-sources `install.ps1`, loads
  without the network.
- GitHub API: 60 requests per hour unauthenticated. The resolver sends `gh auth token` (account via `OFFLOAD_UPDATE_GH_USER`) when `gh` is
  present, else no token, `per_page=10`, and caches ETags in `installed.json` so a repeat is a 304.
- The 3-card reference box's hand-kept `C:/llama-swap/llama-swap.yaml` names `C:/llama.cpp-b<N>` directly and must never be
  re-rendered (operator doctrine in its header). The ampere-6 reference box's `config.yaml` names `llamacpp-b<N>/bin` and sets
  `LD_LIBRARY_PATH` per block; it once ran a new exe on old libraries because that variable pointed elsewhere. Both
  need a one-time, backed-up repoint to a stable path; the Linux switch sets `LD_LIBRARY_PATH` to the exe's own
  directory.
- Maxwell nodes (sm_50 to sm_53) take the CUDA 12.x asset family; others may take 13.x. The CUDA major is a per-node
  capability floor, read from the driver, never a pin.
- whisper.cpp publishes binaries on `b` tags only (its `v1.9.5` has no assets); sd.cpp ships Windows CUDA 12 only;
  llama.cpp `/releases/latest` is a semver stub with one asset (`nightly-tag.txt`) and all binaries are on `b` tags.
- Disk: keep exactly current plus previous per component; delete rejected candidates at once.
- Tests are earned, not default: one lint test for the no-pins rule (this incident). Deleted tests are the ones whose
  contract was the pin itself (section 3 of the inventory).
- Three accounts never crossed; every `gh` write pinned to `dmmdea`; the repo is public: no real names, hostnames or
  local paths.

## 5. How it gets made

Platform: the existing Go harness (1.27) plus the PowerShell and bash installers; no new language. New Go package
`internal/update` built on `internal/nodeswap` (the proven swap, verify and rollback state machine) and
`internal/hfinstall`'s stage-verify-swap-prev precedent.

### 5.1 Resolution ("latest") per component

| component | source | rule | assets (Windows / Linux) | integrity |
|---|---|---|---|---|
| llama.cpp | GitHub releases list, `ggml-org/llama.cpp` | max `b<N>`, `draft=false`, every required asset present with a non-empty `digest` (a half-uploaded release steps back one) | `llama-b<N>-bin-win-{cuda-12.4,cuda-13.4,vulkan,cpu}-x64.zip` + matching `cudart-...` / `llama-b<N>-bin-ubuntu-{cuda-12.8,cuda-13.4,vulkan,x64}` | `assets[].digest`; `gh attestation verify` when `gh` is present (fail closed when present and failing) |
| llama.cpp `release` channel | `/releases/latest` body asset `nightly-tag.txt` | the b-tag it names | same | same |
| llama-swap | GitHub releases, `mostlygeek/llama-swap` | max `v<N>` | `llama-swap_<N>_windows_amd64.zip` / `linux_amd64.tar.gz` / `linux_arm64.tar.gz` | `assets[].digest`, cross-checked with `_checksums.txt` |
| whisper.cpp | GitHub releases, `ggml-org/whisper.cpp` | max `b<N>` with Windows binaries | `whisper-bin-win-cuda-12.4.0-x64.zip` or `whisper-cublas-12.4.0-bin-x64.zip` (both names), `whisper-bin-x64.zip` / `whisper-bin-ubuntu-x64.tar.gz`; Linux CUDA and Vulkan are a source build into `versions/<tag>` | `assets[].digest` |
| stable-diffusion.cpp | GitHub releases, `leejet/stable-diffusion.cpp` | max `master-<N>-<sha7>` by `<N>` | `sd-master-<sha7>-bin-win-{cuda12,vulkan,cpu}-x64.zip` + `cudart-sd-bin-win-cu12-x64.zip` / Linux Ubuntu zips (cpu, vulkan) | `assets[].digest` |
| HyperFrames | npm registry `dist-tags.latest` | newest | tarball | `dist.integrity` (npm verifies) |
| ComfyUI | git tags `v*` sorted by version | newest tag, checked out detached; the Windows portable from the newest release object | git / `.7z` | commit sha / `assets[].digest` |
| ComfyUI node packs | the allowlisted repositories in `render/node-packs.json` | HEAD of the default branch at install time, commit recorded; an installed pack is moved only by `update --component comfy-packs` (operator or sweep, under the GPU lease), never by a render; gated on `pip check` and `main.py --quick-test-for-ci`, rolled back in git and pip on failure | git | commit sha recorded (the git commit id is the content digest) |
| models (GGUF, safetensors, bin) | HF tree API `?recursive=true` at `main` | the file at `main`; re-fetched only when the remote `lfs.oid` differs from the recorded one | resolve URL | `lfs.oid` (sha256) + `size`, or `X-Linked-ETag` on the 302 |
| Timestral, Coral, RKNPU artifacts | HF `main` / GitHub HEAD | newest | files | `lfs.oid` / digest |
| OpenSpec CLI | npm `dist-tags.latest` | newest | tarball | `dist.integrity` |
| Go toolchain | `go.mod` `go` line | floor; `GOTOOLCHAIN=auto` downloads newer; no `toolchain` line | | sumdb |
| CI actions | `actions/*@v<major>` | upstream's own floating major channel (Ruling R10) | | |

Channel: `nightly` (default) or `release` per node in `config.json` (`update.channel`), only meaningful for llama.cpp.

### 5.2 Layout per node (same volume as `<home>`, never C:)

```
<home>/installed.json                  node-local truth; replaced atomically (tmp + rename)
<home>/<component>                     the LIVE path every rendered yaml and task already uses:
                                       Windows: a junction -> versions/<id>; Linux: a symlink
<home>/<component>-versions/<id>/      write-once after promotion; <id> = the upstream's own id
<home>/<component>-versions/.staging/<id>.<rand>/   download + extract + self-check; deleted on failure
<home>/<component>-versions/<id>.meta.json          source, asset, digest, attestation, size, installed_at, gate numbers
```

The live path keeps its name (`<home>/llama`, `<home>/sdcpp`, `<home>/whisper`, `<home>/hyperframes`,
`<home>/llama-swap`), so no rendered yaml, scheduled task or probe changes. The first `update` on a box that has a
real directory there migrates it: rename to `<component>-versions/<id>` (id from `installed.json`, else from
`--version`), then create the junction. Hand-kept yamls that name a build directory (the 3-card reference box, the ampere-6 reference box) get a
one-time repoint (`update --repoint <yaml>`): back up the file, replace every occurrence of the old build directory
with the live path, set `LD_LIBRARY_PATH` to the exe's own directory on Linux, reload llama-swap. Models keep their
flat `<home>/models/` layout; a re-fetched file is staged beside the old one and swapped by rename, the old one kept
as `<name>.prev` until the next promotion.

### 5.3 The update verb

`local-offload update [--component llama|llama-swap|whisper|sdcpp|hyperframes|comfyui|models|all] [--channel nightly|release]
[--dry-run] [--json] [--rollback <component>] [--repoint <yaml>] [--resolve <component>]`

1. Lock `<home>/update.lock` (busy: exit 0, say so).
2. Resolve (5.1) with a conditional request (`If-None-Match` from `installed.json`); 304 or candidate equals current
   or candidate in `rejected[]`: write `checked_at`, exit 0. This is the 5-minute no-op.
3. Floor check against `setup/floors.json` (embedded in the binary at build): a candidate under a floor is never
   selected; a floor only rises.
4. Download to `.staging`, verify size and digest, verify the attestation when one exists, extract, run the offline
   self-check (`--version`, `--list-devices` for GPU builds, the rendered yaml's flags against the staged
   `llama-server --help`: an unknown flag is a reject before any seat moves).
5. Gate under the GPU lease (`gpu reserve --wait 8h --drain --unload-seat`): start the staged build on a side port
   with the node's resident model, one completion, read decode and prefill tok/s and VRAM; compare with the
   previous generation measured back to back in the same window. Reject on: no load, wrong output, decode or prefill
   more than 20 percent slower, VRAM more than 10 percent higher. A lease timeout is "inconclusive, retry later", not
   a reject.
6. Promote: rename staging to `versions/<id>`, flip the junction or symlink, rotate `previous`, prune everything
   that is neither current nor previous, reload the consumer (llama-swap seats through its own unload and reload,
   never a kill), post-flip watch of the first real seat load.
7. Rollback on any failure after the flip: re-point to `previous`, mark `rejected{id, reason, numbers, ts}`, exit 1
   (loud). A rejected id is never retried; a newer id is a fresh candidate.
8. Write `installed.json` (schema 2: `components.<name>.{current, previous, rejected[], etag, checked_at, floor}`),
   print the generation table, and the ADR 0085 notice when a running MCP server still holds the old harness image.

### 5.4 Installers

`install.ps1` and `install.sh` lose `$PINNED`, `$LLAMA_TAG`, `$SWAP_TAG`, `HF_PIN` and every literal; they call the
same resolver (the Go binary once built, a PowerShell/bash twin for the bootstrap before Go exists), stage and
rename instead of extracting over the live directory, and SKIP a component only when the installed generation is at
or above the floor and healthy. A newer hand-placed build is kept, never downgraded. `Show-FrontierNote` goes; the
install prints the generation table instead.

### 5.5 Runtime refusals become floors or records

- HyperFrames: `PINNED_VERSION`, `assertPinnedInstall`, `MatchesPin`, `LockPin`, `Drift` go; the runner records the
  installed version and refuses only below the floor in `floors.json`. The chrome-headless-shell build is
  HyperFrames' own; the harness stops comparing it.
- `mediacap/modelsizes.go`: INCOMPLETE means the file is smaller than the size recorded at download time in
  `installed.json`, not different from a literal.
- node packs: admission by repository; the commit is recorded at install.
- the vendored llama-swap config schema: refreshed from upstream by `go generate`; `version drift` compares server
  to schema as a floor (server older than the schema is the only drift).
- OpenSpec: newest from npm with registry integrity; the admitted-version table in `security.md` becomes an
  admitted-source table.

### 5.6 The guard

One root test, `no_version_pins_test.go` (earned by this incident): fails when `setup/install.ps1`, `setup/install.sh`,
`install_*.go`, `render/*.mjs`, `render/*.json`, `setup/hyperframes/package.json`, `scripts/openspec*`,
`accelerators/**` (non-test, non-testdata) contain a release tag (`\bb\d{4,5}\b`, `master-\d+-[0-9a-f]{7}`,
`\bv\d{3}\b`, `releases/download/`), a 40- or 64-hex digest, an `==x.y` requirement, or an exact version string in a
dependency map. `setup/floors.json` is the one place a build number may appear. The repository `AGENTS.md` and
`CLAUDE.md` carry the rule in one sentence each, and the house `AGENTS.md` gains a hard-prohibition line.

### 5.7 Fleet side (node-services)

Registry rows for llama.cpp, llama.cpp-sidecar, llama.cpp-mtp, llama-swap, whisper.cpp, sd.cpp, comfyui, hyperframes
on every node get `update: <path>/local-offload update --component <x> --json` under the node's lease wrapper, and
`latest: <path>/local-offload update --resolve <x>` so the sweep and the verb agree. `Get-FuLlamaCppBlessed` is
replaced by the verb's resolver. The `offload-harness-stale` row is removed. The Windows llama-swap swap is the verb,
so no second script.

## 6. What is left to the builder

Exact PowerShell and bash shapes of the resolver, the junction migration code path, the side-port gate's model
choice (the tier's resident model), the attestation call's signer identity string, the `installed.json` schema-2
reader and its tolerance of schema 1, and the order of the doc rewrites.

## 7. Rulings

```
Ruling R1: "latest" = newest complete, verifiable published build (nightly channel); the blessed semver cut is the per-node "release" channel, not the default - the operator's 2026-10-28 order is bleeding edge, and the measured gate plus rollback replaces curation; this reverses the 2026-08-27 "blessed tag" policy and the report says so - cost if wrong: one config line per node flips the channel.
Ruling R2: integrity comes from the publisher at install time (GitHub digest, HF lfs.oid+size, npm integrity, attestation when published), never a literal in the repo - every upstream checked exposes one - cost if wrong: a hijacked publisher serves a matching digest; the gate and the kept previous generation are the backstop.
Ruling R3: floors are allowed and live in exactly one file (setup/floors.json); ceilings and exact pins are unrepresentable there - a seat may need a minimum build, never a maximum - cost if wrong: a floor set too high blocks a node until it is lowered.
Ruling R4: the live path keeps its current name and becomes a junction or symlink to a write-once generation directory - no rendered yaml, task or probe changes - cost if wrong: a box whose live path is a real directory needs the one-time migration the verb performs.
Ruling R5: hand-kept yamls that name a build directory (the 3-card and the ampere-6 reference boxes) get a one-time backed-up repoint by the verb, never a re-render - the 3-card reference box's header forbids re-rendering and the ampere-6 reference box's LD_LIBRARY_PATH must follow the exe - cost if wrong: a seat starts on the wrong libraries; the backup restores it.
Ruling R6: the gate is a side-port smoke measured back to back against the previous generation (20 percent throughput, 10 percent VRAM), under the GPU lease, never a 30-minute canary suite - a 5-minute cadence needs a minutes-long gate - cost if wrong: a subtle regression under the threshold ships; the previous generation is one pointer write away.
Ruling R7: keep exactly current plus previous; delete rejected candidates at once - the data drives are tight - cost if wrong: a rollback past one generation re-downloads.
Ruling R8: models are "latest" at HF main and re-fetched only when the published oid changes, with the old file kept as .prev until the next promotion - re-uploads are rare and multi-GB - cost if wrong: a bad re-upload costs one rollback.
Ruling R9: lockfiles (package-lock.json x2, uv.lock) and the OpenSpec sha go; npm and uv verify against their registries at install - a lockfile is a pin set - cost if wrong: a compromised new release installs; mitigations are --ignore-scripts and the gate.
Ruling R10: CI actions keep upstream's floating major tags, at the publisher's newest major (a govulncheck-job step fails when one is behind); setup-go takes go-version: stable with check-latest (amended 2026-10-28: go-version-file reads go.mod's `go` line as an exact version and held CI at the floor); node uses the current release; govulncheck@latest; the go.mod toolchain line goes - a floating major is the upstream's own channel, an exact toolchain line is a pin - cost if wrong: a breaking action major fails CI loudly.
Ruling R11: vLLM and LMCache patches are not pins but carry a re-base duty: the update path re-applies them on the newest version and fails loud when they do not apply, keeping the previous generation - the house absorbs update breakage - cost if wrong: a seat waits on a re-base.
Ruling R12: no scheduler; the verb is on-demand and the fleet-update sweep is the fleet lever; a scheduled task is offered in the report - house rule on unattended schedulers - cost if wrong: the operator runs it by hand or asks for the task.
Ruling R13: one new test, the no-pins lint - earned by this incident; every test whose contract was a pin is deleted, not rewritten - cost if wrong: a lint false positive on a legitimate number, fixed by moving it to floors.json.
Ruling R14: dated measurements, ADR bodies, CHANGELOG entries and contracts stay; policy sentences are rewritten; docs/tiers regenerate from profiles.json; the change ships as a changes/unreleased fragment - history is not falsified - cost if wrong: an old number reads like a pin to a hasty reader; the glossary entry defines floor, pin and placement pin.
Ruling R15: ships as four PRs (installers+policy+lint; update verb; runtime refusals and dev tooling; node-services rows), each with its own real run - 172 files cannot pass one gate - cost if wrong: a few days where the installer is unpinned but the verb is not yet there; the fleet-update sweep still cannot apply engines until PR B and D land.
Ruling R16: one security-pass on PR A and B - fetched binaries are executed - cost if wrong: none; it is read-only.
```
