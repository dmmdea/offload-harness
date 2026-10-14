### Added - Every tool result of a stale MCP server carries the stale-binary notice, a stale server's read no longer blocks a rollback, and the deploy report lists the sessions still on an old image

- **The gap.** An MCP server keeps the executable image it started from, so a rename-swap deploy leaves every session that was
  open before it on the old build. The stale-binary guard told a session on three doors only (`agent_delegate`,
  `offload_research`, `offload_status`); a session that called `offload_summarize`, `offload_ask`, a media door or any other was
  never told, and went on stamping ledger rows with old behaviour for as long as its client stayed open.
- **The notice.** `staleNotice` (`internal/mcpserver/stale_notice.go`), a receiving middleware registered after the config gate,
  appends the very note `offload_status` carries under `stale_binary` as a second text block (`stale_binary: <note>`) to the
  result of every `tools/call` that does not carry the signal already, on every call (no cap, no config key), with `Content[0]`
  untouched so the door's JSON still parses for a client that reads only the first block. The decision is the result's, not
  the door's name: the three doors that carry the key in their own answer (`agent_delegate`, `offload_research`,
  `offload_status`) do so only on the path that ran the work, so a first block that is a JSON object holding `stale_binary` or
  `stale_binary_guard` at its top level is left alone with no check, and every other result gets the block, those three doors'
  refusals (bad arguments, no subtask, no url, an unknown status section) and the config gate's deferral included. A summary
  that merely quotes the key is still told. A test reads the call sites in `mcpserver.go` and keeps the list of the keyed doors
  in step, the result is copied before the block is appended (a handler may keep what it returned), and a failed call, a result
  with no content and a typed-nil result pass through as they are. A guard that is not watching rides the same way as `stale_binary_guard: ...`, on the first call that
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
  It compares start times with the swap's own time (the `install-new` stamp node-swap writes, so a server started while the
  node waited for idle, before the swap, is still told apart); only a result without that stamp falls back to the preflight
  clock, and the report line then says its start-time check is partial. A deploy record's `summary.json` is written even if
  the report's own file is unreadable (the field is null). The script lives outside this repository.
- **Not changed.** Nothing is interrupted and nothing is killed. No tool is added or removed (`tools/list` is byte-identical on a
  stale server, pinned), and a client that reads only the first content block sees exactly what it saw before. How each client
  renders a second block is for the release's live check to record; it is not assumed.
- **Red first, then proofs.** The tests were committed before the code and failed on their assertions, in two steps. The build:
  eleven in `internal/mcpserver` (every other door answered with one block after a swap, for 28 doors; no notice on every call;
  the error result, the shared result, the config deferral, the unreadable stamp, the guard cadence, the verbatim note, the
  stalled read and the `/mcp` wording all missing), and on Windows two in `internal/gpulease` (the installed image could not be
  renamed aside while a reader held it open, through `OpenShared` and through the audit's opener). The review of the build found
  that the three keyed doors answered a stale server with no signal on every refusal before the line that adds the key and on
  the config gate's deferral, because the middleware skipped them by name; its tests failed first (the eight early returns of
  the three doors and the invalid-config deferral of two of them answered with one block where two were wanted, and a result
  that carries the key at its top level was changed under another door's name), then the middleware was made to decide from
  the result. The proofs: twenty-three single-line mutations on the final code, each run through `scripts/mutation-check.sh`
  and each killed (RED): the registration dropped; the registration made conditional on a valid config; the key check removed;
  its cheap test misspelled; the guard key alone not recognised; a mere mention of the key counted as the signal; a first block
  that is not JSON counted as the signal; the old skip by name put back for one door; the stale condition removed; the notice
  capped at the first three calls; the typed-nil guard removed; the no-content guard removed; the result appended to in place
  instead of copied; the error flag lost in the copy; the guard block on every call; the guard block dropped; the note not
  verbatim; the wording without `/mcp`; a note that ranks two builds of one version; the share-delete bit removed; the audit
  opener back to `os.Open`; the installed-image opener back to `os.Open`; the hash opening the file directly. The Windows-only
  control (a plain `os.Open` blocks the rename that `OpenShared` allows) tests the platform and has no mutant in the code; as a
  test it dies when its opener is swapped.
- **The deploy script (outside this repository).** Its mock tests were written against a behaviour-free seam of the old
  script first and failed on their assertions (34), then passed against the edited one together with the whole existing mock
  suite and the script's own `--selftest`, which also runs the snippet for real against a hidden dummy MCP server (listed by
  image path, by start time and by neither; the session joined from an owners registry; a bad swap time answers `ok=false`).
  The review round added tests that failed first (nine assertions: the swap time that rides, its classifier, the fallback
  caveat, and a deploy record whose stale-mcp file is not JSON) and ran `write_record` for real, which no test had executed.
  Twenty single-line mutations of the final script, each killed: the swap time reverted to the preflight clock on either leg;
  the classifier reading node-swap's start, the wrong step, or leaving the fraction of a second in; the fallback caveat
  dropped or always off on either leg; no swap time passed on either leg; a dry run handed a swap time; either leg's report
  call dropped; either leg counting after a swap that did not complete; the record's merge raising on a bad file, writing
  nothing, or keyed on the wrong name; the record's builder writing an empty object; the snippet's date parser narrowed to the
  old offset-less format (killed by the `--selftest` lines that hand real PowerShell a stamp with an offset and one in UTC).
