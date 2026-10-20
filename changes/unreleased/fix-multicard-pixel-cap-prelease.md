### Fixed - a multicard image call above the row's canvas cap is refused before any lease

- **What it does.** The typed generate_image door now holds a card-only (`imagegen_placement: multicard`) call to the image row's canvas cap before it takes the GPU lease. The Go copy of the row carries
  `MaxPixels` (1472700, read from the `maxPixels` field of `render/multicard-recipes.json`, pinned by `TestTheMulticardRowsAreTheRunnersRecipes`). A request whose width times height is above it, a default-size
  call (1328x1328, 1.76 MP) included, is answered with a refusal that names the cap, the asked size and the way out (a smaller width and height, or the single-card placement). The cap stays labelled chosen, not measured.
  `render/README.md` and `docs/systems/media-generation.md` now state the cap and both refusals (this one and the `COMFY-PLACEMENT-MISSING` one for a call with no placement).
- **Why.** The runner already refused such a canvas, but only after the harness had taken the lease on the pair and drained the seats, so a refused call still cost the pair a drain. The size is known at the door.
- **Check.** Build and vet of `internal/mediacap` and `internal/pipeline`, and the existing multicard and lane tests of both; the lane probe's own output showed the refusal text for a 1328x1328 request. No render was run. The batch door does not judge its jobs' sizes; the runner still refuses an over-cap job.
