---
status: Accepted
date: "2026-07-18"
---

# Grammar-constrained output via a raw GBNF field, not a schema parameter

## Context

Every offload task returns structured data — a classification label, extracted fields, a bounded
summary. The harness validates that structure, and a Tier that cannot produce parseable output is
useless regardless of how good its prose is.

llama.cpp servers expose more than one way to ask for structured output, and they are not equally
reliable on the model family this harness runs. The `--json-schema` server flag and OpenAI-style
`response_format` parameter crash the model on this stack. Separately, leaving the server's
reasoning mode enabled returns empty content, because the model spends its output on a reasoning span
the harness never reads.

These are not preferences. They are the difference between a working harness and one that returns
nothing.

## Decision

Structured output is requested by passing a **raw GBNF `grammar` field** on the completion request.
The harness compiles its own JSON Schema to GBNF internally (`internal/gbnf`) and sends the grammar
as a top-level member of the request body.

`--json-schema` and `response_format` are never used. Neither appears anywhere in the repository. (Amended 2026-09-18: that sentence once covered a third name, and one of the three now does appear — vLLM's `structured_outputs`, on vLLM seats only. See the amendment below.)

Two serving flags are mandatory on every model that serves offload tasks, across every backend
template:

- **`--jinja`** — the server applies the model's chat template.
- **`--reasoning off`** — without it, content comes back empty. The reasoning Tier still gets a
  thinking span, but the *grammar* supplies it (`gbnf.WrapThinking`), not the chat template. That is
  why turning the server's reasoning mode off is safe even for the thinking tier.

**No MTP or draft/speculative-decoding flags** are used in any template.

Two things that are deliberately **not** universal, and are frequently misremembered as such:

- **KV cache type is profile-driven.** `--cache-type-k` / `--cache-type-v` are substituted per
  hardware profile. `q8_0` is the majority (8 of 13 profiles); `f16` is used on the remaining five —
  the large-VRAM Blackwell tiers, the two AMD/Vulkan profiles, and CPU. K and V are always kept
  symmetric, and a `q8_0` V cache requires flash-attention to be on.
- **Flash-attention is profile-driven.** On for every GPU profile (`amd-gcn` was the one exception until 2026-09-20, when two GCN boxes measured FA neutral-to-positive); the CPU template
  omits the flag entirely rather than passing `off`, because the CPU backend has neither `-ngl` nor
  `--flash-attn`.

One template entry is intentionally exempt from all of the above: **`embeddinggemma` bypasses the
shared flag macro**, taking `--embedding --pooling mean` instead. An embedding server needs none of
the chat-template or grammar machinery.

## Amendment 2026-09-18 (register D-129)

**A vLLM seat is constrained by vLLM's own field, not by a GBNF grammar it discards.**

The decision above is llama.cpp's request shape, and it was applied to every seat. vLLM's request
model ALLOWS unknown extras, so a vLLM seat behind llama-swap **accepts the top-level `grammar`
member, ignores it, and answers unconstrained.** The repair shipped in 0.115.14 was downstream — trim
to the outermost `{...}`, then coerce — and the delegation log measured what that costs over nine
days: **1,018 structured re-packs on vLLM seats against 506 on every other seat, and 3-attempt
exhaustion at 19.7 % against 12.6 %.** [ADR 0048](0048-vllm-is-a-first-class-engine-on-every-tier.md)
makes vLLM a first-class engine on every tier; a first-class engine is sent the field it reads.

- **vLLM seats** — the seats `vllm_seats` declares, matched case-insensitively and **alias-resolved
  through the live llama-swap roster** — receive `structured_outputs: {"json": <schema>}` and **no
  `grammar`**. The alias step is the common case, not padding: the Qube's agent seat is bound as
  `agent-pool-3card`, an alias of the declared `qwen3.8-27b-vllm-3card`, so an exact-match-only gate
  would have left the three-card box on the discarded field.
- **llama.cpp seats** keep the raw GBNF, byte-identically. The two fields are alternatives, never
  companions: a request carries one or the other.
- **`structured_outputs` is vLLM's current name** for this. The `guided_json` / `guided_*` family is
  deprecated and unused here, and only the `json` arm is sent — the harness compiles its own schema
  (`gbnf.JSONSchema`), so the `regex`, `choice` and `grammar` (xgrammar **EBNF**, not GBNF) arms have
  no caller.
- **`response_format` stays unused on every engine.** Nothing about this amendment reopens it.
- **A vLLM rung asks for the NON-thinking render** (`WithoutThinking`). vLLM applies the constraint to
  the WHOLE output, so a thinking seat would have to emit its think block inside the JSON schema,
  which is unsatisfiable. The reasoning tier's trick of putting the think span *inside* the grammar
  (`gbnf.WrapThinking`) is llama.cpp-only for exactly this reason.
- **A roster that cannot be read resolves to "not a vLLM seat"** — keep sending the grammar, the
  behaviour every seat had before this. Failing the other way would strip the constraint from a
  llama.cpp seat on a transient probe failure and turn a working call into a parse error.
- `outerObject` and `coerceToSchema` stay on the re-pack path as a belt: an *undeclared* vLLM seat, or
  one whose alias could not be resolved, still falls back to the grammar path.
- The ledger row gains **`repack_attempts`** beside `repack_ms`, because the wall alone cannot separate
  one slow attempt from a three-attempt loop — and the attempt count is the figure this change is
  measured on.

Both send sites are pinned by
[`internal/pipeline/vllm_structured_outputs_test.go`](../../../internal/pipeline/vllm_structured_outputs_test.go).

## Consequences

- Structured output works on every backend — CUDA, Vulkan, and CPU serve the same aliases with the
  same grammar mechanism.
- The harness owns schema-to-grammar compilation, so schema features are limited to what
  `internal/gbnf` can express — a real constraint, and the right place for it.
- Anyone adding a serving template must carry `--jinja` and `--reasoning off` forward. Omitting
  `--reasoning off` produces empty output, which reads as a model problem rather than a config
  problem and costs real debugging time.
- Per-token logprobs under an active grammar are raw and pre-mask: grammar-illegal tokens can appear
  in the distribution, and a forced non-preferred spelling can show a low logprob. The confidence
  code accounts for this; naive readings of logprobs under grammar will mislead.
- "All served models get these flags" is false because of the embedding entry. Statements about
  serving flags need to say *which* entries they cover.

## Alternatives considered

- **`--json-schema` / `response_format`.** Rejected: they crash the model on this stack. This is the
  originating constraint, not a stylistic choice.
- **Prompting for JSON and parsing leniently.** Rejected: small models emit almost-JSON often enough
  that a parser becomes a guessing machine, and the failure is silent corruption rather than a clean
  validation failure.
- **Leaving server reasoning on and stripping the span afterwards.** Rejected: content comes back
  empty, so there is nothing to strip. Grammar-supplied thinking gives the reasoning tier what it
  needs without the server mode.
- **A uniform `f16` KV cache everywhere.** Rejected: it wastes VRAM on the tiers that most need it.
  `q8_0` is the default precisely because the constrained profiles are the common case.

## Related code

- [`internal/llamaclient/client.go`](../../../internal/llamaclient/client.go) — the `grammar` request
  field, and (2026-09-18) the `structured_outputs` field beside it
- [`internal/llamaclient/thinking.go`](../../../internal/llamaclient/thinking.go) — `WithJSONSchema`,
  and the rule that the two constraint fields are alternatives
- [`internal/pipeline/vllmseat.go`](../../../internal/pipeline/vllmseat.go) — "is this seat vLLM?",
  the declared roster plus alias resolution
- [`internal/gbnf/`](../../../internal/gbnf/) — schema-to-GBNF compilation, `WrapThinking`, and
  `JSONSchema` (one field list, rendered as the JSON Schema a vLLM seat takes)
- [`setup/templates/`](../../../setup/templates/) — per-backend serving templates
- [`internal/confidence/confidence.go`](../../../internal/confidence/confidence.go) — logprob
  handling under grammar

## Related docs

- [../../systems/offload-pipeline.md](../../systems/offload-pipeline.md)
- [../../systems/setup-installer.md](../../systems/setup-installer.md) — hardware profiles and flag
  substitution

**Amendment note 2026-09-18 (same day, A-100 proof):** on a vLLM seat the re-pack sends the contract's OWN JSON Schema in `structured_outputs.json`, never the GBNF-typed projection (`gbnf.JSONSchema`), because `internal/gbnf` has no object or array-of-object type and the projection had constrained a nested schema into strings. The GBNF projection remains the llama.cpp path and its documented limit.
