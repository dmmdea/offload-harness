# Hardware scope: Linux and the low end

**Status:** proposal — for decision
**Date:** 2026-07-17
**Author:** afelopez
**Decides:** which hardware and which OS `offload-harness` claims to support, and what the harness
should do when the cascade cannot fit on the box.

---

## Why this document exists

The harness advertises three backends (CUDA / Vulkan / CPU) and three operating systems
(Linux / macOS / Windows). Every profile below the NVIDIA line is marked `PROJECTED` in
`setup/templates/profiles.json` — that is, reasoned from specs and never run on the hardware.

This document reports the **first measurements ever taken on config #12 (`amd-gcn`)**, on a machine
that matches that profile's hardware class, and lays out the decisions those numbers force. It is
written to be argued with: every claim is either measured (with the command to reproduce it) or
labelled as a projection.

**The short version.** The harness *works* on this class of machine — it is not broken, and quality
was not the problem. But the cascade collapses to a single tier, and a triage call takes **39
seconds** where the cloud model it is protecting would take about two. The open question is not
"does it run" but **"what is the harness for, when the tier it escalates to cannot fit?"**

---

## 1. The machine under test

A Lenovo laptop running CachyOS (Linux 6.18), which lands squarely in the `amd-gcn` profile:

| Property | Measured value | How |
|---|---|---|
| CPU | AMD Ryzen 5 5500U, 6C/12T | `lscpu` |
| GPU | Radeon Lucienne (Vega/GCN, gfx90c-class) | `lspci` |
| GPU as llama.cpp sees it | `AMD Radeon Graphics (RADV RENOIR)`, `uma: 1`, `fp16: 1`, **`matrix cores: none`** | `llama-bench --list-devices` |
| Dedicated VRAM | **2.0 GiB** carve-out | `/sys/class/drm/card*/device/mem_info_vram_total` |
| GTT (shared, GPU-addressable) | **10.7 GiB** | `mem_info_gtt_total` |
| Total addressable by GPU | 12,974 MiB — but only **6,672 MiB free** under normal desktop load | `llama-bench --list-devices` |
| System RAM | **21 GiB** total, ~9 GiB available | `free -h` |
| Memory | DDR4, dual channel (bandwidth-bound; see §3) | — |

Against the roster this profile would have to serve:

| Model | Size on disk |
|---|---|
| `gemma-4-E2B-it-qat-UD-Q4_K_XL.gguf` | 2.44 GiB |
| `gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf` | 3.93 GiB |
| `gemma-4-26B-A4B-it-qat-UD-Q4_K_XL.gguf` | **13.27 GiB** |

The 26B tier does not fit in the ~9 GiB of available RAM, let alone alongside a desktop. This is
not a surprise — `profiles.json` already anticipated it (§2).

---

## 2. What the repo already says about this machine

`setup/templates/profiles.json` → `amd-gcn` (config #12):

```json
{
  "ctx_size": 8192, "kv_type": "f16", "resident_tier": "gemma4-e2b",
  "include_26b": false, "moe_26b": "drop", "flash_attn": "off", "backend": "vulkan",
  "notes": "Config #12 (Vega 7/GCN + 32GB, Vulkan). Weakest path: E2B, 8K f16,
            flash-attn OFF (older Vulkan FA unreliable), 26B dropped. PROJECTED."
}
```

The design already reached the right conclusion for this hardware class: **E2B only, 26B dropped.**
Two things follow that the profile does not say out loud:

1. With `include_26b: false`, `escalation_model` and `reasoning_model` are unset. The harness then
   falls back to the workhorse — and the workhorse *is* the entry tier. **Every tier becomes the
   same model.** Verified live on this box:

   ```
   entry   triage,classify          -> gemma4-e2b  (triage_model)
   work    summarize,extract        -> gemma4-e2b  (model)
   escal   on quality failure       -> (unset -> falls back to workhorse)
   reason  grammar defers, pre-Opus -> (unset -> falls back to workhorse)
   ```

   The confidence gate still computes a margin and still decides to escalate — to itself. On this
   profile the cascade is not a cascade; it is one model plus a defer.

2. The profile assumes **32 GB of RAM**. This machine has 21 GB, which `detect.ps1`'s
   `Get-RamTier` bands as `min` (`< 28`). So even the weakest profile in the matrix is specified
   for a machine strictly larger than this one.

---

## 3. Measurements

All numbers from llama.cpp **b9934** — the exact tag pinned in `setup/install.ps1` — using the
official `llama-b9934-bin-ubuntu-vulkan-x64` build, and the byte-identical E2B model the installer
pins (2,620,368,960 bytes, verified against the `size` field in `install.ps1`).

### 3.1 Raw throughput (`llama-bench`, 2 repetitions)

| Backend | flash-attn | pp512 (t/s) | tg128 (t/s) |
|---|---|---|---|
| Vulkan, `-ngl 99` | off | **213.31 ± 1.12** | **14.86 ± 0.79** |
| Vulkan, `-ngl 99` | on | 209.82 ± 2.06 | 14.94 ± 0.41 |
| CPU, `-ngl 0` | off | 169.58 ± 2.73 | 10.69 ± 0.03 |

**Finding A — the iGPU buys +26% prompt processing and +39% token generation over pure CPU.**
The README's AMD table promises *"prompt processing ≈ 4× CPU"*. That figure is from the 780M
(RDNA3, DDR5). It does **not** carry to GCN: measured here it is **1.26×**. The likely cause is
visible in the device line — `matrix cores: none`. RDNA3 has WMMA; Vega does not, and prompt
processing is exactly the matrix-heavy phase. The README presents its AMD row as generic AMD
guidance; it is RDNA3 guidance.

**Finding B — flash-attn on is free on this hardware.** The profile disables it citing "older
Vulkan FA unreliable". On RADV/Renoir at b9934 it is performance-neutral (within noise). This does
**not** clear it: `llama-bench` measures speed, not deep-context correctness, and the known
Adrenalin device-lost issue is a *driver* bug that `selftest.ps1`'s depth-7000 canary exists to
catch. The honest statement is: the FA-off default costs nothing here, so leaving it off is cheap
insurance — but the stated *reason* ("unreliable") remains untested on this stack.

### 3.2 End-to-end through the harness

The real question is not t/s but seconds-to-answer. Served with the `amd-gcn` invariants
(`--ctx-size 8192 --cache-type-k f16 --cache-type-v f16 --jinja --reasoning off -ngl 99`,
flash-attn off), driven through the actual `local-offload` CLI, cold cache:

| Task | Input | Wall clock |
|---|---|---|
| `classify` | 500 chars | **3 s** |
| `triage` | 500 chars | **6 s** |
| `summarize` | 5,000 chars | **20 s** |
| `triage` | 19,162 chars | **39 s** |
| `summarize` | 19,162 chars | **51 s** |
| `summarize` | 24,000 chars (= default `max_input_chars`) | **59 s** |

`doctor` reports `health: OK`; **0 of 6 calls deferred**; every answer was correct on inspection.

**Finding C — quality is not the bottleneck on this class of machine. Latency is.** The E2B tier
answered everything asked of it. What makes it unusable is that a triage of a mid-size document
takes 39 seconds, and the model it is protecting would answer in ~2.

**Finding D — the default input ceiling is a tight fit, not a broken one.** 24,000 chars encodes to
6,734 tokens; with output that is ~88% of the 8K window. It completed rather than deferring, but the
margin is thin and untested against a full 8K prompt plus exemplars.

### 3.3 The economics, measured

Those calls, straight from the harness's own ledger:

```json
{ "calls": 5, "completed": 5, "deferred": 0,
  "tokens_saved": 13788, "est_value_kept_local": 0.20682 }
```

**119 seconds of the laptop's full attention to avoid $0.21 of Opus input cost.**

Extrapolating from the measured `pp512`, the ceiling on this box is around **$6–11 per hour** of
100%-saturated prompt processing (213 tok/s × 3600 × $15/Mtok, discounted by generation and
overhead). That number is the *best case* — it assumes you can keep the GPU pinned, on a laptop,
while doing nothing else with it.

Note the shape of it: both tokens saved and time spent scale linearly with input size, so **the
value per second is roughly constant** regardless of how big the inputs get. There is no input size
at which this becomes a good interactive trade. There is only a *volume* at which the total becomes
worth the wall clock — and that volume is batch, not interactive.

---

## 4. Gaps between what is claimed and what is true

| # | Gap | Evidence |
|---|---|---|
| **G1** | **There is no Linux install path at all.** `setup/` is 5 PowerShell scripts and 0 shell scripts; every template is named `llama-swap.win-*.yaml`. `SETUP-AGENT.md` is a PowerShell runbook. Yet the README's Requirements table lists **OS: Linux, macOS, Windows**, and `go install` is offered unconditionally. | `ls setup/` → `*.ps1`; `ls setup/templates/` → `win-*.yaml` |
| **G2** | **The Go side is fine on Linux.** `go build ./...`, `go vet`, `go test ./...` all pass on Linux/Go 1.26.5, and the CLI drove a real llama.cpp server to `health: OK` with a hand-written config. The gap in G1 is *packaging*, not portability. | §3.2 |
| **G3** | **The weakest profile still assumes a bigger machine than the weakest realistic machine.** `amd-gcn` says 32 GB; this box has 21 GB → `ram_tier = min`, which the matrix only warns about. | §2 |
| **G4** | **VRAM banding is meaningless on a UMA APU.** `detect.ps1` bands NVIDIA by `VramGb`, and AMD only by arch regex. This APU reports 2 GiB dedicated but addresses 12.7 GiB via GTT, of which only 6.7 GiB was actually free. Neither number is "VRAM" in the sense the bands mean. | §1 |
| **G5** | **The README's AMD performance row is RDNA3-specific, presented as AMD-generic.** "≈4× CPU" prompt processing measures 1.26× here. | Finding A |
| **G6** | **On this profile the cascade is one model.** Entry = workhorse = escalation. The confidence margin still fires and escalates to the same weights. | §2 |
| **G7** | **The harness has no latency gate.** It defers on *confidence*, on *truncation*, and on *infra health* — never on "this will take 39 seconds and the caller is interactive". On hardware where the model is slow but correct (Finding C), none of the existing defer paths ever trigger, so the harness cheerfully spends a minute on work the caller wanted in two seconds. | §3.2, §3.3 |
| **G8** | **Every non-NVIDIA profile is `PROJECTED`.** Before this document, config #12 had never been run. `amd-rdna3`, `cpu` and the three Blackwell tiers still have not. | `grep PROJECTED setup/templates/profiles.json` |

---

## 5. Decisions to make

### D1 — Does Linux get a first-class install path? (G1, G2)

| Option | Cost | Consequence |
|---|---|---|
| **a. Port the runbook** — `detect.sh` / `install.sh` / `selftest.sh` + `linux-*` templates | High. Duplicates ~1,000 lines of PowerShell logic and doubles the surface to keep in sync. | Linux becomes real. |
| **b. Document the manual path** — a short "Linux: bring your own llama.cpp" section with the serving invariants and a config example | Low. One doc page; the Go side already works (G2). | Honest, cheap, no new maintenance. |
| **c. Narrow the claim** — README says Windows-only for the managed stack, "builds anywhere" for the binary | Lowest. | Truthful, but concedes the platform. |

**Recommendation: (b), then (c)'s honesty applied to the README regardless.** The Requirements table
should not say "Linux" while the only runbook is PowerShell. (b) costs one page and is *already
proven to work* — this document's measurements were produced that way.

### D2 — Is sub-32 GB / no-escalation a supported mode or an unsupported one? (G3, G6)

| Option | Consequence |
|---|---|
| **a. Support it explicitly** — name the degenerate cascade in the docs, add a `min`-RAM profile, and make `models`/`doctor` say "single-tier: no escalation available on this machine" | Users on weak boxes get an honest picture instead of a cascade diagram that doesn't apply to them. |
| **b. Declare it unsupported** — `detect` hard-fails below 32 GB and says so | Clean, defensible, and closes the long tail of "it's slow" reports. |
| **c. Leave as is** | The matrix keeps a profile it has never run, specified for RAM the target machines don't have. |

**Recommendation: (a).** The profile already exists and already does the right thing; what's missing
is that nothing *tells the operator* the cascade collapsed. A one-line addition to `doctor` and
`models` output closes G6 at near-zero cost.

### D3 — Should the harness defer on predicted latency? (G7)

This is the substantive design question, and it generalises well beyond weak hardware.

Today's contract is "verified result or structured defer", where "can't" means *unsure*. On this
machine the harness is never unsure — it is *slow*. A caller that wanted a 2-second answer has no
way to say so, and no defer path fires.

Sketch: the harness already records `tok_per_s` and `latency_ms` per call in the ledger, and already
computes cheap input features (`len_chars` is in `meta.feat`). A `max_latency_ms` request field (or
config key) plus an estimate from measured throughput would let it return
`{"deferred": true, "reason": "predicted 39s exceeds the 5s budget"}` **before** spending the
minute. That reuses the ledger the flywheel already maintains, and it makes the harness usable on
slow hardware for the first time: it becomes "fast when I can, defer when I can't", which is exactly
the promise on fast hardware too.

| Option | Consequence |
|---|---|
| **a. Add a latency budget + predicted-latency defer** | Makes weak hardware viable (defers instead of stalling). New config surface + a predictor to keep honest. |
| **b. Document the latency and let callers decide** | Free. But an MCP tool call is synchronous — by the time the agent knows, it has already waited. |
| **c. Out of scope** | Slow boxes stay unusable interactively; the harness is batch-only there. |

**Recommendation: (a)**, as its own spec. It is the one change that would make this hardware class a
*supported* story rather than a *tolerated* one.

### D4 — Do the README's hardware claims get narrowed to what's measured? (G5, G8)

**Recommendation: yes, and cheaply.** Split the AMD row into RDNA3 (community-measured) and
GCN/Vega (now measured, and much weaker), and mark the Blackwell tiers as projected in the README
the way `profiles.json` already does internally. The repo's own convention of labelling `PROJECTED`
is good practice; the README does not inherit it.

---

## 6. Threats to validity

- **One machine, one model, one build.** E2B on RADV/Renoir at b9934. E4B (3.93 GiB) was not
  measured and would be slower roughly in proportion to size; the 26B was not attempted.
- **`llama-bench` measures throughput, not correctness.** Nothing here validates GBNF reliability,
  deep-context stability, or the FA-on reliability question (Finding B).
- **The box was not idle.** ~11 GiB of RAM and a desktop session were live — which is the realistic
  condition for a laptop, but it means 6.7 GiB free, not 12.9 GiB.
- **The dollar figures are avoided-cost estimates**, with all the counterfactual assumptions
  `est_value_kept_local` already carries. They compare *rates*, not billed savings.
- **Nothing here tests macOS**, which the README also claims.

## 7. Reproducing this

```bash
# 1. The exact pinned llama.cpp, Linux Vulkan build
gh release download b9934 --repo ggml-org/llama.cpp \
  --pattern 'llama-b9934-bin-ubuntu-vulkan-x64.tar.gz'
tar xzf llama-b9934-bin-ubuntu-vulkan-x64.tar.gz && cd llama-b9934
export LD_LIBRARY_PATH=$PWD

# 2. The exact pinned model (verify: 2620368960 bytes)
curl -L -o e2b.gguf 'https://huggingface.co/unsloth/gemma-4-E2B-it-qat-GGUF/resolve/main/gemma-4-E2B-it-qat-UD-Q4_K_XL.gguf'

# 3. Throughput
./llama-bench --list-devices
./llama-bench -m e2b.gguf -ngl 99 -fa 0 -p 512 -n 128 -r 2   # Vulkan
./llama-bench -m e2b.gguf -ngl 0  -fa 0 -p 512 -n 128 -r 2   # CPU baseline

# 4. Serve with the amd-gcn invariants
./llama-server -m e2b.gguf --alias gemma4-e2b -ngl 99 --ctx-size 8192 \
  --cache-type-k f16 --cache-type-v f16 --jinja --reasoning off \
  --port 11436 --host 127.0.0.1

# 5. Drive the harness (config: model=triage_model=gemma4-e2b, escalation_model="")
go build -o local-offload .
./local-offload --config cfg.json doctor
./local-offload --config cfg.json triage big.txt --question "..." --json
./local-offload --config cfg.json ledger
```
