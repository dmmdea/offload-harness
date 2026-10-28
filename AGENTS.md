# AGENTS.md

Vendor-neutral entry point for any AI coding agent working in this repository. (Claude Code also
auto-loads `CLAUDE.md`, which carries the same orientation in more detail.)

## What this repo is

`offload-harness` — a local-first Go harness that delegates short-context, low-judgment work
(summarize / classify / extract / triage, plus vision / OCR / transcription / media generation) to a
free **Gemma-4 cascade** served by llama.cpp. It ships as a CLI, an **MCP server** (`local-offload`),
and an optional local **coding agent** (`local-agent`). It never calls a cloud model; on low
confidence it returns a structured **defer** so the calling agent does that task itself.

## Documentation map

Detailed documentation lives in `docs/`. This file only routes you there.

- [`docs/README.md`](docs/README.md) — the index. Start here.
- `docs/systems/` — how each part of the harness works (offload pipeline, coding agent, MCP server,
  media generation, fleet node, installer, accelerators).
- `docs/tiers/` — one page per hardware tier (GENERATED from `setup/templates/profiles.json` by
  `cmd/gentiers`; run `go generate ./...` after changing the profile table) plus checked-in
  capability reports from real machines.
- `docs/flows/` — behavior that crosses systems (cascade escalation and defer, run-graph manifest
  satisfaction, fleet job lifecycle, zero-warm generation).
- [`docs/systems/printed-clis.md`](docs/systems/printed-clis.md) — the `tools/` printed CLIs: what
  they are, why they are separate Go modules, and the regenerate/re-vendor cycle.
- [`docs/systems/cache-server.md`](docs/systems/cache-server.md) — the optional second-device KV tier
  (`kv_cache_server`): off by default, capacity at parity cost, the pipeline-layout constraint.
- [`docs/systems/composite-tier.md`](docs/systems/composite-tier.md) — a box that is several
  hardware tiers at once (ADR 0052): its device layers, the one placement table, the display-card
  guards that fail closed, and the `placed` block every result carries.
- [`docs/systems/fleet-overview.md`](docs/systems/fleet-overview.md) — `fleet-ui` / `top` /
  `fleet-smoke`: the read-only operator page on the delegator, its poller, and the jobs/errors feeds.
- [`docs/systems/node-swap.md`](docs/systems/node-swap.md) — `node-swap`: the reusable Windows
  fleet-node binary/render-tree swap tool (hash verify, idle wait, backup+rename, restart,
  verify, automatic rollback) and its detached, SSH-drop-proof launcher.
- [`docs/systems/browse-lane.md`](docs/systems/browse-lane.md) — the opt-in browse lane
  (`offload_browse` / agent `browse`): sidecar protocol, deny-list, doors, and what leaves the machine (ADR 0060).
- [`docs/systems/opencode-integration.md`](docs/systems/opencode-integration.md) — the harness inside
  opencode: MCP registration, rules parity, and the `opencode-local-offload` plugin.
- [`docs/systems/leak-gate.md`](docs/systems/leak-gate.md) — the privacy gate: write machine, person and
  brand names as node letters and role phrases ([`docs/STYLE.md`](docs/STYLE.md#privacy)), never the real
  name; the test fails the build on a hit and there is no allow list.
- [`docs/architecture/decisions/`](docs/architecture/decisions/README.md) — Architecture Decision
  Records. **Only `Accepted` status is current guidance.**
- [`docs/glossary.md`](docs/glossary.md) — terms with a specific meaning here (Defer, Tier,
  Zero-Warm, Node Manifest, …).

**Workflow:**

1. Read the system and flow docs for the area you are about to change.
2. Inspect the source for implementation detail.
3. Make the change.
4. Update the affected docs **in the same pull request** when behavior, responsibilities, flows,
   invariants, interfaces, or glossary concepts change.
5. Make sure docs and code agree before you finish. No test checks doc wording any more;
   you check meaning. If they disagree and you cannot resolve it, say so explicitly in the PR.

Do not put detailed system behavior in this file — it belongs in `docs/`.

## Installing the stack

Point yourself at **`setup/SETUP-AGENT.md`** and follow it exactly. It is an agent-executable
runbook: run `setup/detect.ps1` → `setup/install.ps1` → `setup/selftest.ps1` (Windows, PowerShell),
read each script's final JSON line, and branch on the decision tables. Do **not** install ROCm/CUDA or start the unauthenticated agent server beyond loopback without asking
the human. Versions are never pinned: the installers resolve the newest complete build from the publisher and verify its digest; floors live in `setup/floors.json` (ADR 0099).

## Operating the stack

- **Orientation map** (ports, model tiers, golden commands, do-not-break invariants, where things
  live): `CLAUDE.md` in the repo root.
- **Task walkthroughs** (start/stop the stack, chat, run an offload task, drive the coding agent,
  add a model, diagnose failures, update): `docs/OPERATOR-GUIDE.md`.

## Repo tooling (`tools/`)

`tools/` holds **printed CLIs** — standalone Go modules generated by CLI Printing Press and vendored
here. They are separate modules, so a root `go build ./...` / `go test ./...` does **not** cover
them; `cd` into the module.

- **Driving a local ComfyUI server — read [`tools/comfyui/SKILL.md`](tools/comfyui/SKILL.md) first.**
  It is the agent entry point for `comfyui-pp-cli`: submitting graphs, node schemas and COMBO
  options, model-visibility triage, honest run timing, output provenance, and replay. Prefer it over
  hand-rolled HTTP against the ComfyUI API. Note this is the *operator/agent* tool for ComfyUI — the
  harness's own render path is `docs/systems/media-generation.md`.
- **Inspecting or operating llama-swap — read
  [`tools/llamaswap/SKILL.md`](tools/llamaswap/SKILL.md) first.** It is the agent entry point for
  `llamaswap-pp-cli`: what is loaded, what a swap cost, seat and model bindings, config and its
  backup history, and measurement. Prefer it over curling the llama-swap API by hand. This is the
  *operator/agent* tool — the harness's own serving path is `docs/systems/offload-pipeline.md`.
- **Never hand-edit a generated file under `tools/`.** Fix it upstream in the Printing Press and
  re-vendor, or the next reprint silently drops your change. Full rules:
  [`docs/systems/printed-clis.md`](docs/systems/printed-clis.md).

## Working in the code

- Build: `go build ./...`  ·  Vet: `go vet ./...` — both must stay green. CI runs the test suite and is the
  gate (see "Testing rule" below).
- Go 1.26+. Keep changes scoped and minimal.
- Do not break the serving invariants or the agent safety defaults — see `CLAUDE.md` → "Invariants".

## Testing rule

The maintainer's rule for every change, by a person or an agent. It replaces the earlier red-first,
mutation-check, seam-test and review-round practice (superseded 2026-10-18).

- **New changes carry no new tests.** Do not write, add or extend a test of any kind (unit, regression,
  golden, fixture, lint, self-test, dry-run assertion harness, mutant, test plan), directly or through a
  subagent or workflow. The one exception: a change that cannot be seen working any other way gets the
  single smallest check that shows it works.
- **CI as it stands is the gate.** Do not run mutation checks, red-first proofs, coverage hunts,
  repeat-until-clean cycles, refute or skeptic panels, fresh-context reviews of a fix, or full-suite,
  multi-host or remote-gate runs beyond what CI already runs.
- **The check is one real run of the change** (the real command, the deployed binary, the live seat, the
  rendered file), run once and read back. Say in the pull request what you ran and what you saw.
- **Review is at most one pass, and only for credentials/security or data loss.** Fix only what a real
  input reaches; drop low-severity findings and findings that need an input no real system produces.
- **A test a change turns red** is fixed in the code if real behaviour broke; otherwise the test is edited or
  deleted. It is never replaced with a new one.
- Model evals, benchmarks, smokes and soaks run only when the maintainer orders that measurement, and then
  at the bare minimum.
- Existing tests, scripts and CI config stay as they are until the maintainer prunes them in a separate
  change. The security guards (the leak gate, the identity scrub, credential scans) are guards, not tests,
  and stay.
