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
# The MP server unit is per seat stack (default lmcache-mp). A scratch/benchmark stack in the same box MUST use
# another unit name and port: on 2026-09-03 a benchmark arm on the seat's port and unit made llama-swap's health
# check pass against the arm, so other sessions' contracts were served by an engine without the tool-call flags,
# and every seat start stopped the arm's MP server in turn. seat_stop.sh reads the same variable.
MP_UNIT="${SEAT_MP_UNIT:-lmcache-mp}"

# Everything the engine prints must reach llama-swap's per-model log AND the seat log.
exec > >(tee -a "$LOG") 2>&1

# fs_native over a network share (measured 2026-09-04: the Lenovo tmpfs over SMB 3.1.1 recovers a 23.7k-token prefix
# in 2.6-2.9 s at fp16 and 0.80 s at fp8 KV, vs 3.8 / 0.92 s through Valkey). The share must be mounted BEFORE the
# MP server opens its base_path, and a mount that fails must stop the seat: an unmounted base_path is a local
# directory the adapter happily writes to, so the seat looks healthy and the cache server holds nothing.
#   SEAT_L2_MOUNT_SRC=//cache-server/kvcache  SEAT_L2_MOUNT_DIR=/mnt/kvcache
#   SEAT_L2_MOUNT_OPTS=credentials=/root/.smbcred,vers=3.1.1,rsize=4194304,wsize=4194304,cache=none,actimeo=1,noserverino,nobrl
# Name the store by a HOSTNAME this box resolves (tailnet MagicDNS or a static DNS name), never a DHCP address:
# on 2026-09-04 the store's LAN lease vanished after a reboot and every seat start refused here for hours. A name
# that resolves is not a path that performs — after any network change re-measure the share (the same store wrote
# at 4.6 MB/s over a Wi-Fi hop vs ~1 GB/s on its wired path). SEAT_L2_MIN_MBPS (default 0 = off) is a write floor:
# below it the seat refuses to start, because a crawling share makes the tier slower than recomputing the prefix.
if [ -n "${SEAT_L2_MOUNT_SRC:-}" ] && [ -n "${SEAT_L2_MOUNT_DIR:-}" ]; then
  mkdir -p "$SEAT_L2_MOUNT_DIR"
  if ! mountpoint -q "$SEAT_L2_MOUNT_DIR"; then
    if ! timeout 30 mount -t "${SEAT_L2_MOUNT_TYPE:-cifs}" "$SEAT_L2_MOUNT_SRC" "$SEAT_L2_MOUNT_DIR" -o "${SEAT_L2_MOUNT_OPTS:-}"; then
      # Say WHY, in the terms the operator can act on: name resolution, reachability, or the share itself.
      host="${SEAT_L2_MOUNT_SRC#//}"; host="${host%%:*}"; host="${host%%/*}"
      case "${SEAT_L2_MOUNT_TYPE:-cifs}" in nfs|nfs4) port=2049 ;; *) port=445 ;; esac
      case "$host" in
        *[!0-9.]*) resolved="$(getent hosts "$host" 2>/dev/null | awk '{print $1; exit}')" ;;   # a name: resolve it
        *)         resolved="$host" ;;                                                          # an IPv4 literal: probe it as is
      esac
      if [ -z "$resolved" ]; then
        why="'$host' does not resolve on this box (use a tailnet MagicDNS or static name, not a DHCP address)"
      elif ! timeout 3 bash -c "</dev/tcp/$resolved/$port" 2>/dev/null; then
        why="'$host' = $resolved, port $port unreachable (store down, wrong interface, or the address moved)"
      else
        why="'$host' = $resolved answers on $port — check credentials, the share name, and the store's allow-list"
      fi
      echo "seat_fg: REFUSING to start — cache-server share $SEAT_L2_MOUNT_SRC did not mount at $SEAT_L2_MOUNT_DIR: $why"
      exit 1
    fi
  fi
  echo "seat_fg: cache-server share mounted: $(df -h "$SEAT_L2_MOUNT_DIR" | tail -1)"
  MIN_MBPS="${SEAT_L2_MIN_MBPS:-0}"
  if [ "$MIN_MBPS" -gt 0 ] 2>/dev/null; then
    probe="$SEAT_L2_MOUNT_DIR/.seat_fg-write-probe.$$"
    t0=$(date +%s%N)
    if timeout 120 dd if=/dev/zero of="$probe" bs=4M count=16 conv=fsync status=none 2>/dev/null; then
      t1=$(date +%s%N); rm -f "$probe"
      mbps=$(( 67109 / ( (t1 - t0) / 1000000 + 1 ) ))   # 64 MiB = 67,108,864 bytes → decimal MB/s over elapsed ms
      if [ "$mbps" -lt "$MIN_MBPS" ]; then
        echo "seat_fg: REFUSING to start — cache-server share writes at ~${mbps} MB/s, below SEAT_L2_MIN_MBPS=${MIN_MBPS}: the tier would be slower than recompute. Fix the path, or run the same-box tier (SEAT_L2= empty, no SEAT_L2_MOUNT_* vars)"
        exit 1
      fi
      echo "seat_fg: cache-server share write probe ~${mbps} MB/s (floor ${MIN_MBPS})"
    else
      rm -f "$probe"
      echo "seat_fg: REFUSING to start — cannot write a 64 MiB probe to the cache-server share at $SEAT_L2_MOUNT_DIR within 120 s"
      exit 1
    fi
  fi
fi

# Refuse to start on a port something else already owns. vLLM would load for ~2 minutes and then die on
# "Address already in use" while llama-swap's health check passes against the FOREIGN listener — the seat is
# then "ready" and serving somebody else's engine. Fail here, at once, and name the squatter.
if ss -ltnp 2>/dev/null | grep -q ":$PORT "; then
  echo "seat_fg: REFUSING to start — :$PORT is already bound: $(ss -ltnp 2>/dev/null | grep ":$PORT " | grep -oE 'users:\(.*\)' | head -1)"
  exit 1
fi

# One MP server per engine start, with THIS start's settings (a reused unit keeps stale L1/chunk/L2).
# The store keeps its pages; only the staging buffer is rebuilt.
systemctl stop "$MP_UNIT" 2>/dev/null; systemctl reset-failed "$MP_UNIT" 2>/dev/null
L2ARG=(); [ -n "$L2" ] && L2ARG=(--l2-adapter "$L2")
if ! systemd-run --unit="$MP_UNIT" --collect --working-directory="$WORK" -p TimeoutStopSec=20 \
    -p StandardOutput=append:"$WORK/lmcache-mp.log" -p StandardError=append:"$WORK/lmcache-mp.log" \
    -E CUDA_DEVICE_ORDER=PCI_BUS_ID -E HOME=/root -E LMCACHE_DISABLE_BANNER=1 -E LMCACHE_LOG_LEVEL=INFO \
    -E PATH="$VENV/bin:/usr/local/bin:/usr/bin:/bin" \
    "$VENV/bin/lmcache" server --host 127.0.0.1 --port "$MP_PORT" --chunk-size "$CHUNK" \
      --separate-object-groups --l1-size-gb "$L1_GB" --eviction-policy LRU --supported-transfer-mode auto "${L2ARG[@]}"; then
  echo "seat_fg: $MP_UNIT failed to start (systemd-run); see $WORK/lmcache-mp.log"; exit 1
fi
for i in $(seq 1 60); do
  ss -ltn 2>/dev/null | grep -q ":$MP_PORT " && break
  systemctl is-active --quiet "$MP_UNIT" || { echo "seat_fg: $MP_UNIT died during start; see $WORK/lmcache-mp.log"; exit 1; }
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
