# Offload pipeline

## Purpose

The Cascade: the code that takes a grunt-work task, walks it up a ladder of local model Tiers, and
either returns validated structured output or a Defer. This is the core of the harness — everything
else is a surface onto it.

## Questions this doc answers

- Which model runs first, and what makes the harness try a bigger one?
- What exactly triggers a Defer, and what does a caller do with it?
- Where do the confidence thresholds come from, and what are the defaults?
- Why does the coding agent get a different path through the same code?
- Where are token savings recorded?

## Scope

Task dispatch, the Tier chain and escalation, output validation, grounding, confidence gating, the
terminal reasoning Tier, defer construction, and ledger accounting. Covers the text tasks
(summarize, classify, extract, triage) and the dispatch layer for vision, OCR, speech, and media
tasks.

## Non-scope

- How models are served, and with which flags → [setup-installer.md](setup-installer.md)
- How media generation actually renders → [media-generation.md](media-generation.md)
- The tool surface callers see → [mcp-server.md](mcp-server.md)
- The agent loop that consumes the recordless path → [coding-agent.md](coding-agent.md)

## Key concepts

**Tier** — one model seat, named by a stable alias. **Cascade** — the ordered set of Tiers a task
walks. **Defer** — a structured "you do it" result. **Escalation** — moving to the next Tier after a
recoverable failure. **Grounding** — checking that output values actually appear in the input.

## How the system works

A request names a task and carries input. Before the chain runs, oversized input meets the context
budget: with `gcf_compact` on (default off), JSON arrays of flat objects inside an over-budget input
are first re-encoded columnar by `internal/gcf` — LOSSLESS, round-trip proven — so content that the
head/tail trim would have cut can instead fit the local window at full fidelity; whatever still
overflows is trimmed as before. An in-budget input is never touched.

That entry packing belongs to the ENTRY tier only (TO-3, 0.58.0). The original source is retained,
and every tier the request CLIMBS to — the escalation slot and the terminal reasoning tier — is
re-packed FROM THE ORIGINAL against its own budget: `n_ctx(callee) − task max_tokens − reserve −
tokenized(scaffold)`, with `n_ctx` probed live from the serving endpoint (`/props`, cached with a
TTL) and every count measured by the callee's OWN served tokenizer (`internal/tokclient`) — never
chars/4. A source that fits the bigger window arrives WHOLE; one that still overflows is cut
token-exact, head+tail on rune-safe piece boundaries. Any probe/tokenize failure falls open to the
entry packing — byte-identical to the pre-TO-3 behavior — and the disposition is recorded on the
ledger row as `tier_pack` ("token-exact (full source)" / "token-exact (cut K/N tokens)" /
"entry-inherited (<why>)"): fail-open, never fail-unobservable. Forwarding the small tier's lossy
cut up the ladder was a correctness bug class — the bigger model was paying its VRAM bill and never
being offered the source it could hold. The cache key is likewise the ORIGINAL input (the logical
request's identity): under-cap inputs key byte-identically to before, and two different oversized
originals that share an entry trim no longer collide.

The pipeline then builds a Tier chain for that task and walks it:

```
chain = [triage_model?] → model → escalation_model
```

The triage Tier is only included for `triage` and `classify` tasks, and can be skipped by the
entry-tier router. Duplicate aliases collapse, Tiers whose circuit breaker is open are skipped, and
if everything is pruned the chain falls back to the workhorse model alone.

What fills those slots — and the separate terminal reasoning tier described below — is a
per-machine choice; the validated recommendation per hardware tier lives in the model matrix
(`setup/SETUP-AGENT.md`, "Text-cascade matrix"). As of 2026-07-21 the ≥16GB recommendation binds
the escalation slot to a 12B-MTP tier and the terminal reasoning tier to the 26B (the chain shape
above is unchanged — three slots plus the terminal tier); 8GB tiers keep the default shape. The
matrix also records the models that must NOT fill cascade slots at all (e.g. `gpt-oss-20b`, whose
harmony output format is incompatible with the GBNF path described below).

For each Tier the pipeline generates under a GBNF grammar, then applies a series of gates. Each gate
answers "is this good enough, and if not, is it worth trying a bigger model?"

- **Schema validation.** Output that does not satisfy the compiled schema is a retry/escalate.
- **Grounding.** Extract output whose values do not appear in the source escalates. Grounding is
  *computed and logged* for other tasks but only *actioned* for extract — summarization paraphrases
  legitimately, so acting on it would be noise.
- **Confidence gate.** For classify, a self-reported confidence below `classify_min_confidence`
  (default **0.88**) escalates. For decision tasks, a logprob decision margin below the task's
  threshold escalates — a learned per-task conformal value when one exists, otherwise
  `confidence_margin_threshold` (default **0.65**). Both defaults were calibrated 2026-08-14
  from the confcal probe's observed distributions; the prior constants (0.45 / 0.35) sat below
  the entire observed support of their signals and had never fired on probe or production
  traffic.
- **Confhead gate.** A learned correctness head below its threshold escalates.

An OK result returns immediately. A recoverable failure at a non-final Tier escalates. Infrastructure
failures — connection refused, timeouts, 5xx — do **not** escalate, because a bigger model on a
broken endpoint fails the same way; they defer with `err_class` set.

When the whole chain has deferred, one last attempt runs on the **terminal reasoning Tier**, for
grammar tasks whose output was not truncated. It gets a thinking span supplied by the grammar
(`WrapThinking`) and an extra token budget, runs once, and is not subject to the confidence gate.
Results from it are marked `Reasoning: true`, which is what distinguishes them from ordinary
escalation results in the ledger. With the shipped default binding both run on the same model
(`gemma4-26b-a4b`); under the ≥16GB matrix recommendation they differ — escalation on the 12B,
the terminal tier on the 26B.

If that also fails, the pipeline returns a Defer and records it.

**Vision-task dispatch.** `vqa` / `assess_image` / `extract_image` ride `vision_model`. The `ocr`
task alone may ride a dedicated `ocr_model` seat when one is bound — purpose-built OCR models beat
a general VLM on dense text but are text-recognition only, which is why it is a separate binding
and never a `vision_model` replacement. It is deliberately unbound by default (no tier seeds it):
empty = OCR rides `vision_model`, byte-identical to before. The alias is resolved once and threaded
through, so the call, the cache key, the circuit breaker, and the ledger all name the model that
actually ran; `offload_status`'s roster reports the effective `ocr` model, falling back to vision.

## Important flows

- [../flows/cascade-escalation-and-defer.md](../flows/cascade-escalation-and-defer.md) — the walk in
  detail.

## Data and state

- **Ledger** — append-only JSONL at the configured `ledger_path`, `fsync`ed per entry so a crash
  cannot lose recorded savings. Carries `tokens_saved` (input tokens kept out of the calling model)
  and per-call metadata.
- **Cache** — keyed result reuse. Bypassed on the *recordless* path (`NewRecordlessPipeline`);
  **shared** on the *in-loop* path (`NewInLoopPipeline`) — see Interfaces below for why those are two
  different things.
- **Media artifact addressing** (`internal/mediahash`) — audio and video cache keys identify the
  source file by `sha256` of its **bytes**, matching what the image path has always done
  (`"img:"+sha256hex(loaded bytes)`).
  - Audio previously keyed on (path, size, mtime) and video on the **path string**, unhashed. Both
    failed in two directions: a file replaced at the same path could produce a **false hit** —
    serving the old file's transcript or description — and an identical file at a second path always
    missed, which is the reuse an artifact cache exists to capture.
  - **A TOCTOU window remains, and is DETECTED rather than prevented.** The digest and ffmpeg are
    two independent opens of a path, so ordering alone cannot close the gap — hashing first merely
    transposes which side is misattributed (hash-then-read stores the *new* bytes' transcript under
    the *old* digest; read-then-hash does the reverse). Both are false hits reachable from any path
    holding the misattributed bytes. Closing it outright would mean sharing one descriptor with
    ffmpeg. Instead the file is re-`stat`ed **after** the consuming read and compared against what
    the digest saw; on a detected difference the call is treated as unidentifiable and **nothing is
    stored**. One stat, and it covers audio, video, and the case no re-ordering can touch at all: a
    file still being *appended to*, where the digest covers a prefix and ffmpeg reads more.
    `mediahash` also verifies it hashed exactly the number of bytes `stat` reported, so a growing
    file yields an error rather than a confident prefix-identity.
  - **The detector is (size, mtime), so it narrows the window rather than closing it.** A same-size
    overwrite inside one mtime tick is invisible to it, and 1–2 s mtime granularity is common on
    FAT/SMB/FUSE and Drive-backed mounts. Stating this because the alternative — implying "any
    difference is detected" — is the same overclaim this section previously made about ordering.
  - **Cost of an unidentifiable input:** it is never cached, so it re-runs the model on every call,
    and each call writes a fresh nonce-salted `.srt`/`.txt`/`.segments.json` triple. Nothing reaps
    `media_dir`, so a file on a persistently flaky mount accumulates three files per invocation.
    That is a deliberate trade — a wrong cached transcript is worse than a repeated one — but it is
    a real cost and an operator may need to reclaim the directory.
  - The video digest is hoisted above the width-halving retry loop (it is loop-invariant, and
    re-reading a multi-GB clip per retry is pure waste), but the verification runs **per iteration** —
    hoisting alone widened the window to *digest-at-t₀ versus the final successful sampling*, which
    on a 4K vertical reel is the 4th attempt, minutes later.
  - **A bypassed cache is observable.** `cache_bypass` on the ledger row names why, because a
    permanently unidentifiable input is otherwise byte-identical in telemetry to an ordinary cold
    miss — it would re-run the model at full cost forever while the ledger looked healthy and the
    hit-rate dashboard invited the wrong diagnosis.
  - **No identity, no cache.** When the digest fails, the work is computed and returned but nothing
    is looked up or stored, and the on-disk media stem is salted so two failures at one path cannot
    overwrite each other's `.srt`/`.txt`. `mediahash.Digest` returns an **error** rather than a
    synthetic key: an earlier design returned `media:staterr:<hash(path+error)>`, which is a *path*
    key — so a transient read failure wrote a durable entry that a different file at that path later
    hit, reintroducing the exact false hit this change removes.
  - `media_hash_max_full_bytes` defaults to **0 = always hash the whole file**. The cost is a cold
    file read, so it is **I/O-bound, not SHA-bound** — on `V:` or a `G:\My Drive` mount a large clip
    is nowhere near memory-speed. It is still cheap *relative to the work it guards*, because both
    call sites already read the same file through ffmpeg before hashing it. A positive value
    switches larger files to a **sampled** digest (size + up to three 8 MiB windows, de-duplicated);
    that is opt-in because its failure mode is a false hit between two same-size files agreeing on
    those windows. The mode is encoded in the digest, so sampled and full can never be confused.
  - **Migration:** this changes every existing audio and video key once. Intended — those entries
    were keyed on an identity that could be wrong.
- **Embed memo** — `internal/embedmemo`, a bbolt store at `embed_memo_path` keyed on
  `sha256(embedder_id, epoch, exact input bytes)`. Embedding is a pure function of (model, text) and
  the harness re-embeds the same strings by construction, so a hit skips the call — and, because the
  embedder carries `ttl=300` like every other seat, it also skips the ~1–2 s cold load the first
  embed after an idle gap would pay.
  - **Wired consumers:** the kNN pre-filter on the request path, and the shadow-label drain
    (`shadow-label`). Exemplar selection is **not** a consumer — `internal/exemplars` retrieves
    lexically and contains no embedder.
  - **Where the repeats actually come from — the original justification was wrong, and this states
    the corrected one.** The memo was introduced on the claim that the drain "re-embeds the same
    stored inputs and re-scores the same reference summaries on every run". Reading the code
    refutes all three parts: `shadow.Drain` is **destructive** (it renames the queue to
    `.draining`, reads it, and drops the claim), so each run consumes a **fresh** item set and only
    a crash-recovered claim ever replays; there is **no shared reference set**, because `label.go`
    calls `Similar(entrySummary, escSummary)` on summaries derived from *that item's own* output;
    and the drain's `Embed` path is itself gated on `knn_prefilter_enabled`. The genuine repeat
    sources are the **request-path pre-filter** (repeat inputs are real and measurable — that is
    what the Phase 0.1 identity fields count) and **within-run** repeats inside one drain.
  - **Consequence: with `knn_prefilter_enabled` at its default of `false`, this feature is close to
    inert.** It is correct, cheap and hardened, and it costs nothing when idle — but enabling the
    pre-filter is a separate decision with its own quality implications, and the memo's value is
    gated on it. Do not read a zero hit count on a stock config as a fault.
  - Keys are **never normalized**: casefolding or whitespace collapsing would let two different texts
    share a key and return a vector computed for the other one, which is a silent correctness bug in
    a semantic quantity rather than a cache miss. Vectors are stored as verbatim `float64`, so a hit
    is bit-identical to what the embedder returned.
  - Every key, counter and prune-order entry is scoped to a **namespace** derived from
    (embedder id, epoch), so switching embedders cannot serve the previous model's vectors — the two
    address disjoint keyspaces rather than relying on a check. A namespace records its vector
    dimension on first store; a later disagreement proves the model changed behind a stable id and is
    reported rather than mixed into a cosine routine.
  - Every failure path (disabled, file held by another process, malformed record, embedder error)
    degrades to a plain live call, and each is **counted and published** — `offload_status` and
    `loupe` both report decode/read/write faults, because an unpublished fault counter cannot
    distinguish a working memo from one whose store fails every write.
  - **Persisting the counters requires `embedmemo.CloseShared()` on the owning binary's shutdown
    path.** It is the only writer of the lifetime hit/miss totals; a process that exits without it
    leaves them at zero, and the reports then state that a memo serving thousands of hits was "never
    consulted".
- **Learned thresholds** — per-task conformal values loaded from `thresholds.json` when present,
  falling back to config defaults.
- **Circuit breakers** — per-Tier, consulted during chain construction.

## Interfaces and entry points

- `Run` — the full cascade with recording.
- `RunTier` — one specific Tier, no escalation, **no ledger/shadow/exemplar recording**. It reads and
  writes the result cache only when the pipeline it belongs to opted in (below).
- `NewRecordlessPipeline` / `NewRecordlessOffload` — nil cache **and** nil ledger. For callers that
  must share no state at all: the shadow-labelling flywheel and prompt A/B arms.
- `NewInLoopPipeline` / `NewInLoopOffload` — nil ledger, **shared result cache**. This is what the
  ordinary drive modes (MCP front door, `local-agent` CLI) use.

  A defer on either path is returned as a *successful tool result*
  (`{"deferred": true, "reason": ...}`) rather than an error, because the agent loop should read it
  and move on.

### Why "recordless" split into two constructors

The original single invariant bundled two unrelated guarantees:

1. **the agent's internal offload calls must not pollute the savings ledger** — those are the harness
   talking to itself, not work a caller delegated, so counting them inflates every savings number; and
2. **those calls must not read or write the result cache.**

(1) is a real accounting invariant and is kept exactly. (2) was collateral damage: it made the loop
re-run the model on byte-identical input, so an agent that summarized the same file twice in one run
paid twice. Nothing about ledger hygiene requires that, and the cache key binds the prompt template
and exemplar set, so an entry written by one caller is valid for any other.

**Cache participation is a property of the pipeline, never of `RunTier`.** That distinction is
load-bearing: the shadow-labelling flywheel drives `RunTier` on the *main* pipeline — the one with an
open cache — to evaluate what a counterfactual tier *would* have answered. A cache hit there would
grade a stored answer instead of the tier, and a cache write would fill the store with counterfactual
results. Only `NewInLoopPipeline` opts in.

`RunTier` has its **own keyspace** (`cacheKeyForTier`), disjoint from `Run`'s, keyed on the **actual
tier** and carrying the template tag. Sharing `Run`'s constructor was not enough: with
`exemplar_shots` at its default of 0 every other ingredient coincided, so both paths computed the
same key whenever the pinned tier was the primary model, and they overwrote each other's entries
repeatedly. The consequence of the split is that an in-loop offload does not reuse a cascade answer
— which was never sound anyway, since a pinned tier must get *that tier's* output.

## Dependencies

`internal/llamaclient` (completion client — including the per-model `seat_endpoints` static
pins and the busy-aware `cascade_remote_lanes` failover), `internal/gbnf` (schema→grammar),
`internal/grounding`, `internal/confidence`, `internal/ledger`, `internal/config`. Media, vision, and
speech tasks dispatch out to their own backends.

## Downstream effects

Every caller — CLI, MCP tools, the coding agent, fleet jobs — goes through here. Changing gate
behavior changes the defer rate everywhere at once, which is why thresholds are config-driven rather
than compiled in.

## Invariants and assumptions

1. **A Defer is a success signal.** Never convert one into an error, and never add a cloud fallback
   to avoid one — see
   [ADR 0001](../architecture/decisions/0001-defer-never-cloud-fallback.md).
2. **Structured output comes from a raw GBNF grammar field**, never `--json-schema` or
   `response_format` — see
   [ADR 0002](../architecture/decisions/0002-grammar-reliable-serving-flags.md).
3. The recordless path writes nothing — no ledger, no cache, no shadow capture.
4. Infrastructure failures do not escalate.
5. The reasoning Tier never fabricates a pass: garbage from it still defers.
6. **One llama-swap serving slot holds one model at a time.** Every generation request passes
   through the Model Affinity Gate (`internal/modelaffinity`) before it is sent. Same model on the
   same base is concurrent; a different model parks until the in-flight batch drains. See
   [ADR 0025](../architecture/decisions/0025-model-residency-is-arbitrated-in-process-by-base.md).

## Error handling

Recoverable model-quality failures escalate. Infrastructure failures defer with `err_class`
(`oom`, `timeout`, `http_5xx`, `conn_refused`, `gpu_busy`). Exhausting the chain defers with the last
reason and any partial output preserved in `Partial`.

A request can also fail without reaching llama-swap at all: if the Model Affinity Gate's bound
expires while the request is parked behind another model, it returns a `*modelaffinity.WaitError`
naming the base, the model wanted, the model that held the slot, and how many switches were queued
ahead. Its wording carries the substring `classifyErr` buckets congestion by, so it files as
`timeout` rather than `other`.

## Security and privacy notes

The cascade holds no credentials. By default it reaches no network beyond the configured local
endpoint; since 0.65.0 two opt-in config keys can route a seat's completions to a base on the
operator's OWN tailnet, never cloud (ADR 0023): `seat_endpoints` (a static per-model pin — that
seat is always remote) and `cascade_remote_lanes` (busy-aware failover — the lane is taken per
call, only while the local machine-wide GPU lease is held and a cached alias-aware roster probe
confirms the lane serves the SAME model, fail-closed to local, one serve-log line per reroute).
Routing never changes WHICH model answers, only WHERE. Both keys are vetted by the tailnet guard
at config load (naming the offending key) and by the resolve-and-pin `SafeTransport` dial gate on
every request. Task input passes through the ledger only as metadata and token counts, not as
content.

## Prefix reuse — why the prompt shape is load-bearing

The serving stack reuses a KV prefix, so the cost of a call is dominated by how much of
its prompt the server has already seen. **Measured on <node-b>, 2026-07-27, `gemma-4-e4b`:**

| call | `prompt_n` | `cache_n` | prefill | wall |
|---|---:|---:|---:|---:|
| cold — instructions + payload A | 2037 | 0 | 514.7 ms | 7.1 s |
| identical repeat | 1 | 2036 | 30.5 ms | 0.22 s |
| **same instructions, NEW payload** | **41** | 1996 | 85.8 ms | **0.30 s** |

The third row is the one that matters: a harness payload always differs, and only the 41
changed tokens were re-prefilled. That saving exists **only while the stable text comes
first and the variable document last**, which is how every builder in `internal/tasks`
is written — a constant `System` per task, then instructions/labels/schema/question,
then `TEXT:\n<input>` at the end.

It is one `fmt.Sprintf` argument order away from being lost **silently**: nothing would
fail, every call would just quietly pay full prefill again. `internal/tasks/prefixorder_test.go`
is the guard — the input must be the suffix of the user message, appear exactly once, and
the system prompt must be byte-identical across two different inputs.

Two things this does NOT mean:

- **It is not a reason to add `--swa-full` to the small tiers.** That flag was measured as
  load-bearing for the large iSWA models (Laguna, V4-Flash) and is set for them. The
  harness's own tiers reuse prefixes on this box *without* it — the table above was
  measured against a `gemma-4-e4b` entry that does not carry the flag — and forcing a full
  window would cost KV memory on exactly the tiers that have least to spare. Verify per
  model before copying serving flags between them.
- **It changes no defer or escalation decision.** Those gates are confidence- and
  validation-driven, never latency-driven, so cheaper repeat calls do not re-tune anything.

If you benchmark this yourself, use the server's own `timings.prompt_n` as ground truth —
did it re-prefill? — never process memory. An mmap'd model's weights live in the page
cache, not the process working set, so a "did it restart?" check based on RSS reports
phantom restarts and has already produced one retracted root cause.

## Observability and debugging

- `local-offload doctor` — endpoint health, per-alias reachability, and the derived media routes
  (a media binding whose file is absent exits non-zero — see
  [media-generation.md](media-generation.md#capability-is-derived-never-declared)).
- `local-offload models` — the resolved Tier routing table.
- `local-offload ledger --since N` — savings accounting.
- The ledger's per-entry metadata (`escalations`, `margin`, `grounded`, `err_class`, `reasoning`) is
  the primary debugging signal for "why did this defer?"

## Testing notes

`internal/pipeline/` carries focused suites per concern: `runtier_test.go` (the no-side-effect
invariant), `pipeline_reasoning_test.go`, `pipeline_confhead_test.go`, `knn_prefilter_test.go`
(entry-tier selection), plus per-task defer tests. `internal/grounding/` and `internal/ledger/` have
their own unit tests.

## Common pitfalls

- **Treating a defer as a bug.** It is the designed outcome when confidence is low.
- **Expecting grounding to gate summaries.** It is logged for summaries, actioned only for extract.
- Assuming escalation happens on any failure — infrastructure failures deliberately do not escalate.
- Assuming `Reasoning` implies a different model. Under the shipped default it is the same model
  as the escalation Tier (they differ only if the config binds them apart, as the ≥16GB matrix
  recommendation does); the flag is what tells them apart.
- Reading logprobs under an active grammar as if they were unconstrained. They are pre-mask.

## Source map

- [`internal/pipeline/pipeline.go`](../../internal/pipeline/pipeline.go) — chain construction, the
  walk, gates, reasoning tier
- [`internal/pipeline/recordless.go`](../../internal/pipeline/recordless.go) — the nil-store
  construction
- [`internal/core/types.go`](../../internal/core/types.go) — `Result`, `Meta`, `Deferf`
- [`internal/grounding/grounding.go`](../../internal/grounding/grounding.go)
- [`internal/ledger/ledger.go`](../../internal/ledger/ledger.go)
- [`internal/config/config.go`](../../internal/config/config.go) — tier aliases and threshold defaults
- [`internal/modelaffinity/affinity.go`](../../internal/modelaffinity/affinity.go) — the Model
  Affinity Gate: admission keyed on the resolved base, batching, the bound
- [`internal/llamaclient/client.go`](../../internal/llamaclient/client.go) — the three generation
  methods and where the gate is taken
- [`internal/llamaclient/lanes.go`](../../internal/llamaclient/lanes.go) — `resolveEndpoint`, whose
  base decision the gate consumes and never re-decides

## Related docs

- [../architecture/decisions/0001-defer-never-cloud-fallback.md](../architecture/decisions/0001-defer-never-cloud-fallback.md)
- [../architecture/decisions/0002-grammar-reliable-serving-flags.md](../architecture/decisions/0002-grammar-reliable-serving-flags.md)
- [../architecture/decisions/0025-model-residency-is-arbitrated-in-process-by-base.md](../architecture/decisions/0025-model-residency-is-arbitrated-in-process-by-base.md)
- [../architecture/decisions/0010-tier-optimization-before-latency-defer.md](../architecture/decisions/0010-tier-optimization-before-latency-defer.md)
- [../glossary.md](../glossary.md)
