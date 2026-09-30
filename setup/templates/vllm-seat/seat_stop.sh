#!/usr/bin/env bash
# seat_stop.sh — llama-swap `cmdStop` for the vLLM seat: stop THIS seat's engine (matched by its port,
# never by model name — two seats may serve the same checkpoint), then the LMCache MP server, then
# clean only this stack's shared-memory segments. Exits non-zero when the port is still bound, so
# llama-swap records a failed unload instead of a clean one that leaves the next start unable to bind.
#
# It is also the seat's CRASH cleanup. When the engine dies, nothing runs a stop: the API server exits, but the engine
# workers it could not stop and this stack's LMCache MP server outlive it (2026-09-29: three workers survived SIGTERM,
# the MP server kept its HTTP port, and every restart refused for 23 minutes). seat_fg.sh runs this script once at the
# start of a generation whose predecessor died, and the Windows stub's crash exit runs it through the stop task. It reaps
# only what is provably this seat's own: an engine process is an orphan only when NO ancestor is a live `vllm serve` (a
# live engine's workers are never touched), and the MP server is matched by THIS stack's MP port and unit, never by
# whatever holds a port — a foreign listener is left alone, and seat_fg.sh refuses to start on it.
# SEAT_REAP_WAIT_SEC (default 10) is how long a reaped process gets to exit before it is reported as stuck, and how long
# an MP server gets to obey SIGTERM before SIGKILL.
set -u
CFG="${1:-${SEAT_ENV:-/root/g7/seat.env}}"
[ -f "$CFG" ] && . "$CFG"
PORT="${SEAT_PORT:-18797}"
MP_UNIT="${SEAT_MP_UNIT:-lmcache-mp}"
MP_PORT="${SEAT_MP_PORT:-18796}"
WAIT="${SEAT_REAP_WAIT_SEC:-10}"; case "$WAIT" in ''|*[!0-9]*) WAIT=10 ;; esac
PAT="vllm serve .*--port $PORT"
# The API server owns the engine as CHILD processes ("VLLM::EngineCore", "VLLM::Worker_TP0" …). Killing the server
# alone leaves them alive when it dies mid-request — measured 2026-09-05: an EngineCore + worker outlived the
# unload by 8 minutes holding 1.8 / 12 / 3.5 GB on three cards with the port free, so llama-swap saw a clean
# stop and the next start waited on cards nobody was using. Capture the tree FIRST, then kill it all.
descendants() { local p; for p in $(pgrep -P "$1" 2>/dev/null); do echo "$p"; descendants "$p"; done; }
TREE=""
for api in $(pgrep -f "$PAT" 2>/dev/null); do TREE="$TREE $(descendants "$api" | tr '\n' ' ')"; done
pkill -f "$PAT" 2>/dev/null
for i in $(seq 1 20); do pgrep -f "$PAT" >/dev/null || break; sleep 1; done
pkill -9 -f "$PAT" 2>/dev/null
# Reap whatever of the tree outlived its parent (the grace only when there is a tree: a crash cleanup has none).
for p in $TREE; do kill -0 "$p" 2>/dev/null && kill -TERM "$p" 2>/dev/null; done
[ -n "${TREE//[[:space:]]/}" ] && sleep 3
for p in $TREE; do kill -0 "$p" 2>/dev/null && kill -KILL "$p" 2>/dev/null; done
# Then any engine process with no live `vllm serve` ancestor (nothing else spawns the VLLM:: names). An engine process
# is an orphan when NO ancestor is a live `vllm serve` — it is reparented to whatever held the wrapper's stdout (a tee
# subshell, pid 381 on 2026-09-05), or to the WSL session's init, not to pid 1, so a ppid==1 test misses it. Walk up.
has_api_ancestor() { local q="$1" n=0; while [ "$q" -gt 1 ] 2>/dev/null && [ $n -lt 32 ]; do
  if ps -o args= -p "$q" 2>/dev/null | grep -q "vllm serve"; then return 0; fi
  q=$(ps -o ppid= -p "$q" 2>/dev/null | tr -d ' '); n=$((n+1)); [ -z "$q" ] && break; done; return 1; }
# A killed process that is still listed as a zombie has released its memory: only a live entry counts as stuck.
alive() { kill -0 "$1" 2>/dev/null && [ "$(ps -o stat= -p "$1" 2>/dev/null | cut -c1)" != Z ]; }
REAPED=""; STUCK=""
for p in $(ps -eo pid,args 2>/dev/null | awk '$2 ~ /^VLLM::/ {print $1}'); do
  if ! has_api_ancestor "$p"; then
    echo "seat_stop: reaping orphaned engine process $p ($(ps -o args= -p "$p" 2>/dev/null | cut -c1-40))"; kill -KILL "$p" 2>/dev/null
    REAPED="$REAPED $p"
  fi
done
# SIGKILL is the end of what a signal can do: a process stuck in the GPU driver (state D) survives it, keeps its VRAM and
# fails the next start. Wait for the reaped processes to go and name any that stay, so the cause is in the log.
if [ -n "$REAPED" ]; then
  for _ in $(seq 1 $(( WAIT * 2 ))); do
    STUCK=""; for p in $REAPED; do alive "$p" && STUCK="$STUCK $p"; done
    [ -z "$STUCK" ] && break; sleep 0.5
  done
  if [ -n "$STUCK" ]; then
    echo "seat_stop: WARN orphaned engine process(es) still alive ${WAIT} s after SIGKILL:$(for p in $STUCK; do printf ' %s(state %s)' "$p" "$(ps -o stat= -p "$p" 2>/dev/null | tr -d ' ')"; done) — a process in state D is stuck in the GPU driver and keeps its VRAM until it exits"
  else
    echo "seat_stop: orphaned engine process(es) gone:$REAPED"
  fi
fi
# The MP server: its unit first; a unit that is already gone is not a failure (a crash cleanup finds none).
if ! systemctl stop "$MP_UNIT" 2>/dev/null && systemctl is-active --quiet "$MP_UNIT" 2>/dev/null; then echo "seat_stop: WARN systemctl stop $MP_UNIT failed"; fi
systemctl reset-failed "$MP_UNIT" 2>/dev/null
# An MP server that outlived its unit (or never had one) still holds this stack's L1 staging RAM and its ports —
# matched by THIS stack's MP port, never by name alone (a benchmark stack runs its own on another port) and never by
# whatever holds a port. SIGTERM first; one that ignores it (stuck in its own shutdown) gets SIGKILL after the grace, or
# it keeps the MP HTTP port and every restart refuses on it.
MP_PAT="lmcache server .*--port $MP_PORT( |\$)"
for p in $(pgrep -f "$MP_PAT" 2>/dev/null); do
  echo "seat_stop: reaping MP server $p outside its unit (port $MP_PORT)"; kill -TERM "$p" 2>/dev/null
done
for _ in $(seq 1 $(( WAIT * 2 ))); do pgrep -f "$MP_PAT" >/dev/null 2>&1 || break; sleep 0.5; done
MP_KILLED=""; MP_STUCK=""
for p in $(pgrep -f "$MP_PAT" 2>/dev/null); do
  echo "seat_stop: MP server $p ignored SIGTERM for ${WAIT} s; SIGKILL"; kill -KILL "$p" 2>/dev/null; MP_KILLED="$MP_KILLED $p"
done
if [ -n "$MP_KILLED" ]; then
  for _ in $(seq 1 $(( WAIT * 2 ))); do
    MP_STUCK=""; for p in $MP_KILLED; do alive "$p" && MP_STUCK="$MP_STUCK $p"; done
    [ -z "$MP_STUCK" ] && break; sleep 0.5
  done
  [ -n "$MP_STUCK" ] && echo "seat_stop: WARN MP server still alive ${WAIT} s after SIGKILL:$(for p in $MP_STUCK; do printf ' %s(state %s)' "$p" "$(ps -o stat= -p "$p" 2>/dev/null | tr -d ' ')"; done)"
fi
# --- reaping ends here: seat_fg.stale-mp.tests.sh runs everything above this line against stand-in processes; the rest touches the box ---
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
    vf_var="SEAT_VRAM_FLOOR_MIB_$d"; vf="${!vf_var:-$VFLOOR}"   # per-device floor (seat_fg.sh): a display card keeps the desktop
    used="$(CUDA_DEVICE_ORDER=PCI_BUS_ID nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits -i "$d" 2>/dev/null | head -1 | tr -d ' ')"
    if [ -n "$used" ] && [ "$used" -gt "$vf" ] 2>/dev/null; then held="$held dev$d=${used}MiB>${vf}"; fi
  done
  [ -n "$held" ] && echo "seat_stop: WARN seat devices still hold VRAM after the stop:$held — holders: $(nvidia-smi --query-compute-apps=gpu_uuid,pid,process_name,used_memory --format=csv,noheader 2>/dev/null | tr '\n' ';')"
fi
# --- exit status: seat_fg.stale-mp.tests.sh runs from here to the end as well ---
# A process that survived SIGKILL (or an MP server that did) is an incomplete stop: say so and fail, like a bound port.
if [ -n "$STUCK" ] || [ -n "$MP_STUCK" ]; then echo "seat_stop: INCOMPLETE — processes of this seat survived SIGKILL:${STUCK}${MP_STUCK}"; exit 1; fi
echo "seat stopped"
