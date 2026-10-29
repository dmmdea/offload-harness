#!/usr/bin/env bash
# install.sh — Linux install path for the offload harness.
#
# setup/detect.ps1 refuses to run anywhere but Windows, so a Linux node had no
# supported install: every one was hand-built, and on the measured 6 GB node the
# first two hand-derived serving topologies each broke the box. This script is the
# supported path.
#
# It is DELIBERATELY thin. Every decision that can be wrong lives in the harness
# binary, which is cross-compiled and unit-tested:
#
#   local-offload install detect   -> which hardware tier is this machine?
#   local-offload install volumes  -> which disk should hold it? (never the OS disk)
#   local-offload install seed     -> the media bindings that tier ships, for THIS OS
#   local-offload install render   -> the llama-swap serving config for that tier
#   local-offload acceptance       -> the gate: is the node actually able to work?
#
# This script only fetches, places, and registers. If you find yourself adding a
# decision here, it belongs in the binary instead — that is the whole reason the
# Windows installer and this one cannot drift.
#
# PREREQUISITES (this script does not build or download them, and says so up front):
#   - a built llama.cpp (llama-server + its shared objects), at or above the floors in
#     setup/floors.json: the harness floor, and the per-seat floors for a tier that renders the
#     embeddinggemma2 memory-stack entry or a RAM-spill agent seat. A node that must keep an older
#     main build passes a second build that meets the embeddinggemma2 seat floor for that entry
#     alone as --llama-bin-eg2
#   - the GGUF model files for the tier
#   - node (for the render runners), and the local-offload binary itself
#
# THE FRONTIER RULE: this file carries no version, build number, digest or size for any
# component. Everything it fetches (HyperFrames today) is resolved from its publisher at run
# time (`resolve_source`) and checked against the publisher's own digest; the only numbers that
# may exist live in setup/floors.json, and they are floors, never ceilings. A newer
# hand-placed build is kept, never downgraded. A component is staged beside the live path and
# promoted by rename; it is never installed over a live directory.
#
# `source setup/install.sh --lib` defines the functions and stops, for a caller that wants
# `resolve_source <key>` alone. OFFLOAD_RESOLVE=offline answers from setup/testdata/resolve-offline.json
# and never touches the network.
set -euo pipefail

PREFIX=""
LLAMA_BIN=""
LLAMA_BIN_CPU=""
LLAMA_BIN_EG2=""
MODELS=""
LISTEN="127.0.0.1:11436"
NODE_ID="$(hostname)"
SERVICE_USER="$(id -un)"
BIN=""
DRY_RUN=0
NO_SERVICE=0
CLIENT=0
REMOTES=""
TOKEN_FILE=""
# The repo this script lives in, for `install vllm-seat --root` (it reads the seat
# templates under setup/templates/vllm-seat/). Resolved from $0, not $PWD.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FLOORS_FILE="$REPO_ROOT/setup/floors.json"
RESOLVE_FIXTURE="$REPO_ROOT/setup/testdata/resolve-offline.json"
# The deployment half of a tier's vLLM agent seat. All three may stay empty: the
# renderer falls back to the tier's llama.cpp agent seat and prints why, which is the
# correct outcome on a box that has not hand-built the vLLM venv.
#   TS_IP        - the LITERAL address the engine binds. A hostname is wrong here: on
#                  the reference box MagicDNS resolved to IPv6 only while vLLM bound
#                  the Tailscale IPv4, so llama-swap's health check would never pass.
#   VLLM_VENV    - default <prefix>/vllm-env, chosen by the renderer.
#   HF_HOME_DIR  - default $HF_HOME, else <prefix>/hf. KEEP IT SHORT: LMCache's
#                  fs_native page names embed the resolved model path and a long one
#                  produced 268-byte names against NAME_MAX 255, failing every L2
#                  store silently.
TS_IP="$(tailscale ip -4 2>/dev/null | head -n1 || true)"
VLLM_VENV=""
HF_HOME_DIR=""

die() { printf '\nerror: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }
run() {
  if [ "$DRY_RUN" -eq 1 ]; then printf '  would run: %s\n' "$*"; else "$@"; fi
}

# ---- resolution: every component is "latest" from its publisher ---------------
# floor_of <component>: the harness floor in setup/floors.json, empty when there is none (or no file).
floor_of() {
  [ -r "$FLOORS_FILE" ] || return 0
  jq -r --arg c "$1" '(.[$c].harness // empty) | tostring' "$FLOORS_FILE" 2>/dev/null || true
}

# version_ge A B: true when A is the same as or newer than B (version sort).
version_ge() { [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -n1)" = "$1" ]; }

# resolve_source <key>: one JSON line {version, digest, url, size, source} for the newest published
# build of <key>, with the digest the PUBLISHER states for it. Nonzero (and a reason on stderr) when it
# cannot be resolved or the publisher states no usable digest. The npm registry is the only source this
# installer fetches from: dist-tags.latest names the version, dist.integrity (an SRI digest) is its digest,
# and the registry is asked with no Accept header, so it answers with the full
# document that carries both.
resolve_source() {
  local key="${1:?resolve_source needs a component key}" out
  if [ "${OFFLOAD_RESOLVE:-}" = "offline" ]; then
    [ -r "$RESOLVE_FIXTURE" ] || { echo "resolve: $RESOLVE_FIXTURE is missing (OFFLOAD_RESOLVE=offline)" >&2; return 1; }
    jq -ce --arg k "$key" '.[$k] // error("no offline entry for " + $k)' "$RESOLVE_FIXTURE"
    return
  fi
  case "$key" in
    hyperframes)
      out="$(curl -fsS --retry 3 --max-time 120 "https://registry.npmjs.org/$key" \
        | jq -ce --arg pkg "$key" '
            (."dist-tags".latest) as $v
            | (.versions[$v // ""].dist // {}) as $d
            | select($v != null and (($d.integrity // "") | test("^sha(256|384|512)-")) and ($d.tarball // "") != "")
            | {version: $v, digest: $d.integrity, url: $d.tarball, size: ($d.unpackedSize // null), source: ("npm:" + $pkg)}')" \
        || { echo "resolve: the npm registry did not answer for $key" >&2; return 1; }
      [ -n "$out" ] || { echo "resolve: the npm registry states no latest version with an SRI integrity for $key" >&2; return 1; }
      printf '%s\n' "$out" ;;
    *) echo "resolve: unknown component $key" >&2; return 1 ;;
  esac
}

# record_component <key> <json>: merge {version, digest, size, source, installed_at} into
# $MANIFEST as schema 2. A schema 1 file (no components) is upgraded in place, every other key kept;
# a record already at the same version is left alone. Written beside the file and renamed.
record_component() {
  local key="$1" rec="$2" now
  now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  jq --arg k "$key" --argjson rec "$rec" --arg now "$now" '
      if ((.components // {}) | type == "object") and (.components[$k].version // null) == $rec.version then .
      else .schema = 2
        | .components = ((.components // {}) | if type == "object" then . else {} end)
        | .components[$k] = ($rec + {installed_at: $now})
      end' "$MANIFEST" > "$MANIFEST.new" \
    && mv "$MANIFEST.new" "$MANIFEST" || { rm -f "$MANIFEST.new"; return 1; }
}

if [ "${1:-}" = "--lib" ]; then return 0 2>/dev/null || exit 0; fi

usage() {
  cat <<'USAGE'
Usage: install.sh [options]

  --bin PATH          the local-offload binary to install from (default: ./local-offload
                      or the first on PATH)
  --prefix DIR        install root. Default: chosen by `install volumes` — the volume with
                      the most free space, never the OS volume.
  --llama-bin DIR     directory holding llama-server and its shared objects (required, except on a
                      tier whose backend is rk3588: the NPU serves there and no llama.cpp runs)
  --llama-bin-cpu DIR directory of a CPU llama-server build: renders the tier's CPU seat family
                      beside its GPU seats (only for a tier that declares alt_backends [cpu])
  --llama-bin-eg2 DIR directory of the llama.cpp build that serves ONLY the embeddinggemma2 entry
                      (default: --llama-bin). For a node whose main build is below the embeddinggemma2
                      seat floor in setup/floors.json, which cannot load that entry; only for a tier
                      that renders it (include_embeddinggemma2)
  --models DIR        directory holding the GGUF files (default: <prefix>/models)
  --listen ADDR       llama-swap listen address (default 127.0.0.1:11436)
  --node-id NAME      fleet node id (default: hostname)
  --user NAME         the identity the services run as (default: you). Capability is
                      IDENTITY-DEPENDENT, so this must be the account that owns the
                      models, venvs and lease.
  --no-service        write configs but do not register systemd units
  --vllm-venv DIR     hand-built vLLM virtualenv for a tier that declares a vllm_seat
                      (default: <prefix>/vllm-env). Absent = the tier's llama.cpp
                      agent seat is rendered instead, with the reason printed.
  --hf-home DIR       HF cache root (default: $HF_HOME, else <prefix>/hf). Keep it
                      short: LMCache page names embed the model path (NAME_MAX 255).
  --tailscale-ip ADDR literal address the vLLM engine binds (default: tailscale ip -4)
  --client            install a DELEGATION CLIENT instead (ADR 0071): the binary and a config with
                      no local model, no media lane and no service, delegating agent work and
                      compositions to --remotes; registers the local-offload MCP for Claude Code
  --remotes LIST      with --client: comma-separated fleet nodes (http://<node>:18811)
  --token-file PATH   the file holding the fleet token (never printed). With --client: the delegation
                      client's token. On a node: stored as fleet_auth_token in the node's config, which a
                      node that serves the tailnet needs before its unit is registered (a config that
                      already holds fleet_auth_token is enough; --no-service asks for none)
  --dry-run           print every decision and command, change nothing
  -h, --help          this text
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --bin) BIN="${2:?}"; shift 2 ;;
    --prefix) PREFIX="${2:?}"; shift 2 ;;
    --llama-bin) LLAMA_BIN="${2:?}"; shift 2 ;;
    --llama-bin-cpu) LLAMA_BIN_CPU="${2:?}"; shift 2 ;;
    --llama-bin-eg2) LLAMA_BIN_EG2="${2:?}"; shift 2 ;;
    --models) MODELS="${2:?}"; shift 2 ;;
    --listen) LISTEN="${2:?}"; shift 2 ;;
    --node-id) NODE_ID="${2:?}"; shift 2 ;;
    --user) SERVICE_USER="${2:?}"; shift 2 ;;
    --no-service) NO_SERVICE=1; shift ;;
    --vllm-venv) VLLM_VENV="${2:?}"; shift 2 ;;
    --hf-home) HF_HOME_DIR="${2:?}"; shift 2 ;;
    --tailscale-ip) TS_IP="${2:?}"; shift 2 ;;
    --client) CLIENT=1; shift ;;
    --remotes) REMOTES="${2:?}"; shift 2 ;;
    --token-file) TOKEN_FILE="${2:?}"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option $1 (try --help)" ;;
  esac
done

# ---- the binary makes every decision, so find it first ----------------------
if [ -z "$BIN" ]; then
  if [ -x "./local-offload" ]; then BIN="$(pwd)/local-offload"
  elif command -v local-offload >/dev/null 2>&1; then BIN="$(command -v local-offload)"
  else die "no local-offload binary found — pass --bin PATH"; fi
fi
[ -x "$BIN" ] || die "$BIN is not executable"
say "harness:   $("$BIN" --version)"
say "identity:  $SERVICE_USER (services will run as this account)"

command -v jq >/dev/null 2>&1 || die "jq is required (sudo apt install jq)"

# ---- client mode: a delegation client (ADR 0071) ----------------------------
# A machine too small for any local lane (a 2-core laptop, an arm64 single-board computer) still
# runs the harness to place work on the fleet. It is a role, not a hardware tier: no detect, no
# models, no llama.cpp, no service. `install client` renders its config from the binary.
if [ "$CLIENT" -eq 1 ]; then
  [ -n "$REMOTES" ] || die "--client requires --remotes (the fleet nodes this machine delegates to)"
  [ -n "$TOKEN_FILE" ] || die "--client requires --token-file (the fleet token)"
  [ "$DRY_RUN" -eq 1 ] || [ -r "$TOKEN_FILE" ] || die "--token-file $TOKEN_FILE is not readable"
  if [ -z "$PREFIX" ]; then
    VOL_JSON="$("$BIN" install volumes --json 2>/dev/null || true)"
    ROOT="$(printf '%s' "$VOL_JSON" | jq -r '.choice.volume.root // empty')"
    [ -n "$ROOT" ] || die "no eligible install volume; a one-volume machine needs an explicit --prefix (e.g. --prefix \$HOME/offload-client)"
    PREFIX="${ROOT%/}/offload-client"
  fi
  say "mode:      delegation client (no local model, no media lane, no service)"
  say "remotes:   $REMOTES"
  say "prefix:    $PREFIX"
  for d in "$PREFIX" "$PREFIX/etc" "$PREFIX/bin"; do run mkdir -p "$d"; done
  run install -m 0755 "$BIN" "$PREFIX/bin/local-offload"
  CLIENT_CFG="$PREFIX/etc/config.json"
  if [ -f "$CLIENT_CFG" ]; then
    say "config:    $CLIENT_CFG exists, left untouched (re-render: $PREFIX/bin/local-offload install client ... --force)"
  else
    run "$PREFIX/bin/local-offload" install client --home "$PREFIX" --remotes "$REMOTES" --token-file "$TOKEN_FILE" --config "$CLIENT_CFG"
  fi
  if command -v claude >/dev/null 2>&1; then
    if [ "$DRY_RUN" -eq 0 ] && claude mcp list 2>/dev/null | grep -q '^local-offload:'; then
      say "mcp:       local-offload is already registered for $(id -un)"
    else
      run claude mcp add local-offload --scope user -- "$PREFIX/bin/local-offload" mcp --config "$CLIENT_CFG"
    fi
  else
    say "mcp:       claude is not on PATH; register later: claude mcp add local-offload --scope user -- $PREFIX/bin/local-offload mcp --config $CLIENT_CFG"
  fi
  run "$PREFIX/bin/local-offload" acceptance --config "$CLIENT_CFG"
  say "done:      delegation client installed"
  exit 0
fi

# ---- 1. which tier is this machine? -----------------------------------------
DETECT_JSON="$("$BIN" install detect --json)"
TIER="$(printf '%s' "$DETECT_JSON" | jq -r .verdict.profile)"
[ -n "$TIER" ] && [ "$TIER" != "null" ] || die "could not classify this machine"
say "tier:      $TIER  ($(printf '%s' "$DETECT_JSON" | jq -r .verdict.reason))"

# ram_tier gates BOTH the 26B placement (a partial --n-cpu-moe needs a real RAM path, and the
# every-expert form is written for no tier with a card) and the RAM-gated media seed. This script passed
# neither, so both gates were inert on Linux: a tier served the 26B on a box that
# cannot host it, and the mid/high-only image seed never applied. An EMPTY value means
# "do not gate", so it is a hard failure here rather than a silent downgrade.
RAM_TIER="$(printf '%s' "$DETECT_JSON" | jq -r '.verdict.ram_tier // empty')"
[ -n "$RAM_TIER" ] || die "detect returned no ram_tier — refusing to install with both RAM gates inert"
say "ram_tier:  $RAM_TIER  ($(printf '%s' "$DETECT_JSON" | jq -r .facts.ram_gb) GB)"

# ADR 0024: accelerators ride BESIDE the tier. This script merged no accelerator seed
# at all until the Coral tier (D4) — install.ps1 always had. OFFLOAD_ACCELERATORS
# overrides the probe, exactly as on Windows.
ACCELERATORS="$(printf '%s' "$DETECT_JSON" | jq -r '(.verdict.accelerators // []) | join(",")')"
[ -n "${OFFLOAD_ACCELERATORS:-}" ] && ACCELERATORS="$OFFLOAD_ACCELERATORS"
[ -n "$ACCELERATORS" ] && say "accel:     $ACCELERATORS"

# ---- 2. where should it live? -----------------------------------------------
if [ -z "$PREFIX" ]; then
  VOL_JSON="$("$BIN" install volumes --json 2>/dev/null || true)"
  ROOT="$(printf '%s' "$VOL_JSON" | jq -r '.choice.volume.root // empty')"
  [ -n "$ROOT" ] || die "no eligible install volume. Run '$BIN install volumes' to see why, then pass --prefix explicitly."
  PREFIX="${ROOT%/}/offload-stack"
  say "prefix:    $PREFIX  ($(printf '%s' "$VOL_JSON" | jq -r '.choice.because'))"
else
  say "prefix:    $PREFIX  (given)"
fi
# ---- 2b. the fleet bearer ------------------------------------------------------
# Every lane of a fleet node requires the fleet bearer, and fleet-serve refuses to start on the tailnet address
# when the config it reads holds no fleet_auth_token: a unit registered without one would restart-loop. So the key
# is settled here, before anything is written. --token-file stores it (the value is never printed); a config that
# already holds it is enough; --no-service writes no unit, so it asks for none (fleet-serve refuses by itself).
CONFIG="$PREFIX/etc/config.json"
KEY_SOURCE=""
if [ -n "$TOKEN_FILE" ]; then
  if [ "$DRY_RUN" -eq 0 ]; then
    [ -r "$TOKEN_FILE" ] || die "--token-file $TOKEN_FILE is not readable"
    [ -n "$(tr -d '[:space:]' < "$TOKEN_FILE")" ] || die "--token-file $TOKEN_FILE is empty"
  fi
  KEY_SOURCE="file"
elif [ -f "$CONFIG" ] && jq -e '(.fleet_auth_token // "") != ""' "$CONFIG" >/dev/null 2>&1; then
  KEY_SOURCE="config"
fi
if [ "$NO_SERVICE" -eq 0 ] && [ -z "$KEY_SOURCE" ]; then
  die "a fleet node needs fleet_auth_token in the config it reads ($CONFIG): every lane requires the fleet bearer, and fleet-serve will not start on the tailnet address without it. Pass --token-file PATH (the file holding the token, the same value on every delegator that calls this node), or put the key in $CONFIG and run again; --no-service skips the unit"
fi

# The tier's serving backend decides whether a llama.cpp build is needed at all: rk3588's template has
# no llama.cpp entry (the NPU serves), so a board there has no build to point at. Every other tier
# still needs one. The backend read is best-effort: an unreadable one keeps the requirement.
BACKEND="$("$BIN" install tier-info --profile "$TIER" --json 2>/dev/null | jq -r '.backend // empty' 2>/dev/null || true)"
if [ "$BACKEND" != "rk3588" ]; then
  [ -n "$LLAMA_BIN" ] || die "--llama-bin is required (the directory holding llama-server and its shared objects)"
fi
[ -z "$LLAMA_BIN" ] || [ -d "$LLAMA_BIN" ] || die "--llama-bin $LLAMA_BIN is not a directory"
[ -z "$LLAMA_BIN_EG2" ] || [ -d "$LLAMA_BIN_EG2" ] || die "--llama-bin-eg2 $LLAMA_BIN_EG2 is not a directory"
[ -n "$MODELS" ] || MODELS="$PREFIX/models"

# ---- 3. lay out the tree ----------------------------------------------------
for d in "$PREFIX" "$PREFIX/etc" "$PREFIX/state" "$PREFIX/bin" "$PREFIX/kvslots" "$MODELS"; do
  run mkdir -p "$d"
done
run install -m 0755 "$BIN" "$PREFIX/bin/local-offload"
# The render runners resolve RELATIVE to the executable, so they must ship beside
# it. Their absence is not cosmetic: every media route resolves to a script that
# is not there, which the acceptance gate reports as BOUND-BUT-MISSING. Say it here
# rather than letting the gate explain it later.
if [ -d "$(dirname "$BIN")/render" ]; then
  run cp -r "$(dirname "$BIN")/render" "$PREFIX/bin/"
else
  say "WARNING:   no render/ directory beside $BIN — every media route will report"
  say "           BOUND-BUT-MISSING until the render runners are placed in $PREFIX/bin/render/"
fi

# ---- 3b. the composition lane (HyperFrames; ADR 0059) ------------------------
# The newest published HyperFrames, resolved from the npm registry at run time (resolve_source) and
# installed project-local with --ignore-scripts into a staging directory beside the live path, never
# `npm install -g` (a global HyperFrames self-upgrades in a detached process). The installed
# integrity must equal the registry's published dist.integrity, and `npm audit signatures` must pass,
# before the generation is promoted by rename to <prefix>/hyperframes-versions/<version>; the live path
# <prefix>/hyperframes is then a symlink to it, swapped by rename. A newer hand-placed install is kept,
# never downgraded. The node floor and the HyperFrames floor come from setup/floors.json. Below the node
# floor the tier's compose_script is UN-bound ("") so the route reads NOT CONFIGURED instead of
# BOUND-BUT-MISSING. An integrity failure is FATAL: an unverifiable install is never bound.
HF_SEED='{"compose_script":""}'
HF_DIR="$PREFIX/hyperframes"
HF_VERSIONS="$PREFIX/hyperframes-versions"
HF_REC=""
NODE_FLOOR="$(floor_of node)"
HF_FLOOR="$(floor_of hyperframes)"
[ -r "$FLOORS_FILE" ] || say "NOTE  $FLOORS_FILE not found - no floor is enforced"
NODE_VER="$(node --version 2>/dev/null || true)"
NODE_MAJOR="$(printf '%s' "$NODE_VER" | sed -n 's/^v\([0-9][0-9]*\)\..*/\1/p')"
if [ -z "$NODE_MAJOR" ]; then
  say "SKIP  hyperframes (compose lane): no node on PATH — compose_video stays unbound"
elif [ -n "$NODE_FLOOR" ] && [ "$NODE_MAJOR" -lt "$NODE_FLOOR" ]; then
  say "SKIP  hyperframes (compose lane): node $NODE_VER found, HyperFrames needs >= $NODE_FLOOR — compose_video stays unbound"
elif ! command -v npm >/dev/null 2>&1; then
  say "SKIP  hyperframes (compose lane): node $NODE_VER found but no npm beside it — compose_video stays unbound"
elif [ "$DRY_RUN" -eq 1 ] || [ "${OFFLOAD_RESOLVE:-}" = "offline" ]; then
  # A dry run changes nothing, and an offline resolve has no registry to install from: say what would happen.
  if HF_RES="$(resolve_source hyperframes)"; then
    say "  would install hyperframes $(printf '%s' "$HF_RES" | jq -r .version) (digest $(printf '%s' "$HF_RES" | jq -r .digest)) into $HF_VERSIONS, then point $HF_DIR at it (npm install --ignore-scripts, integrity check, npm audit signatures, npm rebuild esbuild, browser ensure)"
  else
    say "  would install the newest published hyperframes into $HF_VERSIONS (it could not be resolved just now)"
  fi
else
  HF_RES="$(resolve_source hyperframes)" || die "could not resolve hyperframes from its publisher (the npm registry)"
  HF_VER="$(printf '%s' "$HF_RES" | jq -r .version)"
  HF_DIGEST="$(printf '%s' "$HF_RES" | jq -r .digest)"
  # The version names directories below (staging, the generation, the symlink): only a semver shape is ever used
  # in a path, so a registry answer cannot steer rm -rf or mv outside the versions tree.
  printf '%s' "$HF_VER" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$' \
    || die "the npm registry answered an unusable hyperframes version '$HF_VER' (not a semver); refusing to use it in a path"
  if [ -n "$HF_FLOOR" ] && ! version_ge "$HF_VER" "$HF_FLOOR"; then
    die "the newest published hyperframes ($HF_VER) is below the floor $HF_FLOOR in setup/floors.json"
  fi
  HF_HAVE="$(jq -r '.version // empty' "$HF_DIR/node_modules/hyperframes/package.json" 2>/dev/null || true)"
  HF_ENSURE_DIR="$HF_DIR"
  HF_GEN=""
  if [ -n "$HF_HAVE" ] && version_ge "$HF_HAVE" "$HF_VER"; then
    say "hyperframes: $HF_HAVE is installed and the newest published is $HF_VER — kept, never downgraded"
    HF_ID="$HF_HAVE"
    HF_KEPT_DIGEST="$HF_DIGEST"
    [ "$HF_HAVE" = "$HF_VER" ] || HF_KEPT_DIGEST="$(jq -r '.packages["node_modules/hyperframes"].integrity // ""' "$HF_DIR/package-lock.json" 2>/dev/null || true)"
    HF_REC="$(printf '%s' "$HF_RES" | jq -c --arg v "$HF_HAVE" --arg d "$HF_KEPT_DIGEST" '{version: $v, digest: $d, size: (if $v == .version then .size else null end), source: .source}')"
  else
    HF_ID="$HF_VER"
    HF_GEN="$HF_VERSIONS/$HF_VER"
    HF_STAGE="$HF_VERSIONS/.staging/$HF_VER.$$"
    hf_fail() { rm -rf "$HF_STAGE"; die "$*"; }
    mkdir -p "$HF_STAGE"
    printf '{"name":"offload-harness-hyperframes","version":"0.0.0","private":true}\n' > "$HF_STAGE/package.json"
    (cd "$HF_STAGE" && npm install --ignore-scripts --no-audit --no-fund "hyperframes@$HF_VER") || hf_fail "npm install (hyperframes $HF_VER) failed"
    HF_GOT="$(jq -r '.packages["node_modules/hyperframes"].integrity // empty' "$HF_STAGE/package-lock.json" 2>/dev/null || true)"
    [ "$HF_GOT" = "$HF_DIGEST" ] || hf_fail "hyperframes $HF_VER does not match the digest its publisher states (installed ${HF_GOT:-none}, published $HF_DIGEST) — refusing to promote it"
    (cd "$HF_STAGE" && npm audit signatures) || hf_fail "npm audit signatures FAILED for the hyperframes install — registry signature or provenance check did not pass; refusing to bind the compose lane"
    (cd "$HF_STAGE" && npm rebuild esbuild) || hf_fail "npm rebuild esbuild failed"
    # The lock npm wrote stays inside the generation as its record. Against the previous generation's lock, name
    # every package whose version moved (detect, not gate: the gate is the real render that follows).
    if [ -n "$HF_HAVE" ] && [ -f "$HF_VERSIONS/$HF_HAVE/package-lock.json" ]; then
      HF_MOVED="$(comm -13 \
        <(jq -r '.packages | to_entries[] | select(.key != "") | "\(.key) \(.value.version)"' "$HF_VERSIONS/$HF_HAVE/package-lock.json" 2>/dev/null | sort) \
        <(jq -r '.packages | to_entries[] | select(.key != "") | "\(.key) \(.value.version)"' "$HF_STAGE/package-lock.json" 2>/dev/null | sort) \
        | sed 's#^node_modules/##')"
      [ -z "$HF_MOVED" ] || say "hyperframes: packages that changed against $HF_HAVE ($(printf '%s\n' "$HF_MOVED" | wc -l | tr -d ' ')): $(printf '%s\n' "$HF_MOVED" | head -40 | tr '\n' ';')"
    fi
    if [ -e "$HF_GEN" ]; then
      rm -rf "$HF_STAGE"
      say "hyperframes: generation $HF_VER already present (write-once) — reused"
    else
      mv "$HF_STAGE" "$HF_GEN" || hf_fail "could not promote $HF_STAGE to $HF_GEN"
    fi
    HF_ENSURE_DIR="$HF_GEN"
    HF_REC="$(printf '%s' "$HF_RES" | jq -c '{version, digest, size, source}')"
  fi
  # browser ensure runs through the harness runner: the scrubbed env, --json, and
  # HyperFrames' state in <dir>/home — never the service account's real home. A new generation is
  # ensured BEFORE the live path moves, so a failure leaves the live install exactly as it was.
  HF_RUNNER="$PREFIX/bin/render/compose-hyperframes.mjs"
  [ -f "$HF_RUNNER" ] || HF_RUNNER="$REPO_ROOT/render/compose-hyperframes.mjs"
  HF_JSON="$(node "$HF_RUNNER" browser --hyperframes-dir "$HF_ENSURE_DIR" | tail -n1 || true)"
  HF_BROWSER="$(printf '%s' "$HF_JSON" | jq -r 'select(.ok == true) | .browser_path // empty' 2>/dev/null || true)"
  [ -n "$HF_BROWSER" ] || die "hyperframes browser ensure failed: $HF_JSON"
  if [ -n "$HF_GEN" ]; then
    # Promote: an existing real directory at the live path becomes a generation of its own (named by the
    # version it holds), then the live path is a symlink to the new generation, swapped by rename.
    # The link is made first, so a filesystem that cannot hold one fails before the live path is touched,
    # and a failed swap puts a migrated directory back.
    rm -f "$HF_DIR.new"
    ln -s "hyperframes-versions/$HF_VER" "$HF_DIR.new" || die "could not create a symlink beside $HF_DIR (the live path must be a symlink to a generation)"
    HF_OLD_ID=""
    if [ -d "$HF_DIR" ] && [ ! -L "$HF_DIR" ]; then
      HF_OLD_ID="${HF_HAVE:-legacy-$(date -u +%Y%m%dT%H%M%SZ)}"
      [ ! -e "$HF_VERSIONS/$HF_OLD_ID" ] || HF_OLD_ID="$HF_OLD_ID-$$"
      mv "$HF_DIR" "$HF_VERSIONS/$HF_OLD_ID" || { rm -f "$HF_DIR.new"; die "could not move the live $HF_DIR into $HF_VERSIONS/$HF_OLD_ID"; }
    fi
    if ! mv -T "$HF_DIR.new" "$HF_DIR"; then
      rm -f "$HF_DIR.new"
      [ -z "$HF_OLD_ID" ] || mv "$HF_VERSIONS/$HF_OLD_ID" "$HF_DIR"
      die "could not point $HF_DIR at $HF_GEN"
    fi
    [ -z "$HF_OLD_ID" ] || say "hyperframes: the live directory became generation $HF_OLD_ID"
    # The browser path the config binds names the LIVE path, so the next generation changes nothing in it.
    case "$HF_BROWSER" in "$HF_GEN"/*) HF_BROWSER="$HF_DIR${HF_BROWSER#"$HF_GEN"}" ;; esac
    [ -e "$HF_BROWSER" ] || die "the browser $HF_BROWSER is not reachable through the live path $HF_DIR"
  else
    (cd "$HF_DIR" && npm audit signatures) || die "npm audit signatures FAILED for the hyperframes install — registry signature or provenance check did not pass; refusing to bind the compose lane"
  fi
  # "auto": the managed browser of the installed release, resolved at render time (never one Chrome version's path)
  HF_SEED="$(jq -n --arg dir "$HF_DIR" \
    '{compose_script: "render/compose-hyperframes.mjs", hyperframes_dir: $dir, hyperframes_browser_path: "auto"}')"
  say "hyperframes: $HF_ID, chrome-headless-shell $(printf '%s' "$HF_JSON" | jq -r .chrome_version) at $HF_BROWSER"
fi

# ---- 4. the harness config: one home key plus this tier's media bindings -----
# A seed failure is FATAL, never a silent '{}': an install that quietly ships no
# media bindings is exactly the drift this path exists to end. A tier that
# genuinely has none says so on stdout and is not an error.
if ! SEED="$("$BIN" install seed --profile "$TIER" --home "$PREFIX" --os linux --ram-tier "$RAM_TIER" \
        --vllm-venv "$VLLM_VENV" --hf-home "$HF_HOME_DIR" --accelerators "$ACCELERATORS" \
        --hailo-home "${HAILO_HOME:-$PREFIX/hailo}" --coral-home "${CORAL_HOME:-$PREFIX/coral}" \
        --rknpu-home "${RKNPU_HOME:-$PREFIX/rknpu}")"; then
  die "could not resolve the media seed for tier $TIER"
fi
case "$SEED" in *"ships no media"*) SEED='{}'; say "media:     tier $TIER ships none — text only until bound by hand" ;; esac
# The composition lane (3b) binds or un-binds itself over the tier seed.
SEED="$(printf '%s' "$SEED" | jq --argjson hf "$HF_SEED" '. + $hf')"
if [ -f "$CONFIG" ] && [ "$DRY_RUN" -eq 0 ]; then
  say "config:    $CONFIG exists — left untouched (delete it to regenerate)"
  # A box that just gained the composition lane is told exactly which keys bind it.
  if [ -n "${HF_BROWSER:-}" ] && ! jq -e '(.hyperframes_dir // "") != ""' "$CONFIG" >/dev/null 2>&1; then
    say "NOTE  compose lane installed but $CONFIG predates it — add: $(printf '%s' "$HF_SEED" | jq -c .)"
  fi
else
  BODY="$(printf '%s' "$SEED" | jq --arg home "$PREFIX" '. + {home: $home}')"
  if [ "$DRY_RUN" -eq 1 ]; then
    say "  would write $CONFIG:"; printf '%s\n' "$BODY" | sed 's/^/    /'
  else
    printf '%s\n' "$BODY" > "$CONFIG"
    say "config:    $CONFIG written (home + $(printf '%s' "$SEED" | jq -r 'keys|length') seeded key(s))"
  fi
fi
# The fleet bearer (2b): merged over whatever the config holds, so no other key changes; the file is readable by its owner only.
if [ "$KEY_SOURCE" = "file" ]; then
  if [ "$DRY_RUN" -eq 1 ]; then
    say "  would store fleet_auth_token from $TOKEN_FILE in $CONFIG (the value is never printed)"
  else
    ( umask 077; jq --rawfile t "$TOKEN_FILE" '.fleet_auth_token = ($t | gsub("^\\s+|\\s+$"; ""))' "$CONFIG" > "$CONFIG.new" ) \
      || { rm -f "$CONFIG.new"; die "could not store fleet_auth_token in $CONFIG"; }
    chmod 600 "$CONFIG.new"
    mv "$CONFIG.new" "$CONFIG"
    # Readable by its owner only, and the unit and the acceptance gate run as the service identity: when that is not the
    # installing user, the file is handed to it (the installer escalates with sudo for the unit and the gate already).
    if [ "$SERVICE_USER" != "$(id -un)" ]; then
      sudo chown "$SERVICE_USER" "$CONFIG" \
        || die "could not hand $CONFIG to $SERVICE_USER: it holds fleet_auth_token and is readable by its owner only, and the fleet node and the acceptance gate run as $SERVICE_USER"
    fi
    say "config:    fleet_auth_token stored in $CONFIG from --token-file (the value is not shown)"
  fi
elif [ "$KEY_SOURCE" = "config" ]; then
  say "config:    $CONFIG holds fleet_auth_token (the value is not shown)"
fi

# The install manifest the fleet node reads (installed.json: profile + accelerators). The node falls
# back to config.json's list when it is absent (Coral D6). Written when absent. When one exists it
# keeps what it recorded, except that a profile other than the tier this run detected (and rendered
# the serving config for) is rewritten together with its backend: install.ps1 rebuilds the file
# whole on every run, and without this a box that gained a card kept its older layout in the
# manifest for good, next to a serving config rendered for the newer one. Accelerators, alt_backends
# and every other key stay as recorded.
MANIFEST="$PREFIX/installed.json"
if [ ! -f "$MANIFEST" ]; then
  if [ "$DRY_RUN" -eq 0 ]; then
    # backend from the tier table; alt_backends lists only what THIS install rendered (the CPU family
    # needs --llama-bin-cpu), so health never advertises a route the box cannot serve.
    BACKEND="$("$BIN" install tier-info --profile "$TIER" --json | jq -r .backend)"
    ALT=""; [ -n "$LLAMA_BIN_CPU" ] && ALT="cpu"
    jq -n --arg profile "$TIER" --arg accel "$ACCELERATORS" --arg backend "$BACKEND" --arg alt "$ALT" \
      '{schema: 2, profile: $profile, backend: $backend, accelerators: ($accel | if . == "" then [] else split(",") end), alt_backends: ($alt | if . == "" then [] else split(",") end), components: {}}' > "$MANIFEST.new"
    mv "$MANIFEST.new" "$MANIFEST"
    say "manifest:  $MANIFEST written"
  fi
elif ! RECORDED="$(jq -r '.profile // empty' "$MANIFEST" 2>/dev/null)"; then
  say "NOTE  manifest: $MANIFEST is not readable JSON — left untouched"
elif [ "$RECORDED" != "$TIER" ]; then
  if [ "$DRY_RUN" -eq 1 ]; then
    say "manifest:  would rewrite $MANIFEST profile ${RECORDED:-(none)} -> $TIER (backend from the tier table, other keys kept)"
  else
    BACKEND="$("$BIN" install tier-info --profile "$TIER" --json | jq -r .backend)"
    jq --arg profile "$TIER" --arg backend "$BACKEND" '. + {profile: $profile, backend: $backend}' "$MANIFEST" > "$MANIFEST.new"
    # (a schema 1 file is upgraded to 2 only when a component is recorded into it, below)
    mv "$MANIFEST.new" "$MANIFEST"
    say "manifest:  $MANIFEST profile ${RECORDED:-(none)} -> $TIER, backend $BACKEND (other keys kept)"
  fi
fi
# Schema 2 (components.<key> = {version, digest, size, source, installed_at}): what this run installed or
# kept is merged in, over a schema 1 file too (every other key stays), replaced by rename. A run that
# installed nothing leaves the file untouched.
if [ -n "$HF_REC" ] && [ "$DRY_RUN" -eq 0 ] && [ -f "$MANIFEST" ] && jq -e . "$MANIFEST" >/dev/null 2>&1; then
  record_component hyperframes "$HF_REC" || die "could not record hyperframes in $MANIFEST"
  say "manifest:  $MANIFEST records hyperframes $(printf '%s' "$HF_REC" | jq -r .version) (schema 2)"
fi

# ---- 5. the serving config --------------------------------------------------
SWAP_YAML="$PREFIX/etc/llama-swap.yaml"
if [ "$DRY_RUN" -eq 1 ]; then
  say "  would render $SWAP_YAML for tier $TIER${LLAMA_BIN:+ --llama-bin $LLAMA_BIN}${LLAMA_BIN_CPU:+ --llama-bin-cpu $LLAMA_BIN_CPU}${LLAMA_BIN_EG2:+ --llama-bin-eg2 $LLAMA_BIN_EG2}"
else
  # --home is load-bearing: a tier's media seats name their binaries under the
  # install root, and without it the render REFUSES — after step 4 has already
  # written a config.json binding those seats' aliases.
  "$BIN" install render --profile "$TIER" --os linux --home "$PREFIX" \
    --rknpu-home "${RKNPU_HOME:-$PREFIX/rknpu}" --ram-tier "$RAM_TIER" \
    --vllm-user "$SERVICE_USER" --vllm-proxy-host "$TS_IP" \
    --vllm-venv "$VLLM_VENV" --hf-home "$HF_HOME_DIR" \
    ${LLAMA_BIN:+--llama-bin "$LLAMA_BIN"} ${LLAMA_BIN_CPU:+--llama-bin-cpu "$LLAMA_BIN_CPU"} ${LLAMA_BIN_EG2:+--llama-bin-eg2 "$LLAMA_BIN_EG2"} --models "$MODELS" --listen "$LISTEN" --out "$SWAP_YAML"
fi

# ---- 5b. the persistent vLLM agent seat (ADR 0035), when this box can run it ----
# The engine is a HAND-BUILT venv (vLLM 0.28, torch 2.13+cu130) holding an HF
# snapshot; this installer does not create either. So the seat renders only when both
# are present, and a box without them keeps the tier's llama.cpp fallback -- a unit
# pointing at a venv nobody built fails at boot, which is worse than the fallback.
# The two root steps (the unit and the polkit rule) are PRINTED, never taken here:
# this script runs unprivileged and escalating silently is not its job.
if [ "$DRY_RUN" -eq 1 ]; then
  say "  would render the vLLM agent seat for tier $TIER (if its venv + weights are present)"
elif "$BIN" install vllm-seat --profile "$TIER" --root "$REPO_ROOT" --home "$PREFIX" \
       --user "$SERVICE_USER" --proxy-host "$TS_IP" \
       --venv "$VLLM_VENV" --hf-home "$HF_HOME_DIR" 2>/dev/null; then
  chmod 0755 "$PREFIX"/seat/*.sh 2>/dev/null || true
  say "vllm seat: rendered under $PREFIX/seat (two root steps printed above)"
else
  say "vllm seat: not rendered for tier $TIER (no vllm_seat declared, or its venv/weights are absent) --"
  say "           the tier's llama.cpp agent seat is what got rendered and bound"
fi

# ---- 6. services ------------------------------------------------------------
if [ "$NO_SERVICE" -eq 1 ]; then
  say "services:  skipped (--no-service)"
else
  UNIT="/etc/systemd/system/offload-fleet-node.service"
  unit_body() {
    cat <<UNIT
# Generated by setup/install.sh. Binds the TAILSCALE address only, and every fleet lane
# requires the fleet bearer (fleet_auth_token in $CONFIG, which fleet-serve refuses to
# start without), so never 0.0.0.0. Restart=on-failure covers the boot race where
# tailscaled has not yet assigned the address.
[Unit]
Description=offload-harness fleet node ($NODE_ID)
After=network-online.target tailscaled.service llama-swap.service
Wants=network-online.target

[Service]
Type=simple
User=$SERVICE_USER
WorkingDirectory=$PREFIX
Environment=HOME=$PREFIX
Environment=LOCAL_OFFLOAD_CONFIG=$CONFIG
# OFFLOAD_HOME is where fleet-serve looks for installed.json (the tier manifest that
# names vendor/arch and the UMA memory model). Without it the node resolved
# $HOME/offload-stack/installed.json, found nothing, and advertised vendor=unknown
# with a non-UMA composition on an APU (<node-f>, 2026-09-20).
Environment=OFFLOAD_HOME=$PREFIX
ExecStart=/bin/sh -c '$PREFIX/bin/local-offload fleet-serve --listen "\$(tailscale ip -4)":18811 --listen-trusted-network --node-id $NODE_ID'
Restart=on-failure
# Since 0.144.1 fleet-serve refuses an empty host (":18811" = every interface), so
# the boot race FAILS and this restart retries until the tailnet address exists;
# 15 s keeps the retries clear of systemd's default start limit (5 starts in 10 s).
RestartSec=15

[Install]
WantedBy=multi-user.target
UNIT
  }
  if [ "$DRY_RUN" -eq 1 ]; then
    say "  would write $UNIT:"; unit_body | sed 's/^/    /'
  else
    unit_body | sudo tee "$UNIT" >/dev/null
    sudo systemctl daemon-reload
    sudo systemctl enable --now offload-fleet-node.service
    say "services:  offload-fleet-node registered and started as $SERVICE_USER"
  fi
fi

# ---- 7. the gate ------------------------------------------------------------
# Run AS the service identity: capability is identity-dependent, and an install
# verified as the wrong account is the failure mode this gate exists to catch.
say ""
if [ "$DRY_RUN" -eq 1 ]; then
  say "would verify: sudo -u $SERVICE_USER $PREFIX/bin/local-offload --config $CONFIG acceptance"
  say ""
  say "dry run — nothing was changed."
  exit 0
fi
if [ "$SERVICE_USER" = "$(id -un)" ]; then
  "$PREFIX/bin/local-offload" --config "$CONFIG" acceptance
else
  sudo -u "$SERVICE_USER" "$PREFIX/bin/local-offload" --config "$CONFIG" acceptance
fi
