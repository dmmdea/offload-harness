### Added - The asker's session and run class ride the fleet wire, and `ledger --by-session` splits a session's tokens by where they ran

- **The gap.** Every ledger row says which session asked (`origin_session`, ADR 0046) and whether the run was real work (`run_class`, register
  F48), and both stopped at the box's edge: a job a delegator dispatches to a fleet node is run by the node's own process, whose row named
  neither. A bake dispatched to a node wrote production-looking rows there, the node's ledger could not say whose work it ran beyond the asker's
  box name (`requester`), and nothing could read a session's tokens by where they ran. The delegator's own ledger already holds that split (its
  rows carry `origin_session`, `cards_tokens` and `fleet_job_id` or `node`); only the reader and the node-side stamp were missing.
- **The wire.** Two request headers, `X-Offload-Origin` (the session label) and `X-Offload-Run-Class` (the class), sent only when non-empty, never
  `production`, on every request that creates work on a node: `pairworkloads.Emitter.SetWireHeaders`, the one chokepoint the delegation dispatch,
  the queue submit and the accelerator, compose, vision, text, media and stt lanes already pass. A header and not an envelope field, because the
  dispatch envelope is a strict decoder and a new field would `400` on every node one release behind; a node that predates them ignores them and
  an asker that predates them sends neither, so a mixed fleet behaves as before. A delegation pinned for `measurement` sends the class
  `measurement` when its process has none of its own (the process class wins, as in the ledger's own `settleRunClass`), so the node's row for it is
  a measurement too.
- **One cleaning rule at both ends.** `ledger.CleanOrigin` (printable ASCII, at most 96 bytes, interior spaces allowed) and `ledger.CleanClass`
  (a short `[a-z0-9_-]` token, lower-cased, `production` is no class) are exported. The sender sends only a value they accept; the node, the queue
  holder and the claim loop keep only a value they accept. A label valid in the asker's own `Record` is therefore never dropped on the wire, and one
  the ledger would refuse never reaches a row. A value over the cap is refused whole, not cut like the dispatch tenant is: a cut session id would
  silently attribute the row to a different session.
- **Both admission doors stamp them.** A pushed job: `admit`, which every lane joins, reads the headers and puts `Request.OriginSession` and
  `RunClass` on the request the runner builds. A pulled job: the holder's submit handler stores them on the queued job (`fleetqueue.Job.Origin`,
  `RunClass`, omitted when empty, so an old holder and a new one read each other's records), and the claim loop cleans them again, since they came
  over the network, and stamps the same two fields. The pipeline carries them as `Meta.AskerSession` and `AskerClass` (named apart from the node's
  own process origin) into the row. All four fields are `json:"-"`: a request body cannot name its own session or class, and `Meta`, which a node
  marshals whole into its poll results, does not echo the asker's session id back.
- **The class is believed only from a caller the node can authenticate.** A labelled row is left out of every production view, so a class from a
  caller the node cannot identify would let any peer that reaches the port hide the node's rows. A token-gated lane has already passed its gate
  (a bearer when a token is configured, else a loopback listener); a lane that is open on purpose (the media task types and the legacy
  path-taking stt lane, which deployed tokenless clients use) is trusted only when a token is configured and the request presented it. For any
  other caller the class is dropped, and so is a session label that carries a class of its own (`bake:run-7`, the `go-test` label), because
  `ledger.ClassOf` reads a class off such a label when the row names none; a plain session label is kept. The queue submit route sits behind the
  holder's own guard, so a class that reaches it is already authenticated.
- **`stampOrigin` keeps a named session and fills the pids.** A caller that names only a session (the node writing a job's row for the asker's
  session) used to get no pids; it now gets the pids of the process that recorded the row, which is what the columns mean. A caller that names a
  pid keeps both, as before. A row whose asker sent no session falls back to the node process's own origin, as before.
- **The reader.** `ledger.SessionShares` and `SessionSharesFile`, and `local-offload ledger --by-session` (optionally `--session <id>`, which implies
  it): a `sessions` array added to the existing JSON (the default output is byte-identical), and a line saying what the number is. Per session:
  `remote_tokens` (rows that carry a fleet job id or a node), `local_tokens`, `cards_tokens`, `remote_share`, and `served_rows` / `served_tokens` for
  the rows this box ran as a fleet node (door `fleet`). Served rows are never added to the session's own totals, so a ledger shared by a delegator
  and a node does not count a job twice. Production rows only unless `--include-test-traffic`, dispatch markers and inner rows are not jobs, and
  rows with no session are one entry apart, never merged into a named one.
- **What the share is.** It is the card-side placement split, not the share gate's cards/(cards+cloud), which needs the session's transcript and stays
  in the hook; the fields are named apart (`remote_*`, not `fleet_*`) so the two are not confused. It is a floor on the remote part: a cascade call
  routed to a node over the chat lane writes no node row, and a delegation whose started marker failed to write has no fleet job id, so both read as
  local (a test pins it). It reads one box's ledger; a view across the fleet is a reader of several.
- **Known limits, on purpose.** Accelerator rows served by a node stay unattributed (`RecordAccel` writes from no `Meta`); the `cli:fleet-measure`
  door sends no class from a box with no process class, because the media and vision lanes know the config, not the door; the session id now
  leaves the box, to every node the asker dispatches to, beside the asker's name, with no opt-out; and the share gate hook, which sums by session
  with no door filter, would count a job twice on a box that is both session host and node (it is paused; the fix is one line there, outside this
  repository).
- **Docs.** ADR 0085 (Proposed; linked from the index and from ADR 0046's Consequences, which had named this change), the attribution header table and
  the node's recording rules in `docs/systems/pair-workloads.md` and `docs/systems/fleet-node.md`, the `run_class` row of the delegation ledger table in
  `docs/FLEET-NODE.md`, the ledger paragraph of `docs/systems/offload-pipeline.md`, and `docs/OPERATOR-GUIDE.md` (the `origin_session` column, a bake
  dispatched to a node, and `ledger --by-session`).
- **Red first.** A behaviour-free seam first (declarations, stubs, and the two flags registered and ignored, because the ledger flag set exits the
  process on an unknown flag), then tests that failed on their assertions against it, for example: `X-Offload-Origin = "", want sess-abc`;
  `originOf = ("", ""), want ("sess-abc", "bake")`; `queued job = origin "", class "", want sess-abc and bake`; `session-only row = session "sess-abc"
  pid 0 ppid 0, want ... this process`; `written row = origin_session "" run_class "", want the asker's sess-abc and bake`; `no entry for session
  "sess-abc" in []`; `--by-session printed no sessions key`. Then the behaviour.
- **Mutation proofs.** Every rule has a single-line mutant that `scripts/mutation-check.sh` scored RED (the mutant compiled, the named test failed,
  the file was restored byte for byte), 43 in all. Ledger: `stampOrigin` restored to return early on a named session; the reader reading a fleet job id
  or node as local, counting a served row as the asker's own, ignoring the run class filter, counting markers and inner rows as jobs, dividing by
  zero for an idle session, reading a row without `cards_tokens` as free, ignoring `--since`, reversing the order, merging unattributed rows into a
  named session; `CleanOrigin` without its cap; `CleanClass` accepting `production`; `ClassOfSession` naming no class. Sender: the origin header
  not sent, the class header not sent, either value not cleaned, the environment not read. Queue: the stored job dropping the origin or the class,
  the submit handler storing either uncleaned, the origin key written when empty. Core: `Request` or `Meta` carrying the session on the wire. Node
  (run on the remote gate runner): the stamp deleted on the push door and on the claim loop, the request always trusted, the bearer not checked, a
  session cut to length tenant-style, a class-carrying label kept for an untrusted caller, the class not cleaned, the queued session or class not
  cleaned again. Pipeline: the copy into `Meta` deleted, `entryFrom` dropping the session or the class. Delegate: a measurement pin sending no class,
  and overriding a process class. `ledger`: `--session` not narrowing, not implying `--by-session`, the array emitted unconditionally, the key
  losing `omitempty`.
