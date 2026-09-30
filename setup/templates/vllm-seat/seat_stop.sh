#!/usr/bin/env bash
# seat_stop.sh — llama-swap `cmdStop` for the vLLM seat: stop THIS seat's engine (matched by its port,
# never by model name — two seats may serve the same checkpoint), then the LMCache MP server, then
# clean only this stack's shared-memory segments. Exits 1 when the engine port is still bound or a process of the seat
# survived SIGKILL, so a caller that reads the exit code sees a stop that did not finish, not a clean one that leaves the next
# start unable to bind: llama-swap for a direct cmdStop, and on the Windows/WSL shape the stub, which reads the stop task's
# last result after a crash cleanup (seat-cmdstop.ps1 judges its own stop by the port alone).
#
# It is also the seat's CRASH cleanup. When the engine dies, nothing runs a stop: the API server exits, but the engine
# workers it could not stop and this stack's LMCache MP server outlive it (2026-09-29: three workers survived SIGTERM,
# the MP server kept its HTTP port, and every restart refused for 23 minutes). seat_fg.sh runs this script once at the
# start of a generation whose predecessor died, and the Windows stub's crash exit runs it through the stop task. What it
# reaps is chosen by rules on the process tree and on this stack's own names, never by the port a process holds: an
# engine process (argv[0] starts with VLLM::) is an orphan when NO ancestor is a live vLLM API server (`vllm serve`, or
# `python -m vllm.entrypoints.…`), so a live engine's workers are never touched; the MP server is matched by THIS stack's
# MP port and unit. That is a rule about the process tree, not proof of ownership: an engine started so that it has no
# such ancestor (a script that builds the engine in-process) looks orphaned to it, so run such jobs where no seat lives.
# A foreign listener is left alone, and seat_fg.sh refuses to start on it.
# SEAT_REAP_WAIT_SEC (default 10) is how long a reaped process gets to exit before it is reported as stuck, and how long
# an MP server gets to obey SIGTERM before SIGKILL. It is read as a decimal number of seconds ("08" is 8, not an octal
# error that skips every wait), a value that is not a number is said and means 10, and 0 waits for nothing but still looks once.
# Its output goes to the seat log (SEAT_LOG) whichever way it was started: seat_fg.sh runs it attached
# (SEAT_STOP_ATTACHED=1: its own stdout already is that log); the stop task and cmdStop have no stdout to speak of, so it
# appends to the log itself, and a worker named as stuck or an INCOMPLETE stop is not lost.
set -u
# An env file that was NAMED (the argument, or SEAT_ENV) and is not there is a mistake, not "no env file": falling back to
# the default seat's ports and unit would stop whatever holds them. Only the default path may be absent (a box that
# configures the seat through its environment).
CFG="${1:-${SEAT_ENV:-/root/g7/seat.env}}"
if [ ! -f "$CFG" ] && { [ -n "${1:-}" ] || [ -n "${SEAT_ENV:-}" ]; }; then
  echo "seat_stop: env file $CFG not found - not guessing this seat's ports and unit; nothing was stopped"; exit 2
fi
[ -f "$CFG" ] && . "$CFG"
LOG="${SEAT_LOG:-/root/g7/seat.log}"
if [ -z "${SEAT_STOP_ATTACHED:-}" ] && [ -d "$(dirname "$LOG")" ]; then exec > >(tee -a "$LOG") 2>&1; fi
PORT="${SEAT_PORT:-18797}"
MP_UNIT="${SEAT_MP_UNIT:-lmcache-mp}"
MP_PORT="${SEAT_MP_PORT:-18796}"
WAIT="${SEAT_REAP_WAIT_SEC:-10}"
case "$WAIT" in *[!0-9]*) echo "seat_stop: WARN SEAT_REAP_WAIT_SEC='$WAIT' is not a number of seconds; using 10"; WAIT=10 ;; esac
WAIT=$((10#$WAIT))
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
# Then any engine process with no live API-server ancestor (nothing else spawns the VLLM:: names). An engine process
# is an orphan when NO ancestor is a live `vllm serve` (or `python -m vllm.entrypoints.…`, the other way to start the same
# server) — it is reparented to whatever held the wrapper's stdout (a tee subshell, pid 381 on 2026-09-05), or to the WSL
# session's init, not to pid 1, so a ppid==1 test misses it. Walk up.
has_api_ancestor() { local q="$1" n=0; while [ "$q" -gt 1 ] 2>/dev/null && [ $n -lt 32 ]; do
  if ps -o args= -p "$q" 2>/dev/null | grep -Eq 'vllm serve|vllm\.entrypoints\.'; then return 0; fi
  q=$(ps -o ppid= -p "$q" 2>/dev/null | tr -d ' '); n=$((n+1)); [ -z "$q" ] && break; done; return 1; }
# A killed process that is still listed as a zombie has released its memory: only a live entry counts as stuck.
alive() { kill -0 "$1" 2>/dev/null && [ "$(ps -o stat= -p "$1" 2>/dev/null | cut -c1)" != Z ]; }
# The processes of a list that are still alive, each with a leading space (nothing when none is).
alive_of() { local p out=""; for p in "$@"; do alive "$p" && out="$out $p"; done; echo "$out"; }
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
  for _ in $(seq 1 $(( WAIT * 2 ))); do [ -z "$(alive_of $REAPED)" ] && break; sleep 0.5; done
  STUCK="$(alive_of $REAPED)"   # judged after the wait, not only inside it, so a wait of 0 still looks
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
  for _ in $(seq 1 $(( WAIT * 2 ))); do [ -z "$(alive_of $MP_KILLED)" ] && break; sleep 0.5; done
  MP_STUCK="$(alive_of $MP_KILLED)"
  [ -n "$MP_STUCK" ] && echo "seat_stop: WARN MP server still alive ${WAIT} s after SIGKILL:$(for p in $MP_STUCK; do printf ' %s(state %s)' "$p" "$(ps -o stat= -p "$p" 2>/dev/null | tr -d ' ')"; done)"
fi
# --- reaping ends here: seat_fg.stale-mp.tests.sh runs everything above this line against stand-in processes; the rest touches the box ---
# Only this stack's segments — never every process's shared memory in the distro, and none while ANY vLLM engine is alive
# (a sibling seat's NCCL/torch/LMCache segments live in the same directory), however it was started.
if ! pgrep -f 'vllm serve|vllm\.entrypoints\.' >/dev/null; then
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
