#!/usr/bin/env bash
# Launcher the harness spawns on demand (config `coral_sidecar_cmd`, ADR 0024 lane; Coral D7).
# Called by accelclient.SpawnCmd as:  coral-http.sh --idle-sec <n>   (the harness passes
# coral_idle_sec so config is the single source); a bare positional <n> is accepted too.
#
# CORAL_HOME layout (D8): $CORAL_HOME/venv (ai-edge-litert + numpy + pillow, built from the box's
# staged wheels), $CORAL_HOME/models (the manifest's artifacts), and this directory either copied
# FLAT into $CORAL_HOME (the profiles.json seed: __CORAL_HOME__/coral-http.sh) or as a checkout of
# accelerators/coral/ beneath it ($CORAL_HOME/accelerators/coral/coral-http.sh, the Lenovo). Both
# shapes resolve: the home is wherever venv/ is found walking up from here. Pins the venv's python
# explicitly: a transient unit or a sandboxed service has no PATH worth trusting.
#
# Found live 2026-09-08 (the first harness-path gate): the launcher looked one level up only, so
# under the fleet unit it named a venv that did not exist and exited 2 before the sidecar started;
# and it read "$1" as the idle seconds, which the harness passes as "--idle-sec 300".
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

idle="${CORAL_IDLE_SEC:-300}"
while [ $# -gt 0 ]; do
  case "$1" in
    --idle-sec) shift; idle="${1:-$idle}" ;;
    --idle-sec=*) idle="${1#--idle-sec=}" ;;
    ''|*[!0-9]*) ;;            # anything else is ignored, never passed to python
    *) idle="$1" ;;
  esac
  shift
done
case "$idle" in ''|*[!0-9]*) echo "coral-http.sh: idle seconds must be an integer, got '$idle'" >&2; exit 2 ;; esac
export CORAL_IDLE_SEC="$idle"

if [ -z "${CORAL_HOME:-}" ]; then
  for cand in "$HERE" "$(dirname "$HERE")" "$(dirname "$(dirname "$HERE")")"; do
    if [ -d "$cand/venv" ]; then CORAL_HOME="$cand"; break; fi
  done
  CORAL_HOME="${CORAL_HOME:-$(dirname "$HERE")}"
fi
export CORAL_HOME
PY="${CORAL_PYTHON:-$CORAL_HOME/venv/bin/python}"
export CORAL_MODELS_DIR="${CORAL_MODELS_DIR:-$CORAL_HOME/models}"
export CORAL_MANIFEST="${CORAL_MANIFEST:-$HERE/models.json}"
export CORAL_PORT="${CORAL_PORT:-18814}"
export CORAL_BIND=127.0.0.1
if [ ! -x "$PY" ]; then
  echo "coral-http.sh: no python at $PY (CORAL_HOME=$CORAL_HOME; build the venv from the staged litert wheels)" >&2
  exit 2
fi
LOG="${CORAL_LOG_FILE:-$CORAL_HOME/sidecar.log}"
# Detach: the harness's SpawnCmd uses Start (not Run) and this process must outlive the caller.
# setsid is Linux (util-linux); a bash without it (Git Bash running the contract suite) execs directly.
if command -v setsid >/dev/null 2>&1; then
  exec setsid "$PY" "$HERE/server.py" >>"$LOG" 2>&1 < /dev/null
fi
exec "$PY" "$HERE/server.py" >>"$LOG" 2>&1 < /dev/null
