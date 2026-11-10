# local-offload capability report

| | |
|---|---|
| host | <node-h> |
| harness version | 0.207.1 |
| platform | windows/amd64 |
| generated | 2026-11-04 19:36 UTC |
| config | C:\Users\<user>\.local-offload\config.json |
| hardware tier | blackwell-16 (backend cuda) |
| manifest | C:\Users\<user>\offload-stack\installed.json |

## Serving

Endpoint `http://127.0.0.1:11436` — **OK**

| config key | alias | live |
|---|---|---|
| model | offload-e4b | OK |
| agent_model | gemma-4-26b-agent | OK |
| triage_model | gemma4-e2b | OK |
| escalation_model | gemma4-26b-a4b | OK |
| reasoning_model | gemma4-26b-a4b | OK |
| vision_model | qwen3-vl-8b | OK |
| ocr_model | — | unset |
| stt_model | whisper-stt | OK |
| stt_model_hq | — | unset |
| forecast_model | timestral-209m | OK |

## Media routes

Derived from this machine's bindings, not declared. `CONFIGURED` = bound and the file exists · `NOT CONFIGURED` = no binding here, the task defers by design · `BOUND-BUT-MISSING` = the config names a file that is not there, so the task fails when called.

| route | verdict | engine | bindings |
|---|---|---|---|
| generate_image | CONFIGURED | comfyui | imagegen_script=C:\Users\<user>\offload-stack\harness\render\comfy-generate.mjs |
| inpaint_image | CONFIGURED | comfyui | inpaint_script=C:\Users\<user>\offload-stack\harness\render\comfy-inpaint.mjs; inpaint_ckpt=RealVisXL_V5.0_fp16.safetensors (ComfyUI model name, not checked here) |
| edit_image_generative | CONFIGURED | comfyui | gen_edit_script=C:\Users\<user>\offload-stack\harness\render\comfy-edit.mjs; gen_edit_unet=qwen-image-edit-2511-Q5_1.gguf preset=lightning8 (ComfyUI model name, not checked here); models and custom nodes not checked (comfy_dir is not a ComfyUI install on this machine) |
| upscale_image | CONFIGURED | comfyui | upscale_script=C:\Users\<user>\offload-stack\harness\render\comfy-upscale.mjs; upscale_model=4x-UltraSharp.pth (upscale_model; ComfyUI model name, not checked here) |
| generate_video | CONFIGURED | comfyui | videogen_script=C:\Users\<user>\offload-stack\harness\render\comfy-video.mjs; family=wan22; models and custom nodes not checked (comfy_dir is not a ComfyUI install on this machine) |
| animate_character | CONFIGURED | comfyui | animategen_script=C:\Users\<user>\offload-stack\harness\render\comfy-animate.mjs; models and custom nodes not checked (comfy_dir is not a ComfyUI install on this machine) |
| generate_audio:voice | BOUND-BUT-MISSING | chatterbox-tts | voicegen_script=C:\Users\<user>\offload-stack\harness\render\tts.mjs; tts python C:\Users\<user>\AppData\Local\Programs\Python\Python314\python.exe (python on PATH — the fallback, not a TTS venv) has no chatterbox package (looked in C:\Users\<user>\AppData\Local\Programs\Python\Python314\Lib\site-packages, C:\Users\<user>\AppData\Roaming\Python\Python314\site-packages) — build the .tts-venv (render/README.md, Voice setup); Chatterbox weights are not in the Hugging Face cache (C:\Users\<user>\.cache\huggingface\hub\models--ResembleAI--chatterbox) — the first call would download them inside the render's timeout; pre-fetch them with the tts python (HF_HOME decides where) |
| generate_audio:music | CONFIGURED | acestep | musicgen_script=C:\Users\<user>\offload-stack\harness\render\comfy-music.mjs; models and custom nodes not checked (comfy_dir is not a ComfyUI install on this machine) |
| run_graph | CONFIGURED | comfyui | run_graph_script=C:\Users\<user>\offload-stack\harness\render\comfy-run-graph.mjs |
| generate_audio:voice:endpoint | NOT CONFIGURED | openai-compatible-tts | tts_endpoint is unset |
| edit_image | CONFIGURED | pil | edit_python=C:/Users/<user>/AppData/Local/Programs/Python/Python314/python.exe |
| flatten_design | NOT CONFIGURED | gimp | gimp_console_path is unset |
| media | CONFIGURED | ffmpeg | ffmpeg_path=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Links\ffmpeg.exe |
| media:ffprobe | CONFIGURED | ffprobe | ffprobe=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Links\ffprobe.exe |
| compose_video | CONFIGURED | hyperframes | compose_script=C:\Users\<user>\offload-stack\harness\render\compose-hyperframes.mjs; hyperframes_dir (project-local CLI)=C:\Users\<user>\offload-stack\hyperframes\node_modules\hyperframes\bin\hyperframes.mjs; ffmpeg_path=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Links\ffmpeg.exe; browser=auto (C:\Users\<user>\offload-stack\hyperframes\home\.cache\hyperframes\chrome\chrome-headless-shell\win64-152.0.7977.30\chrome-headless-shell-win64\chrome-headless-shell.exe); ffprobe=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Links\ffprobe.exe; hyperframes=0.9.13; templates=callout-label,captions-bar,checklist-card,lower-third,section-title,stat-card,title-card |
| node _(prereq)_ | CONFIGURED | runtime | node_path=C:\Program Files\nodejs\node.exe — every render script runs under it |
| comfyui _(prereq)_ | BOUND-BUT-MISSING | runtime | comfy_dir=C:/ComfyUI is not a directory on this machine |

### Needs attention (2)

- **generate_audio:voice** — voicegen_script=C:\Users\<user>\offload-stack\harness\render\tts.mjs; tts python C:\Users\<user>\AppData\Local\Programs\Python\Python314\python.exe (python on PATH — the fallback, not a TTS venv) has no chatterbox package (looked in C:\Users\<user>\AppData\Local\Programs\Python\Python314\Lib\site-packages, C:\Users\<user>\AppData\Roaming\Python\Python314\site-packages) — build the .tts-venv (render/README.md, Voice setup); Chatterbox weights are not in the Hugging Face cache (C:\Users\<user>\.cache\huggingface\hub\models--ResembleAI--chatterbox) — the first call would download them inside the render's timeout; pre-fetch them with the tts python (HF_HOME decides where)
- **comfyui** — comfy_dir=C:/ComfyUI is not a directory on this machine

---

Send this file back as-is. It is read-only output: no model was loaded, no GPU work was queued, and nothing on this machine changed.

## Operator measurements, 2026-11-05 (not part of the `report` output above)

Taken on the same box by hand-run jobs (llama-bench and bare llama-server probes on a scratch port), inside a GPU window that opens only while the owner's own tenants are idle. The card is the desktop's display card: the owner's dictation stack and an Ollama embed model held 4.5 GB at the start and idle-unloaded to about 1.6 GB during the run (the "desktop before" column). llama.cpp win-cuda-13.4 prebuilt b12110 (commit cd959bae9), driver CUDA 13.x.

### llama-bench (`-ngl 99 -fa 1`, 3 repeats)

| model | pp512 tok/s | pp2048 tok/s | tg128 tok/s |
|---|---|---|---|
| gemma4-e2b (2.43 GiB) | 34454 (sd 3069) | 40701 | 345.1 |
| offload-e4b (3.91 GiB) | 17694 (sd 909) | 19532 | 212.4 |

This retires the "CUDA-13 build serves Blackwell through a cuBLAS fallback, ~5.6x slower prefill" warning for sm_120: there is no fallback on this build.

### 26B-A4B (UD-Q4_K_XL) placement sweep, ctx 32768, q8_0 KV, 784-token prompt, 96 tokens generated

| placement | desktop before (MiB) | seat on the card (MiB) | free after load (MiB) | pp tok/s | tg tok/s | load s |
|---|---|---|---|---|---|---|
| `-ngl 99` (the seat as rendered before 0.207.2) | 4546 | 10997 + spill | 760 | 351 | 17.0 | 18.6 |
| `-ngl 99`, ctx 16384 | 1810 | 13627 + spill | 866 | 357 | 18.5 | 10.6 |
| `--n-cpu-moe 4` | 1546 | 13065 | 1692 | 1428 | 121.2 | 6.5 |
| `--n-cpu-moe 8` | 1553 | 11563 | 3187 | 824 | 81.2 | 6.6 |
| `--n-cpu-moe 14` | 1556 | 9109 | 5638 | 535 | 62.5 | 4.5 |

Full GPU needs roughly 14.4 GB on the card, which a display card never has free while the desktop runs (the selftest saw 139 tok/s once at a near-empty card and 20 tok/s on the next run). `--n-cpu-moe 14` is the placement the tier ships from 0.207.2: it fits with 1794 MiB to spare under the heaviest desktop observed on this box (16303 MiB card - 9109 MiB placement - 5400 MiB desktop; the margin is 2.6 GB at the 4546 MiB desktop of the first table row) and holds about 5 GB of experts in host RAM. N=8 fits only below a 4.5 GB desktop; N=4 only an empty card.

Qwen3.8-27B UD-IQ3_S at `-ngl 99`, ctx 32768, q8_0 KV: 12370 MiB on the card, 2103 free, pp 1347 tok/s, tg 59.4 tok/s, coherent with thinking. Not a tier seat on Windows (the tier's declared agent seat is the vLLM build, with the 26B agent entry as the fallback this box serves); recorded as a measured option.

### The shipped placement, live through llama-swap

Both 26B entries patched to `--n-cpu-moe 14 -ngl 999` on the box ahead of the release that renders it, llama-swap restarted while idle. One chat completion on `gemma4-26b-a4b` at the owner's desktop: 8.5 s wall including the cold load, pp 472 tok/s, tg 58.2 tok/s, 10678 of 16303 MiB used with the seat loaded (1378 MiB before), a coherent three-sentence answer.
