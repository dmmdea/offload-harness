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

`schema_version` and `depth` are minted by the delegator; putting them in a file has no effect.

## Acceptance DSL

| Check | Passes when | Use for |
|---|---|---|
| `contains:<s>` | output contains `s` | a term the answer must mention |
| `not_contains:<s>` | output lacks `s` | banned filler (`TODO`, an apology phrase) |
| `regex:<re>` | Go regexp matches output | shape demands, e.g. `regex:[0-9]` = "carries a number" |
| `min_items:<field>:<n>` | `structured.<field>` is an array with ≥ n items (n ≥ 1) | minimum yield from an extraction |
| `nonempty:<field>` | `structured.<field>` present and non-empty (`0`/`false` count as values) | required fields that must not be omitted |

Text verbs read the final `output`, falling back to the raw `structured` bytes when `output`
is empty; the field verbs require `structured` and fail closed without it. Unfalsifiable
checks (`contains:`, `min_items:f:0`) are rejected at validation.

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
