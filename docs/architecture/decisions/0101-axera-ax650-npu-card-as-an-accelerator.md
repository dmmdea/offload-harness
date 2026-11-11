---
status: Proposed
date: "2026-11-10"
---

# An AX650-class NPU card is an accelerator with its own memory, and it ships useful or not at all

## Context

The inference server carries a fourth kind of silicon: an AX650-class NPU on an M.2 card (the AX8850 card), with 4 GB
of memory of its own. 945 MiB of that is the card's OS and 2944 MiB is the CMM, the contiguous pool every model loads
into. The host reaches it over PCIe Gen2 x2 (about 1 GB/s, the chip's own maximum) through the vendor's AXCL runtime.
A model on the card is never in host RAM or in VRAM, so the card can work while the GPUs serve LLMs and takes nothing
from them. That is the whole value: free, extra, concurrent compute.

[ADR 0024](0024-accelerators-are-additive-to-the-gpu-tier.md) gives an accelerator a place (additive to the tier), and
[ADR 0037](0037-a-capability-name-has-one-owner-per-box.md), [0038](0038-accelerator-work-travels-to-the-box-that-has-the-device.md)
and [0068](0068-the-operator-may-name-the-owner-of-a-shared-accelerator-tool.md) give it ownership and reach. What a
new device still has to earn is use. A card that only answers "hello" gets uninstalled; one that does detection,
segmentation, depth, pose, OCR and a text-searchable image index in tens of milliseconds, in parallel with the GPUs,
gets used. The forces:

- **The roster must be licence-clean.** The operator's rule (2026-09-30, recorded for the RKNPU in
  [accelerators.md](../../systems/accelerators.md#the-models-the-rknpu-sidecar-serves)) is that no AGPL or GPL model
  ships, which rules out the YOLO family.
- **Models come from the publisher, compiled.** There is no conversion toolchain on the box, so only publisher-compiled
  AX650 models count, and they are fetched and verified under [ADR 0099](0099-frontier-updates-floors-never-pins.md).
- **The link is narrow and the card is small.** Anything that moves state across PCIe every step loses to the link,
  and the CMM is a shared pool a later NPU LLM seat also needs.
- **Observability is part of the device.** A card that throttles, loses link width or fills its CMM without anyone
  seeing it is a silent fault. The vendor CLI is the obvious reader and is the wrong one here (below).

## Decision

### The device

`axera-ax650` is a fourth accelerator id, kind `npu`, **fleet-visible** (not on the local-only list): detected on
Linux when a line of `/proc/bus/pci/devices` has `1f4b0650` as its second field and the driver `ax_pcie_dev_host` as
its last, listed after the Hailo, Coral and RKNPU in detection order, served by an on-demand loopback sidecar on
`127.0.0.1:18816` that exits itself when idle (`axera_idle_sec`, clamped 1..300), and wired through the same lockstep
tables, `profiles.json` seed, `install seed --axera-home` / `AXERA_HOME`, status block and fleet `accel` task as the
other devices. The sidecar is `accelerators/axera/`, Python on pyaxengine (the publisher's binding of the AXCL
runtime). Detail: [accelerators.md](../../systems/accelerators.md).

### The roster: ten tools, one model each, and why

| Tool | Model | Why this one |
|---|---|---|
| `object_detect` | RF-DETR small (Apache-2.0) | the 80 COCO classes at 46-52 ms warm; the YOLO family is AGPL; its person and instance boxes also feed `pose` and `segment` |
| `classify`, `embed`, `text_embed`, `zero_shot` | SigLIP2 B/16-224, both towers (Apache-2.0) | one checkpoint gives one 768-d space for images and text, so search and free-text labels work with no cloud; classification is zero-shot over the 1000 ImageNet names |
| `semantic_segment` | DeepLabV3+ MobileNetV2, Pascal VOC (MIT) | a per-pixel class map in 61-64 ms |
| `segment` | RF-DETR boxes refined by MobileSAM (Apache-2.0) | instance masks at 220-250 ms for five instances, and `everything` for class-agnostic masks; EdgeTAM was slower for the same job |
| `depth` | Depth Anything 3 small (Apache-2.0) | 27.0 ms raw against Depth Anything V2 small's 37.3 ms, so V2 is superseded |
| `pose` | RF-DETR person boxes, then RTMPose-M (Apache-2.0) | COCO-17 keypoints in 52-60 ms for one person |
| `ocr` (through `offload_ocr` `engine:"npu"`) | PP-OCRv5 mobile detection, classifier, recognition (Apache-2.0) | a page in 0.2-0.3 s off the GPU; the GPU OCR stays primary for stylised and small screen text |

Nine capabilities register as `offload_*` tools on both surfaces; OCR is reached only through the existing
`engine:"npu"` switch, which now goes Hailo first, then this box's Axera card, then a fleet node's
(`fleet_accelerators`), else a defer. The OCR path departs from the publisher's demo in three measured ways
(letterboxed detection, half-overlapping 320 px recognition windows stitched at a CTC blank, mid-grey padding), each
fixing a failure seen on real lines.

### The rulings

- **Ownership is explicit where the card is stronger.** Axera is listed last, so an earlier device keeps a shared name
  by ADR 0037; the operator hands the card `object_detect`, `classify_image`, `image_embed` and `semantic_segment`
  through `accelerator_tool_owners` (ADR 0068) on a box that also has a Coral or an RKNPU. Moving `image_embed`
  changes the embedding space, so stored vectors are re-embedded.
- **Not owned.** `face_detect` and `face_embed` (the InsightFace weights are non-commercial), `person_embed` (no
  AX650-compiled model), `enhance_low_light` (no licence-clean fast model).
- **Speech is not served through the Python binding.** Whisper measured 270 ms per decoded token, because the KV cache
  re-crosses PCIe every step. A C++ path that keeps the KV on the card is a later slice.
- **An NPU LLM or VLM seat is a later slice.** The vendor's OpenAI-compatible server measured 17.7 tok/s decode for
  Qwen3.5 0.8B Int4 in 1185 MiB of CMM and 11.0 tok/s for 2B Int4 in 2260 MiB. The sidecar's LRU budget
  (`AXERA_CMM_BUDGET_MIB`, default 1536, half the pool) leaves that share free.
- **One in-flight call, resident sessions under a budget.** One lock; sessions stay loaded under the CMM budget and a
  load the card refuses evicts the rest and retries once. The sidecar unloads explicitly with each session's own AXCL
  context bound, because the library's own unload fails from any thread but the creator's and keeps every session in
  a module-level list.
- **Verified artifacts.** `fetch-models.sh` resolves at run time and checks every file against the publisher's
  digest; the sidecar re-hashes a model against the recorded digest before it loads it (ADR 0099).
- **Telemetry is part of the device, read through the runtime library in a child process.** The card's temperature,
  loads, CMM, clocks, thermal trips, PCIe link and AER counters, per-process CMM and the host's fans are read with
  `ctypes` against the AXCL runtime, by `axcl_telemetry.py`, which the sidecar runs as a child so a read never
  initialises or finalises the runtime inside the process that holds live sessions. They surface in the sidecar's
  `/health`, `axera-http.sh --telemetry` (which works while the sidecar is down), `offload_status`, `/fleet/health`
  `accelerator_telemetry`, and the fleet UI and `top`.
- **Tuning is to full performance and nothing risky.** The card's CPU ships at 1.2 GHz of a 1.2/1.4/1.5 GHz table and
  is pinned to its top step by `--apply-perf`, applied at every sidecar spawn and by `axera-perf.service` at boot. The
  NPU (800 MHz) and NN-bus (1.6 GHz) clocks already ship at their maxima and are reported, never written; the thermal
  trips (80, 105, 120 C) are left alone because the card idles at 34 C; the card does not support ASPM, so there is no
  link power lever.

## Consequences

- A box with the card gains ten tools that run beside the GPUs and cost them nothing: detection at about 50 ms,
  segmentation and pose at 60-250 ms, depth at 40-50 ms, an image index with free-text queries at 16 ms per image and
  about 5 ms per new query, a page of OCR in 0.2-0.3 s.
- The card's memory is the budget that matters: about 796 MiB for the whole vision set resident, 466 MiB more for the
  text tower, a 1536 MiB default cap. A cold `text_embed` pays a 6.5 s load and the first-ever `classify` 13.7 s
  (it builds and caches the class embeddings); idle exit returns the card to its 18 MiB baseline.
- The CPU pin is cheap and small: an interleaved A-B run (400 inferences per model per clock, p50) gained 3.8 % on
  RTMPose-M, 2.6 % on the MobileSAM decoder and 1.9 % on the SigLIP2 image tower, and RF-DETR's -1.2 % is noise.
  Latency is bound by the NPU (already at its maximum) and the Gen2 x2 link, so the pin is worth at most about 4 %
  on small models and is kept only because it is free and idempotent.
- Under sustained load (RF-DETR back to back for 120 s) the card ran 23.5 inferences per second at 88-89 % NPU load,
  held full clocks and rose 1.2 C to 36.4 C, 44 C below the first passive trip: no throttling.
- The card is observable without the vendor CLI, beside other pollers on the host and beside inference, at one read
  per 0.57 s, cached off the request path.
- Image bytes cross the fleet for a forwarded call, and the `accel` task now returns every `<stem>_path` file the tool
  wrote as `<stem>_b64` (a depth map as well as a mask).
- Four devices share the shared-name table and the lockstep sites; a missed site registers nothing, and only the status
  block says so (the existing test holds each device `profiles.json` declares to all three).
- Small screen text reads with glyph errors on the card (15 px text gave "count=l" for "count=1"), so OCR stays
  caller-selected, as ADR 0024 already requires.
- The card is a Linux, AXCL-package, Python 3.14 install; Windows has no probe and never matches.

## Alternatives considered

- **The vendor CLI for telemetry.** Rejected: it is single-instance (a shared-memory guard), other pollers on the
  host use it, and a second reader is refused. It also prints `--` for the power and fan fields the card does not have.
  The runtime's own calls take no such lock.
- **Whisper through the Python binding.** Measured at 270 ms per decoded token and rejected; it stays a later slice
  as a C++ path with the KV on the card.
- **YOLO-family detectors** (and YOLO segmentation and pose heads). Rejected: AGPL weights.
- **Depth Anything V2 small.** Measured at 37.3 ms against Depth Anything 3 small's 27.0 ms and superseded by it.
- **EdgeTAM for segmentation.** Measured at 71.9 ms for the encoder and 44.5 ms for the decoder against MobileSAM's
  63.6 and 20.6, so slower for the same job.
- **Importing the telemetry reader into the sidecar.** Rejected: it would initialise and finalise the runtime inside
  the process that holds live sessions.
- **Leaving the card local-only like the Hailo-8L.** Rejected: nothing about it needs a single standalone box, and a
  delegator with no card gains detection, segmentation and OCR by forwarding.

## Related code

- `accelerators/axera/`: `server.py`, `axcl_telemetry.py`, `axera-http.sh`, `axera-perf.sh`, `axera-perf.service`,
  `fetch_models.py`, `fetch-models.sh`, `models.json`.
- `internal/hwdetect/classify.go` (`DetectAxera`), `internal/config/config.go` (`axera_*`),
  `setup/templates/profiles.json` (`axera-ax650`), `internal/tierseed/tierseed.go`.
- `internal/mcpserver/acceltools.go`, `internal/agent/acceltools.go`, `internal/pipeline/loopaccel.go`: the lockstep
  tables; `internal/mcpserver/mcpserver_media.go`: the `offload_ocr` branch order.
- `internal/pipeline/acceltask.go`: `<stem>_b64` file shipping for a forwarded call.
- `internal/accelclient/telemetry.go`, `internal/fleetnode/accel_telemetry.go`, `internal/fleetview/acceltelemetry.go`:
  the telemetry path from card to fleet UI.

## Related docs

- [accelerators.md](../../systems/accelerators.md), [FLEET-NODE.md](../../FLEET-NODE.md#accelerator-card-telemetry-accelerator_telemetry),
  [OPERATOR-GUIDE.md](../../OPERATOR-GUIDE.md#the-axera-card-axera-ax650), [SETUP-AGENT.md](../../../setup/SETUP-AGENT.md).
- [0024](0024-accelerators-are-additive-to-the-gpu-tier.md), [0037](0037-a-capability-name-has-one-owner-per-box.md),
  [0038](0038-accelerator-work-travels-to-the-box-that-has-the-device.md), [0062](0062-rk3588-soc-tier-serves-from-the-npu-on-a-unified-memory-budget.md),
  [0068](0068-the-operator-may-name-the-owner-of-a-shared-accelerator-tool.md), [0099](0099-frontier-updates-floors-never-pins.md).
