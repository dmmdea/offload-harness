# Browse lane

## Purpose

The browse lane lets an MCP caller (`offload_browse`) and the coding-agent seat (the `browse` tool)
drive the operator's own, already-running, logged-in Chromium browser toward a natural-language goal.
It is opt-in and off by default. A pinned Python sidecar owns the browser; the harness owns every model
call, every limit and every door. The decision is recorded in
[ADR 0060](../architecture/decisions/0060-opt-in-browse-lane-drives-the-operators-browser.md).

## Questions this doc answers

- How do I turn the lane on, and what must be running first?
- Which door may do what: the MCP call, the CLI, `agent_run`, `agent_delegate`, a fleet node?
- What is the sidecar protocol, and what does each failure class mean?
- What leaves the machine when a browse runs?
- How do I verify an install without risking a real page?

## Scope

`internal/pipeline/browse.go` (`runBrowse`), the sidecar in `setup/browse/`, the `browse_*` config
keys and the node opt-in `agent_allow_browse`, the MCP tool `offload_browse`, the agent tool `browse`
and its policy kind, and the placement rules for contracts that carry `allow_browse`.

## Non-scope

The decision model itself. The lane calls a loopback, TypeSafe-shaped decision endpoint that the
operator runs; what sits behind it is the operator's. Also out of scope: launching or restarting the
operator's browser, and any remote browser service.

## Key concepts

- **Sidecar** — `setup/browse/runner.py`, built on browser-use's `jev-ultrafast` agent loop and the
  `browser-harness` CDP client. It observes the page as an indexed element table and executes one
  CLICK, TYPE_TEXT, SELECT, SCROLL or WAIT per decision.
- **Decision endpoint** — `browse_decision_url`, a plain-`http` loopback URL. The harness POSTs the
  sidecar's typed request to it and hands the typed answer back.
- **Deny-list** — labels the run may never click or select (see below). Denied controls are removed
  before the model sees them and rechecked at execution.
- **Attended vs unattended** — only the MCP call `offload_browse` is attended (its caller is the
  operator's own session). Every agent door is unattended for browse: the `local-agent` CLI (which
  always builds unattended: a non-interactive CLI turns an ask into a deny), `agent_run`,
  `agent_delegate` and fleet contracts.
- **DONE is a claim** — `model_done` is the decision model's opinion, not proof of the outcome.

## How the system works

1. The caller's request is validated before anything is spawned: an absolute `http(s)` start URL, a
   goal of 1 to 4,000 characters, `max_actions` between 1 and 60 (default `browse_max_actions`, 30),
   host names in `allow_hosts`, and up to eight `capture` URL prefixes whose hosts are in
   `allow_hosts`. `route` must be `local`.
2. The harness takes the in-process browse slot (capacity one: there is one operator browser), and
   spawns the sidecar with an allowlisted environment, telemetry forced off, and a fresh temp
   directory as cwd.
3. The sidecar attaches to the running browser through the browser's `DevToolsActivePort` file, opens a
   background tab, and loops: observe, ask the harness for a decision, execute one action.
4. Every model call goes through the harness. A typed decision is proxied to `browse_decision_url`
   (the bearer, if any, comes from `LOCAL_OFFLOAD_BROWSE_BEARER` in the harness process and never
   reaches the sidecar). A field value for TYPE_TEXT is generated on the local agent seat under a raw
   GBNF `grammar` (Invariant 1).
5. The sidecar sends one `result` line and exits. The harness stops the lane's browser-harness daemon
   (`offload-browse`), removes nothing of the operator's, and returns the result.

### The stdio protocol

One JSON object per line, UTF-8. Harness to sidecar on stdin, sidecar to harness on stdout. stderr is
a diagnostic tail only.

| Direction | Line | Meaning |
|---|---|---|
| harness to sidecar | `{"type":"start","url","goal","max_actions","allow_labels":[],"allow_hosts":[],"capture_prefixes":[],"capture_path","browser","unattended"}` | Sent once. `unattended` is true on agent doors. |
| sidecar to harness | `{"type":"decide","id","body":{typed request}}` | Ask for a typed decision. |
| harness to sidecar | `{"type":"decision","id","ok":true,"result":{"answers","model","usage"}}` or `{"type":"decision","id","ok":false,"error"}` | The endpoint's answer. |
| sidecar to harness | `{"type":"text","id","context":{goal,field,page,recent_actions}}` | Ask for a field value (TYPE_TEXT). |
| harness to sidecar | `{"type":"text_result","id","ok":true,"text"}` or `ok:false,"error"` | The local seat's answer. |
| sidecar to harness | `{"type":"step","n","op","label","url"}` | One executed action. |
| sidecar to harness | `{"type":"result","status":"done\|blocked\|denied\|budget\|error","class","reason","model_done","final":{url,title,text},"actions":[],"decisions","text_calls","captured"}` | Once, last line. |

`class` in the result is one of `""`, `BROWSER_UNAVAILABLE`, `DECISION_UNAVAILABLE`,
`TEXT_UNAVAILABLE`, `HOST_NOT_ALLOWED`, `DENIED` or `RUNNER_FAILED`. The harness refuses a `decide`
body larger than 256 KiB and a protocol line larger than 1 MiB.

### The deny-list

Matched case-insensitively and word-bounded against a click or select label: publish, send, post,
delete, remove, pay, buy, purchase, checkout, place order, order now, subscribe, unsubscribe, transfer,
withdraw, sign out, log out, logout, deactivate, close account, confirm. "Published posts" does not
match; "Send test email" does. Typing into a field is not an effect and is not filtered. The list is a
last-line guard, not a sandbox: a control labelled "Save" or "Submit" is not on it.

`allow_labels` lifts a deny for an exact label (case-insensitive match) on the **attended MCP door
only**. On every agent door, the CLI included, the deny-list can never be lifted.

## Important flows

### The doors

| Door | Attended? | `allow_labels` lifts the deny-list | Host allowlist | Extra requirement |
|---|---|---|---|---|
| MCP `offload_browse` | yes (the only one) | yes | optional (`allow_hosts`) | lane configured; `route` local |
| CLI `local-agent --allow-browse` | no (always unattended) | never | required (`--browse-hosts host1,host2`); the grant is refused without it | lane configured on this box; an audit path (default `<HOME>/.local-offload/agent-audit.jsonl`) |
| `agent_run` with `allow_browse` | no (unattended) | never | required (non-empty `browse_hosts`) | `agent_allow_browse: true` on this node; lane configured; an audit path |
| `agent_delegate` with `allow_browse` | no (unattended) | never | required (non-empty `browse_hosts`) | `route` local (intake rejects otherwise); placement never picks a remote node |
| Fleet contract with `allow_browse` | no (unattended) | never | required | the node refuses it at ACK unless `agent_allow_browse: true` |

The policy broker has the action kind `browse`, with the start URL's lowercased host as its subject, so
`--rules` entries such as `{"kind":"browse","glob":"*.bank.example","decision":"deny"}` apply.
Every agent-door browse call is audited by the policy broker (the audit trail the grant requires); an
MCP `offload_browse` call is recorded in the savings ledger like every other lane, and the decision
endpoint keeps its own spend ledger.

## Data and state

- Config keys (all default empty except where shown; none is seeded):

| Key | Meaning |
|---|---|
| `browse_python` | The sidecar venv's interpreter (printed by the installer). |
| `browse_script` | The installed `runner.py`. |
| `browse_decision_url` | Loopback decision endpoint. Must be plain `http` on `127.0.0.1`, `::1` or `localhost`, or the lane stays unconfigured and the load warns. |
| `browse_browser` | `chrome`, `edge`, `brave`, `chromium`, or empty for the first running browser with remote debugging allowed. |
| `browse_cdp_url` | Pins the lane to ONE browser endpoint, e.g. `http://127.0.0.1:9333` (resolved through `/json/version`) or a `ws://` URL. Loopback host, a real port, no credentials, query or fragment; an `http://` value is the endpoint root (no path) and a `ws://` value a `/devtools/` socket. Anything else leaves the lane unregistered. Wins over `browse_browser`. Use it for a dedicated agent profile (below). |
| `browse_timeout_sec` | One run's wall budget. Default 300. |
| `browse_max_actions` | Default executed-action budget. Default 30, ceiling 60. |
| `browse_capture_dir` | Where redacted captures land. Empty means `<state_dir>/browse-captures`, or the OS temp dir. |
| `agent_allow_browse` | Node opt-in for the agent doors. Default false. |

- `BrowseConfigured()` is true only when `browse_python`, `browse_script` and a loopback
  `browse_decision_url` are all set. `offload_browse` is registered only then.
- The bearer for the decision endpoint is the environment variable `LOCAL_OFFLOAD_BROWSE_BEARER` in
  the harness process. It is never config.
- Captures are JSONL files under the capture dir; the result's `capture_path` names one.

## Interfaces and entry points

- MCP `offload_browse` (`url`, `goal`, `max_actions?`, `allow_hosts?`, `allow_labels?`, `capture?`,
  `route?`). Result: `{status, model_done, final{url,title,text}, actions[], steps, step_log[],
  decisions, text_calls, decision_model, decision_cost_usd, capture_path, captured, removed_labels}`.
- Agent tool `browse` (`url`, `goal`, `max_actions`, `security_risk`).
- CLI `local-agent --allow-browse --browse-hosts host1,host2` (the host list is required).
- `agent_run` and `agent_delegate` inputs `allow_browse` and `browse_hosts`.
- `offload_status`'s `remote` block: `browse_configured` and `browse_decision_url`.

### Install

```powershell
pwsh setup/browse/install.ps1                          # into ~/.local-offload/browse
pwsh setup/browse/install.ps1 -OffloadHome <OFFLOAD_HOME>
```

The script copies the sidecar, runs `uv sync --frozen` against the locked pins (`browser-harness`
0.1.13, `jev-ultrafast` at a 40-character commit), runs the unit tests, disables browser-harness
telemetry, and prints one JSON line with the `browse_python` and `browse_script` values. Then:

1. Set `browse_python`, `browse_script` and `browse_decision_url` (and optionally `browse_browser`) in
   the harness config, and export `LOCAL_OFFLOAD_BROWSE_BEARER` in the harness's environment if the
   decision endpoint wants one.
2. Choose the browser the lane drives. **Recommended: a dedicated agent profile** started with its own
   port and profile directory, e.g. `brave.exe --remote-debugging-port=9333 --user-data-dir=<OFFLOAD_HOME>/browse/agent-profile`,
   and `browse_cdp_url: "http://127.0.0.1:9333"`. Log in once, in that window, to the sites the agent
   may use. Recent Chromium builds make the operator approve EVERY new debugging connection to the
   main profile (the per-instance toggle at `<browser>://inspect/#remote-debugging` exposes only a
   WebSocket and prompts per connection), so an unattended run against the main profile times out in
   the handshake; a dedicated instance does not prompt, and keeps the agent out of the everyday
   profile. The alternative is the toggle itself (`browse_browser` names which browser's
   `DevToolsActivePort` to read), with someone present to approve each run. The harness never launches
   or restarts the browser; register the dedicated port in your port inventory.
3. Restart the MCP client so `offload_browse` appears in `tools/list`.
4. For the agent doors, set `agent_allow_browse: true` on each node that may run one.

## Dependencies

Python 3.12 and `uv`; the two locked pins; a Chromium-family browser with remote debugging allowed;
the loopback decision endpoint; the local agent seat (for grammar-constrained field values).

## Downstream effects

`offload_status` (`remote.browse_configured`, `remote.browse_decision_url`), the MCP manifest
(`.printing-press.json`), the agent tool list, the policy broker's rule vocabulary, and delegation
placement (a contract with `allow_browse` is local-only).

## Invariants and assumptions

1. Off by default: no `browse_*` key is seeded and `agent_allow_browse` is false.
2. The decision URL is loopback plain `http` or the lane is unconfigured. No request leaves otherwise.
3. The harness holds no provider key. The bearer is env-only and never reaches the sidecar.
4. The sidecar never opens a socket to a model; the harness makes every model call.
5. Text generation never uses `response_format` (Invariant 1): a llama.cpp seat gets a raw GBNF `grammar`,
   and a vLLM seat gets the same one-field JSON schema through the path the cascade already uses for vLLM.
6. Denied controls are removed before the model sees them and rechecked at execution.
7. Agent doors need a non-empty host allowlist, `route: local`, and a node opt-in; the deny-list
   cannot be lifted there.
8. One browse at a time per process. No GPU lease.
9. A failure is a typed defer; nothing falls back to another model or lane.

## Error handling

Every failure returns `deferred:true` with `browse: <CLASS>: <detail>`. A run that started carries the
run so far as `partial`.

| Class | Meaning |
|---|---|
| `NOT_CONFIGURED` | `browse_python`, `browse_script` or a loopback `browse_decision_url` is missing. |
| `BUSY` | Another browse run in this process holds the browser (one at a time). |
| `BAD_INPUT` | Validation failed before any spawn (URL, goal, budgets, host, capture, route). |
| `CLI_MISSING` | The interpreter or runner file is unreadable, or the sidecar cannot start. |
| `BROWSER_UNAVAILABLE` | No running browser with remote debugging allowed, or it closed mid-run. |
| `DECISION_UNAVAILABLE` | The loopback endpoint refused, errored, or returned something unusable. |
| `TEXT_UNAVAILABLE` | The local seat could not write a field value. |
| `HOST_NOT_ALLOWED` | The page left `allow_hosts` (or `browse_hosts`). |
| `DENIED` / `denied` | A denied control was about to be executed. |
| `blocked` | The model reported it cannot proceed. |
| `budget` | `max_actions` was exhausted. |
| `TIMEOUT` | No result within `browse_timeout_sec`; the process tree was killed. |
| `RUNNER_FAILED` | The sidecar exited without a result, or the capture dir could not be created. |
| busy | Another browse run in this process holds the operator's browser. |

## Security and privacy notes

**What leaves the machine.** The visible page text and the element labels go to the decision endpoint
on every step. The harness calls only loopback and holds no key, but the operator's endpoint may front
a cloud decision model, so the page's text can leave the machine. Do not browse pages whose text must
not. Field values are generated locally and stay local unless the goal names them.

The browser session is the operator's real, logged-in profile, so a run acts with those sessions. The
posture is: off by default, a deny-list on effectful labels, a host allowlist (required on agent
doors), local-only placement, a per-node opt-in, redacted capture (cookie, set-cookie,
authorization, proxy-authorization, and any header name containing `token`, `auth`, `session`,
`csrf`, `key` or `secret`, are dropped, and sensitive query parameters and JSON or form keys —
passwords, tokens, keys, session ids, codes, signatures — are replaced, before anything is written), telemetry off for both packages,
and a lane-only daemon name that is stopped after every run. The caller verifies the outcome: DONE
is not proof.

## Observability and debugging

- The result's `step_log[]`, `actions[]`, `removed_labels` and `decision_model` show what ran and what
  the deny-list removed.
- A sidecar stderr tail is appended to a defer reason.
- Browse calls are audited by the policy broker on agent doors, and ledgered like other lanes.
- `offload_status remote` shows whether the lane is configured and which decision URL it will call.
- A missing `offload_browse` in `tools/list` means the lane is unconfigured or the client was not
  restarted.

## Testing notes

- Pipeline tests run a Go helper-process sidecar through the real protocol loop, with `httptest`
  servers for the decision endpoint and the local seat.
- The sidecar's pure helpers (deny regex, host check, header redaction, `DevToolsActivePort` parsing)
  are stdlib `unittest`, run by the installer: `cd setup/browse && python -m unittest -v test_runner`.
- Verify an install without touching a real page: with the browser running and remote debugging
  ticked, call `offload_browse` with a start URL on a harmless page you own, `allow_hosts` set to its
  host, `max_actions` 3 and a goal that only reads. Check that `status` is `done`, that `final.url`
  is the page you named, and that `offload_status remote.browse_configured` is true. Then ask for a
  goal that needs a "Send" control and confirm the run ends `denied` rather than clicking.

## Common pitfalls

- Pointing `browse_decision_url` at a provider URL: it is refused, and the lane stays unconfigured.
- Treating `status: done` as verified. It is the model's claim.
- Expecting `allow_labels` to work through the CLI, `agent_run` or `agent_delegate`. It never does.
- Sending an agent-door browse (including `local-agent --allow-browse`) without a host list, with a
  non-local `route`, without an audit path, or to a node without `agent_allow_browse`.
- Expecting the harness to start the browser or tick the remote-debugging box.

## Source map

- [`internal/pipeline/browse.go`](../../internal/pipeline/browse.go) — `runBrowse`, the slot, the
  protocol loop, the decision proxy, grammar text, the typed defers
- [`setup/browse/runner.py`](../../setup/browse/runner.py) — the sidecar
- [`setup/browse/README.md`](../../setup/browse/README.md) — pins, install, tests
- [`internal/config/config.go`](../../internal/config/config.go) — the `browse_*` keys and
  `BrowseConfigured`
- [`internal/mcpserver/mcpserver.go`](../../internal/mcpserver/mcpserver.go) — `offload_browse` and the
  status `remote` block
- [`internal/agent/browsetool.go`](../../internal/agent/browsetool.go), [`internal/agent/policy.go`](../../internal/agent/policy.go), [`internal/agent/rules.go`](../../internal/agent/rules.go) — the `browse`
  tool, the `browse` action kind and the rule vocabulary
- [`cmd/local-agent/`](../../cmd/local-agent/) — the CLI flags
- [`internal/core/agentwire.go`](../../internal/core/agentwire.go),
  [`internal/pipeline/agenttask.go`](../../internal/pipeline/agenttask.go),
  [`internal/fleetnode/tasks.go`](../../internal/fleetnode/tasks.go) — the contract fields, local
  wiring and the ACK refusal

## Related docs

- [../architecture/decisions/0060-opt-in-browse-lane-drives-the-operators-browser.md](../architecture/decisions/0060-opt-in-browse-lane-drives-the-operators-browser.md)
- [../architecture/decisions/0001-defer-never-cloud-fallback.md](../architecture/decisions/0001-defer-never-cloud-fallback.md)
- [../architecture/decisions/0003-policy-broker-and-capability-flags-off-by-default.md](../architecture/decisions/0003-policy-broker-and-capability-flags-off-by-default.md)
- [mcp-server.md](mcp-server.md)
- [coding-agent.md](coding-agent.md)
- [../OPERATOR-GUIDE.md](../OPERATOR-GUIDE.md)
- [Glossary: Browse lane, Deny-list](../glossary.md)
