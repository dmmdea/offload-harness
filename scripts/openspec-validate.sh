#!/usr/bin/env bash
# openspec-validate.sh -- the OpenSpec gate: validate every spec and change under openspec/, strictly, and the
# project-local schemas (docs/systems/openspec.md).
#
# Usage:  OPENSPEC_BIN=<path to the openspec binary> scripts/openspec-validate.sh
#         (OPENSPEC_BIN defaults to `openspec` on PATH; scripts/openspec-install.sh prints the path it installed)
#
# The three switches are set here whatever the caller exported: the CLI sends usage telemetry unless
# OPENSPEC_TELEMETRY=0 or DO_NOT_TRACK=1, and checks the registry for a newer version unless
# OPENSPEC_NO_UPDATE_CHECK=1. XDG_CONFIG_HOME is the caller's (scripts/openspec-install.sh prints one under the
# install prefix); this script writes no OpenSpec config.
#
# `openspec validate --all` reads specs and changes, not schemas, so each openspec/schemas/<name>/ also goes
# through `openspec schema validate <name>`. Exit code: 0 only when every call passes.
set -euo pipefail

# An exported CDPATH makes `cd <relative dir>` look in other folders first and print the one it chose, which would put a
# second line into every `$(cd ... && pwd)` below. Clear it for the whole script, so no cd here or added later has to
# remember to.
unset CDPATH

export OPENSPEC_TELEMETRY=0 DO_NOT_TRACK=1 OPENSPEC_NO_UPDATE_CHECK=1

# OPENSPEC_BIN is the caller's, so a relative path means relative to the caller's directory: make it absolute before the
# cd below moves to the repository root. A bare name (no slash) is looked up on PATH as it is.
bin="${OPENSPEC_BIN:-openspec}"
case "$bin" in
  /* | [A-Za-z]:*) ;;
  */*)
    bin_dir="$(dirname "$bin")"
    if [ -d "$bin_dir" ]; then bin="$(cd "$bin_dir" && pwd)/$(basename "$bin")"; fi
    ;;
esac

cd "$(dirname "${BASH_SOURCE[0]}")/.."

if ! command -v "$bin" >/dev/null 2>&1 && [ ! -x "$bin" ]; then
  echo "openspec-validate: '$bin' not found; install the CLI with scripts/openspec-install.sh and set OPENSPEC_BIN" >&2
  exit 2
fi

"$bin" validate --all --strict --no-interactive

for dir in openspec/schemas/*/; do
  [ -d "$dir" ] || continue
  "$bin" schema validate "$(basename "$dir")"
done
