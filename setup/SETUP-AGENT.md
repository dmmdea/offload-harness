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

**Fleet node (optional, post-install):** `local-offload fleet-serve` joins this box to the
fleet-dispatcher fleet — an unauthenticated HTTP server (default `127.0.0.1:18811`; production
binds the **Tailscale** address behind `--listen-trusted-network`, never `0.0.0.0`) that accepts
dispatched GPU renders through the same pipeline. Measured per-model VRAM footprints live at
`~/.local-offload/footprints.json` (prime a fresh box with `local-offload fleet-measure`). **MSI
Afterburner is the recommended companion** on a fleet box — its per-process VRAM display (which
nvidia-smi cannot provide under WDDM) validates our recorded footprints and doubles as a live
monitor; recommended, never required. Full guide: `docs/FLEET-NODE.md`.

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
**`profile`** (one of the arch-class ids in the matrix below), `big_ram`, `amd_adrenalin` (the
Radeon Software version read from the registry; `null` off-AMD or unreadable), `warnings[]`.

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
// AMD box (RDNA3 iGPU — this is the intended AMD path; amd_adrenalin is READ and classified:
// a warning appears only for the known crash class <= 25.11.1 or an unreadable version.
// A discrete RDNA3 card with >=12GB dedicated VRAM bands to profile amd-rdna3-dgpu instead.)
{"os":"windows","gpu_vendor":"amd","gpu_name":"AMD Radeon(TM) 780M Graphics","gpu_arch":"rdna3","backend":"vulkan","profile":"amd-rdna3","amd_adrenalin":"26.3.2","warnings":["AMD path uses the llama.cpp VULKAN backend ..."]}
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
| `blackwell-72` | #15 (RTX PRO 5000 72 GB; PRO 6000 96 GB) | **ALL-RESIDENT** (whole roster hot, no swap group, no ttl) | 128K | **f16** | full-GPU, resident |
| `blackwell-48` | #14 (RTX PRO 5000 48 GB) | **ALL-RESIDENT** | 128K | **f16** | full-GPU, resident |
| `blackwell-32` | #13 (RTX 5090 / RTX PRO 4500, 32 GB) | **ALL-RESIDENT** | 64K | q8_0 | full-GPU, resident |
| `blackwell-16` | #1 (5060 Ti 16 GB) | `gemma4-26b-a4b` | 32K | q8_0 | full-GPU — CUDA-13 serves (slower); 12.8 for peak |
| `volta-16` | #2 (V100 16 GB) | `gemma4-26b-a4b` | 32K | q8_0 | full-GPU |
| `ampere-16` | 3090-class ≥12 GB (defensive) | `gemma4-26b-a4b` | 32K | q8_0 | full-GPU |
| `dual-gpu` | #3/#4 (5060 Ti + V100 32 GB) | 26B architect + E4B editor, both resident | 32K | q8_0 | resident, **zero-swap** two-tier |
| `ampere-8` | #5/#6 (3070 8 GB; +64 GB) | `offload-e4b` | 16K | q8_0 | `--cpu-moe` if RAM ≥ ~56 GB, else dropped |
| `blackwell-8` | #8/#9 (5060 8 GB; +64 GB) | `offload-e4b` | 16K | q8_0 | `--cpu-moe` if RAM ≥ ~56 GB; CUDA-13 serves, 12.8 peak |
| `amd-rdna3` | #7 (780M + 64 GB, Vulkan) | `offload-e4b` | 16K (floor; canary → 32K) | f16 (floor; canary → q8_0) | `--cpu-moe` floor; canary → full-offload `-ngl 99` (~20–25 t/s measured elsewhere) — see the [AMD RDNA3 chapter](#amd-rdna3--the-780m-class-runbook-for-the-installing-agent) |
| `amd-rdna3-dgpu` | discrete RDNA3 ≥12 GB (RX 7900-class, Vulkan) | `gemma4-26b-a4b` | 32K | q8_0 | full-GPU `-ngl 99` (on 12 GB the OOM remediation flips it to `--cpu-moe`) |
| `ampere-6` | #10/#11 (3050 6 GB) | `gemma4-e2b` | 16K | q8_0 (**mandatory**) | dropped |
| `amd-gcn` | #12 (Vega 7 + 32 GB, Vulkan) | `gemma4-e2b` | 8K | f16, FA off | dropped |
| `cpu` | no GPU | `offload-e4b` (CPU) | 8K | f16, FA off | `--cpu-moe` if RAM ≥ ~56 GB, else dropped |

**Big-VRAM Blackwell tiers (#13–15, added 2026-07-16):** cards ≥24 GB render the
`cuda-resident` template — every model is a standalone entry (no swap group, no ttl), so the
whole roster runs CONCURRENTLY with zero swap latency. RTX PRO Blackwell workstation cards
("NVIDIA RTX PRO 5000 Blackwell" etc.) are classified by their own arch rules — the plain
`RTX 50xx` regex does not match them. The 48/72 tiers serve full-precision **f16 KV** at 128K
ctx (quality lever; model window is 256K design / 128K common serving cap). These profiles also
carry a `config_seed` (720p-class video defaults) applied ONLY to a fresh
`~/.local-offload/config.json` — an existing per-machine config is never touched. llama-swap
does NO VRAM accounting: all values are PROJECTED until `selftest.ps1` measures the real box.
On a real `blackwell-72` box the 26B stays Q4_K today; a **Q8_0 26B pin (~28 GB)** is an
available quality upgrade held as a follow-up — activate it only after a ≥64 GB card verifies
the download fits all-resident and measures the quality/throughput gain (spec decision 3 +
"Out of scope").
Spec: `docs/superpowers/specs/2026-07-16-blackwell-profile-tiers-design.md`.

### Quality-first generation policy (operator directive, 2026-07-16 — applies to EVERY tier)

**Bind the highest-quality model/precision the box can RUN AT ALL. RAM offload and long
renders are acceptable; visible artifacts are defects. Speed variants exist only as explicit
opt-ins (`fast:true`), never defaults.** Spec + measured evidence:
`docs/superpowers/specs/2026-07-16-quality-first-generation-design.md` (the quantized-distilled
image default produced 3x on-grid patch blocking; the bf16 Base + official family graph at
native 2048 removed it at 3.9 min/render on a 16GB card).

**run-graph host pinning (v1 protection, 2026-07-17):** when `offload_run_graph` satisfies a
node manifest, every pip/uv it spawns is constrained to the host's installed
`torch/torchvision/torchaudio/numpy` (`PIP_CONSTRAINT`/`UV_CONSTRAINT` + a post-install drift
tripwire). A pack set that cannot install additively around the box's CUDA stack DEFERs
`VENV_INCOHERENT` — it never replaces ComfyUI's torch (which would break the video path).
Do not remove these pins to "make a pack install work"; that trade is never authorized.

Every ≥16GB CUDA tier's `config_seed` binds: HiDream-O1 **bf16 Base** via `imagegen_family`
(the official graph — never the generic SDXL graph for a DiT), Wan 2.2 **Q8_0** experts +
**umt5_xxl_fp16**, 720p × 81 frames. The seed only writes a FRESH config; for an existing
box, set the same keys in its config.json. Model download set (SHA256 from the HF API):
`hidream_o1_image_bf16.safetensors` (16.4GB, Comfy-Org/HiDream-O1-Image),
`Wan2.2-I2V-A14B-{High,Low}Noise-Q8_0.gguf` (15.4GB ×2, QuantStack/Wan2.2-I2V-A14B-GGUF),
`umt5_xxl_fp16.safetensors` (10.6GB, Comfy-Org/Wan_2.2_ComfyUI_Repackaged). Requires
ComfyUI ≥ v0.21.1 (the HiDream-O1 nodes) and ≥~48GB system RAM for the offload path.

The **≥16GB image-EDIT primitive is Qwen-Image-Edit-2511** (Apache-2.0, commercial-safe — the
8GB→16GB compositing/edit unlock). This is a *recommended-model designation*, **not** a `config_seed`
binding: edit workflows (e.g. the creative-marketing-pipelines scene-swap) run through `run-graph`
with the model set declared in their own node manifest, so the harness never binds an edit checkpoint
in `config.json`. HiDream-O1 (t2i) and Wan (video) stay the config_seed bindings; RealVisXL is the
SDXL-class inpaint binding. **FLUX-family stays prohibited** (BFL non-commercial — ADR 0011).

> **GGUF quant caveat — pin a `_1` quant, never a `_K_` one.** Qwen-Image-Edit-2511 K-quants
> (`Q5_K_S` and friends, including the common unsloth default) **fail to load** in ComfyUI-GGUF's
> `UnetLoaderGGUF` with `cannot reshape array`, even on a byte-perfect file (verified: disk sha ==
> upstream LFS oid, gguf 0.19.0, pack at HEAD). Only **`Q4_1` / `Q5_1`** load for 2511 — see
> city96/ComfyUI-GGUF issue #247. Measured live on `ampere-16` (Qube) 2026-07-19 by the
> creative-marketing-pipelines session: **Q5_1 (15.4GB) + fp8 Qwen2.5-VL encoder fits 16GB with
> block-swap** — composite peak **15,757 MiB** (HiDream for comparison: 15,688 MiB). A manifest that
> pins a K-quant will download 15GB and then fail at load time, so pin the `_1` quant explicitly.
8GB tiers: **VERIFIED** — O1 bf16 @2048 runs on an 8GB 3070 with 64GB RAM (5.9 min/render,
an 8GB 3070 + 64GB RAM box, 2026-07-16). The seed stays off for 8GB tiers only because low-RAM boxes can't offload
it — bind manually on any 8GB box with ≥~48GB RAM; video Q8_0 via DisTorch2 likewise.

### run-graph satisfier prerequisite (`offload_run_graph`)

`offload_run_graph` self-provisions a workflow's node manifest against the ComfyUI install.
Packs are cloned/checked out at their pinned commits via **git**, then ALL packs' deps are
resolved in **one `uv pip compile`** (under the host-torch constraints) and installed as one
lock — so **`uv` in the ComfyUI venv is the REQUIRED satisfier tool** (live finding
2026-07-17: the installed cm-cli has no `--uv` flag, so uv is driven directly).
`install.ps1` provisions the full set — ComfyUI-Manager clone + GitPython (cm-cli's own
import) + **`pip install uv`** — as part of the run-graph deps step, guarded to skip when
present. It runs only when a ComfyUI install is discoverable — override with
`$env:COMFY_DIR` (default `C:/ComfyUI`) / `$env:COMFY_PY`.

`comfy-cli` is an **optional convenience only** — run-graph does **not** depend on it.
`install.ps1` installs it best-effort: on boxes where its wheel deps (`pydantic-core`) have
no prebuilt wheel and no Rust toolchain, the install logs a **WARN and continues**.

If `uv` is missing at call time, run-graph returns a clean **DEFER `SATISFIER_UNAVAILABLE`**
— never a crash, never a cloud fallback. A box without ComfyUI at all is unaffected by install
(the deps step SKIPs); run-graph simply DEFERs the same way when invoked.

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
  `sm_70` (Volta). It must be compiled against a **CUDA 12.8/12.9 toolkit** — CUDA 13.0 removed
  offline compilation for Volta (`sm_70`), so a 13.x toolkit cannot produce this build (an R580+
  *driver* still drives the V100; only the toolkit dropped it). The dual-gpu profile pins
  26B→device 0 / E4B→device 1; heterogeneous-arch multi-GPU is supported but confirm per-device
  placement.

**H4 (shipped): `install.ps1` automates this selection.** Step 3 reads detect's
`cuda_driver`/`cuda_toolkit` (`Select-CudaBuild`): Blackwell profile + CUDA-13 driver → the pinned
`llama-cuda13` (13.3) prebuilt, SHA-verified (tier `serves`); Blackwell on a 12.x driver / undetected
CUDA → **refuses loudly** with the exact driver-upgrade-or-source-build guidance; `dual-gpu` →
refuses with the multi-arch source-build recipe (`-DCMAKE_CUDA_ARCHITECTURES="70;120"`, 12.8/12.9
toolkit); every other CUDA profile keeps the verified 12.4 prebuilt. The selection is reported as
`NOTE`/`OK` lines and recorded in `installed.json` (`cuda_build`); re-running install after a driver
upgrade or the V100 arriving adapts automatically (the manifest key change forces the re-install).
Overrides for synthetic boxes: `OFFLOAD_CUDA_DRIVER` / `OFFLOAD_CUDA_TOOLKIT`.

Runtime env for Blackwell: `CUDA_VISIBLE_DEVICES` set explicitly (avoid the hybrid-graphics
`-1` trap), `CUDA_MODULE_LOADING=LAZY` — **install.ps1 injects both automatically** into every model
block of the rendered `llama-swap.yaml` on `blackwell-*` profiles (the dual-cuda template already
pins devices per group). Standard Q4_K GGUFs get **no** FP4 tensor-core speedup
(NVFP4 MMQ only helps NVFP4-format models) — do not expect an FP4 win. **Report the detected CUDA
version + which build you selected + the expected perf tier to the human; get consent before
installing a new CUDA toolkit or a driver (Hard rule 3).**

---

## AMD RDNA3 — the 780M-class runbook (for the installing agent)

**You are probably Claude running on the owner's machine.** This chapter is the complete
install → test → validate → report sequence for the `amd-rdna3` profile (Radeon 780M-class iGPU +
big dual-channel DDR5; `amd-rdna3-dgpu` for discrete cards differs only where noted). This tier is
**PROJECTED, not yet validated on real hardware** — your job is to run the sequence, apply ONLY the
canary-gated promotions below, and send back the receipt that turns every PROJECTED number into
MEASURED. The profile's serving floor is deliberately conservative; the canaries are how it earns
its real performance.

**Why this tier is frontier-pinned:** the AMD Vulkan flash-attention tuning that makes a 780M
competitive landed **Feb–Mar 2026** (scalar-FA Wave32 + AMD graphics-queue work ≈ +56% on AMD
iGPUs), and FA + q8_0-KV on Vulkan dates from the same window. The pinned llama.cpp build satisfies
this **≥ Mar-2026 floor**; `install.ps1` prints a NOTE when upstream is ahead of the pin
(`Show-FrontierNote`). **Never install a build older than the floor, and never substitute a newer
one mid-install** (Hard rule 2 — pins change by a human bumping them, then re-running this
chapter's canaries).

### What to expect (so you don't misread normal as broken)

- **Token generation is memory-bandwidth-bound.** DDR5-5600 dual-channel envelope: E2B/3–4B-class
  ≈ 30–45 t/s · E4B/7–9B-class Q4 ≈ 15–20 t/s · 26B-A4B full-offload ≈ 20–25 t/s (yes, the MoE is
  FASTER than dense 7B — active params are what count). ~20 t/s is **normal and correct**, not a
  defect, and ROCm would not improve it (Forbidden detours, Hard rules).
- **Prompt processing is the UX pain:** pp ≈ 150–260 t/s, so a 4K prompt costs 20–30 s. llama-swap
  KV reuse and the harness's compaction rungs are the mitigations — do not chase pp with driver
  or backend experiments.
- **A small `vram_dedicated_gb` is expected** (BIOS carve-out). Real capacity is UMA/GTT shared
  memory — the iGPU can address ~half of system RAM. Capacity is a non-issue on a 64 GB box;
  `-ngl 99` full offload of the 14.2 GB 26B through GTT is the design, not an accident.
- **Single-channel RAM is a clean ~2× loss** on every tg number above. Confirm dual-channel
  (hardware question 1) before judging any measurement.

### Sequence

1. `detect.ps1` — expect `profile:"amd-rdna3"`, `backend:"vulkan"`, and the new `amd_adrenalin`
   field. If the Adrenalin warning fires (driver ≤ 25.11.1 = the deep-context Vulkan crash class,
   llama.cpp #17432): **STOP and ask the human to update the driver. Never update it yourself.**
2. `install.ps1` — renders the Vulkan template: 16K ctx / f16 KV / FA explicitly `on` / 26B
   `--cpu-moe` / `GGML_VK_VISIBLE_DEVICES=0` on every model. If `llama-server --list-devices`
   shows the wrong adapter as device 0 (multi-ICD boxes can enumerate a second GPU or a software
   rasterizer first), fix the index in the rendered `llama-swap.yaml` — that edit is yours to make.
3. `selftest.ps1` — on a Vulkan backend the **H3 canary suite runs by default** (budget 15–30 min;
   the 26B GTT load alone can take minutes — that is a load, not a hang). Read
   `receipt.canaries.*` against the table below.
4. Apply the **authorized promotions** (next section) to the rendered `llama-swap.yaml` +
   `installed.json`'s `agent_ctx_tokens`, restart llama-swap, re-run `selftest.ps1` once to
   confirm the promoted config still passes.
5. **Send the receipt back** (last section). Until the receipts land upstream, every number in
   this tier stays PROJECTED.

### The canaries — what each PASS proves, and does not prove

| Canary | Proves | Does NOT prove | On fail |
|---|---|---|---|
| `fa_q8kv` | FA is genuinely ON (server log at `-lv 10`) and q8_0-KV text matches f16 at temp 0 (word overlap ≥ 0.80) on THIS gpu+driver | long-session stability at depth; other models | Stay on f16. `fa_confirmed:"off"` despite `-fa on` = the silent-disable mode — report it, do not force q8. |
| `moe_full_offload` | the 26B loads at `-ngl 99` through GTT with a measured decode t/s — `promote:true` when it beats the `--cpu-moe` baseline, or when no baseline was measured this run (the receipt's `cpu_moe_tps` says which case you are in) | that 32K ctx also fits alongside; thermals under sustained load | Keep the `--cpu-moe` floor. The manual middle path is `--n-cpu-moe N` (a few expert layers in RAM — preferred over `-ot` regex; try N≈4–8). |
| `ctx_sweep` | the resident tier loads AND generates at 8/16/32K with the canary-chosen KV | quality at depth (SWA-1024 keeps KV modest; depth-quality is the model's own behavior) | The standing ctx probe/downshift result governs; serve the largest PASSING size. |
| `bench` | pp512/tg128 on this exact build+driver (your regression baseline) | anything cross-machine | `tg < 8` = CPU-class → offload is broken; check FA state, device pin, and the Adrenalin warning before anything else. |
| `swap_leak` | llama-swap evicts cleanly across a tier cycle (≤1 llama-server process after) | multi-hour TTL behavior on :11436 | Report; do not ship a leaking stack. |
| `embedder` | embeddinggemma orders related > unrelated by cosine through the real endpoint | retrieval quality | Report; the memory-stack seat depends on it. |
| `whisper` | (always skipped — no seat installed) | — | When the human later wants STT here: **whisper.cpp ≥ 1.8.3 is the floor** on AMD iGPUs (~3–4× realtime); earlier builds are not viable. |

**Vision seat warning:** Gemma-Vulkan's open bugs cluster on the VISION path (garbled/frozen image
processing on APUs). The text tier is unaffected. **Do not bind a vision model on this box** until
a dedicated vision canary exists and passes — this is a named non-binding, like `gpt-oss-20b`.

### Decisions you MAY take autonomously (each gated on its canary, nothing else)

| Decision | Gate | Action |
|---|---|---|
| KV `f16 → q8_0` | `fa_q8kv.status:"pass"` (which requires `fa_confirmed:"on"`) | Set `--cache-type-k/v q8_0` in the rendered yaml (K and V stay symmetric). |
| ctx `16384 → 32768` | `ctx_sweep.max_ok_ctx ≥ 32768` | Raise `--ctx-size` and `installed.json`→`agent_ctx_tokens` to 32768 together. |
| 26B `--cpu-moe → -ngl 99` | `moe_full_offload.promote:true` | Replace `--cpu-moe -ngl 999` with `-ngl 99` on the 26B cmd. If a later request OOMs, the standing remediation returns it to `--cpu-moe` — that rollback is also yours to keep. |
| Vulkan device index | `--list-devices` shows the wrong device 0 | Edit `GGML_VK_VISIBLE_DEVICES` to the correct index. |

Apply promotions ONE at a time, re-running selftest after the set — a combined failure you cannot
attribute is worse than a slower verified config.

**HARD FORBIDDEN (no canary overrides these):** updating/downgrading the GPU driver or Adrenalin
(**always** STOP-and-ask); ROCm/HIP or WSL2 for inference (dead ends — Hard rules); substituting
any pinned asset; binding a vision model (above); exposing any server beyond loopback.

### The 5 hardware questions (ask the human once; answers pin the perf envelope)

1. Exact DDR5 speed (MT/s) and **stick count/channel config** — dual-channel confirm; single is a ~2× tg loss.
2. BIOS UMA/carve-out setting (Auto / 4 GB / 16 GB …).
3. Chassis/SKU (mini-PC vs laptop — TDP; an 8700G clocks ~15% higher than a 780M laptop).
4. Task Manager → GPU: the **dedicated vs shared** memory totals it shows.
5. Installed AMD Adrenalin version (cross-check against detect's `amd_adrenalin`).

Put the answers in the receipt notes.

### The receipt to send back (PROJECTED → MEASURED promotion)

Send BOTH files, plus the answers above, back to the harness maintainer (agent-talk if wired,
else any file drop the human prefers):

```json
// 1. $OFFLOAD_HOME\installed.json  (versions actually installed)
// 2. selftest receipt — the LAST stdout line of the FINAL selftest run (post-promotions),
//    which carries: tiers[] (per-tier cold_load_s/tok_s), canaries.fa_q8kv{overlap,fa_confirmed},
//    canaries.moe_full_offload{full_tps,cpu_moe_tps,promote}, canaries.ctx_sweep{results,max_ok_ctx},
//    canaries.bench{pp512_tps,tg128_tps}, canaries.swap_leak, canaries.embedder,
//    profile_measure.tuned{...}, verdict, and the honest does_not_prove[] list.
```

Maintainer side: those numbers replace this chapter's "expect" ranges and `profiles.json`'s
PROJECTED notes with MEASURED values (same-PR docs), and this box becomes the harness's third
real-hardware validation profile after `ampere-8` and `blackwell-16`.

### `amd-rdna3-dgpu` deltas (discrete RX 7900-class)

Renders 32K / q8_0 / 26B `-ngl 99` resident directly (dedicated VRAM, no GTT dependency). The same
canaries run; `moe_full_offload` self-skips (`profile already projects full-offload`). On a 12 GB
card the 26B OOMs and the standing remediation flips it to `--cpu-moe` — expected, keep it. All
other rules of this chapter apply unchanged.

---

## Text-cascade matrix — validated ladder for ≥16GB tiers (2026-07-21)

The recommended cascade binding on a **≥16GB** box that serves a 12B-class MTP tier is the
**4-rung ladder**. Three rows use canonical repo aliases; the 12B rung has **no repo alias yet**
(no template serves it — see "Installer honesty" below), so its row shows the alias convention
used where it is live today (Qube: `offload-12b`, also answering to `gemma4-12b-qat`; the
historical bench script called the same tier `gemma4-12b`). When a template first serves a 12B
tier, pick ONE repo alias and update this row:

| Slot | Alias | Why (measured on `ampere-16`, 2026-07-20/21) |
|---|---|---|
| `triage_model` | `gemma4-e2b` | 154.5 tok/s; grammar-clean |
| `model` (workhorse) | `offload-e4b` | 95.7 tok/s; grammar-clean |
| `escalation_model` | `offload-12b` *(Qube-local — see note above)* (gemma-4-12B + MTP drafter) | **82.1 tok/s — 2.5× the 26B it offloads from**; grammar-clean 5/5; task-level A/B vs the both-26B incumbent showed zero regressions (outputs content-identical) |
| `reasoning_model` | `gemma4-26b-a4b` | terminal local tier, 32.5 tok/s; grammar-clean |

"Rungs" name the roster ladder, not the chain shape: the request chain stays
`[triage_model?] → model → escalation_model`, and the reasoning slot is the separate
grammar-task **terminal tier** that runs after the chain is exhausted (see
`docs/systems/offload-pipeline.md`).

**Know what you are changing from:** the shipped default binds `gemma4-26b-a4b` to BOTH
`escalation_model` and `reasoning_model` — exactly the both-26B shape this recommendation
replaces. That default is correct for 8GB tiers (a 12B-class rung does not fit beside the
residents there — the 8GB ladder stays e2b → e4b → 26b-`--cpu-moe`) and stays the built-in;
this is a matrix recommendation for ≥16GB operators, not a default change.

**Installer honesty:** the profile templates do NOT yet serve a 12B tier — no template renders an
`offload-12b` entry. On a box installed purely from this runbook the recommendation is
inapplicable until the operator adds the 12B entry to llama-swap out-of-band (as Qube did:
gemma-4-12B QAT + MTP drafter, aliases `offload-12b`/`12b`/`quality`). Full validation record:
the operator's benchmark archive (`2026-07-20_qube-roster-validation/`, ROSTER.md).

Two validated **non-bindings** (as load-bearing as the bindings):

- **`gpt-oss-20b` must never fill ANY cascade slot.** Every cascade tier generates under a GBNF
  grammar (all tasks, summarize included), and its harmony channel format is structurally
  incompatible with GBNF (hard 500: "output does not match the expected peg-native format");
  separately, its reasoning phase consumes the token budget (empty `content` on small budgets).
  Outside the cascade it keeps its real role: the free-text/interactive throughput model —
  4-slot admission proven, with aggregate throughput HALVING under 4-way load (the seat's claim
  is "no queueing", not "4× tokens").
- **`stt_model_hq` CAN bind an llama-server mtmd STT model since v0.22.15 — but only with
  `stt_hq_api` set.** Historical gap: the HQ transcribe client spoke only the whisper-server HTTP
  protocol, so binding Qwen3-ASR (served by llama-server, mtmd) deferred with a whisper-endpoint
  404 (verified live 2026-07-21, rolled back). The HQ path now also speaks the OpenAI
  `/v1/audio/transcriptions` shape via the same `/upstream/<model>/` passthrough. To bind such a
  model set BOTH: `"stt_model_hq": "qwen3-asr"` AND `"stt_hq_api": "openai"` (omitting the API
  field keeps the whisper protocol and reproduces the 404). Caveats: no timestamps on this path —
  the result carries ONE full-span segment (whisper-turbo keeps the timestamps/SRT/long-form
  role); language comes from the model's own detection (the `language X<asr_text>` prefix is
  parsed out); whisper decode knobs (VAD/beam/language forcing) do not apply.

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

**`canaries` (J1 — promotion gates).** On a **Vulkan** backend the receipt also carries a
`canaries` block (`fa_q8kv` / `moe_full_offload` / `ctx_sweep` / `bench` / `swap_leak` /
`embedder` / `whisper`), run by default (force on any box with `OFFLOAD_SELFTEST_CANARIES=1`,
off with `=0`). These never change the verdict — they authorize specific config promotions.
Read them against the tables in the [AMD RDNA3 chapter](#amd-rdna3--the-780m-class-runbook-for-the-installing-agent).

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
