# Frontier updates Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (Native) to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** remove every dependency version pin from offload-harness and replace it with run-time resolution from the publisher, publisher-side integrity, generations with a health gate and automatic rollback, a fleet-update hook, and a lint that keeps pins out.

**Architecture:** the installers resolve each component at run time; a new `local-offload update` verb (Go, `internal/update` on `internal/nodeswap`) owns generations and the switch; floors live in `setup/floors.json`; a root lint test guards the repo; node-services rows call the verb.

**Tech Stack:** Go 1.27 (floor in go.mod), PowerShell 7, bash, GitHub REST, Hugging Face tree API, npm registry.

**Spec:** `docs/specs/2026-10-28-frontier-updates-design.md` (Rulings R1 to R16 govern every task).

## Global Constraints

- No release tag, commit, byte count or digest literal for any dependency in the repository; floors only in `setup/floors.json`.
- CI downloads nothing: the resolver has an offline seam (`OFFLOAD_RESOLVE=offline` + fixture under `setup/testdata/`).
- Never downgrade; stage and rename, never extract over a live directory.
- No new tests beyond `no_version_pins_test.go`; delete tests whose contract was a pin.
- `CHANGELOG.md` and `contracts/digest-*.json` are never hand-edited; the change ships as `changes/unreleased/<slug>.md`.
- Public repo: no real names, hostnames or local paths.

---

## PR A: installers, floors, policy, lint (branch `feat/frontier-updates`)

### Task A1: `install.ps1` resolves from the publisher

**Files:** Modify `setup/install.ps1`; Create `setup/floors.json`, `setup/testdata/resolve-offline.json`; Modify `setup/tests/install-config-seed.test.ps1`, `setup/render.tests.ps1` (fixture names only), `setup/tests/install-cuda-build.test.ps1`.

**Interfaces:**
- Produces `$SOURCES` (replaces `$PINNED`): a hashtable keyed by the same component keys (`llama-cuda`, `llama-swap`, `model-e2b`, ...). Each value: `kind` (`github-release` | `hf-file`), `repo`, for github-release `asset` (a pattern with `<TAG>`, e.g. `llama-<TAG>-bin-win-cuda-12.4-x64.zip`) and `tagRule` (`b-number` | `v-number` | `master-number`), for hf-file `file` (path inside the repo), plus `dest`. No `sha`, `bytes`, `version`, `url` fields.
- Produces `Resolve-Source -Key <k>` returning `@{ tag; url; digest; size }` from the publisher (GitHub releases list `per_page=10` with `b`/`v`/`master` max rule, required-assets-complete check, `assets[].digest`; HF `api/models/<repo>/tree/main?recursive=true` `lfs.oid`+`size`). Token: `gh auth token --user dmmdea` when `gh` is on PATH, else none. `OFFLOAD_RESOLVE=offline` reads `setup/testdata/resolve-offline.json` instead (CI and `render.tests.ps1`).
- Produces `setup/floors.json`: `{ "llama.cpp": { "harness": 11452, "seats": { "embeddinggemma2": 11452, "qwen3.6-35b-a3b-agent": 10964, "mimo-9b-agent": 11102, "lfm3": 11938 } }, "whisper.cpp": { "harness": "1.8.3" }, "stable-diffusion.cpp": { "harness": 968 }, "hyperframes": { "harness": "0.8.0" }, "node": { "harness": "22" } }` with a `_comment` line stating floors only, no ceilings.
- `installed.json` gains per component `{ version, digest, size, source, installed_at }` (schema 2; readers tolerate schema 1).

- [ ] Step 1: Replace `$LLAMA_TAG`, `$SWAP_TAG`, `$PINNED` and the header comment block with `$SOURCES` and a short header stating the frontier rule.
- [ ] Step 2: Implement `Resolve-Source`, `Get-GitHubLatestTag`, `Get-HfFile` with the offline seam; `Install-Zip` stages into `<dest>-versions/.staging/<tag>.<rand>` then renames to `<dest>-versions/<tag>` and junctions `<dest>` to it (migrating an existing real directory first).
- [ ] Step 3: SKIP rule: a component is skipped when `installed.json` records a version at or above the floor and the live path exists; it is never downgraded; `-Update` re-resolves and installs a newer tag.
- [ ] Step 4: Delete `Show-FrontierNote`; print the generation table (component, installed, resolved, action).
- [ ] Step 5: Fix the PowerShell tests to the new shape (names only, no sizes or shas); keep the b-number fixture directory names in `render.tests.ps1`.
- [ ] Step 6: Run `pwsh -NoProfile -File setup/render.tests.ps1` and `pwsh -NoProfile -File setup/tests/install-config-seed.test.ps1` with `OFFLOAD_RESOLVE=offline`; expected ALL PASS. Then `OFFLOAD_RESOLVE=` unset: `pwsh -NoProfile -Command '. setup/install.ps1 -RenderOnly; Resolve-Source -Key llama-cuda'` prints the newest complete b-tag with a `sha256:` digest.

### Task A2: `install.sh` twin

**Files:** Modify `setup/install.sh`, `setup/install.tests.sh`.

- [ ] Step 1: Remove `HF_PIN` and every literal; add `resolve_source` (curl + jq; same rules and offline seam, fixture shared with A1) for the HyperFrames npm `dist-tags.latest` and for any GitHub or HF asset the script fetches; stage and rename; write `installed.json` schema 2 via jq merge.
- [ ] Step 2: Run `bash setup/install.tests.sh` (offline) and `OFFLOAD_RESOLVE= bash -c 'source setup/install.sh --lib; resolve_source hyperframes'`; expected the newest npm version and its `sha512-` integrity.

### Task A3: Go side of the pin store and the lint

**Files:** Modify `internal/mediacap/modelsizes.go`, `internal/mediacap/modelbindings_test.go`; Delete `internal/servingtmpl/eg2bin_test.go` and `internal/servingtmpl/testdata/eg2bin-unset-render.golden`; Modify `install_render_eg2bin_test.go` (delete `TestInstallPs1PinsABuildAtOrAboveTheEG2Floor`), `internal/tierseed/amdgcn_media_seed_test.go`, `install_gatedmodels_contract_test.go`; Create `no_version_pins_test.go`.

- [ ] Step 1: `modelsizes.go`: drop the literal size map; `KnownSize(name)` reads the size recorded in `installed.json` (schema 2) and returns ok=false when absent; INCOMPLETE only when the file is smaller than a recorded size.
- [ ] Step 2: Delete the eg2bin golden test and golden; delete the install.ps1 tag-parsing test; the amdgcn seed test asserts each seeded weight has a `$SOURCES` row (name only).
- [ ] Step 3: `no_version_pins_test.go`: scan `setup/install.ps1`, `setup/install.sh`, `setup/floors.json` excluded; patterns `\bb\d{4,5}\b`, `master-\d+-[0-9a-f]{7}`, `\bv\d{3}\b`, `releases/download/`, `[0-9a-f]{64}`, `[0-9a-f]{40}`; report file:line; scope widens in PR C.
- [ ] Step 4: Run `go test . ./internal/mediacap/ ./internal/servingtmpl/ ./internal/tierseed/`; expected ok, and the lint passes on the new `install.ps1`.

### Task A4: ADR, docs, runbook, profiles, fragment

**Files:** Create `docs/architecture/decisions/0099-frontier-updates-floors-never-pins.md`, `changes/unreleased/feat-frontier-updates-no-version-pins.md`; Modify `docs/architecture/decisions/README.md` (0099 Accepted; 0081 Superseded by 0099), `docs/architecture/decisions/0081-*.md` (status line), `setup/SETUP-AGENT.md` (Hard rule 2 and the two SHA rows), `docs/systems/setup-installer.md`, `docs/systems/security.md`, `docs/systems/media-generation.md`, `docs/systems/openspec.md`, `docs/systems/mcp-server.md`, `docs/systems/offload-pipeline.md`, `docs/systems/accelerators.md`, `docs/systems/coding-agent.md`, `docs/OPERATOR-GUIDE.md`, `docs/README.md`, `README.md`, `CONTRIBUTING.md`, `docs/glossary.md`, `docs/flows/run-graph-manifest-satisfaction.md`, `AGENTS.md`, `CLAUDE.md`, `docs/specs/2026-10-28-maxwell-3-tier-design.md` (R-ENGINE), `docs/superpowers/plans/2026-10-28-maxwell-3-tier.md`, `setup/templates/profiles.json` (notes wording) then `go generate ./...` for `docs/tiers/*.md`, `skill/local-offload-setup/SKILL.md`, `render/README.md`.

- [ ] Step 1: Write ADR 0099 (Context: the pin store and the fleet drift; Decision: floors never pins, publisher integrity, generations, the verb, the lint; Consequences; Supersedes 0081's second-build doctrine and the runbook rule that a held asset is never swapped for a newer build).
- [ ] Step 2: Rewrite every `stale-doc` entry under `docs/`, `setup/SETUP-AGENT.md`, root docs and `profiles.json` from `pin-inventory.json` (class `stale-doc`, plus the policy sentences in `version-pin` entries that are prose): measured numbers stay as dated records; "pinned" becomes "installed" or "the build measured"; the SHA/size mismatch rows become "integrity mismatch against the publisher: re-run; recurs: STOP".
- [ ] Step 3: Glossary: Floor, Version pin (forbidden), Placement pin (ADR 0078, unrelated), Generation.
- [ ] Step 4: `go generate ./...`; `go test -run 'TestTierDocsAreCurrent|TestTrackedTreeCarriesNoOperatorIdentity|TestTheChangelogTreeRulesHoldOnTheRealTree' .`; expected ok. a case-insensitive search of `docs`, `setup/SETUP-AGENT.md`, `setup/templates/profiles.json`, the root docs, `skill` and `render/README.md` for the old pin wording (the held-assets and pin-bump doctrine phrases) returns only placement-pin, rule-statement and dated-record lines.

### Task A5: real run, ship

- [ ] Step 1: Build `go build ./...`; `go run ./cmd/changelog check`; gates once.
- [ ] Step 2: Real run on <node-g> (installed b11490): copy the branch, `pwsh setup/install.ps1 -Update`, read back: llama moved to the newest complete b-tag, `installed.json` schema 2, `llama` is a junction, `llama-versions/` holds b11490 and the new tag, selftest pass, one `vqa` lane answer.
- [ ] Step 3: clean-ship: Jev tier verdict, one security-pass, PR, CI, merge, cleanup.

## PR B: the update verb (`feat/update-verb`)

### Task B1: `internal/update` on `internal/nodeswap`
**Files:** Create `internal/update/{resolve.go,generations.go,gate.go,update.go}`, `update_cmd.go`; Modify `internal/nodeswap/nodeswap.go` (Plan: artifact file-or-dir + consumer + gate), `installed.json` readers (`internal/fleetnode/gpuinfo.go`), `main.go` (verb), `docs/systems/node-swap.md`, `docs/systems/setup-installer.md` (update section), `setup/floors.json` embed (`//go:embed`).
- [ ] Steps: resolver (shared rules with A1, Go implementation is the reference; the PowerShell twin stays for bootstrap), conditional GET with ETag in `installed.json`, floors embed, staging + digest + attestation (`gh attestation verify` when available), offline self-check incl. rendered-yaml flags vs `llama-server --help`, gate on a side port under `gpu reserve`, promote (rename + junction/symlink flip + rotate + prune), consumer reload through llama-swap, rollback, `--repoint <yaml>` (backup + replace build-dir occurrences + Linux `LD_LIBRARY_PATH` = exe dir), `--resolve`, `--dry-run`, `--json`, `--rollback`.
- [ ] Real run: on the ampere-8 laptop node (`D:\offload-stack`, llama at the installed build): `local-offload update --component llama` moves to the newest tag with the gate numbers printed; a second run within a minute prints `no change (304)`; `--rollback llama` returns to the previous generation; read back `installed.json` and `llama-swap` serving.

## PR C: runtime refusals and dev tooling (`feat/no-runtime-pins`)
- HyperFrames chain (`render/compose-hyperframes.mjs`, `internal/hfinstall`, `internal/mediacap/mediacap.go`, `install_hyperframes.go`, `setup/hyperframes/package.json` `latest`, delete `package-lock.json`, `install.sh` HF step), `render/node-packs.json` + `.mjs` + `internal/mediacap/nodepacks.go` (repo allowlist, commit recorded), `install_forecast.go` (HF main), `accelerators/*` (resolve at fetch), OpenSpec (`scripts/openspec-install.sh` npm latest + integrity, delete lock, `openspec_tree_test.go`), `setup/browse` (delete `uv.lock`, `>=` floors), CI (`go-version-file` + `check-latest`, node current, `govulncheck@latest`; delete `gomod_toolchain_pin_test.go`, `ci_toolchain_proof_step_test.go`; go.mod toolchain line), vendored llama-swap schema via `go generate`, widen the lint to `render/`, `install_*.go`, `scripts/openspec*`, `accelerators/**`, `setup/hyperframes/package.json`.
- Real run: one `compose_video` render through the newest HyperFrames; `go test ./...` in CI.

## PR D: node-services (`feat/harness-update-rows`, worktree `<worktrees>/node-services-harness-update`)
- Registry rows (`fleet/fleet-nodes.json`): `update` and `latest` through the verb for llama.cpp, llama.cpp-sidecar, llama.cpp-mtp, llama-swap, whisper.cpp, sd.cpp, comfyui, hyperframes on every node; remove `offload-harness-stale`; `FleetUpdate.psm1`: `Get-FuLlamaCppBlessed` replaced by the verb's `--resolve`; skill docs (`skills/fleet-update/SKILL.md` lines that describe the blessed target and the frozen-seat exception).
- Real run: `pwsh fleet/fleet-update.ps1 -Only <node-b>` report shows the llama.cpp row with an update command and the same resolved tag the verb prints.

## Fleet upgrade (after B and D)
- `fleet/fleet-update.ps1 -Apply -TimeBudgetMin 60` detached, harness rows only (Ruling in the ledger), then <node-g> by hand (`local-offload update`), read back per node: build, llama-swap version, selftest or one seat load, `installed.json` generations.
```
Ruling: executor = Native with one lean-builder per disjoint unit (A1..A4 in parallel, then A5) - four units share no files; the tests that parse install.ps1 are briefed with A1's $SOURCES shape - cost if wrong: a merge conflict inside one worktree, resolved by hand.
```
