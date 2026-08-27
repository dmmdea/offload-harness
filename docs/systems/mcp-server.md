# MCP server

## Purpose

The Model Context Protocol surface — how a calling agent (Claude Code and equivalents) reaches the
harness. It is the primary consumer-facing interface: most usage arrives here rather than through the
CLI.

## Questions this doc answers

- Which tools exist, and what do they group into?
- What keeps the advertised tool list honest?
- Why does a newly built binary not show new tools?
- Which tool can reach the network?

## Scope

Tool registration, the tool inventory, the stdio transport, the manifest and its drift test, and the
operational lifecycle of the server as an MCP client sees it.

## Non-scope

- What the tools actually do → [offload-pipeline.md](offload-pipeline.md),
  [media-generation.md](media-generation.md), [coding-agent.md](coding-agent.md)
- The CLI surface over the same capabilities → `local-offload` with no arguments prints usage

## Key concepts

**Tool** — one named, schema-described capability offered to the calling agent. **Manifest** — the
declared inventory in `.printing-press.json`, checked against what the code actually registers.

## How the system works

The server runs over **stdio** and registers its tools at startup. A calling agent discovers them,
calls them with JSON arguments, and receives JSON results — including Defers, which are successful
results, not errors.

**Twenty-six tools** are registered on every box, in families. The advertised set is per-box and
larger elsewhere: `agent_delegate` is gated on `agent_delegation_enabled`, and a box listing an
accelerator registers 11 more (see [accelerators.md](accelerators.md)). Read `tools/list` rather
than any number written down:

| Family | Tools |
|---|---|
| Text offload | `offload_summarize`, `offload_classify`, `offload_extract`, `offload_triage` |
| Vision | `offload_vqa`, `offload_assess_image`, `offload_extract_image`, `offload_video_describe`, `offload_video_watch` |
| Speech / OCR | `offload_transcribe`, `offload_ocr` |
| Media generation | `offload_generate_image`, `offload_generate_video`, `offload_animate_character`, `offload_generate_audio`, `offload_generate_svg` |
| Media editing | `offload_edit_image`, `offload_inpaint_image`, `offload_edit_image_generative`, `offload_upscale_image`, `offload_media` |
| Graph execution | `offload_run_graph` |
| Agent | `agent_run`, `offload_ask`, `offload_review_diff` |
| Remote (opt-in) | `offload_nim` |
| Status | `offload_status` |

`offload_nim` is the **only** tool that reaches a remote service. It is an explicit, caller-invoked
side channel and is not part of the Cascade — nothing escalates or falls back into it. See
[ADR 0001](../architecture/decisions/0001-defer-never-cloud-fallback.md).

`agent_run` drives the coding agent loop. Its default planner is the **agent seat** (config
`agent_model`, else the workhorse `model`; a per-call `model` argument overrides both), its default
timeout honors config `agent_timeout_sec` (else the built-in 180s), and its result reports the
resolved planner `model` alongside `output`/`steps`/`stop_reason` — visibility is the cure for a
silent seat. A resolved planner absent from the endpoint's served roster fails loud with
`deferred: true` naming the model, never a silent fall back to the workhorse — "served" means
matched against canonical ids **or** `meta.llamaswap.aliases`, since a tier-seeded `agent_model`
is normally an alias. Every response
carries the effect ledger (`effects` counts + `effects_flagged` records) on success AND deferred
paths, and `judge: true` adds one end-of-run **advisory** same-seat completion (`judge_report`)
grading the flagged effects for operator review — annotation only, it never gates anything.

`offload_ask` is the ONE-CALL delegation entry: question + paths in, `{answer, evidence}` out,
with the harness (`internal/askjob`) authoring the whole contract — goal, output schema, and an
acceptance check anchored to the distinctive tokens mined from the attached files (one
`regex:` alternation over the three most frequent tokens the goal does not already contain). It runs on the
local seat through `Pipeline.RunAgentContract`, the same entry a local delegation placement
takes. It exists because contract-authoring cost, not caller discipline, is what kept measured
`agent_delegate` adoption at ~0. Two properties are load-bearing rather than incidental: the
anchor is excluded against the FULL BUILT GOAL (the lint measures parrot-passability against
`c.Goal`, whose boilerplate carries its own long words), and when no distinctive anchor survives
the builder REFUSES instead of emitting a check that would pass anything. The generated
acceptance is evaluated by the handler and published as `verified` / `acceptance_failures` —
this lane does not go through `delegate.Run`, and a check nothing evaluates is decoration. `verified` is a
CITATION check, not a correctness verdict: it asks whether the published answer quoted one of a
few distinctive tokens mined from the attached files, never whether the answer is right. Those
tokens are picked to be things only these files would say — real identifiers wherever the files
have them, and ordinary words only when they are long enough to be domain terms or actually name
one of the attached files — but it stays a heuristic, so read `verified: true` as "this answer
demonstrably read the files", not as proof of a verbatim quotation.

The graded text is built from the fields the caller is SHOWN (`answer` + `evidence`, decoded),
never from the loop's prose or the raw structured bytes. Both of the other choices were measured
wrong, one per direction: grading the prose gives `verified: true` beside a published answer that
cites nothing, and grading the bytes gives `verified: false` when the re-pack returned an empty
`answer` and the handler fell back to publishing the prose. `verified: false` is a prompt to read
`acceptance_failures` and then the evidence, never a reason to discard the answer: the residual
case is a question whose subject is a SHORT (<8-character) or question-named identifier, which
leaves nothing anchorable at all.

### The ask lane's result cache

An IDENTICAL repeat of an `offload_ask` call — same question, same `read_root`, and the same
file **bytes** — is served from an in-process cache (`internal/askcache`) without spending the
seat again, and the response says so with `cache_hit: true`. A fresh run publishes
`cache_hit: false`; an absent field would read as unknown, and "was this answer computed just
now" is not something a caller or the adoption instrument should have to guess at. A
**deferred** result carries no `cache_hit` at all, by design rather than omission: a defer is
never stored, so it is always a fresh run.

**Say what this buys, and no more.** It pays on an exact repeat and on nothing else. A
*different* question over the same files still pays full seat time (46–75 s measured), because
the seat has to reason about the new question. The only mechanism that would fix that is
keeping a model context resident between calls, which needs llama-swap slot pinning — trading
a seat's availability for cache warmth, and explicitly declined. Nothing here is a general
speedup, and the tool description says so in as many words.

Four properties are load-bearing:

- **Keyed on CONTENT, never on path.** The key covers the question, the resolved `read_root`,
  and each resolved doc's name plus the SHA-256 of its bytes. A file edited between two
  otherwise-identical calls is a different key, so the seat runs again — a stale answer is not
  merely unlikely, it is unreachable. That single property is the whole safety argument for
  serving a cached answer at all, and it is mutation-proven (key on the path instead and both
  the unit test and the wired front-door test go red).
- **The lookup happens AFTER `askjob.BuildContract`**, because the key *is* the resolved file
  content and `BuildContract` is what resolves it. Reading the files is microseconds against
  the seat time a hit skips. It also means every refusal (no anchor, over a cap, outside
  `read_root`) still happens on every call: only a finished answer is short-circuited, never a
  refusal. Keying on the resolved docs rather than the caller's raw path strings is what makes
  `/abs/cfg.go` and `cfg.go` one key — both hand the seat identical bytes.
- **Only successful, non-deferred results are stored.** A defer, a refusal or a runner error is
  a statement about this minute, not about these files; caching one would turn a transient seat
  failure into a lane that stays dead for the rest of the connection. A `verified: false`
  answer *is* cached — the seat ran and answered, the citation check simply did not match — so
  an identical repeat returns the same unverified answer rather than re-rolling the seat. A
  caller wanting another attempt changes the question, which is a different key.
- **Bounded at 32 entries, oldest out, and scoped to the process.** The MCP server is spawned
  per client over stdio, so one connection is one process is one cache, born and destroyed with
  the connection. That is why there is **no `session_id` argument**: it would be a second,
  weaker spelling of a boundary the process already draws exactly, and adding a required input
  to the one-call tool would undercut the friction removal the tool exists for.

The repeat returns without touching the seat at all, and editing one attached file makes the
next call miss and go back to the seat. That is not a timed measurement — no run log exists to
attribute a number to, and the only number ever in hand was a cold-swap-degraded figure (the
seat's llama-swap slot was mid-load for a different model during the attempt) that does not
belong here. What IS provable: `TestAskSecondIdenticalCallSkipsTheSeatAndSaysSo` asserts the
seat ran exactly once across two identical calls, and the mutation proof named in the "Keyed on
CONTENT" property above (key on the path instead and both the unit test and the wired
front-door test go red) pins the edited-file-misses behaviour directly.

One deliberate gap to know about: the ask lane writes **no delegation ledger or corpus row**.
`delegate.Run` records one per subtask; `Pipeline.RunAgentContract` on its own does not, so
`offload_ask` traffic will not appear in the delegation corpus or in any analysis built on it.
The pipeline's own task ledger still sees the run. Nothing depends on this today — it is
recorded so nobody later reads an empty delegation corpus as "nobody used the tool".

`offload_review_diff` is the CLEAN-CONTEXT review lane: a diff plus a task statement in,
severity-ranked findings out, run on the local seat through the same
`Pipeline.RunAgentContract` entry. Its argument is different in kind from every other lane's.
The others compete with "read the file yourself" on cost, and lose — this one offers something
a lead cannot produce from inside its own context at all: a reviewer that never saw the work.
The isolation is the mechanism, so the contract ships the task and the diff and **nothing
else** (`internal/reviewlane`), and the tool is registered unconditionally beside `offload_ask`
for the same reason — a lane behind a config flag is one more reason not to take it.

On the evidence for it, keep two things apart. Cognition **reports** a dedicated reviewer in
their Fusion setup catching ~2 bugs per PR, ~58% of them severe; that is the vendor's own
published figure, with no sample size, no A/B baseline and no external audit, so treat it as a
claim rather than a citation. The MECHANISM is independently supported: long-context
degradation, measured across 18 SOTA models by Chroma's context-rot study and by Stanford's
lost-in-the-middle work. The lane rests on the mechanism.

Four design choices are load-bearing rather than incidental:

- **The diff rides in the GOAL, not in a context doc.** A context doc becomes a file the seat
  must find with `list_dir` and open with `read_file`, and the measured failure mode of a small
  planner is calling no tool at all — which would produce confident findings about a diff never
  read. The cost is that `AgentContract.Validate`'s 256 KiB context cap never sees the diff, so
  `reviewlane.MaxDiffBytes` owns that bound and refuses early with the real numbers.
- **No acceptance check, deliberately.** An empty findings list is a CORRECT outcome, so any
  content check would either punish a clean diff or pass anything — the decorative acceptance
  `delegate.LintAcceptance` exists to name. What replaces it is a check the harness can actually
  make: a finding naming a file the diff never touched is dropped and reported as
  `dropped_ungrounded`, since an invented path is how a small seat fails here.
- **Findings arrive as an array of strings.** `gbnf.FromJSONSchema` compiles any array to an
  array of strings, so an object-item schema would have become strings anyway; the prompt asks
  for one `severity | file:line | claim | why` line per defect and `ParseFindings` reads them
  back tolerantly, keeping what it cannot parse as an unranked claim rather than dropping it.
- **An empty findings list is never published unless the seat EARNED it.** "No findings" is the
  one result a reader might take as reassurance, and a broken run reaches exactly that shape:
  `agent/loop.go` returns `stop_reason:"done"` as soon as the model stops requesting tools with
  no check that the final message carries content (empty content is live-measured here — the
  re-pack's comment on a GBNF + thinking seat stranding its answer in `reasoning_content`),
  `agenttask.go` special-cases only `"budget"`, and `repackStructured` extracts findings from an
  empty string into a schema-valid `{"findings":[]}`. `steps` and `stop_reason` describe both
  cases. So `reviewlane.VerdictReadsClean` cross-checks the seat's OWN raw answer for the
  explicit `NONE` verdict the prompt asks for, and the handler defers with a distinct reason
  when it is absent. It checks for a signal, never for quality. When the list is genuinely
  empty, the response says in words that it is not a verification.
- **Three counts say what is NOT in the list**, published on the same terms (present when
  non-zero): `dropped_ungrounded`, `dropped_echo` (the prompt's own field spec or worked example
  handed back as a finding — measured behaviour, so it is a byte-equality guard rather than a
  human's vigilance), and `truncated_by_cap`. The `note` on an empty list is gated on them:
  "found nothing" beside a non-zero drop count is false, and says so differently.

Everything the lane returns is ADVISORY: it never gates a merge and never substitutes for the
final does-it-actually-work verification, which stays with the caller — as do security review,
architecture judgement, and any call the caller is accountable for. Findings are triage input;
a `severe` label from a small local model is a prompt to read those lines, not proof. It shares
the ask lane's ledger gap above: `RunAgentContract` writes no delegation corpus row.

## Important flows

Every tool ultimately enters the Cascade or a media backend — see
[../flows/cascade-escalation-and-defer.md](../flows/cascade-escalation-and-defer.md) and
[../flows/run-graph-manifest-satisfaction.md](../flows/run-graph-manifest-satisfaction.md).

## Data and state

The server is stateless between calls. State lives where the underlying system keeps it — the ledger,
the audit trail, footprints.

## Interfaces and entry points

- The MCP entry in `main.go`'s subcommand dispatch; tools registered in `internal/mcpserver/`.
- `.printing-press.json` declares the manifest: `api_name`, `version`, `module`, and the MCP
  transport plus tool list.

## Dependencies

`internal/pipeline`, `internal/agent`, `internal/rungraph`, `internal/nimclient` (the one remote
tool).

## Downstream effects

This is a published interface. Renaming or removing a tool breaks every configured client, and
changing a tool's argument schema breaks callers silently — the calling model simply starts getting
errors it will try to work around.

## Invariants and assumptions

1. **The manifest and the registered tools must agree.** A drift test enforces it, so adding a tool
   without updating `.printing-press.json` fails the build. Currently 22 registered, 22 declared;
   the manifest's `version` tracks `VERSION` release by release. This test arrived via an outside
   contribution after the manifest had silently drifted to claiming four tools.
2. A Defer is a successful result. Do not map it to an MCP error.
3. `offload_nim` is the only remote surface, and it is opt-in.

## Error handling

Tool errors return as errors; Defers return as results with `deferred: true`. The distinction matters
to the calling agent, which should retry neither — it should do the work itself on a Defer, and
diagnose on an error.

## Security and privacy notes

The stdio transport inherits the trust of whoever launched the process. `agent_run` exposes the
coding agent, and therefore its capability flags — the defaults there are what keep this surface
read-only unless deliberately widened. See
[ADR 0003](../architecture/decisions/0003-policy-broker-and-capability-flags-off-by-default.md).

## Observability and debugging

- `offload_status` reports harness state to the calling agent: the configured model roster (the one
  table in `config.ModelRoutes`, shared with `doctor`/`acceptance`/`report`), a live `/v1/models`
  probe through [`internal/swapclient`](../../internal/swapclient/swapclient.go), and
  `media.routes` — this machine's media capability **derived** from its
  bindings (`internal/mediacap`), never declared. See
  [media-generation.md](media-generation.md#capability-is-derived-never-declared) for the verdicts.
- `local-offload doctor` checks the serving layer the tools depend on, and prints the same derived
  media routes — a route bound to a file that is absent exits non-zero.
- **The most common operational surprise:** an MCP client holds its server process for the session,
  so a rebuilt binary is not picked up until the client restarts. Newly added tools appearing absent
  almost always means a stale server process, not a registration bug.

## Testing notes

`internal/mcpserver/` covers tool registration and argument validation (`badargs_test.go`);
`agentrun_e2e_test.go` exercises the agent tool end to end. The manifest drift test
(`TestPrintingPressManifestListsEveryTool`) lives in `main_test.go` at the repo root, since the
manifest is a repo-root file.

## Common pitfalls

- Adding a tool and forgetting the manifest — the drift test catches it, which is the point.
- Expecting a Defer to be an error.
- Debugging "missing tools" without restarting the MCP client first.
- Assuming every tool is local: `offload_nim` is not.

## Source map

- [`internal/mcpserver/mcpserver.go`](../../internal/mcpserver/mcpserver.go) — registration and
  handlers
- [`internal/askjob`](../../internal/askjob/ask.go) — `offload_ask`’s contract builder (goal,
  output schema, and the grounded acceptance anchor)
- [`internal/askcache`](../../internal/askcache/askcache.go) — `offload_ask`’s content-addressed,
  bounded, per-process result cache (the `cache_hit` field)
- [`internal/reviewlane`](../../internal/reviewlane/review.go) — `offload_review_diff`’s contract
  builder, finding parser, diff-grounding filter and severity ranking
- [`internal/swapclient`](../../internal/swapclient/swapclient.go) — the harness's single
  alias-aware llama-swap roster reader, over `tools/llamaswap`'s `pkg/llamaswap`
  ([systems/printed-clis.md](printed-clis.md#the-one-exception-the-harness-consumes-pkgllamaswap))
- [`internal/config`](../../internal/config/config.go) — `Config.ModelRoutes`, the one roster table
- [`.printing-press.json`](../../.printing-press.json) — the declared manifest
- [`main_test.go`](../../main_test.go) — `TestPrintingPressManifestListsEveryTool` (the drift test)
- [`main.go`](../../main.go) — subcommand dispatch and MCP entry

## Related docs

- [../architecture/decisions/0001-defer-never-cloud-fallback.md](../architecture/decisions/0001-defer-never-cloud-fallback.md)
- [../OPERATOR-GUIDE.md](../OPERATOR-GUIDE.md)
- [../../README.md](../../README.md) — full CLI and MCP tool tables
