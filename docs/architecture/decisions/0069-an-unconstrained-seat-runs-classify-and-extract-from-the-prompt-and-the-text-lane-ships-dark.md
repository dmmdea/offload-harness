---
status: Accepted
date: "2026-09-30"
---

# ADR 0069 — An unconstrained seat runs classify and extract from the prompt, and the fleet text lane ships dark

## Context

[ADR 0062](0062-rk3588-soc-tier-serves-from-the-npu-on-a-unified-memory-budget.md) says the grammar cascade tasks
(classify, extract, summarize, triage, `assess_image`) do not run on the RK3588 node. That was true for the reason it
gave: every structured lane in the pipeline always sends a constraint, a llama.cpp GBNF `grammar` or, on a declared vLLM
seat, a `json_schema` (ADR 0002 and its amendments). The RKLLM runtime behind the node's one seat cannot constrain
decoding. `rkllm_server.py` answers HTTP 400 with code `constrained_decoding_unsupported` to any `grammar`,
`json_schema` or `response_format` of type `json_schema`, and it ignores `logprobs`. Measured on the reference board on
2026-09-30, every structured task on that seat therefore deferred with `model call failed: ... 400 ...`, error class
`other`, before the model had produced a token.

The node also had no fleet door for text work. `internal/fleetnode/tasks.go` lists media, agent, accelerator, vision and
pipeline lanes, so a delegator could not place one classify or extract on a node whose own pipeline could serve it.

A blind check on the same 2B seat with no grammar, only a prompt, scored classify 4/4, extract 4/4, summarize 0/4 and
triage 0/4. Four cases is an anecdote, not a measurement, and summarize and triage are the two tasks whose answer quality
the harness guards with a logprob margin, which this runtime cannot provide.

## Decision

**1. A seat whose runtime cannot constrain decoding is declared, and the pipeline then sends it no constraint.** The
node config key `unconstrained_seats` (a list of model ids, matched case-insensitively by
`config.DeclaresUnconstrainedSeat`) is written by `mediaseat.Bindings` from every media seat of kind `rkllm` (its name
and its aliases) and never by `config_seed`, exactly like `vision_model`. For a declared seat, `Pipeline.attempt` and the
terminal reasoning attempt send no `grammar`, no `json_schema` / `structured_outputs` and no logprobs request, and the
system prompt carries the exact JSON shape the grammar would have forced: the keys in order, each type, the allowed
labels of an enum, "no other key", and one compact example (`tasks.Built.ForUnconstrained`). The user prompt, the
injected exemplars and the packed input are untouched, and `tasks.Build` itself is unchanged, so every seat that takes a
grammar keeps a byte-identical prompt, prompt-prefix fingerprint and cache key (a golden digest test pins it).

**2. The reply is parsed leniently and accepted strictly.** `parser.Extract` already strips code fences and leading prose
to the first JSON object. What it returns is then validated against the schema derived from the task's own fields
(`gbnf.JSONSchema`: every key required, the declared types, a classify label inside the allowed set,
`additionalProperties: false`), on top of extract's caller schema. `{"foo":1}`, `{"summary":"x"}`, a label outside the
set and an object with an extra key are refused. A failure takes the existing correction-retry path (the retry counts in
`meta.retries`), and then defers with the validator's own words naming what failed. Grounding still applies to extract,
and classify's self-reported confidence gate is unchanged. The decision-margin gate needs logprobs and is inert on these
seats: nothing is recorded, nothing escalates on it, and strict validation is the only structural guard. That is why the
admitted set below is small.

**3. summarize and triage run the same validated path for a local call and are never admitted on the fleet.** A local
cascade whose rung is an unconstrained seat used to defer those tasks as an infrastructure error; it now validates the
reply and climbs to the next rung on a quality failure, like any other seat. The blind result (0/4 each) and the missing
margin gate put them outside the fleet text lane whatever a config says: the node refuses them at ack time.

**4. The fleet text lane mirrors the vision lane and ships dark.** `POST /fleet/text` (task type `text`, token-gated,
dispatch's 1 MiB body, results through the job store as the node's full `core.Result`, defers included) runs classify or
extract on the node's own pipeline, so the seat's capability, timeouts and input cap live where they are true. It is
declared per seat: a media seat's `tasks` may name `classify` and `extract`, `mediaseat.Bindings` writes the text subset
as `text_tasks`, and a node advertises `text` in `supported_task_types` and `text_tasks` in health only when that set is
non-empty and the listener is safely reachable. The node refuses a task outside `text_tasks` at ack time with a `400`
naming the set. **No shipped tier declares a text task.** A later, data-only change adds `classify` and `extract` to the
seat's `tasks` once at least 30 cases per lane through the node's own pipeline score at least 90 % correct with zero
off-schema outputs accepted.

**5. The delegator door is opt-in and changes nothing by default.** `offload_classify` and `offload_extract` gain `route`
(`local`, `auto`, `remote`; `internal/textremote`, the vision route's shape). `local`, the default, is byte-identical to
before. `auto` leaves the box only while the machine-wide GPU lease is held, so an idle local card still always wins and
a node is never a downgrade by default; with no eligible node it stays local. `remote` forces a node and defers
(`capacity`, or `config` with no `delegate_remotes`) when none is eligible. A node is eligible only when its health lists
`text` and the task in `text_tasks` (`delegate.PlaceText`); unlike the vision lane an absent list is "none", so every
node that predates the lane is never picked. Adding `route` changed `tools/list` on every box.

## Consequences

- The RK3588 node's own pipeline now answers classify and extract locally (a prompt plus strict validation) instead of
  deferring every call, and a measurement can be made through that pipeline before any lane is opened.
- ADR 0062's statement that the grammar cascade tasks do not run on the node is amended: classify and extract run there
  without a grammar, through this path; `assess_image` still does not, and summarize and triage run only as a local,
  validated, unadmitted path.
- A wrong-but-well-formed answer is the residual risk: without logprobs, strict validation cannot catch a valid label
  that is the wrong one. The dark lane and the measured gate exist for that, and classify's self-reported confidence still
  applies.
- Every box's `tools/list` carries the new `route` property on two tools. Every caller that omits it is unchanged.
- The installer's PowerShell mirror of `mediaseat.Bindings` (`setup/install.ps1`) does not know the `rkllm` kind and so
  writes neither new key; the RK3588 tier is Linux only.

## Alternatives considered

- **Keep sending a grammar and let the seat refuse it.** This is the state that deferred every structured call; rejected.
- **Strip the grammar only after a 400.** It spends a round trip per call on a failure that is known in advance from the
  config, and it would hide a real misconfiguration on a seat that should take a grammar.
- **Detect the runtime from the roster.** `/v1/models` names models, not engines (ADR 0048's reasoning for `vllm_seats`
  applies), so the roster is declared by the seat that knows what it is.
- **Admit summarize and triage on the lane.** They scored 0/4 and have no margin gate without logprobs.
- **Advertise classify and extract now, on the 4/4 blind check.** Four cases cannot carry a routing decision; the lane is
  dark until the measured gate passes, and the operator decided the route default stays local.
- **Reuse the vision route on the node for text.** A text call carries no image and a different payload; a typed route
  keeps the two lanes' task sets, body caps and health fields independent.

## Related code

- [`internal/tasks/unconstrained.go`](../../../internal/tasks/unconstrained.go), [`internal/pipeline/unconstrained.go`](../../../internal/pipeline/unconstrained.go)
- [`internal/mediaseat/mediaseat.go`](../../../internal/mediaseat/mediaseat.go) (`Bindings`, `TextTasks`, `UnconstrainedNames`), [`internal/config/config.go`](../../../internal/config/config.go)
- [`internal/fleetnode/text_task.go`](../../../internal/fleetnode/text_task.go), [`internal/textremote/textremote.go`](../../../internal/textremote/textremote.go), [`internal/delegate/gate.go`](../../../internal/delegate/gate.go) (`PlaceText`)
- [`internal/mcpserver/mcpserver.go`](../../../internal/mcpserver/mcpserver.go) (`textRun`, `textRouteSchema`)

## Related docs

- [ADR 0002](0002-grammar-reliable-serving-flags.md) (structured output and its amendments), [ADR 0040](0040-vision-work-travels-to-a-node-with-an-idle-card.md), [ADR 0062](0062-rk3588-soc-tier-serves-from-the-npu-on-a-unified-memory-budget.md)
- [Fleet node](../../systems/fleet-node.md), [FLEET-NODE.md](../../FLEET-NODE.md), [the RK3588 tier](../../tiers/rockchip-rk3588.md)
