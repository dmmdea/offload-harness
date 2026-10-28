#!/usr/bin/env bash
# refresh-llamaswap-schema.sh -- refresh the vendored llama-swap config schema from the newest upstream.
#
# Usage:  scripts/refresh-llamaswap-schema.sh
#
# Fetches config-schema.json from the default branch of the llama-swap repository into
# tools/llamaswap/internal/lsconfig/schema/ and records, in schema/snapshot.json beside it, the retrieval date and the
# newest llama-swap release at that moment (the version the snapshot is read as carrying). It prints that version.
# Nothing is written unless both fetches succeed and the schema parses as JSON. The snapshot is what `config validate`
# judges against and what `version drift` treats as a FLOOR: only a server older than it is drift, a newer one is fine.
# Run it when upstream ships config keys the snapshot does not know, then re-run
# `cd tools/llamaswap && go test ./internal/lsconfig/... ./internal/cli/ -count=1`.
set -euo pipefail
unset CDPATH

repo="mostlygeek/llama-swap"
dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/tools/llamaswap/internal/lsconfig/schema"
[ -d "$dir" ] || { echo "refresh-llamaswap-schema: $dir does not exist" >&2; exit 1; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

curl -fsSL "https://raw.githubusercontent.com/$repo/HEAD/config-schema.json" -o "$work/config-schema.json" \
  || { echo "refresh-llamaswap-schema: could not fetch the schema from $repo" >&2; exit 1; }
curl -fsSL -H "Accept: application/vnd.github+json" "https://api.github.com/repos/$repo/releases/latest" -o "$work/release.json" \
  || { echo "refresh-llamaswap-schema: could not read the newest release of $repo" >&2; exit 1; }

version="$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$work/release.json" | head -n 1)"
case "$version" in
  v[0-9]*) ;;
  *) echo "refresh-llamaswap-schema: no usable release tag in the answer (got '$version')" >&2; exit 1 ;;
esac
grep -q '"\$schema"' "$work/config-schema.json" && grep -q '"properties"' "$work/config-schema.json" \
  || { echo "refresh-llamaswap-schema: the downloaded file does not look like the config schema" >&2; exit 1; }

retrieved="$(date -u +%Y-%m-%d)"
cp "$work/config-schema.json" "$dir/config-schema.json"
printf '{\n  "llama_swap_version": "%s",\n  "retrieved": "%s"\n}\n' "$version" "$retrieved" > "$dir/snapshot.json"

echo "llama-swap config schema refreshed: carries $version (retrieved $retrieved)"
echo "  $dir/config-schema.json"
echo "  $dir/snapshot.json"
