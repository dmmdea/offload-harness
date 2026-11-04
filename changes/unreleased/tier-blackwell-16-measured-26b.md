### Changed - `blackwell-16` serves the 26B with `--n-cpu-moe 14`; the Blackwell CUDA-13 "cuBLAS fallback" warning is retired

- **What it does.** The tier's 26B placement is `n_cpu_moe` with N=14 (`n_cpu_moe_max` 14), so
  both 26B entries render `--n-cpu-moe 14 -ngl 999`: the top 14 expert layers in host RAM,
  everything else on the card. `detect.ps1` no longer warns that a CUDA-13 build serves Blackwell
  through a slow cuBLAS fallback, and the installer's build report says what was measured instead.
- **Why.** Measured 2026-11-05 on an RTX 5080 16 GB that is also the desktop's display card
  (`docs/tiers/reports/blackwell-16-node-h.md`). Full GPU needs about 14.4 GB on the card and
  spills whenever the desktop holds more than about 1.5 GB: 17 tok/s, against 139 tok/s once at a
  near-empty card. N=14 keeps the seat at 9.1 GB on the card, 62.5 tok/s, and fits under the
  heaviest desktop seen there (5.4 GB); N=8 (81 tok/s) and N=4 (121 tok/s) fit only lighter
  desktops. The same run's llama-bench on the win-cuda-13.4 prebuilt b12110: E4B pp512 17694 tok/s,
  tg128 212; E2B 34454 / 345. No fallback, so the 2026-07-15 finding on an earlier build is retired.
- **Check.** On that box, both entries patched by hand to the rendered form and llama-swap
  restarted while idle: one chat completion on `gemma4-26b-a4b` at the owner's desktop ran at
  pp 472 / tg 58.2 tok/s with 10678 of 16303 MiB used, cold load included in 8.5 s.
