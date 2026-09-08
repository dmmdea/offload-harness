#!/usr/bin/env bash
# vllm-seat-run.sh — ExecStart of vllm-seat.service: environment + the MEASURED launch line (reference: Qwen3.5-4B w4a16
# on an NVIDIA A2 16 GB at 40 W, 2026-09-06 — 8/8 digests at 45 s median, pool 180k tokens at a 131,072 window, ~9.6 GB used).
# - --model is the SHORT HF snapshot path (LMCache fs_native page names embed it; the long path exceeded NAME_MAX) — never the repo id.
# - --gpu-memory-utilization is sized so the box's llama-swap small seats fit beside the engine; read the pool from the
#   "GPU KV cache size" banner at that utilization before choosing --max-model-len (the pool must hold one max-length sequence).
# - text-only (--limit-mm-per-prompt all 0) on a multimodal checkpoint drops the weights + the encoder-cache reservation.
# - the three tool flags are what the harness's agent loop needs (it sends tool_choice=auto); the parsers are per model family.
# - VLLM_USE_FLASHINFER_SAMPLER=0: the FlashInfer sampler spun at 100 % on Ampere in the dummy sampler run.
# Change the line here, then `sudo systemctl restart <unit>` (llama-swap's entry re-attaches on its next request).
set -u
S=__STACK_DIR__
export HF_HOME=__HF_HOME__ HF_HUB_OFFLINE=1 CUDA_VISIBLE_DEVICES=__DEVICE__
export VLLM_CACHE_ROOT=$S/cache/vllm
export VLLM_USE_FLASHINFER_SAMPLER=0
export PATH=__VENV_DIR__/bin:/usr/local/cuda/bin:/usr/local/bin:/usr/bin:/bin
# Boot: tailscaled being active does not mean the address is assigned yet — wait (up to 180 s) instead of failing into the
# unit's restart budget.
IP=""; n=0
until IP=$(tailscale ip -4 2>/dev/null) && [ -n "$IP" ]; do
  n=$((n+1)); [ $n -gt 60 ] && { echo "vllm-seat: no tailscale address after 180 s (tailscaled not ready)"; exit 1; }
  sleep 3
done
find "${HOME:-/home/__USER__}/.cache/flashinfer" -path "*/cached_ops/tmp/*.lock" -delete 2>/dev/null
exec vllm serve __MODEL_PATH__ \
  --host "$IP" --port __PORT__ --served-model-name __SERVED_NAMES__ \
  --max-model-len __MAX_MODEL_LEN__ --gpu-memory-utilization __GPU_UTIL__ --max-num-seqs __MAX_NUM_SEQS__ --max-num-batched-tokens __MAX_BATCHED__ \
  --enable-prefix-caching --mamba-cache-mode align --kv-cache-dtype __KV_DTYPE__ \
  --limit-mm-per-prompt '{"image":0,"video":0}' \
  --enable-auto-tool-choice --tool-call-parser __TOOL_PARSER__ --reasoning-parser __REASONING_PARSER__
