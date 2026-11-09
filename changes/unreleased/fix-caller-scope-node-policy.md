### Fixed - a node that must only take its operator's personal work can no longer be given another caller's

- **What happened.** On a box that runs work for a business and the operator's own work side by side, a business
  session's speech-to-text (`offload_transcribe route:"remote"`) and image work was placed on a fleet node that must
  never hold that business's data. Nothing in placement could tell the two callers apart: the only per-node rule
  (`delegate_remote_classes`) is enforced in the agent lane alone, the single-shot lanes (stt, vision, text, forecast,
  compose, media) never consult it, the cascade's `cascade_remote_lanes` and `seat_endpoints` send completions to a
  node outside `delegate_remotes` altogether, and the media ticket path re-reads `delegate_remotes` from the config file,
  so a filter applied at load would have been undone by the next re-read.
- **What changed.** Each process now resolves its caller scope once, when its config loads (`internal/callscope`):
  `LOCAL_OFFLOAD_SCOPE` if set, else the working directory of the Claude Code session it serves (read from the
  session's file under `~/.claude/sessions`, by the parent's pid or the session id), else its own working directory,
  judged against the new `caller_scope_roots` by longest prefix. The new `delegate_remote_scopes` names which scopes a
  machine admits (keyed by host, so one entry covers its fleet port, seat endpoints and cascade lanes), and the load
  removes every machine that does not admit the caller from `delegate_remotes`, `cascade_remote_lanes`,
  `seat_endpoints` and `fleet_queue_holder` at once, on every exit of the load, so no lane and no verb can reach it.
  The media roster re-read applies the same filter (`Config.WithRoster`). A call that names an excluded machine in
  `remotes` or `--remote` is refused with the node's policy and the caller's scope. The rule fails closed: a caller
  whose scope cannot be established (no session file, a directory under no root, a misspelled override) is admitted
  by no machine that lists scopes. A config without the new keys is unchanged.
- **Node side, a backstop.** A node with `fleet_admit_scopes` refuses, with a typed 403 (`scope_not_admitted`)
  before it reads a body, any job whose asker does not declare an admitted scope in the new `X-Offload-Scope`
  header, on every door through `authorizeGated` and on the cascade chat lane, and publishes the list as
  `admit_scopes` in `/fleet/health`. Askers send the header on every fleet request, cascade lanes included, only for a
  known scope.
- **Where it shows.** `doctor` prints `caller scope:` and every excluded node; `offload_status` carries
  `fleet.caller_scope` and `fleet.scope_excluded`; ledger rows carry `caller_scope`.
- **Check.** One test pins the contract (every list filtered for business and unknown callers, the roster re-read
  cannot restore the node, naming it is refused, a missing session file never falls back to the process's directory),
  and a real run on the delegating box: `doctor` from a business session's working directory lists the node as
  excluded, and from the operator's own working directory keeps it. Details: `docs/OPERATOR-GUIDE.md` (the
  `delegate_remote_scopes`, `caller_scope_roots` and `fleet_admit_scopes` rows), `docs/systems/fleet-node.md`
  ("Caller scope").
