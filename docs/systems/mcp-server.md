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

**Twenty-nine tools** are registered on every box, in families. `.printing-press.json` lists all 31
the code can register, and a drift test holds the two together. The advertised set is per-box:
`agent_delegate` and `offload_research` are gated on `agent_delegation_enabled`, and a box listing
an accelerator registers its own — 11 for the Hailo-8L, 4 for the Coral, 3 for the RKNPU (see [accelerators.md](accelerators.md)). Read `tools/list` rather
than any number written down:

| Family | Tools |
|---|---|
| Text offload | `offload_summarize`, `offload_classify`, `offload_extract`, `offload_triage` |
| Vision | `offload_vqa`, `offload_assess_image`, `offload_extract_image`, `offload_video_describe`, `offload_video_watch` |
| Speech / OCR | `offload_transcribe`, `offload_ocr` |
| Media generation | `offload_generate_image`, `offload_generate_video`, `offload_animate_character`, `offload_generate_audio`, `offload_generate_svg` |
| Media editing | `offload_edit_image`, `offload_inpaint_image`, `offload_edit_image_generative`, `offload_upscale_image`, `offload_media` |
| Composition (CPU-class, no GPU lock; ADR 0059) | `offload_compose_video` |
| Graph execution | `offload_run_graph` |
| Agent | `agent_run`, `offload_ask`, `offload_review_diff`, `agent_rig` |
| Delegation (opt-in: `agent_delegation_enabled`) | `agent_delegate`, `offload_research` |
| Browser (opt-in lane; registered only when configured; ADR 0060) | `offload_browse` |
| Remote (opt-in) | `offload_nim` |
| Status | `offload_status` |

**A place in line (`waiter_token`, GPU routing P13b).** Every media tool (`offload_generate_image`,
`offload_edit_image_generative`, `offload_inpaint_image`, `offload_upscale_image`, `offload_generate_video`,
`offload_animate_character`, `offload_generate_audio`, `offload_run_graph`) takes an optional `waiter_token`. On a
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

`offload_transcribe` takes an optional `route` too (0.164.0, [ADR 0072](../architecture/decisions/0072-a-fleet-node-transcribes-audio-its-caller-uploads-so-a-held-card-is-a-place-in-line.md);
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
too (`chosen | queue | cap | slow | lease | cold | probe | unfit(ctx) | noschema`) — read it before
assuming the fleet was even consulted.

Since 0.130.2 (register C-46) `route` is accepted on **`agent_run` and `offload_ask`** too. Both
doors ran local unconditionally before, so a remote seat could not be named from this box at all.
`remote` / `auto` / `spread` / `queue` sends the call as ONE contract through the delegator's
single-contract path, and the response names `node`, `placement`, `seat` and `executed_on`. The two
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
`read_root` on this box and stays.

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
`executed_on`, and carries no sizing of this box: the wall is the executing node's expectation (ADR 0055).

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
so the page does not address it" still fail it. The excuse counts for the node as neither a strike nor a pass
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
them quarantine it), so that failure keeps its one retry on another node. The seats never gain
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
never stored, so it is always a fresh run.

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
(`executed_on`, `node`, `placement`, `seat`) and the fence that moved it, and the findings go
through the SAME filters as a local review (`publishReview` is shared, so the two cannot drift).
An INHERITED lease is not a fence: `gpu reserve … -- <session>` sets `GPU_LEASE_EPOCH`, and the
holder's own review stays on the cards its lease cleared. When the fleet takes nothing, today's path
stands — the wait, then the capacity defer — and the result carries a `fleet` note saying the fleet
was asked and why it declined. That defer's reason now also carries the holder's DECLARED window
(`, declared until 11:40PM (~37m0s left)` — `modelaffinity.LeaseError`), so "gpu busy" finally
answers *when to retry*.

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
  a cut one is split on newlines and loses its last line) and `publishReview` runs them through the
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
  through to a local wait behind the very lease that fenced it; a salvage that comes to no review
  falls through exactly as before. `internal/pipeline`'s `TestReviewLaneReadsWhatTheRunnerFiles`
  runs the lane's real contract through the real runner so the node and the lane cannot drift
  apart. The pipeline's own ledger row still records the run as the defer it was; the salvage is
  the door's.

Everything the lane returns is ADVISORY: it never gates a merge and never substitutes for the
final does-it-actually-work verification, which stays with the caller — as do security review,
architecture judgement, and any call the caller is accountable for. Findings are triage input;
a `severe` label from a small local model is a prompt to read those lines, not proof. It shares
the ask lane's ledger gap above: `RunAgentContract` writes no delegation corpus row.

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
   without updating `.printing-press.json` fails the build. Currently 32 declared (measured 2026-09-28), one per `Name:` literal in `mcpserver.go` (some register only behind their config gate — `offload_browse` only on a box that configured the browse lane);
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
  from the identity the process captured first thing (`buildinfo.CaptureStartup`, see `process` below;
  `internal/mcpserver/stalebinary.go`) and compares them with the file at the path the capture recorded when `agent_delegate`,
  `offload_research` or `offload_status` answers. A stamp that has moved is settled by the bytes, with the check the `process`
  block makes: equal bytes are not stale (a redeploy of the same build rewrites the file), different bytes are, and bytes that
  cannot be compared leave the moved stamp standing, with a note that it rests on the stamp alone. A path that is gone (an OS
  that unlinks the old file) is stale without a comparison. A stale answer carries a top-level `stale_binary` string naming the
  running version, the two stamps and, when the installed file's build marker reads, the installed version, and saying to
  restart the MCP client; the log says it once. It never exits, re-executes or cuts a call (that would drop the jobs the server
  holds), a stat that fails for any other reason accuses nothing, and a server that could not read its own file at start has
  nothing to compare with. The running version is the compiled-in one; the installed version is read from the installed file's
  build marker (`gpulease.ReadBuildMarkerVersion`), never by running it.
  The guard says when it is not watching, because a guard against a silent failure must not be one: a server that could not
  stamp its own executable at start (or whose path was already gone) logs one `the stale-binary guard is inactive` line with the
  reason, and a stat that fails later is logged once; `offload_status` carries a top-level `stale_binary_guard` string
  (`inactive: ...` for the first, `unreadable: ...` for as long as the second lasts) and no such key while the guard is watching.
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
  hash failed) comes with a `note` saying why. `gpu doctor` marks the running processes older than the installed build
  ([gpu-lease.md](gpu-lease.md)). Ledger rows do not get a `stale_image` column: a
  reader compares a row's hash with the deployed one. **One definition, two doors.** `process.stale_image` (`offload_status`: the whole answer, `brief` or
  `section:"process"`) and the `stale_binary` note (`agent_delegate`, `offload_research`, `offload_status`) answer one question
  about one capture. `runMCP` takes the identity before it loads the config or opens the pipeline, so a swap landing during
  start-up is not hashed as the image the process runs; `mcpserver.New` starts the stale-binary guard from that identity (the
  stamp the capture read through the handle it hashed, not a stat taken after the pipeline opened) and `Run` holds it for the
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
- [`internal/mcpserver/status_section.go`](../../internal/mcpserver/status_section.go) —
  `offload_status`'s block table, `section` parsing and the brief lines
- [`internal/mcpserver/stale_image.go`](../../internal/mcpserver/stale_image.go) — the `process` block: the
  startup identity the server holds and the stale-image comparison
- [`internal/mcpserver/stalebinary.go`](../../internal/mcpserver/stalebinary.go) — the stale-binary guard: the stamp of
  the image the process started from, compared with the file on disk and settled by the `process` block's byte comparison
- [`internal/askjob`](../../internal/askjob/ask.go) — `offload_ask`’s contract builder (goal,
  output schema, and the grounded acceptance anchor)
- [`internal/askcache`](../../internal/askcache/askcache.go) — `offload_ask`’s content-addressed,
  bounded, per-process result cache (the `cache_hit` field)
- [`internal/reviewlane`](../../internal/reviewlane/review.go) — `offload_review_diff`’s contract
  builder, finding parser, diff-grounding filter and severity ranking
- [`internal/swapclient`](../../internal/swapclient/swapclient.go) — the harness's single
  alias-aware llama-swap roster reader, over `tools/llamaswap`'s `pkg/llamaswap`
  ([systems/printed-clis.md](printed-clis.md#the-one-exception-the-harness-consumes-pkgllamaswap))
- [`internal/config`](../../internal/config/config.go) — `Config.ModelRoutes`, the one roster table
- [`.printing-press.json`](../../.printing-press.json) — the declared manifest
- [`main_test.go`](../../main_test.go) — `TestPrintingPressManifestListsEveryTool` (the drift test)
- [`main.go`](../../main.go) — subcommand dispatch and MCP entry

## Related docs

- [browse-lane.md](browse-lane.md)
- [../architecture/decisions/0001-defer-never-cloud-fallback.md](../architecture/decisions/0001-defer-never-cloud-fallback.md)
- [../OPERATOR-GUIDE.md](../OPERATOR-GUIDE.md)
- [../../README.md](../../README.md) — full CLI and MCP tool tables
