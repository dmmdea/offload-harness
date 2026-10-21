---
status: Accepted
date: "2026-10-21"
---

# 0095 — Humming serves the weight formats Marlin cannot; Marlin keeps the 4-bit seat

## Context

The operator asked for [Humming](https://github.com/vllm-project/humming) to be integrated into the harness and deployed.
Humming is a JIT-compiled GEMM library for quantized inference from the vLLM project (Apache-2.0). It runs any weight type
up to 8 bits against 16-bit, 8-bit or 4-bit activations on every NVIDIA card from SM75 up, dense and MoE, and compiles its
kernels at run time through NVRTC.

What it already is in this harness, read from the installed packages and the seat logs on 2026-10-21:

- **Already installed everywhere vLLM is.** vLLM pins it as a dependency (vLLM 0.32.0 requires `humming-kernels[cu13]==0.1.17`;
  the 0.28 venvs carry 0.1.12). Integrating it installs nothing new.
- **The only vLLM kernel for 2, 3, 5, 6 and 7-bit weights.** vLLM's compressed-tensors WNA16 scheme accepts 2 to 8 bits and
  Marlin serves only 4 and 8, so a checkpoint at any other width falls through vLLM's kernel list to Humming with no flag.
  That is why the 3-bit GSQ seats declared for `blackwell-16` and `ampere-16` run on it.
- **Not the kernel of a 4-bit seat on SM120.** For 4-bit and 8-bit weights vLLM ranks Marlin first and moves Humming ahead of it
  only on SM90 (`_HUMMING_PREFERRED_CAPABILITIES = {90}` in vLLM 0.32.0). The `blackwell-2x16` and `blackwell-3x16` seats
  (`RedHatAI/Qwen3.8-27B-INT4`, W4A16) log `Using MarlinLinearKernel for CompressedTensorsWNA16`.
- **Its JIT behavior was unmanaged.** With nothing set, Humming keeps compiled kernels and the Python modules it generates in
  `~/.humming`, outside the seat's own directories, and compiles one layer shape's tile variants with one build process per
  CPU. A cache entry is keyed on the venv path and the package's file times, so entries from a removed venv are never reused
  and nothing cleans them.
- **Its other mode does not fit a seat.** vLLM's online quantization (`--quantization humming` with
  `VLLM_HUMMING_ONLINE_QUANT_CONFIG`) quantizes a BF16 checkpoint at load, with round-to-nearest and no calibration. A seat
  unloads after five idle minutes, so every cold start would re-read the BF16 weights (51.75 GiB for the 27B) and quantize
  them again.

## Decision

1. **The seat launchers manage Humming's JIT.** `seat_fg.sh` exports `HUMMING_CACHE_DIR` and `HUMMING_TMP_DIR` under the seat's
   work directory (`humming-cache`, `humming-tmp`), beside vLLM's compile cache, and caps `HUMMING_MAX_PARALLEL_BUILD_WORKERS`
   at 8. The seat env can move them (`SEAT_HUMMING_CACHE`, `SEAT_HUMMING_TMP`) and change the cap
   (`SEAT_HUMMING_BUILD_WORKERS`). `vllm-seat-run.sh` sets the same under the stack's `cache` directory. The cache survives
   the idle unload, is cleaned with the seat, and the generated modules (code the engine imports) stay in a directory only
   the seat's user writes.
2. **Marlin keeps the 4-bit seats.** `--linear-backend humming` is an opt-in, documented commented-out in the WSL seat env
   template. The proving run below found the same output and the same footprint, and the Humming arm's engine stopped during
   a 23.7k-token prefill that Marlin served. No seat moves to it, and a seat that opts in proves that prefill first. Speed is
   not a reason to change a seat ([ADR 0050](0050-placement-ranks-adequate-seats-by-expected-completion.md)).
3. **A higher-precision seat is a saved checkpoint, never online quantization.** A compressed-tensors `pack-quantized`
   checkpoint at 5 or 6 bits loads through vLLM's own WNA16 scheme and runs on Humming with no flag. `--quantization humming`
   is never passed with a compressed-tensors checkpoint: its ignore list is matched as substrings, and on the 27B INT4
   checkpoint the entry for each `linear_attn` module matches the quantized projections inside it (read from vLLM 0.32.0's
   `humming.py`, not run). Such a seat runs on Humming whatever its flags, so the stall below is its first thing to prove.
   Whether to build one, and where, is the operator's decision; the fit estimate and the options are recorded outside this
   repository.
4. **No Humming on a vLLM older than 0.29.** vLLM 0.28 has no `utils/humming` package and its Humming 0.1.12 predates the
   shard-scale and TMA-fence fixes of 0.1.17 (Humming #142, #144). A seat on 0.28 keeps Marlin.
5. **Humming moves with vLLM.** Its version is the one the seat's vLLM pins; a newer Humming arrives with the vLLM release
   that pins it and is a new admission in [the security doc](../../systems/security.md#admitted-third-party-software).

## The proving run

One run on 2026-10-21 on `<node-b>`'s tensor-parallel pair: two 16 GB SM120 cards, vLLM 0.32.0, Humming 0.1.17,
`RedHatAI/Qwen3.8-27B-INT4` (W4A16), a 163,840-token window, utilization 0.90, prefill chunks of 1,568 tokens, the L2
cache-server tier off. Both arms were started on scratch ports under one GPU lease by the launcher with the production seat's env. The control
served on Marlin, as the production seat does; the other arm added `--linear-backend humming`. Each ran the same probes in the
same order: eight greedy chat completions with token logprobs, one tool call, one strict JSON schema, prompt logprobs over a
fixed 7,038-token corpus in 24 chunks, then the needle gate's cold phase (a 23.7k-token prompt with a planted answer).

| | Marlin (control) | Humming |
|---|---|---|
| Kernel line | `Using MarlinLinearKernel for CompressedTensorsWNA16` | `Using HummingLinearKernel for CompressedTensorsWNA16` |
| Weights per worker | 8.89 GiB | 8.91 GiB |
| Weights + non-torch + peak activation | 9.10 GiB | 9.12 GiB |
| KV pool | 203,692 tokens | 202,216 tokens |
| Start | 160 s | 175 s with the kernels cached; 291 s the first time, compiling 94 kernels (51 MB) |
| Prompt perplexity, 7,038 tokens | 14.334 | 14.330 (mean \|Δ logprob\| 0.027, p99 0.144) |
| Greedy completions identical | 5 of 8 | the other 3 part at a near-tie (tokens 1, 141 and 161) |
| Tool call, strict schema | pass, pass | pass, pass |
| Needle (23.7k tokens) | found | engine stopped: no step returned for 120 s, and the seat's step watchdog (`VLLM_EXECUTE_MODEL_TIMEOUT_SECONDS=120`) ended it with `RPC call to sample_tokens timed out` and `EngineDeadError` (HTTP 500) |

It was one step that stalled, not a slower prefill. The seat's LMCache MP server logs each 1,568-token prefill chunk it stores as the engine
finishes it: Marlin's 15 chunks arrived 1.5 to 7.1 s apart (87.9 s to the first token), while Humming's first chunks arrived
within 6 s of the request and then nothing did for 120 s. A longer watchdog would only have held the seat longer.
What the logs rule out: no worker raised an error; Humming compiled nothing during it (every cache entry dates
from the first start); and the seven retried allocations the caching allocator logged before it (364 to 512 MiB, during the
prompt-logprob probe) appear in the Marlin arm with the same count and sizes, and that arm served on. The cause is not
established, and the run was not repeated. vLLM #62816 moves vLLM to Humming 0.1.18, which reworks its kernel heuristics and
stream-k (Humming #150) and adds an SM120 load hint (#151); the needle arm is the first run for that release.

The first attempt also found a hazard that is not Humming's: both arms died on a 7,433-token `prompt_logprobs` request,
because vLLM computes an fp32 log-softmax over the 248k-token vocabulary for every prompt token and asked a full card for
1.45 GiB. The probes above score the corpus in chunks for that reason.

## Consequences

- Every vLLM seat started by the launchers writes Humming's kernels to its own directories, compiles at most eight at once,
  and logs `seat_fg: Humming kernel cache <dir> (at most N parallel builds)`. A seat that never runs Humming leaves the
  directories empty.
- A kernel change changes the KV numerics: a seat moved to `--linear-backend humming` gets a new L2 `base_path`, as for any
  change of the engine's layout.
- The seat pin of `internal/agent/props.go` (engine version, model root, served name, window) cannot tell a Marlin seat from a
  Humming one; a measurement that compares them records the kernel itself.
- The 3-bit tiers already serve on Humming. The stall was seen on a 4-bit checkpoint forced onto it, and it applies to them
  until a long-prefill needle on their own seat says otherwise.
- A client that asks a seat for `prompt_logprobs` over a long prompt can end that seat, on either kernel.

## Alternatives considered

- **Put the 4-bit seats on Humming.** Rejected by the proving run: equal output, no smaller footprint, an engine that stopped
  on a long prefill Marlin served, and vLLM's own maintainers rank Marlin first on this architecture.
- **Online quantization to a higher bit width.** Rejected for serving: it re-reads and re-quantizes the BF16 weights on every
  cold start and has no calibration.
- **Leave the JIT defaults.** Rejected: a cache outside the seat's directories that nothing cleans, and a build that uses every
  CPU of a desktop box on a first forward.
