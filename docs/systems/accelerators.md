# Accelerators

## Purpose

Devices that ride **beside** the GPU tier: a box's `profile` stays one string, and
`accelerators: []` lists the additive compute devices found next to it
([ADR 0024](../architecture/decisions/0024-accelerators-are-additive-to-the-gpu-tier.md)).
Today the harness declares exactly one — the Hailo-8L NPU — served through an on-demand loopback HTTP
sidecar the harness spawns and that exits itself when idle.

## Questions this doc answers

- What is an accelerator here, and why is it not a tier?
- How does a box get detected as having one, and how do I force it on a bench?
- Which config keys does an accelerator seed, and who merges them?
- How does the sidecar start, answer, and stop?
- Which tools does the NPU own, and which stay on the GPU?

## Scope

Detection, config seeding, the sidecar runtime and wire contract, the NPU tool surface and its
ownership boundary, and status reporting.

## Non-scope

- The sidecar's own implementation (models, HEFs, pipelines) — that lives in the Hailo repo
  (Hailo-8L-Analysis-Pipelines), not here.
- GPU tier selection and serving → [setup-installer.md](setup-installer.md)
- The GPU vision seats (VQA, GPU OCR) → [offload-pipeline.md](offload-pipeline.md)

## What an accelerator is

One `profile` string used to travel end-to-end, and a second device had no representation:
vision routed only to llama-swap by model id. [ADR 0024](../architecture/decisions/0024-accelerators-are-additive-to-the-gpu-tier.md)
makes accelerators **additive**: each is declared in `setup/templates/profiles.json` under the
`accelerators` map (beside `profiles`), lists the capabilities it **owns exclusively**, and is
carried as an id list in `installed.json`, `hwdetect.Verdict`, the harness config, and
`/fleet/health` — all `omitempty`, so a box without one serialises those payloads byte-identically
to before the field existed. Tool REGISTRATION is likewise unchanged when the list is empty (no
tool added or removed), with one universal exception: `offload_ocr`'s schema gains an optional
`engine` parameter on every box (see Tools and ownership below).

## Detection

The rule (Go `hwdetect.AcceleratorsFromHailortcli` is authoritative; `Get-Accelerators` in
`setup/detect.ps1` is the PowerShell mirror — change Go first):

1. `hailortcli scan` lists at least one `Device:` line, **and**
2. `hailortcli fw-control identify` reports `Device Architecture: HAILO8L`.

Both must hold → `["hailo-8l"]`. A missing `hailortcli`, a failed probe, or a full Hailo-8
(different HEF build — `hailo8/` vs `hailo8l/` model-zoo artifacts) all mean "no accelerator",
never an error: most boxes have no NPU and that is the normal case. `hwdetect.DetectAccelerators`
runs the probe with an injected runner (`install detect` / `install plan` wire the real
`hailortcli`); detection is a separate probe the installers run and merge into the verdict —
`Classify` itself never fills it.

**Override:** `OFFLOAD_ACCELERATORS` (comma-separated ids) replaces the probe in `detect.ps1`
and `install.ps1`, so an install can be tested or forced without the physical device on the
bench.

## Seeding

`profiles.json` `accelerators.<id>.config_seed` carries the config keys the device needs.
`internal/tierseed.ResolveAccelerators` is the **single authority** on the merge rule
(`Get-AcceleratorSeed` in `setup/install.ps1` is the PowerShell parity copy for the
no-Go-binary path): merge the listed ids' seeds, validate every key against `config.Config`,
and expand tokens — `__HAILO_HOME__` (the Hailo repo checkout) plus the usual
`__OFFLOAD_HOME__`/`__EXE__`. An id detected but not declared in the table is an authoring
error and fails loudly.

`HAILO_HOME` is the installer env var behind `__HAILO_HOME__`; default
`<OFFLOAD_HOME>/hailo` (`Join-Path $HOME_DIR 'hailo'`). It is always non-empty — an empty
value would expand `__HAILO_HOME__` to `""` and silently produce a plausible-wrong
`/hailo-http.cmd`.

The accelerator seed merges **AFTER** the tier seed (and after every tier overlay), so an
accelerator key can never be overwritten by the GPU tier's own seed — and an accelerator on a
profile-less render still seeds, because accelerators are additive to the tier, not part of it.
The installer also writes `accelerators` into `installed.json`, which `fleet-serve` advertises
verbatim in `/fleet/health` so a delegator can route NPU-owned work to the box.

The seeded keys (hailo-8l):

| Key | Meaning |
|---|---|
| `accelerators` | `["hailo-8l"]` — THE gate (`config.HasAccelerator`) for tool registration and status |
| `hailo_endpoint` | sidecar base, `http://127.0.0.1:18813` — loopback only |
| `hailo_sidecar_cmd` | launcher (`__HAILO_HOME__/hailo-http.cmd`); empty = never spawn, defer when down |
| `hailo_timeout_sec` | one NPU call's bound, default 60 (cold HEF load is ~1–8 s) |
| `hailo_idle_sec` | passed to the sidecar as its self-exit idle window, default 300 |

### Coral Edge TPU detection (0.114.0)

`hwdetect.DetectCoral` reports `["coral-edgetpu"]` iff `/sys/class/apex/apex_0/status` reads
`ALIVE` (the gasket/apex driver's status node; Linux only — Windows has no apex driver and
never matches). Any read error is "no accelerator". `hwdetect.DetectAllAccelerators` is the
union of the Hailo and Coral probes **in that order**, and the order is load-bearing: it is
the order the ids land in `config.Accelerators`, and the shared-name rule below gives a
capability name to the first listed owner. `install detect` uses the union on both platforms;
`OFFLOAD_ACCELERATORS` overrides both probes.

The seeded keys (coral-edgetpu):

| Key | Meaning |
|---|---|
| `accelerators` | `["coral-edgetpu"]` — the gate |
| `coral_endpoint` | sidecar base, `http://127.0.0.1:18814` — loopback only, distinct from the Hailo's 18813 |
| `coral_sidecar_cmd` | launcher; the harness runs it as `<cmd> --idle-sec <coral_idle_sec>` (`accelclient.SpawnCmd`); empty = never spawn, defer when down |
| `coral_timeout_sec` | one TPU call's bound, default 30 (a cold model load on the TPU is ~0.5–2 s) |
| `coral_idle_sec` | the sidecar's self-exit idle window, default 300 |

`CORAL_HOME` (`install seed --coral-home`, default `<OFFLOAD_HOME>/coral`) holds the sidecar's
own venv (`venv/`, built from the box's staged cp314 wheels: ai-edge-litert, numpy, pillow),
its `models/`, and `accelerators/coral/` either copied flat (the seed's `__CORAL_HOME__/coral-http.sh`)
or checked out beneath it (`<home>/accelerators/coral/`, the Lenovo) — the launcher finds the home
by walking up to `venv/`. An empty `__HAILO_HOME__`/`__CORAL_HOME__`
is **refused** at seed time (0.114.0) instead of rendering a launcher at the filesystem root.

`install.sh` merged no accelerator seed at all until 0.114.0 — install.ps1 always had. It now
passes detect's verdict to `install seed --accelerators` and writes `installed.json`; and
`fleet-serve` falls back to `config.accelerators` when the manifest lists none, so a hand-built
node (the Lenovo has no `installed.json`) still advertises its device in `/fleet/health`.

## Runtime — the sidecar

The sidecar is the Hailo repo's `server/http_server.py`, bound to loopback
`127.0.0.1:18813` only — it is not an authenticated service and must never listen wider. Wire
contract:

- `GET /health` → the sidecar's status dict, 200.
- `POST /v1/<tool>` → the tool's result dict, 200. A 200 carrying `{"error":true,...}` is a
  **structured result** (the tool refused the input), not a transport error — it passes
  through to the caller verbatim.
- 404 `unknown_tool`; 400 `bad_request`.
- The process **self-exits** after `HAILO_SIDECAR_IDLE_SEC` seconds idle (the harness passes
  the config's `hailo_idle_sec` through on spawn).

`internal/accelclient` (was `hailoclient` until 0.114.0) is the harness's lane: `Client` (pure net/http, mirrors `nimclient` —
a result is a map the caller shapes) and `Sidecar` (`Ensure` is the single entry point every
NPU tool calls first: healthy → no-op; down + spawnable → spawn **once**, detached and
window-hidden, then poll `/health` until the start timeout; down + no `hailo_sidecar_cmd` →
`ErrNoSidecarCmd`). Concurrent first calls share one spawn; transport and spawn failures
become defers, so the calling agent does the task another way.

## Tools and ownership

Registered **only** when the box lists the device (`HasAccelerator("hailo-8l")`), so no NPU
tool appears in `tools/list` elsewhere (the universal changes on every box are
`offload_ocr` and `offload_transcribe` each gaining an optional `engine` parameter). Each
maps 1:1 to a sidecar tool:

**Both surfaces carry the same set (0.86.0):** the MCP server registers the tools for
Claude, and the agent loop registers the same 11 (plus the loop OCR `engine` param) for
`agent_run`/`agent_delegate`/`local-agent` — injected as `agent.NPUFunc` via
`pipeline.NewLoopNPU`, gated identically, so a delegated contract on an accelerator box can
use the NPU while the same contract on any other node advertises no such tool.

| MCP tool | Sidecar tool | Result |
|---|---|---|
| `offload_face_detect` | `face_detect` | faces + 5-landmark keypoints |
| `offload_face_embed` | `face_embed` | per-face 512-d ArcFace identity embeddings |
| `offload_object_detect` | `object_detect` | 80 COCO classes, YOLOv8s with on-chip NMS |
| `offload_person_embed` | `person_embed` | person re-id vectors (OSNet 512-d), no face needed |
| `offload_depth` | `depth` | preview-grade relative depth PNG (Depth-Anything-V2) |
| `offload_enhance_low_light` | `enhance_low_light` | Zero-DCE brightening at source resolution |
| `offload_image_embed` | `embed` | 512-d TinyCLIP image embedding |
| `offload_pose` | `pose` | people with 17 named COCO keypoints (YOLOv8s-pose, host decode) |
| `offload_segment` | `segment` | instance masks + id-map PNG; `everything=true` = FastSAM class-agnostic |
| `offload_text_embed` | `text_embed` | text vectors ON the NPU, same 512-d space as `offload_image_embed` (siglip2 optional) |
| `offload_zero_shot` | `zero_shot` | free-text labels → ranked similarities, both towers on-NPU |

Plus `offload_ocr` gains `engine:"npu"` (the Hailo PaddleOCR path) and `offload_transcribe`
gains `engine:"npu"` (whisper-base fast preview) — both **explicitly caller-selected**,
never an automatic fallback. Whisper-on-NPU is PLATFORM-BLOCKED on Windows HailoRT 4.24
(the sidecar returns a typed diagnosis; Linux-validated upstream), so `engine:"npu"`
transcription on such a box defers honestly with the reason.

Ownership when both devices are present:

| Capability | Owner | Why |
|---|---|---|
| Structured vision outputs (face detect/identity, object detect, re-id, depth, low-light, image embeddings) | **NPU, exclusively** | boxes/vectors/maps are what the device natively produces, fast and free |
| Language about images (VQA, description) | **GPU** (the tier's VLM) | untouched by this feature |
| OCR | **GPU primary**; `engine:"npu"` explicit | the engines read stylised text differently — a silent switch would change results |

### Coral tools and the shared-name rule (0.114.0)

The Coral owns four capabilities, each on an artifact from `google-coral/test_data`
(Apache-2.0) that the sidecar verifies by sha256 (`accelerators/coral/models.json`):

| MCP / loop tool | Sidecar tool | Model (default; options) | Result |
|---|---|---|---|
| `offload_classify_image` **(new capability)** | `classify` | `domain=imagenet` → EfficientNet-EdgeTPU-S (1000 ImageNet); `birds`/`insects`/`plants` → MobileNet v2 1.0/224 iNat | `{results:[{label,score}],best,model,domain}` top-k (default 5) |
| `offload_object_detect` *(name shared with Hailo)* | `object_detect` | EfficientDet-Lite0 320 COCO; `size=lite1\|lite2` | `{objects:[{label,class_id,x,y,w,h,score}],count}` in image pixels |
| `offload_semantic_segment` **(new capability)** | `semantic_segment` | DeepLabV3 MobileNet v2 Pascal (21 classes) | per-pixel class-id PNG at `mask_path`; `{mask_path,classes:[{class_id,label,pixels}],width,height}` |
| `offload_image_embed` *(name shared with Hailo)* | `embed` | EfficientNet-EdgeTPU-S embedding extractor | `{embedding,dim:1280,space:"efficientnet-edgetpu-s"}` |

Deliberately **not** owned by the Coral: `text_embed` and `zero_shot` (no text tower exists for
the Edge TPU — the EfficientNet space is not CLIP, and the tool description says so), the
`face_*`, `pose`, `person_embed`, `depth`, `enhance_low_light` tools, and `segment` (instance
segmentation; the Coral's DeepLab is semantic, hence the distinct name — the two outputs are
not interchangeable). The `engine:"npu"` switches on `offload_ocr` / `offload_transcribe` stay
Hailo-only.

**Shared-name rule ([ADR 0037](../architecture/decisions/0037-a-capability-name-has-one-owner-per-box.md)).**
`offload_object_detect` and `offload_image_embed` are capability names, and a capability has
exactly one owner per box: registration walks `config.Accelerators` **in order** and the first
listed accelerator that owns a name registers it; a later one is skipped for that name and
logged once at startup. Both surfaces apply it identically — `mcpserver.registerAccelTools`
and `agent.accelLaneTools` — and it is tested in both orders. Today no box lists both devices,
so the rule is a pinned invariant, not a live path. The tool description names the device that
serves it, and `offload_image_embed` reports `space` so a caller never mixes a 1280-d
EfficientNet vector with a 512-d TinyCLIP one.

**Both surfaces, generalised (0.114.0):** the MCP server keeps one per-device table
(`internal/mcpserver/acceltools.go`) and one on-demand sidecar per device; the agent loop
receives every device as an `agent.AccelLane` (`pipeline.NewLoopAccel`, config order) and
registers the same tables through `ReadOnlyToolsWithLanes`. The pre-Coral single-lane `NPU`
injection still works for every existing caller and test.

## Status

`offload_status` gains an `accelerators` block — present only when the box lists one. For
hailo-8l: the endpoint, whether a sidecar command is configured, the owned tool list, and a
**live health probe that never spawns the sidecar** (status stays side-effect free). A
`health_error` between uses is normal — the sidecar self-exited.

For coral-edgetpu the same block carries the endpoint, whether a launcher is configured, the
four owned tools, and the sidecar's `/health` — `{enabled, device:"/dev/apex_0", status,
temp_c, loaded:[...], models_missing:[...], runtime:{litert, libedgetpu}}` — read from sysfs
`status`/`temp` (never written).

## Limits

- **Single in-flight inference** — one sidecar process serialises NPU access by construction.
- **NPU calls are not in the savings ledger in v1** — recorded follow-up.
- Windows cannot see the device as an "NPU" (no MCDM driver) — irrelevant to this route, which
  reaches the device through HailoRT via the sidecar, not through Windows ML.

## Verifying on a box

On a box with the device (config seeded, sidecar repo checked out):

1. `offload_status` → `accelerators.hailo-8l.endpoint` present; a first-call `health_error`
   (not running) is expected.
2. `offload_face_embed` on a real photo → `{faces:[{…, embedding:[512 floats]}], count}`;
   right after, `curl http://127.0.0.1:18813/health` shows non-empty loaded networks (the NPU
   ran, not a cache).
3. `offload_ocr` with `engine:"npu"` → PaddleOCR text; without `engine` → the GPU path,
   unchanged.
4. Wait `hailo_idle_sec` + 10 s → the sidecar process is gone; the next NPU call spawns it
   again (cold ~2 s + HEF load).
5. On a box **without** the device: `tools/list` is unchanged.

## Source map

- [`internal/accelclient/hailoclient.go`](../../internal/accelclient/accelclient.go) — the
  loopback client
- [`internal/accelclient/sidecar.go`](../../internal/accelclient/sidecar.go) — `Ensure`,
  `SpawnCmd`, the detached window-hidden spawn
- [`internal/mcpserver/mcpserver.go`](../../internal/mcpserver/mcpserver.go) — gated tool
  registration, `handleHailoTool`, the `offload_ocr` engine switch, the status block
- [`internal/config/config.go`](../../internal/config/config.go) — `Accelerators`,
  `HasAccelerator`, `hailo_*` keys and defaults
- [`internal/hwdetect/classify.go`](../../internal/hwdetect/classify.go) — detection
- [`internal/tierseed/tierseed.go`](../../internal/tierseed/tierseed.go) —
  `ResolveAccelerators`
- [`internal/fleetnode/gpuinfo.go`](../../internal/fleetnode/gpuinfo.go),
  [`internal/fleetnode/server.go`](../../internal/fleetnode/server.go) — manifest read,
  health advertisement
- [`setup/templates/profiles.json`](../../setup/templates/profiles.json) — the
  `accelerators` map
- [`setup/detect.ps1`](../../setup/detect.ps1) — `Get-Accelerators`,
  `OFFLOAD_ACCELERATORS`
- [`setup/install.ps1`](../../setup/install.ps1) — `Get-AcceleratorSeed`, `HAILO_HOME`,
  manifest write

## Related docs

- [ADR 0024](../architecture/decisions/0024-accelerators-are-additive-to-the-gpu-tier.md) —
  the decision record
- [mcp-server.md](mcp-server.md) — the tool surface this extends
- [setup-installer.md](setup-installer.md) — detection and seeding in the install flow
- [fleet-node.md](fleet-node.md) — the health payload that advertises the list
