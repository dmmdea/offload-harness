#!/usr/bin/env bash
# Behavioral test of two start-time decisions in seat_fg.sh, the seat launcher. The REAL launcher runs, start to finish, from a
# scratch copy in a scratch seat directory, and the test reads what it did: the argument list it exec'd `vllm serve` with, the
# `mount` command line it ran, the lines it printed and the status file it wrote.
#   B-54  the KV pool pin can never serve SEAT_MAX_LEN. A card that holds the desktop's own graphics memory gives a pool
#         under what SEAT_MAX_LEN needs (floored to SEAT_KV_FLOOR_GIB), vLLM refuses ("To serve at least one request with the
#         model's max seq len ..., N GiB KV cache is needed, which is larger than the available KV cache memory"), and
#         llama-swap restarts the seat on every request. SEAT_KV_NEED_GIB (the figure vLLM prints) lets the launcher pass
#         --max-model-len -1 instead (vLLM auto-fits the context to the pinned pool) when the pool is below it.
#   B-55  the cache-server share mounted at a random address of the store host. The name resolver returns ONE of the several
#         addresses that the store's mDNS responders answer with (its wired NIC, its Wi-Fi NIC, container bridges, a VPN);
#         the launcher now collects all the answers, takes the first one inside the pinned source address's IPv4 network and
#         mounts that address, and mounts by name exactly as before when it cannot.
# Every command that would touch the box is a stand-in (systemctl, systemd-run, ss, pgrep, ps, mountpoint, mount, ip, getent,
# dmesg, nvidia-smi, vllm, the mDNS helper): the MP server's systemd unit is never stopped or started, nothing is mounted,
# no GPU is asked, no engine starts, and nothing binds a port. The unit name is a scratch one as well. Each run is bounded by
# `timeout`, so a launcher that hangs fails this test instead of holding it. Linux only (bash, awk, timeout, tee, python3 for
# the embedded-helper cases). No lock: nothing here is shared with another run.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
[ "$(uname -s)" = Linux ] || { echo "SKIP (Linux only)"; exit 0; }
for c in awk timeout tee env mktemp cat grep sed seq; do
  command -v "$c" >/dev/null || { echo "SKIP (needs $c)"; exit 0; }
done
bash -n "$HERE/seat_fg.sh" || { echo "FAIL: seat_fg.sh does not parse"; exit 1; }
T="$(mktemp -d)"
BGPIDS=""
trap 'rc=$?; [ -n "$BGPIDS" ] && builtin kill -9 $BGPIDS 2>/dev/null; rm -rf "$T"; exit $rc' EXIT
fail=0
PROBLEMS=""

# ---- stand-ins -------------------------------------------------------------------------------------------------------------
mkdir -p "$T/bin" "$T/venv/bin"
stub() { printf '%s\n' "#!/usr/bin/env bash" "$2" > "$T/$1"; chmod +x "$T/$1"; }
stub bin/systemctl 'exit 0'
stub bin/systemd-run 'exit 0'
stub bin/pgrep 'exit 1'
stub bin/ps 'exit 0'
stub bin/mountpoint 'exit 1'
stub bin/dmesg 'exit 0'
# One listener line, the scratch MP port, so the launcher's wait for the MP server passes and none of its refusals do.
stub bin/ss 'echo "LISTEN 0 4096 127.0.0.1:48796 0.0.0.0:*"'
stub bin/mount 'printf "%s\n" "$@" > "$SEAT_TEST_DIR/mount.args"
rc="$(cat "$SEAT_TEST_DIR/mount.rc" 2>/dev/null || echo 0)"
[ "$rc" -ne 0 ] && echo "mount error(113): No route to host" >&2
exit "$rc"'
# The box has two NICs on one /24 (wired metric 100, Wi-Fi metric 600) and a container bridge on another network.
stub bin/ip 'case "$*" in
  "-4 route show default") echo "default via 192.0.2.1 dev eth0 proto dhcp src 192.0.2.10 metric 100"; echo "default via 192.0.2.1 dev wlan0 proto dhcp src 192.0.2.11 metric 600" ;;
  "-4 -o addr show dev eth0") echo "2: eth0    inet 192.0.2.10/24 brd 192.0.2.255 scope global eth0\       valid_lft forever preferred_lft forever" ;;
  "-4 -o addr show dev wlan0") echo "3: wlan0    inet 192.0.2.11/24 brd 192.0.2.255 scope global wlan0\       valid_lft forever preferred_lft forever" ;;
  "-4 -o addr show")
    echo "1: lo    inet 127.0.0.1/8 scope host lo\       valid_lft forever preferred_lft forever"
    echo "2: eth0    inet 192.0.2.10/24 brd 192.0.2.255 scope global eth0\       valid_lft forever preferred_lft forever"
    echo "3: wlan0    inet 192.0.2.11/24 brd 192.0.2.255 scope global wlan0\       valid_lft forever preferred_lft forever"
    echo "4: docker0    inet 198.51.100.1/24 brd 198.51.100.255 scope global docker0\       valid_lft forever preferred_lft forever" ;;
esac
exit 0'
# What the system resolver says for the store name: ONE address, and not necessarily the right one.
stub bin/getent 'case "$1 $2" in "hosts store.local") echo "198.51.100.200   store.local" ;; *) exit 2 ;; esac'
# The test-only mDNS seam: SEAT_FG_TEST_MDNS_CMD names this stand-in, which prints the scripted answers (or hangs, or fails).
stub bin/mdns-stub 'printf "%s\n" "$@" > "$SEAT_TEST_DIR/mdns.argv"
[ "$(cat "$SEAT_TEST_DIR/mdns.mode" 2>/dev/null)" = hang ] && exec sleep 600
cat "$SEAT_TEST_DIR/mdns.answers" 2>/dev/null
exit "$(cat "$SEAT_TEST_DIR/mdns.rc" 2>/dev/null || echo 0)"'
# nvidia-smi and vllm live in the scratch venv: the launcher resets PATH to start with $VENV/bin before it looks for them.
stub venv/bin/nvidia-smi 'case "$*" in
  *memory.used*) echo "${SEAT_TEST_USED_MIB:-0}" ;;
  *memory.total*) echo "${SEAT_TEST_TOTAL_MIB:-16311}" ;;
esac'
stub venv/bin/vllm 'printf "%s\n" "$@" > "$SEAT_TEST_DIR/vllm.args"
echo "stand-in vllm exec ed"'

# ---- the runner --------------------------------------------------------------------------------------------------------------
# run_seat <name> [KEY=VALUE ...]: the extra lines go into the seat env after the defaults (a later assignment wins; @R@ is the
# run's directory). Per-run inputs are variables set for the call: USED (MiB the card already holds), MDNS_ANSWERS (lines the
# mDNS stand-in prints), MDNS_RC, MDNS_MODE (hang), MOUNT_RC, NO_MDNS_SEAM (1 = leave the test seam unset, the production
# shape) and EXTRA_ENV (more NAME=value words for the launcher's environment). Leaves $R (the run's directory), $RC (the
# launcher's exit status) and $ELAPSED (seconds) behind.
run_seat() {
  local name="$1"; shift
  R="$T/$name"; rm -rf "$R"; mkdir -p "$R/seat" "$R/work" "$R/mnt"
  cp "$HERE/seat_fg.sh" "$R/seat/seat_fg.sh"
  {
    printf '%s\n' "SEAT_LOG=$R/seat.log" "SEAT_WORKDIR=$R/work" "SEAT_VENV=$T/venv" "SEAT_MODEL=scratch/model" \
      "SEAT_PORT=48797" "SEAT_MP_PORT=48796" "SEAT_MP_HTTP_PORT=48790" "SEAT_MP_UNIT=lmcache-mp-scratch-$$" \
      "SEAT_MP_PORT_WAIT_SEC=0" "SEAT_DEVICES=0" "SEAT_TP=1" "SEAT_VRAM_WAIT_SEC=0" "SEAT_MAX_LEN=163840" \
      "SEAT_L2_STATUS_FILE=$R/seat-l2.status"
    printf '%s\n' "${@//@R@/$R}"
  } > "$R/seat.env"
  printf '%s\n' "${MDNS_ANSWERS:-}" > "$R/mdns.answers"
  echo "${MDNS_RC:-0}" > "$R/mdns.rc"; echo "${MDNS_MODE:-normal}" > "$R/mdns.mode"; echo "${MOUNT_RC:-0}" > "$R/mount.rc"
  local seam=(); [ -z "${NO_MDNS_SEAM:-}" ] && seam=(SEAT_FG_TEST_MDNS_CMD="$T/bin/mdns-stub")
  local t0 t1
  t0=$(date +%s.%N)
  timeout -s KILL 90 env -i PATH="$T/bin:/usr/local/bin:/usr/bin:/bin" HOME="$R" LANG=C TERM=dumb \
    SEAT_TEST_DIR="$R" SEAT_TEST_USED_MIB="${USED:-0}" SEAT_TEST_TOTAL_MIB=16311 "${seam[@]}" ${EXTRA_ENV:-} \
    bash "$R/seat/seat_fg.sh" "$R/seat.env" 2>&1 | cat > "$R/out"
  RC="${PIPESTATUS[0]}"
  t1=$(date +%s.%N)
  ELAPSED="$(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.1f", b-a}')"
}

# ---- assertions: each adds to PROBLEMS, a case ends with verdict ------------------------------------------------------------
has()   { grep -qF -- "$1" "$R/out" || PROBLEMS="$PROBLEMS [output lacks: $1]"; }
lacks() { grep -qF -- "$1" "$R/out" && PROBLEMS="$PROBLEMS [output has: $1]"; return 0; }
# The value that follows a flag in the argument list the launcher exec'd vllm with (one argument per line in vllm.args).
argval() { awk -v k="$1" 'prev==k {print; exit} {prev=$0}' "$R/vllm.args" 2>/dev/null; }
argcount() { grep -cxF -- "$1" "$R/vllm.args" 2>/dev/null; }
maxlen_is() {
  local got; got="$(argval --max-model-len)"
  [ "$got" = "$1" ] || PROBLEMS="$PROBLEMS [--max-model-len is '${got:-<absent>}', want '$1']"
  [ "$(argcount --max-model-len)" = 1 ] || PROBLEMS="$PROBLEMS [--max-model-len appears $(argcount --max-model-len) times]"
}
launched() { [ "$RC" = 0 ] && [ -s "$R/vllm.args" ] || PROBLEMS="$PROBLEMS [the launcher did not reach vllm serve (exit $RC)]"; }
verdict() {
  if [ -z "$PROBLEMS" ]; then
    echo "PASS $1"
    [ -n "${VERBOSE:-}" ] && { echo "    ---- launcher output:"; sed 's/^/    | /' "$R/out"; echo "    ---- mount: $(tr '\n' ' ' < "$R/mount.args" 2>/dev/null)"; }
  else
    echo "FAIL $1:$PROBLEMS"; echo "    ---- launcher output:"; sed 's/^/    | /' "$R/out"; echo "    ---- vllm argument list:"; sed 's/^/    | /' "$R/vllm.args" 2>/dev/null
    fail=1
  fi
  PROBLEMS=""
}

# The argument that follows a flag in a file of one-argument-per-line (the mount command line, the vllm argument list).
argafter() { awk -v k="$2" 'prev==k {print; exit} {prev=$0}' "$1" 2>/dev/null; }
mountsrc() { argafter "$R/mount.args" cifs; }
mountopts() { argafter "$R/mount.args" -o; }
status_line() { head -1 "$R/seat-l2.status" 2>/dev/null; }
quick() { awk -v e="$1" -v m="$2" 'BEGIN{exit !(e < m)}'; }

# ---- B-54: the KV pool pin and the context length --------------------------------------------------------------------------
# Card: 16311 MiB. Pool = (16311 - USED)/1024 - non-KV 11.19 - headroom 0.5, floored at 2.0, capped at 3.4:
#   USED=2344 -> 1.950 -> floored to 2.000   USED=1800 -> 2.481   USED=0 -> 4.24 -> capped to 3.400
KV=(SEAT_KV_HEADROOM_GIB=0.5 SEAT_NONKV_GIB=11.19 SEAT_KV_FLOOR_GIB=2.0 SEAT_KV_CAP_GIB=3.4)

USED=2344 run_seat k1 "${KV[@]}" SEAT_KV_NEED_GIB=2.66
launched; maxlen_is -1
has "KV pool PINNED at 2.000 GiB/worker"; [ "$(argval --kv-cache-memory-bytes)" = 2147483648 ] || PROBLEMS="$PROBLEMS [the pin is not 2147483648 bytes]"
has "KV pool 2.000 GiB/worker is below SEAT_KV_NEED_GIB=2.66"; has "SEAT_MAX_LEN=163840"; has "--max-model-len -1"; has "auto-fits the context"; has "LESS than SEAT_MAX_LEN"
lacks "WARNING"
verdict "knob set and the pinned pool below it: --max-model-len -1, one auto-fit line, the pin banner still printed"

USED=0 run_seat k2 "${KV[@]}" SEAT_KV_NEED_GIB=2.66
launched; maxlen_is 163840
has "KV pool PINNED at 3.400 GiB/worker"; lacks "auto-fits"; lacks "WARNING"
verdict "knob set and the pool above it: SEAT_MAX_LEN kept, nothing printed about it"

USED=2344 run_seat k3 "${KV[@]}"
launched; maxlen_is 163840
has "KV pool PINNED at 2.000 GiB/worker"; has "WARNING"; has "FLOORED"; has "SEAT_KV_NEED_GIB"; lacks "auto-fits"
verdict "knob unset and the pool floored: SEAT_MAX_LEN kept and a WARNING names SEAT_KV_NEED_GIB"

USED=0 run_seat k4 "${KV[@]}"
launched; maxlen_is 163840
has "KV pool PINNED at 3.400 GiB/worker"; lacks "WARNING"; lacks "auto-fits"
verdict "knob unset and the pool NOT floored: no warning"

USED=2344 run_seat k5 "${KV[@]}" SEAT_KV_NEED_GIB=abc
launched; maxlen_is 163840
has "SEAT_KV_NEED_GIB='abc' is not a number"; has "FLOORED"; lacks "auto-fits"
verdict "a non-number knob is a warning and is ignored (the floored-pool warning follows)"

for bad in "2,66" "1.2.3" ".5" "5." "-1" "2.66 "; do
  USED=2344 run_seat k5b "${KV[@]}" "SEAT_KV_NEED_GIB='$bad'"
  launched; maxlen_is 163840; has "is not a number"; lacks "auto-fits"
  verdict "knob value '$bad' is not a number and is ignored"
done

USED=2344 run_seat k6 "${KV[@]}" SEAT_KV_NEED_GIB=2.0
launched; maxlen_is 163840
has "KV pool PINNED at 2.000 GiB/worker"; lacks "auto-fits"; lacks "WARNING"
verdict "a pool exactly at the knob is enough: SEAT_MAX_LEN kept"

USED=1800 run_seat k7 "${KV[@]}" SEAT_KV_NEED_GIB=2.66
launched; maxlen_is -1
has "KV pool 2.481 GiB/worker is below SEAT_KV_NEED_GIB=2.66"; lacks "WARNING"
verdict "a pool computed (not floored) below the knob also auto-fits"

USED=2344 run_seat k8 SEAT_KV_NEED_GIB=2.66
launched; maxlen_is 163840
[ -z "$(argval --kv-cache-memory-bytes)" ] || PROBLEMS="$PROBLEMS [a pin appeared without SEAT_KV_HEADROOM_GIB]"
lacks "auto-fits"; lacks "KV pool PINNED"
verdict "no pin (SEAT_KV_HEADROOM_GIB unset): the knob changes nothing"

USED=2344 run_seat k9 SEAT_MAX_LEN= "${KV[@]}" SEAT_KV_NEED_GIB=2.66
launched; maxlen_is -1
has "SEAT_MAX_LEN=131072"
verdict "SEAT_MAX_LEN empty: the 131072 default is what the auto-fit line names"

USED=0 run_seat k10 SEAT_MAX_LEN= "${KV[@]}" SEAT_KV_NEED_GIB=2.66
launched; maxlen_is 131072
verdict "SEAT_MAX_LEN empty and the pool enough: the 131072 default is kept"

# ---- B-55: the address the cache-server share is mounted at ---------------------------------------------------------------
# The launcher box has two NICs on 192.0.2.0/24 (wired .10, Wi-Fi .11) and the store host answers mDNS for store.local with
# every one of its interfaces' addresses: an address of another network first (198.51.100.7), then one on the launcher's own
# network (192.0.2.50). The pinned source address is the wired one (auto = the lowest-metric default route).
STORE=(SEAT_L2_MOUNT_SRC=//store.local/kvcache SEAT_L2_MOUNT_DIR=@R@/mnt SEAT_L2_MOUNT_OPTS=vers=3.1.1,cache=none
  SEAT_L2_MOUNT_SRCADDR=auto "SEAT_L2='{\"type\":\"fs_native\",\"base_path\":\"@R@/mnt/store\"}'")
mounted_ok() { [ "$RC" = 0 ] && [ -s "$R/mount.args" ] || PROBLEMS="$PROBLEMS [no mount command ran (exit $RC)]"; lacks "CACHE SERVER DEGRADED"; }
mount_is() { [ "$(mountsrc)" = "$1" ] || PROBLEMS="$PROBLEMS [mounted '$(mountsrc)', want '$1']"; }
no_helper() { [ ! -e "$R/mdns.argv" ] || PROBLEMS="$PROBLEMS [the mDNS helper ran]"; }
status_is() { case "$(status_line)" in $1) ;; *) PROBLEMS="$PROBLEMS [status file says '$(status_line)', want '$1']" ;; esac; }

MDNS_ANSWERS=$'198.51.100.7\n192.0.2.50' run_seat s1 "${STORE[@]}"
mounted_ok; mount_is //192.0.2.50/kvcache
case "$(mountopts)" in *srcaddr=192.0.2.10*) ;; *) PROBLEMS="$PROBLEMS [mount options '$(mountopts)' lost srcaddr=192.0.2.10]" ;; esac
case "$(mountopts)" in vers=3.1.1,cache=none,srcaddr=192.0.2.10) ;; *) PROBLEMS="$PROBLEMS [mount options changed: '$(mountopts)']" ;; esac
[ "$(cat "$R/mdns.argv" 2>/dev/null | tr '\n' ' ')" = "store.local 192.0.2.10 " ] || PROBLEMS="$PROBLEMS [the helper was called with '$(cat "$R/mdns.argv" 2>/dev/null | tr '\n' ' ')']"
grep -F "seat_fg: cache-server store store.local" "$R/out" | grep -F "198.51.100.7" | grep -F "192.0.2.50" | grep -qF "chose 192.0.2.50" || PROBLEMS="$PROBLEMS [no single line names every answer and the choice]"
[ "$(grep -c 'answers:' "$R/out")" = 1 ] || PROBLEMS="$PROBLEMS [the answers are not printed on exactly one line]"
status_is "ok * mbps=unmeasured addr=192.0.2.50"
verdict "an off-network answer first and an on-network one second: the mount uses the on-network address, one line names every answer and the choice, the status file records it"

MDNS_ANSWERS=$'198.51.100.7\n192.0.2.50' run_seat s2 "${STORE[@]}" SEAT_L2_MOUNT_SRCADDR=192.0.2.10
mounted_ok; mount_is //192.0.2.50/kvcache; status_is "ok * mbps=unmeasured addr=192.0.2.50"
verdict "an explicit source address picks the address the same way"

MDNS_ANSWERS=$'198.51.100.7\n192.0.2.60\n192.0.2.50' run_seat s3 "${STORE[@]}"
mounted_ok; mount_is //192.0.2.60/kvcache
verdict "the FIRST answer inside the network wins"

MDNS_ANSWERS=$'192.0.2.50' run_seat s3b "${STORE[@]}" SEAT_L2_MOUNT_SRC=//store.local/kvcache/sub
mounted_ok; mount_is //192.0.2.50/kvcache/sub
verdict "a share path below the share name is kept"

MDNS_RC=3 run_seat s4 "${STORE[@]}"
mounted_ok; mount_is //store.local/kvcache; has "mounting by name as before"; has "exit 3"
status_is "ok * mbps=unmeasured"
case "$(status_line)" in *addr=*) PROBLEMS="$PROBLEMS [the status file names an address nobody chose]" ;; esac
verdict "the helper fails: the mount is by name exactly as before, the reason is printed, the status line is unchanged"

MDNS_RC=1 run_seat s4b "${STORE[@]}"
mounted_ok; mount_is //store.local/kvcache; has "no mDNS answer"; has "mounting by name as before"
verdict "the helper finds no answer: the mount is by name, the reason is printed"

MDNS_ANSWERS='' run_seat s4c "${STORE[@]}"
mounted_ok; mount_is //store.local/kvcache; has "mounting by name as before"
verdict "the helper exits 0 with nothing usable: the mount is by name"

MDNS_ANSWERS=$'198.51.100.7\n203.0.113.9' run_seat s5 "${STORE[@]}"
mounted_ok; mount_is //store.local/kvcache; has "198.51.100.7 203.0.113.9"; has "none inside 192.0.2.10/24"; has "mounting by name as before"
status_is "ok * mbps=unmeasured"
verdict "no answer on the source address's network: the mount is by name and the line lists what was heard"

MDNS_ANSWERS=$'not-an-address\n192.0.2.300\n192.0.2.50' run_seat s5b "${STORE[@]}"
mounted_ok; mount_is //192.0.2.50/kvcache
verdict "lines that are not IPv4 addresses are ignored"

MDNS_MODE=hang run_seat s6 "${STORE[@]}"
mounted_ok; mount_is //store.local/kvcache; has "timed out"; has "mounting by name as before"
quick "$ELAPSED" 20 || PROBLEMS="$PROBLEMS [the start took ${ELAPSED} s: a helper that hangs holds the seat start]"
verdict "a helper that hangs is cut off at 3 s and the mount is by name"

MDNS_ANSWERS=$'192.0.2.50' run_seat s7 "${STORE[@]}" SEAT_L2_MOUNT_SRC=//nas.example.test/kvcache
mounted_ok; mount_is //nas.example.test/kvcache; no_helper; lacks "mDNS"
verdict "a name that is not .local is not looked up"

MDNS_ANSWERS=$'192.0.2.50' run_seat s7b "${STORE[@]}" SEAT_L2_MOUNT_SRC=//192.0.2.77/kvcache
mounted_ok; mount_is //192.0.2.77/kvcache; no_helper
verdict "an address literal is mounted as given"

MDNS_ANSWERS=$'192.0.2.50' run_seat s8 "${STORE[@]}" SEAT_L2_MOUNT_TYPE=nfs SEAT_L2_MOUNT_SRC=store.local:/srv/store
[ "$RC" = 0 ] && [ -s "$R/mount.args" ] || PROBLEMS="$PROBLEMS [no mount command ran]"
[ "$(argafter "$R/mount.args" nfs)" = store.local:/srv/store ] || PROBLEMS="$PROBLEMS [the nfs source changed: '$(argafter "$R/mount.args" nfs)']"
no_helper; lacks "mDNS"
verdict "an nfs mount is untouched"

MDNS_ANSWERS=$'192.0.2.50' run_seat s9 "${STORE[@]}" SEAT_L2_MOUNT_SRCADDR=
mounted_ok; mount_is //store.local/kvcache; no_helper; lacks "mDNS"
case "$(mountopts)" in *srcaddr*) PROBLEMS="$PROBLEMS [a source address was pinned]" ;; esac
verdict "no source pin: the mount is by name, as before, and nothing is looked up"

# A chosen address that does not mount: the degrade message keeps its text and names the address. 127.0.0.1 stands for the pinned
# source (the ip stand-in lists it as 127.0.0.1/8) so the reachability probe the message runs goes nowhere off this box.
MDNS_ANSWERS=$'127.0.0.9' MOUNT_RC=32 run_seat s10 "${STORE[@]}" SEAT_L2_MOUNT_SRCADDR=127.0.0.1
has "CACHE SERVER DEGRADED — share //store.local/kvcache did not mount at"; has "127.0.0.9"
case "$(status_line)" in "degraded "*"reason=share //store.local/kvcache did not mount at "*127.0.0.9*) ;; *) PROBLEMS="$PROBLEMS [status file says '$(status_line)']" ;; esac
[ "$RC" = 0 ] && [ -s "$R/vllm.args" ] || PROBLEMS="$PROBLEMS [the seat did not start without the cache server (exit $RC)]"
verdict "a chosen address that does not mount: the seat degrades, the message keeps its text and names the address"

MDNS_ANSWERS=$'198.51.100.7\n192.0.2.50' run_seat s11 "${STORE[@]}" SEAT_L2_MIN_MBPS=100000000
has "CACHE SERVER DEGRADED — share writes at ~"; has "below SEAT_L2_MIN_MBPS=100000000"; has "192.0.2.50"
case "$(status_line)" in degraded*) ;; *) PROBLEMS="$PROBLEMS [status file says '$(status_line)']" ;; esac
verdict "the write-floor degrade keeps its text and names the address the share was mounted at"

# ---- B-55: the embedded mDNS helper itself --------------------------------------------------------------------------------
# The helper is a here-doc inside seat_fg.sh, run as `python - <name> <source address>`. Its destination is the mDNS group unless
# SEAT_FG_TEST_MDNS_DEST says otherwise (a test-only seam), so it is driven here against a responder on loopback that answers
# the way real ones do: unicast, the query's id and question repeated, the owner name as a compression pointer, answers of
# other names and other types mixed in, each answer in a datagram of its own, and a junk datagram first.
if ! command -v python3 >/dev/null; then
  echo "SKIP the embedded-helper cases (no python3)"
else
  awk "/<<'PYEOF'/{on=1; next} /^PYEOF\$/{on=0} on{print}" "$HERE/seat_fg.sh" > "$T/helper.py"
  cat > "$T/responder.py" <<'PYEOF'
import socket, struct, sys
port_file, name, specs = sys.argv[1], sys.argv[2], sys.argv[3:]
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(("127.0.0.1", 0))
open(port_file, "w").write(str(s.getsockname()[1]))
def enc(n):
    return b"".join(bytes([len(p)]) + p.encode() for p in n.split(".")) + b"\0"
while True:
    data, peer = s.recvfrom(4096)
    if len(data) < 17 or not specs:
        continue                                  # a silent responder when it has nothing to say
    s.sendto(b"junk", peer)
    for spec in specs:
        owner, _, addr = spec.rpartition("=")
        rname = b"\xc0\x0c" if not owner or owner == name else enc(owner)
        if ":" in addr:
            rr = rname + struct.pack(">HHIH", 28, 0x8001, 120, 16) + socket.inet_pton(socket.AF_INET6, addr)
        else:
            rr = rname + struct.pack(">HHIH", 1, 0x8001, 120, 4) + socket.inet_aton(addr)
        s.sendto(data[:2] + struct.pack(">HHHHH", 0x8400, 1, 1, 0, 0) + data[12:] + rr, peer)
PYEOF
  start_responder() {   # start_responder <tag> <name> [owner=]address ... : leaves the port in $RESP_PORT
    rm -f "$T/$1.port"
    python3 "$T/responder.py" "$T/$1.port" "${@:2}" >/dev/null 2>&1 &
    BGPIDS="$BGPIDS $!"
    local i; for i in $(seq 1 50); do [ -s "$T/$1.port" ] && break; sleep 0.1; done
    RESP_PORT="$(cat "$T/$1.port" 2>/dev/null)"
  }
  run_helper() {   # run_helper <port> <name> <source address>: HOUT, HRC and HSECS
    local t0 t1
    t0=$(date +%s.%N)
    HOUT="$(env -i PATH=/usr/bin:/bin SEAT_FG_TEST_MDNS_DEST="127.0.0.1:$1" timeout -s KILL 8 python3 - "$2" "$3" < "$T/helper.py" 2>"$T/helper.err")"; HRC=$?
    t1=$(date +%s.%N)
    HSECS="$(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.1f", b-a}')"
  }
  R="$T/helper-cases"; mkdir -p "$R"; : > "$R/out"
  [ -s "$T/helper.py" ] || PROBLEMS="$PROBLEMS [seat_fg.sh has no embedded helper (a here-doc ended by PYEOF)]"

  start_responder r1 store.local 198.51.100.7 other.local=203.0.113.9 fe80::1 192.0.2.50 198.51.100.7
  run_helper "$RESP_PORT" store.local 192.0.2.10
  [ "$HRC" = 0 ] || PROBLEMS="$PROBLEMS [exit $HRC: $(head -c 300 "$T/helper.err")]"
  [ "$HOUT" = $'198.51.100.7\n192.0.2.50' ] || PROBLEMS="$PROBLEMS [printed '$(echo "$HOUT" | tr '\n' ' ')']"
  quick "$HSECS" 3 || PROBLEMS="$PROBLEMS [took ${HSECS} s]"
  verdict "the helper prints each A answer of the asked name once, in arrival order, and ignores other names, other types and junk"

  start_responder r2 store.local
  run_helper "$RESP_PORT" store.local 192.0.2.10
  [ "$HRC" != 0 ] && [ -z "$HOUT" ] || PROBLEMS="$PROBLEMS [exit $HRC, printed '$HOUT']"
  quick "$HSECS" 3 || PROBLEMS="$PROBLEMS [took ${HSECS} s: the helper must end by itself well inside the launcher's 3 s bound]"
  verdict "no answer: the helper prints nothing, exits non-zero and ends by itself inside 3 s"

  run_helper 9 store.local 192.0.2.10
  [ "$HRC" != 0 ] && [ -z "$HOUT" ] || PROBLEMS="$PROBLEMS [exit $HRC, printed '$HOUT']"
  quick "$HSECS" 3 || PROBLEMS="$PROBLEMS [took ${HSECS} s]"
  verdict "nothing listening at the destination: same, inside 3 s"

  start_responder r3 store.local 198.51.100.7 192.0.2.50
  NO_MDNS_SEAM=1 EXTRA_ENV="SEAT_FG_TEST_MDNS_DEST=127.0.0.1:$RESP_PORT" run_seat s13 "${STORE[@]}"
  mounted_ok; mount_is //192.0.2.50/kvcache; has "chose 192.0.2.50"
  verdict "the launcher with the real embedded helper, no stand-in: the on-network answer is mounted"
fi

# ---- done ------------------------------------------------------------------------------------------------------------------
if [ "$fail" -eq 0 ]; then echo "ALL PASS"; else echo "FAILED"; exit 1; fi
