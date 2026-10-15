### Fixed - a graph's host-RAM need is the weight files its own loaders name, for a helper's `--graph` and for the `run-graph` lane, not the largest family the box binds

- **What.** `gpu reserve -- node render/comfy-render.mjs ... --graph <file>` (and `comfy-video`) and the pipeline's `run-graph` lane
  (`local-offload run-graph --graph`) posted the caller's own workflow, so the lease could not size it and declared the media class
  default: the largest ComfyUI family the box binds. They now read the graph. `hostneed.ForGraphFile` lists the weight files the graph's
  loaders name with the reader the node door already uses (`mediacap.ScanGraph`), finds each under the box's ComfyUI model roots (new
  `mediacap.ModelFileSizes`, the lookup of the door's held-by-name check, returning sizes) and sizes the set by **the family estimate's own
  rule**, now one function both use (`hostneed.fitToCard`): the **sum, each file once, in full when together the files do not fit the card**
  (the largest card of the lease, less the runner's reserve; a card that is not known counts everything), **0 when they fit**, with the same
  wording and the new source `graph loaders`. The lane's closure is `pipeline.graphRAM`, used by the real admission (`runRunGraph`) and by
  the lane probe (`graphLaneFree`). An explicit `--ram` still wins. Any doubt keeps the class default exactly as before: a graph that is not
  a JSON object (a UI-format workflow included), a file over 8 MiB or not a regular file, a graph that names no weight file, or ONE named
  file that is not in the model tree, is empty, or is not a safe relative path. A size that is not known never shrinks a declaration.
- **Why.** Measured on a 64 GiB node that binds a Wan 2.2 video pair: a 512x512 qwen-image graph that loads about 23 GiB (6.76 GiB
  checkpoint, 16.33 GiB text encoder) was declared 32.89 GiB, and the host-RAM brake held it ("waiting for host RAM: needs 32.9 GiB";
  23.0 committed + 32.89 > 63.7 - 8.0 headroom) although the box could run it. With the loaders' own sum the declaration is 23.09 GiB.
- **Shape.** `internal/hostneed/graph.go` (`ForGraph`, `ForGraphFile`, `SourceGraphLoaders`), wired in `RenderCall.Estimate`'s `--graph`
  branch; `fitToCard` factored out of `estimateWith` (no change to a family's figure); `mediacap.ModelFileSizes` beside
  `MissingModelFilesUpTo` (a name held under two class directories counts as the larger; an empty file is unresolved);
  `pipeline.graphRAM` replaces `classDefaultRAM` at the two graph call sites (`runRunGraph`, `graphLaneFree`) and nowhere else;
  `classDefaultHint` says the lease could not size the graph's files. No change to the lease record, the grant or the queue.
- **Tests.** Red first (two stub commits, tests failing on their assertions): a graph naming two resolvable loader files declares their
  sum with the new source, through `ForGraph`, `ForGraphFile` and `Resolve` on a media and a text lease; the same files declare 0 on a card
  they fit and their sum on one they do not, at the family estimate's edge (a set equal to the usable VRAM fits), and the graph and the
  family estimate agree on the same files at every card size; one unresolved file, an empty file, a name that climbs out of the tree, a
  non-graph document, an oversized one, a graph with no loader and a box with no ComfyUI install keep the class default unchanged; `--ram`
  wins; the incident's two files sum to 23.09 GiB against a 33.1 GiB class default. In the pipeline, the graph lane declares those 23.09 GiB
  (not the class default) through `declaredRAM`, declares 0 on a 32 GiB card, keeps the class default for a file the box does not hold or
  a graph that is not there, the real `runRunGraph` path admits the incident's graph on a host that cannot take the class default and holds one it cannot size, and two new cases of the lane probe's differential test against the real wait-0 admission (a host with room
  for the graph's files and not for the class default is served; the same host with a graph naming a missing file is refused). Fixtures
  are sparse files (`os.Truncate`) of a few MiB, or a size table for the incident's GiB figures; no test starts a process. Mutants run and
  killed: an unresolved name skipped, no-loader graphs declaring 0, no size cap, first match instead of the larger, no safe-relative check.
- **Docs.** `docs/systems/gpu-lease.md` (the "What a lease declares" table, the hint paragraph, the admission paragraph, the warning
  paragraph) and `docs/systems/media-generation.md` (Host RAM, the graph lane question).

#### Not verified here

- No real graph was run and no ComfyUI was started; the sizes are those of sparse fixtures and of a table that stands for the measured files.
- Known gap, left as it is: a measured host peak does not raise a run-graph declaration, today or after this change. The lane sets no
  footprint key (`withFootprint`), so `calibratedRAM` returns it unchanged; no key was added here.
- The declared figure is an estimate from file sizes, not a measurement of the resident memory of a run.
