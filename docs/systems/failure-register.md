# Failure register

## Purpose

Say which failures the harness has, how many of each, and what is known about why. `local-offload ledger --failures`
reads the ledger and prints one JSON report: the failed jobs grouped into failure classes, with a count, the doors,
seats and models they came through, and the card work they spent. Each class is then joined by name to the table on
this page.

The split is the point. The counts come from the ledger. The **root cause** and the **structural fix** of a class are
a person's: they are written in the table below, and no code derives, guesses or fills them in. A class with no row
here reads `undocumented` in the report, so a failure the harness keeps hitting without anyone having written it down
is visible as a gap, not hidden.

It is read-only. It never writes the ledger, starts nothing and calls no model.

## Questions this doc answers

- Which failure classes are the most frequent, and through which door, seat and model do they arrive?
- What does each class mean, what is known about its cause, and has a fix been recorded for it?
- Which classes does the ledger hold that nobody has documented yet?
- Why is a class's token total `null`, and what does a `0` mean?

## How it works

`ledger --failures` is a mode of the existing `ledger` verb. It replaces the savings summary with this report; it
does not add to it, and the verb without the flag is unchanged.

1. It reads the ledger lock-free (the MCP server may be appending) and takes the job rows: a dispatch marker and an
   inner row whose parent is present are not jobs. Test, bake and measurement traffic is left out and counted in
   `excluded_non_production`; `--include-test-traffic` counts it.
2. A job row is a **failure** when it deferred, or carries a failing reason code, or failed its acceptance check. A
   cache hit and a dispatch marker never are.
3. Its **class** is its reason code, the closed set in `internal/ledger/reasoncode.go`. The set already holds the seven
   wire defer classes (the table at the end of this page). A row written before the code was stamped, or with a code
   outside the set, is classed with the same classifier the ledger itself uses (`DeferCode`) and counted in
   `class_derived_rows`.
4. Rows are grouped by class. Within a class they are broken down by door, seat and model.
5. Each class is joined to the table below by its name. This page is embedded in the binary at build time;
   `--table FILE` reads a different copy of the table instead (same columns).

Output is ordered by count, highest first, then by class name, and carries no clock reading, so the same ledger and
the same table give the same bytes. `--since DAYS` keeps only rows from that many days back.

### The fields

| field | meaning |
|---|---|
| `job_rows`, `failure_rows` | the job rows looked at, and how many of them failed |
| `class_derived_rows` | failed rows classed at read time because they carried no usable code |
| `excluded_non_production` | test, bake and measurement jobs left out, per class (absent when none) |
| `undocumented_classes` | the classes present in the ledger that have no row in the table |
| `classes[].class`, `count`, `share` | the class, its rows, and its part of `failure_rows` |
| `classes[].example_reason` | the most frequent short reason among its rows (the smaller text on a tie) |
| `classes[].doors`, `seats`, `models` | the same rows counted per door, per seat and per model (on a delegation row the "model" is the seat alias, see below) |
| `classes[].groups` | the rows counted per (door, seat, model) together |
| `classes[].cards_tokens`, `rows_with_tokens` | the card work the class spent, and how many rows measured it |
| `classes[].documented`, `meaning`, `root_cause`, `structural_fix` | the joined table row |

**Door, seat and model.** The ledger has no seat or model column. `model_tier` is its one identity column: on a
delegation row it is `<node>:<seat alias>`, on a cascade row the model alias. The report reads the **door** as
recorded (`(unknown)` when a row has none), the **seat** as `model_tier` as recorded, and the **model** as the part
of the seat after its last `:`, so the same alias on two nodes is one entry. That part is a model alias only on a
cascade row. On a delegation row it is the seat alias (a pool name such as `agent-pool`): a seat, not a model, because
the ledger records no model name for a delegation. For those rows `models` is the `seats` breakdown with the node
stripped, and reads as such. A row with no `model_tier` reads `(unrecorded)` for both.

**Tokens are null, not 0, when nobody measured them.** `cards_tokens` is the sum over the rows that measured their
card work, and `null` when none did. A row measured it when the ledger holds a figure above 0 for it, or its older
token columns hold work, or its stored 0 is the row's own. A stored 0 is the row's own when its reason code says no
node ran the job (`node_busy`, `capacity_wait`, `queue_full`, `no_eligible_node`, `queue_withdrawn`, `shed`,
`dispatch_refused`, `config`, `contract`, `write`, `bad_input`), when the row names no node, or when it is the
node's own row for a job another box asked for. A delegator's row for a job a node owned (a `poll_deadline`, a
`node_unreachable`, a `failed_verification` that came back from a node) stores 0 because the node's work is not on
that row: the report carries it as unmeasured, never as a zero. Two codes are left off the no-node-ran list on
purpose: `queue_deadline` (the node may still run a job it never confirmed taking back) and `canceled` (the caller
left after a node held the job). A row written before the `cards_tokens` key reads its older columns only. A class
whose `rows_with_tokens` is below its `count` has a floor, not a total, and an unmeasured row is never added as zero.

## Maintaining the table

The table is the human-maintained half of the register.

- To record what is known about a class, edit its row: `root cause` is why it happens, `structural fix` is the change
  that would stop it. An empty `root cause` cell reads `not recorded` and an empty `structural fix` cell reads `open`.
- A new reason code needs a new row. `TestEmbeddedFailureTableCoversEveryReasonCode` fails until there is one, and
  fails again for a row that names a code the ledger does not define.
- The rows below start from what the code itself says. `meaning` and `root cause` restate the comments on the
  constants; they are not an investigation of any observed rate. `open` means no fix is recorded anywhere, not that
  none is possible.

| class | meaning | root cause | structural fix |
|---|---|---|---|
| `failed_verification` | A result arrived and failed the contract's own acceptance checks. The media doors also file their quality gates here: a black or frozen clip, dead air, invalid depth frames, an unmeasurable output. | The work or the output: the stack ran, and what it produced did not pass. | open |
| `queue_full` | Every node the delegator asked refused for capacity: queue full, draining, leased, shed by the node, or a 429. | Fleet capacity when the call was made. | open |
| `queue_deadline` | A node accepted the job and never started it before the queue patience ran out, and did not confirm taking it back, so it may still run. | Capacity: a node accepted more than it started and gave no answer about a withdrawal. | open |
| `queue_withdrawn` | The same, and the node took the job back (ADR 0064): it will never run there. | Capacity, as for `queue_deadline`; here the node confirmed the withdrawal, or its own record said so when polled. | open |
| `poll_deadline` | A node owned the job and it did not finish inside the poll budget. | Not separated by the class: a job larger than the poll budget, or a node that stopped making progress. | open |
| `canceled` | The caller went away. | The caller, not a fault in the stack. | open |
| `node_unreachable` | No node answered: a dial failure, a dropped connection, or a poll that never got an answer. | The network path to the node, or the node process. | open |
| `job_lost` | A node denied ever holding the job it had acknowledged (a 404 on poll). | The node's record of the job was gone when polled; why is not recorded. | open |
| `dispatch_refused` | A node refused the request itself (400, 401, 403, 409 and the like). | The request or the credentials, not capacity. | open |
| `remote_error` | A node reported that the job errored, or its answer could not be used. | The node-side run. | open |
| `capacity_wait` | No node had room within the placement wait. The delegator's own outcome: no node ran the job. | Fleet capacity measured against the placement wait. | open |
| `node_busy` | A node, or this box's own seat, took the job and answered a capacity defer after admission: the seat was at its run cap (`seat busy:`), a GPU lease or fence held its card (`gpu busy:`), or the engine was held busy by other work. The wire defer class `capacity` is filed here. | Another job held what the call needed: congestion at the node, not a fault. A seat at its cap is the node's admission; a capacity wait is the fleet having no room. | open |
| `shed` | Sheddable work (priority -1) found no idle node. | The shed policy: the contract was sheddable and no node had an idle slot. | open |
| `no_eligible_node` | `route=remote`, or a failing fleet, with no node eligible. | Nothing was eligible for the route the call named. | open |
| `seat_down` | The seat the job needed could not serve it: unreachable, down under the run and not back inside the node's recovery wait (`seat down:`, ADR 0066), or answering llama-swap's 5xx past the busy-seat wait (`seat not serving:`). | The seat itself, not its peers. | open |
| `stall_admission` | The run went silent in the admission phase (ADRs 0055 and 0061): the node's own liveness verdict, filed as infrastructure. | A silence in that phase; the class does not record what caused it. | open |
| `stall_cold_load` | The run went silent while the model was loading cold. | A silence in that phase; the class does not record what caused it. | open |
| `stall_prefill` | The run went silent while the prompt was being processed. | A silence in that phase; the class does not record what caused it. | open |
| `stall_decode` | The run went silent while tokens were being generated. | A silence in that phase; the class does not record what caused it. | open |
| `stall_tool` | The run went silent inside a tool call. | A silence in that phase; the class does not record what caused it. | open |
| `stall_repack` | The run went silent in the structured re-pack of its output. | A silence in that phase; the class does not record what caused it. | open |
| `stall_engine` | The seat's engine did no work for anyone, or kept stepping without producing a token: a wedged or thrashing seat. | The seat's engine. | open |
| `stall_other` | A stall whose phase the reason does not name. | Not recorded. | open |
| `budget` | A ceiling stopped the run: the step budget or a time ceiling. The answer may exist behind a larger one. The wire defer class `budget`. | A configured ceiling, or a task larger than it. | open |
| `abstention` | The model was asked, ran, and could not produce a usable answer. The media doors also file an empty transcript, an empty or truncated vision output and non-JSON output here. The wire defer class `abstention`. | The work, not the stack. | open |
| `contract` | The caller's contract cannot be placed or run on any node, however healthy the fleet is: no `output_schema` for a remote placement, a contract already past the origin hop, a token estimate no advertised ceiling can hold; the media doors file an unknown audio kind, voice or image family here. The wire defer class `contract`. | The caller's contract; the fix belongs to whoever wrote it. | open |
| `config` | The node and contract combination can never work as configured: no seat resolvable, the seat not served, an unknown profile, a media route not configured or not ready. The wire defer class `config`. | The node's configuration. | open |
| `write` | The contract asked for the write door and the node will not open it: `agent_allow_write` is false there, or the finished write set broke a door cap. The wire defer class `write`. | The node has not opted in, or the change was larger than a door cap. | open |
| `infrastructure` | Something in the stack failed: an endpoint unreachable, an agent build failed, a transport died mid-run; the media doors file engine crashes, out-of-memory and a full disk here. The wire defer class `infrastructure`. | The box or a service: retrying the same contract does not help until an operator fixes it. | open |
| `bad_input` | The call's own input cannot be processed as given: an audio file with no audio stream, a picture that does not load, a video that cannot be sampled. | The input file. | open |
| `other` | A failure no member above names. The classifier is total, so a row is never without a class; a growing share here means the set needs a new member. | Not classified. | Name the failure with a new member of the closed set (the rule is stated in `reasoncode.go`). |

`ok` and `started` are members of the closed set too, but they are not failures and have no row.

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

- `local-offload ledger --failures [--since DAYS] [--include-test-traffic] [--table FILE] [--config FILE]`: the
  report, JSON on stdout and nothing else. A note about the left-out traffic goes to stderr.

## Invariants and assumptions

- The register reads the ledger and nothing else; it never calls a model and never writes a file.
- Counts, shares, ordering and the example reason are deterministic functions of the ledger rows.
- A class with no rows in the window is not listed.
- An unmeasured token total is `null`, never `0`.
- `root_cause` and `structural_fix` come only from the table. A class with no row says `undocumented`.

## Testing notes

`ledger_failures_test.go` runs the verb on a synthetic ledger and table under `testdata/failure-register/` and compares
the output with a committed golden file. `internal/ledger/failures_test.go` pins the reader's rules one by one: the
token presence rule, the read-time classification, the window, and the breakdown order.

## Source map

- [ledger_failures.go](../../ledger_failures.go): the `--failures` mode, the table parser and the join.
- [internal/ledger/failures.go](../../internal/ledger/failures.go): the reader that groups the rows.
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
