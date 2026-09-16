---
status: Accepted
date: "2026-09-16"
---

# 0047 — The ampere-16 agent seat is re-audited blind; the 4B verdict behind ADR 0029/0035 is void

> **Accepted, with the default binding HELD.** The seat DECISION is settled and unanimous: Qwen3.8-27B UD-IQ3_S
> with its embedded MTP head beat the incumbent 4B 24 of 24 blind judgements. The DEFAULT BINDING is not yet
> that seat, because the live check found it cannot finish a default 300 s delegated contract and no node-side
> setting can extend one. The entry renders and is callable by name with a longer `timeout_sec`; the binding
> flips when the wall is derived from the seat rate (register D-03). See "Live check" below.

## Context

The ampere-16 tier (reference box: Lenovo M720q, NVIDIA A2 16 GB, 15,356 MiB usable with ECC on) has run a
4B agent seat since 2026-08-17 — first `qwen3.5-4b-agent` (llama.cpp, [0029](0029-lenovo-agent-lane-stays-on-the-4b-seat.md)),
then `qwen3.5-4b-vllm` (vLLM w4a16, [0035](0035-persistent-vllm-seat-behind-llama-swap.md)). Register A-07
recorded on 2026-09-14 that "the measured tie stands" and that no larger candidate would be funded until a new
model family shipped.

On 2026-09-15 the operator voided that verdict ("a 4B model is ridiculous on a 15 GB GPU … previous testing has
been biased and contaminated"). The audit of the prior records — digested by the fleet seats from the records
themselves (`Benchmarks and Optimizations/2026-09-16-a07-ampere16-seat-reaudit/`) — found the contamination
the operator named, each item with a number in the record:

1. **The card ran at stock and thermally throttled from 2026-09-10 16:25 to 2026-09-15 10:20** (register A-103):
   a 2-minute reboot inside the powertune dwell left the crash cookie in place, so the accepted 40 W / 1200 MHz
   profile was skipped at every boot. Under load the passive card reached 86 °C and `sw_thermal_slowdown` within
   60 s and sat at 232–980 MHz. Every Lenovo measurement in that window — including instrument run 2 of
   2026-09-14, the run that produced the "tie" — was taken on a throttled card. Contract walls of 300 s were hit
   by the larger candidates, and a wall is a completion failure the instrument counts against the candidate.
2. **The Lenovo node ran a 1,024-token step budget** (no `agent_max_tokens` in its config; the Qube's had been
   raised to 4,096 on 2026-09-04). A Qwen3.5-9B spent 100 % of 1,024 tokens thinking and returned no answer; the
   loop's once-per-run 4× raise blunts but does not remove the bias (CORRECTION-2026-09-08). The published
   −1.53 against the 9B was withdrawn as unsupported; the re-run at a matched budget read −0.11.
3. **The 2026-09-04 bake-off scored shape, not quality** (`deferred == false and len(findings) >= 3 and
   len(summary) > 40`) and then separated the seats on wall time, which INV-5 forbids as a criterion.
4. **The 12B was only ever measured at a 32k window** (register A-41) and never with its MTP drafter; the
   9B-under-llama.cpp "failure" was a timeout at ~10 tok/s, not a quality result; the 12B-under-llama.cpp arm
   was judged with thinking on against a 4B with thinking off in the four-way table.
5. **The instrument itself was measured to saturate on big models** (register G-36), and the H-04 script was
   only verified end to end on 2026-09-14 — on the throttled card.

None of these invalidates the instrument; all of them invalidate the *conditions* under which the 4B "won".

## The re-audit (what was measured, and how)

Register A-07 re-run on the H-04 instrument (`stage3d-drivers/quality_instrument.py`, blind packets, three
Opus lenses — faithfulness / substance / usefulness — Latin-balanced order, exact-bytes ground truth, the
sha-pinned `contracts/digest-8.json` set) with every confound above removed:

- the card verified on the accepted profile (`power.limit` 40 W read back) and **cooled to ≤ 60 °C before every
  arm**, a 10 s thermal sampler running for the whole pass;
- **matched policy on every arm**: thinking off, step budget 4,096, wall 900 s, the same sampling, one contract
  per delegate call (`--serial`, added to the instrument for this pass) so a single-slot llama.cpp arm is never
  measured with three loops queued on it;
- the production 4B vLLM line as the **control**, not a candidate — the operator's rule is that a 4B may not be
  the seat on a 16 GB card, so the 4B is the floor a candidate must clear, never the winner;
- candidates, all on llama.cpp b10991 (0.4.1-dev, built on the box in a separate directory; the production
  b10454 binary untouched): Qwen3.8-27B UD-IQ3_S with its embedded MTP head (`--spec-type draft-mtp`, the
  GGUF carries `blk.64.nextn.*`), Gemma 4 12B QAT UD-Q4_K_XL with the Q8_0 MTP drafter (f16 KV — q8_0 KV is
  documented to zero the drafter's acceptance), gpt-oss-20b Q4_K_M with the EAGLE-3 drafter, and Qwen3.8-27B
  UD-IQ3_XXS with MTP as the quant/context axis of the 27B.

Speed (W1 tok/s, c4 aggregate, MTP acceptance) was recorded as a side column per INV-5 and never ranked on.

## Operational fit — the wall, stated before the verdict

Quality decides the seat (INV-5), but the seat has to run inside the harness's own budget, so the arithmetic is
recorded here with the measurement rather than discovered after a merge. A fleet node clamps a delegated
contract to `AgentTimeoutSecDefault = 300 s`, cap `AgentTimeoutSecCap = 900 s` (`internal/core/agentwire.go`,
clamped at ACK in `DecodeAgentContractWithCap`), and the executing seat fits its FINAL answer to the remaining
wall at its measured rate (`final_budget_fit`). Single-stream rates from the probes, against a 4,096-token final:

| arm | W1 tok/s | seconds for a 4,096-token final | gate walls at the 900 s wall |
|---|---|---|---|
| `a2-4b-vllm` | 12.3 | ~333 | 13–26 s |
| `a2-12b-qat-mtp` | 23.2 | ~177 | 20–38 s |
| `a2-oss20b-eagle3` | 28.4 | ~144 | 18–35 s |
| `a2-27b-iq3s-mtp` | 6.3 | ~650 | 111–806 s |

The 27B is the only candidate whose final answer alone can exceed a default 300 s contract; at the 900 s cap it
completed 8 of 8. **The mitigation named in the first draft of this section — "the node's `agent_timeout_sec`
raised" — does not work, and the live check below proves it**: that key is read only on local-run paths, so a
dispatched contract keeps the delegator's 300 s regardless. The wall is therefore not a knob this tier can turn;
it is the reason the binding is held. This is a cost of the seat, not a reason to rank on
speed: the same instrument that forbids ranking on latency (INV-5) is why the trade is written down instead of
being allowed to pick the winner quietly.

## Decision

**The ampere-16 agent seat becomes Qwen3.8-27B UD-IQ3_S with its embedded MTP head, served by llama.cpp at a
49,152-token window with a q8_0 KV cache.** The 4B stays defined as the fallback and is never again the seat.

The blind instrument (24 Opus judgements, three lenses, Latin-balanced, matched budget, sha-sealed) separates the
classes unanimously:

| arm | OVERALL | vs the 4B, head-to-head | 1st / last of 24 |
|---|---|---|---|
| Qwen3.8-27B UD-IQ3_S + MTP | **9.32** | **24 of 24** | 18 / 0 |
| Qwen3.8-27B UD-IQ3_XXS + MTP | 8.92 | 23 of 24 | 6 / 1 |
| gpt-oss-20b + EAGLE-3 | 6.21 | 18 of 24 | 0 / 3 |
| Gemma 4 12B QAT + MTP | 6.17 | 17 of 24 | 0 / 6 |
| Qwen3.5-4B w4a16 (incumbent) | 5.39 | — | 0 / 14 |

The incumbent takes zero first places and fourteen last places, 3.93 points behind the winner, with every lens
agreeing. The operator's objection of 2026-09-15 is upheld on measurement, not on assertion.

The instrument reports a TIE for the two QUANTS of the winning model (gap 0.40 < 0.50; head-to-head 18/24 = 0.75,
which passes; every lens agrees). Per INV-6 that is a measurement failure **for that pair** and is carried as an
open item — it does not touch the seat decision, which is between model classes. IQ3_S ships on quality grounds
only: faithfulness 9.40 vs 8.64 (the pair's largest lens gap), 18 firsts and no lasts, two genuine inventions
found in IQ3_XXS by the independent mechanical screen and none in IQ3_S, and IQ3_XXS's sole rationale — a larger
window for 1.1 GB less weight — measured void on this card (81,920 aborts at load; 57,344 held, against IQ3_S's
49,152). No speed term enters the choice, per INV-5.

Two costs ship with the winner and are stated rather than discovered later: it **pads** (102 degenerate findings
flagged against the runner-up's 1 — contracts should ask for a bounded list), and it is **slow** (6.3 tok/s), which the live check
below turned from a cost into a blocker for DEFAULT contracts: the seat needs a caller-supplied `timeout_sec`,
and no node-side setting can supply it. Neither is a reason to prefer a thinner answer, but the second decides
what ships as the default today.

Measured fit on the reference card (NVIDIA A2 16 GB at the 40 W / 1200 MHz lock, cooled to ≤ 64 °C before the
arm): 14,410 MiB resident **including** the ~456 MiB memory-stack embedder that was already on the card — so the
seat coexists with the mem0 embedding lane rather than evicting it. Drafter acceptance 0.592 on free text
(244 of 412 drafted tokens), prompt processing 76.5 tok/s, 8 of 8 contracts with zero deferrals at walls
111–806 s.

## Live check — the winner does not fit a DEFAULT contract, and the binding is held

Deployed to the reference box the same day and checked with the harness's own
`contracts/digest-8.json` through the fleet node, which is how production actually reaches the seat. It failed:

| run | result | walls |
|---|---|---|
| as deployed (`agent_thinking` unset → auto) | **2 of 8**, 5 deferred `wall timeout after 300s` | 279–315 s |
| at the measured policy (`agent_thinking: off`) | **0 of 8** | 297–315 s |
| the 4B seat, same set, after reverting | **8 of 8**, 0 deferred | 36–132 s |

**Root cause, traced in code rather than guessed.** A delegated contract's wall is stamped by the DELEGATOR
before dispatch: `internal/delegate/intake.go` sets `TimeoutSec = core.AgentTimeoutSecDefault` (300) when the
caller passes none, the node's `DecodeAgentContractWithCap` leaves a positive value alone, and
`internal/pipeline/agenttask.go` enforces it as a context deadline. `Config.AgentTimeoutSec` is read in exactly
two places — `cmd/local-agent/main.go` and `internal/mcpserver` `agentTimeout` — **both on local-run paths**. So
no setting on the executing node extends a remote contract's wall, and a seat decoding at 6.3 tok/s cannot finish
one. Seeding `agent_timeout_sec: 900` on the node, which this ADR originally claimed would give the seat room,
does nothing for dispatched work. That claim was wrong and is corrected here.

**What this does and does not change.** It does not touch the quality verdict: 24 of 24 blind, on a matched
budget both seats could finish, stands exactly as measured. It changes what ships as the DEFAULT. The entry
renders, its weights download, and any caller may name it with `timeout_sec` up to the 900 s cap — that is the
configuration it won under. `config_seed.agent_model` stays on the llama.cpp 4B until the contract wall is
derived from the seat's measured rate (`seatrate` already computes `wall_estimate_sec` / `min_turn_sec` and
publishes them, and by design never imposes them — register D-03).

**Operational note.** The live node ran the 27B binding for roughly twenty minutes before the revert, during
which delegations to it deferred. Reverted to `qwen3.5-4b-vllm` and re-verified 8/8.

## Consequences

_(the winner's identity is filled from the aggregate; the mechanics below are the same whichever llama.cpp arm
wins, and were established before the verdict so the decision cannot be shaped to fit an easy deploy.)_

**Where the seat is declared.** `setup/templates/profiles.json` → `profiles["ampere-16"]` is the single source:
the `vllm_seat` object (id `qwen3.5-4b-vllm`, `agent_ctx_tokens` 131072, `fallback_agent_model`
`qwen3.5-4b-agent`) and `config_seed.agent_model`. A llama.cpp winner means a NEW gated entry in both CUDA
serving templates, declared exactly as the Qwen3.5-9B seat is: a tier flag `include_*` on `servingProfile`
(`install_render.go`), its twin on `servingtmpl.Params`, copied in `deriveRender`'s params literal; an
`__…_ALT__` token so the set expression, the matrix var and the block are stripped together when the flag is
false; and a `dropModel` entry so a tier that does not declare it renders nothing. The 4B and 9B entries both
claim the `agent-seat` alias and are mutually exclusive by `validate()`; a third agent entry joins that rule.

**The two gates that go red, and why that is correct.** `TestAReasoningSeatSeedsItsCompletionBudget`
(`agent_budget_test.go`) iterates tiers whose `vllm_seat` declares a `reasoning_parser` and fails when the guard
matches nothing — ampere-16 is the tier it was written for. `TestEveryDeclaredVLLMSeatValidates`
(`vllmseat_table_test.go`) owns the rule that `config_seed.agent_model` must name the FALLBACK, never the vLLM
seat id. Both must be re-pointed in the same change, not deleted: the first at whatever tier still declares a
reasoning seat, the second at the new binding. `TestAgentWindowMatchesWhatTheAgentSeatServes`
(`agent_window_test.go`) then governs the new seat: for a tier with no `vllm_seat` it reads the gate flags, and
`agent_ctx_tokens` must equal the entry's literal `--ctx-size` (or `ctx_size` when the entry carries `__CTX__`).
The measured window therefore ships as a number in three places that the test holds in agreement.

**The engine.** The MTP drafter needs llama.cpp **b10991** (`--spec-type draft-mtp` with a Gemma-4 head needs
b9549+; the Qwen `nextn` path needs the mainline MTP support). The Lenovo's production build is **b10454** and
20 llama-swap entries reference it by absolute path. The deploy is therefore surgical, not a fleet-wide bump:
the b10991 `bin` directory (87 MB, binaries + `libggml*`/`libllama*`) is copied into the offload-stack build tree
beside the incumbent, and ONLY the new agent entry points at it, with its own `LD_LIBRARY_PATH`. A full bump of
the other 20 entries is a separate register item with its own re-verification (register A-89 names the ritual:
a bump must touch the `${server}` macro and every launcher that hard-codes the path).

**Live deploy (the Lenovo's llama-swap is hand-maintained, not rendered).** Three files, each backed up beside
itself: `apps/llama-swap/etc/config.yaml` (the new entry + its aliases, `ttl: 300`, heavy-group membership so it
cannot co-reside with the vision seat), `apps/offload-stack/etc/config.json` (`agent_model`, `agent_ctx_tokens`),
and the vLLM seat's unit only if the 4B stops being the fallback. Readback is `/fleet/health` reporting the new
`agent_seat` and its window, plus one live `contracts/digest-8.json` through the fleet node. `audit-yaml` must
still read OK (INV-2: `ttl: 300`, no persistent group, no preload).

**What happens to the 4B.** It stays defined as the fallback — `fallback_agent_model` and the llama-swap entry
both remain, so a box without the new weights still serves an agent lane, and a revert is one config edit plus a
node restart. Nothing is deleted.

**Re-eval triggers.** A new model family shipping for the 16 GB class; the chassis cooling returning to its
2026-09-04 state (the card still reaches its 86 °C limiter about three minutes into sustained load, so today's
walls are a floor, not the seat's ceiling); a llama.cpp release that changes MTP acceptance materially; and the
Ornith 1.5 35B-A3B arm, which needs an expert spill on this card and is measured separately.

## Frontier note

The fleet's digest of the September 2026 rankings for a single 16 GB card (llm-stats, Artificial Analysis,
localllm.in, atomic.chat, the vLLM recipe) put Qwen3.8-27B first for general agentic work (wins 4 of 4 shared
benchmarks against Gemma 4 26B-A4B; Intelligence Index 52), gpt-oss-20b as the "professional reliability"
pick, and a new MIT-licensed agentic MoE — Ornith 1.5 35B-A3B (DeepReinforce, 2026-08-19, `qwen35moe`
architecture, 3B active) — as the coding-agent contender; its only sub-16 GB GGUF (13.67 GB) needs an expert
spill on this card, so it is staged as a follow-up arm rather than a candidate of this pass.
