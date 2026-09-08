#!/usr/bin/env bash
# llama-swap `cmd` for the persistent vLLM seat: make sure the unit is up, then stay attached as the seat's upstream
# process (llama-swap treats this process as the model; it health-checks the proxy URL, not us). Runs as llama-swap's
# user (NoNewPrivileges, so no sudo): the start goes through the polkit rule 50-llama-swap-vllm-seat.rules.
# Detaches (exit 1, so llama-swap marks the seat stopped and re-attaches + health-waits on the next request) when:
#   - the unit leaves active/activating/reloading (`activating` = its own auto-restart window, e.g. at boot — alive;
#     llama-swap's healthCheckTimeout is the real ceiling), or
#   - the unit's InvocationID changes = somebody restarted it (systemd after a crash, an operator's `systemctl restart`):
#     the engine is then 35–250 s from serving and llama-swap must not keep proxying to it as `ready`.
set -u
U=__UNIT__.service
systemctl reset-failed "$U" 2>/dev/null
systemctl start "$U" || { echo "vllm-seat-cmd: systemctl start $U failed (polkit? unit?)" >&2; exit 1; }
inv=$(systemctl show -p InvocationID --value "$U" 2>/dev/null)
trap 'exit 0' TERM INT
while :; do
  st=$(systemctl show -p ActiveState --value "$U" 2>/dev/null)
  case "$st" in active|activating|reloading) ;; *) echo "vllm-seat-cmd: $U left the active state (${st:-unknown}); detaching" >&2; exit 1 ;; esac
  now=$(systemctl show -p InvocationID --value "$U" 2>/dev/null)
  if [ -n "$inv" ] && [ -n "$now" ] && [ "$now" != "$inv" ]; then
    echo "vllm-seat-cmd: $U was restarted (invocation $inv -> $now); detaching so llama-swap re-attaches and health-waits" >&2; exit 1
  fi
  [ -z "$inv" ] && inv="$now"
  sleep 2
done
