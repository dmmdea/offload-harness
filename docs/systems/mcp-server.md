# MCP server

## Purpose

The Model Context Protocol surface — how a calling agent (Claude Code and equivalents) reaches the
harness. It is the primary consumer-facing interface: most usage arrives here rather than through the
CLI.

## Questions this doc answers

- Which tools exist, and what do they group into?
- What keeps the advertised tool list honest?
- Why does a newly built binary not show new tools?
- Which tool can reach the network?

## Scope

Tool registration, the tool inventory, the stdio transport, the manifest and its drift test, and the
operational lifecycle of the server as an MCP client sees it.

## Non-scope

- What the tools actually do → [offload-pipeline.md](offload-pipeline.md),
  [media-generation.md](media-generation.md), [coding-agent.md](coding-agent.md)
- The CLI surface over the same capabilities → `local-offload` with no arguments prints usage

## Key concepts

**Tool** — one named, schema-described capability offered to the calling agent. **Manifest** — the
declared inventory in `.printing-press.json`, checked against what the code actually registers.

## How the system works

The server runs over **stdio** and registers its tools at startup. A calling agent discovers them,
calls them with JSON arguments, and receives JSON results — including Defers, which are successful
results, not errors.

**Thirty-one tools** are registered on every box, in families. `.printing-press.json` lists all 34
the code can register, and a drift test holds the two together. The advertised set is per-box:
`agent_delegate` and `offload_research` are gated on `agent_delegation_enabled`, and a box listing
an accelerator registers its own — 11 for the Hailo-8L, 4 for the Coral, 3 for the RKNPU (see [accelerators.md](accelerators.md)). Read `tools/list` rather
than any number written down:

| Family | Tools |
|---|---|
| Text offload | `offload_summarize`, `offload_classify`, `offload_extract`, `offload_triage` |
| Vision | `offload_vqa`, `offload_assess_image`, `offload_extract_image`, `offload_video_describe`, `offload_video_watch` |
| Speech / OCR | `offload_transcribe`, `offload_ocr` |
| Forecast (any box; a model-less one forwards to a fleet node; ADR 0098) | `offload_forecast` |
| Media generation | `offload_generate_image`, `offload_generate_video`, `offload_animate_character`, `offload_generate_audio`, `offload_generate_svg` |
| Media editing | `offload_edit_image`, `offload_inpaint_image`, `offload_edit_image_generative`, `offload_upscale_image`, `offload_media` |
| Composition (CPU-class, no GPU lock; ADR 0059) | `offload_compose_video` |
| Graph execution | `offload_run_graph` |
| Workflow templates | `offload_run_template` |
| Agent | `agent_run`, `offload_ask`, `offload_review_diff`, `agent_rig` |
| Delegation (opt-in: `agent_delegation_enabled`) | `agent_delegate`, `offload_research` |
| Browser (opt-in lane; registered only when configured; ADR 0060) | `offload_browse` |
| Remote (opt-in) | `offload_nim` |
| Status | `offload_status` |

**A place in line (`waiter_token`, GPU routing P13b).** Every media tool (`offload_generate_image`,
`offload_edit_image_generative`, `offload_inpaint_image`, `offload_upscale_image`, `offload_generate_video`,
`offload_animate_character`, `offload_generate_audio`, `offload_run_graph`, and `offload_run_template` for its `run` action) takes an optional `waiter_token`. On a
host that leases cards, a call that has no card after its `gpu_wait_ms` does not fail with `gpu busy`: it answers
`deferred`, `err_class: gpu_queued`, `defer_class: capacity`, with `waiter_token`, `queue_position`, `eta_s` (the
declared window left on the lease in the way: a ceiling), `devices` and `held_by` in its data. Re-send the same
request with that `waiter_token` and the call resumes the place it left. The token lives ten minutes after its last
poll and holds its place against later callers for the first 30 seconds of that; see
[media-generation.md](media-generation.md), "Per-card media admission". `offload_run_graph` also takes `min_vram_gb` (the least
size in GB of the largest card of the fleet node that takes the graph; 0 lifts the default floor) and `model_family` (which that
default keys on), both forwarded only when given ([Graphs on the fleet](media-generation.md#graphs-on-the-fleet-adr-0082-amendment-1)), and
`devices` (for the operator's use; nvidia-smi indices or GPU uuid prefixes): absent or several, the graph holds the whole
node; one device runs in that card's own ComfyUI instance, which sees no other card. A host that does not lease cards answers `gpu_busy` as it always did, and
so does any call that did not come through this server (the CLI verbs, the fleet dispatch, the image batch): the
server marks its requests resumable (`core.Request.Resumable`), and only those leave a place in line. A call routed to
a fleet node (`route: "remote"`, or `auto` that leaves this machine; ADR 0077) carries no `waiter_token`: the token
names a place in line on this machine, and a resumed call that goes to a node gives that place up.

**Named media families (ADR 0058).** `offload_generate_image` and `offload_edit_image_generative`
take a `family` param that selects one of the box's opt-in bindings beside its default one; the
edit tool also takes `images` (multi-reference, qwen-image-2.1 families) and both take
`transparent`. `offload_status` lists what a `family` can name under `media.image_families` /
`media.edit_families` (license, `commercial_use`, route verdict). A result from a family whose
`commercial_use` is false carries `license` and `commercial_use:false`. `media.video_family_bindings`
rows carry the same two fields for a video family that declares them (`videogen_families`, null
otherwise). See
[media-generation.md](media-generation.md#named-families-launch-profiles-and-license-tags-adr-0058).

`offload_nim` is the **only remote MODEL surface**. It is an explicit, caller-invoked
side channel and is not part of the Cascade — nothing escalates or falls back into it. See
[ADR 0001](../architecture/decisions/0001-defer-never-cloud-fallback.md). Its key goes only to
NVIDIA's hosted hosts (0.143.1), and a caller-named `base` must be NVIDIA's hosted API,
`nim_endpoint` or a `nim_bases` entry (0.144.2, security standard L5): under `nim_base_policy`
`audit` (the default) any other base runs, the result carries `base_policy`, and a would-refuse row
(scheme, host and port only) is appended to `nim-base-audit.jsonl` in the machine-wide state root the GPU
lease uses (`state_dir`, else `LOCAL_OFFLOAD_STATE_DIR`, else `%ProgramData%\local-offload` or
`/var/lib/local-offload`); under `enforce` it is
deferred before any request leaves.

`offload_browse` (0.141.0, ADR 0060) drives the operator's own browser and is registered only when
the lane is configured (`browse_python`, `browse_script` and a loopback `browse_decision_url`). The
harness holds no key and calls only that loopback decision endpoint, which the operator runs and
which may itself front a hosted decision model, so a run's visible page text and element labels can
leave the machine through it. Every other tool runs local. See
[browse-lane.md](browse-lane.md).

**Door order (register A-102, 2026-09-18).** The four cascade tools (`offload_summarize`, `offload_classify`,
`offload_extract`, `offload_triage`) are the FIRST door for one text + one mechanical question: seconds on the
entry rung, automatic climb to the escalation and reasoning rungs (bound on every node) on a margin, schema or
grounding failure. `agent_delegate` is the door for multi-document read-and-reason with context docs, a schema
and acceptance checks; a contract whose goal is to summarize one file costs a 20–200 s seat run for a 1–5 s
cascade answer and starves the calibration loop of the rows it fits thresholds from. The tool descriptions say
so, because the caller reads them and nothing else.

`offload_classify` and `offload_extract` take an optional `route` (0.154.0, the vision tools' shape; this changed
`tools/list` on every box): `local` (default, byte-identical to before), `auto` (a fleet node runs it only while the
machine-wide GPU lease is held, and never when none is eligible) or `remote` (force a node; defers when none is
eligible). The fleet text lane behind it is dark until a node's tier declares text tasks, so on today's fleets `auto`
stays local and `remote` defers. summarize and triage take no `route`. See
[FLEET-NODE.md](../FLEET-NODE.md#the-text-task-post-fleettext).

`offload_transcribe` takes an optional `route` too (0.165.0, [ADR 0072](../architecture/decisions/0072-a-fleet-node-transcribes-audio-its-caller-uploads-so-a-held-card-is-a-place-in-line.md);
this changed `tools/list` on every box), with one difference from the vision and text tools: **the default is `auto`**,
not `local`. Every fleet node serves the same whisper family, so a spill costs no quality, and a transcription held
behind a render's lease is the failure the route removes (a caller that never passed a route used to wait the whole
90 s gate and get `gpu_busy`). `auto` runs on this box's whisper unless that request would actually be held (a lease
fencing the cards over the model while the model is not already resident, `modelaffinity.WouldBlockUpstream`), in
which case a fleet node that advertises the stt upload door transcribes it, and with none eligible it still runs
local. `local` pins the call to this box (a held card then defers `gpu_busy` with a reason that ends by saying route
`auto` or `remote` would let a fleet node take it, when `delegate_remotes` is configured); `remote` forces a node and,
with none eligible, returns `deferred: true` with `defer_class: capacity` (or `config` with no remotes) without touching
the local GPU. The audio is read on this box and sent as 16 kHz mono Opus; `srt_path`, `text_path` and `json_path` are
always files this box wrote under its own `media_dir`, and `meta.node` / `meta.placement` say where the call ran.
When the transcript carries word times the result also has `words_reliable` and `words_drift_sec`: `false` means the
first word of some segment starts 0.5 s or more away from its segment (issue #169, see
[offload-pipeline.md](offload-pipeline.md)), so audio should not be cut at word boundaries from those times.
`select` still projects the result here, whichever box ran it. `engine: "npu"` is this box's own Hailo sidecar and never
travels: a route named there other than `local` is refused (an omitted route is not a request to travel). The CLI
`local-offload transcribe --route` carries the same vocabulary and defaults to `local`. See
[FLEET-NODE.md](../FLEET-NODE.md#the-stt-upload-door-post-fleetstt).

`offload_forecast` forecasts time series on the box's forecast seat (Timestral-209M, the `forecast_model` binding; see
[offload-pipeline.md](offload-pipeline.md#the-forecast-lane-timestral-209m) and
[ADR 0098](../architecture/decisions/0098-a-forecaster-is-a-llama-swap-media-seat-and-a-box-with-no-forecast-model-forwards.md),
Proposed). It is registered on every box, whether or not the box serves the model, and it also exists as the
`local-offload forecast` verb and as a local-only agent-loop tool with inline series (not in the research profile).
Arguments: `series` (inline numbers, `null` for a missing value) or `series_path` (a `.csv` or `.json` file on THIS box;
for a CSV `target_columns`, `past_covariate_columns` and `known_covariate_columns` pick the columns, and trailing rows
whose target cells are all empty are the horizon and must number `prediction_length`), `prediction_length` (required),
`quantiles` (default `[0.1,0.5,0.9]`, each one of 21 levels from 0.01 to 0.99), inline `past_covariates` and
`known_covariates`, `out_path`, `route` and `select`. The result is the server's object (`shape`, `quantile_levels`,
`median`, `quantiles`, `wall_ms`, `device`). **An answer of more than 2,000 numbers is written as JSON** to `out_path`, or
to `<media_dir>/forecast-<sha8>.json`, and the inline result keeps the other fields plus `result_path`, `median_head` and
`median_tail` (the first and last 8 steps of each series); a write that fails is a capacity defer. A payload the tool cannot
build (a missing column, a horizon mismatch) is a contract defer with `err_class: bad_input`, never a tool error.

Its `route` default is `auto` and `auto` **forwards where the other media tools defer**: it runs on this box when the box binds
`forecast_model` and its card is not lease-blocked, and sends the call to a fleet node that advertises `forecast` when the box
binds no forecast model (an NPU, AMD or CPU-only machine) or its card is blocked. `local` pins the call to this box (a
model-less box then defers as config); `remote` forces a node and, with none eligible, returns `deferred: true` with
`defer_class: capacity` (or `config` with no `delegate_remotes`) without touching the local GPU. The series is read on this
box and travels with the job, and the result file is always written here; `meta.node` / `meta.placement` say where the call
ran. `offload_status` lists a missing forecast model as a gap only when the box has no `delegate_remotes` to forward to. See
[FLEET-NODE.md](../FLEET-NODE.md#the-forecast-task-post-fleetforecast).

`agent_run` drives the coding agent loop. Its default planner is the **agent seat** (config
`agent_model`, else the workhorse `model`; a per-call `model` argument overrides both — and on a
composite box (ADR 0052) the placement table decides when no per-call model is given, its seat
outranking `agent_model`, while a per-call model that belongs to an OPT-IN layer is admitted
only if that layer's guards admit it right now: a dormant layer refuses outright and the
display card refuses by the guard's name, before any seat is touched. Both agent doors take
`context_class: "long"`, an input to placement rather than a seat name; the delegation door's
subtasks also take `layer`, a composite node's declared layer id whose agent seat the subtask
runs on (register A-100: <node-c>'s `fast` layer is its 35B digest seat — a node that does
not declare the layer is ineligible for that subtask, an idle local box that does not declare it
does not keep it (route=auto reads the fleet for it, route=spread never deals it the local slot: register A-108), and with no
node declaring it the subtask defers naming the layer), and every result and
defer on a composite box carries `placed` {tier, layer, role, seat, devices, reason, guard,
evicts} — absent on a plain box), its default
timeout honors config `agent_timeout_sec` (else the built-in 180s), and its result reports the
resolved planner `model` alongside `output`/`steps`/`stop_reason` — visibility is the cure for a
silent seat. **Its admission block is the delegation door's, step for step** — the two run the same loop
on the same seat behind the same llama-swap, and every difference between them so far was found in
production rather than in review (see "Admission" in [fleet-node.md](fleet-node.md) for the ordered list
and the reasoning). Before the cordon, `delegate.ForeignFence` reads the machine-wide lease and a hold that
refuses this process's next run defers `capacity` in milliseconds naming the fence and the holder's declared
window, instead of polling that file for the whole admission budget to reach the same verdict
(register S-26); an INHERITED lease is not a fence. Then the llama-swap **swap pre-flight**
(`pipeline.AwaitSeatAdmission`, register S-25) waits out another session's model swap OUTSIDE the
wall — this door had no pre-flight at all, so an `agent_run` that arrived mid-swap spent its wall inside
llama-swap's silent queue. A seat that is not loaded is then warmed BEFORE the wall, on the same admission budget,
exactly as on the delegation door (D-64), and the result reports the whole block in `admission_wait_sec` /
`admission_note` — one number and one `; `-joined note, never a per-step field. A seat that just
cold-loaded is then asked ONE bounded question before the wall starts — the D-118 coherence probe, ≤ 96 tokens,
`pipeline.ProbeSeatCoherence`, shared with the delegation door — and a seat that answers with the NaN shape defers
`infrastructure` in seconds instead of generating garbage for the whole wall; the verdict is reported as
`coherence_note` (`agent_coherence_probe`: `cold` by default, `always`, `off`). The probe fires on the warm-up's
own "a load was attempted" answer, never on the cold load's duration (a sub-tick load measures 0 s) and never on
"a note exists" (the warm-up also speaks when it settled nothing). The
window compaction budgets against is then probed live and reported as `ctx_window` (the box's
`agent_ctx_tokens` when the probe cannot answer, the 8,192 fallback only when that is unset too) — that probe
runs on the admission deadline too (register S-24), because it is allowed to absorb a cold load and on
the wall context a slow seat spent the run's whole clock on it. `ctx_window_note` says which of the three
windows that number IS — probed, configured, or the conservative fallback — because this door measured
8,192 cold and 114,688 warm on the same seat minutes apart and neither result said which it was. A defer
at the CORDON reports its `admission_wait_sec` / `admission_note` like every other admission exit. **The wall is sized before any of that (register D-102):** this door's `timeout_sec`
(else `agent_timeout_sec`, else 180 s) is a hard context deadline, so `pipeline.SizeRun` prices the run from the seat's
rate store (else `agent_seat_tok_s`, for the box's own agent seat only: a named `model`, or a seat placement picks, with no
rate of its own is not sized at all) before the fence check, the cordon or any seat request, and every call that gets past argument validation and seat
placement, answered or deferred, carries `wall_estimate_sec` / `min_turn_sec` / `wall_note` — the delegation wire's own names
(a call refused before a seat is chosen, by its arguments or a composite placement guard, and a `route`d one, carry none). A wall that cannot hold one tool step and a
64-token final (`seatrate.MinViableSec`, the delegator's `feasibleFinal` floor, never the max-final `min_turn_sec` that
ADR 0050 rejects as a refusal; it does not depend on `max_steps`) is refused with `deferred: true`, `defer_class: budget`,
`steps: 0` and the numbers; a
seat with no rate is never refused. **A wall above that floor but below the estimate (the row's own 420 s against a `min_turn_sec` of
807 s) runs, and its final answer is fitted to the wall that is left (register D-134):** the loop carries the D-95 final-budget fit
(`pipeline.InstallFinalFit`, the same function the delegation door's node calls), so at the forced final step the answer opens at
the largest budget the remaining wall can decode at the seat's rate instead of the configured worst case (4x the step budget, up to 8,192
tokens), floored at 1,024, off the live clock; the tool steps are untouched. The rate is the one the wall gate sized the run from, so a
named or placed seat with no rate of its own is left as it was. The answer, and a run that died at its wall, then carry `final_budget_fit`
(the budget the forced final opened at) and `budget_note` (the arithmetic), only when the fit narrowed something (a run whose own final was not narrowed, because it answered early, was cut before its forced final or reached it with wall to spare, carries the fit predicted when its wall started, as the node's result does); a wall that buys fewer
than 1,024 tokens of final keeps the floor, and `budget_note` says the wall will be the stop. A final the seat ended on that budget
(`finish_reason: length`) is a partial, and the fit makes that end likelier on purpose: it is still published as `output`, because a cut answer is
the caller's to use, and the result says so with `output_truncated: true`, the loop's own `stop_note` and a `truncation_note` naming the budget the final
opened at when the loop's fit narrowed it, not the fit predicted at run start (a complete answer carries none of the three). A resolved planner absent from the endpoint's served roster fails loud with
`deferred: true` naming the model, never a silent fall back to the workhorse — "served" means
matched against canonical ids **or** `meta.llamaswap.aliases`, since a tier-seeded `agent_model`
is normally an alias. A read root the run's tools cannot open ends the run at once with `deferred: true` and a reason
that opens `workspace root unavailable: <root>:` (register C-79), where the seat used to be left to answer from nothing;
the call carries `defer_class: infrastructure` when the root existed as it came in, and none for a `read_root` that never
existed (a typo, which looks the same at the first tool call). Every response
carries the effect ledger (`effects` counts + `effects_flagged` records) on success AND deferred
paths, the step `trace` (per tool call: tool, status, `obs_chars`, `rule` — 0.113.22, ADR 0036)
with `rules_fired` and, when the box has an `agent_env_rules` table, its summary as `env_rules`;
`setup_actions` (0.113.24, ADR 0036 P2: up to eight `{tool, args}` replayed before the first model
turn through the same rules and dispatch, spending no step) is reported back as `setup_ran` with
step-0 `trace` entries marked `setup: true` — the same field on every `agent_delegate` subtask; trace steps that did
not commit carry `note` (≤ 160 bytes of what the model was told, 0.113.26). `agent_rig {seat, since?, node?, markdown?}`
(0.113.26, ADR 0036 P3a) is the seat rigger's classifier over this box's delegation-log corpus: every failed row of the
seat on one axis in a published precedence order, weights over eligible rows, evidence job ids, the pre-authored remedy or
"not a rule matter" — reads files only, proposes nothing, applies nothing; an unknown seat defers naming the seats seen;
and `judge: true` adds one end-of-run **advisory** same-seat completion (`judge_report`)
grading the flagged effects for operator review — annotation only, it never gates anything.

`offload_ask` is the ONE-CALL delegation entry: question + paths in, `{answer, evidence}` out,
with the harness (`internal/askjob`) authoring the whole contract — goal, output schema, and an
acceptance check anchored to the distinctive tokens mined from the attached files (one
`regex:` alternation over the three most frequent tokens the goal does not already contain). It runs on the
local seat through `Pipeline.RunAgentContract`, the same entry a local delegation placement
takes. It exists because contract-authoring cost, not caller discipline, is what kept measured
`agent_delegate` adoption at ~0. Two properties are load-bearing rather than incidental: the
anchor is excluded against the FULL BUILT GOAL (the lint measures parrot-passability against
`c.Goal`, whose boilerplate carries its own long words), and when no distinctive anchor survives
the builder REFUSES instead of emitting a check that would pass anything. The generated
acceptance is evaluated by the handler and published as `verified` / `acceptance_failures` —
this lane does not go through `delegate.Run`, and a check nothing evaluates is decoration. `verified` is a
CITATION check, not a correctness verdict: it asks whether the published answer quoted one of a
few distinctive tokens mined from the attached files, never whether the answer is right. Those
tokens are picked to be things only these files would say — real identifiers wherever the files
have them, and ordinary words only when they are long enough to be domain terms or actually name
one of the attached files — but it stays a heuristic, so read `verified: true` as "this answer
demonstrably read the files", not as proof of a verbatim quotation.

The graded text is built from the fields the caller is SHOWN (`answer` + `evidence`, decoded),
never from the loop's prose or the raw structured bytes. Both of the other choices were measured
wrong, one per direction: grading the prose gives `verified: true` beside a published answer that
cites nothing, and grading the bytes gives `verified: false` when the re-pack returned an empty
`answer` and the handler fell back to publishing the prose. `verified: false` is a prompt to read
`acceptance_failures` and then the evidence, never a reason to discard the answer: the residual
case is a question whose subject is a SHORT (<8-character) or question-named identifier, which
leaves nothing anchorable at all.

**A failed local first attempt is retried on another seat.** When the first attempt ran on this box's seat and
failed its acceptance (`acceptance_failures` non-empty), the handler gives it the second chance `agent_delegate`
gives a failed verification: the same contract goes once through the delegator at `route=remote`, whose first
placement never lands on this box's seat. The delegator's own retry inside that call is kept off the seat too
(`RunOptions.RemoteRetryOnly`, which records no `pin_reason`, so `offload_status` still counts the call as
`unreasoned`): when the fleet node's answer also fails, the engine tries another fleet node or none, and its skipped
retry is named in the note, instead of running a third attempt on the seat where the first one failed. A routed ask (`auto`, `remote`,
a named route) needs nothing added: the delegator's own engine already retries inside that call, and the response
now says so (see the route paragraph below). An ask that names `route:"remote"` keeps that engine's `remote -> local`
retry: `offload_ask` has no reason channel, so a named remote is unreasoned under
[ADR 0078](../architecture/decisions/0078-a-placement-pin-needs-a-reason-without-one-it-is-a-hint.md): its first
placement is a fleet node and the engine's retry may come back to this box's seat, like every other door without a
reason channel; a caller that needs a strictly remote run
uses `agent_delegate` with a `pin_reason`. The retry is decoded and graded exactly like the first attempt
(`gradeAskWire`), and only a clean one is published:

- **The retry verified:** it is the answer, with `verified: true`, the fleet block (`node`, `placement`, `seat`,
  `executed_on`), `retried_on` (the node) and `retry_note` (`first attempt on <seat> failed_verification: [...]; this
  result is the retry`).
- **The retry also failed** (a failed verification, no answer, or a defer from a node that took it): the FIRST answer
  stands, `verified: false` with its `acceptance_failures`, plus `retried_on` and `retry_note` (`retry on <node> also
  <outcome>; this result is the first attempt`).
- **The fleet took nothing** (declined, no eligible node, could not be asked) or **the budget is short:** the first
  answer stands with `retry_note: retry skipped: <why>` and no `retried_on`, which would name a node that never ran
  it.
- **The caller named `route: "local"`:** no retry, ever. A named local route is how an ask says its material stays
  on this box (the tool has no `pin_reason`), so the first answer stands with `retry_note: retry skipped: route local
  was named; the ask stays on this box`. The retry applies only to an ask that named no route.

The retry lives inside the contract's own `timeout_sec`, as the delegator's does. An ask never names its wall, so its
contract is auto-sized and owns the 900 s cap; the retry is skipped when what the first attempt left is under the
retry floor (the configured `agent_retry_min_sec`, at least 10 s), and otherwise it is handed what is left as an
explicit `timeout_sec`. When the fleet's engine retried inside the retry too, its own note follows the ask's, after
`the fleet's own retry:`. A deferred first attempt is not retried (it did not fail a verification), and neither is a
verified one.

A deferred `offload_ask` keeps the finished answer (register C-80). When the node says the loop FINISHED and only
its structuring failed (`schema_miss`: the re-pack was skipped because the wall had no time left, was cut by the clock, or
stalled), the deferred payload carries the loop's prose as `output` with `schema_miss: true`, beside the defer's own
`reason` and `defer_class`, for a local run and a routed one alike. It is never graded and never an `answer` field.
`offload_review_diff` publishes no such field: what that lane returns went through its grounding filters, and the raw
prose has not, so a review defer stays bare and never carries `output`. The one thing it does with a finished or cut
answer is read its lines itself, through those same filters, when the clock kept them from being structured (salvage,
under `offload_review_diff` below).

`agent_delegate`'s `route` argument picks the placement rule (see
[fleet-node.md](fleet-node.md#placement-routes-and-the-retry-delegator-side) for the mechanics):
`auto` (default) runs local while the local seat is idle and considers the fleet only while it is
busy; `local`/`remote` pin one side only with a `pin_reason` (`privacy`, `locality`, `measurement` or `operator`; without one they
are hints that placement may override, [ADR 0078](../architecture/decisions/0078-a-placement-pin-needs-a-reason-without-one-it-is-a-hint.md)); `spread` deals every subtask across the local seat and every
eligible remote in one pass. Since PR-5 (ADR
[0050](../architecture/decisions/0050-placement-ranks-adequate-seats-by-expected-completion.md)),
`auto` and `remote` compute that placement for the WHOLE call in one joint deal respecting each
node's headroom, rank quality-adequate remotes by expected completion (a feasibility floor asking only
whether one tool step and a minimal answer fit the contract's own wall, then an eta-based ordering with a
seeded near-tie draw so parallel callers do not herd onto one seat), and demote — rather than exclude — a remote whose only obstacle
is a declared-but-idle lease. `results[].placement` (the published `placement_reason`) names which
node ran each subtask and, for `auto`/`remote`, a one-word verdict for every OTHER reachable remote
too (`chosen | queue | cap | slow | lease | cold | probe | private | unfit(ctx) | noschema`; `private` is the data-class rule: a node declared public in `delegate_remote_classes` takes public-data work only) — read it before
assuming the fleet was even consulted.

Since 0.130.2 (register C-46) `route` is accepted on **`agent_run` and `offload_ask`** too. Both
doors ran local unconditionally before, so a remote seat could not be named from this box at all.
`remote` / `auto` / `spread` / `queue` sends the call as ONE contract through the delegator's
single-contract path, and the response names `node`, `placement`, `seat` and `executed_on`, plus `node_reported` (0.185.0) only when the
node's own result named itself by another id than `node`, the one placement used (see [fleet-node.md](fleet-node.md), "Which node"). The two
doors put different things on the wire: for `agent_run` `read_root` and `judge` do not travel — the
executing node reads its own root, and the response carries `notes` naming each of the two the caller passed — so it is for
self-contained goals and `setup_actions`, while for `offload_ask` the files ride inline, so any node can answer.
An `agent_run` contract carries a flat `{answer}` `output_schema` (register D-125). It was built with none, and the
delegator refuses a schema-less contract on every node (`noschema`: remote output merges only after a mechanical
check, and a node's own ack refuses it too), so a routed `agent_run` could never be placed on a node; the prose is still the
`output`, and the node's `{answer}` object rides beside it as `structured`. A deferral carries the node's `defer_class`,
its `node` and `placement`, and, when only the structuring failed, the finished answer as `output` flagged
`schema_miss`, the way `offload_ask` does. `local` keeps
the old behaviour exactly, and so does an omitted route on `agent_run`, unless it names a model this box does not serve
(next section). An omitted route on `offload_ask`
runs local too, except when loading the agent seat would unload another vLLM seat that holds the cards
(the seat guard's verdict; 0.165.2): the ask then takes `auto`, and the response carries a `route_note`
naming that seat. The ask is self-contained, so nothing is lost by moving it; `agent_run` reads its
`read_root` on this box and stays. A routed response also carries `retried_on` and `retry_note` when the delegator
retried a failed verification (or considered one and skipped it) inside the call: both fields were dropped before
this block surfaced them, and they are absent when no retry was considered. The review lane and `agent_run` share the
block, so they carry the same two fields.

#### A seat only the fleet runs: `agent_run(model: <seat>)` (register D-125)

`agent_run` checked the `model` it was given against this box's llama-swap roster only and refused a name that roster
lacks, however many fleet nodes ran it. A name this box does not serve is now looked up on the fleet. With no route (or
`route: "remote"`) the door reads the roster's health once (`rosterprobe.Probe`, the read every single-shot lane shares:
concurrent, bounded at 5 s a node, a down node named and not waited on) and takes the nodes that advertise the agent lane
and whose `agent_seat` is the name, without regard to case. A composite node whose layer has an agent-role seat with that
model is taken too, and the contract then names the layer (register A-100). One contract names one layer, so nodes that
run the name as their own agent seat are preferred, and when only layered nodes serve it the first one's layer wins and only
the nodes that serve it from that layer are asked.
ONE contract goes through the delegator at `route=remote` with `remotes` narrowed to those nodes (a call's own list replaces the
configured roster, and is checked against it), so it runs on exactly one of them, placed by the usual gate: a node whose
card is leased, or that cannot hold the wall, is passed over for another that serves the name, and with none left the call
defers naming why. The response names `node`, `placement`, `seat`, `model` (the seat that ran it), `route` and
`executed_on`, plus `node_reported` (0.185.0) only when the node's own result named itself by another id than `node`, the one
placement used (see [fleet-node.md](fleet-node.md), "Which node"), and carries no sizing of this box: the wall is the executing
node's expectation (ADR 0055).

- **It honours a name only when it IS a node's agent seat.** The contract names no seat, so the node runs its own agent
  seat. A name a node merely serves (an alias, a non-agent seat) is refused naming the seat that node does run; nothing is
  guessed. A router seat's `model_map` twins are not matched either.
- **The answer is checked against the name.** A node that changed its agent seat between the probe and the dispatch runs
  another seat; an answer whose `seat` differs from the name is withheld and deferred (`defer_class: config`). The finished
  prose a deferral carries (`schema_miss`) is checked the same way: from a seat other than the name, or from a node that reports
  none, it is not published, and the node's own reason and class stay with a clause saying what was withheld.
- **The contract is a reasoned remote pin (`pin_reason: operator`), so its second chance is never this box's seat.** The caller
  named the model, so the placement is theirs, and the delegator retries an attempt it retries (a seat that abstained, a seat
  that went down) on another of the nodes that run the name, or does not retry. A bare `route=remote` would retry remote -> local,
  which for a named seat runs the whole goal on this box's own agent seat, a different model, only for the door to withhold that
  answer. `offload_status` counts the call under `pins.reasoned.operator`; an `agent_run` that names no model keeps the bare remote
  route and is still counted as `unreasoned`.
- **A name nobody runs keeps the roster sentence and says where the fleet looked,** one clause per node (its id, its agent
  seat, and a name it serves without running it as its agent seat). `defer_class` is `config` when every node answered and
  `infrastructure` when one did not; the reason names that node. A box with no `delegate_remotes` has nobody to ask and
  refuses exactly as before. With `route: "remote"` the roster sentence is not used (this box's roster was never consulted):
  the reason says no fleet node runs the model and where the fleet looked.
- **The fork is narrow.** A name this box serves runs here and never reads the fleet; `route: "local"` never asks it
  whatever the name; `route: "auto"`, `"spread"` and `"queue"` with a model are refused (`defer_class: contract`, `steps: 0`,
  nothing asked) because they place by capacity, not by seat name, and the model used to be ignored without a word; a
  local roster that cannot be read leaves the call on this box, where the loop's first request reports the real transport
  error. `allow_browse` cannot travel (the browse tool drives this machine's own browser) and is refused when the name
  could only run on a node.
- **No wire field and no node changed,** so it works against the nodes as deployed. Running a seat that is not a node's
  agent seat needs the contract to name it (a wire field and a node-side change that widens what a fleet-token holder can
  make a node load); that larger design is not here.

#### One bad subtask does not reject the others (register D-111)

`agent_delegate` prepares each subtask on its own (`delegate.PrepareBatchWithCap`) and `offload_research` does the same
for each page's contract. A call used to be refused whole at the first subtask that failed intake (a `context_paths` file
that is not there, a context past the box's cap, a blank goal), so a valid sibling never ran. Now:

- **A rejected subtask is a defer at its own position.** The call answers one result per subtask sent, in the order
  sent. The rejected one is `deferred: true` with `defer_class: contract` (the caller's mistake, not a broken stack) and a
  `reason` that opens `subtask N:` (`source N:` for a research page, N being the page's place in the `urls` list) and
  names the cause. It is counted in `summary.deferred` and never in `summary.infrastructure` or `lost_to_stack`. No node or
  seat was chosen for it, and it writes no ledger row: nothing was placed.
- **The valid subtasks run** as they always did, and the call is flagged `isError` by the same rule as any other result
  (nothing succeeded and something failed, was lost or was skipped), so a rejection beside a delivered answer is a
  successful call. `acceptance_lint` stays on the result it belongs to.
- **`lint_only: true` stops after the lint (D-109).** `agent_delegate` runs intake and `LintAcceptance` exactly as above,
  then returns before `RunWith`: no job id, no lease, no queue, no seat warm-up, no model. The reply is
  `{lint_only, dispatched: false, note, subtasks: [{index, acceptance_lint, placement: {route, where, node, reason}}]}`,
  one row per subtask sent (a rejected one is the same contract-class defer, at its index). The placement comes from
  `delegate.PlanPlacement`, which takes only the readings `RunWith` takes before it deals (the lease file, the local
  seat's in-flight count, the run registry, one `/fleet/health` GET per node, all bounded at 4 s) and calls the same deal
  (`dealAutoRemoteRoom`); routes `spread` and `queue` answer `where: not_computed`. It is a preview of the deal, not a
  promise: a lease or a node's refusal between the preview and the real call can change it.
- **A caller that reads `results[i]` by index must tolerate a contract-class defer inside an otherwise delivered call.**
  The position of a result is the position of the subtask.
- **Two refusals stay whole-call.** More than 8 subtasks (`agent_delegate`) is refused for its number, counted on the
  subtasks sent, so one bad subtask among nine does not make room. A call with no valid subtask keeps its flat
  `deferred: true` answer, now naming every bad subtask (`subtask 0: ...; subtask 2: ...`). The same answer also names
  the subtasks intake refused when the engine itself refuses the whole call and nothing runs (an unknown `route`, say):
  `<the engine's reason>; also refused at intake: subtask 1: ...`, so fixing the first complaint does not meet the second
  on the retry (`offload_research` names a page by its place in the `urls` list).
- **Progress notifications speak in the caller's numbers.** The engine is handed only the valid subtasks, so a mixed call
  reports `subtask K of N` with K the subtask's position in the call (where its result sits) and N the number sent, counts the
  refused subtasks as done from the start (`N of N done` is reachable) and opens with `delegating N subtask(s), M refused at
  intake`. A call with nothing refused reports exactly what it always did.
- **The command-line verbs follow the same rule.** `local-offload delegate --contract` and `local-offload research` prepare
  each subtask or page on their own, run the valid ones and print the refused ones as contract-class defers at their own
  positions (exit code 0, as every contract-class defer is); a file with nothing valid, or more than 8 subtasks, is refused whole
  as before, the refusal naming every bad subtask.
- `extra_read_roots` (attaching files from outside `read_root`) is not part of this: it needs a read floor at intake
  first, because the delegator-side reader would otherwise widen what a model-written call can send to fleet nodes.

#### The whole-call deadline (ADR 0065)

The MCP client aborts a tool call at its own limit (1,800 s in the reference setup) and drops the
response with it, and a producing job is polled to its node's ceiling (up to 14,400 s), so one slow
subtask used to hold a call past the abort and take the finished results down with it (a call ran
2,103 s and lost a finished 423 s answer). `agent_delegate` and `offload_research` therefore have a
**whole-call deadline**, `agent_call_deadline_sec` (default **1,500 s**; `0` = the default; negative =
none), measured from the moment the handler is entered — the client's clock starts when it sends the
request, so `offload_research`'s page fetch spends from it too.

At the deadline the call **returns what has finished**. Every unfinished subtask is a budget-class defer
whose reason opens `call deadline reached; N unfinished` (N is the whole call's count, across every
chunk of a batched research call) and then says what that subtask was doing: running on a named node
under a named job, running on the local seat, not yet placed, never started, or not stopping. The
outstanding work is cancelled — the local seat is told to stop, and polling of a remote job ends with
the give-up every cancel takes ([ADR 0064](../architecture/decisions/0064-a-delegator-takes-back-what-it-has-not-started.md)):
the node is asked once to take the job back (`DELETE /fleet/jobs/{id}`, best effort, never for a job
last seen running; the reason says what the node answered: taken back, or `withdraw not confirmed: ...`
with no route, already started or no answer) — nothing further starts, and a remote job the node did
not take back keeps running there and stays open in the intent ledger for the recovery pass (one it
took back closes as `withdrawn`). The cut changes what an outcome is called, never what was measured: a run cancelled
after nine steps still reports its steps, tokens, stop reason and trace, and what the outcome itself
reported beyond the cancellation is quoted in the reason (`the run itself reported <class>: ...`). A
subtask whose seat ignores its context is abandoned after a bounded unwind: its result carries the job
id of the attempt that had not returned, and its late row, if it ends, is under the same id. On
`route=queue` the delegator takes one last look at each job whose poll it cancelled and publishes what
the holder says: a finished job is returned as its answer, one still held is a defer that says queued
or claimed. The result is a successful tool call: a deadline defer is a result shape, not a failure.
The default is above the longest single subtask that starts at once (`timeout_sec` cap 900 s + the
300 s admission allowance + the 60 s poll grace) and below the client's abort by the margin a response
needs. It is not a promise that no healthy subtask is cut: time queued on a node (credited back to the
wall, up to the queue budget the node's own estimate sets, ADR 0063: the lesser of the poll budget and
300 s for a node that publishes none) and a capacity wait come on top, so a worst-case auto-sized
subtask can run past it.
A seat that goes down under a run ([ADR 0066](../architecture/decisions/0066-a-seat-that-goes-down-is-waited-for-and-the-failed-step-reissued.md)) meets the deadline like any other attempt.
The delegator re-places the node's `seat down:` defer on another node and credits the dead seat's wait back to that retry's
budget, but the credit is on the contract's clock, never the call's: a re-placement still running, or waiting for
capacity, at the deadline is cut with a `budget` row of its own, and the published result is the first attempt (the
seat-down defer, produced in time) with the cut in its `retry_note`, as for an abstention. A `seat down:` defer produced
after the deadline (a local run answering from the unwind, or a finished answer the delegator's own rescue was still
re-packing) is the deadline's budget defer: it quotes what the node reported, keeps `seat_recoveries` and
`seat_down_wait_sec`, and is not re-placed.
Raise `agent_call_deadline_sec` for such work, but keep it below the client's abort; a value at or above
it, or a negative that was meant as a number, is reported by `doctor` and once at startup. The CLI verbs
take no deadline.

**Progress notifications.** A request that carries a progress token (`_meta.progressToken`) also gets
`notifications/progress`: an opening one, one per subtask state change ("subtask 2 of 8 started",
"subtask 2 of 8 finished (succeeded) on <node>; 3 of 8 done" — counted against the whole call, across the
chunks of a batched research call), and a heartbeat every 30 s while nothing changes ("still working: 3
of 8 subtasks done after 4m0s; call deadline in 20m0s"). `progress` is a running counter (the spec asks
for a strictly increasing value; a heartbeat has no new work to count). `offload_transcribe` (register C-89)
gets the opening notification and the same 30 s heartbeat with no subtask counts ("offload_transcribe started",
then "still working: offload_transcribe running for 4m0s"): one request to one whisper upstream is silent for as
long as its audio takes, and it has no whole-call deadline of its own (its HTTP call is bounded by
`stt_request_timeout_sec`, its wait for a card a render holds by `gpu_wait_ms`). It is strictly opt-in and
additive: no token, no notification, and a slow client cannot slow a subtask (events go through a
bounded queue that drops rather than blocks). In the MCP TypeScript client SDK the token is sent only
when the caller passes `onprogress`, and the request timeout restarts on a progress update only when
the caller also sets `resetTimeoutOnProgress` (`maxTotalTimeout` is the absolute cap). Whether the
reference client does either is **unverified**, which is why the whole-call deadline above is a hard
limit that does not depend on it.

### The research lane (`offload_research`)

*A page is handed to the seat in lines it can read (0.184.0).* The agent's `read_file` cuts every line at 2,000 characters and
pages by line, so the rest of a longer line is out of the seat's reach: a page that arrives as one 98,304-character line gave it
its first 2,020 characters, and research runs on such pages reached the forced final step in 38 of 117 cases against 36 of 1,283
for pages with ordinary lines. `research.Build` therefore wraps each context document at `research.MaxLineChars` (1,800, pinned
under `agent.ReadFileLineCap`): at the last space in the final quarter of the window, in the middle of a run with no space, never
inside a rune, with the space becoming the newline and nothing else added or removed. A page whose lines all fit is handed over
byte for byte; the fetched text that anchors, fingerprints and verifies quotes is the page as fetched.

`offload_research` is the one-call answer to "this leg needs the web, so it goes to a cloud
subagent". It takes a goal and up to 12 public URLs, fetches every page DELEGATOR-side under
a public-web guard in TWO halves, strips it to text (dependency-free
HTML→text: scripts, styles and page chrome dropped, block boundaries kept; 2 MiB read cap,
96 KiB text cap), and builds ONE delegation contract per usable page — then runs the same
`delegate.Run` path `agent_delegate` uses (route `spread` by default, so pages are dealt
across the local seat and every eligible fleet node).

The guard's two halves are the point (ADR 0042, 0.117.3). By NAME,
`internal/research.ValidateURL` takes http/https only and refuses `localhost`, `.local`,
`.internal` and every configured tailnet zone (`tailnet_suffix` and `tailnet_suffixes`) —
shapes an address cannot express — and it
refuses without spending a connection. By ADDRESS, the client rides
`netguard.PublicTransport`: the host is resolved at DIAL time through netguard's single
resolution seam, every answer is judged by `netguard.CheckPublicIP` (loopback, RFC 1918,
link-local incl. `169.254.169.254`, multicast, the CGNAT/tailnet range, `0.0.0.0/8`,
`240.0.0.0/4`, the TEST-NETs, and the IPv6 forms that embed an IPv4 — NAT64, 6to4, Teredo),
and the dialer is handed the vetted IP LITERAL. Both halves run on the first hop and on
every redirect hop. Before 0.117.3 only the name half existed and the fetch reconnected by
name through a bare `http.Client`, so a hostile record with a one-second TTL could validate
as public and then resolve to `127.0.0.1` at connect time; `internal/research/fetch_rebind_test.go`
is that regression, and it asserts ZERO accepts on a loopback listener.

**Two more text sources: RSS/Atom feeds and YouTube subtitles.** Both are fetched delegator-side with no login and no cookies,
and both end where a fetched page ends: the text goes through the same digest path, the digest through the same
`internal/untrusted` fencing, and the URL through the same public-web guard.

- *Feeds.* A response whose content type is `application/rss+xml`, `application/atom+xml` or `application/rdf+xml`, or whose
  body sniffs as a feed (a generic XML, plain-text or untyped response whose root element is `rss`, an RDF root that declares
  the RSS 1.0 namespace, or an Atom `feed`), is parsed with `encoding/xml` (`research.FeedToText`; the only added dependency is `golang.org/x/text`'s windows-1252 table, already in the module graph). Each item's title, link, date and summary
  become the text, at most `research.MaxFeedItems` (50) items and 1,000 bytes of summary each, inside the ordinary 2 MiB read
  cap and 96 KiB text cap; a feed cut by either, or one whose document stops before its end (a body that ends inside an
  item), says so first in its own header (`Items: 2 (the feed document was cut off here; later items are not included)`),
  is marked `truncated` on its source row, and does not count the item it stopped in. Item links are text only and are never followed: a
  caller who wants an item lists its URL; a relative link is shown resolved against the feed's own `xml:base` (on the feed,
  the entry or the link) and otherwise against the address the feed was fetched from, after redirects. The text is read in
  the encoding the response's `charset` parameter names, else the one the XML declaration names, else UTF-8: windows-1252
  and cp1252 are decoded, and so are iso-8859-1 and latin1, read as windows-1252 the way browsers do (bytes 0x80-0x9F are
  curly quotes, the euro sign and dashes, never control characters, in a real feed); any other encoding leaves the document
  to the ordinary path. Before parsing, an invalid UTF-8 byte becomes U+FFFD and a character XML forbids becomes a space,
  so one bad byte costs one character and not every later item. A document that does not parse as a feed, or that parses with no item or entry
  (an empty channel, or an RDF/XML, OWL or SKOS file served as `application/rdf+xml`), is read exactly as it was before.
- *YouTube subtitles.* A video URL is routed by `research.ClassifyURL` to the yt-dlp executable named by the config key
  `ytdlp_path`: `youtube.com/watch?v=`, and `/shorts/<id>`, `/live/<id>` and `/embed/<id>`, on exactly `youtube.com`, `www.`,
  `m.` and `music.youtube.com`; `/embed/<id>` on `youtube-nocookie.com` and `www.youtube-nocookie.com`; and `youtu.be/<id>`.
  Over http or https only, on the default port, with no userinfo and an 11-character id (`/embed/videoseries`, a playlist,
  is a page). Whichever shape was written, the binary is given the canonical `https://www.youtube.com/watch?v=<id>`. The key
  is **empty by default, which means the adapter is off** and the URL is read as an ordinary page. The harness never searches
  `PATH` for the binary and never installs it: the value must be an absolute path to an executable file (the admitted
  versions are in [security.md](security.md#admitted-third-party-software)). **`ytdlp_path` is trusted as written**, with
  nothing checking the file, **unless the optional key `ytdlp_sha256` is set** (64 hexadecimal digits, either case): the file
  is then hashed before every run, and a mismatch, an unreadable file or a malformed value fails the source as `ytdlp_path`
  and starts nothing, while unset nothing is hashed. A file replaced between the hash and the start is not caught: the pin
  is against a wrong or changed binary, not against a writer who races each call. On Windows the file must be launchable by
  the system itself, an `.exe` or a wrapper `.cmd` that starts the interpreter: the admitted zipapp is a
  `#!/usr/bin/env python3` script, which runs directly only on a POSIX host with `python3` on `PATH`, and a Windows host that
  names it gets a `ytdlp_path` error at call time. **With a wrapper `.cmd` the hash covers the wrapper, not the interpreter or
  the script it calls**; those are the operator's to admit and pin. The binary runs once per URL with `--skip-download
  --write-subs --write-auto-subs --sub-langs=<sub_langs> --sub-format=vtt/srv3/best --no-cookies --no-cookies-from-browser
  --ignore-config --no-playlist --no-cache-dir --socket-timeout 15` and zero retries, given the canonical URL after a literal
  `--`. Warnings are not suppressed (`--quiet` stays, `--no-warnings` does not). `sub_langs` is an `offload_research` input
  (default `en.*`; letters, digits and `. * _ - ,` only, else the source fails as `bad_sub_langs` before anything runs, so no
  `cmd.exe` metacharacter, `& | < > ^ %`, a double quote or a space, can reach a `.cmd` wrapper). It runs in a per-call
  directory under `<state_dir>/research-scratch` (else a directory under the OS temp dir), with a scrubbed environment (home
  and temp point at that directory), bounded by the call's deadline and the per-fetch timeout (one run is capped at 15
  minutes whatever `fetch_timeout_sec` asks for), with no retry and no scheduling. **A deadline or a cancellation ends the
  whole process tree the binary started**, not only the program launched, because a wrapper `.cmd` leaves its interpreter
  running as a grandchild that would otherwise hold the per-call directory: `taskkill /T /F`, started hidden, on Windows,
  and `SIGKILL` to the process group the binary leads on POSIX (`winexec.KillTreeOnCancel`). The per-call directory is
  removed with a bounded retry of a few hundred milliseconds (a process that has just been ended takes a moment to let go of
  it). A removal that still fails is not dropped: it is recorded as `cleanup_error` on the source row, beside the text when
  there is one, and each call first sweeps `ytdlp-*` directories directly under its own scratch root that nothing can still
  be using (last changed more than 20 minutes ago, the longest run plus slack). The limit: the tree is found through the
  parent chain, so a child whose parent had already exited when the run ended is out of reach of `taskkill` on Windows. A
  binary that exits 0 while a child still holds the output pipe is read as a success once the pipe wait runs out, and that
  child's hold on the directory shows as `cleanup_error` until the sweep takes the directory. Of the files it writes, a
  readable format wins (WebVTT, SRV3, SubRip), then the shortest name, so the plain `en` track beats `en-orig`. The subtitle
  file is converted to plain text: header and NOTE/STYLE/REGION blocks, cue ids, timestamps, cue settings and tags are
  removed, entities decoded, and the rolling duplication automatic captions carry is removed (a line equal to the one before
  it is dropped, and a line that only extends it replaces it). A srv3 track that ends inside a cue, cut by the 2 MiB read
  bound or interrupted, yields the cues read before the cut and is marked `truncated`. Subtitle text and the same words
  pasted as a page therefore reach the digest stage as the same bytes (pinned by
  `TestSubtitleTextAndPastedPageGiveTheSameDigestInput`; no model is called). A site break degrades to a skipped source with a
  typed `error_kind` and no retry: `subtitle_site_break` (the binary exited non-zero), `no_subtitles` (exit zero and no file;
  the reason ends with the last line yt-dlp printed, because it reports a refused subtitle download, an HTTP 429 rate limit
  included, as a warning and exits 0), `unsupported_subtitle_format`, `empty_subtitles` (a subtitle file was read and held
  only blank cues), `subtitle_timeout`, `ytdlp_path` (not absolute, missing, not matching `ytdlp_sha256`, or will not
  start), `bad_sub_langs` or `url_refused` (the canonical URL did not pass the public-web guard). A relative `state_dir` is
  made absolute before the scratch directory is built.
- *Security.* Every URL passes `research.ValidateURL` before anything is fetched or run, the subtitle path included: a hostname
  that resolves to a private, loopback, link-local or CGNAT address is refused before any process starts (the tests drive it
  through the `netguard.LookupIP` resolution seam). The canonical URL handed to the binary is validated too, because its host
  is the one the binary will actually contact, and it is a fixed YouTube host rather than anything the caller wrote. The
  matcher is exact, so a lookalike host (`youtube.com.evil.example`, a userinfo trick, another port) is an ordinary page under
  the ordinary guard and never reaches the binary. The adapters build no HTTP client (the bare-client allowlist in
  `bare_http_client_allowlist_test.go` still lists one in this package; `TestAdaptersBuildNoHTTPClientOfTheirOwn` pins it). One
  limit is stated plainly: yt-dlp opens its own connection, outside `netguard.PublicTransport`, so for this path the guard is
  a pre-flight name check (the resolve-and-check on the two hosts above, made before anything starts), after which the child resolves and dials on its own, plus the fixed canonical URL, which is why the adapter is off until an operator
  names a binary. Audio transcription is not part of this change: a video with no subtitles is a `no_subtitles` source, and
  nothing downloads audio or video.

Two contract rules are baked in because they were measured on the seats (2026-08-28): the
goal names the context document as *already provided* (a goal that says "read the document"
sends a small seat hunting for a file and fails acceptance), and acceptance is ONE any-of
regex (tagged `docanchor`) over the page's top prose content words — taken from sentence lines
and the headings that introduce them, never from identifier-shaped tokens, UI or markup
vocabulary, and never from the goal (0.141.1, register C-65), so an echoed goal cannot pass as
verified while a faithful digest passes by restating any one of about two dozen words.
Two faithful shapes used to fail it and now pass (0.184.0): an **inflection** (each anchor is matched in its singular, so
"programmer" restates a page that says "programmers"; the goal's own words are excluded in the singular too), and an **honest
absence**. The anchors are chosen off the whole goal the seat is handed (the fixed sentences `Build` adds, the file's name and
title, the caller's questions), not only the caller's, and the regex holds the page's words and nothing else, so no sentence the
goal contains satisfies it: the goal echoed back fails, and the delegator's lint does not call the contract parrot-passable.
An honest absence restates no page word (the goal tells the seat to say so in the goal's own vocabulary, which the anchors
exclude by construction), so it is excused by the delegator from the STRUCTURE of the digest and not from a phrase in the
regex: `delegate.EvalAcceptance` drops the fingerprint failure of a result whose structured digest is
`delegate.HonestAbsence`: an object in which every string at any depth (a list entry as much as a field) is empty or a short
statement (60 words at most) that the page does not carry what was asked ("not on page", "does not contain/address/mention/…",
"not documented/stated/…"), at least one such statement, no number or boolean, no second sentence that says what the page does
hold, no contrast word ("but", "however", …), and no report of a failure to read the file ("could not read", "not found", "the
file is missing": an error, not a finding). A stock phrase beside a claim, the goal echoed back, and "I could not read the file,
so the page does not address it" still fail it. Only a FAILED fingerprint can be excused: an unknown (as a qualified field
fingerprint over an empty field grades) is never dropped and stays a reported failure, but over an honest absence it is no
fact about the node, so it strikes nothing and passes nothing. The excuse counts for the node as neither a strike nor a pass
(`Quarantine.Pass`): an absence is no evidence that the node reads the right document. What no lexical rule can tell is a seat
that did not read the page and reports "the page does not address it" without saying that it failed; that is delivered as an
absence, as an honest one is, and only a second seat would tell them apart.

What a digest owes beyond that is one design (register C-74): **presence is declared,
non-emptiness is asked for only where the caller marked it, and the default digest owes that it
said something.** The default schema `{key_facts[], numbers[], quotes[], verdict}` declares all four
fields `required`, and a caller's `output_schema` keeps its own `required` entries and gains every
field its own `min_items:` / `nonempty:` acceptance reads, so a seat's direct JSON answer that
leaves one out fails validation and goes to the structured re-pack instead of being delivered and
failing acceptance after a whole run (36 of the 59 `min_items` failures on 2026-09-29). An item is
required of the digest only for the FIRST array the caller's own schema lists in `required` (one
check, never more than before) or through the caller's own `acceptance`; the default schema asks
for no items and a schema that marks nothing gets none, so a faithful "nothing on this page"
digest is a success (the old rule demanded an item from the alphabetically first array of any
schema). What the default digest does owe, on EVERY page anchored or not, is
`nonempty:key_facts|numbers|quotes|verdict`, a value in any one of its four fields: a page too
thin to anchor would otherwise carry no check at all, and a digest that said nothing (every list
empty, no verdict) would be delivered as a success. Empty lists with a verdict that says so still
pass, and so do populated lists with no verdict. The check began as `nonempty:verdict` alone
(0.147.0), and that failed digests that had said plenty: the research goal never asked for a
verdict and the re-pack writes an empty value for a field the loop's text did not carry, so from
2026-09-30, 36 default-digest pages failed with populated lists (`key_facts` median 10.5 items),
and an acceptance-only failure is not retried, so each one discarded a whole run (4.07 h of seat
wall time). The silent-empty digest itself occurred 0 times in 687 pages. The default digest's
goal now also ends with one sentence asking for the verdict (a caller's schema gets none), so
the verdict is normally there. The any-of `nonempty:` is evaluated by the delegator like every
other check; a node only parses it at the ACK, so a node one release behind admits the contract.

**A digest's `quotes` are the page's own words, or they are flagged (register F4).** A goal that asked for exact UI labels got
digests that paraphrased them, and a fact-checker could not find the "quote" on the page. Two halves, and neither is an
acceptance check: a new verb would be refused at the ACK by every node one release behind, and an acceptance failure discards a
whole run (the `nonempty:verdict` lesson above). `Build` adds one sentence to the goal of any schema with a `quotes` list, the
default digest's included: exact wording the goal asks for (a quotation, a label, button or menu text) goes in `quotes`,
copied character for character from the file, never shortened, reworded, translated or re-cased, and a shorter exact stretch
beats a paraphrase. It sits before the verdict ask, which stays the goal's last sentence. After the engine returns, the
delegator looks every quote up in the page that digest was written from (`research.VerifyQuotes`, applied by both doors
through `research.AnnotateWire`). A quote that is already the page's bytes is left alone. Otherwise it is looked for with case,
runs of white space, typographic quote marks and dashes, and no-break and zero-width characters set aside and, failing that,
with all punctuation set aside too (a colon the seat added after a label, a table's cell borders), always starting and ending
on a word boundary; an ellipsis (`...`, the single-character form, or `[...]`) is a gap between parts that must each be found
in page order. A quote found only that way is REPLACED in the published digest by the page's own span, so it is the page's
bytes by construction; a quote found nowhere (a paraphrase, or words stitched together from separate places) stays where the seat put it and is listed in the result's `quote_check`
(`checked`, `verbatim`, `corrected`, `unverified`, `note`), and the digest still succeeds. `quote_check` is absent when the
digest has no quotes to look up, and only the research doors publish it. Two limits: the page is the text the fetch kept
(navigation, header and footer are stripped before a seat sees the page, so a label that lives only there cannot be
verified), and the field is found by its name, so a caller's schema that keeps its verbatim text in a field not called
`quotes` is not checked. The delegation-log corpus and the ledger rows are written inside the engine, before this step, and
keep the seat's raw answer. On the MCP door the body's untrusted pass (`internal/untrusted`, described below) runs after this
step: it drops the hidden characters a page carries, neutralizes chat-template markers and caps each string at 16,000
characters, so a published span can differ from the page by those only.

A research page that fails only its own checks (a shape, an item count) is not re-run on another
node: a second seat given the same page mostly repeats the verdict at the cost of a whole second
run, and the result's `retry_note` says so. A failed document fingerprint (`docanchor`) is a
different fact: the answer is about another document, which is a property of the node (two of
them quarantine it), so that failure keeps its one retry on another node. A fingerprint that grades unknown for a reason
of the contract (a grounded check on a contract with no context documents, a field check the `output_schema` does not
declare: the `AUTHORITY-UNREACHABLE` lint warns about it) is still reported, but it neither strikes a node nor passes, and
on the research door it is not retried, because another node would grade the same unknown; the same holds for an unknown
over an honest absence. Only a FAILED fingerprint can be excused (by an honest absence): an unknown caused by the contract
or by an honest absence strikes nothing and passes nothing, and otherwise an unknown the result caused (an empty answer beside the
documents, a declared field missing from the result) strikes like a failure. The seats never gain
network access; the agent loop's egress cage is untouched. Failed or refused fetches come
back as `sources[].skipped` and produce no result — a broken page never reads as a digest.

The body marshals in a fixed order — `summary`, then `untrusted` (a one-line notice that the
results and sources are third-party page content, data to weigh and never instructions), then
`partial` / `error` (present only when a batch chunk failed), then `results` with its
`result_sources` index, and `sources` last — because
the MCP client keeps only the head and the tail of a long body, and the digests are the
deliverable (C-75). Every string in `results` and `sources` has been through `internal/untrusted`
(hidden runes dropped, role markers neutralized, each capped at 16,000 characters; SF-45, gate G13). A **partial** result (some pages digested, some failed) is a successful
tool call: `isError` is set only when nothing succeeded, so one failed page can no longer cut the
surviving digests out of the reply. What is missing is named in `summary.failed` /
`summary.lost_to_stack` and in the failed result's own `reason`. `agent_delegate` follows the same
rule (`summary`, then `results`).

### The ask lane's result cache

An IDENTICAL repeat of an `offload_ask` call — same question, same `read_root`, and the same
file **bytes** — is served from an in-process cache (`internal/askcache`) without spending the
seat again, and the response says so with `cache_hit: true`. A fresh run publishes
`cache_hit: false`; an absent field would read as unknown, and "was this answer computed just
now" is not something a caller or the adoption instrument should have to guess at. A
**deferred** result carries no `cache_hit` at all, by design rather than omission: a defer is
never stored, so it is always a fresh run. The stored entry is the answer as it was published, so a cached
unverified answer already includes its cross-seat retry (`retried_on` and `retry_note` ride the entry as they
are): a repeat never retries again.

**Say what this buys, and no more.** It pays on an exact repeat and on nothing else. A
*different* question over the same files still pays full seat time (46–75 s measured), because
the seat has to reason about the new question. The only mechanism that would fix that is
keeping a model context resident between calls, which needs llama-swap slot pinning — trading
a seat's availability for cache warmth, and explicitly declined. Nothing here is a general
speedup, and the tool description says so in as many words.

Four properties are load-bearing:

- **Keyed on CONTENT, never on path.** The key covers the question, the resolved `read_root`,
  and each resolved doc's name plus the SHA-256 of its bytes. A file edited between two
  otherwise-identical calls is a different key, so the seat runs again — a stale answer is not
  merely unlikely, it is unreachable. That single property is the whole safety argument for
  serving a cached answer at all, and it is mutation-proven (key on the path instead and both
  the unit test and the wired front-door test go red).
- **The lookup happens AFTER `askjob.BuildContract`**, because the key *is* the resolved file
  content and `BuildContract` is what resolves it. Reading the files is microseconds against
  the seat time a hit skips. It also means every refusal (no anchor, over a cap, outside
  `read_root`) still happens on every call: only a finished answer is short-circuited, never a
  refusal. Keying on the resolved docs rather than the caller's raw path strings is what makes
  `/abs/cfg.go` and `cfg.go` one key — both hand the seat identical bytes.
- **Only successful, non-deferred results are stored.** A defer, a refusal or a runner error is
  a statement about this minute, not about these files; caching one would turn a transient seat
  failure into a lane that stays dead for the rest of the connection. A `verified: false`
  answer *is* cached — the seat ran and answered, the citation check simply did not match — so
  an identical repeat returns the same unverified answer rather than re-rolling the seat. A
  caller wanting another attempt changes the question, which is a different key.
- **Bounded at 32 entries, oldest out, and scoped to the process.** The MCP server is spawned
  per client over stdio, so one connection is one process is one cache, born and destroyed with
  the connection. That is why there is **no `session_id` argument**: it would be a second,
  weaker spelling of a boundary the process already draws exactly, and adding a required input
  to the one-call tool would undercut the friction removal the tool exists for.

The repeat returns without touching the seat at all, and editing one attached file makes the
next call miss and go back to the seat. That is not a timed measurement — no run log exists to
attribute a number to, and the only number ever in hand was a cold-swap-degraded figure (the
seat's llama-swap slot was mid-load for a different model during the attempt) that does not
belong here. What IS provable: `TestAskSecondIdenticalCallSkipsTheSeatAndSaysSo` asserts the
seat ran exactly once across two identical calls, and the mutation proof named in the "Keyed on
CONTENT" property above (key on the path instead and both the unit test and the wired
front-door test go red) pins the edited-file-misses behaviour directly.

One deliberate gap to know about: the ask lane writes **no delegation ledger or corpus row**.
`delegate.Run` records one per subtask; `Pipeline.RunAgentContract` on its own does not, so
`offload_ask` traffic will not appear in the delegation corpus or in any analysis built on it.
The pipeline's own task ledger still sees the run. Nothing depends on this today — it is
recorded so nobody later reads an empty delegation corpus as "nobody used the tool".

`offload_review_diff` is the CLEAN-CONTEXT review lane: a diff plus a task statement in,
severity-ranked findings out, run on the local seat through the same
`Pipeline.RunAgentContract` entry. Its argument is different in kind from every other lane's.
The others compete with "read the file yourself" on cost, and lose — this one offers something
a lead cannot produce from inside its own context at all: a reviewer that never saw the work.
The isolation is the mechanism, so the contract ships the task and the diff and **nothing
else** (`internal/reviewlane`), and the tool is registered unconditionally beside `offload_ask`
for the same reason — a lane behind a config flag is one more reason not to take it. Its wall
is the box's `agent_timeout_sec` when that is larger than the 300 s wire default (0.115.21,
register D-03/D-09; capped at the wire ceiling): a 51 KB diff on the 30 tok/s 27B spent 9 steps
and timed out at exactly 300 s on 2026-09-10 while a 13 KB one finished in 3 — size the diff by
path (`git diff -- <dir>`) when the estimate in the node log says the wall is below it.

**The lane rides the fleet when the local seat is fenced (0.125.0, register D-110).** Before it
builds the local loop the handler reads the machine-wide lease (`delegate.LocalLease`) and asks
`delegate.ForeignFence`: is the seat fenced — an exclusive text hold, a draining cordon, a media
render — by someone who is NOT this process? If so, the review is shaped as the same contract the
local seat would have run and handed to `delegate.RunWith` at **route `remote`** over the
configured `delegate_remotes`. Without that, the local loop waited the whole `agent_lease_wait_sec`
at the affinity cordon and came back as a capacity defer, for the lease's entire length, however
idle the fleet was — the one lane a lead reaches for at the moment of deciding, unusable for hours
because a bench had the cards. The QUALITY FLOOR is `remoteEligible`'s and is not relaxed for this
lane: the node advertises the agent lane, its own card is not leased, the seat is resident, and the
contract's estimated tokens plus the loop's reserve fit the node's `agent_ctx_tokens` — a diff that
does not fit a remote window is not sent to it. Route `remote` rather than `auto` is the other half
of that floor: `auto` would fall back to the fenced local seat when nothing qualified, so the remote
route is what makes an accepted result provably off-box. The published result names where it ran
(`executed_on`, `node`, `placement`, `seat`, plus `node_reported` (0.185.0) only when the node's own result named itself by another
id than `node`) and the fence that moved it, and the findings go
through the SAME filters as a local review (one `publish` step in the handler serves both, so the two cannot drift).
An INHERITED lease is not a fence: `gpu reserve … -- <session>` sets `GPU_LEASE_EPOCH`, and the
holder's own review stays on the cards its lease cleared. When the fleet takes nothing, the local leg
runs, and it asks the same question again before it admits anything (the in-run fence check of ADR 0032's 2026-09-17
amendment): while the fence holds it defers `capacity` AT ONCE and waits for nothing, naming the holder and its declared
expiry (`gpu lease class=… expires=HH:MM:SS`), and if the lease cleared while the fleet was being asked it simply runs.
Either way the result carries a `fleet` note saying the fleet was asked and why it declined. A fence taken in the
moments after that check meets the cordon instead, which waits out the admission budget and files a defer whose reason
carries the holder's DECLARED window (`, declared until 11:40PM (~37m0s left)` — `modelaffinity.LeaseError`), so
"gpu busy" answers *when to retry*. What each defer gets, and what a caller does with it, is the policy block that
closes this lane's section.

On the evidence for it, keep two things apart. Cognition **reports** a dedicated reviewer in
their Fusion setup catching ~2 bugs per PR, ~58% of them severe; that is the vendor's own
published figure, with no sample size, no A/B baseline and no external audit, so treat it as a
claim rather than a citation. The MECHANISM is independently supported: long-context
degradation, measured across 18 SOTA models by Chroma's context-rot study and by Stanford's
lost-in-the-middle work. The lane rests on the mechanism.

These design choices are load-bearing rather than incidental:

- **The diff rides in the GOAL, not in a context doc.** A context doc becomes a file the seat
  must find with `list_dir` and open with `read_file`, and the measured failure mode of a small
  planner is calling no tool at all — which would produce confident findings about a diff never
  read. The cost is that `AgentContract.Validate`'s 256 KiB context cap never sees the diff, so
  `reviewlane.MaxDiffBytes` owns that bound and refuses early with the real numbers.
- **No acceptance check, deliberately.** An empty findings list is a CORRECT outcome, so any
  content check would either punish a clean diff or pass anything — the decorative acceptance
  `delegate.LintAcceptance` exists to name. What replaces it is a check the harness can actually
  make: a finding naming a file the diff never touched is dropped and reported as
  `dropped_ungrounded`, since an invented path is how a small seat fails here, and a line with
  no severity, no file and no why (a bare claim) is dropped and reported as `dropped_hollow`.
- **Findings arrive as an array of strings.** The schema is a flat list of strings by choice.
  (`gbnf.FromJSONSchema` once compiled any array to an array of strings, so an object-item schema
  would have become strings anyway; it reads arrays of objects since register F43, and this
  lane's schema was not changed.) The prompt asks
  for one `severity | file:line | claim | why` line per defect and `ParseFindings` reads them
  back tolerantly, keeping what it cannot parse as an unranked claim rather than dropping it.
  Whether such a claim *survives* is `Report`'s call (`DropHollow`): a line that carries any
  structure — a known severity, a file or a why — is kept however badly the rest is formatted,
  and only a bare claim is dropped, counted.
- **The re-pack is a copy of the answer, and a lossy one, so the answer is read too (0.178.0).**
  Three independent reports on 2026-10-09 showed findings with every field empty but the claim,
  on a mimo-9b seat (twice) and a qwen3.6-35b-a3b seat (once). Running this lane's exact prompt
  and schema on those fleet seats found two mechanisms, and in both the seat's own answer was
  strictly richer than what the re-pack made of it. (1) The grammar-lane re-pack is told only
  `"findings" (array of strings)`, so it SPLITS the seat's lines: on the qwen3.6 seat one perfectly
  formed line, `severe | file:3764 | claim | why`, came back as its claim and its why in two list
  items with the severity and the location dropped, and on two mimo-9b seats (no re-issue, finish
  `stop`) five well-formed pipe lines came back as ten bare strings. That is the ordinary path on
  these seats, not an edge, and it fits the reports: their counts (4, 6 and 2 findings) are all
  even, as two strings per line gives. (2) A mimo-9b seat repeated a line; the loop's repetition guard read
  that as a cut final (finish_reason was `stop`, 471 tokens of a 2,048 budget) and re-issued it
  with `listCapInstruction`'s "Return the same JSON object that was asked for". This lane never
  asked for JSON, so the seat invented one, `{"findings":[{"severity","path","line","claim","why"},…]}`,
  and the re-pack kept one string per object: its claim. A control run (a 20-line diff, no re-issue)
  came back with nine intact pipe lines and a faithful re-pack, so the split is not deterministic:
  the prompt gives the seat no rule for what a list item is, and what it does varies with the
  input. `reviewlane.RawLines` therefore reads
  the answer in the two shapes seen (the pipe lines the prompt asks for, and a fenced or bare JSON
  document of strings or `severity`/`path`|`file`/`line`/`claim`/`why` objects, rendered back to
  pipe lines), `reviewPayload` runs both readings through the same filters and publishes whichever
  keeps MORE findings, and a tie keeps the re-pack — what every run that worked has always shown,
  including its stripping of a preamble that the raw reading would count as a hollow line.
  When the answer's reading wins the result carries `salvaged: "repack_flattened"` and a note.
  The objects the object-array schema option would have asked the re-pack for could not be had on
  this fleet when this was written: `gbnf.FromJSONSchema` compiled object items to strings, and
  only vLLM seats honoured a full JSON schema. Since register F43 it compiles them (a separate
  decision whether this lane should ask for them). Not changed here, and the upstream of mechanism (2): the cut-final
  re-issue asserts a JSON object for every schema contract whatever its goal asked for, and the
  repetition guard (`minRepeats = 4`) turns a reviewer that repeats a line into that re-issue.
- **An empty findings list is never published unless the seat EARNED it.** "No findings" is the
  one result a reader might take as reassurance, and a broken run reaches exactly that shape:
  `agent/loop.go` returns `stop_reason:"done"` as soon as the model stops requesting tools with
  no check that the final message carries content (empty content is live-measured here — the
  re-pack's comment on a GBNF + thinking seat stranding its answer in `reasoning_content`),
  `agenttask.go` special-cases only `"budget"`, and `repackStructured` extracts findings from an
  empty string into a schema-valid `{"findings":[]}`. `steps` and `stop_reason` describe both
  cases. So `reviewlane.VerdictReadsClean` cross-checks the seat's OWN raw answer for the
  explicit `NONE` verdict the prompt asks for, and the handler defers with a distinct reason
  when it is absent. It checks for a signal, never for quality. When the list is genuinely
  empty, the response says in words that it is not a verification.
- **A list the filters emptied is not a review either (0.178.0).** The gate above used to stand
  down whenever any line had been dropped, on the reasoning that a run which produced text is
  not the broken-run shape. A hollow or invented review produces text too: on 2026-10-09 a small
  seat answered a 228-line diff with four bare claims that merely restated it, and the caller
  got four findings with every other field empty, published as a success with no note and no
  counts. Now whenever **no finding survives** and the raw answer does not read as a clean
  `NONE`, the lane defers (`defer_class: abstention`), whatever was dropped, and the deferral
  carries the non-zero counts and a reason naming them ("none of the seat's 4 findings survived
  the lane's filters (4 had no severity, file or why) …"). A clean `NONE` with zero findings
  stays an empty, noted result; the only emptied-list success left is that same clean verdict
  beside lines that were all dropped, and its note says so.
- **Five counts say what is NOT in the list**, published on the same terms (present when
  non-zero, on a deferral as well as a delivered review): `dropped_ungrounded`, `dropped_echo`
  (the prompt's own field spec or worked example handed back as a finding — measured behaviour,
  so it is a byte-equality guard rather than a human's vigilance), `dropped_hollow` (no known
  severity, no file and no why), `dropped_duplicate` (the same defect reported more than once,
  and findings folded together by the same-line rule below) and `truncated_by_cap`. The `note`
  on an empty list is gated on them: "found nothing" beside a non-zero drop count is false, and
  says so differently.
- **One file and line is one finding, and every claim stays readable (0.178.0).** `Dedupe` keys
  on the normalised claim, so a seat that restates one issue in different words passed through
  as several findings: on 2026-10-09 a 48 KB diff produced three, stacked on a single line of one
  file. `reviewlane.MergeSameLine` now folds findings that cite the SAME file (compared by base
  name, as `Ground` and `Dedupe` do) and line into the most severe one (a tie keeps the first
  in the seat's order) and puts the others' claims in an additive `also` array on the kept
  finding; each folded finding counts in `dropped_duplicate`. Line 0 means the seat did not say
  where, so a finding with no line or no file is never merged on that alone. It runs after
  `Dedupe` and before the cap, for the same register D-90 reason: a stack must not crowd a
  genuinely different finding out of the published list. Nothing distinct is lost, since the
  caller still reads every claim.
- **A review the seat wrote is salvaged when the clock cut its structuring (0.178.0).** The
  harness ledger for the 26 hours to 2026-10-09 held five review defers, and three of them read
  `output failed schema: re-pack skipped: the final answer was cut at the completion budget
  (output_truncated)`, on a 27B seat and a 9B one; a fourth read `structured re-pack skipped: 0 s
  left to the wall + 30 s grace …`. In each, the seat had written review lines and the lane threw
  them away. The answer format is line-oriented and `ParseFindings` already reads raw lines, so the
  re-pack is a convenience here, not a requirement: `reviewlane.Salvage` takes the raw answer's
  complete lines (a finished answer is read with `RawLines`, below, so JSON finding objects count;
  a cut one is split on newlines and loses its last line) and `reviewPayload` runs them through the
  normal `Report` path (echo, hollow, grounding, dedupe, same-line fold, cap). It keys on structure, never on prose: the node's own
  `output_truncated` flag, or `schema_miss` with the `budget` class (a finished answer whose
  re-pack the clock skipped, clamped or cut; a canceled re-pack is excluded because nobody is
  waiting). A cut answer's last line is dropped, because it is a fragment. What survives is
  published with `salvaged: "output_truncated"` or `"wall"` and a `note`: an `output_truncated`
  list may be **incomplete** (whatever the seat had not yet written is unreviewed), a `wall` list
  is the seat's whole answer. A cut answer is never a clean verdict, whatever words it holds. If
  nothing survives, the lane defers exactly as above, and the deferral names both the drops and why
  the answer reached the lane unstructured; a cut answer with no complete line keeps the node's
  own deferral verbatim. Every other deferral — a busy card (`capacity`), a seat or stack failure
  (`infrastructure`), a re-pack that answered the wrong shape (`abstention`) — is unchanged, even
  when it carries a good answer in `output`. The raw prose is still never published, only what
  survives the filters. The same read applies on the fenced-seat fleet path: a deferred fleet
  review is salvaged with the fleet's provenance (`node`, `placement`, `fence`) instead of falling
  through to a local run that defers at once behind the lease that fenced it; a salvage that comes to no review
  falls through exactly as before. `internal/pipeline`'s `TestReviewLaneReadsWhatTheRunnerFiles`
  runs the lane's real contract through the real runner so the node and the lane cannot drift
  apart. The pipeline's own ledger row still records the run as the defer it was; the salvage is
  the door's.

**Every result is bound to the diff it judged.** A review used to say what a seat thought and
nothing about what it thought it about, and a finding the caller skipped left no record. Now every
payload, deferrals and early refusals included, carries an `outcome` and an `evidence` block, all additive
(a reader written before them keeps working; the deferral shape and the findings are untouched):

- **The binding is the diff's sha256**, plus `base_sha` and `head_sha` when the caller passes them (hex,
  7 to 64 characters, echoed and never resolved: the lane has no git access, and a commit id the caller
  typed can be wrong where the bytes cannot). `prompt_sha256` binds the exact prompt the seat was shown and
  `seat.answer_sha256` its raw answer; the seat receipt (`seat`, `node_id`, `node`, `executed_on`, `steps`,
  `stop_reason`, `salvaged`, `deferred`) names who answered and carries no timing. A review refused before
  any diff was read (both sources, neither, a `diff_path` that cannot be read or is over its cap, a bad
  `read_root`, a working directory that cannot be read, arguments that do not decode) is sealed as a diff-less `NOT_RUN`: `diff_bytes` is 0, there is no
  `diff_sha256` (no bytes were chosen, so there is nothing to fingerprint), and the refusal is both
  `evidence.refusal` and `outcome_reason`. The deferral shape and reason are the ones the door always gave.
- **Gates run first.** `evidence.gates` is built before the seat is contacted, so a refusal is bound too:
  the lane's own `diff-size` and `conflict-markers` (a merge-conflict marker on an added line) and then the
  `gate_receipts` the caller handed over, the results of the lints and tests it ran itself (`name`,
  `status` `pass`|`fail`, `detail?`, `head_sha?`; at most 16). **The lane runs no lints or tests itself**: the
  receipts are results the caller ran and hands over, and the lane cannot verify them. It executes nothing a
  caller names, which keeps the review door from becoming a command-execution surface. A receipt whose
  `head_sha` is not the review's head is marked `stale` and cannot support a PASS; when the review names a head,
  a receipt that names no commit is marked `unbound` and cannot either. A review with no `head_sha` is tied to no
  commit: `evidence.head_unbound` is true, a receipt that names a commit is marked `unbound` (nothing shows the diff
  is that commit's), and a receipt that names none stands as given. **Even with a `head_sha`, nothing ties the head
  to the diff bytes**: the lane has no git access, the fingerprint binds the bytes, and the head is the caller's
  word, echoed. A red gate does not stop the seat: the lane is advisory, and the
  findings of a diff that fails a lint are still worth reading.
- **One outcome, decided in one place** (`reviewlane.Decide`). `NOT_RUN`: no clean seat reviewed the diff,
  which covers every result with `deferred: true`, a runner error, an early refusal, a refused binding
  (malformed commit id or receipt), a diff over `MaxDiffBytes` (refused even if a caller skipped the builder), a
  result with no seat receipt, a result with no diff fingerprint, an answer that read as NONE while every line it wrote was discarded, and a run the
  node filed as a deferral whose answer the lane read as a clean NONE (the salvage still publishes what it read,
  and findings read from such a run are `FINDINGS`, but a clean NONE from it is never a PASS: `seat.deferred`
  records the attempt). `STALE`: the diff was not the same bytes at the second check. `FINDINGS`: an advisory
  list. `GATE_FAILED`: a clean seat beside a red, wrong-commit or unbound gate.
  `PASS`: a seat with a receipt, a diff fingerprint and a second check equal to it, an in-bound diff, every gate
  green and for the reviewed commit, an unchanged diff, and the seat's own NONE. A PASS on a review that named no
  `head_sha` carries `outcome_reason` "no head_sha: gates not tied to a commit" beside `evidence.head_unbound`, so it
  does not read as a PASS bound to a commit. `outcome_reason` says why for everything but FINDINGS and a PASS tied
  to a commit.
- **A second fingerprint check right before returning** (`reviewlane.Seal`). A `diff_path` is re-read through
  the same confined reader and compared with the fingerprint taken first; `recheck_sha256` shows the second
  value, and a difference (or a file that cannot be re-read) makes the result `STALE`. For an inline diff the
  check compares the string with itself: it is held in memory and cannot move, so `recheck_sha256` equals
  `diff_sha256` by construction there. A review that never reached a seat has nothing to re-check.
- **Every review leaves a durable trace**: `review-evidence.jsonl` in the same directory as the configured
  ledger file (derived from `ledger_path`, so it sits beside the ledger wherever the operator put it; the harness
  home when no ledger path is set), one `review` row (outcome, evidence, counts of what was shown and what the filters and the cap took out)
  and one `finding` row per published finding (severity, file, line, the claim and why cut to 512 bytes, and a
  digest of the whole finding), joined by `review_id`. It is written whether or not the caller reads or applies
  the findings, and a write that fails is reported as `trace_error` on the result. This sidecar is the durable
  trace of the review: no ledger row is written for the verdict, on purpose, because every ledger reader counts
  a row as a call, the pipeline already writes the run's own row (door `offload_review_diff`) before the outcome
  is known, and the ledger keeps hashes where this trace keeps the short finding text. The ledger row is the
  run record, this file the verdict record.
- **Stays advisory.** An outcome is a label on a result a lead reads; nothing here
  blocks a merge or a commit.

Everything the lane returns is ADVISORY: it never gates a merge and never substitutes for the
final does-it-actually-work verification, which stays with the caller — as do security review,
architecture judgement, and any call the caller is accountable for. Findings are triage input;
a `severe` label from a small local model is a prompt to read those lines, not proof. It shares
the ask lane's ledger gap above: `RunAgentContract` writes no delegation corpus row.

**What a defer means here: retry, backoff and the warm seat.** The lane is one synchronous call, and every defer it
returns leaves its caller the same next action: review the diff yourself (the handler's own comment says so). So its
policy is the narrowest one that stays honest. It waits only where a wait can end. It retries only what a different place
can cure, and only inside the budget the caller is already waiting on. It says on the result what it did. And a retry
goes elsewhere, **never on the same seat**. The door itself has no retry loop and no backoff of its own: its local leg is one
`RunAgentContract` call, `internal/reviewlane` retries, sleeps and warms nothing, and a defer comes back published as it
came, with the `fleet` note when the fleet was asked. What can look like a retry is one of four bounded things that live
elsewhere, listed here so that nobody stacks a fifth on them:

1. **Admission, before the wall starts.** One budget, `agent_admission_wait_sec`, 300 s by default (negative switches it
   off), pays for the fence check (instant), the cordon, the swap pre-flight, the cold load of the seat and the probes
   after it. The seat's run line (`fleet_max_concurrent_jobs`) is separate: a run waits in it for up to its own wall, outside the wall and
   outside that budget.
2. **Contention, inside the run.** `internal/seatwait` re-sends a request that llama-swap answered with 429, 503
   "process is not ready" or 500 on a health-check timeout, after 1, 2, 4, 8, then 15 s (a server `Retry-After` wins),
   out of one budget per run, `seat_contention_wait_sec`, 90 s by default. A budget that runs out files
   `seat contended:` (`infrastructure`).
3. **The fleet's capacity wait, on the fleet leg only.** The delegator re-places a review a node refused and waits for
   capacity for `agent_placement_wait_sec`, 120 s by default (a review carries no whole-call deadline, so nothing else
   bounds it).
4. **The fleet's one retry, on the fleet leg only.** Of the results a different node can cure, the delegator retries
   these once: a node-filed abstention, a seat found incoherent at warm, a failed warm-up (a defer only with
   `agent_warm_failure_defer` on, which it is not by default), a seat that went down, a lost workspace root, and a tool
   call written as the final answer. The retry runs on another node (on this box's own seat
   only once the fence has lifted), inside what is left of the contract's own `timeout_sec`, and is skipped with a
   `retry_note` under `agent_retry_min_sec` (at least 10 s) or when no other node can take it; a retry that ran is
   published as `retried_on` and `retry_note`. A failed verification, the delegator's commonest retry, never applies
   here: the lane has no acceptance check.

Next to its siblings: `offload_ask` retries once, a failed acceptance of a local first attempt and never after
`route: "local"`, and publishes a defer of any class as it came; `agent_run` retries nothing itself (a `route`d one rides
the engine's rules); `agent_delegate` has the whole engine (`retryable` and `runOne` in
[`internal/delegate`](../../internal/delegate/run.go)). What each defer shape of the review lane gets:

| Shape | Class and the words of the reason | What the door does, and whether it retries | What the caller does |
|---|---|---|---|
| A foreign lease fences the seat | `capacity`: `gpu busy: …; no new run is admitted on this box until it is released (gpu lease class=… expires=HH:MM:SS)` | Seen on entry, the review is offered once to the fleet at route `remote` (above). If the fleet declines or fails, the local leg re-reads the lease: while the fence holds it defers at once, and if the lease has cleared it runs. **Retried:** once, on a different place and never on this seat. No backoff, because the verdict was on disk before the call and nothing the call does releases another process's lease. | Review the diff yourself now. Re-call once after `expires=` if you still want the clean-context review; that is not a poll. |
| A fence taken after the door looked | `capacity`: `gpu busy:` with `declared until …`, or `… the seat yielded the cards and this contract is re-placeable` | A lease taken between the check and the cordon is waited for up to the admission budget, then deferred. A lease claimed while the seat was cold-loading makes the seat unload itself and defer at once. The fleet is not asked: it is asked only about a fence the door saw on entry. **Retried:** no. The "re-placeable" is the delegation door's word; nothing re-places a review. | As above. |
| The seat's run line is full | `capacity`: `seat busy:` | Waits in line, first come first served, up to the run's own wall, then defers. **Retried:** no; the wait was the retry. | Re-call later, or review the diff yourself. |
| The seat is unfit to answer | `infrastructure`: `seat incoherent at warm:`, a failed warm-up, `seat down:`, and `stalled:` (the seat stopped producing inside its allowance; behind a re-pack it reads `structured re-pack unreachable: stalled: …`) | The local leg publishes it as it came. On the fleet leg the delegator's one retry (above) applies to the first three; a plain stall is not one of them. **Retried:** once on the fleet leg for those three, never for a stall and never on the local leg. | Review the diff yourself. A seat that does this twice is the operator's to look at (`offload_status`). |
| The stack failed or the seat is misconfigured | `infrastructure` other than the above (endpoint unreachable, transport died, `seat contended:`), and `config` (no seat resolvable, seat not served) | Published as it came. **Retried:** no, by the door or by the delegator: another try meets the same stack, and an operator has to act. | Review the diff yourself; tell whoever runs the box. |
| The seat answered and nothing usable came of it | `abstention`: `the seat returned no structured findings`, `none of the seat's N findings survived the lane's filters …`, a re-pack of the wrong shape | Published with the drop counts, after the salvage kept what the filters could keep (above). **Retried:** on the fleet leg the delegator retries a node-filed abstention once. The door's own filter verdict is raised after the engine returned and is never retried, and the local leg retries nothing. Nothing in this repository measures a second seat curing a hollow review, and a retry costs a second full wall inside a call someone is waiting on. | Review the diff yourself, or send it in pieces by path (`git diff -- <dir>`). |
| A ceiling stopped the run | `budget`: the liveness ceiling (`ceiling … reached while producing`), `step budget exhausted`, a completion cut at its token budget, a re-pack the clock skipped or cut (`structured re-pack skipped:`). `capacity`, when the ceiling found the run held behind the seat's other work (`ceiling … reached while held behind the seat's other work`) | The door imposes no deadline of its own (see what ends a run, below). The lines the seat wrote are salvaged when the clock cut the structuring (published with `salvaged` set to `wall` or `output_truncated`, above); otherwise deferred. **Retried:** no; a second run repeats the shape. The exception is a step budget that ended on a tool call written as the final answer, which the fleet leg retries once (above). | A `budget` defer: send a smaller diff, by path. A `capacity` one says nothing about the diff: re-call later, or review it yourself. |
| The call cannot be run as asked | `contract`, and this door's own refusals: both or neither diff source, no task, a diff over the byte ceiling, a malformed commit id | Refused before any seat is contacted and sealed as a `NOT_RUN`. **Retried:** never; the same input gets the same answer. | Fix the input. |

**What ends a run is not the wall.** The wall (the contract's 300 s by default or sized from the seat's rate, raised to
`agent_timeout_sec` when that is larger, capped at 900 s) is what a run is expected to fit in. It is not a deadline: the
door imposes none, and the node holds only the structured re-pack to it. A run ends when it finishes; when the seat
stops producing inside its allowance (a stall: `infrastructure`, reason `stalled: …`); or when the liveness ceiling
passes, max(3 x the seat's estimated run time, 2 x the wall, 1800 s) and never above 14400 s (`budget`, or `capacity`
when the run spent its time held behind the seat's other work). A review that is still producing its answer is not cut
at 300 s or at 900 s; only the re-pack that structures the answer is held to the wall.

**A cold seat is not a defer.** A seat that is not resident is loaded by the run's own admission, outside the wall and
inside the admission budget above (a vLLM seat's cold load measures 125 to 250 s on the reference fleet), and the wall
starts when the seat reads ready. A seat the call has just loaded is by default also asked one bounded coherence question before
the wall starts (`agent_coherence_probe`). The cost of a cold seat is the latency of the call that finds it. The lane keeps nothing warm and warms
nothing ahead of a call: an idle seat unloads after 5 minutes, and a keep-warm ping or a pre-warm call
would be a scheduler this project does not run. Two reviews sent while the seat's idle timeout has not run out find it
resident for the second. The review's result does not carry the admission block (`admission_wait_sec`, `admission_note`)
that `agent_run`'s does, so a long call cannot be told from its result alone to have been a cold load or a long review.

**A plain text reservation is not a fence.** `gpu reserve --class text` without `--drain` or `--unload-seat` steers the
delegator's placement off this box's seat but refuses no run at the gate, and this door does not consult it: the review
runs on the seat, on the cards the holder reserved. The delegation door's `auto` and `spread` routes, and its retry,
avoid those cards. It is not a defer, so nothing in the table applies to it.

**What the caller does.** Read `defer_class` and `reason`: a deferred review is a result with its reason and not a
failure, and it is never a clean verdict.

- Do not loop. The waits are already inside the call, so a re-call is warranted once, for a fence after its `expires=` or
  for a full run line later, and for nothing else.
- Size the client's patience to the call, which has no fixed upper bound at the wall. It can spend the admission budget
  (`agent_admission_wait_sec`) before the wall starts, when the seat is cold or the cordon is held; then, when the seat
  is at its run cap, its place in the run line, up to a further wall; then the run itself, which ends on a stall or at
  the liveness ceiling, at least 1800 s and higher on a slow seat (above). A run that is still producing is not cut at
  the wall, so a client timeout sized from the wall can cut a legitimate long review, and so can one tuned to the 30 to
  90 s that `offload_ask` quotes for its own lane.
- The review is still owed whatever the defer says: read the diff yourself.

**Where the door does not follow this yet.** A local-leg defer that is a fact about this seat or its cards, meaning a
fence taken after the door looked, a seat that yielded the cards, a seat found incoherent, a failed warm-up or a seat that
went down, is published as it came instead of being offered once to the fleet at route `remote` the way a review fenced
on entry is. The policy is one offer, made at once with no backoff, through the same `publish` step, inside what the
contract's own `timeout_sec` has left and under the same floor as the fleet's retry; a delegation retries these defers,
and the review lane should not be the door that does not. Not built. The admission block above is not on the review's
result either. Not built.

### Workflow templates (`offload_run_template`)

`offload_run_template` looks up and runs the ComfyUI workflow templates that a machine's installed
`comfyui-workflow-templates` package carries. It is one tool with an `action`:

| `action` | What it answers |
|---|---|
| `list` | The catalog, sorted by name and paged (`limit` 50 by default, 500 at most, `offset`), filtered by `kind`, `tag`, `text`, `license_class`, `reads_input` and `runnable`. `runnable` is true unless the call says otherwise (the templates this release can run); false lists the refused ones. `counts` holds the whole catalog's totals by refusal class, so a filter never hides how many templates a class holds. |
| `get` | One template's manifest and verdict: its models as `models/<class>/<name>` with the source the template names and the hash it declares, its input files, node packs and minimum ComfyUI version, and its licence class with the repositories behind it. |
| `readiness` | Whether this node holds what the template needs: each model present or missing (directory-aware, so a file counts only where ComfyUI would offer it to the loader, and the answer says where a misplaced one is), each input file in the input directory, the node packs, ComfyUI's version against the template's minimum, and the licence verdict. `ready` is true only when nothing blocks. |
| `run` | The gate below, then the run-graph path with the template converted on this machine's ComfyUI. |

`list`, `get` and `readiness` are read-only answers of `render/run-template.mjs` over the node's own tree
(`comfy_dir`): no lease, no ComfyUI, no network, no file written, the same bytes for the same tree.

**Why a tool and not a `template=` argument on `offload_run_graph`.** A new tool costs schema on every call of every
client. The cost was measured at 0.186.0: an average tool costs 1,019 tokens in the full profile and 672 in the compact one,
so one more tool is about +3.2% of the 31-tool full list. That is a cost the registered design accepts, and the
`template=` argument was only ever its fallback for a cost that proved unacceptable; a template is also a different object
from a graph (a UI-format workflow, converted on the node that runs it with that node's own listing of its nodes, and gated
by licence), and the run-graph schema is already the longest of the media doors. The tool has a reviewed entry in the compact
profile's keep-list, and a test (`toolsprofile_runtemplate_test.go`) pins the four sentences that carry its rules (what a run is allowed
for and refused with, that it never downloads a model, that an embedding the harness cannot verify is refused as `embedding_unverified`,
where it runs). Measured in bytes of the `tools/list` JSON on the default box at this head (tokens not counted), its own entry is 3,092
bytes in the full profile and 2,930 in the compact one, against means of 4,110 and 2,639 over the 32 tools.

**The gate: what a run is allowed for.** `run` asks the script to resolve the template first
(`render/template-manifest.mjs`, `resolveRun`). Nothing is leased, started or sent for a template this release will not run;
the typed answer is the tool's answer. The class is the first that applies in the order `template_unreadable`, `subgraph_unexpanded`,
`api_paid`, `flux_barred`, `custom_nodes`, `bad_model_path`, then the licence class, and `reasons` lists every one that applies; what the
node lacks is checked after them.

| class | outcome | when |
|---|---|---|
| `api_paid` | Defer (`deferred:true`) | the template calls a paid partner node: it spends money, so it is deferred and never falls back to cloud ([ADR 0001](../architecture/decisions/0001-defer-never-cloud-fallback.md)) |
| `flux_barred` | refused | FLUX-family weights ([ADR 0011](../architecture/decisions/0011-flux-family-license-prohibition.md)) |
| `custom_nodes` | refused | the template needs a custom node pack; this release provisions none |
| `license_unknown` | refused | a repository the licence map does not list, a weights file with no Hugging Face source, or a map entry that was inferred from a sibling and not read: unknown is not permissive |
| `license_conditional` | refused | the weights carry conditions on use, the third licence state ([ADR 0058](../architecture/decisions/0058-non-commercial-model-families-ship-only-as-named-license-tagged-opt-ins.md), accepted 2026-10-14) |
| `license_non_commercial` | refused | the weights are non-commercial |
| `template_unreadable`, `bad_model_path` | refused | the index lists a file that cannot be read; a model name or class directory that would leave its class directory |
| `subgraph_unexpanded` | refused | the catalog's walk left a subgraph instance unexpanded: it is nested more than 8 levels deep (the converter expands 64), or its subgraph contains itself (the converter refuses that too). The models, the licence class, the FLUX bar and the host-RAM figure are all read from that walk, so a template it could not finish is refused instead of run on a list that may be short |
| `models_missing`, `inputs_missing`, `comfyui_too_old` | refused | what this node lacks, with the exact list (`missing`) or both versions |
| `embedding_unverified` | refused | a prompt embedding the harness does not verify (a case variant answers it, more than one file could answer it, or a file answers a probe name), with each name, the reason and the files (`embedding_unverified`) |
| `unknown_template` | refused | no template of that name in this node's package |

A Defer carries `deferred:true`; a refusal carries `refused:true`. Both carry `class`, `code` (`TEMPLATE_<CLASS>`), `template` and a
`detail`, and no manifest. A template with no weights at all (licence class `none`) is permitted.

**What `run` does once the gate passes.**

1. The door writes the resolved runtime manifest (and the overrides, if any) to temp files and sends a `run_graph` task with
   the template **name**, through the same call `offload_run_graph` makes, on route `local`: the same lease, ComfyUI
   lifecycle, satisfier and result envelope. A busy lane answers as a run-graph call on this machine does (a place in this
   machine's line, resumable with `waiter_token`). The lane declares the host RAM the run needs from the model files that manifest
   names (`hostneed.ForManifestFile`, fitted to the card as a graph's files are, on a box that binds no ComfyUI render family too),
   and keeps the media class default for a manifest it cannot read or a file it cannot size; the lane probe reads the same figure. The
   figure is summed from the files the manifest names, and the manifest carries no mark that says it is complete: it is the catalog's reading of
   the template and the graph is the converter's, so two things keep a short list from becoming a load. The gate refuses a template the
   catalog's walk left a subgraph instance unexpanded in (`subgraph_unexpanded`) before any manifest exists, and the runner compares the
   converted graph with the manifest before it posts (step 3), so a file the graph loads that the manifest lacks defers the run. A figure
   summed from a short list can be declared for a run that is then deferred, and never for one that loads the file the list lacked.
2. The runner satisfies the manifest. Every model is named by the path it was found at and carries no `source_url`, so a
   model that went missing since the check is a typed defer and never a download. Models trusted on first use are checked
   (below) before ComfyUI is started, on every run.
3. ComfyUI is started, and only then is the template converted
   ([`template-to-graph.mjs`](media-generation.md#template-to-api-converter-rendertemplate-to-graphmjs)), because the
   conversion needs the running ComfyUI's `/object_info` and version. `overrides` are applied to the converted graph: a key
   is `<node id>.<input name>` (a node inside a subgraph is `<instance>:<node>`), the node must be in the graph, the input
   must already be there as a value (an input the node lacks is never added, and one wired to another node is never cut), and a
   value is a string, a number or a boolean. An override never retargets a file or names an embedding (`override_is_file`): the licence
   gate, the FLUX bar and the trust-on-first-use check read the models the template names, so an input of a model or input loader node, an
   input that holds a model file name, a value that is a model file name, and a string value with `embedding:` in it (in any letter case:
   ComfyUI loads a file of its embeddings directory from that word of the prompt text, with no loader node and no extension in the value)
   are all refused. The check reads the string values of the graph: an embedding assembled while the graph runs by string nodes (a
   concatenation of two overrides, for example) is neither refused nor compared with the manifest (below), so the licence gate, the hash and
   the trust record never see the file it loads. `seed` pins the seed inputs. A refusal of the conversion or of an override is the
   defer `TEMPLATE_CONVERT_REFUSED`, its `ref` the converter's own code (`unknown_node_class`, `comfyui_too_old`,
   `override_unknown_input`, `override_is_file`, ...), and the ComfyUI that was started goes down with it. The converted graph is then
   compared with the manifest: a model file it loads that the manifest does not name defers `TEMPLATE_MODEL_UNLISTED`, its `ref` the first such
   file, before preflight and before anything is posted. The catalog and the converter are two readers of the template and can differ (a value an
   instance carries into an inner loader, a loader whose `widgets_values` is an object, a loader that stores no value and takes the listing's
   default), and the licence gate, the FLUX bar, trust on first use and the host-RAM figure read only the manifest. A file matches an entry by
   its name under the class directory or by its base name, without regard to case or slash direction, as the catalog matches an annotation to
   a widget. An embedding the prompt text names is a model file too. A string input that names an embedding the way ComfyUI reads the
   text (`embedding:<name>`, below) is read as text, not as a file name, and each name it asks for is reported as
   `models/embeddings/<name>` unless the manifest names, under `models/embeddings/`, a file for it: the name as written or with
   `.safetensors`, `.pt` or `.bin` appended, at the top of the directory or in any subfolder (ComfyUI searches every subfolder; whole path
   segments, never a base name that only ends alike). ComfyUI does not walk the embeddings directories in order: `expand_directory_list` puts
   every embeddings root and every subfolder below it into a Python `set`, and `load_embed` opens the file of the first directory of that set
   that holds the name, so the directory that answers is chosen by hash order and differs from one process to the next. The harness does not replicate that
   choice (below). A name is read as the
   tokenizer reads it (`embeddingRefs`, a port of `token_weights`, `tokenize_with_weights` and `_try_get_embedding` in `comfy/sd1_clip.py`):
   the text is cut at parentheses and a closed group loses a numeric tail, so `(embedding:name:1.2)` is `name` and a bare `embedding:name:1.2`
   is the name `name:1.2`; the lower-case identifier starts the text or follows whitespace or a parenthesis; whitespace after it is skipped;
   the name ends at whitespace, `(`, `<` or `[`; commas come off both ends. The catalog lists a name its own prompt gives under the
   `embeddings` class, unless an annotation of that class covers it (the annotation records the name), so the licence gate and trust on first
   use read it like any weights file (a name with no source is `license_unknown`). Readiness verifies it, and fails closed. ComfyUI warns and skips an embedding it cannot find
   (`comfy/sd1_clip.py` logs `embedding:<name> does not exist, ignoring` and runs the prompt without it); the harness refuses, because a
   missing or different embedding silently changes the output of a vetted template, and the licence class, the hash and the trust record
   attach to the file the manifest names. So the lookup is deliberately narrower than ComfyUI's: a name is accepted only when exactly one
   physical file answers it, case-exactly (the name as written, or with `.safetensors`, `.pt` or `.bin` appended, on whole path segments, in
   the roots and every subfolder, `.git` directories included because ComfyUI's embedding search walks them while the listing a loader widget
   reads skips them; a file the class does not list, with no extension, answers the name as written, as `isfile` does), and
   then it is `present` and the runtime manifest names that file, subfolder and extension included. A file reached by several paths is one
   file, by the file system's own word (the inventory records where each embeddings file really is and what the file system calls it): a
   root that links to another registered root, a link inside a root, a root registered inside another and a file linked into two
   directories (symbolic or hard) are each one file, two copies are two, and a file system that gives no identity falls back to the real
   path. Every other outcome with a file in it is refused under its own class, `embedding_unverified` (not `models_missing`: nothing is
   missing), with the reason: a case variant answers it (the only file differs in letter case; whether ComfyUI finds it depends on the
   volume, and the harness does not ask the volume); more than one file could answer it (the root and a subfolder, two subfolders, two
   registered roots, two extensions in one directory, a file with no extension beside a listed one, an exact file beside a case variant
   of it, an annotated file beside another file that answers the same name); or a probe name is answered by a file of the node (below). A
   node that holds none is `models_missing`, listed by the name the text gives. When both apply the gate answers `models_missing`
   first, then `embedding_unverified`, whose answer lists each name with its reason and files. Not replicated, on purpose: ComfyUI's
   choice between the directories that hold a name, its preference of one extension over another, and the case rule of the volume.
   The full replica is a later row, to be taken when the first real template needs it (no template of the installed package names an
   embedding in an active prompt). A loader widget's embeddings file (`ConditioningLoader`) is not a prompt name: it is met by its exact
   path like a file of any other class. Names ComfyUI may also try for a prompt (the name as written before its commas come off, and the
   names the LLM-based text encoders take when they read the text without weights) are not requirements; the catalog row records them
   (`embedding_probes`) and a file of the node that answers one, in any case and however many, makes the template not ready and refuses the
   run as `embedding_unverified` (a probe name is answered by a file of the node). Every probe of a prompt is asked.
   A graph run is not compared.
4. The converted graph goes through the run-graph preflight and is posted. The result is the run-graph envelope with `template`,
   `license_class`, `reads_input`, `local_only` and `fleet_note` beside the outputs, and `tofu_first_seen` when a run recorded a
   model for the first time.

**Where it runs.** Every run stays on this machine's lane. The fleet door takes an API-format graph, and a template is
converted on the node that runs it with that node's own listing, so there is no `route`, `remotes` or `node` parameter, and a
call that passes one is refused as `fleet_not_offered` with the reason. A template that reads input files (an active
`LoadImage`, `LoadImageMask`, `LoadImageOutput`, `LoadVideo` or `LoadAudio`) is local for a reason of its own, and the
answer says so (`local_only:true`, `fleet_note`): input files carry generic names that two machines share with different
bytes, so a node holding a same-named file would run the template on the wrong bytes. Staging an input file under the name the
template reads is the caller's step; `readiness` and the gate name any that is missing.

**Trust on first use.** Most template annotations carry no hash (10 of the 1,135 in the index do, in the measurement below), so a model file
has nothing to be checked against. The first run that meets a model with no declared hash hashes it and records the sha256, the
size and the modification time in `<file>.tofu`, beside the file wherever ComfyUI finds it. A later run whose file has the same
size and modification time trusts the record without hashing (a 20 GB file takes minutes to hash on every run). A file whose
size or time moved is hashed again, and only the recorded hash passes: another hash is refused as `MODEL_SHA_MISMATCH`, naming
both hashes and the record to delete. The first sighting of a large file pays the hash once. A model whose annotation does
declare a sha256 is left to the satisfier's own check. The record is not the `.sha-ok` sentinel (which means "matched a pinned
hash") and never makes a pinned model skip its check. The limits: bytes replaced under an unchanged size and time are not seen,
and a record is the first sighting's word for a file, not a statement about where it came from. A record that cannot be read or
written refuses the model as `MODEL_TOFU_RECORD` (a read-only model directory cannot be trusted this way).

**Measured catalog at this head** (2026-10-14; installed package `comfyui-workflow-templates 0.11.70`, `core 0.3.361`, `json 0.1.96`,
via `node render/run-template.mjs list`; the research's 246 was a heuristic on the upstream index):

| class | first class of | carried by |
|---|---|---|
| `api_paid` (all three API signals agree) | 337 | 337 |
| `flux_barred` | 25 | 39 |
| `license_conditional` | 63 | 64 |
| `license_non_commercial` | 10 | 30 |
| `license_unknown` | 63 | 71 |
| `custom_nodes` | 0 | 0 |
| **runnable by this release** | **80** | |

Of 578 templates in the index, 80 are runnable: 5 with no weights, and 75 whose 247 model entries are 237 trusted on first use and
10 with a declared hash. 55 of the 80 read input files. `custom_nodes` is 0 because this package version marks no template as needing a
pack (an earlier head measured 50). "Carried by" counts every kind, paid-API templates included; among the 236 local templates
that need weights the catalog's own licence tally is 75 permissive, 63 conditional, 29 non-commercial and 69 unknown.

Among the local templates that need weights, 85 of 135 repositories are not in the licence map (the research counted 83 of 133),
and that is what keeps templates in `license_unknown`. On one node (<node-a>, ComfyUI 0.38.0) 4 of the 80 were ready to run that day
(three with no weights, and `audio_ace_step1_5_xl_turbo`): 73 lacked a model file and 52 lacked an input file they read.

**Not built in this release.** Fleet placement of a template; model downloads (a missing model is a refusal that lists it); an
acknowledgement path for a conditional or non-commercial template; staging of a template's sample input files; custom node
packs; a CLI verb and a fleet task for it. Later, with its trigger: the full replica of ComfyUI's embedding lookup (its choice between
the directories that hold a name, its extension preference and the case rule of the volume), taken when the first real template needs it. An override that would retarget a model or input file, or name an embedding, is refused, not
supported: the gate checks the file the template names, so a different file would run unchecked (text that string nodes assemble while
the graph runs is the one thing the checks do not see; step 3).
One template running end to end under a GPU lease is the acceptance measurement of this tool, and it has not been run.

## Important flows

Every tool ultimately enters the Cascade or a media backend — see
[../flows/cascade-escalation-and-defer.md](../flows/cascade-escalation-and-defer.md) and
[../flows/run-graph-manifest-satisfaction.md](../flows/run-graph-manifest-satisfaction.md).

## Data and state

The server is stateless between calls. State lives where the underlying system keeps it — the ledger,
the audit trail, footprints.

## Interfaces and entry points

- The MCP entry in `main.go`'s subcommand dispatch; tools registered in `internal/mcpserver/`.
- `.printing-press.json` declares the manifest: `api_name`, `version`, `module`, and the MCP
  transport plus tool list.

## Dependencies

`internal/pipeline`, `internal/agent`, `internal/rungraph`, `internal/nimclient` (the one remote
tool).

## Downstream effects

This is a published interface. Renaming or removing a tool breaks every configured client, and
changing a tool's argument schema breaks callers silently — the calling model simply starts getting
errors it will try to work around.

## Invariants and assumptions

1. **The manifest and the registered tools must agree.** A drift test enforces it, so adding a tool
   without updating `.printing-press.json` fails the build. Currently 33 declared, one per `Name:` literal in `mcpserver.go` (some register only behind their config gate — `offload_browse` only on a box that configured the browse lane);
   the manifest's `version` tracks `VERSION` release by release. This test arrived via an outside
   contribution after the manifest had silently drifted to claiming four tools.
2. A Defer is a successful result. Do not map it to an MCP error.
3. `offload_nim` is the only remote MODEL surface, and it is opt-in. `offload_browse` is also opt-in and
   asks only a loopback decision endpoint the operator runs (ADR 0060).
4. `offload_status` with no argument answers byte-for-byte what it answered before `section`
   existed. A golden captured from the pre-change handler on a fixture that owns every
   machine-dependent input pins it (`TestStatusDefaultIsByteIdenticalToTheGolden`). A deliberate
   change to the full payload regenerates it with `OFFLOAD_UPDATE_GOLDEN=1` and says so in the
   changelog.

## Error handling

Tool errors return as errors; Defers return as results with `deferred: true`. The distinction matters
to the calling agent, which should retry neither — it should do the work itself on a Defer, and
diagnose on an error.

## Security and privacy notes

The stdio transport inherits the trust of whoever launched the process. `agent_run` exposes the
coding agent, and therefore its capability flags — the defaults there are what keep this surface
read-only unless deliberately widened. See
[ADR 0003](../architecture/decisions/0003-policy-broker-and-capability-flags-off-by-default.md).

## Observability and debugging

- **A server that started before the binary it runs was replaced says so (`stale_binary`).** An MCP server is spawned once
  per client and keeps the executable image it started from, so a release swapped in on disk afterwards is not in that
  process: two servers that had started on 0.180.0 were still stamping their rows with it eight hours after a 0.181.0 and then
  a 0.182.0 binary had been swapped in, so the capacity-wait fix those releases carry could not run for them. The server takes the size and modification time of the image it started from
  out of the identity the process captured first thing (`buildinfo.CaptureStartup`, see `process` below;
  `internal/mcpserver/stalebinary.go`) and compares them with the file at the path the capture recorded when `agent_delegate`,
  `offload_research` or `offload_status` answers. A stamp that has moved is settled by the bytes, with the check the `process`
  block makes: equal bytes are not stale (a redeploy of the same build rewrites the file), different bytes are, and bytes that
  cannot be compared leave the moved stamp standing, with a note that it rests on the stamp alone and the reason the comparison
  gave (a lock on the freshly renamed file, a read that did not finish within 2 s). That read runs in the background, once per
  moved file, and an answer waits for it at most 2 s: a stalled read (an antivirus scan, a slow mount) never holds the results
  of a finished call, and the next call uses what the read found. A path that is gone (an OS
  that unlinks the old file) is stale without a comparison. A stale answer carries a top-level `stale_binary` string naming the
  running version, the two stamps and, when the installed file's build marker reads, the installed version, and saying to
  reconnect this server with `/mcp` (select `local-offload`, Reconnect) or restart the MCP client; the log says it once. It never exits, re-executes or cuts a call (that would drop the jobs the server
  holds), a stat that fails for any other reason accuses nothing, and a server that could not read its own file at start has
  nothing to compare with. The running version is the compiled-in one; the installed version is read from the installed file's
  build marker (`gpulease.ReadBuildMarkerVersion`), never by running it.
  The guard says when it is not watching, because a guard against a silent failure must not be one: a server that could not
  stamp its own executable at start (or whose path was already gone, or whose start-up capture holds no path or no stamp, which
  buildinfo logs) logs one `the stale-binary guard is inactive` line with the reason, and a stat that fails later is logged
  once. `agent_delegate`, `offload_research` and `offload_status` all carry a top-level `stale_binary_guard` string
  (`inactive: ...` for the first, `unreadable: ...` for as long as the second lasts), beside and independent of `stale_binary`,
  and no such key while the guard is watching. The start-up capture takes the stamp from the path when the hash could not open
  the file, so a refused open does not leave the guard to stamp the file as of `mcpserver.New`.
  **Every door says it, on every result (the in-band notice).** The three doors above carry `stale_binary` in their own answer
  only on the path that ran the work: every refusal before it (bad arguments, no subtask, no url, a pin reason, a section that
  does not exist) and the config gate's deferral answer with no key, and a session that called any other door
  (`offload_summarize`, `offload_ask`, a media door, `agent_run`) was never told that its server runs an old image.
  `staleNotice` (`internal/mcpserver/stale_notice.go`, a receiving middleware registered after the config gate, so that a config
  deferral carries it too) appends the same note as a **second text block** to the result of every `tools/call` that does not
  carry the signal already, as `stale_binary: <the note>`. It decides from the result, not from the door's name: a first block
  that is a JSON object holding `stale_binary` or `stale_binary_guard` at its top level is the door's own signal and is passed
  through with no check (the door already stat the file, and may have hashed it, for its answer, and the signal must be there
  once); anything else gets the block, the early returns of those three doors included. A cheap substring test comes first and
  the parse only settles an answer that has the text of a key in it, so a summary that quotes the key (nested, in a string, a
  longer key, text that is not JSON) is still told. `Content[0]` is never touched, so a client that reads only the first block
  still gets the door's JSON, and the block is the very string `offload_status` carries under `stale_binary` (one definition, one
  cache, no way for the two to disagree). It rides on every call of a stale server, with no cap and no config key: a stale server
  stays stale until it is reconnected, and a cap would bring the silent failure back. A test reads the call sites in
  `mcpserver.go` and keeps the list of the doors that carry the key in their own answer in step with them, so a fourth door that
  gains the keys is given the same early-return and invalid-config cases in the same change. A guard that is not watching rides the same way as `stale_binary_guard: ...`, on the first call that finds it so
  and then on every 20th such call, because that is a standing condition of the process and not news. The result is copied before
  the block is appended (a handler may keep what it returned), a call that failed, a result with no content and a nil result pass
  through as they are, and the check runs after the handler finished, bounded as above (at most 2 s on the first call after a swap).
  A server that started before this release has no middleware: only a reconnect or a restart of the client puts it on the new
  image. How each client renders a second block is for the release's live check to record; the three doors above keep the key in
  their own JSON on the path that ran the work, which does not depend on it. One limit is known from the client's documentation: a result larger than the
  client's output cap (`MAX_MCP_OUTPUT_TOKENS` in Claude Code) is stored in a file and the conversation sees a message naming it,
  so the notice of such a result lives in that file and not in the message.
  **The read does not block a rollback.** The installed file is opened through `gpulease.OpenShared`, which on Windows grants
  `FILE_SHARE_DELETE` (Go's `os.Open` grants only read and write sharing), for the hash and for the build marker
  (`openAudited`, the audit's opener, which `gpu doctor` reads every backup through too). With the notice on every door the first
  call of any door on a stale server reads the freshly installed file, and a read that did not share delete made a rollback or a
  second swap landing in that window fail on a sharing violation, with node-swap's holder diagnosis unable to see the stale server
  (its image path was renamed away).
  **Counting a rollout (runbook).** A rollout's effect (a gate that reads ledger rows after a deploy) counts only the rows whose
  `harness_build_sha256` equals the deployed build's hash: a session on an old image keeps writing the old hash until it is
  reconnected, so the rows themselves say which sessions are not on the build. The operator's deploy script reports the MCP servers
  it finds still on an older image at the end of each Windows leg and in its deploy record, under "Sessions still on an old
  image" (process id and start time, and the session when the owners registry names it). It is information only: it never changes a
  node's verdict and never stops anyone's server.
- `offload_status` reports harness state to the calling agent: the configured model roster (the one
  table in `config.ModelRoutes`, shared with `doctor`/`acceptance`/`report`), a live `/v1/models`
  probe through [`internal/swapclient`](../../internal/swapclient/swapclient.go), and
  `media.routes` — this machine's media capability **derived** from its
  bindings (`internal/mediacap`), never declared. See
  [media-generation.md](media-generation.md#capability-is-derived-never-declared) for the verdicts.
  On a COMPOSITE box it also carries `local.tier_profile`, `local.tiers` and `local.layers` (one
  row per device layer: the spec, each seat's live occupancy, the layer's admissibility and the
  reason), and `fleet.nodes[].layers` for each node that publishes them. The rows cost no model
  load — occupancy stops at `/running` for a cold seat — and the render is bounded: a stalled
  probe degrades to the spec rows rather than dropping the table. See
  [composite-tier.md](composite-tier.md).
- **One block, or the brief form (0.137.0).** `offload_status` takes one optional argument,
  `section`. No argument (or `all`) is the whole payload, byte-identical to the answer before the
  argument existed. A block name (`local`, `media`, `remote`, `accelerators`, `reuse`, `fleet`,
  `kv_cache_server`, `gpu_lease`, `pair`, `media_queue`, `process`) returns `{<block>: …}` and computes nothing else: the fleet block
  runs no nvidia-smi, the lease block probes no node. `accelerators` asked for by name on a box that
  lists none is `{}`, never `null`. `pair` is reported only with `pair_workloads_enabled` on: `{mode, relay?, reason?}`, which way
  this box's PAIR emitter reports (`local ingress`, `node-info fallback`, `relay` with the member's route URL, or `off` with
  the reason; see [pair-workloads.md](pair-workloads.md)). `brief` is the sizing answer: the whole `fleet` block plus two
  one-line strings under their own keys, so nothing that decodes `gpu_lease` or `local` as an object
  meets a string there. `gpu_lease_verdict` leads with the verdict word, then what the cards are
  doing, the holder and its reason, the queue length and the queue command. `local_verdict` gives
  the local endpoint's state and served count, the local agent seat's verdict, and the roster
  entries that are empty and so defer here. When the server holds a startup identity the brief also carries the full
  answer's `process` block (below), so a stale server is visible on the call a caller makes first. An unknown section, an unknown argument or a wrong type
  is a defer that lists the valid values, never a fall-back to the full dump. `config_error`, when
  set, stays the first key of every answer. The block table in `status_section.go` feeds both the
  dispatch and the schema's enum. Measured on the reference box (2026-09-22, three fleet nodes, a
  media lease held): full 18,768 bytes, `brief` 4,426 (23.6%), `fleet` 3,786 (20.2%), `gpu_lease`
  8,028 (42.8% of the full answer).
- **`process`: which image this server runs, and whether the installed build has moved on (D-36).** A rename-swap
  deploy replaces the installed binary while every MCP server already running keeps the image it started from, so a
  session could write rows with old behaviour for days and nothing said so (the register counted 13 of 18 MCP processes
  older than the deployed build). The block is `{version, build_sha256, installed_version?, stale_image?, note?}`:
  the version and the build hash this process stamps on its rows, and the comparison with the file now at the path it
  started from. `stale_image` is `true` when the installed bytes differ, `false` when they are equal, and **absent with
  a `note` when the installed file cannot be read** (never "fresh" on a failure); `installed_version` is the build
  marker the installed file carries. `mcpserver.Run` takes the identity from `buildinfo.CaptureStartup()` and holds it on
  the `Server`, so the block is built from what the server holds and not from the process global; a server that holds
  none (the test seam) has no `process` block, and asked for by name it is
  `{}`, as `pair`'s is. The hash is the one every row carries: `buildinfo.BuildSHA256` used to hash the executable
  lazily at its first call, so a process whose first delegation came after a swap stamped its rows with the NEW
  build's hash; `CaptureStartup` takes that one latch at start. A short CLI call never captures and keeps the lazy path
  (no 60 MB hash per hook invocation). The fleet node (`runFleetServe`) captures as its first statement too, so the
  `harness_build_sha256` on its agent answers is the image it started from (it runs no MCP server, so it serves no `process`
  block). A status call does not hash the installed file either: its stat is compared with
  the one the process saw at start, and only a moved stat is hashed (once per stat, then cached). A process that
  started before this build shows no block at all: it predates the field, so the first proof is the next restart.
  **`section:"brief"` carries the same block** under the same key (absent for a server that holds no identity), because the
  brief is the call the tool's description makes first and a stale server must be visible there. Only a read that
  succeeded is cached: a hash or marker read that failed once (an antivirus or indexer lock on the freshly renamed file) is
  retried on the next call, and a missing `installed_version` or a missing `build_sha256` (the process's own start-up
  hash failed) comes with a `note` saying why. The read of a moved file is bounded to 2 s per call and shared by the calls that
  arrive while it runs: past the bound `stale_image` is absent with a `note` that the read did not finish. `gpu doctor` marks the running processes older than the installed build
  ([gpu-lease.md](gpu-lease.md)). Ledger rows do not get a `stale_image` column: a
  reader compares a row's hash with the deployed one. **One definition, two doors.** `process.stale_image` (`offload_status`: the whole answer, `brief` or
  `section:"process"`) and the `stale_binary` note (`agent_delegate`, `offload_research`, `offload_status`, and the second block every other door carries) answer one question
  about one capture. `runMCP` takes the identity before it loads the config or opens the pipeline, so a swap landing during
  start-up is not hashed as the image the process runs; `mcpserver.New` starts the stale-binary guard from that identity (the
  stamp the capture read through the handle it hashed, or from the path when the open failed, not a stat taken after the
  pipeline opened; a capture with no stamp makes the guard inactive and says so) and `Run` holds it for the
  block. Stale means the file at the start path holds other bytes than the image the process started from. The guard looks at
  the cheap stamp first and asks the block's check (the hash, cached per stat, and the build marker) only once the stamp has
  moved, so a rewrite with equal bytes reads fresh on both, different bytes read stale on both, and the installed version is the
  one `installed_version` carries. When the bytes cannot be compared, `stale_image` is absent with a `note` and `stale_binary`
  keeps the moved stamp and says it could not compare; a path that is gone has no installed file to read, so the block gives
  no verdict and the guard says the file is no longer at its path.
- **Driver events on the leased cards (C-83-B).** While a lease is held and the GPU sample is on, `gpu_lease.activity` carries
  `driver_events` (the NVIDIA driver's recent fault events for the leased cards: Windows `nvlddmkm` System-log records, Linux
  `NVRM: Xid` kernel-log lines), `driver_events_unattributed` and `driver_events_note` as facts that never move the verdict;
  the reader is `statusDriverEvents` (wired only with the GPU sample, nil in the test fixture so no test starts PowerShell).
  Only the `gpu_lease` section and the full answer read them: the `brief` returns none, so it starts no read. See [gpu-lease.md](gpu-lease.md#the-drivers-own-fault-events-for-the-leased-cards-c-83-b).
- `local-offload doctor` checks the serving layer the tools depend on, and prints the same derived
  media routes — a route bound to a file that is absent exits non-zero.
- **The most common operational surprise:** an MCP client holds its server process for the session,
  so a rebuilt binary is not picked up until the client restarts. Newly added tools appearing absent
  almost always means a stale server process, not a registration bug.

## Testing notes

`internal/mcpserver/` covers tool registration and argument validation (`badargs_test.go`);
`agentrun_e2e_test.go` exercises the agent tool end to end. The manifest drift test
(`TestPrintingPressManifestListsEveryTool`) lives in `main_test.go` at the repo root, since the
manifest is a repo-root file.
`status_section_test.go` covers `offload_status`'s `section` argument over the in-memory MCP
transport: the default against the golden in `testdata/`, each block alone, the brief form, the
refusals, `config_error` ordering, the schema's enum, and that the fleet section never samples the
GPUs. The fixture holds a fixed startup identity (a fixture version over a fixture installed image), so the
`process` block is in the pinned answer with constant values and the test binary's own build hash never reaches the
golden. `stale_image_test.go` covers the block itself: stale after a swap to another version, fresh for the same
bytes, unknown (no key, a note) for an unreadable installed file, and no hash while the installed file's stat is
unchanged. `stalebinary_test.go` covers the guard on stamps alone, and `stale_agreement_test.go` pins that the two signals
agree: equal bytes under a newer stamp are fresh on both, different bytes are stale on both, bytes that cannot be compared
keep the stamp and say so, a path that is gone is stale without a comparison, and the guard starts from the captured identity
(a swap between the capture and `New` is stale).

## Common pitfalls

- Adding a tool and forgetting the manifest — the drift test catches it, which is the point.
- Expecting a Defer to be an error.
- Debugging "missing tools" without restarting the MCP client first.
- Assuming every tool is local: `offload_nim` is not, and `offload_browse` sends page text to a loopback
  decision endpoint that may front a hosted model.

## Source map

- [`internal/mcpserver/mcpserver.go`](../../internal/mcpserver/mcpserver.go) — registration and
  handlers
- [`internal/mcpserver/reviewdiffio.go`](../../internal/mcpserver/reviewdiffio.go) — the file seam of
  `offload_review_diff`'s `diff_path` branch (working directory, absolute path, confined read), so the refusals behind
  those calls are tested
- [`internal/research`](../../internal/research/fetch.go) — `offload_research`'s fetch and guard; the text sources:
  [`feed.go`](../../internal/research/feed.go) (RSS and Atom) and [`subtitles.go`](../../internal/research/subtitles.go)
  (the URL matcher, the yt-dlp subtitle adapter and the caption-to-text conversion); the tree kill the adapter uses on a
  deadline is [`internal/winexec/tree_windows.go`](../../internal/winexec/tree_windows.go) and
  [`tree_other.go`](../../internal/winexec/tree_other.go)
- [`internal/mcpserver/status_section.go`](../../internal/mcpserver/status_section.go) —
  `offload_status`'s block table, `section` parsing and the brief lines
- [`internal/mcpserver/stale_image.go`](../../internal/mcpserver/stale_image.go) — the `process` block: the
  startup identity the server holds and the stale-image comparison
- [`internal/mcpserver/stalebinary.go`](../../internal/mcpserver/stalebinary.go) — the stale-binary guard: the stamp of
  the image the process started from, compared with the file on disk and settled by the `process` block's byte comparison
- [`internal/mcpserver/stale_notice.go`](../../internal/mcpserver/stale_notice.go) — the middleware that appends the stale
  notice as a second text block to the result of every door that does not carry it itself
- [`internal/gpulease/openshared_windows.go`](../../internal/gpulease/openshared_windows.go) — the read open that shares delete,
  so a stale server's read of the installed image cannot block a rename of it
- [`internal/askjob`](../../internal/askjob/ask.go) — `offload_ask`’s contract builder (goal,
  output schema, and the grounded acceptance anchor)
- [`internal/askcache`](../../internal/askcache/askcache.go) — `offload_ask`’s content-addressed,
  bounded, per-process result cache (the `cache_hit` field)
- [`internal/reviewlane`](../../internal/reviewlane/review.go) — `offload_review_diff`’s contract
  builder, finding parser, diff-grounding filter and severity ranking; [`evidence.go`](../../internal/reviewlane/evidence.go)
  binds a result to the diff it judged (outcome, gate and seat receipts, second fingerprint check, durable trace)
- [`internal/swapclient`](../../internal/swapclient/swapclient.go) — the harness's single
  alias-aware llama-swap roster reader, over `tools/llamaswap`'s `pkg/llamaswap`
  ([systems/printed-clis.md](printed-clis.md#the-one-exception-the-harness-consumes-pkgllamaswap))
- [`internal/config`](../../internal/config/config.go) — `Config.ModelRoutes`, the one roster table
- [`internal/mcpserver/forecast.go`](../../internal/mcpserver/forecast.go) — `offload_forecast`: payload builder (inline, CSV, JSON), route seam, result spill to a file
- [`internal/forecastremote`](../../internal/forecastremote/forecastremote.go) — the route (`local`, `auto`, `remote`) and the fleet caller
- [`.printing-press.json`](../../.printing-press.json) — the declared manifest
- [`main_test.go`](../../main_test.go) — `TestPrintingPressManifestListsEveryTool` (the drift test)
- [`main.go`](../../main.go) — subcommand dispatch and MCP entry

## Related docs

- [browse-lane.md](browse-lane.md)
- [../architecture/decisions/0001-defer-never-cloud-fallback.md](../architecture/decisions/0001-defer-never-cloud-fallback.md)
- [../OPERATOR-GUIDE.md](../OPERATOR-GUIDE.md)
- [../../README.md](../../README.md) — full CLI and MCP tool tables
