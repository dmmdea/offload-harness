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
#   3. stages a copy of scripts/openspec/ (package.json and package-lock.json) in the scratch directory, next to
#      that verified tarball, and runs `npm ci --ignore-scripts` there. The CLI comes from the verified file and
#      every dependency is the exact version in the committed lockfile, checked against its sha512 integrity value;
#      npm ci never resolves a range and refuses a lockfile that disagrees with package.json. No lifecycle script of
#      the package or of its dependencies runs;
#   4. checks that the staged CLI reports the pinned version;
#   5. moves the staged node_modules into <prefix> (replacing an earlier install of this CLI there, refusing a
#      node_modules that is something else's), removes the scratch directory and prints the four environment
#      exports to use with the CLI.
#
# Where things go. Everything lands under <prefix>: node_modules, the npm cache (.npm-cache) and the scratch
# directory. The script writes nothing to the user-level npm prefix or npmrc, and sets npm's cache, the temp
# folders and the CLI's config home itself, so a system drive stays untouched. Use a prefix that is a directory
# of its own, on a data drive. There is no default prefix.
#
# What the pins cover. OPENSPEC_SHA256 pins the openspec tarball. The lockfile (regenerate it only with a new
# admission, see docs/systems/openspec.md) pins the other packages by exact version and sha512.
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

# The lockfile project sits beside this script.
project="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/openspec"
for f in package.json package-lock.json; do
  [ -f "$project/$f" ] || die "$project/$f is missing: the install needs the committed lockfile project (scripts/openspec/)"
done

export OPENSPEC_TELEMETRY=0 DO_NOT_TRACK=1 OPENSPEC_NO_UPDATE_CHECK=1

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | cut -d' ' -f1
  elif command -v openssl >/dev/null 2>&1; then openssl dgst -sha256 "$1" | sed 's/^.*= *//'
  else die "no sha256 tool (sha256sum, shasum or openssl) on PATH"
  fi
}

mkdir -p "$prefix"
# The absolute prefix is spelled one way everywhere: a Windows path from `pwd -W`, else the physical path from `pwd -P`
# (a symlinked temp folder such as macOS /var is resolved, so the paths this script logs and prints are the real ones).
abs="$(cd "$prefix" && { pwd -W 2>/dev/null || pwd -P; })"

# The install replaces <prefix>/node_modules wholesale (as npm ci does), so refuse a node_modules that is not an
# earlier install of this CLI, before anything runs.
if [ -n "$(ls -A "$abs/node_modules" 2>/dev/null)" ] && [ ! -f "$abs/node_modules/$OPENSPEC_PACKAGE/package.json" ]; then
  die "REFUSED: $abs/node_modules exists and holds no $OPENSPEC_PACKAGE install, so it is not this script's to replace. Use a prefix directory of its own."
fi

# npm's cache, logs and temp files stay under the prefix, and so does the CLI's config home.
work="$(mktemp -d "$abs/.openspec-pack.XXXXXX")"
trap 'rm -rf "$work"' EXIT
export npm_config_cache="$abs/.npm-cache"
export npm_config_update_notifier=false npm_config_fund=false npm_config_audit=false
export TMPDIR="$work" TMP="$work" TEMP="$work"
export XDG_CONFIG_HOME="$abs/config"

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

# The verified file goes into a staged copy of the lockfile project under the name package.json refers to.
stage="$work/project"
mkdir "$stage"
cp "$project/package.json" "$project/package-lock.json" "$stage/"
mv "$tarball" "$stage/fission-ai-openspec-${OPENSPEC_VERSION}.tgz"

( cd "$stage" && npm ci --ignore-scripts --loglevel=warn >&2 ) \
  || die "npm ci from the committed lockfile failed; nothing was installed"

staged_bin="$stage/node_modules/.bin/openspec"
[ -e "$staged_bin" ] || die "npm ci finished but $staged_bin does not exist"
got="$("$staged_bin" --version 2>/dev/null | tr -d '[:space:]')" || got="(it did not run)"
[ "$got" = "$OPENSPEC_VERSION" ] || die "the installed CLI reports version '$got', expected $OPENSPEC_VERSION; nothing was installed"

rm -rf "$abs/node_modules"
mv "$stage/node_modules" "$abs/node_modules"

bin="$abs/node_modules/.bin/openspec"
[ -e "$bin" ] || die "the install finished but $bin does not exist"

echo "# openspec $OPENSPEC_VERSION installed in $abs; use these in the shell that runs it:"
echo "export OPENSPEC_TELEMETRY=0"
echo "export DO_NOT_TRACK=1"
echo "export OPENSPEC_NO_UPDATE_CHECK=1"
echo "export XDG_CONFIG_HOME='$abs/config'"
echo "# binary: $bin   (point OPENSPEC_BIN at it for scripts/openspec-validate.sh and the Go test)"
