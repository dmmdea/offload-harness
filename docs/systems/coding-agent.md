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
Two independent limits keep a weak model from burning the budget on nothing:

- **A step budget** — the loop stops with `StopReason: "budget"`.
- **Tool-call caps** — `dispatchOrThrottle` sits between the model's request and execution. It
  refuses an exact repeat (same tool, byte-identical arguments), and it caps calls per tool name
  (`--max-same-tool`, default 3). A tool that breaches the name cap is *also removed from the tool
  list sent on every later request* — structural enforcement, added after a 9B model re-issued an
  already-refused identical call seventeen times in a row.

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
machinery, so claiming them would be decoration.

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
fitting). The chars/4 estimate never decides a drop on this path, in either direction: the
cut's own REAL-token verdict drives the exhausted telemetry (an under-counting estimate would
hide a real overflow; a pessimistic one would report a verified-fitting request exhausted
forever). Endpoints with no `/tokenize` (or a mid-run outage) fall open —
once, stickily, per Loop; a failure under an already-cancelled context is NOT recorded (a
--serve client hang-up says nothing about the endpoint) — to the legacy estimate-driven
oldest-first drop. The downgrade is never silent: `Result.TokenizerPath` reports
`token-exact` vs `legacy (degraded: <why>)` with the per-route failure detail, `agent_run`
returns it as `tokenizer_path`, and the CLI prints a stderr note — the same visibility rule
as the window probe. Both drop paths drop units whole,
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
`render/gpu-lock.mjs::freeLlamaSwap` unloads every GPU-resident model before a render. Arms are compared only
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
real replay corpora are machine-local and never committed. `compaction-eval harvest --traces DIR --out corpus.jsonl`
builds a real corpus from the standalone agent's trace files with REDACTION-AT-HARVEST:
deterministic placeholder substitution over the exact vet refusal classes (git output alone
carries author emails; the private-key class redacts the whole block, not just the header),
kind classification by byte-weighted majority of the TOOL payloads, and production replay
pressure mirrored exactly — `protected_prefix` = the real preamble (turns before the first
assistant turn), `keep_recent` = the live loop's `agent.DefaultKeepRecent`. Transcripts the
ladder already compacted mid-run are refused (`agent.IsCompactionArtifact`): their raw content
is unrecoverable and replaying them would measure the ladder against its own output. The
redaction table derives from the vet's class table (parity by construction), the VetPII gate
re-runs on the result — residual PII refuses the harvest — and the corpus is written atomically
(temp + rename), round-trip-proven through the strict loader before it exists at its
destination. Methodology harvested from the OmniRoute compression service's
eval approach (MIT); metrics and signals are this harness's own.

## Data and state

- **Audit trail** — append-only JSONL, mode `0600`, at `~/.local-offload/agent-audit.jsonl` by
  default. Resolved only when a mutating capability is enabled.
- **Ask queue** — sibling file for deferred approvals and parked high-risk calls.
- **Worktree memory** — an `AGENT.md` loaded into context on a re-injection cadence.
- **Traces** — optional per-run transcripts.

Both the audit trail and the ask queue **must live outside the worktree**. This is enforced at
construction: the builder fails outright if either path resolves inside, because the agent's own
write and shell tools could otherwise clobber the record of what it did.

## Interfaces and entry points

- One-shot: `local-agent --root . "task"`.
- Queue mode and `--serve` (OpenAI-compatible endpoint, loopback-only — see
  [ADR 0005](../architecture/decisions/0005-loopback-only-serve.md)).
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

## Error handling

Tool errors become `is_error` results the model can react to; the loop never panics on tool failure.
Throttle refusals are fed back as ordinary tool results with explicit instructions to move on.

## Security and privacy notes

Confinement is **asymmetric by platform, and the weaker side is disclosed**. Linux uses user,
network, and PID namespaces plus seccomp and Landlock, failing closed if the Landlock ABI floor is
not met rather than running uncaged. Native Windows uses a Job Object plus a low-integrity token:
writes outside the worktree are blocked by MIC, but **network egress is not severed and reads outside
the worktree are not blocked**. The source calls this "HONEST RESIDUAL RISK (documented, not hidden)"
and the tool description the model sees says the same.

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
transcript as "NOT executed" messages.

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
- Pointing `--audit` inside the worktree — a startup error, on purpose.

## Source map

- [`internal/agent/loop.go`](../../internal/agent/loop.go) — loop, budget, `dispatchOrThrottle`
- [`internal/agent/policy.go`](../../internal/agent/policy.go) — broker, `.git` denial, audit append
- [`internal/agent/rules.go`](../../internal/agent/rules.go) — the tighten-only risk-rule table and
  the built-in `defaultRules()` secret-material floor
- [`internal/agent/unattendedrules.go`](../../internal/agent/unattendedrules.go) — the embedded
  default unattended table and the `RulesOff` escape hatch
- [`examples/agent-rules.json`](../../examples/agent-rules.json) — the shipped starter table for
  operator-authored rules
- [`internal/agent/effects.go`](../../internal/agent/effects.go) — effect statuses, `NotPerformed`,
  the `security_risk` park logic
- [`internal/agent/batchjudge.go`](../../internal/agent/batchjudge.go) — the advisory end-of-run
  judge
- [`internal/agent/runtool.go`](../../internal/agent/runtool.go) — allowlist, direct exec
- [`internal/agent/writetools.go`](../../internal/agent/writetools.go) — `os.Root` scoping
- [`internal/agent/builder.go`](../../internal/agent/builder.go) — capability grants, audit-path check
- [`internal/agent/twotier.go`](../../internal/agent/twotier.go), [`profiles.go`](../../internal/agent/profiles.go), [`compaction.go`](../../internal/agent/compaction.go), [`skeleton.go`](../../internal/agent/skeleton.go), [`internal/compeval/`](../../internal/compeval/)
- [`internal/sandbox/`](../../internal/sandbox/) — platform cages
- [`cmd/local-agent/`](../../cmd/local-agent/) — CLI and server

## Related docs

- [../architecture/decisions/0003-policy-broker-and-capability-flags-off-by-default.md](../architecture/decisions/0003-policy-broker-and-capability-flags-off-by-default.md)
- [../architecture/decisions/0004-worktree-confinement-audit-outside.md](../architecture/decisions/0004-worktree-confinement-audit-outside.md)
- [../architecture/decisions/0005-loopback-only-serve.md](../architecture/decisions/0005-loopback-only-serve.md)
- [../OPERATOR-GUIDE.md](../OPERATOR-GUIDE.md)
