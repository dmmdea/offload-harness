# Delegation client

## Purpose

A delegation client is a machine that runs the harness only to place work on the fleet. It has no
local model, no media lane and no fleet service of its own: a 2-core laptop with no room for a model
or a Chrome worker beside its desktop, or an arm64 single-board computer, which has no
chrome-headless-shell build for the composition lane. It is a role, not a hardware tier, so it is
installed by its own mode rather than by a profile.

## What it can do

| work | how it reaches the fleet |
|---|---|
| agent contracts (read, reason, write a diff) | `agent_delegate`. This box has no agent seat, so it is never a placement: the default `auto` route and `spread` send every subtask to `delegate_remotes`, ranked as on any delegator |
| web research | `offload_research`: pages are fetched here and digested by contracts on the fleet's seats |
| a vetted template rendered to video | `offload_compose_video` with the default `route` auto: this box has no lane, so the template goes through a node's template door (`compose-video` over `/fleet/dispatch`) and the video and snapshots come back into this box's media dir |
| a whole HyperFrames project or an inline composition | the same call with `project_dir` or `html`: packed into a bundle, checked here, sent to a node's token-gated project door ([ADR 0071](../architecture/decisions/0071-a-fleet-token-holder-may-send-a-whole-composition-project-to-render.md)); the video and snapshots come back |
| an image, a clip, a character animation, a voice or music clip, a ComfyUI graph | `offload_generate_image`, `offload_generate_video`, `offload_animate_character`, `offload_generate_audio`, `offload_run_graph` with the default `route` auto: this box has no lane, so the job goes to a node from `delegate_remotes`; a still, a reference and driver or a clone sample travels in a hash-checked bundle to the node's media-job door ([ADR 0072](../architecture/decisions/0072-a-fleet-token-holder-may-send-one-media-job-with-its-input-files.md)), and the output comes back verified against the sha256 the node published |
| questions about one image (VQA, OCR, assessment) | `offload_vqa`, `offload_ocr`, `offload_assess_image` with `route: "remote"`: the image is sent to a node that advertises the vision lane. Their `auto` route leaves the box only while a local card is busy, so a client names `remote` |
| classify and extract | `offload_classify`, `offload_extract` with `route: "remote"`, on a node that advertises the text lane |

What stays out of reach:

- The other cascade text tools (`offload_summarize`, `offload_triage`, `offload_ask`, ...) run on a
  local model only and defer here. Text work goes through `agent_delegate`.
- Transcription. A node's `stt` task reads an audio file by a path on that node, and no tool on this box places
  `offload_transcribe` on the fleet, so it defers here. A video that needs word-timed captions gets its
  transcript made on a machine with that lane, or it arrives with the project; the render itself still goes
  through the project door. Voice synthesis is placeable (`offload_generate_audio`, above).
- `offload_video_watch` and the browse lane are local only and defer here.
- The HyperFrames Studio preview is not part of the harness on any machine.

## Install

Linux:

```
install.sh --client --remotes http://<node-a>:18811,http://<node-b>:18811 \
  --token-file <file holding the fleet token> [--prefix <dir>]
```

It installs the binary, renders the config with `local-offload install client`, registers the
`local-offload` MCP server for Claude Code when `claude` is on PATH and it is not registered yet, and
runs `acceptance`. There is no detect, no llama.cpp, no model and no service. A machine whose only
volume is the OS volume needs an explicit `--prefix`.

Windows (no installer mode yet): place the binary, then run the same two steps by hand:

```
local-offload install client --home <dir> --remotes <nodes> --token-file <file>
claude mcp add local-offload --scope user -- <dir>\bin\local-offload.exe mcp --config <dir>\etc\config.json
```

`install client` writes `<home>/etc/config.json` (mode 0600; it holds the token, which is never printed):
`delegate_remotes`, `fleet_auth_token`, `agent_delegation_enabled`, the media, state, cache and ledger
paths under the home, and every model route and every script binding that has a default written empty,
so `doctor`, `offload_status` and `acceptance` never claim a lane the box does not have. `ffmpeg_path` keeps
its default only when ffmpeg and ffprobe are on PATH (`offload_media` and the kit's cut scripts run them
locally). It refuses to replace an existing config without `--force`, and refuses remotes that are not
fleet node bases (off the fleet port, loopback, a `/v1` suffix), removing what it wrote.

## The render nodes it uses

A node serves templates to a client as soon as its composition lane is bound. It serves whole projects
only when its operator opens the project door: `fleet_compose_projects: true` in its config, with the
same `fleet_auth_token` the client holds. `/fleet/health` lists `compose-project` when the door is open.
The client picks, among its `delegate_remotes` that advertise the task, the one with the shortest queue.

Media jobs (image, video, animation, voice, graph) are served by any node whose route for the task is CONFIGURED: a node
lists `video-gen`, `animate`, `audio-gen` and `run-graph` in `/fleet/health` only while the weights and nodes its graph
loads are on disk, and `media_routes` says which route is missing and why. A job that carries a file needs the node to
open the media-job door too: `fleet_media_inputs: true` in its config, with the same `fleet_auth_token` the client holds
(`/fleet/health` lists `media-job` when it is open). Among the eligible nodes the client prefers one holding no lease,
then the shorter queue, and names every node it skipped when none can take the job.

## Checking it

- `local-offload acceptance --config <home>/etc/config.json`: READY, with the model-alias check SKIPped
  (no local model).
- `local-offload doctor --config ...`: the endpoint health line reads SKIP, and the fleet-versions
  section lists each delegate remote's harness version.
- One contract: `local-offload delegate --config ... --contract c.json` lands on a fleet node; the result
  names it in `node` and `placement`, never the local seat.
- One render: `local-offload compose-video --config ... --template title-card --snapshots 2.5 --json`
  returns `video_path` in this box's media dir and `meta.node` naming the node that rendered it.
- One media job: `local-offload generate-image --config ... --route remote --json "a red door"` returns `image_path` in
  this box's media dir, `meta.node` and `meta.placement` (`remote: forced`) naming where it ran.

## Source map

| path | role |
|---|---|
| `install_client.go` | `install client`: the config it writes, `hasLocalModel` |
| `setup/install.sh`, `setup/install.tests.sh` | `--client`, `--remotes`, `--token-file`, and their dry-run tests |
| `acceptance_cmd.go`, `main.go` (`doctorRun`) | the alias check and the endpoint health line that SKIP with no local model |
| `internal/delegate/run.go` (`localServesLayer`, `seatless`) | a view that names no agent seat is never a placement |
| `internal/composeremote/` | `offload_compose_video`'s `route`: node choice, the template and project doors, fetching the outputs back |
| `internal/composebundle/` | packing a project, and the extract and reference checks both sides run |
| `internal/fleetnode/compose_project.go` | the node's project door (ADR 0071) |
