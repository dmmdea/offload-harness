### Added - offload_ask retries a failed local first attempt on another seat

- **The defect.** `offload_ask` had no cross-seat retry: a first attempt that ran on this box's seat and failed its
  generated acceptance was final, while `agent_delegate` retries a failed verification on another seat. The run, the
  grading and the publish all live in the ask handler, so nothing downstream ever gave the ask its second chance.
- **What it does.** When the first attempt ran locally and `acceptance_failures` is non-empty, the handler sends the same
  contract once through the delegator at `route=remote`, which never places on this box's seat, so the retry runs on a
  different seat by construction. The retry is decoded and graded exactly like the first attempt (the decode-and-grade
  step is now `gradeAskWire`, shared by both) and only a clean one is published: a verified retry is the answer, with
  `retried_on` and a `retry_note` naming the first attempt's verdict; a retry that also fails leaves the FIRST answer,
  `verified: false`, with `retried_on` and a note that the retry also failed; a fleet that takes nothing leaves the first
  answer with `retry skipped: <why>` and no `retried_on`. A deferred first attempt and a routed ask are unchanged (the
  routed path's engine already retries). The stored cache entry is the answer as published, so a cached unverified
  answer already includes its retry and a repeat never retries again.
- **The retry floor.** The retry lives inside the contract's own `timeout_sec`, as the delegator's does: it is skipped,
  with a note, when what the first attempt left of the 900 s an auto-sized ask owns is under the retry floor
  (`agent_retry_min_sec`, at least 10 s), and otherwise runs with what is left as an explicit `timeout_sec`.
- **A routed fleet result now says it retried.** `contractOnFleetAt` dropped the delegator's `retried_on` and
  `retry_note`, so a retry the delegator ran inside a routed call (and one it considered and skipped) never reached the
  response. Both fields are surfaced now on `offload_ask`, `agent_run` and the review lane, additively: they are absent
  when no retry was considered.
- **Tests.** `ask_retry_test.go`, red before on the unchanged handler: a clean retry is published with `retried_on`; both
  attempts failing publishes the first answer with an "also" note and caches it as published; a declining fleet leaves
  `retry skipped` and no `retried_on`; a verified first attempt never reaches the fleet (the guard, green before); the
  retry floor skips a retry with a note and lets a configured floor through when enough is left; the fleet's own retry
  note is kept after the ask's; and a routed fleet result carrying a retry surfaces both fields.
