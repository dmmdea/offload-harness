#!/usr/bin/env bash
# Launcher the harness spawns on demand (config `axera_sidecar_cmd`, ADR 0024 lane) for the Axera AX650-class
# NPU card. Called by accelclient.SpawnCmd as:  axera-http.sh --idle-sec <n>   (the harness passes
# axera_idle_sec so config is the single source); a bare positional <n> is accepted too.
#
#     axera-http.sh --telemetry     one telemetry JSON document on stdout, then exit (offload_status and the
#                                   node's /fleet/health call this while the sidecar is down; never starts it)
#     axera-http.sh --apply-perf    card CPU to its top clock, report as JSON, exit (axera-perf.sh runs it at boot)
#
# AXERA_HOME layout: $AXERA_HOME/venv (pyaxengine + numpy + pillow + opencv-python-headless + onnxruntime +
# tokenizers + shapely + pyclipper), $AXERA_HOME/models (the manifest's files, fetched and verified by
# fetch-models.sh), $AXERA_HOME/wheels (the pyaxengine wheel), $AXERA_HOME/cache (derived
# embeddings), and this directory either copied FLAT into $AXERA_HOME (the profiles.json seed:
# __AXERA_HOME__/axera-http.sh) or as a checkout of accelerators/axera/ beneath it. Both shapes resolve: the
# home is the first ancestor holding venv/. The AXCL host runtime (driver + /usr/lib/axcl) is the vendor's
# package, installed once per host. Build the venv once:
#     uv venv --python 3.14 "$AXERA_HOME/venv"
#     uv pip install --python "$AXERA_HOME/venv/bin/python" "$AXERA_HOME"/wheels/axengine-*.whl \
#         numpy pillow opencv-python-headless onnxruntime tokenizers shapely pyclipper
#
# No CPU pinning and no nice: the sidecar's host work (decode, resize, box and mask post-processing) is light
# and the card is the bottleneck; thread pools are capped at two so it never crowds the host's own stack.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

idle="${AXERA_IDLE_SEC:-300}"
mode=serve
while [ $# -gt 0 ]; do
  case "$1" in
    --idle-sec) shift; idle="${1:-$idle}" ;;
    --idle-sec=*) idle="${1#--idle-sec=}" ;;
    --telemetry) mode=telemetry ;;
    --apply-perf) mode=perf ;;
    ''|*[!0-9]*) ;;            # anything else is ignored, never passed to python
    *) idle="$1" ;;
  esac
  shift
done
case "$idle" in ''|*[!0-9]*) echo "axera-http.sh: idle seconds must be an integer, got '$idle'" >&2; exit 2 ;; esac
export AXERA_IDLE_SEC="$idle"

if [ -z "${AXERA_HOME:-}" ]; then
  for cand in "$HERE" "$(dirname "$HERE")" "$(dirname "$(dirname "$HERE")")"; do
    if [ -d "$cand/venv" ]; then AXERA_HOME="$cand"; break; fi
  done
  AXERA_HOME="${AXERA_HOME:-$(dirname "$HERE")}"
fi
export AXERA_HOME
PY="${AXERA_PYTHON:-$AXERA_HOME/venv/bin/python}"
if [ ! -x "$PY" ]; then
  echo "axera-http.sh: no python at $PY (AXERA_HOME=$AXERA_HOME; build the venv, see the header of this script)" >&2
  exit 2
fi
export LD_LIBRARY_PATH="/usr/lib/axcl${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"

case "$mode" in
  telemetry) exec "$PY" "$HERE/axcl_telemetry.py" ;;
  perf) exec "$PY" "$HERE/axcl_telemetry.py" --apply-perf ;;
esac

export AXERA_MODELS_DIR="${AXERA_MODELS_DIR:-$AXERA_HOME/models}"
export AXERA_MANIFEST="${AXERA_MANIFEST:-$HERE/models.json}"
export AXERA_PORT="${AXERA_PORT:-18816}"
export AXERA_BIND=127.0.0.1
export OMP_NUM_THREADS=2 OPENBLAS_NUM_THREADS=2
LOG="${AXERA_LOG_FILE:-$AXERA_HOME/sidecar.log}"
# Detach: the harness's SpawnCmd uses Start (not Run) and this process must outlive the caller.
wrap=()
command -v setsid >/dev/null 2>&1 && wrap+=(setsid)
exec ${wrap[@]+"${wrap[@]}"} "$PY" "$HERE/server.py" >>"$LOG" 2>&1 < /dev/null
