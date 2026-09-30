---
status: Accepted
date: "2026-09-30"
---

# ADR 0062 — An RK3588 SoC board is its own tier: it serves from the NPU on a unified-memory budget, never from the CPU, and a GPU entry waits for a clean measurement

## Context

The operator added a Rockchip RK3588 board (reference: an Orange Pi 5, RK3588S) to the fleet. It has no
NVIDIA or AMD GPU: 4x Cortex-A55 + 4x Cortex-A76, 7.7 GiB of LPDDR4X shared by the CPU, a Mali-G610 GPU
and a 3-core 6 TOPS NPU. It also runs the operator's home-automation stack, and the operator's orders
shape everything below: the CPU is reserved for that stack (no CPU inference, ADR 0054's amendment), part
of the RAM stays reserved for it, and every model unloads after five idle minutes.

Before this change the harness could not use the board at all:

- `hwdetect` only recognised a GPU through a PCI vendor id, so the SoC classified as `cpu` and the
  installer would have rendered CPU seats.
- `fleet-serve` refuses to start without a GPU memory source, and the only Linux source was amdgpu sysfs.
- No engine in the harness drove the NPU.

What was measured on the reference board (2026-09-29/30), after moving it from the vendor 6.1 kernel to
Ubuntu's mainline 7.0 kernel on the operator's order ("latest kernel, latest drivers"):

- **NPU**: Rockchip's RKLLM 1.3.1 and RKNN 2.3.2 runtimes need Rockchip's own `rknpu` driver, not the
  mainline in-tree `rocket` driver. On mainline it comes from an out-of-tree DKMS build (0.9.8). Both
  runtimes run there; LLM decode is weight-bandwidth-bound and RKLLM on RK3588 only offers W8A8, so the
  largest LLM that fits the budget is Qwen3.5-2B (2.98 GB + a 0.70 GB vision encoder). The runtime needs
  at least as many enabled CPU threads as NPU cores (3), and which cores it uses changes prefill ~5x.
- **GPU**: Mesa 25.2.8 panvk exposes the Mali as a conformant Vulkan 1.4 device, but llama.cpp b11270's
  first compute submission never completes: panthor reports a job timeout and llama.cpp aborts with
  `vk::DeviceLostError`, for every model and batch size tried, on a freshly reset GPU.

## Decision

1. **A tier, `rockchip-rk3588`, detected from the device tree.** The root `compatible` carries
   `rockchip,rk3588` (vendor kernel) or `rockchip,rk3588s` (mainline); either classifies the board. It
   is never `cpu`.
2. **Unified memory is the GPU memory source.** A `linux-meminfo` provider advertises MemTotal less
   `uma_reserve_gib` as capacity and MemAvailable less the reserve as free. The reserve is the host
   workload's RAM; the tier seeds 3 GiB. Placement and the fleet overview read the same fields as any
   other node.
3. **The NPU serves the models.** A new seat kind, `rkllm`, renders an llama-swap entry that launches
   `accelerators/rknpu/rkllm_server.py`, an OpenAI-compatible server over the RKLLM runtime (chat and
   vision, one generation at a time, ttl 300 like every seat). A new accelerator, `rknpu`, clones the
   Coral sidecar contract (ADR 0024/0038) for image classification, detection and embedding on the NPU.
   Its detection accepts both kernels' sysfs layouts and never matches `rocket`.
4. **No CPU inference, and the CPU reservation is a seat setting.** The tier renders no CPU seat and no
   `alt_backends`. The RKLLM runtime's host threads are bounded by the seat's `cpu_mask` (at least three
   bits); the tier default is the A55 cluster, the strict reading of the operator's reservation.
5. **The tier has its own template, and it may hold no model of its own.** The stock Vulkan template
   always renders models that do not fit a ~4.7 GiB budget. `llama-swap.linux-rk3588.yaml` lists only what
   fits, which today is nothing: every model is a tier seat. The serving audit therefore accepts an empty
   `models:` map in a raw template only when that template carries an `# offload-seats:` directive, and
   `Render` refuses any result that still serves no model. A set made only of seats no longer renders with
   a leading operator.
6. **A GPU entry comes back only on a clean measurement.** The template keeps the Vulkan conventions
   (device pin, loader path, flag macros) so that a Mesa/panthor/llama.cpp combination that passes on this
   silicon can be re-added as an ordinary llama.cpp entry. vLLM has no Mali or RKNPU target, so the tier
   carries a `vllmSeatDebt` row instead of a `vllm_seat`.

## Consequences

- A second RK3588 board (Rock 5B, Orange Pi 5 Plus, ...) installs as this tier from the same detection,
  but its NPU driver and model files are box work the installer does not do: the vendor runtime libraries,
  the DKMS driver on a mainline kernel, and the `.rkllm` / `.rknn` files.
- The node registers with `fleet_agent_enabled: false`. It serves the vision lane (the NPU VLM) and the
  accelerator lane (the rknpu sidecar) to other boxes, and its chat model is reachable through the chat
  lane. It takes no agent contract.
- Answer quality of the NPU seat against other tiers' seats is not yet judged. The notes say so, and the
  seat is the largest that fits, not a measured winner.
- The NPU seat refuses grammar and json_schema requests with a 400 (`constrained_decoding_unsupported`),
  because the runtime cannot constrain sampling and ignoring the constraint would return an answer that only
  looks valid. `assess_image` and the grammar cascade tasks do not run on this node; free-text chat and vqa do.
- The seats-only template rule is general. Any future template that leaves every model to its tier's
  seats inherits the audit exception and the empty-render refusal.

## Amendment (0.153.0)

Measured on the reference board on 2026-09-30, the NPU seat as first shipped failed the vision lane two ways, and
the tier's node limits were the harness defaults, sized for a card that serves several requests at once.

1. **The seat carries a repeat penalty, 1.1.** Greedy decoding at the seat's default `repeat_penalty` of 1.0 loops
   on VQA and runs to the 256-token cap (the lane defers "vision output truncated"); at 1.1 a blind four-question
   VQA check scored 3/4. `rkllm_server.py` gains `--repeat-penalty` (default 1.0, range 0.01 to 10, refused at
   startup outside it) and applies it to a request that sends neither `repeat_penalty` nor `repetition_penalty`;
   a value the request sends wins. The `rkllm` media seat declares it as `repeat_penalty` and renders the flag only
   when set. It is the only sampling default an rkllm seat may declare; `temp`, `top_p` and `top_k` stay refused.
2. **The vision lane is limited to the seat's declared tasks: `vqa` and `ocr`, never `assess_image`.** The runtime
   cannot constrain sampling and `assess_image` always sends a grammar, so the seat would answer 400 after the
   node had taken the job. A media seat declares `tasks`; `mediaseat.Bindings` derives the node key `vision_tasks`
   from its vision subset, in the order `vqa`, `ocr`, `assess_image`, beside `vision_model`, and a `config_seed`
   may not write it. The node refuses a task outside the list at ack time with a `400` that names the set, and
   publishes the list in `/fleet/health` as `vision_tasks`. The delegator reads it (absent means all three, so a
   node that predates the field is unchanged) and `PlaceVision` skips a node that does not list the task.
   `classify` and `extract` are accepted in `tasks` now for the text door that follows; only the vision subset is
   bound in this release.
3. **The tier seeds limits for a one-generation NPU.** `fleet_max_concurrent_jobs` 1, because the NPU runs one
   generation at a time and the default of 4 would queue three jobs behind it, where their wait counts against
   the delegator's wall. `request_timeout_sec` 240, below the delegator's 300 s fleet vision budget
   (`visionremote.Budget`), with the cold load measured at 13 s. `max_input_chars` 8000 (about 2,000 tokens, about
   90 s of prefill on the A55 cluster at the measured 22.7 tokens per second) and `ocr_max_tokens` 512 (the
   default 1024 is over two minutes of decode at 7.71 tokens per second). `fleet_max_queue_depth` is not set: its
   default is twice the concurrency, which is 2 here (one running, one waiting), inside the NPU server's own
   window of one running and two waiting before it answers 503 `busy`.

**Upgrading an installed node.** The binary does not ship `rkllm_server.py`: it is the `accelerators/rknpu` copy under
`RKNPU_HOME`, which `install.sh` never refreshes, and `rkllm-serve.sh` execs its arguments into it. A 0.153.0 render
emits `--repeat-penalty 1.1`, which a 0.151.1 server refuses (`unrecognized arguments`, exit 2), so the seat would
never start. Refresh `accelerators/rknpu` in `RKNPU_HOME` (at least `rkllm_server.py`) first, then re-render
`llama-swap.yaml`. An existing `config.json` is not reseeded either: add `vision_tasks` (`["vqa","ocr"]`) and the four
limits by hand, or delete the file to regenerate it; `local-offload audit-config` lists them as SEED-ONLY.

Under `route: remote`, an `assess_image` for a fleet whose only vision node is this tier now defers at placement
(defer class `capacity`), naming the node and its `vision_tasks`, instead of a dispatch that would fail; with
another vision node on the roster it runs there.

## Amendment (0.154.0)

The statement above that the grammar cascade tasks do not run on this node is amended by
[ADR 0069](0069-an-unconstrained-seat-runs-classify-and-extract-from-the-prompt-and-the-text-lane-ships-dark.md).
The seat is now declared in the node config key `unconstrained_seats` (written from the `rkllm` media seat), and for
a declared seat the pipeline sends no grammar: classify and extract run from a prompt that states the JSON shape and
are accepted only after strict schema validation. `assess_image` still does not run here. The fleet `text` lane that
would expose classify and extract to a delegator ships dark: this tier declares no text task until measured data
passes.
