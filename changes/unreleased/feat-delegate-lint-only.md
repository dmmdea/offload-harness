### Added - agent_delegate lint_only: the acceptance lint and the placement a contract would get, without running it

- **What it does.** `agent_delegate` takes an optional `lint_only: true`. The call runs intake and the acceptance lint
  as before, then returns before any dispatch, lease, queue or seat warm-up, and runs no model. Per subtask it answers
  `acceptance_lint` and a `placement` (`route`, `where`, `node`, `reason`) read from the same readings the joint deal
  takes before it deals (the lease file, the local seat's in-flight count, the run line and one health read per fleet
  node, bounded at 4 s), through the new `delegate.PlanPlacement`. Routes `spread` and `queue` answer `not_computed`.
  Documented in `docs/OPERATOR-GUIDE.md` and `docs/systems/mcp-server.md`; the tool description gains one sentence,
  pinned in `toolsprofile_keep.go`.
- **Why.** A caller learned that its acceptance was PARROT-PASSABLE or UNGROUNDED only by paying for the full run
  (register D-109).
- **Check.** The built binary as an MCP stdio server answered an `agent_delegate` call with `lint_only: true` and one
  trivial subtask in 68 ms on 2026-10-19: both lint findings, `placement.where: local` ("local idle"), no job id, no
  seat output.

### Changed - composite-tier citations in internal/delegate and internal/mcpserver point at ADR 0052

- **What it does.** 28 comments and strings that said "ADR 0039" for the composite tier now say "ADR 0052", including
  the `agent_run` and `offload_status` description sentences (and their pins in `toolsprofile_dropped_test.go`).
- **Why.** The composite tier was drafted as 0039 and recorded as 0052; 0039 is the held-card lease queue. The earlier
  sweep left these two packages to the tracks editing them. Mentions that mean the lease queue (ADR 0041, `gpuactivity`,
  `mediaremote`, `pipeline/mediamulticard.go`, the lease docs) are untouched. Comments and strings only.
- **Check.** `grep "ADR 0039" internal/delegate internal/mcpserver` finds nothing; the description-sentence gate tests pass.
