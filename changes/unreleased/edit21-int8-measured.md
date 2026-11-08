### Changed - the qwen-image-2.1 int8 convrot edit is measured card-only, so a 16 GB host can run it through the typed door

- **What it does.** The `qwen-image-2.1-edit/int8_convrot` row of `render/multicard-recipes.json` and `internal/mediacap/multicard.go` is now `measured`, with the peaks of a live run: 12.9 GiB on the compute card and 7.5 GiB on the storage card, at the DiT-sequence cap.
  - Admission therefore declares no host RAM for that edit, as it does for every measured row.
  - A predicted row declares the weight files' sum. That is more than a 16 GB host has, so the edit used to be refused at the door.
  - The docs table also corrects the bf16 edit row's cell, which still read 14.4 / predicted although the row has been measured at 14.5 since 2026-10-20.
- **Check.** On node-i (two A2 16 GB, 14.8 GiB of host RAM), compute-first, under a reservation of both cards with 0 GiB host RAM declared: the turbo int8 DiT, the bf16 Qwen3-VL-8B encoder and one 1024x1024 source.
  - **At `--resolution 1440`** (4.15 MP, just under the cap): the encoder phase peaked at 12.82 GiB (compute) and 7.47 GiB (storage), the sampler at 8.76 GiB, the decode at 3.29 GiB. The prompt took 227.6 s.
  - **At `--resolution 1024`:** the peaks were 12.41 and 6.63 GiB, and the prompt took 88.8 s.
  - **Host memory:** the encoder was filled from the read-only map with 0.0 MiB anonymous memory, ComfyUI's anonymous memory peaked at 1.60 GiB, and host free memory never fell under 11.24 GiB.
  - **Output:** both edits were the asked edit, and none of their pixels were clipped at 255.
