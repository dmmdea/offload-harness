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
