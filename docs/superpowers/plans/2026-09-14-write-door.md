# A write-capable delegation door, default off (register D-06)

Status: design accepted, implemented in 0.122.0. Date: 2026-09-14.

## What exists today

`local-agent --allow-write` is the ONLY write door and it is CLI-only
(`cmd/local-agent/main.go:139`). It grants `write_file` / `edit_file` /
`delete_file` (`internal/agent/writetools.go`) confined to the worktree by
`os.Root` — kernel-level, fail-closed on `..`, absolute paths, symlinks and
Windows junctions — and brokered by the deny→ask→allow `Policy`
(`internal/agent/policy.go`), which on an unattended run resolves every `ask`
to deny-and-queue. `internal/sandbox` (Landlock+seccomp+userns on Linux, job
object + low-integrity token on Windows) is the cage for `run_shell` / `run`,
NOT for the file tools.

The delegation lane has no write at all. `core.AgentContract`
(`internal/core/agentwire.go`) is self-contained by construction: context docs
travel INLINE and the node never reaches back to the delegator's filesystem.
`fleetnode.buildAgentRun` materializes those docs into
`<base>/pipeline-jobs/agent-*/context/` and `pipeline.runAgentTask` builds the
loop with `ReadRoot: contextDir` and no write/run/fetch/github capability, then
re-packs the final text through the contract's `output_schema`. The result is a
typed `core.AgentWireResult` — no transcript ever crosses back.

## The door

**`write_root` is RELATIVE to the run's read root, not absolute.** The brief
asked for "an absolute dir inside `read_root`". On the wire an absolute
delegator-box path is meaningless: the node materializes its own copy of the
docs and has never seen the delegator's filesystem. A relative path resolves to
an absolute dir inside the read root on whichever box runs it — the invariant
the brief wanted — and it is enforced by `os.Root` rather than by comparing two
strings. `""` = read-only (today, byte-identical). `"."` = the whole read root.

- **Contract**: `write_root` only. Validated by `ValidateWriteRoot`: relative,
  no volume, no NUL, no `..`, no trailing space/dot segment, no Windows device
  stem, no `.git` segment — the strictest platform's rules on every platform,
  the same reason `validDocName` is strict everywhere.
- **Node opt-in**: `agent_allow_write`, default FALSE. A node that has not
  opted in refuses a write contract at ACK (`buildAgentRun` → 400, so the
  delegator RE-PLACES it on a node that has), and the in-process local door —
  which has no ack hop — defers with the new `defer_class: "write"`.
- **Caps** (constants, not contract fields): 8 files, 64 KiB written, 128 KiB
  of diff. Enforced TWICE: `agent.WriteLimit` refuses the offending call at the
  tool (the model sees "NOT performed" and can correct), and the post-run diff
  re-counts. A cap breach publishes NO diff and defers `write` — publishing a
  capped-out diff would defeat the cap.
- **The write set comes back as a unified DIFF**, never applied. The node
  snapshots `write_root` before and after the loop and diffs the two trees
  (`internal/writedoor`). `diff` + `diff_files` + `write_note` ride the wire
  result and the delegator's response. The harness applies nothing, ever.
- **Writes only.** The door builds with `AllowWrite` + `AllowOverwrite` and
  NOTHING else — no shell, no `run`, no fetch, no github, `AllowDelete` false —
  and narrows the advertised tools to the `edit` profile, which does not list
  `delete_file`. Overwrite is ON because a seat that cannot change an existing
  file cannot do an implementation leg at all; it is safe because the tree it
  overwrites is the node's own throwaway copy.
- **Acceptance**: `diff_touches:<path-prefix>` and `diff_max_files:<n>`. Both
  fail closed on an empty write set — an unmet precondition is a failed check,
  never a skipped one (the existing field-verb rule).

## Cut, with reasons

- **Per-contract cap overrides.** Looser is clamped away; tighter is already
  expressible as `diff_max_files:<n>`. Two knobs for one job.
- **A new `write` profile.** `edit` already lists exactly the right six tools.
- **Advertising `agent_allow_write` on `/fleet/health`.** It would be the
  honest way to route a write contract only at opted-in nodes, but it changes
  the health wire that `healthwire_compat_test.go` pins across a staggered
  fleet. The ACK refusal + re-placement already reaches the right node; the
  cost is one wasted round trip on a mixed fleet. Revisit if it bites.
- **A node-side audit path.** Build would take one, but the whole job dir is
  removed at cleanup, so the trail dies with the run. The diff IS the record,
  and it crosses the wire to a human.

## Roast

- **What a 4B will do wrong.** It will describe the change in prose and never
  call `edit_file`; it will call `write_file` on a file that already exists and
  eat the O_EXCL re-broker; it will reproduce `old_string` with different
  indentation (the whitespace-tolerant fallback exists for exactly this); it
  will "fix" the off-by-one by rewriting the whole function; it will touch a
  second file nothing asked about. Every one of those is VISIBLE in the diff,
  which is why the diff and not a success flag is the deliverable.
- **What proves it worked**: the operator reads the unified diff. Nothing else
  counts. `verified: true` on a write contract means a path was touched and the
  seat said something — never that the change is right.
- **The lint still calls a diff-only acceptance SHAPE-ONLY, deliberately.** A
  touched path is not a correct change. That warning is accurate and stays.
- **What the door must refuse**: a `write_root` outside the read root; any
  write at all on a node without `agent_allow_write`; `.git` under any casing;
  delete; shell; more than the caps; and a diff it cannot bound.

## Residual risk, stated

The confinement is `os.Root`, the same primitive the read tools have always
used — not Landlock and not the Windows job object, which cage a CHILD PROCESS
and have no bearing on in-process file writes. `os.Root` is kernel-enforced on
both platforms and fails closed; the Linux/Windows split in the tests is about
proving that on each OS, not about two different mechanisms.
