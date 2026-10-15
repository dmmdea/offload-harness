### Fixed - an installed.json recorded under an older layout is brought to the detected tier, and no longer hides the config's tier from doctor and report

- **What it does.** `install.sh` now brings an existing `installed.json` to the tier the run detected and rendered the serving
  config for. A recorded `profile` that differs is rewritten together with its `backend` (a jq merge through a temp file);
  `accelerators`, `alt_backends` and every other key stay as recorded, an equal profile leaves the file byte for byte, an unreadable
  file is left alone with a note, and `--dry-run` prints the rewrite it would make. A Linux box that gained a card used to keep
  `dual-gpu` or `blackwell-2x16` in the manifest for good. `install.ps1` already rebuilt the file whole on every run, so the two
  installers now agree. On the readers' side, `doctor`'s `capacity:` row considers two tiers, the config's `tier_profile` and the
  installer's, and a tier counts only when the box's `agent_model` is its declared vLLM seat or that seat's llama.cpp fallback, so
  the `dual-gpu` manifest (which declares no vLLM seat) no longer erases the seat the config's tier owns. When both tiers count (the
  alias `agent-pool` is shared by `blackwell-16`, `blackwell-2x16` and `blackwell-3x16`) the row names the tier whose declared seat
  is the entry of the serving config that `agent_model` resolves to, and when that entry names neither the config's `tier_profile`
  wins over the manifest. `local-offload report` keeps answering the tier line from the manifest, and when the config names another
  tier it shows both ids, what the config composes, and says they disagree; so the report and the capacity row describe two views on
  purpose.
- **Why.** The two installers wrote the manifest differently (`install.ps1` rebuilds it whole on every run, `install.sh` wrote it
  once), and neither rewrites an existing `config.json`. So a box could carry an older id in the manifest next to a newer one in
  the config, and an alias several tiers share cannot tell them apart by itself; the serving config's own entry can. The node itself
  was never affected: a manifest of any recorded layout reads clean, keeps its alternate backends and accelerators, and resolves the
  same vendor, architecture and memory model for all three ids.
- **Not changed.** `config.json` is still never rewritten, so the seeds of a newer tier reach a box only through a regenerated
  config. `setup/selftest.ps1` still lets `installed.json` win in its profile reader: it is the Windows stack, where `install.ps1`
  has just rebuilt the file.
- **Tests.** Red first, each committed against the unfixed code. `setup/install.tests.sh` (run by `TestInstallShDryRunRules` on
  Linux) drives `install.sh` live against a stub binary: a manifest recorded under another tier comes back with the detected profile
  and backend and its other keys intact, an equal profile and an absent manifest behave as before, and a dry run names the rewrite
  and writes nothing. `doctor_fleetcapacity_test.go` pins the capacity row's order for a shared alias in three cases (the serving
  entry is the config tier's seat with an older manifest, the entry is the manifest tier's seat with an older config, the entry
  names neither), and the earlier cases pin the `dual-gpu` and `blackwell-2x16` manifests with the negative controls (no
  `tier_profile`, an agent seat the config tier does not own). `report_test.go` and `internal/fleetnode` pin the report line and
  that a manifest recorded as any of the three ids reads with its backends, accelerators and identity intact.
- **Docs.** `docs/OPERATOR-GUIDE.md` (the `capacity:` row and the capability report) and `docs/systems/setup-installer.md` (the
  manifest across a layout change).
