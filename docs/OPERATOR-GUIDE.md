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
| alias `FAIL — not in the live roster` | The name is served under neither a canonical id nor a `meta.llamaswap.aliases` entry — the yaml doesn't serve it, or a model file is missing. Check `llama-swap.yaml`. Vision/STT aliases are legitimately absent on a grunt-work-only install. |

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

### Moving the harness to another drive ✅

```powershell
local-offload install volumes            # which volume should hold it?
# copy the tree, then in config.json:  "home": "D:/offload-stack"
local-offload doctor                     # every derived path now under the new root
```

`home` (or `$LOCAL_OFFLOAD_HOME`) rebases every path still at its default; anything you set
explicitly is left exactly as you typed it. The machine-wide GPU lease root is never
rebased — it must stay machine-wide.

### Capability report — send this instead of describing your box ✅

```powershell
local-offload report --out capability-report.md      # Windows
./local-offload report --out capability-report.md    # Linux
```

One read-only Markdown document: harness version and platform, which config file is actually
loaded, the hardware tier from the installer manifest, every configured model alias measured
against the live `/v1/models` (matched against canonical ids **and** `meta.llamaswap.aliases`),
and every media route with its derived verdict — plus a
**Needs attention** section that lists only the genuinely broken ones (a route this box never
bound is a legitimate machine, not a fault).

It loads no model, queues no GPU work, and changes nothing. Attach it when asking for help, or
when a collaborator asks "what can that machine do?" — it answers from the same code paths the
harness routes on, so it cannot claim a capability the box would defer. A report generated on
BUILT-IN DEFAULTS says so on its config line: that machine's real bindings are inactive and every
verdict below it describes defaults, not the install.

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

Every row (0.124.0, ADR 0046) also says WHO asked and WHAT ran: `origin_session` (the Claude Code
session id the MCP server inherited from its environment, or `LOCAL_OFFLOAD_ORIGIN` when a caller names
itself; absent on rows a service or a fleet node wrote), `origin_pid` / `origin_ppid`, `cards_tokens`
(the one figure for the tokens the cards processed for that row — prompt work plus generation, 0 on a
cache hit), and on agent / delegate rows `job_id`, `route`, `placement`, `steps`, `stop_reason`,
`repack_ms`, `acceptance_result`. A per-session share is one filter away:
`grep '"origin_session":"<session id>"' ~/.local-offload/ledger.jsonl`. Rows written before 0.124.0
carry none of these and must be read as unattributed, not as another session's.

| Failure | Fix |
|---|---|
| every call `deferred:true` | Run `doctor`. Usually the endpoint is down or unreachable. A defer on genuinely hard/over-long input is by design. |
| one media task always defers (`generate_image`, `generate_video`, `generate_audio`, `run_graph`, `edit_image`…) | Run `doctor` and read its **media routes** section. `BOUND-BUT-MISSING` names the exact configured path that is not on disk (relative script bindings resolve against the binary's directory, not your cwd) and exits non-zero; `NOT CONFIGURED` means this box has no such binding and the defer is by design. |
| `result cache is held by another local-offload process; using the per-process cache <path>` | Expected (0.113.21) — the bbolt cache is single-writer; this process keeps its own hits in a per-process sibling (`cache.p<pid>.db`) instead of running cache-less. Since 0.121.1 the note prints at the FIRST cacheable task rather than at startup, the sibling is deleted on exit when empty or promoted into the shared cache when the lock is free, and stale ones are swept after 1 h (was 12 h). `cache unavailable; continuing without cache: <err>` means BOTH the shared file and the sibling failed (disk, permissions, a bad `cache_path`) — not lock contention. The ledger still appends. |

---

### Image engines (J2)

`generate_image` has two per-machine backends via `imagegen_engine`: `""`/`"comfy"` (the ComfyUI
path, unchanged default) and `"sdcpp"` — stable-diffusion.cpp as a single native Vulkan binary
(`sdcpp_bin` + `sdcpp_model` + companions in config; spawn-per-job, zero-warm, no Python). The
AMD profiles seed sdcpp with the Apache-2.0 Z-Image-Turbo set at install; any Vulkan-capable box
can opt in (`OFFLOAD_WITH_MEDIA=1` at install). Warm-batch below is ComfyUI-engine only.

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

### Never post a graph to `:8188` directly ⛔

Every render goes through the harness — `generate-image`, `generate-video`, `run-graph`, or
the MCP tools. A raw `POST http://127.0.0.1:8188/prompt` skips the
[machine-wide GPU lease](architecture/decisions/0018-machine-wide-fenced-gpu-lease.md), so it
can start a diffusion run on top of a live llama-swap tier or a second render — the exact
collision the lease exists to prevent. The lease is a **convention**: it binds only code
paths that take it, so a bypass is not refused, it is simply unprotected.

Same class, same rule: **any tool that drives ComfyUI directly** bypasses the lease unless the
harness mediates it. That includes the official Comfy-MCP server
(`Comfy-Org/comfy-mcp`) — do not point one at this box's `:8188`.

**It also includes this repo's own `tools/comfyui` CLI (`comfyui-pp-cli`)**, named here so the
rule is not one the repo quietly violates. That binary defaults to
`http://127.0.0.1:8188` and POSTs graphs without taking the machine-wide lease (its
`graph_sha` dedupe is a submit-idempotency mechanism, not an arbitration one). Its SUPPORTED
posture is as the harness's submit backend: `render/comfy-submit.mjs` auto-upgrades to it when
the binary is present, and on that path the harness has already taken the lease, so the CLI
inherits it. Invoking `comfyui-pp-cli` **standalone** against a live box is the bypass — wrap
it in `gpu reserve --class media` if you need to.

Two escape hatches, both of which keep the lease:

```powershell
# a graph the templates do not cover — run-graph takes the lease for you
local-offload run-graph --graph wf.json --out-dir out/

# you genuinely need a bare server (benchmarking) — hold the lease explicitly
local-offload gpu reserve --class media -- <your command>
```

If you started ComfyUI by hand, check that the process owning `:8188` is the one you think
it is before rendering: a flagless instance can keep the port while a correctly-flagged one
fails to bind, and nothing reports it (its argv carries no `ComfyUI` substring, so a naive
kill-filter misses it).

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

For edits that have **no drawable region** ("make it snowing heavily", "turn the leather
into fur") the third edit route is the maskless **generative instruction edit** — MCP-only
(`offload_edit_image_generative`, no CLI verb), bound per machine by the `gen_edit_*`
config keys and unbound by default. See
[systems/media-generation.md](systems/media-generation.md).

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
| `general` (fallback) | anything; today's full capability-gated set | all enabled tools |
| `edit` | a focused code edit in an existing repo | `list_dir`, `read_file`, `search_files`, `edit_file`, `write_file`, `update_plan` |
| `build` | edit-then-verify (needs `--allow-run` / `--allow-shell`) | edit set **+ `run` / `run_shell`** |
| `research` | find + read sources (needs `--allow-search`/`--allow-fetch`) | `web_search`, `web_fetch`, `summarize_file`, `read_file`, `list_dir` |
| `github` | prepare files then publish (needs `--allow-github`) | edit set **+ `github_api` / `github_create_repo` / `github_upload_file`** |

`general` is the FALLBACK, not necessarily your box's default: with no `--profile`, the resolution
is explicit > config `agent_profile` > `general`, so a tier that seeds `agent_profile` (today
`ampere-6` seeds `research`) uses that instead. `local-agent` prints which profile it applied and
whether it came from the flag or the config; `agent_run` reports it in the response.

`--two-tier` conflicts with any **non-default** `--profile` (two-tier picks the architect/editor
toolsets itself); the CLI rejects that combination, while `--profile general` or an empty value
coexists.

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
- `--ctx-tokens` (**default 0 = AUTO**) — the served model context window the loop's transcript
  compaction budgets against. At 0 the loop **probes the serving endpoint** for the planner model's
  live `n_ctx` (`/upstream/{model}/props` on llama-swap, `/props` on a bare llama-server) and falls
  back to 8192 only when the endpoint will not answer — an *assumed* window killed real runs with
  `exceed_context_size` 400s (ADR 0015). An explicit value **overrides the probe** and is warned
  about when it exceeds the served window; set one only to match a tier's `--ctx-size` deliberately
  (the install prints the profile's value). The derived usable **input budget** is
  `ctx-tokens − max-tokens − 512`. Setting it too high lets the transcript overflow the real window
  (a 400); too low compacts sooner than necessary.
- `--gcf-compact` (default **ON** — measured flip decision 2026-07-24) — the compaction ladder's LOSSLESS first rung: over budget, older
  tool results that are JSON arrays of flat objects are re-encoded columnar (keys stated once,
  `internal/gcf`, round-trip proven — nothing is lost) before any lossy rung runs. The same
  transform guards the offload pipeline's context trim via the `gcf_compact` config field: an
  over-budget input's JSON is compacted losslessly before the head/tail cut, converting would-be
  truncations into full-fidelity completions.
- `--skeleton-prune` (default **ON** — same 2026-07-24 flip decision) — the next, lossy-structural rung: over budget, older tool
  results are reduced to signal-preserving **skeletons** (head/tail lines + error/failure/warning
  lines kept, elided runs replaced by `[... n lines elided ...]` markers) before the existing
  bare-marker and turn-drop rungs run. Deterministic and local — no model call, no added latency.
  Long multi-step runs keep *what went wrong earlier* visible to the model instead of losing whole
  older results at the first budget crossing.

### Unattended runs: risk rules, parking, and the advisory judge

The CLI is non-interactive, so every broker "ask" is **deny-and-queue** — deferred approvals and
parked calls land in the ask queue (`--ask-queue`, default `~/.local-offload/agent-asks.jsonl`) for
your morning review. Three mechanisms bear on an unattended run, but **only the first actually
gates** — the other two report:

- `--rules <file>` — a versioned JSON array of **structural, tighten-only** policy rules
  (`{kind, glob, decision, severity, reason}`): action kind + a glob over the worktree-relative
  path (write/delete) or host (fetch); `decision` must be `deny` or `ask`, never allow. A built-in
  floor already denies secret-material paths (`.env*`, `*.pem`, `*.key`, `id_rsa*`, `id_ed25519*`).
  A bad or missing file fails the run rather than silently deactivating the table; the audit trail
  records which rule fired at what severity.

  **With the flag empty, an unattended run loads a built-in default table** (0.55.0; embedded in
  the binary from `internal/agent/unattended-rules.json`, so no path and no installer step): every
  `delete` queues for your review, behind hard denies (write AND delete) for evidence files
  (`*.jsonl`), model weights (`*.gguf`/`*.safetensors`) and worktree-root CI workflows; lockfile
  and `go.sum` **hand-edits** hard-deny (lockfile *deletes* queue like any other delete); config
  and dependency-manifest writes (`config.json`, `settings.json`, `go.mod`, `package.json`,
  `requirements.txt`, `*.yaml`, `*.toml`, …) queue too — note this includes **creating** a new
  config file, so a task like "scaffold a compose file" produces a queued ask rather than a file.
  Ordinary source writes stay governed by your `--allow-*` posture flags, so the agent can still
  do the work you granted, and the table only sees the write/delete tools — file operations run
  *inside* the shell/run cage are governed by the OS cage, not by rules. Two explicit outs:
  `--rules <path>` **replaces** the default with your own table (replacement is what lets you
  loosen the delete catch-all — rules themselves only tighten), and `--rules off` runs ungated
  exactly as before 0.55.0. An alternative starter is `examples/agent-rules.json` — it
  additionally queues source overwrites (`*.py`/`*.go`/`*.ts`/`*.js`/`*.mjs` → ask) and every
  fetch, but covers fewer config/manifest globs than the default. It is a **repo file that no
  installer packages**: from a repo checkout, `local-agent --rules examples/agent-rules.json …`;
  from an installed binary (`$OFFLOAD_HOME\harness\local-agent.exe`), copy the file somewhere
  durable — e.g. `~/.local-offload/agent-rules.json` — and pass that path.
- **Risk parking (reporting, NOT a gate — measured 0% recall)** — every effectful tool asks the
  model to self-annotate `security_risk` (low/medium/high). A call flagged `high` (or with an
  unrecognized value — fail closed) is **parked**: never executed, recorded in the ask queue, the
  run continues without it.

  **This is a backstop, not a gate — do not rely on it.** The mechanism above is real and fires
  as described, but the *input* it depends on was measured to be a literal constant. Pooled over
  BOTH production agent seats and five arms (137 effectful calls), **83 of 83** emitted
  annotations were `low` — zero medium, zero high — *including all 81 structurally destructive
  calls*, and it held under an escalated arm (deleting an entire source tree still self-reported
  `low`). Park-gate recall was **0/81**, and the annotation was least likely to be emitted on the
  calls with the largest blast radius (`web_fetch`: 0/6 annotated). Treat the model's
  self-annotation as telemetry, never as a security control.

  The control that actually fires is the **structural rule table**, which is evaluated against
  tool + arguments rather than against the model's opinion of itself — and since 0.55.0 it is
  **on by default for unattended runs** (see `--rules` above). The ungated posture still exists,
  but only behind the explicit `--rules off`.

  **The `UNGATED` note — when you will see it, and when you will not.** It is not a general startup
  banner. It is appended by the agent builder and printed to stderr by the CLI
  (`[local-agent] UNGATED: …`) on exactly one condition: the run is **unattended**, holds at least
  one of `--allow-write` / `--allow-delete` / `--allow-overwrite` / `--allow-shell` /
  `--allow-github`, **and** you passed `--rules off`. `--allow-run` alone does not raise it (the
  table cannot gate cage execution anyway), an attended run never raises it, and an empty
  `--rules` cannot raise it any more (the default table loads instead, announcing itself with a
  `default unattended rule table ACTIVE` note when `--allow-write` is granted). It is a
  **note, never an error** — the run proceeds; you asked for exactly that.
  **Scope:** this is the CLI/queue path, the path that grants `--allow-*` and sets unattended. The
  MCP `agent_run` front door passes no write/delete/shell/fetch capability at all, so it never
  raises the note and none of this changes what an MCP caller can do.
- **Effect accounting + the judge** — every tool call gets an honest effect status
  (`committed`/`failed`/`unknown`/`none`); the CLI prints the non-committed records at the end of a
  run, and `agent_run` returns them (`effects`, `effects_flagged`). `agent_run judge=true` adds one
  end-of-run **advisory** completion grading the flagged records for your review — it never gates
  anything.
- **Environment rules (`agent_env_rules`, 0.113.22)** — a per-box table that shapes how the SEAT
  behaves inside the loop: `deny_tools` / `allow_tools` (withheld from the offered tools),
  `max_calls_per_tool` (the N+1th execution is refused with a reason and the tool is no longer
  offered), `arg_limits` (whole-number caps on numeric arguments, e.g. `{"read_file":{"limit":400}}`;
  the result tells the model what was capped), `max_observation_tokens` (one tool result bounded,
  head+tail, minimum 64), `observation_strip` (regexps removed from tool results), `rewrite_error`
  (a tool error matching a pattern becomes a short line the model can act on). Start from
  `examples/agent-env-rules.json`; a bad table fails by name (`local-agent` exits 2, `agent_run`
  defers, a fleet node defers with `defer_class: config`). Try a candidate without touching the
  config: `local-agent --env-rules candidate.json …` (`--env-rules off` runs with none). Every
  agent result reports `rules_fired` and a per-call `trace` (tool, status, bytes read, rule) — the
  same fields land in the delegation-log corpus, so a night of ordinary traffic says which rule a
  seat needs. These are NOT the risk rules (`--rules`), which gate effects; env rules never grant
  or deny an effect. ADR 0036; details in [systems/coding-agent.md](systems/coding-agent.md).

Details and rationale: [systems/coding-agent.md](systems/coding-agent.md).

### The timeout chain, and walls sized from the seat's own rate (0.115.21, register D-03)

One contract crosses six clocks. None of them is derived from another; this is where each one is set and what it
does NOT contain:

| clock | set by | contains | does not contain |
|---|---|---|---|
| `timeout_sec` (contract; default 300, box `agent_timeout_sec`) | the caller | the node's whole run: probe, build, loop, structured re-pack | placement wait, admission, cold load, queue time |
| delegator poll | **held open while the node reports progress** (0.131.0): the poll stays alive while the node's `progress.last_progress_ms` + its `stall_allowance_sec` + grace is in the future, bounded by the node's `ceiling_sec`; a node that reports none is polled as before, and a stale report is not extended (the deadline reason then says when the node last moved and what it was allowed). Otherwise `timeout_sec` + a grace window — for an UNSIZED (`timeout_auto`) contract, the bound the target node's advertised `seat_rate`/`seat_budget` imply, by the node's own arithmetic, plus a 300 s allowance for the node's admission (cordon, pre-flight, cold load, coherence probe — all spent in state `running`) until the node publishes a started `wall_sec`, at which point the clock is re-anchored on that wall start and the allowance drops; the 900 s cap when the node advertises no rate, or when the run is placed on a composite LAYER seat whose rate health does not publish (D-116); C-27 credits back intervals the node PROVABLY spent queued (both endpoints observed `accepted`), bounded at `min(poll bound + grace, 5 min)`; a job that never started is a `queue deadline` FAILURE, never a `budget` defer | waiting for the node's answer | the capacity wait (`agent_placement_wait_sec`, `results[].capacity_wait_sec`) |
| admission + warm-up (`agent_admission_wait_sec`, default 300) | the node, BEFORE its wall starts (D-64) | another model's swap on the endpoint, then the seat's own cold load (`admission_wait_sec`, `admission_note`) | anything after the first token |
| coherence probe (`agent_coherence_probe`, default `cold`) | the node, after the warm-up and still BEFORE its wall (D-118) | one ≤ 96-token completion asking the freshly loaded seat to call `read_file` and answer DONE (`coherence_note`; its time is added to `admission_wait_sec`) | the loop, the re-pack, anything the contract asked for |
| node wall (0.131.0: the EXPECTATION, ADR 0055) | `timeout_sec` / the auto wall, reported as `wall_sec` | what the node expects the run to take — what the delegator sizes and anchors from | **it ends nothing any more.** Until 0.130.x it was a context deadline over the loop, and a 27B seat streaming its 949th token at 984 s died at the 900 s cap exactly like a hung engine |
| **stall allowance** (0.131.0) | the node, per phase, from the seat's measured rates | how long the seat may go without a progress event — a streamed token, a tool call, a phase change — before the run is filed `stalled: no progress for Xs in <phase> (allowed Ys: …)` as **infrastructure**: admission → the admission budget; prefill → `prompt tokens ÷ prefill_tok_s × 1.5 + 30 s` (100 tok/s assumed until the seat-rates store has measured it — from the time to first delta, any engine); decoding → 20 deltas at the decode rate; a tool → its own cap + 30 s; the re-pack → 120 s; floor 60 s | the ceiling; a producing seat, however slow |
| **ceiling** (0.131.0) | the node: `max(3 × estimate, 2 × wall, 1800 s)`, cap 4 h (`ceiling_sec`) | the safety net over everything; a run still producing when it passes is filed `ceiling Ns reached while producing (T tok at R tok/s)` as **budget** — the sizing signal | — |
| loop budgets | `agent_max_tokens` per step (default 1,024; 4,096 on a thinking seat), the final answer at 4× (cap 8,192) **narrowed to what the remaining wall can decode** (0.122.1, D-95), 12 steps (`max_steps`, cap 12 remote), the forced final step (D-89) | one completion each | the stall allowance and the ceiling — a step that generates for minutes is progress, and only silence or the ceiling ends it (0.131.0) |
| engine + lease | the client's request timeout (split into connect / first token / stream, `llamaclient`), the GPU lease TTL (3,600 s default) against the media timeouts (`imagegen_timeout_sec` 600, `videogen_timeout_sec` 5,400, `gpu_wait_ms` 600,000 — C-33: a 5,400 s video run outlives the default lease; size the lease `--for` window to the job) | one request / one lease | — |

**Reading a run while it runs (0.131.0).** `gpu status` and `offload_status.gpu_lease.activity.runs[]` carry one
liveness line per run — `producing 3.4 tok/s, last token 2s ago (allowed 60s in decoding)` while the seat streams,
`silent 187s of 214s allowed in prefill` while it does not — beside the holder heartbeat (they are different things:
register C-32). `/fleet/jobs/{id}` publishes the same as `progress`. A `stalled:` reason means the SEAT went quiet
inside its allowance (check the seat, not the contract); a `ceiling` reason means the run was still producing at the
safety ceiling (size the contract, or split it). The seat-rates store's new `prefill_tok_s` is what sizes the prefill
allowance; until a seat has measured one, 100 tok/s is assumed and the reason says `assumed`.

**`doctor` names a clock that has drifted.** `local-offload doctor` prints a `config findings` section — one
`FAIL` row and a non-zero exit per value that loads and then cannot do what it says — and two of its rows are
clocks from this table: `gpu_wait_ms` more than 3x `vision_gpu_wait_sec` (register C-33: the deployed 600,000 ms
against a 90 s vision wait is ten minutes of blocking on every image/video/audio/run-graph call, against that
key’s own documented 90 s design), and one row per RETIRED key the file still carries — `videogen_wait_ms`,
`audiogen_wait_ms` — which until now was a single stderr note at startup that scrolls past every command.

**Sizing rule.** A wall is worth `cold load + (thinking auto ? one think block at the step budget : 0) + (steps − 1) ×
(128 tokens + 6 s prefill) + final budget`, all at the seat's decode rate. The node computes exactly that before every
run and publishes it beside the result: `results[].wall_estimate_sec`, `min_turn_sec` (cold load + one turn at the final
budget — the least a retry is worth, D-46) and `wall_note` (the arithmetic, or why there is none). `seat_tok_s` is the
run's own measured rate (completion tokens per second of call wall over the completions that generated ≥ 1,024 tokens;
tool-call completions of 25–60 tokens are prefill-dominated and excluded), `calls[].ms` the wall of each completion. The
rate and the cold load are remembered per seat in `<state root>/seat-rates.json` (the GPU-lease root: machine-local by
design; EMA 0.3 on the rate, the slowest of the last five loads, written under an exclusive lock file so two
processes on one box never drop each other's sample); until a seat has a sample, `agent_seat_tok_s` in the
box config stands in, and with neither the note says so and no numbers are published. **The estimate never changes an explicit
wall** — a contract that names `timeout_sec` runs under it exactly as before. A contract that names NONE is sized by it
(0.126.0, register D-03): intake stamps `timeout_auto`, the executing node clamps this estimate to 300..900, runs under
that and reports it as `results[].wall_sec` (`wall_note` prefixed `auto wall`), and every retry carries an explicit
remainder instead; a seat with no rate yet runs the 300 s default. **The delegator polls it at the node's wall, not at
the cap** (register D-116, and the paid-down "accepted cost" of 0.126.0 — a node that acked and then died silently used
to be abandoned 900 s later even when its own wall had been 300 s). The delegator's BUDGET — the retry remainder, the
re-placement ledger — still holds the cap open, because the node may legitimately size anything up to it; its POLL CLOCK
is sized from that node's `/fleet/health` `seat_rate` + `seat_budget` through the same function the node sizes its wall
with (`seatrate.AutoWallFor`, clamped to the same 300..900), so the two clocks cannot drift. The node's own number
overrides the estimate the moment a running poll publishes it: `/fleet/jobs/{id}` now carries `wall_sec` while the job
RUNS, and the delegator RAISES its bound to it — it may never lower it, because abandoning a job the node is still
running inside its own wall is the one failure this must not buy. The deadline message names the bound it used (`poll
bound: sized from <node>'s seat_rate 30.0 tok/s (5 samples): 612 s` / `poll bound: the node's own wall 720 s` / `poll
bound: cap: no seat rate advertised by <node>`), and the same line rides every result as `results[].poll_note`. A
contract that names its own `timeout_sec` is polled exactly as before, with no note. The `queue` route still polls at the
cap: its claimant is not a node the delegator chose, so there is no health view to size from.
For an explicit wall, `wall X s is BELOW the estimate` in `wall_note` (and
the node log) is the caller's signal to size the contract. A contract with an `output_schema` carries one more term
(0.117.2): `+ re-pack ≤ N tok` — one final-budget completion for the structured re-pack of a prose answer, an upper
bound that a seat answering in the object shape never pays (a 285 s re-pack on the 27B sat outside every floor on
2026-09-10). The retry floor on the delegator is the RETRY seat's (0.117.2, register D-46): `max(agent_retry_min_sec,
min_turn of the seat the retry lands on)` — a remote node's `/fleet/health` `seat_rate` (its remembered rate and cold
load) at its own `seat_budget`, the local seat's `seat-rates.json`, plus the re-pack term for a schema contract; the
retry note names the publisher, and only a node that publishes no rate falls back to the first attempt's
`min_turn_sec` (the 2026-09-10 shape: a 201 s floor from the 4B cleared, the retry landed on the 27B whose own floor
was ≈ 484 s). `seat_budget` is also what a caller reads to MATCH budgets across seats — a delegator's config does not
travel with the contract. Reference (2026-09-10, ledger-01 on both
seats): the Qube 27B TP2 seat at ~30 tok/s needs ≈ 600 s thinking off / ≈ 730 s auto INCLUDING a 210 s cold load for a
12-step, 8,192-token-final contract — a 600 s box default is at the edge and 900 s is the honest wall; the Lenovo 4B at
~30 tok/s answers the same contract in one step in 250–380 s with a 34 s cold load.

**One question before the wall: the seat coherence probe (register D-118).** A seat can be HEALTHY by every gate the
harness had and still be numerically broken. On 2026-09-16/17 the blackwell-16 vLLM seat (Qwen3.8-27B GSQ, `fp8_e5m2` KV
through FlashInfer) passed `/health`, `/v1/models`, the speed probe and the READY smoke, then answered every contract
with `<tool_call>!!!!!!!!!!!!!!!!!!!!…` to the token cap — 126–336 s of degenerate output per contract, all of it filed
as `unparsed_tool_call`, and two GPU leases spent on parser hypotheses before anyone read a raw completion. So after the
warm-up and still OUTSIDE the wall, the node asks the seat one bounded question: *read `notes.md` with the `read_file`
tool, then answer DONE*, at most 96 tokens, thinking off, the node's own sampling, over the same client the loop uses.

Cost: **one ≤ 96-token completion per cold load** under the default `cold` policy (`always` probes every run, `off`
never). What comes back is reported as `coherence_note` on the wire, in one of three shapes:

| verdict | what the seat did | what happens |
|---|---|---|
| `coherence probe: tool call parsed in Ns` | the seat decoded, the template rendered, the server parsed the call | the run proceeds |
| `coherence probe: answered in text without a tool call in Ns (proceeding)` / `… inconclusive (…); proceeding` | plain prose instead of a call, or the seat could not be reached at all | the run proceeds — the probe never turns silence into a defer |
| `coherence probe: cut inside the think block at the 96-token cap (…; proceeding)` | a thinking seat spent the probe's 96 tokens in its hidden `reasoning` / `reasoning_content` channel | the run proceeds — a think block cut by the cap is not an incoherent seat |
| `seat incoherent at warm: …` | ≥ 20 identical non-whitespace bytes in a row, an unparsed tool-call marker with no parsed call, or nothing printable (empty or whitespace-only) at the cap **and no reasoning channel reported** | the contract defers `infrastructure` after SECONDS, and `agent_delegate` re-places it on another node |
| `seat incoherent at warm: … (remembered from this seat's probe Ns ago…)` | a WARM contract on the seat this process already caught, still resident | the contract defers on the remembered verdict, spending no probe at all |

The defer is the one `infrastructure` defer the delegator retries elsewhere: the fault is a property of that seat and
it was caught before the contract's WALL started. The budget is delegator wall clock, so what the node spent getting
there — cordon, pre-flight, the cold load that triggered the probe (125–250 s for a vLLM seat) and the probe itself — is
credited back to the subtask's `timeout_sec` ledger from the `admission_wait_sec` the node reports, exactly as a
capacity wait is credited; without that credit the retry floor (the alternate seat's own `min_turn`) would refuse the
retry this defer exists for. It is still a broken stack — `--route remote` exits non-zero and an operator has to fix the
box (on that seat the fix was `kv_cache_dtype: fp8` instead of `fp8_e5m2`; see ADR 0048 Amendment 1).

Note what `cold` does and does not cover: it protects the contract that LOADS the seat, and the defer unloads nothing,
so the broken seat stays resident. The node therefore REMEMBERS a broken verdict for that endpoint+seat for 10 minutes
and defers the next warm contract on it without spending a probe. The memo lives in the node process, and it is dropped
by the seat's next cold load, by any later probe that is not broken, and by its own TTL — so a seat you fix comes back
by itself. Set `agent_coherence_probe: "always"` on a box whose seat has gone NaN under it before and you get the probe
on every run instead, at one ≤ 96-token completion per contract.

**The final budget fits the wall (0.122.1, register D-95).** Sizing told the caller a contract would not fit; it did
nothing about the run in flight, which still opened its final answer at the configured 4× budget. On the Lenovo 4B seat
(~15 tok/s) a list-heavy extraction with an `output_schema` owed an 8,192-token final PLUS an 8,192-token re-pack — the
node's own `wall_note` priced that at 1,166–1,310 s against a 900 s wall, and on 2026-09-14 `METHODOLOGY.md` and
`SELF-CONTROL.md` hit the wall instead of answering (at the 4,096 budget the same contracts were CUT instead: three of
ten deferred `output_truncated`). The same arithmetic now runs backwards, before the answer is asked for:

```
fit   = (remaining_wall − other − safety) × tok_s / turns
final = min(configured final, fit), floored at 1,024, never raised above the configured cap
```

`turns` is **2** when the contract carries an `output_schema` (the final answer and its structured re-pack are both
decoded on this seat inside this wall) and 1 otherwise; `other` is everything the run still owes that is not the answer
— the estimate's cold load + think block + tool steps (`wall_estimate_sec`'s own non-final terms) at run start, and **0**
at the forced final step, where those are already spent; `safety` is a tenth of the remaining wall, never less than one
transcript prefill (6 s). It is computed at run start and again at the forced final step, off the LIVE clock. Worked
example, the METHODOLOGY case: a 900 s wall, 15 tok/s, a schema, 12 steps, 2,048-token steps — `other` = 331 s (34 s cold
load + 137 s think block + 160 s tool steps), safety 90 s, so `fit = (900 − 331 − 90) × 15 / 2 = 3,592 tokens`, and the run
then owes 331 + 2×239 = **810 s of its 900 s wall** instead of 1,423 s. Results publish `final_budget_fit` and
`budget_note` ("final 8192 → 3592 to fit 900 s at 15.0 tok/s (split with the output_schema re-pack)") on the node and the
delegator wires. A seat with **no measured rate**, or a wall with room, publishes neither and runs byte-for-byte as
before — the fit is a ceiling, never a raise. When even the floor does not fit, the floor stands and the note says `the
wall will be the stop`, which is today's behaviour, named.

**A cut final on a schema contract is re-issued once with list caps (0.122.1, register D-95).** 0.115.23 (D-91) refuses
to re-pack a `length`-cut final — a partial cannot be re-packed into the requested object — but abstaining there threw
away a run that had read the whole document and only over-answered: nothing had told the seat how long its lists could
be. The loop now asks ONCE more, thinking off, at the same budget, with the caps spelled out: *cap every list at N items
(keep the most important ones and drop the rest), and keep every string under 200 characters*. **N is the smallest
`maxItems` anywhere in the contract's schema, and never more than 8** — a schema the author shaped is honoured, never
broken by the retry, so set `maxItems` on your arrays when you know the bound. Two conditions gate it, so it can never
re-create the shape it fixes: an `output_schema` is set, and the wall still holds one turn at the seat's measured rate
(`min_turn`, the re-pack term included). It is bounded at ONE: a seat that cuts the capped answer too abstains exactly
as before, with `final_reissue=list_cap` and BOTH `finish_reason`s on the record (`calls[]`, and named in
`repack_note`), so a first cut and a second are never read as the same event.

**The re-issue no longer asks the partial to be JSON-shaped (0.123.3, register D-95b).** 0.122.1 gated it on the cut
answer starting with `{` or `[`, on the reasoning that "cap every list" says nothing to a truncated narrative. On the
Lenovo 4B that excluded every run it was built for: the seat answers a schema contract in its OWN prose shape
(`summary (≤100 words):` / `mechanisms:` / `- item`), which the structured re-pack reads perfectly well, so the live
readback of `METHODOLOGY.md` on 2026-09-14 still deferred at 381 s with `output_truncated` and no re-issue at all. Any
`length`-cut final on a schema contract now earns the one re-issue; the wall gate is what bounds it.

**A repetition loop is a cut answer (0.123.3, register D-95b).** The same readback showed the second half of the
failure: the answer had degenerated into a LOOP — one four-line block under `numbers:` repeated about twenty times
until the budget ran out — and the engine reported it as an ordinary completion, so nothing downstream could tell it
from an answer that merely ran long. The loop now reads the text it was handed:

- **The rule.** Take the final's non-blank lines, normalised (trimmed, inner whitespace collapsed, case-folded). If the
  TAIL of the answer is the same block of `p` lines repeated **4 or more times in a row** (`p` from 1 to 32 lines, the
  smallest period wins, and the last block may be cut off mid-way — that is how a budget ends), the answer is a
  degenerate loop. Four is the threshold: three repeats are legitimate parallel structure, and the measured failures
  repeat twenty times or more. A long list of DISTINCT items never matches — the detector keys on repetition, not on
  length.
- **What happens.** The final is treated as CUT even when the engine said `stop`: `stop_note` carries
  `repetition loop (20× "- 100 words cap for summary (enfor…")`, `output_truncated` is set, and the run re-issues ONCE
  with the list caps PLUS *"Your previous answer got stuck REPEATING the same lines … write each list item exactly once
  and then stop."* A second loop abstains exactly as a second cut does. `calls[].finish_reason` is **not** rewritten —
  it stays what the engine reported; the guard is the loop's reading of the text, not the seat's report of itself.
- **What the caller sees.** The repeated tail is trimmed off the partial that rides in `output`: one copy of the block,
  then `[repetition trimmed ×19]`. Everything before the loop survives byte for byte.

**`agent_sampling` / `agent_sampling_final` (0.123.3, register D-95b).** The agent client has always sent
`temperature: 0` and no other sampling key — right for tool calling, and precisely the greedy decoding that makes a
small seat fall into the loop above. These two box-config objects give a MEASURED seat a different policy:
`agent_sampling` applies to planner (tool) calls on the executing seat, `agent_sampling_final` to the final answer turn
and its re-issues — the thinking-off prose turn, which is where a model family's non-thinking recommendation actually
applies. Both take `temperature`, `top_p`, `top_k`, `presence_penalty` and `repetition_penalty`, all optional; a knob
you do not set is ABSENT from the request, so the seat keeps its own default, and an absent object is today's behaviour
byte for byte. An out-of-range value fails the config LOAD, named by its key, rather than as a 400 mid-run. The
effective policy of every completion is published in `calls[].sampling` (`temperature=0` for the default), so a
measurement can prove which decoding produced which answer. **There is no house default and no recommended value
here**: Qwen3.5's own non-thinking card says temperature 0.7 / top_p 0.8 / top_k 20 / presence_penalty 1.5, and that is
a number to measure on a seat, not one to inherit fleet-wide.

### What is the harness doing on the cards? (0.117.0, ADR 0041)

"Busy" is not an answer. `local-offload gpu status` (and `offload_status.gpu_lease`) now say what the cards
are DOING: a one-word `verdict` — `working` (a request or a registered agent run is in flight on the seat),
`held-working` (a lease is held and the cards are busy under it), **`held-idle`** (a lease is held and nothing
is running: seat idle, cards quiet — the holder is draining, queued, loading, or stalled), `loaded-idle`,
`busy-outside` (no lease, cards busy with work the harness does not own), `stale-holder`, `free` — and an
`activity` block with the seat's load state and in-flight count, every registered run (kind, pid, origin,
goal excerpt, phase, step, tokens, age), a utilization/memory sample per card with the processes on them,
and the holder's command (the wrapper form stamps its argv). Every agent loop registers itself in
`<state root>/gpu/activity/` before admission and updates the record per step, so a drain or a status reader
sees a run between its steps, when the engine's own gauge reads zero.

**The seventh clock: the drain.** `gpu reserve --drain` waits until the seat's gauge AND the registry are
empty on two consecutive reads. Its deadline is the rest of `--wait` (from when the reservation began
queueing; floor 2 min) unless `--drain-timeout` is given — a fixed two minutes failed twice on 2026-09-14
under one legitimate 27B step of 3m27s. While it drains the lease is stamped `draining` (new runs hold at
the cordon for their admission budget and defer `capacity`; in-flight runs finish their steps) and turns
`exclusive` only once idle. Progress prints on change with the seat's own turn arithmetic
("one seat turn is ≈ 174 s at 23.6 tok/s"), never a line per tick.

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
to `installed.json`. The install seed also binds the agent's planner seat automatically: config
`agent_model` comes from the profile's explicit `config_seed.agent_model` when it names one (the
measured seat for that tier), and otherwise is DERIVED from `resident_tier` when that differs from
the workhorse. So running the agent no longer requires `-model <resident_tier>` — the `-model` flag
remains an override. `agent_max_tokens` (0.113.9) is the planner's completion budget per call for `agent_run`
and for delegated jobs this node serves (0 = the loop default of 1,024) — set 4096 for a THINKING seat, whose
reasoning spends the same budget (the Qube 27B seat used 839 reasoning tokens of 1,024 and returned nothing,
2026-09-04). A key named `max_tokens` is NOT read: the loader warns `unknown config key`. `agent_thinking`
(0.115.8) is the seat's think-block policy on planner calls: `auto` (default) thinks every step and, when a
step ends EMPTY (no content, no tool call — a think block that ate the budget, or a bare close), re-issues that
same step ONCE with thinking off at 4× the step budget (cap 8,192), then stops the run as `reasoning_starved` /
`empty`, which the node reports as a defer with the arithmetic in `stop_note` and per-call `calls[]` — never as an
empty answer; `off` renders every planner call in non-thinking mode (`chat_template_kwargs: {"enable_thinking":
false}`, the same knob the structured re-pack sends) for grounded extraction on a seat measured to starve; `on`
never sends the kwarg. A contract's own `thinking` overrides the box. `agent_sampling` and `agent_sampling_final`
(0.123.3) are this seat's decoding policy — planner calls and the final answer turn respectively — each an object of
optional `temperature` / `top_p` / `top_k` / `presence_penalty` / `repetition_penalty`; absent or empty means the
historical request (`temperature: 0`, no other key), an unset knob is absent from the body so the seat keeps its own
default, an out-of-range value fails the load by key, and the effective policy of each completion is published in
`calls[].sampling`. They carry no house default on purpose: a sampling setting is a per-seat measurement (see "The
timeout chain"). Until 0.115.8 the loop raised the budget
4× on a starved step, nudged once with a user turn and then accepted a second empty as `done` (1× + 4× + 4× the
budget for nothing; 2026-09-10 retrospective D-01). `agent_retry_min_sec` (0.115.9) is the least `timeout_sec` budget a subtask's cross-seat verification retry is
worth starting with (0 = the historical 10 s): set it to a cold load plus one turn at `max_tokens` on the retry
seat — 300 on the reference box — so a 296 s leftover no longer buys a thinking seat four minutes of think that
times out; the retry is also skipped after an empty final and never lands on a seat already running another
job (`retry_note` names each). `agent_lease_wait_sec` (0.113.14) bounds how long a local `agent_delegate` placement waits for a
foreign TEXT-class GPU lease (`gpu reserve --class text`) to clear before deferring — see the delegate section below. `agent_placement_wait_sec` (0.113.18) bounds the delegator's CAPACITY wait: a subtask every fitting node refused for capacity (queue full, leased, draining, shed), or whose only placement is a reserved seat, waits up to this many seconds (default 120; negative = off) re-reading the fleet's health and lands on the first node that frees — the idle time is not charged to `timeout_sec`; when nothing frees it defers with class `capacity`. `priority` on `agent_delegate` / `--priority` on `delegate` (`-1` sheddable, `0`, `1`) is the scheduling band a node claims by; sheddable work takes idle capacity only and is shed rather than waited (docs/systems/fleet-node.md, "Bands, tenants, saturation and the capacity wait"). `agent_spread_local_slot` (0.113.20) decides what `route=spread` does with the LOCAL rotation slot when the local seat is already busy at deal time: `skip-when-busy` (the default) deals it to the best-fit eligible remote with room, so several delegating sessions do not stack on one local seat; `always` restores the unconditional local slot (docs/systems/fleet-node.md, "The local slot under load"). A tier may also seed `agent_profile`, the box's DEFAULT agent tool profile when
a call names none (resolution: explicit `--profile`/argument > config `agent_profile` > `general`).
`ampere-6` seeds `research` because on that tier the same model scored 0% under `general` and 72%
narrowed — the profile outweighed the choice of model. `--two-tier` ignores the box default, since
it sets its own architect/editor toolsets. A seeded tier is the normal case wherever a bake-off has run: an explicit seed
is how a tier binds a planner that is neither its resident nor its workhorse. Run with `-ctx-tokens <agent_ctx_tokens>` matching the profile. These are **projected
defaults**; `selftest.ps1` measures on the real box and its `receipt.profile_measure.tuned` block
carries any measured override to apply.

| Profile | Resident/default tier | Served ctx (`-ctx-tokens`) | KV | 26B-A4B |
|---|---|---|---|---|
| `blackwell-16` / `volta-16` | `gemma4-26b-a4b` | 32768 | q8_0 | full-GPU resident |
| `blackwell-2x16` | `gemma4-26b-a4b` | 131072 | q8_0 | full-GPU resident; the served window follows the measurement (0.113.29, 32768 → 131072). Bound only for EXACTLY two Blackwell cards in the 12–23 GB band; three cards are `blackwell-3x16` (shipped 0.113.32) |
| `blackwell-3x16` | `gemma4-26b-a4b` (single layer) + the vLLM agent seat (pair layer) | 131072 (long twin 262144) | q8_0 | COMPOSITE (ADR 0052): a complete `blackwell-16` AND `blackwell-2x16` as well as itself. Placement decides per task which layer and seat run the work and records it as `placed`; the display card (device 1) is fenced by a ≥4 GiB floor, a host-RAM guard and an operator-presence guard, all failing closed, and its layer ships dormant. See [systems/composite-tier.md](systems/composite-tier.md) |
| `ampere-16` | `offload-e4b` (agent `qwen38-27b-agent` = Qwen3.8-27B UD-IQ3_S + embedded MTP head, `--ctx-size 49152`, `research`; 26B dropped) | 49152 | q8_0 | **SEAT RE-AUDITED 2026-09-16 (ADR 0047): the 4B is out.** Blind, 24 Opus judgements, 3 lenses, on a card at its accepted 40 W profile and cooled before every arm: Qwen3.8-27B UD-IQ3_S+MTP 9.32 beats the previous seat `qwen3.5-4b-vllm` (5.39) **24/24**, gemma-4-12B+MTP (6.17) 24/24 and gpt-oss-20b+EAGLE-3 (6.21) 24/24; the 4B took zero firsts and fourteen lasts. The 2026-09-14 "measured tie" is withdrawn — it ran on a stock, thermally throttled card (A-103) at a 1,024-token step budget with the 12B capped at 32k. Costs that ship with it: it pads (102 degenerate findings flagged) and decodes at 6.3 tok/s, so the tier seeds `agent_max_tokens` 4096 and `agent_timeout_sec` 900. The llama.cpp 4B entry stays as the fallback. Was: measured 2026-09-04 on an NVIDIA A2 16 GB at 40 W: the projected 26B-A4B seat ran 1/8 digests, the 4B seat 8/8 (0.113.15); 2026-09-06 the reference box's agent lane moved to a vLLM seat behind llama-swap (131,072 ctx). RENDERED by the installer since 0.113.33 and NOT persistent since 0.113.36 (`ttl: 300`, no preload, no boot-enable). Seat choice re-grounded on QUALITY 2026-09-08 (blind 4-way, 24 judgements: 4B-vLLM 7.75 > 4B-llama.cpp 7.38 > 12B-vLLM 6.50 > 12B-llama.cpp 6.21), not on wall time: ADR 0035, `setup/templates/vllm-seat/linux-systemd/` |
| `dual-gpu` | `gemma4-26b-a4b` (architect) + `offload-e4b` (editor), both resident | 32768 | q8_0 | resident (two-tier, **zero swap**) |
| `ampere-8` / `blackwell-8` | `offload-e4b` | 16384 | q8_0 | via `--cpu-moe` only when RAM ≥ ~56 GB; else dropped |
| `amd-rdna3` | `offload-e4b` (Vulkan) | 16384 (floor; canary → 32768) | f16 (floor; canary → q8_0) | `--cpu-moe` floor; canary → full-offload `-ngl 99` (~20–25 t/s on dual-channel DDR5) — see SETUP-AGENT.md, AMD RDNA3 chapter |
| `amd-rdna3-dgpu` | `gemma4-26b-a4b` (Vulkan, discrete RX 7900-class ≥12 GB) | 32768 | q8_0 | full-GPU `-ngl 99` resident |
| `ampere-6` | `offload-e4b` | 32768 | q8_0 (conservative default; f16 measured viable) | dropped (architectural — see the tier page) |
| `amd-gcn` | `gemma4-e2b` (Vulkan; CPU alt route `alt_backends: [cpu]`, ADR 0054; agent seat `qwen3.5-4b-agent`) | 32768 (8192 → 32768 measured 2026-09-20: 24k-token prompt in 278 s, 3.9 GiB GTT) | f16, flash-attn on (measured 2026-09-20 on binxarn: +4 % pp, neutral on Lucienne; the lane is DDR-bandwidth-bound, every RADV/ubatch/KV knob within ±4 %) | dropped |
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

### Parallel sessions on one llama-swap (0.111.0)

Several Claude Code sessions each run their own `local-offload mcp` process and fan out
(`runConcurrency = 4`) against the SAME llama-swap. llama-swap answers **429** once a model's
reserved requests (queued + in-flight) reach its `concurrencyLimit` (default **10**), queues
requests *silently* while it swaps a model, answers **503 "process is not ready"** while a process
is starting, and **500** with `src:"llama-swap"` when a health check times out. The harness treats
all of those as *peers hold the seat* ([ADR 0032](architecture/decisions/0032-a-peer-held-seat-is-waited-for-not-deferred.md)):

| key (`config.json`) | default | meaning |
|---|---|---|
| `seat_contention_wait_sec` | `0` → 90 s | one wait budget per agent contract, shared by every chat step and the re-pack; `-1` = never wait (first busy answer defers) |
| `agent_admission_wait_sec` | `0` → 300 s | pre-flight: wait while any model on the endpoint is mid-swap, then WARM the seat if it is not loaded (0.115.11: one passthrough GET makes llama-swap swap it in; a vLLM cold load is 125–250 s) — all BEFORE the contract's wall starts; `admission_wait_sec` / `admission_note` on the wire report it; `-1` = off |
| `agent_coherence_probe` | `""` → `cold` | post-warm SEAT COHERENCE probe (register D-118): after a cold load, ask the seat one ≤ 96-token question BEFORE the wall starts and defer `infrastructure` if it answers with the NaN shape. `cold` = only when this run loaded the seat, `always` = every run warm or cold, `off` = never. `coherence_note` on the wire reports the verdict |

What you will see on the wire and in the ledger: `contention_wait_sec` and `admission_wait_sec`
on every agent result; a defer whose reason starts with **`seat contended:`** when the budget was
spent (`grep "seat contended:" ~/.local-offload/ledger.jsonl`); `summary.batches` on research
calls that ran more than 8 pages; `summary.quarantined` when a fleet node was blocked for 30 min
after two off-document answers (its reason appears among `probe_errors` as "quarantined until …").

**Capacity is your step — the wait only makes contention honest.** The 429 exists because ten
reserved requests per model are shared by every session. Ready to paste into
`C:\llama-swap\llama-swap.yaml` (Qube; back it up first), on the seats the harness binds
(`agent_model` qwen3.8-27b; cascade gemma-4-e2b / e4b / 12b / 26b):

```yaml
  qwen3.8-27b:
    concurrencyLimit: 32   # was the implicit default 10; queues in llama-swap instead of 429
  gemma-4-26b:
    concurrencyLimit: 32
  gemma-4-e4b:
    concurrencyLimit: 32
```

Then the elevated restart (the `llama-swap` scheduled task is elevated; an unelevated stop is
denied while the OLD process keeps answering 200):

```powershell
Start-Process powershell -Verb RunAs -WindowStyle Hidden -ArgumentList '-NoProfile','-Command',
  'Stop-ScheduledTask llama-swap; Stop-Process -Name llama-swap -Force -ErrorAction SilentlyContinue; Start-ScheduledTask llama-swap'
# proof of apply: C:\llama-swap\llama-swap.log shows a fresh startup banner, then
Invoke-RestMethod http://127.0.0.1:11436/running
```

Raising `--parallel` on the 27B seat (4 today; the 2026-09-01 fairness arms measured ~110–120 tok/s
aggregate at 8 slots on the 5060 Ti pair) adds real throughput rather than queue depth — that is a
seat change and stays your call.

**Deploying this build:** `go build -o bin\local-offload.exe .` in the repo, then restart the
Claude Code sessions (each one spawns its own `local-offload mcp`; a running session keeps the
old binary until it restarts).

### Cache server — an optional second device holding evicted KV (0.112.0)

A vLLM seat's usable KV in VRAM is small on consumer cards (and on a Mamba-hybrid model the
"GPU KV cache size" banner overstates it: align-mode checkpoint blocks share the pool). Contexts
that leave VRAM are recomputed. With LMCache MP and a **cache server** — a store on a second
machine's RAM — they come back at parity cost instead (measured 2026-09-02, Qwen3.8-27B, 24k
tokens: 3.86 s from a Lenovo over the LAN vs 24.68 s recompute, every token from the store; 20.6 s
when the store namespace was shared across layouts), the GPU
stays free while the load streams, and they survive a seat swap. Same-box RAM as the tier is faster
(0.50 s, 49.7×) but spends the serving PC's memory; the second device is the capacity route.

**Off by default; nothing depends on it.** `kv_cache_server` is a LIST of per-seat bindings, one per
vLLM seat, and `vllm_seats` is this box's vLLM roster (0.121.0 — ADR 0045). Declare both only when a
second device exists:

```json
"vllm_seats": ["qwen3.8-27b-vllm", "qwen3.8-27b-vllm-3card"],
"kv_cache_server": [
  {
    "enabled": true,
    "store": "fs_native",
    "address": "/mnt/kvcache/lmcache-seat-tp2-fp8",
    "l1_staging_gb": 8,
    "chunk_size": 1568,
    "key_prefix": "seat-tp2-fp8",
    "seat": "qwen3.8-27b-vllm",
    "kv_dtype": "fp8",
    "tensor_parallel": 2,
    "status_file": "//wsl.localhost/<distro>/root/g7/seat-l2-qwen3.8-27b-vllm.status"
  },
  {
    "enabled": true,
    "store": "fs_native",
    "address": "/mnt/kvcache/lmcache-seat-3card-fp8",
    "l1_staging_gb": 16,
    "chunk_size": 1568,
    "key_prefix": "seat-3card-fp8",
    "seat": "qwen3.8-27b-vllm-3card",
    "kv_dtype": "fp8",
    "status_file": "//wsl.localhost/<distro>/root/g7/seat-l2-qwen3.8-27b-vllm-3card.status"
  }
]
```

- **One binding per seat.** A binding whose `seat` is empty is the BOX DEFAULT and backs every vLLM
  seat that has no binding of its own; an exact seat match always wins over it. Two bindings for one
  seat — or two box defaults — are refused at load: that is two stores whose order in the file
  decides which one the seat gets, not a merge.
- **The pre-0.121 single object still loads**, as a one-element list bound to its own `seat`. No
  deployed config has to change. The list is the shape to write from now on.
- **`vllm_seats` is declared, not sniffed.** `/v1/models` reports model ids, not engines, and the
  cascade deliberately stays on llama.cpp (Gemma-4 hybrids crash LMCache's V2 path, upstream #4263),
  so a sniffed roster would fail every seat that must never have a store. Empty = this box runs no
  vLLM seat and the gate below is inert.
- **`local-offload doctor` FAILS a vLLM seat with no binding**, one line per seat and a non-zero
  exit — the operator rule is that the store backs every vLLM seat while the second device is online:

  ```
  cache server (one binding per vLLM seat; the store backs every seat while the second device is online):
    qwen3.8-27b-vllm:        OK    fs_native /mnt/kvcache/lmcache-seat-tp2-fp8 key_prefix=qube-seat-tp2-fp8 l1=8GB
    qwen3.8-27b-vllm-3card:  FAIL  no kv_cache_server binding — give this seat a store, or opt out explicitly with {"seat":"qwen3.8-27b-vllm-3card","storeless":true,"reason":"<why>"}
  ```

  An explicit `"storeless": true` with a `reason` PASSES. Silence does not — neither an absent
  binding nor one merely switched off with nothing saying why.
- **`kv_dtype` + `tensor_parallel` declare the stack generation.** They are needed only when two
  seats SHARE a `key_prefix`: the load refuses a prefix shared across different generations, and
  refuses a shared prefix where any binding leaves the generation undeclared (it cannot be shown
  safe). Give each seat its own prefix and neither field is required.
- **`status_file` is the fs_native readback (0.129.1, register B-29).** An `fs_native` store is a mounted path with
  no port to dial, so `offload_status` cannot probe it — but the seat wrapper decides the fact at every seat start
  (mount + 64 MiB write probe) and writes the file `SEAT_L2_STATUS_FILE` names (`ok <stamp> mbps=<n>` or
  `degraded <stamp> reason=<why>`): `seat-l2-<seat id>.status` in a rendered seat env, `$WORK/seat-l2.status`
  only when the env sets none. A binding that still names `seat-l2.status` must be repointed after a re-render.
  Declare that file on the binding (on a WSL2 seat, the host-visible `//wsl.localhost/<distro>/…`
  path) and status publishes `reachable` true/false with `status_line` and `status_age_s`; undeclared or not yet
  written stays `null` with a note saying which.
- **Rendering a seat from the box's bindings:** `local-offload install vllm-seat --config
  <config.json> …` picks the binding FOR THAT SEAT by name and renders its adapter, directory,
  namespace, L1 size and chunk into `seat.env`; the tier keeps the mount point, the write floor, the
  prune target, the writer count and the cap, which are properties of the share rather than the
  namespace.
- **A seat that names a shipped chat template (`chat_template`, e.g. the blackwell-3x16 seat's
  `qwen3-fold-system.jinja`):** the render includes `templates/<name>` beside `<seat id>.env`, and
  `SEAT_EXTRA_ARGS` passes `--chat-template <WSL seat dir>/templates/<name>`. Copy the whole render —
  `templates/` included — into the distro's seat directory. `seat_fg.sh` refuses to start and names the
  file when it is missing, before the MP server is touched; vLLM would refuse the path too, but only after
  the wrapper had restarted the MP server, with the reason in a traceback and HTTP 500 on the lane.

The measured transport of choice is `fs_native` over a network share of the store's RAM disk (0.112.1+;
Lenovo tmpfs over SMB 3.1.1: a 23.7k-token prefix back in 0.56–0.70 s vs 3.8 s through Valkey, 2026-09-04).
The block then names the mounted path, and the seat's `seat.env` names the share the wrapper mounts first:

```json
"kv_cache_server": [{
  "enabled": true,
  "store": "fs_native",
  "address": "/mnt/kvcache/lmcache-seat-tp2-v2",
  "l1_staging_gb": 8,
  "chunk_size": 784,
  "key_prefix": "qube-seat-tp2-v2",
  "seat": "qwen3.8-27b-vllm"
}]
```

```sh
# seat.env (the wrapper mounts the share before the MP server opens base_path, and refuses to start without it)
SEAT_L2='{"type":"fs_native","base_path":"/mnt/kvcache/lmcache-seat-tp2-v2","num_workers":8,"use_odirect":false,"max_capacity_gb":38}'
SEAT_L2_MOUNT_SRC=//cache-server/kvcache   # a HOSTNAME this box resolves (tailnet MagicDNS or static DNS) — never a DHCP address
SEAT_L2_MOUNT_DIR=/mnt/kvcache
SEAT_L2_MOUNT_OPTS=credentials=/root/.smbcred,vers=3.1.1,rsize=4194304,wsize=4194304,cache=none,actimeo=1,noserverino,nobrl
SEAT_L2_MIN_MBPS=200                       # optional write floor: refuse to start on a crawling path (0 = off)
SEAT_LMCACHE_PYTHONPATH=/root/g7/lmcache-overlay   # optional: load LMCache from an overlay (an unreleased fix), not the venv
```

- `SEAT_LMCACHE_PYTHONPATH` (optional, 0.113.13): a directory prepended to `PYTHONPATH` for BOTH the LMCache
  MP server and the vLLM engine, so the seat imports LMCache from an overlay copy that carries a not-yet-released
  upstream fix, leaving the installed package untouched. Empty/unset = the installed package. The server and the
  engine must load the SAME LMCache (a mismatched serializer corrupts store/retrieve silently), which is why the
  one variable feeds both. Build the overlay from whatever LMCache is installed plus the fix, and REBUILD it after
  any `pip install -U lmcache` so the patch rides the new base; when the fix ships in a release, unset the variable.
  First use: LMCache PR #4253 (fp8-KV / FlashInfer store corruption on GDN hybrids, LMCache #4247).

- `address` must be private (LAN or tailnet); a public address is refused at load by key name, and so is
  a URL or host:port under `store: "fs_native"` (which takes the absolute path of the mounted export).
  Bulk KV prefers the direct LAN (WireGuard measured 6.6× slower).
- Name the store by a hostname, and re-measure the path after any network change. On 2026-09-04 a seat env
  that mounted `//<lan-ip>/kvcache` refused every start for hours after the store's DHCP lease vanished; the
  MagicDNS name mounted at once — over a Wi-Fi hop, at 4.6 MB/s, which is slower than recomputing the prefix.
  `SEAT_L2_MIN_MBPS` turns that into a refusal instead of a silently useless tier; until the path is back,
  run the same-box tier (`SEAT_L2=` empty, no mount vars, `l1_staging_gb` sized as the tier) and set
  `enabled: false` here so `offload_status` and the seat agree. The wrapper's refusal message names the
  cause (does not resolve / port unreachable / answers but the share refused).
- `chunk_size` must equal the engine's unified block size for the model (vLLM logs "Setting
  attention block size to N tokens"; 784 for Qwen3.8-27B with fp16 KV). The 784 default is that one
  model's number: status reports `chunk_size_defaulted: true` until you set it.
- `key_prefix` is one namespace per stack generation: change it (or flush the store) whenever the
  engine layout, the KV dtype or the LMCache build changes — objects written under another
  generation fail reads with "value size exceeds buffer capacity" and the tier silently pays nothing.
- `key_prefix` or `seat` is required when enabled: two seats must never share a namespace by accident.
- `offload_status.kv_cache_server` LISTS every binding (`bindings[]`, each with its seat, store,
  address, key_prefix, l1_staging_gb and declared/enabled state) plus `unbound_seats` — the same
  list `doctor` fails on, so the report and the gate cannot disagree — and, for a Valkey store named
  by an IP literal, a 1 s TCP `reachable` fact per binding; a hostname is reported as unprobed. A
  binding the load refused is reported `invalid` and never dialed. The bindings are declarative: each
  seat wrapper runs what ITS `seat.env` says — keep them in agreement (`install vllm-seat --config`
  is how you stop doing that by hand).

The seat itself: `setup/templates/vllm-seat/` has the reference `seat_fg.sh` (starts the LMCache MP
server and the engine in the foreground of the llama-swap client, so a swap-out reaps the engine
while the store keeps the pages), `seat_stop.sh`, the llama-swap entry, and the second device's
`kv-cache-server.service` (a systemd-guaranteed Valkey container; do not rely on docker's restart
policy). **KV pool pinned from free memory (2026-09-07, `seat_fg.sh`):** `--gpu-memory-utilization` budgets a fraction of the
card whatever the co-residents hold, and the profiler lands the same config at different pool sizes on different starts; set
`SEAT_KV_HEADROOM_GIB` in the seat's env and the launcher instead computes the pool from what is actually free on the tighter seat
card at launch — free − `SEAT_NONKV_GIB` (the engine's weights + non-torch + peak activation per worker, read from the profiler's
own banner) − the headroom (what must stay free for co-residents' growth) — floored at `SEAT_KV_FLOOR_GIB` (2.0) and capped at
`SEAT_KV_CAP_GIB` (3.4), and passes it as `--kv-cache-memory-bytes` (per worker; vLLM then ignores the utilization). The banner
line names every input. Off unless the headroom knob is set; measured on the reference workstation after a util-0.90 seat stalled
(gate verdict 2026-09-07: headroom 0.5 GiB with the utility seats on CPU → the same 209,597-token pool on 10/10 starts, c32
218.9 tok/s at TTFT p95 11.1 s, a 20-minute soak of 1,624 requests with 0 errors — the run-to-run pool variance is gone) 
under daytime co-resident growth. Prove the tier with vLLM's own `vllm:external_prefix_cache_hits` counter around an
after-eviction request, and prove fidelity with a planted needle retrieved verbatim after eviction
and after a restart — hit counters alone do not prove the context came back intact.

**Pipeline seats and the store (measured 2026-09-03, served since 2026-09-22):** stock LMCache sizes L2
reads from one layout per model, but the stages of a pipeline-parallel seat hold different numbers of
full-attention layers — the 3-card agent seat's 28,13,23 split of a 64-layer model with full attention every
4th layer puts **7/3/6** attention layers on its three ranks — so reads failed for every rank whose layout
differed and the evict phase got 0 external hits. With the per-rank layout overlay patch (each rank's layout
bound to its own pages; loaded through `SEAT_LMCACHE_PYTHONPATH`) the 3-card pipeline seat is bound to its
own `fs_native` store, as in the example above, and serves L2 hits. **Caveat, still open:** the FIRST
request after an MP server start gets 0 L2 hits — it recomputes — until register-time binding lands; later
requests hit. Each seat writes its own wrapper status file (`SEAT_L2_STATUS_FILE`: a rendered seat env names
`seat-l2-<seat id>.status` beside it, e.g. `seat-l2-qwen3.8-27b-vllm-3card.status`; a hand-kept env may name
another), and the binding's `status_file` must name exactly the file that seat's env names.
L1 staging for this hybrid model is 8 GB on the two-card seat and 16 GB on the three-card seat (2 GB, the
default, fails its stores; 8 GB on the three-card seat left too little free to stage an L2 hit back).
Details: [`docs/systems/cache-server.md`](systems/cache-server.md), ADR 0033 (the tier), ADR 0045 (a binding per seat).

### Delegate subtasks across fleet nodes (`agent_delegate` / `delegate`)

Fan self-contained sub-agent contracts out to this box or to fleet nodes on your tailnet
(never cloud — [ADR 0023](architecture/decisions/0023-agent-lane-tailnet-auth-and-locality.md)).
Placement is **quality-first**: an idle local box always runs the work; a remote node is used
only when the local GPU is busy *and* the node passes the capability gate. Wire details:
`docs/FLEET-NODE.md`. Template contracts to start from: [`contracts/`](../contracts/README.md).

**A text-class GPU lease reserves the local seat (0.113.14).** `gpu reserve --class text` is how a
benchmark, eval or measured run keeps everyone else off its cards. Until 0.113.14 delegate placement
read the lease only to *prefer* a remote (route=auto) — and on route=spread not at all — so a
contract with no eligible remote still ran on the reserved seat (three foreign contracts loaded a
reserved two-card seat mid-measurement, 2026-09-05). Now, on route=auto and route=spread, a held
**text** lease takes the local seat out of placement: an eligible remote takes the work; with none,
the placement waits up to `agent_lease_wait_sec` (config; default 0 = defer at once), re-reading the
lease once a second, then defers with class `infrastructure` and a reason naming the holder (class,
pid, reason, origin, expiry) so the caller can wait, route elsewhere, or ask. `route=local` is the
caller's explicit choice and is never gated. A **media** lease is not a placement gate: it keeps
steering toward remotes as before and is arbitrated at the model-affinity gate (ADR 0026), so
single-box render behaviour is unchanged. The local seat's window is probed live before each run —
llama-server `/props`, else the backend's `/v1/models` `max_model_len` (a vLLM seat behind
llama-swap has no `/props`; before 0.113.14 such a seat was budgeted at the 8,192-token fallback
and `offload_status` showed `ctx_probe_error: HTTP 404` while it was warm). The per-model probes share
one cold-start budget of 10 minutes (llama-swap's `healthCheckTimeout`), because llama-swap answers them
only once a cold seat is up — a vLLM seat measured 222 s, and the old 60 s per-URL timeout returned the
fallback for the whole run. When the probe still cannot answer, the box's `agent_ctx_tokens` is the
window; when it answers and disagrees with `agent_ctx_tokens`, the served window wins and the note says so.

**Store steward (0.113.16) — `fleet_store_root`, `fleet_store_cap_gb`, `fleet_store_prune_every_jobs`, `fleet_store_prune_every_sec` (0.130.1: a time tick, default 60 s, because under a GPU lease no job completes on the node while a bench arm or the pair seat writes ~1 GB/min; negative disables it).** A node that owns a
persistent KV page store on disk (the Lenovo's LMCache fs_native dataset) keeps it under budget between its own turns:
`cap = min(fleet_store_cap_gb, 0.8 × (used + free))`, prune oldest-first from 95 % of cap down to 85 %, after every N completed
jobs (default 8) and on any health poll that finds it high. `/fleet/health` shows it under `store`. Set the root to the
store dataset's mount (for example `/srv/kvstore`) and create the marker file `.storesteward` in it once (the steward
writes it into an EMPTY root itself; a populated root without it is refused at start, so a mistyped root can never be pruned;
every removed page is one journal line), the cap to the dataset quota, and keep the seat's own
`SEAT_L2_PRUNE_GB + max_capacity_gb` under the quota too — LMCache evicts only its own pages.

**Serving-config provenance (0.123.0, ADR 0043) — `serving_config_path`.** The llama-swap config is rendered once, at
install, and nothing re-renders it: `ampere-16` served a 32768 window for weeks after the tier table said 131072, and
`audit-yaml` reported OK the whole time because a stale config breaks no operator rule. `install render` now stamps every
config it writes with `spec_sha256` (a closed input set: tier, render params, template, tier entry, harness version) and
`body_sha256` (the yaml below the stamp, so a hand edit stays distinguishable). Check a live box with
`local-offload audit-yaml --against-render <file>` — flags BEFORE the files — which re-derives from the binary's own seeds
and reports `MATCH` / `STALE(<keys>)` / `UNSTAMPED` / `HAND-EDITED`; STALE names the inputs that moved and exits 1, as does
HAND-EDITED, while UNSTAMPED (every config rendered before 0.123.0) prints as a finding and does not fail. Point
`serving_config_path` at the node's rendered config and `/fleet/health` publishes `serving_config_spec_sha256` +
`serving_config_state` as well, so a fleet-wide sweep is one poll; unset, both keys are omitted. Fix a STALE box by
re-rendering with `install render`, never by hand-editing the file.

**Lease in health (0.113.16).** `/fleet/health` carries `lease` while the node's GPU lease is held; a text lease makes the
node ineligible for new delegated work (and its dispatch answers 503, re-placeable). `gpu reserve --drain --unload-seat` and
`gpu release --warm-seat` are the maintenance verbs — see docs/systems/gpu-lease.md.

**A held card is a place in line (0.115.2).** `gpu reserve` QUEUES behind a current holder for `--wait` (default 8h;
`--wait 0` fails fast) in both the wrapper and `--detach` forms, printing one line on entry and one on acquire; the holder's
declared window is reported, never trusted — the wait runs its full length. `--unload-seat` (or `--exclusive`) stamps the
text lease **exclusive**, and the text-load gate then keeps models off the cleared cards for the lease's length — loads ride
a `cascade_remote_lanes` lane or wait their own budget. `offload_status` publishes the local lease under `gpu_lease` with
`queue_with`, the exact command. The rule for every session: **never refuse or defer GPU work because a card looks busy —
reserve it and the machine queues it**:
`local-offload gpu reserve --wait 8h --drain --unload-seat --for <window> --reason "<why>" -- <cmd>`.


**Enable — worker node** (the box that will *execute* contracts), in its
`~/.local-offload/config.json`:

```json
{
  "fleet_agent_enabled": true,
  "fleet_auth_token": "<one shared secret, same on every node>",
  "agent_ctx_tokens": 16384
}
```

`agent_ctx_tokens` is the tier's served agent-seat window (the installer records it in
`installed.json`); `0` means the node advertises no ceiling and is **never** chosen for remote
agent work. Then serve beyond loopback on the machine's Tailscale address:

```powershell
local-offload fleet-serve --listen <tailscale-ip>:18811 --listen-trusted-network
curl http://<tailscale-ip>:18811/fleet/health
# ... "agent_enabled":true,"agent_seat":"offload-e4b","agent_ctx_tokens":16384,"agent_seat_resident":true ...
```

`agent_seat_resident` starts `false` (the roster probe is cached, background-refreshed, and
fail-closed) — give it one health request plus a few seconds before concluding anything.

**If the four `agent_*` fields are missing entirely**, the lane is not admissible and no
delegator will ever place here — by design, since a dispatch would be refused. The three
conditions are the same ones the ack-time guard applies: `fleet_agent_enabled: true`, a
resolvable agent seat (`agent_model`, else the workhorse `model`), and a safe listener —
loopback, **or** `fleet_auth_token` set for anything beyond it. Serving on a Tailscale address
with no token is the common miss: the advertisement is withheld rather than published-then-403'd.

**Enable — delegator** (the box that *places* contracts), in its config:

```json
{
  "agent_delegation_enabled": true,
  "fleet_auth_token": "<the same shared secret>"
}
```

That registers the MCP `agent_delegate` tool (tools/list is byte-identical when off) and
unlocks the CLI verb. Remote nodes are named per call, not in config: `--remote` (repeatable)
on the CLI, `remotes: [...]` on the MCP tool — tailnet URLs only (loopback, `100.64.0.0/10`,
a dotless MagicDNS name, or a host under your own tailnet DNS zone; anything else is refused
by name).

**Context budget — read this before writing a contract.** The 256 KiB wire cap is a transport
bound only; what actually decides remote placement is the gate's arithmetic:
`ceil(chars/3)` over goal + context docs + schema + acceptance, **plus a 3072-token reserve**
(system prompt + tool specs + per-step transcript growth × the 12-step cap), must fit the
node's advertised `agent_ctx_tokens`. The `chars/3` estimate is a deliberately conservative
upper bound (v1 has no remote tokenizer to ask; typical prose runs ~4 chars/token), so the
practical budget is smaller than the raw window suggests:

| Advertised `agent_ctx_tokens` | Estimate budget after the reserve | ≈ contract text that fits | ≈ real prose tokens |
|---|---|---|---|
| 4096 | 1024 | ~3 KiB | ~0.8k — goal + a snippet, no real docs |
| 8192 | 5120 | ~15 KiB | **~2–4k** |
| 16384 | 13312 | ~39 KiB | ~10k |
| 32768 | 29696 | ~87 KiB | ~20k+ |

At an 8k seat, budget **~2–4k tokens of actual document content** per contract; split bigger
inputs across subtasks. A contract that does not fit is not an error — it places locally
(route `auto`) or defers (route `remote`), and the placement reason says why.

**Worked example** (MCP `agent_delegate`; the CLI takes the identical subtask object as
`--contract file.json`):

```json
{
  "subtasks": [{
    "goal": "Read the two release-notes docs and produce a migration digest: list every config key added between them, and summarize the upgrade in two sentences.",
    "context_paths": ["notes/release-a.md", "notes/release-b.md"],
    "output_schema": {
      "type": "object",
      "properties": {
        "added_keys": {"type": "array", "items": {"type": "string"}},
        "summary": {"type": "string"}
      },
      "required": ["added_keys", "summary"]
    },
    "acceptance": ["min_items:added_keys:1", "nonempty:summary", "not_contains:TODO"]
  }],
  "route": "auto",
  "read_root": "/abs/path/to/project",
  "remotes": ["http://<node-b>:18811"]
}
```

#### Delegating an IMPLEMENTATION leg (`write_root`, 0.122.0, register D-06)

A subtask may add `"write_root": "<dir relative to the run's read root>"`. The seat then gets
`write_file` + `edit_file` inside that directory of the node's own copy of the inlined docs, and what
comes back beside the usual result is a **unified diff**:

```json
{
  "subtasks": [{
    "goal": "util.go has an off-by-one in Last(): it returns n where it must return n - 1. Fix it, and add one table-driven case to util_test.go covering n = 1.",
    "context_paths": ["util.go", "util_test.go"],
    "write_root": ".",
    "output_schema": {"type": "object", "properties": {"change": {"type": "string"}}, "required": ["change"]},
    "acceptance": ["diff_touches:util.go", "diff_max_files:2", "contains:n - 1"]
  }],
  "route": "local",
  "read_root": "/abs/path/to/project"
}
```

Read the result's `diff` and apply it yourself (`git apply -p1`); **the harness never applies it**, on
purpose — a small local seat is worth handing an implementation leg precisely because a human reads
what it produced before it touches anything real. `diff_files` lists the touched paths and `write_note`
says what the door did when there is no diff (most often: the seat described the change instead of
making it).

Two acceptance verbs read the write set rather than the prose, and are the only checks on a write
contract a talkative seat cannot satisfy by talking: `diff_touches:<path-prefix>` and
`diff_max_files:<n>`. Both FAIL on an empty write set, `diff_max_files` included.

Requirements and limits, all of them refusals rather than surprises:

- The executing node must have `"agent_allow_write": true` (see docs/FLEET-NODE.md). Without it the
  contract is refused at ACK and re-placed on a node that has it, or — locally — deferred with
  `defer_class: "write"`.
- `write_root` must be RELATIVE and must not escape (no `..`, no absolute path, no `.git`, no reserved
  Windows device name). It is relative because the node has never seen your filesystem: the contract is
  self-contained, and an absolute path from your box would name nothing there.
- 8 files, 64 KiB written, 192 KiB of diff. Past any of them nothing is published and the subtask
  defers `write` — split the leg instead.
- No delete, no shell, no `run`, no network. This lane edits files; it does not verify them. Running the
  tests is still yours.

**Gate it before you rely on it.** `scripts/write-door-gate.ps1` (fixtures under
[`contracts/write-door/`](../contracts/README.md), section "write-door/")
sends three staged legs to one seat — `-Remote http://<node>:18811` for a fleet node's door, no
`-Remote` for this box's own seat — applies each returned diff to a fresh copy and proves it there
(`go test` red → green, a JSON parse, exact-row checks). PASS means the door on that seat produces
diffs a caller can apply; it is the measurement that preceded every door opened so far (register D-06).

`context_paths` are read and inlined **by the delegator**, confined to `read_root`
(≤ 128 KiB per file) — your session's context never pays for them, and the wire contract stays
self-contained (the remote node never reaches back into your filesystem). The node writes them as
files in the seat's read root; the seat then spends its first steps finding them (measured: 89 of
117 failed 4B rows in six days stop at `list_dir` + `read_file`). `setup_actions` (0.113.24) lets
a subtask name up to eight `{tool, args}` calls the node replays before the model's first turn —
`[{"tool":"read_file","args":{"path":"notes.md"}}]` puts the document in front of the seat at
step 0, spending no step. Simpler still: set `agent_seed_context_reads: true` in the executing
node's config and it prepends one `read_file` per context doc by itself (it knows the names).
Read `setup_ran` on the result before crediting either — a node one release behind ignores the
field silently. `acceptance` is
evaluated by the **delegator** after the result returns: a schema-valid result that fails a
check comes back `failed_verification`, never a success. Read the response's `summary` block
first — `{succeeded, deferred, failed_verification, failed, infrastructure}` — eight quiet
defers are a loud outcome, not eight green jobs. `infrastructure` counts the results whose story
is a broken stack rather than the work: defers whose `defer_class` says so (`infrastructure` /
`config`, as opposed to `abstention` / `budget`), plus a local placement taken while every
configured remote was failing its health probe. Non-zero means a node is broken or
misconfigured, not that the small model could not do the job.

`lost_to_stack` rides beside it (omitted when zero) and is the half that is **lost work** —
subtasks that delivered no usable result because the stack failed them: the contracted output
never arrived, as opposed to a local placement that succeeded while the fleet was down. It is not
a "the result is blank" count — a `structured re-pack unreachable` defer carries the finished
loop's prose in `output` with `structured` absent, and still counts, because a contract with an
`output_schema` asked for a checked deliverable. The MCP tool marks the call `isError` on
`failed > 0 || lost_to_stack > 0`, with the JSON body unchanged; the CLI's exit code is the wider
`infrastructure > 0` rule below.

`defer_class: "contract"` is the one class that is **your** problem rather than a box's: the
contract carries no `output_schema` for a remote placement, is past the origin hop, or needs
more context than **every** node advertises — a node that advertises no `agent_ctx_tokens` at all
makes it a `config` verdict on that node instead, because an unadvertised ceiling is unknown, not
small. Those stay exit 0 by design. If the summary carries
`corpus_rows_lost` / `ledger_rows_lost`, the results are still complete — the harness could not
write that many telemetry rows (usually a full or read-only disk).

CLI equivalent:

```powershell
local-offload delegate --contract contracts/research-digest.json --route auto --remote http://<node-b>:18811
```

Exit 0 covers honest defers (`abstention` / `budget`) and failed verification — the JSON says
what happened. Non-zero is reserved for transport/config failures **and** for
`summary.infrastructure > 0`: a worker whose llama-swap is down defers every subtask, and a
scripted caller reading only the exit code must not take that for a good run. Every subtask
also writes a ledger row
(`task=agent_delegate` in `local-offload ledger`) and a full contract+result+verdict line
under the harness base dir at `delegation-log/YYYY-MM-DD.jsonl`.

Loop accounting on the wire (0.115.20): every `results[]` row carries the node's `steps`,
`stop_reason`, `stop_note` and `output_truncated` as the node reported them. Read them before
trusting a `done`: `stop_note` beginning `forced final answer` means the step budget ran out
and the last step extracted the answer without tools (0.115.19, register D-89) — a correct
answer from an incomplete exploration; `output_truncated: true` means the final completion
ended on `length` and the text is a partial. Both used to be visible only in the delegation log.

Acceptance lint (0.88.0, warn-only): every subtask's acceptance is linted at intake and the
warnings ride the response as `results[].acceptance_lint` — PARROT-PASSABLE (every content
check also matches the goal text: an echoed question passes as verified and the retry never
fires), UNGROUNDED (`contains:`/`regex:` matching nothing in the contract's own docs — fails
right answers), SHAPE-ONLY (`nonempty:`/`min_items:` alone — passes garbage). The run still
happens; fix the acceptance before reusing the contract (authoring rule: anchor ≥1 check to
content that appears in the docs but not in the goal; contracts/README.md).

Experiment support (0.79.0–0.81.0): setting `OFFLOAD_DELEGATE_ARM=<label>` tags every
delegation-log line **and every ledger row** the process records with `arm:<label>`, so an
experiment's rows separate cleanly from ordinary traffic after the fact. Each served run's
result also carries A1 config pins — `harness_version`/`harness_build_sha256` (which binary
constructed the requests) and `seat_config_sha256`/`seat_config_basis` (what the seat's
llama-server actually served, hashed from live `/props`) — and the log line adds
`delegator_version`/`delegator_build_sha256` for the acceptance/placement side. Paired
cross-seat comparisons must refuse rows whose pins differ or are absent; absent means
*unknown* (pre-0.81, or the seat was gone by probe time), never "same config".

| Failure | Fix |
|---|---|
| `403 agent lane requires fleet_auth_token on a non-loopback listener` | The worker is bound beyond loopback with no token. Set `fleet_auth_token` (same value) on both sides and restart `fleet-serve`. |
| `401 unauthorized` on dispatch or poll | Token mismatch between delegator and worker configs. |
| `agent delegation is disabled on this box` | Set `"agent_delegation_enabled": true` in the **delegator's** config. |
| Everything places local although a remote exists | Usually correct — idle-local always wins. Force `--route remote` to surface the gate's verdict: the defer reason now names the actual cause — no remotes configured, every remote failing its health probe (each error quoted), or a healthy remote failing the gate (`agent_enabled` + `agent_seat_resident` + `output_schema` + the ctx arithmetic above). |
| `remote "…": hostname … not allowed` | Non-tailnet URL. Loopback, `100.64.0.0/10`, a dotless MagicDNS name, or your own tailnet-zone hostname only. |
| failed `queue deadline after …: the node accepted the job but never started it` | The node admitted the job but never gave it an execution slot — every poll said `accepted`. It is **saturated**, not broken. Check `jobs_running` / `jobs_queued` / `max_concurrent_jobs` on that node's `/fleet/health`; raise `fleet_max_concurrent_jobs` if the box can genuinely run more at once, or spread the fan-out across more nodes. Queued time is credited back to the budget, so this only fires after a real wait (bounded by `min(timeout_sec + grace, 5 min)`). |
| `503 queue full (… limit N)` | `fleet_max_queue_depth` reached — default is now `2x fleet_max_concurrent_jobs` (8 with the default 4 workers), not a flat 32, so a busier node hits this sooner than it used to. The node publishes a `Retry-After` header sized from its own `recent_agent_wall_sec` (bounded `[5, 300]`, worded so you can tell a real estimate from the flat 30s default or a clamped-high one), and health separately publishes the same formula's RAW, unbounded value as `queue_wait_estimate_sec`. `internal/delegate` does not yet consume either automatically — that lands with the placement release (`feat/placement-eta`) — so for now, read the header/field yourself before retrying, or raise `fleet_max_queue_depth` if the box should hold a deeper backlog. Never size your own retry from `seat_rate.min_turn_sec`; that is a per-seat retry floor, not a queue-depth signal. |
| deferred `poll deadline after …: node accepted the job but did not reach a terminal state` | The node acked, STARTED the job, and outran `timeout_sec` + 60 s grace. Check the worker's serve log; the job id in `delegation-log/` reconciles it. A quoted "last poll error" means the node was also answering badly (5xx) — fix that first. A `(+… credited back for time queued …)` clause means part of the wall clock was backlog wait, which was *not* charged to the budget. |
| failed `poll deadline after …: node never answered` | The node died or became unreachable after acking: nothing came back at all, so nothing is claimed on its behalf. The quoted last error (dial refused, dropped connection) is the lead. |
| deferred `output failed schema: …` | The schema was too ambitious for the seat. Flatten it — a `properties` map of string / number / integer / boolean / string-array / enum fields is the supported subset. |
| deferred `structured re-pack unreachable: …` | Not a schema problem: the worker's llama-swap could not be reached for the final grammar completion. Restart/check the worker's endpoint. |

### Busy-hour cascade failover (`cascade_remote_lanes`)

The daily lane — the `offload_summarize` / `offload_classify` / `offload_extract` /
`offload_triage` calls a session fires dozens of times a week — can fail over **per call**
to another node while this box's GPU is held by a render. Opt in on the box whose cascade
should fail over, in its `~/.local-offload/config.json`:

```json
{
  "cascade_remote_lanes": { "offload-e4b": "http://<node-b>:18811" }
}
```

Key = the exact model id or llama-swap alias a cascade call names; value = a tailnet base URL
that **serves that same model**. The value may be either shape, and the lane works out which by
probing it — you never declare it:

| lane base | when to use it | residency read from | the call rides |
|---|---|---|---|
| `http://<node>:18811` (a **fleet node**) | the normal case on this fleet | `GET /fleet/health` → `served_models` | `POST /fleet/chat` with `fleet_auth_token` |
| `http://<node>:11436` (a **llama-swap**) | only where llama-swap is reachable from this box | `GET /v1/models` | `/v1/chat/completions`, no credential |

**Use the `:18811` node form on this fleet.** The Lenovo and the Aorus bind llama-swap to
`127.0.0.1:11436` and nothing else, so the `:11436` form is unreachable from the Qube and the
lane will never engage; binding llama-swap to the tailnet instead would be a new unauthenticated
listener, which is why the node proxies the call behind its own bearer gate (the same door the
vision lane uses). The node must be running a harness with the chat lane — check
`curl http://<node-b>:18811/fleet/health` for `"chat_lane": true` and for the model in
`served_models` — and **this box's `fleet_auth_token` must match the node's**, or the lane
fails closed to local (one log line per window) rather than collecting 401s.

Semantics, in order:

- A `seat_endpoints` static pin on the same model always wins — a pinned seat is *always*
  remote; a lane is *busy-hours only*.
- A lane is taken only when this box would make the call **wait** — either the machine-wide GPU
  lease is held, or (register C-41) the local llama-swap holds ANOTHER model that is `starting`
  or is `ready` with at least one request in flight, so a swap to the cascade tier would queue
  behind it — **and** a roster probe (alias-aware, cached 30 s per lane, fail-closed) confirms
  the lane serves the model. Idle GPU with an idle (or absent) local seat, a loaded seat that is
  the requested model itself, a probe failure, or a roster miss → the call stays local, silently
  and safely. The local busy reading is cached 5 s.
- **Quality-identical by construction**: the lane must serve the SAME model — the failover
  never changes *which* model answers, only *where*. Never point a lane at a smaller tier.
- Every reroute writes one serve-log line naming the reason: `cascade remote lane: <model> ->
  <base> (local GPU lease held)` or `… (local seat busy: qwen3.8-27b-vllm, 2 in flight)`. No
  lines while the box is busy = the lane never engaged — check that the lane actually serves the
  alias, at whichever door the base is: `curl http://<node-b>:18811/fleet/health` (look for
  `chat_lane` and `served_models`) for a node base, `curl http://<node-b>:11436/v1/models` for a
  llama-swap base. A failed probe of either side logs its own line once per window, and so does a
  node that advertises no `chat_lane` or that this box has no `fleet_auth_token` for.

Lane URLs pass the same tailnet guard as everything else here (loopback, `100.64.0.0/10`,
dotless MagicDNS, or your own tailnet zone; validated at config load naming the key, and
again at every dial).

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
| alias `FAIL — not in roster` | Restart llama-swap so it re-reads the yaml; confirm the name matches a model id or one of its `aliases` (the check reads both). |
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
| **`doctor` exits non-zero with a `config findings` section** | One `FAIL` row per value that loads and then cannot do what it says: a `delegate_remotes` base not on the fleet node port `:18811`, one that is loopback (a delegate REMOTE is another box), or one carrying a `/v1` suffix; a `cascade_remote_lanes` base on neither shape a lane can be (a fleet node `:18811`, or a llama-swap on this box’s own `endpoint` port) or carrying `/v1`; `gpu_wait_ms` more than 3x `vision_gpu_wait_sec` (C-33); one row per retired key still in the file. Nothing is refused for these — fix the file, or accept the row. |
| **A config file FAILS VALIDATION naming a key** | A configured HTTP base is refused by key and value when it is not a usable URL (a parse error, a scheme that is not http/https, or no host — `${NODE_A_HOST}:18811` and `http://node-a:$PORT` fail to parse, and `node-a:18811` parses as *scheme* `node-a` with no host), or when its port is `:0`/`:9` (the OS’s "any free port" and IANA discard; compared numerically, so `:09` is refused too). Applies to `endpoint`, `delegate_remotes[]`, `cascade_remote_lanes{}`, `seat_endpoints{}`, `fleet_queue_holder`, `tts_endpoint`, `nim_endpoint`, `hailo_endpoint`, `coral_endpoint`, `pair_workloads_endpoint`. An EMPTY value is fine; a **loopback** base on an unusual port is still allowed — that is INV-10’s bench twin. |
| **What a refused config does, per entry point** | `fleet-serve` **refuses to start** (non-zero exit): a node that cannot prove its own config must not accept other boxes’ dispatches. `mcp` **starts and says so** — `offload_status` carries `config_error` as its first key and every other tool returns `{"deferred":true,"reason":"config invalid: …"}`; it must never vanish, because an MCP server that exits takes every tool out of every session silently. `doctor` prints the refusal verbatim as its first `FAIL` row and exits non-zero. One-shot CLI verbs warn and proceed. The stderr warning and doctor’s `config:` line no longer say "BUILT-IN DEFAULTS" — a file that failed validation is still the file the process is running on. |
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
