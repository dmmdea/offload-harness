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
  "install render") ;;
  "install vllm-seat") exit 1 ;;
  "--config "*) ;;
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

# 8-12. --llama-bin-eg2: a second llama.cpp build for the embeddinggemma2 entry alone (a node whose main
#       build is below the embeddinggemma2 seat floor). Same shape as --llama-bin: it must be a directory, the dry run says
#       what it would render with, an absent flag changes nothing, and the render call passes it only when set.
mkdir -p "$TMP/llama-eg2"
run_install cuda --llama-bin "$TMP/llama" --llama-bin-eg2 "$TMP/absent-eg2"
[ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q -- "--llama-bin-eg2 $TMP/absent-eg2 is not a directory" \
  && pass "a given --llama-bin-eg2 must be a directory" || fail "a given --llama-bin-eg2 must be a directory" "rc=$RC: $OUT"

run_install cuda --llama-bin "$TMP/llama" --llama-bin-eg2 "$TMP/llama-eg2"
[ "$RC" -eq 0 ] && printf '%s' "$OUT" | grep "would render" | grep -q -- "--llama-bin-eg2 $TMP/llama-eg2" \
  && pass "a real --llama-bin-eg2 is accepted and the dry run names it" || fail "a real --llama-bin-eg2 is accepted and the dry run names it" "rc=$RC: $OUT"

run_install cuda --llama-bin "$TMP/llama"
[ "$RC" -eq 0 ] && printf '%s' "$OUT" | grep "would render" | grep -q -- "--llama-bin $TMP/llama" \
  && ! printf '%s' "$OUT" | grep "would render" | grep -q -- "--llama-bin-eg2" \
  && pass "without --llama-bin-eg2 the dry run names no second build" || fail "without --llama-bin-eg2 the dry run names no second build" "rc=$RC: $OUT"

#       the render call (a dry run never renders, so this reads the script, like the --rknpu-home case above).
render_call="$(awk '/"\$BIN" install render/,/--out "\$SWAP_YAML"/' "$INSTALL")"
printf '%s' "$render_call" | grep -q -- '${LLAMA_BIN_EG2:+--llama-bin-eg2 "$LLAMA_BIN_EG2"}' \
  && printf '%s' "$render_call" | tail -1 | grep -q -- '--out "$SWAP_YAML"$' \
  && pass "the render call passes --llama-bin-eg2 only when it is set" || fail "the render call passes --llama-bin-eg2 only when it is set" "$render_call"

bash "$INSTALL" --help 2>&1 | grep -q -- "--llama-bin-eg2 DIR" \
  && pass "--help documents --llama-bin-eg2" || fail "--help documents --llama-bin-eg2" "$(bash "$INSTALL" --help 2>&1 | head -20)"

# 13-16. the install manifest follows the tier this run detected and rendered for. install.ps1 rebuilds
#        installed.json whole on every run; install.sh wrote it once and kept the recorded profile for good, so a
#        box that gained a card carried the older layout in its manifest beside a serving config rendered for the
#        newer one. A differing profile is now rewritten (profile and backend only: accelerators, alt_backends and
#        every other key stay), an equal one leaves the file byte for byte, an absent one is written as before, and
#        a dry run says what it would do. The live runs use the stub for the binary and a stub node too old for the
#        compose lane, so nothing is installed and no package manager is started.
mkdir -p "$TMP/nodebin"
printf '#!/usr/bin/env bash\necho v0.0.0\n' >"$TMP/nodebin/node"; chmod +x "$TMP/nodebin/node"
# run_live PREFIX BACKEND: a real (not dry) run against PREFIX; sets OUT and RC.
run_live() {
  local prefix="$1" backend="$2"
  OUT="$(PATH="$TMP/nodebin:$PATH" STUB_BACKEND="$backend" bash "$INSTALL" --bin "$STUB" --prefix "$prefix" --no-service --llama-bin "$TMP/llama" 2>&1)"
  RC=$?
}
OLD_MANIFEST='{"profile":"old-tier","backend":"vulkan","accelerators":["coral"],"alt_backends":["cpu"],"install_date":"2026-01-01"}'

mkdir -p "$TMP/live-old"; printf '%s' "$OLD_MANIFEST" >"$TMP/live-old/installed.json"
run_live "$TMP/live-old" cuda
if [ "$RC" -eq 0 ] \
  && [ "$(jq -r '.profile' "$TMP/live-old/installed.json")" = "stub-tier" ] \
  && [ "$(jq -r '.backend' "$TMP/live-old/installed.json")" = "cuda" ] \
  && [ "$(jq -c '[.accelerators, .alt_backends, .install_date]' "$TMP/live-old/installed.json")" = '[["coral"],["cpu"],"2026-01-01"]' ] \
  && printf '%s' "$OUT" | grep -q "manifest:.*old-tier.*stub-tier"; then
  pass "a manifest recorded under another tier is rewritten to the detected tier, other keys kept"
else
  fail "a manifest recorded under another tier is rewritten to the detected tier, other keys kept" "rc=$RC manifest=$(cat "$TMP/live-old/installed.json" 2>/dev/null) out=$OUT"
fi

mkdir -p "$TMP/live-same"; printf '{"profile":"stub-tier","backend":"vulkan","extra":1}' >"$TMP/live-same/installed.json"
run_live "$TMP/live-same" cuda
[ "$RC" -eq 0 ] && [ "$(cat "$TMP/live-same/installed.json")" = '{"profile":"stub-tier","backend":"vulkan","extra":1}' ] \
  && pass "a manifest already recorded for the detected tier is left byte for byte" || fail "a manifest already recorded for the detected tier is left byte for byte" "rc=$RC manifest=$(cat "$TMP/live-same/installed.json" 2>/dev/null) out=$OUT"

run_live "$TMP/live-new" cuda
[ "$RC" -eq 0 ] && [ "$(jq -r '[.profile, .backend] | join(",")' "$TMP/live-new/installed.json" 2>/dev/null)" = "stub-tier,cuda" ] \
  && pass "an absent manifest is written for the detected tier" || fail "an absent manifest is written for the detected tier" "rc=$RC out=$OUT"

mkdir -p "$TMP/dry-old"; printf '%s' "$OLD_MANIFEST" >"$TMP/dry-old/installed.json"
OUT="$(STUB_BACKEND=cuda bash "$INSTALL" --bin "$STUB" --prefix "$TMP/dry-old" --no-service --dry-run --llama-bin "$TMP/llama" 2>&1)"; RC=$?
[ "$RC" -eq 0 ] && [ "$(cat "$TMP/dry-old/installed.json")" = "$OLD_MANIFEST" ] \
  && printf '%s' "$OUT" | grep -q "manifest:.*would rewrite.*old-tier.*stub-tier" \
  && pass "a dry run names the manifest rewrite and writes nothing" || fail "a dry run names the manifest rewrite and writes nothing" "rc=$RC manifest=$(cat "$TMP/dry-old/installed.json") out=$OUT"

# --client: a delegation client (ADR 0071) needs no tier, no llama.cpp build and no service.
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

# --token-file on a node install. Every lane of a fleet node requires the fleet bearer, and fleet-serve will not start on
# the tailnet address without fleet_auth_token in the config it reads, so a unit written without one would crash-loop.
# The installer asks for the key before it writes anything, stores it from a file (the value is never printed), and
# refuses to register the service without it.
NODE_KEY="$TMP/node-key"; printf '%s\n' 'fixture-value-0123456789' >"$NODE_KEY"
NODE_KEY_VALUE="fixture-value-0123456789"

# A service wanted and no key anywhere: refused up front, naming the key and the config the node will read.
rm -rf "$TMP/svc"
OUT="$(STUB_BACKEND=cuda bash "$INSTALL" --bin "$STUB" --prefix "$TMP/svc" --dry-run --llama-bin "$TMP/llama" 2>&1)"; RC=$?
if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "fleet_auth_token" && printf '%s' "$OUT" | grep -q -- "$TMP/svc/etc/config.json" \
  && printf '%s' "$OUT" | grep -q -- "--token-file"; then
  pass "a node install with a service and no fleet_auth_token is refused, naming the key, the config and --token-file"
else
  fail "a node install with a service and no fleet_auth_token is refused, naming the key, the config and --token-file" "rc=$RC: $OUT"
fi

# With --token-file the dry run goes on, says where the key would go, and never prints the value.
OUT="$(STUB_BACKEND=cuda bash "$INSTALL" --bin "$STUB" --prefix "$TMP/svc" --dry-run --llama-bin "$TMP/llama" --token-file "$NODE_KEY" 2>&1)"; RC=$?
if [ "$RC" -eq 0 ] && printf '%s' "$OUT" | grep -q "would store fleet_auth_token" && printf '%s' "$OUT" | grep -q -- "$TMP/svc/etc/config.json" \
  && ! printf '%s' "$OUT" | grep -q -- "$NODE_KEY_VALUE" && [ ! -e "$TMP/svc/etc/config.json" ]; then
  pass "--token-file on a node install: the dry run says where the key goes, prints no value, writes nothing"
else
  fail "--token-file on a node install: the dry run says where the key goes, prints no value, writes nothing" "rc=$RC: $OUT"
fi

# A config that already holds the key needs no --token-file.
mkdir -p "$TMP/svc2/etc"; printf '{"home":"/srv/x","fleet_auth_token":"already-set"}' >"$TMP/svc2/etc/config.json"
OUT="$(STUB_BACKEND=cuda bash "$INSTALL" --bin "$STUB" --prefix "$TMP/svc2" --dry-run --llama-bin "$TMP/llama" 2>&1)"; RC=$?
[ "$RC" -eq 0 ] && printf '%s' "$OUT" | grep -q "holds fleet_auth_token" && ! printf '%s' "$OUT" | grep -q "already-set" \
  && pass "a config that already holds fleet_auth_token satisfies a node install, and the value is not printed" || fail "a config that already holds fleet_auth_token satisfies a node install, and the value is not printed" "rc=$RC: $OUT"

# A config whose key is empty does not.
mkdir -p "$TMP/svc3/etc"; printf '{"home":"/srv/x","fleet_auth_token":""}' >"$TMP/svc3/etc/config.json"
OUT="$(STUB_BACKEND=cuda bash "$INSTALL" --bin "$STUB" --prefix "$TMP/svc3" --dry-run --llama-bin "$TMP/llama" 2>&1)"; RC=$?
[ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "fleet_auth_token" \
  && pass "a config whose fleet_auth_token is empty is refused like an absent one" || fail "a config whose fleet_auth_token is empty is refused like an absent one" "rc=$RC: $OUT"

# --no-service writes no unit, so it asks for no key (the operator starts fleet-serve by hand and fleet-serve refuses itself).
OUT="$(STUB_BACKEND=cuda bash "$INSTALL" --bin "$STUB" --prefix "$TMP/svc4" --no-service --dry-run --llama-bin "$TMP/llama" 2>&1)"; RC=$?
[ "$RC" -eq 0 ] && pass "--no-service asks for no fleet_auth_token" || fail "--no-service asks for no fleet_auth_token" "rc=$RC: $OUT"

# A live run stores the key in the config, keeps the other keys, and prints no value.
mkdir -p "$TMP/live-key/etc"; printf '{"home":"/srv/x","keep":1}' >"$TMP/live-key/etc/config.json"
OUT="$(PATH="$TMP/nodebin:$PATH" STUB_BACKEND=cuda bash "$INSTALL" --bin "$STUB" --prefix "$TMP/live-key" --no-service --llama-bin "$TMP/llama" --token-file "$NODE_KEY" 2>&1)"; RC=$?
if [ "$RC" -eq 0 ] \
  && [ "$(jq -r '.fleet_auth_token' "$TMP/live-key/etc/config.json" 2>/dev/null)" = "$NODE_KEY_VALUE" ] \
  && [ "$(jq -r '.keep' "$TMP/live-key/etc/config.json" 2>/dev/null)" = "1" ] \
  && ! printf '%s' "$OUT" | grep -q -- "$NODE_KEY_VALUE"; then
  pass "a live node install stores fleet_auth_token from --token-file, keeps the other keys and prints no value"
else
  fail "a live node install stores fleet_auth_token from --token-file, keeps the other keys and prints no value" "rc=$RC config=$(cat "$TMP/live-key/etc/config.json" 2>/dev/null) out=$OUT"
fi
if [ "$(uname -s)" = "Linux" ]; then
  [ "$(stat -c %a "$TMP/live-key/etc/config.json")" = "600" ] \
    && pass "the config that holds the key is readable by its owner only" || fail "the config that holds the key is readable by its owner only" "$(stat -c %a "$TMP/live-key/etc/config.json")"
fi

# A token file with nothing in it is refused, not stored as an empty key.
: >"$TMP/empty-key"
OUT="$(PATH="$TMP/nodebin:$PATH" STUB_BACKEND=cuda bash "$INSTALL" --bin "$STUB" --prefix "$TMP/live-empty" --no-service --llama-bin "$TMP/llama" --token-file "$TMP/empty-key" 2>&1)"; RC=$?
[ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "empty" && [ ! -e "$TMP/live-empty/etc/config.json" ] \
  && pass "an empty --token-file is refused before anything is written" || fail "an empty --token-file is refused before anything is written" "rc=$RC: $OUT"

# The unit no longer claims the endpoints are unauthenticated, and --help says what --token-file does on a node.
! grep -q "unauthenticated by design" "$INSTALL" \
  && pass "the generated unit's comment no longer says the fleet endpoints are unauthenticated" || fail "the generated unit's comment no longer says the fleet endpoints are unauthenticated" "$(grep -n 'unauthenticated by design' "$INSTALL")"
bash "$INSTALL" --help 2>&1 | grep -A2 -- "--token-file" | grep -q "fleet_auth_token" \
  && pass "--help documents --token-file for a node install" || fail "--help documents --token-file for a node install" "$(bash "$INSTALL" --help 2>&1 | grep -A2 -- '--token-file')"

# A node installed for another service identity: the config the key lands in is readable by its owner only, and the unit and
# the acceptance gate run as that account, so the file is handed to it (sudo chown; the installer escalates for the unit and
# the gate already). For the installing user itself there is nothing to hand over. A stand-in sudo records what it was asked.
export SUDO_LOG="$TMP/sudo-log"; : >"$SUDO_LOG"
printf '%s\n' '#!/usr/bin/env bash' 'printf "%s\n" "$*" >>"$SUDO_LOG"' >"$TMP/nodebin/sudo"; chmod +x "$TMP/nodebin/sudo"
mkdir -p "$TMP/live-user/etc"; printf '{"home":"/srv/x"}' >"$TMP/live-user/etc/config.json"
OUT="$(PATH="$TMP/nodebin:$PATH" STUB_BACKEND=cuda bash "$INSTALL" --bin "$STUB" --prefix "$TMP/live-user" --no-service --llama-bin "$TMP/llama" --token-file "$NODE_KEY" --user svc-account 2>&1)"; RC=$?
[ "$RC" -eq 0 ] && grep -q -- "chown svc-account $TMP/live-user/etc/config.json" "$SUDO_LOG" \
  && pass "the config that holds the key is handed to another service identity" || fail "the config that holds the key is handed to another service identity" "rc=$RC sudo=$(cat "$SUDO_LOG") out=$OUT"

: >"$SUDO_LOG"
mkdir -p "$TMP/live-self/etc"; printf '{"home":"/srv/x"}' >"$TMP/live-self/etc/config.json"
OUT="$(PATH="$TMP/nodebin:$PATH" STUB_BACKEND=cuda bash "$INSTALL" --bin "$STUB" --prefix "$TMP/live-self" --no-service --llama-bin "$TMP/llama" --token-file "$NODE_KEY" 2>&1)"; RC=$?
[ "$RC" -eq 0 ] && ! grep -q "chown" "$SUDO_LOG" \
  && pass "the installing user's own config is not handed over" || fail "the installing user's own config is not handed over" "rc=$RC sudo=$(cat "$SUDO_LOG") out=$OUT"

# A hand-over that fails is not left unsaid.
printf '%s\n' '#!/usr/bin/env bash' 'case "$*" in *chown*) exit 1 ;; esac' >"$TMP/nodebin/sudo"
mkdir -p "$TMP/live-fail/etc"; printf '{"home":"/srv/x"}' >"$TMP/live-fail/etc/config.json"
OUT="$(PATH="$TMP/nodebin:$PATH" STUB_BACKEND=cuda bash "$INSTALL" --bin "$STUB" --prefix "$TMP/live-fail" --no-service --llama-bin "$TMP/llama" --token-file "$NODE_KEY" --user svc-account 2>&1)"; RC=$?
[ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "could not hand" && printf '%s' "$OUT" | grep -q "svc-account" \
  && pass "a failed hand-over of the config stops the install, naming the account" || fail "a failed hand-over of the config stops the install, naming the account" "rc=$RC: $OUT"

if [ "$FAIL" -eq 0 ]; then
  echo "ALL PASS"
  exit 0
fi
echo "FAILURES: $FAIL"
exit 1
