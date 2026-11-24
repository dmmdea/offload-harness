### Changed - an idle MCP server whose image changed on disk exits by itself, so the client starts the installed build

- **What was wrong.** A deploy replaces the installed binary by rename, and every running `local-offload mcp` server keeps the
  image it started from. On one box, 2.5 days after two releases, 36 such servers were running: 7 from a build two releases old,
  29 from the previous one, and none ran the installed build, so a fix that had shipped reached no session. The server already
  detected this (`process.stale_image`, the `stale_binary` and `stale_config` notices) and only said so, and the way out it named,
  `/mcp` and Reconnect, is one nobody takes across 36 sessions.
- **What it does now.** A server that finds its image stale returns from `Run` by itself when it is idle, and the client starts the
  installed build at its next tool call (with client version 2.1.330.0, a stopped stdio server was started again at the next call
  and answered by a new process on the installed build, with no error reaching the model). It acts only for a client on a small
  constant list (`claude-code`, from the `initialize` clientInfo, case-insensitive) at version 2.1.330 or later, and only while the new key
  `mcp_exit_when_stale` is not `false` (default on). It exits only on a definite stale verdict, with the installed file at least
  10 s old, no `tools/call` in flight and none finished in the last 5 s, the process up at least 60 s, and nothing that outlives
  a tool call depending on it: no media ticket the process runs, and no live GPU lease owned by the session it serves (the
  process is that session's entry in the session registry, and a lease whose session has no live process reads as orphaned
  after the grace, with the takeover command shown to other sessions). The check runs every 30 s and shortly after each tool
  call. It logs one line to stderr saying why, runs its deferred cleanup (the
  session-registry entry is removed) and exits with code 0. `offload_status`'s `process` block gains `client_name`,
  `client_version` and `exit_when_stale` (`armed`, `armed, held by: <what it still owns>`, or `off: <why>`). The stale-image
  notice says the server exits by itself for an armed server and keeps the manual `/mcp` instruction for any other.
- **What did not change.** Any other client keeps today's behaviour: the notice and nothing else. A call in flight is never cut. A
  server never exits on an unreadable installed file or without a startup identity. A changed CONFIG file still only gives its
  notice: the one check that a config loads installs process-wide gates as it runs, so a running server does not make it.
  `mcp_exit_when_stale: false` restores the notice-only behaviour. No other key, tool or answer changes.
- **Check.** The client's side, with the released build: one stale server was stopped, the next tool call of that session was
  answered by a new process on the installed build (`stale_image` false), and no error reached the model; the client's own
  binary (2.1.330) constructs its MCP client as `name: "claude-code"`. The server's side, this build over real stdio, named
  as that client: `exit_when_stale: armed` when fresh. With its executable replaced by other bytes and a lease held from the
  same session it answered `stale_image: true`, `exit_when_stale: armed, held by: the session this server serves owns a live
  GPU lease (text, epoch N)` and was still running 75 s in, past the uptime gate and two checks. The lease was released at
  83 s and the process exited by itself at 91 s with code 0, logging `exiting by itself (client claude-code 2.1.330): this
  server runs X and the installed build is Y; no call is in flight, so it exits now and the client starts the installed build
  at its next call`.
