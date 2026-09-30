#!/usr/bin/env bash
# Behavioral test of seat_fg.sh's port-refusal blocks (0.143.0): a crashed generation's MP server holding the MP
# HTTP port is cleaned up by the stack's own seat_stop.sh and the start proceeds; a foreign holder is still refused;
# a LIVE engine is never "cleaned up". Extracts the real blocks from seat_fg.sh (never a copy of their logic), runs
# them in a scratch dir on unprivileged test ports with a stub seat_stop.sh. Linux only (ss, pgrep, python3).
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
command -v ss >/dev/null && command -v pgrep >/dev/null && command -v python3 >/dev/null || { echo "SKIP (needs ss, pgrep, python3)"; exit 0; }
T="$(mktemp -d)"; trap 'rc=$?; kill $(cat "$T"/*.pid 2>/dev/null) 2>/dev/null; rm -rf "$T"; exit $rc' EXIT
PORT=28797; MP_HTTP_PORT=28790
# The block under test: from the engine-port refusal to the end of the MP-port refusal, verbatim.
awk '/^if ss -ltnp 2>\/dev\/null \| grep -q ":\$PORT "; then/{on=1} on{print} on&&/^fi$/{n++; if(n==2){exit}}' "$HERE/seat_fg.sh" > "$T/block.sh"
grep -q 'seat_stop.sh' "$T/block.sh" || { echo "FAIL: extracted block has no stale-MP recovery"; exit 1; }
cat > "$T/seat_fg.sh" <<EOF
#!/usr/bin/env bash
set -u
PORT=$PORT; MP_HTTP_PORT=$MP_HTTP_PORT; CFG=/dev/null
$(cat "$T/block.sh")
echo "START PROCEEDS"
EOF
# stub seat_stop.sh: kills only the listener recorded as THIS stack's stale MP server. With a `slow` marker it fails
# (exit 3) and the listener goes away only 3 s later — a unit that is slow to release its socket.
cat > "$T/seat_stop.sh" <<'EOF'
#!/usr/bin/env bash
d="$(dirname "$(readlink -f "$0")")"
if [ -f "$d/stale.pid" ]; then
  p="$(cat "$d/stale.pid")"
  if [ -f "$d/slow" ]; then ( sleep 3; kill "$p" ) >/dev/null 2>&1 & echo "stub seat_stop: slow release"; exit 3; fi
  kill "$p" 2>/dev/null
fi
echo "stub seat_stop ran"
EOF
listen() { python3 -c "import socket,time,sys;s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('127.0.0.1',int(sys.argv[1])));s.listen();time.sleep(300)" "$1" & echo $! > "$T/$2.pid"; sleep 0.5; }
fail=0
run() { bash "$T/seat_fg.sh" 2>&1; }

# 1. stale MP server, no engine: recovered, start proceeds
listen $MP_HTTP_PORT stale; out="$(run)"
echo "$out" | grep -q "running seat_stop.sh once" && echo "$out" | grep -q "START PROCEEDS" && echo "PASS stale MP server is cleaned up and the start proceeds" || { echo "FAIL stale: $out"; fail=1; }
rm -f "$T/stale.pid"

# 1b. the cleanup fails and the port frees 3 s later: the exit code is printed and the start waits for the port
touch "$T/slow"; listen $MP_HTTP_PORT stale; out="$(run)"
echo "$out" | grep -q "seat_stop.sh exited 3" && echo "$out" | grep -q "START PROCEEDS" && echo "PASS a slow release is waited out and the cleanup's exit code is printed" || { echo "FAIL slow release: $out"; fail=1; }
rm -f "$T/stale.pid" "$T/slow"; sleep 0.3

# 2. a FOREIGN holder the stack's cleanup does not own: still refused
listen $MP_HTTP_PORT foreign; out="$(run)"
echo "$out" | grep -q "REFUSING to start — the MP HTTP port" && ! echo "$out" | grep -q "START PROCEEDS" && echo "PASS a foreign holder is still refused" || { echo "FAIL foreign: $out"; fail=1; }
kill "$(cat "$T/foreign.pid")" 2>/dev/null; rm -f "$T/foreign.pid"; sleep 0.3

# 3. a LIVE engine of this stack: no cleanup runs, the MP port holder is refused
listen $MP_HTTP_PORT stale2; mv "$T/stale2.pid" "$T/held.pid"
( exec -a "vllm serve fake --port $PORT" sleep 300 ) & echo $! > "$T/engine.pid"; sleep 0.3
out="$(run)"
! echo "$out" | grep -q "running seat_stop.sh once" && echo "$out" | grep -q "REFUSING to start — the MP HTTP port" && echo "PASS a live engine is never cleaned up" || { echo "FAIL live-engine: $out"; fail=1; }

[ $fail -eq 0 ] && echo "ALL PASS" || { echo "FAILURES"; exit 1; }
