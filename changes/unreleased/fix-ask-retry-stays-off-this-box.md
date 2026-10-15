### Fixed - offload_ask's cross-seat retry no longer comes back to this box

- **What happened.** A failed first attempt of `offload_ask` is retried on the fleet at `route=remote`, whose first
  placement never lands on this box. Inside that call the delegator has a retry of its own, and for a bare remote route
  it falls back `remote -> local`. So when the fleet node's answer failed the same acceptance, the only place left was
  this box's seat, the one the first attempt had just failed on: the goal ran a third time on this box's GPU, the note
  still read `retry on <node>`, and a caller who never named `route: "local"` got work back on the box the retry exists
  to move it away from.
- **What changed.** `RunOptions` gains `RemoteRetryOnly`, and only `askRetry` sets it (through a new last parameter of
  `contractOnFleetAt`; `agent_run`, the review lane and every other door pass `false` and keep the bare route's
  `remote -> local` retry). With it, `alternativeNode` takes the branch a reasoned remote pin takes: the retry goes to
  another fleet node or is not run, never to the local seat, and the retry-skipped note says the caller asked for it.
  It is a separate predicate (`retryOffLocal`) beside `remotePinned`, not a pin: no `pin_reason` is stamped, the ledger
  rows and results read as before, and `offload_status` still counts the ask's remote route as `unreasoned`. A synthetic
  pin reason would have claimed an operator decision nobody made. The ask's note now ends with the fleet's own skipped
  retry (`the fleet's own retry: retry skipped: ...`) when there is no other node. `agent_run`'s named-seat call is
  unchanged and keeps `PinOperator`. `docs/systems/mcp-server.md`, `docs/systems/fleet-node.md` and ADR 0078 no longer
  list the ask's retry among the callers that keep `remote -> local`.
- **The test.** `TestOffloadAskRetryNeverReturnsToThisBoxWhenTheFleetNodeAlsoFails`
  (`internal/mcpserver/ask_retry_stays_off_box_test.go`) drives the real delegator, with no fleet seam, at a scripted
  fleet node whose answer cites nothing, and a local seat that counts its runs. It asserts one run on the seat, one job
  on the node, the published first answer, `retried_on`, the skipped-retry note and the unreasoned tally. On the parent
  it fails with `ask_retry_stays_off_box_test.go:97: this box's seat ran 2 times, want 1: the ask's retry came back to
  the box the first attempt failed on`.
