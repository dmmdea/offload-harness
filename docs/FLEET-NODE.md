# Fleet node — joining the fleet-dispatcher fleet (`fleet-serve`)

Operator guide for running this box as a **fleet node**: a small HTTP server that lets the
Fleet Dispatcher send GPU render jobs (image / video / audio / stt / run-graph) to this
machine through the same pipeline, GPU lock, and zero-always-warm lifecycle every local call
uses. Wire protocol: fleet-dispatcher `CONTRACT.md` **v2** — three endpoints, JSON
everywhere, GiB everywhere, every failure a non-2xx.

| Endpoint | What it returns |
|---|---|
| `GET /fleet/health` | node id, live global VRAM (total/free GiB), supported task types + model families **derived from this box's actual config**, measured per-family VRAM footprints, queue depth. Never blocks on a render. `503` when the VRAM snapshot is unavailable **or older than 30s** (nvidia-smi failing, e.g. a driver reset) — a stale 200 would mislead routing. |
| `POST /fleet/dispatch` | immediate `202 {"job_id": <exact echo>, "status": "accepted"}`; the render runs async through the pipeline. Duplicate `job_id` → 202 re-ack (accepted/running/**done** — poll `/fleet/jobs/{id}` for the state), never a second render; a job that previously **failed** here answers `409` (an explicit refusal, so the dispatcher may try another node). |
| `GET /fleet/jobs/{id}` | `{"state": "accepted\|running\|done\|error", ...}` with `data` on done / `error` on error; terminal results retained ~1h; 404 for unknown/evicted ids. |

## Quickstart

```powershell
# Loopback bring-up (default 127.0.0.1:18811)
local-offload fleet-serve

# Verify from the same box
curl http://127.0.0.1:18811/fleet/health

# Production: bind the TAILSCALE address so the dispatcher (on another box) can reach it
local-offload fleet-serve --listen 100.64.0.10:18811 --listen-trusted-network
```

Startup runs a **GPU probe** (one `nvidia-smi` memory query). No working NVIDIA GPU →
`fleet-serve` refuses to start: the contract treats `vram_total_gb <= 0` as a broken node,
and refusing loudly beats advertising an empty GPU. Ctrl-C drains: new dispatches get 503,
in-flight renders get up to 30s to finish, survivors are marked terminal
`error:"interrupted"` so pollers always reach a terminal state.

## Config keys

| Key | Default | Purpose |
|---|---|---|
| `fleet_listen` | `127.0.0.1:18811` | Bind address (`--listen` overrides). Port **18811** — the dispatcher owns 18810. |
| `fleet_node_id` | `""` | Node id in `/fleet/health`. Empty = the OS hostname at serve time (`--node-id` overrides). |
| `fleet_sampler` | `auto` | Per-render VRAM footprint source: `auto` \| `pdh` \| `global` (see [Sampler modes](#sampler-modes)). |

## Binding guidance (read before exposing anything)

The fleet endpoints are **unauthenticated by design** (matching the dispatcher's posture):
anyone who can reach them can run renders on this GPU. The same rules as
`local-agent --serve` apply, enforced by the same shared guard:

- **Loopback is the default** and needs no flag.
- A non-loopback `--listen` is **refused** unless you pass `--listen-trusted-network`
  (which prints a loud warning).
- Production binding is the machine's **Tailscale address** (e.g. `100.64.0.10:18811` on
  your workstation) — the tailnet is the trust boundary. **NEVER bind `0.0.0.0`**, and never expose
  the port beyond the tailnet.
- Port **18811** per the house port discipline; update the machine's port file in
  `P:\Port Directory\` when you stand a node up.

## Footprints — measured, not guessed

`/fleet/health` advertises `model_footprints[]`: **measured** per-(family, quant, task)
VRAM peaks, including this box's offload strategy, stored at
`~/.local-offload/footprints.json`. The dispatcher uses them for admission, ignoring any
entry with `vram_peak_gb <= 0` (we never write those).

Recording is **passive**: every GPU render through the pipeline — normal harness use, not
just fleet jobs — samples VRAM while the child process runs and folds the observed peak
into the store (max-keep; `vram_peak_gb = observed_max`, rounded to 0.1 — the **raw** peak,
no padding). Footprints therefore stay current when bindings change: a new model family
simply starts a new entry. The node adds **no** margin: the dispatcher owns all routing
margin (CONTRACT v2.1 / ADR 0013). A node that padded its own ×1.2 on top of the
dispatcher's margin double-inflated footprints and made wan2.2/hidream unroutable on a 16 GB
node — so don't pad the store by hand.

### Priming an empty store: `fleet-measure`

A freshly-installed node has no footprints, so its health advertises none and the
dispatcher has nothing to admit against. Prime it:

```powershell
local-offload fleet-measure
```

One minimal render per configured task — image (512×512, 8 steps), video (the fast
distilled recipe at 9 frames, reusing the probe image as the still), music (5s) — then the
store's on-disk records print as JSON. Voice and run-graph are skipped (no cheap universal
probe); their footprints accumulate passively during normal use. Renders run through the
normal pipeline, so the store records exactly what fleet jobs will cost.

## Sampler modes

Windows GeForce cards with a display attached run **WDDM**, where NVML's per-process VRAM
accounting returns N/A — `nvidia-smi` can only see **global** memory. So the harness has two
sources:

| Mode | Source | What it measures |
|---|---|---|
| `pdh` | Windows PDH counter `\GPU Process Memory(pid_*)\Dedicated Usage`, summed over the render's **process tree**, sampled every 500ms during renders only | What OUR job costs — uncontaminated by the desktop, browsers, or other apps. The same counter Task Manager and Afterburner surface. |
| `global` | `nvidia-smi` global `memory.used` delta from a baseline captured at render start | The whole GPU's swing during the render — includes anything else that allocated meanwhile. |
| `auto` (default) | PDH on Windows, global-delta elsewhere | The right default once PDH is validated (below). |

The PDH counter set has a documented accuracy caveat on some driver/Windows combinations —
hence a one-time validation at bring-up. One honest quirk to expect: the counter set often
shows **bogus values for the `dwm.exe` instance** (the desktop compositor); that is a known
WDDM artifact and harmless here — our tree-sum only includes the render process and its
descendants, never dwm.

### Validation procedure: PDH vs Afterburner (once per box)

1. Install and open **MSI Afterburner**, open its monitor (the graph window), and enable the
   per-process VRAM plot: **"Memory Usage \ Process"** (per-process dedicated VRAM — the
   capability nvidia-smi cannot provide under WDDM).
2. Run `local-offload fleet-measure` (or any real render — `generate-image` works).
3. When it finishes, read the recorded `observed_peak_gb` for that render's entry
   (`fleet-measure` prints it; or open `~/.local-offload/footprints.json`).
4. Compare against the peak Afterburner showed for the render process (the ComfyUI python /
   node child) during the run.
5. **Agreement within 15%** → PDH is trustworthy on this box; leave `fleet_sampler` on
   `auto`. **Disagreement over 15%** → set `"fleet_sampler": "global"` in this machine's
   config: the global-delta source is coarser but never lies about the ceiling.

## Recommended companion: MSI Afterburner

**Recommended, never required** — the harness has no dependency on it, reads nothing from
it, and every fleet feature works without it. But on a WDDM box it is the operator's best
instrument, and we explicitly encourage running it alongside a fleet node:

- **Per-process VRAM under WDDM.** `nvidia-smi` cannot attribute VRAM to processes on a
  WDDM GeForce card; Afterburner's *Memory Usage \ Process* plot can. It is the independent
  reading our PDH sampler is validated against.
- **Validation role.** The bring-up procedure above is the one place the harness asks you
  to look at it; after that it's a live sanity check whenever a recorded footprint looks
  off.
- **Dashboards.** Afterburner exports every plotted metric through the **MAHM shared
  memory** interface (`MAHMSharedMemory`), which third-party dashboards and small scripts
  can read — useful if you want per-process VRAM on a fleet-wide panel. The harness itself
  never reads MAHM (companion only, by design).

## Task surface

Advertised tasks are **derived from this box's config**, never hardcoded — an unbound route
is not advertised, so the dispatcher can't send work the box would defer:

| Fleet `task_type` | Pipeline task | Advertised when | Footprint family |
|---|---|---|---|
| `image-gen` | `generate_image` | `imagegen_script` set | `imagegen_family` (else `sdxl`); quant `bf16` for the HiDream-O1 binding |
| `video-gen` | `generate_video` | `videogen_script` set | `wan2.2`; quant `q8_0` when the bound unets are the Q8_0 GGUFs |
| `stt` | `transcribe` | `stt_model` set | `whisper` (llama-swap-resident — no footprint sampling) |
| `audio-gen` | `generate_audio` | voice or music script set | `acestep` (music) / `chatterbox` (voice) |
| `run-graph` | `run_graph` | `run_graph_script` set | payload-declared `model_family`, else `comfy-graph` |

run-graph payloads carry `graph` and `manifest` as **raw nested JSON** (no base64) and are
strict-validated at ack time — a malformed fleet job dies at the 400 with a clear reason,
never mid-render. Typed run-graph defers (`VENV_INCOHERENT`, `NODE_CLASS_MISSING`, …)
surface in the job's `error` field as `code: detail`.

## Known limits (v1)

- Jobs are **in-memory**: a node restart loses in-flight jobs (the dispatcher's failure
  rule marks them lost). Once acked, a job is never re-dispatched.
- **Drain marks may be unobservable.** Shutdown marks survivors `error:"interrupted"`, but
  if the process exits right after, a poller may never read the mark. The dispatcher's
  lost-contact rule covers it (~50s to declare the job lost).
- **A duplicate dispatch arriving after the ~1h retention re-renders** — the id has been
  evicted, so it looks new. The dispatcher never re-dispatches post-ack, so hitting this
  needs a lost ack *plus* a 1h+ retry. Accepted.
- **`stt` has no measured footprint** (it's llama-swap-resident, not a ComfyUI render), so
  stt jobs route only with caller-supplied `params_b`.
- **The dispatch envelope rejects unknown fields** (strict decode): a future dispatcher
  field addition needs a node upgrade first.
- **Payload paths (`out` / `out_dir` / `still` / `audio`) are node-local writable paths**,
  taken as given. That's the tailnet-trust posture restated: anyone who can dispatch can
  already run renders; don't extend reach beyond the tailnet.
- `priority` is accepted and ignored (contract-reserved).
- No auth — the trusted-network posture above is the boundary; revisit if the fleet ever
  leaves the tailnet.
