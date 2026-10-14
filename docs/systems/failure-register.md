# Failure register

## Purpose

Say which failures the harness has, how many of each, who owns each kind of failure, and what is known about why.
`local-offload ledger --failures` reads the ledger and prints one JSON report: the failed jobs grouped into failure
classes, with a count, a kind, the doors, seats and models they came through, and the card work they spent. Each class
is then joined by name to the table on this page.

The split is the point. The counts come from the ledger. The **root cause** and the **structural fix** of a class are
a person's: they are written in the table below, and no code derives, guesses or fills them in. A class with no row
here reads `undocumented`, and a class whose row holds no root cause and no fix a person wrote reads as not
investigated (`uninvestigated_classes`), so a failure the harness keeps hitting without anyone having looked into it
is visible as a gap, not hidden. The table ships with those cells empty: nothing on this page has been investigated yet.

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
   not decode as a ledger row is in no count and is counted in `unparsed_lines`; a ledger file that is not there reads
   `ledger_found: false` with every count at 0, and a note on stderr.
2. A job row is a **failure** when it deferred, or carries a failing reason code, or failed its acceptance check. A
   cache hit and a dispatch marker never are.
3. Its **class** is its reason code, the closed set in `internal/ledger/reasoncode.go`. The set already holds the seven
   wire defer classes (the table at the end of this page). A row written before the code was stamped, or with a code
   outside the set, is classed with the same classifier the ledger itself uses (`DeferCode`) and counted in
   `class_derived_rows`.
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
count as the failures they were then recorded as. (The savings summary's job counters still count a marked row.)

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
a share of `failure_rows` and counts nothing it cannot name: an `unclassified` failure could add to it.

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
| `class_derived_rows` | failed rows classed at read time because they carried no usable code |
| `excluded_non_production` | test, bake and measurement jobs left out, per class (absent when none) |
| `undocumented_classes` | the classes present in the ledger that have no row in the table |
| `uninvestigated_classes` | the classes present in the ledger with no root cause and no structural fix written by a person (a class with no row is among them) |
| `classes[].class`, `kind`, `count`, `share` | the class, its kind, its rows, and its part of `failure_rows` |
| `classes[].example_reason` | the most frequent short reason among its rows (the smaller text on a tie) |
| `classes[].doors`, `seats`, `models` | the same rows counted per door, per seat and per model (on a delegation row the "model" is the seat alias, see below) |
| `classes[].groups` | the rows counted per (door, seat, model) together |
| `classes[].cards_tokens`, `rows_with_tokens` | the card work the class spent, and how many rows measured it |
| `classes[].documented`, `investigated`, `meaning`, `root_cause`, `structural_fix` | the joined table row, and whether a person wrote a root cause or a fix in it |

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
- **No row below has been investigated.** `meaning` states what the class is, from the constants' own definitions. The
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
  report, JSON on stdout and nothing else. A note about the left-out traffic, a missing ledger or lines it could not
  read goes to stderr. `--failures=false` is the savings summary, as for any boolean flag.

## Invariants and assumptions

- The register reads the ledger and the table (the page embedded in the binary, or the file `--table` names) and
  nothing else; it never calls a model and never writes a file.
- Counts, shares, ordering and the example reason are deterministic functions of the ledger rows.
- A class with no rows in the window is not listed.
- A refused attempt is never a job and never a failure; it is counted in `refused_attempts`.
- A line the register cannot read is counted in `unparsed_lines`, never dropped silently; a missing ledger is
  `ledger_found: false`, never a quiet zero.
- An unmeasured token total is `null`, never `0`.
- `root_cause` and `structural_fix` come only from the table. A class with no row says `undocumented`.

## Testing notes

`ledger_failures_test.go` runs the verb on a synthetic ledger and table under `testdata/failure-register/` and compares
the output with a committed golden file. `internal/ledger/failures_test.go` pins the reader's rules one by one: the
token presence rule, the read-time classification, the window, the breakdown order, the refusal rule, the kinds and
the unreadable lines.

## Source map

- [ledger_failures.go](../../ledger_failures.go): the `--failures` mode, the table parser and the join.
- [internal/ledger/failures.go](../../internal/ledger/failures.go): the reader that groups the rows.
- [internal/ledger/failurekinds.go](../../internal/ledger/failurekinds.go): the kind of every failure class.
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
