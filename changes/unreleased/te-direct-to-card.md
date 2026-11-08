### Fixed - a text encoder bigger than one card is no longer staged in host RAM while it loads

- **What it does.** `OffloadCLIPLoaderDisTorch2` (`render/comfyui-ext/nodes/offload_multicard`) builds a plain-float encoder (bf16, fp16 or fp32, no quantization marker) on torch's `meta` device, which allocates nothing, and fills it by assignment from the read-only map (`OFFLOAD_RO_LOAD=1`), so its parameters are the file's pages and no host copy exists. DisTorch2 then places it block by block onto the two cards, as before.
- **Refusals.** It refuses before any card is touched when a tensor got no weight from the file (`OFFLOAD-TE-UNFILLED`), when a weight sits in anonymous host memory (`OFFLOAD-TE-NOMAP`), or when the stock loader took a path the node did not see (`OFFLOAD-TE-UNJUDGED`). After the placement the 64 MiB host-weight check stays.
- **Quantized encoders keep the earlier build.** The LTX-2.5 int8 convrot Gemma goes through ComfyUI's own quantized layers, which this path was not measured with, so it is still built on the CPU and placed at once.
- **Why.** The earlier build allocated the whole encoder on the CPU and copied the file into it. The 16.33 GiB Qwen3-VL-8B encoder of qwen-image-2.1 ran a 16 GB host (node-i, 14.8 GiB of RAM) out of memory there: anon-rss 13.77 GB.
- **Check.** On node-i, a card-only Qwen-Image-Edit-2511 edit loaded the bf16 Qwen2.5-VL-7B encoder (15.45 GiB file, 14.43 GiB loaded) this way:
  - filled with 0.0 MiB anonymous memory and placed in 11.0 s (6.45 GiB on one card, 7.98 GiB on the other, 0.00 in host RAM), then released in 0.8 s;
  - ComfyUI's anonymous memory peaked at 1.23 GiB for the whole job, and host free memory never fell under 11.63 GiB.
- **Check, qwen-image-2.1.** The same node loaded the 16.33 GiB Qwen3-VL-8B (the encoder that ran the host out of memory) for a 2048x2048, 40-step card-only render through the harness lane:
  - filled with 0.0 MiB anonymous memory, placed in 12.1 s (8.36 and 7.97 GiB, 0.00 in host RAM), released in 0.9 s;
  - the call took 1,456 s, ComfyUI's anonymous memory peaked at 1.43 GiB, and host free memory never fell under 11.37 GiB;
  - the turbo family (int8 convrot DiT, same encoder) took 254 s. Both images were checked by eye and by their clipped-pixel share (0.0 % and 1.0 %).
