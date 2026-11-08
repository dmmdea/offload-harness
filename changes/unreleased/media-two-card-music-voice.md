### Added - card-only ACE-Step music (`musicgen_placement: multicard`) and a voice worker that takes one card

- **What it does.** A node of two equal cards can now run music with every weight in VRAM. `musicgen_placement: "multicard"` (opt-in, unset by default)
  puts ACE-Step 1.5 XL turbo's DiT whole on the compute card and its two text encoders (`DualCLIPLoaderMultiGPU`, type `ace`) and VAE on the storage
  card, through a new `acestep/xl-turbo-bf16` row of `render/multicard-recipes.json` (route `music`, basis predicted, mirrored in
  `internal/mediacap/multicard.go`). `render/comfy-music.mjs` takes `--placement multicard [--cards a,b]` and makes the same plan, placement check
  and pair launch as the image and edit runners; `render/wf-acestep.mjs` builds the recipe's loaders. The door leases the two non-display cards
  (`musicNeed`), the doctor's `generate_audio:music` line names the placement, and `hostneed` sizes the route through the recipe table. The single
  placement is unchanged.
- **Voice.** With `gpu_card_scoped_leases` on and a green reader audit, the Chatterbox voice worker asks for one card instead of the whole node and is
  pinned to the card it is granted (`CUDA_VISIBLE_DEVICES=<uuid>`, `CUDA_DEVICE_ORDER=PCI_BUS_ID`), so it can run beside a render on the other card.
  With card-scoped leases off it behaves exactly as before.
- **Why.** The XL DiT, encoders and VAE are 18.5 GiB, more than one 16 GB card, so the single placement streamed from host RAM and a 16 GB host refused it.
- **Check.** `local-offload doctor` with `musicgen_placement: multicard` prints the `generate_audio:music` line with `placement=multicard` and the
  `UNETLoaderMultiGPU, DualCLIPLoaderMultiGPU, VAELoaderMultiGPU` classes found in the installed pack; `render/comfy-music.mjs --placement multicard` on
  a host that names no cards is refused before anything starts; the single-placement graph is byte-identical to the old one. No card-only music render has
  been run yet, so the recipe row is predicted and declares the ordinary host-RAM estimate until one reads the per-card peaks.
  Documented in `docs/systems/media-generation.md` ("Card-only music").
