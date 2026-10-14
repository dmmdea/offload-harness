### Fixed - the installer stops writing the every-expert `--cpu-moe`: a CUDA box off the tier matrix renders no 26B, and the selftest no longer edits the yaml

- **The defect.** INV-1: the cards do the inference and host RAM is overflow only, so the one sanctioned expert placement in RAM is a
  MEASURED partial `--n-cpu-moe N` up to the tier's `n_cpu_moe_max`. The rule audit (`servingtmpl.AuditCPUMoE`, `renderGate`,
  `audit-yaml`) shipped in 0.184.0 and refuses the bare `--cpu-moe` (every expert layer in host RAM), but the installer still wrote it in
  five places, and said so under "Not changed (needs a decision)": (1) `fallbackProfile` rendered `--cpu-moe -ngl 999` plus the 26B for
  every `--fallback-backend cuda|cuda-resident|dual-cuda` render, which is what `install.ps1` asks for on a box with no recognised tier;
  (2) `moePlacement` returned the bare form for `moe_26b: cpu_moe` on any backend; (3) and for `n_cpu_moe` naming no N ("rather than emit
  a broken flag"); (4) `selftest.ps1`'s `Invoke-Remediate26B` inserted ` --cpu-moe` into the live `llama-swap.yaml` on a 26B allocation
  failure, the only one of the five that wrote onto a real file; (5) the selftest's `Invoke-ProbeLoad -CpuMoe` launched a standalone 26B
  with the experts in RAM, from step 3 (the 26B decode number), the cold-swap probe and, unnoticed until now, the ctx probe of step 2:
  `$residentIsMoe = ($residentId -eq 'gemma4-26b-a4b')` is true on the nine tiers whose resident is the 26B, so the Windows selftest
  recorded "ctx MEASURED ok" for a placement the yaml does not serve. `install.ps1` held a dead PowerShell shadow of (1)-(3)
  (`$defaults.cuda.moe`, `cpu_moe`, and `n_cpu_moe` with no N turned into `cpu_moe`) that nothing consumed except `include_26b`, which
  decided a 14 GB download for a seat the render then served on the wrong terms.
- **The decision (D1, option a, "stop").** An unmeasured placement gets no flag at all. An off-matrix CUDA box has no tier, no
  measurement and no declared fit, so it is not claimed to hold the 26B on its card (the same outcome as `ampere-6`, `ampere-8` and
  `blackwell-8`, which are `moe_26b: drop`); the alternative, `-ngl 999` with the 26B kept, is an entry that fails at load on any small
  card. The cpu backend has no card to overflow from and keeps its exemption (its `--cpu-moe` text is inert: no cpu template carries the
  token in a command). The tier notes and template header prose that still say `cpu-moe` are not touched, because editing a tier entry or
  a template re-stamps every file on it (see Not changed).
- **The fix, renderer (Go).** `fallbackProfile` returns `moe_26b: drop` for the CUDA family (ctx, KV and flash-attn unchanged); `vulkan`
  keeps `-ngl 999` and `cpu` keeps its inert literal. `moePlacement` returns the every-expert form for the cpu backend only, and nothing
  for `n_cpu_moe` with no N (it used to fall back to the every-expert form). New `moeDefect`, called in `deriveRender` before any flag is
  computed, refuses a tier that includes the 26B and names `cpu_moe` on a backend with a card, or `n_cpu_moe` with no N, by tier name,
  INV-1 and what to use instead, at any RAM tier (before: a `min` box silently lost the 26B, and the others were refused by the write gate
  after the flag was computed). `spillViolations` loses its no-N clause (`moeDefect` owns it); `cpuMoeExempt` is the cpu backend only (the
  `moeLiteral` half was a concession to the installer writing the literal); `auditCPUMoEFile` turns a verbatim off-matrix render that
  still carries the flag, which only a binary at or before 0.185.1 wrote, from a `note:` into a violation whose text says to re-render.
  The first walk of `win-cuda-resident` and `win-dual-cuda` with the 26B dropped (no table tier does that on those two templates) renders
  cleanly. Comments only in `internal/servingtmpl` and `internal/hwdetect`.
- **The fix, `selftest.ps1`.** `Invoke-Remediate26B` is deleted. A 26B tier that fails to load is recorded `status:"fail"` with
  `tiers[].detail` (a new field, `null` on every other row, documented in `SETUP-AGENT.md`) from the pure `Get-MoEFailureAdvice`: RAM is
  overflow only so selftest moves nothing and edits no yaml; set the tier's `moe_26b` to `drop` (a tier whose `resident_tier` or agent seat
  is the 26B needs those changed too, or pick a tier whose resident is not the 26B, because `TestNo26BAgentSeatOnRAMDroppable26B` refuses
  the drop alone), or declare a measured `n_cpu_moe` with `n_cpu_moe_max`, then re-run the install. `Invoke-ProbeLoad` loses `-CpuMoe`
  everywhere (the ctx probe, the cold-swap probe, step 3); step 3 and the `moe_full_offload` canary are retired to a fixed
  `skipped` receipt entry (the key stays, the shape does not change; the canary trial was unreachable on every shipped tier, since the
  only tier with the 26B and a non-gpu placement is `cpu`, whose backend skipped it). `receipt.remediations` keeps one writer, the ctx
  downshift.
- **The fix, `install.ps1`.** The unknown-profile fallback table drops the `--cpu-moe -ngl 999` literal and the 26B include for `cuda`
  (`include_26b` false: the 14 GB download follows), mirroring `fallbackProfile`; a tier naming `cpu_moe` on a card backend, or
  `n_cpu_moe` with no N, throws by name at any RAM tier (mirroring `moeDefect`); and `Test-YamlHasEveryExpertFlag` joins the Step 6 SKIP
  probe, so re-running the installer over a yaml an older installer rendered (which passes every other probe) re-renders it instead of
  keeping the flag the audit refuses. The detector's pattern is a character class (`--cpu[-]moe`) so the source pin does not read the
  detector as a writer. Comments only in `install.sh` and `detect.ps1`.
- **Behaviour changes to know about.** An off-matrix CUDA box gets no 26B (and downloads none); `audit-yaml --against-render` on a render
  made by 0.185.1 for such a box reports the `cpu-moe` violation with the re-render text and `STALE(params.include_26b, params.moe_26b)`.
  The Windows selftest's ctx probe now loads the card-resident 26B at the projected window on the nine tiers whose resident is the 26B,
  so it can OOM and take the downshift path (`remediations[]` then holds `downshift ctx a->b`, never an edit). A card too small for the
  26B (`amd-rdna3-dgpu` on 12 GB is the case the old `R4.2` text named) now fails its 26B tier with advice instead of limping on
  `--cpu-moe`; there is no automated path past it, and the advice says so.
- **Docs.** `setup/SETUP-AGENT.md` (tier table cells, the AMD chapter without the `--cpu-moe` floor and the full-offload promotion row,
  the receipt schema, the failure tables), `docs/OPERATOR-GUIDE.md`, `README.md`, `docs/systems/setup-installer.md`, ADR 0043 (the W4
  bullet is amended: no second exemption; "what stays open" is closed), and `_fields.moe_26b` in `profiles.json` (not rendered into
  `docs/tiers`; no tier entry changed).
- **Not changed.** (1) The tier notes in `profiles.json` that still mention `cpu-moe` (`amd-rdna3-dgpu`, `dual-gpu`, `ampere-8`,
  `blackwell-8`, `ampere-6`, `amd-rdna3`, `cpu`) and the header prose of the `win-cuda`, `win-vulkan`, `win-dual-cuda`, `linux-vulkan`
  and `linux-cpu` templates: editing either re-stamps every file on that tier or template (STALE until `install render` is re-run) and
  regenerates `docs/tiers`, so they ride a release that already touches them. (2) The `cpu` tier keeps `moe_26b: cpu_moe` and its inert
  `--cpu-moe -ngl 999` stamp parameter; changing the string would STALE every stamped cpu-tier file. (3) The vulkan off-matrix fallback
  still keeps an unmeasured 26B at `-ngl 999` (never an expert flag; the same reasoning as the CUDA family, left for a decision).
  (4) `audit-yaml` still never applies the spill ceiling (`AuditSpill`) to a live file. (5) The fleet read-back of W4's own acceptance
  (re-running the audit over every node's live yaml with this binary) is not part of this branch and was not run.
- **Tests, red first.** The seams (`Get-MoEFailureAdvice`, `Test-YamlHasEveryExpertFlag`) were committed behaviour-free, then the tests,
  seen failing on their assertions against 0.185.1: `TestOffMatrixFallbacksRenderNoEveryExpertFlag` (the cuda family carried the flag and
  the 26B on every OS and RAM tier), `TestMoePlacementNeverReturnsTheEveryExpertFormOnABackendWithACard`,
  `TestDeriveRenderRefusesAPlacementThatWouldPutEveryExpertInRAM` and `TestDeriveRenderKeepsTheSpecificNoNRefusal` (derive accepted
  both defects), `TestCPUMoeExemptIsTheCPUBackendOnly`, `TestRenderGateExemptsOnlyTheCPUBackend`, `TestRenderedTiersCarryNoBareCPUMoE`,
  `TestAuditYAMLReadsTheExemptionFromTheStamp` (a legacy off-matrix render was a note and exited 0), the source pins
  `TestInstallerSourcesWriteNoEveryExpertFlag` and `TestSelftestHasNoCpuMoeSwitchAndNoRemediator` (20 and 12 offending lines), the
  inverted last block of `TestInstallRenderRefusesNCPUMoEAboveTheTiersMeasuredSpill`; and in PowerShell `selftest-no-cpumoe.test.ps1`
  (renamed from `selftest-remediation.test.ps1`: 26 assertions) and 14 new assertions in `install-config-seed.test.ps1` (the unknown-profile
  fallback, both throws, the SKIP probe's detector against yaml fixtures, the selftest source pins), plus `render.tests.ps1` for the
  unknown-profile render.
- **Mutations.** Each rule has a single-line mutant proved RED with `scripts/mutation-check.sh` (the mutation lands on one line, the
  unmutated tree passes first): the CUDA fallback back to the literal plus the 26B; `moePlacement` returning the flag for `cpu_moe` on any
  backend, a flag for `n_cpu_moe` with N=0, and nothing for a named N; the `moeDefect` call deleted, each of its two conditions flipped and
  its `Include26B` guard removed; `cpuMoeExempt` regaining the literal and losing the cpu backend; `audit-yaml` turning the finding back
  into a note and losing the off-matrix discriminator; a `-CpuMoe` argument, a `Invoke-Remediate26B` function and the `--cpu-moe` literal
  restored in the scripts (Go source pins); and in PowerShell the cuda fallback's include and flag restored, either throw disabled, the
  SKIP-probe clause removed or stripped of its cpu exemption, the detector reading comments, the advice call removed and the advice
  naming the flag.
