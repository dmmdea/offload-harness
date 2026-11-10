# local-offload capability report

| | |
|---|---|
| host | <node-j> |
| harness version | 0.213.0 |
| platform | windows/amd64 |
| generated | 2026-11-10 11:40 UTC |
| config | C:\Users\<user>\.local-offload\config.json |
| hardware tier | amd-rdna3 (backend vulkan) |
| manifest | D:\offload-stack\installed.json |

## Serving

Endpoint `http://127.0.0.1:11437` — **OK**

| config key | alias | live |
|---|---|---|
| model | offload-e4b | OK |
| agent_model | gemma-4-26b-agent | OK |
| triage_model | gemma4-e2b | OK |
| escalation_model | gemma4-26b-a4b | OK |
| reasoning_model | gemma4-26b-a4b | OK |
| vision_model | gemma4-e4b-vision | OK |
| ocr_model | — | unset |
| stt_model | whisper-stt | OK |
| stt_model_hq | — | unset |
| forecast_model | — | unset |

## Media routes

Derived from this machine's bindings, not declared. `CONFIGURED` = bound and the file exists · `NOT CONFIGURED` = no binding here, the task defers by design · `BOUND-BUT-MISSING` = the config names a file that is not there, so the task fails when called.

| route | verdict | engine | bindings |
|---|---|---|---|
| generate_image | CONFIGURED | sdcpp | sdcpp_bin=D:/offload-stack/sdcpp/sd-cli.exe; sdcpp_model=D:/offload-stack/models/z_image_turbo-Q8_0.gguf; sdcpp_script=D:\offload-stack\harness\render\sdcpp-generate.mjs |
| inpaint_image | CONFIGURED | sdcpp | sdcpp_bin=D:/offload-stack/sdcpp/sd-cli.exe; sdcpp_model=D:/offload-stack/models/z_image_turbo-Q8_0.gguf; sdcpp_vae=D:/offload-stack/models/zimage_ae.safetensors; sdcpp_llm=D:/offload-stack/models/Qwen3-4B-Instruct-2507-Q4_K_M.gguf; inpaint_sdcpp_script=D:\offload-stack\harness\render\sdcpp-inpaint.mjs; ffmpeg_path=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Packages\Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe\ffmpeg-8.1.1-full_build\bin\ffmpeg.exe; ffprobe (beside ffmpeg_path, else PATH)=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Packages\Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe\ffmpeg-8.1.1-full_build\bin\ffprobe.exe; inpaint_sdcpp_backend=vulkan0 |
| edit_image_generative | NOT CONFIGURED | comfyui | gen_edit_script/gen_edit_unet is unset |
| upscale_image | CONFIGURED | sdcpp | upscale_sdcpp_bin=D:/offload-stack/sdcpp/sd-cli.exe; upscale_sdcpp_model=D:/offload-stack/models/upscale/4xNomos2_hq_mosr.safetensors; upscale_sdcpp_script=D:\offload-stack\harness\render\sdcpp-upscale.mjs; ffmpeg_path=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Packages\Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe\ffmpeg-8.1.1-full_build\bin\ffmpeg.exe; ffprobe (beside ffmpeg_path, else PATH)=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Packages\Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe\ffmpeg-8.1.1-full_build\bin\ffprobe.exe; upscale_sdcpp_backend=vulkan0 |
| generate_video | CONFIGURED | sdcpp | license Apache-2.0; family=fastwan; sdcpp_bin=D:/offload-stack/sdcpp/sd-cli.exe; sdcpp_model=D:/offload-stack/models/wan22-ti2v-5b/FastWan2.2-TI2V-5B-q8_0.gguf; sdcpp_vae=D:/offload-stack/models/wan22-ti2v-5b/Wan2.2_VAE.safetensors; sdcpp_t5xxl=D:/offload-stack/models/wan22-ti2v-5b/umt5-xxl-encoder-Q8_0.gguf; videogen_sdcpp_script=D:\offload-stack\harness\render\sdcpp-video.mjs; ffmpeg_path=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Packages\Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe\ffmpeg-8.1.1-full_build\bin\ffmpeg.exe; ffprobe (beside ffmpeg_path, else PATH)=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Packages\Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe\ffmpeg-8.1.1-full_build\bin\ffprobe.exe; videogen_families["fastwan"].sdcpp_backend=vulkan0; optional sdcpp_tae=D:/offload-stack/models/wan22-ti2v-5b/taew2_2.safetensors (fast=true decodes with it) |
| animate_character | CONFIGURED | sdcpp | animategen_sdcpp_bin=D:/offload-stack/sdcpp/sd-cli.exe; animategen_depth_bin=D:/offload-stack/build/depth-anything.cpp/build/examples/cli/da3-cli.exe; animategen_sdcpp_model=D:/offload-stack/models/wan21-vace-1.3b/wan2.1_vace_1.3B_fp16.safetensors; animategen_sdcpp_vae=D:/offload-stack/models/wan21-vace-1.3b/wan_2.1_vae.safetensors; animategen_sdcpp_t5xxl=D:/offload-stack/models/wan22-ti2v-5b/umt5-xxl-encoder-Q8_0.gguf; animategen_depth_model=D:/offload-stack/models/depth/depth-anything2-small-q8_0.gguf; animategen_sdcpp_script=D:\offload-stack\harness\render\sdcpp-animate.mjs; ffmpeg_path=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Packages\Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe\ffmpeg-8.1.1-full_build\bin\ffmpeg.exe; ffprobe (beside ffmpeg_path, else PATH)=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Packages\Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe\ffmpeg-8.1.1-full_build\bin\ffprobe.exe; animategen_sdcpp_backend=vulkan0 |
| generate_audio:voice | CONFIGURED | audiocpp | family=chatterbox; audiocpp_bin=D:/offload-stack/build/audio.cpp/build/bin/audiocpp_cli.exe; audiocpp_voice_model=D:/offload-stack/models/chatterbox/chatterbox-q8_0.gguf; audiocpp_script=D:\offload-stack\harness\render\audiocpp-generate.mjs; ffmpeg_path=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Packages\Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe\ffmpeg-8.1.1-full_build\bin\ffmpeg.exe; ffprobe (beside ffmpeg_path, else PATH)=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Packages\Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe\ffmpeg-8.1.1-full_build\bin\ffprobe.exe; audiocpp_backend=vulkan |
| generate_audio:music | CONFIGURED | audiocpp | family=ace_step; audiocpp_bin=D:/offload-stack/build/audio.cpp/build/bin/audiocpp_cli.exe; audiocpp_music_model=D:/offload-stack/models/ace-step/ace-step-1.5-turbo-bf16.gguf; audiocpp_script=D:\offload-stack\harness\render\audiocpp-generate.mjs; ffmpeg_path=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Packages\Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe\ffmpeg-8.1.1-full_build\bin\ffmpeg.exe; ffprobe (beside ffmpeg_path, else PATH)=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Packages\Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe\ffmpeg-8.1.1-full_build\bin\ffprobe.exe; audiocpp_backend=vulkan |
| run_graph | NOT CONFIGURED | comfyui | run_graph_script is unset |
| generate_audio:voice:endpoint | NOT CONFIGURED | openai-compatible-tts | tts_endpoint is unset |
| edit_image | CONFIGURED | pil | edit_python=C:/Users/<user>/AppData/Local/Programs/Python/Python312/python.exe |
| flatten_design | NOT CONFIGURED | gimp | gimp_console_path is unset |
| media | CONFIGURED | ffmpeg | ffmpeg_path=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Packages\Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe\ffmpeg-8.1.1-full_build\bin\ffmpeg.exe |
| media:ffprobe | CONFIGURED | ffprobe | ffprobe=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Packages\Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe\ffmpeg-8.1.1-full_build\bin\ffprobe.exe |
| compose_video | CONFIGURED | hyperframes | compose_script=D:\offload-stack\harness\render\compose-hyperframes.mjs; hyperframes_dir (project-local CLI)=D:\offload-stack\hyperframes\node_modules\hyperframes\bin\hyperframes.mjs; ffmpeg_path=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Packages\Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe\ffmpeg-8.1.1-full_build\bin\ffmpeg.exe; browser=auto (D:\offload-stack\hyperframes\home\.cache\hyperframes\chrome\chrome-headless-shell\win64-152.0.7977.30\chrome-headless-shell-win64\chrome-headless-shell.exe); ffprobe=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Packages\Gyan.FFmpeg_Microsoft.Winget.Source_8wekyb3d8bbwe\ffmpeg-8.1.1-full_build\bin\ffprobe.exe; hyperframes=0.9.29; templates=callout-label,captions-bar,checklist-card,lower-third,section-title,stat-card,title-card |
| node _(prereq)_ | CONFIGURED | runtime | node_path=C:\Program Files\nodejs\node.exe — every render script runs under it |

---

Send this file back as-is. It is read-only output: no model was loaded, no GPU work was queued, and nothing on this machine changed.

## Operator measurements, 2026-11-10 (not part of the `report` output above)

Taken on the same box with the repo's own tools: `setup/selftest.ps1` with the canaries on, then `local-offload acceptance` and `acceptance --render`, each as one detached job with a RAM watcher beside it. The box is a mini-PC (Ryzen 9 7940HS, Radeon 780M, 64 GB dual-channel DDR5 with 55.8 GB visible to Windows, Windows 11, Adrenalin driver 32.0.32015.7029, llama.cpp win-vulkan b12244). It is its owner's working machine and runs scheduled jobs of the owner's; those kept their schedule through every run. The stack's llama-swap listens on `127.0.0.1:11437` with its models from port 5900, because the owner's WSL (mirrored networking) holds 11436, 5800 and 5801 for a llama-swap of its own.

### Selftest (verdict: pass)

| check | result |
|---|---|
| offload-e4b | 24.1 tok/s, cold load 17.6 s |
| gemma4-e2b | 44.7 tok/s, cold load 6.0 s |
| gemma4-26b-a4b on the iGPU (`moe_26b` `gpu`) | 23.5 tok/s, cold load 23.0 s |
| llama-bench, E4B | pp512 1087.5 tok/s, tg128 25.2 tok/s |
| depth canary | generated to 7000 tokens, no device loss |
| ctx sweep | 8K, 16K and 32K load and generate: 23.2, 23.6, 24.0 tok/s |
| fa_q8kv | pass, flash attention confirmed on, word overlap 1.0 |
| swap eviction cycle | one server left, no leak |
| whisper STT | 8.7 s for the sample |
| vision | VQA 5.9 s, OCR 1.6 s |
| sd.cpp image, VAE on the CPU (the tier default) | 68.2 s |
| sd.cpp image, VAE on the GPU (the `gpu_vae` canary) | 41.3 s, clean decode |
| cold swap | E4B 3.1 s, E2B 2.6 s, 26B 19.5 s |
| embedder canary | HTTP 500 on that run: the model's port was one the owner's WSL already held. This is the collision the port settings above remove. |

### Acceptance (every step PASS)

| step | wall | what was checked |
|---|---|---|
| summarize / classify / embed | 8.0 s / 3.4 s / 2.5 s | the gate's own known answers |
| stt | 5.8 s | the transcript of the bundled speech sample |
| vqa / ocr | 4.6 s / 1.7 s | the object and the text on the bundled sign |
| agent (gemma-4-26b-agent) | 33.2 s | the known-answer tool task |
| compose / edit / media | 17.4 s / ok / ok | a 1920x1080 clip, a PIL edit, an ffmpeg probe |
| image | 62.0 s | 512x512 PNG; a VQA on it names the object |
| inpaint | 36.0 s | 720x240 PNG |
| upscale | 4.0 s | 720x240 to 1440x480 |
| voice | 14.4 s | 3.84 s mono 24 kHz, mean -18.4 dB; its transcript carries a word of the sentence |
| music | 72.5 s | 4.63 s stereo 48 kHz, mean -15.7 dB |
| video | 74.9 s | 256x256, 17 frames (the gate's size; low fidelity at that size) |
| animate | 235.8 s | 256x256, 17 frames |

The image, the inpaint, a frame strip of both clips and both audio files were opened and looked at or measured, not only probed.

### One grounded agent task per seat

A workspace with two files; the task asks for a code that is in one and the sum of two counts that are in the other, so a correct answer needs the read tool. `gemma-4-26b-agent`: correct, 5 steps, 16.5 s of model time. `offload-e4b`: correct, 3 steps, 5.8 s. One task is not an agent contract; the tier's agent seat stays labelled projected for that reason.

### Memory and stability

Lowest available RAM over the selftest and every render: 17.5 GB. No display-driver reset in the system log over the day. Every model unloads after its 300 s idle bound.

### What this does to the tier

The tier default stays the floor (f16 KV, 16K ctx, VAE on the CPU): one box is not the class. On this box the q8_0 KV, the 32K ctx and the GPU VAE are promotions its own canaries passed; the first two are applied with `OFFLOAD_KV_TYPE` and `OFFLOAD_CTX_SIZE` at install. Production-size video and animate, remote agent work and a 32 GB box of this class are unmeasured.
