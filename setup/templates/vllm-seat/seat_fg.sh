#!/usr/bin/env bash
# seat_fg.sh — llama-swap `cmd` for a vLLM seat backed by LMCache MP (reference template).
#
# Runs INSIDE the serving box's Linux (WSL distro or native), in the FOREGROUND of the client llama-swap
# started: when llama-swap ends that client, the session is reaped and vLLM with it — that is the
# swap-out. The LMCache MP server (L1 staging + optional cache-server L2) is a transient systemd unit
# that outlives the engine; seat_stop.sh (llama-swap `cmdStop`) stops both.
#
# Layouts measured 2026-09-02/03 on Qwen3.8-27B INT4 (vLLM 0.28.0, LMCache 0.5.4):
#   SEAT_TP=2 (two cards, tensor parallel)  — the ONLY layout that can use a cache server (L2 store):
#     24k-token context back from the store at parity cost, all tokens; MTP possible here.
#   SEAT_PP=3 (three cards, pipeline)       — uses the same-box RAM tier only (SEAT_L1_GB sized as the
#     tier, SEAT_L2 empty): 24k back in 0.53 s (26x). A pipeline seat of a 64-layer / 16-attention model
#     cannot hold equal attention counts per stage, and LMCache's Valkey adapter then fails L2 reads.
#   fp8 KV and 262k measured infeasible on 16 GB cards for this model; MTP has no pipeline support.
# Every knob is overridden from seat.env (SEAT_* variables) without editing this file. The values
# below are placeholders for a generic box; the operator's real ones live in seat.env, next to the
# harness's config.json `kv_cache_server` block, which is what `offload_status` reports — keep them
# in agreement (the status note says so).
set -u
CFG="${1:-${SEAT_ENV:-/root/g7/seat.env}}"   # env file as $1 (wsl.exe passes no environment through), else SEAT_ENV, else the default
[ -f "$CFG" ] && . "$CFG"
LOG="${SEAT_LOG:-/root/g7/seat.log}"
MODEL="${SEAT_MODEL:-RedHatAI/Qwen3.8-27B-INT4}"
NAME="${SEAT_NAME:-qwen3.8-27b-vllm}"
PORT="${SEAT_PORT:-18797}"
MP_PORT="${SEAT_MP_PORT:-18796}"
L1_GB="${SEAT_L1_GB:-8}"
CHUNK="${SEAT_CHUNK:-784}"
# The cache server (L2) is OPT-IN: empty = same-box tier only. `${VAR-default}` (no colon) so that
# SEAT_L2= in seat.env is an explicit "off", not a fall-through to a default.
L2="${SEAT_L2-}"
VENV="${SEAT_VENV:-/root/g7/vllm-env}"
WORK="${SEAT_WORKDIR:-/root/g7}"

# Everything the engine prints must reach llama-swap's per-model log AND the seat log.
exec > >(tee -a "$LOG") 2>&1

# One MP server per engine start, with THIS start's settings (a reused unit keeps stale L1/chunk/L2).
# The store keeps its pages; only the staging buffer is rebuilt.
systemctl stop lmcache-mp 2>/dev/null; systemctl reset-failed lmcache-mp 2>/dev/null
L2ARG=(); [ -n "$L2" ] && L2ARG=(--l2-adapter "$L2")
if ! systemd-run --unit=lmcache-mp --collect --working-directory="$WORK" -p TimeoutStopSec=20 \
    -p StandardOutput=append:"$WORK/lmcache-mp.log" -p StandardError=append:"$WORK/lmcache-mp.log" \
    -E CUDA_DEVICE_ORDER=PCI_BUS_ID -E HOME=/root -E LMCACHE_DISABLE_BANNER=1 -E LMCACHE_LOG_LEVEL=INFO \
    -E PATH="$VENV/bin:/usr/local/bin:/usr/bin:/bin" \
    "$VENV/bin/lmcache" server --host 127.0.0.1 --port "$MP_PORT" --chunk-size "$CHUNK" \
      --separate-object-groups --l1-size-gb "$L1_GB" --eviction-policy LRU --supported-transfer-mode auto "${L2ARG[@]}"; then
  echo "seat_fg: lmcache-mp failed to start (systemd-run); see $WORK/lmcache-mp.log"; exit 1
fi
for i in $(seq 1 60); do
  ss -ltn 2>/dev/null | grep -q ":$MP_PORT " && break
  systemctl is-active --quiet lmcache-mp || { echo "seat_fg: lmcache-mp died during start; see $WORK/lmcache-mp.log"; exit 1; }
  sleep 1
done
ss -ltn 2>/dev/null | grep -q ":$MP_PORT " || { echo "seat_fg: lmcache-mp never bound :$MP_PORT in 60 s"; exit 1; }
if [ -n "$L2" ]; then
  echo "seat_fg: cache server (L2) ON: $L2 — verify vllm:external_prefix_cache_hits > 0 after the first eviction; a size-mismatch warning in $WORK/lmcache-mp.log means the tier is serving nothing"
else
  echo "seat_fg: same-box tier only (L1 ${L1_GB} GB), no cache server"
fi

export CUDA_DEVICE_ORDER=PCI_BUS_ID CUDA_VISIBLE_DEVICES="${SEAT_DEVICES:-0,1}" NCCL_CUMEM_ENABLE=0 NCCL_P2P_DISABLE=1 HF_HUB_OFFLINE=1
export HOME=/root HF_HOME="${SEAT_HF_HOME:-$WORK/hf}" VLLM_CACHE_ROOT="${SEAT_VLLM_CACHE:-$WORK/vllm-cache}" LMCACHE_LOG_LEVEL=INFO
export PATH="$VENV/bin:/usr/local/bin:/usr/bin:/bin"
PAR=()
if [ -n "${SEAT_PP:-}" ]; then
  export VLLM_PP_LAYER_PARTITION="${SEAT_PARTITION:-}"
  PAR=(--pipeline-parallel-size "$SEAT_PP")
else
  PAR=(--tensor-parallel-size "${SEAT_TP:-2}")
fi
EXTRA=()
[ -n "${SEAT_EXTRA_ARGS:-}" ] && read -r -a EXTRA <<< "$SEAT_EXTRA_ARGS"   # e.g. MTP on a tp2 seat
exec "$VENV/bin/vllm" serve "$MODEL" \
  --host 127.0.0.1 --port "$PORT" --served-model-name "$NAME" "${SEAT_ALIAS:-agent-pool}" \
  --max-model-len "${SEAT_MAX_LEN:-131072}" "${PAR[@]}" --gpu-memory-utilization "${SEAT_UTIL:-0.88}" \
  --max-num-seqs "${SEAT_SEQS:-32}" \
  --mamba-cache-mode align --enable-prefix-caching --max-num-batched-tokens "${SEAT_BATCHED:-1567}" \
  --kv-transfer-config "{\"kv_connector\":\"LMCacheMPConnector\",\"kv_role\":\"kv_both\",\"kv_connector_extra_config\":{\"lmcache.mp.server_urls\":\"127.0.0.1:$MP_PORT\"}}" \
  "${EXTRA[@]}"
