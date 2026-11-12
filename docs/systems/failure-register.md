# Failure register

## Purpose

Say which failures the harness has, how many of each, which kind of failure each is (the stack's own, capacity, the
caller's, a ceiling, the work), and what is known about why. The register has no owner column: a class carries its kind,
and the person who looks into it writes the cause and the fix.
`local-offload ledger --failures` reads the ledger and prints one JSON report: the failed jobs grouped into failure
classes, with a count, a kind, the doors, seats and models they came through, and the card work they spent. Each class
is then joined by name to the table on this page.

The split is the point. The counts come from the ledger. The **root cause** and the **structural fix** of a class are
a person's: they are written in the table below, and no code derives, guesses or fills them in. A class with no row
here reads `undocumented`, and a class whose row holds no root cause and no fix a person wrote reads as not
investigated (`uninvestigated_classes`), so a failure the harness keeps hitting without anyone having looked into it
is visible as a gap, not hidden. The table ships with those cells empty: nothing in it has been written by a person yet. The root causes found by reading
the code and the real rows are a separate catalogue, in code, printed with each class (see Root causes).

It is read-only. It never writes the ledger, starts nothing and calls no model.

## Questions this doc answers

- Which failure classes are the most frequent, and through which door, seat and model do they arrive?
- What does each class mean, what is known about its cause, and has a fix been recorded for it?
- Which classes does the ledger hold that nobody has documented, or investigated, yet?
- How much of the failure is the stack itself failing, and how much is a full queue, a caller's request, a ceiling or the work?
- Why is a class's token total `null`, and what does a `0` mean?

## How it works

`ledger --failures` is a mode of the existing `ledger` verb. It replaces the savings summary with this report; it
does not add to it, and the verb without the flag is unchanged.

1. It reads the ledger lock-free (the MCP server may be appending) and takes the job rows: a dispatch marker, an
   inner row whose parent is present and a refused attempt (below) are not jobs. Test, bake and measurement traffic is
   left out and counted in `excluded_non_production`; `--include-test-traffic` counts it. A non-blank line that does
   not decode as a ledger row is in no count and is counted in `unparsed_lines`: a row is a JSON object, so garbage, a
   cut-off line, a line holding `null` or any other non-object is unparsed, while `{}` is an object and so a row (a
   job that did not fail). A ledger file that is not there reads `ledger_found: false` with every count at 0, and a
   note on stderr.
2. A job row is a **failure** when it deferred, or carries a failing reason code, or failed its acceptance check. A
   cache hit and a dispatch marker never are.
3. Its **class** is its reason code, the closed set in `internal/ledger/reasoncode.go`. The set already holds the seven
   wire defer classes (the table at the end of this page). A row written before the code was stamped, with a code
   outside the set, or deferred and stamped `other`, is classed with the same classifier the ledger itself uses
   (`DeferCode`) and counted in `class_derived_rows` when that names a member.
4. Rows are grouped by class. Within a class they are broken down by door, seat and model. Each class has a **kind**
   (below), and the failures are also counted by kind.
5. Each class is joined to the table below by its name. This page is embedded in the binary at build time;
   `--table FILE` reads a different copy of the table instead (same columns).

Output is ordered by count, highest first, then by class name, and carries no clock reading, so the same ledger and
the same table give the same bytes. `--since DAYS` keeps only rows from that many days back, and the report says which
in `since_days` (0 = the whole ledger).

**Refused attempts are not failures.** When a node answers a dispatch with a capacity refusal (a 503 or a 429), the
delegator waits and places the subtask again. The refused attempt writes a ledger row of its own, marked `refusal`,
and the subtask's outcome is another row: the one that landed, or the closing row a lost subtask gets under a job id
of its own. Counting both read one subtask as two failures and inflated `queue_full` and `job_rows`. The register sets
a marked row aside and counts it in `refused_attempts`, with the same window and test-traffic rule as any row, so
queue pressure stays visible without being a failure. The `queue_full` class below is therefore a subtask that ended
refused, not a refusal seen on the way. A ledger written before the column existed has no row marked; its refusals
count as the failures they were then recorded as. No other job counter counts a marked row either: the savings
summary and its top defer reasons, the session split and the stats reports skip it by the same rule
(`ledger.CountsAsJob`).

### Kinds

A count of failures that puts a full queue, a caller's bad contract and a dead seat on one footing cannot answer
"how often does the stack itself fail". Each class therefore has a **kind**, written in code beside the closed
reason-code set (`internal/ledger/failurekinds.go`) and held to it by a test that fails for a class with none: it is
a fact about the class, never read from a row's text. `stack_fault_rows` and `stack_fault_share` report the kind
that says the stack failed on its own line, and `kinds` counts the failure rows of every kind.

| kind | meaning | classes |
|---|---|---|
| `stack_fault` | the stack itself failed or went silent: a node, a seat, a transport, a configuration that cannot work | `node_unreachable`, `job_lost`, `remote_error`, `seat_down`, `infrastructure`, `config`, `stall_admission`, `stall_cold_load`, `stall_prefill`, `stall_decode`, `stall_tool`, `stall_repack`, `stall_engine`, `stall_other` |
| `capacity` | the stack was healthy and full: a place in line, a refusal for room, a wait that ran out | `queue_full`, `queue_deadline`, `queue_withdrawn`, `capacity_wait`, `node_busy`, `shed`, `no_eligible_node` |
| `caller` | the request is what cannot be served as given: its contract, input or credentials, a door the node's policy keeps shut, or a caller that left | `canceled`, `dispatch_refused`, `contract`, `bad_input`, `write` |
| `ceiling` | a configured ceiling (a step budget, a poll budget) stopped work that may have finished behind a larger one | `poll_deadline`, `budget` |
| `work` | the stack ran and the work failed: no usable answer, or one that failed its checks | `failed_verification`, `abstention` |
| `unclassified` | the failure no class names; never counted as the stack's | `other` |

Some classes straddle two kinds, and the comment on each line of `failurekinds.go` says which way it was filed:
`no_eligible_node` is capacity (the delegator's own placement outcome), `poll_deadline` is a ceiling (the class does
not separate a job larger than the budget from a node that stopped progressing), `write` is the caller's (a node's
policy, or a write set past a door cap; neither a broken stack nor an unplaceable contract). The stack-fault share is
a share of `failure_rows` and counts only the classes filed as stack faults: an `unclassified` failure is in
`failure_rows` but never in `stack_fault_rows`, so while unclassified rows exist the share is a lower bound.

### Root causes

Every class of the closed set has an entry in `internal/ledger/rootcause.go`, beside the class definitions: a **cause kind**,
a **root cause** (one or two sentences), the **evidence** (the function that emits the class and the count read) and an
**action**. `ledger --failures` prints it with each class: in the JSON as `classes[].cause` (`kind`, `root_cause`,
`evidence`, `action`, `investigated`) and as text on stderr, one block per class, with `causes_not_investigated` naming
the classes in the window that are still open. The person-written table below it is a different thing and is unchanged:
its `root_cause` and `structural_fix` fields still come only from the page, and `cause.root_cause` comes only from
the catalogue.

The cause kinds are `capacity signal` (the stack was healthy and full), `node configuration`, `harness defect`,
`caller contract` (the request, its limits or its deadline), `upstream service` (an engine, a node host or a network
path failed) and `unknown`. They say whose fix the cause is, which `kind` (above) does not.

How the table was made (2026-11-12): the code that emits each class was read, then the rows of the 30 days to
2026-11-12 in the real ledger, and the whole ledger for a class with no row in the window. An entry is `investigated` only
when the emitter and those rows together settle the cause it states. Where they do not, the cause reads
`not investigated:` and says what would settle it. The counts are that snapshot; the live count of a class is its `count`.
A class with no entry reads as not investigated. **15 of the 31 classes are not investigated**: 8 have rows that do not
settle the cause (`failed_verification`, `poll_deadline`, `stall_admission`, `stall_cold_load`, `stall_prefill`,
`stall_decode`, `stall_repack`, `other`) and 7 have no row in the whole ledger (`job_lost`, `remote_error`, `shed`,
`stall_tool`, `stall_engine`, `stall_other`, `write`). In the 30 days to 2026-11-12, 4 of the 19 classes present were
not investigated.

| failure class | cause kind | root cause | evidence | action |
|---|---|---|---|---|
| `failed_verification` | unknown | not investigated: the rows say which check failed (`min_items` on plans, quotes or numbers; a regex) but not whether the seat's answer was wrong or the check too strict; the acceptance list is shape-only (delegate/lint.go warns of it) | delegate/run.go (failed verification); 357 rows in 30 days, 345 of them from research subtasks | not investigated: read the saved output of a sample of failed research subtasks against their acceptance lists |
| `queue_full` | capacity signal | Every node asked refused the dispatch for room: its queue was at fleet_max_queue_depth (the sampled bodies read `queue full (N jobs accepted+running, limit N)`) or its card was leased. The delegator files every 503 or 429 so. | delegate/run_capacity.go capacityRefusal; 49 rows in 30 days (of the three commonest reason groups, 22 rows read queue full and 1 node leased) | none: working as designed. A node with spare capacity can raise fleet_max_queue_depth. |
| `queue_deadline` | capacity signal | A node accepted the job and its backlog never reached it inside the queue patience, and the node did not confirm taking it back. Jobs queue behind other work on a full node. | delegate/run_remote.go (queue deadline after); 0 rows in 30 days, 318 in the whole ledger, all written before the withdrawal stamp existed | none: working as designed (the delegator withdraws and re-places since ADR 0064; queue_withdrawn is the same event confirmed). |
| `queue_withdrawn` | capacity signal | The same event as queue_deadline, and the node confirmed taking the job back: it sat in the node's backlog (`accepted`, never `running`) past the queue patience and was withdrawn. | delegate/run_remote.go; 186 rows in 30 days, 182 from research subtasks, 111 on one node | none: working as designed. If one node keeps most of them (111 of 186), check its concurrency against the headroom it advertises. |
| `poll_deadline` | unknown | not investigated: the class does not separate a job longer than the poll budget from a node that stopped making progress (failurekinds.go says so), and the 3 rows differ (a poll error, a stale last-progress, the node's own wall) | delegate/run_remote.go (poll deadline after); 3 rows in 30 days | not investigated: for each row, compare the node's own record of the job with the delegator's poll budget |
| `canceled` | caller contract | The caller's context ended (the caller went away, or the call's own deadline passed) before or while a subtask ran. It also holds the node's `agent loop: canceled (the parent context ended ...)`, which the wire files as a budget defer because the wire has no canceled class; the ledger had filed it `budget` until 2026-11-12. | delegate/calldeadline.go and ledger RefileStamped (pipeline/agenttask.go:1295); 29 rows in 30 days as stamped: `canceled: context canceled` and `the call's context ended before this subtask started`, and 17 more once the loop cancellations are read here | none: working as designed. |
| `node_unreachable` | upstream service | A node host did not answer on the network: a dial that timed out (`the connected party did not properly respond`) or a poll that never got an answer. Why the host did not answer (asleep, off, network) is not recorded on the row. | delegate/run_remote.go; 3 rows in 30 days, 2 on one node | none from the harness: the node host or path is the fault; `local-offload doctor` shows which nodes answer now. |
| `job_lost` | unknown | not investigated: the ledger holds no row of this class, so no failure of it has been observed; it settles when a row appears and its reason and node are read | delegate/run_remote.go (a poll 404 after an acknowledged dispatch); 0 rows in the 30 days to 2026-11-12 and 0 in the whole ledger | not investigated |
| `dispatch_refused` | node configuration | A node refused the dispatch itself: it rejected the delegator's bearer token (status 401 `unauthorized`, one node: a fleet_auth_token mismatch). Until 2026-11-12 the class also held a node whose own data folder could not be created (`creating pipeline-jobs dir`, an ack-time 400); that is a fault of the node's configuration and not of the request, and it is now filed `config`. | delegate/calldeadline.go (401 unauthorized); 145 rows in 30 days as stamped, 145 on one node's seat: 115 are the 401 and 30 the folder fault, which ledger RefileStamped now reads as config | re-pair that node's token (`local-offload doctor` checks fleet tokens); an edit on the node. |
| `remote_error` | unknown | not investigated: the ledger holds no row of this class, so no failure of it has been observed; it settles when a row appears and its reason and node are read | delegate/run_remote.go (a node's job reported errored, or its answer was unusable); 0 rows in the 30 days to 2026-11-12 and 0 in the whole ledger | not investigated |
| `capacity_wait` | capacity signal | No node had room for the subtask before the call's deadline: 248 of 267 waited until the call's own deadline (not agent_placement_wait_sec), 259 for one node's agent pool alias. | delegate/run_wait.go:436 and :442; 267 rows in 30 days, 259 on one pool alias | none: working as designed. Reducing it needs more worker slots on the pool's node, or fewer research subtasks per call. |
| `node_busy` | capacity signal | The seat or card the call needed was held by other work when it arrived: the seat was at its local run cap (`seat busy:`), a GPU lease of another class held the card (`gpu busy: held by ...`), or a media job waited for host RAM. It also holds the two text-lease holds filed elsewhere until 2026-11-12: the delegator's `local seat reserved (gpu lease ...)` (a text lease holds the only seat the call may use and no remote had room; filed `infrastructure`) and a node's `a text lease holds its card; it may take the call later` (filed `other`). The harness defers rather than overload the card. | pipeline/agenttask.go (seat busy, defer class capacity), pipeline/pipeline_gpu.go (gpu busy), ledger RefileStamped and DeferCode (the lease holds); 628 rows in 30 days as stamped: video 247, research 205, audio 109 (the agent pool at its cap, a media lease on the cards, host RAM), and 93 more once the lease holds are read here (79 from infrastructure, 14 from other) | none: working as designed. Fewer deferrals need more free card time (keep a card free for media while text leases run) or a higher run cap on a box with room. |
| `shed` | unknown | not investigated: the ledger holds no row of this class, so no failure of it has been observed; it settles when a row appears and its reason and node are read | delegate (sheddable work, priority -1, found no idle node); 0 rows in the 30 days to 2026-11-12 and 0 in the whole ledger | not investigated |
| `no_eligible_node` | upstream service | A lane found no fleet node able to take the call. In the sampled rows each node probed failed its health check with a timeout (`context deadline exceeded`), so the nodes were not answering, not refusing for room. Why they did not answer is not recorded. | the remote lanes' placement sentences (forecastremote and the others) and delegate route=remote; 76 rows in 30 days, 48 of them the vision lane | none from the harness: bring the nodes back (`local-offload doctor`); not investigated: why the probes timed out at the failure times. |
| `seat_down` | upstream service | The inference engine behind the seat was gone from llama-swap's /running, or refused connections, when the run needed it, and did not return inside the node's recovery wait. What stopped the engine is not recorded. | agent/seatdown.go:185; 89 rows in 30 days, 87 on one node's pool alias | not investigated: why that node's engine goes away; read its llama-swap log at the failure times. The harness already waits and re-issues (ADR 0066). |
| `stall_admission` | unknown | not investigated: the monitor reports a run silent in the admission phase; why is not on the row | agent/liveness.go; 0 rows in 30 days, 1 in the whole ledger | not investigated: read the node's engine log at the time of the row |
| `stall_cold_load` | unknown | not investigated: the rows say no byte came for 120 s after the seat read ready, on its first completion since the load; whether the bound is too tight for a cold seat or the engine was wedged is not settled | agent/liveness.go:268; 4 rows in 30 days, all on one node's pool alias | not investigated: time that seat's first completion after a load against the bound |
| `stall_prefill` | unknown | not investigated: the rows say no progress in prefill past an allowance computed from the prompt size and the seat's rate; a prompt larger than the allowance or a wedged engine both read so | agent/liveness.go; 0 rows in 30 days, 246 in the whole ledger (written before the stamp) | not investigated: compare the prompt size of those rows with the seat's measured prefill rate |
| `stall_decode` | unknown | not investigated: no progress while tokens were generated; the rows do not say whether the engine wedged or the allowance was too tight | agent/liveness.go; 0 rows in 30 days, 51 in the whole ledger (written before the stamp) | not investigated: read the engine log at the time of the rows |
| `stall_tool` | unknown | not investigated: the ledger holds no row of this class, so no failure of it has been observed; it settles when a row appears and its reason and node are read | agent/liveness.go (silence inside a tool call); 0 rows in the 30 days to 2026-11-12 and 0 in the whole ledger | not investigated |
| `stall_repack` | unknown | not investigated: no progress in the structured re-pack of an answer; why is not on the row | agent/liveness.go (`structured re-pack unreachable: stalled`); 0 rows in 30 days, 99 in the whole ledger (written before the stamp) | not investigated: read the re-pack requests of those rows against the seat's log |
| `stall_engine` | unknown | not investigated: the ledger holds no row of this class, so no failure of it has been observed; it settles when a row appears and its reason and node are read | agent/liveness.go (the engine did no work, or kept stepping without a token); 0 rows in the 30 days to 2026-11-12 and 0 in the whole ledger | not investigated |
| `stall_other` | unknown | not investigated: the ledger holds no row of this class, so no failure of it has been observed; it settles when a row appears and its reason and node are read | delegate/reasoncode.go stallReasonCode (a stall whose phase the reason does not name); 0 rows in the 30 days to 2026-11-12 and 0 in the whole ledger | not investigated |
| `budget` | caller contract | A limit the call carried ended the run before the agent finished: the call's own deadline (`call deadline reached`, `wall timeout after ... the caller's deadline`), or the step budget (`step budget exhausted`). Most are the caller's deadline, not a node ceiling. The agent loop's `agent loop: canceled` was filed here until 2026-11-12 and is now `canceled`; the browse door's `browse: budget:` (max_actions spent) is read here. | core/calldeadline.go, pipeline/liveness.go:411, pipeline/agenttask.go:1295; 250 rows in 30 days as stamped, 241 from research subtasks; 17 of them are loop cancellations that ledger RefileStamped now reads as canceled | give long research subtasks a longer call deadline or fewer subtasks per call; a run that ends on the step budget needs a larger max_steps. |
| `abstention` | node configuration | The answer was cut at its completion token cap, so there was nothing usable to return: the vision lane's `vision output truncated` (117 rows) and an agent answer cut at the completion budget so the re-pack was skipped (about 94 rows, `output_truncated`). The cap is a setting of the seat or the call. | pipeline/pipeline_vision.go:315 and pipeline/agentlistcap.go:75; 221 rows in 30 days, 107 on one vision seat's questions | raise the vision lane's output cap for that seat, or ask for shorter answers; not investigated: why those answers run past the cap. |
| `contract` | caller contract | The caller named something the node does not serve: an image family the node does not carry. | config/families.go:730; 4 rows in 30 days, all one family name on generate_image | the caller names a served family, or the node adds that family. |
| `config` | node configuration | A seat or node was set up so that it cannot serve the call: a seat's tool-call parser does not match its chat template, so a tool call came back as text (2 rows), or a node's configured data folder does not exist on that machine, so it could not create its pipeline-jobs folder for a dispatch (30 rows, one node; filed `dispatch_refused` until 2026-11-12). | pipeline/agenttask.go:1303 and fleetnode/tasks.go:1031 (read by ledger RefileStamped); 2 rows in 30 days as stamped, 32 once the folder fault is read here | set that seat's --tool-call-parser to the one its chat template needs; fix or remove the node's missing data folder (its `home`). |
| `write` | unknown | not investigated: the ledger holds no row of this class, so no failure of it has been observed; it settles when a row appears and its reason and node are read | pipeline/agenttask.go (the write door refused: agent_allow_write false, or a write set past a door cap); 0 rows in the 30 days to 2026-11-12 and 0 in the whole ledger | not investigated |
| `infrastructure` | upstream service | Mostly a seat whose engine was gone from llama-swap's /running (the node's defer class infrastructure carrying the same sentence as seat_down), then a vision seat that was cold and timed out, and the browse door's `browse: BROWSER_UNAVAILABLE:` and `browse: DECISION_UNAVAILABLE:`. The delegator's `local seat reserved (gpu lease ...)` was filed here until 2026-11-12; it is a capacity hold and is now `node_busy` (the wire class of that defer stays infrastructure, so the exit code and summary still count it). | agent/seatdown.go:185, pipeline_vision.go; 195 rows in 30 days as stamped: research 90, fleet 82; 72 of them were `local seat reserved (gpu lease ...)`: ledger RefileStamped now reads the 55 that name only remotes without room as node_busy, and the 17 that also name a failed health probe stay here | see seat_down; not investigated: why the engine goes away. |
| `bad_input` | caller contract | The input cannot be used as given: an image path that does not exist, a template variable the template does not declare. | pipeline (image load) and compose_video BAD_INPUT; 2 rows in 30 days | none: working as designed. The caller fixes the input. |
| `other` | unknown | not investigated: the class is the residue no rule names, a mix of causes. Since 2026-11-12 a node's `a text lease holds its card; it may take the call later` is read as node_busy, and the browse door's `BROWSER_UNAVAILABLE`, `DECISION_UNAVAILABLE`, `HOST_NOT_ALLOWED` and `budget` errors by their own classes; what is left is mostly the browse door's `blocked` (the model reported it cannot proceed, 4 rows) and media and compose sentences no rule names | reasoncode.go DeferCode (the residue); 55 rows in 30 days as stamped (audio 18, browse 15, video 7, compose 6), 30 once the rules above are read | not investigated: list the distinct reasons of the rows left and add a rule per sentence that ties to one class |

Reading the rows found three misfilings and two sentences left in `other`, all fixed on 2026-11-12 (`RefileStamped` and `DeferCode` in `internal/ledger/reasoncode.go`): a delegate row for a local seat held by a text lease (`local seat reserved (gpu lease ...)`) was filed `infrastructure` and is now `node_busy`, because the class is defined as the seat or card being held by other work (`capacity_wait` is the delegator finding no node with room, a different cause with a different fix); the agent loop's `agent loop: canceled` was filed `budget` and is now `canceled`; and a node's own folder failure (`agent contract: creating pipeline-jobs dir`, an ack-time 400) was filed `dispatch_refused`, a fact about the request or the credentials, and is now `config` (the node's configured home does not exist on that machine; `infrastructure` is for a stack part that failed while configured right). Rows written before the fix are corrected when the register is read: a row stamped with the old class and carrying the writer's own sentence is counted under the new one and in `class_derived_rows`; the ledger file is not rewritten. Rows written from now are stamped right by `Record`. The wire class of the lease-hold defer stays `infrastructure`, so the exit code and the summary count it as before. In `other`, a node's `a text lease holds its card; it may take the call later` is now `node_busy`, and the browse door's `BROWSER_UNAVAILABLE` and `DECISION_UNAVAILABLE` are `infrastructure`, `HOST_NOT_ALLOWED` is `contract` and `budget` is `budget`; its `blocked` (the model reported it cannot proceed) ties to no one class and stays in `other`.

### The fields

| field | meaning |
|---|---|
| `since_days` | the `--since` window the report covers, 0 for the whole ledger |
| `ledger_found` | false when the ledger file does not exist: every count is then 0 because nothing was read |
| `unparsed_lines` | non-blank ledger lines that did not decode as a row (garbage, a line cut off, a last line still being written); they are in no count |
| `job_rows`, `failure_rows` | the job rows looked at, and how many of them failed |
| `refused_attempts` | rows that are a node's capacity refusal at dispatch, set aside: neither jobs nor failures |
| `stack_fault_rows`, `stack_fault_share` | the failure rows whose class is a `stack_fault`, and their part of `failure_rows` (`null` with no failure row) |
| `kinds` | the failure rows per kind and their part of `failure_rows`; kinds with no row are absent |
| `class_derived_rows` | failed rows classed at read time: they carried no usable code, or deferred with the stamp `other` and a rule now names their class |
| `excluded_non_production` | test, bake and measurement jobs left out, per class (absent when none) |
| `undocumented_classes` | the classes present in the ledger that have no row in the table |
| `uninvestigated_classes` | the classes present in the ledger with no root cause and no structural fix written by a person (a class with no row is among them) |
| `classes[].class`, `kind`, `count`, `share` | the class, its kind, its rows, and its part of `failure_rows` |
| `classes[].example_reason` | the most frequent short reason among its rows (the smaller text on a tie) |
| `classes[].doors`, `seats`, `models` | the same rows counted per door, per seat and per model (on a delegation row the "model" is the seat alias, see below) |
| `classes[].groups` | the rows counted per (door, seat, model) together |
| `classes[].cards_tokens`, `rows_with_tokens` | the card work the class spent, and how many rows measured it |
| `classes[].documented`, `investigated`, `meaning`, `root_cause`, `structural_fix` | the joined table row, and whether a person wrote a root cause or a fix in it |
| `classes[].cause` | the catalogue entry (Root causes): `kind`, `root_cause`, `evidence`, `action`, `investigated` |
| `causes_not_investigated` | the classes in the window whose catalogue cause is `not investigated` |

**Failures by build (`--by-build`).** The ledger stamps the build that wrote each row, in `Record`, from the same
self-hash the delegation log uses and under its names: `harness_build_sha256` on a row for work the writing process ran
itself, `delegator_build_sha256` on a delegator's row (a delegation row and its dispatch marker, and any row naming a
remote node), and on a delegation row also the executing node's `harness_build_sha256` and, when its seat served,
`seat_config_sha256`, as the node's result reported them. The `builds` key holds one entry per bucket,
`{build, job_rows, failure_rows, classes}`, failure rows descending: its `job_rows` add up to the report's `job_rows`
and its `failure_rows` to `failure_rows`. A row is bucketed by `harness_build_sha256` when it has one (the build that
ran the work); else by `delegator_build_sha256`, named `delegator:<hash>` (the job never reached a node that reported a
build, or the node predates the stamp, so only the delegator's build is known); else `unstamped`. Every row written
before the stamp, and any row of a process whose self-hash failed, is `unstamped`: it is unknown, never a guess, and
that bucket shrinks only as new rows come in. The split is by build only; `seat_config_sha256` is on the rows but not a
bucket.

**Door, seat and model.** The ledger has no seat or model column. `model_tier` is its one identity column: on a
delegation row (task `agent_delegate`) it is `<node>:<seat alias>`, on every other row the model alias. The report
reads the **door** as recorded (`(unknown)` when a row has none) and the **seat** as `model_tier` as recorded. The
**model** is cut out of it on a delegation row alone: the part after the last `:`, so the same seat alias on two nodes
is one entry. That part is the seat alias (a pool name such as `agent-pool`): a seat, not a model, because the ledger
records no model name for a delegation, so for those rows `models` is the `seats` breakdown with the node stripped,
and reads as such. On any other row the model alias is kept whole, whatever colon it holds (`family:size`,
`engine:file`): cutting it named the wrong model. A forwarded accelerator row, whose `model_tier` is also
`<node>:<id>`, is not a delegation row and keeps both. A row with no `model_tier` reads `(unrecorded)` for both.

**Tokens are null, not 0, when nobody measured them.** `cards_tokens` is the sum over the rows that measured their
card work, and `null` when none did. A row measured it when the ledger holds a figure above 0 for it, or its older
token columns hold work, or its stored 0 is the row's own. A stored 0 is the row's own when its reason code says no
node ran the job (`node_busy`, `capacity_wait`, `queue_full`, `no_eligible_node`, `queue_withdrawn`, `shed`,
`dispatch_refused`, `config`, `contract`, `bad_input`), when the row names no node, or when it is the
node's own row for a job another box asked for. A delegator's row for a job a node owned (a `poll_deadline`, a
`node_unreachable`, a `failed_verification` that came back from a node) stores 0 because the node's work is not on
that row: the report carries it as unmeasured, never as a zero. Three codes are left off the no-node-ran list on
purpose: `queue_deadline` (the node may still run a job it never confirmed taking back), `canceled` (the caller
left after a node held the job) and `write` (a node refuses the write door either before a run, when
`agent_allow_write` is false, or after one, when the finished write set broke a door cap, and the class does not say
which). A row written before the `cards_tokens` key reads its older columns only. A class
whose `rows_with_tokens` is below its `count` has a floor, not a total, and an unmeasured row is never added as zero.

## Maintaining the table

The table is the human-maintained half of the register.

- To record what is known about a class, edit its row: `root cause` is why it happens, `structural fix` is the change
  that would stop it. An empty `root cause` cell reads `not recorded` and an empty `structural fix` cell reads `open`,
  and a class whose two cells are empty (or read exactly those words) is **not investigated**: it is listed in
  `uninvestigated_classes`.
- A new reason code needs a new row. `TestEmbeddedFailureTableCoversEveryReasonCode` fails until there is one, and
  fails again for a row that names a code the ledger does not define.
- **No row of this table has been written by a person.** `meaning` states what the class is, from the constants' own definitions. The
  `root cause` and `structural fix` cells ship empty on purpose: filling them with a restatement of those definitions made
  every class look covered and defeated the gap list. A cell gets text only when a person has looked at an observed
  failure and wrote down what they found; `TestEmbeddedFailureTableShipsOnlyInvestigatedCells` fails for a non-empty
  cell until the class is named in that test with what the finding rests on. `open` means no fix is recorded
  anywhere, not that none is possible.

| class | meaning | root cause | structural fix |
|---|---|---|---|
| `failed_verification` | A result arrived and failed the contract's own acceptance checks. The media doors also file their quality gates here: a black or frozen clip, dead air, invalid depth frames, an unmeasurable output. | | |
| `queue_full` | A subtask that ended refused: every node the delegator asked refused it for capacity (queue full, draining, leased, shed by the node, or a 429). One refusal that the delegator waited out and placed the subtask again after is not this class; it is a refused attempt (`refused_attempts`), beside the row that is the subtask's outcome. | | |
| `queue_deadline` | A node accepted the job and never started it before the queue patience ran out, and did not confirm taking it back, so it may still run. | | |
| `queue_withdrawn` | The same, and the node took the job back (ADR 0064): it will never run there. | | |
| `poll_deadline` | A node owned the job and it did not finish inside the poll budget. | | |
| `canceled` | The caller went away. | | |
| `node_unreachable` | No node answered: a dial failure, a dropped connection, or a poll that never got an answer. | | |
| `job_lost` | A node denied ever holding the job it had acknowledged (a 404 on poll). | | |
| `dispatch_refused` | A node refused the request itself (400, 401, 403, 409 and the like). | | |
| `remote_error` | A node reported that the job errored, or its answer could not be used. | | |
| `capacity_wait` | No node had room within the placement wait. The delegator's own outcome: no node ran the job. | | |
| `node_busy` | A node, or this box's own seat, took the job and answered a capacity defer after admission: the seat was at its run cap (`seat busy:`), a GPU lease or fence held its card (`gpu busy:`), or the engine was held busy by other work. The wire defer class `capacity` is filed here. | | |
| `shed` | Sheddable work (priority -1) found no idle node. | | |
| `no_eligible_node` | `route=remote`, or a failing fleet, with no node eligible. | | |
| `seat_down` | The seat the job needed could not serve it: unreachable, down under the run and not back inside the node's recovery wait (`seat down:`, ADR 0066), or answering llama-swap's 5xx past the busy-seat wait (`seat not serving:`). | | |
| `stall_admission` | The run went silent in the admission phase (ADRs 0055 and 0061): the node's own liveness verdict, filed as infrastructure. | | |
| `stall_cold_load` | The run went silent while the model was loading cold. | | |
| `stall_prefill` | The run went silent while the prompt was being processed. | | |
| `stall_decode` | The run went silent while tokens were being generated. | | |
| `stall_tool` | The run went silent inside a tool call. | | |
| `stall_repack` | The run went silent in the structured re-pack of its output. | | |
| `stall_engine` | The seat's engine did no work for anyone, or kept stepping without producing a token: a wedged or thrashing seat. | | |
| `stall_other` | A stall whose phase the reason does not name. | | |
| `budget` | A ceiling stopped the run: the step budget or a time ceiling. The answer may exist behind a larger one. The wire defer class `budget`. | | |
| `abstention` | The model was asked, ran, and could not produce a usable answer. The media doors also file an empty transcript, an empty or truncated vision output and non-JSON output here. The wire defer class `abstention`. | | |
| `contract` | The caller's contract cannot be placed or run on any node, however healthy the fleet is: no `output_schema` for a remote placement, a contract already past the origin hop, a token estimate no advertised ceiling can hold; the media doors file an unknown audio kind, voice or image family here. The wire defer class `contract`. | | |
| `config` | The node and contract combination can never work as configured: no seat resolvable, the seat not served, an unknown profile, a media route not configured or not ready. The wire defer class `config`. | | |
| `write` | The contract asked for the write door and the node will not open it: `agent_allow_write` is false there, or the finished write set broke a door cap. The wire defer class `write`. | | |
| `infrastructure` | Something in the stack failed: an endpoint unreachable, an agent build failed, a transport died mid-run; the media doors file engine crashes, out-of-memory and a full disk here. The wire defer class `infrastructure`. | | |
| `bad_input` | The call's own input cannot be processed as given: an audio file with no audio stream, a picture that does not load, a video that cannot be sampled. | | |
| `other` | A failure no member above names. The classifier is total, so a row is never without a class; a growing share here means the set needs a new member. | | |

`ok` and `started` are members of the closed set too, but they are not failures and have no row.

### Reading a row written before the code was stamped

A row with a stamped reason code is read as stamped, with two exceptions. `other` is what the ledger files when no member
named the row at write time, so a deferred row stamped `other` is classed again and keeps `other` unless a rule below now
names a member. And three stamps were wrong until 2026-11-12 (`RefileStamped`, applied by `Record` to new rows and by the
register to old ones, and counted in `class_derived_rows` too): `infrastructure` on a reason holding `local seat reserved (` (unless the same reason says the remote fallback failed on a fault: a failed health probe, no remote configured, none past the capability gate, or no advertised context ceiling; that row stays `infrastructure`)
is `node_busy`; `budget` on a reason starting `agent loop: canceled (the parent context ended` is `canceled`; and
`dispatch_refused` on a reason holding `agent contract: creating pipeline-jobs dir` is `config`. Each needs the wrong stamp
and the writer's own sentence, so no other row of those classes moves. A deferred row without a code is classed the same way. Both are counted in `class_derived_rows` and classed
by `DeferCode` in [`internal/ledger/reasoncode.go`](../../internal/ledger/reasoncode.go): its error
class first, then the prefix of its reason, and `other` when neither names it. The prefix rules cover only sentences the
harness itself writes, and each maps to the class its own writer files the same defer under. A rule is added only when a
real sentence in the source ties to an existing class without doubt; the sentence stays in a comment beside it.

- `wall timeout after` and `step budget exhausted` are `budget`; `output failed schema` is `abstention`.
- `failed verification:` is `failed_verification`; `seat down:` is `seat_down`.
- `queue deadline after` is `queue_deadline`, or `queue_withdrawn` when the sentence ends with the node's confirmed
  withdrawal ("the job was withdrawn from the node, which will never run it").
- `stalled: no progress` (bare, or behind `structured re-pack unreachable:`) is the stall class of the phase word in it:
  `stall_prefill`, `stall_decode`, `stall_repack` and the others, read as the delegator reads them.
- `no fleet node is eligible for the ...`, `no fleet node serves ...` and the accelerator forward's `no fleet node
  advertises ...` are `no_eligible_node`.
- `dispatch <url>: status 503|429: ...` is `queue_full`: the delegator files every capacity refusal of a dispatch so,
  whatever the node's body says (queue full, node leased, too many requests).
- `a text lease holds its card; it may take the call later`, the whole sentence of a node's capacity defer on a media lane, is
  `node_busy`; the shorter `a text lease holds its card` also ends a probe list that found no node, and is not matched.
- The browse door's `browse: BROWSER_UNAVAILABLE:` and `browse: DECISION_UNAVAILABLE:` are `infrastructure`,
  `browse: HOST_NOT_ALLOWED:` is `contract` and `browse: budget:` is `budget`.

A door that holds a result's wire defer class (the remote lanes and the accelerator forward) stamps the code when it
writes the row: the error class and the sentence first, then the defer class (`capacity` is `node_busy`), so such a defer
is no longer left to read time. A deferred row whose writer gave no reason is filed with the reason
`deferred: no reason recorded`, which the register reads as `other`.

Not yet ruled, because the sentence does not tie to one class without doubt: a bare `node leased`, `no node that answered
is called ...`, `no fleet node can take this edit now` and the browse door's `blocked` (the model reported it cannot proceed) and its other error classes.

### Wire defer classes

A node's defer carries one of seven classes on the wire (`core.DeferClass*`). The ledger files each under a reason
code; this table is checked against both.

| wire defer class | filed under |
|---|---|
| `abstention` | `abstention` |
| `budget` | `budget` |
| `infrastructure` | `infrastructure` |
| `config` | `config` |
| `contract` | `contract` |
| `capacity` | `node_busy` |
| `write` | `write` |

## Interfaces and entry points

- `local-offload ledger --failures [--by-build] [--since DAYS] [--include-test-traffic] [--table FILE] [--config FILE]`:
  the report, JSON on stdout and nothing else. A note about the left-out traffic, a missing ledger or lines it could not
  read goes to stderr. `--failures=false` is the savings summary, as for any boolean flag. `--by-build` adds the `builds`
  key (below) and prints the same split as a short table on stderr; without it the output is what it was. Every run also
  prints each class with its cause and action as text on stderr (the JSON carries them as `classes[].cause`).

## Invariants and assumptions

- The register reads the ledger and the table (the page embedded in the binary, or the file `--table` names) and
  nothing else; it never calls a model and never writes a file.
- Counts, shares, ordering and the example reason are deterministic functions of the ledger rows.
- A class with no rows in the window is not listed.
- A refused attempt is never a job and never a failure; it is counted in `refused_attempts`.
- A line the register cannot read is counted in `unparsed_lines`, never dropped silently; a missing ledger is
  `ledger_found: false`, never a quiet zero.
- An unmeasured token total is `null`, never `0`.
- `root_cause` and `structural_fix` come only from the table. A class with no row says `undocumented`. `cause` comes only
  from the catalogue in code, and a class with no entry there reads `not investigated`.

## Testing notes

`ledger_failures_test.go` runs the verb on a synthetic ledger and table under `testdata/failure-register/` and compares
the output with a committed golden file. `internal/ledger/failures_test.go` pins the reader's rules one by one: the
token presence rule, the read-time classification, the window, the breakdown order, the refusal rule, the kinds and
the unreadable lines.

## Source map

- [ledger_failures.go](../../ledger_failures.go): the `--failures` mode, the table parser and the join.
- [internal/ledger/failures.go](../../internal/ledger/failures.go): the reader that groups the rows.
- [internal/ledger/failurekinds.go](../../internal/ledger/failurekinds.go): the kind of every failure class.
- [internal/ledger/rootcause.go](../../internal/ledger/rootcause.go): the root-cause catalogue.
- [internal/ledger/reasoncode.go](../../internal/ledger/reasoncode.go): the closed reason-code set and the classifier.
- [internal/core/agentwire.go](../../internal/core/agentwire.go): the wire defer classes.
- [ledger_failures_test.go](../../ledger_failures_test.go): the acceptance tests.
- [internal/ledger/failures_test.go](../../internal/ledger/failures_test.go): the reader's tests.
- [testdata/failure-register/](../../testdata/failure-register/ledger.jsonl): the synthetic ledger, table and golden output.

## Related docs

- [offload-pipeline.md](offload-pipeline.md): the cascade, whose defers fill most of these classes.
- [fleet-node.md](fleet-node.md): the node contract behind the node-side classes.
- [ADR 0064](../architecture/decisions/0064-a-delegator-takes-back-what-it-has-not-started.md): queue withdrawal.
- [ADR 0055](../architecture/decisions/0055-walls-are-ceilings-liveness-is-progress.md) and
  [ADR 0061](../architecture/decisions/0061-liveness-judges-the-seat-engine-busy-hold.md): the stall classes.
- [ADR 0066](../architecture/decisions/0066-a-seat-that-goes-down-is-waited-for-and-the-failed-step-reissued.md): a seat that goes down.
