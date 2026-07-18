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

`--json-schema` and `response_format` are never used. Neither appears anywhere in the repository.

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
- **Flash-attention is profile-driven.** On for eleven profiles, off for `amd-gcn`; the CPU template
  omits the flag entirely rather than passing `off`, because the CPU backend has neither `-ngl` nor
  `--flash-attn`.

One template entry is intentionally exempt from all of the above: **`embeddinggemma` bypasses the
shared flag macro**, taking `--embedding --pooling mean` instead. An embedding server needs none of
the chat-template or grammar machinery.

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
  field
- [`internal/gbnf/`](../../../internal/gbnf/) — schema-to-GBNF compilation, `WrapThinking`
- [`setup/templates/`](../../../setup/templates/) — per-backend serving templates
- [`internal/confidence/confidence.go`](../../../internal/confidence/confidence.go) — logprob
  handling under grammar

## Related docs

- [../../systems/offload-pipeline.md](../../systems/offload-pipeline.md)
- [../../systems/setup-installer.md](../../systems/setup-installer.md) — hardware profiles and flag
  substitution
