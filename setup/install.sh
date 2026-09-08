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
#   - a built llama.cpp (llama-server + its shared objects)
#   - the GGUF model files for the tier
#   - node (for the render runners), and the local-offload binary itself
set -euo pipefail

PREFIX=""
LLAMA_BIN=""
MODELS=""
LISTEN="127.0.0.1:11436"
NODE_ID="$(hostname)"
SERVICE_USER="$(id -un)"
BIN=""
DRY_RUN=0
NO_SERVICE=0
# The repo this script lives in, for `install vllm-seat --root` (it reads the seat
# templates under setup/templates/vllm-seat/). Resolved from $0, not $PWD.
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
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

usage() {
  cat <<'USAGE'
Usage: install.sh [options]

  --bin PATH          the local-offload binary to install from (default: ./local-offload
                      or the first on PATH)
  --prefix DIR        install root. Default: chosen by `install volumes` — the volume with
                      the most free space, never the OS volume.
  --llama-bin DIR     directory holding llama-server and its shared objects (required)
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
  --dry-run           print every decision and command, change nothing
  -h, --help          this text
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --bin) BIN="${2:?}"; shift 2 ;;
    --prefix) PREFIX="${2:?}"; shift 2 ;;
    --llama-bin) LLAMA_BIN="${2:?}"; shift 2 ;;
    --models) MODELS="${2:?}"; shift 2 ;;
    --listen) LISTEN="${2:?}"; shift 2 ;;
    --node-id) NODE_ID="${2:?}"; shift 2 ;;
    --user) SERVICE_USER="${2:?}"; shift 2 ;;
    --no-service) NO_SERVICE=1; shift ;;
    --vllm-venv) VLLM_VENV="${2:?}"; shift 2 ;;
    --hf-home) HF_HOME_DIR="${2:?}"; shift 2 ;;
    --tailscale-ip) TS_IP="${2:?}"; shift 2 ;;
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

# ---- 1. which tier is this machine? -----------------------------------------
DETECT_JSON="$("$BIN" install detect --json)"
TIER="$(printf '%s' "$DETECT_JSON" | jq -r .verdict.profile)"
[ -n "$TIER" ] && [ "$TIER" != "null" ] || die "could not classify this machine"
say "tier:      $TIER  ($(printf '%s' "$DETECT_JSON" | jq -r .verdict.reason))"

# ram_tier gates BOTH the 26B placement (--cpu-moe puts every expert in RAM, so it is
# dropped without a real RAM path) and the RAM-gated media seed. This script passed
# neither, so both gates were inert on Linux: a tier served the 26B on a box that
# cannot host it, and the mid/high-only image seed never applied. An EMPTY value means
# "do not gate", so it is a hard failure here rather than a silent downgrade.
RAM_TIER="$(printf '%s' "$DETECT_JSON" | jq -r '.verdict.ram_tier // empty')"
[ -n "$RAM_TIER" ] || die "detect returned no ram_tier — refusing to install with both RAM gates inert"
say "ram_tier:  $RAM_TIER  ($(printf '%s' "$DETECT_JSON" | jq -r .facts.ram_gb) GB)"

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
[ -n "$LLAMA_BIN" ] || die "--llama-bin is required (the directory holding llama-server and its shared objects)"
[ -d "$LLAMA_BIN" ] || die "--llama-bin $LLAMA_BIN is not a directory"
[ -n "$MODELS" ] || MODELS="$PREFIX/models"

# ---- 3. lay out the tree ----------------------------------------------------
for d in "$PREFIX" "$PREFIX/etc" "$PREFIX/state" "$PREFIX/bin" "$MODELS"; do
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

# ---- 4. the harness config: one home key plus this tier's media bindings -----
# A seed failure is FATAL, never a silent '{}': an install that quietly ships no
# media bindings is exactly the drift this path exists to end. A tier that
# genuinely has none says so on stdout and is not an error.
if ! SEED="$("$BIN" install seed --profile "$TIER" --home "$PREFIX" --os linux --ram-tier "$RAM_TIER" \
        --vllm-venv "$VLLM_VENV" --hf-home "$HF_HOME_DIR")"; then
  die "could not resolve the media seed for tier $TIER"
fi
case "$SEED" in *"ships no media"*) SEED='{}'; say "media:     tier $TIER ships none — text only until bound by hand" ;; esac
CONFIG="$PREFIX/etc/config.json"
if [ -f "$CONFIG" ] && [ "$DRY_RUN" -eq 0 ]; then
  say "config:    $CONFIG exists — left untouched (delete it to regenerate)"
else
  BODY="$(printf '%s' "$SEED" | jq --arg home "$PREFIX" '. + {home: $home}')"
  if [ "$DRY_RUN" -eq 1 ]; then
    say "  would write $CONFIG:"; printf '%s\n' "$BODY" | sed 's/^/    /'
  else
    printf '%s\n' "$BODY" > "$CONFIG"
    say "config:    $CONFIG written (home + $(printf '%s' "$SEED" | jq -r 'keys|length') seeded key(s))"
  fi
fi

# ---- 5. the serving config --------------------------------------------------
SWAP_YAML="$PREFIX/etc/llama-swap.yaml"
if [ "$DRY_RUN" -eq 1 ]; then
  say "  would render $SWAP_YAML for tier $TIER"
else
  # --home is load-bearing: a tier's media seats name their binaries under the
  # install root, and without it the render REFUSES — after step 4 has already
  # written a config.json binding those seats' aliases.
  "$BIN" install render --profile "$TIER" --os linux --home "$PREFIX" \
    --ram-tier "$RAM_TIER" \
    --vllm-user "$SERVICE_USER" --vllm-proxy-host "$TS_IP" \
    --vllm-venv "$VLLM_VENV" --hf-home "$HF_HOME_DIR" \
    --llama-bin "$LLAMA_BIN" --models "$MODELS" --listen "$LISTEN" --out "$SWAP_YAML"
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
# Generated by setup/install.sh. Binds the TAILSCALE address only — the tailnet is
# the trust boundary and the fleet endpoints are unauthenticated by design, so never
# 0.0.0.0. Restart=on-failure covers the boot race where tailscaled has not yet
# assigned the address.
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
ExecStart=/bin/sh -c '$PREFIX/bin/local-offload fleet-serve --listen "\$(tailscale ip -4)":18811 --listen-trusted-network --node-id $NODE_ID'
Restart=on-failure

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
