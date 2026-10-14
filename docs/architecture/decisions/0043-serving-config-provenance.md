---
status: Accepted
date: "2026-09-14"
---

# 0043 — The rendered serving config carries its own provenance

Release: 0.123.0 (register K-02)

## Context

The llama-swap serving config is rendered ONCE. `local-offload install render` resolves the
box's tier out of the embedded `setup/templates/profiles.json`, substitutes it into the
embedded serving template, and writes the file (`setup/install.ps1` step 5,
`setup/install.sh` step 5). Nothing re-renders it afterwards, and until now the written file
recorded nothing about what produced it: not the tier, not the seed values, not the binary.

So a tier revision lands in the repo and the box keeps serving yesterday's config, silently
and indefinitely. That is not a hypothetical. Register A-39 (0.113.32, 2026-09-07, "install
defaults follow the measurement upward") raised `ampere-16`'s `ctx_size` from 32768 to
131072. The reference node's rendered config was never re-rendered, so it kept serving the
32768 window — and `local-offload audit-yaml` reported `OK` the entire time.

It reported OK *honestly*. `internal/servingtmpl/audit.go` checks RULES: every entry
`ttl: 300`, no `-ngl 0`, no empty `CUDA_VISIBLE_DEVICES`, no persistent group, no preload
hook (ADR H-01, the operator's hard rules of 2026-09-10). A config that is a tier revision
stale breaks none of them. **A rule gate cannot see staleness.** Only a record of what the
file was rendered FROM can.

The same blindness reached the fleet. `/fleet/health` publishes `harness_version`, which
closes the node/repo drift gap for the BINARY — a node several releases behind is visible at
a glance. Nothing said anything about the SERVING layer, so serving drift was found by hand,
one ssh session at a time, and was not found at all when the stale config broke no rule.

A closed-field-set hash already existed in the tree and had already earned its keep:
`internal/agent/props.go` reduces a seat's live `/props` answer to a named, documented
`seatPinBasis` so a paired cross-seat run can REFUSE to compare rows produced under different
serving configs. That is the same problem one layer down, and its design is the one to mirror.

## Decision

**Every config `install render` writes carries a provenance stamp, and `audit-yaml` can
re-derive it.**

**1. A closed, documented input set.** `servingtmpl.SpecBasis` names every input the render
was given: the tier id, the render `Params` (mirrored one field for one field by
`ParamsBasis`), the serving template's sha256, the tier's own `profiles.json` entry
canonicalised and hashed, the two render flags that gate which seeds apply (`--ram-tier`,
`--fallback-backend`), and `buildinfo.Version`. Serialised as canonical JSON — sorted keys at
every level, number literals preserved through `json.Number` — and hashed with sha256. Never
a language-default hasher, never a hash of the raw bytes: a raw-byte hash churns on key order
and on fields that cannot change the output, and both make "what changed?" unanswerable.

`TestParamsBasisMirrorsParams` reflects over `Params` and `ParamsBasis` and fails when the
field-name lists differ. The set is closed *and stays closed*: a field added to `Params`
without being added to the basis would render differently while the stamp swore nothing
changed, which is precisely the failure this ADR exists to end.

**2. Two hashes, because they answer different questions.** `spec_sha256` is the config's
IDENTITY — it was rendered from exactly these inputs. `body_sha256` is over the yaml BELOW
the stamp, and it is what makes a HAND EDIT distinguishable from a seed change: a hand-edited
file keeps an intact spec hash over inputs that no longer describe it. Reporting a hand edit
as "stale" would send an operator to re-render and silently destroy their edit.

**3. The verdict is settled by RE-RENDERING, not by comparing hashes.**
`audit-yaml --against-render` re-derives the config from the binary's own embedded seeds and
compares the RESULT to the stamped body; the basis diff is what NAMES the keys that moved.
MATCH therefore means the verifiable thing: *re-installing this box today would produce this
exact file, and no seed input moved.* Exactly one of four states is reported per file:

| state | means | exit |
|---|---|---|
| `MATCH` | re-rendering reproduces the file byte for byte, and no seed input moved | 0 |
| `STALE(<keys>)` | the seeds moved; the report names the basis keys (`params.ctx_size`, …) | 1 |
| `UNSTAMPED` | no provenance block — rendered before stamping, or written by hand | 0 |
| `HAND-EDITED` | the body no longer hashes to the stamp's record, or the stamp itself was edited | 1 |

**4. `/fleet/health` publishes `serving_config_spec_sha256` and `serving_config_state`** —
NEW KEYS on the EXISTING endpoint, no new route and no new bind — when the new
`serving_config_path` config key names the node's rendered config. A fleet-wide staleness
sweep is then one poll instead of three ssh sessions.

## Consequences

- **The kill criterion is a positive control, not an assertion of correctness.** An audit
  that cannot be MADE to say STALE proves nothing by saying MATCH.
  `TestPreA39AmpereConfigIsReportedStaleNamingCtxSize` reconstructs the pre-A-39 `ampere-16`
  entry from git history (`testdata/profiles-pre-a39-ampere-16.json`, verbatim from 549f360 (machine names scrubbed),
  the commit before the raise), renders and stamps it, asserts the RULE audit still passes —
  that is why a rule gate could not catch this — and asserts the provenance audit reports
  `STALE(params.ctx_size, …)`.
- **STALE names keys, or it is not actionable.** A verdict that says only "different" sends an
  operator to diff two configs by eye, which is the work this row removes.
- **`harness_version` is in the identity hash but excluded from the verdict.** Including it
  would put every node in the fleet permanently STALE after the next release, for a bump that
  changed nothing about the tier. A gate that is always red is a gate nobody reads. A version
  delta is reported as an informational suffix on the MATCH line.
- **The tier's own entry is hashed, not the whole `profiles.json`.** Hashing the whole file
  would mark every node's config changed on every tier edit anywhere in the table, including
  nodes the edit cannot touch — the same false-positive shape.
- **Seeds can move without the rendered text moving, and that is reported separately.** The
  re-derive PINS the per-box inputs a binary cannot know (install paths, listen address,
  thread count, the vLLM deployment half, which `vllmseat.Detect` reads off the local machine).
  A seed those pin can therefore change without the replay exercising it. That case reports
  STALE with `profiles_entry_sha256` named and a detail that says the served config is
  unchanged and only the stamp is behind — never MATCH, because claiming "re-installing
  reproduces this file" when it might not is the certified-stale-as-current failure again.
- **UNSTAMPED does not exit 1.** Every config on the fleet today predates stamping (verified
  read-only on all three reference nodes at 0.123.0: `<node-b>`, `<node-c>` and `<node-a>` all report
  UNSTAMPED). Failing on it would make the session-start audit red on every box from the
  moment this ships. It prints as a finding; STALE and HAND-EDITED fail.
- **One derivation, two callers.** `deriveRender` is called by the renderer and by the replay.
  A second copy of the tier-resolution logic written for the audit is how a gate comes to
  certify the thing it was written to catch.
- **The replay refuses rather than guesses.** A stamp naming no tier, or no target OS, would
  make the re-derive classify the AUDITING machine and compare a node's config against some
  other box's tier. `replayRequest` returns not-ok, and the report is STALE with the reason,
  never a confident wrong answer.
- **Nothing in the harness rewrites a live serving config.** The node only READS
  `serving_config_path`; re-rendering stays `install render`, run by a human. Health omits
  both keys when the path is unset or the file cannot be read, so "this node does not report"
  stays distinguishable from "this node reports MATCH".
- **Cost.** The health verdict costs a re-render, so it is cached on the file's (mtime, size);
  health is polled by every delegator every few seconds. A file replaced with the same size
  inside one mtime tick keeps a stale verdict for that tick — an acceptable price for a config
  a human re-renders by hand.
- **The stamp must not re-introduce a token.** The basis carries a tier's `__OFFLOAD_HOME__` seat paths unsubstituted, because the hash covers the render inputs as given. Written verbatim that put the one pattern a rendered config must never contain back into four tiers' output -- caught by the installer self-test in CI, not by review. The basis line escapes the second underscore of every doubled pair as `_`; the string is unchanged on decode and the spec hash, computed over the unescaped canonical bytes, is untouched.
- **The stamp is inert.** It is a yaml comment block prepended to the render; the body below it
  is byte-identical to what `Render` produced, and the rule audit runs on the unstamped text
  exactly as before. `TestEveryShippedTemplateStampsAndSelfVerifies` renders, stamps and
  re-verifies all eight shipped templates.
- **ADR 0042 is not in this tree.** This ADR takes 0043 by instruction; the gap is intentional.

## Amendment 2026-10-12 (0.184.0; register W4 and A-87)

The rule audit this ADR names as blind to staleness (`internal/servingtmpl/audit.go`) reads three
more things, and the stamp turned out to be the evidence two of them need.

- **The every-expert placement (W4, rule `cpu-moe`).** INV-1 sanctions one host-RAM use for experts,
  a measured partial `--n-cpu-moe N`; the bare `--cpu-moe` (and `-cmoe`, the draft-model forms and
  the `LLAMA_ARG_CPU_MOE` environment twin) parks every expert layer in RAM. The seed gate saw
  `profiles.json` only, so a hand-edited live file could carry it. `servingtmpl.AuditCPUMoE` reads
  parsed entries (cmd, env, macros expanded), never the raw text, because a stamped file records
  `"moe_26b": "--cpu-moe -ngl 999"` in its own comment. It is pure and exemption-free; **its two
  callers hold different evidence and each reads its own.** `install render` runs the gate BEFORE
  the stamp exists (the stamp is prepended after the rule audit), so `renderGate` exempts from the
  resolved profile: the cpu backend (no card to overflow from). `audit-yaml` holds a file, so it
  exempts from a *verbatim* stamp: one whose spec hash is the hash of the basis it carries and
  whose body still hashes to `body_sha256`. A verbatim cpu render is clean; an unstamped file, or
  one edited since its stamp, is judged as a box with a card, and the finding says so.
  *Amended by 0186 w4:* there is no second exemption. The off-matrix defaults used to write the
  literal `--cpu-moe -ngl 999` for the CUDA family, `renderGate` exempted that literal, and
  `audit-yaml` printed a verbatim off-matrix render's finding as a `note:` line. The installer no
  longer writes it (an off-matrix CUDA box renders no 26B), so the literal's exemption is gone and a
  verbatim off-matrix render that still carries the flag was written by a binary at or before 0.185.1:
  it is a violation whose text says to re-render. A stamp that cannot vouch for the body
  exempts nothing.
- **A device pin on identical cards (A-87, rules `pin-by-index` and `pin-unresolved`).** An index is
  the card's place on the PCI bus; on a box with two cards of one model nothing in the config says
  which was meant, and a reordering (this board re-enumerates on power loss) moves the seat
  silently. `servingtmpl.AuditPins` reads each entry as llama-swap runs it against the box's own
  card table (`gpuprobe`, through `audit-yaml`'s `renderCardTable` seam, read only for a file that
  pins a device): an index is flagged when it names a card whose model name another card shares
  (a unique card's index is unambiguous; an entry without `CUDA_DEVICE_ORDER=PCI_BUS_ID` is judged
  by the weaker form, any plain index while two cards share a name), and a pin that names no card
  is flagged always, because CUDA hides every device from the first pin it cannot resolve: a UUID
  that matches no card, an index that no card has (under `CUDA_DEVICE_ORDER=PCI_BUS_ID` the
  nvidia-smi index of a card on the table, otherwise a position past the last card), and a token
  that is neither a plain index nor a `GPU-` UUID (a typo, a bare UUID, a non-ASCII hyphen). `-1`
  and a MIG instance id are not judged.
  **Severity follows who maintains the file.** A seed cannot carry a UUID, so the seeded index pins
  of a verbatim render are the renderer's own output: a `note:`, never a failure, or the only way to
  satisfy the rule would be a hand edit, which turns the file HAND-EDITED and red under
  `--against-render`. The same pin in an unstamped or edited file is its maintainer's choice and a
  violation. With no readable card table the audit says `no card table: pin rules not applied` and
  its exit status is unchanged; it never passes silently.
  **The table is the auditing box's, and the rules are valid only for that box's own file.**
  Nothing in a file or its stamp says which machine it belongs to (the stamp records the render's
  paths and listen address, not the cards), and `audit-yaml` may be run on a file kept on another
  machine. Judged against the wrong cards, its UUID pins fail `pin-unresolved` and the audit exits 1
  on a comparison that means nothing. The default stays the box's own file; `audit-yaml
  --other-box` says the files belong to another machine, skips the pin rules with a `note:` line
  that says why (the card table is not read) and leaves every other rule running. A box id in the
  stamp would let the audit tell for itself, but the stamp is a closed input set that `spec_sha256`
  hashes, so that is a change to its format, left for a decision.
- **The display card's cap is reported, not judged (`display-cap-unseen`).** `gpu_display_cap_pct` bounds the card allocator's
  jobs; a model llama-swap loads onto the display card is outside it, and the yaml states no VRAM footprint, so the audit cannot
  say an entry is over the cap. The pin loop above skips an entry with no `CUDA_VISIBLE_DEVICES` (a `proxy:` seat, an unpinned
  entry), so such an entry used to pass in silence. From the same single card-table read (taken now for a file that pins a device
  or has a `proxy:` seat), `servingtmpl.AuditDisplayCapUnseen` prints ONE `note:` naming the entries that may sit on the card the
  driver flags as the display card (by UUID, so a moved monitor is followed), the display card, the default cap and its budget
  (`gpulease.DisplayBudgetGiB`, the allocator's arithmetic), and that nothing is judged. It is never a violation, never an exit code,
  reads no config, and `--other-box` skips it. The live verdict is the display cap row of `gpu status`. Footprint-based violations
  are deferred until a placement-neutral footprint source exists (`docs/systems/gpu-lease.md`).
- **What stays open.** *(The installer's three writers of the bare form, the selftest R4.2
  remediation, the `install.ps1` unknown-profile fallback and `fallbackProfile` for the CUDA
  family, were removed by 0186 w4; a yaml an older binary wrote reads as a `cpu-moe` finding on its
  next audit and re-renders on the next install, which is the gate working.)* A-86
  (each resident sits on the card its tier intended) needs the tier's layer table, which
  `CheckComposite` has at render and `audit-yaml` does not, and the promotion of the seeded-pin
  note to a failure waits for an installer step that substitutes UUIDs.
- Tests: `TestAuditCPUMoERefusesTheBareFlagInEverySpelling`, `TestRenderGateExemptsOnlyTheCPUBackend`,
  `TestAuditYAMLReadsTheExemptionFromTheStamp`, `TestAuditPinsRefusesAnIndexPinOnIdenticalCards`,
  `TestAuditPinsRefusesAPinThatNamesNoCard`, `TestAuditYAMLOtherBoxSkipsThePinRules`,
  `TestAuditYAMLNotesASeededIndexPinOnAVerbatimRender`.

## Evidence

Register A-39 / commit 5e249dd (0.113.32) raising `ampere-16` `ctx_size` 32768 → 131072, and
549f360 immediately before it. `internal/servingtmpl/audit.go` (rules only — the gate that
passed the stale file). `internal/agent/props.go` `seatPinBasis` (the mirrored design).
Live audit at 0.123.0, read-only, three nodes, all UNSTAMPED. Tests:
`TestPreA39AmpereConfigIsReportedStaleNamingCtxSize`, `TestParamsBasisMirrorsParams`,
`TestSpecHashIsSensitiveToEveryInput`, `TestSpecHashIgnoresWhatIsNotAnInput`,
`TestStampKeepsTheBodyByteIdentical`, `TestAgainstRenderVerdicts`,
`TestEveryShippedTemplateStampsAndSelfVerifies`, `TestFreshRenderVerifiesAgainstItsOwnBinary`,
`TestOffMatrixRenderStampsAndVerifies`, `TestReplayRefusesToClassifyTheAuditingMachine`,
`TestServingConfigReporterCachesOnTheFile`, `TestHealthPublishesServingConfigProvenance`.
