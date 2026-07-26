# `ampere-6` — the 6 GB Ampere tier, measured

**Status:** measured — all four hypotheses settled on real hardware; profile updated
**Date:** 2026-07-26
**Decides:** what `ampere-6` should actually serve, on the first real hardware ever to run it.

---

## Why this document exists

`setup/templates/profiles.json` has carried an `ampere-6` entry since the 12-config matrix was
written. Every value in it is `PROJECTED` — reasoned from specs, never run. Its own note concedes
the largest unknown out loud:

> "E2B resident. q8_0 KV is MANDATORY to reach 16K on 6GB. 26B dropped. PROJECTED — H3 confirms the
> 16K ceiling; may downshift to 8K."

A box matching this profile is now available and is a permanent ecosystem node rather than a
borrowed laptop, so the tier can be measured properly and re-measured whenever it changes.

**This tier is its own thing.** Its nearest sibling is `ampere-8` — same Ampere generation, same
CUDA path, same tensor cores, same grammar-constrained serving story. It is *not* related to the
`amd-gcn` findings in [2026-07-17-hardware-scope-linux-and-low-end-design.md](2026-07-17-hardware-scope-linux-and-low-end-design.md);
that document measured a Vega iGPU whose `llama-bench` reports `matrix cores: none`, and its
latency conclusions are a property of that silicon. Nothing there transfers here and this spec does
not assume it does.

---

## 1. Hardware truth (all measured on the box, 2026-07-26)

| Property | Measured | How |
|---|---|---|
| Chassis | Lenovo ThinkCentre M720q (10T8S28P00) | `dmidecode -t system` |
| CPU | **Intel i9-9900**, 8C/16T, 3.1 GHz base / 5.0 GHz max, 16 MiB L3, 1 NUMA node | `lscpu` |
| CPU ISA | AVX, **AVX2**, F16C, FMA — **no AVX-512, no VNNI, no AMX** | `/proc/cpuinfo` |
| RAM | **2 × 16 GB DDR4-2667, dual channel** (both channels populated), 42.7 GB/s theoretical | `dmidecode -t memory` |
| RAM measured | **60.2 GB/s read / 28.2 GB/s write** (sysbench, 1 MiB blocks, 8 threads) | `sysbench memory` |
| GPU | **RTX 3050 6 GB** (GA107), compute capability **8.6** | `nvidia-smi` |
| GPU power | **70 W limit — and 70 W is also `power.max_limit`**, so it cannot be raised | `nvidia-smi -q` |
| GPU clocks | max SM 2100 MHz, max mem 7001 MHz; idles at 210 MHz / 6.6 W | `nvidia-smi` |
| VRAM free | **5424 MiB of 6144** — the desktop compositor holds only 380 MiB, no compute apps | `nvidia-smi` |
| PCIe | **LnkCap gen3 (8 GT/s) × 16** — idle reading of "gen1 × 8 (downgraded)" is ASPM downtrain | `lspci -vv` |
| Storage | Samsung PM9C1a 512 GB NVMe, **DRAM-less**, 2035 MB/s O_DIRECT read | `hdparm -t --direct` |
| Model store | ZFS dataset `ecosystem_backup/apps/offload-stack`, 80 GB quota | `zfs list` |
| RAM band | **`low`** (31 GB; bands are ≥120 high, ≥56 mid, ≥28 low, else min) | `detect.ps1: Get-RamTier` |

### What that hardware implies before any model is loaded

- **VRAM, not compute, is the binding constraint.** 5.4 GB is genuinely free because the desktop
  runs on the compositor's minimal allocation — this box does not spend its VRAM on a browser.
- **Token generation is DDR4-bandwidth-bound** for anything with weights in RAM. The 60 GB/s read
  figure is cache-inflated by the 1 MiB block size; the 28 GB/s write number is the more honest
  proxy for streaming behaviour, and the theoretical ceiling is 42.7 GB/s.
- **CPU-side inference is AVX2-only.** No VNNI and no AMX means int8 paths get no dedicated
  instruction help — relevant to any CPU-offload tier, and a real difference from newer chips.
- **The 70 W cap is hard.** `power.limit == power.max_limit`, so there is no headroom to unlock;
  sustained clocks, not peak clocks, are what this tier gets.
- **PCIe is not a bottleneck.** Gen3 ×16 ≈ 15.75 GB/s is ample for streaming MoE experts from RAM.

### Position relative to its nearest sibling

| | `ampere-8` (reference box) | `ampere-6` (this box) |
|---|---|---|
| GPU | RTX 3070 8 GB, laptop | RTX 3050 6 GB, desktop, 70 W hard cap |
| Usable VRAM | ~8 GB | **5.4 GB** |
| System RAM | 64 GB → band `mid` | **32 GB → band `low`** |
| CPU | laptop-class | **desktop i9-9900, 8C/16T** |
| Resident tier | `offload-e4b` | `gemma4-e2b` *(projected)* |
| 26B-A4B | `cpu_moe` (allowed on mid/high RAM) | `drop` |

The interesting asymmetry: this box has **less VRAM but a stronger CPU** than its sibling, and its
GPU is not also driving a laptop display. Two of the profile's projections lean on the VRAM
disadvantage while ignoring the CPU advantage — which is what the hypotheses below test.

---

## 2. Pre-registered hypotheses

Recorded **before** measuring, so the results cannot be retrofitted to whatever happens.

### H1 — E4B can be the resident tier, not E2B

The profile assigns `resident_tier: gemma4-e2b`. E4B is 3.93 GiB against 5.4 GiB free, leaving
~1.4 GiB for KV and overhead. Gemma-4 uses sliding-window attention, which keeps the KV cache
modest at moderate context.

**Prediction:** E4B loads and serves at 16K with q8_0 KV, with headroom left. If it does, the tier
gets a substantially stronger workhorse than projected at no cost.
**Falsified by:** OOM on load, or an allocation that leaves no room for the KV cache.

### H2 — 16K context holds; no downshift to 8K

The profile flags this as its own open question.

Gemma-4's architecture argues in favour: it interleaves local sliding-window attention with periodic
global attention, and **E2B and E4B use a 512-token sliding window** (the 26B/31B use 1024). Only
the global layers carry full-context KV, and those use unified keys/values. So KV growth with
context is far flatter than a dense model's, which is exactly why q8_0 KV plus 16K is plausible on
6 GB at all.

**Prediction:** 16K holds for E2B comfortably. E4B at 16K is the tight case and is the real test.
**Falsified by:** OOM at 16K, or a KV allocation that forces `--ctx-size 8192`.

### H3 — a 26B-A4B escalation tier is viable despite `ram_tier: low`

This is the one that matters most, and the one the current profile forecloses. `include_26b: false`
means `escalation_model` and `reasoning_model` are unset, so the cascade partially collapses — the
escalation path falls back to the workhorse, and the confidence gate escalates a model to itself.

The 26B-A4B is 13.27 GiB against ~20 GiB available RAM, with only ~2.85 GiB landing on the GPU
under all-experts-on-CPU. A4B activates ~4 B parameters per token; at Q4 that is roughly 2.2–2.5 GB
read per token, so 28–42 GB/s of usable bandwidth puts the ceiling near **11–17 tok/s**, before
AVX2-only overhead.

**The method is a sweep, not a binary.** Current llama.cpp exposes `--n-cpu-moe N` (how many expert
layers to place on CPU, counting from the highest-numbered layer) rather than only the all-or-nothing
`--cpu-moe`. With ~2.85 GiB used by the non-expert weights there is ~2.5 GiB of VRAM spare on this
card, which can host several expert layers. Community tuning guidance is explicit that the right
value is found by walking it: raise `--n-cpu-moe` in small steps and watch for the point where
throughput rises, flattens, then reverses — copying one number blindly is the documented mistake.
This matters more than it looks, because MoE routing is heavily skewed: roughly 15–20% of experts
serve ~80% of tokens, so the first few layers kept resident buy disproportionate speed.

**Prediction:** it loads and runs at roughly **8–14 tok/s** with all experts on CPU, and measurably
better at the sweep's optimum — slow, but usable as an escalation tier that only fires on
low-confidence calls, and far better than having no escalation at all.
**Falsified by:** OOM, thrashing against the box's resident services, or throughput low enough that
escalation costs more than deferring.
**Caveat recorded up front:** this box runs 24/7 services. Even if the 26B fits, holding 13 GiB for
it may be the wrong trade — a viability result is not automatically an adoption decision.

### H4 — the 70 W cap, not VRAM, limits throughput once a model fits

**Prediction:** sustained SM clock under load sits well below the 2100 MHz maximum, and GPU-resident
throughput scales with the power cap rather than with occupancy.

---

## 3. Measurement plan

Each measurement names what it proves and what it does not.

1. **`llama-bench` pp512/tg128** for E2B and E4B, `-ngl 99`, ≥2 repetitions. Proves raw throughput;
   proves nothing about grammar reliability or deep-context stability.
2. **VRAM occupancy at load** for each candidate resident tier at 8K and 16K, q8_0 KV. Settles H1
   and H2.
3. **Sustained clock and power under load** sampled during the bench. Settles H4.
4. **26B-A4B under `--cpu-moe`** with `GGML_CUDA_DISABLE_GRAPHS=1`: does it load, what lands on the
   GPU, what is tg128, and what happens to the box's resident services. Settles H3.
5. **End-to-end harness latency** — `classify` / `triage` / `summarize` across small, mid and
   `max_input_chars` inputs, cold cache, through the real CLI. This is the number that decides
   whether the tier is interactive or batch.
6. **Grammar reliability** — structured JSON via GBNF across repeated calls, since every harness
   task depends on it and `llama-bench` says nothing about it.

## 4. Results

llama.cpp built from source for `sm_86` with CUDA 12.4. All figures on the working box (24/7
services resident), `-ngl 99`, flash-attn on.

### 4.1 Raw throughput (`llama-bench -p 512 -n 128 -r 2`)

| Model | pp512 (t/s) | tg128 (t/s) |
|---|---:|---:|
| gemma-4-E2B QAT (2.43 GiB) | **2631.32 ± 417.38** | **90.92 ± 0.32** |
| gemma-4-E4B QAT (3.91 GiB) | **1528.54 ± 123.37** | **48.12 ± 0.07** |
| gemma-4-26B-A4B QAT (13.26 GiB), `--n-cpu-moe 24` | 210.27 | 13.47 |

For scale: the `amd-gcn` box measured 213 t/s pp and 14.9 t/s tg on the same E2B model. This tier is
**12× the prompt processing and 6× the token generation** of that one. The two are not comparable
hardware classes and must not share conclusions.

### 4.2 VRAM occupancy — H1 and H2

| Model | ctx | KV | VRAM used (of 6144 MiB) |
|---|---:|---|---:|
| E2B | 16384 | q8_0 | 1686 |
| E2B | 32768 | q8_0 | 1752 |
| E2B | 65536 | f16 | 2078 |
| E4B | 8192 | q8_0 | 2992 |
| **E4B** | **16384** | **q8_0** | **3100** |
| E4B | 16384 | f16 | 3230 |
| E4B | 32768 | q8_0 | 3316 |
| E4B | 32768 | f16 | 3502 |
| E4B | 65536 | q8_0 | 3748 |
| E4B | 65536 | f16 | **4046** |

**H1 — CONFIRMED, decisively.** E4B at 16K/q8_0 needs 3100 MiB of 6144. The projected `E2B
resident` left more than half the card idle.

**H2 — CONFIRMED and exceeded.** 16K is not a ceiling; **E4B reaches 64K with full f16 KV** in
4046 MiB. The profile's "may downshift to 8K" worry was unfounded by a factor of eight.

**The profile's "q8_0 KV is MANDATORY" claim is FALSE.** f16 at 16K costs 130 MiB more than q8_0
(3230 vs 3100). Gemma-4's 512-token sliding window means KV barely grows with context, so the
quality-reducing quantisation buys almost nothing on this architecture. q8_0 is retained in the
profile only because the 26B tier wants the headroom.

### 4.3 The 26B escalation tier — H3

`--n-cpu-moe N` sweep (`GGML_CUDA_DISABLE_GRAPHS=1`):

| N | pp512 | tg128 | outcome |
|---:|---:|---:|---|
| 48 (all experts on CPU) | 188.81 | 11.44 | loads |
| 40 | 188.20 | 11.01 | loads |
| 32 | 185.35 | 10.93 | loads |
| **24** | **210.27** | **13.47** | loads |
| 22 | 243.10 | 14.34 | benches, but **fails to serve** |
| 20 | — | — | context creation fails |
| 18, 16 | — | — | model load fails |

Serving check (the one that decides the profile value):

| N | ctx | result |
|---:|---:|---|
| 22 | 16384 | **FAILED** — `ggml_backend_cuda_buffer_type_alloc_buffer` |
| **24** | **16384** | serves, 5134 MiB VRAM, ~21 GB RAM free |
| **24** | **32768** | serves, 5320 MiB VRAM |

**H3 — CONFIRMED.** The 26B runs as a real escalation tier on a 32 GB box at **13.5 tok/s**, inside
the predicted 8–14 band. The sweep matters: N=24 is 18% faster at generation than all-experts-on-CPU
(13.47 vs 11.44) and 11% faster at prompt processing.

**N=22 is a trap.** It benches fastest but cannot allocate at a serving context — `llama-bench`
uses a small window, so bench-optimal ≠ serve-safe. **24 is the value; do not raise it.**

### 4.4 The power cap — H4

Sampled during the benches: **mean SM 1639 MHz against a 2100 MHz maximum, mean 58.1 W / peak
67.0 W against a 70 W cap that `power.max_limit` shows cannot be raised.**

**H4 — CONFIRMED.** Once a model fits, this tier is power-limited, not VRAM-limited. That reframes
optimisation here: there is no headroom to unlock with clocks, so the wins come from fitting a
better model (H1) and a longer context (H2) into VRAM that was sitting idle.

### 4.5 Incidental finding — ZFS ARC starves the escalation tier

Writing 20 GB of model weights grew the ARC to **19.89 GB of 31 GB** (its `c_max` defaulted to
30.21 GB, essentially all of RAM). Linux does not count ARC in `MemAvailable`, so the box reported
**5.3 GB available** and the 26B had nowhere to live.

Capping `zfs_arc_max` to 8 GiB returned available RAM to **17 GB immediately** (ARC 19.89 → 7.98 GB).
**Any ZFS-backed node in this tier must cap the ARC**, or the escalation tier silently becomes
unreachable while `free` appears to explain nothing.

### 4.6 What the profile becomes

| Field | Was (PROJECTED) | Now (MEASURED) |
|---|---|---|
| `resident_tier` | `gemma4-e2b` | **`offload-e4b`** |
| `ctx_size` | 16384 | **32768** |
| `include_26b` | `false` | **`true`** |
| `moe_26b` | `drop` | **`n_cpu_moe`** |
| `n_cpu_moe` | — | **24** |

`n_cpu_moe` is a new mode added to `install.ps1`: `--cpu-moe` puts *every* expert in RAM and is
correctly gated to ≥56 GB boxes, which is why the 26B was dropped here. Partial offload has a
fraction of that appetite, so it is gated only against `min` (<28 GB). Covered by new render tests
for both the `min` and `low` bands.

## 5. Threats to validity

- One box, one GPU, one llama.cpp build. Results bind `ampere-6` on **this** silicon.
- The box runs 24/7 services (Immich, a game server, ev-diagnostics), so measurements are taken on
  a working machine, not a bench. Where a number looks contended it gets re-run against a quieted
  box and both figures are reported.
- `sysbench` memory figures overstate real streaming bandwidth at 1 MiB blocks; they are a relative
  indicator, not a STREAM result.
- Nothing here measures the media/image path, which this tier has no `config_seed` for.

## 6. Reproducing this

```bash
# hardware truth
lscpu; sudo dmidecode -t memory; nvidia-smi -q
sudo lspci -vv -s 01:00.0 | grep -E 'LnkCap:|LnkSta:'
sysbench memory --memory-block-size=1M --memory-total-size=16G --memory-oper=read --threads=8 run

# the stack (models + build live on the ZFS dataset, not root)
GPU_ARCH=86 WITH_FAMILY=1 \
MODELS_ROOT=/srv/ecosystem_backup/apps/offload-stack/models \
LLAMACPP_DIR=/srv/ecosystem_backup/apps/offload-stack/build/llamacpp \
  ./setup.sh
```
