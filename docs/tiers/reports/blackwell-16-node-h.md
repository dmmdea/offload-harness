# local-offload capability report

| | |
|---|---|
| host | <node-h> |
| harness version | 0.215.0 |
| platform | windows/amd64 |
| generated | 2026-11-10 13:04 UTC |
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
| edit_image_generative | CONFIGURED | comfyui | gen_edit_script=C:\Users\<user>\offload-stack\harness\render\comfy-edit.mjs; gen_edit_unet=qwen-image-edit-2511-Q5_1.gguf preset=lightning8 (ComfyUI model name, not checked here); nodes UnetLoaderGGUF (custom_nodes on disk) |
| upscale_image | CONFIGURED | comfyui | upscale_script=C:\Users\<user>\offload-stack\harness\render\comfy-upscale.mjs; upscale_model=4x-UltraSharp.pth (upscale_model; ComfyUI model name, not checked here) |
| generate_video | CONFIGURED | comfyui | videogen_script=C:\Users\<user>\offload-stack\harness\render\comfy-video.mjs; family=wan22; nodes UnetLoaderGGUFDisTorch2MultiGPU,VHS_VideoCombine (custom_nodes on disk); models Wan2.2-I2V-A14B-HighNoise-Q8_0.gguf, Wan2.2-I2V-A14B-LowNoise-Q8_0.gguf, umt5_xxl_fp16.safetensors, wan_2.1_vae.safetensors; not available per request: fast mode high lora (builder default)=Wan_2_2_I2V_A14B_HIGH_lightx2v_4step_lora_260412_rank_64_fp16.safetensors is under no class directory of D:\ComfyUI\models (expected loras); not available per request: fast mode low lora (builder default)=Wan_2_2_I2V_A14B_LOW_lightx2v_4step_lora_260412_rank_64_fp16.safetensors is under no class directory of D:\ComfyUI\models (expected loras) |
| animate_character | CONFIGURED | comfyui | animategen_script=C:\Users\<user>\offload-stack\harness\render\comfy-animate.mjs; models wan_animate_2_distill_int8_convrot.safetensors, umt5_xxl_fp8_e4m3fn_scaled.safetensors, clip_vision_h.safetensors, wan_2.1_vae.safetensors |
| generate_audio:voice | CONFIGURED | chatterbox-tts | voicegen_script=C:\Users\<user>\offload-stack\harness\render\tts.mjs; tts python=C:\Users\<user>\offload-stack\harness\.tts-venv\Scripts\python.exe (.tts-venv) with chatterbox + torch; weights cached in C:\Users\<user>\.cache\huggingface\hub\models--ResembleAI--chatterbox |
| generate_audio:music | CONFIGURED | acestep | musicgen_script=C:\Users\<user>\offload-stack\harness\render\comfy-music.mjs; models acestep_v1.5_xl_turbo_bf16.safetensors, qwen_0.6b_ace15.safetensors, qwen_4b_ace15.safetensors, ace_1.5_vae.safetensors |
| run_graph | CONFIGURED | comfyui | run_graph_script=C:\Users\<user>\offload-stack\harness\render\comfy-run-graph.mjs |
| generate_audio:voice:endpoint | NOT CONFIGURED | openai-compatible-tts | tts_endpoint is unset |
| edit_image | CONFIGURED | pil | edit_python=C:/Users/<user>/AppData/Local/Programs/Python/Python314/python.exe |
| flatten_design | NOT CONFIGURED | gimp | gimp_console_path is unset |
| media | CONFIGURED | ffmpeg | ffmpeg_path=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Links\ffmpeg.exe |
| media:ffprobe | CONFIGURED | ffprobe | ffprobe=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Links\ffprobe.exe |
| compose_video | CONFIGURED | hyperframes | compose_script=C:\Users\<user>\offload-stack\harness\render\compose-hyperframes.mjs; hyperframes_dir (project-local CLI)=C:\Users\<user>\offload-stack\hyperframes\node_modules\hyperframes\bin\hyperframes.mjs; ffmpeg_path=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Links\ffmpeg.exe; browser=auto (C:\Users\<user>\offload-stack\hyperframes\home\.cache\hyperframes\chrome\chrome-headless-shell\win64-152.0.7977.30\chrome-headless-shell-win64\chrome-headless-shell.exe); ffprobe=C:\Users\<user>\AppData\Local\Microsoft\WinGet\Links\ffprobe.exe; hyperframes=0.9.29; templates=callout-label,captions-bar,checklist-card,lower-third,section-title,stat-card,title-card |
| node _(prereq)_ | CONFIGURED | runtime | node_path=C:\Program Files\nodejs\node.exe — every render script runs under it |
| comfyui _(prereq)_ | CONFIGURED | runtime | comfy_dir=D:/ComfyUI; instance harness on http://127.0.0.1:8189 |

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

## Operator measurements, 2026-11-10 (not part of the `report` output above)

Taken on the same box after it was brought from 0.207.2 to 0.215.0 and given what the first report said it lacked: a ComfyUI of the harness's own (a second install on a data drive, launched as the named instance `harness` on port 8189 through `comfy_instance`, so the owner's ComfyUI on 8188 is never contacted), the media weights, the TTS venv and the forecast venv. Every job ran inside the GPU window that opens only while the owner's own tenants are idle, and the owner's open MCP sessions stayed connected across the upgrade. Each output was opened or measured, not only checked by the gate. The `report` output at the top of this file is from the box in this state.

### Selftest, canaries on (verdict: pass)

| check | result |
|---|---|
| E4B | cold load 3.36 s, 187.0 tok/s |
| E2B | cold load 2.82 s, 279.2 tok/s |
| 26B at `--n-cpu-moe 14 -ngl 999` | cold load 7.1 s, 50.1 tok/s |
| cold swap, per tier | 2.57 s / 2.06 s / 5.64 s |
| depth canary | 160 tokens generated at depth ~7000, no device loss |
| served ctx | 32768 measured; agent micro-bench 97.8 tok/s warm, well-formed edit |
| `fa_q8kv` | pass, word overlap 1, flash attention on |
| `ctx_sweep` (26B, q8_0 KV) | 8K / 16K / 32K load and generate at 50.8 / 47.6 / 46.7 tok/s |
| `bench` (26B) | pp512 1131.8 tok/s, tg128 67.6 tok/s |
| embedder | related 0.7914 against unrelated 0.3482 |
| whisper | 5.6 s, transcript has the expected words |
| vision | VQA 10.2 s (names the shape), OCR 0.3 s (reads the sign) |
| sd.cpp media leg | skipped: this tier renders through ComfyUI |

The run before it, on the same day, reported `fa_q8kv` as a divergence with word overlap 0 while the live q8_0 seat answered the same prompt correctly. That was the canary reading an empty text as a divergence, fixed in 0.215.0; the table is the run with the fix. A third run later the same day, taken straight after four ComfyUI launches while the owner's own sessions were using the card, read the three per-tier chat rows low (E4B 11.5 tok/s, E2B 118.9, 26B 27.7; 26B cold swap 20.3 s) and every canary after them at the values above (`ctx_sweep` 51.0 / 50.8 / 50.9 tok/s, `bench` tg128 65.0). The cause of the low rows was not isolated. On a display card that its owner is using, one selftest run is a sample, not a rating.

### Acceptance and render gates (every bound route produced a real output)

| route | result | what was looked at |
|---|---|---|
| summarize / classify / embed | 3.2 s / 3.0 s / 1.3 s | names "lighthouse"; label `documentation`; one vector of 768 |
| vision / stt / agent | VQA 10.0 s, OCR 0.3 s / 5.4 s / 14.2 s | names the shape, reads the sign; transcript has the expected words; answer contains 391 |
| compose / media / edit | 16.9 s / 0.1 s / 0.1 s | a 2.0 s video stream; a 20 kB JPEG; the sign resized to 128x43 |
| forecast | returned | four medians with 0.1 / 0.5 / 0.9 quantiles for a 12-point series |
| voice | 95.3 s (first load) | a 2.9 s clip; transcribed back, it carries a word of the sentence |
| image (HiDream-O1 bf16) | 267.6 s first load, 199.6 s | 512x512, a red bicycle against a blue wall, clean decode; VQA names "bicycle" |
| inpaint | 142.3 s first load, 106.8 s | 720x240, the masked band repainted as a green panel, the rest of the sign untouched |
| upscale | 4.7 s, 4.6 s | 1440x480, the sign's text sharp |
| music (ACE-Step) | 376.7 s first load, 271.3 s | 5.0 s stereo at 48 kHz, mean -17.5 dB, peak -1.0 dB, no silent stretch |
| generative edit (qwen-image-edit Q5_1, lightning8) | 71.3 s | 1680x560, the circle turned green, the text intact; 8 steps at 3.5 s/it |
| run-graph | 30.5 s | a 64x64 PNG from a posted graph |
| video (Wan 2.2 I2V Q8) | 750.1 s | 256x256, 17 frames at 16 fps, 10 steps at 1.3 s/it; frame strip shows motion, text intact |
| animate (Wan-Animate-2) | 264.6 s | 256x256, 17 frames; the gate's reference is a sign, not a character, so the strip shows the sign with drifting overlays |

Where two times are given the first includes the first-ever load of the weights from disk and was taken at the runners' default VRAM reserve; the second is a later run at the reserve the next section arrives at. The pictures of the two runs are the same. The gate sizes are the gate's own (small), so these are proofs that each route works, not throughput figures.

### The display card and ComfyUI's VRAM reserve

The card drives the owner's desktop, which held 2.5 to 5.4 GB of it during these runs. The runners launch ComfyUI with `--reserve-vram 1.0`. With that reserve the generative edit loaded partially (12.7 GB on the card, 2.1 GB offloaded), the card sat at 15.0 of 16.3 GB, the driver spilled into shared memory and sampling ran at 195 to 240 s/it. On a fresh instance launched with `--reserve-vram 3.0` the same call sampled at 3.5 s/it (71 s end to end, 14.5 GB dedicated at the peak, shared GPU memory flat while sampling).

The box now carries `"comfy_extra_args": "--reserve-vram 3.0"` in its config. Gated with it: generative edit, run-graph, video, animate, and a second pass of image, inpaint, upscale and music. Peak card use across the ComfyUI lanes was 14.7 GB. Two reserve values were tried (1.0 and 3.0), not a sweep.

A reserve set this way exposed a defect in how a kept ComfyUI is reused. The instance stays up for 300 s after a job so the next call skips the cold start, and a call that needs another launch shape restarts it. The comparison read the reserve the instance was launched with (3.0, from the extra args) against the reserve the call itself asked for (the runners' 1.0), so every separate call found "another shape". Two upscale calls back to back on 0.215.0: 14 s and 16 s, and the process on 8189 changed between them. The same two calls through the fixed runner: 12 s and 3 s on one process. The fix ships in the release after 0.216.0.

### What this does to the tier

- `blackwell-16` seeds `comfy_extra_args: "--reserve-vram 3.0"` on a fresh install (the tier is sized for a display card). An installed box keeps its config and sets the key by hand.
- The 26B placement (`--n-cpu-moe 14`), q8_0 KV and ctx 32768 that the tier ships are confirmed again by this day's selftest canaries.
