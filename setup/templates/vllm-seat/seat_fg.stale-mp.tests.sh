#!/usr/bin/env bash
# Behavioral test of the seat launcher's crash cleanup: what seat_fg.sh's port-refusal block and seat_stop.sh's reaping
# section do to a dead generation's leftovers.
#   0.143.0: a crashed generation's LMCache MP server holding the MP HTTP port is cleaned up by the stack's own
#            seat_stop.sh and the start proceeds; a foreign holder is still refused; a LIVE engine is never "cleaned up".
#   now:     the engine workers a dead API server leaves behind (VLLM::EngineCore, VLLM::Worker_*) are reaped even when
#            the MP server is gone and its port is free; the MP server is reaped by process identity (this stack's MP
#            port), SIGTERM escalating to SIGKILL; a worker that survives SIGKILL is named; and none of it reaches a
#            foreign listener or a live engine's workers — proven against stand-ins, and by mutants that break those
#            two guards and must be caught.
# The real code runs, never a copy of its logic: seat_fg.sh between its >>> and <<< marker lines, seat_stop.sh up to its
# "reaping ends here" line (the shared-memory cleanup and VRAM readback after it touch the box and are not run). The
# stand-ins carry the real names: `VLLM::Worker_TP0` as argv[0] (what vLLM's setproctitle sets), a script called
# `lmcache` invoked as `lmcache server --port … --http-port …`, and `vllm serve … --port N`. Scratch ports 28790-28798
# and a systemd unit name that does not exist — except one case that, run as root under a live systemd, starts the MP
# server stand-in in a real transient unit of that name (the shape seat_fg.sh gives the real one) and checks it is
# stopped through the unit. Linux only (ss, pgrep, ps, python3, setsid). The orphan sweep is box-wide by design, so the
# test refuses to run (SKIP) on a box where a real VLLM:: process is alive.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
command -v ss >/dev/null && command -v pgrep >/dev/null && command -v python3 >/dev/null && command -v setsid >/dev/null || { echo "SKIP (needs ss, pgrep, python3, setsid)"; exit 0; }
PORT=28797; OTHER_PORT=28798; MP_PORT=28796; OTHER_MP_PORT=28795; MP_HTTP_PORT=28790
if ps -eo pid,args 2>/dev/null | awk '$2 ~ /^VLLM::/ {found=1} END {exit !found}'; then
  echo "SKIP (a VLLM:: engine process is alive on this box, and the orphan sweep under test is box-wide)"; exit 0
fi
for p in $PORT $OTHER_PORT $MP_PORT $OTHER_MP_PORT $MP_HTTP_PORT; do
  if ss -ltn 2>/dev/null | grep -q ":$p "; then echo "SKIP (scratch port :$p is already in use)"; exit 0; fi
done
T="$(mktemp -d)"
trap 'rc=$?; builtin kill -9 $(cat "$T"/*.pid 2>/dev/null) 2>/dev/null; [ -n "${UNIT_STARTED:-}" ] && systemctl stop "lmcache-mp-scratch-$$" >/dev/null 2>&1; rm -rf "$T"; exit $rc' EXIT
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
# STANDIN_TERM=ignore ignores SIGTERM (a server stuck in its own shutdown); STANDIN_TERM=slow exits 1 s after SIGTERM.
mkdir -p "$T/venv/bin"
cat > "$T/venv/bin/lmcache" <<'EOF'
import os, signal, socket, sys, time
a = sys.argv[1:]
socks = []
for flag in ("--port", "--http-port"):
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("127.0.0.1", int(a[a.index(flag) + 1])))
    s.listen()
    socks.append(s)
mode = os.environ.get("STANDIN_TERM", "")
if mode == "ignore":
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
elif mode == "slow":
    def slow(*_):
        time.sleep(1)
        os._exit(0)
    signal.signal(signal.SIGTERM, slow)
time.sleep(600)
EOF

# ---- stand-ins and helpers -------------------------------------------------------------------------------------------
LISTEN_PY='import socket,time,sys;s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(("127.0.0.1",int(sys.argv[1])));s.listen();time.sleep(300)'
wait_bound() { local i; for i in $(seq 1 50); do ss -ltn 2>/dev/null | grep -q ":$1 " && return 0; sleep 0.1; done; return 1; }
wait_free()  { local i; for i in $(seq 1 50); do ss -ltn 2>/dev/null | grep -q ":$1 " || return 0; sleep 0.1; done; return 1; }
wait_name()  { local i a; for i in $(seq 1 50); do a="$(ps -o args= -p "$1" 2>/dev/null)"; case "$a" in "$2"*) return 0 ;; esac; sleep 0.1; done; return 1; }
# a listener that is nobody's: a plain python process, not named like anything of the stack
foreign() { python3 -c "$LISTEN_PY" "$1" >/dev/null 2>&1 & echo $! > "$T/$2.pid"; disown $!; wait_bound "$1" || echo "test setup: :$1 never bound"; }
# a stand-in for an MP server: name, MP port, MP HTTP port, optional SIGTERM behaviour
mp_standin() {
  STANDIN_TERM="${4:-}" python3 "$T/venv/bin/lmcache" server --host 127.0.0.1 --port "$2" --http-host 127.0.0.1 --http-port "$3" --chunk-size 784 >/dev/null 2>&1 &
  echo $! > "$T/$1.pid"; disown $!; wait_bound "$3" || echo "test setup: :$3 never bound"
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
state() { ps -o stat= -p "$1" 2>/dev/null | tr -d ' '; }
gone() { local s; s="$(state "$(cat "$T/$1.pid")")"; [ -z "$s" ] || [ "${s#Z}" != "$s" ]; }   # exited, or a zombie awaiting its parent
alive() { ! gone "$1"; }
reset() { builtin kill -9 $(cat "$T"/*.pid 2>/dev/null) 2>/dev/null; rm -f "$T"/*.pid; local p; for p in $MP_HTTP_PORT $MP_PORT $OTHER_MP_PORT; do wait_free "$p"; done; sleep 0.2; }
run() { SEAT_MP_PORT_WAIT_SEC="${WAITSEC:-2}" bash "$T/seat_fg.sh" 2>&1; }
has() { grep -q -- "$1" <<<"$out"; }

# 1. a crashed generation's MP server (the stack's own, by name and MP port) holds the MP HTTP port, no engine: reaped
mp_standin stale $MP_PORT $MP_HTTP_PORT; out="$(run)"
if has "running seat_stop.sh once" && has "reaping MP server" && has "START PROCEEDS" && gone stale; then pass "stale MP server is cleaned up and the start proceeds"; else failcase "stale MP server" "$out"; fi
reset

# 1b. the cleanup fails (exit 3) and the port frees 3 s later: the exit code is printed and the start waits for the port
cp "$T/seat_stop.stub.sh" "$T/seat_stop.sh"; foreign $MP_HTTP_PORT stale; out="$(WAITSEC=6 run)"; cp "$T/seat_stop.real.sh" "$T/seat_stop.sh"
if has "seat_stop.sh exited 3" && has "START PROCEEDS"; then pass "a slow release is waited out and the cleanup's exit code is printed"; else failcase "slow release" "$out"; fi
reset

# 2. a FOREIGN holder the stack's cleanup does not own: refused, and left alone
foreign $MP_HTTP_PORT foreign; out="$(run)"
if has "REFUSING to start — the MP HTTP port" && ! has "START PROCEEDS" && alive foreign; then pass "a foreign holder is still refused"; else failcase "foreign holder" "$out"; fi
reset

# 3. a LIVE engine of this stack: no cleanup runs, the MP port holder is refused
foreign $MP_HTTP_PORT held; ( exec -a "vllm serve fake --port $PORT" sleep 300 ) >/dev/null 2>&1 & echo $! > "$T/engine3.pid"; disown $!; wait_name "$!" "vllm serve fake --port $PORT"
out="$(run)"
if ! has "running seat_stop.sh once" && has "REFUSING to start — the MP HTTP port" && alive engine3 && alive held; then pass "a live engine is never cleaned up"; else failcase "live engine" "$out"; fi
reset

# 4. orphaned engine workers, MP port FREE (the MP server died, the workers did not): reaped, the start proceeds
orphan o1 "VLLM::EngineCore"; orphan o2 "VLLM::Worker_TP0"; orphan o3 "VLLM::Worker_TP1"; out="$(run)"
if has "engine processes named VLLM::" && [ "$(grep -c 'reaping orphaned engine process' <<<"$out")" -eq 3 ] && has "START PROCEEDS" && gone o1 && gone o2 && gone o3; then
  pass "orphaned engine workers are reaped with the MP port free and the start proceeds"; else failcase "orphaned workers" "$out"; fi
reset

# 5. the MP server AND the orphaned workers of the dead generation: both reaped in the one cleanup
mp_standin stale5 $MP_PORT $MP_HTTP_PORT; orphan o1 "VLLM::Worker_PP0"; orphan o2 "VLLM::Worker_PP1"; out="$(run)"
if has "reaping MP server" && has "reaping orphaned engine process" && has "START PROCEEDS" && gone stale5 && gone o1 && gone o2; then
  pass "the MP server and the orphaned workers are reaped together"; else failcase "MP server + workers" "$out"; fi
reset

# 6. a LIVE engine on another port has workers of its own: never reaped as orphans
engine other $OTHER_PORT; out="$(run)"
if ! has "reaping orphaned engine process" && has "START PROCEEDS" && alive other && alive other-worker; then
  pass "a live engine's workers on another port are never reaped"; else failcase "live engine's workers" "$out"; fi
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
orphan im "VLLM::Worker_TP0"; IMMORTAL="$(cat "$T/im.pid")"; export IMMORTAL
kill() { if [ "${1:-}" = "-KILL" ] && [ "${2:-}" = "$IMMORTAL" ]; then return 0; fi; builtin kill "$@"; }; export -f kill
out="$(run)"; unset -f kill; unset IMMORTAL
if has "WARN orphaned engine process(es) still alive 3 s after SIGKILL: $(cat "$T/im.pid")(state S" && has "START PROCEEDS" && alive im; then pass "a reaped worker that survives SIGKILL is named"; else failcase "worker surviving SIGKILL" "$out"; fi
reset

# 12. the stop path (llama-swap's cmdStop) stops THIS seat's engine and its whole tree, and nothing of another seat's
engine mine $PORT; engine other12 $OTHER_PORT; out="$(bash "$T/seat_stop.sh" "$T/seat.env" 2>&1)"
if has "scratch seat_stop: reaping done" && gone mine && gone mine-worker && alive other12 && alive other12-worker; then pass "the stop path stops this seat's engine tree and only that"; else failcase "stop path" "$out"; fi
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

[ $fail -eq 0 ] && echo "ALL PASS" || { echo "FAILURES"; exit 1; }
