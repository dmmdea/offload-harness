#!/usr/bin/env bash
# Behavioral test of the seat launcher's crash cleanup: what seat_fg.sh's port-refusal block and seat_stop.sh's reaping
# section do to a dead generation's leftovers.
#   0.143.0: a crashed generation's LMCache MP server holding the MP HTTP port is cleaned up by the stack's own
#            seat_stop.sh and the start proceeds; a foreign holder is still refused; a LIVE engine is never "cleaned up".
#   now:     the engine workers a dead API server leaves behind (VLLM::EngineCore, VLLM::Worker_*) are reaped even when
#            the MP server is gone and its port is free; the MP server is reaped by process identity (this stack's MP
#            port), SIGTERM escalating to SIGKILL; a worker or an MP server that survives SIGKILL is named and fails the
#            stop; a real three-level engine tree (`vllm serve` -> VLLM::EngineCore -> VLLM::Worker_*) of a sibling is
#            never touched; the wait knobs are decimal numbers whatever they look like; and none of it reaches a foreign
#            listener or a live engine's workers — proven against stand-ins, and by mutants that break those two guards
#            and must be caught.
# The real code runs, never a copy of its logic: seat_fg.sh between its >>> and <<< marker lines, seat_stop.sh up to its
# "reaping ends here" line (plus, in the cases that say so, its exit-status tail, and its shared-memory sweep redirected to
# a scratch directory; the VRAM readback between them touches the box and is never run). The stand-ins carry the real
# names: `VLLM::Worker_TP0` as argv[0] (what vLLM's setproctitle sets), a script called `lmcache` invoked as
# `lmcache server --port … --http-port …`, and `vllm serve … --port N`. Scratch ports 28790-28798 and a systemd unit name
# that does not exist — except one case that, run as root under a live systemd, starts the MP server stand-in in a real
# transient unit of that name (the shape seat_fg.sh gives the real one) and checks it is stopped through the unit. Linux
# only (ss, pgrep, ps, python3, setsid).
# The code under test sweeps the whole box (every VLLM:: process, every `vllm serve`) and the scratch ports are fixed, so the
# test takes a lock and SKIPs when another run holds it, and SKIPs on a box where a real engine process is alive.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
# One run at a time per box: a second run that overlapped this one would reap its stand-ins and fail on nothing of its own
# making. flock(1) itself holds the lock (-o: nothing this run starts inherits it), so it is released when this run ends. The
# lock is a DIRECTORY (world-writable, sticky), not a file: with fs.protected_regular=2 (the Ubuntu default) a lock file that
# one user created cannot be opened by another in a sticky /tmp, and a root run after an unprivileged one (or the reverse)
# would fail instead of skipping. Any user can open a directory to lock it.
if [ -z "${SEAT_FG_TEST_LOCKED:-}" ] && command -v flock >/dev/null; then
  LOCKDIR="${TMPDIR:-/tmp}/seat_fg.stale-mp.tests.lock.d"
  [ -d "$LOCKDIR" ] || mkdir -m 1777 "$LOCKDIR" 2>/dev/null
  if [ -d "$LOCKDIR" ]; then
    SEAT_FG_TEST_LOCKED=1 flock -n -o -E 99 "$LOCKDIR" bash "$0" "$@"; rc=$?
    [ "$rc" -eq 99 ] && { echo "SKIP (another run of this test holds $LOCKDIR: runs share scratch ports and a box-wide sweep)"; exit 0; }
    exit "$rc"
  fi
fi
command -v ss >/dev/null && command -v pgrep >/dev/null && command -v python3 >/dev/null && command -v setsid >/dev/null || { echo "SKIP (needs ss, pgrep, python3, setsid)"; exit 0; }
PORT=28797; OTHER_PORT=28798; MP_PORT=28796; OTHER_MP_PORT=28795; MP_HTTP_PORT=28790
if ps -eo pid,args 2>/dev/null | awk '$2 ~ /^VLLM::/ {found=1} END {exit !found}'; then
  echo "SKIP (a VLLM:: engine process is alive on this box, and the orphan sweep under test is box-wide)"; exit 0
fi
if pgrep -f 'vllm serve' >/dev/null 2>&1; then
  echo "SKIP (a \`vllm serve\` process is alive on this box, and the shared-memory guard under test looks at every one)"; exit 0
fi
for p in $PORT $OTHER_PORT $MP_PORT $OTHER_MP_PORT $MP_HTTP_PORT; do
  if ss -ltn 2>/dev/null | grep -q ":$p "; then echo "SKIP (scratch port :$p is already in use)"; exit 0; fi
done
T="$(mktemp -d)"
trap 'rc=$?; builtin kill -9 $(cat "$T"/*.pid 2>/dev/null) 2>/dev/null; [ -n "${UNIT_STARTED:-}" ] && systemctl stop "lmcache-mp-scratch-$$" >/dev/null 2>&1; rm -rf "$T"; exit $rc' EXIT
# The stop script appends its own output to the seat log when nothing else carries it there; here that is a scratch file, never the
# real <seat-dir>/seat.log of whatever box runs this.
SEAT_LOG="$T/seat.log"; export SEAT_LOG
fail=0
pass() { echo "PASS $1"; }
failcase() { echo "FAIL $1: $2"; fail=1; }

# ---- scratch stack: the real code, extracted -----------------------------------------------------------------------
bash -n "$HERE/seat_fg.sh" && bash -n "$HERE/seat_stop.sh" && pass "seat_fg.sh and seat_stop.sh parse" || failcase "syntax" "seat_fg.sh or seat_stop.sh does not parse"
awk '/^# >>> port refusals/{on=1} on{print} /^# <<< port refusals/{exit}' "$HERE/seat_fg.sh" > "$T/block.sh"
grep -q 'seat_stop.sh' "$T/block.sh" && grep -q '^# <<< port refusals' "$T/block.sh" || { echo "FAIL: seat_fg.sh has no marked port-refusal block with the stack's cleanup"; exit 1; }
cat > "$T/seat_fg.sh" <<EOF
#!/usr/bin/env bash
set -u
PORT=$PORT; MP_PORT=$MP_PORT; MP_HTTP_PORT=$MP_HTTP_PORT; CFG=$T/seat.env
$(cat "$T/block.sh")
echo "START PROCEEDS"
EOF
sed '/^# --- reaping ends here/,$d' "$HERE/seat_stop.sh" > "$T/seat_stop.real.sh"
grep -q 'has_api_ancestor' "$T/seat_stop.real.sh" && [ "$(wc -l < "$T/seat_stop.real.sh")" -lt "$(wc -l < "$HERE/seat_stop.sh")" ] || { echo "FAIL: seat_stop.sh has no marked reaping section"; exit 1; }
echo 'echo "scratch seat_stop: reaping done"' >> "$T/seat_stop.real.sh"
cp "$T/seat_stop.real.sh" "$T/seat_stop.sh"
cat > "$T/seat.env" <<EOF
SEAT_PORT=$PORT
SEAT_MP_PORT=$MP_PORT
SEAT_MP_HTTP_PORT=$MP_HTTP_PORT
SEAT_MP_UNIT=lmcache-mp-scratch-$$
SEAT_REAP_WAIT_SEC=3
EOF
# stub seat_stop.sh: kills only the listener recorded as THIS stack's stale MP server, 3 s late, and exits 3 — a unit
# that is slow to release its socket.
cat > "$T/seat_stop.stub.sh" <<'EOF'
#!/usr/bin/env bash
d="$(dirname "$(readlink -f "$0")")"
p="$(cat "$d/stale.pid")"
( sleep 3; kill "$p" ) >/dev/null 2>&1 &
echo "stub seat_stop: slow release"; exit 3
EOF
# stand-in for `lmcache server`: binds --port and --http-port on loopback like the real one and idles.
# STANDIN_TERM=ignore ignores SIGTERM (a server stuck in its own shutdown); STANDIN_TERM=slow exits 1 s after SIGTERM;
# STANDIN_NOHTTP=1 never binds the HTTP port (a server that lost its HTTP socket). The SIGTERM behaviour is installed BEFORE
# anything binds: the test takes a bound port for "the stand-in is ready", so a handler installed after the bind would leave a
# window in which the stand-in still dies on SIGTERM like a well-behaved server.
mkdir -p "$T/venv/bin"
cat > "$T/venv/bin/lmcache" <<'EOF'
import os, signal, socket, sys, time
a = sys.argv[1:]
mode = os.environ.get("STANDIN_TERM", "")
if mode == "ignore":
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
elif mode == "slow":
    def slow(*_):
        time.sleep(1)
        os._exit(0)
    signal.signal(signal.SIGTERM, slow)
socks = []
for flag in ("--port", "--http-port"):
    if flag == "--http-port" and os.environ.get("STANDIN_NOHTTP"):
        continue
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("127.0.0.1", int(a[a.index(flag) + 1])))
    s.listen()
    socks.append(s)
time.sleep(600)
EOF

# ---- stand-ins and helpers -------------------------------------------------------------------------------------------
LISTEN_PY='import socket,time,sys;s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(("127.0.0.1",int(sys.argv[1])));s.listen();time.sleep(300)'
wait_bound() { local i; for i in $(seq 1 50); do ss -ltn 2>/dev/null | grep -q ":$1 " && return 0; sleep 0.1; done; return 1; }
wait_free()  { local i; for i in $(seq 1 50); do ss -ltn 2>/dev/null | grep -q ":$1 " || return 0; sleep 0.1; done; return 1; }
wait_name()  { local i a; for i in $(seq 1 50); do a="$(ps -o args= -p "$1" 2>/dev/null)"; case "$a" in "$2"*) return 0 ;; esac; sleep 0.1; done; return 1; }
# a listener that is nobody's: a plain python process, not named like anything of the stack
foreign() { python3 -c "$LISTEN_PY" "$1" >/dev/null 2>&1 & echo $! > "$T/$2.pid"; disown $!; wait_bound "$1" || echo "test setup: :$1 never bound"; }
# a stand-in for an MP server: name, MP port, MP HTTP port, optional SIGTERM behaviour, optional "1" = no HTTP socket
mp_standin() {
  STANDIN_TERM="${4:-}" STANDIN_NOHTTP="${5:-}" python3 "$T/venv/bin/lmcache" server --host 127.0.0.1 --port "$2" --http-host 127.0.0.1 --http-port "$3" --chunk-size 784 >/dev/null 2>&1 &
  echo $! > "$T/$1.pid"; disown $!
  if [ -n "${5:-}" ]; then wait_bound "$2" || echo "test setup: :$2 never bound"; else wait_bound "$3" || echo "test setup: :$3 never bound"; fi
}
# an engine process whose API server is gone: named VLLM::…, in its own session and reparented away from this test, so no
# ancestor of it carries a `vllm serve` in its arguments (whatever launched this test might). The pid comes from inside.
orphan() {
  ( setsid bash -c 'echo $$ > "$0"; exec -a "$1" sleep 300' "$T/$1.pid" "$2" >/dev/null 2>&1 & )
  local i; for i in $(seq 1 50); do [ -s "$T/$1.pid" ] && break; sleep 0.1; done
  wait_name "$(cat "$T/$1.pid")" "$2" || echo "test setup: $2 never renamed"
}
# a LIVE engine (`vllm serve … --port N`) with a worker child named VLLM::Worker_TP0; the worker's pid goes to <name>-worker.pid
engine() {
  ( exec -a "vllm serve fake --port $2" bash -c '( exec -a VLLM::Worker_TP0 sleep 300 ) & echo $! > "$0"; wait' "$T/$1-worker.pid" ) >/dev/null 2>&1 &
  echo $! > "$T/$1.pid"; disown $!; wait_name "$!" "vllm serve fake --port $2" || echo "test setup: engine $1 never renamed"
  local i; for i in $(seq 1 50); do [ -s "$T/$1-worker.pid" ] && break; sleep 0.1; done; wait_name "$(cat "$T/$1-worker.pid")" "VLLM::Worker_TP0" || echo "test setup: worker of $1 never renamed"
}
# a LIVE engine as vLLM really builds it, THREE levels deep: `vllm serve` -> VLLM::EngineCore (the API server spawns it) ->
# VLLM::Worker_TP0 (the engine core spawns the workers). pids go to <name>-core.pid and <name>-worker.pid.
cat > "$T/tree.py" <<'PYEOF'
import shutil, subprocess, sys, time
if sys.argv[1] == "api":          # the API server spawns the engine core (sys.executable is unusable under `exec -a`)
    core = subprocess.Popen(["VLLM::EngineCore", sys.argv[0], "core", sys.argv[2]], executable=shutil.which("python3"))
    print(core.pid, file=open(sys.argv[3], "w"))
else:                             # the engine core spawns a worker
    w = subprocess.Popen(["VLLM::Worker_TP0", "300"], executable="/usr/bin/sleep")
    print(w.pid, file=open(sys.argv[2], "w"))
time.sleep(600)
PYEOF
engine3l() {   # name, port, optional argv[0] of the API server (default: the `vllm serve` form)
  ( exec -a "${3:-vllm serve fake --port $2}" python3 "$T/tree.py" api "$T/$1-worker.pid" "$T/$1-core.pid" ) >/dev/null 2>&1 &
  echo $! > "$T/$1.pid"; disown $!
  local i; for i in $(seq 1 60); do [ -s "$T/$1-worker.pid" ] && [ -s "$T/$1-core.pid" ] && break; sleep 0.1; done
  wait_name "$(cat "$T/$1-worker.pid")" "VLLM::Worker_TP0" || echo "test setup: 3-level worker of $1 never named"
}
state() { ps -o stat= -p "$1" 2>/dev/null | tr -d ' '; }
gone() { local s; s="$(state "$(cat "$T/$1.pid")")"; [ -z "$s" ] || [ "${s#Z}" != "$s" ]; }   # exited, or a zombie awaiting its parent
alive() { ! gone "$1"; }
reset() { builtin kill -9 $(cat "$T"/*.pid 2>/dev/null) 2>/dev/null; rm -f "$T"/*.pid; local p; for p in $MP_HTTP_PORT $MP_PORT $OTHER_MP_PORT; do wait_free "$p"; done; sleep 0.2; }
run() { SEAT_MP_PORT_WAIT_SEC="${WAITSEC-2}" bash "$T/seat_fg.sh" 2>&1; }   # WAITSEC unset = 2 s; WAITSEC= (empty) reaches the launcher empty
has() { grep -q -- "$1" <<<"$out"; }
# Records every `sleep` the shells started under it make (an exported function child bash processes inherit), so a test can
# say "no 3 s grace was waited". A probe proves the recorder reaches a child shell; without it a silent recorder would pass.
SLEEP_LOG="$T/sleeps"; export SLEEP_LOG
record_sleeps() {
  rm -f "$SLEEP_LOG"
  sleep() { printf '%s\n' "$*" >> "$SLEEP_LOG"; command sleep "$@"; }; export -f sleep
  bash -c 'sleep 0.01'; grep -qx 0.01 "$SLEEP_LOG" || failcase "sleep recorder" "it did not reach a child shell, so a \"no 3 s grace\" assertion would pass vacuously"
}
stop_recording() { unset -f sleep; }
# Fast-forwards every `sleep` the shells started under it make to 20 ms (a case that is about a count of waits, not about time).
fast_sleep_on() { sleep() { command sleep 0.02; }; export -f sleep; }
fast_sleep_off() { unset -f sleep; }
# An immortal process: SIGKILL, however it is spelled (-KILL, -SIGKILL, -9, -s KILL, --signal=KILL), leaves pid $1 alive — what a
# process stuck in the GPU driver (state D) does. With a second argument the real SIGKILL lands that many seconds late: a process
# that needs a moment to leave the driver. Exported functions reach the child bash processes the scripts under test run in.
immortal_on() {
  IMMORTAL="$1"; IMMORTAL_LATE="${2:-}"; export IMMORTAL IMMORTAL_LATE
  is_sigkill() {   # succeeds when this kill call sends SIGKILL to $IMMORTAL
    local sig=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -KILL|-SIGKILL|-9|--signal=KILL|--signal=SIGKILL|--signal=9) sig=KILL ;;
        -s|--signal) case "${2:-}" in KILL|SIGKILL|9) sig=KILL ;; esac; shift ;;
        -*) ;;
        *) [ "$sig" = KILL ] && [ "$1" = "${IMMORTAL:-}" ] && return 0 ;;
      esac
      shift
    done
    return 1
  }
  kill() {
    if is_sigkill "$@"; then
      [ -n "${IMMORTAL_LATE:-}" ] && { ( command sleep "$IMMORTAL_LATE"; builtin kill -KILL "$IMMORTAL" ) >/dev/null 2>&1 & }
      return 0
    fi
    builtin kill "$@"
  }
  export -f is_sigkill kill
}
immortal_off() { unset -f kill is_sigkill; unset IMMORTAL IMMORTAL_LATE; }
# the exit-status build of seat_stop.sh: the real reaping section and the real exit-status tail (no shared-memory sweep, no VRAM readback)
sed '/^# --- reaping ends here/,$d' "$HERE/seat_stop.sh" > "$T/seat_stop.exit.sh"; sed -n '/^# --- exit status/,$p' "$HERE/seat_stop.sh" >> "$T/seat_stop.exit.sh"
grep -q 'seat stopped' "$T/seat_stop.exit.sh" && grep -q 'has_api_ancestor' "$T/seat_stop.exit.sh" || { echo "FAIL: seat_stop.sh has no marked exit-status tail"; exit 1; }

# 1. a crashed generation's MP server (the stack's own, by name and MP port) holds the MP HTTP port, no engine: reaped
mp_standin stale $MP_PORT $MP_HTTP_PORT; out="$(run)"
if has "running seat_stop.sh once" && has "reaping MP server" && has "START PROCEEDS" && gone stale; then pass "stale MP server is cleaned up and the start proceeds"; else failcase "stale MP server" "$out"; fi
reset

# 1b. the cleanup fails (exit 3) and the port frees 3 s later: the exit code is printed and the start waits for the port
cp "$T/seat_stop.stub.sh" "$T/seat_stop.sh"; foreign $MP_HTTP_PORT stale; out="$(WAITSEC=6 run)"; cp "$T/seat_stop.real.sh" "$T/seat_stop.sh"
if has "seat_stop.sh exited 3" && has "START PROCEEDS"; then pass "a slow release is waited out and the cleanup's exit code is printed"; else failcase "slow release" "$out"; fi
reset

# 1c. SEAT_MP_PORT_WAIT_SEC is a decimal number of seconds whatever it looks like: "08" is not an octal error that skips the wait
#     (the port is refused at once instead of waited for), and a value that is not a number falls back to 10. The holder frees 3 s
#     after the cleanup, so only a start that really waits (8 s, 10 s) gets to proceed.
#     A value that is not a number is said so in the log (it was replaced by 10 without a word); a number, "08" included, is not.
bad=""
for v in 08 abc; do
  cp "$T/seat_stop.stub.sh" "$T/seat_stop.sh"; foreign $MP_HTTP_PORT stale; out="$(WAITSEC="$v" run)"; cp "$T/seat_stop.real.sh" "$T/seat_stop.sh"
  has "START PROCEEDS" || bad="$bad [SEAT_MP_PORT_WAIT_SEC='$v': $(tr '\n' '|' <<<"$out" | cut -c1-200)]"
  if [ "$v" = abc ]; then has "WARN SEAT_MP_PORT_WAIT_SEC='abc' is not a number of seconds; using 10" || bad="$bad [abc: the launcher did not say the value was not a number]"
  else has "WARN SEAT_MP_PORT_WAIT_SEC" && bad="$bad [$v is a number and was warned about]"; fi
  reset
done
if [ -z "$bad" ]; then pass "SEAT_MP_PORT_WAIT_SEC is a decimal number of seconds: 08 is waited out, abc means 10 and is said"; else failcase "SEAT_MP_PORT_WAIT_SEC" "$bad"; fi

# 2. a FOREIGN holder the stack's cleanup does not own: refused, and left alone. The wait for the port to free is the
#    knob's (2 s here), not the default 10 s.
foreign $MP_HTTP_PORT foreign; t0=$SECONDS; out="$(run)"; dt=$(( SECONDS - t0 ))
if has "REFUSING to start — the MP HTTP port" && ! has "START PROCEEDS" && alive foreign && [ "$dt" -lt 8 ]; then pass "a foreign holder is still refused"; else failcase "foreign holder" "took ${dt} s: $out"; fi
reset

# 3. a LIVE engine of this stack: no cleanup runs, the MP port holder is refused
foreign $MP_HTTP_PORT held; ( exec -a "vllm serve fake --port $PORT" sleep 300 ) >/dev/null 2>&1 & echo $! > "$T/engine3.pid"; disown $!; wait_name "$!" "vllm serve fake --port $PORT"
out="$(run)"
if ! has "running seat_stop.sh once" && has "REFUSING to start — the MP HTTP port" && ! has "START PROCEEDS" && alive engine3 && alive held; then pass "a live engine is never cleaned up"; else failcase "live engine" "$out"; fi
reset

# 4. orphaned engine workers, MP port FREE (the MP server died, the workers did not): reaped, the start proceeds. A unit
#    that is already gone is no warning.
orphan o1 "VLLM::EngineCore"; orphan o2 "VLLM::Worker_TP0"; orphan o3 "VLLM::Worker_TP1"; out="$(run)"
if has "engine processes named VLLM::" && [ "$(grep -c 'reaping orphaned engine process' <<<"$out")" -eq 3 ] && has "START PROCEEDS" && gone o1 && gone o2 && gone o3 && ! has "WARN systemctl stop"; then
  pass "orphaned engine workers are reaped with the MP port free and the start proceeds"; else failcase "orphaned workers" "$out"; fi
reset

# 5. the MP server AND the orphaned workers of the dead generation: both reaped in the one cleanup, and with no process tree
#    to wait for the cleanup does not sit out the 3 s grace the stop path gives a tree (every `sleep` the run makes is recorded)
mp_standin stale5 $MP_PORT $MP_HTTP_PORT; orphan o1 "VLLM::Worker_PP0"; orphan o2 "VLLM::Worker_PP1"
record_sleeps; out="$(run)"; stop_recording
if has "reaping MP server" && has "reaping orphaned engine process" && has "START PROCEEDS" && gone stale5 && gone o1 && gone o2 && ! grep -qx 3 "$SLEEP_LOG"; then
  pass "the MP server and the orphaned workers are reaped together"; else failcase "MP server + workers" "sleeps: $(tr '\n' ' ' < "$SLEEP_LOG"): $out"; fi
reset

# 6. a LIVE engine on another port has workers of its own: not this seat's leftovers, so no cleanup runs at all (a healthy
#    box with a sibling seat must not report a leftover or run a stop at every start), and nothing is reaped
engine other $OTHER_PORT; out="$(run)"
if ! has "reaping orphaned engine process" && ! has "running seat_stop.sh once" && has "START PROCEEDS" && alive other && alive other-worker; then
  pass "a live engine's workers on another port are never reaped"; else failcase "live engine's workers" "$out"; fi
reset

# 6b. a real engine is THREE levels deep (`vllm serve` -> VLLM::EngineCore -> VLLM::Worker_TP0): a live sibling's tree is never a
#     leftover at the start, and the stop path stops this seat's whole tree and nothing of the sibling's. A walk that stops one
#     level early would take every sibling worker for an orphan and SIGKILL a healthy seat.
engine3l sib $OTHER_PORT; out="$(run)"
if ! has "reaping orphaned engine process" && ! has "running seat_stop.sh once" && has "START PROCEEDS" && alive sib && alive sib-core && alive sib-worker; then pass "a live sibling's three-level engine tree is never a leftover"; else failcase "3-level sibling, start" "$out"; fi
reset
engine3l mine3 $PORT; engine3l sib3 $OTHER_PORT
out="$(bash "$T/seat_stop.sh" "$T/seat.env" 2>&1)"
if gone mine3 && gone mine3-core && gone mine3-worker && alive sib3 && alive sib3-core && alive sib3-worker; then pass "the stop path stops this seat's three-level tree and only that"; else failcase "3-level tree, stop path" "$out"; fi
reset

# 6c. an engine process is an orphan when NO ancestor is a live `vllm serve`: a live wrapper that merely mentions vllm (a launcher
#     script named vllm-seat-run.sh, a shell in a vllm venv) above it protects nothing
( exec -a "bash vllm-seat-run.sh --port $PORT" bash -c '( exec -a VLLM::Worker_TP0 sleep 300 ) & echo $! > "$0"; wait' "$T/wrapworker.pid" ) >/dev/null 2>&1 &
echo $! > "$T/wrap.pid"; disown $!
for i in $(seq 1 50); do [ -s "$T/wrapworker.pid" ] && break; sleep 0.1; done; wait_name "$(cat "$T/wrapworker.pid")" "VLLM::Worker_TP0" || echo "test setup: wrapped worker never renamed"
out="$(run)"
if has "reaping orphaned engine process" && has "START PROCEEDS" && gone wrapworker; then pass "an orphan under a live wrapper that only mentions vllm is still reaped"; else failcase "worker under a vllm-mentioning wrapper" "$out"; fi
reset

# 6d. this stack's MP server that lost its HTTP socket (MP port bound, HTTP port free) is a leftover of its own
mp_standin nohttp $MP_PORT $MP_HTTP_PORT "" 1
out="$(run)"
if has "running seat_stop.sh once" && has "an MP server on :$MP_PORT" && has "reaping MP server" && has "START PROCEEDS" && gone nohttp; then pass "an MP server without its HTTP socket is a leftover and is reaped"; else failcase "MP server without HTTP socket" "$out"; fi
reset

# 7. orphans are reaped, but a foreign holder of the MP HTTP port beside them is still refused and left alone
orphan o1 "VLLM::Worker_TP0"; foreign $MP_HTTP_PORT foreign7; out="$(run)"
if has "reaping orphaned engine process" && has "REFUSING to start — the MP HTTP port" && ! has "START PROCEEDS" && gone o1 && alive foreign7; then
  pass "orphans are reaped but a foreign MP HTTP holder is still refused and left alone"; else failcase "orphans + foreign holder" "$out"; fi
reset

# 8. an MP server of ANOTHER stack (another MP port) holding this stack's MP HTTP port is foreign: named like ours, not ours
mp_standin other-stack $OTHER_MP_PORT $MP_HTTP_PORT; out="$(run)"
if has "REFUSING to start — the MP HTTP port" && ! has "START PROCEEDS" && ! has "reaping MP server" && alive other-stack; then
  pass "another stack's MP server holding the MP HTTP port is foreign"; else failcase "other stack's MP server" "$out"; fi
reset

# 9. an MP server that ignores SIGTERM (stuck in its own shutdown) is killed once the grace is over
mp_standin stuck $MP_PORT $MP_HTTP_PORT ignore; out="$(run)"
if has "ignored SIGTERM for 3 s; SIGKILL" && has "START PROCEEDS" && gone stuck; then pass "an MP server that ignores SIGTERM is killed after the grace"; else failcase "MP server ignoring SIGTERM" "$out"; fi
reset

# 10. an MP server that obeys SIGTERM a second late is waited out, not killed
mp_standin slow $MP_PORT $MP_HTTP_PORT slow; out="$(run)"
if has "reaping MP server" && ! has "ignored SIGTERM" && has "START PROCEEDS" && gone slow; then pass "an MP server that obeys SIGTERM late is waited out, not killed"; else failcase "slow MP server" "$out"; fi
reset

# 11. a reaped worker that survives SIGKILL (stuck in the GPU driver, state D, in real life) is named, and the start goes on
orphan im "VLLM::Worker_TP0"; immortal_on "$(cat "$T/im.pid")"
out="$(run)"; immortal_off
if has "WARN orphaned engine process(es) still alive 3 s after SIGKILL: $(cat "$T/im.pid")(state S" && has "START PROCEEDS" && alive im; then pass "a reaped worker that survives SIGKILL is named"; else failcase "worker surviving SIGKILL" "$out"; fi
reset

# 11b. the exit status: a stop that leaves a process of the seat alive fails (INCOMPLETE, exit 1), a clean one says so and
#      exits 0 (the exit-status build of seat_stop.sh, above)
orphan im2 "VLLM::Worker_TP0"; immortal_on "$(cat "$T/im2.pid")"
out="$(bash "$T/seat_stop.exit.sh" "$T/seat.env" 2>&1)"; rc=$?; immortal_off
reset
out2="$(bash "$T/seat_stop.exit.sh" "$T/seat.env" 2>&1)"; rc2=$?
if [ "$rc" -eq 1 ] && has "INCOMPLETE" && [ "$rc2" -eq 0 ] && grep -qx "seat stopped" <<<"$out2"; then pass "a stop that leaves a process of the seat alive fails, a clean one exits 0"; else failcase "exit status" "rc=$rc: $out / rc=$rc2: $out2"; fi

# 11c. a reaped worker whose parent never wait()s stays <defunct> until the parent looks: it has released its memory, so it is
#      not stuck (in real life the orphans reparent to a tee subshell, not to pid 1, so zombies are the normal case)
cat > "$T/zparent.py" <<'PYEOF'
import os, subprocess, sys, time
w = subprocess.Popen(["VLLM::Worker_TP0", "300"], executable="/usr/bin/sleep")
print(w.pid, file=open(sys.argv[1], "w"))
print(os.getpid(), file=open(sys.argv[2], "w"))
time.sleep(600)          # never wait(): a killed child stays a zombie
PYEOF
( setsid python3 "$T/zparent.py" "$T/zw.pid" "$T/zp.pid" >/dev/null 2>&1 & )
for i in $(seq 1 50); do [ -s "$T/zw.pid" ] && [ -s "$T/zp.pid" ] && break; sleep 0.1; done
wait_name "$(cat "$T/zw.pid")" "VLLM::Worker_TP0" || echo "test setup: zombie-to-be never named"
out="$(bash "$T/seat_stop.exit.sh" "$T/seat.env" 2>&1)"; rc=$?; zs="$(state "$(cat "$T/zw.pid")")"
if [ "$rc" -eq 0 ] && ! has "INCOMPLETE" && has "orphaned engine process(es) gone" && [ "${zs#Z}" != "$zs" ]; then pass "a reaped worker left as a zombie by a parent that never waits is not stuck"; else failcase "zombie worker" "rc=$rc state=$zs: $out"; fi
reset

# 11d. an MP server that survives SIGKILL is named, and the stop fails: a worker is not the only thing that can be stuck, and an MP
#      server nobody names keeps its HTTP port and refuses the next start with no cause in the log
mp_standin stuck2 $MP_PORT $MP_HTTP_PORT ignore; immortal_on "$(cat "$T/stuck2.pid")"; fast_sleep_on
out="$(bash "$T/seat_stop.exit.sh" "$T/seat.env" 2>&1)"; rc=$?; fast_sleep_off; immortal_off
if [ "$rc" -eq 1 ] && has "WARN MP server still alive 3 s after SIGKILL" && has "INCOMPLETE" && alive stuck2; then pass "an MP server that survives SIGKILL is named and fails the stop"; else failcase "MP server surviving SIGKILL" "rc=$rc: $out"; fi
reset

# 11e. a reaped worker that needs 1.2 s to actually die after SIGKILL (a process leaving the GPU driver) is waited out for up to
#      SEAT_REAP_WAIT_SEC (3 s here), not reported stuck after half a second: the shim lands the real SIGKILL 1.2 s late
orphan slowdie "VLLM::Worker_TP0"; immortal_on "$(cat "$T/slowdie.pid")" 1.2
out="$(bash "$T/seat_stop.exit.sh" "$T/seat.env" 2>&1)"; rc=$?; immortal_off
if [ "$rc" -eq 0 ] && ! has "INCOMPLETE" && has "orphaned engine process(es) gone" && gone slowdie; then pass "a worker that dies 1.2 s after SIGKILL is waited out, not reported stuck"; else failcase "slow-dying worker" "rc=$rc: $out"; fi
reset

# 11f. the same for an MP server: SIGTERM ignored, SIGKILL lands 1.2 s late
mp_standin slowdie2 $MP_PORT $MP_HTTP_PORT ignore; immortal_on "$(cat "$T/slowdie2.pid")" 1.2
out="$(bash "$T/seat_stop.exit.sh" "$T/seat.env" 2>&1)"; rc=$?; immortal_off
if [ "$rc" -eq 0 ] && has "ignored SIGTERM for 3 s; SIGKILL" && ! has "WARN MP server still alive" && ! has "INCOMPLETE" && gone slowdie2; then pass "an MP server that dies 1.2 s after SIGKILL is waited out, not reported stuck"; else failcase "slow-dying MP server" "rc=$rc: $out"; fi
reset

# 11g. SEAT_REAP_WAIT_SEC is a decimal number of seconds whatever it looks like. "08" and "09" are not octal errors that skip
#      every wait (a survivor was then reported gone, exit 0), "0" waits for nothing but still checks once, and a value that is not
#      a number means 10. An immortal worker makes the number visible: the stop names it "still alive N s after SIGKILL" and
#      fails; an immortal MP server shows the same on its SIGTERM grace and its post-SIGKILL wait. The knob comes from its own env
#      file (seat.env sets it, and the file is sourced after the environment).
bad=""
for spec in "08:8" "09:9" "0:0" "abc:10" ":10" "12:12"; do
  v="${spec%%:*}"; want="${spec#*:}"
  printf 'SEAT_PORT=%s\nSEAT_MP_PORT=%s\nSEAT_MP_HTTP_PORT=%s\nSEAT_MP_UNIT=lmcache-mp-scratch-%s\nSEAT_REAP_WAIT_SEC=%s\n' "$PORT" "$MP_PORT" "$MP_HTTP_PORT" "$$" "$v" > "$T/seat.knob.env"
  orphan knob "VLLM::Worker_TP0"; immortal_on "$(cat "$T/knob.pid")"; fast_sleep_on
  out="$(bash "$T/seat_stop.exit.sh" "$T/seat.knob.env" 2>&1)"; rc=$?; fast_sleep_off; immortal_off
  { [ "$rc" -eq 1 ] && has "still alive $want s after SIGKILL" && has "INCOMPLETE"; } || bad="$bad [worker, SEAT_REAP_WAIT_SEC='$v' want $want s: rc=$rc $(tr '\n' '|' <<<"$out" | cut -c1-220)]"
  if [ "$v" = abc ]; then has "WARN SEAT_REAP_WAIT_SEC='abc' is not a number of seconds; using 10" || bad="$bad [abc: the stop did not say the value was not a number]"
  else has "WARN SEAT_REAP_WAIT_SEC" && bad="$bad [SEAT_REAP_WAIT_SEC='$v' is a number or empty and was warned about]"; fi
  reset
done
for spec in "08:8" "0:0"; do
  v="${spec%%:*}"; want="${spec#*:}"
  printf 'SEAT_PORT=%s\nSEAT_MP_PORT=%s\nSEAT_MP_HTTP_PORT=%s\nSEAT_MP_UNIT=lmcache-mp-scratch-%s\nSEAT_REAP_WAIT_SEC=%s\n' "$PORT" "$MP_PORT" "$MP_HTTP_PORT" "$$" "$v" > "$T/seat.knob.env"
  mp_standin knobmp $MP_PORT $MP_HTTP_PORT ignore; immortal_on "$(cat "$T/knobmp.pid")"; fast_sleep_on
  out="$(bash "$T/seat_stop.exit.sh" "$T/seat.knob.env" 2>&1)"; rc=$?; fast_sleep_off; immortal_off
  { [ "$rc" -eq 1 ] && has "ignored SIGTERM for $want s; SIGKILL" && has "WARN MP server still alive $want s after SIGKILL" && has "INCOMPLETE"; } || bad="$bad [MP server, SEAT_REAP_WAIT_SEC='$v' want $want s: rc=$rc $(tr '\n' '|' <<<"$out" | cut -c1-220)]"
  reset
done
if [ -z "$bad" ]; then pass "SEAT_REAP_WAIT_SEC is a decimal number of seconds: 08, 09 and 0 are honoured, abc means 10 and is said, an empty value means 10"; else failcase "SEAT_REAP_WAIT_SEC" "$bad"; fi

# 12. the stop path (llama-swap's cmdStop) stops THIS seat's engine and its whole tree, and nothing of another seat's; with a
#     process tree to wait for it still gives the tree its 3 s grace
engine mine $PORT; engine other12 $OTHER_PORT
record_sleeps; out="$(bash "$T/seat_stop.sh" "$T/seat.env" 2>&1)"; stop_recording
if has "scratch seat_stop: reaping done" && gone mine && gone mine-worker && alive other12 && alive other12-worker && grep -qx 3 "$SLEEP_LOG"; then pass "the stop path stops this seat's engine tree and only that"; else failcase "stop path" "sleeps: $(tr '\n' ' ' < "$SLEEP_LOG"): $out"; fi
reset

# 12b. the production shape: the stack's own MP server runs in its transient systemd unit (as seat_fg.sh starts it) and
#      outlives its engine. Needs root and a running systemd, so it is not run everywhere (a runner without them says so).
UNIT="lmcache-mp-scratch-$$"
if [ "$(id -u)" = 0 ] && command -v systemd-run >/dev/null && systemctl list-units --no-legend >/dev/null 2>&1; then
  UNIT_STARTED=1; systemd-run --unit="$UNIT" --collect -p TimeoutStopSec=5 python3 "$T/venv/bin/lmcache" server --host 127.0.0.1 --port $MP_PORT --http-host 127.0.0.1 --http-port $MP_HTTP_PORT --chunk-size 784 >/dev/null 2>&1
  wait_bound $MP_HTTP_PORT; wait_bound $MP_PORT; out="$(run)"
  if systemctl is-active --quiet "$UNIT"; then failcase "MP server in its unit" "the unit is still active: $out"
  elif has "running seat_stop.sh once" && has "START PROCEEDS" && ! ss -ltn 2>/dev/null | grep -q ":$MP_HTTP_PORT "; then pass "the MP server in its own systemd unit is stopped through the unit"
  else failcase "MP server in its unit" "$out"; fi
  systemctl stop "$UNIT" >/dev/null 2>&1; systemctl reset-failed "$UNIT" >/dev/null 2>&1
  reset
else
  echo "note: the systemd-unit case needs root and a running systemd; not run on this box"
fi

# 12c. the shared-memory sweep at the end of seat_stop.sh (outside the extracted reaping section) must not run while ANY
#      `vllm serve` is alive: a sibling seat's NCCL/torch/LMCache segments live in the same directory. The launcher and the
#      stub run this whole script at every crash, so this guard is the sibling's only protection. /dev/shm is redirected to a
#      scratch directory; the script is cut before the VRAM readback (no nvidia-smi).
mkdir -p "$T/shm"; sed -e '/^# VRAM readback/,$d' -e "s#/dev/shm#$T/shm#" "$HERE/seat_stop.sh" > "$T/seat_stop.shm.sh"; echo 'echo "scratch tail done"' >> "$T/seat_stop.shm.sh"
grep -q "$T/shm" "$T/seat_stop.shm.sh" || { echo "FAIL: seat_stop.sh has no /dev/shm sweep to redirect"; exit 1; }
touch "$T/shm/nccl-sib" "$T/shm/vllm_sib" "$T/shm/torch_sib" "$T/shm/lmcache_sib" "$T/shm/psm_sib" "$T/shm/unrelated"
engine sib5 $OTHER_PORT; out="$(bash "$T/seat_stop.shm.sh" "$T/seat.env" 2>&1)"
if [ -e "$T/shm/nccl-sib" ] && [ -e "$T/shm/vllm_sib" ] && [ -e "$T/shm/torch_sib" ] && [ -e "$T/shm/lmcache_sib" ] && [ -e "$T/shm/psm_sib" ] && has "scratch tail done"; then pass "the shared-memory sweep leaves a live sibling's segments alone"; else failcase "shm sweep with a live sibling" "$(ls "$T/shm" | tr '\n' ' '): $out"; fi
reset
# the same for a sibling started as `python -m vllm.entrypoints…`: it is as live as a `vllm serve` one
engine3l ep5 $OTHER_PORT "python3 -m vllm.entrypoints.openai.api_server --port $OTHER_PORT"; out="$(bash "$T/seat_stop.shm.sh" "$T/seat.env" 2>&1)"
if [ -e "$T/shm/nccl-sib" ] && [ -e "$T/shm/vllm_sib" ] && [ -e "$T/shm/torch_sib" ] && [ -e "$T/shm/lmcache_sib" ] && [ -e "$T/shm/psm_sib" ] && has "scratch tail done"; then pass "the shared-memory sweep leaves the segments of a sibling started as vllm.entrypoints alone"; else failcase "shm sweep with an entrypoints sibling" "$(ls "$T/shm" | tr '\n' ' '): $out"; fi
reset
out="$(bash "$T/seat_stop.shm.sh" "$T/seat.env" 2>&1)"
if [ ! -e "$T/shm/nccl-sib" ] && [ ! -e "$T/shm/vllm_sib" ] && [ ! -e "$T/shm/torch_sib" ] && [ ! -e "$T/shm/lmcache_sib" ] && [ ! -e "$T/shm/psm_sib" ] && [ -e "$T/shm/unrelated" ]; then pass "with no engine alive the sweep removes the stack's segments and only those"; else failcase "shm sweep, no engine" "$(ls "$T/shm" | tr '\n' ' '): $out"; fi

# 15. an env file that was NAMED (as the argument, or by SEAT_ENV) and is not there is refused with exit 2, never replaced by the
#     default seat's ports and unit: a typo in the stop task's argument must not stop whatever holds the defaults. The orphan
#     sweep is box-wide, so an orphan the run leaves alone is the proof that nothing was reaped.
orphan miss "VLLM::Worker_TP0"
out="$(bash "$T/seat_stop.exit.sh" "$T/no-such.env" 2>&1)"; rc=$?
out2="$(SEAT_ENV="$T/no-such.env" bash "$T/seat_stop.exit.sh" 2>&1)"; rc2=$?
if [ "$rc" -eq 2 ] && [ "$rc2" -eq 2 ] && has "env file $T/no-such.env not found" && grep -q "env file $T/no-such.env not found" <<<"$out2" && ! has "reaping" && ! grep -q "reaping" <<<"$out2" && alive miss; then
  pass "a named env file that is not there is refused and nothing is reaped"; else failcase "missing env file" "rc=$rc rc2=$rc2: $out / $out2"; fi
reset

# 16. an engine started as `python -m vllm.entrypoints.…` instead of `vllm serve` is as live as any: its EngineCore and workers have
#     a live API server above them, so they are no leftover at the start and no victim of the stop path (a box that also runs
#     a benchmark engine that way would otherwise lose it to this seat's next start or unload)
EP="python3 -m vllm.entrypoints.openai.api_server --port $OTHER_PORT"
engine3l ep $OTHER_PORT "$EP"; out="$(run)"
if ! has "reaping orphaned engine process" && ! has "running seat_stop.sh once" && has "START PROCEEDS" && alive ep && alive ep-core && alive ep-worker; then pass "a live sibling started as vllm.entrypoints is never a leftover"; else failcase "entrypoints sibling, start" "$out"; fi
reset
engine3l mine4 $PORT; engine3l ep2 $OTHER_PORT "$EP"
out="$(bash "$T/seat_stop.sh" "$T/seat.env" 2>&1)"
if gone mine4 && gone mine4-core && gone mine4-worker && alive ep2 && alive ep2-core && alive ep2-worker; then pass "the stop path leaves a sibling started as vllm.entrypoints and its workers alone"; else failcase "entrypoints sibling, stop path" "$out"; fi
reset

# 17. the stop's own output reaches the seat log when nothing else carries it there. The stop task's stdout goes nowhere (a hidden
#     wscript, no redirect), so a worker named as stuck, or an INCOMPLETE stop, would be lost with it. Attached to the launcher,
#     whose own stdout already is the seat log, it must not write the log a second time; the launcher runs it attached (the
#     scratch launcher has no tee of its own, so any write to the log by the stop is the duplicate the real one would show).
rm -f "$SEAT_LOG"; orphan lg "VLLM::Worker_TP0"; immortal_on "$(cat "$T/lg.pid")"; fast_sleep_on
out="$(bash "$T/seat_stop.exit.sh" "$T/seat.env" 2>&1)"; rc=$?; fast_sleep_off; immortal_off
if [ "$rc" -eq 1 ] && has "INCOMPLETE" && [ "$(grep -c "INCOMPLETE" "$SEAT_LOG" 2>/dev/null)" = 1 ] && grep -q "still alive 3 s after SIGKILL" "$SEAT_LOG"; then pass "the stop's output reaches the seat log when nothing else carries it there"; else failcase "stop output in the seat log" "rc=$rc log=[$(tr '\n' '|' < "$SEAT_LOG" 2>&1)]: $out"; fi
reset
rm -f "$SEAT_LOG"; orphan lg2 "VLLM::Worker_TP0"
out="$(SEAT_STOP_ATTACHED=1 bash "$T/seat_stop.exit.sh" "$T/seat.env" 2>&1)"; rc=$?
if [ "$rc" -eq 0 ] && has "reaping orphaned engine process" && [ ! -s "$SEAT_LOG" ]; then pass "attached to the launcher, the stop does not write the seat log a second time"; else failcase "attached stop" "rc=$rc log=[$(tr '\n' '|' < "$SEAT_LOG" 2>&1)]: $out"; fi
reset
rm -f "$SEAT_LOG"; orphan lg3 "VLLM::Worker_TP0"; out="$(run)"
if has "running seat_stop.sh once" && has "reaping orphaned engine process" && has "START PROCEEDS" && [ ! -s "$SEAT_LOG" ]; then pass "the launcher runs the stop attached, so the seat log is written once"; else failcase "launcher attaches the stop" "log=[$(tr '\n' '|' < "$SEAT_LOG" 2>&1)]: $out"; fi
reset

# 18. an MP server is this stack's only when its --port IS this stack's MP port, not when that port merely starts with it: a stack
#     whose MP port is 2879 must not take the MP server on 28795 for its own — neither the stop (SIGTERM, SIGKILL) nor the launcher
#     (a cleanup at every start). The stand-in binds only its --port, so nothing else about it looks like a leftover.
printf 'SEAT_PORT=%s\nSEAT_MP_PORT=2879\nSEAT_MP_HTTP_PORT=%s\nSEAT_MP_UNIT=lmcache-mp-scratch-%s\nSEAT_REAP_WAIT_SEC=3\n' "$PORT" "$MP_HTTP_PORT" "$$" > "$T/seat.prefix.env"
sed -e "s/MP_PORT=$MP_PORT;/MP_PORT=2879;/" -e "s#CFG=$T/seat.env#CFG=$T/seat.prefix.env#" "$T/seat_fg.sh" > "$T/seat_fg.prefix.sh"
grep -q "MP_PORT=2879;" "$T/seat_fg.prefix.sh" && grep -q "seat.prefix.env" "$T/seat_fg.prefix.sh" || { echo "FAIL: the prefix-port launcher was not generated"; exit 1; }
mp_standin prefixmp $OTHER_MP_PORT $MP_HTTP_PORT "" 1
out="$(bash "$T/seat_stop.exit.sh" "$T/seat.prefix.env" 2>&1)"; rc=$?
if [ "$rc" -eq 0 ] && ! has "reaping MP server" && alive prefixmp; then pass "the stop leaves an MP server whose port merely starts with this stack's alone"; else failcase "prefix MP port, stop path" "rc=$rc: $out"; fi
out="$(SEAT_MP_PORT_WAIT_SEC=2 bash "$T/seat_fg.prefix.sh" 2>&1)"
if ! has "running seat_stop.sh once" && has "START PROCEEDS" && alive prefixmp; then pass "the launcher does not take an MP server whose port merely starts with its own for a leftover"; else failcase "prefix MP port, launcher" "$out"; fi
reset

# 19. an engine process is one whose argv[0] STARTS with VLLM:: (what vLLM's setproctitle sets): a name that merely contains it is not
#     one, and neither the launcher nor the stop touches it
orphan decoy "not-VLLM::Worker_TP0"
out="$(run)"
if ! has "engine processes named VLLM::" && ! has "running seat_stop.sh once" && has "START PROCEEDS" && alive decoy; then pass "a process whose name merely contains VLLM:: is no engine process at the start"; else failcase "decoy name, start" "$out"; fi
out="$(bash "$T/seat_stop.sh" "$T/seat.env" 2>&1)"
if ! has "reaping orphaned engine process" && alive decoy; then pass "a process whose name merely contains VLLM:: is left alone by the stop"; else failcase "decoy name, stop path" "$out"; fi
reset

# 20. the unit stop, needing neither root nor systemd: a stand-in systemctl first on the PATH records what the stop asks of it. The
#     stop goes through THIS stack's unit; a failing stop of a unit that is still active is a warning; one of a unit that is already
#     gone (a crash cleanup finds none) is not.
mkdir -p "$T/shim"
cat > "$T/shim/systemctl" <<'EOF'
#!/usr/bin/env bash
echo "$*" >> "$SHIM_LOG"
case "$1" in
  stop) [ "${SHIM_STOP_FAILS:-}" = 1 ] && exit 1; exit 0 ;;
  is-active) [ "${SHIM_ACTIVE:-}" = 1 ] && exit 0; exit 3 ;;
esac
exit 0
EOF
chmod +x "$T/shim/systemctl"; SHIM_LOG="$T/shim.log"; export SHIM_LOG
rm -f "$SHIM_LOG"; out="$(PATH="$T/shim:$PATH" bash "$T/seat_stop.exit.sh" "$T/seat.env" 2>&1)"
if grep -qx "stop lmcache-mp-scratch-$$" "$SHIM_LOG" && grep -qx "reset-failed lmcache-mp-scratch-$$" "$SHIM_LOG" && ! has "WARN systemctl stop"; then pass "the stop asks systemd to stop this stack's unit, and a stopped unit is no warning"; else failcase "unit stop" "shim log: $(tr '\n' '|' < "$SHIM_LOG" 2>&1): $out"; fi
out="$(SHIM_STOP_FAILS=1 SHIM_ACTIVE=1 PATH="$T/shim:$PATH" bash "$T/seat_stop.exit.sh" "$T/seat.env" 2>&1)"
out2="$(SHIM_STOP_FAILS=1 PATH="$T/shim:$PATH" bash "$T/seat_stop.exit.sh" "$T/seat.env" 2>&1)"
if has "WARN systemctl stop lmcache-mp-scratch-$$ failed" && ! grep -q "WARN systemctl stop" <<<"$out2"; then pass "a failed stop of a live unit is a warning, a unit that is already gone is not"; else failcase "unit stop failure" "active: $out / gone: $out2"; fi

# 13. mutation: reaping by PORT instead of by identity (kill whatever holds the MP HTTP port) must be caught by the foreign-holder test
mkdir -p "$T/mut1"; cp "$T/seat_fg.sh" "$T/mut1/seat_fg.sh"; cp "$T/seat_stop.real.sh" "$T/mut1/seat_stop.sh"
printf '%s\n' 'for p in $(ss -ltnp 2>/dev/null | grep ":${SEAT_MP_HTTP_PORT} " | grep -oE "pid=[0-9]+" | cut -d= -f2); do kill -KILL "$p" 2>/dev/null; done' >> "$T/mut1/seat_stop.sh"
# 14. mutation: dropping the final MP HTTP port refusal must be caught by the same test
mkdir -p "$T/mut2"
python3 - "$T/seat_fg.sh" "$T/mut2/seat_fg.sh" <<'EOF'
import sys
lines = open(sys.argv[1]).read().split("\n")
pat = 'if ss -ltnp 2>/dev/null | grep -q ":$MP_HTTP_PORT "; then'
hits = [i for i, l in enumerate(lines) if l == pat]
if hits:
    i = hits[-1]
    j = i
    while lines[j] != "fi":
        j += 1
    del lines[i:j + 1]
open(sys.argv[2], "w").write("\n".join(lines))
EOF
cp "$T/seat_stop.real.sh" "$T/mut2/seat_stop.sh"
guard_holds() {   # dir -> 0 when a foreign MP HTTP holder is still refused and left alive by that stack
  foreign $MP_HTTP_PORT fm; local o r
  o="$(SEAT_MP_PORT_WAIT_SEC=2 bash "$1/seat_fg.sh" 2>&1)"
  grep -q "REFUSING to start — the MP HTTP port" <<<"$o" && ! grep -q "START PROCEEDS" <<<"$o" && alive fm; r=$?; reset; return $r
}
if cmp -s "$T/seat_stop.real.sh" "$T/mut1/seat_stop.sh"; then failcase "mutation 1" "the reap-by-port mutation did not apply"
elif guard_holds "$T/mut1"; then failcase "mutation 1" "reaping the holder of the MP HTTP port by port went UNDETECTED: the foreign-holder test does not guard identity"
else pass "mutation: reaping by port instead of by identity is caught"; fi
if cmp -s "$T/seat_fg.sh" "$T/mut2/seat_fg.sh"; then failcase "mutation 2" "the drop-the-refusal mutation did not apply"
elif guard_holds "$T/mut2"; then failcase "mutation 2" "dropping the MP HTTP port refusal went UNDETECTED: the foreign-holder test does not guard the refusal"
else pass "mutation: dropping the final MP HTTP port refusal is caught"; fi
guard_holds "$T" && pass "the unmutated stack holds the foreign-holder guard" || failcase "control" "the unmutated stack lost the foreign-holder guard"

# 16. the path defaults derive from the directory the scripts sit in: copies placed in a scratch directory resolve the env file, the log,
# the venv and the work directory beside themselves, and the environment still wins. The real default lines are extracted from the
# scripts (HERE through WORK in seat_fg.sh, HERE through LOG in seat_stop.sh) and evaluated with the copy's own path as $0.
mkdir -p "$T/derive"; cp "$HERE/seat_fg.sh" "$HERE/seat_stop.sh" "$T/derive/"; D="$(cd "$T/derive" && pwd)"
fg_eval='eval "$(sed -n "/^HERE=/,/^WORK=/p" "$0")"; printf "%s|%s|%s|%s|%s" "$HERE" "$CFG" "$LOG" "$VENV" "$WORK"'
stop_eval='eval "$(sed -n "/^HERE=/,/^LOG=/p" "$0")"; printf "%s|%s|%s" "$HERE" "$CFG" "$LOG"'
got_fg="$(cd / && env -u SEAT_ENV -u SEAT_LOG -u SEAT_VENV -u SEAT_WORKDIR bash -c "set -u; $fg_eval" "$D/seat_fg.sh" 2>&1)"
got_stop="$(cd / && env -u SEAT_ENV -u SEAT_LOG bash -c "set -u; $stop_eval" "$D/seat_stop.sh" 2>&1)"
if [ "$got_fg" = "$D|$D/seat.env|$D/seat.log|$D/vllm-env|$D" ] && [ "$got_stop" = "$D|$D/seat.env|$D/seat.log" ]; then pass "a copy of the scripts resolves its env file, log, venv and work directory beside itself"
else failcase "directory defaults" "seat_fg.sh: $got_fg / seat_stop.sh: $got_stop"; fi
got_env="$(cd / && SEAT_ENV=/e/x.env SEAT_LOG=/e/l.log SEAT_VENV=/e/v SEAT_WORKDIR=/e/w bash -c "set -u; $fg_eval" "$D/seat_fg.sh" 2>&1)"
if [ "$got_env" = "$D|/e/x.env|/e/l.log|/e/v|/e/w" ]; then pass "the environment still overrides every derived default"; else failcase "directory defaults" "environment overrides: $got_env"; fi

[ $fail -eq 0 ] && echo "ALL PASS" || { echo "FAILURES"; exit 1; }
