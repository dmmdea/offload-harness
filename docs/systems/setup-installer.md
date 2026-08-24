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
first, because a heterogeneous pair outranks any single-card band:

| Condition | Profile |
|---|---|
| ≥2 NVIDIA GPUs | `dual-gpu` (the only path that sets `big_ram`, when RAM ≥ 120 GB) |
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

Fourteen profiles. Two boundaries are deliberately below their nominal card size: the `ampere-8` band
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
  and `--flash-attn` (on for twelve profiles, off for `amd-gcn`, and omitted entirely by the CPU
  template because that backend has neither `-ngl` nor `--flash-attn`). On `amd-rdna3` the f16/16K
  values are an explicit SAFE FLOOR: the selftest's H3 canary suite (`fa_q8kv`, `ctx_sweep`,
  `moe_full_offload`) measures the q8_0/32K/26B-full-offload promotions on the real box, and the
  installing agent applies them canary-gated per the runbook's AMD RDNA3 chapter.
- **Vulkan device pinning:** every model entry in the Vulkan template carries
  `GGML_VK_VISIBLE_DEVICES=0` so multi-ICD boxes serve from a deterministic adapter.
- **One exemption:** the `embeddinggemma` entry bypasses the shared flag macro entirely, taking
  `--embedding --pooling mean` instead. "All served models get these flags" is therefore false.

See [ADR 0002](../architecture/decisions/0002-grammar-reliable-serving-flags.md).

**Config seeds** bind media models per profile. Tiers at 16 GB and above seed HiDream-O1 bf16 and Wan
2.2 Q8_0. 8 GB tiers gained a **RAM-conditional layer** (J4): `config_seed_ram_mid_high` merges on
top of the base seed only when `ram_tier` is mid/high, so a 64 GB 8 GB box auto-binds what previously
needed manual config. The two 8 GB tiers diverge by operator decision: `ampere-8`'s overlay stays the
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
config. Templates in `setup/templates/` carry placeholders substituted at install time.

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

Selection is pure and unit-tested (`internal/volumes`); only enumeration is platform-specific
(kernel32 on Windows, `/proc/mounts` + `statfs` on Unix), so the policy cannot drift between
operating systems. `--json` emits the full enumeration plus `{volume, because}` for a wrapper to
consume; the console view shows the roomiest few and says how many it withheld.

`because` is meant to be stored with the install, so a later operator can see why the tree is where
it is rather than re-deriving it. Wiring the choice into `install.ps1` (and recording it in
`installed.json`) needs the bootstrap to fetch the binary before it picks a target, and belongs with
the detection move — this verb is the decision engine those wrappers will call.

### Serving config on Linux (`install render`)

Template rendering lived in `install.ps1`, so a Linux node could not produce a serving
config at all — every Linux deployment hand-wrote one, and on the measured 6 GB node the
first two hand-written topologies each broke the box.

```
local-offload install render --profile ampere-6 --home /srv/offload   --llama-bin /srv/offload/build/llamacpp/build/bin   --models /srv/offload/models --listen 127.0.0.1:11436 --out llama-swap.yaml
```

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
group names a model that does not exist.

**Verified end to end on the Linux node:** the rendered `ampere-6` config was handed to the
node's own `llama-swap` on a throwaway port, which accepted it and listed exactly
`offload-e4b`, `gemma4-e2b`, `embeddinggemma`, `bge-reranker-v2-m3` — the 26B correctly
absent. The live service on `:11436` was untouched throughout.

### The Linux install path (`setup/install.sh`)

```
setup/install.sh --bin ./local-offload --llama-bin /path/to/llamacpp/build/bin [--prefix DIR] [--user NAME]
```

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
`env: 'bash': No such file or directory` — a message naming neither the script nor the
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
`ampere-6` (RTX 3050, 6 GB), each matching the tier it actually runs.

> <node-b> read `blackwell-16` (single RTX 5060 Ti, 15.9 GB) until **2026-08-02**, when the
> 5070 Ti was installed. Detection already handles this — `hwdetect.Classify` returns
> `blackwell-2x16` for two Blackwell cards, and its test names this exact pair as the
> reference box — but this sentence lagged, and stale "5060 Ti solo" wording in several
> places led to the box repeatedly being budgeted as a single 16 GB card. It is 32 GB
> across two cards. Corrected 2026-08-05.

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

## Observability and debugging

`local-offload doctor` verifies the serving layer end to end and reports per-alias reachability.
`local-offload models` prints the resolved tier routing table. Both are the fastest way to tell a
serving problem from a harness problem.

## Testing notes

`detect.ps1 -SelfTest` covers classification (including the AMD VRAM banding and Adrenalin
version classifier). `setup/tests/` carries PowerShell tests for config-seed behavior and for the
canary pure helpers (`selftest-canaries.test.ps1` — word-overlap, flash-attn log-state scan with
live-captured log lines, cosine). Go-side config round-tripping is covered by
`example_config_test.go` and `doctor_test.go`, which also guard against tier-key drift between
`config.example.json` and the code.

## Common pitfalls

- Assuming `f16` KV cache everywhere. It is the minority.
- Assuming a flash-attention exception for speech. There is none in these templates — a whisper
  entry is never baked in. It arrives from the TIER: a `media_seats` entry of kind `stt` renders a
  whisper-server seat (with its own loader path, since it is a separate binary) into the models map,
  and writes `stt_model` at the same time. A tier that declares no seat leaves `stt_model` empty and
  the route defers — it does not name an upstream nothing serves.
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
