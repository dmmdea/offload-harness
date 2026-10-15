### Fixed - an installed.json recorded under an older layout no longer hides the config's tier from doctor and report

- **What it does.** A box whose `installed.json` says `dual-gpu` or `blackwell-2x16` and whose config declares
  `tier_profile` `blackwell-3x16` (a three-card box that classified as `dual-gpu` before `blackwell-3x16` existed, or a
  two-card box that gained a card) used to read as the old layout wherever the manifest wins. `doctor`'s `capacity:`
  row tries the installer's tier first and the config's `tier_profile` next, and names a tier only when the box's
  `agent_model` is that tier's declared vLLM seat or its fallback, so the `dual-gpu` manifest (which declares no vLLM
  seat) no longer erases the seat the config's tier owns. `local-offload report` keeps answering the tier line from the
  manifest, and when the config names another tier it shows both ids, what the config composes, and says they disagree.
- **Why.** The two installers write the manifest differently: `install.ps1` rebuilds it whole on every run from a fresh
  detection, `install.sh` writes it once and keeps the recorded profile, and neither rewrites an existing `config.json`.
  So a box can carry an older id in the manifest next to a newer one in the config. The node itself was never affected:
  a manifest of any recorded layout reads clean, keeps its alternate backends and accelerators, and resolves the same
  vendor, architecture and memory model for all three ids.
- **Not changed.** `install.sh` still writes `installed.json` once; the seeds of a newer tier still reach a box only
  through a regenerated `config.json`.
- **Tests.** Red first (`doctor_fleetcapacity_test.go`, `report_test.go`, committed against the unfixed code): the
  `dual-gpu` capacity row loses the tier clause and the report names only the old id; the `blackwell-2x16` case
  already worked (both tiers declare the same seat) and is pinned beside it, with the negative controls (no
  `tier_profile`, an agent seat the config tier does not own). `internal/fleetnode` pins that a manifest recorded as
  `dual-gpu`, `blackwell-2x16` or `blackwell-3x16` reads with its backends, accelerators and identity intact.
- **Docs.** `docs/OPERATOR-GUIDE.md` (the `capacity:` row and the capability report) and `docs/systems/setup-installer.md`
  (the manifest across a layout change).
