#!/usr/bin/env bash
# Fetch and verify the Axera lane's runtime pieces and models into AXERA_HOME (default: the first ancestor of
# this directory holding venv/ or models/). A thin wrapper over fetch_models.py, which reads models.json,
# resolves every source at run time (ADR 0099: no pins) and checks each file against its publisher's digest.
#     fetch-models.sh                       everything (runtime wheels and binaries, then every model)
#     fetch-models.sh --models --only rfdetr-s,siglip2-b16-224
#     fetch-models.sh --dry-run             show what would be fetched, write nothing
#     fetch-models.sh --verify              re-hash what is on disk instead of trusting size + record
# Standard library Python only, so it runs before the venv exists.
set -eu
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PY="$(command -v python3 || true)"
[ -n "$PY" ] || { echo "fetch-models.sh: python3 not found" >&2; exit 2; }
exec "$PY" "$HERE/fetch_models.py" "$@"
