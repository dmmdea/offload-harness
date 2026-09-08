#!/usr/bin/env bash
# Launcher the harness spawns on demand (config `coral_sidecar_cmd`, ADR 0024 lane; Coral D7).
# Called as: coral-http.sh <idle_sec>   — the harness passes coral_idle_sec so config is the single source.
#
# CORAL_HOME layout (D8): $CORAL_HOME/venv (ai-edge-litert + numpy + pillow, built from the box's
# staged wheels), $CORAL_HOME/models (the manifest's artifacts), and this repo's accelerators/coral/
# checked out or copied beside it. Pins the venv's python explicitly: a transient unit or a
# sandboxed service has no PATH worth trusting.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CORAL_HOME="${CORAL_HOME:-$(dirname "$HERE")}"
PY="${CORAL_PYTHON:-$CORAL_HOME/venv/bin/python}"
export CORAL_MODELS_DIR="${CORAL_MODELS_DIR:-$CORAL_HOME/models}"
export CORAL_MANIFEST="${CORAL_MANIFEST:-$HERE/models.json}"
export CORAL_IDLE_SEC="${1:-${CORAL_IDLE_SEC:-300}}"
export CORAL_PORT="${CORAL_PORT:-18814}"
export CORAL_BIND=127.0.0.1
if [ ! -x "$PY" ]; then
  echo "coral-http.sh: no python at $PY (build the venv: $CORAL_HOME/venv from the staged litert wheels)" >&2
  exit 2
fi
LOG="${CORAL_LOG_FILE:-$CORAL_HOME/sidecar.log}"
# Detach: the harness's SpawnCmd uses Start (not Run) and this process must outlive the caller.
exec setsid "$PY" "$HERE/server.py" >>"$LOG" 2>&1 < /dev/null
