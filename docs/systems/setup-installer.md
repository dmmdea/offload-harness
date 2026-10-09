# Setup and installer

## Purpose

The cross-vendor installer that turns a bare machine into a serving harness: detect the hardware,
pick a profile, install pinned binaries and models, generate a serving config, and prove it works.

## Questions this doc answers

- How does a machine get classified, and what does the classification control?
- What are the hardware profiles, and where are the VRAM boundaries?
- Which serving flags are universal and which are profile-driven?
- What is an agent allowed to do unsupervised during an install?

## Scope

`detect.ps1`, `install.ps1`, `selftest.ps1`, the serving templates, the profile table and its config
seeds, and the agent-executable runbook.

## Non-scope

- What the served tiers then do → [offload-pipeline.md](offload-pipeline.md)
- Media model bindings in use → [media-generation.md](media-generation.md)

## Key concepts

**Profile** — a named hardware class (`ampere-8`, `blackwell-48`, `cpu`, …) that selects a serving
template and a config seed. **Config seed** — the profile's default model bindings. **Receipt** — the
JSON line each script prints, which the runbook's decision tables key on.

## How the system works

Three scripts run in order, each ending with a machine-readable JSON line:

1. **`detect.ps1`** classifies the machine and emits a backend verdict.
2. **`install.ps1`** installs pinned binaries and models, substitutes the template placeholders, and
   builds the Go binaries.
3. **`selftest.ps1`** emits a receipt with verdict `pass | warn | fail`. On a Vulkan backend it
   also runs the **H3 canary suite** (`fa_q8kv`, `moe_full_offload`, `ctx_sweep`, `bench`,
   `swap_leak`, `embedder`, `whisper`) — promotion gates recorded in `receipt.canaries`, never
   verdict-changing; `OFFLOAD_SELFTEST_CANARIES=1|0` forces them on/off anywhere.

`setup/SETUP-AGENT.md` is written for an agent to execute directly, with decision tables keyed to
those receipts.

**Classification** happens in `Get-Profile`, and the evaluation order matters — multi-GPU is checked
first, because a heterogeneous pair outranks any single-card band. The three multi-GPU rules are the only
paths that set `big_ram` (RAM ≥ 120 GB), and `hwdetect` carries the same table in Go
(`TestHomogeneousBlackwellPairGetsItsOwnTier`):

| Condition | Profile |
|---|---|
| 2 NVIDIA GPUs, both Blackwell (an arch captured per card), largest card 12–23 GB | `blackwell-2x16` (checked before the generic multi-GPU rule) |
| 3 NVIDIA GPUs, all Blackwell (an arch captured per card), largest card 12–23 GB | `blackwell-3x16` (checked before the generic multi-GPU rule; before this rule a 3× Blackwell rig classified as `dual-gpu`, a tier the installer refuses because CUDA 13 cannot compile sm_70) |
| ≥2 NVIDIA GPUs, anything else | `dual-gpu` — a mixed-arch rig, four or more cards, a pair or triple outside the 12–23 GB band, or one whose archs were not all captured (`Get-Profile`'s self-test asserts the mixed-arch, four-card, uncaptured-arch and out-of-band pair cases). |
| NVIDIA Blackwell ≥64 GB | `blackwell-72` |
| NVIDIA Blackwell ≥40 GB | `blackwell-48` |
| NVIDIA Blackwell ≥24 GB | `blackwell-32` |
| NVIDIA Blackwell ≥12 GB | `blackwell-16` |
| NVIDIA Blackwell, below | `blackwell-8` |
| NVIDIA Volta (unconditional) | `volta-16` |
| NVIDIA Ampere/Ada ≥12 GB | `ampere-16` |
| NVIDIA Ampere/Ada ≥7 GB | `ampere-8` |
| NVIDIA Ampere/Ada, below | `ampere-6` |
| AMD RDNA3 ≥12 GB dedicated | `amd-rdna3-dgpu` (discrete RX 7900-class; 26B resident) |
| AMD RDNA3, below | `amd-rdna3` (iGPU/UMA floor — an iGPU's dedicated number is just the BIOS carve-out) |
| AMD, anything else | `amd-gcn` |
| No usable GPU | `cpu` |

Fifteen bands. Linux adds one PowerShell has no row for: `hwdetect` (the Go classifier `install detect`
runs) reads the device tree's root `compatible` when no NVIDIA or AMD adapter answered, and a board
that names the RK3588 SoC (`rockchip,rk3588` on the vendor kernel, `rockchip,rk3588s` on mainline)
is `rockchip-rk3588` — a board with no PCI adapter would otherwise fall through to `cpu` and be handed
CPU inference. Two boundaries are deliberately below their nominal card size: the `ampere-8` band
starts at **7 GB**, and `blackwell-72` starts at **64 GB** so it covers both 72 GB and 96 GB
workstation cards until larger hardware is actually measured.

For AMD, `detect.ps1` also reads the **Adrenalin (Radeon Software) version** from the registry and
classifies it against the known deep-context Vulkan crash class (≤ 25.11.1, llama.cpp #17432) —
the old generic "keep your driver current" warning is now a checked verdict, emitted as
`amd_adrenalin` in the JSON.

`detect.ps1 -SelfTest` asserts this table against **20 synthetic configurations**, plus separate
assertion families for architecture detection, RAM tiering, Adrenalin classification, and
unrecognized-hardware warnings.

> **Known coverage gap:** two configurations in the numbered matrix have no profile assertion, and
> `blackwell-8` is only asserted at exactly 8 GB rather than via a low-VRAM fallthrough.

**Serving flags** are split between universal and profile-driven, and conflating them causes real
confusion:

- **Universal on every task-serving entry:** `--jinja` and `--reasoning off`. Omitting
  `--reasoning off` produces empty output. No MTP or draft/speculative flags appear anywhere.
- **Profile-driven:** `--cache-type-k` / `--cache-type-v` (`q8_0` on nine profiles; `f16` on the
  remaining five — the two large-VRAM Blackwell tiers, the two AMD floor profiles (`amd-rdna3`,
  `amd-gcn`), and CPU — K and V always symmetric, and `q8_0` for V requires flash-attention on)
  and `--flash-attn` (on for every GPU profile since 2026-09-20 — `amd-gcn` was the exception until it was measured — and omitted entirely by the CPU
  `--cache-ram` (llama-server's host-RAM prompt cache) is rendered per RAM tier from the top-level
  `cache_ram_mib_by_ram_tier` map since 0.131.3 (ADR 0056) — never 0.
  template because that backend has neither `-ngl` nor `--flash-attn`). On `amd-rdna3` the f16/16K
  values are an explicit SAFE FLOOR: the selftest's H3 canary suite (`fa_q8kv`, `ctx_sweep`,
  `moe_full_offload`) measures the q8_0/32K/26B-full-offload promotions on the real box, and the
  installing agent applies them canary-gated per the runbook's AMD RDNA3 chapter.
- **Vulkan device pinning:** every model entry in the Vulkan template carries
  `GGML_VK_VISIBLE_DEVICES=0` so multi-ICD boxes serve from a deterministic adapter.
- **One exemption:** the `embeddinggemma` entry bypasses the shared flag macro entirely, taking
  `--embedding --pooling mean` instead. "All served models get these flags" is therefore false. The
  `embeddinggemma2` entry (below) takes the same exemption.
- **The memory stack's second embedder** (0.175.0): every template that renders the stack
  (`linux-cuda`, `linux-vulkan`, `linux-cpu`, `win-cuda`, `win-cuda-resident`, `win-cpu`, `win-vulkan`,
  `win-dual-cuda`, `win-dual-blackwell`, `win-triple-blackwell`) carries an `embeddinggemma2` entry:
  EmbeddingGemma-2 Q8_0 plus its multimodal projector (`--mmproj`), `--embeddings --pooling mean --ctx-size 4096
  --batch-size 4096 --ubatch-size 2048 --n-gpu-layers 99 --flash-attn on` (the CPU templates drop `-ngl` and
  `--flash-attn`, as their `embeddinggemma` entry runs without `-ngl`), `ttl: 300`, no aliases (the memory stack
  selects the id). It renders only on a tier that sets `include_embeddinggemma2`: ampere-6, the memory
  authority's card where it was measured, WITH the projector, and ampere-8 and blackwell-3x16 as **text-only
  replicas** (`embeddinggemma2_projector: false`). A replica only embeds text (media adds go to the authority) and
  text vectors are identical with and without the projector (cosine 1.0, measured), so the replica's entry is the
  authority's entry with the one `--mmproj <GGUF>` argument removed at render (`servingtmpl.dropEG2Projector`),
  every other flag identical (`--ubatch-size 2048` is load-bearing: the stack's hot budget is 1,900 tokens and a
  smaller ubatch returns HTTP 500 on long memories), and the installer does not download the projector for it.
  The field is optional and absent means true. The projector is off by arithmetic: with it the entry would exceed
  those cards beside the tier's seats (8,703 MiB on the 8 GB card, 16,375 MiB on the 16,311 MiB utility card of
  the 3-card tier), text-only it fits (7,649 and 15,321 MiB), and `eg2CardBudget` in
  `embeddinggemma2_stack_test.go` carries those sums, refuses the projector on a card its recorded sum exceeds and
  refuses a text-only tier whose projector sum has come to fit; an on-box co-residency measurement can turn the
  projector on. The entry joins the stack's residency
  set (`emb & rer & eg2`, or `emb & eg2` where a template has no reranker, or the one `resident` set of an
  all-resident template): resident beside the swappable seats, never swapped by them, with the stack's evict
  cost. With the flag off the entry, its matrix var and its evict row are all stripped. The `embeddinggemma`
  (300M) entry stays on every template: the harness's own embed lane and callers of `text-embedding` /
  `local-embed` keep using it. Footprint measured on the reference 6 GB node: 1,196 MiB loaded, 1,466 MiB after
  image embeds, 1,536 MiB after a short video with the projector; 460 MiB loaded and 482 MiB at peak text-only, at
  `--ubatch-size 2048`. On a text-only box the memory stack's `MEM0_MEDIA_EMBEDDER=off` (memory stack 1.35.1)
  makes offline media search a clean 400. It needs llama.cpp
  b11452 or newer (the `gemma-embedding2` architecture); on Windows `install.ps1` pins b11490 and downloads the two
  GGUFs (309,855,456 and 554,821,024 bytes) for a tier that carries the projector and the model alone for a text-only
  tier, with no RAM gate (`Get-GatedModelKeys -IncludeEmbeddingGemma2Projector`). A node whose main build is older keeps
  it for its other seats and runs this one entry from a second build (0.176.0, `--llama-bin-eg2`; see "A second llama.cpp
  build for the embeddinggemma2 entry" under "Serving config on Linux" below), and `install render` refuses a build it
  can read as older than b11452.
- **The Windows llama.cpp pin is b11490** (was b9934): the pre-built assets are `win-cuda-12.4`, **`win-cuda-13.4`**
  (named `13.3` until b9934, so the `llama-cuda13` / `llama-cudart13` URLs moved, not only their hashes), `win-vulkan`
  and `win-cpu`, each pinned by the GitHub release API digest. `Select-CudaBuild` still keys on the DRIVER's CUDA
  major (13.0 or newer for the Blackwell serve path), so the selection logic did not move.
- **The RAM-spill agent seat is a second, measured exception** (ADR 0080): `qwen3.6-35b-a3b-agent`
  writes its flags as literals (`--n-cpu-moe 40`, `--cache-ram 0`, `--load-mode none`, a 32768 window,
  q8_0 KV, no `--reasoning` flag because it is a thinking model) instead of the tier macros, because
  those literals are the configuration the 2026-10-07/08 bake measured. It renders only where the tier
  carries it (`include_qwen36_35b`) **and** the box's `ram_tier` is `low`, `mid` or `high` (28 GB and up;
  a `min` box, or a caller that names no tier, renders no spill seat and keeps the Qwen3.5-4B as
  `agent-seat`; the tier name is read case-blind and trimmed). The same predicate (`tierseed.RAMLowUp`) gates the seed
  overlay `config_seed_ram_low_up` that binds `agent_model` to it, so a binding never names a seat the
  roster dropped; `TestTheAgentSeatEachRAMTierBindsIsTheOneItsRenderServes` renders ampere-6 on both
  operating systems at every RAM tier to hold that. It claims the `agent-seat` alias, and the
  `qwen3.5-4b-agent` entry stays rendered beside it as an un-aliased opt-in rollback. It requires
  llama.cpp b10964 or newer (`--load-mode`), and on Windows `install.ps1` downloads the 12.3 GiB GGUF
  only when both conditions hold.

See [ADR 0002](../architecture/decisions/0002-grammar-reliable-serving-flags.md).

**Config seeds** bind media models per profile. Tiers at 16 GB and above seed HiDream-O1 bf16 and Wan
2.2 Q8_0. 8 GB tiers gained a **RAM-conditional layer** (J4): `config_seed_ram_mid_high` merges on
top of the base seed only when `ram_tier` is mid/high, so a 64 GB 8 GB box auto-binds what previously
needed manual config. A second layer, `config_seed_ram_low_up`, merges when `ram_tier` is low, mid or
high (28 GB and up, which is what a 32 GB box reports) and BEFORE the mid/high layer, so the mid/high
value wins key by key; `ampere-6` uses it to bind its agent seat to the Qwen3.6-35B-A3B spill seat on a
32 GB-class box while a `min` box keeps the Qwen3.5-4B. The two 8 GB tiers diverge by operator decision: `ampere-8`'s overlay stays the
verified O1 bf16 image seat, image only (2026-07-23 decision, standing there); `blackwell-8`'s
overlay ALSO seeds the wan22 video lane, `gen_edit_*`, and `inpaint_*` (2026-08-23 reversal, every
seat measured on its reference box — see `docs/tiers/blackwell-8.md`). The AMD profiles
seed the **sdcpp engine** (J2): `imagegen_engine:"sdcpp"` with the Apache-2.0 Z-Image-Turbo GGUF
set, full paths carried via the `__OFFLOAD_HOME__` token that `Merge-ConfigSeed` expands at install
time. The media leg itself (Step 5b: the pinned sd.cpp win-vulkan zip + roster downloads) defaults
on for `amd-*` profiles only; `OFFLOAD_WITH_MEDIA=1|0` forces it on/off anywhere and
`OFFLOAD_MEDIA_EXTRAS=1` adds the SD1.5/SDXL extras. `selftest.ps1` gains the first **media leg**
(`receipt.media`): a fixed-prompt reference render (non-blank gate: sampled distinct-colors) plus a
gpu-vae promotion trial mirroring the H3 canary pattern.

## Data and state

`$OFFLOAD_HOME` holds the serving config and binaries; `~/.local-offload/config.json` holds harness
config (the one file the harness finds by a fixed path). The harness's own **data** (cache, ledger,
media and svg output, delegation log, pipeline jobs, footprints, the coding agent's audit trail) hangs
off one install root, the `home` key. On Windows a fresh `install.ps1` writes `home` onto the data drive
that `local-offload install volumes --data` picks (never a cloud-synced virtual drive or a FAT
volume): C: holds Windows and program installs, never data
(operator rule, register C-92). `OFFLOAD_DATA_HOME` names another directory, and
`OFFLOAD_ALLOW_OS_DATA=1` is the explicit, recorded decision to keep the data on the OS drive (a
one-disk machine); with neither and no qualifying volume the install FAILS at Step 8 rather than
falling back to C:. The choice and its reason land in `installed.json` (`data_home`,
`data_home_because`). An existing `config.json` is never rewritten, so a node that already has one keeps
whatever `home` it names and is told, with a NOTE, when it names none. Templates in `setup/templates/`
carry placeholders substituted at install time; the config template spells no data path, because a
path written in the file is an explicit value that `home` does not rebase.

## Interfaces and entry points

`pwsh -NoProfile -File setup/detect.ps1` (add `-SelfTest` for the assertion suite), then
`install.ps1`, then `selftest.ps1`. The `local-offload-setup` skill is a thin wrapper pointing at the
runbook.

## Dependencies

PowerShell 7, a serving backend (CUDA, Vulkan, or CPU llama.cpp builds), Go 1.26+, and network access
for pinned assets at install time.

## Downstream effects

The profile string selects the serving template and seeds media bindings, so a misclassification
quietly under-uses hardware rather than failing loudly. Note that the fleet dispatcher routes on
*live* VRAM, not this string, so fleet placement is unaffected by a wrong profile.

## Invariants and assumptions

1. `--jinja` and `--reasoning off` on every task-serving entry.
2. No MTP or draft flags.
3. K and V cache types stay symmetric.
4. Pinned assets are pinned — an agent does not substitute versions.
5. Profiles are additive: adding a band means adding its template and its self-test assertion.

## Error handling

Each script's JSON receipt carries the verdict and the reason. `warn` is actionable and documented in
the runbook's decision tables; `fail` stops the install.

## Security and privacy notes

The runbook explicitly bounds unsupervised agent behavior: **do not** substitute pinned assets,
install ROCm/CUDA, or start the agent server beyond loopback without asking the human. Installers run
with real privileges and fetch remote assets, which is why the boundary is stated rather than assumed.

## Where an install goes (`local-offload install volumes`)

The installer has always used `$OFFLOAD_HOME`, defaulting to `$HOME\offload-stack` — the **OS
drive** on every machine. That is how a laptop ends up with a multi-GB model tree beside Windows and
a services box fills its root while a 250 GB pool sits idle next to it.

`local-offload install volumes` decides instead, from one policy:

1. Never a **removable** or **network** volume — an install that vanishes with a USB stick or a
   dropped share is worse than no install.
2. Never a volume below the floor (**20 GiB** free by default; a tier's media set alone exceeds 12 GiB).
3. Prefer any qualifying volume over the **OS volume**. Filling the OS volume takes the machine
   down, not just the harness, so it is selected only with `--allow-os-volume` — an explicit
   decision that gets recorded, never a silent fallback.
4. Among the rest, **most free space wins**; ties break on path depth, then name. Depth matters on
   ZFS, where every dataset of a pool reports the same free space: without it the harness lands
   under whatever sorts first (`apps/adventurelog` on the measured box) instead of the pool root.

`--data` asks the same question for the harness's **data** (a bbolt cache, an append-only ledger,
media that grows by gigabytes a day) and adds two refusals, because the roomiest volume is not always
a disk: a **cloud-synced virtual drive** (a volume label or mount-path segment naming Google Drive,
OneDrive, Dropbox, iCloud, pCloud, Nextcloud and the other sync roots the GPU lease already refuses as
a state directory) is a view of an account, and Google Drive for desktop reports FAT32, not removable,
with a cloud quota's free space; and a **FAT-family filesystem** (FAT, FAT32, exFAT) has no journal for
a store written continuously and, for FAT32, a 4 GiB file ceiling. `--json` then carries
`"data_target": true`, the choice's `because` names what was passed over, and with only such volumes
left the error names them. `install.ps1` always passes `--data` (and `Get-DataHome` refuses a choice
without the marker); doctor's FAIL text and `local-offload data migrate` use the same rule
(`datahome.DataTarget`). Without `--data` the answer is unchanged, which is what `install.sh` and the
model placement use.

Selection is pure and unit-tested (`internal/volumes`); only enumeration is platform-specific
(kernel32 on Windows, `/proc/mounts` + `statfs` on Unix), so the policy cannot drift between
operating systems. `--json` emits the full enumeration plus `{volume, because}` for a wrapper to
consume; the console view shows the roomiest few and says how many it withheld.

`because` is meant to be stored with the install, so a later operator can see why the tree is where
it is rather than re-deriving it. `install.sh` writes the chosen prefix as `home`; `install.ps1`
Step 8 does the same for a fresh config (the pure rule is `Get-DataHome`, fed by
`install volumes --json --data`) and records `data_home` and its `because` in `installed.json`. The stack
directory (`$OFFLOAD_HOME`, default under the user profile) is a separate install-root decision this
verb does not make for Windows yet.

### Serving config on Linux (`install render`)

Template rendering lived in `install.ps1`, so a Linux node could not produce a serving
config at all — every Linux deployment hand-wrote one, and on the measured 6 GB node the
first two hand-written topologies each broke the box.

```
local-offload install render --profile ampere-6 --home /srv/offload   --llama-bin /srv/offload/build/llamacpp/build/bin   --models /srv/offload/models --listen 127.0.0.1:11436 --ram-tier low --out llama-swap.yaml
```

Name the node's RAM with `--ram-tier min|low|mid|high` (28 GB and up is `low`, which is what a 32 GB box reports). It gates
the RAM-hungry placements: the 26B on the tiers that carry it and the RAM-spill agent seat of ampere-6 (`low` and up). A render
without the flag leaves the 26B placement ungated (the caller does not know) but does not render the spill seat: an unspecified
tier is below that floor.

The templates are **embedded in the binary**, so a fetched binary can render a config on a
machine with no checkout — which is the shape a real install needs. Omit `--profile` and it
classifies the machine first.

`setup/templates/llama-swap.linux-cuda.yaml` is not a translation of the Windows template.
Two things in it are Linux-specific and load-bearing:

- **`LD_LIBRARY_PATH` on every seat.** A self-built `llama-server` links its own shared
  objects; without it the process dies at exec with a loader error that reads nothing like
  a config problem.
- **The group topology is MEASURED.** `heavy` is `swap:true, exclusive:false` and `support`
  is `swap:false`. Both were learned the hard way on the 6 GB node: `exclusive:true` on a
  swapping tier meant the loaded seat evicted everything and nothing evicted it, so every
  chat request returned 502 for the full 5-minute TTL after any render; and with the
  embedder inside the swapping tier, one RAG query paid three full model loads (free VRAM
  dropped 3655 → 1005 MiB because loading the embedder had evicted the chat model).
  `TestHeavyGroupIsNeverExclusive` encodes that as a test rather than a comment.

Rendering **refuses to emit a config that still contains a token**. `install.ps1` carries a
comment about that exact failure; a llama-swap started with a literal `--ctx-size __CTX__`
fails in a way that looks like a model problem. A tier that drops the 26B has its model
block **and** its group membership removed together — llama-swap rejects a config whose
group names a model that does not exist. The **download** follows the same flag: Step 5
resolves the profile (`Resolve-ProfileParams`, the RAM gate included) before it builds the
download set, and `Get-FamilyModelKeys` adds `model-26b` only when the resolved
`include_26b` is true. It used to add it on the family gate alone, so `blackwell-8`
(`include_26b: false`) fetched 14.25 GB the rendered yaml never serves (<node-e> parity
audit, 2026-09-23).

#### The `rk3588` backend (Rockchip SoC boards)

The `rockchip-rk3588` tier renders from its own template, `llama-swap.linux-rk3588.yaml`, and not
from `llama-swap.linux-vulkan.yaml`: the stock vulkan template always renders `offload-e4b` (~5 GB
plus KV) beside an embedder and a reranker, and none of that fits a board whose GPU and NPU share
about 4.7 GiB of inference budget with the host's own workload (`uma_reserve_gib` holds the rest
back). The template lists only what fits — one llama.cpp Vulkan chat entry, `GGML_VK_VISIBLE_DEVICES=0`,
every layer offloaded — and places the tier's `rkllm` NPU seats (a model served by the Rockchip RKLLM
runtime, with its own window, CPU mask and optional repeat-penalty default) as alternatives to it. No model runs on the CPU there: the
tier declares no `alt_backends`, so `--llama-bin-cpu` is refused, and it renders no `embeddinggemma2` entry, so
`--llama-bin-eg2` is refused by name too. `--llama-bin` still names a
llama.cpp build with the Vulkan backend, as for `vulkan`; the installer script needs nothing else.

#### A second llama.cpp build for the embeddinggemma2 entry (`--llama-bin-eg2`)

The `embeddinggemma2` entry needs llama.cpp b11452 or newer (the `gemma-embedding2` architecture). The render used to
have one build for every entry, so a node whose main build is older, and has to stay that way for its other seats (the
`ampere-6` reference box ran b10964 when the entry arrived), could not load the entry from a rendered config. Its only
way out was a hand-edited config, which `audit-yaml` reads as HAND-EDITED or UNSTAMPED and which the next re-render
would overwrite. `install render --llama-bin-eg2 <dir>` (`Params.EG2LlamaBin`, the sibling of `--llama-bin-cpu`, ADR
[0081](../architecture/decisions/0081-the-embeddinggemma2-entry-may-run-from-a-second-llama-build.md)) points that one
entry at a second build and leaves every other entry on `--llama-bin`:

```
local-offload install render --profile ampere-6 --home /opt/offload --llama-bin /opt/offload/build/llamacpp-b10964 --llama-bin-eg2 /opt/offload/build/llamacpp-b11490 --models /opt/offload/models --ram-tier low --out llama-swap.yaml
```

- **What moves.** The entry's `cmd` path and nothing else on a Windows template. On a Linux template (one that defines the
  loader macro `ld:`) the entry's env also swaps its `${ld}` list item for a macro of its own, `ldembed`
  (`LD_LIBRARY_PATH=<dir>:...`), inserted right after `ld:`, because the shared macro names the main build's directory and
  a build links its own shared objects; the Vulkan template keeps its `${vk}` device pin beside it. With `--llama-bin-cpu`
  as well the macros read `ld`, `ldcpu`, `ldembed`. Which path a render takes follows the template (does it define `ld:`),
  not the target OS.
- **How.** A rewrite of the `embeddinggemma2` block inside `servingtmpl.Render`, before substitution, on the same block
  scanner as the text-only projector strip. No template and no `profiles.json` edit, so `template_sha256` and every
  `profiles_entry_sha256` stay put: an unset render is byte-identical to the previous release's (compared on all ten
  templates, projector kept and stripped), and no stamped node reads STALE because of this change. The rewrite is exact in
  both directions: the entry must name `__LLAMA_BIN__` exactly once, and on a Linux template carry exactly one `${ld}`
  item, or the render fails naming the entry.
- **The value.** Backslashes become forward slashes and a trailing slash goes (llama-swap on Windows mis-parses
  backslashes inside a `cmd`); the main build spelled again is no build of its own, so nothing is recorded and the render
  is the unset one. A double quote, a line break or a `$` is refused (the value lands in a double-quoted YAML macro that
  llama-swap expands), and so is a value that is only separators. A tier that does not carry `include_embeddinggemma2`
  (the other CUDA tiers, the Rockchip board, an off-matrix box) refuses the flag by naming the tier and the flag, because
  the stamp would record a build that serves nothing.
- **The floor check, at write time only.** `eg2MinLlamaBuild` is 11452. `install render` reads the build from a `b<digits>`
  token of four to six digits in the build directory's own name (`llamacpp-b10964`, `llama.cpp-b11490`,
  `llama-b11490-bin-win-cuda-12.4-x64`) and never runs `llama-server`, whose `--version` initialises every CUDA card on
  the box while `install render` runs on live nodes. It checks the entry's own build when the flag is set and the main
  build otherwise, and only for a render that includes the entry. A build the name states below the floor is refused
  (`tier T renders embeddinggemma2, which needs llama.cpp b11452 or newer (gemma-embedding2), but <dir> is bN: pass
  --llama-bin-eg2 <dir of a b11452+ build> - not written`), at or above it is silent, and a name that states none (a
  directory called `llama`) is a `note:` line and the render proceeds. The note goes to stdout when `--out` is set
  (`install.ps1` reads that stream and its self-test treats stderr output as an error) and to stderr otherwise, where
  stdout is the stamped config itself. **Behaviour change when the flag is unset:** a render whose main build is named
  below b11452, on a tier that carries the entry, is now refused where it used to be written; that entry cannot start on
  such a build. The replay never checks the floor (`audit-yaml` runs on another machine with another machine's recorded
  paths and must not judge them), and `TestInstallPs1PinsABuildAtOrAboveTheEG2Floor` holds the Windows installer's pinned
  tag at or above it.
- **Provenance and audit.** The stamp's basis records `eg2_llama_bin` (omitted when unset, so an unset render keeps its
  spec hash) and the replay carries it, so a node rendered with the flag audits MATCH. `audit-yaml` has no per-entry
  view: an entry's path edited by hand still reads HAND-EDITED, and the same change made by the renderer (on a template
  without a loader macro it is exactly that path edit) reads MATCH. The replay now also carries `--llama-bin-cpu`, which
  it used to drop, so a node rendered with a CPU family would have read STALE for a path it chose itself (dormant: no tier
  declares `alt_backends`). **A binary older than 0.176.0 auditing a stamp that carries `eg2_llama_bin` reads HAND-EDITED**
  (it drops the key it does not know, so the spec hash no longer matches): upgrade the binary before re-rendering with the
  flag.
- **Windows.** `install.ps1` installs one pinned tag for every node it installs (b11490, at or above the floor), so an
  installer-managed node needs no second build. `OFFLOAD_EG2_LLAMA_BIN=<dir>` is the opt-in override for a node that keeps
  an older main build: the directory must hold `llama-server.exe` (else the script throws), is normalised to forward
  slashes, is appended to the render args as `--llama-bin-eg2`, and joins the Step 6 skip test so an upgrade re-renders. The
  directory must be a complete extraction (the llama zip, plus the cudart zip for CUDA), because Windows resolves its DLLs
  beside the executable and the entry carries no loader macro; a bare llama zip fails at load with a DLL error that reads
  like a model problem. The skip test cannot tell that the variable was removed: dropping the override needs a fresh render
  (`-RenderOnly`, or delete the yaml). A hand-kept config the installer does not render is not helped until the node
  adopts `install render`.
- **Linux.** `setup/install.sh --llama-bin-eg2 DIR` (it must be a directory; `install.sh` builds and downloads nothing, so
  the operator supplies the build).
- **Adopting it on a node that keeps an older main build.** (1) Put a b11452-or-newer build beside the old one, in a
  directory named with its build. (2) Render to a scratch file with `--llama-bin <old> --llama-bin-eg2 <new>` and the box's
  existing flags. (3) Diff it against the live config: expect the stamp header and the entry's path (and macro on Linux);
  anything else is hand-wiring the render does not carry, to be decided line by line. (4) In a quiet window, swap the file
  in and restart llama-swap (a restart drops every loaded seat). (5) `audit-yaml --against-render` reads
  MATCH.

> **Unverified:** whether llama-swap fails the whole residents set when one member (here the entry on a build too old to
> load it) cannot start. The floor check exists either way: the entry cannot start on such a build.

> **Unverified:** llama-swap's rule for macro names. `ldembed` is letters only, like the existing `ldcpu`, which is the
> shape measured to work.

> **Unverified:** that a build-consistency check across loaded seats (`llamaswap build check`) reports drift for a node that
> runs two builds side by side, as this shape does. Read from its source, not run.

#### What `install render` refuses to write

Every render passes one gate (`renderGate`) before it can be written. A failure names the tier and
every offender, and nothing is written:

1. **The serving-config rules (H-01, INV-1 / INV-2).** No `-ngl 0` or empty `CUDA_VISIBLE_DEVICES` (a
   model on the CPU), `ttl: 300` on every entry, no `persistent` group, no preload hook.
   `local-offload audit-yaml` runs the same checker over a live file.
2. **The spill ceiling (H-01, INV-1).** `--n-cpu-moe N` (every spelling, and the `LLAMA_ARG_N_CPU_MOE`
   twin) above the tier's `n_cpu_moe_max`, its MEASURED spill, is refused. A tier that recorded none
   sanctions none, so any `N` above zero is refused. A tier that names `moe_26b: n_cpu_moe` with no `N`
   is refused too, because that renders the every-expert `--cpu-moe` on a box with a card.
   `n_cpu_moe_max` is a separate number from `n_cpu_moe` on purpose: the placement a tier ships and the
   ceiling its measurement supports are two decisions, and one field cannot check itself. Only `ampere-6`
   declares one: `n_cpu_moe_max: 40`, the number its Qwen3.6-35B-A3B spill seat was measured at (the seat's
   `--n-cpu-moe 40` is a literal in the template, so the ceiling is what lets it render; see
   [ADR 0080](../architecture/decisions/0080-a-ram-spill-moe-is-the-agent-seat-when-it-earns-it.md)).
   The check is entry-agnostic, so the same number would also sanction a 26B placement of up to 40 on that
   tier, which is inert while its `moe_26b` is `drop`.
3. **The layer check (ADR 0052, D5).** A tier that declares layers must render the seats they name, on the
   cards they name. It runs for any tier that declares layers, not only one that composes others: the
   `ampere-16` tier's `fast` layer would otherwise route to a seat the config never defined.

Rules 1 and 2 read each entry the way llama-swap runs it: a `${name}` macro is replaced by its text first
(nested macros too), because the templates keep their shared flags in `macros:` and a flag placed there is run
by every entry that references it.

`TestInstallRendersOnAnyTierWithoutACacheServer` is the other half of the same promise (INV-16: the
harness installs and serves on any single PC, and the cache-server tier is optional): every tier renders
with no vLLM prerequisites, and a tier whose vLLM seat declares no store renders the seat, its unit and its
wrappers with no cache-server piece anywhere and seeds an explicit storeless binding for it.

#### The provenance stamp (0.123.0, ADR 0043)

Every config `install render` writes now begins with a six-line comment block:

```
# local-offload serving-config provenance -- generated; re-derive with `local-offload audit-yaml --against-render`
# spec_sha256: <64 hex>
# body_sha256: <64 hex>
# rendered_by: 0.123.0
# tier: ampere-16
# rendered_at: 2026-09-14T11:22:33Z
# basis: {"harness_version":"0.123.0","params":{...},"profiles_entry_sha256":"...","render":{...},"template_sha256":"...","tier_id":"ampere-16"}
```

`spec_sha256` hashes a **closed, documented input set** (`servingtmpl.SpecBasis`): the tier
id, the render `Params` mirrored field for field, the serving template's sha256, the tier's
own `profiles.json` entry canonicalised and hashed, the two flags that gate which seeds apply
(`--ram-tier`, `--fallback-backend`), and the harness version — canonical JSON, sorted keys,
number literals preserved. `body_sha256` hashes the yaml **below** the block, which is what
keeps a hand edit distinguishable from a seed change.

The block is inert yaml, and the body beneath it is byte-identical to what `Render` produced:
the rule audit still runs on the unstamped text, exactly as before. The config only goes to
stdout when `--out` is omitted, so `install.ps1` and `install.sh` are unaffected; the `--out`
success line now also carries the spec hash.

Why it exists: the serving config is rendered ONCE and nothing re-renders it. Register A-39
raised `ampere-16`'s `ctx_size` 32768 → 131072 and the live file kept serving 32768 while
`audit-yaml` reported `OK` — honestly, because a config a tier revision stale breaks no
operator rule. A rule gate cannot see staleness.

#### Re-deriving it: `audit-yaml --against-render`

```
local-offload audit-yaml --against-render C:/llama-swap/llama-swap.yaml
```

Flags come **before** the files (Go's flag package stops at the first non-flag argument). Each
file gets its usual rule line plus one provenance line, reporting exactly one state:

| state | means | exit |
|---|---|---|
| `MATCH` | re-rendering from this binary's seeds reproduces the file byte for byte, and no seed input moved | 0 |
| `STALE(<keys>)` | the seeds moved — the report NAMES the basis keys (`params.ctx_size`, `profiles_entry_sha256`, …) | 1 |
| `UNSTAMPED` | no provenance block: rendered before 0.123.0, or written by hand | 0 |
| `HAND-EDITED` | the body no longer hashes to the stamp's record, or the stamp itself was edited | 1 |

The verdict is settled by **re-rendering**, not by comparing hashes — a hash moves on inputs
that cannot change the output, and a gate that cries stale on a documentation edit gets
ignored. The replay pins the per-box inputs the stamp recorded (install paths, listen address,
thread count, the vLLM deployment half) instead of re-deriving them from the auditing machine,
which would report every node stale; a seed those pin can therefore move without changing the
rendered text, and that case reports STALE naming `profiles_entry_sha256` with a detail saying
the served config is unchanged and only the stamp is behind.

`UNSTAMPED` deliberately does not exit 1: every config on the fleet predates stamping, and a
gate that is red on every box from day one is a gate nobody reads.

#### The config half: `audit-config`

`audit-yaml` audits the serving YAML. It never looked at `config.json`, and that is where the
drift actually lived. Measured winners were wired **by hand into a node's config** and never
written back to `profiles.json`:

- <node-f>'s `qwen3.5-4b-agent` seat and its four lane keys;
- <node-c>'s layers, its 35B digest seat and its cascade rungs;
- <node-b>'s image-edit, inpaint and animate routes.

The node kept working, the seed kept the loser, and every fresh install lost the win. Every
regeneration of the tier matrix, which reads the seed, erased it from the record too. The
2026-09-21 wiring-debt audit found this pattern on every node it read.

```
local-offload audit-config                                   # this node, its own tier and detected RAM tier
local-offload audit-config --config node.json --tier ampere-16 --home /srv/x     --goos linux --ram-tier mid --vllm-seat-active true      # a node read over SSH
```

It resolves the tier seed **exactly as `install seed` does**, with the same `--goos`,
`--ram-tier` and vLLM-seat detection. Skip them and the audit compares the node against a seed
the installer would never have written, such as a vLLM box against its fallback agent, and
reports drift that is its own artifact.

`--ram-tier` defaults to **auto**: the RAM tier this machine detects (`detect` stamps the same
value), so a 64 GB box is compared against the base seed **plus** the `config_seed_ram_low_up` and
`config_seed_ram_mid_high` overlays its installer applied (a 32 GB box, `low`, against the base seed
plus `config_seed_ram_low_up`; a tier that declares neither overlay compares against its base seed). It used to default to the base seed alone, and on a blackwell-8 box
with 64 GB of RAM 23 of the 38 rows it called drifted were overlay-carried false positives. Pass
`--ram-tier none` to compare the base seed alone, `min` to speak for a smaller box (no overlay
applies), or `low`, `mid` or `high` to name one explicitly, which is what a node read
over SSH needs: the auditing machine's RAM is not the node's. An unknown value is refused.
Detection reads **this** machine only, so when `--config`, `--home` or a `--goos` other than this
platform's point away from it and `--ram-tier` is not named, the audit prints a warning on stderr
that the overlay compared is this machine's RAM tier and asks for `--ram-tier` for another node's
config. A RAM probe that reads 0 GB is refused with the same pointer: 0 GB would classify as `min`,
and the audit would pick the base seed without saying so.
Both outputs say which seed was compared. The text header reads
`ram-tier=mid (detected, 64 GB; base seed + config_seed_ram_low_up and config_seed_ram_mid_high overlays (where the tier declares them))` or
`ram-tier=none (--ram-tier; base seed only, no RAM overlay)`, and `--json` carries `ram_tier`,
`ram_tier_source` (`detected` or `--ram-tier`) and `ram_overlay` (the most specific overlay compared:
`config_seed_ram_mid_high` on mid/high, `config_seed_ram_low_up` on low, else `none`). `--vllm-seat-active auto` runs the installer's own
detection, which is right for the local box. Pass `true` or `false` for a remote one. The flag speaks
for the tier's vLLM seats as a set: `true` says the node serves the lane seat and every extra seat,
`false` none, and `auto` detects each seat on its own (the venv plus that seat's weights, and for an extra
seat the wrapper scripts in `--vllm-seat-dir`, default `<home>/seat`).

It reports only **seed-owned** keys: every key some tier's resolved seed can write, plus live
bindings no tier seeds at all. A config also holds keys that are legitimately this machine's
own, such as endpoints, install paths and ports. A report that flagged those would bury the
drift it exists to show.

| class | means |
|---|---|
| `DIFFERENT` | both set, values differ |
| `LIVE-ONLY` | set by hand on the node; this tier's seed does not write it; a fresh install loses it |
| `UNSEEDED` | a live binding (a key ending in `_model`, `_script`, `_unet`, `_ckpt`, …) that **no** tier seeds |
| `SEED-ONLY` | the seed writes it; the node does not have it |
| `MATCH` | agrees (listed with `--all`) |

`UNSEEDED` exists because a seed-owned comparison alone was blind to the worst case. A media
route that no tier carries is invisible to a check that reads only what seeds can write. That
is exactly how <node-b>'s image-edit and animate wins stayed node-only. Empty-string values
are unbound routes and are not reported. `nim_*` keys are the cloud escalation account, not a
seat, and are not reported either.

Exit 1 on any drift.

**Verified end to end on the Linux node:** the rendered `ampere-6` config was handed to the
node's own `llama-swap` on a throwaway port, which accepted it and listed exactly
`offload-e4b`, `gemma4-e2b`, `embeddinggemma`, `bge-reranker-v2-m3` — the 26B correctly
absent. The live service on `:11436` was untouched throughout.

### The Linux install path (`setup/install.sh`)

```
setup/install.sh --bin ./local-offload --llama-bin /path/to/llamacpp/build/bin [--prefix DIR] [--user NAME]
```

`--llama-bin` is required on every tier except one whose backend (`install tier-info`) is `rk3588`: that
tier's template has no llama.cpp entry (the NPU serves), so a board there has no build to point at.
`setup/install.tests.sh` pins the rule and the `--rknpu-home` pass-through with a stub binary under `--dry-run`.
`--llama-bin-eg2 DIR` is optional (it must be a directory when given): the directory of the llama.cpp build that serves only
the `embeddinggemma2` entry, for a node whose main build is older than b11452 (see "A second llama.cpp build for the
embeddinggemma2 entry" above). The dry run names it in its `would render` line and the render call passes it only when set.

It is **deliberately thin**. Every decision that can be wrong lives in the binary, which
is cross-compiled and unit-tested; the script only fetches, places and registers:

| step | who decides |
|---|---|
| which tier is this machine? | `install detect` |
| which disk should hold it? | `install volumes` (never the OS volume) |
| what media does the tier bind? | `install seed` (rendered for this OS) |
| what serving config? | `install render` |
| may this node be handed work? | `acceptance`, run **as the service identity** |

If you find yourself adding a decision to the script, it belongs in the binary — that is
the whole reason this path and `install.ps1` cannot drift.

**`--dry-run` prints every decision and command and changes nothing.** Use it first; it is
how the two real defects below were found before any node was touched.

**The tier table and serving templates are EMBEDDED in the binary.** An install begins by
fetching one binary onto a machine with no checkout, so reading `profiles.json` from a repo
path is the development case, not the install case. When that lookup silently failed, the
script produced a config with **no media bindings at all and said nothing** — the exact
class of drift this workstream exists to end. A seed failure is now fatal.

**Prerequisites the script does not install** (and says so up front): a built llama.cpp,
the tier's GGUF models, node, and — for the sdcpp tiers — the sd.cpp binary. The acceptance
gate refuses the node until they are present, which is the intended outcome: a node that
cannot render must not be advertised as one that can.

**Line endings are load-bearing here.** A `.sh` checked out with CRLF fails at exec with
`env: 'bash
': No such file or directory` — a message naming neither the script nor the
cause. This repo is developed on Windows and deployed to Linux, so `.gitattributes` pins
`*.sh`, the serving templates and the generated docs to LF.

### Classification without PowerShell (`install detect` / `install plan`)

`setup/detect.ps1`'s second statement refuses to run anywhere but Windows, so a Linux
box could never be told what it IS — and its serving topology, resident tier and media
bindings had to be hand-derived. On the measured Linux node the first two hand-derived
topologies were both wrong in ways that broke chat.

```
local-offload install detect            # what is this machine, and which tier?
local-offload install plan              # ...and what would an install bind here?
```

Both are read-only: probe, classify, print. `--json` feeds a wrapper.

`internal/hwdetect.Classify` is a **straight port of `Get-Profile`** — same order, same
bands — and `ArchFromName` ports `Get-Arch` rule for rule, because the rule ORDER is the
logic (an "RTX PRO 5000 Blackwell" must not fall through to the RTX-50xx rule it does not
match). Both are verified against the same table `detect.tests.ps1` asserts, so a
machine's tier cannot depend on which implementation asked. `detect.ps1` remains the
Windows install path until the wrapper work lands; this is what makes a non-Windows
install possible at all.

Detection prefers `nvidia-smi` and falls back per OS (CIM on Windows, DRM sysfs + lspci on
Linux) — an AMD box that cannot be identified must never be silently called `cpu`, which
would strip it of the entire Vulkan serving path.

Verified on the fleet: <node-b> → `blackwell-2x16` (RTX 5060 Ti 16 GB **+** RTX 5070 Ti 16 GB,
~32 GB total), the <node-a> laptop → `ampere-8` (RTX 3070 Laptop, 8 GB), and the Linux node →
`ampere-6` (RTX 3050, 6 GB), each matching the tier that box ran AT THAT TIME. Both have since moved: the Linux node is `ampere-16` (A2 16 GB) since 2026-09-04, and <node-b> has run three Blackwell cards since 2026-08-31, which `Get-Profile` files as `blackwell-3x16` (shipped 0.113.32; before it, the same rig filed as `dual-gpu`, a tier the installer refuses because CUDA 13 cannot compile sm_70). Historical text: each matching the tier it actually runs.

> <node-b> read `blackwell-16` (single RTX 5060 Ti, 15.9 GB) until **2026-08-02**, when the
> 5070 Ti was installed. Detection already handles this — `hwdetect.Classify` returns
> `blackwell-2x16` for two Blackwell cards, and its test names this exact pair as the
> reference box — but this sentence lagged, and stale "5060 Ti solo" wording in several
> places led to the box repeatedly being budgeted as a single 16 GB card. It is 32 GB
> across two cards. Corrected 2026-08-05. **Superseded 2026-08-31 (recorded 2026-09-07):** a THIRD card (RTX 5060 Ti 16 GB) took <node-b> to 3× 16 GB = **48.9 GB**, which used to file as `dual-gpu` — `blackwell-48` means ONE 48 GB card. **`blackwell-3x16` SHIPPED 0.113.32**, and since 0.123.2 it is a COMPOSITE tier (ADR 0052): it declares `composes` (the tiers it is a complete instance of) and `layers` (its device layers, their seats, their guards), and `install seed` writes `tier_profile`, `tiers` and `layers` into config.json beside `config_seed`/`media_seats`. `install render` refuses a composite render that is not the checked union of what it composes. See [composite-tier.md](composite-tier.md).

### Tier media seeds (`local-offload install seed`)

A tier's `config_seed` is the media/config fragment a fresh install of that hardware class
starts from. It lived only inside `install.ps1`, which made it Windows-shaped (`sd-cli.exe`
baked into the table) and unreachable from a non-Windows install. Resolution now lives in
`internal/tierseed`:

```
local-offload install seed --profile ampere-6 --home /srv/offload-stack --os linux
```

- `__OFFLOAD_HOME__` expands to the install root, `__EXE__` to `.exe` on Windows and nothing
  elsewhere — **one row renders on every OS**, which is what makes a tier a hardware class
  rather than a Windows class.
- `--os` targets a machine other than the one resolving, so a Windows box can render a Linux
  node's fragment.
- `vae_mode: tiling|cpu|none` replaces free-text `sdcpp_extra_args` for the VAE lever and is
  **refused as `cpu` on a CUDA backend**, where it measured 7.8× slower (58.2 s vs 7.5 s with
  tiling). It is correct on an AMD/UMA part; free text is how it would spread to a tier it is
  wrong for.
- Every seed key is checked against the real `config.Config` fields. A typo'd key is silently
  dropped by the loader on *every* install of that tier, so it is refused at authoring time.

`TestEveryShippedSeedIsValid` resolves every tier in the table for **both** platforms, which
is the gate that would have caught `sd-cli.exe`.

**vLLM seats, the roster and the layers.** `install seed` decides once, per seat, whether the box can run
it (the hand-built venv plus that seat's weights) and seeds accordingly:

- The lane seat (`vllm_seat`) binds `agent_model`; a box without it binds the seat's fallback.
- Every further seat (`extra_vllm_seats`) that the box can run joins the `vllm_seats` roster with its own
  `kv_cache_server` binding, and never touches `agent_model`. "Can run" adds the operator's part to the lane
  seat's check: the seat's wrapper scripts must be in the seat directory (`--vllm-seat-dir`, default
  `<home>/seat`, the same flag `install render` takes), because the installer does not write them and
  llama-swap would list a seat whose scripts are missing and fail only when it is asked for. A seat with no store seeds an explicit
  storeless opt-out carrying its `storeless_reason` (the measured reason) or, when the tier recorded none,
  the generic one, so a fresh install never ships a config its own `doctor` rejects.
- The tier's `layers` are seeded as the box can serve them: a layer whose vLLM seat is absent is dropped, and
  a layer set that lost `single` is not seeded at all (`tierseed.ResolveLayers`).

The composite design and what an `ampere-16` operator still installs by hand are in
[composite-tier.md](composite-tier.md).

### Relocating an install: one knob, not a dozen paths

Choosing a volume is only half the job — the harness has to actually live there. Every
derived path (cache, ledger, media/svg output, exemplars, thresholds, router and confhead
stores) hangs off an install root:

| source | precedence |
|---|---|
| an explicit value for that key in `config.json` | always wins |
| `"home": "D:/offload-stack"` in `config.json` | rebases everything still at its default |
| `$LOCAL_OFFLOAD_HOME` | same, before any config file exists (the bootstrap case) |
| `~/.local-offload` | the fallback |

So moving an install is: copy the tree, set `home`, done. Before this, relocating meant
hand-writing about a dozen absolute paths into the config — which is how a machine ends up
with a model tree on its OS drive and bindings that drift from the binary.

**The machine-wide state root is deliberately excluded.** `state_dir` / `gpu_lock_path`
stay unset so `internal/gpulease` resolves them machine-wide (`%ProgramData%` /
`/var/lib`). Rebasing the GPU lease under a home directory is the per-user trap that
silently un-serializes the GPU — 0.24.1 added a warning for exactly that.

#### Windows: data never lives on the OS drive (register C-92)

The runtime default does not pick a drive. A default that depended on the volumes mounted at load time
would re-point every existing node at an empty tree the moment the binary was upgraded, which is moving
live data silently; the path would also change when a drive was added. The decision is made once, by the
installer, and recorded as an explicit `home` (existing configs keep theirs). Three surfaces keep it
honest:

- **`local-offload doctor`** prints a `data volume` section and exits non-zero when any data location
  resolves onto the OS drive while a volume qualifies to hold it: a non-OS volume that is neither a
  cloud-synced virtual drive nor a FAT volume (`internal/datahome`, `internal/volumes`). It prints the install root once with the paths that follow it folded in, and any
  key written elsewhere on the OS drive on its own row, because moving `home` does not move it. A box
  with no other qualifying volume gets one note and no FAIL, and a non-Windows host prints nothing.
  `state_dir` and `gpu_lock_path` are not data and are never listed. A junction does not count as a fix:
  the audit reads the path you wrote.
- **`local-offload data status`** is the same audit as a full table (`--json` for scripts); it exits 1
  exactly when doctor would FAIL.
- **`local-offload data migrate [--from DIR] [--to DIR] [--apply] [--stopped]`** is the way off C:.
  Without `--to` the target is a fixed directory on the volume the data-volume rule picks
  (`install volumes --data`), never the OS drive, a cloud-synced virtual drive or a FAT volume. `--from`
  and `--to` are resolved to absolute paths before anything is checked, so a relative `--to` that lands on
  the OS drive is refused and the `home` line it prints is absolute. It is a **copy**: cached transcribe results embed absolute paths into the old media tree and
  the result cache is a bbolt file a running door holds open, so nothing is moved, deleted or rewritten
  under the source, and no link is created or followed. It is a dry run until `--apply`. The bbolt
  stores (`*.db`) are held back until `--stopped` says fleet-serve and every MCP door are stopped.
  Each file goes to a temp name, is verified by re-reading it, takes the source's mtime and only then
  replaces the destination; a file that changes under the reader is retried and then reported, never
  accepted as a torn copy; a destination newer than its source is kept, so re-running after the switch
  cannot clobber live data. `config.json` is not carried (the harness finds its config by a fixed path).
  An apply that held back or could not read anything exits non-zero.

The migration, in order: stop fleet-serve and the MCP doors, `data migrate --apply --stopped`, set the
`home` line the verb prints, restart the doors, run `doctor`, and delete the old tree yourself once the
new home has run clean. The ComfyUI side (its `input` and `output` folders under the ComfyUI install) is a
separate surface and is not covered here.

## Observability and debugging

`local-offload doctor` verifies the serving layer end to end and reports per-alias reachability, and on
Windows says where the harness keeps its data (the `data volume` section above).
`local-offload models` prints the resolved tier routing table. Both are the fastest way to tell a
serving problem from a harness problem.

## Testing notes

`detect.ps1 -SelfTest` covers classification (including the AMD VRAM banding and Adrenalin
version classifier). `setup/tests/` carries PowerShell tests for config-seed behavior and for the
canary pure helpers (`selftest-canaries.test.ps1` — word-overlap, flash-attn log-state scan with
live-captured log lines, cosine). Go-side config round-tripping is covered by
`example_config_test.go` and `doctor_test.go`, which also guard against tier-key drift between
`config.example.json` and the code. The second-build flag is pinned by `internal/servingtmpl/eg2bin_test.go` (what the
rewrite touches and refuses, on all ten templates), `install_render_eg2bin_test.go` (the flag, the floor check, the replay),
`setup/install.tests.sh` and the `OFFLOAD_EG2_LLAMA_BIN` cases of `setup/render.tests.ps1` and
`setup/tests/install-config-seed.test.ps1`.

## Common pitfalls

- Assuming `f16` KV cache everywhere. It is the minority.
- Assuming a flash-attention exception for speech. There is none in these templates — a whisper
  entry is never baked in. It arrives from the TIER: a `media_seats` entry of kind `stt` renders a
  whisper-server seat (with its own loader path, since it is a separate binary) into the models map,
  and writes `stt_model` at the same time. A tier that declares no seat leaves `stt_model` empty and
  the route defers — it does not name an upstream nothing serves.
- Expecting a second vision seat to bind. A tier may declare at most one seat per bound key, but a seat
  flagged `extra: true` (a registered extra, e.g. blackwell-8's `lfm2.5-vl` and `gemma4-e4b-vision`) writes no
  key and is not counted: it renders into llama-swap with its aliases and answers to them, and nothing
  routes to it by default. A rendered extra runs the template's flag shape (`--reasoning off`, `-ngl 99`, the
  tier's KV type), which can differ from a hand-wired entry of the same model, so the seat's `measured` note
  says "rendered form not yet re-measured" until an EN/ES OCR and spatial VQA pass has been run on it.
- Adding a profile without its self-test assertion.
- Expecting the `ampere-8` band to start at 8 GB. It starts at 7.
- Treating the profile string as fleet routing input. It is not.

## Source map

- [`setup/detect.ps1`](../../setup/detect.ps1) — `Get-Profile`, self-test matrix
- [`setup/install.ps1`](../../setup/install.ps1) — asset install and template substitution
- [`setup/selftest.ps1`](../../setup/selftest.ps1) — the receipt
- [`setup/templates/`](../../setup/templates/) — per-backend serving templates and `profiles.json`
- [`setup/SETUP-AGENT.md`](../../setup/SETUP-AGENT.md) — the agent runbook

## Related docs

- [../architecture/decisions/0002-grammar-reliable-serving-flags.md](../architecture/decisions/0002-grammar-reliable-serving-flags.md)
- [../architecture/decisions/0010-tier-optimization-before-latency-defer.md](../architecture/decisions/0010-tier-optimization-before-latency-defer.md)
- [../OPERATOR-GUIDE.md](../OPERATOR-GUIDE.md)
