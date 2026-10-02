---
status: Accepted
date: "2026-09-20"
---

# Walls are ceilings; liveness is progress

## Context

Until 0.130.x a delegation contract ran under ONE number sized before the run — D-03's auto wall:
cold load + think block + steps × (tokens ÷ rate + overhead) + final + re-pack, clamped to
`[300, 900]` s — enforced as a `context.WithTimeout` over the whole node-side run. The delegator
polled until `timeout_sec + 60 s` (re-anchored on the node's published `wall_sec`, D-116).

The ledger on 2026-09-20 showed what that does to a slow but healthy seat. Every wall timeout
that day was a **producing** job:

| time | seat | steps | tokens out | seat tokens in | latency | reason |
|---|---|---|---|---|---|---|
| 11:48 | `<node-c>` `qwen38-27b-gsq-vllm` | 11 | 949 | 213,442 | 984 s | wall timeout after 900s |
| 12:07 | `<node-c>` `qwen38-27b-gsq-vllm` | 11 | 544 | 169,515 | 900 s | wall timeout after 900s |
| 16:09 | `<node-b>` `agent-pool` | 11 | 6,236 | 549,594 | 769 s | wall timeout after 600s |

The 16:09 row's `tok_per_s` read **0** on the `agent_delegate` row — the one field that should
have proved the job alive was unfilled on the row the operator reads.

The code could not tell a live job from a hung one because it never looked: the loop's seat call
was **non-streaming** (`Stream: false`), so inside a step the node had no telemetry at all — a
550 k-token prefill and a dead engine were indistinguishable until the clock ran out. The run
registry's heartbeat (`HeartbeatMs`) was the holder *process* ticking, not the job (register
C-32). The operator's standing order — "smart, dynamic, and with the correct telemetry to
determine if a job is truly still going" — was not met by any of the three surfaces.

## Decision

1. **Every seat completion streams** (`stream: true`, `stream_options.include_usage: true`).
   The client decodes the SSE stream into the same `wireResp` a JSON answer produces, so
   nothing after the decode changed; each delta (content, reasoning or tool-call arguments) is a
   progress event delivered through a `ProgressFunc` carried in the call's context. A seat that
   answers JSON anyway (a proxy that ignores `stream`) is decoded as before. The structured
   re-pack is one of these completions too (item 8).

2. **A run is ended by a STALL or by a CEILING — never by its expectation expiring.** The
   contract's wall (`timeout_sec`, or the node's auto wall) is now the **expectation**: still
   reported as `wall_sec`, still what the delegator sizes and anchors from, no longer a kill.
   `agent.Monitor` owns the run's liveness:
   - **Stall** = no progress event inside the phase's allowance. The allowance is dynamic:
     admission → the admission budget; **prefill** → `pending prompt tokens ÷ the seat's
     measured prefill rate × 1.5 + 30 s` (100 tok/s assumed until measured — 400 was the first guess and filed a false stall on the `<node-c>` GSQ seat, 0.131.1); **decoding** →
     `20 deltas ÷ decode rate`; **tool** → the tool's own cap + 30 s (an uncapped tool: 1 h);
     **re-pack** → `max(120 s, 1.5 × expected answer tokens ÷ decode rate + 30 s)` (item 8); all
     floored at 60 s. A stall is filed
     `stalled: no progress for Xs in <phase> (allowed Ys: <arithmetic>)` as
     **`DeferClassInfrastructure`** — the seat's health, never the budget signal.
   - **Ceiling** = `max(3 × estimate, 2 × wall, 1800 s)`, capped at 4 h
     (`AgentCeilingSecCap`). A run still producing when it passes is filed
     `ceiling Ns reached while producing (T tok at R tok/s)` as **`DeferClassBudget`** —
     the sizing signal. The context's `Deadline()` reports the ceiling so every existing
     reader (the re-pack floor, the seat-wait budget) keeps working; the cause is typed
     (`context.Cause`), so no arm guesses from the clock.
   - `wall timeout after Ns` is retired from the node's own vocabulary; it survives only as the
     text for a **caller's** deadline expiring, and says so.

3. **Telemetry on every surface the operator reads.** The run registry carries job liveness
   *beside* the process heartbeat: `last_progress_ms`, `tok_s`, `live_phase`, `allowance_ms`,
   rendered as one shared line ("producing 3.4 tok/s, last token 2s ago (allowed 60s in
   decoding)" / "silent 187s of 214s allowed in prefill") in `gpu status` and
   `offload_status.gpu_lease.activity.runs[]`. The fleet node publishes the same as `progress`
   on `/fleet/jobs/{id}` with `stall_allowance_sec` and `ceiling_sec` twinned at the top level.
   The wire result carries `ceiling_sec`, `stall_allowance_sec`, `last_progress_ms`. The
   ledger's `agent_delegate` row now fills `tok_per_s`.

4. **The delegator polls while the node reports progress.** Its deadline holds while the node's
   `last_progress_ms + allowance + grace` is in the future, bounded by the node's ceiling. A node
   that reports no progress is polled exactly as before; a node whose report goes stale is not
   extended, and the deadline reason says when it last moved and what it was allowed.

5. The seat-rates store records the seat's **prefill rate** (`prefill_tok_s`) from the loop's
   prefill accounting, so the prefill allowance is measured after the first run, not assumed.

6. **A seat that is loading is not stalled (0.140.0).** The prefill allowance is sized from the
   prefill rate, so a request that waits for its seat to LOAD was filed as a prefill stall. The
   admission warm-up covers a seat that is absent when the run starts, but not one evicted mid-run
   (another model's swap between two steps, or the idle unload during a long tool call). On
   2026-09-23 the 3-card seat's ~180 s reload deferred an `offload_review_diff` twice at 60 s. While a
   request may be waiting for its first byte (**admission**, **prefill** or **re-pack**), the monitor
   reads llama-swap's `/running` through a seat probe every 5 s, and once more before filing a stall.
   The run then enters a **`cold-load`** phase with two parts:
   - **Loading.** This part is entered only on positive evidence that a load is in progress: the
     seat's row is `starting` or `stopping`, or the seat is absent while another row is `starting` or
     `stopping` (a swap). Absence alone keeps the normal stall clock: a removed seat or a restarted
     llama-swap must fail at the floor, not hold. This part is bounded by the **cold-load ceiling**
     `max(600 s, 2 × measured cold_load_sec)`, counted from the silence and clamped to the run's own
     ceiling. 600 s is llama-swap's `healthCheckTimeout` on the reference boxes.
   - **Post-ready.** This part starts when the load the probe saw is over, or when the admission
     warm-up just loaded the seat (`MarkSeatLoaded`). The first completion gets a short bound,
     `max(120 s, 2 × the waiting phase's allowance)`, so a seat that wedges right after loading is
     visible within minutes. It is deliberately not sized to cover the separate warm-seat silence
     under investigation.
   - **The hold ends on the seat's first byte.**

   Defers read `stalled: seat still loading after Xs in cold-load (…)` or `stalled: no byte for Xs
   after the seat read ready, in cold-load (…)`, filed as infrastructure. The `stalled: ` prefix is
   kept on purpose, so every reader keyed on it classifies them unchanged. An unreadable `/running` is
   named as such in the reason, never reported as a ready seat. With no load seen it counts as
   "cannot tell", and the prefill clock runs as before. The wait is never open-ended.

7. **On vLLM, a progress event is a generated token, not a visible delta (0.140.1).** Item 1
   counted content, reasoning and tool-call-argument deltas. vLLM's tool parser holds some of those
   back: a trailing non-string argument (an object, an array, a number) until it closes, and a call
   to a name the request did not offer for its whole length. `serving.py` sends no frame while it
   holds. Measured on the 3-card seat on 2026-09-23: one `offload_extract` call with a 40-property
   `schema` object streamed nothing for 62.8 s while the engine generated 1,460 tokens, past the
   60 s floor. So on a seat the box declares as vLLM (`vllm_seats`, alias resolved through the
   roster) the loop asks for `return_token_ids`, and vLLM then sends one frame per engine step
   carrying the generated ids whether or not the parser emits a delta (the same call: 1,462 frames,
   largest gap under 0.05 s). The decoder counts ids as progress. A tool-call frame carrying only
   the tool name counts as well. Every other seat keeps the request it always sent. Sizing the
   allowance from an expected tool-call length was rejected: the engine has a real signal, and an
   estimate would either cut long calls or blind the watch to a hung engine for minutes.

8. **The structured re-pack follows the same rules (amendment 2026-09-30, register C-66, RC-6).**
   Items 1 and 2 said every completion streams and the allowance follows the seat, but the re-pack was
   the one call that did neither. It went out non-streamed under a flat 120 s allowance, so the monitor
   heard nothing between the phase start and the reply and the allowance fired at exactly the phase
   start plus 120 s, whatever the answer size or the seat's rate. Each attempt also carried a
   transport bound of its own, and once the monitor cancelled, attempts 2 and 3 still started on the
   dead context and were counted. From 2026-09-20 to 2026-09-29, 119 re-packs were killed that way,
   every one carrying a finished answer (about 15 job-hours discarded); a seat producing at 3 to 8
   tok/s was the day's worst case. Now:
   - **It streams.** `stream: true` with `stream_options.include_usage`, decoded into the same result a
     JSON answer gives (`llamaclient.WithProgress`). A server that answers JSON anyway is decoded as
     before; a stream that dies mid-body is a transport failure (`*BodyError`); a server that refuses the
     stream with a 400 or 422 is asked again once as one JSON answer, and a retry that answers is
     remembered for that seat for 30 minutes, and the wire says so: `repack_note` reads `streaming refused
     by this seat` (on a success too), because on such a seat the re-pack is one silent request under its
     allowance again and nothing else would show that the streaming fix did not apply. vLLM's own
     structured-outputs example runs with `--stream`,
     so a stream together with `structured_outputs` is documented; it was not verified live on the seat
     builds in use, which is why the JSON answer is the fallback when a server refuses.
   - **Every delta is progress**, for the monitor and for the job record (phase `repack`, tokens, last
     progress), so a delegator polling the node sees the re-pack work. The allowance is published when the
     phase starts: a remote delegator stops polling one allowance plus grace after the node's last report.
   - **The allowance follows the answer and the seat.** `max(120 s, 1.5 × expected answer tokens ÷ decode
     rate + 30 s)`, where expected is the answer's size (about a token per three characters plus the
     object's overhead), never the completion cap: a wedged engine's flat bound stretches to the phase's
     allowance (ADR 0061), and one sized to the cap would hold a dead seat for it. A seat with no
     measured rate keeps the flat 120 s. A stalled re-pack's reason names the arithmetic, or the flat bound
     and why it is the flat bound.
   - **The context owns the deadline.** Under the monitor the re-pack's requests carry no transport
     bound (the client's `Timeout` covers the whole body read), so the busy hold governs a request whose
     seat is working for others, and once the monitor has cancelled no further attempt starts:
     `repack_attempts` counts the requests actually sent. A caller with no monitor keeps the per-attempt
     bound.
   - **A finished answer is not lost with its structuring.** The node flags the defer `schema_miss` when
     its loop finished and only the re-pack failed. The delegator re-packs the answer itself before
     acceptance: the lossless reading first (the answer may already be the object), then one completion on
     its own agent seat. It is one request, not a run, so it takes no slot of the run cap and queues
     through the client's admission like any other. A seat that is not resident is warmed first, on the
     box's admission budget and outside the re-pack's allowance, as a run's own admission does: the
     delegator's seat is idle-unloaded after five minutes, so the rescue routinely finds it cold, and a
     cold load is longer than the whole allowance. The rescue may therefore wait up to
     `agent_admission_wait_sec` (300 s by default) behind a GPU-lease fence or another model's swap
     before its own allowance starts; the delegation context has no deadline of its own until the
     whole-call deadline (PR-6), so those two bound it. The object is validated against the contract's schema
     with every field the acceptance reads required, acceptance then decides, and a rescue that cannot
     produce a validated object leaves the node's defer exactly as sent, still counted as lost work. A node
     that predates the flag is recognized from `stop_reason: done` and the reason prefix.
   - **A warm-up refused with a server error is a seat that did not start, only when nothing says it is
     busy.** Admission proceeded into the wall on any non-200 warm-up answer and reported it as a load.
     A non-200 that loaded nothing is no longer counted as a load, and a 5xx now defers as infrastructure
     before any wall exists when there is positive evidence the seat's process died: llama-swap's answer
     is not one of its busy shapes, `/running` lists no row for the seat and nothing else is mid-swap, and
     `/running` was readable. A busy card is a place in line, so a 503 `process is not ready`, a 500 of
     llama-swap's own, a health-check timeout, an empty 502, a seat that reads starting and another
     model's swap all proceed, as do a 404 and a timeout. The rule ships in audit mode
     (`agent_warm_failure_defer`, off by default: the run proceeds and `admission_note` says it would have
     deferred); enforced, the delegator gives the defer one retry on another node, like the coherence defer.

9. **The re-pack is bounded by the wall, and a cut request is not resent blindly (amendment
   2026-10-01, register C-80).** Item 2 made the wall an expectation and the ceiling the only
   deadline, and item 8 left the re-pack under the ceiling alone: nothing compared what an attempt
   asked for with the time left. A 9B agent seat at about 5.6 tok/s ended its loop 219 s into a 600 s
   wall and then spent 1,670 s re-packing a 2,782-byte answer: the grammar request, sized at 1,439
   tokens, ended `length`; the same request, escalated unconditionally to the 8,192-token cap, ended
   `length` again; and the chat lane answered JSON numbers where the schema wanted strings. Now:
   - **The wall bounds each attempt, by arithmetic.** Before every attempt the node takes the time
     left before `min(the run's ceiling, the wall's end + the liveness slack)` and the seat's decode
     rate (the seat-rates store, else this run's observed rate, else the rate this run's own
     completions measured, else `agent_seat_tok_s`). The slack is the 30 s that the re-pack's own
     stall allowance already adds to its generation estimate, so the bound and the allowance share
     one unit. An attempt whose `max_tokens` fits is sent as sized; one that does not is sent with
     the tokens the time buys, down to the answer's own size (`len/3 + 64` tokens); under that it is
     skipped, and so is every later attempt, because time only runs out. With no known rate there
     is no arithmetic, and a failed re-pack then says so in `repack_note` (`re-pack time bound
     off`); the log says it once per seat. A skip with no attempt before it is a `budget` defer
     carrying the finished answer, flagged `schema_miss`, which the delegator re-packs (item 8) on
     the doors that wire the rescue; after a failed attempt it is a note on that attempt's own
     verdict. It is a token budget and a deadline check, not a transport timeout: a request in
     flight is not cut, and ADR 0061's busy hold and its "no transport bound" stand. The
     delegator-side rescue passes no wall and keeps its own context deadline.

     The consequences are deliberate. A loop that ends after the wall plus its grace never gets a
     node-side re-pack, whatever the seat's speed or the answer's size (a 2 KB answer on a 30 tok/s
     seat is a re-pack of about 25 s): the time left buys no tokens, so the re-pack is skipped, and the
     structured result depends on the delegator's rescue (item 8), which makes one grammar
     completion on the delegator's own agent seat. That rescue is wired on `agent_delegate`,
     `offload_research` and the `delegate` and `research` verbs and, as
     [coding-agent.md](../../systems/coding-agent.md) records, deliberately not on `agent_run` (no
     schema to structure), on the review lane's fenced fallthrough, or on `offload_ask`: a deferred
     `offload_ask` carries the finished answer as `output`, flagged `schema_miss`, and a review
     defer is bare because its filters never saw the raw prose. The rate is tokens per second of
     call wall, prefill included, measured over earlier completions and not re-measured from the
     attempt just run, so an optimistic rate can let one request overrun the wall and its grace:
     the bound holds at request boundaries. Both floors, the skip's and the resend's below, take
     the answer's own size as the object's size, which a condensing schema (a digest) undershoots
     and a restating one overshoots; both fail soft through the rescue. An attempt the time left
     narrowed and the seat cut at that narrowed budget, with a tail that is no runaway, is the
     clock's verdict and is filed the same way: a `budget` defer, never an abstention, which the
     delegator retries on another node.
   - **A cut grammar request is resent at the cap only when the budget was the problem and the time
     left buys it.** The retry used to go on every truncation, and a greedy seat answers the same
     request byte for byte. It now goes only when the budget is under the cap, the tail of what the
     seat wrote is not degenerate (only whitespace, or a block of up to 64 bytes repeated back from
     the end of the output across at least half of its last 256 bytes, or 32 bytes for whitespace
     alone, or a block of lines repeated: a markdown rule, base64 padding or a short list of
     identical items is content), the answer needs more tokens than the budget held at the bytes
     per token the seat actually wrote, the wall did not set the budget, and the time left buys
     what the answer needs: a resend that would carry fewer tokens than the request just judged too
     small cannot finish and spends the time the chat lane needed. The size test measures that
     density because the code's own estimate (a token per three bytes plus 64) is under
     `repackBudget` (the same estimate plus 512, between 1,024 and 8,192) for every answer length
     below the cap, and at the cap an escalation is a request identical to the first. Otherwise
     the loop goes to the chat lane, which is a different request, and the note says what was seen.

     > **Unverified:** the gate was not replayed against the incident's request. It is a
     > heuristic: a legitimate re-pack whose JSON is longer than the answer's own size at the
     > measured density (a restating schema) reads as "ran on past the budget" and gets no cap
     > retry, and a runaway of dense, non-repeating text passes the tail test and is stopped only
     > by the size test and the time fit. The per-attempt `head` and `tail` below show which
     > happens in production.
   - **The prompts and the grammar stop permitting the suspected runaway.** Both prompts give each
     field's type and a list's item type (the chat prompt used to end "numbers unquoted", which a
     seat read together with a field called `numbers`). The grammar lane names the types its
     grammar enforces, not the schema's own: gbnf compiles every array to a list of strings and an
     object to a string, so a prompt that said "array of numbers" beside a grammar that forces a
     quote after the `[` was a contradiction; the chat lane and a vLLM seat's schema lane name the
     schema's types. The grammar's whitespace rule is llama.cpp's own bounded one instead of an
     unbounded repetition, and the scalar coercion turns a bare JSON number into the string a
     string field or list item asks for, keeping its text, and reads array items both ways, saying
     which fields it re-typed. The string and array rules of the grammar are still unbounded, and
     the whitespace bound reaches llama.cpp grammar lanes only: a vLLM seat's `structured_outputs`
     carries no whitespace control, so there the fix rests on the prompts, the gate and the time
     bound.

     > **Unverified:** that whitespace was what the incident's seat wrote was not confirmed (the
     > request was not replayed), and the bounded `{0,20}` repetition was not compiled against a
     > live seat when this was written (it is the repetition llama.cpp's own `json.gbnf` uses for
     > the same rule). The per-attempt `head` and `tail` confirm or refute the first in
     > production; the first grammar request on a live seat confirms the second, and a rejected
     > grammar would surface as an engine error naming the rule.
   - **Every attempt is on the wire.** `repack_attempts_detail` carries one record per attempt
     (lane, `max_tokens`, tokens generated, finish reason, the first and last 80 bytes of what it
     wrote, why it failed or was skipped, and `coerced to the schema's types: ...` when the node
     re-typed scalars), and `tokens_out` counts the tokens of failed attempts too. An attempt that
     died mid-stream (a stall, the ceiling, a dropped connection) keeps the deltas it had streamed
     and what had arrived, a lower bound of its tokens. The row an `agent_delegate` caller reads
     carries the first four records.

## Consequences

- A 27B seat at 1 tok/s finishes its contract. A seat that dies mid-stream (an engine error
  frame, a socket that stops) is a stall within one allowance, filed as infrastructure, with the
  arithmetic in the reason.
- A seat that answers 429 past the contract's busy-seat budget ends the re-pack as a transport
  verdict on that attempt — under the wall the clock cut that wait; now the budget decides, and
  no chat-lane attempt is spent on a seat already given up on. A counted 429 wait is itself a
  liveness touch (the seat answered), never a stall.
- The client's own `Timeout` no longer applies to a call under liveness: with streaming it
  would cover the whole body read and cut a long answer mid-stream. The context owns the
  deadline there (the re-pack included, item 8); the CLI doors and probes keep the client timeout.
- Contract sizing (`timeout_sec`, `wall_estimate_sec`, `wall_sec`) and the delegator's
  anchoring (D-116) are untouched. `AgentTimeoutSecCap = 900` still caps the *declared*
  value; it no longer ends anything.
- New stable reason prefixes: `stalled: ` and `ceiling `. Readers keyed on
  `wall timeout after` see it only for a caller-imposed deadline.
- Deliberately out of scope: the media lease's reclaim rule (C-32's other half) still has no
  job-liveness input — it is a different holder (ComfyUI) with a different progress signal.
- Tests never sleep real seconds for liveness: the pipeline's `livenessFloor`, `livenessSlack`
  and `ceilingFloorSec` are package vars a test compresses, as `pollSecond` already was.

Register: plan `plans/2026-09-20-liveness-walls.md` (AI Ecosystem), rows C-32 (partial) and
the D-row this ADR is filed under. Supersedes the enforcement half of D-03; D-03's sizing stands.

## Alternatives considered

- **Raise the cap** (900 s → 3,600 s). Rejected: it moves the cliff, it does not remove it — a
  1 tok/s seat on a 12-step contract still dies producing, and a hung engine now holds the seat
  four times longer. The defect is enforcing a pre-run number, not its value.
- **Per-step request timeouts instead of streaming.** Rejected: a step is one blocking POST, so a
  per-step timeout is the same blindness at a finer grain — a 214 k-token prefill is minutes of
  legitimate silence inside one step, and only a streamed first delta ends it.
- **Reuse the registry heartbeat as job liveness.** Rejected: it tracks the holder process (C-32's
  finding); folding job progress into it would hide a hung job behind a live process again.
- **Poll the engine's own metrics** (`/metrics`, `/slots`). Rejected for the node path: the
  engine's counters are per seat, not per run, and vLLM/llama.cpp expose them differently; the
  stream is per run and identical on both.

## Related code

- `internal/agent/client_stream.go`, `internal/agent/client.go` — the SSE decoder and the streamed `Chat`.
- `internal/agent/progress.go`, `internal/agent/liveness.go` — the progress callback, `StallPolicy`, `Monitor`.
- `internal/llamaclient/stream.go` — `WithProgress`, the SSE decoder and the JSON fallback (item 8).
- `internal/pipeline/agentrescue.go`, `internal/delegate/rescue.go` — the delegator's rescue of a finished answer (item 8).
- `internal/pipeline/agentrepack.go` — the re-pack's time fit, escalation gate, type text for its prompts and attempt records (item 9).
- `internal/agent/loop.go` — phases and per-delta progress from the loop.
- `internal/pipeline/liveness.go`, `internal/pipeline/agenttask.go` — the policy, the ceiling, the stall/ceiling arms, the fleet progress report.
- `internal/gpuactivity/registry.go` — job liveness beside the heartbeat; `Run.Liveness`.
- `internal/fleetnode/jobs.go`, `internal/fleetnode/server.go` — `progress` on `/fleet/jobs/{id}`.
- `internal/delegate/run.go`, `internal/delegate/intent.go` — polling on progress; `tok_per_s` on the delegate row.
- `internal/seatrate/seatrate.go` — `prefill_tok_s`.

## Related docs

- [OPERATOR-GUIDE.md — The timeout chain](../../OPERATOR-GUIDE.md) (rewritten for 0.131.0).
- ADR 0041 (the drain waits for runs inside the queue) — the run registry this builds on.
- Register D-03 (wall sizing, unchanged), D-116 (the delegator anchors on `wall_sec`, unchanged), C-32 (job liveness, the media half still open).
