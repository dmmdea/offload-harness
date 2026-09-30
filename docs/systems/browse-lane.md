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
3. The sidecar attaches to the running browser (the `browse_cdp_url` endpoint when set, else the named
   browser's `DevToolsActivePort` file), opens a background tab (which it brings to the front once, at the
   first observe, when `browse_activate_tab` is on with a dedicated `browse_cdp_url`), and loops: observe, ask
   the harness for a decision, execute one action. The observe that follows an action first waits for the page to settle
   (about 0.3 s on a quiet page, 1.5 s of page time at most), and immediately before every page read the sidecar jumps
   the finite CSS animations and transitions the snapshot cannot see to their end state (see
   [Background tab rendering](#background-tab-rendering)).
4. Every model call goes through the harness. A typed decision is proxied to `browse_decision_url`
   (the bearer, if any, comes from `LOCAL_OFFLOAD_BROWSE_BEARER` in the harness process and never
   reaches the sidecar). A field value for TYPE_TEXT is generated on the local agent seat under a raw
   GBNF `grammar` (Invariant 1).
5. Before closing its tab the sidecar waits until the page's XHR/fetch traffic has been quiet for 3.5 s
   (capped at 15 s): an editor saves on a debounce after the last input (one production editor's autosave left
   2.3 s after a keystroke), and closing the tab at DONE dropped that save while the run reported done. Then it
   closes the tab, sends one `result` line, stops the lane's browser-harness daemon (`offload-browse`) and
   exits; the harness returns the result. Nothing of the operator's is removed.

### Background tab rendering

The lane's tab is opened in the background (`Target.createTarget` with `background: true`, so by default
the operator's own tab is never activated) with focus emulation on, which jev-ultrafast enables to keep
`requestAnimationFrame` and menus rendering. A hidden tab still produces no rendering frames for CSS
animations (measured below). The lane corrects the main consequence; the bullets say how, and where the
correction stops. The last bullet is the opt-in setting that removes the cause on a dedicated agent browser
(`browse_activate_tab`, recommended there):

- **CSS animations and transitions stay at their start state.** A dialog or dropdown menu that fades in
  with `@keyframes` or a `transition` keeps opacity 0. The page snapshot drops every element whose
  computed opacity is 0 (and everything inside it), so the model never sees the control the last click
  opened and the run ends `blocked` ("the model or the loop reported no progress"). Measured in a
  background tab with a dialog opened by a real CDP mouse click and read 400 ms later: a `@keyframes`
  fade and a `transition` fade both read opacity 0 and not visible until their animations were
  finished, then opacity 1 and visible. A production web app's dropdown menu showed 0 of 7 items visible
  (its animation and transition both running) and 7 of 7 after.
- **The lane finishes what the snapshot cannot see, immediately before every page read.** The sidecar
  wraps jev's page read (`browser_operation`, which `Browser.observe` calls once per attempt) and, for an
  observe read, first evaluates a short script that calls `finish()` on every animation in
  `document.getAnimations()` whose end time is finite, that has not finished and whose target element the
  snapshot does not already see: hidden by opacity or `visibility`, with an empty box, or outside the
  viewport (a slide-in panel is fully opaque but starts off screen). Because it runs on every read
  attempt, it covers content the page mounts during jev's short wait after an input (an autocomplete
  option, for one) and the retries after a stale read or a navigation. Infinite animations (spinners,
  shimmer) have an infinite end time and are left alone, and an animation whose `finish()` throws is
  skipped. The call never raises: a navigating page or a dead session means nothing was finished and the
  read runs as it always did. The runner logs `finished N pending CSS animation(s) before observe` to
  stderr when it finished any.
- **An element that is already visible is left alone.** An animation whose target is visible and on
  screen is not finished, so a pending exit on it stays as it was without this fix: a toast that a CSS
  animation fades out after a delay stays in the snapshot (finishing it would jump it to its hidden end
  state), and a dialog that is closing keeps showing until the page removes it. This follows from the
  animation model and jev's snapshot code; it was not measured on a live page.
- **Not before an action.** A `finish()` between the observe and the pre-click freshness check could make
  that check fail: the snapshot's per-element guard is empty for an element at opacity 0 and ends with the
  innerText of the element's enclosing form, dialog, row or list item (which leaves out text hidden with
  `visibility`), and the page marker reads the opacity-filtered text and controls. Every action is
  followed by an observe, which already finishes what the action started.
- **`requestAnimationFrame` fades are not targeted.** A fade driven from script measured opacity 0.0055
  at 400 ms on the fixture: above 0, so the snapshot keeps the element, and `finish()` changes nothing
  there. Whether such a fade ever reaches opacity 1 in this tab was not measured.
- **Forcing frames with screenshots does not work.** `Page.captureScreenshot`, clipped or not, hung for
  more than 15 s in the hidden tab on the same fixture, so the lane does not try to wake the tab with
  screenshots (it never requests one itself). Other ways to produce frames were not tried.
- **A dialog the page mounts on its own timer is waited for after every action.** jev reads the page about
  50 ms after an input (200 ms for a combobox fill). A confirm dialog opened from a menu item is, at that
  moment, either not mounted yet or mounted at opacity 0 with its open-state style change not applied yet:
  the finish above has no animation to finish, the read returns the page as it was, the model sees no
  progress and the run ends `blocked`. Measured on a production web app in the operator's browser: 50 ms
  after the click the snapshot was unchanged, 1.5 s later one finish made the dialog's confirm button
  visible, and the same flow ended `blocked` 3 times out of 3 with only the finish. So the observe that
  follows an action first runs a settle loop (`settle_after_input`): every 100 ms it finishes newly started
  animations, with the same script as above, and reads a counter of DOM mutations that a `MutationObserver`
  keeps in the page. A poll is quiet when no animation was finished and the counter did not move. The loop
  stops after two quiet polls in a row once 0.3 s have passed, and at 1.5 s of page time whatever the page
  does (plus the calls in flight, see below). With it, the read after the menu click showed exactly the
  dialog (its text, `Cancel` and
  the confirm button), and once the goal named the confirm click as a step of its own the run clicked it and
  the record was deleted (checked independently). Starting the browser with its background-throttling and
  occlusion-detection switches off (`--disable-background-timer-throttling`,
  `--disable-renderer-backgrounding`, `--disable-backgrounding-occluded-windows`,
  `--disable-features=CalculateNativeWinOcclusion`) did not remove the need for the settle: the flow still
  ended `blocked` without it.
- **The settle's cost and bounds.** It runs at the start of every observe that follows an action, not the
  first observe and not after a `wait` action, and once per action however many times jev retries the read
  (a typed field pays it too). A quiet page costs about 0.3 s of waiting per action plus two CDP calls per
  poll (more on a slow daemon), on top of jev's own wait; a page that never stops changing (a live ticker, a
  timer that rewrites the DOM) costs the full 1.5 s every time.
  A page that is completely silent for the first 0.3 s counts as settled, so a dialog whose first DOM change
  comes later than that is not waited for: the following observe finishes whatever is pending then, and the
  `wait` action re-observes. The counter is read as an observer id plus a count, so a navigation, which gives
  the new document a new observer, reads as a change even when the count happens to match. It never raises:
  a page that is navigating, a dead session or an IPC timeout counts as activity, a dead session is polled
  until the cap and the read that follows reports the real error. The cap is checked between polls, so it
  bounds the page time and not the calls in flight: the harness can hold one CDP call for several seconds on
  a page whose JS thread is frozen (a native `confirm()` or `alert()` open), and a poll makes two. jev's own
  read fails the same way right after, and `browse_timeout_sec` still bounds the run. The observer is
  installed by the first read, so mutations between the input and that read are not counted (an animation
  they started is still finished by the first poll). In a run with `capture` the settle drains the daemon's
  event buffer between its polls: the buffer holds 500 events and drops the oldest, and the settle starts
  right after the input, when the action's own requests fire (without `capture` that drain does nothing).
  The runner logs `settled N.Ns after an input (...)` to stderr when the page was active.
- **Content mounted after the read shows up at the next observe.** The finish runs immediately before each
  read, not after it, and the settle covers only the first 0.3 to 1.5 s after an input. An element the page
  inserts once jev has read the page (a debounce or a network response that lands later) has no animation
  yet when the finish runs, so the following observe finishes it (the `wait` action re-observes).
- **Activating the lane's tab removes the cause (opt-in: `browse_activate_tab`).** The tab jev creates reports
  `document.visibilityState` `"visible"` but is not the window's active tab, and Chromium produces almost no
  frames for it, which is why every correction above exists. With `browse_activate_tab: true` and a
  `browse_cdp_url`, the sidecar calls `Target.activateTarget` for the lane's own tab once per run, at the
  first observe, before the settle and before jev's read. It is a browser-level call in no session, with the
  target id of the tab jev created, so no other tab is touched. Measured on a production web app's confirm
  dialog opened from a menu item, in a dedicated agent browser (a separate profile nobody looks at), with
  nothing finished by the lane: the dialog mounted at opacity 0 at +0.1 s. Left in the background, its
  opacity transition started at +0.2 s in one run but, in the lane's own run, not within +0.33 s (the settle
  saw no DOM mutation and no animation and stopped at 0.3 s, and the read was an empty page, because the
  modal hides everything else); left alone, the dialog reached opacity 1 only at +2.1 s. After the activation
  the same flow read opacity 0.957 at +0.2 s and 1 at +0.4 s, and the model's read after the menu click showed
  the whole dialog in every instrumented run. End to end, deleting a record through the real UI (checked
  independently): 5 of 5 runs with the activation and a goal that describes the dialog's question and says
  the task is not done and not blocked while it is open (see Common pitfalls); about half with the older
  step-list wording, where the failures were the model choosing BLOCKED with the dialog fully visible, not
  rendering; 0 of 3 on the MCP path with only the settle and no activation. The finish and the settle stay on;
  those runs had both in place. **Recommended for a dedicated agent browser.**
- **Why the activation is opt-in and needs a dedicated endpoint.** It switches the window's active tab. In the
  operator's everyday browser that is the tab someone is looking at, so the harness sends `activate_tab` to
  the sidecar only when `browse_activate_tab` is true AND `browse_cdp_url` names an endpoint the lane
  accepts. With the key true and no such endpoint the setting is ignored and the lane is not failed: the run
  goes on as before, the config load prints a warning, and `offload_status` reports
  `remote.browse_activate_tab` false with a `remote.browse_activate_tab_note`. (A `browse_cdp_url` that is set
  but refused leaves the lane unregistered; the warning and the note then say refused instead of missing.) An
  activating run also stops the lane's own browser-harness daemon before jev starts one: jev reuses any live
  daemon of that name and never compares it with the endpoint the run pins, and one left by a crashed run
  could still be attached to the everyday browser. A failed activation (a dead
  daemon, a tab that is already gone) is logged as `activate_tab skipped: ...` and the run continues with the
  tab in the background, exactly as without the setting; it is attempted once per run, never retried. It was
  measured in a separate profile started away from the screen; how an on-screen window behaves was not
  measured.

Out of scope here: `date` and `datetime-local` inputs are not in the snapshot at all (its role table maps
an input only for text, email, url, tel, search, number, checkbox, radio and button types), so the run
cannot fill them, animation or not.

### The stdio protocol

One JSON object per line, UTF-8. Harness to sidecar on stdin, sidecar to harness on stdout. stderr is
a diagnostic tail only.

| Direction | Line | Meaning |
|---|---|---|
| harness to sidecar | `{"type":"start","url","goal","max_actions","allow_labels":[],"allow_hosts":[],"capture_prefixes":[],"capture_path","browser","cdp_url","activate_tab","unattended"}` | Sent once. `unattended` is true on agent doors. `activate_tab` is true only when `browse_activate_tab` is on and `browse_cdp_url` is set; the sidecar reads anything but a JSON `true`, or an absent field, as false. |
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
| `browse_activate_tab` | Default false. Brings the lane's own tab to the front of its window once per run, so the page renders frames like a foreground tab (see [Background tab rendering](#background-tab-rendering)). Recommended for a dedicated agent browser. Honoured only together with `browse_cdp_url`: without it the setting is ignored (status says so, the load warns) and the lane runs as before, because activating the tab switches the window's active tab, which must never happen in the operator's everyday browser. |
| `browse_timeout_sec` | One run's wall budget. Default 300. |
| `browse_max_actions` | Default executed-action budget. Default 30, ceiling 60. |
| `browse_capture_dir` | Where redacted captures land. Empty means `<state_dir>/browse-captures`, or the OS temp dir. |
| `agent_allow_browse` | Node opt-in for the agent doors. Default false. |

- `BrowseConfigured()` is true only when `browse_python`, `browse_script` and a loopback
  `browse_decision_url` are all set. `offload_browse` is registered only then.
- `EffectiveBrowseActivateTab()` is what the harness sends as `activate_tab`: `browse_activate_tab` true and a
  `browse_cdp_url` the lane accepts. A true setting without that endpoint is reported as ignored
  (`BrowseActivateTabIgnored()`) and never fails the lane.
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
- `offload_status`'s `remote` block: `browse_configured`, `browse_decision_url` and `browse_activate_tab` (what
  the start line will carry when the lane runs, so the effective value, not the raw key; a
  `browse_activate_tab_note` appears when the setting is on but ignored, and says whether `browse_cdp_url` is
  missing or set but refused).

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
   or restarts the browser; register the dedicated port in your port inventory. On a dedicated profile that nobody
   looks at, also set `browse_activate_tab: true` (see [Background tab rendering](#background-tab-rendering)).
3. Restart the MCP client so `offload_browse` appears in `tools/list`.
4. For the agent doors, set `agent_allow_browse: true` on each node that may run one.

## Dependencies

Python 3.12 and `uv`; the two locked pins; a Chromium-family browser with remote debugging allowed;
the loopback decision endpoint; the local agent seat (for grammar-constrained field values).

## Downstream effects

`offload_status` (`remote.browse_configured`, `remote.browse_decision_url`, `remote.browse_activate_tab`), the MCP manifest
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
10. The lane's tab is activated only when the operator opted in (`browse_activate_tab`) AND pinned a dedicated
    endpoint (`browse_cdp_url`); never in a browser the lane found by discovery. An activating run first stops
    a leftover lane daemon, so the pinned endpoint is the browser it drives.

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
- A `finished N pending CSS animation(s) before observe` line in that tail means the lane un-stuck a fade-in
  in the background tab before reading the page.
- A `settled N.Ns after an input (P poll(s), M with page activity, K animation(s) finished)` line means the
  page was still reacting when the lane would have read it, and says how long the lane waited. No line means
  the page was quiet from the first poll.
- An `activated the lane's tab (activate_tab)` line in that tail means the lane brought its tab to the front at
  the run's first observe; `activate_tab skipped: ...` means it tried and the call failed (or the browser had
  no tab id), and the run went on with the tab in the background. No line means the setting is off or ignored,
  or that the installed sidecar predates 0.154.1 and does not read it (rerun `setup/browse/install.ps1`).
- Browse calls are audited by the policy broker on agent doors, and ledgered like other lanes.
- `offload_status remote` shows whether the lane is configured, which decision URL it will call, and whether
  the harness will ask the sidecar to activate its tab (`browse_activate_tab`, with a note when the setting is on
  but ignored). It reports what the start line will carry, not what the installed sidecar does: an older
  `runner.py` ignores the field, and status cannot see which one is installed.
- A missing `offload_browse` in `tools/list` means the lane is unconfigured or the client was not
  restarted.

## Testing notes

- Pipeline tests run a Go helper-process sidecar through the real protocol loop, with `httptest`
  servers for the decision endpoint and the local seat.
- The sidecar's pure helpers (deny regex, host check, header redaction, `DevToolsActivePort` parsing)
  are stdlib `unittest`, run by the installer: `cd setup/browse && python -m unittest -v test_runner`.
  The same file drives the observe and act wrappers through fake `Browser` classes, which is how the
  finish-before-every-read order (and its absence from act) is pinned without a browser, and, when `node`
  is on the PATH, runs the finish script against fake animations to pin which ones it finishes. The settle
  loop is driven with a scripted page, a fake clock and a fake sleep (its stop rule, its minimum, its cap, an
  unreadable page, a navigation), its counter script runs under `node` against a fake `MutationObserver`,
  and the wrapper tests pin that it runs once after an action, before jev's read and in the observed session,
  and never on the first observe, after a `wait` action or in act. The tab activation is pinned through the same
  fake `Browser`: exactly one `Target.activateTarget` per run, at the first observe and before the settle and
  jev's read, a browser-level call (no session) carrying the Browser's own target id, on the attended and the
  unattended door alike, nothing unless the start line carries a JSON `true`, and a failure swallowed, logged
  once and not retried. `main()` runs against fake jev and browser-harness modules to pin that an activating
  run stops the lane's daemon (only its own name) before the Agent starts, that a run that does not activate
  never does, and that a failed stop is logged and the run goes on.
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
- Writing a menu-then-confirm flow as one step of the goal. When the menu item and the confirm button of the
  dialog it opens carry the same label (an "Archive" item, then an "Archive" button in the dialog), the
  decision model reads the second click as the step it just took and chooses BLOCKED (measured: the run
  ended `blocked` until the goal named the confirm click as a separate required step). The form that
  measured best describes the dialog's question and says the task is not finished and not blocked while the
  dialog is open, for example: "Open the row's menu and click Archive. A dialog then asks whether to archive
  the item, with Cancel and Archive buttons. The task is NOT done and NOT blocked while that dialog is open:
  click its Archive button. The task is done only when the row is gone." Measured on a production web app's
  delete flow in a dedicated agent browser with `browse_activate_tab` on, each run checked independently: 5
  of 5 with that wording, about half with the step-list wording ("click its Archive button as a second,
  separate step"), and the failures of the step-list wording were the model choosing BLOCKED with the dialog
  fully visible, not a rendering problem. A similar flow, one label clicked twice in a row on purpose, is
  likely to behave the same way (not measured).

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
