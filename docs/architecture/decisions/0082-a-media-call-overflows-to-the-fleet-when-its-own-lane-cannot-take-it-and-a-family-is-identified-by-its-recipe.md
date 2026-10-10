---
status: Proposed
date: "2026-10-10"
---

# ADR 0082 — A media call overflows to the fleet when its own lane cannot take it, and a family is identified by its recipe

## Context

A heavy image call on a box that has the lane waits for it. With the host-RAM guard of 0.178.0 that wait is longer on
purpose: a lane whose declared need would take committed memory past physical RAM less the headroom is queued, because RAM is
overflow only and the box must never page (the guard compares a declaration, which is an estimate: an admitted lane can still
read over that line, docs/systems/gpu-lease.md, "Known limits"). Meanwhile other nodes of the fleet stood idle with the same model on disk. The operator's
standing order for media (2026-10-10) is that the harness route media work to any node of the cluster even when every
machine is busy, that it queue and never refuse, and that it not retry in a loop. ADR 0077 already lets a call run on a node
(`route` `auto` or `remote`), but `auto` meant "here when this machine has the lane, a node when it has none", so a lane that
exists and is busy stayed the only place the call could wait.

Three facts made the obvious change unsafe as stated.

- **A name does not identify a render.** Two nodes bind `qwen-image-2.1` and one holds the bf16 DiT, the other an int8 build;
  a third binds an NVFP4 file under the same label. A call that overflows must render what its own lane would have, so
  "family X exists on that node" proves nothing. The live blocks also differ in cosmetic ways that do not change the pixels:
  one node writes the builder's default scheduler out, another leaves it unset. A comparison over the raw config keys would
  refuse that match for no reason.
- **The local lane has to be asked without being used.** The admission in `acquireCards` interleaves its reads and its grant
  over many lines that the host-RAM guard and the card-table retry (F24) both edit. An extraction would be a third hand in that
  function; a no-wait try of the real run opens and quiet-closes a PAIR card for every refusal.
- **The nodes this is for cannot pull.** The editor node accepts inbound connections only and holds no outbound fleet token;
  the Rockchip board has no outbound access. The consolidated pull queue of ADR 0030 would need a holder, a second mechanism
  for exactly those nodes, a lease carried through the context into the hottest function in the pipeline, and a rebuilt claim
  loop (it advertises every task type including text, claims in a tight loop and files a capacity defer as a terminal failure).

## Decision

1. **Placement is the delegator's, on the ADR 0077 lane; the pull queue stays dark for media.** An `auto` image call on a
   machine that has the lane and a fleet configured asks its pipeline whether the lane is free. If it is not, the call is sent
   to a node that renders the same recipe and is idle. Nothing is added to a node beyond what it publishes and one check it
   makes. This is one first-in-first-out line per delegating machine (the machine's own lease line), not one global order; a
   holder node's queue is the named upgrade, not built.
2. **The question is a read-only relaxation (`core.LaneProber`, `Pipeline.MediaLaneFree`).** It is built from the helpers the
   admission reads state with (`planMedia`, `gpualloc.Claims` and `QueuedClaims`, the in-process slots, the grant's own
   host-RAM function, and the allocator itself for a call that names no card) and edits nothing inside `acquireCards`. It says
   "busy" only when something that makes the real wait-0 grant refuse is present, so it never calls busy a lane the grant would
   have served; it may call free a lane the grant then refuses, and the call then runs locally as before, after the one bounded card-table read the question costs. The lease
   queue orders by arrival only on the whole-node plan, so there a call that resumes a place counts only the callers that
   arrived before that place; on the pinned and allocated plans every other caller in line counts
   (`TestMediaLaneFreeOrdersByArrivalOnlyOnTheWholeNodePlan`; the S1 trigger never asks about a call that carries a token, the
   ticket scan of the next step does). Every doubt
   (a lease this process inherited, a card table that does not read in 4 s, a request it does not model) reads as free. It
   creates no lease, place in line, epoch, waiter record, ledger row, PAIR card or ComfyUI instance, and it removes nothing: the
   readers of the line it shares with the admission prune a record they find dead (an expired place, a waiter that stopped
   polling) as housekeeping, and the probe reads through `gpulease.Manager.ReadOnly`, a view that skips such a record and leaves
   it on disk. A differential test drives the real wait-0 admission over a table of lane states and holds the prober to the one
   direction that matters, and `TestMediaLaneFreeWritesNothing` snapshots the lease root over each of them, those records included.
3. **A family is its recipe (`mediacap.ImageRecipe`).** The recipe is the weight files the graph loads (checkpoint, text
   encoder, VAE, LoRA), each with its name and byte size on the node's disk, and the sampling it renders with, digested (sha256
   of canonical JSON) over RESOLVED values: a key the binding leaves unset takes the builder's own default, from a table that a
   test reads out of `render/wf-qwen-image-21.mjs` and `render/comfy-render.mjs`, so it cannot drift from the code that renders
   (Qwen-Image-2.1 only in this release; any other graph leaves unset keys unset, which is stricter, never looser). The license
   the result is tagged with is part of it. Node-local keys (`comfy_*`, timeouts, scripts, `reserve_vram`, the pool keys) are not;
   a test fails when a key joins the image overlay's clear list unclassified. No recipe exists for an sd.cpp binding, so a
   ComfyUI recipe never matches one. The match is strict, also requires the same `harness_version` (the graph builders ship
   with the release) and a recipe with no missing file, and never substitutes: a caller who accepts another build names that
   family himself, and the answer tells him which of his own families would match. **The identity is name plus size, not
   content.** It tells a bf16 build from an int8 one and catches a truncated copy; it cannot tell two same-named files of one
   size whose bytes differ (a full-length corrupted copy, a same-named re-release with the same tensor layout, a file updated in
   place on one node), and the node's 412 re-check recomputes the same size-based digest, so it does not catch them either. A
   sha256 per weight file, cached by path, size and mtime and published in `image_recipes`, is the upgrade and is not built.
4. **Nodes publish and check recipes.** `/fleet/health` gains `image_recipes[]` (one row per ComfyUI binding that names a
   checkpoint: name, default, digest, files with sizes, resolved sampling, license, and which sampling keys the binding set) and
   `refine_honoured`, from a memo of the route cache's 60 s so health stays cheap. An image-gen dispatch may carry
   `recipe_digest`; the node recomputes the digest of the family the payload names from the files on disk now and refuses a
   mismatch with `412` and no job (not `409`, which already means "this job failed here before", nor `503`, which means busy).
   `refine` is decoded as the MCP handler does, so `refine=false` reaches the pipeline; a delegator that sends it requires
   `refine_honoured`, and a node that predates it is a named miss instead of one that refines anyway.
5. **A node is a candidate when** it serves `image-gen`, holds a family whose recipe digests alike, runs this release, carries
   `refine` when the call sent `refine=false`, has the family's route CONFIGURED when it reports its routes, accepts a
   per-request `steps` when the graph takes steps and cfg together (the node must have SET cfg), and holds **no lease of any
   class** (the delegator cannot know which card a node's lease sits on or which card the job would take, so it is
   conservative; the node's own grant, including its host-RAM guard, is the authority), and is not this machine's own node (its
   `node_id` equals `fleet_node_id`, else the OS hostname: such an entry would match perfectly and send the call back into the
   lane that is not free; it is named in `cluster[]`). Candidates rank by the shorter queue, then config order. The family is sent under the node's own name for the recipe, never the caller's, and never empty: a call
   that names no family is this machine's default binding, and must not land on a node's different default.
6. **Refusals move on; an accepted job is final.** At most 3 nodes are tried. A refusal at the door (`503`, `429`, `412`, any
   other status, or a dial that never connected) means nothing ran: the call goes on, under a fresh job id. A node that accepted
   the job and answers `gpu_busy` or `gpu_queued` (another job holds its card, nothing ran) or `gpu_lease_unavailable` (it could
   not take its lease: a lease location it cannot use, a host-RAM need no state of it admits; nothing ran) is passed over the
   same way. Any other answer after acceptance is the call's result: a render that failed is never re-placed, a POST that was
   sent and got no answer is final too (the node may hold the job: `TestAnAmbiguousPostIsFinal`), and a job a node holds is never
   also run here (a media job cannot be withdrawn, ADR 0064). A node that does not answer is left alone for 5, 15, 60 and then
   300 seconds by consecutive failure, a `Retry-After` pauses it for that long (jittered once), so a powered-off box costs one
   probe per step. A node that took a call and passed it back (a bounce) is left alone for 60 s the first time and 300 s for each
   consecutive bounce after, because a node that reads idle in its health but whose own grant refuses the job (a card the display
   rule keeps closed, a quarantined card, a host short of RAM, a card another process holds that no lease shows) refuses every
   call alike, and each call sent to it would park for the node's whole `gpu_wait_ms` before the bounce came back; a call the node
   serves forgets the count, a health read does not. The cluster row says which pause it is (`bounced`, `refused`,
   `unreachable`) and for how long the call was not offered to the node. The step and attempt numbers are chosen, not measured. The
   memory is the placer's package variable, so it is per process: the MCP server keeps it across the calls of its session, and a
   one-shot CLI call starts with none, so it pays a bounced node's `gpu_wait_ms` again (the node-published verdict that would
   replace the memory is not built).
7. **Attribution stays one card per node that held the job and one row per call.** An overflowing call opens its PAIR card only
   once a node has ACCEPTED the job. A node that bounces it after accepting has its card closed quiet (completed, never
   started, the reason kept; `core.RemoteAttribution.Bounce`) and writes no ledger row; the call's one asker row names the node
   that served it.
8. **When nothing admits, the call goes on to the local admission, from when it came back.** It joins the local line when the
   fleet attempt is over and not from when it arrived (Named limits). If that ends in a deferral (a place in line, a busy
   defer), the answer carries `cluster[]`: one row per roster node in config order (`busy`, `not-capable`, `unreachable`,
   `refused`, `bounced`, `skipped`), each with the reason in the node's own terms (the lease and its reason, the key that
   differs and the family of this machine that would match, the release or the refine it lacks), and the reason quotes them. A
   call that ran is not annotated.
9. **Never overflow** a call that resumes a place in the local line (it carries `waiter_token`: the place is kept, and the first
   queued answer's token simply lapses if the call went to a node instead), that runs under a lease its process inherited, on a
   box with no `delegate_remotes` (that path is byte for byte what it was, and asks the lane nothing), or for anything but an
   image job in this release. Graphs arrive with their own file check (a whole-node graph placed on a multi-GPU node would land
   on its display card); video, animation and audio carry no per-family identity yet.
10. **ADR 0077 changes in three sentences.** Decision 6: `auto` runs here when the lane is free and, when it is not and a
    matching node is idle, there. The consequence that `refine=false` defers on a remote route no longer holds for image jobs:
    it travels, gated on `refine_honoured`. The consequence about `waiter_token` stands: a call that goes to a node leaves the
    local place.

## Consequences

- A busy local lane stops being the only place an image call can wait, under every rule the cluster already had: the node's own
  grant decides whether it takes the job, so the host-RAM guard, the display-card rules and the idle-unload rule apply on the
  node exactly as they would to a local call. A remote job is zero-warm like any other: the node cold-starts ComfyUI and unloads
  it when the lease ends.
- The delegator never counts cards from a node's health and never overrides a node's refusal. A mis-predicted placement costs
  one bounce per node per pause (60 s, then 300 s): up to the node's `gpu_wait_ms` (90 s by default) parked on the node and one
  deferred row there, before the placer sees it. The health pre-filter (no lease of any class) makes that rare; if the logs show it is not, a place-now header read in
  admission is the first optimisation (a lane verdict published by the node, answered before a job exists), not built here.
- **Named limits.** The recipe is name plus size, not content (decision 3). One FIFO per delegating machine; cross-machine order is node arrival. A call resumed with its token stays in
  the local line, so a node that frees later is not used by it (the ticket queue with late binding is the next step). **A call
  that went to the fleet first queues locally from when it came back**, not from when it arrived: it joins the local line only
  when the fleet attempt is over (the lane question and the roster read, then at most 3 nodes, each up to its `gpu_wait_ms`), so a
  caller that arrived meanwhile is ahead of it, and one bounce can cost it 90 s of seniority (270 s for three nodes). Two halves would have to be fixed to carry the arrival time into the
  local admission: the lease queue (it takes an arrival time) and the in-process slot queue, which serves callers in the order they
  join and has no arrival time to take; the ticket queue of the next step keeps a call's place across the attempt and is where
  that is done, not here. Every
  remote job pays ComfyUI's cold start. The same recipe and seed on a different GPU architecture is the same composition, not
  bit-identical pixels. A hand-forced `route=remote` can still park beside a placed job (`concurrencyCapped` is false for media).
  A per-request `steps` needs the node's binding to have set cfg for a graph that takes both together.
- **Measured and unmeasured.** The recipe tests use fixtures that reproduce the image blocks of three live nodes as they stood
  in config text read on 2026-10-09 and 2026-10-10 (two from stored extracts; text, not a render), and the extract of one of
  them was cut inside the block it matters for; the live acceptance reads `image_recipes` on both nodes first. Nothing here was
  run against a live lease or a real render. The backoff steps, the attempt bound and the probe's 4 s card-table bound are chosen
  constants, not resource numbers; the host-RAM numbers are the guard's and are not chosen here.
- **Cost, stated plainly.** Every `auto` image call on a box that has the lane and a fleet pays one `nvidia-smi` read (bounded at
  4 s) for the lane question, and a roster read when the lane is not free, even when no node can ever match (the default family
  and the 2512 bf16 family of the busiest submitter have no remote match today). A caller that re-sends without its token pays it
  per re-send. The first sketch kept a 60 s copy of the card table; it was dropped for the AUTO plan, where the allocator reads the
  display card's free memory and a minute-old table can call busy a lane the grant would serve, which the contract forbids. Two
  follow-ups, neither built: a roster-first short circuit (health is memoised 2 s; when no published digest equals the local one the
  lane question need not be asked), and a short table copy for the pinned and whole-node plans only, where the table maps a pin
  to a card and sizes the host-RAM need (total VRAM) and nothing else. The delegator's own recipe is three or four stats of its
  model tree per call that gets as far as the lane question; the recipes of its OTHER families are read only when a node misses, to
  name the family that would match. A node's health rows are 60 s old at worst, and the node re-checks the digest at admission from
  the files then on disk.
- **An attribution edge.** An overflowing call opens its PAIR card when the node's 202 arrives, not before the POST. If the caller's
  own deadline ends the call after the node accepted but before the 202 was read, the node runs the job with no card on the
  delegator's side. Bounded: the call's budget is 2 h against a 20 min dispatch timeout, so it needs a caller-supplied deadline of
  minutes or less.

## Alternatives considered

- **Enable the pull queue (ADR 0030) for media.** Rejected for this release: the nodes in question cannot pull, the claim loop
  would have to be rebuilt first, and it brings a holder that is a single point through every deploy wave and at-least-once
  duplicate renders. It is the upgrade path if a second heavy submitter appears.
- **Extract the admission's read half into a shared function.** Rejected: that is a third edit of `acquireCards`.
- **Try the real run with no wait and see.** It works (a wait of 0 is one gated attempt), but every refusal opens and quiet-closes
  a PAIR card, which pollutes the cards one per overflowing call.
- **Match by name, or by raw config keys.** A name proves nothing, and the raw keys refuse a match the pixels would not notice.
- **Let the caller's `family` travel as given.** A name binds different files on different nodes, and an empty one lands on the
  node's default, which is another model.
- **Substitute a quantization when none matches.** The operator's rule that one node stays on the fast family, and the licence
  tag on a result, both make a silent substitution wrong; the answer names the family that would match instead.
