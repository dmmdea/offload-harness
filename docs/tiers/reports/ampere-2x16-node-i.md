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
