#!/usr/bin/env bash
# Boot-time tuning for the Axera AX650-class card(s), run once by axera-perf.service (a oneshot unit).
# The card boots with its CPU governor at userspace 1.2 GHz of a 1.5 GHz table; this pins it to the top step
# (axera-http.sh --apply-perf) as soon as the card answers. The sidecar applies the same setting at every
# spawn, so the unit only removes the window between boot and the first call (and covers the NPU LLM seat).
# The NPU (800 MHz) and NN-bus (1.6 GHz) clocks ship at their maxima and are left alone, as are the thermal
# trips (80/105/120 C): the heatsink and fan keep the card far below the first one.
# Waits up to AXERA_PERF_WAIT_SEC (default 180) for the card to finish booting its firmware, then exits 0
# with the JSON report on stdout (the journal), or 1 if the card never answered.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
wait="${AXERA_PERF_WAIT_SEC:-180}"
case "$wait" in ''|*[!0-9]*) wait=180 ;; esac
end=$(( $(date +%s) + wait ))
last=""
while :; do
  last="$("$HERE/axera-http.sh" --apply-perf 2>&1)"
  case "$last" in *'"at_max": true'*) echo "$last"; exit 0 ;; esac
  [ "$(date +%s)" -ge "$end" ] && break
  sleep 5
done
echo "axera-perf.sh: the card did not reach its top CPU clock within ${wait}s; last report: $last" >&2
exit 1
