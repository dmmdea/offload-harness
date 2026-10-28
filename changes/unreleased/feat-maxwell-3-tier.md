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
  weights plus the ggml-org Q8_0 projector at an 8192 window, reasoning off, an image capped at 768 tokens;
  2,578 MiB loaded, 2,819 peak on a receipt and 2,896 on a 4K screenshot under the cap, a screenshot and a
  receipt read correctly; a 512 cap misreads receipt lines, which is why 768 is the bound) and an STT seat (`whisper-stt`: large-v3-turbo q8_0 with the silero
  v5.1.2 VAD and flash attention OFF per the house Spanish-audio rule; 1,263 / 1,323 MiB, a 759 KB Spanish clip
  in 3.2 s warmed, the same transcript as q5_0 and as flash attention on). Both are swappable, because either
  one beside the chat seat overflows a 3 GB card. The image lane binds the house sd.cpp Vulkan pin with
  sdxl-turbo Q4_0 exactly as ampere-6 does: a correct 512x512 in 45 s on the card, the weights parked in host
  RAM and staged per step (1,964 MiB peak on the card).
- **Agent seat and spill, measured.** The six-arm bake on the box (G1 fit and decode, G2 ten strict tool
  calls, G3 the grounded digest-8 contract through `local-offload delegate`, G4 a Jev judge over the
  survivors) keeps the resident E2B as the agent seat: 8/8 grounded digests, every one format-clean, the
  lowest fabrication signal of the three survivors, 926 MiB free at peak, and one weight file shared with
  chat and vision, so an agent call never evicts the chat seat. LFM3-1.7B Q6_K and Q8_0 also reach 8/8
  (Q6_K leads on coverage) but need llama.cpp b11938 or later, so the global pin stays at b11490 and Q6_K
  is recorded as the runner-up for a later pin-bump PR; Qwen3.5-2B and LFM3 QAD-Q4_0 fall at G3, the 4B
  quants and MiniCPM5-2B at G1. The Qwen3.6-35B-A3B RAM spill loads cleanly (ADR 0080 gates 2 and 3)
  but decodes at 9.4-10.9 tok/s on this single-channel host, under the 10 tok/s bar, so it is not carried.
- **Pins.** `model-e2b` and `model-e4b` are re-pinned to the current upstream LFS objects (unsloth re-uploaded
  both QAT GGUFs, 2,016 bytes larger with new oids); the old pins failed the size check on a fresh install. The
  three media-seat files get pin-only rows (`model-e2b-mmproj`, `model-whisper-turbo-q8`, `model-silero-v5`):
  recorded bytes and sha256, placed by the operator, warned about by `install render` when absent.
- **Check.** `detect.ps1 -SelfTest` and `go test ./internal/hwdetect/` with the new rows (970M 3 GB ->
  `maxwell-3`, 3.9 -> `maxwell-3`, 4 -> `ampere-6`, unknown VRAM -> `ampere-6`, 1050 Ti -> `other`);
  `setup/render.tests.ps1` with a `maxwell-3` block on the min and high RAM bands (no `offload-e4b` entry or var,
  the interactive set opens on `e2b`, f16, no spill); an `ampere-6` render unchanged; the design and the
  measurement plan in `docs/specs/2026-10-28-maxwell-3-tier-design.md`, the tier page in
  `docs/tiers/maxwell-3.md`. Installed end to end on the reference box `<node-g>` on 2026-10-28: `install.ps1`
  classified it `maxwell-3` and exit 0 in 409 s, `selftest.ps1` passed (E2B 37.1 tok/s, the depth-7000 canary), a
  boot task serves llama-swap on 11436, and one real call per lane came back correct through it (summarize, vqa on
  a receipt, transcribe a Spanish clip, generate-image through sd.cpp); the scrubbed report is
  `docs/tiers/reports/maxwell-3-node-g.md`. Nothing in the profile is PROJECTED any more.
