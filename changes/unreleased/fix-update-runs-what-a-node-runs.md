### Fixed - `update` updates what a node runs: it finds a hand-laid live path from its record and never installs on `all`

- **The gap.** The verb found a component only through `--live`, `update.paths` in `config.json` or `<home>/<component>`.
  A node whose llama-swap lives outside `<home>` (a plain `C:/llama-swap` folder) was updated once with `--live`, and
  the next bare run failed with `llama-swap: <home>/llama-swap/llama-swap.exe is not there`, although `installed.json`
  already held the live file as the current generation. And `--component all` (the default) installed every component
  the node did not have: a dry run on a node with no stable-diffusion.cpp planned `sdcpp ... install`.
- **What it does.** `installed.json` records each component's `live` path whenever the verb finds the live build there,
  and the live path is now `--live`, then `update.paths`, then the recorded `live`, then `<home>/<component>`. A record
  written before `live` existed still finds a single-file component through its current generation's path when that
  is a file. On `all`, a component with no record and nothing at its live path is reported `not-installed` (exit 0,
  nothing written); naming it with `--component` is the one way to install it. `--rollback` resolves the live path the
  same way.
- **Check.** This branch's binary, dry run on a Windows node with `update.paths` removed from a copy of its config:
  llama-swap was found from its record (`v268  none`) where the released binary stopped with `is not there`; `sdcpp`
  read `none  -  -  not-installed` with the hint line; `--component sdcpp --dry-run` still planned `install`. Docs:
  `docs/systems/update.md` (Usage, Layout).
