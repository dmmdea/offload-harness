### Fixed - the card-only render runners refuse two calls up front and say why: an unkeyed runner under a two-card lease with no `--placement multicard` (PF-4), and a canvas above the 2512 Q5_1 recipe's pixel cap (PF-3)

Two misleading or missing refusals of the render runners, found in the media session of 2026-10-15 (registers PF-3 and PF-4). Both are in `render/`; no Go code changes.

- **PF-4, the defect.** A still run under `gpu reserve --devices <a>,<b>` without `--placement multicard`, aimed at a non-default ComfyUI endpoint (`--api` or `COMFY_API`) that nothing answered on, failed as
  `COMFY-ENDPOINT-DOWN`. That line tells the operator to start ComfyUI on the port or to key the instance (`COMFY_INSTANCE`, `COMFY_CARD_UUID`), and a card key pins ONE card, so it does not say what is missing:
  only `--placement multicard` keys the pair's own instance and launches it on its own port.
- **PF-4, the fix.** `ensureComfy` (`render/comfy-lifecycle.mjs`) answers `COMFY-PLACEMENT-MISSING`, which names the two leased cards (`GPU_LEASE_DEVICES`) and `--placement multicard`, when the lease holds exactly two cards, the
  call carries no multicard launch and the endpoint it was aimed at does not answer. Nothing else moves: an endpoint that answers is reused as before, one or three leased cards and a keyed instance keep their lines, the
  default endpoint launches as before, and nothing is launched in the refused case (it never was). The refusal still comes after the single probe of the endpoint, not before it, so a ComfyUI someone started there is reused.
  A caller that matched the old text on this case matches `COMFY-PLACEMENT-MISSING` now (nothing in the harness matches either).
- **PF-3, the defect.** The `qwen-image/gguf-q5_1` row of `render/multicard-recipes.json` (the 2512 Q5_1 GGUF across two cards, basis `predicted`) ran out of memory in the sampler on its first live run, at 928x1664
  (1.54 MP, on the storage card, 56 s in). The row held any canvas, so a call at the film geometry, or at no size at all (the qwen-image default is 1328x1328, 1.76 MP), took both cards and failed late.
- **PF-3, the fix.** The row carries `maxPixels: 1472700` (1.47 MP) and `maxPixelsNote`; a new `canvasRefusal` (`render/multicard-recipes.mjs`) turns a canvas above it into a refusal that names the row, both sizes, the
  cap, that it is chosen, and the flags to change (`--width` and `--height`, or the single placement). It is enforced where the size is known: `buildRenderGraph` (`render/comfy-render.mjs`, which every path to a
  card-only render passes, one job of a `--batch` included, with the family default resolved) and, for a single `comfy-generate` call that names its canvas, `planImageMulticard` (`render/multicard-image.mjs`),
  before the lease's cards are read and before ComfyUI starts. The single placement is not held to the cap, the edit rows keep their `maxMegapixels`, and the row stays `predicted`.
- **The cap is chosen, not measured.** No canvas of this row has been measured on two cards. 1.47 MP is the largest canvas the row's evidence shows a same-size Q5_1 GGUF running on two cards (the 2511 edit, every weight on
  the storage card, 0.3 GiB free: "too thin for production"), and it sits below the canvas that failed. The OOM was on the storage card, which argues that the row's storage figure, not its activations, is what is wrong
  at that canvas. A measured arm at a larger canvas, or a re-split that fits 928x1664, replaces the cap; the existing default-size multicard call is refused until then (`--width`/`--height` below the cap).
- **Not done.** There is no refusal at the typed door (`internal/pipeline`, `internal/hostneed`): a request that names its size reaches the runner, which refuses it after the pair was leased. `render/README.md` and
  `docs/systems/media-generation.md` do not describe either refusal yet.
- **Tests (red first).** `render/comfy-unkeyed-api.test.mjs` (4 tests: the refusal names the placement and both cards; an endpoint that answers is reused; one or three cards, a keyed instance and a multicard launch are
  unchanged) and the new `render/multicard-canvas.test.mjs` (13 tests, 7 of the first 10 red on the unmodified code; the others are guards). Mutants of the fix that the tests catch: the launch exemption dropped, "exactly two" turned
  into "two or more", no pair at all, the cap check dropped from `buildRenderGraph`, from the planner and from `comfy-generate`, `<=` turned into `<`, the no-cap guard dropped, the batch given the shared canvas, and the
  non-number guard dropped. `render/multicard-builders.test.mjs` names a 1024x1024 canvas in the one test that built the multicard graph at the default size.
