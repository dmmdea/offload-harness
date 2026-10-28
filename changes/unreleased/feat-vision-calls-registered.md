### Fixed - a vision call outside a lease is visible: `gpu status`, `offload_status` and node health name it

- **The gap.** A single-shot vision call (`vqa`, `ocr`, `assess_image`, a `video_describe` window) holds no lease and is no
  job of a node's store, so nothing recorded it. A script that ran one `local-offload vqa` per image kept a node's vision
  seat busy for hours, one call every few seconds, while `gpu status` read `free`, the node's health read `jobs_running: 0`
  and every delegator's `offload_status` showed the node `in_flight: 0, verdict: cold`. Finding the caller took a walk of
  the process table.
- **What it does.** `runVisionGen`, where every vision door converges (CLI, MCP, `/fleet/vision`), registers the call in
  the activity registry while it is on its seat (`gpuactivity.KindVision`: the seat, the door and the parent pid, the
  question or the file name). It registers in admission while a cold seat loads and is running from the request on, like
  every run (register C-60). A cache hit and a lease defer never reach the seat and are not registered. `gpu status` and
  `offload_status.gpu_lease` then read `working` and list the call. Node health gains `vision_calls_running`: the vision
  calls other processes on the box have on a seat (the node's own `/fleet/vision` jobs are already in `jobs_running`). The
  delegator's `offload_status` adds it to the node row's `in_flight` and publishes it beside it.
- **What it leaves alone.** A vision record carries no `devices`, so the run-cap line of a seat pinned to one card does not
  count it and an agent contract never waits behind it; a card-scoped drain still sees it through the seat's placement
  (`LeaseScope.TouchesRun`). No placement rule reads `vision_calls_running`, and the CLI's default `--route local` is
  unchanged.
- **Check.** One CLI `vqa` with the built binary on a real vision seat, beside a loopback `fleet-serve` of the same build
  reading the same registry, polled every 0.5 s: `vision_calls_running` 0 → 1 → 0 and `gpu status` `free` → `working`
  (seat, pid, `origin: cli:vqa, parent pid N`, the question) → `free`. Docs: `docs/systems/fleet-node.md`,
  `docs/systems/gpu-lease.md`.
