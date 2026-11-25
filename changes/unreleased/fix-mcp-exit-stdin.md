### Fixed - an idle MCP server that leaves after a deploy really leaves, and costs its session no call

- **What was wrong.** Since 0.238.0 an idle MCP server whose installed binary changed exits by itself and the client starts the
  installed build at its next call. With the client it was built for it did not: a client of the per-request protocol (2026-07-28,
  spoken by claude-code 2.1.330) holds one request open for as long as it is connected, `subscriptions/listen`, and the SDK's `Close`
  waits for open requests and cancels none. A server that had decided to leave stayed, half closed, until its session's next call,
  answered it with "server is closing", and ended on it: the session saw `Connection closed`, once per open session and deploy. Seen on
  the reference box after the 0.239.0 deploy: ten idle servers, none of them gone two minutes later, and the next call of one session
  lost. The check that shipped with 0.238.0 drove the older `initialize` handshake, which opens no such request.
- **What it does now.** The server ends its listen requests when it decides to leave, which is the answer the protocol has for a
  server that shuts down, so the session closes and the process ends at once. `serve` gives the SDK a second and then returns
  whatever it still waits for: the server is idle, so there is nothing left to answer.
- **Check.** The real executable over stdio under a Node client, replaced on disk under it. Per-request protocol with the listen
  request open: 0.238.0 logged that it was exiting at 61.1 s, was still running 75 s later, and the tool call sent then came back as a
  closed connection; this build's process ended in the instant it decided to (61.2 s, exit code 0). The older handshake: both end at
  once. `TestAnIdleStaleServerExitsOnlyForAListedClient` now connects the way the real client does, and fails on the code before.
- **What it cannot do.** A server of 0.238.0 or 0.239.0 that is running when this build is deployed still has the old end: its session
  loses one call, or the server is stopped by hand.
