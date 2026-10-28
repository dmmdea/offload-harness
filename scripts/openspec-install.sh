#!/usr/bin/env bash
# openspec-install.sh -- install the newest OpenSpec CLI into a prefix you name (docs/systems/openspec.md).
#
# Usage:  scripts/openspec-install.sh <prefix>          (or OPENSPEC_PREFIX=<prefix> scripts/openspec-install.sh)
#
# What it does, in this order:
#   1. stages a copy of scripts/openspec/package.json (one dependency, @fission-ai/openspec at the `latest` dist-tag:
#      the scoped name; the unscoped npm package `openspec` belongs to another publisher) in a scratch directory
#      under <prefix> and runs `npm install --ignore-scripts` there. npm resolves the registry's newest release and
#      checks every tarball against the sha512 integrity value the registry publishes for it; the lockfile that run
#      writes is a by-product of the install, not an input, and is discarded with the scratch directory. No lifecycle
#      script of the package or of its dependencies runs;
#   2. verifies what was installed: the package's own package.json names a version, the CLI reports that version,
#      the integrity npm recorded for the package equals the registry's `dist.integrity` for that exact version
#      (a second, separate registry query), and `npm audit signatures` passes for the whole installed tree
#      (registry signatures, and provenance attestations where a package has them);
#   3. moves the staged node_modules into <prefix> (replacing an earlier install of this CLI there, refusing a
#      node_modules that is something else's), removes the scratch directory and prints the four environment
#      exports to use with the CLI.
#
# Where things go. Everything lands under <prefix>: node_modules, the npm cache (.npm-cache) and the scratch
# directory. The script writes nothing to the user-level npm prefix or npmrc, and sets npm's cache, the temp
# folders and the CLI's config home itself, so a system drive stays untouched. Use a prefix that is a directory
# of its own, on a data drive. There is no default prefix.
#
# What the integrity covers. Nothing is pinned here: the version is whatever the registry calls newest when the
# script runs, and the integrity comes from the registry at that moment. Re-run the script to update.
#
# Telemetry. The CLI sends usage telemetry unless OPENSPEC_TELEMETRY=0 or DO_NOT_TRACK=1, and checks the registry
# for a newer version unless OPENSPEC_NO_UPDATE_CHECK=1. This script sets all three for its own runs and prints
# them for yours.
set -euo pipefail

# An exported CDPATH makes `cd <relative dir>` look in other folders first and print the one it chose, which would put a
# second line into every `$(cd ... && pwd)` below. Clear it for the whole script, so no cd here or added later has to
# remember to.
unset CDPATH

readonly OPENSPEC_PACKAGE="@fission-ai/openspec"

die() { echo "openspec-install: $*" >&2; exit 1; }

prefix="${1:-${OPENSPEC_PREFIX:-}}"
[ -n "$prefix" ] || die "usage: scripts/openspec-install.sh <prefix>   (or set OPENSPEC_PREFIX); the prefix is the directory the CLI is installed into, on a data drive"
command -v npm >/dev/null 2>&1 || die "npm is not on PATH"

# The install project sits beside this script.
project="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/openspec"
[ -f "$project/package.json" ] || die "$project/package.json is missing: the install needs the install project (scripts/openspec/)"

export OPENSPEC_TELEMETRY=0 DO_NOT_TRACK=1 OPENSPEC_NO_UPDATE_CHECK=1

mkdir -p "$prefix"
# The absolute prefix is spelled one way everywhere: a Windows path from `pwd -W`, else the physical path from `pwd -P`
# (a symlinked temp folder such as macOS /var is resolved, so the paths this script logs and prints are the real ones).
abs="$(cd "$prefix" && { pwd -W 2>/dev/null || pwd -P; })"

# The install replaces <prefix>/node_modules wholesale, so refuse a node_modules that is not an
# earlier install of this CLI, before anything runs.
if [ -n "$(ls -A "$abs/node_modules" 2>/dev/null)" ] && [ ! -f "$abs/node_modules/$OPENSPEC_PACKAGE/package.json" ]; then
  die "REFUSED: $abs/node_modules exists and holds no $OPENSPEC_PACKAGE install, so it is not this script's to replace. Use a prefix directory of its own."
fi

# npm's cache, logs and temp files stay under the prefix, and so does the CLI's config home.
work="$(mktemp -d "$abs/.openspec-pack.XXXXXX")"
trap 'rm -rf "$work"' EXIT
export npm_config_cache="$abs/.npm-cache"
export npm_config_update_notifier=false npm_config_fund=false
export TMPDIR="$work" TMP="$work" TEMP="$work"
export XDG_CONFIG_HOME="$abs/config"

# A staged copy of the install project; the install happens there, never in <prefix>.
stage="$work/project"
mkdir "$stage"
cp "$project/package.json" "$stage/"

( cd "$stage" && npm install --ignore-scripts --no-audit --no-fund --loglevel=warn >&2 )   || die "npm install of ${OPENSPEC_PACKAGE}@latest failed; nothing was installed"

staged_bin="$stage/node_modules/.bin/openspec"
[ -e "$staged_bin" ] || die "npm install finished but $staged_bin does not exist"

# The version npm installed, read from the installed package itself (node is npm's own runtime, so it is here).
pkgdir="$stage/node_modules/$OPENSPEC_PACKAGE"
version="$(node -p "require(process.argv[1]).version" "$pkgdir/package.json" 2>/dev/null | tr -d '[:space:]')" || version=""
[ -n "$version" ] || die "the installed $OPENSPEC_PACKAGE has no readable version; nothing was installed"
got="$("$staged_bin" --version 2>/dev/null | tr -d '[:space:]')" || got="(it did not run)"
[ "$got" = "$version" ] || die "the installed CLI reports version '$got', its package.json says $version; nothing was installed"

# Integrity: what npm recorded for the package while installing must be what the registry publishes for that exact
# version, asked a second time and separately.
recorded="$(node -p "const l=require(process.argv[1]); l.packages['node_modules/'+process.argv[2]].integrity" "$stage/package-lock.json" "$OPENSPEC_PACKAGE" 2>/dev/null | tr -d '[:space:]')" || recorded=""
published="$(cd "$stage" && npm view "${OPENSPEC_PACKAGE}@${version}" dist.integrity 2>/dev/null | tr -d '[:space:]')" || published=""
[ -n "$recorded" ] && [ -n "$published" ] || die "could not read the integrity of ${OPENSPEC_PACKAGE}@${version} (installed: '${recorded}', registry: '${published}'); nothing was installed"
[ "$recorded" = "$published" ] || die "REFUSED: integrity mismatch for ${OPENSPEC_PACKAGE}@${version}: installed $recorded, registry $published. Nothing was installed."
echo "openspec-install: ${OPENSPEC_PACKAGE}@${version} integrity matches the registry ($published)" >&2

( cd "$stage" && npm audit signatures --loglevel=warn >&2 )   || die "REFUSED: npm audit signatures failed for the installed tree of ${OPENSPEC_PACKAGE}@${version}. Nothing was installed."

rm -rf "$abs/node_modules"
mv "$stage/node_modules" "$abs/node_modules"

bin="$abs/node_modules/.bin/openspec"
[ -e "$bin" ] || die "the install finished but $bin does not exist"

echo "# openspec $version installed in $abs; use these in the shell that runs it:"
echo "export OPENSPEC_TELEMETRY=0"
echo "export DO_NOT_TRACK=1"
echo "export OPENSPEC_NO_UPDATE_CHECK=1"
echo "export XDG_CONFIG_HOME='$abs/config'"
echo "# binary: $bin   (point OPENSPEC_BIN at it for scripts/openspec-validate.sh and the Go test)"
