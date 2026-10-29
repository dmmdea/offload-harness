### Fixed - the 6 GB tier's support tier no longer starves its two heavy seats, and two tiers stop seeding an image model the installer never installs

- **The defect.** On the 6 GB reference box (RTX 3050) the support tier held embeddinggemma (300M, 464 MiB),
  bge-reranker-v2-m3 (350 MiB) and embeddinggemma2 (1,216 MiB with its projector). With all three resident, both heavy
  seats failed to load: `gemma4-e4b-vision` and `qwen3.5-4b-agent` died with a cudaMalloc out of memory. Nothing used the
  300M: the memory authority and the wiki embed with embeddinggemma2, and the harness kNN pre-filter is off by default
  (`knn_prefilter_enabled` false); the 300M was only loaded by the memory-stack warm-back.
- **The fix.** A new tier flag `omit_embeddinggemma` (inverted polarity, absent = false, requires
  `include_embeddinggemma2`) makes the render drop the 300M entry, its `emb` matrix var, its evict row and its place in
  the residents set, and hand its `text-embedding` / `local-embed` aliases to embeddinggemma2 (`servingtmpl.Params.OmitEmbed300M`,
  hashed into the render stamp only when set). ampere-6 sets it and seeds `memory_stack` as
  `[embeddinggemma2, bge-reranker-v2-m3]`, so `EmbedModel()` falls to embeddinggemma2, the model a kNN index is built with
  when an operator turns the pre-filter on. Every other tier renders as before: ampere-8 and blackwell-3x16 keep the 300M
  because their recorded co-residency budgets count it and nothing there measured it removed.
- **Measured.** With the 300M gone, 4,283 MiB stayed free beside the support tier and `gemma4-e4b-vision` loads and
  answers (3,986 MiB). `qwen3.5-4b-agent` at ctx 65536 with q8_0 K and V still does not load (compute buffer 380 MiB at
  the default ubatch, 318 MiB at ubatch 256). Beside the support tier on a 29,914-token prompt, ctx 49152 loads with 216
  MiB free, ctx 57344 leaves 40 MiB, and ctx 65536 with a q4_0 V cache leaves 120 MiB but changes the cache the bake-off
  measured. 49152 with q8_0 K and V is the ceiling; the tier's own `ctx_size` (32768) is inside it and is unchanged. The
  template and the tier notes carry the figures.
- **Image seed.** ampere-6 and maxwell-3 seeded `sdcpp_model` as the SDXL-Turbo Q4_0 file, which the installer excludes on
  license grounds (non-commercial) and therefore never installs. Neither tier gets the Z-Image set the installer does fetch
  (only amd-rdna3 tiers, or `OFFLOAD_WITH_MEDIA=1`), and that set does not fit 3 GB and was not measured on the 6 GB card, so
  the seed now names no image model (`sdcpp_model`, `sdcpp_model_kind` and the SDXL-specific family, steps and cfg are
  gone); the sd.cpp binary binding and ampere-6's flux3-klein family stay, and an operator binds a model they are licensed
  to run.
- **Docs.** The tier pages (regenerated), the setup installer page and the field docs in `setup/templates/profiles.json`
  say the same.
- **Check.** `install render --profile ampere-6` on linux and windows renders no `embeddinggemma` entry, the residents
  set reads `rer & eg2` (linux) and `eg2` (windows), and the linux entry for embeddinggemma2 carries the handed-over aliases.
