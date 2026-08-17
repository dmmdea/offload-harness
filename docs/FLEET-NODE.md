# Fleet node — joining the fleet-dispatcher fleet (`fleet-serve`)

Operator guide for running this box as a **fleet node**: a small HTTP server that lets the
Fleet Dispatcher send GPU render jobs (image / video / audio / stt / run-graph) to this
machine through the same pipeline, GPU lock, and zero-always-warm lifecycle every local call
uses — and, since 0.63.0, lets a delegator send **agent** jobs (self-contained sub-agent
contracts, see [The agent task](#the-agent-task-task_type-agent) below). Wire protocol:
fleet-dispatcher `CONTRACT.md` **v2** — three endpoints, JSON everywhere, GiB everywhere,
every failure a non-2xx.

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

Startup resolves a **GPU memory provider** (J3): `nvidia-smi` first (a working NVIDIA node
behaves exactly as before), else the **windows-generic WDDM source** — capacity from the
display-class registry (`qwMemorySize`), usage from the `\GPU Adapter Memory` PDH counters,
vendor/arch from the installer's `installed.json` profile, and the UMA memory model from the
profile (an iGPU advertises carve-out + the ~RAM/2 WDDM shared budget as its total, and
Dedicated+Shared as usage). Only when **no memory source works** does `fleet-serve` refuse to
start: the contract treats `vram_total_gb <= 0` as a broken node, and refusing loudly beats
advertising an empty GPU. The serve log names the resolved source
(`... via nvidia-smi|windows-generic, vendor=... arch=...`). Ctrl-C drains: dispatches for a
job_id this node has never seen get 503; a re-dispatch of a job_id this node already knows
about (running, done, or previously failed) still re-acks 202 — or 409 if it previously
failed — even mid-drain, since that's not new work. In-flight renders get up to 30s to finish, survivors are marked terminal
`error:"interrupted"` so pollers always reach a terminal state.

## Multi-GPU: `gpu_devices[]` and the headline VRAM numbers

On any node whose VRAM source is `nvidia-smi`, `/fleet/health` always adds a per-device
breakdown — **including a single-GPU box**, which reports a one-element array. There is no
single-vs-multi-GPU special case: `gpu_devices[]` is present whenever nvidia-smi is the resolved
source, full stop (`chooseSamplerKind` in `main.go`, unit-tested at that exact seam in
`fleet_verbs_test.go`). It is **absent** only on a source that cannot enumerate devices at all —
today, only the Windows PDH/windows-generic path (`vram_windows.go`), which has no per-adapter
identity to report.

```json
"vram_total_gb": 15.93, "vram_free_gb": 15.08,
"gpu_devices": [
  {"index": 0, "uuid": "GPU-3ee161b5-c188-495b-eaeb-291e6e6e1d97", "name": "NVIDIA GeForce RTX 5060 Ti", "vram_total_gb": 15.93, "vram_free_gb": 15.08},
  {"index": 1, "uuid": "GPU-2a44210f-6739-2d89-0e21-44cd5143faf7", "name": "NVIDIA GeForce RTX 5070 Ti", "vram_total_gb": 15.92, "vram_free_gb": 13.46}
]
```

`gpu_devices` is **additive** — `vram_total_gb`/`vram_free_gb` keep exactly the meaning they
always had, and adding a new field breaks nothing on the consumer side in practice: the
fleet-dispatcher decodes health with a plain `json.Decoder` (no `DisallowUnknownFields`
anywhere in its `internal/`), so an extra field it doesn't know about is silently ignored, not a
wire error, on any node — single-GPU included.

**Why this exists:** `nvidia-smi` enumerates devices in PCI bus order — that order has no
relationship to which device a render actually runs on. A CUDA app (ComfyUI included) can bind
its `cuda:0` to a different physical card via `CUDA_DEVICE_ORDER=FASTEST_FIRST`, so trusting
"index 0" for the headline VRAM numbers can silently advertise the WRONG card's free memory —
an idle donor card looking free while the real compute card is mid-render, which lets the
dispatcher over-admit a second job that then contends or OOMs.

**The default headline rule** (no `primary_gpu_uuid` set): `vram_total_gb`/`vram_free_gb`
describe the device with the **largest total VRAM**; an exact tie in total is broken by
whichever card has more free VRAM right now. A single render binds to one device, so the
admission-relevant number is the biggest device a job could actually land on — never an
arbitrary enumeration index, and deliberately **not a sum** across cards (summing would let the
dispatcher admit a job no single card can hold). This rule is a defensible, deterministic
heuristic, not a way to detect which card CUDA will actually pick — nvidia-smi carries no such
signal. On a box with two near-identical-capacity cards it can pick either one; the full
`gpu_devices[]` breakdown exists precisely so a consumer that needs the real per-card numbers
(or a smarter dispatcher) isn't limited to the headline guess. Implementation:
`fleetnode.ParseSmiMemoryDevices` / `fleetnode.HeadlineDevice` in `internal/fleetnode/vram.go`.

### `primary_gpu_uuid` — pin the headline device deterministically

The largest-total rule cannot tell two near-identical-capacity cards apart, and total VRAM says
nothing about which card CUDA will actually compute on. **Canonical guidance (CMP tier notes):
pin by GPU UUID, NEVER index.** Set `primary_gpu_uuid` to one of the UUIDs read off this node's
own `gpu_devices[]` (see the JSON example above) and that device — regardless of its total or
free VRAM — becomes the headline `vram_total_gb`/`vram_free_gb`:

```json
{ "primary_gpu_uuid": "GPU-2a44210f-6739-2d89-0e21-44cd5143faf7" }
```

On <node-b>, `gpu_devices[]` shows the RTX 5060 Ti at index 0 with the marginally larger total
(16311 vs 16303 MiB), so the default largest-total rule headlines it — but ComfyUI's CUDA
ordering actually computes on the RTX 5070 Ti (index 1, verified via ComfyUI's own
`/system_stats`). Pinning `primary_gpu_uuid` to the 5070 Ti's UUID
(`GPU-2a44210f-6739-2d89-0e21-44cd5143faf7`) makes the health payload finally describe the card
that is actually doing the work.

Behavior: unset (`""`, the default) = the largest-total rule, unchanged. Set and found among the
parsed devices = that device wins outright. The match is case-insensitive, and the config value
is whitespace-trimmed at load — both matter because the UUID is meant to be copy-pasted straight
out of `gpu_devices[]` above, and a copy can pick up a trailing newline or land in a different
case than nvidia-smi's own lowercase output. **Set but not found** (typo, or the card was
removed/reseated) = falls back to the largest-total rule **and** logs one
`[fleet-serve] warning: primary_gpu_uuid "..." not found among N parsed GPU device(s)...` line to
stderr (once per process lifetime, not once per 2s sampler tick) — a silent fallback would hide
a typo'd UUID forever. No effect on a single-GPU node beyond confirming what's already true (its
one `gpu_devices[]` entry is already the headline); no effect at all on the windows-generic
source, which has no `gpu_devices[]` to match against. Implementation:
`fleetnode.SelectHeadlineDevice` in `internal/fleetnode/vram.go`.

## Config keys

| Key | Default | Purpose |
|---|---|---|
| `fleet_listen` | `127.0.0.1:18811` | Bind address (`--listen` overrides). Port **18811** — the dispatcher owns 18810. |
| `fleet_node_id` | `""` | Node id in `/fleet/health`. Empty = the OS hostname at serve time (`--node-id` overrides). |
| `fleet_sampler` | `auto` | Per-render VRAM footprint source: `auto` \| `pdh` \| `pdh-shared` \| `global` (see [Sampler modes](#sampler-modes)). |
| `primary_gpu_uuid` | `""` | Pins the headline `vram_total_gb`/`vram_free_gb` to one card by nvidia-smi UUID, overriding the largest-total rule (see [`primary_gpu_uuid`](#primary_gpu_uuid--pin-the-headline-device-deterministically) above). Empty = unchanged largest-total behavior. |
| `fleet_agent_enabled` | `false` | Opts this node into executing fleet **agent** jobs (see [The agent task](#the-agent-task-task_type-agent)). Explicit opt-in: the binding (an agent seat) exists on every tier, the worker ROLE is a per-box decision. Off = the task is not advertised and health is byte-identical to a pre-0.63 node. |
| `fleet_auth_token` | `""` | Bearer token for the **agent lane only** (agent dispatches + polls of agent-created jobs; media stays tokenless in v1). Same value on every node and in the delegator's config. Empty + non-loopback listener = agent dispatches refused 403. |
| `agent_ctx_tokens` | `0` | The agent seat's served context window, advertised in health for the delegator's placement arithmetic. From config, never probed (a live probe could cold-start a multi-GB model on the health cadence). `0` = not advertised = this node is never chosen for remote agent work. |

## Binding guidance (read before exposing anything)

The **media** endpoints are unauthenticated by design (matching the dispatcher's posture):
anyone who can reach them can run renders on this GPU. The **agent lane is the exception**
since 0.63.0 — it executes caller-supplied agent contracts, so beyond loopback it requires
`fleet_auth_token` (details in [The agent task](#the-agent-task-task_type-agent)). The same
rules as `local-agent --serve` apply, enforced by the same shared guard:

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
| `pdh-shared` | The same PDH process tree, summing **Dedicated + Shared** Usage | The UMA/iGPU mode (J3): on unified memory (amd-rdna3 tier) allocations land in SHARED and the Dedicated counter reads ~0 — footprints would silently never record. The `amd-rdna3` config_seed sets this; discrete boxes should not (Shared is noise there). |
| `global` | `nvidia-smi` global `memory.used` delta from a baseline captured at render start | The whole GPU's swing during the render — includes anything else that allocated meanwhile. |
| `auto` (default) | PDH (Dedicated) on Windows, global-delta elsewhere | The right default once PDH is validated (below). |

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
| `agent` | `agent` | `fleet_agent_enabled` **and** a resolvable agent seat **and** (loopback listener **or** `fleet_auth_token` set) | none — llama-swap-resident text work, no render footprint |
| *(config-driven)* | `pipeline-job` | a valid `pipelines.<task_type>` entry (see below) | none — sizing rides on the task-scoped `Record("", "", task_type, peak)` entry |

run-graph payloads carry `graph` and `manifest` as **raw nested JSON** (no base64) and are
strict-validated at ack time — a malformed fleet job dies at the 400 with a clear reason,
never mid-render. Typed run-graph defers (`VENV_INCOHERENT`, `SATISFIER_SPAWN_FAILED`, `NODE_CLASS_MISSING`, …)
surface in the job's `error` field as `code: detail`.

## Pipeline-job task families

`pipelines` (`internal/config` `Config.Pipelines map[string]PipelineSpec`) lets this node
serve an **externally-provided pipeline CLI** as its own fleet `task_type` — 100%
config-driven, unlike every hardcoded row in the table above: adding a new pipeline needs no
code change, only a new `pipelines.<task_type>` entry:

```json
{
  "pipelines": {
    "scene-swap": {
      "script": "D:/Dev/dmmdea/creative-marketing-pipelines/scripts/run-scene-swap.mjs",
      "workdir": "D:/Dev/dmmdea/creative-marketing-pipelines",
      "timeout_sec": 2400,
      "artifacts": ["final.png", "qa-report.json"],
      "max_ref_mb": 24
    }
  }
}
```

`script`/`workdir`/`timeout_sec`/`artifacts` are required (an invalid entry is rejected LOUDLY
at config load, never silently at dispatch); `ingress_allow`/`max_ref_mb` are optional (default
allowlist / 24 MiB per ref). The task_type key (`"scene-swap"` above) is dispatched exactly
like any other fleet task — it is advertised in `/fleet/health`'s task list once the entry is
valid, and refused with the usual `unsupported task_type` 400 otherwise.

### Payload

```json
{
  "task_type": "scene-swap",
  "payload": {
    "job_spec": { "id": "web-1a2b3c4d", "background": { "mode": "generate", "...": "..." }, "...": "..." },
    "image_refs": {
      "product": "http://<node>:<port>/api/uploads/<name>.png",
      "logo": "http://<node>:<port>/api/uploads/<name>.png",
      "background": "http://<node>:<port>/api/uploads/<name>.png"
    },
    "tier": "16gb"
  }
}
```

**Ack-time validation** (every rule below is checked, and every ref fetched, BEFORE the job is
accepted — a bad payload or an unreachable ref is a 400, never a mid-render surprise):

- `job_spec` is required and must be a JSON object with a slug-valid `id`
  (`^[A-Za-z0-9_-]{1,64}$` — it becomes the materialization dir name and a filename prefix on
  every published artifact).
- `tier` is a required non-empty string (the CLI's own tier resolution is authoritative).
- `image_refs.product` and `image_refs.logo` are required; `image_refs.background` is
  required **iff** `job_spec.background.mode == "stock"`.
- `job_spec` must **not** already contain `product`, `logo`, or `background.path` — the node,
  never the dispatch payload, injects those from the fetched `image_refs` (a payload trying to
  set them itself is rejected outright).
- `job_spec.id` must **not** already have an in-flight job on this node: the materialization
  dir is created with an exclusive `os.Mkdir` (not `MkdirAll`), so a second dispatch sharing an
  id with a still-running job is refused at ack (`"job_spec.id ... already in flight on this
  node"`) rather than sharing the directory — which would otherwise let the first job's
  eventual cleanup delete the second job's still-live assets/`job.json` out from under it.
- Every `image_refs` URL goes through the same allowlisted/capped/magic-sniffed ingress every
  other fetched ref uses (tailnet CGNAT + loopback + this pipeline's `ingress_allow`; PNG/JPEG/
  WebP only; capped at `max_ref_mb`).

On success the node materializes `<base_dir>/pipeline-jobs/<job_id>/`: `assets/` (the fetched
refs), `job.json` (`job_spec` with `product`/`logo`/`background.path` injected as absolute
local paths), and `out/` (the CLI's `--out` root — the CLI itself creates `out/<job_id>/` and
writes its artifacts there). This whole directory is removed once the job reaches a terminal
state (success or failure) — never before, since the CLI reads `job.json` and the fetched
assets for the entire run.

### Result + publishing

```go
type pipelineJobResult struct {
    FinalPath    string  `json:"final_path"`               // FIRST key — becomes artifacts[0]
    QaReportPath string  `json:"qa_report_path,omitempty"` // artifacts[1], when produced
    JobID        string  `json:"job_id"`
    Tier         string  `json:"tier"`
    DurationSec  float64 `json:"duration_sec"`
}
```

`final_path` is marshaled **first on purpose** — the web viewmodel harvests artifacts in
insertion order, so `artifacts[0]` must be the primary render. Every configured `artifacts[i]`
is copied from `out/<job_id>/<name>` to `MediaDir` as **`<job_id>-<name>`** (flat, bare
filenames — `GET /fleet/media/{name}` rejects any separator, same as every other published
render). The **primary** artifact (`artifacts[0]`) missing is an error; any other
(**optional**, e.g. `qa-report.json`) artifact missing is silently omitted from the result,
never an error.

The JSON result only ever **names** `artifacts[0]` (`final_path`) and `artifacts[1]`
(`qa_report_path`) — the binding is by **index**, not by "whichever artifacts happened to be
present": if `artifacts[1]` is missing but a `artifacts[2]` exists, `qa_report_path` stays
absent (never silently reports `artifacts[2]`'s path instead). A 3rd-or-later configured
artifact is still copied to `MediaDir` under its own `<job_id>-<name>` name when present, it is
simply never referenced in the result JSON — a consumer that configures more than two
artifacts must know the extra names out of band (e.g. a fixed convention agreed with the
pipeline's own docs).

A child CLI failure (non-zero exit) is surfaced as a **plain failure**, not a defer — a fleet
job has no interactive caller to hand work back to. When the CLI's stderr carries the CMP CLI
contract's `SCENE-SWAP-FAIL stage=<...>: <message>` line, that line becomes the job's `error`
verbatim; otherwise the generic exec error (including a timeout-kill) is used.

### GPU concurrency caveats (read before dispatching two pipeline jobs at once)

- **No machine-wide GPU lease is taken in Go for a pipeline job** — only this process's
  single in-process media slot. The externally-provided CLI's own nested per-stage calls
  acquire the machine-wide lease themselves, exactly like a manual run of the same pipeline
  today; taking the machine lease again here would self-deadlock against that nested
  acquisition. Practical effect: **a second concurrent pipeline-job dispatch to this node
  waits behind the first for up to `gpu_wait_ms`, then fails as `gpu_busy`** — same shape as
  every other GPU route's busy-card behavior, just arbitrated one level higher (in-process
  only, not across processes).
- **A node restart loses an in-flight pipeline job** exactly like every other fleet task
  (see Known limits below): survivors are marked terminal `error:"interrupted"` on drain: an
  in-flight run may leave partial output under its `pipeline-jobs/<job_id>/out/` — the
  dispatcher's failure handling (re-dispatch elsewhere) is the recovery path, not a node-side
  resume. A **graceful** stop (SIGINT, drained) removes nothing extra — the job's own
  directory is cleaned up by its normal cleanup path once the drain marks it terminal. An
  **ungraceful** stop (crash, `kill -9`, power loss) leaves the directory behind with no
  in-memory record of it at all; since `job_spec.id` collisions are guarded by an exclusive
  directory create (see above), that orphaned directory would otherwise refuse EVERY future
  dispatch reusing the same `job_spec.id`, forever. `fleet-serve` sweeps
  `<base_dir>/pipeline-jobs/`'s contents once at startup, **before** it starts listening —
  every directory present at that instant is orphaned by definition (this process has not
  accepted a single dispatch yet) — and logs how many it removed.

## The agent task (`task_type: "agent"`)

An **agent** job carries a self-contained delegation contract; the node runs it with its own
local read-only agent loop (planner = this box's `agent_model`, falling back to the workhorse;
no write/run/fetch/github tools, no delegate tool) over the contract's inlined context docs,
then returns a versioned result. Full behavior:
[systems/fleet-node.md](systems/fleet-node.md#the-agent-task-task_type-agent); the decisions:
[ADR 0023](architecture/decisions/0023-agent-lane-tailnet-auth-and-locality.md). Enable recipe
for both roles: [OPERATOR-GUIDE.md](OPERATOR-GUIDE.md#delegate-subtasks-across-fleet-nodes-agent_delegate--delegate).

### Dispatch payload — the contract

The envelope is the normal `{"job_id", "task_type": "agent", "payload": {…}}`; the payload is
one contract. Unknown payload fields are **ignored** (staggered node deploys must not flag-day);
`schema_version` skew and the size caps refuse loudly at ack (400).

| Field | Type | Meaning |
|---|---|---|
| `schema_version` | int | must be `1`; anything else is a 400 at ack |
| `goal` | string | **required** — the self-contained task; the sub-agent sees only this + `context` |
| `context` | `[{name, text}]` | inline docs, ≤ 16, ≤ 256 KiB total; `name` = flat filename (no separators/colons, no duplicates) — each becomes a file the sub-agent can read |
| `output_schema` | object | JSON Schema with a `properties` map of string/number/integer/boolean/string-array/enum fields. **Required** — an agent dispatch without one is refused at ack |
| `acceptance` | `[string]` | delegator-evaluated checks: `contains:<s>` \| `not_contains:<s>` \| `regex:<re>` \| `min_items:<field>:<n>` \| `nonempty:<field>`; malformed or unfalsifiable checks are a 400 |
| `profile` | string | agent task profile, default `research`; unknown names defer naming the valid set |
| `max_steps` | int | default 12, clamped to 12 |
| `timeout_sec` | int | default 300, clamped to 900; enforced node-side as a hard wall deadline |
| `depth` | int | advisory — the node executes anything off the wire at `max(1, depth)`, so a wire "origin" claim is never trusted |

### Job result — what `data` holds on `done`

| Field | Type | Meaning |
|---|---|---|
| `schema_version` | int | `1` |
| `node_id` | string | this node (`fleet_node_id`, else hostname) |
| `seat` | string | the planner model that ran |
| `output` | string | final assistant text (kept even when the structured re-pack failed) |
| `structured` | object | present iff `output_schema` given AND the post-loop grammar re-pack validated (one retry) |
| `steps` / `stop_reason` | int / string | loop telemetry |
| `deferred` | bool | the node ran and could not complete the contract — **still a `done` job**, never `error` (`error` = internal wiring bug only) |
| `reason` | string | the defer shape: seat unserved on the roster, `wall timeout after <N>s`, `step budget exhausted (…)`, `output failed schema: …`, build/loop/profile errors |
| `wall_ms` / `tokens_out` | int | node wall clock / re-pack completion tokens |

No transcript field exists — remote reasoning never crosses the wire.

### Auth — the one token-gated lane

- `fleet_auth_token` set → agent dispatches **and** polls of agent-created jobs require
  `Authorization: Bearer <token>` (constant-time compared); wrong/missing → `401` with the
  standard error envelope (`"error": "unauthorized"`), answered before the job_id validation
  and the known-job lookup, so an unauthorized caller can neither probe field validation nor
  learn job state.
- Token empty + non-loopback listener → agent dispatches are refused
  `403 agent lane requires fleet_auth_token on a non-loopback listener`, and `agent` is
  withheld from the advertised `supported_task_types`. Loopback + no token stays open (same
  trust boundary as the local MCP surface).
- **Media dispatch, media job polls, `/fleet/media/*`, and health never check the token** —
  deployed tokenless media clients keep working byte-identically. Whole-fleet enforcement is a
  recorded follow-up for a coordinated whole-fleet deploy window (ADR 0023).

### Health advertisement (only when `fleet_agent_enabled`)

```json
"agent_enabled": true, "agent_seat": "offload-e4b",
"agent_ctx_tokens": 16384, "agent_seat_resident": true
```

`agent_ctx_tokens` comes from config (`0` = omitted = the delegator never places here).
`agent_seat_resident` is a cached, alias-aware roster probe refreshed in the background at most
once per 30 s — **fail-closed**: `false` until the first probe lands, and `false` again on any
probe failure (a stale "resident" while llama-swap is down would route work at a node that
cannot run it; `false` only costs a conservative local placement).

### Job ids and polling (what a delegator does)

Agent job ids are delegator-minted (`agd-` + 24 random hex chars). Dispatch doubt is retried
once under the **same id** — the store re-acks `202` idempotently, so a lost ack never buys a
second run. The delegator polls every 3 s; a poll `404` triggers a bounded re-dispatch of the
same id (max 2); past `timeout_sec` + 60 s grace it marks the subtask deferred
(`poll deadline`) and stops — the node may still finish server-side, and the job id in the
delegation log lets you reconcile by hand.

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
  field addition needs a node upgrade first. (The agent contract **inside** the envelope is
  the deliberate exception — its payload decoder ignores unknown fields so staggered node
  deploys interoperate; version skew is caught by its explicit `schema_version` instead.)
- **Payload paths (`out` / `out_dir` / `still` / `audio`) are node-local writable paths**,
  taken as given. That's the tailnet-trust posture restated: anyone who can dispatch can
  already run renders; don't extend reach beyond the tailnet.
- `priority` is accepted and ignored (contract-reserved).
- **Media lanes carry no auth** — the trusted-network posture above is the boundary; revisit
  if the fleet ever leaves the tailnet. The agent lane is bearer-gated (see
  [The agent task](#the-agent-task-task_type-agent)); its accepted v1 weaknesses — one shared
  token, no rotation — are recorded in
  [ADR 0023](architecture/decisions/0023-agent-lane-tailnet-auth-and-locality.md).
