#!/usr/bin/env bash
# install.tests.sh - dry-run tests for setup/install.sh's --llama-bin rule and its --rknpu-home
# pass-through. install.sh only asks the harness binary questions, so a stub binary answers them
# (detect, tier-info, seed) and every case runs with --dry-run: nothing is installed or written
# outside the temp dir.
#
# PASS/FAIL lines to stdout; exit 0 = all pass, exit 1 = any fail, 0 with a SKIP line when jq
# (a prerequisite of install.sh itself) is absent.
# Usage: bash setup/install.tests.sh
set -uo pipefail
HERE="$(cd "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
INSTALL="$HERE/install.sh"

if ! command -v jq >/dev/null 2>&1; then
  echo "SKIP install.tests.sh: jq is not installed (install.sh requires it)"
  exit 0
fi

FAIL=0
pass() { echo "PASS $1"; }
fail() { echo "FAIL $1: $2"; FAIL=$((FAIL + 1)); }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
STUB="$TMP/local-offload"
SEED_LOG="$TMP/seed-args"
# The stub answers the four questions a dry run asks. STUB_BACKEND names the tier's backend;
# STUB_BACKEND=fail makes tier-info exit non-zero, like a binary that cannot read its table.
cat >"$STUB" <<'STUBEOF'
#!/usr/bin/env bash
case "$1 $2" in
  "--version "*) echo "stub 0.0.0" ;;
  "install detect") echo '{"verdict":{"profile":"stub-tier","reason":"stub","ram_tier":"min","accelerators":[]},"facts":{"ram_gb":7}}' ;;
  "install tier-info")
    [ "${STUB_BACKEND:-}" = "fail" ] && exit 1
    echo "{\"profile\":\"stub-tier\",\"backend\":\"${STUB_BACKEND:-cuda}\"}" ;;
  "install seed") printf '%s\n' "$*" >"$SEED_LOG"; echo '{}' ;;
  *) echo "stub: unexpected call: $*" >&2; exit 99 ;;
esac
STUBEOF
chmod +x "$STUB"
export SEED_LOG

# run_install BACKEND [install.sh args...]: dry run against a fresh prefix; sets OUT and RC.
run_install() {
  local backend="$1"; shift
  OUT="$(STUB_BACKEND="$backend" bash "$INSTALL" --bin "$STUB" --prefix "$TMP/prefix" --no-service --dry-run "$@" 2>&1)"
  RC=$?
}

# 1. rk3588: its template has no llama.cpp entry, so no --llama-bin is fine.
run_install rk3588
[ "$RC" -eq 0 ] && printf '%s' "$OUT" | grep -q "would render" \
  && pass "rk3588 needs no --llama-bin" || fail "rk3588 needs no --llama-bin" "rc=$RC: $OUT"

# 2. every other backend still requires it.
for backend in cuda vulkan cpu; do
  run_install "$backend"
  [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q -- "--llama-bin is required" \
    && pass "$backend still requires --llama-bin" || fail "$backend still requires --llama-bin" "rc=$RC: $OUT"
done

# 3. an unreadable backend keeps the requirement (never silently drops it).
run_install fail
[ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q -- "--llama-bin is required" \
  && pass "an unreadable tier backend keeps --llama-bin required" || fail "an unreadable tier backend keeps --llama-bin required" "rc=$RC: $OUT"

# 4. a --llama-bin that is given must still be a directory, on rk3588 too.
run_install rk3588 --llama-bin "$TMP/absent"
[ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "is not a directory" \
  && pass "a given --llama-bin must be a directory" || fail "a given --llama-bin must be a directory" "rc=$RC: $OUT"

# 5. a given, real --llama-bin still passes on any backend.
mkdir -p "$TMP/llama"
run_install cuda --llama-bin "$TMP/llama"
[ "$RC" -eq 0 ] && pass "a real --llama-bin passes" || fail "a real --llama-bin passes" "rc=$RC: $OUT"

# 6. seed gets the RKNPU home (default <prefix>/rknpu, or $RKNPU_HOME).
rm -f "$SEED_LOG"; run_install rk3588
grep -q -- "--rknpu-home $TMP/prefix/rknpu\$" "$SEED_LOG" 2>/dev/null \
  && pass "seed defaults --rknpu-home to <prefix>/rknpu" || fail "seed defaults --rknpu-home to <prefix>/rknpu" "$(cat "$SEED_LOG" 2>/dev/null)"
rm -f "$SEED_LOG"; RKNPU_HOME=/srv/npu run_install rk3588
grep -q -- "--rknpu-home /srv/npu\$" "$SEED_LOG" 2>/dev/null \
  && pass "seed follows \$RKNPU_HOME" || fail "seed follows \$RKNPU_HOME" "$(cat "$SEED_LOG" 2>/dev/null)"

# 7. the render call resolves the RKNPU home the same way as the seed call (a dry run never renders,
#    so this reads the script: the two expressions must be the same text).
seed_expr="$(grep -o -- '--rknpu-home "[^"]*"' "$INSTALL" | sed -n 1p)"
render_expr="$(awk '/"\$BIN" install render/,/--out "\$SWAP_YAML"/' "$INSTALL" | grep -o -- '--rknpu-home "[^"]*"')"
[ -n "$render_expr" ] && [ "$render_expr" = "$seed_expr" ] \
  && pass "render passes --rknpu-home like seed" || fail "render passes --rknpu-home like seed" "seed=[$seed_expr] render=[$render_expr]"

# --client: a delegation client (ADR 0070) needs no tier, no llama.cpp build and no service.
OUT="$(bash "$INSTALL" --bin "$STUB" --prefix "$TMP/client" --client --dry-run 2>&1)"; RC=$?
[ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q -- "--client requires --remotes" \
  && pass "--client requires --remotes" || fail "--client requires --remotes" "rc=$RC: $OUT"
OUT="$(bash "$INSTALL" --bin "$STUB" --prefix "$TMP/client" --client --remotes http://render-a:18811 --dry-run 2>&1)"; RC=$?
[ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q -- "--client requires --token-file" \
  && pass "--client requires --token-file" || fail "--client requires --token-file" "rc=$RC: $OUT"
OUT="$(bash "$INSTALL" --bin "$STUB" --prefix "$TMP/client" --client --remotes http://render-a:18811 --token-file "$TMP/tok" --dry-run 2>&1)"; RC=$?
if [ "$RC" -eq 0 ] \
  && printf '%s' "$OUT" | grep -q -- "local-offload install client --home $TMP/client --remotes http://render-a:18811 --token-file $TMP/tok --config $TMP/client/etc/config.json" \
  && printf '%s' "$OUT" | grep -q -- "local-offload acceptance --config $TMP/client/etc/config.json" \
  && ! printf '%s' "$OUT" | grep -q -E "^tier:|--llama-bin is required|systemd"; then
  pass "--client renders a client config and gates it, with no tier, llama.cpp or service"
else
  fail "--client renders a client config and gates it, with no tier, llama.cpp or service" "rc=$RC: $OUT"
fi

if [ "$FAIL" -eq 0 ]; then
  echo "ALL PASS"
  exit 0
fi
echo "FAILURES: $FAIL"
exit 1
