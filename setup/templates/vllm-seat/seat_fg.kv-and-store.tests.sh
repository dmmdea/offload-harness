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
#         the launcher now collects all the answers, mounts the one inside the pinned source address's IPv4 network and, when
#         SEVERAL lie inside it (the store's wired and Wi-Fi addresses on one network), the one with the lowest minimum ping
#         round-trip time, and mounts by name exactly as before when it cannot.
# Every command that would touch the box is a stand-in (systemctl, systemd-run, ss, pgrep, ps, mountpoint, mount, ip, getent,
# dmesg, nvidia-smi, vllm, ping, the mDNS helper): the MP server's systemd unit is never stopped or started, nothing is mounted,
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
# Not mounted unless a run says so (MOUNTED_RC=0): the share's mount state is a file the stand-in reads.
stub bin/mountpoint 'exit "$(cat "$SEAT_TEST_DIR/mountpoint.rc" 2>/dev/null || echo 1)"'
# What the kernel says about the mounted share: `findmnt -n -o SOURCE,OPTIONS -M <dir>` prints the source and the option list.
stub bin/findmnt 'printf "%s\n" "$@" > "$SEAT_TEST_DIR/findmnt.argv"
cat "$SEAT_TEST_DIR/findmnt.out" 2>/dev/null
exit 0'
# The launcher must never unmount a share: this stand-in only leaves a mark, and a case that finds the mark fails.
stub bin/umount 'printf "%s\n" "$@" > "$SEAT_TEST_DIR/umount.argv"; exit 0'
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
    echo "4: docker0    inet 198.51.100.1/24 brd 198.51.100.255 scope global docker0\       valid_lft forever preferred_lft forever"
    echo "5: br28    inet 203.0.113.17/28 brd 203.0.113.31 scope global br28\       valid_lft forever preferred_lft forever"
    echo "6: tun16    inet 10.0.1.10/16 brd 10.0.255.255 scope global tun16\       valid_lft forever preferred_lft forever" ;;
esac
exit 0'
# What the system resolver says for the store name: ONE address, and not necessarily the right one.
stub bin/getent 'case "$1 $2" in "hosts store.local") echo "198.51.100.200   store.local" ;; *) exit 2 ;; esac'
# The test-only mDNS seam: SEAT_FG_TEST_MDNS_CMD names this stand-in, which prints the scripted answers (or hangs, or fails).
stub bin/mdns-stub 'printf "%s\n" "$@" > "$SEAT_TEST_DIR/mdns.argv"
[ "$(cat "$SEAT_TEST_DIR/mdns.mode" 2>/dev/null)" = hang ] && exec sleep 600
cat "$SEAT_TEST_DIR/mdns.answers" 2>/dev/null
exit "$(cat "$SEAT_TEST_DIR/mdns.rc" 2>/dev/null || echo 0)"'
# Nothing here sends a packet. Two stand-ins for ping, both scripted by one file per address (rtt.<address>, absent = no reply):
#   ping-stub is the test-only seam SEAT_FG_TEST_PING_CMD, called as `<cmd> <source address> <address>`; it prints the minimum RTT.
#   ping is the production shape: found on PATH, called as the launcher calls the real one, it prints what iputils (or busybox)
#   print. A file holds `<min>` or `<min>:<avg>`, or `garbage` (replies, but nothing a round-trip time can be read from).
# Both append what they were called with to ping.calls, and both hang when ping.mode says so.
stub bin/ping-stub 'printf "%s %s\n" "$1" "$2" >> "$SEAT_TEST_DIR/ping.calls"
[ "$(cat "$SEAT_TEST_DIR/ping.mode" 2>/dev/null)" = hang ] && exec sleep 600
f="$SEAT_TEST_DIR/rtt.$2"; [ -s "$f" ] || exit 1
v="$(cat "$f")"; echo "${v%%:*}"; exit 0'
stub bin/ping 'printf "%s\n" "$*" >> "$SEAT_TEST_DIR/ping.calls"
[ "$(cat "$SEAT_TEST_DIR/ping.mode" 2>/dev/null)" = hang ] && exec sleep 600
for last; do :; done
f="$SEAT_TEST_DIR/rtt.$last"
if [ ! -s "$f" ]; then
  printf "PING %s (%s) 56(84) bytes of data.\n\n--- %s ping statistics ---\n5 packets transmitted, 0 received, 100%% packet loss, time 819ms\n\n" "$last" "$last" "$last"; exit 1
fi
v="$(cat "$f")"
if [ "$v" = garbage ]; then printf "5 packets transmitted, 5 received, 0%% packet loss\n"; exit 0; fi
mn="${v%%:*}"; av="${v#*:}"
case "$(cat "$SEAT_TEST_DIR/ping.style" 2>/dev/null)" in
  busybox) printf "5 packets transmitted, 5 packets received, 0%% packet loss\nround-trip min/avg/max = %s/%s/%s ms\n" "$mn" "$av" "$av" ;;
  *) printf -- "--- %s ping statistics ---\n5 packets transmitted, 5 received, 0%% packet loss, time 804ms\nrtt min/avg/max/mdev = %s/%s/%s/0.100 ms\n" "$last" "$mn" "$av" "$av" ;;
esac
exit 0'
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
# mDNS stand-in prints), MDNS_RC, MDNS_MODE (hang), MOUNT_RC, MOUNTED_RC (0 = the share is already mounted) with FINDMNT_OUT
# (what findmnt then reports for it), NO_MDNS_SEAM (1 = leave the test seam unset, the production
# shape) and EXTRA_ENV (more NAME=value words for the launcher's environment). The ping inputs: RTTS (words of
# `<address>=<min>[:<avg>]`, or `<address>=garbage`; an address with no word gets no reply), PING_SEAM (the command the test seam
# SEAT_FG_TEST_PING_CMD names; unset = the production shape, `ping` found on PATH), PING_MODE (hang), PING_STYLE (busybox) and
# NO_PING_BIN (1 = a PATH on which no ping exists). Leaves $R (the run's directory), $RC (the launcher's exit status) and
# $ELAPSED (seconds) behind.
noping_path() {   # a directory of links to every command of this box except ping, built once: the PATH of a box without ping
  if [ ! -d "$T/noping" ]; then
    mkdir -p "$T/noping"
    local d f files
    for d in "$T/bin" /usr/local/bin /usr/bin; do
      files=(); for f in "$d"/*; do
        case "${f##*/}" in ping|ping[46]|ping6) ;; *) [ -e "$f" ] && files+=("$f") ;; esac
      done
      ln -s -t "$T/noping" -- "${files[@]}" 2>/dev/null   # a name taken by an earlier directory is kept
    done
  fi
  printf '%s' "$T/noping"
}
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
  echo "${MOUNTED_RC:-1}" > "$R/mountpoint.rc"; printf '%s\n' "${FINDMNT_OUT:-}" > "$R/findmnt.out"
  local pair; for pair in ${RTTS:-}; do printf '%s\n' "${pair#*=}" > "$R/rtt.${pair%%=*}"; done
  echo "${PING_MODE:-normal}" > "$R/ping.mode"; echo "${PING_STYLE:-iputils}" > "$R/ping.style"
  local seam=(); [ -z "${NO_MDNS_SEAM:-}" ] && seam=(SEAT_FG_TEST_MDNS_CMD="$T/bin/mdns-stub")
  [ -n "${PING_SEAM:-}" ] && seam+=(SEAT_FG_TEST_PING_CMD="$PING_SEAM")
  local pathv="$T/bin:/usr/local/bin:/usr/bin:/bin"; [ -n "${NO_PING_BIN:-}" ] && pathv="$(noping_path)"
  local t0 t1
  t0=$(date +%s.%N)
  timeout -s KILL 90 env -i PATH="$pathv" HOME="$R" LANG=C TERM=dumb \
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
verdict "two answers inside the network and no round-trip time to compare (the stand-in ping gets no reply): the FIRST one wins"

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

# The pinned address's network is its OWN prefix, not a fixed /24: a /28 and a /16 pinned address (the ip stand-in lists
# 203.0.113.17/28 and 10.0.1.10/16). An answer that shares the /24 with the pinned /28 address is NOT on its network, and an
# answer on a /16 pinned address's network is on it whatever its third octet.
MDNS_ANSWERS=$'203.0.113.50\n203.0.113.20' run_seat s12 "${STORE[@]}" SEAT_L2_MOUNT_SRCADDR=203.0.113.17
mounted_ok; mount_is //203.0.113.20/kvcache; has "inside 203.0.113.17/28"
verdict "a /28 pinned address: the answer inside the /28 is mounted, the one outside it (same /24) is passed over"

MDNS_ANSWERS=$'203.0.113.50' run_seat s12b "${STORE[@]}" SEAT_L2_MOUNT_SRCADDR=203.0.113.17
mounted_ok; mount_is //store.local/kvcache; has "none inside 203.0.113.17/28"; has "mounting by name as before"
verdict "a /28 pinned address: an answer only elsewhere in its /24 is no answer on its network, the mount is by name"

MDNS_ANSWERS=$'172.16.0.9\n10.0.5.9' run_seat s12c "${STORE[@]}" SEAT_L2_MOUNT_SRCADDR=10.0.1.10
mounted_ok; mount_is //10.0.5.9/kvcache; has "inside 10.0.1.10/16"
verdict "a /16 pinned address: an answer in its /16 beyond its /24 is mounted"

# ---- B-55: SEVERAL answers inside the pinned network: the lowest minimum round-trip time ------------------------------------
# The store's wired and Wi-Fi addresses sit on ONE network, so "first inside the network" is arrival order and cannot tell them
# apart. The launcher pings each candidate from the pinned source address and mounts the lowest minimum RTT. On the reference
# workstation the wired address measured a minimum of 0.425-0.477 ms and the Wi-Fi one 0.983-2.288 ms (three runs each), while
# the AVERAGE of the Wi-Fi runs went from 1.5 to 26 ms, so the minimum is what is compared. The answers below put one off-network
# address first (198.51.100.7: never pinged) and two on the pinned network (192.0.2.60 first, 192.0.2.50 second).
PSTUB="$T/bin/ping-stub"
THREE=$'198.51.100.7\n192.0.2.60\n192.0.2.50'
pinged_is() { local got=""; [ -e "$R/ping.calls" ] && got="$(tr '\n' '|' < "$R/ping.calls")"; [ "$got" = "$1" ] || PROBLEMS="$PROBLEMS [ping was called as '${got:-<never>}', want '$1']"; }
no_ping() { [ ! -e "$R/ping.calls" ] || PROBLEMS="$PROBLEMS [ping ran: $(tr '\n' '|' < "$R/ping.calls")]"; }
one_line() { [ "$(grep -c 'answers:' "$R/out")" = 1 ] || PROBLEMS="$PROBLEMS [the answers are not printed on exactly one line]"; }
SRC10="192.0.2.10"

MDNS_ANSWERS="$THREE" RTTS="192.0.2.60=1.10 192.0.2.50=0.43" PING_SEAM="$PSTUB" run_seat t1 "${STORE[@]}"
mounted_ok; mount_is //192.0.2.50/kvcache; one_line
has "inside 192.0.2.10/24: 192.0.2.60 (min RTT 1.10 ms) 192.0.2.50 (min RTT 0.43 ms); chose 192.0.2.50, the lowest round-trip time"
has "mDNS answers: 198.51.100.7 192.0.2.60 192.0.2.50, inside"; lacks "arrival order"
pinged_is "$SRC10 192.0.2.60|$SRC10 192.0.2.50|"
status_is "ok * mbps=unmeasured addr=192.0.2.50"
verdict "two answers inside the network, the second with the lower round-trip time: the second is mounted, each candidate is pinged once from the source address, one line lists every RTT and the rule"

MDNS_ANSWERS="$THREE" RTTS="192.0.2.60=0.43 192.0.2.50=1.10" PING_SEAM="$PSTUB" run_seat t2 "${STORE[@]}"
mounted_ok; mount_is //192.0.2.60/kvcache; one_line
has "192.0.2.60 (min RTT 0.43 ms) 192.0.2.50 (min RTT 1.10 ms); chose 192.0.2.60, the lowest round-trip time"; lacks "arrival order"
verdict "two answers inside the network, the first with the lower round-trip time: the first is mounted"

MDNS_ANSWERS=$'192.0.2.60\n192.0.2.61\n192.0.2.50' RTTS="192.0.2.60=1.20 192.0.2.61=0.90 192.0.2.50=0.43" PING_SEAM="$PSTUB" run_seat t3 "${STORE[@]}"
mounted_ok; mount_is //192.0.2.50/kvcache; has "chose 192.0.2.50, the lowest round-trip time"
pinged_is "$SRC10 192.0.2.60|$SRC10 192.0.2.61|$SRC10 192.0.2.50|"
verdict "three answers inside the network, the last the lowest: the last is mounted and all three were pinged"

MDNS_ANSWERS=$'192.0.2.60\n192.0.2.61\n192.0.2.50' RTTS="192.0.2.60=0.90 192.0.2.61=0.43 192.0.2.50=1.20" PING_SEAM="$PSTUB" run_seat t3b "${STORE[@]}"
mounted_ok; mount_is //192.0.2.61/kvcache; has "chose 192.0.2.61, the lowest round-trip time"
verdict "three answers inside the network, the middle one the lowest: the middle one is mounted"

MDNS_ANSWERS="$THREE" RTTS="192.0.2.60=12.5 192.0.2.50=9" PING_SEAM="$PSTUB" run_seat t3c "${STORE[@]}"
mounted_ok; mount_is //192.0.2.50/kvcache; has "192.0.2.60 (min RTT 12.5 ms) 192.0.2.50 (min RTT 9 ms); chose 192.0.2.50, the lowest round-trip time"
verdict "round-trip times are compared as numbers, not as text (12.5 ms is above 9 ms)"

MDNS_ANSWERS="$THREE" RTTS="192.0.2.60=0.43 192.0.2.50=0.43" PING_SEAM="$PSTUB" run_seat t4 "${STORE[@]}"
mounted_ok; mount_is //192.0.2.60/kvcache; has "chose 192.0.2.60, the lowest round-trip time"
verdict "equal round-trip times: the first in arrival order is kept"

MDNS_ANSWERS="$THREE" RTTS="192.0.2.50=1.10" PING_SEAM="$PSTUB" run_seat t5 "${STORE[@]}"
mounted_ok; mount_is //192.0.2.50/kvcache; one_line
has "192.0.2.60 (no reply) 192.0.2.50 (min RTT 1.10 ms); chose 192.0.2.50, the only candidate that replied to ping"; lacks "arrival order"
verdict "the first candidate gets no reply: the one that replied is mounted although it came second, and the line says no reply"

MDNS_ANSWERS="$THREE" RTTS="192.0.2.60=1.10" PING_SEAM="$PSTUB" run_seat t5b "${STORE[@]}"
mounted_ok; mount_is //192.0.2.60/kvcache
has "192.0.2.60 (min RTT 1.10 ms) 192.0.2.50 (no reply); chose 192.0.2.60, the only candidate that replied to ping"
verdict "the second candidate gets no reply: the first, which replied, is mounted (a failed ping is not a round-trip time of 0)"

MDNS_ANSWERS=$'192.0.2.60\n192.0.2.61\n192.0.2.50' RTTS="192.0.2.61=2.20 192.0.2.50=1.30" PING_SEAM="$PSTUB" run_seat t5c "${STORE[@]}"
mounted_ok; mount_is //192.0.2.50/kvcache
has "192.0.2.60 (no reply) 192.0.2.61 (min RTT 2.20 ms) 192.0.2.50 (min RTT 1.30 ms); chose 192.0.2.50, the lowest round-trip time"
verdict "three candidates, the first without a reply: the lower of the two that replied is mounted"

MDNS_ANSWERS="$THREE" PING_SEAM="$PSTUB" run_seat t6 "${STORE[@]}"
mounted_ok; mount_is //192.0.2.60/kvcache; one_line
has "inside 192.0.2.10/24: 192.0.2.60 (no reply) 192.0.2.50 (no reply); chose 192.0.2.60, the first inside the network in arrival order (round-trip times could not be compared: no candidate replied to ping)"
lacks "the lowest round-trip time"; pinged_is "$SRC10 192.0.2.60|$SRC10 192.0.2.50|"
status_is "ok * mbps=unmeasured addr=192.0.2.60"
verdict "every ping fails: the first inside the network by arrival order, the line says that rule and why, and lists each candidate as no reply"

MDNS_ANSWERS="$THREE" PING_SEAM="$T/bin/no-such-ping" run_seat t7 "${STORE[@]}"
mounted_ok; mount_is //192.0.2.60/kvcache; one_line
has "192.0.2.60 (not measured) 192.0.2.50 (not measured); chose 192.0.2.60, the first inside the network in arrival order (round-trip times could not be compared: ping is not available on this box)"
verdict "the ping command named by the seam does not exist: the first inside the network by arrival order, the line says ping is not available"

MDNS_ANSWERS="$THREE" RTTS="192.0.2.60=1.10 192.0.2.50=0.43" NO_PING_BIN=1 run_seat t7b "${STORE[@]}"
mounted_ok; mount_is //192.0.2.60/kvcache; one_line
has "192.0.2.60 (not measured) 192.0.2.50 (not measured); chose 192.0.2.60, the first inside the network in arrival order (round-trip times could not be compared: ping is not available on this box)"
[ ! -e "$R/ping.calls" ] || PROBLEMS="$PROBLEMS [a ping ran on a PATH that has none]"
verdict "no ping on the box's PATH (the production shape): the first inside the network by arrival order, the line says ping is not available"

MDNS_ANSWERS="$THREE" RTTS="192.0.2.60=garbage 192.0.2.50=garbage" PING_SEAM="$PSTUB" run_seat t8 "${STORE[@]}"
mounted_ok; mount_is //192.0.2.60/kvcache
has "192.0.2.60 (RTT unreadable) 192.0.2.50 (RTT unreadable); chose 192.0.2.60, the first inside the network in arrival order (round-trip times could not be compared: no round-trip time could be read from ping's output)"
verdict "ping replies but no round-trip time can be read: the first inside the network by arrival order, the line says so"

MDNS_ANSWERS=$'198.51.100.7\n192.0.2.50' RTTS="198.51.100.7=0.05 192.0.2.50=0.43" PING_SEAM="$PSTUB" run_seat t9 "${STORE[@]}"
mounted_ok; mount_is //192.0.2.50/kvcache; no_ping; one_line
has "mDNS answers: 198.51.100.7 192.0.2.50 — chose 192.0.2.50, the first inside 192.0.2.10/24 (the pinned source address's network)"; lacks "RTT"
verdict "ONE answer inside the network: it is mounted as before and ping is not called"

MDNS_ANSWERS=$'192.0.2.50' RTTS="192.0.2.50=0.43" run_seat t9b "${STORE[@]}"
mounted_ok; mount_is //192.0.2.50/kvcache; no_ping; lacks "RTT"
verdict "ONE answer, the production shape (ping on PATH): ping is not called"

MDNS_ANSWERS=$'198.51.100.7\n203.0.113.9\n192.0.2.60\n192.0.2.50' RTTS="198.51.100.7=0.05 203.0.113.9=0.05 192.0.2.60=1.10 192.0.2.50=0.43" PING_SEAM="$PSTUB" run_seat t10 "${STORE[@]}"
mounted_ok; mount_is //192.0.2.50/kvcache
pinged_is "$SRC10 192.0.2.60|$SRC10 192.0.2.50|"; lacks "198.51.100.7 (min RTT"; lacks "203.0.113.9 (min RTT"
verdict "answers outside the pinned network are never pinged, even with a lower round-trip time, and are never chosen"

MDNS_ANSWERS="$THREE" RTTS="192.0.2.60=1.00:1.20 192.0.2.50=0.45:26.0" run_seat t11 "${STORE[@]}"
mounted_ok; mount_is //192.0.2.50/kvcache
pinged_is "-I $SRC10 -c 5 -i 0.2 -q -W 1 192.0.2.60|-I $SRC10 -c 5 -i 0.2 -q -W 1 192.0.2.50|"
has "192.0.2.60 (min RTT 1.00 ms) 192.0.2.50 (min RTT 0.45 ms); chose 192.0.2.50, the lowest round-trip time"
verdict "the real ping command line (from the pinned source address, 5 probes 0.2 s apart, quiet, 1 s wait) and the MINIMUM of its min/avg/max line is compared, not the average (0.45 min but 26.0 avg beats 1.00/1.20)"

MDNS_ANSWERS="$THREE" RTTS="192.0.2.50=0.80" run_seat t12 "${STORE[@]}"
mounted_ok; mount_is //192.0.2.50/kvcache
has "192.0.2.60 (no reply) 192.0.2.50 (min RTT 0.80 ms); chose 192.0.2.50, the only candidate that replied to ping"
verdict "the real ping reports 100% loss and exits 1 for the first candidate: the one that replied is mounted"

MDNS_ANSWERS="$THREE" RTTS="192.0.2.60=1.00 192.0.2.50=0.45" PING_STYLE=busybox run_seat t13 "${STORE[@]}"
mounted_ok; mount_is //192.0.2.50/kvcache; has "192.0.2.60 (min RTT 1.00 ms) 192.0.2.50 (min RTT 0.45 ms); chose 192.0.2.50"
verdict "a busybox-style summary line (round-trip min/avg/max = ...) is read too"

MDNS_ANSWERS="$THREE" RTTS="192.0.2.60=garbage 192.0.2.50=garbage" run_seat t14 "${STORE[@]}"
mounted_ok; mount_is //192.0.2.60/kvcache; has "(RTT unreadable)"; has "no round-trip time could be read from ping's output"
verdict "the real ping prints no min/avg/max line: the first inside the network by arrival order, the line says so"

MDNS_ANSWERS="$THREE" RTTS="192.0.2.60=1.10 192.0.2.50=0.43" PING_SEAM="$PSTUB" PING_MODE=hang run_seat t15 "${STORE[@]}"
mounted_ok; mount_is //192.0.2.60/kvcache; has "192.0.2.60 (no reply) 192.0.2.50 (no reply)"; has "arrival order"
quick "$ELAPSED" 20 || PROBLEMS="$PROBLEMS [the start took ${ELAPSED} s: a ping that hangs holds the seat start]"
verdict "a ping stand-in that hangs is cut off at 3 s per candidate and the first inside the network is mounted"

MDNS_ANSWERS="$THREE" RTTS="192.0.2.60=1.10 192.0.2.50=0.43" PING_MODE=hang run_seat t15b "${STORE[@]}"
mounted_ok; mount_is //192.0.2.60/kvcache; has "192.0.2.60 (no reply) 192.0.2.50 (no reply)"; has "arrival order"
quick "$ELAPSED" 20 || PROBLEMS="$PROBLEMS [the start took ${ELAPSED} s: a ping that hangs holds the seat start]"
verdict "a ping that hangs (the production shape) is cut off at 3 s per candidate and the first inside the network is mounted"

MDNS_ANSWERS="$THREE" RTTS="192.0.2.60=1.10 192.0.2.50=0.43" PING_SEAM="$PSTUB" run_seat t16 "${STORE[@]}" SEAT_L2_MOUNT_SRC=//192.0.2.77/kvcache
mounted_ok; mount_is //192.0.2.77/kvcache; no_helper; no_ping
verdict "SEAT_L2_MOUNT_SRC naming an address literal still overrides everything: no lookup, no ping"

MDNS_ANSWERS="$THREE" RTTS="192.0.2.60=1.10 192.0.2.50=0.43" PING_SEAM="$PSTUB" run_seat t16b "${STORE[@]}" SEAT_L2_MOUNT_SRC=//nas.example.test/kvcache
mounted_ok; mount_is //nas.example.test/kvcache; no_helper; no_ping
verdict "SEAT_L2_MOUNT_SRC naming a name that is not .local still overrides everything: no lookup, no ping"

# ---- B-55: a share that is ALREADY mounted ---------------------------------------------------------------------------------
# The store address is chosen when the share is mounted, which is once per WSL session: a later start (llama-swap restarts the
# seat on a swap) finds it mounted. It must not mount again and must not unmount (another seat may be using the share, and with
# two addresses of the store on one network a re-pick at every start could trade a good mount for the Wi-Fi one); it asks no
# mDNS, says where the share IS (source and server address, read from the mount), how to choose again, and records the address.
BYNAME='//store.local/kvcache rw,relatime,vers=3.1.1,cache=none,addr=192.0.2.51,srcaddr=192.0.2.10,file_mode=0755'
mounted_left_alone() {
  [ "$RC" = 0 ] && [ -s "$R/vllm.args" ] || PROBLEMS="$PROBLEMS [the seat did not start (exit $RC)]"
  [ ! -e "$R/mount.args" ] || PROBLEMS="$PROBLEMS [a mount command ran on a share that was already mounted]"
  [ ! -e "$R/umount.argv" ] || PROBLEMS="$PROBLEMS [the launcher unmounted the share]"
  no_helper; lacks "CACHE SERVER DEGRADED"
}
MOUNTED_RC=0 FINDMNT_OUT="$BYNAME" MDNS_ANSWERS=$'192.0.2.50' run_seat m1 "${STORE[@]}"
mounted_left_alone
has "cache-server share $R/mnt is already mounted from //store.local/kvcache (server address 192.0.2.51)"
has "not looked up again"; has "unmount the share while no seat uses it"; has "restart the seat"
grep -qxF "$R/mnt" "$R/findmnt.argv" 2>/dev/null || PROBLEMS="$PROBLEMS [findmnt was not asked about the mount directory]"
status_is "ok * mbps=unmeasured addr=192.0.2.51"
verdict "a share already mounted by name: no mount, no unmount, no lookup; the line names its source and server address and how to choose again; the status file records the address"

MOUNTED_RC=0 FINDMNT_OUT='//192.0.2.50/kvcache rw,relatime,vers=3.1.1,cache=none,addr=192.0.2.50,srcaddr=192.0.2.10' MDNS_ANSWERS=$'192.0.2.50' run_seat m2 "${STORE[@]}"
mounted_left_alone
has "is already mounted from //192.0.2.50/kvcache (server address 192.0.2.50)"; lacks "unmount the share"
status_is "ok * mbps=unmeasured addr=192.0.2.50"
verdict "a share already mounted at an address an earlier start chose: left alone, the address is recorded, no advice to unmount"

MOUNTED_RC=0 FINDMNT_OUT='' MDNS_ANSWERS=$'192.0.2.50' run_seat m3 "${STORE[@]}"
mounted_left_alone
has "cache-server share $R/mnt is already mounted"; lacks "server address"
status_is "ok * mbps=unmeasured"
case "$(status_line)" in *addr=*) PROBLEMS="$PROBLEMS [the status file names an address nobody read]" ;; esac
verdict "a share already mounted that findmnt cannot describe: left alone, said so, no address recorded"

MOUNTED_RC=0 FINDMNT_OUT="$BYNAME" MDNS_ANSWERS=$'192.0.2.50' run_seat m4 "${STORE[@]}" SEAT_L2_MOUNT_SRC=//nas.example.test/kvcache
mounted_left_alone; lacks "already mounted"; [ ! -e "$R/findmnt.argv" ] || PROBLEMS="$PROBLEMS [findmnt ran for a name that is not .local]"
status_is "ok * mbps=unmeasured"; case "$(status_line)" in *addr=*) PROBLEMS="$PROBLEMS [the status file names an address]" ;; esac
verdict "a mounted share whose name is not .local: untouched and unreported, as before"

MOUNTED_RC=0 FINDMNT_OUT="$BYNAME" MDNS_ANSWERS=$'192.0.2.50' run_seat m5 "${STORE[@]}" SEAT_L2_MOUNT_SRCADDR=
mounted_left_alone; lacks "already mounted"; [ ! -e "$R/findmnt.argv" ] || PROBLEMS="$PROBLEMS [findmnt ran without a source pin]"
verdict "a mounted share with no source pin: untouched and unreported, as before"

MOUNTED_RC=0 FINDMNT_OUT="$BYNAME" MDNS_ANSWERS=$'192.0.2.50' run_seat m6 "${STORE[@]}" SEAT_L2_MOUNT_TYPE=nfs SEAT_L2_MOUNT_SRC=store.local:/srv/store
mounted_left_alone; lacks "already mounted"; [ ! -e "$R/findmnt.argv" ] || PROBLEMS="$PROBLEMS [findmnt ran for an nfs mount]"
verdict "a mounted nfs share: untouched and unreported, as before"

MOUNTED_RC=0 FINDMNT_OUT="$BYNAME" MDNS_ANSWERS=$'192.0.2.50' run_seat m7 "${STORE[@]}" SEAT_L2_MIN_MBPS=100000000
[ "$RC" = 0 ] && [ -s "$R/vllm.args" ] || PROBLEMS="$PROBLEMS [the seat did not start (exit $RC)]"
[ ! -e "$R/umount.argv" ] || PROBLEMS="$PROBLEMS [the launcher unmounted the share]"
has "CACHE SERVER DEGRADED — share writes at ~"; has "below SEAT_L2_MIN_MBPS=100000000"
has "the share was already mounted at server address 192.0.2.51; a start does not choose the address again"
case "$(status_line)" in degraded*"already mounted at server address 192.0.2.51"*) ;; *) PROBLEMS="$PROBLEMS [status file says '$(status_line)']" ;; esac
verdict "the write-floor degrade of an already-mounted share keeps its text and says the share was mounted at that address before this start"

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
    i = 12                                        # answer only the question the helper must ask: <name> type A, class IN + QU bit
    while i < len(data) and data[i]:
        i += data[i] + 1
    if data[12:i + 1] != enc(name) or data[i + 1:i + 5] != struct.pack(">HH", 1, 0x8001):
        continue
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

  # The helper's DEFAULT destination, how often it asks and how long it listens cannot be driven through a loopback responder
  # (every case above moves the destination with SEAT_FG_TEST_MDNS_DEST, and real time makes a window test flaky). So the helper
  # runs here, unmodified, on a simulated clock and a simulated socket: sendto records the destination and the time, recv
  # advances the clock to the next scheduled datagram or by its timeout. Scenarios: silent (nobody answers), instant, drop-first
  # (the first query is lost, as one UDP datagram can be), delayed (every answer comes 0.7 s after its query).
  cat > "$T/sim.py" <<'PYEOF'
import json, socket, struct, sys, time
scenario, helper, out, name, src = sys.argv[1:6]
clock, pending, record, count = [0.0], [], {"sends": [], "binds": []}, [0]
def reply(query, addr):
    return query[:2] + struct.pack(">HHHHH", 0x8400, 1, 1, 0, 0) + query[12:] + b"\xc0\x0c" + struct.pack(">HHIH", 1, 0x8001, 120, 4) + socket.inet_aton(addr)
class Sock:
    def __init__(self, *a, **k): self.t = None
    def bind(self, a): record["binds"].append(list(a))
    def setsockopt(self, *a): pass
    def settimeout(self, t): self.t = t
    def sendto(self, data, dest):
        record["sends"].append([round(clock[0], 3), dest[0], dest[1]])
        count[0] += 1
        if scenario == "instant" or (scenario == "drop-first" and count[0] > 1) or scenario == "delayed":
            pending.append((clock[0] + (0.7 if scenario == "delayed" else 0.0), reply(data, "192.0.2.50")))
        return len(data)
    def recv(self, n):
        pending.sort(key=lambda p: p[0])
        if pending and pending[0][0] <= clock[0] + self.t + 1e-9:
            at, data = pending.pop(0)
            clock[0] = max(clock[0], at)
            return data
        clock[0] += self.t
        raise socket.timeout()
socket.socket = Sock
time.monotonic = lambda: clock[0]
time.sleep = lambda s: clock.__setitem__(0, clock[0] + s)
sys.argv = [helper, name, src]
rc = 0
try:
    exec(compile(open(helper).read(), helper, "exec"), {"__name__": "__main__"})
except SystemExit as e:
    rc = e.code if isinstance(e.code, int) else 1
record["end"] = round(clock[0], 3)
json.dump(record, open(out, "w"))
sys.exit(rc)
PYEOF
  sim_run() {   # sim_run <scenario>: SOUT (what the helper printed), SRC (its exit status); the record is in $T/sim.json
    rm -f "$T/sim.json"
    SOUT="$(env -i PATH=/usr/bin:/bin timeout -s KILL 20 python3 -I "$T/sim.py" "$1" "$T/helper.py" "$T/sim.json" store.local 192.0.2.10 2>"$T/sim.err")"; SRC=$?
    [ -s "$T/sim.json" ] || PROBLEMS="$PROBLEMS [the simulation produced no record: $(head -c 300 "$T/sim.err")]"
  }
  sim_val() { python3 -I -c 'import json, sys; r = json.load(open(sys.argv[1])); print(eval(sys.argv[2]))' "$T/sim.json" "$1" 2>/dev/null; }

  sim_run silent
  [ "$SRC" != 0 ] && [ -z "$SOUT" ] || PROBLEMS="$PROBLEMS [exit $SRC, printed '$SOUT']"
  [ "$(sim_val 'sorted(set((s[1], s[2]) for s in r["sends"]))')" = "[('224.0.0.251', 5353)]" ] || PROBLEMS="$PROBLEMS [the default destination is $(sim_val 'sorted(set((s[1], s[2]) for s in r["sends"]))'), want the mDNS group 224.0.0.251 port 5353]"
  [ "$(sim_val '2 <= len(r["sends"]) <= 3')" = True ] || PROBLEMS="$PROBLEMS [the helper asked $(sim_val 'len(r["sends"])') times, want 2 or 3]"
  [ "$(sim_val 'max(s[0] for s in r["sends"]) < 1.0')" = True ] || PROBLEMS="$PROBLEMS [a query went out after the listen window]"
  [ "$(sim_val '0.9 <= r["end"] <= 1.5')" = True ] || PROBLEMS="$PROBLEMS [the helper listened for $(sim_val 'r["end"]') s, want about 1 s]"
  verdict "the helper's default destination is the mDNS group and port, it asks 2 or 3 times and listens for about 1 s"

  sim_run drop-first
  [ "$SRC" = 0 ] && [ "$SOUT" = 192.0.2.50 ] || PROBLEMS="$PROBLEMS [exit $SRC, printed '$SOUT': one lost datagram must not lose the answer]"
  verdict "the first query is lost: the next one still brings the answer"

  sim_run delayed
  [ "$SRC" = 0 ] && [ "$SOUT" = 192.0.2.50 ] || PROBLEMS="$PROBLEMS [exit $SRC, printed '$SOUT': an answer 0.7 s after the query must be heard]"
  [ "$(sim_val 'r["end"] < 3')" = True ] || PROBLEMS="$PROBLEMS [the helper ran $(sim_val 'r["end"]') s]"
  verdict "an answer that takes 0.7 s is heard inside the window"
fi

# ---- done ------------------------------------------------------------------------------------------------------------------
if [ "$fail" -eq 0 ]; then echo "ALL PASS"; else echo "FAILED"; exit 1; fi
