# local-offload capability report

| | |
|---|---|
| host | <node-a> |
| harness version | 0.26.0 |
| platform | windows/amd64 |
| generated | 2026-07-27 09:14 UTC |
| config | C:\Users\<user>\.local-offload\config.json |
| hardware tier | UNKNOWN — no installer manifest at that path (open C:\Users\<user>\offload-stack\installed.json: The system cannot find the path specified.) |
| manifest | C:\Users\<user>\offload-stack\installed.json |

## Serving

Endpoint `http://127.0.0.1:11436` — **OK**

| config key | alias | live |
|---|---|---|
| model | offload-e4b | OK |
| triage_model | gemma4-e2b | OK |
| escalation_model | gemma4-26b-a4b | OK |
| reasoning_model | gemma4-26b-a4b | OK |
| vision_model | — | unset |
| stt_model | whisper-stt | OK |
| stt_model_hq | — | unset |

## Media routes

Derived from this machine's bindings, not declared. `CONFIGURED` = bound and the file exists · `NOT CONFIGURED` = no binding here, the task defers by design · `BOUND-BUT-MISSING` = the config names a file that is not there, so the task fails when called.

| route | verdict | engine | bindings |
|---|---|---|---|
| generate_image | CONFIGURED | comfyui | imagegen_script=<repo-root>/render/comfy-generate.mjs |
| inpaint_image | NOT CONFIGURED | comfyui | inpaint_script/inpaint_ckpt is unset |
| generate_video | CONFIGURED | comfyui | videogen_script=<repo-root>/render/comfy-video.mjs |
| generate_audio:voice | CONFIGURED | chatterbox-tts | voicegen_script=<repo-root>/render/tts.mjs |
| generate_audio:music | CONFIGURED | acestep | musicgen_script=<repo-root>/render/comfy-music.mjs |
| run_graph | CONFIGURED | comfyui | run_graph_script=<repo-root>/render/comfy-run-graph.mjs |
| edit_image | CONFIGURED | pil | edit_python=C:\ComfyUI\.venv\Scripts\python.exe |
| flatten_design | NOT CONFIGURED | gimp | gimp_console_path is unset |
| media | CONFIGURED | ffmpeg | ffmpeg_path=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Packages\Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe\ffmpeg-8.1.1-full_build\bin\ffmpeg.exe |
| node _(prereq)_ | CONFIGURED | runtime | node_path=C:\Program Files\nodejs\node.exe — every render script runs under it |
| comfyui _(prereq)_ | CONFIGURED | runtime | comfy_dir=C:/ComfyUI |

---

Send this file back as-is. It is read-only output: no model was loaded, no GPU work was queued, and nothing on this machine changed.
