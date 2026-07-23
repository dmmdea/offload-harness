# Operator Guide — running, driving & diagnosing the stack

Task-oriented walkthroughs for an agent (or human) operating an installed offload-harness stack.
Each task is **goal → commands → expected output → failure table**. Commands are given for
**PowerShell** (native Windows install) and **bash** where applicable. `$OFFLOAD_HOME` defaults to
`$HOME\offload-stack`; the harness config lives at `~/.local-offload/config.json`. If no config file resolves at all, every command warns on stderr that it is running on BUILT-IN DEFAULTS (machine bindings inactive) — `local-offload doctor` shows the resolved source on its `config:` line; never debug a binding problem without checking that line first.

> **Verified on this machine:** the commands marked ✅ below were spot-executed verbatim while
> writing this guide (NVIDIA/CUDA host, llama-swap live on :11436). Output snippets are real.

---

## 1. Start / stop / restart the whole stack

### Native Windows (the primary path)

**Start** llama-swap (loopback-only, the user's long-running service):

```powershell
& "$env:OFFLOAD_HOME\llama-swap\llama-swap.exe" --config "$env:OFFLOAD_HOME\llama-swap.yaml" --listen 127.0.0.1:11436
```

**Verify it's up** ✅:

```powershell
& "$env:OFFLOAD_HOME\harness\local-offload.exe" --config "$HOME\.local-offload\config.json" doctor
```
```
config:     ~/.local-offload/config.json
endpoint:   http://127.0.0.1:11436/v1/chat/completions
model:      offload-e4b
health:     OK
model:             OK    offload-e4b
triage_model:      OK    gemma4-e2b
escalation_model:  OK    gemma4-26b-a4b
...
```

**Stop:** Ctrl-C the llama-swap window, or:

```powershell
Get-Process llama-swap,llama-server -ErrorAction SilentlyContinue | Stop-Process -Force
```

**Restart:** stop, then start. llama-swap lazy-loads a model on the first chat request, so a fresh
start shows no model resident until you call one.

### WSL / NVIDIA legacy GUI path

`scripts/openwebui-stack.sh` brings up the coding-agent server (:18800) **and** OpenWebUI (:8081) in
one shot (idempotent — skips whatever is already up):

```bash
bash scripts/openwebui-stack.sh
# -> starting agent server on :18800 ...
# -> starting OpenWebUI on :8081
# -> stack UP — open http://localhost:8081
```

Override defaults via env before running: `LOCAL_AGENT_MODEL` (default `offload-e4b`),
`LOCAL_AGENT_WORKSPACE`, `LOCAL_AGENT_CAPS`, `LOCAL_AGENT_MAX_STEPS`, `LOCAL_AGENT_MAX_SAME_TOOL`.
Logs: `/tmp/agent-server.log`, `/tmp/openwebui.log`.

| Failure | Fix |
|---|---|
| `doctor: health: DOWN` | llama-swap not running / wrong port. Start it; confirm `--listen 127.0.0.1:11436`. |
| `stack did not confirm ready` | Read the two `/tmp/*.log` files; usually OpenWebUI still installing or the agent port busy. |
| alias `FAIL — not in the live roster` | The yaml doesn't serve that alias, or a model file is missing. Check `llama-swap.yaml`. Vision/STT aliases are legitimately absent on a grunt-work-only install. |

### Fleet node (`fleet-serve`) — accept dispatched renders from other boxes

Full guide: `docs/FLEET-NODE.md`. The short operating loop:

**Start** (loopback for a local check; the **Tailscale address** for production — the
endpoints are unauthenticated, so the tailnet is the trust boundary; port **18811**,
**never `0.0.0.0`**):

```powershell
local-offload fleet-serve                                                    # loopback smoke
local-offload fleet-serve --listen 100.64.0.10:18811 --listen-trusted-network   # production (Tailscale addr)
```

**Verify:**

```powershell
curl http://127.0.0.1:18811/fleet/health
# {"node_id":"node-a","schema_version":1,...,"vram_total_gb":15.9,"supported_task_types":["image-gen",...],"model_footprints":[...],"queue_depth":0}
```

An empty `model_footprints` on a fresh box is expected — prime it once with
`local-offload fleet-measure` (one minimal render per configured task; prints the recorded
entries).

**Stop:** Ctrl-C. The server drains: new dispatches 503, in-flight renders get up to 30s,
survivors are marked terminal `error:"interrupted"` so the dispatcher's pollers always
resolve.

| Failure | Fix |
|---|---|
| `refusing to bind --listen` | Non-loopback address without `--listen-trusted-network`. Use loopback, or add the flag for the Tailscale bind only. |
| `GPU probe failed` at startup | `nvidia-smi` missing/broken. By design — a node that can't report VRAM would be treated as broken by the dispatcher. Fix the driver first. |
| footprints look wrong vs Afterburner | Run the PDH-vs-Afterburner validation in `docs/FLEET-NODE.md`; >15% off → set `"fleet_sampler":"global"`. |

---

## 2. Chat with a model directly

### curl (against llama-swap) ✅

```bash
curl -s http://127.0.0.1:11436/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"offload-e4b","messages":[{"role":"user","content":"Reply with one word: ready"}],"max_tokens":16,"temperature":0}'
```

Returns a standard OpenAI chat completion. Switch `model` to `gemma4-e2b` (fast) or `gemma4-26b-a4b`
(MoE) — the first request to a different alias triggers llama-swap to evict the current model and
cold-load the new one (a few seconds).

### OpenWebUI

Open `http://localhost:8081`, create your account on first launch (auth is ON by design — do not set
`WEBUI_AUTH=false`), pick the advertised model, and chat. In the GUI, the "model" is the agent
server, so each message runs a full agent loop (see §4), not a bare completion.

---

## 3. Run an offload task + read the ledger

An offload task returns a verified JSON result or a structured defer. ✅

```powershell
"The Q3 review: infra costs fell 12% after moving to reserved capacity; the support backlog cleared." |
  local-offload --config setup\templates\config.json summarize - --max-points 3 --json
```
```json
{
  "ok": true,
  "result": { "bullets": ["Infrastructure costs decreased by 12% ...", "..."], "summary": "..." },
  "meta": { "tokens_in": 115, "tokens_out": 123, "tok_per_s": 87.0, "model": "offload-e4b", "grounded": true }
}
```

Other tasks: `classify - --labels bug,feature,question`, `extract - --schema fields.json`,
`triage - --question "Is this an error?"`. A `{"deferred":true,"reason":"..."}` result is **normal**
for low-confidence / over-long input — hand it back to yourself.

**Read the ledger** (token-savings accounting) ✅:

```powershell
local-offload --config setup\templates\config.json ledger --since 1
```
```json
{"calls":11,"completed":11,"deferred":0,"tokens_saved":2920,"tokens_out":755,
 "est_value_kept_local":0.0438,"by_task":{"summarize":3,"triage":8}}
tokens kept local (est.): 2920 (~$0.04 Opus-input value — an estimate, not billed savings)
```

| Failure | Fix |
|---|---|
| every call `deferred:true` | Run `doctor`. Usually the endpoint is down or unreachable. A defer on genuinely hard/over-long input is by design. |
| `cache unavailable (held by the MCP server?)` | Expected — the bbolt cache is single-writer. The CLI continues cache-less; the ledger still appends. |

---

### Warm-batch image generation ✅

```powershell
local-offload generate-image --batch jobs.jsonl --json
```

One JSON object per line (`prompt` required; `negative`/`width`/`height`/`steps`/`seed`/`out`
optional — a missing seed is minted, a missing out defaults under the media dir). All jobs
render through **one warm ComfyUI session** — the checkpoint loads once instead of once per
image; the zero-always-warm teardown (free VRAM, kill the spawned ComfyUI, release the GPU
lock) runs at the **batch boundary**, however the batch ends. Measured on the 16GB box:
first job ~32s (absorbs the checkpoint load), warm jobs ~22s. A failed job is recorded in
its result item and does not abort the rest. Single (unbatched) renders keep the zero-warm
default — nothing changes unless you pass `--batch`.

### Run an arbitrary ComfyUI graph (run-graph) ✅

```powershell
local-offload run-graph --graph wf.json --manifest manifest.json --out-dir out/
```

Executes any API-format ComfyUI graph under the same GPU-lock/zero-warm lifecycle. The
**node manifest** (`node_packs[{name,repo,commit}]` + `models[{path,source_url,sha256}]`)
is satisfied BEFORE ComfyUI starts: packs cloned at their pinned commits with all deps
resolved **together** (uv) under **host-torch constraints** (provisioning never replaces the
installed CUDA torch — an unsatisfiable pack set defers instead), models downloaded and
sha-verified when a hash is given (`sha256: null` downloads are reported in
`unverified_models[]`). Results are node-addressed (`outputs.<node_id>[]` with
`path/type/kind/width/height`); every failure is a **typed defer**
(`SATISFIER_UNAVAILABLE`, `VENV_INCOHERENT`, `SATISFIER_SPAWN_FAILED`, `NODE_CLASS_MISSING`,
`EXTERNAL_COMFY_NEEDS_PACKS`, …) so callers branch on `code`, never parse prose.
Prerequisites (provisioned by `install.ps1`): **uv** (the pack satisfier's unified-resolve
tool — the hard requirement) + ComfyUI-Manager + GitPython in the
ComfyUI venv; set Manager `network_mode = offline` in `<comfy>/user/__manager/config.ini`
(its first-start registry fetch otherwise slows every cold start).

### Generative inpainting (inpaint-image) ✅

```powershell
# 1. build a white-on-black mask (white = repaint) with the deterministic mask_boxes edit op
local-offload edit-image render.png --ops '[{"op":"mask_boxes","boxes":[{"x":620,"y":540,"width":820,"height":820}],"feather":6}]' --out mask.png
# 2. re-render ONLY the masked region from a prompt
local-offload inpaint-image render.png --mask mask.png --prompt "clean glossy dark watch dial, no writing" --negative "text, letters" --seed 4242 --json
```

Masked re-denoise on the local ComfyUI (core nodes only, same GPU-lock/zero-warm
lifecycle): white mask pixels are re-imagined from the prompt, black pixels stay
untouched. The route needs a per-machine **SDXL-class** binding in `config.json` —
`inpaint_script` (path to `render/comfy-inpaint.mjs`), `inpaint_ckpt` (e.g.
`RealVisXL_V5.0_fp16.safetensors`), optional `inpaint_vae` (`builtin` = the
checkpoint's own VAE) — because `VAEEncodeForInpaint` is a latent-space technique: a
pixel-space DiT (HiDream) canNOT drive it even when it is the machine's
`imagegen_ckpt`. Unbound = the task defers cleanly. Knobs: `--denoise` (default 1.0 —
full re-imagination inside the mask), `--grow-mask` (default 16 px seam feathering in
latent space), `--steps/--seed/--out`. Diffusion cannot WRITE legible text: inpaint the
region clean, then add real type with the `edit-image` `text` op.
`--auto-text` (EXPERIMENTAL) replaces `--mask` with vision-detected text boxes; it
defers whenever detection is unparseable, empty, or absurd (>60% coverage) — build the
mask with `mask_boxes` yourself when it does.

### Deterministic post-production (edit-image op pack) ✅

```powershell
# grade -> resize -> finish (delivery sharpen LAST), plus a platform export matrix
local-offload edit-image render.png --ops '[{"op":"grade","levels":{"black":8,"white":248,"gamma":1.05},"wb":{"mode":"gray_world"}},{"op":"resize","width":1920},{"op":"finish"}]' --renditions '[{"width":1920,"format":"jpg","suffix":"-web"},{"width":1080,"format":"webp","suffix":"-ig"}]' --json
# apply a .cube LUT look at 60% strength
local-offload edit-image render.png --ops '[{"op":"lut_cube","path":"D:/luts/teal-orange.cube","strength":0.6}]'
# place a render onto a laptop-screen mockup (quad = UL,UR,LR,LL screen corners)
local-offload edit-image mockup.png --ops '[{"op":"perspective_composite","overlay":"render.png","quad":[[412,180],[1508,236],[1490,940],[398,905]]}]'
# GIMP layered-template factory: new copy + swapped product image, then PIL ops
local-offload edit-image template.xcf --ops '[{"op":"instantiate_design","set_text":{"Headline":"Hola Bogotá"},"replace_image":{"ProductShot":"D:/renders/watch.png"}},{"op":"resize","width":1080}]'
```

All raster ops are deterministic, CPU-only PIL (no GPU lock, run in parallel with
renders); GIMP is needed only for `flatten_design`/`instantiate_design` (headless
`gimp-console`, invocations serialized process-wide). The op pack:

- **`grade`** `{levels{black,white,gamma}?, curve{points[[in,out],…]}?, wb{mode:gray_world|scale,r,g,b}?, luminance_only?}` —
  tone/color grading. Every transform composes mathematically into ONE 256-entry LUT
  per channel and quantizes ONCE (chained 8-bit `.point()` passes band visibly); the
  alpha channel is never remapped.
- **`lut_cube`** `{path, strength?}` — a `.cube` 3D LUT "look" via Pillow's built-in
  `Color3DLUT`; `strength` 0–1 blends the graded result over the original. 1D cubes
  and non-standard domains are rejected.
- **`perspective_composite`** `{overlay, quad:[[x,y]×4]}` — warps the overlay into the
  destination quad (**UL,UR,LR,LL winding** — a mismatched winding silently mirrors)
  with a pure-Python homography solve + BICUBIC resample, then alpha-composites:
  screen/frame mockup placement. Quads live with the mockup asset; there is no
  auto-detection.
- **`finish`** `{sharpen{radius,percent,threshold}?, median 3|5?}` — delivery
  sharpening; defaults (radius 1.2 / percent 80 / threshold 3) are tuned for
  post-AI-upscale web output (Pillow's 150% default over-crisps upscaler output).
  Explicit zeros are honored (`"sharpen":{"percent":0}` = no visible sharpening —
  the way to get a median-only finish through the harness; direct worker callers
  may also pass `"sharpen":null`).
  **Ordering rule: `finish` must be the LAST op, after any `resize`** — sharpening
  before a resize is undone by resampling. `median` is only for salt-and-pepper
  speckle; real sensor/upscaler noise reduction (NLM/BM3D-class) is out of PIL's
  reach and deliberately not faked.
- **`--renditions`** `[{width/height, format png|jpg|webp, suffix}]` — after the ops
  pipeline produces the master `out`, each rendition re-runs the worker
  (resize+convert) writing `<out-stem><suffix>.<format>`; results land in
  `renditions[]`. One master, full platform matrix in one call.
- **`instantiate_design`** `{set_text{LayerName: copy}, replace_image{LayerName: path}}`
  (FIRST op only, `image` = a `.xcf`/`.psd` template) — the GIMP template factory:
  sets named **text layers'** copy, swaps named **pixel layers** for replacement
  images at the old layer's offsets, flattens, and hands the raster to the rest of
  the pipeline (e.g. `grade` → `renditions` = a one-call brand-variant factory).
  A layer-name mismatch is THE common failure — the error surfaces GIMP's stderr
  naming the failing lookup; check names with `flatten_design`'s `layers` output.

## 4. Drive the coding agent

The agent plans with a local model and acts through tools confined to a workspace. Build it once:
`go build -o local-agent ./cmd/local-agent` (or use `$OFFLOAD_HOME\harness\local-agent.exe`).

### One-shot CLI (read-only by default) ✅

```bash
local-agent --root . --base http://127.0.0.1:11436 --max-steps 4 \
  "List the files in the workspace and tell me what README.md contains in one sentence."
# ...final answer...
# [local-agent] steps=3 stop=done tools=6
```

### Server mode (`--serve`) — for a chat GUI ✅

```bash
local-agent --serve --listen 127.0.0.1:18808 --base http://127.0.0.1:11436
# [local-agent] OpenAI server on http://127.0.0.1:18808  (model="offload-e4b")
curl -s http://127.0.0.1:18808/v1/models
# {"data":[{"id":"offload-e4b","object":"model","owned_by":"local-offload"}],"object":"list"}
```

Each `/v1/chat/completions` POST runs the **full agent loop** over the last user message and returns
the final answer. The endpoint is **unauthenticated** — keep it loopback-only.

### Capability flags (all OFF by default)

| Flag | Grants | Notes |
|---|---|---|
| `--allow-write` | `write_file` (+ `delete`/`edit` only with the next two) | Worktree-scoped, policy-gated. |
| `--allow-overwrite` | overwrite existing files + `edit_file` | requires `--allow-write`. |
| `--allow-delete` | `delete_file` | requires `--allow-write`. |
| `--allow-fetch` | `web_fetch` | egress-allowlist gated; add hosts with repeatable `--egress-host` (bare host or `*.host`). Deny-all by default. |
| `--allow-search` | `web_search` (DuckDuckGo, keyless) | auto-allowlists the search host. |
| `--allow-run` | `run` — an allowlisted program run **directly** (no shell) in the OS sandbox | Linux **and** Windows. Allowlist + broker are the control (see "The runner" below). |
| `--allow-shell` | `run_shell` in the OS sandbox | **Linux only**; no network, FS-confined, syscall-limited. |
| `--allow-github` | `github_api` / `create_repo` / `upload_file` | token from `$GITHUB_TOKEN`, repo from `$GITHUB_REPO`. Use a least-privilege token. |
| `--listen-trusted-network` | bind `--serve` beyond loopback | prints a loud warning; only on a trusted LAN. |

### Tool profiles (`--profile`)

`--profile <name>` narrows the advertised tools to a curated subset and adds a tuned prompt + a couple
of worked few-shot exemplars — a small local model selects tools more reliably with fewer advertised.
A profile can only **narrow** the enabled set; it never grants a tool your `--allow-*` flags didn't
turn on.

| Profile | Use it for | Advertised tools (subject to your `--allow-*`) |
|---|---|---|
| `general` (default) | anything; today's full capability-gated set | all enabled tools |
| `edit` | a focused code edit in an existing repo | `list_dir`, `read_file`, `search_files`, `edit_file`, `write_file`, `update_plan` |
| `build` | edit-then-verify (needs `--allow-run` / `--allow-shell`) | edit set **+ `run` / `run_shell`** |
| `research` | find + read sources (needs `--allow-search`/`--allow-fetch`) | `web_search`, `web_fetch`, `summarize_file`, `read_file`, `list_dir` |
| `github` | prepare files then publish (needs `--allow-github`) | edit set **+ `github_api` / `github_create_repo` / `github_upload_file`** |

`--profile` and `--two-tier` are **mutually exclusive** (two-tier picks the architect/editor toolsets
itself); the CLI rejects the combination.

### The runner (`--allow-run`) + how to extend the allowlist

`run` executes an **allowlisted program directly — no shell**: you pass `command` (a bare executable
name) and an `args` array that is handed to the program literally (no pipes, globs, redirection, or
`&&`). The executable allowlist is the real control:

```
go, gofmt, python, python3, pytest, npm, node, cargo, git
```

A command must be a **bare name** (a path or `./go` is refused) that resolves on the **trusted system
PATH** — a `go.exe` planted inside the worktree is not resolvable and is refused. Every accepted
command is broker-gated and written to the audit log. **On native Windows, reads and network are not
contained** (Job Object + low-integrity writes only) — see [Diagnose](#6-diagnose) / README Security.

**Extend the allowlist** by editing `runAllowedExecutables` in `internal/agent/runtool.go` and
rebuilding `local-agent`. It is a compile-time list on purpose (no runtime flag) so the confinement
surface is auditable in the source.

### Two-tier (`--two-tier`) — plan once, then execute

`--two-tier` runs aider's architect/editor one-shot handoff: the **architect** (`--architect-model`,
default `gemma4-26b-a4b`) drafts one complete, standalone plan with read/search tools only, then the
**editor** (`--editor-model`, default `offload-e4b`) executes that plan as its **sole** message — it
never sees the original request or any history. On a single GPU this is **exactly one cold model
swap** (plan-once, not per-step alternation); on a dual-GPU box (profile `dual-gpu`) the two models
are resident on separate cards, so it is **zero swap**. A degenerate/empty architect plan falls back
to a single-model run of the original objective (logged as `fallback=…`). `--allow-*` flags gate the
**editor's** write tools; the architect is always read-only.

### Circuit breakers & budget

- `--max-steps` (default 12) — hard step budget, owned in code.
- `--max-same-tool` (default 3) — cap on calls to any one tool per run; the breaker for a model that
  loops (e.g. repeated reworded `web_search`). Negative disables; 0 → built-in default.
- `--max-tokens` (default 4096) — planner tokens per completion. Must be large enough for the biggest
  tool-call argument (e.g. a whole file's content) or the model's JSON gets cut off mid-string and
  the call fails. 4096 is the tested value; do not lower it for write-heavy runs.
- `--ctx-tokens` (default 16384) — the served model context window the loop's transcript compaction
  budgets against. **Set it to match the tier's served `--ctx-size`** (the CUDA tier serves 16384;
  the install prints the profile's value). The derived usable **input budget** is
  `ctx-tokens − max-tokens − 512`. Setting it too high lets the transcript overflow the real window
  (a 400); too low compacts sooner than necessary.
- `--gcf-compact` (default off) — the compaction ladder's LOSSLESS first rung: over budget, older
  tool results that are JSON arrays of flat objects are re-encoded columnar (keys stated once,
  `internal/gcf`, round-trip proven — nothing is lost) before any lossy rung runs. The same
  transform guards the offload pipeline's context trim via the `gcf_compact` config field: an
  over-budget input's JSON is compacted losslessly before the head/tail cut, converting would-be
  truncations into full-fidelity completions.
- `--skeleton-prune` (default off) — the next, lossy-structural rung: over budget, older tool
  results are reduced to signal-preserving **skeletons** (head/tail lines + error/failure/warning
  lines kept, elided runs replaced by `[... n lines elided ...]` markers) before the existing
  bare-marker and turn-drop rungs run. Deterministic and local — no model call, no added latency.
  Long multi-step runs keep *what went wrong earlier* visible to the model instead of losing whole
  older results at the first budget crossing.

### Context-budget guidance (why prompt shape matters)

The planner models here have a **~32K context window** and the loop **resends the full growing
transcript every step**. A wide, exploratory `web_search`-heavy prompt accumulates search results
into that transcript and can blow the window before finishing. **Prefer "edit an existing file, then
upload it" over "search the web, build, upload."** The search leg is what overflows context on broad
topics; edit+upload alone completes reliably and fast. For anything beyond a narrow task, keep the
toolset lean (no shell/delete unless needed — each tool adds schema overhead) and the same-tool cap
low so a stuck model gets its tool disabled quickly.

**Prompting rules of thumb (read before your first prompt):**

1. **ONE bounded task per message.** "Edit index.html to add a pricing section, then upload it" — good.
   A multi-goal essay ("research X, then build Y, then also refactor Z and…") — the run dies mid-way.
2. **Never paste long documents into the agent chat.** The whole paste rides in the transcript on
   every step. To digest a big file, use the harness instead (`local-offload summarize <file>`) and
   hand the agent the summary or the file *path*.
3. **Don't chain more than ~3 tool-kinds in one ask** (search → write → upload is the practical
   ceiling on a 32K model). Split bigger jobs into sequential messages — each run starts fresh.
4. **If you see `agent error: chat 400 … context`, your prompt was too big or too broad.** Nothing is
   broken — narrow the ask and send again.
5. These are guardrails, not conventions: the loop hard-caps at `--max-steps` (12) and disables any
   tool called more than `--max-same-tool` (3) times per run, so a bad prompt costs one failed run,
   never the installation.

### Per-hardware-profile serving expectations

The installer resolves a hardware **profile** (`detect.ps1` → `install.ps1`, see
`setup/SETUP-AGENT.md`) and renders the serving template + writes the profile's `agent_ctx_tokens`
to `installed.json`. Run the agent with `-model <resident_tier>` and `-ctx-tokens <agent_ctx_tokens>`
matching the profile. These are **projected defaults**; `selftest.ps1` measures on the real box and
its `receipt.profile_measure.tuned` block carries any measured override to apply.

| Profile | Resident/default tier | Served ctx (`-ctx-tokens`) | KV | 26B-A4B |
|---|---|---|---|---|
| `blackwell-16` / `ampere-16` / `volta-16` | `gemma4-26b-a4b` | 32768 | q8_0 | full-GPU resident |
| `dual-gpu` | `gemma4-26b-a4b` (architect) + `offload-e4b` (editor), both resident | 32768 | q8_0 | resident (two-tier, **zero swap**) |
| `ampere-8` / `blackwell-8` | `offload-e4b` | 16384 | q8_0 | via `--cpu-moe` only when RAM ≥ ~56 GB; else dropped |
| `amd-rdna3` | `offload-e4b` (Vulkan) | 16384 | f16 (conservative) | `--cpu-moe`, very slow; else dropped |
| `ampere-6` | `gemma4-e2b` | 16384 | q8_0 (**mandatory** for 16K on 6 GB) | dropped |
| `amd-gcn` | `gemma4-e2b` (Vulkan) | 8192 | f16, flash-attn off | dropped |
| `cpu` | `offload-e4b` (CPU) | 8192 | f16, flash-attn off | `--cpu-moe` when RAM ≥ ~56 GB; else dropped |

Notes: q8_0 KV keeps the KV cache ~half the size (V-quant needs flash-attn on, which the CUDA/Vulkan
templates set); the 26B is placed full-GPU only on ≥12 GB single-card profiles, `--cpu-moe` (experts
in RAM, much slower — "reduce, not enable") on 8 GB + big-RAM boxes, and dropped where there is no
RAM path. On the dual-GPU profile the two models sit on separate cards so `--two-tier` costs no swap.
Anything *italic/projected* is refined by the install-time measurement — trust the selftest receipt
over the projected table when they differ.

| Failure | Fix |
|---|---|
| `refusing to bind --listen` | You passed a non-loopback address. Use `127.0.0.1`, or `--listen-trusted-network` (only if authorized). |
| agent stops with `stop=step-cap` / loops | Raise `--max-steps`, or the model is stuck — lower `--max-same-tool`, narrow the prompt (edit+upload shape). |
| GitHub tool refuses | `$GITHUB_TOKEN` unset or under-scoped, or `$GITHUB_REPO` unset. See §6. |

---

## 5. Add / replace a model in llama-swap.yaml

1. Download the GGUF into `$OFFLOAD_HOME\models\`.
2. Edit `$OFFLOAD_HOME\llama-swap.yaml`. Copy an existing `models:` entry, set `-m` to the new file,
   keep the `${common}` macro (it carries the grammar-reliable flags), and give it an alias. To make
   it swap-exclusive with the others, add the alias to `groups.offload-family.members`.
3. **Use forward slashes** in paths inside the yaml (llama-swap on Windows chokes on backslash
   escapes; the installer already renders forward slashes).
4. Point the harness at it by editing the matching key in `~/.local-offload/config.json` (e.g.
   `escalation_model`).
5. Restart llama-swap, then verify ✅:

```powershell
local-offload --config "$HOME\.local-offload\config.json" doctor   # the new alias must show OK
```

| Failure | Fix |
|---|---|
| alias `FAIL — not in roster` | Restart llama-swap so it re-reads the yaml; confirm the alias name matches exactly. |
| model won't load / device-lost | VRAM too small — add `--cpu-moe` (MoE) or lower `-ngl`. See §6. |

---

## 6. Diagnose

| Symptom | Cause → fix |
|---|---|
| **Everything defers** | Endpoint unreachable. Run `local-offload doctor`: `health: DOWN` → start llama-swap; an alias `FAIL` → fix the yaml. A defer on hard/over-long input is by design. |
| **Port busy** (`18801`/`18802` in selftest, or `:11436`/`:18800`) | `Get-NetTCPConnection -LocalPort <p> -State Listen` to find the owner; kill a leaked `llama-server.exe`/`local-agent.exe`, or reboot after asking. |
| **Model load fails** (OOM at load) | VRAM ceiling. For the 26B MoE add `--cpu-moe` (experts stay in RAM) to its `cmd`; for dense tiers lower `-ngl`. On AMD, selftest auto-remediates 26B with `--cpu-moe`. |
| **Vulkan driver crash** (device-lost mid-generation, AMD) | The deep-context crash class (llama.cpp #17432). Re-run `selftest.ps1` — its depth-7000 **canary** reproduces it and the receipt names your GPU + driver. Remediation: **update the AMD Adrenalin driver** (ask the human first), keep it current; interim: `--cpu-moe` / lower `-ngl`. If it persists on a big MoE, add `nodes_per_submit: 1` to the model entry. |
| **Context overflow** (agent HTTP 400 / `context` error) | The transcript exceeded the ~32K window (see §4). Narrow the prompt to edit+upload shape, trim the toolset, or lower `--max-steps`. |
| **GitHub tool refusals** | Token/scope. Ensure `$GITHUB_TOKEN` is set (least-privilege — only the scopes the task needs) and `$GITHUB_REPO=owner/name`. Put both in a **gitignored** `~/.local-agent-github.env`, never in the repo or a logged command line. |
| **Empty / truncated model output** | Serving with reasoning on, or `--max-tokens` too low. Confirm `--reasoning off` on the server; raise `--max-tokens`. Never pass `--json-schema`/`response_format` — they crash the model. |

---

## 7. Update the stack

```bash
git pull                                   # get the latest harness + scripts
go build ./...                             # rebuild — must stay green
go build -o "$OFFLOAD_HOME/harness/local-offload.exe" .
go build -o "$OFFLOAD_HOME/harness/local-agent.exe" ./cmd/local-agent
```
```powershell
pwsh -NoProfile -File setup\install.ps1    # picks up any bumped pins (idempotent; unchanged components SKIP)
pwsh -NoProfile -File setup\selftest.ps1   # re-verify: expect verdict pass|warn
```

A bumped pin in `install.ps1`'s `$PINNED` table forces a re-download of exactly that component
(the `installed.json` version check fails for it) while everything else SKIPs. After any update,
re-run selftest and confirm the receipt `verdict` is `pass` (or `warn` for known-soft signals).
