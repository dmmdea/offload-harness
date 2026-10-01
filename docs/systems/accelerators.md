# Accelerators

## Purpose

Devices that ride **beside** the GPU tier: a box's `profile` stays one string, and
`accelerators: []` lists the additive compute devices found next to it
([ADR 0024](../architecture/decisions/0024-accelerators-are-additive-to-the-gpu-tier.md)).
Today the harness declares three — the Hailo-8L NPU, the Coral Edge TPU and the Rockchip RK3588
NPU (`rknpu`) — each served through an on-demand loopback HTTP sidecar the harness spawns and that
exits itself when idle.

## Questions this doc answers

- What is an accelerator here, and why is it not a tier?
- How does a box get detected as having one, and how do I force it on a bench?
- Which config keys does an accelerator seed, and who merges them?
- How does the sidecar start, answer, and stop?
- Which tools does the NPU own, and which stay on the GPU?
- How is the Rockchip RK3588 NPU detected on a vendor and on a mainline kernel?

## Scope

Detection, config seeding, the sidecar runtime and wire contract, the NPU tool surface and its
ownership boundary, and status reporting.

## Non-scope

- The Hailo sidecar's own implementation (models, HEFs, pipelines) — that lives in the Hailo repo
  (Hailo-8L-Analysis-Pipelines), not here. The Coral's sidecar is `accelerators/coral/` and the
  RKNPU's `accelerators/rknpu/`; this doc holds their contract, not their code.
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
in `/fleet/health` — except the Hailo-8L, which stays local-only (see Fleet routing below) — so a
delegator can route NPU-owned work to the box.

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
The Hailo-8L is the one exception: it is local-only and no source publishes it (see Fleet routing
below).

### Rockchip RK3588 NPU detection and seed (`rknpu`)

`hwdetect.DetectRknpu` reports `["rknpu"]` iff one of a fixed set of sysfs uevent files carries the
line `DRIVER=RKNPU` — Rockchip's NPU platform driver, matched as a whole line and case-sensitively.
Which file carries it depends on the kernel, so both layouts are read (Linux only: Windows has no
sysfs and never matches):

| Kernel | Where the driver names itself | Files read |
|---|---|---|
| Vendor 6.1 BSP (driver built in) | the NPU platform device is its DRM node's own device; the `cardN` it takes follows probe order (the display subsystem is `card0`, the NPU `card1` on the reference Orange Pi 5, RK3588S) | `/sys/class/drm/card0` … `card3/device/uevent` |
| Mainline (7.0 measured) with the out-of-tree `rknpu` module (DKMS, 0.9.8) | the NPU's DRM card (`card2` there) hangs off a virtual `/sys/devices/rknpu` whose uevent is **empty**; the line is on the NPU's three core platform devices (`OF_COMPATIBLE_0=rockchip,rk3588-rknn-core`), and any one bound is enough | `/sys/bus/platform/devices/fdab0000.npu`, `fdac0000.npu`, `fdad0000.npu` `/uevent` |
| Mainline with the in-tree `rocket` driver | the same core devices report `DRIVER=rocket` | **not a match**: the RKNN runtime does not run on it, so listing it would register tools whose first call can never succeed |

A read that merely succeeds proves nothing — every DRM card reads, the Mali GPU's
(`DRIVER=panthor`) included — so the driver line is the whole rule. `DRIVER=RKNPU2`,
`DRIVER=rknpu` and an `OF_COMPATIBLE` string with no driver bound all read as "no accelerator", and
so does an unreadable candidate. `DetectAllAccelerators` lists the RKNPU **after** the Hailo and the
Coral (the order is the shared-name rule's), and `install detect` / `install plan` hand it the same
injected reader as the Coral probe. `OFFLOAD_ACCELERATORS` overrides the probe here as for the other
devices. `setup/detect.ps1` carries no mirror of this probe: the hardware is Linux-only, and
`install.sh` takes its accelerator list from the Go verdict.

The seeded keys (rknpu):

| Key | Meaning |
|---|---|
| `accelerators` | `["rknpu"]` — the gate |
| `rknpu_endpoint` | sidecar base, `http://127.0.0.1:18815` — loopback only, after the Hailo's 18813 and the Coral's 18814 |
| `rknpu_sidecar_cmd` | launcher (`__RKNPU_HOME__/rknpu-http.sh`); the harness runs it as `<cmd> --idle-sec <rknpu_idle_sec>`; empty = never spawn, defer when down |
| `rknpu_timeout_sec` | one NPU call's bound, default 60; a forwarded call is cut off at `accelremote.Budget` (150 s) whatever this says, and a cold start can spend 45 s of it |
| `rknpu_idle_sec` | the sidecar's self-exit idle window, default 300, clamped by the sidecar to 1..300 (0, negative and larger values mean 300) — also how long a loaded model holds system RAM |

`RKNPU_HOME` (`install seed --rknpu-home` and `install render --rknpu-home`, default `<OFFLOAD_HOME>/rknpu`; the
rkllm seat's default launcher `__RKNPU_HOME__/rkllm-serve.sh` follows it, so the seat and the sidecar never split) holds the sidecar's own
`venv/`, its `models/` and the sidecar itself (`accelerators/rknpu/`, copied flat or checked out
beneath it); an empty home is refused at seed time like the other two. `install seed`,
`install plan` and `audit-config` all resolve accelerator seeds through one helper
(`accelOptions`) that fills every device's home from flag, environment and `<home>/<device>` —
`install plan` used to supply only the Hailo's, so it failed on any box that also listed a Coral.
A box carrying several devices is seeded `accelerators` = the union of their ids, in detection
order; the seeds used to merge key by key, so the last device's list replaced the others' and the
config gated on one device while `installed.json` advertised both. `audit-config` owns the
accelerator keys and compares a node with the seed of the devices it lists.

#### The RKLLM seat's repeat penalty and task set (0.153.0)

The tier's `rkllm` seat (`accelerators/rknpu/rkllm_server.py` behind `rkllm-serve.sh`, not the sidecar) carries
two settings declared on the seat in `profiles.json`; each renders into the seat's command or binds into the
node config from that one declaration.

- **`repeat_penalty`**, rendered as `--repeat-penalty` (0.01 to 10, rkllm seats only; unset renders no flag).
  It is the seat's default for a request that sends neither `repeat_penalty` nor `repetition_penalty`; a value the
  request sends wins, so a caller that wants the runtime's 1.0 sends 1.0. The tier ships 1.1, because greedy
  decoding at 1.0 looped on VQA until the 256-token cap (the vision lane deferred "vision output truncated"),
  while at 1.1 a blind four-question VQA check scored 3/4 (measured 2026-09-30). It is the only sampling default
  a seat may declare: `temp`, `top_p` and `top_k` stay refused on an rkllm seat.
- **`tasks`**, the task names the seat serves (`vqa`, `ocr`, `assess_image`, `classify`, `extract`; `classify`
  and `extract` are validated now and bound by a later release). The vision subset becomes the node's
  `vision_tasks` key. The tier declares `["vqa", "ocr"]`: the RKLLM runtime cannot constrain sampling (the
  server answers a `grammar` or `json_schema` with 400 `constrained_decoding_unsupported`) and `assess_image`
  always sends a grammar. The node refuses `assess_image` at ack time and a delegator places it elsewhere; see
  [FLEET-NODE.md](../FLEET-NODE.md#the-vision-task-post-fleetvision). The text subset (`classify`, `extract`)
  becomes the node's `text_tasks`, which opens the fleet text lane; the tier declares none (the lane ships dark,
  see [FLEET-NODE.md](../FLEET-NODE.md#the-text-task-post-fleettext)).
- **`unconstrained_seats`** is written for every `rkllm` seat (its name and aliases), whatever its `tasks`: the
  runtime cannot constrain decoding, so the node's own pipeline sends it no grammar and validates the reply
  strictly instead ([ADR 0069](../architecture/decisions/0069-an-unconstrained-seat-runs-classify-and-extract-from-the-prompt-and-the-text-lane-ships-dark.md)).

The tier also seeds limits for an NPU that runs one generation at a time: `fleet_max_concurrent_jobs` 1,
`request_timeout_sec` 240 (below the delegator's 300 s fleet vision budget), `max_input_chars` 8000 and
`ocr_max_tokens` 512. `fleet_max_queue_depth` stays at its default, twice the concurrency (2: one running, one
waiting), inside the server's own window of one running and two waiting. Numbers and reasons:
[ADR 0062](../architecture/decisions/0062-rk3588-soc-tier-serves-from-the-npu-on-a-unified-memory-budget.md).

**Upgrading an installed node (0.153.0).** The binary does not ship `rkllm_server.py`; it is the `accelerators/rknpu`
copy under `RKNPU_HOME`, which `install.sh` never refreshes. A 0.153.0 render emits `--repeat-penalty 1.1`, which a
0.151.1 server refuses (`unrecognized arguments`), so refresh `accelerators/rknpu` in `RKNPU_HOME` (at least
`rkllm_server.py`) first and only then re-render `llama-swap.yaml`. `install.sh` also leaves an existing `config.json`
untouched, so add `unconstrained_seats` (the seat's model id, 0.154.0), `vision_tasks` (`["vqa","ocr"]`), `fleet_max_concurrent_jobs` 1, `request_timeout_sec` 240,
`max_input_chars` 8000 and `ocr_max_tokens` 512 by hand, or delete the file to regenerate it; `local-offload audit-config`
lists them as SEED-ONLY.

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

**Input quantisation (0.151.1).** Every Coral model takes quantised uint8 pixels, and the sidecar feeds
them with pycoral's rule `q = (px - 128) / (128 * scale) + zero_point` (`_set_input`). Where a model's
input quantisation is the raw pixel to within one step (the iNat MobileNets and DeepLab at scale 1/128,
zero point 128; EfficientDet-Lite at 1/128, 127) the pixels pass through untouched. EfficientNet-EdgeTPU-S
(`classify` imagenet and `embed`) quantises with scale 0.012566 and zero point 131: before 0.151.1 it was
fed raw pixels and scored 41.6 % top-1 on 1000 ImageNetV2 images, 63.9 % with the rescale, the same as its
CPU twin. `embed` vectors from before 0.151.1 are not comparable with later ones.

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
and `agent.accelLaneTools` — and it is tested in both orders. The tool description names the
device that serves it, and `offload_image_embed` reports `space` so a caller never mixes a 1280-d
EfficientNet vector with a 512-d TinyCLIP one.

**Naming the owner of one tool ([ADR 0068](../architecture/decisions/0068-the-operator-may-name-the-owner-of-a-shared-accelerator-tool.md), 0.145.0).**
The rule became a live path when a Coral box added the RKNPU through `fleet_accelerators`: local
devices are walked first, so the Coral took every shared name. `accelerator_tool_owners` maps a
tool name to the device that serves it, and only that name moves:

```json
"accelerators": ["coral-edgetpu"],
"fleet_accelerators": ["rknpu"],
"accelerator_tool_owners": {"offload_object_detect": "rknpu"}
```

Here `offload_object_detect` forwards to the RK3588 node, and `offload_classify_image`,
`offload_image_embed` and `offload_semantic_segment` stay on the local Coral. An entry applies only
when its device is listed in `accelerators` or `fleet_accelerators` and has that tool; otherwise it
is logged at startup and ignored, and the first-listed rule decides. `mcpserver.accelOwnerPlan` is
the one decision the MCP registration and the status block read; the loop's lanes carry the same
claims (`AccelLane.Claims`). Each device's status entry lists `serves` (what it registered) beside
`owns` (what it could).

**Both surfaces, generalised (0.114.0):** the MCP server keeps one per-device table
(`internal/mcpserver/acceltools.go`) and one on-demand sidecar per device; the agent loop
receives every device as an `agent.AccelLane` (`pipeline.NewLoopAccel`, config order) and
registers the same tables through `ReadOnlyToolsWithLanes`. The pre-Coral single-lane `NPU`
injection still works for every existing caller and test.

### RKNPU tools

The RKNPU owns three capabilities, each a sidecar tool under the Coral's contract
(`POST /v1/<tool>`) on `.rknn` models the sidecar verifies by sha256 (an `.rknn` is compiled for one
target SoC, here rk3588):

| MCP / loop tool | Sidecar tool | Arguments | Result |
|---|---|---|---|
| `offload_classify_image` *(name shared with the Coral)* | `classify` | `image_path`, `top_k?` | `{results:[{label,score}],best,model,domain}`, ImageNet only |
| `offload_object_detect` *(shared with the Hailo and the Coral)* | `object_detect` | `image_path`, `score_threshold?` | `{objects:[{label,class_id,x,y,w,h,score}],count}` in image pixels |
| `offload_image_embed` *(shared with the Hailo and the Coral)* | `embed` | `image_path` | `{embedding,dim,space,model}` from a CLIP-class image tower |

The Coral's `domain` (iNat label spaces) and `size` (EfficientDet inputs) arguments are not offered:
they belong to its models. The descriptions do not name the RKNN models — the sidecar's manifest
decides which ship, and each result's `model` says what ran. `space` keeps embeddings honest: an
RKNPU vector is in its own space, neither the Hailo's `tinyclip` nor the Coral's
`efficientnet-edgetpu-s`, and vectors are comparable only within one space, whatever the dimension.

#### The models the RKNPU sidecar serves

Changed 2026-09-30 on operator order (no licence costs): **no AGPL or GPL model ships.** The
Ultralytics YOLOv8n (AGPL-3.0) and the Rockchip ResNet18 prebuilt (untraced provenance) are gone.
The manifest is [`accelerators/rknpu/models.json`](../../accelerators/rknpu/models.json); the
sidecar serves only a file whose sha256 it pins.

| Tool | Manifest key | Model | Licence | What the sidecar feeds it |
|---|---|---|---|---|
| `object_detect` | `ppyoloe_s` | PP-YOLOE+ s, INT8, 640 x 640, the 80 COCO classes | Apache-2.0 (PaddleDetection) | the picture letterboxed onto black, RGB uint8; `/255` is compiled in |
| `classify` | `resnet50tv2-i8` (`resnet50tv2-fp16` is the same network unquantised) | ResNet-50 with torchvision's `IMAGENET1K_V2` weights, packaged by timm as `resnet50.tv2_in1k`, 224 x 224 | BSD-3-Clause | the short side to 232 (bilinear), a centred 224 crop, RGB uint8; mean and std are compiled in |
| `embed` | `clip-vit-b32-image` | CLIP ViT-B/32 image tower, FP16 | MIT (OpenAI) | unchanged |

The zoo and toolkit files are Rockchip's (zoo code Apache-2.0). The classifier's key is one constant,
`CLASSIFY_MODEL` in `server.py`; the board measured both builds (below) and the INT8 one is served.

- **No model has a public `.rknn`.** `fetch-models.sh --convert` builds each on an x86_64 host from
  the recipe under its `convert` key: `ppyoloe_s` and CLIP from a sha256-pinned ONNX on the
  zoo's download host, the two ResNet-50 files from an ONNX **exported** from the timm weights
  (`export` recipe: weights downloaded and sha256-checked at a pinned Hugging Face revision, opset
  12; the exported ONNX has no hash, because an export is not reproducible across torch versions).
  A conversion is not bit-reproducible either (about a kilobyte of embedded build metadata differs
  between two runs), so each `sha256` pins the exact bytes that were built and checked, and a
  rebuild has to have its new hash recorded before the sidecar serves it.
- **The INT8 calibration images are not what a rebuild uses.** `ppyoloe_s` was calibrated on the 200
  COCO val2017 images of `accelerators/rknpu/calib/coco_val2017_calib_200.txt`; `resnet50tv2-i8`
  was calibrated on 200 ImageNetV2 images, while its recipe names the COCO list. Both are Flickr
  photographs under per-image terms, used only to compute quantisation scales and never
  redistributed. The list is fetched image by image from `images.cocodataset.org` over plain http
  (its https certificate does not match its name) and the images are not pinned by hash.
- **Detector decoding.** The head is anchor-free with a Distribution Focal Loss box: the sidecar
  reads the bin count from the box tensor (17 bins for PP-YOLOE, 16 for a YOLOv8-shaped head),
  takes the best class probability as the score (no box-confidence factor, exactly as the zoo's
  Python demo does), applies per-class NMS, and reads the three stride branches in any order. The
  score-sum branch is ignored: the zoo's Python demo replaces it with ones and its C demo uses it
  only to skip cells early. The defaults are the zoo demo's own: score threshold 0.25, NMS IoU 0.45.
- **Measured on the NPU (Orange Pi 5, RK3588S, 2026-09-30).** The pinned files ran on the board
  (librknnrt 2.3.2, rknpu 0.9.8, one NPU core) over the same evaluation lists as the host FP32
  baselines below, with a post-processing port that reproduces those baselines on the host to the
  fourth decimal:

  | Model | NPU result | vs host FP32 | NPU inference (median / p90) |
  |---|---|---|---|
  | PP-YOLOE+ s INT8 | mAP@[.5:.95] 0.4255, mAP@.5 0.5926 | −0.0105 mAP | 56.3 / 59.7 ms |
  | ResNet-50 (tv2) INT8 | top-1 69.7 %, top-5 89.1 % | −0.2 / −0.2 (not significant) | 13.2 / 13.5 ms |
  | ResNet-50 (tv2) FP16 | top-1 69.9 %, top-5 89.2 % | 0.0 / −0.1 | 29.7 / 30.9 ms |

  The INT8 classifier is served (`CLASSIFY_MODEL`): it matches FP16 accuracy at twice the speed.
  The PP-YOLOE outlier weight the build log flagged (`conv2d_97.w_0` = 23.8) costs no more than the
  1-point drop above. The same lists through the Coral Edge TPU (EfficientDet-Lite, 25 boxes per
  image): Lite0 0.2700, Lite1 0.3128, Lite2 0.3561 mAP@[.5:.95]; the NPU's detector capped to its
  top 25 boxes per image still scores 0.4099. The simulator's PP-YOLOE output on the zoo's `bus.jpg`
  (person 0.950 / 0.935 / 0.923, bus 0.893, person 0.473, handbag 0.411) is what the decoder tests
  replay (`RKNPU_SIM_DIR`, see `test_server.py`). Evidence: the operator's benchmark records (kept
  outside this repository).

The reference the board's INT8 numbers must be checked against is the host FP32 run of the same
ONNX files (onnxruntime on CPU, 2026-09-30), on evaluation lists that are disjoint from the
calibration lists (seed 20260930):

| Model | Evaluation list | Host FP32 result | The upstream figure (another dataset and pipeline) |
|---|---|---|---|
| PP-YOLOE+ s | 500 COCO val2017 images | mAP@[.5:.95] 0.4360, mAP@.5 0.6043, mAP@.75 0.4707 (zoo post-process at score 0.001 and NMS 0.65, the mAP thresholds, not the demo's) | 43.7 mAP, COCO val, Paddle FP32 |
| ResNet-50 (tv2) | 1000 ImageNetV2 matched-frequency images, one per class | top-1 69.9 %, top-5 89.3 % (FP16 build: the same ONNX, the same baseline) | 80.858 / 95.434, ImageNet-val |

Read each as a same-list baseline, not a reproduction of the upstream number: ImageNetV2 is about
ten points harder than ImageNet-val, so 69.9 % against 80.9 % is expected. The simulator's INT8
tensors sit at cosine 0.963-0.991 to FP32 for PP-YOLOE's nine outputs and 0.988 for ResNet-50 INT8
(1.000 for the FP16 build).

Deliberately **not** owned: `text_embed` and `zero_shot` (only the image side is served), the tools
the Hailo covers (`face_*`, `person_embed`, `pose`, `segment`, `depth`, `enhance_low_light`) and the
Coral's `semantic_segment`. `offload_ocr` and `offload_transcribe` `engine:"npu"` stay Hailo-only.

**The shared-name rule over three devices.** With a third device sharing names, the first listed
owner is not always the first device listed: `classify_image` is owned by the Coral and the RKNPU
only, so on `hailo-8l,rknpu,coral-edgetpu` the RKNPU takes it while the Hailo takes `object_detect`
and `image_embed`. Both surfaces walk `config.Accelerators` in order, and every order of the three
devices is tested on both.

**Kept in lockstep.** The device id is spelled by hand in the MCP table (`accelMCPTools`,
`accelOwns`, `accelLaneConfigFor`), in the agent loop's (`laneToolsFor`) and in the pipeline's lane
table (`laneConfigFor`), with no registry — a missed site registers nothing, and only the status
block says so. A test holds every device `profiles.json` declares to all three: its owned capabilities are the MCP
tools, the loop's tools and a pipeline lane.

## Fleet routing — reaching a device this box lacks (0.115.0, ADR 0038)

A box that carries no accelerator can still use one over the fleet. Opt in with
`fleet_accelerators: ["coral-edgetpu"]` (beside `delegate_remotes`): that device's tool table
registers locally — MCP surface and agent loop alike, so the parity test still holds — with
`[FLEET: …]` appended to every description, and each call forwards through
[`internal/accelremote`](../../internal/accelremote/accelremote.go):

1. **`image_path` is read on THIS box** and its bytes travel inside the job (cap 8 MiB); a
   caller-side `out_path` is dropped. There is no shared filesystem and no mount.
2. **Placement is a live probe**: `delegate_remotes` in order, first node whose
   `/fleet/health` lists the id (2 s per probe; every miss is named in the defer).
3. **The node runs a fleet task `accel`** — `{accelerator, tool, args, image_b64, image_name}`
   — on its **local** lane only (a forwarded call never forwards again), in a job-scoped dir
   that lives as long as the job; a mask the tool writes there returns as `mask_b64`. `accel`
   is exempt from `fleet_max_concurrent_jobs` (it never touches the text endpoint).
4. **The result is the tool's dict plus `placement{node, base, accelerator, job_id, wall_ms,
   remote:true}`.** Transport, placement and dispatch failures are device-prefixed defers
   (`coral-edgetpu (fleet): …`); the node's own defers and the sidecar's refusals pass through.

Ownership across the fleet is ADR 0037 extended: `accelerators` is walked before
`fleet_accelerators` on both surfaces, so a local device wins a shared name unless an
`accelerator_tool_owners` entry names the fleet device for it (ADR 0068, below), and a box
that lists a device in both registers the local lane only. A box that lists nothing in
`fleet_accelerators` is byte-identical to 0.114.x (pinned by
`TestFleetAcceleratorRegistersForwardedToolsOnly`).

**The Hailo-8L never travels** (register E-08, [ADR 0038](../architecture/decisions/0038-accelerator-work-travels-to-the-box-that-has-the-device.md)
amendment): the standalone box that carries it keeps it local. `/fleet/health` omits it from
`accelerators`, `accel` is advertised only for a device the fleet may use, an `accel` job naming it is
refused, listing it in `fleet_accelerators` is a `doctor` finding, and `fleet-ui` refuses a `--listen` port
equal to `hailo_endpoint`'s on a box that lists it. The Coral and the RKNPU are unaffected; the one list is
`config.LocalOnlyAccelerator`.

`offload_status.accelerators` lists a fleet device with `fleet: true` and no health probe (the
probe is per call, at the node). The node's `supported_task_types` gains `accel` exactly when it
lists a device the fleet may use; `NodeView.Accelerators` decodes the same field for the delegator.

Measured 2026-09-08 from the Qube (no device) against the Lenovo (`coral-edgetpu`): see the
gate lines in the CHANGELOG entry for 0.115.0.

## Status

`offload_status` gains an `accelerators` block — present only when the box lists one. For
hailo-8l: the endpoint, whether a sidecar command is configured, the owned tool list, and a
**live health probe that never spawns the sidecar** (status stays side-effect free). A
`health_error` between uses is normal — the sidecar self-exited.

For coral-edgetpu the same block carries the endpoint, whether a launcher is configured, the
four owned tools, and the sidecar's `/health` — `{enabled, device:"/dev/apex_0", status,
temp_c, loaded:[...], models_missing:[...], runtime:{litert, libedgetpu}}` — read from sysfs
`status`/`temp` (never written).

For rknpu the block carries the endpoint, whether a launcher is configured, the three owned tools
and the sidecar's `/health` dict, embedded verbatim.

## Limits

- **Single in-flight inference** — one sidecar process serialises NPU access by construction.
- **NPU calls are in the savings ledger since 0.130.3** (register E-04): one row per accelerator MCP call — task = the sidecar tool, `model_tier` = the device id, `<node>:<device>` for a forwarded call (node = the host of the answering node's base URL, the name PAIR knows the member by), `<device>@fleet` for a forward that never reached a node — with latency and the defer verdict; an empty required argument writes no row. Until 0.130.5 a successful forward was also recorded as `<device>@fleet`: `accelremote` stamps a typed `Placement` and the ledger helper only read a JSON map. The PAIR card built from the row carries the device as its engine and runs on that node (`docs/systems/pair-workloads.md`).
- **Forwarded calls ship the whole image** (cap 8 MiB, base64 in the job); a fan-out of
  accelerator calls across several nodes is not a thing yet — one call, one node.
- Windows cannot see the device as an "NPU" (no MCDM driver) — irrelevant to this route, which
  reaches the device through HailoRT via the sidecar, not through Windows ML.
- **RKNPU is Linux-only and shares the host's memory.** A loaded model holds system RAM until the
  sidecar's idle exit (`rknpu_idle_sec`): shorten it on a box whose memory is also a host's, at the
  cost of a cold start per burst. The user the fleet node runs as needs the NPU's DRM node (on the
  reference board `/dev/dri/renderD129` is `root:render` and `card2` `root:video`). One in-flight
  inference per sidecar, as for every device; a forwarded call is cut off at the 150 s Budget,
  45 s of which a cold start can spend.

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

To verify an RKNPU (a Rockchip RK3588 board, driver bound):

1. `uname -r`, then the driver: on the vendor kernel one of `/sys/class/drm/card*/device/uevent`
   holds `DRIVER=RKNPU`; on a mainline kernel `cat /sys/bus/platform/devices/fdab0000.npu/uevent`
   does, and `/sys/module/rknpu/version` reads the DKMS module's version (0.9.8 measured).
   `/sys/kernel/debug/rknpu/version` names the driver on both (debugfs: usually root only).
2. `local-offload install detect -json` → `"accelerators": ["rknpu"]`. On a board whose driver is
   `rocket` it is empty: that is the rule, not a fault.
3. `offload_status` → `accelerators.rknpu.endpoint` present; a first-call `health_error` (not
   running) is expected.
4. `offload_object_detect` or `offload_classify_image` on a real photo → a result carrying `model`;
   right after, `curl http://127.0.0.1:18815/health` shows it loaded.
5. Wait `rknpu_idle_sec` + 10 s → the sidecar process is gone and its RAM back; the next call spawns
   it again.

## Source map

- [`internal/accelclient/hailoclient.go`](../../internal/accelclient/accelclient.go) — the
  loopback client
- [`internal/accelclient/sidecar.go`](../../internal/accelclient/sidecar.go) — `Ensure`,
  `SpawnCmd`, the detached window-hidden spawn
- [`internal/mcpserver/mcpserver.go`](../../internal/mcpserver/mcpserver.go) — gated tool
  registration, `handleHailoTool`, the `offload_ocr` engine switch, the status block
- [`internal/config/config.go`](../../internal/config/config.go) — `Accelerators`,
  `HasAccelerator`, the `hailo_*` / `coral_*` / `rknpu_*` keys and defaults
- [`internal/config/localonly.go`](../../internal/config/localonly.go) — the local-only device
  list and the one filter behind the three fleet-visible points (register E-08)
- [`internal/hwdetect/classify.go`](../../internal/hwdetect/classify.go) — detection
- [`internal/mcpserver/acceltools.go`](../../internal/mcpserver/acceltools.go),
  [`internal/agent/acceltools.go`](../../internal/agent/acceltools.go),
  [`internal/pipeline/loopaccel.go`](../../internal/pipeline/loopaccel.go) — the per-device tables
  (tools, owned capabilities, lane config) that must stay in lockstep
- [`internal/pairworkloads/pairworkloads.go`](../../internal/pairworkloads/pairworkloads.go) —
  `EngineFor`: the device is the PAIR card's engine
- [`internal/tierseed/tierseed.go`](../../internal/tierseed/tierseed.go) —
  `ResolveAccelerators`
- [`internal/accelremote/accelremote.go`](../../internal/accelremote/accelremote.go) — the
  forwarder (image bytes, node pick, dispatch + poll, placement)
- [`internal/fleetnode/accel_task.go`](../../internal/fleetnode/accel_task.go),
  [`internal/pipeline/acceltask.go`](../../internal/pipeline/acceltask.go) — the `accel`
  fleet task (payload → job dir → local lane → `mask_b64`)
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
- [ADR 0038](../architecture/decisions/0038-accelerator-work-travels-to-the-box-that-has-the-device.md) —
  fleet routing: the work travels to the device, bytes included
- [mcp-server.md](mcp-server.md) — the tool surface this extends
- [setup-installer.md](setup-installer.md) — detection and seeding in the install flow
- [fleet-node.md](fleet-node.md) — the health payload that advertises the list
