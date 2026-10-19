### Fixed - the nightly offload-dream run finishes, and adopts into the files the harness reads

- **What.** `scripts/offload-dream.ps1` changes in two ways.
  - It asks the binary where each file lives (`local-offload data status --json`) instead of assuming `%USERPROFILE%\.local-offload`. Its ledger check and the five files it adopts into (`thresholds_path`, `tier_overrides_path`, `router_weights_path`, `confhead_path`, `confhead_thresholds_path`) are the paths the harness reads. Staging, backups, the proposal and the log go under the configured `home`.
  - Passing `-DataDir` still points the whole run at that folder. A binary without `data status` keeps the old default.
  - Every call to the binary now goes through one helper that keeps stderr apart from stdout.
- **Why.** Two defects.
  - Under Windows PowerShell 5.1 with `$ErrorActionPreference = 'Stop'`, a stderr line from a native command ends the script. `confhead-calibrate` exits 0 but prints a note on stderr about the test traffic it left out, so on a node with such traffic the run stopped there with exit 1, before `optimize` and before any adoption. The fleet node that showed it had stopped at that step on every run in its log.
  - A node that moves its data off C: by setting `home` would have had its nightly thresholds written beside a tree the harness no longer reads.
- **Check.** The script ran under Windows PowerShell 5.1 on a fleet node whose home had just moved to its data drive, with `-DryRun`.
  - It exited 0 and ran every stage through `confhead-eval`, whose JSON parsed.
  - It named the data-drive paths as the adoption targets.
  - It wrote its staging, backup, proposal and log there and nothing under the old home.
