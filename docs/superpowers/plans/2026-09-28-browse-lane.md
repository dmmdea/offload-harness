# Browse lane (offload_browse + agent `browse`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an MCP caller and the coding-agent seat drive the operator's own, already-running
Chromium browser toward a natural-language goal, as an explicit opt-in lane.

**Architecture:** The harness spawns a pinned Python sidecar (`setup/browse/runner.py`, built on
browser-use's `jev-ultrafast` agent loop and `browser-harness` CDP client) with a scrubbed environment.
The sidecar owns the browser; the harness owns every model call. Over a JSON-lines stdio protocol
the sidecar asks the harness for (a) a typed decision, which the harness proxies to a configured
**loopback** TypeSafe-shaped endpoint (`POST /v1/systemone`), and (b) a field value, which the
harness generates on its own local seat with a GBNF grammar. The harness holds no provider key.

**Tech Stack:** Go 1.26+ (harness), Python 3.12 + uv (sidecar), CDP.

**Spec:** ADR 0060 (`docs/architecture/decisions/0060-opt-in-browse-lane-drives-the-operators-browser.md`, written in Task 7).

## Global Constraints

- Off by default: no config key seeded; `offload_browse` is registered only when `BrowseConfigured()`.
- `browse_decision_url` must be `http://` on a loopback host (`127.0.0.1`, `::1`, `localhost`); anything else leaves the lane unconfigured and warns at load.
- The bearer for the decision endpoint comes from the env var `LOCAL_OFFLOAD_BROWSE_BEARER` in the harness process; it is never a config field and never reaches the sidecar.
- The sidecar env is an allowlist (the compose-lane list) plus `BH_TELEMETRY=0`, `BROWSER_HARNESS_TELEMETRY=0`, `ANONYMIZED_TELEMETRY=0`; cwd is a fresh temp dir.
- Invariant 1 holds: text generation uses the pipeline's llama client with a raw `grammar`, never `response_format`.
- One browser session at a time (`browseSlot`, capacity 1). No GPU lease.
- `max_actions` 1..60 (default `browse_max_actions`, 30). Timeout `browse_timeout_sec`, default 300.
- Deny-list (case-insensitive, word-bounded) on click/select labels: `publish|send|post|delete|remove|pay|buy|purchase|checkout|place order|order now|subscribe|unsubscribe|transfer|withdraw|sign out|log out|logout|deactivate|close account|confirm`. Denied controls are removed before the model sees them AND rechecked at execution. `allow_labels` (exact, case-insensitive label match) lifts a deny only on the attended MCP door; never on agent doors.
- Agent doors (`agent_run`, `agent_delegate`, fleet) require a non-empty host allowlist and the node opt-in `agent_allow_browse`; `route` must be `local`. The CLI (`local-agent --allow-browse`) is the attended door.
- Public repo: no operator names, hostnames, drive-letter dev paths or tailnet addresses anywhere (identity lint).
- Version 0.141.0 in `VERSION`, `internal/buildinfo/buildinfo.go`, `CHANGELOG.md`, `.printing-press.json`.

## Review Focus

1. A decision endpoint configured as a non-loopback URL (e.g. a provider URL) → lane stays unconfigured, load warns, no request leaves.
2. The model picks a "Publish"/"Send" button or the page relabels it after observation → the control is never offered, and the recheck at execution stops the run with status `denied`.
3. The sidecar hangs or the browser is closed mid-run → the harness kills the process tree at the timeout and returns a typed defer (`TIMEOUT` / `BROWSER_UNAVAILABLE`), never a hang.
4. A delegated contract with `allow_browse` placed by `route:auto` → rejected at intake; a node without `agent_allow_browse` refuses it at ACK.
5. Captured traffic carries cookies/authorization headers → redacted before anything is written.

---

## Stdio protocol (the sidecar's whole interface)

One JSON object per line, UTF-8. Harness → sidecar on stdin; sidecar → harness on stdout. stderr is a
diagnostic tail only.

1. Harness writes `{"type":"start","url":str,"goal":str,"max_actions":int,"allow_labels":[str],"allow_hosts":[str],"capture_prefixes":[str],"capture_path":str,"browser":str,"unattended":bool}`.
2. Sidecar, per decision: `{"type":"decide","id":int,"body":{TypeSafe request}}` → harness answers `{"type":"decision","id":int,"ok":true,"result":{"answers":..,"model":..,"usage":..}}` or `{"type":"decision","id":int,"ok":false,"error":str}`.
3. Sidecar, per TYPE_TEXT: `{"type":"text","id":int,"context":{goal,field,page,recent_actions}}` → harness answers `{"type":"text_result","id":int,"ok":true,"text":str}` or `ok:false,"error"`.
4. Sidecar, per executed action: `{"type":"step","n":int,"op":str,"label":str,"url":str}`.
5. Sidecar, once, last line: `{"type":"result","status":"done|blocked|denied|budget|error","class":str,"reason":str,"model_done":bool,"final":{"url":str,"title":str,"text":str},"actions":[..],"decisions":int,"text_calls":int,"captured":int}`. `class` is one of `""`, `BROWSER_UNAVAILABLE`, `DECISION_UNAVAILABLE`, `TEXT_UNAVAILABLE`, `HOST_NOT_ALLOWED`, `DENIED`, `RUNNER_FAILED`.

## File map

| File | Responsibility |
|---|---|
| `setup/browse/runner.py` | Sidecar: browser resolution, deny filter, host check, capture + redaction, jev-ultrafast loop with harness-backed `post_json`/`field_text`. Pure helpers importable without the pinned deps. |
| `setup/browse/test_runner.py` | stdlib `unittest` for the pure helpers and the protocol. |
| `setup/browse/pyproject.toml`, `setup/browse/uv.lock` | Pins: `browser-harness==0.1.13`, `jev-ultrafast` at a 40-char commit. |
| `setup/browse/install.ps1` | Opt-in install into `<OFFLOAD_HOME>/browse` (`uv sync --frozen`), prints the config keys. |
| `internal/config/config.go` | `browse_*` keys, `agent_allow_browse`, `BrowseConfigured()`, load warning. |
| `internal/core/types.go` | `TaskBrowse`. |
| `internal/pipeline/browse.go` | `runBrowse`: validation, slot, spawn, protocol loop, decision proxy, grammar text, typed defers, ledger. |
| `internal/mcpserver/mcpserver.go` | `offload_browse` tool; `agent_run`/`agent_delegate` `allow_browse` + `browse_hosts`; status strings. |
| `internal/agent/browsetool.go`, `policy.go`, `rules.go`, `builder.go` | `browse` tool, `ActBrowse`, `WithBrowse`, rule kind, BuildConfig wiring. |
| `cmd/local-agent/main.go` | `--allow-browse`, `--browse-hosts`. |
| `internal/core/agentwire.go`, `internal/delegate/*`, `internal/fleetnode/tasks.go`, `internal/pipeline/agenttask.go` | Contract fields, local-only placement, ACK refusal, local wiring. |
| docs (ADR 0060, `docs/systems/browse-lane.md`, mcp-server, coding-agent, OPERATOR-GUIDE, README, CONTRIBUTING, CHANGELOG) | Same-PR documentation. |

## Tasks

### Task 1: Config keys and `BrowseConfigured`
Tests first in `internal/config/browse_test.go`: loopback URL accepted (`127.0.0.1`, `localhost`, `[::1]`); `https://openrouter.ai/...` and `http://198.51.100.7/...` rejected; all three of python/script/url required. Then the fields, `Default()` values (`BrowseTimeoutSec: 300`, `BrowseMaxActions: 30`), path-key list, `go generate ./...` for `config.example.json`.

### Task 2: Pipeline lane `runBrowse`
Tests first in `internal/pipeline/browse_test.go` with a Go helper-process sidecar (`os.Args[1] == "-test.run=^TestBrowseHelperProcess$"`) and `httptest` servers for the decision endpoint and the llama chat route: happy path (decide → text → done), decision endpoint 500 → defer `DECISION_UNAVAILABLE`, sidecar exits without a result → `RUNNER_FAILED`, sidecar hangs → `TIMEOUT` within the budget, bad URL → `BAD_INPUT` before any spawn, unconfigured → `not_configured`, second concurrent call → `BUSY`, oversized decide body → refused.

### Task 3: Sidecar (`setup/browse/`)
Pure helpers + unittest; the jev-ultrafast integration behind `main()`; pins and install script. Tests: deny regex (blocks "Publish", "Send test email", "Delete draft"; does not block "Published posts", because the pattern is word-bounded), allow_labels only when attended, header redaction, host allowlist, DevToolsActivePort parsing, protocol request/response pairing.

### Task 4: MCP `offload_browse`
Registered only when configured; `route` other than `local`/"" → defer; Door `offload_browse`; `.printing-press.json` lists it; status strings name the external decision endpoint.

### Task 5: Agent tool and policy
`ActBrowse` (subject = lowercased host), rule kind accepted, `WithBrowse`; `BrowseTools(pol, run, hosts, unattended)`; BuildConfig `AllowBrowse`, `BrowseHosts`, `Browse`; unattended without hosts → not granted, note says why; prompt line appended.

### Task 6: Doors and placement
`local-agent` flags; `agent_run`/`agent_delegate` inputs; `AgentContract.AllowBrowse/BrowseHosts`; intake rejects non-local route; placement excludes remotes; fleet ACK refuses without `agent_allow_browse`; local agent task wires the pipeline's browse runner.

### Task 7: Docs, ADR, version
ADR 0060 (amends 0001 and 0059 without editing them), system doc, every table that lists tools or capability flags, CONTRIBUTING's "no cloud calls" line, CHANGELOG `## [0.141.0]`, all four version sources.
