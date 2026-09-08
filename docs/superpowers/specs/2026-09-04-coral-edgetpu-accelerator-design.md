# Coral Edge TPU as a harness accelerator — design

Date: 2026-09-04 · Status: DRAFT, awaiting operator review · Path: architectural
(second accelerator device; touches config, detection, seeding, MCP + loop tool surfaces,
fleet health, docs, tier matrix)

## Why

The Lenovo M720q now carries a Coral Edge TPU (M.2 A+E, PCI `03:00.0`) that is driven,
tuned and measured (record: `Ecosystem/Lenovo M720q/2026-09-04_coral-tpu/SETUP.md`):
MobileNet v2 iNat at **p50 3.16 ms / ~300 inf/s**, 70–71 °C sustained on `libedgetpu1-std`,
no throttle. Nothing in the harness knows it exists. The harness already has exactly one
pattern for "a device beside the GPU": the Hailo-8L accelerator lane (ADR 0024,
`docs/systems/accelerators.md`). This spec applies that pattern to the Coral so that

1. the device's live configuration (feranick gasket-dkms 1.0-18.4, libedgetpu1-std
   16.0TF2.19.1, ai-edge-litert 2.2.0, `temp_poll_interval=1000`, trip points untouched) is
   **preserved verbatim** — the tier reads it, never rewrites it;
2. agent contracts that land on the Lenovo, and Claude sessions on the Lenovo, get free
   on-box tools for the work the TPU does natively (classification, detection, semantic
   segmentation, image embeddings);
3. the fleet advertises the device so a delegator can route fitting work to it
   (phase B below makes that routing real from the Qube).

## What already exists and is reused unchanged

| Piece | Where | Reused how |
|---|---|---|
| Additive `accelerators: []` list, `HasAccelerator` gate, `omitempty` byte-identity pin | `internal/config`, `internal/hwdetect`, `internal/fleetnode` | new id appended to the same list; no schema change |
| `profiles.json` `accelerators` map + `tierseed.ResolveAccelerators` | `setup/templates/profiles.json`, `internal/tierseed` | one new entry; one new home token |
| Loopback sidecar client + on-demand `Sidecar.Ensure` (spawn once, poll `/health`, self-exit idle) | `internal/hailoclient` | generalised (see D2) and used for both devices |
| Gated MCP registration + `handleHailoTool` adapter + status block | `internal/mcpserver` | table becomes per-accelerator |
| Loop-side `agent.NPUFunc` injection + per-process sidecar singleton | `internal/agent/nputools.go`, `internal/pipeline/loopnpu.go` | same injection, second table |
| Health advertisement of `accelerators` | `internal/fleetnode/server.go` | unchanged wire; source gains a config fallback (D6) |

## Decisions (D1–D9)

### D1 — Identity: `coral-edgetpu`, kind `tpu`, port 18814

Accelerator id `coral-edgetpu` (one id for the whole Edge TPU family: USB/M.2/Dual all
run the same `_edgetpu.tflite` artifacts, unlike Hailo-8 vs 8L). Sidecar base
`http://127.0.0.1:18814` — loopback only, distinct from Hailo's 18813 so a box with both
devices never collides. 18814 is free on the Lenovo (live `ss` 2026-09-04) and inside its
safe-pick range 18792–18999; the port file gains the row in the same change.

### D2 — One generic sidecar lane, not a second copy of `hailoclient`

`internal/hailoclient` contains nothing Hailo-specific except its name and error strings.
It is renamed to `internal/accelclient` with a `Device string` label carried on the client
(`"hailo-8l"`, `"coral-edgetpu"`) so every error/defer reads `<device>: ...` as today.
`hailoclient` callers are updated in the same PR; the package tests move with it. No
behaviour change for the Hailo path (the existing `hailo_test.go` assertions must pass
unmodified except the import path).

*Rejected:* a copied `coralclient` (two 160-line packages diverging from day one);
a config-level `accelerator_lanes` map replacing the `hailo_*` keys (a migration of a
deployed box for no user-visible gain).

### D3 — Config keys mirror the Hailo set

| Key | Seed value | Meaning |
|---|---|---|
| `accelerators` | `["coral-edgetpu"]` | THE gate |
| `coral_endpoint` | `http://127.0.0.1:18814` | sidecar base, loopback only |
| `coral_sidecar_cmd` | `__CORAL_HOME__/coral-http.sh` | launcher; empty = never spawn, defer when down |
| `coral_timeout_sec` | `30` | one call's bound (cold model load on the TPU is ~0.5–2 s; the 60 s Hailo figure is HEF-load driven and not needed) |
| `coral_idle_sec` | `300` | sidecar self-exit window, passed on spawn |

Defaults in `config.Default()` are inert while `accelerators` is empty, exactly like the
`hailo_*` defaults. `__CORAL_HOME__` expands from `CORAL_HOME`, default
`<OFFLOAD_HOME>/coral`, never empty (same guard as `HAILO_HOME`). `tierseed.Options` gains
`CoralHome`.

### D4 — Detection is a sysfs read, Linux only

`hwdetect.DetectCoral(read func(path string) (string, error)) []string` returns
`["coral-edgetpu"]` iff `/sys/class/apex/apex_0/status` reads `ALIVE` (trimmed). Any read
error is "no accelerator", never a failure. `DetectAccelerators` becomes the union of the
Hailo probe and the Coral probe, in that order (order matters for D5). Windows has no apex
driver and never matches; `OFFLOAD_ACCELERATORS` keeps overriding both probes.

`install.sh` today merges no accelerator seed at all (verified: zero accelerator references)
— `install plan` in Go does. The Linux installer gains the same step the Windows one has:
read `verdict.accelerators` from `install detect --json`, merge
`install seed --accelerators <ids>` (a new flag on the existing verb, backed by
`tierseed.ResolveAccelerators`) over the tier seed, and write `accelerators` into
`installed.json`. This closes a real gap for the Hailo path too, so it is in scope as the
same defect on the Linux surface.

### D5 — Tool surface: capability-named tools, one owner per name per box

The Coral owns four capabilities in v1, each grounded on an artifact that exists in the
Coral zoo (`google-coral/test_data`, Apache-2.0) and, for classification, already measured
on this box:

| MCP / loop tool | Sidecar tool | Model (default; options) | Result |
|---|---|---|---|
| `offload_classify_image` **(new capability)** | `classify` | `domain=imagenet` → EfficientNet-EdgeTPU-S 224 (1000 ImageNet); `birds`/`insects`/`plants` → MobileNet v2 1.0/224 iNat | `{results:[{label,score}],best,model,domain}` top-k (default 5) |
| `offload_object_detect` *(name shared with Hailo)* | `object_detect` | EfficientDet-Lite0 320 COCO; `size=lite1\|lite2` | `{objects:[{label,class_id,x,y,w,h,score}],count}` — same shape as Hailo's |
| `offload_semantic_segment` **(new capability)** | `semantic_segment` | DeepLabV3 MobileNet v2 Pascal (21 classes) | per-pixel class-id PNG at `out_path`; `{mask_path,classes:[{class_id,label,pixels}],width,height}` |
| `offload_image_embed` *(name shared with Hailo)* | `embed` | EfficientNet-EdgeTPU-S embedding extractor | `{embedding,dim:1280,space:"efficientnet-edgetpu-s"}` |

Deliberately **not** owned by the Coral: `text_embed`, `zero_shot` (no text tower exists
for the Edge TPU — the EfficientNet space is not CLIP, and the tool description says so),
`face_*`, `pose`, `person_embed`, `depth`, `enhance_low_light`, `segment` (instance
segmentation; the Coral's DeepLab is semantic, hence the distinct name — the two outputs
are not interchangeable). Face detection (`ssd_mobilenet_v2_face`) and PoseNet are listed
as follow-ups once their artifacts are verified on the box; this spec does not promise them.

**Shared-name rule.** `offload_object_detect` and `offload_image_embed` are capability
names, and a capability has exactly one owner per box: registration walks
`cfg.Accelerators` in order and the **first listed accelerator that owns a name registers
it**; a later one is skipped for that name (logged once at startup). Today no box lists
both, so the rule is a tested invariant, not a live path. The tool description names the
device that serves it, and `offload_image_embed` reports `space` so a caller never mixes a
1280-d EfficientNet vector with a 512-d TinyCLIP one.

The `engine:"npu"` switches on `offload_ocr` / `offload_transcribe` stay Hailo-only (the
Coral has no OCR or ASR path); they keep deferring with "no hailo-8l" on the Lenovo.

### D6 — Health advertises the device from config when there is no manifest

`fleet-serve` today reads `accelerators` only from `installed.json`. The Lenovo has **no**
`installed.json` (hand-built node; verified) so its health would never list the device.
Rule: `Options.Accelerators` = manifest list when the manifest exists and lists any,
else `cfg.Accelerators`. The Windows/manifest path is unchanged; a test pins both sources.

### D7 — The sidecar lives in the harness repo

`accelerators/coral/` in `local-offload-public`: `server.py` (stdlib `http.server`, the
Hailo wire contract verbatim: `GET /health`, `POST /v1/<tool>`, 404 `unknown_tool`, 400
`bad_request`, structured 200 error dicts, 500 `internal` guard, idle self-exit, refuses a
non-loopback bind), `coral-http.sh` (pins `$CORAL_HOME/venv/bin/python`, `CORAL_MODELS_DIR`
default `$CORAL_HOME/models`), `models.json` (the artifact manifest: file name, sha256,
labels file, input size, task — the sidecar refuses to serve an unlisted or mismatched
file), `fetch-models.sh` (downloads from the `google-coral/test_data` raw URLs and verifies
the sha256), `test_server.py` (contract tests with `CORAL_ENABLED=0`, runnable anywhere).

The Hailo sidecar lives in its own repo because that repo pre-dated the tier and holds the
HEF pipelines. The Coral has no such repo, the runtime is ~400 lines of stdlib + litert
with no personal paths, and shipping it with the harness means one binary drop + one
directory to deploy. ADR 0024's "sidecar implementation lives elsewhere" wording is
amended to "lives with its device: the Hailo repo for Hailo, `accelerators/<id>/` here for
devices with no repo of their own".

Runtime facts the sidecar encodes: `load_delegate("libedgetpu.so.1")` via `ai_edge_litert`;
one `Interpreter` per model kept resident after first use (the TPU holds one model's
weights at a time — a switch re-uploads, ~10–60 ms for these sizes); one lock, one
in-flight inference; `/health` returns `{enabled, device:"/dev/apex_0", status(sysfs),
temp_c(sysfs), loaded:[...], models_missing:[...], runtime:{litert, libedgetpu}}`.
The sidecar **never writes sysfs** — the thermal knobs stay the operator's (`SETUP.md`
§6.4); it only reads `temp` and `status`.

### D8 — Deployment on the Lenovo respects the unit's sandbox

`offload-fleet-node.service` runs as the fleet service user with `ProtectHome=yes` (verified). The
sidecar it spawns cannot see `~/coral-venv`. So `CORAL_HOME =
<OFFLOAD_HOME>/coral` with its **own** venv built from the same
staged wheels (`~/coral-stage/wheels314`, offline) and its own `models/`; `~/coral-venv`
stays untouched as the operator's bench. The unit needs no change: no `PrivateDevices`, so
`/dev/apex_0` is visible; the service user is in group `apex`; `ReadWritePaths` already covers the
root. Nothing else on the box changes — no udev, modprobe, sysfs, kernel or NVIDIA state
is touched (the SETUP.md rollback stays valid).

The binary follows the established path: cross-compiled on the Qube, scp'd over the drop
at `src/offload-harness/local-offload` (memory: the Lenovo's src clone is stale and must
not be built there). Config: `accelerators`, `coral_*` keys added to
`etc/config.json` (backup first, as every prior edit there). Deploy sequence and its
verification are the last task of the implementation plan.

### D9 — Versioning, docs, matrix

- Version **0.114.0** (new tool surface). Bump ritual as recorded (VERSION, `main.go`
  const, `.printing-press.json`, CHANGELOG, one commit; `go test -count=1 ./...` after,
  unpiped).
- **Tier matrix first** (canonical rule): the Accelerators sheet gains the
  `coral-edgetpu` row before any Go code, from `profiles.json` via the existing generator.
- `docs/systems/accelerators.md` gains the Coral sections (detection, seed, tools,
  shared-name rule, deploy notes); ADR 0024 gets the D7 amendment paragraph; a new short
  ADR records D5's shared-name rule (it constrains every future accelerator).
  `AGENTS.md`, `README.md`, `setup/SETUP-AGENT.md`, `docs/README.md` index lines.
- Ecosystem records outside the repo, same change: `P:\Port Directory\lenovo-ports.md`
  (18814 row), the Lenovo wiki entity (Coral section: "served through the harness"),
  `SETUP.md` gets a pointer paragraph, not a rewrite.

## Phase B — proactive routing from a box that lacks the device

Phase A makes the Coral usable **on** the Lenovo (Claude sessions there; every
`agent_delegate`/`agent_run` contract that lands there advertises the four tools).
"Proactively route fitting work to it" from the Qube needs one more hop, designed here so
Phase A leaves the right seams, planned and shipped as its own PR after A is verified:

1. **Fleet task type `accel`** on the node: payload `{accelerator, tool, args}` where an
   image is carried as `image_b64` (cap 8 MiB; the node writes it to its media dir, calls
   its local lane, and returns the result dict; a produced PNG comes back as `mask_b64`
   plus a `/fleet/media/...` path). Not concurrency-capped (it never touches the text
   endpoint) — added to the explicit uncapped list in `concurrencyCapped`.
2. **`NodeView.Accelerators`** decoded from health (tolerant, absent = none).
3. **Delegator-side registration opt-in**: config `fleet_accelerators: ["coral-edgetpu"]`
   on the Qube registers that accelerator's tools locally, each forwarding to the first
   reachable fleet node whose health lists the id (health probe per call, 2 s; quarantine
   reuse). Explicit opt-in keeps the `tools/list` pin: a box that declares nothing changes
   nothing. Local device always wins over a remote one for the same name.
4. Placement reason carried in the result (`node`, `accelerator`, `wall_ms`) so a
   defer or a slow call is attributable.

Out of scope for both phases: the savings ledger for accelerator calls (still the recorded
follow-up from the Hailo tier), multi-TPU pipelining, USB Coral hot-plug.

## Testing

- **Go unit:** detection (ALIVE / absent / read error / Windows), `ResolveAccelerators`
  with `__CORAL_HOME__` and an empty home guard, `HasAccelerator`, registration gating
  (Coral tools present iff listed; `tools/list` byte-identical otherwise — the existing
  Hailo pin extended), the shared-name rule (both ids listed → one `offload_object_detect`,
  owner = first), pass-through of a fake sidecar result, defer when down + unspawnable,
  status block with live health, loop tool set parity with the MCP set, health fallback to
  config (D6).
- **Sidecar contract (Python, `CORAL_ENABLED=0`):** routing, 404/400/500 shapes, idle
  exit, non-loopback refusal, manifest mismatch refusal.
- **Live gate on the Lenovo (the only check that counts):** `offload_classify_image` on
  `parrot.jpg`, `domain=birds` → top-1 "Ara macao (Scarlet Macaw)" ≥ 0.7 (the measured
  0.758); `curl 127.0.0.1:18814/health` right after shows the model loaded and a TPU
  `temp_c`; `offload_object_detect` on a real photo returns COCO boxes; wait
  `coral_idle_sec`+10 s → process gone → next call re-spawns; `/fleet/health` from the
  Qube lists `"accelerators":["coral-edgetpu"]`; an `agent_delegate` contract with
  `route:remote` that calls `offload_classify_image` succeeds on `lenovo-ampere6`;
  `tools/list` on the Qube unchanged; sustained 90 s classify loop through the sidecar
  stays ≤ 72 °C with no throttle line in `dmesg` (SETUP.md §6.3 baseline).

## Open questions for the operator

1. D7 (sidecar in the harness repo) vs a `coral-edgetpu-pipelines` repo of its own.
2. Phase B in the same release train (0.114.x) or parked until A has run for a while.
3. Whether to add face detection / PoseNet in A once their artifacts are verified, or keep
   A to the four tools above (recommended: four, then measure).
