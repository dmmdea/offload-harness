### Fixed - vLLM agent runs report their KV reuse, the CUDA mimo seat takes the tier's host cache, nvfp4 KV is refused beside a cache server

- **What it does.** Three fixes:
  - **Reuse accounting on vLLM.** The agent loop's prefill instrument now reads vLLM's
    `usage.prompt_tokens_details.cached_tokens` when a step carries no llama.cpp `timings`. When the server
    does not report the field, the step stays unmeasured. A new `prefill_source` (`timings`, `usage` or
    `mixed`) travels with the run's wire result and ledger row, and the per-step prefill milliseconds are
    averaged only over steps that carried timings.
  - **CUDA mimo seat.** The literal `mimo-9b-agent` entry on the Windows and Linux CUDA templates now
    carries `--cache-ram __CACHE_RAM__ __SLOT_SAVE__` like every other agent seat and its Vulkan twin.
  - **nvfp4 KV refusal.** A vLLM seat spec that pairs `kv_cache_dtype` nvfp4 with a `cache_server` binding
    fails validation with the reason. A storeless nvfp4 seat stays allowed, and there is no dtype allowlist.
- **Why.** These are the "what we store, how much work we reuse" lessons of ADR 0100.
  - A vLLM seat sends `usage` and no `timings`, so every vLLM step counted as observed with zero cached and
    zero prefilled tokens: a run read `measured` with no reuse, on the seat that carries the pair boxes'
    agent work.
  - The mimo entry ran at llama-server's 8,192 MiB default on every RAM tier, a 1 GiB box included.
  - The LMCache overlay guard keys on an fp8 prefix, so an nvfp4 seat with a store rendered without it. vLLM
    PR #46329 reports 8.8 vs 62 tok/s decode and every-second-request corruption for nvfp4 KV on two
    RTX 5060 Ti.
- **Check.** All on `<node-b>`:
  - A local delegation on the vLLM agent seat wrote a ledger row with `prefill_steps 3, prefill_tokens 6235,
    cache_tokens 1568, prefill_source "usage"` (20 % reuse measured). The previous row from the same seat
    carried `prefill_steps 2` and no token counts.
  - `install render --profile ampere-8 --ram-tier min` renders `--cache-ram 1024` on `mimo-9b-agent` for
    both Windows and Linux.
  - `Spec.Validate` refuses blackwell-2x16 with `kv_cache_dtype` nvfp4, citing the PR, and passes the shipped
    fp8 spec.
