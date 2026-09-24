#!/usr/bin/env bash
# linux-node-swap-launch.sh - detached launcher for `local-offload node-swap` on a
# Linux fleet node, the sibling of setup/windows-node-swap-launch.ps1.
#
# Gap (bigger-models-2026-09-24.md / deploy-d5207011.md): every Linux deploy before
# this script was an ad-hoc, per-node bash sequence hand-written for that one box —
# the Lenovo's own script hardcoded a `curl http://127.0.0.1:<port>/fleet/health`
# health check while fleet-serve there actually binds only its tailnet address
# (`--listen 192.0.2.10:18811 --listen-trusted-network`, a placeholder example —
# a real tailnet address here, not loopback, is the point), so every poll silently
# failed and an unguarded `${RUNNING:-1}` fallback read that as "still busy" for
# about 46 minutes before anyone noticed it was a wrong URL, not real load. This
# script never reinvents that polling logic in bash at all: it just launches the
# SAME `local-offload node-swap` engine every Windows node already uses, which (as
# of this fix) auto-resolves --health-url from THIS node's own config fleet_listen
# when the caller does not pass one explicitly — never a guessed loopback address.
# It survives an SSH drop the POSIX way: `setsid nohup ... &`, detached from the
# calling session's process group and controlling terminal, exactly the pattern
# deploy-d5207011.md already used by hand for every Linux node in that deploy
# ("Linux nodes used adapted deploy-f2e38cdb.md scripts run via setsid/nohup so an
# SSH drop can't kill them mid-swap").
#
# Usage (local, or over ssh - the launch call itself returns almost instantly):
#
#   ./linux-node-swap-launch.sh \
#     --staged /opt/offload/bin/local-offload.new \
#     --target /opt/offload/bin/local-offload \
#     --sha256 <hex> \
#     --restart-command "sudo systemctl restart offload-fleet-node.service" \
#     [--health-url http://192.0.2.10:18811/fleet/health] \
#     [--config /path/to/config.json] \
#     [--backup-suffix pre-<sha>] [--render-tarball render.tar.gz --render-dir /opt/offload/render] \
#     [--wait-idle-timeout 10m] [--verify-timeout 90s] [--dry-run] [--skip-hash-check] \
#     [--runner-exe /opt/offload/bin/local-offload] [--log-dir /opt/offload/bin] [--node-swap-bin local-offload]
#
# --health-url is OPTIONAL here on purpose (unlike the Windows launcher, which has
# always required the equivalent -HealthUrl for a fleet-serve node): node-swap now
# reads it from --config's fleet_listen itself when this node's config already
# names a real (non-loopback) bind address. Pass it explicitly only when this
# node's config does not carry that address, or to override it.
#
# --restart-command is the Linux equivalent of the Windows launcher's -RestartTask
# (Linux has no scheduled-task concept here): the systemd unit restart every deploy
# record already uses by hand (`sudo systemctl restart offload-fleet-node.service`).
# Omitting both --restart-command and --health-url is the OptiPlex/standalone
# pattern (binary-only swap, GPU-lease-checked, nothing restarted — see gap 6b,
# internal/nodeswap's standalone GPU-lease wait).
#
# Then, over a FRESH connection (the whole point):
#   tail -n 20 <log path>
#   [ -f <result path> ] && cat <result path> | python3 -m json.tool
#
# The result JSON is nodeswap.Outcome (internal/nodeswap/nodeswap.go): ok, steps[],
# error, rolled_back, rollback_ok, old_sha256, new_sha256, final_pid,
# final_image_sha256, health_version. ok:false with rolled_back:true and
# rollback_ok:true means the swap failed safely and the box is back on the old
# binary, running.
set -euo pipefail
# Job control OFF for this script's own shell, regardless of how it was invoked
# (a non-interactive `bash script.sh`/ssh exec already has it off; this is the
# belt-and-braces case for someone sourcing or running it from an INTERACTIVE
# shell). With job control ON, backgrounding `setsid nohup ... &` below puts the
# child in a NEW process group, which forces setsid to fork internally (it only
# skips the fork when the CALLER is not already a process-group leader) — `$!`
# then captures that short-lived setsid parent, not the real detached grandchild,
# and the liveness check below could misfire on a swap that is actually still
# running fine. `set +m` keeps the background job in this shell's own process
# group, so setsid never needs to fork and `$!` names the real process.
set +m

usage() {
  echo "usage: $0 --staged PATH --target PATH --sha256 HEX [--restart-command CMD] [--health-url URL] [flags...]" >&2
  echo "       (see the header comment in this file for the full flag list)" >&2
}

# resolve_runner_exe implements the runner-selection half of the 2026-09-24 fix
# (docs/systems/node-swap.md; mirrors Resolve-RunnerExe in the sibling
# windows-node-swap-launch.ps1 — same defect, same fix, same reasoning): default
# the exe used to EXECUTE `node-swap` to the STAGED binary — the NEW engine,
# verified by hash first (node-swap's own --sha256 check happens INSIDE the
# process this chooses to start — trusting an unverified staged exe to
# self-check would mean running arbitrary staged bytes first) — never the
# currently-installed target. This inverts the tool's original default
# (target): a fix to the swap engine itself (e.g. a node-swap bug fixed in the
# very build being staged — exactly what happened on the Lenovo/binxarn
# 2026-09-24 rollout: their installed build still had the OLD, broken
# deps_other.go stub, so running node-swap FROM it re-triggered the very bug
# the staged build had already fixed) can never take effect while the launcher
# keeps running the old installed code to perform the swap.
#
# Falls back to the currently-installed target, with a clear log line, only
# when the staged binary does not support the node-swap subcommand AT ALL (a
# very old staged build — an intentional downgrade, or the first-ever rollout
# of this tool in the OTHER direction). An explicit --runner-exe always wins
# outright and skips every check here.
#
# $6/$7/$8 are FUNCTION NAMES (bash's own dependency-injection idiom — see
# linux-node-swap-launch.tests.sh), never hardcoded in this function's own
# body, so tests can substitute fakes with no real binary and no real process
# spawn. Prints the resolved runner path on stdout on success; on refusal,
# prints an "error: ..." line to stderr and returns non-zero — never prints a
# path in that case, so a careless caller cannot mistake stderr noise for a
# resolved runner.
resolve_runner_exe() {
  local runner="$1" staged="$2" target="$3" sha256="$4" skip_hash="$5"
  local hash_fn="$6" supports_fn="$7" log_fn="$8"
  if [ -n "$runner" ]; then
    printf '%s\n' "$runner"
    return 0
  fi
  if [ "$skip_hash" != "1" ]; then
    if [ -z "$sha256" ]; then
      echo "error: --sha256 is required to verify the staged binary before it can run as the default node-swap runner (pass --runner-exe to override, or --skip-hash-check for testing only)" >&2
      return 2
    fi
    local got
    got="$("$hash_fn" "$staged")" || { echo "error: hashing staged binary $staged failed" >&2; return 2; }
    if [ "$(printf '%s' "$got" | tr 'A-F' 'a-f')" != "$(printf '%s' "$sha256" | tr 'A-F' 'a-f')" ]; then
      echo "error: staged binary $staged sha256 $got does not match --sha256 $sha256 — refusing to use it as the node-swap runner" >&2
      return 2
    fi
  fi
  if "$supports_fn" "$staged"; then
    printf '%s\n' "$staged"
    return 0
  fi
  "$log_fn" "staged binary $staged does not support node-swap (very old build) — falling back to the currently-installed $target as the runner"
  printf '%s\n' "$target"
  return 0
}

# real_hash_file / real_supports_node_swap / real_log are resolve_runner_exe's
# production dependencies.
#
# real_supports_node_swap: `node-swap` called with no flags is always a
# RECOGNIZED subcommand in any build that has it (it just fails Plan
# validation — --staged/--target are required — which main.go routes through
# the ordinary `error` path, exit code 1); an entirely UNRECOGNIZED
# subcommand falls to main.go's own `default:` case, which prints usage and
# calls os.Exit(2) directly. So "supported" is a NARROW allowlist — exit code
# 0 or 1, the only two codes a real `local-offload` build can produce for
# this exact call — never a broad "anything but 2" — because a staged binary
# that is corrupt, wrong-architecture, or killed by a signal on exec (rc 126,
# 127, 137, 139, ...) is a DIFFERENT failure than "doesn't support node-swap"
# and must never be misread as "supports it, safe to run as the runner"
# (code review finding, 2026-09-24 rollout PR): falling back to the
# currently-installed target is the safe response to ANY of those, exactly
# as it is to a confirmed-unsupported (exit 2) staged build. Never touches
# --staged/--target either way (Plan validation is the FIRST thing
# node-swap's own Run() does, nodeswap.go, before any file is read), so this
# is safe to run against any exe, staged or installed.
real_hash_file() { sha256sum -- "$1" | awk '{print $1}'; }
real_supports_node_swap() {
  local rc=0
  "$1" node-swap >/dev/null 2>&1 || rc=$?
  [ "$rc" -eq 0 ] || [ "$rc" -eq 1 ]
}
real_log() { echo "[node-swap-launch] $1" >&2; }

# main is everything the script actually DOES when run for real — split out
# from the function definitions above so linux-node-swap-launch.tests.sh can
# `source` this file (pulling in resolve_runner_exe and friends) without
# also triggering a real detached launch. See the BASH_SOURCE guard at the
# bottom of this file.
main() {
  local STAGED="" TARGET="" SHA256="" BACKUP_SUFFIX="" HEALTH_URL="" CONFIG_PATH=""
  local RESTART_COMMAND="" RENDER_TARBALL="" RENDER_DIR=""
  local WAIT_IDLE_TIMEOUT="10m" VERIFY_TIMEOUT="90s"
  local DRY_RUN=0 SKIP_HASH_CHECK=0
  local RUNNER_EXE="" LOG_DIR="" NODE_SWAP_BIN=""

  while [ $# -gt 0 ]; do
    case "$1" in
      --staged) STAGED="$2"; shift 2 ;;
      --target) TARGET="$2"; shift 2 ;;
      --sha256) SHA256="$2"; shift 2 ;;
      --backup-suffix) BACKUP_SUFFIX="$2"; shift 2 ;;
      --health-url) HEALTH_URL="$2"; shift 2 ;;
      --config) CONFIG_PATH="$2"; shift 2 ;;
      --restart-command) RESTART_COMMAND="$2"; shift 2 ;;
      --render-tarball) RENDER_TARBALL="$2"; shift 2 ;;
      --render-dir) RENDER_DIR="$2"; shift 2 ;;
      --wait-idle-timeout) WAIT_IDLE_TIMEOUT="$2"; shift 2 ;;
      --verify-timeout) VERIFY_TIMEOUT="$2"; shift 2 ;;
      --dry-run) DRY_RUN=1; shift ;;
      --skip-hash-check) SKIP_HASH_CHECK=1; shift ;;
      --runner-exe) RUNNER_EXE="$2"; shift 2 ;;
      --log-dir) LOG_DIR="$2"; shift 2 ;;
      --node-swap-bin) NODE_SWAP_BIN="$2"; shift 2 ;;
      -h|--help) usage; exit 0 ;;
      *) echo "unknown flag: $1" >&2; usage; exit 2 ;;
    esac
  done

  if [ -z "$STAGED" ] || [ -z "$TARGET" ] || [ -z "$SHA256" ]; then
    echo "error: --staged, --target and --sha256 are required" >&2
    usage
    exit 2
  fi

  # RUNNER_EXE: the exe used to RUN `node-swap` itself — see
  # resolve_runner_exe's own doc comment above for the full reasoning. An
  # explicit --runner-exe (already captured above) always wins outright;
  # resolve_runner_exe prints the resolved path on stdout, or fails loudly
  # (its own "error: ..." already on stderr) with a non-zero return this
  # launcher exits on immediately.
  RUNNER_EXE="$(resolve_runner_exe "$RUNNER_EXE" "$STAGED" "$TARGET" "$SHA256" "$SKIP_HASH_CHECK" real_hash_file real_supports_node_swap real_log)" || exit 2
  if [ ! -x "$RUNNER_EXE" ]; then
    echo "error: RunnerExe not found or not executable: $RUNNER_EXE" >&2
    exit 2
  fi
  if [ -z "$NODE_SWAP_BIN" ]; then NODE_SWAP_BIN="$RUNNER_EXE"; fi

  if [ -z "$LOG_DIR" ]; then LOG_DIR="$(dirname -- "$TARGET")"; fi
  mkdir -p "$LOG_DIR"

  STAMP="$(date +%Y%m%d-%H%M%S)"
  LOG_PATH="$LOG_DIR/node-swap-$STAMP.log"
  RESULT_PATH="$LOG_DIR/node-swap-$STAMP.result.json"

  # argv array, never a shell-quoted command-line string: unlike the Windows launcher
  # (which must re-encode into ONE Win32_Process.Create command-line string for the
  # receiving process's CommandLineToArgvW parser), setsid/nohup exec a real argv
  # array — every value (a restart command with its own spaces/quotes, a path with a
  # space) reaches the child byte-for-byte with no re-escaping step to get wrong.
  local ARGS=(node-swap --staged "$STAGED" --target "$TARGET" --sha256 "$SHA256"
        --result "$RESULT_PATH" --log "$LOG_PATH" --json
        --wait-idle-timeout "$WAIT_IDLE_TIMEOUT" --verify-timeout "$VERIFY_TIMEOUT")
  [ -n "$BACKUP_SUFFIX" ] && ARGS+=(--backup-suffix "$BACKUP_SUFFIX")
  [ -n "$HEALTH_URL" ] && ARGS+=(--health-url "$HEALTH_URL")
  [ -n "$CONFIG_PATH" ] && ARGS+=(--config "$CONFIG_PATH")
  [ -n "$RESTART_COMMAND" ] && ARGS+=(--restart-command "$RESTART_COMMAND")
  if [ -n "$RENDER_TARBALL" ]; then
    if [ -z "$RENDER_DIR" ]; then echo "error: --render-tarball requires --render-dir" >&2; exit 2; fi
    ARGS+=(--render-tarball "$RENDER_TARBALL" --render-dir "$RENDER_DIR")
  fi
  [ "$DRY_RUN" = 1 ] && ARGS+=(--dry-run)
  [ "$SKIP_HASH_CHECK" = 1 ] && ARGS+=(--skip-hash-check)

  echo "[node-swap-launch] command: $NODE_SWAP_BIN ${ARGS[*]}"
  echo "[node-swap-launch] log:    $LOG_PATH"
  echo "[node-swap-launch] result: $RESULT_PATH"

  # setsid: new session, detached from this shell's controlling terminal/process
  # group, so an SSH drop's SIGHUP never reaches it. nohup: belt-and-braces against
  # SIGHUP for a caller that isn't itself a fresh session leader. Stdio redirected to
  # the SAME log file node-swap already writes via --log, so nothing is lost even if
  # the child dies before opening its own log (matches --log's own append contract).
  setsid nohup "$NODE_SWAP_BIN" "${ARGS[@]}" >>"$LOG_PATH" 2>&1 < /dev/null &
  local SWAP_PID=$!
  disown "$SWAP_PID" 2>/dev/null || true

  # The child's own first action (node_swap_cmd.go's runNodeSwap) is to open --log;
  # if it dies before reaching that, a poller would otherwise find no log and no
  # result file and have no way to tell that apart from "still running". Same
  # liveness check as the Windows launcher's post-Create probe.
  sleep 0.5
  if ! kill -0 "$SWAP_PID" 2>/dev/null; then
    if [ ! -s "$LOG_PATH" ] && [ ! -f "$RESULT_PATH" ]; then
      echo "error: node-swap (pid $SWAP_PID) exited within 500ms and wrote neither --log nor --result — it likely failed before parsing its flags; re-run attached (without this launcher) to see the error directly" >&2
      exit 1
    fi
  fi

  echo "[node-swap-launch] started, pid $SWAP_PID"
  echo "[node-swap-launch] this session may disconnect now - the swap is detached. Poll from a fresh connection:"
  echo "  tail -n 20 '$LOG_PATH'"
  echo "  [ -f '$RESULT_PATH' ] && cat '$RESULT_PATH'"

  # json_escape: backslash then quote, in that order (a naive path never needs more —
  # no python3/jq dependency for two directory paths this script built itself).
  json_escape() { printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'; }
  printf '{"pid": %s, "log_path": "%s", "result_path": "%s"}\n' \
    "$SWAP_PID" "$(json_escape "$LOG_PATH")" "$(json_escape "$RESULT_PATH")"
}

# Only run for real when EXECUTED directly (`bash linux-node-swap-launch.sh
# ...` / `./linux-node-swap-launch.sh ...`), never when SOURCED — this is
# what lets linux-node-swap-launch.tests.sh pull in resolve_runner_exe and
# the real_* helpers via `source` with no side effects (same convention the
# sibling windows-node-swap-launch.ps1 gets via its -SelfTest switch).
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  main "$@"
fi
