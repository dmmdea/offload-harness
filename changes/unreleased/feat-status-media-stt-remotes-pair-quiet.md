### Added - fleet node rows say what media a node renders; every spill lane can pin a node; the PAIR ingress is checked

Three changes from a media session's feedback.

A caller concluded a GPU node had "no image or video engine" because the `offload_status` fleet row showed only `served_models`, which
lists llama-swap chat seats. The row now carries a `media` block built from the node's own `/fleet/health`: its media task types, the
image, video, animate and audio families, the state of its edit route, the configured and not configured route names, and its loadable
model families (`loadable_model_families` and `image_families` are now decoded). The block is absent for a node that published nothing
media-shaped, so an older node or a failed probe never reads as "no media". The tool description says so.

`offload_transcribe` and the CLI `transcribe` verb (repeatable `--remote`) take `remotes`, as `offload_generate_image` does, and so do the other
single-shot spill lanes that had a `route` and no `remotes`: `offload_vqa`, `offload_ocr`, `offload_assess_image`, `offload_classify`,
`offload_extract`, `offload_forecast` and `offload_compose_video`. The bases narrow the fleet for that call and must each be in
`delegate_remotes`; one outside it is a contract defer naming it. One helper (`rosterprobe.Narrow`) serves every lane, `mediaremote` included; a node that the caller-scope policy (`delegate_remote_scopes`) removed is refused with that policy and the caller's scope rather than as "not in delegate_remotes" (`rosterprobe.NarrowFor`).

### Fixed - the PAIR ingress warning no longer prints on every short-lived command

`local-offload gpu reserve` and the other one-shot commands printed `pairworkloads: PAIR ingress unreachable` every time, because the
warning was once per process and each command is a new process. Those emitters now warn at most once per six hours per state directory
(a marker file under the state root; any marker error means the warning prints). The MCP server, fleet node and delegate runner keep the
once-per-process warning. `doctor` gains a `pair ingress:` row (a 1 s TCP dial, OK or WARN with the fix) and the `offload_status` `pair` block
gains `ingress_reachable`, so the condition stays visible.

Check: built the binary and ran it against a config with PAIR enabled and nothing listening: the first `gpu reserve` printed the warning, the
second did not, `doctor` printed the WARN row and, with a listener up, the OK row; `offload_status` against a stub node showed the `media` block;
`transcribe --remote` with a base outside `delegate_remotes` returned the contract defer. Details: `docs/systems/mcp-server.md` and
`docs/systems/pair-workloads.md`.
