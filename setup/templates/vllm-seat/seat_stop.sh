#!/usr/bin/env bash
# seat_stop.sh — llama-swap `cmdStop` for the vLLM seat: stop THIS seat's engine (matched by its port,
# never by model name — two seats may serve the same checkpoint), then the LMCache MP server, then
# clean only this stack's shared-memory segments. Exits non-zero when the port is still bound, so
# llama-swap records a failed unload instead of a clean one that leaves the next start unable to bind.
set -u
CFG="${1:-${SEAT_ENV:-/root/g7/seat.env}}"
[ -f "$CFG" ] && . "$CFG"
PORT="${SEAT_PORT:-18797}"
MP_UNIT="${SEAT_MP_UNIT:-lmcache-mp}"
PAT="vllm serve .*--port $PORT"
# The API server owns the engine as CHILD processes ("VLLM::EngineCore", "VllmWorker-N"). Killing the server
# alone leaves them alive when it dies mid-request — measured 2026-09-05: an EngineCore + worker outlived the
# unload by 8 minutes holding 1.8 / 12 / 3.5 GB on three cards with the port free, so llama-swap saw a clean
# stop and the next start waited on cards nobody was using. Capture the tree FIRST, then kill it all.
descendants() { local p; for p in $(pgrep -P "$1" 2>/dev/null); do echo "$p"; descendants "$p"; done; }
TREE=""
for api in $(pgrep -f "$PAT" 2>/dev/null); do TREE="$TREE $(descendants "$api" | tr '\n' ' ')"; done
pkill -f "$PAT" 2>/dev/null
for i in $(seq 1 20); do pgrep -f "$PAT" >/dev/null || break; sleep 1; done
pkill -9 -f "$PAT" 2>/dev/null
# Reap whatever of the tree outlived its parent, then any parentless engine process at all (ppid 1 = an orphan
# of a dead API server; nothing else spawns these names).
for p in $TREE; do kill -0 "$p" 2>/dev/null && kill -TERM "$p" 2>/dev/null; done
sleep 3
for p in $TREE; do kill -0 "$p" 2>/dev/null && kill -KILL "$p" 2>/dev/null; done
for p in $(ps -eo pid,ppid,args 2>/dev/null | awk '$2==1 && ($3 ~ /^VLLM::EngineCore/ || $3 ~ /^VllmWorker/) {print $1}'); do
  echo "seat_stop: reaping orphaned engine process $p ($(ps -o args= -p "$p" 2>/dev/null | cut -c1-40))"; kill -KILL "$p" 2>/dev/null
done
if ! systemctl stop "$MP_UNIT" 2>/dev/null; then echo "seat_stop: WARN systemctl stop $MP_UNIT failed"; fi
systemctl reset-failed "$MP_UNIT" 2>/dev/null
# Only this stack's segments — never every process's shared memory in the distro.
if ! pgrep -f 'vllm serve' >/dev/null; then
  find /dev/shm -maxdepth 1 \( -name 'nccl-*' -o -name 'vllm*' -o -name 'torch_*' -o -name 'lmcache*' -o -name 'psm_*' \) -exec rm -rf {} + 2>/dev/null
fi
if ss -ltn 2>/dev/null | grep -q ":$PORT "; then echo "seat_stop: WARN :$PORT still bound"; exit 1; fi
# VRAM readback: the stop is only real when the seat's cards are empty again. Warning, not failure — the
# holder may be another seat that legitimately shares a card; the message names it.
DEVS="${SEAT_DEVICES:-0,1}"; VFLOOR="${SEAT_VRAM_FLOOR_MIB:-1024}"; held=""
if command -v nvidia-smi >/dev/null 2>&1; then
  sleep 2
  for d in ${DEVS//,/ }; do
    used="$(CUDA_DEVICE_ORDER=PCI_BUS_ID nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits -i "$d" 2>/dev/null | head -1 | tr -d ' ')"
    if [ -n "$used" ] && [ "$used" -gt "$VFLOOR" ] 2>/dev/null; then held="$held dev$d=${used}MiB"; fi
  done
  [ -n "$held" ] && echo "seat_stop: WARN seat devices still hold VRAM after the stop:$held — holders: $(nvidia-smi --query-compute-apps=gpu_uuid,pid,process_name,used_memory --format=csv,noheader 2>/dev/null | tr '\n' ';')"
fi
echo "seat stopped"
