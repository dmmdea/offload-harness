#!/usr/bin/env bash
# Launcher llama-swap runs for each `rkllm` seat (an NPU LLM/VLM seat). The seat's cmd is
#   rkllm-serve.sh --model <.rkllm> [--vision-encoder <.rknn>] --ctx-size N --cpu-mask 0x0f --served-name NAME \
#                  --port ${PORT} --host 127.0.0.1
# and every argument is passed straight through to rkllm_server.py.
#
# llama-swap owns the process: this script EXECs python (no setsid, no log redirect, no wrapper left behind), so the
# SIGTERM llama-swap sends when the seat's ttl runs out reaches the server, which aborts a running generation and
# calls rkllm_destroy before it exits.
#
# RKNPU_HOME layout, resolved the way coral-http.sh resolves CORAL_HOME: $RKNPU_HOME/venv (numpy + Pillow, needed only
# by a vision seat) and $RKNPU_HOME/lib (librkllmrt.so + librknnrt.so, put on LD_LIBRARY_PATH because the runtime loads
# librknnrt.so by name; the server's --lib / --rknn-lib default to it). This directory sits either FLAT in $RKNPU_HOME
# (the profiles.json seed: __RKNPU_HOME__/rkllm-serve.sh) or as a checkout of accelerators/rknpu/ beneath it. The home
# is wherever venv/ is found walking up from here. The venv's python is pinned explicitly: a transient unit or a
# sandboxed service has no PATH worth trusting.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ -z "${RKNPU_HOME:-}" ]; then
  for cand in "$HERE" "$(dirname "$HERE")" "$(dirname "$(dirname "$HERE")")"; do
    if [ -d "$cand/venv" ]; then RKNPU_HOME="$cand"; break; fi
  done
  RKNPU_HOME="${RKNPU_HOME:-$(dirname "$HERE")}"
fi
export RKNPU_HOME
PY="${RKNPU_PYTHON:-$RKNPU_HOME/venv/bin/python}"
if [ ! -x "$PY" ]; then
  echo "rkllm-serve.sh: no python at $PY (RKNPU_HOME=$RKNPU_HOME; build the venv with numpy and Pillow)" >&2
  exit 2
fi
export LD_LIBRARY_PATH="$RKNPU_HOME/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
exec "$PY" "$HERE/rkllm_server.py" "$@"
