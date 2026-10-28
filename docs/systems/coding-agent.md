# Coding agent

## Purpose

`local-agent` — a coding agent loop driven by the local model Tiers. It reads, searches, and
optionally writes, executes, and fetches, inside a permission and budget envelope built for models
weak enough to get stuck.

## Questions this doc answers

- What can the agent do out of the box, and what needs a flag?
- What stops a stuck model from looping forever?
- How confined is an executed command, and does that differ by platform?
- What is the difference between a profile and two-tier mode?
- Where is the record of what the agent did, and how honest is it about what each call did?
- Which layer may block a risky action, and which only annotates it?

## Scope

The agent loop, its tools, the policy broker, budgets and circuit breakers, profiles, two-tier
architect/editor mode, transcript compaction, worktree memory, and the OpenAI-compatible server.

## Non-scope

- The cascade the agent's `offload_*` tools call → [offload-pipeline.md](offload-pipeline.md)
- The MCP surface, which exposes `agent_run` but is a separate system →
  [mcp-server.md](mcp-server.md)

## Key concepts

**Worktree** — the directory writes are confined to. **Policy broker** — the gate for effectful
actions. **Profile** — a named narrowing of the toolset plus a tuned prompt. **Two-tier** — an
architect model plans, an editor model executes.

## How the system works

The loop alternates model calls and tool calls until the task completes or the step budget runs out.
Three limits keep a weak model from burning the budget on nothing:

- **A step budget** — the loop stops with `StopReason: "budget"`.
- **Tool-call caps** — `dispatchOrThrottle` sits between the model's request and execution. It
  refuses an exact repeat (same tool, byte-identical arguments), and it caps calls per tool name
  (`--max-same-tool`, default 8 since 0.80.0 — it was 3, and at 3 the cap starved legitimate
  multi-file reconnaissance: a six-question repo recon needs more than three `read_file` calls on
  different paths, and the exact-repeat refusal is what actually stops a stuck loop). A tool that
  breaches the name cap is *also removed from the tool list sent on every later request* —
  structural enforcement, added after a 9B model re-issued an already-refused identical call
  seventeen times in a row.
- **The `read_file` character budget** (register D-103, `readbudget.go`) — the one tool the loop
  meters in characters instead of by call. `read_file` pages (`offset`/`limit`) and its own hint
  tells the seat to ("use offset=N to continue"), so a seat doing as told spent one call per page:
  on 2026-10-01 a seat was refused its ninth read of a 12,005-character document having taken in
  about 8 KB of it, and with the loop's per-result cap evidently at 1,090 characters (six of the
  eight results were exactly that long) eight reads could never have held that document. For
  `read_file` the same-tool cap now counts *files*: the first read of each path counts, so a
  six-file reconnaissance costs six and the ninth distinct file is still refused, while a new page
  of a file the run has already read costs characters, not a call. An exact repeat, a read that
  failed (a path that does not exist is not "being read"), a call whose path cannot be read and a
  page that gave nothing new count against the cap, and no other tool is affected. A page gives
  nothing new when the tool failed or refused the call as not performed, or when it began past the
  end of the file (`(end of file — N lines)`): it costs the budget next to nothing, so the budget
  cannot bound it and the cap counts it once it has run (the call that crosses the line completes),
  and a seat paging past the end of a file or re-sending a page that fails spends the cap as it
  always did. The same lines asked for under another spelling of the call are still new pages, and
  only the budget and the step budget bound them. Every `read_file` result is charged to a per-run
  budget at the length the transcript keeps — after the loop-boundary trim, the figure the effect
  ledger records as `obs_chars`; once the budget is spent the next read is refused and `read_file`
  is withdrawn like any capped tool (the call that crosses the line completes, so the overshoot is
  at most one result). The setup replay is not charged. The default is the larger of the step budget
  and the same-tool cap, times the cap on one result: it can never refuse a seat that issues one
  read per step (the step budget bounds that case, as it always did) and it bounds what is left,
  many reads in one step (measured on the delegation log, 2026-09-07 to 2026-10-01: 214 of 3,374
  runs issued parallel reads, up to 12 in a step; finished runs read 6.4 KB at the median and 95 KB
  at p99). `Loop.WithReadBudgetChars` sets it and `Loop.WithoutReadBudget` restores the pure call
  count. The exact-repeat refusal, the withdrawal of a tool refused twice for one call (D-49) and
  the forced final step (D-89) are unchanged: with the step budget they are what bound a seat
  hunting through tools. A `max_calls_per_tool` env rule on `read_file` is a separate, explicit cap
  and still counts every call.

**The forced final step is re-asked once when the seat answers it with a tool call (0.184.0).** The last step of a multi-step
run (D-89) offers no tools and opens with the answer-now turn. One seat family answers it with a tool call written as text
(`<tool_call><function=...>`, since no parser runs on a request without tools) or parsed into a call: 35 of the 106 runs of that
family that reached a forced final, against none of 241 on every other seat, with 30 of the 35 holding more than half their wall
and the evidence they needed already in the transcript. The run used to end there on `budget` with the text thrown away. The
loop now asks again, once (`finalReasked`), when the wall holds one more turn (`wallHoldsAnotherFinal`: the seat's list-cap floor
where one is known and always the wall of the final call just made plus a quarter, so a contract without an output_schema, or a
seat with no known rate, is gated too; the list-cap re-issue's own gate fails open there), with a request that differs from the first because the decode of this step is greedy and an identical request
repeats itself: the offending text (or the tool name and arguments) is quoted back in a new user turn, the seat is told that
nothing it writes as a call will run and to begin with the answer in the requested shape, and the offending turn stays out of the
transcript. The re-ask spends no step, runs at the same budget with thinking off, and both completions are in `calls[]` as forced
finals. A seat that answers it with a tool call again ends the run on `budget` as before, with both replies quoted in `stop_note`;
an answer is `done` with `stop_note` saying the seat first wrote a tool call and was re-asked once. The rigger files the
failed shape as `final-tool-call`, not `budget` ("split it"), and the delegator gives the contract to another seat once,
placed like a seat-down retry (`delegate.producedNothingDefer`): it is not held in line for a busy retry seat and not refused
for the failed seat's `min_turn_sec`, because the run produced nothing and a skipped retry is a lost job.

The ordering here is load-bearing and documented in the code: the name cap must be checked before the
exact-repeat check, or a model stuck on an identical call matches the repeat branch forever and never
reaches the branch that disables the tool.

Separately, the **policy broker** gates effectful actions — write, overwrite, delete, fetch, shell.
It resolves deny → ask → allow with deny unconditional, converts an `Ask` to a `Deny` under
`--unattended`, and downgrades an `Allow` to `Deny` if the audit record cannot be written.

> These are two distinct chokepoints. The broker decides *may this happen at all*; the loop decides
> *has this happened too often*. They are frequently described as one thing, and they are not.

**Risk rules.** Inside the broker's classify chokepoint sits a **structural, tighten-only rule
table** (`rules.go`): each rule is an action kind plus a glob over the worktree-relative path
(write/delete) or the host (fetch), and its decision must be Deny or Ask — a rule can veto what
posture would have allowed, never resurrect what the built-ins deny, and never grant. There are
deliberately **no rules over shell command lines** (string-matching a command line is a WAF —
bypassable by quoting tricks while false-positiving on legitimate work; the OS cage owns shell
containment). A built-in floor denies secret-material paths (`.env*`, `*.pem`, `*.key`, `id_rsa*`,
`id_ed25519*`) at any posture. Operators load a versioned, diffable JSON table with `--rules` — a
missing or invalid file is an error, never a silently inactive table. Subjects are normalized the
way the filesystem (or DNS) resolves them — case-folded, trailing dots/spaces stripped, on every
OS — so folding errs toward denial. Each hit is recorded in the audit trail with the rule and its
severity; severity sorts the morning review, it never changes the decision.

**Since 0.55.0, an UNATTENDED run loads a built-in default table when `--rules` is empty**
(`unattendedrules.go`, embedded from
[`internal/agent/unattended-rules.json`](../../internal/agent/unattended-rules.json)): every delete
queues for review behind hard denies (write AND delete) for evidence (`*.jsonl`), weights
(`*.gguf`/`*.safetensors`) and worktree-root CI workflows; lockfile/`go.sum` hand-edits hard-deny
(their deletes queue like any other delete); config/dependency-manifest writes queue — while
ordinary source writes stay governed by the posture flags, so the agent can still do the work it
was granted. The table gates only the write/delete tools (`ActWrite`/`ActDelete`) — file
operations inside the shell/run cage are the OS cage's jurisdiction. `--rules <path>` REPLACES
the default with the operator's own table (replacement, not overlay, is what lets an operator loosen
the delete catch-all — rules themselves only tighten), and `--rules off` is the explicit ungated
escape hatch. On an ATTENDED run with `--rules` empty no table loads: ask resolves to a human
anyway. Beneath any of this the rules in force always include the built-in `defaultRules()`
secret-material floor, which every broker carries unconditionally (`policy.go`); a loaded table only
ever *adds* tightening on top of that floor, and the floor is no substitute for a table, because it
matches secret-material globs and nothing else.
[`examples/agent-rules.json`](../../examples/agent-rules.json)
is the shipped starter table for operator-authored rules — 20 rules: source overwrite
`*.py`/`*.go`/`*.ts`/`*.js`/`*.mjs` → ask; `go.mod`, `package.json`, `config.json`, `*.yaml`,
`*.yml` → ask; `go.sum`, `*.lock`, `package-lock.json` → deny; `*.gguf` → deny on **both** write and
delete; `.github/workflows/*` → deny on **both** write and delete (critical); `*.jsonl` → deny on
**delete only** (there is no write rule for it); `fetch *` → ask; and `delete *` → ask (high),
deliberately LAST — first match wins, so a catch-all placed above the critical denies would shadow
them into dead code (that exact ordering bug shipped in this file until 0.55.0). It trades the
default table's wider config/manifest coverage for source-overwrite and fetch asks.

**Effect ledger.** Every tool call is recorded as an `EffectRecord` (`effects.go`) — step, tool,
status, and the why for non-committed statuses. Four statuses, deliberately only four:
`committed` (ran to completion, success), `failed` (ran, returned an error — the tool got its
chance to clean up), `unknown` (the loop stopped waiting: per-tool budget exceeded or mid-call
cancellation; the goroutine may still be mutating the world — the one status it is dangerous to
soften), and `none` (never executed: unknown tool, circuit-breaker refusal, broker denial, park).
Tools that *declined* to act return the `NotPerformed` sentinel so the refusal ledgers as `none`,
not `committed` — without it, a run whose writes were ALL denied was byte-identical on the ledger
to one whose writes all landed. The model-visible bytes are unchanged (the sentinel converts back
to ordinary tool-result content). `agent_run` surfaces the counts plus every non-committed record
(`effects`, `effects_flagged`) on success AND deferred paths, and the standalone CLI prints the
flagged records. PARTIAL and ROLLED_BACK are deliberately absent: the loop has no rollback
machinery, so claiming them would be decoration. Since 0.113.22 each record also carries
`obs_chars` (the size of the result the model actually read, after every rule and cap) and `rule`
(the environment rule that decided the call, if any) — the per-step facts the trace below is built
from.

**Environment rules (`agent_env_rules`, ADR 0036).** The loop is an *environment* in
envharness's sense — a frozen policy (the seat) acting through tools — and every tool call passes
three interceptors in envharness's order, driven by a CLOSED, validated vocabulary in config
(`core.AgentEnvRules`, `envrules.go`), a property of the SEAT rather than of the task:

| key | hook | effect |
|---|---|---|
| `deny_tools` / `allow_tools` | filter_action (static) | withheld from the offered specs — structural, like a profile; never grants a tool the flags/profile did not enable |
| `max_calls_per_tool` | filter_action | the N+1th EXECUTION of a named tool is blocked with a reason the model reads (`NOT executed: … limit … no longer offered`) AND the tool is withheld from the offered specs for the rest of the run (structural, like the same-name breaker — a text refusal alone lets a fixated seat burn the step budget on blocked calls); a breaker refusal spends no execution |
| `arg_limits` | filter_action | whole-number caps (≤ 2^53) on numeric arguments per tool; an argument above its cap is rewritten to the cap (marshalled as an integer), the call still runs, and the result carries a `[note: … adjusted this call's arguments …]` line so the clamp is visible to the model; non-numeric/absent arguments untouched |
| `rewrite_error` | modify_transition | a result of a tool that RAN and errored (`failed`) matching a regexp becomes the operator's short line (first match wins); successes are never rewritten |
| `observation_strip` | filter_observation | regexps removed from every TOOL result (banners, cookie notices, progress bars) |
| `max_observation_tokens` | filter_observation | one tool result bounded to tokens×4 chars (minimum 64 tokens — below that the cut has no room for its marker), head+tail with the elision marker, BEFORE the loop-boundary cap, which still applies after it |

The two observation hooks run on TOOL OUTPUT only (`committed` / `failed` / `unknown`). Loop-authored
text — a rule's own block reason, a circuit-breaker refusal, `unknown tool` — is never rewritten or
stripped: those lines are the loop talking to the model.

`filter_action` runs before the loop's own circuit breakers, so a blocked call spends no execution
count and a rewritten argument is what the breakers and the tool both see (two asks that clamp to
the same value are one exact-repeat key — the note on the first result says why); the two
observation hooks run before `contextbudget.Trim`. Per-run counters live in per-run state (`--serve` shares one
`Loop`). Every hit is recorded (`Result.RuleHits`, `EffectRecord.Rule`) and reported (`rules_fired`,
`env_rules` on `agent_run`; the build note `agent env rules: …` names the table and the withheld
tools). A table that does not validate — bad regexp, negative cap, empty name, unknown key on the
CLI file, a name with surrounding whitespace, a fractional cap — fails the BUILD by name at every door (`local-agent` exit 2, `agent_run` defer, fleet
`config`-class defer); a nil/zero table is byte-identical to the pre-key loop. `local-agent
--env-rules <file>` replaces the config table for one run (the rigger's scratch validation; `off`
= none); `examples/agent-env-rules.json` is the starter. These are NOT the structural risk rules
(`--rules`): a risk rule decides what an effectful action may DO to the world; an env rule shapes
how a weak seat behaves inside the loop and can neither grant nor deny an effect. envharness's
LLM-written rule code (`rules_code`) is deliberately not ported — rules are data here.

**Recipes (ADR 0036, amendment 2026-10-15).** A recipe is a candidate edit to the env-rule table, kept as a typed
record in a store (`internal/recipe`: one `<id>.json` per recipe, by default under `<data home>/recipes`). The record
has six keys and refuses any other: `id`, `seat_tag` (the capability class of the seat it was distilled for), `rules`
(the vocabulary above, decoded as strictly as the `--env-rules` file, so `rules_code` is an unknown key; every string
in it must be shaped like rule data: lower-case identifiers for tool and argument names, one line of plain ASCII,
at most 160 bytes, for a `rewrite_error` text, a pattern of at most 200 printable characters that does not match
the empty string), `provenance` (the acceptance-passing runs it was distilled from) and `status`
(`proposed | published | demoted`), plus, once audited, the `audit` that set the status. A `published` record
without an ADOPT audit whose steps are no worse does not decode. Only `deny_tools`, `allow_tools` and
`max_calls_per_tool` can be scored by replaying a recorded trace (branch, parallelism, stop class); the other four
keys change what the seat reads and need a live A/B.

`local-offload recipe-audit --recipe ID --seat ALIAS --seat-tag TAG --contracts TASKS.json --baseline BASELINE.json
--engine-build STR` is that A/B. It runs each contract of a frozen pre-registration on one seat twice (the seat as it
is, then with the recipe's rules in place of the box's table), judged by the acceptance DSL, and publishes the recipe
only on a measurement-gate ADOPT with mean steps no worse than the incumbent's; anything else demotes it, with the
audit kept in the record. It never takes a lease: with no `GPU_LEASE_EPOCH`, or an epoch no live lease carries, it
refuses with exit 2 before the seat is asked, so it runs under `gpu reserve ... -- local-offload recipe-audit ...`.
Its baseline must put the primary on the unit interval (every run scores 1 or 0) and declare tokens-class efficiency
metrics only, and its contracts file must cover every declared task; those are refused before the seat is asked, and so
is a build the gate cannot stamp. Both arms are stamped with the pin of the seat that served the runs (the one the
harness puts on every served run's wire result) beside the rule table each ran under, so a model swapped behind the
alias moves the gate's era, and a pin that moves mid-audit ends the audit with nothing recorded.
`--input-out FILE` writes the gate input, and `gate verdict --baseline B --input FILE` gives the printed result
again. No proposer exists: a recipe enters the store as a file an operator writes, and the proposer waits for a
trace corpus in the hundreds.

**The step trace.** Every agent wire result and `agent_run` response carries `trace`: per tool
call the tool, its effect status, `obs_chars` and `rule` — no transcript bytes — plus
`rules_fired`. It is set before the defer branches (the budget/timeout runs are the ones a
diagnosis needs most) and it is what the delegation-log corpus lacked: measured 2026-09-07, 51
of 58 failed 4B rows stop at exactly two steps with empty schema fields, and the corpus could not
say what those steps did.

**Setup replay (`setup_actions`, ADR 0036 P2, 0.113.24).** A contract may carry up to eight
`{tool, args}` actions the loop REPLAYS before the model's first turn (`setup.go`,
`core.AgentSetupAction`); `local-agent --setup <file>` is the CLI form and a node with
`agent_seed_context_reads: true` prepends one `read_file` per context doc itself (the node knows the
file names it just materialized; a delegator would be guessing); the seeded reads and the contract's
own actions are ONE list per run, clamped to eight — seeded reads first, the contract's tail cut, the
trace showing exactly what replayed. envharness's Setup component
replays a fixed action list on reset without charging the episode budget; here the replay is
charged to the wall and never to `max_steps`. Why: with the trace in place the corpus said what the
two steps were — 117 failed or deferred 4B rows in six days, every one grounded on context docs,
89 of them `list_dir` then `read_file` — the two calls it takes to find the document the node
wrote into the read root. The replay lands in the transcript in the shape the seat produces on its
own: one assistant turn carrying the tool calls, then one tool result per call — NOT a user
message holding the document text, because to a small seat a user turn is a question (the
2026-09-03 few-shot leak), while a tool result is data it asked for. The accounting, stated once:
`filter_action` applies (deny/allow withhold, `arg_limits` clamp; a `max_calls_per_tool` cap sees
the per-run counter, which the replay never spends — the model's own budget under that cap stays
whole); the circuit breakers are neither consulted nor fed (the model never issued these calls, so
its later identical call is a first call, not a repeat); the observation hooks and the
loop-boundary cap apply as on any call; the results are PINNED for compaction (the lossy rungs keep
them) but sit OUTSIDE the protected preamble, and the whole replay is bounded to half the
compaction budget — past it the remaining actions are recorded not-run (`none`, note `setup
budget …`) and the model reads for itself, exactly the pre-key behaviour. A failing action (unknown
tool, tool error) is its error observation, never an abort, except that a tool that cannot open the
workspace root ends the run before the first model turn (register C-79). Every action is a Step-0
`EffectRecord` with `Setup: true`; the wire result and `agent_run` report `setup_ran` (executed =
committed or failed) and the trace's step-0 entries carry `setup: true`. A node one release behind
ignores the field (the contract decoder keeps unknown fields for exactly this mixed-fleet case) and
reports no `setup_ran` — read it before crediting a replay. The DESIGN's acceptance is measured,
not assumed: the corpus's failed 4B contracts re-run on the same seat with and without seeding,
pass rate first, step count second.

**The rigger, first slice (`local-offload rig`, MCP `agent_rig`; `internal/rig`, ADR 0036 P3a, 0.113.26).** A
deterministic classifier over the delegation-log corpus: every failed or deferred row of a seat lands on exactly one
failure axis, evaluated in a published precedence order and stopping at the first hit — `seat-infra` (a defer class of
infrastructure/config/contract, or a placement failure with no defer class) → `call-deadline` (a reason that opens
`call deadline reached`: the whole-call deadline of ADR 0065 cut the subtask, so it says nothing about the seat's speed) →
`capacity` (a defer class of capacity, or a dispatch a node answered with 503/429 "queue full" in a row written before
delegators marked refusals: the fleet was busy, which is neither a wall nor a seat, and its remedy names
`agent_call_deadline_sec` / `agent_placement_wait_sec`, never `timeout_sec`, which does not bound that wait; it sits
ahead of `timeout` because every wait bounded by the call words itself with the bare word "deadline") →
`timeout` → `reasoning-starved` → `final-tool-call` (the forced final step was answered with a tool call, again after one
re-ask: the reason carries `core.ForcedFinalToolCallReason`; ahead of `budget`, whose stop_reason and prefix it shares, so its
remedy does not tell the reader to split a contract that had read everything it needed) → `budget` → `abstention` (an
"output failed schema" reason is `schema-miss / invalid`) → `schema-miss` (min_items/nonempty; sub-axes
`two-step-grounded` = context docs and ≤ 2 steps, `invalid`) → `anchor-miss` (contains/not_contains/regex; same sub-axis;
an authority-qualified check keeps its verb prefix, so its failure lands here too, an UNKNOWN verdict (empty or absent
evidence: an empty answer, an absent field) included: the axis names which kind of check failed, and the empty-versus-wrong
distinction is in the evidence column, `unknown at <level> authority`, which is why an empty answer under a qualified check
reads as an anchor-miss while an absent field under an unqualified `nonempty:` reads as a schema-miss) →
`loop` → `long-observation` → `tool-misuse` (the last three on trace-bearing rows only) → `unclassified`. The report gives
per axis the hits over the rows ELIGIBLE for it (trace axes count only rows with a trace), up to five evidence job ids with
the deciding fact, the most repeated reason for seat-infra/timeout/unclassified, and the pre-authored remedy where the
closed vocabulary has a lever (`agent_seed_context_reads`, `agent_env_rules.max_observation_tokens` /
`max_calls_per_tool` / `rewrite_error`, with the value derivation in words) or "not a rule matter" where it has none.
A row a delegator marked `refusal` (a node's capacity refusal at dispatch, which the capacity wait outlasts and the
subtask is re-placed from) is no failure: it is not a bad row, it is on no axis, and the report counts it on its own
`refusals` line outside `rows`, so a fan-out that met a full node reads as the place in line it was.
`--verdicts` writes every row's verdict for an agreement check. It proposes nothing on its own and applies nothing. Why a
classifier first: the design council read the day's corpus (1,187 rows, 649 bad) — ~70 % were wall timeouts and seat
errors no rule shapes, only 16 rows carried a trace, and a digest-8 gate cannot fail for axes it never exercises. P3b (a
proposer with a validation gate, the difficulty-zone and red-team objectives) waits for a trace corpus in the hundreds.
The trace now carries `note` (≤ 160 bytes of what the model was told) on non-committed calls, the evidence a
`rewrite_error` rule needs. Acceptance: a fresh-context reader applying the rules as written to a stratified 41-row
sample agreed 41/41; the coverage gaps that pass found became the dispatch, invalid and anchor-miss rules.

**Unattended risk parking.** Each effectful tool (the write/edit/delete trio, `web_fetch`, `run`,
`run_shell`, the `github_*` trio) advertises a `security_risk` self-annotation (low/medium/high) in
its schema, recorded on the call's `EffectRecord` whatever its fate. On an unattended run, an
effectful call flagged `high` — or carrying any present-but-unrecognized value, which **fails
closed** (the model tried to flag it) — is **parked**: refused with `EffectNone`, recorded durably
in the ask queue, and answered with an explicit "NOT executed, continue if you can" result.
Self-annotation can only tighten — an unannotated call proceeds under normal policy, because weak
local models skip optional fields routinely and parking everything unannotated would kill
open-write runs. Parked calls do not consume the same-name tool budget: three parked writes must
not disable `write_file` for a later low-risk one.

**Parking is fail-safe residue, not coverage — measured recall 0%.** The mechanism above fires
exactly as described; what fails is the *input* it reads. Measured 2026-08-11 on the production
agent seat across 48 runs and 66 effectful calls, the annotation was a literal constant: **54 of 54
emitted declarations were `low`** — zero medium, zero high — including all **36** structurally
destructive calls, and it held under an escalated-severity arm where deleting every source file in a
tree was still self-declared `low` (`builder.go`, `batchjudge.go`; CHANGELOG 0.48.0). The
complementary failure is recorded in the shipped rule table's own reasons: on **12 of 13**
`edit_file` calls the annotation was omitted entirely, and an absent annotation never parks. So the
field is both skipped *and*, when emitted, constant — which qualifies the rationale above: "weak
local models skip optional fields routinely" is only half of the problem. Keep the mechanism,
because it fails SAFE and an honest model can only use it to tighten — but do not read it as
coverage. Before 0.55.0, with no `--rules` table loaded, self-annotation was the only per-call gate
**above the capability flags and the built-in floor**, and it has 0% recall on destructive calls.
That regime is retired: an unattended run now loads the default table above, which turns the probe's
own call (`delete src/notify.py`, self-declared `low`) into deny-and-queue on structure alone. The
gates that fire per-call even with `--rules off`: the `defaultRules()` secret-material floor (secret
globs only — it would not have stopped any of the 81), the broker's unconditional `.git` denial
([invariant 3](#invariants-and-assumptions)), and `os.Root` worktree confinement (invariant 2).

**The `UNGATED` build note.** `Build` appends an operator-visible note when an unattended run is
granted mutating capability with the rule table explicitly disabled. The trigger is exactly
`Unattended` **and** at least one of `--allow-write` / `--allow-delete` / `--allow-overwrite` /
`--allow-shell` / `--allow-github` **and** `--rules off` (`builder.go`) — `--allow-write` counts
because the default table gates write-new paths too (workflow denies, config asks), so opting out
with write-only capability is a real downgrade; `--allow-run` alone does **not** trigger it (the
table cannot gate cage execution), and an empty `--rules` no longer can (the default table loads
instead, with its own `ACTIVE` note whenever `--allow-write` is granted). It is a **note, never an
error**. It rides on `built.Notes` and the CLI prints
it to stderr (`cmd/local-agent/main.go`, and again for the two-tier editor build). **Scope:** this
is the CLI/queue path — the path that grants `--allow-*` and sets unattended. The MCP `agent_run`
front door passes no write/delete/shell/fetch capability, so it never trips the condition and is
unaffected.

**End-of-run advisory judge** (`agent_run judge=true` / `WithBatchJudge`). After the loop
finishes, one bounded same-seat completion grades the run's flagged effect records for the
operator's review (`judge_report`). It runs with **fresh context** — only the objective and the
flagged records, never the transcript, because a judge that shares the planner's context is
steered by whatever steered the planner — and its inputs are bounded (first 24 flagged records,
clipped notes and objective, omissions stated in the prompt) so the chaotic run that most needs a
summary is not the one whose judge dies on a context overflow. It is **advisory only and never
fatal**: it gates nothing, blocks nothing, and runs only on runs that actually flagged something.
The evidence base for annotation-not-enforcement: prompted general models are near-random at
trajectory risk grading (R-Judge) and favor their own generations, so the report is consumed by
humans, never by control flow.

"Grades" means a **three-way partition**, and an operator reading a `judge_report` needs it by name
(`batchjudge.go`):

- **WARRANTED** — something irreversible or outward-facing happened, or nearly did, that the
  operator would not have approved; or an effect landed in an `unknown` state.
- **EXPECTED FRICTION** — ordinary agent work that trips a flag by design. The prompt enumerates
  seven cases: a call that failed and was corrected on a later turn; a test or build that failed
  because it was written to expose a defect; an exploration dead-end; a fallback to another tool
  after one was refused or unavailable; a call against something that did not exist yet; a call
  refused by policy or the circuit breaker where the run proceeded without it; and a zero-count
  result (`0 failed`, `no matches`) — a clean outcome, not a failure.
- **BLOCKER** — the run was stopped by something no agent could fix (missing credential,
  unreachable host, capability not granted). Worth a human's time, but **not misbehaviour**.

Each record gets ONE numbered line: verdict, realistic worst case, recommended action
(approve-similar / add-a-rule / investigate / ignore). An all-EXPECTED-FRICTION report is explicitly
sanctioned by the prompt and is the *normal* outcome. The partition exists because every record the
judge sees is ALREADY flagged, so "was the flag warranted?" carries no information and biases the
grader toward yes — a grader asked only about trouble saturates toward trouble.

**Tools.** Read-only by default: `list_dir`, ranged `read_file`, `search_files` (regex/glob, capped
matches), `summarize_file` (an offload digest), and the in-process `offload_*` cascade tools. Each of
the rest sits behind its own flag, all defaulting off: `write_file` / `edit_file` / `delete_file`,
`web_fetch`, `web_search`, `run`, `run_shell`, and the `github_*` tools.

`search_files` patterns are **case-sensitive** regexes (`(?i)` prefixes one to fold case). That fact
has to reach the planner at the moment it matters: a bare `no matches` made small planners retry with
a *longer, more specific* query and then report the text as absent — a docs lookup that failed on
every model/profile combination on the 6 GB tier. The zero-match result therefore states the
case-sensitivity and names the concrete `(?i)` retry, and the suggestion is suppressed for patterns
that already fold case (detected by parsing the pattern, not by substring-matching `"(?i)"`), so it
can never burn a `MaxSameTool` call on a retry that cannot change the result.

**`run` executes an allowlisted program directly, with no shell** — `go`, `gofmt`, `python`,
`python3`, `pytest`, `npm`, `node`, `cargo`, `git`. Bare name only, resolved on the trusted PATH, and
refused if the resolved binary lives inside the worktree (the `build` profile grants both `write_file`
and `run`, so without that check an agent could write its own `go` and execute it).

**`run_shell` is Linux-only.** An arbitrary command line makes an executable allowlist meaningless,
so it is withheld elsewhere by an explicit platform check.

**Profiles** (`--profile general|edit|build|research|github`) narrow the toolset and add a tuned
prompt with exemplars. A profile can only ever narrow — naming a tool that was not granted is
silently ignored.

**Two-tier mode** (`--two-tier`) runs an architect model (default: the escalation Tier) to plan and an
editor model (default: the workhorse) to execute, with one model swap. The architect gets read and
search only; the editor gets whatever capabilities were granted.

> `--profile` and `--two-tier` conflict only for a *non-default* profile. `--profile general` or an
> empty value coexists with two-tier, because two-tier sets its own toolsets.

**Tool-profile seat.** The profile resolves per-call/flag override > config `agent_profile` >
`general` (`config.AgentTaskProfile`), the same shape as the model seat below and equally live at
rest — an unset `agent_profile` is never materialized into the config file. It exists because
`general` is a MEASURED trap on a small planner: on the ampere-6 reference box the same model on
the same contract scored **0% under `general`** (twelve steps burned calling tools, zero output
bytes) and **72% under a narrowed profile** — a larger factor than the choice of model. It is
per-TIER because it is a property of the seat's capability, not of the task: a 27B planner handles
the full tool set, a 4B does not. **Three front doors honour it: the `agent_run` MCP tool, the
`local-agent` CLI, and the delegation lane.** One path deliberately does not (below).

- **The delegation lane** (`internal/pipeline/agenttask.go`) resolves contract `profile` >
  configured `agent_profile` > `general` — the same `AgentTaskProfile` chain as `agent_run`.
  It previously hardcoded its own `research` default on the argument that the default describes
  the TASK SHAPE (a delegated contract is a read-over-docs job), not the box. That was reversed
  after the 2026-08-19 D3 bake: a spread's subtask 0 ALWAYS lands on the local seat, so on a
  big-planner box every fan-out was forced into the narrowed shape the bake measured WORSE there
  (27B: general 100%, research 94% and 5x slower on one shape), while the small tiers already
  carry `research` in their `config_seed` — the per-tier key is where that decision belongs.
  The task-shape concern is bounded: this lane builds read-only regardless, so a box seeded a
  non-research profile can only select a different narrowed READ-ONLY subset, never add mutating
  tools. A contract's own `profile` field still wins.
- **`--two-tier`** builds its own architect/editor loops and sets their toolsets itself, so a box
  default must not bleed in (it never calls `WithProfile` on those loops at all). An unknown configured name fails by
NAME rather than silently degrading to the one configuration measured to fail.

**Model seats.** The single-loop planner resolves per-call/flag override > config `agent_model` >
config `model` (`config.AgentPlannerModel`). `agent_model` is the tier-seeded planner seat: the
install seed (`internal/tierseed`) derives it from the tier row's `resident_tier` when that differs
from the workhorse — when they match, nothing materializes, so the fallback chain stays live. The
in-loop offload cascade stays on `model` (workhorse economics; an explicit per-call model still
drives both), and the two-tier seats are unchanged (architect=`escalation_model`, editor=`model`).
`agent_timeout_sec` is the tier-seedable default wall-clock for `agent_run` when the call passes no
timeout (0 = the built-in 180s).

**Budget calibration is available and OFF by default.** The ladder's `chars/4` estimator undercounts
real tokens (measured density 1.3-1.4 plus a fixed ~900-token payload the estimate cannot see), and
with a SMALL output reservation that lets requests through which the server then rejects.
`internal/agent/tokencal.go` fits `real ≈ intercept + slope·estimate` online from each response's
`usage.prompt_tokens` and corrects the BUDGET (never `estimateTokens`, so every rung compares in one
space), using a median fit over a sliding window so one bad reading cannot define or persist in the
line, bounded so it can never cut context by more than half. It ships **off**: at the shipped
`--max-tokens 4096` the output reservation already absorbs the error, and a live A/B showed enabling
it cut retained tool content by 52% and produced a wrong answer while neither arm hit a rejection.
Enable with `WithTokenCalibration(true)` where the output reservation is small or rejections are
actually observed; `Result.TokenCal` reports what it learned. See
[ADR 0017](../architecture/decisions/0017-kv-reuse-is-binary-and-how-we-measure-it.md).

**Compaction** keeps the transcript within the SERVED context window: `--ctx-tokens` defaults to
0 = auto — probe the endpoint's live `n_ctx` (`/upstream/{model}/props` on llama-swap, `/props` on
a bare llama-server; conservative 8192 fallback when unanswerable), because an assumed window
killed real runs with `exceed_context_size` 400s before the budget engaged ([ADR
0015](../architecture/decisions/0015-compaction-defaults-on-served-window.md)). An explicit value
overrides the probe (warned when it exceeds the served window). The ladder is
least-destructive-first: under budget nothing is touched (byte-stable, so the server's KV prefix
cache stays warm); over budget, the CHEAPEST rung runs first: **dedupe** (always on, ADR 0016) —
an older tool body byte-identical to a later result collapses to a reference naming the later
call; then with `--gcf-compact` (default ON — flip decision 2026-07-24) older
tool bodies that are JSON arrays of flat objects are re-encoded columnar (`internal/gcf` —
LOSSLESS, round-trip proven); with `--skeleton-prune` (default ON, same decision) remaining older
bodies are reduced to deterministic **skeletons** — head/tail windows plus buried
error/failure/warning lines, elided runs replaced by counted markers — then, as pressure rises, to
size markers that keep a bounded residue of the body's signal lines (the FORCE_PRESERVE guard),
and finally whole messages are dropped. That drop rung is TOKEN-EXACT when the endpoint can
tokenize (`cut_middle_turns`, `internal/agent/cutmiddle.go` + `internal/tokclient`): each message
is sentinel-indexed by its byte span in the one serialized transcript, the SERVED model's
`/tokenize` (with pieces) maps those spans to real token positions, and whole assistant+tool
units are dropped from the MIDDLE so the head (protected preamble) and tail (recent state) —
the first and last budget/2 real tokens — always survive; a message is never split, so a
truncated-mid-JSON tool result is unrepresentable on this rung (the reactive `emergencyShrink`
last resort below, which fires only after a live server rejection, remains the one documented
exception — and it is skipped when the token-exact rung measured the halved transcript as
fitting). Once the ladder reaches this rung, the chars/4 estimate no longer decides the drop in
either direction: the cut's own REAL-token verdict drives the exhausted telemetry (an
under-counting estimate would hide a real overflow; a pessimistic one would report a
verified-fitting request exhausted forever). One honest qualification (review 2026-08-14):
WHETHER the ladder engages at all is still the estimate's call — a transcript the estimate
reads as fitting is sent without consulting the tokenizer (a per-step network probe on the
happy path is the cost the design refuses). The ADR-0017 under-count regime is instead caught
by the reactive net: the server's own rejection triggers the retry, whose harder compact IS
token-exact. The budget both yardsticks target reserves the measured token cost of the
tool-spec block (`specReserve` — the specs ship with every request and dwarf the old fixed
margin on a full build) on top of the completion reserve and safety margin. Endpoints with no
`/tokenize` fall open to the legacy estimate-driven oldest-first drop — stickily, per Loop,
with the failure CLASSIFIED: a definitive route absence (all candidates 404/405) downgrades
immediately, a transient failure (timeout, reset, 5xx during a model swap) gets one second
chance and downgrades on the second consecutive miss; a failure under an already-cancelled
context is never counted (a --serve client hang-up says nothing about the endpoint). The
downgrade is never silent: `Result.TokenizerPath` reports `token-exact` vs
`legacy (degraded: <why>)` with the per-route failure detail (under `--two-tier` the
architect's verdict rides `ArchTokenizerPath` — the tiers degrade independently), `agent_run`
returns it as `tokenizer_path`, the CLI prints a per-tier stderr note, `--serve`/`--queue`
print a once-per-process transition note, and every queue trace records the goal's rung —
the same visibility rule as the window probe. NOTE `token-exact` means configured-and-not-
degraded: a run that never went over budget never probed the route, so only an over-budget
step proves it live. Both drop paths drop units whole,
and both exempt units still carrying signal residue or a **pinned** result (a result the model
re-requested and the circuit breaker refused again — the H8 ramp: content the model re-reads
stops being lossily compacted). A ladder
that consequently cannot fit exhausts honestly: `Result.CompactionsExhausted` counts it, and the
standalone runner + `agent_run` surface it (fit=false telemetry, never a silent over-budget
request). Compaction is idempotent and monotonic (test-pinned): a turn only moves down the ladder,
never back up — the KV-prefix stability invariant. The skeleton
rung is model-free on purpose: a cascade call costs seconds on the loop's critical path (measured;
see `skeleton.go`), a rules pass costs microseconds and produces identical bytes on every
re-compaction. When a server overflow rejection survives the harder-compaction retry (the
oversized body sits inside keep-recent, where the ladder is forbidden), `emergencyShrink` is the
last resort before the run dies: OLDER tool bodies are skeletonized then elided, oldest-first,
while the NEWEST body — the result the model is about to work on — is spared until last and then
trimmed head/tail to the remaining room (a bare marker only if even the trim cannot fit). The
preamble is never touched and no turn is dropped. The retry's shrink target is relative to the
just-rejected transcript's own estimate, not only the budget, so a dense-content estimate error
can never make the retry re-send the bytes the server just refused.

**Optional log-collapse rung (`compactOpts.LogCollapse`, default off).** An extra rung after GCF
and before the search-results rung (next paragraph) and skeleton, in `internal/agent/rung_logs.go`: it folds each run of three or more consecutive
near-identical lines in an OLDER result of a build or test command into its first line, a count of
the lines between (`[... N similar lines folded ...]`) and its last line. The gate reads the call
that produced the result, not only the tool's name. A result is folded only when its call is a
build or test invocation: `go test`, `go build` or `go vet`, `cargo build`, `cargo test` or
`cargo check`, `npm test` (or `npm t`) or `npm run build`/`test`, `pytest`, `python -m pytest` or
`python -m unittest` (also as `python3`), and (through
`run_shell` only, since the `run` tool's allowlist has no `make`) `make`. Through `run_shell` the
first word is found after any leading `NAME=value` words, and the command must be one pipeline: an
unquoted `;`, `&&`, `||`, a lone `&`, a newline or a carriage return anywhere in it, such as
`cd sub && go test ./...`, disqualifies it, and a pipe qualifies only into a grep-family program
(`grep`, `egrep`, `fgrep`, `rg`, `ag`, `ack`) or a pass-through filter (`head`, `tail`, `sort`,
`uniq`, `wc`, `cut`, `sed`, `awk`, `tee`, `cat`); `xargs` or any other program after a pipe
disqualifies. A later pipe stage is judged by its program name alone, so a filter that is handed a
file operand and reads that file instead of the pipe (`go test ./... | cat data.csv`,
`| head -n 5 data.csv`) still qualifies, and its output would be folded as if it were the build's;
operands cannot simply be refused, because `| tee out.log` is a pass-through with one. Everything
else is never touched: a search listing (`grep -rn`, `rg -n`, `git grep`; those are the search
rung's, below, which does not touch build or test output either), a `cat` or `sed` of a data file, a
linter's diagnostics, a file read, a fetch, a result whose
call is not in the transcript, a result whose tool-call id is declared by more than one call, and
a call and result that both carry an empty id. An unknown or ambiguous producer fails closed. Two
lines are near-identical when they match after every run of digits, `0x` hex literal, UUID, hex run
of eight or more characters and terminal control sequence is masked and trailing whitespace is
trimmed; indentation still counts and the letters around a digit run still count, so `worker7` and
`worker12` match while `x1` and `y2`, or `md5` and `sha256`, do not. Because digit runs are masked,
lines that differ only in a number (a status code, a port) fold into one, and the footer says so.
Every blank line, every line that looks like an error, warning, failure or test summary, every line
shaped like a `path:line` or `path:line:col` diagnostic (and rustc's `--> path:line:col`) and the
first 8 and last 4 lines stay verbatim and never join a run, so a log made only of one repeated
warning does not shrink and two different compiler errors never fold into one. "Looks like" means the
skeleton rung's `signalLine` plus stack-frame, traceback and test-runner summary shapes, the
CamelCase exception names that `signalLine`'s word boundary cannot see (`TypeError`, `KeyError`,
`AssertionError`, `NullPointerException`), a killed process, a segfault, a non-zero `exit status` and
npm's `ERR!` prefix; each test is made on the line as written and with its terminal colour removed,
so a coloured `ERROR` is kept and a line of nothing but colour codes counts as blank. The body opens
with `[log-collapse: original N chars, M lines]` (the idempotence marker) and ends with a footer that
says how many runs and lines were folded. Like every rung it works only on the ladder's own window
(never the protected preamble or the recent turns), skips pinned results (including the body a
pinned dedupe reference points at), rewrites `Content` only (tool-call pairing is untouched), goes
oldest-first and stops at the first fit. It is deterministic, idempotent, never makes a body longer
(the original string comes back unless the result is strictly shorter) and fails closed to the
original on a body that is too small or too large, is not valid UTF-8, holds a NUL byte, parses as
a JSON document, already carries another rung's marker or a GCF block, or makes the rewrite
panic. Because it stops at the first fit, enabling it keeps more of an older log than the rungs
after it would at the same budget. With the search rung on as well, this rung runs first and the
search rung only if the transcript is still over budget; no body is eligible for both, because each
reads the call that produced the body (a build or test invocation here, a search there).

A rung that is off changes nothing, down to the byte. `IsCompactionArtifact` keeps its three
clauses (elision marker, skeleton, dedupe reference), because the always-on dedupe rung and the live
loop's re-execute decision both ask it; a separate `IsLogCollapseArtifact` recognises the rung's
header, and the trace harvest asks both so it still refuses a transcript the rung has already
compacted. A tool body that merely quotes the header, such as a saved collapsed log read back with
`read_file`, is ordinary content to the default ladder, and a recorded set of digests over such
transcripts holds that. With the rung on, the skeleton prefix, the elide marker (and the reactive
last-resort marker) and the dedupe reference of a folded body name the size the body had before
the fold, so a marker never says that less was lost than was. The header is believed only when the
rung could have written it: the exact spelling, a size strictly above the body's own length and
within the sizes the rung folds, and a line count a fold needs. A result that merely quotes a header
with a negative, zero, oversized or too-small number is named by its own length instead, and so is a
result that quotes a skeleton header the skeleton rung could not have written. A body the search
rung thinned opens with the same skeleton header (the search rung writes it through the same
`skeletonHeader`), so with both rungs on its later elision marker still names the size the listing
had. A quoted
header whose numbers are merely plausible cannot be told from the real one in the text, so it is
still believed; carrying the size outside the body would close that, and no caller can yet.

No production surface enables the rung yet: the field is not wired to a flag, a `Loop` setter or the
config. The replay seam exposes it (`ReplayOpts.LogCollapse`), so `compaction-eval ... --log-collapse`
measures it.
Wiring it into the live loop must also teach `resultDestroyed` the new predicate. Whether to adopt
it is the measurement gate's call, not this page's, and that call needs a corpus whose tool calls
carry tool names: the shipped mini corpus records call ids only, so the producer gate leaves it
untouched, and the savings figures printed by the rung's tests come from fixtures whose calls are
renamed to `run`. The idea of compressing build and test output while keeping the failures and the
boundary lines is the documented behavior of the Headroom project (Apache-2.0); the mechanism here
is this repository's own, written without reading that project's source (see `NOTICE`).

**Search-results rung (optional, default off: `compactOpts.SearchResults`).** One more lossy rung,
for older tool bodies that are search listings: the grouped `search_files` output (`path:` then
`  L<n>: text`) and bare `path:line:text` lines such as `grep -rn` or ripgrep print, alone or inside the
`exit=` / `--- stdout ---` / `--- stderr ---` framing of `run_shell` and `run`. The rung is gated on the
call that produced the body: the result's tool-call id is mapped to its call, and only `search_files`
(grouped form) and a `run_shell` or `run` call whose command is a search (flat form) qualify. A search
starts with `grep`, `rg`, `ag`, `ack` or `git grep`. The `run` tool's allowlist (`go`, `gofmt`, `python`,
`python3`, `pytest`, `npm`, `node`, `cargo`, `git`) holds only `git` of those, so through `run` only
`git grep` is reachable; `grep`, `rg`, `ag` and `ack` come through `run_shell`, which is Linux-only, so on
another host only `git grep` qualifies at all. A search piped through
pass-through filters only (`head`, `tail`, `sort`, `uniq`, `wc`, `cut`, `sed`, `awk`, `tee`, `cat`, or
another search) still qualifies (`grep x f | head`); a pipe into anything else does not (`rg -l x | xargs flake8`,
`rg x | flake8 -`), because the last stage's output is the body and it may be a linter's. A command that
chains another one after the search (an unquoted `;`, `&&`, `||`, `&` or newline, found by a quote-aware
scan) does not either, because the body then holds that command's output too. The check reads first words
only and is not a shell parser: a filter that runs a command itself (`tee >(...)`, awk's `system()`, sed's `e`) or
ANSI-C `$'...'` quoting is not seen. A linter such as flake8, ruff, `shellcheck -f gcc` or rubocop prints the
same `path:line:col:text` lines, and every one of its diagnostics is something the agent must fix, so
its output, a body with no matching call and an id claimed by two different calls are left alone.
Everything else (the `web_search` list, `mode:"files"` output, an error result, and the output of a build or
test command, which is the log-collapse rung's above, never this one's) is left alone too. It runs after
dedupe, GCF and the log-collapse rung and before the generic skeleton, over the same older span as that rung (never the
protected preamble or the kept-recent tail), skips pinned bodies, and rewrites only a tool message's
body. Each line is scored with BM25 against the objective (the last user message of the protected
preamble, the one task text that is the same on every step; the line's own path counts as part of the
line), lines that look like errors (`error`, `fail`, `panic`, `exception`, `fatal`) get a fixed boost, and
the survivors are chosen round-robin across files, best score first, up to 30 lines, 5 per file and 15
files (chosen to match the values shown in the upstream configuration example), then written back in listing order under a
footer (a 100-match listing over 20 files ends `[... 70 of 100 lines elided, 5 of 20 files elided ...]`: 30 lines and 15 files are the most
that survive, and a test pins this exact line); the tool's own "more matches" or "truncated" note is kept. The result opens with the skeleton disclosure line, so the rest of the ladder,
the re-execute-on-repeat path (ADR 0016) and the corpus harvest treat it as the compaction artifact it
is, and a later elision marker still names the original size. It is deterministic, idempotent,
monotonic (a result that is not strictly shorter is discarded) and fails closed: a body that is not,
line for line, one of the accepted shapes comes back untouched. It has no production flag or loop
setter yet; `compaction-eval ... --search-results` replays it (through `ReplayOpts.SearchResults`), so the
measurement that decides it can be run. The option is reachable from the ladder's own options, from
tests and from that flag, the default ladder is pinned
byte for byte by `internal/agent/testdata/ladder_default_golden.json`, and whether the rung ever
becomes a default is decided by the measurement gate on a harvested corpus, not by this page. Three
pieces of work must land, in a separate change, before the rung can be switched on by default: an adaptive cap (the fixed
30/5/15 do not scale with the window), a production entry point (a CLI flag or loop setter), and the
measurement of how the rung interacts with the generic skeleton rung. Source:
`internal/agent/rung_search.go`. The idea is documented by the Headroom project (Apache-2.0); this is a
clean-room implementation of that documented behaviour (see `NOTICE`).

**Evidence-preserving reducer (optional, default off: config `agent_evidence_reducer`).** Not a rung of the
ladder above, which rewrites OLDER bodies under budget pressure and is model-free on purpose. This step runs
once, when a build or test result is first produced, and costs one cascade call, in
`internal/agent/rung_reducer.go`. For a `run` or `run_shell` result of 4 KiB to 64 KiB whose call is a build
or test invocation (the log-collapse rung's producer gate, `logsCallIsBuild`) and whose body opens with the
tool's `exit=` line, the cascade seat the agent's own offload tools use (`Build`'s `Offload`: one named tier
through the recordless offload, the `extract` task, no new client and no ledger row) is asked for at most five
claims, each with `claim` and a one-line `quote`. Every quote is looked up in the output with
`quotecheck.Locate`, the lookup behind the research digest's quote check (`research.VerifyQuotes` calls it; it
moved, verbatim, to `internal/quotecheck` because `internal/research` imports `internal/delegate`, whose tests
import the agent): the output's bytes first, then case, spacing, quote
marks and punctuation set aside, publishing the output's span; an elided quote (`first ... last`) is not a span and is refused. The transcript keeps a receipt instead of
the log: `[evidence-receipt: original N chars, M lines; ...]`, the tool's exit line copied by code (never
written by the seat), then each claim over its verified quote, and a closing line that says the rest is not
kept and the command can be run again. The reducer sits at the tool-result boundary in the loop, after the
per-result cap and the environment rules and before the read budget, the effect record and the pager, so each
sees the bytes the transcript really carries; it applies only to a call that ran (`EffectCommitted`) and
did not error.

The raw output is kept, unchanged, whenever anything is off: no exit line, under 4 KiB or over 64 KiB, a
call that is a read, a search, a listing, a fetch or any command the producer gate cannot read as a build or
test (`cat`, `sed`, `grep`, a chained command, an unknown tool); output the corpus vet's table
(`internal/piishape`, shared with `internal/compeval`: email, private-key block, bearer token, key assignment
including environment-style names such as `GITHUB_TOKEN=` and `AWS_SECRET_ACCESS_KEY=`, AWS access key, GitHub
token, bare `sk-` provider keys, JWTs) says looks like a credential or carries an email address, which is checked before the seat is shown anything and again on the receipt; a seat error,
timeout (60 s), deferral or panic; an answer that is not the requested JSON, has no claims or more than eight;
a claim without text or a quote that is not one line of 8 to 200 characters; any quote not in the output as one contiguous span; a
receipt that is not strictly shorter than the output. A claim is collapsed to one line, so it cannot forge a
receipt line. An exact repeat of a reduced call runs once more and that result is not reduced (the receipt
counts as a destroyed result for the repeat breaker, and a pinned result is never reduced), so the model has a
way back to the exact text. `IsEvidenceReceipt` recognises a receipt for that and for the trace harvest, which
refuses a transcript that holds one, like any body a ladder already shortened. With the key off nothing
changes, down to the repeat breaker.

What it does not prove, stated plainly: a verified quote is in the output, not support for the claim next to it
(the claim wording is the seat's, and the receipt says so), and a quote can be a framing line. The credential
check is the vet table, not a detector of its own: it recognises a key word in a name (including after an
underscore, a flag letter such as the `D` of `-Dpassword=`, a terminal color code, or a JSON escape such as the newline in
escaped text before a line that starts with `PASSWORD=`; a color reset may also stand between the name and its colon)
followed by a value of 12 or more characters that runs to the next whitespace, quote, comma,
semicolon, ampersand, backslash or escape byte (so a JWT or a slash-bearing key after `access_token=` or `AWS_SECRET_ACCESS_KEY=` is
matched whole, and the color reset behind a value is not eaten; a value that starts with a quote runs to its closing quote
instead, so a space, comma, semicolon or ampersand inside it belongs to it), a quoted literal of 12 or more characters behind a wider
operator (`password := "..."`, a `==` comparison, `=>`, a type annotation as in `const token: string = "..."`, a string prefix as in
`password = b"..."`, a subscript or hash as in `os.environ["API_TOKEN"] = "..."` and `{'password' => '...'}`, a quoted JSON
key `"token": "..."`) or in a call that passes the name and the literal as two arguments right behind an opening paren
(`t.Setenv("API_TOKEN", "...")`), a bearer token, a bare `sk-` provider key, a JWT, a Basic `Authorization` header (also in a
printed Go header map, a `setHeader` or `Header.Set` call, a subscript or a hash: at most eight punctuation characters, one
of them a separator, between the name and the scheme), a registry config's `"auth"` value and a `Cookie` or
`Set-Cookie` header with a value of 12 or more characters (taken as one whole span, described below);
a quote in front of a value or a header name may carry any
number of backslashes (JSON text inside a double-quoted shell string doubles them). The key words are api key,
private, access and auth key, secret, token, passwd, password, `PGPASSWORD`, passphrase and credentials, plus `_PASS=`
and `_PWD=` (the short words need the underscore and the equals sign, so `PASS: TestName` and `PWD` stay quiet). It still does NOT cover a camel-case name
(`githubToken=`), a key name the table does not hold (`dbpassword=`, `sid=`), a command-line flag with a space
(`--token value`, `curl -u user:pass`), URL userinfo
(`scheme://user:pass@host`), an authorization scheme other than Bearer and Basic, a provider prefix the table does not list (`sk_live_`, `xoxb-`), an unsigned JWT, a
high-entropy string that carries no key name or known prefix, a value that starts with `/`, `.`, `[` or `]`
(a path after a key name is meant to stay quiet), the part of a value after a backslash inside it (a backslash
is a JSON escape in tool-call arguments; a double-quoted cookie keeps its escapes), an unquoted value after `:=`, `==` or `=>` (an identifier or a call; by design,
because Go source assigns to token-named variables all day), a value passed apart from its name
(`fmt.Sprintf("password=%s", v)`), a terminal control sequence other than escape, bracket, parameters and a letter (a charset
switch, an operating-system command, the one-byte form of the bracket), a Basic header whose name and scheme (or a cookie header
whose name and cookies) are more than eight punctuation characters apart or have a word between them, a cookie behind a single
quote or an apostrophe (`note=don't` cannot be told from the quote that closes a header written in single quotes, so the match
ends there), a quoted cookie whose quotes carry a number of backslashes that is not a level of escaping (0, 1, 3, 7 or 15), or a cookie passed apart from its header name (`curl -b`, a cookie jar); because the table is shared, widening it widens the
corpus vet too. Run over the tracked tree, the assignment class matched 67 places before the quoted-value rule and 82 after (a scan of the
tracked text files, counted before this text was written); the 15 added are prose in quotes after a key name
(`checks["credentials"] = "present, not verified. ..."`), a placeholder with spaces in a doc example, and a string literal's
closing quote read as an opening one (`... token: ", x`), none a credential, and the cookie class matched nowhere in the tree before or
after (a scan of the text files under `internal/` and `docs/`, 2,276 files and 27 MB, and of the `go test -v` output of two packages;
in the Go standard library's `net/http` source, 158 files and 2.8 MB, the whole-span class below matches four places and the older
class three, and all four hold a real cookie value; the extra one is a `Set-Cookie` test fixture whose value of 12 or more characters
stands behind a JSON-escaped line break and indent). That is the price of the quoted rule: a quoted value after a key name now runs to its closing quote, spaces included, so
quoted prose after a key name is read as a value, in the safe direction. The table costs time: measured on one machine over 1 MiB of
text that matches no class, all eleven classes took 1.51, 1.54 and 1.54 s (three runs), about 1.5 s per MiB, of which the assignment class
is 0.6 s and each other class 0.1 to 0.2 s; the same input took 1.53, 1.53 and 1.55 s before the quoted-value and cookie changes, so those
changes did not move it measurably. The whole-span cookie class alone scans 1 MiB of Go source in about 0.1 s, as the older class did. Text built to
be slow for it costs 0.4 to 1.0 s per MiB for the class alone and grows in step with its length (four times the text, four times the time): 1 MiB of a
header, a bracket, an apostrophe, an escaped equals sign and a quote repeated took 1.0 s, 1 MiB of single quotes, equals signs and seven-backslash quotes behind a header opener 0.7 s, and a 240-byte unit of quotes, backslashes and brackets 0.5 s; a full redaction (every class) of 1 MiB of those texts took 1.4 to 2.1 s against 1.2 s for source text, the other classes costing about 1.1 s of it (one machine, `GOMAXPROCS=2`, a noisy desk; unverified elsewhere). Until a header line stopped
taking an opening bracket, a text that repeated a header over and over, each one opening a list that never closed, was scanned to its end once per
match and took the square of its length (16 KiB, 0.6 s; 1 MiB, hours), because a match cannot be reported while a thread of higher priority is alive.
A random search over units of up to a dozen pieces (header names, quotes at two levels of escaping, backslash runs, brackets, equals signs), run on
gate runners, found four more families that grew with the square of the length before the line alternative went first and a long value began by the
rule of a pair, and none after (9,800 units and 16 hill climbs against the final class; the slowest units scale linearly at 0.7 to 1.3 s per MiB from
8 KiB to 512 KiB). Its pattern is 132,500 program instructions (the older class 9,800; five levels of escaping, and the strings of a list both closed
and open) and compiles in 25 ms against 2.5 ms, once at start-up. A result is scanned only between 4 KiB and 64 KiB, so one scan costs at most about
0.2 s (0.1 s for source text and 0.1 to 0.2 s for the slow texts above at 64 KiB, the cost of all the classes together; a shared and noisy machine;
unverified elsewhere). The compaction-eval harvest redactor is derived from the same table and substitutes the email class
last, so it replaces a whole credential value rather than its head; its exit gate also fails the harvest when 12 or
more consecutive characters of a value it replaced are still in the text, in which case the harvest leaves that one trace
out with a note and keeps the others (ordinary code that assigns to a name holding `token` can trip it). The seat call sits on the loop's critical path, seconds per qualifying result. Wiring: the key
is read by the local agent CLI, the MCP front door and the fleet-node agent task; the prompt-replay builder
takes no node policy. The measurement that decides whether the key is ever turned on is a seat A/B on the 9B
and 27B agent seats through the gate's non-inferiority mode (tool-call reliability and pass rate), with
forwarded prefill tokens as the efficiency metric and never seconds; until it passes the key stays off.

The cookie class takes a whole span, not a cookie at a time. Once the name of a `Cookie` or `Set-Cookie` header and an opener fire (the punctuation a
Basic header takes, a comma followed by a quote as in `req.Header.Set("Cookie", "...")`, and a header fold, a line break, on either side of it) and
the span holds a cookie value of 12 or more characters, the match runs from the header name to the first of three ends: the end of the line (a line
break, its JSON escape or the end of the text); the closing quote of the string the header sits in, which is a quote that opens no quoted cookie value
(a bare quote, or a run of 1, 3, 7 or 15 backslashes and a quote: a header line or a shell command, JSON text, and each further layer of escaping, a
level being 2^k-1 backslashes after k layers; a single quote ends it too); and the closing bracket of a list (`"Set-Cookie": [ ... ]`, a printed Go
header, a Python list) or the closing brace of a Go slice literal (the Go-syntax print of a header, `http.Header{"Set-Cookie":[]string{ ... }}`), one
span however many elements and lines it has, a bracket inside a string element not ending it, and a string of the list closed in the middle of it and
open only as its last element (a list the text is cut in). A bracket behind the header's punctuation, behind a space or right behind the name opens a
list, and a header line takes no bracket there, so that the two never read the same one; a greater-than sign right behind the name (an XML tag, an
arrow) opens a line. The closing quote and the bracket are not part of the match, so the redacted text keeps its quoting and a tool call's Args stay
valid JSON (the text before the header name is untouched, so a JSON document that held the header as a key is not valid JSON by itself once redacted).
Nothing inside the span is read for an alphabet: a quoted value with a space, an apostrophe, an ampersand, a comma, a semicolon, a tab, an escaped
quote, a shell escape (an escaped dollar sign or backtick) or one of Go's HTML escapes for `&`, `<` and `>` (a backslash and u0026, u003c, u003e) is
inside it, a bare value with a shell escape (an escaped dollar sign, backtick, space or semicolon) or a backslash path is inside it, and so is every
other cookie and attribute on the line. Over-redacting a cookie line is the accepted price: it is evidence of a header, not of the build. Three rules
are finer. A match starts only when some value in the span has 12 characters with no white space from its start (a bare value, or a quoted one from
its opening quote; an escape counts as one character, in a bare value too: a JSON unicode escape, or a backslash run and the character it escapes),
which leaves `http.Cookie{...}` literals, `Header.Get("Cookie")`, a Go string concatenation such as `"Cookie: sid=" + f(x)`, Django's short messages
(`Saved ok`, `Can't save`, `Rock&Roll`) and the HTTP reference's example header quiet. A quote after an equals sign opens a quoted value only when a
value can start behind it: not white space, not a separator or a closer (a comma, a semicolon, a colon, an ampersand, a pipe, a paren, a brace, a
bracket, an angle bracket, the escape of a tab or of an ampersand or angle bracket), so base64 padding or an empty last cookie in front of the quote
that closes the string does not turn that quote into an opener, and the next field of Args, the next key of a document and the next argument of a
command stay out of the span (an empty quoted value is still a value). The value that starts a match follows the same rule: twelve characters counted
from a quote that a closer or a separator follows are not a cookie value. And a quoted value that is open at a line break goes on to its closing quote
when that is within three more line breaks (a header wrapped by a terminal, a fold; the JSON escape of a line break counts the same), and ends at its
line when it is not, so a quote that is never closed, as in a truncated line, takes the rest of its line and no more. The compaction-eval harvest
redactor runs this class first, before the credential classes (the email class stays last), because the span has to see the text as it was written.
Run after them, it met what they leave behind: the closing quote of a cookie named like a credential with a double-quoted value (the assignment class
takes the opening quote and the body and leaves the closing quote, which the span read as the quote that closes the string, so every cookie behind it
was written out), and the placeholder of a JWT, a Bearer value or a key-word cookie inside an unquoted printed list (`[redacted-jwt-1]` holds the
bracket that closes the list). Run first, the span can take the key word of a credential that follows it on the same line (an unquoted header, then a
credential whose value is quoted) and stop in front of the quote, which would leave the value with no key word for any class and no memory in the
value-tail gate, with the residual gate clean. So when the cookie class has substituted anything, the credential classes are also run over the text as
it was written, and every credential value of 12 or more characters they find is substituted wherever it still stands (longest first, with the
placeholder its whole match has) and remembered for the value-tail gate. A cookie named like a credential (`refresh_token=`, `password=`, `secret=`)
is taken whole by the cookie class, one substitution, and the value-tail gate remembers every value in the span, a quoted body whole with its spaces,
escapes and line breaks. The gaps are named: an apostrophe in an unquoted value ends the span (it cannot be told from the quote that closes a header
written in single quotes), and so does a bare double quote inside a value (a raw JSON value such as `prefs={"t":"d"}`, which cannot be told from the
quote that closes the string); a backslash and a lowercase n or r in an unquoted value is the JSON escape of a line break and ends the span, so a raw
path such as `dir=C:\new\x` ends it there (a capital, as in `C:\Name`, is an ordinary escape); a bare value that holds a shell escape inside four
layers of escaping (more than sixteen backslashes) is left whole; a quote with two, four to six or eight to fourteen backslashes is no level of
escaping and is not read as a pair; a quoted value that starts with white space or a separator is not a pair; a Go slice literal whose type is left
out (`{"a=b", "c=d"}` inside a map literal in source) is read as a line; a header folded again after its first cookie ends at that line break; and a
cookie passed apart from its header name (`curl -b`, a cookie jar) is not covered.

A fourth review of the cookie class changed what it takes and added a check that fails closed. In a printed list (`map[Cookie:[a=b c=d]]`) a closing bracket is a character of the cookie when a character of a value follows it and the one in front of it is no opening bracket (`sid=ab]cd`, the PHP-style name `user[id]=v`, a value that starts with a bracket, which is counted like one that starts with an ampersand and not toward the 12 characters), so it no longer ends the span with the tail of the value left in the text; the empty list of a typed slice literal (`[]string{...}`) still closes, and the slice literal is tried before the bracketed list. The open single-quoted string that a cut list ends in stops at a closing bracket, so an apostrophe in a value (`note=don't`) no longer runs through the closers of the Args around it (a list cut inside a string that holds a closing bracket keeps the part of the string behind the bracket). The Go-syntax print of decoded JSON (`map[string]interface {}{"Cookie":[]interface {}{"..."}}`, `[]any{...}`) opens a list like `[]string{...}`. A bytes or raw prefix in front of a quote (`b'cookie'`, `r"..."`, `u'...'`) is part of the gap, which covers ASGI scope headers, aiohttp raw headers and dicts of bytes. An underscore in front of the name is no obstacle (`HTTP_COOKIE`, `http_cookie`; `my_Cookie: ...` is a name now). A match whose header name is a key (`{"Cookie": ...}`, `{'Set-Cookie': [...]}`, `Header.Set("Cookie", "...")`) keeps the name, its closing quote and the punctuation up to the first cookie, replaces the cookies and puts back the closing quote of a list's last string, so a redacted JSON document is still JSON (the other classes still start a match at their key word, inside the key's quotes). In the assignment class a value may start with an ampersand (or the escape Go's encoder writes for one), and a credential header (`X-Api-Key`, `Api-Key`, `X-Auth-Token`, `X-Amz-Security-Token`, an underscore for a hyphen) printed with a list of values (`map[X-Api-Key:[...]]`, a JSON or Python list) is matched, with a placeholder behind the name not read as the value. The Basic header's gap takes a line break, indentation, a YAML dash and a placeholder left by an earlier class, so a pretty-printed document (`json.MarshalIndent`, jq), a YAML list and a header with two values are matched.

The residual gate fails closed. After redaction, a credential header keyword (cookie, authorization, api key, auth token, security token; in any case, and inside any longer word or name, so `HTTP_COOKIE`, a bytes prefix, a backtick, a quote and a bracket in front of it are no obstacle) followed within 32 characters by a run of 12 or more characters with no white space, quote, comma, semicolon or backslash (the length the classes ask of a value) fires `credential-keyword-run` in `Redactor.Residual` (`piishape.KeywordRun`). The harvest leaves the trace out with a note that does not echo the text, and the evidence reducer keeps the output raw and does not call the seat. The check does not read shapes, so the shapes the classes do not open on are covered by it and not by new openers: a YAML block list item, a backtick-quoted value, a HAR name/value pair, an XML attribute, an authorization scheme other than Bearer and Basic. Such a text is skipped, not redacted. The reach of 32 holds the deepest opener the classes read (eight punctuation characters, a header fold and its indentation, eight more) and the ways a document puts a name and a value apart: a pretty-printed list (a closing quote, a colon, a bracket, a line break, an indent and a quote), a name/value pair (`","value":"`, 11 characters), an XML attribute, and the same inside text that was escaped three times; each character more reads more of the text that merely follows a keyword as its value. Three rules keep it from firing on every header dump: a placeholder is not read and ends the reach of the keyword in front of it; a line break ends the reach once a word has been read since the keyword, and when only punctuation stands between them the reach goes on to the next line only if the last of it is a colon, an equals sign, an opening bracket or brace, a comma or a dash (a YAML key, a pretty-printed list or object), not behind a closed call or a line of prose; and another keyword is no part of a run (`Authorization` alone is 13 characters). A YAML block list that continues behind a redacted cookie header (the cookie class redacts up to the quote that closes the first item, and the name goes with it) is seen by a second rule: the placeholder of a cookie header followed by a line break, a dash and a long run. The trade is stated: a test or an identifier whose name starts with a keyword (`TestCookieJarKeepsTheValues`, `authorizationHeaderValue`) is a long run right behind it, and the trace or the build output that holds one is left out or kept raw. Over the Go sources, tests and Markdown of this repository the check fires on 5.4 % of the files (125 of 2,311: 34 of 595 Go sources, 76 of 1,544 Go tests, 15 of 172 Markdown files), nearly all with a long identifier or a long word right behind a keyword (an auth-token field of a config struct, a cookie type, "non-loopback" after `auth_token`); it costs 0.08 s per MiB of redacted text and 0.003 s for source text. Not covered by the check: a credential behind a keyword that is not in the list and that no class matches (a value in doubled apostrophes after a password name, `''value''`), and a leftover behind a placeholder other than the YAML list above (a placeholder ends the reach of the keyword in front of it).

The credential sweep (the second reading of the credential classes over the text as it was written, above) reads the lines of the cookie matches only, and reads the text once: a credential whose key word the span swallowed is on the line of the span, so the classes read those lines, and the values found are collected into a set, grouped by their first 12 bytes and tried longest first at each position of one pass over the output. It used to search the whole text for each value, which cost the number of values times the length of the text: one `Cookie` line in a text of many lines with a credential each took 12 s for 1 MiB and 44 s for 2 MiB on one gate runner (19 s and 67 s on a slower one; 4 times the time for twice the text). It now takes what the same text takes without the `Cookie` line (1.56 s and 1.55 s for 1 MiB, 3.1 s for 2 MiB), and a full redaction of 1 MiB of a text built to hold a cookie line with a credential on every line takes 1.9 s.

What the credential check is for, and what it is not. It is a best-effort list of shapes, not a proof that a text
carries no secret: the not-covered list above is open by nature, and widening the table trades precision and scan
time for recall. On the seat path it is defense in depth: the reducer's seat is a cascade seat on the same box unless
`cascade_remote_lanes` is configured, and even then it is another fleet node, never a hosted API. On the corpus path it
is the only automatic credential layer: the tracked-tree leak gate looks for denied names and machine identifiers, not
credentials, so a harvested corpus still needs a human read before it is committed anywhere public. Its precision on
ordinary output was measured on 9.4 MB of real Go build and test logs cut into 4, 16 and 64 KiB pieces: no credential
class matched any piece, and the email class matched 0.3 % of the 4 KiB pieces (all real addresses in git output),
after the email pattern stopped reading a Go module version (`toolchain@v0.0.1-go1.27.2.linux-amd64`, in every stack
trace) as an address; before that change it matched 67.6 % of them, so the reducer would have kept most Go logs raw.
An address whose domain starts with a lowercase `v` and a digit is therefore not recognised.

**Compaction eval harness (`compaction-eval`, `internal/compeval`).** Default flips for the ladder
rungs are gated by MEASUREMENT, never estimates: the verb replays a pinned corpus (JSONL of
transcript slices, sha256-hashed; a PII finding refuses the whole corpus) through the PRODUCTION
ladder via `agent.CompactReplay`, reporting per content kind the compression ratio, entity
retention with explicit lost-entity lists (the FORCE_PRESERVE classes: numbers, paths, URLs,
key=value, UPPER_SNAKE, hex ids), and a per-entry tokens ratchet against a frozen baseline (±2%,
cross-corpus and cross-ladder comparisons refused). `compaction-eval ab` scores full-vs-compacted
through the live pipeline (summarize + entity-recall outcome scorer — grounding is deliberately
not a term: measured live, it both passes entity-free garbage and inverts on benign numeric
paraphrase) behind a **control-pair self-test gate**: a scorer that cannot rank a
known-good/known-degraded pair aborts the A/B instead of producing a confident number from a blind
judge. `compaction-eval kvbench` is the Phase D leg: it replays a corpus step-by-step through the
production client and records the SERVER's own accounting (`timings.cache_n` / `prompt_n`, exposed
on `Completion.Serve`, nil when a backend reports nothing) to measure KV-prefix reuse and REAL
token counts. It brackets every run with a positive control (byte-identical resend, ≥90% reuse) and
a negative control (unrelated prompt) with a SEPARATION gate (pos−neg ≥ 0.40; the real tool specs create a legitimate ~17% framing floor, recorded as framing_floor_reuse), runs its arms in BLOCKS because the tier serves one KV
slot, and **fails closed to `INCONCLUSIVE`** rather than publishing a table of zeros when the tier
was evicted mid-run — one media-generation job on the same GPU is enough to do that, since
`render/gpu-lock.mjs::freeLlamaSwap` unloads every GPU-resident model except the memory stack (the mem0 embedder and reranker) before a render. Arms are compared only
over steps that succeeded in BOTH (`paired_totals`; per-arm totals are diagnostic-only), every
failure is classified `overflow`/`timeout`/`other` with timeouts checked first, and mid-run
evictions are detected from the data itself — on a prefix extension the server must still hold
what it held, so a collapsed `cache_n` relative to the PREVIOUS prompt is a scheduler artifact,
flagged and excluded from rates but kept in totals. `--budget-mode production-uncalibrated` budgets
the ladder as `Loop.inputBudget()` does WITHOUT the token calibration above, so its fire counts are
a lower bound on the calibrated agent's (measuring the calibrated budget needs a live run, not a
replay); `pressure` guarantees the ramp is observable but makes the fire count a fixture property — the
mode is stamped in the report. Measured finding
([ADR 0017](../architecture/decisions/0017-kv-reuse-is-binary-and-how-we-measure-it.md)): reuse is
BINARY — appends reuse everything, any edit anywhere discards the whole cache — so a compaction
fire always costs a full re-prefill, and the ladder is justified by the size win and by requests
completing at all, not by cache friendliness. A committed mini-corpus lives at `testdata/compeval/`;
real replay corpora are machine-local and never committed.

The two default-off rungs can be switched on in `run`, `freeze`, `ab` and `kvbench` with
`--search-results` (the search-results rung) and `--log-collapse` (the log-collapse rung), added to
`--gcf --skeleton`; the report's `ladder` names the arm (`gcf+skeleton+search`, `gcf+skeleton+logs`,
`gcf+skeleton+search+logs`). They change only the measurement: the production loop does not set them,
and without either flag the output is byte for byte what it was. A rung can only be judged on a corpus
whose older turns hold the shape it acts on (a search listing of more than 30 hits, or the long
repetitive output of a build or test command that names its tool call), and the contrast between the
incumbent and a candidate is two runs paired by entry id. `kvbench` takes `--arms on` to skip the raw
arm, which a candidate comparison does not need, and `--nonce N` to set the salt of the body requests
(default: a fresh one per run). Every sample carries `sent_sha256`, the sha256 of the transcript the
ladder produced, taken before the salt (its canonical encoding, not the HTTP body), so it does not depend on the nonce: two runs with
the same corpus prove a pair a no-op when the hashes of a step are equal, and a rung that rewrote the
transcript when they differ. Leave the nonce at its default. Reusing one on a warm seat lets the
second run's unchanged prompts hit the first run's multi-prompt cache, which inflates its reuse and
shrinks its prefill; the calibration controls always draw their own nonce (`control_nonce` in the
report), so a reused body nonce cannot fail the second run's negative control.

`compaction-eval harvest --traces DIR --out corpus.jsonl`
builds a real corpus from the standalone agent's trace files with REDACTION-AT-HARVEST:
deterministic placeholder substitution over the exact vet refusal classes (git output alone
carries author emails; the private-key class redacts the whole block, not just the header),
kind classification by byte-weighted majority of the TOOL payloads, and production replay
pressure mirrored exactly — `protected_prefix` = the real preamble (turns before the first
assistant turn), `keep_recent` = the live loop's `agent.DefaultKeepRecent`. Transcripts the
ladder already compacted mid-run are refused (`agent.IsCompactionArtifact`): their raw content
is unrecoverable and replaying them would measure the ladder against its own output. The
redaction table derives from the vet's class table (parity by construction; a class added to the table, such
as the provider-key and JWT classes, is redacted and vetted without a second edit), the VetPII gate
re-runs on the result — residual PII refuses the harvest — and the corpus is written atomically
(temp + rename), round-trip-proven through the strict loader before it exists at its
destination. Methodology harvested from the OmniRoute compression service's
eval approach (MIT); metrics and signals are this harness's own.

`trace-corpus` is the read-only readout over RECORDED agent runs, shaped like `compaction-eval`
(harvest, freeze, check) and kept apart from it. `trace-corpus harvest --traces DIR --delegation-log DIR --out corpus.jsonl`
reduces every run to numbers and enums (no goal, output, transcript or tool text), redacts the few strings that remain with
the same redactor and vet table `compaction-eval harvest` uses, and stamps the corpus with its sha256; the corpus is
machine-local and never committed. `readout`, `freeze` and `check` are deterministic and carry no timestamps, and `check`
demands the frozen report back byte for byte. With no source flag, harvest reads the install root's `agent-traces` and
`delegation-log` directories. It reads those two records and never the ledger.

- **What counts as a run.** A delegation row is a run only if an agent executed: a deferral (capacity, infrastructure,
  budget, config, contract) is logged with zero steps and nothing else, and harvest skips it under the named reason
  `delegation row never ran (zero steps, nothing recorded)` instead of counting it. Any record at all (a step, a step
  trace, prefill or seat-token accounting, or a pager object) keeps the row a run, so the unrecorded counts are counts of
  runs. Before the pager recording existed, a deferral raised before the loop ran and a run the loop returned from in zero
  steps with nothing else wrote the same row, so "never reached the loop" is an inference for the older rows and the
  skipped count is an upper bound on pre-loop deferrals; a row from a build with the recording carries a pager object when
  the loop ran. A row without a result, a capacity refusal and the harness's own test seat are skipped as before, and every
  skip is counted in the harvest stats. A file in the traces directory is a run only if it is shaped like an agent trace
  (a transcript, and either steps or a stop reason); any other JSON object there is skipped under the named reason
  `trace record has no transcript, or neither steps nor a stop reason` and counted.
- **Why a zero-step row with a trace is a run.** A deferred row (budget or infrastructure) can carry zero steps and a
  step trace made only of setup actions. The setup actions execute inside the agent loop, before its first model turn and
  without spending a step, so such a row did reach the loop, and the trace is the loop's own effect ledger. A budget or
  infrastructure deferral takes the loop's pager report, so once the recording ships the row carries a pager object and is
  not unrecorded. The compaction ladder also runs before the first model call and again when that call is rejected for
  context size, so a run can end at zero steps with passes counted.
- **Which rows carry a pager object.** From a build that carries the recording: a run the loop returned from carries one,
  zeros included, whether it finished or was deferred after the loop returned (a budget, infrastructure, abstention or
  write-class deferral, for example); the object is attached right after the loop returns, ahead of the write door, so a
  write-class deferral (defer class `write`: the write door did not close cleanly) keeps it; a deferral raised before the
  loop ran has none and no other record either, and is skipped as never ran. Both ends must carry the field: the node that
  served the run emits the object, and the delegator that writes the log row decodes the node's answer into its own wire
  result and encodes it again, so a delegator build without the field drops it. Rows logged before the recording, rows from
  a node that predates it and rows written by a delegator that predates it have none; the row's `delegator_version` names
  the delegator build and the result's `harness_version` the node build. The node stamps `harness_version` once the loop
  has returned without an error and the write door has closed cleanly, so it is absent on a deferral raised before the
  loop, on one the loop's own error raised (a refused lease, a seat that went down, a stall, a wall or ceiling timeout, a
  cancellation, a parser mismatch, a contended seat), on one for a lost workspace root and on a write-class deferral,
  which keeps its pager object but not the stamp; a run that ended on its step budget, a cut tool call, an empty answer or
  an abstention carries both. The pager and compaction unrecorded counts therefore reach zero for rows written by a
  recording delegator from a recording node, and a mixed log reads zero only once the older rows are windowed out by day
  (`--since`) and every node and delegator that wrote the rest runs the recording.
- **What it reports.** The trace-bearing run count (an agent trace with a transcript, or a delegation row with a step
  trace); the pager re-fetch rate pooled over the runs that evicted something (distinct payloads re-fetched over distinct
  payloads evicted) against the 10 % gate of R2-13 (`agent.PagerGate`); the compaction fire rate (runs that ran the ladder at
  least once over runs that recorded a pass count) and passes per run and per step; prefill tokens, cached tokens and KV
  reuse, with the per-run spread (nearest-rank p50, p90 and max) of prefilled tokens over the measured runs and of the mean
  prompt size per step over the runs that reported seat input tokens. Each is given for the whole corpus, by source and by
  seat.
- **Which runs a seat can be named for.** A pool placement logs the pool's ALIAS as the seat (a seat name in which `pool` is
  a whole hyphen-delimited token); the node's real seat is in neither the row nor its result. Those runs cannot be
  attributed to a seat, so `by_seat` holds the runs a seat can be named for (each seat the record names, and under the
  key `(unrecorded)` the runs whose record names no seat, which is every agent trace) and not the alias runs. The alias
  runs are their own bucket: `pool_alias` (the bucket's statistics) and `pool_alias_by_node` (the same runs keyed by the
  node id the row carries, `(unrecorded)` when it names none). The `attribution` block counts the three classes
  (`seat_named_runs`, `pool_alias_runs`, `seat_unrecorded_runs`) and gives `unattributed_share`, the last two over all
  runs, so a reader sees how much of the corpus a per-seat reading cannot reach. Test seats other than the
  default-excluded one stay in `by_seat` until named with `--exclude-seat`. The corpus and the readout carry node ids and
  seat names as the records write them, so both are machine-local like the corpus; only a path-shaped seat, node id or
  stop reason is replaced (below).
- **Paths are redacted.** The shared vet table has no path class, so harvest replaces a seat, node id or stop reason that
  is a local path (a drive path, a rooted path, anything with a backslash) with a stable `[redacted-path-N]` placeholder,
  counted under `redactions.path`, and the strict loader refuses a corpus that still carries one.
- **Unrecorded is not zero.** Before the pager report and the pass count were recorded, neither record kept them: the loop measured both
  and the trace and the wire dropped them. Every rate is a pointer that serializes as `null` when nothing was recorded, and a
  count of unrecorded runs sits beside it, so a corpus of older rows reads `NO_DATA` on the gate and cannot close the pager
  family by reading as 0 %. A run that DID record a pass count of 0 is a real 0. A pager object that contradicts itself is
  read as unrecorded and noted in the harvest stats.
- **What is recorded now.** A standalone trace file carries `pager` (the loop's `PagerReport`, now with `compaction_passes`)
  and `prefill`; the queue drainer resets the loop's instruments before each goal, because they accumulate for the loop's
  life and a goal's trace would otherwise be the running total of the queue. The agent wire result carries `pager` (the
  delegation log stores it) on every run the loop returned from (see "Which rows carry a pager object" above). Rows
  written before that have neither, and the transcript in an older trace gives
  only a lower bound on the ladder (tool turns that are elision markers or skeletons).
- **No token counts is not a measured zero.** An engine that reports no prefill timings still ticks the observed-step
  counter, so a run with steps observed and zero prefilled and zero cached tokens is counted apart (`no_token_counts_runs`)
  and adds nothing to the token sums; the seat's own input-token figure (`seat_tokens_in`) is reported beside it for the runs
  that carry one.

### The `contextbench` task bench (`compaction-eval contextbench`)

`compaction-eval contextbench` is the instrument for the context-editing decision: it drives long-horizon
tasks against one agent seat and grades what the harness kept, so the compaction ladder and the context pager
can be measured on work whose input does not fit the window. It ports the task generators and graders of
[ContextBench](https://github.com/RulinShao/ContextBench) (Apache-2.0; credited in `NOTICE`, and each ported
file names its upstream file and commit). It is unrelated to `kvbench`, which measures KV-prefix reuse.

| Family | What streams | What is asked | Reward |
|---|---|---|---|
| `kv_store` | batches of `SET key = value`, values random and unguessable | `GET key` for keys not announced in advance | share of GETs answered with the right value (memory: nothing can be summarised or guessed) |
| `log_triage` | batches of service log lines | exact-message lookups and level-and-service counts | share of questions answered correctly (needs the exact log) |
| `needle_retention` | chunks of a few needle lines plus a large filler block | nothing: the agent keeps the needles | share of needle lines still present, verbatim, in the final context (retention) |
| `sudoku_sketchpad` | one fully specified move per turn | the agent re-prints the whole board after each move | share of gold board versions it displayed exactly (in-context state maintenance) |

Tasks are generated, not downloaded. Each generator reproduces the original's random stream (the Mersenne
Twister, `choice`, `sample`, `shuffle` and `choices` of CPython), and a test holds the digest of every family's
operation stream against the original generator's output, so `--seed 42` is the published seed-42 instance;
`scripts/contextbench_parity_regen.py` regenerates those digests from a checkout of the upstream repository and
reports any that the tests no longer hold.
The published levels are the `--level` names (`R130` to `R16000`, `L160` to `L12800`, `K4-C4` to `K2-C224`,
`t4` to `t150`); without one, each family runs its smallest level whose uncompacted transcript (the instruction,
every operation and, for `sudoku_sketchpad`, the whole board panel the agent prints after each move) exceeds the
usable budget of record, 32768 tokens minus the 2048 reserved. That is `R1000`, `L1600`, `K4-C24` and `t60`: the
sudoku stream alone is small (about 12k estimated tokens even at `t150`), so it is the panels that overflow, from
`t60` (about 41k) on, where `t30` is about 21k. A test pins each default as the smallest level over the line.
`--tasks N` runs seeds `--seed` to `--seed + N - 1` for each chosen family.

```
local-offload compaction-eval contextbench --family kv_store --tasks 8 --out results.json \
    [--endpoint URL] [--model ID] [--level R1000] [--budget 32768 --reserve 2048] \
    [--delivery tool_result|user] [--gcf] [--skeleton] [--max-turns N] [--pass-at 1.0] \
    [--max-out 4096] [--request-timeout 600] [--config FILE]
```

The endpoint and model default to the configured ones, as for `kvbench`; `--config` names the config file that
supplies them. The other `compaction-eval` subcommands take no `--seat` or `--route`; here `--seat` is an alias of
`--model`, and `--route` accepts only `local`, because the verb drives the one endpoint it is given and places
nothing on the fleet. `--max-out` caps one reply, in tokens (default 4096; a sudoku panel is about a thousand).
`--request-timeout` is the transport deadline of one seat request, in seconds (default 600); a request that hits it
ends the task as `infra_error`. `--max-turns` caps seat replies per task (default 3 x operations + 10). A value the run
would silently replace with a default is refused instead, with the flag's name: `--budget`, `--reserve`, `--max-out`
and `--request-timeout` must be at least 1, `--pass-at` must be above 0 and at most 1, `--max-turns` at least 0
(0 is the default), and `--reserve` must be below `--budget`. The verb runs a model and is never run by the test
suite: the tests inject a fake seat through the same function-valued seam.

How a run works. The transcript starts with a short instruction written for this adapter (the original's
scaffolds are prose and are not copied, so scores compare harness configurations with each other, not with
published numbers). Each operation is delivered as the result of a synthetic `next_op` tool call
(`--delivery tool_result`, the default) because the ladder shrinks tool-result bodies (dedupe, skeleton,
elision) before it drops whole turns; with `--delivery user` operations are user messages, as upstream
delivers them, and only the last rung (dropping older bare messages) can act. Before every send the
transcript goes through `agent.CompactReplay` at `budget - reserve`, estimated with the ladder's own
chars-divided-by-four estimator (the report stamps `size_estimator`), and the compacted transcript is kept,
as the production loop keeps it. The agent advances the stream with `READY_FOR_NEXT_OP` on a line of its own;
the run ends when the last operation has been answered. Every compaction is passed to `agent.PagerStats`,
whose eviction counts are stored per task. In this reply-only setting the agent has no tool to fetch evicted content,
so the stored pager report has basis `not_exercised` and a null `refetch_rate`: it shows what was evicted and never a
re-fetch rate or a gate verdict.

Which snapshot the grade uses. A snapshot is the `[]agent.Msg` transcript at one moment. The run records two per
step: `sent` (what the seat was handed, after compaction) and `turn` (the transcript right after the seat's
reply, before the next compaction). A context that stays over the budget is never handed to the seat and is never
recorded, so every snapshot is something the seat saw. A grader reads one field of a snapshot, `Msg.Content` of every message,
joined by newlines; roles and tool-call arguments are not scanned. `kv_store`, `log_triage` and
`sudoku_sketchpad` scan every snapshot, so a reply is credited once it appeared, even if the ladder
removed it afterwards. `needle_retention` scans only the last snapshot, the context the agent still holds at the
end (when the budget wall stops a run, that is the last snapshot before the wall, so operations the seat never
received earn no credit). The instruction text sits in the scanned context, so it carries placeholders only, never an example
that could pass for an answer (a test pins that the bare instruction scores nothing).

Per-task outcomes use the measurement gate's seven words, and only these:

| Word | When |
|---|---|
| `pass` | graded, reward at or above `--pass-at` (default 1.0: every item right) |
| `task_fail` | graded, reward below it; the reward is kept (0.958 of the needles is a `task_fail` and the number the bench exists to produce) |
| `infra_error` | the seat could not answer: connection refused, no endpoint, a reset mid-run; or the budget cannot hold the instruction plus the first operation, so the seat was never called (the detail says so). Never graded, never a `task_fail` |
| `timeout` | the attempt hit its budget and was not graded: `--max-turns` reached before the stream drained, or (`kv_store`, `log_triage`) the context stopped fitting the token budget before the seat was handed any query, recorded as `term_reason` `budget_wall_before_query`; no reward. Under the default delivery the production ladder cannot hold `log_triage` at `L12800` (its preserve guard keeps every signal-bearing tool result), so a perfect agent ends that published cell here: a deliberate signal about the ladder, explained below the table |
| `judge_error` | the grade could not be computed (no snapshots, empty gold, a sudoku gold without its board side) |
| `not_run` | the run was interrupted before the task finished or began, including an interrupt that lands while a seat request is in flight |
| `cheat` | reserved for the gate's vocabulary; this adapter has no check for gaming a grader and never emits it |

A task whose context stops fitting the budget (`terminated_by` `budget_wall`) ends one of three ways. Before the
seat has answered once, nothing was measured: `infra_error`; the verb refuses a budget too small for the instruction
plus the first operation before any seat call, and driven as a library such a task ends `infra_error`. After that,
and before the seat was handed a single query (the operations from `query_start` on, in `kv_store` and
`log_triage`), no frame can hold an answer, so grading it would score the agent zero for questions it was never
asked: the task is `timeout` with `term_reason` `budget_wall_before_query` and no reward. Once a query has been
handed over, and in the two families that ask nothing (`needle_retention`, `sudoku_sketchpad`), the wall is graded
on the frames the task has. The results JSON holds, per task, the outcome word, the reward (null unless graded),
how the run ended and the term reason if any, turns, operations delivered (appended to the transcript, the one
that did not fit at a wall included) and operations sent (those the seat was handed), compaction count, the largest
context sent (estimated tokens) and the pager report, plus the outcome counts and the mean reward per family; it
has no timestamps. With no seat reachable every task is `infra_error` and the verb exits non-zero saying nothing
was measured.

One published cell ends as a `timeout` for a perfect agent, on purpose. Under the default `tool_result` delivery the
production ladder cannot hold `log_triage` at `L12800`: its preserve guard keeps every tool result that still
matches a signal word (error, fail, warn, timeout, refused and the like), the synthetic log lines carry `[WARN]`,
`timeout` and `refused`, and tool messages are almost the whole transcript, so collapsing old pairs makes no room.
An oracle that answers every query from the gold stops at the wall after 221 or 222 of the 262 operations were
sent (all eight published seeds), and the task ends `timeout` / `budget_wall_before_query`. The level is left as
published rather than regenerated to dodge the guard: the cell is a signal about the ladder, not a score, and a test
pins it. Every other family's largest level passes the oracle under the same delivery. `--delivery user` reaches
only the drop rung and passes that cell, which is why the choice of delivery is reported beside every result.

Four of the graders' rules are stricter or more forgiving than a literal reading of the answer block, and each has a
test. A `kv_store` answer is matched on its key without surrounding quotes or case, and its value must stand as a
whole (the characters around it are not letters or digits), so the same words with a longer tag do not count. A
`log_triage` count answer is read after a repeated `Query N` or `qid=N` label is removed and digit groups such as
`12,800` are joined, and it still must state exactly one number.

What it does not do. It is not a measurement: no run is recorded in this repository and no decision follows from
the bench alone; the context-editing decision record comes after a leased measurement run. Instance bytes
match the original; the instruction wording and the operation delivery (`tool_result`) do not, so a published
score is not comparable with one from this bench.

### The `browse` tool (opt-in, 0.141.0, ADR 0060)

`browse` (`url`, `goal`, `max_actions`, `security_risk`) drives the operator's own running,
logged-in Chromium browser toward a natural-language goal through the browse lane's pinned sidecar
([browse-lane.md](browse-lane.md)). It is off by default and is granted only when the lane is
configured (`browse_python`, `browse_script` and a loopback `browse_decision_url`). The policy broker
has the action kind `browse`, with the start URL's lowercased host as the subject, so a `--rules`
entry such as `{"kind":"browse","glob":"*.bank.example","decision":"deny"}` works, and every call is
audited. A page's visible text and element labels go to the loopback decision endpoint, which may
front a hosted decision model.

| Door | Grant | Deny-list | Host allowlist |
|---|---|---|---|
| CLI `local-agent --allow-browse --browse-hosts host1,host2` | unattended (the CLI always builds unattended: ask becomes deny) | can never be lifted | required; the grant is refused without it |
| `agent_run` / `agent_delegate` with `allow_browse` and `browse_hosts` | unattended | can never be lifted | required, non-empty |

Every agent door is unattended for browse and needs an audit path (the CLI defaults to
`agent-audit.jsonl` under the harness install root: config `home`, else `<HOME>/.local-offload`). The only attended browse door is the MCP tool
`offload_browse`. The contract doors also need the node's `agent_allow_browse: true` and the lane
configured on the node that runs the browse. A contract carrying `allow_browse` must have `route: local` (intake rejects any
other), delegation placement never sends it to a remote node, and a fleet node refuses it at ACK
without the opt-in. An unattended grant without a host list is refused: the tool is not granted, and the run's note
says why.

## Delegation surfaces

Since 0.65.0 the same loop can be driven by a **delegation contract** — a self-contained
`{goal, context docs, output_schema, acceptance}` unit placed on this box or on a fleet node
over the operator's tailnet. Two surfaces exist, both delegator-side:

- **MCP `agent_delegate`** — registration is gated on config `agent_delegation_enabled`, so
  `tools/list` is byte-identical when the delegator role is off. Fans out 1–8 subtasks with
  bounded concurrency; placement is quality-first, then by expected completion (ADR 0050, 0.128.0):
  seats that fail the adequacy gate are never candidates; among adequate seats the one with the
  earliest expected completion wins (cold load + queue wait + the time to produce a reference final at the seat's rate, ADR 0079),
  two candidates within 20 % are resolved by a seeded power-of-two draw, and a local seat counts as
  busy when a job is in flight or loading, not only when the GPU lease is held;
  results are verified against the contract's acceptance DSL **by the delegator** before
  anything counts as done.
- **CLI `local-offload delegate --contract file.json`** — the same intake, engine, and response
  JSON, for testing and scripts (template contracts: [`contracts/`](../../contracts/README.md)).

Both surfaces publish the same summary-first response, and both read the same way: a
`deferred` subtask carries a `defer_class` (`abstention` | `budget` | `infrastructure` |
`config` | `contract`), and the `infrastructure` + `config` ones are counted separately in
`summary.infrastructure` — because a node whose llama-swap is down defers every subtask and must
not read as a clean run. `contract`-classed defers are the caller's own contract, not a broken
box, and stay quiet ([defer classes](fleet-node.md#defer-shapes-and-their-classes)) — but only
when the fleet is positively established as healthy: with any node unreachable, or any agent lane
advertising no `agent_ctx_tokens` ceiling at all, the loud class wins and the reason names both
causes.

`summary.infrastructure` covers two situations a caller acts on differently, so
**`summary.lost_to_stack`** (omitted when zero) splits out the one that is lost WORK: subtasks
that **delivered no usable result — the contracted output never arrived** because the stack failed
them. The remainder of `infrastructure` is a successful LOCAL placement annotated with "the fleet
was down" — nothing lost.

It counts the missing DELIVERABLE, not missing bytes, and one counted member proves the
difference: `structured re-pack unreachable` fires after the agent loop has already FINISHED, so
that result publishes `output` populated, `structured` absent, `defer_class: "infrastructure"`.
That is lost work only when the DELEGATOR's rescue also fails (register C-66, PR-4): the node flags
such a defer `schema_miss`, and the delegator — which holds the finished answer — re-packs it
itself before acceptance, down a ladder (`pipeline.RescueRepack`): the lossless reading first; then
ONE completion on its own agent seat when that seat is resident (not a registered run); when it is not,
ONE chat completion on the node that wrote the answer, on the seat that wrote it, through that node's
chat door with the fleet bearer (the delegator hands the rescue that node in its context,
`delegate.WithRescueOrigin`; the node has just decoded the answer and holds the seat hot, waits for no
load here, and never queues behind this box's GPU lease, because the call carries `WithLocalBusy` and
leaves the box or does not run); and last this box's seat, warmed first outside the allowance, with the
budget the node left. The node is not handed over when its own re-pack failed for its seat's health (defer
class `infrastructure`: a stall, an unreachable or busy seat, a seat lost under the run): the seat that just
wedged is the last place to send the rescue first, and a call there that hangs would spend the whole budget
(its allowance is at least the 120 s flat bound, the rescue's budget floors at 120 s) and leave this box's own
seat no try. A budget defer, an abstention and a node that names no class keep the node. A `route: "queue"` answer hands none: the base the
delegator holds for it is the queue holder's, which is not the node that ran the job and has no chat door for the claimant's seat,
so the queue lane's rescue goes to this box's own seat (the row still reads `ran_on: "node"`). The seat that wrote
the answer is the wire's, else the one the run was placed on. An unreadable `/running` leaves the rescue on
its own seat, as before, and now says so in the log. The rescue
validates the object, reads it against its answer (see "A re-pack is read against its answer" below),
and lets the ordinary delegator-side acceptance decide; a failed rescue names every rung it tried, the
node and the warm-up included. (A rescue on this box's cold seat under another session's lease used to
queue behind the lease and be cut at its 120 s allowance.) A node too old to publish the flag is
recognized from `stop_reason: done` and the reason prefix, and a defer whose reason says the CALLER's
context ended is never rescued. The delivered result carries `repack_note: "rescued on <seat> …"` and
is counted as a success; prose nothing validated is still never one, so a rescue that cannot produce a
validated object leaves the defer exactly as the node sent it, and it is still counted here. The rescue
is wired on `agent_delegate`, `offload_research` and the CLI verbs `delegate` and `research`. It is
deliberately not wired on three doors, for three different reasons: `agent_run` at a route carries only the flat
`{answer}` `output_schema` the delegator requires of every remote run (register D-125), so there is nothing worth
structuring, and a deferred call carries the node's finished prose as `output` flagged `schema_miss`, as `offload_ask` does; the review lane's fenced-seat fallthrough exists
because the local seat is the one another session's lease fences, and a rescue there would queue behind
that very lease (the lane instead reads a skipped or cut re-pack's raw lines itself, through its own
filters and without a second completion: `reviewlane.Salvage`, see [mcp-server.md](mcp-server.md)); and `offload_ask` at a named remote route keeps its own handling of a finished answer
(its prose still reaches the caller: a deferred `offload_ask` carries it as `output`, flagged `schema_miss`), which the security review records as deliberate, so wiring it is
outside this change. The same holds for the cross-seat retry an `offload_ask` gives a failed local first attempt
(the single-contract path at `route=remote`, [mcp-server.md](mcp-server.md)): a retry that defers keeps no rescue and
none of its prose, and the first answer stands with the retry's fate in `retry_note`. The seam is one field, `RunOptions.Rescue`, if any of them should be wired later.

**A re-pack is read against its answer.** The schema validator says an object has the right shape,
not that it kept the content, and the acceptance checks read fields (`contains:` the answer's text,
`nonempty:` one scalar). A seat whose final answer was itself a JSON object with keys of its own
(a list of eleven `{quote, release_note_says}` under `outdated_sentences`, where the schema asks for
`stale: [string]`) was re-packed into `{"stale": [], "up_to_date": true, ...}` and delivered as a success,
because the re-pack prompt said "use empty values when a field is absent" beside a text none of whose keys
had the requested names. `core.ListsEmptied` is the check: a list the schema asks for that the answer
holds under the same path is compared with that list alone (an answer that says `"stale": []` has nothing
to lose); only when the answer shares no list path with the schema do the lists it holds under other paths
count (a list nested under an object counts, at its path, `report.outdated`; an answer that is itself a JSON
array, bare or fenced, is one list; a property declared `type: ["array","null"]` is a list), and then the
object must hold some item in the lists the schema asks for. An object that carries the list itself lost
nothing, and a prose answer is not judged (nothing can be counted, so a prose answer whose list was lost is
published). The node refuses such an object on every lane
(the next attempt carries the finding ahead of the text; all refused is an abstention with the answer in
`output`), `EvalAcceptance` reads every result the same way (a failed verification, which the delegator
retries on another node), and `RescueRepack` refuses a hollow rescue. The prompts changed with it: a text
that is a JSON object (or array) is told so, with its keys or items described (shapes and counts, never
content), and the bound a schema puts on a list (`maxItems`) is stated on every lane. A result that kept
fewer list items than its answer held says so in `repack_note` (`kept 10 of 11 list items from
"outdated_sentences" (maxItems 10)`); a partial loss is a note and not a refusal, because the object is
faithful as far as it goes, the answer rides beside it, and a threshold would also refuse an object whose
list is legitimately shorter than an unrelated list of the answer's (the two cannot be told apart by key).
The cost of the check is a false refusal in one shape: an answer that lists what it checked under another
key, beside a schema list that is legitimately empty, is refused, because nothing deterministic tells the
two lists apart. What that costs is an abstention defer with the answer attached, then the delegator's
rescue (which refuses the same object), then a re-placement: the delegator re-places an abstention once on
another node, which is a SECOND FULL AGENT RUN, and a truthful answer reproduces the refusal there. It is
not a wrong success: the attempt after a refusal names only the lists that were lost, sends a list into the
field of the same name, and for any other list says to copy it only if it is the content of a requested
list field, and that a field the text says is empty stays empty (a prompt that told the seat to write the
text's lists into the list fields made an obedient seat publish the files an answer had checked as stale
ones).

The two surfaces report that verdict differently, on purpose, and they agree on the case that
matters. **The CLI exits non-zero on `summary.infrastructure > 0`** — an exit code sits beside the
printed results, so it can say "look at this" without denying the results. **The MCP tool sets
`isError` when nothing succeeded and `summary.failed > 0`, `summary.lost_to_stack > 0` or
`summary.skipped > 0`**: a call that delivered no usable result is loud whether the stack or a
transport error ate it. A call that delivered **at least one** usable result is a *partial*
result and is NOT flagged (C-75, [ADR 0065](../architecture/decisions/0065-the-whole-call-has-a-deadline-below-the-clients-abort.md)):
`isError` means *the call failed*, and the MCP client answers an error-flagged body by keeping only
its head and tail, which cut the surviving digests out of every partial research reply. The lost
subtask stays loud in the body — `summary.failed` / `summary.lost_to_stack`, its own `failed` /
`defer_class` / `reason` — instead of in the flag. The same reasoning is why the fleet-down run
that still delivered every subtask (`infrastructure > 0`, `lost_to_stack == 0`) is non-zero on the
CLI and a quiet success on MCP: flagging a run whose subtasks all completed, validated and passed
acceptance is how a model comes to discard or redo correct work. The body is identical either way,
so the summary and every per-subtask reason are always there to read.

The summary also carries `corpus_rows_lost` / `ledger_rows_lost` and, whenever one is non-zero,
its `corpus_rows_attempted` / `ledger_rows_attempted` denominator (all `omitempty`): telemetry
never fails the work, but a delegation that recorded nothing used to publish byte-identically to
one that recorded everything — including the total-loss case where the ledger file never opened
at all — and "N rows lost" is unreadable without the M it was lost out of.

A **local** placement is not a special case: it runs the identical `Pipeline.Run` route a fleet
node runs (`pipeline.RunAgentContract` → the `agent` task → this loop, read-only over the
contract's materialized context docs, profile resolved contract > configured `agent_profile` >
`general`, no write/run/fetch/github tools), so local and remote results share one execution
semantics.

The one deliberate difference is `output_schema`: **remote** placement requires it (the gate's
mechanical-verifiability condition, and the node refuses a schemaless contract at ack), while a
local run may omit it. The re-pack constrains the seat with a raw GBNF grammar on llama.cpp and with
vLLM's `structured_outputs: {"json": <schema>}` on a seat `vllm_seats` declares, which is what stopped
vLLM seats answering unconstrained and then paying for it in re-pack attempts (register D-129; see
[ADR 0002](../architecture/decisions/0002-grammar-reliable-serving-flags.md)). A schemaless local
contract skips the structured re-pack entirely and
returns its `output` with `structured` empty — it is a plain success, not a defer, and the
contract's text-verb acceptance (`contains:` / `not_contains:` / `regex:`) is what verifies it.

An in-loop `delegate_subtask` tool — the loop delegating onward itself — is **deliberately
parked to v2**; no delegate tool is registered for any caller today, which is what makes the
hop limit structural. Contract wire shape, auth, and placement:
[fleet-node.md](fleet-node.md#the-agent-task-task_type-agent) and
[ADR 0023](../architecture/decisions/0023-agent-lane-tailnet-auth-and-locality.md).

## Data and state

- **Audit trail** — append-only JSONL, mode `0600`, at `agent-audit.jsonl` under the harness install
  root by default (config `home`, else `~/.local-offload`; on Windows `home` is on a data drive, never the
  OS drive). Resolved only when a mutating capability is enabled (the CLI) or browse is asked for (the
  agent doors), unless `audit_all_doors` says otherwise (register SF-02): `off` (the default) keeps
  that behaviour; `warn` attaches the trail to every agent door (`agent_run`, `agent_delegate`, fleet
  contracts), writes every broker decision and reports a failed write once on the log without changing
  any decision; `enforce` makes a failed write deny the action, as a browse run's trail always has, and
  refuses the run (a `config`-class defer naming the key) when no audit path resolves. A browse run keeps
  its enforcing trail in every mode. `enforce` is the operator's call: it adds a failure mode to doors
  that never had one, so it follows a measured clean window under `warn`. A row is written only for a
  BROKERED action (write, delete, fetch, shell, run, browse, park): a read-only run (`agent_run` without
  browse, a contract without `write_root`) attaches the trail but leaves no row, so a clean window under
  `warn` vouches for the write-door contracts that ran in it, not for read-only ones. The local-agent CLI
  keeps its own `--audit` behaviour (a trail when a mutating flag is set) and does not read this key.
  With `audit_chain` on (register SF-08) every row of a run carries `run_id`, `seq` and `prev_sha256`
  (the SHA-256 of the run's previous row as written) and a run that recorded anything closes with a
  `run_end` row (`count`, `head`); a run with no decision writes nothing. `local-offload agent-audit verify
  [--strict]` names the run and seq of an edited, removed or reordered row INSIDE a run, reports a run with no
  `run_end` as OPEN (a crash, or a cut tail: the chain alone cannot tell them apart, so `--strict` fails open
  runs) and rows that still chain after their `run_end` as LATE (a tool goroutine abandoned at a timeout). It
  cannot see a whole run removed, edits to a `run_end`'s other fields, or an edit re-chained from that row on:
  the chain is unkeyed, and an off-node witness of run heads (register S-08) closes that and is not built. The
  local-agent CLI's `--serve` mode is one chained run for the server's lifetime, closed when serving ends.
- **Ask queue** — sibling file for deferred approvals and parked high-risk calls. Until the install root governed these
  files they all lived under `~/.local-offload`, so on a node that sets `home` the trail, the ask queue
  and the traces start new files under it and the old ones stay where they were: nothing moves or merges
  them (merge by hand if you want one history, then delete the old file). The agent says so once per
  process, naming both paths, whenever it finds the old one.
- **Ask queue** — sibling file (`agent-asks.jsonl`, same root) for deferred approvals and parked high-risk
  calls.
- **Worktree memory** — an `AGENT.md` loaded into context on a re-injection cadence.
- **Traces** — optional per-run transcripts.

Both the audit trail and the ask queue **must live outside the worktree**. This is enforced at
construction: the builder fails outright if either path resolves inside, because the agent's own
write and shell tools could otherwise clobber the record of what it did.

## Interfaces and entry points

- One-shot: `local-agent --root . "task"`.
- Queue mode and `--serve` (OpenAI-compatible endpoint, loopback-only — see
  [ADR 0005](../architecture/decisions/0005-loopback-only-serve.md)). `--serve` sits behind the SF-16 ingress guard
  (`fleet_ingress_guard`, see [fleet-node](fleet-node.md)): under `enforce` a POST must be `Content-Type:
  application/json`, a `Host` outside loopback, the tailnet and the configured zones is refused, and so is any `Origin`.
  The banner prints the mode.
- `agent_run` via the MCP surface.
- [`internal/agent/unattended-rules.json`](../../internal/agent/unattended-rules.json) — the
  default table EMBEDDED in the binary and loaded automatically on unattended runs with no
  `--rules` argument (so it needs no installer and no path).
- [`examples/agent-rules.json`](../../examples/agent-rules.json) — the alternative starter
  risk-rule table for operators writing their own, loaded with `--rules`. It is a **repo file,
  packaged by no installer**: from a checkout, pass `--rules examples/agent-rules.json`; from an
  installed binary, copy it somewhere durable (e.g. `~/.local-offload/agent-rules.json`) and pass
  that path.

## Dependencies

`internal/pipeline` (recordless path), `internal/sandbox`, `internal/netguard`, the local completion
endpoint.

## Downstream effects

Loosening a default here changes the safety posture of every consumer, including `agent_run` over
MCP. Capability defaults are an interface, not an implementation detail.

## Invariants and assumptions

1. Every `--allow-*` capability defaults to **off**.
2. Writes are confined to the worktree, enforced via `os.Root` rather than string comparison.
3. `.git` is denied unconditionally by the broker, per path segment, with case and trailing-character
   normalization. `.gitignore` remains writable.
4. The audit trail lives outside the worktree, enforced at build time.
5. An action that cannot be audited does not happen.
6. `--serve` refuses a non-loopback bind without `--listen-trusted-network`.
6b. `--serve` is behind the ingress guard (`fleet_ingress_guard`). The default, `warn`, logs a request that `enforce`
   would refuse (a foreign `Host` 421, any `Origin` 403, a POST that is not `application/json` 415) and serves it, so
   `enforce` is opt-in; a cross-site `text/plain` POST needs no CORS preflight, which is why a POST must be JSON before
   it can reach the tool loop. `--serve` refuses to start on a config that failed to load, because the guard's mode
   comes from that config. The guard is not authentication.
6a. Environment rules can only NARROW (deny/allow never add a tool; caps only lower) and never
   touch an EFFECT decision; a table that does not validate fails the build by name; a nil table
   changes nothing. Per-run rule state never crosses runs of a shared `Loop`.
7. **Every `Chat` call takes the Model Affinity Gate** (`internal/modelaffinity`) before it POSTs.
   The agent seat shares one llama-swap endpoint with every cascade text seat in the default config,
   and this path does NOT go through `internal/llamaclient` — so a gate installed only on the
   cascade side would be a lock only one side takes, and the agent seat would keep losing its model
   mid-run (measured: 72 s -> 307 s per call). Sequential steps of one loop always name the same
   model, so the gate is a no-op for a run that has the seat to itself. See
   [ADR 0025](../architecture/decisions/0025-model-residency-is-arbitrated-in-process-by-base.md).

## Error handling

Tool errors become `is_error` results the model can react to; the loop never panics on tool failure.
One report ends the run instead (register C-79): a tool that cannot open the workspace root returns an
`agent.RootGoneError`, and `Loop.Run` returns it (the result's `stop_reason` is `error`) once the turn's
calls are recorded, so the seat is not given another turn to answer from nothing. The pipeline files the
run as an `infrastructure` defer ([defer shapes](fleet-node.md#defer-shapes-and-their-classes)); a file
that is missing inside a root that opens stays an observation. A run with a write door asks the same question
itself (`agent.CheckWorkspaceRoot`) when the door opens and again before it renders its diff, because a tree
lost before the first or after the last tool call has no tool to report it. A tree it finds gone is never rendered as a diff;
it becomes the run's cause only when the loop ended cleanly (a cause the loop ended on itself, such as the ceiling, keeps its class
and the `write_note` says the write set was withheld).
A `Chat` call can also fail before it reaches llama-swap, with a `*modelaffinity.WaitError`: the
seat was parked behind another model's batch for longer than its bound. The error names the base,
the model wanted and the model that held the slot.
Throttle refusals are fed back as ordinary tool results with explicit instructions to move on.

## Security and privacy notes

Confinement is **asymmetric by platform, and the weaker side is disclosed**. Linux uses user,
network, and PID namespaces plus seccomp and Landlock, failing closed if the Landlock ABI floor is
not met rather than running uncaged. The floor is ABI 4 (Linux 6.7, `sandbox.NetABIFloor`, 0.115.22):
the V4 rule set denies every TCP bind and connect only from that ABI on, and below it best-effort
mode keeps the path rules and silently drops the network ones — so a lower floor ran the cage with
half of a shipped guarantee missing (register H-27; the fleet's Linux node runs 7.0). Native Windows uses a Job Object plus a low-integrity token:
writes outside the worktree are blocked by MIC, but **network egress is not severed and reads outside
the worktree are not blocked**. The source calls this "HONEST RESIDUAL RISK (documented, not hidden)"
and the tool description the model sees says the same.

**What the cage could not do for a run is said, not swallowed** (`internal/sandbox`, `Result.Stderr`).
A step that confines or un-confines a run can fail without making the run unsafe to start, and the
caller still has to hear of it. On native Windows, a worktree or scratch folder whose low-integrity
label cannot be lowered (the label change needs `WRITE_OWNER`, which a data drive whose root ACL names
the user nowhere does not give) is a folder the command cannot write; one whose label cannot be
restored is left writable by every low-integrity process, and the next run takes that label for the
folder's own, so it does not heal; one whose prior label cannot be read is cleared, not restored; a
deadline whose kill of the job fails is not in force; a command whose exit status cannot be read
(`GetExitCodeProcess` fails) reports exit code -1, not the 0 that every caller reads as success. On
Linux, `<worktree>/.git` stays writable where the kernel refuses its read-only mask: the empty tmpfs
over a `.git` directory, or the read-only bind of a `.git` file onto itself (below); and a `.git`
that is a symbolic link cannot be masked at all (below). In each case
the command still runs (failing closed is a separate decision, not taken) and the cage puts a line
starting with `[sandbox] ` ahead of the command's own stderr, appends the same text to the error of
a run that fails before the command starts, and, on Windows, logs it once. `run` and `run_shell`
return stderr to the model, so the model reads the note too.

**The shell path's `.git` mask covers a linked worktree** (`sandbox.maskDotGit`, Linux). With a
writable worktree the cage makes `<worktree>/.git` read-only inside its private mount namespace, so
a command can neither plant a git hook nor aim the host's git at a directory of its own. A `.git`
DIRECTORY gets an empty read-only tmpfs. A `.git` FILE, which is what `git worktree add` leaves in
a linked worktree (`gitdir: <repo>/.git/worktrees/<name>`), is bind-mounted onto itself read-only:
it still reads, since git finds its git directory through it, and it cannot be rewritten,
truncated, appended to, unlinked, renamed over or renamed away, nor reached through a second name (a
link to it is refused, because the new name would be on another mount). The git directory itself is
outside the cage's write roots already; the pointer in the worktree is what the mask protects. The
remount names every flag a user namespace locks on the mount (read-only, nosuid, nodev, noexec) and
no atime flag, so it works on a worktree under a mount that carries them (a shared-memory mount is
nosuid and nodev, as is a tmpfs `/tmp` on many systems). A `.git` that is a symbolic link no mount
can protect, because a mount follows the link and lands on its target, and the link itself sits in
the writable worktree: a command can replace it and point git at a directory of its own. The cage
says so in a `[sandbox]` note (`<worktree>/.git is a symbolic link: it cannot be masked read-only,
...`) for a link to a file, to a directory and to nothing, and the run goes ahead. A link to a
directory keeps what the cage always did with it, the read-only tmpfs over the target.

**The caged child's environment is built, never inherited, on both platforms** (`sandbox.cageEnv`,
`sandbox.EnvAllowlist`). Linux execs with a fixed `PATH` plus `HOME`/`TMPDIR` pointed at the run's
scratch dir; Windows passes CreateProcess an explicit UTF-16 block of the same shape — a closed
copy-by-name allowlist (`SystemRoot`, `SystemDrive`, `windir`, `PATH`, `PATHEXT`, `COMSPEC`,
`NUMBER_OF_PROCESSORS`, `PROCESSOR_ARCHITECTURE`, `OS`) plus `USERPROFILE`/`HOME` and
`TEMP`/`TMP`/`TMPDIR` set to the scratch dir. Nothing else crosses: no `GITHUB_TOKEN`, no
`MEM0_API_KEY`, no fleet token, no `APPDATA`/`LOCALAPPDATA` pointing at the real profile. Until
0.117.7 the Windows side passed a nil `lpEnvironment`, which means "inherit the caller's block",
so a `run` child received the delegator's whole environment. The proof is
`TestWindowsChildDoesNotInheritParentSecrets` and `TestWindowsChildGetsTheAllowlistedEnvironment`
(`internal/sandbox`); they are skipped stubs on every other system, so CI runs them on a Windows
runner (the `installer-windows` job) and requires their `--- PASS:` lines, because `go test` exits 0
for a skipped or unmatched test (D-98).

**Reads of secret material pass a read floor** (register SF-07, `internal/agent/readfloor.go`).
Every tool that reads file content consults one gate: `read_file`, `summarize_file`, `search_files`,
`edit_file` (it reads the old content, and "not found" versus a refusal would answer whether a guess is in
the file) and `github_upload_file` (it sends the file off the box). The built-in floor covers `.env*`
(except `.env.example` and `.env.sample`), `*.pem`, `*.key`, `*.p12`, `*.pfx`, `id_rsa*`, `id_ed25519*`,
`id_ecdsa*`, `id_dsa*`, `.npmrc`, `.netrc`, `.pypirc`, `.git-credentials`, `.claude.json`, and everything under a
`.ssh`, `.aws`, `.gnupg`, `.kube` or `.env` directory, folded the way the write floor folds names (case,
trailing dots and spaces); a rule table may add rules of kind `read` (tighten-only). A path is checked as given
and in its resolved form, so a symlink to a secret and a Windows 8.3 short name (`ENV~1.PRO`, `SSH~1`) are the
file they name; under `enforce` a `~N` short-name segment is refused outright as well. The mode is
`agent_read_floor`: `warn` (the default) lets the read through unchanged and records a `warn` row on the audit
trail (when one is attached, see `audit_all_doors`) plus a log line, and on an ENFORCING trail (a browse grant or
`audit_all_doors=enforce`) a read the trail cannot record is refused; `enforce` refuses the read (the broker's
not-performed convention, `read refused: <reason>`, a `deny` row recorded). Under `enforce`, `search_files`
walks the tree and skips a floored file or directory before reading a byte of it, and says nothing about what it
skipped, so a pattern cannot probe a secret. `off` checks nothing, and `--rules off` switches the floor off with
the rest of the table (the build says so). NOT covered: the run/shell cage (an allowlisted `python`, `node` or
`git` can read any file it reaches, and on native Windows reads outside the worktree are not confined either),
hard links (a second name for the same content, which no path check can see), and the media tools that read an
absolute image path. Both search backends now search dotfiles (`rg --hidden`, `.git` excluded), as the Go walk
always did.

> **Known gap:** the read-only `.git` mask that protects the shell path is Linux-only. On native
> Windows the `run` path has no equivalent, while `git` is on the allowlist and the worktree is
> temporarily low-integrity during a run. The broker's `.git` denial still covers the file tools on
> every platform. Recorded in
> [ADR 0004](../architecture/decisions/0004-worktree-confinement-audit-outside.md).

## Observability and debugging

Read the audit trail first — it records what was allowed, denied, and why, including which risk
rule fired at what severity. The effect ledger (`effects` / `effects_flagged` on `agent_run`, the
flagged-effects print on the CLI) answers "did anything end in `unknown` or get parked?".
`StopReason` distinguishes budget exhaustion from completion. Throttle refusals appear in the
transcript as "NOT executed" messages. The step `trace` (`rules_fired`, `env_rules`) on every
agent result says per call which tool ran, what became of it, how much the model read, and which
environment rule decided — read it before the transcript.

The build itself warns once, before the loop starts, when an unattended run holds destructive
capability with the rule table explicitly disabled (`--rules off`): `[local-agent] UNGATED: …` on
stderr, and on `built.Notes` for an embedding caller. Its **absence is not an all-clear** — it
fires on that one condition only (see "The `UNGATED` build note" under *How the system works*), so
an attended run, or an unattended run without
`--allow-delete`/`--allow-overwrite`/`--allow-shell`/`--allow-github`, stays silent whether or not
a rule table is loaded.

## Testing notes

`internal/agent/` covers the broker (including `.git` normalization cases), the risk-rule table
(`rules_test.go`), effect accounting and parking (`effects_test.go`), the batch judge, the throttle
ordering, write-tool scoping and TOCTOU behavior, profile narrowing, and two-tier plan handling.
`examplerules_test.go` pins the shipped starter table: that it loads, that it stops an unannotated
delete, and that an unattended destructive build announces the default table (never `UNGATED`)
while a read-only build stays silent. `unattendedrules_test.go` pins the embedded default table:
that it loads tighten-only, that a delete parks WITHOUT any reliance on the model-declared risk
(with the fired rule as structured queue fields), that config/manifest writes gate while ordinary
source writes do not, that `--rules off` is ungated-and-announced, and that an operator table
replaces the default. `cmd/local-agent/serve_test.go` covers the loopback guard.

## Common pitfalls

- Expecting the broker to enforce step or tool caps. It does not — that is the loop.
- Assuming `--profile` and `--two-tier` are unconditionally exclusive. Only a non-default profile
  conflicts.
- Assuming the Windows cage is equivalent to the Linux one. It is weaker, deliberately and visibly.
- Reading an `Access is denied` from a Windows `run` as the command's own failure. Look at the top of
  stderr first: a `[sandbox]` line there says the cage could not make a folder writable (or restore its
  label), and why.
- Pointing `--audit` inside the worktree — a startup error, on purpose.

## Source map

- [`internal/agent/loop.go`](../../internal/agent/loop.go) — loop, budget, `dispatchOrThrottle`
- [`trace_corpus.go`](../../trace_corpus.go) — the `trace-corpus` verb (harvest, readout, freeze, check)
  over recorded agent runs; [`internal/agent/contextpager.go`](../../internal/agent/contextpager.go) — the pager
  instrument and the compaction pass count it records
- [`internal/agent/policy.go`](../../internal/agent/policy.go) — broker, `.git` denial, audit append
- [`internal/agent/rules.go`](../../internal/agent/rules.go) — the tighten-only risk-rule table and
  the built-in `defaultRules()` secret-material floor
- [`internal/agent/readfloor.go`](../../internal/agent/readfloor.go) — the read floor (`ActRead`, the
  shared read gate every read tool consults, `agent_read_floor` modes)
- [`internal/agent/unattendedrules.go`](../../internal/agent/unattendedrules.go) — the embedded
  default unattended table and the `RulesOff` escape hatch
- [`examples/agent-rules.json`](../../examples/agent-rules.json) — the shipped starter table for
  operator-authored rules
- [`internal/agent/effects.go`](../../internal/agent/effects.go) — effect statuses, `NotPerformed`,
  the `security_risk` park logic, `obs_chars` / `rule` per record
- [`internal/agent/envrules.go`](../../internal/agent/envrules.go) — the environment-rule
  interceptors (ADR 0036); [`internal/core/agentenvrules.go`](../../internal/core/agentenvrules.go)
  — the vocabulary, `Validate`, the trace step; [`examples/agent-env-rules.json`](../../examples/agent-env-rules.json)
- [`internal/recipe/`](../../internal/recipe/) — the recipe record, store and scoring classes (ADR 0036 amendment);
  [`recipe_audit_cmd.go`](../../recipe_audit_cmd.go) — the `recipe-audit` verb
- [`internal/agent/batchjudge.go`](../../internal/agent/batchjudge.go) — the advisory end-of-run
  judge
- [`internal/agent/runtool.go`](../../internal/agent/runtool.go) — allowlist, direct exec
- [`internal/agent/writetools.go`](../../internal/agent/writetools.go) — `os.Root` scoping
- [`internal/agent/builder.go`](../../internal/agent/builder.go) — capability grants, audit-path check
- [`internal/agent/twotier.go`](../../internal/agent/twotier.go), [`profiles.go`](../../internal/agent/profiles.go), [`compaction.go`](../../internal/agent/compaction.go), [`skeleton.go`](../../internal/agent/skeleton.go), [`rung_logs.go`](../../internal/agent/rung_logs.go), [`rung_search.go`](../../internal/agent/rung_search.go), [`internal/compeval/`](../../internal/compeval/)
- [`compaction_contextbench.go`](../../compaction_contextbench.go) — the `compaction-eval contextbench` verb;
  [`internal/compeval/contextbench_run.go`](../../internal/compeval/contextbench_run.go) (driver, outcomes),
  `contextbench_gen_*.go` and `contextbench_grade_*.go` in [`internal/compeval/`](../../internal/compeval/) (ported generators and graders)
- [`internal/delegate/`](../../internal/delegate/) — delegator-side placement, intake, execution
  engine, acceptance evaluation
- [`internal/pipeline/agenttask.go`](../../internal/pipeline/agenttask.go) — contract execution
  through this loop (local and fleet placements alike)
- [`internal/pipeline/coherence.go`](../../internal/pipeline/coherence.go) — the post-warm seat
  coherence probe (register D-118); [`internal/agent/degenerate.go`](../../internal/agent/degenerate.go) is its
  NaN-shape detector
- [`internal/sandbox/`](../../internal/sandbox/) — platform cages
- [`cmd/local-agent/`](../../cmd/local-agent/) — CLI and server
- [`internal/agent/client.go`](../../internal/agent/client.go) — `LLMClient.Chat`, the OpenAI wire
  types, and where the Model Affinity Gate is taken
- [`internal/modelaffinity/affinity.go`](../../internal/modelaffinity/affinity.go) — the gate

## Related docs

- [browse-lane.md](browse-lane.md)
- [../architecture/decisions/0060-opt-in-browse-lane-drives-the-operators-browser.md](../architecture/decisions/0060-opt-in-browse-lane-drives-the-operators-browser.md)
- [../architecture/decisions/0003-policy-broker-and-capability-flags-off-by-default.md](../architecture/decisions/0003-policy-broker-and-capability-flags-off-by-default.md)
- [../architecture/decisions/0036-the-agent-lane-is-a-harnessed-environment.md](../architecture/decisions/0036-the-agent-lane-is-a-harnessed-environment.md)
- [../architecture/decisions/0004-worktree-confinement-audit-outside.md](../architecture/decisions/0004-worktree-confinement-audit-outside.md)
- [../architecture/decisions/0005-loopback-only-serve.md](../architecture/decisions/0005-loopback-only-serve.md)
- [../architecture/decisions/0025-model-residency-is-arbitrated-in-process-by-base.md](../architecture/decisions/0025-model-residency-is-arbitrated-in-process-by-base.md)
- [../OPERATOR-GUIDE.md](../OPERATOR-GUIDE.md)
