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
