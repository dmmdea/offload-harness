#!/usr/bin/env bash
# Launcher the harness spawns on demand (config `rknpu_sidecar_cmd`, ADR 0024 lane).
# Called by accelclient.SpawnCmd as:  rknpu-http.sh --idle-sec <n>   (the harness passes
# rknpu_idle_sec so config is the single source); a bare positional <n> is accepted too.
#
# RKNPU_HOME layout: $RKNPU_HOME/venv (rknn-toolkit-lite2 + numpy + pillow), $RKNPU_HOME/lib/librknnrt.so,
# $RKNPU_HOME/models (the manifest's artifacts), and this directory either copied FLAT into $RKNPU_HOME
# (the profiles.json seed: __RKNPU_HOME__/rknpu-http.sh) or as a checkout of accelerators/rknpu/ beneath it
# ($RKNPU_HOME/accelerators/rknpu/rknpu-http.sh). Both shapes resolve: the home is wherever venv/ is found
# walking up from here. Pins the venv's python explicitly: a transient unit or a sandboxed service has no
# PATH worth trusting. Build it once (the wheel is cp312 aarch64; fetch-models.sh downloads the pinned wheel
# and librknnrt.so, verified by sha256, into $RKNPU_HOME/wheels and $RKNPU_HOME/lib):
#     uv venv --python 3.12 "$RKNPU_HOME/venv"
#     uv pip install --python "$RKNPU_HOME/venv/bin/python" "$RKNPU_HOME"/wheels/rknn_toolkit_lite2-*.whl numpy pillow
#
# The runtime library: RKNN-Toolkit-Lite2 2.3.2 looks for librknnrt.so at /usr/lib only, and LD_LIBRARY_PATH
# does not change that (server.py explains the probe). The launcher exports RKNPU_RUNTIME_LIB (default
# $RKNPU_HOME/lib/librknnrt.so when that file exists) so the sidecar can load its private copy, and also puts
# $RKNPU_HOME/lib on LD_LIBRARY_PATH for anything that resolves the library by name.
#
# The box's own workload comes first: the process is pinned with taskset to RKNPU_CPUS (default 0-3, the
# A55 cluster on an RK3588), runs at nice 10, and numpy's thread pools are capped at one, because the
# sidecar's CPU work (JPEG decode, resize, box decode) is light and must stay on those cores. The NPU
# device node needs root or the render group; the launcher does not change privileges.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

idle="${RKNPU_IDLE_SEC:-300}"
while [ $# -gt 0 ]; do
  case "$1" in
    --idle-sec) shift; idle="${1:-$idle}" ;;
    --idle-sec=*) idle="${1#--idle-sec=}" ;;
    ''|*[!0-9]*) ;;            # anything else is ignored, never passed to python
    *) idle="$1" ;;
  esac
  shift
done
case "$idle" in ''|*[!0-9]*) echo "rknpu-http.sh: idle seconds must be an integer, got '$idle'" >&2; exit 2 ;; esac
export RKNPU_IDLE_SEC="$idle"

cpus="${RKNPU_CPUS:-0-3}"
case "$cpus" in *[!0-9,-]*) echo "rknpu-http.sh: RKNPU_CPUS must be a CPU list such as 0-3 or 0,2, got '$cpus'" >&2; exit 2 ;; esac

if [ -z "${RKNPU_HOME:-}" ]; then
  for cand in "$HERE" "$(dirname "$HERE")" "$(dirname "$(dirname "$HERE")")"; do
    if [ -d "$cand/venv" ]; then RKNPU_HOME="$cand"; break; fi
  done
  RKNPU_HOME="${RKNPU_HOME:-$(dirname "$HERE")}"
fi
export RKNPU_HOME
PY="${RKNPU_PYTHON:-$RKNPU_HOME/venv/bin/python}"
export RKNPU_MODELS_DIR="${RKNPU_MODELS_DIR:-$RKNPU_HOME/models}"
export RKNPU_MANIFEST="${RKNPU_MANIFEST:-$HERE/models.json}"
export RKNPU_PORT="${RKNPU_PORT:-18815}"
export RKNPU_BIND=127.0.0.1
if [ -z "${RKNPU_RUNTIME_LIB:-}" ] && [ -f "$RKNPU_HOME/lib/librknnrt.so" ]; then
  RKNPU_RUNTIME_LIB="$RKNPU_HOME/lib/librknnrt.so"
fi
[ -n "${RKNPU_RUNTIME_LIB:-}" ] && export RKNPU_RUNTIME_LIB
export LD_LIBRARY_PATH="$RKNPU_HOME/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export OMP_NUM_THREADS=1 OPENBLAS_NUM_THREADS=1
if [ ! -x "$PY" ]; then
  echo "rknpu-http.sh: no python at $PY (RKNPU_HOME=$RKNPU_HOME; build the venv, see the header of this script)" >&2
  exit 2
fi
LOG="${RKNPU_LOG_FILE:-$RKNPU_HOME/sidecar.log}"
# Detach: the harness's SpawnCmd uses Start (not Run) and this process must outlive the caller.
# setsid, taskset and nice are Linux (util-linux, coreutils); a bash without them (Git Bash running the
# contract suite) execs the interpreter directly.
wrap=()
command -v setsid >/dev/null 2>&1 && wrap+=(setsid)
command -v taskset >/dev/null 2>&1 && wrap+=(taskset -c "$cpus")
command -v nice >/dev/null 2>&1 && wrap+=(nice -n 10)
exec ${wrap[@]+"${wrap[@]}"} "$PY" "$HERE/server.py" >>"$LOG" 2>&1 < /dev/null
