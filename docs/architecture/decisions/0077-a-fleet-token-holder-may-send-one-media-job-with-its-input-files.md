---
status: Accepted
date: "2026-10-03"
---

# ADR 0077 — A fleet-token holder may send one media job with its input files; a node advertises only the media routes it can run

## Context

A fleet node already runs the five media tasks for another machine: `image-gen`, `video-gen`, `animate`, `audio-gen` and
`run-graph` over `POST /fleet/dispatch`, results from `GET /fleet/jobs/{id}`, files from `GET /fleet/media/{name}`. Nothing
in the repository dispatched them, though: the MCP doors and the CLI verbs called the local pipeline directly, so a machine
with no render lane, or a caller who wanted a particular node's card, could not use the fleet for them.

Three facts stood in the way.

- **The tokenless door reads node-local paths.** The payload builders take a `still`, a `ref`, a `driver` or a `clone`
  sample as a path on the node. [ADR 0023](0023-agent-lane-tailnet-auth-and-locality.md) leaves media dispatch tokenless so
  deployed media clients keep working, and [ADR 0071](0071-a-fleet-token-holder-may-send-a-whole-composition-project-to-render.md)
  showed what a door that writes a caller's bytes to a node's disk needs: a token, an opt-in and confinement. No door
  carried a media job's input files.
- **Advertisement was config-only.** `config.Default()` ships non-empty render scripts, so a box listed `video-gen`,
  `animate` and `run-graph` it had never provisioned: the script path was bound while the weights its graph loads or the
  custom nodes it names were missing, and the first job failed on the node. `internal/mediacap` already derives each route
  from the disk (CONFIGURED, NOT CONFIGURED, BOUND-BUT-MISSING) for `offload_status` and `doctor`, but dispatch and health
  did not read it.
- **A client could not tell a node's routes from its tasks.** `supported_task_types` says "video-gen"; it cannot say the
  Wan graph's VAE is missing.

## Decision

1. **A new door, `POST /fleet/media-job` (task type `media-job`).** The body is `{job_id, task_type, payload, bundle?,
   bundle_sha256?, inputs?}`: `task_type` is one of `image-gen`, `video-gen`, `animate`, `audio-gen`, `run-graph`;
   `payload` is the inner task's payload exactly as `/fleet/dispatch` takes it; `bundle` is base64 of a gzip-tar of the
   input files; `inputs` maps a payload field to a bare file name in the bundle. The fields that may be files are
   `video-gen.still`, `animate.ref`, `animate.driver` and `audio-gen.clone`; `image-gen` and `run-graph` take none.
2. **Closed unless the node opts in, and never open without a token.** The door is advertised and admitted only when
   `fleet_media_inputs` is true, `fleet_auth_token` is set and at least one media task is bound
   (`config.MediaInputsAdmissible`, and in the advertisement at least one inner task this node can run right now). "Bound" is the
   advertisement's own notion: a ComfyUI/python script, the speech endpoint, or the sd.cpp / audio.cpp engine that replaces it
   (`VideoGenBound`, `AnimateGenBound`, `VoiceGenBound`, `MusicGenBound`), so a node whose only renderers are the engines, with
   every script key blank, opens the door too. The door
   and the bearer are checked before any of the body is read; the body is capped at the bundle cap in base64 plus 64 KiB
   (`fleet_media_inputs_max_mb`, default 256 MiB compressed); the decoder refuses unknown fields; a token holder gets a
   15-minute read and write window. `media-job` is token-gated like `compose-project` (`tokenGated`), so a dispatch of it
   over `/fleet/dispatch` needs the bearer too, and its jobs are masked from tokenless polls and feeds. The body is read and
   decoded before the admission gates, so the node holds at most `mediaJobInFlightMax` bodies at once: **one** (the door's own
   bound; the stt upload door's is 2, for bodies of at most 64 MiB). At the 256 MiB default cap one body is about 0.58 GiB in
   memory, the base64 text (341 MiB) beside the decoded bundle (up to 256 MiB), so the door's node-wide peak is slots x 0.58 GiB:
   one slot keeps it under 0.6 GiB beside a ComfyUI render, where two would be 1.17 GiB, and uploads are served one at a time.
   A caller takes the slot (`takeUploadSlot`) after the bearer check and before the first body byte, holds it until the handler
   returns (up to the 15-minute read window on a slow link), and when it waits past 30 s is answered `503` with `Retry-After: 5`.
   The door's only client, `internal/mediaremote`, reads that status as a `capacity` defer but does not read `Retry-After` and
   makes one pass over the node it picked: the caller gets the capacity defer and a later call places the job again. The slot's
   occupancy is not in `/fleet/health` or in the queue depth the node pick ranks on, so a node whose slot a long upload holds can
   still rank first.
3. **The bundle is verified, extracted into a fresh directory and sniffed.** The node checks the declared sha256, extracts
   with `internal/composebundle` (regular files only, confined names, caps counted on the bytes written) into
   `<media_dir>/fleet-inputs/in-*` (a directory, which the media route never serves), requires every `inputs` value to be a
   regular file directly in that directory, and reads its first bytes: an image field takes PNG, JPEG or WebP; a video field
   MP4/MOV (`ftyp`) or WebM/MKV (EBML); an audio field WAV (`RIFF`…`WAVE`), FLAC, MP3 (ID3 or frame sync), OGG or M4A
   (`ftyp`). Anything else is refused with a 400 naming the field. A file field the payload fills with a path of its own
   is refused too: this door carries bytes, never paths. Each input field is rewritten to the extracted absolute path and
   the SAME builder `/fleet/dispatch` uses builds the request. The directory is removed when the job ends and on every
   refusal, and `fleet-serve` sweeps `in-*` directories older than the longest media timeout plus an hour at startup.
4. **A finished media job names its files.** For `image-gen`, `video-gen`, `animate`, `audio-gen`, `run-graph` and
   `media-job`, the job's stored data gains `artifacts: [{name, bytes, sha256}]` for every output the result names
   (`image_path`, `video_path`, `audio_path`, run-graph `outputs`) that is a regular file directly inside `media_dir`.
   A file that cannot be hashed is left out and never fails the job. The field is additive.
5. **Advertisement is honest.** `video-gen`, `animate`, `audio-gen` and `run-graph` are advertised and admitted only while
   the matching mediacap route is CONFIGURED (`generate_video`, `animate_character`, `run_graph`, and for audio any of
   `generate_audio:voice`, `generate_audio:voice:endpoint`, `generate_audio:music`); `image-gen` keeps
   `ImageGenAdvertisable`. One predicate (`taskConfiguredFor`) answers both questions. `/fleet/health` gains
   `media_routes: [{route, engine, state}]` from the same derivation, and `supported_task_types` and
   `loadable_model_families` are derived per request from it, cached at most 60 seconds. A node that predates the field
   publishes none, which a client reads as unknown, never as "no route".
6. **A new client package, `internal/mediaremote`,** mirrors `composeremote`. `Run(ctx, cfg, runner, req, route,
   remotes)` runs `local` here; `auto` here when this machine has the lane (read from the files, as the advertisement is)
   and on a node when it has none and a fleet is configured; `remote` always on a node. The job's input files travel
   through `/fleet/media-job`; a job with none goes through `/fleet/dispatch`. A node is eligible when it lists the task
   (and `media-job` when files travel), reports every route the task needs as CONFIGURED when it reports routes at all, and
   holds no TEXT lease; a node holding no lease ranks first, then the shorter queue, then config order, and every miss is
   named in the defer. Each output is fetched by bare name and verified against the node's published sha256 before
   anything lands: a mismatch deletes what was fetched and defers as infrastructure. A caller's `remotes` must be a subset
   of `delegate_remotes`. The candidates are read through `internal/rosterprobe` like every other single-shot lane
   (ADR 0074): an entry the tailnet guard refuses is a named miss that is never dialled, and the probes run at once through
   the shared memo and negative cache. A call sent to a node is attributed like the other remote lanes (0.165.0, D5-D11): it
   opens a `core.BeginRemote` handle, writes one asker ledger row and, once a node is chosen, one PAIR card, closed on every
   exit of the call, and both POSTs (the dispatch and the media-job) carry `X-Offload-Asker` and, when this machine's emitter
   is off, `X-Offload-Pair-Card: node`.
7. **The five MCP doors and four CLI verbs take the route.** `offload_generate_image`, `offload_generate_video`,
   `offload_animate_character`, `offload_generate_audio` and `offload_run_graph` gain `route` and `remotes`, and
   `generate-image`, `generate-video`, `generate-audio` and `run-graph` gain `--route` and a repeatable `--remote`. In each
   handler the direct pipeline call became the `mediaremote.Run` call and nothing else changed. No tool was added.

8. **A media-job's outputs are served only to the fleet token.** The outputs are rendered from the caller's private input
   files, but `GET /fleet/media/{name}` is tokenless by design for the dispatch lanes, which meant a name was enough to read
   one. The door now sets the render's `out` itself, after the inner builder, to `<media_dir>/mediajob-<16 hex>.<ext>`, and
   `gatedMediaName` recognises that stem, so on a node with a token a tokenless fetch is refused exactly as a gated project
   render or an stt upload transcript is (`media_gate.go`). `mediaremote` already sends the fleet bearer on every output
   fetch, and a test pins it. run-graph, which takes no input file through this door and whose outputs the graph names, and
   every output of the tokenless `/fleet/dispatch` door are unchanged. Like a project render, a media-job render is not swept.

## Consequences

- A thin client, or any machine that names a node, can render an image, a clip, a character animation, a voice or music
  clip or a ComfyUI graph on the fleet with its input files, and receives the output hash-verified. This closes the input
  half of register C-90 for media; transcription (`stt`) stays a separate door.
- **Residual risk, recorded with this decision:** a holder of the fleet token can write a media-sniffed file of up to
  `fleet_media_inputs_max_mb` to a node's disk and have a render read it. The sniff reads magic bytes, not the whole file:
  a file that begins like a PNG can still be malformed, and the decoder that opens it (ComfyUI's image loader, ffmpeg) is the
  boundary for that. The fleet token is held only by the operator's machines.
- Some request fields cannot ride the fleet task and defer by name on a remote route instead of being dropped: `refine=false`
  (image), `tts_voice` (audio), `transformer` (video), a run-graph's `devices` (a card id names a card on the calling
  machine); a run-graph's `out_dir` is never sent (the node writes into its own media dir) but is honoured on the calling
  machine: the fetched outputs are written into it, created if missing. A `waiter_token` is not carried either, by design:
  it resumes a place in line on the calling machine, and a call that goes to a node leaves that place.
- A node whose weights go missing stops advertising the task within a minute and says why in `media_routes`; a client that
  reads `media_routes` skips it and names it in the defer. A default config on a thin client no longer reads as having a
  lane, so `route: auto` goes to a node there. A box with no lane and no `delegate_remotes` still runs the call locally and
  gets the pipeline's own deferral, as before.
- The door is per node and off by default; enabling it is a deliberate configuration change on each render node.

## Alternatives considered

- **Raise the tokenless dispatch cap and accept files there.** The tokenless door would then write a caller's bytes to every
  node's disk without a token. Rejected: ADR 0071's rule is that the door that writes needs the token.
- **Reuse `/fleet/compose-project` for media.** Its confinement checks read HyperFrames markup, which a media file has not,
  and it renders a composition. A separate door keeps each door's checks about what it carries.
- **Send the files as separate uploads and reference them by id.** Several round trips and a store with its own lifecycle,
  for jobs that carry one to four files. A single bundle reuses the extractor, the caps and the sweep that exist.
- **Keep the config-only advertisement and let the client retry.** A job sent to a node that cannot run it costs a queue
  place and a failed render before the client learns; the node already knows.
