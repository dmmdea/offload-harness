# SETUP-AGENT.md — install the offload-harness stack (agent runbook)

**You are the installer.** This document is written for an AI coding agent (Claude Code or
equivalent) driving a Windows machine on the user's behalf. It is a **decision tree keyed to the
JSON that the setup scripts emit** — read each script's final JSON line, match it against the
tables here, and take the exact next action. Do not improvise. When a rule says STOP, stop and ask
the human.

## What you are installing

A local-first grunt-work harness: a **Gemma-4 model cascade** served by llama.cpp, plus a Go
**offload CLI/MCP** (`local-offload`) and an optional local **coding agent** (`local-agent`). It
runs entirely on this machine and holds no cloud credentials. Three processes, three ports:

| Component | Port | Role |
|---|---|---|
| llama-swap (serves llama.cpp) | `127.0.0.1:11436` | Model server the harness talks to (`/v1/*`). Always on. |
| `local-agent --serve` | `127.0.0.1:18800` | OPTIONAL OpenAI-compatible coding-agent endpoint. OFF by default. |
| OpenWebUI | `127.0.0.1:8081` | OPTIONAL chat GUI in front of the agent. OFF by default. |

The install is **three scripts, in order**: `detect.ps1` → `install.ps1` → `selftest.ps1`. Each
prints human-readable progress and then **one machine-readable JSON line as its last stdout line** —
that line is your input signal.

## Hard rules (read before running anything)

1. **Verify, do not infer.** Parse the actual JSON each script prints. Never assume a step
   succeeded — read its verdict.
2. **NO SUBSTITUTION.** The installer pins exact release tags and model SHA256s. If a pinned asset
   404s, the hash mismatches, or a download fails after 3 retries, the script fails loud. **Do not
   substitute a different model, a different llama.cpp build, or an unpinned "latest" asset. Stop
   and surface the failure to the human.**
3. **STOP-AND-ASK-THE-HUMAN** before any of:
   - **Updating the GPU driver** (AMD Adrenalin / NVIDIA) — you must never do this unattended.
   - **Rebooting** the machine.
   - **The ~7–20 GB model download** if the link may be metered/capped — get consent first
     (`OFFLOAD_WITH_FAMILY=1` pulls ~21 GB of GGUFs; `=0` pulls ~4.5 GB).
   - **Any deviation from the pins** (see rule 2).
4. **FORBIDDEN detours (research-verified dead ends — do not attempt):**
   - **ROCm / HIP on an AMD iGPU (gfx1103, e.g. Radeon 780M).** AMD ships no gfx1103 compute
     kernels; llama.cpp does not target it; the Vulkan backend is both supported and faster on this
     arch. The `HSA_OVERRIDE_GFX_VERSION` spoof is Linux-only and unreliable. Use the **Vulkan**
     backend (`detect.ps1` selects it automatically for AMD).
   - **WSL2 for AMD iGPU inference.** AMD's ROCm-on-WSL support matrix lists discrete GPUs only; an
     iGPU cannot be accelerated through WSL2. Inference on AMD must be **native Windows**.

---

## Step 0 — detect hardware

```powershell
pwsh -NoProfile -File setup\detect.ps1     # or: powershell.exe -NoProfile -File setup\detect.ps1
```

The last stdout line is JSON. Fields: `os`, `gpu_vendor` (`amd|nvidia|none`), `gpu_name`,
`gpu_arch` (`blackwell|ampere|ada|volta|rdna3|gcn|other|none`), `gpu_count`, `vram_dedicated_gb`,
`ram_gb`, `ram_tier` (`high|mid|low|min`), `disk_free_gb`, `backend` (`vulkan|cuda|cpu`),
**`profile`** (one of the arch-class ids in the matrix below), `big_ram`, `warnings[]`.

The **`profile`** is the load-bearing new field: `install.ps1` keys the serving template + params
(context, KV type, 26B placement) off it, and `selftest.ps1` measures against it. It is chosen from
vendor + arch + VRAM band + GPU count + RAM tier (a heterogeneous NVIDIA pair → `dual-gpu`; RAM ≥
~56 GB unlocks the 26B via `--cpu-moe`). An unrecognized NVIDIA card falls back to an `ampere-*` band
by VRAM and **warns** — verify the template fits before relying on it.

Expected shapes:

```json
// NVIDIA box (3070 laptop → profile ampere-8)
{"os":"windows","gpu_vendor":"nvidia","gpu_name":"NVIDIA GeForce RTX 3070 ...","gpu_arch":"ampere","backend":"cuda","profile":"ampere-8","ram_tier":"low", ...}
// Blackwell box (5060 Ti → profile blackwell-16 — see the Blackwell note below)
{"os":"windows","gpu_vendor":"nvidia","gpu_name":"NVIDIA GeForce RTX 5060 Ti","gpu_arch":"blackwell","backend":"cuda","profile":"blackwell-16", ...}
// AMD box (RDNA3 iGPU — this is the intended AMD path)
{"os":"windows","gpu_vendor":"amd","gpu_name":"AMD Radeon(TM) 780M Graphics","gpu_arch":"rdna3","backend":"vulkan","profile":"amd-rdna3","warnings":["AMD path uses the llama.cpp VULKAN backend ...","Keep the AMD Adrenalin driver CURRENT ..."]}
// No GPU
{"os":"windows","gpu_vendor":"none","backend":"cpu","profile":"cpu", ...}
```

**Decision:**

| Signal | Action |
|---|---|
| exit 0, `backend` + `profile` present | Proceed to Step 1. Remember `backend` and `profile`. |
| `profile` is `blackwell-16` or `blackwell-8` | **Blackwell (sm_120).** Detect the installed CUDA and pick the build accordingly — see the [Blackwell note](#blackwell-sm_120--detect-the-installed-cuda-and-adapt-be-flexible) below. CUDA 13.x serves now (slower prefill); CUDA 12.8 is peak; the old 12.4 prebuilt won't run. Do not hard-require one version. |
| exit 1 + stderr `Only NGB free ... need >=25GB` | **STOP.** `disk_free_gb < 25` is a hard blocker. Ask the human to free disk on the install-target drive, then re-run. |
| exit 1 + stderr `targets Windows` | Wrong OS. This runbook is Windows-only. Stop. |
| `warnings[]` contains a `RAM ...< 32GB` note | Note it. You will set `OFFLOAD_WITH_FAMILY=0` in Step 1 (E4B workhorse only). |
| `warnings[]` contains `unrecognized NVIDIA GPU ... arch=other` | The card matched no arch regex and was banded into `ampere-*` by VRAM. Note it; verify the served template fits before relying on it. |
| `gpu_vendor:"amd"` warnings about ROCm/Adrenalin | Informational. Do NOT act on the driver note yourself — see Hard rule 3. |

`vram_dedicated_gb` reads small on an iGPU (a BIOS carve-out); that is **expected** — Vulkan uses
shared system memory, not the carve-out. Do not treat a small iGPU VRAM number as a blocker.

---

## The hardware-profile matrix (what each profile serves)

`detect.ps1` emits one `profile`; `install.ps1` renders the serving template from it. These are the
projected per-profile serving choices — `selftest.ps1` measures and refines them on the real box
(trust the receipt's `profile_measure.tuned` over this table when they differ).

| Profile | Config(s) | Resident/default tier | Ctx | KV | 26B-A4B |
|---|---|---|---|---|---|
| `blackwell-16` | #1 (5060 Ti 16 GB) | `gemma4-26b-a4b` | 32K | q8_0 | full-GPU — CUDA-13 serves (slower); 12.8 for peak |
| `volta-16` | #2 (V100 16 GB) | `gemma4-26b-a4b` | 32K | q8_0 | full-GPU |
| `ampere-16` | 3090-class ≥12 GB (defensive) | `gemma4-26b-a4b` | 32K | q8_0 | full-GPU |
| `dual-gpu` | #3/#4 (5060 Ti + V100 32 GB) | 26B architect + E4B editor, both resident | 32K | q8_0 | resident, **zero-swap** two-tier |
| `ampere-8` | #5/#6 (3070 8 GB; +64 GB) | `offload-e4b` | 16K | q8_0 | `--cpu-moe` if RAM ≥ ~56 GB, else dropped |
| `blackwell-8` | #8/#9 (5060 8 GB; +64 GB) | `offload-e4b` | 16K | q8_0 | `--cpu-moe` if RAM ≥ ~56 GB; CUDA-13 serves, 12.8 peak |
| `amd-rdna3` | #7 (780M + 64 GB, Vulkan) | `offload-e4b` | 16K | f16 (safe) | `--cpu-moe` very slow, else dropped |
| `ampere-6` | #10/#11 (3050 6 GB) | `gemma4-e2b` | 16K | q8_0 (**mandatory**) | dropped |
| `amd-gcn` | #12 (Vega 7 + 32 GB, Vulkan) | `gemma4-e2b` | 8K | f16, FA off | dropped |
| `cpu` | no GPU | `offload-e4b` (CPU) | 8K | f16, FA off | `--cpu-moe` if RAM ≥ ~56 GB, else dropped |

### Blackwell (sm_120) — detect the installed CUDA and adapt (be flexible)

**If `detect.ps1` reports `gpu_arch:"blackwell"` (RTX 50-series: 5060 / 5060 Ti / 5090 → profile
`blackwell-16` / `blackwell-8`), the right llama.cpp build depends on the CUDA version actually
installed on the box.** Do NOT hard-require one version — detect what's present and pick the best
build for it. Three cases:

- **CUDA 13.x installed (e.g. 13.3):** the card **DOES serve** on a CUDA-13 build — it just falls
  back from the Blackwell MMQ kernel to a cuBLAS path that is roughly **5.6× slower on prefill** for
  Q4 (measured elsewhere: ~772 vs ~5611 t/s pp512; token generation is far less affected). This is a
  **functional, ship-it-now state** — good enough to run, not peak. Use the CUDA-13 build and record
  the perf caveat; do not treat 13.x as broken.
- **CUDA 12.8 installed:** the **peak** path — a build with `-DCMAKE_CUDA_ARCHITECTURES=120` uses the
  MMQ integer kernels for full Q4 prefill speed. Recommend/install this when you want peak throughput.
- **CUDA 12.4 (the old stock prebuilt) only:** has **no sm_120** — Blackwell won't run on it at all.
  This is the one case that requires action (install a CUDA-13 or 12.8 build).

**Flexibility is the requirement.** The workstation is transitional — a 5060 Ti may run alone on
CUDA 13.3 today and gain a V100 tomorrow — so the installer must key the build off the *detected*
CUDA version + the *detected* GPU set, not a fixed assumption:
- **Single 5060 Ti on 13.3:** CUDA-13 build, serves now (cuBLAS-fallback perf note); offer 12.8 for peak.
- **Dual `5060 Ti + V100`:** a **multi-arch** build covering **both** `sm_120` (Blackwell) and
  `sm_70` (Volta), compiled against whatever CUDA (≥12.8 or 13.x) is installed. The dual-gpu profile
  pins 26B→device 0 / E4B→device 1; heterogeneous-arch multi-GPU is supported but confirm per-device
  placement.

Runtime env for Blackwell: `CUDA_VISIBLE_DEVICES` set explicitly (avoid the hybrid-graphics
`-1` trap), `CUDA_MODULE_LOADING=LAZY`. Standard Q4_K GGUFs get **no** FP4 tensor-core speedup
(NVFP4 MMQ only helps NVFP4-format models) — do not expect an FP4 win. **Report the detected CUDA
version + which build you selected + the expected perf tier to the human; get consent before
installing a new CUDA toolkit or a driver (Hard rule 3).**

---

## Step 1 — install

```powershell
# Optional env overrides (set BEFORE running):
#   $env:OFFLOAD_HOME='C:\Users\<you>\offload-stack'   # default: $HOME\offload-stack
#   $env:OFFLOAD_WITH_FAMILY='0'                        # RAM<32GB or to skip E2B+26B (~4.5GB vs ~21GB)
#   $env:OFFLOAD_BACKEND='vulkan'                       # override detect (cuda|vulkan|cpu)
pwsh -NoProfile -File setup\install.ps1
```

`install.ps1` runs `detect.ps1` itself, then installs idempotently. Every step prints `DO` /
`OK` / `SKIP`. **Duration:** the model pull dominates — expect **several minutes to ~30 min** on a
normal link for the full family (~21 GB), less for `OFFLOAD_WITH_FAMILY=0`. Progress lines print at
≤60 s intervals (bytes/percent), so a quiet gap is not a hang.

**Profile-driven serving (H2).** Install reads the detected `profile` + `ram_tier` and renders the
llama-swap yaml from `setup/templates/profiles.json` — substituting the profile's `--ctx-size`, KV
type, flash-attn, and 26B MoE placement (`gpu` / `--cpu-moe` / dropped) into the backend template
(`dual-gpu` renders the `win-dual-cuda` two-model-resident template). It drops the 26B entirely when
`ram_tier` is `low`/`min` (no RAM path for `--cpu-moe`). The install step prints the resolved
`profile | ram_tier | ctx | kv | 26b` and writes `profile`, `ram_tier`, `big_ram`, and
`agent_ctx_tokens` into `installed.json`. Run the agent with `-model <resident tier>` and
`-ctx-tokens <agent_ctx_tokens>` — install prints the exact command. An unknown/absent profile falls
back to the backend template's baked-in defaults.

Last stdout line: `{"installed":true,"backend":"...","home":"...","next":"run selftest.ps1"}`.

**What it lays down** under `$OFFLOAD_HOME` (default `$HOME\offload-stack`):
`llama\llama-server.exe`, `llama-swap\llama-swap.exe`, `models\*.gguf`, `harness\local-offload.exe`,
`harness\local-agent.exe`, rendered `llama-swap.yaml`, `installed.json` (version manifest),
`install.log` (full transcript). The harness config is copied to `$HOME\.local-offload\config.json`
(not overwritten if present — prints SKIP).

**Idempotency & re-run:** re-running is safe. A satisfied step prints `SKIP`. A component only
SKIPs when **both** the artifact exists on disk **and** `installed.json` records the currently
pinned version — so bumping a pin forces exactly that component to refresh. There is **no
`OFFLOAD_PLAN` dry-run mode** in this installer (see "Preview" below).

**Decision table:**

| Signal (in the transcript / final JSON) | Cause | Action |
|---|---|---|
| final `{"installed":true,...}` | success | Proceed to Step 2. |
| `git missing and winget unavailable` / `Go >=1.26 missing and winget unavailable` | no package manager | **STOP.** Ask the human to install Git / Go 1.26+ manually, then re-run. |
| `Go still <1.26 after winget install + PATH refresh` | stale Go | Report to human; a reboot may be needed to fully refresh PATH — **ask before rebooting**. |
| `SHA256 mismatch` / `size mismatch` | corrupt or wrong asset | Do NOT substitute. The script already retried 3×. Delete the named `.part`/component dir and re-run once; if it recurs, **STOP** and surface (the pin may be stale — a human decision). |
| `download failed after 3 attempts` (HTTP 429 / network) | rate-limited or offline | Wait and re-run (idempotent — completed files SKIP). Repeated 429 from Hugging Face → **STOP**, ask the human. |
| `template not found` | repo tree incomplete | Ensure the full repo is checked out; re-run. |

**Preview without downloading (there is no dry-run flag):** to see the plan first, read the
`$PINNED` table at the top of `setup\install.ps1` (URLs + sizes) and the model list. A partial
install can be resumed safely — completed components SKIP on the next run.

---

## Step 2 — selftest (the integrity gate)

```powershell
$env:OFFLOAD_HOME='C:\Users\<you>\offload-stack'   # same value you installed to; omit for default
pwsh -NoProfile -File setup\selftest.ps1
```

This stands up a **transient** llama-swap on test port `18801` (and the agent on `18802`), exercises
each installed tier through the real swap group, runs a **deep-context canary** at depth ~7000, and
prints ONE **receipt JSON** as the last line. Both transient ports are freed on every exit path.

Receipt schema (real fields):

```json
{"schema":1,"backend":"cuda","gpu":"NVIDIA GeForce RTX 3070 ...","driver_version":"32.0.15.xxxx",
 "tiers":[{"id":"offload-e4b","cold_load_s":6.2,"tok_s":81.4,"status":"pass"}],
 "canary":{"depth":7000,"status":"pass","detail":"generated N tokens at depth ~7000 ..."},
 "remediations":[],"harness_smoke":"ok","agent_smoke":"ok",
 "profile_measure":{"profile":"ampere-8","profile_src":"installed.json","ram_tier":"low",
   "projected":{"ctx_size":16384,"kv_type":"q8_0","moe_26b":"cpu_moe","resident_tier":"offload-e4b"},
   "ctx":{"projected_ctx":16384,"measured_ctx":16384,"measured_ctx_ok":true,"downshifted":false,"src":"measured"},
   "moe26b":{"applicable":false,"src":"skipped"},"cold_swap":[{"tier":"offload-e4b","cold_swap_s":6.2}],
   "tuned":{"ctx_size":16384,"kv_type":"q8_0","source":"measured"}},
 "verdict":"pass","proves":[...],"does_not_prove":[...]}
```

**`profile_measure` (H3 — measured-vs-projected).** selftest reads the active `profile` (from
`installed.json`), pulls the PROJECTED serving params (`profiles.json`), then MEASURES on THIS box:
does the projected ctx actually load without OOM (`ctx.measured_ctx_ok`, halving + retry if it
OOMs → `downshifted:true`); the 26B `--cpu-moe` decode tok/s where applicable (`moe26b`); per-tier
cold-swap latency (`cold_swap[]`); and whether q8_0 KV held. The **`tuned`** block is the payload:
`source:"measured"` means at least one value was measured on-device and should be applied OVER the
projected profile; `source:"projected"` means nothing on this host could measure it (recorded
honestly, never faked). Dual-GPU / Optane checks are marked `not-applicable-on-this-host` /
`measure-on-target` on a single-GPU box.

**Verdict decision table:**

| `verdict` | Meaning | Action |
|---|---|---|
| `pass` | Every tier live, canary clean, harness+agent smoke ok. | Done — proceed to Step 3. |
| `warn` | Installed and usable, but a soft signal fired: a tier is CPU-class slow (`tok_s < 8`), the canary did not pass, the 26B tier failed even after remediation, or the harness `summarize` **deferred**. | Read the receipt. A `harness_smoke:"defer"` on a tiny sample is **designed behavior, not a failure** — proceed. A `canary.status:"fail"` → see the canary row below. |
| `fail` | `harness_smoke:"fail"` OR `agent_smoke:"fail"` OR **all** tiers failed. | Do not proceed. Diagnose per the table below. |

**Per-field reads:**

| Receipt field/value | Meaning → action |
|---|---|
| `tiers[].status:"pass"` | Tier cold-loads and generates. Good. |
| `tiers[].status:"warn"` (`tok_s < 8`) | Throughput is CPU-class. Expected on `backend:"cpu"`; on a GPU backend it hints the model fell back to CPU — check the canary/remediations. |
| `tiers[].status:"fail"` on `gemma4-26b-a4b` | The MoE tier OOM'd. selftest **auto-remediated** (added `--cpu-moe`, restarted, retried once) — see `remediations[]`. If still failed: the message says try a lower `-ngl` or update the AMD Adrenalin driver → **STOP and ask the human** re the driver. |
| `remediations[]` non-empty | selftest edited your `llama-swap.yaml` (added `--cpu-moe` to the 26B cmd). This is persisted and correct — keep it. |
| `canary.status:"fail"` (device-lost / connection drop) | The deep-context Vulkan crash class (llama.cpp #17432). The detail says: update the AMD Adrenalin driver; if it persists switch 26b/e4b to `--cpu-moe` / lower `-ngl`. **STOP and ask the human to update the driver.** |
| `canary.status:"fail"` (empty completion) | Model returned nothing at depth. Re-run selftest once; if it recurs treat as a model/serving fault and surface. |
| `harness_smoke:"fail"` | The grammar pipeline did not return parseable JSON. This is a **fail** verdict — the endpoint or a serving flag is wrong. Run `harness\local-offload.exe --config <tmp> doctor` (see Troubleshooting). |
| `agent_smoke:"fail"` | The agent server did not answer `/v1/models` on 18802. Check the agent built (`harness\local-agent.exe` exists) and the port is free. |
| exit code | 0 for `pass`/`warn`, 1 for `fail`. |

**A `fail` right after an interrupted install** usually means a corrupt unzip. **Delete only the
named component directory** under `$OFFLOAD_HOME` (e.g. `llama\`) and re-run `install.ps1` — targeted,
not a full wipe.

**AMD throughput expectations (do NOT chase ROCm):** on a Radeon 780M via Vulkan, expect the
780M ≈ **19–25 t/s** token-generation on the workhorse tier (community-measured; token generation is
memory-bandwidth-bound). NVIDIA RTX 3070 ≈ 70–83 t/s. A tier landing at ~20 t/s on AMD is **normal
and correct** — it is not a defect and is not fixed by ROCm (which does not work here anyway).

---

## Step 3 — start the stack + register the MCP

Start llama-swap as the user's long-running service (loopback-only):

```powershell
& "$env:OFFLOAD_HOME\llama-swap\llama-swap.exe" --config "$env:OFFLOAD_HOME\llama-swap.yaml" --listen 127.0.0.1:11436
```

Register the offload harness as an MCP server for the agent:

```powershell
claude mcp add local-offload -- "$env:OFFLOAD_HOME\harness\local-offload.exe" mcp
```

Confirm health once llama-swap is up:

```powershell
& "$env:OFFLOAD_HOME\harness\local-offload.exe" --config "$HOME\.local-offload\config.json" doctor
# expect: "health:     OK" and each configured alias -> OK
```

### Optional: the coding agent + chat GUI (OFF by default)

The `local-agent --serve` endpoint is **unauthenticated** and drives write/GitHub tools, so it is
**loopback-only by default** and **not started by the install**. Only bring it up if the user wants
the chat-driven coding agent. See `docs/OPERATOR-GUIDE.md` → "Drive the coding agent" and "Chat GUI".

**MANDATORY HANDOFF if you enable the agent:** relay the "Prompting rules of thumb" from
`docs/OPERATOR-GUIDE.md` → *Context-budget guidance* to the user, verbatim, before their first
prompt. The planner is a small ~32K-context local model: **one bounded task per message**, **never
paste long documents into the chat** (use `local-offload summarize <file>` for that), **≤3
tool-kinds per ask**. An oversized prompt fails one run with `agent error: chat 400 … context` —
nothing breaks, they just narrow the ask and resend — but relaying these rules up front is the
difference between a good first impression and a confused user.

- **Token handling:** GitHub tools read `$GITHUB_TOKEN` / `$GITHUB_REPO` from the environment. Put
  them in a **gitignored env file** (`$HOME\.local-agent-github.env`), never in the repo, never on a
  command line that gets logged. Use a **least-privilege** token (only the scopes the task needs).
- **Loopback guard:** the server refuses any non-loopback `--listen`. To expose it on a trusted LAN
  you must pass `--listen-trusted-network` explicitly (it prints a loud warning). Do not do this
  unless the human asked.

---

## Troubleshooting table

| Symptom | Cause | Fix |
|---|---|---|
| `detect.ps1` exits 1, `need >=25GB` | disk full on target drive | Free space or set `$env:OFFLOAD_HOME` to a bigger drive; re-run. |
| `install.ps1`: `winget unavailable` | no package manager | Human installs Git + Go 1.26+ manually; re-run. |
| `install.ps1`: SHA/size mismatch | corrupt/wrong asset | Do not substitute. Delete the `.part` / component dir, re-run once; recurs → STOP. |
| HF download 429 / timeout | rate-limited | Re-run (idempotent); persistent → STOP, ask human. |
| `selftest` canary `fail`, device-lost | old AMD Adrenalin (deep-context crash #17432) | STOP → ask human to update the AMD driver; interim: `--cpu-moe` / lower `-ngl`. |
| `selftest` port `18801`/`18802` in use | a prior run leaked a process | Kill stray `llama-server.exe` / `local-agent.exe`, or reboot after asking; re-run. |
| `doctor`: `health: DOWN` | llama-swap not running / wrong port | Start llama-swap (Step 3); confirm `--listen 127.0.0.1:11436`. |
| `doctor`: an alias `FAIL — not in the live roster` | model not served / wrong yaml | Check `llama-swap.yaml` lists that alias; some non-core aliases (vision/stt) are absent on a grunt-work-only install — expected. |
| every offload call returns `deferred:true` | endpoint unreachable, or genuinely low-confidence/over-long input | `doctor` first; a defer on hard/over-long input is **by design**. |
| `local-agent --serve` errors `refusing to bind` | you passed a non-loopback `--listen` | Use `127.0.0.1`; only add `--listen-trusted-network` if the human authorized LAN exposure. |
| `go build` in install fails | Go < 1.26 or PATH not refreshed | Ensure Go 1.26+; a reboot fully refreshes PATH (ask first). |

## Uninstall / retry / logs

- **Logs:** `$OFFLOAD_HOME\install.log` (full install transcript). selftest writes transient logs to
  `%TEMP%\offload-selftest-*` and cleans them up.
- **Version manifest:** `$OFFLOAD_HOME\installed.json` (llama.cpp tag, llama-swap version, model
  list + SHA256s). A component re-installs only when a pin changes.
- **Hash sentinels:** each model has a `<file>.sha-ok` sentinel beside it caching its verified hash;
  delete it to force a re-hash.
- **Retry a component:** delete its directory under `$OFFLOAD_HOME` (e.g. `llama\`, `llama-swap\`,
  or a single `models\*.gguf`) and re-run `install.ps1` — only the missing piece re-downloads.
- **Full uninstall:** stop llama-swap, `claude mcp remove local-offload`, delete `$OFFLOAD_HOME` and
  `$HOME\.local-offload\`.
