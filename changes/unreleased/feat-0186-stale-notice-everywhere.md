### Added - Every tool result of a stale MCP server carries the stale-binary notice, a stale server's read no longer blocks a rollback, and the deploy report lists the sessions still on an old image

- **The gap.** An MCP server keeps the executable image it started from, so a rename-swap deploy leaves every session that was
  open before it on the old build. The stale-binary guard told a session on three doors only (`agent_delegate`,
  `offload_research`, `offload_status`); a session that called `offload_summarize`, `offload_ask`, a media door or any other was
  never told, and went on stamping ledger rows with old behaviour for as long as its client stayed open.
- **The notice.** `staleNotice` (`internal/mcpserver/stale_notice.go`), a receiving middleware registered after the config gate,
  appends the very note `offload_status` carries under `stale_binary` as a second text block (`stale_binary: <note>`) to the
  result of every other `tools/call`, on every call (no cap, no config key), with `Content[0]` untouched so the door's JSON
  still parses for a client that reads only the first block. The three doors that carry the key themselves are passed through
  before any check (a test reads the call sites in `mcpserver.go` and pins the set), the result is copied before the block is
  appended (a handler may keep what it returned), and a failed call, a result with no content and a typed-nil result pass
  through as they are. A guard that is not watching rides the same way as `stale_binary_guard: ...`, on the first call that
  finds it so and then on every 20th, because that is a standing condition of the process and not news. The note now names
  `/mcp` (select `local-offload`, Reconnect) beside the restart, the way out that works for a stdio server the client does not
  reconnect by itself. **Reach:** a server that started before this release has no middleware, so only a reconnect or a restart
  of the client puts it on this one; the first real proof is two deploys away.
- **The read.** The installed executable is hashed, and its build marker read, through `gpulease.OpenShared`, which grants
  `FILE_SHARE_DELETE` on Windows (Go's `os.Open` grants only read and write sharing); the audit's own opener (`openAudited`, which
  `gpu doctor` reads every backup through) is the same one. With the notice on every door, the first call of any door on a stale
  server reads the freshly installed file, and a read that did not share delete made a rollback or a second swap landing in
  that window fail on a sharing violation, with node-swap's holder diagnosis unable to see the stale server (its image path
  had been renamed away).
- **The record.** ADR 0085 (status Proposed) records the decision: the stale server is told, not replaced; a stable stdio shim
  and a loopback HTTP endpoint the client reconnects by itself were weighed and neither is built, with the Windows rename and
  lock facts, an acceptance matrix and a go/no-go gate (two deploys with sessions still stale after a day despite the notice).
  `docs/systems/mcp-server.md` documents the notice, the shared read and the rule for counting a rollout: only ledger rows whose
  `harness_build_sha256` equals the deployed build's hash. The operator's deploy script also reports, at the end of each Windows
  leg and in its deploy record, the MCP servers it finds on an image older than the deployed build (pid, start time, session
  when the registry names it); that report is information only, it never changes a node's verdict or stops a process, a check
  that could not run says "not counted" and never 0, a Linux leg says "not probed", and a dry run says what a swap would leave.
  The script lives outside this repository.
- **Not changed.** Nothing is interrupted and nothing is killed. No tool is added or removed (`tools/list` is byte-identical on a
  stale server, pinned), and a client that reads only the first content block sees exactly what it saw before. How each client
  renders a second block is for the release's live check to record; it is not assumed.
- **Red first, then proofs.** The tests were committed before the code and failed on their assertions: eleven in
  `internal/mcpserver` (every other door answered with one block after a swap, for 28 doors; no notice on every call; the error
  result, the shared result, the config deferral, the unreadable stamp, the guard cadence, the verbatim note, the stalled read
  and the `/mcp` wording all missing), and on Windows two in `internal/gpulease` (the installed image could not be renamed aside
  while a reader held it open, through `OpenShared` and through the audit's opener). Twenty-one single-line mutations, each run
  through `scripts/mutation-check.sh` and each killed (RED): the registration dropped; the registration made conditional on a
  valid config; the skip set widened to every door; one door left out of the skip set (twice: by the behaviour tests, and by the
  call-site pin alone); the stale condition removed; the notice capped at the first three calls; the typed-nil guard removed;
  the no-content guard removed; the result appended to in place instead of copied; the error flag lost in the copy; the guard
  block on every call; the guard block dropped; the note not verbatim; a stamp read before the skip; the wording without `/mcp`;
  a note that ranks two builds of one version; the share-delete bit removed; the audit opener back to `os.Open`; the
  installed-image opener back to `os.Open`; the hash opening the file directly.
- **The deploy script (outside this repository).** Its new mock tests were written against a behaviour-free seam of the old
  script first and failed on their assertions (34), then passed against the edited one together with the whole existing mock
  suite and the script's own `--selftest`, which also runs the snippet for real against a hidden dummy MCP server (listed by
  image path, by start time and by neither; the session joined from an owners registry; a bad swap time answers `ok=false`).
  Fifteen single-line mutations of the script, each killed: either leg's report call dropped; a failed check read as 0; the
  report setting a node's status; a dry run run as a real one; a dry run handed a swap time; either leg counting after a swap
  that did not complete; the swap time not passed; the wrong exe base name; the section's node line dropped; the `summary.json`
  count wrong; the record's section dropped; the snippet's image-path rule dropped; its start-time rule flipped.
