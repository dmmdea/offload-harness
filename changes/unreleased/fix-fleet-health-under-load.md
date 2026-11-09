### Fixed - a busy fleet node answers its health probe in well under a second

- **What happened.** Askers read a node's `/fleet/health` before placing image work. While a node was rendering and
  loading weights, those probes timed out and the asker concluded that no node served image-gen. Two parts of the
  handler could hold a probe for a long time: a request that found the node's llama-swap residency answer too old
  waited up to 1.5 s for the refresh, and every request that arrived while a slow refresh was still running waited the
  full 1.5 s again (measured: five probes in a row at 1.5 s against an unreachable engine). The disk reads behind the
  media routes and the image and media recipe rows were rebuilt inline once a minute, so a busy disk stalled that
  request as well.
- **What changed.** The residency wait is now 400 ms and only the first request of a refresh cycle pays it; the rest
  serve the previous answer at once. The three disk reads run one at a time on their own goroutine and a request waits
  for a rebuild at most 250 ms, otherwise it serves the last known value. Nothing is served silently: the payload gains
  an additive `stale_reads` list of `{name, age_ms}` (omitted when every part is fresh) naming each part served from an
  older reading. No field is dropped or renamed. A part that has never been read is still waited for in full.
- **Measured** on a scratch node with image families and an unreachable engine address: probes against the
  unreachable engine went from 1.5 s per request for the first several seconds (and again each minute) to one 0.42 s
  request at start and about 3 ms after, plus one request of about 0.5 s when the minute-old reads are rebuilt; a node
  with reachable engines answers in about 3 to 8 ms warm.
- **Where it shows.** `docs/systems/fleet-node.md` (the `stale_reads` row and the residency paragraph).
