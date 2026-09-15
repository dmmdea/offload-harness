# Fleet release notes - 0.124.0

Status: draft

## What shipped

- The write door (ADR 0044) is opened per node with `agent_allow_write: true`. A contract that carries
  `write_root` may create and change files under that directory and nowhere else; the node publishes the
  change as a unified diff and applies none of it. Reviewing and applying the patch stays the caller's job.
- The delegator places a write contract only on a node whose door is open. A node that has not opted in
  answers the dispatch with a 400 at ack time, so the delegator re-places instead of burning the wall.
- The diff is taken from a before/after tree snapshot of `write_root`, not from the tool-call ledger: the
  ledger records that `write_file` ran, never what the bytes became, and a file the seat wrote without
  being asked to is invisible in it.
- The write set is rendered before every defer branch. A run that hit its step budget may still have made
  a real, reviewable change, and a door that published only on the success path would throw that away
  exactly when the caller most needs to see what the seat did.

## Caps

| cap | value | why |
|---|---|---|
| files per run | 8 | a seat that rewrites a tree is not doing the leg it was given |
| bytes per file | 65536 | one file a reviewer can read in one sitting |
| path shape | relative, non-escaping | no `..`, no absolute path, no `.git` segment, no reserved device name |

## Operating notes

Run `scripts/write-door-gate.ps1` after opening the door on a node. The gate sends the staged legs under
`contracts/write-door/` to one seat, applies each returned diff to a fresh copy of the task and proves it
there - a green `summary.succeeded` is not the proof, the test run on the applied copy is.

A seat writes its diff with LF line endings whatever the box it runs on, so the fixtures are pinned
`text eol=lf` in `.gitattributes`. A CRLF checkout would make every context line of the patch miss.

## Known limits

- The door is off by default and stays off on any node whose config does not say otherwise.
- A cut tool-call argument is a budget defect, not a broken node: the seat could not fit the write into
  one completion. Ask for a smaller write or raise the seat's step budget.
- Nothing here grants shell access. The write tools are `write_file` and `edit_file`, both confined to
  `write_root` on every platform the harness runs on.

## Rollout

1. Build the node binary and swap it in with the rename dance, so the previous build stays one `mv` away.
2. Open the door on ONE node first and run the gate against that node only.
3. Read the gate's report: every leg must report `succeeded`, a diff that touches exactly the expected
   files, a patch that applies cleanly to a fresh copy, and the per-leg proof that holds on the copy.
4. Only then open the door on the rest of the fleet, and re-run the gate on each node as it opens.

A node whose gate fails keeps its door shut. The failure names the leg and the reason - a deferred run,
a patch that would not apply, or a proof that did not hold on the applied copy - so the next attempt
starts from evidence instead of a guess.
