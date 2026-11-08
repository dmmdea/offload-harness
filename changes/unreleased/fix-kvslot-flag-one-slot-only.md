### Fixed - `--slot-save-path` renders only after `--parallel 1`, the 35B agent seat gets it, and the audit refuses an entry that states no slot count

- **What it does.** Three changes to the 0.209.0 render:
  - **Templates.** The `__SLOT_SAVE__` token leaves the shared `common` macro of all twelve llama.cpp
    templates. It now sits right after `--parallel 1` in each entry that states one slot. The
    `qwen3.6-35b-a3b-agent` entry on the Windows and Linux CUDA templates (the `ampere-6` RAM-spill
    agent seat) gains it; it had none, because it does not use the macro.
  - **Audit.** The render-time slot audit now also refuses the flag on an entry that states no
    `--parallel` at all, as the agent loop and the `/fleet/kvslot` lane already did at run time.
- **Why.** Through the macro, 0.209.0 rendered the flag on `gemma4-e2b`, which states no slot count and
  so runs the server's default of several. Nothing was saved or restored there: both run-time doors
  refuse such a seat (`not-eligible`). But the render broke its own rule that a slot directory sits
  only beside one slot, and the audit only looked for a stated count above one. Live nodes are
  unaffected: a deploy never rewrites a node's llama-swap.yaml.
- **Check.** This branch's binary rendered every tier for both OSes into a scratch folder, with a
  b11977 build directory:
  - Of the 38 tier and OS combinations, 30 rendered. The other 8 have no template for that OS.
  - 27 of the 30 carry the flag. `maxwell-3` and `rockchip-rk3588` have no one-slot llama.cpp chat or
    agent entry.
  - In every rendered config, after expanding the macros, each entry with the flag states `--parallel 1`.
  - `ampere-6` now flags `qwen3.6-35b-a3b-agent`.
  - The new audit refuses `gemma4-e2b` in the 0.209.0 renders of `ampere-8`/linux and
    `blackwell-2x16`/windows, and passes the new renders.

### Fixed - `loupe` reads the KV reuse of local delegations again

- **What it does.** The `agent_prefill` section of `local-offload loupe` now reads every run row,
  including the inner `agent` row that a delegator-local job writes beside its `agent_delegate` row.
  Dispatch markers and refusal rows stay out. Job counters still read one row per job (register C-62).
- **Why.** The report built its prefill figures from the job rows, and since 0.143.0 those drop the inner
  row whenever its parent is present. The inner row is the only one carrying `prefill_*`, and it now also
  carries `kvslot_*`. So every local delegation read as unmeasured: on the vLLM agent seat, the 0.209.0
  reuse fix was in the ledger and nowhere in the report. The parent row carries no prefill steps, so
  each run is still counted once.
- **Check.** `loupe --since 1 --include-test-traffic --json` on `<node-b>`'s ledger:
  - Released 0.209.0: `measured_rows 0`, `kv_reuse_pct null`, `insufficient_data`.
  - This branch: `measured_rows 25`, `prefill_tokens 6235`, `cache_tokens 1568`, `kv_reuse_pct 20.09`.
  - Over the whole ledger, measured rows go from 2,334 to 3,908, and reuse from 49.5 % to 48.9 %.
