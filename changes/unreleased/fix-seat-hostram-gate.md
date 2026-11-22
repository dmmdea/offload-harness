### Fixed - a loaded agent seat is no longer refused by its own cold-load host-RAM gate

- **What was wrong.** Both contract doors (`runAgentTask` and the MCP `agent_run` door) put a run to the cold-load
  host-RAM gate when the admission pre-flight reported the seat "not ready" with no load in flight. The pre-flight
  matched `/running` rows by the seat's bound name and resolved an alias to its canonical id only when another model
  was mid-swap. On a box that binds its seat by an alias, a seat that was loaded and ready with nothing else swapping
  therefore read as not ready. The gate then judged the seat's whole footprint against a commit charge that already
  held the seat, and the contract was deferred in milliseconds as `capacity` with
  `host RAM: ... the seat was not loaded`.
- **What it does now.** The pre-flight resolves the alias whenever the seat's row is not found under its bound name
  and `/running` lists anything, and it reports a third fact, `absent`: the last reading established that no row is
  the seat's under any of its names. The doors gate only an absent seat. A seat that is loaded, loading, or whose
  state could not be told (a roster or `/running` read that failed) is not gated. An empty `/running` still costs no
  roster read.
- **Why it matters.** On the reference box the local seat deferred 786 contracts with this reason in five days
  against 37 local runs. 79 of the 345 deferrals after the serving log's first entry fell inside a window that log
  shows the seat ready. The remaining deferrals are the gate judging a real cold load on a host with more than
  about 64 GiB committed, which this change does not alter.
- **Check.** One real run on the reference box, the same pinned-local contract twice within four seconds while
  `/running` listed the seat ready and 92.1 GiB was committed. The 0.234.0 binary deferred it
  (`host RAM: the host cannot take the seat's 56.0 GiB ... committed 92.1 of 127.7 GiB physical ... the seat was not
  loaded`); this build answered it in one step. One test,
  `TestALoadedSeatBoundByAliasIsNotReadAsAbsent`, drives the exported pre-flight with that shape and its control.
- **Where.** `internal/pipeline/agenttask.go` (`awaitSeatAdmissionLoad`), `internal/pipeline/seathostram.go`,
  `internal/mcpserver/mcpserver_agentrun.go`, `docs/systems/gpu-lease.md`.
