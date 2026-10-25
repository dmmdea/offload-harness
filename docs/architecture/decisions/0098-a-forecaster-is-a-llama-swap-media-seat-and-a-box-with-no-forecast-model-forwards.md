---
status: Proposed
date: "2026-10-24"
---

# A forecaster is a llama-swap media seat, and a box with no forecast model forwards

## Context

The operator asked on 2026-10-24 for a new model type on the harness, Mistral AI's Timestral-209M, tested and wired across
the fleet. It is a zero-shot probabilistic time-series forecaster: numbers in, 21 quantiles (0.01 to 0.99) out, no text, no
tokenizer, no chat route. It is not a llama.cpp model. It runs in torch, in float32 only (the package states that is the
validated dtype), and holds 804 MiB of weights on the card.

It was evaluated on a 16 GB RTX 5060 Ti (sm_120, torch 2.14.1 with CUDA 13.2) before any wiring. The median beat both
baselines (seasonal naive, naive) on all six series (26 to 43 percent lower MASE than seasonal naive), the weighted quantile
loss was about half of seasonal naive's or better, and the 10 to 90 percent band covered 73 to 91 percent against 80 nominal.
Cold load was 19 s, a warm batch of 8 to 20 windows took 0.2 to 0.7 s, and the peak was 0.87 to 1.08 GiB for univariate
batches and 2.46 GiB with six covariate rows on 20 series of 4,096 steps.

The harness already had the pieces of a place to run it, and each one answered part of the question:

- [ADR 0019](0019-alias-backed-media-is-declared-per-tier.md) and [ADR 0020](0020-residency-is-declared-with-matrix.md): an
  alias-backed model is declared per tier as a media seat, which renders the llama-swap entry and writes the config binding
  together, with a residency role per tier. Whisper is the precedent for a non-chat model on this path.
- [ADR 0024](0024-accelerators-are-additive-to-the-gpu-tier.md): an accelerator is a device beside the GPU, with its own
  sidecar and its own tools. A forecaster that runs on the GPU is not one.
- [ADR 0018](0018-machine-wide-fenced-gpu-lease.md) and [ADR 0094](0094-a-lease-names-its-cards-a-card-has-one-holder.md): a
  call to a model on a card goes through the lease fence, and a held card is a place in line or a capacity defer.
- [ADR 0072](0072-a-fleet-node-transcribes-audio-its-caller-uploads-so-a-held-card-is-a-place-in-line.md) and the vision and
  text lanes: a node runs one single-shot task for a caller whose own card is held, behind a door with its own body cap.

Two things were new. The model needs a python runtime that no tier ships (torch plus a pinned package plus weights). And a
box that cannot run torch on a card at all (an NPU board, an AMD or CPU-only machine, a Volta card the CUDA 13 wheels dropped)
has no local forecast and, unlike the other media lanes, nothing else to fall back to.

## Decision

**1. A forecaster is a media seat of a new kind, `forecast`.** A tier declares it in `media_seats` like whisper. The seat
renders a llama-swap entry (`ttl: 300`, `checkEndpoint: /health`, the tier's card pin in `env`) and binds the config key
`forecast_model` (roster route `forecast`) in the same step; doctor fails a bound alias llama-swap does not serve. It is
not an accelerator: it uses the GPU, so it gets llama-swap's idle unload, card pins, the lease fence and `/running` for free
instead of a second serving path.

**2. Residency is per tier.** The seat is resident, beside the agent seat and pinned to the card that holds the whisper or
embedder residents, on the tiers with cards of 16 GB or more (`blackwell-16`, `blackwell-32`, `blackwell-2x16`,
`blackwell-3x16`, `blackwell-48`, `blackwell-72`, `ampere-16`, `dual-gpu`). It is swappable on `ampere-8`, `blackwell-8` and
`ampere-6`, the whisper precedent on those cards, so a forecast there can evict the agent seat for its length. It is not
seated on `volta-16` (the CUDA 13 wheels dropped sm_70 and nothing was measured), on `amd-rdna3`, `amd-rdna3-dgpu` and
`amd-gcn` (no torch GPU path on those boxes), on `cpu` (no model runs on the CPU) or on `rockchip-rk3588` (NPU only). Those
boxes reach the model through the fleet. The reference box of `ampere-6` ships the release unbound, because it is shared
production at 5.6 of 6.1 GiB worst case and its llama-swap restart drops the memory stack's embedder; it is bound in a window
the release lane agrees.

**3. The seat launches through the harness binary.** The seat's command is `<bin> forecast-serve --forecast-dir
<home>/forecast --model <models>/timestral-209m --port ${PORT}`, with `bin` written once with the `__OFFLOAD_HOME__` and
`__EXE__` tokens so one string renders on Windows and Linux. `forecast-serve` holds the python child under a kill-on-close
job (Windows) or `Pdeathsig` (Linux), so llama-swap stopping the seat never orphans a process that holds the card. The
launcher passes `HF_HUB_OFFLINE=1` and binds loopback only; the server has no authentication.

**4. The python server is embedded in the binary.** `internal/forecastserve/timestral_server.py` is `go:embed`ded.
`local-offload install forecast` writes it to `<home>/forecast/timestral_server.py`, and `forecast-serve` rewrites it when it
differs, so the server is versioned with the binary that launches it. `install forecast` also builds the venv with `uv`
(python 3.13, torch through `--torch-backend=auto`), installs the `timestral` package at the pinned commit and fetches the
weights at the pinned Hugging Face revision into `<models>/timestral-209m`, verifying the sha256. Every step skips when it
is already right.

**5. `offload_forecast` is registered on every box and its route defaults to `auto`, which forwards.** `auto` runs on this
box when it binds `forecast_model` and its card is not lease-blocked. It forwards to a fleet node that advertises the lane
when the box binds no forecast model, or when the model is bound and the card is blocked and a node is eligible. This is new
behaviour: the stt, vision and text tools defer on a model-less box in `auto`. It is what wiring the model on the entire
fleet means for a box that cannot run torch on a card. `route: local` stays available for a caller that wants nothing to
leave the box, and a model-less box with no eligible node defers with the placement reason, never with a silent local
failure. A box with the model and a held card and no eligible node still waits locally (queued-local beats ineligible-remote).

**6. The fleet door is dedicated, and exempt from the job cap.** `POST /fleet/forecast` (bearer before body, 32 MiB cap, job id
`forecast-<16 hex>` inside the payload) stores the node's full `core.Result` as the job data. `forecast` does not count
against `fleet_max_concurrent_jobs`, like `stt`: a 0.2 s forecast must not queue behind a four-minute agent contract, and it
drives its own seat, not the text endpoint the cap protects. `/fleet/dispatch` is not used because its 1 MiB cap is below one
long multi-series request.

**7. The agent loop gets `offload_forecast` too**, local only and with inline series. It is not added to the research profile.

**The weights licence is surfaced, not gated.** The code is Apache-2.0. The weights are Modified MIT: the attribution "2026 -
Mistral AI" stays in copies, and there are no rights for a company whose global consolidated monthly revenue exceeds USD 20M.
The operator's businesses are far below that cap. The harness does not check revenue and does not refuse to serve.

## Consequences

- A forecast costs no tokens and no cloud call (ADR 0001 holds): every route ends on this box or a tailnet node.
- A node adds one health field, `forecast_model`, and one `supported_task_types` entry, both only when the lane is admissible.
  An older node never lists them and is never picked. `tools/list` grows by one tool on every box.
- On a swappable tier a forecast evicts the agent seat for a moment and the seat's own `ttl` frees the card after 300 s. That is
  the whisper behaviour on those cards; a measurement on the 6 GB and 8 GB cards is still owed.
- A model-less box now sends work over the tailnet without being asked. It is the only lane that does. The series travels in
  the job body, and a large result file is written on the asking box.
- A box without `uv` or without the weights cannot serve the seat; `forecast-serve` fails with a message naming
  `local-offload install forecast`.
- The python server is a second runtime to keep current. Its dependencies are pinned by commit and revision, and a changed or
  truncated checkpoint is an error.

## Alternatives considered

- **An accelerator sidecar (ADR 0024).** Rejected: an accelerator is a device beside the GPU, with its own detection, tools and
  fleet visibility rules. This model runs on the GPU, so it would duplicate the card pins, the lease fence, the idle unload and
  the `/running` view that llama-swap already gives a seat.
- **A generic `/fleet/dispatch` task.** Rejected: dispatch caps its body at 1 MiB, below one long multi-series request, and
  would put a 0.2 s call behind the job cap. The vision, text and stt doors made the same call.
- **CPU inference on the model-less tiers.** Rejected: no model runs on the CPU in this harness, the server refuses `cpu`
  without an explicit flag, and no tier seats it there. Those tiers forward instead.
- **`auto` that defers on a model-less box, like stt.** Rejected: those boxes can never run the model, so a defer would make
  the lane unreachable for them.
- **A launcher per operating system.** Rejected for the `<bin> forecast-serve` verb, which renders from one string.

## Related code

- [internal/forecastserve/forecastserve.go](../../../internal/forecastserve/forecastserve.go) and
  [timestral_server.py](../../../internal/forecastserve/timestral_server.py): the launcher and the embedded server.
- [install_forecast.go](../../../install_forecast.go): `install forecast`, with the pinned revisions and hashes.
- [internal/mediaseat/mediaseat.go](../../../internal/mediaseat/mediaseat.go) and
  [internal/servingtmpl/servingtmpl.go](../../../internal/servingtmpl/servingtmpl.go): the `forecast` seat kind and its entry.
- [setup/templates/profiles.json](../../../setup/templates/profiles.json): the per-tier seat declarations.
- [internal/forecastclient/forecastclient.go](../../../internal/forecastclient/forecastclient.go) and
  [internal/pipeline/forecast.go](../../../internal/pipeline/forecast.go): the fenced client and `runForecast`.
- [internal/mcpserver/forecast.go](../../../internal/mcpserver/forecast.go): the tool, the CSV reader and the result spill.
- [internal/forecastremote/forecastremote.go](../../../internal/forecastremote/forecastremote.go) and
  [internal/fleetnode/forecast_task.go](../../../internal/fleetnode/forecast_task.go): the caller and the door.

## Related docs

- [../../systems/offload-pipeline.md](../../systems/offload-pipeline.md#the-forecast-lane-timestral-209m): the seat, the wire, the tool and the tier table.
- [../../FLEET-NODE.md](../../FLEET-NODE.md#the-forecast-task-post-fleetforecast) and [../../systems/fleet-node.md](../../systems/fleet-node.md): the door.
- [../../systems/mcp-server.md](../../systems/mcp-server.md): the tool surface.
- [../../glossary.md](../../glossary.md#forecast-lane): the term.
