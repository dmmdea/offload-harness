#!/usr/bin/env bash
# seat_fg.sh — llama-swap `cmd` for a vLLM seat backed by LMCache MP (reference template).
#
# Runs INSIDE the serving box's Linux (WSL distro or native), in the FOREGROUND of the client llama-swap
# started: when llama-swap ends that client, the session is reaped and vLLM with it — that is the
# swap-out. The LMCache MP server (L1 staging + optional cache-server L2) is a transient systemd unit
# that outlives the engine; seat_stop.sh (llama-swap `cmdStop`) stops both.
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
#   fp8 KV at 262,144 context runs on the three-card pipeline seat (the blackwell-3x16 tier profile); the
#     2026-09-03 "fp8 KV and 262k infeasible" result was the two-card layout on vLLM 0.28. MTP has no
#     pipeline support.
# Every knob is overridden from seat.env (SEAT_* variables) without editing this file. The values
# below are placeholders for a generic box; the operator's real ones live in seat.env, next to the
# harness's config.json `kv_cache_server` block, which is what `offload_status` reports — keep them
# in agreement (the status note says so).
set -u
CFG="${1:-${SEAT_ENV:-/root/g7/seat.env}}"   # env file as $1 (wsl.exe passes no environment through), else SEAT_ENV, else the default
[ -f "$CFG" ] && . "$CFG"
LOG="${SEAT_LOG:-/root/g7/seat.log}"
MODEL="${SEAT_MODEL:-RedHatAI/Qwen3.8-27B-INT4}"
NAME="${SEAT_NAME:-qwen3.8-27b-vllm}"
PORT="${SEAT_PORT:-18797}"
MP_PORT="${SEAT_MP_PORT:-18796}"
# The MP server also opens an HTTP frontend (LMCache 0.5.x: /, cache control, observability). Its upstream default is
# 0.0.0.0:8080 - on a WSL2 distro in mirrored networking that is the HOST's LAN + tailnet, and 8080 is somebody else's port on
# every box in this fleet (measured 2026-09-18: the production MP server logged `Uvicorn running on http://0.0.0.0:8080`).
# Loopback only, on its own port (reference pairing 18797 engine / 18796 MP ZMQ / 18790 MP HTTP — 18790 is a listed safe pick
# on every port file of the fleet; 18793 is the Qube's LiteLLM reservation); seat.env overrides it.
MP_HTTP_PORT="${SEAT_MP_HTTP_PORT:-18790}"
# SEAT_L1_GB default 2 (register B-02, measured 2026-09-18 on the pair: 2 / 4 / 8 GB restore the same context, RSS ≈ L1 + 1.1 GiB).
L1_GB="${SEAT_L1_GB:-2}"
CHUNK="${SEAT_CHUNK:-784}"
# The cache server (L2) is OPT-IN: empty = same-box tier only. `${VAR-default}` (no colon) so that
# SEAT_L2= in seat.env is an explicit "off", not a fall-through to a default.
L2="${SEAT_L2-}"
VENV="${SEAT_VENV:-/root/g7/vllm-env}"
WORK="${SEAT_WORKDIR:-/root/g7}"
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

# fs_native over a network share (measured 2026-09-04: the Lenovo tmpfs over SMB 3.1.1 recovers a 23.7k-token prefix
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
    if ! timeout 30 mount -t "${SEAT_L2_MOUNT_TYPE:-cifs}" "$SEAT_L2_MOUNT_SRC" "$SEAT_L2_MOUNT_DIR" -o "$MOUNT_OPTS"; then
      # Say WHY, in the terms the operator can act on: name resolution, reachability, or the share itself.
      host="${SEAT_L2_MOUNT_SRC#//}"; host="${host%%:*}"; host="${host%%/*}"
      case "${SEAT_L2_MOUNT_TYPE:-cifs}" in nfs|nfs4) port=2049 ;; *) port=445 ;; esac
      case "$host" in
        *[!0-9.]*) resolved="$(getent hosts "$host" 2>/dev/null | awk '{print $1; exit}')" ;;   # a name: resolve it
        *)         resolved="$host" ;;                                                          # an IPv4 literal: probe it as is
      esac
      if [ -z "$resolved" ]; then
        why="'$host' does not resolve on this box (use a tailnet MagicDNS or static name, not a DHCP address)"
      elif ! timeout 3 bash -c "</dev/tcp/$resolved/$port" 2>/dev/null; then
        why="'$host' = $resolved, port $port unreachable (store down, wrong interface, or the address moved)"
      else
        why="'$host' = $resolved answers on $port — check credentials, the share name, and the store's allow-list (source address ${SRCADDR:-unpinned; set SEAT_L2_MOUNT_SRCADDR=auto})"
      fi
      degrade_l2 "share $SEAT_L2_MOUNT_SRC did not mount at $SEAT_L2_MOUNT_DIR: $why"
    fi
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
        degrade_l2 "share writes at ~${mbps} MB/s, below SEAT_L2_MIN_MBPS=${MIN_MBPS} (the tier would be slower than recompute)"
      else
        echo "seat_fg: cache-server share write probe ~${mbps} MB/s (floor ${MIN_MBPS})"
      fi
    else
      rm -f "$probe"
      degrade_l2 "cannot write a 64 MiB probe to the share at $SEAT_L2_MOUNT_DIR within 120 s"
    fi
  fi
  if [ "$L2_DEGRADED" -eq 0 ]; then
    printf 'ok %s mbps=%s\n' "$(date -Is)" "${mbps:-unmeasured}" > "$L2_STATUS" 2>/dev/null || true
  fi
fi

# Refuse to start on a port something else already owns. vLLM would load for ~2 minutes and then die on
# "Address already in use" while llama-swap's health check passes against the FOREIGN listener — the seat is
# then "ready" and serving somebody else's engine. Fail here, at once, and name the squatter.
if ss -ltnp 2>/dev/null | grep -q ":$PORT "; then
  echo "seat_fg: REFUSING to start — :$PORT is already bound: $(ss -ltnp 2>/dev/null | grep ":$PORT " | grep -oE 'users:\(.*\)' | head -1)"
  exit 1
fi
if ss -ltnp 2>/dev/null | grep -q ":$MP_HTTP_PORT "; then
  echo "seat_fg: REFUSING to start — the MP HTTP port :$MP_HTTP_PORT is already bound: $(ss -ltnp 2>/dev/null | grep ":$MP_HTTP_PORT " | grep -oE 'users:\(.*\)' | head -1)"
  exit 1
fi

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
# WSL2 GPU-paravirtualization hazards (information, never a refusal). `make_resident: Ioctl failed: -12` (the host ran
# out of residency) and `reserve_gpu_va … -75` in the distro's kernel log mean the dxg path is degraded until the distro
# restarts, and an engine started on it fails later with a CUDA error that names neither. The recurring
# `query_adapter_info: Ioctl failed: -2` and dxgvmbus FORTIFY lines are noise and are not counted.
if grep -qi microsoft /proc/version 2>/dev/null; then
  dxg_n=$(dmesg 2>/dev/null | grep -cE "make_resident: Ioctl failed: -12|reserve_gpu_va.*-75")
  [ "${dxg_n:-0}" -gt 0 ] && echo "seat_fg: WARNING — the kernel log holds $dxg_n dxg residency/VA failure(s) since the distro started; a CUDA error in this start points there first (last: $(dmesg 2>/dev/null | grep -E "make_resident: Ioctl failed: -12|reserve_gpu_va.*-75" | tail -1 | cut -c1-160)). Restart the distro to clear it."
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
    pool_gib="$(awk -v f="$minfree" -v n="$nonkv" -v h="$SEAT_KV_HEADROOM_GIB" -v fl="$kvfloor" -v c="$kvcap" 'BEGIN{p=f/1024-n-h; if(p<fl)p=fl; if(p>c)p=c; printf "%.3f", p}')"
    pool_bytes="$(awk -v p="$pool_gib" 'BEGIN{printf "%d", p*1073741824}')"
    KVPIN=(--kv-cache-memory-bytes "$pool_bytes")
    echo "seat_fg: KV pool PINNED at ${pool_gib} GiB/worker = min free ${minfree} MiB on devices ${DEVS} − non-KV ${nonkv} GiB − headroom ${SEAT_KV_HEADROOM_GIB} GiB (floor ${kvfloor}, cap ${kvcap}) → --kv-cache-memory-bytes ${pool_bytes}; --gpu-memory-utilization ${SEAT_UTIL:-0.88} is ignored by vLLM while the pin is set"
  else
    echo "seat_fg: WARNING — SEAT_KV_HEADROOM_GIB set but $NVSMI answered nothing for devices ${DEVS}; falling back to --gpu-memory-utilization ${SEAT_UTIL:-0.88} — the pool is NOT pinned"
  fi
fi
KV_POLICY_JSON=""; [ -n "$KV_POLICY" ] && KV_POLICY_JSON=",\"kv_load_failure_policy\":\"$KV_POLICY\""
[ -n "$KV_POLICY" ] && echo "seat_fg: kv_load_failure_policy=$KV_POLICY (SEAT_KV_LOAD_FAILURE_POLICY)"
exec "$VENV/bin/vllm" serve "$MODEL" \
  --host 127.0.0.1 --port "$PORT" --served-model-name "$NAME" "${SEAT_ALIAS:-agent-pool}" \
  --max-model-len "${SEAT_MAX_LEN:-131072}" "${PAR[@]}" --gpu-memory-utilization "${SEAT_UTIL:-0.88}" \
  --max-num-seqs "${SEAT_SEQS:-32}" \
  --mamba-cache-mode align --enable-prefix-caching --max-num-batched-tokens "${SEAT_BATCHED:-1567}" "${KVPIN[@]}" \
  --kv-transfer-config "{\"kv_connector\":\"LMCacheMPConnector\",\"kv_role\":\"kv_both\"$KV_POLICY_JSON,\"kv_connector_extra_config\":{\"lmcache.mp.server_urls\":\"127.0.0.1:$MP_PORT\"}}" \
  "${EXTRA[@]}"
