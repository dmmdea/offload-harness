<!-- Capability report of the maxwell-3 reference box <node-g> (GTX 970M 3 GB, Windows 11, 32 GB RAM), taken
2026-10-28 right after the install (install.ps1 at feat/maxwell-3-tier 39f3c0c7: tier classified, llama-swap
served by the boot task on 11436, the render runners placed by Step 7a). Scrubbed: host name and user path.
The agent lane is the resident E2B by design (no agent_model entry); the sd.cpp binary, the sdxl-turbo weights
and whisper-server are placed by the operator on this tier, as its page says. The hyperframes browser cache path is
elided to `<browser cache>`: its `home/.cache` segment reads as a home directory to the identity lint. -->

# local-offload capability report

| | |
|---|---|
| host | <node-g> |
| harness version | 0.203.0 |
| platform | windows/amd64 |
| generated | 2026-10-28 13:20 UTC |
| config | C:\Users\<user>\.local-offload\config.json |
| hardware tier | maxwell-3 (backend cuda) |
| manifest | D:\offload-stack\installed.json |

## Serving

Endpoint `http://127.0.0.1:11436` — **OK**

| config key | alias | live |
|---|---|---|
| model | gemma4-e2b | OK |
| agent_model | — | unset |
| triage_model | gemma4-e2b | OK |
| escalation_model | — | unset |
| reasoning_model | — | unset |
| vision_model | gemma4-e2b-vision | OK |
| ocr_model | — | unset |
| stt_model | whisper-stt | OK |
| stt_model_hq | — | unset |
| forecast_model | — | unset |

## Media routes

Derived from this machine's bindings, not declared. `CONFIGURED` = bound and the file exists · `NOT CONFIGURED` = no binding here, the task defers by design · `BOUND-BUT-MISSING` = the config names a file that is not there, so the task fails when called.

| route | verdict | engine | bindings |
|---|---|---|---|
| generate_image | CONFIGURED | sdcpp | sdcpp_bin=D:/offload-stack/sdcpp/sd-cli.exe; sdcpp_model=D:/offload-stack/models/sdxl-turbo/stable-diffusion-xl-1.0-turbo-Q4_0.gguf; sdcpp_script=D:\offload-stack\harness\render\sdcpp-generate.mjs |
| inpaint_image | NOT CONFIGURED | comfyui | inpaint_script/inpaint_ckpt is unset |
| edit_image_generative | NOT CONFIGURED | comfyui | gen_edit_script/gen_edit_unet is unset |
| upscale_image | NOT CONFIGURED | comfyui | upscale_script/upscale_model is unset (videogen_upscale_model is the fallback) |
| generate_video | NOT CONFIGURED | comfyui | videogen_script is unset |
| animate_character | NOT CONFIGURED | comfyui | animategen_script is unset |
| generate_audio:voice | NOT CONFIGURED | chatterbox-tts | voicegen_script is unset |
| generate_audio:music | NOT CONFIGURED | acestep | musicgen_script is unset |
| run_graph | NOT CONFIGURED | comfyui | run_graph_script is unset |
| generate_audio:voice:endpoint | NOT CONFIGURED | openai-compatible-tts | tts_endpoint is unset |
| edit_image | NOT CONFIGURED | pil | edit_python unset and no <comfy_dir>/.venv python found |
| flatten_design | NOT CONFIGURED | gimp | gimp_console_path is unset |
| media | CONFIGURED | ffmpeg | ffmpeg_path=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Links\ffmpeg.exe |
| media:ffprobe | CONFIGURED | ffprobe | ffprobe=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Links\ffprobe.exe |
| compose_video | CONFIGURED | hyperframes | compose_script=D:\offload-stack\harness\render\compose-hyperframes.mjs; hyperframes_dir (installed CLI)=D:\offload-stack\hyperframes\node_modules\hyperframes\bin\hyperframes.mjs; hyperframes_browser_path=D:/offload-stack/hyperframes/<browser cache>/chrome-headless-shell.exe ffmpeg_path=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Links\ffmpeg.exe; ffprobe=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Links\ffprobe.exe; hyperframes=0.8.114; templates=callout-label,captions-bar,checklist-card,lower-third,section-title,stat-card,title-card |
| node _(prereq)_ | CONFIGURED | runtime | node_path=D:\Programas\nodejs\node.exe — every render script runs under it |

---

Send this file back as-is. It is read-only output: no model was loaded, no GPU work was queued, and nothing on this machine changed.
