# local-offload capability report

| | |
|---|---|
| host | lenovo-m720q |
| harness version | 0.26.0 |
| platform | linux/amd64 |
| generated | 2026-07-27 09:14 UTC |
| config | /srv/ecosystem_backup/apps/offload-stack/etc/config.json |
| hardware tier | UNKNOWN — no installer manifest at that path (open /home/anield/offload-stack/installed.json: no such file or directory) |
| manifest | /home/anield/offload-stack/installed.json |

## Serving

Endpoint `http://127.0.0.1:11436` — **OK**

| config key | alias | live |
|---|---|---|
| model | offload-e4b | OK |
| triage_model | gemma4-e2b | OK |
| escalation_model | — | unset |
| reasoning_model | — | unset |
| vision_model | gemma4-e4b-vision | OK |
| stt_model | whisper-stt | OK |
| stt_model_hq | — | unset |

## Media routes

Derived from this machine's bindings, not declared. `CONFIGURED` = bound and the file exists · `NOT CONFIGURED` = no binding here, the task defers by design · `BOUND-BUT-MISSING` = the config names a file that is not there, so the task fails when called.

| route | verdict | engine | bindings |
|---|---|---|---|
| generate_image | CONFIGURED | sdcpp | sdcpp_bin=/srv/ecosystem_backup/apps/offload-stack/build/sd.cpp/build/bin/sd-cli; sdcpp_model=/srv/ecosystem_backup/apps/offload-stack/models/sdxl-turbo/stable-diffusion-xl-1.0-turbo-Q4_0.gguf; sdcpp_script=/srv/ecosystem_backup/apps/offload-stack/src/offload-harness/render/sdcpp-generate.mjs |
| inpaint_image | NOT CONFIGURED | comfyui | inpaint_script/inpaint_ckpt is unset |
| generate_video | CONFIGURED | comfyui | videogen_script=/srv/ecosystem_backup/apps/offload-stack/src/offload-harness/render/comfy-video.mjs |
| generate_audio:voice | CONFIGURED | chatterbox-tts | voicegen_script=/srv/ecosystem_backup/apps/offload-stack/src/offload-harness/render/tts.mjs |
| generate_audio:music | CONFIGURED | acestep | musicgen_script=/srv/ecosystem_backup/apps/offload-stack/src/offload-harness/render/comfy-music.mjs |
| run_graph | CONFIGURED | comfyui | run_graph_script=/srv/ecosystem_backup/apps/offload-stack/src/offload-harness/render/comfy-run-graph.mjs |
| edit_image | CONFIGURED | pil | edit_python=/srv/ecosystem_backup/apps/comfyui/.venv/bin/python |
| flatten_design | NOT CONFIGURED | gimp | gimp_console_path is unset |
| media | CONFIGURED | ffmpeg | ffmpeg_path=/usr/bin/ffmpeg |
| node _(prereq)_ | CONFIGURED | runtime | node_path=/usr/bin/node — every render script runs under it |
| comfyui _(prereq)_ | CONFIGURED | runtime | comfy_dir=/srv/ecosystem_backup/apps/comfyui |

---

Send this file back as-is. It is read-only output: no model was loaded, no GPU work was queued, and nothing on this machine changed.
