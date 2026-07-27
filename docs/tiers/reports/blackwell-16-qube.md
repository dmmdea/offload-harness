# local-offload capability report

| | |
|---|---|
| host | <node-b> |
| harness version | 0.26.0 |
| platform | windows/amd64 |
| generated | 2026-07-27 09:14 UTC |
| config | <repo-root>/config.json |
| hardware tier | UNKNOWN — no installer manifest at that path (open C:\Users\<user>\offload-stack\installed.json: The system cannot find the path specified.) |
| manifest | C:\Users\<user>\offload-stack\installed.json |

## Serving

Endpoint `http://127.0.0.1:11436` — **OK**

| config key | alias | live |
|---|---|---|
| model | gemma-4-e4b | OK |
| triage_model | gemma-4-e2b | OK |
| escalation_model | gemma-4-12b | OK |
| reasoning_model | gemma-4-26b | OK |
| vision_model | qwen3-vl-8b | OK |
| stt_model | whisper-stt | OK |
| stt_model_hq | qwen3-asr | OK |

## Media routes

Derived from this machine's bindings, not declared. `CONFIGURED` = bound and the file exists · `NOT CONFIGURED` = no binding here, the task defers by design · `BOUND-BUT-MISSING` = the config names a file that is not there, so the task fails when called.

| route | verdict | engine | bindings |
|---|---|---|---|
| generate_image | CONFIGURED | comfyui | imagegen_script=<repo-root>/render/comfy-generate.mjs |
| inpaint_image | CONFIGURED | comfyui | inpaint_script=<repo-root>/render/comfy-inpaint.mjs; inpaint_ckpt=RealVisXL_V5.0_fp16.safetensors (ComfyUI model name, not checked here) |
| generate_video | CONFIGURED | comfyui | videogen_script=<repo-root>/render/comfy-video.mjs |
| generate_audio:voice | CONFIGURED | chatterbox-tts | voicegen_script=<repo-root>/render/tts.mjs |
| generate_audio:music | CONFIGURED | acestep | musicgen_script=<repo-root>/render/comfy-music.mjs |
| run_graph | CONFIGURED | comfyui | run_graph_script=<repo-root>/render/comfy-run-graph.mjs |
| edit_image | CONFIGURED | pil | edit_python=C:\ComfyUI\.venv\Scripts\python.exe |
| flatten_design | CONFIGURED | gimp | gimp_console_path=C:/Program Files/GIMP 3/bin/gimp-console-3.2.exe |
| media | CONFIGURED | ffmpeg | ffmpeg_path=C:/ComfyUI/.venv/Lib/site-packages/imageio_ffmpeg/binaries/ffmpeg-win-x86_64-v7.1.exe |
| node _(prereq)_ | CONFIGURED | runtime | node_path=C:\Program Files\nodejs\node.exe — every render script runs under it |
| comfyui _(prereq)_ | CONFIGURED | runtime | comfy_dir=C:/ComfyUI |

---

Send this file back as-is. It is read-only output: no model was loaded, no GPU work was queued, and nothing on this machine changed.
