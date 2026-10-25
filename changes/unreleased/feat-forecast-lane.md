### Added - the forecast lane: `offload_forecast` forecasts time series on Timestral-209M, on any box, locally or through the fleet

- **What it does.** A new model type, Mistral AI's Timestral-209M (zero-shot, probabilistic: 21 quantiles from 0.01 to 0.99,
  univariate or multivariate, past and known-future covariates, `null` for a missing value), runs as a llama-swap media seat of
  kind `forecast` that binds the config key `forecast_model`. The seat is resident (beside the agent seat) on `blackwell-16`,
  `blackwell-32`, `blackwell-2x16`, `blackwell-3x16`, `blackwell-48`, `blackwell-72`, `ampere-16` and `dual-gpu`, swappable on
  `ampere-8`, `blackwell-8` and `ampere-6`, and not seated on `volta-16`, the AMD tiers, `cpu` and `rockchip-rk3588`. It is not an
  accelerator: it uses the GPU, so it gets llama-swap's `ttl` 300, card pins, the GPU-lease fence and `/running`. The tool
  `offload_forecast` (also `local-offload forecast`, and a local-only agent-loop tool) takes `series` or a local `.csv` / `.json`
  `series_path` with `prediction_length`, `quantiles`, covariates, `out_path`, `route` and `select`. An answer of more than 2,000
  numbers is written to a file and the inline result keeps `result_path` and the first and last 8 median steps of each series.
- **Auto forwards.** `route` defaults to `auto`: this box's model when it binds one and its card is free, otherwise a fleet node
  that advertises the lane. This is new: the stt, vision and text tools defer on a model-less box in `auto`. A box that cannot run
  torch on a card (an NPU board, an AMD or CPU-only machine) reaches the model through the fleet. The node side is the dedicated
  door `POST /fleet/forecast` (bearer before body, 32 MiB cap, job id `forecast-<16 hex>`, the node's full `core.Result` as job
  data), advertised as `forecast` in `supported_task_types` with the additive health field `forecast_model`, and exempt from
  `fleet_max_concurrent_jobs` (a 0.2 s forecast must not queue behind a four-minute agent contract). Dispatch's 1 MiB cap is below
  one long multi-series request, which is why it has its own door.
- **Install.** `local-offload install forecast` (needs `uv`) builds `<home>/forecast/venv` (python 3.13, torch through
  `--torch-backend=auto`), installs the `timestral` package at a pinned commit, fetches the weights at a pinned Hugging Face
  revision into `<models>/timestral-209m` with a sha256 check, and writes the python server, which is embedded in the binary.
  The seat runs `<bin> forecast-serve`, which holds the python child under a kill-on-close job (Windows) or `Pdeathsig` (Linux).
  Design and alternatives: ADR 0098 (Proposed). Docs: `docs/systems/offload-pipeline.md`, `docs/FLEET-NODE.md`,
  `docs/systems/mcp-server.md`.
- **Measured.** On a 16 GB RTX 5060 Ti (sm_120, torch 2.14.1 with CUDA 13.2), on 2026-10-24: cold load 19 s, 804 MiB of fp32
  weights, a warm batch of 8 to 20 windows in 0.2 to 0.7 s at a 0.87 to 1.08 GiB peak, and 2.46 GiB with six covariate rows on
  20 series of 4,096 steps. Rolling-origin windows at the end of each series, MASE scaled by the context's seasonal-naive error,
  every output finite and every quantile set monotone:

  | Series | Windows x horizon, context | MASE Timestral / seasonal naive / naive | WQL Timestral / seasonal naive | 80 % coverage |
  |---|---|---|---|---|
  | ETTh1 OT, hourly | 20 x 96, 4,096 | 1.087 / 1.559 / 1.529 | 0.136 / 0.299 | 0.81 |
  | ETTh1 OT + 6 past covariates | 20 x 96, 4,096 | 1.087 / 1.559 / 1.529 | 0.134 / 0.299 | 0.82 |
  | ETTh1 OT, long context | 8 x 96, 16,384 | 0.639 / 0.893 / 1.028 | 0.108 / 0.204 | 0.91 |
  | ETTh1 OT, long horizon | 10 x 336, 4,096 | 1.494 / 2.028 / 2.439 | 0.242 / 0.463 | 0.73 |
  | ETTm1 OT, 15 minutes | 20 x 96, 8,192 | 0.520 / 0.910 / 0.692 | 0.077 / 0.183 | 0.86 |
  | The harness's own ledger, calls per hour (private, after the model's release) | 10 x 48, 1,901 | 1.442 / 2.122 / 1.711 | 1.096 / 4.231 | 0.79 |

  The median beats both baselines on every series (26 to 43 percent lower MASE than seasonal naive) and the quantile loss is
  about half of seasonal naive's or better. ETT is a public benchmark that may sit in the pretraining corpus, so those rows say
  "works and is calibrated", not "held out"; the ledger row is the clean one. Covariates did not move ETTh1 OT at horizon 96.
  The model card's GIFT-Eval figures and bf16 or fp16 were not run.
- **Weights licence.** The code is Apache-2.0. The weights are Modified MIT: the attribution "2026 - Mistral AI" is kept in
  copies, and there are no rights for a company whose global consolidated monthly revenue exceeds USD 20M (derivatives
  included). The harness surfaces this and does not gate on it.
- **Check.** The evaluation above, run on the real weights on a leased card, and the embedded server's every route hit once
  (`/health`, univariate, ragged batch, covariates, joint variates, and the 400 refusals). Then the built binary on a leased
  RTX 5060 Ti, with the seat served by llama-swap exactly as the tier renders it (ttl 300, card pin, `checkEndpoint /health`):
  `local-offload forecast` on the last 4,096 ETTh1 OT steps before a held-out 96 answered ok in 41.3 s cold (llama-swap start,
  load and warm-up) and 0.17 s warm, MAE 0.716 against the held-out steps, 0.97 of them inside the 10-90 band; the seat took
  1.25 GiB on the card; a box with no forecast model forwarded the same call through `POST /fleet/forecast` to a node serving
  the seat (job `forecast-<16 hex>`, placement `remote: no local forecast model`) and got the same answer; and llama-swap's
  `/running` was empty 330 s after the last call.
