# MCP server

## Purpose

The Model Context Protocol surface — how a calling agent (Claude Code and equivalents) reaches the
harness. It is the primary consumer-facing interface: most usage arrives here rather than through the
CLI.

## Questions this doc answers

- Which tools exist, and what do they group into?
- What keeps the advertised tool list honest?
- Why does a newly built binary not show new tools?
- Which tool can reach the network?

## Scope

Tool registration, the tool inventory, the stdio transport, the manifest and its drift test, and the
operational lifecycle of the server as an MCP client sees it.

## Non-scope

- What the tools actually do → [offload-pipeline.md](offload-pipeline.md),
  [media-generation.md](media-generation.md), [coding-agent.md](coding-agent.md)
- The CLI surface over the same capabilities → `local-offload` with no arguments prints usage

## Key concepts

**Tool** — one named, schema-described capability offered to the calling agent. **Manifest** — the
declared inventory in `.printing-press.json`, checked against what the code actually registers.

## How the system works

The server runs over **stdio** and registers its tools at startup. A calling agent discovers them,
calls them with JSON arguments, and receives JSON results — including Defers, which are successful
results, not errors.

**Twenty-one tools** are registered, in families:

| Family | Tools |
|---|---|
| Text offload | `offload_summarize`, `offload_classify`, `offload_extract`, `offload_triage` |
| Vision | `offload_vqa`, `offload_assess_image`, `offload_extract_image`, `offload_video_describe` |
| Speech / OCR | `offload_transcribe`, `offload_ocr` |
| Media generation | `offload_generate_image`, `offload_generate_video`, `offload_generate_audio`, `offload_generate_svg` |
| Media editing | `offload_edit_image`, `offload_inpaint_image`, `offload_media` |
| Graph execution | `offload_run_graph` |
| Agent | `agent_run` |
| Remote (opt-in) | `offload_nim` |
| Status | `offload_status` |

`offload_nim` is the **only** tool that reaches a remote service. It is an explicit, caller-invoked
side channel and is not part of the Cascade — nothing escalates or falls back into it. See
[ADR 0001](../architecture/decisions/0001-defer-never-cloud-fallback.md).

## Important flows

Every tool ultimately enters the Cascade or a media backend — see
[../flows/cascade-escalation-and-defer.md](../flows/cascade-escalation-and-defer.md) and
[../flows/run-graph-manifest-satisfaction.md](../flows/run-graph-manifest-satisfaction.md).

## Data and state

The server is stateless between calls. State lives where the underlying system keeps it — the ledger,
the audit trail, footprints.

## Interfaces and entry points

- The MCP entry in `main.go`'s subcommand dispatch; tools registered in `internal/mcpserver/`.
- `.printing-press.json` declares the manifest: `api_name`, `version`, `module`, and the MCP
  transport plus tool list.

## Dependencies

`internal/pipeline`, `internal/agent`, `internal/rungraph`, `internal/nimclient` (the one remote
tool).

## Downstream effects

This is a published interface. Renaming or removing a tool breaks every configured client, and
changing a tool's argument schema breaks callers silently — the calling model simply starts getting
errors it will try to work around.

## Invariants and assumptions

1. **The manifest and the registered tools must agree.** A drift test enforces it, so adding a tool
   without updating `.printing-press.json` fails the build. Currently 21 registered, 21 declared, both
   at version 0.22.1. This test arrived via an outside contribution after the manifest had silently
   drifted to claiming four tools.
2. A Defer is a successful result. Do not map it to an MCP error.
3. `offload_nim` is the only remote surface, and it is opt-in.

## Error handling

Tool errors return as errors; Defers return as results with `deferred: true`. The distinction matters
to the calling agent, which should retry neither — it should do the work itself on a Defer, and
diagnose on an error.

## Security and privacy notes

The stdio transport inherits the trust of whoever launched the process. `agent_run` exposes the
coding agent, and therefore its capability flags — the defaults there are what keep this surface
read-only unless deliberately widened. See
[ADR 0003](../architecture/decisions/0003-policy-broker-and-capability-flags-off-by-default.md).

## Observability and debugging

- `offload_status` reports harness state to the calling agent.
- `local-offload doctor` checks the serving layer the tools depend on.
- **The most common operational surprise:** an MCP client holds its server process for the session,
  so a rebuilt binary is not picked up until the client restarts. Newly added tools appearing absent
  almost always means a stale server process, not a registration bug.

## Testing notes

`internal/mcpserver/` covers tool registration and argument validation (`badargs_test.go`);
`agentrun_e2e_test.go` exercises the agent tool end to end. The manifest drift test
(`TestPrintingPressManifestListsEveryTool`) lives in `main_test.go` at the repo root, since the
manifest is a repo-root file.

## Common pitfalls

- Adding a tool and forgetting the manifest — the drift test catches it, which is the point.
- Expecting a Defer to be an error.
- Debugging "missing tools" without restarting the MCP client first.
- Assuming every tool is local: `offload_nim` is not.

## Source map

- [`internal/mcpserver/mcpserver.go`](../../internal/mcpserver/mcpserver.go) — registration and
  handlers
- [`.printing-press.json`](../../.printing-press.json) — the declared manifest
- [`main_test.go`](../../main_test.go) — `TestPrintingPressManifestListsEveryTool` (the drift test)
- [`main.go`](../../main.go) — subcommand dispatch and MCP entry

## Related docs

- [../architecture/decisions/0001-defer-never-cloud-fallback.md](../architecture/decisions/0001-defer-never-cloud-fallback.md)
- [../OPERATOR-GUIDE.md](../OPERATOR-GUIDE.md)
- [../../README.md](../../README.md) — full CLI and MCP tool tables
