#!/usr/bin/env bash
# Idempotent installer for the local-offload harness on an NVIDIA + Linux box.
# Builds a recent llama.cpp, pulls the Gemma-4 QAT family, configures a
# GRAMMAR-RELIABLE cascade (E2B -> E4B -> 26B-A4B MoE -> defer-to-Opus), builds
# the Go CLI+MCP, and wires serving. Safe to re-run. Override any env var below.
#
# IMPORTANT: this harness uses GBNF grammar-constrained decoding for structured
# JSON. MTP / speculative draft (--spec-type draft-mtp) is INCOMPATIBLE with the
# grammar field (llama.cpp returns a 500 "logits computation" error), so MTP is
# NOT used here. Flags below are the verified grammar-reliable set (-fa on, f16
# KV, --reasoning off). See reference/verified-config.md.
set -u

# ---- parameters (override via env) ----
: "${GPU_ARCH:=$(nvidia-smi --query-gpu=compute_cap --format=csv,noheader 2>/dev/null | head -1 | tr -d '.')}"
: "${MODELS_ROOT:=$HOME/models}"
: "${LLAMACPP_DIR:=$HOME/llama.cpp-offload}"
: "${HARNESS_SRC:=$HOME/local-offload}"            # local source dir (auto-cloned if missing)
: "${HARNESS_REPO_URL:=https://github.com/dmmdea/local-offload.git}"  # cloned into HARNESS_SRC if absent
: "${PORT:=18790}"                                 # dedicated-server port (if no llama-swap)
: "${CONFIG_OUT:=$HOME/.local-offload/config.json}"
: "${WITH_FAMILY:=1}"                              # 1 = also pull E2B (fast) + 26B-A4B (escalation); 0 = E4B only
: "${LLAMASWAP_CONFIG:=$HOME/llama-swap/config.yaml}"  # your live llama-swap config (override for your setup)
: "${LLAMASWAP_PORT:=11436}"                        # llama-swap front-end port the harness talks to

# Gemma-4 QAT family — Unsloth dynamic UD-Q4_K_XL GGUFs
E4B_REPO=unsloth/gemma-4-E4B-it-qat-GGUF;     E4B_FILE=gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf;     E4B_DIR=$MODELS_ROOT/gemma-4-E4B-it-qat
E2B_REPO=unsloth/gemma-4-E2B-it-qat-GGUF;     E2B_FILE=gemma-4-E2B-it-qat-UD-Q4_K_XL.gguf;     E2B_DIR=$MODELS_ROOT/gemma-4-E2B-it-qat
MOE_REPO=unsloth/gemma-4-26B-A4B-it-qat-GGUF; MOE_FILE=gemma-4-26B-A4B-it-qat-UD-Q4_K_XL.gguf; MOE_DIR=$MODELS_ROOT/gemma-4-26B-A4B-it-qat

fail(){ echo "ERROR: $*" >&2; exit 1; }
[ -n "$GPU_ARCH" ] || fail "could not detect GPU_ARCH; set it (e.g. 86 for Ampere)"
command -v go   >/dev/null 2>&1 || fail "Go 1.26+ required"
command -v git  >/dev/null 2>&1 || fail "git required"
command -v cmake >/dev/null 2>&1 || fail "cmake required"

# resolve CUDA
if [ -z "${CUDA_HOME:-}" ]; then
  if command -v nvcc >/dev/null 2>&1; then CUDA_HOME=$(dirname "$(dirname "$(command -v nvcc)")")
  elif [ -x /usr/local/cuda/bin/nvcc ]; then CUDA_HOME=/usr/local/cuda
  else CUDA_HOME=$(dirname "$(dirname "$(find "$HOME" /usr/local /opt -maxdepth 4 -name nvcc -type f 2>/dev/null | head -1)")")
  fi
fi
[ -x "$CUDA_HOME/bin/nvcc" ] || fail "nvcc not found; set CUDA_HOME (CUDA 12.x; avoid 13.x)"
echo ">> GPU_ARCH=$GPU_ARCH  CUDA_HOME=$CUDA_HOME"
export PATH="$CUDA_HOME/bin:$PATH"
export LD_LIBRARY_PATH="$CUDA_HOME/lib64:${LD_LIBRARY_PATH:-}"

# ---- 1. build a recent llama.cpp ----
if [ ! -x "$LLAMACPP_DIR/build/bin/llama-server" ]; then
  echo ">> building llama.cpp into $LLAMACPP_DIR"
  [ -d "$LLAMACPP_DIR/.git" ] || git clone --depth 1 https://github.com/ggml-org/llama.cpp "$LLAMACPP_DIR" || fail "clone llama.cpp"
  cmake -S "$LLAMACPP_DIR" -B "$LLAMACPP_DIR/build" -DGGML_CUDA=ON -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_CUDA_COMPILER="$CUDA_HOME/bin/nvcc" -DCUDAToolkit_ROOT="$CUDA_HOME" -DCMAKE_CUDA_ARCHITECTURES="$GPU_ARCH" || fail "cmake configure"
  cmake --build "$LLAMACPP_DIR/build" --config Release -j --target llama-server || fail "build llama-server"
else
  echo ">> llama.cpp already built; skipping"
fi
# sanity: needs the --reasoning toggle (Gemma-4 thinking mode must be disabled, else
# short-budget grammar replies are eaten by the think phase) and grammar support.
"$LLAMACPP_DIR/build/bin/llama-server" --help 2>&1 | grep -q -- '--reasoning' \
  || fail "this llama.cpp is too old (no --reasoning); update it (2026-06+)"
LS_BIN="$LLAMACPP_DIR/build/bin/llama-server"

# ---- 2. download the family ----
mkdir -p "$MODELS_ROOT"
HF=$(command -v hf || command -v huggingface-cli) || fail "hf / huggingface-cli required (pip install huggingface_hub)"
dl(){ local repo=$1 file=$2 dir=$3; mkdir -p "$dir"; if [ ! -f "$dir/$file" ]; then echo ">> downloading $file"; "$HF" download "$repo" "$file" --local-dir "$dir" || fail "download $file"; fi; }
dl "$E4B_REPO" "$E4B_FILE" "$E4B_DIR"                 # workhorse (mandatory)
if [ "$WITH_FAMILY" = 1 ]; then
  dl "$E2B_REPO" "$E2B_FILE" "$E2B_DIR"               # fast triage tier
  dl "$MOE_REPO" "$MOE_FILE" "$MOE_DIR"               # near-frontier escalation tier (MoE, experts in RAM)
fi

# ---- 3. build the harness ----
if [ ! -d "$HARNESS_SRC" ]; then
  [ -n "$HARNESS_REPO_URL" ] || fail "HARNESS_SRC ($HARNESS_SRC) missing and HARNESS_REPO_URL not set"
  git clone "$HARNESS_REPO_URL" "$HARNESS_SRC" || fail "clone harness"
fi
( cd "$HARNESS_SRC" && go build -o local-offload . ) || fail "go build harness"
BIN="$HARNESS_SRC/local-offload"
echo ">> built $BIN"

# ---- 4. verified grammar-reliable per-tier flags (NO MTP) ----
COMMON="--ctx-size 8192 --flash-attn on --cache-type-k f16 --cache-type-v f16 --threads 8 --jinja --reasoning off"
E4B_FLAGS="--model $E4B_DIR/$E4B_FILE --n-gpu-layers 99 --parallel 1 $COMMON"   # ~70-83 tok/s, 8GB
E2B_FLAGS="--model $E2B_DIR/$E2B_FILE --n-gpu-layers 99 $COMMON"                # ~120-131 tok/s, ~3.4GB
MOE_FLAGS="--model $MOE_DIR/$MOE_FILE --cpu-moe --n-gpu-layers 999 --parallel 1 $COMMON" # ~16 tok/s, ~2.85GB GPU (needs GGML_CUDA_DISABLE_GRAPHS=1)

# ---- 5. configure serving ----
ENDPOINT="http://127.0.0.1:$LLAMASWAP_PORT"; MODEL_NAME="offload-e4b"; TRIAGE="gemma4-e2b"; ESCAL="gemma4-26b-a4b"
SNIPPET="$HARNESS_SRC/offload-family.llama-swap.yaml"
cat > "$SNIPPET" <<EOF
# Gemma-4 QAT family for the local-offload cascade. Merge the entries under your
# llama-swap 'models:' map and add the group under 'groups:'. All tiers are
# swap-EXCLUSIVE (one large model on the GPU at a time on 8GB). NO MTP (breaks
# GBNF grammar). Generated by local-offload-setup.
models:
  offload-e4b:
    env: ["LD_LIBRARY_PATH=$CUDA_HOME/lib64:\${LD_LIBRARY_PATH:-}"]
    cmd: $LS_BIN $E4B_FLAGS --port \${PORT} --host 127.0.0.1
    checkEndpoint: /health
    ttl: 300
    aliases: ["offload", "gemma4-e4b-qat", "gemma4-e4b"]
  gemma4-e2b:
    env: ["LD_LIBRARY_PATH=$CUDA_HOME/lib64:\${LD_LIBRARY_PATH:-}"]
    cmd: $LS_BIN $E2B_FLAGS --port \${PORT} --host 127.0.0.1
    checkEndpoint: /health
    ttl: 300
    aliases: ["e2b", "gemma4-e2b-qat"]
  gemma4-26b-a4b:
    env: ["LD_LIBRARY_PATH=$CUDA_HOME/lib64:\${LD_LIBRARY_PATH:-}", "GGML_CUDA_DISABLE_GRAPHS=1"]
    cmd: $LS_BIN $MOE_FLAGS --port \${PORT} --host 127.0.0.1
    checkEndpoint: /health
    ttl: 300
    aliases: ["26b-a4b", "moe", "quality-moe"]
groups:
  offload_family:
    swap: true
    exclusive: true
    members: ["offload-e4b", "gemma4-e2b", "gemma4-26b-a4b"]
EOF
echo ">> wrote llama-swap snippet: $SNIPPET"

if curl -sf --max-time 2 "localhost:$LLAMASWAP_PORT/v1/models" >/dev/null 2>&1 || [ -f "$LLAMASWAP_CONFIG" ]; then
  echo ">> llama-swap detected at $LLAMASWAP_CONFIG"
  echo "   Merge $SNIPPET into it (back up first), then restart llama-swap and verify /health."
  echo "   The cascade (E2B->E4B->26B-A4B) needs all three entries served by llama-swap."
  if [ "$WITH_FAMILY" != 1 ]; then TRIAGE=""; ESCAL=""; fi
else
  echo ">> no llama-swap — writing a dedicated E4B launcher (single workhorse, NO cascade) on port $PORT"
  cat > "$HARNESS_SRC/serve-offload.sh" <<EOF
#!/usr/bin/env bash
export LD_LIBRARY_PATH=$CUDA_HOME/lib64:\${LD_LIBRARY_PATH:-}
exec $LS_BIN $E4B_FLAGS --port $PORT --host 0.0.0.0
EOF
  chmod +x "$HARNESS_SRC/serve-offload.sh"
  ENDPOINT="http://127.0.0.1:$PORT"; MODEL_NAME=""; TRIAGE=""; ESCAL=""   # single model -> no tier routing
  echo "   start: $HARNESS_SRC/serve-offload.sh  (install llama-swap to enable the E2B/26B tiers)"
fi

# ---- 6. write harness config (with cascade routing) ----
mkdir -p "$(dirname "$CONFIG_OUT")"
cat > "$CONFIG_OUT" <<EOF
{
  "endpoint": "$ENDPOINT",
  "completion_path": "/v1/chat/completions",
  "model": "$MODEL_NAME",
  "triage_model": "$TRIAGE",
  "escalation_model": "$ESCAL",
  "temperature": 0,
  "max_retries": 1,
  "classify_min_confidence": 0.45,
  "confidence_margin_threshold": 0.35,
  "cache_path": "$HOME/.local-offload/cache.db",
  "ledger_path": "$HOME/.local-offload/ledger.jsonl",
  "request_timeout_sec": 120
}
EOF
echo ">> wrote config $CONFIG_OUT"

# ---- 7. report ----
echo ""
echo "=== DONE. Next steps ==="
echo "1. Serve the models (merge $SNIPPET into llama-swap, or run serve-offload.sh)."
echo "2. Smoke test:  echo 'Acme shipped a robot for \$4999.' | $BIN summarize - --json"
echo "   (set \$LOCAL_OFFLOAD_CONFIG=$CONFIG_OUT, or the MCP uses built-in defaults)"
echo "3. Inspect routing:  $BIN models      (shows the cascade tiers)"
echo "4. Register MCP (then restart Claude Code):"
echo "   claude mcp add local-offload --scope user -- \"$BIN\" mcp"
echo "   (add  --config \"$CONFIG_OUT\"  only if you need non-default endpoints/paths)"
echo "5. Token savings over time:  $BIN ledger"
