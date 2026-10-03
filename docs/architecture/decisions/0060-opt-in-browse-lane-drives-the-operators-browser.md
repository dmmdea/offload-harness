---
status: Accepted
date: "2026-09-28"
---

# ADR 0060 — An opt-in browse lane drives the operator's own browser; the harness owns every model call and reaches only a loopback decision endpoint

## Context

The operator asked for a browser agent that both an MCP caller (`offload_browse`) and the coding-agent
seat (the `browse` tool) can use. Much of what a person does in a web app has no API, or only an API
that needs the session the person already holds: reading a dashboard, filling a form, walking a
multi-step flow. Two facts shape the design.

- **The useful browser is the operator's own.** It holds the logged-in sessions. A fresh, empty
  browser cannot do the work, and copying its cookies elsewhere would be worse than driving it.
- **A small local model is not a reliable browser driver.** browser-use's `jev-ultrafast` agent loop
  observes a page as an indexed element table and asks a model for ONE typed choice per step
  (CLICK, TYPE_TEXT, SELECT, SCROLL, WAIT, DONE). That is a typed-decision shape, served well by a
  TypeSafe-style `POST /v1/systemone` endpoint. The operator runs such an endpoint on loopback, and it
  may itself front a hosted decision model.

The harness already has rules this lane touches:

- ADR 0001: the Cascade never falls back to a cloud model, and the harness holds no cloud
  credentials. Invariant 2 reads that as "the harness never makes a cloud call".
- ADR 0003: every effectful action goes through one policy broker, and every capability is off by
  default.
- ADR 0002 and Invariant 1: text generation is grammar-constrained through a raw GBNF field, never
  `response_format`.
- ADR 0044: a node opts in to a write-capable door with its own config flag, and refuses at ACK
  otherwise.
- ADR 0059: an external tool runs pinned, env-scrubbed and with its telemetry off. This lane is the
  second such tool.

Driving a logged-in browser is dangerous in a way no other lane is. One mis-chosen click on "Publish",
"Send" or "Pay" is an irreversible act in the operator's name. And the page text the decision model
reads may leave the machine if the decision endpoint fronts a hosted model.

## Decision

The browse lane is an **opt-in** lane. A pinned Python sidecar drives the browser; the harness owns
every model call, every limit and every door.

1. **Opt-in, loopback-only, no key.** No `browse_*` key is seeded. The lane is configured only when
   `browse_python`, `browse_script` and `browse_decision_url` are all set, and the URL must be plain
   `http` on a loopback host (`127.0.0.1`, `::1`, `localhost`). Anything else leaves the lane
   unconfigured with a warning at load, and no request leaves. `offload_browse` is registered only
   when the lane is configured. The harness holds no provider key. If the local decision endpoint
   wants a bearer, it comes from the environment variable `LOCAL_OFFLOAD_BROWSE_BEARER` in the harness
   process. It is never a config field and never reaches the sidecar.
2. **The harness owns every model call.** The sidecar never opens a socket to a model. Over a
   JSON-lines stdio protocol it asks the harness for a typed decision, which the harness proxies to
   the loopback endpoint (request body bounded), and for a field value, which the harness generates
   on its own local seat under a GBNF `grammar` (Invariant 1).
3. **The sidecar is pinned, env-scrubbed and leaves nothing running.** `browser-harness` and
   `jev-ultrafast` are locked (`uv.lock`, installed with `uv sync --frozen`; `jev-ultrafast` at a
   40-character commit). The sidecar's environment is an allowlist (the compose lane's list plus
   `XDG_CONFIG_HOME`), with `BH_TELEMETRY=0`, `BROWSER_HARNESS_TELEMETRY=0` and
   `ANONYMIZED_TELEMETRY=0` forced, and it starts in a fresh temp directory. No `*_API_KEY`, token,
   lease variable or the decision bearer can reach it. It uses a lane-only browser-harness daemon
   name (`offload-browse`), and that daemon is stopped after every run, so the lane never stops a
   daemon some other tool started and leaves none of its own.
4. **A deny-list, a host allowlist, and one attended door.** Controls whose label matches the
   deny-list (publish, send, post, delete, remove, pay, buy, purchase, checkout, place order, order
   now, subscribe, unsubscribe, transfer, withdraw, sign out, log out, deactivate, close account,
   confirm; case-insensitive, word-bounded) are removed before the model sees them and rechecked
   against the live element at execution, so a page that relabels a control after observation ends
   the run as `denied` and never clicks it. `allow_hosts` pins the run to named hosts, subdomains
   included. A click whose link or form target is off-list is refused before it happens; a page that
   navigates off-list by itself (a script or server redirect) ends the run at the next observation. The only **attended** door is the MCP `offload_browse`
   call, whose caller is the operator's own session: `allow_labels` may lift the deny-list there for
   exact labels. Every **agent door** is judged **unattended**: the CLI `local-agent --allow-browse`
   (which always builds unattended, because a non-interactive CLI turns an ask into a deny), `agent_run`,
   `agent_delegate` and fleet contracts with `allow_browse`. There a non-empty host list
   (`--browse-hosts` or `browse_hosts`) is required and the grant is refused without one,
   `allow_labels` can never lift the deny-list, and an audit path is required (the CLI defaults to
   `<HOME>/.local-offload/agent-audit.jsonl` *(amended 2026-10-02: under the harness install root, see the
   amendment below)*). The agent doors driven by a contract also need the node
   opt-in of item 5. The policy broker gains the action kind `browse`
   (subject: the start URL's lowercased host), so a `--rules` entry such as
   `{"kind":"browse","glob":"*.bank.example","decision":"deny"}` works, and every agent-door browse
   call is audited (the MCP door is ledgered like every lane).
5. **Local-only placement, and a node opt-in.** A contract that carries `allow_browse` must have
   `route: local`; intake rejects any other route. Delegation placement never sends such a contract
   to a remote node, and a fleet node refuses it at ACK unless its own config has
   `agent_allow_browse: true` (default false), as `agent_allow_write` does for ADR 0044. The node that
   runs the browse must have the lane configured too, because the browser is that machine's own.
6. **Capture is redacted before it is written.** A caller may ask for up to eight URL prefixes (each
   on an allowed host) whose requests and responses are saved as JSONL. Cookie, set-cookie,
   authorization and proxy-authorization headers, sensitive query parameters and JSON or form keys
   (passwords, tokens, keys, session ids, codes, signatures), and any header whose name carries `token`, `auth`,
   `session`, `csrf`, `key` or `secret`, are dropped in the sidecar before anything reaches disk.
7. **Every failure is a typed defer.** `BAD_INPUT`, `CLI_MISSING`, `BROWSER_UNAVAILABLE`,
   `DECISION_UNAVAILABLE`, `TEXT_UNAVAILABLE`, `HOST_NOT_ALLOWED`, `DENIED`, `blocked`, `budget`,
   `TIMEOUT` and `RUNNER_FAILED`, plus `NOT_CONFIGURED` and `BUSY` when the lane is unbound or
   in use. A run that ends blocked, denied or over budget carries the run so far as `partial`. The
   harness kills the process tree at `browse_timeout_sec` and returns `TIMEOUT` rather than hang. One
   browse runs per process at a time, because there is one operator browser. There is no GPU lease.
   Nothing falls back to another model or lane.
8. **DONE is not proof.** `model_done` is the decision model's claim. The result carries the final
   URL, title and visible text and the executed actions, and the caller verifies the outcome
   independently.

## Consequences

- **What leaves the machine.** The visible page text and the element labels go to the decision
  endpoint on every step. The harness calls only loopback and holds no key, but that endpoint may
  front a hosted decision model, so the page's text can leave the machine. The operator accepts that
  by configuring the endpoint; the tool description says so, so a calling agent never has to guess.
  Pages whose text must not leave the machine must not be browsed. Field values (what the run types)
  are generated by the local seat and stay local, unless the goal itself names them.
- **This amends ADR 0001's "never a cloud call" reading for THIS lane only, without editing it**, the
  way ADR 0058 amends ADR 0011. The Cascade still never falls back to a cloud model and the harness
  still holds no cloud credential. What changes is that one opt-in, caller-invoked lane asks a
  loopback service the operator runs, and that service may be a proxy to a hosted model.
  `offload_nim` remains the only remote MODEL surface the harness itself calls; `offload_browse` asks
  a loopback decision endpoint. `offload_status.remote` names both (`browse_configured`,
  `browse_decision_url`), which deliberately changed the `offload_status` golden.
- **The browser session is the operator's real logged-in profile.** A run acts with the operator's
  sessions. The deny-list is a last-line guard against the most damaging clicks, not a sandbox: a
  control labelled "Save" or "Submit" is not on it. The safety posture is that the lane is off by
  default, agent doors need a named host allowlist, an audit path and (for contracts) a node opt-in, and the caller verifies the
  outcome.
- The browser must already be running with "Allow remote debugging for this browser instance"
  ticked at `<browser>://inspect/#remote-debugging`. The harness never launches or restarts the
  operator's browser.
- The lane is as fresh as its lockfile. Bumping either pin is a lockfile change.
- One browser at a time: a second concurrent call in the same process is refused as busy.

## Alternatives considered

- **Cookie-replay private APIs.** Exporting the browser's cookies and replaying the site's private
  API calls. Rejected: it moves session secrets out of the browser into a second process, breaks on
  anti-bot and token rotation, and works only for sites whose API was reverse-engineered. Driving the
  live tab needs no secret to leave it.
- **A generic browser MCP server.** Rejected: it would give the calling agent an ungated click and
  type surface with no deny-list, no host allowlist, no policy-broker kind and no node opt-in, and
  every observation would flow through the calling model's own (cloud) context.
- **Let the sidecar call the provider directly.** Rejected: the sidecar would need a provider key,
  which the harness deliberately never holds (ADR 0001), and the harness would lose the one place
  where the endpoint is pinned to loopback and the request size is bounded.
- **A remote browser cloud.** Rejected: it would run without the operator's sessions, and hosting the
  browsing itself is a heavier exception to ADR 0001 than a loopback decision call.

## Related code

- [`internal/pipeline/browse.go`](../../../internal/pipeline/browse.go) — `runBrowse`: validation, the slot, the spawn, the protocol loop, the decision proxy, grammar text and the typed defers.
- [`setup/browse/runner.py`](../../../setup/browse/runner.py) — the sidecar: browser resolution, deny filter, host check, capture and redaction.
- [`internal/config/config.go`](../../../internal/config/config.go) — the `browse_*` keys, `agent_allow_browse` and `BrowseConfigured`.
- [`internal/mcpserver/mcpserver.go`](../../../internal/mcpserver/mcpserver.go) — `offload_browse` and the `offload_status` remote block.
- [`internal/agent/browsetool.go`](../../../internal/agent/browsetool.go) and [`internal/agent/policy.go`](../../../internal/agent/policy.go) — the `browse` tool and the `browse` action kind.
- [`cmd/local-agent/`](../../../cmd/local-agent/) — `--allow-browse`, `--browse-hosts`.
- [`internal/core/agentwire.go`](../../../internal/core/agentwire.go) and [`internal/fleetnode/tasks.go`](../../../internal/fleetnode/tasks.go) — the contract fields and the ACK refusal.

## Related docs

- [Browse lane](../../systems/browse-lane.md)
- [ADR 0001 — defer, never cloud fallback](0001-defer-never-cloud-fallback.md)
- [ADR 0003 — single policy broker; capability flags off by default](0003-policy-broker-and-capability-flags-off-by-default.md)
- [ADR 0059 — an external-CLI media tool runs pinned and env-scrubbed](0059-external-cli-media-tool-runs-cpu-class-pinned-env-scrubbed.md)
- [Glossary: Browse lane, Deny-list](../../glossary.md)

## Amendment 2026-10-02 (register C-92, the audit path follows the install root)

The audit path an agent door requires, and the CLI defaults, is now `agent-audit.jsonl` under the harness
install root (`home` in the config, else `~/.local-offload`, the earlier location), not always
`~/.local-offload`. The requirement is unchanged: every agent-door browse run leaves a trail, outside any
worktree, and the grant is refused when no audit path can be resolved. An earlier trail left at the old
location on a node that sets `home` is not moved; see the amendment to
[ADR 0004](0004-worktree-confinement-audit-outside.md).
