# Delegation contract file

## Purpose

`local-offload delegate --contract file.json` places delegation contracts from a shell or a script. The
file it reads is NOT the argument shape of the MCP `agent_delegate` tool, and the difference has cost
launches: a file written in the tool's shape does not fail to parse, it parses as one empty subtask and is
refused at intake. This page is the one place that states the file shape, what the CLI takes from flags
instead, and the one config key (`agent_model`) that decides whether a well-formed contract has a seat to
run on.

## Questions this doc answers

- What does the JSON in a `--contract` file look like, and how many subtasks may it hold?
- Why does a file shaped like the MCP call come back with "goal is required"?
- Which fields are read, and what happens to a field this build does not know?
- Where do `route`, `read_root`, `pin_reason`, `priority` and the remote list go on the CLI?
- Why does a run defer as `infrastructure` when the contract itself is fine?

## Scope

The file format of `delegate --contract`, the flags beside it, and the seat id rule that its runs depend on.

## Non-scope

What a contract field means and how placement works are in [mcp-server.md](mcp-server.md) (the
`agent_delegate` tool), [coding-agent.md](coding-agent.md) and [fleet-node.md](fleet-node.md). The
acceptance DSL is documented with the tool.

## Key concepts

**Subtask object.** One contract: the same JSON object the MCP tool takes for each entry of its
`subtasks` array, plus `context_paths`, a delegator-side extension (files the delegator inlines under
`--read-root` so the caller's context does not pay for them).

**Contract file.** A `--contract` file holds either one bare subtask object or an array of subtask
objects. Nothing else wraps them.

## How the system works

### The file

A bare object (one subtask):

```json
{
  "goal": "List every ADR number mentioned in the attached page.",
  "context_paths": ["docs/architecture/decisions/README.md"],
  "output_schema": {"properties": {"numbers": {"type": "array", "items": {"type": "string"}}}},
  "acceptance": ["min_items:numbers:1"],
  "max_steps": 6,
  "timeout_sec": 300
}
```

An array (several subtasks, one per element, at most 8):

```json
[
  {"goal": "Summarise the first page.", "context_paths": ["a.md"], "output_schema": {"properties": {"summary": {"type": "string"}}}},
  {"goal": "Summarise the second page.", "context_paths": ["b.md"], "output_schema": {"properties": {"summary": {"type": "string"}}}}
]
```

The rules, each held by a test:

- The file is a bare object or an array. A file that starts with `[` is read as an array; anything else
  is read as one object.
- An array holds at most 8 subtasks. A ninth is refused for its number before anything is prepared, with
  the same words as a call of nine good ones, so a call of nine with one bad subtask is refused for its
  size and not for the bad one.
- The decode is tolerant: unknown fields are ignored, so a file written for a newer harness still parses
  on an older one. The same tolerance is what makes the next point a trap.
- **The MCP wrapper is not unwrapped.** A file shaped `{"subtasks": [{"goal": ...}]}` parses without
  error as one subtask whose only content is an unknown `subtasks` field, so its `goal` is blank and intake
  refuses it with `agent contract: goal is required`. Write the bare object or the array.
- `schema_version` and `depth` are minted at intake; a value in the file is overwritten, never trusted.
  `max_steps` and `timeout_sec` are clamped to the same ceilings the wire applies.

### The fields

`delegate --help` lists the eight common fields; this page is held to that list by a test.

| field | what it is |
|---|---|
| `goal` | required; the self-contained task (the sub-agent sees only this and the context) |
| `context` | inline documents, `[{"name": "a.md", "text": "..."}]`, within the box's context cap |
| `context_paths` | files the delegator inlines under `--read-root` (at most 128 KiB each) |
| `output_schema` | a JSON Schema with a `properties` map; required for any placement on a fleet node |
| `acceptance` | the machine-checkable checks the delegator evaluates before a result counts as done |
| `profile` | the agent task profile; omit it to take the executing box's configured profile |
| `max_steps` | the loop step budget |
| `timeout_sec` | the execution budget per subtask |

The object decodes into the same contract struct the MCP tool's subtasks use, so the other contract
fields (`thinking`, `setup_actions`, `write_root`, `allow_browse` with `browse_hosts`, `context_class`,
`layer`) are read from a file too; each means what it means on the tool.

### What comes from flags

The file carries only subtasks. The call-level inputs of the MCP tool are flags:

| MCP argument | CLI flag |
|---|---|
| `route` | `--route auto\|spread\|local\|remote` (default `auto`) |
| `pin_reason` | `--pin-reason` |
| `read_root` | `--read-root` (default: the current directory) |
| `remotes` | `--remote http://node:18811` (repeatable; default: the config's `delegate_remotes`) |
| `priority` | `--priority -1\|0\|1` |
| (none) | `--tenant`, the id the fleet round-robins across |

The verb needs `agent_delegation_enabled` in the config. It exits non-zero when a subtask failed or came
back deferred as `infrastructure`; a deferral of any other class is a success shape and exits 0, because the
result JSON says what happened.

## The seat id rule

A contract names no model. The model is the executing box's config key `agent_model`, and the harness sends
it as the model id of every completion. The endpoint (llama-swap) serves only the ids its `/v1/models`
lists, so `agent_model` must equal an id that list publishes (a model name or one of its aliases).

Register D-70 recorded what a wrong id costs: three failed launches on 2026-08-27, each with a well-formed
contract. The run does not defer as a contract problem. An error from the endpoint inside the agent loop is
filed as a stack failure, so the subtask comes back deferred with `defer_class` `infrastructure`, counts in
`summary.infrastructure` and in `summary.lost_to_stack` (lost work: no usable result), and the verb exits
non-zero. Read `summary` first: a non-zero `infrastructure` means a node is broken or misconfigured, not
that the work was too hard for the seat. This page does not pin the HTTP status the endpoint answers with
for an unpublished id; the defer class is what a caller sees.

To check the id, read the live roster instead of a written figure: `offload_status {section: "local"}` lists
`served_now` (the ids the local endpoint serves now), and `offload_status {section: "brief"}` lists each
fleet node's `agent_seat`. `agent_model` for the local box must be one of the first; a remote node runs on the
`agent_model` of its own config.

## Important flows

Writing a contract file: start from the bare object above, put the call-level choices on the command line,
run `local-offload delegate --contract file.json --route auto`, and read `summary` before `results`.

## Data and state

The file is read once at the start of the call and never written. The call's results go to stdout as the
same JSON the MCP tool returns; the ledger and corpus rows the run writes are the engine's, not this
verb's.

## Interfaces and entry points

`local-offload delegate --contract <file>` (CLI). The MCP tool `agent_delegate` takes the same subtask
objects inside `{"subtasks": [...]}` together with the call-level arguments.

## Dependencies

[mcp-server.md](mcp-server.md) for the tool whose subtasks the file mirrors; the config's
`agent_delegation_enabled`, `agent_model` and `delegate_remotes`.

## Downstream effects

A change to the contract struct changes what a file may carry, with no change to this page's rules; a
change to the flag help's `(fields: ...)` list fails the doc test below until this page names the field.

## Invariants and assumptions

- A file is a bare subtask object or an array of at most 8; never the tool's wrapper.
- Unknown fields are ignored, never an error.
- `agent_model` is a published id of the endpoint that serves it.

## Error handling

| symptom | cause |
|---|---|
| `agent contract: goal is required` on a file that has a goal | the file is in the MCP shape (`{"subtasks": [...]}`); unwrap it |
| a ninth subtask is refused for its number | the cap is 8 per call; split the file |
| every subtask `infrastructure`, `lost_to_stack` above 0, exit non-zero | the box or node behind the run is broken or misconfigured; check `agent_model` against `served_now` first |
| `agent delegation is disabled on this box` | set `agent_delegation_enabled: true` in the config |

## Source map

| piece | where |
|---|---|
| the verb, its flags and the `--contract` help | `runDelegate` in `main.go` |
| the file reader (bare object or array, tolerant decode) | `parseContractFile` in `delegate_cmd.go` |
| the subtask object and its intake | `SubtaskSpec`, `PrepareContract` in `internal/delegate/intake.go` |
| the subtask cap | `CheckSubtaskCount` in `internal/delegate/intake_batch.go` |
| the contract fields and their json names | `AgentContract` in `internal/core/agentwire.go` |
| the summary counts | `Summary.LostToStack` in `internal/delegate/wire.go` |
| tests | `delegate_cli_test.go` (both shapes), `delegation_contract_docs_test.go` (this page against the flag help, and the wrapper trap) |
