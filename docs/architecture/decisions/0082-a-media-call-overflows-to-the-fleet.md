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
   (a lease this process inherited, a card table that does not read in 4 s nor in the one 10 s retry that follows a read that ran
   out of time, a request it does not model) reads as free; the card table that does not read and a lease directory that does not
   open are marked unread (`Unjudged`, ADR 0083 decision 5), and a router then looks at the fleet before it runs the call here. A
   card table that was not read leaves a log line ("media lane probe", F55: the first time, then once a minute per probe and cause
   while the fault lasts). It
   creates no lease, place in line, epoch, waiter record, ledger row, PAIR card or ComfyUI instance, and it removes nothing: the
   readers of the line it shares with the admission prune a record they find dead (an expired place, a waiter that stopped
   polling) as housekeeping, and the probe reads through `gpulease.Manager.ReadOnly`, a view that skips such a record and leaves
   it on disk. A differential test drives the real wait-0 admission over a table of lane states and holds the prober to the one
   direction that matters, and `TestMediaLaneFreeWritesNothing` snapshots the lease root over each of them, those records included.
3. **A family is its recipe (`mediacap.ImageRecipe`).** The recipe is the weight files the graph loads (checkpoint, text
   encoder, VAE, LoRA), each with its name, byte size on the node's disk and sampled content fingerprint (Amendment 2), and the
   sampling it renders with, digested (sha256
   of canonical JSON) over RESOLVED values: a key the binding leaves unset takes the builder's own default, from a table that a
   test reads out of `render/wf-qwen-image-21.mjs` and `render/comfy-render.mjs`, so it cannot drift from the code that renders
   (Qwen-Image-2.1 only in this release; any other graph leaves unset keys unset, which is stricter, never looser). The license
   the result is tagged with is part of it. Node-local keys (`comfy_*`, timeouts, scripts, `reserve_vram`, the pool keys) are not;
   a test fails when a key joins the image overlay's clear list unclassified. No recipe exists for an sd.cpp binding, so a
   ComfyUI recipe never matches one. The match is strict, also requires the same `harness_version` (the graph builders ship
   with the release) and a recipe with no missing file, and never substitutes: a caller who accepts another build names that
   family himself, and the answer tells him which of his own families would match. **The first cut (0.179.0) identified a file by
   name plus size, not content**: it told a bf16 build from an int8 one and caught a truncated copy, but not two same-named files
   of one size whose bytes differ (a full-length altered copy, a same-named re-release with the same tensor layout, a file updated
   in place on one node), and the node's 412 re-check recomputed the same size-based digest, so it did not catch them either.
   Amendment 2 adds a content fingerprint per weight file and says what it sees and what it does not.
4. **Nodes publish and check recipes.** `/fleet/health` gains `image_recipes[]` (one row per ComfyUI binding that names a
   checkpoint: name, default, digest, files with sizes and fingerprints, resolved sampling, license, and which sampling keys the
   binding set) and `refine_honoured`, from a memo of the route cache's 60 s so health stays cheap (and never waiting for a hash:
   Amendment 2). An image-gen dispatch may carry
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
6. **Refusals move on; an accepted job is final.** (ADR 0083 replaced the bound of three nodes for an image call: the ticket tries one node per scan and has no limit on how many it tries over its life. A graph kept the bound in 0.180.0, where it was offered to at most three nodes, once; its Amendment 1 makes the graph a ticket and lifts it.) A refusal at the door (`503`, `429`, `412`, any
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
   A call that ran is not annotated. **A whole-node graph (Amendment 1) is a ticket too** (ADR 0083 Amendment 1, which supersedes the one-shot placement of 0.180.0: a graph that went on to the local admission from when it came back): when nothing admits it, it waits for
   the first matching lane that frees, and its answer carries the same `cluster[]`.
9. **Never overflow** a call that runs under a lease its process inherited, on a box with no `delegate_remotes` (that path is byte for
   byte what it was, and asks the lane nothing), or for anything but an image job or a whole-node graph in this release (Amendment 1
   adds the graph, under its own file check and a single-card rule). (An image call that resumed a place did not overflow in the
   first cut; ADR 0083 brings its place into the ticket, and its Amendment 1 does the same for a graph.) Video, animation and audio
   carry no per-family identity yet (Amendment 3 lets a caller name the node that runs one and has a node publish the family rows of
   those lanes; their identity and their overflow are later releases).
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
- **Named limits.** The content check is a sample of each weight file, not a full hash, and does not see a change confined to the stretch between two sample blocks (about a sixty-third of the file; Amendment 2). One FIFO per delegating machine; cross-machine order is node arrival. ADR 0083 closes the two limits the first cut named: a call resumed with
  its token joins the cluster queue and brings its place with it, and a call no node could take keeps its arrival time through the
  ticket's place in the local line, so a bounce no longer costs it its seniority. Every
  remote job pays ComfyUI's cold start. The same recipe and seed on a different GPU architecture is the same composition, not
  bit-identical pixels. A hand-forced `route=remote` can still park beside a placed job (`concurrencyCapped` is false for media).
  A per-request `steps` needs the node's binding to have set cfg for a graph that takes both together.
- **Measured and unmeasured.** The recipe tests use fixtures that reproduce the image blocks of three live nodes as they stood
  in config text read on 2026-10-09 and 2026-10-10 (two from stored extracts; text, not a render), and the extract of one of
  them was cut inside the block it matters for; the live acceptance reads `image_recipes` on both nodes first. Nothing here was
  run against a live lease or a real render. The backoff steps, the attempt bound and the probe's 4 s card-table bound and 10 s retry are chosen
  constants, not resource numbers; the host-RAM numbers are the guard's and are not chosen here.
- **Cost, stated plainly.** Every `auto` image call on a box that has the lane and a fleet pays one `nvidia-smi` read (bounded at
  4 s, and read once more under 10 s when it ran out of time) for the lane question, and a roster read when the lane is not free, even when no node can ever match (the default family
  and the 2512 bf16 family of the busiest submitter have no remote match today). A caller that re-sends without its token pays it
  per re-send. The first sketch kept a 60 s copy of the card table; it was dropped for the AUTO plan, where the allocator reads the
  display card's free memory and a minute-old table can call busy a lane the grant would serve, which the contract forbids. Two
  follow-ups, neither built: a roster-first short circuit (health is memoised 2 s; when no published digest equals the local one the
  lane question need not be asked), and a short table copy for the pinned and whole-node plans only, where the table maps a pin
  to a card and sizes the host-RAM need (total VRAM) and nothing else. The delegator's own recipe is three or four stats of its model tree per call whose lane read busy or could not be read (the lane is asked first, and a call whose lane is free reads no weight file) and, the first time this process sees a file, a read of a sample of at most 64 MiB of it (Amendment 2); the recipes of its OTHER families are read only when a node misses, from the fingerprint cache without waiting, to
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
   graph** (it supersedes the one-shot placement of 0.180.0, which offered the graph once to at most three nodes and then joined the local queue late): a busy graph waits for the first matching lane that frees, the scanner applying rules 1, 3 and 4
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

**Named limit of the lane probe at an exact tie (found in the 0.181.0 integration).** The lane table orders places by the millisecond
they were left, and the real admission counts only a place STRICTLY earlier as ahead of a call (so simultaneous arrivals do not wait
on each other). On the whole-node path, for a NEW arrival, the probe (`laneReading.callersAhead` with no `since`) counts every live place
on the call's cards as ahead without comparing times, so when a place was left in the very millisecond the call arrives the probe says
busy where the grant would succeed: the one direction the probe's contract forbids. The window is one millisecond and the cost is a
ticket whose first scan grants it. A
test of the table had stated an order it did not guarantee (a place left with `time.Now()` against a call arriving in the same
millisecond) and was fixed in the TEST (`nextMillisecond`, the lane tables of `medialane_graph_test.go` and `medialane_test.go`); the
probe is not changed. The follow-up that closes it is to pass the call's own arrival as `since` for a new arrival in
`laneReading.wholeNode`.

**Named limits of the amendment.** *Closed by ADR 0083 Amendment 1:* in 0.180.0 a graph did not wait in the cluster queue; it is a ticket now, identified by the content of its graph and manifest. The file check is by name (a same-named file of other bytes is held), which is why a graph that
reads an input file is not auto-overflowed, though a call that must leave its machine anyway keeps the same-named-file risk. Input files need to be on the node already when a node is named by hand. The node's
host-RAM guard sizes a graph at the media class default, not at the files the door has resolved, so a graph naming weights bigger
than every bound family can be under-counted. The default floor reads two things the harness otherwise never reads in a graph (`model_family`, the
`height` inputs), and a graph that uses neither key gets none. A forced or thin-client graph can no longer reach a multi-GPU
node, the workstation among them; whether to relax that for the operator's own machine, once the monitors-off detector exists, is the
operator's. With the fleet as the film owner described it on 2026-10-09 (the 16 GB cards are the three of the multi-card
workstation, the single-card nodes carry 8 GB cards) rules 3 and 4 together leave a graph that gets the default Wan floor no remote
target: it waits for its own lane. The durable fix is on the node (keep a whole-node graph off the display card there). A graph is
placed only on a node that runs this machine's release, on every route, so a fleet part-way through an upgrade routes graphs only
to the nodes already on it (and `route remote` to a node still on the old release is a named miss, not a send). Nothing here was run live: the live acceptance named A8 in the CHANGELOG entry of that change is the first.

## Amendment 2 (2026-10-10, P0 step F59): a family's identity includes the content of its weight files

Decision 3 identified a weight file by its name and byte size and named the limit: two same-named files of one size with
different bytes digested alike, so a delegator could send a job to a node that renders other pixels, and the node's 412
re-check, which recomputes the same digest, could not catch it. This amendment closes that for the differences a sample of a
file can see. Every other decision stands.

1. **Each present weight file carries a content fingerprint, and it is in the digest.** `mediacap.RecipeFile.Fingerprint` is a
   sha256 over a scheme tag, the file's size and a sample of its bytes: 64 blocks of 1 MiB at evenly spaced offsets, the first
   at the start of the file and the last ending at its end, or the whole file when it is 64 MiB or smaller. The block size and
   the block count are **chosen**, not derived from a requirement. The path is not in it (the same bytes under two paths are one
   file, which is what lets two machines agree), and `RecipeVersion` is 2, so a digest of the first cut can never equal one of
   this release. A family that has a file the node does not hold keeps `bytes` -1 and no fingerprint, and never matches.
2. **A sample, not the whole file, and why (the times are measured, the weight given to the window is a judgement).** On one workstation on 2026-10-10 (its local model store;
   the page cache was not controlled, so read these as warm-cache figures) the sample read 0.17 to 0.87 s per file for files from 0.36 GB to 42 GB. A whole-file
   sha256 of the same store ran at 389 MB/s on one core (3.8 s for a 1.47 GB file), which extrapolates (not run) to about 50 s for
   a 20 GB checkpoint and about 110 s for a 42 GB one, per node, after every release and every model change, with the node unable
   to match until it finished: minutes of a core and of the disk, and a window in which the fleet cannot take a call. That the
   window is too costly is a judgement, not a measurement, and RAM was not the reason (a streaming hash holds one 1 MiB buffer). The sample catches the failure this exists for: two real builds
   of one 42 GB transformer that share a size differed in 58 of the 64 sample blocks. There is no persisted cache: the
   fingerprint cache is in memory, keyed by path, size and modification time, so a restart reads each file's sample again
   (0.17 to 0.87 s per file on the machine measured; a cold or network disk was not measured) and nothing on disk can go stale or be written by two processes.
3. **What it does not see (named).** (a) A change confined to the stretch between two sample blocks. The blocks are evenly
   spaced, so that stretch is about a sixty-third of the file less one block: roughly 0.3 GB of a 20 GB checkpoint and 0.65 GB of
   a 42 GB one (`TestTheUnsampledStretchBetweenTwoBlocksIsAboutASixtyThirdOfTheFile` pins those widths, and a test rewrites a
   whole stretch and shows the fingerprint unchanged). Every byte in it can differ and the digest is the same: a layer-wise merge
   that changes a few transformer blocks, a VAE or text encoder baked into a checkpoint, a few corrupted bytes. A retrain or a
   fine-tune that touches the weights throughout is seen; a change that leaves such a stretch alone may not be. (b) A file
   replaced by one of the same size **and** the same modification time (a copy that preserves times) while the process that
   cached the old reading is still running. A full hash would catch both; after a copy that is suspect, or a merge swapped in,
   compare whole-file hashes (`sha256sum`) on the nodes. Amendment 1's weight check for a graph is still by name and is not
   changed by this one.
4. **Computed off the request path; a file not yet hashed is a named miss.** `/fleet/health` never waits for a hash. It reads the
   cache (`Fingerprinter.Peek`); a file not yet hashed starts one background read, one file at a time in this process (a model load in
   another process, ComfyUI's, is not coordinated with), and is published `"fingerprint":"pending"`; a file that could not be read, or that changed while it was read, is
   `"unreadable"` and is tried again after 30 s (chosen). **A row with any such file publishes an empty `digest`**: two nodes
   that were both pending would otherwise share a digest of unhashed files, which is the match by size this amendment ends. The
   row memo is also rebuilt when a read settles, so a pending row turns into its digest on the next health read and not up to
   a minute later. The node's 412 re-check and the delegator's own recipe wait for a fingerprint they do not have
   (`Fingerprinter.AwaitContext`, the same cache and the same single worker), because a dispatch and a placement are not health.
   **The wait has a bound, and it is per file**: the wait for one file's fingerprint ends when the request's context does (the dispatch handler's, the door's) and, whatever the context, after 20 s (chosen; a cold family of four files read in a few seconds on the machine measured), and answers `pending` for that file. A recipe waits for its files one after another, so the bound of a whole recipe is its file count times 20 s (four files: 80 s) and is reached only when the request lives that long and every read is stalled behind the single worker; the request's own end stops all of it at once.
   A family with a file the node cannot fingerprint, or has not finished, is refused with 412 naming the file and its state, and
   the cause (decision 7); at the door the same state is no identity for that call, which runs on the local line and logs the
   reason once (the identity is built only when the lane read busy: a free lane reads no file). **A read that does not finish in
   60 s (chosen) is given up on**: its file is `unreadable`, the worker serves
   the other files, and the read, which cannot be cancelled, runs on until the disk lets it return; the file is not read again
   meanwhile, so a hung disk costs one goroutine and one handle per stuck file, and one log line. Until the 60 s pass, a stalled
   read holds the single worker and every other file stays `pending`: the node publishes no digest for those families and is a
   named miss, which is what a node with a hung disk should be. Nothing watches the disk beyond that (no watchdog, by the
   operator's rule).
5. **A recipe that is not fully fingerprinted is never compared, on either side.** `localIdentity` gives no identity to a family
   with an unfingerprinted present file (the call runs as it does for a missing file, and the door and the adopter log why); `matchNode` refuses to compare when this
   machine's recipe has one (two all-pending recipes are the same string), and a node row whose files are not all hashed is a
   miss whether or not it carries a digest (the files are read, not only the digest). When the node holds this machine's build
   by name, size and sampling and has only not finished hashing it, the cluster row is `not-capable` and says so, and that the
   node is a candidate once it has; the ticket's next scan reads health again. A same-named, same-size file with other bytes is
   a miss that names it: `imagegen_ckpt: <name> has the same size on both and different content (fingerprint <12 hex> vs <12 hex>)`. The
   "send family=X to use its Y" hint reads this machine's other families from the cache without waiting and leaves out a family
   not hashed yet, so it never holds the ticket scan up on a hash; it is built again at the next miss while any family was left
   out, so a ticket that waits for hours gains the hint once the hashes land.
6. **The deploy story.** The digest is version 2 and a recipe is compared only between the same release (`harness_version`), so a
   fleet part-way through the upgrade matches only inside each release: a delegator at this release finds no match on a node still
   on the one before, and a node at this release is matched by no older delegator, and an overflow then degrades to the local
   line and the ticket as it does for any release skew. **Every node that should take overflow must run this release before
   the live matches can be read again.** A dispatch is sent only to a node whose health row carries a
   digest, which the node publishes once it has read its files, so the first dispatch after a node restarts finds them cached and is
   not refused for want of them. A dispatch that does meet a file the node has not read (one that changed after its row was
   published, or a cache that was dropped) waits for it (decision 4: until the request ends and, whatever the request, 20 s at most for each file; a family of four files read in a few seconds on the machine measured) and is admitted when the digest then matches. Only a
   read still unfinished at that bound (a slow or cold disk, a stalled read) is answered 412 with the cause `pending`, which the
   delegator reads as a wait and not as a finding (decision 7).
7. **A 412 says why, and the delegator tells a wait from a difference.** A 412 used to mean one thing, that the node's files had
   changed since it published them, and the delegator left such a node alone for a minute and, on the next, for the cap (5 minutes),
   counting each as a bounce. A node can now also refuse because it has not finished reading its weight files and cannot say yet
   whether the family is what it was sent, which is a wait for an answer and not a finding. The refusal's JSON body carries a
   `cause` beside `status` and `error`, one of `pending` (every file the node lacks a fingerprint for is still being read),
   `unreadable` (a file is there and could not be read, so the node cannot vouch for the family) or `changed` (the family no longer
   digests to the digest sent, or the node no longer serves it or binds it to another engine). The member is read as a member and
   not matched out of a sentence, and the node's words choose nothing beyond these three: a refusal that names none this build
   knows (a proxy, a node of another build, a node of an earlier build) is bounced like `changed`, the conservative reading, and its
   row says the node did not say why. A delegator that reads `pending` leaves the node alone for the first bounce step (a minute:
   chosen) and does **not** count it as a bounce, so a run of `pending` answers never reaches the cap and a later `changed` still
   starts at a minute. `unreadable` and `changed` are bounced as before. The cluster row names the cause. The pause is there because
   the rows a node published can be up to a minute old (its health memo is 60 s) and would offer it the job again at the next scan,
   each dispatch waiting up to 20 s for the same answer; what the node is, is decided by the health row the next scan reads, not by
   the pause, and it is a minute and not the cap because a node that is reading its files is expected to have read them by then.

**Named limits of the amendment.** For a window of seconds after a node starts, or after one of its weight files changes (0.17 to
0.87 s per file on the machine measured, with a warm cache; a cold disk was not measured), its family is
`pending` and the node is a named miss; a delegator that starts reads its own files once, a sample per file, before its first
comparison. **A one-shot CLI call is a fresh process, so on a busy lane it reads its family's samples again on every call, before
it makes its ticket** (an estimate from the figures above: one to three seconds for a family of three or four files), and on a free
lane it reads nothing, because the lane is asked first; the MCP server and `fleet-serve` keep the cache for the life of the
process. The two sizes, the 30 s retry, the 20 s wait and the 60 s read bound are chosen, and the timings above are one machine's, with the
page cache not controlled. The 30 s retry is keyed on the file's state (path, size, modification time), so a file still being
copied, whose size or time keeps moving, is read again on every health refresh (64 MiB at most per read, bounded by the health
poll) and not once per 30 s. Nothing here was run against a
live fleet: the acceptance is the three live matches of 0.179.0 read again after every node runs this release
(docs/systems/media-generation.md, "Reading a node's recipes").

## Amendment 3 (2026-10-13, N7): a video, animation or audio call can name the node that runs it, and a node publishes its family rows

Decision 9 left video, animation and audio out of overflow because they carry no per-family identity, and the standing order for the
fleet (the P0 plan) is that any machine that holds the model takes the call. **D2 is decided: video, animation and audio overflow
like images, on by default, with `route local` as the escape.** That is three releases of work, and this amendment is the first and the
smallest of them: it makes no call overflow. It lets a caller **name** the node, which needs no identity (naming a node is the
caller's explicit act, as `route remote` is), and it has each node publish what its video, animation and audio bindings name, so that
a call for a weight the node lacks is refused before anything is sent. Every other decision stands.

1. **`node` names the one node that runs the call.** `offload_generate_video`, `offload_animate_character` and
   `offload_generate_audio` (CLI `--node` on `generate-video`, `animate-character`, `generate-audio`) take `node`: a node id (the
   `node_id` its `/fleet/health` publishes, in any case) or one of the `delegate_remotes` URLs. The named node is the only
   candidate. A box that has the lane itself still sends the call there (`auto` with a node leaves this machine, as `remote` does),
   no other node is tried, and this machine is not asked. `route local` with a node is a contradiction and is refused; a node on an
   image or a graph call is refused by name by the library and the CLI (the image and graph verbs have no `--node`, and the image batch
   none), because this release carries it for the three lanes only. The image and graph MCP tools list no `node`, so a `node` sent to them
   is dropped as any argument a tool does not list is; no door advertises a parameter it would drop. `meta.placement` reads `remote: named node <name>`.
2. **A node that cannot take the call is refused before any POST, by a word.** The deferred answer carries
   `{refusal, node, lane, family, missing[], roster[]}` in its data. Two are the caller's mistake and are `contract` defers:
   `node_unknown` (the name matches no node of the roster this call may use; `roster[]` lists the ids that answered) and
   `node_is_self` (the name is this machine's own node, whose lane is the local route). Four are a fact about the reachable node,
   which waiting does not change, and are filed as the misses `pickNodeFor` already files for a roster left out for configuration:
   `capacity` with `err_class` `no_node_configured`, narrowed to the one node the caller chose. `node_lacks_route`: the node does not
   serve the lane or its route is not CONFIGURED (the answer names the route state and, when the node published its row, the files
   the row lacks). `node_lacks_media_job`: the call carries input files and the node has not opened the media-job door. `node_lacks_model`:
   see 4. `node_below_min_vram`: see 6. **A node that does not answer, or holds a text lease, is not refused:** it is an ordinary
   `capacity` defer with no err class, because it may take the call later; and a node id that no node which answered has, while some entry
   that was **dialled** did not answer, is that same defer (the id may be one of them), not `node_unknown`; a URL outside the roster is
   always `node_unknown`. **A roster entry the tailnet guard refuses is not such an entry:** it is never dialled, has no node id, and
   waiting does not change it, so an id no answering node has is `node_unknown` however many entries the guard refuses, and the reason
   names them (`not dialled`) for the operator who meant one. A URL the caller names that is on the roster but that the guard refuses
   is a configuration fact like the four, filed `capacity` with `err_class` `no_node_configured`, with no refusal word (not the `config`
   class, which reads as a broken stack). Two answers of a named call therefore carry no refusal data: that one and a call on a
   machine that has no `delegate_remotes` at all, which never reaches a roster and is the plain `config` defer every remote call on
   such a machine gets (`no delegate_remotes configured`): the name is not the caller's mistake there, the fleet is the operator's to
   configure, and the local lane is not tried. The choice of `capacity` for the four
   configuration refusals is the pairing the code already has for `no_node_configured` (a test of the image batch pins it); the
   caller's remedy is to name another node, not to wait.
3. **A node publishes the family rows of its video, animation and audio bindings.** `/fleet/health` gains `media_recipes[]`, additive
   (a node that has no ComfyUI install, `comfy_dir` being a directory on it, and binds no sd.cpp video family omits it, and a reader
   that does not know it sees the shape it always did; the shipped default config binds the three ComfyUI scripts on every machine, so a
   binding alone makes no row, while a node that has the install lists every lane its config binds, an image-only node with ComfyUI
   among them, with -1 for each weight it lacks): one row `{lane, name, default, engine, digest, files[{role, name, bytes}], license, commercial_use}` per family. A video
   row is named by the value of the `model` parameter that selects it: the ComfyUI families `wan22`, `ltx25`, `hunyuan`, `h3`
   (the default family always, any other only when the node bound it in `videogen_families` or holds every file its graph loads) and
   an sd.cpp family by its own name; an animation row is `wan-animate2`; a music row is `acestep`. `bytes` is the size where the node
   finds the file and **-1 where it does not**, so a row with a missing weight is still listed and a refusal can name the file. The
   rows are built from the files the bindings name and a stat per file, memoised for a minute like the route verdicts. **A row is not
   an identity in this release:** it has no content fingerprint, its `digest` is always empty, and nothing matches two rows against
   each other. A ComfyUI file keeps its loader-relative name with its subfolders; an absolute, drive-lettered or climbing name is
   published as its file name, and an sd.cpp file by its bare file name whatever its path looks like (a relative path keeps no
   subfolder), so no row carries a path of the node's.
   There is no row for voice, for the audio.cpp engines or for the sd.cpp animation lane: a reader takes the absence as unknown.
4. **A row is read for one thing: the weight files it says the node lacks.** `node_lacks_model` is raised only when the node
   published at least one row for the call's lane and the row of the family the call renders is absent or shows a file with `bytes`
   -1. With no `model`, that is the node's default row. A `model` is matched by the row name exactly (an sd.cpp family) and otherwise
   folded the way the node folds it, through `mediacap.VideoBinding`, which carries its own copy of the runner's closed set
   (`mediacap.videoRunnerFamily`; `pipeline.canonicalVideoFamily` and `fleetnode.familyFor` carry the others, and nothing can import
   all three), pinned to the pipeline's fold by `TestMediacapFoldsAVideoFamilyTheWayThePipelineRendersIt`: the runner's closed set
   (`ltx25`, `h3`, `hunyuan`) renders itself and every other spelling, the documented `wan` included, renders Wan 2.2. `ace` has no
   row and is not judged. A lane the node publishes no row for is **unknown, not refused**: the call goes on to the node's own door,
   which refuses a missing weight itself with a 503, exactly as a forced call does today; voice, the sd.cpp animation lane and a node
   that predates the rows are in that case. Music is judged by the `acestep` row; voice is never refused for a music row.
5. **The answer says what rendered.** A named call that ran on a node whose row judged it carries `rendered_family` and
   `rendered_files` in its data beside the node's own answer: the family and the loader names the node's row lists for it. That is
   the row the node published, not a check of the render, which is a node-side fact the delegator does not repeat. A call that
   names no node is not annotated and no row is read for it.
6. **`min_vram_gb` on video and animation.** The caller's number (a number of GB, 0 or more; a card counts as the size it rounds to,
   as for a graph) filters the remote candidates of a video or animation call, in every placement that leaves the machine, and on a
   named node it is `node_below_min_vram`. It filters nodes only; this machine's lane is its own grant. A malformed value is refused
   by name before the lane is asked, on every route, as a graph's is. The Wan 720p default floor of a graph is not applied to these
   calls: it needs the resolved height, which belongs with the identity release.
7. **What does not change.** A call with no `node` is placed exactly as it was: `auto`, `remote`, a thin client's `auto`, the image
   overflow, the graph overflow, the image batch. No `node`-less call reads a row. The image recipe, `RecipeVersion` and every image digest
   are untouched. A node whose media-job door is closed (an operator's choice) takes a named call with no input file and
   refuses one with input files by `node_lacks_media_job`; this amendment does not design around the closed door.

**The releases that follow (written down so the order is not lost; none of this is built).**

- **Identity and one-shot overflow.** Rows gain a `digest` and content fingerprints: a lane recipe per engine (the ComfyUI video
  families from `VideoModelFilesFor`; sd.cpp video from the model files and a hash of the extra arguments; the ComfyUI animation
  and music files; audio.cpp; voice last, from the files of the hub snapshot `refs/main` reached by a followed stat, and the default
  reference sample by content), the resolved settings that change pixels (runner family, Wan loader and decode, `fps`, the upscale
  model and its size when `upscale` is asked, the license) and not the per-box size defaults, which travel as the caller's resolved
  width, height and frames. The node checks the digest at its door with the 412 of decision 3; the delegator matches by equal
  digest, same release, route CONFIGURED, the media-job door when files travel, the request's `fast` and `upscale` extras, and the
  floor from the resolved height. The busy-lane probe in `internal/pipeline` is generalised from image to a per-task need by calling
  the same functions the grants call (`videoNeed`, `singleCardNeed`, `wholeNeed`) with sd.cpp, audio.cpp and endpoint requests
  answering "not judged" before any card read; the resolver copies (`pipeline.resolveVideoFamily`, `mediacap.videoRunnerFamily`,
  `fleetnode.familyFor`) stay where they are, pinned by a test. This is where D2 becomes behaviour for a call with no input file.
- **Tickets with input files.** A call that carries a still, a reference and driver or a clone sample becomes a ticket whose
  inputs it copies (owner-only, swept with the ticket, bounded per ticket and machine-wide), keyed lazily by their content after the
  lane reads busy, with the media-job bundle built once per scan and a fresh single-node health read before an upload; the MCP doors
  move from `Run` to `RunWith` with `wait_s`. Until a node with an equal digest and an open door exists, an input-carrying call keeps
  the local-only ticket a graph that reads input files has. Whether to open a ComfyUI node's media-job door is the operator's call
  and not assumed.

**Named limits of the amendment.** (a) A row is a size, not content: a weight replaced by a file of another content under the same
name is present in a row, and a delegator that wants the call to render the same pixels as its own lane gets no such promise from
this release (it is the identity release's). (b) A node that publishes no row for a lane is not judged, so a call to it with a missing
weight reaches its door and is refused there (503), which is the behaviour of a forced call today; the typed refusal covers the
nodes that publish rows. (c) `rendered_family` is the family the node's row names for the call, read from the row published a
moment before, not a report from the render. (d) The `capacity` class of the four configuration refusals follows the existing pairing
and is a choice: a caller that reads `defer_class` alone will treat them as retryable, and `err_class` `no_node_configured` with
`data.refusal` is what tells it that waiting does not help. (e) Nothing here was run against a live fleet. The tests use fake nodes
and the real node server with its health rewritten; the live acceptance (read `media_recipes` on every node, name a node that lacks a
weight and see the refusal with no job on it, name a node by id for a music render and verify the file) is the release's.

## Amendment 4 (2026-10-18): every lane needs the fleet bearer

Where this record says the run-graph door is open to any peer, or that a node answers a graph from any caller, it describes the node
when the decision was taken. Since the amendment of 2026-10-15 to
[ADR 0023](0023-agent-lane-tailnet-auth-and-locality.md), every lane needs the fleet bearer on a node that has a token (dispatch of
any task type, the poll and withdraw of any job, and the read of any artifact), and a node with none answers only on loopback. The
bound on the files a graph names (up to six of each kind shown, and no count past six) is unchanged and holds for the same reason as
before: a graph can name thousands of files, whoever sends it. The decision recorded here is otherwise unchanged.
