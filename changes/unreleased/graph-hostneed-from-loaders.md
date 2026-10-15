### Fixed - a helper's own `--graph` declares the weight files its loaders name as its host RAM, not the largest family the box binds

- **What.** `gpu reserve -- node render/comfy-render.mjs ... --graph <file>` (and `comfy-video`) posts the caller's own workflow, so the
  lease could not size it and declared the media class default: the largest ComfyUI family the box binds. It now reads the graph.
  `hostneed.ForGraphFile` lists the weight files the graph's loaders name with the reader the node door already uses
  (`mediacap.ScanGraph`), finds each under the box's ComfyUI model roots (new `mediacap.ModelFileSizes`, the lookup of the door's
  held-by-name check, returning sizes) and declares the **sum, in full, each file once**, with the new source `graph loaders`. An
  explicit `--ram` still wins. Any doubt keeps the class default exactly as before: a graph that is not a JSON object (a UI-format
  workflow included), a file over 8 MiB or not a regular file, a graph that names no weight file, or ONE named file that is not in the
  model tree, is empty, or is not a safe relative path. A size that is not known never shrinks a declaration.
- **Why.** Measured on a 64 GiB node that binds a Wan 2.2 video pair: a 512x512 qwen-image graph that loads about 23 GiB (6.76 GiB
  checkpoint, 16.33 GiB text encoder) was declared 32.89 GiB, and the host-RAM brake held it (23.0 committed + 32.89 > 63.7 - 8.0 headroom)
  although the box could run it. With the loaders' own sum the declaration is 23.09 GiB.
- **Shape.** `internal/hostneed/graph.go` (`ForGraph`, `ForGraphFile`, `SourceGraphLoaders`), wired in `RenderCall.Estimate`'s `--graph`
  branch; `mediacap.ModelFileSizes` beside `MissingModelFilesUpTo`; `classDefaultHint` now says the lease could not size the graph's
  files (the figure it sizes prints the declaring line and no hint). The sum is not zeroed for fitting a card: where a graph places its
  weights is its own. A name held under two class directories counts as the larger. No change to the lease record, the grant or the queue.
- **Tests.** Red first (a stub commit, tests failing on their assertions): a graph naming two resolvable loader files declares their sum
  with the new source, through `ForGraph`, `ForGraphFile` and `Resolve` on a media and a text lease; one unresolved file, an empty file,
  a name that climbs out of the tree, a non-graph document, an oversized one, a graph with no loader and a box with no ComfyUI install keep
  the class default unchanged; the sum is in full and deduplicated; `--ram` wins; the incident's two files sum to 23.09 GiB against a
  32.8 GiB class default; the `gpu reserve` caller prints the declaring line with no hint for a graph it sizes and keeps the hint for one it
  cannot. Fixtures are sparse files (`os.Truncate`) of a few MiB; no test starts a process. Mutants run and killed: an unresolved name
  skipped, no-loader graphs declaring 0, no size cap, first match instead of the larger, no safe-relative check.
- **Docs.** `docs/systems/gpu-lease.md` (the "What a lease declares" table, the hint paragraph, the admission paragraph, the warning
  paragraph) and `docs/systems/media-generation.md` (Host RAM).

#### Not verified here

- No real graph was run and no ComfyUI was started; the sizes are those of sparse fixtures and of a table that stands for the measured files.
- Not wired: the pipeline's `run-graph` lane (`local-offload run-graph`) still declares the class default, because its need is computed
  in `internal/pipeline` (`classDefaultRAM`), which this change does not touch. That lane also sets no footprint key, so a measured host
  peak does not raise its declaration today.
- The declared figure is an estimate from file sizes, not a measurement of the resident memory of a run.
