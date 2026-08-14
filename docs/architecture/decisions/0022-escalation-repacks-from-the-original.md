---
status: Accepted
date: "2026-08-14"
---

# The escalation boundary repacks from the original; the cache key is the logical request

Decision provenance: made by the operator-approved master plan (2026-08-07 multimodal plan,
item TO-3, consolidated pending ledger section A "ready to execute"); this ADR records that
already-made decision per the ownership rule in the index README.

## Context

The cascade packed its input ONCE at entry — GCF compaction plus a char-budget head/tail
trim sized by the global `max_input_chars` — and every tier the request climbed to,
including the escalation slot and the terminal reasoning tier, inherited that entry cut.
The bigger model paid its VRAM bill and was never offered the source it could hold.
Forwarding a small tier's lossy view up the ladder is a correctness bug class, not a
tuning knob (master plan 2026-08-07, TO-3): an escalated extract can only ground against
what survived a trim sized for a model a fraction of its size.

The cache key had the same root: it was derived from the TRIMMED input, so two different
oversized originals sharing a trim collided into one cache entry — a wrong cache hit by
construction.

## Decision

1. **The original input is retained, and every climbed-to tier — escalation and terminal
   reasoning — is re-packed FROM THE ORIGINAL against its own budget**:
   `n_ctx(callee) − genBudget(callee) − reserve − tokenized(scaffold)`, with `n_ctx`
   probed live per model (`/props`, TTL-cached including failures) and every count
   measured by the callee's own served tokenizer. `genBudget` is the callee's REAL
   completion request (the reasoning tier's includes its think-span budget). A source
   that fits arrives whole; an over-window source is cut token-exact, head+tail, on
   rune-safe piece boundaries.
2. **The entry tier's hot path is untouched** — no probes, no tokenize round-trips; its
   packing is byte-identical to the pre-TO-3 behavior, and every repack failure falls
   open to exactly that packing with the reason recorded on the ledger row
   (`tier_pack`). A repack that would not BUY view over the entry packing (a callee
   served with a small window) falls open too — the escalated view may never shrink.
3. **Cascade probes and tokenize calls are upstream-only** (`/upstream/{model}/…`, no
   bare-root fallback): the root routes answer for whatever model is currently loaded,
   which mid-cascade is the previous tier — budgets computed with the wrong window and
   vocabulary, cached under the callee's key.
4. **The cache key is the ORIGINAL input** — the logical request's identity. Under-cap
   inputs key byte-identically to before (continuity); oversized originals no longer
   collide.
5. **Ledger semantics hold steady**: `input_chars` keeps its historical entry-view
   meaning on every row (it is a trained confhead feature), and the exemplar sidecar
   harvests entry rows only.

## Consequences

- An escalated tier reads as much of the source as its window honestly holds; the
  `tier_pack` column makes every disposition auditable (`full source (under entry cap)` /
  `token-exact (full source)` / `token-exact (cut K/N tokens)` / `entry-inherited (why)`).
- Two `Count` round-trips plus at most one `Pieces` call per climb, on the escalation
  path only; probe AND tokenize failures are TTL-cached so a dead route costs one probe
  per TTL window.
- On a bare llama-server (no `/upstream`) the repack stays entry-inherited — honest, and
  a single-model server cannot meaningfully repack per-tier anyway.
- A serving-config change (an operator lowering a model's `n_ctx`) can be mis-budgeted
  for up to one TTL (10 min); the failure mode is loud (server-side context rejections),
  bounded, and documented at the probe cache.
