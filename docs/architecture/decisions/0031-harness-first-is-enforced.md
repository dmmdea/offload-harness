---
status: Accepted
date: "2026-08-30"
---

# 0031 — Harness-first is enforced, and web research is a harness lane

## Context

Every measurement since the agent lanes shipped said the same thing: the seats sit
idle while the expensive context does their work. The 2026-08-18 autopsy found zero
organic `agent_run`/`agent_delegate` use; the 2026-08-21 audit found 72 % of bulk-read
sessions used no offload at all. Each finding was answered with more steering — a rules
file, a nudge hook, a skeleton generator — and the 2026-08-25 overhaul deliberately
removed the last blocking gate on the theory that management beats compliance.

On 2026-08-30 the operator watched a session dispatch three cloud subagents for digest
work and run a 37-request HEAD loop through its own context, with zero harness contracts,
and set the rule: the harness gets used correctly or the sessions are not worth running.
Two facts decide the design. First, the only steering that has ever changed behaviour on
this box is a hook that DENIES (the gh-account guard, the Actions guard, the blast-radius
guard); advisory text has not moved this metric once. Second, the recurring excuse for a
cloud spawn was "the seats have no network" — true of the agent loop, and irrelevant: the
CALLER names the URLs, so the harness can fetch them delegator-side and hand the seats
inline text, exactly as it inlines `context_paths`.

## Decision

1. **A read-only subagent spawn is denied** (PreToolUse `Agent`, `offload-first-guard.js`)
   unless its prompt carries an auditable exemption — `harness-exempt: <web|write|judgment|
   capacity> — <reason>`. The H15 classifier is unchanged: judgment and implementation legs
   pass untouched; the deny message hands back the local call ready to run (the
   `agent_delegate` skeleton, or `offload_research` when the leg names URLs). Every
   decision is logged to the dispatch log; `HARNESS_GUARD=off` overrides an inspected case.
2. **A bulk network loop in the session shell is denied** (PreToolUse `Bash|PowerShell`,
   `harness-loop-guard.js`): a loop with a network verb and ≥ 8 numeric iterations or a
   list-driven body. Probes under 8 and loops without network verbs are untouched.
3. **`offload_research` makes web research a harness lane**: `{goal, urls ≤ 12}` → fetch
   delegator-side under a public-web guard (http/https only; loopback, private, link-local,
   `.local`/`.internal`, the tailnet zone and the CGNAT range refused; every redirect hop
   re-checked; 2 MiB / 96 KiB caps) → one contract per page, written the way the seats were
   measured to pass (document named as already provided; acceptance anchored to a page-only
   token plus a shape check) → the same `delegate.Run` path as `agent_delegate`, route
   `spread`. The seats never gain network access; the agent loop's egress cage is untouched.
   Same registration gate as `agent_delegate`.
4. The constitution states the enforced contract in the delegation section and names the
   research lane as the answer to "needs the web".

## Consequences

- A session must now decide, at the tool call, why a leg cannot run locally — and say so
  in a form the dispatch log records. Declines become countable; the scoreboard stops
  measuring silence.
- False positives fall on the override and the exemption, both explicit and logged. A leg
  that genuinely needs Claude costs one extra line, not a denied task.
- The research lane's quality is bounded by the seats' digest quality (measured 2026-08-28:
  contracts pass on the 27B, 9B and 4B seats when the document is named as provided) and by
  the stripper; a page that strips to nothing is reported as skipped, never digested.
- Re-measure against the 2026-08-21 audit's metric — organic `agent_delegate` /
  `offload_research` calls in non-harness-dev sessions — before touching the classifier
  again. The nudge's skeleton builder is reused, not duplicated.
