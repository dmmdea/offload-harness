### Changed - gpu-lease doc carries its first measured working-set swing row

- **What it does.** The per-node working-set swing table in `docs/systems/gpu-lease.md` ("How the numbers get measured") had no
  row. It now has one, `measured 2026-10-11` on the reference 3-card box with n=30, taken from the 30 existing lane timelines of
  that day. The row is labelled lane-run swing, not interactive-use swing, and the headroom stays `8 (chosen)`.
- **Why.** The doc promised a measured swing per node before the 8 GiB headroom is revisited, and the data was already on disk
  but unentered. Entering it, with its limits stated, keeps the headroom claim honest: the figure bounds what a running lane
  moves, and the interactive figure is still owed.
- **Check.** Per run, over the lane's samples: system commit less the tree's private bytes moved a median of 9.9 GiB (0.5 to
  44.9) and available RAM plus the tree's working set 5.4 GiB (0.3 to 35.7), across 9,227 samples in 30 runs.
