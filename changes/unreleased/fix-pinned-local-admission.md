### Fixed - a reasoned local pin no longer queues without bound on the local seat

- **What it does.** A subtask pinned with `route=local` and a `pin_reason` joins the local seat's run-cap line only while the line is
  shallower than `fleet_max_queue_depth` (default twice `fleet_max_concurrent_jobs`, the bound every fleet node applies to its own
  backlog). Past that depth it defers at once as `capacity`, the pin still holds (it is never re-placed), and the reason names the
  line's depth, the fleet nodes with a free worker right now, and that a call without the pin (route spread) would use them.
  `offload_research` now refuses `privacy` and `locality` (its pages are public and travel inline), and the `operator` reason's text in
  both schemas says the human operator typed the placement, which a subagent, workflow stage or script never has.
- **`offload_status` publishes `fleet.agent_slots`** `{total, local, fleet}`: the local run cap plus every reachable agent node's
  `max_concurrent_jobs`, the most agent jobs the cluster runs at once, so a fan-out is cut to the cluster instead of to the task.
- **Why.** On 2026-10-19 a workflow's agents each pinned `offload_research` to the local seat under a claimed `operator` reason: 36 runs
  registered on one seat, 184 subtasks deferred `node_busy`, the median latency was the 600 s wall, and four fleet nodes had free
  workers. ADR 0078 made a reasonless pin a hint; a claimed reason still bought an unbounded line.
- **Check.** One real `offload_research` call pinned `route:"local", pin_reason:"operator"` against a full local line, read back:
  see the pull request. Details: ADR 0078, amendment 2026-10-19.
