### Added - An overdue lease's holder can be asked to yield (`gpu_on_overdue = request`): it runs the job's own `on_yield`, and nothing is killed, released or taken

- **The gap.** A lease that outlives its declared window while its holder is alive is surfaced as `held-overdue` and nothing else
  happens to it. Somebody waits for the card, the holder is overdue (its term was not renewed, or it reached the maximum total), and
  the only recourse was a human running `gpu release --epoch N`. For a wrapper that command is a hazard: the claim goes, the
  wrapper's next tick prints `LEASE LOST` and stops its own job within about 15 seconds while the card is already free and a second
  job can be loading onto it. For a detached job it frees the claim and leaves the job running on the card. Jobs already record, at
  the start, what they would like to happen when asked to stop (`--on-yield`, `--yield-grace`), and until now nothing read it.
- **The policy.** One flat key, `gpu_on_overdue`. **Unset keeps today's behaviour: nobody is asked.** `request` asks the lease's
  holder. `hold` says in words that nobody is. The value is trimmed and lower-cased; any other (`yield`, `kill`, `takeover`, a
  typo) is refused by name when the config loads, and, because a one-shot verb goes on after a load error and the load installs
  the policy on every exit, it is installed as `hold`, the direction that asks nobody; the holder's first tick says why no request
  is made. **There is no automatic mode:** no timer, no daemon and no waiter acts on a lease; only the holder's own tick can ask. The
  plan names `request` as the default; the operator decided that asking is opt-in, so the default stays what it was (ADR 0085 records
  the deviation).
- **Who asks, and when.** The holder's own tick (the wrapper of `gpu reserve`, and the detached holder of `gpu reserve --detach`),
  not a blocked waiter. The preconditions are checked cheapest first, from the lease record alone: the policy is `request`; the
  record is labelled expired or is past its declared end (a lease the same tick renewed is not asked); the holder was not asked
  yet (an in-memory flag, so a vanished file cannot make it ask twice); the record says this holder runs the request; and only then
  are the waiters read. Demand is a live waiter that is not the holder's own process, not waiting only on host RAM, and wants a
  card the lease holds. **The same waiter (pid and start identity) must be seen on two consecutive ticks**, so a short probe
  that comes and goes cannot stop a long job through its `on_yield`; the persistence is an in-memory counter on the holder's own
  cadence and adds no time constant.
- **The request.** A sidecar beside the lease record, `yield.<epoch>`: when it was asked, by which waiter pid, why (the basis in
  words), up to three waiters and the queue length, the policy, the job's declared grace, and a second write of what the holder
  did about it (`on_yield running`, `ran on_yield`, `on_yield failed` with the exit status, or `no on_yield recorded`). It is
  written under the epoch lock after the writability probe and the liveness check the orphan marker uses, with tmp and rename; both
  the ask and the later action update check the lease is still live, so a late update cannot bring the file back after a release;
  both sweeps that end a lease remove it. A binary that predates the request ignores it (the preserved old-reader fixture pins that).
  If the ask cannot be recorded nothing is run, and the holder says so once.
- **What the holder does.** It runs the `--on-yield` it was STARTED with, from memory, never the line the record holds (the record is
  shared and can be rewritten), once, in its own working directory, through `sh -c` (or `cmd.exe /d /s /c` hidden on Windows), with
  its output discarded (a wrapper's stdout is the job's), in a goroutine so the heartbeat goes on, and never waited out or cut short.
  The wrapper's command is told `GPU_LEASE_YIELD_FILE`, the path of the sidecar (which exists only once the holder is asked), so a
  job can poll it between items; the line itself gets `GPU_LEASE_EPOCH`, `GPU_LEASE_DIR` and `GPU_LEASE_YIELD_FILE`. A detached job is
  not the holder's child, so `gpu reserve --detach` prints the path (and `yield_file` under `--json`), and `gpu status --json` carries
  it. **A detached holder never returns on a yield:** its job is a separate process, and releasing would free the card under a live
  job (the 2026-09-07 incident). A holder started with `--release-at-expiry` stamps nothing and is never asked.
- **A record gains an additive `yields` stamp** (`wrapper` or `detached`), set at acquire by a holder that runs the request. The
  version cannot say whether a holder would answer (none of the older builds ticks this), so a record without the stamp reads, on
  every surface, as "its holder build predates the yield request: ask whoever owns it", and is never asked.
- **What a waiter and a status call read.** Read-only everywhere (a test pins that no status surface writes the sidecar):
  `ExplainHeld` (the refusal text of a blocked request), `gpu status` and its `holder.yield` object (asked at, waiters, grace, the
  action, the exit status, whether the ask is unanswered), the one-line brief, and the overdue, stalled and orphaned notes all say
  what became of the request. **No sentence aimed at a waiter names `gpu release --epoch N` for a lease whose wrapper is alive.** For a
  wrapper the words are: nothing is killed; ask whoever owns it, or stop its job by hand (the wrapper's pid); the claim frees when
  the wrapper ends. Only a detached holder's sentence names the release command, with what it does: it frees the claim at once, and
  the job keeps running on the card until it ends. An ask is called unanswered once the lease still holds its cards past the job's
  declared grace, or past a display-only ten minutes when the job declared none; nothing keys off that figure, which is chosen and
  not measured.
- **Not changed, on purpose.** Nothing is killed, signalled, released or taken over: a static test reads the source of the yield files
  and fails on a kill, a signal, a process-tree stop, `taskkill`, `os.FindProcess` or a second process start. A claim whose holder
  is alive is never freed by the harness. The term check still renews or labels and never frees. No `gpu yield` or `gpu takeover` verb
  is added, and plan P12 (claim-until-dead routing, `--abandon-tree`, quarantine) is not part of this change.
- **Limits, stated.** (1) A hung job that ignores the request keeps its card: the lease stays `held-overdue` with its heartbeat and
  fence intact, and only its holder ending or a human acting frees it. (2) A progressing job is not asked: under the term rules a
  renewal moves the declared end, so only a lease labelled expired or one that reached `gpu_max_total_min` is asked. (3) The request is
  node-local: the fleet node's lease standing wire does not carry it. (4) A seat waiter registered for the whole node (the card table
  was unreadable when it queued) conflicts with every lease and can ask an expired lease on a card it does not need; the two-tick
  rule bounds that. (5) A holder started before the upgrade carries no stamp and is never asked, so a host is covered only once
  its holders are this build; restart the long-lived processes so their surfaces say the same thing. (6) A holder whose command ends
  before its `on_yield` returns leaves the action at `on_yield running`; the file goes with the lease.
- **Record and docs.** ADR 0085 (status Proposed) amends plan invariant I5 and the plan's description of the takeover: here the holder's
  own tick runs the job's own pre-declared `on_yield`, instead of a takeover command running it. The file-only alternative (the holder
  writes the sidecar and the job polls it) is a one-line change in the ticker, and the choice between the two is the operator's to
  confirm. `docs/systems/gpu-lease.md` has the new section, the corrected `on_yield` / `yield_grace` rows (they were described as
  read by the takeover command), the `yields` row, the source map and the held-overdue verdict row; `docs/OPERATOR-GUIDE.md` has the
  sentence; `--on-yield` and `--yield-grace` help say who runs the line; `config.example.json` carries the key (the generator
  and the file agree).
- **Red first, then proofs.** A seam commit added only the types and signatures (the policy parser returning the unset value, the
  asker never asking, the view empty, the shell runner doing nothing); the tests were committed next and failed on their
  assertions, then the code followed. Recorded failures: `ParseOnOverdue("request") = "", <nil>; want "request"`, `ParseOnOverdue("kill")
  accepted a value that is neither request nor hold`, `the holder of an overdue lease with a waiter on its cards was not asked after
  two ticks`, `installed "kill" reads "", want hold`; in `internal/config` `request: Load installed "", want "request"` and `"yield"
  was accepted: there is no automatic mode, and a typo must not pass`; in `internal/gpuactivity` `yield_file = "", want ...`, `note lacks
  "was asked to yield"` and `note lacks "gpu release --epoch 41"`; in the root package `on_yield ran 0 times, want exactly once`,
  `a detached holder stamps its form: ""`, `the report lacks "release with: local-offload gpu release --epoch 9"`, `--on-yield help lacks
  "gpu_on_overdue"` and `exit 0, err <nil>: want the line's own status 3 and no error`; and in `internal/mcpserver`
  `the assessed note carries the yield sentence verbatim`. Two fix-ups of the tests themselves came after the red run and are separate commits with their reasons (the wrapper
  was queued behind the waiter the test planted too early; the policy has to come from the config file the verb loads, because every
  `gpu` verb re-installs it). **Tests that were NOT red first, and the history says so.** Four absence assertions passed on the behaviour-free seam, as an
  assertion that nothing happens must: `TestTickerSilentUnderTheDefaultPolicy`, `TestStatusNeverWritesTheYieldSidecar`,
  `TestGPUOnOverdueUnsetKeepsTheBehaviourFromBeforeTheKey` and `TestDetachHolderThatReleasesAtExpiryRunsNoRequest`. Four more were
  written after the red commit and passed on their first run against code that already did the right thing:
  `TestAskedOnceEvenIfTheSidecarVanishes`, `TestATreeOrphanHasNoHolderToAsk`,
  `TestGPUReserveUnderAnInvalidPolicyStillRunsAndAsksNobody`, and the strengthening of `TestLateActionAfterReleaseLeavesNoFile`
  (it re-plants the file after the release, because the first version passed even with the liveness check removed). Each of the eight
  is proven by a named mutation that makes the forbidden thing happen, below.
  Sixty-six single-line mutations against the final tree, each run through `scripts/mutation-check.sh` (it proves the line landed alone,
  that the unmutated tree passes first, that the mutant compiles, and that the file is restored byte for byte), and each `RED`: **in
  `internal/gpulease` (37)** a lease still inside its window asked, a renewed lease asked, a waiter on another card counted as
  demand, a host-RAM-only waiter counted, the holder's own pid counted, an unset policy asking (twice: by the sidecar tests and by the
  ticker test), a hold or unrecognised policy asking, the ask repeated when its file vanished, two askers both writing, a holder
  without the stamp asked, the two-tick rule dropped, the ask touching the claim, each of the two release sweeps leaving the
  sidecar behind, a late action update bringing the file back, an ask made when it could not be recorded, the record's `on_yield`
  run instead of the holder's own, `on_yield` run on every tick, run in line with the heartbeat, a failure not recorded, a missing
  `on_yield` not said, the unrecognised-policy note said every tick, the failure-to-record note said every tick, a kill call added to
  the request files (the source pin), the refusal text of a blocked request without the sentence, a live wrapper's sentence
  naming `gpu release`, a detached holder's sentence naming it without what it does, the unanswered threshold ignoring the
  declared grace and then the display figure, a holder without the stamp not said to predate the request, the hold policy not said,
  the stamp not written to the record, not mirrored by `Info`, the parser accepting a value that is neither `request` nor `hold`, an
  unrecognised value installed as unset, the value not trimmed and lower-cased, and an old reader tripping on the new field and
  sidecar; **in `internal/config` (6)** the install behind another key's refusal, `Load` not installing, an unknown value not
  refused by name, an unset key reading as `request` (by the accessor and by a stale earlier load), and the accessor not falling back
  to `hold`; **in `internal/gpuactivity` and `internal/mcpserver` (7)** the holder without its yield file or sentence, the standing
  notes without the sentence, `holder.yield` absent once asked, its `unanswered` flag, a tree-orphan reported as awaiting an answer,
  the brief clipped over the sentence, and a status snapshot that writes the request; **in `internal/winexec` (1)** the runner
  started through a raw `exec.Command`; **in the root package (15)** the wrapper's command not told the yield file, the wrapper not
  ticking, the detached holder not ticking, the detached holder returning on a yield, the wrapper running the record's `on_yield`,
  the wrapper not stamping its form, `--release-at-expiry` stamping, the detached form not stamped, a Windows command line that
  mangles a quoted path, the lease environment missing, the line's exit status lost, the detach report (text and `--json`) without
  the yield file, the help text not saying who runs the line, and a verb under an unrecognised `gpu_on_overdue` falling back to
  `request`. First attempts that did not count: one mutant survived (the liveness check in the late action update was redundant
  while the release sweep had already run, so the test was strengthened, see above), and three first attempts did not compile
  (BAD-MUTANT) and were replaced by mutants that build; none of those is in the sixty-six. One redundancy was left alone on purpose: the unlocked stat
  before the epoch lock in the asker is only a shortcut, and the in-lock re-check is what is pinned.
