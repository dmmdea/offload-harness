### Added - two contract pages (the `delegate --contract` file, the dispatch log), stale vLLM debt reasons corrected, a labelled-agent-seat gate (A-20), and an `install render` refusal of a llama.cpp 9B agent seat on ampere-16 (A-40)

- **`docs/systems/delegate-contract-file.md` (register D-70).** The file `local-offload delegate --contract` reads is a bare subtask
  object or an array of at most 8, never the `{"subtasks": [...]}` wrapper the MCP tool takes. A wrapper file does not fail to parse:
  unknown fields are ignored, so it becomes one subtask with a blank goal and intake refuses it with `goal is required`. The page also
  maps the tool's call-level arguments to the flags that carry them, and records the seat id rule behind three failed launches:
  `agent_model` must equal an id the endpoint's `/v1/models` publishes, or every subtask comes back deferred as `infrastructure`
  (counted in `lost_to_stack`) while the contract itself is fine. Held by `TestDelegateContractFilePageNamesWhatTheCLIReads` (the page
  names every field of the `--contract` help's own `(fields: ...)` list) and `TestContractFileWrapperIsNotUnwrapped` (the wrapper trap,
  against the real parser and intake).
- **`docs/systems/dispatch-log.md` (register D-74).** `state/dispatch-log.jsonl` is written by two harnesses (the Claude Code hooks, which
  also rotate it at 1 MiB, and the opencode plugin, append-only, tagged `"harness":"opencode"`) and read by one scoreboard that divides by
  `readonly_spawn` and checks a `session` heartbeat. The page lists every event with its writer, fields and reader, and names the rows a
  rewrite of either writer must keep emitting. `TestDispatchLogContractPageNamesEveryEventTheWritersEmit` reads the `DispatchEvent`
  type in `integrations/opencode/src/instrument.ts` and fails when the page leaves out an event the plugin may write;
  `TestDispatchLogContractIsLinkedFromItsWriters` holds the links from `instrument.ts`, `opencode-integration.md` and `docs/README.md`.
- **vLLM coverage debt reasons (register A-07, coverage).** Four `vllmSeatDebt` reasons were stale: ampere-8 said "9B w4a16 arm never
  run" and blackwell-8 "vLLM arm never measured" although both profiles record that vLLM is not a path for the 9B on 8 GB (the W4A16
  weights alone are 7.51 to 9.35 GiB), and amd-gcn and cpu said nothing of the 2026-09-20 vLLM-CPU run (Qwen3.5-4B bf16 only, 3.1 tok/s
  against 9.27 for llama.cpp CPU, not seated). `TestVLLMDebtReasonsAreCurrent` ties each debt reason to its own profile's recorded
  measurement, so the next one cannot go stale unnoticed.
- **Agent seats are measured or labelled (register A-20).** `TestEveryTierIsMeasuredOrLabelledProjectedForItsAgentSeat` makes every tier
  either MEASURED or carry a PROJECTED label naming the seat it seeds; a projected seed that ends up with a seat a measurement failed
  (the 26B that scored 1/8 on the A2, offload-e4b that failed on amd-gcn) must record the failure. It does not decide whether the A2
  finding generalises to other card sizes; it makes the open question countable. No profile changed.
- **`install render` refuses a llama.cpp 9B agent seat on ampere-16 (register A-40).** The prohibition was measured (the 9B under llama.cpp
  fails on the A2, under vLLM it only ties the 4B) and recorded, never gated. `renderGate` now reads the rendered models and refuses a
  `mimo-9b-agent` or `qwen3.5-9b-agent` entry for that tier, naming the tier and the register row. The 8 GB tiers, which ship those entries, are
  untouched (`TestRenderGateLeavesTheNineBSeatsOfTheEightGBTiersAlone`).
- **Red-first.** Every test above was committed before its page or fix and failed on the unmodified tree: the two page tests on the missing
  pages, `TestVLLMDebtReasonsAreCurrent` on the four stale reasons, `TestAgentSeatLabelCheckRefusesEachDefect` on the empty gate, and
  `TestRenderGateRefusesALlamaCppNineBAgentSeatOnAmpere16` on "the write gate passed it".
- **Not done here.** A-86 (an `audit-yaml` check that residents and singles sit on the intended cards) needs a machine-readable layout
  declaration and the layout it would protect moved when the display card changed, so it waits for that declaration.
