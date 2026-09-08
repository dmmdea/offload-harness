#!/usr/bin/env bash
# Fetch the Coral zoo artifacts named in models.json into CORAL_MODELS_DIR and verify every
# sha256 (Coral D7). Source: google-coral/test_data (Apache-2.0), raw GitHub URLs. A file whose
# hash does not match is deleted and reported — the sidecar refuses to serve a mismatched file,
# so leaving one on disk would only move the failure to first use.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFEST="${CORAL_MANIFEST:-$HERE/models.json}"
DEST="${CORAL_MODELS_DIR:-$(dirname "$HERE")/models}"
BASE="${CORAL_ZOO_BASE:-https://raw.githubusercontent.com/google-coral/test_data/master}"
mkdir -p "$DEST"
fail=0
# Every file the manifest names: model files, their labels, and the test images.
python3 - "$MANIFEST" <<'PY' | while read -r name sha; do
import json, sys
m = json.load(open(sys.argv[1], encoding="utf-8"))
seen = set()
for spec in m["models"].values():
    for key in ("file", "labels"):
        f = spec.get(key)
        if f and f not in seen:
            seen.add(f); print(f, spec["sha256"] if key == "file" else m["labels"].get(f, ""))
for f, sha in m.get("testdata", {}).items():
    if f not in seen:
        seen.add(f); print(f, sha)
PY
  out="$DEST/$name"
  if [ -f "$out" ] && [ -n "$sha" ] && [ "$(sha256sum "$out" | cut -d' ' -f1)" = "$sha" ]; then
    echo "ok       $name"; continue
  fi
  echo "fetching $name"
  if ! curl -fsSL --retry 3 --max-time 300 -o "$out.part" "$BASE/$name"; then
    echo "FAILED   $name (download)"; rm -f "$out.part"; fail=1; continue
  fi
  if [ -n "$sha" ]; then
    got="$(sha256sum "$out.part" | cut -d' ' -f1)"
    if [ "$got" != "$sha" ]; then
      echo "FAILED   $name (sha256 $got != $sha) — removed"; rm -f "$out.part"; fail=1; continue
    fi
  fi
  mv "$out.part" "$out"; echo "ok       $name"
done
exit $fail
