#!/usr/bin/env bash
# seat_stop.sh — llama-swap `cmdStop` for the vLLM seat: stop THIS seat's engine (matched by its port,
# never by model name — two seats may serve the same checkpoint), then the LMCache MP server, then
# clean only this stack's shared-memory segments. Exits non-zero when the port is still bound, so
# llama-swap records a failed unload instead of a clean one that leaves the next start unable to bind.
set -u
CFG="${SEAT_ENV:-/root/g7/seat.env}"
[ -f "$CFG" ] && . "$CFG"
PORT="${SEAT_PORT:-18797}"
PAT="vllm serve .*--port $PORT"
pkill -f "$PAT" 2>/dev/null
for i in $(seq 1 20); do pgrep -f "$PAT" >/dev/null || break; sleep 1; done
pkill -9 -f "$PAT" 2>/dev/null
if ! systemctl stop lmcache-mp 2>/dev/null; then echo "seat_stop: WARN systemctl stop lmcache-mp failed"; fi
systemctl reset-failed lmcache-mp 2>/dev/null
# Only this stack's segments — never every process's shared memory in the distro.
if ! pgrep -f 'vllm serve' >/dev/null; then
  find /dev/shm -maxdepth 1 \( -name 'nccl-*' -o -name 'vllm*' -o -name 'torch_*' -o -name 'lmcache*' -o -name 'psm_*' \) -exec rm -rf {} + 2>/dev/null
fi
if ss -ltn 2>/dev/null | grep -q ":$PORT "; then echo "seat_stop: WARN :$PORT still bound"; exit 1; fi
echo "seat stopped"
