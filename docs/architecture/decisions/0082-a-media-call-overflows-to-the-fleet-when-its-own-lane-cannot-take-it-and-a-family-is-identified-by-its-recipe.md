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
   have served; it may call free a lane the grant then refuses, and the call then runs locally as before, after the one bounded card-table read the question costs. A call that resumes a place in line counts only the callers
   that arrived before that place, on every plan, because the lease queue serves a resumed place by arrival time (ADR 0083
   decision 4; `TestMediaLaneFreeOrdersByArrivalForACallThatResumesItsPlace`); a new arrival counts every other caller in line. Every doubt
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
6. **Refusals move on; an accepted job is final.** (ADR 0083 replaced the bound of three nodes for an image call: the ticket tries
   one node per scan and has no limit on how many it tries over its life. A graph keeps the bound: it is offered to at most three
   nodes, once.) A refusal at the door (`503`, `429`, `412`, any
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
   `unreachable`) and for how long the call was not offered to the node. The step numbers are chosen, not measured. The
   memory is the placer's package variable, so it is per process: the MCP server keeps it across the calls of its session, and a
   one-shot CLI call starts with none (its ticket keeps it across its own scans), so it pays a bounced node's `gpu_wait_ms` once
   (the node-published verdict that would replace the memory is not built).
7. **Attribution stays one card per node that held the job and one row per call.** An overflowing call opens its PAIR card only
   once a node has ACCEPTED the job. A node that bounces it after accepting has its card closed quiet (completed, never
   started, the reason kept; `core.RemoteAttribution.Bounce`) and writes no ledger row; the call's one asker row names the node
   that served it.
8. **When nothing admits, an image call is a ticket (ADR 0083).** It does not go on to the local admission from when it came back,
   as the first cut did: it waits for the first matching lane that frees anywhere, keeping its arrival time in the local line, and
   the answer carries `cluster[]`: one row per roster node in config order (`free`, `busy`, `draining`, `not-capable`,
   `unreachable`, `in-use`, `refused`, `bounced`, `skipped`), each with the reason in the node's own terms (the lease and its
   reason, the key that differs and the family of this machine that would match, the release or the refine it lacks) and since when.
   A call that ran is not annotated. **A whole-node graph (Amendment 1) is a ticket too** (ADR 0083 Amendment 1, which supersedes
   what 0.180.0 shipped: a graph that went on to the local admission from when it came back): when nothing admits it, it waits for
   the first matching lane that frees, and its answer carries the same `cluster[]`.
9. **Never overflow** a call that runs under a lease its process inherited, on a box with no `delegate_remotes` (that path is byte for
   byte what it was, and asks the lane nothing), or for anything but an image job or a whole-node graph in this release (Amendment 1
   adds the graph, under its own file check and a single-card rule). (An image call that resumed a place did not overflow in the
   first cut; ADR 0083 brings its place into the ticket, and its Amendment 1 does the same for a graph.) Video, animation and audio
   carry no per-family identity yet.
10. **ADR 0077 changes in three sentences.** Decision 6: `auto` runs here when the lane is free and, when it is not and a
    matching node is idle, there. The consequence that `refine=false` defers on a remote route no longer holds for image jobs:
    it travels, gated on `refine_honoured`. The consequence about `waiter_token` stands: a call that goes to a node leaves the
    local place.
11. **Amended by ADR 0083** in four places, for an image call and, by its Amendment 1, for a whole-node graph: decision 2 (a resumed
    place is ordered by arrival on every plan), decision 6 (no bound of three nodes), decision 8 (a ticket, not a late local run) and
    decision 9 (a call with a `waiter_token` overflows).

## Consequences

- A busy local lane stops being the only place an image call can wait, under every rule the cluster already had: the node's own
  grant decides whether it takes the job, so the host-RAM guard, the display-card rules and the idle-unload rule apply on the
  node exactly as they would to a local call. A remote job is zero-warm like any other: the node cold-starts ComfyUI and unloads
  it when the lease ends.
- The delegator never counts cards from a node's health and never overrides a node's refusal. A mis-predicted placement costs
  one bounce per node per pause (60 s, then 300 s): up to the node's `gpu_wait_ms` (90 s by default) parked on the node and one
  deferred row there, before the placer sees it. The health pre-filter (no lease of any class) makes that rare; if the logs show it is not, a place-now header read in
  admission is the first optimisation (a lane verdict published by the node, answered before a job exists), not built here.
- **Named limits.** The recipe is name plus size, not content (decision 3). One FIFO per delegating machine; cross-machine order is node arrival. ADR 0083 closes the two limits the first cut named: a call resumed with
  its token joins the cluster queue and brings its place with it, and a call no node could take keeps its arrival time through the
  ticket's place in the local line, so a bounce no longer costs it its seniority. Every
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

## Amendment 1 (2026-10-10, P0 step S4): a whole-node graph overflows too

Decision 9 left graphs out: a graph has no recipe, and a whole-node graph placed on a multi-GPU node could land on its display
card. This amendment adds them under four rules; every other decision stands, and none of the four is a recipe.

1. **A graph's identity is that the node holds the files it names, and the node decides it at its door.** Before it writes a
   temp file, takes a lease or starts ComfyUI, `buildRunGraph` reads the graph: any string input, under any key but one, whose
   value ends in `.safetensors`, `.gguf`, `.pth`, `.pt`, `.ckpt` or `.sft` is a weight file (the loaders' key names are
   open-ended, the extension is not). The two exclusions the code keeps: the key `filename_prefix` (an output prefix, whatever
   its value) is never read, and neither is a value that contains `://` (a URL). The file input of `LoadImage`, `LoadImageMask`,
   `LoadImageOutput`, `LoadVideo` and `LoadAudio` is an
   input file. A weight file must resolve **by name** under any class directory of any of the node's models roots (a class
   directory that is a link counts), the way a ComfyUI loader opens it (subfolders included, no search for the basename;
   `mediacap.MissingModelFilesUpTo`); an input file must be in ComfyUI's `input` directory (`output` or `temp` when its annotation
   says so). A name that is absolute or climbs out with `..` is never looked up and reads as missing. A node
   that lacks any of them answers with the existing re-placeable route-not-ready refusal, **503**, naming what is missing (up to
   six names of each kind; past six weight files it says `7 or more files ... and more` and gives no count, because the node stops
   looking at the seventh: the door is open to any peer and a graph can name thousands of files, so the work is bounded by what
   the refusal shows and not by the graph), before any job exists. A model the graph's manifest tells the node to fetch (a `models[]` entry with a
   `source_url`) is not refused: the satisfier downloads it before ComfyUI starts. **Input files do not travel with a graph**
   (the dispatch door carries JSON; the media-job door carries no input for `run-graph`), so the refusal says so; carrying
   them is a named seam, not built. "Holds" is **name, not content**. For a weight file the name is the practical identity; an
   input file is the caller's own with a generic name, so a same-named file of other bytes would run the graph on the wrong
   bytes, and rule 2 keeps such a graph off the auto-overflow. A custom node's own file key is not read. The delegator sends no
   family and no digest, and requires the node to run its own release (the check ships with it), where a new health key would
   have said the same; that proves the check only because the release that adds it carries a new version number, so the bump
   ships with the merge, before any delegator is deployed. The release proof holds for **every** remote placement of a graph, as
   rule 3's card rule does: the overflow asks it of its candidates, and `route remote` and a thin client's `auto` ask it when they
   pick a node, so a node on another release, or one that publishes none, is a named miss on all three.
2. **The trigger is the image's; the placement is the ticket's.** `Pipeline.MediaLaneFree` answers for a whole-node `run_graph` with the
   whole-node reading (any lease, in-process slot or place in line, and the host's memory against the media class default, the
   largest family the box binds: the lease is taken for the graph as a whole before anything reads its weights, so that is an
   upper bound and not the graph's own size), under the same contract: a relaxation of the real wait-0 admission, held by
   a differential test, that writes nothing. ADR 0083 turned an image call into a ticket, and **its Amendment 1 does the same for a
   graph** (it supersedes the one-shot placement 0.180.0 shipped, which offered the graph once to at most three nodes and then
   joined the local queue late): a busy graph waits for the first matching lane that frees, the scanner applying rules 1, 3 and 4
   below to each node. A node's 503 for a file the graph names carries `refusal: "not_capable"`: the ticket marks that node
   not-capable for itself, quoting the node, and puts no pause on it (a full node's 503 is the placer's, as for an image). A graph
   that names `devices` (refused by `plan()` before the lane is asked) and one under an inherited lease are no tickets and stay here.
   One that reads an input file is a ticket that is never offered to a node; it waits for this machine's own lane, keeps its place and
   position, and every answer says the fleet was not asked and why (`fleet_note`). A call with no lane of its own to wait for
   (`route remote`, or `auto` on a thin client) is placed as before, checked by name only, and held to rule 1's release proof and
   rules 3 and 4. Hashing each input file to the node would lift that last exception and close that limit; it is not built.
3. **A whole-node graph is placed only on a node that publishes exactly one card.** A graph that names no devices holds the
   whole node and runs in the default ComfyUI instance, which sees every card; on a node with several, the delegator cannot
   know which card that instance picks and it may be the display card. So a node with more than one card is a named miss, and
   so is a node that publishes no `gpu_devices` (unknown is never a pass). The rule is the delegator's **placement** rule and it
   covers every remote placement of a graph: the overflow, `route remote` and a thin client alike, so a graph cannot be sent to
   a multi-GPU node by this client at all. It does not change what such a node does with a graph sent to it by hand or run on
   its own machine: that local whole-node case is the parked item the operator knows. A single card that drives a display passes
   the rule: a box whose only card is its display card runs there by necessity (the allocator's carve-out, as for an image).
4. **`min_vram_gb`.** A call may ask for a node whose largest `gpu_devices[].vram_total_gb` is at least N GB; a node that
   publishes no cards cannot be shown to have it and is a miss. A card counts as the size it rounds to: nvidia-smi reports
   MiB/1024, so a 16 GB card reads 15.92 or 15.93 GiB (the figures in `docs/FLEET-NODE.md`) and a literal comparison with 16
   would shut out every one. Absent the argument, a graph that names `model_family` `wan22` (or `wan2.2`, `wan`) and carries a
   `height` input of 720 or more (the tallest of any node; none = no floor) gets a floor of 16. **That row is CHOSEN from the
   film owner's stated constraint that Wan 2.2 14B at 720p needs a 16 GB card; it is not a measurement**, and every answer that
   cites it says so. The caller's number wins over it, 0 included. The floor filters remote candidates only: the local lane is
   the node's own grant.

**Named limits of the amendment.** *Closed by ADR 0083 Amendment 1:* in 0.180.0 a graph did not wait in the cluster queue; it is a ticket
now, identified by the content of its graph and manifest. The file check is by name (a same-named file of other bytes is held), which is why a graph that
reads an input file is not auto-overflowed, though a call that must leave its machine anyway keeps the same-named-file risk. Input files need to be on the node already when a node is named by hand. The node's
host-RAM guard sizes a graph at the media class default, not at the files the door has resolved, so a graph naming weights bigger
than every bound family can be under-counted. The default floor reads two things the harness otherwise never reads in a graph (`model_family`, the
`height` inputs), and a graph that uses neither key gets none. A forced or thin-client graph can no longer reach a multi-GPU
node, the workstation among them; whether to relax that for the operator's own machine, once the monitors-off detector exists, is the
operator's. With the fleet as the film owner described it on 2026-10-09 (the 16 GB cards are the three of the multi-card
workstation, the single-card nodes carry 8 GB cards) rules 3 and 4 together leave a graph that gets the default Wan floor no remote
target: it waits for its own lane. The durable fix is on the node (keep a whole-node graph off the display card there). A graph is
placed only on a node that runs this machine's release, on every route, so a fleet part-way through an upgrade routes graphs only
to the nodes already on it (and `route remote` to a node still on the old release is a named miss, not a send). Nothing here was run live: A8 in the step report is the first.
