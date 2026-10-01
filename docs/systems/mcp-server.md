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

**Named media families (ADR 0058).** `offload_generate_image` and `offload_edit_image_generative`
take a `family` param that selects one of the box's opt-in bindings beside its default one; the
edit tool also takes `images` (multi-reference, qwen-image-2.1 families) and both take
`transparent`. `offload_status` lists what a `family` can name under `media.image_families` /
`media.edit_families` (license, `commercial_use`, route verdict). A result from a family whose
`commercial_use` is false carries `license`, `commercial_use:false` and `license_note`; the tool
descriptions say so, so a calling agent never has to guess which outputs are research-only. See
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

`agent_run` drives the coding agent loop. Its default planner is the **agent seat** (config
`agent_model`, else the workhorse `model`; a per-call `model` argument overrides both — and on a
composite box (ADR 0052) the placement table decides when no per-call model is given, its seat
outranking `agent_model`, while a per-call model that belongs to an OPT-IN layer is admitted
only if that layer's guards admit it right now: a dormant layer refuses outright and the
display card refuses by the guard's name, before any seat is touched. Both agent doors take
`context_class: "long"`, an input to placement rather than a seat name; the delegation door's
subtasks also take `layer`, a composite node's declared layer id whose agent seat the subtask
runs on (register A-100: the Lenovo's `fast` layer is its 35B digest seat — a node that does
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
at the CORDON reports its `admission_wait_sec` / `admission_note` like every other admission exit. A resolved planner absent from the endpoint's served roster fails loud with
`deferred: true` naming the model, never a silent fall back to the workhorse — "served" means
matched against canonical ids **or** `meta.llamaswap.aliases`, since a tier-seeded `agent_model`
is normally an alias. Every response
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

`agent_delegate`'s `route` argument picks the placement rule (see
[fleet-node.md](fleet-node.md#placement-routes-and-the-retry-delegator-side) for the mechanics):
`auto` (default) runs local while the local seat is idle and considers the fleet only while it is
busy; `local`/`remote` force one side; `spread` deals every subtask across the local seat and every
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
doors put different things on the wire: for `agent_run` neither `read_root` nor `model` travels — the
executing node reads its own root and runs its own seat, so it is for self-contained goals and
`setup_actions` — while for `offload_ask` the files ride inline, so any node can answer. Omitted, or
`local`, keeps the old behaviour exactly.

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
for a strictly increasing value; a heartbeat has no new work to count). It is strictly opt-in and
additive: no token, no notification, and a slow client cannot slow a subtask (events go through a
bounded queue that drops rather than blocks). In the MCP TypeScript client SDK the token is sent only
when the caller passes `onprogress`, and the request timeout restarts on a progress update only when
the caller also sets `resetTimeoutOnProgress` (`maxTotalTimeout` is the absolute cap). Whether the
reference client does either is **unverified**, which is why the whole-call deadline above is a hard
limit that does not depend on it.

### The research lane (`offload_research`)

`offload_research` is the one-call answer to "this leg needs the web, so it goes to a cloud
subagent". It takes a goal and up to 12 public URLs, fetches every page DELEGATOR-side under
a public-web guard in TWO halves, strips it to text (dependency-free
HTML→text: scripts, styles and page chrome dropped, block boundaries kept; 2 MiB read cap,
96 KiB text cap), and builds ONE delegation contract per usable page — then runs the same
`delegate.Run` path `agent_delegate` uses (route `spread` by default, so pages are dealt
across the local seat and every eligible fleet node).

The guard's two halves are the point (ADR 0042, 0.117.3). By NAME,
`internal/research.ValidateURL` takes http/https only and refuses `localhost`, `.local`,
`.internal` and the configured tailnet zone — shapes an address cannot express — and it
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

What a digest owes beyond that is one design (register C-74): **presence is declared,
non-emptiness is asked for only where the caller marked it, and the default digest owes one
statement.** The default schema `{key_facts[], numbers[], quotes[], verdict}` declares all four
fields `required`, and a caller's `output_schema` keeps its own `required` entries and gains every
field its own `min_items:` / `nonempty:` acceptance reads, so a seat's direct JSON answer that
leaves one out fails validation and goes to the structured re-pack instead of being delivered and
failing acceptance after a whole run (36 of the 59 `min_items` failures on 2026-09-29). An item is
required of the digest only for the FIRST array the caller's own schema lists in `required` (one
check, never more than before) or through the caller's own `acceptance`; the default schema asks
for no items and a schema that marks nothing gets none, so a faithful "nothing on this page"
digest is a success (the old rule demanded an item from the alphabetically first array of any
schema). What the default digest does owe, on EVERY page anchored or not, is a `nonempty:verdict`:
a page too thin to anchor would otherwise carry no check at all, and a digest that said nothing
(every list empty, no verdict) would be delivered as a success. Empty lists with a verdict that
says so still pass.

A research page that fails only its own checks (a shape, an item count) is not re-run on another
node: a second seat given the same page mostly repeats the verdict at the cost of a whole second
run, and the result's `retry_note` says so. A failed document fingerprint (`docanchor`) is a
different fact: the answer is about another document, which is a property of the node (two of
them quarantine it), so that failure keeps its one retry on another node. The seats never gain
network access; the agent loop's egress cage is untouched. Failed or refused fetches come
back as `sources[].skipped` and produce no result — a broken page never reads as a digest.

The body marshals in a fixed order — `summary`, then `partial` / `error` (present only when a
batch chunk failed), then `results` with its `result_sources` index, and `sources` last — because
the MCP client keeps only the head and the tail of a long body, and the digests are the
deliverable (C-75). A **partial** result (some pages digested, some failed) is a successful
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

Four design choices are load-bearing rather than incidental:

- **The diff rides in the GOAL, not in a context doc.** A context doc becomes a file the seat
  must find with `list_dir` and open with `read_file`, and the measured failure mode of a small
  planner is calling no tool at all — which would produce confident findings about a diff never
  read. The cost is that `AgentContract.Validate`'s 256 KiB context cap never sees the diff, so
  `reviewlane.MaxDiffBytes` owns that bound and refuses early with the real numbers.
- **No acceptance check, deliberately.** An empty findings list is a CORRECT outcome, so any
  content check would either punish a clean diff or pass anything — the decorative acceptance
  `delegate.LintAcceptance` exists to name. What replaces it is a check the harness can actually
  make: a finding naming a file the diff never touched is dropped and reported as
  `dropped_ungrounded`, since an invented path is how a small seat fails here.
- **Findings arrive as an array of strings.** `gbnf.FromJSONSchema` compiles any array to an
  array of strings, so an object-item schema would have become strings anyway; the prompt asks
  for one `severity | file:line | claim | why` line per defect and `ParseFindings` reads them
  back tolerantly, keeping what it cannot parse as an unranked claim rather than dropping it.
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
- **Three counts say what is NOT in the list**, published on the same terms (present when
  non-zero): `dropped_ungrounded`, `dropped_echo` (the prompt's own field spec or worked example
  handed back as a finding — measured behaviour, so it is a byte-equality guard rather than a
  human's vigilance), and `truncated_by_cap`. The `note` on an empty list is gated on them:
  "found nothing" beside a non-zero drop count is false, and says so differently.

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
  `kv_cache_server`, `gpu_lease`) returns `{<block>: …}` and computes nothing else: the fleet block
  runs no nvidia-smi, the lease block probes no node. `accelerators` asked for by name on a box that
  lists none is `{}`, never `null`. `brief` is the sizing answer: the whole `fleet` block plus two
  one-line strings under their own keys, so nothing that decodes `gpu_lease` or `local` as an object
  meets a string there. `gpu_lease_verdict` leads with the verdict word, then what the cards are
  doing, the holder and its reason, the queue length and the queue command. `local_verdict` gives
  the local endpoint's state and served count, the local agent seat's verdict, and the roster
  entries that are empty and so defer here. An unknown section, an unknown argument or a wrong type
  is a defer that lists the valid values, never a fall-back to the full dump. `config_error`, when
  set, stays the first key of every answer. The block table in `status_section.go` feeds both the
  dispatch and the schema's enum. Measured on the reference box (2026-09-22, three fleet nodes, a
  media lease held): full 18,768 bytes, `brief` 4,426 (23.6%), `fleet` 3,786 (20.2%), `gpu_lease`
  8,028 (42.8% of the full answer).
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
GPUs.

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
