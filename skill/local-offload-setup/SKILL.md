---
name: local-offload-setup
description: >-
  Use when setting up the "local-offload" harness on a Windows machine — a free local Gemma-4
  cascade that lets a coding agent delegate short-context grunt work (summarize / classify /
  extract / triage) so those tokens never hit the cloud context. Cross-vendor: NVIDIA (CUDA, ≥8GB),
  AMD Radeon incl. RDNA3 iGPUs like the 780M/gfx1103 (Vulkan, native Windows), or CPU-only fallback.
  Builds a pinned llama.cpp + llama-swap stack, pulls the Gemma-4 QAT family (E2B/E4B/26B-A4B) +
  EmbeddingGemma, builds the Go CLI/MCP + local coding agent, and registers it with Claude Code.
  Triggers: "set up local offload", "install the local-offload harness", "offload model setup",
  "give Claude a free local model", "install the AMD/NVIDIA offload stack".
---

# local-offload-setup

This skill is a **thin wrapper**. The canonical, always-current procedure lives in the repo at
**`setup/SETUP-AGENT.md`** — follow that, not a copy here, so the skill can never drift from the
scripts it drives.

## Hardware gate

| Detected | Backend | Notes |
|---|---|---|
| NVIDIA GPU (~8 GB VRAM) | `cuda` | First-class. `nvidia-smi` or CIM name match selects it. |
| AMD Radeon, incl. RDNA3 iGPU (780M / gfx1103) | `vulkan` | **Native Windows Vulkan.** ROCm/HIP and WSL2-for-iGPU are research-verified dead ends — do NOT attempt. |
| No GPU | `cpu` | Fallback. Slow (`<8 t/s`); 26B tier needs ≥48 GB RAM. |
| RAM < 32 GB | (any) | Install E4B workhorse only: `OFFLOAD_WITH_FAMILY=0`. |
| Free disk < 25 GB | — | Hard blocker; `detect.ps1` exits 1. |

## Procedure

Run the three scripts in order, reading each one's final JSON line, and follow
`setup/SETUP-AGENT.md` for the decision tree at every branch:

```powershell
pwsh -NoProfile -File setup\detect.ps1      # -> backend + blockers
pwsh -NoProfile -File setup\install.ps1     # -> pinned binaries, models, Go build, rendered yaml
pwsh -NoProfile -File setup\selftest.ps1    # -> receipt JSON: verdict pass|warn|fail
```

Then start the stack and register the MCP (Step 3 of `setup/SETUP-AGENT.md`):

```powershell
& "$env:OFFLOAD_HOME\llama-swap\llama-swap.exe" --config "$env:OFFLOAD_HOME\llama-swap.yaml" --listen 127.0.0.1:11436
claude mcp add local-offload -- "$env:OFFLOAD_HOME\harness\local-offload.exe" mcp
```

## Do not

- Do NOT install CUDA or ROCm — the pinned llama.cpp release binaries carry their own runtime;
  Vulkan uses the GPU driver.
- Do NOT substitute a different model, build, or "latest" asset if a pin 404s or a hash mismatches —
  stop and surface it.
- Do NOT update a GPU driver, reboot, or start the unauthenticated `local-agent --serve` endpoint
  beyond loopback without asking the human first.

For operation after install (chat, offload tasks, driving the coding agent, diagnosis), see
`CLAUDE.md` and `docs/OPERATOR-GUIDE.md` in the repo.
