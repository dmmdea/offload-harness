### Fixed - the sd.cpp video and animate lanes run on every build from master-1000 on

- **The gap.** From master-1000, stable-diffusion.cpp prints, right after each module's compute buffer on the device, a
  small one on the CPU for the same module (`taehv compute buffer size: 35.50 MB(VRAM) on Vulkan0`, then
  `taehv compute buffer size: 0.76 MB(RAM) on CPU`). The CPU-placement guard failed every video and animate run on such a
  build with `CPU_PLACEMENT`, although no op runs on the CPU: with `GGML_SCHED_DEBUG=2` the measured master-1024 FastWan
  run had 10 graph splits, all on Vulkan0, and the parameters on Vulkan0. A node therefore kept older builds for its video
  lanes beside a newer one for its image lanes.
- **What it does.** A CPU compute buffer is read as that host-side companion only when the same module already printed a
  compute buffer on the device in the run and the CPU one is no larger; a module whose only compute buffer is on the CPU,
  whose CPU line comes first, or whose CPU buffer is the larger still fails `CPU_PLACEMENT`. The real master-1024 log is a
  new healthy fixture (`render/testdata/sdcpp1024-video-healthy.log`). The video and animate runners pass
  `--flash-attn off,diffusion=on` instead of `--diffusion-fa`, which master-1000 deprecated, and the sd.cpp floor in
  `setup/floors.json` rises from 968 to 1000, the first build with that flag.

### Fixed - a short `fast` clip no longer decodes through the tiny autoencoder

- **The gap.** On `<node-f>` (FastWan 832x480, 3 steps, seed 42, 2026-10-28), a 9-frame clip (3 latent frames) decoded
  through `taew2_2` into blocks of false colour over the subject, on master-945 and master-1024 alike. The full VAE
  decoded the same latents clean (141.8 s against 1.6 s), and 49 frames decoded clean through the tiny one (8.1 s).
- **What it does.** The video and animate runners pass `--taesd` only from 17 frames (`TAE_MIN_FRAMES` in
  `render/igpu-engine.mjs`, the shortest clip measured clean, SSIM 0.95 against the full VAE). A shorter `fast` clip
  decodes with the full VAE, and the runner says so on stderr. The runner comments that still said "pin bump" or
  "pinned CLI" now say an engine update and the installed CLI.
