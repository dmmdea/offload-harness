### Fixed - a call a fleet node refuses for capacity keeps its place in the fleet's line instead of leaving it

- **What happened.** On 2026-11-25 nine `transcribe` calls on route `auto` ran while a GPU lease held this box's cards. The one
  fleet node took four and refused four with `503 queue full (4 jobs accepted+running, limit 4): retry later ... (~183 s until
  a worker frees)`. The four left the fleet's line at the first refusal and ran here, where the lease was, and the ninth ended
  in `gpu busy: gpu-lease timeout after 1m29.82s`. Nothing asked the node again. The vision, text and forecast lanes had the same
  shape.
- **What changed.** The four single-shot lanes now share one placement loop (`internal/lanehold`). A dispatch refused with 503 or
  429, or a roster whose only misses are temporary (card leased, queue full, cooling), keeps the call's place: the roster is read
  again every 3 s, a node that refused is left alone for 10 s (stretched once by up to 30 % so a burst of callers does not return
  together) and is then asked as soon as its fresh health shows room, and a node whose health reads full is no longer uploaded to
  only to be refused. On route `auto` with this box's card leased the call runs on whichever frees first, a node or this box's
  card, and keeps a seat waiter so a chained media acquire does not jump it. The bound is the lane's own budget (transcribe:
  `stt_request_timeout_sec` less the run budget), at most half of a caller's remaining deadline; `agent_placement_wait_sec: -1`
  restores the single try. The run half now gets its own full budget once a node accepts. A node publishes
  `progress.phase` `queued` while an stt job waits at its stt gate, and the asker does not charge that time to its run budget.
  Placements say what happened (`remote: local gpu busy, waited 41s in line`, `local: card freed after 41s in line, ...`); the
  ledger still files a roster that ended full or cooling as `queue_full`. No config key, no other wire change.
- **Check.** One run of `Run` on route `auto` with the local card leased and a node that answers the upload 503 with a
  `Retry-After` and then accepts: the call came back transcribed by the node after two uploads, placed `remote: local gpu busy,
  waited 1s in line`, with `transcribe: waiting in line: node-c refused it; this box's card is leased` and `transcribe: placed on
  node-c after 1 s in line` on stderr. Decision: [ADR 0105](docs/architecture/decisions/0105-a-refused-single-shot-call-holds-its-place-in-both-lines.md).
