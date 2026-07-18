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
3. **`selftest.ps1`** emits a receipt with verdict `pass | warn | fail`.

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
| AMD RDNA3 | `amd-rdna3` |
| AMD, anything else | `amd-gcn` |
| No usable GPU | `cpu` |

Thirteen profiles. Two boundaries are deliberately below their nominal card size: the `ampere-8` band
starts at **7 GB**, and `blackwell-72` starts at **64 GB** so it covers both 72 GB and 96 GB
workstation cards until larger hardware is actually measured.

`detect.ps1 -SelfTest` asserts this table against **17 synthetic configurations**, plus separate
assertion families for architecture detection, RAM tiering, and unrecognized-hardware warnings.

> **Known coverage gap:** two configurations in the numbered matrix have no profile assertion, and
> `blackwell-8` is only asserted at exactly 8 GB rather than via a low-VRAM fallthrough.

**Serving flags** are split between universal and profile-driven, and conflating them causes real
confusion:

- **Universal on every task-serving entry:** `--jinja` and `--reasoning off`. Omitting
  `--reasoning off` produces empty output. No MTP or draft/speculative flags appear anywhere.
- **Profile-driven:** `--cache-type-k` / `--cache-type-v` (`q8_0` on eight profiles; `f16` on the
  remaining five — the two large-VRAM Blackwell tiers, both AMD profiles, and CPU — K and V always
  symmetric, and `q8_0` for V requires flash-attention on) and `--flash-attn` (on for eleven
  profiles, off for `amd-gcn`, and omitted entirely by the CPU template because that backend has
  neither `-ngl` nor `--flash-attn`).
- **One exemption:** the `embeddinggemma` entry bypasses the shared flag macro entirely, taking
  `--embedding --pooling mean` instead. "All served models get these flags" is therefore false.

See [ADR 0002](../architecture/decisions/0002-grammar-reliable-serving-flags.md).

**Config seeds** bind media models per profile. Tiers at 16 GB and above seed HiDream-O1 bf16 and Wan
2.2 Q8_0; 8 GB tiers stay SDXL-class until O1 on 8 GB is verified on real hardware.

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

## Observability and debugging

`local-offload doctor` verifies the serving layer end to end and reports per-alias reachability.
`local-offload models` prints the resolved tier routing table. Both are the fastest way to tell a
serving problem from a harness problem.

## Testing notes

`detect.ps1 -SelfTest` covers classification. `setup/tests/` carries PowerShell tests for config-seed
behavior. Go-side config round-tripping is covered by `example_config_test.go` and `doctor_test.go`,
which also guard against tier-key drift between `config.example.json` and the code.

## Common pitfalls

- Assuming `f16` KV cache everywhere. It is the minority.
- Assuming a flash-attention exception for speech. There is none in these templates — no whisper
  entry is templated at all; `whisper-stt` is a config alias to a separately provisioned upstream.
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
