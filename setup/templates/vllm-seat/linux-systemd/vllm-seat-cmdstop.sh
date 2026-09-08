#!/usr/bin/env bash
# llama-swap `cmdStop` for the persistent vLLM seat: a real stop of the unit, so an unload through llama-swap (the
# harness's `gpu reserve --drain --unload-seat`, /api/models/unload, a llama-swap restart) truly frees the card. The cmd
# wrapper exits on its own once the unit is inactive (<= 5 s); the entry's unloadTimeout covers the unit's TimeoutStopSec.
set -u
exec systemctl stop __UNIT__.service
