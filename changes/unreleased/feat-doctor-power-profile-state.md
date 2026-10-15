### Added - doctor says whether the boot-time GPU power profile was applied this boot

- **The defect.** A box can apply a GPU power limit and a clock lock at boot with a separate tool. When that step is
  skipped, refused or blocked the card runs at stock limits, and the only record was in the tool's own log. Nothing in
  `doctor` or the fleet view said so, so a box could run for days at stock limits with every row green.
- **What it does.** `doctor` prints one `power:` row from the state record the tool writes on every run (refusals
  included). `OK` says a profile was applied since this boot began, how long ago and how many steps returned success.
  `WARN` says no profile was applied this boot (once the box has been up ten minutes; a box just started gets a `SKIP`
  that says the boot apply is not yet due), or names the outcome that left the profile out (dry run, refusal, failure,
  empty plan, the tool's crash guard, which says only that the run applied nothing and how each kind of cookie ends,
  never that the card is at stock limits), or says the apply was partial (a step short of the plan, a lever skipped
  other than by the tool's own design, a driver that differs from the profile's stamp). `UNKNOWN` names why the file
  cannot be trusted (unreadable, not JSON, another schema, no usable time) and is never reported as absent. No state
  file is one `SKIP` line saying nothing is recorded, not an error. The file is found only by
  `LOCAL_OFFLOAD_POWER_PROFILE_STATE`, which holds the path of the tool's state file; there is no platform default, so
  a box sets the variable and reads `SKIP` until it does.
- **What it does not do.** It reads the record, not the card: it does not compare the recorded limit and lock with the
  driver's live values, because the harness has no reader of a card's power limit or clock lock. That comparison, and a
  `power_profile` block on `/fleet/health` so the delegator and `offload_status` can show it for every node, are the
  follow-ups. The row is informational like the capacity row and never changes doctor's exit code. It prints no path,
  host name, card id or the tool's free-form detail.
- **Tests.** Written red first (a stub row that prints nothing, 12 failing tests), one per behaviour: absent file, whole
  apply this boot, last record older than the boot, boot apply not yet due, every outcome that leaves the profile out,
  partial applies and skips by design, unknown boot time, untrustworthy files, the record shape the tool really writes,
  the path resolver, the boot-time reader on Windows and Linux, the age text, and the doctor registration. Seven mutations
  of the row (age truncated not rounded, by-design skips counted, boot check off, grace off, absent read as unknown,
  driver mismatch ignored, dry-run flag ignored) each fail at least one test. A second pass added tests for the
  `cookie_blocked` wording, the filter that keeps a record's free text and control characters out of the row, the skip
  list read as an array, a bare string and null, the clock slack (inside it, at its edge and past it), a record with no
  `schema_version`, a timestamp with stray whitespace, the ten-minute boot-apply boundary and the read-failure reason;
  nine more mutations of the row (outcome filter off, plain list decoder, slack zero, slack widened, slack bound
  exclusive, schema-less message folded, timestamp trim off, grace bound inclusive, permission message dropped) each
  fail at least one of them.
- **Docs.** `docs/OPERATOR-GUIDE.md`, next to the capacity and `lan ip:` rows.
