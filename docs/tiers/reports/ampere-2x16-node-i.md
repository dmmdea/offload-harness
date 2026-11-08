<!-- Capability record of the ampere-2x16 reference box <node-i> (2x NVIDIA A2 16 GB, a small-form-factor desktop,
Ubuntu, 16 GB RAM), taken 2026-11-07 after the first install: llama.cpp b12192 (sm_86, CUDA 13.4) behind
llama-swap v270, served by a systemd unit on 127.0.0.1:11436. Scrubbed: host name, user name, addresses and
install paths. This is the install's measurement record in the shape of `local-offload report`, written by hand
from that day's measurements; it is not the tool's verbatim output, and it carries only what was measured.
The box's rendered serving config was the ampere-16 (single A2) render post-processed into the two-card layout
by a script, because `install detect` classified two A2s as `dual-gpu`, which has no Linux template. The tier
this page belongs to (`ampere-2x16`) renders that layout natively and is what this record motivated. -->

# local-offload capability record

| | |
|---|---|
| host | <node-i> |
| platform | linux/amd64 |
| measured | 2026-11-07 |
| hardware | 2x NVIDIA A2 (sm_86, 15,356 MiB each, no NVLink), 16 GB RAM (ram tier `min`) |
| serving | llama.cpp b12192 (sm_86, CUDA 13.4), llama-swap v270, `ttl: 300` on every entry |
| layout | pin, don't split: every model whole on one card (CUDA_DEVICE_ORDER=PCI_BUS_ID) |
| doctor | rc 0 |
| acceptance | READY |

## Card roles and the thermal caveat

| card | slot | power | role in the measured run |
|---|---|---|---|
| 0 | CPU-lane x16 slot | 60 W | memory stack, cascade (utility) |
| 1 | chipset x4 slot | held at a 35 W floor | Qwen3.8-27B agent (heavy) |

The 60 W card was thermally limited by its chassis fan header: under load it climbed to 91-97 C and collapsed to a 210 MHz clock, because the board's embedded controller does not follow GPU heat. That is a **host airflow fault, not a tier property**. It is why the 27B ran on the slot-limited 35 W card in this run, and why the heavy card is a render-time choice (`install render --heavy-card N`, default 0) rather than a constant. **The 27B figures below are therefore a lower bound**: the card that carries it drew 46-47 W against a 35 W enforced cap, and a box with sound airflow puts the 27B on the faster 60 W card.

## Serving

Endpoint `http://127.0.0.1:11436`

| config key | alias | note |
|---|---|---|
| agent_model | qwen3.5-4b-agent | the 4B on the utility card; the 27B (`qwen38-27b-agent`) is an alternative bound by name |
| vision_model | qwen38-27b-vision | an alternative of the 27B on the heavy card; not measured on this box |
| stt_model | whisper-stt | resident on the utility card |
| forecast_model | timestral-209m | resident on the utility card |
| escalation_model, reasoning_model | unset | |

The comfy, image, video and voice scripts are unset: this box has no ComfyUI or TTS install, so the tier declares no image, video or voice lane until one is measured on it.

## Measured

| seat | card | result |
|---|---|---|
| `qwen38-27b-agent` (Qwen3.8-27B UD-IQ3_S, ctx 49152, MTP draft) | heavy (35 W card) | 8.4 tok/s generation, 42.5 tok/s prompt on a short prompt; 13,956 MiB |
| `offload-e4b` + `embeddinggemma` | utility | 4,430 MiB + 452 MiB resident concurrently with the 27B |
| `whisper-stt` (large-v3-turbo, CUDA) | utility | 1,624 MB; an 11 s clip transcribed exactly in 6.7 s wall, cold |
| `timestral-209m` (forecast) | utility | 54.5 ms model wall; 934 MiB |
| idle unload | both | every seat unloaded at `ttl: 300`; both cards back to 0 MiB, power state P8 |

Also verified: `doctor` rc 0, `acceptance` READY, a summarize call returned valid JSON, embeddings are 768-dimensional.

Native render check, same day: `install render --profile ampere-2x16 --os linux` with both card roles given as `GPU-<uuid>` was served by a throwaway llama-swap on a test port. It loaded five seats at once in 56 s: the embedder, e4b, whisper and the forecaster on the utility card, and the 27B agent on the heavy card. That placement was confirmed per process with `nvidia-smi --query-compute-apps`, at 7,822 MiB and 13,946 MiB used. The embedding was 768-dimensional, the forecaster's health endpoint answered 200, and e4b and the 27B both answered. Once the instance stopped, both cards were back at 0 MiB. `audit-yaml --against-render` reads the file as `MATCH` (byte for byte), with no pin-by-index note because the pins are by UUID.

Not measured on this box: the vision seat, the cascade running beside the 27B under sustained traffic, and anything at 32 GB of host RAM.

## Pooled seats (measured later the same day)

The tier also serves three models that span BOTH cards, for work where quality outranks a free second card. They are pinned to both cards explicitly (`CUDA_VISIBLE_DEVICES=<utility>,<heavy>`, utility first), split by layer with equal weights, and live in their own `pooled` matrix set beside the memory stack: the cascade and the single-card 27B are never co-scheduled with them. Same builds as above (llama.cpp b12192, llama-swap v270), `--split-mode layer --tensor-split 1,1 --parallel 1 --flash-attn on --cache-type-k q8_0 --cache-type-v q8_0 --cache-ram 1024 --jinja --reasoning off`, `CUDA_DEVICE_ORDER=PCI_BUS_ID`.

| seat | weights | ctx | VRAM (first card + second) | decode | prefill | note |
|---|---|---|---|---|---|---|
| `qwen38-27b-pool` | Qwen3.8-27B UD-Q6_K (21,983,677,344 B) + mmproj-F16, MTP draft depth 3 | 65536 | 12,504 + 13,594 MiB | 10.34 tok/s | 237 tok/s (3,672-token prompt) | loaded in 18 s; known-answer correct; MTP acceptance 293/465 (63 %); resident beside the embedder through llama-swap (12,964 + 13,594 MiB) |
| `gemma4-26b-a4b-pool` | gemma-4-26B-A4B-it-qat UD-Q4_K_XL (14,249,047,104 B) + mmproj-F16 | 32768 | 9,012 + 7,250 MiB | 11.92 tok/s | 234 tok/s | one card thermally throttled: a lower bound |
| `gemma4-31b-pool` | gemma-4-31B-it-qat UD-Q4_K_XL (17,287,670,048 B) + mmproj-F16 | 32768 | not recorded | 2.38 tok/s | not recorded | one card thermally throttled; **awaits a measurement on a cooled pair** |

For comparison the tier's single-card 27B (UD-IQ3_S, ctx 49152) decodes 8.4 tok/s, so the pooled 27B at the higher UD-Q6_K quant is also the faster 27B. `--split-mode tensor` on the pooled 27B decoded 10.71 tok/s but its prefill fell to 138 tok/s, with one card throttled during that run, so layer is what ships and tensor is not shipped.

Sources: the 27B and its projector are from `unsloth/Qwen3.8-27B-GGUF` (LFS sha256 of the Q6_K file `c9c206812fbe4ac7b76a729e25928b63f2ae89d37f69da7a71c20aec763cd436`); the 26B-A4B from `unsloth/gemma-4-26B-A4B-it-qat-GGUF` (model sha256 `a7c5bc715f5ff8e99a3e8901ce7d2b42b402c669bf24f7c5250747633d0f5891`, projector sha256 beginning `d00f211a7d4f7fb1`); the 31B from `unsloth/gemma-4-31B-it-qat-GGUF` (model sha256 `00b5a7c497f0c8934033088c10a7fa9a4c015e46ee6d89e9c6890650ba5d0e71`, projector sha256 beginning `4775e1bf6ef6f8df`). The three projectors are all named `mmproj-F16.gguf` upstream, so the template reads them as `mmproj-F16.gguf` (the 27B's, shared with the vision seat), `mmproj-gemma-4-26B-A4B-it-F16.gguf` and `mmproj-gemma-4-31B-it-F16.gguf`; the Linux install leaves model downloads out-of-band, and `install render` warns by name when any of these six files is missing.

The tier now seeds `agent_model` and `reasoning_model` as `qwen38-27b-pool` and `escalation_model` as `gemma4-31b-pool` (the single-card 27B and the 4B stay rendered and are still bound by name when one card is busy). The thermal caveat above applies to the pooled figures: one card was throttled during the 26B-A4B and 31B runs (and during the tensor-split run), which is why they are recorded as lower bounds.

Not measured: the pooled 27B with the reranker, whisper and the forecaster all loaded on the utility card (the run above carried the embedder only; a full resident stack takes roughly 4 GB of that card's 15,356 MiB), image input through the pooled seats' projectors, and the 31B on a cooled pair.
