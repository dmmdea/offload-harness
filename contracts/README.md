# Delegation contract templates

Starting points for `local-offload delegate --contract <file>` and the MCP `agent_delegate`
tool (each template is one subtask object; the same shape works inside an `agent_delegate`
`subtasks` array, and a file may also hold an array of up to 8). Copy one, replace the
`context_paths` placeholders with real files under your `--read-root`, and sharpen the goal.
Enable recipe and a worked run:
[docs/OPERATOR-GUIDE.md](../docs/OPERATOR-GUIDE.md#delegate-subtasks-across-fleet-nodes-agent_delegate--delegate);
wire semantics: [docs/FLEET-NODE.md](../docs/FLEET-NODE.md#the-agent-task-task_type-agent).

JSON carries no comments, so the field guidance lives here.

## Fields (the `--contract` file shape)

| Field | Required | Notes |
|---|---|---|
| `goal` | yes | Self-contained — the sub-agent sees only this plus the context docs. Name the output you want and what "wrong" looks like; a vague goal is the number-one cause of schema-valid garbage. |
| `context_paths` | no | Files the **delegator** reads and inlines as context docs (≤ 128 KiB each), confined to `--read-root`. The templates ship placeholders — replace them. |
| `context` | no | Inline docs `[{name, text}]` if you already hold the text; ≤ 16 docs, ≤ 256 KiB total including anything `context_paths` adds. |
| `output_schema` | for remote | JSON Schema, flat `properties` map only — string / number / integer / boolean / string-array / `enum` fields (the grammar-compilable subset). **Required for any remote placement**; keep `required` listed, the validator enforces it. |
| `acceptance` | recommended | Delegator-evaluated DSL (below). A result failing any check is `failed_verification`, never a success. |
| `profile` | no | Default `research` (read-over-docs). |
| `max_steps` | no | Default 12, clamped to 12. |
| `timeout_sec` | no | Default 300, clamped to 900. Size it to the seat — a weak seat reading three docs can legitimately need minutes. |
| `write_root` | no | Opens the WRITE door (0.122.0, D-06): a directory RELATIVE to the run's read root that the seat may create and change files under. The executing node must have `agent_allow_write: true` or the contract is refused (ack 400 on the fleet path, `defer_class: "write"` locally). The seat works on the node's own throwaway copy of the docs and the result carries a unified `diff` + `diff_files` the **caller** applies — the harness never does. Grants create+overwrite only: no delete, no shell, no `run`, no network. Caps: 8 files, 64 KiB written, 192 KiB of diff; past any of them nothing is published. |

`schema_version` and `depth` are minted by the delegator; putting them in a file has no effect.

## Acceptance DSL

| Check | Passes when | Use for |
|---|---|---|
| `contains:<s>` | output contains `s` | a term the answer must mention |
| `not_contains:<s>` | output lacks `s` | banned filler (`TODO`, an apology phrase) |
| `regex:<re>` | Go regexp matches output | shape demands, e.g. `regex:[0-9]` = "carries a number" |
| `min_items:<field>:<n>` | `structured.<field>` is an array with ≥ n items (n ≥ 1) | minimum yield from an extraction |
| `nonempty:<field>` | `structured.<field>` present and non-empty (`0`/`false` count as values) | required fields that must not be omitted |
| `diff_touches:<prefix>` | the write set holds a changed path starting with `prefix` | a `write_root` contract: the leg changed the file it was pointed at |
| `diff_max_files:<n>` | the write set is NON-EMPTY and touches ≤ n files | a `write_root` contract: the leg stayed in its lane |

Text verbs read the final `output`, falling back to the raw `structured` bytes when `output`
is empty; the field verbs require `structured` and fail closed without it. Unfalsifiable
checks (`contains:`, `min_items:f:0`) are rejected at validation.

The two diff verbs read the run's WRITE SET and are the only checks a talkative seat cannot
satisfy by talking. Both fail closed on an empty write set, `diff_max_files` included: a cap
assertion that passed because nothing was written would make a contract that verified nothing
read as verified. They do not check that the change is CORRECT — nothing mechanical can. Read
the diff.

### Authoring rule: anchor at least one check to content that appears only in the docs

Acceptance is what makes a result *verified* and what fires the cross-seat retry — and three
authoring shapes, each measured in the standing corpus, quietly disable it. The intake lints
every contract and returns warnings per subtask (`results[].acceptance_lint`, warn-only):

- **PARROT-PASSABLE** — every content check is also satisfied by the goal text itself, so a
  model that echoes the question back passes as verified and the retry never fires (measured
  on 5/5 of the first organic contracts). Note `not_contains:<s>` counts as parrot-passable
  when `s` is absent from the goal — an echoed question trivially lacks it.
- **UNGROUNDED** — a `contains:`/`regex:` matching nothing in the contract's own context docs
  fails RIGHT answers (measured: `contains:OptiPlex` failed both seats on a task both did
  right, because the word never appeared in the doc).
- **SHAPE-ONLY** — `nonempty:`/`min_items:` alone verify that fields exist, not that they are
  true (measured: "the docs directory does not exist" passed `nonempty:summary`).

The fix is one habit: pick a term or figure that appears **in the docs but not in the goal**
(a number, a proper noun the question does not name) and anchor a `contains:`/`regex:` to it.

## The templates

| Template | Shape | Acceptance logic |
|---|---|---|
| [`docs-drift-scan.json`](docs-drift-scan.json) | one doc vs. source excerpts → drifted claims + verdict | verdict and count must exist; `drifted` may legitimately be empty (a clean doc), so no `min_items` on it |
| [`bench-log-digest.json`](bench-log-digest.json) | one benchmark log → best config, regressions, summary | a digest with no digit anywhere is wrong: `regex:[0-9]` |
| [`schema-extraction.json`](schema-extraction.json) | one reference doc → the identifiers it documents | an extraction yielding zero keys is a failure: `min_items:keys:1` |
| [`research-digest.json`](research-digest.json) | several docs → grounded findings, open questions, summary | a synthesis is only a synthesis with ≥ 3 findings: `min_items:findings:3` |

## Run one

```powershell
local-offload delegate --contract contracts/research-digest.json --read-root . --route auto --remote http://<node-b>:18811
```

Read the response's `summary` block first (`succeeded` / `deferred` / `failed_verification` /
`failed` / `infrastructure`); exit 0 covers honest defers and failed verification, non-zero is
transport/config failures **and** `infrastructure > 0` — the defers whose `defer_class` blames
a broken or misconfigured node rather than the work.

## `write-door/` — the write door's three-task gate fixtures (register D-06)

Three staged implementation legs, one directory each with its own `contract.json` (`write_root: "."`,
`thinking: "off"`, diff-verb acceptance): `t1` a one-file Go fix (`Clamp` returns the wrong bound —
`go test` is RED until it is fixed), `t2` a two-file Go fix plus one table case (`ParsePort` never
rejects > 65535 — the fix and the case that exercises it), `t3` a JSON + Markdown record edit (flip
one node's flag and its table cell, nothing else). `scripts/write-door-gate.ps1` sends them to one seat,
applies each returned diff to a FRESH copy and proves it there (`go test`, a JSON parse, exact-row
checks) — the proof a caller of any write contract owes, since the harness never applies its own
writes. Run it after opening `agent_allow_write` on a node:

```powershell
scripts/write-door-gate.ps1 -Remote http://<node>:18811     # a fleet node's door
scripts/write-door-gate.ps1                                  # this box's own seat, route local
```

Measured 2026-09-15: 3/3 on a 4B vLLM seat (18 / 45 / 48 s) and t1 in 10 s on a 27B seat. The files
are pinned `eol=lf` in `.gitattributes` so a seat's LF diff applies on every checkout.

## `digest-8.json` — the parallel-sessions gate fixture (0.111.0)

Eight self-contained digest subtasks (inline `context`: this repo's own ADRs), used by
`scripts/parallel-sessions-gate.ps1` to run K concurrent 8-wide fan-outs against one llama-swap
and prove the seat-contention wait fires instead of deferring (ADR 0032). Needs no `--read-root`.
## `digest-8-grounded.json` — the same eight digests with a doc-only anchor each (register D-100)

`digest-8.json`'s acceptance is SHAPE-ONLY (`min_items:findings:3` + `nonempty:summary`; the intake lint
says so on every run), so an 8/8 on it proves the loop completed and the schema filled, not that the
digests are right. This fixture adds ONE `contains:` per subtask on an identifier the document names and
the goal does not (`installed.json`, `agent_model`, `gpulease.InspectDir`, `gpt-oss-20b`,
`delegate-intent.jsonl`, `fleet_queue_holder`, `harness-loop-guard.js`, `AnchorCheck`) — extracted by the
fleet seats on 2026-09-15 and re-checked against each document, so a faithful digest cannot avoid naming
it and an evasive one fails verification. Run it BESIDE the old fixture (same seats, same day) for three
K×8 passes before it replaces the old one: a step in pass rate is then attributable to the acceptance,
not to the seats. Same inline `context`, no `--read-root`:

```powershell
local-offload delegate --contract contracts/digest-8-grounded.json --route local     # this box's seat
local-offload delegate --contract contracts/digest-8-grounded.json --route remote --remote http://<node>:18811
```
