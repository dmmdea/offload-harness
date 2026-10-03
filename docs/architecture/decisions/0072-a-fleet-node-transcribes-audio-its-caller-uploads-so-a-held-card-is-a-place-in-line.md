---
status: Accepted
date: "2026-10-03"
---

# ADR 0072 — A fleet node transcribes audio its caller uploads, so a transcription behind a held card goes to an idle node

## Context

[ADR 0040](0040-vision-work-travels-to-a-node-with-an-idle-card.md) let an image travel to a node whose card is idle;
transcription had no such door. On 2026-10-03 a box held its cards for a render for hours and every
`offload_transcribe` on it waited the full 90 s gate and came back `gpu_busy` (five calls, about 90 s each), while three
fleet nodes advertised the `stt` task and their ledgers show no stt or vision work in that window. Four facts
kept the work from leaving the box:

- The fleet `stt` task takes a path on the NODE's disk (`audio`), and `POST /fleet/dispatch` caps its body at 1 MiB.
  Nothing could upload audio to a node: a 16 kHz mono WAV is 1.9 MB a minute, past the cap after 33 seconds.
- Nothing in this repository asked for an `stt` job. `offload_transcribe` and `local-offload transcribe` had no `route`.
- A node's `stt` task turned a defer into job state `error` with only the reason, so `defer_class` and `err_class` did
  not survive the wire the way they do on the vision and text lanes.
- It was tokenless and path-taking (a tailnet peer could make the node convert and transcribe any file it could read),
  and it was not concurrency-capped: 5 of 18 jobs in one burst on one node failed `whisper-server 500 … matrix: model
  unloaded`.

The mechanism to copy exists (the vision lane, ADR 0040) and so does the attribution the remote lanes share (one PAIR
card per remote call on the serving node, one asker ledger row; `pair-workloads.md`).

## Decision

1. **A new door, `POST /fleet/stt` (task type `stt-upload`).** The body is JSON: `job_id` (`stt-<hex>`, minted by the
   caller), `audio_b64` (the audio file, base64), `audio_ext`, and optionally `language` and `hq`; unknown fields are a
   `400`. The body is capped from the node's `fleet_stt_upload_max_mb` (decoded MiB; default 48, which is 64 MiB on the
   wire and about 4.6 hours of the 32 kbps Opus an asker sends), and a decoded file over the cap is a `400` naming the
   key. The legacy path-taking `stt` task is unchanged.
2. **The node runs its own pipeline over a private copy and returns the whole result.** The bytes are written to a
   private file under `<media_dir>/.stt-upload/` (mode 0600; a dot directory, which `GET /fleet/media` never serves),
   `TaskTranscribe` runs with `Door = fleet`, the fleet job id and the asker as the requester, and the job's data is the
   FULL `core.Result`, so a defer is a `done` job whose data says `deferred: true` with its `defer_class` and `err_class`,
   as on the vision lane. The file is removed when the job ends and on every refusal or drop, and fleet-serve sweeps
   orphans at startup. The node's `.srt`, `.txt` and `.segments.json` stay fetchable by bare name through
   `GET /fleet/media/{name}`, as for every transcription. As first shipped those outputs persisted in `media_dir` with no
   retention and `GET /fleet/media` served them without the bearer, protected only by the stem (`stt-` plus the random part of
   an `os.CreateTemp` name, a 32-bit number, plus 8 hex of a content hash), which is hard to stumble on but not a secret.
   > **Correction (2026-10-03, the PAIR card relay change):** both gaps are closed. A node removes an upload job's transcript
   > files when the job record is evicted or after `fleet_stt_transcript_ttl_min` (default 30 minutes, swept at start and on the
   > janitor tick), and on a node that has a `fleet_auth_token` `GET /fleet/media` serves those names (and a project render's,
   > ADR 0071) only to a bearer holder. Media names of the tokenless lanes stay as they were. The decision above is unchanged.
3. **The door is token-gated, and advertised only when it admits.** It rides the vision lane's rule (`tokenGated`: a
   `fleet_auth_token` for anything beyond loopback; loopback with no token stays open), the bearer is checked before a
   byte of the body is read, and a token holder gets a 10-minute delivery window (the server's blanket 30 seconds would
   cut 64 MiB on an ordinary link). Because the body is read before the admission gates (a known job id must still
   re-ack, so the id is read first), at most two uploads are in flight on a node (`sttUploadInFlightMax`); a third waits
   for a slot, up to 30 seconds, then gets a re-placeable `503` with `Retry-After`. The audio is never copied by the
   decode: it is parsed in place and decoded once, straight into the buffer that becomes the private file. Health lists `stt-upload` in `supported_task_types` and publishes `stt_hq` (the node
   has an `stt_model_hq`) and `stt_upload_max_mb` exactly when `STTUploadAdmissible` holds (a bound `stt_model` and the
   reachability rule). An asker keys on these, never on `stt` (every node with a whisper model lists that), so a node
   that predates the door is never sent an upload. An `hq` upload to a node with no hq model is a `400`, never a silent
   run on the standard model.
4. **The legacy `stt` lane joins the bearer rule when the node has a token** (D17). It reads an arbitrary path on the
   node's disk, which is the shape the other lanes were gated to avoid. A node with no `fleet_auth_token` keeps its
   legacy lane open, so no deployed tokenless node starts refusing.
5. **One concurrency cap covers both stt lanes** (D18): `fleet_stt_max_concurrent`, default 1. Jobs over it wait in
   arrival order inside their run closure, before their card turns running, and never fail; a waiter whose context ends
   (the node is shutting down) leaves the line. The pulled (claim-loop) path takes the same gate. While jobs wait,
   `sttclient.Queued` keeps the model loaded for them, so a burst pays one cold start, not one per job. Whisper is a
   single-slot upstream and a process-wide mutex already serialized inference inside one process (register C-91); the cap
   is the node's own, configurable statement of that, and it is what gives the waiting an order.
6. **The asker is `internal/sttremote`, with the vision lane's routes.** `local` is byte-identical to before (a
   `gpu_busy` defer gains a hint, when `delegate_remotes` is configured, that `auto` or `remote` would let a fleet node
   take the work); `auto` runs local unless the local whisper request would actually be held right now; `remote` never
   touches the local card and defers `capacity` (or `config` with no remotes) when it cannot place. The auto trigger is
   `modelaffinity.WouldBlockUpstream`, the upstream fence's own first inspection exported without its wait: a lease that
   fences the cards over the model AND the model not already resident. A render that holds other cards, or a whisper
   that is already loaded, is served here at once, so a spill is never wasted.
7. **The audio is read on the asker and shipped small.** It is converted to 16 kHz mono Opus at 32 kbps (about 14 MB per
   hour; the original is sent when conversion is impossible and it fits the node's published cap). A node is eligible
   when it advertises the door, has an hq model when `hq` is asked, takes a file this size, and its card is not leased,
   ranked like `PlaceVision`. The wait for the answer is sized from the audio and bounded by `stt_request_timeout_sec`.
   The request carries the bearer and the attribution headers (`X-Offload-Asker`, and `X-Offload-Pair-Card: node` only
   when this box will not card the job), the call opens one PAIR card on the serving node and writes one asker ledger
   row through `core.BeginRemote`.
8. **The asker writes its own outputs.** On success it writes `.srt`, `.txt` and `.segments.json` under its own
   `media_dir` from the full segment list (fetching the node's `.segments.json` through `/fleet/media` when the inline
   copy was cut), so the result's paths are local files and none names a path on the node. The node's answer is checked
   first (at least one segment, no segment ending before it starts, starts never running backwards, a fetched list
   agreeing with the result's own count); a failure is a deferred result naming the node.
9. **`offload_transcribe` gains `route`, default `auto`; the CLI gains `--route`.** The default is auto because every
   fleet node serves the same whisper family, so a spill costs no quality, while a transcription waiting behind a render
   is the failure this removes; a caller wanting the old behaviour passes `local`. `engine:"npu"` is this box's own Hailo
   sidecar and never travels: a route the caller names there other than `local` is refused, as for OCR. The CLI
   `transcribe` defaults to `local` (a script that never passes `--route` is byte-identical, as for the vision verbs).
   The CLI `classify` and `extract` gain `--route` through `textremote`, like their MCP twins; `summarize` and `triage`
   refuse a route.

## Consequences

- A box with its cards held no longer refuses transcription while a node is idle; the cost is the upload (about 14 MB per
  hour of audio) and Opus instead of WAV on the node's side of the conversion. The transcript can differ slightly from a
  local run's for that reason, and only on a spilled call.
- **A node with a `fleet_auth_token` now refuses a tokenless legacy `stt` dispatch (401) and masks its polls.** The only
  external dispatcher known to send `stt` jobs (a separate service on `<node-c>`, the Linux edge node, port 18810) sends no
  bearer token. No production use of `stt` through it was found: its web console form sends an empty payload, and the
  jobs seen on one node on 2026-10-01 were a burst of test ids. Such a dispatcher keeps working against a node with no
  token and needs the bearer against one that has it. > **Unverified:** where that dispatcher mints its job ids was not
  traced.
- A spilled call is visible: one card on the node that served it ("Requested from" the asker), one ledger row with
  `route`, `placement`, `node` and `fleet_job_id`, and `meta.node` / `meta.placement` on the result. `meta.route` is
  deliberately not carried: `core.Meta` is the wire and ledger shape every lane shares (vision, text and compose stamp
  node and placement only), the placement reason already says which way the route went (`remote: forced`, `remote:
  local gpu busy`, `local: gpu idle`), and the route the caller asked for is on the asker's ledger row.
- A job that waits at the stt gate reads `running` to a poller (it is inside its run closure) while its card is still
  queued on the node; the asker's card therefore turns running when the job is admitted, not when whisper starts.
- `GET /fleet/media` now refuses any name that starts with a dot.
- The MCP `tools/list` changes on every box (the `route` property and the description).
- A node that serves the door holds, per upload in flight, the request body (up to 64 MiB) plus the decoded audio (up to
  48 MiB) while it admits a job, and at most two uploads are in flight at once, so the peak is about 224 MiB. A job
  waiting at the stt gate holds neither: the body is released at admission and the audio is on disk.
- `stt-upload` is exempt from `fleet_max_concurrent_jobs`, as the legacy `stt` is: it waits behind its own
  `fleet_stt_max_concurrent` gate, and an upload parked there would otherwise hold a text execution slot while doing no
  work (`TestConcurrencyCappedRule`, `TestSTTUploadsWaitingAtTheGateDoNotHoldAFleetSlot`).
- **The transcript of a gated upload stayed on the node, readable by name without the bearer.** `.srt`, `.txt` and
  `.segments.json` are written to `media_dir` (mode 0644). *Closed by the correction under decision 2:* the outputs are
  removed after their retention and served only to a bearer holder on a node with a token. The pipeline's cache is keyed on
  the audio's content, so a hit can name an earlier job's files: a finished job restarts its outputs' clock, and a cache hit
  whose files are gone is treated as a miss and redone.

## Alternatives considered

- **Raise `/fleet/dispatch`'s body cap and add an `audio_b64` field to the `stt` task.** That cap protects every media
  envelope and agent contract at 1 MiB, the door would stay tokenless, and the legacy task would keep returning a defer as
  an `error` job. The vision lane made the same call for the same reason.
- **Spill whenever the machine-wide lease is held (the vision trigger).** Simple, but the actual refusal is the upstream
  fence, which lets a resident whisper through; a lease on other cards would send audio off the box for nothing. The
  exported probe reads what the request will meet.
- **Send the 16 kHz WAV.** No quality difference and no conversion on the asker, but 115 MB an hour against 14 MB.
- **Default `local` for the MCP tool, as the vision tools do.** The failure that prompted this is a caller that never
  passed a route; with the same-family argument there is no quality reason to make every caller opt in.
