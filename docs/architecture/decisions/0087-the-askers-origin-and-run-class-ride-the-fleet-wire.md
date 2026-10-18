---
status: Proposed
date: "2026-10-13"
---

# 0087 — The asker's origin and run class ride the fleet wire

Release: unreleased (register D-101, completing ADR 0046 and the run class of register F48)

## Context

ADR 0046 made every ledger row on a box say which session asked, and the run class of register F48 made every row
say whether it was real work. Both stop at the box's edge. A job a delegator dispatches to a fleet node is run by
the node's own process, which writes its own row, and that row said neither: `origin_session` was empty (a service
inherits no Claude environment) and `run_class` was empty (the node's process is not a bake). ADR 0046's
Consequences recorded it as "a separate, additive change (a header, like the tenant)"; the changelog of the run class
recorded that "the class does not ride the fleet wire".

What this left open, measured against the delegator's own ledger:

- The delegator's ledger already answers "what share of session S's card tokens ran on fleet nodes": its rows carry
  `origin_session`, `cards_tokens`, and `fleet_job_id` (or `node`) for work that left the box. Only a reader was
  missing.
- The node's ledger could not say who a row was for beyond the asker's box name (`requester`, X-Offload-Asker). A
  bake dispatched to a node wrote unlabelled, production-looking rows there, which every production view of that
  node's ledger counted.
- A box that is both a session host and a fleet node writes the asker's row and the node's row for the same job
  into one ledger file. Once the node's row carries the session, a reader that sums by session without a door filter
  counts the job twice.

## Decision

1. **Two request headers carry the asker's session and run class**, like `X-Offload-Tenant` and `X-Offload-Asker`:
   `X-Offload-Origin` (the session label) and `X-Offload-Run-Class` (the class). A header, not an envelope field,
   because the dispatch envelope is decoded with `DisallowUnknownFields` and a new field would `400` on every node
   one release behind; a node that predates them ignores them, an asker that predates them sends neither. Each is
   sent **only when non-empty**, and the class is never `production`.
2. **One cleaning rule at both ends: the ledger's own.** `ledger.CleanOrigin` (printable ASCII, at most 96 bytes,
   interior spaces allowed) and `ledger.CleanClass` (a short `[a-z0-9_-]` token, lower-cased, `production` is no
   class) are exported. The sender sends only a value they accept, and the node, the queue holder and the claim loop
   keep only a value they accept. A label valid in the asker's own `Record` can therefore never be dropped on the
   wire, and one the ledger would refuse never reaches a row. A value over the cap is **refused whole, not cut**
   (unlike the tenant, which truncates): a cut session id would silently attribute the row to a different session.
3. **One sender.** `pairworkloads.Emitter.SetWireHeaders` is the chokepoint every work-creating request already
   passes (delegation dispatch, the queue submit, and the accelerator, compose, vision, text, media and stt lanes).
   It reads the process's own session and class from the environment by the ledger's resolvers (no process-table
   walk, which is slow on Windows and would run on every request). A delegation pinned for `measurement` sends the
   class `measurement` when the process has none of its own, so the node's row for it is a measurement too; the
   process class wins when there is one, as it does in the ledger's own `settleRunClass`.
4. **Both admission doors parse them.** A pushed job: `admit` (every lane joins it) reads the headers and stamps
   `Request.OriginSession` and `Request.RunClass` on the request the runner builds. A pulled job: the holder's
   submit handler stores them on the queued job (`fleetqueue.Job.Origin`, `RunClass`), the claimant cleans them again
   (they came over the network) and stamps the same two fields. The pipeline carries them as `Meta.AskerSession` and
   `Meta.AskerClass` (named apart from the node's own `ProcessOrigin`) into the row. All four fields are `json:"-"`:
   a request body must not be able to name its own session or class, and `Meta` is marshalled whole into a node's
   poll results, which must not echo the asker's session id back.
5. **The class is believed only from a caller the node can authenticate.** A labelled row is left out of every
   production view, so a class from a caller the node cannot identify would let any peer that reaches the port hide
   the node's rows. A token-gated lane has already passed its gate (a bearer when a token is configured, else a
   loopback listener); a lane that is open on purpose (the media task types and the legacy path-taking stt lane,
   which deployed tokenless clients use) is trusted only when a token is configured and the request presented it.
   Otherwise the class is dropped, and so is a session label that carries a class of its own (`bake:run-7`, the
   `go-test` label), because `ledger.ClassOf` reads a class off such a label when the row names none. A plain
   session label from that caller is kept: it is attribution, not a hiding place. The queue submit route sits
   behind the holder's own guard, so a class that reaches it is already authenticated.
6. **`stampOrigin` keeps a session its caller named and still fills the pids.** The row then reads "process P
   recorded this while serving session S", the meaning ADR 0046 gave the columns. A caller that names a pid keeps
   both, as before. A node row whose asker sent no session (an older asker) falls back to the node process's own
   origin, as before.
7. **A reader splits a session's card tokens by where they ran.** `ledger.SessionShares` and `SessionSharesFile`
   (and `local-offload ledger --by-session [--session ID]`, a `sessions` array added to the existing JSON; the
   default output is byte-identical) report, per session: `remote_tokens` (rows that carry a fleet job id or a
   node), `local_tokens`, `remote_share`, and `served_rows` / `served_tokens` for rows this box ran as a fleet node
   (door `fleet`). Served rows are **never** added to the session's own totals, so a ledger shared by a delegator
   and a node does not count a job twice. Production rows only unless `--include-test-traffic`; dispatch markers
   and inner rows are not jobs; rows with no session are one entry apart and never merged into a named session.

## Consequences

- **The share is the card-side placement split, not the share gate's figure.** `remote_share` is remote tokens over
  remote plus local card tokens. The gate's figure is cards over cards plus cloud and needs the session's transcript;
  it stays in the hook. The two answer different questions and are named apart on purpose.
- **The remote part is a floor.** A cascade call routed to a node over the chat lane writes no node row, and the
  asker's row names neither the node nor a fleet job, so it reads as local. A delegation sent with `route: "queue"`
  builds no runner and writes no row on the asker's ledger at all (the claiming node's ledger holds it, as a served
  row), so on the asker's box it is in neither number. A delegation pushed to a node is not a floor: its finished row
  carries the fleet job id whenever the node acked the dispatch, because the id is set from the ack and the started
  marker is written after it, so a marker that fails to write costs the marker and not the id. The reader's help text
  says so and tests pin both shapes (`TestSessionSharesReadsAChatLaneRowAsLocalTheDocumentedFloor`, and
  `TestQueueRouteWritesNoRowOnTheAskersLedger` from the writer's side).
- **The reader reads one box's ledger.** The node-side stamp adds a node-local `served_*` view; a per-session view
  across the whole fleet is a reader of several ledgers, which this is not.
- **A shared ledger is safe for this reader and not for others.** The reader separates served rows by door. The
  share gate hook (`harness-share-gate.js`, in the operator's agent configuration, not this repository) adds to its
  own session figure every row that names the session, with no door filter: on a box that is both session host and
  node it would count a dispatched job twice, and its `session`-mode test (every row attributed) now flips toward
  session mode once node rows carry sessions. The gate is paused, so nothing is exposed today; the fix is one line in
  that hook, skipping rows whose door is `fleet`.
- **Door is only as good as `dispatchDoor`.** The node keeps a door a payload supplies; today none does, so every
  node row reads `fleet`. A future payload that carries a door would turn a served row into an asker row for this
  reader.
- **Known gaps left on purpose.** Accelerator rows served by a node are still unattributed (`RecordAccel` writes from
  no `Meta`); composite sub-request inner rows carry no wire origin, which is fine because every counter skips inner
  rows; the `cli:fleet-measure` door sends no class from a box with no process class, because the media and vision
  lanes know the config, not the door.
- **The session id now leaves the box.** It is an opaque identifier Claude Code assigns, sent to every fleet node the
  asker dispatches to, beside the asker's name. No opt-out is built. `LOCAL_OFFLOAD_ORIGIN` names a label of the
  caller's choosing in its place.
- **Trust is the same as `X-Offload-Asker`** for the session (any peer holding the route can name one), and stronger
  for the class (decision 5).
- A node launched from a shell that carries `CLAUDE_CODE_SESSION_ID` still attributes a row whose asker sent no
  session to that launching session; that is unchanged.

## Alternatives considered

- **An envelope field.** Rejected: the dispatch envelope is a strict decoder and a new field would `400` on every
  node one release behind.
- **Truncating an over-long session like the tenant.** Rejected: a truncated id is another session's id.
- **Honouring the class from any caller.** Rejected: see decision 5; the media lanes are open on purpose.
- **Closing the register row as accepted** because the delegator's own ledger already answers the card-side
  question. The reader is built (it works from the delegator's ledger alone); the wire half is what gives the node's
  ledger the same attribution and closes the node-row gap of the run class.
- **Counting served rows in the asker's totals** and deduplicating by fleet job id. Rejected: the join needs both
  rows present in one file, and a delegator and its node are usually two files.

## Related code

- `internal/core/agentwire.go`: `OriginHeader`, `RunClassHeader`
- `internal/core/types.go`: `Request.OriginSession`, `Request.RunClass`, `Meta.AskerSession`, `Meta.AskerClass`
- `internal/ledger/origin.go`, `runclass.go`: `CleanOrigin`, `CleanClass`, `SessionFromEnv`, `ClassOfSession`, `stampOrigin`
- `internal/ledger/sessionshare.go`: `SessionShares`, `SessionSharesFile`
- `internal/pairworkloads/wire.go`: `SetWireHeaders`
- `internal/delegate/run.go`: the measurement class on a pinned dispatch
- `internal/fleetnode/nodecard.go`, `server.go`, `claimloop.go`: `originOf`, `trustsWire`, both doors
- `internal/fleetqueue/http.go`, `queue.go`: the queued job's `Origin` and `RunClass`
- `internal/pipeline/pipeline.go`: `Meta.AskerSession` into the row
- `main.go`: `local-offload ledger --by-session`

## Related docs

- [0046 — Ledger rows carry their origin and their job](0046-ledger-rows-carry-their-origin-and-job.md) (this completes its Consequences)
- [pair-workloads](../../systems/pair-workloads.md) (the attribution headers)
- [fleet-node](../../systems/fleet-node.md), [FLEET-NODE](../../FLEET-NODE.md#the-delegation-ledger-row)
- [OPERATOR-GUIDE](../../OPERATOR-GUIDE.md) (reading the ledger)

## Amendment (2026-10-17): every lane needs the fleet bearer

Where this record calls the media task types and the legacy path-taking stt lane open on purpose, for the tokenless clients then deployed, it describes the node when the decision was taken. Since the amendment of 2026-10-15 to
[ADR 0023](0023-agent-lane-tailnet-auth-and-locality.md), every lane needs the fleet bearer on a node that has a token
(dispatch of any task type, the poll and withdraw of any job, and the read of any artifact), and a node with none answers only on
loopback. Every lane has passed its gate by the time the class is read, and the rule that a class from a caller the node cannot authenticate is dropped is unchanged.
