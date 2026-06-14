#!/usr/bin/env bash
# Detect hardware + toolchain for the local-offload harness. Read-only.
set -u
echo "=== GPU ==="
if command -v nvidia-smi >/dev/null 2>&1; then
  nvidia-smi --query-gpu=name,memory.total --format=csv,noheader
  CC=$(nvidia-smi --query-gpu=compute_cap --format=csv,noheader 2>/dev/null | head -1 | tr -d '.')
  echo "GPU_ARCH (use as CMAKE_CUDA_ARCHITECTURES) = ${CC:-UNKNOWN}"
else
  echo "MISSING: nvidia-smi — an NVIDIA GPU is required"
fi
echo "=== RAM ==="
free -g 2>/dev/null | awk 'NR==2{print $2" GB total / "$7" GB available"}' || echo "(free unavailable)"
echo "=== CUDA (need 12.x; AVOID 13.x) ==="
if command -v nvcc >/dev/null 2>&1; then nvcc --version | tail -1
elif [ -x /usr/local/cuda/bin/nvcc ]; then echo "CUDA_HOME=/usr/local/cuda"; /usr/local/cuda/bin/nvcc --version | tail -1
else
  found=$(find "$HOME" /usr/local /opt -maxdepth 4 -name nvcc -type f 2>/dev/null | head -1)
  [ -n "$found" ] && echo "nvcc found at $found (set CUDA_HOME to its parent's parent)" || echo "MISSING: nvcc — install CUDA 12.4-12.8"
fi
echo "=== Go (need 1.26+) ==="
command -v go >/dev/null 2>&1 && go version || echo "MISSING: Go"
echo "=== tools ==="
for t in git cmake python3; do command -v "$t" >/dev/null 2>&1 && echo "$t: ok" || echo "$t: MISSING"; done
if command -v hf >/dev/null 2>&1; then echo "hf: ok"
elif command -v huggingface-cli >/dev/null 2>&1; then echo "huggingface-cli: ok"
else echo "hf/huggingface-cli: MISSING (pip install huggingface_hub)"; fi
echo "=== disk (\$HOME) ==="
df -h "$HOME" | tail -1
echo "=== llama-swap present? ==="
(curl -sf --max-time 2 localhost:11436/v1/models >/dev/null 2>&1 && echo "yes (:11436 responds)") || echo "no (will use a dedicated server)"
