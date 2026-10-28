### Added - the `maxwell-3` tier: a 3 GB NVIDIA card (GTX 970M-class, sm_52) gets its own band, with the E4B workhorse omitted and the E2B resident

- **What it does.** `hwdetect` and `detect.ps1` classify an NVIDIA card with less than 4 GB of VRAM (and no
  Blackwell or Volta label) as `maxwell-3` instead of letting it fall through to `ampere-6`, whose E4B workhorse
  needs 3,100 MiB at 16K and cannot load on a 3,072 MiB card. The arch label `maxwell` (GTX 900 desktop and
  mobile, 750/750 Ti/745, the 8x0M and 9x0M(X) mobile parts, Quadro M, the original TITAN X) keeps the card out
  of the "unrecognized NVIDIA" warning; the band keys on VRAM, so a 4 GB card stays `ampere-6` and an unknown
  VRAM (0) keeps the old fall-through. The tier renders the cuda template with the E2B resident, 32768 ctx, f16
  KV, flash-attn on, no 26B, no spill seat, no embeddinggemma2, one fleet worker and the research agent profile;
  the 12.4 prebuilt serves the card through its embedded sm_50 PTX (a one-time JIT the install caches).
- **`omit_e4b`.** A new profile field in the other polarity of the `include_*` family: `true` removes the
  `offload-e4b` entry, its matrix var and its set membership (new `__E4B_LEAD__` / `__E4B_AND__` tokens in every
  template, rendering their old literals by default) and drops `model-e4b` from the Windows download set.
  Absent or false renders every existing tier byte for byte as before (the render goldens did not move). The
  renderer and `Resolve-ProfileParams` refuse the flag without `resident_tier` `gemma4-e2b`, and together with
  the CPU seat family, which mirrors the workhorse. `selftest.ps1` benches the first served tier when
  `offload-e4b` is not served. The spec stamp carries the field (`omit_e4b`, omitted when false).
- **Media seats, measured on the box.** The tier declares a vision seat (`gemma4-e2b-vision`: the resident E2B
  weights plus the ggml-org Q8_0 projector at an 8192 window, reasoning off; 2,578 MiB loaded, 2,819 peak, a
  screenshot and a receipt read correctly) and an STT seat (`whisper-stt`: large-v3-turbo q5_0 with the silero
  v5.1.2 VAD and flash attention OFF, the faster path on this card: 10.4 s against 14.5 s for the same Spanish
  clip, 977 / 1,037 MiB). Both are swappable, because either one beside the chat seat overflows a 3 GB card.
- **Pins.** `model-e2b` and `model-e4b` are re-pinned to the current upstream LFS objects (unsloth re-uploaded
  both QAT GGUFs, 2,016 bytes larger with new oids); the old pins failed the size check on a fresh install. The
  three media-seat files get pin-only rows (`model-e2b-mmproj`, `model-whisper-turbo-q5`, `model-silero-v5`):
  recorded bytes and sha256, placed by the operator, warned about by `install render` when absent.
- **Check.** `detect.ps1 -SelfTest` and `go test ./internal/hwdetect/` with the new rows (970M 3 GB ->
  `maxwell-3`, 3.9 -> `maxwell-3`, 4 -> `ampere-6`, unknown VRAM -> `ampere-6`, 1050 Ti -> `other`);
  `setup/render.tests.ps1` with a `maxwell-3` block on the min and high RAM bands (no `offload-e4b` entry or var,
  the interactive set opens on `e2b`, f16, no spill); an `ampere-6` render unchanged; the design and the
  measurement plan in `docs/specs/2026-10-28-maxwell-3-tier-design.md`, the tier page in
  `docs/tiers/maxwell-3.md`. The profile's unmeasured values are marked PROJECTED in its notes until the
  reference box `<node-g>` measures them.
