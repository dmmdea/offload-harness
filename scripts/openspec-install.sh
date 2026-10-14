#!/usr/bin/env bash
# openspec-install.sh -- install the pinned OpenSpec CLI into a prefix you name (docs/systems/openspec.md).
#
# Usage:  scripts/openspec-install.sh <prefix>          (or OPENSPEC_PREFIX=<prefix> scripts/openspec-install.sh)
#
# What it does, in this order:
#   1. fetches the package tarball into a scratch directory under <prefix> with
#      `npm pack @fission-ai/openspec@<version> --ignore-scripts` (the scoped name; the unscoped npm package
#      `openspec` belongs to another publisher);
#   2. computes the tarball's sha256 and REFUSES, installing nothing, when it is not the pinned one;
#   3. installs FROM THAT FILE: `npm install --prefix <prefix> <tarball> --ignore-scripts --no-save` (no lifecycle
#      script of the package or of its dependencies runs; --no-save because the tarball lives in a scratch directory
#      that step 5 removes, and a package.json entry pointing into it would break every later npm command);
#   4. checks that the installed CLI reports the pinned version;
#   5. removes the scratch directory and prints the four environment exports to use with the CLI.
#
# Where things go. Everything lands under <prefix>: node_modules, the npm cache (.npm-cache) and the scratch
# directory. The script writes nothing to the user-level npm prefix or npmrc, and sets npm's cache and the temp
# folders itself, so a system drive stays untouched. Pass a prefix on a data drive. There is no default prefix.
#
# What the pin covers. OPENSPEC_SHA256 pins the openspec tarball itself. Its dependencies are resolved by npm
# from the registry at install time (each under npm's own sha512 integrity check) and are not pinned here.
#
# Telemetry. The CLI sends usage telemetry unless OPENSPEC_TELEMETRY=0 or DO_NOT_TRACK=1, and checks the registry
# for a newer version unless OPENSPEC_NO_UPDATE_CHECK=1. This script sets all three for its own runs and prints
# them for yours.
set -euo pipefail

readonly OPENSPEC_VERSION="1.14.1"
readonly OPENSPEC_SHA256="4a88e334938316db6916fd4f3aaf2213e6fc23b4fe327efd2afe7754c2d3b0bf"
readonly OPENSPEC_PACKAGE="@fission-ai/openspec"

die() { echo "openspec-install: $*" >&2; exit 1; }

prefix="${1:-${OPENSPEC_PREFIX:-}}"
[ -n "$prefix" ] || die "usage: scripts/openspec-install.sh <prefix>   (or set OPENSPEC_PREFIX); the prefix is the directory the CLI is installed into, on a data drive"
command -v npm >/dev/null 2>&1 || die "npm is not on PATH"

export OPENSPEC_TELEMETRY=0 DO_NOT_TRACK=1 OPENSPEC_NO_UPDATE_CHECK=1

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | cut -d' ' -f1
  elif command -v openssl >/dev/null 2>&1; then openssl dgst -sha256 "$1" | sed 's/^.*= *//'
  else die "no sha256 tool (sha256sum, shasum or openssl) on PATH"
  fi
}

mkdir -p "$prefix"
abs="$(cd "$prefix" && { pwd -W 2>/dev/null || pwd; })"

# npm's cache, logs and temp files stay under the prefix.
work="$(mktemp -d "$abs/.openspec-pack.XXXXXX")"
trap 'rm -rf "$work"' EXIT
export npm_config_cache="$abs/.npm-cache"
export npm_config_update_notifier=false npm_config_fund=false npm_config_audit=false
export TMPDIR="$work" TMP="$work" TEMP="$work"

( cd "$work" && npm pack "${OPENSPEC_PACKAGE}@${OPENSPEC_VERSION}" --ignore-scripts --loglevel=warn >/dev/null ) \
  || die "npm pack ${OPENSPEC_PACKAGE}@${OPENSPEC_VERSION} failed"
tarballs=("$work"/*.tgz)
[ "${#tarballs[@]}" -eq 1 ] && [ -f "${tarballs[0]}" ] || die "expected exactly one tarball from npm pack in $work"
tarball="${tarballs[0]}"

actual="$(sha256_of "$tarball")"
if [ "$actual" != "$OPENSPEC_SHA256" ]; then
  die "REFUSED: sha256 mismatch for ${OPENSPEC_PACKAGE}@${OPENSPEC_VERSION}: expected $OPENSPEC_SHA256, got $actual. Nothing was installed."
fi
echo "openspec-install: tarball sha256 verified ($OPENSPEC_SHA256)" >&2

npm install --prefix "$abs" "$tarball" --ignore-scripts --no-save >&2 \
  || die "npm install into $abs failed"

bin="$abs/node_modules/.bin/openspec"
[ -e "$bin" ] || die "the install finished but $bin does not exist"
got="$("$bin" --version 2>/dev/null | tr -d '[:space:]')"
[ "$got" = "$OPENSPEC_VERSION" ] || die "the installed CLI reports version '$got', expected $OPENSPEC_VERSION"

echo "# openspec $OPENSPEC_VERSION installed in $abs; use these in the shell that runs it:"
echo "export OPENSPEC_TELEMETRY=0"
echo "export DO_NOT_TRACK=1"
echo "export OPENSPEC_NO_UPDATE_CHECK=1"
echo "export XDG_CONFIG_HOME='$abs/config'"
echo "# binary: $bin   (point OPENSPEC_BIN at it for scripts/openspec-validate.sh and the Go test)"
