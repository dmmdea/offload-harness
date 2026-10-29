# Changelog archive: 0.140.7 to 0.113.21

Verbatim entries moved out of the root `CHANGELOG.md` so the file GitHub renders stays under its size limit.
History, never edited. Relative links inside the entries are written from the repository root.
Newest first; the live changelog is [`../../CHANGELOG.md`](../../CHANGELOG.md).

## [0.140.7] - 2026-09-24 - the music QA gate stopped silently skipping itself fleet-wide, ComfyUI's own console output is captured for failed renders, and doctor catches a model file still mid-copy

Three media-lane defects found during the 2026-09-23/24 `<node-c>`/`<node-a>`/`<node-e>` media-route
remediation (the operator's per-node route records, 2026-09-23).

### Fixed

- **The music route's over-render/trim/dead-air QA gate (PR #466) was silently skipped on every
  node that never set an explicit `ffmpeg_path`.** `config.Default()`'s `FFmpegPath` is the bare
  name `"ffmpeg"`, so `Pipeline.genEnv()`'s `!= ""` check was always true and every render child
  got `FFMPEG_PATH=ffmpeg` — a bare name, not a path. `render/audio-qa.mjs`'s `resolveFfmpeg()`
  treated a set `FFMPEG_PATH` as an exact file via `existsSync()`, which cannot see PATH
  resolution, so it read `"ffmpeg"` as missing and the whole gate degraded to a silent skip,
  shipping the raw, unverified render with its known ACE-Step dead-air tail (reproduced
  identically on `<node-c>` and `<node-a>`). `genEnv()` now resolves `ffmpeg_path` through PATH
  (`internal/mediaops.ResolveBinary`, shared with `mediacap.binaryPresent` and
  `internal/mediaops.RunMedia`, which had the identical bug) and threads the resolved absolute
  path; `resolveFfmpeg()` also now tries a set-but-bare `FFMPEG_PATH` as a PATH-searchable command.
  When ffmpeg/ffprobe genuinely cannot be found anywhere, the music lane now fails loudly with a
  typed `FFMPEG_UNAVAILABLE` error (`gpugen.ClassifyErr` maps it, mirroring `DEAD_AIR`) instead of
  silently shipping an unverified file. `doctor` gained a `media:ffprobe` route (it previously
  checked only `ffmpeg`, so a box with ffmpeg but no ffprobe stayed green while `offload_media`
  and the music gate deferred at call time).
- **ComfyUI's own stdout/stderr were discarded** (`stdio: "ignore"`), so a ComfyUI-side failure
  (e.g. `[Errno 28] No space left on device` on `<node-a>`) surfaced only as a terse
  `/history execution_error` JSON — finding the real cause took a hand-built stdout-capturing
  bypass copy of `render/`. `render/comfy-lifecycle.mjs` now captures a harness-launched ComfyUI's
  console to a rotating, 5 MB-bounded log file beside the install (`offload-comfyui.log`, up to 3
  previous runs kept), and `withGpuSlot` (`render/gpu-lock.mjs`) appends its last ~20 lines to a
  render failure's error message — never for a reused foreign instance.
- **`doctor`'s model-binding check was `os.Stat`-only**, so it reported a route `OK CONFIGURED`
  the instant a same-named file of any size existed — reproduced live on `<node-b>`: a 16.65 GB
  WAN-Animate-2 unet read as configured at ~70% copied. `internal/mediacap/resolveBinding` now
  compares the file's size against `knownModelSizes` (mirroring `setup/install.ps1`'s `$PINNED`
  models, cross-checked by `TestKnownModelSizesMatchInstaller`) whenever the configured name is
  one the harness's own installer pins, and reports `INCOMPLETE (have X of Y bytes)` on a
  mismatch. Never hashes a file — cheap by construction. A name the installer does not pin (the
  overwhelming majority of ComfyUI weights, sourced ad hoc between fleet boxes) is unaffected.

## [0.140.6] - 2026-09-23 - every node reports its own work to PAIR, lease jobs get a card, and a media card reads "running" only once it holds the GPU

### Fixed — PAIR showed only the workstation's delegations while the other nodes ran at 84-100 %

Operator report 2026-09-23 ("work is not being properly shown as routed"): every open card read
"Running on `<node-b>`" while `<node-a>` ran a seat bench and a ComfyUI diagnostic, `<node-c>` a Wan 2.2
smoke render and `<node-f>` a tuning sweep. None of it was delegation, and none of it reached PAIR.

- **Jobs under `gpu reserve -- <cmd>` get a card** (`gpu_leasecard.go`). The lease queue is where
  the house runs every bench, render and measurement on every node. The wrapper now owns one card:
  `queued` while it waits in the queue and drains the seat, `running` once the command starts,
  closed with the exit (`exit status N`, `(interrupted)`, `(lease lost)`). The model is the
  harness verb (`generate-video`) or the script (`seatbench.ps1`) or the program; the requester is
  `--origin`. A one-shot harness verb run directly under the lease gets
  `OFFLOAD_PAIR_UNDER_LEASE=1` and adds no second card; a session (`mcp`, `fleet-serve`), shell or
  interpreter under a lease keeps its own reporting (review finding: the flag would otherwise
  silence a wrapped MCP session for its whole life).
- **`pair_workloads_enabled` is safe on every box.** Work a fleet node serves for another box
  (the `fleet` door) is now skipped by `Begin` and by the ledger observer, as `agent` rows always
  were, so enabling reporting on a fleet node no longer shows a delegation twice, and the node's
  OWN work (a CLI render started over ssh, a lease job) reaches PAIR. Enabled on `<node-c>` and
  `<node-f>` in the same deploy (backups `*.pre-pair-workloads`).
- **A media call's card opens `queued` and turns `running` when the lane holds the GPU**
  (`core.MarkWorking`, fired by `acquireMediaLease` and the compose slot). 0.140.5 opened it
  `running`, so a call waiting behind another job read "Running" through the wait; a transcription
  waiting minutes for whisper behind the 3-card seat did exactly that. `transcribe` cannot see its
  engine start (the wait is a load inside llama-swap), so its card stays queued until it ends.
- Not covered: work started outside the harness altogether (`<node-f>`'s root tuning scripts). Run it
  under `gpu reserve -- <cmd>` and it gets a card.

## [0.140.5] - 2026-09-23 - PAIR's Jobs list shows long media calls while they run, and a delegation reads "running" only once its seat works

### Fixed — PAIR cards that said nothing, or said "Running" over an idle card

Three gaps between what the fleet was doing and what NVIDIA PAIR's Jobs list showed, found
against live cards on 2026-09-23 (docs/systems/pair-workloads.md).

- **A long tool call now has a card while it runs.** A tool call reached PAIR only through its
  ledger row, written when the call ENDS, so a ten-minute `animate_character` render on
  `<node-e>` held its GPU at 100 % with no card at all. `Pipeline.Run` now opens a `running` card
  when a media call starts (`pairworkloads.Emitter.Begin`, `internal/pairworkloads/calls.go`) and
  the call's own ledger row closes THAT card: same id, engine and start, the row's model and
  outcome. A call that writes no row (a cache hit) is closed by the return of `Run`, and a call that
  panics closes it failed (`closeCall`) rather than as the zero result's "completed". Only tasks
  whose engine the task alone decides open one (`comfyui`, `hyperframes`, `whispercpp` lanes):
  PAIR keys a card on its engine, and a text or vision call learns llamacpp vs vllm only as it
  runs, so those short calls keep the single terminal card.
- **A delegation reads "running" only once the seat is working on it.** The running frame went
  out at the node's ack (remote) or the hand-off to the runner (local), so `<node-b>`'s agent-pool
  and the `<node-c>` 27B showed "Running" through a 2-minute seat load while their cards sat at
  0 %. The card now stays `queued` through admission and cold load and turns `running` on the
  first sign the seat is serving the request (`seatWorking`, `internal/delegate/pairevents.go`):
  a streamed token, a decode, a tool call, a re-pack, or a prefill older than 8 s (the seat
  probe names a load within 5 s). Remote runs read it from the poll's `progress`, local runs from
  their own progress reports; a run that reports no progress at all turns running after 30 s. A
  run that never worked closes with no `startedAt`.
- The third gap was configuration, not code: `<node-e>`'s config lacked
  `pair_workloads_enabled`, so none of its own calls ever reached PAIR. Enabled there
  (backup `config.json.pre-pair-workloads`); it serves no delegations, so no job shows twice.

## [0.140.4] - 2026-09-23 - four media-lane defects from the compact-desktop remediation: animate_character's silent no-op, a crop at the origin, sdcpp's iGPU pin, an untyped audio timeout

Four defects surfaced by `<node-e>` media-lane remediation (2026-09-23), each reproduced from the
code path (not just observed live) and covered by a test broken-then-restored at its real call site.

- **`animate_character` returned the driving video unmodified, on every box, every time — a harness
  defect, not a box fault.** `render/comfy-output.mjs`'s `firstOutputFile()` scanned a ComfyUI `/history`
  outputs object with a plain `for (const node of Object.values(outputs))`, and JS objects enumerate
  integer-like keys in ASCENDING NUMERIC order regardless of insertion order. The native `LoadVideo` node
  echoes a UI preview of its own input into its `outputs` entry — the same shape as a real result — and
  in `wf-wan-animate2.mjs` that loader's id ("240") sorts before the real `SaveVideo` node's id ("246"),
  so the loader's echoed preview was always picked first: full 806s render, exit 0, "WROTE `<out>`"
  printed, and the file on disk byte-identical to the driver video (measured on `<node-e>`; confirmed
  by sha256 + frame inspection). `firstOutputFile` now takes an optional second argument, the API-format
  graph the caller already holds, and skips any node whose graph `class_type` starts with `Load` — a
  loader never legitimately produces the result, whatever kind of file it echoes. Every caller
  (`comfy-animate.mjs`, `comfy-video.mjs`, `comfy-edit.mjs`, `comfy-inpaint.mjs`, `comfy-music.mjs`,
  `comfy-upscale.mjs`) now passes its graph. Audited every other `wf-*.mjs` builder for the same shape:
  only WAN-Animate-2's graph uses a `LoadVideo` node; every other video lane uses `LoadImage`, which does
  not register a preview entry in ComfyUI's execution outputs.
- **`edit_image` crop at `x:0` or `y:0` failed with `pipeline failed: 'x'`.** `internal/mediaops/editimage.go`
  tagged `EditOp.X`/`Y` `json:"x,omitempty"`/`"y,omitempty"`, so an explicit zero coordinate was dropped
  from the JSON entirely, and `render/edit_image.py`'s `int(op["x"])` then raised `KeyError` — any
  crop/composite/text op anchored at the image origin failed outright. Both sides fixed: the Go tags drop
  `omitempty` on X/Y (Width/Height keep it — 0 is never valid there), and the Python side now defaults
  safely (`op.get("x") or 0`) as defense in depth, matching the pattern the composite/text ops already used.
- **sdcpp (stable-diffusion.cpp / Vulkan) pinned device 0 whenever `GGML_VK_VISIBLE_DEVICES` was unset,
  which is the integrated GPU on any box with one enabled.** Measured on `<node-e>`: Vulkan0 = Intel UHD
  630, Vulkan1 = the RTX 5060; every Z-Image render silently ran on the iGPU at 565-608s/step (the same
  recipe runs at 4.85s/step on the RTX). `render/sdcpp-generate.mjs` now parses `<bin> --list-devices`
  and auto-picks the first discrete (NVIDIA/AMD) adapter when the env is unset, falling back to device 0
  only when none is found or the probe fails; an explicit env override still always wins. `doctor` now
  reports which Vulkan device an sdcpp render will actually use (`internal/mediacap/sdcppdevice.go`,
  doctor-only — it makes a live subprocess call, so it stays out of `mediacap.Routes` proper) and prints a
  `WARN` line when that device is an integrated GPU.
- **A music/audio timeout was reported as a generic `exit status 1`, not the typed timeout other lanes
  rely on.** `internal/gpugen.Generate` kills the whole process tree on a context timeout via
  `taskkill /T /F` on Windows, which terminates through `TerminateProcess` — reporting exit code 1 with
  none of `ClassifyErr`'s recognized substrings (`timeout`/`deadline`/`killed`/`signal:`). `Generate` now
  checks its OWN derived context's `DeadlineExceeded` directly (authoritative regardless of the child's
  exit code) and folds "timeout"/"deadline exceeded" into the returned error text, so `ClassifyErr(gerr)
  == "timeout"` is reliable for EVERY `gpugen`-based lane (audio, video, image), not just the one that
  happened to reproduce it. `TestGenerateTimeoutKillsTree`'s classification assertion, previously a
  `t.Logf` "best-effort" note because of exactly this flakiness, is now a hard `t.Fatalf`.
  Also (a narrower, related finding from the same report): a music request whose dead-air retry still
  fails could leave the retry's raw ComfyUI bytes (SaveAudio always emits FLAC) under the caller's
  requested output path — a mismatched container under whatever extension was asked for ("FLAC bytes
  in a .wav name", 2026-09-23) whenever the over-render trim step didn't run (a `--graph` passthrough)
  or its own ffmpeg call failed; when the trim step DOES run and succeed it already re-mixes to match
  the requested extension, so the file left behind is not always literally raw FLAC — either way it is
  a render that failed content QA and should never be left at the caller's path.
  `render/comfy-music.mjs`'s dead-air-persists branch now removes that stray file unconditionally
  (best-effort; `cleanupFailedDeadAirOutput`) before throwing the typed `DEAD_AIR` error. The "FLAC
  bytes under a .wav name" symptom on a *successful* render did NOT reproduce on 0.140.3 (confirmed: a
  real RIFF/pcm_s16le file) — not fixed, since it could not be reproduced from the code path on that
  branch.
- Tests: `internal/mediaops/editimage_test.go` (`TestCropOriginZeroRoundTrip`), `internal/gpugen/gpugen_test.go`
  (hardened `TestGenerateTimeoutKillsTree`), `internal/mediacap/sdcppdevice_test.go` (new),
  `render/comfy-output.test.mjs`, `render/sdcpp-generate.test.mjs`, `render/comfy-music.test.mjs`.

## [0.140.3] - 2026-09-23 - a short music render dodges its own dead air by rendering longer than asked, then trimming back

### Fixed — the ACE-Step dead-air gate (0.139.3) was defense-in-depth for a defect the render could avoid outright

Measured on an 8GB box, ComfyUI 95539f56, ACE-Step 1.5 XL turbo, the harness's own `render/wf-acestep.mjs`
graph, prompt "upbeat latin pop instrumental, acoustic guitar, congas, bright brass, 110 bpm":
`generate_audio kind=music` was hitting the 0.139.3 DEAD_AIR gate on essentially every short instrumental
request, even after its one re-seeded retry.

- **Root cause, refined from the 0.139.3 writeup:** with `generate_audio_codes` true,
  `comfy/text_encoders/ace15.py`'s ACE-Step LM always emits exactly `duration*5` audio codes (min=max) and
  reliably plans the song to END 2-6s BEFORE the requested duration, filling the remainder with silence
  codes — a hard cut to about -65 dBFS, not a fade. Per-second RMS: 30s requested, seed 7 -> music to
  27.99s then 2.01s silence; seed 759155896809805 -> music to 24.89s then 5.11s silence (17% silent). At
  36s requested (same prompt) all three seeds tried put music across the WHOLE requested span (seed 7 to
  31.11s; seed 759155896809805 to 30.24s; seed 11 to ~34s then a natural fade) — asking for more than
  needed avoids the defect outright. `generate_audio_codes=false` has no planned ending and is 5-8x
  faster, but drops the LM the model card names as the quality path and pumped one seed's level 15dB
  second-to-second — not a substitute.
- **`render/comfy-music.mjs` now over-renders and trims.** When the graph is built from args (not a
  verbatim `--graph` passthrough) and ffmpeg/ffprobe resolve, `generate()` asks `buildAceStep` for
  `renderSeconds = seconds + max(6, ceil(0.2*seconds))` (`computeRenderSeconds`) instead of the requested
  seconds, then trims the produced file back to exactly what was asked for with a 1.0s fade-out on the cut
  (`audio-qa.mjs`'s new `trimToSeconds`, same FLAC container) — before the 0.139.3 dead-air gate ever
  measures the file. The re-seeded retry trims the same way. ffmpeg unavailable, or a caller-supplied
  `--graph` (its duration is opaque to `buildGraphFromArgs`), renders the requested seconds exactly, as
  before this fix — the gate itself, the retry, and loudness normalization are unchanged.
- Tests: `render/comfy-music.test.mjs` covers `computeRenderSeconds` against the measured 30s/36s example
  and the +6s-floor-vs-20% crossover, the over-length graph build, the default-false (unchanged) path, and
  the `--graph` passthrough ignoring `trim`. `render/audio-qa.test.mjs` adds fixture coverage for
  `trimToSeconds` (real ffmpeg: cuts to the exact requested length; the fade-out itself never reads as dead
  air) plus its invalid-input handling.

## [0.140.2] - 2026-09-23 - a tool call cut at the cap is a cut, whatever finish reason vLLM reports; a tool refuses arguments of the wrong type

### Fixed — a tool call with a wrong-typed argument ran with that field empty (data loss)

- **Every agent tool decoded its arguments with `_ = json.Unmarshal(...)`.** A syntax error leaves the struct
  zero and the required-field checks caught it, but a TYPE error does not: `encoding/json` fills every field it
  can and reports the first mismatch. `edit_file` with `{"path":"x","old_string":"foo","new_string":123}` ran with
  `new_string` empty, deleted `foo` from the file and reported success; `write_file` with a non-string `content`
  wrote an empty file. Every tool that decoded this way (`write_file`, `edit_file`, `delete_file`, `read_file`,
  `list_dir`, `summarize_file`, `search_files`, `update_plan`, `run`, `run_shell`, `web_fetch`, `web_search`,
  `github_api`, `github_create_repo`, `github_upload_file`) now goes through one decoder that refuses the call on
  ANY decode error, as `NOT performed: <tool> arguments do not match the tool's schema (…)`, before any policy,
  filesystem or network step. Empty arguments are still a zero-valued call (a no-argument call on some engines).
- **A wrong type in a NON-string field is coerced once, never refused outright.** On a type error only, one pass
  driven by the target struct's field types rewrites a JSON string sent for a number (`"offset":"5"`), a bool
  (`"private":"true"`, case-insensitive) or a list of strings (`"args":"./..."` becomes `["./..."]`), then
  decodes strictly again. Small models send those shapes, and they used to work with the field silently dropped
  (`"private":"true"` was read as `false`); refusing them would spend the seat's same-tool retries. A string
  field is never coerced: a number, object, array or bool where a string is declared (`new_string`, `content`, a
  github `body`, a path) is still refused, because coercing it could change what is written. A string that is not
  a number or bool literal, and `"null"`, are refused too. Unknown fields stay ignored.
  The offload and accelerator tools already checked the error and are unchanged.

### Fixed — every "cut by the budget" test trusted `finish_reason: length` alone

- The same engine behaviour reached four more places: the truncated-final re-issue, `OutputTruncated`, the
  starvation classifier (`Completion.Starvation`, which now takes the call's budget) and the client's
  reasoning fallback (a think block cut at the cap could be promoted to the answer when the engine said `stop`).
  One helper, `cutByBudget` (finish `length`, or the server's `completion_tokens` reached the call's
  `max_tokens`), now decides at all of them and in the tool-call cut check. A stop note built on the token count
  says so (`finish stop at the 1024-token cap (1024 completion tokens)`).
- The client proves a returned tool call's arguments once, when the call arrives, and does not re-scan it on
  every later request while its arguments are unchanged; a caller-built or rewritten call is validated on
  every request, so invalid arguments still never reach the engine.

### Fixed — vLLM reports a cut tool call as `tool_calls`, and the fragment went into the transcript

- **A tool call cut at the completion cap arrived with `finish_reason: "tool_calls"`, not `length`.** vLLM
  rewrites the finish reason to `tool_calls` whenever a tool call was streamed, cap or no cap. Measured on a
  warm vLLM seat (harness 0.140.x): a streamed call hit the 8,192-token cap in the middle of an
  `offload_triage` argument, `cutToolCallInCompletion` checked only for `length`, the unterminated argument
  was appended to the transcript, and the next request failed with HTTP 400 `Unterminated string` from the
  engine's own parse of the argument it was handed back. The loop recovered on the following step, but the
  step was wasted and the transcript carried invalid tool-call JSON.
- **Fixed: the cut is read from the arguments, not the finish reason alone.** A non-empty argument that is
  not valid JSON is a cut when the finish reason is `length`, when the server's `completion_tokens` reached
  the step's `max_tokens`, or when the JSON ends mid-value (an open string, object or array: only a cut
  leaves that). A cut takes the existing path: the turn is dropped, the step is re-issued once at the final
  budget, and a second cut stops on `tool_call_cut`. A complete but malformed argument below the cap is the
  seat's own mistake, not the budget: it is dispatched as before and the tool reports the error.
- **The client never sends invalid tool-call arguments to the engine.** Every assistant tool call the request
  carries goes out with valid JSON arguments: arguments that do not parse go out as `{}` (the tool's error
  result follows in the transcript), while valid and empty arguments go out byte for byte. That covers the
  paths the loop's cut check does not: a malformed call below the cap, a replayed or caller-supplied
  transcript. The caller's transcript is not modified.
- Legitimate tool calls are unchanged: valid arguments with `tool_calls` at the cap run as before (tested).

## [0.140.1] - 2026-09-23 - a tool call the vLLM parser is still holding is progress, not a stall

### Fixed — the liveness watch read a vLLM seat writing a held tool-call argument as silence

- **A vLLM seat that is writing some tool calls sends no SSE frame at all until the call closes.**
  vLLM's tool parser (the unified parser engine that serves `--tool-call-parser qwen3_xml` on 0.28)
  streams a string argument token by token, but holds a trailing non-string argument (an object, an
  array, a number) until the next parameter or the closing tag: `_safe_arg_prefix` drops the trailing
  value unless the schema types it as a string. It holds a call to a name the request did not offer
  for the call's whole length. While it holds, `serving.py` sends nothing (`delta_message is None` →
  `continue`). Measured on the reference workstation's 3-card seat (`agent-pool`, thinking off, the
  harness's own request shape): an `offload_extract` call with a 40-property `schema` object sent 12
  frames, and between the `"schema": ` frame at 1.9 s and the whole object at 64.7 s there were
  **62.8 s with no frame**, while the engine generated 1,460 tokens. The liveness floor is 60 s, so
  the watch files that as `stalled: no progress for 60s in …` — an infrastructure defer of a seat
  that was producing at ~23 tok/s.
- **Fixed: a vLLM seat is asked for its per-token signal, and the decoder counts it.** On a seat this
  box declares in `vllm_seats` (alias resolved through the roster, the same gate the structured
  re-pack uses), the agent loop's streamed calls carry `"return_token_ids": true`. vLLM then sends a
  frame per engine step with the generated ids even when the parser emits no delta (the same call:
  1,462 frames, largest gap under 0.05 s). `decodeSSE` counts a frame's `token_ids` as progress, by
  the number of ids, whether or not the frame carries a delta. A tool-call frame that carries only
  the tool's id and name (vLLM sends the name in a frame of its own) now counts as progress too.
  Every other seat, and every call with no progress listener (the CLI doors, the probes), sends the
  request it always sent, byte for byte: other OpenAI-compatible servers may reject an unknown key.
- Side effect, a correction: the loop's prefill measurement (`FirstDeltaMS`, the seat-rates store's
  `prefill_tok_s`) is the time to the first frame the decoder counts. A call that opened with a held
  argument measured the hold as prefill; it now measures the first generated token.
- Cost: with `return_token_ids` vLLM also puts the prompt's ids in the first frame of each call,
  about 4.5 bytes per prompt token (measured: 78 KB for the 17.5k-token review prompt; ~1.2 MB at the
  262k window), on loopback, once per step.
- Live E2E on the 3-card seat (the same 44.8 KB `offload_review_diff`, warm seat, this build): 11
  steps, `done`, 10 findings in 293 s, every loop call carrying `return_token_ids`. Even ordinary
  `read_file` calls are partly held: the re-issue's call sent 63 frames for 61 tokens, 19 of them with
  a visible delta, so the largest gap between frames the watch counts fell from 0.5 s to 0.05 s.
- Tests: `internal/agent/client_heldtool_test.go` (the decoder on the captured wire shape; a fake
  vLLM seat that sends per-step frames only when asked, run under a real `Monitor`: completes with
  the signal, files the stall without it), and
  `internal/pipeline/agenttask_heldtool_test.go` (the real call site: a declared vLLM seat is asked
  and survives a hold longer than the allowance; the same seat undeclared gets the historical body
  and stalls). Red before the fix; mutating either the call site (`StreamTokenIDs: false`) or the
  decoder's id count fails them.

## [0.140.0] - 2026-09-23 - a seat that is LOADING is not a prefill stall

### Fixed — the liveness wall filed a normal cold load as a prefill stall

- **A seat reloading mid-run deferred the contract as `stalled: no progress for 60s in prefill`.**
  Measured on the reference workstation's 3-card vLLM seat (harness 0.139.0, `offload_review_diff`, a
  43 KB diff, ~11.8k prompt tokens): the admission warm-up loaded the seat and step 1 answered
  (4,096 tokens), then another model's swap on the same llama-swap evicted the seat, and the loop's next
  request sat in llama-swap while the seat reloaded (~180 s; cold vLLM loads measure 188–282 s). The
  prefill clock — sized from the prefill rate alone (`11784 tok / 1986 tok/s x 1.5 + 30s`, floored at
  60 s) — ran through the reload and filed it as a stall, twice in a row. The same call on a warm seat
  finished in 12 steps. The same happens after the 5-minute idle unload during a long tool call. The
  "4096 tok so far" in that reason is the run's streamed-delta count (step 1's output), not an estimate.
- **Fixed: while the seat is loading, the stall clock does not run; after it loads, a short bound
  applies.** `agent.Monitor` gains a `cold-load` phase and a seat probe (`WithSeatProbe`). While a
  request waits for its first byte (admission, prefill or re-pack), the node reads llama-swap's
  `/running` every 5 s and once more before filing a stall. The hold has two parts:
  - **Loading.** The hold enters this part only on positive evidence that a load is in progress, in
    llama-swap's own states: the seat's row is `starting` or `stopping`, or the seat is absent while
    another row is `starting` or `stopping` (a swap in progress). **Absence alone is not evidence.** A
    removed seat, a renamed alias, or a restarted llama-swap answering `{"running":[]}` keeps the normal
    stall clock. This part gets the **cold-load ceiling**:
    - `max(600 s, 2 x the seat's measured cold_load_sec)`, counted from when the request went silent;
    - never longer than the run's own ceiling, so a load that outlasts the run is still filed as
      infrastructure, not budget;
    - past it the defer reads `stalled: seat still loading after Ns in cold-load (allowed Ms:
      cold-load ceiling = max(600s floor, 2 x 226s measured cold load); the seat read "starting", …)`.
  - **Post-ready.** This part starts once a load the probe saw is over, or after the admission warm-up
    loaded the seat (`MarkSeatLoaded`). The first completion gets its own short bound,
    `max(120 s, 2 x the waiting phase's own allowance)`, never the rest of the cold-load ceiling, so a
    seat that wedges right after loading is seen within about two minutes. Past it the defer reads
    `stalled: no byte for Ns after the seat read ready, in cold-load (allowed Ms: post-ready bound = …)`.
    If the probe errors, the reason names the unreadable `/running` and never claims the seat read
    ready.
  - **The hold ends on the first byte**, or when the call completes.

  An unreadable `/running` with no load seen counts as "cannot tell", and the prefill clock runs
  exactly as before. Status readers and the delegator's progress poll see the `cold-load` phase and
  its allowance, and the first byte moves them to `decoding`.
- **Admission is covered as well.** With the admission warm-up off, the load ran under the loop's
  own tokenizer probe in the monitor's `admission` phase and was filed `stalled: no progress for 60s
  in admission`. That phase is held the same way.
- **Every door is covered.** `agent_run`, `agent_delegate` (local leg), `offload_ask`,
  `offload_review_diff` and fleet jobs all run through `runAgentTask`, which installs the probe. Known
  residual: the grammar re-pack's own per-request HTTP timeout (`repackTimeout`, at least 120 s) is a
  client timeout outside the monitor, so a seat evicted between the loop's last step and the re-pack can
  still cut that attempt. The monitor no longer files it as a stall.
- Tests: six pipeline tests run against a fake llama-swap:
  - a load is not a prefill stall;
  - a seat stuck `starting` defers with the cold-load reason;
  - the first completion after the warm-up is held;
  - a seat absent with nothing loading stalls at the floor;
  - silence after `ready` defers at the post-ready bound;
  - a silent ready seat is still a prefill stall (the guard).

  There are nine `agent.Monitor` unit tests, including an unreadable `/running` mid-hold being
  reported honestly and the hold clamped to the run ceiling. Seven mutants are caught: dropping the
  call-site probe wiring, dropping `MarkSeatLoaded`, a probe that reads `starting` as ready, absence
  counted as loading, the post-ready part given the cold-load ceiling, no run-ceiling clamp, and an
  unreadable probe labelled ready.

## [0.139.3] - 2026-09-23 - a music render with dead air re-renders once, then defers; every clip is loudness-normalized; whisper's no-speech crash defers calmly

### Fixed — ACE-Step music dead air + a genuine whisper-server crash surfaced calmly (F-35 regression follow-up)

- **Music (`render/wf-acestep.mjs` + `render/comfy-music.mjs`): short instrumental renders reliably
  went dead partway through.** Root-caused 2026-09-23 on 4 independent 15s renders (the two original
  defect clips plus two fresh reproductions — plain empty lyrics and an `"[instrumental]"` lyric tag,
  which does NOT fix it): real content for ~9-11s then a clean, near-silent tail for the rest.
  Traced to `comfy/ldm/ace/ace_step15.py`'s `AceStepConditionGenerationModel.prepare_condition` — an
  instrumental (no-lyrics) render's actual content is driven entirely by the `generate_audio_codes`
  LLM planner's own output, which degrades to a near-constant low-energy code well before the
  requested duration; disabling the planner is not a workaround either — by source-code trace (the
  live confirmation render hit unrelated GPU-lease contention and was not retried) the model falls
  back to the pure silence-latent reference for the whole clip instead. No graph/parameter change fixes
  this — the harness already matches the official `audio_ace_step1_5_xl_turbo` Comfy-Org template
  field for field, and no shipped official template exercises the instrumental/no-lyrics case at
  all. Separately, true peak measured at a literal 0.0 dBFS (clipping) on two of the three renders —
  no loudness normalization existed anywhere in the pipeline.
  **Fix: a new post-render QA gate, `render/audio-qa.mjs`.** Measures trailing/leading silence
  (ffmpeg `silencedetect`) and true peak/integrated loudness (`ebur128`) after every render; a dead
  clip (>1.0s edge silence, or >10% of the clip silent) gets exactly one automatic re-render with a
  fresh seed (the planner's output does vary by seed); still dead after the retry fails the run with
  a `DEAD_AIR`-tagged error (`internal/gpugen`'s `ClassifyErr` gains a `dead_air` class, so
  `runGenerateAudio` defers it typed instead of shipping — or silently timing out on — a broken
  clip). Every accepted render is loudness-normalized to -14 LUFS / -1 dBTP regardless of the
  dead-air verdict. `Pipeline.genEnv()` now threads `FFMPEG_PATH` to every ComfyUI-backed render
  script (previously only the Go-side `internal/audioio` transcribe path had it configured).
  Docs: `docs/systems/media-generation.md` → "ACE-Step 1.5 music".
- **`offload_transcribe` (`internal/sttclient`): a genuine whisper-server crash surfaced as an
  alarming "transcribe call failed" error instead of a calm no-speech result.** whisper-server
  (`whisper.cpp` build-v194) reliably exits mid-request — confirmed via `llama-swap.log`'s "upstream
  process exited unexpectedly" — on audio with no speech content. This is NOT the cold-load/
  near-silence-specific behavior the existing code comment assumed: direct `/inference` calls
  reproduced the crash 100% of the time, repeatedly, well outside any cold-restart window, on (a) a
  near-silent ACE-Step tail, (b) the identical clip with the per-request `vad` field both on and off
  (the server always loads with `--vad` baked into its launch command, so the field cannot disable
  it), and (c) a plain loud 440 Hz sine tone with no ACE-Step involvement at all — the common factor
  is no speech content, not loudness or a cold load. This is an upstream whisper.cpp bug outside
  this repo (not something a retry works around — the same audio crashes the server again every
  time). The harness-side fix: `sttclient.Transcribe` now wraps the crash-signature error in a new
  `ErrUpstreamNoSpeech` sentinel, and `runTranscribe` (`internal/pipeline/pipeline.go`) maps it to
  the SAME calm "empty transcript (no speech detected)" defer the clean no-speech case already uses,
  instead of the alarming "transcribe call failed: ... upstream crashed" wording.

## [0.139.2] - 2026-09-23 - sd.cpp Qwen-Image-2.1 renders opaque by default, a family-only node takes fleet image-gen, HyperFrames browser ensure prefers IPv4

### Fixed — sdcpp alpha default, family-only fleet gate, HyperFrames browser-ensure IPv6 hang

- **sd.cpp's qwen-image-2.1 render was never byte-opaque by default, and `transparent` was refused
  on every sdcpp binding regardless of family.** `SplitImageWithAlpha` — the ComfyUI graph's
  channel-drop step D5 specifies for the non-transparent default — had no sdcpp equivalent, so an
  ordinary prompt on the sdcpp engine handed back a PNG with a real (if visually subtle) alpha
  channel (measured live on `<node-f>`: 5.53% of pixels < 255 on a non-RGBA prompt). Separately,
  `config.SupportsTransparentImage` excluded the sdcpp engine unconditionally
  (`ImageGenEngine != "sdcpp"`), so `transparent:true` was refused for qwen-image-2.1 on sdcpp even
  though sd.cpp's build of the model carries the identical RGBA VAE — the refusal message also
  wrongly implied the fix was "run this on ComfyUI." Fixed: `SupportsTransparentImage` now follows
  the FAMILY, not the engine; `render/sdcpp-generate.mjs` flattens the default render to opaque RGB
  after sd-cli writes it (`render/png-alpha.mjs`, a new dependency-free RGB/RGBA PNG codec — no
  compositing, a straight channel drop, matching `SplitImageWithAlpha`'s own behavior) and wraps the
  prompt in the official RGBA template (`wf-qwen-image-21.mjs`'s `rgbaPrompt`, reused not
  duplicated) when `transparent:true`; `internal/imagegen.sdcppArgs` now carries the `--transparent`
  flag through to the runner, mirroring the ComfyUI path's `buildArgs`.
- **The fleet HTTP door could not see or reach an image-gen family on a node with no default image
  binding** — the shape every non-commercial opt-in family requires (ADR 0058: never the default).
  `fleetnode.taskConfiguredFor`'s `"image-gen"` case and the `/fleet/health` `image_families`
  advertiser both gated on the default binding alone (`config.ImageRouteConfigured`), so
  `POST /fleet/dispatch` refused `generate_image` with `unsupported task_type` even when
  `local-offload doctor` showed the family fully CONFIGURED and it rendered correctly through the
  CLI door. Fixed with a new `config.ImageGenAdvertisable` (default binding OR any named family);
  the advertised `loadable_model_families`/`image_families` no longer claim a phantom `sdxl` default
  or an unconfigured default row for a family-only node. A request with no `family` on such a node
  still gets the same "no image-gen route configured" defer any unconfigured node gets — never a
  silent render of a named family as if it were the default.
- **HyperFrames' `browser ensure` could hang indefinitely on a host with a dead IPv6 route** to
  `storage.googleapis.com` (chrome-headless-shell's download CDN): plain `dns.lookup`'s default
  result order returns whatever the OS resolver gives back first, with no `curl`-style
  happy-eyeballs failover, so an IPv6 address up front stalls the whole download. `render/
  compose-hyperframes.mjs` now passes `--dns-result-order=ipv4first` as a NODE flag (not env — the
  scrubbed child-env allowlist is untouched) on the `browser ensure` spawn only; `browser path` and
  every other subcommand are unaffected. The existing per-op deadline (`--timeout-sec`, default
  900s) and typed `TIMEOUT`/`BROWSER_MISSING` `COMPOSE-FAIL` classes already bound and classify a
  stuck ensure — both installers (`setup/install.sh`, `setup/install.ps1`) call this same runner for
  `browser ensure`, so they inherit the fix with no changes of their own.

## [0.139.1] - 2026-09-22 - the GPU lease queue serves waiters in arrival order

### Fixed — the GPU lease queue serves waiters FIFO instead of racing them

- **A queued `gpu reserve` could lose to a later arrival indefinitely.** Measured live
  2026-09-22: a text reservation queued at ~18:40 was still waiting at 20:24 while two media
  reservations that queued after it (~18:40, 19:06) each took the card ahead of it. Root cause:
  `Acquire`'s retry loop had every waiting process poll `TryAcquire` once a second with no
  ordering between them — `registerWaiter`/`Waiters()` recorded who was queued, but nothing
  in the acquire path ever consulted that record before racing for the `O_EXCL` claim, so
  whichever process's poll tick landed first after a release won; arrival order was pure
  scheduling luck. Each poll now checks `isFrontOfQueue`: only the OLDEST live waiter
  attempts the claim, everyone else keeps waiting, so the front of the line takes a
  just-freed card uncontested. A dead waiter's record is pruned on read (unchanged) and
  never blocks the line; a waiter whose own `--wait` expires leaves the line for whoever is
  behind it; class carries no priority (no ADR documents one for the queue itself), so text
  and media waiters interleave in pure arrival order. `registerWaiter`'s file name now mixes
  in a random token so two registrations from the same pid in the same millisecond cannot
  collide, and its removal goes through `removeClaim`'s Windows-safe retry — a bare
  `os.Remove` was safe when the waiters directory was read only occasionally, but the FIFO
  gate now reads it on every poll from every waiter, and a removal a concurrent reader
  blocked (the same sharing-violation window `meta.json` and the epoch lock already retry)
  left a phantom, un-removable "front of queue" that starved everyone behind it. Docs:
  `docs/systems/gpu-lease.md` ("A held card is a place in line" → the new FIFO bullet).
- **Lead review before ship: an alive-but-not-polling waiter could still wedge the FIFO
  queue forever.** Pid liveness cannot distinguish "queued and actively polling" from
  "queued, still alive, and never polling again" — a suspended process, one wedged in
  another goroutine, or an older harness binary whose `Acquire` loop exited without
  unregistering. Every waiter now re-stamps its own record's mtime on every poll tick
  (`refreshWaiter`, the same beside-then-rename-over pattern as `Renew`/`Restamp`, since this
  file is read by every OTHER waiter on every one of ITS ticks); `Waiters()` treats a record
  unrefreshed for 10x the poll interval (floor 15 s, both overridable in tests via
  `waiterHeartbeatTTL`) the same as a dead pid — skipped for ordering, pruned best-effort.
  This doubles as mixed-version safety during a rollout: an older binary's record is the
  identical JSON shape (no schema change) so a newer reader parses it fine, but the old code
  never refreshes it, so it goes stale on the same clock and cannot wedge a new-version
  waiter behind it. Pid recycling was already covered (`StartTimeMs` vs. the current
  `procStart`, the same check `Reclaimable` uses for the lease holder) — added a dedicated
  regression test. Added a same-process concurrency test (several goroutines, one real pid,
  zero stagger, to force `SinceMs` ties) confirming the random-token filename tie-break
  always resolves to exactly one front-of-queue, never both (livelock) or neither; the
  in-process `mediaSlot` path never touches the waiters directory and was unaffected either
  way. Added a dedicated concurrent-reader test for `registerWaiter`'s unregister call site
  (`removeClaim`, not the `os.Remove` this round replaced) mirroring
  `TestRenameReplacingSurvivesAConcurrentReader`'s tight-reader-loop pattern. Every new check
  was mutation-tested: disabling the heartbeat-staleness check, the tie-break, or reverting
  to a bare `os.Remove` each turns its matching test red.
- **`-count=10` caught a real race the review's own fix introduced: `Waiters()`'s read had no
  retry against a now-much-more-frequent concurrent rename.** Once every waiter rewrites its
  own record every poll tick (the heartbeat above), a plain `os.ReadFile`/`os.Stat` can
  transiently fail on Windows while that rename is in flight — the exact class of ephemeral
  error `renameReplacing`/`removeClaim` already retry, just never on this read path. Treating
  the failure as "this waiter isn't here" silently hid a live, genuinely-earlier waiter from
  one reader for one tick — enough for a later arrival to see itself as front-of-queue and win.
  A diagnostic run confirmed the recorded `SinceMs` values stayed perfectly ordered throughout
  (`[0 1 2 3 4 5]`) even in a failing trial (`acquisition=[0 1 3 2 4 5]`), ruling out timing
  jitter and pointing at the read path itself. The `os.Stat` call was worse: it deleted the
  file on ANY error, not only a confirmed absence, so a transient failure could permanently
  destroy a live waiter's queue position. `readWaiterFile`/`statWaiterFile` retry a transient
  failure (`removeClaim`'s budget) and return `os.IsNotExist` unretried; `Waiters()` now prunes
  only on a confirmed absence or a confirmed stale/dead/recycled record, never a retry-exhausted
  transient one. `TestQueuedWaitersAreServedInArrivalOrder`/`TestMixedClassWaitersAreServedInArrivalOrder`
  ran clean 30/30 at `-count=30` after the fix (previously ~1 in 10 failed); a new dedicated
  test, `TestWaiterReadSurvivesAConcurrentRename` (mirroring
  `TestRenameReplacingSurvivesAConcurrentReader` with the reader/writer roles reversed), catches
  the regression directly and fast — mutation-tested: reverting to bare `os.ReadFile`/`os.Stat`
  fails it 5/5 in under a second, and reproduces the original `[0 2 1 3 4 5]`-style symptom in
  the FIFO order tests at `-count=10`.

## [0.139.0] - 2026-09-22 - a Tier-1 cascade call never evicts a loaded vLLM seat

- **A summarize / classify / extract / triage call could unload the loaded vLLM seat.** On the
  reference box the three-card seat shares the mutually exclusive `interactive` matrix set with
  every cascade rung. The llama-swap log recorded four matrix evictions of it, one by the e4b rung:
  `model=gemma-4-e4b set=interactive … evict=[qwen3.8-27b-vllm-3card]`. Each cost a 3–5 minute cold
  load and the seat's whole prefix cache in the middle of a long session. The C-41 lane never
  fired, because a loaded seat with nothing in flight is not "busy": llama-swap swaps it out at once.
- **New `internal/seatguard`, on by default (`cascade_seat_guard`, `*bool`; `false` turns it
  off; inert without `vllm_seats`).** Per rung it reads which models hold the cards from llama-swap's
  `/running`, through the new `seatload.Occupants`. That is the same decode `Running` / `Inflight`
  (`offload_status.local_agent_seat`, `gpu status`, the drain) now share. It then asks whether
  loading the rung would unload a declared seat, using llama-swap's OWN routing read from
  `serving_config_path`:
  - matrix `vars` / `sets` / `evict_costs`, in the legacy top-level form or under
    `routing.router.settings`, expanded with `&` `|` `()` `+ref` and solved as llama-swap solves it
    (the lowest-cost combination holding the rung; a tie reports the union; a model in no set runs
    alone);
  - or `groups` (swap / exclusive / persistent, and the default group).

  No model name is compiled in. Checked against the live config, it reproduces all four logged
  evictions line for line.
- **The pipeline gives a protected rung the first non-evicting door.** Its `cascade_remote_lanes`
  lane is used when that lane serves the SAME model, sent with the new `llamaclient.WithLocalBusy`
  (the send takes the lane although neither busy gate fires; residency is still required). Otherwise
  the loaded seat itself serves as the rung, a D-129 vLLM rung with `structured_outputs` and the
  non-thinking render. A busy seat queues the call, and nothing is refused.
- **The terminal reasoning tier is guarded the same way.** A protected reasoning tier with no lane
  runs its ATTEMPT on the loaded seat. It is still one attempt, with the reasoning budget, marked
  `Reasoning`, and behind the grammar and truncation gate. It uses the seat's `structured_outputs`
  and the non-thinking render instead of the think-wrapped GBNF, which a vLLM seat cannot take. It is
  skipped when that seat already answered the call. (Review round 2 found a first cut that appended
  the seat to the ordinary chain as a plain rung. That lost the budget and the `Reasoning` mark, and
  a matrix where only the reasoning tier is exclusive exposed it.)
- **A rung served off the box no longer probes its window here.** The climb's `tierNCtx` asks
  `/upstream/<rung>/props` on this box, which LOADS the rung. Such a rung now reads the entry
  packing, recorded as `tier_pack: entry-inherited (seat guard: …)`.
- **Unknown readings fail toward the seat.**
  - An unreadable serving config treats every other model as evicting a loaded seat, and the line
    names every loaded seat.
  - A `/running` that cannot be refreshed keeps the last reading that saw a seat loaded for 300 s.
    A non-2xx `/running`, or one with no `running` list, now counts as unreadable. It used to decode
    as "nothing running"; this also hardens `seatload.Running` / `Inflight`, which share the read.
  - **Known residual risk:** past 300 s, or with no usable reading at all, the guard names no seat.
    The rung rides its lane when one serves it, and otherwise runs as configured, unguarded. That
    can evict a seat that is in fact loaded. It is kept because the guard never refuses a call, and
    it is logged on every such call.
- **A lane lost between plan and send never falls through to this box.** A `WithLocalBusy` call
  whose lane stopped serving the model is refused before any request
  (`llamaclient.ErrLaneUnavailable`). The pipeline then serves the rung, or the reasoning tier, from
  the loaded seat and logs the divergence.
- **`seatguard.Shared` checks its cache first.** It used to build, and discard, a Guard and its
  HTTP client on every call, once per contract.
- **Tie caveat.** On a genuine cost tie, the guard's `set=` / `target=` / `evict=` can differ from
  llama-swap's log line. llama-swap picks one set by definition order; the guard unions the tied
  sets on purpose. Var names fold case, while llama-swap matches them exactly (commented in
  `residency.go`).
- **Log lines.** Guard warnings are rate-limited per cause and text, so a changed failure reason is
  logged at once. A model in no matrix set, which llama-swap runs alone and which evicts everything,
  is warned about once per model.
- **With no vLLM seat loaded, the chain and every request body are byte-identical to the guard-off
  build** (pinned by test). One serve-log line per call per door, in llama-swap's own shape:
  `cascade seat guard: model=gemma-4-e4b set=interactive evict=[qwen3.8-27b-vllm-3card] target=[…]
  cost=1 -> served by the loaded seat qwen3.8-27b-vllm-3card (no lane serves …)`.
- New `llamaclient.Client.OffBoxFor` answers whether a lane or pin would carry a model, without
  side effects.
- Tests:
  - the solver (both engines, both config shapes, costs, ties, expansion, rejected configs);
  - the guard (inert, control, loaded / starting / stopping, alias-declared seat, stale, unknown,
    unreadable config, config reload, one read per window, process-wide sharing);
  - `seatload.Occupants`, and the client hooks;
  - seven `Run` scenarios over a real config file: loaded seat, no-seat control, unknown readings,
    no alternative served by the seat, lane, the climb's window probe, and the reasoning tier.

  Nine compiling mutants of the solver, the verdict, staleness, the doors, the probe skip, the
  reasoning skip and the forced lane are each killed. An E2E through the built binary, against a
  fixture `/running` and the box's real serving config, logs the decision line above and shows the
  single completion going to the seat with `structured_outputs`.

## [0.138.0] - 2026-09-22 - opencode: the offload subagent's prompt diet, and three hook fixes for 1.18.32

- **The delegate placement digest never reached the model.** For MCP tools opencode 1.18.32 hands
  `tool.execute.after` the raw MCP result (`{content: [...]}`) and builds the model-visible text
  from `content` after the hook, so the digest the plugin appended to `output.output` was dropped
  (0 of the 5 `agent_delegate` results stored in opencode.db carried it). The digest is now the
  first text part of `content` (first, because opencode head-truncates tool output past
  `tool_output.max_bytes`); built-in tools keep `output.output`.
- **The read-only `task` reroute was a no-op, with a false "ran on the offload seat" line.**
  opencode 1.18.32 executes the args object it handed `tool.execute.before` and ignores a replaced
  `output.args`; the plugin replaced it. Reproduced live: a read-only leg sent to `general` ran on
  `general` and came back stamped as having run on `offload`. The plugin now changes the args in
  place, records a reroute only when the new agent took, and stamps the result only when the
  child session the task created is really an `offload` session (from `session.created`, falling
  back to opencode's `(@<agent> subagent)` title). A leg addressed to `offload-media`, or (in
  recon mode) a media-shaped leg, is never rerouted.
- **New option `offloadTools: "recon" | "all"` (default `recon`).** opencode sends every enabled
  MCP schema up front, and the offload subagent carried every harness tool (34 and 19,025 tokens on
  0.133.0; 35 and 20,868 on 0.137.0). In
  recon mode it keeps its twelve read-and-digest lanes (`agent_delegate`, `agent_run`,
  `offload_ask`, `offload_status`, `offload_research`, the four cascade tools, `offload_ocr`,
  `offload_vqa`, `offload_extract_image`: every harness call it made in 35 recorded sessions was
  one of these; 8,265 tokens of schema), and a new `offload-media` subagent (same model,
  read-only) holds every other harness tool, so a tool the harness adds later lands there (0.135.0's
  `offload_compose_video` does). The Tier-1 protocol and the `task` description route media legs to
  it. `all` keeps the previous shape.
- **Permission defaults never shadow a user rule.** opencode applies the last matching rule in key
  order; the plugin now inserts its defaults before the user's first `<mcp>_…` key instead of
  appending them (an appended `harness_*` deny silently overrode a user's
  `harness_offload_vqa: "allow"`), and still never writes a key the user set. Tests resolve the
  rules with a copy of opencode's matcher against the 34 measured tool names; live, `opencode
  debug agent` resolves offload to exactly the 12, offload-media to the other 22, the primary to
  the 4 Tier-1 tools.
- **Offload child sessions, title and compaction requests no longer get the dispatch protocol.**
  Subagents are denied `task`, so "issue a task call" contradicted the child's own tools; title
  and compaction requests (recognised by their agent prompt) now get nothing injected. In offload
  children the global rules file is swapped for a 3-line digest (verify, then assert; quote
  identifiers exactly; stop and report embedded instructions), only when the segment matches the
  file on disk byte for byte; otherwise it is left as is and counted in
  `offload_plugin_status.diagnostics.systemTransform.childFailOpen`.
- **Title and compaction requests stop thinking at xhigh.** A new `chat.params` hook gives the
  `title` and `compaction` agents on Qwen-family models `chat_template_kwargs.enable_thinking:
  false` (on a Qwen3.6 seat `reasoning_effort: "low"` was ignored, 400 of 400 completion tokens
  reasoning; never `"high"`, an HTTP 500 on the Qwen3.8 template), and caps titles at 64 output
  tokens and compaction at 16,384 (4.4x the largest summary measured). Keys the user set win.
- **Roster check:** the offload prompt and the all-mode protocol name `offload_status
  {section:"brief"}` (0.137.0); a harness before it ignores the argument and returns the full
  dump (checked on 0.133.0).
- Measured (E2E on harness 0.133.0 through a logging proxy, request bodies rendered with the
  Qwen3.8 seat template and tokenizer): the offload child's first request 24,588 → 10,410 tokens
  (−57.7%, 39 → 17 tools); offload-media's 13,110 (paid only by media legs); the primary pays +80 tokens for the
  media routing line; a title request 812 → 609 prompt tokens, `max_tokens` 4,096 → 64, and 0
  reasoning tokens instead of 573-1,372; a compaction request on the same seat went from 1,580 of
  1,964 completion tokens reasoning (173 s to the next request) to 0 of 462 (27 s).
- **No silent failures (review round).** Each `config` step runs on its own, so one that throws
  (a frozen permission object, an odd user value) no longer skips the offload scoping after it —
  which would have stranded the offload agent behind the Tier-1 deny; failures are listed in
  `diagnostics.configStepFailed`, and a permission value that is not an object in
  `permissionRejected`. The permission rewrite computes the new order first and restores the
  user's original rules if the write fails part-way. A rerouted leg's FAILED and needs-primary
  notes are written whether or not the child agent was confirmed; only the "ran on the offload
  seat" claim needs the confirmation, and a reroute counts only when both the agent and the prompt
  suffix took. New counters: `systemTransform.childHeaderMissing` and `unknownSession`,
  `chatParams` (applied and each skip reason), and `auxAgreement` (title/compaction requests seen
  by prompt text versus by agent name, so a reworded opencode prompt shows up as a mismatch).
- **The reroute works on both of opencode's task paths** (checked in the bundle and live): a
  model tool call (`SessionTools.resolve`) and a subtask part (`SessionPrompt.handleSubtask`, a
  `/command` with `subtask: true`), which runs the registry's `task` tool with the very args object
  the hook changed. On the subtask path the parent's stored task part keeps the command's agent
  name; the child session is the source of truth.
- Docs: `docs/systems/opencode-integration.md` gains the measured tool surface, the recon /
  offload-media split, the permission precedence rule, the one-system-message shape, the
  title/compaction parameters and the 1,568-token cache-block granularity; the plugin README gains
  the options table (`primaryTools`, `offloadTools`). Plugin 0.3.0; 109 bun tests; 49 mutants, each
  confirmed to typecheck, all caught.

## [0.137.0] - 2026-09-22 - offload_status answers one block, or the brief form

### Added

- **`offload_status` takes one optional argument, `section`.** The full answer is ~19 KB (4.7k tokens
  by a 4-chars/token estimate; a tokenizer measured an earlier 16 KB answer at 6.1k), and it is the
  usual FIRST call of a delegating session. Sizing a contract needs only the fleet block, and 43%
  of the dump is `gpu_lease` (every process on every card). `section:"brief"` returns the whole
  `fleet` block plus one-line `gpu_lease_verdict` and `local_verdict`: 4,426 bytes against 18,768
  on the reference box (23.6%). A block name (`local`, `media`, `remote`, `accelerators`, `reuse`,
  `fleet`, `kv_cache_server`, `gpu_lease`) returns that block alone, and computes only that block:
  the fleet section runs no nvidia-smi. The two brief lines have their own keys, so nothing that
  decodes `gpu_lease` or `local` as an object meets a string. `config_error` stays the first key of
  every answer.
- **The default is unchanged, byte for byte.** No argument, `{}`, `section:"all"` and `section:""`
  answer exactly what the handler answered before. A golden captured from the pre-change handler
  on a fixture that owns every machine-dependent input pins it. Two mutations proved the pin bites:
  a block emitted as `null` when absent, and one changed character in a note.
- **A wrong call is a defer, not the full dump.** An unknown section (`"gpu"`), an unknown argument
  (`brief:true`) or a wrong type now defers with the valid values listed. The old handler ignored
  every argument, so a guessed one silently cost the whole ~19 KB.
- `agent_delegate`'s sizing guidance now names `offload_status {section:"brief"}`, and
  `offload_status`'s description names the brief form. The schema's enum is built from the same
  block table the handler dispatches on.

## [0.135.2] - 2026-09-22 - the GPU lease survives a concurrent reader on Windows

### Fixed — the lease record's rename-over retries the Windows sharing race

- **The GPU lease's rename-over could fail on Windows under an ordinary concurrent reader.**
  `Manager.Restamp` (the DRAINING -> EXCLUSIVE stamp `gpu reserve --drain` applies once the seat
  goes idle) renamed a fresh record over `meta.json` with a single `os.Rename`; on Windows,
  `os.Rename` is `MoveFileEx`, which must delete the destination's directory entry to replace it,
  and Go's `os.Open` never grants the `FILE_SHARE_DELETE` bit its readers would need for that to
  succeed while they hold the file open (`os.ReadFile`/`Inspect()`, `gpu status`,
  `offload_status`, any other reader — none of them coordinate with a writer's rename). A reader
  mid-flight at the exact rename instant made it fail outright with `ERROR_ACCESS_DENIED` /
  `ERROR_SHARING_VIOLATION` — intermittently, ~1 run in 24 (`TestReserveRenewsTheLeaseWhileDraining`,
  `TestReserveRenewsTheLeaseWhileWarmingBack`), and in production the same failure leaves the
  DRAINING stamp on a held lease, cordoning the seat for the rest of the lease's window (register
  C-50/S-31 territory) or drops a heartbeat renewal. `internal/gpulease/renamesafe.go` adds
  `renameReplacing`, a Windows-only bounded retry (2 s, mirroring the epoch lock's own wedge
  bound) on the two ephemeral errnos — the same pattern `cmd/internal/robustio` uses in the Go
  toolchain, and the rename-side twin of this package's existing `removeClaim` retry for the
  analogous `os.Remove` case. `Restamp`, the epoch counter's `writeEpoch`, and the heartbeat's
  `Renew` all route through it; Linux/macOS behaviour is unchanged (`isEphemeralRenameError` is
  unconditionally false there, so `renameReplacing` is exactly one `os.Rename`).
  `TestDrainGivesUpOnAnUnchangedBusyStateBeforeTheDeadline`'s one observed flake (under full-suite
  load, 8/8 clean alone) does not touch `gpulease` at all — a different cause, a tight no-progress
  timing bound (`stuckAfter: 80ms`) under CPU contention, not this race.

## [0.135.1] - 2026-09-22 - a GPU lease fences in-flight probes, the chat lane and the embedder; a media drain no longer deadlocks

### Fixed — a GPU lease fences the probes of runs already in flight; a media drain no longer deadlocks

- **In-flight agent runs loaded the agent seat onto a render's cards.** llama-swap starts any model a
  request under `/upstream/<model>/…` names, and the served-window probe, the seat-pin probe, the
  tokenizer, the warm-up, the whisper transcription and the KV-slot lane each built that route outside
  every gate. A run admitted just before a video render kept sending them: the 3-card seat started
  repeatedly mid-render, 14.3 GB landed on a card the render held, and the render took 895 s against a
  usual 228-324 s. `modelaffinity.AwaitUpstream` is now the one builder of an `/upstream` URL: under a
  media or exclusive lease it lets a request through only when `/running` lists the model `ready`, and
  otherwise waits for the card inside the caller's deadline and returns the typed `*LeaseError`. The
  window probe and warm-up wait inside the admission budget and the run defers `capacity` (holder
  named, no bare-root fallback); the seat pin and the per-step tokenizer do not wait (no pin; the
  tokenizer fails open without a sticky strike, `LastFailFenced`); the cascade's per-tier re-pack
  neither waits nor caches a fenced answer (`ProbeUpstreamWindowNow`); transcription waits its client
  timeout. A run whose generation ran out its wait mid-run is filed `capacity` on both run doors,
  before the stall and ceiling branches. `TestUpstreamURLsAreBuiltOnlyBehindTheFence` fails on any
  other file that spells the route; the lease holder's own warm-back uses the one unfenced builder,
  `HolderUpstreamURL`, from `gpu_drain.go` only. ADR 0026 extended.
- **The fleet chat lane and the embedder loaded models past the lease too.** `/v1/chat/completions` and
  `/v1/embeddings` are model-dispatched llama-swap routes that swap the named model in. `POST /fleet/chat`
  forwarded another box's cascade call there with no gate, and the embedder (kNN pre-filter,
  `shadow-label`) posted outside `Admit`. Both now build the URL with `modelaffinity.AwaitModelRoute`
  (the same fence). The chat lane waits inside the caller's budget and answers `503` with the typed lease
  refusal (filed `timeout`, congestion) when the render outlasts it. The embedder waits its own timeout,
  and the kNN pre-filter fails open. `TestModelDispatchedRoutesAreBuiltOnlyBehindAGate` fails on any
  ungated literal of a model-dispatched route.
- **`gpu reserve --class media --drain` blocked the runs it was waiting for.** The lease record
  dropped the draining stamp on a media lease, so the media class fenced the runs in flight from
  acquire and the drain waited on work it was itself holding up (ADR 0041's deadlock, for the media
  class). The stamp is recorded for either class, a draining hold blocks no load, a new run is
  cordoned under a draining hold of either class, and the media class fences once the drain clears
  the stamp. ADR 0041 extended.

## [0.135.0] - 2026-09-22 - HyperFrames motion graphics as a CPU-class composition lane

### Added — `offload_compose_video`: HyperFrames motion graphics as a CPU-class lane (ADR 0059, Proposed)

- **The harness could not produce designed, text-exact motion graphics.** New tool
  `offload_compose_video`, CLI verb `compose-video` and fleet task `compose-video` render an
  HTML/CSS composition to MP4, WebM (VP9 alpha), MOV (ProRes 4444 alpha), GIF or a PNG sequence
  with HyperFrames (npm `hyperframes` 0.8.61, Apache-2.0). Inputs: a vetted template plus typed
  variables (`title-card`, `lower-third`; `render/compose-templates/`), an inline `html` page, or a
  `project_dir`.
- **CPU-class, no GPU lease.** Software GL (`--no-browser-gpu`) and CPU encode; `--gpu`,
  `--browser-gpu` and `--docker` are refused. The pipeline serializes compositions on its own
  in-process slot (`compose_busy` after `gpu_wait_ms`), never `mediaSlot` or `withGpuSlot`
  (ADR 0026), and the fleet task is exempt from the text concurrency cap.
- **One door, env-scrubbed.** `render/compose-hyperframes.mjs` is the only way the CLI runs:
  - an allowlisted child env (no `*_API_KEY`, token, `NODE_OPTIONS` or `GPU_LEASE_*`) with
    telemetry, update checks, auto-install and skills off;
  - `--json` on every invocation, because only `--json` skips the npm and GitHub pings;
  - a subcommand allowlist (`lint`, `check`, `render`, `snapshot`, `browser ensure|path`,
    `--version`); `snapshot` always carries `--describe false`;
  - a fresh empty cwd (no `.env` autoload) and a harness-owned HOME.
  The Go side adds the same allowlist to the runner's own env (new `gpugen.Spec.EnvExact`).
- **Gated and typed.** `lint` (0 errors), then `check`, then `render --batch` (one manifest row),
  then an ffprobe gate on codec, size, fps, duration, alpha and audio. The payload is what ffprobe
  measured. Failures defer with a class: `BAD_INPUT`, `LINT_ERRORS`, `CHECK_FAILED`,
  `RENDER_FAILED`, `BROWSER_MISSING`, `FFMPEG_MISSING`, `CLI_MISSING`, `SPAWN_EBUSY` (retried
  once, hyperframes#4058), `DISK_HEADROOM` or `TIMEOUT`.
- **Trusted code only.** HyperFrames' Chrome runs without a sandbox, so the fleet task refuses
  `html`, `project_dir` and `png-sequence` at ack time and renders only the node's vetted templates.
- **Pinned install.** `setup/hyperframes/` holds the exact-version lockfile. `install.ps1` (Step 7b)
  and `install.sh` (3b) run `npm ci --ignore-scripts`, `npm audit signatures` (fatal on failure),
  `npm rebuild esbuild` and `browser ensure` through the runner on nodes with Node >= 22, then seed
  `compose_script`, `hyperframes_dir` and `hyperframes_browser_path`. Older nodes print a SKIP line
  and the lane stays NOT CONFIGURED. The 14 CUDA and Vulkan tiers that ship the render tree seed
  `compose_script` (`render/compose-hyperframes.mjs`; `dual-gpu` and `cpu` do not); the installer
  un-binds it where it could not install. The runner refuses an install that is not its pin
  (`CLI_MISSING`); a test holds `PINNED_VERSION`, `package.json` and the lock's integrity together.
- **New config keys** (all empty = NOT CONFIGURED): `compose_script`, `hyperframes_dir`,
  `hyperframes_browser_path`, `compose_timeout_sec` (1800), `compose_workers` (`""`/`auto` or
  1-24), `compose_cache_dir`, `compose_quality` (`high`). `doctor`, `offload_status` and
  `acceptance` report the `compose_video` route (BOUND-BUT-MISSING per missing piece, with the
  template list when bound).
- Docs: ADR 0059 (Proposed), media-generation "Composition (HyperFrames)" with a Security
  subsection, OPERATOR-GUIDE, SETUP-AGENT, README, mcp-server (Composition family row; tool count now 29),
  glossary "Composition".

### Security — a remote media task's output never leaves the node's media dir

- **Any tailnet peer could make a fleet node overwrite any file its account could write.** Media
  dispatch is not token-gated (ADR 0023), and the fleet builders forwarded a caller's `out`
  verbatim: `buildImageGen`, `buildVideoGen`, `buildAnimate`, `buildAudioGen` and the new
  `compose-video` task. `buildRunGraph` did the same with `out_dir`. The pipeline wrote there, and
  the compose runner replaces an existing file.
- The builders now drop those keys and the pipeline writes its own
  `<media_dir>/<task>-<hash8>.<ext>` (run-graph: `<media_dir>`). A stray key is ignored, not
  refused, so older payloads keep working. No current caller changes: results are fetched by
  bare name from `GET /fleet/media/{name}`, which only serves files directly inside `media_dir`.
  The console client reads the returned path's basename. The local MCP and CLI doors keep `out`.
- Tests: `TestRemoteMediaTaskOutNeverReachesThePipeline` tries absolute, `..`, UNC,
  drive-relative, existing-file and bare-name targets against every writer.
  `TestEveryFleetWriterIsCoveredByTheOutRule` fails on a new file-writing task left out of that
  table. `TestFleetComposeWritesOnlyUnderTheMediaDir` runs a hostile dispatch end to end through
  `Pipeline.Run` and checks the named file is untouched. Docs: FLEET-NODE, fleet-node.md,
  ADR 0059.

## [0.134.0] - 2026-09-22 - Qwen-Image-2.1 ships as a named, license-tagged opt-in family

### Added — Qwen-Image-2.1 as a named, license-tagged opt-in family; per-binding ComfyUI launch profile (ADR 0058)

- **Named families (ADR 0058, Proposed).** `imagegen_families` / `gen_edit_families` add opt-in
  bindings beside a node's default one, selected per request by `family` (`offload_generate_image`,
  `offload_edit_image_generative`, `generate-image --family`, the fleet `image-gen` payload). An
  overlay is a complete binding — every model-binding key is cleared before its own keys apply — and
  MUST declare `license` + `commercial_use`; an unknown/forbidden key, a missing license or a name
  colliding with the default refuses the config load by name. A request without `family` renders
  exactly what it did before.
- **Every result is license-tagged.** Results carry `family`, and `license` / `commercial_use` when
  the binding declares them; `commercial_use:false` adds `license_note` ("research/evaluation use
  only under <license>; not for commercial work"). The ledger row carries `license` (absent = unknown).
  `offload_status` gains `media.image_families` / `media.edit_families` and `/fleet/health` gains
  `image_families`, each with its license flags. Default bindings may declare `imagegen_license` /
  `imagegen_commercial_use` (edit: `gen_edit_*`), both or neither.
- **Qwen-Image-2.1 graphs** (`render/wf-qwen-image-21.mjs`, family `qwen-image-2.1`, needs ComfyUI
  ≥ v0.37.0): T2I (`TextEncodeQwenImage21`, `EmptyLatentImage`, 4-channel `VAEDecode`, /32 snap,
  default 2048², 40 steps / cfg 1 / euler) and a multi-reference edit (up to 10 images, target first,
  `QwenImage21Cache`). `imagegen_schedule` `official` computes diffusers' dynamic-mu exponential
  schedule with `shift_terminal` in JS (`ManualSigmas`), golden-tested to 1e-6 against the real
  diffusers scheduler; `comfy` uses ComfyUI's fixed shift. `transparent` wraps the official RGBA
  prompt template and keeps alpha; otherwise the output is opaque RGB. Edit knobs:
  `gen_edit_family`, `gen_edit_resolution`, `gen_edit_cache_device`; `offload_edit_image_generative`
  gains `images`. No tier seeds it.
- **Per-binding ComfyUI launch profile.** `comfy_cuda_device` (`--cuda-device`, ComfyUI device
  order; single-card routes only — never a pooled seat, never run-graph), `comfy_dynamic_vram`
  (`on` strips `--disable-dynamic-vram`, `off` adds it) and `comfy_extra_args`. A running ComfyUI
  whose `/system_stats` argv contradicts the profile is relaunched when the harness owns it and
  nobody holds it, otherwise the render defers with `COMFY-PROFILE-MISMATCH: …` — never reused
  silently. The config load warns on `on` or a device pin together with pool keys.
- **Fixed: blackwell-3x16's single-card ComfyUI routes rendered on the display card.** ComfyUI was
  launched with every card visible and no device flag, so edit / upscale / inpaint / animate ran on
  `cuda:0` — the 5070 Ti driving the desktop in ComfyUI's fastest-first order. The tier now seeds
  `comfy_cuda_device: "2"` (the 5060 Ti at PCI B5:00.0, the better-cooled card of the pair — operator
  choice), and `TestTripleBlackwellNeverSchedulesOntoTheDisplayCard` fails a tier
  that seeds a single-card route without a non-display pin.
- **Fixed: an unknown `--family` rendered the SDXL graph.** `comfy-render.mjs` / `comfy-edit.mjs`
  exit 2 on a family outside their closed set, before any GPU work.
- **Batch results are license-tagged too.** `generate-image --batch` items and the batch payload
  carry the default binding's `family` / `license` / `commercial_use` / `license_note`, as a single
  render does (before, only the ledger rows carried the license).
- **Fixed: `generate_image` reported the request's (or a guessed 1024) size.** `width`/`height` are
  now measured from the written file on both engines; `edit_image_generative` reports them too.
- **Fixed: doctor could not see preset-implied model files.** `comfyui model bindings` now resolves
  a preset's Lightning LoRA and a builder's default text encoder/VAE, so a 2511 edit bound to
  `lightning8` with its LoRA absent is a `MISSING` row; every named family gets its own
  `generate_image:<name>` / `edit_image_generative:<name>` route verdict with its model files.
- `offload_edit_image_generative`'s description no longer claims a ~1 MP snap (the 2511 canvas
  follows the source within 0.9–2.0 MP). Docs: media-generation (families, 2.1, launch profile,
  license), OPERATOR-GUIDE how-tos, SETUP-AGENT 2.1 download set with sha256, mcp-server (tool count
  and `agent_rig` corrected), glossary ("Named family", "Launch profile"), ADR 0058.

## [0.133.1] - 2026-09-22 - the harness closes PAIR cards its dead processes left running

- **A killed harness process left its PAIR card "Running" until PAIR restarted.** A `local-offload
  delegate` CLI run killed by its parent after its running frame (job `agd-91c2ad1201b33033ab20dbfa`)
  never sent the terminal frame, and nothing on PAIR's side can know the producer died: the workload
  manager keeps local-ingress records with no expiry and re-asserts them on its anti-entropy
  heartbeat, and the broker's staleness sweep exempts records of its own origin. The 0.132.8 note
  that PAIR's sweep fails such a card was wrong for local-ingress cards.
- **New open-card register** (`internal/pairworkloads/orphans.go`): every in-flight frame
  (queued / running) writes one marker under `<state root>/pair-open/` (beside `seat-inflight/`)
  holding the frame's workloadInfo, the writer's pid and its process start identity; the terminal
  frame removes it once delivered. A terminal frame that could not be delivered (PAIR restarting, a
  reply slower than 2 s) replaces it as a pending marker that the next sweep resends with the job's
  real verdict (review finding: dropping it lost the only record of a card PAIR still showed running). Written atomically (temp + rename) on the caller's
  goroutine, so a job's markers follow its frames in order; every error is swallowed.
- **The sweep**: a marker whose process is dead, whose pid now belongs to a different process
  (start identity differs), or that is older than 24 h gets the terminal frame the producer never
  sent — state `failed`, error "harness process exited before the job finished", the in-flight
  frame's id / origin / node / engine / timestamps unchanged, `completedAt` = now — and is deleted.
  Runs once per emitter on its first `Emit` (every harness process that reports anything), and every
  45 s in fleet-serve when `pair_workloads_enabled` or `pair_seat_activity_enabled` is on. A
  disabled emitter writes and sweeps nothing.
- **Racing sweepers send one frame per orphan**: a claim is an O_EXCL `<marker>.lock`, and the
  winner removes the marker before its lock. Rename-to-claim was tried first and the race test sent
  every card twice: on Windows two sweepers that opened the marker before either renamed it both
  succeed. PAIR down: the lock is released, the marker stays, the next sweep retries.
- **Seat-watch cards are covered the same way**: they go through `Emit`, so a fleet-serve killed
  with a direct-traffic card open leaves a marker the next sweep closes (a clean stop still
  completes them via `closeAll`).
- Tests: marker lifecycle, dead producer closed once with the original identity, live producer
  untouched, recycled pid, leak cap, disabled emitter, PAIR down keeps the marker, two racing
  sweepers, first-Emit sweep, killed seat watcher, an undelivered terminal frame resent with its
  verdict, and a real child process killed after its running frame and swept with the production
  liveness rule. Mutants (sweep never judges orphaned; marker never written; undelivered terminal
  marker dropped) each turn tests red.

## [0.133.0] - 2026-09-22 - PAIR shows direct traffic on the vLLM seats

- **Seat load that bypassed the harness never reached PAIR.** On 2026-09-22 `<node-b>`'s three
  cards sat at 100 % for hours under a curl soak against llama-swap while PAIR's Jobs list was
  empty: PAIR only shows work somebody reports, and only the harness reports. New
  `pair_seat_activity_enabled` (off by default): fleet-serve polls each ready `vllm_seats` seat's
  `num_requests_running` + `num_requests_waiting` every 2 s and shows the direct part as one PAIR
  card per busy stretch (requester `llama-swap/direct`, engine `vllm`).
- **No duplicates.** The harness's own requests are subtracted: `modelaffinity.Admit` and the
  fleet chat lane write one marker per in-flight request under `<state root>/seat-inflight/`
  (`internal/seatinflight`), resolved through the llama-swap roster so an alias counts against
  its seat. Two agreeing polls open or close a card, so a request ending between the two reads
  never becomes one.
- Review fixes (all three with a regression test and a red mutant): an unresolvable marker now
  suppresses the seat's reporting for that poll instead of subtracting zero and publishing a
  harness request as direct traffic (the roster read fails most easily when the seat is loaded);
  the last roster survives a failed refresh; the register is armed only where the key is on, so a
  box without the watcher pays nothing on `modelaffinity.Admit`; marker removal retries in the
  background (the watcher's own read blocks delete on Windows, which would hide a seat's direct
  traffic for an hour).
- Tests: seat watcher (direct card lifecycle, harness-only load raises nothing, one-poll blips,
  seat exit, unreadable metrics, shutdown, disabled), marker register (count, dead/stale markers),
  Admit marks on-box and not off-box. Mutants on the subtraction, the Admit marker and the debounce
  each go red. Live: the watcher's readers against `<node-b>`'s llama-swap read the 3-card seat's
  load (1, the soak) with 0 harness markers.

## [0.132.8] - 2026-09-22 - PAIR frames are delivered before a CLI process exits

- **A delegate run from the CLI left its PAIR card "running".** Frames are sent on background
  goroutines and nothing waited for them, so the terminal frame of a short-lived process died in
  flight (seen live: a deferred `local-offload delegate` run produced the running frame only; PAIR
  fails such a card by staleness sweep much later). `RunWith` now waits for its emitter before
  returning, and the one-shot task cleanup in `main.go` waits for the ledger emitter. Each send is
  bounded at 2 s, so a down PAIR cannot hold a run. Test: a 300 ms ingress must have both frames
  by the time `RunWith` returns (fails with the wait removed).

## [0.132.7] - 2026-09-22 - PAIR cards name vLLM for an alias-bound vLLM seat

- **PAIR labelled `<node-b>`'s flagship jobs `llamacpp`.** `EngineFor` reads the engine off the seat
  NAME, and the agent seat is bound as `agent-pool`, an alias of `qwen3.8-27b-vllm-3card`. The
  emitter now carries the box's `vllm_seats` and endpoint; `Emitter.LocalEngine` labels a local seat
  `vllm` when it is declared directly or its roster canonical id is (cached 10 min, 30 s after a
  failed roster read, which keeps the name-based answer). Local delegate placements and ledger rows
  use it; remote placements keep `EngineFor`. The ledger observer builds its frame off the writer's
  goroutine because the first lookup per seat may read the roster.

## [0.132.6] - 2026-09-21 - the three-card seat is the flagship agent seat

- **The operator's order, finally in the table.** 2026-09-19: "the 3 card tier as the agent seat now and the 2 card
  tier to be the opt in one ... biggest context window and keeps the LMCache server ... leave around 20% of the 5070ti
  free ... gpu 0 gets a bit less load than gpu 2". The 3-card seat had been measured and hand-wired several times, and
  every time the blackwell-3x16 seed still described the 2-card pair, so the next install or audit put it back.
  blackwell-3x16 now seeds `qwen3.8-27b-vllm-3card` as `agent-pool`: pipeline-parallel 3 on devices `2,1,0`, layers
  `28,13,23`, fp8 KV pinned at 3.75 GiB per card, window **262,144** (pool 269,676).
- **Why that layout** (W1 campaign, 8 sweeps). Qwen3.8-27B has 4 KV heads, so three cards means a pipeline, and its
  lm_head is untied (2.37 GiB bf16) — the last stage carries it, the first carries the embedding plus the vision tower.
  Order `2,1,0` puts the 5070 Ti (the display card) in the MIDDLE, where it holds neither: it keeps 18-22% free with a
  2.6-3.3 GB desktop (~13% with a 4.1 GB one). Card 0 runs 23 layers plus the head (~26 layer-equivalents), card 2
  runs 28. 28 layers per rank is the ceiling for 262k at this KV budget (29 caps the window at 235,200). Order `0,1,2`
  failed LMCache `register_kv_caches` on the card-2 last rank twice; a two-process IPC probe cleared card 2 itself.
- **Schema.** `vllm_seat` gains `pipeline_parallel`, `layer_partition` and `kv_cache_memory_bytes`, rendered into
  `SEAT_PP`, `SEAT_PARTITION` and `--kv-cache-memory-bytes` for the windows-wsl launcher (refused on linux-systemd,
  which renders no pipeline flags). A seat that spans the display card must pin its KV bytes and keep the display
  card off the first and last stage (`TestTripleBlackwellVLLMSeatSpansTheDisplayCardOnlyWithABoundedFootprint`,
  replacing the blanket "never span it" rule the order overturned).
  `fallback_agent_device` names the pin the llama.cpp fallback renders on ("0,2") when it differs from the seat's
  own ("2,1,0"); the Go seed and the installer's PowerShell fill both honour it (render tests caught the mismatch).
- **Placement.** The free agent choice goes to the `triple` layer when it carries a non-opt-in agent seat; the pair is
  opt-in by name (`contract.layer: pair`). A box whose triple is opt-in or absent keeps the pair, unchanged.
- **The pair.** `<node-b>` keeps serving the 2-card tp2 seat as the opt-in `agent-pool-2card` (163,840). The seed holds
  one vLLM seat per tier (register A-113), so on a fresh install the pair's agent seat is the llama.cpp 27B the tier
  renders on the same cards.
- **Open.** LMCache stores the flagship's pages in the `<node-c>` store (~205 MB per 1568-token chunk), but the pipeline
  lookup serves none back after eviction (0 external hits) — a miss recomputes, no worse than the storeless seat it
  replaces. The L1 staging is 8 GB, not the 2 GB default: a 2 GB L1 failed this model's stores.

## [0.132.5] - 2026-09-21 - every seeded media model now has the script that runs it

- **Image generation and upscale deferred on every fresh 16 GB+ install.** A ComfyUI route needs BOTH
  the model the tier chose and the script that runs it — `internal/mediacap` reports the route NOT
  CONFIGURED when either is empty, and there is no default. Eight tiers (blackwell-16/32/2x16/3x16/48/72,
  ampere-16, volta-16) seeded an image model with no `imagegen_script`, and thirteen seeded
  `upscale_model` while no tier anywhere seeded `upscale_script`. `<node-b>` served both only because they
  were wired by hand. Every ComfyUI tier now seeds both halves.
- **The two 8 GB tiers' image script pointed at a directory that does not exist.** ampere-8 and
  blackwell-8 seeded `__OFFLOAD_HOME__/render/comfy-generate.mjs`; both installers put `render/` next to the
  BINARY (`<home>/bin/render`), and a relative script resolves there (`gpugen.ResolveScriptIn`). Measured on
  `<node-a>`: `D:/offload-stack/render` absent, `D:/offload-stack/bin/render` present — `<node-a>` worked
  only because it was hand-set. Seeds now use the relative `render/…` form.
- **Edit, inpaint and animate are seeded on every 16 GB+ ComfyUI tier** — the three routes that were
  measured, kept and served on `<node-b>` and seeded nowhere: Qwen-Image-Edit-2511 Q5_1 + lightning8
  ("frontier confirmed ≥16GB edit primitive"), RealVisXL V5 inpaint, WAN-Animate-2 int8 (15.9 GB on one
  16 GB card; operator 2026-08-27 "route WAN-Animate as a media capability"). blackwell-8 already seeded
  the edit UNET and inpaint checkpoint but not their scripts, so both routes deferred there too.
  `SETUP-AGENT.md` no longer calls the ≥16 GB edit model a "designation, not a binding".
- **Gates.** `TestEveryComfyMediaModelHasItsScript`, `TestMediaScriptsResolveNextToTheBinary`,
  `TestSixteenGBComfyTiersSeedTheMeasuredEditInpaintAnimateRoutes` — 70 violations on the previous seeds.

## [0.132.4] - 2026-09-21 - seeds carry what was measured, and two house rules become gates

- **The AMD tier's agent seat was the one model measured to fail.** `amd-gcn` named no
  `agent_model`, so the tierseed fallback made its `resident_tier` — `gemma4-e2b` — the agent planner
  seat on every fresh install. On `<node-f>`, `gemma4-e2b` FAILED the agent contract in 18.6 s and
  `offload-e4b` in 31 s, while `qwen3.5-4b-agent` passed three times (smoke 44.7 s, a real contract
  188 s, a 32k contract 143 s) — and existed only as a hand edit on the node. `resident_tier` is now
  `offload-e4b` (Vulkan pp512 129.08 / tg128 11.93 measured there), and the tier seeds the agent seat
  explicitly with the four lane keys the node needed live: `agent_seat_tok_s` 10 (the first run
  without it deferred with 1 s of wall left), `agent_max_tokens` 2048, `agent_timeout_sec` 900,
  `fleet_agent_enabled`. The note's "Weakest path: E2B … PROJECTED" clause, overturned by those
  measurements, is marked superseded.
- **RAM is overflow only, now in the seeds.** `ampere-8` and `blackwell-8` (8 GB discrete cards)
  seeded the 26B with `moe_26b: "cpu_moe"` — every expert in RAM — and `<node-a>` served it that way,
  against the operator's 2026-09-10 rule. Both tiers now drop the 26B, and seed their escalation and
  reasoning rungs explicitly on `qwen3.5-9b-agent` (the largest model the tier serves) instead of
  falling to a code default that would name a model no longer rendered. Rung quality on the 9B is
  UNMEASURED; the per-rung cascade eval owns that. `amd-rdna3` is a unified-memory iGPU, so its 26B
  runs on the iGPU (`gpu`), not the CPU. The `cpu` tier is exempt: it has no card to overflow from.
- **Two gates so neither comes back.** `TestNoGPUTierParksTheMoEExpertsInRAM` fails any GPU tier
  that seeds `cpu_moe`. `TestEveryAgentSeatIsChosenNotDerived` fails any tier whose agent seat comes
  from the silent `resident_tier` fallback; `blackwell-32` (projected, no box) now names the seat it
  used to derive, with no behaviour change. `TestAmdGcnSeedsTheSeatItWasMeasuredOn` pins `<node-f>`
  measurements. All three were RED on the previous seeds and name the defects exactly.
- **`audit-config`: the drift check that was missing.** `audit-yaml --against-render` audits the
  serving YAML; nothing compared a node's `config.json` against its tier seed, and that is where
  every measured win was being wired by hand and never written back. It resolves the seed exactly as
  `install seed` does (`--goos`, `--ram-tier`, the vLLM-seat detection) and reports seed-owned keys
  as DIFFERENT / LIVE-ONLY / UNSEEDED / SEED-ONLY. First fleet run: `<node-f>` 0 drifted keys after this
  release's AMD fix, `<node-b>` 28, `<node-a>` 12, `<node-c>` 30 — the wiring-debt program works those
  down. Reports under `plans/offload-harness/2026-09-21-wiring-debt/drift-*.txt`.
- **A Linux install now gets its tier's agent window.** `install.ps1` wrote `agent_ctx_tokens` from
  the tier field directly; `install.sh` never did, and the seed did not carry it, so every fresh
  Linux node ran the code default instead of the window its tier was measured at. The seed now
  carries it (unless `config_seed` or a vLLM seat binding sets it first), so both installers get it
  from one place.

## [0.132.3] - 2026-09-21 - the display card is a card PROPERTY, and placement is one order

- **`display_active`, not a guess from the process list (ADR 0057 revised).** 0.132.2 decided
  which card drove a display by inference: "`nvidia-smi` could not size this process, so it is the
  desktop". That was wrong twice over. `[N/A]` memory is a WDDM property, not a graphics-process
  property — `nvidia-smi` types all 24 of `<node-b>`'s desktop rows `C+G` and can size none of them —
  and a native-Windows CUDA seat (ComfyUI, which the 3-card law pins to card 0 or 2) produces the
  same shape. The heuristic would have flagged the card the harness was WORKING on, dropped it from
  `work_util_pct`, and made a saturated node advertise itself as idle — inverting the defect it was
  written to cure. `--query-gpu=display_active` answers it directly and rides the per-device query
  every reader already runs, so it costs no extra `nvidia-smi` call. Measured 2026-09-21: `<node-b>`
  (3 cards, operator gaming) `Disabled/ENABLED/Disabled`; `<node-c>` (headless Linux A2) `Disabled`;
  `<node-a>` (laptop, screen on the iGPU) `Disabled`.
- **Less machinery, not more.** Because the answer now rides the device sample the health tick
  already takes, the fleet node's separate display probe, its 30 s cadence and its carry-forward
  are all gone. Nothing to go stale, nothing to expire, one fewer driver call.
- **Placement is ONE order again.** Two defects made `betterRemote` return orderings that depend on
  the order of the roster it is folded over — Go requires a strict weak ordering from any
  comparison used to order a set, transitive incomparability included:
  - the 0.132.2 tie-break chose `work_util_pct` vs `gpu_util_pct` per COMPARISON, so a mixed fleet
    mid-rollout ranked the same three nodes on two different metrics depending on who was being
    compared, and cycled. `placementUtil` now picks the figure per NODE.
  - the P2C near-tie draw hashed the SEED ALONE, so it answered the same whichever way round it was
    asked: `betterRemote(P, Q)` and `betterRemote(Q, P)` were both true, and the fold kept whichever
    near-tied seat came last in the roster. The draw is now a bounded jitter on the seat's own eta,
    derived from the seed AND the node id, so the ordering is total by construction while near-tied
    seats still come out differently per Run. It spreads across any number of tied seats, not two.
- **An unmeasured seat is ranked, not parked.** A node publishing no `seat_rate` used to make its
  comparisons fall back to window for that PAIR only — the same per-pair defect, and the third
  source of cycles. It is now ranked as if it ran at the FLEET'S MEDIAN published rate, keeping its
  own backlog and cold load, exactly as `agent.assumedPrefillTokS` assumes a rate for a stall wall
  until one is measured. A constant would have been either "unmeasured wins everything" or "never
  gets work, so never earns samples"; the median is neither and adapts as the fleet does. With no
  node publishing a rate there is no prior, every seat is unranked, and the ordering falls to
  window for all of them — the original rule, applied uniformly.
- Tests: a brute-force strict-weak-ordering check over a 16-node roster (measured and unmeasured,
  four window sizes, backlogs, cold loads) across 40 seeds; both new guards verified to go RED on
  the defects they pin.

## [0.132.2] - 2026-09-21 - placement scores the cards the harness can use, not the operator's screen (ADR 0057)

- **The other half of 0.132.1.** That release stopped the GPU lease verdict from calling the
  operator's desktop the holder's work, and named the same bug class on the node's
  `gpu_util_pct` — which `delegate/gate.go` uses to BREAK PLACEMENT TIES — without fixing it. A
  Windows node whose operator was gaming advertised that load and lost ties to a node it should
  have beaten. This fixes it.
- **Two questions, two figures.** `gpu_util_pct` keeps its documented meaning (the busiest card on
  the box; the fleet overview and the PAIR multi-GPU rule want exactly that, and changing a field's
  meaning under its readers would break a stated contract). New additive `work_util_pct` +
  `work_util_known` is the busiest card the harness can actually run a seat on. The tie-break uses
  `work_util_pct` only when BOTH nodes publish it, so a mixed fleet mid-rollout never compares one
  node's desktop-free number with another's desktop-inclusive one.
- **One rule, in the leaf.** `gpuprobe.DisplayCardUUIDs` now decides which card is a display card,
  and BOTH the lease verdict and the node's placement figure call it — a second copy of the rule
  would have been a second chance to get it wrong, which is how 0.132.1 left this half standing.
  Evidence, not config: on Windows/WDDM `--query-compute-apps` reports graphics processes with
  `[N/A]` memory; on Linux it lists only CUDA processes with real memory, so nothing is flagged.
  A display card is excluded only when a non-display card exists (a single-GPU box runs its seats
  there by necessity).
- **An engine the harness launches never marks its own card.** `[N/A]` memory is a WDDM property,
  not a graphics-process property — nvidia-smi cannot size ANY process there, compute included. On
  `<node-b>` the harness's seats happen to be invisible to that query (llama-swap runs in session 0,
  the fleet node in the operator's session), but that is a session accident: on a node where they
  share one, a CUDA seat would look exactly like the desktop and its card would drop out of the
  placement figure, making a BUSY node advertise itself as idle. Over-flagging sends work to a
  loaded box; under-flagging only costs the tie the node used to lose — so the rule now reads the
  process name and needs positive evidence of something that is not ours. Found by an adversarial
  review of this change, and confirmed against the live driver.
- **Cadence:** the display set refreshes every 15 ticks of the 2 s sampler (30 s), not every tick —
  which card drives the desktop is hardware plus a login session, not something that changes between
  health polls — and a failed probe keeps the previous set rather than re-counting the desktop.
- **ADR numbering repaired.** The llama.cpp prompt-cache record was filed as a second **ADR 0055**
  while ADR 0055 (liveness walls, PR #424) already existed on main; both numbers were live in code
  and docs at once. The prompt-cache record is now **ADR 0056** and its 17 references moved with it;
  the liveness record keeps 0055. `TestADRNumbersAreUnique` makes the collision unmergeable and also
  requires the index to link every record — which caught four records (0028–0031) that had silently
  never been indexed.

## [0.132.1] - 2026-09-21 - the lease verdict counted the operator's own screen as the holder working

- `held-working` now requires a busy card the harness can actually run a seat on. It used to take
  the MAX utilization across every card, so on a box anyone was using the desktop or a game kept
  the verdict pinned at `held-working` — and because that outranks `held-idle`, the verdict built
  for a stalled holder could never fire there.
  Measured on `<node-b>` 2026-09-20 while the operator played a game: `gpu status` read
  `held-working — 33% on card 1 (RTX 5070 Ti)` while the lease holder had spent **4 seconds of CPU
  in 141 minutes**, had spawned nothing, and the two cards it actually fenced sat at 0%. Card 1 is
  the display card and that 33% was the game. Three jobs queued behind a holder doing nothing, and
  telling that apart took reading the holder's CPU time and children by hand.
- Which card is a display card is taken from EVIDENCE, not config: on Windows/WDDM nvidia-smi
  reports graphics processes under `--query-compute-apps` with `[N/A]` memory, and those are the
  desktop. On `<node-b>` all 28 such rows sat on card 1 while the harness's own resident seat on
  card 0 produced no row at all. On Linux that query lists only CUDA processes with real memory, so
  nothing is flagged and every card stays eligible — unchanged everywhere but the case this fixes.
- A display card is excluded ONLY when a non-display card exists. A single-GPU box (a laptop, an
  iGPU node) runs its seats on its display card by necessity, and excluding it would have made
  `held-working` unreachable there and turned a genuine bench into `held-idle`.
- `held-idle` now names what IS using the box ("the cards ARE busy (33% on card 1) but that is the
  display card … so it is not the holder's work") and reports utilization "on the cards it can run
  on", so nobody re-derives why a busy machine reads idle under the lease. `busy-outside` still
  scores every card, which is correct: unleased, the game IS the answer to "is anything using this".
- KNOWN, NOT FIXED HERE: the same bug class exists on the fleet node's health `gpu_util_pct`
  (`internal/fleetnode/server.go`, the busiest-device loop), which `internal/delegate/gate.go`
  uses to break placement ties — so a Windows fleet node whose operator is gaming advertises
  inflated utilization and loses placements it should win. Fixing it needs its own design (score
  the devices the node's layers declare, or add a process sample to the background refresh) and
  its own tests in a path polled every few seconds; it is not folded in silently here.

## [0.132.0] - 2026-09-21 - the node can save and restore a seat's KV to disk — and the measurement that says it buys nothing yet (ADR 0056 Layer 2, node side)

- `POST /fleet/kvslot/save` and `POST /fleet/kvslot/restore` on the fleet node, bearer-gated like
  the chat lane, proxy llama-server's slot API through llama-swap. The key is `k1-<sha256 hex>` and
  nothing else, so the file name it derives cannot escape the slot directory; an optional
  `seat_pin_sha256` that disagrees with the live `/props` pin is a `409`, so a file is only ever
  restored into the identical seat shape. Absent file = `404` "miss"; a seat rendered without
  `--slot-save-path` = `501`, which is how an older node tells a caller to fall through. A 2xx whose
  body does not parse, or which reports zero tokens, never reads as `ok` — a caller that believed
  either would skip a prefill it never restored.
- Every llama.cpp chat/agent seat (and its CPU twin) renders `--slot-save-path <home>/kvslots/`;
  whisper, embedding and reranker entries do not. Both installers create the directory; an LRU
  sweeper bounded by `kvslot_cap_gib` (0 = 8 GiB) runs after every save. Health advertises `kvslot`.
- **Layer 1 tier map corrected before it could bite:** 1024 / 8192 / 16384 / 24576 MiB, under a floor
  rule that the map may never hand a box less than llama-server's own 8192 default unless its RAM
  cannot carry it. 0.131.3 shipped 1024 / 2048 / 6144 / 12288, which silently CUT the 32 GB and 64 GB
  nodes below the default they had been running on — sizing per tier in the losing direction. Only the
  min tier (a 4 GB box) stays under the default, which is the case the map existed for.
- **Layer 1's size is now measured and it is worth 18x.** A, B, C, A with ~11.6k-token prompts on
  `<node-f>`'s 32k seat: at `--cache-ram 1024` the repeat of A re-processes all 11,606 tokens
  (110.8 s), at 8192 it re-processes 516 (6.2 s). The cache only matters when several contexts
  share a seat — the shape of a delegation stream — and the 0.131.3 map put the fleet's two 32 GB
  nodes at 2048, between those arms. That is what 0.131.4's floor rule prevents; ADR 0056 carries
  the table.
- **The `--slot-save-path` flag is NOT rendered, from a second measurement.** A rendered path that
  does not exist makes llama-server refuse to START (`not a directory`), which would take every
  chat and agent seat on that node down with it — and the node's slot directory (`OFFLOAD_HOME`,
  runtime) and the rendered flag (`--home`, render time) are never tied, with the Windows
  fleet-node launcher setting neither. Separately, `qwen3.8-27b-par8` on both Blackwell pair
  templates runs `--parallel 8`, where slot 0 holds whichever of eight requests last touched it.
  Neither risk is worth carrying for a capability measured at zero, so the installer self-test now
  asserts the flag is ABSENT; the plumbing stays wired behind one function.
- Four endpoint defects fixed from an adversarial review: the siblings' Content-Type gate (a
  cross-origin `fetch` sends text/plain — a CORS simple request, no preflight, side effect lands);
  `health`'s `kvslot` is the lane's real admissibility predicate, not "a directory exists"; the
  lane refuses to start a COLD seat, because `/upstream/<seat>/…` starts one and that would defeat
  the 5-minute idle unload, ignore a drain or a GPU lease, and save an empty slot over a good file;
  and the post-save sweep never evicts the file that save just wrote.
- **MEASURED INERT on b9934 and the delegator side is BLOCKED because of it.** On `<node-f>`, a
  restore of 3,231 tokens (151 MB, 25 ms) leaves the next identical request paying the full
  27.2 s prefill — same as no restore, in all three call shapes — while the same run's control
  shows the 0.131.3 RAM cache cutting 27.2 s to 4.6 s. `GET /slots` shows why: a restore
  populates the KV cells and none of the prompt bookkeeping the prefix matcher reads. Upstream
  ggml-org/llama.cpp #25913, fix open in #26004. The endpoints ship because they are correct and
  verified; nothing calls them, and nothing will until that fix lands and the ADR's table is
  re-run with the restore arm beating the baseline.
## [0.131.4] - 2026-09-21 - the admission-budget test asserted a sentence, not the behaviour

- `TestRunAgentTaskWarmUpIsBoundedByTheAdmissionBudget` required the note to contain
  "exceeded the admission budget". The window probe runs first and is charged to admission
  (`admitted += time.Since(probeStart)`), so against an upstream that sleeps past the budget the
  PROBE spends it and the warm-up correctly reports "no admission budget left for the warm-up"
  instead. Both notes are true and both proceed into the wall, which is what the test's own doc
  comment says it is for — so which sentence appears came down to how the runner split 3 s between
  the two consumers. It passed on the CI runner for #425 and failed 3/3 on a developer machine and
  again on CI afterwards, leaving `build` red on main.
- The assertion now requires the note to name the ADMISSION BUDGET, which every legitimate path
  does and an empty or budget-silent note does not; the timing bounds are unchanged. Verified by
  mutation: swapping the substring for one no note contains turns the test red.

## [0.131.3] - 2026-09-20 - llama.cpp prompt cache sized per RAM tier (ADR 0056, Layer 1)

- Every llama.cpp seat renders `--cache-ram __CACHE_RAM__` from the new top-level profiles map
  `cache_ram_mib_by_ram_tier` (min 1024 / low 2048 / mid 6144 / high 12288 MiB; starting values,
  measured and revised by Task 3 of the prompt-cache plan). llama-server's host-RAM prompt cache
  had been running at its fixed 8192 default on every box; a 4 GB laptop and a 64 GB node (high RAM tier) got
  the same figure. An unset figure renders 8192, never 0 (`Params.cacheRAMMiB()`), so an older
  caller cannot switch the cache off by omission. vLLM seats are untouched (LMCache, ADR 0045).
- `render.tests.ps1` asserts the flag on the ampere-8/mid row; `TestCacheRAMFollowsTheRAMTier`.
- `amd-gcn` window 8192 → 32768 (`ctx_size` + `agent_ctx_tokens`), measured on `<node-f>`: a 23,889-token
  prompt in 277.5 s at 86 tok/s on the Vulkan agent seat with 3.9 GiB of GTT, and 8.4 s on the repeat
  (the RAM prompt cache at work). The installer self-test row follows.
- Plan: `plans/2026-09-20-llamacpp-prompt-cache-tiers.md` (operator-approved 2026-09-20); Layer 2
  (harness-driven `--slot-save-path` SSD save/restore) follows in its own releases.
## [0.131.2] - 2026-09-20 - PAIR cards: a remote job's card stays on the node that ran it

### Fixed
- **Every completed remote delegation was shown in PAIR's Jobs list as running on the delegator.** The in-flight frames named the node by its dispatch host (a PAIR member name, 0.126.1), but the terminal frame named it by what the node reported on the wire — its configured `fleet_node_id` — which PAIR's `members.json` cannot resolve, and an unresolvable name fell back to *this box*. So the card was right while running and re-pointed at the delegator on completion (measured 2026-09-20 in the PAIR history: ~300 cards of remote-only seats attributed to the delegator; the operator saw the connection line land on the wrong node). A node dispatched by an address literal never resolved at all and showed on the delegator for its whole life.
- The emitter now resolves a node from **all the names it goes by** (`Event.NodeAliases`: the dispatch host, the fleet node id, the wire name) against PAIR's member names **and member addresses**, remembers per job what the in-flight frame resolved to so the terminal frame keeps the card there, and when nothing resolves stamps **no node** (`scheduledOn: null` — PAIR draws no line and names no node) instead of the delegator, logging the unresolved name set once. `pairTerminal` names the node the in-flight frame named and demotes the wire name to an alias.

### Tests
- `TestResolvesAliasesAddressesAndRemembers`: fleet id + hostname alias, address literal, in-flight resolved then terminal unresolvable (card stays; memory dies with the terminal frame), nothing resolvable (no node, not self), local (self). The old "unknown name falls back to self" expectation is inverted.

## [0.131.1] - 2026-09-20 - Liveness walls: the unmeasured prefill prior was 4x too optimistic, and vLLM seats were never measured

### Fixed
- **The first live proof of 0.131.0 filed a FALSE STALL on the `<node-c>` 27B GSQ seat** — `stalled: no progress for 94s in prefill (allowed 94s: 17041 tok / 400 tok/s assumed x 1.5 + 30s)` — while the seat was prefilling ~12k uncached tokens at its real ~130 tok/s. Two defects: the unmeasured prior (400 tok/s) was 3-4x too optimistic for a 3-bit GSQ seat on an A2, and `prefill_tok_s` could never be MEASURED on a vLLM seat because the prefill accounting read llama.cpp `timings` only. Now the prior is 100 tok/s, and every streamed completion records its time to first delta (`Completion.FirstDeltaMS`); the loop publishes per-call `(uncached prompt tokens, first-delta ms)` samples on `Result.PrefillSamples`, and the pipeline folds the largest into the seat-rates store on EVERY run, deferred ones included — the run a false stall costs is the run that measures the seat.
- **`tok_per_s` on the ledger was still 0 on short runs** — the column came only from the calibrated seat rate (>= 1,024-token completions). The wire now carries `observed_tok_s` (the liveness monitor's smoothed rate over the run's deltas) and both ledger rows prefer the calibrated rate, then the observed one; the seat-rates store still reads only the calibrated sample.

## [0.131.0] - 2026-09-20 - Liveness walls: a run is ended by a stall or a ceiling, never by its wall expiring while the seat produces

### Changed
- **Liveness walls (ADR 0055): a contract is ended by a STALL or by a safety CEILING, never by its wall expiring while the seat is still producing.** Every seat completion now streams (`stream: true`, `stream_options.include_usage`), and each delta is a progress event; the wall (`timeout_sec` / the auto wall) is the EXPECTATION, still reported as `wall_sec`, no longer a kill. The stall allowance is dynamic per phase — prefill at the seat's measured prefill rate (new `prefill_tok_s` in the seat-rates store), decoding at 20 deltas of the decode rate, a tool's own cap + slack, all floored at 60 s — and a stall is filed `stalled: …` as infrastructure with the arithmetic in the reason; the ceiling is `max(3 × estimate, 2 × wall, 1800 s)` capped at 4 h and filed `ceiling …` as budget. `wall timeout after Ns` is retired from the node's vocabulary. Telemetry: the run registry and `gpu status` / `offload_status` carry `last_progress_ms`, `tok_s`, `live_phase`, `allowance_ms` beside the process heartbeat (C-32); `/fleet/jobs/{id}` publishes `progress` + `stall_allowance_sec` + `ceiling_sec`; the wire result carries `ceiling_sec` / `stall_allowance_sec` / `last_progress_ms`; the ledger's `agent_delegate` row fills `tok_per_s`. The delegator polls while the node reports progress, bounded by the node's ceiling. Measured cause: on 2026-09-20 every wall timeout in the ledger was a producing job (949 tok in 984 s on the `<node-c>` 27B; 6,236 tok in 769 s on the `<node-b>` pair).
- **vLLM-CPU measured on the first Linux AMD node** (`cpu` and `amd-gcn` notes): builds and seats only with `--enforce-eager` + a 4 GB KV pool on a 30 GB box, at 3.1 tok/s vs 9.27 for llama.cpp CPU on the same family — recorded, not seated. (#422)

## [0.130.8] - 2026-09-20 - amd-gcn: flash-attn ON, measured; the iGPU lane's optimization pass is on the record

- `amd-gcn` renders `--flash-attn on`. The tier had carried `off` on an untested "older Vulkan FA unreliable"
  claim; two GCN boxes now say otherwise at b9934 — neutral on the Lucienne laptop (2026-07-17) and
  +4 % pp512 / +1 % tg128 on `<node-f>` (2026-09-20: 266.2/23.1 vs 256.1/22.8, E2B UD-Q4_K_XL).
- The tier notes carry the research-first optimization pass (RADV guide #23295, Vulkan thread #10879, the
  Vega APU toolkit, the amdttm GTT posts) and its measured sweep: `RADV_PERFTEST=nogttspill`, ubatch
  128..1024, q8_0 KV, DPM `high`, ryzenadj fclk pins — every knob within ±4 %; the lane is
  DDR4-2667-bandwidth-bound. The whisper.cpp Vulkan STT seat is recorded as built and live.
- Prose surfaces that said "off for amd-gcn" (CLAUDE.md, ADR 0002, setup-installer, SETUP-AGENT, the
  OPERATOR-GUIDE tier table, both Vulkan template headers) now say on; `docs/tiers/` regenerated.

## [0.130.7] - 2026-09-20 - amd-gcn gets the Qwen3.5-4B agent seat (measured on the first Linux AMD node)

### Changed
- **`amd-gcn` declares `include_qwen35_4b: true`.** On `<node-f>` the fleet's grounded smoke contract fails on the tier's
  gemma4-e2b planner (no tool call, 18.6 s) and on the offload-e4b workhorse (same shape, 31 s), and passes on the
  Qwen3.5-4B agent seat (44.7 s). Rendered configs for this tier now carry `qwen3.5-4b-agent`; the installer downloads its
  GGUF. Nodes on this tier should publish `agent_seat_tok_s` (10 on `<node-f>`) so the wall is sized from a rate — the first run
  without one deferred at the grammar re-pack with 1 s of wall left. (#421)

## [0.130.6] - 2026-09-20 - the first Linux AMD node: detect, templates and a Linux GPU memory provider

### Added
- **Linux serving templates for the `vulkan` and `cpu` backends** (`setup/templates/llama-swap.linux-vulkan.yaml`,
  `llama-swap.linux-cpu.yaml`). A tier is a hardware class, but both backends shipped Windows templates only, so a
  Linux AMD box carrying the MEASURED `amd-gcn` tier could not be installed (`install.sh`: "no serving template for
  linux/vulkan"). `TestVulkanAndCPUTiersRenderOnLinux` keeps every vulkan/cpu tier rendering on both OSes with its
  seats present. (#416)
- **`linux-amdgpu` GPU memory provider** (ADR 0053): `fleet-serve` on Linux reads `/sys/class/drm/card*/device/mem_info_*`
  — VRAM plus, on a UMA profile, the driver's own GTT pool as the shared budget — instead of refusing with "no working
  GPU memory source". The generic source is now named per OS (`linux-amdgpu` / `windows-generic`) in the banner and
  the gate error; `ResolveProvider`'s signature and label are unchanged. (#417)

### Fixed
- **An unrecognised `lspci` device name erased the PCI vendor the Linux probe had already read**, classifying a Ryzen
  APU ("Barcelo") as profile `cpu`. The probe now keeps the PCI-id vendor as a hint, and the arch/vendor rules know the
  Vega-era APU codenames (Barcelo, Cezanne, Lucienne, Renoir, Picasso, Raven). (#415)
- **`install.sh`'s fleet-node unit never exported `OFFLOAD_HOME`**, so the node resolved `$HOME/offload-stack/installed.json`,
  found no manifest and advertised `vendor=unknown` with a non-UMA composition. (#417)
- **`seat_closure_test.renderParams` never set `Backend`**, so every closure render treated the `cpu` tier as a GPU tier
  (seats gained `-ngl`/`--flash-attn` the installer never emits). (#416)

Measured on the first node (`<node-f>`, Ryzen 5 5625U / Vega 7, Ubuntu 26.04, llama.cpp `b9934` release builds, 512 MiB
carve-out): Vulkan E2B pp512 256 / tg128 22.8 t/s, E4B 129 / 11.9; CPU E2B 109 / 21.5. Six protocol tasks 0/6 deferred.

## [0.130.5] - 2026-09-19 - a forwarded accelerator call's PAIR card names the device and the node that ran it

### Fixed
- **A forwarded accelerator call was recorded as `<device>@fleet` even when a node answered**, so its PAIR Jobs
  card read "llamacpp, ran on <this box>": `accelremote` stamps the result with a typed `Placement`, and the
  ledger helper only recognised a JSON map, so the node was never seen. The helper now reads the typed value (and
  the map form), and names the node by the host of its base URL — the name PAIR knows the member by — so the
  ledger row is `<node>:<device>` (E-04 as documented). Red test: a typed placement whose base is
  `http://node-b:18811` must yield `node-b`.
- **The PAIR card for an NPU call carries the device as its engine** (`coral-edgetpu`, `hailo-8l`) instead of
  `llamacpp`, and `FromLedger` splits `<node>:<device>` so the card shows the device as its model and runs on
  that node (`scheduledOn` resolves through PAIR's member list). Red tests: `TestEngineFor` accelerator rows,
  `TestFromLedgerAcceleratorRowNamesDeviceAndNode`.

## [0.130.4] - 2026-09-19 - doctor checks the model files behind every ComfyUI route (register F-31); the L1 staging default is the measured 2 GB (register B-02)

### Added
- **`doctor` resolves every configured ComfyUI model name against the class directory its loader node opens**, across `<comfy_dir>/models` and every `extra_model_paths.yaml` root (base_path plus ComfyUI's newline-separated alias lists such as `diffusion_models | unet`): `FOUND`, `MISSING` (no class directory holds it) or `MISPLACED` (present only under a class the graph never loads from — the 2026-08-31 disk-swap shape, when a DiT and a LoRA landed in the wrong class directory and every krea2 file was found by a graph rejection at render time). A subfolder name resolves as the loader opens it; a file that moved one level is still reported in its class. `MISSING`/`MISPLACED` fail doctor like a bound-but-missing route. A box with no models root prints no section. `internal/mediacap.ModelBindings` / `ModelRoots`; the expected-class table is derived from the shipped `render/wf-*.mjs` loader nodes. Mutation-checked: counting a misplaced file as found turns the test red.

### Changed
- **`l1_staging_gb` / `SEAT_L1_GB` default 8 → 2** (register B-02, `Benchmarks and Optimizations/2026-09-18-b02-l1-staging`):
  on the pair seat, 8 / 4 / 2 GB arms (fill + restore against one NVMe store, 24 real contracts) restore the same
  context — every graded contract passes at every size, 0 preemptions, tier-served tokens within the metric's
  noise — at 9.1 / 5.1 / 3.1 GiB of MP-server RSS. The smallest at parity is the default (INV-17); the pair profile
  (`blackwell-2x16`) carries 2, the 3-card same-box tier keeps its sized 32. A deployed `seat.env` keeps its
  explicit value until it is re-rendered; the production pair seat moves after an H-24 soak.

### Fixed
- **The re-pack on a vLLM seat sends the contract's own schema** (register D-129 follow-up, found by the A-100 proof contract):
  `structured_outputs.json` carried the GBNF-typed projection of the schema, and `internal/gbnf` has no object or
  array-of-object type, so a contract whose schema nests objects was constrained into strings on every vLLM seat
  (three re-pack attempts "got string, want object"). vLLM accepts full JSON Schema; llama.cpp seats keep the GBNF
  projection. Red test `TestRepackSendsTheContractsOwnSchemaToAVLLMSeat`.

## [0.130.3] - 2026-09-19 - accelerator calls reach the savings ledger (register E-04)

### Added
- **Every accelerator MCP call writes one ledger row.** The Coral/Hailo tools bypass the pipeline (the sidecar or a fleet node answers directly), so since 0.114.0 nothing they ran reached `ledger.jsonl` or `offload_status`'s savings: `pipeline.RecordAccel` now records task = the sidecar tool (`classify_image`, `object_detect`, `semantic_segment`, `image_embed`), `model_tier` = the device id, `<node>:<device>` for a forwarded call that a fleet node answered, or `<device>@fleet` when the forward failed before a node answered, plus latency and the defer verdict with the caller's reason text. An empty required argument is refused before anything runs and writes no row. Mutation-checked: dropping the fleet record line turns the new test red.

## [0.130.2] - 2026-09-19 - `route` on agent_run and offload_ask: a call from this box can name where it runs

### Added
- **`route` on `agent_run` and `offload_ask`** (register C-46, diagnosis S-27): both doors always ran local, so a
  remote seat could not be named from this box at all (the other face of D-125). `route` = `remote` / `auto` /
  `spread` / `queue` sends the call as ONE contract through the delegator's single-contract path (the one the
  review fence built), and the response names `node`, `placement`, `seat` and `executed_on`. For `agent_run`,
  `read_root` and `model` do not travel — the executing node reads its own root and runs its own seat — so it is
  for self-contained goals and `setup_actions`; for `offload_ask` the files ride inline, so any node can answer.
  Omitted or `local` keeps the old behaviour exactly.

## [0.130.1] - 2026-09-18 - the store steward ticks on a clock, not only on completed jobs

### Fixed
- **The KV store steward could not fire while a GPU lease kept jobs off the node** (register B-48 / B-16, L5): its
  scan ran after every `fleet_store_prune_every_jobs` completed jobs or on a health poll above the high mark, and under a
  lease no fleet job completes — exactly when a bench arm or the pair seat writes pages at ~1 GB/min. Measured
  2026-09-18 on `<node-c>`: a tick at 20:26, then the dataset went 53 → 71 GB (its quota, `ENOSPC`, 15 failed store
  tasks on the A2 seat) by 20:54 with no tick between. `fleet_store_prune_every_sec` (default 60, negative disables)
  adds a time tick (`storesteward.Every`); a tick below the high mark is one directory walk and one statfs.
  `TestEveryTicksOnTimeAndStopsWithTheContext`.

### Added
- **The decision margin can be measured over its FULL denominator, behind a flag** (register D-130, ADR 0051).
  `internal/confidence` normalised the top-1 vs top-2 gap over MATCHED class tokens only, so mass on unmatched
  tokens and on legal labels outside the `top_logprobs` window never entered the denominator; research measured the
  declared (matched) mass at ~0.097 on llama.cpp, i.e. the live classify margin p50 of 0.985 is inflated about
  tenfold and the gate under-escalates exactly on hard, many-label contracts. `confidence_margin_full_denominator`
  (default **false**) selects the full scale and `confidence_margin_threshold_full` (default **0**) is its own
  threshold - 0 means the margin gate never fires there, announced once per process; the matched-scale 0.65 is never
  applied to the full scale, and a matched-scale value in the full key is warned about at load. Every row now carries
  `margin_scale` (`matched` | `full`; empty on an old row reads as matched), `margin_declared_mass` and
  `margin_ambiguous` (alternatives credited to no class because they prefixed more than one), the last two recorded
  in BOTH modes so a full-scale threshold can be re-derived before anyone flips the flag. `health`, the exemplar
  harvest gate and conformal `calibrate` each filter to one scale and report the rows they excluded. With the flag
  off nothing changes but the new omitted-when-zero row fields.
- **A cap on the runs started on the box itself** (register C-42, diagnosis S-07 / W-09). The fleet capped the
  jobs it sent to a node (`fleet_max_concurrent_jobs`, "queue full" 503), but nothing capped the runs the box
  started on its own seat — `agent_run`, a delegation's local leg — and the run registry "never gated anything
  by itself". `modelaffinity.AwaitSeatSlot` waits, inside the admission budget, while the registered runs on
  the seat (its own record excluded) number the cap or more; a slot that never frees is a capacity defer
  (`seat busy: …`, re-placeable), never a refusal. Wired at the MCP `agent_run` door with the same cap the
  fleet uses (`FleetConcurrencyLimit`, default 4; `fleet_max_concurrent_jobs: -1` disables both). The
  pipeline's contract runner (`internal/pipeline/agenttask.go`, the delegation's local leg and every fleet job) takes the same
  call in this same cut (the pipeline's contract runner, 0.130.x).

### Fixed
- **A warm-back gave up on a 5xx while the seat was still loading** (register D-124 readback, 2026-09-18
  19:5x): the 3-card seat's cold load outlasts llama-swap's `healthCheckTimeout`, the warm's health request
  came back `status 500`, the wrapper reported the warm-back failed and released — and the engine came up
  minutes later, untracked by the lease that owed it. `warmSeat` now reads the seat's own state after a 5xx
  (`/running` through `seatload`): a load in progress is waited out (15 min bound, 5 s poll), a ready seat is
  a warm that succeeded, a seat that never started is the failure, said as such.

## [0.130.0] - 2026-09-18 - the decide lane's plumbing and the cascade's doors: calibrate reads the labels sidecar, the escalating attempt records its row, vLLM seats get structured_outputs, every row names its door, the cascade tools are the first door

### Fixed
- **A margin escalation is now written to the ledger** (register D-127): the confidence gate's defer returned before
  `p.record`, so the escalating attempt — a call the seat answered — left no row of its own; `esc_source` counted 4 rows
  in 9,217 for 43 real firings that lived only in the labels sidecar. The escalating attempt records its row (its tier,
  its margin, the gate that fired, not deferred) before the defer, under the same `record` gate as a success. Red test
  `TestMarginEscalationRecordsTheEscalatingAttempt`.

### Fixed
- **A route=local run against another box's engine was attributed to this box** (register C-58, operator
  2026-09-18: "nvidia pair showing `<node-b>` doing `<node-c>` work"). Bench configs on `<node-b>` set `endpoint` to
  `<node-c>`'s vLLM arm (`http://node-b:18797`, `agent_model: a2-pool`); the delegator ran the loop
  here against that engine and stamped the ledger row and the PAIR card with its own hostname — 20 cards on
  `<node-b>`'s UUID for work `<node-c>` did (measured: the arm's `prompt_tokens_total` rose by the gate's
  87,354 tokens, the `<node-b>` seat served none). `modelaffinity.EndpointHost` now names the endpoint's box when
  it is not loopback, `localhost` or this machine; a local run then carries that host as its node, its
  placement reason says `engine <endpoint> is <host>'s (attributed there)`, and the PAIR card is scheduled on
  that member. The same reading disarms this box's text-load gate in `config.Load` (one note per process):
  a run that never touches a local card is no longer deferred `gpu busy` by this box's lease (8 of 16
  contracts were, in 0.7 s, while `<node-b>`'s cards sat under a lease and the work was on `<node-c>`).

- **`calibrate` had never fitted a threshold, and the cause was file plumbing, not the 60-row floor**
  (register D-126): the only classify/triage label writer appends to the confhead labels sidecar
  (`confhead_labels_path`) while `calibrate` read the ledger alone, whose rows of the same calls carry the
  margin and no label — 0 of 9,217 live ledger rows passed its filter. `calibration.RunSources` reads every
  labeled-row source (ledger + sidecar), a missing file is a 0-row source, and the report names each source
  with its usable-row count. Red test `TestRunSourcesFitsFromTheLabelsSidecar`.
### Verified
- **A vLLM seat's logprobs reach the confidence gate unchanged** (register D-128, measured 2026-09-18 on the `<node-b>` pair seat):
  vLLM's `/v1/chat/completions` answers the OpenAI `logprobs.content[]` shape the client decodes; the legacy
  `top_logprobs` list-of-dicts belongs to `/v1/completions`, which the harness never calls. Pinned by
  `TestVLLMChatLogprobShapeDecodesIntoTopAlternatives` and `TestMeasuredVLLMTokenStreamYieldsAMargin`.
### Changed
- **The cascade tools declare themselves the first door for single-shot mechanical text** (register A-102 (a)): the
  `offload_summarize` / `offload_classify` / `offload_extract` / `offload_triage` descriptions and `agent_delegate`'s
  opener state the door order (one text + one mechanical question → the cascade; multi-document read-and-reason →
  a contract), because the caller reads the tool text and nothing else. Measured 2026-09-18: the cascade carried
  0.12 % of 14 days' wall while the agent doors carried 98.2 %; the operator reads that as a routing defect.
### Added
- **Every cascade ledger row now names its door** (register A-102 (e)): `door` on the request, the meta and the ledger row —
  the MCP tool name (`offload_summarize` …), the CLI command (`cli:summarize` …) or `fleet` for a request a node received from
  a delegator (a forwarded request keeps its origin door); agent-contract rows carry the contract's door on the wire
  (`agent_delegate`, `offload_ask`, `offload_review_diff`, `offload_research`, `cli:delegate`, `cli:research`, `fleet`)
  and the composite sub-calls keep their parent's. Before this all 558 cascade rows in the live ledger carried no
  caller at all, so "which door produced this call" could not be asked of the telemetry. Additive and `omitempty` on every
  surface: a node one release behind decodes the request unchanged.
### Fixed
- **A vLLM seat is no longer constrained by a GBNF grammar it discards** (register D-129, ADR 0002
  amendment 2026-09-18): the seats `vllm_seats` declares — matched case-insensitively and
  **alias-resolved through the live llama-swap roster**, so `<node-b>`'s `agent-pool-3card` alias of
  `qwen3.8-27b-vllm-3card` counts — now receive vLLM's own `structured_outputs: {"json": <schema>}`
  and **no `grammar`** on both send sites (the in-loop tier path in `attempt`, the structured re-pack
  in `repackStructured`), plus the non-thinking render, since vLLM applies the constraint to the whole
  output. llama.cpp seats keep the raw GBNF byte-identically, and an unreadable roster keeps the
  grammar (the pre-fix behaviour) rather than stripping a constraint on a transient probe failure.
  vLLM's request model allows unknown extras, so it accepted `grammar`, ignored it, and answered
  unconstrained; the 0.115.14 repair was a downstream trim and coercion, and nine days of the
  delegation log measured its cost as 1,018 structured re-packs on vLLM seats against 506 on every
  other seat, with 3-attempt exhaustion at 19.7 % against 12.6 %. `response_format` stays unused on
  every engine. New: `llamaclient.WithJSONSchema`, `gbnf.JSONSchema` (round-trips with
  `FromJSONSchema`, declaration order and all), `tasks.Built.Fields`,
  `config.Config.DeclaresVLLMSeat`, `pipeline.isVLLMSeat` (60 s per-name memo). Ledger rows and
  `core.Meta` gain `repack_attempts` beside `repack_ms` — the wall alone cannot separate one slow
  attempt from a three-attempt loop, and the attempt count is what this change is measured on. Red
  test first:
  `internal/pipeline/vllm_structured_outputs_test.go:TestGrammarNeverReachesADeclaredVLLMSeat`.

## [0.129.2] - 2026-09-18 - the lease hand-off is ordered: the warm-back belongs to the last holder, a lost lease never warms, the seat unit never restarts itself
- **A failed drain left the DRAINING stamp on a held lease** (register C-50, diagnosis S-31): the detach form keeps
  the lease after a drain that misses its deadline, and the stamp cordons the seat — no new run admitted — for the
  rest of the window, so one failed drain refused the box for up to 8 h. `maintainSeat` now clears `Draining` on the
  failure path (the lease stays held and non-exclusive, the caller decides); the wrapper form releases as before.
- **A run that heartbeats without progressing held the drain for the whole queue budget** (S-32): the drain's
  overall deadline is the `--wait` budget on purpose (ADR 0041, a legitimate 12-step run is never cut), but it had
  no bound on a seat whose state never changes. The drain now gives up when the busy state — in-flight count, runs
  at the same step and phase — is unchanged for two seat turns plus the cold load, derived from the seat's own
  rate sample (`seatStuckAfter`, floor 2 min, disabled without a sample), and says so in words distinct from the
  deadline error. Progress (a step advance, an in-flight change, a load finishing) resets the bound.

### Fixed
- **A releasing holder's warm-back raced the next lease's `--unload-seat`** (register D-124, `<node-c>`, 2026-09-18
  02:57): a wrapper whose command had been cut warmed the agent seat after the next queued lease had already taken the
  card, drained an "idle" seat and unloaded it; the unload killed the engine, the seat unit's `Restart=on-failure`
  brought it back 20 s later on the new holder's EXCLUSIVE card, and three measurement rows read the seat's 10 GiB
  as their own fit. Now: a queued `Acquire` registers itself under `<state>/gpu/waiters/` for as long as it polls
  (pruned on read when the pid is gone; `gpu status` lists them as `queued:`, `offload_status.gpu_lease.queued`
  carries the count); an unload stamps `<state>/gpu/seat-warm-owed`; a warm-back runs only while the card is still
  ours (the wrapper checks its epoch, `gpu release --warm-seat --epoch N` checks the record is still N) AND nobody is
  queued behind us — with a waiter it is skipped and said so, the successor unloads the seat again anyway, and the
  marker makes the LAST releaser pay the one warm; a holder that lost the card never warms; the warm is heartbeat for
  its length (a 27B load is minutes, the heartbeat TTL two) and losing the lease mid-warm cancels it loudly. A plain
  `gpu release` prints a note when a warm is owed. Tests pin the wire order (unload, unload, warm) for two queued
  `--unload-seat` leases, the no-warm after a lost lease, the heartbeat during the warm, and the same rule on
  `gpu release --warm-seat`.
- **The vLLM seat unit template restarted the engine on its own** (`Restart=on-failure`). `vllm-seat-cmd.sh` detaches
  the moment the unit's invocation changes, so a systemd relaunch is a seat llama-swap no longer tracks and no lease
  can order — the mechanism that put the 27B under another lease's window above. The template is now `Restart=no`
  (ADR 0035 amended); a crashed seat is reloaded by llama-swap on the next request, which the lease gate orders like
  any other load. Deployed by hand on `<node-c>`'s two seat units (backups beside them).
- **The drain could read no run at all between two steps.** The run registry rewrites a run's record on every
  step (tmp + rename, in-place on Windows when the rename meets a reader), and `Registry.List` skipped a record
  it caught empty, absent or torn — a 2 ms poll then printed a run-less `1 in flight` line (the print-cadence
  test failed 1 run in 5 on main) and, with the seat's gauge at zero between steps, would have counted the gap
  toward "drained". `readRecord` now waits a record out (4 × 5 ms) before treating it as unreadable; a young torn
  record is skipped, never listed and never removed.

## [0.129.1] - 2026-09-18 - fs_native bindings publish `reachable` from the seat wrapper's own verdict file

### Added
- **`kv_cache_server[].status_file` (register B-29).** An `fs_native` store is a mounted path with no port to dial, so
  `offload_status` published `reachable: null, "not validated end to end in this release"` for every such binding since
  0.113. The seat wrapper already decides the fact at every seat start (mount + 64 MiB write probe) and writes
  `$WORK/seat-l2.status`; a binding that declares that file (host-visible `//wsl.localhost/<distro>/…` on a WSL2 seat)
  now reads it: `ok` → `reachable: true`, `degraded` → `reachable: false` + `reachable_error`, unreadable → `null` with
  a note, undeclared → `null` with the instruction. `status_line` and `status_age_s` ride along.
  `TestStatusReadsFSNativeReachabilityFromTheSeatStatusFile`.

### Changed
- **`skill/` tool reference libraries refreshed from the operator's current copies and DaVinci Resolve added** (register J-22):
  `skill/ffmpeg` (12 files were a week behind: ffmpeg 9.0 traps, NVENC, subtitles, quoting, failure modes),
  `skill/gimp` (10 files: two hosts measured, text-outline correction), new `skill/davinci-resolve`
  (Studio 21.1 catalog, CLI + sidecar, playbook, render, failure modes; `PROVENANCE.md` beside it).
  Scrubbed by the evidence repo's script plus a supplemental pass for the editor's account, the
  private video-pipeline repo, the dev and cloud drive paths and a tailnet address; the identity lint
  passes. The unredacted originals stay in the operator's `~/.claude/skills`.

## [0.129.0] - 2026-09-18 - a contract can name the layer it runs on (the Linux edge node's fast digest seat by name); the digest-adr-hard-8 contract set

### Added
- **A contract can name the layer it runs on** (register A-100, the measured fast-seat lever): the
  delegation door's subtasks take `layer`, the placement table gains row 4b (a named layer → its
  agent seat, under the window check and the layer's own guards, naming the loaded seat it displaces
  on a shared card) and row 5b (a box that declares no pair places the free choice on the `single`
  layer's agent seat — the planner default under a layer name), and the delegator decides FOR a
  caller-named layer instead of overwriting it with the free choice — on its own box and over every
  remote's rows — so a node that does not declare the layer is ineligible for that subtask, an idle
  local box that does not declare it does not keep it, and with no node declaring it the subtask
  defers naming the layer (never a silent run on the planner seat). Before this the table keyed every agent row on the pair
  and the triple: a one-card box (the ampere-16 node, `<node-c>`) that declared layers became ineligible for
  every contract, and its second layer — the 35B digest seat beside the 27B GSQ (digest-8 164.8 s vs
  1,597.8 s, blind coverage 4.65 vs 8.53, so by name only, never the default) — deferred "no layer
  serves an agent contract" when requested. Red tests on both seams
  (`internal/placement/decide_requested_layer_test.go`, `internal/delegate/gate_requested_layer_test.go`).
- `contracts/digest-adr-hard-8.json` (register G-36): the harder digest set for the standard quality instrument —
  eight contracts of three ADRs each (24 ADRs disjoint from `digest-8.json`), four verbatim anchored findings per
  document plus a cross-document finding, `min_items:findings:12` and one grounded `regex:` alternation per contract
  built from the 2026-09-17 anchor corpus (195 anchors re-verified against the ADR bytes at build time). Rebuilt
  because the instrument saturated on `digest-8.json` (every 27B-class arm at accuracy ~9, top pair TIE, INV-6).

## [0.128.4] - 2026-09-18 - the MP HTTP frontend default moves to 18790

### Changed
- The `SEAT_MP_HTTP_PORT` default (0.128.3) moves from 18793 to **18790** and the spec default from engine port − 4 to
  engine port − 7: 18793 is `<node-b>` port file's LiteLLM gateway reservation (`0.0.0.0`, LAN + tailnet), which under WSL2
  mirrored networking would collide with the seat's loopback bind; 18790 is the safe pick every port file of the fleet lists.

## [0.128.3] - 2026-09-18 - the vLLM seat launcher binds the LMCache MP HTTP frontend to loopback on its own port

### Fixed
- **The LMCache MP server's HTTP frontend was listening on `0.0.0.0:8080`** (register B-01 / L5, found 2026-09-18 while
  re-measuring the cache tier): LMCache 0.5.x opens an HTTP API beside the ZMQ port and defaults it to `0.0.0.0:8080`, and
  `seat_fg.sh` never passed `--http-host/--http-port`, so the production MP server logged `Uvicorn running on
  http://0.0.0.0:8080` — on a WSL2 distro in mirrored networking that is the host's LAN and tailnet, and 8080 is somebody
  else's port on every box in this fleet. The launcher now passes `--http-host 127.0.0.1 --http-port "$MP_HTTP_PORT"`
  (`SEAT_MP_HTTP_PORT`, default 18793 — the reference pairing is 18797 engine / 18796 MP ZMQ / 18793 MP HTTP), refuses a
  squatted HTTP port the way it refuses a squatted engine port, and the seat spec carries `mp_http_port` (0 = engine
  port − 4) rendered into `seat.env` as `SEAT_MP_HTTP_PORT`. `TestSeatLauncherBindsTheMPHTTPFrontendOnLoopback` pins the
  three launcher facts, the env token and the default.

## [0.128.2] - 2026-09-17 - the node's residency cache no longer serves the pre-last-job seat state: a too-stale read waits for the probe, a completed call on the seat writes seat_loaded from both doors

### Fixed
- **A node's health served the seat state from before its last job, however long ago** (found by the
  0.128.1 slot census: the delegator's eta charged `cold 28` for a seat whose admission wait was 6 ms). The
  residency cache (resident / served roster / `seat_loaded`) was stale-while-revalidate with no bound: any
  read past the 30 s window served the previous answer and refreshed behind it, so the first read after a
  quiet period always described the seat as it was before the last job. A read older than two windows,
  or never taken now waits for the fresh `/running`, bounded by 1.5 s (under every health client's budget)
  and said once per refresh on the node's log when the wait runs out; a read 30–60 s old keeps the old
  shape. And the dispatch path writes the one fact a finished agent contract proves — a completed call on
  the advertised seat (`steps > 0`, `seat == agent_seat`) — as `seat_loaded:true` straight into the cache,
  no probe, never touching residency or the cache's age, from BOTH doors (the push dispatch and the
  pull-queue claim loop); a zero-step defer, a job error or a foreign-seat result writes nothing, a probe
  that started before the write keeps the job's seat facts when it lands, and an undecodable result is
  logged once per process instead of silently losing the fast path. Both eta directions were affected: a cold charge on a warm seat, and no cold charge on a seat
  that had idle-unloaded since the cached read.

## [0.128.1] - 2026-09-17 - the feasibility floor asks only for a minimum viable final, the eta is fitted to the wall, forced-remote refusals carry per-node verdicts

### Changed
- Post-#369 cleanup: dropped the now-dead `leaseFences` helper (its logic lives on in
  `leaseFenceReason`, the only caller `eligibilityVerdict` ever used), fixed the `cap` placement
  verdict to print real `headroom`/`dealt` numbers instead of an always-zero subtraction, and
  renamed a `RunWith`-local `busyReading` that shadowed the `local LocalRunner` parameter.

### Fixed
- **The feasibility floor refused seats that answer, and charged a cold load the wall never pays** (0.128.0 regression,
  found by the deploy smoke: a cold `<node-a>` was refused a 60 s contract it completes in ~25 s — `fitted final 0 < floor 1024`
  — while `<node-c>` slipped through on the fitter's "no opinion" early return and printed an eta of 4,682 s for a 60 s
  wall). `feasibleFinal` now asks the INV-5 rider's question and nothing more: can the seat produce one tool step and a
  minimal 64-token answer inside the wall, with no think block, no re-pack and NO cold load (admission pays the cold load
  outside the wall, D-64). Everything above that floor is a ranking matter: `etaFor` fits the final to the WALL (not the
  wall minus cold), keeps the cold load in the eta, and caps generation at the wall, so an eta never exceeds cold + wall.
  The forced-remote "no eligible remote" result now carries the deal's per-node verdict line beside the aggregate text.

## [0.128.0] - 2026-09-17 - placement by expected completion: feasibility from the fitted final, quality-adequate seats ranked by ETA with power-of-two-choices, per-node headroom in the deal, busy means a job in flight, one-word verdicts per node

### Added
- **Placement (`route=auto`/`remote`) ranks quality-adequate seats by expected completion, under the
  operator-signed INV-5 rider** (ADR [0050](docs/architecture/decisions/0050-placement-ranks-adequate-seats-by-expected-completion.md);
  registers S-01/S-02/S-03/S-05/S-11/S-13/S-15, D-105/D-106).
  - `route=auto`'s busy reading widens from the GPU lease alone to `leaseInfo.Held || local.inflight
    >= FleetConcurrencyLimit() || local.loading` (W-01) — `probeLocalBusy` read once per Run, cached
    on the runner, the same one-probe invariant `route=spread` already held.
  - `remoteEligible` gains a feasibility floor (W-05): a seat whose fitted final cannot clear
    `seatrate.FinalBudgetFloor` within its own effective wall is excluded, naming the arithmetic
    (`fitted final 312 < floor 1024 at 5.4 tok/s in 300 s wall, cold 69 s`) — deliberately the
    contract's own FITTED final, never the seat's max-final `min_turn_sec`, which the rider forbids.
  - `betterRemote` and `scoreFit` gain an expected-completion axis (W-11, new file
    `internal/delegate/eta.go`): cold load (charged only when `seat_loaded` is KNOWN false) + the
    node's own queue wait + the fitted final's generation time. Reasoning-shaped work still ranks
    window first, eta as its tie-break; mechanical work now ranks eta first (replacing "smallest
    adequate seat wins"), window as its tie-break. Two candidates within 20% of each other are
    resolved by a deterministic seeded draw (power-of-two-choices) so independent dispatchers spread
    across near-tied seats instead of herding onto the single fastest one. An unknown rate keeps
    today's window-only ordering.
  - `RunWith` computes route=auto/remote's WHOLE-Run placement in one joint deal over one fleet
    snapshot (W-06, `dealAutoRemote`), respecting per-node headroom
    (`max_concurrent_jobs − jobs_running − subtasks already dealt`, no floor) — the same invariant
    `route=spread`'s `dealSpread` already held, closing the gap where `runConcurrency` siblings
    probed and placed independently within milliseconds of each other.
  - A remote's `LeaseBusy` (a declared-but-idle reservation) demotes instead of hard-excluding
    (W-14); only an exclusive/draining hold, or a busy lease that is not a plain text reservation,
    still fences.
  - `placement_reason` on an `auto`/`remote` placement now names EVERY reachable remote with a
    one-word verdict (`chosen | queue | cap | slow | lease | cold | probe | unfit(ctx) | noschema`,
    new file `internal/delegate/placement_reason.go`), appended after the existing
    `route=auto`/`route=remote` prefix (kept byte-identical for `fleet_smoke_cmd.go`).
  - The delegator's poll now sends `GET /fleet/jobs/<id>?wait=12`; an older node ignores the
    parameter and answers at once. A dispatch 503 carrying its own `Retry-After` is honored with a
    bounded wait-then-retry to the SAME node, invisible to the re-placement loop (no `tried` mark).
  - `NodeView` decodes the 0.127 health activity fields (`jobs_admitting`, `seat_loaded`/
    `seat_starting` as tri-state `*bool`, `lease_exclusive`/`lease_draining`,
    `recent_agent_wall_sec`, `queue_wait_estimate_sec`).
  - `offload_status`'s fleet node rows and the local seat entry publish `in_flight`
    (`jobs_running − jobs_admitting` remotely, the seat's own gauge locally — a job-registry count,
    never GPU utilization or a lease alone) and a one-word `verdict` (`busy | held-idle | loaded-idle
    | cold | unknown`) (W-31). **`offload_status` only in this PR** — `gpu status` and
    `fleet-ui`/`top` keep their existing `gpuactivity`-based vocabulary; adopting the same words
    there is a follow-up, not shipped here.

## [0.127.1] - 2026-09-17 - the final's tail and the node's honest backlog: fitted-final re-issue, one bounded re-pack, last attempt decides the class, queue depth = 2x workers with an ETA-bearing Retry-After

### Added
- **A `503 queue full` refusal now says when it will lift, and `/fleet/health` says it in advance**
  (register S-04, diagnosis §2(a)/§5.2, the overhaul plan's roast correction: an admission refusal
  must be something the delegator can outwait, never terminal). The NODE now publishes both signals
  — `internal/delegate` does not consume either one automatically yet; that lands with the
  placement release (`feat/placement-eta`), and this PR gives it something to read. The dispatch
  response carries a `Retry-After` header, and health separately publishes `queue_wait_estimate_sec`
  (float, omitted when a worker is free or no wall sample exists) from the SAME formula:
  `ceil(excess x recent_agent_wall_sec / max(1, max_concurrent_jobs))`, `excess = capped_backlog -
  max_concurrent_jobs` — the CAPPED backlog only (`Jobs.CountsCapped`), never the wire's all-task-
  types `queue_depth`, or an uncapped media/stt/pipeline job would inflate the estimate for a node
  whose agent slots are genuinely idle (the same class of bug `saturation.score` was already fixed
  for, S-17, and this release's own `IdleSlot` fix, S-20, repeats the lesson of). Deliberately sized
  from the node's own MEASURED recent wall, never from `seat_rate.min_turn_sec` — that number is a
  max-final RETRY floor for one seat with no relationship to backlog depth. A job still in its
  ADMISSION phase counts toward the estimate exactly like any other running job (the worker slot is
  genuinely taken), unlike `saturation.score`, which excludes it because no card is busy yet.

  The HEADER (an HTTP retry contract) is bounded to `[5, 300]`; the HEALTH FIELD is the raw,
  unbounded estimate — a delegator's own placement signal, where a genuine 600s estimate is more
  useful reported honestly than floored to 300. The header's clamp is said out loud rather than
  disguised as a precise number, so a caller can tell three shapes apart: no wall sample yet
  (`no recent completions yet — retry in 30 s`), a real estimate (`~N s until a worker frees, from
  recent completions`), and a clamped-high one (`>=300 s until a worker frees, …`) — an undisguised
  300 would otherwise be indistinguishable from a genuine 300s measurement and invite every waiter
  to retry in lockstep. The existing "queue full" message text stays a byte-identical prefix — the
  delegator already quotes it verbatim in its own placement-refused message — with one of the three
  clauses above appended.

### Changed
- **`fleet_max_queue_depth`'s default is now `2x fleet_max_concurrent_jobs` (8 with the default 4
  workers), not a flat 32** (register S-04/C-25, diagnosis §2(a)). A node admitting 32 deep behind 4
  workers could pile up 28 jobs it had no hope of starting inside any wall a caller would wait out —
  236 measured contracts died at the delegator's 5-minute queue deadline having never started, 75% of
  them while another node sat idle. `0 = use the built-in default` and a negative value = unlimited
  are unchanged; an explicit `fleet_max_queue_depth` still wins outright, and the default now tracks a
  custom `fleet_max_concurrent_jobs` instead of ignoring it.

### Fixed
- **`Jobs.IdleSlot()` reported a node full because of a queued job that would never contend for a
  capped execution slot** (register S-20/C-26). It returned `false` for ANY job sitting in `accepted`,
  including one admitted `Uncapped` (a render, an stt, a pipeline route — work that never touches the
  shared llama-swap endpoint `fleet_max_concurrent_jobs` protects); `claimLocked` already skips only
  CAPPED entries when the concurrency cap is at its ceiling (`jb.capped && full`), so `IdleSlot` now
  mirrors it — only a capped queued job makes the shed predicate (health's `saturation.idle_slot`, the
  admission rule for `priority: -1` dispatches) report non-idle.
- **The claim loop's back-pressure check read the RAW `fleet_max_queue_depth` config field instead
  of the resolved limit** (register review round 1 addendum item 7). `StartClaimLoop` compared
  `cfg.FleetMaxQueueDepth` (0 at the default, unset) directly, so `limit > 0` was always false and
  the pull-queue's own back-off never activated no matter how deep the backlog got — even though the
  push path was already refusing new dispatches at the resolved default. Extracted into
  `claimBackpressureActive(cfg, depth)`, which reads `cfg.FleetQueueLimit()` instead — the same
  resolved limit the push path's admission gate compares `QueueDepth()` against.
- **The list-cap re-issue's gate could never fire on a slow seat** (register S-06/W-07, diagnosis
  `2026-09-17-harness-scheduling-diagnosis.md` §2(d)/§6). A final answer cut at the completion budget
  on a schema contract is re-issued once with the schema's own list caps (0.122.1, D-95), gated on the
  wall still holding one more turn — but that gate sized the turn from the CONFIGURED final (up to
  8,192 tokens) plus a full re-pack term, `MinTurnFor(0, finalBudget, repackBudget, tok_s)`, when the
  re-issue itself is one turn at the FITTED final (`final_budget_fit`) with nothing to re-pack. Below
  ~9 tok/s the configured-budget gate exceeded the 900 s wall cap and was unsatisfiable — the #1
  measured defer fleet-wide (48 rows "re-pack skipped: the final answer was cut at the completion
  budget"). Now gates on `MinTurnFor(0, final_budget_fit, 0, tok_s)` — the arithmetic the re-issue
  actually costs.
- **The structured re-pack rebuilt its cascade-lane probe cache on every attempt** (register D-85/D-108,
  S-21/W-19, same diagnosis §2(d)/§3/§6). Up to three attempts — two grammar completions and the chat
  fallback — each called `repackClient` independently, and each one built a FRESH `FleetLaneGates` cache
  and a fresh `LocalSwapBusy` closure, so every attempt re-paid a live `/v1/models` + `/running` +
  per-model gauge read of the LOCAL seat before spending a token. Measured fleet-wide: 1,301 rows,
  median 18 s, p90 109 s, max 581 s, **15.13 h total**, 244 rows at all three attempts — 180 of those
  deferred anyway. The probe closures are now built ONCE per `repackStructured` call and threaded into
  every client the call still constructs, so their own TTL cache does the sharing. Each attempt's own
  transport timeout is also now bounded by `min(repackTimeout, remaining wall / attempts still owed a
  turn)` instead of the full per-call allowance, so one slow attempt can no longer sit on everything
  while the retry and the fallback are still due — sized against the client's own timeout, never a
  wrapped context, so `llamaclient`'s seatwait 429/503 retry loop and the caller's wall-timeout
  classification keep reading the same, real wall clock they always have.
- **A cancelled parent mid-LOOP was filed as broken hardware** (register S-22/W-16, same diagnosis
  §2(e)/§6). The loop branch of `runAgentTask` fell through to `agent loop: <err>` /
  `defer_class: infrastructure` on a canceled parent context — the delegator abandoning the poll, or
  the node shutting down — while the re-pack branch thirty-odd lines below already carried the correct
  arm (12 `agent loop: context canceled` rows filed as broken hardware, an operator sent to fix a box
  that never misbehaved). Mirrored into the loop branch: a canceled parent is now `defer_class: budget`
  there too — nothing on the box failed; the caller went away.

## [0.127.0] - 2026-09-17 - the fleet answers faster: honest node health, long-poll completion, concurrent probes, alias-aware admission on both doors, config validation that names a dead endpoint

### Added
- **`GET /fleet/jobs/{id}?wait=<seconds>` answers when the job FINISHES** (register S-19, diagnosis
  `2026-09-17-harness-scheduling-diagnosis.md` §2(d)/§5.3). The node had no completion event at all,
  so the only way to learn a job was done was to ask again in three seconds — all 236 measured
  queue-deadline rows report exactly `101 poll(s)` = 300 s of pure polling — while the job store had
  been broadcasting every terminal transition all along. The handler now waits on that broadcast
  (`Jobs.WaitTerminal`), returning at the finish, at the wait, or when the caller hangs up, and
  answering with the job's live state either way. Capped at `fleetnode.MaxJobWaitSec` = 12 s, which
  MUST stay below the delegator's `pollRequestTimeout` (15 s, `internal/delegate/run.go`) or every
  long poll would be cancelled client-side just before the node answered — pinned by a test that reads
  the delegator's own source. Absent the parameter the route is byte-identical, so every deployed
  delegator is unaffected and the 3 s poll remains the fallback. `finish` and the drain's shutdown mark
  now broadcast too, so a parked poll wakes on any terminal transition, not only on the one that frees
  a slot.
- **`/fleet/health` publishes six honest fields about work in flight** (registers S-17, S-36; all
  additive and `omitempty`, so a node without the lane, the lease or the holds emits a byte-identical
  payload and an older delegator decodes exactly what it did before):
  `jobs_admitting` (the subset of `jobs_running` whose worker is still in the run's ADMISSION phase —
  cordon → pre-flight → warm → coherence probe, up to 300 s with the card idle — counted from this
  process's own `gpuactivity` records, ADR 0041); `seat_loaded` / `seat_starting` (llama-swap's
  `/running` verdict on the agent seat, read alias-aware through `internal/seatload` and `/running`
  ONLY, never a path that could LOAD the seat — register C-05; BOTH a failed read and an AMBIGUOUS one
  leave the pair absent rather than publishing "not loaded", where ambiguous is `seatload`'s own
  predicate — the roster GET failed AND `/running` lists models the bare-name fallback cannot match, so
  an alias-bound seat cannot be seen. That is the same refusal `gpu_drain` and
  `internal/placement/live.go` already apply to this reading, and it is narrower than "the roster
  failed": a failed roster over an EMPTY `/running` is knowable and publishes `seat_loaded:false`.
  Both withheld cases are logged); `lease_exclusive` / `lease_draining` (what the held reservation is
  DOING, beside `busy`'s verdict about declared time); and `recent_agent_wall_sec` (the median wall of
  the last up to 8 agent jobs to finish, from the same job map `/fleet/jobs` walks — the completion
  signal a node with no `seat_rate` sample could not otherwise publish).
- **`config.Load` refuses a configured HTTP base it cannot prove is dialable** (register S-38).
  Two classes, both of which used to be reported only as a dial timeout on the first real call:

  - **Not a usable URL.** A parse error, a scheme that is not `http`/`https`, or no host at all.
    An endpoint whose value was never substituted usually is not a URL — `${NODE_A_HOST}:18811`
    and `http://node-a:$PORT` both fail `url.Parse`, and `node-a:18811` PARSES, as scheme
    `node-a` with an empty host, so nothing errored, there was no port to judge and the dialer
    resolved nothing.
  - **A port nothing answers on.** `:0` is the OS’s "any free port", which nothing ever listens
    on; `:9` is the IANA discard port. The comparison is NUMERIC, so `:09` is refused too.

  Both apply to `endpoint`, `delegate_remotes[]`, `cascade_remote_lanes{}`, `seat_endpoints{}`,
  `fleet_queue_holder`, `tts_endpoint`, `nim_endpoint`, `hailo_endpoint`, `coral_endpoint` and
  `pair_workloads_endpoint`, and the error names the key and the value. An EMPTY value is not a
  finding (an unset optional key is a machine that does not have that thing), and a **loopback**
  base on an unusual port is explicitly still allowed: INV-10 sanctions a loopback-only bench
  twin beside the production seat, and refusing it would break measurement on the delegator box.
  The two base-URL maps and the one base-URL list share ONE per-value gate
  (`validateEndpointValue`) with the tailnet guard, so the never-cloud rule and the dead-base
  rule cannot drift apart across keys.
- **What a refused config actually does, per entry point.** The refusal is not advisory, and it
  is not uniform — each door gets the answer that door can afford:

  - **`fleet-serve` REFUSES to start** (non-zero exit, the error on stderr). A node advertises
    capability to other boxes and then accepts their dispatches, so one that cannot prove its own
    config does not fail alone: it turns every delegator’s placement into a wasted wall.
  - **`mcp` STARTS and says so.** A server that exits removes every `offload_*` tool from every
    session with no message on any surface an operator reads. Instead `offload_status` carries
    `config_error` as its FIRST key, and every other tool returns
    `{"deferred":true,"reason":"config invalid: <err>"}` until the file is fixed.
  - **One-shot CLI verbs proceed**, warning as they already did.
  - **`doctor` prints the refusal VERBATIM as its first `FAIL` row and exits non-zero.** It kept
    the load error and printed only a generic one-liner, so the one text that names the offending
    key never reached the operator through the one verb they run to find it.
- **`local-offload doctor` prints a `config findings` section** — one `FAIL` row per value that
  loads and then cannot do what it says, and a non-zero exit, like every other doctor verdict.
  Three classes, all previously invisible: (1) a fleet/lane base shape (S-38, WARN half) — a
  `delegate_remotes` entry not on the fleet node port `:18811`, one that is loopback (a remote
  cannot be this box), one carrying a `/v1` suffix, or one that is not a usable URL; a
  `cascade_remote_lanes` base on neither shape a lane can be (a fleet node, or a llama-swap on
  this box’s own `endpoint` port), carrying `/v1`, or unusable; (2) `gpu_wait_ms` more than 3x
  `vision_gpu_wait_sec` (register C-33 — a deployed 600,000 ms against a 90 s vision wait is ten
  minutes of blocking on every media-lane call, against that key’s own documented 90 s design),
  and a NEGATIVE `gpu_wait_ms`/`vision_gpu_wait_sec`, which both consumers turn into a ZERO wait
  while the file reads as a wait — a finding, deliberately not a load error, because it behaves
  as the documented `0` rather than breaking anything; (3) one row per RETIRED key the file still
  carries (`videogen_wait_ms`, `audiogen_wait_ms`), which until now was a single stderr note at
  startup that scrolls past every command. These WARN rather than refuse for one release — a
  strict validator that refuses a working odd config is a worse outage than the dial timeout it
  replaces — and the fleet/lane shapes also print one stderr line at load.

### Changed
- **`saturation.score` no longer counts a job whose worker is still in admission** (register S-17).
  `jobs.go` flips a job to `running` before `execute()`, and the run then spends up to 300 s in
  admission — so the node published `score 1.0` beside `gpu_util_pct 0`, and in 10 of 47 measured
  lease-timeout rows the box had zero agent jobs actually in flight. The exclusion applies to the
  CONCURRENCY numerator only: `saturation.high`, `saturation.idle_slot` and the depth term are
  unchanged, because the slot really is taken and `max_queue_depth` bounds every admitted job whatever
  its phase.
- `internal/seatload` gained `Running`, the `/running`-only half of `Inflight` (no `/upstream` read at
  all, so it can never load an unloaded seat), shared by both so the alias resolution has one
  implementation.
- **A config that FAILED validation no longer reports itself as "BUILT-IN DEFAULTS".** Both
  disclosures said so and both were false: `Load` returns the FILE’s settings with only the five
  composite keys stripped, so the process runs on that file — which is exactly why the refusal
  matters. `config.WarnOnDefaults` and `config.SourceLine` now name the file, carry the
  validation error verbatim, and say which entry points refuse, limp or proceed. Saying
  "defaults" sent an operator hunting a path problem while the real one was a named key in the
  file they already had open. `TestConfigSourceLine`’s load-failed assertion pinned the false
  text and is amended.
- **The delegator's fixed sleeps are jittered by ±20 %** (`pollEvery`, `placementPollInterval`,
  `refusalCooldown`). Every dispatcher sleeps on the same constants, so K sessions started within a second
  of each other re-read health, re-dispatch and re-ask a refusing node in lockstep for a whole run. Each
  sleep is scaled by a uniform factor in `[0.8, 1.2]` — the cadence's mean is unchanged — and the
  jitter is CLAMPED so no sleep can run past the deadline it lives under (the wait's TTL, the poll
  deadline). The cooldown is jittered once when the refusal is recorded rather than re-rolled per check, so
  a node cannot flicker in and out of the candidate list.

### Fixed
- **The 30 s blanket `WriteTimeout` truncated every handler that legitimately runs longer** (register
  S-09). Go arms the write deadline at header-read, so the table bounded the chat lane — whose own
  budget is `ChatProxyTimeout` = 10 minutes — and a forwarded cascade call that generated past 30 s
  was cut mid-write and read to the caller as broken infrastructure. The blanket STAYS as the floor for
  every other route; the two handlers that outlive it (the chat lane, and the new long poll) now extend
  their own deadline per request with `http.NewResponseController(w).SetWriteDeadline` — the lane to
  `ChatProxyTimeout` + 30 s of copy-back slack, the poll to its wait + 2 s. A `ResponseWriter` that
  cannot carry a deadline simply runs under the blanket, as before. `TestServeTimeoutTable` now pins
  both halves: the blanket value AND that exactly those two handlers extend it.
- **The agent client dialled `/v1/v1/chat/completions` for any base written with a `/v1`
  suffix** (register S-37). `agent.NewLLMClient` trimmed only a trailing slash and then
  appended `/v1/chat/completions`, so a base spelled `http://x/v1` — the shape `nim_endpoint`
  documents, and the shape an operator copies off a vLLM seat’s own docs — produced a 404 that
  named neither the key nor the doubling (8 rows in the delegation ledger, all on one seat).
  The harness already owned the one normalisation rule — `swapclient.BaseURL`, the reader every
  other consumer of `endpoint` goes through, whose own comment says a blind append "would have
  produced `/v1/v1/models`" — and this client was the one that did not use it. It does now, so
  `http://x`, `http://x/v1` and `http://x/v1/` all dial `/v1/chat/completions`. Normalising in
  the constructor rather than at the ~60 call sites also keys the `modelaffinity` ticket on the
  normalised base, so two clients spelling one endpoint two ways now contend on one gate.
- **A capacity wait could not see a remote whose health was merely SLOW, and never said so.** The first cut
  of the per-tick probe bound was `2 x placementPollInterval` (6 s), below `fetchNodeViewTimeout` (15 s) —
  this repo's own boundary between slow and down. A remote answering health in 6-15 s under load was
  cancelled on every tick for the whole wait, never became a candidate, and was never negative-cached either
  (a cancellation is not evidence about a node), so the wait expired with `no node had room ... 0
  refusal(s)`: an operator told to add a node while the nodes they had were failing to answer. The tick is
  now bounded only by what is LEFT of the wait; the concurrent fan-out's per-base `fetchNodeViewTimeout` cap
  is what keeps one dead remote from stalling it, and the 30 s negative cache absorbs the repeats.
  `min(fetchNodeViewTimeout, remaining)` was tried and reverted: the per-base context is derived from the
  tick's, so the two deadlines land on the same instant, the tick's fires first, nothing is ever attributable
  to a node, and a dead base is re-dialled at full cost every tick (measured: 30 dials across one compressed
  wait, 6 after). The tick's `probeErrs`, previously dropped with `_`, are now tallied per base and folded
  into the capacity defer as their own clause -- `; N probe(s) failed during the wait: <base>: <reason>
  (last of 3)` -- distinct from refusals, because nobody declined the work.
- **The fleet health probe was serial, uncached and on the critical path of every placement** (register
  D-106; `plans/2026-09-17-harness-scheduling-diagnosis.md` §2(d), S-10/W-02). `delegate.fetchViews`
  probed every configured remote SEQUENTIALLY at `fetchNodeViewTimeout` (15 s) each, with no cache, once
  per subtask on `route=auto`/`remote`, once per re-placement, once per retry and once per capacity-wait
  tick; only `route=spread` amortised it. Measured over 1,977 delegation rows: **46 rows kept work on the
  local seat because one remote's health probe timed out, and in 41 of them that remote had zero jobs in
  flight.** The probe is now (a) **concurrent** — one goroutine per base, each still bounded by
  `fetchNodeViewTimeout`, answers reassembled in the CONFIGURED order so the positional
  `views`/`bases`/`probeErrs` contract holds; (b) **memoised per Run** (`fetchViewsMemoTTL`, 2 s) so the
  sibling subtasks of a fan-out share one snapshot, deliberately shorter than `placementPollInterval` (3 s)
  so the memo can never answer a capacity-wait tick, and dying with the runner so no snapshot outlives its
  call; (c) **negative-cached** (`probeNegativeTTL`, 30 s) for a base that failed at the TRANSPORT only —
  a dial refusal, a reset, DNS or the per-base timeout — with the reason REPLAYED into `probeErrs` so the
  placement note still names the node (a `401`/`404`/`503` is a node that answered and is never cached);
  and (d) **bounded inside the capacity wait** by `probeTickBound` = `min(2 × placementPollInterval,
  what is left of the wait)`, because that loop alone handed the probe the RAW run context, so one
  black-holed remote could spend the whole TTL inside a single tick and the node that DID free was never
  re-asked. Nothing is negative-cached when the tick bound fires: it expires every base at the same
  instant, which is a fact about the tick, not about any node.
- **A cross-seat retry was refused by any node running a single job** (S-14; register D-46 shipped the rule
  and not the threshold). `retrySeatBusy`'s remote arm read `view.JobsRunning > 0` — a threshold of zero
  instead of the node's own ceiling — so a four-worker box with one job in flight blocked every retry
  although three workers were idle. It now asks `!provablyStartsNow(view)`, the same ceiling-aware
  predicate the placement gate ranks on, and the note names the numbers it read
  (`jobs_running 4 of max_concurrent_jobs 4, jobs_queued 0 (fresh health)`).
- **Two remotes with the same `node_id` collapsed into one spread cycle entry** (S-12). `dealSpread` keyed
  its per-cycle `dealt` set on `NodeID` while every other exclusion in the delegator keys on the dial base,
  and a node id is neither unique nor guaranteed to be published (register C-19 shows it drifting). The
  second remote of a cycle found its key already taken, the cycle was reshuffled, and the fit score handed
  the SAME seat both subtasks while the other idled. `dealt` and `fitPick` now key on the base.
- **A READY seat waited the whole admission budget whenever any OTHER model was mid-swap.**
  The swap pre-flight matched llama-swap's `GET /running` by the seat's BOUND name, while `/running`
  names models by canonical id only — and the harness binds agent seats by alias on the reference
  boxes (`agent-pool` → `qwen3.8-27b-vllm`). The "my own seat is already ready" exit could therefore
  never fire on an alias-bound seat, and 406 delegation-log rows say so verbatim ("/running lists the
  seat under another id"). `awaitSeatAdmission` and `warmSeat` now resolve the seat through
  `swapclient.Roster.Canonical` — the resolution `internal/seatload` has used for the drain since
  C-11 — and match `/running` by alias AND canonical id. The roster read is lazy: it is paid only
  when the bare name missed and the alternative is a sleep, so a seat bound by its own id costs no
  extra round trip.
- **The MCP `agent_run` door never ran the swap pre-flight.** llama-swap queues, with no timeout of
  its own, any request that needs a model it is still loading, so an `agent_run` that arrived
  mid-swap spent its wall inside that queue and reported a wall timeout — the failure ADR 0032
  removed from the delegation door in 2026-09-02 and this door never had. `pipeline.AwaitSeatAdmission`
  exports the same function; `handleAgentRun` calls it between the cordon and the warm-up, on the
  remaining admission budget, and reports it under the same wire names (`admission_wait_sec` /
  `admission_note`). The two doors' admission blocks are now the same steps in the same order.
- **The served-window probe ran on the contract's WALL, on both doors.** `agent.ProbeServedWindow`
  carries a ten-minute cold-start budget by design — it is allowed to absorb a seat's load — so on a
  cold or slow seat it consumed the whole wall before the first token and the run was filed as a wall
  timeout. On both doors it now runs on a context derived from the admission deadline, before the
  wall context exists, and its duration is added to `admission_wait_sec`. On the delegation door the
  seat-residency (roster) check moved just ahead of it, so a seat this endpoint does not serve is
  never cold-started by a run that is about to defer.
- **`warmSeat` failed silently on two exits.** A `/running` read that ERRORED and a budget already
  spent by earlier admission both returned "nothing to report", so `admission_note` was empty on a
  seat that might still be cold — "could not read" and "is ready" were indistinguishable on the wire.
  Both now return a note. `warmSeat` also reports whether a load was ATTEMPTED as its own value:
  neither the duration (a sub-tick load measures 0) nor "a note exists" can carry that fact, and the
  D-118 coherence probe keys on it.
- **One transient roster error silently disabled the alias match for the rest of the wait.** The
  resolution above latched "already tried" on the ATTEMPT rather than on a successful read, so a
  single timeout or 500 on `GET /v1/models` — likeliest on exactly the contended box where another
  model is mid-swap — left the seat matching by its bound name for the remaining polls and burning
  the whole admission budget. S-08's own symptom, intermittent and with nothing in the log or on the
  wire. The latch now closes only on success, the failure is logged and carried into
  `admission_note`, and the next poll retries inside the same budget.
- **The MCP door's cordon-timeout defer reported no admission at all.** `admission_wait_sec` /
  `admission_note` were absent on that one path while the delegation door has stamped them since
  0.117.0, so a caller could not tell a 300 s wait from an instant refusal. It now stamps them. (The
  path is the fence pre-check's race window — the two share `modelaffinity.BlocksNewRun`, which
  `TestForeignFenceAndTheCordonShareOnePredicate` pins across all eight lease shapes.)
- **Both doors discarded the served-window resolver's note** (`effCtx, _ :=`), so a run that budgeted
  against the conservative 8,192-token fallback on a 131,072-token seat looked exactly like a correct
  one and the only symptom was a task that compacted for no reason — the same invisibility that let
  the MCP door measure 8,192 cold and 114,688 warm on one seat, minutes apart. The line is now on the
  wire as `ctx_window_note`, and when the fallback was caused by the probe running out of admission
  budget it is repeated in `admission_note`, where the cause actually is.
- **Both agent doors held a worker at the cordon for the full admission budget under a FOREIGN GPU
  fence** — 47 rows × 300 s (3.92 h) in the three days to 2026-09-17. An exclusive text hold, a
  draining cordon or a media render held by another process refuses a new run for as long as it is
  held; nothing the run can do inside its own budget changes that, so the wait reached the same
  `capacity` defer five minutes later while the delegator sat on the re-placement path that defer
  exists to trigger. `delegate.ForeignFence` — wired into the review lane since 0.125.0 (D-110) — is
  now asked BEFORE `AwaitRunSlot` on `runAgentTask` and on `agent_run`, and defers at once naming the
  fence and the holder's line (class, pid, declared reason, expiry). It reads the lease directory
  `config.Load` armed for the cordon, so the pre-check and the cordon can never disagree. An
  INHERITED lease (`GPU_LEASE_EPOCH`) and a plain non-fencing reservation are unchanged: ADR 0032's
  "a peer-held seat is waited for" governs every hold whose answer can still change.

## [0.126.2] - 2026-09-17 - a PAIR card's failure text is one short line

### Changed
- **The failure text on a PAIR Jobs card is capped to one short line** (`pairworkloads.CardError`, 140
  characters, whitespace folded, ellipsis). On the first live day a jsonschema re-pack dump or a full
  acceptance report ran to hundreds of characters per card and turned the Jobs list into a wall of red; the
  card wants the verdict, the ledger keeps the whole reason.

## [0.126.1] - 2026-09-17 - a remote delegation's in-flight PAIR frames name the node by its dispatch host

### Fixed
- **A remote delegation's `queued` / `running` PAIR frames showed the delegator as the running node.**
  `runRemote` handed the emitter the fleet node id (`<node>-ampere8`-style), which is not a PAIR member
  name, so the lookup fell back to this box; only the terminal frame, built from the node's reported
  name, said where the job ran (measured live on the first remote card, 2026-09-17). The in-flight
  frames now use `pairNodeName(base, …)`: the host of the dispatch URL, the tailnet name PAIR's
  `members.json` also carries (`docs/systems/pair-workloads.md`).

## [0.126.0] - 2026-09-17 - harness jobs appear in NVIDIA PAIR's Jobs list (`pair_workloads_*`); an auto contract is polled at the node's sized wall (D-116)

### Added
- **Harness jobs appear in NVIDIA Personal AI Router's Jobs list** (`docs/systems/pair-workloads.md`).
  PAIR's Jobs list is a stream of workload lifecycle frames only PAIR's own proxies produce; the harness
  routes around them, so nothing it ran was visible there and PAIR's scheduler could route its own
  traffic onto a card the harness was using. New package `internal/pairworkloads` posts the same frames
  to the loopback ingress of PAIR's workload manager (our patch: fork `dmmdea/Personal-AI-Router`,
  branch `feat/workload-local-ingress`, worker 0.14.0, upstream PR pending): `id`/`runId` = the harness
  job id, `model` = the seat, `engine` = the real engine with the identifiers PAIR's engine PRs use
  (`llamacpp`, `vllm`, `whispercpp`, `comfyui`), `originatedFrom` = this box's PAIR UUID (from PAIR's
  `node-id.json`), `scheduledOn` = the running node's UUID (by name from `cluster/members.json`),
  `requesterId` = `offload-harness/<session>`. Two sources: the delegate runner emits `queued` before a
  dispatch, `running` at the ack (or when a local placement starts) and the terminal frame in
  `attempt()`'s `finish`, with the card identity pinned on the `PlacedResult` so every frame of one job
  merges into ONE card; a new `ledger.Ledger.Observe` hook turns every other written row into one
  terminal frame (`agent_delegate`, `agent` and cache-hit rows skipped). Fire-and-forget: a 2 s
  timeout, one warning per process, never a result changed. Keys `pair_workloads_enabled` (default
  false; delegator boxes only) and `pair_workloads_endpoint`
  (`http://127.0.0.1:14324/v1/workloads/events`); env `OFFLOAD_PAIR_APPDIR` for a non-default PAIR
  data dir. Verified live on the delegator box: frames accepted, the broker applied them with the stamped
  origin, the record persisted to `workloads-history.json`.

### Changed
- **An auto contract is polled at the node's sized wall, not at the wire cap** (register D-116). Since D-03 a contract
  whose caller named no `timeout_sec` rides the wire as `timeout_auto` and the EXECUTING node sizes the wall from its
  own seat's measured rate, inside 300..900. The delegator budgeted AND POLLED it at the cap, so a node that acked a job
  and then died silently was abandoned 900 s + grace later although its own wall had been 300 s — the "accepted cost"
  the 0.126.0 review wrote down. It is paid down here, from both ends:
  - **The delegator sizes its poll clock from what the node advertises.** `delegate.autoPollBound(view, contract,
    runSeat)` builds a `seatrate.SeatPolicy` from the target node's health `seat_rate` + `seat_budget` and runs
    `seatrate.AutoWallFor` — the SAME function `pipeline.AutoWallFor` runs on the node, over the same
    `seatrate.FinalBudgets` / `seatrate.InputFor` rule, clamped to the same
    `[AgentTimeoutSecDefault, AgentTimeoutSecCap]`. One arithmetic, two sources of numbers; a test sizes one contract
    through both entry points and fails if they disagree. A node advertising no usable rate is still polled to the cap —
    unchanged behaviour, and the reason says so.
  - **One arithmetic is one clock only while both halves are fed the SAME seat.** On a composite node the delegator
    dispatches a LAYER, and the node re-decides it, switches to that layer's seat and sizes its wall from THAT seat's
    rate — which health publishes for the agent seat alone (`layers` carries seats, never rates). So the decided seat is
    carried into the bound, and a run on a seat whose rate nobody advertised falls back to the cap with
    `poll bound: cap: the contract runs on <node>'s seat <x> and only <agent_seat>'s rate is advertised`. Without it a
    long layer seat put the node's wall up to the whole 300..900 span beyond the delegator's clock.
  - **The two clocks also have two STARTS, and the delegator allows for the node's.** A node stamps a job `running` the
    moment it claims it, so its ADMISSION — the cordon wait, the llama-swap pre-flight, a vLLM seat's 125–250 s cold
    load, the coherence probe, up to `core.AgentAdmissionSecDefault` (300 s) — is spent in `running` and earns no queued
    credit, while the delegator's clock starts at DISPATCH. Sized at wall + grace it therefore abandoned an auto
    contract that landed on a COLD seat while the node was still inside its own wall. Two halves close it: the
    pre-adoption bound carries an admission allowance (named in the deadline message, and dropped the moment it is no
    longer needed), and `wall_sec` is now published at the line that OPENS the wall context rather than at the sizing —
    so it means "the wall has started", and the first poll carrying one RE-ANCHORS the delegator's clock to that
    instant. An admission-time defer publishes no wall at all, because no wall ran.
  - **The node publishes the wall it is RUNNING under.** `GET /fleet/jobs/{id}` carries `wall_sec` (additive,
    `omitempty`) while the job is `running`, fed by `core.ReportWall` from the lane that stamps the wall through
    `fleetnode.Jobs.SetWall` — reported where the wall STARTS, after admission. The delegator anchors its clock on the
    first one it sees and RAISES its bound to any larger number after, never lowering it:
    the node's wall is authoritative, and abandoning a job a node is still running inside its own wall is the one thing
    this change must not buy. A terminal job's record is never rewritten; an older delegator ignores the field.
  - **The poll-deadline message names the bound's source** — `poll bound: sized from <node>'s seat_rate X tok/s (N
    samples): M s` (plus `, + Xs allowed for the node's admission before its wall starts` until a wall is observed),
    `poll bound: the node's own wall M s, from where the node started it`, or `poll bound: cap: no seat rate advertised
    by <node>` — on the defer and on the never-owned FAILURE alike.
  - **Wire:** `poll_note` on `delegate.ResultWire` (so `agent_delegate` and the CLI publish it), carrying that same line
    on every outcome including a green one. Delegator-side only: no new field on `core.AgentWireResult`.
  - Unchanged: a contract that names its own `timeout_sec` (polled at `timeout_sec` + grace, no note, byte-identical
    output), the queued-time credit, retries and re-placements (their walls are explicit remainders), the delegator's
    execution BUDGET for an auto contract (still the cap — the node may legitimately size anything up to it), and the
    `queue` route, whose claimant the delegator never chose and for which there is therefore no health view to size from.

### Added
- **Seat coherence probe at warm: an incoherent seat defers before the wall** (register D-118). A seat can pass
  `/health`, `/v1/models`, the speed probe and the READY smoke and still be numerically broken — on 2026-09-16/17 a
  Qwen3.8-27B GSQ seat (fp8_e5m2 KV, RTX 5060 Ti, vLLM 0.29 / FlashInfer) answered every contract with
  `<tool_call>!!!!!!!!!!!!!!!!!!!!…` to the cap, 126–336 s of degenerate output filed as `unparsed_tool_call`, and two
  GPU leases went on parser hypotheses before one raw completion was read. Both agent doors (the delegation lane's
  `runAgentTask` and the MCP `agent_run`) now ask the freshly loaded seat ONE bounded question on the admission budget
  — "call `read_file`, then answer DONE", ≤ 96 tokens, thinking off, the node's own sampling, through the same
  `agent.LLMClient` the loop uses — BEFORE the contract's wall context exists.
  - A parsed tool call, or plain text with no tool call, proceeds; a transport error, a timeout or an empty choice list
    proceeds as `inconclusive` (fail-open, exactly like the warm-up). A completion carrying a run of ≥ 20 identical
    non-whitespace bytes (`agent.DegenerateRun`), an unparsed tool-call marker with no parsed call
    (`agent.UnparsedToolCallMarker`, exported from the loop), or nothing printable (empty or whitespace-only) at the
    token cap **with no hidden reasoning reported** is BROKEN: the contract defers `infrastructure` with the reason prefixed
    `seat incoherent at warm: ` (`core.IncoherentSeatReason`) having spent seconds instead of its wall. A THINKING seat
    cut inside its think block arrives in that same empty-at-the-cap shape and is healthy — the client refuses to fold
    the reasoning channel into the content at `finish_reason: length` on purpose, and `enable_thinking:false` is a kwarg
    a DeepSeek-R1 / gpt-oss / llama.cpp `--reasoning-format` template is free to ignore — so a completion carrying
    `reasoning` / `reasoning_content` or `reasoning_tokens` proceeds with a `cut inside the think block` note instead,
    the same shape the loop itself calls recoverable reasoning starvation.
  - Config `agent_coherence_probe`: `""`/`"cold"` (default — probe only after a cold load this run observed), `"off"`,
    `"always"`. Any other value is refused at the config door by name.
  - Wire: `coherence_note` on `core.AgentWireResult` and on every `agent_delegate` subtask (`delegate.ResultWire`),
    beside `admission_note`; the MCP door emits it in its result map. Set on every run where the probe RAN — coherent,
    inconclusive or broken — and empty when it did not. The probe's time is added to `admission_wait_sec`, never to the
    wall.
  - A broken verdict is REMEMBERED per endpoint+seat for 10 minutes, because nothing unloads or quarantines the seat
    the probe caught and the default `cold` policy probes only the contract that loaded it: a later WARM contract on the
    same resident seat defers on the remembered verdict, spending no probe, instead of paying a whole wall for output
    already proved degenerate. The memo is process-local (the fleet node and the MCP server are the processes that take
    contract after contract), and it is cleared by the seat's next cold load, by any later non-broken probe, and by its
    own TTL, so a seat fixed out of band is never trapped deferring.
  - `delegate.IncoherentSeatDefer` makes this the ONE retry-eligible infrastructure defer: the fault is a property of
    THIS seat and it was caught before the wall started, so the delegator re-places the contract on another node. What
    the node's admission spent getting there — on the default trigger, a 125–250 s vLLM cold load — is credited back to
    the subtask's `timeout_sec` ledger (`delegate.admissionCredit`), the same mechanism the capacity wait already uses;
    without it the retry floor refused the retry on the very path that produces the defer. Every other infrastructure
    defer is unchanged.
  - The `coherence-probe` run phase heals on the loop's first step (`gpuactivity`), so `gpu status` / `offload_status`
    no longer advertise a multi-minute agent run as still being probed.

### Changed
- **The 26B agent lane runs with thinking OFF** — `config_seed.agent_thinking: "off"` on the four tiers that ship
  `gemma-4-26b-agent` (blackwell-16, volta-16, amd-rdna3-dgpu, dual-gpu), masterplan D-119 follow-up, 2026-09-17. Blind
  on the H-04 instrument at a matched 4,096 budget (one axis, `pack --axis thinking`): thinking off 7.31 vs the shipped
  auto 6.73 — gap 0.58, head-to-head 19/24, every lens agrees, SEPARATED; the think block costs specificity and
  coverage and buys padding (8 degenerate flags vs 2), accuracy level. The seat entry keeps `--reasoning on`; the
  harness renders the no-think turn per request. Measured on one RTX 5060 Ti; inherited by the three siblings as a
  property of the model's output. The tier pages and profile notes carry the record
  (`2026-09-17-d119-26b-budget-bake/out-thinking/`).
- **`blackwell-16`'s `config_seed.agent_max_tokens 4096` is now a measurement** (masterplan D-119, 2026-09-17): the
  26B llama.cpp agent lane at the tier's own line on one RTX 5060 Ti (14,540 of 16,311 MiB, 16 s load, 100 tok/s),
  blind Opus 1,024 vs 4,096 = TIE (6.76 vs 6.30, gap 0.46, 18/24, both 8/8) — the seed stays; the profile note and
  the generated tier page no longer call it unmeasured. Record `2026-09-17-d119-26b-budget-bake/`.
- **`blackwell-16` declares its vLLM seat, measured on an RTX 5060 Ti** — ADR 0048 Amendment 1. Same checkpoint as
  ampere-16 (Qwen3.8-27B 3-bit GSQ) at 49,152 @ util 0.92 (KV 76,314 tokens, 1.55x), 27.75 tok/s single / 64.39 at 4
  streams, digest-8 8/8 at the bound lane, blind 8.53 (level with the A2's 8.46). **`kv_cache_dtype` is `fp8` (e4m3),
  not the A2's `fp8_e5m2`:** e5m2 through FlashInfer returns NaN tokens on this card under vLLM 0.29 (two 0/8 runs,
  raw completions `<tool_call>!!!!…`) while e4m3 on the same backend is coherent — isolated one variable per arm and
  pinned by `blackwell16_bound_lane_test.go`. The coverage gate now reads 4 of 16 tiers seated, 12 owing.
- **WSL2 launches of a vLLM >= 0.29 venv pin the V1 model runner** (`seat_fg.sh`): 0.29's default V2 runner needs
  UVA, which WSL2 reports unavailable; with `VLLM_WSL2_ENABLE_PIN_MEMORY=1` it starts and dies in kernel warm-up
  (`CUDA error: invalid device ordinal`). `VLLM_USE_V2_MODEL_RUNNER=0` is exported when `/proc/version` says microsoft
  AND the venv's vLLM dist-info is >= 0.29, unless the env file already set it. 0.28 is deliberately untouched (its V2
  runner runs on WSL2; the production pair seat logs it) and native Linux launches are untouched at any version.
  blackwell-16's `config_seed.agent_max_tokens` is seeded 4,096 for the llama.cpp fallback lane (not a 26B measurement;
  masterplan D-119 measures it).
- **`ampere-16`'s vLLM seat is Qwen3.8-27B 3-bit GSQ** — ADR 0049. `qwen3.5-4b-vllm` ->
  `qwen38-27b-gsq-vllm` (ISTA-DASLab/Qwen3.8-27B-3Bit-GSQ, 11.85 GB, `max_model_len` 49,152, util 0.92).
  ADR 0047 recorded a 27B-under-vLLM on one 16 GB card as impossible; that was a SEARCH error twice over —
  every W4A16/AWQ/GPTQ build is 19.45-19.56 GB because it leaves the ~248k-vocab embedding at high precision,
  while GSQ quantizes the embedding and LM head (4-bit RTN g64) alongside 3-bit transformer weights, exactly
  as llama.cpp's IQ3_S does. Chosen for **3.7x concurrency** (19.53 vs 5.26 tok/s aggregate at 4 streams;
  single-stream 5.75 vs 5.91 is a tie) and for being the tier's only path to the LMCache cache server
  (ADR 0045) — **not** on blind quality, where the llama.cpp IQ3_S+MTP arm leads. Matched-window result (both
  arms at 49,152, same sampling): **8.35 vs 9.29**, gap 0.94, 21/24, every lens agreeing — the window did not
  close it (coverage 7.56 either way; accuracy 9.63 vs 9.47 and 0 fabrications on the GSQ side), so the ADR now
  records the 0.94 as the real cost of choosing concurrency and the cache server.
  **Deployed live on the reference box the same night** (venv vLLM 0.28.0 → 0.29.0 + the carried patch; the 4B
  seat on the new engine passes digest-8 8/8): the GSQ loads (200 s direct / 67 s front door, KV 65,967 tokens),
  serves, and passes digest-8 **8/8 at 900 s** walls (3/8 at the 300 s wire default — timeouts, no wrong answers;
  no decode-rate sample recorded, so D-03's auto wall needs `agent_seat_tok_s` seeded). **The bound lane stays the
  4B**: at util 0.92 the seat takes mem0's embedder off the card (`embeddinggemma → HTTP 500`, every mem0 write
  500) and at util 0.90 vLLM refuses the 49,152 window (estimated maximum 32,928). Binding the GSQ is an operator
  decision recorded in the ADR: 32,768 @ 0.90 (blind 8.42 already measured), or move the embedder off the box.
  **D5 = A (ADR 0049 Amendment 3): the GSQ is the bound lane at 32,768 @ util 0.90**, measured first — KV 43,690
  tokens (1.33x), the embedder 200 beside it idle and under 4-stream load, 7.17 tok/s single / 20.36 tok/s at 4
  streams — then wired live, and the seat joined llama-swap's `heavy` swap group so cascade seats swap with it
  instead of dying (HTTP 500 measured). **A vLLM seat declaration can now carry its bound-lane settings**
  (`agent_max_tokens`, `agent_thinking`, `agent_sampling`, `agent_timeout_sec`, `agent_seat_tok_s`): validated at
  render with the harness config's own rules, emitted by `Bindings()` only when set, untouched in
  `FallbackBindings()`. Until now the installer bound a vLLM seat as the agent lane at the tier's `config_seed`
  values — the FALLBACK seat's 1,024 tokens and 300 s — which is the configuration the gate measured failing (3/8
  at 300 s, 8/8 at 900 s). `profiles.json` declares the GSQ at 4,096 / thinking off / vendor sampling / 900 s /
  `agent_seat_tok_s` 7.17 — the single-stream rate measured at THIS operating point (its completions never reach the 1,024 tokens a rate sample needs, so the D-03 auto wall
  needs the seed). Tier docs render the utilization and the bound-lane table.
  The seat declares its engine floor as data: `engine_min_version` 0.29.0 plus the checkpoint's
  `patch_vllm_qwen35_embedding.py`, which is CARRIED, not upstream (vLLM `main` still builds a stock
  `VocabParallelEmbedding` for Qwen3.5). On 0.28.0 the patch applies and is still insufficient.
  The BOUND agent lane is untouched: `config_seed.agent_model` stays on the llama.cpp fallback, and the
  TIER-level `agent_ctx_tokens` stays 131,072 because it describes the bound lane, not the vLLM seat — a
  distinction `TestAgentWindowMatchesWhatTheAgentSeatServes` caught when this change first got it wrong.
- **vLLM is a first-class engine on EVERY tier, and a tier never loses it as a side effect** — ADR 0048.
  The A-07 ship (ADR 0047) deleted `ampere-16`'s entire `vllm_seat` object from `setup/templates/profiles.json`
  in order to change a MODEL. That stopped the installer rendering the tier's systemd unit, both llama-swap
  wrappers, the polkit rule and the llama-swap entry, and it removed the tier's only path to the LMCache cache
  server, which ADR 0045 defines per vLLM seat. The seat is **restored** unchanged (`qwen3.5-4b-vllm`); the A-07
  seat verdict and the held binding are untouched.
  **Both existing guards passed through the deletion**, because each `continue`s on a nil declaration
  (`agent_budget_test.go:64`, `vllmseat_table_test.go:35`) — a tier that loses a capability falls out of the
  iteration instead of failing it. New gate `TestEveryTierCanSeatAModelUnderVLLM` asserts the SET of tiers: a
  `vllmSeatTiers` regression floor that names the tier and the seat it lost, a countable `vllmSeatDebt` for the
  tiers that cannot yet seat one (today **3 of 16 covered, 13 owing**), and a failure for any tier tracked by
  neither. `TestEveryDeclaredVLLMSeatValidates` now FAILS instead of skipping when nothing is declared, and
  `TestAmpere16AgentSeatIsTheMeasuredWinner`'s assertion is inverted — it required the absence of a `vllm_seat`.
  Mutation-verified: re-applying the deletion turns the new gate red while both old gates still report `ok`.
- **`ampere-16`: the 27B agent entry renders, but the DEFAULT BINDING is held on the 4B** — ADR 0047, after the
  live check. The seat decision is unchanged and unanimous (24/24 blind), but deployed to the reference box the
  winner returned 2/8 and then 0/8 on `contracts/digest-8.json` through the fleet node, every failure
  "wall timeout after 300s". A dispatched contract carries `timeout_sec` = `core.AgentTimeoutSecDefault` (300)
  stamped by the DELEGATOR in `internal/delegate/intake.go`; the node's decoder leaves it, and
  `Config.AgentTimeoutSec` is read only on local-run paths — so no node-side setting extends a remote
  contract's wall, and a 6.3 tok/s seat cannot finish one. The earlier claim in this changelog that seeding
  `agent_timeout_sec: 900` gave the seat room was WRONG. `config_seed.agent_model` stays on
  `qwen3.5-4b-agent` (re-verified 8/8 at walls 36-132 s) and `agent_ctx_tokens` back to 131072; the 27B entry
  is callable by name with a caller-supplied `timeout_sec` up to the 900 s cap, and becomes the default when
  the wall is derived from the seat's measured rate (register D-03). `TestAgentWindowMatchesWhatTheAgentSeatServes`
  now resolves the seat from `config_seed.agent_model` rather than from a gate flag, because a tier may render
  several agent-capable entries and only the binding routes the lane.
- **`ampere-16` agent seat is now `qwen38-27b-agent`** (Qwen3.8-27B UD-IQ3_S with the MTP head embedded in the same GGUF, `--spec-type draft-mtp --spec-draft-n-max 3`, literal `--ctx-size 49152`, q8_0 KV, `--reasoning off`) — ADR 0047, register A-07. Re-audited blind on the standard instrument (24 Opus judgements, three lenses, Latin-balanced, matched budget) on a card held at its accepted 40 W / 1200 MHz profile and cooled before every arm: it beats the previous seat `qwen3.5-4b-vllm` **24/24** (9.32 vs 5.39), Gemma 4 12B + MTP 24/24 and gpt-oss-20b + EAGLE-3 24/24. The tier drops its `vllm_seat` declaration, advertises `agent_ctx_tokens` 49152, and seeds `agent_max_tokens` 4096 / `agent_timeout_sec` 900 (the seat decodes at 6.3 tok/s, so an inherited budget is silently floored by `FitFinalBudget`). The llama.cpp `qwen3.5-4b-agent` entry stays rendered as the fallback. New tier gate `include_qwen38_27b` + `__Q3827B_ALT__` in both CUDA templates, covered by the serving-config provenance hash.

### Added
- **A contract that names no `timeout_sec` is sized by the node that runs it (register D-03, `timeout_auto`).**
  Two defaults turned "unset" into a legitimate-looking 300 before anything could tell it from a caller who asked
  for 300 (`intake.go` on the delegator, `DecodeAgentContract` on the node), so a 29 tok/s thinker ran every
  unsized contract at a wall sized for a fast seat. Intake now stamps the wire default AND `timeout_auto: true`;
  the executing node — both doors run the same `runAgentTask` — sizes the wall from the estimate it already
  publishes (`wallEstimateFor`: cold load + think block + tool steps + final answer + re-pack at the seat's
  remembered rate), clamped to the wire bounds 300..900, and reports what it ran under as `wall_sec`
  (`wall_note` prefixed `auto wall`). A seat with no rate yet runs the default, as before. The delegator budgets,
  polls and re-places an auto contract at the cap (the node's wall lands anywhere inside it), every retry and
  re-placement clears the marker (its wall is what is left, an explicit number), placement's long-seat prefill
  check sizes an auto contract against the cap instead of deferring it at 300, and the review lane keeps an
  owner-set `agent_timeout_sec` explicit (a box that sets none is auto-sized like any other caller). An older
  node ignores the field and runs the default; an older delegator never sends it — wire-compatible both ways.
  **Accepted cost:** a node that acks and then dies silently is abandoned at the cap, 900 s instead of 300 s, on
  this now-default path — the delegator cannot tell a slow seat under a sized wall from a dead node without
  waiting; the tighter bound (the node's advertised `seat_rate`) is a follow-up.

### Fixed
- **A cold seat no longer runs its whole task at an 8,192-token window.** On 2026-09-16 the same `agent_run`
  reported `ctx_window` 8,192 cold and 114,688 warm, minutes apart, on `<node-b>` `agent-pool` seat: a silent 14x
  loss that exhausted five compactions and truncated every file read. Replayed against the unloaded seat,
  llama-swap reported `starting` at 3 s and `ready` at **222 s**, while the window probe gave each per-model URL
  a fixed 60 s timeout, gave up at 60 s and 120 s, fell to the bare-root `/props` (404) and returned the
  fallback. Three fixes, one per surface of the defect:
  - the MCP `agent_run` door now runs the **cold-load warm-up** (D-64) the delegation door has run since
    0.115.11 — the seat loads on the admission budget BEFORE the wall, and the result reports it as
    `admission_wait_sec` / `admission_note`, the wire's own names. The two doors had drifted;
  - `ProbeServedWindow` gives both per-model probes one **cold-start budget** (10 min, llama-swap's
    `healthCheckTimeout` on the reference boxes) carried by context, instead of 60 s each; the bare-root probe
    keeps 60 s. Bounded when the caller's context has no deadline;
  - when the probe still cannot answer, `ResolveContextTokens` uses the box's configured `agent_ctx_tokens`
    before the 8,192 fallback, and when the probe DOES answer and disagrees with the config, the note names
    the disagreement — the served window wins, and a stale config is surfaced before it can mislead the
    fallback path.
  Mutation-verified at each call site: skipping the warm-up, restoring a per-request timeout, unbounding the
  wait and dropping the disagreement note each turn their test red.

## [0.125.1] - 2026-09-15 - a cascade lane call never waits on this box's GPU lease (C-41c); the write-door gate's proof applies its patches (D-114)

### Fixed
- **A cascade lane call no longer waits on THIS box's GPU lease (register C-41c).** The 0.125.0 readback under a
  foreign media lease logged `cascade remote lane: gemma-4-e4b -> http://<node>:18811/fleet/chat (local GPU lease held)`
  and the call still deferred after the full two-minute bound: `llamaclient.sendWithSeatWait` admitted every send
  through `modelaffinity.Admit`, whose card wait reads the machine-local lease whatever the endpoint, so a request
  bound for another box sat at home and the node never saw it. `endpointChoice` now carries `offBox` (a lane, or a
  seat pinned to another node) and such a send admits through `modelaffinity.AdmitOffBox`, which keeps the in-process
  per-base queue and skips the card wait; the local base still waits (pinned as the control). Red first:
  `TestAdmitOffBoxIgnoresTheLocalLease` and `TestFleetLaneSkipsTheLocalLeaseWait` reproduced the live shape against a
  held media lease, then went green.
- `scripts/write-door-gate.ps1` applied NOTHING and then judged the unpatched copy (register D-114 readback, 2026-09-15 0.125.0
  gate on the `<node-a>` 9B: VERDICT 1/4 while every seat diff was correct — t4 was exactly `Status: draft` → `Status: final`).
  `git -C <copy> apply` from a SUBDIRECTORY of this repository resolves patch paths against the repository root and skips
  every path outside the subdirectory with exit 0 (`Skipped patch …`). The proof now `git init`s a throwaway repository in
  the fresh copy so the copy is the root, and a skip — or any apply that reports no `Applied patch` — fails the task
  instead of passing it to a proof that cannot run. Reproduced and fixed on the saved t4 patch.

## [0.125.0] - 2026-09-15 - the review lane and the cascade ride the fleet when the local seat is held; a cut tool call is a budget defect; delegate rows carry their calls (D-110, C-41, D-114, D-99, D-100)

### Fixed
- **A cut tool-call argument is a BUDGET defect, never infrastructure (register D-114).** When the seat's
  completion budget runs out in the middle of a tool call's JSON argument, the engine refuses the call -
  llama.cpp answers HTTP 500 `Failed to parse tool call arguments as JSON ... invalid string: missing closing
  quote`, vLLM-shaped engines return the completion on `finish_reason: length` with the fragment attached.
  The loop returned `stop_reason: error` and the node filed `defer_class: infrastructure` with `write_note:
  the seat wrote nothing` - the stack blamed for a ~3 KB single-call write asked for at a 1,024-token step
  budget (measured 2026-09-15, `<node-a>` 9B, 2 steps / 135 s). Now `internal/agent/loop.go` recognises both shapes,
  re-issues the SAME step ONCE at the wall-fitted final budget (4,096 on the fleet seats) with the transcript
  unchanged and the fragment never appended or executed, and on a second cut ends the run on the new
  `stop_reason: tool_call_cut` whose `stop_note` names both budgets and the partial argument size
  ("tool-call argument cut at the completion budget twice (step 1024 tok, re-issued at 4096 tok; partial
  argument 2847 chars) ..."). Every cut attempt appends a call record, so `results[].calls` shows the budget
  each one generated at. `internal/pipeline/agenttask.go` maps the new stop reason to `defer_class: budget`
  with that note as the reason. An unrelated 500 still stops on `error` and still reads as infrastructure.
- **A cascade lane can now REACH a fleet node at all (register C-41b — the second half of C-41).** C-41 taught the
  lane to fire when a local swap would have to wait. It could still not reach anything: both fleet nodes bind
  llama-swap to `127.0.0.1:11436` and nothing else, so a lane base of `http://<node>:11436` is unreachable from
  another box, every residency probe failed, and the lane logged "not resident" for ever while the measured symptom
  reproduced. Binding llama-swap to the tailnet would be a new unauthenticated listener; the vision lane (0.116.0,
  ADR 0040) already solved this shape, and the cascade now gets the same door. **Node:** `POST /fleet/chat` on
  `:18811` forwards ONE OpenAI chat completion, byte for byte (the grammar survives), for a model this node's own
  roster serves — alias-aware, 404 for anything else, 503 when the roster is unreadable, the upstream status copied
  back unflattened so the caller's seat-wait loop still sees llama-swap's 429/503. Bearer-gated by the agent lane's
  rule verbatim (`fleet_auth_token` required past loopback), body capped at 8 MiB, and advertised as `chat_lane` in
  health exactly when it will admit — with `served_models` now published whenever the chat lane is admissible, not
  only under the agent lane. It is a synchronous forward, not a job: a short cascade call does not belong in the job
  store. **Delegator:** `llamaclient.FleetLaneGates(fleet_auth_token)` replaces `RosterResident` at all three client
  constructions and returns the residency gate and the lane ROUTER over ONE cached probe per base — a base that
  answers `/fleet/health` with a `node_id` is a fleet node (residency = `served_models`, route = `/fleet/chat` plus
  the bearer); anything else is a plain llama-swap and behaves exactly as before (`/v1/models`, the client's own
  path, no credential). Fail-closed throughout: a node that does not advertise `chat_lane`, does not serve the
  model, or that this box holds no token for past loopback is NOT resident, so the call stays local and logs why
  once per window.
- **A cascade tier no longer waits out its HTTP deadline behind another session's seat (register C-41).**
  `cascade_remote_lanes` already reroutes a cascade call to a node that roster-serves the SAME model, but its
  only busy gate was `delegate.LocalBusy` — the machine-wide GPU lease. The measured symptom carries no lease:
  another session's contract holds `qwen3.8-27b-vllm` through llama-swap, the `interactive` set is mutually
  exclusive, so every cascade tier (e2b / e4b / 12b / 26b) needs a swap — and llama-swap swaps only once the
  loaded model's in-flight requests finish (300–900 s contracts). The call sat in that queue until its own
  deadline and the tier deferred, with a fleet node idle and serving the identical GGUF. `WithRemoteLanes` now
  takes a second, PER-MODEL gate beside the lease one (either firing takes the lane, residency still required):
  `llamaclient.LocalSwapBusy(endpoint)` reads this box's llama-swap and answers "would a swap to this model
  have to wait?" — another model listed `starting`/`stopping`, or `ready` with `internal/seatload`'s gauge (the
  reader behind `gpu status`'s "N request(s) in flight") above zero. Alias-aware through the roster, so a seat
  bound by alias is not mistaken for a different model; a loaded but idle model, the requested model itself
  being loaded, and every unreadable probe all read as NOT busy — fail-closed to local, exactly as
  `RosterResident` does. One reading cached 5 s per base; each reroute logs its reason
  (`cascade remote lane: gemma-4-e4b -> http://<node>:11436 (local seat busy: qwen3.8-27b-vllm, 1 in flight)`).
  Wired at all three client constructions (CLI/MCP `openPipeline`, the in-loop pipeline, the re-pack lane), so
  every cascade door gets the same failover. A config without `cascade_remote_lanes` is unchanged.

### Added
- **`results[].calls` on every delegate row (register D-99).** The `agent_delegate` tool description promised the
  per-completion record — `finish_reason`, `completion_tokens`, `reasoning_tokens`, `thinking_off`, `sampling`,
  `ms` — but `delegate.WireResponse` dropped it when it shaped the node's `AgentWireResult` into the published
  row, so a delegator could read it only from the node's own `GET /fleet/jobs/{id}` with the fleet token (how
  the D-95b measurement read `calls[].sampling`). The row now carries the LAST eight completions (the final
  answer and the structured re-pack included), `omitempty` so a node that recorded none publishes a
  byte-identical row. Pinned by `TestWireResponseCarriesTheLastEightCalls`, which reads the JSON, not the struct,
  and was red before the field existed. MCP, CLI and the opencode plugin all publish through `WireResponse`,
  so the field appears on every surface at once.
- `contracts/write-door/t4` (register D-114): the measured shape as a write-door gate leg - a ~3 KB
  `release-notes.md` whose status line must be flipped with ONE `write_file` call carrying the complete file.
  `scripts/write-door-gate.ps1` is now a FOUR-task gate and proves t4 by comparing the applied copy line by
  line against the fixture: same line count, exactly one differing line, and that line `Status: draft` ->
  `Status: final`.
- **The review lane rides the fleet when the local seat is fenced, and a lease refusal says when to retry
  (register D-110).** `offload_review_diff` built its local agent loop unconditionally, so under another
  process's exclusive or draining GPU lease it waited the whole `agent_lease_wait_sec` at the affinity cordon
  and was filed as a capacity defer — for the lease's entire length, however idle the fleet was. Two changes:
  (1) `modelaffinity.LeaseError` now appends the holder's DECLARED window to its message
  (`, declared until 11:40PM (~37m0s left)`) whenever the lease carries one — the idiom `gpulease.ErrHeld`
  has rendered since 0.113.14, now on the refusal a delegation actually reads; the wait is still never bounded
  by that TTL. (2) `handleReviewDiff` reads the machine-wide lease first and, when `delegate.ForeignFence`
  says the seat is fenced by someone who is NOT this process, hands the SAME contract to `delegate.RunWith`
  at route `remote` over the configured `delegate_remotes`. The quality floor is `remoteEligible`'s, unrelaxed
  — agent lane advertised, card not leased, seat resident, diff + `specReserve` inside the node's
  `agent_ctx_tokens` — and route `remote` (never `auto`) is what stops a fallthrough onto the fenced local
  seat. The result names where it ran (`executed_on`, `node`, `placement`, `seat`) and the fence that moved
  it, and its findings go through the same grounding/echo/dedupe/cap filters as a local review
  (`publishReview` is now shared by both paths). An INHERITED lease (`GPU_LEASE_EPOCH`) is not a fence: the
  holder's own review stays on the cards its lease cleared. When no remote is eligible today's path stands —
  the wait, then the capacity defer — with a `fleet` note saying the fleet was asked and why it declined.
- `contracts/digest-8-grounded.json` (register D-100): the eight digest contracts of `digest-8.json` with ONE
  grounded check per subtask on identifiers the document names and the goal does not. The old fixture's
  acceptance is shape-only (`min_items:findings:3` + `nonempty:summary`, which the intake lint says on every
  run), so an 8/8 on it proves completion and schema fill, not digest correctness. The grounded fixture runs
  BESIDE the old one for three K×8 runs before it replaces it, so a step in pass rate is attributable to the
  acceptance and not to the seats.

### Changed
- `contracts/digest-8-grounded.json` v2: the single `contains:` per subtask became a `regex:` alternation over
  the document's CENTRAL identifiers (each alternative checked present-in-document and absent-from-goal at
  build time). v1's anchors (`installed.json`, `agent_model`, `gpulease.InspectDir`, `gpt-oss-20b`,
  `delegate-intent.jsonl`, `fleet_queue_holder`, `harness-loop-guard.js`, `AnchorCheck`) each occur exactly
  once in their document — a table cell, a passing comparison — and correct digests skipped them: measured
  2026-09-15 on quiet seats, the 9B passed 2/8 and the 4B 4/8 with every contract completed and the
  spot-checked digests substantively right. An acceptance that fails correct answers measures the anchor, not
  the seat; the three side-by-side runs restart with v2.
- `contracts/digest-8-grounded.json` v2.1: the ADR 0026 alternation gains `tryJoin|Admit` and the ADR 0030 alternation
  `holder|Option B|slack` — v2 run 1 on the 9B and the 4B (7/8 each, 8/8 on the 27B) missed exactly those two, and both
  digests were correct ones anchored on other central identifiers of their document. The three side-by-side runs count
  from v2.1.

## [0.124.0] - 2026-09-15 - every ledger row names the session that asked and the job behind it (D-101)

### Added
- **Ledger rows carry their origin and their job (register D-101, ADR 0046).** `ledger.Record` stamps every
  row — cascade, agent, delegate, media, whichever site wrote it — with `origin_session` (from
  `LOCAL_OFFLOAD_ORIGIN` when a caller names itself, else `CLAUDE_CODE_SESSION_ID`, which Claude Code
  exports to every child process: one MCP server is one session), `origin_pid` / `origin_ppid`, and ONE
  token figure, `cards_tokens`: the seat's prompt work plus what it generated (`seat_tokens_in` or
  `tokens_in`, never both, plus `tokens_out`), 0 on a cache hit and on a defer that never reached a model, the
  chars/4 estimate only for a completed render that recorded no counts. The `cards_tokens` key is always
  written, so its presence marks the new schema; pre-0.124.0 rows read as UNATTRIBUTED, never as another
  session's. Delegate rows also carry `job_id`, `route`, `placement` (the placement note, capped like
  `reason`), `steps`, `stop_reason`, `repack_ms` and `acceptance_result` (`pass` | `fail` | empty when nothing
  was evaluated); agent rows carry `job_id`, `steps`, `stop_reason`, `repack_ms`; vision rows the route's
  `placement`. `core.Meta` gains the same four job fields (omitempty). Why: the harness-share gate summed every
  ledger row in a session's time window, so a session was credited with every concurrent session's delegations
  (a five-call session read 35 %) and a session that routed everything it had could still be blocked — the floor
  could not be enforced honestly until rows said who asked. A per-session share is now one filter away:
  `grep '"origin_session":"<id>"' ledger.jsonl`.
- The write door's three-task gate: `contracts/write-door/{t1,t2,t3}` (a one-file Go fix, a two-file Go
  fix plus the table case that exercises it, a JSON + Markdown record edit) and
  `scripts/write-door-gate.ps1`, which sends them to one seat (`-Remote http://<node>:18811` or this
  box's own seat), applies each returned diff to a FRESH copy and proves it there (`go test` red → green,
  a JSON parse, exact-row checks) — the proof a caller of a write contract owes, since the harness never
  applies its own writes. Measured 2026-09-15: 3/3 on a 4B vLLM fleet seat (18 / 45 / 48 s), t1 in 10 s
  on a 27B seat (register D-06). The fixtures are pinned `eol=lf` so a seat's LF diff applies on every
  checkout. Hardened after a clean-context review: the gate fails on a non-zero CLI exit, pins the EXACT set
  of files each patch may touch itself (the contract's diff verbs are the delegator's check, not this proof's),
  requires t1's pre-patch red to be the staged case, and both new guards were shown red with stub CLIs.

### Security
- Operator identity scrubbed from the public tree, and a gate that keeps it out
  (`TestTrackedTreeCarriesNoOperatorIdentity`, root package): a Windows profile path in a plan, the
  development-drive layout in two plans, a doc, a doc comment and three test fixtures, the cloud-synced
  workspace path in a spec and a doc, a real node's hostname in a config comment, a collaborator's and the
  operator's usernames in fixtures (`someone` / `~` / `%USERPROFILE%` now), a tailnet example outside the
  documentation range (examples stay in the lowest /22 of the CGNAT block), and the vendored llama-swap CLI's
  metadata and copyright headers naming a second GitHub account (now the repository owner, read from
  `go.mod`). The rules name shapes, never the values; placeholders the rules admit are documented in the
  test. The tracked machine-config backup `config.backup-2026-08-11-pre-0.47.0.json` is removed from the
  public tree (kept in the private evidence repository); `.gitignore` now covers every backup spelling of
  the working config (they carry the fleet token) and the second TTS venv. History is not rewritten: the
  paths it carries were already public and hold no credential (the fleet token never entered history).

## [0.123.3] - 2026-09-14 - a cut prose final is re-issued; repetition loops are cut; agent_sampling (D-95b)

Register D-95b, from the live readback of 0.122.1 on the `<node-c>` 4B seat (`qwen3.5-4b-vllm`, vLLM 0.28.0, step 1,024 /
final 4,096, thinking off, 14.9 tok/s). The `METHODOLOGY.md` digest STILL deferred at 381 s with `output_truncated` and
`re-pack skipped … a partial cannot be re-packed`, for two reasons 0.122.1 did not cover: the partial was PROSE-shaped
(`summary (≤100 words):` / `mechanisms:` / `- item` — that seat's normal answer on a schema contract, which the
ordinary re-pack reads fine), so the JSON-shape gate excluded it; and the answer had degenerated into a REPETITION
LOOP, one four-line block under `numbers:` repeated about twenty times until the budget ran out, which nothing in the
harness could see.

### Fixed
- The list-cap re-issue of a `length`-cut final fires for ANY partial on a schema contract, prose or JSON. The
  JSON-shape precondition (`jsonShapedPartial`) is gone; the two gates that bound it — an `output_schema` is set, and
  the wall still holds one more turn at the seat's measured rate — are unchanged, as is the one-re-issue bound.
  `TestCutProseFinalOnASchemaContractIsReissuedWithListCaps` replaces
  `TestNoListCapReissueOnProseThatIsNotAPartialObject`; the no-schema and no-wall refusals keep their tests.

### Added
- **Repetition guard** (`internal/agent/repetition.go`, wired into the loop's final handling). A final whose tail is
  the same normalised block of 1–32 lines repeated 4 or more times in a row — the last block may be cut off mid-way —
  is treated as a CUT answer even when the engine reported `stop`: the run records
  `stop_note: repetition loop (20× "- 100 words cap for summary (enfor…")`, sets `output_truncated`, and re-issues ONCE
  with the list caps PLUS an explicit "do not repeat any line; write each list item exactly once" sentence. A second
  loop abstains exactly as a second cut does. `calls[].finish_reason` is NOT rewritten — it stays what the engine said.
  The repeated tail is trimmed off the partial that rides in `output`: one copy plus `[repetition trimmed ×N]`.
  Table-tested against the live 20× loop, a 4× loop (the threshold), a 3× repeat that must NOT trigger, a legitimate
  list of 30 distinct items, single-line loops, prose and blank-line runs.
- **`agent_sampling` / `agent_sampling_final`** (box config, `core.AgentSampling`): the executing seat's decoding
  policy for planner calls and, separately, for the thinking-off FINAL answer turn and its re-issues. Objects of
  optional `temperature` / `top_p` / `top_k` / `presence_penalty` / `repetition_penalty`. Every knob is a pointer: an
  unset one is ABSENT from the request body, so the seat keeps its own default, and an absent or empty object
  reproduces today's request byte for byte (`temperature: 0` and no other sampling key — which is also exactly the
  greedy decoding that produces the loop above). Out-of-range values fail the config LOAD, named by key. The effective
  policy of every completion is published in `calls[].sampling` (`temperature=0` for the default) on the loop record
  and the node/delegator wires, so a measurement can prove which decoding produced which answer. No house default and
  no seeded value: a sampling setting is a per-seat measurement.

### Docs
- OPERATOR-GUIDE "The timeout chain": the re-issue's gate list drops the JSON-shape condition, plus new paragraphs for
  the repetition guard (the detector rule, what the caller sees) and for `agent_sampling` / `agent_sampling_final`; the
  agent config reference block names both keys.

## [0.123.2] - 2026-09-14 - one box, three tiers: placement is a per-task decision (ADR 0052)

The reference workstation is not one hardware tier. It is three 16 GB Blackwell cards — a
measured 5060 Ti pair (devices 0 and 2) and the RTX 5070 Ti that drives the desktop — which makes
it simultaneously a complete `blackwell-16`, a complete `blackwell-2x16`, and the installed
`blackwell-3x16`. Filing it as one row made the fleet read one capacity row where three exist and
turned every placement into a fixed binding. The operator's directive was to make the box *be*
its tiers ("`<node-b>` = blackwell-16 / 2x16 / 3x16 routed per task") and the design council's verdict
was RESHAPE: window overflow escalates to the PAIR's long seat and only when the pair's agent seat
is idle, saturation is recorded and never acted on, one placement table serves local and remote
alike, the display-card guards fail closed with footprint arithmetic, and the display layer ships
dormant until the operator enables it. Both are recorded in ADR 0052.

### Added
- **`composes` + `layers` in the tier table, seeded into config.** `blackwell-3x16` declares the
  tiers it is a complete instance of and the device layers it routes work to; `tierseed` seeds
  `tier_profile`, `tiers` and `layers` into `config.json` (the pair's agent seat is left bare in
  the table and filled from `vllm_seat`, so the seat's numbers live in one place). `install.ps1`
  mirrors it, and `setup/render.tests.ps1` now compares its output against `install seed` field by
  field.
- **`internal/placement` — one decision table.** (task class, token need, quality gate,
  `context_class`, budget, live occupancy, live guards) → a `Decision`, used identically by the
  local box, the delegator over a remote's advertised rows, and the node's re-check at admission.
  Live readers are memoised for 2 s, so a 32-subtask spread execs `nvidia-smi` once, not 32 times.
- **`internal/gpuprobe`** — the nvidia-smi parser, runner and host-RAM reader as a leaf package,
  with index-or-UUID device resolution (the display card is pinned by UUID because the board
  reorders CUDA indices on power loss).
- **`placed` on every result** — `{tier, layer, role, seat, devices, ctx_tokens, reason, guard,
  evicts}` on the agent wire, `agent_run`, the cascade result and the ledger row. `devices` is the
  seat's own pin, never the layer's alternatives. `results[].placement` stays the string it was.
- **`context_class` on both agent doors** (`""` | `"long"`) — the one `tools/list` change in this
  release, present on every box.
- **Health advertises `tiers` and `layers`** on a composite node, lane-gated: the layer spec, each
  seat's `served` flag from the roster the residency refresh already fetched, and the node's own
  admissibility verdict per layer (the display-card guards can only be read where the card is).
  Pinned to exactly two roster GETs per TTL across 21 health requests.
- **`offload_status`** carries `local.tier_profile`, `local.tiers`, `local.layers` and
  `fleet.nodes[].layers`; the render is bounded and degrades to spec rows rather than dropping the
  table, and it never loads a model.
- **A dormant display layer.** Two rungs pinned to the display card render behind a fence in the
  triple template and join a matrix set that runs them BESIDE the vLLM seat. Placement never routes
  there until the operator sets `dormant: false`, and every placement onto it passes the
  display-floor, host-RAM and presence guards.
- **`scripts/check-deletions.sh` — an undeclared deletion does not reach main (H-51).** Run by
  the CI `build` job on both events, because the two failure shapes differ: a pull request is
  diffed against the base branch tip, and a push to `main` against the MERGE COMMIT’S FIRST
  PARENT — the relationship nothing looked at, and the one that let #324 through. Every deleted
  path must appear on a `Deletes:` line in the PR body or a commit message, or in a `DELETIONS`
  file. `TestDeletionGuardRefusesAnUndeclaredDeletion` drives it over synthetic repositories,
  including a reconstruction of #324’s exact commit shape and an honest merge it must not flag.
  Documented in CONTRIBUTING.md.
- **`servingtmpl.CheckComposite`** — a composite render must be the checked union of what it
  composes; `install render` refuses one that is not.
- **`docs/systems/composite-tier.md`** and **ADR 0052**; `docs/tiers/` pages gain a Composes
  section with the layer/seat/guard table.

### Changed
- **The contract context cap is the box's.** 256 KiB on a plain box; on a composite box
  `min(2 MiB, largest layer window × 3)`. At chars/3 a 256 KiB contract estimates ~87k tokens, so
  without this a composite box's own 262k seat was unreachable through its own front door. Both
  agent doors, `intake.PrepareContract` and the node's decode validate at that cap.
- **Seat resolution on `agent_run`.** With no per-call model the placement table decides and its
  seat outranks `agent_model`; a per-call model that belongs to an opt-in layer is admitted only if
  that layer's guards admit it right now.
- **A job dispatched at a layer names that layer's seat on the node's job feed**, instead of the
  planner default.
- **`local-offload report`** names the tier from config when no installer manifest exists, saying
  which source answered and what the tier composes — the composite box reported "hardware tier
  UNKNOWN" while every placement it made was keyed on `blackwell-3x16`.
- **The tier table declares no `triple` layer.** Its only seats were the three-card Flash-Next
  arms, removed by the 2026-09-10 operator rule that RAM is overflow only (0.115.4), so
  `context_class: long` resolves to the pair's `qwen3.8-27b-262k` under the same eviction rule
  window overflow takes. The table still serves a three-card layer for a box that declares one.
- `CONTRIBUTING.md` named `main.go` as the home of the version const; it is
  `internal/buildinfo/buildinfo.go`.

### Fixed
- **A two-card device pin was serving ONE card.** `env: [CUDA_VISIBLE_DEVICES=0,2]` is valid YAML
  that parses as two entries — `"CUDA_VISIBLE_DEVICES=0"` and `2` — in the same parser llama-swap
  uses. Every seat pinned that way rendered as a one-card seat: the triple template's
  `qwen3.8-27b` and its `qwen3.8-27b-262k` twin (both `-sm layer --tensor-split 24,26`, both
  needing the pair) and every media seat with a two-card `gpu_env`. Entries a flow list would tear
  are now quoted; entries without a comma stay barewords, so every other render is byte-identical.
  Found by the checked union on its first run over the shipped tier.
- `internal/tierdocs` carried a dead `Extra json.RawMessage` field whose comment claimed a new
  tier-table field would surface in the docs automatically. It never could; the field is gone and
  the generator renders the composite keys explicitly.
- **The display layer is inside the provenance hash.** Restoring K-02 beside the composite tier
  made `TestParamsBasisMirrorsParams` fail immediately and correctly: the composite had added
  `DisplayLayer` to `servingtmpl.Params`, and a render param outside `ParamsBasis` is a param the
  `spec_sha256` does not cover — the config would render differently and the stamp would swear
  nothing changed. The whole `*config.LayerSpec` is hashed, not its name, because the template
  substitutes the layer’s rungs, device pin and guards into the rendered text. The round-trip half
  of that test now populates the field, so dropping it from `BasisOf` fails (mutation-checked).
- **Restores K-02 (0.123.0) and H-49 (0.123.1), dropped by the composite merge #324.** That
  branch was rebuilt by replaying its trees onto the 0.122.2-era base, so its merge commit
  carried a tree in which both releases had never happened — six files deleted, the
  provenance stamp and the `Sampler` seam gone, and this entry written under a version number
  K-02 had already shipped. Nothing in the gate reads a merge against its base, so nothing
  said so; `scripts/check-deletions.sh` (H-51) now does.

### Byte-identity
A box that seeds no `layers` publishes not one new key on `/fleet/health`, `offload_status`, any
agent result row, the cascade result or the ledger row, and renders its serving config exactly as
before. Pinned by `TestStatusOnAPlainBoxCarriesNoLayerKeys`,
`TestHealthPublishesTiersAndLayerRowsOnlyOnACompositeLane`,
`TestDisplayTwinsRenderOnlyForATierThatDeclaresADisplayLayer`,
`TestNoLayersIsNotCompositeAndRoundTripsWithoutTheKeys`,
`TestAgentDelegateRegistrationGated` and `TestHealthAgentFieldsAbsentWhenDisabled`.

## [0.123.1] - 2026-09-14 - offload_status verdict tests stop reading the live cards (H-49)

Register H-49. `TestStatusPublishesTheLocalLeaseWithTheQueueCommand` asserted a held lease over an idle
seat reads `held-idle`, but `offload_status`'s `gpuactivity.Snapshot` call always sampled the REAL
nvidia-smi — the test never wired the existing `statusSamplesGPU` seam to a fake. On any box where the
cards are genuinely busy, including this repo's own measurement runs, the verdict correctly read
`held-working` and the test failed on live hardware, reproduced identically on untouched `main` by four
agents while a measurement ran the pair at 100%.

### Fixed
- `gpuactivity.Options` gains a `Sampler` field so a test one package up (which cannot reach the
  package-private `smiRun` seam in `internal/gpuactivity/smi.go`) can inject specific per-card
  utilization instead of only turning sampling on/off. `internal/mcpserver` threads it through a new
  `statusGPUSampler` package var, mirroring the existing `statusSamplesGPU` seam.
  `TestStatusPublishesTheLocalLeaseWithTheQueueCommand` now sets a fake idle sampler for its own
  scenario; the seam's mutation-tested (a 99%-busy fake correctly flips the verdict to `held-working`
  and fails the test).
- Added `TestLiveStatusVerdictReadsTheRealCards`, gated behind `OFFLOAD_LIVE_GPU=1` (the
  `TestLiveWindowsProbes`/`OFFLOAD_LIVE_PDH` convention), asserting only vocabulary + sanity bounds
  against the real nvidia-smi — the receipt that the new seam is genuinely wired, not a dead field.

## [0.123.0] - 2026-09-14 - the rendered serving config carries its provenance

Register K-02. The llama-swap config is rendered ONCE, at install, from the tier table baked into the binary, and
nothing re-renders it or recorded what it was rendered FROM. `audit-yaml` checks RULES, and a config that is a tier
revision stale breaks no rule - so it reported OK on ampere-16 for the whole time that tier's served window said
32768 in a file whose seed had been raised to 131072 (register A-39, 0.113.32). A rule gate cannot see staleness.

### Added
- **The provenance stamp.** `install render` now writes a six-line comment block at the head of every config it
  renders: `spec_sha256` (the sha256 of a CLOSED, documented input set - tier id, the render Params, the template
  bytes, the tier's own profiles.json entry, the harness version, canonicalised as sorted-key JSON with number
  literals preserved), `body_sha256` (the yaml BELOW the stamp, so a hand edit stays distinguishable from a seed
  change), `rendered_by`, `tier`, `rendered_at`, and the canonical basis itself. The closed set mirrors
  `servingtmpl.Params` one field for one field and `TestParamsBasisMirrorsParams` reflects over both structs, so a
  field added to Params cannot silently fall outside the hash. Design mirrored from `internal/agent/props.go`
  (`SeatPin`): a named closed set, never a language-default hasher, never a hash of the raw bytes.
- **`local-offload audit-yaml --against-render`.** Re-derives each file from THIS binary's embedded tier seeds and
  reports exactly one of MATCH / STALE(`<keys>`) / UNSTAMPED / HAND-EDITED. STALE NAMES the basis keys that moved
  (`params.ctx_size`, `profiles_entry_sha256`, ...) - a verdict that cannot say which input changed sends an
  operator to diff two configs by eye. Positive control, and the row's kill criterion:
  `TestPreA39AmpereConfigIsReportedStaleNamingCtxSize` renders the pre-A-39 ampere-16 seed reconstructed from git
  history (`testdata/profiles-pre-a39-ampere-16.json`, the entry at 549f360), stamps it, and asserts the audit says
  `STALE(params.ctx_size, ...)` while the rule audit still says OK. Flags come BEFORE the files.
- **The stamp never re-introduces an unsubstituted token.** A rendered serving config must contain no `__TOKEN__` (Render refuses one; `setup/render.tests.ps1` greps every tier for the pattern), but the basis legitimately carries them -- a tier's media seats declare their binaries as `__OFFLOAD_HOME__/...` and the hash covers the render inputs AS GIVEN. The basis line therefore escapes the second underscore of every doubled pair as `_`: same string on decode, same spec hash, no forbidden pattern. `TestStampNeverReintroducesAnUnsubstitutedToken`.
- **`/fleet/health`: `serving_config_spec_sha256` and `serving_config_state`.** NEW KEYS on the EXISTING endpoint -
  no new route and no new bind. Published only when the new `serving_config_path` config key names this node's
  rendered config; both are omitted otherwise, so "this node does not report" stays distinguishable from "this node
  reports MATCH". The verdict is cached on the file's (mtime, size): health is polled every few seconds by every
  delegator and the verdict costs a re-render.

### Changed
- **One derivation, two callers.** `install render`'s tier resolution moved into `deriveRender`, which both the
  renderer and the `--against-render` replay call. A second copy of that logic for the audit is how a gate comes to
  certify the thing it was written to catch. The replay pins the per-box inputs the stamp recorded (install paths,
  listen address, thread count, the vLLM deployment half) rather than re-deriving them from the auditing machine,
  which would report every node stale.
- **`install render --out` writes a stamped config**, and its success line now carries the spec hash. The rule audit
  runs on the UNSTAMPED render, exactly as before, and the block is prepended after it - the stamp is inert yaml and
  `TestEveryShippedTemplateStampsAndSelfVerifies` renders, stamps and re-verifies all eight shipped templates.

### Deliberately not done
- **The verdict is settled by RE-RENDERING, not by comparing hashes.** A basis hash moves on inputs that cannot
  change the output (a tier's `measured` prose, a version bump with no renderer change), and a gate that cries stale
  on a documentation edit is a gate operators learn to ignore. MATCH therefore means the verifiable thing:
  re-rendering from this binary's seeds reproduces this file byte for byte AND no seed input moved.
  `harness_version` stays in the identity hash but is excluded from the verdict, or every node would read STALE
  after every release.
- **UNSTAMPED does not exit 1.** Every config on the fleet today predates stamping (verified read-only on all three
  reference nodes). Failing on it would make the session-start audit red on every box from the moment this ships.
  It prints as a finding; STALE and HAND-EDITED exit 1.
- **Nothing re-renders a live config.** The node only READS `serving_config_path`. Re-rendering stays `install
  render`, run by a human.
## [0.122.2] - 2026-09-14 - measured winners wired: the 27B fan-out twin, `-sm tensor` everywhere it belongs, the pair seat's own utilization, the 3-card media roster

Four ⚑WINNER rows of the harness master plan v2 (A-70, A-25, A-22, A-15) were measured on the reference box
and never reached the tier seeds, so a fresh render shipped the loser of each measurement. This release wires
them, each with the gate that keeps it wired and an assertion that it does NOT appear where it was not measured.

### Added
- **`qwen3.8-27b-par8`, the 27B's FAN-OUT twin, on both pair-spanning Blackwell templates (A-70).** The 27B
  llama.cpp seat ships `--parallel 1` and is flat under fan-out — 34.6-37.4 tok/s aggregate at c1..c32, and it
  answers 429 from c16 (6 of 16 and 22 of 32 requests failed in the baseline arm). On the SAME 5060 Ti pair at
  the SAME `-c`, `--parallel 8` holds single-stream (W1 43.4 vs 43.6) and roughly TRIPLES aggregate: 78.7 /
  110.8 / 100.7 / 106.8 at c4/8/16/32 with 0 failures (arm `par8-pair`, 2026-09-01, `llamacpp-par8-pair.json`,
  ledger "FAIRNESS ARM par8-pair").
  **It ships as its own entry rather than as a flag on the seat, and that is the whole decision.** llama.cpp
  divides `-c` among its slots: eight slots at `--ctx-size 131072` serve **16,384 tokens each**. The same
  night's `<node-c>` arm measured the mechanism in-house (`--parallel 8 -c 32768` → "16k/32k prompts exceed the
  4k/slot window"). Both pair tiers declare `qwen3.8-27b` as their fallback agent lane at
  `fallback_agent_ctx_tokens: 131072`, so moving the flag onto that seat would have served an eighth of the
  advertised window — the advertise-more-than-you-serve defect `blackwell-8` shipped once already. Both entries
  are swappable alternatives at `ttl: 300`, so the twin costs nothing until something asks for it.
  Not `--parallel 16` (fails to start on this pair at `-c 131072` and at `-c 65536`; needs `-c 49152` plus
  `--tensor-split 27,23`, for 119 vs 111 tok/s at c16) and not 32 (infeasible: ~350 MB of GDN recurrent state
  PER SLOT → an 11.3 GB rs buffer beside ~9 GB of weights on a 16 GB card). The three-card arm is not what
  ships: `par8-3c` reached 102.9 at c8 against the pair's 110.8 and it spans the display card, so the twin is
  pair-pinned on `blackwell-3x16` like every other default seat there.
  `TestPairSpanningTemplatesShipTheMeasuredFanoutTwin`, `TestDroppingQ38TakesTheFanoutTwinWithIt`,
  `TestSingleCardTemplatesKeepTheirSingleSlot27B`.

- **`-sm tensor` is now an INVARIANT, not one tier's flag (A-25).** The measurement is what makes it free: at the
  SAME `--tensor-split 25,25`, moving Qwen3-VL-32B from `-sm layer` to `-sm tensor` produced BYTE-IDENTICAL output
  5/5 on a fixed image+prompt while generation went 19.6 -> 34.1 tok/s (+74%) and prefill 995 -> 926 (CUDA-X
  2026-09-05). It reached `blackwell-3x16` only. The audit: that is the ONLY tier in the table seating
  Qwen3-VL-32B, so there is nothing left to propagate to -- `blackwell-2x16` is excluded on a recorded reason (the
  seat spans BOTH cards of its pair, and on a 2-card box the second card pays the desktop/DWM tax, so the tier
  would hand ~25 GB of its 32 GB to a swappable vision seat; it needs its own fit measurement first), and every
  other tier is single-card or seats a model that fits one card. So the row ships as the rule instead of the copy:
  a seat spanning more than one device MUST declare a split_mode (llama.cpp's default is the `-sm layer` this
  measurement beat), a single-device seat must declare NEITHER split_mode nor tensor_split (a meaningless flag
  copied between tiers is how the 3-card media block became a byte-for-byte copy of the 2-card one), and
  tensor_split arity must match the pin. `TestEveryMultiCardMediaSeatCarriesTheMeasuredSplitMode`.
- **The RENDERED vision command is gated, not just the JSON field.** `TestTripleBlackwellVisionSeatIsTheMeasuredWinner`
  read `split_mode` out of the tier table; nothing asserted that the llama-swap entry servingtmpl builds by hand
  carries `-sm tensor --tensor-split 25,25` -- and a field nothing renders measured nothing.
  `TestTripleBlackwellRendersTheMeasuredSplitMode` renders the real committed seats through the tier's own
  template and asserts the flag present there and absent from every single-card seat.
- **`mediaseat.Seat.Measured`.** Tiers were already writing `measured` records into media seats (`ampere-16`'s
  vision seat since 0.116.1) and the field did not exist, so every one of them was parsed and dropped. The
  `blackwell-3x16` vision seat now carries its own: the bake-off that chose the model, the CUDA-X numbers behind
  each flag, and the A-25 propagation audit.
- **`blackwell-2x16` seeds its OWN measured utilization, 0.85 (A-22).** The tier carried
  `gpu_memory_utilization: 0.9` copied from `blackwell-3x16`, and a test asserted the two were IDENTICAL -- the
  assertion that kept the copy in place. vLLM has ONE utilization for every rank, so the figure only means what
  the cards it spans are also doing: on the 3-card box the pair is devices 0 and 2 and the DISPLAY card is
  outside the seat, which is what makes 0.90 soak-verified there (10/10 cold loads plus a 20-minute soak; 0.92
  lifted the pool ~10% without staying stable; 0.95 cannot initialise beside the mem0 embedder). On a TWO-card
  box the pair IS every card the machine has, so one of the two also draws the desktop -- and that failure is
  measured: 2026-09-04, a 3-card engine at util 0.87-0.90 left the display card under 1 GB, Windows fell to a
  720p-class mode and the box needed a reboot. The two-card operating point was measured on the pair itself (arm
  P, 2026-09-08): util 0.85 -> 2.32 GiB KV / 141,266 tokens, "Maximum concurrency for 131,072 tokens per request:
  1.08x" -- still one full-window request. (Arm F, devices 1+2 at util 0.50, was VOID at -3.36 GiB.) This is a
  SEED for a fresh 2-card install; the reference box's pair seat runs 0.90 under the H-24 soak and is not
  redeployed by this change. `TestDualBlackwellSeedsThePairSeatWithTheCacheServer` now asserts each tier's own
  figure and says why they differ; `max_model_len` and `ttl` stay pinned identical.
- **The 3-card media roster is audited against its own bake-off and gated (A-15).** The 48 GB bake-off ran the
  whole media lane on the reference box; the tier shipped a media block copied byte-for-byte from
  `blackwell-2x16`, which is why the 3-card box measured no media difference from the 2-card one. PR #264
  corrected the PLACEMENTS and #266 the vision MODEL — nothing ever gated what the bake-off DECIDED about models
  and precision, so the roster could drift back into a copy without failing anything. Slot by slot: vision
  qwen3-vl-32b on the pair (MMMU +11); ocr qwen3-vl-8b on card 2 (the 32B regressed there: DocVQA -3.7 plus dense
  transcription); stt whisper large-v3-TURBO on card 2 (turbo wins or ties every group, es-long WER 0.0652 vs
  0.0838, at ~2x the speed — the proposed `stt_hq` lane FAILED and stays empty); imagegen Krea 2 TURBO bf16 at
  8 steps / cfg 1 on pool cuda:1 + donor cuda:2 (Krea 2 RAW is BROKEN on ComfyUI 0.34.0 under every recipe tested
  — composition correct, buried in terminal noise, same-seed Turbo clean — so RAW is a fine-tuning base, not a
  servable lane); videogen LTX-2.5 int8-convrot with the CONV video VAE (round 2 of the decoder A/B changed only
  the VAE: conv resolved more detail on every one of 5 pairs, +15-25% Laplacian energy and +9-11% high-frequency,
  start-frame fidelity a wash at 0.02-0.26 dB, and conv was 12-15% FASTER — it wins on both measurable axes),
  compute cuda:0 still the documented ComfyUI-MultiGPU #220 exception. **One slot has no winner and was left
  alone: TTS.** Chatterbox v3 and both Spanish packs transcribe back verbatim, but naturalness was explicitly
  left as an operator A/B and adoption needs a contained-venv bump — the seed is unchanged and the gate says
  nothing about it. `TestTripleBlackwellMediaRosterIsTheMeasuredOne`; the ocr and stt seats carry their own
  `measured` records now, as the vision seat does.
### Fixed
- **A multi-device pin rendered as two env entries.** `env: [CUDA_VISIBLE_DEVICES=0,2]` is a YAML flow
  sequence, and YAML reads it as TWO items — `CUDA_VISIBLE_DEVICES=0` and `2` — so every seat that must span
  the 5060 Ti pair was handed ONE card plus a nonsense variable: `qwen3.8-27b` and `qwen3.8-27b-262k` in
  `llama-swap.win-triple-blackwell.yaml`, and, through the media-seat renderer, the `qwen3-vl-32b` vision seat
  whose whole point is that a 32B does not fit one 16 GB card. The three template lines are quoted, and
  `flowItems` now quotes any per-seat env entry containing a flow separator; barewords are kept for everything
  else so the existing rendered text and its assertions are unchanged.
## [0.122.1] - 2026-09-14 - the final budget fits the wall; a cut final on a schema contract is re-issued once with list caps

### Fixed
- **The final answer's completion budget now FITS the wall that is left** (`internal/seatrate`,
  `internal/agent/loop.go`, `internal/pipeline/agenttask.go`; register D-95). The budget was
  `FinalBudgetFor` alone — 4x the step budget, cap 8,192 — whatever the wall could actually decode.
  Measured 2026-09-14 on the `<node-c>` 4B seat (`qwen3.5-4b-vllm`, ~15 tok/s): list-heavy grounded
  extractions with an `output_schema` owed a 8,192-token final PLUS an 8,192-token re-pack, which the
  harness's own `wall_note` priced at 1,166–1,310 s against a 900 s wall. Three of ten contracts died
  there — at the 4,096 budget they were cut (`finish_reason=length` → `output_truncated` → re-pack
  skipped → deferred after 309–450 s), and at 8,192 `METHODOLOGY.md` and `SELF-CONTROL.md` hit the
  900 s wall instead. `seatrate.FitFinalBudget` now computes
  `fit = (remaining_wall - other - safety) x tok_s / turns` (`turns` = 2 when a schema is set, because
  the final answer and its re-pack are both decoded inside this wall; `other` = the estimate's cold
  load + think block + tool steps, 0 once those are spent; `safety` = a tenth of the wall, never less
  than one transcript prefill) and the run uses `min(configured, fit)`, floored at 1,024 and never
  raised above the configured cap. Computed at run start and again at the forced final step, where the
  live clock is the honest input. `Estimate.OtherSec` publishes the non-final terms so the fit
  subtracts exactly what the estimate charged. Results carry `final_budget_fit` and `budget_note`
  ("final 8192 → 3592 to fit 900 s at 15.0 tok/s (split with the output_schema re-pack)") on the node
  and delegator wires; a seat with no measured rate, or a wall with room, publishes neither and runs
  byte-for-byte as before.
- **A cut final on a schema contract is re-issued ONCE with explicit list caps** (`internal/agent`,
  `internal/pipeline`; register D-95). 0.115.23 (D-91) rightly refuses to re-pack a `length`-cut final
  — a partial cannot be re-packed — but abstaining there threw away a run that had read the whole
  document and only over-answered: the seat was never told how long its lists could be. The loop now
  asks once more, thinking off, at the same budget, with the caps spelled out ("cap every list at N
  items … keep every string under 200 characters"), where N is the SMALLEST `maxItems` in the
  contract's schema and never more than 8 — a schema the author shaped is honoured, never broken by
  the retry. Gated on three conditions, so it can never re-create the shape it fixes: an
  `output_schema` is set, the partial is JSON-shaped (a cut narrative earns nothing), and the wall
  still holds one turn at the seat's measured rate (`seatrate.MinTurnFor`, re-pack term included).
  Bounded at one: a seat that cuts the capped answer too abstains exactly as before, with
  `final_reissue=list_cap` and both `finish_reason`s on the record (`calls[]` and `repack_note`), so a
  first cut and a second are never read as the same event.

## [0.122.0] - 2026-09-14 - a write-capable delegation door, default off (D-06)

### Added
- **The delegation lane can hand a seat an IMPLEMENTATION leg, and the seat hands back a DIFF.** A
  contract may now carry `write_root` — a directory, RELATIVE to the run's read root, that the seat may
  create and change files under. The seat works on the node's own throwaway copy of the inline context
  docs; what crosses back is a unified diff (`diff`, `diff_files`) plus a `write_note`, and **the harness
  applies none of it** — reviewing and applying the change stays the caller's job, which is the whole
  safety story of letting a 4B write anything. Relative and not absolute because the contract is
  self-contained: the executing node has never seen the delegator's filesystem, so an absolute
  delegator-box path would name nothing there (`internal/core/agentwire.go`,
  `internal/pipeline/agentwrite.go`, `internal/writedoor`).
- **`agent_allow_write`, default FALSE.** A node that has not opted in refuses a write contract at ACK
  (a 400, so the delegator RE-PLACES it on a node that has) and, on the in-process local path that has no
  ack hop, defers with the new `defer_class: "write"` — neither a broken stack nor an unplaceable
  contract, so it is its own class (`internal/config/config.go`, `internal/fleetnode/tasks.go`).
- **Two acceptance verbs that read the write set rather than the prose**: `diff_touches:<path-prefix>`
  and `diff_max_files:<n>`. Both fail CLOSED on an empty write set, `diff_max_files` included — a cap
  assertion that passed because nothing was written would make a contract that verified nothing read as
  verified. `AcceptanceCheck.Eval` now takes the whole `AgentWireResult`.
- **ADR 0044** — the write-capable delegation door.

### Security
- The door grants create+overwrite inside ONE directory and nothing else: no delete (the `edit` profile it
  defaults to does not advertise `delete_file`), no shell, no `run`, no fetch, no github. Confinement is
  `os.Root` on both platforms, verified at the write root before the absolute path reaches the write
  tools — proven against a real Windows junction (a real `mklink /J` spawn) and a real Linux symlink.
  Caps are enforced twice: `agent.WriteLimit` refuses the offending call at the tool, and the finished
  write set is re-counted before it crosses the wire. A cap breach publishes NO diff — a truncated patch
  applies as silent damage.

## [0.121.1] - 2026-09-14 - the result cache opens lazily, and reads without writing

Four ⚑WINNER rows of the harness master plan v2 (A-70, A-25, A-22, A-15) were measured on the reference box
and never reached the tier seeds, so a fresh render shipped the loser of each measurement. This release wires
them, each with the gate that keeps it wired and an assertion that it does NOT appear where it was not measured.

### Added
- **`qwen3.8-27b-par8`, the 27B's FAN-OUT twin, on both pair-spanning Blackwell templates (A-70).** The 27B
  llama.cpp seat ships `--parallel 1` and is flat under fan-out — 34.6-37.4 tok/s aggregate at c1..c32, and it
  answers 429 from c16 (6 of 16 and 22 of 32 requests failed in the baseline arm). On the SAME 5060 Ti pair at
  the SAME `-c`, `--parallel 8` holds single-stream (W1 43.4 vs 43.6) and roughly TRIPLES aggregate: 78.7 /
  110.8 / 100.7 / 106.8 at c4/8/16/32 with 0 failures (arm `par8-pair`, 2026-09-01, `llamacpp-par8-pair.json`,
  ledger "FAIRNESS ARM par8-pair").
  **It ships as its own entry rather than as a flag on the seat, and that is the whole decision.** llama.cpp
  divides `-c` among its slots: eight slots at `--ctx-size 131072` serve **16,384 tokens each**. The same
  night's `<node-c>` arm measured the mechanism in-house (`--parallel 8 -c 32768` → "16k/32k prompts exceed the
  4k/slot window"). Both pair tiers declare `qwen3.8-27b` as their fallback agent lane at
  `fallback_agent_ctx_tokens: 131072`, so moving the flag onto that seat would have served an eighth of the
  advertised window — the advertise-more-than-you-serve defect `blackwell-8` shipped once already. Both entries
  are swappable alternatives at `ttl: 300`, so the twin costs nothing until something asks for it.
  Not `--parallel 16` (fails to start on this pair at `-c 131072` and at `-c 65536`; needs `-c 49152` plus
  `--tensor-split 27,23`, for 119 vs 111 tok/s at c16) and not 32 (infeasible: ~350 MB of GDN recurrent state
  PER SLOT → an 11.3 GB rs buffer beside ~9 GB of weights on a 16 GB card). The three-card arm is not what
  ships: `par8-3c` reached 102.9 at c8 against the pair's 110.8 and it spans the display card, so the twin is
  pair-pinned on `blackwell-3x16` like every other default seat there.
  `TestPairSpanningTemplatesShipTheMeasuredFanoutTwin`, `TestDroppingQ38TakesTheFanoutTwinWithIt`,
  `TestSingleCardTemplatesKeepTheirSingleSlot27B`.

- **`-sm tensor` is now an INVARIANT, not one tier's flag (A-25).** The measurement is what makes it free: at the
  SAME `--tensor-split 25,25`, moving Qwen3-VL-32B from `-sm layer` to `-sm tensor` produced BYTE-IDENTICAL output
  5/5 on a fixed image+prompt while generation went 19.6 -> 34.1 tok/s (+74%) and prefill 995 -> 926 (CUDA-X
  2026-09-05). It reached `blackwell-3x16` only. The audit: that is the ONLY tier in the table seating
  Qwen3-VL-32B, so there is nothing left to propagate to -- `blackwell-2x16` is excluded on a recorded reason (the
  seat spans BOTH cards of its pair, and on a 2-card box the second card pays the desktop/DWM tax, so the tier
  would hand ~25 GB of its 32 GB to a swappable vision seat; it needs its own fit measurement first), and every
  other tier is single-card or seats a model that fits one card. So the row ships as the rule instead of the copy:
  a seat spanning more than one device MUST declare a split_mode (llama.cpp's default is the `-sm layer` this
  measurement beat), a single-device seat must declare NEITHER split_mode nor tensor_split (a meaningless flag
  copied between tiers is how the 3-card media block became a byte-for-byte copy of the 2-card one), and
  tensor_split arity must match the pin. `TestEveryMultiCardMediaSeatCarriesTheMeasuredSplitMode`.
- **The RENDERED vision command is gated, not just the JSON field.** `TestTripleBlackwellVisionSeatIsTheMeasuredWinner`
  read `split_mode` out of the tier table; nothing asserted that the llama-swap entry servingtmpl builds by hand
  carries `-sm tensor --tensor-split 25,25` -- and a field nothing renders measured nothing.
  `TestTripleBlackwellRendersTheMeasuredSplitMode` renders the real committed seats through the tier's own
  template and asserts the flag present there and absent from every single-card seat.
- **`mediaseat.Seat.Measured`.** Tiers were already writing `measured` records into media seats (`ampere-16`'s
  vision seat since 0.116.1) and the field did not exist, so every one of them was parsed and dropped. The
  `blackwell-3x16` vision seat now carries its own: the bake-off that chose the model, the CUDA-X numbers behind
  each flag, and the A-25 propagation audit.
- **`blackwell-2x16` seeds its OWN measured utilization, 0.85 (A-22).** The tier carried
  `gpu_memory_utilization: 0.9` copied from `blackwell-3x16`, and a test asserted the two were IDENTICAL -- the
  assertion that kept the copy in place. vLLM has ONE utilization for every rank, so the figure only means what
  the cards it spans are also doing: on the 3-card box the pair is devices 0 and 2 and the DISPLAY card is
  outside the seat, which is what makes 0.90 soak-verified there (10/10 cold loads plus a 20-minute soak; 0.92
  lifted the pool ~10% without staying stable; 0.95 cannot initialise beside the mem0 embedder). On a TWO-card
  box the pair IS every card the machine has, so one of the two also draws the desktop -- and that failure is
  measured: 2026-09-04, a 3-card engine at util 0.87-0.90 left the display card under 1 GB, Windows fell to a
  720p-class mode and the box needed a reboot. The two-card operating point was measured on the pair itself (arm
  P, 2026-09-08): util 0.85 -> 2.32 GiB KV / 141,266 tokens, "Maximum concurrency for 131,072 tokens per request:
  1.08x" -- still one full-window request. (Arm F, devices 1+2 at util 0.50, was VOID at -3.36 GiB.) This is a
  SEED for a fresh 2-card install; the reference box's pair seat runs 0.90 under the H-24 soak and is not
  redeployed by this change. `TestDualBlackwellSeedsThePairSeatWithTheCacheServer` now asserts each tier's own
  figure and says why they differ; `max_model_len` and `ttl` stay pinned identical.
- **The 3-card media roster is audited against its own bake-off and gated (A-15).** The 48 GB bake-off ran the
  whole media lane on the reference box; the tier shipped a media block copied byte-for-byte from
  `blackwell-2x16`, which is why the 3-card box measured no media difference from the 2-card one. PR #264
  corrected the PLACEMENTS and #266 the vision MODEL — nothing ever gated what the bake-off DECIDED about models
  and precision, so the roster could drift back into a copy without failing anything. Slot by slot: vision
  qwen3-vl-32b on the pair (MMMU +11); ocr qwen3-vl-8b on card 2 (the 32B regressed there: DocVQA -3.7 plus dense
  transcription); stt whisper large-v3-TURBO on card 2 (turbo wins or ties every group, es-long WER 0.0652 vs
  0.0838, at ~2x the speed — the proposed `stt_hq` lane FAILED and stays empty); imagegen Krea 2 TURBO bf16 at
  8 steps / cfg 1 on pool cuda:1 + donor cuda:2 (Krea 2 RAW is BROKEN on ComfyUI 0.34.0 under every recipe tested
  — composition correct, buried in terminal noise, same-seed Turbo clean — so RAW is a fine-tuning base, not a
  servable lane); videogen LTX-2.5 int8-convrot with the CONV video VAE (round 2 of the decoder A/B changed only
  the VAE: conv resolved more detail on every one of 5 pairs, +15-25% Laplacian energy and +9-11% high-frequency,
  start-frame fidelity a wash at 0.02-0.26 dB, and conv was 12-15% FASTER — it wins on both measurable axes),
  compute cuda:0 still the documented ComfyUI-MultiGPU #220 exception. **One slot has no winner and was left
  alone: TTS.** Chatterbox v3 and both Spanish packs transcribe back verbatim, but naturalness was explicitly
  left as an operator A/B and adoption needs a contained-venv bump — the seed is unchanged and the gate says
  nothing about it. `TestTripleBlackwellMediaRosterIsTheMeasuredOne`; the ocr and stt seats carry their own
  `measured` records now, as the vision seat does.
### Fixed
- **A multi-device pin rendered as two env entries.** `env: [CUDA_VISIBLE_DEVICES=0,2]` is a YAML flow
  sequence, and YAML reads it as TWO items — `CUDA_VISIBLE_DEVICES=0` and `2` — so every seat that must span
  the 5060 Ti pair was handed ONE card plus a nonsense variable: `qwen3.8-27b` and `qwen3.8-27b-262k` in
  `llama-swap.win-triple-blackwell.yaml`, and, through the media-seat renderer, the `qwen3-vl-32b` vision seat
  whose whole point is that a 32B does not fit one 16 GB card. The three template lines are quoted, and
  `flowItems` now quotes any per-seat env entry containing a flow separator; barewords are kept for everything
  else so the existing rendered text and its assertions are unchanged.

Register D-05 (P1). `offload_status.reuse.result_cache` on this workstation reported `fallback: true ...
cache.p47084.db`, and the cache directory held **49** per-process sibling files of 32 KB each, accruing at
roughly 12/h and swept only after 12 h. The cause was placement, not contention: `openPipeline` runs for every
CLI command and every MCP server start, and it opened the bbolt cache EAGERLY - taking, or losing, an exclusive
file lock before anything knew whether a cacheable task would ever run. With 21 MCP servers live, one won the
primary and twenty each created a sibling they then never wrote a single entry into.

D-72's "one cache service" would have solved it with a daemon, which house rules forbid. This is the in-process
fix instead.

### Changed
- **The cache handle is LAZY.** `cache.New(path)` opens nothing; the first `Get`/`Put`/`Count` resolves it once.
  A process that never runs a cacheable task now never touches the cache directory at all - which is the
  common case for an MCP server that has served only status and fleet calls. The operator note about a lost
  shared cache moved to a `WithNotify` callback that fires at resolve, so it prints when the fallback is true
  rather than at startup when it was merely predicted.
- **The bbolt lock `Timeout` is 250 ms** (was 1 s). With the eager open gone, the opens that remain are ones
  that will really use the cache, and a loser should reach its sibling in milliseconds.
- **`SiblingMaxAge` is 1 h** (was 12 h), and the sweep runs at handle CONSTRUCTION rather than at open - a
  sweep that only ran on a real open would, after this change, essentially never run.

### Added
- **A read-only, read-through handle** (`cache.NewReader`). Verified against the vendored bbolt v1.5.0 rather
  than assumed: `Options.ReadOnly` takes a genuine SHARED lock on every platform (on Windows,
  `bolt_windows.go` calls `LockFileEx` and adds `LOCKFILE_EXCLUSIVE_LOCK` only for the exclusive case), so many
  readers coexist with each other - but a shared request still conflicts with a writer's exclusive hold. Since
  exactly one harness process holds the primary read-write, a reader cannot assume it is reachable, so it reads
  THROUGH: the primary if free, else this process's own sibling, else nothing. It creates no file on any path
  (`ReadOnly` opens with `O_RDONLY` and no `O_CREATE`), a miss is answered as a miss, and `Put` returns
  `ErrReadOnly` rather than quietly opening a sibling. `TestReadOnlyOpensShareWithEachOtherButNotWithAWriter`
  pins those semantics so a future bbolt bump that changes them fails loudly.
- **Sibling cleanup on `Close`.** An EMPTY sibling - the 32 KB file every losing process used to strand - is
  deleted. A non-empty one makes exactly ONE bounded attempt to promote its entries into the primary if that
  lock happens to be free (<= 1,000 keys, <= 5 s) and is removed only when every entry crossed; an
  over-budget or un-promotable sibling is kept, because it still holds cached work, and the age sweep takes it.
- **`offload_status.reuse.result_cache` now reports `mode`** - `primary`, `sibling`, `readonly`, `unavailable`,
  or `unopened` (the new steady state: lazy, no lock held, not a failure) - plus `siblings` and `sibling_bytes`,
  the census the D-05 regression is measured in and which nothing reported while 49 files accumulated. It also
  reports `entries` / `entries_from`, counted through the read-only handle. Asking never resolves the server's
  own handle and never creates a file.

## [0.121.0] - 2026-09-14 - a cache-server binding per vLLM seat

### Changed
- **Register B-01 - `kv_cache_server` is a LIST of per-seat bindings, and the box declares its vLLM
  roster.** The block bound one store to one `seat` name (the deployed `<node-b>` config: `seat:
  "qwen3.8-27b-vllm"`), which was right while a box ran one vLLM seat and wrong the moment one did
  not: the reference workstation runs the tensor-parallel pair AND an opt-in 3-card layout, the
  second box runs its own small seat, and the shape gave the others nothing to be — a seat with no
  tier and a seat nobody thought about produced the same config, the same `offload_status` and the
  same silence. The operator directive of 2026-09-10 is that the second device's store backs EVERY
  vLLM seat while that device is online, and a config that binds `kv_cache_server` to a single seat
  name is a DEFECT. Now:
  - `kv_cache_server` takes a list of bindings, each naming its `seat`; a binding with no `seat` is
    the BOX DEFAULT and backs every vLLM seat that has no binding of its own, with an exact match
    always winning. **The pre-0.121 single object still loads**, as a one-element list bound to its
    own seat — nothing already deployed has to change.
  - `vllm_seats` names the seats this box serves with a vLLM engine. Declared rather than sniffed:
    `/v1/models` reports model ids and not engines, and the cascade deliberately stays on llama.cpp
    (Gemma-4 hybrids crash LMCache's V2 path, upstream #4263), so a sniffed roster would fail every
    seat that must never have a store. Empty (the default, and every llama.cpp box) = inert.
  - Validation refuses two bindings for one seat (including two box defaults — that is two stores
    whose order in the file picks the winner, not a merge) and a `key_prefix` shared by seats of
    different stack generations. The new `kv_dtype` / `tensor_parallel` fields make B-45's "one
    key_prefix per stack generation" checkable: a shared prefix where any binding leaves the
    generation undeclared is refused too, because it cannot be shown to be safe and the failure it
    guards against (`value size exceeds buffer capacity`) reports success while serving nothing.
  - `offload_status.kv_cache_server` lists every binding (`bindings[]` with seat, store, address,
    key_prefix, l1_staging_gb, declared/enabled, or `storeless` with its reason) plus
    `unbound_seats` — the same list `doctor` fails on, from the same code, so the report and the
    gate cannot disagree.

### Added
- **`doctor` FAILS a vLLM seat with no cache-server binding** — one line per seat, non-zero exit,
  printed above the health probe because the verdicts are pure config and a dead serving layer must
  not hide them. The escape hatch is a DECLARATION and never silence: `{"seat": "…", "storeless":
  true, "reason": "…"}` passes, an absent binding fails, and a binding merely switched off with
  nothing saying why fails too — "the tier is off here" and "nobody considered this seat" must not
  look alike. A binding naming a seat `vllm_seats` does not list is surfaced but never fatal.
- **`local-offload install vllm-seat --config <config.json>`** renders a seat from THE BINDING THAT
  NAMES IT, so two vLLM seats on one box each carry their own store directory, namespace, L1 size
  and chunk without either being edited into a hardware tier. The tier keeps what only the tier
  knows — the mount point, the write floor, the prune target, the writer count, the cap. A storeless
  binding renders a seat with no L2 at all.
- A tier that renders a vLLM seat now seeds `vllm_seats` and a binding for it — the tier's store when
  it declares one, an explicit storeless opt-out when it does not — because a fresh install must not
  ship a config its own `doctor` rejects.
- [ADR 0045](docs/architecture/decisions/0045-a-cache-server-binding-per-vllm-seat.md); `docs/systems/cache-server.md`
  and the OPERATOR-GUIDE config reference rewritten around the list shape and the gate.

## [0.117.7] - 2026-09-14 - output tokens are priced; the pager gate can fire; the Windows cage builds its child's environment; retries skip a fenced seat

### Security
- **The Windows OS-cage builds its child's environment instead of inheriting it**
  (`internal/sandbox`). `CreateProcessAsUser` was called with a nil `lpEnvironment`, and nil does
  not mean "empty" — it means "give the child the caller's block". The caller is the harness, so
  every `run` child on Windows received the delegator's entire environment: `GITHUB_TOKEN`,
  `MEM0_API_KEY`, the fleet auth token, the session's own messaging token, `AWS_*`, anything a
  shell had exported. The line's comment justified it with "reads are not contained on Windows
  anyway", which conflates two things: a cage that does not stop a child GOING AND LOOKING for a
  secret is no reason to HAND it one, and the Linux cage has exec'd with a hand-built env since it
  landed.
  - One contract for both platforms (`sandbox.cageEnv`, `sandbox.EnvAllowlist`,
    `sandbox.inheritedEnv`), four explicit lists per platform — fixed / copied-by-name / home /
    temp — so the two files cannot drift apart again.
  - Linux keeps EXACTLY the env it had: a fixed `PATH`, `HOME` and `TMPDIR` at the scratch dir,
    nothing copied from the parent at all.
  - Windows copies by name only what a process needs to START and cannot synthesise: `SystemRoot`,
    `SystemDrive`, `windir`, `PATH`, `PATHEXT`, `COMSPEC`, `NUMBER_OF_PROCESSORS`,
    `PROCESSOR_ARCHITECTURE`, `OS`. `USERPROFILE`/`HOME` and `TEMP`/`TMP`/`TMPDIR` are DERIVED from
    `Spec.Scratch`, the same redirection Linux does, so a child writes inside the cage rather than
    into the operator's real profile. `APPDATA`/`LOCALAPPDATA`/`USERNAME`/`USERDOMAIN` are
    deliberately absent: they name or point at that profile. The block is UTF-16, sorted, and
    passed with `CREATE_UNICODE_ENVIRONMENT` — without that flag the same bytes are read as ANSI
    and the child gets one truncated entry.
  - The proof is adversarial: `cmd /c set` inside the cage with three canaries exported in the
    parent, asserted absent by name AND by value, the allowlist asserted present, and the block
    asserted CLOSED. On non-Windows the two test names remain as explicit `t.Skip`s rather than
    vanishing behind a build tag — a guarantee whose test is merely absent reads as passing on
    every Linux CI run, which is the same silence that let the nil ship.

### Fixed
- **The savings ledger prices output tokens instead of multiplying them by zero**
  (`internal/ledger`). `est_value_kept_local` was `tokens_saved x opus_input_price_per_mtok` and
  nothing else, so every token a local seat GENERATED was worth $0.00 — and output is the expensive
  half on Opus ($75/MTok against $15/MTok input). 5M locally generated tokens reported nothing
  instead of $375. The rows were always complete (`tokens_out` from the first version,
  `seat_tokens_in` since 0.115.5), so this is arithmetic over the existing schema and no ledger
  field changed.
  - `ledger.Prices{InputPerMTok, OutputPerMTok}` replaces the single price float; `PricesFrom`
    resolves each config key against the shipped defaults INDEPENDENTLY, so every config written
    before `opus_output_price_per_mtok` existed keeps a real output price instead of re-creating
    the zero on upgrade.
  - New config key `opus_output_price_per_mtok` (default 75.0), in `config.example.json`, the
    install template and the README. `local-offload ledger` now prints both token counts, since
    the dollar figure is no longer a function of the input count alone.
  - Deferred rows stay at zero on both halves — pinned by its own case, so the fix cannot
    degenerate into "price every `tokens_out` in the file".
- **The context-pager gate (R2-13) can now fire in either direction** (`internal/agent`).
  `PagerStats` shipped complete — eviction hashes, re-fetch counting, a pointer rate, its own gate
  verdict — and with NO production caller: `NoteEvicted`/`NoteFetched` were reachable only from the
  instrument's own unit test, so every real run reported `insufficient_data` and the stated gate
  ("build nothing further until the re-fetch rate clears 10 %") could neither close the pager family
  nor open it. The instrument was not obsolete, it was unarmed.
  - `PagerStats.NoteCompaction(before, after)` runs at every compaction pass in the loop (proactive,
    the harder retry after a context-overflow rejection, and the emergency shrink) and records every
    tool body that was in the transcript and is not in it byte-identical afterwards — dropped,
    collapsed to a dedupe marker, or pruned to a skeleton all leave the agent without the content. A
    no-op pass records nothing, so the happy path stays invisible.
  - `NoteFetched` runs at the ONE boundary every tool result crosses, AFTER the result cap and the
    env-rule rewrites, so the bytes hashed are the bytes the transcript carries.
  - `Result.Pager` carries the report out beside `Result.Prefill`, and `local-agent --queue` prints
    it beside the prefill line — basis WITH rate, so a run that evicted nothing can never be read as
    a measured 0 %.
- **A retry never lands on a fenced local seat** (`internal/delegate`, register D-94). A contract
  deferred on the 4B seat and its cross-seat retry was placed on the local seat whose GPU lease was
  EXCLUSIVE, waited the whole `gpu-lease timeout after 5m0s (bound 5m0s)` at the model-affinity
  cordon and deferred as capacity — while another node was eligible and idle for those five minutes.
  `alternativeNode` returned the local seat unconditionally whenever the first attempt had run
  remote, with no lease read at all, and then learned the refusal by dialling.
  - `delegate.Fenced(info)` reports whether a lease REFUSES A NEW RUN and names it. It CALLS
    `modelaffinity.BlocksNewRun` rather than restating it, so placement and admission cannot drift
    into a placement that dials a seat its own gate will refuse. Exclusive text, draining text and
    media fence; a plain text reservation does not (it already removes the seat from FIRST
    placement, and the affinity gate admits the load).
  - Fenced with something else eligible: the retry goes there, through `gate.Place` with local out
    of contention, so an idle node beats one that would queue. Fenced with nothing else: the
    subtask defers AT ONCE, `retry_note` naming the fence and the holder. The lease is read fresh
    and read as a VERDICT — never discovered by dialling.
  - FIRST placement was audited and needs no change: `route=auto` defers to the capacity wait on
    `Reserved(LocalLease(...))` and `route=spread` drops the local seat out of the deal, both
    already covered by tests. A media lease is deliberately not a first-placement fence (ADR 0026)
    — which is why it fences a RETRY: a retry runs on the leftovers of `timeout_sec` and cannot
    spend them queueing behind a render.

### Docs
- `docs/systems/coding-agent.md`: the caged child's environment contract on both platforms.
- `docs/systems/fleet-node.md`: the retry's fence rule, and what first placement already does.

## [0.117.6] - 2026-09-14 - duplicate review findings no longer crowd a unique one out of the cap

### Added
- **Register D-90 — `offload_review_diff` dedupes findings before the cap.** A seat restating the
  same defect more than once (once per hunk it touches, or once plainly and once with the file:line
  it already reported repeated inside the claim text) used to count as that many findings toward
  `max_findings`, so duplicate copies of ONE defect could crowd a genuinely different one out of the
  published list — the same class of bug `TruncatedByCap`'s own doc already named for an uncounted
  drop, just reached from the other side. `reviewlane.Dedupe` now runs between `Ground` and the cap:
  it keys on (file, normalised claim) — claim lowercased, whitespace collapsed, punctuation stripped,
  and a leading file:line prefix the seat folded into the claim text itself removed — and merges lines
  within a CHAINED ±2 tolerance of each other rather than requiring an exact line match, since a seat
  citing the same defect one line off is not a second defect. Within a duplicate cluster the most
  severe report wins; a severity tie keeps whichever occurrence came first in the seat's own answer.
  The drop count publishes as `dropped_duplicate` on the wire beside `dropped_ungrounded` and
  `dropped_echo`, and in the tool description's field list.
  `TestDedupeCollapsesExactAndNearDuplicates`, `TestDedupeChainsLineTolerance`,
  `TestDedupeKeepsTheMostSevereOfADuplicateCluster`, `TestDedupeSeverityTieBreakKeepsFirstOccurrence`,
  `TestDedupeStripsARepeatedFileLinePrefixFromTheClaim`,
  `TestDedupeNeverMergesAcrossFilesOrDistinctClaims`, `TestReportDedupesBeforeApplyingTheCap`
  (mutation-verified: reordering `Report` to dedupe AFTER the cap makes it fail),
  `TestReviewDiffPublishesDroppedDuplicateAndDedupesBeforeTheCap`.

## [0.117.3] - 2026-09-14 - the research lane dials the address it validated

### Security
- **Register K-01 - the research lane's DNS-rebinding window is closed.** `internal/research.ValidateURL`
  resolved the caller's host, approved every answer, and then THREW IT AWAY: the fetch reconnected by
  NAME through a bare `&http.Client{}` (default transport, no dial-time hook), so the OS resolved again
  outside the guard. An attacker serving their own name with a one-second TTL answered a public address
  for the lookup the harness validated and `127.0.0.1` for the lookup the transport made. Redirect hops
  were re-validated the same way - by name - so hop two had the identical window through any open
  redirect. The new `internal/research/fetch_rebind_test.go` proved it on HEAD: the lane read
  `fleet admin secret` off a loopback `httptest` listener, directly and again through a redirect.
  The lane's client now rides `netguard.PublicTransport`, which resolves at DIAL time and connects to
  the vetted IP LITERAL, on the first hop and on every redirect hop. The test asserts ZERO accepts on
  the loopback listener and zero dial attempts, across 13 rebind targets.
- **One public-IP predicate in the tree.** The research lane carried a second, weaker copy that missed
  `0.0.0.0/8`, `240.0.0.0/4`, the TEST-NETs, `192.0.0.0/24`, `192.88.99.0/24`, NAT64 (`64:ff9b::/96`)
  and 6to4 (`2002::/16`) - so it graded `64:ff9b::7f00:1` as public even at validate time. Deleted.
  The agent lane's `blockedNets`/`isDisallowedIP`/`safeControl` moved into the shared
  `internal/netguard/publicnet.go` (plus `192.88.99.0/24`, `64:ff9b:1::/48`, Teredo, ORCHIDv2, AS112
  and the RFC 9637/9602 documentation ranges), and `internal/netguard/publicnet_test.go` table-tests
  every reserved range and nine public addresses as the predicate's spec.

### Added
- `internal/netguard/publicnet.go`: `LookupIP` (the tree's single DNS seam - the tailnet guard's
  `lookupNetIP` folded into it), `CheckPublicIP`/`PublicIP`, `PublicDialControl` (the agent lane's
  `safeControl`, moved), `PublicDialContext` (resolve-and-pin), and `PublicTransport` (clones the
  caller's transport, wraps `DialContext` and any `DialTLSContext`, strips `Proxy`).
- ADR [0042 - No bare HTTP client on a caller-named host](docs/architecture/decisions/0042-no-bare-http-client-on-a-caller-named-host.md):
  the house rule, its two-half guard shape, and why a mixed DNS answer is refused whole on the public
  lane but skipped on the tailnet lane.

### Changed
- `research.Options.Client`'s contract: the transport is no longer used as-is, it is wrapped by
  `netguard.PublicTransport`. Injecting a `DialContext` is the supported test seam; a stub
  `RoundTripper` now gets `http.DefaultTransport`'s settings with the gate, because the dial gate
  cannot be optional.
- `docs/systems/mcp-server.md` describes the research lane's guard as the two halves it now is.

## [0.117.2] - 2026-09-14 - the retry floor is the retry seat's; the deal reads a starting seat as busy

The D-46 sizing half of the stranded `fix/drain-starting-seat` branch, rebased onto the 0.117.0 drain. Its D-92
half (the `starting` reading, the drain's `starting` case, both drain tests, the docs) shipped inside 0.117.0 and
is dropped here rather than re-applied; what D-92 still lacked was the delegator's side of the same reading.

### Added
- **Register D-46 follow-up 1 - the retry floor is the RETRY seat's.** The 2026-09-10 retry cleared a 201 s floor
  sized from the 4B's numbers and landed on the 27B, whose own floor was around 484 s. A fleet node now publishes
  `seat_rate` on `/fleet/health` (the seat's remembered `tok_s`, `cold_load_sec`, `samples` from its seat-rates
  store, and `min_turn_sec` at its own final budget; cached 30 s; absent until the seat has a sample) and
  `seat_budget` (`step_tokens`, `final_tokens`, `thinking`). `runner.retryFloorOn` sizes a retry by the seat it
  lands on - a remote node's published numbers, or this box's seat-rates store for the local seat - with the
  re-pack term for a schema contract, and falls back to the first attempt's `min_turn_sec` (named as the
  fallback in the retry note) when the retry seat published none. `TestRunRetryFloorComesFromTheRetrySeat`,
  `TestFetchNodeViewMapsSeatRateAndBudget`, `TestHealthPublishesSeatBudgetAndRate`.
- **Register H-04 (why `seat_budget` exists).** The standard quality instrument's first live arm ran on `<node-c>`
  at the NODE's `agent_max_tokens` (1,024, so a 4,096-token final) while the delegator declared 4,096: a
  delegator's config does not travel with the contract. Matched budgets across seats are read from `seat_budget`,
  never assumed from the caller's config.

### Changed
- **Register D-46 follow-up 2 - the estimate charges the re-pack.** A contract with an `output_schema` may pay one
  more completion for the structured re-pack when the final answer is prose (a 285 s re-pack on the 27B sat
  outside every floor and estimate). `wall_estimate_sec` and `min_turn_sec` now include one final-budget
  completion for such contracts as an upper bound, and `wall_note` says `+ re-pack <= N tok (... s; output_schema
  set - skipped when the seat answers in the object shape)`. `seatrate.FinalBudgetFor` is now the ONE
  final-budget rule (4x the step, capped at 8,192); `agent.finalMaxTokens` and the exported
  `agent.FinalBudgetFor` delegate to it, so the agent loop, the node's health and the delegator's floor can
  never run three different budgets. `TestComputeAddsTheRepackTermForASchemaContract`,
  `TestWallEstimateChargesTheRepackForASchemaContract`.
- **Register D-92, the delegator's half.** `runner.probeLocalBusy` reads a `starting` local seat as BUSY with the
  count unknown rather than `0 in flight`, and `retrySeatBusy` carries that note through instead of printing
  `0 in flight on the local seat`. The seatload reading and the drain's own `starting` case landed in 0.117.0;
  this is the spread deal reading the same state the drain already does.

## [0.117.1] - 2026-09-14 - live work outranks a stale lease record

Found by the 0.117.0 live proof on `<node-c>`: its lease root held a record from a dead bake-off holder, and
while the seat was visibly loading for a delegated run `gpu status` said `stale-holder — nothing is running
under it`. The stale verdict sat above the working ones.

### Fixed
- `gpuactivity.Assess`: a stale lease record is the verdict only when nothing else is running; beside a
  loading seat, an in-flight request or a registered run it is a tail on the `working` (or `loaded-idle` /
  `busy-outside`) verdict, still naming the dead holder and that the next `gpu reserve` reclaims it
  (`TestAssessVocabulary`: two new cases).
- The drain's turn hint is plain ASCII (`~174 s`): the `≈` glyph came through an SSH console as `�`.

## [0.117.0] - 2026-09-14 - the drain waits for runs, inside the queue budget; "busy" says what the cards are doing

`gpu reserve --wait 8h --drain --unload-seat` failed twice on 2026-09-14 with `drain of agent-pool did
not finish within 2m0s (last: 1 in flight)` after printing "1 in flight" sixty-one times. The lease was
free both times; the "1 in flight" was one legitimate 27B step (3m27s at the seat's measured 23.6 tok/s).
Three defects, each sufficient (ADR 0041, register D-93): the drain's deadline was a fixed two minutes
outside the `--wait` budget; `--unload-seat` stamped the lease exclusive at acquire, so the admission
gate blocked the very run the drain was waiting for; and the drain read the engine's gauge, which reads
zero between a run's steps — the third attempt unloaded the seat in that gap and the run's next step
died behind the fence (ledger 10:15:00, `wall timeout after 600s`). Behind all three: no surface said
what the cards were DOING, only that something held them.

### Added
- `internal/gpuactivity`: the run registry and the activity reading. Every agent loop launcher
  (`agent_run`, the contract runner behind local delegation legs and fleet jobs) registers its run under
  `<state root>/gpu/activity/` before admission — seat, kind, origin, goal excerpt, phase, step, tokens —
  updates it per step (`agent.RunObserver`, `Loop.WithObserver`) and every 15 s, removes it at the end;
  stale records (dead/recycled pid, heartbeat > 120 s) are swept by readers. `Snapshot` composes the
  lease, the seat (`seatload`), the runs and an nvidia-smi sample (utilization, memory, processes) into
  one `verdict`: `working` | `held-working` | `held-idle` | `loaded-idle` | `busy-outside` |
  `stale-holder` | `free`, with a note that names the evidence.
- `gpu status [--json]` prints the verdict, the seat's load state and in-flight count, the registered
  runs, the cards, and the holder's command; `offload_status.gpu_lease` carries `verdict`, `activity`,
  `draining`, `command`. The tool description names the vocabulary.
- Lease record: `draining` and `command` (`gpulease.Meta`/`Info`/`Options`); `Lease.Restamp` /
  `Manager.Restamp` rewrite the holder's own flags in place under the epoch lock without moving the
  epoch; `InspectDirDetail` tells a stale record from a free card; `Info.HeartbeatAt`.
- `modelaffinity.BlocksNewRun` / `AwaitRunSlot`: a DRAINING text hold refuses a NEW run (the launcher
  holds at the cordon for its admission budget, then defers `capacity` with the holder named) while
  `blocksLoad` — every request — is unchanged, so runs already in flight finish their steps.
  `LeaseError.Draining` and a message that says running work is not interrupted.
- `gpu hold --draining`; `pipeline.AdmissionBudget` (exported for the MCP door).
- Tests: `TestDrainWaitsForARegisteredRunAcrossTheStepGap`, `TestDrainPrintsOnChangeNotOnEveryTick`,
  `TestDrainIgnoresRunsOnOtherSeats`, `TestDrainDeadlineFollowsTheQueueBudget`,
  `TestReserveDrainsUnderADrainingStampAndTurnsExclusiveAfter`, `TestReserveStampsTheWrappedCommand`,
  `TestRestampTurnsADrainingLeaseExclusiveWithoutMovingTheEpoch`, `TestRestampRefusesAFencedOutEpoch`,
  `TestInspectDirDetailReportsAStaleRecord`, `TestADrainingTextHoldRefusesNewRunsButAdmitsRunningWork`,
  `TestAnExclusiveHoldRefusesBothNewRunsAndRequests`, `TestLoopReportsStepsAndTheFinalPhaseToItsObserver`,
  the `gpuactivity` suite (`TestAssessVocabulary`, staleness by a real dead pid, parsers), and the
  seat-starting drain tests carried from the unmerged 0.115.24 branch.

### Changed
- `gpu reserve --drain`: `--drain-timeout` defaults to **the rest of `--wait`** (from when the
  reservation began queueing), floor 2 min; an explicit value still wins. The lease is stamped
  `draining` at acquire and `exclusive` only once the seat is idle (`--unload-seat` / `--exclusive`
  with `--drain`); `--exclusive` without `--drain` stamps at acquire as before. The wrapper form stamps
  its argv as the record's `command`. The drain waits until the engine's gauge AND the registry are
  empty on two consecutive reads; a seat listed `starting`/`stopping` counts as busy and its upstream
  is never probed (register D-92, `seatload.Reading.Starting`). Progress prints on CHANGE (count, load
  state, a run's step) with a reminder every 5 min; every line and the deadline error carry the seat's
  own turn arithmetic and how to wait longer.
- The contract runner registers and holds at the cordon BEFORE its admission pre-flight and warm-up,
  so the warm-up can no longer load the seat past an exclusive or draining hold (it did at 10:20:03).

### Fixed
- The drain no longer deadlocks against the run it waits for, no longer fails under one legitimate
  seat turn, and no longer unloads a seat between a run's steps.
- Reviewer round (sonnet, clean context): the `agent_run` door awaited the cordon on the WALL's context,
  charging the wait to the run — now on the caller's ctx with the admission deadline, the wall starts
  after; the wrapper form never heartbeat its lease during the drain, so a drain longer than `--for`
  (now possible: the drain runs for the queue budget) became reclaimable mid-wait — `renewWhile` beats
  every 15 s for the drain's length (`TestReserveRenewsTheLeaseWhileDraining`); the cordon, the admission
  pre-flight and the warm-up each had a full `agent_admission_wait_sec` window — one shared deadline now,
  and the cordon wait is reported in `admission_wait_sec` (`TestCordonWaitIsChargedToTheAdmissionBudget`).

## [0.116.1] - 2026-09-12 - the ampere-16 vision seat is the measured winner

The `ampere-16` tier seeded `qwen3-vl-8b` for vision as a J-media inheritance from `blackwell-16`; it was
never run on the tier's reference box (`<node-c>`'s A2 16 GB), which served `gemma4-e4b-vision` by
hand. The 0.116.0 vision lane made the seat worth measuring: four candidates through the harness's OWN
`assess_image` / `ocr` / `vqa` over a labelled two-phase set on that box (record:
`Benchmarks and Optimizations/2026-09-12-a2-vision-bakeoff/`).

### Changed
- `ampere-16` vision seat: `qwen3-vl-8b` -> **`qwen38-27b-vision`** (Qwen3.8-27B UD-IQ3_S + mmproj-F16,
  ctx 8192, `reasoning: off`, swappable, ttl 300; aliases `vision` / `vlm` / `ocr`). Phase-2 hard set: the
  27B was the only candidate that read every serial, email and low-contrast string (OCR 9/9, VQA 5/5) at a
  14 s median on the 40 W lock; gemma4-12b is the fast runner-up (9/9, 4/5 at 3 s); qwen3.5-9b fabricated
  text twice and is disqualified for OCR; the old E4B seat missed the small-print codes and deferred once.
  Chosen on quality (tier doctrine). NOT propagated to `blackwell-16` / `volta-16` / `blackwell-2x16` — their
  8B seat was not in the bake. `TestAmpere16VisionSeatIsTheMeasuredWinner`; docs/tiers regenerated.

### Fixed
- `TestGPUReserveAcceptsAnExplicitWindowAndTheWrapperForm` no longer touches the machine's REAL lease: it
  passed a nonexistent `--config`, which resolves to built-in defaults and therefore the real state dir, so
  on a box whose card a render held it queued for the default 8 h and timed the root package out (and on an
  idle box it would have taken a real 8 h `training` lease). It now uses the temp lease fixture, `--wait 0`,
  and proves the `--detach` guard was passed via the later mutual-exclusion refusal.

## [0.116.0] - 2026-09-12 - vision work travels to a node with an idle card

The harness could place text work on fleet nodes (`agent_delegate`) but every image call —
`offload_vqa`, `offload_assess_image`, `offload_ocr` — ran against the local endpoint, so a fleet
node with an idle card and a vision seat sat unused while image QA queued behind the local render.
The first question was what `:8080` on `<node-c>` wanted a credential for: it is `pzdash` (Basic
Auth), not an inference server at all, and the node's llama-swap listens on loopback only — so the
lane proxies through `:18811`, and nothing new is bound.

### Added
- **Fleet task `vision`** on `POST /fleet/vision` (`fleetnode.VisionPayload`:
  `{job_id, task: vqa|ocr|assess_image, image: data URI, question?, brief?}`). Its own route only
  because its body is an image — capped at `vision_max_image_bytes` × 4/3 + slack
  (`VisionBodyCap`) rather than dispatch's 1 MiB — after which it joins the SAME admission path
  (`Server.admit`: bearer auth, known-id re-ack, drain, lease, band, queue cap) and the job store's
  concurrency cap (it contends for the shared llama-swap endpoint). The node runs the task on its
  own vision seat and stores the FULL `core.Result` as the done job's data, defers included, so a
  remote judgment and a local one have one shape. Advertised (`supported_task_types` +
  `vision_model`) exactly when it admits: `VisionLaneAdmissible` = a bound `vision_model` and the
  agent lane's reachability rule (loopback, or `fleet_auth_token`). Polls of vision jobs ride the
  agent lane's bearer gate (`JobView.Gated`) without being listed as agent runs.
- **`route` on the three tools and `--route` on `vqa` / `ocr` / `assess-image`** (`internal/visionremote`):
  `local` (default — every existing call is byte-identical), `auto` (an idle local card always
  runs it; when the machine-wide GPU lease is held — a render in flight or a text reservation —
  the least-loaded node advertising the lane runs it, and with no eligible node it still runs
  local), `remote` (force a node; none eligible ⇒ `deferred:true` with `defer_class` `capacity`,
  or `config` with no `delegate_remotes`, never a local run). The image is read on the CALLING box
  under its own `vision_max_image_bytes` and travels as a data URI; `meta.node` / `meta.placement`
  say where it ran. `engine: npu` OCR refuses a non-local route rather than silently running local.
- `core.Result.DeferClass`, `core.Meta.Node` / `Meta.Placement` (all omitempty);
  `delegate.NodeView.Tasks` / `VisionModel` decode `supported_task_types` / `vision_model`;
  `delegate.PlaceVision` ranks eligible nodes with the agent lane's `betterRemote` and returns the
  roster INDEX (an id is not an address).

### Operator notes
- The `<node-c>` A2 reports 15,356 MiB, not 16,384, because **ECC is enabled** (`nvidia-smi -q -d ECC`:
  Current Enabled); GDDR6 ECC is in-band and costs 1/16 of the frame buffer. `nvidia-smi -e 0` +
  reboot restores the full 16 GiB at the cost of ECC — an operator decision, not made here.

## [0.115.23] - 2026-09-10 - the re-pack cannot eat the wall

Register D-91, found by the D-03 live probe: the `<node-c>` 4B's final answer came back `length`-cut at 4,096 tokens
(12,100 chars), the loop was `done` after four minutes, and the structured re-pack then ran two grammar attempts
and the chat lane over the cut text — three full re-generations, ~690 s — into the 900 s wall, so a finished run
deferred `wall timeout after 900s` with nothing on the wire to say where the time went.

### Changed
- A `length`-cut final answer (`output_truncated`) is never re-packed: the run abstains at once (`output failed
  schema: re-pack skipped: the final answer was cut at the completion budget …`) with the partial in `output`;
  the cross-seat retry stays eligible. `TestRunAgentTaskTruncatedAnswerSkipsTheRepack`.
- No re-pack attempt starts with under a tenth of the wall left (capped at 45 s, `repackAttemptFloor`); the error names the wall
  and the attempts that ran. `TestRepackStopsAttemptsTheWallCannotHold`.

### Added
- `repack_ms`, `repack_attempts`, `repack_note` on the agent result, the delegation log and the delegate response.

## [0.115.22] - 2026-09-10 - supply-chain lane (H-26) and the cage's network floor (H-27)

### Changed
- `go 1.26.7` in go.mod (the built binaries were already on 1.26.7; the directive now says so — 1.26.5 carried
  five stdlib advisories, 1.26.6 broke unencrypted HTTP/2), `golang.org/x/sys` v0.47.0 → v0.48.0,
  `modelcontextprotocol/go-sdk` v1.6.1 → v1.7.0 (protocol 2026-07-28 negotiated at connect; backward compatible
  with 2025-11-25 clients on every endpoint; `ping`/`logging/setLevel`/`resources/subscribe` are rejected on the
  new revision; tool input/output schemas are validated — every harness tool passed). `go-landlock` stays at
  v0.10.0 (taken in 0.104.0 on 2026-08-27; GHSA-vv6c-69r6-chg9 closed there, register H-27's "still running v0.9.0"
  was stale).
- The Linux cage's Landlock ABI floor is 4 (`sandbox.NetABIFloor`), raised by `Run` for any lower request: the V4
  rule set denies TCP bind/connect only from ABI 4 on, and below it best-effort mode silently dropped the network
  rules while the callers asked for floor 1. Fail-closed on a kernel older than 6.7; the fleet's Linux node runs
  7.0. `TestEffectiveABIFloorCoversTheNetworkGuarantee`.
- `seat_tok_s` samples only completions of ≥ 1,024 tokens (was 128): the first live probe on the 27B recorded
  11.5 tok/s from a ~200-token answer whose call wall was mostly the 43 KB prefill — an under-read that would have
  tripled the next estimate. The wall_note names the threshold.

## [0.115.21] - 2026-09-10 - walls sized from the seat's own rate

Register D-03: walls sized for a fast seat were spent on a 30 tok/s thinker, and nothing in the harness could say
what a contract NEEDED on a given seat. Now every agent run measures the seat (the client wall of each planner
completion, the cold load the warm-up waited for), remembers it per seat, and publishes an estimate beside the
result — without touching the wall itself.

### Added
- `calls[].ms` (client-measured wall per planner completion) and `seat_tok_s` on the agent result (effective decode
  rate over the ≥ 1,024-token completions; also the ledger row's `tok_per_s`, empty on agent rows until now).
- `internal/seatrate`: `<state root>/seat-rates.json` (per seat: rate EMA 0.3, the slowest of the last five cold
  loads, samples; load-observe-save under an exclusive lock file with stale-lock takeover, atomic writes; a
  missing or corrupt file is no memory, never an error) and the estimate
  `cold load + (auto ? one think block : 0) + (steps − 1) × (128 tok + 6 s prefill) + final budget` at the seat's rate.
- `wall_estimate_sec`, `min_turn_sec` (cold load + one turn at the final budget, D-46) and `wall_note` on the agent
  result, the delegation log and the delegate response; `wall … is BELOW the estimate` logged by the node.
- Config `agent_seat_tok_s`: the rate to estimate with until the seat has recorded its own.
- Delegator retry floor = `max(agent_retry_min_sec, first attempt's min_turn_sec)`; the retry note names the source.
- `offload_review_diff` runs under the box's `agent_timeout_sec` when that is larger than the 300 s wire default
  (capped at the wire ceiling) — the wall half of register D-09: a 51 KB diff on the 27B spent 9 steps and timed
  out at exactly 300 s on 2026-09-10 while a 13 KB one finished in 3. `TestReviewDiffWallComesFromTheBoxTimeout`.
- OPERATOR-GUIDE "The timeout chain": the six clocks a contract crosses (contract → delegator poll + C-27 credit →
  admission/warm-up → node wall → loop budgets → engine timeouts and the GPU lease vs the media timeouts of C-33),
  the sizing rule and the measured reference numbers for both fleet seats.

## [0.115.20] - 2026-09-10 - the delegate result carries the loop accounting

Follow-up to D-89, found scoring its live acceptance (`runs/ff-0.115.19/`): the forced final step publishes its
evidence as `stop_note` on a `done` result, but the delegate wire (`delegate.ResultWire`, what the CLI prints and
the `agent_delegate` MCP tool returns) never carried `steps`, `stop_reason`, `stop_note` or `output_truncated` —
a forced answer at the 12-step cap, a `length`-cut partial and a voluntary complete answer published as the same
bytes, and the only way to tell them apart was the delegation log.

### Added
- `results[].steps`, `results[].stop_reason`, `results[].stop_note`, `results[].output_truncated` on the delegate
  response (CLI `delegate` and MCP `agent_delegate`), passed through from the node's `AgentWireResult`; all
  omitempty, so a pre-0.115.20 node's result publishes as before. `TestWireResponseCarriesLoopAccounting`.
- OPERATOR-GUIDE: how to read `stop_note` / `output_truncated` before trusting a `done`.

## [0.115.19] - 2026-09-10 - the last step asks for the answer

Register D-89, found by the D-43 measurement on 0.115.18: the `<node-b>` 27B (thinking off) spent all 12 steps of
ledger-01 on `read_file` / `search_files` / `list_dir` and the run deferred `step budget exhausted` with nothing,
although the whole 43 KB document had been replayed into its transcript. The trigger was an ungrounded goal
instruction in the contract (a SEGMENT-ID the document does not contain — the 27B hunted for it); the harness
defect is that nothing ended the hunt with an answer: every step, the last included, offered every tool with
`tool_choice: auto`, and the same-name cap (8) fired on the 9th search — the last step — without an answer-now
turn.

- **Forced final step.** The last step of a multi-step run offers NO tools, opens with an answer-now user turn
  (`agent.FinalAnswerTurn`: answer in the requested shape from what you have read; say inside the answer what could
  not be found), runs at the final completion budget (`finalMaxTokens`) with thinking off unless the seat is pinned
  to `thinking: on`. Its answer is `done` with `stop_note` "forced final answer"; a tool call returned on it —
  parsed, or written as text — is never executed and the run ends `budget` with the evidence in `stop_note` (not
  the tool-call-parser error an unparsed marker means on other steps). `calls[].forced_final` marks the call. A
  one-step run keeps its tools. `Loop.WithoutForcedFinal()` restores the old last step.
- **Mechanism, from the deployed sources:** tools are withheld rather than sent with `tool_choice: "none"` —
  vLLM 0.28.0 still renders the tools under "none" and its engine parser strips a call the model writes anyway (an
  empty answer); llama.cpp returns it as text. A tool-less request with tool history is accepted on every seat; it
  costs one re-prefill of the transcript, only on the path that used to return nothing.
- **Honest setup footer.** The replay's "complete file" footer now names lines `read_file` cut at 2,000 characters
  (it claimed completeness over them) and says an excerpt is all there is even where it begins or ends mid-sentence.
- Reviewer round (PR #303): the footer counts only lines `read_file` itself cut (an `N: ` line of exactly 2,000
  runes plus the marker) — a whole-content substring count claimed cut lines for any file that merely contains the
  marker text, this repo's `tools.go` included; the budget defer's `reason` carries `stop_note`
  (`step budget exhausted (N steps): forced final step: …`, the prefix unchanged); a forced final that is re-issued
  (empty answer) records two `calls[]` entries with `forced_final`, one per completion like every re-issue.

## [0.115.18] - 2026-09-10 - in-loop offload tools ride the planner seat

Register D-88, found by the D-43 measurement on 0.115.17: both `<node-b>` legs lost their 900 s wall because the 27B
planner called `offload_triage` three times, and each call loaded the workhorse (`gemma-4-e4b`) on the SAME
llama-swap — the cascade pin shares the planner's card and the `interactive` set is mutually exclusive by
design — so llama-swap evicted the 27B and the next step reloaded it: four 3-minute reloads per leg
(`seat-cmd-tp2.log` 15:44 / 15:48 / 15:56 / 16:00). The tools were kept on the workhorse "for its economics"
since the agent seat was introduced; the economics were a 6-minute swap per call.

- `pipeline.InLoopOffloadModel(planner, workhorse)`: the in-loop `offload_*` text tools run on the PLANNER SEAT
  whenever the planner is not the workhorse (it is loaded and idle while the tool runs); a single-model box keeps
  the workhorse. Applied to all three drive modes — the fleet-node agent task (`NewRecordlessOffloadForPlanner`),
  the MCP front door and the CLI single-loop planner (`NewInLoopOffloadForPlanner`). Two-tier keeps its documented
  zero-swap architect+editor pair.
- On the seat the tier calls render WITHOUT thinking (`chat_template_kwargs.enable_thinking=false`) — a mechanical
  shape, not a reasoning step; a thinking seat would spend the task's 64–768-token budget inside the think block.
  `Pipeline.RunTierWith` threads the option; `RunTier` keeps its signature (four consumers hold it as a func value).
- The fleet-node path logs which model the tools ride and why (ledger evidence).
- Reviewer round (PR #302): the cached in-loop pipeline behind the MCP front door and the CLI now builds the
  same seat-routed client as the fleet-node one (`inLoopClient`: `seat_endpoints` + cascade remote lanes) — a
  bare client would have sent a seat-targeted tool request to the base endpoint without an error; the tier
  cache keys on the render (`llamaclient.RenderKey`, `cacheKeyForTierRender`) so a seat's non-thinking answer
  never serves its thinking-render caller — option-free keys stay byte-identical, the live cache survives.

## [0.115.17] - 2026-09-10 - a 200 from the passthrough confirms the cold load

The 0.115.15 acceptance run's 27B row: a 187 s cold load kept outside the wall (0.115.11 working) and then
noted "warm request answered HTTP 200 … but /running never listed agent-pool ready" — because `agent-pool`
is an ALIAS and `/running` carries the real id. llama-swap proxies only after the upstream's health check
passed, so the 200 is the confirmation.

- `warmSeat`: a 200 from the passthrough reports "cold load Ns outside the wall" (with a note when `/running`
  lists the seat under another id) and spends no extra poll interval; a non-200 keeps the two-poll check
  and the "never listed" note.

## [0.115.16] - 2026-09-10 - the serving config has a gate

Register H-01 (INV-1 / INV-2 as a gate that can fail). The operator's hard rules — the cards do the inference,
every idle model unloads at five minutes — were enforced by hand: the templates were fixed twice (0.115.4,
0.115.7) and nothing stopped the next regression.

- `servingtmpl.Audit`: one checker over a llama-swap config — every model `ttl: 300`, no `-ngl 0` /
  `--n-gpu-layers 0`, no empty `CUDA_VISIBLE_DEVICES`, no persistent group, no preload hook, must parse.
- `install render` REFUSES a rendered config that breaks a rule (nothing is written; every violation named).
- `local-offload audit-yaml FILE...` runs the same checker over live files, exit 1 on any violation — the
  session-start audit's (H-02) input.
- `TestAuditRefusesEveryOperatorRuleBreak` (one red fixture per rule) and `TestEveryTemplatePassesTheAudit`.
- The gate's first run over the RENDERED tiers found blackwell-72's vision and STT seats with no `ttl`: the
  resident template's `# offload-seats: … ttl=none` directive (pre-rule, "a resident seat never unloads") made
  the renderer skip the ttl the 0.115.7 template fix had put on every static entry. The token is now ignored,
  every rendered seat carries `ttl: 300`, and the old test is inverted. Its first run over the LIVE configs
  found `<node-a>` memory-stack lane (embeddinggemma, bge-reranker-v2-m3) still on `--n-gpu-layers 0`; fixed
  on the box the same hour.
- Not covered yet: `--n-cpu-moe` above the tier's measured spill (no per-tier spill field exists to compare
  against), seat-unit `[Install]`, and UUID-vs-index pins (A-87) — separate rows.

## [0.115.15] - 2026-09-10 - thinking stays off after the first starved step

From the 0.115.12 acceptance run's 27B row: three starved finals (4,096 of 4,096 tokens reasoning each) in
660 s. Each thinking-off re-issue answered with a TOOL CALL, the next step went back to thinking, and the
seat starved again — it never reached the answer it gives in one no-think turn (6,667 tokens on 0.115.10).

- Under `agent_thinking: auto`, once a re-issue has rendered without thinking, every later planner call in
  that run does too: the seat has shown its think block does not fit the step budget. `on` still never sends
  the kwarg; `off` was already every call.

## [0.115.14] - 2026-09-10 - a truncated final answer gets the final budget too

From the 0.115.12 acceptance run's 4B row: the seat thought for 282 tokens, began the seven-array answer, and was
cut at exactly its 1,024-token tool-turn budget — 1,935 chars of a JSON prefix that no re-pack can repair; the
re-pack then failed on a backtick, because vLLM behind llama-swap ignores llama.cpp's `grammar` field and answers
fenced.

- A final cut on `finish_reason: length` with visible content is re-issued once at the final budget (4× the step
  budget, cap 8,192, thinking off under `auto`) exactly like an empty one; a second cut is accepted and flagged
  `output_truncated`. Same per-run episode bound (two).
- The grammar re-pack lane trims its completion to the outermost `{…}` before validating (the chat lane already
  did), so a fenced answer from a seat that ignores the grammar is not filed as invalid JSON.

## [0.115.13] - 2026-09-10 - the seat's answer shape is on the record

Register D-45. Two seat facts cost a week each because nothing recorded them: the 2026-09-04 tool-call
parser mismatch (hermes vs qwen3_xml) and the 2026-09-10 reasoning-key blind spot (`reasoning` vs
`reasoning_content`). The seat pin says what the seat IS; nothing said how it ANSWERED.

- `response_shape` on the wire: `reasoning_key=<reasoning|reasoning_content|none> reasoning_tokens=<reported|unreported>
  tool_calls_parsed=N completions=N`, derived from the run's `calls[]`; set before the defer branches.
- Docs: fleet-node result row.

## [0.115.12] - 2026-09-10 - the seat stops re-reading what it was handed, and stops repeating refused calls

Register D-48 and D-49, both from the 2026-09-10 4B rows: the seat re-read the 41 KB document its setup
replay had already put in front of it (173,784 prompt tokens, two copies), paged past its end three times,
and on another contract issued the same refused `list_dir` call eight turns in a row (729 s) before an empty
final.

- Setup replay feeds the EXACT-REPEAT breaker (D-48): a committed replayed call is registered as the first
  call of its (tool, args), so the model repeating it byte for byte is refused with "you already have that
  result" and the pinned result is restored if compaction cut it. The same-name cap and `disabledTools` are
  still not fed. A replayed `read_file` that reached EOF ends with a `(complete file: N lines — …)` footer.
  This reverses the 2026-09-07 "feed no breaker" rule on the corpus evidence.
- A refused call repeated byte for byte (D-49) — breaker, env rule, parked, unknown — is on its SECOND
  refusal withdrawn from the tool list, and the next turn opens with an answer-now user instruction.
- Docs: fleet-node `setup_actions` row; tests for all three behaviours.
- The structured re-pack runs on its OWN seat client whose per-call timeout scales with the budget
  (`repackTimeout`: max of `request_timeout_sec`, 120 s, budget/6 s). The 0.115.10 acceptance run's 4B row
  produced its 8,314-char answer and then died in the re-pack at the seat client's 120 s per-call timeout —
  filed as `structured re-pack unreachable`, a transport verdict on a seat that was answering. The contract
  wall still bounds every call.
- An answer that is ALREADY the requested object (register D-84) is validated directly — trimmed to its
  outermost `{…}`, same scalar coercion as the re-pack lanes — and spends no re-pack completion. The
  0.115.10 acceptance run's 27B row answered in shape (19,117 chars) and then spent its last 200 s of a
  900 s wall re-packing that JSON into the same JSON. Prose still re-packs as before.

## [0.115.11] - 2026-09-10 - a cold seat loads outside the wall

Register D-64. The admission pre-flight (0.111.0) waited for ANOTHER model's swap before starting the
contract's wall, but a seat that was simply not loaded loaded on the loop's first call — inside the wall.
Under `ttl 300` that is the common case: the `<node-c>` vLLM seat cold-started seven times in two hours on
2026-09-10, 125–250 s each plus a Triton JIT, and 89 of the `<node-b>` seat's 184 budget defers had `steps: 0`.

- `warmSeat`: when the seat is absent from `/running` after the pre-flight, one GET through llama-swap's
  per-model passthrough (`/upstream/<seat>/v1/models`) makes it load, bounded by the admission budget's
  remainder; `/running` is then polled until the seat reads ready. The time rides `admission_wait_sec`; a
  spent budget or a seat llama-swap never lists is named in `admission_note` and the run proceeds.
- `agent_admission_wait_sec` default 120 → 300 s (covers a vLLM cold load); negative still disables both.
- Docs: operator-guide config row.

## [0.115.10] - 2026-09-10 - the structured re-pack budget scales with the answer

0.115.8 let a thinking seat finish a long answer (the acceptance run's 27B row: 22,865 chars over seven
arrays; the 4B's: 8,380 chars). The re-pack then failed on both: its completion budget was a fixed 1,024
tokens (raised from 512 on 2026-08-30 for a four-field digest), so the grammar completion came back as a
JSON prefix and was filed as `output failed schema: invalid json: unexpected end of JSON input` / `invalid
character '�' after array element` — an abstention the caller could not tell from a real one.

- `repackBudget(output)`: `len(output)/3 + 512` tokens, floor 1,024, cap 8,192, on both re-pack lanes.
- A grammar completion cut at `max_tokens` (`finish_reason: length`) is never validated: the reason names
  `re-pack truncated at N tokens (the answer is M chars…)` and the retry runs at the cap; the chat-fallback
  lane treats a truncated answer as a miss instead of trimming it to its outer braces.
- Docs: fleet-node `structured` row.

## [0.115.9] - 2026-09-10 - the cross-seat retry is seat-aware

Register D-46. The verification retry ran inside whatever the first attempt left of `timeout_sec` with a
10 s floor, on whichever different node the placer chose. On 2026-09-10 a 603 s empty first attempt of a
900 s contract handed the `<node-b>` 27B a 296 s retry: it generated 4,178 tokens of think and timed out, on a seat
that was already mid-generation for another job. Over 318 retries in the corpus the pass rate was 13 %.

- Config `agent_retry_min_sec`: the least remaining budget a retry starts with (0 = the historical 10 s).
  Reference box: 300 (a cold vLLM load plus one 4,096-token turn at ~30 tok/s). The re-placement floor
  after a refused dispatch is unchanged.
- A first attempt that ended on an empty final (`stop_reason` `reasoning_starved` / `empty`) is never
  retried: the shape is the seat's completion budget, not a wrong answer.
- The retry never lands on a seat already running another job: a remote publishing `jobs_running > 0`, or
  the local seat with requests in flight (the same probe `route=spread` deals by).
- Every skip is named in `retry_note` (floor + knob, empty final, busy seat).
- Docs: fleet-node retry paragraph, operator guide, `agent_delegate` schema; example config.

## [0.115.8] - 2026-09-10 - an empty final is a named defer, never an answer

The 2026-09-10 retrospective (register D-01, two independent diagnoses) traced every empty delegation
result of the week to one path: on a vLLM thinking seat (`--reasoning-parser qwen3`) the whole per-step
`max_tokens` goes into the think block; vLLM returns it under `reasoning` with `content: null`, the client
decoded only `reasoning_content`, so the loop read silence — raised the budget 4x, re-ran, nudged with a user
turn, re-ran, and published the second empty as `done`; the node re-packed `""` into a schema-valid all-empty
object; the delegator failed it on acceptance and retried on a cold seat with the wall's leftovers. 20,526
tokens on the `<node-b>` 27B and 9,628 on the `<node-c>` 4B for zero visible characters, then a retry that timed out.

- `internal/agent/client.go` (D-41): decodes vLLM's `reasoning` key and
  `usage.completion_tokens_details.reasoning_tokens` alongside `reasoning_content`; a reasoning channel cut on
  `finish_reason: length` is NEVER promoted to the answer (the 0.81 fallback now applies to finished turns
  only); which key a seat answers under is logged once per seat.
- `internal/agent/thinking.go` (new): `Completion.Starvation()` names an empty completion
  (`reasoning_starved`: reasoning ≥ 0.9 × completion, or finish `length`; `empty`: a bare close), the
  `ThinkingMode` policy, `ContextWithoutThinking` (the client then sends `chat_template_kwargs:
  {"enable_thinking": false}`, the re-pack's knob), the final budget (4× the step budget, cap 8,192) and the
  per-call `CallRecord`.
- `internal/agent/loop.go` (D-42, D-44): the 0.113.5 4× raise and the 0.113.6 nudge are GONE. An empty step is
  re-issued once — same transcript, thinking off (under `auto`; `on` keeps thinking), final budget — and a
  second empty in a row ends the run on `StopReasoningStarved` / `StopEmpty` with `StopNote`; cost 1× + 1×
  instead of 1× + 4× + 4×. A re-issue that yields a tool call keeps the run going and a later empty final
  earns its own re-issue, at most two per run. The plan-recitation block is keyed per step so the re-issue
  cannot append the plan twice. `Result` carries
  `Calls` (D-47: finish reason, completion/reasoning tokens, chars, thinking-off per completion) on every return
  path and `OutputTruncated` when a non-empty final was cut on `length`.
- `internal/pipeline/agenttask.go` (D-42): an empty final answer DEFERS before any re-pack — class `budget` for
  `reasoning_starved`, `abstention` for `empty` — with the loop's arithmetic in the reason; `calls`,
  `stop_note`, `output_truncated` ride the wire on every result shape.
- Config `agent_thinking` (D-43) and contract / `agent_run` / `agent_delegate` subtask `thinking`
  (`auto` | `on` | `off`, validated by name at every door); `agent_run` and `agent_delegate` schemas document it.
- `internal/rig`: new axis `reasoning-starved` (precedence after `timeout`, before `budget`).
- Docs: fleet-node wire shapes (contract `thinking`; result `stop_reason` values, `stop_note`,
  `output_truncated`, `calls`; the new defer shape), operator guide (`agent_thinking`, the retired raise/nudge).
- Tests: the loop protocol (re-issue once, thinking off, final budget, named stops, both modes), the client's
  vLLM shape (probe of 2026-09-10 recorded verbatim), the node-side defer without a re-pack, contract validation,
  the rigger axis.
- Not in this release (register): D-45 seat response-shape validation into `seat_config_basis`, D-46 the
  seat-aware retry floor, D-48/D-49 setup-replay and blocked-tool breakers, D-64 admission for vLLM cold loads.

## [0.115.7] - 2026-09-10 - every template model unloads at five idle minutes

The operator rule (2026-09-08, restated 2026-09-10) is that no model stays loaded past five idle minutes on
any tier, any engine. The live boxes complied; the templates a fresh install renders did not: eight
templates still carried `ttl: 600` on `embeddinggemma` / `bge-reranker-v2-m3`, `win-cuda-resident.yaml`
(blackwell-32/48/72) declared no ttl at all by design, and `ampere-16.vllm_seat` had no `ttl_seconds`.
0.115.4's "ttl 300 in every template" was true of the swap seats only.

- Every model entry in every `llama-swap.*.yaml` template carries `ttl: 300`; the resident-roster template
  keeps its concurrency (no swap group) but no longer keeps models hot.
- `ampere-16.vllm_seat.ttl_seconds` 300.
- New gate `TestEveryTemplateModelUnloadsAfterFiveIdleMinutes` (`internal/servingtmpl`): fails on any
  template model without `ttl: 300`.
- Tier docs: a slow cache mount DEGRADES the seat to the same-box L1 tier (0.115.1); the page said REFUSED.

## [0.115.6] - 2026-09-10 - the 3-card tier registers the long window its third card buys

The operator's rule for `blackwell-3x16` is that the third card buys CONTEXT, and the tier must register more
than `blackwell-2x16`. It did not: both tiers declared `ctx_size` 131072 and `agent_ctx_tokens` 131072 on a
vLLM seat serving 163,840, and the triple template rendered no seat above the 131k window although the
reference box serves a `qwen3.8-27b-262k` twin live (q8_0 KV, pair pin, KV upgraded 2026-09-01) and the
3-card vLLM seat was measured at 262,144 with fp8 KV (Nightshift 2 seat v7, needle fidelity PASS x3; 238,592
at batched 3135 in the seat program).

- `profiles.json`: new optional field `long_ctx_size` (documented in `_fields`); `blackwell-3x16` sets it to
  262144; `blackwell-2x16` and `blackwell-3x16` advertise `agent_ctx_tokens` 163840 = the declared vLLM
  seat's served window (was 131072 - every contract was sized to 80 percent of the seat).
- `llama-swap.win-triple-blackwell.yaml`: renders the `qwen3.8-27b-262k` twin (literal `--ctx-size 262144`,
  no MTP drafter, pair pin, swappable, ttl 300) and adds it to the text swap set as `q38l`.
- `docs/tiers`: the tier page carries `long_ctx_size`; `render.tests.ps1` asserts the twin renders on 3x16
  and not on 2x16.
- The vLLM long layer (pp3 at 262,144, fp8 KV, desktop floor) is the composite tier's job (0.116.0).

## [0.115.5] - 2026-09-10 - the ledger counts the work the seats actually did

The operator's 30 % harness-share floor (2026-09-10) reads the ledger, and the ledger undercounted the
cards: an agent row recorded only the structured RE-PACK call's `tokens_out` — never the loop's own
generation — and nothing on a deferred run, so a 27B run that generated at 29–54 tok/s for six minutes was
ledgered as 0. Prompt tokens the seat processed were not recorded at all (`tokens_in` stays 0 on agent
rows on purpose: the summary counts it as tokens saved).

- `agent.Completion.Serve` carries `usage_completion_tokens`; `agent.Result` sums the seat's prompt and
  completion usage over every turn (`TokensIn`/`TokensOut`) and reports them on EVERY return — done,
  budget and error alike.
- `core.AgentWireResult`/`core.Meta` gain `seat_tokens_in` (omitempty); `tokens_out` is the loop's
  generation plus the re-pack's; both are stamped BEFORE the defer branches, so deferred results carry them.
- `ledger.Entry.seat_tokens_in` (omitempty), written by the pipeline's `entryFrom` and the delegator's
  `agent_delegate` row. The summary never adds it to tokens saved. The harness-share gate hook sums it.

## [0.115.4] - 2026-09-10 - the cards do the inference: no CPU residents, no bulk-in-RAM seats, ttl 300 in every template

Operator rule, 2026-09-10 02:00, after finding 80 GB of host RAM in use: THE THREE CARDS (and the fleet's
other two) DO THE INFERENCE — RAM IS OVERFLOW ONLY — and EVERY IDLE MODEL UNLOADS AFTER 5 MINUTES on every
engine and every node. The live `<node-b>` yaml had two `-ngl 0` residents (moved off 5060 Ti #1 by a 09-07
session) and two Flash-Next entries parking 28–32 expert layers in RAM; the templates rendered the same
shapes on a fresh install.

- `llama-swap.linux-cuda.yaml`: `bge-reranker-v2-m3` moves to `--n-gpu-layers 99` (it ran on CPU "to buy
  back ~245 MiB of VRAM"; that trade is forbidden).
- `llama-swap.win-dual-cuda.yaml`: the two `ttl: 0` entries (`offload-e4b`, `gemma4-26b-a4b`) become `ttl: 300`.
- `llama-swap.win-triple-blackwell.yaml`: the `qwen3.8-flash-next` and `qwen3.8-flash-next-262k` opt-in
  seats are removed; the display-card placement test drops their exemption (every rendered seat now answers
  to the display rule); the blackwell-3x16 note records why. Long context on the tier is the pair's
  `qwen3.8-27b-262k`.
- The only sanctioned host-RAM use remains spill of a model that does not fit its card (the 26B's
  `--n-cpu-moe`). Partner rules recorded in the operator's constitution: the ≥30 % harness-share floor and
  the `<node-c>` cache server backing every vLLM seat (a follow-up: `kv_cache_server` still binds one seat).

## [0.115.3] - 2026-09-10 - the linux template's 26B graphs flag follows the tier, as the windows templates already did

`setup/templates/llama-swap.linux-cuda.yaml` hard-coded `GGML_CUDA_DISABLE_GRAPHS=1` on both 26B seats
(lines 47 and 61) while the Windows templates rendered it from `profiles.json` `disable_cuda_graphs` through
`__M26_GRAPHS__` — so a Linux tier that measured graphs as a win could never turn them on. Found by the
2026-09-09 A2 CUDA-graphs A/B (+3.8 % gen, no MoE gain on sm_86; ampere-16 keeps `disable_cuda_graphs: true`,
so its rendered YAML is byte-identical before and after this change).

- Both Linux 26B seats now carry `env: [__M26_GRAPHS__, "${ld}"]`; the renderer drops the token WITH its list
  separator when graphs are on, so the loader-path macro every Linux seat needs survives in a valid list
  (`env: ["${ld}"]`), and `env: []` lines are still removed as before.
- `TestLinuxCudaGraphsFlagFollowsTheTier` pins both states on both seats, the raw template stays parseable
  YAML (`TestEveryTemplateIsParseableYAML`), and `TestRenderLeavesNoTokens` still holds.

## [0.115.2] - 2026-09-09 - a held card is a place in line: `gpu reserve` queues, and an unloaded seat stays unloaded

For the tenth time a session refused GPU work — "the harness is live, it pins models to these cards and can
spawn on any of them mid-segment, which is what voided two 5070 Ti runs" — on a machine that has had a
machine-wide GPU queue since 0.113.14 and a fleet to route around it. Two harness defects made that refusal
look reasonable, and no instruction surface said otherwise.

- `gpu reserve` now **queues** behind a current holder: `--wait` (default **8h**; `0` = the old fail-fast) in
  both the wrapper and `--detach` forms. The CLI called `TryAcquire`, so a held card was an *error*, while the
  pipeline underneath had queued renders behind each other since ADR 0018. The detached holder (`gpu hold`)
  is the process that queues, so the pid `reserved:` reports is the one that took the card; the parent waits
  for that, or for the holder's exit when the line did not move in time. Exactly two stderr lines per wait
  (queued / acquired) — never one per poll. The holder's declared window is reported, never trusted
  (`gpulease.Options.WaitOut`): the declared-window short-circuit is right for a 90 s tool call and wrong for a
  reservation, since holders release before their window as a rule — the first live proof found a `--wait 2m`
  waiter refused at once behind a `--for 3m` holder that released six seconds later. Every refusal names the
  flag that would have kept queueing. `gpu status` ends with the queue command (`queue_with` in `--json`).
- `--unload-seat` (or an explicit `--exclusive`) stamps the text lease **exclusive**. The text-load admission
  gate (ADR 0026) gated media leases only, on the reasoning that a text holder "unloads nothing" — false the
  moment `--unload-seat` existed: the next interactive text call pulled a model straight back onto the cards a
  measurement had just cleared. An exclusive text hold is now gated like a render: a load rides its
  `cascade_remote_lanes` lane or waits its own budget and is told who holds the card. Plain text leases are
  unchanged. `gpulease.Inspect` and `infoFrom` are one builder now, so the stamp cannot reach `ErrHeld` and
  miss the gate.
- `offload_status` publishes the LOCAL lease under `gpu_lease` (held/class/reason/expiry/exclusive) with
  `queue_with`, so a session sees the queue command in the same call that told it the card is held.
- Docs: `docs/systems/gpu-lease.md` ("A held card is a place in line", "Exclusive text holds"),
  OPERATOR-GUIDE, ADR 0039.

## [0.115.1] - 2026-09-09 - a slow or missing cache server degrades the vLLM seat, never refuses it

For hours on 2026-09-09 every delegation on `<node-b>` came back HTTP 500: `seat_fg.sh` refused to start
the 27B seat because the cache-server share wrote at ~36 MB/s (under `SEAT_L2_MIN_MBPS=200`), llama-swap
turned each refusal into a 500, and sessions concluded "delegation is not viable" and did the work in
the cloud context — the exact failure the harness exists to prevent, caused by a cache ACCELERATOR being
slow. (The slowness itself was a degraded WSL mirrored datapath after a `<node-b>` reboot: WSL→`<node-c>` 311 Mbit/s
while the Windows host wrote 640 MB/s to the same share; a further reboot restored 7.8 Gbit/s.)

- `seat_fg.sh`: a share that will not mount, cannot be probed, or writes under the floor now routes through
  `degrade_l2` — `SEAT_L2` is emptied (the MP server registers no store), the log says `CACHE SERVER
  DEGRADED — <why>`, `$WORK/seat-l2.status` records `degraded <when> reason=<why>` / `ok <when> mbps=<n>`,
  and the seat serves the same-box tier (L1 only). The only refusal left is the port-already-bound case.
  Verified live on `<node-b>` under the slow share: the next start logged the degradation, pinned its KV
  pool and loaded. `TestSeatLauncherDegradesInsteadOfRefusingOnACacheServerFault` pins the template.
- `docs/systems/cache-server.md` describes the degrade path and the status file.

## [0.115.0] - 2026-09-08 - accelerator work travels to the box that has the device (Coral Phase B)

The Coral was fully served on `<node-c>` and unreachable from `<node-b>` except by a hand-routed
`agent_delegate` contract naming a path on `<node-c>`'s disk. Operator: "ship it" (routing),
"fix it" (the path). Both, per ADR 0038:

- **Fleet task `accel`** — `{accelerator, tool, args, image_b64?, image_name?}`: the node writes the
  shipped image (cap 8 MiB) into a job-scoped dir, rewrites `args.image_path` to it, runs the tool on
  its LOCAL lane only (a forwarded call never forwards again), and returns the tool's dict; a mask
  the tool writes there comes back as `mask_b64`. Served exactly when the node lists a device;
  exempt from the fleet concurrency cap (it never touches the text endpoint).
- **`fleet_accelerators: [<id>]`** on a box without the device registers that device's tools
  locally — MCP surface and agent loop, `[FLEET: …]` in every description — forwarding through the
  new `internal/accelremote` to the first `delegate_remotes` node whose health lists the id.
  `image_path` is read on the calling box; its bytes travel with the job. Results carry
  `placement{node, base, accelerator, job_id, wall_ms, remote:true}`. A local device always wins
  a shared name (ADR 0037 extended: local first, then fleet, in config order). A box that lists
  nothing is byte-identical (pinned).
- `NodeView.Accelerators` decodes health's `accelerators`; `offload_status` lists a fleet device
  with `fleet: true`. ADR 0037 was missing from the decisions index; both rows added.

## [0.114.2] - 2026-09-08 - blackwell-2x16 seeds the pair's vLLM seat (0.114.1 overturned)

0.114.1 declared the 2-card tier had no vLLM seat, from an arm that spanned the RTX 5070 Ti display
card. That measured the wrong thing: the tier's two cards ARE the 5060 Ti pair, and the production
seat has run TP2 on exactly that pair since 2026-09-03 with the `<node-c>` cache server. Operator
correction the same afternoon. `blackwell-2x16` now seeds `blackwell-3x16`'s `vllm_seat` verbatim
except `device: "0,1"` (the pair on a 2-card box), including the fs_native cache server on `<node-c>`,
so a fresh install of the tier serves `agent-pool` at 163,840 instead of falling back to llama.cpp.
`TestDualBlackwellSeedsThePairSeatWithTheCacheServer` replaces the 0.114.1 test; the display-card arm
stays in the record only as the reason no seat is pinned to the 5070 Ti. Docs/tiers regenerated.

## [0.114.1] - 2026-09-08 - blackwell-2x16 has no vLLM seat, by measurement

`blackwell-3x16` seeds the 27B vLLM seat on its 5060 Ti pair; the 2-card tier was deliberately
left without one because its second card is the display card. That reason is now measured
(`Benchmarks and Optimizations/2026-09-08-blackwell-2x16-fit/`): a TP2 worker of the seat has an
**11.22 GiB per-card non-KV footprint** (util 0.85 on the utility pair: 2.32 GiB of KV = 141,266
tokens), and at the operating point that keeps the >= 4 GiB desktop floor on the 5070 Ti (util
0.50) vLLM profiled **-3.36 GiB** of KV and refused to start. No operating point hosts the seat
without starving the display. The tier keeps the llama.cpp 27B agent seat at 131,072; the
`profiles.json` notes carry the numbers and `TestDualBlackwellHasNoVLLMSeatByMeasurement` fails
any future copy of the seat across without a contradicting measurement. Docs/tiers regenerated.

## [0.114.0] - 2026-09-08 - the Coral Edge TPU is a second harness accelerator

`<node-c>` has carried a Coral Edge TPU since 2026-09-04 — driven, tuned and measured
(MobileNet v2 iNat p50 3.16 ms / ~300 inf/s, 70-71 C sustained, no throttle) — and nothing in
the harness knew it existed. The harness had exactly one pattern for "a device beside the GPU",
the Hailo-8L lane (ADR 0024); this release applies it to the Coral, per the reviewed design in
`docs/superpowers/specs/2026-09-04-coral-edgetpu-accelerator-design.md` (D1-D9, operator-approved
2026-09-08).

- **`coral-edgetpu`** is a declared accelerator (`profiles.json`, kind `tpu`, sidecar port
  18814 so a box carrying both devices never collides with the Hailo's 18813). Detected on Linux
  when `/sys/class/apex/apex_0/status` reads `ALIVE`; Windows has no apex driver and never
  matches. `hwdetect.DetectAllAccelerators` is the union of the probes IN ORDER (Hailo, then
  Coral) and `install detect` uses it.
- **Four capabilities, one owner per name.** `offload_classify_image` (EfficientNet-EdgeTPU-S
  ImageNet; MobileNet v2 iNat birds/insects/plants), `offload_object_detect` (EfficientDet-Lite
  0/1/2, COCO), `offload_semantic_segment` (DeepLabV3 MNv2 Pascal — semantic, NOT the Hailo's
  instance `segment`), `offload_image_embed` (EfficientNet-EdgeTPU-S extractor, 1280-d, space
  `efficientnet-edgetpu-s` — not CLIP, so no text tower). Two of those names the Hailo lane also
  owns, so the **shared-name rule** (ADR 0037) now governs registration on BOTH surfaces: the
  MCP server and the agent loop walk `config.Accelerators` in order and the FIRST listed owner
  of a name registers it; a later device's same-named tool is skipped and logged once. Tested in
  both orders. Today no box lists both, so the rule is a pinned invariant, not a live path.
- **The sidecar lives in this repo** (`accelerators/coral/`): `server.py` speaks the Hailo wire
  contract verbatim over loopback, keeps one resident Interpreter per model, refuses a
  non-loopback bind, serves only manifest-listed files whose sha256 matches
  (`models.json`, hashes computed from the fetched bytes), reads sysfs `temp`/`status` and never
  writes it, and exits itself idle. `test_server.py` runs the whole contract with
  `CORAL_ENABLED=0` on any box; `fetch-models.sh` downloads and verifies the zoo artifacts.
- **`internal/hailoclient` is `internal/accelclient`** with a `Device` label (`NewDevice`), so
  every defer reads `<device>: ...` whichever lane produced it. `New()` stays the Hailo
  constructor and the Hailo tests pass unmodified but for the import path. The MCP server's
  Hailo-only table, sidecar singleton, call path, handler and status block became per-device
  (`internal/mcpserver/acceltools.go`); the agent loop gained `AccelLane`s
  (`pipeline.NewLoopAccel`, `agent.ReadOnlyToolsWithLanes`) in config order, with the old
  single-lane `NPU` path kept for every existing caller.
- **Every agent builder wires every lane.** The live harness-path gate (a remote contract on
  `<node-c>` calling `offload_classify_image`) found the fleet-node delegation builder and the
  prompt-replay builder passing the Hailo lane only — the Coral tools existed for a local `agent_run`
  and were invisible to the same seat via `agent_delegate route=remote`. Both pass `Accel` now, and a
  root test parses every builder literal and fails on `NPU` without `Accel`. The same gate then
  caught the launcher: `coral-http.sh` read `$1` as the idle seconds where the harness passes
  `--idle-sec <n>`, and resolved `CORAL_HOME` one level up, so under the fleet unit it exited 2 before
  the sidecar started. It now parses the harness call shape, walks up to `venv/` for the home (flat
  or nested layout), and `test_server.py` runs the launcher with a stub python. Gate result on
  `<node-c>`: `agent_delegate route=remote` → `offload_classify_image` → `Ara macao (Scarlet Macaw)`
  0.746, 18 s wall, sidecar spawned by the harness.
- **Two installer gaps closed on the way.** `install.sh` merged NO accelerator seed at all
  (install.ps1 always had) — it now passes detect's verdict to `install seed --accelerators`
  and writes `installed.json`. And `fleet-serve` read `accelerators` only from that manifest,
  which a hand-built node (`<node-c>`, verified) does not have — health now falls back to the
  config's own list when the manifest lists none (D6), so the device is advertised and a
  delegator can route to it.
- **Config keys** `coral_endpoint` / `coral_sidecar_cmd` / `coral_timeout_sec` (30 — a cold
  model load on the TPU is ~0.5-2 s, unlike the Hailo's HEF-driven 60) / `coral_idle_sec`;
  `__CORAL_HOME__` in the seed (installer `--coral-home`, env `CORAL_HOME`, default
  `<OFFLOAD_HOME>/coral`). An EMPTY home token is now REFUSED at seed time for both devices
  rather than rendering a launcher path at the filesystem root.

Not in this release, by design (D5): face detection and PoseNet on the Coral (follow-ups once
their artifacts are verified on the box), Phase B proactive routing from a box without the
device, and the savings ledger for accelerator calls.

## [0.113.42] - 2026-09-08 - a vLLM seat could not be pinned, so two vLLM arms were never comparable

`ProbeSeatPin` only ever spoke llama.cpp: it GETs `/props`, which vLLM does not serve and
answers 404. Every vLLM seat therefore recorded `seat_config_sha256` / `seat_config_basis` as
**ABSENT**, and an unpinned row cannot enter a paired experiment - so the harness could not
tell two vLLM arms apart at all.

That is not theoretical. The 2026-09-08 A2 seat-quality comparison published a 4B-beats-9B
result whose two vLLM arms differed in `--gpu-memory-utilization` (0.65 vs 0.95) and ran on
harness builds nine releases apart, with the checkpoint, the engine build and the served
window all unrecorded on both sides. The conclusion rested on arms nothing in the record
could distinguish.

A 404 from `/props` now falls through to the shape vLLM DOES serve, verified live against
vLLM 0.28.0: `GET /version` for the engine build and `GET /v1/models` for the resolved
checkpoint path (`root`) and the served window (`max_model_len`). The vLLM basis is a
SEPARATE closed struct led by an `engine` field, so a vLLM pin can never collide with a
llama.cpp one, and the same refusal discipline applies - a missing discriminator yields no
pin rather than one hashed over empty strings.

**Stated limit, because a pin is only worth its honesty:** vLLM publishes no sampler defaults
and no `--tool-call-parser` / `--reasoning-parser` / `--gpu-memory-utilization` values on any
endpoint. Two seats differing only in those pin identically, and the basis line leads with
`vllm ` so an analyst reads the coverage class before trusting a match.

Also: `fleetview` labelled `agent_seat_resident` as **resident/cold**, which is simply not what
the flag means - it is a probe of llama-swap's `/v1/models` ROSTER, so a seat idled out by
`ttl:300` still reads true. The wording cost a session an hour chasing an imagined cold-seat
placement deadlock while the real fault was a node whose llama-swap config had been truncated
to zero models. It now reads **served / not served**, with a title explaining both states, and
a false value is styled as the error it is.

## [0.113.41] - 2026-09-08 - `go test ./...` failed on a clean checkout of main

The repo's own documented gate - "go test ./... exit 0, 76 packages" - returned **FAIL** on a
clean checkout of main, and had nothing to do with the repo. `TestNoStrayControlCharacters`
walked the working directory with a hardcoded skip list, so a gitignored tree in the repo root
was linted like source: a VoiceStudio `.tts-venv/` put vendored gradio and transformers bundles
in front of it, and their minified control bytes failed the suite.

It now lints the files **git tracks**, which is also the honest scope. The test exists to catch
a `\b` mangled into byte 0x08 in OUR sources - the failure that silently voided three gates,
including the ONE assertion guarding the triple-Blackwell display-card placement law. A control
byte inside a vendored minified bundle is normal and nothing here would act on it, and a file
git does not track cannot carry that defect into a commit.

Two things keep the narrowing from turning the gate blind. If git is unavailable (a source
tarball) it falls back to the filesystem walk and LOGS which source it used, so a silent
widening cannot masquerade as the real thing; and an empty file list is now a hard failure
rather than a clean bill.

Proven by mutation: byte 0x08 planted in the tracked `setup/render.tests.ps1` turns it RED
naming the file and the byte, the same byte planted inside an ignored tree leaves it GREEN,
and the restore is byte-identical.

## [0.113.40] - 2026-09-08 - the flagship seat was declared but never proven to render

0.113.39 gave `blackwell-3x16` its measured vLLM seat. Two green gates covered it and neither
covered the join between them: `TestEveryDeclaredVLLMSeatValidates` checks the tier's JSON in
isolation and never renders it, while `setup/render.tests.ps1` renders the tier but exercises the
FALLBACK path, because the seat renders only when `Detect` finds the hand-built venv and the
weights - false on CI and on every box but one.

So the question nothing asked was: does the declared seat, put through the template that tier
actually renders, produce a config llama-swap can load? `insertVLLMSeat` refuses a template whose
`# offload-seats:` directive places no resident, and `blackwell-3x16` renders
`llama-swap.win-triple-blackwell.yaml` rather than the linux template every other seat test uses.
A tier could have shipped a seat that fails to render on the only box able to run it.

It does render, and `TestFlagshipSeatRendersIntoItsOwnTemplate` now holds that: the COMMITTED
seat read from the tier table, through the real template, parsed as YAML, asserting the entry
drives the PowerShell stubs, keeps `useModelName`, joins a matrix var, idles at ttl 300, and
carries every declared alias.

**Mutation testing found the first draft of this gate half-blind.** Three defects were injected;
one turned it red and two SURVIVED - dropping the seat's aliases, and moving `max_num_seqs`. Both
assertions compared the render against the same declaration they were derived from, so they could
not fail when the declaration itself moved: a self-consistency check that certifies nothing. The
gate now also asserts the MEASURED absolutes a change has to justify - 32 streams, a 163,840
window, and the `agent-pool` alias the harness binds `agent_model` to. All three mutants now turn
it red for the stated reason, with a byte-identical restore.

## [0.113.39] - 2026-09-08 - the flagship tier shipped no vLLM agent seat at all

`blackwell-3x16` is the tier whose reference box **is** the 3-card workstation - the machine that produces most of
the fleet's numbers. It declared no `vllm_seat`, and `llama-swap.win-triple-blackwell.yaml` carried no seat entry,
no `agent-pool` alias and no cache server. So a fresh install of that tier bound the agent lane to the llama.cpp
`qwen3.8-27b` seat, while the box itself served `qwen3.8-27b-vllm` across the 5060 Ti pair with a 163,840-token
window and a 50 GB warm KV store. That is the defect ADR 0035 and #267 fixed for `ampere-16`, one tier over.

The tier now declares its measured seat, and the schema grew the three things that seat needs and `ampere-16`'s
single-card seat did not:

- **`tensor_parallel`**, validated against the device list. Two cards declared as one loads the whole model onto the
  first card and silently ignores the second; one card declared as two refuses to start. Both are authoring mistakes
  the tier table now catches, and the refusal names which one the engine would do.
- **`cache_server`**, the seat's binding to the LMCache KV store, with `ConfigBlock` deriving the harness's
  `kv_cache_server` half from it. Those were two hand-maintained descriptions of one store - `kvcacheserver.go` says
  so itself - and a chunk size that agrees in one file and not the other produces a store that registers cleanly and
  then serves nothing.
- **`launch`**, because a Windows box whose engine lives in WSL starts differently: llama-swap runs as SYSTEM and a
  WSL distro belongs to the interactive user, so the entry drives PowerShell stubs that trigger a scheduled task in
  the operator's session. New reference set under `setup/templates/vllm-seat/windows-wsl/`.

**A render-time refusal that prevents silent corruption.** `kv_cache_dtype: fp8` together with a cache server and no
LMCache overlay is refused outright. Stock LMCache restores fp8 pages corrupt (upstream PR #4253) *and reports
success while doing it* - the store logs hits, the engine answers, and the recovered context is wrong. A seat that
serves bad text while looking healthy is the worst thing this package can emit, so it is refused rather than left to
an operator's memory.

**The third card is deliberately not in this seat.** The 3-card pipeline layout serves a *smaller* window (100,352
against 163,840), costs 32 GB of host RAM instead of 8, draws in the display card, and can use no cache server at
all - LMCache's documentation addresses per-TP-rank behaviour and never mentions pipeline parallelism. It also
failed the delegation gate, deferring at 300,010 ms on a contract the 2-card seat finished in 272 s. `ttl_seconds`
is the house 300: the seat holds two cards while up, and an idle seat that keeps them is a card spent on nothing.

Gated by `TestTripleBlackwellVLLMSeatNeverSpansTheDisplayCard` (display card, tp/device agreement, ttl, cache
server), proven able to fail by mutating each of the three back and confirming a byte-identical restore. Tier docs
gain a generated **Agent seat** section, so a seat is no longer documented only inside a tier's free-text notes.

## [0.113.38] — 2026-09-08 — five Blackwell tiers were shipping a ~40% generation loss

`GGML_CUDA_DISABLE_GRAPHS=1` was baked into the `win-cuda` and `win-cuda-resident` templates, on the 26B seats. On
sm_120 that flag is pure loss: measured 2026-09-05 (b10720, 3 fresh-serve reps, temp 0, exact-string gate), turning
CUDA graphs back **on** took the 26B seats from **41.4 → 58.0 t/s** and **39.0 → 58.8 t/s**, prefill unchanged and
**output byte-identical**. The triple-blackwell template dropped it as cargo on that evidence; the other two kept
it, so `blackwell-16`, `blackwell-8`, `blackwell-32`, `blackwell-48` and `blackwell-72` all still paid it.

It could not simply be deleted — `win-cuda` also renders `ampere-16`, `ampere-8`, `ampere-6` and `volta-16`, where
graphs-on has never been measured. So it becomes a tier field, `disable_cuda_graphs`, **defaulting to false (graphs
ON, the measured state)** and set true only on those four unmeasured tiers, which keep today's behaviour exactly.

### Gates
- `blackwell-16` must render **no** `GGML_CUDA_DISABLE_GRAPHS` on any env line, and its 26B env must be the tier
  `gpu_env` alone — no empty `env: []` left where the flag used to be.
- **`ampere-8` must still carry it.** It is the tier that both serves the 26B and has no graphs-on measurement, so
  it is the guard that the sm_120 result does not silently transfer to Ampere.

Both assertions read **env lines**, not the file text. A bare `-match` also hits the template's own comment about
the flag — the fourth time in this release series that a text match found prose instead of a directive. The first
attempt at the token also put a bare `__M26_GRAPHS_ENV__` line inside a mapping, which
`TestEveryTemplateIsParseableYAML` rejected immediately; the token now sits inside a list literal so the raw
template stays valid YAML.

## [0.113.37] — 2026-09-08 — two seat-spec defects found by actually launching the arm nobody had launched

The `ampere-16` seat spec was written from one working deployment and generalised without ever being tried on a
second model. Serving Gemma-4-12B under vLLM on the same card broke it twice before it started.

### Fixed
- **`--limit-mm-per-prompt` listed only image and video.** The template's own comment said "all 0"; the flag did
  not. Gemma-4 is audio-multimodal, so vLLM built the audio tower anyway and died at init with
  `AttributeError: 'Gemma4UnifiedAudioFeatureExtractor' object has no attribute 'fft_length'`. Now lists audio too,
  with the rule stated: name **every** modality the checkpoint has.
- **"fp8_e5m2 KV is free on Ampere" is false as a blanket claim.** Measured on the reference A2 (SM86):
  `FP8 KV cache is not supported by the Triton attention backend on NVIDIA A2 (compute capability 8.6); native FP8
  (fp8e4nv) requires SM89+`. The tier's Qwen3.5 seat gets fp8 because it runs FlashAttention; Gemma-4 on the **same
  card** falls back to Triton and refuses to start. It is a per-SEAT, backend-dependent property — a tier that
  hard-codes it breaks every model whose backend lacks it. Documented on the field.

### Measured while doing it
`gemma-4-12B-it-qat-w4a16-ct` (8.28 GiB weights) needs util **0.95** on a 15,356 MiB A2 to serve a 32,768 window —
KV pool 48,925 tokens, 1.49× concurrency, 14,140 MiB. **The 12B gets 32k where the 4B gets 131k on the same card.**

And the quality question it was launched to answer, judged blind by the same instrument (24 judgements, four
candidates per packet, no model/engine/timing visible):

|  | llama.cpp | vLLM | engine effect |
|---|---|---|---|
| Qwen3.5-4B | 7.38 | **7.75** | +0.38 |
| Gemma-4-12B | 6.21 | 6.50 | +0.29 |
| **model effect** | **−1.17** | **−1.25** | |

Giving the 12B the winning engine did not rescue it: the engine is worth ~+0.3 to both, the 12B is worse by ~1.2 on
both, and it took 1 of 24 firsts. The `ampere-16` binding stands on quality evidence. Caveat kept in the record:
one task shape, and these are different model *families*, not just sizes. Record:
`Benchmarks and Optimizations/2026-09-08-a2-seat-quality-reeval/`.

## [0.113.36] — 2026-09-08 — the vLLM seat occupied a GPU forever, and a test demanded that it do so

0.113.33 shipped a rendered vLLM agent seat that never unloads. On the reference A2 that meant **10,338 of
15,356 MiB held with the engine idle and llama-swap reporting no models running at all** — the harness's own idle
rule could not reach it, because none of the four mechanisms pinning it were things llama-swap controls.

Operator ruling: *"NO MODEL GETS TO BE LOADED FOR MORE THAN 5 MINUTES IF IT GOES UNUSED, AS THE REST OF THE
HARNESS… 30 MINUTES OF LOADING TIME IS WAY BETTER THAN NO USE, AND NO USE IS WHAT YOU CAUSE BY LEAVING A MODEL
LOADED AND UNUSED."* A long cold load is paid once, by one request. A pinned card is paid continuously, by
everything else that wanted the hardware.

### Fixed — four independent mechanisms, each sufficient on its own

| mechanism | why it defeated the idle rule | now |
|---|---|---|
| `ttl: 0` on the rendered entry | llama-swap never idles it out | `ttl: 300` (`ttl_seconds`, house default) |
| `groups: {persistent: true}` | nothing may evict it | no `groups:` block |
| `hooks.on_startup.preload` | loaded the moment llama-swap starts, wanted or not | no `hooks:` block |
| unit `[Install] WantedBy=` | **a boot-enabled unit outlives its own front door** — llama-swap's TTL can only unload what llama-swap *started* | no `[Install]` section |

The last is why the other three were not enough: the engine is a *system* unit, so once enabled it returns at boot
and stays up regardless of what the entry says.

### Fixed — the gate that made it permanent

`TestVLLMSeatRendersAsAResidentMatrixMember` **asserted `ttl` must be 0**, with the comment "the agent lane must not
idle out and pay a cold reload". Fixing the seat failed the build. The assertion is inverted: TTL must now be
positive and ≤ 1800, and three further guards refuse the return of a `groups:` block, a `hooks:` block, or an
`[Install]` section.

Those guards assert against the **parsed** document and against line-anchored, comment-excluded directives. The
first drafts grepped the text and matched the linux-cuda template's own comments explaining why `persistent` was
abandoned, and then this file's own "NO `[Install]` SECTION ON PURPOSE" — the same regex-cannot-tell-a-mapping-from-
a-comment defect fixed in 0.113.33.

### Amended
ADR 0035's residency decision is superseded in place, with the four mechanisms tabulated so nobody rebuilds it from
the record. The front-door design survives: llama-swap remains the one endpoint, `cmd`/`cmdStop` still drive the
unit through polkit, and `gpu reserve --drain --unload-seat` still frees the card.

### Applied to the live reference box
The A2's resident engine was stopped, `vllm-a2-seat` was `systemctl disable`d, and its llama-swap entry lost the
preload hook and the persistent group with `ttl: 0 → 300`. Card went from 10,338 MiB to **0 MiB**; the fleet node
stayed active and still advertises the seat, which now loads on first use.

## [0.113.35] — 2026-09-08 — the 3-card tier shipped the loser of its own vision bake-off

### Added
- **`blackwell-3x16` vision seat: `qwen3-vl-8b` → `qwen3-vl-32b`, with the 8B keeping OCR.** The 48 GB gates measured the two on the
  reference box and split the roles by which model won which: visioncanary **MMMU 0.740 vs 0.630 (+11 points**, clearing the ≥8 promotion
  bar) sends VISION to the 32B, while **DocVQA ANLS −3.7 and a dense-transcription regression** keep OCR on the 8B. The split is itself
  quality-driven — each role goes to the model that measured better *at that role* — which is why it must not be collapsed back to one seat.
  The decision was **applied on the box** (both live config files bind `vision_model=qwen3-vl-32b` / `ocr_model=qwen3-vl-8b`) and the tier
  table was never updated, so every fresh install of the tier whose reference box IS that machine got a measured 11-point MMMU regression.
- Seat schema gains `split_mode` (`-sm`), `tensor_split`, `image_min_tokens`, `top_p` and `top_k`, which is what the measured
  configuration needs and what previously made this upgrade "blocked". `-sm tensor` at the same `--tensor-split 25,25` produced
  **byte-identical output 5/5** on a fixed image+prompt while generation went 19.6 → 34.1 t/s — identical output is what makes it free
  rather than a trade.

### Fixed
- **The ampere-16 seat justification was a speed argument, and it is withdrawn.** The 2026-09-04 bake-off scored
  `not deferred AND len(findings)>=3 AND len(summary)>40` — a shape check the harness's own acceptance linter calls `SHAPE-ONLY (passes
  garbage)`. Its 8/8 meant "emitted well-formed output", not "equally good", and the seats were then separated on wall time.
  A blind re-evaluation of the retained answers against the ground-truth ADRs (3 lenses × 8 contracts = 24 judgements, candidates
  anonymised and permuted, no model name/engine/timing visible) puts the vLLM seat first on quality: **overall 7.58 vs 6.79 for the same
  weights on llama.cpp and 6.38 for the 12B**, first on specificity and coverage, and the only one with zero filler findings. The binding
  stands — on quality evidence. Record: `Benchmarks and Optimizations/2026-09-08-a2-seat-quality-reeval/`.
- The display-card gate split multi-device pins: it compared the whole `CUDA_VISIBLE_DEVICES` string against `"1"`, so a pin of `"0,1"`
  would have passed while putting half a 32B vision seat on the desktop's card.

### Still open
**The 12B has never been served under vLLM, and that is the question that was actually asked.** The engine alone moved the *same 4B
weights* +0.79 overall and removed every filler finding; the measured comparison is vLLM-4B vs llama.cpp-12B, confounded by engine. The
llama.cpp 12B measured 7,512 MiB, so a 12B under vLLM (w4a16 or Q4, `fp8_e5m2` KV) plausibly fits at util ≈0.85 with a 32–64k window.
That arm is the next measurement, judged with the new instrument rather than the shape gate.

Not propagated to `blackwell-2x16`: the 32B spans both cards of its pair, and on the 2-card tier the second card pays the desktop tax.
That tier needs its own fit measurement first.

### Gate
`TestTripleBlackwellVisionSeatIsTheMeasuredWinner` — the tier must declare BOTH a vision and an ocr seat, vision must be the 32B and ocr
the 8B, and the 32B must carry the flags it was measured with. Mutation-tested against `main`: it reports the collapsed single-seat state.

## [0.113.34] — 2026-09-07 — blackwell-8 advertised half the window its own agent seat was serving

`agent_ctx_tokens` is what the harness ADVERTISES, and the fleet sizes every delegation contract from it. When it
understates what the seat actually serves, the node quietly works at a fraction of its window and nothing ever
errors — the failure the delegation rules already call out by name ("size contracts from the live ceiling, never
from a written figure").

### Fixed
- **`blackwell-8` advertised `agent_ctx_tokens` 16384 while its rendered `qwen3.5-9b-agent` entry carries an
  explicit `--ctx-size 32768`.** That literal is backed by the tier's own fit measurement on the RTX 5060 (6,344
  MiB at 16K q8_0, 6,696 MiB at 32K — Qwen3.5's Gated-DeltaNet hybrid KV barely grows with context), and the twin
  `ampere-8`, same 8 GB class and same seat, has advertised 32768 since its own raise. Every contract to a
  blackwell-8 node was cut to half the window the seat was already serving.

### Not changed, deliberately
`blackwell-8.ctx_size` stays **16384**. It governs the CASCADE seats (e4b/e2b and the cpu_moe 26B), not the agent
seat, and the 32K fit above was measured on the 9B alone — reading it as evidence for the cascade is exactly the
notes-field-as-measurement error this table keeps being bitten by. `ampere-8` ships 32768 for the cascade on the
same 8 GB class, so the raise is plausible and probably right; it needs one llama-bench pass on the `<node-e>`
reference box before it ships, not an inference from a sibling.

### Gate
`TestAgentWindowMatchesWhatTheAgentSeatServes` — for every tier declaring an agent seat, the advertised window must
equal what the seat serves: a literal `--ctx-size` must match `agent_ctx_tokens` exactly, and a seat carrying
`__CTX__` must match `ctx_size`. Mutation-tested: it reports blackwell-8's 16384/32768 split.

## [0.113.33] — 2026-09-07 — the 3-card tier could not load, and the gates that should have caught it were blind

0.113.32 shipped `blackwell-3x16` with a llama-swap config that does not parse, a placement law nothing enforced,
a media block copied wholesale from the 2-card tier, and the heaviest seat unpinned. CI passed it 5/5, because
every render assertion is a regex over the config TEXT and a regex cannot tell a mapping from a comment.

### Fixed
- **The template is not parseable YAML.** The generator that added the opt-in over-2-card seats anchored on the
  literal `matrix:` and hit its FIRST occurrence — inside the comment ``RESIDENCY IS DECLARED WITH `matrix:` `` —
  wedging the seat block into that comment line and leaving its tail at column 0 as a bogus top-level key, with the
  real mapping duplicated below. `yaml.v3`, the parser llama-swap itself uses, rejects the file outright.
- **The placement assertion could never fail.** `setup/render.tests.ps1` gated on `'CUDA_VISIBLE_DEVICES=1<0x08>'`:
  a regex word boundary (`\b`) written through a double-quoted string became byte 0x08, which no config contains,
  so `-notmatch` was always true. CHANGELOG entries record two earlier instances of the same corruption
  (`\bnone\b`, `\bq354\b`) — and both were themselves still corrupted in this file. Restored and repaired.
- **The law was actually being broken.** `blackwell-3x16`'s entire media block was byte-identical to
  `blackwell-2x16`'s, whose device map is 0=utility / 1=FAST. On three cards 1 is the DISPLAY card, so the copy put
  the vision seat (11,751 MiB) and image compute on it and named card 2 nowhere — which is also why the 3-card box
  measured no media difference from the 2-card box. It was an unadapted copy, not a hardware result. Corrected to
  what the reference box runs: vision and STT to device 2, imagegen compute `cuda:1` / donor `cuda:2`.
- **The 27B spread onto the display card.** `qwen3.8-27b` was the only default seat with no `CUDA_VISIBLE_DEVICES`
  pin and it carries `-ngl 999 -sm layer`, so llama.cpp divided it across every visible card. The reference box hit
  this and re-pinned on 2026-08-31 ("Old split 21,29 spanned 5070Ti+5060Ti by CUDA fastest-first"). Now pinned to
  the pair.

### Beware
**Two device orderings are in play and the display card has a different index in each.** `gpu_env` /
`CUDA_VISIBLE_DEVICES` is PCI_BUS_ID order (display = **1**); the ComfyUI `*_pool_*` keys are ComfyUI's
fastest-first order (display = **cuda:0**, the pair = cuda:1/cuda:2). Confusing them is how the media block ended up
pointing at the display card twice, and a first attempt at the fix got it wrong in the opposite direction.
`videogen_pool_compute` stays `cuda:0` as a documented law exception: an int8 DiT cannot compute on a non-default
CUDA device (ComfyUI-MultiGPU #220) and moving it threw `cudaErrorIllegalAddress` mid-render.

### Added
- **The installer renders the `ampere-16` vLLM agent seat** (`internal/vllmseat`, `install vllm-seat`). ADR 0035
  said the seat was hand-installed and not something the installer renders; the consequence was that fresh installs
  kept getting `qwen3.5-4b-agent` — the arm that LOST its own bake-off (8/8 at 159 s vs the vLLM seat's 8/8 at 45 s,
  4x the window, GO-decided 2026-09-06). One Spec now produces the llama-swap entry, the systemd/polkit/wrapper
  files and the `agent_model` binding, so the seat and the model the agent lane routes to cannot disagree.
  Prerequisites are NOT rendered on purpose: the venv and weights are hand-built, so a box without them binds the
  declared llama.cpp fallback and prints why.
- Three values measured, adopted live, and seeded by no tier: `agent_max_tokens` 4096, `ffmpeg_video_encoder`
  `h264_nvenc`, and `healthCheckTimeout` 300 → 600 for the Flash-Next cold load.

### Gates (each mutation-tested RED against the shipped state before landing)
- `TestEveryTemplateIsParseableYAML` / `TestRenderedConfigIsParseableYAML` — every template AND its rendered output
  must parse with `yaml.v3`, not merely match a regex.
- `TestNoStrayControlCharacters` — a C0 byte in any source or config file, the mechanism that voids a regex silently.
- `TestTripleBlackwellNeverSchedulesOntoTheDisplayCard` — both orderings, and it fails if the 3-card media bindings
  are ever byte-identical to the 2-card tier's again.
- `TestEveryDefaultTripleBlackwellSeatNamesItsCards` — every default seat must name the cards it may use.

## [0.113.32] — 2026-09-07 — `blackwell-3x16` ships, and three measured wins get wired

The operator asked three times for the 3-card tier and for install defaults to follow the measurements. This is
that, not another note about the gap.

**`blackwell-3x16` is a real tier.** The fleet's MOST-measured machine — the reference workstation, three
Blackwell 16 GB cards = 48.9 GB since 2026-08-31 — had no tier id, and `Get-Profile` filed it as `dual-gpu`: a
tier no hardware has ever run and that the installer refuses to install (CUDA 13 cannot compile sm_70). A fresh
install on the box that produces most of the fleet's numbers would have seeded a projection. Now shipped end to
end: a classifier rule in `setup/detect.ps1` and `internal/hwdetect/classify.go` placed BEFORE the generic `-ge 2`
catch, a `blackwell-3x16` profile, and `setup/templates/llama-swap.win-triple-blackwell.yaml`.

The template encodes the measured **placement law**, which is the whole point of the tier: **devices 0 and 2 (the
5060 Ti pair) carry every default seat; device 1 (the RTX 5070 Ti) is the DISPLAY card and gets nothing.** A render
test asserts mechanically that nothing is pinned to device 1 — on 2026-09-04 a 3-card engine at util 0.87-0.90
left that card under 1 GB, Windows dropped to a 720p-class mode and the operator had to reboot. The third card
joins only for two opt-in over-2-card seats that earned their place: `qwen3.8-flash-next` at ncmoe28 /
`--tensor-split 28,10,10` / q8_0 KV / 131k (12.5-12.9 tok/s, **+47%** over the best 2-card arm, and **15/16** on the
seed-48 decision set = exact parity with the 27B incumbent) and its native-262k twin at ncmoe32 / 32,8,8.

What the tier does NOT claim, because it was measured: the agent seat stays the **2-card-pinned** 27B. The 3-card
vLLM seat FAILED the delegation gate — a real harness digest contract DEFERRED at 300,010 ms where the two-card
seat finished the same contract in 272 s. Three-card serving is opt-in long-context work, not the delegation lane.
And the third card buys context and concurrency, not speed: llama.cpp's 3-way ceiling is 134.4 tok/s at c32 vs the
pair's 110-120, while pipeline-parallel vLLM is ~8% SLOWER single-stream than tp2 because stages serialize.

**CUDA graphs back ON for the 26B seats** (`blackwell-2x16` and the new tier). `GGML_CUDA_DISABLE_GRAPHS=1` was
cargo: it predated every retained yaml backup with no recorded reason. Measured 2026-09-05 (b10720, 3 fresh-serve
reps, temp 0, exact-string gate): gen **41.4 → 58.0 t/s** (`gemma-4-26b`) and **39.0 → 58.8 t/s**
(`gemma-4-26b-agent`), prefill unchanged, output identical, GBNF fine. Left set on the non-Blackwell templates,
which that measurement does not cover. `GGML_CUDA_GRAPH_OPT` stays off (changes output; crashes the MTP 12b).

**`ampere-16` serves the window it was measured at: 32768 → 131072.** 0.113.29 held it back because "no installer
path renders the vLLM seat". That was the wrong call — the fix is to render the seat, not to ship a smaller number
than the hardware was measured at. On the reference A2 16 GB at its shipped 40 W / 1200 MHz lock the vLLM seat
serves `max_model_len` 131,072 with a 356,721-token pool at 11,888 MiB, fans out to **293 tok/s at 32 streams**
with zero failures, and answers the harness's own 8-digest set **8/8 at a 45 s median against the llama.cpp seat's
159 s**.

Also fixed: `setup/tests/selftest-profile-measure.test.ps1` asserted `ampere-8` at ctx 16384 and had been failing
on main since that tier's own measurement raised it to 32768.

## [0.113.31] — 2026-09-07 — the fleet facts the docs still had wrong

Follow-up to 0.113.30. A staleness sweep across every surface that repeats fleet hardware facts found the docs
describing machines that have since changed and tiers that have since been measured. Documentation only.

- **`setup/SETUP-AGENT.md`** — named only `ampere-8` and `blackwell-16` as validated on real hardware; seven
  tiers are measured. `blackwell-16` and `ampere-6` rows carried pre-measurement projections.
- **`docs/systems/setup-installer.md`** — "each matching the tier it actually runs" is no longer true for two of
  the three boxes it cites, and the multi-GPU classification row did not say what `Get-Profile` actually returns
  for three cards (`dual-gpu`).
- **`docs/systems/fleet-node.md`** described <node-b> in the present tense as a 2x16 GiB box; it has been 3x16 GiB
  since 2026-08-31. **`docs/systems/accelerators.md`**'s "today that is one device" is a fleet inventory claim that
  ignores the measured Coral on `<node-c>` (device measured, harness lane still an unmerged draft).
- **`README.md`** generalised a 780M/RDNA3 figure to all AMD: prompt processing is ~4x CPU on RDNA3 but a measured
  **1.26x** on GCN, because Vega has no matrix cores.
- **`docs/OPERATOR-GUIDE.md`** had no `blackwell-2x16` row at all — the tier with the most measurements behind it.
- **`docs/tiers/reports/*`** banners stopped at 2026-08-05 and read as current; ADR 0019's addendum called measured
  tiers projected.

No code paths change.

## [0.113.30] — 2026-09-07 — three tiers were measured and the docs said PROJECTED: notes corrected, fleet table corrected

A rebuild of the tier/model matrix's "Tested on (reference hardware)" column searched the evidence instead of the prose,
and found that this repo has been describing three of its own measured tiers as projected — in some cases directly
above the measurements that refute it. Documentation only: no code paths, no serving values, no seeds change.

**Why a `notes` field cannot carry measurement status, which is the root cause of all three.** The field ACCUMULATES
rather than supersedes, and nothing ever removes a superseded clause. `blackwell-8`'s note carried
"PROJECTED here (the reference is the ampere-8 laptop, not this exact card)" from 2026-07-28, immediately followed in
the same string by four rounds of on-box `<node-e>` measurement from 2026-08-19 through 2026-08-25.
`blackwell-16`'s note asserted PROVEN, PROJECTED, and "informed by a LIVE REFERENCE, not a runtime measurement" about
the SAME seat. A reader deriving status from these strings gets the wrong answer, and one did.

**`amd-gcn`: "PROJECTED — no GCN box measured" → MEASURED 2026-07-17.** This was wrong for over a month while
this repo's own measurement record sat in `docs/specs/2026-07-17-hardware-scope-linux-and-low-end-design.md`, whose
title is literally the first measurements ever taken on config #12. A collaborator's CachyOS laptop (Ryzen 5 5500U,
Radeon Lucienne / Vega-GCN gfx90c-class, RADV RENOIR, 21 GiB RAM) ran llama-bench at the pinned b9934 Vulkan build with
the byte-identical pinned E2B — pp512 213.31 / tg128 14.86 t/s, quiesced control 208.64 / 14.63, FA on 209.82 /
14.94 — plus six end-to-end tasks through the real CLI at this tier's own invariants (3 s to 59 s, doctor OK, 0 of 6
deferred). Two findings the note now carries because they contradict shipped guidance: flash-attn is
performance-NEUTRAL here, so the FA-off default is free but its stated reason stays untested; and the iGPU buys
**1.26x** on prompt processing, not the README's ~4x — that figure is RDNA3/WMMA guidance and does not carry to Vega
(`matrix cores: none`). Still open, and still in the note: a whisper.cpp Vulkan build for the STT seat.

**`blackwell-8`: the fossil "PROJECTED here" clause marked SUPERSEDED, with the on-box record beside it.** The tier has
had its own reference box since 2026-08-19 — `<node-e>` with an RTX 5060 8GB (sm_120) on a Gen3 x4 PCH
slot — and it is the FIRST box in the fleet where `detect.ps1` resolved a tier and `install.ps1` rendered its config.
Measured there: the 2026-08-22 agent bake in which this tier's DEPLOYED e4b entry scored 0% x2 extraction with zero
tool calls while qwen3.5-9b think-off scored 100% x2 (the reason the tier seats the 9B at 32768), the 2026-08-24 speed
legs (pp512 1638.93, pp2048 1877.80, tg128 49.43 t/s), an 11-leg media roster bake, and 26B cpu_moe soaks. The
install-time WARN "CUDA-13.3 cuBLAS on sm_120 = ~5.6x slower prefill" was FALSIFIED on that box (b9934 == b10603).

**`blackwell-16`: the PROVEN/PROJECTED contradiction resolved into a dated statement.** `<node-b>` ran as this tier on a
single RTX 5060 Ti 16GB from 2026-07-12 to 2026-08-02, with six committed llama-bench receipts, a 4-rung roster
validation, and the capability report already checked in at `docs/tiers/reports/blackwell-16-node-b.md`. The note now
also says what is NOT true: the box never ran `install.ps1`, so this tier's INSTALLER-RENDERED serving params were
never selftested there, and the "3.9 min / 2048" HiDream figure is prose with no timing artifact. It has no CURRENT
reference box — that machine has three cards now.

**`docs/ROADMAP.md` fleet table: three stale rows and one wrong one.** The **`<node-e>` was listed as
`ampere-8`** while every other authority in this repo calls it `blackwell-8`; the `<node-b>` row predated its third GPU
(recorded further down the same page); the `<node-c>` row predated the A2 16GB swap. The table now also states the known
gap it was papering over: a 3x Blackwell rig has no tier id, so `Get-Profile` files it as `dual-gpu` — its own
self-test asserts `'3x blackwell -> dual-gpu'` — which means the fleet's most-measured machine classifies into one
of its least-measured tiers. `blackwell-3x16` is proposed, not shipped.

`docs/tiers/{amd-gcn,blackwell-8,blackwell-16}.md` regenerated by `cmd/gentiers`.

## [0.113.29] — 2026-09-07 — blackwell-2x16's served window follows the measurement: 32768 → 131072

Operator rule, stated plainly: *"install defaults have always needed to be updated after we make successful measurements that give us more
capability."* The tier/model matrix audit found the reverse had been happening — defaults sat where they were first written while the
reference box moved on.

**`blackwell-2x16` `ctx_size` and `agent_ctx_tokens`: 32768 → 131072.** The reference box for this tier IS the live `<node-b>` workstation, and its
llama-swap serves the tier's OWN seeded agent model at four times the seeded window: `qwen3.8-27b --ctx-size 131072` (the tier's
`config_seed.agent_model`), `gemma-4-26b-agent --ctx-size 131072` (the 26B-class resident), and the same 27B weights at `--ctx-size 262144` on
the long-context twin. So 32768 was an un-raised default, not a fit limit — and the tier's own note already said "PROJECTED as a tier; the
reference box is live — promote values to measured as the bake-offs land". The bake-offs landed. `docs/tiers/blackwell-2x16.md` regenerated.

**Deliberately NOT propagated, and the notes say why.** `blackwell-16` / `volta-16` / `dual-gpu` stay at 32768: that 131072 was measured on
`<node-b>`'s TWO-card pool, and nothing has measured a 26B at 131072 on ONE 16 GB card — which is that entire band. `ampere-16` also stays at 32768:
its own 131072 datum comes from the persistent vLLM seat, and `setup/templates/vllm-seat/` only DOCUMENTS that as a manual pattern (a grep of
`setup/*.ps1|*.sh` finds no reference), so a fresh install there still renders the llama.cpp 4B seat that was measured at 32768. Raising a seed
to a window its rendered seat was never measured at would ship a number, not a capability.

No code paths change — this is a serving template default plus its generated tier page.

## [0.113.28] — 2026-09-07 — three filed defects resolved, and a suite that stops measuring the runner

Housekeeping release: no behaviour change an operator asked for, four defects that were FILED rather than fixed now fixed, each with a
mutation-red guard. Three were open issues; the fourth surfaced in this release's own gate pass and belongs to the same class as one of them.

**Fixed — the delegate suite read the MACHINE's GPU lease (#249).** `run.go` consults `LocalLease(cfg.GPULockPath, cfg.StateDir)` on the
placement path, and `gpulease.LeaseDir` falls back to the real machine-wide root when both are empty. `testCfg` left both empty, so
`TestRunSpread*` and `TestRunRetries*` read whatever lease the box happened to hold and went red whenever another session had reserved the
cards — a suite whose verdict depended on what else was running on the desk. `testCfg` now pins `StateDir` and `GPULockPath` under the
test's own temp home. `GPULockPath` specifically, because it is the highest-precedence input and therefore also beats a `GPU_LOCK` set in the
environment, which `StateDir` alone would not. Two guards pin both halves.

**Fixed — fleet-smoke named the LOCAL box as the node under test (#250).** A forced remote route that finds nothing eligible returns a DEFER
carrying the local node and seat — the box that made the decision, not one that ran anything — so the row read `<node-b> / agent-pool / DEFER`
and the base that was actually unreachable went unnamed. (The issue was filed as "the dispatcher's own local row always DEFERs"; there is in
fact no separate local row, it is the remote's row coming back local.) The verdict and the non-zero exit were always correct — an unexercised
node is real fleet signal — so only the label changed: `PlacedResult.Unplaced` is now exported and set on that path, and fleet-smoke drops
the local node/seat for such a row so the table renders the BASE, with a detail that says the node was not exercised and why. The row
classification moved into `smokeRowFor`, testable without a fleet, for the same reason `exitError` was extracted. A node that ANSWERED and
deferred keeps its identity — pinned by its own test, so the fix cannot degrade into "blank the node whenever things look odd".

**Fixed — a capacity-wait assertion measured the runner (#248).** `TestRunCapacityWaitTimesOutAsACapacityDefer` asserted
`capacity_wait_sec >= 0.7` of a configured 1 s wait. Attempts are real HTTP round-trips whose cost scales with runner load, so a loaded runner
ate the idle budget and the test reported 0.50 and went green on the re-run. The bar is now a share of the MEASURED wait rather than of the
configured second. It still catches the regression it exists for: an implementation that CHARGED the wait instead of crediting it reports ~0.

**Fixed — `TestHistoryIsBounded` had the same disease, and never tested boundedness.** It slept 200 ms at a 5 ms interval and asserted
`len == 3` exactly, which asserted both that the ring is capped and that a loaded runner completed three polls in a fixed window; only the
first is about the poller, and it flaked under the CPU contention of a full `go test ./...`. Worse, a single sample taken while the ring was
still filling passes without the cap ever being exercised. It now waits for the ring to fill (deadline-bounded), then keeps polling well past
the cap and re-checks — which is the actual property.

## [0.113.27] — 2026-09-07 — the fleet can see a busy card: nine scheduling defects found by audit and fixed

The operator's question — "queueing, scheduling and multi-node exist, so why would a long GPU job collide with anything?" — was right, and a
20-agent read-plus-refute audit of the scheduling surfaces answered it: **the machinery was there; a long GPU job had no way to tell it.**
Node availability was expressed in execution SLOTS only, with no DURATION and no GPU-occupancy dimension, so nothing on the wire separated a
slot that frees in 900 ms from one held for six hours. Nine defects, all fixed here, each with a mutation-red guard.

**Fixed — a long lease of ANY class now refuses (#254).** `/fleet/health`'s lease block gains `remaining_sec` and `busy`, the node's OWN
verdict that its card is spoken for long enough to place elsewhere, decided by the new `fleet_busy_lease_sec` (default 120; negative disables
the rule and restores the text-only behaviour). The verdict travels rather than the threshold, so a delegator never needs a remote box's
config and a node one release behind simply omits both fields, decoding to false — exactly the previous behaviour. It is a DURATION rule, not
a class rule, because that is the real distinction: a 20-second render must not refuse, a six-hour reservation must. Before this, only a TEXT
lease refused, so the harness's LONGEST job class — anything holding the media lease — left the node advertising idle slots while its card was
gone, and placement routed work TOWARD it. The delegator carries the verdict as `NodeView.LeaseBusy`, `remoteEligible` excludes it exactly as
it excludes a text lease, `leasedLanes` names it to the operator ("long GPU lease held"), and `offload_status` reports `gpu_lease_busy`.

**Fixed — `refusing` and `idle_slot` can no longer contradict each other.** They were computed independently, so a draining or leased node
still advertised a free slot and sheddable work was dealt to it. `idle_slot` now means what it says: a job handed to me would start now.

**Fixed — the saturation score is computed from the CAPPED running set (#256).** `max_concurrent_jobs` caps only the capped set, which is what
`IdleSlot` compares against, so feeding the score the all-jobs count let a node publish `score 1.0` and `idle_slot true` in one payload
whenever an uncapped job was executing. The depth term still counts every admitted job, because `max_queue_depth` bounds them all.

**Fixed — `animate` is exempt from the concurrency cap (#256).** It runs a ComfyUI render through gpugen under the media lease exactly as
`image-gen` and `video-gen` do, never touching the shared text endpoint the cap protects. Capping it made it hold a fleet execution slot while
parked in the capacity-1 media slot — verbatim the failure the exemption exists to prevent.

**Fixed — the pull queue's claim loop admits with a real spec (#255, security-shaped).** It called `Jobs.Accept`, an empty `AcceptSpec`, while
the push path filled one. `Agent` unset took a PULLED agent job out of `handleJob`'s bearer gate, so its state and RESULT were readable without
the token while the identical contract arriving by dispatch was gated, and the agent-row error redaction was skipped too; `Uncapped` unset made
a pulled render burn a concurrency slot; `OnDropped` unset left drain's never-started arm unable to clean up. The spec now lives in
`claimSpec`, a function, so it is testable without a live holder, and a source guard keeps both bare shapes out of production files.

**Fixed — a REFUSED re-claim releases its own materialization.** `Admit` deliberately does not fire `OnDropped` on a refusal, and the
already-known job's cleanup closes over the ORIGINAL directory, so the second build's job dir was stranded under `pipeline-jobs/` until the
next fleet-serve start swept it.

**Fixed — `gpu reserve --detach` requires an explicit `--for` (#257).** A detached holder exits at that deadline and releases whether or not
the work has finished, so the 45-minute default silently freed the card mid-job; the next render then claimed it and unloaded the seat on top
of the running work, leaving the node advertising a seat that was not there. The wrapper form ties the hold to a process and is unaffected.

**Fixed — the vision lane stops polling a reservation that cannot free (#257).** `gpulock.WaitFree` gained the far-expiry short-circuit
`gpulease.Acquire` already had, and `Info` now carries the holder's declared `ExpiresAt`. A long TEXT reservation used to cost every
`vqa`/`ocr`/`assess_image`/`video_describe` call its full `vision_gpu_wait_sec` (90 s) for the reservation's whole life. MEDIA leases are
deliberately excluded, for the reason `Acquire` gives: a media expiry is a timeout CEILING, not a promise, and a video job that declares 25
minutes routinely finishes in three.

## [0.113.26] — 2026-09-07 — the seat rigger, first slice (`local-offload rig`, MCP `agent_rig`) and a note on failed trace steps

**Added — the rigger's classifier (`internal/rig`, ADR 0036 amendment P3a).** `local-offload rig --seat <alias> [--since 7d]
[--node ID] [--out report.json] [--verdicts rows.json] [--json]` and MCP `agent_rig {seat, since?, node?, markdown?}` read
this box's delegation-log corpus and put every failed or deferred row of the seat on exactly ONE failure axis, in a
PUBLISHED precedence order that the report prints: `seat-infra` (a defer class of infrastructure/config/contract, or a
placement failure with no defer class at all — a node's 503 shed, a refused dispatch) `→ timeout → budget → abstention`
(except an "output failed schema" reason, which is `schema-miss / invalid`) `→ schema-miss` (min_items/nonempty misses;
sub-axes `two-step-grounded`: context docs and ≤ 2 steps, `invalid`) `→ anchor-miss` (contains/not_contains/regex misses —
a valid answer that missed the document; same sub-axis) `→ loop → long-observation → tool-misuse → unclassified`, the last
three on trace-bearing rows only. Acceptance measured: a fresh-context reader applying the rules as written to a stratified
41-row sample of real corpus rows agreed with the classifier on 41/41; the three rule extensions above are what that pass
found uncovered (rows that fell to `unclassified` or read as `abstention`), re-labelled blind after the change. The report gives, per axis, hits over the rows ELIGIBLE for that axis (trace axes count only
rows with a trace — a 30 % weight over 11 traced rows and a 3 % weight over 649 rows are different claims), up to five
evidence job ids with the deciding fact, the most repeated reason text for seat-infra/timeout/unclassified, and the
pre-authored remedy where the harness's closed vocabulary has a lever (`agent_seed_context_reads`,
`agent_env_rules.max_observation_tokens` / `max_calls_per_tool` / `rewrite_error`, with the value derivation stated in
words) or the honest "not a rule matter" where it has none. The rigger PROPOSES NOTHING on its own and applies nothing —
adoption stays a measured A/B on the seat. Deterministic for a fixed corpus; an unknown seat fails naming the seats seen;
a clean corpus reports zero hits everywhere. Why this and not the full observe/diagnose/write/validate rigger: the design
council read the corpus of the day (1,187 rows, 649 bad) — ~70 % were wall timeouts and seat errors no rule shapes, only
16 rows carried a trace, and a digest-8 gate cannot fail for axes it never exercises — so a proposer built on an
unvalidated diagnosis would inherit every ambiguity. P3b (proposer + validation gate + difficulty-zone / red-team
objectives) waits for a trace corpus in the hundreds. Acceptance for this slice: blind-labelled agreement on a stratified
sample of real rows (an independent reader applying the published rules), reported in the ledger with the ship.

**Added — `trace[].note`.** For a call that did NOT commit (failed, none, unknown) the trace step now carries the first
160 bytes of what the model was told — the tool's error, the breaker's refusal, the rule's reason — one line, never a
page; empty on committed calls. Without it a `rewrite_error` rule could not be authored from the corpus (council finding).

## [0.113.25] — 2026-09-07 — `tts_endpoint`: generate_audio voice through an OpenAI-compatible speech server

**Added — the `tts_endpoint` lane (`internal/ttsclient`, `pipeline/ttsendpoint.go`).** Config `tts_endpoint` (base URL, no
`/v1`), `tts_model` (default `tts-1` — the OpenAI alias every compatible server maps to its default engine; VoiceStudio
0.5.1 rejects `default` as a model, measured), `tts_voice`, `tts_api_key`. `generate_audio kind=voice` renders through the
server when the request says `voice: endpoint`, or by default on a box with `tts_endpoint` and no `voicegen_script`; a
box with both keeps Chatterbox as its default voice, byte-for-byte as before. `POST /v1/audio/speech`
(`response_format: wav`, `language` hint), the body written atomically to the media dir (or `out`), an HTTP error / a
JSON-or-HTML 200 / a body under 64 bytes is a defer naming the server's own words (`err_class tts_endpoint`), never an
empty "audio" file. No media lease: the server owns its GPU, the rule for every remote engine. Result carries
`engine: tts_endpoint`, `endpoint`, `model`, `voice`, `bytes`; `meta.model` = `tts-endpoint:<model>`. Doors: MCP
`offload_generate_audio` (`voice: endpoint`, `tts_voice`), CLI `generate-audio --voice endpoint [--tts-voice NAME]`; the
route shows in `offload_status.media.routes` as `generate_audio:voice:endpoint` (a URL binding — configured means the key
is set; liveness is the server's `/health`). Why: VoiceStudio (item 8 of the 2026-09-07 order) was proven live headless on
the reference workstation the same day — a render in 80 s at −18.9 LUFS with an exact round-trip transcription — and the
harness needed a way to render through it, or through any server that speaks that contract, without a python worker.

## [0.113.24] — 2026-09-07 — setup replay on the agent loop (`setup_actions`, `agent_seed_context_reads`)

**Added — `setup_actions` (ADR 0036 amendment, P2 of the envharness port).** A contract may carry up to eight
`{tool, args}` calls the loop REPLAYS before the model's first turn (`agent/setup.go`, `core.AgentSetupAction`):
each runs through the seat's env rules (deny/allow withhold, `arg_limits` clamp; the `max_calls_per_tool`
counter is not spent), dispatch (no circuit breaker consulted or fed — the model never issued these calls),
the observation hooks and the loop-boundary cap, and lands in the transcript as one assistant turn carrying
the calls plus one tool result each — the shape the seat produces itself, never a user message holding the
document (to a small seat a user turn is a question). Charged to the wall, never to `max_steps`; pinned for
compaction but outside the protected preamble; bounded to half the compaction budget (the rest are recorded
not-run and the model reads for itself); a failing action is its error observation, never an abort. Doors:
`local-agent --setup <file>`, MCP `agent_run` / `agent_delegate` `setup_actions`, the CLI `delegate` contract
file, the fleet wire. Telemetry: Step-0 effect/trace entries with `setup: true`; `setup_ran` (executed count)
on the wire result and `agent_run`. Validated by name at every door (`core.ErrAgentSetupActions`); a node
one release behind ignores the field and reports no `setup_ran`.

**Added — `agent_seed_context_reads` (node config, default off).** The node prepends one `read_file` per
context doc to the replay — it knows the file names it just materialized; a delegator would be guessing. The
seeded reads and the contract's own actions are one list per run, clamped to eight (seeded first; review
finding: without the clamp a node could replay 16).
Why (measured on the 0.113.23 trace): the delegation-log corpus holds 117 failed or deferred 4B-seat rows in
six days, every one grounded on context docs, 89 of them stopping at exactly `list_dir` then `read_file` —
the two calls it takes to find the document. Acceptance is the corpus's own failed contracts re-run on the
same seat with and without seeding, pass rate first, step count second; the key ships off until that A/B
says otherwise. `examples/agent-setup-actions.json` is the starter.

**Template (no code): `setup/templates/vllm-seat/seat_fg.sh` can pin the KV pool from FREE memory.** `SEAT_KV_HEADROOM_GIB`
(off unless set) + `SEAT_NONKV_GIB` / `SEAT_KV_FLOOR_GIB` / `SEAT_KV_CAP_GIB`: the pool = min free on the seat devices at launch −
non-KV − headroom, floored/capped, passed as `--kv-cache-memory-bytes` (per worker) so `--gpu-memory-utilization` no longer
budgets a fraction of a card that co-residents also live on. Why: the reference workstation's util-0.90 seat fit at launch and
stalled at noon when the utility seats sharing its card grew 0.7–1.3 GB (2026-09-06); the profiler also lands the same config
at 183k or 191k tokens on different starts (a c32 coin flip). The launcher prints every input of the computation. Item 3 of the
2026-09-06 operator order; measured with the production seat gate (`seat_gate.py`) before it was switched on: with the
utility seats moved to CPU and `SEAT_KV_HEADROOM_GIB=0.5`, 10/10 starts landed the same 209,597-token pool (+9 % over the
profiled 191,884, loads 139–163 s), probe c8 136.4 tok/s (TTFT p95 2.8 s) / c32 218.9 tok/s (TTFT p95 11.1 s, 0 failed),
soak 1,208 s = 1,624 requests, 0 errors, 0 health failures — STABLE, adopted as the reference workstation's seat config.

## [0.113.23] — 2026-09-07 — the `agent_env_rules` review fixes that 0.113.22 was meant to carry

**Fixed — 0.113.22 shipped the PRE-review draft of `agent_env_rules`.** The commit that became 0.113.22 was cut from a stale
index (the `git add` in the ship command never ran; the staged first draft was committed, tested by CI, merged and deployed
to two nodes), while the eight fresh-context review fixes sat unstaged in the worktree — where the mutation guards had been
measured. This release is that reviewed code, byte-for-byte the state the guards were red on: (1) a capped tool is withheld
STRUCTURALLY after the N+1th execution (removed from the offered specs via the loop's disabled set, not just refused);
(2) an `arg_limits` clamp is surfaced in the observation the model reads, so the model knows the number it asked for was
not the number that ran; (3) `arg_limits` caps are integral (a fractional cap is a validation error, not a silent
truncation); (4) `max_observation_tokens` has a 64-token floor (below it the elision marker would be most of the
observation); (5) the observation hooks — strip, cap, `rewrite_error` — touch TOOL OUTPUT only: a breaker refusal, a block
reason or an `unknown tool` line is never rewritten; (6) rule names with leading/trailing whitespace are rejected at
`Validate` instead of silently never matching; (7) every `Validate` failure wraps `core.ErrAgentEnvRules`, so the three doors
classify a bad table by identity, not by string; (8) `examples/agent-env-rules.json` round-trips through the strict decoder
(an unknown key in the starter file was a deploy-time exit 2). Plus a nil-interface compile fix on the MCP door. Nodes on
0.113.22 with no `agent_env_rules` table behave identically to 0.113.21 (the table is off by default); the difference is
only what a configured table does. The ship command now proves the staging before the commit (`git diff --cached --stat`)
and the emptiness of the worktree after it (`git diff HEAD --stat`).

## [0.113.22] — 2026-09-07 — environment rules on the agent loop (`agent_env_rules`) and a per-step trace in every agent result

**Added — `agent_env_rules` (ADR 0036).** The coding-agent loop runs every tool call through three interceptors in
envharness's order — filter_action → modify_transition → filter_observation — driven by a CLOSED, validated vocabulary
in config, a property of the seat: `deny_tools` / `allow_tools` (withheld structurally, like a profile), `max_calls_per_tool`
(the N+1th EXECUTION is blocked with a reason the model reads and the tool is withheld from the offered specs for the rest
of the run; a breaker refusal spends no execution), `arg_limits` (whole-number caps on numeric arguments per tool, the call
still runs and the result names the clamp), `max_observation_tokens` (≥ 64; head+tail with the elision marker, before the
loop-boundary cap), `observation_strip` (regexps removed from every tool result), `rewrite_error` (the result of a tool that
ran and errored, matching a pattern, becomes a short line the model can act on). The observation hooks touch tool output
only — a breaker refusal, a block reason or an `unknown tool` line is never rewritten. A bad table fails by name at every door — `local-agent` exit 2, MCP
`agent_run` defer, fleet `config`-class defer — never a silent no-op; a nil/zero table is byte-identical to the previous
loop. `local-agent --env-rules <file>` replaces the table for one run (`off` = none); `examples/agent-env-rules.json` is
the starter. This is the P1 slice of the 2026-09-07 envharness/axolotl integration order; envharness's LLM-written rule
code is deliberately NOT ported (generated code in-process is out of policy — rules are data here).

**Added — the step trace.** Every agent wire result (fleet `agent_run`, so every delegation-log corpus row) and every MCP
`agent_run` response carries `trace`: per tool call the tool, its effect status, the bytes the model actually read
(`obs_chars`) and the environment rule that decided (`rule`), plus `rules_fired` and, on the MCP door, `env_rules` (the
seat's table summary). Set before the defer branches, like the prefill accounting. Why: the corpus recorded contracts and
results but not steps — 51 of the 58 failed 4B rows of the last six days stop at exactly two steps with empty schema
fields and 24 of the 27B pool's defers are "step budget exhausted", and nothing on disk said what those steps did. The
rigger (P3) diagnoses on this trace. `EffectRecord` gains `obs_chars` / `rule` for the same reason.

## [0.113.21] — 2026-09-07 — the result cache falls back to a per-process file when the shared one is held

**Fixed — every session but one ran cache-less.** The result cache (`cache_path`, bbolt) is single-writer with an exclusive
file lock, and a workstation runs several `local-offload` processes at once (one MCP server per Claude session, the CLI, the
fleet node): measured 2026-09-07, six processes, one holding `cache.db`, five reporting `result_cache: not opened (held by
another process)` in `offload_status` — `agent_run`'s in-loop offloads re-ran the model on repeated identical input in
five of six sessions, silently. `cache.OpenPreferred` now falls back to a per-process sibling (`cache.p<pid>.db` beside
the configured file) when the lock times out, sweeps siblings older than 12 h on the way (pids are reused, so age is the
rule, not liveness), and both the CLI and `local-agent` say which file they use. `offload_status.reuse.result_cache`
reports `path` (the file actually opened), `configured`, and `fallback`. A non-lock failure (bad path, permissions) still
surfaces as the error it is. Item 5 ("harness result cache") of the 2026-09-06 operator order; the correctness gate is the
existing single-writer + round-trip tests plus the new fallback/sweep/non-lock tests.
Review fixes before merge: the sweep is anchored to the exact `<stem>.p<pid><ext>` shape (an operator's `cache.prev.db` beside the
cache is never touched — tested), it runs on every open so siblings left by an earlier busy window do not accumulate once
contention ends, and the status text for an unopened cache now says what it means after this change (both files failed, or
no path configured) instead of the old benign "held by another process".
