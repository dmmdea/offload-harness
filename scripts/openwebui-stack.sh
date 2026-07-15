#!/usr/bin/env bash
# Bring up the local-model coding-agent GUI stack (idempotent):
#   1) local-agent OpenAI-compatible server on :18800  (create/edit/upload tools)
#   2) OpenWebUI on :8081, pre-pointed at the agent server (log in with the account you created on first launch)
# The agent works inside ~/local-agent-workspace. Chat in OpenWebUI -> it builds.
#
# Tuning (learned live, 2026-07-08): qwythos has a 32K context window and the
# loop resends the FULL growing transcript every step, so a wide/exploratory
# web_search-heavy prompt can exceed it before finishing. Defaults below are the
# verified-working configuration: a lean toolset (no shell/delete — they add
# tool-schema overhead and aren't needed for search/edit/upload demos), a small
# search-result cap, a low same-tool-name cap so a stuck model gets its tool
# disabled (not just refused) quickly, and a modest step budget. For anything
# beyond a narrow demo prompt, prefer "edit an existing file, then upload it"
# over "search the web, build, upload" — the search leg is what blows context on
# broad topics; edit+upload alone completes in ~25s reliably.
set -u
WS="${LOCAL_AGENT_WORKSPACE:-$HOME/local-agent-workspace}"
BIN="${LOCAL_AGENT_BIN:-/mnt/d/repos/local-offload/bin/local-agent-linux}"
MODEL="${LOCAL_AGENT_MODEL:-qwythos}"
CAPS="${LOCAL_AGENT_CAPS:--allow-write -allow-overwrite -allow-search -allow-github}"
MAX_STEPS="${LOCAL_AGENT_MAX_STEPS:-12}"
MAX_SAME_TOOL="${LOCAL_AGENT_MAX_SAME_TOOL:-2}"
mkdir -p "$WS"

if ! curl -sf http://127.0.0.1:18800/v1/models >/dev/null 2>&1; then
  echo "starting agent server on :18800 (model=$MODEL, workspace=$WS)"
  # GitHub creds live in a gitignored env file so the token never enters the repo.
  # Contents: export GITHUB_TOKEN=ghp_...   and   export GITHUB_REPO=owner/name
  [ -f "$HOME/.local-agent-github.env" ] && . "$HOME/.local-agent-github.env"
  nohup "$BIN" -serve -listen 127.0.0.1:18800 \
    $CAPS \
    -base http://127.0.0.1:11436 -model "$MODEL" -max-tokens 4096 -max-same-tool "$MAX_SAME_TOOL" \
    -root "$WS" -worktree "$WS" -max-steps "$MAX_STEPS" -timeout 180 \
    > /tmp/agent-server.log 2>&1 &
fi

if ! curl -sf http://127.0.0.1:8081/health >/dev/null 2>&1; then
  echo "starting OpenWebUI on :8081"
  # Auth ON (create your account on first launch). Do NOT set WEBUI_AUTH=false —
  # auth-off + an existing account makes the frontend hang on "Signing in".
  # DATA_DIR is OUTSIDE site-packages so `pip -U open-webui` upgrades can't wipe the DB.
  export DATA_DIR="$HOME/.open-webui" \
         ENABLE_OPENAI_API=true \
         OPENAI_API_BASE_URLS=http://127.0.0.1:18800/v1 OPENAI_API_KEYS=local \
         ENABLE_OLLAMA_API=false
  nohup ~/.local/bin/open-webui serve --port 8081 > /tmp/openwebui.log 2>&1 &
fi

for i in $(seq 1 60); do
  sleep 2
  if curl -sf http://127.0.0.1:8081/health >/dev/null 2>&1 \
     && curl -sf http://127.0.0.1:18800/v1/models >/dev/null 2>&1; then
    echo "stack UP — open http://localhost:8081 and pick the $MODEL model"
    exit 0
  fi
done
echo "stack did not confirm ready in time; check /tmp/openwebui.log and /tmp/agent-server.log" >&2
exit 1
