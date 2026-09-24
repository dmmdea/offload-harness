#!/usr/bin/env bash
# linux-node-swap-launch.tests.sh - unit tests for resolve_runner_exe, the
# runner-selection function inside linux-node-swap-launch.sh (mirrors
# windows-node-swap-launch.tests.ps1 / its embedded -SelfTest coverage of
# Resolve-RunnerExe: same defect, same fix, same test shape).
#
# Sources the launcher WITHOUT running it — linux-node-swap-launch.sh only
# calls main() when EXECUTED directly (see its own BASH_SOURCE guard at the
# bottom), so sourcing it here just pulls in resolve_runner_exe/real_*
# without launching anything — then drives resolve_runner_exe with fake
# hash/support/log functions: no real binaries, no real process spawns.
#
# PASS/FAIL lines to stdout; exit 0 = all pass, exit 1 = any fail.
# Usage: bash setup/linux-node-swap-launch.tests.sh
set -uo pipefail
HERE="$(cd "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=linux-node-swap-launch.sh
source "$HERE/linux-node-swap-launch.sh"

FAIL=0
pass() { echo "PASS $1"; }
fail() { echo "FAIL $1: $2"; FAIL=$((FAIL + 1)); }

# --- fakes (function names, passed BY NAME to resolve_runner_exe — its own
# dependency-injection seam; see its doc comment) ---
fake_hash_ok()   { echo "abc123"; }              # matches --sha256 abc123 (case-insensitively)
fake_hash_bad()  { echo "deadbeef"; }            # never matches
fake_hash_fails() { return 1; }                  # simulates a hashing failure (e.g. staged missing)
fake_supports_yes() { return 0; }
fake_supports_no()  { return 2; }
noop_log() { :; }
must_not_be_called() { echo "must not be called" >&2; exit 99; }
# fake_log writes to a FILE, not a variable: resolve_runner_exe is always
# invoked below via command substitution `$(...)`, which forks a subshell —
# a variable fake_log set there would vanish with that subshell, so IPC has
# to cross the subshell boundary through the filesystem instead.
FAKE_LOG_FILE="$(mktemp)"
fake_log() { printf '%s' "$1" >"$FAKE_LOG_FILE"; }

# 1. An explicit --runner-exe always wins; hash/support/log must never even
#    be invoked (all three wired to a hard exit(99) to prove it).
got="$(resolve_runner_exe "explicit.exe" "staged.exe" "target.exe" "" 0 must_not_be_called must_not_be_called must_not_be_called)"
[ "$got" = "explicit.exe" ] && pass "explicit --runner-exe wins outright" || fail "explicit --runner-exe wins outright" "got [$got]"

# 2. No override, hash matches, staged supports node-swap -> staged.
got="$(resolve_runner_exe "" "staged.exe" "target.exe" "abc123" 0 fake_hash_ok fake_supports_yes noop_log)"
[ "$got" = "staged.exe" ] && pass "defaults to the staged binary" || fail "defaults to the staged binary" "got [$got]"

# 3. Hash mismatch -> refuses outright (non-zero return, no path printed —
#    never silently falls back to either binary with an unverified staged exe).
if out="$(resolve_runner_exe "" "staged.exe" "target.exe" "abc123" 0 fake_hash_bad fake_supports_yes noop_log 2>/dev/null)"; then
  fail "hash mismatch refuses to run the staged binary" "expected a non-zero return, got success with stdout [$out]"
else
  pass "hash mismatch refuses to run the staged binary"
fi

# 3b. Hashing itself fails (e.g. the staged file vanished) -> refuses, same as a mismatch.
if resolve_runner_exe "" "staged.exe" "target.exe" "abc123" 0 fake_hash_fails fake_supports_yes noop_log >/dev/null 2>/dev/null; then
  fail "hashing failure refuses" "expected a non-zero return"
else
  pass "hashing failure refuses"
fi

# 4. Staged does not support node-swap (a very old staged build) -> falls
#    back to target, and logs why.
: >"$FAKE_LOG_FILE"
got="$(resolve_runner_exe "" "staged.exe" "target.exe" "abc123" 0 fake_hash_ok fake_supports_no fake_log)"
[ "$got" = "target.exe" ] && pass "falls back to the installed target when staged lacks node-swap" \
  || fail "falls back to the installed target when staged lacks node-swap" "got [$got]"
loggedMsg="$(cat "$FAKE_LOG_FILE")"
case "$loggedMsg" in
  *"does not support node-swap"*) pass "fallback logs why" ;;
  *) fail "fallback logs why" "log was: [$loggedMsg]" ;;
esac

# 5. --skip-hash-check (skip_hash=1) bypasses hash verification entirely but
#    still probes support — hash_fn wired to a hard exit(99) to prove it is
#    never called.
got="$(resolve_runner_exe "" "staged.exe" "target.exe" "" 1 must_not_be_called fake_supports_yes noop_log)"
[ "$got" = "staged.exe" ] && pass "--skip-hash-check bypasses hash verification" \
  || fail "--skip-hash-check bypasses hash verification" "got [$got]"

# 6. Missing --sha256 with no override and no --skip-hash-check -> refuses.
if resolve_runner_exe "" "staged.exe" "target.exe" "" 0 fake_hash_ok fake_supports_yes noop_log >/dev/null 2>/dev/null; then
  fail "missing --sha256 refuses" "expected a non-zero return"
else
  pass "missing --sha256 refuses"
fi

# 7. Case-insensitive hash comparison (sha256sum lowercases; a caller-passed
#    --sha256 in uppercase must still match).
fake_hash_upper() { echo "ABC123"; }
got="$(resolve_runner_exe "" "staged.exe" "target.exe" "abc123" 0 fake_hash_upper fake_supports_yes noop_log)"
[ "$got" = "staged.exe" ] && pass "hash comparison is case-insensitive" || fail "hash comparison is case-insensitive" "got [$got]"

# --- real_supports_node_swap: the REAL exit-code classification (code review
# finding, 2026-09-24 rollout PR): "supported" must be a narrow allowlist —
# exit 0 or 1 only — never a broad "anything but 2", because a staged binary
# that is corrupt, wrong-architecture, or killed on exec (rc 126/127/137/139)
# is a DIFFERENT failure than "doesn't support node-swap" and must not be
# misread as safe to run. These drive real_supports_node_swap against tiny
# throwaway scripts standing in for a `local-offload` build — real process
# spawns, real exit codes, no fake involved (unlike resolve_runner_exe's own
# tests above, which fake supports_fn directly).
FAKE_BIN_DIR="$(mktemp -d)"
trap 'rm -f "$FAKE_LOG_FILE"; rm -rf "$FAKE_BIN_DIR"' EXIT

make_fake_bin() {
  # $1 = script path, $2 = exit code the fake `<bin> node-swap` call reports
  cat >"$1" <<EOF
#!/usr/bin/env bash
exit $2
EOF
  chmod +x "$1"
}

make_fake_bin "$FAKE_BIN_DIR/exit0" 0
make_fake_bin "$FAKE_BIN_DIR/exit1" 1
make_fake_bin "$FAKE_BIN_DIR/exit2" 2
make_fake_bin "$FAKE_BIN_DIR/exit126" 126   # not executable / exec format error shape
make_fake_bin "$FAKE_BIN_DIR/exit137" 137   # SIGKILL (128+9) shape — e.g. OOM-killed on exec
make_fake_bin "$FAKE_BIN_DIR/exit139" 139   # SIGSEGV (128+11) shape — a crashing/corrupt binary

if real_supports_node_swap "$FAKE_BIN_DIR/exit0"; then pass "real_supports_node_swap: exit 0 is supported"
else fail "real_supports_node_swap: exit 0 is supported" "returned false"; fi

if real_supports_node_swap "$FAKE_BIN_DIR/exit1"; then pass "real_supports_node_swap: exit 1 (validation error) is supported"
else fail "real_supports_node_swap: exit 1 (validation error) is supported" "returned false"; fi

if real_supports_node_swap "$FAKE_BIN_DIR/exit2"; then fail "real_supports_node_swap: exit 2 (unrecognized subcommand) is NOT supported" "returned true"
else pass "real_supports_node_swap: exit 2 (unrecognized subcommand) is NOT supported"; fi

if real_supports_node_swap "$FAKE_BIN_DIR/exit126"; then fail "real_supports_node_swap: exit 126 (exec-format-error shape) is NOT supported" "returned true"
else pass "real_supports_node_swap: exit 126 (exec-format-error shape) is NOT supported"; fi

if real_supports_node_swap "$FAKE_BIN_DIR/exit137"; then fail "real_supports_node_swap: exit 137 (SIGKILL shape) is NOT supported" "returned true"
else pass "real_supports_node_swap: exit 137 (SIGKILL shape) is NOT supported"; fi

if real_supports_node_swap "$FAKE_BIN_DIR/exit139"; then fail "real_supports_node_swap: exit 139 (SIGSEGV shape) is NOT supported" "returned true"
else pass "real_supports_node_swap: exit 139 (SIGSEGV shape) is NOT supported"; fi

if [ "$FAIL" -eq 0 ]; then
  echo "ALL PASS"
  exit 0
fi
echo "FAILURES: $FAIL"
exit 1
