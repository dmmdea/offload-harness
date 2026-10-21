#!/usr/bin/env bash
# seat_fg.sh — llama-swap `cmd` for a vLLM seat backed by LMCache MP (reference template).
#
# Runs INSIDE the serving box's Linux (WSL distro or native), in the FOREGROUND of the client llama-swap
# started: when llama-swap ends that client, the session is reaped and vLLM with it — that is the
# swap-out. The LMCache MP server (L1 staging + optional cache-server L2) is a transient systemd unit
# that outlives the engine; seat_stop.sh (llama-swap `cmdStop`) stops both. When the engine CRASHES nothing runs
# cmdStop, so the MP server and any engine worker its API server could not stop are still there at the next start:
# the port-refusal block below runs seat_stop.sh once to reap them (and only them) before it judges the ports.
#
# Layouts on Qwen3.8-27B INT4 (first measured 2026-09-02/03 on vLLM 0.28.0, LMCache 0.5.4):
#   SEAT_TP=2 (two cards, tensor parallel)  — uses a cache server (L2 store) with stock LMCache: 24k-token
#     context back from the store at parity cost, all tokens; MTP possible here.
#   SEAT_PP=3 (three cards, pipeline)       — the stages hold different counts of full-attention layers
#     (7/3/6 for a 28,13,23 split of the 64-layer model, full attention every 4th layer), and stock LMCache
#     sizes L2 reads from one layout per model, so they fail for the ranks that differ. With the per-rank
#     layout overlay on SEAT_LMCACHE_PYTHONPATH (below) the pipeline seat is bound to its own L2 store and
#     serves hits, except the FIRST request after an MP server start, which gets 0 L2 hits and recomputes
#     (open until register-time binding lands). Without the overlay, run it on the same-box RAM tier only
#     (SEAT_L2 empty): 24k back in 0.53 s (26x).
#   fp8 KV at 262,144 context runs on the three-card pipeline seat (hand-wired on the 3-card reference box as opencode's model since 2026-10-04; no tier seeds it); the
#     2026-09-03 "fp8 KV and 262k infeasible" result was the two-card layout on vLLM 0.28. MTP has no
#     pipeline support.
# Every knob is overridden from seat.env (SEAT_* variables) without editing this file. The values
# below are placeholders for a generic box; the operator's real ones live in seat.env, next to the
# harness's config.json `kv_cache_server` block, which is what `offload_status` reports — keep them
# in agreement (the status note says so). Path defaults (env file, log, venv, work directory) derive from the
# directory the scripts sit in, so a copy installed anywhere keeps its own.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
CFG="${1:-${SEAT_ENV:-$HERE/seat.env}}"   # env file as $1 (wsl.exe passes no environment through), else SEAT_ENV, else the default
[ -f "$CFG" ] && . "$CFG"
LOG="${SEAT_LOG:-$HERE/seat.log}"
MODEL="${SEAT_MODEL:-RedHatAI/Qwen3.8-27B-INT4}"
NAME="${SEAT_NAME:-qwen3.8-27b-vllm}"
PORT="${SEAT_PORT:-18797}"
MP_PORT="${SEAT_MP_PORT:-18796}"
# The MP server also opens an HTTP frontend (LMCache 0.5.x: /, cache control, observability). Its upstream default is
# 0.0.0.0:8080 - on a WSL2 distro in mirrored networking that is the HOST's LAN + tailnet, and 8080 is somebody else's port on
# every box in this fleet (measured 2026-09-18: the production MP server logged `Uvicorn running on http://0.0.0.0:8080`).
# Loopback only, on its own port (reference pairing 18797 engine / 18796 MP ZMQ / 18790 MP HTTP — 18790 is a listed safe pick
# on every port file of the fleet; 18793 is <node-b>'s LiteLLM reservation); seat.env overrides it.
MP_HTTP_PORT="${SEAT_MP_HTTP_PORT:-18790}"
# SEAT_L1_GB default 2 for a seat with no measured value (register B-02; RSS ≈ L1 + 1.1 GiB). Measured seats set it in
# seat.env: the pair 8 (2 starved its staging on 2026-09-21), the hand-wired three-card seat 16.
L1_GB="${SEAT_L1_GB:-2}"
CHUNK="${SEAT_CHUNK:-784}"
# The cache server (L2) is OPT-IN: empty = same-box tier only. `${VAR-default}` (no colon) so that
# SEAT_L2= in seat.env is an explicit "off", not a fall-through to a default.
L2="${SEAT_L2-}"
VENV="${SEAT_VENV:-$HERE/vllm-env}"
WORK="${SEAT_WORKDIR:-$HERE}"
# The MP server unit is per seat stack (default lmcache-mp). A scratch/benchmark stack in the same box MUST use
# another unit name and port: on 2026-09-03 a benchmark arm on the seat's port and unit made llama-swap's health
# check pass against the arm, so other sessions' contracts were served by an engine without the tool-call flags,
# and every seat start stopped the arm's MP server in turn. seat_stop.sh reads the same variable.
MP_UNIT="${SEAT_MP_UNIT:-lmcache-mp}"
# SEAT_KV_LOAD_FAILURE_POLICY (optional): vLLM's kv_load_failure_policy for the LMCache connector, "recompute" or
# "fail" (vLLM's own default, used when this is empty). The shipped overlay's patch 05 (LMCache #4709,
# lmcache-patches/) reports the blocks of a failed L2 retrieve to vLLM instead of serving them; vLLM recomputes them
# only under "recompute" and fails the request under "fail". On vLLM 0.28 a hybrid model raises in the scheduler on
# ANY flagged block whatever the policy (vLLM #50388, fixed in 0.30), so "recompute" means something from 0.30 on.
# lmcache-patches/repatch-lmcache-overlay.sh refuses to put 05 into an overlay a seat env names unless that env sets
# "recompute".
KV_POLICY="${SEAT_KV_LOAD_FAILURE_POLICY:-}"
# SEAT_MP_EXTRA_ARGS (optional): extra `lmcache server` arguments, appended after the ones below (e.g.
# "--max-gpu-workers 3"), so an MP-server arm needs no edit here. Empty = none.
MPX=(); [ -n "${SEAT_MP_EXTRA_ARGS:-}" ] && read -r -a MPX <<< "$SEAT_MP_EXTRA_ARGS"

# Everything the engine prints must reach llama-swap's per-model log AND the seat log.
exec > >(tee -a "$LOG") 2>&1
case "$KV_POLICY" in
  ""|recompute|fail) ;;
  *) echo "seat_fg: REFUSING to start — SEAT_KV_LOAD_FAILURE_POLICY=$KV_POLICY (want recompute, fail, or empty)"; exit 1 ;;
esac

# fs_native over a network share (measured 2026-09-04: the <node-c> tmpfs over SMB 3.1.1 recovers a 23.7k-token prefix
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
# degrade_l2 <reason>: the cache server is not usable, so the seat serves the SAME-BOX tier (L1 only)
# instead of refusing to start. 2026-09-09: the write floor refused every start for hours while the share
# crawled at ~36 MB/s, and llama-swap turned each refusal into HTTP 500 — the whole agent lane died over a
# cache ACCELERATOR being slow. A slow share is still never used (that is what the floor is for); the seat
# just runs without it and says so, here and in the file SEAT_L2_STATUS_FILE names (a rendered env names
# seat-l2-<seat id>.status per seat; default $WORK/seat-l2.status) — the readback for health/status.
L2_STATUS="${SEAT_L2_STATUS_FILE:-$WORK/seat-l2.status}"
degrade_l2() {
  echo "seat_fg: CACHE SERVER DEGRADED — $1 — serving the same-box tier (L1 ${L1_GB} GB, no L2). Fix the path and restart the seat to get the cache server back."
  printf 'degraded %s reason=%s\n' "$(date -Is)" "$1" > "$L2_STATUS" 2>/dev/null || true
  L2=""
  L2_DEGRADED=1
}
L2_DEGRADED=0
# Which address of the store host to mount (register B-55). A .local store name is resolved by the system resolver to ONE of the
# addresses the store's mDNS responders answer with, and on the reference store host that is arbitrary: one responder is pinned to
# its wired NIC (the right answer), another agent answers on every interface with that interface's own address (a Wi-Fi address
# on the same /24, container bridges, a VPN address). The mount then landed on the Wi-Fi path (writes ~24 MB/s, so the write
# floor degraded L2), on an unreachable bridge address, or on the VPN address, which refuses the pinned source address: every
# start for three days ran without L2. So, for a cifs mount whose source is //<name>.local/<share> with a pinned source
# address (SEAT_L2_MOUNT_SRCADDR), l2_pick_address asks mDNS itself for ALL the A answers (a raw query to 224.0.0.251:5353 with
# the QU bit, three times in about a second, sent out of the pinned source address) and mounts //<address>/<share> at an
# answer inside the pinned source address's IPv4 network (the prefix is read from `ip -4 -o addr`). Anything else keeps the
# mount by name exactly as before, and the line printed says why: no python, a helper that fails or finds nothing, no answer on
# that network, a prefix that cannot be read. Names that are not .local, nfs mounts and an unset source pin never get here. The
# lookup is bounded: `timeout -s KILL 3` around the helper, so it can never hold a seat start.
# ONE answer inside the network is mounted as it is. SEVERAL inside it (the store's wired and Wi-Fi addresses on one network)
# cannot be told apart by arrival order, and the Wi-Fi path writes about ten times slower, so l2_min_rtt pings each of them from
# the pinned source address (`ping -I <source address> -c 5 -i 0.2 -q -W 1 <address>`, the minimum of the min/avg/max line) and
# the lowest minimum wins; equal minimums go to the earlier answer. On the reference workstation the wired address measured a
# minimum RTT of 0.425-0.477 ms and the Wi-Fi one 0.983-2.288 ms (three runs each, so more than 2x apart on every run), while the
# averages (0.57-0.62 ms and 1.5-26 ms) are much noisier, which is why the minimum is what is compared. A candidate whose ping
# fails is shown as "no reply" and is never chosen over one that replied. When ping is missing, no candidate replies or no
# round-trip time can be read, the choice is what it was before this tie-break, the first answer inside the network by arrival
# order, and the line printed says which rule chose and lists each candidate's result. The pings run one after the other, each
# under `timeout -s KILL 3`: at most 3 s per candidate inside the network, so they can never hold a seat start either.
# The limit: the round-trip time is a heuristic for the better path, not a throughput measurement. A store whose fast path has a
# higher latency than its slow path would be picked wrong, and a store whose fast path drops ICMP loses to a slow path that
# answers it. SEAT_L2_MOUNT_SRC overrides all of it: name the wired address there, either as a RESERVED or static address of
# the store (an IPv4 literal is mounted as given; a DHCP lease is the 2026-09-04 outage above) or as a name that is not .local
# and that this box resolves to it (such a name is not looked up, so nothing is pinged either; a .local name pinned in
# /etc/hosts is NOT honoured here, because the lookup asks mDNS directly).
# The address is chosen when the share is MOUNTED, which is once per WSL session. A start that finds the share already mounted
# does not mount again, does not unmount (another seat may be using it, and a new pick at every start could trade a good mount
# on one network for the Wi-Fi one) and asks no mDNS: l2_report_mounted says where the share is (the source and the server
# address, read with findmnt) and how to choose again, and the status file records that address. A share that was mounted at
# the wrong address stays there until it is unmounted (`umount <dir>` while no seat uses it) and the seat is started again.
# Three seams exist for the test (seat_fg.kv-and-store.tests.sh) and nothing else sets them: SEAT_FG_TEST_MDNS_CMD names a command
# that stands in for the lookup (it is called as `<cmd> <name> <source address>` and prints one IPv4 answer per line),
# SEAT_FG_TEST_MDNS_DEST (host:port) moves the helper's query off the mDNS group onto a test responder, and
# SEAT_FG_TEST_PING_CMD names a command that stands in for ping (it is called as `<cmd> <source address> <address>`, under the
# same `timeout -s KILL 3`, and prints the minimum round-trip time in ms, or exits non-zero for no reply).
L2_ADDR=""          # the address the share was mounted at when the launcher chose one: status file and degrade messages
L2_SEEN_ADDR=""     # the server address of a share that was ALREADY mounted when this start found it (read from the mount)
L2_SEEN_NOTE=""     # and the sentence the degrade messages append for such a share
MOUNT_SRC="${SEAT_L2_MOUNT_SRC:-}"
l2_mdns_answers() {   # $1 = the .local name, $2 = the source address; prints each distinct IPv4 answer, in arrival order
  local py=""
  if [ -n "${SEAT_FG_TEST_MDNS_CMD:-}" ]; then
    timeout -s KILL 3 "$SEAT_FG_TEST_MDNS_CMD" "$1" "$2"; return
  fi
  if [ -x "$VENV/bin/python" ]; then py="$VENV/bin/python"; else py="$(command -v python3 2>/dev/null)"; fi
  [ -n "$py" ] || return 127
  timeout -s KILL 3 "$py" - "$1" "$2" <<'PYEOF'
import os, random, socket, struct, sys, time

def encode(name):
    return b"".join(bytes([len(p)]) + p.encode("ascii") for p in name.split(".")) + b"\0"

def read_name(buf, off):
    labels, end, hops = [], None, 0
    while True:
        n = buf[off]
        if n == 0:
            off += 1
            break
        if n & 0xC0 == 0xC0:
            if end is None:
                end = off + 2
            off = ((n & 0x3F) << 8) | buf[off + 1]
            hops += 1
            if hops > 16:
                raise ValueError("compression loop")
            continue
        labels.append(buf[off + 1:off + 1 + n].decode("ascii", "replace").lower())
        off += 1 + n
    return ".".join(labels), (off if end is None else end)

def a_answers(buf, name):
    flags, qd, an, ns, ar = struct.unpack(">HHHHH", buf[2:12])
    if not flags & 0x8000:
        return []
    off, found = 12, []
    for _ in range(qd):
        off = read_name(buf, off)[1] + 4
    for _ in range(an + ns + ar):
        owner, off = read_name(buf, off)
        rtype, _cls, _ttl, rdlen = struct.unpack(">HHIH", buf[off:off + 10])
        off += 10
        if rtype == 1 and rdlen == 4 and owner == name:
            found.append(socket.inet_ntoa(buf[off:off + 4]))
        off += rdlen
    return found

def main():
    name = sys.argv[1].rstrip(".").lower()
    src = sys.argv[2] if len(sys.argv) > 2 else ""
    host, _, port = os.environ.get("SEAT_FG_TEST_MDNS_DEST", "224.0.0.251:5353").rpartition(":")
    dest = (host, int(port))
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    if src:
        try:
            sock.bind((src, 0))
            sock.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_IF, socket.inet_aton(src))
        except OSError:
            pass
    try:
        sock.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_TTL, 255)
    except OSError:
        pass
    # id, flags 0, one question: <name> type A, class IN with the QU bit (0x8001): answer me by unicast
    query = struct.pack(">HHHHHH", random.randrange(1, 65536), 0, 1, 0, 0, 0) + encode(name) + struct.pack(">HH", 1, 0x8001)
    start = time.monotonic()
    sends = [0.0, 0.3, 0.6]
    seen = []
    while True:
        elapsed = time.monotonic() - start
        if elapsed >= 1.0:
            break
        while sends and elapsed >= sends[0]:
            sends.pop(0)
            try:
                sock.sendto(query, dest)
            except OSError:
                pass
        wait = (sends[0] if sends else 1.0) - elapsed
        sock.settimeout(max(0.01, min(wait, 1.0 - elapsed)))
        try:
            data = sock.recv(9000)
        except socket.timeout:
            continue
        except OSError:
            time.sleep(0.05)
            continue
        try:
            for a in a_answers(data, name):
                if a not in seen:
                    seen.append(a)
        except (IndexError, ValueError, struct.error, UnicodeError):
            pass
    for a in seen:
        print(a)
    if not seen:
        print("no mDNS A answer for " + name, file=sys.stderr)
    return 0 if seen else 1

try:
    sys.exit(main())
except Exception as e:
    print("mDNS helper failed: %s" % e, file=sys.stderr)
    sys.exit(2)
PYEOF
}
l2_is_ipv4() {
  local o
  [[ "$1" =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$ ]] || return 1
  for o in 1 2 3 4; do [ $(( 10#${BASH_REMATCH[$o]} )) -le 255 ] || return 1; done
}
l2_ip2int() { local a b c d; IFS=. read -r a b c d <<< "$1"; echo $(( (10#$a << 24) | (10#$b << 16) | (10#$c << 8) | 10#$d )); }
l2_ms_to_us() {   # $1 = milliseconds written as 9 or 0.425 or 0.4256: prints whole microseconds, so two times compare as integers
  local i f
  i="${1%%.*}"; f="${1#"$i"}"; f="${f#.}000"
  echo $(( 10#$i * 1000 + 10#${f:0:3} ))
}
l2_min_rtt() {   # $1 = the source address, $2 = an address: prints the minimum of five pings from the source, in ms
  # Returns 0 with the time printed, 1 for no reply (ping failed or was cut off), 2 when ping ended well but no time can be read
  # from its output, 127 when there is no ping command. Bounded by `timeout -s KILL 3`, so it can never hold a seat start.
  local out rc=0 rtt="" re='min/avg/max[^=]*=[[:space:]]*([0-9]+(\.[0-9]+)?)/'
  if [ -n "${SEAT_FG_TEST_PING_CMD:-}" ]; then
    out="$(timeout -s KILL 3 "$SEAT_FG_TEST_PING_CMD" "$1" "$2" 2>/dev/null)" || rc=$?
  else
    out="$(LC_ALL=C timeout -s KILL 3 ping -I "$1" -c 5 -i 0.2 -q -W 1 "$2" 2>/dev/null)" || rc=$?
  fi
  case "$rc" in 0) ;; 127) return 127 ;; *) return 1 ;; esac
  if [ -n "${SEAT_FG_TEST_PING_CMD:-}" ]; then rtt="${out//[[:space:]]/}"
  elif [[ "$out" =~ $re ]]; then rtt="${BASH_REMATCH[1]}"; fi
  [[ "$rtt" =~ ^[0-9]+(\.[0-9]+)?$ ]] || return 2
  printf '%s' "$rtt"
}
l2_local_name() {   # $1 = a mount source: prints <name> when it is //<name>.local/<share>, returns 1 for anything else
  local rest host
  rest="${1#//}"; host="${rest%%/*}"
  [ "$rest" != "$host" ] || return 1                 # not the //host/share shape
  case "$host" in *:*) return 1 ;; esac              # an explicit port is not ours to rewrite
  case "${host,,}" in *.local) ;; *) return 1 ;; esac
  printf '%s' "$host"
}
l2_report_mounted() {   # the share was already mounted: say where it is and leave it alone; sets L2_SEEN_ADDR and L2_SEEN_NOTE
  local name src opts a="" hint=""
  name="$(l2_local_name "$SEAT_L2_MOUNT_SRC")" || return 0
  read -r src opts <<< "$(findmnt -n -o SOURCE,OPTIONS -M "$SEAT_L2_MOUNT_DIR" 2>/dev/null)"
  if [[ ",${opts:-}," =~ ,addr=([0-9.]+), ]]; then a="${BASH_REMATCH[1]}"; fi
  case "${src:-}" in ""|"//$name"/*) hint="; to choose it again, unmount the share while no seat uses it (umount $SEAT_L2_MOUNT_DIR) and restart the seat" ;; esac
  L2_SEEN_ADDR="$a"
  L2_SEEN_NOTE=" [the share was already mounted${a:+ at server address $a}; a start does not choose the address again]"
  echo "seat_fg: cache-server share $SEAT_L2_MOUNT_DIR is already mounted${src:+ from $src}${a:+ (server address $a)} — the store address is chosen only when the share is mounted, so it was not looked up again${hint}"
}
l2_pick_address() {   # sets MOUNT_SRC and L2_ADDR when it chooses; prints one seat_fg: line either way
  local rest name share out rc=0 plen mask net ans chosen="" why="" got=() dup g inside=() rule list m r us best="" best_us=0 nok=0 nmiss=0 nunread=0 fb
  name="$(l2_local_name "$SEAT_L2_MOUNT_SRC")" || return 0
  rest="${SEAT_L2_MOUNT_SRC#//}"; share="${rest#*/}"
  out="$(l2_mdns_answers "$name" "$SRCADDR" 2>/dev/null)" || rc=$?
  case "$rc" in
    0) ;;
    1) why="no mDNS answer within about 1 s" ;;
    124|137) why="the mDNS lookup timed out (bound 3 s)" ;;
    127) why="no python interpreter for the mDNS lookup (neither $VENV/bin/python nor python3)" ;;
    *) why="the mDNS lookup failed (exit $rc)" ;;
  esac
  if [ -z "$why" ]; then
    while IFS= read -r ans; do
      l2_is_ipv4 "$ans" || continue
      dup=0; for g in "${got[@]}"; do [ "$g" = "$ans" ] && dup=1; done
      [ "$dup" -eq 1 ] || got+=("$ans")
    done <<< "$out"
    [ "${#got[@]}" -gt 0 ] || why="the mDNS lookup answered nothing usable"
  fi
  if [ -z "$why" ]; then
    plen="$(ip -4 -o addr show 2>/dev/null | awk -v a="$SRCADDR" '{split($4, x, "/"); if (x[1] == a) { print x[2]; exit }}')"
    if ! l2_is_ipv4 "$SRCADDR"; then why="the source address '$SRCADDR' is not an IPv4 address"
    else case "$plen" in ''|*[!0-9]*) why="cannot read the prefix of $SRCADDR from ip -4 -o addr" ;; *) [ "$plen" -le 32 ] || why="cannot read the prefix of $SRCADDR from ip -4 -o addr" ;; esac
    fi
  fi
  if [ -n "$why" ]; then
    echo "seat_fg: cache-server store ${name}: ${why}${got[*]:+ (mDNS answers: ${got[*]})} — mounting by name as before"
    return 0
  fi
  mask=$(( (0xFFFFFFFF << (32 - 10#$plen)) & 0xFFFFFFFF )); net=$(( $(l2_ip2int "$SRCADDR") & mask ))
  for ans in "${got[@]}"; do
    if [ $(( $(l2_ip2int "$ans") & mask )) -eq "$net" ]; then inside+=("$ans"); fi
  done
  if [ "${#inside[@]}" -eq 0 ]; then
    echo "seat_fg: cache-server store ${name}: mDNS answers: ${got[*]}, none inside ${SRCADDR}/${plen} — mounting by name as before"
    return 0
  fi
  chosen="${inside[0]}"
  if [ "${#inside[@]}" -eq 1 ]; then
    echo "seat_fg: cache-server store ${name}: mDNS answers: ${got[*]} — chose ${chosen}, the first inside ${SRCADDR}/${plen} (the pinned source address's network); mounting //${chosen}/${share} instead of //${name}/${share}"
  else
    # Several answers on the pinned network: the one with the lowest minimum round-trip time from the pinned source address.
    # A candidate that did not reply has no time and is never compared; the strict < keeps the earlier answer on a tie.
    for ans in "${inside[@]}"; do
      m="$(l2_min_rtt "$SRCADDR" "$ans")"; r=$?
      case "$r" in
        0) list="${list:+$list }${ans} (min RTT ${m} ms)"; nok=$(( nok + 1 )); us="$(l2_ms_to_us "$m")"
           if [ -z "$best" ] || [ "$us" -lt "$best_us" ]; then best="$ans"; best_us="$us"; fi ;;
        2) list="${list:+$list }${ans} (RTT unreadable)"; nunread=$(( nunread + 1 )) ;;
        127) list="${list:+$list }${ans} (not measured)"; nmiss=$(( nmiss + 1 )) ;;
        *) list="${list:+$list }${ans} (no reply)" ;;
      esac
    done
    if [ "$nok" -gt 1 ]; then rule="the lowest round-trip time"; chosen="$best"
    elif [ "$nok" -eq 1 ]; then rule="the only candidate that replied to ping"; chosen="$best"
    else
      if [ "$nmiss" -eq "${#inside[@]}" ]; then fb="ping is not available on this box"
      elif [ "$nunread" -gt 0 ]; then fb="no round-trip time could be read from ping's output"
      else fb="no candidate replied to ping"; fi
      rule="the first inside the network in arrival order (round-trip times could not be compared: ${fb})"
    fi
    echo "seat_fg: cache-server store ${name}: mDNS answers: ${got[*]}, inside ${SRCADDR}/${plen}: ${list}; chose ${chosen}, ${rule}; mounting //${chosen}/${share} instead of //${name}/${share}"
  fi
  MOUNT_SRC="//${chosen}/${share}"; L2_ADDR="$chosen"
}
if [ -n "${SEAT_L2_MOUNT_SRC:-}" ] && [ -n "${SEAT_L2_MOUNT_DIR:-}" ]; then
  mkdir -p "$SEAT_L2_MOUNT_DIR"
  # SEAT_L2_MOUNT_SRCADDR: pin the CIFS client's source address. On a box with two NICs on one subnet (wired +
  # Wi-Fi) the mount was measured leaving from the Wi-Fi address even while `ip route get` chose the wire, and a store
  # whose allow-list names only the wired address refused it — the seat then ran without its cache server for that
  # whole start. "auto" = the IPv4 of the lowest-metric default-route interface; or give an explicit address.
  MOUNT_OPTS="${SEAT_L2_MOUNT_OPTS:-}"; SRCADDR=""
  case "${SEAT_L2_MOUNT_SRCADDR:-}" in
    "") ;;
    auto)
      dev="$(ip -4 route show default 2>/dev/null | awk '{m=1e9; for(i=1;i<NF;i++) if($i=="metric") m=$(i+1); print m, $0}' | sort -n | head -1 | awk '{for(i=1;i<NF;i++) if($i=="dev") print $(i+1)}')"
      [ -n "$dev" ] && SRCADDR="$(ip -4 -o addr show dev "$dev" 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -1)" ;;
    *) SRCADDR="$SEAT_L2_MOUNT_SRCADDR" ;;
  esac
  if [ -n "$SRCADDR" ] && [ "${SEAT_L2_MOUNT_TYPE:-cifs}" = cifs ]; then
    MOUNT_OPTS="${MOUNT_OPTS:+$MOUNT_OPTS,}srcaddr=$SRCADDR"; echo "seat_fg: cache-server mount source address pinned to $SRCADDR"
  fi
  if ! mountpoint -q "$SEAT_L2_MOUNT_DIR"; then
    if [ -n "$SRCADDR" ] && [ "${SEAT_L2_MOUNT_TYPE:-cifs}" = cifs ]; then
      l2_pick_address
    fi
    if ! timeout 30 mount -t "${SEAT_L2_MOUNT_TYPE:-cifs}" "$MOUNT_SRC" "$SEAT_L2_MOUNT_DIR" -o "$MOUNT_OPTS"; then
      # Say WHY, in the terms the operator can act on: name resolution, reachability, or the share itself.
      host="${SEAT_L2_MOUNT_SRC#//}"; host="${host%%:*}"; host="${host%%/*}"
      case "${SEAT_L2_MOUNT_TYPE:-cifs}" in nfs|nfs4) port=2049 ;; *) port=445 ;; esac
      case "$host" in
        *[!0-9.]*)
          if [ -n "$L2_ADDR" ]; then resolved="$L2_ADDR"                                         # mounted at the address mDNS gave: probe that one
          else resolved="$(getent hosts "$host" 2>/dev/null | awk '{print $1; exit}')"; fi ;;    # a name: resolve it
        *)         resolved="$host" ;;                                                          # an IPv4 literal: probe it as is
      esac
      if [ -z "$resolved" ]; then
        why="'$host' does not resolve on this box (use a tailnet MagicDNS or static name, not a DHCP address)"
      elif ! timeout 3 bash -c "</dev/tcp/$resolved/$port" 2>/dev/null; then
        why="'$host' = $resolved, port $port unreachable (store down, wrong interface, or the address moved)"
      else
        why="'$host' = $resolved answers on $port — check credentials, the share name, and the store's allow-list (source address ${SRCADDR:-unpinned; set SEAT_L2_MOUNT_SRCADDR=auto})"
      fi
      degrade_l2 "share $SEAT_L2_MOUNT_SRC did not mount at $SEAT_L2_MOUNT_DIR: $why${L2_ADDR:+ [the mount was tried at the mDNS-chosen address $L2_ADDR, not by name]}"
    fi
  elif [ -n "$SRCADDR" ] && [ "${SEAT_L2_MOUNT_TYPE:-cifs}" = cifs ]; then
    l2_report_mounted
  fi
fi
if [ "$L2_DEGRADED" -eq 0 ] && [ -n "${SEAT_L2_MOUNT_SRC:-}" ] && [ -n "${SEAT_L2_MOUNT_DIR:-}" ]; then
  echo "seat_fg: cache-server share mounted: $(df -h "$SEAT_L2_MOUNT_DIR" | tail -1)"
  # Prune the persistent store to its cap (0.113.12). LMCache's fs_native L2 eviction controller starts fresh with every MP
  # server and accounts only for the pages THAT instance writes; pages left by earlier instances are never counted or
  # evicted, so a store that survives seat restarts grows past max_capacity_gb to the filesystem limit — measured
  # 2026-09-05: 832 files / 40 GB on a 40 GB tmpfs (100 %, 192 MB free) with max_capacity_gb 38, new pages then fail to land
  # and the tier stops paying while reads of old pages still hit. SEAT_L2_PRUNE_GB (default 0 = off) deletes the OLDEST
  # files under SEAT_L2_PRUNE_DIR (default: the adapter's base_path, if SEAT_L2 names one) until the directory is below
  # the cap. Pages are a recomputable cache; nothing else lives in that directory.
  PRUNE_GB="${SEAT_L2_PRUNE_GB:-0}"
  PRUNE_DIR="${SEAT_L2_PRUNE_DIR:-$(printf '%s' "${SEAT_L2-}" | sed -n 's/.*"base_path"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')}"
  if [ "$PRUNE_GB" -gt 0 ] 2>/dev/null && [ -n "$PRUNE_DIR" ] && [ -d "$PRUNE_DIR" ]; then
    used_kb=$(du -sk "$PRUNE_DIR" 2>/dev/null | cut -f1); cap_kb=$(( PRUNE_GB * 1024 * 1024 )); removed=0; freed_kb=0
    if [ "${used_kb:-0}" -gt "$cap_kb" ]; then
      while IFS= read -r f; do
        [ "$used_kb" -le "$cap_kb" ] && break
        sz=$(( ( $(stat -c %s "$f" 2>/dev/null || echo 0) + 1023 ) / 1024 ))
        rm -f -- "$f" && { used_kb=$(( used_kb - sz )); freed_kb=$(( freed_kb + sz )); removed=$(( removed + 1 )); }
      done < <(find "$PRUNE_DIR" -type f -printf '%T@ %p\n' 2>/dev/null | sort -n | cut -d' ' -f2-)
      echo "seat_fg: cache-server store pruned: removed $removed oldest file(s), $(( freed_kb / 1024 )) MiB, now $(( used_kb / 1024 )) MiB of a $(( cap_kb / 1024 )) MiB cap ($PRUNE_DIR)"
    else
      echo "seat_fg: cache-server store $(( ${used_kb:-0} / 1024 )) MiB under the $(( cap_kb / 1024 )) MiB cap ($PRUNE_DIR)"
    fi
  fi
  MIN_MBPS="${SEAT_L2_MIN_MBPS:-0}"
  if [ "$MIN_MBPS" -gt 0 ] 2>/dev/null; then
    probe="$SEAT_L2_MOUNT_DIR/.seat_fg-write-probe.$$"
    t0=$(date +%s%N)
    if timeout 120 dd if=/dev/zero of="$probe" bs=4M count=16 conv=fsync status=none 2>/dev/null; then
      t1=$(date +%s%N); rm -f "$probe"
      mbps=$(( 67109 / ( (t1 - t0) / 1000000 + 1 ) ))   # 64 MiB = 67,108,864 bytes → decimal MB/s over elapsed ms
      if [ "$mbps" -lt "$MIN_MBPS" ]; then
        degrade_l2 "share writes at ~${mbps} MB/s, below SEAT_L2_MIN_MBPS=${MIN_MBPS} (the tier would be slower than recompute)${L2_ADDR:+ [the share is mounted at the mDNS-chosen address $L2_ADDR]}${L2_SEEN_NOTE}"
      else
        echo "seat_fg: cache-server share write probe ~${mbps} MB/s (floor ${MIN_MBPS})"
      fi
    else
      rm -f "$probe"
      degrade_l2 "cannot write a 64 MiB probe to the share at $SEAT_L2_MOUNT_DIR within 120 s${L2_SEEN_NOTE}"
    fi
  fi
  if [ "$L2_DEGRADED" -eq 0 ]; then
    L2_ON="${L2_ADDR:-$L2_SEEN_ADDR}"     # the address the share is on: chosen by this start, or read from the mount that was already there
    printf 'ok %s mbps=%s%s\n' "$(date -Is)" "${mbps:-unmeasured}" "${L2_ON:+ addr=$L2_ON}" > "$L2_STATUS" 2>/dev/null || true
  fi
fi

# >>> port refusals and the dead generation's cleanup: seat_fg.stale-mp.tests.sh runs everything between this line and the matching <<< line
# Refuse to start on a port something else already owns. vLLM would load for ~2 minutes and then die on
# "Address already in use" while llama-swap's health check passes against the FOREIGN listener — the seat is
# then "ready" and serving somebody else's engine. Fail here, at once, and name the squatter.
if ss -ltnp 2>/dev/null | grep -q ":$PORT "; then
  echo "seat_fg: REFUSING to start — :$PORT is already bound: $(ss -ltnp 2>/dev/null | grep ":$PORT " | grep -oE 'users:\(.*\)' | head -1)"
  exit 1
fi
# A CRASHED generation leaves its own processes behind. When the engine dies (EngineDeadError, a worker that stopped
# answering) llama-swap restarts the seat through this script, and nothing ran seat_stop.sh: the dead generation's MP
# server still held its HTTP port and every restart refused right here (2026-09-29: 23 minutes with the agent seat
# fully down, 59 failed starts, 75 HTTP 500s), and the engine workers its API server could not stop kept the cards.
# No engine of THIS stack serves :$PORT (checked just above, and by process here), so what of the stack's own is still
# around belongs to the dead generation: run the stack's own cleanup once, then look again. Three kinds of leftover start
# it: a listener on the MP HTTP port, an `lmcache server` of this stack's MP port, and an ORPHANED engine process — named
# VLLM::* (found by the start of its command line: `pgrep -f VLLM::` also matches any wrapper whose arguments mention the
# name) with no live vLLM API server (`vllm serve`, or `python -m vllm.entrypoints.…`) among its ancestors. That is
# seat_stop.sh's rule, and the definition below is a copy of its (a Go test keeps the two identical): a sibling seat's
# live workers are not leftovers, and a healthy start must not log a crash or run a stop. The rule reads the process
# tree; it does not prove ownership, and an engine started so that it has no such ancestor (a script that builds the
# engine in-process) would look orphaned. seat_stop.sh reaps by such rules and by this stack's MP port and unit, never
# by what holds a port, so anything still holding the MP HTTP port after it is foreign and is refused exactly as before.
# A stopping unit can hold its socket for a few seconds, so the re-check waits up to SEAT_MP_PORT_WAIT_SEC (default 10;
# a decimal number of seconds, 0 = judge at once, anything else is said and means 10) for the port to free instead of
# judging one look. This is the only cleanup the launcher owns: the script becomes `vllm serve` (exec, below), so nothing
# of it survives a crash to clean up after it — the Windows stub's crash exit runs the same cleanup through the stop
# task, and there is no watchdog and no proactive relaunch anywhere.
MP_PORT_WAIT="${SEAT_MP_PORT_WAIT_SEC:-10}"
case "$MP_PORT_WAIT" in *[!0-9]*) echo "seat_fg: WARN SEAT_MP_PORT_WAIT_SEC='$MP_PORT_WAIT' is not a number of seconds; using 10"; MP_PORT_WAIT=10 ;; esac
MP_PORT_WAIT=$((10#$MP_PORT_WAIT))
has_api_ancestor() { local q="$1" n=0; while [ "$q" -gt 1 ] 2>/dev/null && [ $n -lt 32 ]; do
  if ps -o args= -p "$q" 2>/dev/null | grep -Eq 'vllm serve|vllm\.entrypoints\.'; then return 0; fi
  q=$(ps -o ppid= -p "$q" 2>/dev/null | tr -d ' '); n=$((n+1)); [ -z "$q" ] && break; done; return 1; }
if ! pgrep -f "vllm serve .*--port $PORT" >/dev/null 2>&1; then
  left=""
  ss -ltnp 2>/dev/null | grep -q ":$MP_HTTP_PORT " && left="$left a listener on the MP HTTP port :$MP_HTTP_PORT;"
  pgrep -f "lmcache server .*--port $MP_PORT( |\$)" >/dev/null 2>&1 && left="$left an MP server on :$MP_PORT;"
  orphans=""
  for ep in $(ps -eo pid,args 2>/dev/null | awk '$2 ~ /^VLLM::/ {print $1}'); do has_api_ancestor "$ep" || orphans="$orphans $ep"; done
  [ -n "$orphans" ] && left="$left engine processes named VLLM::* with no live engine above them (pids$orphans);"
  if [ -n "$left" ]; then
    echo "seat_fg: no engine serves :$PORT and the previous generation may have left something of this stack:${left} — running seat_stop.sh once (it reaps engine processes with no live engine above them and this stack's own MP server, never a port's holder)"
    # Attached: this script's stdout already is the seat log, so the stop must not append to it a second time. The env file
    # is passed when there is one; a default path that is not there is no argument, not a wrong one (the stop refuses a NAMED
    # env file that is missing).
    stop_args=(); [ -f "$CFG" ] && stop_args=("$CFG")
    stop_rc=0
    SEAT_STOP_ATTACHED=1 bash "$(dirname "$(readlink -f "$0")")/seat_stop.sh" "${stop_args[@]}" || stop_rc=$?
    [ "$stop_rc" -eq 0 ] || echo "seat_fg: seat_stop.sh exited $stop_rc (continuing to the re-check)"
    for _ in $(seq 1 $(( MP_PORT_WAIT * 2 ))); do
      ss -ltnp 2>/dev/null | grep -q ":$MP_HTTP_PORT " || break
      sleep 0.5
    done
  fi
fi
if ss -ltnp 2>/dev/null | grep -q ":$MP_HTTP_PORT "; then
  echo "seat_fg: REFUSING to start — the MP HTTP port :$MP_HTTP_PORT is already bound: $(ss -ltnp 2>/dev/null | grep ":$MP_HTTP_PORT " | grep -oE 'users:\(.*\)' | head -1)"
  exit 1
fi
# <<< port refusals and the dead generation's cleanup

# Chat template precheck. A --chat-template in SEAT_EXTRA_ARGS names a file; vLLM (0.29 validate_chat_template) refuses a
# path-like value that does not exist, but only after this wrapper has stopped the old MP unit, waited out the VRAM
# precheck and started a new MP server, and the reason is buried in a traceback while llama-swap answers the lane with
# HTTP 500. A rendered seat names a shipped template at <seat dir>/templates/<name>: the renderer emits templates/<name>
# beside the env, and BOTH must be copied into the distro's seat directory. Refuse at once, before the MP server is
# touched, and name the file.
CT_ARGS=()
[ -n "${SEAT_EXTRA_ARGS:-}" ] && read -r -a CT_ARGS <<< "$SEAT_EXTRA_ARGS"
for ((ct_i = 0; ct_i < ${#CT_ARGS[@]}; ct_i++)); do
  case "${CT_ARGS[$ct_i]}" in
    --chat-template)   ct_path="${CT_ARGS[$((ct_i + 1))]:-}" ;;
    --chat-template=*) ct_path="${CT_ARGS[$ct_i]#--chat-template=}" ;;
    *) continue ;;
  esac
  case "$ct_path" in
    /*) if [ ! -r "$ct_path" ]; then
          echo "seat_fg: REFUSING to start — --chat-template $ct_path does not exist (copy the rendered templates/ directory into $(dirname "$(dirname "$ct_path")") beside the seat env)"
          exit 1
        fi ;;
    "") echo "seat_fg: REFUSING to start — --chat-template has no value in SEAT_EXTRA_ARGS"; exit 1 ;;
  esac
done

# One MP server per engine start, with THIS start's settings (a reused unit keeps stale L1/chunk/L2).
# The store keeps its pages; only the staging buffer is rebuilt.
systemctl stop "$MP_UNIT" 2>/dev/null; systemctl reset-failed "$MP_UNIT" 2>/dev/null
# VRAM precheck (0.113.9). A start that follows a swap-out by a few seconds can find the seat's cards still
# holding the previous engine's memory (or a co-resident seat); vLLM then sizes its KV pool against a smaller
# card and refuses ("… KV cache is needed, which is larger than the available KV cache memory") — measured
# 2026-09-04: two cold loads in a row failed at util 0.90 on cards that gate clean 10/10, both ~12 s after the
# previous engine died; 30 s later the same cards read 0 MiB. Wait up to SEAT_VRAM_WAIT_SEC (default 60) for
# every seat device to drop below its floor, and name the holders if it does not.
# A warning, never a refusal: the engine's own error is the final word.
# ORDER (2026-09-22): this runs AFTER the old MP server unit is stopped and BEFORE the new one is started. It used
# to run after the start, so it measured the NEW MP server's own CUDA contexts and burned the full wait on every
# start. PER-DEVICE FLOOR: SEAT_VRAM_FLOOR_MIB_<index> (the CUDA index as listed in SEAT_DEVICES) wins over
# SEAT_VRAM_FLOOR_MIB (default 1024) — a display card never drops below the desktop's own 2-4 GB, so one global
# floor either never clears there or is too loose to catch a previous engine on the other cards.
DEVS="${SEAT_DEVICES:-0,1}"; VWAIT="${SEAT_VRAM_WAIT_SEC:-60}"; VFLOOR="${SEAT_VRAM_FLOOR_MIB:-1024}"
if command -v nvidia-smi >/dev/null 2>&1 && [ "$VWAIT" -gt 0 ] 2>/dev/null; then
  deadline=$(( $(date +%s) + VWAIT )); busy=""
  while :; do
    busy=""
    for d in ${DEVS//,/ }; do
      vf_var="SEAT_VRAM_FLOOR_MIB_$d"; vf="${!vf_var:-$VFLOOR}"   # per-device floor, else the global one
      used="$(CUDA_DEVICE_ORDER=PCI_BUS_ID nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits -i "$d" 2>/dev/null | head -1 | tr -d ' ')"
      if [ -n "$used" ] && [ "$used" -gt "$vf" ] 2>/dev/null; then busy="$busy dev$d=${used}MiB>${vf}"; fi
    done
    [ -z "$busy" ] && break
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "seat_fg: WARNING — seat devices still hold VRAM above their floor after ${VWAIT} s:$busy — holders: $(nvidia-smi --query-compute-apps=gpu_uuid,pid,process_name,used_memory --format=csv,noheader 2>/dev/null | tr '\n' ';')"
      break
    fi
    sleep 2
  done
  [ -z "$busy" ] && echo "seat_fg: seat devices $DEVS below their VRAM floor (default ${VFLOOR} MiB) — VRAM clear"
fi
# WSL2 GPU-paravirtualization hazards (information, never a refusal). `make_resident: Ioctl failed: -12` (Windows refused
# to make an allocation resident: the GPU's residency budget, not the card's free memory) and `reserve_gpu_va … -75` are
# what a WSL2 engine start hits when it asks for more than the host will back; CUDA then reports "out of memory" at an
# unrelated call (measured 2026-09-23: vLLM 0.30 died in LMCache's IPC export, `_share_cuda_`, with 16-22 of these per
# failed start, while the next 0.28 start on the same distro logged none and served). What matters is what appeared
# SINCE THE PREVIOUS START — the count is kept in $WORK/.dxg-failures so an old failure is not reported forever. The
# recurring `query_adapter_info: Ioctl failed: -2` and dxgvmbus FORTIFY lines are noise and are not counted.
if grep -qi microsoft /proc/version 2>/dev/null; then
  DXG_RE="make_resident: Ioctl failed: -12|reserve_gpu_va.*-75"
  dxg_all=$(dmesg 2>/dev/null | grep -cE "$DXG_RE"); DXG_FILE="$WORK/.dxg-failures"
  dxg_prev=$(cat "$DXG_FILE" 2>/dev/null); case "$dxg_prev" in ''|*[!0-9]*) dxg_prev=0 ;; esac
  [ "${dxg_all:-0}" -lt "$((10#$dxg_prev))" ] && dxg_prev=0   # fewer than last time: the distro restarted
  dxg_new=$(( ${dxg_all:-0} - 10#$dxg_prev ))
  if [ "$dxg_new" -gt 0 ]; then
    echo "seat_fg: WARNING — $dxg_new new dxg residency/VA failure(s) in the kernel log since the previous seat start ($dxg_all since the distro started; last: $(dmesg 2>/dev/null | grep -E "$DXG_RE" | tail -1 | cut -c1-120)). If THAT start died with a CUDA out-of-memory, this is why: the host refused residency."
  elif [ "${dxg_all:-0}" -gt 0 ]; then
    echo "seat_fg: note — $dxg_all older dxg residency/VA failure(s) in the kernel log, none since the previous seat start"
  fi
  echo "${dxg_all:-0}" > "$DXG_FILE" 2>/dev/null || true
fi
# L2 store failures of this unit's PREVIOUS generation (information, never a refusal). A store that fails is a silent
# miss for every later lookup of those chunks, and nothing else surfaces the count: it sits in the MP log as
# `Store task N to adapter 0 failed` warnings. The offset of the log at each start is kept per unit, so the count
# covers the lines written since this unit's previous start (another stack's unit appending to the same log is
# counted too; the units rarely run together).
MP_LOG="$WORK/lmcache-mp.log"; MP_OFF_FILE="$WORK/.$MP_UNIT.log-offset"
if [ -f "$MP_LOG" ] && [ -f "$MP_OFF_FILE" ]; then
  mp_off=$(cat "$MP_OFF_FILE" 2>/dev/null); mp_size=$(stat -c %s "$MP_LOG" 2>/dev/null || echo 0)
  # Digits only, read in base 10: a hand-edited or corrupt file ("-5", "089") would otherwise reach $(( )) as octal
  # or `tail -c +-4` and drop the note without a word. A log shorter than the offset was rotated: skip the count.
  case "$mp_off" in ''|*[!0-9]*) mp_off="" ;; esac
  if [ -n "$mp_off" ] && [ "$(( 10#$mp_off ))" -le "$mp_size" ]; then
    st_fail=$(tail -c +$(( 10#$mp_off + 1 )) "$MP_LOG" | grep -a -cE "Store task [0-9]+ to adapter [0-9]+ failed")
    [ "${st_fail:-0}" -gt 0 ] && echo "seat_fg: note — the previous $MP_UNIT generation logged $st_fail failed L2 store task(s) (\"Store task … failed\" in $MP_LOG); those chunks miss on every later lookup"
  fi
fi
stat -c %s "$MP_LOG" > "$MP_OFF_FILE" 2>/dev/null || echo 0 > "$MP_OFF_FILE"
L2ARG=(); [ -n "$L2" ] && L2ARG=(--l2-adapter "$L2")
# SEAT_LMCACHE_PYTHONPATH (optional): a directory prepended to PYTHONPATH so the MP server (and the engine, below) import
# LMCache from an overlay instead of the installed package — used to run an unreleased upstream fix without touching the
# venv (0.113.13). Empty = the installed package, unchanged. Both the server and the engine must load the SAME LMCache, so
# the value is injected in both places. `-E PYTHONPATH=""` would put the CWD on the path, so the flag is added only when set.
PP_ENV=(); [ -n "${SEAT_LMCACHE_PYTHONPATH:-}" ] && PP_ENV=(-E PYTHONPATH="$SEAT_LMCACHE_PYTHONPATH")
if ! systemd-run --unit="$MP_UNIT" --collect --working-directory="$WORK" -p TimeoutStopSec=20 \
    -p StandardOutput=append:"$WORK/lmcache-mp.log" -p StandardError=append:"$WORK/lmcache-mp.log" \
    -E CUDA_DEVICE_ORDER=PCI_BUS_ID -E HOME=/root -E LMCACHE_DISABLE_BANNER=1 -E LMCACHE_LOG_LEVEL=INFO \
    -E PATH="$VENV/bin:/usr/local/bin:/usr/bin:/bin" "${PP_ENV[@]}" \
    "$VENV/bin/lmcache" server --host 127.0.0.1 --port "$MP_PORT" --http-host 127.0.0.1 --http-port "$MP_HTTP_PORT" --chunk-size "$CHUNK" \
      --separate-object-groups --l1-size-gb "$L1_GB" --eviction-policy LRU --supported-transfer-mode auto "${L2ARG[@]}" "${MPX[@]}"; then
  echo "seat_fg: $MP_UNIT failed to start (systemd-run); see $WORK/lmcache-mp.log"; exit 1
fi
[ -n "${SEAT_LMCACHE_PYTHONPATH:-}" ] && echo "seat_fg: LMCache overlay ON: PYTHONPATH=$SEAT_LMCACHE_PYTHONPATH (MP server + engine)"
[ ${#MPX[@]} -gt 0 ] && echo "seat_fg: MP server extra args (SEAT_MP_EXTRA_ARGS): ${MPX[*]}"
for i in $(seq 1 60); do
  ss -ltn 2>/dev/null | grep -q ":$MP_PORT " && break
  systemctl is-active --quiet "$MP_UNIT" || { echo "seat_fg: $MP_UNIT died during start; see $WORK/lmcache-mp.log"; exit 1; }
  sleep 1
done
ss -ltn 2>/dev/null | grep -q ":$MP_PORT " || { echo "seat_fg: lmcache-mp never bound :$MP_PORT in 60 s"; exit 1; }
if [ -n "$L2" ]; then
  echo "seat_fg: cache server (L2) ON: $L2 — verify vllm:external_prefix_cache_hits > 0 after the first eviction; a size-mismatch warning in $WORK/lmcache-mp.log means the tier is serving nothing"
else
  if [ "$L2_DEGRADED" -eq 1 ]; then
    echo "seat_fg: same-box tier only (L1 ${L1_GB} GB) — DEGRADED from the cache-server tier this start (see $L2_STATUS)"
  else
    echo "seat_fg: same-box tier only (L1 ${L1_GB} GB), no cache server"
  fi
fi

export CUDA_DEVICE_ORDER=PCI_BUS_ID CUDA_VISIBLE_DEVICES="${SEAT_DEVICES:-0,1}" NCCL_CUMEM_ENABLE=0 NCCL_P2P_DISABLE=1 HF_HUB_OFFLINE=1
export HOME=/root HF_HOME="${SEAT_HF_HOME:-$WORK/hf}" VLLM_CACHE_ROOT="${SEAT_VLLM_CACHE:-$WORK/vllm-cache}" LMCACHE_LOG_LEVEL=INFO
# Humming (ADR 0095), the JIT-compiled GEMM library vLLM ships for quantized weights, compiles a layer's kernels on its first
# forward and keys each one on the venv path. Its cache sits beside vLLM's compile cache, so it survives the five-minute idle
# unload and is cleaned with the seat, not left in root's home. A layer shape's tile variants compile together (eight on the
# 3-bit seat), so eight build workers add no wait, where Humming's default of one per CPU loads every core of a desktop box.
export HUMMING_CACHE_DIR="${SEAT_HUMMING_CACHE:-$WORK/humming-cache}" HUMMING_TMP_DIR="${SEAT_HUMMING_TMP:-$WORK/humming-tmp}"
export HUMMING_MAX_PARALLEL_BUILD_WORKERS="${SEAT_HUMMING_BUILD_WORKERS:-8}"
echo "seat_fg: Humming kernel cache $HUMMING_CACHE_DIR (at most $HUMMING_MAX_PARALLEL_BUILD_WORKERS parallel builds)"
export PATH="$VENV/bin:/usr/local/bin:/usr/bin:/bin"
# WSL2 + vLLM >= 0.29: 0.29 selects its V2 GPU model runner by default and that runner needs UVA (pinned host memory),
# which vLLM reports unavailable under WSL2; with VLLM_WSL2_ENABLE_PIN_MEMORY=1 it starts and then dies in kernel warm-up
# (CUDA error: invalid device ordinal, measured 2026-09-17 on a 5060 Ti). The V1 runner is the one that serves there, so a
# WSL2 launch of a >= 0.29 venv pins it unless the env file already chose. 0.28 is NOT touched: it picks its runner per
# configuration and its V2 runner runs on WSL2 (the 2026-09-04 TP2 DFlash spec-decode arms logged `gpu_worker.py:396]
# Using V2 Model Runner` and served), while the production seats serve on its V1 runner (corrected 2026-09-23: the V2
# lines once credited to the production pair seat are `gpu_worker.py:429`, a 0.29.0 line). Native Linux is untouched
# at any version (a native 0.29 box runs the V2 runner). The version is read from the venv's dist-info name: importing
# vllm costs seconds.
if grep -qi microsoft /proc/version 2>/dev/null; then
  vllm_di="$(ls -d "$VENV"/lib/python3*/site-packages/vllm-*.dist-info 2>/dev/null | head -1)"
  vllm_ver="${vllm_di##*/vllm-}"; vllm_ver="${vllm_ver%.dist-info}"; vllm_major="${vllm_ver%%.*}"; vllm_rest="${vllm_ver#*.}"; vllm_minor="${vllm_rest%%.*}"
  case "$vllm_major$vllm_minor" in *[!0-9]*|"") vllm_major=0; vllm_minor=0;; esac
  if [ "$vllm_major" -gt 0 ] || [ "$vllm_minor" -ge 29 ]; then
    export VLLM_USE_V2_MODEL_RUNNER="${VLLM_USE_V2_MODEL_RUNNER:-0}"
    echo "seat_fg: WSL2 + vLLM ${vllm_ver} — V1 model runner pinned (VLLM_USE_V2_MODEL_RUNNER=${VLLM_USE_V2_MODEL_RUNNER})"
  fi
fi
# Same overlay as the MP server above (0.113.13): the engine's LMCache connector must match the server's, or store/retrieve
# uses two different serializers. Prepend, never replace, so a caller-set PYTHONPATH survives.
[ -n "${SEAT_LMCACHE_PYTHONPATH:-}" ] && export PYTHONPATH="$SEAT_LMCACHE_PYTHONPATH${PYTHONPATH:+:$PYTHONPATH}"
PAR=()
if [ -n "${SEAT_PP:-}" ]; then
  export VLLM_PP_LAYER_PARTITION="${SEAT_PARTITION:-}"
  PAR=(--pipeline-parallel-size "$SEAT_PP")
else
  PAR=(--tensor-parallel-size "${SEAT_TP:-2}")
fi
EXTRA=()
[ -n "${SEAT_EXTRA_ARGS:-}" ] && read -r -a EXTRA <<< "$SEAT_EXTRA_ARGS"   # e.g. MTP on a tp2 seat
# KV pool PINNED FROM FREE MEMORY (item 3 "card-0 headroom", 2026-09-07). Off unless SEAT_KV_HEADROOM_GIB is set.
# --gpu-memory-utilization budgets a FRACTION OF THE CARD whatever the co-residents hold, and the profiler lands the same
# config at 183k or 191k tokens on different starts (c32 301 vs 230 t/s — a per-start coin flip); worse, the utility seats
# that share card 0 grow 0.7–1.3 GB by day, so a 0.90 start that fit at launch stalled the engine at noon (nvlddmkm 153,
# 2026-09-06). With the knob set the pool is computed here, deterministically, from what is ACTUALLY free on the tighter
# seat card at launch: free − SEAT_NONKV_GIB (the engine's own weights + non-torch + peak activation per worker, read
# from the profiler's banner: "Actual usage is X GiB … Y GiB for peak activation" → X+Y) − SEAT_KV_HEADROOM_GIB (what
# must STAY free for the co-residents' growth), floored at SEAT_KV_FLOOR_GIB (a smaller pool is still a seat) and capped
# at SEAT_KV_CAP_GIB (never more than a profiled start could have taken), passed as --kv-cache-memory-bytes (per worker;
# vLLM then ignores gpu-memory-utilization). The banner line names every input so a stall can be read back to its cause.
# A pool the floor raised can still be too small for SEAT_MAX_LEN (register B-54): a card holding ~2.2 GB of desktop graphics
# memory gave 1.95 GiB, floored to 2.0, and vLLM 0.28 refused every start ("To serve at least one request with the model's max
# seq len (163840), (2.66 GiB KV cache is needed, which is larger than the available KV cache memory (2.0 GiB)") while
# llama-swap restarted the seat on each request. SEAT_KV_NEED_GIB (optional) is that "N GiB KV cache is needed" figure for
# SEAT_MAX_LEN, rounded UP past the second decimal (vLLM prints two, to nearest) and measured again whenever SEAT_MAX_LEN, the
# parallelism, MTP or the model changes; with it set and the pinned pool below it, --max-model-len -1 is passed instead of SEAT_MAX_LEN, and vLLM 0.28
# (vllm/v1/core/kv_cache_utils.py _auto_fit_max_model_len, fed the --kv-cache-memory-bytes value) picks the largest length that
# fits the PINNED pool, never more than the model's own maximum. Unset, SEAT_MAX_LEN is always passed (an auto-fit could exceed the operator's length) and a
# floored pool is only warned about.
MAXLEN="${SEAT_MAX_LEN:-131072}"
KVPIN=()
# nvidia-smi is resolved HERE, not trusted from PATH: the launcher exports a minimal PATH above, and under WSL the binary lives
# in /usr/lib/wsl/lib — the first pinned start skipped this whole block in silence because `command -v` came back empty.
NVSMI="$(command -v nvidia-smi 2>/dev/null)"; [ -z "$NVSMI" ] && [ -x /usr/lib/wsl/lib/nvidia-smi ] && NVSMI=/usr/lib/wsl/lib/nvidia-smi
if [ -n "${SEAT_KV_HEADROOM_GIB:-}" ] && [ -z "$NVSMI" ]; then
  echo "seat_fg: WARNING — SEAT_KV_HEADROOM_GIB=${SEAT_KV_HEADROOM_GIB} is set but nvidia-smi was not found (PATH=$PATH, no /usr/lib/wsl/lib/nvidia-smi); falling back to --gpu-memory-utilization ${SEAT_UTIL:-0.88} — the pool is NOT pinned"
fi
if [ -n "${SEAT_KV_HEADROOM_GIB:-}" ] && [ -n "$NVSMI" ]; then
  nonkv="${SEAT_NONKV_GIB:-11.19}"; kvfloor="${SEAT_KV_FLOOR_GIB:-2.0}"; kvcap="${SEAT_KV_CAP_GIB:-3.4}"
  minfree=""
  for d in ${DEVS//,/ }; do
    used="$(CUDA_DEVICE_ORDER=PCI_BUS_ID "$NVSMI" --query-gpu=memory.used --format=csv,noheader,nounits -i "$d" 2>/dev/null | head -1 | tr -d ' ')"
    total="$(CUDA_DEVICE_ORDER=PCI_BUS_ID "$NVSMI" --query-gpu=memory.total --format=csv,noheader,nounits -i "$d" 2>/dev/null | head -1 | tr -d ' ')"
    if [ -n "$used" ] && [ -n "$total" ]; then
      free=$(( total - used ))
      if [ -z "$minfree" ] || [ "$free" -lt "$minfree" ]; then minfree=$free; fi
    fi
  done
  if [ -n "$minfree" ]; then
    # "pool floored raw": the pool as passed, 1 when the computed value was under the floor, and that computed value.
    pool_calc="$(awk -v f="$minfree" -v n="$nonkv" -v h="$SEAT_KV_HEADROOM_GIB" -v fl="$kvfloor" -v c="$kvcap" 'BEGIN{p=f/1024-n-h; r=p; fd=0; if(p<fl){p=fl; fd=1}; if(p>c)p=c; printf "%.3f %d %.3f", p, fd, r}')"
    read -r pool_gib pool_floored pool_raw <<< "$pool_calc"
    pool_bytes="$(awk -v p="$pool_gib" 'BEGIN{printf "%d", p*1073741824}')"
    KVPIN=(--kv-cache-memory-bytes "$pool_bytes")
    echo "seat_fg: KV pool PINNED at ${pool_gib} GiB/worker = min free ${minfree} MiB on devices ${DEVS} − non-KV ${nonkv} GiB − headroom ${SEAT_KV_HEADROOM_GIB} GiB (floor ${kvfloor}, cap ${kvcap}) → --kv-cache-memory-bytes ${pool_bytes}; --gpu-memory-utilization ${SEAT_UTIL:-0.88} is ignored by vLLM while the pin is set"
    # SEAT_KV_NEED_GIB: a decimal number of GiB (digits with at most one point, none at either end); anything else is said and
    # means unset.
    kvneed="${SEAT_KV_NEED_GIB:-}"
    case "$kvneed" in
      "") ;;
      *[!0-9.]*|.*|*.|*.*.*) echo "seat_fg: WARNING — SEAT_KV_NEED_GIB='${kvneed}' is not a number of GiB (digits with one optional decimal point); ignored"; kvneed="" ;;
    esac
    if [ -n "$kvneed" ] && awk -v p="$pool_gib" -v n="$kvneed" 'BEGIN{exit !(p+0 < n+0)}'; then
      MAXLEN=-1
      echo "seat_fg: KV pool ${pool_gib} GiB/worker is below SEAT_KV_NEED_GIB=${kvneed} GiB (what SEAT_MAX_LEN=${SEAT_MAX_LEN:-131072} tokens need) — passing --max-model-len -1 instead: vLLM auto-fits the context to the pinned pool, so this seat serves LESS than SEAT_MAX_LEN (vLLM logs the length it chose: \"Auto-fit max_model_len\")"
    elif [ -z "$kvneed" ] && [ "${pool_floored:-0}" = 1 ]; then
      echo "seat_fg: WARNING — the KV pool was FLOORED: free memory allows ${pool_raw} GiB/worker, raised to SEAT_KV_FLOOR_GIB=${kvfloor} GiB. vLLM refuses to start when that is below what SEAT_MAX_LEN=${SEAT_MAX_LEN:-131072} needs (it prints \"N GiB KV cache is needed, which is larger than the available KV cache memory\"). Set SEAT_KV_NEED_GIB=N (that figure) and the launcher passes --max-model-len -1 whenever the pool is below it; unset, SEAT_MAX_LEN is kept"
    fi
  else
    echo "seat_fg: WARNING — SEAT_KV_HEADROOM_GIB set but $NVSMI answered nothing for devices ${DEVS}; falling back to --gpu-memory-utilization ${SEAT_UTIL:-0.88} — the pool is NOT pinned"
  fi
fi
KV_POLICY_JSON=""; [ -n "$KV_POLICY" ] && KV_POLICY_JSON=",\"kv_load_failure_policy\":\"$KV_POLICY\""
[ -n "$KV_POLICY" ] && echo "seat_fg: kv_load_failure_policy=$KV_POLICY (SEAT_KV_LOAD_FAILURE_POLICY)"
# Host RAM the seat holds, logged once per start on ONE line (register B-56 / H-08). The seat's host memory was declared nowhere
# and bounded by nothing: the VM that carries it (vmmem on WSL2), the pinned L1 staging buffer (SEAT_L1_GB) and about 1.1 GiB of
# process overhead on top (B-27: RSS = L1 + ~1.1 GiB), and on 2026-10-18 the host's commit reached 168 of 171.7 GiB. The figure is
# the resident set of the MP server unit and its children, summed from ps (shared pages are counted once per process, so it
# is an upper bound for a tree that maps the same memory twice). It is read HERE, after the MP server is up and before the exec,
# because this script becomes `vllm serve` on the next line and nothing of it survives to log later: no loop, no sampler, no
# new process. So the engine's own resident set is not in it, and the line says so; the loaded seat's whole figure is what
# `agent_seat_host_ram_gib` states (doctor warns while it is unset). Never a refusal, never a wait; an unreadable figure is said.
rss_kib=0
rss_walk() { local p k; for p in "$@"; do k="$(ps -o rss= -p "$p" 2>/dev/null | tr -d ' ')"
  case "$k" in ''|*[!0-9]*) ;; *) rss_kib=$(( rss_kib + 10#$k )) ;; esac
  rss_walk $(pgrep -P "$p" 2>/dev/null); done; }
mp_pid="$(systemctl show -p MainPID --value "$MP_UNIT" 2>/dev/null | head -1)"
case "$mp_pid" in ''|*[!0-9]*|0) mp_pid="$(pgrep -f "lmcache server .*--port $MP_PORT( |\$)" 2>/dev/null | head -1)" ;; esac
if [ -n "$mp_pid" ] && [ "$mp_pid" -gt 1 ] 2>/dev/null; then
  rss_walk "$mp_pid"
  echo "seat_fg: host rss $(awk -v k="$rss_kib" 'BEGIN{printf "%.1f", k/1048576}') GiB (L1 ${L1_GB} GiB) — the MP server unit $MP_UNIT and its children at seat start; the engine is not loaded yet and this script becomes vllm serve, so its resident set is not in this figure"
else
  echo "seat_fg: host rss not read — the MP server unit $MP_UNIT has no main pid (L1 ${L1_GB} GiB)"
fi
# Page-cache bound on WSL2 (register B-56, operator rule 2026-10-18: no swapping, no out-of-memory). vLLM reads the checkpoint
# through the guest page cache and keeps none of it mapped once the weights are on the cards, so the cache is a second copy in
# host RAM, and on WSL2 guest page cache is VM memory the host cannot take back until the guest frees it. Measured on the
# 3-card reference box 2026-10-18: loading the INT4 27B pair seat moved host commit 76.6 -> 150 of 171.7 GiB, 23 GiB of it guest
# page cache; one drop took vmmem 66.1 -> 44.3 GiB and host commit 149 -> 127 GiB with the seat serving. The earlier 168 GiB
# peak and a 42 GiB cache read on 10-15 are the same growth over hours. So a watcher (a subshell started before the exec, which
# keeps this PID for the engine) drops clean page cache once the engine answers /health and again whenever the guest's cache
# passes SEAT_CACHE_CAP_GIB, until the engine exits. drop_caches=1 frees clean cache only (no dirty data, no process memory) and
# needs root, which a WSL seat has. Native Linux is left alone: there the cache is reclaimable kernel memory, not host commit.
SEAT_PID=$$
if grep -qi microsoft /proc/sys/kernel/osrelease 2>/dev/null && [ -w /proc/sys/vm/drop_caches ]; then
  ( cap_kib=$(( ${SEAT_CACHE_CAP_GIB:-4} * 1048576 )); ready=0
    while kill -0 "$SEAT_PID" 2>/dev/null; do
      sleep 30
      if [ "$ready" = 0 ]; then
        "$VENV/bin/python" -c "import urllib.request,sys; urllib.request.urlopen('http://127.0.0.1:$PORT/health', timeout=5)" 2>/dev/null || continue
        ready=1; force=1
      fi
      c=$(awk '/^Cached:/{print $2}' /proc/meminfo)
      if [ "${force:-0}" = 1 ] || [ "${c:-0}" -gt "$cap_kib" ]; then
        sync; echo 1 > /proc/sys/vm/drop_caches
        echo "seat_fg: page cache dropped ($(( ${c:-0} / 1048576 )) GiB -> $(awk '/^Cached:/{print int($2/1048576)}' /proc/meminfo) GiB; cap ${SEAT_CACHE_CAP_GIB:-4} GiB)"
        force=0
      fi
    done ) &
fi
exec "$VENV/bin/vllm" serve "$MODEL" \
  --host 127.0.0.1 --port "$PORT" --served-model-name "$NAME" "${SEAT_ALIAS:-agent-pool}" \
  --max-model-len "$MAXLEN" "${PAR[@]}" --gpu-memory-utilization "${SEAT_UTIL:-0.88}" \
  --max-num-seqs "${SEAT_SEQS:-32}" \
  --mamba-cache-mode align --enable-prefix-caching --max-num-batched-tokens "${SEAT_BATCHED:-1567}" "${KVPIN[@]}" \
  --kv-transfer-config "{\"kv_connector\":\"LMCacheMPConnector\",\"kv_role\":\"kv_both\"$KV_POLICY_JSON,\"kv_connector_extra_config\":{\"lmcache.mp.server_urls\":\"127.0.0.1:$MP_PORT\"}}" \
  "${EXTRA[@]}"
