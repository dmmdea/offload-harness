### Added - ADR 0089, Proposed: Strata is an opt-in agent-seat backend that earns its place through the gate

- **What it rules.** Strata, a local MoE inference engine, is a candidate for the agent class only: never a text-cascade rung
  (ADR 0027 stands), and it displaces no declared `vllm_seat` (ADR 0048). Every tier gets a dormant slot (`strata_seat` or
  `strata_debt`), rendered and seeded by one predicate (`tierseed.StrataEligible`, the ADR 0080 pattern). Its inputs (NVIDIA
  driver 580 or newer, compute capability 7.5 or newer, 12 GB of VRAM with 8 GB a slow opt-in, AVX2, host RAM by quantization
  arm) were re-read from the project's install and model guides on 2026-10-14 and are hard-coded only by the predicate change,
  each with its `source` and `verified_at`; the spike pin is release v0.1.44.1, re-read at spike time. Only `blackwell-3x16`
  names a checkpoint.
- **The reversal it records.** The default checkpoint (Flash-Next GSQ-RCO IQ3_S, experts spilled to host RAM) is the shape release
  0.115.4 removed. It is admitted under the operator's later standing rules of 2026-10-07 (spill only for a model that does not
  fit its card, three measured conditions) and 2026-10-10 (RAM-dependent services allowed), and only if the spike measures that
  Strata's split matches the clause: attention and dense layers on the cards, only expert matmuls on the host, per-process CPU
  share within 10 percentage points of the `--n-cpu-moe` arm. The rule also keeps the KV cache on the cards, which Strata's
  default does not at 64K of context and above, so the clause arm pins `--kv-streaming off` and every arm records where the KV
  cache lives. The Coder arm is the fallback, but its pre-spike VRAM estimate was for all three cards, not the pair, so it is
  rule-clean only if the spike measures the fit. It is recorded as applied by default under the
  operator's 2026-10-13 instruction and confirmed on 2026-10-14 (his answer on the weights' licences: "internal-use measurement
  OK"); the ADR stays Proposed. The measured history (18/18 at 1828 s against 271 s; the 20-contract production path 15/20
  against 20/20) is cited as history and is never a gate.
- **Shapes and exceptions.** The spike is hand-launched, lease-gated, outside llama-swap, with `idle_unload_s=300`; the production
  shape is `internal/strataseat` plus `install_strataseat.go` with `ttl: 300` and a generalized seat-guard roster. A Strata seat
  launched by llama-swap is a harness seat, the established exception to the no-hosting rule. The pair is resolved by role and
  pinned by GPU UUID prefix at run time, never an index and never a UUID in a public file.
- **Licences and the ladder.** The Qwen Community License 1.0 and the Swift Open License 1.0 are tagged `commercial_use:
  unverified` (an internal-use measurement is accepted; the licence reading is pending), and the tier-default rung needs that
  reading recorded. ADR 0058 is accepted by the operator with a third licence state, `conditional`; its own edit lands with the
  template-route change, not here. Promotion is a ladder (spike, opt-in
  seat, tier default, main seat) through the one measurement gate (ADR 0088) and ADR 0080's resource gates; quality is the only
  gate, wall time is a side column, and the feasibility floor is tagged `speed-lint: allow feasibility floor`.
- **Scope.** Documentation only: one ADR and its index row. No code, no configuration and no seat changes; the follow-up work
  (preflight and spike, production-path A/B, predicate and coverage test, seat package, re-pack lane, promotion record) is not
  built here. ADR 0090 holds the DeepGEMM reopen trigger that the NVFP4 arm of the spike links to.
- **Checks.** `go test -run 'TestDocsLint|TestADRNumbersAreUnique' .` (links resolve, frontmatter, index row), `go run ./cmd/changelog check`,
  the leak-gate scans on the staged files, and the H-03 speed-term scan over the ADR and this fragment, which reports no hit.
